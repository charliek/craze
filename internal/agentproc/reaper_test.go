package agentproc

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

const helperEnv = "CRAZE_AGENTPROC_TEST_HELPER"
const helperGateEnv = "CRAZE_AGENTPROC_TEST_GATE"

func init() {
	switch os.Getenv(helperEnv) {
	case "exit-256":
		awaitGate(os.Getenv(helperGateEnv))
		os.Exit(256)
	case "hold-stdin-exit-7":
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
}

func awaitGate(gate string) {
	for deadline := time.Now().Add(30 * time.Second); gate != "" && time.Now().Before(deadline); {
		if _, err := os.Stat(gate); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func openGate(t *testing.T, gate string) {
	t.Helper()
	if err := os.WriteFile(gate, nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

func helperArgs(t *testing.T) (string, []string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return exe, []string{"-test.run=^$"}
}

func spawnShell(t *testing.T, script string, stderr io.Writer) *Child {
	t.Helper()
	if stderr == nil {
		stderr = io.Discard
	}
	ch, stdout, stdin, err := Start(StartOptions{Binary: "/bin/sh", Args: []string{"-c", script}, Stderr: stderr})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ch.Shutdown()
		_ = stdin.Close()
		_ = stdout.Close()
		ch.Wait()
	})
	return ch
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
		_ = syscall.Kill(-ch.pgid, syscall.SIGKILL)
		t.Fatalf("Shutdown did not return within %v", d)
	}
	return time.Since(start)
}

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

func gone(pid int) bool { return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) }

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

type lifeEvent struct {
	target int
	sig    syscall.Signal
	reap   bool
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
	events map[int][]lifeEvent
}

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

func groupSignals(events []lifeEvent) string {
	var b strings.Builder
	for _, e := range events {
		if !e.reap && e.target < 0 {
			fmt.Fprintf(&b, "%d ", e.sig)
		}
	}
	return b.String()
}

type reapedEvents struct {
	ch     *Child
	atReap []lifeEvent
}

func eventsAtTheReap(t *testing.T, r *lifeRecorder, ch *Child) reapedEvents {
	t.Helper()
	waitClosed(t, ch.reapedCh, 10*time.Second, "the reap")
	return reapedEvents{ch: ch, atReap: r.of(ch.pgid)}
}

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

// TestCloseReapsSurvivingGrandchild verifies across Linux and macOS that
// shutting down a Child reaps both the child process group AND a surviving
// grandchild process spawned inside a subshell.
func TestCloseReapsSurvivingGrandchild(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "grandchild.pid")
	var stderr syncBuffer
	ch := spawnShell(t, `sh -c 'sleep 60 & echo $! > `+pidFile+`; wait' & echo ready >&2; wait`, &stderr)
	stderr.waitFor(t, "ready")
	grandchild := readPID(t, pidFile)
	if gone(grandchild) {
		t.Fatalf("grandchild (pid %d) was already gone before Shutdown", grandchild)
	}
	shutdownWithin(t, ch, 2*ShutdownGrace+10*time.Second)
	waitGone(t, grandchild, 2*ShutdownGrace+10*time.Second, "surviving grandchild")
	if !gone(ch.PID()) {
		t.Fatalf("child (pid %d) was left a zombie", ch.PID())
	}
}

func TestShutdownKillsALeaderThatIgnoresSIGTERM(t *testing.T) {
	var stderr syncBuffer
	ch := spawnShell(t, `trap '' TERM; echo ready >&2; exec sleep 60`, &stderr)
	stderr.waitFor(t, "ready")
	took := shutdownWithin(t, ch, ShutdownGrace+10*time.Second)
	if took < ShutdownGrace {
		t.Fatalf("Shutdown returned after %v, inside the %v grace", took, ShutdownGrace)
	}
	if took > ShutdownGrace+3*time.Second {
		t.Fatalf("Shutdown took %v, not about the %v grace", took, ShutdownGrace)
	}
	checkExit(t, ch, "signal: killed", -1)
	if !gone(ch.PID()) {
		t.Fatal("the agent was left a zombie")
	}
}

func TestWhatTheAgentLeftBehindDiesAtItsExit(t *testing.T) {
	for _, tc := range []struct{ name, trap string }{
		{"a tool", ""},
		{"a tool that ignores SIGTERM", "trap '' TERM; "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pidFile := filepath.Join(t.TempDir(), "tool.pid")
			ch := spawnShell(t, tc.trap+"sleep 60 & echo $! > "+pidFile+"; exit 0", nil)
			waitClosed(t, ch.exitedCh, 10*time.Second, "the agent's exit")
			tool := readPID(t, pidFile)
			waitGone(t, tool, 2*ShutdownGrace+10*time.Second, "the tool the agent left behind")
			waitClosed(t, ch.reapedCh, 10*time.Second, "the reap")
			checkExit(t, ch, "", 0)
		})
	}
}

func TestShutdownRacesTheAgentsOwnExit(t *testing.T) {
	r := recordLife(t)
	var reaped []reapedEvents
	outcomes := map[string]int{}
	for i := range 24 {
		script := "exit 4"
		if i%4 == 3 {
			script = "exec sleep 60"
		}
		ch := spawnShell(t, script, io.Discard)
		switch i % 4 {
		case 1:
			<-ch.exitedCh
		case 2:
			<-ch.reapedCh
		}
		shutdownWithin(t, ch, ShutdownGrace+10*time.Second)
		var ee *ExitError
		if !errors.As(ch.exitErr, &ee) || (ee.Error() != "exit status 4" && ee.Error() != "signal: terminated") {
			t.Fatalf("round %d: observed %v, want exit status 4 or signal: terminated", i, ch.exitErr)
		}
		checkExit(t, ch, ee.Error(), ee.ExitCode())
		outcomes[ee.Error()]++
		reaped = append(reaped, eventsAtTheReap(t, r, ch))
		if !gone(ch.PID()) {
			t.Fatalf("round %d: the agent was left a zombie", i)
		}
	}
	checkSignalsPrecedeTheReap(t, r, reaped...)
	if outcomes["exit status 4"] == 0 || outcomes["signal: terminated"] == 0 {
		t.Fatalf("exits %v: want both the agent's own and the SIGTERM's", outcomes)
	}
}

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
		ch := spawnShell(t, script(t.TempDir()), &stderr)
		waitClosed(t, ch.exitedCh, 10*time.Second, "the agent's exit")
		ch.Shutdown()
		ch.Wait()
		check(t, stderr.String())
	})
	t.Run("into a file", func(t *testing.T) {
		f, err := os.Create(filepath.Join(t.TempDir(), "stderr"))
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		ch := spawnShell(t, script(t.TempDir()), f)
		waitClosed(t, ch.exitedCh, 10*time.Second, "the agent's exit")
		ch.Shutdown()
		ch.Wait()
		got, err := os.ReadFile(f.Name())
		if err != nil {
			t.Fatal(err)
		}
		check(t, string(got))
	})
}

type slowWriter struct {
	syncBuffer
	delay time.Duration
}

func (w *slowWriter) Write(p []byte) (int, error) {
	time.Sleep(w.delay)
	return w.syncBuffer.Write(p)
}

func TestTheAgentsStderrTailOutlivesTheReap(t *testing.T) {
	w := &slowWriter{delay: 30 * time.Millisecond}
	ch := spawnShell(t, `dd if=/dev/zero bs=1024 count=64 2>/dev/null | tr '\0' x >&2; echo agent-last >&2; exit 0`, w)
	waitClosed(t, ch.reapedCh, 10*time.Second, "the reap")
	ch.Wait()
	got := w.String()
	if n := strings.Count(got, "x"); n != 64*1024 || !strings.HasSuffix(got, "agent-last\n") {
		t.Fatalf("stderr has %d of 65536 x's, ending %q", n, got[max(0, len(got)-20):])
	}
}

func TestAnInterruptedObserveIsRetried(t *testing.T) {
	calls := stubObserve(t, 3, nil, nil)
	pidFile := filepath.Join(t.TempDir(), "tool.pid")
	ch := spawnShell(t, "sleep 60 & echo $! > "+pidFile+"; exit 6", nil)
	waitClosed(t, ch.exitedCh, 10*time.Second, "the agent's exit")
	tool := readPID(t, pidFile)
	waitGone(t, tool, 2*ShutdownGrace+10*time.Second, "the tool the agent left behind")
	checkExit(t, ch, "exit status 6", 6)
	if n := calls.Load(); n < 4 {
		t.Fatalf("the observation's call was made %d times; want at least 4", n)
	}
}

func TestEveryGroupSignalPrecedesTheReap(t *testing.T) {
	r := recordLife(t)
	term, kill := int(syscall.SIGTERM), int(syscall.SIGKILL)
	t.Run("a shutdown", func(t *testing.T) {
		var stderr syncBuffer
		pidFile := filepath.Join(t.TempDir(), "tool.pid")
		ch := spawnShell(t, `trap '' TERM; sleep 60 & echo $! > `+pidFile+`; trap - TERM; echo ready >&2; exec sleep 60`, &stderr)
		stderr.waitFor(t, "ready")
		tool := readPID(t, pidFile)
		shutdownWithin(t, ch, 2*ShutdownGrace+10*time.Second)
		sent := checkSignalsPrecedeTheReap(t, r, eventsAtTheReap(t, r, ch))
		want := fmt.Sprintf("%d %d %d %d ", term, term, kill, kill)
		if got := groupSignals(sent); got != want {
			t.Fatalf("group signals %q, want %q", got, want)
		}
		waitGone(t, tool, 10*time.Second, "the tool")
	})
	t.Run("its own exit", func(t *testing.T) {
		pidFile := filepath.Join(t.TempDir(), "tool.pid")
		ch := spawnShell(t, `trap '' TERM; sleep 60 & echo $! > `+pidFile+`; exit 0`, nil)
		tool := readPID(t, pidFile)
		sent := checkSignalsPrecedeTheReap(t, r, eventsAtTheReap(t, r, ch))
		want := fmt.Sprintf("%d %d %d ", term, kill, kill)
		if got := groupSignals(sent); got != want {
			t.Fatalf("group signals %q, want %q", got, want)
		}
		waitGone(t, tool, 10*time.Second, "the tool")
	})
	t.Run("a clean exit", func(t *testing.T) {
		ch := spawnShell(t, `exit 0`, nil)
		sent := checkSignalsPrecedeTheReap(t, r, eventsAtTheReap(t, r, ch))
		if got, want := groupSignals(sent), fmt.Sprintf("%d ", kill); got != want {
			t.Fatalf("group signals %q, want %q", got, want)
		}
	})
}

func stopAtFallback(t *testing.T) (entered <-chan struct{}, release func()) {
	in, out := make(chan struct{}), make(chan struct{})
	var once sync.Once
	fallbackEntered = func(int) {
		close(in)
		<-out
	}
	t.Cleanup(func() { fallbackEntered = nil })
	return in, func() { once.Do(func() { close(out) }) }
}

func checkFallback(t *testing.T, r *lifeRecorder, ch *Child, want ...syscall.Signal) {
	t.Helper()
	events := checkSignalsPrecedeTheReap(t, r, eventsAtTheReap(t, r, ch))
	var got []string
	for _, e := range events {
		if !e.reap && e.target != -ch.pgid {
			t.Fatalf("events %v: a signal to %d in the fallback", events, e.target)
		}
		got = append(got, e.String())
	}
	var exp []string
	for _, s := range want {
		exp = append(exp, lifeEvent{target: -ch.pgid, sig: s, pinned: true}.String())
	}
	exp = append(exp, "reap")
	if strings.Join(got, ", ") != strings.Join(exp, ", ") {
		t.Fatalf("events %v, want %v", got, exp)
	}
	if ch.waitErr != ch.exitErr {
		t.Fatalf("waitErr %v, exitErr %v", ch.waitErr, ch.exitErr)
	}
}

type finalWaits struct {
	mu    sync.Mutex
	calls map[*exec.Cmd][2]any
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
	t.Cleanup(func() { finalWait = real })
	return fw
}

func (fw *finalWaits) checkNoWaitAfterTheReap(t *testing.T, ch *Child) {
	t.Helper()
	fw.mu.Lock()
	call, ok := fw.calls[ch.cmd]
	fw.mu.Unlock()
	if !ok {
		t.Fatal("the fallback never called cmd.Wait for its pipes")
	}
	if pid, err := call[0].(int), call[1]; pid != -1 || !errors.Is(err.(error), syscall.EINVAL) {
		t.Fatalf("cmd.Wait after the fallback's reap: pid %d, err %v; want -1 and EINVAL", pid, err)
	}
	if ch.PID() <= 0 || ch.PID() != ch.pgid {
		t.Fatalf("PID() = %d after the release, want %d", ch.PID(), ch.pgid)
	}
}

func TestTheFallbackOwnsTheReapAndItsSignals(t *testing.T) {
	stubObserve(t, 0, syscall.ENOSYS, nil)
	r := recordLife(t)
	fw := recordFinalWaits(t)
	t.Run("a shutdown at its entry", func(t *testing.T) {
		entered, release := stopAtFallback(t)
		var stderr syncBuffer
		ch := spawnShell(t, `echo ready >&2; exec sleep 60`, &stderr)
		t.Cleanup(release)
		waitClosed(t, entered, 10*time.Second, "the fallback's entry")
		stderr.waitFor(t, "ready")
		done := make(chan struct{})
		go func() {
			ch.Shutdown()
			close(done)
		}()
		for !isClosed(ch.shutdownCh) {
			time.Sleep(time.Millisecond)
		}
		release()
		waitClosed(t, done, ShutdownGrace+10*time.Second, "the Shutdown")
		var ee *ExitError
		if !errors.As(ch.exitErr, &ee) || ee.Error() != "signal: terminated" {
			t.Fatalf("exit %v, want signal: terminated", ch.exitErr)
		}
		checkFallback(t, r, ch, syscall.SIGTERM)
		fw.checkNoWaitAfterTheReap(t, ch)
		if !gone(ch.PID()) {
			t.Fatal("the agent was left a zombie")
		}
	})
	t.Run("a leader that ignores SIGTERM", func(t *testing.T) {
		var stderr syncBuffer
		ch := spawnShell(t, `trap '' TERM; echo ready >&2; exec sleep 60`, &stderr)
		stderr.waitFor(t, "ready")
		shutdownWithin(t, ch, ShutdownGrace+10*time.Second)
		var ee *ExitError
		if !errors.As(ch.exitErr, &ee) || ee.Error() != "signal: killed" {
			t.Fatalf("exit %v, want signal: killed", ch.exitErr)
		}
		checkFallback(t, r, ch, syscall.SIGTERM, syscall.SIGKILL)
		fw.checkNoWaitAfterTheReap(t, ch)
	})
	t.Run("its own exit", func(t *testing.T) {
		ch := spawnShell(t, "exit 3", nil)
		waitClosed(t, ch.reapedCh, 10*time.Second, "the reap")
		checkFallback(t, r, ch)
		fw.checkNoWaitAfterTheReap(t, ch)
	})
}

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
	ch := spawnShell(t, `echo ready >&2; exec sleep 60`, &stderr)
	stderr.waitFor(t, "ready")
	done := make(chan struct{})
	go func() {
		ch.Shutdown()
		close(done)
	}()
	deadline := time.Now().Add(10 * time.Second)
	for !isClosed(entered) && groupSignals(r.of(ch.pgid)) == "" {
		if time.Now().After(deadline) {
			t.Fatal("neither the fallback nor a signal")
		}
		time.Sleep(time.Millisecond)
	}
	if !isClosed(entered) {
		t.Fatalf("the group was signalled (%q) before the fallback was chosen", groupSignals(r.of(ch.pgid)))
	}
	released.Do(func() { close(hold) })
	waitClosed(t, done, 2*ShutdownGrace+10*time.Second, "the Shutdown")
	checkFallback(t, r, ch, syscall.SIGTERM)
}

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
				t.Fatalf("the second watch: %v", err)
			}
			again, againKnown, err := w()
			if err != nil {
				t.Fatalf("the second observation: %v", err)
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

func TestLiveMember(t *testing.T) {
	const agent = 4240
	for _, tc := range []struct {
		name    string
		entries []procEntry
		live    bool
	}{
		{"the agent, running with no flags", []procEntry{{pid: agent}}, false},
		{"a member in its exit", []procEntry{{pid: 4242, exiting: true}}, false},
		{"a zombie member", []procEntry{{pid: 4242, zombie: true}}, false},
		{"a running member", []procEntry{{pid: 4242}}, true},
		{"a running member listed last", []procEntry{{pid: agent, exiting: true}, {pid: 4241, zombie: true}, {pid: 4243, exiting: true}, {pid: 4242}}, true},
	} {
		if got := liveMember(tc.entries, agent); got != tc.live {
			t.Errorf("%s: liveMember(%+v, %d) = %v, want %v", tc.name, tc.entries, agent, got, tc.live)
		}
	}
}
