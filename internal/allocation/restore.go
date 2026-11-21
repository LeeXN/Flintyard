package allocation

import (
	"context"
	"fmt"
	"github.com/LeeXN/Flintyard/internal/network"
	"reflect"

	"github.com/LeeXN/Flintyard/internal/state"
)

// EnsureStopped restores a lost /run registration without allocating new IDs.
// The caller holds the environment lock and has established that its VMMs are
// stopped. New host accounts, subordinate delegations or processes must not
// silently gain access to a resumed node's writable material.
func (c Coordinator) EnsureStopped(ctx context.Context, want Reservation, retained []Reservation, ids Range) error {
	a, err := ReadAccounts()
	if err != nil {
		return err
	}
	if err = addProcessIDs(ctx, &a); err != nil {
		return err
	}
	path, lock, err := c.paths()
	if err != nil {
		return err
	}
	unlock, err := state.LockContext(ctx, lock)
	if err != nil {
		return err
	}
	defer unlock()
	if err = network.CheckPlanHost(ctx, want.Plan); err != nil {
		return err
	}
	return restoreStoppedLocked(ctx, path, want, retained, ids, a)
}
func restoreStoppedLocked(ctx context.Context, path string, want Reservation, retained []Reservation, ids Range, a Accounts) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateReservation(want); err != nil {
		return err
	}
	root, err := state.OpenRoot(ctx, want.RootPath, false)
	if err != nil {
		return err
	}
	if root.ID != want.Plan.RootID {
		return fmt.Errorf("restored state root identity changed")
	}
	for _, node := range want.Plan.Nodes {
		if node.UID < ids.First || node.UID > ids.Last || node.GID < ids.First || node.GID > ids.Last {
			return fmt.Errorf("retained node IDs are outside requested range")
		}
		if blocked(node.UID, a.UIDs, a.SubUIDs) || blocked(node.GID, a.GIDs, a.SubGIDs) {
			return fmt.Errorf("retained node IDs conflict with current host credentials or delegation")
		}
	}
	r, err := readRegistry(path)
	if err != nil {
		return err
	}
	for _, record := range append(append([]Reservation(nil), retained...), want) {
		if err := validateReservation(record); err != nil {
			return err
		}
		if record.RootPath != want.RootPath || record.Plan.RootID != want.Plan.RootID {
			return fmt.Errorf("retained record belongs to another root")
		}
		found := false
		for _, existing := range r.Records {
			if existing.ID == record.ID {
				if !reflect.DeepEqual(existing, record) {
					return fmt.Errorf("reservation identity changed")
				}
				found = true
				continue
			}
			if existing.RootPath == record.RootPath && existing.Plan.RootID != record.Plan.RootID || existing.Plan.RootID == record.Plan.RootID && existing.RootPath != record.RootPath {
				return fmt.Errorf("state root path/identity conflict")
			}
			if existing.Plan.EnvironmentID == record.Plan.EnvironmentID && existing.Plan.RootID == record.Plan.RootID {
				return fmt.Errorf("environment has a different reservation")
			}
			if existing.Plan.CIDR.Overlaps(record.Plan.CIDR) {
				return fmt.Errorf("retained subnet is now reserved by another environment")
			}
			if network.MappingConflict(existing.Plan.Forwards, record.Plan.Forwards) {
				return fmt.Errorf("%w: retained mapping is now reserved by another environment", network.ErrPortInUse)
			}
			for _, n := range record.Plan.Nodes {
				for _, other := range existing.Plan.Nodes {
					if n.UID == other.UID || n.GID == other.GID || n.MAC == other.MAC || n.Tap == other.Tap {
						return fmt.Errorf("retained node resources now conflict")
					}
				}
			}
		}
		if !found {
			r.Records = append(r.Records, record)
		}
	}
	return state.Write(path, r)
}
