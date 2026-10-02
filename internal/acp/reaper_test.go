package acp

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// The reaper's tests (plan 032 §3.2a, A4; SF-80). The agents are /bin/sh
// scripts: what a test needs of one — a leader or a tool that ignores
// SIGTERM, an exit status, a tool left behind — is a line of shell. What a
// shell cannot do, this test binary does, run again as a helper.

// helperEnv names the helper this test binary is when it is run as one.
const helperEnv = "CRAZE_ACP_TEST_HELPER"

// helperGateEnv names a file the helper waits for, when set, before it does
// what it is for.
const helperGateEnv = "CRAZE_ACP_TEST_GATE"

func init() {
	if os.Getenv(helperEnv) == "exit-256" {
		awaitGate(os.Getenv(helperGateEnv))
		// exit(256): a status only its low byte of reaches a wait — and
		// macOS's zombie record saturates (reaper_darwin.go).
		os.Exit(256)
	}
}

// awaitGate waits, up to 30 s, for the file gate to exist; "" waits for
// nothing.
func awaitGate(gate string) {
	for deadline := time.Now().Add(30 * time.Second); gate != "" && time.Now().Before(deadline); {
		if _, err := os.Stat(gate); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// openGate creates the file gate, which a gated agent waits for.
func openGate(t *testing.T, gate string) {
	t.Helper()
	if err := os.WriteFile(gate, nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

// helperArgs is how a test runs this binary as a helper: its path, and a run
// pattern no test matches, should the helper ever get as far as the tests.
func helperArgs(t *testing.T) (string, []string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return exe, []string{"-test.run=^$"}
}

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

// lifeEvent is a signal the reaper sent, or a reap the fallback's wait4
// made, as recordLife saw it.
type lifeEvent struct {
	// target is a signal's: the agent's pid, or -pgid for its group. A reap
	// has none.
	target int
	sig    syscall.Signal
	reap   bool
	// pinned: a signal's, kill(agent, 0) found the agent — running or a
	// zombie, not reaped — just before the signal went.
	pinned bool
}

func (e lifeEvent) String() string {
	if e.reap {
		return "reap"
	}
	return fmt.Sprintf("%d→%d pinned=%v", e.sig, e.target, e.pinned)
}

type lifeRecorder struct {
	mu     sync.Mutex
	events map[int][]lifeEvent // by the agent's pid
}

// recordLife puts stubs over the reaper's signal and wait4 for the rest of
// the test: they note every signal — and whether the agent was still
// unreaped when it went — and every reap wait4 makes, in order, then do
// what they stand for.
func recordLife(t *testing.T) *lifeRecorder {
	r := &lifeRecorder{events: make(map[int][]lifeEvent)}
	realSignal, realWait4 := sendSignal, wait4
	sendSignal = func(pid int, sig syscall.Signal) error {
		agent := max(pid, -pid)
		pinned := syscall.Kill(agent, 0) == nil
		r.add(agent, lifeEvent{target: pid, sig: sig, pinned: pinned})
		return realSignal(pid, sig)
	}
	wait4 = func(pid int, ws *syscall.WaitStatus, options int) (int, error) {
		got, err := realWait4(pid, ws, options)
		if err == nil && got == pid {
			r.add(pid, lifeEvent{reap: true})
		}
		return got, err
	}
	// Registered before any Spawn, so it runs after every Close.
	t.Cleanup(func() { sendSignal, wait4 = realSignal, realWait4 })
	return r
}

func (r *lifeRecorder) add(agent int, e lifeEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events[agent] = append(r.events[agent], e)
}

func (r *lifeRecorder) of(agent int) []lifeEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]lifeEvent(nil), r.events[agent]...)
}

// groupSignals is the signals in events that went to the group, as a string
// of their numbers.
func groupSignals(events []lifeEvent) string {
	var b strings.Builder
	for _, e := range events {
		if !e.reap && e.target < 0 {
			fmt.Fprintf(&b, "%d ", e.sig)
		}
	}
	return b.String()
}

// reapedEvents is an agent's events, taken at its reap.
type reapedEvents struct {
	ch     *Child
	atReap []lifeEvent
}

func eventsAtTheReap(t *testing.T, r *lifeRecorder, ch *Child) reapedEvents {
	t.Helper()
	waitClosed(t, ch.reapedCh, 10*time.Second, "the reap")
	return reapedEvents{ch: ch, atReap: r.of(ch.pgid)}
}

// checkSignalsPrecedeTheReap checks reaped agents' events, as r saw them:
// every signal sent while the agent was unreaped, none after a reap wait4
// made, and none since the reap — the count taken at the reap and again now,
// after a pause in which a stray signal would have shown up. It returns the
// last agent's.
func checkSignalsPrecedeTheReap(t *testing.T, r *lifeRecorder, reaped ...reapedEvents) []lifeEvent {
	t.Helper()
	time.Sleep(200 * time.Millisecond)
	var later []lifeEvent
	for _, re := range reaped {
		later = r.of(re.ch.pgid)
		afterReap := false
		for i, e := range later {
			switch {
			case e.reap:
				afterReap = true
			case !e.pinned || afterReap:
				t.Fatalf("event %d of %v (%v) went after the agent was reaped", i, later, e)
			}
		}
		if len(later) != len(re.atReap) {
			t.Fatalf("events %v after the reap (%v before it)", later[len(re.atReap):], re.atReap)
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
// signal follow the reap. Both outcomes are required, each forced in rounds
// of its own: the agent's exit first (Shutdown after its exit, or after its
// reap), and the SIGTERM first (an agent that never exits by itself); the
// remaining rounds race the two for real.
func TestShutdownRacesTheAgentsOwnExit(t *testing.T) {
	r := recordLife(t)
	var reaped []reapedEvents
	outcomes := map[string]int{}
	for i := range 24 {
		script := "exit 4"
		if i%4 == 3 {
			script = "exec sleep 60"
		}
		c, err := Spawn(SpawnOptions{Binary: "/bin/sh", Args: []string{"-c", script}, Stderr: io.Discard})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		switch i % 4 {
		case 1:
			<-c.child.exitedCh
		case 2:
			<-c.child.reapedCh
		}
		shutdownWithin(t, c.child, shutdownGrace+10*time.Second)
		var ee *ExitError
		if !errors.As(c.child.exitErr, &ee) || (ee.Error() != "exit status 4" && ee.Error() != "signal: terminated") {
			t.Fatalf("round %d: observed %v, want exit status 4 or signal: terminated", i, c.child.exitErr)
		}
		checkExit(t, c.child, ee.Error(), ee.ExitCode())
		outcomes[ee.Error()]++
		reaped = append(reaped, eventsAtTheReap(t, r, c.child))
		if !gone(c.PID()) {
			t.Fatalf("round %d: the agent was left a zombie", i)
		}
	}
	checkSignalsPrecedeTheReap(t, r, reaped...)
	if outcomes["exit status 4"] == 0 || outcomes["signal: terminated"] == 0 {
		t.Fatalf("exits %v: want both the agent's own and the SIGTERM's", outcomes)
	}
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
	calls := stubObserve(t, 3, nil, nil)
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

// TestEveryGroupSignalPrecedesTheReap (A4): a stub over the signal function
// records every signal the reaper sends, and whether the agent was still
// unreaped — so its pid, the group's id, still taken — when it went: every
// signal precedes the reap, and none follows it. Three ways in: a Shutdown
// that escalates against a running agent and then cleans up a tool that
// ignores SIGTERM; an agent's own exit leaving that tool; and a clean exit,
// which costs the group nothing but the last SIGKILL.
func TestEveryGroupSignalPrecedesTheReap(t *testing.T) {
	r := recordLife(t)
	term, kill := int(syscall.SIGTERM), int(syscall.SIGKILL)
	t.Run("a shutdown", func(t *testing.T) {
		var stderr syncBuffer
		pidFile := filepath.Join(t.TempDir(), "tool.pid")
		c := spawnShell(t, `trap '' TERM; sleep 60 & echo $! > `+pidFile+`; trap - TERM; echo ready >&2; exec sleep 60`, &stderr)
		stderr.waitFor(t, "ready")
		tool := readPID(t, pidFile)
		shutdownWithin(t, c.child, 2*shutdownGrace+10*time.Second)
		sent := checkSignalsPrecedeTheReap(t, r, eventsAtTheReap(t, r, c.child))
		// The escalation's SIGTERM, the cleanup's SIGTERM and SIGKILL, the
		// last SIGKILL.
		want := fmt.Sprintf("%d %d %d %d ", term, term, kill, kill)
		if got := groupSignals(sent); got != want {
			t.Fatalf("group signals %q, want %q", got, want)
		}
		waitGone(t, tool, 10*time.Second, "the tool")
	})
	t.Run("its own exit", func(t *testing.T) {
		pidFile := filepath.Join(t.TempDir(), "tool.pid")
		c := spawnShell(t, `trap '' TERM; sleep 60 & echo $! > `+pidFile+`; exit 0`, nil)
		tool := readPID(t, pidFile)
		sent := checkSignalsPrecedeTheReap(t, r, eventsAtTheReap(t, r, c.child))
		want := fmt.Sprintf("%d %d %d ", term, kill, kill)
		if got := groupSignals(sent); got != want {
			t.Fatalf("group signals %q, want %q", got, want)
		}
		waitGone(t, tool, 10*time.Second, "the tool")
	})
	t.Run("a clean exit", func(t *testing.T) {
		c := spawnShell(t, `exit 0`, nil)
		sent := checkSignalsPrecedeTheReap(t, r, eventsAtTheReap(t, r, c.child))
		if got, want := groupSignals(sent), fmt.Sprintf("%d ", kill); got != want {
			t.Fatalf("group signals %q, want %q", got, want)
		}
	})
}

// stopAtFallback holds the reaper at its entry to the fallback, until the
// returned release is called; entered is closed as it gets there.
func stopAtFallback(t *testing.T) (entered <-chan struct{}, release func()) {
	in, out := make(chan struct{}), make(chan struct{})
	var once sync.Once
	fallbackEntered = func(int) {
		close(in)
		<-out
	}
	// Registered before any Spawn, so it runs after every Close.
	t.Cleanup(func() { fallbackEntered = nil })
	return in, func() { once.Do(func() { close(out) }) }
}

// checkFallback checks a reaped agent's events in the fallback: no group
// signal, the agent's own signals as want lists them, then the reap wait4
// made, and nothing after.
func checkFallback(t *testing.T, r *lifeRecorder, ch *Child, want ...syscall.Signal) {
	t.Helper()
	events := checkSignalsPrecedeTheReap(t, r, eventsAtTheReap(t, r, ch))
	var got []string
	for _, e := range events {
		if !e.reap && e.target != ch.pgid {
			t.Fatalf("events %v: a signal to %d in the fallback, which signals the agent alone", events, e.target)
		}
		got = append(got, e.String())
	}
	var exp []string
	for _, s := range want {
		exp = append(exp, lifeEvent{target: ch.pgid, sig: s, pinned: true}.String())
	}
	exp = append(exp, "reap")
	if strings.Join(got, ", ") != strings.Join(exp, ", ") {
		t.Fatalf("events %v, want %v", got, exp)
	}
	if ch.waitErr != ch.exitErr {
		t.Fatalf("waitErr %v, exitErr %v: the fallback's reap is its status", ch.waitErr, ch.exitErr)
	}
}

// finalWaits records the fallback's cmd.Wait calls (finalWait): the
// Process's pid as each was made, and its answer.
type finalWaits struct {
	mu    sync.Mutex
	calls map[*exec.Cmd][2]any // pid at the call, the error
}

func recordFinalWaits(t *testing.T) *finalWaits {
	fw := &finalWaits{calls: map[*exec.Cmd][2]any{}}
	real := finalWait
	finalWait = func(cmd *exec.Cmd) error {
		pid := cmd.Process.Pid
		err := real(cmd)
		fw.mu.Lock()
		fw.calls[cmd] = [2]any{pid, err}
		fw.mu.Unlock()
		return err
	}
	// Registered before any Spawn, so it runs after every Close.
	t.Cleanup(func() { finalWait = real })
	return fw
}

// checkNoWaitAfterTheReap checks that the fallback's cmd.Wait for ch, after
// its own reap, waited on no pid: the Process released first (its Pid -1),
// so its wait answered EINVAL without a system call. ch keeps the agent's
// pid all the same.
func (fw *finalWaits) checkNoWaitAfterTheReap(t *testing.T, ch *Child) {
	t.Helper()
	fw.mu.Lock()
	call, ok := fw.calls[ch.cmd]
	fw.mu.Unlock()
	if !ok {
		t.Fatal("the fallback never called cmd.Wait for its pipes")
	}
	if pid, err := call[0].(int), call[1]; pid != -1 || !errors.Is(err.(error), syscall.EINVAL) {
		t.Fatalf("cmd.Wait after the fallback's reap: the Process's pid %d, its answer %v; want it released (-1) and EINVAL, no wait on a pid", pid, err)
	}
	if ch.PID() <= 0 || ch.PID() != ch.pgid {
		t.Fatalf("PID() = %d after the release, want the agent's %d", ch.PID(), ch.pgid)
	}
}

// TestTheFallbackOwnsTheReapAndItsSignals (X71): where the exit cannot be
// observed without a reap — the observation refused (ENOSYS, as under a
// seccomp filter) — the reaper polls wait4 itself and signals only the agent,
// only between its polls, while it knows the agent is unreaped: no group
// signal, and no signal after the reap. Its status is that wait4's. No wait
// on the pid follows that reap (X73): cmd.Wait, called for the pipes, finds
// the Process released.
func TestTheFallbackOwnsTheReapAndItsSignals(t *testing.T) {
	stubObserve(t, 0, syscall.ENOSYS, nil)
	r := recordLife(t)
	fw := recordFinalWaits(t)
	t.Run("a shutdown at its entry", func(t *testing.T) {
		entered, release := stopAtFallback(t)
		var stderr syncBuffer
		c := spawnShell(t, `echo ready >&2; exec sleep 60`, &stderr)
		t.Cleanup(release)
		waitClosed(t, entered, 10*time.Second, "the fallback's entry")
		stderr.waitFor(t, "ready")
		done := make(chan struct{})
		go func() {
			c.child.Shutdown()
			close(done)
		}()
		for !isClosed(c.child.shutdownCh) {
			time.Sleep(time.Millisecond)
		}
		release()
		waitClosed(t, done, shutdownGrace+10*time.Second, "the Shutdown")
		var ee *ExitError
		if !errors.As(c.child.exitErr, &ee) || ee.Error() != "signal: terminated" {
			t.Fatalf("exit %v, want signal: terminated", c.child.exitErr)
		}
		checkFallback(t, r, c.child, syscall.SIGTERM)
		fw.checkNoWaitAfterTheReap(t, c.child)
		if !gone(c.PID()) {
			t.Fatal("the agent was left a zombie")
		}
	})
	t.Run("a leader that ignores SIGTERM", func(t *testing.T) {
		var stderr syncBuffer
		c := spawnShell(t, `trap '' TERM; echo ready >&2; exec sleep 60`, &stderr)
		stderr.waitFor(t, "ready")
		shutdownWithin(t, c.child, shutdownGrace+10*time.Second)
		var ee *ExitError
		if !errors.As(c.child.exitErr, &ee) || ee.Error() != "signal: killed" {
			t.Fatalf("exit %v, want signal: killed", c.child.exitErr)
		}
		checkFallback(t, r, c.child, syscall.SIGTERM, syscall.SIGKILL)
		fw.checkNoWaitAfterTheReap(t, c.child)
	})
	t.Run("its own exit", func(t *testing.T) {
		c := spawnShell(t, "exit 3", nil)
		waitClosed(t, c.child.reapedCh, 10*time.Second, "the reap")
		if err := c.Close(); !errors.Is(err, ErrAgentExited) || !strings.HasSuffix(err.Error(), ": exit status 3") {
			t.Fatalf("Close = %v, want ErrAgentExited with exit status 3", err)
		}
		checkFallback(t, r, c.child)
		fw.checkNoWaitAfterTheReap(t, c.child)
	})
}

// TestTheFallbackIsChosenBeforeAnySignal (X71): whether the exit can be
// observed is settled before the reaper runs, so a Shutdown that comes at
// once never finds the normal reaper escalating against the group while the
// observation's refusal is still on its way: here the observation's wait
// would be held until the test lets it go, and the group is never signalled.
func TestTheFallbackIsChosenBeforeAnySignal(t *testing.T) {
	hold := make(chan struct{})
	var released sync.Once
	stubObserve(t, 0, syscall.ENOSYS, hold)
	r := recordLife(t)
	entered := make(chan struct{})
	var once sync.Once
	fallbackEntered = func(int) { once.Do(func() { close(entered) }) }
	t.Cleanup(func() { fallbackEntered = nil })
	t.Cleanup(func() { released.Do(func() { close(hold) }) })
	var stderr syncBuffer
	c := spawnShell(t, `echo ready >&2; exec sleep 60`, &stderr)
	stderr.waitFor(t, "ready")
	done := make(chan struct{})
	go func() {
		c.child.Shutdown()
		close(done)
	}()
	// The fallback's entry, or — the decision left to the observation — a
	// group signal: whichever comes, the held observation is let go.
	deadline := time.Now().Add(10 * time.Second)
	for !isClosed(entered) && groupSignals(r.of(c.child.pgid)) == "" {
		if time.Now().After(deadline) {
			t.Fatal("neither the fallback nor a signal")
		}
		time.Sleep(time.Millisecond)
	}
	released.Do(func() { close(hold) })
	waitClosed(t, done, 2*shutdownGrace+10*time.Second, "the Shutdown")
	if got := groupSignals(r.of(c.child.pgid)); got != "" {
		t.Fatalf("group signals %q with nothing pinning the group's id", got)
	}
	checkFallback(t, r, c.child, syscall.SIGTERM)
}

// withoutStatus makes the observation report the exit with no status for
// the rest of the test, as macOS's does for an agent found already a zombie.
func withoutStatus(t *testing.T) {
	real := watch
	watch = func(pid int) (func() (syscall.WaitStatus, bool, error), error) {
		w, err := real(pid)
		if err != nil {
			return nil, err
		}
		return func() (syscall.WaitStatus, bool, error) {
			_, _, err := w()
			return 0, false, err
		}, nil
	}
	// Registered before any Spawn, so it runs after every Close.
	t.Cleanup(func() { watch = real })
}

// TestAnExitWithoutAStatusTakesTheReaps (X71): an exit observed with no
// status — macOS's for an agent already a zombie, whose kernel record
// saturates it — closes exitedCh at once and publishes the status at the
// reap, cmd.Wait's: the call pending at the exit fails then, with it, and so
// does Close's ErrAgentExited. Here a tool that ignores SIGTERM holds the
// reap a grace after the exit.
func TestAnExitWithoutAStatusTakesTheReaps(t *testing.T) {
	withoutStatus(t)
	c := spawnShell(t, `trap '' TERM; sleep 60 & read line; exit 7`, nil)
	failed := make(chan error, 1)
	go func() {
		_, err := c.Initialize(t.Context())
		failed <- err
	}()
	waitClosed(t, c.child.exitedCh, 10*time.Second, "the agent's exit")
	if isClosed(c.child.statusCh) {
		t.Fatalf("a status (%v) published at an exit observed without one", c.child.exitErr)
	}
	err := <-failed
	var ee *ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 7 || !strings.Contains(err.Error(), "acp: agent exited: exit status 7") {
		t.Fatalf("Initialize = %v, want it failed with the reap's exit status 7", err)
	}
	checkExit(t, c.child, "exit status 7", 7)
	if err := c.Close(); !errors.Is(err, ErrAgentExited) || !strings.HasSuffix(err.Error(), ": exit status 7") {
		t.Fatalf("Close = %v, want ErrAgentExited with exit status 7", err)
	}
}

// TestALaterShutdownLeavesASelfExitItsOwn (X73): whether craze asked for
// the end is latched when the exit is observed. An agent that exits 7 on its
// own, its status known only at the reap (here held a grace by a tool that
// ignores SIGTERM), and then a Close — its Shutdown arriving while the reaper
// still cleans up — keeps exit 7: the call pending at the exit fails with
// it, and Close reports it, rather than ErrClosed.
func TestALaterShutdownLeavesASelfExitItsOwn(t *testing.T) {
	withoutStatus(t)
	c := spawnShell(t, `trap '' TERM; sleep 60 & read line; exit 7`, nil)
	failed := make(chan error, 1)
	go func() {
		_, err := c.Initialize(t.Context())
		failed <- err
	}()
	waitClosed(t, c.child.exitedCh, 10*time.Second, "the agent's exit")
	closed := make(chan error, 1)
	go func() { closed <- c.Close() }()
	waitClosed(t, c.child.shutdownCh, 10*time.Second, "Close's Shutdown")
	if isClosed(c.child.statusCh) {
		t.Fatal("the status came before the Shutdown: the schedule this test is for did not happen")
	}
	err := <-failed
	var ee *ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 7 {
		t.Fatalf("the pending call failed with %v, want the agent's own exit status 7", err)
	}
	select {
	case err := <-closed:
		if !errors.Is(err, ErrAgentExited) || !strings.HasSuffix(err.Error(), ": exit status 7") {
			t.Fatalf("Close = %v, want ErrAgentExited with exit status 7", err)
		}
	case <-time.After(2*shutdownGrace + 10*time.Second):
		t.Fatal("Close did not return")
	}
}

// TestWatchExitLeavesTheProcessUnreaped: the platform's observation reports a
// process's exit and its status and leaves it a zombie, so it can be
// observed again, and the reap that follows agrees. The process waits for a
// gate the test opens once the first watch is set up, so that watch sees the
// exit happen (macOS: its NOTE_EXIT, not a registration refused). The second
// watch is set up on the zombie: Linux's waitid reports the status again;
// macOS's registration is refused, so the exit comes with no status, and the
// reap's stands.
func TestWatchExitLeavesTheProcessUnreaped(t *testing.T) {
	for _, tc := range []struct{ last, want string }{
		{"exit 9", "exit status 9"},
		{"kill -KILL $$", "signal: killed"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			gate := filepath.Join(t.TempDir(), "go")
			cmd := exec.Command("/bin/sh", "-c", "while [ ! -e "+gate+" ]; do sleep 0.01; done; "+tc.last)
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			pid := cmd.Process.Pid
			t.Cleanup(func() {
				if cmd.ProcessState == nil {
					_ = cmd.Process.Kill()
					_ = cmd.Wait()
				}
			})
			w, err := watchExit(pid)
			if err != nil {
				t.Fatal(err)
			}
			openGate(t, gate)
			first, known, err := w()
			if err != nil || !known {
				t.Fatalf("the first observation: %v, a status %v", err, known)
			}
			w, err = watchExit(pid)
			if err != nil {
				t.Fatalf("the second watch: %v (was the first observation a reap?)", err)
			}
			again, againKnown, err := w()
			if err != nil {
				t.Fatalf("the second observation: %v (was the first a reap?)", err)
			}
			if wantKnown := runtime.GOOS != "darwin"; againKnown != wantKnown {
				t.Fatalf("the observation of a zombie came with a status %v, want %v", againKnown, wantKnown)
			}
			_ = cmd.Wait()
			reaped := cmd.ProcessState.Sys().(syscall.WaitStatus)
			if first != reaped || (againKnown && again != reaped) {
				t.Fatalf("observed %#x, then %#x (a status: %v); the reap says %#x", first, again, againKnown, reaped)
			}
			if got := (&ExitError{Status: first}).Error(); got != tc.want || got != cmd.ProcessState.String() {
				t.Fatalf("observed %q, reaped %q; want %q", got, cmd.ProcessState.String(), tc.want)
			}
		})
	}
}

// TestParseStat: a /proc/<pid>/stat line's state, process group and thread
// count, its command's parentheses and spaces notwithstanding.
func TestParseStat(t *testing.T) {
	tail := " 0 -1 4194304 92 0 0 0 1 2 3 4 20 0 %d 0 9"
	for _, tc := range []struct {
		line string
		want procStat
		ok   bool
	}{
		{"4242 (sleep) S 4241 4240 4240" + fmt.Sprintf(tail, 1), procStat{'S', 4240, 1}, true},
		{"4242 (sh) Z 1 4242 4242" + fmt.Sprintf(tail, 3), procStat{'Z', 4242, 3}, true},
		{"77 (a) b) (c) R 1 99 99" + fmt.Sprintf(tail, 1), procStat{'R', 99, 1}, true},
		{"77 (tool name) D 2 123 123" + fmt.Sprintf(tail, 12), procStat{'D', 123, 12}, true},
		{"77 (sleep) S 1 2 3 0 -1", procStat{}, false},
		{"77 sleep S 1 2 3" + fmt.Sprintf(tail, 1), procStat{}, false},
		{"77 (sleep) SS 1 2 3" + fmt.Sprintf(tail, 1), procStat{}, false},
		{"77 (sleep) S 1 x 3" + fmt.Sprintf(tail, 1), procStat{}, false},
		{"77 (sleep) S 1 2 3" + strings.Replace(fmt.Sprintf(tail, 1), " 1 0 9", " x 0 9", 1), procStat{}, false},
	} {
		got, ok := parseStat([]byte(tc.line))
		if got != tc.want || ok != tc.ok {
			t.Errorf("parseStat(%q) = %+v, %v; want %+v, %v", tc.line, got, ok, tc.want, tc.ok)
		}
	}
}
