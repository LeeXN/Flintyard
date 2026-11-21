package flintyard

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/LeeXN/Flintyard/internal/firecracker"
	"github.com/LeeXN/Flintyard/internal/state"
)

func writePlan(t *testing.T, p GCPlan) string {
	t.Helper()
	p.SHA256, _ = planDigest(p)
	path := filepath.Join(t.TempDir(), "plan.json")
	if err := state.Write(path, p); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestGCLiveProcessExcludedEvenInFailedEnvironment(t *testing.T) {
	c, root, e := environmentFixture(t)
	e.State = "failed"
	id, err := firecracker.Observe(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	e.Nodes[0].Process = &id
	if err = saveEnvironment(root, &e); err != nil {
		t.Fatal(err)
	}
	r, err := c.GC(context.Background(), GCRequest{})
	if err != nil || len(r.Plan.Targets) != 0 || len(r.Excluded) != 1 {
		t.Fatal("gc included a live node", r, err)
	}
	if _, err = firecracker.Observe(os.Getpid()); err != nil {
		t.Fatal(err)
	}
}
func TestGCDryRunKeepsStoppedAndKeepFailedEnvironments(t *testing.T) {
	c, root, e := environmentFixture(t)
	ctx := context.Background()
	for _, phase := range []string{"stopped", "ready", "running"} {
		e.State = phase
		if err := saveEnvironment(root, &e); err != nil {
			t.Fatal(err)
		}
		r, err := c.GC(ctx, GCRequest{})
		if err != nil || len(r.Plan.Targets) != 0 || len(r.Excluded) != 1 {
			t.Fatal(phase, r, err)
		}
	}
	e.State = "failed"
	e.KeepFailed = true
	if err := saveEnvironment(root, &e); err != nil {
		t.Fatal(err)
	}
	r, err := c.GC(ctx, GCRequest{})
	if err != nil || len(r.Plan.Targets) != 0 {
		t.Fatal(r, err)
	}
}
func TestGCPlanExpiryChangedStateAndLockRejectedWithoutDeletion(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("gc apply privilege gate")
	}
	c, root, e := environmentFixture(t)
	ctx := context.Background()
	e.State = "failed"
	if err := saveEnvironment(root, &e); err != nil {
		t.Fatal(err)
	}
	r, err := c.GC(ctx, GCRequest{})
	if err != nil || len(r.Plan.Targets) != 1 {
		t.Fatal(r, err)
	}
	plan := *r.Plan
	expired := plan
	expired.CreatedAt = time.Now().Add(-20 * time.Minute).UTC()
	expired.ExpiresAt = expired.CreatedAt.Add(10 * time.Minute)
	if _, err = c.GC(ctx, GCRequest{Apply: true, PlanFile: writePlan(t, expired)}); err == nil {
		t.Fatal("applied expired plan")
	}
	path := writePlan(t, plan)
	unlock, err := tryEnvironmentLock(root, e.Name)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.GC(ctx, GCRequest{Apply: true, PlanFile: path})
	unlock()
	if err == nil {
		t.Fatal("applied while environment operation active")
	}
	if err = os.WriteFile(filepath.Join(nodeDir(root, e.ID, e.Nodes[0].ID), "new-resource"), []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = c.GC(ctx, GCRequest{Apply: true, PlanFile: path}); err == nil {
		t.Fatal("applied stale resource snapshot")
	}
	if _, err = os.Stat(filepath.Join(nodeDir(root, e.ID, e.Nodes[0].ID), "new-resource")); err != nil {
		t.Fatal("stale plan deleted resource", err)
	}
}
func TestGCApplyOwnedFailedEnvironmentAndPreserveDiagnostics(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("gc apply privilege gate")
	}
	c, root, e := environmentFixture(t)
	ctx := context.Background()
	e.State = "failed"
	if err := saveEnvironment(root, &e); err != nil {
		t.Fatal(err)
	}
	disk := filepath.Join(nodeDir(root, e.ID, e.Nodes[0].ID), "rootfs.raw")
	if err := os.WriteFile(disk, []byte("disposable"), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "plan.json")
	r, err := c.GC(ctx, GCRequest{PlanFile: path})
	if err != nil || len(r.Plan.Targets) != 1 {
		t.Fatal(r, err)
	}
	if _, err = os.Stat(disk); err != nil {
		t.Fatal("dry-run deleted disk", err)
	}
	applied, err := c.GC(ctx, GCRequest{Apply: true, PlanFile: path})
	if err != nil || !applied.Applied || len(applied.Cleaned) != 1 {
		t.Fatal(applied, err)
	}
	if _, err = os.Stat(disk); !os.IsNotExist(err) {
		t.Fatal("owned failed disk not removed")
	}
	loaded, err := loadEnvironment(root, e.ID)
	if err != nil || loaded.State != "deleted" {
		t.Fatal(loaded, err)
	}
	if _, err = c.GC(ctx, GCRequest{Apply: true, PlanFile: path}); err == nil {
		t.Fatal("reused changed snapshot plan")
	}
}
func TestGCActiveOperationExcludedAndUnknownResourcesNotClaimed(t *testing.T) {
	c, root, e := environmentFixture(t)
	ctx := context.Background()
	e.State = "starting"
	if err := saveEnvironment(root, &e); err != nil {
		t.Fatal(err)
	}
	unlock, err := tryEnvironmentLock(root, e.Name)
	if err != nil {
		t.Fatal(err)
	}
	r, err := c.GC(ctx, GCRequest{})
	unlock()
	if err != nil || len(r.Plan.Targets) != 0 {
		t.Fatal(r, err)
	}
	operations := filepath.Join(envDir(root, e.ID), "operations")
	if err = os.Mkdir(operations, 0700); err != nil {
		t.Fatal(err)
	}
	op := state.UUID()
	j, err := state.NewOperationJournal(filepath.Join(operations, op+".json"), e.ID, op)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = j.Intent("unknown-system-object"); err != nil {
		t.Fatal(err)
	}
	r, err = c.GC(ctx, GCRequest{})
	if err != nil || len(r.Plan.Targets) != 0 || len(r.Excluded) != 1 {
		t.Fatal(r, err)
	}
}
