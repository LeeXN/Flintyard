package allocation

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/LeeXN/Flintyard/internal/network"
	"github.com/LeeXN/Flintyard/internal/state"
)

func TestIDsExcludeAccountsDelegationAndActiveReservations(t *testing.T) {
	r, err := ParseRange("60000:60009")
	if err != nil {
		t.Fatal(err)
	}
	a := Accounts{UIDs: map[uint32]bool{60000: true}, GIDs: map[uint32]bool{60001: true}, SubUIDs: []Range{{60002, 60004}}, SubGIDs: []Range{{60005, 60006}}}
	ids, err := ChooseIDs(r, a, []uint32{60007}, 2)
	if err != nil || ids[0] != 60008 || ids[1] != 60009 {
		t.Fatal(ids, err)
	}
	if _, err := ChooseIDs(r, a, []uint32{60007}, 3); err == nil {
		t.Fatal("reused reserved IDs")
	}
	for _, s := range []string{"0:1", "60001:60000", "1:4294967295", "1", "x:4"} {
		if _, err := ParseRange(s); err == nil {
			t.Fatal("accepted", s)
		}
	}
	for _, s := range []string{"user:1:0", "user:4294967295:2", "user:x:1", "user:1"} {
		if _, err := parseSubordinate(s); err == nil {
			t.Fatal("accepted delegation", s)
		}
	}
}
func requestFixture(t *testing.T) Request {
	t.Helper()
	root, err := state.OpenRoot(context.Background(), filepath.Join(t.TempDir(), "state"), true)
	if err != nil {
		t.Fatal(err)
	}
	return Request{Root: root, OperationID: state.UUID(), ReservationID: state.UUID(), UIDRange: Range{60000, 60010}, Network: network.AllocationRequest{RootID: root.ID, EnvironmentID: state.UUID(), CIDR: "auto", Nodes: []network.NodeRequest{{ID: state.UUID(), Name: "node-1"}, {ID: state.UUID(), Name: "node-2"}, {ID: state.UUID(), Name: "node-3"}}}}
}
func TestRegistryCrossRootReservationsAndExactRelease(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "runtime")
	if err := state.PrivateDir(dir); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "allocations.json")
	accounts := Accounts{UIDs: map[uint32]bool{}, GIDs: map[uint32]bool{}}
	a := requestFixture(t)
	b := requestFixture(t)
	first, err := reserveLocked(ctx, path, a, network.Snapshot{}, accounts)
	if err != nil {
		t.Fatal(err)
	}
	second, err := reserveLocked(ctx, path, b, network.Snapshot{}, accounts)
	if err != nil {
		t.Fatal(err)
	}
	if first.Plan.CIDR == second.Plan.CIDR || first.Plan.Nodes[0].UID == second.Plan.Nodes[0].UID {
		t.Fatal("cross-root allocation reused resources")
	}
	coord := Coordinator{RuntimeDir: dir}
	forged := first
	forged.RootPath = b.Root.Path
	if coord.Release(ctx, forged) == nil {
		t.Fatal("released a different owner")
	}
	if err = coord.Release(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err = coord.Release(ctx, first); err != nil {
		t.Fatal("repeat release", err)
	}
	current, err := readRegistry(path)
	if err != nil || len(current.Records) != 1 || current.Records[0].ID != second.ID {
		t.Fatal(current, err)
	}
}
func TestRetainedReservationsSurviveRuntimeRegistryLoss(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "allocations.json")
	accounts := Accounts{UIDs: map[uint32]bool{}, GIDs: map[uint32]bool{}}
	a := requestFixture(t)
	old, err := reserveLocked(ctx, path, a, network.Snapshot{}, accounts)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	a.Retained = []Reservation{old}
	a.OperationID = state.UUID()
	a.ReservationID = state.UUID()
	a.Network.EnvironmentID = state.UUID()
	for i := range a.Network.Nodes {
		a.Network.Nodes[i].ID = state.UUID()
	}
	next, err := reserveLocked(ctx, path, a, network.Snapshot{}, accounts)
	if err != nil {
		t.Fatal(err)
	}
	if old.Plan.CIDR == next.Plan.CIDR || old.Plan.Nodes[0].UID == next.Plan.Nodes[0].UID {
		t.Fatal("forgot stopped reservation after /run loss")
	}
}
