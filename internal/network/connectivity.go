package network

import (
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"strings"
)

var ErrForwardingDisabled = errors.New("IPv4 forwarding is disabled; enable net.ipv4.ip_forward explicitly for NAT or external mappings")
var ErrPortInUse = errors.New("mapping port is already in use")

type ForwardRequest struct {
	Protocol    string `json:"protocol"`
	BindAddress string `json:"bind_address"`
	HostPort    int    `json:"host_port"`
	Node        string `json:"node"`
	GuestPort   int    `json:"guest_port"`
}
type ConnectivityRequest struct {
	NAT             bool
	EgressInterface string
	Forwards        []ForwardRequest
}
type Forward struct {
	Protocol    string     `json:"protocol"`
	BindAddress netip.Addr `json:"bind_address"`
	HostPort    int        `json:"host_port"`
	NodeID      string     `json:"node_id"`
	Node        string     `json:"node"`
	GuestIP     netip.Addr `json:"guest_ip"`
	GuestPort   int        `json:"guest_port"`
}
type Socket struct {
	Protocol string
	Address  netip.Addr
	Port     int
}
type ConnectivityHost struct {
	Snapshot   Snapshot
	Forwarding bool
	Sockets    []Socket
}

var interfaceName = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,15}$`)

func validBind(a netip.Addr) bool {
	return a.Is4() && !a.IsUnspecified() && !a.IsMulticast() && a != netip.MustParseAddr("255.255.255.255")
}
func (f Forward) Key() string { return fmt.Sprintf("%s/%s/%d", f.Protocol, f.BindAddress, f.HostPort) }
func MappingConflict(a, b []Forward) bool {
	for _, x := range a {
		for _, y := range b {
			if x.Key() == y.Key() {
				return true
			}
		}
	}
	return false
}
func (p Plan) validateConnectivity() error {
	if p.NAT != (p.EgressInterface != "") || p.NAT && (!interfaceName.MatchString(p.EgressInterface) || p.EgressInterface == "lo" || p.EgressInterface == p.HostLink || p.EgressInterface == p.PeerLink || p.EgressInterface == p.Bridge) {
		return fmt.Errorf("invalid NAT egress plan")
	}
	seen := map[string]bool{}
	for _, f := range p.Forwards {
		if !validBind(f.BindAddress) || p.CIDR.Contains(f.BindAddress) || f.HostPort < 1 || f.HostPort > 65535 || f.GuestPort < 1 || f.GuestPort > 65535 || f.Protocol != "tcp" && f.Protocol != "udp" || seen[f.Key()] {
			return fmt.Errorf("invalid or duplicate mapping")
		}
		seen[f.Key()] = true
		found := false
		for _, n := range p.Nodes {
			if n.ID == f.NodeID && n.Name == f.Node && n.IPv4 == f.GuestIP {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("mapping target does not match a node")
		}
	}
	return nil
}
func requestFromPlan(p Plan) ConnectivityRequest {
	r := ConnectivityRequest{NAT: p.NAT, EgressInterface: p.EgressInterface}
	for _, f := range p.Forwards {
		r.Forwards = append(r.Forwards, ForwardRequest{Protocol: f.Protocol, BindAddress: f.BindAddress.String(), HostPort: f.HostPort, Node: f.Node, GuestPort: f.GuestPort})
	}
	return r
}

// ResolveConnectivity is pure: callers supply a fresh host snapshot under the
// global allocation lock. No interface, sysctl or firewall is changed here.
func ResolveConnectivity(p Plan, r ConnectivityRequest, h ConnectivityHost) (Plan, error) {
	resolved, err := resolveRequest(r, h)
	if err != nil {
		return p, err
	}
	p.NAT, p.EgressInterface = resolved.NAT, resolved.EgressInterface
	p.Forwards = nil
	for _, f := range resolved.Forwards {
		next := Forward{Protocol: f.Protocol, BindAddress: netip.MustParseAddr(f.BindAddress), HostPort: f.HostPort, Node: f.Node, GuestPort: f.GuestPort}
		for _, n := range p.Nodes {
			if n.Name == f.Node {
				next.NodeID, next.GuestIP = n.ID, n.IPv4
			}
		}
		p.Forwards = append(p.Forwards, next)
	}
	return p, p.validateConnectivity()
}
func resolveRequest(r ConnectivityRequest, h ConnectivityHost) (ConnectivityRequest, error) {
	if !r.NAT && r.EgressInterface != "" {
		return r, fmt.Errorf("egress_interface requires nat=true")
	}
	needsForwarding := r.NAT
	seen := map[string]bool{}
	for _, f := range r.Forwards {
		a, err := netip.ParseAddr(f.BindAddress)
		if err != nil || !validBind(a) || f.Protocol != "tcp" && f.Protocol != "udp" || f.HostPort < 1 || f.HostPort > 65535 || f.GuestPort < 1 || f.GuestPort > 65535 {
			return r, fmt.Errorf("invalid concrete IPv4 mapping")
		}
		key := fmt.Sprintf("%s/%s/%d", f.Protocol, a, f.HostPort)
		if seen[key] {
			return r, fmt.Errorf("duplicate mapping")
		}
		seen[key] = true
		if !a.IsLoopback() {
			needsForwarding = true
			found := false
			for _, l := range h.Snapshot.Links {
				for _, v := range l.Addresses {
					if v.Family == "inet" && v.Local == a.String() {
						if strings.HasPrefix(l.Alias, "flintyard:") {
							return r, fmt.Errorf("cannot bind mapping to a Flintyard interface")
						}
						found = true
					}
				}
			}
			if !found {
				return r, fmt.Errorf("bind address %s is not held by the host", a)
			}
		}
		for _, s := range h.Sockets {
			if s.Protocol == f.Protocol && s.Port == f.HostPort && (s.Address.IsUnspecified() || s.Address.Unmap() == a) {
				return r, fmt.Errorf("%w: %s has an existing host socket", ErrPortInUse, key)
			}
		}
	}
	if needsForwarding && !h.Forwarding {
		return r, ErrForwardingDisabled
	}
	if r.NAT {
		name := r.EgressInterface
		if name == "" {
			names := map[string]bool{}
			for _, v := range h.Snapshot.DefaultEgress {
				names[v] = true
			}
			if len(names) != 1 || names[""] {
				return r, fmt.Errorf("NAT requires an unambiguous default egress or explicit egress_interface")
			}
			for v := range names {
				name = v
			}
		}
		if !interfaceName.MatchString(name) || name == "lo" {
			return r, fmt.Errorf("invalid egress_interface")
		}
		found := false
		for _, l := range h.Snapshot.Links {
			if l.Name == name {
				if strings.HasPrefix(l.Alias, "flintyard:") {
					return r, fmt.Errorf("NAT egress cannot be a Flintyard interface")
				}
				found = true
			}
		}
		if !found {
			return r, fmt.Errorf("egress interface %s does not exist", name)
		}
		r.EgressInterface = name
	}
	return r, nil
}
