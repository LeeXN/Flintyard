// Package state provides private, locked, crash-consistent local storage.
package state

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

func UUID() string {
	var b [16]byte
	if _, e := rand.Read(b[:]); e != nil {
		panic(e)
	}
	b[6] = b[6]&15 | 64
	b[8] = b[8]&63 | 128
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
}
func PrivateDir(p string) error {
	p, e := filepath.Abs(p)
	if e != nil {
		return e
	}
	// Inspect every ancestor, including existing leaves, before creating anything.
	for q := p; ; q = filepath.Dir(q) {
		st, e := os.Lstat(q)
		if e == nil {
			if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("unsafe state path %s", q)
			}
			if os.Geteuid() == 0 {
				u := st.Sys().(*syscall.Stat_t)
				if u.Uid != 0 || st.Mode().Perm()&0022 != 0 {
					return fmt.Errorf("root state ancestor controlled by another user: %s", q)
				}
			}
		} else if !os.IsNotExist(e) {
			return e
		}
		if q == "/" {
			break
		}
	}
	if e = os.MkdirAll(p, 0700); e != nil {
		return e
	}
	st, e := os.Stat(p)
	if e != nil {
		return e
	}
	if st.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("state directory must be private (0700): %s", p)
	}
	return nil
}
func Lock(p string) (func(), error) {
	f, e := os.OpenFile(p, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if e != nil {
		return nil, e
	}
	st, e := f.Stat()
	if e != nil || !st.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("unsafe lock file")
	}
	if e = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		f.Close()
		return nil, fmt.Errorf("lock held: %w", e)
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }, nil
}

// LockContext serializes short global allocations without making concurrent
// operations fail just because another environment is reserving resources.
func LockContext(ctx context.Context, p string) (func(), error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		unlock, err := Lock(p)
		if err == nil {
			return unlock, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}
func Atomic(p string, b []byte, mode os.FileMode) error {
	f, e := os.CreateTemp(filepath.Dir(p), ".commit-")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if e = f.Chmod(mode); e != nil {
		return e
	}
	if _, e = f.Write(b); e != nil {
		return e
	}
	if e = f.Sync(); e != nil {
		return e
	}
	if e = f.Close(); e != nil {
		return e
	}
	if e = os.Rename(f.Name(), p); e != nil {
		return e
	}
	return SyncDir(filepath.Dir(p))
}
func SyncDir(p string) error {
	f, e := os.Open(p)
	if e != nil {
		return e
	}
	defer f.Close()
	return f.Sync()
}
func Write(p string, v any) error {
	b, e := json.MarshalIndent(v, "", "  ")
	if e != nil {
		return e
	}
	return Atomic(p, append(b, '\n'), 0600)
}
