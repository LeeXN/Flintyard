package flintyard

import (
	"context"
	"errors"
	"net/netip"
	"testing"

	"github.com/LeeXN/Flintyard/internal/network"
)

func TestNetworkResultConnectionPathsAndTypedFailures(t *testing.T) {
	p := network.Plan{NAT: true, EgressInterface: "eth0", Forwards: []network.Forward{{Protocol: "tcp", BindAddress: netip.MustParseAddr("127.0.0.1"), HostPort: 2022, Node: "node", GuestPort: 22}, {Protocol: "udp", BindAddress: netip.MustParseAddr("192.0.2.1"), HostPort: 5353, Node: "node", GuestPort: 53}}}
	r := networkResult(p)
	if r.Forwards[0].HostEndpoint != "127.0.0.1:2022" || r.Forwards[0].ExternalEndpoint != "" || r.Forwards[1].ExternalEndpoint != "192.0.2.1:5353" || r.Forwards[1].ExternalVerified {
		t.Fatal(r)
	}
	for _, tc := range []struct {
		cause error
		code  string
		exit  int
	}{{network.ErrForwardingDisabled, "E_FORWARDING_DISABLED", 3}, {network.ErrPortInUse, "E_PORT_IN_USE", 6}, {network.ErrFirewall, "E_FIREWALL_CONFLICT", 6}, {context.Canceled, "E_CANCELLED", 130}} {
		err := networkFailure("preflight", tc.cause)
		if err.Code != tc.code || !errors.Is(err, tc.cause) || ExitCode(err) != tc.exit {
			t.Fatal(tc, err)
		}
	}
}
