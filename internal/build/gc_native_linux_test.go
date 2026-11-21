package build

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/LeeXN/Flintyard/internal/firecracker"
	"github.com/LeeXN/Flintyard/internal/state"
	"golang.org/x/sys/unix"
)

func nativeBuildGCOnly(t *testing.T) {
	t.Helper()
	if os.Getenv("FY_BUILD_GC_INTEGRATION") != "1" {
		t.Skip("requires tests/native-build-gc.sh; no native build recovery checked")
	}
	nativeBuildOnly(t)
	self, _ := os.Readlink("/proc/self/ns/mnt")
	initial, _ := os.Readlink("/proc/1/ns/mnt")
	var fs unix.Statfs_t
	if self == initial || unix.Statfs("/run", &fs) != nil || fs.Type != unix.TMPFS_MAGIC {
		t.Fatal("refusing build GC native checks outside private mount namespace and /run")
	}
}

func TestNativeGCBuildRejectsForeignMountAndOutputAlias(t *testing.T) {
	nativeBuildGCOnly(t)
	for _, outside := range []bool{false, true} {
		t.Run(map[bool]string{false: "mounted-child", true: "mounted-alias"}[outside], func(t *testing.T) {
			root, id, dir := gcBuildFixture(t)
			ctx := context.Background()
			unlock, err := LockAttempt(ctx, root, id, false)
			if err != nil {
				t.Fatal(err)
			}
			defer unlock()
			target, err := GCSnapshot(ctx, root, id)
			if err != nil {
				t.Fatal(err)
			}
			foreign := t.TempDir()
			sentinel := filepath.Join(foreign, "retain")
			if err = os.WriteFile(sentinel, []byte("foreign data"), 0600); err != nil {
				t.Fatal(err)
			}
			source, destination := foreign, filepath.Join(dir, "root", "mounted")
			if outside {
				source, destination = filepath.Join(dir, "payload"), filepath.Join(t.TempDir(), "alias")
			}
			if err = os.Mkdir(destination, 0700); err != nil {
				t.Fatal(err)
			}
			if err = unix.Mount(source, destination, "", unix.MS_BIND, ""); err != nil {
				t.Fatal(err)
			}
			defer unix.Unmount(destination, 0)
			if err = ApplyGC(ctx, root, target); err == nil {
				t.Fatal("accepted newly mounted scope")
			}
			if _, err = os.Stat(filepath.Join(dir, "root/etc/fixture")); err != nil {
				t.Fatal("snapshot failure deleted own data", err)
			}
			if outside {
				if err = checkBuildOutput(root.Path, filepath.Join(destination, "user.fyimg")); err == nil {
					t.Fatal("output bind alias into scratch accepted")
				}
			} else if err = removeBuildTree(ctx, dir, "root"); err == nil {
				t.Fatal("fd traversal crossed a bind mount")
			}
			if b, err := os.ReadFile(sentinel); err != nil || string(b) != "foreign data" {
				t.Fatal("foreign data changed", string(b), err)
			}
		})
	}
}

func TestNativeGCBuildExcludesActiveLoopThenReconcilesReleasedJournal(t *testing.T) {
	nativeBuildGCOnly(t)
	root, id, dir := gcBuildFixture(t)
	ctx := context.Background()
	control, err := unix.Open("/dev/loop-control", unix.O_RDWR|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENODEV) || errors.Is(err, unix.ENOENT) {
		t.Skipf("BLOCKED: kernel loop control: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(control)
	number, err := unix.IoctlRetInt(control, unix.LOOP_CTL_GET_FREE)
	if err != nil {
		t.Fatal(err)
	}
	device := "/dev/loop" + strconv.Itoa(number)
	if err = unix.Mknod(device, unix.S_IFBLK|0600, int(unix.Mkdev(7, uint32(number)))); err != nil && !errors.Is(err, unix.EEXIST) {
		t.Fatal(err)
	}
	loop, err := os.OpenFile(device, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer loop.Close()
	disk := filepath.Join(dir, "payload/rootfs.raw")
	backing, err := os.OpenFile(disk, os.O_RDWR|unix.O_NOFOLLOW, 0)
	if err != nil {
		t.Fatal(err)
	}
	var st unix.Stat_t
	if err = unix.Fstat(int(backing.Fd()), &st); err != nil {
		backing.Close()
		t.Fatal(err)
	}
	j, err := state.NewJournal(filepath.Join(dir, "worker-journal.json"), id)
	if err != nil {
		backing.Close()
		t.Fatal(err)
	}
	meta := map[string]string{"device": device, "disk": disk, "backing_device": strconv.FormatUint(uint64(st.Dev), 10), "backing_inode": strconv.FormatUint(st.Ino, 10), "owner": id}
	resource, err := j.IntentIdentity("build-loop", meta)
	if err != nil {
		backing.Close()
		t.Fatal(err)
	}
	config := unix.LoopConfig{Fd: uint32(backing.Fd()), Info: unix.LoopInfo64{Flags: unix.LO_FLAGS_AUTOCLEAR}}
	copy(config.Info.File_name[:], []byte("flintyard-build:"+id))
	err = unix.IoctlLoopConfigure(int(loop.Fd()), &config)
	backing.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err = j.Done(resource, meta); err != nil {
		t.Fatal(err)
	}
	unlock, err := LockAttempt(ctx, root, id, false)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if _, err = GCSnapshot(ctx, root, id); err == nil || !strings.Contains(err.Error(), "loop") {
		t.Fatal("active backing was not excluded", err)
	}
	if _, err = os.Stat(disk); err != nil {
		t.Fatal(err)
	}
	if err = loop.Close(); err != nil {
		t.Fatal(err)
	}
	target, err := GCSnapshot(ctx, root, id)
	if err != nil {
		t.Fatal(err)
	}
	if err = ApplyGC(ctx, root, target); err != nil {
		t.Fatal(err)
	}
	stored, err := state.ReadJournal(filepath.Join(dir, "worker-journal.json"), id)
	if err != nil || stored.Resources[0].State != "released" {
		t.Fatal(stored, err)
	}
}

func TestNativeGCBuildSIGKILLDeadWorkerScratchIsRecoverable(t *testing.T) {
	nativeBuildGCOnly(t)
	dir, recipe := recipeFixture(t)
	recipe.Steps = []Step{{Argv: []string{"/fixture-step", "wait"}, WorkingDir: "/", TimeoutSeconds: 60}}
	file := writeRecipe(t, dir, recipe)
	stateDir := filepath.Join(dir, "state")
	log, err := os.Create(filepath.Join(dir, "controller.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	cmd := exec.Command(os.Getenv("FY_BUILD_WORKER"), "image", "build", "--recipe", file, "--offline", "--state-dir", stateDir, "--json")
	cmd.Stdout, cmd.Stderr = log, log
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	deadline := time.Now().Add(15 * time.Second)
	id := ""
	var worker firecracker.Identity
	for time.Now().Before(deadline) {
		entries, _ := os.ReadDir(filepath.Join(stateDir, "builds"))
		for _, entry := range entries {
			if !state.ValidUUID(entry.Name()) {
				continue
			}
			buildDir := filepath.Join(stateDir, "builds", entry.Name())
			if _, e := os.Stat(filepath.Join(buildDir, "root/waiting-step")); e == nil {
				if e = state.Read(filepath.Join(buildDir, "worker-identity.json"), &worker); e == nil {
					id = entry.Name()
					break
				}
			}
		}
		if id != "" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if id == "" {
		t.Fatal("actual isolated step did not reach wait checkpoint")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	root, err := state.OpenRoot(ctx, stateDir, false)
	if err != nil {
		t.Fatal(err)
	}
	targets, excluded, err := GCInventory(ctx, root)
	if err != nil || len(targets) != 0 || len(excluded) != 1 || !strings.Contains(excluded[0].Reason, "active") {
		t.Fatal(targets, excluded, err)
	}
	if err = cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err = cmd.Wait(); err == nil {
		t.Fatal("SIGKILL unexpectedly succeeded")
	}
	for {
		phase, e := firecracker.ProcessState(ctx, worker)
		// Exit can race the proc verification after pidfd polling. Re-poll the
		// same recorded worker on ENOENT; do not mistake it for a new launch.
		if e != nil && !errors.Is(e, os.ErrNotExist) && !errors.Is(e, unix.ESRCH) {
			t.Fatal(e)
		}
		if e == nil && phase == "exited" {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("worker survived controller death")
		case <-time.After(20 * time.Millisecond):
		}
	}
	planFile := filepath.Join(dir, "gc-plan.json")
	planCmd := exec.CommandContext(ctx, os.Getenv("FY_BUILD_WORKER"), "gc", "--dry-run", "--plan", planFile, "--state-dir", stateDir, "--json")
	data, err := planCmd.CombinedOutput()
	if err != nil {
		t.Fatal("public GC plan", err, string(data))
	}
	var planned struct {
		OK   bool `json:"ok"`
		Data struct {
			Plan struct {
				Builds []GCTarget `json:"builds"`
			} `json:"plan"`
		} `json:"data"`
	}
	if err = json.Unmarshal(data, &planned); err != nil || !planned.OK || len(planned.Data.Plan.Builds) != 1 || planned.Data.Plan.Builds[0].BuildID != id {
		t.Fatal("public GC target", err, string(data))
	}
	applyCmd := exec.CommandContext(ctx, os.Getenv("FY_BUILD_WORKER"), "gc", "--apply", "--plan", planFile, "--state-dir", stateDir, "--json")
	data, err = applyCmd.CombinedOutput()
	if err != nil {
		t.Fatal("public GC apply", err, string(data))
	}
	var applied struct {
		OK   bool `json:"ok"`
		Data struct {
			Applied       bool     `json:"applied"`
			BuildsCleaned []string `json:"builds_cleaned"`
		} `json:"data"`
	}
	if err = json.Unmarshal(data, &applied); err != nil || !applied.OK || !applied.Data.Applied || len(applied.Data.BuildsCleaned) != 1 || applied.Data.BuildsCleaned[0] != id {
		t.Fatal("public GC result", err, string(data))
	}
	buildDir := filepath.Join(stateDir, "builds", id)
	var record buildRecord
	if err = state.Read(filepath.Join(buildDir, "build.json"), &record); err != nil || record.CleanupState != "complete" || record.State != "failed" || record.FinishedAt == nil {
		t.Fatal(record, err)
	}
	if _, err = os.Stat(filepath.Join(dir, recipe.Output.Path)); !os.IsNotExist(err) {
		t.Fatal("interrupted build published package", err)
	}
	if _, err = os.Stat(filepath.Join(buildDir, "build.log")); err != nil {
		t.Fatal("gc lost build diagnostics", err)
	}
}
