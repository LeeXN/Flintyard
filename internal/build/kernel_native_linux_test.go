package build

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LeeXN/Flintyard/internal/firecracker"
	"github.com/LeeXN/Flintyard/internal/host"
	"github.com/LeeXN/Flintyard/internal/image"
	"github.com/LeeXN/Flintyard/internal/state"
)

// This source fixture exercises make/patch/module plumbing, not Linux. Its
// copied image header and module bytes are explicitly nonbootable test data.
func sourceRecipeFixture(t *testing.T, modules bool, behavior string) (string, Recipe) {
	t.Helper()
	dir, r := recipeFixture(t)
	target, path := "bzImage", "arch/x86/boot/bzImage"
	if r.Architecture == "aarch64" {
		target, path = "Image", "arch/arm64/boot/Image"
	}
	config := "# synthetic kernel config\n# CONFIG_MODULES is not set\n"
	if modules {
		config = "# synthetic kernel config\nCONFIG_MODULES=y\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "config"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	blob, err := os.ReadFile(filepath.Join(dir, "kernel"))
	if err != nil {
		t.Fatal(err)
	}
	command := "cp blob " + path
	if behavior == "config" {
		command += "; echo CONFIG_SURPRISE=y >> .config"
	}
	if behavior == "wrong-arch" {
		command = "printf invalid > " + path
	}
	if behavior == "timeout" {
		command = "sleep 5"
	}
	makefile := fmt.Sprintf(".PHONY: %s kernelrelease modules modules_install\n%s:\n\t@mkdir -p %s\n\t@%s\nkernelrelease:\n\t@cat release.txt\nmodules:\n\t@printf synthetic-module > fixture.ko\nmodules_install:\n\t@mkdir -p $(INSTALL_MOD_PATH)/lib/modules/fixture-second/kernel\n\t@cp fixture.ko $(INSTALL_MOD_PATH)/lib/modules/fixture-second/kernel/fixture.ko\n", target, target, filepath.Dir(path), command)
	if behavior == "modules" {
		makefile += "\t@false\n"
	}
	f, err := os.Create(filepath.Join(dir, "source.tar"))
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(f)
	for name, b := range map[string][]byte{"Makefile": []byte(makefile), "blob": blob, "release.txt": []byte("fixture\n")} {
		if err = tw.WriteHeader(&tar.Header{Name: "source/" + name, Typeflag: tar.TypeReg, Mode: 0644, Size: int64(len(b))}); err != nil {
			t.Fatal(err)
		}
		if _, err = tw.Write(b); err != nil {
			t.Fatal(err)
		}
	}
	if err = tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	sum, _, err := image.Hash(context.Background(), f.Name())
	if err != nil {
		t.Fatal(err)
	}
	r.Kernel = Kernel{Mode: "source", Source: Source{Path: "source.tar", SHA256: sum}, StripComponents: 1, ConfigFile: "config", ConfigSHA256: image.Sum([]byte(config)), Origin: "user-provided", ImageTarget: target, Jobs: 2, TimeoutSeconds: 20, ExpectedRelease: "fixture-second", Patches: []Patch{}}
	for i, change := range [][2]string{{"fixture", "fixture-first"}, {"fixture-first", "fixture-second"}} {
		data := []byte(fmt.Sprintf("--- a/release.txt\n+++ b/release.txt\n@@ -1 +1 @@\n-%s\n+%s\n", change[0], change[1]))
		name := fmt.Sprintf("patch-%d", i)
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
		r.Kernel.Patches = append(r.Kernel.Patches, Patch{Path: name, SHA256: image.Sum(data)})
	}
	return dir, r
}

func TestNativeBuildSourceKernelOrderedPatchesModulesInitrdAndOfflineCache(t *testing.T) {
	nativeBuildOnly(t)
	dir, r := sourceRecipeFixture(t, true, "")
	r.Boot.VirtioTransport = "pci"
	r.GeneratedInitrd = "/boot/fixture-initrd"
	r.Steps = []Step{{Argv: []string{"/fixture-step", "initrd"}, TimeoutSeconds: 10}}
	file := writeRecipe(t, dir, r)
	request := Request{StateDir: filepath.Join(dir, "state"), RecipeFile: file, WorkerPath: os.Getenv("FY_BUILD_WORKER"), Offline: true}
	result, err := Build(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	m := result.Image.Manifest
	p := m.Provenance.KernelBuild
	if p == nil || p.Compiler == "" || p.Make == "" || p.Linker == "" || !p.ConfigUnchanged || p.ConfigSHA256 != r.Kernel.ConfigSHA256 || len(p.Patches) != 2 || p.Patches[0] != r.Kernel.Patches[0].SHA256 || p.Patches[1] != r.Kernel.Patches[1].SHA256 || len(p.Commands) != 7 {
		t.Fatalf("missing source provenance: %+v", p)
	}
	if m.Kernel.Release != "fixture-second" || m.Kernel.Modules != "embedded" || m.Boot.Initrd == "" || m.Boot.VirtioTransport != "pci" || result.Image.Validation != "not_run" {
		t.Fatal(result)
	}
	disk := filepath.Join(dir, "state/images/sha256-"+strings.TrimPrefix(result.Image.ID, "sha256:"), "payload/rootfs.raw")
	for name, want := range map[string]string{"/lib/modules/fixture-second/kernel/fixture.ko": "synthetic-module", "/lib/modules/fixture-second/modules.dep": "kernel/fixture.ko:\n", "/boot/fixture-initrd": "synthetic initrd fixture\n", "/boot/config-fixture-second": "# synthetic kernel config\nCONFIG_MODULES=y\n"} {
		out, err := host.Run(context.Background(), host.CommandRequest{Name: "debugfs", Args: []string{"-R", "cat " + name, disk}})
		if err != nil || !strings.HasSuffix(out.Output, want) {
			t.Fatal(name, out, err)
		}
	}
	if err := os.Rename(filepath.Join(dir, "out/image.fyimg"), filepath.Join(dir, "portable.fyimg")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"source.tar", "config", "patch-0", "patch-1", "root.tar"} {
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	again, err := Build(context.Background(), request)
	if err != nil || !again.CacheHit || again.Image.ID != result.Image.ID {
		t.Fatal(again, err)
	}
	imported, err := image.Import(context.Background(), filepath.Join(dir, "other-state"), filepath.Join(dir, "portable.fyimg"), result.Image.PackageSHA256, image.Limits{})
	if err != nil || imported.ID != result.Image.ID || imported.Manifest.Provenance.KernelBuild == nil || imported.Manifest.Boot.VirtioTransport != "pci" {
		t.Fatal(imported, err)
	}
}

func TestNativeBuildSourceKernelRejectsConfigurationReleasePatchAndModuleFailures(t *testing.T) {
	nativeBuildOnly(t)
	for _, tc := range []struct {
		name, behavior, want string
		modules              bool
	}{
		{"config", "config", "normalization is not authorized", false},
		{"release", "", "differs from expected", false},
		{"patch-order", "", "kernel patch 0", false},
		{"modules", "modules", "kernel command make", true},
		{"wrong-arch", "wrong-arch", "kernel", false},
		{"timeout", "timeout", "deadline exceeded", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, r := sourceRecipeFixture(t, tc.modules, tc.behavior)
			if tc.name == "release" {
				r.Kernel.ExpectedRelease = "wrong-release"
			}
			if tc.name == "timeout" {
				r.Kernel.TimeoutSeconds = 1
			}
			if tc.name == "patch-order" {
				r.Kernel.Patches[0], r.Kernel.Patches[1] = r.Kernel.Patches[1], r.Kernel.Patches[0]
			}
			result, err := Build(context.Background(), Request{StateDir: filepath.Join(dir, "state"), RecipeFile: writeRecipe(t, dir, r), WorkerPath: os.Getenv("FY_BUILD_WORKER"), Offline: true})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatal(err)
			}
			if tc.name == "timeout" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal("kernel timeout lost typed cause", err)
			}
			var identity firecracker.Identity
			if err := state.Read(filepath.Join(dir, "state/builds", result.BuildID, "worker-identity.json"), &identity); err != nil {
				t.Fatal(err)
			}
			if phase, err := firecracker.ProcessState(context.Background(), identity); err != nil || phase != "exited" {
				t.Fatal("source worker survived failure", phase, err)
			}
			if _, err := os.Stat(filepath.Join(dir, "out/image.fyimg")); !os.IsNotExist(err) {
				t.Fatal("published invalid source build", err)
			}
			if tc.name == "config" {
				b, err := os.ReadFile(filepath.Join(dir, "state/builds", result.BuildID, "kernel-config-diff.json"))
				if err != nil || !strings.Contains(string(b), "CONFIG_SURPRISE=y") {
					t.Fatal("configuration difference was lost", string(b), err)
				}
			}
		})
	}
}
