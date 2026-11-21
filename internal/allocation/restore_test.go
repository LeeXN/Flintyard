package allocation

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/LeeXN/Flintyard/internal/network"
)

func TestStoppedRestoreKeepsIDsAndRejectsCredentialOrOtherRootConflicts(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "registry.json")
	a := Accounts{UIDs: map[uint32]bool{}, GIDs: map[uint32]bool{}}
	req := requestFixture(t)
	record, err := reserveLocked(ctx, path, req, network.Snapshot{}, a)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err = restoreStoppedLocked(ctx, path, record, nil, req.UIDRange, a); err != nil {
		t.Fatal(err)
	}
	got, err := readRegistry(path)
	if err != nil || len(got.Records) != 1 || got.Records[0].ID != record.ID || got.Records[0].Plan.Nodes[0] != record.Plan.Nodes[0] {
		t.Fatal(got, err)
	}
	a.UIDs[record.Plan.Nodes[0].UID] = true
	if restoreStoppedLocked(ctx, path, record, nil, req.UIDRange, a) == nil {
		t.Fatal("resumed with a newly occupied host UID")
	}
	delete(a.UIDs, record.Plan.Nodes[0].UID)
	otherReq := requestFixture(t)
	otherReq.Network.CIDR = record.Plan.CIDR.String()
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err = reserveLocked(ctx, path, otherReq, network.Snapshot{}, a); err != nil {
		t.Fatal(err)
	}
	if restoreStoppedLocked(ctx, path, record, nil, req.UIDRange, a) == nil {
		t.Fatal("restored over another root's subnet reservation")
	}
}
