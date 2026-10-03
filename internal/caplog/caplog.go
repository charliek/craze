// Package caplog is a log file capped at a size with one rotation, safe for
// concurrent use, standard library only (plan 034 §3.3, C2a). It was craze
// serve's host log (plan 030 §3.3) and is extracted so the sign-in log can
// share the capped writer and the safe open without sharing the host log's one
// process-wide side effect: caplog never touches the runtime's crash output
// (debug.SetCrashOutput). A user that wants the file to be where a fatal error
// lands installs that itself, through Options.OnOpen and Options.OnClose.
package caplog

import (
	"fmt"
	"os"
	"sync"
	"syscall"
)

// Options are a Log's hooks. Once New has returned, both run with the log's
// lock held, so they are ordered with the writes and with each other, and must
// not call back into the Log; New's own call of OnOpen is before the Log is
// published, when nothing else can reach it, and takes no lock.
type Options struct {
	// OnOpen is called with the file a Log has just started writing to: the
	// first one, before New returns, and every one a rotation opens, once it
	// has replaced the old file — after the old one is closed (reopenLocked).
	// A user that points a process-wide resource at the file (hostlog's crash
	// output) re-points it here, so it is never left on a file this Log has
	// closed for longer than the rotation itself (the runtime keeps a
	// duplicate of the crash output's descriptor, so a fatal error in that
	// instant still lands in the renamed file).
	OnOpen func(f *os.File)
	// OnClose is called by Close, once, before the file is closed: a user
	// that gave a process-wide resource the file gives it back here, while
	// the file is still open, so that nothing writes to a closed descriptor.
	OnClose func()
}

// Log is a log file capped at max bytes with one rotation: a write that would
// take the file past max first renames it to <path>.1 — replacing the rotation
// before it — and opens a new, empty <path>, 0600. One write is never split: a
// write larger than max on its own lands whole in a fresh file. A rotation
// that fails leaves the writes going to whichever file is open, and is
// finished later: a log that cannot rotate must not stop the process that
// writes it. A rename that failed renamed nothing, and the next write past the
// cap tries the rotation again; a rename that succeeded whose new <path> could
// not be opened leaves the writes going to the renamed file and the open owed
// (reopen), tried again before every write until it succeeds — never the
// rename again, whose source is gone (astra r3-c2 5). It is safe for
// concurrent use — a host's own lines and the agent's stderr tee arrive from
// different goroutines — and every write is one write(2) under its lock, so
// lines never interleave.
type Log struct {
	mu   sync.Mutex
	path string
	max  int64
	f    *os.File
	size int64
	opts Options
	// reopen is a rotation half done: <path> was renamed to <path>.1, which f
	// still is, and the new <path> is still to be opened.
	reopen bool
	// open opens a log file (OpenFile): a seam for the test of a rotation
	// whose open fails after its rename.
	open func(path string, flag int) (*os.File, error)
}

// New is a Log over f, which the caller opened (OpenFile) at path and which
// already holds size bytes, capped at max. Rotations reopen path itself.
// opts.OnOpen is called with f before New returns.
func New(path string, f *os.File, size, max int64, opts Options) *Log {
	l := &Log{path: path, max: max, f: f, size: size, opts: opts, open: OpenFile}
	if l.opts.OnOpen != nil {
		l.opts.OnOpen(f)
	}
	return l
}

// OpenFile opens a log file for writing, 0600 when it is made: O_NOFOLLOW, so
// a symlink planted at the name is refused rather than written through, and
// anything but a regular file is refused too (a FIFO would block the writer on
// its first line). flag is O_APPEND for the log a process starts on and
// O_TRUNC for the one a rotation starts.
func OpenFile(path string, flag int) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|flag, 0o600)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !st.Mode().IsRegular() {
		_ = f.Close()
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	// O_NONBLOCK was for the open alone: a regular file's writes block, as a
	// log's should.
	if err := syscall.SetNonblock(int(f.Fd()), false); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// Write appends p: first finishing a rotation whose open is owed (reopen), or
// rotating when p would take the file past the cap.
func (l *Log) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return 0, os.ErrClosed
	}
	switch {
	case l.reopen:
		l.reopenLocked()
	case l.size > 0 && l.size+int64(len(p)) > l.max:
		l.rotateLocked()
	}
	n, err := l.f.Write(p)
	l.size += int64(n)
	return n, err
}

// TruncateIfOver empties the file when it already holds more than the cap (a
// log left by a process that ran with a larger one), and counts it empty. The
// hub calls it as it opens its log, once it holds its namespace's lock, so no
// other writer races it.
func (l *Log) TruncateIfOver() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil || l.size <= l.max {
		return nil
	}
	if err := l.f.Truncate(0); err != nil {
		return err
	}
	l.size = 0
	return nil
}

// rotateLocked renames the log to <path>.1 and opens a new one in its place
// (reopenLocked). The rename comes first, while the old file is still open,
// and the old file is closed only once the new one is: a failure at either
// step leaves the process writing to a file it has open (the renamed one, if
// only the open failed), with its size as it is. A rename that failed is tried
// again at the next write past the cap; an open that failed, at the next write
// (reopen).
func (l *Log) rotateLocked() {
	if err := os.Rename(l.path, l.path+".1"); err != nil {
		return
	}
	l.reopen = true
	l.reopenLocked()
}

// reopenLocked is a rotation's second half: <path>, renamed away, is opened
// new and empty, and the writes — and, through OnOpen, whatever the user
// pointed at the file — move to it. A failure leaves reopen set, and the
// writes on the renamed file.
func (l *Log) reopenLocked() {
	f, err := l.open(l.path, os.O_TRUNC)
	if err != nil {
		return
	}
	_ = l.f.Close()
	l.f, l.size, l.reopen = f, 0, false
	if l.opts.OnOpen != nil {
		l.opts.OnOpen(f)
	}
}

// Close closes the log. Idempotent. OnClose runs first, under the lock and
// with the file still open: hostlog's crash output is given back to stderr
// before the descriptor it pointed at goes away, as it always was.
func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	if l.opts.OnClose != nil {
		l.opts.OnClose()
	}
	err := l.f.Close()
	l.f = nil
	return err
}
