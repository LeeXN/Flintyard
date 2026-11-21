package image

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func rootTar(t *testing.T, headers []tar.Header) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "root.tar.gz")
	f, err := os.Create(file)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for _, h := range headers {
		if h.Typeflag != tar.TypeXGlobalHeader {
			h.Uid, h.Gid = os.Getuid(), os.Getgid()
			if h.ModTime.IsZero() {
				h.ModTime = time.Unix(1700000000, 0)
			}
		}
		if err = tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if h.Size > 0 {
			if _, err = tw.Write(make([]byte, h.Size)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err = tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err = gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	return file
}
func TestRootfsSystemLinksModesAndXattrs(t *testing.T) {
	archive := rootTar(t, []tar.Header{
		{Name: "./", Typeflag: tar.TypeDir, Mode: 0755},
		{Name: "./usr/bin/tool", Typeflag: tar.TypeReg, Mode: 04755, Size: 16, Xattrs: map[string]string{"user.fixture": "preserve"}},
		{Name: "./bin", Typeflag: tar.TypeSymlink, Linkname: "usr/bin", Mode: 0777},
		{Name: "./absolute", Typeflag: tar.TypeSymlink, Linkname: "/usr/bin/tool", Mode: 0777},
		{Name: "./usr/bin/hard", Typeflag: tar.TypeLink, Linkname: "./usr/bin/tool"},
	})
	dir := filepath.Join(t.TempDir(), "root")
	r, err := ExtractRootfs(context.Background(), ExtractRequest{Archive: archive, Destination: dir})
	if err != nil {
		t.Fatal(err)
	}
	if r.Files != 4 || r.Xattrs != 1 {
		t.Fatal(r)
	}
	a, _ := os.Stat(filepath.Join(dir, "usr/bin/tool"))
	b, _ := os.Stat(filepath.Join(dir, "usr/bin/hard"))
	if !os.SameFile(a, b) || a.Mode()&os.ModeSetuid == 0 {
		t.Fatal("hard link or setuid metadata lost")
	}
	for _, p := range []string{dir, filepath.Join(dir, "usr"), filepath.Join(dir, "usr/bin")} {
		st, err := os.Stat(p)
		if err != nil || st.Mode().Perm() != 0755 {
			t.Fatal(p, st, err)
		}
	}
	value := make([]byte, 64)
	n, err := unix.Getxattr(filepath.Join(dir, "usr/bin/tool"), "user.fixture", value)
	if err != nil || string(value[:n]) != "preserve" {
		t.Fatal(err)
	}
	if link, err := os.Readlink(filepath.Join(dir, "absolute")); err != nil || link != "/usr/bin/tool" {
		t.Fatal(link, err)
	}
}

func TestRootfsPreservesLinuxSystemdEscapedNames(t *testing.T) {
	name := `usr/lib/systemd/system/system-systemd\x2dcryptsetup.slice`
	archive := rootTar(t, []tar.Header{
		{Name: name, Typeflag: tar.TypeReg, Mode: 0644, Size: 1},
		{Name: "unit-hardlink", Typeflag: tar.TypeLink, Linkname: name},
	})
	root := filepath.Join(t.TempDir(), "root")
	if _, err := ExtractRootfs(context.Background(), ExtractRequest{Archive: archive, Destination: root}); err != nil {
		t.Fatal(err)
	}
	a, err := os.Stat(filepath.Join(root, name))
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.Stat(filepath.Join(root, "unit-hardlink"))
	if err != nil || !os.SameFile(a, b) {
		t.Fatal("systemd escaped name or hard link changed", err)
	}
}
func TestRootfsRejectsEscapeAndLinkTraversalWithoutHostMutation(t *testing.T) {
	for _, headers := range [][]tar.Header{
		{{Name: "../escape", Typeflag: tar.TypeReg, Size: 1}},
		{{Name: "/escape", Typeflag: tar.TypeReg, Size: 1}},
		{{Name: "bin", Typeflag: tar.TypeSymlink, Linkname: "/tmp"}, {Name: "bin/escape", Typeflag: tar.TypeReg, Size: 1}},
		{{Name: "alias", Typeflag: tar.TypeSymlink, Linkname: "inside"}, {Name: "alias/file", Typeflag: tar.TypeReg, Size: 1}},
		{{Name: "hard", Typeflag: tar.TypeLink, Linkname: "../host"}},
		{{Name: "dev/null", Typeflag: tar.TypeChar, Devmajor: 1, Devminor: 3}},
		{{Name: "fifo", Typeflag: tar.TypeFifo}},
		{{Name: "same", Typeflag: tar.TypeReg, Size: 1}, {Name: "same", Typeflag: tar.TypeReg, Size: 1}},
	} {
		archive := rootTar(t, headers)
		parent := t.TempDir()
		dir := filepath.Join(parent, "root")
		if _, err := ExtractRootfs(context.Background(), ExtractRequest{Archive: archive, Destination: dir}); err == nil {
			t.Fatal("accepted unsafe archive", headers)
		}
		if entries, _ := os.ReadDir(parent); len(entries) != 0 {
			t.Fatal("failed extraction left root/files", entries)
		}
	}
}
func TestRootfsStripLimitsCancellationAndCorruption(t *testing.T) {
	archive := rootTar(t, []tar.Header{{Name: "linux/", Typeflag: tar.TypeDir, Mode: 0755}, {Name: "linux/Makefile", Typeflag: tar.TypeReg, Size: 16, Mode: 0644}})
	dir := filepath.Join(t.TempDir(), "source")
	if _, err := ExtractRootfs(context.Background(), ExtractRequest{Archive: archive, Destination: dir, StripComponents: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "Makefile")); err != nil {
		t.Fatal(err)
	}
	for _, r := range []ExtractRequest{{Archive: archive, MaxFiles: 1}, {Archive: archive, MaxBytes: 8}} {
		r.Destination = filepath.Join(t.TempDir(), "root")
		if _, err := ExtractRootfs(context.Background(), r); err == nil {
			t.Fatal("ignored limits")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ExtractRootfs(ctx, ExtractRequest{Archive: archive, Destination: filepath.Join(t.TempDir(), "root")}); err == nil {
		t.Fatal("ignored cancellation")
	}
	data, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)-5] ^= 1
	if err = os.WriteFile(archive, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = ExtractRootfs(context.Background(), ExtractRequest{Archive: archive, Destination: filepath.Join(t.TempDir(), "root")}); err == nil {
		t.Fatal("accepted corrupt compressed trailer")
	}
}

func TestRootfsGlobalPAXCommentAndUnsafeGlobalMetadata(t *testing.T) {
	for _, tc := range []struct {
		key   string
		valid bool
	}{{"comment", true}, {"path", false}, {"uid", false}, {"SCHILY.xattr.user.global", false}} {
		archive := rootTar(t, []tar.Header{{Name: "pax_global_header", Typeflag: tar.TypeXGlobalHeader, PAXRecords: map[string]string{tc.key: "123"}}, {Name: "linux/Makefile", Typeflag: tar.TypeReg, Size: 16, Mode: 0644}})
		dest := filepath.Join(t.TempDir(), "source")
		_, err := ExtractRootfs(context.Background(), ExtractRequest{Archive: archive, Destination: dest, StripComponents: 1})
		if (err == nil) != tc.valid {
			t.Fatal(tc, err)
		}
	}
}
