package host

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func TestCommandHelper(t *testing.T) {
	switch os.Args[len(os.Args)-1] {
	case "flintyard-helper-output":
		fmt.Print(strings.Repeat("x", 2*CommandOutputLimit))
		fmt.Fprint(os.Stderr, "TAIL")
		os.Exit(19)
	case "flintyard-helper-wait":
		for {
			time.Sleep(time.Hour)
		}
	case "flintyard-helper-json":
		fmt.Printf("{\"value\":%q}", strings.Repeat("x", 40000))
		fmt.Fprintln(os.Stderr, "warning: diagnostics are separate from JSON")
		os.Exit(0)
	case "flintyard-helper-large-json":
		fmt.Print(strings.Repeat("x", 8<<20+1))
		os.Exit(0)
	}
}

func TestRunJSONSeparatesStderrAndRejectsTruncation(t *testing.T) {
	var result struct {
		Value string `json:"value"`
	}
	args := []string{"-test.run=^TestCommandHelper$", "--", "flintyard-helper-json"}
	if err := RunJSON(context.Background(), CommandRequest{Name: os.Args[0], Args: args}, &result); err != nil || len(result.Value) != 40000 {
		t.Fatal(len(result.Value), err)
	}
	args[len(args)-1] = "flintyard-helper-large-json"
	if err := RunJSON(context.Background(), CommandRequest{Name: os.Args[0], Args: args}, &result); err == nil || !strings.Contains(err.Error(), "exceeds 8 MiB") {
		t.Fatal(err)
	}
}
func TestRunBoundsOutputAndPreservesExit(t *testing.T) {
	r, err := Run(context.Background(), CommandRequest{Name: os.Args[0], Args: []string{"-test.run=^TestCommandHelper$", "--", "flintyard-helper-output"}})
	var ce *CommandError
	if !errors.As(err, &ce) || r.ExitCode != 19 || !r.Truncated || len(r.Output) != CommandOutputLimit || !strings.HasSuffix(r.Output, "TAIL") {
		t.Fatal(r, err)
	}
}
func TestRunPreservesDeadline(t *testing.T) {
	_, err := Run(context.Background(), CommandRequest{Name: os.Args[0], Args: []string{"-test.run=^TestCommandHelper$", "--", "flintyard-helper-wait"}, Timeout: 30 * time.Millisecond})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
}
