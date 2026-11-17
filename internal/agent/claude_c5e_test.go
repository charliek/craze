package agent

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charliek/craze/internal/acp"
	"github.com/charliek/craze/internal/hostspawn"
)

// Fixes from the sol re-review of C5d.

// TestRecordJoinFitsTheLauncherGrace (review C5d item 1): the close's wait for
// a record write fits inside the launcher's SIGTERM grace, so a host its
// launcher stops reaches the warning rather than being killed mid-wait.
func TestRecordJoinFitsTheLauncherGrace(t *testing.T) {
	if recordWriteDrain >= hostspawn.TermGrace {
		t.Fatalf("the record join (%v) is not below the launcher's grace (%v)", recordWriteDrain, hostspawn.TermGrace)
	}
}

// TestClaudeCloseFindsAStopThatBeganWhileItWaited (review C5d item 2): a close
// that begins while the trip's write is pending — before the stop exists —
// still waits for the stop the trip starts once its write bound passes, and
// for its kill.
func TestClaudeCloseFindsAStopThatBeganWhileItWaited(t *testing.T) {
	record, stopped := overageRig(t, 150*time.Millisecond)
	prevJoin := overageKillJoin
	overageKillJoin = 30 * time.Second
	t.Cleanup(func() { overageKillJoin = prevJoin })
	parked := make(chan struct{})
	releaseKill := make(chan struct{})
	var once sync.Once
	restore := acp.SetAfterSeverForTest(func() {
		once.Do(func() { close(parked) })
		<-releaseKill
	})
	t.Cleanup(restore)
	pids := filepath.Join(t.TempDir(), "pids")
	r, _ := startClaude(t, "claude-overage", Options{Interactive: true}, "CRAZE_FAKE_CHILD_PIDS="+pids)
	releaseWrite := blockIO(t, "write", record)
	go func() { _, _ = r.s.Prompt(context.Background(), "ESCAPE") }()
	t.Cleanup(func() { waitStopped(t, r, stopped, 10*time.Second) })
	waitFor(t, "the record write", func() bool {
		r.s.mu.Lock()
		defer r.s.mu.Unlock()
		return len(r.s.recordPending) > 0
	})
	// The close begins: no stop yet, only the write.
	closed := make(chan struct{})
	go func() { _ = r.s.Close(); close(closed) }()
	// The trip's write bound passes and the stop begins, parked after its
	// sever; then the write lands.
	<-parked
	releaseWrite()
	select {
	case <-closed:
		t.Fatal("the close ended before the kill of a stop that began while it waited")
	case <-time.After(5 * time.Second):
	}
	close(releaseKill)
	select {
	case <-closed:
	case <-time.After(8 * time.Second):
		t.Fatal("the close never ended")
	}
	waitChildrenGone(t, pids)
}

// TestClaudeResolutionHoldsNoLock (review C5d item 5): a record path
// resolution that stalls holds neither the guard's clock nor the session's
// close.
func TestClaudeResolutionHoldsNoLock(t *testing.T) {
	_, _ = overageRig(t, 0)
	stall := make(chan struct{})
	entered := make(chan struct{})
	var once sync.Once
	overageMu.Lock()
	prev := overagePath
	overagePath = func() string {
		once.Do(func() { close(entered) })
		<-stall
		return "/nowhere"
	}
	overageMu.Unlock()
	t.Cleanup(func() { overageMu.Lock(); overagePath = prev; overageMu.Unlock() })
	r := newClaudeRig(t, "claude-overage", Options{Interactive: true})
	collect(t, r.s)
	t.Cleanup(func() { close(stall) }) // ahead of the rig's own close
	go func() { _ = r.s.Start(context.Background()) }()
	<-entered
	clock := make(chan struct{})
	go func() { overageClock(); close(clock) }()
	select {
	case <-clock:
	case <-time.After(2 * time.Second):
		t.Fatal("the guard's clock waited on a stalled resolution")
	}
	closed := make(chan struct{})
	go func() { _ = r.s.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("the session's close waited on a stalled resolution")
	}
}

// blockingDiag blocks every write of a line holding match until released;
// other lines are kept.
type blockingDiag struct {
	lockedDiag
	match   string
	release chan struct{}
}

func (d *blockingDiag) Write(p []byte) (int, error) {
	if strings.Contains(string(p), d.match) {
		<-d.release
	}
	return d.lockedDiag.Write(p)
}

// TestClaudeRecordWarningDoesNotHoldTheClose (review C5d item 6): the record
// timeout's warning goes to a sink that blocks without holding the close.
func TestClaudeRecordWarningDoesNotHoldTheClose(t *testing.T) {
	record, stopped := overageRig(t, 150*time.Millisecond)
	prev := recordWriteDrain
	recordWriteDrain = 300 * time.Millisecond
	t.Cleanup(func() { recordWriteDrain = prev })
	r := newClaudeRig(t, "claude-overage", Options{Interactive: true})
	diag := &blockingDiag{match: "still being written", release: make(chan struct{})}
	t.Cleanup(func() { close(diag.release) })
	r.s.opts.Diag = diag
	collect(t, r.s)
	if err := r.s.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	release := blockIO(t, "write", record)
	ctx, cancel := context.WithTimeout(t.Context(), 6*time.Second)
	defer cancel()
	_, _ = r.s.Prompt(ctx, "MID")
	waitStopped(t, r, stopped, 5*time.Second)
	closed := make(chan struct{})
	go func() { _ = r.s.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(4 * time.Second):
		t.Fatal("the close waited on the warning's sink")
	}
	release()
	waitFor(t, "the late record", func() bool { rec, err := readOverageRecord(record); return err == nil && rec != nil })
}

// TestClaudeStalledReaderStopsTheSession (review C5d item 3): with nobody
// reading, a flood of output fills the outbox to its hard bound and no
// further: the agent is stopped while nothing reads, nothing blocks the read
// loop, and once a reader comes back the turn ends with why.
func TestClaudeStalledReaderStopsTheSession(t *testing.T) {
	r := newClaudeRig(t, "claude-overage", Options{Interactive: true})
	r.s.log.outboxMaxEvents = 64 // the hard bound is 4×: 256 events
	if err := r.s.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	client := r.s.client
	// Events() is not read: the primary fills, then the outbox.
	done := make(chan error, 1)
	go func() { _, err := r.s.Prompt(context.Background(), "FLOOD-ONLY"); done <- err }()
	select {
	case <-client.Conn().ReadDone():
	case <-time.After(10 * time.Second):
		t.Fatalf("the agent was never stopped (refused %d)", r.s.log.outboxRefused.Load())
	}
	r.s.log.outboxMu.Lock()
	held := r.s.log.outboxEvents
	r.s.log.outboxMu.Unlock()
	if held > outboxHardFactor*64 {
		t.Fatalf("the outbox holds %d events; want at most its hard bound %d", held, outboxHardFactor*64)
	}
	if r.s.log.outboxRefused.Load() == 0 {
		t.Fatal("nothing was refused")
	}
	// A reader comes back: the turn ends with the stop's error.
	go func() {
		for range r.s.Events() {
		}
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "event reader stalled") {
			t.Fatalf("the turn ended with %v; want the stalled reader's stop", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the turn never ended once read")
	}
}

// TestClaudeSaysWhatItsStderrSinkDropped (review C5d item 8): claude's
// stderr queue drops what a sink that is behind cannot take, and the session
// says how much, once, as it closes.
func TestClaudeSaysWhatItsStderrSinkDropped(t *testing.T) {
	r := newClaudeRig(t, "claude-overage", Options{Interactive: true})
	release := make(chan struct{})
	var once sync.Once
	free := func() { once.Do(func() { close(release) }) }
	t.Cleanup(free)
	// The child's stderr sink (the rig keeps no journal, so no tee).
	r.s.opts.Stderr = &blockingDiag{match: "line", release: release}
	r.s.tee = nil
	collect(t, r.s)
	if err := r.s.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	go func() { _, _ = r.s.Prompt(context.Background(), "STDERR-FLOOD") }()
	time.Sleep(1500 * time.Millisecond) // the 600 lines, while the sink is behind
	free()
	_ = r.s.Close()
	waitFor(t, "the drop's line", func() bool {
		return strings.Count(r.diag.String(), "chunks of the agent's stderr were dropped") == 1
	})
}
