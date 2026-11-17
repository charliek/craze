package agent

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/charliek/craze/internal/acp"
	"github.com/charliek/craze/internal/atomicfile"
)

// Fixes from the sol re-review of C5c (items 1–3, 5–8, 10) and L5 (F1).

// TestClaudeCloseWaitsForAPendingRecord (review C5c item 1): a session closed
// while its trip record is still being written waits for the write, and past
// the bound says on its diagnostic lane which record may not hold.
func TestClaudeCloseWaitsForAPendingRecord(t *testing.T) {
	record, stopped := overageRig(t, 150*time.Millisecond)
	r, _ := startClaude(t, "claude-overage", Options{Interactive: true})
	release := blockIO(t, "write", record)
	ctx, cancel := context.WithTimeout(t.Context(), 6*time.Second)
	defer cancel()
	if _, err := r.s.Prompt(ctx, "MID"); !isQuota(err) {
		t.Fatalf("the trip: %v", err)
	}
	waitStopped(t, r, stopped, 5*time.Second)
	closed := make(chan struct{})
	go func() { _ = r.s.Close(); close(closed) }()
	select {
	case <-closed:
		t.Fatal("the session closed with its record still being written")
	case <-time.After(300 * time.Millisecond):
	}
	release()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("the close never ended")
	}
	readRecord(t, record)
}

// TestClaudeCloseBoundsAStalledRecord: past its bound the close goes on, and
// names the record that may not hold.
func TestClaudeCloseBoundsAStalledRecord(t *testing.T) {
	record, stopped := overageRig(t, 150*time.Millisecond)
	prev := recordWriteDrain
	recordWriteDrain = 300 * time.Millisecond
	t.Cleanup(func() { recordWriteDrain = prev })
	r, _ := startClaude(t, "claude-overage", Options{Interactive: true})
	release := blockIO(t, "write", record)
	ctx, cancel := context.WithTimeout(t.Context(), 6*time.Second)
	defer cancel()
	_, _ = r.s.Prompt(ctx, "MID")
	waitStopped(t, r, stopped, 5*time.Second)
	start := time.Now()
	_ = r.s.Close()
	if took := time.Since(start); took < 250*time.Millisecond || took > 3*time.Second {
		t.Fatalf("the close took %s; want the record's bound", took)
	}
	waitFor(t, "the warning", func() bool {
		return strings.Contains(r.diag.String(), "still being written") && strings.Contains(r.diag.String(), record)
	})
	// The abandoned write lands once released, before the directory goes.
	release()
	waitFor(t, "the late record", func() bool { rec, err := readOverageRecord(record); return err == nil && rec != nil })
}

// TestClaudeCloseJoinsTheKill (review C5c item 2): a close that comes while
// the overage stop has severed and not yet killed waits for the kill, so the
// process that exits after it leaves no escaped descendant alive.
func TestClaudeCloseJoinsTheKill(t *testing.T) {
	_, stopped := overageRig(t, 150*time.Millisecond)
	prevJoin := overageKillJoin
	overageKillJoin = 30 * time.Second
	t.Cleanup(func() { overageKillJoin = prevJoin })
	parked := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	restore := acp.SetAfterSeverForTest(func() {
		once.Do(func() { close(parked) })
		<-release
	})
	t.Cleanup(restore)
	pids := filepath.Join(t.TempDir(), "pids")
	r, _ := startClaude(t, "claude-overage", Options{Interactive: true}, "CRAZE_FAKE_CHILD_PIDS="+pids)
	go func() { _, _ = r.s.Prompt(context.Background(), "ESCAPE") }()
	t.Cleanup(func() { waitStopped(t, r, stopped, 10*time.Second) }) // before the rig's seams go
	<-parked
	closed := make(chan struct{})
	go func() { _ = r.s.Close(); close(closed) }()
	// Longer than the client's own shutdown (its grace, then its group's):
	// a close that did not wait for the kill has returned by now, the
	// escaped child alive.
	select {
	case <-closed:
		t.Fatal("the close ended before the stop's kill")
	case <-time.After(5 * time.Second):
	}
	close(release)
	select {
	case <-closed:
	case <-time.After(8 * time.Second):
		t.Fatal("the close never ended")
	}
	raw, _ := os.ReadFile(pids)
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		pid, _ := strconv.Atoi(f[1])
		t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
		deadline := time.Now().Add(3 * time.Second)
		for !processGone(pid) {
			if time.Now().After(deadline) {
				t.Fatalf("the %s child outlived the close", f[0])
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
}

// TestClaudeTripUsesThePathsResolvedAtStart (review C5c item 3a): once
// started, a trip resolves no path — a working directory that stalls holds
// the start, never the containment.
func TestClaudeTripUsesThePathsResolvedAtStart(t *testing.T) {
	record, stopped := overageRig(t, 150*time.Millisecond)
	r, _ := startClaude(t, "claude-overage", Options{Interactive: true})
	stall := make(chan struct{})
	overageMu.Lock()
	prev := overagePath
	overagePath = func() string { <-stall; return "/nowhere" }
	overageMu.Unlock()
	t.Cleanup(func() { overageMu.Lock(); overagePath = prev; overageMu.Unlock() })
	t.Cleanup(func() { close(stall) }) // first: anything parked lets the mutex go
	go func() { _, _ = r.s.Prompt(context.Background(), "MID") }()
	waitStopped(t, r, stopped, 5*time.Second)
	readRecord(t, record)
}

// TestClaudeFallbackCheckFailsClosed (review C5c item 3b): a fallback whose
// directory check stalls, or fails for any reason but not existing, refuses;
// one whose directory does not exist is no fallback.
func TestClaudeFallbackCheckFailsClosed(t *testing.T) {
	record, _ := overageRig(t, 0)
	prevBound := overageReadBound
	overageReadBound = 200 * time.Millisecond
	t.Cleanup(func() { overageReadBound = prevBound })
	dir := filepath.Join(t.TempDir(), "fb")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	fallback := filepath.Join(dir, "fallback.json")
	at := overagePaths{primary: record, fallback: fallback}
	// Stalled: the check is held until the refusal is in, then let go and
	// waited for — it reads the seams this test changes next.
	release := make(chan struct{})
	finished := make(chan struct{})
	hook := func(op, p string) {
		switch {
		case op == "checkdir" && p == fallback:
			<-release
		case op == "read" && p == fallback:
			close(finished)
		}
	}
	prevIO := testOverageIO.Swap(&hook)
	err := overageRefusalAt(at)
	close(release)
	<-finished
	testOverageIO.Store(prevIO)
	if !isRefusal(err, fallback) {
		t.Fatalf("a stalled fallback check: %v; want a refusal naming it", err)
	}
	// Not this user's.
	prevUID := overageUID
	overageUID = func() int { return os.Getuid() + 1 }
	err = overageRefusalAt(at)
	overageUID = prevUID
	if !isRefusal(err, fallback) {
		t.Fatalf("someone else's fallback directory: %v; want a refusal naming it", err)
	}
	// Missing: no fallback.
	if err := overageRefusalAt(overagePaths{primary: record, fallback: filepath.Join(t.TempDir(), "none", "fallback.json")}); err != nil {
		t.Fatalf("a missing fallback directory: %v; want no fallback", err)
	}
}

// TestClaudeRecordWritesAreDurable (review C5c item 7): the record is
// replaced with the durable write — the file synced before the rename, the
// directory after.
func TestClaudeRecordWritesAreDurable(t *testing.T) {
	if reflect.ValueOf(overageWriteFile).Pointer() != reflect.ValueOf(atomicfile.WriteSync).Pointer() {
		t.Fatal("the record is not written with atomicfile.WriteSync")
	}
}

// TestClaudeConfigUpdateDoesNotHoldTheTrip (review C5c item 5): with
// publication stalled, a config update the read loop publishes does not keep
// it from the tripping usage behind it.
func TestClaudeConfigUpdateDoesNotHoldTheTrip(t *testing.T) {
	_, stopped := overageRig(t, 150*time.Millisecond)
	r := newClaudeRig(t, "claude-overage", Options{Interactive: true})
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
	if _, err := r.s.Prompt(t.Context(), "hello"); err != nil {
		t.Fatal(err)
	}
	if err := r.s.log.Flush(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	stall.Store(true)
	go func() { _, _ = r.s.Prompt(context.Background(), "CONFIG-TRIP") }()
	waitStopped(t, r, stopped, 5*time.Second)
	free()
}

// TestClaudeUsageDeltasCoalesce (review C5c item 8): a stalled reader holds
// one queued usage delta, not one per report.
func TestClaudeUsageDeltasCoalesce(t *testing.T) {
	r := newClaudeRig(t, "claude-basic", Options{})
	var stall atomic.Bool
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	r.s.log.hooks = &logHooks{outboxAdmitting: func(int) {
		if stall.Load() {
			<-release
		}
	}}
	stall.Store(true)
	r.s.mu.Lock()
	for i := range 200 {
		r.s.usage.used = int64(i)
		r.s.publishUsageLocked()
	}
	r.s.mu.Unlock()
	r.s.log.outboxMu.Lock()
	n := len(r.s.log.outbox) // nothing but usage is enqueued here
	r.s.log.outboxMu.Unlock()
	if n > 1 {
		t.Fatalf("%d usage deltas queued behind a stalled reader; want the latest (and the one the drainer holds)", n)
	}
}

// TestClaudeQueuedAndDirectKeepOrder (review C5c item 10): an event the
// session emits while a delta of its is still queued goes behind it, never
// ahead.
func TestClaudeQueuedAndDirectKeepOrder(t *testing.T) {
	r := newClaudeRig(t, "claude-basic", Options{})
	var stall atomic.Bool
	release := make(chan struct{})
	r.s.log.hooks = &logHooks{outboxAdmitting: func(int) {
		if stall.Load() {
			<-release
		}
	}}
	log := collect(t, r.s)
	stall.Store(true)
	r.s.mu.Lock()
	r.s.usage.used = 7
	r.s.publishUsageLocked()
	r.s.mu.Unlock()
	go r.s.emit(Event{Type: EventText, Text: "after the usage"})
	time.Sleep(100 * time.Millisecond)
	stall.Store(false)
	close(release)
	waitFor(t, "both events", func() bool {
		return firstIndex(log.snapshot(), isUsageDelta) >= 0 && firstIndex(log.snapshot(), isText("after the usage")) >= 0
	})
	evs := log.snapshot()
	if u, txt := firstIndex(evs, isUsageDelta), firstIndex(evs, isText("after the usage")); u > txt {
		t.Fatalf("the text (%d) overtook the queued usage (%d)", txt, u)
	}
}

// TestClaudeLoadRestoresTheRecordedEffort (L5 F1): a load sends the effort
// the row recorded unless --effort names one, and a recorded effort the
// adapter does not take keeps its own, with a note.
func TestClaudeLoadRestoresTheRecordedEffort(t *testing.T) {
	r := newClaudeRig(t, "claude-load", Options{LoadSessionID: "s-old", LoadModel: "haiku", LoadEffort: "low"})
	collect(t, r.s)
	if err := r.s.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if setup := r.read(t, "setup"); !strings.Contains(setup, `{"configId":"model","value":"haiku"},{"configId":"effort","value":"low"}`) {
		t.Fatalf("load params %s", setup)
	}
	if opt := EffortOption(r.s.Snapshot()); opt == nil || opt.Current != "low" {
		t.Fatalf("effort %+v", opt)
	}
	r = newClaudeRig(t, "claude-load", Options{LoadSessionID: "s-old", LoadEffort: "bogus"})
	collect(t, r.s)
	if err := r.s.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := r.diag.String(); !strings.Contains(got, "the session's recorded effort bogus was not applied") {
		t.Fatalf("diag %q", got)
	}
	r = newClaudeRig(t, "claude-load", Options{LoadSessionID: "s-old", LoadEffort: "low", Effort: "max"})
	collect(t, r.s)
	if err := r.s.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(r.read(t, "setup"), `"configId":"effort"`) {
		t.Fatalf("--effort must not send the recorded effort: %s", r.read(t, "setup"))
	}
}

// TestClaudeChildRetiredAfterAnUndrainedHold (review C5c item 6): the agent's
// output ends while a load's hold still holds a child's running change and
// the drain has not begun: the retirement waits for the drain, so the child's
// row, once published, is retired — never left running after the end.
func TestClaudeChildRetiredAfterAnUndrainedHold(t *testing.T) {
	p := ClaudeProvider()
	s := newSession(Options{Provider: &p})
	t.Cleanup(func() { _ = s.Close() })
	log := collect(t, s)
	cr, sw := io.Pipe()
	sr, cw := io.Pipe()
	t.Cleanup(func() { _ = cr.Close(); _ = cw.Close(); _ = sr.Close(); _ = sw.Close() })
	client := acp.DialWithDialect(cr, cw, acp.DialectClaude)
	t.Cleanup(func() { _ = client.Close() })
	s.mu.Lock()
	s.client = client
	s.sessionID = "root"
	s.mu.Unlock()
	client.SetUpdateHandler(s.onUpdate)
	client.SetSubagentHandler(s.onSubagent)
	barrier := make(chan struct{}, 1)
	client.SetAuthStatusHandler(func(acp.AuthStatus) { barrier <- struct{}{} })
	enc, dec := acp.NewEncoder(sw), acp.NewDecoder(sr)
	loaded := make(chan error, 1)
	go func() {
		_, err := client.LoadSession(context.Background(), "root", t.TempDir(), acp.SessionParams{})
		loaded <- err
	}()
	req, err := dec.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if err := enc.WriteMessage(&acp.Message{JSONRPC: "2.0", ID: req.ID, Result: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if err := <-loaded; err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			if _, err := dec.ReadMessage(); err != nil {
				return
			}
		}
	}()
	raw, _ := json.Marshal(map[string]any{"sessionId": "root", "update": announce("c1", "running")})
	_ = enc.WriteMessage(&acp.Message{JSONRPC: "2.0", Method: acp.MethodSessionUpdate, Params: raw})
	_ = enc.WriteMessage(&acp.Message{JSONRPC: "2.0", Method: acp.MethodClaudeAuthStatus, Params: json.RawMessage(`{"authStatus":{"kind":"account"}}`)})
	<-barrier
	_ = sw.Close() // the agent's output ends, the drain not begun
	<-client.Conn().ReadDone()
	retired := make(chan struct{})
	go func() { s.retireChildrenAtEnd(client); close(retired) }()
	time.Sleep(150 * time.Millisecond) // a retirement that does not wait runs here
	if err := client.ReplayDone(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-retired:
	case <-time.After(5 * time.Second):
		t.Fatal("the retirement never ran")
	}
	// The retirement's row is queued behind the drain's (emitCtx): it is
	// waited for, bounded.
	rows := func() (out []SubagentStatus) {
		for _, ev := range log.snapshot() {
			if ev.Type == EventSubagent && ev.Subagent != nil && ev.Subagent.ID == "c1" {
				out = append(out, ev.Subagent.Status)
			}
		}
		return out
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if r := rows(); len(r) > 0 && r[len(r)-1] == SubagentFailed {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if r := rows(); len(r) < 2 || r[0] != SubagentRunning || r[len(r)-1] != SubagentFailed {
		t.Fatalf("the child's rows %v after the end; want running, then retired", r)
	}
}

// TestClaudeTextFloodDoesNotHoldTheTrip (review C5c item 5): with nobody
// reading the session's events, the read loop's text does not keep it from
// the tripping usage behind it.
func TestClaudeTextFloodDoesNotHoldTheTrip(t *testing.T) {
	_, stopped := overageRig(t, 150*time.Millisecond)
	r := newClaudeRig(t, "claude-overage", Options{Interactive: true})
	if err := r.s.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	// Events() is never read: the primary fills.
	go func() { _, _ = r.s.Prompt(context.Background(), "FLOOD-TRIP") }()
	select {
	case <-stopped:
	case <-time.After(8 * time.Second):
		t.Fatal("the trip behind the text was never reached")
	}
}
