package flintyard

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/LeeXN/Flintyard/internal/guestinit"
	"github.com/LeeXN/Flintyard/internal/state"
	"golang.org/x/sys/unix"
)

func availableDiskBytes(path string) (uint64, error) {
	var fs unix.Statfs_t
	if err := unix.Statfs(path, &fs); err != nil {
		return 0, err
	}
	return fs.Bavail * uint64(fs.Bsize), nil
}

func checkRunSpace(root state.Root, input resolvedInput, newDisks bool) error {
	available, err := availableDiskBytes(root.Path)
	if err != nil {
		return Failure("E_IMAGE_INVALID", "preflight", err)
	}
	var required uint64
	for _, image := range input.images {
		for _, payload := range image.Manifest.Payloads {
			if payload.Role != "rootfs" || newDisks {
				required += uint64(payload.Size)
			}
		}
		if newDisks && image.Manifest.Initialization.Mode == "seed-v1" {
			required += guestinit.SeedBytes
		}
	}
	if available < required {
		return Failure("E_DISK_FULL", "preflight", fmt.Errorf("node disks and boot files need %d bytes, but %s has %d bytes available: %w", required, root.Path, available, unix.ENOSPC))
	}
	return nil
}

func addStorageObservation(root state.Root, env environmentState, result *EnvironmentResult) {
	const logWarningBytes = 64 << 20
	available, err := availableDiskBytes(root.Path)
	if err != nil {
		result.Warnings = append(result.Warnings, fmt.Sprintf("cannot inspect state disk space: %v", err))
	} else {
		result.DiskAvailableBytes = &available
		if available < logWarningBytes {
			result.Warnings = append(result.Warnings, "less than 64 MiB available for Flintyard state and startup logs")
		}
	}
	for _, node := range env.Nodes {
		dir := nodeDir(root, env.ID, node.ID)
		owner := state.Owner{RootID: root.ID, EnvironmentID: env.ID, NodeID: node.ID}
		if state.VerifyOwner(dir, owner) != nil {
			continue
		}
		logs := filepath.Join(dir, "logs")
		if state.CheckPrivateDir(logs) != nil {
			continue
		}
		for _, name := range []string{"serial.log", "firecracker.log"} {
			st, err := os.Lstat(filepath.Join(logs, name))
			if err == nil && st.Mode().IsRegular() && st.Size() >= logWarningBytes {
				result.Warnings = append(result.Warnings, fmt.Sprintf("node %s %s is %d bytes; down --purge removes retained startup logs and discards this environment", node.Name, name, st.Size()))
			}
		}
	}
}
