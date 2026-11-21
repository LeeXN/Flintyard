package network

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"testing"
)

func connectivityFixture(t *testing.T) (Plan, ConnectivityHost) {
	t.Helper()
	p, err := Allocate(allocationFixture(1))
	if err != nil {
		t.Fatal(err)
	}
	l := Link{Name: "eth0", Index: 1}
	l.Addresses = append(l.Addresses, struct {
		Family string `json:"family"`
		Local  string `json:"local"`
		Prefix int    `json:"prefixlen"`
	}{"inet", "192.0.2.1", 24})
	return p, ConnectivityHost{Forwarding: true, Snapshot: Snapshot{Links: []Link{l}, DefaultEgress: []string{"eth0"}}}
}
func TestConnectivityResolveAndRejectsUnsafeHostConditions(t *testing.T) {
	p, h := connectivityFixture(t)
	r := ConnectivityRequest{NAT: true, Forwards: []ForwardRequest{{Protocol: "tcp", BindAddress: "127.0.0.1", HostPort: 2022, Node: p.Nodes[0].Name, GuestPort: 22}, {Protocol: "udp", BindAddress: "192.0.2.1", HostPort: 2053, Node: p.Nodes[0].Name, GuestPort: 53}}}
	resolved, err := ResolveConnectivity(p, r, h)
	if err != nil || resolved.EgressInterface != "eth0" || len(resolved.Forwards) != 2 || resolved.Forwards[0].NodeID != p.Nodes[0].ID {
		t.Fatal(resolved, err)
	}
	if err := resolved.Validate(); err != nil {
		t.Fatal(err)
	}
	if !MappingConflict(resolved.Forwards, resolved.Forwards) {
		t.Fatal("mapping did not reserve its key")
	}
	for _, tc := range []string{"forwarding", "ambiguous", "missing-bind", "owned-egress", "missing-node", "wildcard", "socket4", "socket6", "invalid-port", "duplicate"} {
		t.Run(tc, func(t *testing.T) {
			p, h := connectivityFixture(t)
			cfg := r
			cfg.Forwards = append([]ForwardRequest(nil), r.Forwards...)
			switch tc {
			case "forwarding":
				h.Forwarding = false
			case "ambiguous":
				h.Snapshot.DefaultEgress = []string{"eth0", "vpn0"}
			case "missing-bind":
				cfg.Forwards[1].BindAddress = "192.0.2.44"
			case "owned-egress":
				h.Snapshot.Links[0].Alias = p.Owner
			case "missing-node":
				cfg.Forwards[0].Node = "missing"
			case "wildcard":
				cfg.Forwards[0].BindAddress = "0.0.0.0"
			case "socket4":
				h.Sockets = []Socket{{Protocol: "tcp", Address: netip.MustParseAddr("0.0.0.0"), Port: 2022}}
			case "socket6":
				h.Sockets = []Socket{{Protocol: "udp", Address: netip.MustParseAddr("::"), Port: 2053}}
			case "invalid-port":
				cfg.Forwards[0].GuestPort = 65536
			case "duplicate":
				cfg.Forwards = append(cfg.Forwards, cfg.Forwards[0])
			}
			_, err := ResolveConnectivity(p, cfg, h)
			if err == nil {
				t.Fatal("accepted", tc)
			}
			if tc == "forwarding" && !errors.Is(err, ErrForwardingDisabled) {
				t.Fatal(err)
			}
			if strings.HasPrefix(tc, "socket") && !errors.Is(err, ErrPortInUse) {
				t.Fatal(err)
			}
		})
	}
	h.Forwarding = false
	_, err = ResolveConnectivity(p, ConnectivityRequest{Forwards: r.Forwards[:1]}, h)
	if err != nil {
		t.Fatal("local-only mapping required global forwarding", err)
	}
}
func TestSocketSnapshotIPv4IPv6AndTCPState(t *testing.T) {
	procAddr := func(v string) string {
		a := netip.MustParseAddr(v)
		b := append([]byte{}, a.AsSlice()...)
		for i := 0; i < len(b); i += 4 {
			word := binary.BigEndian.Uint32(b[i : i+4])
			binary.NativeEndian.PutUint32(b[i:i+4], word)
		}
		return hex.EncodeToString(b)
	}
	for _, v := range []string{"127.0.0.1", "0.0.0.0", "::", "::ffff:127.0.0.1", "::1"} {
		name := "tcp"
		if strings.Contains(v, ":") {
			name += "6"
		}
		input := fmt.Sprintf("header\n 0: %s:1F90 00000000:0000 0A rest\n 1: %s:1F91 00000000:0000 01 rest\n", procAddr(v), procAddr(v))
		s, err := parseSockets(context.Background(), strings.NewReader(input), name)
		if err != nil || len(s) != 1 || s[0].Address != netip.MustParseAddr(v).Unmap() || s[0].Port != 8080 {
			t.Fatal(v, s, err)
		}
	}
	if _, err := parseSockets(context.Background(), strings.NewReader("header\n 0: bad 00 0A\n"), "udp"); err == nil {
		t.Fatal("accepted malformed socket snapshot")
	}
}
