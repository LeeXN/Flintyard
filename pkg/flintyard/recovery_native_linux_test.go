package flintyard

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/LeeXN/Flintyard/internal/firecracker"
	"github.com/LeeXN/Flintyard/internal/state"
	"golang.org/x/sys/unix"
)

func nativeRecoveryOnly(t *testing.T) {
	t.Helper()
	if os.Getenv("FY_RECOVERY_INTEGRATION") != "1" {
		t.Skip("requires tests/native-recovery.sh; no privileged recovery checks performed")
	}
	self, _ := os.Readlink("/proc/self/ns/mnt")
	init, _ := os.Readlink("/proc/1/ns/mnt")
	var fs unix.Statfs_t
	if os.Geteuid() != 0 || self == init || unix.Statfs("/run", &fs) != nil || fs.Type != unix.TMPFS_MAGIC {
		t.Fatal("refusing recovery tests outside isolated mount namespace/tmpfs /run")
	}
}
func TestNativeRecoveryGCReclaimsOwnedEmptyCgroupsAndDisk(t *testing.T) {
	nativeRecoveryOnly(t)
	ctx := context.Background()
	layout, err := firecracker.DiscoverCgroup()
	if err != nil {
		t.Skipf("BLOCKED: cgroup delegation: %v", err)
	}
	var fs unix.Statfs_t
	if err = unix.Statfs(layout.Mount, &fs); err != nil {
		t.Fatal(err)
	}
	if fs.Flags&unix.ST_RDONLY != 0 {
		t.Skip("BLOCKED: read-only cgroup hierarchy")
	}
	c, root, e := environmentFixture(t)
	e.State = "failed"
	operations := filepath.Join(envDir(root, e.ID), "operations")
	if err = os.Mkdir(operations, 0700); err != nil {
		t.Fatal(err)
	}
	op := state.UUID()
	j, err := state.NewOperationJournal(filepath.Join(operations, op+".json"), e.ID, op)
	if err != nil {
		t.Fatal(err)
	}
	owner := state.Owner{RootID: root.ID, EnvironmentID: e.ID, NodeID: e.Nodes[0].ID}
	cg, err := firecracker.PrepareCgroup(ctx, firecracker.CgroupRequest{Layout: layout, Owner: owner, Anchor: envDir(root, e.ID), MemoryMiB: 128, Journal: j})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = firecracker.RemoveCgroups(ctx, j) })
	e.Nodes[0].Cgroup = &cg
	disk := filepath.Join(nodeDir(root, e.ID, owner.NodeID), "rootfs.raw")
	id, err := j.IntentIdentity("node-disk", map[string]string{"node_id": owner.NodeID, "path": disk})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(disk, []byte("owned disposable bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	identity, err := pathIdentity(disk)
	if err != nil {
		t.Fatal(err)
	}
	if err = j.Done(id, map[string]string{"node_id": owner.NodeID, "path": disk, "device": identity["device"], "inode": identity["inode"]}); err != nil {
		t.Fatal(err)
	}
	if err = saveEnvironment(root, &e); err != nil {
		t.Fatal(err)
	}
	plan := filepath.Join(t.TempDir(), "plan.json")
	r, err := c.GC(ctx, GCRequest{PlanFile: plan})
	if err != nil || len(r.Plan.Targets) != 1 {
		t.Fatal(r, err)
	}
	if _, err = os.Stat(cg.Path); err != nil {
		t.Fatal("dry-run removed cgroup", err)
	}
	r, err = c.GC(ctx, GCRequest{Apply: true, PlanFile: plan})
	if err != nil || !r.Applied {
		t.Fatal(r, err)
	}
	for _, path := range []string{cg.Path, cg.Parent, disk} {
		if _, err = os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("resource not reclaimed", path, err)
		}
	}
}
func TestNativeRecoveryGCRejectsNewForeignBindBeforeAnyDeletion(t *testing.T) {
	nativeRecoveryOnly(t)
	ctx := context.Background()
	c, root, e := environmentFixture(t)
	e.State = "failed"
	disk := filepath.Join(nodeDir(root, e.ID, e.Nodes[0].ID), "rootfs.raw")
	if err := os.WriteFile(disk, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := saveEnvironment(root, &e); err != nil {
		t.Fatal(err)
	}
	r, err := c.GC(ctx, GCRequest{})
	if err != nil || len(r.Plan.Targets) != 1 {
		t.Fatal(r, err)
	}
	plan := writePlan(t, *r.Plan)
	foreign := filepath.Join(t.TempDir(), "foreign")
	if err = os.WriteFile(foreign, []byte("retain foreign bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = unix.Mount(foreign, disk, "", unix.MS_BIND, ""); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Unmount(disk, 0) })
	if _, err = c.GC(ctx, GCRequest{Apply: true, PlanFile: plan}); err == nil {
		t.Fatal("applied stale plan after foreign mount appeared")
	}
	if b, err := os.ReadFile(foreign); err != nil || string(b) != "retain foreign bytes" {
		t.Fatal("foreign source changed", err)
	}
	if b, err := os.ReadFile(disk); err != nil || string(b) != "retain foreign bytes" {
		t.Fatal("foreign bind changed", err)
	}
	loaded, err := loadEnvironment(root, e.ID)
	if err != nil || loaded.State != "failed" {
		t.Fatal("changed phase before all plan validation", loaded, err)
	}
}
