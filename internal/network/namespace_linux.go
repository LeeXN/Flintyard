package network

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/LeeXN/Flintyard/internal/host"
	"golang.org/x/sys/unix"
)

// Older tmpfs kernels do not support user.* xattrs, while trusted.* works on
// root-owned tmpfs and is only writable with the necessary host privilege.
const namespaceOwnerXattr = "trusted.flintyard.owner"
const previousNamespaceOwnerXattr = "user.flintyard.owner"

// newNamespace performs thread-local netns operations on a dedicated locked
// thread. No caller callback runs there. Restore must succeed before unlocking;
// otherwise the goroutine exits locked, causing Go to discard that OS thread.
// The namespace is marked BEFORE it is made persistent. A crash before the bind
// mount closes its last fd; a crash after it leaves a recoverable kernel marker.
func newNamespace(ctx context.Context, owner string) (*os.File, error) {
	type result struct {
		file *os.File
		err  error
	}
	done := make(chan result, 1)
	go func() {
		runtime.LockOSThread()
		old, err := unix.Open("/proc/thread-self/ns/net", unix.O_RDONLY|unix.O_CLOEXEC, 0)
		if err != nil {
			runtime.UnlockOSThread()
			done <- result{err: err}
			return
		}
		defer unix.Close(old)
		var ns *os.File
		if err = unix.Unshare(unix.CLONE_NEWNET); err == nil {
			_, err = host.Run(ctx, host.CommandRequest{Name: "ip", Args: []string{"link", "set", "dev", "lo", "alias", owner}})
			if err == nil {
				for _, name := range []string{"all", "default"} {
					err = os.WriteFile("/proc/sys/net/ipv6/conf/"+name+"/disable_ipv6", []byte("1\n"), 0)
					if os.IsNotExist(err) {
						err = nil
					}
					if err != nil {
						break
					}
				}
			}
			if err == nil {
				ns, err = os.Open("/proc/thread-self/ns/net")
			}
		}
		restoreErr := unix.Setns(old, unix.CLONE_NEWNET)
		if restoreErr == nil {
			runtime.UnlockOSThread()
		}
		err = errors.Join(err, restoreErr)
		if err != nil && ns != nil {
			_ = ns.Close()
			ns = nil
		}
		done <- result{ns, err}
	}()
	r := <-done
	return r.file, r.err
}

func createNamespace(ctx context.Context, path, owner string) error {
	ns, err := newNamespace(ctx, owner)
	if err != nil {
		return err
	}
	defer ns.Close()
	if err = publishNamespacePlaceholder(path, owner); err != nil {
		return err
	}
	// The caller's journal records this intent. Failure after publishing retains
	// a marked placeholder for the same ownership-checked compensation path.
	return unix.Mount("/proc/self/fd/"+strconv.Itoa(int(ns.Fd())), path, "", unix.MS_BIND, "")
}

func publishNamespacePlaceholder(path, owner string) error {
	// O_TMPFILE plus owner xattr plus linkat publishes the placeholder already
	// marked. There is no unowned-file window before mounting nsfs over it.
	fd, err := unix.Open(filepath.Dir(path), unix.O_TMPFILE|unix.O_RDWR|unix.O_CLOEXEC, 0600)
	if err != nil {
		return fmt.Errorf("create owned netns handle: %w", err)
	}
	defer unix.Close(fd)
	if err = unix.Fsetxattr(fd, namespaceOwnerXattr, []byte(owner), 0); err != nil {
		return fmt.Errorf("mark owned netns handle: %w", err)
	}
	if err = unix.Linkat(fd, "", unix.AT_FDCWD, path, unix.AT_EMPTY_PATH); err != nil {
		return err
	}
	return nil
}

func verifyNamespacePlaceholder(fd int, owner string) error {
	matched := false
	for _, key := range []string{namespaceOwnerXattr, previousNamespaceOwnerXattr} {
		value := make([]byte, 256)
		n, err := unix.Fgetxattr(fd, key, value)
		if errors.Is(err, unix.ENODATA) || errors.Is(err, unix.EOPNOTSUPP) {
			continue
		}
		if err != nil {
			return fmt.Errorf("read netns placeholder ownership: %w", err)
		}
		if string(value[:n]) != owner {
			return fmt.Errorf("netns placeholder ownership mismatch: %w", ErrOwnership)
		}
		matched = true
	}
	if !matched {
		return fmt.Errorf("netns placeholder ownership missing: %w", ErrOwnership)
	}
	return nil
}

type namespaceInfo struct {
	Exists, Mounted bool
	Device, Inode   uint64
}

func inspectNamespace(ctx context.Context, path, name, owner string) (namespaceInfo, error) {
	var info namespaceInfo
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if errors.Is(err, unix.ENOENT) {
		return info, nil
	}
	if err != nil {
		return info, err
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	var fs unix.Statfs_t
	if err = unix.Fstat(fd, &st); err != nil {
		return info, err
	}
	if err = unix.Fstatfs(fd, &fs); err != nil {
		return info, err
	}
	info = namespaceInfo{Exists: true, Mounted: fs.Type == unix.NSFS_MAGIC, Device: uint64(st.Dev), Inode: st.Ino}
	if info.Mounted {
		link, err := findLink(ctx, name, "lo")
		if err != nil {
			return info, err
		}
		if link == nil || link.Alias != owner {
			return info, fmt.Errorf("%w: netns kernel ownership mismatch", ErrOwnership)
		}
	} else {
		if st.Mode&unix.S_IFMT != unix.S_IFREG {
			return info, ErrOwnership
		}
		if err = verifyNamespacePlaceholder(fd, owner); err != nil {
			return info, err
		}
	}
	return info, nil
}

func namespaceBusy(ctx context.Context, inode uint64) error {
	if inode == 0 {
		return nil
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return err
	}
	want := "net:[" + strconv.FormatUint(inode, 10) + "]"
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := strconv.Atoi(entry.Name()); err != nil {
			continue
		}
		tasks, err := os.ReadDir(filepath.Join("/proc", entry.Name(), "task"))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("cannot establish namespace process absence: %w", err)
		}
		for _, task := range tasks {
			link, err := os.Readlink(filepath.Join("/proc", entry.Name(), "task", task.Name(), "ns/net"))
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return fmt.Errorf("cannot establish namespace thread absence: %w", err)
			}
			if link == want {
				return fmt.Errorf("namespace is still in use by PID %s thread %s", entry.Name(), task.Name())
			}
		}
	}
	return nil
}

func disableHostIPv6(name string) error {
	// Names are derived and validated, never supplied as sysctl path fragments.
	if strings.ContainsAny(name, "/.") {
		return ErrOwnership
	}
	err := os.WriteFile("/proc/sys/net/ipv6/conf/"+name+"/disable_ipv6", []byte("1\n"), 0)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
