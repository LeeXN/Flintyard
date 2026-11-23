package flintyard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/LeeXN/Flintyard/internal/allocation"
	"github.com/LeeXN/Flintyard/internal/contract"
	"github.com/LeeXN/Flintyard/internal/firecracker"
	"github.com/LeeXN/Flintyard/internal/guestinit"
	"github.com/LeeXN/Flintyard/internal/host"
	"github.com/LeeXN/Flintyard/internal/image"
	"github.com/LeeXN/Flintyard/internal/network"
	"github.com/LeeXN/Flintyard/internal/state"
	"golang.org/x/sys/unix"
)

type resolvedInput struct {
	spec         EnvironmentSpec
	images       map[string]image.Result
	sources      map[string]string
	keys, keySHA string
}

func (c *Client) resolveInput(ctx context.Context, file string) (resolvedInput, error) {
	var input resolvedInput
	if file == "" {
		return input, Failure("E_CONFIG_INVALID", "parse", fmt.Errorf("file is required"))
	}
	m, err := c.CheckEnvironment(file)
	if err != nil {
		return input, err
	}
	b, err := json.Marshal(m)
	if err != nil {
		return input, err
	}
	if err = contract.Decode(b, &input.spec); err != nil {
		return input, Failure("E_CONFIG_INVALID", "parse", err)
	}
	if input.spec.Architecture != host.Architecture() {
		return input, Failure("E_ARCH_MISMATCH", "preflight", fmt.Errorf("environment %s, host %s", input.spec.Architecture, host.Architecture()))
	}
	input.images, input.sources = map[string]image.Result{}, map[string]string{}
	inspected := make(map[string]image.Result)
	for i, n := range input.spec.Nodes {
		if err := ctx.Err(); err != nil {
			return input, Failure("E_CANCELLED", "parse", err)
		}
		r, ok := inspected[n.Image]
		if !ok {
			r, err = c.InspectImage(ctx, InspectRequest{Image: n.Image})
			if err != nil {
				return input, err
			}
			inspected[n.Image] = r
		}
		if r.Manifest.Architecture != input.spec.Architecture {
			return input, Failure("E_ARCH_MISMATCH", "preflight", fmt.Errorf("node %s image architecture differs", n.Name))
		}
		if r.KernelIssue != "" {
			return input, Failure("E_KERNEL_INCOMPATIBLE", "preflight", fmt.Errorf("node %s: %s", n.Name, r.KernelIssue))
		}
		if r.Manifest.Initialization.Mode == "external" && input.spec.Access.PublicKey != "" {
			return input, Failure("E_CONFIG_INVALID", "parse", fmt.Errorf("external initialization cannot inject access.public_key"))
		}
		if r.Manifest.Initialization.Mode == "seed-v1" && input.spec.Access.Probe.Type == "ssh_banner" && input.spec.Access.Probe.Port != r.Manifest.Initialization.Probe.Port {
			return input, Failure("E_CONFIG_INVALID", "parse", fmt.Errorf("SSH probe port differs from prepared image port"))
		}
		input.images[n.Name] = r
		input.sources[n.Name] = n.Image
		input.spec.Nodes[i].Image = r.ID
	}
	if path := input.spec.Access.PublicKey; path != "" {
		f, err := os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
		if err != nil {
			return input, Failure("E_CONFIG_INVALID", "parse", err)
		}
		st, err := f.Stat()
		if err != nil || !st.Mode().IsRegular() {
			f.Close()
			return input, Failure("E_CONFIG_INVALID", "parse", errors.Join(err, fmt.Errorf("public key must be an ordinary file")))
		}
		data, err := io.ReadAll(io.LimitReader(f, 64<<10+1))
		f.Close()
		if err != nil {
			return input, Failure("E_CONFIG_INVALID", "parse", err)
		}
		keys, err := guestinit.PublicKeys(data)
		if err != nil {
			return input, Failure("E_CONFIG_INVALID", "parse", err)
		}
		input.keys, input.keySHA = keys, image.Sum(data)
	}
	return input, nil
}
func runPreflight(ctx context.Context, fc, jailer string) (firecracker.Binaries, firecracker.CgroupLayout, error) {
	var binaries firecracker.Binaries
	var layout firecracker.CgroupLayout
	if os.Geteuid() != 0 {
		return binaries, layout, Failure("E_PERMISSION", "preflight", fmt.Errorf("VM mutations require root"))
	}
	if err := host.KVM(); err != nil {
		return binaries, layout, Failure("E_KVM_UNAVAILABLE", "preflight", err)
	}
	if err := host.TUN(); err != nil {
		return binaries, layout, Failure("E_DEPENDENCY", "preflight", err)
	}
	var err error
	binaries, err = firecracker.CheckBinaries(ctx, fc, jailer)
	if err != nil {
		var be *host.BinaryError
		code := "E_DEPENDENCY"
		if errors.As(err, &be) {
			code = be.Code
		}
		return binaries, layout, Failure(code, "preflight", err)
	}
	for _, tool := range []string{"ip", "nft", "mkfs.ext4"} {
		if _, err = exec.LookPath(tool); err != nil {
			return binaries, layout, Failure("E_DEPENDENCY", "preflight", err)
		}
	}
	layout, err = firecracker.DiscoverCgroup()
	if err != nil {
		return binaries, layout, Failure("E_DEPENDENCY", "preflight", err)
	}
	return binaries, layout, nil
}

func (c *Client) Up(ctx context.Context, r UpRequest) (result EnvironmentResult, err error) {
	defer func() {
		if err != nil {
			var typed *Error
			if errors.As(err, &typed) {
				if errors.Is(err, context.Canceled) && typed.Code != "E_CANCELLED" {
					cancelled := *typed
					cancelled.Code = "E_CANCELLED"
					err = &cancelled
				}
			} else {
				code := "E_LAUNCH_FAILED"
				if errors.Is(err, context.Canceled) {
					code = "E_CANCELLED"
				}
				if errors.Is(err, unix.ENOSPC) {
					code = "E_DISK_FULL"
				}
				err = Failure(code, "operation", err)
			}
		}
	}()
	if r.Wait == "" {
		r.Wait = "configured"
	}
	if r.Firecracker == "" {
		r.Firecracker = "firecracker"
	}
	if r.Jailer == "" {
		r.Jailer = "jailer"
	}
	if r.UIDRange == "" {
		r.UIDRange = "60000:64999"
	}
	if r.Concurrency == 0 {
		r.Concurrency = 4
	}
	if r.Wait != "configured" && r.Wait != "none" && r.Wait != "tcp" && r.Wait != "ssh_banner" || r.Concurrency < 1 || r.Concurrency > 128 {
		return result, Failure("E_CONFIG_INVALID", "parse", fmt.Errorf("invalid wait mode or concurrency"))
	}
	uidRange, e := allocation.ParseRange(r.UIDRange)
	if e != nil {
		return result, Failure("E_CONFIG_INVALID", "parse", e)
	}
	if e = c.emit(Event{Stage: "parse", Status: "started", Message: "resolving environment inputs"}); e != nil {
		return result, Failure("E_CANCELLED", "parse", e)
	}
	input, e := c.resolveInput(ctx, r.File)
	if e != nil {
		return result, e
	}
	binaries, layout, e := runPreflight(ctx, r.Firecracker, r.Jailer)
	if e != nil {
		return result, e
	}
	if input.spec.Network.NAT || len(input.spec.PortForwards) > 0 || input.spec.Network.EgressInterface != "" {
		connectivity, e := network.PreflightConnectivity(ctx, network.ConnectivityRequest{NAT: input.spec.Network.NAT, EgressInterface: input.spec.Network.EgressInterface, Forwards: input.spec.PortForwards})
		if e != nil {
			return result, networkFailure("preflight", e)
		}
		input.spec.Network.EgressInterface = connectivity.EgressInterface
	}
	root, e := state.OpenRoot(ctx, c.StateDir, true)
	if e != nil {
		return result, stateRootFailure(e)
	}
	unlock, e := lockEnvironment(ctx, root, input.spec.Name)
	if e != nil {
		return result, Failure("E_LOCKED", "preflight", e)
	}
	defer unlock()
	existing, e := findEnvironment(ctx, root, input.spec.Name)
	if e == nil && existing.State != "deleted" {
		if existing.Reservation != nil {
			if input.spec.Network.CIDR == "auto" {
				input.spec.Network.CIDR = existing.Spec.Network.CIDR
			}
			for i := range input.spec.Nodes {
				if input.spec.Nodes[i].IPv4 == "" {
					for _, old := range existing.Spec.Nodes {
						if old.Name == input.spec.Nodes[i].Name {
							input.spec.Nodes[i].IPv4 = old.IPv4
						}
					}
				}
			}
		}
		digest, e := canonicalSpec(input.spec, input.keySHA)
		if e != nil {
			return result, e
		}
		if digest != existing.Digest {
			return result, Failure("E_SPEC_CONFLICT", "preflight", fmt.Errorf("resolved specification differs; explicitly down before replacing"))
		}
		if existing.State == "ready" || existing.State == "running" {
			if e = hydrateProcesses(root, &existing); e != nil {
				return result, Failure("E_PROCESS_IDENTITY", "preflight", e)
			}
			result, e = observeEnvironment(ctx, existing, false)
			if e != nil {
				return result, e
			}
			if result.State != "running" && result.State != "ready" {
				return result, Failure("E_SPEC_CONFLICT", "preflight", fmt.Errorf("existing environment is degraded; stop/down before recovery"))
			}
			return c.waitExisting(ctx, existing, r.Wait)
		}
		if existing.State == "stopped" {
			if e = checkRunSpace(root, input, false); e != nil {
				return resultFromState(existing), e
			}
			return c.resumeStopped(ctx, root, existing, input, binaries, layout, r, uidRange)
		}
		return resultFromState(existing), Failure("E_SPEC_CONFLICT", "preflight", fmt.Errorf("environment is %s; failed environments require stop/down", existing.State))
	}
	if e != nil && !errors.Is(e, errEnvironmentNotFound) {
		return result, Failure("E_SPEC_CONFLICT", "preflight", e)
	}
	importedIDs := make(map[string]string)
	for name, source := range input.sources {
		if !strings.HasPrefix(source, "sha256:") {
			importedID, ok := importedIDs[source]
			if !ok {
				imported, e := c.ImportImage(ctx, ImportRequest{Package: source})
				if e != nil {
					return result, e
				}
				importedID = imported.ID
				importedIDs[source] = importedID
			}
			if importedID != input.images[name].ID {
				return result, Failure("E_DIGEST_MISMATCH", "import", fmt.Errorf("image changed during import"))
			}
		}
	}
	if e = checkRunSpace(root, input, true); e != nil {
		return result, e
	}
	id := state.UUID()
	owner := state.Owner{RootID: root.ID, EnvironmentID: id}
	env := environmentState{APIVersion: APIVersion, Kind: "EnvironmentState", RootID: root.ID, ID: id, Name: input.spec.Name, Architecture: input.spec.Architecture, State: "preparing", CreatedAt: time.Now().UTC(), Spec: input.spec, PublicKeys: input.keys, PublicKeySHA256: input.keySHA, KeepFailed: r.KeepFailed, Operations: []string{}}
	for _, n := range input.spec.Nodes {
		env.Nodes = append(env.Nodes, NodeState{ID: state.UUID(), Name: n.Name, State: "preparing", ImageID: n.Image, Username: input.images[n.Name].Manifest.Initialization.Username})
	}
	env.UpdatedAt = env.CreatedAt
	checkpoint, e := makeCheckpoint(env)
	if e != nil {
		return result, e
	}
	if e = state.OwnedDirContents(envDir(root, id), owner, map[string]any{"state.json": env, "recovery.json": checkpoint}); e != nil {
		return result, Failure("E_SPEC_CONFLICT", "commit", e)
	}
	defer func() {
		if err == nil {
			return
		}
		env.State = "failed"
		var cleanupErr error
		if !r.KeepFailed {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			cleanupErr = c.cleanupEnvironment(cleanupCtx, root, &env, false)
		}
		cleanupErr = errors.Join(cleanupErr, saveEnvironment(root, &env))
		result = resultFromState(env)
		if cleanupErr != nil {
			err = rollbackFailure(err, cleanupErr)
		}
	}()
	operations := filepath.Join(envDir(root, id), "operations")
	if e = state.PrivateDir(operations); e != nil {
		return result, e
	}
	journal, e := state.NewOperationJournal(filepath.Join(operations, c.OperationID+".json"), id, c.OperationID)
	if e != nil {
		return result, e
	}
	env.Operations = append(env.Operations, journal.OperationID)
	all, e := scanEnvironments(ctx, root)
	if e != nil {
		return result, e
	}
	var retained []allocation.Reservation
	for _, other := range all {
		if other.ID != env.ID && other.State != "deleted" && other.Reservation != nil {
			retained = append(retained, *other.Reservation)
		}
	}
	request := network.AllocationRequest{RootID: root.ID, EnvironmentID: id, CIDR: env.Spec.Network.CIDR}
	request.Connectivity = network.ConnectivityRequest{NAT: env.Spec.Network.NAT, EgressInterface: env.Spec.Network.EgressInterface, Forwards: env.Spec.PortForwards}
	for i, n := range env.Nodes {
		request.Nodes = append(request.Nodes, network.NodeRequest{ID: n.ID, Name: n.Name, IPv4: env.Spec.Nodes[i].IPv4})
	}
	reservationID := state.UUID()
	reservationIntent, e := journal.IntentIdentity("allocation", map[string]string{"id": reservationID, "root_id": root.ID, "environment_id": id})
	if e != nil {
		return result, e
	}
	reservation, e := (allocation.Coordinator{}).Reserve(ctx, allocation.Request{Root: root, OperationID: journal.OperationID, ReservationID: reservationID, Network: request, UIDRange: uidRange, Retained: retained})
	if e != nil {
		return result, networkFailure("network", e)
	}
	encoded, _ := json.Marshal(reservation)
	if e = journal.Done(reservationIntent, map[string]string{"id": reservation.ID, "reservation": string(encoded)}); e != nil {
		return result, e
	}
	env.Reservation = &reservation
	env.Spec.Network.CIDR = reservation.Plan.CIDR.String()
	for i, n := range reservation.Plan.Nodes {
		env.Spec.Nodes[i].IPv4 = n.IPv4.String()
	}
	env.Digest, e = canonicalSpec(env.Spec, input.keySHA)
	if e != nil {
		return result, e
	}
	if e = saveEnvironment(root, &env); e != nil {
		return result, e
	}
	if e = state.PrivateDir(filepath.Join(envDir(root, id), "nodes")); e != nil {
		return result, e
	}
	for i := range env.Nodes {
		n := &env.Nodes[i]
		if e = c.emit(Event{Stage: "disks", Status: "started", ResourceID: n.ID, Node: n.Name, Message: "creating writable root disk"}); e != nil {
			return result, Failure("E_CANCELLED", "disks", e)
		}
		nodeOwner := owner
		nodeOwner.NodeID = n.ID
		dir := nodeDir(root, id, n.ID)
		if e = state.OwnedDir(dir, nodeOwner); e != nil {
			return result, e
		}
		intent, e := journal.IntentIdentity("node-disk", map[string]string{"node_id": n.ID, "path": filepath.Join(dir, "rootfs.raw")})
		if e != nil {
			return result, e
		}
		disk, e := image.CloneRootfs(ctx, root.Path, n.ImageID, filepath.Join(dir, "rootfs.raw"))
		if e != nil {
			return result, imageError(e)
		}
		n.Disk = disk
		if e = journal.Done(intent, map[string]string{"node_id": n.ID, "path": disk.Path, "device": strconv.FormatUint(disk.Device, 10), "inode": strconv.FormatUint(disk.Inode, 10)}); e != nil {
			return result, e
		}
		manifest := input.images[n.Name].Manifest
		if manifest.Initialization.Mode == "seed-v1" {
			if e = c.emit(Event{Stage: "seed", Status: "started", ResourceID: n.ID, Node: n.Name, Message: "creating guest seed disk"}); e != nil {
				return result, Failure("E_CANCELLED", "seed", e)
			}
			plan := reservation.Plan.Nodes[i]
			var dns []netip.Addr
			for _, d := range env.Spec.Network.DNS {
				dns = append(dns, netip.MustParseAddr(d))
			}
			n.Seed = filepath.Join(dir, "seed.raw")
			intent, e := journal.IntentIdentity("node-seed", map[string]string{"node_id": n.ID, "path": n.Seed})
			if e != nil {
				return result, e
			}
			seed, e := guestinit.CreateSeed(ctx, guestinit.SeedRequest{EnvironmentID: id, NodeID: n.ID, Hostname: plan.Hostname, MAC: plan.MAC, Address: netip.PrefixFrom(plan.IPv4, reservation.Plan.CIDR.Bits()), Gateway: reservation.Plan.Gateway, DNS: dns, Username: n.Username, PublicKeys: []byte(input.keys), Destination: n.Seed})
			if e != nil {
				return result, Failure("E_BUILD_FAILED", "seed", e)
			}
			if e = journal.Done(intent, map[string]string{"node_id": n.ID, "path": seed.Path, "device": strconv.FormatUint(seed.Device, 10), "inode": strconv.FormatUint(seed.Inode, 10)}); e != nil {
				return result, e
			}
		}
		if e = saveEnvironment(root, &env); e != nil {
			return result, e
		}
	}
	if e = c.emit(Event{Stage: "network", Status: "started", ResourceID: id, Message: "creating private network"}); e != nil {
		return result, Failure("E_CANCELLED", "network", e)
	}
	if _, e = (network.Manager{}).Create(ctx, network.CreateRequest{Plan: reservation.Plan, Journal: journal, KeepFailed: true}); e != nil {
		return result, networkFailure("network", e)
	}
	env.State = "starting"
	if e = saveEnvironment(root, &env); e != nil {
		return result, e
	}
	if err = c.launchNodes(ctx, root, &env, input, binaries, layout, r, operations); err != nil {
		return result, err
	}
	env.State = "ready"
	for _, n := range env.Nodes {
		if !n.LastProbePassed {
			env.State = "running"
		}
	}
	if e = c.emit(Event{Stage: "commit", Status: "started", ResourceID: id, Message: "committing environment"}); e != nil {
		return result, Failure("E_CANCELLED", "commit", e)
	}
	if e = saveEnvironment(root, &env); e != nil {
		return result, e
	}
	if e = c.emit(Event{Stage: "commit", Status: "succeeded", ResourceID: id, Message: "environment committed"}); e != nil {
		return result, Failure("E_CANCELLED", "commit", e)
	}
	result = resultFromState(env)
	for i := range result.Nodes {
		result.Nodes[i].Stale = false
		result.Nodes[i].Access.ProbePassed = env.Nodes[i].LastProbePassed
		result.Nodes[i].Access.Probe = effectiveProbe(env.Spec.Access.Probe.Type, r.Wait)
	}
	return result, nil
}

func effectiveProbe(configured, wait string) string {
	if wait == "configured" || wait == "" {
		return configured
	}
	return wait
}
func (c *Client) waitExisting(ctx context.Context, e environmentState, wait string) (EnvironmentResult, error) {
	r := resultFromState(e)
	kind := effectiveProbe(e.Spec.Access.Probe.Type, wait)
	r.State = "running"
	for i := range r.Nodes {
		r.Nodes[i].State = "running"
		r.Nodes[i].Access.Probe = kind
		if kind == "none" {
			continue
		}
		if err := firecracker.WaitProbe(ctx, net.JoinHostPort(r.Nodes[i].IPv4, strconv.Itoa(r.Nodes[i].Access.Port)), kind); err != nil {
			return r, Failure("E_ACCESS_TIMEOUT", "probe", err)
		}
		r.Nodes[i].State = "ready"
		r.Nodes[i].Access.ProbePassed = true
		r.Nodes[i].Stale = false
	}
	if kind != "none" {
		r.State = "ready"
	}
	return r, nil
}

func (c *Client) launchNodes(ctx context.Context, root state.Root, e *environmentState, input resolvedInput, binaries firecracker.Binaries, layout firecracker.CgroupLayout, r UpRequest, operations string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	initial := append([]NodeState(nil), e.Nodes...)
	journals := make([]*state.Journal, len(initial))
	for i := range initial {
		op := state.UUID()
		j, err := state.NewOperationJournal(filepath.Join(operations, op+".json"), e.ID, op)
		if err != nil {
			return err
		}
		journals[i] = j
		e.Operations = append(e.Operations, op)
	}
	if err := saveEnvironment(root, e); err != nil {
		return err
	}
	common := *e
	var mu sync.Mutex
	var first error
	var wg sync.WaitGroup
	slots := make(chan struct{}, r.Concurrency)
	publish := func(i int, node NodeState) error {
		mu.Lock()
		defer mu.Unlock()
		e.Nodes[i] = node
		return saveEnvironment(root, e)
	}
	for i, node := range initial {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case slots <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-slots }()
			local, err := c.launchNode(ctx, root, common, node, input.images[node.Name].Manifest, binaries, layout, journals[i], effectiveProbe(common.Spec.Access.Probe.Type, r.Wait), func(node NodeState) error { return publish(i, node) })
			mu.Lock()
			e.Nodes[i] = local
			if err != nil && first == nil {
				first = err
				cancel()
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	if first != nil {
		return first
	}
	return ctx.Err()
}
func (c *Client) launchNode(ctx context.Context, root state.Root, e environmentState, node NodeState, manifest image.Manifest, binaries firecracker.Binaries, layout firecracker.CgroupLayout, j *state.Journal, probe string, publish func(NodeState) error) (NodeState, error) {
	if err := c.emit(Event{Stage: "launch", Status: "started", ResourceID: node.ID, Node: node.Name, Message: "launching Firecracker node"}); err != nil {
		return node, Failure("E_CANCELLED", "launch", err)
	}
	node.LastProbePassed = false
	node.LastProbeAt = nil
	owner := state.Owner{RootID: root.ID, EnvironmentID: e.ID, NodeID: node.ID}
	var plan network.Node
	for _, n := range e.Reservation.Plan.Nodes {
		if n.ID == node.ID {
			plan = n
		}
	}
	cg, err := firecracker.PrepareCgroup(ctx, firecracker.CgroupRequest{Layout: layout, Owner: owner, Anchor: envDir(root, e.ID), MemoryMiB: nodeSpec(e, node.Name).MemoryMiB, Journal: j})
	if err != nil {
		return node, Failure("E_LAUNCH_FAILED", "launch", err)
	}
	node.Cgroup = &cg
	jail, err := firecracker.PrepareJail(ctx, firecracker.JailRequest{Owner: owner, NodeDir: nodeDir(root, e.ID, node.ID), StateDir: root.Path, ImageID: node.ImageID, Firecracker: binaries.Firecracker, UID: plan.UID, GID: plan.GID, RootDisk: node.Disk, Seed: node.Seed, Journal: j})
	if err != nil {
		return node, Failure("E_LAUNCH_FAILED", "launch", err)
	}
	node.Jail = &jail
	if err = publish(node); err != nil {
		return node, err
	}
	namespace := filepath.Join("/run/netns", e.Reservation.Plan.Namespace)
	var ns unix.Stat_t
	if err = unix.Stat(namespace, &ns); err != nil {
		return node, Failure("E_LAUNCH_FAILED", "launch", err)
	}
	launched, err := firecracker.Launch(ctx, firecracker.LaunchRequest{Binaries: binaries, Owner: owner, RunDir: jail.RunDir, JailBase: jail.Base, JailRoot: jail.Root, Socket: jail.Socket, SerialLog: jail.SerialLog, NetNS: namespace, NetNSInode: ns.Ino, UID: plan.UID, GID: plan.GID, Cgroup: cg, Journal: j, VirtioTransport: manifest.Boot.VirtioTransport})
	if err != nil {
		return node, Failure("E_LAUNCH_FAILED", "launch", err)
	}
	node.Process = &launched.Identity
	node.FirecrackerVersion = firecracker.Version
	node.State = "starting"
	if err = publish(node); err != nil {
		return node, err
	}
	api := firecracker.NewAPI(jail.Socket)
	defer api.Close()
	for {
		var info map[string]any
		err = api.Call(ctx, "GET", "/", nil, &info)
		if err == nil {
			if info["id"] != node.ID || info["vmm_version"] != firecracker.Version || info["state"] != "Not started" {
				return node, Failure("E_PROCESS_IDENTITY", "api", fmt.Errorf("API instance identity or initial state mismatch"))
			}
			break
		}
		phase, e := firecracker.ProcessState(ctx, launched.Identity)
		if e != nil || phase != "running" {
			return node, Failure("E_LAUNCH_FAILED", "api", errors.Join(err, e))
		}
		select {
		case <-ctx.Done():
			return node, Failure("E_API_TIMEOUT", "api", ctx.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}
	spec := nodeSpec(e, node.Name)
	args := append([]string{"root=" + manifest.Boot.RootDevice}, manifest.Boot.Args...)
	config := firecracker.Configuration{VCPUs: spec.VCPUs, MemoryMiB: spec.MemoryMiB, Kernel: "/kernel", Args: strings.Join(args, " "), Rootfs: "/rootfs.raw", Tap: plan.Tap, MAC: plan.MAC, Logger: jail.Logger}
	if manifest.Boot.Initrd != "" {
		config.Initrd = "/initrd"
	}
	if node.Seed != "" {
		config.Seed = "/seed.raw"
	}
	if err = api.Configure(ctx, config); err != nil {
		return node, Failure("E_LAUNCH_FAILED", "api", err)
	}
	node.State = "running"
	if err = publish(node); err != nil {
		return node, err
	}
	if probe != "none" {
		if err = c.emit(Event{Stage: "probe", Status: "started", ResourceID: node.ID, Node: node.Name, Message: "waiting for guest access probe"}); err != nil {
			return node, Failure("E_CANCELLED", "probe", err)
		}
		if err = firecracker.WaitProbe(ctx, net.JoinHostPort(plan.IPv4.String(), strconv.Itoa(e.Spec.Access.Probe.Port)), probe); err != nil {
			return node, Failure("E_ACCESS_TIMEOUT", "probe", err)
		}
		if err = c.emit(Event{Stage: "probe", Status: "succeeded", ResourceID: node.ID, Node: node.Name, Message: "guest access probe passed"}); err != nil {
			return node, Failure("E_CANCELLED", "probe", err)
		}
		node.State = "ready"
		node.LastProbePassed = true
		at := time.Now().UTC()
		node.LastProbeAt = &at
	}
	return node, publish(node)
}
func nodeSpec(e environmentState, name string) NodeSpec {
	for _, n := range e.Spec.Nodes {
		if n.Name == name {
			return n
		}
	}
	return NodeSpec{}
}
