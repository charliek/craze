package acp

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// Fixes from the sol re-reviews of C5b (items 3, 4, 8) and C4d (findings 1,
// 2): the kill's process identities and its reach when the agent exits first,
// the sever after a rate limit's handling, ReplayDone's dispatch wait, and the
// drain across an EOF.

// alive reports whether pid is a live process (not gone, not a zombie).
func alive(pid int) bool {
	if syscall.Kill(pid, 0) != nil {
		return false
	}
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	return err == nil && !strings.HasPrefix(strings.TrimSpace(string(out)), "Z")
}

func waitPidGone(t *testing.T, what string, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for alive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("%s (pid %d) is still alive", what, pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// entryOf is pid's process table entry, read now.
func entryOf(t *testing.T, pid int) treeEntry {
	t.Helper()
	table, err := procTable()
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range table {
		if e.pid == pid {
			return e
		}
	}
	t.Fatalf("pid %d is not in the process table", pid)
	return treeEntry{}
}

// TestKillVerifiedSparesAReusedPid (review C5b item 3): a snapshot entry whose
// pid is now another process — its start time differs — is not signalled;
// the same pid with its own start time is.
func TestKillVerifiedSparesAReusedPid(t *testing.T) {
	cmd := exec.Command("sleep", "600")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	go func() { _ = cmd.Wait() }()
	now := entryOf(t, cmd.Process.Pid)
	stale := now
	stale.start = "1"
	if killVerified(stale) {
		t.Fatal("a pid whose start time is not the snapshot's was signalled")
	}
	time.Sleep(100 * time.Millisecond)
	if !alive(cmd.Process.Pid) {
		t.Fatal("the process the pid now names was killed")
	}
	if !killVerified(now) {
		t.Fatal("the snapshot's own process was not signalled")
	}
	waitPidGone(t, "the snapshot's process", cmd.Process.Pid)
}

// TestSnapshotHoldsTheReap (review C5b item 3): the agent is not reaped while
// its tree is read, so its pid — the tree's root — cannot be reused under the
// read.
func TestSnapshotHoldsTheReap(t *testing.T) {
	c := spawnShell(t, `exit 0`, nil)
	parked := make(chan struct{})
	release := make(chan struct{})
	prev := testDuringSnapshot
	testDuringSnapshot = func() { close(parked); <-release }
	t.Cleanup(func() { testDuringSnapshot = prev })
	go c.child.SnapshotTree()
	<-parked
	waitClosed(t, c.child.exitedCh, 5*time.Second, "the agent's exit")
	time.Sleep(200 * time.Millisecond)
	if isClosed(c.child.reapedCh) {
		t.Fatal("the agent was reaped while its tree was read")
	}
	close(release)
	waitClosed(t, c.child.reapedCh, 5*time.Second, "the reap")
}

// escapeScript starts a child in the shell's group and one in a session of
// its own, says their pids, says ready, and exits after a moment.
const escapeScript = `sleep 600 & echo "g $!" >&2
perl -e 'use POSIX qw(setsid); setsid(); exec "sleep", "600"' & echo "e $!" >&2
echo ready >&2; sleep 0.5; exit 0`

func pidsFrom(t *testing.T, out string) (group, escaped int) {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 2 {
			n, _ := strconv.Atoi(f[1])
			switch f[0] {
			case "g":
				group = n
			case "e":
				escaped = n
			}
		}
	}
	if group == 0 || escaped == 0 {
		t.Fatalf("pids from %q", out)
	}
	t.Cleanup(func() { _ = syscall.Kill(group, syscall.SIGKILL); _ = syscall.Kill(escaped, syscall.SIGKILL) })
	return group, escaped
}

// TestStopWithinKillsWhatAnAgentThatExitedFirstLeft (review C5b item 4): the
// agent exits during the stop's grace, leaving a child in its group and one
// in a session of its own; both die — on the normal reaper and on the polling
// fallback, which does not clean the group at an exit of the agent's own.
func TestStopWithinKillsWhatAnAgentThatExitedFirstLeft(t *testing.T) {
	if _, err := exec.LookPath("perl"); err != nil {
		t.Skip("perl is not installed")
	}
	for _, fallback := range []bool{false, true} {
		t.Run("fallback="+strconv.FormatBool(fallback), func(t *testing.T) {
			if fallback {
				stubObserve(t, 0, syscall.ENOSYS, nil)
			}
			var stderr syncBuffer
			c := spawnShell(t, escapeScript, &stderr)
			stderr.waitFor(t, "ready")
			group, escaped := pidsFrom(t, stderr.String())
			done := make(chan struct{})
			go func() { c.StopWithin(3 * time.Second); close(done) }()
			waitClosed(t, done, 10*time.Second, "the stop")
			waitPidGone(t, "the child in the agent's group", group)
			waitPidGone(t, "the child in a session of its own", escaped)
		})
	}
}

// TestClaudeSeverAfterTheRateLimit (review C5b item 8): an update whose rate
// limit's handling ran the stop — sever and kill — is handed to no one.
func TestClaudeSeverAfterTheRateLimit(t *testing.T) {
	r := newClaudeRig(t)
	r.client.SetRateLimitHandler(func(rl RateLimit) error {
		r.client.conn.sever()
		return nil
	})
	r.ready(t)
	r.pushUpdate(t, rigRoot, usageWith(probe2RateLimit, false))
	time.Sleep(150 * time.Millisecond)
	if got := r.log.get(); slices.Contains(got, "usage_update") {
		t.Fatalf("an update reached the session after the sever its rate limit ran: %q", got)
	}
}

// TestClaudeReplayDoneGivesUpTheDispatchWithTheHold (review C4d finding 1):
// a read-loop handler holding the dispatch — a rate limit's, before the hold
// — does not keep ReplayDone past the hold's failure.
func TestClaudeReplayDoneGivesUpTheDispatchWithTheHold(t *testing.T) {
	r := newClaudeRig(t)
	parked := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	r.client.SetRateLimitHandler(func(RateLimit) error {
		close(parked)
		<-release
		return nil
	})
	done := make(chan error, 1)
	go func() {
		_, err := r.client.LoadSession(context.Background(), rigRoot, t.TempDir(), SessionParams{})
		done <- err
	}()
	req := r.readWithin(t, 3*time.Second, "session/load")
	r.answerWith(t, req, `{}`)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	r.pushUpdate(t, rigRoot, usageWith(probe2RateLimit, false))
	<-parked
	replayed := make(chan error, 1)
	go func() { replayed <- r.client.ReplayDone() }()
	time.Sleep(100 * time.Millisecond)
	r.client.mu.Lock()
	gen := r.client.cf.holdGen
	r.client.mu.Unlock()
	r.client.failHold(gen, ErrReplayHold)
	select {
	case err := <-replayed:
		if !errors.Is(err, ErrReplayHold) {
			t.Fatalf("ReplayDone = %v; want the hold's failure", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ReplayDone stayed behind a handler holding the dispatch after the hold failed")
	}
}

// TestClaudeDrainDeliversWhatCameBeforeEOF (review C4d finding 2): a child's
// announce, its last text and its idle ending, held behind a load's reply, are
// all delivered by the drain though the agent's output ends while it runs.
func TestClaudeDrainDeliversWhatCameBeforeEOF(t *testing.T) {
	r := newClaudeRig(t)
	done := make(chan error, 1)
	go func() {
		_, err := r.client.LoadSession(context.Background(), rigRoot, t.TempDir(), SessionParams{})
		done <- err
	}()
	req := r.readWithin(t, 3*time.Second, "session/load")
	r.answerWith(t, req, `{}`)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	r.pushUpdate(t, rigRoot, `{"sessionUpdate":"subagent_update","sessionId":"kid","title":"t","state":{"state":"running"}}`)
	r.pushUpdate(t, "kid", chunk("the last words"))
	r.pushUpdate(t, rigRoot, `{"sessionUpdate":"subagent_update","sessionId":"kid","state":{"state":"idle","stopReason":"end_turn"}}`)
	r.roundTrip(t)
	parked := make(chan struct{})
	release := make(chan struct{})
	prev := testDrainFrame
	first := true
	testDrainFrame = func() {
		if first {
			first = false
			close(parked)
			<-release
		}
	}
	t.Cleanup(func() { testDrainFrame = prev })
	replayed := make(chan error, 1)
	go func() { replayed <- r.client.ReplayDone() }()
	<-parked
	// The agent's output ends.
	if cl, ok := r.enc.w.(io.Closer); ok {
		_ = cl.Close()
	}
	waitClosed(t, r.client.conn.ReadDone(), 3*time.Second, "the read loop's end")
	close(release)
	if err := <-replayed; err != nil {
		t.Fatal(err)
	}
	got := strings.Join(r.log.get(), ",")
	if !strings.Contains(got, "agent_message_chunk@kid") || !strings.Contains(got, "sub kid idle") {
		t.Fatalf("the drain dropped what came before the EOF: %s", got)
	}
}

// TestKillTreeSendsNoGroupSignalAfterTheReap (review C5c item 4): once the
// agent has been reaped its group's id may be anyone's: no raw group signal
// goes, only the snapshot's checked members.
func TestKillTreeSendsNoGroupSignalAfterTheReap(t *testing.T) {
	var sent []int
	prev := groupKill
	groupKill = func(pid int, sig syscall.Signal) error {
		sent = append(sent, pid)
		return nil
	}
	t.Cleanup(func() { groupKill = prev })
	c := spawnShell(t, `exit 0`, nil)
	waitClosed(t, c.child.reapedCh, 5*time.Second, "the reap")
	c.child.KillTree(nil)
	if len(sent) != 0 {
		t.Fatalf("raw group signals %v after the agent was reaped", sent)
	}
	// While the agent is unreaped, the group is signalled.
	r := spawnShell(t, `sleep 60`, nil)
	r.child.KillTree(nil)
	if len(sent) != 1 || sent[0] != -r.child.pgid {
		t.Fatalf("group signals %v for an unreaped agent; want one to -%d", sent, r.child.pgid)
	}
}

// TestClaudeSeverWhileTellingAnEviction (review C5c item 9): a stop that
// severs while the session is being told of an evicted child keeps the new
// child's announce from it.
func TestClaudeSeverWhileTellingAnEviction(t *testing.T) {
	r := newClaudeRig(t)
	var newSeen atomic.Bool
	r.client.SetSubagentHandler(func(n SubagentNotification) {
		if n.Evicted {
			r.client.conn.sever()
		}
		if n.ChildSessionID == "newcomer" {
			newSeen.Store(true)
		}
	})
	r.ready(t)
	for i := range childRouteCap {
		r.pushOn(t, rigRoot, `{"sessionUpdate":"subagent_update","sessionId":"idle`+strconv.Itoa(i)+`","state":{"state":"idle","stopReason":"end_turn"}}`)
	}
	r.pushUpdate(t, rigRoot, `{"sessionUpdate":"subagent_update","sessionId":"newcomer","state":{"state":"running"}}`)
	time.Sleep(150 * time.Millisecond)
	if newSeen.Load() {
		t.Fatal("the new child's announce reached the session after the sever")
	}
}

// TestStderrCopyRetiresBehindABlockedSink (review C5c item 11): on claude, a
// sink that never returns leaves the copy from the agent's pipe retired
// within the cap.
func TestStderrCopyRetiresBehindABlockedSink(t *testing.T) {
	prev := stderrWaitBound
	stderrWaitBound = 300 * time.Millisecond
	t.Cleanup(func() { stderrWaitBound = prev })
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	sink := writerFunc(func(p []byte) (int, error) { <-release; return len(p), nil })
	c, err := Spawn(SpawnOptions{Binary: "/bin/sh", Args: []string{"-c", `echo one >&2; echo two >&2; exit 0`}, Stderr: sink, Dialect: DialectClaude})
	if err != nil {
		t.Fatal(err)
	}
	// The agent has written and exited on its own: the copy holds its words.
	waitClosed(t, c.child.exitedCh, 5*time.Second, "the agent's exit")
	_ = c.Close()
	waitClosed(t, c.child.stderrDone, 3*time.Second, "the stderr copy's retirement")
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// TestKillTreeReachesWhatACapturedMemberStartedAfterTheExit (review C5d item
// 7): on the polling reaper, a group member the stop's first snapshot caught
// starts another process during the grace and exits, and then the agent
// exits and is reaped: the late process, left in the group, is found in the
// process table and killed too.
func TestKillTreeReachesWhatACapturedMemberStartedAfterTheExit(t *testing.T) {
	stubObserve(t, 0, syscall.ENOSYS, nil)
	var stderr syncBuffer
	// The member forks the late process and exits: it is nobody's
	// descendant, only the group's.
	c := spawnShell(t, `sh -c 'sleep 0.4; sleep 600 & echo "b $!" >&2; exit 0' &
echo ready >&2; sleep 0.8; exit 0`, &stderr)
	stderr.waitFor(t, "ready")
	done := make(chan struct{})
	go func() { c.StopWithin(3 * time.Second); close(done) }()
	stderr.waitFor(t, "b ")
	var late int
	for _, line := range strings.Split(stderr.String(), "\n") {
		if f := strings.Fields(line); len(f) == 2 && f[0] == "b" {
			late, _ = strconv.Atoi(f[1])
		}
	}
	if late == 0 {
		t.Fatalf("no late pid in %q", stderr.String())
	}
	t.Cleanup(func() { _ = syscall.Kill(late, syscall.SIGKILL) })
	waitClosed(t, done, 10*time.Second, "the stop")
	waitPidGone(t, "the process a captured member started after the snapshot", late)
}

// TestKillTreeReachesWhatAnEscapedMemberStartedAfterTheExit (review C5d item
// 7): a captured process in a session of its own starts another during the
// grace, and then the agent exits and is reaped: the late process, a
// descendant of a captured one, is killed too.
func TestKillTreeReachesWhatAnEscapedMemberStartedAfterTheExit(t *testing.T) {
	if _, err := exec.LookPath("perl"); err != nil {
		t.Skip("perl is not installed")
	}
	stubObserve(t, 0, syscall.ENOSYS, nil)
	inner := filepath.Join(t.TempDir(), "inner.sh")
	if err := os.WriteFile(inner, []byte("sleep 0.4; sleep 600 & echo \"d $!\" >&2; wait\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	var stderr syncBuffer
	c := spawnShell(t, `perl -e 'use POSIX qw(setsid); setsid(); exec "sh", "`+inner+`"' &
echo ready >&2; sleep 0.8; exit 0`, &stderr)
	stderr.waitFor(t, "ready")
	done := make(chan struct{})
	go func() { c.StopWithin(3 * time.Second); close(done) }()
	stderr.waitFor(t, "d ")
	var late int
	for _, line := range strings.Split(stderr.String(), "\n") {
		if f := strings.Fields(line); len(f) == 2 && f[0] == "d" {
			late, _ = strconv.Atoi(f[1])
		}
	}
	if late == 0 {
		t.Fatalf("no late pid in %q", stderr.String())
	}
	t.Cleanup(func() { _ = syscall.Kill(late, syscall.SIGKILL) })
	waitClosed(t, done, 10*time.Second, "the stop")
	waitPidGone(t, "the process an escaped member started after the snapshot", late)
}

// TestStderrSinkQueueIsClaudesAlone (review C5d item 8): every other dialect's
// copy waits for a sink that is behind and loses nothing; claude's queue drops
// what it cannot hold, and counts it.
func TestStderrSinkQueueIsClaudesAlone(t *testing.T) {
	// 600 lines of about 33KB each, by the shell's own echo: the copy reads at
	// most 32KB at a time, so the ~20MB arrive as some 600 chunks however the
	// reads coalesce, well past the queue's 256 (asyncSinkQueue), with no
	// timing in it. It used to space short lines with an external sleep 0.001
	// per line, 600 fork-execs that took 6s on an idle Mac and passed the 10s
	// bound on a loaded macOS runner under -race.
	script := `pad=$(printf "%033000d" 0); i=0; while [ $i -lt 600 ]; do echo "line $i $pad" >&2; i=$((i+1)); done; exit 0`
	for _, d := range []DialectID{"", DialectClaude} {
		t.Run("dialect="+string(d), func(t *testing.T) {
			release := make(chan struct{})
			var got syncBuffer
			var once atomic.Bool
			sink := writerFunc(func(p []byte) (int, error) {
				if once.CompareAndSwap(false, true) {
					<-release // behind for a while, from the first write
				}
				return got.Write(p)
			})
			c, err := Spawn(SpawnOptions{Binary: "/bin/sh", Args: []string{"-c", script}, Stderr: sink, Dialect: d})
			if err != nil {
				t.Fatal(err)
			}
			if d == DialectClaude {
				// The agent writes everything while the sink is behind.
				waitClosed(t, c.child.exitedCh, 10*time.Second, "the agent's exit")
				close(release)
			} else {
				// Held back by the sink: let go once it has been for a while.
				time.Sleep(500 * time.Millisecond)
				close(release)
				waitClosed(t, c.child.exitedCh, 10*time.Second, "the agent's exit")
			}
			_ = c.Close()
			lines := strings.Count(got.String(), "\n")
			switch {
			case d != DialectClaude && (lines != 600 || c.StderrDropped() != 0):
				t.Fatalf("%d lines reached the sink, %d chunks dropped; want all 600 and none", lines, c.StderrDropped())
			case d == DialectClaude && (c.StderrDropped() == 0 || lines >= 600):
				t.Fatalf("%d lines reached the sink, %d chunks dropped; want the queue's overflow dropped and counted", lines, c.StderrDropped())
			}
		})
	}
}
