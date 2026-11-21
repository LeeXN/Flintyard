// Package guestinit implements the data-only seed-v1 first-boot protocol.
package guestinit

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"

	"github.com/LeeXN/Flintyard/internal/host"
)

const SeedBytes = 16 << 20

type SeedRequest struct {
	EnvironmentID, NodeID string
	Hostname, MAC         string
	Address               netip.Prefix
	Gateway               netip.Addr
	DNS                   []netip.Addr
	Username              string
	PublicKeys            []byte
	Destination           string
}
type SeedResult struct {
	Path   string `json:"path"`
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
	Size   int64  `json:"size_bytes"`
}

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
var hostnamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,61}[a-z0-9]$|^[a-z]$`)
var userPattern = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)

func seedFiles(req SeedRequest) (map[string]string, error) {
	if !uuidPattern.MatchString(req.EnvironmentID) || !uuidPattern.MatchString(req.NodeID) || !hostnamePattern.MatchString(req.Hostname) || !userPattern.MatchString(req.Username) {
		return nil, fmt.Errorf("invalid seed instance identity, hostname or username")
	}
	mac, err := net.ParseMAC(req.MAC)
	if err != nil || len(mac) != 6 || mac[0]&3 != 2 || mac.String() != req.MAC {
		return nil, fmt.Errorf("seed requires a canonical locally administered unicast MAC")
	}
	a := req.Address
	if !a.IsValid() || !a.Addr().Is4() || !a.Addr().IsPrivate() || a.Bits() < 8 || a.Bits() > 29 || !req.Gateway.Is4() || !a.Masked().Contains(req.Gateway) || a.Addr() == req.Gateway {
		return nil, fmt.Errorf("invalid seed IPv4 prefix or gateway")
	}
	network := a.Masked().Addr()
	last := network.As4()
	bits := uint32(last[0])<<24 | uint32(last[1])<<16 | uint32(last[2])<<8 | uint32(last[3])
	bits |= (1 << uint(32-a.Bits())) - 1
	broadcast := netip.AddrFrom4([4]byte{byte(bits >> 24), byte(bits >> 16), byte(bits >> 8), byte(bits)})
	if !network.IsPrivate() || !broadcast.IsPrivate() || a.Addr() == network || a.Addr() == broadcast || req.Gateway != network.Next() {
		return nil, fmt.Errorf("seed address is reserved or gateway is not the first usable address")
	}
	if len(req.DNS) > 16 {
		return nil, fmt.Errorf("too many seed DNS servers")
	}
	var dns []string
	for _, d := range req.DNS {
		if !d.Is4() || !d.IsGlobalUnicast() || d.IsLoopback() || d.IsLinkLocalUnicast() {
			return nil, fmt.Errorf("seed DNS must be a reachable unicast IPv4 address")
		}
		dns = append(dns, d.String())
	}
	keys := ""
	if len(req.PublicKeys) != 0 {
		keys, err = PublicKeys(req.PublicKeys)
		if err != nil {
			return nil, err
		}
	}
	dnsText := ""
	if len(dns) != 0 {
		dnsText = strings.Join(dns, "\n") + "\n"
	}
	return map[string]string{
		"version": "1\n", "instance-id": req.EnvironmentID + "/" + req.NodeID + "\n", "hostname": req.Hostname + "\n", "mac": req.MAC + "\n",
		"address": a.Addr().String() + "\n", "prefix": strconv.Itoa(a.Bits()) + "\n", "gateway": req.Gateway.String() + "\n", "dns": dnsText,
		"username": req.Username + "\n", "authorized_keys": keys,
	}, nil
}

// CreateSeed makes an actual ext4 disk without mounting it or changing global
// mount state. Its caller holds the node lock and journals the destination.
func CreateSeed(ctx context.Context, req SeedRequest) (SeedResult, error) {
	var result SeedResult
	files, err := seedFiles(req)
	if err != nil {
		return result, err
	}
	if req.Destination == "" {
		return result, fmt.Errorf("seed destination is required")
	}
	// An absolute path cannot be mistaken for a mkfs option even when a
	// library caller uses a relative directory whose name starts with '-'.
	req.Destination, err = filepath.Abs(req.Destination)
	if err != nil {
		return result, err
	}
	if err = ctx.Err(); err != nil {
		return result, err
	}
	dir, err := os.MkdirTemp(filepath.Dir(req.Destination), ".fy-seed-")
	if err != nil {
		return result, err
	}
	defer os.RemoveAll(dir)
	for name, value := range files {
		if err = os.WriteFile(filepath.Join(dir, name), []byte(value), 0400); err != nil {
			return result, err
		}
	}
	err = host.PublishFile(ctx, req.Destination, 0400, func(f *os.File) error {
		if err := f.Truncate(SeedBytes); err != nil {
			return err
		}
		// Fix the feature set rather than inheriting a host mke2fs.conf that may
		// enable features unsupported by a guest kernel. Seed is read-only and
		// needs no journal. No input is a shell command or an option fragment.
		_, err := host.Run(ctx, host.CommandRequest{Name: "mkfs.ext4", Args: []string{"-q", "-F", "-b", "1024", "-I", "256", "-m", "0", "-O", "none,extent,filetype,sparse_super,large_file", "-L", "FYSEED", "-d", dir, f.Name()}})
		if err != nil {
			return err
		}
		st, err := f.Stat()
		if err != nil {
			return err
		}
		s := st.Sys().(*syscall.Stat_t)
		result = SeedResult{Path: req.Destination, Device: uint64(s.Dev), Inode: s.Ino, Size: st.Size()}
		return nil
	})
	if err != nil {
		return SeedResult{}, err
	}
	return result, nil
}
