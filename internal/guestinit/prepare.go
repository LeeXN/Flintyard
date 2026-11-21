package guestinit

import (
	"fmt"
	"os"
	"path"
	"strings"

	"github.com/LeeXN/Flintyard/internal/image"
)

// Resolve maps guest absolute symlinks into the private root instead of the
// host's /. It is used for offline preparation, never for archive writes.
func Resolve(root *os.Root, name string, missing bool) (string, error) {
	if !strings.HasPrefix(name, "/") || strings.ContainsRune(name, 0) {
		return "", fmt.Errorf("guest path must be absolute")
	}
	pending := strings.Split(strings.TrimPrefix(name, "/"), "/")
	var done []string
	links := 0
	for len(pending) > 0 {
		part := pending[0]
		pending = pending[1:]
		if part == "" || part == "." {
			continue
		}
		if part == ".." {
			if len(done) > 0 {
				done = done[:len(done)-1]
			}
			continue
		}
		candidate := strings.Join(append(append([]string(nil), done...), part), "/")
		st, err := root.Lstat(candidate)
		if os.IsNotExist(err) && missing {
			done = append(done, part)
			for _, p := range pending {
				if p == ".." {
					return "", fmt.Errorf("missing guest path contains parent traversal")
				}
			}
			return path.Join(append(done, pending...)...), nil
		}
		if err != nil {
			return "", err
		}
		if st.Mode()&os.ModeSymlink != 0 {
			links++
			if links > 40 {
				return "", fmt.Errorf("guest symlink loop")
			}
			target, err := root.Readlink(candidate)
			if err != nil {
				return "", err
			}
			if strings.HasPrefix(target, "/") {
				done = nil
			}
			pending = append(strings.Split(strings.TrimPrefix(target, "/"), "/"), pending...)
			continue
		}
		if len(pending) > 0 && !st.IsDir() {
			return "", fmt.Errorf("guest path traverses a non-directory")
		}
		done = append(done, part)
	}
	if len(done) == 0 {
		return ".", nil
	}
	return strings.Join(done, "/"), nil
}
func WriteGuest(root *os.Root, name string, data []byte, mode os.FileMode) error {
	parent, err := Resolve(root, path.Dir(name), true)
	if err != nil {
		return err
	}
	if err = root.MkdirAll(parent, 0755); err != nil {
		return err
	}
	file := path.Join(parent, path.Base(name))
	if st, e := root.Lstat(file); e == nil {
		if st.IsDir() {
			return fmt.Errorf("guest output collides with directory")
		}
		if e = root.Remove(file); e != nil {
			return e
		}
	} else if !os.IsNotExist(e) {
		return e
	}
	f, err := root.OpenFile(file, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Chmod(mode)
	}
	ce := f.Close()
	if err != nil {
		return err
	}
	return ce
}
func guestRead(root *os.Root, name string) ([]byte, error) {
	p, err := Resolve(root, name, false)
	if err != nil {
		return nil, err
	}
	return root.ReadFile(p)
}
func findGuest(root *os.Root, names ...string) (string, error) {
	for _, name := range names {
		p, err := Resolve(root, name, false)
		if err == nil {
			st, err := root.Stat(p)
			if err == nil && st.Mode().IsRegular() && st.Mode().Perm()&0111 != 0 {
				return name, nil
			}
		}
	}
	return "", fmt.Errorf("missing executable guest dependency %s", strings.Join(names, " or "))
}

type PrepareRequest struct {
	Root           string
	Initialization image.Initialization
}

func Prepare(r PrepareRequest) error {
	if r.Initialization.Mode == "external" {
		return nil
	}
	i := r.Initialization
	if i.Mode != "seed-v1" || !userPattern.MatchString(i.Username) || i.Probe.Port < 1 || i.Probe.Port > 65535 {
		return fmt.Errorf("invalid guest preparation")
	}
	if i.Profile != "systemd-networkd" && i.Profile != "networkmanager" && i.Profile != "openrc-ifupdown" {
		return fmt.Errorf("unknown guest profile")
	}
	root, err := os.OpenRoot(r.Root)
	if err != nil {
		return err
	}
	defer root.Close()
	for _, names := range [][]string{{"/bin/sh", "/usr/bin/sh"}, {"/bin/mount", "/usr/bin/mount"}, {"/sbin/blkid", "/usr/sbin/blkid", "/bin/blkid"}, {"/usr/sbin/sshd", "/sbin/sshd"}, {"/usr/bin/ssh-keygen", "/bin/ssh-keygen"}, {"/usr/bin/awk", "/bin/awk"}, {"/usr/bin/install", "/bin/install"}, {"/usr/bin/readlink", "/bin/readlink"}, {"/usr/bin/tr", "/bin/tr"}, {"/usr/bin/cat", "/bin/cat"}} {
		if _, err = findGuest(root, names...); err != nil {
			return err
		}
	}
	for _, tool := range []string{"hostname", "sleep", "mkdir", "chmod", "rm", "ln", "mv", "umount"} {
		if _, err = findGuest(root, "/bin/"+tool, "/usr/bin/"+tool, "/sbin/"+tool, "/usr/sbin/"+tool); err != nil {
			return err
		}
	}
	passwd, err := guestRead(root, "/etc/passwd")
	if err != nil {
		return err
	}
	found := false
	for _, line := range strings.Split(string(passwd), "\n") {
		f := strings.Split(line, ":")
		if len(f) == 7 && f[0] == i.Username {
			if !strings.HasPrefix(f[5], "/") || f[6] == "/sbin/nologin" || f[6] == "/usr/sbin/nologin" || f[6] == "/bin/false" {
				return fmt.Errorf("guest login account has no usable home/shell")
			}
			found = true
		}
	}
	if !found {
		return fmt.Errorf("declared guest login account does not exist")
	}
	usePAM := false
	if existing, e := guestRead(root, "/etc/ssh/sshd_config"); e == nil {
		for _, line := range strings.Split(string(existing), "\n") {
			fields := strings.Fields(line)
			if len(fields) >= 2 && strings.EqualFold(fields[0], "UsePAM") {
				usePAM = strings.EqualFold(fields[1], "yes")
				break
			}
		}
	} else if !os.IsNotExist(e) {
		return e
	}
	if !usePAM {
		if shadow, e := guestRead(root, "/etc/shadow"); e == nil {
			for _, line := range strings.Split(string(shadow), "\n") {
				fields := strings.Split(line, ":")
				if len(fields) >= 2 && fields[0] == i.Username && (strings.HasPrefix(fields[1], "!") || strings.HasPrefix(fields[1], "*")) {
					return fmt.Errorf("login account is locked without PAM; explicitly prepare public-key access in a recipe step")
				}
			}
		} else if !os.IsNotExist(e) {
			return e
		}
	}
	sshConfig := fmt.Sprintf("Port %d\nPubkeyAuthentication yes\nPasswordAuthentication no\nKbdInteractiveAuthentication no\nPermitEmptyPasswords no\nPermitRootLogin prohibit-password\nAuthorizedKeysFile .ssh/authorized_keys\n", i.Probe.Port)
	if usePAM {
		sshConfig += "UsePAM yes\n"
	}
	if err = WriteGuest(root, "/etc/ssh/sshd_config", []byte(sshConfig), 0600); err != nil {
		return err
	}
	if i.Profile == "openrc-ifupdown" {
		for _, file := range []string{"/sbin/openrc-run", "/etc/init.d/networking", "/etc/init.d/sshd"} {
			if _, err = findGuest(root, file); err != nil {
				return err
			}
		}
		if _, err = findGuest(root, "/sbin/ifup", "/usr/sbin/ifup"); err != nil {
			return err
		}
	} else {
		if _, err = findGuest(root, "/bin/systemctl", "/usr/bin/systemctl"); err != nil {
			return err
		}
	}
	if i.Profile == "networkmanager" {
		if _, err = findGuest(root, "/usr/sbin/NetworkManager", "/sbin/NetworkManager"); err != nil {
			return err
		}
	}
	if i.Profile == "systemd-networkd" {
		if _, err = findGuest(root, "/usr/lib/systemd/systemd-networkd", "/lib/systemd/systemd-networkd"); err != nil {
			return err
		}
	}
	for name, data := range map[string][]byte{"/usr/lib/flintyard/seed-init": []byte(seedAdapter), "/etc/flintyard/profile": []byte(i.Profile + "\n"), "/etc/flintyard/username": []byte(i.Username + "\n"), "/etc/flintyard/ssh-port": []byte(fmt.Sprint(i.Probe.Port) + "\n")} {
		mode := os.FileMode(0400)
		if name == "/usr/lib/flintyard/seed-init" {
			mode = 0755
		}
		if err = WriteGuest(root, name, data, mode); err != nil {
			return err
		}
	}
	if i.Profile == "openrc-ifupdown" {
		if err = WriteGuest(root, "/etc/init.d/flintyard-seed", []byte(openrcUnit), 0755); err != nil {
			return err
		}
		if err = guestLink(root, "/etc/runlevels/boot/flintyard-seed", "/etc/init.d/flintyard-seed"); err != nil {
			return err
		}
		if err = guestLink(root, "/etc/runlevels/default/networking", "/etc/init.d/networking"); err != nil {
			return err
		}
		if err = guestLink(root, "/etc/runlevels/default/sshd", "/etc/init.d/sshd"); err != nil {
			return err
		}
	} else {
		if err = WriteGuest(root, "/etc/systemd/system/flintyard-seed.service", []byte(systemdUnit), 0644); err != nil {
			return err
		}
		if err = guestLink(root, "/etc/systemd/system/multi-user.target.wants/flintyard-seed.service", "/etc/systemd/system/flintyard-seed.service"); err != nil {
			return err
		}
		if i.Profile == "systemd-networkd" {
			if err = guestLink(root, "/etc/systemd/system/NetworkManager.service", "/dev/null"); err != nil {
				return err
			}
			if err = enableGuestUnit(root, "systemd-networkd.service"); err != nil {
				return err
			}
		} else {
			if err = guestLink(root, "/etc/systemd/system/systemd-networkd.service", "/dev/null"); err != nil {
				return err
			}
			if err = enableGuestUnit(root, "NetworkManager.service"); err != nil {
				return err
			}
		}
		if err = enableGuestUnit(root, "ssh.service"); err != nil {
			if err = enableGuestUnit(root, "sshd.service"); err != nil {
				return err
			}
		}
	}
	configDir := "/etc/systemd/network"
	if i.Profile == "networkmanager" {
		configDir = "/etc/NetworkManager/system-connections"
	}
	if i.Profile == "openrc-ifupdown" {
		configDir = "/etc/network/interfaces.d"
	}
	if p, e := Resolve(root, configDir, false); e == nil {
		entries, e := os.ReadDir(path.Join(root.Name(), p))
		if e != nil {
			return e
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			if e = root.Remove(path.Join(p, entry.Name())); e != nil {
				return e
			}
		}
	} else if !os.IsNotExist(e) {
		return e
	}
	return clearIdentities(root)
}
func enableGuestUnit(root *os.Root, name string) error {
	for _, base := range []string{"/usr/lib/systemd/system/", "/lib/systemd/system/", "/etc/systemd/system/"} {
		if _, err := Resolve(root, base+name, false); err == nil {
			return guestLink(root, "/etc/systemd/system/multi-user.target.wants/"+name, base+name)
		}
	}
	return fmt.Errorf("required guest service unit %s is absent", name)
}
func guestLink(root *os.Root, name, target string) error {
	parent, err := Resolve(root, path.Dir(name), true)
	if err != nil {
		return err
	}
	if err = root.MkdirAll(parent, 0755); err != nil {
		return err
	}
	file := path.Join(parent, path.Base(name))
	if err = root.Remove(file); err != nil && !os.IsNotExist(err) {
		return err
	}
	return root.Symlink(target, file)
}
func clearIdentities(root *os.Root) error {
	for _, file := range []string{"/etc/machine-id", "/var/lib/dbus/machine-id"} {
		if _, err := Resolve(root, path.Dir(file), false); os.IsNotExist(err) {
			continue
		}
		if err := WriteGuest(root, file, nil, 0444); err != nil {
			return err
		}
	}
	if p, err := Resolve(root, "/etc/ssh", false); err == nil {
		entries, err := os.ReadDir(path.Join(root.Name(), p))
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), "ssh_host_") && (strings.HasSuffix(entry.Name(), "_key") || strings.HasSuffix(entry.Name(), "_key.pub")) {
				if err = root.Remove(path.Join(p, entry.Name())); err != nil {
					return err
				}
			}
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	for _, dir := range []string{"/var/lib/dhcp", "/var/lib/dhclient", "/var/lib/NetworkManager", "/var/lib/systemd/network"} {
		p, err := Resolve(root, dir, false)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		entries, err := os.ReadDir(path.Join(root.Name(), p))
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if strings.Contains(entry.Name(), "lease") {
				if err = root.RemoveAll(path.Join(p, entry.Name())); err != nil {
					return err
				}
			}
		}
	}
	for _, file := range []string{"/root/.bash_history", "/root/.ash_history"} {
		p, err := Resolve(root, file, false)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if err = root.Remove(p); err != nil {
			return err
		}
	}
	return nil
}

const systemdUnit = `[Unit]
Description=Flintyard seed-v1 initialization
DefaultDependencies=no
After=local-fs.target systemd-udev-settle.service systemd-tmpfiles-setup.service
Before=network-pre.target systemd-networkd.service NetworkManager.service ssh.service sshd.service
Wants=network-pre.target
[Service]
Type=oneshot
ExecStart=/usr/lib/flintyard/seed-init
RemainAfterExit=yes
[Install]
WantedBy=multi-user.target
`
const openrcUnit = `#!/sbin/openrc-run
description="Flintyard seed-v1 initialization"
depend() { need localmount; before networking sshd; }
start() { /usr/lib/flintyard/seed-init; }
`
