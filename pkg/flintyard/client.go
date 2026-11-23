// Package flintyard exposes the local core independently of CLI presentation.
package flintyard

import (
	"context"
	"errors"
	"fmt"
	"github.com/LeeXN/Flintyard/internal/allocation"
	"github.com/LeeXN/Flintyard/internal/contract"
	"github.com/LeeXN/Flintyard/internal/firecracker"
	"github.com/LeeXN/Flintyard/internal/host"
	"github.com/LeeXN/Flintyard/internal/image"
	"github.com/LeeXN/Flintyard/internal/network"
	"github.com/LeeXN/Flintyard/internal/state"
	"github.com/LeeXN/Flintyard/internal/version"
	"golang.org/x/sys/unix"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const APIVersion = "flintyard/v1alpha1"
const Version = version.Version

type Error struct {
	Code              string `json:"code"`
	Message           string `json:"message"`
	Stage             string `json:"stage"`
	Node              string `json:"node,omitempty"`
	Retryable         bool   `json:"retryable"`
	Details           any    `json:"details"`
	Cause             error  `json:"-"`
	CleanupIncomplete bool   `json:"-"`
}

func (e *Error) Error() string { return e.Message }
func (e *Error) Unwrap() error { return e.Cause }
func Failure(code, stage string, e error) *Error {
	return &Error{Code: code, Stage: stage, Message: e.Error(), Cause: e, Details: map[string]any{}}
}

func stateRootFailure(err error) *Error {
	if errors.Is(err, context.Canceled) {
		return Failure("E_CANCELLED", "preflight", err)
	}
	return Failure("E_STATE_INVALID", "preflight", err)
}

func ExitCode(e error) int {
	var f *Error
	if errors.As(e, &f) && (f.CleanupIncomplete || f.Code == "E_CLEANUP_INCOMPLETE") {
		return 7
	}
	if errors.Is(e, context.Canceled) {
		return 130
	}
	if !errors.As(e, &f) {
		return 4
	}
	switch f.Code {
	case "E_CONFIG_INVALID":
		return 2
	case "E_PERMISSION", "E_DEPENDENCY", "E_KVM_UNAVAILABLE", "E_ARCH_MISMATCH", "E_VERSION_UNSUPPORTED", "E_FORWARDING_DISABLED":
		return 3
	case "E_LAUNCH_FAILED", "E_ACCESS_TIMEOUT", "E_API_TIMEOUT":
		return 5
	case "E_STATE_INVALID", "E_SPEC_CONFLICT", "E_LOCKED", "E_PROCESS_IDENTITY", "E_NETWORK_CONFLICT", "E_FIREWALL_CONFLICT", "E_PORT_IN_USE":
		return 6
	case "E_CLEANUP_INCOMPLETE":
		return 7
	case "E_CANCELLED":
		return 130
	}
	return 4
}

type Event struct {
	APIVersion  string `json:"api_version"`
	OperationID string `json:"operation_id"`
	Sequence    uint64 `json:"sequence"`
	Time        string `json:"time"`
	Stage       string `json:"stage"`
	ResourceID  string `json:"resource_id"`
	Node        string `json:"node"`
	Status      string `json:"status"`
	Message     string `json:"message"`
}
type EventSink func(Event) error
type Client struct {
	StateDir    string
	OperationID string
	Events      EventSink
	sequence    atomic.Uint64
	eventMu     sync.Mutex
}
type ImageResult = image.Result
type ImportRequest struct {
	Package, Spec, Output, SHA256 string
	MaxBytes                      int64
	MaxFiles                      int
}
type InspectRequest struct {
	Image    string
	MaxBytes int64
	MaxFiles int
}
type DoctorReport = host.Report
type DoctorRequest struct {
	Mode, Firecracker, Jailer, UIDRange string
}

func New(root string) *Client {
	if absolute, e := filepath.Abs(root); e == nil {
		root = absolute
	}
	return &Client{StateDir: root, OperationID: state.UUID()}
}
func (c *Client) emit(event Event) error {
	c.eventMu.Lock()
	defer c.eventMu.Unlock()
	if c.Events == nil {
		return nil
	}
	event.APIVersion = APIVersion
	event.OperationID = c.OperationID
	event.Sequence = c.sequence.Add(1)
	event.Time = time.Now().UTC().Format(time.RFC3339Nano)
	return c.Events(event)
}
func (c *Client) ImportImage(ctx context.Context, r ImportRequest) (ImageResult, error) {
	if e := c.emit(Event{Stage: "import", Status: "started", Message: "checking image inputs"}); e != nil {
		return ImageResult{}, Failure("E_CANCELLED", "import", e)
	}
	p := r.Package
	if r.Spec != "" {
		if r.Output == "" || p != "" {
			return ImageResult{}, Failure("E_CONFIG_INVALID", "parse", fmt.Errorf("spec requires output and excludes package"))
		}
		if _, e := image.PackSpec(ctx, r.Spec, r.Output); e != nil {
			return ImageResult{}, imageError(e)
		}
		p = r.Output
	}
	if p == "" {
		return ImageResult{}, Failure("E_CONFIG_INVALID", "parse", fmt.Errorf("package or spec is required"))
	}
	out, e := image.Import(ctx, c.StateDir, p, r.SHA256, image.Limits{MaxBytes: r.MaxBytes, MaxFiles: r.MaxFiles})
	if e != nil {
		return out, imageError(e)
	}
	if e = c.emit(Event{Stage: "commit", Status: "succeeded", ResourceID: out.ID, Message: "image imported"}); e != nil {
		return out, Failure("E_CANCELLED", "commit", fmt.Errorf("image committed but audit write failed: %w", e))
	}
	return out, nil
}
func (c *Client) InspectImage(ctx context.Context, r InspectRequest) (ImageResult, error) {
	var out ImageResult
	var e error
	if strings.HasPrefix(r.Image, "sha256:") {
		out, e = image.Cached(ctx, c.StateDir, r.Image)
	} else {
		out, e = image.Inspect(ctx, r.Image, image.Limits{MaxBytes: r.MaxBytes, MaxFiles: r.MaxFiles})
	}
	if e != nil {
		return out, imageError(e)
	}
	return out, nil
}
func imageError(e error) *Error {
	code := "E_IMAGE_INVALID"
	s := e.Error()
	if strings.Contains(s, "digest") {
		code = "E_DIGEST_MISMATCH"
	}
	if strings.Contains(s, "unsafe") || strings.Contains(s, "forbidden") {
		code = "E_IMAGE_UNTRUSTED_INPUT"
	}
	if strings.Contains(s, "lock held") {
		code = "E_LOCKED"
	}
	if errors.Is(e, context.Canceled) {
		code = "E_CANCELLED"
	}
	if errors.Is(e, unix.ENOSPC) {
		code = "E_DISK_FULL"
	}
	return Failure(code, "import", e)
}
func (c *Client) Doctor(ctx context.Context, mode, fc, jailer string) (DoctorReport, error) {
	return c.DoctorWithOptions(ctx, DoctorRequest{Mode: mode, Firecracker: fc, Jailer: jailer})
}

func (c *Client) DoctorWithOptions(ctx context.Context, request DoctorRequest) (DoctorReport, error) {
	if request.Mode != "run" && request.Mode != "build" && request.Mode != "validate" {
		return DoctorReport{}, Failure("E_CONFIG_INVALID", "parse", fmt.Errorf("mode must be run, build or validate"))
	}
	if request.UIDRange == "" {
		request.UIDRange = "60000:64999"
	}
	ids, err := allocation.ParseRange(request.UIDRange)
	if err != nil {
		return DoctorReport{}, Failure("E_CONFIG_INVALID", "parse", err)
	}
	r := host.Doctor(ctx, request.Mode, request.Firecracker, request.Jailer)
	if request.Mode != "build" {
		addCheck := func(name, detail string, err error) {
			if err != nil {
				detail = err.Error()
				r.Ready = false
			}
			r.Checks = append(r.Checks, host.Check{Name: name, OK: err == nil, Detail: detail})
		}
		layout, err := firecracker.DiscoverCgroup()
		addCheck("cgroup", fmt.Sprintf("v%d memory controller at %s", layout.Version, layout.Mount), err)
		if os.Geteuid() == 0 {
			err = (allocation.Coordinator{}).CheckIDRange(ctx, ids)
		} else {
			err = fmt.Errorf("checking all reserved UID/GID identities requires root")
		}
		addCheck("uid_range", request.UIDRange, err)
	}
	if request.Mode != "build" && os.Geteuid() == 0 {
		warnings, err := network.PolicyDiagnostics(ctx, network.Plan{})
		if err != nil {
			r.Warnings = append(r.Warnings, "cannot inspect host nft policy: "+err.Error())
		} else {
			r.Warnings = append(r.Warnings, warnings...)
		}
	}
	if !r.Ready {
		return r, Failure("E_DEPENDENCY", "preflight", fmt.Errorf("host prerequisites not satisfied; see checks"))
	}
	return r, nil
}
func (c *Client) CheckEnvironment(file string) (map[string]any, error) {
	absolute, e := filepath.Abs(file)
	if e != nil {
		return nil, Failure("E_CONFIG_INVALID", "parse", e)
	}
	file = absolute
	f, e := os.Open(file)
	if e != nil {
		return nil, Failure("E_CONFIG_INVALID", "parse", e)
	}
	defer f.Close()
	b, e := io.ReadAll(io.LimitReader(f, contract.MaxConfig+1))
	if e != nil {
		return nil, Failure("E_CONFIG_INVALID", "parse", e)
	}
	m, e := contract.Environment(b, filepath.Dir(file))
	if e != nil {
		return nil, Failure("E_CONFIG_INVALID", "parse", e)
	}
	return m, nil
}
