package flintyard

import (
	"context"
	"errors"
	"strings"

	imagebuild "github.com/LeeXN/Flintyard/internal/build"
	"github.com/LeeXN/Flintyard/internal/image"
	"golang.org/x/sys/unix"
)

type BuildRequest struct {
	Recipe     string
	Offline    bool
	WorkerPath string
}
type BuildResult struct {
	ImageResult
	BuildID  string `json:"build_id"`
	LogPath  string `json:"log_path"`
	CacheHit bool   `json:"cache_hit"`
}

func (c *Client) BuildImage(ctx context.Context, r BuildRequest) (BuildResult, error) {
	if err := c.emit(Event{Stage: "parse", Status: "started", Message: "checking locked image recipe"}); err != nil {
		return BuildResult{}, Failure("E_CANCELLED", "parse", err)
	}
	result, err := imagebuild.Build(ctx, imagebuild.Request{StateDir: c.StateDir, RecipeFile: r.Recipe, Offline: r.Offline, WorkerPath: r.WorkerPath, Events: func(e imagebuild.Event) error {
		return c.emit(Event{Stage: e.Stage, Status: e.Status, Message: e.Message, ResourceID: e.ResourceID})
	}})
	out := BuildResult{ImageResult: result.Image, BuildID: result.BuildID, LogPath: result.LogPath, CacheHit: result.CacheHit}
	if err == nil {
		return out, nil
	}
	code, stage := "E_BUILD_FAILED", "build"
	var failure *imagebuild.Error
	if errors.As(err, &failure) {
		stage = failure.Stage
		if stage == "parse" {
			code = "E_CONFIG_INVALID"
		}
		if stage == "preflight" && strings.Contains(err.Error(), "requires root") {
			code = "E_PERMISSION"
		}
	}
	if strings.Contains(err.Error(), "architecture") {
		code = "E_ARCH_MISMATCH"
	}
	if strings.Contains(err.Error(), "digest") {
		code = "E_DIGEST_MISMATCH"
	}
	if errors.Is(err, image.ErrKernelIncompatible) {
		code = "E_KERNEL_INCOMPATIBLE"
	}
	if errors.Is(err, unix.ENOSPC) {
		code = "E_DISK_FULL"
	}
	if errors.Is(err, context.Canceled) {
		code = "E_CANCELLED"
	}
	return out, Failure(code, stage, err)
}
