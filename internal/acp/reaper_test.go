package acp

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agentproc"
)

// ACP client integration tests for process lifecycle, exit reporting, and
// pending-call failure (the low-level reaper/proctree unit tests live in
// internal/agentproc).

const helperEnv = "CRAZE_ACP_TEST_HELPER"

func init() {
	if os.Getenv(helperEnv) != "hold-stdin-exit-7" {
		return
	}
	signal.Ignore(syscall.SIGTERM)
	holder := exec.Command("sleep", "30")
	holder.Stdin = os.Stdin
	if err := holder.Start(); err != nil {
		os.Exit(3)
	}
	fmt.Fprintln(os.Stderr, "ready")
	var b [1]byte
	if n, _ := os.Stdin.Read(b[:]); n != 1 {
		syscall.Exit(4)
	}
	syscall.Exit(7)
}

func helperArgs(t *testing.T) (string, []string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return exe, []string{"-test.run=^$"}
}

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

func waitClosed(t *testing.T, ch <-chan struct{}, d time.Duration, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(d):
		t.Fatalf("%s did not happen within %v", what, d)
	}
}

func isClosedCh(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func gone(pid int) bool { return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) }

func shutdownWithin(t *testing.T, ch *agentproc.Child, d time.Duration) time.Duration {
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
		_ = syscall.Kill(-ch.PGID(), syscall.SIGKILL)
		t.Fatalf("Shutdown did not return within %v", d)
	}
	return time.Since(start)
}

func checkExit(t *testing.T, ch *agentproc.Child, want string, code int) {
	t.Helper()
	waitClosed(t, ch.Reaped(), 10*time.Second, "the reap")
	if want == "" {
		if ch.ExitErr() != nil || ch.WaitErr() != nil {
			t.Fatalf("observed %v, reaped %v; want an exit 0 both times", ch.ExitErr(), ch.WaitErr())
		}
		return
	}
	var ee *ExitError
	if !errors.As(ch.ExitErr(), &ee) {
		t.Fatalf("observed %v (%T), want an *ExitError", ch.ExitErr(), ch.ExitErr())
	}
	if ee.Error() != want || ee.ExitCode() != code {
		t.Fatalf("observed %q, code %d; want %q, code %d", ee.Error(), ee.ExitCode(), want, code)
	}
	var we *exec.ExitError
	if !errors.As(ch.WaitErr(), &we) || we.Error() != want || we.ExitCode() != code {
		t.Fatalf("cmd.Wait at the reap said %v, which does not agree with the observed %q", ch.WaitErr(), want)
	}
}

func withoutStatus(t *testing.T) {
	restore := agentproc.SetWatchForTest(func(pid int) (func() (syscall.WaitStatus, bool, error), error) {
		w, err := agentproc.WatchExitDefault(pid)
		if err != nil {
			return nil, err
		}
		return func() (syscall.WaitStatus, bool, error) {
			_, _, err := w()
			return 0, false, err
		}, nil
	})
	t.Cleanup(restore)
}

func forcePollingReaper(t *testing.T) {
	restore := agentproc.SetWatchForTest(func(int) (func() (syscall.WaitStatus, bool, error), error) {
		return nil, syscall.ENOSYS
	})
	t.Cleanup(restore)
}

func TestTheExitStatusIsObservedOnEveryPath(t *testing.T) {
	if ErrAgentExited.Error() != "acp: agent exited" {
		t.Fatalf("ErrAgentExited.Error() = %q, want %q", ErrAgentExited.Error(), "acp: agent exited")
	}
	t.Run("its own clean exit", func(t *testing.T) {
		c := spawnShell(t, "exit 0", nil)
		waitClosed(t, c.child.Exited(), 10*time.Second, "the agent's clean exit")
		err := c.Close()
		if !errors.Is(err, ErrAgentExited) || err.Error() != "acp: agent exited: exit 0" {
			t.Fatalf("Close = %v, want %q", err, "acp: agent exited: exit 0")
		}
	})
	t.Run("its own exit", func(t *testing.T) {
		c := spawnShell(t, "exit 3", nil)
		waitClosed(t, c.child.Exited(), 10*time.Second, "the agent's exit")
		checkExit(t, c.child, "exit status 3", 3)
		err := c.Close()
		if !errors.Is(err, ErrAgentExited) || err.Error() != "acp: agent exited: exit status 3" {
			t.Fatalf("Close = %v, want %q", err, "acp: agent exited: exit status 3")
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
			case <-time.After(agentproc.ShutdownGrace + 10*time.Second):
				_ = syscall.Kill(-c.child.PGID(), syscall.SIGKILL)
				t.Fatal("Close did not return")
			}
			checkExit(t, c.child, tc.want, tc.code)
		})
	}
}

func TestPendingCallsFailWithTheObservedExit(t *testing.T) {
	c := spawnShell(t, `trap '' TERM; sleep 60 & read line; exit 7`, nil)
	_, err := c.Initialize(t.Context())
	var ee *ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 7 || !strings.Contains(err.Error(), "acp: agent exited: exit status 7") {
		t.Fatalf("Initialize = %v, want it failed with the agent's exit status 7", err)
	}
	select {
	case <-c.child.Reaped():
		t.Fatal("the pending call failed only at the reap, not at the exit")
	default:
	}
	checkExit(t, c.child, "exit status 7", 7)
}

func TestAWriteBlockedOnTheAgentsStdinFailsAtItsExit(t *testing.T) {
	var observedAt atomic.Int64
	restore := agentproc.SetWatchForTest(func(pid int) (func() (syscall.WaitStatus, bool, error), error) {
		w, err := agentproc.WatchExitDefault(pid)
		if err != nil {
			return nil, err
		}
		return func() (syscall.WaitStatus, bool, error) {
			st, known, err := w()
			observedAt.Store(time.Now().UnixNano())
			return st, known, err
		}, nil
	})
	t.Cleanup(restore)
	exe, args := helperArgs(t)
	stderr := &syncBuffer{}
	c, err := Spawn(SpawnOptions{
		Binary: exe, Args: args, Stderr: stderr,
		Env: append(os.Environ(), helperEnv+"=hold-stdin-exit-7"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	stderr.waitFor(t, "ready")

	pad := map[string]string{"pad": strings.Repeat("x", 1<<20)}
	err = c.conn.Call(t.Context(), "x/blocked", pad, nil)
	returned := time.Now()
	var ee *ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 7 {
		t.Fatalf("the blocked call = %v, want it failed with the agent's exit status 7", err)
	}
	if took := returned.Sub(time.Unix(0, observedAt.Load())); took >= agentproc.ShutdownGrace/2 {
		t.Fatalf("the blocked call failed %v after the exit was observed", took)
	}
	checkExit(t, c.child, "exit status 7", 7)
}

func TestACleanAgentShutsDownWellUnderTheGrace(t *testing.T) {
	c := spawnScript(t, "echo")
	handshake(t, c)
	took := shutdownWithin(t, c.child, agentproc.ShutdownGrace+10*time.Second)
	if took >= agentproc.ShutdownGrace/2 {
		t.Fatalf("Shutdown of a clean agent took %v; want well under %v", took, agentproc.ShutdownGrace)
	}
	if !gone(c.PID()) {
		t.Fatal("the agent was left a zombie")
	}
}

func TestNoZombieIsLeftAfterShutdown(t *testing.T) {
	t.Run("running", func(t *testing.T) {
		c := spawnScript(t, "echo")
		handshake(t, c)
		shutdownWithin(t, c.child, agentproc.ShutdownGrace+10*time.Second)
		if err := syscall.Kill(c.PID(), 0); !errors.Is(err, syscall.ESRCH) {
			t.Fatalf("kill(pid, 0) after Shutdown = %v, want ESRCH", err)
		}
	})
	t.Run("exited on its own", func(t *testing.T) {
		c := spawnShell(t, "exit 0", nil)
		waitClosed(t, c.child.Exited(), 10*time.Second, "the agent's exit")
		shutdownWithin(t, c.child, agentproc.ShutdownGrace+10*time.Second)
		if err := syscall.Kill(c.PID(), 0); !errors.Is(err, syscall.ESRCH) {
			t.Fatalf("kill(pid, 0) after Shutdown = %v, want ESRCH", err)
		}
	})
}

func TestAnExitWithoutAStatusTakesTheReaps(t *testing.T) {
	withoutStatus(t)
	c := spawnShell(t, `trap '' TERM; sleep 60 & read line; exit 7`, nil)
	failed := make(chan error, 1)
	go func() {
		_, err := c.Initialize(t.Context())
		failed <- err
	}()
	waitClosed(t, c.child.Exited(), 10*time.Second, "the agent's exit")
	if isClosedCh(c.child.StatusReady()) {
		t.Fatalf("a status (%v) published at an exit observed without one", c.child.ExitErr())
	}
	err := <-failed
	var ee *ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 7 || !strings.Contains(err.Error(), "acp: agent exited: exit status 7") {
		t.Fatalf("Initialize = %v, want it failed with exit status 7", err)
	}
	checkExit(t, c.child, "exit status 7", 7)
	if err := c.Close(); !errors.Is(err, ErrAgentExited) || !strings.HasSuffix(err.Error(), ": exit status 7") {
		t.Fatalf("Close = %v, want ErrAgentExited with exit status 7", err)
	}
}

func TestALaterShutdownLeavesASelfExitItsOwn(t *testing.T) {
	withoutStatus(t)
	c := spawnShell(t, `trap '' TERM; sleep 60 & read line; exit 7`, nil)
	failed := make(chan error, 1)
	go func() {
		_, err := c.Initialize(t.Context())
		failed <- err
	}()
	waitClosed(t, c.child.Exited(), 10*time.Second, "the agent's exit")
	closed := make(chan error, 1)
	go func() { closed <- c.Close() }()
	waitClosed(t, c.child.ShutdownRequested(), 10*time.Second, "Close's Shutdown")
	if isClosedCh(c.child.StatusReady()) {
		t.Fatal("the status came before the Shutdown")
	}
	err := <-failed
	var ee *ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 7 {
		t.Fatalf("the pending call failed with %v, want exit status 7", err)
	}
	select {
	case err := <-closed:
		if !errors.Is(err, ErrAgentExited) || !strings.HasSuffix(err.Error(), ": exit status 7") {
			t.Fatalf("Close = %v, want ErrAgentExited with exit status 7", err)
		}
	case <-time.After(2*agentproc.ShutdownGrace + 10*time.Second):
		t.Fatal("Close did not return")
	}
}
