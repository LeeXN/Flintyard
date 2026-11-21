package flintyard

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/LeeXN/Flintyard/internal/allocation"
	"github.com/LeeXN/Flintyard/internal/firecracker"
	"github.com/LeeXN/Flintyard/internal/network"
	"github.com/LeeXN/Flintyard/internal/state"
	"golang.org/x/sys/unix"
)

func verifyStoppedDisks(root state.Root, e environmentState) error {
	if e.State != "stopped" || e.Reservation == nil {
		return fmt.Errorf("stopped environment has no retained allocation")
	}
	journals, err := environmentJournals(root, e)
	if err != nil {
		return err
	}
	for _, n := range e.Nodes {
		dir := nodeDir(root, e.ID, n.ID)
		if err := state.CheckPrivateDir(dir); err != nil {
			return err
		}
		if err := state.VerifyOwner(dir, state.Owner{RootID: root.ID, EnvironmentID: e.ID, NodeID: n.ID}); err != nil {
			return err
		}
		if n.Disk.Path != filepath.Join(dir, "rootfs.raw") {
			return fmt.Errorf("retained disk path changed")
		}
		f, err := os.OpenFile(n.Disk.Path, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
		if err != nil {
			return err
		}
		st, err := f.Stat()
		f.Close()
		if err != nil {
			return err
		}
		s := st.Sys().(*syscall.Stat_t)
		if !st.Mode().IsRegular() || uint64(s.Dev) != n.Disk.Device || s.Ino != n.Disk.Inode || st.Size() != n.Disk.Size {
			return fmt.Errorf("retained writable disk identity/size changed")
		}
		if n.Seed != "" {
			if n.Seed != filepath.Join(dir, "seed.raw") {
				return fmt.Errorf("retained seed path changed")
			}
			f, err := os.OpenFile(n.Seed, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
			if err != nil {
				return err
			}
			st, err := f.Stat()
			f.Close()
			if err != nil {
				return err
			}
			if !st.Mode().IsRegular() {
				return fmt.Errorf("retained seed is not an ordinary file")
			}
			seedStat := st.Sys().(*syscall.Stat_t)
			matched := false
			for _, j := range journals {
				for _, resource := range j.Resources {
					if resource.Kind == "node-seed" && resource.State == "done" && resource.Identity["node_id"] == n.ID && resource.Identity["path"] == n.Seed {
						matched = resource.Identity["device"] == strconv.FormatUint(uint64(seedStat.Dev), 10) && resource.Identity["inode"] == strconv.FormatUint(seedStat.Ino, 10)
					}
				}
			}
			if !matched {
				return fmt.Errorf("retained seed identity is missing or changed")
			}
		}
	}
	return nil
}

func (c *Client) resumeStopped(ctx context.Context, root state.Root, e environmentState, input resolvedInput, binaries firecracker.Binaries, layout firecracker.CgroupLayout, r UpRequest, ids allocation.Range) (result EnvironmentResult, err error) {
	result = resultFromState(e)
	if err = verifyStoppedDisks(root, e); err != nil {
		return result, Failure("E_PROCESS_IDENTITY", "disks", err)
	}
	// Reconcile old runtime resources before starting anything new. This path
	// preserves disks, seed, networking and the reservation even on failure.
	if err = c.cleanupEnvironment(ctx, root, &e, true); err != nil {
		return result, Failure("E_CLEANUP_INCOMPLETE", "cleanup", err)
	}
	all, err := scanEnvironments(ctx, root)
	if err != nil {
		return result, err
	}
	var retained []allocation.Reservation
	for _, other := range all {
		if other.ID != e.ID && other.State != "deleted" && other.Reservation != nil {
			retained = append(retained, *other.Reservation)
		}
	}
	if err = (allocation.Coordinator{}).EnsureStopped(ctx, *e.Reservation, retained, ids); err != nil {
		return result, networkFailure("preflight", err)
	}
	oldJournals, err := environmentJournals(root, e)
	if err != nil {
		return result, err
	}
	present, err := (network.Manager{}).ReconcileStopped(ctx, e.Reservation.Plan, oldJournals)
	if err != nil {
		return result, Failure("E_NETWORK_CONFLICT", "network", err)
	}
	operations := filepath.Join(envDir(root, e.ID), "operations")
	if err = state.PrivateDir(operations); err != nil {
		return result, err
	}
	j, err := state.NewOperationJournal(filepath.Join(operations, c.OperationID+".json"), e.ID, c.OperationID)
	if err != nil {
		return result, err
	}
	previous := append([]NodeState(nil), e.Nodes...)
	newOps := map[string]bool{j.OperationID: true}
	start := len(e.Operations)
	e.Operations = append(e.Operations, j.OperationID)
	e.State = "starting"
	e.KeepFailed = r.KeepFailed
	defer func() {
		if err == nil {
			return
		}
		e.State = "failed"
		var cleanupErr error
		if !r.KeepFailed {
			for _, op := range e.Operations[start:] {
				newOps[op] = true
			}
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			cleanupErr = rollbackRestart(cleanupCtx, root, &e, newOps)
			if cleanupErr == nil {
				e.Nodes = previous
				e.State = "stopped"
				e.KeepFailed = false
			}
		}
		cleanupErr = errors.Join(cleanupErr, saveEnvironment(root, &e))
		result = resultFromState(e)
		if cleanupErr != nil {
			var original *Error
			if !errors.As(err, &original) {
				original = Failure("E_LAUNCH_FAILED", "rollback", err)
			}
			copy := *original
			copy.CleanupIncomplete = true
			copy.Cause = errors.Join(err, cleanupErr)
			copy.Details = map[string]any{"cleanup_errors": []string{cleanupErr.Error()}}
			err = &copy
		}
	}()
	if err = saveEnvironment(root, &e); err != nil {
		return result, err
	}
	if !present {
		if _, err = (network.Manager{}).Create(ctx, network.CreateRequest{Plan: e.Reservation.Plan, Journal: j, KeepFailed: true}); err != nil {
			return result, networkFailure("network", err)
		}
	}
	if err = c.launchNodes(ctx, root, &e, input, binaries, layout, r, operations); err != nil {
		return result, err
	}
	e.State = "ready"
	for _, n := range e.Nodes {
		if !n.LastProbePassed {
			e.State = "running"
		}
	}
	if err = saveEnvironment(root, &e); err != nil {
		return result, err
	}
	if err = c.emit(Event{Stage: "commit", Status: "succeeded", ResourceID: e.ID, Message: "stopped environment resumed with its original disks and identities"}); err != nil {
		return result, Failure("E_CANCELLED", "commit", err)
	}
	result = resultFromState(e)
	for i := range result.Nodes {
		result.Nodes[i].Stale = false
		result.Nodes[i].Access.ProbePassed = e.Nodes[i].LastProbePassed
		result.Nodes[i].Access.Probe = effectiveProbe(e.Spec.Access.Probe.Type, r.Wait)
	}
	return result, nil
}
func rollbackRestart(ctx context.Context, root state.Root, e *environmentState, operations map[string]bool) error {
	journals, err := environmentJournals(root, *e)
	if err != nil {
		return err
	}
	var selected []*state.Journal
	for _, j := range journals {
		if operations[j.OperationID] {
			selected = append(selected, j)
		}
	}
	var failures []error
	for _, j := range selected {
		if err = firecracker.StopJournalProcesses(ctx, root.ID, j); err != nil {
			failures = append(failures, err)
		}
	}
	if len(failures) > 0 {
		return errors.Join(failures...)
	}
	for _, j := range selected {
		if err = firecracker.RemoveJails(ctx, root.ID, j); err != nil {
			failures = append(failures, err)
		}
		if err = firecracker.RemoveCgroups(ctx, j); err != nil {
			failures = append(failures, err)
		}
		for _, resource := range j.Resources {
			if resource.Kind == "netns" && resource.State != "released" {
				if e.Reservation == nil {
					failures = append(failures, fmt.Errorf("new network lost its ownership plan"))
				} else if _, err = (network.Manager{}).Remove(ctx, e.Reservation.Plan, j); err != nil {
					failures = append(failures, err)
				}
				break
			}
		}
	}
	return errors.Join(failures...)
}
