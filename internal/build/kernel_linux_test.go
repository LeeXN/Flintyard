package build

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestKernelTargetBothArchitectures(t *testing.T) {
	for _, tc := range []struct{ arch, target, makeArch, path string }{
		{"x86_64", "vmlinux", "x86_64", "vmlinux"},
		{"x86_64", "bzImage", "x86_64", "arch/x86/boot/bzImage"},
		{"aarch64", "Image", "arm64", "arch/arm64/boot/Image"},
	} {
		a, p, err := kernelTarget(tc.arch, tc.target)
		if err != nil || a != tc.makeArch || p != tc.path {
			t.Fatal(tc, a, p, err)
		}
	}
	for _, pair := range [][2]string{{"x86_64", "Image"}, {"aarch64", "bzImage"}, {"aarch64", "vmlinux"}, {"unknown", "Image"}} {
		if _, _, err := kernelTarget(pair[0], pair[1]); err == nil {
			t.Fatal("accepted", pair)
		}
	}
}

func TestKernelConfigDifferenceAndReleaseNames(t *testing.T) {
	added, removed := configDifference([]byte("CONFIG_A=y\n# CONFIG_B is not set\n"), []byte("CONFIG_A=y\nCONFIG_B=y\n"))
	if len(added) != 1 || added[0] != "CONFIG_B=y" || len(removed) != 1 || removed[0] != "# CONFIG_B is not set" {
		t.Fatal(added, removed)
	}
	for _, release := range []string{"../escape", "bad/release", "-option", "two words", ""} {
		if releaseName.MatchString(release) {
			t.Fatal("unsafe release", release)
		}
	}
}

func TestTargetKernelConfigIsAvailableToGuestInitramfsWithoutImplicitReplacement(t *testing.T) {
	root := t.TempDir()
	config := []byte("CONFIG_BLK_DEV_INITRD=y\nCONFIG_RD_GZIP=y\n")
	if err := installKernelConfig(root, "fixture", config); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(root, "boot/config-fixture")
	if b, err := os.ReadFile(p); err != nil || string(b) != string(config) {
		t.Fatal(string(b), err)
	}
	if err := installKernelConfig(root, "fixture", config); err != nil {
		t.Fatal(err)
	}
	if err := installKernelConfig(root, "fixture", []byte("different")); err == nil {
		t.Fatal("same-release guest config silently replaced")
	}
	if b, err := os.ReadFile(p); err != nil || string(b) != string(config) {
		t.Fatal("conflict changed guest config", string(b), err)
	}
}

func TestModulesInstallResolvesGuestLibAndRejectsMergeAndLinks(t *testing.T) {
	dir := t.TempDir()
	stage, root := filepath.Join(dir, "stage"), filepath.Join(dir, "root")
	src := filepath.Join(stage, "lib/modules/fixture/kernel")
	for _, d := range []string{src, filepath.Join(root, "usr/lib")} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("/usr/lib", filepath.Join(root, "lib")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "fixture.ko"), []byte("test module"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/private/kernel/tree", filepath.Join(filepath.Dir(src), "build")); err != nil {
		t.Fatal(err)
	}
	if err := installModules(context.Background(), stage, root, "fixture"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(root, "usr/lib/modules/fixture/kernel/fixture.ko"))
	if err != nil || string(b) != "test module" {
		t.Fatal(string(b), err)
	}
	if _, err := os.Lstat(filepath.Join(root, "usr/lib/modules/fixture/build")); !os.IsNotExist(err) {
		t.Fatal("host build link copied", err)
	}
	if err := installModules(context.Background(), stage, root, "fixture"); err == nil || !strings.Contains(err.Error(), "merge") {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(root, "usr/lib/modules/fixture")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/host/path", filepath.Join(src, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := installModules(context.Background(), stage, root, "fixture"); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatal(err)
	}
}
