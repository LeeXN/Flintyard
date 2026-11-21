package host

import (
	"context"
	"debug/elf"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestBinaryArchitectureRejectedBeforeExecution(t *testing.T) {
	data, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	machine := uint16(elf.EM_AARCH64)
	if Architecture() == "aarch64" {
		machine = uint16(elf.EM_X86_64)
	}
	binary.LittleEndian.PutUint16(data[18:20], machine)
	path := filepath.Join(t.TempDir(), "wrong-architecture")
	if err = os.WriteFile(path, data, 0700); err != nil {
		t.Fatal(err)
	}
	_, err = InspectBinary(context.Background(), path, "Firecracker")
	var be *BinaryError
	if !errors.As(err, &be) || be.Code != "E_ARCH_MISMATCH" {
		t.Fatal(err)
	}
}
func TestKVMRequirementsAreArchitectureSpecific(t *testing.T) {
	arm, err := requiredKVMCapabilities("aarch64")
	if err != nil {
		t.Fatal(err)
	}
	x86, err := requiredKVMCapabilities("x86_64")
	if err != nil {
		t.Fatal(err)
	}
	has := func(list []uintptr, value uintptr) bool {
		for _, n := range list {
			if n == value {
				return true
			}
		}
		return false
	}
	if has(arm, 0) || !has(arm, 89) || !has(arm, 102) || !has(x86, 0) || has(x86, 102) {
		t.Fatal("architecture capability requirements mixed", arm, x86)
	}
}
func TestMountInfoEscapesAndOwnershipScope(t *testing.T) {
	m, err := ParseMounts("24 1 0:1 / /owned\\040dir/root ro,relatime - ext4 /dev/example rw\n")
	if err != nil || len(m) != 1 || m[0].Path != "/owned dir/root" || !m[0].ReadOnly {
		t.Fatal(m, err)
	}
	if Within("/owned", "/owned-other/file") || !Within("/owned", "/owned/child") {
		t.Fatal("scope check used prefix matching")
	}
	m, err = ParseMounts("25 1 0:4 mnt:[1234] /run/held-ns rw - nsfs nsfs rw\n")
	if err != nil || len(m) != 1 || m[0].Root != "mnt:[1234]" {
		t.Fatal("valid nsfs root rejected", m, err)
	}
}
