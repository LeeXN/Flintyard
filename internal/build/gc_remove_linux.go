package build

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

func fdMountID(fd int) (uint64, error) {
	var st unix.Statx_t
	if err := unix.Statx(fd, "", unix.AT_EMPTY_PATH|unix.AT_SYMLINK_NOFOLLOW, unix.STATX_MNT_ID, &st); err != nil {
		return 0, err
	}
	if st.Mask&unix.STATX_MNT_ID == 0 || st.Mnt_id == 0 {
		return 0, fmt.Errorf("mount identity is unavailable for build cleanup")
	}
	return st.Mnt_id, nil
}

// Directory fds pin the selected tree. Each opened directory must stay on the
// build's mount ID, including same-device bind mounts. Symlinks are unlinked
// without following them; no recursive path operation can enter a new mount.
func removeBuildTree(ctx context.Context, dir, name string) error {
	if name != "root" && name != "payload" && name != "kernel-source" && name != "module-stage" {
		return fmt.Errorf("unrecognized build scratch directory")
	}
	base, err := os.OpenFile(dir, os.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer base.Close()
	allowed, err := fdMountID(int(base.Fd()))
	if err != nil {
		return err
	}
	return removeBuildEntry(ctx, int(base.Fd()), name, allowed)
}
func removeBuildEntry(ctx context.Context, parent int, name string, allowed uint64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	fd, err := unix.Openat(parent, name, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	mount, err := fdMountID(fd)
	var st unix.Stat_t
	if err == nil {
		err = unix.Fstat(fd, &st)
	}
	unix.Close(fd)
	if err != nil {
		return err
	}
	if mount != allowed {
		return fmt.Errorf("refusing build cleanup across a mount boundary")
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		return unix.Unlinkat(parent, name, 0)
	}
	fd, err = unix.Openat(parent, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	var opened unix.Stat_t
	if err = unix.Fstat(fd, &opened); err != nil {
		return err
	}
	if opened.Dev != st.Dev || opened.Ino != st.Ino {
		return fmt.Errorf("build directory identity changed while opening")
	}
	mount, err = fdMountID(fd)
	if err != nil {
		return err
	}
	if mount != allowed {
		return fmt.Errorf("build directory was replaced by a mount")
	}
	for {
		entries, err := f.ReadDir(128)
		for _, entry := range entries {
			if err := removeBuildEntry(ctx, fd, entry.Name(), allowed); err != nil {
				return err
			}
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		if len(entries) == 0 {
			break
		}
	}
	return unix.Unlinkat(parent, name, unix.AT_REMOVEDIR)
}
