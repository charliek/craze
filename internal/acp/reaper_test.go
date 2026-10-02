package acp

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// The reaper's tests (plan 032 §3.2a, A4; SF-80). The agents are /bin/sh
// scripts: what a test needs of one — a leader or a tool that ignores
// SIGTERM, an exit status, a tool left behind — is a line of shell.

// spawnShell spawns /bin/sh -c script as the agent, its stderr into stderr
// (io.Discard when nil), and closes it when the test ends.
func spawnShell(t *testing.T, script string, stderr io.Writer) *Client {
	t.Helper()
	if stderr == nil {
		stderr = io.Discard
	}
	c, err := Spawn(SpawnOptions{Binary: "/bin/sh", Args: []string{"-c", script}, Stderr: stderr})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// syncBuffer is a stderr sink a test reads while the copy writes it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// waitFor waits for want to have been written.
func (s *syncBuffer) waitFor(t *testing.T, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(s.String(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("the agent never wrote %q to its stderr (got %q)", want, s.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitClosed waits up to d for ch to be closed.
func waitClosed(t *testing.T, ch <-chan struct{}, d time.Duration, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(d):
		t.Fatalf("%s did not happen within %v", what, d)
	}
}

// shutdownWithin runs Shutdown and fails the test unless it returns within d:
// how long it took.
func shutdownWithin(t *testing.T, ch *Child, d time.Duration) time.Duration {
	t.Helper()
	start := time.Now()
	done := make(chan struct{})
	go func() {
		ch.Shutdown()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(d):
		// Unwedge before failing, or the cleanup's Close waits on it too.
		_ = syscall.Kill(-ch.pgid, syscall.SIGKILL)
		t.Fatalf("Shutdown did not return within %v", d)
	}
	return time.Since(start)
}

// readPID reads the pid a script wrote to path, waiting for it to be there.
func readPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		raw, err := os.ReadFile(path)
		if err == nil && strings.HasSuffix(string(raw), "\n") {
			pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
			return pid
		}
		if time.Now().After(deadline) {
			t.Fatalf("no pid in %s", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// gone reports whether no process has pid: it exited and was reaped.
func gone(pid int) bool { return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) }

// waitGone waits up to d for pid to be gone. A tool left behind is reaped by
// whoever adopted it, which takes a moment after it dies.
func waitGone(t *testing.T, pid int, d time.Duration, what string) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !gone(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("%s (pid %d) is still running %v on", what, pid, d)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// checkExit checks the reaped child's exit: observed as want (an
// *ExitError's message and code; "" for an exit 0, observed as nil), and
// cmd.Wait's answer at the reap saying the same.
func checkExit(t *testing.T, ch *Child, want string, code int) {
	t.Helper()
	waitClosed(t, ch.reapedCh, 10*time.Second, "the reap")
	if want == "" {
		if ch.exitErr != nil || ch.waitErr != nil {
			t.Fatalf("observed %v, reaped %v; want an exit 0 both times", ch.exitErr, ch.waitErr)
		}
		return
	}
	var ee *ExitError
	if !errors.As(ch.exitErr, &ee) {
		t.Fatalf("observed %v (%T), want an *ExitError", ch.exitErr, ch.exitErr)
	}
	if ee.Error() != want || ee.ExitCode() != code {
		t.Fatalf("observed %q, code %d; want %q, code %d", ee.Error(), ee.ExitCode(), want, code)
	}
	var we *exec.ExitError
	if !errors.As(ch.waitErr, &we) || we.Error() != want || we.ExitCode() != code {
		t.Fatalf("cmd.Wait at the reap said %v, which does not agree with the observed %q", ch.waitErr, want)
	}
}

// groupSignal is one group signal the reaper sent, as recordGroupSignals saw
// it.
type groupSignal struct {
	sig syscall.Signal
	// pinned: kill(pgid, 0) found the agent — the group's leader, running or
	// a zombie, but not reaped — just before the signal went.
	pinned bool
}

type signalRecorder struct {
	mu   sync.Mutex
	sent map[int][]groupSignal // by process group
}

// recordGroupSignals puts a stub over killGroup for the rest of the test: it
// notes every group signal, and whether the agent was still unreaped when it
// went, then sends it.
func recordGroupSignals(t *testing.T) *signalRecorder {
	r := &signalRecorder{sent: make(map[int][]groupSignal)}
	real := killGroup
	killGroup = func(pgid int, sig syscall.Signal) error {
		pinned := syscall.Kill(pgid, 0) == nil
		r.mu.Lock()
		r.sent[pgid] = append(r.sent[pgid], groupSignal{sig: sig, pinned: pinned})
		r.mu.Unlock()
		return real(pgid, sig)
	}
	// Registered before any Spawn, so it runs after every Close.
	t.Cleanup(func() { killGroup = real })
	return r
}

func (r *signalRecorder) of(pgid int) []groupSignal {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]groupSignal(nil), r.sent[pgid]...)
}

// reapedSignals waits for ch's reap and takes its group signals, as r saw
// them, then.
type reapedSignals struct {
	ch     *Child
	atReap []groupSignal
}

func signalsAtTheReap(t *testing.T, r *signalRecorder, ch *Child) reapedSignals {
	t.Helper()
	waitClosed(t, ch.reapedCh, 10*time.Second, "the reap")
	return reapedSignals{ch: ch, atReap: r.of(ch.pgid)}
}

// checkSignalsPrecedeTheReap checks reaped children's group signals, as r
// saw them: every one sent while the agent was unreaped, and none since the
// reap — the count taken at the reap and again now, after a pause in which a
// stray signal would have shown up. It returns the last child's.
func checkSignalsPrecedeTheReap(t *testing.T, r *signalRecorder, reaped ...reapedSignals) []groupSignal {
	t.Helper()
	time.Sleep(200 * time.Millisecond)
	var later []groupSignal
	for _, rs := range reaped {
		later = r.of(rs.ch.pgid)
		for i, s := range later {
			if !s.pinned {
				t.Fatalf("signal %d of %v (%v) went after the agent was reaped", i, later, s.sig)
			}
		}
		if len(later) != len(rs.atReap) {
			t.Fatalf("signals %v after the reap (%v before it)", later[len(rs.atReap):], rs.atReap)
		}
	}
	return later
}

// TestShutdownKillsALeaderThatIgnoresSIGTERM (A4): the reaper owns the whole
// escalation, so an agent that ignores SIGTERM is killed at the grace and
// Shutdown returns then — not when the agent decides to exit.
func TestShutdownKillsALeaderThatIgnoresSIGTERM(t *testing.T) {
	var stderr syncBuffer
	c := spawnShell(t, `trap '' TERM; echo ready >&2; exec sleep 60`, &stderr)
	stderr.waitFor(t, "ready")
	took := shutdownWithin(t, c.child, shutdownGrace+10*time.Second)
	if took < shutdownGrace {
		t.Fatalf("Shutdown returned after %v, inside the %v grace an agent that ignores SIGTERM has", took, shutdownGrace)
	}
	if took > shutdownGrace+3*time.Second {
		t.Fatalf("Shutdown took %v, not about the %v grace", took, shutdownGrace)
	}
	checkExit(t, c.child, "signal: killed", -1)
	if !gone(c.PID()) {
		t.Fatal("the agent was left a zombie")
	}
}

// TestWhatTheAgentLeftBehindDiesAtItsExit (A4): a tool the agent started is
// in its process group, and when the agent exits on its own the reaper ends
// it at once — no Shutdown, no Close — SIGKILL included, past the grace, for
// one that ignores SIGTERM.
func TestWhatTheAgentLeftBehindDiesAtItsExit(t *testing.T) {
	for _, tc := range []struct{ name, trap string }{
		{"a tool", ""},
		{"a tool that ignores SIGTERM", "trap '' TERM; "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pidFile := filepath.Join(t.TempDir(), "tool.pid")
			c := spawnShell(t, tc.trap+"sleep 60 & echo $! > "+pidFile+"; exit 0", nil)
			waitClosed(t, c.child.exitedCh, 10*time.Second, "the agent's exit")
			tool := readPID(t, pidFile)
			waitGone(t, tool, 2*shutdownGrace+10*time.Second, "the tool the agent left behind")
			waitClosed(t, c.child.reapedCh, 10*time.Second, "the reap")
			checkExit(t, c.child, "", 0)
			if err := c.Close(); !errors.Is(err, ErrAgentExited) {
				t.Fatalf("Close = %v, want ErrAgentExited", err)
			}
		})
	}
}

// TestTheExitStatusIsObservedOnEveryPath (A4): the status the reaper records
// at the exit — before the reap — is the agent's, however it ended, and the
// reap's cmd.Wait says the same.
func TestTheExitStatusIsObservedOnEveryPath(t *testing.T) {
	t.Run("its own exit", func(t *testing.T) {
		c := spawnShell(t, "exit 3", nil)
		waitClosed(t, c.child.exitedCh, 10*time.Second, "the agent's exit")
		checkExit(t, c.child, "exit status 3", 3)
		err := c.Close()
		if !errors.Is(err, ErrAgentExited) || !strings.HasSuffix(err.Error(), ": exit status 3") {
			t.Fatalf("Close = %v, want ErrAgentExited with exit status 3", err)
		}
	})
	for _, tc := range []struct {
		name, script, want string
		code               int
	}{
		{"SIGTERM, which it handles", `trap 'exit 5' TERM; echo ready >&2; while :; do sleep 1; done`, "exit status 5", 5},
		{"SIGTERM", `echo ready >&2; exec sleep 60`, "signal: terminated", -1},
		{"SIGKILL, at the grace", `trap '' TERM; echo ready >&2; exec sleep 60`, "signal: killed", -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stderr syncBuffer
			c := spawnShell(t, tc.script, &stderr)
			stderr.waitFor(t, "ready")
			closed := make(chan error, 1)
			go func() { closed <- c.Close() }()
			select {
			case err := <-closed:
				if err != nil {
					t.Fatalf("Close = %v, want nil: craze ended the agent", err)
				}
			case <-time.After(shutdownGrace + 10*time.Second):
				_ = syscall.Kill(-c.child.pgid, syscall.SIGKILL)
				t.Fatal("Close did not return")
			}
			checkExit(t, c.child, tc.want, tc.code)
		})
	}
}

// TestShutdownRacesTheAgentsOwnExit (A4): a Shutdown that comes as the agent
// exits on its own — before its exit, around it, after its reap — returns,
// leaves no zombie, records an exit the reap agrees with, and never has a
// group signal follow the reap.
func TestShutdownRacesTheAgentsOwnExit(t *testing.T) {
	r := recordGroupSignals(t)
	var reaped []reapedSignals
	outcomes := map[string]int{}
	for i := range 24 {
		c, err := Spawn(SpawnOptions{Binary: "/bin/sh", Args: []string{"-c", "exit 4"}, Stderr: io.Discard})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		switch i % 3 {
		case 1:
			<-c.child.exitedCh
		case 2:
			<-c.child.reapedCh
		}
		shutdownWithin(t, c.child, shutdownGrace+10*time.Second)
		// Either the exit was its own, or the SIGTERM came first.
		var ee *ExitError
		if !errors.As(c.child.exitErr, &ee) || (ee.Error() != "exit status 4" && ee.Error() != "signal: terminated") {
			t.Fatalf("iteration %d: observed %v, want exit status 4 or signal: terminated", i, c.child.exitErr)
		}
		checkExit(t, c.child, ee.Error(), ee.ExitCode())
		outcomes[ee.Error()]++
		reaped = append(reaped, signalsAtTheReap(t, r, c.child))
		if !gone(c.PID()) {
			t.Fatalf("iteration %d: the agent was left a zombie", i)
		}
	}
	checkSignalsPrecedeTheReap(t, r, reaped...)
	t.Logf("exits: %v", outcomes)
}

// TestTheAgentsStderrIsCopiedToTheEnd (A4, an inherited stderr): what the
// agent's process group writes to the stderr it inherited reaches the sink —
// a pipe-fed buffer or a file — up to the group's very end: here a tool's
// last line, written as the reaper's cleanup SIGTERMs it after the agent has
// exited. The reap, which closes craze's end of the pipe, comes after.
func TestTheAgentsStderrIsCopiedToTheEnd(t *testing.T) {
	script := func(dir string) string {
		return `(trap 'echo tool-last >&2; exit 0' TERM; : > ` + dir + `/trapped; while :; do sleep 1; done) & ` +
			`while [ ! -e ` + dir + `/trapped ]; do sleep 0.01; done; echo agent-last >&2; exit 0`
	}
	check := func(t *testing.T, got string) {
		t.Helper()
		if !strings.Contains(got, "agent-last\n") || !strings.Contains(got, "tool-last\n") {
			t.Fatalf("stderr %q, want the agent's last line and the tool's", got)
		}
	}
	t.Run("into a buffer", func(t *testing.T) {
		var stderr syncBuffer
		c := spawnShell(t, script(t.TempDir()), &stderr)
		waitClosed(t, c.child.exitedCh, 10*time.Second, "the agent's exit")
		_ = c.Close()
		check(t, stderr.String())
	})
	t.Run("into a file", func(t *testing.T) {
		f, err := os.Create(filepath.Join(t.TempDir(), "stderr"))
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		c := spawnShell(t, script(t.TempDir()), f)
		waitClosed(t, c.child.exitedCh, 10*time.Second, "the agent's exit")
		_ = c.Close()
		got, err := os.ReadFile(f.Name())
		if err != nil {
			t.Fatal(err)
		}
		check(t, string(got))
	})
}

// slowWriter is a stderr sink that takes its time over every write.
type slowWriter struct {
	syncBuffer
	delay time.Duration
}

func (w *slowWriter) Write(p []byte) (int, error) {
	time.Sleep(w.delay)
	return w.syncBuffer.Write(p)
}

// TestTheAgentsStderrTailOutlivesTheReap (A4, an inherited stderr): what the
// agent wrote to its stderr just before it exited — a pipe's worth, into a
// sink slow to take it — is all copied, though the reap closes craze's end of
// the pipe: the reaper lets the copy reach the pipe's end first.
func TestTheAgentsStderrTailOutlivesTheReap(t *testing.T) {
	w := &slowWriter{delay: 30 * time.Millisecond}
	c := spawnShell(t, `dd if=/dev/zero bs=1024 count=64 2>/dev/null | tr '\0' x >&2; echo agent-last >&2; exit 0`, w)
	waitClosed(t, c.child.reapedCh, 10*time.Second, "the reap")
	c.child.Wait()
	got := w.String()
	if n := strings.Count(got, "x"); n != 64*1024 || !strings.HasSuffix(got, "agent-last\n") {
		t.Fatalf("stderr has %d of the agent's 65536 x's, ending %q; want them all, then its last line", n, got[max(0, len(got)-20):])
	}
}

// TestAnInterruptedObserveIsRetried (A4): the observation's system call
// interrupted (EINTR, through the test seam) is made again, and the exit is
// still observed — status included — without a reap: the reaper goes on to
// clean the group, which ends the tool the agent left behind.
func TestAnInterruptedObserveIsRetried(t *testing.T) {
	calls := stubObserve(t, 3, nil)
	pidFile := filepath.Join(t.TempDir(), "tool.pid")
	c := spawnShell(t, "sleep 60 & echo $! > "+pidFile+"; exit 6", nil)
	waitClosed(t, c.child.exitedCh, 10*time.Second, "the agent's exit")
	tool := readPID(t, pidFile)
	waitGone(t, tool, 2*shutdownGrace+10*time.Second, "the tool the agent left behind")
	checkExit(t, c.child, "exit status 6", 6)
	if n := calls.Load(); n < 4 {
		t.Fatalf("the observation's call was made %d times; want the 3 interrupted and one more", n)
	}
}

// TestPendingCallsFailWithTheObservedExit (A4): a call pending when the agent
// exits fails at the exit, with its status — while the reaper is still
// cleaning up the group, here a tool that ignores SIGTERM and holds the
// agent's stdout open, so the connection sees no EOF of its own.
func TestPendingCallsFailWithTheObservedExit(t *testing.T) {
	c := spawnShell(t, `trap '' TERM; sleep 60 & read line; exit 7`, nil)
	_, err := c.Initialize(t.Context())
	var ee *ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 7 || !strings.Contains(err.Error(), "acp: agent exited: exit status 7") {
		t.Fatalf("Initialize = %v, want it failed with the agent's exit status 7", err)
	}
	select {
	case <-c.child.reapedCh:
		t.Fatal("the pending call failed only at the reap, not at the exit")
	default:
	}
	checkExit(t, c.child, "exit status 7", 7)
}

// TestACleanAgentShutsDownWellUnderTheGrace (A4): an agent that exits on its
// SIGTERM and leaves nothing behind is shut down at once: its zombie alone in
// its group costs no signal and no wait.
func TestACleanAgentShutsDownWellUnderTheGrace(t *testing.T) {
	c := spawnScript(t, "echo")
	handshake(t, c)
	took := shutdownWithin(t, c.child, shutdownGrace+10*time.Second)
	if took >= shutdownGrace/2 {
		t.Fatalf("Shutdown of a clean agent took %v; want well under the %v grace", took, shutdownGrace)
	}
	if !gone(c.PID()) {
		t.Fatal("the agent was left a zombie")
	}
}

// TestNoZombieIsLeftAfterShutdown (A4): once Shutdown has returned the agent
// has been reaped — kill(pid, 0) finds nothing — whether it was running or
// had exited on its own.
func TestNoZombieIsLeftAfterShutdown(t *testing.T) {
	t.Run("running", func(t *testing.T) {
		c := spawnScript(t, "echo")
		handshake(t, c)
		shutdownWithin(t, c.child, shutdownGrace+10*time.Second)
		if err := syscall.Kill(c.PID(), 0); !errors.Is(err, syscall.ESRCH) {
			t.Fatalf("kill(pid, 0) after Shutdown = %v, want ESRCH", err)
		}
	})
	t.Run("exited on its own", func(t *testing.T) {
		c := spawnShell(t, "exit 0", nil)
		waitClosed(t, c.child.exitedCh, 10*time.Second, "the agent's exit")
		shutdownWithin(t, c.child, shutdownGrace+10*time.Second)
		if err := syscall.Kill(c.PID(), 0); !errors.Is(err, syscall.ESRCH) {
			t.Fatalf("kill(pid, 0) after Shutdown = %v, want ESRCH", err)
		}
	})
}

// TestEveryGroupSignalPrecedesTheReap (A4): a stub over the group-signal
// function records every signal the reaper sends, and whether the agent was
// still unreaped — so its pid, the group's id, still taken — when it went:
// every signal precedes the reap, and none follows it. Both ways in: a
// Shutdown that escalates against a running agent and then cleans up a tool
// that ignores SIGTERM, and an agent's own exit leaving that tool.
func TestEveryGroupSignalPrecedesTheReap(t *testing.T) {
	r := recordGroupSignals(t)
	sigs := func(sent []groupSignal) string {
		var b strings.Builder
		for _, s := range sent {
			fmt.Fprintf(&b, "%d ", s.sig)
		}
		return b.String()
	}
	t.Run("a shutdown", func(t *testing.T) {
		var stderr syncBuffer
		pidFile := filepath.Join(t.TempDir(), "tool.pid")
		c := spawnShell(t, `trap '' TERM; sleep 60 & echo $! > `+pidFile+`; trap - TERM; echo ready >&2; exec sleep 60`, &stderr)
		stderr.waitFor(t, "ready")
		tool := readPID(t, pidFile)
		shutdownWithin(t, c.child, 2*shutdownGrace+10*time.Second)
		sent := checkSignalsPrecedeTheReap(t, r, signalsAtTheReap(t, r, c.child))
		// The escalation's SIGTERM, then the cleanup's SIGTERM and SIGKILL.
		want := fmt.Sprintf("%d %d %d ", syscall.SIGTERM, syscall.SIGTERM, syscall.SIGKILL)
		if got := sigs(sent); got != want {
			t.Fatalf("group signals %q, want %q", got, want)
		}
		waitGone(t, tool, 10*time.Second, "the tool")
	})
	t.Run("its own exit", func(t *testing.T) {
		pidFile := filepath.Join(t.TempDir(), "tool.pid")
		c := spawnShell(t, `trap '' TERM; sleep 60 & echo $! > `+pidFile+`; exit 0`, nil)
		tool := readPID(t, pidFile)
		sent := checkSignalsPrecedeTheReap(t, r, signalsAtTheReap(t, r, c.child))
		want := fmt.Sprintf("%d %d ", syscall.SIGTERM, syscall.SIGKILL)
		if got := sigs(sent); got != want {
			t.Fatalf("group signals %q, want %q", got, want)
		}
		waitGone(t, tool, 10*time.Second, "the tool")
	})
}

// TestAnUnobservableExitIsReapedWithoutAGroupSignal: where the exit cannot be
// observed without a reap — the observation refused (ENOSYS, as under a
// seccomp filter) — the reap is the observation, and no group signal is ever
// sent, since nothing pins the group's id: a Shutdown signals the agent
// alone, and still ends it.
func TestAnUnobservableExitIsReapedWithoutAGroupSignal(t *testing.T) {
	stubObserve(t, 0, syscall.ENOSYS)
	r := recordGroupSignals(t)
	t.Run("a shutdown", func(t *testing.T) {
		var stderr syncBuffer
		c := spawnShell(t, `echo ready >&2; exec sleep 60`, &stderr)
		stderr.waitFor(t, "ready")
		shutdownWithin(t, c.child, shutdownGrace+10*time.Second)
		var we *exec.ExitError
		if !errors.As(c.child.exitErr, &we) || we.Error() != "signal: terminated" || c.child.exitErr != c.child.waitErr {
			t.Fatalf("exit %v, reap %v; want the reap's signal: terminated as both", c.child.exitErr, c.child.waitErr)
		}
		if sent := r.of(c.child.pgid); len(sent) != 0 {
			t.Fatalf("group signals %v with nothing pinning the group's id", sent)
		}
		if !gone(c.PID()) {
			t.Fatal("the agent was left a zombie")
		}
	})
	t.Run("its own exit", func(t *testing.T) {
		c := spawnShell(t, "exit 3", nil)
		waitClosed(t, c.child.reapedCh, 10*time.Second, "the reap")
		if err := c.Close(); !errors.Is(err, ErrAgentExited) || !strings.HasSuffix(err.Error(), ": exit status 3") {
			t.Fatalf("Close = %v, want ErrAgentExited with exit status 3", err)
		}
		if sent := r.of(c.child.pgid); len(sent) != 0 {
			t.Fatalf("group signals %v with nothing pinning the group's id", sent)
		}
	})
}

// TestWatchExitLeavesTheProcessUnreaped: the platform's observation reports a
// process's exit status and leaves it a zombie — so it can be observed again
// (on macOS through the zombie's own record, the exit watch refusing a
// process already gone) — and the reap that follows agrees.
func TestWatchExitLeavesTheProcessUnreaped(t *testing.T) {
	for _, tc := range []struct{ script, want string }{
		{"exit 9", "exit status 9"},
		{"kill -KILL $$", "signal: killed"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			cmd := exec.Command("/bin/sh", "-c", tc.script)
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			pid := cmd.Process.Pid
			first, err := watchExit(pid)()
			if err != nil {
				_ = cmd.Wait()
				t.Fatal(err)
			}
			again, err := watchExit(pid)()
			if err != nil {
				_ = cmd.Wait()
				t.Fatalf("the second observation: %v (was the first a reap?)", err)
			}
			_ = cmd.Wait()
			reaped := cmd.ProcessState.Sys().(syscall.WaitStatus)
			if first != reaped || again != reaped {
				t.Fatalf("observed %#x, then %#x; the reap says %#x", first, again, reaped)
			}
			if got := (&ExitError{Status: first}).Error(); got != tc.want || got != cmd.ProcessState.String() {
				t.Fatalf("observed %q, reaped %q; want %q", got, cmd.ProcessState.String(), tc.want)
			}
		})
	}
}

// TestParseStatGroup: a /proc/<pid>/stat line's state and process group, its
// command's parentheses and spaces notwithstanding.
func TestParseStatGroup(t *testing.T) {
	for _, tc := range []struct {
		line  string
		state byte
		pgrp  int
		ok    bool
	}{
		{"4242 (sleep) S 4241 4240 4240 0 -1 4194304 92 0 0 0", 'S', 4240, true},
		{"4242 (sh) Z 1 4242 4242 0", 'Z', 4242, true},
		{"77 (a) b) (c) R 1 99 99", 'R', 99, true},
		{"77 (tool name) D 2 123", 'D', 123, true},
		{"77 (sleep) S 1", 0, 0, false},
		{"77 sleep S 1 2 3", 0, 0, false},
		{"77 (sleep) SS 1 2", 0, 0, false},
		{"77 (sleep) S 1 x", 0, 0, false},
	} {
		state, pgrp, ok := parseStatGroup([]byte(tc.line))
		if state != tc.state || pgrp != tc.pgrp || ok != tc.ok {
			t.Errorf("parseStatGroup(%q) = %q, %d, %v; want %q, %d, %v", tc.line, state, pgrp, ok, tc.state, tc.pgrp, tc.ok)
		}
	}
}
