package flintyard

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/LeeXN/Flintyard/internal/host"
	"github.com/LeeXN/Flintyard/internal/image"
	"github.com/LeeXN/Flintyard/internal/state"
)

func upInputFixture(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	b, err := os.ReadFile("../../examples/import-spec.json")
	if err != nil {
		t.Fatal(err)
	}
	var spec image.Spec
	if err = json.Unmarshal(b, &spec); err != nil {
		t.Fatal(err)
	}
	spec.Architecture = host.Architecture()
	spec.Initialization.Mode = "external"
	spec.Initialization.Profile = ""
	spec.Kernel.Config = ""
	for i := range spec.Inputs {
		entry := &spec.Inputs[i]
		data := []byte("NONBOOTABLE preflight contract fixture " + entry.Role)
		if entry.Role == "kernel" {
			data = make([]byte, 4096)
			if spec.Architecture == "aarch64" {
				copy(data[56:], "ARM\x64")
			} else {
				copy(data[0x202:], "HdrS")
				data[0x1fe], data[0x1ff], data[0x236] = 0x55, 0xaa, 1
			}
		}
		entry.Path = entry.Role
		entry.SHA256 = image.Sum(data)
		if err = os.WriteFile(filepath.Join(dir, entry.Path), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	b, _ = json.Marshal(spec)
	specPath := filepath.Join(dir, "import.json")
	if err = os.WriteFile(specPath, b, 0600); err != nil {
		t.Fatal(err)
	}
	packagePath := filepath.Join(dir, "contract.fyimg")
	if _, err = image.PackSpec(context.Background(), specPath, packagePath); err != nil {
		t.Fatal(err)
	}
	b, err = os.ReadFile("../../examples/environment.json")
	if err != nil {
		t.Fatal(err)
	}
	var env map[string]any
	if err = json.Unmarshal(b, &env); err != nil {
		t.Fatal(err)
	}
	env["architecture"] = host.Architecture()
	access := env["access"].(map[string]any)
	delete(access, "public_key")
	access["probe"] = map[string]any{"type": "none", "port": 22}
	node := env["nodes"].([]any)[0].(map[string]any)
	node["image"] = packagePath
	node["memory_mib"] = 128
	node["vcpu_count"] = 1
	env["nodes"] = []any{node}
	b, _ = json.Marshal(env)
	config := filepath.Join(dir, "environment.json")
	if err = os.WriteFile(config, b, 0600); err != nil {
		t.Fatal(err)
	}
	return dir, config
}
func TestUpPreflightFailureHasNoEnvironmentOrRuntimeMutation(t *testing.T) {
	dir, file := upInputFixture(t)
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var spec EnvironmentSpec
	if err = json.Unmarshal(data, &spec); err != nil {
		t.Fatal(err)
	}
	spec.Network.NAT = true
	data, err = json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(file, data, 0600); err != nil {
		t.Fatal(err)
	}
	// With NAT enabled and no ip on PATH, checking connectivity first would
	// return a network error before the host prerequisite failure.
	t.Setenv("PATH", dir)
	root := filepath.Join(dir, "state")
	c := New(root)
	// A nonexistent private binary path guarantees refusal even on a KVM host.
	_, err = c.Up(context.Background(), UpRequest{File: file, Firecracker: filepath.Join(dir, "missing-firecracker"), Jailer: filepath.Join(dir, "missing-jailer")})
	if err == nil || ExitCode(err) != 3 {
		t.Fatal(err)
	}
	if _, err = os.Stat(root); !os.IsNotExist(err) {
		t.Fatal("preflight mutated state", err)
	}
}

func TestRollbackFailurePreservesOperationDetails(t *testing.T) {
	cleanupErr := errors.New("failed to save the final state")
	orig := Failure("E_CANCELLED", "launch", context.Canceled)
	orig.Node = "node-1"
	orig.Retryable = true
	previous := make([]string, 1, 2)
	previous[0] = "previous cleanup error"
	orig.Details = map[string]any{"resource": "disk", "cleanup_errors": previous}

	failure := rollbackFailure(orig, cleanupErr)
	if failure == orig || failure.Code != orig.Code || failure.Stage != orig.Stage ||
		failure.Message != orig.Message || failure.Node != orig.Node || !failure.Retryable ||
		!failure.CleanupIncomplete || ExitCode(failure) != 7 ||
		!errors.Is(failure, context.Canceled) || !errors.Is(failure, cleanupErr) {
		t.Fatalf("unexpected rollback error: %+v", failure)
	}
	details := failure.Details.(map[string]any)
	if details["resource"] != "disk" ||
		!slices.Equal(details["cleanup_errors"].([]string), []string{previous[0], cleanupErr.Error()}) {
		t.Fatalf("lost operation details: %+v", details)
	}
	details["resource"] = "changed"
	if orig.CleanupIncomplete || orig.Details.(map[string]any)["resource"] != "disk" || previous[:2][1] != "" {
		t.Fatalf("modified the original error: %+v", orig)
	}

	raw := errors.New("unexpected launch error")
	failure = rollbackFailure(raw, cleanupErr)
	if failure.Code != "E_LAUNCH_FAILED" || failure.Stage != "rollback" ||
		!errors.Is(failure, raw) || !errors.Is(failure, cleanupErr) || ExitCode(failure) != 7 {
		t.Fatalf("unexpected rollback error for an unclassified operation: %+v", failure)
	}
}
func TestUpCancellationBeforeMutationAndCleanupExitPriority(t *testing.T) {
	dir, file := upInputFixture(t)
	root := filepath.Join(dir, "state")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := New(root).Up(ctx, UpRequest{File: file})
	if !errors.Is(err, context.Canceled) || ExitCode(err) != 130 {
		t.Fatal(err)
	}
	if _, err = os.Stat(root); !os.IsNotExist(err) {
		t.Fatal("cancelled preflight created state")
	}
	cleanup := Failure("E_CANCELLED", "rollback", context.Canceled)
	cleanup.CleanupIncomplete = true
	if ExitCode(cleanup) != 7 {
		t.Fatal("cancellation hid residual resources")
	}
}
func TestStopDownRetainLogsAndDoNotTouchSiblingOrCache(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("public stop/down privilege gate; ordinary file helpers are covered separately")
	}
	c, root, e := environmentFixture(t)
	ctx := context.Background()
	dir := nodeDir(root, e.ID, e.Nodes[0].ID)
	for _, name := range []string{"rootfs.raw", "seed.raw"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("node data"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "logs"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "logs", "serial.log"), []byte("startup evidence"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root.Path, "images"), 0700); err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(root.Path, "images", "cache-sentinel")
	if err := os.WriteFile(cache, []byte("cache data"), 0400); err != nil {
		t.Fatal(err)
	}
	foreign := envDir(root, e.ID) + "-foreign"
	if err := os.Mkdir(foreign, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(foreign, "retain"), []byte("unrelated"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Stop(ctx, StopRequest{Environment: e.ID}); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "rootfs.raw")); err != nil || string(b) != "node data" {
		t.Fatal("stop discarded disk", err)
	}
	r, err := c.Down(ctx, DownRequest{Environment: e.ID})
	if err != nil || r.State != "deleted" {
		t.Fatal(r, err)
	}
	if _, err = os.Stat(filepath.Join(dir, "rootfs.raw")); !os.IsNotExist(err) {
		t.Fatal("down retained writable disk")
	}
	for _, path := range []string{cache, filepath.Join(foreign, "retain"), filepath.Join(dir, "logs", "serial.log")} {
		if _, err = os.Stat(path); err != nil {
			t.Fatal("removed retained state", path, err)
		}
	}
	if _, err = c.Down(ctx, DownRequest{Environment: e.ID}); err != nil {
		t.Fatal("repeat down", err)
	}
	r, err = c.Down(ctx, DownRequest{Environment: e.ID, Purge: true})
	if err != nil || !r.DiagnosticsPurged {
		t.Fatal(r, err)
	}
	if _, err = os.Stat(envDir(root, e.ID)); !os.IsNotExist(err) {
		t.Fatal("purge retained diagnostics")
	}
}
func TestDownDoesNotCallMissingStateADeletedEnvironment(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root for public mutation gate")
	}
	c, root, e := environmentFixture(t)
	if err := os.Remove(filepath.Join(envDir(root, e.ID), "state.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(envDir(root, e.ID), "recovery.json")); err != nil {
		t.Fatal(err)
	}
	r, err := c.Down(context.Background(), DownRequest{Environment: e.ID})
	if err == nil || r.AlreadyAbsent || r.State == "deleted" {
		t.Fatal("reported lost state as successful cleanup", r, err)
	}
}

func TestEnvironmentBootstrapPublishesCompleteState(t *testing.T) {
	root, err := state.OpenRoot(context.Background(), filepath.Join(t.TempDir(), "root"), true)
	if err != nil {
		t.Fatal(err)
	}
	id := state.UUID()
	owner := state.Owner{RootID: root.ID, EnvironmentID: id}
	e := environmentState{APIVersion: APIVersion, Kind: "EnvironmentState", RootID: root.ID, ID: id, Name: "boot", State: "preparing", Architecture: host.Architecture(), Spec: EnvironmentSpec{Name: "boot", Architecture: host.Architecture()}}
	e.Spec.Nodes = []NodeSpec{{Name: "node-1"}}
	e.Nodes = []NodeState{{ID: state.UUID(), Name: "node-1", State: "preparing"}}
	if err = state.OwnedDirContents(envDir(root, id), owner, map[string]any{"state.json": e}); err != nil {
		t.Fatal(err)
	}
	if got, err := loadEnvironment(root, id); err != nil || got.Name != "boot" {
		t.Fatal(got, err)
	}
}

func TestUnknownActiveJournalRetainsDiskAndOwnership(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("public mutation privilege gate")
	}
	c, root, e := environmentFixture(t)
	disk := filepath.Join(nodeDir(root, e.ID, e.Nodes[0].ID), "rootfs.raw")
	if err := os.WriteFile(disk, []byte("retain on unknown resource"), 0600); err != nil {
		t.Fatal(err)
	}
	op := state.UUID()
	operations := filepath.Join(envDir(root, e.ID), "operations")
	if err := os.Mkdir(operations, 0700); err != nil {
		t.Fatal(err)
	}
	j, err := state.NewOperationJournal(filepath.Join(operations, op+".json"), e.ID, op)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = j.IntentIdentity("unrecognized-resource", map[string]string{"owner": e.ID}); err != nil {
		t.Fatal(err)
	}
	r, err := c.Down(context.Background(), DownRequest{Environment: e.ID})
	if err == nil || r.State != "failed" || ExitCode(err) != 7 {
		t.Fatal(r, err)
	}
	if b, err := os.ReadFile(disk); err != nil || string(b) != "retain on unknown resource" {
		t.Fatal("deleted disk before resolving an unknown resource", err)
	}
}
