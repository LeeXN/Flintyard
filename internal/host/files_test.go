package host

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestPublishFileFailureAndCancellationLeaveNoPartialFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "disk")
	sentinel := errors.New("disk write failed")
	err := PublishFile(context.Background(), path, 0600, func(f *os.File) error { _, _ = f.Write([]byte("partial")); return sentinel })
	if !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	err = PublishFile(ctx, path, 0600, func(f *os.File) error { _, err := f.Write([]byte("complete")); cancel(); return err })
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatal("partial file published or stage leaked", entries)
	}
}
