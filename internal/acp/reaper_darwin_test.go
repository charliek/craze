//go:build darwin

package acp

import (
	"sync/atomic"
	"testing"

	"golang.org/x/sys/unix"
)

// stubObserve puts a stub over the observation's system call (kevent, the
// exit watch's registration as well as its wait) for the rest of the test:
// refuse, when set, answers every call; otherwise the first interrupts calls
// are answered EINTR and the rest made. It counts the calls.
func stubObserve(t *testing.T, interrupts int32, refuse error) *atomic.Int32 {
	t.Helper()
	var calls atomic.Int32
	real := kevent
	kevent = func(kq int, changes, events []unix.Kevent_t, timeout *unix.Timespec) (int, error) {
		n := calls.Add(1)
		if refuse != nil {
			return -1, refuse
		}
		if n <= interrupts {
			return -1, unix.EINTR
		}
		return real(kq, changes, events, timeout)
	}
	// Registered before any Spawn, so it runs after every Close.
	t.Cleanup(func() { kevent = real })
	return &calls
}
