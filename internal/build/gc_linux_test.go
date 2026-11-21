package build

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LeeXN/Flintyard/internal/firecracker"
	"github.com/LeeXN/Flintyard/internal/image"
	"github.com/LeeXN/Flintyard/internal/state"
	"golang.org/x/sys/unix"
)

func gcBuildFixture(t *testing.T) (state.Root, string, string) {
	t.Helper()
	ctx := context.Background()
	root, err := state.OpenRoot(ctx, filepath.Join(t.TempDir(), "state"), true)
	if err != nil {
		t.Fatal(err)
	}
	id := state.UUID()
	dir := filepath.Join(root.Path, "builds", id)
	creator, err := firecracker.Observe(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	done := time.Now().UTC()
	record := buildRecord{APIVersion: "flintyard/v1alpha1", Kind: "BuildState", RootID: root.ID, ID: id, State: "failed", RecipeSHA256: image.Sum([]byte("locked test recipe")), StartedAt: done, FinishedAt: &done, Error: "synthetic failed build", RecoveryProtocol: RecoveryProtocol, Creator: creator}
	if err = state.PrivateDir(filepath.Dir(dir)); err != nil {
		t.Fatal(err)
	}
	if err = state.OwnedDirContents(dir, state.Owner{RootID: root.ID, EnvironmentID: id}, map[string]any{"build.json": record}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"root/etc", "payload", "kernel-source", "module-stage"} {
		if err = os.MkdirAll(filepath.Join(dir, name), 0755); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"root/etc/fixture", "payload/rootfs.raw", "kernel-source/Makefile", "module-stage/module.ko", "build.log", "keep-note"} {
		if err = os.WriteFile(filepath.Join(dir, name), []byte("owned fixture; not Linux or an image\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return root, id, dir
}
func gcRuntimeOnly(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("full proc/namespace absence proof and cleanup require root")
	}
}
func TestBuildGCReclaimsScratchAndRetainsInputsOutputsAndDiagnostics(t *testing.T) {
	gcRuntimeOnly(t)
	root, id, dir := gcBuildFixture(t)
	ctx := context.Background()
	retained := filepath.Join(t.TempDir(), "published.fyimg")
	if err := os.WriteFile(retained, []byte("user output"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(retained, filepath.Join(dir, "root", "absolute-link")); err != nil {
		t.Fatal(err)
	}
	unlock, err := LockAttempt(ctx, root, id, false)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	target, err := GCSnapshot(ctx, root, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(target.Paths) != 4 {
		t.Fatal(target)
	}
	if err = ApplyGC(ctx, root, target); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"root", "payload", "kernel-source", "module-stage"} {
		if _, err = os.Lstat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatal("scratch remains", name, err)
		}
	}
	for _, name := range []string{"owner.json", "build.json", "build.log", "keep-note"} {
		if _, err = os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatal("diagnostic removed", name, err)
		}
	}
	if b, err := os.ReadFile(retained); err != nil || string(b) != "user output" {
		t.Fatal("followed scratch symlink", string(b), err)
	}
	var record buildRecord
	if err = state.Read(filepath.Join(dir, "build.json"), &record); err != nil || record.CleanupState != "complete" || record.Error != "synthetic failed build" {
		t.Fatal(record, err)
	}
	if _, err = GCSnapshot(ctx, root, id); err == nil {
		t.Fatal("cleaned build remained eligible")
	}
}
func TestBuildGCRejectsChangedSnapshotHeldFilesAndActiveWorkers(t *testing.T) {
	gcRuntimeOnly(t)
	for _, mode := range []string{"changed", "replaced-directory", "special-file", "held-file", "held-alias", "worker", "bad-worker-identity", "creator"} {
		t.Run(mode, func(t *testing.T) {
			root, id, dir := gcBuildFixture(t)
			ctx := context.Background()
			unlock, err := LockAttempt(ctx, root, id, false)
			if err != nil {
				t.Fatal(err)
			}
			defer unlock()
			target, err := GCSnapshot(ctx, root, id)
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "changed":
				err = os.WriteFile(filepath.Join(dir, "payload/rootfs.raw"), []byte("changed bytes"), 0600)
			case "replaced-directory":
				if err = os.Rename(filepath.Join(dir, "root"), filepath.Join(dir, "retained-old-root")); err != nil {
					t.Fatal(err)
				}
				if err = os.MkdirAll(filepath.Join(dir, "root/etc"), 0700); err != nil {
					t.Fatal(err)
				}
				err = os.WriteFile(filepath.Join(dir, "root/etc/fixture"), []byte("foreign replacement"), 0600)
			case "special-file":
				err = unix.Mkfifo(filepath.Join(dir, "root/unknown-pipe"), 0600)
			case "held-file":
				f, e := os.Open(filepath.Join(dir, "build.log"))
				if e != nil {
					t.Fatal(e)
				}
				defer f.Close()
			case "held-alias":
				alias := filepath.Join(t.TempDir(), "outside-link")
				if err = os.Link(filepath.Join(dir, "payload/rootfs.raw"), alias); err != nil {
					t.Fatal(err)
				}
				f, e := os.Open(alias)
				if e != nil {
					t.Fatal(e)
				}
				defer f.Close()
			case "worker", "bad-worker-identity":
				identity, e := firecracker.Observe(os.Getpid())
				if e != nil {
					t.Fatal(e)
				}
				if mode == "bad-worker-identity" {
					identity.StartTime++
				}
				err = state.Write(filepath.Join(dir, "worker-identity.json"), identity)
			case "creator":
				var record buildRecord
				if e := state.Read(filepath.Join(dir, "build.json"), &record); e != nil {
					t.Fatal(e)
				}
				record.State = "running"
				record.FinishedAt = nil
				err = state.Write(filepath.Join(dir, "build.json"), record)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err = ApplyGC(ctx, root, target); err == nil {
				t.Fatal("unsafe build cleanup accepted", mode)
			}
			if _, err = os.Stat(filepath.Join(dir, "root/etc/fixture")); err != nil {
				t.Fatal("precondition failure deleted scratch", mode, err)
			}
		})
	}
}
func TestBuildGCInventoryExcludesLocksUnsupportedMetadataAndCompleted(t *testing.T) {
	root, id, dir := gcBuildFixture(t)
	ctx := context.Background()
	unlock, err := LockAttempt(ctx, root, id, true)
	if err != nil {
		t.Fatal(err)
	}
	targets, excluded, err := GCInventory(ctx, root)
	unlock()
	if err != nil || len(targets) != 0 || len(excluded) != 1 || !strings.Contains(excluded[0].Reason, "active") {
		t.Fatal(targets, excluded, err)
	}
	var record buildRecord
	if err = state.Read(filepath.Join(dir, "build.json"), &record); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"unsupported", "completed", "unknown-resource"} {
		r := record
		switch mode {
		case "unsupported":
			r.RecoveryProtocol = ""
		case "completed":
			r.State = "completed"
		case "unknown-resource":
			j, e := state.NewJournal(filepath.Join(dir, "worker-journal.json"), id)
			if e != nil {
				t.Fatal(e)
			}
			_, err = j.Intent("foreign-resource")
		}
		if err != nil {
			t.Fatal(err)
		}
		if err = state.Write(filepath.Join(dir, "build.json"), r); err != nil {
			t.Fatal(err)
		}
		targets, excluded, err = GCInventory(ctx, root)
		if err != nil || len(targets) != 0 || len(excluded) != 1 {
			t.Fatal(mode, targets, excluded, err)
		}
	}
	if err = removeBuildTree(ctx, dir, "../escape"); err == nil {
		t.Fatal("cleanup allowed traversal name")
	}
}
func TestBuildGCCancelledInventoryMakesNoChanges(t *testing.T) {
	root, id, dir := gcBuildFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := GCInventory(ctx, root)
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(dir, "root/etc/fixture")); err != nil {
		t.Fatal(id, err)
	}
}
