package image

import (
	"debug/elf"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

var ErrKernelIncompatible = errors.New("kernel incompatible")

// ValidateKernel checks boot headers without interpreting the root filesystem
// or claiming that the complete kernel/initrd combination can boot.
func ValidateKernel(kernel io.ReaderAt, architecture string) error {
	var header [4096]byte
	n, err := kernel.ReadAt(header[:], 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if n >= 4 && string(header[:4]) == "\x7fELF" {
		parsed, err := elf.NewFile(kernel)
		if err != nil {
			return fmt.Errorf("%w: invalid ELF header: %w", ErrKernelIncompatible, err)
		}
		want := elf.EM_X86_64
		if architecture == "aarch64" {
			want = elf.EM_AARCH64
		}
		if parsed.Class != elf.ELFCLASS64 || parsed.Machine != want {
			return fmt.Errorf("%w: ELF machine %s does not match %s", ErrKernelIncompatible, parsed.Machine, architecture)
		}
		return nil
	}
	if architecture == "aarch64" && n >= 64 && binary.LittleEndian.Uint32(header[56:60]) == 0x644d5241 {
		return nil
	}
	if architecture == "x86_64" && n >= 0x238 && string(header[0x202:0x206]) == "HdrS" &&
		header[0x1fe] == 0x55 && header[0x1ff] == 0xaa && binary.LittleEndian.Uint16(header[0x236:0x238])&1 != 0 {
		return nil
	}
	return fmt.Errorf("%w: unrecognized boot format for %s", ErrKernelIncompatible, architecture)
}
