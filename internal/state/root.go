package state

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"syscall"
	"time"

	"github.com/LeeXN/Flintyard/internal/contract"
)

var uuidRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

const MaxState = 16 << 20

func ValidUUID(s string) bool { return uuidRE.MatchString(s) }

type Root struct {
	APIVersion string    `json:"api_version"`
	Kind       string    `json:"kind"`
	ID         string    `json:"id"`
	CreatedAt  time.Time `json:"created_at"`
	Path       string    `json:"-"`
}

func OpenRoot(ctx context.Context, path string, create bool) (Root, error) {
	var r Root
	p, err := filepath.Abs(path)
	if err != nil {
		return r, err
	}
	if create {
		if err = PrivateDir(p); err != nil {
			return r, err
		}
		unlock, err := LockContext(ctx, filepath.Join(p, ".root.lock"))
		if err != nil {
			return r, err
		}
		defer unlock()
		if err = Read(filepath.Join(p, "host.json"), &r); os.IsNotExist(err) {
			// An existing environment tree without an owner must not be adopted.
			entries, e := os.ReadDir(filepath.Join(p, "envs"))
			if e != nil && !os.IsNotExist(e) {
				return r, e
			}
			if len(entries) > 0 {
				return r, fmt.Errorf("environment state exists without root ownership")
			}
			r = Root{APIVersion: "flintyard/v1alpha1", Kind: "StateRoot", ID: UUID(), CreatedAt: time.Now().UTC()}
			err = Write(filepath.Join(p, "host.json"), r)
		}
		if err != nil {
			return r, err
		}
		for _, dir := range []string{"envs", "locks"} {
			if err = PrivateDir(filepath.Join(p, dir)); err != nil {
				return r, err
			}
		}
	} else {
		if err = CheckPrivateDir(p); err != nil {
			return r, err
		}
		if err = Read(filepath.Join(p, "host.json"), &r); err != nil {
			return r, err
		}
	}
	if r.APIVersion != "flintyard/v1alpha1" || r.Kind != "StateRoot" || !ValidUUID(r.ID) || r.CreatedAt.IsZero() {
		return Root{}, fmt.Errorf("invalid state root ownership metadata")
	}
	r.Path = p
	return r, nil
}

// CheckPrivateDir validates an existing root without creating files or parents.
func CheckPrivateDir(path string) error {
	p, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	for q := p; ; q = filepath.Dir(q) {
		st, err := os.Lstat(q)
		if err != nil {
			return err
		}
		if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("unsafe private directory %s", q)
		}
		if os.Geteuid() == 0 {
			s := st.Sys().(*syscall.Stat_t)
			if s.Uid != 0 || st.Mode().Perm()&0022 != 0 {
				return fmt.Errorf("private ancestor controlled by another user: %s", q)
			}
		}
		if q == p && st.Mode().Perm()&0077 != 0 {
			return fmt.Errorf("directory is not private: %s", q)
		}
		if q == "/" {
			break
		}
	}
	return nil
}

// Read only accepts a bounded, ordinary state file. Configuration paths have
// separate handling; state files are never followed through a final symlink.
func Read(path string, result any) error {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() || st.Size() > MaxState {
		return fmt.Errorf("invalid state file type or size")
	}
	b, err := io.ReadAll(io.LimitReader(f, MaxState+1))
	if err != nil {
		return err
	}
	return contract.DecodeLimit(b, result, MaxState)
}

type Owner struct {
	RootID        string `json:"root_id"`
	EnvironmentID string `json:"environment_id"`
	NodeID        string `json:"node_id,omitempty"`
}

func (o Owner) Validate() error {
	if !ValidUUID(o.RootID) || !ValidUUID(o.EnvironmentID) || o.NodeID != "" && !ValidUUID(o.NodeID) {
		return fmt.Errorf("invalid resource owner")
	}
	return nil
}
func (o Owner) Tag() string { return "flintyard:" + o.RootID + ":" + o.EnvironmentID + ":" + o.NodeID }

// OwnedDir publishes a fully marked directory atomically. The caller records
// intent first and holds its operation lock. Recovery checks owner.json, never
// a name prefix. Existing directories are not silently adopted or overwritten.
func OwnedDir(path string, owner Owner) error {
	return OwnedDirContents(path, owner, nil)
}
func OwnedDirContents(path string, owner Owner, files map[string]any) error {
	return ownedDirContents(path, owner, files, nil)
}
func ownedDirContents(path string, owner Owner, files map[string]any, onMarked func(string) error) error {
	if err := owner.Validate(); err != nil {
		return err
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	encoded, err := encodeBootstrapFiles(files)
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp(filepath.Dir(path), ".owned-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	if err = Write(filepath.Join(dir, "owner.json"), owner); err != nil {
		return err
	}
	marker, err := newBootstrap(dir, path, owner, encoded)
	if err != nil {
		return err
	}
	if err = Write(filepath.Join(dir, "bootstrap.json"), marker); err != nil {
		return err
	}
	if onMarked != nil {
		if err = onMarked(dir); err != nil {
			return err
		}
	}
	for name, data := range encoded {
		if err = Atomic(filepath.Join(dir, name), data, 0600); err != nil {
			return err
		}
	}
	if err = SyncDir(dir); err != nil {
		return err
	}
	// Linux renameat2 is used by the platform-specific helper to avoid replacing
	// even an empty foreign directory at the destination.
	if err = renameExclusive(dir, path); err != nil {
		return err
	}
	return SyncDir(filepath.Dir(path))
}
func VerifyOwner(path string, want Owner) error {
	st, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("invalid owned directory")
	}
	var got Owner
	if err = Read(filepath.Join(path, "owner.json"), &got); err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("directory ownership mismatch")
	}
	return nil
}
