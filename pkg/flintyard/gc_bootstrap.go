package flintyard

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/LeeXN/Flintyard/internal/host"
	"github.com/LeeXN/Flintyard/internal/image"
	"github.com/LeeXN/Flintyard/internal/state"
	"golang.org/x/sys/unix"
)

type GCBootstrapTarget struct {
	Scope    string      `json:"scope"`
	Name     string      `json:"name"`
	Token    string      `json:"token"`
	Owner    state.Owner `json:"owner"`
	Snapshot string      `json:"snapshot_sha256"`
}
type GCBootstrapExcluded struct {
	Scope  string `json:"scope"`
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

func bootstrapBase(root state.Root, scope string) (string, error) {
	switch scope {
	case "environment":
		return filepath.Join(root.Path, "envs"), nil
	case "build":
		return filepath.Join(root.Path, "builds"), nil
	}
	return "", fmt.Errorf("unknown bootstrap scope")
}
func bootstrapLock(ctx context.Context, root state.Root, token string) (func(), error) {
	if !state.ValidUUID(token) {
		return nil, fmt.Errorf("invalid bootstrap token")
	}
	base := filepath.Join(root.Path, "locks", "bootstrap")
	if err := state.PrivateDir(base); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return state.Lock(filepath.Join(base, token+".lock"))
}
func bootstrapSnapshot(ctx context.Context, root state.Root, scope, name string) (GCBootstrapTarget, error) {
	target := GCBootstrapTarget{Scope: scope, Name: name}
	base, err := bootstrapBase(root, scope)
	if err != nil {
		return target, err
	}
	if name == "." || name == ".." || filepath.Base(name) != name || state.ValidUUID(name) {
		return target, fmt.Errorf("invalid staging entry name")
	}
	path := filepath.Join(base, name)
	marker, err := state.ReadBootstrap(path)
	if err != nil {
		return target, err
	}
	if marker.Owner.RootID != root.ID || marker.Owner.NodeID != "" || marker.Destination != filepath.Join(base, marker.Owner.EnvironmentID) || marker.StagingName != name {
		return target, fmt.Errorf("staging owner/target binding changed")
	}
	exited, err := state.PublisherExited(ctx, marker.Creator)
	if err != nil {
		return target, err
	}
	if !exited {
		return target, fmt.Errorf("bootstrap publisher still alive")
	}
	if err = host.CheckScopeIdle(ctx, path, nil); err != nil {
		return target, err
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return target, err
	}
	if len(entries) > 1024 {
		return target, fmt.Errorf("staging inventory exceeds limit")
	}
	known := map[host.ScopeFileIdentity]bool{}
	st, err := os.Lstat(path)
	if err != nil {
		return target, err
	}
	known[host.ScopeFileID(st)] = true
	files := map[string]map[string]string{}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return target, err
		}
		p := filepath.Join(path, entry.Name())
		st, err := os.Lstat(p)
		if err != nil {
			return target, err
		}
		if !st.Mode().IsRegular() || st.Size() > state.MaxState {
			return target, fmt.Errorf("unexpected non-control staging object")
		}
		known[host.ScopeFileID(st)] = true
		meta, err := pathIdentity(p)
		if err != nil {
			return target, err
		}
		sum, _, err := image.Hash(ctx, p)
		if err != nil {
			return target, err
		}
		meta["sha256"] = sum
		if expected, ok := marker.Files[entry.Name()]; ok && expected != sum {
			return target, fmt.Errorf("bootstrap control file changed")
		}
		files[entry.Name()] = meta
	}
	if err = host.CheckScopeIdle(ctx, path, known); err != nil {
		return target, err
	}
	directory, err := pathIdentity(path)
	if err != nil {
		return target, err
	}
	b, err := json.Marshal(struct {
		Marker    state.Bootstrap
		Directory map[string]string
		Files     map[string]map[string]string
	}{marker, directory, files})
	if err != nil {
		return target, err
	}
	target.Owner, target.Token, target.Snapshot = marker.Owner, marker.Token, image.Sum(b)
	return target, nil
}
func inventoryBootstraps(ctx context.Context, root state.Root) ([]GCBootstrapTarget, []GCBootstrapExcluded, error) {
	targets, excluded := []GCBootstrapTarget{}, []GCBootstrapExcluded{}
	for _, scope := range []string{"environment", "build"} {
		base, _ := bootstrapBase(root, scope)
		entries, err := os.ReadDir(base)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		if err = state.CheckPrivateDir(base); err != nil {
			return nil, nil, err
		}
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return nil, nil, err
			}
			if state.ValidUUID(entry.Name()) {
				continue
			}
			target, err := bootstrapSnapshot(ctx, root, scope, entry.Name())
			if err != nil {
				excluded = append(excluded, GCBootstrapExcluded{scope, filepath.Join(base, entry.Name()), err.Error()})
				continue
			}
			targets = append(targets, target)
		}
	}
	return targets, excluded, nil
}
func applyBootstrap(ctx context.Context, root state.Root, target GCBootstrapTarget) (string, error) {
	current, err := bootstrapSnapshot(ctx, root, target.Scope, target.Name)
	if err != nil {
		return "", err
	}
	if current != target {
		return "", fmt.Errorf("bootstrap snapshot changed")
	}
	base, _ := bootstrapBase(root, target.Scope)
	path := filepath.Join(base, target.Name)
	diagnostics := filepath.Join(root.Path, "diagnostics", "bootstrap")
	if err = state.PrivateDir(diagnostics); err != nil {
		return "", err
	}
	archive := filepath.Join(diagnostics, target.Token)
	if err = ctx.Err(); err != nil {
		return "", err
	}
	// Publication staging contains control metadata only. Preserve it as a
	// diagnostic archive using one no-replace rename; never remove the owner
	// proof first and leave an anonymous directory after a second crash.
	if err = unix.Renameat2(unix.AT_FDCWD, path, unix.AT_FDCWD, archive, unix.RENAME_NOREPLACE); err != nil {
		return "", err
	}
	if err = state.SyncDir(base); err != nil {
		return archive, err
	}
	if err = state.SyncDir(diagnostics); err != nil {
		return archive, err
	}
	receipt := struct {
		APIVersion, Kind, Scope, OriginalPath string
		Token                                 string
		Owner                                 state.Owner
		ArchivedAt                            time.Time
	}{APIVersion, "BootstrapGCReceipt", target.Scope, path, target.Token, target.Owner, time.Now().UTC()}
	if err = state.Write(filepath.Join(archive, "gc-receipt.json"), receipt); err != nil {
		return archive, err
	}
	return archive, nil
}
func sortBootstraps(targets []GCBootstrapTarget) {
	sort.Slice(targets, func(i, j int) bool {
		if targets[i].Scope != targets[j].Scope {
			return targets[i].Scope < targets[j].Scope
		}
		return targets[i].Name < targets[j].Name
	})
}
