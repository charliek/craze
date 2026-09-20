package tui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The runner's tests (plan 022 A17). Every one of them starts real processes:
// what is under test is whether the kernel still has the process group
// afterwards, and craze's own idea of that is the thing being checked, so the
// assertions are process checks and never the controller's bookkeeping.

// shellMarkerSeq numbers the markers so two tests in one binary can never pick
// the same one.
var shellMarkerSeq atomic.Int64

// shellMarker is a string no other process on the box can be carrying. The
// tests put it in a child's argv and then ask the system whether anything still
// holds it.
//
// It is letters and digits only, and deliberately not the test's name: pgrep -f
// takes an extended regular expression, and a name like "ctrl+d" would be read
// as one — matching "ctrld" and never the marker actually on the command line.
func shellMarker(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("crazeShellTest%dn%d", os.Getpid(), shellMarkerSeq.Add(1))
}

// sleeperScript is a child that lives until something kills it and carries
// marker in its own argv.
//
// The loop matters: `sh -c 'sleep 30' marker` would be exec-optimised by dash
// and bash into a bare `sleep 30`, which carries no marker at all and would
// make every assertion below vacuous. A shell that has a loop to run cannot
// replace itself with anything.
func sleeperScript(marker string) string {
	return "/bin/sh -c 'while :; do sleep 1; done' " + marker
}

// markerAlive asks the system whether any process still carries marker.
func markerAlive(t *testing.T, marker string) bool {
	t.Helper()
	out, err := exec.Command("pgrep", "-f", marker).Output()
	if err == nil {
		return len(bytes.TrimSpace(out)) > 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 1 {
		return false // pgrep's own "nothing matched"
	}
	// No pgrep here: the /proc (or ps) scan the live tests already use.
	return processRunning(t, marker)
}

// waitMarker waits for marker to be alive (want) or gone (!want). It polls a
// condition rather than sleeping a guessed interval, so it is as fast as the
// box is and never a race.
func waitMarker(t *testing.T, marker string, want bool, d time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		if markerAlive(t, marker) == want {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// shortShellTimeout shortens the runner's 120 s ceiling for the tests that are
// about the ceiling itself.
func shortShellTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	prev := shellTimeout
	shellTimeout = d
	t.Cleanup(func() { shellTimeout = prev })
}

func TestUserShellFallsBackToBinSh(t *testing.T) {
	dir := t.TempDir()
	notExec := filepath.Join(dir, "not-exec")
	if err := os.WriteFile(notExec, []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, shell, want string }{
		{"unset", "", fallbackShell},
		{"relative", "sh", fallbackShell},
		{"missing", filepath.Join(dir, "nope"), fallbackShell},
		{"a directory", dir, fallbackShell},
		{"not executable", notExec, fallbackShell},
		{"absolute and executable", "/bin/sh", "/bin/sh"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SHELL", tc.shell)
			if got := userShell(); got != tc.want {
				t.Fatalf("userShell() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestShellRingKeepsTheTailAndNeverGrows(t *testing.T) {
	r := &shellRing{}
	if got, over := r.text(); got != "" || over {
		t.Fatalf("empty ring = %q, over=%v", got, over)
	}
	// Under the cap: everything is kept, in order, and nothing was dropped.
	if _, err := r.Write([]byte("abc")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Write([]byte("def")); err != nil {
		t.Fatal(err)
	}
	if got, over := r.text(); got != "abcdef" || over {
		t.Fatalf("short ring = %q, over=%v", got, over)
	}

	// Three times the cap in small writes, then one write bigger than the whole
	// ring: either way what is left is the last shellOutputCap bytes.
	r = &shellRing{}
	var want []byte
	chunk := bytes.Repeat([]byte("0123456789"), 97) // 970 bytes, no factor of the cap
	for i := 0; i < 3*shellOutputCap/len(chunk); i++ {
		b := append([]byte(fmt.Sprintf("<%04d>", i)), chunk...)
		if _, err := r.Write(b); err != nil {
			t.Fatal(err)
		}
		want = append(want, b...)
	}
	got, over := r.text()
	if !over {
		t.Fatal("a flood three times the cap must report that it dropped the head")
	}
	if len(got) != shellOutputCap {
		t.Fatalf("ring holds %d bytes, want exactly %d", len(got), shellOutputCap)
	}
	if string(want[len(want)-shellOutputCap:]) != got {
		t.Fatal("the ring kept something other than the tail")
	}

	big := bytes.Repeat([]byte("z"), 3*shellOutputCap)
	big = append(big, []byte("TAIL")...)
	if _, err := r.Write(big); err != nil {
		t.Fatal(err)
	}
	got, over = r.text()
	if !over || len(got) != shellOutputCap || !strings.HasSuffix(got, "TAIL") {
		t.Fatalf("one oversized write: len=%d over=%v suffix=%q", len(got), over, got[len(got)-8:])
	}
}

func TestSanitizeShellOutputIsAgentGrade(t *testing.T) {
	in := "plain\ttab\nline\x1b[31mred\x1b[0m\rcarriage\x07bell\xffbad"
	got := sanitizeShellOutput(in)
	want := "plain\ttab\nlineredcarriagebellbad"
	if got != want {
		t.Fatalf("sanitizeShellOutput = %q, want %q", got, want)
	}
}

// TestShellRunCapsAFloodAndKeepsTheTail is the bounded half of A17's flood: a
// command that produces far more than the cap keeps its ending, loses its
// beginning, and says so.
func TestShellRunCapsAFloodAndKeepsTheTail(t *testing.T) {
	script := "echo CRAZE_HEAD_MARKER; yes ABCDEFGHIJKLMNOP | head -n 20000; echo CRAZE_TAIL_MARKER"
	res := runShellCommand(context.Background(), script, t.TempDir())
	if res.start != nil || res.why != shellExited || res.exit != 0 {
		t.Fatalf("flood ended start=%v why=%v exit=%d", res.start, res.why, res.exit)
	}
	if max := shellOutputCap + len(shellTruncNote) + 1; len(res.out) > max {
		t.Fatalf("output is %d bytes, cap is %d", len(res.out), max)
	}
	if !strings.HasPrefix(res.out, shellTruncNote) {
		t.Fatalf("truncated output must say so, got %q", res.out[:min(80, len(res.out))])
	}
	if strings.Contains(res.out, "CRAZE_HEAD_MARKER") {
		t.Fatal("the head survived a flood three hundred times the cap")
	}
	if !strings.Contains(res.out, "CRAZE_TAIL_MARKER") {
		t.Fatal("the tail is what the cap keeps, and it is missing")
	}
}

// TestShellRunBoundsAnEndlessFlood is the other half, and the one that proves
// the ring rather than a trim: `yes` never stops, so a buffer that grew first
// and trimmed afterwards would allocate for as long as the command ran.
func TestShellRunBoundsAnEndlessFlood(t *testing.T) {
	shortShellTimeout(t, 400*time.Millisecond)
	start := time.Now()
	res := runShellCommand(context.Background(), "yes CRAZE_FLOOD_PAYLOAD", t.TempDir())
	if res.why != shellTimedOut {
		t.Fatalf("an endless command must end on the timeout, got why=%v exit=%d", res.why, res.exit)
	}
	if elapsed := time.Since(start); elapsed > shellTermGrace+shellKillWait+5*time.Second {
		t.Fatalf("the timeout took %s to end it", elapsed)
	}
	if max := shellOutputCap + len(shellTruncNote) + 1; len(res.out) > max {
		t.Fatalf("output is %d bytes, cap is %d", len(res.out), max)
	}
	if !strings.Contains(res.out, "CRAZE_FLOOD_PAYLOAD") {
		t.Fatalf("the flood produced nothing craze kept: %q", res.out)
	}
}

// TestShellRunTimeoutKillsTheGroup: the ceiling does not only stop the shell,
// it takes everything the shell started with it.
func TestShellRunTimeoutKillsTheGroup(t *testing.T) {
	shortShellTimeout(t, 400*time.Millisecond)
	marker := shellMarker(t)
	res := runShellCommand(context.Background(), sleeperScript(marker)+" & sleep 60", t.TempDir())
	if res.why != shellTimedOut {
		t.Fatalf("why = %v, want the timeout", res.why)
	}
	if !waitMarker(t, marker, false, 10*time.Second) {
		t.Fatal("the timeout left the background child running")
	}
}

// TestShellRunBackgroundChildDiesWithItsShell is `sleep 300 &`: the shell
// itself exits at once and cleanly, and craze still kills the group — the
// unconditional SIGKILL on every ending is the whole point of this test.
func TestShellRunBackgroundChildDiesWithItsShell(t *testing.T) {
	marker := shellMarker(t)
	// Output redirected away, so nothing holds the pipe: this is the path where
	// Wait returns immediately and only the final kill is left to do it.
	script := sleeperScript(marker) + " >/dev/null 2>&1 & echo CRAZE_STARTED"
	start := time.Now()
	res := runShellCommand(context.Background(), script, t.TempDir())
	if res.start != nil || res.why != shellExited || res.exit != 0 {
		t.Fatalf("ended start=%v why=%v exit=%d", res.start, res.why, res.exit)
	}
	if !strings.Contains(res.out, "CRAZE_STARTED") {
		t.Fatalf("output %q", res.out)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("a shell that exited at once took %s", elapsed)
	}
	if !waitMarker(t, marker, false, 10*time.Second) {
		t.Fatal("`&` outlived its shell")
	}
}

// TestShellRunDescendantHoldingThePipeDoesNotBlock is the WaitDelay case: the
// shell has exited but a child it left behind still holds the output pipe, so
// exec's copying goroutine would block for as long as that child lives.
func TestShellRunDescendantHoldingThePipeDoesNotBlock(t *testing.T) {
	marker := shellMarker(t)
	// No redirection this time: the child inherits craze's end of the pipe.
	script := sleeperScript(marker) + " & echo CRAZE_HELD"
	start := time.Now()
	res := runShellCommand(context.Background(), script, t.TempDir())
	elapsed := time.Since(start)
	if res.start != nil || res.why != shellExited {
		t.Fatalf("ended start=%v why=%v", res.start, res.why)
	}
	if res.exit != 0 {
		// exec reports ErrWaitDelay rather than a status; the shell exited 0.
		t.Fatalf("exit = %d, want 0", res.exit)
	}
	if !strings.Contains(res.out, "CRAZE_HELD") {
		t.Fatalf("the output written before the shell exited is missing: %q", res.out)
	}
	if elapsed > shellWaitDelay+5*time.Second {
		t.Fatalf("a held pipe blocked completion for %s", elapsed)
	}
	if !waitMarker(t, marker, false, 10*time.Second) {
		t.Fatal("the child holding the pipe outlived the command")
	}
}

// TestShellRunCancelKillsTheGroup is what Esc and Ctrl+C reach.
func TestShellRunCancelKillsTheGroup(t *testing.T) {
	marker := shellMarker(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan shellResult, 1)
	go func() { done <- runShellCommand(ctx, sleeperScript(marker), t.TempDir()) }()
	if !waitMarker(t, marker, true, 10*time.Second) {
		t.Fatal("the command never started")
	}
	cancel()
	select {
	case res := <-done:
		if res.why != shellKilled {
			t.Fatalf("why = %v, want killed", res.why)
		}
	case <-time.After(shellTermGrace + shellKillWait + 5*time.Second):
		t.Fatal("a cancel did not end the command inside its own deadline")
	}
	if !waitMarker(t, marker, false, 10*time.Second) {
		t.Fatal("the cancelled command's group is still alive")
	}
}

func TestShellRunReportsANonZeroExit(t *testing.T) {
	res := runShellCommand(context.Background(), "echo out; echo err 1>&2; exit 3", t.TempDir())
	if res.start != nil || res.why != shellExited || res.exit != 3 {
		t.Fatalf("start=%v why=%v exit=%d", res.start, res.why, res.exit)
	}
	// Both streams, through one pipe, in the order the shell wrote them.
	if !strings.Contains(res.out, "out") || !strings.Contains(res.out, "err") {
		t.Fatalf("stdout and stderr both belong in the output, got %q", res.out)
	}
}

// TestShellRunUsesTheWorkspaceAndTheEnvironment: the session's directory and
// craze's own environment, with stdin at /dev/null so nothing can block on a
// terminal craze is drawing over.
func TestShellRunUsesTheWorkspaceAndTheEnvironment(t *testing.T) {
	ws := t.TempDir()
	real, err := filepath.EvalSymlinks(ws)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRAZE_SHELL_ENV_PROBE", "probe-value")
	res := runShellCommand(context.Background(), "pwd; echo $CRAZE_SHELL_ENV_PROBE; cat", ws)
	if res.start != nil || res.exit != 0 {
		t.Fatalf("start=%v exit=%d out=%q", res.start, res.exit, res.out)
	}
	if !strings.Contains(res.out, real) {
		t.Fatalf("pwd %q is not the workspace %q", res.out, real)
	}
	if !strings.Contains(res.out, "probe-value") {
		t.Fatalf("craze's environment did not reach the command: %q", res.out)
	}
	// `cat` returned, so stdin was /dev/null and not something that blocks.
}
