package firecracker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/LeeXN/Flintyard/internal/host"
	"github.com/LeeXN/Flintyard/internal/state"
	"golang.org/x/sys/unix"
)

func nativeJailOnly(t *testing.T) {
	t.Helper()
	if os.Getenv("FY_JAIL_INTEGRATION") != "1" {
		t.Skip("requires tests/native-jail.sh; no privileged jail checks performed")
	}
	self, _ := os.Readlink("/proc/self/ns/mnt")
	init, _ := os.Readlink("/proc/1/ns/mnt")
	var fs unix.Statfs_t
	if os.Geteuid() != 0 || self == init || unix.Statfs("/run", &fs) != nil || fs.Type != unix.TMPFS_MAGIC {
		t.Fatal("refusing jail tests outside isolated mount namespace/tmpfs /run")
	}
}

func TestNativeJailLauncherIntentBeforePIDRecord(t *testing.T) {
	nativeJailOnly(t)
	ctx := context.Background()
	r, _ := jailFixture(t)
	base := "/run/flintyard"
	if err := state.PrivateDir(base); err != nil {
		t.Fatal(err)
	}
	run := filepath.Join(base, runtimeName(r.Owner, r.Journal.OperationID))
	if err := state.OwnedDir(run, r.Owner); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(r.NodeDir, "jails"), 0700); err != nil {
		t.Fatal(err)
	}
	scope := filepath.Join(r.NodeDir, "jails", r.Journal.OperationID)
	if err := state.OwnedDir(scope, r.Owner); err != nil {
		t.Fatal(err)
	}
	jailBase := filepath.Join(scope, "j")
	jailRoot := filepath.Join(jailBase, "firecracker", r.Owner.NodeID, "root")
	// A real, independently-sessioned Go test helper exercises only the owned
	// argv recovery contract. It is not an official jailer/VMM boot test.
	args := []string{"-test.run=^TestDetachedHelper$", "--", "--id", r.Owner.NodeID, "--chroot-base-dir", jailBase, "--exec-file", "/official/firecracker", "flintyard-detached-worker"}
	encoded, _ := json.Marshal(append([]string{os.Args[0]}, args...))
	parent, err := Observe(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	meta := map[string]string{"node_id": r.Owner.NodeID, "owner": r.Owner.Tag(), "boot_id": parent.BootID, "run_dir": run, "jail_root": jailRoot, "fc_name": "firecracker", "fc_path": "/official/firecracker", "jailer_device": strconv.FormatUint(parent.ExecutableDevice, 10), "jailer_inode": strconv.FormatUint(parent.ExecutableInode, 10), "argv": string(encoded), "min_starttime": strconv.FormatUint(parent.StartTime, 10)}
	id, err := r.Journal.IntentIdentity("vmm-process", meta)
	if err != nil {
		t.Fatal(err)
	}
	log, err := os.OpenFile(filepath.Join(r.NodeDir, "serial.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	d, err := startDetached(os.Args[0], args, log, nil)
	log.Close()
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(d.fd)
	t.Cleanup(func() {
		identity, e := Observe(d.pid)
		if e == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_, _ = Terminate(ctx, TerminateRequest{Identity: identity, Grace: 0})
		}
	})
	observed, phase, err := RecoverProcess(ctx, r.Owner.RootID, r.Journal, r.Journal.Resources[len(r.Journal.Resources)-1])
	if err != nil || observed == nil || observed.PID != d.pid || phase != "launcher" {
		t.Fatal(observed, phase, err)
	}
	if err = StopJournalProcesses(ctx, r.Owner.RootID, r.Journal); err != nil {
		t.Fatal(err)
	}
	for _, resource := range r.Journal.Resources {
		if resource.ID == id && resource.State != "released" {
			t.Fatal("launcher intent not reconciled", resource)
		}
	}
}
func jailFixture(t *testing.T) (JailRequest, string) {
	t.Helper()
	owner := state.Owner{RootID: state.UUID(), EnvironmentID: state.UUID(), NodeID: state.UUID()}
	base := t.TempDir()
	anchor := filepath.Join(base, "env")
	envOwner := owner
	envOwner.NodeID = ""
	if err := state.OwnedDir(anchor, envOwner); err != nil {
		t.Fatal(err)
	}
	node := filepath.Join(anchor, "node")
	if err := state.OwnedDir(node, owner); err != nil {
		t.Fatal(err)
	}
	j, err := state.NewJournal(filepath.Join(anchor, "operation.json"), owner.EnvironmentID)
	if err != nil {
		t.Fatal(err)
	}
	return JailRequest{Owner: owner, NodeDir: node, UID: 60000, GID: 60000, Journal: j}, anchor
}
func TestNativeJailBindIdentityReadOnlyAndCrashWindow(t *testing.T) {
	nativeJailOnly(t)
	ctx := context.Background()
	r, _ := jailFixture(t)
	run := filepath.Join("/run", "fy-test-"+r.Journal.OperationID)
	if err := state.OwnedDir(run, r.Owner); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(r.NodeDir, "jails"), 0700); err != nil {
		t.Fatal(err)
	}
	scope := filepath.Join(r.NodeDir, "jails", r.Journal.OperationID)
	if err := state.OwnedDir(scope, r.Owner); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(r.NodeDir, "disk.raw")
	target := filepath.Join(scope, "disk.raw")
	if err := os.WriteFile(source, []byte("original independent bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := bindJail(ctx, r, run, source, target, true); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Unmount(target, 0) })
	if err := os.WriteFile(target, []byte("changed"), 0600); !errors.Is(err, unix.EROFS) {
		t.Fatal("read-only mount was writable", err)
	}
	resource := r.Journal.Resources[0]
	// Discard the done observation to reproduce creation before durable done.
	resource.State = "intent"
	delete(resource.Identity, "mount_id")
	r.Journal.Resources[0] = resource
	if err := state.Write(filepath.Join(filepath.Dir(r.NodeDir), "recovery.json"), r.Journal); err != nil {
		t.Fatal(err)
	}
	recovered, err := state.ReadJournal(filepath.Join(filepath.Dir(r.NodeDir), "recovery.json"), r.Owner.EnvironmentID)
	if err != nil {
		t.Fatal(err)
	}
	if err = removeJailMount(recovered.Resources[0], recovered, r.Owner); err != nil {
		t.Fatal(err)
	}
	if recovered.Resources[0].State != "released" {
		t.Fatal("unreleased mount intent")
	}
	b, err := os.ReadFile(source)
	if err != nil || string(b) != "original independent bytes" {
		t.Fatal(string(b), err)
	}
	if mount, err := host.MountAt(target); err != nil || mount != nil {
		t.Fatal(mount, err)
	}
}
func TestNativeJailForeignMountIsRetained(t *testing.T) {
	nativeJailOnly(t)
	ctx := context.Background()
	r, _ := jailFixture(t)
	run := filepath.Join("/run", "fy-test-"+r.Journal.OperationID)
	if err := state.OwnedDir(run, r.Owner); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(r.NodeDir, "jails"), 0700); err != nil {
		t.Fatal(err)
	}
	scope := filepath.Join(r.NodeDir, "jails", r.Journal.OperationID)
	if err := state.OwnedDir(scope, r.Owner); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(r.NodeDir, "disk")
	target := filepath.Join(scope, "disk")
	if err := os.WriteFile(source, []byte("one"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := bindJail(ctx, r, run, source, target, false); err != nil {
		t.Fatal(err)
	}
	if err := unix.Unmount(target, 0); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(r.NodeDir, "other")
	if err := os.WriteFile(other, []byte("foreign"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mount(other, target, "", unix.MS_BIND, ""); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Unmount(target, 0) })
	if err := removeJailMount(r.Journal.Resources[0], r.Journal, r.Owner); !errors.Is(err, ErrProcessIdentity) {
		t.Fatal("accepted foreign replacement", err)
	}
	if b, err := os.ReadFile(target); err != nil || string(b) != "foreign" {
		t.Fatal("modified foreign mount", string(b), err)
	}
}
func TestNativeJailCgroupRoundTrip(t *testing.T) {
	nativeJailOnly(t)
	layout, err := DiscoverCgroup()
	if err != nil {
		t.Skipf("BLOCKED: no usable delegated cgroup: %v", err)
	}
	var fs unix.Statfs_t
	if err = unix.Statfs(layout.Mount, &fs); err != nil {
		t.Fatal(err)
	}
	if fs.Flags&unix.ST_RDONLY != 0 {
		t.Skip("BLOCKED: cgroup hierarchy is read-only")
	}
	r, anchor := jailFixture(t)
	cg, err := PrepareCgroup(context.Background(), CgroupRequest{Layout: layout, Owner: r.Owner, Anchor: anchor, MemoryMiB: 128, Journal: r.Journal})
	t.Cleanup(func() { _ = RemoveCgroups(context.Background(), r.Journal) })
	if err != nil {
		t.Fatal(err)
	}
	if err = cg.Verify(); err != nil {
		t.Fatal(err)
	}
	file := "memory.limit_in_bytes"
	if layout.Version == 2 {
		file = "memory.max"
	}
	b, err := os.ReadFile(filepath.Join(cg.Path, file))
	if err != nil {
		t.Fatal(err)
	}
	n, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
	if err != nil || n != 256<<20 {
		t.Fatal(string(b), err)
	}
	forged := cg
	forged.Inode++
	if forged.Verify() == nil {
		t.Fatal("accepted replaced cgroup identity")
	}
	if err = RemoveCgroups(context.Background(), r.Journal); err != nil {
		t.Fatal(err)
	}
	for _, resource := range r.Journal.Resources {
		if resource.State != "released" {
			t.Fatal(resource)
		}
	}
}
