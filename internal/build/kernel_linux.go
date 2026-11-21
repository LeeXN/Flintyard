package build

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/LeeXN/Flintyard/internal/guestinit"
	"github.com/LeeXN/Flintyard/internal/host"
	"github.com/LeeXN/Flintyard/internal/image"
	"github.com/LeeXN/Flintyard/internal/state"
	"golang.org/x/sys/unix"
)

func kernelTools(ctx context.Context, patches bool) (*image.KernelBuild, error) {
	if patches {
		if _, err := exec.LookPath("patch"); err != nil {
			return nil, err
		}
	}
	p := &image.KernelBuild{Patches: []string{}, Commands: [][]string{}}
	for name, dst := range map[string]*string{"gcc": &p.Compiler, "ld": &p.Linker, "make": &p.Make} {
		r, err := host.Run(ctx, host.CommandRequest{Name: name, Args: []string{"--version"}})
		if err != nil {
			return nil, err
		}
		if r.Truncated || strings.TrimSpace(r.Output) == "" {
			return nil, fmt.Errorf("invalid %s version output", name)
		}
		*dst = strings.TrimSpace(r.Output)
	}
	return p, nil
}

func kernelTarget(arch, target string) (string, string, error) {
	switch arch {
	case "x86_64":
		if target == "vmlinux" {
			return "x86_64", "vmlinux", nil
		}
		if target == "bzImage" {
			return "x86_64", "arch/x86/boot/bzImage", nil
		}
	case "aarch64":
		if target == "Image" {
			return "arm64", "arch/arm64/boot/Image", nil
		}
	}
	return "", "", fmt.Errorf("unsupported kernel target %s/%s", arch, target)
}

var releaseName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,127}$`)

// buildSourceKernel runs only fixed host argv for the explicitly trusted source
// Makefile, inside the build worker's private namespaces. Rootfs steps use the
// separate chroot worker. There is no olddefconfig or cross-compiler fallback.
func buildSourceKernel(ctx context.Context, c workerConfig, root, payload string) (image.Kernel, *image.KernelBuild, error) {
	k := c.Recipe.Kernel
	result := image.Kernel{Origin: k.Origin, Modules: "none"}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(k.TimeoutSeconds)*time.Second)
	defer cancel()
	arch, artifact, err := kernelTarget(c.Recipe.Architecture, k.ImageTarget)
	if err != nil {
		return result, nil, err
	}
	p, err := kernelTools(ctx, len(k.Patches) > 0)
	if err != nil {
		return result, nil, err
	}
	if c.KernelTools != nil {
		a, _ := json.Marshal(p)
		b, _ := json.Marshal(c.KernelTools)
		if string(a) != string(b) {
			return result, nil, fmt.Errorf("kernel tool versions changed after preflight")
		}
	}
	tree := filepath.Join(c.BuildDir, "kernel-source")
	if _, err = image.ExtractRootfs(ctx, image.ExtractRequest{Archive: c.Inputs["kernel"].Path, Destination: tree, StripComponents: k.StripComponents}); err != nil {
		return result, nil, err
	}
	log, err := os.OpenFile(filepath.Join(c.BuildDir, "build.log"), os.O_WRONLY|os.O_APPEND|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return result, nil, err
	}
	defer log.Close()
	run := func(name string, args []string, input io.Reader) (string, error) {
		p.Commands = append(p.Commands, append([]string{name}, args...))
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.Dir = c.BuildDir
		cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C", "LC_ALL=C", "HOME=" + c.BuildDir, "KCONFIG_NOSILENTUPDATE=1"}
		cmd.Stdin = input
		var output strings.Builder
		// Only kernelrelease is captured. Compiler output streams to the log.
		if name == "make" && args[len(args)-1] == "kernelrelease" {
			cmd.Stdout = &limitedText{dst: &output, remaining: 4096}
		} else {
			cmd.Stdout = log
		}
		cmd.Stderr = log
		cmd.WaitDelay = time.Second
		if err := cmd.Run(); err != nil {
			if ctx.Err() != nil {
				err = ctx.Err()
			}
			return "", fmt.Errorf("kernel command %s: %w (see build.log)", name, err)
		}
		return output.String(), nil
	}
	for i := range k.Patches {
		in := c.Inputs[fmt.Sprintf("kernel-patch-%03d", i)]
		f, err := os.Open(in.Path)
		if err != nil {
			return result, nil, err
		}
		_, err = run("patch", []string{"--batch", "--forward", "--fuzz=0", "--no-backup-if-mismatch", "-p1", "-d", "kernel-source"}, f)
		f.Close()
		if err != nil {
			return result, nil, fmt.Errorf("kernel patch %d: %w", i, err)
		}
		p.Patches = append(p.Patches, in.SHA256)
	}
	configPath := filepath.Join(tree, ".config")
	if _, err := os.Lstat(configPath); err == nil {
		// The recipe config is authoritative, including when the source carries
		// one. Never follow a source-provided .config symlink.
		if err = os.Remove(configPath); err != nil {
			return result, nil, err
		}
	} else if !os.IsNotExist(err) {
		return result, nil, err
	}
	if err = copyFile(ctx, c.Inputs["kernel-config"].Path, configPath, 0600); err != nil {
		return result, nil, err
	}
	config, err := readKernelConfig(configPath)
	if err != nil {
		return result, nil, err
	}
	checkConfig := func() error {
		sum, _, err := image.Hash(ctx, configPath)
		if err != nil {
			return err
		}
		if sum != c.Inputs["kernel-config"].SHA256 {
			actual, e := readKernelConfig(configPath)
			if e != nil {
				return e
			}
			added, removed := configDifference(config, actual)
			diff := struct {
				Expected, Actual string
				Added, Removed   []string
			}{c.Inputs["kernel-config"].SHA256, sum, added, removed}
			e = state.Write(filepath.Join(c.BuildDir, "kernel-config-diff.json"), diff)
			return errors.Join(fmt.Errorf("kernel .config changed; normalization is not authorized (see kernel-config-diff.json)"), e)
		}
		return nil
	}
	modules := false
	for _, line := range strings.Split(string(config), "\n") {
		if line == "CONFIG_MODULES=y" {
			modules = true
		}
	}
	base := []string{"-C", "kernel-source", "ARCH=" + arch, "CC=gcc", "LD=ld", fmt.Sprintf("-j%d", k.Jobs)}
	makeTarget := func(target string, extra ...string) (string, error) {
		args := append(append([]string{}, base...), extra...)
		args = append(args, target)
		text, err := run("make", args, nil)
		return text, errors.Join(err, checkConfig())
	}
	target := k.ImageTarget
	if _, err = makeTarget(target); err != nil {
		return result, nil, err
	}
	release, err := makeTarget("kernelrelease", "-s", "--no-print-directory")
	if err != nil {
		return result, nil, err
	}
	release = strings.TrimSpace(release)
	if !releaseName.MatchString(release) || release != k.ExpectedRelease {
		return result, nil, fmt.Errorf("kernel release %q differs from expected %q", release, k.ExpectedRelease)
	}
	result.Release, result.Config = release, c.Inputs["kernel-config"].SHA256
	if err = ValidateKernel(filepath.Join(tree, artifact), c.Recipe.Architecture); err != nil {
		return result, nil, err
	}
	if err = copyFile(ctx, filepath.Join(tree, artifact), filepath.Join(payload, "kernel"), 0400); err != nil {
		return result, nil, err
	}
	if err = installKernelConfig(root, release, config); err != nil {
		return result, nil, err
	}
	if modules {
		if _, err = makeTarget("modules"); err != nil {
			return result, nil, err
		}
		// Install into a fresh staging tree, never through guest /lib symlinks.
		stage := filepath.Join(c.BuildDir, "module-stage")
		if err = os.Mkdir(stage, 0700); err != nil {
			return result, nil, err
		}
		if _, err = makeTarget("modules_install", "INSTALL_MOD_PATH=../module-stage", "DEPMOD=/bin/true"); err != nil {
			return result, nil, err
		}
		if err = installModules(ctx, stage, root, release); err != nil {
			return result, nil, err
		}
		// depmod runs with guest tools and its view of /lib, in a chroot/PID ns.
		step := Step{Argv: []string{"depmod", "-a", release}, WorkingDir: "/", TimeoutSeconds: k.TimeoutSeconds}
		d := c
		d.Operation = "step"
		d.StepIndex = len(d.Recipe.Steps)
		d.Recipe.Steps = append(append([]Step{}, d.Recipe.Steps...), step)
		if err = runWorker(ctx, d, nil); err != nil {
			return result, nil, fmt.Errorf("guest depmod: %w", err)
		}
		p.Commands = append(p.Commands, []string{"guest:depmod", "-a", release})
		if err = requireModuleFile(root, release, "modules.dep"); err != nil {
			return result, nil, err
		}
		result.Modules = "embedded"
	}
	p.ConfigSHA256, p.ConfigUnchanged = result.Config, true
	if err = checkConfig(); err != nil {
		return result, nil, err
	}
	if err = state.Write(filepath.Join(c.BuildDir, "kernel-build.json"), p); err != nil {
		return result, nil, err
	}
	return result, p, nil
}

// Guest initramfs generators must inspect the target kernel, not the host's
// /proc/config.gz. Preserve the exact locked bytes and reject a conflicting
// same-release declaration already present in the input root.
func installKernelConfig(rootPath, release string, config []byte) error {
	if !releaseName.MatchString(release) {
		return fmt.Errorf("invalid kernel config release")
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return err
	}
	defer root.Close()
	name := "/boot/config-" + release
	path, err := guestinit.Resolve(root, name, true)
	if err != nil {
		return err
	}
	f, err := root.OpenFile(path, os.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW, 0)
	if err == nil {
		st, e := f.Stat()
		if e != nil || !st.Mode().IsRegular() {
			f.Close()
			return errors.Join(e, fmt.Errorf("guest kernel config is not an ordinary file"))
		}
		b, e := io.ReadAll(io.LimitReader(f, (4<<20)+1))
		closeErr := f.Close()
		if e != nil || closeErr != nil {
			return errors.Join(e, closeErr)
		}
		if string(b) != string(config) {
			return fmt.Errorf("guest contains a different config for kernel release %s", release)
		}
		return nil
	}
	if !os.IsNotExist(err) {
		return err
	}
	return guestinit.WriteGuest(root, name, config, 0444)
}

func readKernelConfig(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, (4<<20)+1))
	if len(b) > 4<<20 {
		return nil, fmt.Errorf("kernel config exceeds 4 MiB")
	}
	return b, err
}

func configDifference(expected, actual []byte) (added, removed []string) {
	a, b := map[string]bool{}, map[string]bool{}
	for _, line := range strings.Split(string(expected), "\n") {
		a[line] = true
	}
	for _, line := range strings.Split(string(actual), "\n") {
		b[line] = true
	}
	for line := range b {
		if !a[line] {
			added = append(added, line)
		}
	}
	for line := range a {
		if !b[line] {
			removed = append(removed, line)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	return
}

type limitedText struct {
	dst       *strings.Builder
	remaining int
}

func (w *limitedText) Write(p []byte) (int, error) {
	if len(p) > w.remaining {
		return 0, fmt.Errorf("kernelrelease output exceeds limit")
	}
	w.remaining -= len(p)
	return w.dst.Write(p)
}

func installModules(ctx context.Context, stage, root, release string) error {
	source := filepath.Join(stage, "lib", "modules", release)
	st, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if !st.IsDir() {
		return fmt.Errorf("modules_install did not produce the actual release directory")
	}
	guest, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer guest.Close()
	parent, err := guestinit.Resolve(guest, "/lib/modules", true)
	if err != nil {
		return err
	}
	if err = guest.MkdirAll(parent, 0755); err != nil {
		return err
	}
	dest := filepath.Join(parent, release)
	if _, err = guest.Lstat(dest); err == nil {
		return fmt.Errorf("refusing to merge source modules with an existing release directory")
	} else if !os.IsNotExist(err) {
		return err
	}
	if err = guest.Mkdir(dest, 0755); err != nil {
		return err
	}
	return filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		target := filepath.Join(dest, rel)
		if entry.IsDir() {
			return guest.Mkdir(target, 0755)
		}
		if entry.Type()&os.ModeSymlink != 0 {
			if rel == "build" || rel == "source" {
				return nil
			}
			return fmt.Errorf("unexpected module symlink %s", rel)
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("unexpected module file %s", rel)
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := guest.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
		if err != nil {
			return err
		}
		_, err = io.Copy(out, &ctxReader{ctx: ctx, r: in})
		return errors.Join(err, out.Close())
	})
}

func requireModuleFile(root, release, name string) error {
	guest, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer guest.Close()
	path, err := guestinit.Resolve(guest, "/lib/modules/"+release+"/"+name, false)
	if err != nil {
		return err
	}
	st, err := guest.Stat(path)
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("guest depmod did not create %s", name)
	}
	return nil
}
