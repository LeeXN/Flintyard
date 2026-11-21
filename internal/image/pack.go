package image

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"github.com/LeeXN/Flintyard/internal/contract"
	"github.com/LeeXN/Flintyard/internal/state"
	"github.com/LeeXN/Flintyard/internal/version"
	"io"
	"os"
	"path/filepath"
	"time"
)

// PackSpec snapshots checked ordinary inputs before atomically publishing a package.
// Relative input paths are resolved against the spec, never the caller's cwd.
func PackSpec(ctx context.Context, specPath, output string) (string, error) {
	f, e := regular(specPath)
	if e != nil {
		return "", e
	}
	defer f.Close()
	b, e := io.ReadAll(io.LimitReader(f, contract.MaxConfig+1))
	if e != nil {
		return "", e
	}
	if e = contract.Validate("import", b); e != nil {
		return "", e
	}
	var s Spec
	if e = contract.Decode(b, &s); e != nil {
		return "", e
	}
	m := Manifest{APIVersion: s.APIVersion, Kind: "Image", Name: s.Name, Version: s.Version, Architecture: s.Architecture, OS: s.OS, Boot: s.Boot, Kernel: s.Kernel, Initialization: s.Initialization, Provenance: Provenance{BuiltAt: time.Now().UTC().Format(time.RFC3339), Reproducibility: "inputs_locked", Builder: "flintyard/v" + version.Version}}
	paths := map[string]string{"kernel": m.Boot.Kernel, "rootfs": m.Boot.Rootfs, "initrd": m.Boot.Initrd}
	stage, e := os.MkdirTemp("", "fy-pack-")
	if e != nil {
		return "", e
	}
	defer os.RemoveAll(stage)
	seen := map[string]bool{}
	for _, in := range s.Inputs {
		p := paths[in.Role]
		if p == "" || seen[in.Role] {
			return "", fmt.Errorf("missing boot path or duplicate input role")
		}
		seen[in.Role] = true
		if e = contract.Path(p); e != nil {
			return "", e
		}
		if !DigestValid(in.SHA256) {
			return "", fmt.Errorf("placeholder or invalid input digest")
		}
		source := in.Path
		if !filepath.IsAbs(source) {
			source = filepath.Join(filepath.Dir(specPath), source)
		}
		rf, e := regular(source)
		if e != nil {
			return "", e
		}
		dst := filepath.Join(stage, p)
		if e = os.MkdirAll(filepath.Dir(dst), 0700); e != nil {
			rf.Close()
			return "", e
		}
		of, e := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			rf.Close()
			return "", e
		}
		_, e = io.Copy(of, contextReader{ctx, io.LimitReader(rf, 64<<30+1)})
		rf.Close()
		ce := of.Close()
		if e != nil {
			return "", e
		}
		if ce != nil {
			return "", ce
		}
		digest, n, e := Hash(ctx, dst)
		if e != nil {
			return "", e
		}
		if digest != in.SHA256 {
			return "", fmt.Errorf("input digest mismatch for %s", in.Role)
		}
		m.Payloads = append(m.Payloads, Payload{Path: p, Role: in.Role, Size: n, SHA256: digest})
		// Do not embed private host paths into distributable provenance.
		m.Provenance.Inputs = append(m.Provenance.Inputs, Source{Kind: in.Role, Source: "local:" + in.Role, SHA256: digest})
	}
	if e = m.Validate(); e != nil {
		return "", e
	}
	mb, e := json.MarshalIndent(m, "", "  ")
	if e != nil {
		return "", e
	}
	if e = contract.Validate("image", mb); e != nil {
		return "", e
	}
	if e = state.Atomic(filepath.Join(stage, "manifest.json"), mb, 0600); e != nil {
		return "", e
	}
	return WritePackage(ctx, stage, m, output)
}
func WritePackage(ctx context.Context, dir string, m Manifest, output string) (string, error) {
	out, e := os.CreateTemp(filepath.Dir(output), ".fyimg-")
	if e != nil {
		return "", e
	}
	defer os.Remove(out.Name())
	defer out.Close()
	gz := gzip.NewWriter(out)
	tw := tar.NewWriter(gz)
	names := []string{"manifest.json"}
	for _, p := range m.Payloads {
		names = append(names, p.Path)
	}
	for _, p := range names {
		f, e := regular(filepath.Join(dir, p))
		if e != nil {
			return "", e
		}
		st, e := f.Stat()
		if e != nil {
			f.Close()
			return "", e
		}
		if e = tw.WriteHeader(&tar.Header{Name: p, Mode: 0400, Size: st.Size(), Typeflag: tar.TypeReg}); e == nil {
			_, e = io.Copy(tw, contextReader{ctx, f})
		}
		f.Close()
		if e != nil {
			return "", e
		}
	}
	if e = tw.Close(); e != nil {
		return "", e
	}
	if e = gz.Close(); e != nil {
		return "", e
	}
	if e = out.Sync(); e != nil {
		return "", e
	}
	if e = out.Close(); e != nil {
		return "", e
	}
	// Link publishes without overwriting an existing user package.
	if e = os.Link(out.Name(), output); e != nil {
		return "", e
	}
	if e = state.SyncDir(filepath.Dir(output)); e != nil {
		return "", e
	}
	sum, _, e := Hash(ctx, output)
	return sum, e
}

func ExportCached(ctx context.Context, root, id, output string) (Result, error) {
	r, err := Cached(ctx, root, id)
	if err != nil {
		return r, err
	}
	dir, err := cacheDir(root, id)
	if err != nil {
		return r, err
	}
	if err = os.MkdirAll(filepath.Dir(output), 0700); err != nil {
		return r, err
	}
	r.PackageSHA256, err = WritePackage(ctx, dir, r.Manifest, output)
	return r, err
}
