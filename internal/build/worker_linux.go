package build

import (
	"context"
	"debug/elf"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"syscall"
	"time"

	"github.com/LeeXN/Flintyard/internal/contract"
	"github.com/LeeXN/Flintyard/internal/guestinit"
	"github.com/LeeXN/Flintyard/internal/host"
	"github.com/LeeXN/Flintyard/internal/image"
	"github.com/LeeXN/Flintyard/internal/state"
	"golang.org/x/sys/unix"
)

// Worker is an internal one-shot process entry, never a public execution API.
// Only cmd decides process exit; core routines preserve the error and report.
func Worker(ctx context.Context) (err error) {
	c, err := readWorkerConfig()
	if err != nil {
		return err
	}
	if c.Operation == "step" {
		return stepWorker(ctx, c)
	}
	stage := "prepare"
	defer func() {
		report := workerReport{OK: err == nil, Stage: stage}
		if err != nil {
			report.Message = err.Error()
			report.Cancelled = errors.Is(err, context.Canceled)
			report.Deadline = errors.Is(err, context.DeadlineExceeded)
			report.KernelIncompatible = errors.Is(err, image.ErrKernelIncompatible)
			var errno syscall.Errno
			if errors.As(err, &errno) {
				report.Errno = uint64(errno)
			}
		}
		err = errors.Join(err, state.Write(filepath.Join(c.BuildDir, "worker-result.json"), report))
	}()
	if err = unix.Mount("", "/", "", unix.MS_PRIVATE|unix.MS_REC, ""); err != nil {
		return err
	}
	// /proc inherited from the caller describes its PID namespace. Remount it
	// before observing nested step workers, whose PIDs are relative to ours.
	if err = unix.Mount("proc", "/proc", "proc", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, ""); err != nil {
		return err
	}
	if err = privateDevices(); err != nil {
		return err
	}
	journal, err := state.NewJournal(filepath.Join(c.BuildDir, "worker-journal.json"), c.Owner.EnvironmentID)
	if err != nil {
		return err
	}
	for kind, input := range c.Inputs {
		sum, _, e := image.Hash(ctx, input.Path)
		if e != nil {
			return e
		}
		if sum != input.SHA256 {
			return fmt.Errorf("locked %s input changed", kind)
		}
	}
	payload := filepath.Join(c.BuildDir, "payload")
	if err = os.Mkdir(payload, 0700); err != nil {
		return err
	}
	root := filepath.Join(c.BuildDir, "root")
	disk := filepath.Join(payload, "rootfs.raw")
	stage = "rootfs"
	var cleanupRaw func() error
	if c.Recipe.Rootfs.Kind == "archive" {
		id, e := journal.IntentIdentity("build-root", map[string]string{"path": root, "owner": c.Owner.Tag()})
		if e != nil {
			return e
		}
		if _, err = image.ExtractRootfs(ctx, image.ExtractRequest{Archive: c.Inputs["rootfs"].Path, Destination: root}); err != nil {
			return err
		}
		if err = journal.Done(id, map[string]string{"path": root, "owner": c.Owner.Tag()}); err != nil {
			return err
		}
	} else {
		if err = copyFile(ctx, c.Inputs["rootfs"].Path, disk, 0600); err != nil {
			return err
		}
		if err = checkExt4(disk); err != nil {
			return err
		}
		if err = os.Mkdir(root, 0700); err != nil {
			return err
		}
		mounted, e := mountRaw(ctx, disk, root, journal)
		if e != nil {
			return e
		}
		cleanupRaw = mounted
		defer func() { err = errors.Join(err, mounted()) }()
	}
	stage = "kernel"
	kernel := image.Kernel{Release: c.Recipe.Kernel.Release, Origin: c.Recipe.Kernel.Origin, Modules: c.Recipe.Kernel.Modules}
	if kernel.Modules == "rootfs" {
		kernel.Modules = "embedded"
	}
	var kernelBuild *image.KernelBuild
	if c.Recipe.Kernel.Mode == "source" {
		kernel, kernelBuild, err = buildSourceKernel(ctx, c, root, payload)
		if err != nil {
			return err
		}
	} else {
		if err = ValidateKernel(c.Inputs["kernel"].Path, c.Recipe.Architecture); err != nil {
			return err
		}
		if err = copyFile(ctx, c.Inputs["kernel"].Path, filepath.Join(payload, "kernel"), 0400); err != nil {
			return err
		}
	}
	if err = validateGuestArchitecture(root, c.Recipe.Architecture); err != nil {
		return err
	}
	stage = "steps"
	for index, step := range c.Recipe.Steps {
		if err = ctx.Err(); err != nil {
			return err
		}
		stepConfig := c
		stepConfig.Operation = "step"
		stepConfig.StepIndex = index
		stepCtx, cancel := context.WithTimeout(ctx, time.Duration(step.TimeoutSeconds)*time.Second)
		err = runWorker(stepCtx, stepConfig, nil)
		cancel()
		if err != nil {
			return fmt.Errorf("build step %d: %w", index, err)
		}
	}
	if kernel.Modules == "embedded" {
		stage = "modules"
		if err = requireModuleFile(root, kernel.Release, "modules.dep"); err != nil {
			return err
		}
	}
	stage = "guest_prepare"
	if err = guestinit.Prepare(guestinit.PrepareRequest{Root: root, Initialization: c.Recipe.Guest}); err != nil {
		return err
	}
	if input, ok := c.Inputs["initrd"]; ok {
		if err = copyFile(ctx, input.Path, filepath.Join(payload, "initrd"), 0400); err != nil {
			return err
		}
	}
	if c.Recipe.GeneratedInitrd != "" {
		guest, e := os.OpenRoot(root)
		if e != nil {
			return e
		}
		path, e := guestinit.Resolve(guest, c.Recipe.GeneratedInitrd, false)
		if e == nil {
			e = copyFile(ctx, filepath.Join(root, path), filepath.Join(payload, "initrd"), 0400)
		}
		guest.Close()
		if e != nil {
			return e
		}
	}
	stage = "filesystem"
	if c.Recipe.Rootfs.Kind == "archive" {
		if err = host.PublishFile(ctx, disk, 0600, func(f *os.File) error {
			if err := f.Truncate(int64(c.Recipe.Rootfs.SizeMiB) << 20); err != nil {
				return err
			}
			_, err := host.Run(ctx, host.CommandRequest{Name: "mkfs.ext4", Args: []string{"-q", "-F", "-m", "0", "-d", root, f.Name()}, Timeout: 10 * time.Minute})
			return err
		}); err != nil {
			return err
		}
	} else {
		// Sync/unmount before hashing the disk; the deferred compensation handles
		// failures too. No writable loop handle survives a completed worker.
		if err = cleanupRaw(); err != nil {
			return err
		}
	}
	stage = "manifest"
	m := image.Manifest{APIVersion: "flintyard/v1alpha1", Kind: "Image", Name: c.Recipe.Output.Name, Version: c.Recipe.Output.Version, Architecture: c.Recipe.Architecture, OS: c.Recipe.OS, Kernel: kernel, Initialization: c.Recipe.Guest, Provenance: image.Provenance{BuiltAt: time.Now().UTC().Format(time.RFC3339), Recipe: c.RecipeSHA256, Reproducibility: c.Recipe.Reproducibility, Builder: Protocol, KernelBuild: kernelBuild}}
	m.Boot = image.Boot{Kernel: "payload/kernel", Rootfs: "payload/rootfs.raw", RootDevice: c.Recipe.Boot.RootDevice, Args: c.Recipe.Boot.Args, VirtioTransport: c.Recipe.Boot.VirtioTransport}
	if _, e := os.Stat(filepath.Join(payload, "initrd")); e == nil {
		m.Boot.Initrd = "payload/initrd"
	}
	if c.Network && len(c.Recipe.Steps) > 0 {
		m.Provenance.Reproducibility = "best_effort"
	}
	for _, role := range []string{"kernel", "rootfs", "initrd"} {
		file := map[string]string{"kernel": "kernel", "rootfs": "rootfs.raw", "initrd": "initrd"}[role]
		sum, n, e := image.Hash(ctx, filepath.Join(payload, file))
		if os.IsNotExist(e) && role == "initrd" {
			continue
		}
		if e != nil {
			return e
		}
		m.Payloads = append(m.Payloads, image.Payload{Path: "payload/" + file, Role: role, Size: n, SHA256: sum})
	}
	keys := make([]string, 0, len(c.Inputs))
	for kind := range c.Inputs {
		keys = append(keys, kind)
	}
	sort.Strings(keys)
	for _, kind := range keys {
		input := c.Inputs[kind]
		m.Provenance.Inputs = append(m.Provenance.Inputs, image.Source{Kind: kind, Source: input.Source, SHA256: input.SHA256})
	}
	if err = m.Validate(); err != nil {
		return err
	}
	b, e := json.MarshalIndent(m, "", "  ")
	if e != nil {
		return e
	}
	if e = contract.Validate("image", b); e != nil {
		return e
	}
	return state.Atomic(filepath.Join(c.BuildDir, "manifest.json"), append(b, '\n'), 0600)
}

func stepWorker(ctx context.Context, c workerConfig) error {
	if c.StepIndex < 0 || c.StepIndex >= len(c.Recipe.Steps) {
		return fmt.Errorf("invalid worker step index")
	}
	step := c.Recipe.Steps[c.StepIndex]
	root := filepath.Join(c.BuildDir, "root")
	runtime.LockOSThread() // This one-shot process execs or exits while locked.
	if err := unix.Mount("", "/", "", unix.MS_PRIVATE|unix.MS_REC, ""); err != nil {
		return err
	}
	if err := unix.Chroot(root); err != nil {
		return err
	}
	if err := os.Chdir("/"); err != nil {
		return err
	}
	for _, dir := range []string{"/proc", "/dev", "/sys", "/tmp"} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return err
		}
	}
	if err := unix.Mount("proc", "/proc", "proc", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, ""); err != nil {
		return err
	}
	if err := unix.Mount("tmpfs", "/dev", "tmpfs", unix.MS_NOSUID, "mode=0755,size=16m"); err != nil {
		return err
	}
	for _, device := range []struct {
		name  string
		minor uint32
	}{{"null", 3}, {"zero", 5}, {"random", 8}, {"urandom", 9}} {
		if err := unix.Mknod("/dev/"+device.name, unix.S_IFCHR|0666, int(unix.Mkdev(1, device.minor))); err != nil {
			return err
		}
	}
	if err := os.Symlink("/proc/self/fd", "/dev/fd"); err != nil {
		return err
	}
	for _, pair := range [][2]string{{"stdin", "0"}, {"stdout", "1"}, {"stderr", "2"}} {
		if err := os.Symlink("/proc/self/fd/"+pair[1], "/dev/"+pair[0]); err != nil {
			return err
		}
	}
	// No host sysfs is exposed. Hardware/device administration is not a build
	// dependency; a recipe requiring it must fail instead of mounting the host.
	if err := unix.Mount("tmpfs", "/sys", "tmpfs", unix.MS_RDONLY|unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, "size=1m"); err != nil {
		return err
	}
	if err := os.Chdir(step.WorkingDir); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	values := map[string]string{"PATH": "/usr/sbin:/usr/bin:/sbin:/bin", "HOME": "/root", "LANG": "C"}
	for key, value := range step.Env {
		values[key] = value
	}
	var env []string
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		env = append(env, key+"="+values[key])
	}
	pathEnv := "/usr/sbin:/usr/bin:/sbin:/bin"
	if value, ok := step.Env["PATH"]; ok {
		pathEnv = value
	}
	if err := os.Setenv("PATH", pathEnv); err != nil {
		return err
	}
	program, err := exec.LookPath(step.Argv[0])
	if err != nil {
		return err
	}
	return unix.Exec(program, step.Argv, env)
}

func copyFile(ctx context.Context, source, dest string, mode os.FileMode) error {
	f, err := os.OpenFile(source, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("build copy source must be an ordinary file")
	}
	return host.PublishFile(ctx, dest, mode, func(out *os.File) error { _, err := io.Copy(out, ctxReader{ctx, f}); return err })
}
func ValidateKernel(file, architecture string) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	return image.ValidateKernel(f, architecture)
}
func validateGuestArchitecture(rootPath, architecture string) error {
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return err
	}
	defer root.Close()
	for _, name := range []string{"/bin/sh", "/bin/busybox", "/usr/bin/env", "/sbin/init"} {
		p, err := guestinit.Resolve(root, name, false)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		f, err := root.Open(p)
		if err != nil {
			return err
		}
		b := make([]byte, 4)
		n, _ := f.ReadAt(b, 0)
		if n < 4 || string(b) != "\x7fELF" {
			f.Close()
			continue
		}
		ef, err := elf.NewFile(f)
		f.Close()
		if err != nil {
			return err
		}
		want := elf.EM_X86_64
		if architecture == "aarch64" {
			want = elf.EM_AARCH64
		}
		if ef.Machine != want {
			return fmt.Errorf("guest executable architecture differs from recipe/host")
		}
	}
	return nil
}
func checkExt4(file string) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	b := make([]byte, 2)
	if _, err = f.ReadAt(b, 1080); err != nil {
		return err
	}
	if binary.LittleEndian.Uint16(b) != 0xef53 {
		return fmt.Errorf("raw build input is not direct ext4 (partitions/other filesystems are unsupported for customization)")
	}
	return nil
}
