package firecracker

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// Only the test binary implements this helper. Production has no environment
// controlled execution or fault injection hook.
func TestProcessHelper(t *testing.T) {
	mode := os.Getenv("FY_PROCESS_TEST")
	if mode != "ignore-term" && mode != "term" {
		return
	}
	ready := os.NewFile(3, "ready")
	if mode == "ignore-term" {
		signal.Ignore(syscall.SIGTERM)
	}
	_, _ = ready.Write([]byte{1})
	_ = ready.Close()
	for {
		time.Sleep(time.Hour)
	}
}

func childIdentity(t *testing.T, ignoreTerm bool) Identity {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	cmd := exec.Command(os.Args[0], "-test.run=^TestProcessHelper$")
	mode := "term"
	if ignoreTerm {
		mode = "ignore-term"
	}
	cmd.Env = append(os.Environ(), "FY_PROCESS_TEST="+mode)
	cmd.ExtraFiles = []*os.File{w}
	if err = cmd.Start(); err != nil {
		w.Close()
		t.Fatal(err)
	}
	w.Close()
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	if err = r.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err = r.Read(make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	id, err := Observe(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestTerminateRejectsIdentityWithoutSignaling(t *testing.T) {
	id := childIdentity(t, true)
	for _, field := range []string{"pid", "boot", "starttime", "exe", "device", "inode", "cgroup", "netns"} {
		t.Run(field, func(t *testing.T) {
			bad := id
			switch field {
			case "pid":
				bad.PID = 0
			case "boot":
				bad.BootID = "another-boot"
			case "starttime":
				bad.StartTime++
			case "exe":
				bad.Executable += "-replacement"
			case "device":
				bad.ExecutableDevice++
			case "inode":
				bad.ExecutableInode++
			case "cgroup":
				bad.Cgroup += "another-group"
			case "netns":
				bad.NetNS = "net:[0]"
			}
			_, err := Terminate(context.Background(), TerminateRequest{Identity: bad})
			if !errors.Is(err, ErrProcessIdentity) {
				t.Fatal(err)
			}
			if err = Verify(id); err != nil {
				t.Fatalf("foreign process affected: %v", err)
			}
		})
	}
}

func TestTerminateEscalatesAndObservesUnreapedExit(t *testing.T) {
	id := childIdentity(t, true)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	r, err := Terminate(ctx, TerminateRequest{Identity: id, Grace: 30 * time.Millisecond})
	if err != nil || !r.Exited || !r.Escalated {
		t.Fatal(r, err)
	}
	// The child is deliberately not reaped until Cleanup: waiting must use the
	// pidfd, not /proc/PID existence or a parent-only wait syscall.
	if _, err = os.Stat("/proc/" + strconv.Itoa(id.PID)); err != nil {
		t.Fatal(err)
	}
}

func TestTerminateCancelledDoesNotSignal(t *testing.T) {
	id := childIdentity(t, true)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Terminate(ctx, TerminateRequest{Identity: id}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := Verify(id); err != nil {
		t.Fatal(err)
	}
}

func TestTerminateWithoutEscalation(t *testing.T) {
	id := childIdentity(t, false)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	r, err := Terminate(ctx, TerminateRequest{Identity: id, Grace: 2 * time.Second})
	if err != nil || !r.Exited || r.Escalated {
		t.Fatal(r, err)
	}
}

func TestTerminateDeadlineDuringGraceDoesNotEscalate(t *testing.T) {
	id := childIdentity(t, true)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	r, err := Terminate(ctx, TerminateRequest{Identity: id, Grace: time.Second})
	if !errors.Is(err, context.DeadlineExceeded) || r.Escalated || r.Exited {
		t.Fatal(r, err)
	}
	if err = Verify(id); err != nil {
		t.Fatal("cancelled stop killed process", err)
	}
}
