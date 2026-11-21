package host

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type Mount struct {
	ID         int
	Path       string
	Filesystem string
	ReadOnly   bool
	Root       string
	Device     string
}

func Mounts() ([]Mount, error) {
	b, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return nil, err
	}
	return ParseMounts(string(b))
}
func ParseMounts(text string) ([]Mount, error) {
	var result []Mount
	decode := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	for _, line := range strings.Split(text, "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, " - ", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("malformed mountinfo")
		}
		left, right := strings.Fields(parts[0]), strings.Fields(parts[1])
		if len(left) < 6 || len(right) < 3 {
			return nil, fmt.Errorf("short mountinfo")
		}
		id, err := strconv.Atoi(left[0])
		if err != nil || id < 1 {
			return nil, fmt.Errorf("invalid mount id")
		}
		path := decode.Replace(left[4])
		if !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return nil, fmt.Errorf("invalid mount path")
		}
		ro := false
		for _, flag := range strings.Split(left[5], ",") {
			if flag == "ro" {
				ro = true
			}
		}
		root := decode.Replace(left[3])
		result = append(result, Mount{ID: id, Path: path, Filesystem: right[0], ReadOnly: ro, Root: root, Device: left[2]})
	}
	return result, nil
}
func MountAt(path string) (*Mount, error) {
	mounts, err := Mounts()
	if err != nil {
		return nil, err
	}
	var found *Mount
	for _, mount := range mounts {
		if mount.Path == path {
			if found != nil {
				return nil, fmt.Errorf("stacked mounts at %s", path)
			}
			copy := mount
			found = &copy
		}
	}
	return found, nil
}
func Within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}
