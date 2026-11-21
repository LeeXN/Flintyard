package flintyard

import (
	"context"
	"errors"
	"net"
	"os"
	"strconv"

	"github.com/LeeXN/Flintyard/internal/network"
)

func networkFailure(stage string, err error) *Error {
	code := "E_NETWORK_CONFLICT"
	switch {
	case errors.Is(err, network.ErrForwardingDisabled):
		code = "E_FORWARDING_DISABLED"
	case errors.Is(err, network.ErrPortInUse):
		code = "E_PORT_IN_USE"
	case errors.Is(err, network.ErrFirewall):
		code = "E_FIREWALL_CONFLICT"
	case errors.Is(err, context.Canceled):
		code = "E_CANCELLED"
	case os.IsPermission(err):
		code = "E_PERMISSION"
	}
	return Failure(code, stage, err)
}
func networkResult(p network.Plan) *NetworkResult {
	r := &NetworkResult{NAT: p.NAT, EgressInterface: p.EgressInterface, Forwards: []ForwardResult{}, Diagnostics: []string{}}
	for _, f := range p.Forwards {
		v := ForwardResult{Protocol: f.Protocol, BindAddress: f.BindAddress.String(), HostPort: f.HostPort, Node: f.Node, GuestPort: f.GuestPort, HostEndpoint: net.JoinHostPort(f.BindAddress.String(), strconv.Itoa(f.HostPort))}
		if !f.BindAddress.IsLoopback() {
			v.ExternalEndpoint = v.HostEndpoint
		}
		r.Forwards = append(r.Forwards, v)
	}
	return r
}
