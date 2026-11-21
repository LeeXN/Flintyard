package contract

import (
	"fmt"
	"net/netip"
	"path/filepath"
)

func Environment(b []byte, base string) (map[string]any, error) {
	if e := Validate("environment", b); e != nil {
		return nil, e
	}
	var m map[string]any
	if e := Decode(b, &m); e != nil {
		return nil, e
	}
	net := m["network"].(map[string]any)
	if _, ok := net["egress_interface"]; ok && !net["nat"].(bool) {
		return nil, fmt.Errorf("egress_interface requires nat=true")
	}
	var prefix netip.Prefix
	if s := net["cidr"].(string); s != "auto" {
		p, e := netip.ParsePrefix(s)
		if e != nil || !p.Addr().Is4() || !p.Addr().IsPrivate() || p != p.Masked() || p.Bits() > 29 || p.Bits() < 8 {
			return nil, fmt.Errorf("CIDR must be canonical private IPv4 /8 through /29")
		}
		prefix = p
		// The entire subnet must be private, not only its first address (for
		// example 192.168.0.0/15 also contains public 192.169.0.0/16).
		b := p.Addr().As4()
		last := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3]) | (1<<uint(32-p.Bits()) - 1)
		if !netip.AddrFrom4([4]byte{byte(last >> 24), byte(last >> 16), byte(last >> 8), byte(last)}).IsPrivate() {
			return nil, fmt.Errorf("entire CIDR must be private IPv4")
		}
	}
	for _, v := range net["dns"].([]any) {
		a, _ := netip.ParseAddr(v.(string))
		if a.IsLoopback() || a.IsUnspecified() || a.IsMulticast() {
			return nil, fmt.Errorf("guest DNS must be a reachable unicast address")
		}
	}
	names := map[string]bool{}
	ips := map[string]bool{}
	for _, v := range m["nodes"].([]any) {
		n := v.(map[string]any)
		name := n["name"].(string)
		if n["vcpu_count"].(float64) > 32 {
			return nil, fmt.Errorf("Firecracker v1.17.0 supports at most 32 vCPUs")
		}
		if names[name] {
			return nil, fmt.Errorf("duplicate node %s", name)
		}
		names[name] = true
		if s, ok := n["ipv4"].(string); ok {
			a, _ := netip.ParseAddr(s)
			if ips[s] || a.IsLoopback() || a.IsUnspecified() || a.IsMulticast() {
				return nil, fmt.Errorf("duplicate or invalid node IP")
			}
			ips[s] = true
			if prefix.IsValid() {
				last := prefix.Addr().As4()
				hostbits := 32 - prefix.Bits()
				num := uint32(last[0])<<24 | uint32(last[1])<<16 | uint32(last[2])<<8 | uint32(last[3])
				x := a.As4()
				ip := uint32(x[0])<<24 | uint32(x[1])<<16 | uint32(x[2])<<8 | uint32(x[3])
				if !prefix.Contains(a) || ip == num || ip == num+1 || ip == num+(1<<hostbits)-1 {
					return nil, fmt.Errorf("node IP outside usable subnet")
				}
			}
		}
		s := n["image"].(string)
		if len(s) < 7 || s[:7] != "sha256:" {
			if !filepath.IsAbs(s) {
				n["image"] = filepath.Join(base, s)
			}
		}
	}
	access := m["access"].(map[string]any)
	if p, ok := access["public_key"].(string); ok && !filepath.IsAbs(p) {
		access["public_key"] = filepath.Join(base, p)
	}
	keys := map[string]bool{}
	for _, v := range m["port_forwards"].([]any) {
		f := v.(map[string]any)
		if !names[f["node"].(string)] {
			return nil, fmt.Errorf("forward refers to missing node")
		}
		a, _ := netip.ParseAddr(f["bind_address"].(string))
		if !a.Is4() || a.IsUnspecified() || a.IsMulticast() || a.String() == "255.255.255.255" {
			return nil, fmt.Errorf("forward must bind a concrete IPv4 address")
		}
		key := fmt.Sprint(f["protocol"], "/", a, "/", f["host_port"])
		if keys[key] {
			return nil, fmt.Errorf("duplicate port forward")
		}
		keys[key] = true
	}
	return m, nil
}
