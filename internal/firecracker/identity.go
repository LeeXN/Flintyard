package firecracker

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

type Identity struct {
	PID              int    `json:"pid"`
	BootID           string `json:"boot_id"`
	StartTime        uint64 `json:"starttime"`
	Executable       string `json:"executable"`
	ExecutableDevice uint64 `json:"executable_device"`
	ExecutableInode  uint64 `json:"executable_inode"`
	Cgroup           string `json:"cgroup"`
	NetNS            string `json:"netns"`
}

// ParseStartTime handles spaces and closing parentheses inside /proc/PID/stat comm.
func ParseStartTime(stat string) (uint64, error) {
	i := strings.LastIndex(stat, ") ")
	if i < 0 {
		return 0, fmt.Errorf("malformed proc stat")
	}
	f := strings.Fields(stat[i+2:])
	if len(f) < 20 {
		return 0, fmt.Errorf("short proc stat")
	}
	return strconv.ParseUint(f[19], 10, 64)
}
func Observe(pid int) (Identity, error) {
	r := Identity{PID: pid}
	if pid <= 0 {
		return r, fmt.Errorf("invalid PID")
	}
	base := filepath.Join("/proc", strconv.Itoa(pid))
	boot, e := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if e != nil {
		return r, e
	}
	r.BootID = strings.TrimSpace(string(boot))
	b, e := os.ReadFile(filepath.Join(base, "stat"))
	if e != nil {
		return r, e
	}
	r.StartTime, e = ParseStartTime(string(b))
	if e != nil {
		return r, e
	}
	r.Executable, e = os.Readlink(filepath.Join(base, "exe"))
	if e != nil {
		return r, e
	}
	st, e := os.Stat(filepath.Join(base, "exe"))
	if e != nil {
		return r, e
	}
	exe := st.Sys().(*syscall.Stat_t)
	r.ExecutableDevice, r.ExecutableInode = uint64(exe.Dev), exe.Ino
	b, e = os.ReadFile(filepath.Join(base, "cgroup"))
	if e != nil {
		return r, e
	}
	r.Cgroup = string(b)
	r.NetNS, e = os.Readlink(filepath.Join(base, "ns/net"))
	if e != nil {
		return r, e
	}
	// A PID can disappear and be reused while proc files are read. Refuse a
	// snapshot spanning two process lifetimes; signaling additionally pins a pidfd.
	b, e = os.ReadFile(filepath.Join(base, "stat"))
	if e != nil {
		return r, e
	}
	end, e := ParseStartTime(string(b))
	if e != nil || end != r.StartTime {
		return r, fmt.Errorf("process changed during observation: %w", errors.Join(e, ErrProcessIdentity))
	}
	return r, nil
}

// Verify is observational only. Callers must use pidfd and revalidate through it
// before signaling; this function must never be followed by a bare kill(PID).
func Verify(want Identity) error {
	got, e := Observe(want.PID)
	if e != nil {
		return e
	}
	if got != want {
		return ErrProcessIdentity
	}
	return nil
}
