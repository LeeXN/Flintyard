package flintyard

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/LeeXN/Flintyard/internal/firecracker"
	"github.com/LeeXN/Flintyard/internal/host"
	"github.com/LeeXN/Flintyard/internal/image"
	"github.com/LeeXN/Flintyard/internal/state"
)

func environmentFixture(t *testing.T) (*Client, state.Root, environmentState) {
	t.Helper()
	ctx := context.Background()
	root, err := state.OpenRoot(ctx, filepath.Join(t.TempDir(), "state"), true)
	if err != nil {
		t.Fatal(err)
	}
	id, node := state.UUID(), state.UUID()
	owner := state.Owner{RootID: root.ID, EnvironmentID: id}
	if err = state.OwnedDir(envDir(root, id), owner); err != nil {
		t.Fatal(err)
	}
	e := environmentState{APIVersion: APIVersion, Kind: "EnvironmentState", RootID: root.ID, ID: id, Name: "lab", State: "stopped", Architecture: host.Architecture(), CreatedAt: time.Now().UTC(), Spec: EnvironmentSpec{APIVersion: APIVersion, Kind: "Environment", Name: "lab", Architecture: host.Architecture(), Access: AccessSpec{Probe: image.Probe{Type: "tcp", Port: 22}}}, Nodes: []NodeState{{ID: node, Name: "node-1", State: "stopped", Username: "user"}}, Operations: []string{}}
	e.Spec.Nodes = []NodeSpec{{Name: "node-1"}}
	if err = os.Mkdir(filepath.Join(envDir(root, id), "nodes"), 0700); err != nil {
		t.Fatal(err)
	}
	owner.NodeID = node
	if err = state.OwnedDir(nodeDir(root, id, node), owner); err != nil {
		t.Fatal(err)
	}
	if err = saveEnvironment(root, &e); err != nil {
		t.Fatal(err)
	}
	return New(root.Path), root, e
}
func TestListMissingRootIsReadOnlyAndMissingOwnerFailsClosed(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "absent")
	r, err := New(dir).List(context.Background(), ListRequest{})
	if err != nil || len(r.Environments) != 0 {
		t.Fatal(r, err)
	}
	if _, err = os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("readonly list initialized state")
	}
	c, root, _ := environmentFixture(t)
	if err = os.Remove(filepath.Join(root.Path, "host.json")); err != nil {
		t.Fatal(err)
	}
	if _, err = c.List(context.Background(), ListRequest{}); err == nil {
		t.Fatal("hid environments whose ownership root was lost")
	}
}
func TestStoredTerminalStateAndOwnedStartupLogs(t *testing.T) {
	c, root, e := environmentFixture(t)
	ctx := context.Background()
	r, err := c.Status(ctx, StatusRequest{Environment: "lab"})
	if err != nil || r.State != "stopped" || r.Nodes[0].Access.Authenticated {
		t.Fatal(r, err)
	}
	logs := filepath.Join(nodeDir(root, e.ID, e.Nodes[0].ID), "logs")
	if err = os.Mkdir(logs, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(logs, "serial.log")
	if err = os.WriteFile(path, []byte("one\ntwo\nthree\n"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := c.Logs(ctx, LogsRequest{Environment: e.ID, Node: "node-1", Tail: 2})
	if err != nil || got.Content != "two\nthree\n" {
		t.Fatal(got, err)
	}
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink("/etc/passwd", path); err != nil {
		t.Fatal(err)
	}
	if _, err = c.Logs(ctx, LogsRequest{Environment: "lab", Node: "node-1"}); err == nil {
		t.Fatal("followed a startup log symlink")
	}
}
func TestObservationUsesKernelAndDoesNotReuseHistoricalReadiness(t *testing.T) {
	c, root, e := environmentFixture(t)
	ctx := context.Background()
	// This is a contract fixture using this test process and a local HTTP Unix
	// socket, not a Firecracker, jailer, KVM or guest acceptance test.
	identity, err := firecracker.Observe(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	base, err := os.MkdirTemp(os.TempDir(), "fy-observe-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(base)
	run := filepath.Join(base, "run")
	owner := state.Owner{RootID: root.ID, EnvironmentID: e.ID, NodeID: e.Nodes[0].ID}
	if err = state.OwnedDir(run, owner); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(filepath.Join(run, "api"), 0700); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(run, "api", "api.sock")
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"id": e.Nodes[0].ID, "state": "Running", "vmm_version": host.FirecrackerVersion})
	})}
	defer server.Close()
	go server.Serve(l)
	e.State = "ready"
	e.Nodes[0].State = "ready"
	e.Nodes[0].Process = &identity
	e.Nodes[0].Jail = &firecracker.Jail{RunDir: run, Socket: socket}
	e.Nodes[0].LastProbePassed = true
	when := time.Now().Add(-time.Hour).UTC()
	e.Nodes[0].LastProbeAt = &when
	if err = saveEnvironment(root, &e); err != nil {
		t.Fatal(err)
	}
	r, err := c.Status(ctx, StatusRequest{Environment: "lab"})
	if err != nil || r.State != "running" || !r.Nodes[0].Stale || r.Nodes[0].Access.ProbePassed || !r.Nodes[0].LastProbePassed {
		t.Fatal(r, err)
	}
	e.Nodes[0].Process.StartTime++
	if err = saveEnvironment(root, &e); err != nil {
		t.Fatal(err)
	}
	r, err = c.Status(ctx, StatusRequest{Environment: "lab"})
	if err == nil || r.State != "degraded" || r.Nodes[0].State != "degraded" {
		t.Fatal("accepted reused process identity", r, err)
	}
}
func TestPendingLauncherRecoveredFromJournalIsNotReportedExited(t *testing.T) {
	c, root, e := environmentFixture(t)
	e.State = "starting"
	if err := saveEnvironment(root, &e); err != nil {
		t.Fatal(err)
	}
	operations := filepath.Join(envDir(root, e.ID), "operations")
	if err := os.Mkdir(operations, 0700); err != nil {
		t.Fatal(err)
	}
	op := state.UUID()
	j, err := state.NewOperationJournal(filepath.Join(operations, op+".json"), e.ID, op)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = j.IntentIdentity("vmm-process", map[string]string{"node_id": e.Nodes[0].ID}); err != nil {
		t.Fatal(err)
	}
	r, err := c.Status(context.Background(), StatusRequest{Environment: e.ID})
	if err == nil || r.Nodes[0].State != "unknown" || r.State == "ready" {
		t.Fatal("claimed missing process record meant exited", r, err)
	}
}
