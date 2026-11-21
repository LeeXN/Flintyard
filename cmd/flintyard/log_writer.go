package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"syscall"
	"time"
)

// A blocked stdout pipe must not swallow SIGINT/TERM cancellation. Reopening
// our own descriptor gives the pipe an independent nonblocking description;
// verify its inode and leave the caller's descriptor flags/ownership intact.
func logOutput(ctx context.Context, out io.Writer) (func([]byte) error, func(), error) {
	f, ok := out.(*os.File)
	if ok {
		st, err := f.Stat()
		if err != nil {
			return nil, nil, err
		}
		if st.Mode()&os.ModeNamedPipe != 0 {
			raw, err := f.SyscallConn()
			if err != nil {
				return nil, nil, err
			}
			var pipe *os.File
			var openErr error
			err = raw.Control(func(fd uintptr) {
				pipe, openErr = os.OpenFile(fmt.Sprintf("/proc/self/fd/%d", fd), os.O_WRONLY|syscall.O_NONBLOCK, 0)
			})
			if err != nil {
				if pipe != nil {
					pipe.Close()
				}
				return nil, nil, err
			}
			if openErr != nil {
				return nil, nil, openErr
			}
			actual, err := pipe.Stat()
			if err != nil {
				pipe.Close()
				return nil, nil, err
			}
			if !os.SameFile(st, actual) {
				pipe.Close()
				return nil, nil, fmt.Errorf("stdout pipe identity changed")
			}
			write := func(b []byte) error {
				for len(b) > 0 {
					if err := ctx.Err(); err != nil {
						return err
					}
					if err := pipe.SetWriteDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
						return err
					}
					n, err := pipe.Write(b)
					b = b[n:]
					if err != nil {
						if os.IsTimeout(err) {
							continue
						}
						return err
					}
					if n == 0 {
						return io.ErrShortWrite
					}
				}
				return nil
			}
			return write, func() { _ = pipe.Close() }, nil
		}
	}
	return func(b []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := out.Write(b)
		if err == nil && n != len(b) {
			return io.ErrShortWrite
		}
		return err
	}, func() {}, nil
}
