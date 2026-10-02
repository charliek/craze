//go:build darwin

package acp

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// stubObserve puts a stub over the observation's system call (kevent) for the
// rest of the test. refuse, when set, answers every call — the registration's
// at once; otherwise the first interrupts waits (the calls that ask for an
// event, not the registration) are answered EINTR and every other call made.
// hold, when set, keeps a refused wait until it is closed. It counts the
// waits.
func stubObserve(t *testing.T, interrupts int32, refuse error, hold <-chan struct{}) *atomic.Int32 {
	t.Helper()
	var waits atomic.Int32
	real := kevent
	kevent = func(kq int, changes, events []unix.Kevent_t, timeout *unix.Timespec) (int, error) {
		waiting := len(events) > 0
		n := int32(0)
		if waiting {
			n = waits.Add(1)
		}
		if refuse != nil {
			if waiting && hold != nil {
				<-hold
			}
			return -1, refuse
		}
		if waiting && n <= interrupts {
			return -1, unix.EINTR
		}
		return real(kq, changes, events, timeout)
	}
	// Registered before any Spawn, so it runs after every Close.
	t.Cleanup(func() { kevent = real })
	return &waits
}

// TestAnExitOf256IsTheReapsOnEveryPath (X71): exit(256) is kept by the kernel
// as 0x10000; wait4, and the NOTE_EXIT event, report its low 16 bits (exit
// 0), while the zombie's kinfo_proc saturates it to 0xffff. So an agent found
// already a zombie — the exit watch's registration refused with ESRCH, held
// here until the agent is one — reports the reap's status, not the zombie
// record's; and an agent watched as it exits reports the event's, which says
// the same.
func TestAnExitOf256IsTheReapsOnEveryPath(t *testing.T) {
	exe, args := helperArgs(t)
	for _, zombieFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("already a zombie %v", zombieFirst), func(t *testing.T) {
			var registered atomic.Value
			if zombieFirst {
				real := kevent
				kevent = func(kq int, changes, events []unix.Kevent_t, timeout *unix.Timespec) (int, error) {
					if len(changes) == 0 {
						return real(kq, changes, events, timeout)
					}
					pid := int(changes[0].Ident)
					deadline := time.Now().Add(10 * time.Second)
					for {
						if z, err := isZombie(pid); (err == nil && z) || time.Now().After(deadline) {
							break
						}
						time.Sleep(5 * time.Millisecond)
					}
					n, err := real(kq, changes, events, timeout)
					registered.Store(fmt.Sprint(err))
					return n, err
				}
				// Registered before any Spawn, so it runs after every Close.
				t.Cleanup(func() { kevent = real })
			}
			c, err := Spawn(SpawnOptions{Binary: exe, Args: args, Env: append(os.Environ(), helperEnv+"=exit-256"), Stderr: io.Discard})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = c.Close() })
			waitClosed(t, c.child.reapedCh, 10*time.Second, "the reap")
			if c.child.exitErr != nil || c.child.waitErr != nil {
				t.Fatalf("observed %v, reaped %v; want the reap's exit 0 both times", c.child.exitErr, c.child.waitErr)
			}
			t.Logf("the registration said %v", registered.Load())
		})
	}
}

// TestAFailingZombieQueryFallsBack (X71): an exit watch whose NOTE_EXIT never
// comes and whose zombie query keeps failing gives up after
// zombieQueryErrors failures in a row, and the reaper falls back to polling
// wait4, which reaps the agent and reports its status.
func TestAFailingZombieQueryFallsBack(t *testing.T) {
	realKevent, realKinfo := kevent, kinfoProc
	kevent = func(kq int, changes, events []unix.Kevent_t, timeout *unix.Timespec) (int, error) {
		if len(events) > 0 {
			return 0, nil // every wait times out: a NOTE_EXIT missed
		}
		return realKevent(kq, changes, events, timeout)
	}
	kinfoProc = func(string, ...int) (*unix.KinfoProc, error) { return nil, unix.EIO }
	entered := make(chan struct{})
	fallbackEntered = func(int) { close(entered) }
	// Registered before any Spawn, so it runs after every Close.
	t.Cleanup(func() { kevent, kinfoProc, fallbackEntered = realKevent, realKinfo, nil })
	c := spawnShell(t, "exit 3", nil)
	waitClosed(t, entered, 10*time.Second, "the fallback")
	waitClosed(t, c.child.reapedCh, 10*time.Second, "the reap")
	var ee *ExitError
	if !errors.As(c.child.exitErr, &ee) || ee.Error() != "exit status 3" {
		t.Fatalf("exit %v, want exit status 3", c.child.exitErr)
	}
}
