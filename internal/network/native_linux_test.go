package network

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/LeeXN/Flintyard/internal/host"
	"github.com/LeeXN/Flintyard/internal/state"
	"golang.org/x/sys/unix"
)

func nativeOnly(t *testing.T) {
	t.Helper()
	if os.Getenv("FY_NETWORK_INTEGRATION") != "1" {
		t.Skip("requires tests/native-network.sh; no privileged checks were performed")
	}
	self, err := os.Readlink("/proc/self/ns/net")
	if err != nil {
		t.Fatal(err)
	}
	init, _ := os.Readlink("/proc/1/ns/net")
	var fs unix.Statfs_t
	if os.Geteuid() != 0 || self == init || unix.Statfs("/run", &fs) != nil || fs.Type != unix.TMPFS_MAGIC {
		t.Fatal("refusing network tests outside their private namespace and tmpfs /run")
	}
}
func requireTUN(t *testing.T) {
	t.Helper()
	fd, err := unix.Open("/dev/net/tun", unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Skipf("BLOCKED: real kernel TUN/TAP unavailable: %v", err)
	}
	unix.Close(fd)
}
func requireBridgeNFT(t *testing.T) {
	t.Helper()
	name := "fy_probe_" + short(state.UUID())[:12]
	_, err := host.Run(context.Background(), host.CommandRequest{Name: "nft", Args: []string{"add", "table", "bridge", name}})
	if err != nil {
		var ce *host.CommandError
		if errors.As(err, &ce) && strings.Contains(ce.Result.Output, "Operation not supported") {
			t.Skipf("BLOCKED: kernel nft bridge family unavailable: %v", err)
		}
		t.Fatal(err)
	}
	runNative(t, "nft", "delete", "table", "bridge", name)
}
func testPlan(t *testing.T, octet int) Plan {
	t.Helper()
	r := allocationFixture(3)
	r.CIDR = fmt.Sprintf("10.240.%d.0/24", octet)
	p, err := Allocate(r)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func testJournal(t *testing.T, p Plan) *state.Journal {
	t.Helper()
	j, err := state.NewJournal(filepath.Join(t.TempDir(), "journal.json"), p.EnvironmentID)
	if err != nil {
		t.Fatal(err)
	}
	return j
}
func runNative(t *testing.T, name string, args ...string) {
	t.Helper()
	if _, err := host.Run(context.Background(), host.CommandRequest{Name: name, Args: args}); err != nil {
		t.Fatal(err)
	}
}
func allReleased(t *testing.T, j *state.Journal) {
	t.Helper()
	for _, r := range j.Resources {
		if r.State != "released" {
			t.Fatalf("unreleased: %+v", r)
		}
	}
}
func networkSnapshot(t *testing.T) string {
	t.Helper()
	var links any
	if err := host.RunJSON(context.Background(), host.CommandRequest{Name: "ip", Args: []string{"-j", "-d", "link", "show"}}, &links); err != nil {
		t.Fatal(err)
	}
	var nft any
	if err := host.RunJSON(context.Background(), host.CommandRequest{Name: "nft", Args: []string{"-j", "list", "ruleset"}}, &nft); err != nil {
		t.Fatal(err)
	}
	var addresses, routes any
	if err := host.RunJSON(context.Background(), host.CommandRequest{Name: "ip", Args: []string{"-j", "-4", "address", "show"}}, &addresses); err != nil {
		t.Fatal(err)
	}
	if err := host.RunJSON(context.Background(), host.CommandRequest{Name: "ip", Args: []string{"-j", "-4", "route", "show", "table", "all"}}, &routes); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal([]any{links, addresses, routes, nft})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func attachEndpoint(t *testing.T, p Plan, node Node) string {
	t.Helper()
	ctx := context.Background()
	hash := short(state.UUID())
	guest, port := "gt-"+hash[:12], "gtp"+hash[:12]
	runNative(t, "ip", "netns", "add", guest)
	t.Cleanup(func() {
		_ = ip(ctx, p.Namespace, "link", "delete", "dev", port)
		_, _ = host.Run(ctx, host.CommandRequest{Name: "ip", Args: []string{"netns", "delete", guest}})
	})
	runNative(t, "ip", "-n", p.Namespace, "link", "add", "name", port, "type", "veth", "peer", "name", "eth0", "netns", guest)
	runNative(t, "ip", "-n", p.Namespace, "link", "set", "dev", port, "master", p.Bridge)
	runNative(t, "ip", "-n", p.Namespace, "link", "set", "dev", port, "up")
	runNative(t, "ip", "-n", guest, "address", "add", node.IPv4.String()+"/"+strconv.Itoa(p.CIDR.Bits()), "dev", "eth0")
	runNative(t, "ip", "-n", guest, "link", "set", "dev", "lo", "up")
	runNative(t, "ip", "-n", guest, "link", "set", "dev", "eth0", "up")
	runNative(t, "ip", "-n", guest, "route", "add", "default", "via", p.Gateway.String())
	return guest
}

func TestNativeTransportConnectivityAndIsolation(t *testing.T) {
	nativeOnly(t)
	requireBridgeNFT(t)
	ctx := context.Background()
	// Endpoints below are host network test namespaces, not VM/TAP substitutes
	// returned by the product. Create remains failed at the first TAP intent.
	runNative(t, "ip", "link", "add", "name", "sentinel0", "type", "veth", "peer", "name", "sentinel1")
	t.Cleanup(func() { _ = ip(ctx, "", "link", "delete", "dev", "sentinel0") })
	runNative(t, "nft", "add", "table", "inet", "sentinel")
	t.Cleanup(func() {
		_, _ = host.Run(ctx, host.CommandRequest{Name: "nft", Args: []string{"delete", "table", "inet", "sentinel"}})
	})
	before := networkSnapshot(t)
	original, err := os.ReadFile("/proc/sys/net/ipv4/ip_forward")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1\n"), 0); err != nil {
		t.Fatal(err)
	}
	defer os.WriteFile("/proc/sys/net/ipv4/ip_forward", original, 0)
	t.Run("two-three-endpoint-transports", func(t *testing.T) {
		plans := []Plan{testPlan(t, 80), testPlan(t, 81)}
		journals := []*state.Journal{testJournal(t, plans[0]), testJournal(t, plans[1])}
		sentinel := errors.New("transport test stops before TAP creation")
		done := make(chan error, 2)
		for i, p := range plans {
			j := journals[i]
			go func() {
				_, err := (Manager{}).Create(ctx, CreateRequest{Plan: p, Journal: j, KeepFailed: true, Events: func(e ResourceEvent) error {
					if e.Kind == "tap" && e.Status == "intent" {
						return sentinel
					}
					return nil
				}})
				done <- err
			}()
			t.Cleanup(func() {
				if _, err := (Manager{}).Remove(ctx, p, j); err != nil {
					t.Error(err)
				}
			})
		}
		for range plans {
			if err := <-done; !errors.Is(err, sentinel) {
				t.Fatal(err)
			}
		}
		endpoints := [][]string{{}, {}}
		for i, p := range plans {
			for _, node := range p.Nodes {
				endpoints[i] = append(endpoints[i], attachEndpoint(t, p, node))
			}
		}
		for i, p := range plans {
			for k, node := range p.Nodes {
				runNative(t, "ping", "-c", "1", "-W", "1", node.IPv4.String())
				runNative(t, "ip", "netns", "exec", endpoints[i][k], "ping", "-c", "1", "-W", "1", p.Gateway.String())
				target := p.Nodes[(k+1)%len(p.Nodes)].IPv4.String()
				runNative(t, "ip", "netns", "exec", endpoints[i][k], "ping", "-c", "1", "-W", "1", target)
				for _, other := range plans[1-i].Nodes {
					_, err := host.Run(ctx, host.CommandRequest{Name: "ip", Args: []string{"netns", "exec", endpoints[i][k], "ping", "-c", "1", "-W", "1", other.IPv4.String()}})
					var commandErr *host.CommandError
					if !errors.As(err, &commandErr) || commandErr.Result.ExitCode != 1 {
						t.Fatalf("cross-environment path was reachable or probe failed for another reason: %v", err)
					}
				}
			}
		}
		for k := 0; k < 2; k++ {
			runNative(t, "ip", "-n", endpoints[0][k], "-6", "address", "add", fmt.Sprintf("fd00:100::%d/64", 10+k), "dev", "eth0", "nodad")
		}
		_, err := host.Run(ctx, host.CommandRequest{Name: "ip", Args: []string{"netns", "exec", endpoints[0][0], "ping", "-6", "-c", "1", "-W", "1", "fd00:100::11"}})
		var ce *host.CommandError
		if !errors.As(err, &ce) || ce.Result.ExitCode != 1 {
			t.Fatalf("unmanaged IPv6 path was not blocked: %v", err)
		}
	})
	if after := networkSnapshot(t); after != before {
		t.Fatalf("transport test changed non-owned state\nbefore %s\nafter %s", before, after)
	}
}

func TestNativePrivateNetworkLifecycle(t *testing.T) {
	nativeOnly(t)
	requireTUN(t)
	requireBridgeNFT(t)
	ctx := context.Background()
	runNative(t, "ip", "link", "add", "name", "sentinel0", "type", "veth", "peer", "name", "sentinel1")
	runNative(t, "ip", "link", "set", "dev", "sentinel0", "alias", "foreign-test-resource")
	t.Cleanup(func() { _ = ip(ctx, "", "link", "delete", "sentinel0") })
	runNative(t, "nft", "add", "table", "inet", "sentinel")
	t.Cleanup(func() {
		_, _ = host.Run(ctx, host.CommandRequest{Name: "nft", Args: []string{"delete", "table", "inet", "sentinel"}})
	})
	before := networkSnapshot(t)
	p := testPlan(t, 70)
	j := testJournal(t, p)
	m := Manager{}
	r, err := m.Create(ctx, CreateRequest{Plan: p, Journal: j})
	if err != nil {
		t.Fatal(err, r)
	}
	info, err := inspectNamespace(ctx, nsPath(p), p.Namespace, p.Owner)
	if err != nil || !info.Mounted {
		t.Fatal(info, err)
	}
	for _, node := range p.Nodes {
		link, err := findLink(ctx, p.Namespace, node.Tap)
		if err != nil || link == nil || link.Alias != p.Owner || link.Info.Kind != "tun" {
			t.Fatal(link, err)
		}
	}
	if _, err = m.Create(ctx, CreateRequest{Plan: p, Journal: j}); !errors.Is(err, ErrConflict) {
		t.Fatal("adopted existing resources", err)
	}
	if _, err = m.Remove(ctx, p, j); err != nil {
		t.Fatal(err)
	}
	allReleased(t, j)
	if _, err = m.Remove(ctx, p, j); err != nil {
		t.Fatal("repeated remove", err)
	}
	if after := networkSnapshot(t); before != after {
		t.Fatalf("non-owned host resources changed\nbefore %s\nafter %s", before, after)
	}
}

func TestNativeRollbackAtEveryResourceBoundary(t *testing.T) {
	nativeOnly(t)
	for _, status := range []string{"intent", "created", "done"} {
		for boundary := 1; boundary <= 8; boundary++ {
			t.Run(fmt.Sprintf("%s-%d", status, boundary), func(t *testing.T) {
				if status == "intent" && boundary > 4 || status != "intent" && boundary >= 4 {
					requireBridgeNFT(t)
				}
				if status == "intent" && boundary > 6 || status != "intent" && boundary > 5 {
					requireTUN(t)
				}
				before := networkSnapshot(t)
				p := testPlan(t, 71)
				j := testJournal(t, p)
				m := Manager{}
				sentinel := errors.New("injected audit callback failure")
				n := 0
				_, err := m.Create(context.Background(), CreateRequest{Plan: p, Journal: j, Events: func(e ResourceEvent) error {
					if e.Status == status {
						n++
						if n == boundary {
							return sentinel
						}
					}
					return nil
				}})
				if !errors.Is(err, sentinel) {
					t.Fatal("did not preserve original failure", err)
				}
				if ce := new(CleanupError); errors.As(err, &ce) {
					t.Fatal("rollback left resources", err)
				}
				allReleased(t, j)
				if before != networkSnapshot(t) {
					t.Fatal("rollback changed non-owned network state")
				}
			})
		}
	}
}

func TestNativeKeepFailedRecoveryAndOwnershipMismatch(t *testing.T) {
	nativeOnly(t)
	ctx := context.Background()
	p := testPlan(t, 72)
	j := testJournal(t, p)
	m := Manager{}
	sentinel := errors.New("stop before done commit")
	_, err := m.Create(ctx, CreateRequest{Plan: p, Journal: j, KeepFailed: true, Events: func(e ResourceEvent) error {
		if e.Kind == "veth" && e.Status == "created" {
			return sentinel
		}
		return nil
	}})
	if !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
	if j.Resources[len(j.Resources)-1].State != "intent" {
		t.Fatal("not testing created-before-done window")
	}
	// Install a real host filter and leave its intent uncommitted as well. A
	// foreign veth must keep this isolation in place until it is recoverable.
	if _, err = j.IntentIdentity("nft-host", map[string]string{"name": p.Table, "owner": p.Owner, "namespace": p.Namespace}); err != nil {
		t.Fatal(err)
	}
	data, err := PrivateFirewall(p, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = host.Run(ctx, nftRequest("", []string{"-j", "-f", "-"}, data)); err != nil {
		t.Fatal(err)
	}
	// Reload from durable state, discarding the creating manager's observations.
	path := filepath.Join(t.TempDir(), "recovery.json")
	if err = state.Write(path, j); err != nil {
		t.Fatal(err)
	}
	recovered, err := state.ReadJournal(path, p.EnvironmentID)
	if err != nil {
		t.Fatal(err)
	}
	if err = ip(ctx, "", "link", "set", "dev", p.HostLink, "alias", "foreign-owner"); err != nil {
		t.Fatal(err)
	}
	if _, err = m.Remove(ctx, p, recovered); err == nil {
		t.Fatal("removed foreign interface")
	}
	if link, _ := findLink(ctx, "", p.HostLink); link == nil || link.Alias != "foreign-owner" {
		t.Fatal("foreign resource deleted")
	}
	if handle, err := firewallHandle(ctx, p, ""); err != nil || handle == 0 {
		t.Fatal("removed isolation protecting retained veth", handle, err)
	}
	if err = ip(ctx, "", "link", "set", "dev", p.HostLink, "alias", p.Owner); err != nil {
		t.Fatal(err)
	}
	if _, err = m.Remove(ctx, p, recovered); err != nil {
		t.Fatal(err)
	}
	allReleased(t, recovered)
}

func TestNativeCancelCompensatesWithIndependentContext(t *testing.T) {
	nativeOnly(t)
	p := testPlan(t, 73)
	j := testJournal(t, p)
	before := networkSnapshot(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := (Manager{}).Create(ctx, CreateRequest{Plan: p, Journal: j, Events: func(e ResourceEvent) error {
		if e.Kind == "veth" && e.Status == "created" {
			cancel()
			return ctx.Err()
		}
		return nil
	}})
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	allReleased(t, j)
	if before != networkSnapshot(t) {
		t.Fatal("cancel left network resources")
	}
}

func TestNativeCrashHelper(t *testing.T) {
	if os.Getenv("FY_NETWORK_CRASH_CASE") != "1" {
		return
	}
	nativeOnly(t)
	var spec struct {
		Plan    Plan
		Journal string
		StopAt  string
	}
	b, err := os.ReadFile(os.Getenv("FY_NETWORK_CHILD_SPEC"))
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(b, &spec); err != nil {
		t.Fatal(err)
	}
	j, err := state.NewJournal(spec.Journal, spec.Plan.EnvironmentID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = (Manager{}).Create(context.Background(), CreateRequest{Plan: spec.Plan, Journal: j, Events: func(e ResourceEvent) error {
		if e.Kind == spec.StopAt && e.Status == "created" {
			f := os.NewFile(3, "ready")
			_, _ = f.Write([]byte{1})
			_ = f.Close()
			for {
				time.Sleep(time.Hour)
			}
		}
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
}
func TestNativeSIGKILLRecovery(t *testing.T) {
	nativeOnly(t)
	crashRecovery(t, "veth")
}
func TestNativeSIGKILLRecoveryAfterFirewall(t *testing.T) {
	nativeOnly(t)
	requireBridgeNFT(t)
	crashRecovery(t, "nft-host")
}
func crashRecovery(t *testing.T, kind string) {
	t.Helper()
	before := networkSnapshot(t)
	p := testPlan(t, 74)
	dir := t.TempDir()
	journal := filepath.Join(dir, "journal.json")
	spec := filepath.Join(dir, "spec.json")
	if err := state.Write(spec, struct {
		Plan    Plan
		Journal string
		StopAt  string
	}{p, journal, kind}); err != nil {
		t.Fatal(err)
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	cmd := exec.Command(os.Args[0], "-test.run=^TestNativeCrashHelper$", "-test.v")
	cmd.Env = append(os.Environ(), "FY_NETWORK_CRASH_CASE=1", "FY_NETWORK_CHILD_SPEC="+spec)
	cmd.ExtraFiles = []*os.File{w}
	var log strings.Builder
	cmd.Stdout, cmd.Stderr = &log, &log
	if err = cmd.Start(); err != nil {
		w.Close()
		t.Fatal(err)
	}
	w.Close()
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	_ = r.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err = r.Read(make([]byte, 1)); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatal(err, log.String())
	}
	if err = cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	j, err := state.ReadJournal(journal, p.EnvironmentID)
	if err != nil {
		t.Fatal(err)
	}
	if j.Resources[len(j.Resources)-1].State != "intent" {
		t.Fatal("no crash-window intent")
	}
	if _, err = (Manager{}).Remove(context.Background(), p, j); err != nil {
		t.Fatal(err)
	}
	allReleased(t, j)
	if after := networkSnapshot(t); !reflect.DeepEqual(before, after) {
		t.Fatal("SIGKILL recovery left resources")
	}
}
