package build

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/LeeXN/Flintyard/internal/firecracker"
	"github.com/LeeXN/Flintyard/internal/host"
	"github.com/LeeXN/Flintyard/internal/image"
	"github.com/LeeXN/Flintyard/internal/state"
)

const Protocol = "flintyard-build/v1"

type Event struct{ Stage, Status, Message, ResourceID string }
type Request struct {
	StateDir, RecipeFile, WorkerPath string
	Offline                          bool
	Events                           func(Event) error
}
type Result struct {
	Image    image.Result `json:"image"`
	BuildID  string       `json:"build_id"`
	LogPath  string       `json:"log_path"`
	CacheHit bool         `json:"cache_hit"`
}
type Error struct {
	Stage string
	Cause error
}

func (e *Error) Error() string { return e.Stage + ": " + e.Cause.Error() }
func (e *Error) Unwrap() error { return e.Cause }

type buildRecord struct {
	APIVersion       string               `json:"api_version"`
	Kind             string               `json:"kind"`
	RootID           string               `json:"root_id"`
	ID               string               `json:"id"`
	State            string               `json:"state"`
	RecipeSHA256     string               `json:"recipe_sha256"`
	StartedAt        time.Time            `json:"started_at"`
	FinishedAt       *time.Time           `json:"finished_at,omitempty"`
	Error            string               `json:"error,omitempty"`
	RecoveryProtocol string               `json:"recovery_protocol"`
	Creator          firecracker.Identity `json:"creator"`
	CleanupState     string               `json:"cleanup_state,omitempty"`
}
type cacheRecord struct {
	Protocol string `json:"protocol"`
	Key      string `json:"key"`
	ImageID  string `json:"image_id"`
}

func Build(ctx context.Context, req Request) (result Result, err error) {
	loaded, err := LoadRecipe(ctx, req.RecipeFile)
	if err != nil {
		return result, &Error{"parse", err}
	}
	r := loaded.Recipe
	if os.Geteuid() != 0 {
		return result, &Error{"preflight", fmt.Errorf("isolated build worker requires root")}
	}
	for _, tool := range append([]string{"mkfs.ext4"}, r.Requires...) {
		if _, e := exec.LookPath(tool); e != nil {
			return result, &Error{"preflight", e}
		}
	}
	if _, e := os.Lstat(r.Output.Path); e == nil {
		return result, &Error{"preflight", fmt.Errorf("output already exists")}
	} else if !os.IsNotExist(e) {
		return result, &Error{"preflight", e}
	}
	version, e := host.Run(ctx, host.CommandRequest{Name: "mkfs.ext4", Args: []string{"-V"}})
	if e != nil {
		return result, &Error{"preflight", e}
	}
	var tools *image.KernelBuild
	if r.Kernel.Mode == "source" {
		tools, e = kernelTools(ctx, len(r.Kernel.Patches) > 0)
		if e != nil {
			return result, &Error{"preflight", e}
		}
	}
	root, e := state.OpenRoot(ctx, req.StateDir, true)
	if e != nil {
		return result, &Error{"preflight", e}
	}
	if e = checkBuildOutput(root.Path, r.Output.Path); e != nil {
		return result, &Error{"preflight", e}
	}
	inputs := map[string]Input{}
	for kind, source := range map[string]Source{"rootfs": r.Rootfs.Source, "kernel": r.Kernel.Source} {
		input, e := lockInput(ctx, root.Path, kind, source, r.Network, req.Offline)
		if e != nil {
			return result, &Error{"inputs", e}
		}
		inputs[kind] = input
	}
	if r.Initrd != nil {
		input, e := lockInput(ctx, root.Path, "initrd", *r.Initrd, r.Network, req.Offline)
		if e != nil {
			return result, &Error{"inputs", e}
		}
		inputs["initrd"] = input
	}
	if r.Kernel.Mode == "source" {
		sources := map[string]Source{"kernel-config": {Path: r.Kernel.ConfigFile, SHA256: r.Kernel.ConfigSHA256}}
		for i, p := range r.Kernel.Patches {
			sources[fmt.Sprintf("kernel-patch-%03d", i)] = Source{Path: p.Path, SHA256: p.SHA256}
		}
		for kind, source := range sources {
			input, e := lockInput(ctx, root.Path, kind, source, r.Network, req.Offline)
			if e != nil {
				return result, &Error{"inputs", e}
			}
			inputs[kind] = input
		}
	}
	worker := req.WorkerPath
	if worker == "" {
		worker, e = os.Executable()
		if e != nil {
			return result, e
		}
	}
	worker, e = filepath.EvalSymlinks(worker)
	if e != nil {
		return result, e
	}
	worker, e = filepath.Abs(worker)
	if e != nil {
		return result, e
	}
	workerSHA, _, e := image.Hash(ctx, worker)
	if e != nil {
		return result, e
	}
	keyBytes, _ := json.Marshal(struct {
		Protocol, Recipe, Architecture, Go, Mkfs, Worker string
		KernelTools                                      *image.KernelBuild
	}{Protocol, loaded.SHA256, r.Architecture, runtime.Version(), version.Output, workerSHA, tools})
	key := image.Sum(keyBytes)
	cacheDir := filepath.Join(root.Path, "build-cache")
	if e = state.PrivateDir(cacheDir); e != nil {
		return result, e
	}
	unlock, e := state.LockContext(ctx, filepath.Join(cacheDir, key+".lock"))
	if e != nil {
		return result, e
	}
	defer unlock()
	var cached cacheRecord
	if e = state.Read(filepath.Join(cacheDir, key+".json"), &cached); e == nil {
		if cached.Protocol != Protocol || cached.Key != key {
			return result, &Error{"cache", fmt.Errorf("build cache ownership/key mismatch")}
		}
		img, e := image.ExportCached(ctx, root.Path, cached.ImageID, r.Output.Path)
		if e != nil {
			return result, &Error{"cache", e}
		}
		return Result{Image: img, CacheHit: true}, nil
	} else if !os.IsNotExist(e) {
		return result, &Error{"cache", e}
	}
	id := state.UUID()
	releaseAttempt, e := LockAttempt(ctx, root, id, true)
	if e != nil {
		return result, e
	}
	defer releaseAttempt()
	creator, e := firecracker.Observe(os.Getpid())
	if e != nil {
		return result, e
	}
	base := filepath.Join(root.Path, "builds")
	if e = state.PrivateDir(base); e != nil {
		return result, e
	}
	dir := filepath.Join(base, id)
	owner := state.Owner{RootID: root.ID, EnvironmentID: id}
	record := buildRecord{APIVersion: "flintyard/v1alpha1", Kind: "BuildState", RootID: root.ID, ID: id, State: "preparing", RecipeSHA256: loaded.SHA256, StartedAt: time.Now().UTC()}
	record.RecoveryProtocol, record.Creator = RecoveryProtocol, creator
	if e = state.OwnedDirContents(dir, owner, map[string]any{"build.json": record}); e != nil {
		return result, e
	}
	result.BuildID = id
	result.LogPath = filepath.Join(dir, "build.log")
	defer func() {
		done := time.Now().UTC()
		record.FinishedAt = &done
		if err != nil {
			record.State = "failed"
			record.Error = err.Error()
		} else {
			record.State = "completed"
		}
		if e := state.Write(filepath.Join(dir, "build.json"), record); e != nil {
			err = errors.Join(err, e)
		}
	}()
	if req.Events != nil {
		if e = req.Events(Event{Stage: "build", Status: "started", Message: "starting isolated image worker", ResourceID: id}); e != nil {
			return result, &Error{"audit", e}
		}
	}
	config := workerConfig{Protocol: Protocol, Operation: "build", Owner: owner, BuildDir: dir, Recipe: r, RecipeSHA256: loaded.SHA256, Inputs: inputs, Network: r.Network && !req.Offline, Worker: worker}
	config.KernelTools = tools
	record.State = "running"
	if e = state.Write(filepath.Join(dir, "build.json"), record); e != nil {
		return result, e
	}
	if e = runWorker(ctx, config, req.Events); e != nil {
		return result, &Error{"worker", e}
	}
	var report workerReport
	if e = state.Read(filepath.Join(dir, "worker-result.json"), &report); e != nil {
		return result, e
	}
	if !report.OK {
		return result, &Error{report.Stage, report.failure()}
	}
	var manifest image.Manifest
	if e = state.Read(filepath.Join(dir, "manifest.json"), &manifest); e != nil {
		return result, e
	}
	if e = os.MkdirAll(filepath.Dir(r.Output.Path), 0700); e != nil {
		return result, e
	}
	packageSHA, e := image.WritePackage(ctx, dir, manifest, r.Output.Path)
	if e != nil {
		return result, &Error{"package", e}
	}
	img, e := image.Import(ctx, root.Path, r.Output.Path, packageSHA, image.Limits{})
	if e != nil {
		return result, &Error{"import", e}
	}
	// This digest was calculated by our own packer, not supplied through an
	// independent trust channel. Keep the integrity comparison above but do not
	// describe it as external source verification.
	img.SourceTrust = "unverified"
	result.Image = img
	if e = state.Write(filepath.Join(cacheDir, key+".json"), cacheRecord{Protocol: Protocol, Key: key, ImageID: img.ID}); e != nil {
		return result, e
	}
	if req.Events != nil {
		if e = req.Events(Event{Stage: "build", Status: "succeeded", Message: "image package committed; VM validation not run", ResourceID: id}); e != nil {
			return result, &Error{"audit", e}
		}
	}
	return result, nil
}
