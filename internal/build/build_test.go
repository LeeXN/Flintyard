package build

import (
	"archive/tar"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/LeeXN/Flintyard/internal/host"
	"github.com/LeeXN/Flintyard/internal/image"
)

func recipeFixture(t *testing.T) (string, Recipe) {
	t.Helper()
	dir := t.TempDir()
	kernel := make([]byte, 4096)
	if host.Architecture() == "aarch64" {
		binary.LittleEndian.PutUint32(kernel[56:60], 0x644d5241)
	} else {
		kernel[0x1fe], kernel[0x1ff] = 0x55, 0xaa
		copy(kernel[0x202:], "HdrS")
		binary.LittleEndian.PutUint16(kernel[0x236:], 1)
	}
	if err := os.WriteFile(filepath.Join(dir, "kernel"), kernel, 0600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(filepath.Join(dir, "root.tar"))
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(f)
	data := []byte("real root filesystem content\n")
	header := tar.Header{Name: "etc/fixture", Mode: 0644, Uid: os.Getuid(), Gid: os.Getgid(), Typeflag: tar.TypeReg, Size: int64(len(data)), Xattrs: map[string]string{"user.fixture": "preserved through ext4"}}
	if os.Getenv("FY_BUILD_STEP_FIXTURE") != "" {
		header.PAXRecords = map[string]string{"LIBARCHIVE.xattr.security.capability": base64.StdEncoding.EncodeToString(capabilityFixture())}
	}
	if err = tw.WriteHeader(&header); err != nil {
		t.Fatal(err)
	}
	if _, err = tw.Write(data); err != nil {
		t.Fatal(err)
	}
	if helper := os.Getenv("FY_BUILD_STEP_FIXTURE"); helper != "" {
		b, err := os.ReadFile(helper)
		if err != nil {
			t.Fatal(err)
		}
		if err = tw.WriteHeader(&tar.Header{Name: "bin/sh", Mode: 0755, Uid: os.Getuid(), Gid: os.Getgid(), Typeflag: tar.TypeReg, Size: int64(len(b))}); err != nil {
			t.Fatal(err)
		}
		if _, err = tw.Write(b); err != nil {
			t.Fatal(err)
		}
		if err = tw.WriteHeader(&tar.Header{Name: "fixture-step", Typeflag: tar.TypeLink, Linkname: "bin/sh", Mode: 0755}); err != nil {
			t.Fatal(err)
		}
		if err = tw.WriteHeader(&tar.Header{Name: "bin/depmod", Typeflag: tar.TypeLink, Linkname: "bin/sh", Mode: 0755}); err != nil {
			t.Fatal(err)
		}
	}
	if err = tw.Close(); err != nil {
		t.Fatal(err)
	}
	f.Close()
	rootSHA, _, err := image.Hash(context.Background(), filepath.Join(dir, "root.tar"))
	if err != nil {
		t.Fatal(err)
	}
	r := Recipe{APIVersion: "flintyard/v1alpha1", Kind: "BuildRecipe", Architecture: host.Architecture(), OS: image.OS{Family: "contract-fixture", Release: "nonbootable"}, Rootfs: Rootfs{Kind: "archive", Source: Source{Path: "root.tar", SHA256: rootSHA}, SizeMiB: 64}, Kernel: Kernel{Mode: "prebuilt", Source: Source{Path: "kernel", SHA256: image.Sum(kernel)}, Release: "fixture", Origin: "user-provided", Modules: "none"}, Guest: image.Initialization{Mode: "external", Username: "root", Probe: image.Probe{Type: "none", Port: 22}}, Steps: []Step{}, Requires: []string{}, Reproducibility: "inputs_locked", Output: Output{Name: "fixture", Version: "0", Path: "out/image.fyimg"}}
	r.Boot.RootDevice = "/dev/vda"
	r.Boot.Args = []string{"console=ttyS0"}
	return dir, r
}
func capabilityFixture() []byte {
	b := make([]byte, 20)
	binary.LittleEndian.PutUint32(b, 0x02000001)
	binary.LittleEndian.PutUint32(b[4:8], 1<<10)
	return b
}
func writeRecipe(t *testing.T, dir string, r Recipe) string {
	t.Helper()
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "recipe.json")
	if err = os.WriteFile(file, b, 0600); err != nil {
		t.Fatal(err)
	}
	return file
}
func TestRecipeRelativePathsAndPreflightValidation(t *testing.T) {
	dir, r := recipeFixture(t)
	file := writeRecipe(t, dir, r)
	loaded, err := LoadRecipe(context.Background(), file)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Recipe.Rootfs.Source.Path != filepath.Join(dir, "root.tar") || loaded.Recipe.Output.Path != filepath.Join(dir, "out/image.fyimg") {
		t.Fatal(loaded)
	}
	if err = ValidateKernel(loaded.Recipe.Kernel.Source.Path, r.Architecture); err != nil {
		t.Fatal(err)
	}
	r.Rootfs.Source.SHA256 = string(make([]byte, 64))
	file = writeRecipe(t, dir, r)
	if _, err = LoadRecipe(context.Background(), file); err == nil {
		t.Fatal("accepted unlocked input")
	}
}

func TestRecipeTransportRequiresValidPCIArguments(t *testing.T) {
	dir, r := recipeFixture(t)
	r.Boot.VirtioTransport = "pci"
	if got, err := LoadRecipe(context.Background(), writeRecipe(t, dir, r)); err != nil || got.Recipe.Boot.VirtioTransport != "pci" {
		t.Fatal(got, err)
	}
	for _, args := range [][]string{{"pci=off"}, {"pci=noaer,off"}} {
		r.Boot.Args = args
		if _, err := LoadRecipe(context.Background(), writeRecipe(t, dir, r)); err == nil {
			t.Fatal("accepted PCI-disabling command line", args)
		}
	}
	r.Boot.Args = []string{"console=ttyS0"}
	r.Boot.VirtioTransport = "other"
	if _, err := LoadRecipe(context.Background(), writeRecipe(t, dir, r)); err == nil {
		t.Fatal("accepted invalid transport")
	}
}
func TestLockedInputOfflineCacheAndTamper(t *testing.T) {
	dir, r := recipeFixture(t)
	cache := filepath.Join(dir, "state")
	if err := os.Mkdir(cache, 0700); err != nil {
		t.Fatal(err)
	}
	source := r.Rootfs.Source
	source.Path = filepath.Join(dir, source.Path)
	input, err := lockInput(context.Background(), cache, "rootfs", source, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(source.Path); err != nil {
		t.Fatal(err)
	}
	if _, err = lockInput(context.Background(), cache, "rootfs", source, false, true); err != nil {
		t.Fatal("offline cache depended on original path", err)
	}
	if err = os.Chmod(input.Path, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(input.Path, []byte("tampered"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = lockInput(context.Background(), cache, "rootfs", source, false, true); err == nil {
		t.Fatal("silently repaired changed cache")
	}
	if _, err = lockInput(context.Background(), cache, "other", Source{URL: "https://example.invalid/locked", SHA256: image.Sum([]byte("missing"))}, true, true); err == nil {
		t.Fatal("offline attempted uncached fetch")
	}
}
