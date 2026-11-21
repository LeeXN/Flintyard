// Package host inspects prerequisites without changing the host.
package host

import (
	"context"
	"fmt"
	"golang.org/x/sys/unix"
	"os"
	"os/exec"
	"runtime"
	"syscall"
)

type Check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}
type Report struct {
	Mode         string   `json:"mode"`
	Architecture string   `json:"architecture"`
	Ready        bool     `json:"ready"`
	Checks       []Check  `json:"checks"`
	Warnings     []string `json:"warnings,omitempty"`
}

func Architecture() string {
	switch runtime.GOARCH {
	case "amd64":
		return "x86_64"
	case "arm64":
		return "aarch64"
	}
	return runtime.GOARCH
}
func Doctor(ctx context.Context, mode, fc, jailer string) Report {
	r := Report{Mode: mode, Architecture: Architecture(), Ready: true}
	add := func(n string, ok bool, d string) {
		r.Checks = append(r.Checks, Check{n, ok, d})
		r.Ready = r.Ready && ok
	}
	add("platform", runtime.GOOS == "linux" && (runtime.GOARCH == "amd64" || runtime.GOARCH == "arm64"), runtime.GOOS+"/"+runtime.GOARCH)
	add("root", os.Geteuid() == 0, fmt.Sprintf("euid=%d", os.Geteuid()))
	if mode != "build" {
		add("page_size", os.Getpagesize() == 4096, fmt.Sprintf("%d", os.Getpagesize()))
		e := KVM()
		add("kvm", e == nil, detail(e))
		e = TUN()
		add("tun", e == nil, detail(e))
		for _, b := range []struct{ path, name string }{{fc, "Firecracker"}, {jailer, "Jailer"}} {
			binary, e := InspectBinary(ctx, b.path, b.name)
			add(b.path, e == nil, fmt.Sprintf("%s %s", binary.Path, detail(e)))
		}
		for _, b := range []string{"ip", "nft", "mkfs.ext4"} {
			p, e := exec.LookPath(b)
			add(b, e == nil, p+detail(e))
		}
	} else {
		for _, b := range []string{"mkfs.ext4", "mount", "umount", "chroot", "unshare"} {
			p, e := exec.LookPath(b)
			add(b, e == nil, p+detail(e))
		}
	}
	return r
}

// TUN checks the actual kernel driver, not merely a character device pathname.
// It does not create an interface or modify a network namespace.
func TUN() error {
	fd, err := unix.Open("/dev/net/tun", unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open /dev/net/tun: %w", err)
	}
	defer unix.Close(fd)
	features, err := unix.IoctlGetInt(fd, unix.TUNGETFEATURES)
	if err != nil {
		return fmt.Errorf("TUNGETFEATURES: %w", err)
	}
	if features&unix.IFF_TAP == 0 {
		return fmt.Errorf("kernel TUN driver does not support TAP")
	}
	return nil
}
func detail(e error) string {
	if e != nil {
		return e.Error()
	}
	return "ok"
}
func KVM() error {
	if os.Getpagesize() != 4096 {
		return fmt.Errorf("Firecracker baseline requires 4096-byte host pages")
	}
	f, e := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if e != nil {
		return e
	}
	defer f.Close()
	v, _, eno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), 0xae00, 0)
	if eno != 0 {
		return eno
	}
	if v != 12 {
		return fmt.Errorf("unsupported KVM API %d", v)
	}
	caps, e := requiredKVMCapabilities(Architecture())
	if e != nil {
		return e
	}
	// Match the pinned upstream architecture lists; ARM uses device-control/VGIC,
	// not x86's IRQCHIP requirement. Cross compilation cannot verify these ioctls.
	for _, cap := range caps {
		v, _, eno = syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), 0xae03, cap)
		if eno != 0 || v == 0 {
			return fmt.Errorf("KVM capability %d unavailable", cap)
		}
	}
	fd, _, eno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), 0xae01, 0)
	if eno != 0 {
		return eno
	}
	return syscall.Close(int(fd))
}

func requiredKVMCapabilities(architecture string) ([]uintptr, error) {
	switch architecture {
	case "x86_64":
		return []uintptr{0, 36, 32, 3, 4, 33, 35, 39, 50, 14, 41, 56, 55, 7}, nil
	case "aarch64":
		return []uintptr{36, 32, 3, 102, 89, 14, 70}, nil
	}
	return nil, fmt.Errorf("unsupported KVM architecture %s", architecture)
}
