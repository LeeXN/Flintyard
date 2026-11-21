package flintyard

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/LeeXN/Flintyard/internal/allocation"
	"github.com/LeeXN/Flintyard/internal/image"
	"github.com/LeeXN/Flintyard/internal/network"
	"github.com/LeeXN/Flintyard/internal/state"
)

func stoppedFixture(t *testing.T) (*Client, state.Root, environmentState) {
	t.Helper()
	c, root, e := environmentFixture(t)
	p, err := network.Allocate(network.AllocationRequest{RootID: root.ID, EnvironmentID: e.ID, CIDR: "10.240.98.0/24", Nodes: []network.NodeRequest{{ID: e.Nodes[0].ID, Name: e.Nodes[0].Name, UID: 64000, GID: 64000}}})
	if err != nil {
		t.Fatal(err)
	}
	e.Reservation = &allocation.Reservation{ID: state.UUID(), RootPath: root.Path, OperationID: state.UUID(), Plan: p}
	path := filepath.Join(nodeDir(root, e.ID, e.Nodes[0].ID), "rootfs.raw")
	if err = os.WriteFile(path, []byte("persistent guest modifications"), 0600); err != nil {
		t.Fatal(err)
	}
	identity, err := pathIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	e.Nodes[0].Disk = image.Disk{Path: path, Device: mustUint(t, identity["device"]), Inode: mustUint(t, identity["inode"]), Size: int64(len("persistent guest modifications"))}
	if err = saveEnvironment(root, &e); err != nil {
		t.Fatal(err)
	}
	return c, root, e
}
func mustUint(t *testing.T, s string) uint64 {
	t.Helper()
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestStoppedDiskValidationAndRestartRollbackPreserveChanges(t *testing.T) {
	_, root, e := stoppedFixture(t)
	if err := verifyStoppedDisks(root, e); err != nil {
		t.Fatal(err)
	}
	oldID, oldMAC, oldIP := e.Nodes[0].ID, e.Reservation.Plan.Nodes[0].MAC, e.Reservation.Plan.Nodes[0].IPv4
	operations := filepath.Join(envDir(root, e.ID), "operations")
	if err := os.Mkdir(operations, 0700); err != nil {
		t.Fatal(err)
	}
	op := state.UUID()
	j, err := state.NewOperationJournal(filepath.Join(operations, op+".json"), e.ID, op)
	if err != nil {
		t.Fatal(err)
	}
	owner := state.Owner{RootID: root.ID, EnvironmentID: e.ID, NodeID: e.Nodes[0].ID}
	runtimeBase, err := os.MkdirTemp(os.TempDir(), "fy-restart-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(runtimeBase)
	path := filepath.Join(runtimeBase, image.Sum([]byte(owner.Tag() + ":" + op))[:16])
	id, err := j.IntentIdentity("runtime-dir", map[string]string{"path": path, "runtime_base": runtimeBase, "owner": owner.Tag(), "node_id": owner.NodeID})
	if err != nil {
		t.Fatal(err)
	}
	if err = state.OwnedDir(path, owner); err != nil {
		t.Fatal(err)
	}
	if err = j.Done(id, j.Resources[0].Identity); err != nil {
		t.Fatal(err)
	}
	if err = rollbackRestart(context.Background(), root, &e, map[string]bool{op: true}); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(e.Nodes[0].Disk.Path); err != nil || string(b) != "persistent guest modifications" {
		t.Fatal("restart rollback destroyed original disk", err)
	}
	if e.Nodes[0].ID != oldID || e.Reservation.Plan.Nodes[0].MAC != oldMAC || e.Reservation.Plan.Nodes[0].IPv4 != oldIP {
		t.Fatal("restart changed node identity")
	}
	if err = verifyStoppedDisks(root, e); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(e.Nodes[0].Disk.Path, e.Nodes[0].Disk.Path+".original"); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(e.Nodes[0].Disk.Path, []byte("persistent guest modifications"), 0600); err != nil {
		t.Fatal(err)
	}
	if verifyStoppedDisks(root, e) == nil {
		t.Fatal("accepted same-size replacement inode")
	}
}
