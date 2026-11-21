package network

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"github.com/LeeXN/Flintyard/internal/host"
	"github.com/LeeXN/Flintyard/internal/state"
	"golang.org/x/sys/unix"
)

var ErrOwnership = errors.New("network resource ownership cannot be established")
var ErrConflict = errors.New("network resource conflicts with existing state")
var ErrFirewall = errors.New("nft policy cannot be installed")

// Manager's caller holds the environment lock; Manager takes only the short
// system network lock, never another environment lock and never waits for VMs.
type Manager struct{ RuntimeDir string }
type ResourceEvent struct{ Stage, ResourceID, Kind, Status string }
type CreateRequest struct {
	Plan       Plan
	Journal    *state.Journal
	KeepFailed bool
	Events     func(ResourceEvent) error
}
type ResourceError struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Message string `json:"message"`
	Cause   error  `json:"-"`
}
type Result struct {
	Plan          Plan             `json:"plan"`
	Resources     []state.Resource `json:"resources"`
	CleanupErrors []ResourceError  `json:"cleanup_errors"`
}
type CleanupError struct {
	Errors []ResourceError
	Cause  error
}

func (e *CleanupError) Error() string {
	return fmt.Sprintf("network cleanup incomplete (%d resources): %v", len(e.Errors), e.Cause)
}
func (e *CleanupError) Unwrap() error { return e.Cause }
func cleanupCause(failures []ResourceError) error {
	var causes []error
	for _, failure := range failures {
		if failure.Cause != nil {
			causes = append(causes, failure.Cause)
		}
	}
	return errors.Join(causes...)
}

func (m Manager) lock(ctx context.Context) (func(), error) {
	if os.Geteuid() != 0 {
		return nil, fmt.Errorf("network changes require root")
	}
	if m.RuntimeDir == "" {
		m.RuntimeDir = "/run/flintyard"
	}
	if err := state.PrivateDir(m.RuntimeDir); err != nil {
		return nil, err
	}
	return state.LockContext(ctx, filepath.Join(m.RuntimeDir, "network.lock"))
}
func checkJournal(p Plan, j *state.Journal) error {
	if err := p.Validate(); err != nil {
		return err
	}
	if j == nil || j.OwnerID != p.EnvironmentID {
		return fmt.Errorf("%w: environment journal mismatch", ErrOwnership)
	}
	return nil
}
func ip(ctx context.Context, ns string, args ...string) error {
	if ns != "" {
		args = append([]string{"-n", ns}, args...)
	}
	_, err := host.Run(ctx, host.CommandRequest{Name: "ip", Args: args})
	return err
}
func nsPath(p Plan) string   { return filepath.Join("/run/netns", p.Namespace) }
func number(n uint64) string { return strconv.FormatUint(n, 10) }

func (m Manager) Create(ctx context.Context, req CreateRequest) (result Result, err error) {
	p, j := req.Plan, req.Journal
	result = Result{Plan: p, Resources: []state.Resource{}, CleanupErrors: []ResourceError{}}
	if err = checkJournal(p, j); err != nil {
		return result, err
	}
	if err = ctx.Err(); err != nil {
		return result, err
	}
	unlock, err := m.lock(ctx)
	if err != nil {
		return result, err
	}
	defer unlock()
	if err = m.preflight(ctx, p); err != nil {
		return result, err
	}
	start := len(j.Resources)
	defer func() {
		if err != nil && !req.KeepFailed {
			// Cancellation must not cancel compensation. It is still finite and
			// touches only this operation's newly journaled network resources.
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			result.CleanupErrors = m.removeUnlocked(cleanupCtx, p, j, j.Resources[start:])
			if len(result.CleanupErrors) > 0 {
				err = &CleanupError{Errors: result.CleanupErrors, Cause: errors.Join(err, cleanupCause(result.CleanupErrors))}
			}
		}
		result.Resources = append(result.Resources, j.Resources[start:]...)
	}()
	step := func(kind, name string, create func() error) error {
		id, e := j.IntentIdentity(kind, map[string]string{"name": name, "owner": p.Owner, "namespace": p.Namespace})
		if e != nil {
			return e
		}
		if req.Events != nil {
			if e = req.Events(ResourceEvent{"network", id, kind, "intent"}); e != nil {
				return e
			}
		}
		if e = create(); e != nil {
			return e
		}
		if req.Events != nil {
			if e = req.Events(ResourceEvent{"network", id, kind, "created"}); e != nil {
				return e
			}
		}
		r := j.Resources[len(j.Resources)-1]
		identity, exists, e := m.observe(ctx, p, r)
		if e != nil {
			return e
		}
		if !exists {
			return fmt.Errorf("new network resource disappeared")
		}
		if e = j.Done(id, identity); e != nil {
			return e
		}
		if req.Events != nil {
			return req.Events(ResourceEvent{"network", id, kind, "done"})
		}
		return nil
	}
	if err = step("netns", p.Namespace, func() error { return createNamespace(ctx, nsPath(p), p.Owner) }); err != nil {
		return result, err
	}
	if err = step("bridge", p.Bridge, func() error {
		if e := ip(ctx, p.Namespace, "link", "add", "name", p.Bridge, "type", "bridge"); e != nil {
			return e
		}
		return ip(ctx, p.Namespace, "link", "set", "dev", p.Bridge, "alias", p.Owner)
	}); err != nil {
		return result, err
	}
	if err = step("veth", p.HostLink, func() error {
		// Some supported kernels ignore IFLA_IFALIAS during NEWLINK. Create
		// both ends inside the already owned namespace, mark them, and only
		// then atomically move the marked host end out. No unmarked host link
		// can survive a crash between link creation and journal completion.
		if e := ip(ctx, p.Namespace, "link", "add", "name", p.HostLink, "type", "veth", "peer", "name", p.PeerLink); e != nil {
			return e
		}
		for _, name := range []string{p.HostLink, p.PeerLink} {
			if e := ip(ctx, p.Namespace, "link", "set", "dev", name, "alias", p.Owner); e != nil {
				return e
			}
		}
		return ip(ctx, p.Namespace, "link", "set", "dev", p.HostLink, "netns", strconv.Itoa(os.Getpid()))
	}); err != nil {
		return result, err
	}
	for _, kind := range []string{"nft-ns", "nft-host"} {
		if err = step(kind, p.Table, func() error {
			inside := kind == "nft-ns"
			ns := ""
			if inside {
				ns = p.Namespace
			}
			data, e := PrivateFirewall(p, inside)
			if e != nil {
				return e
			}
			_, e = host.Run(ctx, nftRequest(ns, []string{"-j", "-f", "-"}, data))
			return e
		}); err != nil {
			return result, err
		}
	}
	// All filters exist before links are brought up. Only the owned host veth's
	// IPv6 knob changes; the host's global forwarding and IPv6 knobs stay intact.
	if err = disableHostIPv6(p.HostLink); err != nil {
		return result, err
	}
	if err = allowLoopbackMappings(ctx, p); err != nil {
		return result, err
	}
	if err = ip(ctx, "", "address", "add", p.Gateway.String()+"/"+strconv.Itoa(p.CIDR.Bits()), "dev", p.HostLink); err != nil {
		return result, err
	}
	if err = ip(ctx, p.Namespace, "link", "set", "dev", p.PeerLink, "master", p.Bridge); err != nil {
		return result, err
	}
	for _, name := range []string{"lo", p.Bridge, p.PeerLink} {
		if err = ip(ctx, p.Namespace, "link", "set", "dev", name, "up"); err != nil {
			return result, err
		}
	}
	if err = ip(ctx, "", "link", "set", "dev", p.HostLink, "up"); err != nil {
		return result, err
	}
	for _, node := range p.Nodes {
		if err = step("tap", node.Tap, func() error {
			if e := ip(ctx, p.Namespace, "tuntap", "add", "dev", node.Tap, "mode", "tap", "user", number(uint64(node.UID)), "group", number(uint64(node.GID))); e != nil {
				return e
			}
			return ip(ctx, p.Namespace, "link", "set", "dev", node.Tap, "alias", p.Owner)
		}); err != nil {
			return result, err
		}
		if err = ip(ctx, p.Namespace, "link", "set", "dev", node.Tap, "master", p.Bridge); err != nil {
			return result, err
		}
		if err = ip(ctx, p.Namespace, "link", "set", "dev", node.Tap, "up"); err != nil {
			return result, err
		}
	}
	return result, nil
}

func (m Manager) preflight(ctx context.Context, p Plan) error {
	if err := safeNamespaceDir(); err != nil {
		return err
	}
	if _, err := os.Lstat(nsPath(p)); !os.IsNotExist(err) {
		return fmt.Errorf("%w: namespace handle already exists or is unreadable", ErrConflict)
	}
	s, err := ReadSnapshot(ctx)
	if err != nil {
		return err
	}
	if _, err = ResolveHostConnectivity(ctx, p, requestFromPlan(p), s); err != nil {
		return err
	}
	data, err := PrivateFirewall(p, false)
	if err != nil {
		return err
	}
	if _, err = host.Run(ctx, nftRequest("", []string{"-c", "-j", "-f", "-"}, data)); err != nil {
		return fmt.Errorf("%w: %w", ErrFirewall, err)
	}
	if conflicts(p.CIDR, s.Occupied) {
		return fmt.Errorf("%w: subnet overlaps a host address, route or policy selector", ErrConflict)
	}
	for _, link := range s.Links {
		for _, name := range []string{p.HostLink, p.PeerLink, p.Bridge} {
			if link.Name == name {
				return fmt.Errorf("%w: interface name exists", ErrConflict)
			}
		}
	}
	handle, err := firewallHandle(ctx, p, "")
	if err != nil {
		return err
	}
	if handle != 0 {
		return fmt.Errorf("%w: firewall table already exists", ErrConflict)
	}
	return nil
}
func safeNamespaceDir() error {
	if err := os.MkdirAll("/run/netns", 0755); err != nil {
		return err
	}
	for _, path := range []string{"/run", "/run/netns"} {
		var st unix.Stat_t
		if err := unix.Lstat(path, &st); err != nil {
			return err
		}
		if st.Mode&unix.S_IFMT != unix.S_IFDIR || st.Uid != 0 || st.Mode&0022 != 0 {
			return fmt.Errorf("unsafe namespace directory %s", path)
		}
	}
	return nil
}

func allowLoopbackMappings(ctx context.Context, p Plan) error {
	for _, f := range p.Forwards {
		if f.BindAddress.IsLoopback() {
			link, err := findLink(ctx, "", p.HostLink)
			if err != nil {
				return err
			}
			if link == nil || link.Alias != p.Owner {
				return ErrOwnership
			}
			return os.WriteFile(filepath.Join("/proc/sys/net/ipv4/conf", p.HostLink, "route_localnet"), []byte("1\n"), 0)
		}
	}
	return nil
}

// Remove only processes network journal entries. Disk and process resources
// remain the lifecycle transaction's responsibility. Cleanup never kills a PID.
func (m Manager) Remove(ctx context.Context, p Plan, j *state.Journal) (Result, error) {
	r := Result{Plan: p, Resources: []state.Resource{}, CleanupErrors: []ResourceError{}}
	if err := checkJournal(p, j); err != nil {
		return r, err
	}
	unlock, err := m.lock(ctx)
	if err != nil {
		return r, err
	}
	defer unlock()
	r.CleanupErrors = m.removeUnlocked(ctx, p, j, j.Resources)
	r.Resources = append(r.Resources, j.Resources...)
	if len(r.CleanupErrors) > 0 {
		return r, &CleanupError{Errors: r.CleanupErrors, Cause: cleanupCause(r.CleanupErrors)}
	}
	return r, nil
}
func isNetworkKind(kind string) bool {
	switch kind {
	case "netns", "bridge", "veth", "tap", "nft-host", "nft-ns":
		return true
	}
	return false
}
func (m Manager) removeUnlocked(ctx context.Context, p Plan, j *state.Journal, resources []state.Resource) []ResourceError {
	errs := []ResourceError{}
	fail := func(r state.Resource, e error) {
		errs = append(errs, ResourceError{ID: r.ID, Kind: r.Kind, Message: e.Error(), Cause: e})
	}
	info, err := inspectNamespace(ctx, nsPath(p), p.Namespace, p.Owner)
	if err != nil {
		return []ResourceError{{Kind: "netns", Message: err.Error(), Cause: err}}
	}
	if info.Mounted {
		if err = namespaceBusy(ctx, info.Inode); err != nil {
			return []ResourceError{{Kind: "netns", Message: err.Error(), Cause: err}}
		}
	}
	// A removed handle can hide a namespace still held by a process. Consult the
	// recorded inode as well before releasing its links, firewall or reservation.
	for _, r := range resources {
		if r.Kind == "netns" && r.State == "done" {
			ino, _ := strconv.ParseUint(r.Identity["inode"], 10, 64)
			if info.Mounted && (number(info.Inode) != r.Identity["inode"] || number(info.Device) != r.Identity["device"]) {
				return []ResourceError{{ID: r.ID, Kind: r.Kind, Message: "namespace handle identity changed", Cause: ErrOwnership}}
			}
			if ino != 0 {
				if err = namespaceBusy(ctx, ino); err != nil {
					return []ResourceError{{ID: r.ID, Kind: r.Kind, Message: err.Error(), Cause: err}}
				}
			}
		}
	}
	// Firewalls protect retained links: detach TAPs and veth first, then remove
	// filters, bridge and namespace. Reverse journal order alone would remove
	// isolation before learning that a link cannot safely be reclaimed.
	ordered := make([]state.Resource, 0, len(resources))
	for i := len(resources) - 1; i >= 0; i-- {
		ordered = append(ordered, resources[i])
	}
	priority := map[string]int{"tap": 0, "veth": 1, "nft-host": 2, "nft-ns": 3, "bridge": 4, "netns": 5}
	sort.SliceStable(ordered, func(i, j int) bool { return priority[ordered[i].Kind] < priority[ordered[j].Kind] })
	for _, r := range ordered {
		if !isNetworkKind(r.Kind) || r.State == "released" {
			continue
		}
		if (r.Kind == "netns" || r.Kind == "bridge") && len(errs) > 0 {
			fail(r, fmt.Errorf("dependent cleanup failed; retaining parent"))
			continue
		}
		if r.Kind == "nft-host" {
			link, e := findLink(ctx, "", p.HostLink)
			if e != nil {
				fail(r, e)
				continue
			}
			if link != nil {
				fail(r, fmt.Errorf("%w: retaining firewall while host veth remains", ErrOwnership))
				continue
			}
		}
		if info.Mounted && (r.Kind == "nft-ns" || r.Kind == "bridge" || r.Kind == "netns") {
			if e := namespaceLeavesGone(ctx, p, r.Kind == "bridge" && r.State == "intent"); e != nil {
				fail(r, e)
				continue
			}
		}
		if r.Kind == "netns" && r.State == "done" && info.Exists && !info.Mounted {
			// Recover a crash after unmount but before unlink/state commit. All
			// owned children must already be recorded as released; the original
			// namespace must have no live process, and the placeholder's xattr
			// was independently checked above.
			pending := false
			for _, child := range j.Resources {
				if isNetworkKind(child.Kind) && child.ID != r.ID && child.State != "released" {
					pending = true
				}
			}
			if pending {
				fail(r, fmt.Errorf("retaining unmounted handle with unresolved children"))
				continue
			}
			e := os.Remove(nsPath(p))
			if e == nil {
				e = j.Released(r.ID)
			}
			if e != nil {
				fail(r, e)
			}
			continue
		}
		identity, exists, e := m.observe(ctx, p, r)
		if e != nil {
			fail(r, e)
			continue
		}
		if !exists {
			if r.State == "intent" {
				e = j.Absent(r.ID)
			} else {
				e = j.Released(r.ID)
			}
			if e != nil {
				fail(r, e)
			}
			continue
		}
		if r.State == "intent" {
			if e = j.Done(r.ID, identity); e != nil {
				fail(r, e)
				continue
			}
		}
		switch r.Kind {
		case "netns":
			if identity["mounted"] == "true" {
				e = unix.Unmount(nsPath(p), 0)
			}
			if e == nil {
				// After unmount, verify the originally marked placeholder again.
				_, e = inspectNamespace(ctx, nsPath(p), p.Namespace, p.Owner)
				if e == nil {
					e = os.Remove(nsPath(p))
				}
			}
		case "bridge", "tap":
			e = ip(ctx, p.Namespace, "link", "delete", "dev", r.Identity["name"])
		case "veth":
			ns := ""
			if identity["location"] == "namespace" {
				ns = p.Namespace
			}
			e = ip(ctx, ns, "link", "delete", "dev", p.HostLink)
		case "nft-host", "nft-ns":
			family, ns := "inet", ""
			if r.Kind == "nft-ns" {
				family, ns = "bridge", p.Namespace
			}
			data, _ := json.Marshal(object{"nftables": []any{object{"delete": object{"table": object{"family": family, "handle": mustNumber(identity["handle"])}}}}})
			_, e = host.Run(ctx, nftRequest(ns, []string{"-j", "-f", "-"}, data))
		}
		if e == nil {
			e = j.Released(r.ID)
		}
		if e != nil {
			fail(r, e)
		}
	}
	return errs
}

func namespaceLeavesGone(ctx context.Context, p Plan, allowUnmarkedBridge bool) error {
	links, err := ipLinks(ctx, p.Namespace)
	if err != nil {
		return err
	}
	for _, link := range links {
		if link.Name == "lo" && link.Alias == p.Owner {
			continue
		}
		if link.Name == p.Bridge && link.Info.Kind == "bridge" && (link.Alias == p.Owner || allowUnmarkedBridge && link.Alias == "") {
			continue
		}
		return fmt.Errorf("%w: retaining parent/filter while namespace link %s remains", ErrOwnership, link.Name)
	}
	return nil
}
func mustNumber(s string) uint64 { n, _ := strconv.ParseUint(s, 10, 64); return n }

func (m Manager) observe(ctx context.Context, p Plan, r state.Resource) (map[string]string, bool, error) {
	identity := map[string]string{"name": r.Identity["name"], "owner": p.Owner, "namespace": p.Namespace}
	if r.Identity["owner"] != p.Owner || r.Identity["namespace"] != p.Namespace {
		return nil, false, ErrOwnership
	}
	expected := map[string]string{"netns": p.Namespace, "bridge": p.Bridge, "veth": p.HostLink, "nft-host": p.Table, "nft-ns": p.Table}
	var node *Node
	if r.Kind == "tap" {
		for _, n := range p.Nodes {
			if n.Tap == identity["name"] {
				node = &n
				expected["tap"] = n.Tap
			}
		}
	}
	if expected[r.Kind] == "" || expected[r.Kind] != identity["name"] {
		return nil, false, ErrOwnership
	}
	info, err := inspectNamespace(ctx, nsPath(p), p.Namespace, p.Owner)
	if err != nil {
		return nil, false, err
	}
	if r.Kind == "netns" {
		if !info.Exists {
			return nil, false, nil
		}
		identity["device"], identity["inode"], identity["mounted"] = number(info.Device), number(info.Inode), strconv.FormatBool(info.Mounted)
	} else if r.Kind == "nft-host" || r.Kind == "nft-ns" {
		ns := ""
		if r.Kind == "nft-ns" {
			if !info.Mounted {
				return nil, false, nil
			}
			ns = p.Namespace
		}
		handle, e := firewallHandle(ctx, p, ns)
		if e != nil {
			return nil, false, e
		}
		if handle == 0 {
			return nil, false, nil
		}
		identity["handle"] = number(handle)
	} else {
		ns := p.Namespace
		if r.Kind == "veth" {
			ns = ""
		} else if !info.Mounted {
			return nil, false, nil
		}
		link, e := findLink(ctx, ns, identity["name"])
		if e != nil {
			return nil, false, e
		}
		if link == nil && r.Kind == "veth" && r.State == "intent" && info.Mounted {
			ns = p.Namespace
			link, e = findLink(ctx, ns, identity["name"])
			if e != nil {
				return nil, false, e
			}
		}
		if link == nil {
			return nil, false, nil
		}
		kind := map[string]string{"bridge": "bridge", "veth": "veth", "tap": "tun"}[r.Kind]
		if link.Info.Kind != kind {
			return nil, false, ErrOwnership
		}
		if link.Alias != p.Owner {
			// TUNSETIFF cannot set ifalias atomically. Recovery may recognize this
			// one window using the owned parent netns, exact intent, type and UID.
			ownedChildWindow := r.State == "intent" && link.Alias == "" && ns == p.Namespace && (r.Kind == "bridge" || r.Kind == "veth" || r.Kind == "tap" && node != nil && link.Info.Data.Type == "tap" && link.Info.Data.User == node.UID && link.Info.Data.Group == node.GID)
			if !ownedChildWindow {
				return nil, false, ErrOwnership
			}
		}
		identity["ifindex"] = strconv.Itoa(link.Index)
		if r.Kind == "veth" {
			if !info.Mounted {
				return nil, false, ErrOwnership
			}
			peer, e := findLink(ctx, p.Namespace, p.PeerLink)
			if e != nil {
				return nil, false, e
			}
			if peer == nil || peer.Info.Kind != "veth" || !(peer.Index == link.PeerIndex || peer.Name == link.PeerName) || !(peer.PeerIndex == link.Index || peer.PeerName == link.Name) || peer.Alias != p.Owner && !(r.State == "intent" && peer.Alias == "") {
				return nil, false, ErrOwnership
			}
			identity["peer_ifindex"] = strconv.Itoa(peer.Index)
			identity["location"] = "host"
			if ns == p.Namespace {
				identity["location"] = "namespace"
			}
		}
	}
	if r.State == "done" {
		for _, key := range []string{"device", "inode", "mounted", "handle", "ifindex", "peer_ifindex", "location"} {
			if r.Identity[key] != "" && r.Identity[key] != identity[key] {
				return nil, false, fmt.Errorf("%w: %s changed", ErrOwnership, key)
			}
		}
	}
	return identity, true, nil
}
