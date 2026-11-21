package flintyard

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestAuditFailureBeforeMutation(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	c := New(root)
	c.Events = func(Event) error { return errors.New("audit full") }
	_, e := c.ImportImage(context.Background(), ImportRequest{Package: "nonexistent"})
	if e == nil || ExitCode(e) != 130 {
		t.Fatal(e)
	}
	if _, e = os.Stat(root); !os.IsNotExist(e) {
		t.Fatal("created state after audit failure")
	}
}
func TestCoreConfigNoCLI(t *testing.T) {
	c := New(t.TempDir())
	m, e := c.CheckEnvironment("../../examples/environment.json")
	if e != nil {
		t.Fatal(e)
	}
	if m["name"] != "ci-lab" {
		t.Fatal(m)
	}
}
func TestExitCodes(t *testing.T) {
	for code, want := range map[string]int{"E_CONFIG_INVALID": 2, "E_KVM_UNAVAILABLE": 3, "E_DIGEST_MISMATCH": 4, "E_ACCESS_TIMEOUT": 5, "E_LOCKED": 6, "E_CLEANUP_INCOMPLETE": 7, "E_CANCELLED": 130} {
		if n := ExitCode(Failure(code, "test", errors.New("test"))); n != want {
			t.Fatal(code, n)
		}
	}
}
