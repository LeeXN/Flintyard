package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	imagebuild "github.com/LeeXN/Flintyard/internal/build"
	fy "github.com/LeeXN/Flintyard/pkg/flintyard"
)

type envelope struct {
	APIVersion  string    `json:"api_version"`
	OperationID string    `json:"operation_id"`
	OK          bool      `json:"ok"`
	Data        any       `json:"data"`
	Error       *fy.Error `json:"error"`
}

type versionResult struct {
	Version     string `json:"version"`
	Go          string `json:"go"`
	Protocol    string `json:"protocol"`
	Firecracker string `json:"firecracker"`
}

func main() {
	if len(os.Args) == 2 && os.Args[1] == "__build-worker" {
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		if err := imagebuild.Worker(ctx); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(4)
		}
		return
	}
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}
func run(args []string, out, errOut io.Writer) int {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" {
		fmt.Fprintln(out, "Flintyard — local Firecracker image and environment tools\n\nUsage: flintyard version|doctor|image build|image import|image inspect|image validate|up|status|list|logs|stop|down|gc [options] [resource]\nFlags precede positional arguments. Use -o json for structured output.\nUse COMMAND --help for options. Down discards the selected environment's writable disks.")
		return 0
	}
	command := args[0]
	args = args[1:]
	if command == "image" {
		if len(args) == 0 {
			fmt.Fprintln(errOut, "image requires a subcommand")
			return 2
		}
		command += " " + args[0]
		args = args[1:]
	}
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	fs.SetOutput(errOut)
	format := fs.String("o", "text", "output format: text or json")
	legacyJSON := fs.Bool("json", false, "alias for -o json")
	root := fs.String("state-dir", "/var/lib/flintyard", "private state directory")
	events := fs.String("events", "", "write events as JSON Lines")
	timeout := fs.Duration("timeout", 180*time.Second, "total operation deadline (follow: only when explicitly set)")
	var mode, fc, jailer, spec, output, digest string
	var maxBytes int64
	var maxFiles int
	var probe bool
	var node, source string
	var tail int
	var follow bool
	var file, wait, uidRange string
	var keepFailed, purge bool
	var recipe string
	var publicKey string
	var offline bool
	var gcApply, gcDry bool
	var gcPlan string
	switch command {
	case "version":
	case "up":
		fs.StringVar(&file, "file", "", "environment JSON file (required)")
		fs.StringVar(&wait, "wait", "configured", "configured, ssh_banner, tcp or none")
		fs.StringVar(&fc, "firecracker", "firecracker", "official Firecracker binary")
		fs.StringVar(&jailer, "jailer", "jailer", "matching official jailer binary")
		fs.StringVar(&uidRange, "uid-range", "60000:64999", "reserved numeric UID/GID range")
		fs.BoolVar(&keepFailed, "keep-failed", false, "retain confirmed resources after failure")
	case "stop":
	case "gc":
		fs.BoolVar(&gcApply, "apply", false, "apply a still-valid owned-resource plan")
		fs.BoolVar(&gcDry, "dry-run", false, "generate a plan without deleting resources (default)")
		fs.StringVar(&gcPlan, "plan", "", "write a new raw plan, or read it with --apply")
	case "down":
		fs.BoolVar(&purge, "purge", false, "also delete diagnostics after owned resources are gone; writable disks are always discarded")
	case "status", "list":
		fs.BoolVar(&probe, "probe", false, "repeat the configured access probe")
	case "logs":
		fs.StringVar(&node, "node", "", "node name (required)")
		fs.StringVar(&source, "source", "serial", "serial or firecracker startup log")
		fs.IntVar(&tail, "tail", 200, "maximum trailing lines (1-10000)")
		fs.BoolVar(&follow, "follow", false, "follow this owned log inode until cancellation; requires text output")
	case "doctor":
		fs.StringVar(&mode, "mode", "run", "run, build or validate")
		fs.StringVar(&uidRange, "uid-range", "60000:64999", "reserved numeric UID/GID range")
		fs.StringVar(&fc, "firecracker", "firecracker", "Firecracker binary")
		fs.StringVar(&jailer, "jailer", "jailer", "jailer binary")
	case "image import":
		fs.StringVar(&spec, "spec", "", "local import specification")
		fs.StringVar(&output, "output", "", "output package (must not exist)")
		fs.StringVar(&digest, "sha256", "", "trusted package digest")
		fallthrough
	case "image inspect":
		fs.Int64Var(&maxBytes, "max-bytes", 64<<30, "maximum expanded bytes")
		fs.IntVar(&maxFiles, "max-files", 32, "maximum archive file count")
	case "image build":
		fs.StringVar(&recipe, "recipe", "", "locked JSON build recipe")
		fs.BoolVar(&offline, "offline", false, "use local locked inputs/cache and isolate build commands from networking")
	case "image validate":
		fs.StringVar(&publicKey, "public-key", "", "public SSH key for seed-v1 initialization")
		fs.StringVar(&fc, "firecracker", "firecracker", "official Firecracker binary")
		fs.StringVar(&jailer, "jailer", "jailer", "matching official jailer binary")
		fs.StringVar(&uidRange, "uid-range", "60000:64999", "reserved numeric UID/GID range")
		fs.BoolVar(&keepFailed, "keep-failed", false, "retain failed validation environment")
	default: // Unsupported operations fail explicitly; no dummy successes.
	}
	pe := fs.Parse(args)
	if errors.Is(pe, flag.ErrHelp) {
		return 0
	}
	jsonOut := *format == "json" || *legacyJSON
	c := fy.New(*root)
	emit := func(data any, e error) int {
		var fe *fy.Error
		if e != nil {
			if !errors.As(e, &fe) {
				fe = fy.Failure("E_IMAGE_INVALID", "operation", e)
			}
		}
		var writeErr error
		if jsonOut {
			writeErr = json.NewEncoder(out).Encode(envelope{fy.APIVersion, c.OperationID, e == nil, data, fe})
		} else {
			writeErr = writeText(out, data)
			if fe != nil {
				fmt.Fprintf(errOut, "%s: %s\n", fe.Code, fe.Message)
			}
		}
		if writeErr != nil {
			fmt.Fprintln(errOut, writeErr)
			return 4
		}
		if e != nil {
			return fy.ExitCode(e)
		}
		return 0
	}
	if pe != nil {
		return emit(nil, fy.Failure("E_CONFIG_INVALID", "parse", pe))
	}
	if *format != "text" && *format != "json" {
		return emit(nil, fy.Failure("E_CONFIG_INVALID", "parse", fmt.Errorf("unknown output format %q; choose text or json", *format)))
	}
	if *timeout <= 0 {
		return emit(nil, fy.Failure("E_CONFIG_INVALID", "parse", fmt.Errorf("timeout must be positive")))
	}
	if follow && jsonOut {
		return emit(nil, fy.Failure("E_CONFIG_INVALID", "parse", fmt.Errorf("--follow requires text output")))
	}
	explicitTimeout := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "timeout" {
			explicitTimeout = true
		}
	})
	if !follow || explicitTimeout {
		var deadline context.CancelFunc
		ctx, deadline = context.WithTimeout(ctx, *timeout)
		defer deadline()
	}
	if *events != "" {
		f, e := os.OpenFile(*events, os.O_CREATE|os.O_APPEND|os.O_WRONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
		if e != nil {
			return emit(nil, fy.Failure("E_LOG_IO", "preflight", e))
		}
		defer f.Close()
		st, e := f.Stat()
		if e != nil || !st.Mode().IsRegular() {
			return emit(nil, fy.Failure("E_LOG_IO", "preflight", errors.Join(fmt.Errorf("events must be a regular file"), e)))
		}
		c.Events = func(ev fy.Event) error {
			if e := json.NewEncoder(f).Encode(ev); e != nil {
				return e
			}
			return f.Sync()
		}
	}
	n := fs.NArg()
	bad := func() int {
		return emit(nil, fy.Failure("E_CONFIG_INVALID", "parse", fmt.Errorf("wrong arguments for %s", command)))
	}
	switch command {
	case "image validate":
		if n != 1 {
			return bad()
		}
		r, e := c.ValidateImage(ctx, fy.ValidateRequest{Image: fs.Arg(0), PublicKey: publicKey, Firecracker: fc, Jailer: jailer, UIDRange: uidRange, KeepFailed: keepFailed})
		return emit(r, e)
	case "image build":
		if n != 0 || recipe == "" {
			return bad()
		}
		r, e := c.BuildImage(ctx, fy.BuildRequest{Recipe: recipe, Offline: offline})
		return emit(r, e)
	case "gc":
		if n != 0 || gcApply && gcDry {
			return bad()
		}
		r, e := c.GC(ctx, fy.GCRequest{Apply: gcApply, PlanFile: gcPlan})
		return emit(r, e)
	case "up":
		if n != 0 {
			return bad()
		}
		r, e := c.Up(ctx, fy.UpRequest{File: file, Wait: wait, Firecracker: fc, Jailer: jailer, UIDRange: uidRange, KeepFailed: keepFailed})
		return emit(r, e)
	case "stop":
		if n != 1 {
			return bad()
		}
		r, e := c.Stop(ctx, fy.StopRequest{Environment: fs.Arg(0)})
		return emit(r, e)
	case "down":
		if n != 1 {
			return bad()
		}
		r, e := c.Down(ctx, fy.DownRequest{Environment: fs.Arg(0), Purge: purge})
		return emit(r, e)
	case "status":
		if n != 1 {
			return bad()
		}
		r, e := c.Status(ctx, fy.StatusRequest{Environment: fs.Arg(0), Probe: probe})
		return emit(r, e)
	case "list":
		if n != 0 {
			return bad()
		}
		r, e := c.List(ctx, fy.ListRequest{Probe: probe})
		return emit(r, e)
	case "logs":
		if n != 1 {
			return bad()
		}
		if follow {
			write, closeOutput, e := logOutput(ctx, out)
			if e != nil {
				fmt.Fprintln(errOut, e)
				return 4
			}
			defer closeOutput()
			_, e = c.FollowLogs(ctx, fy.FollowLogsRequest{LogsRequest: fy.LogsRequest{Environment: fs.Arg(0), Node: node, Source: source, Tail: tail}, OnChunk: func(chunk fy.LogChunk) error { return write(chunk.Data) }})
			if e != nil {
				fmt.Fprintln(errOut, e)
				return fy.ExitCode(e)
			}
			return 0
		}
		r, e := c.Logs(ctx, fy.LogsRequest{Environment: fs.Arg(0), Node: node, Source: source, Tail: tail})
		return emit(r, e)
	case "version":
		if n != 0 {
			return bad()
		}
		return emit(versionResult{Version: fy.Version, Go: runtime.Version(), Protocol: fy.APIVersion, Firecracker: "1.17.0"}, nil)
	case "doctor":
		if n != 0 {
			return bad()
		}
		r, e := c.DoctorWithOptions(ctx, fy.DoctorRequest{Mode: mode, Firecracker: fc, Jailer: jailer, UIDRange: uidRange})
		return emit(r, e)
	case "image inspect":
		if n != 1 {
			return bad()
		}
		r, e := c.InspectImage(ctx, fy.InspectRequest{Image: fs.Arg(0), MaxBytes: maxBytes, MaxFiles: maxFiles})
		return emit(r, e)
	case "image import":
		if spec == "" && n != 1 || spec != "" && n != 0 {
			return bad()
		}
		p := ""
		if n == 1 {
			p = fs.Arg(0)
		}
		r, e := c.ImportImage(ctx, fy.ImportRequest{Package: p, Spec: spec, Output: output, SHA256: digest, MaxBytes: maxBytes, MaxFiles: maxFiles})
		return emit(r, e)
	default:
		return emit(nil, fy.Failure("E_CONFIG_INVALID", "parse", fmt.Errorf("unknown command %q; see flintyard --help", command)))
	}
}
