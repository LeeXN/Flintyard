package image

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func fixture(t *testing.T) (string, string) {
	t.Helper()
	d := t.TempDir()
	b, e := os.ReadFile("../../examples/import-spec.json")
	if e != nil {
		t.Fatal(e)
	}
	var s Spec
	if e = json.Unmarshal(b, &s); e != nil {
		t.Fatal(e)
	}
	s.Initialization.Mode = "external"
	s.Initialization.Profile = ""
	s.Kernel.Config = ""
	for i := range s.Inputs {
		p := filepath.Join(d, s.Inputs[i].Role)
		data := []byte("test " + s.Inputs[i].Role)
		if e = os.WriteFile(p, data, 0600); e != nil {
			t.Fatal(e)
		}
		s.Inputs[i].Path = s.Inputs[i].Role
		s.Inputs[i].SHA256 = Sum(data)
	}
	b, _ = json.Marshal(s)
	spec := filepath.Join(d, "spec.json")
	if e = os.WriteFile(spec, b, 0600); e != nil {
		t.Fatal(e)
	}
	return d, spec
}
func TestImportRoundTripAndTamper(t *testing.T) {
	ctx := context.Background()
	d, s := fixture(t)
	p := filepath.Join(d, "test.fyimg")
	sum, e := PackSpec(ctx, s, p)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = PackSpec(ctx, s, p); e == nil {
		t.Fatal("overwrote existing package")
	}
	root := filepath.Join(d, "state")
	r, e := Import(ctx, root, p, sum, Limits{})
	if e != nil {
		t.Fatal(e)
	}
	if r.SourceTrust != "external_digest_verified" || r.Validation != "not_run" {
		t.Fatal(r)
	}
	got, e := Cached(ctx, root, r.ID)
	if e != nil || got.ID != r.ID {
		t.Fatal(e)
	}
	if _, e = Import(ctx, root, p, sum, Limits{}); e != nil {
		t.Fatal(e)
	}
	cache, _ := cacheDir(root, r.ID)
	payload := filepath.Join(cache, r.Manifest.Boot.Rootfs)
	if e = os.Chmod(payload, 0600); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(payload, []byte("tampered"), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e = Cached(ctx, root, r.ID); e == nil {
		t.Fatal("accepted modified cache")
	}
	if _, e = Import(ctx, root, p, sum, Limits{}); e == nil {
		t.Fatal("silently repaired cache")
	}
}

func TestPCIVirtioSurvivesPackAndImport(t *testing.T) {
	for _, arch := range []string{"x86_64", "aarch64"} {
		t.Run(arch, func(t *testing.T) {
			ctx := context.Background()
			dir, spec := fixture(t)
			b, err := os.ReadFile(spec)
			if err != nil {
				t.Fatal(err)
			}
			var input Spec
			if err := json.Unmarshal(b, &input); err != nil {
				t.Fatal(err)
			}
			input.Architecture, input.Boot.VirtioTransport = arch, "pci"
			b, err = json.Marshal(input)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(spec, b, 0600); err != nil {
				t.Fatal(err)
			}
			pkg := filepath.Join(dir, "pci.fyimg")
			sum, err := PackSpec(ctx, spec, pkg)
			if err != nil {
				t.Fatal(err)
			}
			got, err := Import(ctx, filepath.Join(dir, "state"), pkg, sum, Limits{})
			if err != nil || got.Manifest.Boot.VirtioTransport != "pci" || got.Manifest.Architecture != arch {
				t.Fatal(got, err)
			}
			input.Boot.Args = []string{"pci=noaer,off"}
			b, _ = json.Marshal(input)
			if err := os.WriteFile(spec, b, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := PackSpec(ctx, spec, filepath.Join(dir, "bad.fyimg")); err == nil {
				t.Fatal("packed incompatible PCI command line")
			}
		})
	}
}
func TestWrongDigestAndLimitsLeaveNoCache(t *testing.T) {
	d, s := fixture(t)
	p := filepath.Join(d, "test.fyimg")
	if _, e := PackSpec(context.Background(), s, p); e != nil {
		t.Fatal(e)
	}
	for _, test := range []struct {
		sha string
		l   Limits
	}{{strings.Repeat("1", 64), Limits{}}, {"", Limits{MaxBytes: 8}}, {"", Limits{MaxFiles: 1}}} {
		root := filepath.Join(t.TempDir(), "state")
		if _, e := Import(context.Background(), root, p, test.sha, test.l); e == nil {
			t.Fatal("accepted bad input")
		}
		entries, _ := os.ReadDir(filepath.Join(root, "images"))
		if len(entries) != 0 {
			t.Fatal("dirty cache", entries)
		}
	}
}
func TestForbiddenArchives(t *testing.T) {
	for _, tc := range []struct {
		name      string
		kind      byte
		duplicate bool
	}{{"../outside", tar.TypeReg, false}, {"/outside", tar.TypeReg, false}, {`payload/systemd\x2dunit`, tar.TypeReg, false}, {"payload/link", tar.TypeSymlink, false}, {"payload/link", tar.TypeLink, false}, {"payload/dev", tar.TypeChar, false}, {"payload/fifo", tar.TypeFifo, false}, {"payload/x", tar.TypeReg, true}} {
		t.Run(tc.name+string(tc.kind), func(t *testing.T) {
			d := t.TempDir()
			p := filepath.Join(d, "bad.fyimg")
			f, e := os.Create(p)
			if e != nil {
				t.Fatal(e)
			}
			gz := gzip.NewWriter(f)
			tw := tar.NewWriter(gz)
			n := 1
			if tc.duplicate {
				n = 2
			}
			for i := 0; i < n; i++ {
				if e = tw.WriteHeader(&tar.Header{Name: tc.name, Typeflag: tc.kind, Mode: 0600, Linkname: "/etc/passwd"}); e != nil {
					t.Fatal(e)
				}
			}
			tw.Close()
			gz.Close()
			f.Close()
			if _, e = Inspect(context.Background(), p, Limits{}); e == nil {
				t.Fatal("accepted unsafe archive")
			}
		})
	}
}
func TestConcurrentImport(t *testing.T) {
	d, s := fixture(t)
	p := filepath.Join(d, "test.fyimg")
	if _, e := PackSpec(context.Background(), s, p); e != nil {
		t.Fatal(e)
	}
	root := filepath.Join(d, "state")
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, e := Import(context.Background(), root, p, "", Limits{}); errs <- e }()
	}
	wg.Wait()
	close(errs)
	n := 0
	for e := range errs {
		if e == nil {
			n++
		} else if !strings.Contains(e.Error(), "lock held") {
			t.Fatal(e)
		}
	}
	if n < 1 {
		t.Fatal("no successful import")
	}
	files, _ := os.ReadDir(filepath.Join(root, "images"))
	for _, f := range files {
		if strings.HasPrefix(f.Name(), ".staging") {
			t.Fatal("stale staging")
		}
	}
}
func TestCancelAndInputSymlink(t *testing.T) {
	d, s := fixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e := PackSpec(ctx, s, filepath.Join(d, "test.fyimg")); e == nil {
		t.Fatal("ignored cancellation")
	}
	p := filepath.Join(d, "link")
	if e := os.Symlink(s, p); e != nil {
		t.Fatal(e)
	}
	if _, _, e := Hash(context.Background(), p); e == nil {
		t.Fatal("accepted symlink")
	}
}
func TestRejectZeroDigests(t *testing.T) {
	if DigestValid(strings.Repeat("0", 64)) {
		t.Fatal("zero digest")
	}
}
