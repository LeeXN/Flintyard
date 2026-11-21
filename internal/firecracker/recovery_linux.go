package firecracker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/LeeXN/Flintyard/internal/contract"
	"github.com/LeeXN/Flintyard/internal/state"
	"golang.org/x/sys/unix"
)

func processOwner(r state.Resource, rootID string, j *state.Journal) (state.Owner, error) {
	owner := state.Owner{RootID: rootID, EnvironmentID: j.OwnerID, NodeID: r.Identity["node_id"]}
	if err := owner.Validate(); err != nil {
		return owner, err
	}
	if owner.NodeID == "" || r.Identity["owner"] != owner.Tag() {
		return owner, ErrProcessIdentity
	}
	return owner, nil
}
func groupForProcess(r state.Resource, j *state.Journal) (*Cgroup, error) {
	for _, resource := range j.Resources {
		if resource.Kind != "cgroup-leaf" || resource.State == "released" || resource.Identity["path"] != r.Identity["cgroup"] {
			continue
		}
		if resource.Identity["owner"] != r.Identity["owner"]+":"+j.OperationID {
			return nil, ErrProcessIdentity
		}
		device, e := strconv.ParseUint(resource.Identity["device"], 10, 64)
		if e != nil {
			return nil, e
		}
		inode, e := strconv.ParseUint(resource.Identity["inode"], 10, 64)
		if e != nil {
			return nil, e
		}
		version, e := strconv.Atoi(resource.Identity["version"])
		if e != nil {
			return nil, e
		}
		cg := &Cgroup{Layout: CgroupLayout{version, resource.Identity["mount"]}, Parent: filepath.Dir(resource.Identity["path"]), Path: resource.Identity["path"], Owner: resource.Identity["owner"], Device: device, Inode: inode, Anchor: resource.Identity["anchor"]}
		if err := cg.Verify(); os.IsNotExist(err) {
			return nil, nil
		} else if err != nil {
			return nil, err
		}
		return cg, nil
	}
	return nil, nil
}

// RecoverProcess recognizes only an owned cgroup/executable or an exact
// journaled jailer argv + executable inode + independent session + owned run
// directory. Scanning is not process-name matching and never itself signals.
func RecoverProcess(ctx context.Context, rootID string, j *state.Journal, r state.Resource) (*Identity, string, error) {
	owner, err := processOwner(r, rootID, j)
	if err != nil {
		return nil, "", err
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return nil, "", err
	}
	if r.Identity["boot_id"] == "" {
		return nil, "", ErrProcessIdentity
	}
	if strings.TrimSpace(string(boot)) != r.Identity["boot_id"] {
		return nil, "", nil
	}
	if err = state.VerifyOwner(r.Identity["run_dir"], owner); err != nil {
		return nil, "", err
	}
	if filepath.Base(r.Identity["run_dir"]) != runtimeName(owner, j.OperationID) {
		return nil, "", ErrProcessIdentity
	}
	name := r.Identity["fc_name"]
	if name == "" || filepath.Base(name) != name {
		return nil, "", ErrProcessIdentity
	}
	base := filepath.Dir(filepath.Dir(filepath.Dir(r.Identity["jail_root"])))
	scope := filepath.Dir(base)
	if filepath.Base(scope) != j.OperationID || filepath.Base(base) != "j" || r.Identity["jail_root"] != filepath.Join(base, name, owner.NodeID, "root") {
		return nil, "", ErrProcessIdentity
	}
	if err = state.VerifyOwner(scope, owner); err != nil {
		return nil, "", err
	}
	cg, err := groupForProcess(r, j)
	if err != nil {
		return nil, "", err
	}
	check := func(pid int) (*Identity, string, error) {
		identity, err := Observe(pid)
		if os.IsNotExist(err) {
			return nil, "", nil
		}
		if err != nil {
			return nil, "", err
		}
		if identity.BootID != r.Identity["boot_id"] {
			return nil, "", ErrProcessIdentity
		}
		if expected := r.Identity["spawn_starttime"]; expected != "" && strconv.Itoa(pid) == r.Identity["pid"] && strconv.FormatUint(identity.StartTime, 10) != expected {
			return nil, "", ErrProcessIdentity
		}
		minStart, e := strconv.ParseUint(r.Identity["min_starttime"], 10, 64)
		if e != nil || identity.StartTime < minStart {
			return nil, "", ErrProcessIdentity
		}
		jdev, e := strconv.ParseUint(r.Identity["jailer_device"], 10, 64)
		if e != nil {
			return nil, "", e
		}
		jino, e := strconv.ParseUint(r.Identity["jailer_inode"], 10, 64)
		if e != nil {
			return nil, "", e
		}
		if identity.ExecutableDevice == jdev && identity.ExecutableInode == jino {
			var argv []string
			if e = contract.Decode([]byte(r.Identity["argv"]), &argv); e != nil {
				return nil, "", e
			}
			pair := func(flag, want string) bool {
				count := 0
				for i := 1; i+1 < len(argv); i++ {
					if argv[i] == flag {
						if argv[i+1] != want {
							return false
						}
						count++
					}
				}
				return count == 1
			}
			if !pair("--id", owner.NodeID) || !pair("--exec-file", r.Identity["fc_path"]) || !pair("--chroot-base-dir", base) {
				return nil, "", ErrProcessIdentity
			}
			b, e := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
			if e != nil {
				return nil, "", e
			}
			actual := strings.Split(strings.TrimSuffix(string(b), "\x00"), "\x00")
			sid, e := unix.Getsid(pid)
			if e != nil {
				return nil, "", e
			}
			if !reflect.DeepEqual(actual, argv) || sid != pid || processCredentials(pid, 0, 0) != nil {
				return nil, "", ErrProcessIdentity
			}
			return &identity, "launcher", nil
		}
		if cg == nil || cg.Contains(pid) != nil {
			return nil, "", ErrProcessIdentity
		}
		inode, e := strconv.ParseUint(r.Identity["netns_inode"], 10, 64)
		if e != nil {
			return nil, "", e
		}
		uid, e := strconv.ParseUint(r.Identity["uid"], 10, 32)
		if e != nil {
			return nil, "", e
		}
		gid, e := strconv.ParseUint(r.Identity["gid"], 10, 32)
		if e != nil {
			return nil, "", e
		}
		if identity.NetNS != fmt.Sprintf("net:[%d]", inode) || processCredentials(pid, uint32(uid), uint32(gid)) != nil {
			return nil, "", ErrProcessIdentity
		}
		copyPath := filepath.Join(r.Identity["jail_root"], r.Identity["fc_name"])
		var st unix.Stat_t
		if e = unix.Stat(copyPath, &st); e != nil {
			return nil, "", e
		}
		if uint64(st.Dev) != identity.ExecutableDevice || st.Ino != identity.ExecutableInode {
			return nil, "", ErrProcessIdentity
		}
		if e = verifyCopiedBinary(ctx, copyPath, r.Identity["fc_sha256"]); e != nil {
			return nil, "", e
		}
		return &identity, "vmm", nil
	}
	var mismatch error
	if pid, e := strconv.Atoi(r.Identity["pid"]); e == nil && pid > 0 {
		if identity, phase, e := check(pid); e == nil && identity != nil {
			return identity, phase, nil
		} else if e != nil {
			if !errors.Is(e, ErrProcessIdentity) && !os.IsNotExist(e) {
				return nil, "", e
			}
			mismatch = e
		}
		// A reused PID is never accepted here. A separate, positively owned
		// cgroup member may still reveal the real child if the PID record was
		// incomplete; otherwise retain the ambiguity below.
	}
	if cg != nil {
		pids, e := cg.Members()
		if e != nil {
			return nil, "", e
		}
		if len(pids) > 1 {
			return nil, "", fmt.Errorf("%w: multiple processes in node cgroup", ErrProcessIdentity)
		}
		if len(pids) == 1 {
			return check(pids[0])
		}
	}
	// Covers the v1 interval before the official jailer enters its cgroup and
	// the parent dies before persisting the spawned PID. A v2 launch starts in
	// the owned leaf using CLONE_INTO_CGROUP instead.
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, "", err
	}
	wantDev, e := strconv.ParseUint(r.Identity["jailer_device"], 10, 64)
	if e != nil {
		return nil, "", e
	}
	wantInode, e := strconv.ParseUint(r.Identity["jailer_inode"], 10, 64)
	if e != nil {
		return nil, "", e
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, "", err
		}
		pid, e := strconv.Atoi(entry.Name())
		if e != nil {
			continue
		}
		var st unix.Stat_t
		if e = unix.Stat(filepath.Join("/proc", entry.Name(), "exe"), &st); errors.Is(e, unix.ENOENT) {
			continue
		} else if e != nil {
			return nil, "", e
		}
		if uint64(st.Dev) != wantDev || st.Ino != wantInode {
			continue
		}
		identity, phase, e := check(pid)
		if e == nil && identity != nil {
			return identity, phase, nil
		}
		if e != nil && !errors.Is(e, ErrProcessIdentity) && !os.IsNotExist(e) {
			return nil, "", e
		}
	}
	return nil, "", mismatch
}

func StopJournalProcesses(ctx context.Context, rootID string, j *state.Journal) error {
	var failures []error
	for _, r := range append([]state.Resource(nil), j.Resources...) {
		if r.Kind != "vmm-process" || r.State == "released" {
			continue
		}
		if _, err := processOwner(r, rootID, j); err != nil {
			failures = append(failures, err)
			continue
		}
		var identity *Identity
		if text := r.Identity["identity"]; text != "" && r.Identity["launch_phase"] == "vmm" {
			var observed Identity
			if err := contract.Decode([]byte(text), &observed); err != nil {
				failures = append(failures, err)
				continue
			}
			identity = &observed
		} else {
			observed, phase, err := RecoverProcess(ctx, rootID, j, r)
			if err != nil {
				failures = append(failures, err)
				continue
			}
			if observed == nil {
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
			identity = observed
			encoded, _ := json.Marshal(identity)
			r.Identity["identity"], r.Identity["launch_phase"] = string(encoded), phase
			if r.State == "intent" {
				err = j.Done(r.ID, r.Identity)
			} else {
				err = j.Refresh(r.ID, r.Identity)
			}
			if err != nil {
				failures = append(failures, err)
				continue
			}
		}
		phase, err := ProcessState(ctx, *identity)
		if err == nil && phase == "running" {
			_, err = Terminate(ctx, TerminateRequest{Identity: *identity, Grace: 2 * time.Second})
		}
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if err = j.Released(r.ID); err != nil {
			failures = append(failures, err)
		}
	}
	// Unknown members must block disk/jail cleanup even after the one recorded
	// VMM was stopped. Never kill arbitrary members to make cleanup look green.
	for _, resource := range j.Resources {
		if resource.Kind != "cgroup-leaf" || resource.State == "released" {
			continue
		}
		fake := state.Resource{Identity: map[string]string{"cgroup": resource.Identity["path"], "owner": strings.TrimSuffix(resource.Identity["owner"], ":"+j.OperationID)}}
		cg, err := groupForProcess(fake, j)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if cg != nil {
			members, e := cg.Members()
			if e != nil {
				failures = append(failures, e)
			} else if len(members) > 0 {
				failures = append(failures, fmt.Errorf("node cgroup still contains unverified processes"))
			}
		}
	}
	return errors.Join(failures...)
}
