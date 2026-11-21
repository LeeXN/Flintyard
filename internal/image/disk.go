package image

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"syscall"

	"github.com/LeeXN/Flintyard/internal/host"
)

type Disk struct {
	Path         string `json:"path"`
	Size         int64  `json:"size_bytes"`
	Device       uint64 `json:"device"`
	Inode        uint64 `json:"inode"`
	SourceSHA256 string `json:"source_sha256"`
	CopyMethod   string `json:"copy_method"`
}

// CloneRootfs copies and rechecks immutable cached bytes into an independent
// writable inode. It never reuses an existing node disk (including on restart).
// The lifecycle caller must journal the destination before calling this method.
func CloneRootfs(ctx context.Context, root, id, destination string) (Disk, error) {
	return copyPayload(ctx, root, id, "rootfs", destination, 0600)
}

// CopyBootPayload preserves the requested kernel/initrd bytes in a private jail
// inode; immutable cache files never need to be chowned to the VMM UID.
func CopyBootPayload(ctx context.Context, root, id, role, destination string) (Disk, error) {
	if role != "kernel" && role != "initrd" {
		return Disk{}, fmt.Errorf("boot payload must be kernel or initrd")
	}
	return copyPayload(ctx, root, id, role, destination, 0400)
}
func copyPayload(ctx context.Context, root, id, role, destination string, mode os.FileMode) (Disk, error) {
	var disk Disk
	dir, err := cacheDir(root, id)
	if err != nil {
		return disk, err
	}
	cache, err := os.OpenRoot(dir)
	if err != nil {
		return disk, err
	}
	defer cache.Close()
	manifest, err := readCachedManifest(cache, id)
	if err != nil {
		return disk, err
	}
	var expected Payload
	for _, p := range manifest.Payloads {
		if p.Role == role {
			expected = p
		}
	}
	if expected.Path == "" {
		return disk, fmt.Errorf("missing %s payload", role)
	}
	f, err := cache.OpenFile(expected.Path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return disk, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return disk, err
	}
	if !st.Mode().IsRegular() {
		return disk, fmt.Errorf("cached rootfs is not an ordinary file")
	}
	err = host.PublishFile(ctx, destination, mode, func(out *os.File) error {
		h := sha256.New()
		n, err := io.Copy(io.MultiWriter(out, h), contextReader{ctx, io.LimitReader(f, expected.Size+1)})
		if err != nil {
			return err
		}
		if n != expected.Size || hex.EncodeToString(h.Sum(nil)) != expected.SHA256 {
			return fmt.Errorf("cached rootfs digest mismatch during copy")
		}
		st, err := out.Stat()
		if err != nil {
			return err
		}
		s := st.Sys().(*syscall.Stat_t)
		disk = Disk{Path: destination, Size: n, Device: uint64(s.Dev), Inode: s.Ino, SourceSHA256: expected.SHA256, CopyMethod: "copy"}
		return nil
	})
	if err != nil {
		return Disk{}, err
	}
	return disk, nil
}
