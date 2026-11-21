package firecracker

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"regexp"
	"strings"
	"time"
)

var sshID = regexp.MustCompile(`^SSH-(2\.0|1\.99)-[!-~]+( [\x20-\x7e]*)?\r\n$`)

// Probe checks reachability only; it never authenticates or executes guest commands.
func Probe(ctx context.Context, address, kind string) error {
	if kind == "none" {
		return nil
	}
	if kind != "tcp" && kind != "ssh_banner" {
		return fmt.Errorf("unknown probe %q", kind)
	}
	conn, e := (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "tcp", address)
	if e != nil {
		return e
	}
	defer conn.Close()
	if kind == "tcp" {
		return nil
	}
	deadline := time.Now().Add(2 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetReadDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	reader := bufio.NewReader(io.LimitReader(conn, 8192))
	for i := 0; i < 32; i++ {
		line, e := reader.ReadString('\n')
		if e != nil {
			return e
		}
		if strings.HasPrefix(line, "SSH-") {
			if len(line) > 255 || !sshID.MatchString(line) {
				return fmt.Errorf("invalid SSH identification")
			}
			return nil
		}
	}
	return fmt.Errorf("SSH identification not found")
}
func WaitProbe(ctx context.Context, address, kind string) error {
	for {
		if e := Probe(ctx, address, kind); e == nil {
			return nil
		}
		timer := time.NewTimer(200 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
