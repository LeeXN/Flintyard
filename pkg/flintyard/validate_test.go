package flintyard

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/LeeXN/Flintyard/internal/image"
	"github.com/LeeXN/Flintyard/internal/state"
)

func TestValidatePreflightRecordsFailureWithoutBootClaimsOrEnvironment(t *testing.T) {
	dir, _ := upInputFixture(t)
	root := filepath.Join(dir, "validate-state")
	c := New(root)
	path := filepath.Join(dir, "contract.fyimg")
	before, _, err := image.Hash(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	result, err := c.ValidateImage(context.Background(), ValidateRequest{Image: path, Firecracker: filepath.Join(dir, "missing-firecracker"), Jailer: filepath.Join(dir, "missing-jailer")})
	if err == nil || ExitCode(err) != 3 || result.Validation != "failed" || !result.StaticPassed || result.BootPassed || result.AccessPassed || result.EnvironmentID != "" || result.CleanupIncomplete {
		t.Fatal(result, err)
	}
	var record validationRecord
	if err = state.Read(result.RecordPath, &record); err != nil {
		t.Fatal(err)
	}
	if record.FinishedAt == nil || record.Error == nil || record.HostBootID == "" || record.HostKernel == "" || record.ObservedFirecrackerVersion != "" || record.Result.AccessPassed {
		t.Fatal(record)
	}
	entries, err := os.ReadDir(filepath.Join(root, "envs"))
	if err != nil || len(entries) != 0 {
		t.Fatal("preflight created a VM environment", entries, err)
	}
	after, _, err := image.Hash(context.Background(), path)
	if err != nil || after != before {
		t.Fatal("validation mutated portable package", err)
	}
}

func TestValidationCleanupPolicyUsesOwnedDownAndPreservesDiagnostics(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("normal down requires root; rerun this cleanup policy case with mutation authority")
	}
	for _, keep := range []bool{false, true} {
		t.Run(map[bool]string{false: "default-clean", true: "keep-failed"}[keep], func(t *testing.T) {
			c, root, e := environmentFixture(t)
			logDir := filepath.Join(nodeDir(root, e.ID, e.Nodes[0].ID), "logs")
			if err := os.Mkdir(logDir, 0700); err != nil {
				t.Fatal(err)
			}
			log := filepath.Join(logDir, "serial.log")
			if err := os.WriteFile(log, []byte("diagnostic remains\n"), 0600); err != nil {
				t.Fatal(err)
			}
			out := ValidationResult{EnvironmentID: e.ID}
			if err := c.cleanupValidation(&out, keep, context.Canceled); err != nil {
				t.Fatal(err)
			}
			if out.Retained != keep {
				t.Fatal(out)
			}
			stored, err := loadEnvironment(root, e.ID)
			if err != nil {
				t.Fatal(err)
			}
			if keep && stored.State != "stopped" || !keep && stored.State != "deleted" {
				t.Fatal(stored.State)
			}
			if _, err = os.Stat(log); err != nil {
				t.Fatal("cleanup discarded diagnostic log", err)
			}
		})
	}
	// --keep-failed does not retain a successful temporary environment.
	c, root, e := environmentFixture(t)
	out := ValidationResult{EnvironmentID: e.ID}
	if err := c.cleanupValidation(&out, true, nil); err != nil {
		t.Fatal(err)
	}
	stored, err := loadEnvironment(root, e.ID)
	if err != nil || stored.State != "deleted" || out.Retained {
		t.Fatal(out, stored.State, err)
	}
}

func TestValidationCleanupRefusesChangedOwner(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("root is needed to reach ownership checks beyond down's privilege gate")
	}
	c, root, e := environmentFixture(t)
	owner := filepath.Join(envDir(root, e.ID), "owner.json")
	var marker state.Owner
	if err := state.Read(owner, &marker); err != nil {
		t.Fatal(err)
	}
	marker.RootID = state.UUID()
	if err := state.Write(owner, marker); err != nil {
		t.Fatal(err)
	}
	out := ValidationResult{EnvironmentID: e.ID}
	err := c.cleanupValidation(&out, false, context.Canceled)
	if err == nil || !out.CleanupIncomplete || ExitCode(err) != 7 {
		t.Fatal(out, err)
	}
	if _, err := os.Stat(owner); err != nil {
		t.Fatal("unowned diagnostics removed", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Fatal("cleanup replaced its actual ownership failure")
	}
}

func TestValidationRejectsUnavailableAccessEvidenceBeforeStateMutation(t *testing.T) {
	for _, mode := range []string{"no-probe", "seed-without-key", "external-with-key", "other-architecture"} {
		t.Run(mode, func(t *testing.T) {
			dir, _ := upInputFixture(t)
			var spec image.Spec
			b, err := os.ReadFile(filepath.Join(dir, "import.json"))
			if err != nil {
				t.Fatal(err)
			}
			if err = json.Unmarshal(b, &spec); err != nil {
				t.Fatal(err)
			}
			request := ValidateRequest{}
			switch mode {
			case "no-probe":
				spec.Initialization.Probe.Type = "none"
			case "seed-without-key":
				spec.Initialization.Mode = "seed-v1"
				spec.Initialization.Profile = "systemd-networkd"
			case "external-with-key":
				request.PublicKey = "no-key-is-read"
			case "other-architecture":
				if spec.Architecture == "x86_64" {
					spec.Architecture = "aarch64"
				} else {
					spec.Architecture = "x86_64"
				}
			}
			b, _ = json.Marshal(spec)
			if err = os.WriteFile(filepath.Join(dir, "validate-spec.json"), b, 0600); err != nil {
				t.Fatal(err)
			}
			request.Image = filepath.Join(dir, "validate-image.fyimg")
			if _, err = image.PackSpec(context.Background(), filepath.Join(dir, "validate-spec.json"), request.Image); err != nil {
				t.Fatal(err)
			}
			root := filepath.Join(dir, "never-created")
			result, err := New(root).ValidateImage(context.Background(), request)
			want := 2
			if mode == "other-architecture" {
				want = 3
			}
			if err == nil || ExitCode(err) != want || result.BootPassed || result.AccessPassed || result.RecordPath != "" {
				t.Fatal(result, err)
			}
			if _, err = os.Stat(root); !os.IsNotExist(err) {
				t.Fatal("invalid request created persistent state", err)
			}
		})
	}
}
