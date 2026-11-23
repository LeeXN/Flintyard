package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LeeXN/Flintyard/internal/contract"
	fy "github.com/LeeXN/Flintyard/pkg/flintyard"
)

func TestJSONEnvelopes(t *testing.T) {
	for _, args := range [][]string{{"version", "-o", "json"}, {"version", "--json"}, {"image", "inspect", "-o=json", "/nonexistent"}, {"image", "validate", "-o", "json"}, {"image", "validate", "-o", "json", "/nonexistent"}, {"image", "import", "-o", "json"}, {"version", "-o", "json", "--unknown"}, {"up", "-o", "json"}, {"doctor", "-o", "json", "--mode", "wrong"}} {
		t.Run(args[0], func(t *testing.T) {
			var out, errOut bytes.Buffer
			exit := run(args, &out, &errOut)
			if e := contract.Validate("result", out.Bytes()); e != nil {
				t.Fatal(e, out.String())
			}
			var v envelope
			if e := json.Unmarshal(out.Bytes(), &v); e != nil {
				t.Fatalf("not one JSON document: %q %v", out.String(), e)
			}
			if v.OK != (exit == 0) || v.OperationID == "" {
				t.Fatal(v, exit)
			}
			if exit != 0 && v.Error == nil {
				t.Fatal("missing error")
			}
		})
	}
}

func TestFlagDiagnostics(t *testing.T) {
	for _, args := range [][]string{
		{"version", "--unknown"},
		{"version", "-o", "json", "--unknown"},
		{"version", "--json", "--timeout", "invalid"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var out, errOut bytes.Buffer
			if code := run(args, &out, &errOut); code != 2 {
				t.Fatalf("exit %d, stdout %q, stderr %q", code, out.String(), errOut.String())
			}
			if len(args) == 2 {
				msg := strings.TrimSuffix(errOut.String(), "\n")
				if out.Len() != 0 || !strings.HasPrefix(msg, "E_CONFIG_INVALID: ") || strings.Contains(msg, "\n") {
					t.Fatalf("expected a single text diagnostic, stdout %q, stderr %q", out.String(), errOut.String())
				}
				return
			}
			var result envelope
			if err := json.Unmarshal(out.Bytes(), &result); err != nil || result.OK ||
				result.Error == nil || result.Error.Code != "E_CONFIG_INVALID" ||
				result.Error.Stage != "parse" || errOut.Len() != 0 {
				t.Fatalf("expected a single JSON diagnostic, stdout %q, stderr %q", out.String(), errOut.String())
			}
		})
	}
	var out, errOut bytes.Buffer
	if code := run([]string{"up", "--help"}, &out, &errOut); code != 0 || out.Len() != 0 ||
		strings.Count(errOut.String(), "Usage of up:") != 1 || !strings.Contains(errOut.String(), "-uid-range") {
		t.Fatalf("expected command help, exit %d, stdout %q, stderr %q", code, out.String(), errOut.String())
	}
}

func TestCommandErrorClassification(t *testing.T) {
	raw := errors.New("cannot read boot ID")
	imageFailure := fy.Failure("E_IMAGE_INVALID", "import", errors.New("missing image"))
	for _, tc := range []struct {
		err   error
		cause error
		code  string
		exit  int
	}{
		{raw, raw, "E_INTERNAL", 4},
		{fmt.Errorf("image inspection: %w", imageFailure), imageFailure, "E_IMAGE_INVALID", 4},
		{fmt.Errorf("interrupted: %w", context.Canceled), context.Canceled, "E_CANCELLED", 130},
	} {
		failure := commandError(tc.err)
		if failure.Code != tc.code || fy.ExitCode(failure) != tc.exit || !errors.Is(failure, tc.cause) {
			t.Fatalf("%v: got %+v", tc.err, failure)
		}
	}
}

func TestCLIInvalidStateRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "host.json"), []byte("{invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"list", "-o", "json", "--state-dir", root},
		{"status", "-o", "json", "--state-dir", root, "lab"},
		{"logs", "-o", "json", "--state-dir", root, "lab"},
		{"gc", "-o", "json", "--state-dir", root},
	} {
		var out, errOut bytes.Buffer
		if code := run(args, &out, &errOut); code != 6 || errOut.Len() != 0 {
			t.Fatalf("%v: exit %d, stdout %q, stderr %q", args, code, out.String(), errOut.String())
		}
		var result envelope
		if err := json.Unmarshal(out.Bytes(), &result); err != nil || result.OK ||
			result.Error == nil || result.Error.Code != "E_STATE_INVALID" ||
			result.Error.Stage != "preflight" {
			t.Fatalf("%v: expected a state-root error, stdout %q", args, out.String())
		}
	}
}

func TestTextStatusLabelsCachedAccess(t *testing.T) {
	observed := time.Now()
	result := fy.EnvironmentResult{
		EnvironmentID: "environment-id",
		Name:          "demo",
		State:         "running",
		Nodes: []fy.NodeResult{{
			Name:            "node-1",
			State:           "running",
			Stale:           true,
			LastProbeAt:     &observed,
			LastProbePassed: true,
		}},
	}
	var out bytes.Buffer
	if err := writeText(&out, result); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "last passed") || strings.Contains(out.String(), "failed") {
		t.Fatalf("the cached probe passed: %s", out.String())
	}
}

func TestTextOutput(t *testing.T) {
	root := filepath.Join(t.TempDir(), "missing")
	for _, tc := range []struct {
		args    []string
		code    int
		text    string
		errText string
	}{
		{args: []string{"version"}, text: "Flintyard "},
		{args: []string{"version", "-o", "text"}, text: "Flintyard "},
		{args: []string{"list", "--state-dir", root}, text: "No environments.\n"},
		{args: []string{"version", "-o", "yaml"}, code: 2, errText: "unknown output format"},
		{args: []string{"image", "inspect", filepath.Join(root, "missing.fyimg")}, code: 4, errText: "E_IMAGE_INVALID"},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			var out, errOut bytes.Buffer
			code := run(tc.args, &out, &errOut)
			if code != tc.code || json.Valid(out.Bytes()) || !strings.Contains(out.String(), tc.text) || !strings.Contains(errOut.String(), tc.errText) {
				t.Fatalf("exit %d, stdout %q, stderr %q", code, out.String(), errOut.String())
			}
			if tc.text == "" && out.Len() > 0 || tc.errText == "" && errOut.Len() > 0 {
				t.Fatalf("unexpected output, stdout %q, stderr %q", out.String(), errOut.String())
			}
		})
	}
}

func TestEnvironmentCommandEnvelopesAndReadOnlyList(t *testing.T) {
	root := filepath.Join(t.TempDir(), "missing")
	for _, args := range [][]string{{"list", "--json", "--state-dir", root}, {"status", "--json", "--state-dir", root, "lab"}, {"logs", "--json", "--state-dir", root, "lab"}, {"stop", "--json", "--state-dir", root, "lab"}, {"down", "--json", "--state-dir", root, "lab"}, {"up", "--json", "--state-dir", root, "--uid-range", "bad"}} {
		var out, errOut bytes.Buffer
		run(args, &out, &errOut)
		if err := contract.Validate("result", out.Bytes()); err != nil {
			t.Fatal(args, out.String(), err)
		}
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatal("non-creating commands initialized missing state")
	}
}

func TestGCRequiresPlanAndRejectsConflictingModes(t *testing.T) {
	for _, args := range [][]string{{"gc", "--apply", "--json"}, {"gc", "--apply", "--dry-run", "--json"}} {
		var out, errOut bytes.Buffer
		if code := run(args, &out, &errOut); code != 2 {
			t.Fatal(code, out.String(), errOut.String())
		}
		if err := contract.Validate("result", out.Bytes()); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDoctorAcceptsAndValidatesUIDRange(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run([]string{"doctor", "--json", "--mode", "build", "--uid-range", "10:9"}, &out, &errOut)
	if code != 2 || !bytes.Contains(out.Bytes(), []byte("invalid UID range")) {
		t.Fatal(code, out.String(), errOut.String())
	}
	out.Reset()
	errOut.Reset()
	code = run([]string{"doctor", "--json", "--mode", "build", "--uid-range", "60000:60099"}, &out, &errOut)
	if code == 2 || !bytes.Contains(out.Bytes(), []byte(`"mode":"build"`)) {
		t.Fatal(code, out.String(), errOut.String())
	}
}

func TestLogsFollowRejectsJSONAndKeepsErrorOffStdout(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"logs", "--follow", "-o", "json", "--node", "node", "lab"}, &out, &errOut); code != 2 {
		t.Fatal(code, out.String(), errOut.String())
	}
	if err := contract.Validate("result", out.Bytes()); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errOut.Reset()
	root := filepath.Join(t.TempDir(), "missing")
	code := run([]string{"logs", "--follow", "--state-dir", root, "--node", "node", "lab"}, &out, &errOut)
	if code == 0 || out.Len() != 0 || errOut.Len() == 0 {
		t.Fatal(code, out.String(), errOut.String())
	}
}
