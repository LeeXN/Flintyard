package network

import (
	"net/netip"
	"reflect"
	"strings"
	"testing"

	"github.com/LeeXN/Flintyard/internal/state"
)

func allocationFixture(n int) AllocationRequest {
	r := AllocationRequest{RootID: state.UUID(), EnvironmentID: state.UUID(), CIDR: "auto"}
	for i := 0; i < n; i++ {
		r.Nodes = append(r.Nodes, NodeRequest{ID: state.UUID(), Name: string(rune('a' + i)), UID: uint32(60000 + i), GID: uint32(60000 + i)})
	}
	return r
}
func TestAllocateAvoidsRoutesReservationsAndStaticNodes(t *testing.T) {
	r := allocationFixture(3)
	r.Occupied = []netip.Prefix{netip.MustParsePrefix("10.240.0.0/24"), netip.MustParsePrefix("10.240.1.128/25")}
	p, err := Allocate(r)
	if err != nil {
		t.Fatal(err)
	}
	if p.CIDR.String() != "10.240.2.0/24" || p.Nodes[0].IPv4.String() != "10.240.2.10" {
		t.Fatal(p)
	}
	r.Nodes[2].IPv4 = "10.240.2.10"
	p, err = Allocate(r)
	if err != nil {
		t.Fatal(err)
	}
	if p.Nodes[0].IPv4.String() != "10.240.2.11" || p.Nodes[2].IPv4.String() != "10.240.2.10" {
		t.Fatal(p)
	}
	if err = p.Validate(); err != nil {
		t.Fatal(err)
	}
	again, err := Allocate(r)
	if err != nil || !reflect.DeepEqual(p, again) {
		t.Fatal("unstable plan", err)
	}
	for _, node := range p.Nodes {
		if len(node.Tap) > 15 || len(node.Hostname) > 63 || node.MAC == "" {
			t.Fatal(node)
		}
	}
	r.Occupied = append(r.Occupied, p.CIDR)
	if _, err = Allocate(r); err == nil {
		t.Fatal("moved explicit nodes away from reserved subnet")
	}
}
func TestAllocateRejectsReservedIPsAndCIDRs(t *testing.T) {
	for _, cidr := range []string{"0.0.0.0/0", "172.16.0.0/8", "192.168.0.1/24", "10.0.0.0/30", "fd00::/64"} {
		r := allocationFixture(1)
		r.CIDR = cidr
		if _, err := Allocate(r); err == nil {
			t.Fatal("accepted", cidr)
		}
	}
	for _, ip := range []string{"10.2.3.8", "10.2.3.9", "10.2.3.15", "10.2.3.16", "127.0.0.1"} {
		r := allocationFixture(1)
		r.CIDR = "10.2.3.8/29"
		r.Nodes[0].IPv4 = ip
		if _, err := Allocate(r); err == nil {
			t.Fatal("accepted reserved IP", ip)
		}
	}
	r := allocationFixture(1)
	r.CIDR = "10.2.3.8/29"
	r.Nodes[0].IPv4 = "10.2.3.10"
	if _, err := Allocate(r); err != nil {
		t.Fatal(err)
	}
	r.Nodes[0].IPv4 = ""
	if _, err := Allocate(r); err == nil {
		t.Fatal("invented automatic address outside /29")
	}
}
func TestPlanIdentityAndCollisions(t *testing.T) {
	r := allocationFixture(2)
	p, err := Allocate(r)
	if err != nil {
		t.Fatal(err)
	}
	r.ExistingMACs = []string{p.Nodes[0].MAC}
	if _, err = Allocate(r); err == nil {
		t.Fatal("accepted MAC collision")
	}
	r.ExistingMACs = nil
	r.ExistingNames = []string{p.HostLink}
	if _, err = Allocate(r); err == nil {
		t.Fatal("accepted interface collision")
	}
	p.HostLink = "eth0"
	if p.Validate() == nil {
		t.Fatal("accepted forged plan")
	}
	r = allocationFixture(2)
	r.Nodes[1].ID = r.Nodes[0].ID
	if _, err = Allocate(r); err == nil {
		t.Fatal("accepted duplicate node identity")
	}
	r = allocationFixture(1)
	r.Occupied = []netip.Prefix{netip.MustParsePrefix("10.240.0.0/16")}
	if _, err = Allocate(r); err == nil {
		t.Fatal("accepted exhausted pool")
	}
	r = allocationFixture(1)
	r.Nodes[0].Name = strings.Repeat("a", 49)
	if _, err = Allocate(r); err == nil {
		t.Fatal("accepted long hostname")
	}
	r = allocationFixture(2)
	r.Nodes[1].UID = r.Nodes[0].UID
	if _, err = Allocate(r); err == nil {
		t.Fatal("shared jailer UID")
	}
	r = allocationFixture(1)
	r.Nodes[0].UID = ^uint32(0)
	if _, err = Allocate(r); err == nil {
		t.Fatal("accepted unset TUN owner value")
	}
}
