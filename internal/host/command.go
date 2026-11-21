package host

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"time"
)

const CommandOutputLimit = 32 << 10

type CommandRequest struct {
	Name    string
	Args    []string
	Input   io.Reader
	Timeout time.Duration
}

type CommandResult struct {
	Output    string
	Truncated bool
	ExitCode  int
}

type CommandError struct {
	Name   string
	Result CommandResult
	Cause  error
}

func (e *CommandError) Error() string {
	return fmt.Sprintf("%s failed (exit %d): %v: %s", e.Name, e.Result.ExitCode, e.Cause, e.Result.Output)
}
func (e *CommandError) Unwrap() error { return e.Cause }

// Run is for finite host utilities, not the detached VMM or build worker. It
// bounds diagnostic memory and time, preserves exit status and cancellation,
// and never evaluates a shell or prints to the caller's terminal.
func Run(ctx context.Context, req CommandRequest) (CommandResult, error) {
	r, _, err := run(ctx, req, false)
	return r, err
}

// RunJSON separates machine-readable stdout from bounded stderr diagnostics.
// Oversized output is rejected, never parsed as a partial kernel snapshot.
func RunJSON(ctx context.Context, req CommandRequest, result any) error {
	_, data, err := run(ctx, req, true)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, result)
}

func run(ctx context.Context, req CommandRequest, separate bool) (CommandResult, []byte, error) {
	if req.Timeout <= 0 {
		req.Timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, req.Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, req.Name, req.Args...)
	var output boundedOutput
	var data boundedOutput
	cmd.Stdin, cmd.Stdout, cmd.Stderr = req.Input, &output, &output
	if separate {
		data.limit = 8 << 20
		cmd.Stdout = &data
	}
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	r := CommandResult{Output: string(output.data), Truncated: output.truncated, ExitCode: -1}
	if cmd.ProcessState != nil {
		r.ExitCode = cmd.ProcessState.ExitCode()
	}
	if err != nil {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return r, nil, &CommandError{Name: req.Name, Result: r, Cause: err}
	}
	if separate && data.truncated {
		return r, nil, fmt.Errorf("%s JSON output exceeds 8 MiB", req.Name)
	}
	return r, data.data, nil
}

type boundedOutput struct {
	mu        sync.Mutex
	data      []byte
	truncated bool
	limit     int
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	limit := b.limit
	if limit == 0 {
		limit = CommandOutputLimit
	}
	if n >= limit {
		b.truncated = b.truncated || len(b.data)+n > limit
		b.data = append(b.data[:0], p[n-limit:]...)
	} else {
		if drop := len(b.data) + n - limit; drop > 0 {
			b.data = append(b.data[:0], b.data[drop:]...)
			b.truncated = true
		}
		b.data = append(b.data, p...)
	}
	return n, nil
}
