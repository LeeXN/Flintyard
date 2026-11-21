package flintyard

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/LeeXN/Flintyard/internal/allocation"
	"github.com/LeeXN/Flintyard/internal/contract"
	"github.com/LeeXN/Flintyard/internal/firecracker"
	"github.com/LeeXN/Flintyard/internal/host"
	"github.com/LeeXN/Flintyard/internal/network"
	"github.com/LeeXN/Flintyard/internal/state"
)

func environmentJournals(root state.Root, e environmentState) ([]*state.Journal, error) {
	path := filepath.Join(envDir(root, e.ID), "operations")
	entries, err := os.ReadDir(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var result []*state.Journal
	for _, entry := range entries {
		op := strings.TrimSuffix(entry.Name(), ".json")
		if !state.ValidUUID(op) || entry.Name() != op+".json" {
			continue
		}
		j, err := state.ReadJournal(filepath.Join(path, entry.Name()), e.ID)
		if err != nil {
			return nil, err
		}
		if j.OperationID != op {
			return nil, fmt.Errorf("operation journal name/header mismatch")
		}
		result = append(result, j)
	}
	return result, nil
}
func (c *Client) cleanupEnvironment(ctx context.Context, root state.Root, e *environmentState, preserveDisks bool) error {
	if err := state.VerifyOwner(envDir(root, e.ID), state.Owner{RootID: root.ID, EnvironmentID: e.ID}); err != nil {
		return err
	}
	journals, err := environmentJournals(root, *e)
	if err != nil {
		return err
	}
	knownNodes := map[string]bool{}
	for _, node := range e.Nodes {
		knownNodes[node.ID] = true
	}
	for _, j := range journals {
		for _, resource := range j.Resources {
			if resource.State == "released" {
				continue
			}
			nodeID := ""
			switch resource.Kind {
			case "node-disk", "node-seed", "jail-dir", "jail-mount", "runtime-dir", "vmm-process":
				nodeID = resource.Identity["node_id"]
			case "cgroup-leaf":
				nodeID = filepath.Base(resource.Identity["path"])
			case "allocation", "netns", "bridge", "veth", "tap", "nft-host", "nft-ns", "cgroup-parent":
				continue
			default:
				return fmt.Errorf("unsupported active resource kind %s; retaining environment", resource.Kind)
			}
			if !knownNodes[nodeID] {
				return fmt.Errorf("resource journal refers to unknown node")
			}
			if resource.Kind == "node-disk" || resource.Kind == "node-seed" {
				name := "rootfs.raw"
				if resource.Kind == "node-seed" {
					name = "seed.raw"
				}
				if resource.Identity["path"] != filepath.Join(nodeDir(root, e.ID, nodeID), name) {
					return fmt.Errorf("node disk journal path mismatch")
				}
			}
		}
	}
	var failures []error
	for _, j := range journals {
		if err = firecracker.StopJournalProcesses(ctx, root.ID, j); err != nil {
			failures = append(failures, err)
		}
	}
	if len(failures) > 0 {
		return errors.Join(failures...)
	}
	for _, n := range e.Nodes {
		if n.Process != nil {
			phase, err := firecracker.ProcessState(ctx, *n.Process)
			if err != nil {
				failures = append(failures, err)
			} else if phase == "running" {
				failures = append(failures, fmt.Errorf("unreconciled live process for node %s", n.Name))
			}
		}
	}
	if len(failures) > 0 {
		return errors.Join(failures...)
	}
	for i := range e.Nodes {
		e.Nodes[i].Process = nil
	}
	for _, j := range journals {
		if err = firecracker.RemoveJails(ctx, root.ID, j); err != nil {
			failures = append(failures, err)
		}
		if err = firecracker.RemoveCgroups(ctx, j); err != nil {
			failures = append(failures, err)
		}
	}
	if preserveDisks {
		if len(failures) == 0 {
			for i := range e.Nodes {
				e.Nodes[i].State = "stopped"
				e.Nodes[i].Jail = nil
				e.Nodes[i].Cgroup = nil
			}
		}
		return errors.Join(failures...)
	}
	if e.Reservation == nil {
		for _, j := range journals {
			for _, r := range j.Resources {
				if r.Kind != "allocation" || r.State == "released" {
					continue
				}
				if text := r.Identity["reservation"]; text != "" {
					var record allocation.Reservation
					if err = contract.Decode([]byte(text), &record); err != nil {
						return errors.Join(append(failures, err)...)
					}
					if record.Plan.RootID != root.ID || record.Plan.EnvironmentID != e.ID {
						return fmt.Errorf("allocation ownership mismatch")
					}
					e.Reservation = &record
				} else {
					records, err := (allocation.Coordinator{}).Reservations(ctx)
					if err != nil {
						return errors.Join(append(failures, err)...)
					}
					for _, record := range records {
						if record.ID == r.Identity["id"] {
							if record.Plan.RootID != root.ID || record.Plan.EnvironmentID != e.ID {
								return fmt.Errorf("allocation owner mismatch")
							}
							copy := record
							e.Reservation = &copy
						}
					}
				}
			}
		}
	}
	for _, j := range journals {
		hasNetwork := false
		for _, resource := range j.Resources {
			if resource.Kind == "netns" && resource.State != "released" {
				hasNetwork = true
			}
		}
		if !hasNetwork {
			continue
		}
		if e.Reservation == nil {
			failures = append(failures, fmt.Errorf("network ownership plan is missing"))
			continue
		}
		if _, err = (network.Manager{}).Remove(ctx, e.Reservation.Plan, j); err != nil {
			failures = append(failures, err)
		}
	}
	if len(failures) > 0 {
		return errors.Join(failures...)
	}
	for _, node := range e.Nodes {
		dir := nodeDir(root, e.ID, node.ID)
		if _, err = os.Lstat(dir); os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if err = state.CheckPrivateDir(dir); err != nil {
			return err
		}
		if err = state.VerifyOwner(dir, state.Owner{RootID: root.ID, EnvironmentID: e.ID, NodeID: node.ID}); err != nil {
			return err
		}
		mounts, err := host.Mounts()
		if err != nil {
			return err
		}
		for _, mount := range mounts {
			if host.Within(dir, mount.Path) {
				return fmt.Errorf("owned node scope still has a mount")
			}
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		// The node UUID + owner marker identifies an exclusively managed scope,
		// not a name prefix. Retain startup logs and ownership metadata by default.
		for _, entry := range entries {
			if entry.Name() == "logs" || entry.Name() == "owner.json" {
				continue
			}
			if err = os.RemoveAll(filepath.Join(dir, entry.Name())); err != nil {
				return err
			}
		}
	}
	for _, j := range journals {
		for _, resource := range append([]state.Resource(nil), j.Resources...) {
			if resource.State == "released" {
				continue
			}
			switch resource.Kind {
			case "node-disk", "node-seed":
				if resource.State == "intent" {
					err = j.Absent(resource.ID)
				} else {
					err = j.Released(resource.ID)
				}
				if err != nil {
					return err
				}
			}
		}
	}
	if e.Reservation != nil {
		if err = (allocation.Coordinator{}).Release(ctx, *e.Reservation); err != nil {
			return err
		}
	}
	for _, j := range journals {
		for _, resource := range append([]state.Resource(nil), j.Resources...) {
			if resource.Kind == "allocation" && resource.State != "released" {
				if resource.State == "intent" {
					err = j.Absent(resource.ID)
				} else {
					err = j.Released(resource.ID)
				}
				if err != nil {
					return err
				}
			}
		}
	}
	for _, j := range journals {
		for _, resource := range j.Resources {
			if resource.State != "released" {
				return fmt.Errorf("unreleased %s resource %s", resource.Kind, resource.ID)
			}
		}
	}
	for i := range e.Nodes {
		e.Nodes[i].State = "deleted"
		e.Nodes[i].Jail = nil
		e.Nodes[i].Cgroup = nil
	}
	return nil
}

func (c *Client) Stop(ctx context.Context, r StopRequest) (EnvironmentResult, error) {
	var result EnvironmentResult
	if os.Geteuid() != 0 {
		return result, Failure("E_PERMISSION", "preflight", fmt.Errorf("stop requires root"))
	}
	root, err := state.OpenRoot(ctx, c.StateDir, false)
	if err != nil {
		return result, Failure("E_SPEC_CONFLICT", "preflight", err)
	}
	e, err := findEnvironment(ctx, root, r.Environment)
	if err != nil {
		return result, Failure("E_SPEC_CONFLICT", "preflight", err)
	}
	unlock, err := lockEnvironment(ctx, root, e.Name)
	if err != nil {
		return result, Failure("E_LOCKED", "preflight", err)
	}
	defer unlock()
	e, err = loadEnvironment(root, e.ID)
	if err != nil {
		return result, Failure("E_SPEC_CONFLICT", "preflight", err)
	}
	if e.State == "deleted" {
		return resultFromState(e), nil
	}
	if err = c.emit(Event{Stage: "cleanup", Status: "started", ResourceID: e.ID, Message: "stopping environment"}); err != nil {
		return resultFromState(e), Failure("E_LOG_IO", "cleanup", err)
	}
	if err = c.cleanupEnvironment(ctx, root, &e, true); err != nil {
		e.State = "failed"
		_ = saveEnvironment(root, &e)
		return resultFromState(e), Failure("E_CLEANUP_INCOMPLETE", "cleanup", err)
	}
	e.State = "stopped"
	if err = saveEnvironment(root, &e); err != nil {
		return resultFromState(e), Failure("E_CLEANUP_INCOMPLETE", "commit", err)
	}
	if err = c.emit(Event{Stage: "cleanup", Status: "succeeded", ResourceID: e.ID, Message: "environment stopped"}); err != nil {
		return resultFromState(e), Failure("E_LOG_IO", "cleanup", err)
	}
	return resultFromState(e), nil
}
func (c *Client) Down(ctx context.Context, r DownRequest) (CleanupResult, error) {
	return c.down(ctx, r, true)
}

func (c *Client) down(ctx context.Context, r DownRequest, audit bool) (CleanupResult, error) {
	result := CleanupResult{State: "unknown", Errors: []string{}}
	if os.Geteuid() != 0 {
		return result, Failure("E_PERMISSION", "preflight", fmt.Errorf("down requires root; it discards environment disks"))
	}
	root, err := state.OpenRoot(ctx, c.StateDir, false)
	if os.IsNotExist(err) {
		entries, e := os.ReadDir(filepath.Join(c.StateDir, "envs"))
		if e == nil && len(entries) > 0 {
			return result, Failure("E_SPEC_CONFLICT", "preflight", fmt.Errorf("environment state exists without root metadata"))
		}
		if e := rejectLostReservation(ctx, c.StateDir, ""); e != nil {
			return result, e
		}
		result.AlreadyAbsent = true
		result.State = "deleted"
		return result, nil
	}
	if err != nil {
		return result, Failure("E_SPEC_CONFLICT", "preflight", err)
	}
	e, err := findEnvironment(ctx, root, r.Environment)
	if errors.Is(err, errEnvironmentNotFound) {
		if e := rejectLostReservation(ctx, root.Path, root.ID); e != nil {
			return result, e
		}
		result.AlreadyAbsent = true
		result.State = "deleted"
		return result, nil
	}
	if err != nil {
		return result, Failure("E_SPEC_CONFLICT", "preflight", err)
	}
	result.EnvironmentID, result.Name = e.ID, e.Name
	result.State = e.State
	unlock, err := lockEnvironment(ctx, root, e.Name)
	if err != nil {
		return result, Failure("E_LOCKED", "preflight", err)
	}
	defer unlock()
	e, err = loadEnvironment(root, e.ID)
	if err != nil {
		return result, Failure("E_SPEC_CONFLICT", "preflight", err)
	}
	if audit {
		if err = c.emit(Event{Stage: "cleanup", Status: "started", ResourceID: e.ID, Message: "removing environment resources"}); err != nil {
			return result, Failure("E_LOG_IO", "cleanup", err)
		}
	}
	if e.State != "deleted" {
		e.State = "deleting"
		if err = saveEnvironment(root, &e); err != nil {
			return result, Failure("E_SPEC_CONFLICT", "commit", err)
		}
		result.State = "deleting"
		if err = c.cleanupEnvironment(ctx, root, &e, false); err != nil {
			e.State = "failed"
			_ = saveEnvironment(root, &e)
			result.State = "failed"
			result.Errors = append(result.Errors, err.Error())
			return result, Failure("E_CLEANUP_INCOMPLETE", "cleanup", err)
		}
		e.State = "deleted"
		if err = saveEnvironment(root, &e); err != nil {
			return result, Failure("E_CLEANUP_INCOMPLETE", "commit", err)
		}
		result.State = "deleted"
	} else {
		result.AlreadyAbsent = true
	}
	if r.Purge {
		journals, e2 := environmentJournals(root, e)
		if e2 != nil {
			return result, Failure("E_CLEANUP_INCOMPLETE", "cleanup", e2)
		}
		for _, j := range journals {
			for _, resource := range j.Resources {
				if resource.State != "released" {
					return result, Failure("E_CLEANUP_INCOMPLETE", "cleanup", fmt.Errorf("diagnostics contain an active resource journal"))
				}
			}
		}
		mounts, e2 := host.Mounts()
		if e2 != nil {
			return result, Failure("E_CLEANUP_INCOMPLETE", "cleanup", e2)
		}
		for _, mount := range mounts {
			if host.Within(envDir(root, e.ID), mount.Path) {
				return result, Failure("E_CLEANUP_INCOMPLETE", "cleanup", fmt.Errorf("diagnostic scope contains a mount"))
			}
		}
		if err = state.VerifyOwner(envDir(root, e.ID), state.Owner{RootID: root.ID, EnvironmentID: e.ID}); err != nil {
			return result, Failure("E_CLEANUP_INCOMPLETE", "cleanup", err)
		}
		if err = os.RemoveAll(envDir(root, e.ID)); err != nil {
			result.Errors = append(result.Errors, err.Error())
			return result, Failure("E_CLEANUP_INCOMPLETE", "cleanup", err)
		}
		result.DiagnosticsPurged = true
	}
	if audit {
		if err = c.emit(Event{Stage: "cleanup", Status: "succeeded", ResourceID: e.ID, Message: "environment resources removed"}); err != nil {
			return result, Failure("E_LOG_IO", "cleanup", err)
		}
	}
	return result, nil
}

func rejectLostReservation(ctx context.Context, path, rootID string) error {
	records, err := (allocation.Coordinator{}).Existing(ctx)
	if err != nil {
		return Failure("E_SPEC_CONFLICT", "preflight", err)
	}
	for _, record := range records {
		if record.RootPath == path && (rootID == "" || record.Plan.RootID == rootID) {
			return Failure("E_SPEC_CONFLICT", "preflight", fmt.Errorf("runtime reservation exists without matching durable environment state; recovery required"))
		}
	}
	return nil
}
