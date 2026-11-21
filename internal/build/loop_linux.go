package build

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"

	"github.com/LeeXN/Flintyard/internal/host"
	"github.com/LeeXN/Flintyard/internal/state"
	"golang.org/x/sys/unix"
)

func privateDevices() error {
	if err := unix.Mount("tmpfs", "/dev", "tmpfs", unix.MS_NOSUID, "mode=0755,size=16m"); err != nil {
		return err
	}
	for _, device := range []struct {
		name         string
		major, minor uint32
	}{{"null", 1, 3}, {"zero", 1, 5}, {"random", 1, 8}, {"urandom", 1, 9}, {"loop-control", 10, 237}} {
		if err := unix.Mknod("/dev/"+device.name, unix.S_IFCHR|0600, int(unix.Mkdev(device.major, device.minor))); err != nil {
			return err
		}
	}
	if err := os.Symlink("/proc/self/fd", "/dev/fd"); err != nil {
		return err
	}
	for _, pair := range [][2]string{{"stdin", "0"}, {"stdout", "1"}, {"stderr", "2"}} {
		if err := os.Symlink("/proc/self/fd/"+pair[1], "/dev/"+pair[0]); err != nil {
			return err
		}
	}
	return nil
}
func mountRaw(ctx context.Context, disk, target string, j *state.Journal) (cleanup func() error, err error) {
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	control, err := unix.Open("/dev/loop-control", unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("raw customization needs kernel loop support: %w", err)
	}
	defer unix.Close(control)
	number, err := unix.IoctlRetInt(control, unix.LOOP_CTL_GET_FREE)
	if err != nil {
		return nil, err
	}
	device := "/dev/loop" + strconv.Itoa(number)
	if err = unix.Mknod(device, unix.S_IFBLK|0600, int(unix.Mkdev(7, uint32(number)))); err != nil {
		return nil, err
	}
	loop, err := unix.Open(device, unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	backing, err := unix.Open(disk, unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		unix.Close(loop)
		return nil, err
	}
	defer unix.Close(backing)
	var st unix.Stat_t
	if err = unix.Fstat(backing, &st); err != nil {
		unix.Close(loop)
		return nil, err
	}
	meta := map[string]string{"device": device, "disk": disk, "backing_device": strconv.FormatUint(uint64(st.Dev), 10), "backing_inode": strconv.FormatUint(st.Ino, 10), "owner": j.OwnerID}
	id, err := j.IntentIdentity("build-loop", meta)
	if err != nil {
		unix.Close(loop)
		return nil, err
	}
	config := unix.LoopConfig{Fd: uint32(backing), Info: unix.LoopInfo64{Flags: unix.LO_FLAGS_AUTOCLEAR}}
	copy(config.Info.File_name[:], []byte("flintyard-build:"+j.OwnerID))
	if err = unix.IoctlLoopConfigure(loop, &config); err != nil {
		unix.Close(loop)
		return nil, err
	}
	configured := true
	mounted := false
	closed := false
	cleanup = func() error {
		if closed {
			return nil
		}
		if mounted {
			if err := unmountRaw(target, j); err != nil {
				return err
			}
			mounted = false
		}
		if configured {
			info, e := unix.IoctlLoopGetStatus64(loop)
			if e == nil {
				if info.Device != uint64(st.Dev) || info.Inode != st.Ino {
					return fmt.Errorf("loop backing identity changed; refusing detach")
				}
			} else if !errors.Is(e, unix.ENXIO) {
				return e
			}
			// AUTOCLEAR detaches on the final close, not by a guessed loop name.
			if e = unix.Close(loop); e != nil {
				return e
			}
			configured = false
		}
		closed = true
		for _, resource := range j.Resources {
			if resource.ID == id {
				if resource.State == "intent" {
					if e := j.Done(id, meta); e != nil {
						return e
					}
				}
				return j.Released(id)
			}
		}
		return nil
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, cleanup())
		}
	}()
	if err = j.Done(id, meta); err != nil {
		return cleanup, err
	}
	mountID, err := j.IntentIdentity("build-mount", map[string]string{"target": target, "device": device, "owner": j.OwnerID})
	if err != nil {
		return cleanup, err
	}
	if err = unix.Mount(device, target, "ext4", unix.MS_NOSUID|unix.MS_NODEV, ""); err != nil {
		return cleanup, err
	}
	mounted = true
	mount, err := host.MountAt(target)
	if err != nil || mount == nil {
		return cleanup, errors.Join(err, fmt.Errorf("raw root mount disappeared"))
	}
	if err = j.Done(mountID, map[string]string{"target": target, "device": device, "mount_id": strconv.Itoa(mount.ID), "owner": j.OwnerID}); err != nil {
		return cleanup, err
	}
	return cleanup, nil
}
func unmountRaw(target string, j *state.Journal) error {
	for i := len(j.Resources) - 1; i >= 0; i-- {
		r := j.Resources[i]
		if r.Kind != "build-mount" || r.State == "released" || r.Identity["target"] != target {
			continue
		}
		if r.Identity["owner"] != j.OwnerID {
			return fmt.Errorf("build mount owner changed")
		}
		mount, err := host.MountAt(target)
		if err != nil {
			return err
		}
		if mount != nil {
			if r.State == "done" && r.Identity["mount_id"] != strconv.Itoa(mount.ID) {
				return fmt.Errorf("build mount identity changed")
			}
			if err = unix.Unmount(target, 0); err != nil {
				return err
			}
		}
		if r.State == "intent" {
			if err = j.Absent(r.ID); err != nil {
				return err
			}
		} else if err = j.Released(r.ID); err != nil {
			return err
		}
	}
	return nil
}
