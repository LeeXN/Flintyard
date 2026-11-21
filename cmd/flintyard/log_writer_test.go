package main

import (
	"bytes"
	"context"
	"errors"
	"golang.org/x/sys/unix"
	"io"
	"os"
	"testing"
	"time"
)

func TestLogPipeBackpressureIsCancellableAndOriginalDescriptorSurvives(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	flags := func() int {
		raw, err := w.SyscallConn()
		if err != nil {
			t.Fatal(err)
		}
		var n int
		var getErr error
		if err = raw.Control(func(fd uintptr) { n, getErr = unix.FcntlInt(fd, unix.F_GETFL, 0) }); err != nil || getErr != nil {
			t.Fatal(err, getErr)
		}
		return n
	}
	before := flags()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	write, closeClone, err := logOutput(ctx, w)
	if err != nil {
		t.Fatal(err)
	}
	err = write(bytes.Repeat([]byte("x"), 1<<20))
	closeClone()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("blocked pipe ignored cancellation", err)
	}
	if _, err = w.Stat(); err != nil {
		t.Fatal("closed caller's stdout", err)
	}
	if flags() != before {
		t.Fatal("changed caller's stdout flags")
	}
}
func TestLogPipeBytesRemainUnmodified(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	write, closeClone, err := logOutput(ctx, w)
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan []byte, 1)
	go func() { b, _ := io.ReadAll(r); result <- b }()
	want := bytes.Repeat([]byte{0, 1, 255, '\n'}, 20000)
	if err = write(want); err != nil {
		t.Fatal(err)
	}
	closeClone()
	w.Close()
	if got := <-result; !bytes.Equal(got, want) {
		t.Fatal("stream bytes changed", len(got))
	}
}
