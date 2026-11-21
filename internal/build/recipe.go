// Package build implements local, isolated image construction, without a daemon.
package build

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"

	"github.com/LeeXN/Flintyard/internal/contract"
	"github.com/LeeXN/Flintyard/internal/host"
	"github.com/LeeXN/Flintyard/internal/image"
)

type Source struct {
	Path   string `json:"path,omitempty"`
	URL    string `json:"url,omitempty"`
	SHA256 string `json:"sha256"`
}
type Rootfs struct {
	Kind    string `json:"kind"`
	Source  Source `json:"source"`
	SizeMiB int    `json:"size_mib,omitempty"`
}
type Patch struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}
type Kernel struct {
	Mode            string  `json:"mode"`
	Source          Source  `json:"source"`
	Release         string  `json:"release,omitempty"`
	Origin          string  `json:"origin"`
	Modules         string  `json:"modules,omitempty"`
	StripComponents int     `json:"strip_components,omitempty"`
	ConfigFile      string  `json:"config_file,omitempty"`
	ConfigSHA256    string  `json:"config_sha256,omitempty"`
	Patches         []Patch `json:"patches,omitempty"`
	ImageTarget     string  `json:"image_target,omitempty"`
	Jobs            int     `json:"jobs,omitempty"`
	TimeoutSeconds  int     `json:"timeout_seconds,omitempty"`
	ExpectedRelease string  `json:"expected_release,omitempty"`
}
type Step struct {
	Argv           []string          `json:"argv"`
	WorkingDir     string            `json:"working_dir,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	TimeoutSeconds int               `json:"timeout_seconds"`
}
type Output struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Path    string `json:"path"`
}
type Recipe struct {
	APIVersion      string   `json:"api_version"`
	Kind            string   `json:"kind"`
	Architecture    string   `json:"architecture"`
	OS              image.OS `json:"os"`
	Rootfs          Rootfs   `json:"rootfs"`
	Kernel          Kernel   `json:"kernel"`
	Initrd          *Source  `json:"initrd,omitempty"`
	GeneratedInitrd string   `json:"generated_initrd_path,omitempty"`
	Boot            struct {
		RootDevice      string   `json:"root_device"`
		Args            []string `json:"kernel_args"`
		VirtioTransport string   `json:"virtio_transport,omitempty"`
	} `json:"boot"`
	Steps           []Step               `json:"steps"`
	Guest           image.Initialization `json:"guest_prepare"`
	Requires        []string             `json:"requires"`
	Network         bool                 `json:"network"`
	Reproducibility string               `json:"reproducibility"`
	Output          Output               `json:"output"`
}
type RecipeResult struct {
	Recipe Recipe
	SHA256 string
	Base   string
}

func LoadRecipe(ctx context.Context, file string) (RecipeResult, error) {
	var result RecipeResult
	if err := ctx.Err(); err != nil {
		return result, err
	}
	absolute, err := filepath.Abs(file)
	if err != nil {
		return result, err
	}
	f, err := os.Open(absolute)
	if err != nil {
		return result, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, contract.MaxConfig+1))
	if err != nil {
		return result, err
	}
	if err = contract.Validate("recipe", b); err != nil {
		return result, err
	}
	if err = contract.Decode(b, &result.Recipe); err != nil {
		return result, err
	}
	result.SHA256 = image.Sum(b)
	result.Base = filepath.Dir(absolute)
	resolve := func(s *Source) error {
		if !image.DigestValid(s.SHA256) {
			return fmt.Errorf("all build inputs require non-placeholder SHA-256")
		}
		if s.Path != "" && !filepath.IsAbs(s.Path) {
			s.Path = filepath.Join(result.Base, s.Path)
		}
		if s.URL != "" {
			u, err := url.Parse(s.URL)
			if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" {
				return fmt.Errorf("input URL must be HTTPS without userinfo/fragment")
			}
		}
		return nil
	}
	r := &result.Recipe
	if r.Architecture != host.Architecture() {
		return result, fmt.Errorf("recipe architecture %s differs from native host %s", r.Architecture, host.Architecture())
	}
	if err = resolve(&r.Rootfs.Source); err != nil {
		return result, err
	}
	if err = resolve(&r.Kernel.Source); err != nil {
		return result, err
	}
	if r.Initrd != nil {
		if err = resolve(r.Initrd); err != nil {
			return result, err
		}
	}
	if r.Kernel.Mode == "source" {
		if !releaseName.MatchString(r.Kernel.ExpectedRelease) {
			return result, fmt.Errorf("invalid expected kernel release")
		}
		if !image.DigestValid(r.Kernel.ConfigSHA256) {
			return result, fmt.Errorf("kernel config needs a locked non-placeholder digest")
		}
		if !filepath.IsAbs(r.Kernel.ConfigFile) {
			r.Kernel.ConfigFile = filepath.Join(result.Base, r.Kernel.ConfigFile)
		}
		for i := range r.Kernel.Patches {
			p := &r.Kernel.Patches[i]
			if !image.DigestValid(p.SHA256) {
				return result, fmt.Errorf("patch digest is not locked")
			}
			if !filepath.IsAbs(p.Path) {
				p.Path = filepath.Join(result.Base, p.Path)
			}
		}
		if r.Architecture == "x86_64" && r.Kernel.ImageTarget == "Image" || r.Architecture == "aarch64" && r.Kernel.ImageTarget != "Image" {
			return result, fmt.Errorf("kernel image target does not match architecture")
		}
	}
	if r.Initrd != nil && r.GeneratedInitrd != "" {
		return result, fmt.Errorf("initrd input and generated path are mutually exclusive")
	}
	if r.GeneratedInitrd != "" && !guestAbsolute(r.GeneratedInitrd) {
		return result, fmt.Errorf("invalid generated initrd guest path")
	}
	if !strings.HasPrefix(r.Boot.RootDevice, "/dev/") || strings.ContainsFunc(r.Boot.RootDevice, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
		return result, fmt.Errorf("invalid root device")
	}
	if err := image.ValidateVirtioTransport(r.Boot.VirtioTransport, r.Boot.Args); err != nil {
		return result, err
	}
	length := len(r.Boot.RootDevice) + 5
	for _, arg := range r.Boot.Args {
		if strings.HasPrefix(arg, "root=") || strings.ContainsFunc(arg, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
			return result, fmt.Errorf("kernel args must be individual tokens without root=")
		}
		length += len(arg) + 1
	}
	if length > 4095 {
		return result, fmt.Errorf("kernel command line exceeds pinned limit")
	}
	envName := regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	for i := range r.Steps {
		s := &r.Steps[i]
		if s.WorkingDir == "" {
			s.WorkingDir = "/"
		}
		if !guestAbsolute(s.WorkingDir) || s.Argv[0] == "" {
			return result, fmt.Errorf("invalid step working directory/program")
		}
		for _, arg := range s.Argv {
			if strings.ContainsRune(arg, 0) {
				return result, fmt.Errorf("NUL in step argv")
			}
		}
		for name, value := range s.Env {
			if !envName.MatchString(name) || strings.ContainsRune(value, 0) {
				return result, fmt.Errorf("invalid step environment")
			}
		}
	}
	if !filepath.IsAbs(r.Output.Path) {
		r.Output.Path = filepath.Join(result.Base, r.Output.Path)
	}
	return result, nil
}
func guestAbsolute(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path && !strings.ContainsRune(path, 0)
}
