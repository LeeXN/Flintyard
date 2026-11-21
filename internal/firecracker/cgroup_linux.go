package firecracker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/LeeXN/Flintyard/internal/state"
	"golang.org/x/sys/unix"
)

const cgroupOwnerAttribute = "trusted.flintyard.owner"

type CgroupLayout struct {
	Version int    `json:"version"`
	Mount   string `json:"mount"`
}
type Cgroup struct {
	Layout      CgroupLayout `json:"layout"`
	Parent      string       `json:"parent"`
	Path        string       `json:"path"`
	Owner       string       `json:"owner"`
	Device      uint64       `json:"device"`
	Inode       uint64       `json:"inode"`
	MemoryBytes uint64       `json:"memory_bytes"`
	Anchor      string       `json:"anchor"`
}

func MemoryLimit(memoryMiB int) (uint64, error) {
	if memoryMiB < 128 || memoryMiB > 1048576 {
		return 0, fmt.Errorf("invalid guest memory")
	}
	// Explicit VMM headroom: max(128 MiB, 25% guest RAM). This is a host
	// safety limit, not a promise of CPU or performance isolation.
	return uint64(memoryMiB+max(128, (memoryMiB+3)/4)) << 20, nil
}
func parseCgroupMounts(data string) ([]CgroupLayout, error) {
	var layouts []CgroupLayout
	for _, line := range strings.Split(data, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) != 6 {
			return nil, fmt.Errorf("malformed /proc/mounts")
		}
		version := 0
		if fields[2] == "cgroup2" {
			version = 2
		}
		if fields[2] == "cgroup" {
			for _, option := range strings.Split(fields[3], ",") {
				if option == "memory" {
					version = 1
				}
			}
		}
		if version == 0 {
			continue
		}
		path := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`).Replace(fields[1])
		if !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsAny(path, "\n\r\t") {
			return nil, fmt.Errorf("unsupported cgroup mount path")
		}
		layouts = append(layouts, CgroupLayout{version, path})
	}
	return layouts, nil
}
func DiscoverCgroup() (CgroupLayout, error) {
	b, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return CgroupLayout{}, err
	}
	l, err := parseCgroupMounts(string(b))
	if err != nil {
		return CgroupLayout{}, err
	}
	for _, layout := range l {
		if layout.Version != 2 {
			continue
		}
		available, e := os.ReadFile(filepath.Join(layout.Mount, "cgroup.controllers"))
		if e != nil {
			continue
		}
		if !hasWord(string(available), "memory") {
			continue
		}
		enabled, e := os.ReadFile(filepath.Join(layout.Mount, "cgroup.subtree_control"))
		if e != nil {
			return CgroupLayout{}, e
		}
		if !hasWord(string(enabled), "memory") {
			return CgroupLayout{}, fmt.Errorf("memory controller is not delegated at %s; Flintyard does not change the host ancestor controllers", layout.Mount)
		}
		return layout, nil
	}
	for _, layout := range l {
		if layout.Version == 1 {
			return layout, nil
		}
	}
	return CgroupLayout{}, fmt.Errorf("no usable cgroup v2 memory delegation or v1 memory hierarchy")
}
func hasWord(text, word string) bool {
	for _, w := range strings.Fields(text) {
		if w == word {
			return true
		}
	}
	return false
}

type CgroupRequest struct {
	Layout    CgroupLayout
	Owner     state.Owner
	Anchor    string
	MemoryMiB int
	Journal   *state.Journal
}

func PrepareCgroup(ctx context.Context, r CgroupRequest) (Cgroup, error) {
	var cg Cgroup
	if err := r.Owner.Validate(); err != nil {
		return cg, err
	}
	if r.Owner.NodeID == "" || r.Journal == nil || !state.ValidUUID(r.Journal.OperationID) || r.Journal.OwnerID != r.Owner.EnvironmentID {
		return cg, fmt.Errorf("cgroup requires a node and its environment journal")
	}
	anchorOwner := r.Owner
	anchorOwner.NodeID = ""
	if err := state.VerifyOwner(r.Anchor, anchorOwner); err != nil {
		return cg, err
	}
	if r.Layout.Version != 1 && r.Layout.Version != 2 {
		return cg, fmt.Errorf("invalid cgroup version")
	}
	limit, err := MemoryLimit(r.MemoryMiB)
	if err != nil {
		return cg, err
	}
	var fs unix.Statfs_t
	if err = unix.Statfs(r.Layout.Mount, &fs); err != nil {
		return cg, err
	}
	if r.Layout.Version == 2 && fs.Type != unix.CGROUP2_SUPER_MAGIC || r.Layout.Version == 1 && fs.Type != unix.CGROUP_SUPER_MAGIC {
		return cg, fmt.Errorf("cgroup filesystem identity mismatch")
	}
	parent := filepath.Join(r.Layout.Mount, "fy-"+r.Journal.OperationID)
	parentTag := anchorOwner.Tag() + ":" + r.Journal.OperationID
	if err = ensureCgroup(ctx, parent, parentTag, r.Anchor, r.Layout, r.Journal, "cgroup-parent"); err != nil {
		return cg, err
	}
	if r.Layout.Version == 2 {
		if err = os.WriteFile(filepath.Join(parent, "cgroup.subtree_control"), []byte("+memory"), 0); err != nil {
			return cg, err
		}
	}
	path := filepath.Join(parent, r.Owner.NodeID)
	tag := r.Owner.Tag() + ":" + r.Journal.OperationID
	if err = ensureCgroup(ctx, path, tag, r.Anchor, r.Layout, r.Journal, "cgroup-leaf"); err != nil {
		return cg, err
	}
	file := "memory.limit_in_bytes"
	if r.Layout.Version == 2 {
		file = "memory.max"
	}
	if err = os.WriteFile(filepath.Join(path, file), []byte(strconv.FormatUint(limit, 10)), 0); err != nil {
		return cg, err
	}
	var st unix.Stat_t
	if err = unix.Stat(path, &st); err != nil {
		return cg, err
	}
	cg = Cgroup{Layout: r.Layout, Parent: parent, Path: path, Owner: tag, Device: uint64(st.Dev), Inode: st.Ino, MemoryBytes: limit, Anchor: r.Anchor}
	return cg, nil
}

func ensureCgroup(ctx context.Context, path, tag, anchor string, layout CgroupLayout, j *state.Journal, kind string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := os.Lstat(path); err == nil {
		// Multiple nodes in the same operation reuse only the marked parent.
		if kind == "cgroup-parent" {
			e := verifyCgroupTag(path, tag)
			if !errors.Is(e, unix.EOPNOTSUPP) {
				return e
			}
			var st unix.Stat_t
			if e = unix.Stat(path, &st); e != nil {
				return e
			}
			for _, resource := range j.Resources {
				if resource.Kind == kind && resource.State == "done" && resource.Identity["path"] == path && resource.Identity["owner"] == tag && resource.Identity["device"] == strconv.FormatUint(uint64(st.Dev), 10) && resource.Identity["inode"] == strconv.FormatUint(st.Ino, 10) {
					return nil
				}
			}
			return ErrProcessIdentity
		}
		return fmt.Errorf("cgroup leaf already exists")
	} else if !os.IsNotExist(err) {
		return err
	}
	var parent unix.Stat_t
	if err := unix.Stat(filepath.Dir(path), &parent); err != nil {
		return err
	}
	identity := map[string]string{"path": path, "owner": tag, "anchor": anchor, "mount": layout.Mount, "version": strconv.Itoa(layout.Version), "parent_device": strconv.FormatUint(uint64(parent.Dev), 10), "parent_inode": strconv.FormatUint(parent.Ino, 10)}
	id, err := j.IntentIdentity(kind, identity)
	if err != nil {
		return err
	}
	if err = unix.Mkdir(path, 0755); err != nil {
		return err
	}
	if err = unix.Setxattr(path, cgroupOwnerAttribute, []byte(tag), unix.XATTR_CREATE); err != nil && !errors.Is(err, unix.EOPNOTSUPP) {
		return fmt.Errorf("cgroup ownership xattr: %w", err)
	}
	var st unix.Stat_t
	if err = unix.Stat(path, &st); err != nil {
		return err
	}
	identity["device"], identity["inode"] = strconv.FormatUint(uint64(st.Dev), 10), strconv.FormatUint(st.Ino, 10)
	return j.Done(id, identity)
}
func verifyCgroupTag(path, want string) error {
	var st unix.Stat_t
	if err := unix.Lstat(path, &st); err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		return ErrProcessIdentity
	}
	value := make([]byte, 512)
	n, err := unix.Getxattr(path, cgroupOwnerAttribute, value)
	if err != nil {
		return err
	}
	if string(value[:n]) != want {
		return ErrProcessIdentity
	}
	return nil
}
func (c Cgroup) Verify() error {
	if err := verifyCgroupTag(c.Path, c.Owner); err != nil {
		if !errors.Is(err, unix.EOPNOTSUPP) {
			return err
		}
		var anchor state.Owner
		if e := state.Read(filepath.Join(c.Anchor, "owner.json"), &anchor); e != nil {
			return e
		}
		if e := state.VerifyOwner(c.Anchor, anchor); e != nil {
			return e
		}
		if anchor.NodeID != "" || !strings.HasPrefix(c.Owner, anchor.Tag()) || filepath.Base(c.Path) == "" {
			return ErrProcessIdentity
		}
	}
	var st unix.Stat_t
	if err := unix.Stat(c.Path, &st); err != nil {
		return err
	}
	if uint64(st.Dev) != c.Device || st.Ino != c.Inode {
		return ErrProcessIdentity
	}
	return nil
}
func (c Cgroup) Members() ([]int, error) {
	if err := c.Verify(); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(filepath.Join(c.Path, "cgroup.procs"))
	if err != nil {
		return nil, err
	}
	var pids []int
	for _, s := range strings.Fields(string(b)) {
		pid, err := strconv.Atoi(s)
		if err != nil || pid <= 0 {
			return nil, fmt.Errorf("invalid cgroup membership")
		}
		pids = append(pids, pid)
	}
	return pids, nil
}
func (c Cgroup) Contains(pid int) error {
	pids, err := c.Members()
	if err != nil {
		return err
	}
	for _, p := range pids {
		if p == pid {
			return nil
		}
	}
	return fmt.Errorf("VMM is not in its owned cgroup")
}
func (c Cgroup) Args(nodeID string) ([]string, error) {
	if c.Layout.Version != 1 && c.Layout.Version != 2 {
		return nil, fmt.Errorf("unsupported cgroup version")
	}
	if !state.ValidUUID(nodeID) || filepath.Base(c.Path) != nodeID {
		return nil, fmt.Errorf("cgroup/node mismatch")
	}
	rel, err := filepath.Rel(c.Layout.Mount, c.Path)
	if err != nil || strings.HasPrefix(rel, "..") {
		return nil, fmt.Errorf("invalid cgroup path")
	}
	if c.Layout.Version == 2 {
		return []string{"--cgroup-version", "2", "--parent-cgroup", rel}, nil
	}
	return []string{"--cgroup-version", "1", "--parent-cgroup", filepath.Dir(rel), "--cgroup", "memory.limit_in_bytes=" + strconv.FormatUint(c.MemoryBytes, 10)}, nil
}

// RemoveCgroups runs after VMM termination. It never uses cgroup.kill; nonempty
// or changed groups are retained. An unmarked creation-window directory can be
// reclaimed only by its exact intent, owned anchor, unchanged parent and empty
// kernel cgroup, never by a name prefix or a scan of unrelated descendants.
func RemoveCgroups(ctx context.Context, j *state.Journal) error {
	var failures []error
	for i := len(j.Resources) - 1; i >= 0; i-- {
		r := j.Resources[i]
		if r.Kind != "cgroup-leaf" && r.Kind != "cgroup-parent" || r.State == "released" {
			continue
		}
		if err := ctx.Err(); err != nil {
			return errors.Join(append(failures, err)...)
		}
		err := removeCgroupResource(r, j)
		if err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", r.Kind, err))
		}
	}
	return errors.Join(failures...)
}
func removeCgroupResource(r state.Resource, j *state.Journal) error {
	path, tag := r.Identity["path"], r.Identity["owner"]
	if !strings.Contains(tag, ":"+j.OwnerID+":") || !strings.HasSuffix(tag, ":"+j.OperationID) {
		return ErrProcessIdentity
	}
	var anchor state.Owner
	if err := state.Read(filepath.Join(r.Identity["anchor"], "owner.json"), &anchor); err != nil {
		return err
	}
	if anchor.EnvironmentID != j.OwnerID || anchor.NodeID != "" || !strings.HasPrefix(tag, anchor.Tag()) {
		return ErrProcessIdentity
	}
	if err := state.VerifyOwner(r.Identity["anchor"], anchor); err != nil {
		return err
	}
	parentPath := filepath.Join(r.Identity["mount"], "fy-"+j.OperationID)
	expectedTag := anchor.Tag() + ":" + j.OperationID
	if r.Kind == "cgroup-leaf" {
		if !state.ValidUUID(filepath.Base(path)) || filepath.Dir(path) != parentPath {
			return ErrProcessIdentity
		}
		expectedTag = anchor.Tag() + filepath.Base(path) + ":" + j.OperationID
	} else if path != parentPath {
		return ErrProcessIdentity
	}
	if tag != expectedTag {
		return ErrProcessIdentity
	}
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		if r.State == "intent" {
			return j.Absent(r.ID)
		}
		return j.Released(r.ID)
	} else if err != nil {
		return err
	}
	var st unix.Stat_t
	if err := unix.Lstat(path, &st); err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		return ErrProcessIdentity
	}
	if r.State == "done" && (r.Identity["device"] != strconv.FormatUint(uint64(st.Dev), 10) || r.Identity["inode"] != strconv.FormatUint(st.Ino, 10)) {
		return ErrProcessIdentity
	}
	err := verifyCgroupTag(path, tag)
	if err != nil {
		if !errors.Is(err, unix.EOPNOTSUPP) && (r.State != "intent" || !errors.Is(err, unix.ENODATA)) {
			return err
		}
		var parent unix.Stat_t
		if err = unix.Stat(filepath.Dir(path), &parent); err != nil {
			return err
		}
		if r.Identity["parent_device"] != strconv.FormatUint(uint64(parent.Dev), 10) || r.Identity["parent_inode"] != strconv.FormatUint(parent.Ino, 10) {
			return ErrProcessIdentity
		}
		if filepath.Base(path) != "fy-"+j.OperationID && (!state.ValidUUID(filepath.Base(path)) || filepath.Base(filepath.Dir(path)) != "fy-"+j.OperationID) {
			return ErrProcessIdentity
		}
	}
	var fs unix.Statfs_t
	if err = unix.Statfs(path, &fs); err != nil {
		return err
	}
	if fs.Type != unix.CGROUP_SUPER_MAGIC && fs.Type != unix.CGROUP2_SUPER_MAGIC {
		return ErrProcessIdentity
	}
	b, err := os.ReadFile(filepath.Join(path, "cgroup.procs"))
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(b)) != "" {
		return fmt.Errorf("cgroup still contains processes")
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			return fmt.Errorf("cgroup still contains child groups")
		}
	}
	if r.State == "intent" {
		identity := r.Identity
		identity["device"], identity["inode"] = strconv.FormatUint(uint64(st.Dev), 10), strconv.FormatUint(st.Ino, 10)
		if err = j.Done(r.ID, identity); err != nil {
			return err
		}
	}
	if err = unix.Rmdir(path); err != nil {
		return err
	}
	return j.Released(r.ID)
}
