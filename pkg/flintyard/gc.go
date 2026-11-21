package flintyard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/LeeXN/Flintyard/internal/allocation"
	imagebuild "github.com/LeeXN/Flintyard/internal/build"
	"github.com/LeeXN/Flintyard/internal/firecracker"
	"github.com/LeeXN/Flintyard/internal/host"
	"github.com/LeeXN/Flintyard/internal/image"
	"github.com/LeeXN/Flintyard/internal/network"
	"github.com/LeeXN/Flintyard/internal/state"
	"golang.org/x/sys/unix"
)

type GCRequest struct {
	Apply    bool
	PlanFile string
}
type GCExcluded struct {
	EnvironmentID string `json:"environment_id"`
	Name          string `json:"name"`
	Reason        string `json:"reason"`
}
type GCTarget struct {
	EnvironmentID string               `json:"environment_id"`
	Name          string               `json:"name"`
	Snapshot      string               `json:"snapshot_sha256"`
	Resources     []GCResourceSnapshot `json:"resources"`
}
type GCResourceSnapshot struct {
	OperationID string            `json:"operation_id"`
	ID          string            `json:"id"`
	Kind        string            `json:"kind"`
	State       string            `json:"state"`
	Identity    map[string]string `json:"identity"`
	Observed    map[string]string `json:"observed"`
}
type GCPlan struct {
	APIVersion string                `json:"api_version"`
	Kind       string                `json:"kind"`
	RootID     string                `json:"root_id"`
	RootPath   string                `json:"root_path"`
	BootID     string                `json:"boot_id"`
	CreatedAt  time.Time             `json:"created_at"`
	ExpiresAt  time.Time             `json:"expires_at"`
	Targets    []GCTarget            `json:"targets"`
	Builds     []imagebuild.GCTarget `json:"builds"`
	Bootstraps []GCBootstrapTarget   `json:"bootstraps"`
	SHA256     string                `json:"sha256"`
}
type GCResult struct {
	Plan               *GCPlan                 `json:"plan"`
	Applied            bool                    `json:"applied"`
	Cleaned            []string                `json:"cleaned"`
	Excluded           []GCExcluded            `json:"excluded"`
	BuildsCleaned      []string                `json:"builds_cleaned"`
	BuildsExcluded     []imagebuild.GCExcluded `json:"builds_excluded"`
	BootstrapsCleaned  []string                `json:"bootstraps_cleaned"`
	BootstrapsExcluded []GCBootstrapExcluded   `json:"bootstraps_excluded"`
}

func planDigest(p GCPlan) (string, error) {
	p.SHA256 = ""
	b, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	return image.Sum(b), nil
}
func hostBootID() (string, error) {
	b, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	return strings.TrimSpace(string(b)), err
}

func (c *Client) GC(ctx context.Context, r GCRequest) (GCResult, error) {
	result := GCResult{Cleaned: []string{}, Excluded: []GCExcluded{}, BuildsCleaned: []string{}, BuildsExcluded: []imagebuild.GCExcluded{}, BootstrapsCleaned: []string{}, BootstrapsExcluded: []GCBootstrapExcluded{}}
	if err := ctx.Err(); err != nil {
		return result, Failure("E_CANCELLED", "preflight", err)
	}
	if r.Apply && r.PlanFile == "" {
		return result, Failure("E_CONFIG_INVALID", "parse", fmt.Errorf("gc apply requires --plan"))
	}
	root, err := state.OpenRoot(ctx, c.StateDir, false)
	if err != nil {
		return result, Failure("E_SPEC_CONFLICT", "preflight", err)
	}
	if r.Apply {
		if os.Geteuid() != 0 {
			return result, Failure("E_PERMISSION", "preflight", fmt.Errorf("gc apply requires root"))
		}
		var p GCPlan
		if err = state.Read(r.PlanFile, &p); err != nil {
			return result, Failure("E_CONFIG_INVALID", "parse", err)
		}
		result.Plan = &p
		return c.applyGC(ctx, root, p, result)
	}
	boot, err := hostBootID()
	if err != nil {
		return result, err
	}
	now := time.Now().UTC()
	p := GCPlan{APIVersion: APIVersion, Kind: "GCPlan", RootID: root.ID, RootPath: root.Path, BootID: boot, CreatedAt: now, ExpiresAt: now.Add(10 * time.Minute), Targets: []GCTarget{}, Builds: []imagebuild.GCTarget{}, Bootstraps: []GCBootstrapTarget{}}
	environments, excluded, err := scanGCEnvironments(ctx, root)
	if err != nil {
		return result, Failure("E_SPEC_CONFLICT", "preflight", err)
	}
	result.Excluded = append(result.Excluded, excluded...)
	for _, e := range environments {
		if err := ctx.Err(); err != nil {
			return result, Failure("E_CANCELLED", "preflight", err)
		}
		if reason := gcPhaseExclusion(e); reason != "" {
			result.Excluded = append(result.Excluded, GCExcluded{e.ID, e.Name, reason})
			continue
		}
		unlock, err := tryEnvironmentLock(root, e.Name)
		if err != nil {
			if errors.Is(err, unix.EWOULDBLOCK) {
				result.Excluded = append(result.Excluded, GCExcluded{e.ID, e.Name, "active operation"})
				continue
			}
			return result, Failure("E_LOCKED", "preflight", err)
		}
		current, err := loadEnvironment(root, e.ID)
		if err == nil {
			if reason := gcPhaseExclusion(current); reason != "" {
				result.Excluded = append(result.Excluded, GCExcluded{e.ID, e.Name, reason})
			} else {
				var target GCTarget
				target, err = gcSnapshot(ctx, root, current)
				if err == nil {
					p.Targets = append(p.Targets, target)
				} else {
					result.Excluded = append(result.Excluded, GCExcluded{e.ID, e.Name, err.Error()})
					err = nil
				}
			}
		}
		unlock()
		if err != nil {
			return result, Failure("E_SPEC_CONFLICT", "preflight", err)
		}
	}
	p.Builds, result.BuildsExcluded, err = imagebuild.GCInventory(ctx, root)
	if err != nil {
		return result, Failure("E_SPEC_CONFLICT", "preflight", err)
	}
	p.Bootstraps, result.BootstrapsExcluded, err = inventoryBootstraps(ctx, root)
	if err != nil {
		return result, Failure("E_SPEC_CONFLICT", "preflight", err)
	}
	p.SHA256, err = planDigest(p)
	if err != nil {
		return result, err
	}
	result.Plan = &p
	if r.PlanFile != "" {
		b, err := json.MarshalIndent(p, "", "  ")
		if err != nil {
			return result, err
		}
		if err = host.PublishFile(ctx, r.PlanFile, 0600, func(f *os.File) error { _, err := f.Write(append(b, '\n')); return err }); err != nil {
			return result, Failure("E_SPEC_CONFLICT", "commit", err)
		}
	}
	return result, nil
}
func gcPhaseExclusion(e environmentState) string {
	if e.KeepFailed {
		return "keep-failed environment"
	}
	switch e.State {
	case "stopped":
		return "retained stopped environment"
	case "ready", "running":
		return "committed environment; use stop/down"
	case "preparing", "starting", "failed", "deleting", "deleted":
		return ""
	}
	return "unsupported persisted phase"
}
func tryEnvironmentLock(root state.Root, name string) (func(), error) {
	if !environmentName.MatchString(name) {
		return nil, fmt.Errorf("invalid environment name")
	}
	if err := state.CheckPrivateDir(filepath.Join(root.Path, "locks")); err != nil {
		return nil, err
	}
	return state.Lock(filepath.Join(root.Path, "locks", name+".lock"))
}

func (c *Client) applyGC(ctx context.Context, root state.Root, p GCPlan, result GCResult) (GCResult, error) {
	boot, err := hostBootID()
	if err != nil {
		return result, err
	}
	digest, err := planDigest(p)
	now := time.Now().UTC()
	if err != nil || p.APIVersion != APIVersion || p.Kind != "GCPlan" || p.RootID != root.ID || p.RootPath != root.Path || p.BootID != boot || digest != p.SHA256 || p.CreatedAt.After(now) || !p.ExpiresAt.After(now) || p.ExpiresAt.Sub(p.CreatedAt) > 10*time.Minute || !p.ExpiresAt.After(p.CreatedAt) {
		return result, Failure("E_SPEC_CONFLICT", "preflight", fmt.Errorf("gc plan expired or root/boot/schema/digest changed"))
	}
	targets := append([]GCTarget(nil), p.Targets...)
	sort.Slice(targets, func(i, j int) bool {
		if targets[i].Name != targets[j].Name {
			return targets[i].Name < targets[j].Name
		}
		return targets[i].EnvironmentID < targets[j].EnvironmentID
	})
	seen, names := map[string]bool{}, map[string]bool{}
	var unlocks []func()
	defer func() {
		for i := len(unlocks) - 1; i >= 0; i-- {
			unlocks[i]()
		}
	}()
	for _, target := range targets {
		if !state.ValidUUID(target.EnvironmentID) || !environmentName.MatchString(target.Name) || seen[target.EnvironmentID] {
			return result, Failure("E_CONFIG_INVALID", "parse", fmt.Errorf("invalid/duplicate gc target"))
		}
		seen[target.EnvironmentID] = true
		if !names[target.Name] {
			unlock, err := tryEnvironmentLock(root, target.Name)
			if err != nil {
				return result, Failure("E_LOCKED", "preflight", err)
			}
			unlocks = append(unlocks, unlock)
			names[target.Name] = true
		}
	}
	builds := append([]imagebuild.GCTarget(nil), p.Builds...)
	sort.Slice(builds, func(i, j int) bool { return builds[i].BuildID < builds[j].BuildID })
	buildIDs := map[string]bool{}
	for _, target := range builds {
		if !state.ValidUUID(target.BuildID) || buildIDs[target.BuildID] {
			return result, Failure("E_CONFIG_INVALID", "parse", fmt.Errorf("invalid/duplicate build gc target"))
		}
		buildIDs[target.BuildID] = true
		unlock, err := imagebuild.LockAttempt(ctx, root, target.BuildID, false)
		if err != nil {
			return result, Failure("E_LOCKED", "preflight", err)
		}
		unlocks = append(unlocks, unlock)
	}
	bootstraps := append([]GCBootstrapTarget(nil), p.Bootstraps...)
	sortBootstraps(bootstraps)
	tokens := map[string]bool{}
	for _, target := range bootstraps {
		if tokens[target.Token] {
			return result, Failure("E_CONFIG_INVALID", "parse", fmt.Errorf("duplicate bootstrap token"))
		}
		tokens[target.Token] = true
		unlock, err := bootstrapLock(ctx, root, target.Token)
		if err != nil {
			return result, Failure("E_LOCKED", "preflight", err)
		}
		unlocks = append(unlocks, unlock)
	}
	// Validate every target before the first side effect. The same locks remain
	// held while components independently revalidate their kernel ownership.
	var current []environmentState
	for _, target := range targets {
		e, err := loadEnvironment(root, target.EnvironmentID)
		if err != nil {
			return result, Failure("E_SPEC_CONFLICT", "preflight", err)
		}
		if e.Name != target.Name || gcPhaseExclusion(e) != "" {
			return result, Failure("E_SPEC_CONFLICT", "preflight", fmt.Errorf("gc environment eligibility changed"))
		}
		observed, err := gcSnapshot(ctx, root, e)
		if err != nil {
			return result, Failure("E_SPEC_CONFLICT", "preflight", err)
		}
		if observed.Snapshot != target.Snapshot {
			return result, Failure("E_SPEC_CONFLICT", "preflight", fmt.Errorf("gc resource identity snapshot changed"))
		}
		current = append(current, e)
	}
	for _, target := range builds {
		observed, err := imagebuild.GCSnapshot(ctx, root, target.BuildID)
		if err != nil {
			return result, Failure("E_SPEC_CONFLICT", "preflight", err)
		}
		a, _ := json.Marshal(observed)
		b, _ := json.Marshal(target)
		if string(a) != string(b) {
			return result, Failure("E_SPEC_CONFLICT", "preflight", fmt.Errorf("build gc snapshot changed"))
		}
	}
	for _, target := range bootstraps {
		observed, err := bootstrapSnapshot(ctx, root, target.Scope, target.Name)
		if err != nil {
			return result, Failure("E_SPEC_CONFLICT", "preflight", err)
		}
		if observed != target {
			return result, Failure("E_SPEC_CONFLICT", "preflight", fmt.Errorf("bootstrap snapshot changed"))
		}
	}
	for i := range current {
		e := &current[i]
		if err := c.emit(Event{Stage: "cleanup", Status: "started", ResourceID: e.ID, Message: "applying verified environment GC target"}); err != nil {
			return result, Failure("E_CANCELLED", "cleanup", err)
		}
		e.State = "deleting"
		if err = saveEnvironment(root, e); err != nil {
			return result, Failure("E_CLEANUP_INCOMPLETE", "commit", err)
		}
		if err = c.cleanupEnvironment(ctx, root, e, false); err != nil {
			e.State = "failed"
			_ = saveEnvironment(root, e)
			return result, Failure("E_CLEANUP_INCOMPLETE", "cleanup", err)
		}
		e.State = "deleted"
		if err = saveEnvironment(root, e); err != nil {
			return result, Failure("E_CLEANUP_INCOMPLETE", "commit", err)
		}
		result.Cleaned = append(result.Cleaned, e.ID)
	}
	for _, target := range builds {
		if err := c.emit(Event{Stage: "cleanup", Status: "started", ResourceID: target.BuildID, Message: "applying verified build GC target"}); err != nil {
			return result, Failure("E_CANCELLED", "cleanup", err)
		}
		if err := imagebuild.ApplyGC(ctx, root, target); err != nil {
			return result, Failure("E_CLEANUP_INCOMPLETE", "cleanup", err)
		}
		result.BuildsCleaned = append(result.BuildsCleaned, target.BuildID)
	}
	for _, target := range bootstraps {
		if err := c.emit(Event{Stage: "cleanup", Status: "started", ResourceID: target.Token, Message: "archiving verified bootstrap"}); err != nil {
			return result, Failure("E_CANCELLED", "cleanup", err)
		}
		path, err := applyBootstrap(ctx, root, target)
		if path != "" {
			result.BootstrapsCleaned = append(result.BootstrapsCleaned, path)
		}
		if err != nil {
			return result, Failure("E_CLEANUP_INCOMPLETE", "cleanup", err)
		}
	}
	result.Applied = true
	return result, nil
}

func gcSnapshot(ctx context.Context, root state.Root, e environmentState) (GCTarget, error) {
	target := GCTarget{EnvironmentID: e.ID, Name: e.Name, Resources: []GCResourceSnapshot{}}
	if err := hydrateProcesses(root, &e); err != nil {
		return target, err
	}
	for _, n := range e.Nodes {
		if n.Process != nil {
			phase, err := firecracker.ProcessState(ctx, *n.Process)
			if err != nil {
				return target, err
			}
			if phase == "running" {
				return target, fmt.Errorf("live node process")
			}
		}
	}
	journals, err := environmentJournals(root, e)
	if err != nil {
		return target, err
	}
	for _, j := range journals {
		for _, r := range j.Resources {
			if r.State == "released" {
				continue
			}
			observed, err := gcObserveResource(ctx, root, e, j, r)
			if err != nil {
				return target, err
			}
			target.Resources = append(target.Resources, GCResourceSnapshot{OperationID: j.OperationID, ID: r.ID, Kind: r.Kind, State: r.State, Identity: r.Identity, Observed: observed})
		}
	}
	tree, err := snapshotOwnedTree(ctx, root, e)
	if err != nil {
		return target, err
	}
	b, err := json.Marshal(struct {
		Environment environmentState             `json:"environment"`
		Tree        map[string]map[string]string `json:"tree"`
		Resources   []GCResourceSnapshot         `json:"resources"`
	}{e, tree, target.Resources})
	if err != nil {
		return target, err
	}
	target.Snapshot = image.Sum(b)
	return target, nil
}
func pathIdentity(path string) (map[string]string, error) {
	st, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return map[string]string{"exists": "false"}, nil
	}
	if err != nil {
		return nil, err
	}
	s := st.Sys().(*syscall.Stat_t)
	return map[string]string{"exists": "true", "device": strconv.FormatUint(uint64(s.Dev), 10), "inode": strconv.FormatUint(s.Ino, 10), "mode": strconv.FormatUint(uint64(st.Mode()), 10), "size": strconv.FormatInt(st.Size(), 10), "mtime_ns": strconv.FormatInt(st.ModTime().UnixNano(), 10), "ctime_ns": strconv.FormatInt(s.Ctim.Sec*1e9+s.Ctim.Nsec, 10)}, nil
}
func snapshotOwnedTree(ctx context.Context, root state.Root, e environmentState) (map[string]map[string]string, error) {
	base := envDir(root, e.ID)
	mounts, err := host.Mounts()
	if err != nil {
		return nil, err
	}
	mounted := map[string]host.Mount{}
	for _, m := range mounts {
		if host.Within(base, m.Path) {
			mounted[m.Path] = m
		}
	}
	tree := map[string]map[string]string{}
	err = filepath.WalkDir(base, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		identity, err := pathIdentity(path)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(base, path)
		if err != nil {
			return err
		}
		tree[relative] = identity
		if mount, ok := mounted[path]; ok {
			identity["mount_id"] = strconv.Itoa(mount.ID)
			identity["mount_ro"] = strconv.FormatBool(mount.ReadOnly)
			if entry.IsDir() {
				return filepath.SkipDir
			}
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("unexpected symlink in gc scope")
		}
		if entry.Type().IsRegular() && (entry.Name() == "state.json" || entry.Name() == "owner.json" || strings.HasSuffix(entry.Name(), ".json")) {
			f, err := os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
			if err != nil {
				return err
			}
			b, err := io.ReadAll(io.LimitReader(f, state.MaxState+1))
			f.Close()
			if err != nil {
				return err
			}
			if len(b) > state.MaxState {
				return fmt.Errorf("gc state snapshot exceeds limit")
			}
			identity["sha256"] = image.Sum(b)
		}
		return nil
	})
	return tree, err
}

func gcObserveResource(ctx context.Context, root state.Root, e environmentState, j *state.Journal, r state.Resource) (map[string]string, error) {
	absent := map[string]string{"exists": "false"}
	switch r.Kind {
	case "vmm-process":
		if text := r.Identity["identity"]; text != "" && r.Identity["launch_phase"] == "vmm" {
			var id firecracker.Identity
			if err := json.Unmarshal([]byte(text), &id); err != nil {
				return nil, err
			}
			phase, err := firecracker.ProcessState(ctx, id)
			if err != nil {
				return nil, err
			}
			if phase == "running" {
				return nil, fmt.Errorf("live VMM prevents gc")
			}
			return absent, nil
		}
		id, _, err := firecracker.RecoverProcess(ctx, root.ID, j, r)
		if err != nil {
			return nil, err
		}
		if id != nil {
			return nil, fmt.Errorf("live owned launcher prevents gc")
		}
		return absent, nil
	case "netns", "bridge", "veth", "tap", "nft-host", "nft-ns":
		if e.Reservation == nil {
			return nil, fmt.Errorf("network gc requires retained ownership plan")
		}
		identity, exists, err := (network.Manager{}).ObserveResource(ctx, e.Reservation.Plan, r)
		if err != nil {
			return nil, err
		}
		if !exists {
			return absent, nil
		}
		identity["exists"] = "true"
		return identity, nil
	case "allocation":
		records, err := (allocation.Coordinator{}).Existing(ctx)
		if err != nil {
			return nil, err
		}
		for _, record := range records {
			if record.ID == r.Identity["id"] {
				if record.Plan.RootID != root.ID || record.Plan.EnvironmentID != e.ID {
					return nil, fmt.Errorf("allocation gc owner mismatch")
				}
				b, _ := json.Marshal(record)
				return map[string]string{"exists": "true", "sha256": image.Sum(b)}, nil
			}
		}
		return absent, nil
	case "node-disk", "node-seed":
		node := r.Identity["node_id"]
		known := false
		for _, n := range e.Nodes {
			if n.ID == node {
				known = true
			}
		}
		name := "rootfs.raw"
		if r.Kind == "node-seed" {
			name = "seed.raw"
		}
		if !known || r.Identity["path"] != filepath.Join(nodeDir(root, e.ID, node), name) {
			return nil, fmt.Errorf("disk gc scope mismatch")
		}
		identity, err := pathIdentity(r.Identity["path"])
		if err != nil {
			return nil, err
		}
		if identity["exists"] == "true" && r.State == "done" && (identity["device"] != r.Identity["device"] || identity["inode"] != r.Identity["inode"]) {
			return nil, fmt.Errorf("disk gc identity changed")
		}
		return identity, nil
	case "runtime-dir", "jail-dir", "jail-mount":
		return gcJailIdentity(root, e, j, r)
	case "cgroup-parent", "cgroup-leaf":
		return gcCgroupIdentity(root, e, j, r)
	}
	return nil, fmt.Errorf("unsupported gc resource kind %s", r.Kind)
}
