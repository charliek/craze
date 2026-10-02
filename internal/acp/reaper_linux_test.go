//go:build linux

package acp

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

func init() {
	if os.Getenv(helperEnv) != "main-thread-exits" {
		return
	}
	// A tool whose main thread exits while the rest of it runs on. init runs
	// on the main thread, locked to it, so SYS_EXIT — the thread's own exit,
	// not the group's — ends that thread alone; the runtime's other threads,
	// and this goroutine, go on. /proc then reads the process as a zombie
	// thread-group leader with threads left.
	go func() {
		for {
			time.Sleep(time.Hour)
		}
	}()
	runtime.LockOSThread()
	_, _, _ = syscall.RawSyscall(syscall.SYS_EXIT, 0, 0, 0)
}

// stubObserve puts a stub over the observation's system call (waitid) for the
// rest of the test. refuse, when set, answers every call — the probe at once,
// a blocking wait once hold (when set) is closed; otherwise the first
// interrupts blocking waits are answered EINTR and every other call made. It
// counts the blocking waits.
func stubObserve(t *testing.T, interrupts int32, refuse error, hold <-chan struct{}) *atomic.Int32 {
	t.Helper()
	var waits atomic.Int32
	real := waitid
	waitid = func(idType, id int, info *unix.Siginfo, options int, rusage *unix.Rusage) error {
		blocking := options&unix.WNOHANG == 0
		n := int32(0)
		if blocking {
			n = waits.Add(1)
		}
		if refuse != nil {
			if blocking && hold != nil {
				<-hold
			}
			return refuse
		}
		if blocking && n <= interrupts {
			return unix.EINTR
		}
		return real(idType, id, info, options, rusage)
	}
	// Registered before any Spawn, so it runs after every Close.
	t.Cleanup(func() { waitid = real })
	return &waits
}

// TestCLDStatus: a child's exit, as waitid's si_code and si_status say it,
// reads as the wait status the reap's would.
func TestCLDStatus(t *testing.T) {
	for _, tc := range []struct {
		code, status int32
		want         string
	}{
		{cldExited, 0, "exit status 0"},
		{cldExited, 3, "exit status 3"},
		{cldExited, 255, "exit status 255"},
		{cldKilled, int32(syscall.SIGKILL), "signal: killed"},
		{cldKilled, int32(syscall.SIGTERM), "signal: terminated"},
		{cldDumped, int32(syscall.SIGSEGV), "signal: segmentation fault (core dumped)"},
	} {
		st, err := cldStatus(tc.code, tc.status)
		if err != nil {
			t.Fatalf("cldStatus(%d, %d): %v", tc.code, tc.status, err)
		}
		if got := (&ExitError{Status: st}).Error(); got != tc.want {
			t.Errorf("cldStatus(%d, %d) reads %q, want %q", tc.code, tc.status, got, tc.want)
		}
	}
	for _, code := range []int32{0, 4, 5, 6} { // not an exit: trapped, stopped, continued
		if _, err := cldStatus(code, 0); err == nil {
			t.Errorf("cldStatus(%d) took a stop or a continue for an exit", code)
		}
	}
}

// TestSigchldInfoLayout: the _sigchld member of siginfo_t's union starts
// where the union does, after the three int header aligned as a pointer is,
// and the whole fits in unix.Siginfo.
func TestSigchldInfoLayout(t *testing.T) {
	ptr := unsafe.Sizeof(uintptr(0))
	want := (12 + ptr - 1) &^ (ptr - 1)
	if got := unsafe.Offsetof(sigchldInfo{}.pid); got != want {
		t.Fatalf("si_pid at offset %d, want %d", got, want)
	}
	if unsafe.Offsetof(sigchldInfo{}.status) != want+8 {
		t.Fatalf("si_status at offset %d, want %d", unsafe.Offsetof(sigchldInfo{}.status), want+8)
	}
	if unsafe.Sizeof(sigchldInfo{}) > unsafe.Sizeof(unix.Siginfo{}) {
		t.Fatal("sigchldInfo is larger than the siginfo it reads")
	}
}

// TestAToolWhoseMainThreadExitedIsLive (X71): a tool whose main thread has
// exited while a worker runs on reads as a zombie in /proc — a zombie
// thread-group leader with threads left — and is live all the same: the
// cleanup at the agent's exit sends the group its SIGTERM, ahead of the last
// SIGKILL, and the tool dies.
func TestAToolWhoseMainThreadExitedIsLive(t *testing.T) {
	r := recordLife(t)
	exe, args := helperArgs(t)
	dir := t.TempDir()
	c := spawnShell(t, helperEnv+"=main-thread-exits '"+exe+"' "+args[0]+" & echo $! > "+dir+"/tool.pid; "+
		"while [ ! -e "+dir+"/go ]; do sleep 0.01; done; exit 0", nil)
	tool := readPID(t, dir+"/tool.pid")
	// The premise: the tool's main thread has exited, and the tool runs on.
	deadline := time.Now().Add(10 * time.Second)
	for {
		raw, err := os.ReadFile("/proc/" + strconv.Itoa(tool) + "/stat")
		if err != nil {
			t.Fatal(err)
		}
		if st, ok := parseStat(raw); ok && st.state == 'Z' && st.threads > 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the tool never read as a zombie leader with threads left: %q", raw)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := os.WriteFile(filepath.Join(dir, "go"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	waitClosed(t, c.child.exitedCh, 10*time.Second, "the agent's exit")
	waitGone(t, tool, 2*shutdownGrace+10*time.Second, "the tool whose main thread exited")
	sent := checkSignalsPrecedeTheReap(t, r, eventsAtTheReap(t, r, c.child))
	if got := groupSignals(sent); len(got) < 3 || got[:3] != fmt.Sprintf("%d ", syscall.SIGTERM) {
		t.Fatalf("group signals %q: the cleanup never found the tool live", got)
	}
}

// TestAForkTheScanCannotSeeDiesAtTheFinalKill (X71): /proc is read a process
// at a time, so a process that joins the agent's group after the listing —
// a member's fork — is not in the scan, which can then find the group empty.
// The last SIGKILL, sent whatever the scans said while the zombie agent still
// pins the group's id, ends it. Here the scan's seam starts that process in
// the group between the listing and the reads.
func TestAForkTheScanCannotSeeDiesAtTheFinalKill(t *testing.T) {
	r := recordLife(t)
	var (
		target  atomic.Int64
		once    sync.Once
		late    *exec.Cmd
		lateErr error
		started = make(chan struct{})
	)
	procListed = func(pgid int) {
		if int64(pgid) != target.Load() {
			return
		}
		once.Do(func() {
			late = exec.Command("sleep", "60")
			late.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: pgid}
			lateErr = late.Start()
			close(started)
		})
	}
	// Registered before any Spawn, so it runs after every Close.
	t.Cleanup(func() { procListed = nil })
	dir := t.TempDir()
	c := spawnShell(t, "while [ ! -e "+dir+"/go ]; do sleep 0.01; done; exit 0", nil)
	target.Store(int64(c.child.pgid))
	if err := os.WriteFile(filepath.Join(dir, "go"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	waitClosed(t, started, 10*time.Second, "the scan")
	if lateErr != nil {
		t.Fatal(lateErr)
	}
	t.Cleanup(func() { _ = late.Process.Kill() })
	waitClosed(t, c.child.reapedCh, 10*time.Second, "the reap")
	ended := make(chan error, 1)
	go func() { ended <- late.Wait() }()
	select {
	case err := <-ended:
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.String() != "signal: killed" {
			t.Fatalf("the late member ended with %v, want signal: killed", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the process the scan could not see outlived the reap")
	}
	if got, want := groupSignals(r.of(c.child.pgid)), fmt.Sprintf("%d ", syscall.SIGKILL); got != want {
		t.Fatalf("group signals %q, want %q: the scan should have found the group empty", got, want)
	}
}

// TestAStatReadErrorCountsAsLive (X71): a /proc/<pid>/stat that cannot be read
// makes the group live, with the error — unknown is live; a process gone
// since the listing (ENOENT, ESRCH) is not there; EINTR is retried.
func TestAStatReadErrorCountsAsLive(t *testing.T) {
	real := statRead
	t.Cleanup(func() { statRead = real })
	own := syscall.Getpgrp() // this process lives in it
	for _, tc := range []struct {
		err      error
		live     bool
		reported bool
	}{
		{syscall.EACCES, true, true},
		{syscall.EMFILE, true, true},
		{syscall.ENOENT, false, false},
		{syscall.ESRCH, false, false},
	} {
		statRead = func(string, []byte) (int, error) { return 0, tc.err }
		live, err := groupHasLiveMember(own)
		if live != tc.live || (err != nil) != tc.reported {
			t.Errorf("every read failing with %v: live %v, err %v; want live %v, an error %v", tc.err, live, err, tc.live, tc.reported)
		}
	}
	interrupted := map[string]bool{}
	statRead = func(path string, buf []byte) (int, error) {
		if !interrupted[path] {
			interrupted[path] = true
			return 0, syscall.EINTR
		}
		return real(path, buf)
	}
	if live, err := groupHasLiveMember(own); !live || err != nil {
		t.Fatalf("every read interrupted once: live %v, err %v; want this process's group live", live, err)
	}
}
