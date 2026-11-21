package firecracker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/LeeXN/Flintyard/internal/host"
	"github.com/LeeXN/Flintyard/internal/image"
	"github.com/LeeXN/Flintyard/internal/state"
	"golang.org/x/sys/unix"
)

const mountOwnerAttribute = "user.flintyard.owner"

type JailRequest struct {
	Owner                                   state.Owner
	NodeDir, RuntimeBase, StateDir, ImageID string
	Firecracker                             host.Binary
	UID, GID                                uint32
	RootDisk                                image.Disk
	Seed                                    string
	Journal                                 *state.Journal
}
type Jail struct {
	RunDir    string `json:"run_dir"`
	Scope     string `json:"scope"`
	Base      string `json:"base"`
	Root      string `json:"root"`
	Socket    string `json:"socket"`
	SerialLog string `json:"serial_log"`
	Logger    string `json:"logger"`
}

func runtimeName(o state.Owner, op string) string {
	h := sha256.Sum256([]byte(o.Tag() + ":" + op))
	return hex.EncodeToString(h[:8])
}

// PrepareJail binds only independently writable node files into the official
// jailer's tree. The cache is never writable or chowned to the VMM account.
func PrepareJail(ctx context.Context, r JailRequest) (Jail, error) {
	var jail Jail
	if err := r.Owner.Validate(); err != nil {
		return jail, err
	}
	if r.Journal == nil || !state.ValidUUID(r.Journal.OperationID) || r.Journal.OwnerID != r.Owner.EnvironmentID || r.Owner.NodeID == "" || r.UID == 0 || r.GID == 0 {
		return jail, fmt.Errorf("invalid jail ownership or credentials")
	}
	if err := state.VerifyOwner(r.NodeDir, r.Owner); err != nil {
		return jail, err
	}
	if !host.Within(r.NodeDir, r.RootDisk.Path) || r.RootDisk.Path == r.NodeDir {
		return jail, fmt.Errorf("root disk outside owned node")
	}
	if r.RuntimeBase == "" {
		r.RuntimeBase = "/run/flintyard"
	}
	if err := state.PrivateDir(r.RuntimeBase); err != nil {
		return jail, err
	}
	run := filepath.Join(r.RuntimeBase, runtimeName(r.Owner, r.Journal.OperationID))
	id, err := r.Journal.IntentIdentity("runtime-dir", map[string]string{"path": run, "owner": r.Owner.Tag(), "node_id": r.Owner.NodeID, "runtime_base": r.RuntimeBase})
	if err != nil {
		return jail, err
	}
	if err = state.OwnedDir(run, r.Owner); err != nil {
		return jail, err
	}
	if err = r.Journal.Done(id, map[string]string{"path": run, "owner": r.Owner.Tag(), "node_id": r.Owner.NodeID, "runtime_base": r.RuntimeBase}); err != nil {
		return jail, err
	}
	// /run may be noexec. Keep the executable jail on the state filesystem;
	// only the short socket directory lives in /run and is bound into /run in
	// the jail. Never change host mount security flags to make execution work.
	scopeParent := filepath.Join(r.NodeDir, "jails")
	if err = state.PrivateDir(scopeParent); err != nil {
		return jail, err
	}
	scope := filepath.Join(scopeParent, r.Journal.OperationID)
	meta := map[string]string{"path": scope, "node_dir": r.NodeDir, "owner": r.Owner.Tag(), "node_id": r.Owner.NodeID}
	scopeID, err := r.Journal.IntentIdentity("jail-dir", meta)
	if err != nil {
		return jail, err
	}
	if err = state.OwnedDir(scope, r.Owner); err != nil {
		return jail, err
	}
	if err = r.Journal.Done(scopeID, meta); err != nil {
		return jail, err
	}
	var fs unix.Statfs_t
	if err = unix.Statfs(scope, &fs); err != nil {
		return jail, err
	}
	if fs.Flags&unix.ST_NOEXEC != 0 {
		return jail, fmt.Errorf("state filesystem is noexec; cannot execute jailed Firecracker")
	}
	base := filepath.Join(scope, "j")
	root := filepath.Join(base, filepath.Base(r.Firecracker.Path), r.Owner.NodeID, "root")
	if err = os.MkdirAll(root, 0700); err != nil {
		return jail, err
	}
	api := filepath.Join(run, "api")
	if err = os.Mkdir(api, 0700); err != nil {
		return jail, err
	}
	if err = os.Chown(api, int(r.UID), int(r.GID)); err != nil {
		return jail, err
	}
	logs := filepath.Join(r.NodeDir, "logs")
	if err = state.PrivateDir(logs); err != nil {
		return jail, err
	}
	jail = Jail{RunDir: run, Scope: scope, Base: base, Root: root, Socket: filepath.Join(api, "api.sock"), SerialLog: filepath.Join(logs, "serial.log"), Logger: "/firecracker.log"}
	manifest, err := image.Cached(ctx, r.StateDir, r.ImageID)
	if err != nil {
		return jail, err
	}
	for _, role := range []string{"kernel", "initrd"} {
		if role == "initrd" && manifest.Manifest.Boot.Initrd == "" {
			continue
		}
		path := filepath.Join(root, role)
		if _, err = image.CopyBootPayload(ctx, r.StateDir, r.ImageID, role, path); err != nil {
			return jail, err
		}
		if err = os.Chown(path, int(r.UID), int(r.GID)); err != nil {
			return jail, err
		}
	}
	if err = chownDisk(r.RootDisk, r.UID, r.GID); err != nil {
		return jail, err
	}
	if err = bindJail(ctx, r, run, r.RootDisk.Path, filepath.Join(root, "rootfs.raw"), false); err != nil {
		return jail, err
	}
	if r.Seed != "" {
		if !host.Within(r.NodeDir, r.Seed) {
			return jail, fmt.Errorf("seed is outside owned node")
		}
		if err = chownRegular(r.Seed, r.UID, r.GID); err != nil {
			return jail, err
		}
		if err = bindJail(ctx, r, run, r.Seed, filepath.Join(root, "seed.raw"), true); err != nil {
			return jail, err
		}
	}
	logpath := filepath.Join(logs, "firecracker.log")
	lf, err := os.OpenFile(logpath, os.O_CREATE|os.O_APPEND|os.O_WRONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0600)
	if err != nil {
		return jail, err
	}
	st, err := lf.Stat()
	if err != nil || !st.Mode().IsRegular() {
		lf.Close()
		return jail, fmt.Errorf("Firecracker log must be ordinary")
	}
	err = lf.Chown(int(r.UID), int(r.GID))
	lf.Close()
	if err != nil {
		return jail, err
	}
	if err = bindJail(ctx, r, run, logpath, filepath.Join(root, "firecracker.log"), false); err != nil {
		return jail, err
	}
	if err = bindJail(ctx, r, run, api, filepath.Join(root, "run"), false); err != nil {
		return jail, err
	}
	return jail, nil
}
func chownRegular(path string, uid, gid uint32) error {
	f, err := os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("expected ordinary node file")
	}
	return f.Chown(int(uid), int(gid))
}
func chownDisk(d image.Disk, uid, gid uint32) error {
	f, err := os.OpenFile(d.Path, os.O_RDWR|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	s := st.Sys().(*syscall.Stat_t)
	if !st.Mode().IsRegular() || uint64(s.Dev) != d.Device || s.Ino != d.Inode {
		return ErrProcessIdentity
	}
	return f.Chown(int(uid), int(gid))
}
func bindJail(ctx context.Context, r JailRequest, run, source, target string, readonly bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	scope := filepath.Join(r.NodeDir, "jails", r.Journal.OperationID)
	if !host.Within(scope, target) || target == scope || !host.Within(r.NodeDir, source) && !host.Within(run, source) {
		return fmt.Errorf("jail bind outside owned scope")
	}
	f, err := os.OpenFile(source, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() && !st.IsDir() {
		return fmt.Errorf("jail bind source must be ordinary file or directory")
	}
	s := st.Sys().(*syscall.Stat_t)
	identity := map[string]string{"run_dir": run, "jail_dir": scope, "node_dir": r.NodeDir, "node_id": r.Owner.NodeID, "owner": r.Owner.Tag(), "target": target, "source": source, "source_device": strconv.FormatUint(uint64(s.Dev), 10), "source_inode": strconv.FormatUint(s.Ino, 10), "readonly": strconv.FormatBool(readonly)}
	id, err := r.Journal.IntentIdentity("jail-mount", identity)
	if err != nil {
		return err
	}
	if st.IsDir() {
		err = state.OwnedDir(target, r.Owner)
	} else {
		err = host.PublishFile(ctx, target, 0600, func(f *os.File) error {
			return unix.Fsetxattr(int(f.Fd()), mountOwnerAttribute, []byte(r.Owner.Tag()), 0)
		})
	}
	if err != nil {
		return err
	}
	if err = unix.Mount("/proc/self/fd/"+strconv.Itoa(int(f.Fd())), target, "", unix.MS_BIND, ""); err != nil {
		return err
	}
	if readonly {
		if err = unix.Mount("", target, "", unix.MS_BIND|unix.MS_REMOUNT|unix.MS_RDONLY, ""); err != nil {
			return err
		}
	}
	mount, err := host.MountAt(target)
	if err != nil || mount == nil {
		return fmt.Errorf("cannot observe jail mount: %w", err)
	}
	identity["mount_id"] = strconv.Itoa(mount.ID)
	return r.Journal.Done(id, identity)
}

func RemoveJails(ctx context.Context, rootID string, j *state.Journal) error {
	var failures []error
	for _, r := range j.Resources {
		if r.Kind == "vmm-process" && r.State != "released" {
			return fmt.Errorf("process cleanup must complete before removing jail mounts")
		}
	}
	for i := len(j.Resources) - 1; i >= 0; i-- {
		r := j.Resources[i]
		if r.State == "released" || r.Kind != "jail-mount" && r.Kind != "runtime-dir" && r.Kind != "jail-dir" {
			continue
		}
		if err := ctx.Err(); err != nil {
			return errors.Join(append(failures, err)...)
		}
		owner := state.Owner{RootID: rootID, EnvironmentID: j.OwnerID, NodeID: r.Identity["node_id"]}
		if err := owner.Validate(); err != nil {
			failures = append(failures, err)
			continue
		}
		if r.Identity["owner"] != owner.Tag() {
			failures = append(failures, ErrProcessIdentity)
			continue
		}
		scope := r.Identity["jail_dir"]
		if r.Kind == "jail-dir" {
			scope = r.Identity["path"]
		}
		if r.Kind == "runtime-dir" {
			scope = r.Identity["path"]
			if scope != filepath.Join(r.Identity["runtime_base"], runtimeName(owner, j.OperationID)) {
				failures = append(failures, ErrProcessIdentity)
				continue
			}
		} else {
			node := r.Identity["node_dir"]
			if scope != filepath.Join(node, "jails", j.OperationID) {
				failures = append(failures, ErrProcessIdentity)
				continue
			}
			if err := state.VerifyOwner(node, owner); err != nil {
				failures = append(failures, err)
				continue
			}
		}
		err := state.VerifyOwner(scope, owner)
		if os.IsNotExist(err) {
			if r.State == "intent" {
				err = j.Absent(r.ID)
			} else {
				err = j.Released(r.ID)
			}
			if err != nil {
				failures = append(failures, err)
			}
			continue
		}
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if r.Kind == "jail-mount" {
			err = removeJailMount(r, j, owner)
		} else {
			if r.Kind == "runtime-dir" {
				for _, child := range j.Resources {
					if (child.Kind == "jail-mount" || child.Kind == "jail-dir") && child.State != "released" && child.Identity["node_id"] == owner.NodeID {
						err = fmt.Errorf("retaining runtime scope while jail cleanup is incomplete")
						break
					}
				}
			}
			mounts, e := host.Mounts()
			if e != nil {
				err = e
			} else {
				for _, mount := range mounts {
					if host.Within(scope, mount.Path) {
						err = fmt.Errorf("runtime scope still contains mount %s", mount.Path)
						break
					}
				}
			}
			if err == nil {
				if r.State == "intent" {
					err = j.Done(r.ID, r.Identity)
				}
				if err == nil {
					err = os.RemoveAll(scope)
				}
				if err == nil {
					err = j.Released(r.ID)
				}
			}
		}
		if err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}
func removeJailMount(r state.Resource, j *state.Journal, owner state.Owner) error {
	target, run := r.Identity["target"], r.Identity["jail_dir"]
	if !host.Within(run, target) || target == run {
		return ErrProcessIdentity
	}
	mount, err := host.MountAt(target)
	if err != nil {
		return err
	}
	if mount != nil {
		var st unix.Stat_t
		if err = unix.Stat(target, &st); err != nil {
			return err
		}
		if r.Identity["source_device"] != strconv.FormatUint(uint64(st.Dev), 10) || r.Identity["source_inode"] != strconv.FormatUint(st.Ino, 10) {
			return ErrProcessIdentity
		}
		if r.State == "done" && r.Identity["mount_id"] != strconv.Itoa(mount.ID) {
			return ErrProcessIdentity
		}
		if r.State == "intent" {
			identity := r.Identity
			identity["mount_id"] = strconv.Itoa(mount.ID)
			if err = j.Done(r.ID, identity); err != nil {
				return err
			}
			r.State = "done"
		}
		if err = unix.Unmount(target, 0); err != nil {
			return err
		}
	}
	st, err := os.Lstat(target)
	if os.IsNotExist(err) {
		if r.State == "intent" {
			return j.Absent(r.ID)
		}
		return j.Released(r.ID)
	}
	if err != nil {
		return err
	}
	if st.IsDir() {
		if err = state.VerifyOwner(target, owner); err != nil {
			return err
		}
		err = os.RemoveAll(target)
	} else {
		if !st.Mode().IsRegular() {
			return ErrProcessIdentity
		}
		value := make([]byte, 256)
		n, e := unix.Lgetxattr(target, mountOwnerAttribute, value)
		if e != nil || string(value[:max(n, 0)]) != owner.Tag() {
			return ErrProcessIdentity
		}
		err = os.Remove(target)
	}
	if err != nil {
		return err
	}
	if r.State == "intent" {
		if err = j.Done(r.ID, r.Identity); err != nil {
			return err
		}
	}
	return j.Released(r.ID)
}
