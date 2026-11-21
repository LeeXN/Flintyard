package build

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/LeeXN/Flintyard/internal/host"
	"github.com/LeeXN/Flintyard/internal/image"
	"github.com/LeeXN/Flintyard/internal/state"
	"golang.org/x/sys/unix"
)

const maxInputBytes = 64 << 30

type Input struct {
	Kind   string `json:"kind"`
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Source string `json:"source"`
}

func lockInput(ctx context.Context, root string, kind string, source Source, network, offline bool) (Input, error) {
	result := Input{Kind: kind, SHA256: source.SHA256, Source: "local:" + kind}
	if !image.DigestValid(source.SHA256) {
		return result, fmt.Errorf("invalid input digest")
	}
	cache := filepath.Join(root, "sources")
	if err := state.PrivateDir(cache); err != nil {
		return result, err
	}
	path := filepath.Join(cache, "sha256-"+source.SHA256)
	unlock, err := state.LockContext(ctx, path+".lock")
	if err != nil {
		return result, err
	}
	defer unlock()
	result.Path = path
	if source.URL != "" {
		u, _ := url.Parse(source.URL)
		u.RawQuery = ""
		result.Source = u.String()
	}
	if _, err = os.Lstat(path); err == nil {
		actual, n, err := image.Hash(ctx, path)
		if err != nil {
			return result, err
		}
		if n < 1 || n > maxInputBytes || actual != source.SHA256 {
			return result, fmt.Errorf("cached build input digest/size mismatch")
		}
		return result, nil
	} else if !os.IsNotExist(err) {
		return result, err
	}
	var reader io.ReadCloser
	if source.Path != "" {
		f, err := os.OpenFile(source.Path, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
		if err != nil {
			return result, err
		}
		st, err := f.Stat()
		if err != nil || !st.Mode().IsRegular() {
			f.Close()
			return result, errors.Join(err, fmt.Errorf("build input must be an ordinary file"))
		}
		reader = f
	} else {
		if offline || !network {
			return result, fmt.Errorf("offline/network-disabled build input is not in local cache")
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, source.URL, nil)
		if err != nil {
			return result, err
		}
		client := &http.Client{Timeout: 30 * time.Minute, CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > 10 || req.URL.Scheme != "https" || req.URL.User != nil {
				return fmt.Errorf("unsafe build input redirect")
			}
			return nil
		}}
		response, err := client.Do(req)
		if err != nil {
			var uerr *url.Error
			if errors.As(err, &uerr) {
				return result, fmt.Errorf("build input fetch: %w", uerr.Err)
			}
			return result, err
		}
		if response.StatusCode != http.StatusOK {
			response.Body.Close()
			return result, fmt.Errorf("build input fetch HTTP %d", response.StatusCode)
		}
		if response.ContentLength > maxInputBytes {
			response.Body.Close()
			return result, fmt.Errorf("build input exceeds 64 GiB")
		}
		reader = response.Body
	}
	defer reader.Close()
	err = host.PublishFile(ctx, path, 0400, func(f *os.File) error {
		h := sha256.New()
		n, err := io.Copy(io.MultiWriter(f, h), ctxReader{ctx, io.LimitReader(reader, maxInputBytes+1)})
		if err != nil {
			return err
		}
		if n < 1 || n > maxInputBytes || hex.EncodeToString(h.Sum(nil)) != source.SHA256 {
			return fmt.Errorf("build input digest/size mismatch")
		}
		return nil
	})
	return result, err
}

type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (r ctxReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}
