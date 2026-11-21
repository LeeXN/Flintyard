package image

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

type ExtractRequest struct {
	Archive         string
	Destination     string
	StripComponents int
	MaxBytes        int64
	MaxFiles        int
}
type ExtractResult struct {
	Files  int   `json:"files"`
	Bytes  int64 `json:"bytes"`
	Xattrs int   `json:"xattrs"`
}

// ExtractRootfs handles Linux root archives, not the stricter .fyimg transport.
// Its destination must be a new private directory beneath an owned build scope.
// No path is ever written through an archive-provided symlink, even if that
// symlink would resolve inside the tree. Absolute guest symlinks are preserved.
func ExtractRootfs(ctx context.Context, r ExtractRequest) (out ExtractResult, err error) {
	if r.MaxBytes == 0 {
		r.MaxBytes = 64 << 30
	}
	if r.MaxFiles == 0 {
		r.MaxFiles = 1000000
	}
	if r.MaxBytes < 1 || r.MaxFiles < 1 || r.StripComponents < 0 || r.StripComponents > 8 {
		return out, fmt.Errorf("invalid root archive limits")
	}
	if err = ctx.Err(); err != nil {
		return out, err
	}
	if err = os.Mkdir(r.Destination, 0700); err != nil {
		return out, err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, os.RemoveAll(r.Destination))
		}
	}()
	root, err := os.OpenRoot(r.Destination)
	if err != nil {
		return out, err
	}
	defer root.Close()
	reader, closeReader, err := rootArchiveReader(ctx, r.Archive)
	if err != nil {
		return out, err
	}
	defer closeReader()
	limited := &io.LimitedReader{R: reader, N: r.MaxBytes + 1}
	tr := tar.NewReader(limited)
	seen, regulars := map[string]byte{}, map[string]bool{}
	type directory struct {
		name   string
		header tar.Header
	}
	var directories []directory
	var rootHeader *tar.Header
	entries := 0
	for {
		if err = ctx.Err(); err != nil {
			return out, err
		}
		h, e := tr.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			return out, e
		}
		entries++
		if entries > r.MaxFiles {
			return out, fmt.Errorf("root archive entry count exceeds limit")
		}
		if h.Typeflag == tar.TypeXGlobalHeader {
			// kernel.org/git archives carry a global commit comment. Go's tar
			// reader returns global headers separately and does not apply them to
			// following entries. Accept only this descriptive metadata, never
			// silently drop global paths, owners, links, ACLs or xattrs.
			for key := range h.PAXRecords {
				if key != "comment" {
					return out, fmt.Errorf("unsupported global PAX attribute %s", key)
				}
			}
			continue
		}
		name, e := rootArchivePath(h.Name, r.StripComponents)
		if e != nil {
			return out, e
		}
		if name == "" {
			if h.Typeflag != tar.TypeDir {
				return out, fmt.Errorf("archive entry strips to extraction root")
			}
			if r.StripComponents == 0 {
				if rootHeader != nil {
					return out, fmt.Errorf("duplicate root directory metadata")
				}
				copy := *h
				rootHeader = &copy
			}
			continue
		}
		if out.Files >= r.MaxFiles || h.Size < 0 || h.Size > r.MaxBytes-out.Bytes {
			return out, fmt.Errorf("root archive expansion exceeds limits")
		}
		if _, exists := seen[name]; exists {
			return out, fmt.Errorf("duplicate root archive entry %s", name)
		}
		if e = directoryAncestors(root, name); e != nil {
			return out, e
		}
		seen[name] = h.Typeflag
		out.Files++
		out.Bytes += h.Size
		xattrs, e := archiveXattrs(h)
		if e != nil {
			return out, e
		}
		out.Xattrs += len(xattrs)
		switch h.Typeflag {
		case tar.TypeDir:
			if e = root.Mkdir(name, 0700); e != nil && !os.IsExist(e) {
				return out, e
			}
			st, e := root.Lstat(name)
			if e != nil || !st.IsDir() {
				return out, fmt.Errorf("directory archive entry collides with a non-directory")
			}
			directories = append(directories, directory{name, *h})
		case tar.TypeReg, tar.TypeRegA:
			f, e := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY|unix.O_NOFOLLOW, 0600)
			if e != nil {
				return out, e
			}
			_, e = io.CopyN(f, contextReader{ctx, tr}, h.Size)
			if e == nil {
				e = fileMetadata(f, h, xattrs)
			}
			if e == nil {
				e = f.Sync()
			}
			ce := f.Close()
			if e != nil {
				return out, e
			}
			if ce != nil {
				return out, ce
			}
			regulars[name] = true
		case tar.TypeSymlink:
			if h.Linkname == "" || len(h.Linkname) > 4096 || strings.ContainsRune(h.Linkname, 0) {
				return out, fmt.Errorf("invalid guest symlink")
			}
			if e = root.Symlink(h.Linkname, name); e != nil {
				return out, e
			}
			if e = root.Lchown(name, h.Uid, h.Gid); e != nil {
				return out, e
			}
			// Linux symlink permissions are fixed. Unsupported link metadata must
			// be explicit rather than silently discarded.
			if len(xattrs) > 0 {
				return out, fmt.Errorf("symlink xattrs require a normalized archive")
			}
			dir, e := root.Open(".")
			if e != nil {
				return out, e
			}
			atime := h.AccessTime
			if atime.IsZero() {
				atime = h.ModTime
			}
			e = unix.UtimesNanoAt(int(dir.Fd()), name, []unix.Timespec{unix.NsecToTimespec(atime.UnixNano()), unix.NsecToTimespec(h.ModTime.UnixNano())}, unix.AT_SYMLINK_NOFOLLOW)
			dir.Close()
			if e != nil {
				return out, e
			}
		case tar.TypeLink:
			target, e := rootArchivePath(h.Linkname, r.StripComponents)
			if e != nil || target == "" || !regulars[target] {
				return out, fmt.Errorf("hard link target must be an already extracted ordinary file")
			}
			if e = checkAncestors(root, target); e != nil {
				return out, e
			}
			st, e := root.Lstat(target)
			if e != nil || !st.Mode().IsRegular() {
				return out, fmt.Errorf("hard link target changed")
			}
			if e = root.Link(target, name); e != nil {
				return out, e
			}
			regulars[name] = true
			// A hard-link record is not allowed to change the shared inode's
			// metadata after the canonical ordinary entry was checked.
			if len(xattrs) > 0 {
				return out, fmt.Errorf("hard link xattrs must be on the canonical file entry")
			}
		default:
			return out, fmt.Errorf("forbidden root archive type %d", h.Typeflag)
		}
	}
	// Read compressed trailers too: CRC/decompressor errors are not success.
	if _, err = io.Copy(io.Discard, contextReader{ctx, limited}); err != nil {
		return out, err
	}
	if limited.N == 0 {
		return out, fmt.Errorf("root archive decompressed bytes exceed limit")
	}
	if err = closeReader(); err != nil {
		return out, err
	}
	closeReader = func() error { return nil }
	// Implicit parents have ordinary guest directory permissions. Their private
	// build-scope ancestor, not the guest root's mode, protects the host staging.
	if err = filepath.WalkDir(r.Destination, func(p string, entry os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if !entry.IsDir() {
			return nil
		}
		relative, e := filepath.Rel(r.Destination, p)
		if e != nil {
			return e
		}
		if relative != "." {
			if _, explicit := seen[filepath.ToSlash(relative)]; !explicit {
				return os.Chmod(p, 0755)
			}
		}
		return nil
	}); err != nil {
		return out, err
	}
	sort.SliceStable(directories, func(i, j int) bool {
		return strings.Count(directories[i].name, "/") > strings.Count(directories[j].name, "/")
	})
	for _, d := range directories {
		if err = checkAncestors(root, d.name); err != nil {
			return out, err
		}
		f, e := root.OpenFile(d.name, os.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
		if e != nil {
			return out, e
		}
		x, e := archiveXattrs(&d.header)
		if e == nil {
			e = fileMetadata(f, &d.header, x)
		}
		f.Close()
		if e != nil {
			return out, e
		}
	}

	f, e := root.Open(".")
	if e != nil {
		return out, e
	}
	if rootHeader == nil {
		err = f.Chmod(0755)
	} else {
		attrs, e := archiveXattrs(rootHeader)
		err = e
		if err == nil {
			err = fileMetadata(f, rootHeader, attrs)
		}
	}
	f.Close()
	if err != nil {
		return out, err
	}
	return out, nil
}

func rootArchivePath(name string, strip int) (string, error) {
	// Root archives describe Linux filesystems. A backslash is a literal Linux
	// filename byte, notably used by systemd unit escaping, not a separator.
	// The outer .fyimg transport retains its stricter portable path rules.
	if name == "" || len(name) > 4096 || strings.HasPrefix(name, "/") || strings.ContainsRune(name, '\x00') {
		return "", fmt.Errorf("unsafe root archive path")
	}
	for strings.HasPrefix(name, "./") {
		name = strings.TrimPrefix(name, "./")
	}
	name = strings.TrimSuffix(name, "/")
	if name == "." || name == "" {
		return "", nil
	}
	parts := strings.Split(name, "/")
	for _, p := range parts {
		if p == "" || p == "." || p == ".." {
			return "", fmt.Errorf("unsafe root archive path component")
		}
	}
	if len(parts) <= strip {
		return "", nil
	}
	return strings.Join(parts[strip:], "/"), nil
}
func checkAncestors(root *os.Root, name string) error {
	parts := strings.Split(path.Dir(name), "/")
	prefix := ""
	for _, p := range parts {
		if p == "." {
			continue
		}
		prefix = path.Join(prefix, p)
		st, err := root.Lstat(prefix)
		if err != nil {
			return err
		}
		if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("archive traversal through a link/non-directory")
		}
	}
	return nil
}
func directoryAncestors(root *os.Root, name string) error {
	parts := strings.Split(path.Dir(name), "/")
	prefix := ""
	for _, p := range parts {
		if p == "." {
			continue
		}
		prefix = path.Join(prefix, p)
		st, err := root.Lstat(prefix)
		if os.IsNotExist(err) {
			if err = root.Mkdir(prefix, 0700); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("archive traversal through a link/non-directory")
		}
	}
	return nil
}
func archiveXattrs(h *tar.Header) (map[string][]byte, error) {
	x := map[string][]byte{}
	for name, value := range h.Xattrs {
		x[name] = []byte(value)
	}
	for key, value := range h.PAXRecords {
		if strings.HasPrefix(key, "SCHILY.acl.") {
			return nil, fmt.Errorf("text ACL metadata requires a normalized numeric xattr archive")
		}
		if strings.HasPrefix(key, "LIBARCHIVE.xattr.") {
			name := strings.TrimPrefix(key, "LIBARCHIVE.xattr.")
			decoded, err := base64.StdEncoding.DecodeString(value)
			if err != nil {
				return nil, err
			}
			if old, exists := x[name]; exists && string(old) != string(decoded) {
				return nil, fmt.Errorf("conflicting archive xattrs")
			}
			x[name] = decoded
		}
	}
	for name, value := range x {
		if name == "" || len(name) > 255 || strings.ContainsRune(name, 0) || len(value) > 64<<10 {
			return nil, fmt.Errorf("invalid archive xattr")
		}
	}
	return x, nil
}
func fileMetadata(f *os.File, h *tar.Header, xattrs map[string][]byte) error {
	if h.Uid < 0 || h.Gid < 0 || uint64(h.Uid) >= 1<<32-1 || uint64(h.Gid) >= 1<<32-1 {
		return fmt.Errorf("invalid numeric archive ownership")
	}
	if err := f.Chown(h.Uid, h.Gid); err != nil {
		return err
	}
	mode := os.FileMode(h.Mode & 0777)
	if h.Mode&04000 != 0 {
		mode |= os.ModeSetuid
	}
	if h.Mode&02000 != 0 {
		mode |= os.ModeSetgid
	}
	if h.Mode&01000 != 0 {
		mode |= os.ModeSticky
	}
	if err := f.Chmod(mode); err != nil {
		return err
	}
	for name, value := range xattrs {
		if err := unix.Fsetxattr(int(f.Fd()), name, value, 0); err != nil {
			return fmt.Errorf("cannot preserve archive xattr %s: %w", name, err)
		}
	}
	atime := h.AccessTime
	if atime.IsZero() {
		atime = h.ModTime
	}
	if atime.IsZero() {
		atime = time.Unix(0, 0)
	}
	mtime := h.ModTime
	if mtime.IsZero() {
		mtime = time.Unix(0, 0)
	}
	return unix.Futimes(int(f.Fd()), []unix.Timeval{unix.NsecToTimeval(atime.UnixNano()), unix.NsecToTimeval(mtime.UnixNano())})
}

func rootArchiveReader(ctx context.Context, file string) (io.Reader, func() error, error) {
	f, err := regular(file)
	if err != nil {
		return nil, nil, err
	}
	header := make([]byte, 6)
	n, err := f.ReadAt(header, 0)
	if err != nil && err != io.EOF {
		f.Close()
		return nil, nil, err
	}
	if n >= 2 && header[0] == 0x1f && header[1] == 0x8b {
		gz, err := gzip.NewReader(contextReader{ctx, f})
		if err != nil {
			f.Close()
			return nil, nil, err
		}
		closed := false
		return gz, func() error {
			if closed {
				return nil
			}
			closed = true
			return errors.Join(gz.Close(), f.Close())
		}, nil
	}
	if n == 6 && string(header) == "\xfd7zXZ\x00" {
		cmd := exec.CommandContext(ctx, "xz", "--decompress", "--stdout", "--single-stream")
		cmd.Stdin = f
		var stderr limitedArchiveError
		cmd.Stderr = &stderr
		pipe, err := cmd.StdoutPipe()
		if err != nil {
			f.Close()
			return nil, nil, err
		}
		if err = cmd.Start(); err != nil {
			f.Close()
			return nil, nil, err
		}
		closed := false
		return pipe, func() error {
			if closed {
				return nil
			}
			closed = true
			pipe.Close()
			err := cmd.Wait()
			f.Close()
			if err != nil {
				return fmt.Errorf("xz archive decode: %w: %s", err, stderr.data)
			}
			return nil
		}, nil
	}
	closed := false
	return contextReader{ctx, f}, func() error {
		if closed {
			return nil
		}
		closed = true
		return f.Close()
	}, nil
}

type limitedArchiveError struct{ data []byte }

func (b *limitedArchiveError) Write(p []byte) (int, error) {
	n := len(p)
	remaining := (32 << 10) - len(b.data)
	if remaining > 0 {
		b.data = append(b.data, p[:min(remaining, n)]...)
	}
	return n, nil
}
