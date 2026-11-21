package image

import (
	"fmt"
	"github.com/LeeXN/Flintyard/internal/contract"
	"regexp"
	"strings"
	"unicode"
)

type OS struct {
	Family  string `json:"family"`
	Release string `json:"release"`
}
type Boot struct {
	Kernel          string   `json:"kernel_path"`
	Initrd          string   `json:"initrd_path,omitempty"`
	Rootfs          string   `json:"rootfs_path"`
	RootDevice      string   `json:"root_device"`
	Args            []string `json:"kernel_args"`
	VirtioTransport string   `json:"virtio_transport,omitempty"`
}
type Kernel struct {
	Release string `json:"release"`
	Origin  string `json:"origin"`
	Modules string `json:"modules"`
	Config  string `json:"config_sha256,omitempty"`
}
type Probe struct {
	Type string `json:"type"`
	Port int    `json:"port"`
}
type Initialization struct {
	Mode     string `json:"mode"`
	Profile  string `json:"profile,omitempty"`
	Username string `json:"username"`
	Probe    Probe  `json:"probe"`
}
type Source struct {
	Kind   string `json:"kind"`
	Source string `json:"source"`
	SHA256 string `json:"sha256"`
}
type Provenance struct {
	Inputs          []Source     `json:"inputs"`
	BuiltAt         string       `json:"built_at"`
	Recipe          string       `json:"recipe_sha256,omitempty"`
	Reproducibility string       `json:"reproducibility"`
	Builder         string       `json:"builder"`
	KernelBuild     *KernelBuild `json:"kernel_build,omitempty"`
}

// KernelBuild records the actual source build, including ordered patches and
// fixed argv. Config normalization is deliberately unsupported in v0.1.
type KernelBuild struct {
	Compiler        string     `json:"compiler"`
	Linker          string     `json:"linker"`
	Make            string     `json:"make"`
	ConfigSHA256    string     `json:"config_sha256"`
	ConfigUnchanged bool       `json:"config_unchanged"`
	Patches         []string   `json:"patches"`
	Commands        [][]string `json:"commands"`
}
type Payload struct {
	Path   string `json:"path"`
	Role   string `json:"role"`
	Size   int64  `json:"size_bytes"`
	SHA256 string `json:"sha256"`
}
type Manifest struct {
	APIVersion     string         `json:"api_version"`
	Kind           string         `json:"kind"`
	Name           string         `json:"name"`
	Version        string         `json:"version"`
	Architecture   string         `json:"architecture"`
	OS             OS             `json:"os"`
	Boot           Boot           `json:"boot"`
	Kernel         Kernel         `json:"kernel"`
	Initialization Initialization `json:"initialization"`
	Provenance     Provenance     `json:"provenance"`
	Payloads       []Payload      `json:"payloads"`
}
type Input struct {
	Role   string `json:"role"`
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}
type Spec struct {
	APIVersion     string         `json:"api_version"`
	Kind           string         `json:"kind"`
	Name           string         `json:"name"`
	Version        string         `json:"version"`
	Architecture   string         `json:"architecture"`
	OS             OS             `json:"os"`
	Boot           Boot           `json:"boot"`
	Kernel         Kernel         `json:"kernel"`
	Initialization Initialization `json:"initialization"`
	Inputs         []Input        `json:"inputs"`
}

func DigestValid(s string) bool {
	ok, _ := regexp.MatchString("^[0-9a-f]{64}$", s)
	return ok && s != strings.Repeat("0", 64)
}

// ValidateVirtioTransport preserves the historical MMIO default and rejects
// a PCI declaration that the guest's own command line would disable.
func ValidateVirtioTransport(transport string, args []string) error {
	if transport != "" && transport != "mmio" && transport != "pci" {
		return fmt.Errorf("virtio_transport must be mmio or pci")
	}
	if transport == "pci" {
		for _, arg := range args {
			if strings.HasPrefix(arg, "pci=") {
				for _, option := range strings.Split(strings.TrimPrefix(arg, "pci="), ",") {
					if option == "off" {
						return fmt.Errorf("pci=off conflicts with pci virtio transport")
					}
				}
			}
		}
	}
	return nil
}

func (m Manifest) Validate() error {
	if err := ValidateVirtioTransport(m.Boot.VirtioTransport, m.Boot.Args); err != nil {
		return err
	}
	if !strings.HasPrefix(m.Boot.RootDevice, "/dev/") || strings.ContainsFunc(m.Boot.RootDevice, func(r rune) bool { return unicode.IsControl(r) || unicode.IsSpace(r) }) {
		return fmt.Errorf("root_device must be one /dev/ path without whitespace or controls")
	}
	roles := map[string]string{"kernel": m.Boot.Kernel, "rootfs": m.Boot.Rootfs}
	if m.Boot.Initrd != "" {
		roles["initrd"] = m.Boot.Initrd
	}
	seen := map[string]bool{}
	for _, p := range m.Payloads {
		if e := contract.Path(p.Path); e != nil {
			return e
		}
		if !strings.HasPrefix(p.Path, "payload/") || roles[p.Role] != p.Path || seen[p.Role] {
			return fmt.Errorf("payload role/path mismatch or duplicate")
		}
		seen[p.Role] = true
		if p.Size < 1 || p.Size > 64<<30 || !DigestValid(p.SHA256) {
			return fmt.Errorf("invalid payload size or digest")
		}
	}
	if len(seen) != len(roles) {
		return fmt.Errorf("missing boot payload")
	}
	if m.Kernel.Config != "" && !DigestValid(m.Kernel.Config) {
		return fmt.Errorf("placeholder config digest")
	}
	if p := m.Provenance.KernelBuild; p != nil {
		if !p.ConfigUnchanged || p.ConfigSHA256 != m.Kernel.Config || !DigestValid(p.ConfigSHA256) {
			return fmt.Errorf("kernel build provenance does not match final configuration")
		}
		for _, sum := range p.Patches {
			if !DigestValid(sum) {
				return fmt.Errorf("invalid kernel patch digest")
			}
		}
	}
	for _, s := range m.Provenance.Inputs {
		if !DigestValid(s.SHA256) {
			return fmt.Errorf("placeholder provenance digest")
		}
	}
	if m.Provenance.Recipe != "" && !DigestValid(m.Provenance.Recipe) {
		return fmt.Errorf("placeholder recipe digest")
	}
	n := len(m.Boot.RootDevice) + 6
	for _, a := range m.Boot.Args {
		if strings.HasPrefix(a, "root=") || strings.ContainsFunc(a, unicode.IsControl) || strings.ContainsAny(a, " \t") {
			return fmt.Errorf("kernel args must be single tokens without root= or controls")
		}
		n += len(a) + 1
	}
	if n > 4095 {
		return fmt.Errorf("kernel command line too long")
	}
	return nil
}
