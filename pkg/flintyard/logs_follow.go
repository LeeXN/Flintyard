package flintyard

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/LeeXN/Flintyard/internal/state"
)

type LogChunk struct {
	EnvironmentID, Node, Source string
	Offset                      int64
	Data                        []byte
	Reset                       bool
}
type FollowLogsRequest struct {
	LogsRequest
	// Called synchronously outside the environment lock. The consumer must
	// return promptly and may return an error to stop streaming.
	OnChunk func(LogChunk) error
}
type FollowLogsResult struct {
	EnvironmentID  string `json:"environment_id"`
	Node           string `json:"node"`
	Source         string `json:"source"`
	BytesDelivered uint64 `json:"bytes_delivered"`
	Truncated      bool   `json:"truncated"`
}

func (v *startupLog) check() error {
	owner := v.owner
	owner.NodeID = ""
	if err := state.VerifyOwner(v.envDir, owner); err != nil {
		return err
	}
	if err := state.VerifyOwner(v.nodeDir, v.owner); err != nil {
		return err
	}
	if err := state.CheckPrivateDir(v.logsDir); err != nil {
		return err
	}
	dir, err := os.Lstat(v.logsDir)
	if err != nil {
		return err
	}
	if !os.SameFile(dir, v.logsIdentity) {
		return fmt.Errorf("log directory identity changed")
	}
	current, err := os.Lstat(v.path)
	if err != nil {
		return err
	}
	pinned, err := v.file.Stat()
	if err != nil {
		return err
	}
	if !current.Mode().IsRegular() || !os.SameFile(current, pinned) {
		return fmt.Errorf("log file identity changed; restart follow to select a new file")
	}
	return nil
}
func logAnchor(f *os.File, offset int64) ([]byte, error) {
	start := max(int64(0), offset-64)
	b := make([]byte, offset-start)
	n, err := f.ReadAt(b, start)
	if errors.Is(err, io.EOF) {
		err = nil
	}
	return b[:n], err
}

// FollowLogs pins one owned ordinary inode. It never adopts a renamed file or
// follows a replacement symlink. In-place truncation restarts from offset zero.
// Memory stays bounded and stop/down are free to take the environment lock.
func (c *Client) FollowLogs(ctx context.Context, r FollowLogsRequest) (result FollowLogsResult, err error) {
	if r.OnChunk == nil {
		return result, Failure("E_CONFIG_INVALID", "parse", fmt.Errorf("log consumer is required"))
	}
	v, err := c.openStartupLog(ctx, r.LogsRequest)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return result, logStreamError(err)
		}
		return result, err
	}
	defer v.file.Close()
	result.EnvironmentID, result.Node, result.Source = v.result.EnvironmentID, v.result.Node, v.result.Source
	if err = v.check(); err != nil {
		return result, Failure("E_SPEC_CONFLICT", "logs", err)
	}
	initial, truncated, offset, err := tailLogFile(v.file, v.lines)
	if err != nil {
		return result, Failure("E_LOG_IO", "logs", err)
	}
	result.Truncated = truncated
	anchor, err := logAnchor(v.file, offset)
	if err != nil {
		return result, Failure("E_LOG_IO", "logs", err)
	}
	deliver := func(data []byte, start int64, reset bool) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := r.OnChunk(LogChunk{EnvironmentID: result.EnvironmentID, Node: result.Node, Source: result.Source, Offset: start, Data: data, Reset: reset}); err != nil {
			return err
		}
		result.BytesDelivered += uint64(len(data))
		return nil
	}
	if err = deliver(initial, offset-int64(len(initial)), false); err != nil {
		return result, logStreamError(err)
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	buffer := make([]byte, 32<<10)
	for {
		if err = ctx.Err(); err != nil {
			return result, logStreamError(err)
		}
		if err = v.check(); err != nil {
			return result, Failure("E_SPEC_CONFLICT", "logs", err)
		}
		st, err := v.file.Stat()
		if err != nil {
			return result, Failure("E_LOG_IO", "logs", err)
		}
		now, err := logAnchor(v.file, offset)
		if err != nil {
			return result, Failure("E_LOG_IO", "logs", err)
		}
		if st.Size() < offset || !bytes.Equal(now, anchor) {
			offset = 0
			anchor = nil
			if err = deliver(nil, 0, true); err != nil {
				return result, logStreamError(err)
			}
		}
		n, readErr := v.file.ReadAt(buffer, offset)
		if n > 0 {
			nextAnchor := append(append([]byte(nil), anchor...), buffer[:n]...)
			if len(nextAnchor) > 64 {
				nextAnchor = nextAnchor[len(nextAnchor)-64:]
			}
			// Copy each chunk: a consumer may retain it after returning.
			if err = deliver(append([]byte(nil), buffer[:n]...), offset, false); err != nil {
				return result, logStreamError(err)
			}
			offset += int64(n)
			anchor = nextAnchor
		}
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return result, Failure("E_DEPENDENCY", "logs", readErr)
		}
		if n == len(buffer) {
			continue
		}
		select {
		case <-ctx.Done():
			return result, logStreamError(ctx.Err())
		case <-ticker.C:
		}
	}
}
func logStreamError(err error) *Error {
	code := "E_LOG_IO"
	if errors.Is(err, context.Canceled) {
		code = "E_CANCELLED"
	}
	return Failure(code, "logs", err)
}
