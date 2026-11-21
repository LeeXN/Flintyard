package firecracker

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestConfigureOrdering(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "api")
	l, e := net.Listen("unix", socket)
	if e != nil {
		t.Fatal(e)
	}
	defer l.Close()
	var paths []string
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		var m map[string]any
		if e := json.NewDecoder(r.Body).Decode(&m); e != nil {
			t.Error(e)
		}
		if r.URL.Path == "/actions" && m["action_type"] != "InstanceStart" {
			t.Error(m)
		}
		w.WriteHeader(204)
	})}
	defer server.Close()
	go server.Serve(l)
	a := NewAPI(socket)
	defer a.Close()
	if e = a.Configure(context.Background(), Configuration{VCPUs: 2, MemoryMiB: 128, Kernel: "/kernel", Rootfs: "/rootfs", Seed: "/seed", Tap: "tap0", MAC: "02:00:00:00:00:01"}); e != nil {
		t.Fatal(e)
	}
	if strings.Join(paths, ",") != "/machine-config,/boot-source,/drives/rootfs,/drives/seed,/network-interfaces/eth0,/actions" {
		t.Fatal(paths)
	}
}
func TestAPIErrorDoesNotStart(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "api")
	l, e := net.Listen("unix", socket)
	if e != nil {
		t.Fatal(e)
	}
	defer l.Close()
	started := false
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/actions" {
			started = true
		}
		w.WriteHeader(400)
	})}
	defer server.Close()
	go server.Serve(l)
	a := NewAPI(socket)
	defer a.Close()
	if e = a.Configure(context.Background(), Configuration{VCPUs: 1, MemoryMiB: 128}); e == nil || started {
		t.Fatal("started after device configuration failed")
	}
}
func TestProbe(t *testing.T) {
	for _, tc := range []struct {
		banner string
		ok     bool
	}{{"SSH-2.0-OpenSSH_9.0\r\n", true}, {"notice\r\nSSH-1.99-test\r\n", true}, {"SSH-1.0-test\r\n", false}, {"SSH-2.0-test\n", false}, {"HTTP/1.0 200 OK\r\n", false}, {"SSH-2.0-\r\n", false}} {
		t.Run(strconv.Quote(tc.banner), func(t *testing.T) {
			l, e := net.Listen("tcp", "127.0.0.1:0")
			if e != nil {
				t.Fatal(e)
			}
			defer l.Close()
			go func() {
				c, e := l.Accept()
				if e == nil {
					defer c.Close()
					_, _ = c.Write([]byte(tc.banner))
				}
			}()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			e = Probe(ctx, l.Addr().String(), "ssh_banner")
			if (e == nil) != tc.ok {
				t.Fatal(e)
			}
		})
	}
}
func TestProcIdentity(t *testing.T) {
	r, e := Observe(os.Getpid())
	if e != nil {
		t.Fatal(e)
	}
	if e = Verify(r); e != nil {
		t.Fatal(e)
	}
	r.StartTime++
	if Verify(r) == nil {
		t.Fatal("accepted reused identity")
	}
	f := []string{"S"}
	for i := 0; i < 18; i++ {
		f = append(f, "0")
	}
	f = append(f, "123")
	n, e := ParseStartTime("123 (weird ) process) " + strings.Join(f, " "))
	if e != nil || n != 123 {
		t.Fatal(n, e)
	}
}
