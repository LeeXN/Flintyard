package flintyard

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/LeeXN/Flintyard/internal/host"
	"github.com/LeeXN/Flintyard/internal/image"
	"github.com/LeeXN/Flintyard/internal/state"
	"golang.org/x/sys/unix"
)

func gcNodeOwner(root state.Root, e environmentState, r state.Resource) (state.Owner, error) {
	id := r.Identity["node_id"]
	for _, n := range e.Nodes {
		if n.ID == id {
			owner := state.Owner{RootID: root.ID, EnvironmentID: e.ID, NodeID: id}
			if r.Identity["owner"] != owner.Tag() {
				return owner, fmt.Errorf("gc node owner mismatch")
			}
			return owner, nil
		}
	}
	return state.Owner{}, fmt.Errorf("gc resource references unknown node")
}
func gcJailIdentity(root state.Root, e environmentState, j *state.Journal, r state.Resource) (map[string]string, error) {
	owner, err := gcNodeOwner(root, e, r)
	if err != nil {
		return nil, err
	}
	node := nodeDir(root, e.ID, owner.NodeID)
	if r.Kind == "runtime-dir" {
		path := r.Identity["path"]
		base := r.Identity["runtime_base"]
		if !filepath.IsAbs(base) || path != filepath.Join(base, image.Sum([]byte(owner.Tag() + ":" + j.OperationID))[:16]) {
			return nil, fmt.Errorf("runtime gc path mismatch")
		}
		identity, err := pathIdentity(path)
		if err != nil || identity["exists"] == "false" {
			return identity, err
		}
		if err = state.VerifyOwner(path, owner); err != nil {
			return nil, err
		}
		return identity, nil
	}
	scope := filepath.Join(node, "jails", j.OperationID)
	if r.Identity["node_dir"] != node {
		return nil, fmt.Errorf("jail gc anchor mismatch")
	}
	if r.Kind == "jail-dir" {
		if r.Identity["path"] != scope {
			return nil, fmt.Errorf("jail gc directory mismatch")
		}
		identity, err := pathIdentity(scope)
		if err != nil || identity["exists"] == "false" {
			return identity, err
		}
		if err = state.VerifyOwner(scope, owner); err != nil {
			return nil, err
		}
		return identity, nil
	}
	if r.Identity["jail_dir"] != scope || !host.Within(scope, r.Identity["target"]) || r.Identity["target"] == scope {
		return nil, fmt.Errorf("jail gc mount scope mismatch")
	}
	target := r.Identity["target"]
	mount, err := host.MountAt(target)
	if err != nil {
		return nil, err
	}
	identity, err := pathIdentity(target)
	if err != nil {
		return nil, err
	}
	if mount != nil {
		if identity["device"] != r.Identity["source_device"] || identity["inode"] != r.Identity["source_inode"] || r.State == "done" && strconv.Itoa(mount.ID) != r.Identity["mount_id"] {
			return nil, fmt.Errorf("jail gc mount identity changed")
		}
		identity["mount_id"], identity["mount_ro"] = strconv.Itoa(mount.ID), strconv.FormatBool(mount.ReadOnly)
	} else if identity["exists"] == "true" {
		st, err := os.Lstat(target)
		if err != nil {
			return nil, err
		}
		if st.IsDir() {
			if err = state.VerifyOwner(target, owner); err != nil {
				return nil, err
			}
		} else {
			if !st.Mode().IsRegular() {
				return nil, fmt.Errorf("invalid jail gc placeholder")
			}
			value := make([]byte, 256)
			n, err := unix.Lgetxattr(target, "user.flintyard.owner", value)
			if err != nil || string(value[:max(0, n)]) != owner.Tag() {
				return nil, fmt.Errorf("jail gc placeholder owner mismatch")
			}
		}
	}
	return identity, nil
}
func gcCgroupIdentity(root state.Root, e environmentState, j *state.Journal, r state.Resource) (map[string]string, error) {
	path := r.Identity["path"]
	parent := filepath.Join(r.Identity["mount"], "fy-"+j.OperationID)
	anchor := state.Owner{RootID: root.ID, EnvironmentID: e.ID}
	expected := anchor.Tag() + ":" + j.OperationID
	if r.Identity["anchor"] != envDir(root, e.ID) {
		return nil, fmt.Errorf("cgroup gc anchor mismatch")
	}
	if r.Kind == "cgroup-leaf" {
		id := filepath.Base(path)
		found := false
		for _, n := range e.Nodes {
			if n.ID == id {
				found = true
			}
		}
		if !found || filepath.Dir(path) != parent {
			return nil, fmt.Errorf("cgroup gc leaf mismatch")
		}
		owner := anchor
		owner.NodeID = id
		expected = owner.Tag() + ":" + j.OperationID
	} else if path != parent {
		return nil, fmt.Errorf("cgroup gc parent mismatch")
	}
	if r.Identity["owner"] != expected {
		return nil, fmt.Errorf("cgroup gc owner mismatch")
	}
	identity, err := pathIdentity(path)
	if err != nil || identity["exists"] == "false" {
		return identity, err
	}
	if r.State == "done" && (identity["device"] != r.Identity["device"] || identity["inode"] != r.Identity["inode"]) {
		return nil, fmt.Errorf("cgroup gc inode changed")
	}
	var fs unix.Statfs_t
	if err = unix.Statfs(path, &fs); err != nil {
		return nil, err
	}
	if fs.Type != unix.CGROUP2_SUPER_MAGIC && fs.Type != unix.CGROUP_SUPER_MAGIC {
		return nil, fmt.Errorf("cgroup gc filesystem mismatch")
	}
	value := make([]byte, 512)
	n, err := unix.Lgetxattr(path, "trusted.flintyard.owner", value)
	if err == nil {
		if string(value[:n]) != expected {
			return nil, fmt.Errorf("cgroup gc owner tag changed")
		}
		identity["owner"] = expected
	} else if err != unix.EOPNOTSUPP && (r.State != "intent" || err != unix.ENODATA) {
		return nil, err
	}
	b, err := os.ReadFile(filepath.Join(path, "cgroup.procs"))
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(string(b)) != "" {
		return nil, fmt.Errorf("live cgroup members prevent gc")
	}
	identity["members"] = "empty"
	return identity, nil
}
