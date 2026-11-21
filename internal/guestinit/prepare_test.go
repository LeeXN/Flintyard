package guestinit

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LeeXN/Flintyard/internal/image"
)

func preparationFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{"etc/passwd": "root:x:0:0:root:/root:/bin/sh\n", "etc/shadow": "root::1:0:99999:7:::\n", "etc/machine-id": "SHARED-IMAGE-ID\n", "etc/ssh/ssh_host_ed25519_key": "PRIVATE-SHARED-KEY", "etc/ssh/ssh_host_ed25519_key.pub": "PUBLIC-SHARED-KEY", "etc/customer-application": "retain customer data", "var/lib/dbus/machine-id": "SHARED-IMAGE-ID\n", "var/lib/dhcp/client.lease": "old lease", "root/.bash_history": "old commands", "etc/systemd/network/old.network": "old host identity", "etc/NetworkManager/system-connections/old.nmconnection": "old connection ID", "etc/network/interfaces.d/old": "old interface"}
	for _, bin := range []string{"sh", "mount", "umount", "hostname", "sleep", "mkdir", "chmod", "rm", "ln", "mv", "blkid", "sshd", "ssh-keygen", "awk", "install", "readlink", "tr", "cat", "systemctl", "ifup"} {
		files["usr/bin/"+bin] = "#!/bin/sh\nexit 0\n"
	}
	files["usr/sbin/NetworkManager"] = "#!/bin/sh\nexit 0\n"
	files["usr/sbin/sshd"] = "#!/bin/sh\nexit 0\n"
	files["usr/sbin/ifup"] = "#!/bin/sh\nexit 0\n"
	files["usr/lib/systemd/systemd-networkd"] = "#!/bin/sh\nexit 0\n"
	files["sbin/openrc-run"] = "#!/bin/sh\nexit 0\n"
	files["etc/init.d/networking"] = "#!/bin/sh\nexit 0\n"
	files["etc/init.d/sshd"] = "#!/bin/sh\nexit 0\n"
	for _, unit := range []string{"NetworkManager.service", "systemd-networkd.service", "ssh.service"} {
		files["usr/lib/systemd/system/"+unit] = "[Service]\n"
	}
	for name, content := range files {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("usr/bin", filepath.Join(root, "bin")); err != nil {
		t.Fatal(err)
	}
	return root
}
func TestPrepareThreeProfilesInstallsDataOnlyAdapterAndClearsOnlyIdentities(t *testing.T) {
	for _, profile := range []string{"systemd-networkd", "networkmanager", "openrc-ifupdown"} {
		t.Run(profile, func(t *testing.T) {
			root := preparationFixture(t)
			err := Prepare(PrepareRequest{Root: root, Initialization: image.Initialization{Mode: "seed-v1", Profile: profile, Username: "root", Probe: image.Probe{Type: "ssh_banner", Port: 2222}}})
			if err != nil {
				t.Fatal(err)
			}
			if b, err := os.ReadFile(filepath.Join(root, "etc/machine-id")); err != nil || len(b) != 0 {
				t.Fatal("shared machine identity retained", err)
			}
			for _, name := range []string{"etc/ssh/ssh_host_ed25519_key", "etc/ssh/ssh_host_ed25519_key.pub", "var/lib/dhcp/client.lease", "root/.bash_history"} {
				if _, err := os.Stat(filepath.Join(root, name)); !os.IsNotExist(err) {
					t.Fatal("shared identity/history retained", name, err)
				}
			}
			if b, err := os.ReadFile(filepath.Join(root, "etc/customer-application")); err != nil || string(b) != "retain customer data" {
				t.Fatal("application data changed", err)
			}
			config, _ := os.ReadFile(filepath.Join(root, "etc/ssh/sshd_config"))
			if !strings.Contains(string(config), "Port 2222") || !strings.Contains(string(config), "PasswordAuthentication no") {
				t.Fatal(string(config))
			}
			script := filepath.Join(root, "usr/lib/flintyard/seed-init")
			if out, err := exec.Command("/bin/sh", "-n", script).CombinedOutput(); err != nil {
				t.Fatal(err, string(out))
			}
			if strings.Contains(seedAdapter, "eval ") || strings.Contains(seedAdapter, ". \"$seed/") {
				t.Fatal("adapter interprets seed commands")
			}
		})
	}
}
func TestGuestAbsoluteSymlinksStayInRootAndMissingUsersFail(t *testing.T) {
	dir := preparationFixture(t)
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err = root.Symlink("/usr/bin/sh", "absolute"); err != nil {
		t.Fatal(err)
	}
	got, err := Resolve(root, "/absolute", false)
	if err != nil || got != "usr/bin/sh" {
		t.Fatal(got, err)
	}
	if err = Prepare(PrepareRequest{Root: dir, Initialization: image.Initialization{Mode: "seed-v1", Profile: "networkmanager", Username: "missing", Probe: image.Probe{Type: "tcp", Port: 22}}}); err == nil {
		t.Fatal("created undeclared login user")
	}
}

func TestLockedGuestAccountRequiresExplicitPAMAndKeepsPasswordLoginDisabled(t *testing.T) {
	for _, usePAM := range []bool{false, true} {
		t.Run(map[bool]string{false: "without-pam", true: "with-pam"}[usePAM], func(t *testing.T) {
			root := preparationFixture(t)
			shadow := filepath.Join(root, "etc/shadow")
			const locked = "root:!!:1:0:99999:7:::\n"
			if err := os.WriteFile(shadow, []byte(locked), 0600); err != nil {
				t.Fatal(err)
			}
			if usePAM {
				if err := os.WriteFile(filepath.Join(root, "etc/ssh/sshd_config"), []byte("UsePAM yes\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			err := Prepare(PrepareRequest{Root: root, Initialization: image.Initialization{Mode: "seed-v1", Profile: "networkmanager", Username: "root", Probe: image.Probe{Type: "ssh_banner", Port: 22}}})
			if !usePAM {
				if err == nil || !strings.Contains(err.Error(), "locked without PAM") {
					t.Fatal("expected locked account rejection", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			config, err := os.ReadFile(filepath.Join(root, "etc/ssh/sshd_config"))
			if err != nil || !strings.Contains(string(config), "UsePAM yes\n") || !strings.Contains(string(config), "PasswordAuthentication no\n") || !strings.Contains(string(config), "KbdInteractiveAuthentication no\n") {
				t.Fatal("unsafe guest SSH configuration", string(config), err)
			}
			if got, err := os.ReadFile(shadow); err != nil || string(got) != locked {
				t.Fatal("guest account credentials changed", string(got), err)
			}
		})
	}
}
