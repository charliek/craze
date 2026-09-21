package tui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
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
// marker in its own argv — and a child of its own, under it, carrying
// leafMarker(marker). Two generations because one would not be a test: a runner
// that killed the process it started and left everything below it running would
// pass every assertion a single marked child can make, and killing the whole
// group is the entire point of this file.
//
// The loops matter: `sh -c 'sleep 30' marker` would be exec-optimised by dash
// and bash into a bare `sleep 30`, which carries no marker at all and would
// make every assertion below vacuous. A shell that has a loop to run cannot
// replace itself with anything. (`sleep` itself cannot carry one: it sums its
// arguments, so an extra word is an error and not a marker.)
//
// The leaf's marker is built at run time out of $0, so that it appears in the
// leaf's own command line and nowhere above it: every ancestor's argv holds the
// script that says `"$0"leaf`, not what it expands to. So a pgrep for the leaf
// marker finds the grandchild and only the grandchild, while a pgrep for marker
// — a substring of it — finds either.
func sleeperScript(marker string) string {
	const loop = `while :; do sleep 1; done`
	return `/bin/sh -c 'L="$0"leaf; /bin/sh -c "` + loop + `" "$L" & ` + loop + `' ` + marker
}

// leafMarker names the grandchild sleeperScript's shell starts. Waiting for it
// is how a test proves the whole line reached exec before it asserts anything
// about killing it.
func leafMarker(marker string) string { return marker + "leaf" }

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

// shellGate is a point a script can be held at until the test lets it past: it
// polls for a file, and openGate writes one. It is how a test proves what was
// running *before* the thing it is about to assert on — a background child that
// reached exec before its shell exited, say — rather than asserting on a
// command that may never have got that far.
//
// A poll and not a fifo: opening a fifo blocks both ends, so a shell that died
// early would hang the test instead of failing it. The whole-second sleep is
// what every /bin/sh has; the wait costs at most that.
func shellGate(gate string) string {
	return `while [ ! -f "` + gate + `" ]; do sleep 1; done`
}

func openGate(t *testing.T, gate string) {
	t.Helper()
	if err := os.WriteFile(gate, []byte("go\n"), 0o644); err != nil {
		t.Fatal(err)
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
	// "Never grows" is true by construction, and this is what says so: the
	// ring's storage is an array inside the ring, so whatever it is written it
	// is this many bytes and not one more. A ring that grew a slice instead
	// would pass every assertion below and fail here.
	if got, max := reflect.TypeOf(shellRing{}).Size(), uintptr(shellOutputCap)+64; got > max {
		t.Fatalf("a shellRing is %d bytes, want the %d-byte buffer and a header, under %d", got, shellOutputCap, max)
	}

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

	// Exactly the cap, into an empty ring: the fast path takes it whole, and
	// nothing was dropped — output the size of the cap to the byte must not be
	// led by a note about a head that is right there.
	exact := bytes.Repeat([]byte("e"), shellOutputCap)
	r = &shellRing{}
	if _, err := r.Write(exact); err != nil {
		t.Fatal(err)
	}
	if got, over := r.text(); over || got != string(exact) {
		t.Fatalf("a write of exactly the cap into an empty ring: len=%d over=%v, want the whole of it and nothing dropped", len(got), over)
	}
	// One byte in front of it, and the same write does drop that byte.
	r = &shellRing{}
	if _, err := r.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Write(exact); err != nil {
		t.Fatal(err)
	}
	if got, over := r.text(); !over || got != string(exact) {
		t.Fatalf("a byte then exactly the cap: len=%d over=%v, want the cap kept and the byte reported dropped", len(got), over)
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
// the ring rather than a trim: what a run allocates must not follow what the
// command printed, so a buffer that grew first and trimmed at the end fails
// here even though its answer would be the same string.
//
// The instrument is TotalAlloc, which only ever rises, so no collection can
// make a run that allocated a flood look thrifty. floodAllocCap is the bound:
// generous enough for the 32 KiB copy buffer, the ring's own answer and the
// test's, and a small multiple of that for whatever else the process does in
// the window — and an order of magnitude under the smallest of the two floods.
func TestShellRunBoundsAnEndlessFlood(t *testing.T) {
	const (
		floodBytes    = 64 << 20 // what the measured command writes, exactly
		floodAllocCap = 4 << 20
	)

	t.Run("a measured 64 MiB", func(t *testing.T) {
		// A command that ends by itself, so the size is not an estimate: head
		// stops at the byte, and `yes` takes the SIGPIPE.
		script := fmt.Sprintf("yes CRAZE_FLOOD_PAYLOAD | head -c %d; echo CRAZE_FLOOD_DONE", floodBytes)
		before := totalAlloc()
		res := runShellCommand(context.Background(), script, t.TempDir())
		grew := totalAlloc() - before
		if res.start != nil || res.why != shellExited {
			t.Fatalf("the flood ended start=%v why=%v", res.start, res.why)
		}
		if !strings.Contains(res.out, "CRAZE_FLOOD_DONE") {
			t.Fatalf("the command did not run to its end: %q", res.out)
		}
		if grew > floodAllocCap {
			t.Fatalf("reading %d MiB allocated %d MiB; the ring is fixed-size, so this run kept what it read",
				floodBytes>>20, grew>>20)
		}
		if max := shellOutputCap + len(shellTruncNote) + 1; len(res.out) > max {
			t.Fatalf("output is %d bytes, cap is %d", len(res.out), max)
		}
	})

	t.Run("an endless one, cut short by the timeout", func(t *testing.T) {
		// `yes` never stops, so this is the same bound held while the command
		// is still running rather than after it ended.
		shortShellTimeout(t, 400*time.Millisecond)
		start := time.Now()
		before := totalAlloc()
		res := runShellCommand(context.Background(), "yes CRAZE_FLOOD_PAYLOAD", t.TempDir())
		grew := totalAlloc() - before
		if res.why != shellTimedOut {
			t.Fatalf("an endless command must end on the timeout, got why=%v exit=%d", res.why, res.exit)
		}
		if elapsed := time.Since(start); elapsed > shellTermGrace+shellKillWait+5*time.Second {
			t.Fatalf("the timeout took %s to end it", elapsed)
		}
		if grew > floodAllocCap {
			t.Fatalf("an endless flood allocated %d MiB in %s", grew>>20, time.Since(start))
		}
		if max := shellOutputCap + len(shellTruncNote) + 1; len(res.out) > max {
			t.Fatalf("output is %d bytes, cap is %d", len(res.out), max)
		}
		if !strings.Contains(res.out, "CRAZE_FLOOD_PAYLOAD") {
			t.Fatalf("the flood produced nothing craze kept: %q", res.out)
		}
	})
}

// totalAlloc is every byte this process has allocated since it started. It
// never falls, so a difference across a run is what that run allocated and a
// collection in the middle cannot hide anything.
func totalAlloc() uint64 {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return ms.TotalAlloc
}

// TestShellRunTimeoutKillsTheGroup: the ceiling does not only stop the shell,
// it takes everything the shell started with it.
//
// The run goes on a goroutine of its own so that the test can watch the
// timeout happen *to* a command it has seen running. Asserting the marker is
// gone afterwards is worth nothing on its own — it is gone before the command
// starts too — so the wait for the grandchild comes first, and the ceiling is
// set well above the time that wait takes on any box that is not broken.
func TestShellRunTimeoutKillsTheGroup(t *testing.T) {
	shortShellTimeout(t, 2*time.Second)
	marker := shellMarker(t)
	done := make(chan shellResult, 1)
	go func() {
		done <- runShellCommand(context.Background(), sleeperScript(marker)+" & sleep 60", t.TempDir())
	}()
	if !waitMarker(t, leafMarker(marker), true, 2*time.Second) {
		t.Fatal("the marked grandchild never reached exec before the ceiling")
	}
	select {
	case res := <-done:
		if res.why != shellTimedOut {
			t.Fatalf("why = %v, want the timeout", res.why)
		}
	case <-time.After(shellTermGrace + shellKillWait + 10*time.Second):
		t.Fatal("the ceiling did not end the command inside its own deadline")
	}
	// The marker matches the grandchild's own command line too, so this is
	// every generation and not only the one craze started.
	if !waitMarker(t, marker, false, 10*time.Second) {
		t.Fatal("the timeout left the background child running")
	}
}

// TestShellRunBackgroundChildDiesWithItsShell is `sleep 300 &`: the shell
// itself exits at once and cleanly, and craze still kills the group — the
// unconditional SIGKILL on every ending is the whole point of this test.
//
// As above, the background line has to be *running* before the shell that
// started it exits, or the kill would have nothing to prove. So the run goes on
// its own goroutine, and what lets the shell exit is the test, once it has seen
// the grandchild.
func TestShellRunBackgroundChildDiesWithItsShell(t *testing.T) {
	marker := shellMarker(t)
	// Output redirected away, so nothing holds the pipe: this is the path where
	// Wait returns immediately and only the final kill is left to do it. The
	// gate is the test saying "the child is up, you may exit now".
	gate := filepath.Join(t.TempDir(), "gate")
	script := sleeperScript(marker) + " >/dev/null 2>&1 & echo CRAZE_STARTED; " + shellGate(gate)
	done := make(chan shellResult, 1)
	go func() { done <- runShellCommand(context.Background(), script, t.TempDir()) }()
	if !waitMarker(t, leafMarker(marker), true, 20*time.Second) {
		t.Fatal("the marked grandchild never reached exec")
	}
	start := time.Now()
	openGate(t, gate)
	res := <-done
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

// TestShellRunDescendantHoldingThePipeDoesNotBlock is the held-pipe case: the
// shell has exited but a child it left behind still holds the output pipe, so
// exec's copying goroutine would block for as long as that child lives. Two
// things keep it from doing that — the group's last SIGKILL, which goes out
// before the leader is reaped and so before exec's Wait joins that goroutine,
// and cmd.WaitDelay behind it for a holder that left the group and no signal of
// craze's can reach.
func TestShellRunDescendantHoldingThePipeDoesNotBlock(t *testing.T) {
	marker := shellMarker(t)
	// No redirection this time: the child inherits craze's end of the pipe. The
	// gate again, for the same reason: a child that had not reached exec when
	// its shell exited would be holding nothing, and the block this is about
	// could not happen.
	gate := filepath.Join(t.TempDir(), "gate")
	script := sleeperScript(marker) + " & echo CRAZE_HELD; " + shellGate(gate)
	done := make(chan shellResult, 1)
	go func() { done <- runShellCommand(context.Background(), script, t.TempDir()) }()
	if !waitMarker(t, leafMarker(marker), true, 20*time.Second) {
		t.Fatal("the marked grandchild never reached exec")
	}
	start := time.Now()
	openGate(t, gate)
	res := <-done
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
	// The grandchild, not the shell craze started: a cancel that reached only
	// the process it knows about is exactly the failure this is looking for.
	if !waitMarker(t, leafMarker(marker), true, 10*time.Second) {
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

// TestShellLeaderIsNotReapedBeforeTheFinalSignal is the pid-reuse fix.
//
// The run's last act is a SIGKILL to its process group, and a group's id is its
// leader's pid: reap the leader first and that id can have been handed to a
// stranger's group by the time the signal goes out. So the leader is held
// unreaped until the run releases it, and this is the order, asserted where the
// runner performs it — on the group itself, because the window it closes is
// microseconds wide in the runner and cannot be raced from a test.
//
// On Linux that hold is waitid(WNOWAIT) and the leader is a zombie when the
// signal goes out, which is what the assertions below read out of the process
// table. On macOS there is no such hold (waitNoReap), the leader is reaped
// first, and the guarantee is the narrower one watch documents: this test says
// so rather than pretending otherwise.
func TestShellLeaderIsNotReapedBeforeTheFinalSignal(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "exit 7")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	g, err := startShellGroup(cmd)
	if err != nil {
		t.Fatal(err)
	}
	<-g.exited
	switch {
	case g.pinned:
		// The leader has exited and has not been reaped, so its pid — and with
		// it the group id the final signal is about to name — is still this
		// command's and cannot have been given to anything else.
		if st := procState(t, g.pid); st != "Z" {
			t.Fatalf("the leader is in state %q, want the Z that holds the group id", st)
		}
		if err := syscall.Kill(-g.pid, 0); err != nil {
			t.Fatalf("the group is already gone before the final signal: %v", err)
		}
	case runtime.GOOS == "linux":
		t.Fatal("waitid(WNOWAIT) is how Linux holds the leader unreaped, and it did not; " +
			"the run's last SIGKILL can now land on a group id somebody else was given")
	default:
		// macOS, and a Linux waitid that failed: the reap has happened, so the
		// pid may already be free. Accepted, narrow, and watch says why.
		t.Logf("%s does not hold the leader unreaped; the final signal follows the reap", runtime.GOOS)
	}

	// What runShellCommand does next, in this order.
	g.signal(syscall.SIGKILL)
	close(g.release)
	<-g.reaped
	if got := shellExitCode(g.err); got != 7 {
		t.Fatalf("exit = %d after the release, want the 7 the leader exited with", got)
	}
}

// procState is the process's state letter as the kernel reports it: "Z" for a
// zombie, the state a leader waits in between its exit and its reap.
func procState(t *testing.T, pid int) string {
	t.Helper()
	if runtime.GOOS != "linux" {
		out, err := exec.Command("ps", "-o", "state=", "-p", strconv.Itoa(pid)).Output()
		if err != nil {
			t.Fatalf("ps: %v", err)
		}
		// BSD ps states carry flags after the letter ("Z+"); the letter is the
		// state.
		if s := strings.TrimSpace(string(out)); s != "" {
			return s[:1]
		}
		return ""
	}
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		t.Fatalf("read /proc/%d/stat: %v", pid, err)
	}
	// The second field is the executable's name in parentheses and may hold
	// anything, spaces and parentheses included, so the state is read from
	// after the last ')' rather than from the third space-separated field.
	rest := string(b[strings.LastIndexByte(string(b), ')')+1:])
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		t.Fatalf("/proc/%d/stat: no state in %q", pid, b)
	}
	return fields[0]
}

// TestShellRunReapsItsLeaderOnEveryEnding is the other half of holding the
// leader: whatever ended the command, the run releases it again on its way out.
// A path that forgot to would leave the watcher blocked on that channel for
// good — a goroutine per run, and a zombie holding the pid with it — which is
// the leak the hold could have introduced and the process table is the honest
// place to look for it.
func TestShellRunReapsItsLeaderOnEveryEnding(t *testing.T) {
	marker := shellMarker(t)
	before := zombieChildren(t)

	// Ended by itself.
	runShellCommand(context.Background(), "echo one", t.TempDir())
	// Ended by itself with a background child left in the group, so the final
	// kill is what ends the run.
	runShellCommand(context.Background(), sleeperScript(marker)+" >/dev/null 2>&1 &", t.TempDir())
	// Ended by a cancel, the path where the signals come first.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan shellResult, 1)
	go func() { done <- runShellCommand(ctx, sleeperScript(marker), t.TempDir()) }()
	if !waitMarker(t, leafMarker(marker), true, 10*time.Second) {
		t.Fatal("the command never started")
	}
	cancel()
	<-done

	// The reap happens on the watcher's own goroutine just after the run
	// returns, so this is a wait and not a glance.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if n := zombieChildren(t); n <= before {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d zombie children are left, was %d before the runs: a leader was never reaped",
				zombieChildren(t), before)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !waitMarker(t, marker, false, 10*time.Second) {
		t.Fatal("a run left its group behind")
	}
}

// zombieChildren counts this process's children that have exited and not been
// reaped. ps and not /proc because both systems have it and the one question
// asked of it — parent and state — is in POSIX's own column names.
func zombieChildren(t *testing.T) int {
	t.Helper()
	out, err := exec.Command("ps", "-o", "ppid=,state=", "-ax").Output()
	if err != nil {
		t.Fatalf("ps: %v", err)
	}
	mine, n := strconv.Itoa(os.Getpid()), 0
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[0] == mine && strings.HasPrefix(f[1], "Z") {
			n++
		}
	}
	return n
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
