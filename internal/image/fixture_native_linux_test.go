package image

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// This exercises extraction of real locked distribution root filesystems. It
// does not execute the guest or establish that the image can boot.
func TestNativeFixtureRootfsArchives(t *testing.T) {
	dir := os.Getenv("FLINTYARD_FIXTURE_ROOTFS_DIR")
	if dir == "" {
		t.Skip("set FLINTYARD_FIXTURE_ROOTFS_DIR for the native fixture archive check")
	}
	if os.Geteuid() != 0 {
		t.Fatal("fixture rootfs extraction requires root to preserve numeric owners")
	}
	data, err := os.ReadFile("../../fixtures/lock.json")
	if err != nil {
		t.Fatal(err)
	}
	var lock struct {
		Families map[string]struct {
			Arches map[string]struct {
				RootSHA    string `json:"rootfs_sha256"`
				Normalized *struct {
					SHA string `json:"sha256"`
				} `json:"normalization"`
			} `json:"arches"`
		} `json:"families"`
	}
	if err := json.Unmarshal(data, &lock); err != nil {
		t.Fatal(err)
	}
	for _, family := range []string{"ubuntu", "debian", "rocky", "alpine"} {
		for _, arch := range []string{"x86_64", "aarch64"} {
			t.Run(family+"-"+arch, func(t *testing.T) {
				entry, ok := lock.Families[family].Arches[arch]
				if !ok {
					t.Fatal("fixture input missing from lock")
				}
				want := entry.RootSHA
				if entry.Normalized != nil {
					want = entry.Normalized.SHA
				}
				archive := filepath.Join(dir, family+"-"+arch+".tar.gz")
				f, err := os.Open(archive)
				if err != nil {
					t.Fatal(err)
				}
				h := sha256.New()
				_, readErr := io.Copy(h, f)
				closeErr := f.Close()
				if readErr != nil || closeErr != nil || hex.EncodeToString(h.Sum(nil)) != want {
					t.Fatalf("fixture archive checksum failed: read=%v close=%v", readErr, closeErr)
				}
				result, err := ExtractRootfs(context.Background(), ExtractRequest{
					Archive: archive, Destination: filepath.Join(t.TempDir(), "root"),
				})
				if err != nil {
					t.Fatal(err)
				}
				t.Logf("sha256=%s files=%d bytes=%d xattrs=%d; guest execution not run", want, result.Files, result.Bytes, result.Xattrs)
			})
		}
	}
}
