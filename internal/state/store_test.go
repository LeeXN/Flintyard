package state

import (
	"context"
	"errors"
	"fmt"
	"github.com/LeeXN/Flintyard/internal/contract"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestLockExclusion(t *testing.T) {
	p := filepath.Join(t.TempDir(), "lock")
	u, e := Lock(p)
	if e != nil {
		t.Fatal(e)
	}
	if v, e := Lock(p); e == nil {
		v()
		t.Fatal("double lock")
	}
	u()
	u, e = Lock(p)
	if e != nil {
		t.Fatal(e)
	}
	u()
}

func TestLockContextCancellationAndHandoff(t *testing.T) {
	p := filepath.Join(t.TempDir(), "lock")
	unlock, err := Lock(p)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	if u, err := LockContext(ctx, p); !errors.Is(err, context.DeadlineExceeded) {
		if u != nil {
			u()
		}
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		u, err := LockContext(ctx, p)
		if u != nil {
			u()
		}
		done <- err
	}()
	unlock()
	if err = <-done; err != nil {
		t.Fatal(err)
	}
}
func TestAtomicReaders(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state.json")
	if e := Write(p, map[string]int{"n": 0}); e != nil {
		t.Fatal(e)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for n := 0; n < 50; n++ {
				if e := Write(p, map[string]int{"n": n, "writer": i}); e != nil {
					errs <- e
					return
				}
				b, e := os.ReadFile(p)
				if e != nil {
					errs <- e
					return
				}
				var v map[string]int
				if e = contract.Decode(b, &v); e != nil {
					errs <- fmt.Errorf("partial state: %w", e)
					return
				}
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatal(e)
	}
}
func TestRefuseSymlinks(t *testing.T) {
	d := t.TempDir()
	link := filepath.Join(d, "link")
	if e := os.Symlink(d, link); e != nil {
		t.Fatal(e)
	}
	if PrivateDir(link) == nil {
		t.Fatal("accepted state symlink")
	}
	if u, e := Lock(link); e == nil {
		u()
		t.Fatal("accepted lock symlink")
	}
}
