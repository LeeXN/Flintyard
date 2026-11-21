package guestinit

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LeeXN/Flintyard/internal/host"
)

func keyFixture(t *testing.T) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var wire []byte
	for _, f := range [][]byte{[]byte("ssh-ed25519"), pub} {
		wire = binary.BigEndian.AppendUint32(wire, uint32(len(f)))
		wire = append(wire, f...)
	}
	return "ssh-ed25519 " + base64.StdEncoding.EncodeToString(wire)
}
func seedFixture(t *testing.T) SeedRequest {
	return SeedRequest{
		EnvironmentID: "12345678-1234-4234-8234-123456789abc", NodeID: "87654321-4321-4321-8321-cba987654321",
		Hostname: "node-1", MAC: "02:00:00:12:34:56", Address: netip.MustParsePrefix("10.240.12.10/24"), Gateway: netip.MustParseAddr("10.240.12.1"),
		DNS: []netip.Addr{netip.MustParseAddr("10.0.0.53")}, Username: "devops", PublicKeys: []byte(keyFixture(t) + " user@ci\n"), Destination: filepath.Join(t.TempDir(), "seed.raw"),
	}
}

func TestPublicKeysRejectCommandsAndMalformedWire(t *testing.T) {
	key := keyFixture(t)
	got, err := PublicKeys([]byte("# comment\r\n" + key + " harmless comment\r\n"))
	if err != nil || got != key+"\n" {
		t.Fatal(got, err)
	}
	for _, data := range []string{"", "# no keys", "-----BEGIN OPENSSH PRIVATE KEY-----", "command=\"id\" " + key, "ssh-rsa " + strings.Fields(key)[1], "ssh-ed25519 YQ==", key + "\x00", strings.Repeat(key+"\n", 33)} {
		if _, err := PublicKeys([]byte(data)); err == nil {
			t.Errorf("accepted invalid public key: %.50s", data)
		}
	}
}

func TestPublicKeysRSAAndECDSAWireValidation(t *testing.T) {
	encode := func(kind string, fields ...[]byte) string {
		var wire []byte
		for _, field := range append([][]byte{[]byte(kind)}, fields...) {
			wire = binary.BigEndian.AppendUint32(wire, uint32(len(field)))
			wire = append(wire, field...)
		}
		return kind + " " + base64.StdEncoding.EncodeToString(wire)
	}
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	n := append([]byte{0}, rsaKey.N.Bytes()...)
	key := encode("ssh-rsa", []byte{1, 0, 1}, n)
	if got, err := PublicKeys([]byte(key)); err != nil || got != key+"\n" {
		t.Fatal(err)
	}
	for _, bad := range []string{
		encode("ssh-rsa", []byte{2}, n),                 // even exponent
		encode("ssh-rsa", []byte{0x81}, n),              // negative mpint
		encode("ssh-rsa", []byte{1, 0, 1}, []byte{127}), // weak modulus
		encode("ssh-rsa", []byte{1, 0, 1}, n, []byte("extra")),
	} {
		if _, err := PublicKeys([]byte(bad)); err == nil {
			t.Fatal("accepted malformed RSA key")
		}
	}
	for name, curve := range map[string]elliptic.Curve{"nistp256": elliptic.P256(), "nistp384": elliptic.P384(), "nistp521": elliptic.P521()} {
		private, err := ecdsa.GenerateKey(curve, rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		point := elliptic.Marshal(curve, private.X, private.Y)
		kind := "ecdsa-sha2-" + name
		key := encode(kind, []byte(name), point)
		if got, err := PublicKeys([]byte(key)); err != nil || got != key+"\n" {
			t.Fatal(name, err)
		}
		for _, bad := range []string{encode(kind, []byte("wrong-curve"), point), encode(kind, []byte(name), make([]byte, len(point)))} {
			if _, err := PublicKeys([]byte(bad)); err == nil {
				t.Fatal("accepted malformed ECDSA key")
			}
		}
	}
}

func TestSeedRejectsInvalidDataBeforeWriting(t *testing.T) {
	for _, field := range []string{"env", "node", "hostname", "user", "mac", "multicast", "public", "partial-private", "v6", "network", "broadcast", "gateway", "dns", "keys"} {
		t.Run(field, func(t *testing.T) {
			r := seedFixture(t)
			switch field {
			case "env":
				r.EnvironmentID = "../../escape"
			case "node":
				r.NodeID = "$(id)"
			case "hostname":
				r.Hostname = "host\ncommand"
			case "user":
				r.Username = "root:bad"
			case "mac":
				r.MAC = "00:00:00:12:34:56"
			case "multicast":
				r.MAC = "03:00:00:12:34:56"
			case "public":
				r.Address = netip.MustParsePrefix("8.8.8.8/24")
			case "partial-private":
				r.Address = netip.MustParsePrefix("172.16.12.10/8")
				r.Gateway = netip.MustParseAddr("172.0.0.1")
			case "v6":
				r.Address = netip.MustParsePrefix("fd00::10/64")
			case "network":
				r.Address = netip.MustParsePrefix("10.240.12.0/24")
			case "broadcast":
				r.Address = netip.MustParsePrefix("10.240.12.255/24")
			case "gateway":
				r.Gateway = netip.MustParseAddr("10.240.12.2")
			case "dns":
				r.DNS = []netip.Addr{netip.MustParseAddr("127.0.0.53")}
			case "keys":
				r.PublicKeys = []byte("not a key")
			}
			if _, err := CreateSeed(context.Background(), r); err == nil {
				t.Fatal("accepted malformed seed")
			}
			entries, err := os.ReadDir(filepath.Dir(r.Destination))
			if err != nil || len(entries) != 0 {
				t.Fatal("invalid input mutated disk", entries, err)
			}
		})
	}
}

func TestSeedExt4ContentsAndNoOverwrite(t *testing.T) {
	for _, name := range []string{"mkfs.ext4", "debugfs"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Skipf("ext4 integration requires %s: %v", name, err)
		}
	}
	r := seedFixture(t)
	t.Chdir(filepath.Dir(r.Destination))
	if err := os.Mkdir("-output", 0700); err != nil {
		t.Fatal(err)
	}
	r.Destination = filepath.Join("-output", "seed.raw")
	ctx := context.Background()
	got, err := CreateSeed(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	if got.Size != SeedBytes || got.Inode == 0 {
		t.Fatal(got)
	}
	if !filepath.IsAbs(got.Path) {
		t.Fatal("seed path was not resolved", got.Path)
	}
	r.Destination = got.Path
	st, err := os.Stat(r.Destination)
	if err != nil || st.Mode().Perm() != 0400 {
		t.Fatal(st, err)
	}
	// Inspect the actual filesystem independently of the producer. No loop
	// device, mount namespace or KVM is needed for this integration test.
	stats, err := host.Run(ctx, host.CommandRequest{Name: "debugfs", Args: []string{"-R", "stats", r.Destination}})
	if err != nil || !strings.Contains(stats.Output, "FYSEED") {
		t.Fatal(stats, err)
	}
	files, _ := seedFiles(r)
	for name, want := range files {
		out, err := host.Run(ctx, host.CommandRequest{Name: "debugfs", Args: []string{"-R", "cat /" + name, r.Destination}})
		if err != nil || !strings.HasSuffix(out.Output, want) {
			t.Errorf("seed file %s: %q, want suffix %q (%v)", name, out.Output, want, err)
		}
	}
	if _, err = CreateSeed(ctx, r); !errors.Is(err, os.ErrExist) {
		t.Fatal("overwrote seed", err)
	}
	stages, _ := filepath.Glob(filepath.Join(filepath.Dir(r.Destination), ".fy-*"))
	if len(stages) != 0 {
		t.Fatal("stale seed staging", stages)
	}
}

func TestSeedCancellationAndMkfsFailure(t *testing.T) {
	r := seedFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := CreateSeed(ctx, r); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	// An unavailable utility must fail with its cause and leave no disk behind.
	t.Setenv("PATH", t.TempDir())
	if _, err := CreateSeed(context.Background(), r); err == nil {
		t.Fatal("missing mkfs reported success")
	}
	entries, err := os.ReadDir(filepath.Dir(r.Destination))
	if err != nil || len(entries) != 0 {
		t.Fatal("failed mkfs left disk", entries, err)
	}
}

func TestSeedWithoutKeyOrDNS(t *testing.T) {
	r := seedFixture(t)
	r.PublicKeys, r.DNS = nil, nil
	files, err := seedFiles(r)
	if err != nil || files["authorized_keys"] != "" || files["dns"] != "" {
		t.Fatal(files, err)
	}
	r.Address = netip.MustParsePrefix("10.240.12.10/29")
	r.Gateway = netip.MustParseAddr("10.240.12.9")
	if files, err = seedFiles(r); err != nil || files["prefix"] != "29\n" || files["gateway"] != "10.240.12.9\n" {
		t.Fatal(files, err)
	}
}
