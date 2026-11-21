// Package image implements portable, content-addressed image packages.
package image

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"github.com/LeeXN/Flintyard/internal/contract"
	"github.com/LeeXN/Flintyard/internal/state"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

type Result struct {
	ID            string   `json:"image_id"`
	Manifest      Manifest `json:"manifest"`
	PackageSHA256 string   `json:"package_sha256,omitempty"`
	SourceTrust   string   `json:"source_trust"`
	Validation    string   `json:"validation"`
	KernelIssue   string   `json:"kernel_issue,omitempty"`
}
type Limits struct {
	MaxBytes int64
	MaxFiles int
}

func (l Limits) defaults() Limits {
	if l.MaxBytes == 0 {
		l.MaxBytes = 64 << 30
	}
	if l.MaxFiles == 0 {
		l.MaxFiles = 32
	}
	return l
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(b []byte) (int, error) {
	if e := r.ctx.Err(); e != nil {
		return 0, e
	}
	return r.r.Read(b)
}
func Hash(ctx context.Context, p string) (string, int64, error) {
	f, e := regular(p)
	if e != nil {
		return "", 0, e
	}
	defer f.Close()
	h := sha256.New()
	n, e := io.Copy(h, contextReader{ctx, f})
	return hex.EncodeToString(h.Sum(nil)), n, e
}
func regular(p string) (*os.File, error) {
	f, e := os.OpenFile(p, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if e != nil {
		return nil, e
	}
	st, e := f.Stat()
	if e != nil || !st.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("input is not an ordinary file: %s", p)
	}
	return f, nil
}
func Sum(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func extract(ctx context.Context, p, dir string, l Limits) (Result, error) {
	var out Result
	l = l.defaults()
	if l.MaxBytes < 1 || l.MaxFiles < 1 {
		return out, fmt.Errorf("invalid archive limits")
	}
	f, e := regular(p)
	if e != nil {
		return out, e
	}
	defer f.Close()
	h := sha256.New()
	gz, e := gzip.NewReader(io.TeeReader(contextReader{ctx, f}, h))
	if e != nil {
		return out, e
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	seen := map[string]bool{}
	var total int64
	var mb []byte
	for {
		hd, e := tr.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			return out, e
		}
		if e = contract.Path(hd.Name); e != nil {
			return out, e
		}
		if hd.Typeflag != tar.TypeReg || seen[hd.Name] || len(seen) >= l.MaxFiles || hd.Size < 0 || hd.Size > l.MaxBytes-total {
			return out, fmt.Errorf("forbidden entry, duplicate, or archive limit exceeded")
		}
		if hd.Name != "manifest.json" && !strings.HasPrefix(hd.Name, "payload/") {
			return out, fmt.Errorf("undeclared archive location")
		}
		seen[hd.Name] = true
		total += hd.Size
		if hd.Name == "manifest.json" {
			if hd.Size > contract.MaxConfig {
				return out, fmt.Errorf("manifest too large")
			}
			mb, e = io.ReadAll(tr)
			if e != nil {
				return out, e
			}
			continue
		}
		target := filepath.Join(dir, filepath.FromSlash(hd.Name))
		if e = os.MkdirAll(filepath.Dir(target), 0700); e != nil {
			return out, e
		}
		of, e := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if e != nil {
			return out, e
		}
		_, e = io.CopyN(of, contextReader{ctx, tr}, hd.Size)
		if e == nil {
			e = of.Sync()
		}
		ce := of.Close()
		if e != nil {
			return out, e
		}
		if ce != nil {
			return out, ce
		}
	}
	// Read the gzip trailer, including CRC, and reject hidden trailing data.
	if n, e := io.Copy(io.Discard, gz); e != nil || n != 0 {
		return out, fmt.Errorf("invalid gzip trailer or trailing archive data: %v", e)
	}
	if _, e = io.Copy(io.Discard, io.TeeReader(f, h)); e != nil {
		return out, e
	}
	if e = contract.Validate("image", mb); e != nil {
		return out, e
	}
	if e = contract.Decode(mb, &out.Manifest); e != nil {
		return out, e
	}
	if e = out.Manifest.Validate(); e != nil {
		return out, e
	}
	if len(seen) != len(out.Manifest.Payloads)+1 {
		return out, fmt.Errorf("unlisted payload")
	}
	for _, p := range out.Manifest.Payloads {
		if !seen[p.Path] {
			return out, fmt.Errorf("missing %s", p.Path)
		}
		s, n, e := Hash(ctx, filepath.Join(dir, p.Path))
		if e != nil {
			return out, e
		}
		if n != p.Size || s != p.SHA256 {
			return out, fmt.Errorf("digest mismatch: %s", p.Path)
		}
		if p.Role == "kernel" {
			kernel, err := regular(filepath.Join(dir, p.Path))
			if err != nil {
				return out, err
			}
			err = ValidateKernel(kernel, out.Manifest.Architecture)
			kernel.Close()
			if err != nil {
				out.KernelIssue = err.Error()
			}
		}
	}
	if e = state.Atomic(filepath.Join(dir, "manifest.json"), mb, 0600); e != nil {
		return out, e
	}
	out.ID = "sha256:" + Sum(mb)
	out.PackageSHA256 = hex.EncodeToString(h.Sum(nil))
	out.SourceTrust = "unverified"
	out.Validation = "not_run"
	return out, nil
}
func Inspect(ctx context.Context, p string, l Limits) (Result, error) {
	d, e := os.MkdirTemp("", "fy-inspect-")
	if e != nil {
		return Result{}, e
	}
	defer os.RemoveAll(d)
	return extract(ctx, p, d, l)
}
func cacheDir(root, id string) (string, error) {
	if !strings.HasPrefix(id, "sha256:") || !DigestValid(strings.TrimPrefix(id, "sha256:")) {
		return "", fmt.Errorf("invalid image ID")
	}
	return filepath.Join(root, "images", "sha256-"+strings.TrimPrefix(id, "sha256:")), nil
}
func Cached(ctx context.Context, root, id string) (Result, error) {
	var r Result
	d, e := cacheDir(root, id)
	if e != nil {
		return r, e
	}
	if e := filepath.WalkDir(d, func(p string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink in cached image")
		}
		if !entry.IsDir() && !entry.Type().IsRegular() {
			return fmt.Errorf("nonregular cached file")
		}
		return nil
	}); e != nil {
		return r, e
	}
	// os.Root prevents symlink ancestors from escaping a tampered cache tree.
	rd, e := os.OpenRoot(d)
	if e != nil {
		return r, e
	}
	defer rd.Close()
	r.Manifest, e = readCachedManifest(rd, id)
	if e != nil {
		return r, e
	}
	for _, p := range r.Manifest.Payloads {
		f, e := rd.Open(p.Path)
		if e != nil {
			return r, e
		}
		st, e := f.Stat()
		if e != nil || !st.Mode().IsRegular() {
			f.Close()
			return r, fmt.Errorf("invalid cached payload")
		}
		h := sha256.New()
		n, e := io.Copy(h, contextReader{ctx, f})
		if e == nil && p.Role == "kernel" {
			if err := ValidateKernel(f, r.Manifest.Architecture); err != nil {
				r.KernelIssue = err.Error()
			}
		}
		f.Close()
		if e != nil {
			return r, e
		}
		if n != p.Size || hex.EncodeToString(h.Sum(nil)) != p.SHA256 {
			return r, fmt.Errorf("cached payload digest mismatch")
		}
	}
	r.ID = id
	r.SourceTrust = "unverified"
	r.Validation = "not_run"
	return r, nil
}

func readCachedManifest(root *os.Root, id string) (Manifest, error) {
	var manifest Manifest
	f, err := root.OpenFile("manifest.json", os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return manifest, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return manifest, fmt.Errorf("cached manifest must be an ordinary file")
	}
	b, err := io.ReadAll(io.LimitReader(f, contract.MaxConfig+1))
	if err != nil {
		return manifest, err
	}
	if "sha256:"+Sum(b) != id {
		return manifest, fmt.Errorf("cached manifest digest mismatch")
	}
	if err = contract.Validate("image", b); err != nil {
		return manifest, err
	}
	if err = contract.Decode(b, &manifest); err != nil {
		return manifest, err
	}
	return manifest, manifest.Validate()
}
func Import(ctx context.Context, root, p, expected string, l Limits) (Result, error) {
	var out Result
	if expected != "" && !DigestValid(expected) {
		return out, fmt.Errorf("invalid external digest")
	}
	if e := state.PrivateDir(root); e != nil {
		return out, e
	}
	images := filepath.Join(root, "images")
	if e := state.PrivateDir(images); e != nil {
		return out, e
	}
	d, e := os.MkdirTemp(images, ".staging-")
	if e != nil {
		return out, e
	}
	defer os.RemoveAll(d)
	out, e = extract(ctx, p, d, l)
	if e != nil {
		return out, e
	}
	if expected != "" && out.PackageSHA256 != expected {
		return out, fmt.Errorf("package digest mismatch")
	}
	dst, _ := cacheDir(root, out.ID)
	unlock, e := state.Lock(dst + ".lock")
	if e != nil {
		return out, e
	}
	defer unlock()
	if _, e = os.Lstat(dst); e == nil {
		if _, e = Cached(ctx, root, out.ID); e != nil {
			return out, e
		}
	} else if !os.IsNotExist(e) {
		return out, e
	} else {
		e = filepath.WalkDir(d, func(p string, de os.DirEntry, e error) error {
			if e != nil {
				return e
			}
			if !de.IsDir() {
				return os.Chmod(p, 0400)
			}
			return nil
		})
		if e != nil {
			return out, e
		}
		if e = state.SyncDir(d); e != nil {
			return out, e
		}
		if e = os.Rename(d, dst); e != nil {
			return out, e
		}
		if e = state.SyncDir(images); e != nil {
			return out, e
		}
	}
	if expected != "" {
		out.SourceTrust = "external_digest_verified"
	}
	return out, nil
}
