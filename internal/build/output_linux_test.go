package build

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBuildOutputCannotPublishIntoScratchThroughSymlinks(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "state")
	scratch := filepath.Join(root, "builds", "existing", "root")
	if err := os.MkdirAll(scratch, 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(dir, "alias")
	if err := os.Symlink(scratch, alias); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(scratch, "out.fyimg"), filepath.Join(alias, "new-parent", "out.fyimg"), alias + "/../out.fyimg"} {
		if err := checkBuildOutput(root, path); err == nil {
			t.Fatal("accepted scratch publication", path)
		}
	}
	if err := checkBuildOutput(root, filepath.Join(dir, "exports", "new-parent", "out.fyimg")); err != nil {
		t.Fatal(err)
	}
}
