package network

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/LeeXN/Flintyard/internal/host"
	"github.com/LeeXN/Flintyard/internal/state"
	"golang.org/x/sys/unix"
)

func keepAt(t *testing.T, p Plan, kind, status string) *state.Journal {
	t.Helper()
	j := testJournal(t, p)
	sentinel := errors.New("retain a precise resource boundary")
	_, err := (Manager{}).Create(context.Background(), CreateRequest{Plan: p, Journal: j, KeepFailed: true, Events: func(e ResourceEvent) error {
		if e.Kind == kind && e.Status == status {
			return sentinel
		}
		return nil
	}})
	if !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
	return j
}

func TestNativeNamespacePlaceholderOwnership(t *testing.T) {
	nativeOnly(t)
	if err := safeNamespaceDir(); err != nil {
		t.Fatal(err)
	}
	p := testPlan(t, 97)
	path := nsPath(p)
	if err := publishNamespacePlaceholder(path, p.Owner); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(path) })
	info, err := inspectNamespace(context.Background(), path, p.Namespace, p.Owner)
	if err != nil || !info.Exists || info.Mounted {
		t.Fatal("owned placeholder was not recognized", info, err)
	}
	if _, err = inspectNamespace(context.Background(), path, p.Namespace, p.Owner+"-different"); !errors.Is(err, ErrOwnership) {
		t.Fatal("wrong owner was accepted", err)
	}
	if err = unix.Setxattr(path, previousNamespaceOwnerXattr, []byte("foreign"), 0); err == nil {
		if _, err = inspectNamespace(context.Background(), path, p.Namespace, p.Owner); !errors.Is(err, ErrOwnership) {
			t.Fatal("conflicting markers were accepted", err)
		}
		if err = unix.Setxattr(path, previousNamespaceOwnerXattr, []byte(p.Owner), 0); err != nil {
			t.Fatal(err)
		}
		if err = unix.Removexattr(path, namespaceOwnerXattr); err != nil {
			t.Fatal(err)
		}
		if _, err = inspectNamespace(context.Background(), path, p.Namespace, p.Owner); err != nil {
			t.Fatal("previously published marker could not be recovered", err)
		}
	} else if !errors.Is(err, unix.EOPNOTSUPP) {
		t.Fatal(err)
	} else {
		t.Log("tmpfs does not support the previous user xattr; trusted marker verified")
	}
}

func TestNativeStoppedPartialNetworkIsReclaimedBeforeRebuild(t *testing.T) {
	nativeOnly(t)
	ctx := context.Background()
	p := testPlan(t, 91)
	j := keepAt(t, p, "veth", "created")
	present, err := (Manager{}).ReconcileStopped(ctx, p, []*state.Journal{j})
	if err != nil || present {
		t.Fatal("accepted an incomplete retained network", present, err)
	}
	allReleased(t, j)
	if info, err := inspectNamespace(ctx, nsPath(p), p.Namespace, p.Owner); err != nil || info.Exists {
		t.Fatal(info, err)
	}
}

func TestNativeNamespaceThreadStillInUseRejected(t *testing.T) {
	nativeOnly(t)
	ctx := context.Background()
	p := testPlan(t, 83)
	j := keepAt(t, p, "bridge", "intent")
	target, err := os.Open(nsPath(p))
	if err != nil {
		t.Fatal(err)
	}
	ready, done := make(chan error, 1), make(chan error, 1)
	release := make(chan struct{})
	go func() {
		runtime.LockOSThread()
		old, err := os.Open("/proc/thread-self/ns/net")
		if err != nil {
			runtime.UnlockOSThread()
			ready <- err
			done <- err
			return
		}
		defer old.Close()
		err = unix.Setns(int(target.Fd()), unix.CLONE_NEWNET)
		ready <- err
		if err == nil {
			<-release
		}
		err = errors.Join(err, unix.Setns(int(old.Fd()), unix.CLONE_NEWNET))
		if err == nil {
			runtime.UnlockOSThread()
		}
		done <- err
	}()
	if err = <-ready; err != nil {
		close(release)
		<-done
		target.Close()
		t.Fatal(err)
	}
	_, removeErr := (Manager{}).Remove(ctx, p, j)
	close(release)
	restoreErr := <-done
	target.Close()
	var cleanupErr *CleanupError
	if !errors.As(removeErr, &cleanupErr) || len(cleanupErr.Errors) != 1 || !strings.Contains(cleanupErr.Errors[0].Message, "thread") {
		t.Fatal("ignored thread-local namespace membership", removeErr)
	}
	if restoreErr != nil {
		t.Fatal(restoreErr)
	}
	if _, err = (Manager{}).Remove(ctx, p, j); err != nil {
		t.Fatal(err)
	}
	allReleased(t, j)
}

func TestNativeLinkCreationBeforeAliasRecovery(t *testing.T) {
	nativeOnly(t)
	for _, kind := range []string{"bridge", "veth"} {
		t.Run(kind, func(t *testing.T) {
			p := testPlan(t, 75)
			j := keepAt(t, p, kind, "intent")
			if kind == "bridge" {
				runNative(t, "ip", "-n", p.Namespace, "link", "add", "name", p.Bridge, "type", "bridge")
			} else {
				runNative(t, "ip", "-n", p.Namespace, "link", "add", "name", p.HostLink, "type", "veth", "peer", "name", p.PeerLink)
			}
			// Reconstruct exclusively from persisted intent + kernel namespace
			// ownership; neither bridge nor veth has an alias or a done identity.
			path := filepath.Join(t.TempDir(), "journal.json")
			if err := state.Write(path, j); err != nil {
				t.Fatal(err)
			}
			reloaded, err := state.ReadJournal(path, p.EnvironmentID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = (Manager{}).Remove(context.Background(), p, reloaded); err != nil {
				t.Fatal(err)
			}
			allReleased(t, reloaded)
		})
	}
}

func TestNativeNamespaceIdentityReplacementRejected(t *testing.T) {
	nativeOnly(t)
	ctx := context.Background()
	p := testPlan(t, 76)
	j := keepAt(t, p, "bridge", "intent")
	original, err := os.Open(nsPath(p))
	if err != nil {
		t.Fatal(err)
	}
	defer original.Close()
	replacement, err := newNamespace(ctx, p.Owner)
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()
	// The saved fd intentionally keeps the old bind mount busy. Detach only
	// this test-owned mount so the old namespace can be restored afterwards.
	if err = unix.Unmount(nsPath(p), unix.MNT_DETACH); err != nil {
		t.Fatal(err)
	}
	if err = unix.Mount("/proc/self/fd/"+strconv.Itoa(int(replacement.Fd())), nsPath(p), "", unix.MS_BIND, ""); err != nil {
		t.Fatal(err)
	}
	info, err := inspectNamespace(ctx, nsPath(p), p.Namespace, p.Owner)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = (Manager{}).Remove(ctx, p, j); err == nil {
		t.Fatal("accepted substituted namespace inode")
	}
	after, err := inspectNamespace(ctx, nsPath(p), p.Namespace, p.Owner)
	if err != nil || info.Inode != after.Inode {
		t.Fatal("substituted namespace was deleted", after, err)
	}
	if err = unix.Unmount(nsPath(p), 0); err != nil {
		t.Fatal(err)
	}
	if err = unix.Mount("/proc/self/fd/"+strconv.Itoa(int(original.Fd())), nsPath(p), "", unix.MS_BIND, ""); err != nil {
		t.Fatal(err)
	}
	if _, err = (Manager{}).Remove(ctx, p, j); err != nil {
		t.Fatal(err)
	}
	allReleased(t, j)
}

func TestNativeNamespaceStillInUseRejected(t *testing.T) {
	nativeOnly(t)
	ctx := context.Background()
	p := testPlan(t, 77)
	j := keepAt(t, p, "bridge", "intent")
	info, err := inspectNamespace(ctx, nsPath(p), p.Namespace, p.Owner)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("ip", "netns", "exec", p.Namespace, "sleep", "60")
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	want := "net:[" + strconv.FormatUint(info.Inode, 10) + "]"
	deadline := time.Now().Add(3 * time.Second)
	for {
		got, _ := os.Readlink("/proc/" + strconv.Itoa(cmd.Process.Pid) + "/ns/net")
		if got == want {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("test process did not join target namespace")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err = (Manager{}).Remove(ctx, p, j); err == nil {
		t.Fatal("removed namespace of a live process")
	}
	if got, err := inspectNamespace(ctx, nsPath(p), p.Namespace, p.Owner); err != nil || !got.Mounted {
		t.Fatal("busy namespace changed", got, err)
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	if _, err = (Manager{}).Remove(ctx, p, j); err != nil {
		t.Fatal(err)
	}
	allReleased(t, j)
}

func TestNativeNamespaceUnmountCommitWindow(t *testing.T) {
	nativeOnly(t)
	ctx := context.Background()
	p := testPlan(t, 78)
	j := keepAt(t, p, "bridge", "intent")
	if err := j.Absent(j.Resources[1].ID); err != nil {
		t.Fatal(err)
	}
	if err := unix.Unmount(nsPath(p), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := (Manager{}).Remove(ctx, p, j); err != nil {
		t.Fatal("failed to recover unmount-before-state window", err)
	}
	allReleased(t, j)
}

func TestNativeHostFirewallCreatedBeforeDoneRecovery(t *testing.T) {
	nativeOnly(t)
	ctx := context.Background()
	p := testPlan(t, 79)
	j := testJournal(t, p)
	_, err := j.IntentIdentity("nft-host", map[string]string{"owner": p.Owner, "name": p.Table, "namespace": p.Namespace})
	if err != nil {
		t.Fatal(err)
	}
	data, err := PrivateFirewall(p, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = host.Run(ctx, nftRequest("", []string{"-j", "-f", "-"}, data)); err != nil {
		t.Fatal(err)
	}
	if handle, err := firewallHandle(ctx, p, ""); err != nil || handle == 0 {
		t.Fatal(handle, err)
	}
	if _, err = (Manager{}).Remove(ctx, p, j); err != nil {
		t.Fatal(err)
	}
	allReleased(t, j)
	if handle, err := firewallHandle(ctx, p, ""); err != nil || handle != 0 {
		t.Fatal("table not removed", handle, err)
	}
}

func TestNativeFirewallForeignTableRejected(t *testing.T) {
	nativeOnly(t)
	ctx := context.Background()
	p := testPlan(t, 82)
	j := testJournal(t, p)
	_, err := j.IntentIdentity("nft-host", map[string]string{"owner": p.Owner, "name": p.Table, "namespace": p.Namespace})
	if err != nil {
		t.Fatal(err)
	}
	runNative(t, "nft", "add", "table", "inet", p.Table)
	t.Cleanup(func() {
		_, _ = host.Run(ctx, host.CommandRequest{Name: "nft", Args: []string{"delete", "table", "inet", p.Table}})
	})
	if _, err = (Manager{}).Remove(ctx, p, j); !errors.Is(err, ErrOwnership) {
		t.Fatal("removed a foreign table")
	}
	var listing nftListing
	if err = host.RunJSON(ctx, nftRequest("", []string{"-j", "list", "table", "inet", p.Table}, nil), &listing); err != nil {
		t.Fatal("foreign table disappeared", err)
	}
}

func TestNativeCleanupPreservesCommandErrorAndCanRetry(t *testing.T) {
	nativeOnly(t)
	ctx := context.Background()
	p := testPlan(t, 84)
	j := testJournal(t, p)
	_, err := j.IntentIdentity("nft-host", map[string]string{"owner": p.Owner, "name": p.Table, "namespace": p.Namespace})
	if err != nil {
		t.Fatal(err)
	}
	data, err := PrivateFirewall(p, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = host.Run(ctx, nftRequest("", []string{"-j", "-f", "-"}, data)); err != nil {
		t.Fatal(err)
	}
	originalPATH := os.Getenv("PATH")
	t.Setenv("PATH", t.TempDir())
	result, err := (Manager{}).Remove(ctx, p, j)
	var commandErr *host.CommandError
	if !errors.As(err, &commandErr) || !errors.Is(err, exec.ErrNotFound) || len(result.CleanupErrors) != 1 {
		t.Fatal("lost cleanup failure type/cause", err)
	}
	t.Setenv("PATH", originalPATH)
	if _, err = (Manager{}).Remove(ctx, p, j); err != nil {
		t.Fatal("cleanup could not be retried", err)
	}
	allReleased(t, j)
}
