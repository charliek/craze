//go:build darwin

package acp

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
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

// recordBranches puts a stub over watchBranch for the rest of the test: it
// notes which way the exit watch went for each pid.
func recordBranches(t *testing.T) func(pid int) []string {
	var (
		mu  sync.Mutex
		got = map[int][]string{}
	)
	watchBranch = func(pid int, branch string) {
		mu.Lock()
		got[pid] = append(got[pid], branch)
		mu.Unlock()
	}
	// Registered before any Spawn, so it runs after every Close.
	t.Cleanup(func() { watchBranch = nil })
	return func(pid int) []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), got[pid]...)
	}
}

// onRegistration puts a stub over kevent for the rest of the test that runs
// around the exit watch's registration — before, with the pid it is for, and
// after, with the registration's error — and passes every other call on.
func onRegistration(t *testing.T, before func(pid int), after func(pid int, err error)) {
	real := kevent
	kevent = func(kq int, changes, events []unix.Kevent_t, timeout *unix.Timespec) (int, error) {
		if len(changes) == 0 {
			return real(kq, changes, events, timeout)
		}
		pid := int(changes[0].Ident)
		if before != nil {
			before(pid)
		}
		n, err := real(kq, changes, events, timeout)
		if after != nil {
			after(pid, err)
		}
		return n, err
	}
	// Registered before any Spawn, so it runs after every Close.
	t.Cleanup(func() { kevent = real })
}

// TestAnExitOf256IsTheReapsOnEveryPath (X71, X73): exit(256) is kept by the
// kernel as 0x10000; wait4, and the NOTE_EXIT event, report its low 16 bits
// (exit 0), while the zombie's kinfo_proc saturates it to 0xffff. The agent —
// this binary as a helper — exits only once a gate opens: after its exit
// watch registered, for the event's path; before, and only once it is a
// zombie, for the path where the registration is refused (ESRCH). Each path
// is checked to be the one taken; the event's reports its own status, the
// other the reap's — never the zombie record's.
func TestAnExitOf256IsTheReapsOnEveryPath(t *testing.T) {
	exe, args := helperArgs(t)
	for _, tc := range []struct {
		name, branch string
		zombieFirst  bool
	}{
		{"its NOTE_EXIT", branchEvent, false},
		{"already a zombie", branchGoneAtRegistration, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			branches := recordBranches(t)
			gate := filepath.Join(t.TempDir(), "go")
			var registered atomic.Value
			onRegistration(t, func(pid int) {
				if !tc.zombieFirst {
					return
				}
				openGate(t, gate)
				deadline := time.Now().Add(10 * time.Second)
				for z, err := isZombie(pid); (err != nil || !z) && time.Now().Before(deadline); z, err = isZombie(pid) {
					time.Sleep(5 * time.Millisecond)
				}
			}, func(_ int, err error) {
				registered.Store(fmt.Sprint(err))
				if !tc.zombieFirst && err == nil {
					openGate(t, gate)
				}
			})
			c, err := Spawn(SpawnOptions{
				Binary: exe, Args: args, Stderr: io.Discard,
				Env: append(os.Environ(), helperEnv+"=exit-256", helperGateEnv+"="+gate),
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = c.Close() })
			waitClosed(t, c.child.reapedCh, 10*time.Second, "the reap")
			if got := branches(c.PID()); len(got) != 1 || got[0] != tc.branch {
				t.Fatalf("the exit watch went %v (the registration said %v), want %q", got, registered.Load(), tc.branch)
			}
			if c.child.exitErr != nil || c.child.waitErr != nil {
				t.Fatalf("observed %v, reaped %v; want the reap's exit 0 both times", c.child.exitErr, c.child.waitErr)
			}
		})
	}
}

// TestAFailingZombieQueryFallsBack (X71, X73): an exit watch whose NOTE_EXIT
// never comes — its registration made while the agent runs, every event
// suppressed — asks for the zombie on each timed-out wait, and when those
// queries keep failing gives up after zombieQueryErrors of them in a row: the
// reaper falls back to polling wait4, which reaps the agent once a gate lets
// it exit and reports its status.
func TestAFailingZombieQueryFallsBack(t *testing.T) {
	branches := recordBranches(t)
	var registered atomic.Value
	onRegistration(t, nil, func(_ int, err error) { registered.Store(fmt.Sprint(err)) })
	realKevent, realKinfo := kevent, kinfoProc
	kevent = func(kq int, changes, events []unix.Kevent_t, timeout *unix.Timespec) (int, error) {
		if len(events) > 0 {
			return 0, nil // every wait times out: the NOTE_EXIT suppressed
		}
		return realKevent(kq, changes, events, timeout)
	}
	var queries atomic.Int32
	kinfoProc = func(string, ...int) (*unix.KinfoProc, error) {
		queries.Add(1)
		return nil, unix.EIO
	}
	var atFallback atomic.Int32
	entered := make(chan struct{})
	fallbackEntered = func(int) {
		atFallback.Store(queries.Load())
		close(entered)
	}
	// Registered before any Spawn, so it runs after every Close.
	t.Cleanup(func() { kevent, kinfoProc, fallbackEntered = realKevent, realKinfo, nil })
	gate := filepath.Join(t.TempDir(), "go")
	c := spawnShell(t, "while [ ! -e "+gate+" ]; do sleep 0.01; done; exit 3", nil)
	waitClosed(t, entered, 10*time.Second, "the fallback")
	if got := branches(c.PID()); len(got) != 1 || got[0] != branchQueriesFailed {
		t.Fatalf("the exit watch went %v (the registration said %v), want %q", got, registered.Load(), branchQueriesFailed)
	}
	if n := atFallback.Load(); n != zombieQueryErrors {
		t.Fatalf("%d failed zombie queries before the fallback, want %d", n, zombieQueryErrors)
	}
	openGate(t, gate)
	waitClosed(t, c.child.reapedCh, 10*time.Second, "the reap")
	var ee *ExitError
	if !errors.As(c.child.exitErr, &ee) || ee.Error() != "exit status 3" {
		t.Fatalf("exit %v, want exit status 3", c.child.exitErr)
	}
}

// srun is a runnable process's p_stat, SRUN in sys/proc.h.
const srun = 2

// pLP64 is the p_flag bit every 64-bit process has, P_LP64 in XNU's
// bsd/sys/proc.h.
const pLP64 = 0x00000004

// stubLeaderInExit puts a stub over the group scan's sysctl (kinfoProcs) for
// the rest of the test: a group is listed as its leader alone — the agent,
// whose pid is the group's id — runnable, with P_WEXIT among its flags, as
// macOS can list an agent whose NOTE_EXIT came before its zombie state (run
// 37005407853). It counts the scans.
func stubLeaderInExit(t *testing.T) *atomic.Int32 {
	t.Helper()
	var scans atomic.Int32
	real := kinfoProcs
	kinfoProcs = func(name string, args ...int) ([]unix.KinfoProc, error) {
		if name != "kern.proc.pgrp" || len(args) != 1 {
			return real(name, args...)
		}
		scans.Add(1)
		var kp unix.KinfoProc
		kp.Proc.P_pid = int32(args[0])
		kp.Proc.P_stat = srun
		kp.Proc.P_flag = pLP64 | pWexit
		return []unix.KinfoProc{kp}, nil
	}
	// Registered before any Spawn, so it runs after every Close.
	t.Cleanup(func() { kinfoProcs = real })
	return &scans
}

// TestALeaderInExitCostsTheCleanupNothing (plan 035 P10): a group listed as
// the agent alone, still in its exit — runnable, P_WEXIT set, not yet a
// zombie — has no live member, so the cleanup sends nothing and waits for
// nothing: the last SIGKILL is the reaper's, after it (reap), not the
// cleanup's. Under the old rule, any member not a zombie live, it sent a
// SIGTERM and polled out the grace. The child is made up — its pid beyond
// macOS's 99999, so no group has its id — and only its cleanup runs.
func TestALeaderInExitCostsTheCleanupNothing(t *testing.T) {
	r := recordLife(t)
	scans := stubLeaderInExit(t)
	const agent = 1 << 30
	ch := &Child{pid: agent, pgid: agent}
	ch.cleanGroup()
	if got := groupSignals(r.of(agent)); got != "" {
		t.Fatalf("the cleanup sent the group %q, want nothing", got)
	}
	if scans.Load() == 0 {
		t.Fatal("the cleanup never scanned the group")
	}
}

// TestALeaderInExitCostsTheGroupOnlyTheLastKill (plan 035 P10): the whole
// reaper, over an agent's clean exit, its scans listing the agent still in
// its exit, sends the group the last SIGKILL alone, before the reap, and
// nothing after it: TestEveryGroupSignalPrecedesTheReap's "a clean exit",
// with the schedule run 37005407853 met — the NOTE_EXIT ahead of the zombie —
// forced.
func TestALeaderInExitCostsTheGroupOnlyTheLastKill(t *testing.T) {
	r := recordLife(t)
	scans := stubLeaderInExit(t)
	c := spawnShell(t, "exit 0", nil)
	sent := checkSignalsPrecedeTheReap(t, r, eventsAtTheReap(t, r, c.child))
	if got, want := groupSignals(sent), fmt.Sprintf("%d ", syscall.SIGKILL); got != want {
		t.Fatalf("group signals %q, want %q", got, want)
	}
	if scans.Load() == 0 {
		t.Fatal("the cleanup never scanned the group")
	}
}
