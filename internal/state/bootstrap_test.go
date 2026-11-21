package state

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestBootstrapDescriptorBindsOwnerDirectoryAndTarget(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "published")
	owner := Owner{RootID: UUID(), EnvironmentID: UUID()}
	if err := OwnedDirContents(path, owner, map[string]any{"control.json": map[string]int{"value": 1}}); err != nil {
		t.Fatal(err)
	}
	b, err := ReadBootstrap(path)
	if err != nil || b.Owner != owner || b.Destination != path || b.StagingName == filepath.Base(path) || len(b.Files) != 1 {
		t.Fatal(b, err)
	}
	active, err := PublisherExited(context.Background(), b.Creator)
	if err != nil || active {
		t.Fatal("live publication process classified dead", active, err)
	}
	creator := b.Creator
	creator.PIDNamespace = "pid:[different]"
	if _, err = PublisherExited(context.Background(), creator); err == nil {
		t.Fatal("foreign pid namespace trusted")
	}
	b.Inode++
	if err = Write(filepath.Join(path, "bootstrap.json"), b); err != nil {
		t.Fatal(err)
	}
	if _, err = ReadBootstrap(path); err == nil {
		t.Fatal("changed directory identity accepted")
	}
}
func TestBootstrapFailureBeforePublicationLeavesNoDestination(t *testing.T) {
	base := t.TempDir()
	path := filepath.Join(base, UUID())
	owner := Owner{RootID: UUID(), EnvironmentID: UUID()}
	sentinel := errors.New("publication aborted")
	err := ownedDirContents(path, owner, map[string]any{"control.json": map[string]int{"value": 1}}, func(stage string) error {
		b, err := ReadBootstrap(stage)
		if err != nil || b.Destination != path {
			t.Fatal(b, err)
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(base)
	if err != nil || len(entries) != 0 {
		t.Fatal("failed bootstrap leaked known stage", entries, err)
	}
	for _, name := range []string{"bootstrap.json", "gc-receipt.json", "../escape"} {
		if err = OwnedDirContents(path, owner, map[string]any{name: json.RawMessage(`{}`)}); err == nil {
			t.Fatal("reserved bootstrap name accepted", name)
		}
	}
}
func TestPublisherStatUsesFinalCommParenthesis(t *testing.T) {
	b := []byte("7 (worker ) with spaces) R 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 4321 0")
	if n, err := publisherStartTime(b); err != nil || n != 4321 {
		t.Fatal(n, err)
	}
}
