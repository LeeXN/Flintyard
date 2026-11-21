package build

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LeeXN/Flintyard/internal/firecracker"
	"github.com/LeeXN/Flintyard/internal/host"
	"github.com/LeeXN/Flintyard/internal/image"
	"github.com/LeeXN/Flintyard/internal/state"
	"golang.org/x/sys/unix"
)

func nativeBuildOnly(t *testing.T) {
	t.Helper()
	if os.Getenv("FY_BUILD_INTEGRATION") != "1" {
		t.Skip("requires tests/native-build.sh; no privileged build checked")
	}
	if os.Geteuid() != 0 || os.Getenv("FY_BUILD_WORKER") == "" {
		t.Fatal("build test prerequisites not established")
	}
}
func TestNativeBuildRealExt4StepOfflinePackageAndCache(t *testing.T) {
	nativeBuildOnly(t)
	hostnameBefore, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	dir, r := recipeFixture(t)
	sentinel := filepath.Join(dir, "host-secret")
	if err := os.WriteFile(sentinel, []byte("must not enter guest root"), 0600); err != nil {
		t.Fatal(err)
	}
	r.Steps = []Step{{Argv: []string{"/fixture-step", "write", sentinel}, WorkingDir: "/", TimeoutSeconds: 10}}
	file := writeRecipe(t, dir, r)
	root := filepath.Join(dir, "state")
	result, err := Build(context.Background(), Request{StateDir: root, RecipeFile: file, Offline: true, WorkerPath: os.Getenv("FY_BUILD_WORKER")})
	if err != nil {
		t.Fatal(err)
	}
	if result.Image.Validation != "not_run" || result.Image.SourceTrust != "unverified" || result.Image.Manifest.OS.Family != "contract-fixture" {
		t.Fatal("synthetic build claimed external trust or VM validation", result)
	}
	disk := filepath.Join(root, "images", "sha256-"+strings.TrimPrefix(result.Image.ID, "sha256:"), "payload", "rootfs.raw")
	for name, want := range map[string]string{"/etc/fixture": "real root filesystem content\n", "/made-by-step": "real isolated build step\n"} {
		out, err := host.Run(context.Background(), host.CommandRequest{Name: "debugfs", Args: []string{"-R", "cat " + name, disk}})
		if err != nil || !strings.HasSuffix(out.Output, want) {
			t.Fatal(name, out, err)
		}
	}
	attrs, err := host.Run(context.Background(), host.CommandRequest{Name: "debugfs", Args: []string{"-R", "ea_get /etc/fixture user.fixture", disk}})
	if err != nil || !strings.Contains(attrs.Output, "preserved through ext4") {
		t.Fatal("ext4 generation lost archive xattr", attrs, err)
	}
	capFile := filepath.Join(dir, "capability-output")
	if _, err = host.Run(context.Background(), host.CommandRequest{Name: "debugfs", Args: []string{"-R", "ea_get -f " + capFile + " /etc/fixture security.capability", disk}}); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(capFile); err != nil || !bytes.Equal(b, capabilityFixture()) {
		t.Fatal("ext4 output lost Linux file capabilities", b, err)
	}
	packagePath := filepath.Join(dir, "out/image.fyimg")
	moved := filepath.Join(dir, "portable.fyimg")
	if err = os.Rename(packagePath, moved); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(filepath.Join(dir, "root.tar")); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(filepath.Join(dir, "kernel")); err != nil {
		t.Fatal(err)
	}
	second, err := Build(context.Background(), Request{StateDir: root, RecipeFile: file, Offline: true, WorkerPath: os.Getenv("FY_BUILD_WORKER")})
	if err != nil || !second.CacheHit || second.Image.ID != result.Image.ID || second.Image.SourceTrust != "unverified" {
		t.Fatal(second, err)
	}
	other := filepath.Join(t.TempDir(), "other-state")
	imported, err := image.Import(context.Background(), other, moved, result.Image.PackageSHA256, image.Limits{})
	if err != nil || imported.ID != result.Image.ID {
		t.Fatal("package retained original build dependencies", imported, err)
	}
	if b, err := os.ReadFile(sentinel); err != nil || string(b) != "must not enter guest root" {
		t.Fatal("host sentinel changed", err)
	}
	if hostnameAfter, err := os.Hostname(); err != nil || hostnameAfter != hostnameBefore {
		t.Fatal("step changed host UTS namespace", hostnameAfter, err)
	}
}
func TestNativeBuildTimeoutStopsPIDNamespaceAndLeavesNoPublishedOutput(t *testing.T) {
	nativeBuildOnly(t)
	dir, r := recipeFixture(t)
	r.Steps = []Step{{Argv: []string{"/fixture-step", "wait"}, WorkingDir: "/", TimeoutSeconds: 1}}
	root := filepath.Join(dir, "state")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	result, err := Build(ctx, Request{StateDir: root, RecipeFile: writeRecipe(t, dir, r), Offline: true, WorkerPath: os.Getenv("FY_BUILD_WORKER")})
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(result, err)
	}
	if _, err = os.Stat(filepath.Join(dir, "out/image.fyimg")); !os.IsNotExist(err) {
		t.Fatal("published failed build")
	}
	var identity firecracker.Identity
	if err = state.Read(filepath.Join(root, "builds", result.BuildID, "worker-identity.json"), &identity); err != nil {
		t.Fatal(err)
	}
	if phase, err := firecracker.ProcessState(context.Background(), identity); err != nil || phase != "exited" {
		t.Fatal("worker survived failed build", phase, err)
	}
}

func TestNativeBuildRawExt4UsesPrivateCopy(t *testing.T) {
	nativeBuildOnly(t)
	dir, r := recipeFixture(t)
	base := filepath.Join(dir, "raw-root")
	if err := os.Mkdir(base, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "kept"), []byte("original raw content\n"), 0644); err != nil {
		t.Fatal(err)
	}
	raw := filepath.Join(dir, "raw.img")
	f, err := os.Create(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.Truncate(64 << 20); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if _, err = host.Run(context.Background(), host.CommandRequest{Name: "mkfs.ext4", Args: []string{"-q", "-F", "-d", base, raw}}); err != nil {
		t.Fatal(err)
	}
	sha, _, err := image.Hash(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	r.Rootfs = Rootfs{Kind: "raw", Source: Source{Path: "raw.img", SHA256: sha}}
	result, err := Build(context.Background(), Request{StateDir: filepath.Join(dir, "state"), RecipeFile: writeRecipe(t, dir, r), Offline: true, WorkerPath: os.Getenv("FY_BUILD_WORKER")})
	if errors.Is(err, unix.ENODEV) || errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ENOTTY) {
		t.Skipf("BLOCKED: raw build needs kernel loop support: %v", err)
	}
	if err != nil {
		t.Fatal(result, err)
	}
	after, _, err := image.Hash(context.Background(), raw)
	if err != nil || after != sha {
		t.Fatal("raw customization changed original input", after, err)
	}
	disk := filepath.Join(dir, "state", "images", "sha256-"+strings.TrimPrefix(result.Image.ID, "sha256:"), "payload", "rootfs.raw")
	output, err := host.Run(context.Background(), host.CommandRequest{Name: "debugfs", Args: []string{"-R", "cat /kept", disk}})
	if err != nil || !strings.HasSuffix(output.Output, "original raw content\n") {
		t.Fatal(output, err)
	}
}
