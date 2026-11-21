package network

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/LeeXN/Flintyard/internal/host"
	"github.com/LeeXN/Flintyard/internal/state"
)

// Helpers run only in test-created namespaces, never through the product CLI.
func TestNativeNATSocketPeer(t *testing.T) {
	mode := os.Getenv("FY_NAT_CHILD")
	if mode == "" {
		return
	}
	proto, addr := os.Getenv("FY_NAT_PROTO"), os.Getenv("FY_NAT_ADDR")
	if mode == "probe" {
		response, err := natExchange(proto, addr)
		if os.Getenv("FY_NAT_DENY") == "1" {
			if err == nil {
				t.Fatal("forbidden path responded:", response)
			}
			return
		}
		if err != nil || response != os.Getenv("FY_NAT_EXPECT")+"|probe" {
			t.Fatal(response, err)
		}
		return
	}
	if mode == "persist" {
		c, err := net.DialTimeout("tcp", addr, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(5 * time.Second))
		if _, err = io.WriteString(c, "before\n"); err != nil {
			t.Fatal(err)
		}
		if _, err = bufio.NewReader(c).ReadString('\n'); err != nil {
			t.Fatal(err)
		}
		fmt.Println("READY")
		if _, err = bufio.NewReader(os.Stdin).ReadString('\n'); err != nil {
			t.Fatal(err)
		}
		c.SetDeadline(time.Now().Add(time.Second))
		_, err = io.WriteString(c, "after\n")
		if err == nil {
			_, err = bufio.NewReader(c).ReadString('\n')
		}
		if err == nil {
			t.Fatal("established connection bypassed new isolation")
		}
		return
	}
	if mode != "serve" {
		t.Fatal("unknown test helper mode")
	}
	if proto == "tcp" {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		fmt.Println("READY")
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				reader := bufio.NewReader(c)
				for {
					b, err := reader.ReadString('\n')
					if err != nil {
						return
					}
					host, _, _ := net.SplitHostPort(c.RemoteAddr().String())
					if _, err = io.WriteString(c, host+"|"+b); err != nil {
						return
					}
				}
			}()
		}
	}
	c, err := net.ListenPacket("udp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fmt.Println("READY")
	b := make([]byte, 1024)
	for {
		n, a, err := c.ReadFrom(b)
		if err != nil {
			return
		}
		host, _, _ := net.SplitHostPort(a.String())
		if _, err = c.WriteTo([]byte(host+"|"+string(b[:n])), a); err != nil {
			return
		}
	}
}
func natExchange(proto, addr string) (string, error) {
	c, err := net.DialTimeout(proto, addr, time.Second)
	if err != nil {
		return "", err
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(time.Second))
	if _, err = io.WriteString(c, "probe\n"); err != nil {
		return "", err
	}
	b, err := bufio.NewReader(c).ReadString('\n')
	return strings.TrimSpace(b), err
}
func natChild(t *testing.T, ns, mode, proto, addr, expect string, deny bool) *exec.Cmd {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("ip", "netns", "exec", ns, self, "-test.run", "^TestNativeNATSocketPeer$", "-test.timeout", "45s")
	cmd.Env = append(os.Environ(), "FY_NAT_CHILD="+mode, "FY_NAT_PROTO="+proto, "FY_NAT_ADDR="+addr, "FY_NAT_EXPECT="+expect)
	if deny {
		cmd.Env = append(cmd.Env, "FY_NAT_DENY=1")
	}
	cmd.Stderr = os.Stderr
	return cmd
}
func natServer(t *testing.T, ns, proto, addr string) {
	t.Helper()
	cmd := natChild(t, ns, "serve", proto, addr, "", false)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	ready := make(chan string, 1)
	go func() { line, _ := bufio.NewReader(stdout).ReadString('\n'); ready <- line }()
	select {
	case line := <-ready:
		if line != "READY\n" {
			t.Fatal("socket helper failed:", line)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("socket helper readiness timeout")
	}
}
func natProbe(t *testing.T, ns, proto, addr, expected string, deny bool) {
	t.Helper()
	cmd := natChild(t, ns, "probe", proto, addr, expected, deny)
	if b, err := cmd.Output(); err != nil {
		t.Fatalf("probe %s/%s from %s deny=%v: %v %s", proto, addr, ns, deny, err, b)
	}
}
func natTransport(t *testing.T, hostName, ns, hostIP, guestIP, alias string) {
	t.Helper()
	runNative(t, "ip", "netns", "add", ns)
	t.Cleanup(func() {
		_ = ip(context.Background(), "", "link", "delete", "dev", hostName)
		_, _ = host.Run(context.Background(), host.CommandRequest{Name: "ip", Args: []string{"netns", "delete", ns}})
	})
	runNative(t, "ip", "link", "add", hostName, "type", "veth", "peer", "name", "eth0", "netns", ns)
	if alias != "" {
		runNative(t, "ip", "link", "set", "dev", hostName, "alias", alias)
	}
	runNative(t, "ip", "addr", "add", hostIP, "dev", hostName)
	runNative(t, "ip", "link", "set", "dev", hostName, "up")
	runNative(t, "ip", "-n", ns, "addr", "add", guestIP, "dev", "eth0")
	runNative(t, "ip", "-n", ns, "link", "set", "dev", "eth0", "up")
	runNative(t, "ip", "-n", ns, "link", "set", "dev", "lo", "up")
}
func installNATFirewall(t *testing.T, p Plan) {
	t.Helper()
	b, err := PrivateFirewall(p, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = host.Run(context.Background(), nftRequest("", []string{"-j", "-f", "-"}, b)); err != nil {
		t.Fatal(err)
	}
	handle, err := firewallHandle(context.Background(), p, "")
	if err != nil || handle == 0 {
		t.Fatal(handle, err)
	}
	t.Cleanup(func() {
		actual, err := firewallHandle(context.Background(), p, "")
		if err == nil && actual == handle {
			_, _ = host.Run(context.Background(), host.CommandRequest{Name: "nft", Args: []string{"delete", "table", "inet", p.Table}})
		}
	})
}

func requireNFTConntrack(t *testing.T) {
	t.Helper()
	name := "ctprobe_" + short(state.UUID())[:12]
	runNative(t, "nft", "add", "table", "inet", name)
	defer func() {
		_, _ = host.Run(context.Background(), host.CommandRequest{Name: "nft", Args: []string{"delete", "table", "inet", name}})
	}()
	runNative(t, "nft", "add", "chain", "inet", name, "probe")
	_, err := host.Run(context.Background(), host.CommandRequest{Name: "nft", Args: []string{"add", "rule", "inet", name, "probe", "ct", "state", "established", "accept"}})
	if err != nil {
		if strings.Contains(err.Error(), "No such file or directory") || strings.Contains(err.Error(), "Operation not supported") {
			t.Skipf("BLOCKED: kernel nft conntrack expression unavailable: %v", err)
		}
		t.Fatal(err)
	}
}

// These tests cover real IPv4 host rules with veth endpoint namespaces. They do
// not substitute for TAP/bridge IPv6, Firecracker guests or a second real host.
func TestNativeNATAndMappingsPackets(t *testing.T) {
	nativeOnly(t)
	requireNFTConntrack(t)
	loop, err := net.InterfaceByName("lo")
	if err != nil {
		t.Fatal(err)
	}
	if loop.Flags&net.FlagUp == 0 {
		runNative(t, "ip", "link", "set", "dev", "lo", "up")
		t.Cleanup(func() { _ = ip(context.Background(), "", "link", "set", "dev", "lo", "down") })
	}
	for _, nat := range []bool{false, true} {
		t.Run(fmt.Sprintf("nat-%v", nat), func(t *testing.T) {
			ctx := context.Background()
			p := testPlan(t, 70)
			hash := short(state.UUID())[:10]
			external := "nx" + hash
			guest := "ng" + hash
			uplink := "nu" + hash
			natTransport(t, uplink, external, "192.0.2.1/24", "192.0.2.2/24", "")
			natTransport(t, p.HostLink, guest, p.Gateway.String()+"/24", p.Nodes[0].IPv4.String()+"/24", p.Owner)
			runNative(t, "ip", "-n", guest, "route", "add", "default", "via", p.Gateway.String())
			runNative(t, "ip", "-n", external, "route", "add", p.CIDR.String(), "via", "192.0.2.1")
			previous, err := os.ReadFile("/proc/sys/net/ipv4/ip_forward")
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1\n"), 0); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.WriteFile("/proc/sys/net/ipv4/ip_forward", previous, 0) })
			for _, proto := range []string{"tcp", "udp"} {
				natServer(t, guest, proto, "0.0.0.0:19022")
				natServer(t, external, proto, "0.0.0.0:19023")
			}
			r := ConnectivityRequest{NAT: nat}
			if nat {
				r.EgressInterface = uplink
			}
			for i, proto := range []string{"tcp", "udp"} {
				r.Forwards = append(r.Forwards, ForwardRequest{Protocol: proto, BindAddress: "127.0.0.1", HostPort: 20000 + i, Node: p.Nodes[0].Name, GuestPort: 19022}, ForwardRequest{Protocol: proto, BindAddress: "192.0.2.1", HostPort: 21000 + i, Node: p.Nodes[0].Name, GuestPort: 19022})
			}
			snapshot, err := ReadSnapshot(ctx)
			if err != nil {
				t.Fatal(err)
			}
			p, err = ResolveHostConnectivity(ctx, p, r, snapshot)
			if err != nil {
				t.Fatal(err)
			}
			installNATFirewall(t, p)
			if err = allowLoopbackMappings(ctx, p); err != nil {
				t.Fatal(err)
			}
			for _, f := range p.Forwards {
				addr := fmt.Sprintf("%s:%d", f.BindAddress, f.HostPort)
				response, err := natExchange(f.Protocol, addr)
				if err != nil || response != p.Gateway.String()+"|probe" {
					t.Fatal("host mapping", addr, response, err)
				}
				if !f.BindAddress.IsLoopback() {
					natProbe(t, external, f.Protocol, addr, p.Gateway.String(), false)
				}
			}
			for _, proto := range []string{"tcp", "udp"} {
				natProbe(t, guest, proto, "192.0.2.2:19023", "192.0.2.1", !nat)
				natProbe(t, external, proto, net.JoinHostPort(p.Nodes[0].IPv4.String(), "19022"), "", true)
			}
			foreign := fmt.Sprintf("foreign_%s", hash)
			runNative(t, "nft", "add", "table", "inet", foreign)
			t.Cleanup(func() {
				_, _ = host.Run(ctx, host.CommandRequest{Name: "nft", Args: []string{"delete", "table", "inet", foreign}})
			})
			runNative(t, "nft", "add", "chain", "inet", foreign, "forward", "{ type filter hook forward priority 10; policy drop; }")
			diagnostics, err := PolicyDiagnostics(ctx, p)
			if err != nil || len(diagnostics) == 0 || !strings.Contains(strings.Join(diagnostics, " "), foreign) {
				t.Fatal(diagnostics, err)
			}
			natProbe(t, external, "tcp", "192.0.2.1:21000", "", true)
		})
	}
}

func TestNativeNATRejectsEstablishedCrossEnvironment(t *testing.T) {
	nativeOnly(t)
	requireNFTConntrack(t)
	a, b := testPlan(t, 71), testPlan(t, 72)
	nsA, nsB := "na"+short(state.UUID())[:10], "nb"+short(state.UUID())[:10]
	natTransport(t, a.HostLink, nsA, a.Gateway.String()+"/24", a.Nodes[0].IPv4.String()+"/24", a.Owner)
	natTransport(t, b.HostLink, nsB, b.Gateway.String()+"/24", b.Nodes[0].IPv4.String()+"/24", b.Owner)
	for _, v := range []struct{ ns, gateway string }{{nsA, a.Gateway.String()}, {nsB, b.Gateway.String()}} {
		runNative(t, "ip", "-n", v.ns, "route", "add", "default", "via", v.gateway)
	}
	previous, err := os.ReadFile("/proc/sys/net/ipv4/ip_forward")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1\n"), 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.WriteFile("/proc/sys/net/ipv4/ip_forward", previous, 0) })
	natServer(t, nsB, "tcp", "0.0.0.0:19022")
	cmd := natChild(t, nsA, "persist", "tcp", net.JoinHostPort(b.Nodes[0].IPv4.String(), "19022"), "", false)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || line != "READY\n" {
		t.Fatal(line, err)
	}
	// Existing ct state must not grant routing between different host links.
	a.NAT, a.EgressInterface = true, "outside0"
	natTransport(t, "outside0", "no"+short(state.UUID())[:10], "198.51.100.1/24", "198.51.100.2/24", "")
	installNATFirewall(t, a)
	installNATFirewall(t, b)
	if _, err = io.WriteString(stdin, "check\n"); err != nil {
		t.Fatal(err)
	}
	if err = cmd.Wait(); err != nil {
		t.Fatal(err)
	}
}

func TestNativeNATFirewallJournalRecoveryPreservesForeignTable(t *testing.T) {
	nativeOnly(t)
	requireNFTConntrack(t)
	for _, done := range []bool{false, true} {
		t.Run(fmt.Sprintf("done-%v", done), func(t *testing.T) {
			ctx := context.Background()
			p := testPlan(t, 73)
			p.NAT, p.EgressInterface = true, "outside0"
			p.Forwards = []Forward{{Protocol: "tcp", BindAddress: netip.MustParseAddr("127.0.0.1"), HostPort: 2022, Node: p.Nodes[0].Name, NodeID: p.Nodes[0].ID, GuestIP: p.Nodes[0].IPv4, GuestPort: 22}}
			j := testJournal(t, p)
			id, err := j.IntentIdentity("nft-host", map[string]string{"name": p.Table, "owner": p.Owner, "namespace": p.Namespace})
			if err != nil {
				t.Fatal(err)
			}
			foreign := "sentinel_" + short(state.UUID())[:10]
			runNative(t, "nft", "add", "table", "inet", foreign)
			t.Cleanup(func() {
				_, _ = host.Run(ctx, host.CommandRequest{Name: "nft", Args: []string{"delete", "table", "inet", foreign}})
			})
			installNATFirewall(t, p)
			if done {
				identity, exists, err := (Manager{}).observe(ctx, p, j.Resources[0])
				if err != nil || !exists {
					t.Fatal(identity, exists, err)
				}
				if err = j.Done(id, identity); err != nil {
					t.Fatal(err)
				}
			}
			if _, err = (Manager{}).Remove(ctx, p, j); err != nil {
				t.Fatal(err)
			}
			allReleased(t, j)
			if handle, err := firewallHandle(ctx, p, ""); err != nil || handle != 0 {
				t.Fatal(handle, err)
			}
			runNative(t, "nft", "list", "table", "inet", foreign)
		})
	}
}
