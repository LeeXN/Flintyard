package flintyard

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LeeXN/Flintyard/internal/state"
)

func followFixture(t *testing.T, source, content string) (*Client, state.Root, environmentState, string) {
	t.Helper()
	c, root, e := environmentFixture(t)
	dir := filepath.Join(nodeDir(root, e.ID, e.Nodes[0].ID), "logs")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, source+".log")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return c, root, e, path
}
func appendFollowLog(path, content string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	_, err = f.WriteString(content)
	return errors.Join(err, f.Close())
}
func TestFollowLogsTailAppendAndCancellationBothSources(t *testing.T) {
	for _, source := range []string{"serial", "firecracker"} {
		t.Run(source, func(t *testing.T) {
			c, _, e, path := followFixture(t, source, "one\ntwo\nthree\n")
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			var chunks [][]byte
			calls := 0
			result, err := c.FollowLogs(ctx, FollowLogsRequest{LogsRequest: LogsRequest{Environment: e.ID, Node: "node-1", Source: source, Tail: 2}, OnChunk: func(chunk LogChunk) error {
				if chunk.EnvironmentID != e.ID || chunk.Node != "node-1" || chunk.Source != source {
					return errors.New("chunk identity mismatch")
				}
				chunks = append(chunks, chunk.Data)
				calls++
				if calls == 1 {
					return appendFollowLog(path, "four\n")
				}
				cancel()
				return nil
			}})
			if !errors.Is(err, context.Canceled) || ExitCode(err) != 130 || string(bytes.Join(chunks, nil)) != "two\nthree\nfour\n" || result.BytesDelivered != uint64(len("two\nthree\nfour\n")) {
				t.Fatal(result, string(bytes.Join(chunks, nil)), err)
			}
		})
	}
}
func TestFollowLogsDetectsTruncateAndRegrowAndKeepsChunksIndependent(t *testing.T) {
	c, _, e, path := followFixture(t, "serial", "old\n")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	big := "new\n" + strings.Repeat("x", 70<<10) + "\n"
	var data [][]byte
	reset, initial := false, true
	_, err := c.FollowLogs(ctx, FollowLogsRequest{LogsRequest: LogsRequest{Environment: e.ID, Node: "node-1"}, OnChunk: func(chunk LogChunk) error {
		if chunk.Reset {
			reset = true
			return nil
		}
		data = append(data, chunk.Data)
		if initial {
			initial = false
			return os.WriteFile(path, []byte(big), 0600)
		}
		if len(bytes.Join(data, nil)) == len("old\n")+len(big) {
			cancel()
		}
		return nil
	}})
	if !errors.Is(err, context.Canceled) || !reset || string(bytes.Join(data, nil)) != "old\n"+big {
		t.Fatal("truncate/regrow or retained chunk corruption", reset, len(bytes.Join(data, nil)), err)
	}
}
func TestFollowLogsReleasesEnvironmentLockBeforeConsumerDown(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("normal down requires root; other follow file/pipe cases run without root")
	}
	c, root, e, _ := followFixture(t, "serial", "diagnostic\n")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var clean CleanupResult
	_, err := c.FollowLogs(ctx, FollowLogsRequest{LogsRequest: LogsRequest{Environment: e.ID, Node: "node-1"}, OnChunk: func(chunk LogChunk) error {
		var err error
		clean, err = c.Down(ctx, DownRequest{Environment: e.ID})
		cancel()
		return err
	}})
	if !errors.Is(err, context.Canceled) || clean.State != "deleted" {
		t.Fatal("follow held the environment lock", clean, err)
	}
	stored, err := loadEnvironment(root, e.ID)
	if err != nil || stored.State != "deleted" {
		t.Fatal(stored.State, err)
	}
}
func TestFollowLogsRejectsReplacedFileLinkDirectoryAndOwner(t *testing.T) {
	for _, mode := range []string{"file", "symlink", "directory", "owner"} {
		t.Run(mode, func(t *testing.T) {
			c, root, e, path := followFixture(t, "serial", "old\n")
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			var data []byte
			_, err := c.FollowLogs(ctx, FollowLogsRequest{LogsRequest: LogsRequest{Environment: e.ID, Node: "node-1"}, OnChunk: func(chunk LogChunk) error {
				data = append(data, chunk.Data...)
				switch mode {
				case "file":
					if err := os.Rename(path, path+".old"); err != nil {
						return err
					}
					return os.WriteFile(path, []byte("NEVER STREAM FOREIGN\n"), 0600)
				case "symlink":
					foreign := filepath.Join(root.Path, "foreign")
					if err := os.WriteFile(foreign, []byte("NEVER STREAM FOREIGN\n"), 0600); err != nil {
						return err
					}
					if err := os.Remove(path); err != nil {
						return err
					}
					return os.Symlink(foreign, path)
				case "directory":
					if err := os.Rename(filepath.Dir(path), filepath.Dir(path)+".old"); err != nil {
						return err
					}
					if err := os.Mkdir(filepath.Dir(path), 0700); err != nil {
						return err
					}
					return os.WriteFile(path, []byte("NEVER STREAM FOREIGN\n"), 0600)
				case "owner":
					return state.Write(filepath.Join(nodeDir(root, e.ID, e.Nodes[0].ID), "owner.json"), state.Owner{RootID: state.UUID(), EnvironmentID: e.ID, NodeID: e.Nodes[0].ID})
				}
				return nil
			}})
			if err == nil || ExitCode(err) != 6 || string(data) != "old\n" {
				t.Fatal(mode, string(data), err)
			}
		})
	}
}
func TestFollowLogsConsumerErrorAndBoundedInitialTail(t *testing.T) {
	c, _, e, path := followFixture(t, "serial", strings.Repeat("x", 9<<20))
	sentinel := errors.New("consumer closed")
	var n int
	result, err := c.FollowLogs(context.Background(), FollowLogsRequest{LogsRequest: LogsRequest{Environment: e.ID, Node: "node-1"}, OnChunk: func(chunk LogChunk) error { n = len(chunk.Data); return sentinel }})
	if !errors.Is(err, sentinel) || !result.Truncated || n != 8<<20 {
		t.Fatal(result, n, err)
	}
	if _, err = c.FollowLogs(context.Background(), FollowLogsRequest{}); err == nil {
		t.Fatal("nil consumer accepted")
	}
	if err = os.WriteFile(path, []byte("small\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err = c.FollowLogs(ctx, FollowLogsRequest{LogsRequest: LogsRequest{Environment: e.ID, Node: "node-1"}, OnChunk: func(LogChunk) error { return nil }})
	if !errors.Is(err, context.DeadlineExceeded) || ExitCode(err) != 4 {
		t.Fatal(err)
	}
}

func TestFollowLogsDeadlineWhileOpening(t *testing.T) {
	c, root, env, _ := followFixture(t, "serial", "one line\n")
	unlock, err := lockEnvironment(context.Background(), root, env.Name)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err = c.FollowLogs(ctx, FollowLogsRequest{
		LogsRequest: LogsRequest{Environment: env.ID, Node: "node-1"},
		OnChunk: func(LogChunk) error {
			t.Fatal("log delivered while the environment lock was held")
			return nil
		},
	})
	var failure *Error
	if !errors.Is(err, context.DeadlineExceeded) || !errors.As(err, &failure) || failure.Code != "E_LOG_IO" || ExitCode(err) != 4 {
		t.Fatalf("expected a log deadline error, got %#v", err)
	}
}
