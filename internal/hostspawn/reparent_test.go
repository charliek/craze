package hostspawn

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/charliek/craze/internal/rundir"
)

// The reparenting start (plan 041 X32) with this test binary as both of its
// processes: run marked ReparentEnv it is the intermediate — Reparent, first
// thing, as craze's main runs it — and run marked fakeHostEnv it is a
// stand-in for craze serve (fakeHost), which says on its ready pipe what it
// finds of itself. Every wait is bounded (step).

const (
	// fakeHostEnv makes this test binary the stand-in host (fakeHost).
	fakeHostEnv = "CRAZE_HOSTSPAWN_TEST_HOST"
	// fakeHostParent is the test process's pid: the stand-in exits once it
	// has gone, so none outlives a test binary that died.
	fakeHostParent = "CRAZE_HOSTSPAWN_TEST_PARENT"
	// holdEnv is a directory: the intermediate, its host started, waits
	// (ReparentStarted) for the stand-in host to have written its line and
	// created <dir>/up there, and only then reports — so a test makes the
	// report fail with the host up and announced (C6 r1, 1).
	holdEnv = "CRAZE_HOSTSPAWN_TEST_HOLD"
	// markFailEnv makes the intermediate's marking of its descriptors fail
	// (markCloseOnExec): what it does then, before any host (C11).
	markFailEnv = "CRAZE_HOSTSPAWN_TEST_MARK_FAIL"
	// step bounds each wait here.
	step = 30 * time.Second
)

func init() {
	if _, ok := os.LookupEnv(markFailEnv); ok {
		markCloseOnExec = func(int) error { return errors.New("descriptor 9: refused by the test") }
	}
	if dir, ok := os.LookupEnv(holdEnv); ok {
		ReparentStarted = func(int) {
			up := filepath.Join(dir, "up")
			for born := time.Now(); time.Since(born) < step; time.Sleep(5 * time.Millisecond) {
				if _, err := os.Stat(up); err == nil {
					return
				}
			}
		}
	}
	Reparent()
	if _, ok := os.LookupEnv(fakeHostEnv); ok {
		fakeHost()
	}
}

// hostSelf is what the stand-in host finds of itself.
type hostSelf struct {
	PID, SID int
	// Null: stdin, stdout and stderr are all /dev/null.
	Null bool
	// Pipes is every descriptor it holds that is a pipe: the ready pipe's 3
	// alone, unless the intermediate's report pipe reached it too.
	Pipes []int
	Env   []string
}

// fakeHost writes hostSelf as one JSON line on its ready pipe (fd 3), closes
// it, and waits: for SIGTERM (its default ends it), for the test process to
// be gone, or for a minute — then exits.
func fakeHost() {
	parent, _ := strconv.Atoi(os.Getenv(fakeHostParent))
	sid, _ := unix.Getsid(0)
	self := hostSelf{PID: os.Getpid(), SID: sid, Null: stdioNull(), Env: os.Environ()}
	// Every pipe it holds: the ready pipe, and the report pipe had it leaked.
	for fd := range 256 {
		var st unix.Stat_t
		if unix.Fstat(fd, &st) == nil && st.Mode&unix.S_IFMT == unix.S_IFIFO {
			self.Pipes = append(self.Pipes, fd)
		}
	}
	ready := os.NewFile(3, "ready")
	_ = json.NewEncoder(ready).Encode(self)
	_ = ready.Close()
	if dir, ok := os.LookupEnv(holdEnv); ok {
		_ = os.WriteFile(filepath.Join(dir, "up"), nil, 0o600)
	}
	for born := time.Now(); time.Since(born) < time.Minute; time.Sleep(20 * time.Millisecond) {
		if parent <= 1 || errors.Is(syscall.Kill(parent, 0), syscall.ESRCH) {
			break
		}
	}
	syscall.Exit(0)
}

// stdioNull reports whether fds 0, 1 and 2 are all /dev/null.
func stdioNull() bool {
	var null unix.Stat_t
	if unix.Stat(os.DevNull, &null) != nil {
		return false
	}
	for fd := range 3 {
		var st unix.Stat_t
		if unix.Fstat(fd, &st) != nil || st.Rdev != null.Rdev || st.Ino != null.Ino {
			return false
		}
	}
	return true
}

// fakeHostCmd is this test binary as the stand-in host, its environment this
// process's and extra.
func fakeHostCmd(t *testing.T, extra ...string) *exec.Cmd {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^$")
	cmd.Env = append(os.Environ(), append([]string{fakeHostEnv + "=1", fakeHostParent + "=" + strconv.Itoa(os.Getpid())}, extra...)...)
	return cmd
}

// readLine reads one line from f within step.
func readLine(t *testing.T, f *os.File) (string, error) {
	t.Helper()
	type res struct {
		line string
		err  error
	}
	got := make(chan res, 1)
	go func() {
		line, err := bufio.NewReader(f).ReadString('\n')
		got <- res{line, err}
	}()
	select {
	case r := <-got:
		return r.line, r.err
	case <-time.After(step):
		t.Fatalf("no line within %v", step)
		return "", nil
	}
}

// TestStartReparentedLeavesTheSpawnersTree is the reparenting start's whole
// contract: the Child is the host the intermediate started, not the
// intermediate — which has exited 0 and been reaped — and the host's parent
// is neither this process nor the intermediate; it leads a session of its
// own, its stdio is /dev/null, its environment carries the ready pipe's marks
// and the caller's but not ReparentEnv, and the report pipe never reached it;
// the ready pipe is the host's alone (EOF once it closed it). The Child, an
// identity and no reaper, says it serves, then ends it: Terminate returns
// with it gone, its pid no one's, its Err nil.
//
// The spawner here holds a pipe that is not close-on-exec, as a CI runner
// hands its test process (C10): the intermediate inherits it, and the host
// must not — its pipes are the ready pipe's 3 alone.
func TestStartReparentedLeavesTheSpawnersTree(t *testing.T) {
	inheritedR, inheritedW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = inheritedR.Close(); _ = inheritedW.Close() })
	for _, f := range []*os.File{inheritedR, inheritedW} {
		if _, err := unix.FcntlInt(f.Fd(), unix.F_SETFD, 0); err != nil {
			t.Fatal(err)
		}
	}
	cmd := fakeHostCmd(t, "CRAZE_HOSTSPAWN_TEST_MARK=kept")
	child, r, err := StartReparented(context.Background(), cmd, HostChildEnv, "", "", ReparentSpawn{})
	if err != nil {
		t.Fatalf("StartReparented: %v", err)
	}
	defer func() { _ = r.Close() }()
	t.Cleanup(func() {
		if !child.Exited() {
			child.Terminate()
		}
	})
	line, err := readLine(t, r)
	if err != nil {
		t.Fatalf("the host's line: %q, %v", line, err)
	}
	var self hostSelf
	if err := json.Unmarshal([]byte(line), &self); err != nil {
		t.Fatalf("the host's line %q: %v", line, err)
	}
	mid := cmd.Process.Pid
	switch {
	case self.PID != child.PID() || self.PID == mid:
		t.Fatalf("the host is pid %d, the Child %d, the intermediate %d", self.PID, child.PID(), mid)
	case cmd.ProcessState == nil || !cmd.ProcessState.Success():
		t.Fatalf("the intermediate has not exited 0 and been reaped: %v", cmd.ProcessState)
	}
	id, err := rundir.ProcessIdentity(self.PID)
	if err != nil || id.PPID == os.Getpid() || id.PPID == mid {
		t.Fatalf("the host's parent is %d (%v): this process is %d, the intermediate %d", id.PPID, err, os.Getpid(), mid)
	}
	if self.SID != self.PID || !self.Null || !slices.Equal(self.Pipes, []int{3}) {
		t.Fatalf("the host: session %d (its pid %d), stdio /dev/null %v, its pipes %v (want the ready pipe's 3 alone)", self.SID, self.PID, self.Null, self.Pipes)
	}
	for _, kv := range []string{ReadyFDEnv + "=3", HostChildEnv + "=1", "CRAZE_HOSTSPAWN_TEST_MARK=kept"} {
		if !slices.Contains(self.Env, kv) {
			t.Errorf("the host's environment lacks %s", kv)
		}
	}
	if slices.ContainsFunc(self.Env, func(kv string) bool { return strings.HasPrefix(kv, ReparentEnv+"=") }) {
		t.Errorf("the host's environment carries %s", ReparentEnv)
	}
	if more, err := readLine(t, r); err == nil || more != "" {
		t.Fatalf("after the host's line: %q, %v; want EOF, the host's the only write end", more, err)
	}
	if child.Exited() || child.Err() != nil {
		t.Fatalf("the serving host: exited %v, err %v", child.Exited(), child.Err())
	}
	child.Terminate()
	if !child.Exited() || child.Err() != nil {
		t.Fatalf("after Terminate: exited %v, err %v", child.Exited(), child.Err())
	}
	select {
	case <-child.Done():
	default:
		t.Fatal("Done is open with the host gone")
	}
	if _, err := rundir.ProcessIdentity(self.PID); !errors.Is(err, rundir.ErrNoProcess) {
		t.Fatalf("the host's pid after Terminate: %v", err)
	}
}

// TestCloseOnExecListedMarksWhatItLists (C10): the walk the intermediate
// falls back to — macOS's only way, /dev/fd; Linux's before close_range's
// CLOEXEC flag (5.11), /proc/self/fd — marks close-on-exec each descriptor it
// lists from first up: a pipe whose flag was cleared has it again, both ends,
// and the walk's own directory descriptor, closed under it, is no error.
func TestCloseOnExecListedMarksWhatItLists(t *testing.T) {
	dir := "/dev/fd"
	if runtime.GOOS == "linux" {
		dir = "/proc/self/fd"
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close(); _ = w.Close() })
	fds := []int{int(r.Fd()), int(w.Fd())}
	for _, fd := range fds {
		if _, err := unix.FcntlInt(uintptr(fd), unix.F_SETFD, 0); err != nil {
			t.Fatal(err)
		}
	}
	if err := closeOnExecListed(dir, min(fds[0], fds[1])); err != nil {
		t.Fatalf("closeOnExecListed(%s): %v", dir, err)
	}
	for _, fd := range fds {
		if flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); err != nil || flags&unix.FD_CLOEXEC == 0 {
			t.Fatalf("descriptor %d after the walk: flags %#x, %v; want close-on-exec", fd, flags, err)
		}
	}
}

// TestAnIntermediateThatCannotKeepItsDescriptorsStartsNoHost (C11): when
// the intermediate cannot mark the descriptors past the ready pipe
// close-on-exec (markFailEnv), it starts no host — one that would hold them
// for its life — and exits 1 with no report: the start fails, no Child, no
// ready pipe, the intermediate reaped, and no host ever ran (the stand-in
// host, which says it is up in holdEnv's directory, never did).
func TestAnIntermediateThatCannotKeepItsDescriptorsStartsNoHost(t *testing.T) {
	dir := t.TempDir()
	cmd := fakeHostCmd(t, markFailEnv+"=1", holdEnv+"="+dir)
	child, r, err := StartReparented(context.Background(), cmd, HostChildEnv, "", "", ReparentSpawn{})
	if err == nil {
		_ = r.Close()
		child.Terminate()
		t.Fatal("a start whose intermediate could not keep its descriptors from the host succeeded")
	}
	if child != nil || r != nil || !strings.Contains(err.Error(), "it exited without a report") {
		t.Fatalf("StartReparented: %v, %v, %v; want no host reported", child, r, err)
	}
	if cmd.ProcessState == nil || cmd.ProcessState.ExitCode() != 1 {
		t.Fatalf("the intermediate's end: %v; want exit 1, reaped", cmd.ProcessState)
	}
	if _, err := os.Stat(filepath.Join(dir, "up")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a host ran: %v", err)
	}
}

// TestAStrayMarkIsRefused: craze marked ReparentEnv without what
// StartReparented hands an intermediate — a descriptor past the ready pipe's
// that is a pipe, and the ready pipe itself, ReadyFDEnv=3 — exits 2 and
// starts nothing (a craze that went on would run no test and exit 0), and
// writes no report on a pipe it was handed.
func TestAStrayMarkIsRefused(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, mark string
		ready      bool
	}{
		{"not a number", "x", true},
		{"the ready pipe's own", "3", true},
		{"no such descriptor", "9", true},
		{"no ready pipe", "4", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			readyR, readyW, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			reportR, reportW, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = readyR.Close(); _ = reportR.Close() }()
			cmd := exec.Command(exe, "-test.run=^$")
			cmd.Env = append(withoutEnv(os.Environ(), ReadyFDEnv), ReparentEnv+"="+tc.mark, fakeHostEnv+"=1")
			cmd.ExtraFiles = []*os.File{readyW, reportW}
			if tc.ready {
				cmd.Env = append(cmd.Env, ReadyFDEnv+"=3")
			}
			out, err := cmd.CombinedOutput()
			_ = readyW.Close()
			_ = reportW.Close()
			var ee *exec.ExitError
			if !errors.As(err, &ee) || ee.ExitCode() != 2 || !strings.Contains(string(out), ReparentEnv) {
				t.Fatalf("craze marked %s=%s: %v, %q; want exit 2 naming it", ReparentEnv, tc.mark, err, out)
			}
			if line, err := readLine(t, reportR); line != "" || err == nil {
				t.Fatalf("the report pipe: %q, %v; want nothing", line, err)
			}
			if line, err := readLine(t, readyR); line != "" || err == nil {
				t.Fatalf("the ready pipe: %q, %v; want nothing — no host started", line, err)
			}
		})
	}
}

// TestABrokenIntermediateIsAFailedStart: an intermediate that exits without a
// report, writes one that is not a report — not two numbers, pid 1, no start
// time — or never writes at all within ReparentWait, is a start that failed:
// an error, no Child and no ready pipe, and the intermediate reaped — killed
// first when it would not go.
func TestABrokenIntermediateIsAFailedStart(t *testing.T) {
	prev := ReparentWait
	ReparentWait = 300 * time.Millisecond
	t.Cleanup(func() { ReparentWait = prev })
	for _, tc := range []struct {
		name, script, why string
		// mustKill: the intermediate never goes of itself. Any other may be
		// killed or not, as its own exit and the kill fall.
		mustKill bool
	}{
		{"no report", "exit 0", "it exited without a report", false},
		{"not a report", "echo nonsense >&4", `"nonsense" is not a report`, false},
		{"pid 1", "echo '1 4242' >&4", `"1 4242" is not a report`, false},
		{"no start time", `echo "$$ 0" >&4`, "is not a report", false},
		{"no end", "printf '4242 4242' >&4", "no end", false},
		{"never", "exec sleep 30", "no report within", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command("/bin/sh", "-c", tc.script)
			done := make(chan struct{})
			var (
				child *Child
				r     *os.File
				err   error
			)
			go func() {
				child, r, err = StartReparented(context.Background(), cmd, HostChildEnv, "", "", ReparentSpawn{})
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(step):
				t.Fatalf("StartReparented has not returned after %v", step)
			}
			if err == nil || child != nil || r != nil || !strings.Contains(err.Error(), tc.why) {
				t.Fatalf("StartReparented: %v, %v, %v; want an error naming %q", child, r, err, tc.why)
			}
			if cmd.ProcessState == nil {
				t.Fatal("the intermediate was not reaped")
			}
			ws := cmd.ProcessState.Sys().(syscall.WaitStatus)
			if killed := ws.Signaled() && ws.Signal() == syscall.SIGKILL; tc.mustKill && !killed {
				t.Fatalf("the intermediate's end: %v; want it killed", cmd.ProcessState)
			}
		})
	}
}

// sleeper is a process this test started — `sleep 30` by default, leading a
// group of its own — its start time, and its end once this test, its parent,
// has reaped it.
type sleeper struct {
	pid   int
	start uint64
	done  chan struct{}
	err   error
}

// newSleeper starts a sleeper running argv (sleep 30 when none), reaped and
// ended when the test ends.
func newSleeper(t *testing.T, argv ...string) *sleeper {
	t.Helper()
	if len(argv) == 0 {
		argv = []string{"sleep", "30"}
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	id, err := rundir.ProcessIdentity(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	s := &sleeper{pid: cmd.Process.Pid, start: id.Start, done: make(chan struct{})}
	go func() { s.err = cmd.Wait(); close(s.done) }()
	t.Cleanup(func() {
		if s.running() {
			_ = syscall.Kill(-s.pid, syscall.SIGKILL)
		}
		<-s.done
	})
	return s
}

// ended is s's end, within step.
func (s *sleeper) ended(t *testing.T) error {
	t.Helper()
	select {
	case <-s.done:
		return s.err
	case <-time.After(step):
		t.Fatalf("process %d did not end within %v", s.pid, step)
		return nil
	}
}

// running reports whether s has not ended.
func (s *sleeper) running() bool {
	select {
	case <-s.done:
		return false
	default:
		return true
	}
}

// untouched fails the test unless s is running and was sent no fatal signal.
// On Linux that is certain, not a matter of time: kill(2) leaves SIGTERM —
// and, the target having no handler, the SIGKILL it becomes — pending before
// it returns, and a process that acted on it is a zombie this test has not
// reaped, or one it has; so a process still listed, not a zombie, with
// neither pending (/proc/<pid>/status) was signalled by nobody. Elsewhere it
// is s still running.
func (s *sleeper) untouched(t *testing.T, what string) {
	t.Helper()
	if !s.running() {
		t.Fatalf("%s was signalled: %v", what, s.err)
	}
	if runtime.GOOS != "linux" {
		return
	}
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", s.pid))
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	fatal := uint64(1)<<(syscall.SIGTERM-1) | uint64(1)<<(syscall.SIGKILL-1)
	for _, line := range strings.Split(string(b), "\n") {
		k, v, _ := strings.Cut(line, ":")
		v = strings.TrimSpace(v)
		switch k {
		case "State":
			if strings.HasPrefix(v, "Z") || strings.HasPrefix(v, "X") {
				t.Fatalf("%s has exited: %s", what, v)
			}
		case "SigPnd", "ShdPnd":
			mask, err := strconv.ParseUint(v, 16, 64)
			if err != nil || mask&fatal != 0 {
				t.Fatalf("%s has a fatal signal pending: %s %s (%v)", what, k, v, err)
			}
		}
	}
}

// TestAReparentedChildActsOnlyOnItsHost: a reparented Child tells its host by
// pid and start time, as an agent's record does (AgentGroup.stale). A process
// with the host's pid and another start time — the pid reused — is the host
// gone: the Child is exited at once, and terminate sends it nothing — it is
// still running, no signal pending, once terminate's real target, another
// process, has been signalled and reaped (C6 r1: no timing in the check).
// With the host's own start time it is signalled (terminate ends it), and
// Kill — its group, SIGKILL — returns once it has been reaped (here by this
// test, its parent), Done closed.
func TestAReparentedChildActsOnlyOnItsHost(t *testing.T) {
	a, b := newSleeper(t), newSleeper(t)
	stranger := &Child{pid: a.pid, start: a.start + 1, done: make(chan struct{})}
	stranger.terminate()
	if !stranger.Exited() {
		t.Fatal("a process with the host's pid and another start time is taken for the host")
	}
	a.untouched(t, "a process with the host's pid and another start time")
	host := &Child{pid: b.pid, start: b.start, done: make(chan struct{})}
	if host.Exited() {
		t.Fatal("the running host is taken for gone")
	}
	host.terminate()
	if err := b.ended(t); err == nil || !strings.Contains(err.Error(), "terminated") {
		t.Fatalf("the host after terminate: %v", err)
	}
	if !host.WaitExit(step) {
		t.Fatal("the host's Child never saw it go")
	}
	a.untouched(t, "a process with the host's pid and another start time, after the host's own end")

	s := newSleeper(t)
	host = &Child{pid: s.pid, start: s.start, done: make(chan struct{})}
	killed := make(chan struct{})
	go func() { host.Kill(); close(killed) }()
	if err := s.ended(t); err == nil || !strings.Contains(err.Error(), "killed") {
		t.Fatalf("the host after Kill: %v", err)
	}
	select {
	case <-killed:
	case <-time.After(step):
		t.Fatalf("Kill has not returned within %v of the host's reaping", step)
	}
	select {
	case <-host.Done():
	default:
		t.Fatal("Done is open after Kill")
	}
}

// TestAZombieHostIsGone (C6 r1, 3): a host that has exited and that its new
// parent never reaps — a container's first process, a slow subreaper — is a
// zombie with its own start time still on it. It is gone: Exited at once,
// Settle returns without waiting out its grace, and Kill without waiting for
// a reaping that never comes. The zombie here is a child of this test's that
// the test leaves unreaped until the end — the state a reparented host is in
// under a parent that does not reap. Linux only: the state is read from /proc
// there; macOS's SZOMB is the same identity read (rundir), not run here.
func TestAZombieHostIsGone(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the zombie is checked through /proc; macOS reads kinfo_proc's p_stat the same way, not exercised here")
	}
	prev := TermGrace
	TermGrace = 2 * step
	t.Cleanup(func() { TermGrace = prev })
	cmd := exec.Command("true")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Wait() })
	var id rundir.ProcIdentity
	for born := time.Now(); !id.Zombie; time.Sleep(5 * time.Millisecond) {
		var err error
		if id, err = rundir.ProcessIdentity(cmd.Process.Pid); err != nil {
			t.Fatal(err)
		}
		if time.Since(born) > step {
			t.Fatalf("process %d never became a zombie", cmd.Process.Pid)
		}
	}
	zombie := func() *Child { return &Child{pid: cmd.Process.Pid, start: id.Start, done: make(chan struct{})} }
	if !zombie().Exited() {
		t.Fatal("a zombie host is taken for running")
	}
	done := make(chan struct{})
	go func() {
		zombie().Settle()
		zombie().Kill()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(step):
		t.Fatalf("Settle and Kill of a zombie host have not returned within %v (its grace is %v)", step, TermGrace)
	}
}

// TestSignalHostIsBoundToTheProcess (C6 r1, 4): on Linux a reparented host
// is signalled through a pidfd, opened before its identity is checked —
// bound: a process with the host's pid and another start time is sent
// nothing, still running with nothing pending; the host is sent SIGTERM and
// ends; a host gone and reaped is sent nothing; and its group's SIGKILL
// reaches every member, a child left in the group by its leader included
// (through the pidfd on a kernel with PIDFD_SIGNAL_PROCESS_GROUP, by the
// group's id before one: logged). Elsewhere the check-then-kill fallback
// says it is not bound.
func TestSignalHostIsBoundToTheProcess(t *testing.T) {
	linux := runtime.GOOS == "linux"
	a := newSleeper(t)
	if sent, bound := signalHost(a.pid, a.start+1, syscall.SIGTERM, false); sent || bound != linux {
		t.Fatalf("a process with another start time: sent %v, bound %v", sent, bound)
	}
	a.untouched(t, "a process with the host's pid and another start time")
	if sent, bound := signalHost(a.pid, a.start, syscall.SIGTERM, false); !sent || bound != linux {
		t.Fatalf("the host: sent %v, bound %v; want sent, and bound on Linux", sent, bound)
	}
	if err := a.ended(t); err == nil || !strings.Contains(err.Error(), "terminated") {
		t.Fatalf("the host after its SIGTERM: %v", err)
	}
	if sent, _ := signalHost(a.pid, a.start, syscall.SIGTERM, false); sent {
		t.Fatal("a host gone and reaped was signalled")
	}

	member := filepath.Join(t.TempDir(), "member")
	leader := newSleeper(t, "/bin/sh", "-c", `sleep 30 & echo $! > "$0.tmp" && mv "$0.tmp" "$0"; wait`, member)
	var pid int
	for born := time.Now(); pid == 0; time.Sleep(5 * time.Millisecond) {
		if b, err := os.ReadFile(member); err == nil {
			pid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		}
		if time.Since(born) > step {
			t.Fatal("the group's member never said its pid")
		}
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	sent, bound := signalHost(leader.pid, leader.start, syscall.SIGKILL, true)
	t.Logf("the group's SIGKILL: sent %v, through the pidfd %v", sent, bound)
	if !sent {
		t.Fatal("the host's group was not signalled")
	}
	if err := leader.ended(t); err == nil || !strings.Contains(err.Error(), "killed") {
		t.Fatalf("the group's leader: %v", err)
	}
	for born := time.Now(); !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH); time.Sleep(5 * time.Millisecond) {
		if time.Since(born) > step {
			t.Fatalf("the group's member %d outlived the group's SIGKILL", pid)
		}
	}
}

// TestAnUnreportableHostIsEndedByItsIntermediate (C6 r1, 1 and r2, 1, the
// intermediate's side): an intermediate whose report cannot be written — its
// spawner gone, the report pipe's read end closed — after its host is up and
// has written its line exits 1, and ends that host, its whole group, first:
// the host's pid, from its line, is no one's once the intermediate has exited
// (it reaped its host). Unless the spawn is shared (ReparentSharedEnv=1: a
// reload's): that host is never signalled, and still runs once its
// intermediate has gone. The intermediate is held (holdEnv) until the host
// has written its line, so the report fails with the host up.
func TestAnUnreportableHostIsEndedByItsIntermediate(t *testing.T) {
	for _, shared := range []bool{false, true} {
		t.Run(map[bool]string{false: "own", true: "shared"}[shared], func(t *testing.T) {
			unreportableHost(t, shared)
		})
	}
}

// unreportableHost is TestAnUnreportableHostIsEndedByItsIntermediate for one
// kind of spawn.
func unreportableHost(t *testing.T, shared bool) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	readyR, readyW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	reportR, reportW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = readyR.Close() }()
	// The spawner has gone: nothing reads the report.
	_ = reportR.Close()
	cmd := exec.Command(exe, "-test.run=^$")
	cmd.Env = append(withoutEnv(os.Environ(), ReadyFDEnv), ReparentEnv+"=4", ReadyFDEnv+"=3", holdEnv+"="+t.TempDir(),
		fakeHostEnv+"=1", fakeHostParent+"="+strconv.Itoa(os.Getpid()))
	if shared {
		cmd.Env = append(cmd.Env, ReparentSharedEnv+"=1")
	}
	cmd.ExtraFiles = []*os.File{readyW, reportW}
	// A file, not a pipe: the host inherits the intermediate's stdio, and
	// Wait would otherwise wait for every holder of a pipe to close it.
	out, err := os.CreateTemp(t.TempDir(), "out")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = out.Close() }()
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	_ = readyW.Close()
	_ = reportW.Close()
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	line, err := readLine(t, readyR)
	var self hostSelf
	if err != nil || json.Unmarshal([]byte(line), &self) != nil || self.PID <= 1 {
		t.Fatalf("the host's line: %q, %v", line, err)
	}
	t.Cleanup(func() {
		if _, err := rundir.ProcessIdentity(self.PID); err == nil {
			_ = syscall.Kill(self.PID, syscall.SIGKILL)
		}
	})
	select {
	case err = <-waited:
	case <-time.After(step):
		t.Fatalf("the intermediate has not exited within %v", step)
	}
	said, _ := os.ReadFile(out.Name())
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 1 || !strings.Contains(string(said), "the session host's report") {
		t.Fatalf("the intermediate: %v, %q; want exit 1 naming the report", err, said)
	}
	id, err := rundir.ProcessIdentity(self.PID)
	if !shared {
		if !errors.Is(err, rundir.ErrNoProcess) {
			t.Fatalf("the unreported host %d after its intermediate exited: %+v, %v", self.PID, id, err)
		}
		return
	}
	if err != nil || id.Zombie || id.PPID == cmd.Process.Pid {
		t.Fatalf("the shared spawn's unreported host %d after its intermediate exited: %+v, %v; want it running", self.PID, id, err)
	}
}

// setEntry rewrites host id's registry entry in env to name pid, with
// pidStart as the start the host recorded of itself (none at all for 0) and
// startedAt — as an entry a crashed host left reads once its pid is another
// process's — by rename, so no scan reads half of it.
func setEntry(t *testing.T, env rundir.Env, id string, pid int, pidStart uint64, startedAt time.Time) {
	t.Helper()
	path := filepath.Join(env.Home, ".cache", "craze", "hosts", id+".json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var e map[string]any
	if err := json.Unmarshal(b, &e); err != nil {
		t.Fatal(err)
	}
	e["pid"], e["startedAt"] = pid, startedAt.UTC().Format(time.RFC3339Nano)
	delete(e, "pidStart")
	if pidStart != 0 {
		e["pidStart"] = pidStart
	}
	if b, err = json.Marshal(e); err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(filepath.Dir(filepath.Dir(path)), id+".json.tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

// TestARegistryEntryNamingAnotherProcessIsNeverSignalled (C6 r2, 2; r3): a
// host found through the registry is ended only when the process its entry
// names started exactly when the host recorded of itself (pidStart) — no
// clock, no tolerance. The entry here is live (its lock held), as a crashed
// host's entry reads while a sweeper holds its lock, and names a running
// process:
//
//   - whose start differs from the one recorded — the host's pid given to
//     another process, here one that started moments after the entry's
//     startedAt, inside any tolerance a clock could allow: not taken for the
//     host, why says so, and the process runs on, nothing pending;
//   - whose start is the one recorded, the entry's startedAt years off
//     either way: the host, returned, and its Terminate ends it;
//   - and an entry that records no start at all: never taken for anyone's,
//     however the process with its pid began.
func TestARegistryEntryNamingAnotherProcessIsNeverSignalled(t *testing.T) {
	home := t.TempDir()
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	runtimeDir, err := os.MkdirTemp("/tmp", "czs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runtimeDir) })
	env := rundir.Env{Home: home, CrazeDir: filepath.Join(home, ".craze"), CrazeRuntimeDir: runtimeDir, EUID: os.Geteuid()}
	const id = "0123456789ab"
	h, err := rundir.Bind(env, id, rundir.Entry{StartedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	rs := ReparentSpawn{Env: env, HostID: id}

	startedAt := time.Now()
	q := newSleeper(t)
	setEntry(t, env, id, q.pid, q.start+1, startedAt)
	c, why := unreported(rs, "", "")
	if c != nil {
		c.Terminate()
		t.Fatalf("an entry naming a process whose start is not the host's is taken for the host: pid %d", c.PID())
	}
	if !strings.Contains(why, "is another process now") {
		t.Fatalf("why it was not taken: %q", why)
	}
	q.untouched(t, "a process with the host's pid and another start, started within a second of the entry's startedAt")

	r := newSleeper(t)
	setEntry(t, env, id, r.pid, 0, startedAt)
	if c, why = unreported(rs, "", ""); c != nil {
		c.Terminate()
		t.Fatalf("an entry that records no start is taken for the host: pid %d", c.PID())
	}
	if !strings.Contains(why, "records no start") {
		t.Fatalf("why an entry with no start was not taken: %q", why)
	}
	r.untouched(t, "the process an entry with no recorded start names")

	for _, at := range []time.Time{time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), time.Now().AddDate(10, 0, 0)} {
		setEntry(t, env, id, q.pid, q.start, at)
		if c, why = unreported(rs, "", ""); c == nil || c.PID() != q.pid || why != "" {
			t.Fatalf("an entry naming its host (startedAt %v): %v, %q; want it taken for the host", at, c, why)
		}
	}
	c.Terminate()
	if err := q.ended(t); err == nil || !strings.Contains(err.Error(), "terminated") {
		t.Fatalf("the host after Terminate: %v", err)
	}
}

// TestACancelledStartIsAbandoned (C6 r1, 2): a context that ends while the
// intermediate has not reported ends StartReparented's wait at once, however
// long ReparentWait is: an error that is the context's, no Child, no ready
// pipe, the intermediate killed and reaped.
func TestACancelledStartIsAbandoned(t *testing.T) {
	prev := ReparentWait
	ReparentWait = 4 * step
	t.Cleanup(func() { ReparentWait = prev })
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.Command("/bin/sh", "-c", "exec sleep 300")
	type result struct {
		child *Child
		r     *os.File
		err   error
	}
	got := make(chan result, 1)
	go func() {
		child, r, err := StartReparented(ctx, cmd, HostChildEnv, "", "", ReparentSpawn{})
		got <- result{child, r, err}
	}()
	cancel()
	var res result
	select {
	case res = <-got:
	case <-time.After(step):
		t.Fatalf("StartReparented has not returned within %v of its context's end (ReparentWait %v)", step, ReparentWait)
	}
	if res.child != nil || res.r != nil || !errors.Is(res.err, context.Canceled) {
		t.Fatalf("StartReparented: %v, %v, %v; want the context's error", res.child, res.r, res.err)
	}
	if cmd.ProcessState == nil {
		t.Fatal("the intermediate was not reaped")
	}
}
