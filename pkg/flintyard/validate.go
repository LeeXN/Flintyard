package flintyard

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/LeeXN/Flintyard/internal/host"
	"github.com/LeeXN/Flintyard/internal/image"
	"github.com/LeeXN/Flintyard/internal/state"
)

type ValidateRequest struct {
	Image, PublicKey, Firecracker, Jailer, UIDRange string
	KeepFailed                                      bool
}
type ValidationResult struct {
	ImageID           string         `json:"image_id"`
	Validation        string         `json:"validation"`
	StaticPassed      bool           `json:"static_passed"`
	BootPassed        bool           `json:"boot_passed"`
	AccessPassed      bool           `json:"access_passed"`
	EnvironmentID     string         `json:"environment_id,omitempty"`
	EnvironmentName   string         `json:"environment_name,omitempty"`
	DiagnosticsPath   string         `json:"diagnostics_path,omitempty"`
	RecordPath        string         `json:"record_path,omitempty"`
	Retained          bool           `json:"retained"`
	CleanupIncomplete bool           `json:"cleanup_incomplete"`
	Cleanup           *CleanupResult `json:"cleanup,omitempty"`
}
type validationRecord struct {
	APIVersion                 string           `json:"api_version"`
	Kind                       string           `json:"kind"`
	ID                         string           `json:"id"`
	RootID                     string           `json:"root_id"`
	OperationID                string           `json:"operation_id"`
	StartedAt                  time.Time        `json:"started_at"`
	FinishedAt                 *time.Time       `json:"finished_at,omitempty"`
	Architecture               string           `json:"architecture"`
	HostBootID                 string           `json:"host_boot_id"`
	HostKernel                 string           `json:"host_kernel"`
	HostPageSize               int              `json:"host_page_size"`
	ExpectedFirecrackerVersion string           `json:"expected_firecracker_version"`
	ObservedFirecrackerVersion string           `json:"observed_firecracker_version,omitempty"`
	Probe                      image.Probe      `json:"probe"`
	Result                     ValidationResult `json:"result"`
	Error                      *Error           `json:"error,omitempty"`
}

// ValidateImage uses the normal lifecycle and access probe, never a guest login
// or a second launcher. Sidecars are local host evidence, not manifest edits or
// promises that another host has the same capabilities.
func (c *Client) ValidateImage(ctx context.Context, r ValidateRequest) (out ValidationResult, err error) {
	out.Validation = "not_run"
	if r.Image == "" {
		return out, Failure("E_CONFIG_INVALID", "parse", fmt.Errorf("image is required"))
	}
	if err = c.emit(Event{Stage: "preflight", Status: "started", Message: "checking image for temporary guest validation"}); err != nil {
		return out, Failure("E_CANCELLED", "validate", err)
	}
	img, err := c.InspectImage(ctx, InspectRequest{Image: r.Image})
	if err != nil {
		return out, err
	}
	out.ImageID, out.StaticPassed = img.ID, true
	if img.Manifest.Architecture != host.Architecture() {
		return out, Failure("E_ARCH_MISMATCH", "preflight", fmt.Errorf("image architecture differs from native host"))
	}
	probe := img.Manifest.Initialization.Probe
	if probe.Type != "tcp" && probe.Type != "ssh_banner" {
		return out, Failure("E_CONFIG_INVALID", "parse", fmt.Errorf("image validate requires a declared TCP or SSH banner probe; no access evidence is available for probe=none"))
	}
	if img.Manifest.Initialization.Mode == "seed-v1" && r.PublicKey == "" {
		return out, Failure("E_CONFIG_INVALID", "parse", fmt.Errorf("seed-v1 validation requires public_key"))
	}
	if img.Manifest.Initialization.Mode == "external" && r.PublicKey != "" {
		return out, Failure("E_CONFIG_INVALID", "parse", fmt.Errorf("external initialization cannot inject a public key"))
	}
	if img.KernelIssue != "" {
		return out, Failure("E_KERNEL_INCOMPATIBLE", "preflight", errors.New(img.KernelIssue))
	}
	source := r.Image
	if !strings.HasPrefix(source, "sha256:") {
		source, err = filepath.Abs(source)
		if err != nil {
			return out, err
		}
	}
	if r.PublicKey != "" {
		r.PublicKey, err = filepath.Abs(r.PublicKey)
		if err != nil {
			return out, err
		}
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return out, Failure("E_DEPENDENCY", "preflight", err)
	}
	kernel, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		return out, Failure("E_DEPENDENCY", "preflight", err)
	}
	root, err := state.OpenRoot(ctx, c.StateDir, true)
	if err != nil {
		return out, stateRootFailure(err)
	}
	base := filepath.Join(root.Path, "validations", "sha256-"+strings.TrimPrefix(img.ID, "sha256:"))
	if err = state.PrivateDir(base); err != nil {
		return out, Failure("E_SPEC_CONFLICT", "commit", err)
	}
	unlock, err := state.LockContext(ctx, filepath.Join(base, "validation.lock"))
	if err != nil {
		return out, Failure("E_LOCKED", "preflight", err)
	}
	defer unlock()
	attempt := state.UUID()
	dir := filepath.Join(base, attempt)
	out.RecordPath = filepath.Join(dir, "validation.json")
	out.EnvironmentName = "validate-" + attempt
	out.Validation = "static_passed"
	record := validationRecord{APIVersion: APIVersion, Kind: "ImageValidation", ID: attempt, RootID: root.ID, OperationID: c.OperationID, StartedAt: time.Now().UTC(), Architecture: host.Architecture(), HostBootID: strings.TrimSpace(string(boot)), HostKernel: strings.TrimSpace(string(kernel)), HostPageSize: os.Getpagesize(), ExpectedFirecrackerVersion: host.FirecrackerVersion, Probe: probe, Result: out}
	spec := EnvironmentSpec{APIVersion: APIVersion, Kind: "Environment", Name: out.EnvironmentName, Architecture: img.Manifest.Architecture, Network: NetworkSpec{CIDR: "auto", DNS: []string{}}, Access: AccessSpec{PublicKey: r.PublicKey, Probe: probe}, Nodes: []NodeSpec{{Name: "validate-node", Image: source, VCPUs: 2, MemoryMiB: 1024}}, PortForwards: []PortForward{}}
	if err = state.OwnedDirContents(dir, state.Owner{RootID: root.ID, EnvironmentID: attempt}, map[string]any{"validation.json": record, "environment.json": spec}); err != nil {
		return out, Failure("E_SPEC_CONFLICT", "commit", err)
	}
	defer func() {
		if cleanupErr := c.cleanupValidation(&out, r.KeepFailed, err); cleanupErr != nil {
			failure := Failure("E_CLEANUP_INCOMPLETE", "cleanup", errors.Join(err, cleanupErr))
			failure.CleanupIncomplete = true
			err = failure
		}
		if err != nil {
			out.Validation = "failed"
		}
		finished := time.Now().UTC()
		record.FinishedAt = &finished
		record.Result = out
		if err != nil {
			var failure *Error
			if errors.As(err, &failure) {
				copy := *failure
				record.Error = &copy
			} else {
				record.Error = Failure("E_LAUNCH_FAILED", "validate", err)
			}
		}
		if e := state.Write(out.RecordPath, record); e != nil {
			out.Validation = "failed"
			err = errors.Join(err, Failure("E_SPEC_CONFLICT", "commit", fmt.Errorf("validation sidecar commit: %w", e)))
		}
	}()
	run, err := c.Up(ctx, UpRequest{File: filepath.Join(dir, "environment.json"), Wait: probe.Type, Firecracker: r.Firecracker, Jailer: r.Jailer, UIDRange: r.UIDRange, KeepFailed: r.KeepFailed, Concurrency: 1})
	out.EnvironmentID = run.EnvironmentID
	if state.ValidUUID(run.EnvironmentID) {
		out.DiagnosticsPath = envDir(root, run.EnvironmentID)
	}
	if len(run.Nodes) > 0 {
		record.ObservedFirecrackerVersion = run.Nodes[0].FirecrackerVersion
	}
	if err != nil {
		return out, err
	}
	if len(run.Nodes) != 1 || !run.Nodes[0].LastProbePassed || run.Nodes[0].LastProbeAt == nil || record.ObservedFirecrackerVersion != host.FirecrackerVersion {
		return out, Failure("E_ACCESS_TIMEOUT", "probe", fmt.Errorf("normal lifecycle did not return fresh matching-version access evidence"))
	}
	// A successful userspace TCP/SSH probe establishes boot too. VMM Running
	// alone never sets boot_passed for an otherwise unresponsive guest.
	out.Validation, out.BootPassed, out.AccessPassed = "access_passed", true, true
	if err = c.emit(Event{Stage: "probe", Status: "succeeded", ResourceID: out.EnvironmentID, Message: "temporary guest access probe passed"}); err != nil {
		return out, Failure("E_CANCELLED", "validate", err)
	}
	return out, nil
}

func (c *Client) cleanupValidation(out *ValidationResult, keepFailed bool, runErr error) error {
	if !state.ValidUUID(out.EnvironmentID) {
		return nil
	}
	if keepFailed && runErr != nil {
		out.Retained = true
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	// Compensation must run even if a requested audit write caused cancellation.
	clean, err := c.down(ctx, DownRequest{Environment: out.EnvironmentID}, false)
	out.Cleanup = &clean
	if err != nil {
		out.CleanupIncomplete = true
		failure := Failure("E_CLEANUP_INCOMPLETE", "cleanup", err)
		failure.CleanupIncomplete = true
		return failure
	}
	return nil
}
