package flintyard

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/LeeXN/Flintyard/internal/firecracker"
	"github.com/LeeXN/Flintyard/internal/host"
	"github.com/LeeXN/Flintyard/internal/network"
	"github.com/LeeXN/Flintyard/internal/state"
)

func (c *Client) Status(ctx context.Context, r StatusRequest) (EnvironmentResult, error) {
	root, err := state.OpenRoot(ctx, c.StateDir, false)
	if err != nil {
		return EnvironmentResult{}, stateRootFailure(err)
	}
	e, err := findEnvironment(ctx, root, r.Environment)
	if err != nil {
		return EnvironmentResult{}, Failure("E_SPEC_CONFLICT", "preflight", err)
	}
	unlock, err := lockEnvironment(ctx, root, e.Name)
	if err != nil {
		return EnvironmentResult{}, Failure("E_LOCKED", "preflight", err)
	}
	defer unlock()
	e, err = loadEnvironment(root, e.ID)
	if err != nil {
		return EnvironmentResult{}, Failure("E_SPEC_CONFLICT", "preflight", err)
	}
	if err = hydrateProcesses(root, &e); err != nil {
		return resultFromState(e), Failure("E_SPEC_CONFLICT", "preflight", err)
	}
	result, err := observeEnvironment(ctx, e, r.Probe)
	addStorageObservation(root, e, &result)
	return result, err
}
func (c *Client) List(ctx context.Context, r ListRequest) (ListResult, error) {
	result := ListResult{Environments: []EnvironmentResult{}}
	root, err := state.OpenRoot(ctx, c.StateDir, false)
	if os.IsNotExist(err) {
		entries, e := os.ReadDir(filepath.Join(c.StateDir, "envs"))
		if e == nil && len(entries) > 0 {
			return result, stateRootFailure(fmt.Errorf("environment state exists without root metadata"))
		}
		return result, nil
	}
	if err != nil {
		return result, stateRootFailure(err)
	}
	all, err := scanEnvironments(ctx, root)
	if err != nil {
		return result, Failure("E_SPEC_CONFLICT", "preflight", err)
	}
	for _, e := range all {
		if err := ctx.Err(); err != nil {
			return result, Failure("E_CANCELLED", "preflight", err)
		}
		unlock, err := lockEnvironment(ctx, root, e.Name)
		if err != nil {
			return result, Failure("E_LOCKED", "preflight", err)
		}
		current, err := loadEnvironment(root, e.ID)
		if os.IsNotExist(err) {
			unlock()
			continue
		}
		if err != nil {
			unlock()
			return result, Failure("E_SPEC_CONFLICT", "preflight", err)
		}
		if err = hydrateProcesses(root, &current); err != nil {
			unlock()
			return result, Failure("E_SPEC_CONFLICT", "preflight", err)
		}
		observed, err := observeEnvironment(ctx, current, r.Probe)
		unlock()
		result.Environments = append(result.Environments, observed)
		if err != nil {
			return result, err
		}
	}
	return result, nil
}
func observeEnvironment(ctx context.Context, e environmentState, probe bool) (EnvironmentResult, error) {
	r := resultFromState(e)
	var connectivityErr error
	if e.Reservation != nil && (e.Reservation.Plan.NAT || len(e.Reservation.Plan.Forwards) > 0) {
		p := e.Reservation.Plan
		if err := network.CheckPlanHost(ctx, p); err != nil {
			r.Network.Diagnostics = append(r.Network.Diagnostics, err.Error())
			connectivityErr = networkFailure("network", err)
		}
		warnings, err := network.PolicyDiagnostics(ctx, p)
		if err != nil {
			r.Network.Diagnostics = append(r.Network.Diagnostics, "cannot inspect host nft policy: "+err.Error())
		} else {
			r.Network.Diagnostics = append(r.Network.Diagnostics, warnings...)
		}
	}
	terminal := e.State == "deleted" || e.State == "stopped"
	hasProcess := false
	for _, n := range e.Nodes {
		if n.Process != nil || n.PendingLaunch {
			hasProcess = true
		}
	}
	if terminal && !hasProcess {
		for i := range r.Nodes {
			r.Nodes[i].State = e.State
		}
		return r, nil
	}
	allReady, allRunning := len(e.Nodes) > 0, len(e.Nodes) > 0
	var failure error
	failure = connectivityErr
	for i, node := range e.Nodes {
		if err := ctx.Err(); err != nil {
			return r, Failure("E_CANCELLED", "preflight", err)
		}
		n := &r.Nodes[i]
		if node.PendingLaunch {
			n.State = "unknown"
			n.ObservationError = Failure("E_PROCESS_IDENTITY", "launch", fmt.Errorf("launcher intent requires recovery"))
			failure = n.ObservationError
			allReady, allRunning = false, false
			continue
		}
		if node.Process == nil {
			n.State = "exited"
			allReady, allRunning = false, false
			continue
		}
		phase, err := firecracker.ProcessState(ctx, *node.Process)
		if err != nil {
			n.State = "degraded"
			n.ObservationError = Failure("E_PROCESS_IDENTITY", "preflight", err)
			allReady, allRunning = false, false
			failure = n.ObservationError
			continue
		}
		if phase != "running" {
			n.State = "exited"
			if terminal {
				n.State = e.State
			}
			allReady, allRunning = false, false
			continue
		}
		if terminal {
			n.State = "degraded"
			n.ObservationError = Failure("E_PROCESS_IDENTITY", "preflight", fmt.Errorf("terminal state still has a live process"))
			failure = n.ObservationError
			allReady, allRunning = false, false
			continue
		}
		if node.Jail == nil {
			n.State = "degraded"
			allReady, allRunning = false, false
			continue
		}
		if err := state.VerifyOwner(node.Jail.RunDir, state.Owner{RootID: e.RootID, EnvironmentID: e.ID, NodeID: node.ID}); err != nil {
			n.State = "degraded"
			n.ObservationError = Failure("E_PROCESS_IDENTITY", "preflight", err)
			failure = n.ObservationError
			allReady, allRunning = false, false
			continue
		}
		api := firecracker.NewAPI(node.Jail.Socket)
		var info struct {
			ID      string `json:"id"`
			State   string `json:"state"`
			Version string `json:"vmm_version"`
			AppName string `json:"app_name"`
		}
		err = api.Call(ctx, "GET", "/", nil, &info)
		api.Close()
		if err != nil || info.ID != node.ID || info.Version != host.FirecrackerVersion || info.State != "Running" {
			n.State = "degraded"
			allRunning, allReady = false, false
			if err == nil {
				err = fmt.Errorf("API instance identity/state mismatch")
			}
			n.ObservationError = Failure("E_API_TIMEOUT", "api", err)
			continue
		}
		n.State = "running"
		if !probe || n.Access.Probe == "none" {
			allReady = false
			continue
		}
		err = firecracker.Probe(ctx, net.JoinHostPort(n.IPv4, strconv.Itoa(n.Access.Port)), n.Access.Probe)
		n.Stale = false
		n.LastProbePassed = err == nil
		observed := time.Now().UTC()
		n.LastProbeAt = &observed
		if err != nil {
			n.State = "degraded"
			n.Access.ProbePassed = false
			allRunning, allReady = false, false
			n.ObservationError = Failure("E_ACCESS_TIMEOUT", "probe", err)
			continue
		}
		n.State = "ready"
		n.Access.ProbePassed = true
	}
	if allReady {
		r.State = "ready"
	} else if allRunning {
		r.State = "running"
	} else {
		r.State = "degraded"
	}
	if connectivityErr != nil {
		r.State = "degraded"
	}
	// A failed/preparing transaction remains failed until explicitly cleaned;
	// observing an API socket cannot retroactively commit it.
	if e.State != "ready" && e.State != "running" {
		r.State = e.State
	}
	if terminal && failure != nil {
		r.State = "degraded"
	}
	return r, failure
}

type LogsRequest struct {
	Environment, Node, Source string
	Tail                      int
}
type LogsResult struct {
	EnvironmentID string `json:"environment_id"`
	Node          string `json:"node"`
	Source        string `json:"source"`
	Content       string `json:"content"`
	Truncated     bool   `json:"truncated"`
}

func (c *Client) Logs(ctx context.Context, r LogsRequest) (LogsResult, error) {
	view, err := c.openStartupLog(ctx, r)
	if err != nil {
		return LogsResult{}, err
	}
	defer view.file.Close()
	b, truncated, _, err := tailLogFile(view.file, view.lines)
	if err != nil {
		return view.result, Failure("E_LOG_IO", "logs", err)
	}
	view.result.Content, view.result.Truncated = string(b), truncated
	return view.result, nil
}

type startupLog struct {
	file                           *os.File
	path, logsDir, envDir, nodeDir string
	owner                          state.Owner
	logsIdentity                   os.FileInfo
	result                         LogsResult
	lines                          int
}

// The environment lock protects selection and opening only. Streaming and its
// consumer callbacks must never prevent stop/down from taking that lock.
func (c *Client) openStartupLog(ctx context.Context, r LogsRequest) (*startupLog, error) {
	var result LogsResult
	if err := ctx.Err(); err != nil {
		return nil, Failure("E_CANCELLED", "preflight", err)
	}
	if r.Tail == 0 {
		r.Tail = 200
	}
	if r.Tail < 1 || r.Tail > 10000 {
		return nil, Failure("E_CONFIG_INVALID", "parse", fmt.Errorf("tail must be 1 through 10000"))
	}
	if r.Source == "" {
		r.Source = "serial"
	}
	if r.Source != "serial" && r.Source != "firecracker" {
		return nil, Failure("E_CONFIG_INVALID", "parse", fmt.Errorf("source must be serial or firecracker"))
	}
	root, err := state.OpenRoot(ctx, c.StateDir, false)
	if err != nil {
		return nil, stateRootFailure(err)
	}
	e, err := findEnvironment(ctx, root, r.Environment)
	if err != nil {
		return nil, Failure("E_SPEC_CONFLICT", "preflight", err)
	}
	unlock, err := lockEnvironment(ctx, root, e.Name)
	if err != nil {
		return nil, Failure("E_LOCKED", "preflight", err)
	}
	defer unlock()
	e, err = loadEnvironment(root, e.ID)
	if err != nil {
		return nil, Failure("E_SPEC_CONFLICT", "preflight", err)
	}
	var node *NodeState
	for _, n := range e.Nodes {
		if n.Name == r.Node {
			copy := n
			node = &copy
		}
	}
	if node == nil {
		return nil, Failure("E_CONFIG_INVALID", "parse", fmt.Errorf("node name is required and must exist"))
	}
	owner := state.Owner{RootID: root.ID, EnvironmentID: e.ID, NodeID: node.ID}
	dir := nodeDir(root, e.ID, node.ID)
	if err = state.VerifyOwner(dir, owner); err != nil {
		return nil, Failure("E_SPEC_CONFLICT", "preflight", err)
	}
	path := filepath.Join(dir, "logs", "serial.log")
	if err = state.CheckPrivateDir(filepath.Join(dir, "logs")); err != nil {
		return nil, Failure("E_SPEC_CONFLICT", "preflight", err)
	}
	if r.Source == "firecracker" {
		path = filepath.Join(dir, "logs", "firecracker.log")
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, Failure("E_DEPENDENCY", "preflight", err)
	}
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		f.Close()
		return nil, Failure("E_SPEC_CONFLICT", "preflight", errors.Join(err, fmt.Errorf("log must be an ordinary file")))
	}
	logsDir := filepath.Dir(path)
	logsIdentity, err := os.Lstat(logsDir)
	if err != nil {
		f.Close()
		return nil, Failure("E_SPEC_CONFLICT", "preflight", err)
	}
	result = LogsResult{EnvironmentID: e.ID, Node: node.Name, Source: r.Source}
	return &startupLog{file: f, path: path, logsDir: logsDir, envDir: envDir(root, e.ID), nodeDir: dir, owner: owner, logsIdentity: logsIdentity, result: result, lines: r.Tail}, nil
}

func tailLogFile(f *os.File, lines int) ([]byte, bool, int64, error) {
	st, err := f.Stat()
	if err != nil {
		return nil, false, 0, err
	}
	if !st.Mode().IsRegular() {
		return nil, false, 0, fmt.Errorf("log must be an ordinary file")
	}
	const limit = 8 << 20
	start := max(int64(0), st.Size()-limit)
	b := make([]byte, st.Size()-start)
	n, err := f.ReadAt(b, start)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, false, 0, err
	}
	b = b[:n]
	offset := start + int64(n)
	end := len(b)
	if end > 0 && b[end-1] == '\n' {
		end--
	}
	count := 0
	for i := end - 1; i >= 0; i-- {
		if b[i] == '\n' {
			count++
			if count == lines {
				return b[i+1:], false, offset, nil
			}
		}
	}
	return b, start > 0, offset, nil
}
