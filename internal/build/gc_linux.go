package build

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/LeeXN/Flintyard/internal/firecracker"
	"github.com/LeeXN/Flintyard/internal/host"
	"github.com/LeeXN/Flintyard/internal/image"
	"github.com/LeeXN/Flintyard/internal/state"
	"golang.org/x/sys/unix"
)

const RecoveryProtocol = "flintyard-build-recovery/v1"

type GCTarget struct {
	BuildID  string            `json:"build_id"`
	Snapshot string            `json:"snapshot_sha256"`
	Paths    []string          `json:"scratch_paths"`
	Scratch  map[string]string `json:"scratch_sha256"`
}
type GCExcluded struct {
	BuildID string `json:"build_id,omitempty"`
	Path    string `json:"path"`
	Reason  string `json:"reason"`
}

// The attempt lock is separate from cache keys and environment-name locks. It
// is acquired before publishing owner/build metadata, and released only after
// the final build state is committed. GC never waits for a live build.
func LockAttempt(ctx context.Context, root state.Root, id string, wait bool) (func(), error) {
	if !state.ValidUUID(id) {
		return nil, fmt.Errorf("invalid build ID")
	}
	base := filepath.Join(root.Path, "locks", "builds")
	if err := state.PrivateDir(base); err != nil {
		return nil, err
	}
	if wait {
		return state.LockContext(ctx, filepath.Join(base, id+".lock"))
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return state.Lock(filepath.Join(base, id+".lock"))
}

func GCInventory(ctx context.Context, root state.Root) ([]GCTarget, []GCExcluded, error) {
	targets, excluded := []GCTarget{}, []GCExcluded{}
	base := filepath.Join(root.Path, "builds")
	entries, err := os.ReadDir(base)
	if os.IsNotExist(err) {
		return targets, excluded, nil
	}
	if err != nil {
		return targets, excluded, err
	}
	if err = state.CheckPrivateDir(base); err != nil {
		return targets, excluded, err
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return targets, excluded, err
		}
		path := filepath.Join(base, entry.Name())
		if !state.ValidUUID(entry.Name()) || !entry.IsDir() {
			excluded = append(excluded, GCExcluded{Path: path, Reason: "unrecognized build/bootstrap entry; no cleanup authority"})
			continue
		}
		unlock, err := LockAttempt(ctx, root, entry.Name(), false)
		if err != nil {
			if errors.Is(err, syscall.EWOULDBLOCK) {
				excluded = append(excluded, GCExcluded{BuildID: entry.Name(), Path: path, Reason: "active build or GC operation"})
				continue
			}
			return targets, excluded, err
		}
		target, err := GCSnapshot(ctx, root, entry.Name())
		unlock()
		if err != nil {
			excluded = append(excluded, GCExcluded{BuildID: entry.Name(), Path: path, Reason: err.Error()})
		} else {
			targets = append(targets, target)
		}
	}
	return targets, excluded, nil
}

func loadGCBuild(ctx context.Context, root state.Root, id string) (buildRecord, string, error) {
	var record buildRecord
	if !state.ValidUUID(id) {
		return record, "", fmt.Errorf("invalid build ID")
	}
	dir := filepath.Join(root.Path, "builds", id)
	if err := state.CheckPrivateDir(dir); err != nil {
		return record, dir, err
	}
	if err := state.VerifyOwner(dir, state.Owner{RootID: root.ID, EnvironmentID: id}); err != nil {
		return record, dir, err
	}
	if err := state.Read(filepath.Join(dir, "build.json"), &record); err != nil {
		return record, dir, err
	}
	if record.APIVersion != "flintyard/v1alpha1" || record.Kind != "BuildState" || record.ID != id || record.RootID != root.ID || record.RecoveryProtocol != RecoveryProtocol || !image.DigestValid(record.RecipeSHA256) || record.StartedAt.IsZero() {
		return record, dir, fmt.Errorf("build ownership/recovery metadata is incomplete or unsupported")
	}
	if record.Creator.PID <= 0 || !state.ValidUUID(record.Creator.BootID) || record.Creator.StartTime == 0 || !filepath.IsAbs(record.Creator.Executable) || record.Creator.ExecutableInode == 0 || record.Creator.Cgroup == "" || record.Creator.NetNS == "" {
		return record, dir, fmt.Errorf("build creator identity is incomplete")
	}
	switch record.State {
	case "completed":
		return record, dir, fmt.Errorf("completed build retained")
	case "preparing", "running", "failed":
	default:
		return record, dir, fmt.Errorf("unknown build phase")
	}
	if record.CleanupState == "complete" {
		return record, dir, fmt.Errorf("build scratch already cleaned")
	}
	if record.CleanupState != "" && record.CleanupState != "deleting" {
		return record, dir, fmt.Errorf("unknown build cleanup phase")
	}
	if record.State != "failed" || record.FinishedAt == nil {
		phase, err := firecracker.ProcessState(ctx, record.Creator)
		if err != nil {
			return record, dir, err
		}
		if phase != "exited" {
			return record, dir, fmt.Errorf("build creator still alive")
		}
	}
	return record, dir, nil
}

func GCSnapshot(ctx context.Context, root state.Root, id string) (GCTarget, error) {
	target := GCTarget{BuildID: id, Paths: []string{}, Scratch: map[string]string{}}
	_, dir, err := loadGCBuild(ctx, root, id)
	if err != nil {
		return target, err
	}
	if err = host.CheckScopeIdle(ctx, dir, nil); err != nil {
		return target, err
	}
	var worker firecracker.Identity
	if err = state.Read(filepath.Join(dir, "worker-identity.json"), &worker); err == nil {
		phase, err := firecracker.ProcessState(ctx, worker)
		if err != nil {
			return target, err
		}
		if phase != "exited" {
			return target, fmt.Errorf("build worker still alive")
		}
	} else if !os.IsNotExist(err) {
		return target, err
	}
	if _, err = gcBuildJournal(dir, state.Owner{RootID: root.ID, EnvironmentID: id}); err != nil {
		return target, err
	}
	for _, name := range []string{"root", "payload", "kernel-source", "module-stage"} {
		path := filepath.Join(dir, name)
		st, err := os.Lstat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return target, err
		}
		if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
			return target, fmt.Errorf("unexpected scratch scope %s", name)
		}
		target.Paths = append(target.Paths, path)
	}
	h := sha256.New()
	scratchHashes := map[string]hash.Hash{}
	for _, path := range target.Paths {
		scratchHashes[filepath.Base(path)] = sha256.New()
	}
	identities := map[host.ScopeFileIdentity]bool{}
	encoder := json.NewEncoder(h)
	count := 0
	err = filepath.WalkDir(dir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		count++
		if count > 2<<20 {
			return fmt.Errorf("build tree exceeds recovery inventory limit")
		}
		st, err := entry.Info()
		if err != nil {
			return err
		}
		if !st.Mode().IsRegular() && !st.IsDir() && st.Mode()&os.ModeSymlink == 0 {
			return fmt.Errorf("unrecognized special file in build scope")
		}
		identities[host.ScopeFileID(st)] = true
		s := st.Sys().(*syscall.Stat_t)
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		meta := map[string]any{"path": rel, "device": uint64(s.Dev), "inode": s.Ino, "mode": uint32(s.Mode), "size": s.Size, "mtime_ns": st.ModTime().UnixNano(), "ctime_sec": s.Ctim.Sec, "ctime_ns": s.Ctim.Nsec}
		if entry.Type()&os.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			meta["link"] = link
		}
		if filepath.Dir(path) == dir && entry.Type().IsRegular() && strings.HasSuffix(entry.Name(), ".json") {
			f, err := os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
			if err != nil {
				return err
			}
			data, err := io.ReadAll(io.LimitReader(f, state.MaxState+1))
			closeErr := f.Close()
			if err != nil || closeErr != nil {
				return errors.Join(err, closeErr)
			}
			if len(data) > state.MaxState {
				return fmt.Errorf("build control file exceeds recovery limit")
			}
			meta["sha256"] = image.Sum(data)
		}
		if err := encoder.Encode(meta); err != nil {
			return err
		}
		first := strings.SplitN(rel, "/", 2)[0]
		if scratch := scratchHashes[first]; scratch != nil {
			return json.NewEncoder(scratch).Encode(meta)
		}
		return nil
	})
	if err != nil {
		return target, err
	}
	if err = host.CheckScopeIdle(ctx, dir, identities); err != nil {
		return target, err
	}
	target.Snapshot = hex.EncodeToString(h.Sum(nil))
	for name, h := range scratchHashes {
		target.Scratch[name] = hex.EncodeToString(h.Sum(nil))
	}
	return target, nil
}

func gcBuildJournal(dir string, owner state.Owner) (*state.Journal, error) {
	id := owner.EnvironmentID
	j, err := state.ReadJournal(filepath.Join(dir, "worker-journal.json"), id)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	for _, r := range j.Resources {
		switch r.Kind {
		case "build-root":
			if r.Identity["path"] != filepath.Join(dir, "root") || r.Identity["owner"] != owner.Tag() {
				return nil, fmt.Errorf("build root journal identity changed")
			}
		case "build-loop":
			if r.Identity["owner"] != id || r.Identity["disk"] != filepath.Join(dir, "payload", "rootfs.raw") || !validLoopDevice(r.Identity["device"]) {
				return nil, fmt.Errorf("build loop journal scope changed")
			}
			for _, key := range []string{"backing_device", "backing_inode"} {
				n, err := strconv.ParseUint(r.Identity[key], 10, 64)
				if err != nil || n == 0 {
					return nil, fmt.Errorf("invalid loop backing identity")
				}
			}
			st, err := os.Lstat(r.Identity["disk"])
			if err == nil {
				s := st.Sys().(*syscall.Stat_t)
				if !st.Mode().IsRegular() || strconv.FormatUint(uint64(s.Dev), 10) != r.Identity["backing_device"] || strconv.FormatUint(s.Ino, 10) != r.Identity["backing_inode"] {
					return nil, fmt.Errorf("build backing disk identity changed")
				}
			} else if !os.IsNotExist(err) {
				return nil, err
			}
		case "build-mount":
			if r.Identity["owner"] != id || r.Identity["target"] != filepath.Join(dir, "root") || !validLoopDevice(r.Identity["device"]) {
				return nil, fmt.Errorf("build mount journal scope changed")
			}
		default:
			return nil, fmt.Errorf("unknown build resource kind %s", r.Kind)
		}
	}
	return j, nil
}
func validLoopDevice(path string) bool {
	name := strings.TrimPrefix(path, "/dev/loop")
	if name == path || name == "" {
		return false
	}
	n, err := strconv.ParseUint(name, 10, 32)
	return err == nil && path == "/dev/loop"+strconv.FormatUint(n, 10)
}

// ApplyGC requires the same attempt lock as Build. Only the explicit scratch
// directories are deleted; logs, owner/control records, caches and output
// packages remain. A crash leaves cleanup_state=deleting for a new plan.
func ApplyGC(ctx context.Context, root state.Root, target GCTarget) error {
	current, err := GCSnapshot(ctx, root, target.BuildID)
	if err != nil {
		return err
	}
	a, _ := json.Marshal(current)
	b, _ := json.Marshal(target)
	if string(a) != string(b) {
		return fmt.Errorf("build gc snapshot changed")
	}
	record, dir, err := loadGCBuild(ctx, root, target.BuildID)
	if err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	record.CleanupState = "deleting"
	if err = state.Write(filepath.Join(dir, "build.json"), record); err != nil {
		return err
	}
	remaining := map[string]string{}
	for name, digest := range current.Scratch {
		remaining[name] = digest
	}
	for _, path := range current.Paths {
		fresh, e := GCSnapshot(ctx, root, target.BuildID)
		if e != nil {
			return e
		}
		if len(fresh.Scratch) != len(remaining) {
			return fmt.Errorf("build scratch inventory changed during cleanup")
		}
		for name, digest := range remaining {
			if fresh.Scratch[name] != digest {
				return fmt.Errorf("build scratch identity changed during cleanup")
			}
		}
		if err = removeBuildTree(ctx, dir, filepath.Base(path)); err != nil {
			return err
		}
		delete(remaining, filepath.Base(path))
	}
	j, err := gcBuildJournal(dir, state.Owner{RootID: root.ID, EnvironmentID: target.BuildID})
	if err != nil {
		return err
	}
	if j != nil {
		for _, r := range j.Resources {
			if r.State == "released" {
				continue
			}
			if r.State == "intent" {
				err = j.Absent(r.ID)
			} else {
				err = j.Released(r.ID)
			}
			if err != nil {
				return err
			}
		}
	}
	if record.State != "failed" {
		record.State = "failed"
		if record.Error == "" {
			record.Error = "build interrupted; scratch reclaimed by gc"
		}
	}
	if record.FinishedAt == nil {
		done := time.Now().UTC()
		record.FinishedAt = &done
	}
	record.CleanupState = "complete"
	return state.Write(filepath.Join(dir, "build.json"), record)
}
