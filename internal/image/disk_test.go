package image

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestRootfsCopiesAreIndependentAndDoNotOverwrite(t *testing.T) {
	ctx := context.Background()
	d, spec := fixture(t)
	p := filepath.Join(d, "test.fyimg")
	if _, err := PackSpec(ctx, spec, p); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(d, "state")
	r, err := Import(ctx, root, p, "", Limits{})
	if err != nil {
		t.Fatal(err)
	}
	a, err := CloneRootfs(ctx, root, r.ID, filepath.Join(d, "a.raw"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := CloneRootfs(ctx, root, r.ID, filepath.Join(d, "b.raw"))
	if err != nil {
		t.Fatal(err)
	}
	cache, _ := cacheDir(root, r.ID)
	source := filepath.Join(cache, r.Manifest.Boot.Rootfs)
	ast, _ := os.Stat(a.Path)
	bst, _ := os.Stat(b.Path)
	cst, _ := os.Stat(source)
	if os.SameFile(ast, bst) || os.SameFile(ast, cst) || os.SameFile(bst, cst) {
		t.Fatal("writable inode shared")
	}
	if ast.Mode().Perm() != 0600 || cst.Mode().Perm() != 0400 {
		t.Fatal("disk or cache permissions")
	}
	if err = os.WriteFile(a.Path, []byte("node A private changes"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{b.Path, source} {
		got, _ := os.ReadFile(name)
		if string(got) != "test rootfs" {
			t.Fatalf("disk contaminated: %s", name)
		}
	}
	if _, err = CloneRootfs(ctx, root, r.ID, a.Path); !errors.Is(err, os.ErrExist) {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(a.Path)
	if string(got) != "node A private changes" {
		t.Fatal("restart data overwritten")
	}
	ctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err = CloneRootfs(ctx, root, r.ID, filepath.Join(d, "cancel.raw")); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(d, "cancel.raw")); !os.IsNotExist(err) {
		t.Fatal("published cancelled disk")
	}
	stages, _ := filepath.Glob(filepath.Join(d, ".fy-file-*"))
	if len(stages) != 0 {
		t.Fatal("stale file stages", stages)
	}
}

func TestRootDeviceCannotInjectKernelArguments(t *testing.T) {
	d, spec := fixture(t)
	path := filepath.Join(d, "image.fyimg")
	if _, err := PackSpec(context.Background(), spec, path); err != nil {
		t.Fatal(err)
	}
	r, err := Inspect(context.Background(), path, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"/dev/vda root=/dev/other", "/dev/vda\ninit=/bin/sh", "/dev/vda\x00"} {
		m := r.Manifest
		m.Boot.RootDevice = value
		if m.Validate() == nil {
			t.Fatal("accepted root device argument injection")
		}
	}
}
