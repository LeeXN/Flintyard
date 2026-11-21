package host

import (
	"context"
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const FirecrackerVersion = "1.17.0"

type Binary struct {
	Path         string `json:"path"`
	Name         string `json:"name"`
	Version      string `json:"version"`
	Architecture string `json:"architecture"`
	Device       uint64 `json:"device"`
	Inode        uint64 `json:"inode"`
	SHA256       string `json:"sha256"`
}
type BinaryError struct {
	Code  string
	Cause error
}

func (e *BinaryError) Error() string { return e.Cause.Error() }
func (e *BinaryError) Unwrap() error { return e.Cause }

// TrustedPath rejects root execution through files or ancestors another user
// can replace. Symlinks are resolved first; the returned canonical path is the
// only path subsequently executed and its inode is rechecked before launch.
func TrustedPath(path string) (string, error) {
	p, err := exec.LookPath(path)
	if err != nil {
		return "", err
	}
	p, err = filepath.EvalSymlinks(p)
	if err != nil {
		return "", err
	}
	p, err = filepath.Abs(p)
	if err != nil {
		return "", err
	}
	for q := p; ; q = filepath.Dir(q) {
		st, err := os.Lstat(q)
		if err != nil {
			return "", err
		}
		if st.Mode()&os.ModeSymlink != 0 || q != p && !st.IsDir() {
			return "", fmt.Errorf("unsafe binary path %s", q)
		}
		s := st.Sys().(*syscall.Stat_t)
		if os.Geteuid() == 0 && (s.Uid != 0 || st.Mode().Perm()&0022 != 0) {
			return "", fmt.Errorf("binary path controlled by another user: %s", q)
		}
		if q == "/" {
			break
		}
	}
	return p, nil
}
func InspectBinary(ctx context.Context, path, name string) (Binary, error) {
	var b Binary
	fail := func(code string, err error) (Binary, error) { return Binary{}, &BinaryError{code, err} }
	p, err := TrustedPath(path)
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) || os.IsNotExist(err) {
			return fail("E_DEPENDENCY", err)
		}
		return fail("E_PERMISSION", err)
	}
	f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return fail("E_DEPENDENCY", err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return fail("E_DEPENDENCY", err)
	}
	if !st.Mode().IsRegular() || st.Mode().Perm()&0111 == 0 || st.Mode()&(os.ModeSetuid|os.ModeSetgid) != 0 {
		return fail("E_PERMISSION", fmt.Errorf("binary must be an ordinary executable without set-id bits"))
	}
	ef, err := elf.NewFile(f)
	if err != nil {
		return fail("E_ARCH_MISMATCH", fmt.Errorf("invalid ELF executable: %w", err))
	}
	architecture := ""
	switch ef.Machine {
	case elf.EM_X86_64:
		architecture = "x86_64"
	case elf.EM_AARCH64:
		architecture = "aarch64"
	}
	if architecture != Architecture() || ef.Class != elf.ELFCLASS64 {
		return fail("E_ARCH_MISMATCH", fmt.Errorf("%s is %s, host is %s", name, ef.Machine, Architecture()))
	}
	for _, prog := range ef.Progs {
		if prog.Type == elf.PT_INTERP {
			return fail("E_VERSION_UNSUPPORTED", fmt.Errorf("official static %s binary required; dynamic ELF is unsupported", name))
		}
	}
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		return fail("E_DEPENDENCY", err)
	}
	h := sha256.New()
	buf := make([]byte, 128<<10)
	for {
		if err = ctx.Err(); err != nil {
			return Binary{}, err
		}
		n, e := f.Read(buf)
		if n > 0 {
			_, _ = h.Write(buf[:n])
		}
		if e == io.EOF {
			break
		}
		if e != nil {
			return fail("E_DEPENDENCY", e)
		}
	}
	s := st.Sys().(*syscall.Stat_t)
	b = Binary{Path: p, Name: name, Version: FirecrackerVersion, Architecture: architecture, Device: uint64(s.Dev), Inode: s.Ino, SHA256: hex.EncodeToString(h.Sum(nil))}
	r, err := Run(ctx, CommandRequest{Name: p, Args: []string{"--version"}, Timeout: 5 * time.Second})
	if err != nil {
		return fail("E_VERSION_UNSUPPORTED", err)
	}
	first := strings.TrimSpace(strings.SplitN(strings.TrimSpace(r.Output), "\n", 2)[0])
	if r.Truncated || first != name+" v"+FirecrackerVersion {
		return fail("E_VERSION_UNSUPPORTED", fmt.Errorf("%s requires v%s, observed %.256s", name, FirecrackerVersion, first))
	}
	if err = b.Verify(ctx); err != nil {
		return fail("E_PROCESS_IDENTITY", err)
	}
	return b, nil
}
func (b Binary) Verify(ctx context.Context) error {
	f, err := os.OpenFile(b.Path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	s := st.Sys().(*syscall.Stat_t)
	if !st.Mode().IsRegular() || uint64(s.Dev) != b.Device || s.Ino != b.Inode {
		return fmt.Errorf("binary identity changed: %s", b.Path)
	}
	h := sha256.New()
	buf := make([]byte, 128<<10)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, e := f.Read(buf)
		if n > 0 {
			_, _ = h.Write(buf[:n])
		}
		if e == io.EOF {
			break
		}
		if e != nil {
			return e
		}
	}
	if hex.EncodeToString(h.Sum(nil)) != b.SHA256 {
		return fmt.Errorf("binary bytes changed: %s", b.Path)
	}
	return nil
}
