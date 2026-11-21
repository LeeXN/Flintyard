package state

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const BootstrapProtocol = "flintyard-owned-bootstrap/v1"

// PublisherLifetime is an absence proof for an unfinished publication only.
// It must never be used to signal or adopt a process.
type PublisherLifetime struct {
	PID          int    `json:"pid"`
	BootID       string `json:"boot_id"`
	StartTime    uint64 `json:"starttime"`
	PIDNamespace string `json:"pid_namespace"`
}
type Bootstrap struct {
	APIVersion  string            `json:"api_version"`
	Kind        string            `json:"kind"`
	Protocol    string            `json:"protocol"`
	Token       string            `json:"token"`
	Owner       Owner             `json:"owner"`
	Destination string            `json:"destination"`
	StagingName string            `json:"staging_name"`
	Device      uint64            `json:"directory_device"`
	Inode       uint64            `json:"directory_inode"`
	Creator     PublisherLifetime `json:"creator"`
	CreatedAt   time.Time         `json:"created_at"`
	Files       map[string]string `json:"files_sha256"`
}

func encodeBootstrapFiles(files map[string]any) (map[string][]byte, error) {
	result := map[string][]byte{}
	for name, value := range files {
		if name == "owner.json" || name == "bootstrap.json" || name == "gc-receipt.json" || name == "." || name == ".." || filepath.Base(name) != name {
			return nil, fmt.Errorf("invalid owned directory bootstrap file")
		}
		b, err := json.MarshalIndent(value, "", "  ")
		if err != nil {
			return nil, err
		}
		if len(b)+1 > MaxState {
			return nil, fmt.Errorf("bootstrap file exceeds state limit")
		}
		result[name] = append(b, '\n')
	}
	return result, nil
}
func bootstrapSum(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func newBootstrap(dir, destination string, owner Owner, files map[string][]byte) (Bootstrap, error) {
	var b Bootstrap
	st, err := os.Lstat(dir)
	if err != nil {
		return b, err
	}
	s := st.Sys().(*syscall.Stat_t)
	creator, err := CurrentPublisher()
	if err != nil {
		return b, err
	}
	b = Bootstrap{APIVersion: "flintyard/v1alpha1", Kind: "OwnedDirectoryBootstrap", Protocol: BootstrapProtocol, Token: UUID(), Owner: owner, Destination: destination, StagingName: filepath.Base(dir), Device: uint64(s.Dev), Inode: s.Ino, Creator: creator, CreatedAt: time.Now().UTC(), Files: map[string]string{}}
	for name, data := range files {
		b.Files[name] = bootstrapSum(data)
	}
	return b, nil
}
func CurrentPublisher() (PublisherLifetime, error) {
	p := PublisherLifetime{PID: os.Getpid()}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return p, err
	}
	p.BootID = strings.TrimSpace(string(boot))
	p.PIDNamespace, err = os.Readlink("/proc/self/ns/pid")
	if err != nil {
		return p, err
	}
	stat, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		return p, err
	}
	p.StartTime, err = publisherStartTime(stat)
	return p, err
}
func publisherStartTime(b []byte) (uint64, error) {
	text := string(b)
	i := strings.LastIndex(text, ") ")
	if i < 0 {
		return 0, fmt.Errorf("malformed publisher stat")
	}
	fields := strings.Fields(text[i+2:])
	if len(fields) < 20 {
		return 0, fmt.Errorf("short publisher stat")
	}
	return strconv.ParseUint(fields[19], 10, 64)
}
func PublisherExited(ctx context.Context, p PublisherLifetime) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if p.PID <= 0 || !ValidUUID(p.BootID) || p.StartTime == 0 || p.PIDNamespace == "" {
		return false, fmt.Errorf("incomplete bootstrap publisher lifetime")
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(string(boot)) != p.BootID {
		return true, nil
	}
	namespace, err := os.Readlink("/proc/self/ns/pid")
	if err != nil {
		return false, err
	}
	if namespace != p.PIDNamespace {
		return false, fmt.Errorf("bootstrap publisher PID namespace differs")
	}
	fd, err := unix.PidfdOpen(p.PID, 0)
	if errors.Is(err, unix.ESRCH) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	defer unix.Close(fd)
	poll := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	if _, err = unix.Poll(poll, 0); err != nil {
		return false, err
	}
	if poll[0].Revents&(unix.POLLIN|unix.POLLHUP) != 0 {
		return true, nil
	}
	if poll[0].Revents&(unix.POLLERR|unix.POLLNVAL) != 0 {
		return false, fmt.Errorf("invalid bootstrap pidfd")
	}
	stat, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(p.PID), "stat"))
	if err != nil {
		return false, err
	}
	start, err := publisherStartTime(stat)
	if err != nil {
		return false, err
	}
	if start > p.StartTime {
		return true, nil
	} // PID was reused; no signal is sent.
	if start != p.StartTime {
		return false, fmt.Errorf("bootstrap publisher lifetime mismatch")
	}
	return false, nil
}
func ReadBootstrap(path string) (Bootstrap, error) {
	var b Bootstrap
	if err := CheckPrivateDir(path); err != nil {
		return b, err
	}
	if err := Read(filepath.Join(path, "bootstrap.json"), &b); err != nil {
		return b, err
	}
	if b.APIVersion != "flintyard/v1alpha1" || b.Kind != "OwnedDirectoryBootstrap" || b.Protocol != BootstrapProtocol || !ValidUUID(b.Token) || b.CreatedAt.IsZero() || b.Inode == 0 || b.Files == nil || !filepath.IsAbs(b.Destination) || filepath.Clean(b.Destination) != b.Destination || filepath.Base(b.StagingName) != b.StagingName || b.StagingName == "." || b.StagingName == ".." {
		return b, fmt.Errorf("invalid directory bootstrap identity")
	}
	if err := b.Owner.Validate(); err != nil {
		return b, err
	}
	if err := VerifyOwner(path, b.Owner); err != nil {
		return b, err
	}
	st, err := os.Lstat(path)
	if err != nil {
		return b, err
	}
	s := st.Sys().(*syscall.Stat_t)
	if uint64(s.Dev) != b.Device || s.Ino != b.Inode {
		return b, fmt.Errorf("bootstrap directory inode changed")
	}
	for name, digest := range b.Files {
		if name == "owner.json" || name == "bootstrap.json" || name == "gc-receipt.json" || name == "." || name == ".." || filepath.Base(name) != name || len(digest) != 64 {
			return b, fmt.Errorf("invalid bootstrap file identity")
		}
		if _, err := hex.DecodeString(digest); err != nil {
			return b, err
		}
	}
	return b, nil
}
