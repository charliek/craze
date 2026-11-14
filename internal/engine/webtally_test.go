package engine

import (
	"context"
	"slices"
	"sync"
	"testing"

	"github.com/charliek/craze/internal/agent"
)

// The web tally's close cutoff on the engine's side (plan 051 F.4.2): Close
// calls the session's FreezeWebTally once, under e.mu, after the fence is up
// and before the synthetic closing ending is built and enqueued, saying
// whether a turn was current; what the freeze enqueues is therefore committed
// ahead of that ending, through the log's one FIFO. A settlement calls its
// RestateWebTally the same way, ahead of the turn's ended. A session without
// the interface settles and closes as it always has. The native session's own side is
// internal/agent's native_webtally_test.go, and both together over the real
// engine internal/tui's.

// freezingSession is the fake session with a WebTallyFreezer: its freeze
// records each call — whether a turn was current, whether e.mu was held, and
// whether the closing ending had been enqueued yet — and enqueues a web delta
// through the session's log, as the native session does.
type freezingSession struct {
	*fakeSession
	e *Engine

	mu       sync.Mutex
	calls    []freezeCall
	restates []bool
}

type freezeCall struct {
	turnCurrent, locked, fenced bool
}

// heldElsewhere says mu is held — by the caller up the stack, for a call
// made under it.
func heldElsewhere(mu *sync.Mutex) bool {
	if mu.TryLock() {
		mu.Unlock()
		return false
	}
	return true
}

func (s *freezingSession) FreezeWebTally(turnCurrent bool) {
	locked := heldElsewhere(&s.e.mu)
	s.mu.Lock()
	s.calls = append(s.calls, freezeCall{turnCurrent: turnCurrent, locked: locked, fenced: s.e.closed})
	s.mu.Unlock()
	s.log.Enqueue(agent.Event{Type: agent.EventMeta, At: s.clock(), State: &agent.StateDelta{
		WebSearch: &agent.WebSearchState{Searches: 1, URLs: []string{"https://frozen.example/"}}}})
}

// RestateWebTally records each settlement's call — whether e.mu was held —
// and enqueues a web delta through the session's log, as the native session
// does when its released tally counts anything.
func (s *freezingSession) RestateWebTally() {
	locked := heldElsewhere(&s.e.mu)
	s.mu.Lock()
	s.restates = append(s.restates, locked)
	s.mu.Unlock()
	s.log.Enqueue(agent.Event{Type: agent.EventMeta, At: s.clock(), State: &agent.StateDelta{
		WebSearch: &agent.WebSearchState{Fetches: 1, URLs: []string{"https://restated.example/"}}}})
}

func (s *freezingSession) recorded() []freezeCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]freezeCall(nil), s.calls...)
}

func (s *freezingSession) restated() []bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]bool(nil), s.restates...)
}

// TestSettleRestatesTheWebTallyBeforeTheEnded: a turn that ends on its own is
// settled with one RestateWebTally, under e.mu, before its ended is enqueued
// — so the restating delta is committed at a lower seq than the ended — and
// no freeze.
func TestSettleRestatesTheWebTallyBeforeTheEnded(t *testing.T) {
	fs, e := freezingRig(t)
	res, err := e.Submit(Command{}, "go", SubmitQueue, "")
	if err != nil || res.Turn == "" {
		t.Fatalf("submit = %+v, %v", res, err)
	}
	evs := waitSettled(t, e, res.Turn)
	ending := len(evs) - 1
	if got := fs.restated(); len(got) != 1 || !got[0] {
		t.Fatalf("restate calls = %v; want one, under e.mu", got)
	}
	if calls := fs.recorded(); len(calls) != 0 {
		t.Fatalf("freeze calls = %+v at a settlement; want none", calls)
	}
	web := slices.IndexFunc(evs, func(ev agent.Event) bool {
		return ev.Type == agent.EventMeta && ev.State != nil && ev.State.WebSearch != nil
	})
	if web < 0 || evs[web].Seq >= evs[ending].Seq {
		t.Fatalf("the restating delta at %d, the ended at %d: %s", web, ending, describe(evs))
	}
}

// freezingRig is an engine over a freezingSession, with a primary to read
// what was committed.
func freezingRig(t *testing.T) (*freezingSession, *Engine) {
	t.Helper()
	fs := &freezingSession{fakeSession: newFake(t, agent.EventLogOptions{})}
	e, err := newEngine(fs, Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	fs.e = e
	t.Cleanup(func() { _ = e.Close() })
	if err := e.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	return fs, e
}

// TestCloseFreezesTheWebTallyBeforeTheClosingEnding: with a turn current,
// Close freezes once, under e.mu, with the engine closed and turnCurrent true,
// before its ending is enqueued — so the freeze's delta is committed at a
// lower seq than the synthetic closing ending (not necessarily adjacent: the
// two are separate batches).
func TestCloseFreezesTheWebTallyBeforeTheClosingEnding(t *testing.T) {
	fs, e := freezingRig(t)
	sc := fs.script(held())
	res, err := e.Submit(Command{}, "go", SubmitQueue, "")
	if err != nil || res.Turn == "" {
		t.Fatalf("submit = %+v, %v", res, err)
	}
	await(t, sc.opened, "the turn's opening")
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	calls := fs.recorded()
	if len(calls) != 1 || !calls[0].turnCurrent || !calls[0].locked || !calls[0].fenced {
		t.Fatalf("freeze calls = %+v; want one, a turn current, under e.mu, after closed was set", calls)
	}
	evs := drainEngine(e)
	freeze, ending := -1, -1
	for i, ev := range evs {
		switch {
		case ev.Type == agent.EventMeta && ev.State != nil && ev.State.WebSearch != nil:
			freeze = i
		case ended(res.Turn)(ev):
			ending = i
		}
	}
	if freeze < 0 || ending < 0 {
		t.Fatalf("freeze delta at %d, closing ending at %d: %s", freeze, ending, describe(evs))
	}
	if !evs[ending].Turn.Synthetic || evs[ending].Turn.StopReason != stopClosing {
		t.Fatalf("the ending is %+v; want the synthetic closing one", evs[ending].Turn)
	}
	if evs[freeze].Seq >= evs[ending].Seq {
		t.Fatalf("the freeze's delta (seq %d) is not before the closing ending (seq %d)", evs[freeze].Seq, evs[ending].Seq)
	}
}

// TestCloseFreezesAnIdleSession: with no turn current — the last one settled —
// the freeze still runs, once, with turnCurrent false, and no ending is made.
func TestCloseFreezesAnIdleSession(t *testing.T) {
	fs, e := freezingRig(t)
	res, err := e.Submit(Command{}, "go", SubmitQueue, "")
	if err != nil {
		t.Fatal(err)
	}
	waitSettled(t, e, res.Turn)
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	if calls := fs.recorded(); len(calls) != 1 || calls[0].turnCurrent {
		t.Fatalf("freeze calls = %+v; want one, with no turn current", calls)
	}
	// Close is idempotent: no second freeze.
	_ = e.Close()
	if n := len(fs.recorded()); n != 1 {
		t.Fatalf("%d freezes after a second Close", n)
	}
}

// waitSettled waits until turn has ended in the engine's record, and answers
// what it drained from the primary, through that ending.
func waitSettled(t *testing.T, e *Engine, turn string) []agent.Event {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), watchdog)
	defer cancel()
	var evs []agent.Event
	for {
		for _, ev := range drainEngine(e) {
			evs = append(evs, ev)
			if ended(turn)(ev) {
				return evs
			}
		}
		if err := e.Sync(ctx); err != nil {
			t.Fatalf("turn %s did not settle: %v", turn, err)
		}
	}
}

// TestCloseWithoutAFreezerIsAsBefore: a session that keeps no web tally (the
// fake, an ACP session) is closed exactly as before: the closing ending, and
// no web section anywhere.
func TestCloseWithoutAFreezerIsAsBefore(t *testing.T) {
	r := newRigOn(t, Options{}, agent.EventLogOptions{})
	if _, ok := agent.Session(r.s).(agent.WebTallyFreezer); ok {
		t.Fatal("fixture: the fake session is a WebTallyFreezer")
	}
	sc := r.s.script(held())
	res := r.submit("go")
	await(t, sc.opened, "the turn's opening")
	if err := r.e.Close(); err != nil {
		t.Fatal(err)
	}
	var ending *agent.TurnInfo
	for _, ev := range r.committed() {
		if ev.Type == agent.EventMeta && ev.State != nil && ev.State.WebSearch != nil {
			t.Fatalf("a web section from a session with no tally: %+v", ev.State.WebSearch)
		}
		if ended(res.Turn)(ev) {
			ending = ev.Turn
		}
	}
	if ending == nil || !ending.Synthetic || ending.StopReason != stopClosing {
		t.Fatalf("the ending is %+v; want the synthetic closing one", ending)
	}
	if offered, known := r.e.WebSearch(); offered || known {
		t.Fatalf("WebSearch() = %v, %v for a session that is no WebSearcher", offered, known)
	}
}
