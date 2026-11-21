// Package firecracker implements the pinned local Unix-socket API boundary.
// It uses only the official jailer and the pinned local API.
package firecracker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/LeeXN/Flintyard/internal/host"
)

const Version = host.FirecrackerVersion
const maxResponse = 1 << 20

type API struct {
	client    *http.Client
	transport *http.Transport
}

func NewAPI(socket string) *API {
	tr := &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: 2 * time.Second}).DialContext(ctx, "unix", socket)
	}, MaxIdleConns: 1, ResponseHeaderTimeout: 3 * time.Second}
	return &API{client: &http.Client{Transport: tr, Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, transport: tr}
}
func (a *API) Close() { a.transport.CloseIdleConnections() }
func (a *API) Call(ctx context.Context, method, path string, input, output any) error {
	var b []byte
	var e error
	if input != nil {
		b, e = json.Marshal(input)
		if e != nil {
			return e
		}
	}
	req, e := http.NewRequestWithContext(ctx, method, "http://localhost"+path, bytes.NewReader(b))
	if e != nil {
		return e
	}
	req.Header.Set("Content-Type", "application/json")
	r, e := a.client.Do(req)
	if e != nil {
		return e
	}
	defer r.Body.Close()
	b, e = io.ReadAll(io.LimitReader(r.Body, maxResponse+1))
	if e != nil {
		return e
	}
	if len(b) > maxResponse {
		return fmt.Errorf("Firecracker API response exceeds limit")
	}
	if r.StatusCode < 200 || r.StatusCode >= 300 {
		return fmt.Errorf("Firecracker API %s %s: HTTP %d: %.1024s", method, path, r.StatusCode, b)
	}
	if output != nil {
		if e = json.Unmarshal(b, output); e != nil {
			return fmt.Errorf("invalid Firecracker response: %w", e)
		}
	}
	return nil
}

type Configuration struct {
	VCPUs                                                int
	MemoryMiB                                            int
	Kernel, Initrd, Args, Rootfs, Seed, Tap, MAC, Logger string
}

// Configure applies all devices before InstanceStart; an error prevents launch.
func (a *API) Configure(ctx context.Context, c Configuration) error {
	if c.VCPUs < 1 || c.VCPUs > 32 || c.MemoryMiB < 128 {
		return fmt.Errorf("invalid machine resources for Firecracker v%s", Version)
	}
	boot := map[string]any{"kernel_image_path": c.Kernel, "boot_args": c.Args}
	if c.Initrd != "" {
		boot["initrd_path"] = c.Initrd
	}
	calls := []struct {
		path  string
		value any
	}{{"/machine-config", map[string]any{"vcpu_count": c.VCPUs, "mem_size_mib": c.MemoryMiB, "smt": false}}, {"/boot-source", boot}, {"/drives/rootfs", map[string]any{"drive_id": "rootfs", "path_on_host": c.Rootfs, "is_root_device": true, "is_read_only": false}}}
	if c.Seed != "" {
		calls = append(calls, struct {
			path  string
			value any
		}{"/drives/seed", map[string]any{"drive_id": "seed", "path_on_host": c.Seed, "is_root_device": false, "is_read_only": true}})
	}
	calls = append(calls, struct {
		path  string
		value any
	}{"/network-interfaces/eth0", map[string]any{"iface_id": "eth0", "host_dev_name": c.Tap, "guest_mac": c.MAC}})
	for _, call := range calls {
		if e := a.Call(ctx, http.MethodPut, call.path, call.value, nil); e != nil {
			return e
		}
	}
	if c.Logger != "" {
		if err := a.Call(ctx, http.MethodPut, "/logger", map[string]any{"log_path": c.Logger, "level": "Info", "show_level": true, "show_log_origin": true}, nil); err != nil {
			return err
		}
	}
	return a.Call(ctx, http.MethodPut, "/actions", map[string]string{"action_type": "InstanceStart"}, nil)
}
