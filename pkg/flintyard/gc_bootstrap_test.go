package flintyard

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/LeeXN/Flintyard/internal/state"
)

func stagingFixture(t *testing.T, root state.Root, scope string) (string, state.Bootstrap) {
	t.Helper()
	base, err := bootstrapBase(root, scope)
	if err != nil {
		t.Fatal(err)
	}
	if err = state.PrivateDir(base); err != nil {
		t.Fatal(err)
	}
	name := "arbitrary-staging-" + state.UUID()
	path := filepath.Join(base, name)
	if err = os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	owner := state.Owner{RootID: root.ID, EnvironmentID: state.UUID()}
	if err = state.Write(filepath.Join(path, "owner.json"), owner); err != nil {
		t.Fatal(err)
	}
	creator, err := state.CurrentPublisher()
	if err != nil {
		t.Fatal(err)
	}
	creator.BootID = state.UUID() // simulated previous boot, not a reboot acceptance.
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	s := st.Sys().(*syscall.Stat_t)
	b := state.Bootstrap{APIVersion: APIVersion, Kind: "OwnedDirectoryBootstrap", Protocol: state.BootstrapProtocol, Token: state.UUID(), Owner: owner, Destination: filepath.Join(base, owner.EnvironmentID), StagingName: name, Device: uint64(s.Dev), Inode: s.Ino, Creator: creator, CreatedAt: time.Now().UTC(), Files: map[string]string{}}
	data := []byte("{\"partial\":true}\n")
	sum := sha256.Sum256(data)
	b.Files["control.json"] = hex.EncodeToString(sum[:])
	if err = os.WriteFile(filepath.Join(path, "control.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if err = state.Write(filepath.Join(path, "bootstrap.json"), b); err != nil {
		t.Fatal(err)
	}
	return name, b
}
func TestBootstrapPlanArchivesOnlyProvenUnpublishedControlScope(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("complete scope absence proof and GC apply require root")
	}
	for _, scope := range []string{"environment", "build"} {
		t.Run(scope, func(t *testing.T) {
			c, root, _ := environmentFixture(t)
			name, marker := stagingFixture(t, root, scope)
			r, err := c.GC(context.Background(), GCRequest{})
			if err != nil || len(r.Plan.Bootstraps) != 1 {
				t.Fatal(r, err)
			}
			applied, err := c.GC(context.Background(), GCRequest{Apply: true, PlanFile: writePlan(t, *r.Plan)})
			if err != nil || !applied.Applied || len(applied.BootstrapsCleaned) != 1 {
				t.Fatal(applied, err)
			}
			base, _ := bootstrapBase(root, scope)
			if _, err = os.Stat(filepath.Join(base, name)); !os.IsNotExist(err) {
				t.Fatal("staging entry remains", err)
			}
			archive := filepath.Join(root.Path, "diagnostics", "bootstrap", marker.Token)
			archived, err := state.ReadBootstrap(archive)
			if err != nil || archived.Token != marker.Token {
				t.Fatal(archived, err)
			}
			if _, err = os.Stat(filepath.Join(archive, "control.json")); err != nil {
				t.Fatal("diagnostics lost", err)
			}
		})
	}
}
func TestBootstrapPublisherOwnerAndSnapshotChangesAreRejected(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("complete scope absence proof and GC apply require root")
	}
	for _, mode := range []string{"publisher", "owner", "changed", "directory"} {
		t.Run(mode, func(t *testing.T) {
			c, root, _ := environmentFixture(t)
			name, marker := stagingFixture(t, root, "environment")
			base, _ := bootstrapBase(root, "environment")
			path := filepath.Join(base, name)
			if mode == "publisher" {
				creator, err := state.CurrentPublisher()
				if err != nil {
					t.Fatal(err)
				}
				marker.Creator = creator
				if err = state.Write(filepath.Join(path, "bootstrap.json"), marker); err != nil {
					t.Fatal(err)
				}
				r, err := c.GC(context.Background(), GCRequest{})
				if err != nil || len(r.Plan.Bootstraps) != 0 || len(r.BootstrapsExcluded) != 1 || !strings.Contains(r.BootstrapsExcluded[0].Reason, "alive") {
					t.Fatal(r, err)
				}
				return
			}
			r, err := c.GC(context.Background(), GCRequest{})
			if err != nil || len(r.Plan.Bootstraps) != 1 {
				t.Fatal(r, err)
			}
			switch mode {
			case "owner":
				wrong := marker.Owner
				wrong.RootID = state.UUID()
				err = state.Write(filepath.Join(path, "owner.json"), wrong)
			case "changed":
				err = os.WriteFile(filepath.Join(path, "control.json"), []byte("changed"), 0600)
			case "directory":
				err = os.Mkdir(filepath.Join(path, "foreign-directory"), 0700)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err = c.GC(context.Background(), GCRequest{Apply: true, PlanFile: writePlan(t, *r.Plan)}); err == nil {
				t.Fatal("changed bootstrap accepted")
			}
			if _, err = os.Stat(path); err != nil {
				t.Fatal("rejected scope moved", err)
			}
		})
	}
}
