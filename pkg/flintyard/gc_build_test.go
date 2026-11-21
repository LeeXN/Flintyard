package flintyard

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	imagebuild "github.com/LeeXN/Flintyard/internal/build"
	"github.com/LeeXN/Flintyard/internal/firecracker"
	"github.com/LeeXN/Flintyard/internal/image"
	"github.com/LeeXN/Flintyard/internal/state"
)

func gcCoreBuildFixture(t *testing.T, root state.Root) (string, string) {
	t.Helper()
	id := state.UUID()
	dir := filepath.Join(root.Path, "builds", id)
	creator, err := firecracker.Observe(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if err = state.PrivateDir(filepath.Dir(dir)); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	metadata := map[string]any{"api_version": APIVersion, "kind": "BuildState", "root_id": root.ID, "id": id, "state": "failed", "recipe_sha256": image.Sum([]byte("synthetic GC fixture")), "started_at": now, "finished_at": now, "recovery_protocol": imagebuild.RecoveryProtocol, "creator": creator}
	if err = state.OwnedDirContents(dir, state.Owner{RootID: root.ID, EnvironmentID: id}, map[string]any{"build.json": metadata}); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(filepath.Join(dir, "root"), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "root", "scratch"), []byte("temporary build data"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "build.log"), []byte("retained diagnostic"), 0600); err != nil {
		t.Fatal(err)
	}
	return id, dir
}
func TestGCMixedPlanChecksBuildsBeforeEnvironmentDeletion(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("root is needed for GC apply and complete proc inventory")
	}
	c, root, env := environmentFixture(t)
	env.State = "failed"
	if err := saveEnvironment(root, &env); err != nil {
		t.Fatal(err)
	}
	id, dir := gcCoreBuildFixture(t, root)
	ctx := context.Background()
	r, err := c.GC(ctx, GCRequest{})
	if err != nil || len(r.Plan.Targets) != 1 || len(r.Plan.Builds) != 1 {
		t.Fatal(r, err)
	}
	path := writePlan(t, *r.Plan)
	if err = os.WriteFile(filepath.Join(dir, "root", "changed"), []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = c.GC(ctx, GCRequest{Apply: true, PlanFile: path}); err == nil {
		t.Fatal("stale build target accepted")
	}
	current, err := loadEnvironment(root, env.ID)
	if err != nil || current.State != "failed" {
		t.Fatal("environment changed before all targets checked", current.State, err)
	}
	fresh, err := c.GC(ctx, GCRequest{})
	if err != nil {
		t.Fatal(err)
	}
	applied, err := c.GC(ctx, GCRequest{Apply: true, PlanFile: writePlan(t, *fresh.Plan)})
	if err != nil || !applied.Applied || len(applied.BuildsCleaned) != 1 || applied.BuildsCleaned[0] != id || len(applied.Cleaned) != 1 {
		t.Fatal(applied, err)
	}
	if _, err = os.Stat(filepath.Join(dir, "root")); !os.IsNotExist(err) {
		t.Fatal("build scratch retained", err)
	}
	if _, err = os.Stat(filepath.Join(dir, "build.log")); err != nil {
		t.Fatal("diagnostic lost", err)
	}
}
func TestGCBuildAttemptLockAndPlanPathsAreRechecked(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("root is needed for GC apply and complete proc inventory")
	}
	c, root, _ := environmentFixture(t)
	id, dir := gcCoreBuildFixture(t, root)
	ctx := context.Background()
	r, err := c.GC(ctx, GCRequest{})
	if err != nil || len(r.Plan.Builds) != 1 {
		t.Fatal(r, err)
	}
	plan := *r.Plan
	plan.Builds = append([]imagebuild.GCTarget(nil), r.Plan.Builds...)
	plan.Builds[0].Paths = []string{filepath.Join(root.Path, "images")}
	if _, err = c.GC(ctx, GCRequest{Apply: true, PlanFile: writePlan(t, plan)}); err == nil {
		t.Fatal("modified build deletion paths accepted")
	}
	unlock, err := imagebuild.LockAttempt(ctx, root, id, true)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.GC(ctx, GCRequest{Apply: true, PlanFile: writePlan(t, *r.Plan)})
	unlock()
	if err == nil {
		t.Fatal("active build attempt was not locked out")
	}
	if _, err = os.Stat(filepath.Join(dir, "root", "scratch")); err != nil {
		t.Fatal("rejected plan removed scratch", err)
	}
}
