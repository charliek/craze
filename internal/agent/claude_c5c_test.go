package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/charliek/craze/internal/acp"
)

// Fixes from the sol re-reviews of C5b (items 1, 2, 5, 6, 9–11) and C4d
// (findings 1, 3).

// blockIO makes the record's I/O op on path block until released (at the
// test's end at the latest).
func blockIO(t *testing.T, op, path string) (release func()) {
	t.Helper()
	ch := make(chan struct{})
	var once sync.Once
	release = func() { once.Do(func() { close(ch) }) }
	hook := func(o, p string) {
		if o == op && p == path {
			<-ch
		}
	}
	prev := testOverageIO.Swap(&hook)
	t.Cleanup(func() { release(); testOverageIO.Store(prev) })
	return release
}

// TestClaudeOverageStallingFilesystem (review C5b item 1): a record write
// that never returns does not hold the stop past its bound, and a record read
// that never returns refuses — fails closed — within its bound.
func TestClaudeOverageStallingFilesystem(t *testing.T) {
	record, stopped := overageRig(t, 150*time.Millisecond)
	r, _ := startClaude(t, "claude-overage", Options{Interactive: true})
	releaseWrite := blockIO(t, "write", record)
	start := time.Now()
	ctx, cancel := context.WithTimeout(t.Context(), 6*time.Second)
	defer cancel()
	if _, err := r.s.Prompt(ctx, "MID"); !isQuota(err) {
		t.Fatalf("the trip: %v", err)
	}
	at := waitStopped(t, r, stopped, 5*time.Second)
	if took := at.Sub(start); took > overageWriteBound+3*time.Second {
		t.Fatalf("stopped %s after the trip; want the write bound and the grace", took)
	}
	// The disk answers again: the record lands then.
	releaseWrite()
	waitFor(t, "the late record", func() bool {
		_, err := os.Stat(record)
		return err == nil
	})

	prevBound := overageReadBound
	overageReadBound = 200 * time.Millisecond
	t.Cleanup(func() { overageReadBound = prevBound })
	blockIO(t, "read", record)
	start = time.Now()
	answer := make(chan error, 1)
	go func() { answer <- overageRefusal() }()
	select {
	case err := <-answer:
		if !isRefusal(err, record) || time.Since(start) > 2*time.Second {
			t.Fatalf("a check whose read stalls: %v after %s; want a refusal naming the record within the bound", err, time.Since(start))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a check whose read stalls never answered")
	}
}

// isRefusal reports a quota refusal naming path.
func isRefusal(err error, path string) bool {
	return err != nil && strings.Contains(err.Error(), path) && ErrorInfo(err).Kind == "quota"
}

// TestClaudeOverageFallbackOnlyInAPrivateDirectory (review C5b item 2): the
// fallback is written only into a real directory this user owns, which is
// made 0700; one that is a symlink or someone else's is refused, and the
// default is the user's runtime directory or nothing.
func TestClaudeOverageFallbackOnlyInAPrivateDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "craze")
	if err := checkFallbackDir(dir, true); err != nil {
		t.Fatalf("a fresh directory: %v", err)
	}
	if fi, _ := os.Stat(dir); fi.Mode().Perm() != 0o700 {
		t.Fatalf("mode %v; want 0700", fi.Mode().Perm())
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := checkFallbackDir(dir, false); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(dir); fi.Mode().Perm() != 0o700 {
		t.Fatalf("an open mode was left %v; want closed to 0700", fi.Mode().Perm())
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	if err := checkFallbackDir(link, true); err == nil {
		t.Fatal("a symlinked directory was taken")
	}
	prevUID := overageUID
	overageUID = func() int { return os.Getuid() + 1 }
	err := checkFallbackDir(dir, false)
	overageUID = prevUID
	if err == nil {
		t.Fatal("someone else's directory was taken")
	}
	t.Setenv("XDG_RUNTIME_DIR", "")
	if got := defaultOverageFallback("/x/claude-overage.json"); got != "" && !strings.HasPrefix(got, os.TempDir()) {
		t.Fatalf("with no runtime directory, the fallback is %s", got)
	}
}

// TestClaudeOverageRecordRefusesAFIFO (review C5b item 2): a FIFO at the
// record's name is refused at once — never waited on — and the check fails
// closed; a FIFO at the lock's name fails the write rather than block it.
func TestClaudeOverageRecordRefusesAFIFO(t *testing.T) {
	record, _ := overageRig(t, 0)
	if err := syscall.Mkfifo(record, 0o600); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := overageRefusal(); !isRefusal(err, record) || time.Since(start) > time.Second {
		t.Fatalf("a FIFO record: %v after %s; want a refusal at once", err, time.Since(start))
	}
	if err := os.Remove(record); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(record+".lock", 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- writeOverageRecordAt(record, overageRecord{Until: time.Now().Add(time.Hour), Window: "seven_day", At: time.Now()})
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a write through a FIFO lock succeeded")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a write blocked on a FIFO lock")
	}
	// And a symlink as the record is refused, not followed.
	if err := os.Remove(record + ".lock"); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "elsewhere.json")
	if err := os.WriteFile(target, []byte(`{"until":"2099-01-01T00:00:00Z"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, record); err != nil {
		t.Fatal(err)
	}
	if err := overageRefusal(); !isRefusal(err, record) || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("a symlinked record: %v; want refused as a symlink", err)
	}
}

// TestClaudeOverageWakeUnderBackpressure (review C5b item 5): a wake while
// the record holds is contained before anything is published: a stalled
// primary does not hold the check.
func TestClaudeOverageWakeUnderBackpressure(t *testing.T) {
	shortForeign(t, 5*time.Second)
	_, stopped := overageRig(t, 150*time.Millisecond)
	r := newClaudeRig(t, "claude-overage", Options{Interactive: true}, "CRAZE_FAKE_CLAUDE_IDLE_WAKE=400ms")
	var stall atomic.Bool
	release := make(chan struct{})
	var once sync.Once
	free := func() { once.Do(func() { close(release) }) }
	t.Cleanup(free)
	r.s.log.hooks = &logHooks{beforePrimarySend: func(uint64) {
		if stall.Load() {
			<-release
		}
	}}
	if err := r.s.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	go func() {
		for range r.s.Events() {
		}
	}()
	if _, _, err := writeOverageRecord(overageRecord{Until: time.Now().Add(time.Hour), Window: "seven_day", At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	stall.Store(true)
	waitStopped(t, r, stopped, 5*time.Second)
	free()
}

// TestClaudeOverageStderrDoesNotHoldTheStop (review C5b item 6): a stderr
// sink that blocks — the adapter said something during the trip — does not
// keep the stop from closing the client and stopping the session.
func TestClaudeOverageStderrDoesNotHoldTheStop(t *testing.T) {
	_, stopped := overageRig(t, 150*time.Millisecond)
	asked := make(chan string, 2)
	r := newClaudeRig(t, "claude-overage", Options{Interactive: true, Stopped: func(cause, _ string) { asked <- cause }})
	sink := &blockedWriter{release: make(chan struct{})}
	t.Cleanup(func() { close(sink.release) })
	r.s.opts.Stderr = sink
	if err := r.s.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	go func() {
		for range r.s.Events() {
		}
	}()
	go func() { _, _ = r.s.Prompt(context.Background(), "STDERR") }()
	waitStopped(t, r, stopped, 8*time.Second)
	select {
	case cause := <-asked:
		if cause != StopCauseOverage {
			t.Fatalf("stopped with %q", cause)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("the session never asked its host to stop: the stderr sink held the close")
	}
}

// TestClaudeStartModelClearsTheModelInUse (review C5b item 9): a report on
// Sonnet before the start's --model override is cleared by the override.
func TestClaudeStartModelClearsTheModelInUse(t *testing.T) {
	_, _ = overageRig(t, 150*time.Millisecond)
	r := newClaudeRig(t, "claude-usage", Options{Interactive: true, Model: "haiku"}, "CRAZE_FAKE_CLAUDE_SETUP_USAGE=1")
	if err := r.s.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	r.s.mu.Lock()
	model, under := r.s.usage.model, r.s.usage.underModel
	r.s.mu.Unlock()
	if model != "" {
		t.Fatalf("the model in use is %q (reported under %q) after --model haiku; want none", model, under)
	}
}

// blockingCounter counts the writes begun and blocks each until released.
type blockingCounter struct {
	n       atomic.Int32
	release chan struct{}
}

func (w *blockingCounter) Write(p []byte) (int, error) {
	w.n.Add(1)
	<-w.release
	return len(p), nil
}

// TestClaudeOverageRecordLinesCoalesce (review C5b item 10): a flood of
// lines about the record's whereabouts into a sink that blocks keeps one in
// flight, not one per trip.
func TestClaudeOverageRecordLinesCoalesce(t *testing.T) {
	r := newClaudeRig(t, "claude-basic", Options{})
	w := &blockingCounter{release: make(chan struct{})}
	t.Cleanup(func() { close(w.release) })
	r.s.opts.Diag = w
	for i := range 100 {
		r.s.fallbackLine(fmt.Sprintf("line %d", i))
	}
	time.Sleep(100 * time.Millisecond)
	if n := w.n.Load(); n != 1 {
		t.Fatalf("%d lines in flight; want 1", n)
	}
}

// TestClaudeOverageResetlessTripTwiceKeepsItsHold (review C5b item 11): a
// reset-less trip handled again later keeps the first one's hour.
func TestClaudeOverageResetlessTripTwiceKeepsItsHold(t *testing.T) {
	record, _ := overageRig(t, 0)
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	restore := SetClaudeOverageForTest(record, func() time.Time { return now }, 0)
	t.Cleanup(restore)
	r := newClaudeRig(t, "claude-basic", Options{})
	rl := acp.RateLimit{Status: "allowed", IsUsingOverage: true}
	_ = r.s.tripOverage(rl)
	now = now.Add(10 * time.Minute)
	_ = r.s.tripOverage(rl)
	if got := readRecord(t, record); !got.Until.Equal(time.Date(2026, 11, 17, 13, 0, 0, 0, time.UTC)) {
		t.Fatalf("until %s; want the first trip's hour", got.Until)
	}
}

// TestClaudeRateLimitDoesNotHoldTheDispatch (review C4d finding 1): a plan
// limit's usage delta, published from the read loop, does not wait for a
// stalled reader with the dispatch held.
func TestClaudeRateLimitDoesNotHoldTheDispatch(t *testing.T) {
	_, _ = overageRig(t, 150*time.Millisecond)
	r := newClaudeRig(t, "claude-overage", Options{Interactive: true}, "CRAZE_FAKE_CLAUDE_IDLE_LIMIT=1500ms")
	var stall atomic.Bool
	release := make(chan struct{})
	var once sync.Once
	free := func() { once.Do(func() { close(release) }) }
	t.Cleanup(free)
	r.s.log.hooks = &logHooks{outboxAdmitting: func(int) {
		if stall.Load() {
			<-release
		}
	}}
	if err := r.s.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	go func() {
		for range r.s.Events() {
		}
	}()
	// The adapter's setup traffic is read by the end of a turn.
	if _, err := r.s.Prompt(t.Context(), "hello"); err != nil {
		t.Fatal(err)
	}
	if err := r.s.log.Flush(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	stall.Store(true)
	waitFor(t, "the plan limit read", func() bool {
		r.s.mu.Lock()
		defer r.s.mu.Unlock()
		return r.s.usage.rl != nil
	})
	stop := make(chan struct{})
	time.AfterFunc(2*time.Second, func() { close(stop) })
	if !r.s.clientRef().JoinDispatch(stop) {
		t.Fatal("the dispatch is held by a usage delta waiting on a stalled reader")
	}
	free()
}

// TestClaudeStatelessRefusedAncestorsAreCapped (review C4d finding 3):
// ancestors the registry's cap refused, announced with no state, are born
// finished and go through the finished-row cap.
func TestClaudeStatelessRefusedAncestorsAreCapped(t *testing.T) {
	s, _ := claudeUnit(t)
	for i := range 200 {
		n := claudeChildUpdate(fmt.Sprintf("anc%d", i), "", nil)
		n.Unrouted = true
		s.onSubagent(n)
	}
	kept := 0
	for _, r := range s.Snapshot().Subagents {
		if strings.HasPrefix(r.ID, "anc") {
			kept++
		}
	}
	if kept > subagentFinishedCap {
		t.Fatalf("%d stateless refused ancestors kept; want at most %d", kept, subagentFinishedCap)
	}
}
