package host

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/LeeXN/Flintyard/internal/state"
)

// PublishFile creates a new inode, syncs it, and publishes without replacement.
// The caller owns the private parent directory and holds its operation lock;
// it must journal intent before calling and record the returned file identity.
// Linking here only publishes the private staging inode, never a cached image.
func PublishFile(ctx context.Context, path string, mode os.FileMode, write func(*os.File) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".fy-file-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err = write(f); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	// Keep staging writable for utilities such as mkfs, then apply the final
	// permissions before the inode becomes visible at its published path.
	if err = f.Chmod(mode); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Link(f.Name(), path); err != nil {
		return err
	}
	if err = state.SyncDir(filepath.Dir(path)); err != nil {
		return errors.Join(err, os.Remove(path))
	}
	return nil
}
