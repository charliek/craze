package cli

import (
	"context"
	"errors"
	"io"
	"os"

	"golang.org/x/sys/unix"

	"github.com/charliek/craze/internal/errkind"
)

// An inherited pipe or socket can remain open indefinitely without a reader
// making progress. Use nonblocking writes so a signal or cap can end delivery.
type reviewOutput struct {
	ctx context.Context
	dst io.Writer
}

func (w *reviewOutput) Write(p []byte) (int, error) {
	file, ok := w.dst.(*os.File)
	if !ok {
		return w.dst.Write(p)
	}
	st, err := file.Stat()
	if err != nil {
		return 0, err
	}
	if st.Mode()&(os.ModeNamedPipe|os.ModeSocket) == 0 {
		return file.Write(p)
	}
	raw, err := file.SyscallConn()
	if err != nil {
		return 0, err
	}
	var n int
	var writeErr error
	if err := raw.Control(func(fd uintptr) { n, writeErr = w.writePipe(fd, p) }); err != nil {
		return n, err
	}
	return n, writeErr
}

func (w *reviewOutput) writePipe(fd uintptr, p []byte) (n int, resultErr error) {
	flags, err := unix.FcntlInt(fd, unix.F_GETFL, 0)
	if err != nil {
		return 0, err
	}
	if flags&unix.O_NONBLOCK == 0 {
		if err := unix.SetNonblock(int(fd), true); err != nil {
			return 0, err
		}
		defer func() {
			if err := unix.SetNonblock(int(fd), false); resultErr == nil {
				resultErr = err
			}
		}()
	}
	poll := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLOUT}}
	for n < len(p) {
		// Try once even after cancellation so an available reader still
		// receives the command's terminal status and saved review path.
		count, err := unix.Write(int(fd), p[n:])
		if count > 0 {
			n += count
		}
		switch err {
		case nil:
			if n == len(p) {
				return n, nil
			}
		case unix.EINTR, unix.EAGAIN:
		default:
			return n, err
		}
		if err := w.ctx.Err(); err != nil {
			return n, err
		}
		if _, err := unix.Poll(poll, 25); err != nil && err != unix.EINTR {
			return n, err
		}
	}
	return n, nil
}

func reviewOutputError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, context.Canceled):
		return exitf(3, "craze review: interrupted while writing stdout")
	case errors.Is(err, context.DeadlineExceeded):
		return exitf(5, "craze review: timed out while writing stdout")
	default:
		return kindf(errkind.Internal, "craze review: writing stdout: %s", err)
	}
}
