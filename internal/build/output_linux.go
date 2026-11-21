package build

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/LeeXN/Flintyard/internal/host"
)

// Build storage is disposable working space, never a publication destination.
// Resolve symlinks and the current namespace's filesystem roots so a bind alias
// cannot turn a user output into another build's GC scratch.
func checkBuildOutput(root, path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return fmt.Errorf("build output must resolve to a canonical absolute path")
	}
	resolved, err := resolveOutput(path)
	if err != nil {
		return err
	}
	scratch := filepath.Join(root, "builds")
	if host.Within(scratch, resolved) {
		return fmt.Errorf("output cannot be placed inside build scratch storage")
	}
	mounts, err := host.Mounts()
	if err != nil {
		return err
	}
	forPath := func(path string) (*host.Mount, error) {
		var selected *host.Mount
		count := 0
		for _, mount := range mounts {
			if host.Within(mount.Path, path) {
				if selected == nil || len(mount.Path) > len(selected.Path) {
					copy := mount
					selected = &copy
					count = 1
				} else if mount.Path == selected.Path {
					count++
				}
			}
		}
		if selected == nil {
			return nil, fmt.Errorf("cannot establish output filesystem")
		}
		if count > 1 {
			return nil, fmt.Errorf("stacked mounts prevent output scope validation")
		}
		return selected, nil
	}
	a, err := forPath(scratch)
	if err != nil {
		return err
	}
	b, err := forPath(resolved)
	if err != nil {
		return err
	}
	if !filepath.IsAbs(a.Root) || filepath.Clean(a.Root) != a.Root || !filepath.IsAbs(b.Root) || filepath.Clean(b.Root) != b.Root {
		return fmt.Errorf("cannot establish filesystem roots for output publication")
	}
	if a.Device == b.Device {
		x, err := filepath.Rel(a.Path, scratch)
		if err != nil {
			return err
		}
		y, err := filepath.Rel(b.Path, resolved)
		if err != nil {
			return err
		}
		if host.Within(filepath.Join(a.Root, x), filepath.Join(b.Root, y)) {
			return fmt.Errorf("output resolves through a bind alias into build scratch storage")
		}
	}
	return nil
}
func resolveOutput(path string) (string, error) {
	ancestor := path
	var tail []string
	for {
		actual, err := filepath.EvalSymlinks(ancestor)
		if err == nil {
			for i := len(tail) - 1; i >= 0; i-- {
				actual = filepath.Join(actual, tail[i])
			}
			return actual, nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			return "", err
		}
		tail = append(tail, filepath.Base(ancestor))
		ancestor = parent
	}
}
