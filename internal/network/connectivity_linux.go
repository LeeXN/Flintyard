package network

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"net/netip"
	"os"
	"strconv"
	"strings"
)

func readConnectivityHost(ctx context.Context, r ConnectivityRequest, snapshot Snapshot) (ConnectivityHost, error) {
	h := ConnectivityHost{Snapshot: snapshot}
	if !r.NAT && len(r.Forwards) == 0 {
		return h, nil
	}
	if err := ctx.Err(); err != nil {
		return h, err
	}
	b, err := os.ReadFile("/proc/sys/net/ipv4/ip_forward")
	if err != nil {
		return h, err
	}
	value := strings.TrimSpace(string(b))
	if value != "0" && value != "1" {
		return h, fmt.Errorf("invalid IPv4 forwarding snapshot")
	}
	h.Forwarding = value == "1"
	if len(r.Forwards) > 0 {
		for _, name := range []string{"tcp", "udp", "tcp6", "udp6"} {
			f, err := os.Open("/proc/net/" + name)
			if os.IsNotExist(err) && strings.HasSuffix(name, "6") {
				continue
			}
			if err != nil {
				return h, err
			}
			sockets, err := parseSockets(ctx, f, name)
			f.Close()
			if err != nil {
				return h, err
			}
			h.Sockets = append(h.Sockets, sockets...)
		}
	}
	return h, nil
}
func PreflightConnectivity(ctx context.Context, r ConnectivityRequest) (ConnectivityRequest, error) {
	if !r.NAT && len(r.Forwards) == 0 && r.EgressInterface == "" {
		return r, ctx.Err()
	}
	s, err := ReadSnapshot(ctx)
	if err != nil {
		return r, err
	}
	h, err := readConnectivityHost(ctx, r, s)
	if err != nil {
		return r, err
	}
	return resolveRequest(r, h)
}
func ResolveHostConnectivity(ctx context.Context, p Plan, r ConnectivityRequest, s Snapshot) (Plan, error) {
	h, err := readConnectivityHost(ctx, r, s)
	if err != nil {
		return p, err
	}
	return ResolveConnectivity(p, r, h)
}
func CheckPlanHost(ctx context.Context, p Plan) error {
	if err := p.Validate(); err != nil {
		return err
	}
	_, err := PreflightConnectivity(ctx, requestFromPlan(p))
	return err
}
func parseSockets(ctx context.Context, r io.Reader, name string) ([]Socket, error) {
	var result []Socket
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	if !scanner.Scan() {
		return nil, fmt.Errorf("missing socket table header: %s", name)
	}
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		fields := strings.Fields(scanner.Text())
		if len(fields) < 4 {
			return nil, fmt.Errorf("malformed socket table %s", name)
		}
		if strings.HasPrefix(name, "tcp") && fields[3] != "0A" {
			continue
		}
		parts := strings.Split(fields[1], ":")
		if len(parts) != 2 {
			return nil, fmt.Errorf("malformed socket endpoint")
		}
		b, err := hex.DecodeString(parts[0])
		if err != nil || len(b) != 4 && len(b) != 16 {
			return nil, fmt.Errorf("invalid socket address")
		}
		for i := 0; i < len(b); i += 4 {
			word := binary.NativeEndian.Uint32(b[i : i+4])
			binary.BigEndian.PutUint32(b[i:i+4], word)
		}
		a, ok := netip.AddrFromSlice(b)
		if !ok {
			return nil, fmt.Errorf("invalid socket address")
		}
		port, err := strconv.ParseUint(parts[1], 16, 16)
		if err != nil {
			return nil, err
		}
		result = append(result, Socket{Protocol: strings.TrimSuffix(name, "6"), Address: a.Unmap(), Port: int(port)})
	}
	return result, scanner.Err()
}
