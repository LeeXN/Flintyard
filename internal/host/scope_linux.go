package host

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// Inspection is deliberately read-only. An active loop or a namespace retained
// by another process is not detached by guesswork. AUTOCLEAR releases normal
// worker loops when their final references disappear; gc confirms that fact.
type ScopeFileIdentity struct{ Device, Inode uint64 }

func ScopeFileID(st os.FileInfo) ScopeFileIdentity {
	s := st.Sys().(*syscall.Stat_t)
	return ScopeFileIdentity{uint64(s.Dev), s.Ino}
}

func CheckScopeIdle(ctx context.Context, dir string, known map[ScopeFileIdentity]bool) error {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return err
	}
	namespaces := map[string]bool{}
	heldNamespaces := map[string]bool{}
	scopeDevices := make(map[string]bool)
	for identity := range known {
		scopeDevices[fmt.Sprintf("%d:%d", unix.Major(identity.Device), unix.Minor(identity.Device))] = true
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := strconv.Atoi(entry.Name()); err != nil {
			continue
		}
		base := filepath.Join("/proc", entry.Name())
		for _, name := range []string{"root", "cwd", "exe"} {
			path, err := os.Readlink(filepath.Join(base, name))
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return fmt.Errorf("cannot establish build process absence: %w", err)
			}
			if Within(dir, path) {
				return fmt.Errorf("owned scope still referenced by PID %s %s", entry.Name(), name)
			}
			if err := scopeReferenceIdentity(filepath.Join(base, name), known); err != nil {
				return err
			}
		}
		fds, err := os.ReadDir(filepath.Join(base, "fd"))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("cannot inspect build file references: %w", err)
		}
		for _, fd := range fds {
			path, err := os.Readlink(filepath.Join(base, "fd", fd.Name()))
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return fmt.Errorf("cannot inspect build descriptor: %w", err)
			}
			if strings.HasPrefix(path, "/") && Within(dir, path) {
				return fmt.Errorf("owned scope still open by PID %s fd %s", entry.Name(), fd.Name())
			}
			if strings.HasPrefix(path, "mnt:[") {
				heldNamespaces[path] = true
			}
			if err := scopeReferenceIdentity(filepath.Join(base, "fd", fd.Name()), known); err != nil {
				return err
			}
		}
		// Processes can unshare mount namespaces on individual threads.
		tasks, err := os.ReadDir(filepath.Join(base, "task"))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		for _, task := range tasks {
			thread := filepath.Join(base, "task", task.Name())
			if err := scopeThreadReferences(ctx, thread, dir, heldNamespaces, known); err != nil {
				return err
			}
			ns, err := os.Readlink(filepath.Join(thread, "ns/mnt"))
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return fmt.Errorf("cannot inspect build mount namespace: %w", err)
			}
			if namespaces[ns] {
				continue
			}
			b, err := os.ReadFile(filepath.Join(thread, "mountinfo"))
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return fmt.Errorf("cannot inspect build namespace mounts: %w", err)
			}
			mounts, err := ParseMounts(string(b))
			if err != nil {
				return err
			}
			for _, mount := range mounts {
				if Within(dir, mount.Path) {
					return fmt.Errorf("build mount %s remains in namespace %s", mount.Path, ns)
				}
				// mountinfo exposes the filesystem device ID. A bind alias of
				// an owned inode must be on that device. Unrelated mounts,
				// including private FUSE mounts, need no path lookup.
				if !scopeDevices[mount.Device] {
					continue
				}
				if err := scopeReferenceIdentity(filepath.Join(thread, "root", strings.TrimPrefix(mount.Path, "/")), known); err != nil {
					return fmt.Errorf("mounted build alias: %w", err)
				}
			}
			namespaces[ns] = true
		}
	}
	for namespace := range heldNamespaces {
		if !namespaces[namespace] {
			return fmt.Errorf("unobserved retained mount namespace %s prevents scope recovery", namespace)
		}
	}
	return noScopeLoops(ctx, dir, known)
}

func noScopeLoops(ctx context.Context, dir string, known map[ScopeFileIdentity]bool) error {
	entries, err := os.ReadDir("/sys/block")
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		// The sysfs loop directory establishes device type; no name prefix
		// grants deletion authority, and this function never modifies devices.
		path := filepath.Join("/sys/block", entry.Name(), "loop", "backing_file")
		b, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		backing := strings.TrimSuffix(string(b), "\n")
		if backing == "" {
			continue
		}
		// loop/backing_file is file_path() plus one newline, not mountinfo's
		// escaped format. Preserve literal spaces, newlines and backslashes.
		if !filepath.IsAbs(backing) {
			return fmt.Errorf("cannot resolve active loop backing path")
		}
		if Within(dir, backing) {
			return fmt.Errorf("active loop %s still references build scratch", entry.Name())
		}
		// Detect external hard links to any ordinary scratch backing inode.
		st, err := os.Stat(backing)
		if err != nil {
			return fmt.Errorf("cannot establish loop backing independence for %s: %w", entry.Name(), err)
		}
		if known[ScopeFileID(st)] {
			return fmt.Errorf("active loop %s uses a owned scope inode", entry.Name())
		}
	}
	return nil
}

func scopeThreadReferences(ctx context.Context, thread, dir string, held map[string]bool, known map[ScopeFileIdentity]bool) error {
	for _, name := range []string{"root", "cwd", "exe"} {
		path, err := os.Readlink(filepath.Join(thread, name))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("cannot inspect build thread reference: %w", err)
		}
		if Within(dir, path) {
			return fmt.Errorf("owned scope still referenced by thread %s %s", thread, name)
		}
		if err := scopeReferenceIdentity(filepath.Join(thread, name), known); err != nil {
			return err
		}
	}
	fds, err := os.ReadDir(filepath.Join(thread, "fd"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, fd := range fds {
		if err := ctx.Err(); err != nil {
			return err
		}
		path, err := os.Readlink(filepath.Join(thread, "fd", fd.Name()))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if strings.HasPrefix(path, "/") && Within(dir, path) {
			return fmt.Errorf("owned scope is still open by thread %s fd %s", thread, fd.Name())
		}
		if strings.HasPrefix(path, "mnt:[") {
			held[path] = true
		}
		if err := scopeReferenceIdentity(filepath.Join(thread, "fd", fd.Name()), known); err != nil {
			return err
		}
	}
	return nil
}

func scopeReferenceIdentity(path string, known map[ScopeFileIdentity]bool) error {
	if len(known) == 0 {
		return nil
	}
	st, err := os.Stat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("cannot inspect scope reference identity: %w", err)
	}
	if known[ScopeFileID(st)] {
		return fmt.Errorf("build inode still referenced through %s", path)
	}
	return nil
}
