package flintyard

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/LeeXN/Flintyard/internal/state"
)

func TestMissingPrimaryCheckpointKeepsStoppedAndCommittedEnvironments(t *testing.T) {
	for _, phase := range []string{"stopped", "ready", "running"} {
		t.Run(phase, func(t *testing.T) {
			c, root, e := environmentFixture(t)
			e.State = phase
			if err := saveEnvironment(root, &e); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(envDir(root, e.ID), "state.json")
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			recovered, err := loadEnvironment(root, e.ID)
			if err != nil || recovered.State != phase || !recovered.Recovered || resultFromState(recovered).RecoverySource != "checkpoint" {
				t.Fatal(recovered, err)
			}
			if _, err = os.Stat(path); !os.IsNotExist(err) {
				t.Fatal("read-only recovery wrote primary", err)
			}
			gc, err := c.GC(context.Background(), GCRequest{})
			if err != nil || len(gc.Plan.Targets) != 0 || len(gc.Excluded) != 1 {
				t.Fatal("retained checkpoint entered GC", gc, err)
			}
		})
	}
}
func TestCheckpointDoesNotMaskCorruptPrimaryOrChangedRecovery(t *testing.T) {
	_, root, e := environmentFixture(t)
	dir := envDir(root, e.ID)
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadEnvironment(root, e.ID); err == nil {
		t.Fatal("corrupt primary silently replaced")
	}
	if err := os.Remove(filepath.Join(dir, "state.json")); err != nil {
		t.Fatal(err)
	}
	var checkpoint environmentCheckpoint
	if err := state.Read(filepath.Join(dir, "recovery.json"), &checkpoint); err != nil {
		t.Fatal(err)
	}
	checkpoint.Environment.Name = "changed"
	if err := state.Write(filepath.Join(dir, "recovery.json"), checkpoint); err != nil {
		t.Fatal(err)
	}
	if _, err := loadEnvironment(root, e.ID); err == nil {
		t.Fatal("changed recovery accepted")
	}
}
func TestCheckpointAllowsExplicitDownWithoutClaimingMissingMeansDeleted(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("normal down requires root")
	}
	c, root, e := environmentFixture(t)
	if err := os.Remove(filepath.Join(envDir(root, e.ID), "state.json")); err != nil {
		t.Fatal(err)
	}
	r, err := c.Down(context.Background(), DownRequest{Environment: e.ID})
	if err != nil || r.AlreadyAbsent || r.State != "deleted" {
		t.Fatal(r, err)
	}
	current, err := loadEnvironment(root, e.ID)
	if err != nil || current.Recovered || current.State != "deleted" {
		t.Fatal(current, err)
	}
}
func TestGCUnknownStateDoesNotHideVerifiableSibling(t *testing.T) {
	c, root, e := environmentFixture(t)
	e.State = "failed"
	if err := saveEnvironment(root, &e); err != nil {
		t.Fatal(err)
	}
	unknown := state.UUID()
	if err := os.Mkdir(filepath.Join(root.Path, "envs", unknown), 0700); err != nil {
		t.Fatal(err)
	}
	r, err := c.GC(context.Background(), GCRequest{})
	if err != nil || len(r.Plan.Targets) != 1 || len(r.Excluded) != 1 || r.Excluded[0].EnvironmentID != unknown {
		t.Fatal(r, err)
	}
}
