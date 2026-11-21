package state

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestRootInitializationConcurrentAndOwnerPreserved(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state")
	var wg sync.WaitGroup
	results := make(chan Root, 8)
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := OpenRoot(context.Background(), path, true)
			results <- r
			errs <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	id := ""
	for r := range results {
		if id == "" {
			id = r.ID
		}
		if r.ID != id || !ValidUUID(r.ID) {
			t.Fatal("root identity changed", r)
		}
	}
	r, err := OpenRoot(context.Background(), path, false)
	if err != nil || r.ID != id {
		t.Fatal(r, err)
	}
	for _, p := range []string{path, filepath.Join(path, "locks"), filepath.Join(path, "envs")} {
		st, err := os.Stat(p)
		if err != nil || st.Mode().Perm() != 0700 {
			t.Fatal(p, st, err)
		}
	}
	if err := os.WriteFile(filepath.Join(path, "host.json"), []byte(`{"api_version":"other"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenRoot(context.Background(), path, true); err == nil {
		t.Fatal("silently replaced invalid root identity")
	}
}
func TestOwnedDirDoesNotAdoptOrOverwrite(t *testing.T) {
	dir := t.TempDir()
	owner := Owner{RootID: UUID(), EnvironmentID: UUID(), NodeID: UUID()}
	path := filepath.Join(dir, "owned")
	if err := OwnedDir(path, owner); err != nil {
		t.Fatal(err)
	}
	if err := VerifyOwner(path, owner); err != nil {
		t.Fatal(err)
	}
	other := owner
	other.NodeID = UUID()
	if VerifyOwner(path, other) == nil {
		t.Fatal("adopted a different owner")
	}
	if OwnedDir(path, other) == nil {
		t.Fatal("overwrote an owned directory")
	}
	foreign := filepath.Join(dir, "foreign")
	if err := os.Mkdir(foreign, 0700); err != nil {
		t.Fatal(err)
	}
	if OwnedDir(foreign, owner) == nil {
		t.Fatal("replaced an empty foreign directory")
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if VerifyOwner(link, owner) == nil {
		t.Fatal("followed directory symlink")
	}
}
func TestInternalStateCanExceedConfigLimitButStaysStrict(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	want := struct {
		Data string `json:"data"`
	}{strings.Repeat("x", 2<<20)}
	if err := Write(path, want); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Data string `json:"data"`
	}
	if err := Read(path, &got); err != nil || got.Data != want.Data {
		t.Fatal(err)
	}
	bad, _ := json.Marshal(map[string]string{"unexpected": "field"})
	if err := os.WriteFile(path, bad, 0600); err != nil {
		t.Fatal(err)
	}
	if Read(path, &got) == nil {
		t.Fatal("accepted unknown state fields")
	}
}
