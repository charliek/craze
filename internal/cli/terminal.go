//go:build linux || darwin

package cli

import (
	"errors"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

// isTerminal is whether f is a terminal: whether it answers the ioctl that
// reads a terminal's attributes. A character device that is not a terminal
// (/dev/null) does not, where a mode check would have taken it for one.
func isTerminal(f *os.File) bool {
	_, err := unix.IoctlGetTermios(int(f.Fd()), getTermios)
	return err == nil
}

// echoOff is a terminal craze auth login has turned the echo off on (plan
// 031 §3.7, X29; review r2): from before it draws the first prompt whose
// answer may be a key — the provider menu's, or the key's own — until the
// last answer is read, so what is typed or pasted the moment a prompt shows
// is never displayed. It is the terminal's echo alone, not a raw mode: the
// kernel still edits the line, and Ctrl-C is still a signal. The echo is
// turned off through termios directly, because x/term's ReadPassword turns it
// off only for the read it makes, after the prompt is drawn.
//
// A signal that would end craze meanwhile — Ctrl-C, SIGTERM, SIGHUP — would
// leave the echo off behind it, so a handler puts the terminal back and exits
// as the signal asked. Every change to the terminal is made under mu, the
// handler's included, and the handler holds mu through os.Exit: once it has
// put the terminal back, nothing changes it again — quiet turning the echo
// off a moment later would otherwise leave it off, since os.Exit runs no
// deferred restore. restored, under mu, is what quiet and restore check.
type echoOff struct {
	fd    int
	saved unix.Termios // the terminal as quiet found it

	mu       sync.Mutex
	restored bool // the terminal is back as found, for good

	sigs chan os.Signal
	done chan struct{}
	stop sync.Once
}

// The terminal's test seams (auth_test.go's TestAuthSignalRacesTheEchoOff,
// run in the test binary's craze child): authQuieting runs in quiet once the
// handler is in place, just before it takes mu to turn the echo off, and
// authQuieted just after it has; authRestored runs in the handler once the
// terminal is back, before craze exits. All nil in craze.
var authQuieting, authQuieted, authRestored func()

// errEchoRestored is quiet's error when a signal's handler put the terminal
// back first. craze never sees it: that handler exits holding mu, so quiet
// waits on mu until the process ends.
var errEchoRestored = errors.New("the terminal was put back for a signal")

// quiet turns f's echo off, with the handler above in place first, and
// returns what restores it.
func quiet(f *os.File) (*echoOff, error) {
	fd := int(f.Fd())
	saved, err := unix.IoctlGetTermios(fd, getTermios)
	if err != nil {
		return nil, err
	}
	q := &echoOff{fd: fd, saved: *saved, sigs: make(chan os.Signal, 1), done: make(chan struct{})}
	signal.Notify(q.sigs, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	go q.handle()
	if authQuieting != nil {
		authQuieting()
	}
	if err := q.mute(); err != nil {
		q.unhandle()
		return nil, err
	}
	if authQuieted != nil {
		authQuieted()
	}
	return q, nil
}

// mute turns the echo off, unless the terminal was put back for good.
// Canonical mode and signals stay on, and a carriage return still ends the
// line, as x/term's ReadPassword sets them.
func (q *echoOff) mute() error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.restored {
		return errEchoRestored
	}
	off := q.saved
	off.Lflag &^= unix.ECHO
	off.Lflag |= unix.ICANON | unix.ISIG
	off.Iflag |= unix.ICRNL
	return unix.IoctlSetTermios(q.fd, setTermios, &off)
}

// handle is the signal handler: it waits for a signal or for restore, and on
// a signal takes mu for good, puts the terminal back and exits 128 plus the
// signal's number, as a shell reports a process the signal ended.
func (q *echoOff) handle() {
	select {
	case sig := <-q.sigs:
		q.mu.Lock() // never unlocked: craze exits holding it
		q.restored = true
		_ = unix.IoctlSetTermios(q.fd, setTermios, &q.saved)
		if authRestored != nil {
			authRestored()
		}
		code := 128 + int(syscall.SIGINT)
		if s, ok := sig.(syscall.Signal); ok {
			code = 128 + int(s)
		}
		os.Exit(code)
	case <-q.done:
	}
}

// restore puts the terminal back as quiet found it, then takes the handler
// away — in that order, so no signal finds the echo off with nothing to turn
// it back on. A second call does nothing.
func (q *echoOff) restore() {
	q.mu.Lock()
	if !q.restored {
		q.restored = true
		_ = unix.IoctlSetTermios(q.fd, setTermios, &q.saved)
	}
	q.mu.Unlock()
	q.unhandle()
}

// unhandle takes the signal handler away.
func (q *echoOff) unhandle() {
	q.stop.Do(func() {
		signal.Stop(q.sigs)
		close(q.done)
	})
}
