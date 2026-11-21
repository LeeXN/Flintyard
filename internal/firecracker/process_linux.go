package firecracker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

var ErrProcessIdentity = errors.New("process identity cannot be established")

type TerminateRequest struct {
	Identity Identity
	Grace    time.Duration
}

type TerminateResult struct {
	Exited    bool
	Escalated bool
}

// Terminate pins a pidfd before inspecting proc and never signals a numeric PID.
// A caller must hold the environment lock so that no other operation can move a
// managed process between namespaces or cgroups during verification.
func Terminate(ctx context.Context, req TerminateRequest) (TerminateResult, error) {
	var result TerminateResult
	if err := ctx.Err(); err != nil {
		return result, err
	}
	want := req.Identity
	if want.PID <= 0 || want.BootID == "" || want.StartTime == 0 || want.Executable == "" || want.ExecutableInode == 0 || want.Cgroup == "" || want.NetNS == "" || req.Grace < 0 {
		return result, fmt.Errorf("%w: incomplete identity or invalid grace period", ErrProcessIdentity)
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return result, fmt.Errorf("%w: %w", ErrProcessIdentity, err)
	}
	if strings.TrimSpace(string(boot)) != want.BootID {
		return result, fmt.Errorf("%w: host boot changed", ErrProcessIdentity)
	}
	fd, err := unix.PidfdOpen(want.PID, 0)
	if err != nil {
		return result, fmt.Errorf("%w: pidfd_open: %w", ErrProcessIdentity, err)
	}
	defer unix.Close(fd)
	if exited, e := pidfdExited(fd); e != nil {
		return result, e
	} else if exited {
		result.Exited = true
		return result, nil
	}
	signal := func(sig unix.Signal) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := Verify(want); err != nil {
			if exited, e := pidfdExited(fd); e == nil && exited {
				return nil
			}
			return fmt.Errorf("%w: %w", ErrProcessIdentity, err)
		}
		if err := unix.PidfdSendSignal(fd, sig, nil, 0); err != nil {
			return fmt.Errorf("pidfd_send_signal: %w", err)
		}
		return nil
	}
	if err = signal(unix.SIGTERM); err != nil {
		return result, err
	}
	graceCtx, cancel := context.WithTimeout(ctx, req.Grace)
	err = waitPIDFD(graceCtx, fd)
	cancel()
	if err == nil {
		result.Exited = true
		return result, nil
	}
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		return result, err
	}
	// Revalidate before escalation too. The same pidfd remains pinned throughout.
	if err = signal(unix.SIGKILL); err != nil {
		return result, err
	}
	result.Escalated = true
	if err = waitPIDFD(ctx, fd); err != nil {
		return result, err
	}
	result.Exited = true
	return result, nil
}

func pidfdExited(fd int) (bool, error) {
	fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	for {
		_, err := unix.Poll(fds, 0)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return false, err
		}
		break
	}
	if fds[0].Revents&(unix.POLLERR|unix.POLLNVAL) != 0 {
		return false, fmt.Errorf("invalid pidfd")
	}
	return fds[0].Revents&(unix.POLLIN|unix.POLLHUP) != 0, nil
}

// ProcessState observes the kernel rather than trusting a persisted running
// flag. Old-boot and exited/zombie processes are never signaled.
func ProcessState(ctx context.Context, want Identity) (string, error) {
	if want.PID <= 0 || want.BootID == "" || want.StartTime == 0 || want.Executable == "" || want.ExecutableInode == 0 || want.Cgroup == "" || want.NetNS == "" {
		return "unknown", ErrProcessIdentity
	}
	if err := ctx.Err(); err != nil {
		return "unknown", err
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "unknown", err
	}
	if strings.TrimSpace(string(boot)) != want.BootID {
		return "exited", nil
	}
	if want.PID <= 0 {
		return "unknown", ErrProcessIdentity
	}
	fd, err := unix.PidfdOpen(want.PID, 0)
	if errors.Is(err, unix.ESRCH) {
		return "exited", nil
	}
	if err != nil {
		return "unknown", err
	}
	defer unix.Close(fd)
	if exited, err := pidfdExited(fd); err != nil {
		return "unknown", err
	} else if exited {
		return "exited", nil
	}
	if err := Verify(want); err != nil {
		return "identity_mismatch", fmt.Errorf("%w: %w", ErrProcessIdentity, err)
	}
	return "running", nil
}

// pidfds become readable when a process exits, even if it is a non-child or a
// zombie not yet reaped by its parent. Signal(0) and proc existence cannot prove
// that a VMM is still executing.
func waitPIDFD(ctx context.Context, fd int) error {
	for {
		fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		_, err := unix.Poll(fds, 0)
		if err != nil && !errors.Is(err, unix.EINTR) {
			return err
		}
		if fds[0].Revents&(unix.POLLIN|unix.POLLHUP) != 0 {
			return nil
		}
		if fds[0].Revents&(unix.POLLERR|unix.POLLNVAL) != 0 {
			return fmt.Errorf("invalid pidfd poll state: %d", fds[0].Revents)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}
