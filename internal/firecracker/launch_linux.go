package firecracker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/LeeXN/Flintyard/internal/host"
	"github.com/LeeXN/Flintyard/internal/state"
	"golang.org/x/sys/unix"
)

type Binaries struct {
	Firecracker host.Binary `json:"firecracker"`
	Jailer      host.Binary `json:"jailer"`
}

func CheckBinaries(ctx context.Context, fc, jailer string) (Binaries, error) {
	var b Binaries
	var err error
	if b.Firecracker, err = host.InspectBinary(ctx, fc, "Firecracker"); err != nil {
		return b, err
	}
	if b.Jailer, err = host.InspectBinary(ctx, jailer, "Jailer"); err != nil {
		return b, err
	}
	return b, nil
}

type LaunchRequest struct {
	Binaries                                      Binaries
	Owner                                         state.Owner
	RunDir, JailBase, JailRoot, Socket, SerialLog string
	NetNS                                         string
	VirtioTransport                               string
	NetNSInode                                    uint64
	UID, GID                                      uint32
	Cgroup                                        Cgroup
	Journal                                       *state.Journal
}
type LaunchResult struct {
	Identity   Identity `json:"identity"`
	Socket     string   `json:"socket"`
	JailRoot   string   `json:"jail_root"`
	ResourceID string   `json:"resource_id"`
}

func (r LaunchRequest) Args() ([]string, error) {
	if err := r.Owner.Validate(); err != nil {
		return nil, err
	}
	if r.Owner.NodeID == "" || r.UID == 0 || r.GID == 0 || r.UID == ^uint32(0) || r.GID == ^uint32(0) || !filepath.IsAbs(r.JailBase) || !filepath.IsAbs(r.NetNS) {
		return nil, fmt.Errorf("invalid jailer launch parameters")
	}
	args := []string{"--id", r.Owner.NodeID, "--exec-file", r.Binaries.Firecracker.Path, "--uid", strconv.FormatUint(uint64(r.UID), 10), "--gid", strconv.FormatUint(uint64(r.GID), 10), "--chroot-base-dir", r.JailBase, "--netns", r.NetNS}
	cg, err := r.Cgroup.Args(r.Owner.NodeID)
	if err != nil {
		return nil, err
	}
	args = append(args, cg...)
	if r.VirtioTransport != "" && r.VirtioTransport != "mmio" && r.VirtioTransport != "pci" {
		return nil, fmt.Errorf("invalid virtio transport")
	}
	// Deliberately omit --daemonize and --new-pid-ns. Setsid is established by
	// our launcher and the official jailer execs in place with inherited logs.
	args = append(args, "--", "--api-sock", "/run/api.sock")
	if r.VirtioTransport == "pci" {
		args = append(args, "--enable-pci")
	}
	return args, nil
}

type detached struct {
	pid int
	fd  int
}

func startDetached(binary string, args []string, log *os.File, cgroup *Cgroup) (detached, error) {
	probe, err := unix.PidfdOpen(os.Getpid(), 0)
	if err != nil {
		return detached{}, err
	}
	unix.Close(probe)
	null, err := os.Open(os.DevNull)
	if err != nil {
		return detached{}, err
	}
	defer null.Close()
	fd := -1
	attr := &syscall.SysProcAttr{Setsid: true, PidFD: &fd}
	// The privileged bootstrap has predictable real/effective credentials and
	// inherits no supplementary host groups before jailer drops to the VMM UID.
	if os.Geteuid() == 0 {
		attr.Credential = &syscall.Credential{Uid: 0, Gid: 0, Groups: []uint32{}}
	}
	var cgfd int = -1
	if cgroup != nil && cgroup.Layout.Version == 2 {
		if err = cgroup.Verify(); err != nil {
			return detached{}, err
		}
		cgfd, err = unix.Open(cgroup.Path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		if err != nil {
			return detached{}, err
		}
		defer unix.Close(cgfd)
		attr.UseCgroupFD = true
		attr.CgroupFD = cgfd
	}
	// Never CommandContext: ending the successful CLI operation must not kill a
	// committed VM. Stdin is null; stdout/stderr are ordinary files, not pipes.
	p, err := os.StartProcess(binary, append([]string{binary}, args...), &os.ProcAttr{Dir: "/", Env: []string{}, Files: []*os.File{null, log, log}, Sys: attr})
	if err != nil {
		if fd >= 0 {
			unix.Close(fd)
		}
		return detached{}, err
	}
	// Reap only while this caller process remains alive. This is not a VMM
	// supervisor; no restart, heartbeat or long-running management process is
	// introduced. On CLI exit the kernel reparents the independent session.
	pid := p.Pid
	go func() { _, _ = p.Wait() }()
	return detached{pid, fd}, nil
}

func Launch(ctx context.Context, r LaunchRequest) (result LaunchResult, err error) {
	if err = r.Owner.Validate(); err != nil {
		return result, err
	}
	if r.Journal == nil || !state.ValidUUID(r.Journal.OperationID) || r.Journal.OwnerID != r.Owner.EnvironmentID {
		return result, fmt.Errorf("launch journal ownership mismatch")
	}
	if err = state.VerifyOwner(r.RunDir, r.Owner); err != nil {
		return result, err
	}
	if len(r.Socket) > 107 || r.Socket != filepath.Join(r.RunDir, "api", "api.sock") {
		return result, fmt.Errorf("invalid short API socket path")
	}
	if r.JailRoot != filepath.Join(r.JailBase, filepath.Base(r.Binaries.Firecracker.Path), r.Owner.NodeID, "root") {
		return result, fmt.Errorf("jail layout mismatch")
	}
	if err = r.Cgroup.Verify(); err != nil {
		return result, err
	}
	for _, binary := range []host.Binary{r.Binaries.Firecracker, r.Binaries.Jailer} {
		if binary.Version != Version || binary.Architecture != host.Architecture() {
			return result, fmt.Errorf("binary compatibility mismatch")
		}
		if err = binary.Verify(ctx); err != nil {
			return result, err
		}
	}
	var ns unix.Stat_t
	if err = unix.Stat(r.NetNS, &ns); err != nil {
		return result, err
	}
	if ns.Ino != r.NetNSInode {
		return result, ErrProcessIdentity
	}
	args, err := r.Args()
	if err != nil {
		return result, err
	}
	argv, _ := json.Marshal(append([]string{r.Binaries.Jailer.Path}, args...))
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return result, err
	}
	want := map[string]string{"run_dir": r.RunDir, "jail_root": r.JailRoot, "node_id": r.Owner.NodeID, "owner": r.Owner.Tag(), "boot_id": strings.TrimSpace(string(boot)), "argv": string(argv), "cgroup": r.Cgroup.Path, "netns_inode": strconv.FormatUint(r.NetNSInode, 10), "jailer_device": strconv.FormatUint(r.Binaries.Jailer.Device, 10), "jailer_inode": strconv.FormatUint(r.Binaries.Jailer.Inode, 10)}
	parent, err := Observe(os.Getpid())
	if err != nil {
		return result, err
	}
	want["min_starttime"] = strconv.FormatUint(parent.StartTime, 10)
	want["uid"], want["gid"] = strconv.FormatUint(uint64(r.UID), 10), strconv.FormatUint(uint64(r.GID), 10)
	want["fc_sha256"] = r.Binaries.Firecracker.SHA256
	want["fc_name"] = filepath.Base(r.Binaries.Firecracker.Path)
	want["fc_path"] = r.Binaries.Firecracker.Path
	id, err := r.Journal.IntentIdentity("vmm-process", want)
	if err != nil {
		return result, err
	}
	log, err := os.OpenFile(r.SerialLog, os.O_CREATE|os.O_APPEND|os.O_WRONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0600)
	if err != nil {
		return result, err
	}
	st, err := log.Stat()
	if err != nil || !st.Mode().IsRegular() {
		log.Close()
		return result, fmt.Errorf("serial log must be an ordinary file")
	}
	if err = ctx.Err(); err != nil {
		log.Close()
		return result, err
	}
	d, err := startDetached(r.Binaries.Jailer.Path, args, log, &r.Cgroup)
	log.Close()
	if err != nil {
		return result, err
	}
	defer unix.Close(d.fd)
	pid := d.pid
	spawn := map[string]string{"pid": strconv.Itoa(pid)}
	if observed, e := Observe(pid); e == nil {
		spawn["spawn_starttime"] = strconv.FormatUint(observed.StartTime, 10)
	}
	if err = r.Journal.UpdateIntent(id, spawn); err != nil {
		return result, errors.Join(err, stopLaunching(d, r))
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, stopLaunching(d, r))
		}
	}()
	for {
		if err = ctx.Err(); err != nil {
			return result, err
		}
		if exited, e := pidfdExited(d.fd); e != nil {
			return result, e
		} else if exited {
			return result, fmt.Errorf("jailer/VMM exited before verified API readiness; see %s", r.SerialLog)
		}
		identity, e := Observe(pid)
		if e == nil && identity.NetNS == fmt.Sprintf("net:[%d]", r.NetNSInode) && r.Cgroup.Contains(pid) == nil && processCredentials(pid, r.UID, r.GID) == nil {
			copyPath := filepath.Join(r.JailRoot, filepath.Base(r.Binaries.Firecracker.Path))
			copySt, e := os.Stat(copyPath)
			if e == nil {
				s := copySt.Sys().(*syscall.Stat_t)
				if uint64(s.Dev) == identity.ExecutableDevice && s.Ino == identity.ExecutableInode && verifyCopiedBinary(ctx, copyPath, r.Binaries.Firecracker.SHA256) == nil {
					// Persist the complete observed identity before any API mutation.
					encoded, _ := json.Marshal(identity)
					want["identity"] = string(encoded)
					want["launch_phase"] = "vmm"
					want["pid"] = strconv.Itoa(pid)
					if err = r.Journal.Done(id, want); err != nil {
						return result, err
					}
					result = LaunchResult{identity, r.Socket, r.JailRoot, id}
					return result, nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return result, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func verifyCopiedBinary(ctx context.Context, path, want string) error {
	// Reuse the existing digest reader without introducing an image/store
	// dependency into process control. The copied inode must match proc/exe.
	f, err := os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	buf := make([]byte, 128<<10)
	for {
		if err = ctx.Err(); err != nil {
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
	if hex.EncodeToString(h.Sum(nil)) != want {
		return ErrProcessIdentity
	}
	return nil
}
func processCredentials(pid int, uid, gid uint32) error {
	b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "status"))
	if err != nil {
		return err
	}
	seen := 0
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) == 0 || f[0] != "Uid:" && f[0] != "Gid:" {
			continue
		}
		if len(f) != 5 {
			return ErrProcessIdentity
		}
		want := uid
		if f[0] == "Gid:" {
			want = gid
		}
		for _, value := range f[1:] {
			n, e := strconv.ParseUint(value, 10, 32)
			if e != nil || uint32(n) != want {
				return ErrProcessIdentity
			}
		}
		seen++
	}
	if seen != 2 {
		return ErrProcessIdentity
	}
	return nil
}
func stopLaunching(d detached, r LaunchRequest) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if exited, err := pidfdExited(d.fd); err != nil || exited {
		return err
	}
	id, err := Observe(d.pid)
	if err != nil {
		return err
	}
	launcher := id.ExecutableDevice == r.Binaries.Jailer.Device && id.ExecutableInode == r.Binaries.Jailer.Inode
	copy, copyErr := os.Stat(filepath.Join(r.JailRoot, filepath.Base(r.Binaries.Firecracker.Path)))
	vmm := false
	if copyErr == nil {
		s := copy.Sys().(*syscall.Stat_t)
		vmm = id.NetNS == fmt.Sprintf("net:[%d]", r.NetNSInode) && r.Cgroup.Contains(id.PID) == nil && uint64(s.Dev) == id.ExecutableDevice && s.Ino == id.ExecutableInode && processCredentials(id.PID, r.UID, r.GID) == nil
	}
	if !launcher && !vmm {
		return ErrProcessIdentity
	}
	if exited, e := pidfdExited(d.fd); e != nil || exited {
		return e
	}
	_, err = Terminate(ctx, TerminateRequest{Identity: id, Grace: time.Second})
	return err
}
