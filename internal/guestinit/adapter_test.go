package guestinit

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Run the installed script with its absolute guest paths redirected into a
// private fixture. Mount, hostname and key generation are harmless test stubs;
// the script's file creation and permissions are the real shell operations.
func TestSeedAdapterCreatesReadableNetworkAndPrivateKeys(t *testing.T) {
	for _, profile := range []string{"systemd-networkd", "networkmanager", "openrc-ifupdown"} {
		t.Run(profile, func(t *testing.T) {
			root := t.TempDir()
			write := func(name, value string, mode os.FileMode) {
				t.Helper()
				p := filepath.Join(root, name)
				if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte(value), mode); err != nil {
					t.Fatal(err)
				}
			}
			for name, value := range map[string]string{
				"version": "1\n", "instance-id": "12345678-1234-4234-8234-123456789abc/87654321-4321-4321-8321-cba987654321\n",
				"hostname": "node-1\n", "mac": "02:00:00:12:34:56\n", "address": "10.240.12.10\n",
				"prefix": "24\n", "gateway": "10.240.12.1\n", "username": "devops\n",
				"dns": "192.0.2.53\n", "authorized_keys": "ssh-ed25519 fixture\n",
			} {
				write("run/flintyard/seed/"+name, value, 0600)
			}
			write("etc/flintyard/username", "devops\n", 0400)
			write("etc/flintyard/profile", profile+"\n", 0400)
			write("etc/flintyard/ssh-port", "22\n", 0400)
			write("sys/class/net/test0/address", "02:00:00:12:34:56\n", 0444)
			write("proc/sys/kernel/random/uuid", "12345678-1234-4234-8234-123456789abc\n", 0444)
			write("etc/passwd", fmt.Sprintf("devops:x:%d:%d::%s:/bin/sh\n", os.Getuid(), os.Getgid(), filepath.Join(root, "home/devops")), 0644)
			for name, body := range map[string]string{
				"blkid": "printf '/dev/fixture-seed\\n'\n", "mount": "exit 0\n", "umount": "exit 0\n",
				"hostname": "exit 0\n", "ssh-keygen": "exit 0\n",
				// Reproduce a concurrent tmpfiles writer immediately before symlink creation.
				"ln": "printf 'competing resolver\\n' > \"$FY_TEST_ROOT/etc/resolv.conf\"\nexec /bin/ln \"$@\"\n",
			} {
				write("bin/"+name, "#!/bin/sh\n"+body, 0755)
			}
			replacer := strings.NewReplacer(
				"/run/flintyard", `"${FY_TEST_ROOT}"/run/flintyard`,
				"/var/lib/flintyard", `"${FY_TEST_ROOT}"/var/lib/flintyard`,
				"/etc/flintyard", `"${FY_TEST_ROOT}"/etc/flintyard`,
				"/sys/class/net", `"${FY_TEST_ROOT}"/sys/class/net`,
				"/proc/sys/kernel/random/uuid", `"${FY_TEST_ROOT}"/proc/sys/kernel/random/uuid`,
				"/etc/systemd/network", `"${FY_TEST_ROOT}"/etc/systemd/network`,
				"/etc/NetworkManager", `"${FY_TEST_ROOT}"/etc/NetworkManager`,
				"/etc/network", `"${FY_TEST_ROOT}"/etc/network`,
				"/etc/hostname", `"${FY_TEST_ROOT}"/etc/hostname`,
				"/etc/machine-id", `"${FY_TEST_ROOT}"/etc/machine-id`,
				"/etc/resolv.conf", `"${FY_TEST_ROOT}"/etc/resolv.conf`,
				"/etc/passwd", `"${FY_TEST_ROOT}"/etc/passwd`,
				"/run/sshd", `"${FY_TEST_ROOT}"/run/sshd`,
			)
			cmd := exec.Command("/bin/sh", "-eu", "-c", replacer.Replace(seedAdapter))
			cmd.Env = []string{"FY_TEST_ROOT=" + root, "PATH=" + filepath.Join(root, "bin") + ":/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C"}
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("seed script failed: %v: %s", err, out)
			}
			modes := map[string]os.FileMode{
				"run/flintyard": 0755, "run/flintyard/seed": 0700, "run/flintyard/resolv.conf": 0644,
				"etc/hostname": 0644, "home/devops/.ssh": 0700, "home/devops/.ssh/authorized_keys": 0600,
			}
			switch profile {
			case "systemd-networkd":
				modes["etc/systemd/network"] = 0755
				modes["etc/systemd/network/00-flintyard.network"] = 0644
			case "networkmanager":
				modes["etc/NetworkManager/system-connections/flintyard.nmconnection"] = 0600
			case "openrc-ifupdown":
				modes["etc/network"] = 0755
				modes["etc/network/interfaces"] = 0644
			}
			for name, want := range modes {
				st, err := os.Stat(filepath.Join(root, name))
				if err != nil || st.Mode().Perm() != want {
					t.Fatalf("%s: want %s, got %v, %v", name, strconv.FormatUint(uint64(want), 8), st, err)
				}
			}
			if b, err := os.ReadFile(filepath.Join(root, "etc/resolv.conf")); err != nil || string(b) != "nameserver 192.0.2.53\n" {
				t.Fatal("resolver configuration unavailable", string(b), err)
			}
			if leftovers, err := filepath.Glob(filepath.Join(root, "etc/resolv.conf.flintyard.*")); err != nil || len(leftovers) != 0 {
				t.Fatal("temporary resolver links remain", leftovers, err)
			}
			if profile == "openrc-ifupdown" {
				b, err := os.ReadFile(filepath.Join(root, "etc/network/interfaces"))
				if err != nil || !strings.Contains(string(b), "iface test0 inet static\n") || strings.Contains(string(b), "source ") {
					t.Fatal("ifupdown configuration is not self-contained", string(b), err)
				}
			}
			if profile == "networkmanager" {
				b, err := os.ReadFile(filepath.Join(root, "etc/NetworkManager/system-connections/flintyard.nmconnection"))
				if err != nil || !strings.Contains(string(b), "[ethernet]\nmac-address=02:00:00:12:34:56\n") || strings.Contains(string(b), "interface-name=") {
					t.Fatalf("NetworkManager connection must survive interface renaming: %s, %v", b, err)
				}
			}
		})
	}
}
