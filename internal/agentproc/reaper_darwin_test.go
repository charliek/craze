//go:build darwin

package agentproc

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
	t.Cleanup(func() { kevent = real })
	return &waits
}

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
	t.Cleanup(func() { watchBranch = nil })
	return func(pid int) []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), got[pid]...)
	}
}

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
	t.Cleanup(func() { kevent = real })
}

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
			ch, stdout, stdin, err := Start(StartOptions{
				Binary: exe, Args: args, Stderr: io.Discard,
				Env: append(os.Environ(), helperEnv+"=exit-256", helperGateEnv+"="+gate),
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				ch.Shutdown()
				_ = stdin.Close()
				_ = stdout.Close()
				ch.Wait()
			})
			waitClosed(t, ch.reapedCh, 10*time.Second, "the reap")
			if got := branches(ch.PID()); len(got) != 1 || got[0] != tc.branch {
				t.Fatalf("the exit watch went %v (registration: %v), want %q", got, registered.Load(), tc.branch)
			}
			if ch.exitErr != nil || ch.waitErr != nil {
				t.Fatalf("observed %v, reaped %v; want exit 0 both times", ch.exitErr, ch.waitErr)
			}
		})
	}
}

func TestAFailingZombieQueryFallsBack(t *testing.T) {
	branches := recordBranches(t)
	var registered atomic.Value
	onRegistration(t, nil, func(_ int, err error) { registered.Store(fmt.Sprint(err)) })
	realKevent, realKinfo := kevent, kinfoProc
	kevent = func(kq int, changes, events []unix.Kevent_t, timeout *unix.Timespec) (int, error) {
		if len(events) > 0 {
			return 0, nil
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
	t.Cleanup(func() { kevent, kinfoProc, fallbackEntered = realKevent, realKinfo, nil })
	gate := filepath.Join(t.TempDir(), "go")
	ch := spawnShell(t, "while [ ! -e "+gate+" ]; do sleep 0.01; done; exit 3", nil)
	waitClosed(t, entered, 10*time.Second, "the fallback")
	if got := branches(ch.PID()); len(got) != 1 || got[0] != branchQueriesFailed {
		t.Fatalf("the exit watch went %v (registration: %v), want %q", got, registered.Load(), branchQueriesFailed)
	}
	if n := atFallback.Load(); n != zombieQueryErrors {
		t.Fatalf("%d failed zombie queries before fallback, want %d", n, zombieQueryErrors)
	}
	openGate(t, gate)
	waitClosed(t, ch.reapedCh, 10*time.Second, "the reap")
	var ee *ExitError
	if !errors.As(ch.exitErr, &ee) || ee.Error() != "exit status 3" {
		t.Fatalf("exit %v, want exit status 3", ch.exitErr)
	}
}

const srun = 2
const pLP64 = 0x00000004

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
	t.Cleanup(func() { kinfoProcs = real })
	return &scans
}

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

func TestALeaderInExitCostsTheGroupOnlyTheLastKill(t *testing.T) {
	r := recordLife(t)
	scans := stubLeaderInExit(t)
	ch := spawnShell(t, "exit 0", nil)
	sent := checkSignalsPrecedeTheReap(t, r, eventsAtTheReap(t, r, ch))
	if got, want := groupSignals(sent), fmt.Sprintf("%d ", syscall.SIGKILL); got != want {
		t.Fatalf("group signals %q, want %q", got, want)
	}
	if scans.Load() == 0 {
		t.Fatal("the cleanup never scanned the group")
	}
}
