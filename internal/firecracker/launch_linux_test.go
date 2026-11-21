package firecracker

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LeeXN/Flintyard/internal/host"
	"github.com/LeeXN/Flintyard/internal/state"
	"golang.org/x/sys/unix"
)

func TestJailerArgumentsAndMemoryHeadroom(t *testing.T) {
	owner := state.Owner{RootID: state.UUID(), EnvironmentID: state.UUID(), NodeID: state.UUID()}
	for _, version := range []int{1, 2} {
		cg := Cgroup{Layout: CgroupLayout{Version: version, Mount: "/sys/fs/cgroup"}, Path: "/sys/fs/cgroup/owned/" + owner.NodeID, MemoryBytes: 256 << 20}
		r := LaunchRequest{Owner: owner, UID: 60000, GID: 60000, JailBase: "/run/flintyard/j", NetNS: "/run/netns/owned", Cgroup: cg, Binaries: Binaries{Firecracker: host.Binary{Path: "/usr/bin/firecracker"}}}
		args, err := r.Args()
		if err != nil {
			t.Fatal(err)
		}
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "daemonize") || strings.Contains(joined, "new-pid-ns") || strings.Contains(joined, "no-seccomp") {
			t.Fatal("weakened jailer baseline", args)
		}
		if !strings.HasSuffix(joined, "-- --api-sock /run/api.sock") {
			t.Fatal(args)
		}
		if version == 2 && strings.Contains(joined, "--cgroup ") {
			t.Fatal("v2 must enter pre-created leaf without altering ancestors", args)
		}
	}
	for guest, want := range map[int]uint64{128: 256 << 20, 512: 640 << 20, 1024: 1280 << 20} {
		got, err := MemoryLimit(guest)
		if err != nil || got != want {
			t.Fatal(guest, got, err)
		}
	}
}

func TestJailerUsesExplicitPCITransport(t *testing.T) {
	owner := state.Owner{RootID: state.UUID(), EnvironmentID: state.UUID(), NodeID: state.UUID()}
	cg := Cgroup{Layout: CgroupLayout{Version: 1, Mount: "/sys/fs/cgroup"}, Path: "/sys/fs/cgroup/owned/" + owner.NodeID, MemoryBytes: 256 << 20}
	r := LaunchRequest{Owner: owner, UID: 60000, GID: 60000, JailBase: "/run/flintyard/j", NetNS: "/run/netns/owned", Cgroup: cg, Binaries: Binaries{Firecracker: host.Binary{Path: "/usr/bin/firecracker"}}}
	for _, transport := range []string{"", "mmio", "pci", "invalid"} {
		r.VirtioTransport = transport
		args, err := r.Args()
		if transport == "invalid" {
			if err == nil {
				t.Fatal("accepted unknown transport")
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "--enable-pci") != (transport == "pci") || (transport == "pci" && !strings.HasSuffix(joined, "-- --api-sock /run/api.sock --enable-pci")) {
			t.Fatal(transport, args)
		}
	}
}
func TestCgroupMountSelectionParsesBothHierarchies(t *testing.T) {
	mounts, err := parseCgroupMounts("cgroup2 /sys/fs/cgroup cgroup2 rw,nosuid,nodev 0 0\ncgroup /sys/fs/cgroup/memory cgroup rw,memory 0 0\n")
	if err != nil || len(mounts) != 2 || mounts[0].Version != 2 || mounts[1].Version != 1 {
		t.Fatal(mounts, err)
	}
}

func TestDetachedHelper(t *testing.T) {
	args := os.Args
	if len(args) >= 3 && args[len(args)-1] == "flintyard-detached-worker" {
		b := make([]byte, 1)
		n, _ := os.Stdin.Read(b)
		pid := os.Getpid()
		sid, err := unix.Getsid(0)
		if err != nil || sid != pid || unix.Getpgrp() != pid || n != 0 {
			os.Exit(18)
		}
		fmt.Fprintln(os.Stdout, "independent-session null-stdin ordinary-log")
		for {
			time.Sleep(time.Hour)
		}
	}
	if len(args) >= 5 && args[len(args)-3] == "flintyard-detached-parent" {
		log, err := os.OpenFile(args[len(args)-2], os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
		if err != nil {
			os.Exit(19)
		}
		d, err := startDetached(os.Args[0], []string{"-test.run=^TestDetachedHelper$", "--", "flintyard-detached-worker"}, log, nil)
		log.Close()
		if err != nil {
			os.Exit(20)
		}
		identity, err := Observe(d.pid)
		if err != nil {
			os.Exit(21)
		}
		if err = state.Write(args[len(args)-1], identity); err != nil {
			os.Exit(22)
		}
		unix.Close(d.fd)
		os.Exit(0)
	}
}
func TestDetachedPrimitiveSurvivesLauncherExit(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "serial.log")
	record := filepath.Join(dir, "identity.json")
	parent := exec.Command(os.Args[0], "-test.run=^TestDetachedHelper$", "--", "flintyard-detached-parent", log, record)
	if output, err := parent.CombinedOutput(); err != nil {
		t.Fatal(err, string(output))
	}
	var identity Identity
	b, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(b, &identity); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = Terminate(ctx, TerminateRequest{Identity: identity, Grace: 0})
	})
	deadline := time.Now().Add(5 * time.Second)
	for {
		b, _ = os.ReadFile(log)
		if strings.Contains(string(b), "independent-session") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("child did not retain ordinary log/session after parent exit")
		}
		time.Sleep(10 * time.Millisecond)
	}
	phase, err := ProcessState(context.Background(), identity)
	if err != nil || phase != "running" {
		t.Fatal("parent exit ended child", phase, err)
	}
}
