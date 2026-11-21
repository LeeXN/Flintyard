// Package network manages owned local Firecracker networks, without a daemon.
package network

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"net"
	"net/netip"
	"regexp"
	"strings"
)

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
var nodeNamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,47}$`)

type NodeRequest struct {
	ID, Name, IPv4 string
	UID, GID       uint32
}
type AllocationRequest struct {
	RootID, EnvironmentID, CIDR string
	Nodes                       []NodeRequest
	Occupied                    []netip.Prefix
	ExistingMACs, ExistingNames []string
	Connectivity                ConnectivityRequest
}
type Node struct {
	ID       string     `json:"id"`
	Name     string     `json:"name"`
	Hostname string     `json:"hostname"`
	IPv4     netip.Addr `json:"ipv4"`
	MAC      string     `json:"mac"`
	Tap      string     `json:"tap"`
	UID      uint32     `json:"uid"`
	GID      uint32     `json:"gid"`
}
type Plan struct {
	RootID          string       `json:"root_id"`
	EnvironmentID   string       `json:"environment_id"`
	Owner           string       `json:"owner"`
	CIDR            netip.Prefix `json:"cidr"`
	Gateway         netip.Addr   `json:"gateway"`
	Namespace       string       `json:"namespace"`
	HostLink        string       `json:"host_link"`
	PeerLink        string       `json:"peer_link"`
	Bridge          string       `json:"bridge"`
	Table           string       `json:"table"`
	Nodes           []Node       `json:"nodes"`
	NAT             bool         `json:"nat"`
	EgressInterface string       `json:"egress_interface,omitempty"`
	Forwards        []Forward    `json:"port_forwards,omitempty"`
}

func short(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "/")))
	return fmt.Sprintf("%x", sum[:])
}
func Owner(root, env string) string { return "flintyard:" + root + ":" + env }
func ipNumber(a netip.Addr) uint32  { b := a.As4(); return binary.BigEndian.Uint32(b[:]) }
func numberIP(n uint32) netip.Addr {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], n)
	return netip.AddrFrom4(b)
}
func Broadcast(p netip.Prefix) netip.Addr {
	return numberIP(ipNumber(p.Masked().Addr()) | (1<<uint(32-p.Bits()) - 1))
}
func privatePrefix(p netip.Prefix) bool {
	return p.IsValid() && p.Addr().Is4() && p == p.Masked() && p.Bits() >= 8 && p.Bits() <= 29 && p.Addr().IsPrivate() && Broadcast(p).IsPrivate()
}
func usable(p netip.Prefix, a netip.Addr) bool {
	return a.Is4() && p.Contains(a) && a != p.Addr() && a != p.Addr().Next() && a != Broadcast(p)
}

// Allocate is pure planning. The lifecycle caller must reserve the result under
// its system allocation lock before creating disks or any kernel resources.
func Allocate(req AllocationRequest) (Plan, error) {
	var p Plan
	if !uuidPattern.MatchString(req.RootID) || !uuidPattern.MatchString(req.EnvironmentID) || len(req.Nodes) < 1 || len(req.Nodes) > 128 {
		return p, fmt.Errorf("invalid network owner or node count")
	}
	for _, prefix := range req.Occupied {
		if !prefix.IsValid() || !prefix.Addr().Is4() {
			return p, fmt.Errorf("invalid occupied prefix")
		}
	}
	var cidr netip.Prefix
	if req.CIDR == "auto" {
		pool := netip.MustParsePrefix("10.240.0.0/16")
		for _, n := range req.Nodes {
			if n.IPv4 == "" {
				continue
			}
			a, err := netip.ParseAddr(n.IPv4)
			if err != nil || !a.Is4() || !pool.Contains(a) {
				return p, fmt.Errorf("explicit node address is outside the automatic pool")
			}
			candidate := netip.PrefixFrom(a, 24).Masked()
			if cidr.IsValid() && cidr != candidate {
				return p, fmt.Errorf("explicit nodes require different automatic subnets")
			}
			cidr = candidate
		}
		if !cidr.IsValid() {
			for i := uint32(0); i < 256; i++ {
				candidate := netip.PrefixFrom(numberIP(ipNumber(pool.Addr())+i*256), 24)
				if !conflicts(candidate, req.Occupied) {
					cidr = candidate
					break
				}
			}
		}
	} else {
		cidr, _ = netip.ParsePrefix(req.CIDR)
	}
	if !privatePrefix(cidr) || conflicts(cidr, req.Occupied) {
		return p, fmt.Errorf("subnet is invalid, overlaps an existing address/route/reservation, or the automatic pool is exhausted")
	}
	hash := short(req.RootID, req.EnvironmentID)
	p = Plan{RootID: req.RootID, EnvironmentID: req.EnvironmentID, Owner: Owner(req.RootID, req.EnvironmentID), CIDR: cidr, Gateway: cidr.Addr().Next(), Namespace: "fy-" + hash[:20], HostLink: "fyh" + hash[:12], PeerLink: "fyp" + hash[:12], Bridge: "fyb" + hash[:12], Table: "fy_" + hash[:24]}
	ips, ids, names, macs, interfaces := map[netip.Addr]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}
	uids, gids := map[uint32]bool{}, map[uint32]bool{}
	for _, s := range req.ExistingMACs {
		m, err := net.ParseMAC(s)
		if err != nil || len(m) != 6 {
			return Plan{}, fmt.Errorf("invalid existing MAC")
		}
		macs[m.String()] = true
	}
	for _, name := range req.ExistingNames {
		interfaces[name] = true
	}
	for _, name := range []string{p.HostLink, p.PeerLink, p.Bridge} {
		if interfaces[name] {
			return Plan{}, fmt.Errorf("interface name already reserved: %s", name)
		}
		interfaces[name] = true
	}
	for _, n := range req.Nodes {
		if !uuidPattern.MatchString(n.ID) || !nodeNamePattern.MatchString(n.Name) || n.UID == 0 || n.GID == 0 || n.UID == ^uint32(0) || n.GID == ^uint32(0) || uids[n.UID] || gids[n.GID] || ids[n.ID] || names[n.Name] {
			return Plan{}, fmt.Errorf("invalid or duplicate node identity, name, UID or GID")
		}
		ids[n.ID], names[n.Name] = true, true
		uids[n.UID], gids[n.GID] = true, true
		if n.IPv4 != "" {
			a, _ := netip.ParseAddr(n.IPv4)
			if !usable(cidr, a) || ips[a] {
				return Plan{}, fmt.Errorf("duplicate or reserved/out-of-subnet node address")
			}
			ips[a] = true
		}
	}
	next := ipNumber(cidr.Addr()) + 10
	for _, n := range req.Nodes {
		a, _ := netip.ParseAddr(n.IPv4)
		if n.IPv4 == "" {
			for next < ipNumber(Broadcast(cidr)) && ips[numberIP(next)] {
				next++
			}
			if next >= ipNumber(Broadcast(cidr)) {
				return Plan{}, fmt.Errorf("subnet has insufficient automatic addresses starting at host offset ten")
			}
			a = numberIP(next)
			ips[a] = true
			next++
		}
		nhash := short(req.RootID, req.EnvironmentID, n.ID)
		macBytes := sha256.Sum256([]byte(nhash))
		macBytes[0] = macBytes[0]&0xfc | 2
		mac := net.HardwareAddr(macBytes[:6]).String()
		tap := "fyt" + nhash[:12]
		if macs[mac] || interfaces[tap] {
			return Plan{}, fmt.Errorf("derived MAC or TAP name collision")
		}
		macs[mac], interfaces[tap] = true, true
		p.Nodes = append(p.Nodes, Node{ID: n.ID, Name: n.Name, Hostname: strings.TrimRight(n.Name, "-") + "-" + nhash[:8], IPv4: a, MAC: mac, Tap: tap, UID: n.UID, GID: n.GID})
	}
	return p, nil
}
func conflicts(p netip.Prefix, occupied []netip.Prefix) bool {
	for _, q := range occupied {
		if p.Overlaps(q) {
			return true
		}
	}
	return false
}

func (p Plan) Validate() error {
	req := AllocationRequest{RootID: p.RootID, EnvironmentID: p.EnvironmentID, CIDR: p.CIDR.String()}
	for _, n := range p.Nodes {
		req.Nodes = append(req.Nodes, NodeRequest{ID: n.ID, Name: n.Name, IPv4: n.IPv4.String(), UID: n.UID, GID: n.GID})
	}
	expected, err := Allocate(req)
	if err != nil {
		return err
	}
	if p.RootID != expected.RootID || p.EnvironmentID != expected.EnvironmentID || p.Owner != expected.Owner || p.CIDR != expected.CIDR || p.Gateway != expected.Gateway || p.Namespace != expected.Namespace || p.HostLink != expected.HostLink || p.PeerLink != expected.PeerLink || p.Bridge != expected.Bridge || p.Table != expected.Table {
		return fmt.Errorf("network plan ownership or names changed")
	}
	for i := range p.Nodes {
		if p.Nodes[i] != expected.Nodes[i] {
			return fmt.Errorf("node plan identity changed")
		}
	}
	return p.validateConnectivity()
}
