package build

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/LeeXN/Flintyard/internal/contract"
	"github.com/LeeXN/Flintyard/internal/firecracker"
	"github.com/LeeXN/Flintyard/internal/image"
	"github.com/LeeXN/Flintyard/internal/state"
	"golang.org/x/sys/unix"
)

type workerConfig struct {
	Protocol      string             `json:"protocol"`
	Operation     string             `json:"operation"`
	Owner         state.Owner        `json:"owner"`
	BuildDir      string             `json:"build_dir"`
	Recipe        Recipe             `json:"recipe"`
	RecipeSHA256  string             `json:"recipe_sha256"`
	Inputs        map[string]Input   `json:"inputs"`
	Network       bool               `json:"network"`
	Worker        string             `json:"worker"`
	StepIndex     int                `json:"step_index"`
	ParentMountNS string             `json:"parent_mount_ns"`
	ParentNetNS   string             `json:"parent_net_ns"`
	KernelTools   *image.KernelBuild `json:"kernel_tools,omitempty"`
}
type workerReport struct {
	OK                 bool   `json:"ok"`
	Stage              string `json:"stage"`
	Message            string `json:"message,omitempty"`
	Errno              uint64 `json:"errno,omitempty"`
	Cancelled          bool   `json:"cancelled,omitempty"`
	Deadline           bool   `json:"deadline,omitempty"`
	KernelIncompatible bool   `json:"kernel_incompatible,omitempty"`
}

func (r workerReport) failure() error {
	if r.Cancelled {
		return fmt.Errorf("%s: %w", r.Message, context.Canceled)
	}
	if r.Deadline {
		return fmt.Errorf("%s: %w", r.Message, context.DeadlineExceeded)
	}
	if r.Errno != 0 {
		return fmt.Errorf("%s: %w", r.Message, syscall.Errno(r.Errno))
	}
	if r.KernelIncompatible {
		return fmt.Errorf("%s: %w", r.Message, image.ErrKernelIncompatible)
	}
	return fmt.Errorf("%s", r.Message)
}

// The creator thread stays pinned until worker exit: Pdeathsig belongs to the
// creating Linux thread, not abstractly to the whole Go process.
func runWorker(ctx context.Context, c workerConfig, events func(Event) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.ParentMountNS, _ = os.Readlink("/proc/self/ns/mnt")
	c.ParentNetNS, _ = os.Readlink("/proc/self/ns/net")
	configPath := filepath.Join(c.BuildDir, "worker-config-"+c.Operation+fmt.Sprint(c.StepIndex)+".json")
	if err := state.Write(configPath, c); err != nil {
		return err
	}
	f, err := os.Open(configPath)
	if err != nil {
		return err
	}
	defer f.Close()
	log, err := os.OpenFile(filepath.Join(c.BuildDir, "build.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	defer log.Close()
	cmd := exec.Command(c.Worker, "__build-worker")
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C"}
	cmd.Stdin = nil
	cmd.Stdout, cmd.Stderr = log, log
	cmd.ExtraFiles = []*os.File{f}
	flags := uintptr(unix.CLONE_NEWNS | unix.CLONE_NEWPID | unix.CLONE_NEWUTS | unix.CLONE_NEWIPC)
	if !c.Network {
		flags |= unix.CLONE_NEWNET
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Cloneflags: flags, Pdeathsig: syscall.SIGKILL}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err = cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	identity, err := firecracker.Observe(cmd.Process.Pid)
	if err != nil {
		// A finite step can exit and be reaped before proc observation. That is
		// normal completion, not a missing-identity excuse to signal a bare PID.
		select {
		case e := <-done:
			return workerExitError(c, e)
		case <-ctx.Done():
			return errors.Join(err, ctx.Err())
		}
	}
	if c.Operation == "build" {
		if err = state.Write(filepath.Join(c.BuildDir, "worker-identity.json"), identity); err != nil {
			cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, stopErr := firecracker.Terminate(cleanup, firecracker.TerminateRequest{Identity: identity, Grace: time.Second})
			<-done
			return errors.Join(err, stopErr)
		}
	}
	select {
	case err = <-done:
		return workerExitError(c, err)
	case <-ctx.Done():
		cleanup, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		stopErr := terminateWorker(cleanup, identity)
		var waitErr error
		select {
		case waitErr = <-done:
		case <-cleanup.Done():
			waitErr = fmt.Errorf("worker cleanup incomplete: %w", cleanup.Err())
		}
		return errors.Join(ctx.Err(), stopErr, waitErr)
	}
}
func workerExitError(c workerConfig, err error) error {
	if err != nil && c.Operation == "build" {
		var report workerReport
		if e := state.Read(filepath.Join(c.BuildDir, "worker-result.json"), &report); e == nil && !report.OK {
			return report.failure()
		}
	}
	return err
}
func terminateWorker(ctx context.Context, initial firecracker.Identity) error {
	for {
		current, err := firecracker.Observe(initial.PID)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		// Step workers intentionally exec their configured guest program. Boot,
		// starttime, cgroup and netns still bind this to the originally spawned
		// process; the freshly observed executable is revalidated before signal.
		if current.BootID != initial.BootID || current.StartTime != initial.StartTime || current.Cgroup != initial.Cgroup || current.NetNS != initial.NetNS {
			return firecracker.ErrProcessIdentity
		}
		_, err = firecracker.Terminate(ctx, firecracker.TerminateRequest{Identity: current, Grace: 5 * time.Second})
		if !errors.Is(err, firecracker.ErrProcessIdentity) {
			return err
		}
		select {
		case <-ctx.Done():
			return errors.Join(err, ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func readWorkerConfig() (workerConfig, error) {
	var c workerConfig
	f := os.NewFile(3, "worker-config")
	if f == nil {
		return c, fmt.Errorf("worker config fd unavailable")
	}
	defer f.Close()
	var data json.RawMessage
	if err := contract.Read(f, &data); err != nil {
		return c, err
	}
	if err := contract.Decode(data, &c); err != nil {
		return c, err
	}
	if c.Protocol != Protocol || c.Operation != "build" && c.Operation != "step" || !filepath.IsAbs(c.BuildDir) || filepath.Base(c.BuildDir) != c.Owner.EnvironmentID || c.Owner.NodeID != "" {
		return c, fmt.Errorf("invalid internal build worker protocol/scope")
	}
	if err := c.Owner.Validate(); err != nil {
		return c, err
	}
	if err := state.VerifyOwner(c.BuildDir, c.Owner); err != nil {
		return c, err
	}
	if os.Geteuid() != 0 || os.Getpid() != 1 {
		return c, fmt.Errorf("worker must be PID 1 in its private PID namespace")
	}
	mnt, _ := os.Readlink("/proc/self/ns/mnt")
	net, _ := os.Readlink("/proc/self/ns/net")
	if mnt == c.ParentMountNS || !c.Network && net == c.ParentNetNS {
		return c, fmt.Errorf("worker namespace isolation not established")
	}
	return c, nil
}
