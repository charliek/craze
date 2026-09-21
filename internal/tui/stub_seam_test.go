package tui

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
)

// The three seams a component above the provider seam needs of a session, on
// the Stub (plan 021 §3.3, §3.9): the event log it publishes into, the clock it
// stamps from, and a log with no primary at all. The compile-time assertions in
// stub.go pin the method set; these pin that each one is the *session's* own —
// the log Events() reads from, the clock the test injected.

// stubSeamWait bounds every wait here; a correct run never comes near it.
const stubSeamWait = 10 * time.Second

// withinStubSeam fails the test unless fn returns within the bound: how a test
// says "this does not block" without a clock deciding what blocking is.
func withinStubSeam(t *testing.T, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(stubSeamWait):
		t.Fatalf("%s: still blocked after %v", what, stubSeamWait)
	}
}

// TestStubOwnsItsLogAndItsClock: the Stub is an agent.LogOwner whose EventLog is
// the very log Events() reads from — an event enqueued into it lands in the same
// sequence as the Stub's own emits — and an agent.Clocked answering with the
// injected Clock, falling back to the wall clock exactly as emit does.
func TestStubOwnsItsLogAndItsClock(t *testing.T) {
	s := NewStub()
	t.Cleanup(func() { _ = s.Close() })
	if s.NoPrimary {
		t.Fatal("NewStub reports NoPrimary")
	}
	owner, ok := agent.Session(s).(agent.LogOwner)
	if !ok {
		t.Fatal("the Stub is not an agent.LogOwner")
	}
	log := owner.EventLog()
	if log == nil {
		t.Fatal("EventLog is nil")
	}
	if log.Incarnation() != s.Incarnation() {
		t.Fatalf("EventLog's incarnation is %q and the Stub's %q: they are not one log", log.Incarnation(), s.Incarnation())
	}

	clocked, ok := agent.Session(s).(agent.Clocked)
	if !ok {
		t.Fatal("the Stub is not an agent.Clocked")
	}
	// Bracketed both ways: a fallback that answered a fixed date would pass a
	// not-earlier-than check on its own.
	before := time.Now()
	got := clocked.Now()
	after := time.Now()
	if got.Before(before) || got.After(after) {
		t.Fatalf("Now with no Clock injected is %v, outside [%v, %v]: it is not the wall clock", got, before, after)
	}
	fixed := time.Date(2026, 9, 20, 9, 30, 0, 0, time.UTC)
	s.Clock = func() time.Time { return fixed }
	if got := clocked.Now(); !got.Equal(fixed) {
		t.Fatalf("Now is %v, want the injected clock's %v", got, fixed)
	}

	// One sequence, whoever published: the Stub's emit, then the log's outbox.
	s.Emit(agent.Event{Type: agent.EventText, Text: "from the stub"})
	log.Enqueue(agent.Event{Type: agent.EventText, Text: "from above the seam", At: fixed})
	if err := log.Flush(context.Background(), nil); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	evs := stubBuffered(s)
	if len(evs) != 2 {
		t.Fatalf("the primary holds %d events, want the Stub's and the enqueued one", len(evs))
	}
	assertContiguous(t, evs, 1)
	if evs[0].Text != "from the stub" || evs[1].Text != "from above the seam" {
		t.Fatalf("the primary holds %q then %q", evs[0].Text, evs[1].Text)
	}
	if !evs[1].At.Equal(fixed) {
		t.Fatalf("the enqueued event's At is %v: the log stamped one of its own", evs[1].At)
	}
}

// TestStubNoPrimaryEmitsWithNobodyReading: NewStubNoPrimary builds its log with
// no primary send, so a test can drive a session nobody reads — the 257th event
// no longer blocks the emitter — and every event still reaches a subscription.
func TestStubNoPrimaryEmitsWithNobodyReading(t *testing.T) {
	const events = 1000
	s := NewStubNoPrimary()
	t.Cleanup(func() { _ = s.Close() })
	if !s.NoPrimary {
		t.Fatal("NewStubNoPrimary does not report NoPrimary")
	}
	sub, err := s.Subscribe(agent.SubscribeOptions{MaxItems: events + 1})
	if err != nil {
		t.Fatal(err)
	}
	withinStubSeam(t, "1000 emits with nobody reading", func() {
		for i := range events {
			s.Emit(agent.Event{Type: agent.EventText, Text: fmt.Sprintf("%d", i+1)})
		}
	})
	if n := len(stubBuffered(s)); n != 0 {
		t.Fatalf("%d events reached Events() on a NoPrimary Stub", n)
	}
	recs := stubRecords(t, sub, events)
	for i, rec := range recs {
		ev, err := rec.Event()
		if err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
		if want := fmt.Sprint(i + 1); ev.Text != want || rec.Seq != uint64(i+1) {
			t.Fatalf("record %d is seq %d %q, want %d %q", i, rec.Seq, ev.Text, i+1, want)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if rest := stubRecordsToEnd(t, sub); len(rest) != 0 {
		t.Fatalf("the subscription got %d records after Close", len(rest))
	}
	if err := sub.Err(); !errors.Is(err, agent.ErrClosed) {
		t.Fatalf("the subscription ended with %v, want agent.ErrClosed", err)
	}
}

// stubOpenedProbe closes opened the first time its Done channel is asked for.
// The Stub's hung turn — HangNext — parks on exactly one select whose first
// case is <-ctx.Done(), evaluated once, right after inPrompt is set true and
// the claim's continuation has genuinely opened its turn: it is
// internal/agent/session_contract_test.go's openedProbe, copied here because
// that file is a different package and this one cannot import it.
type stubOpenedProbe struct {
	context.Context
	once   sync.Once
	opened chan struct{}
}

func (p *stubOpenedProbe) Done() <-chan struct{} {
	p.once.Do(func() { close(p.opened) })
	return p.Context.Done()
}

// TestStubCancelOutcome pins the Stub's CancelOutcome mapping (plan 021 §3.7)
// over the three states Cancel can find: nothing claimed, claimed and parked
// (not yet open), and a running turn. The Stub never waits, so unlike live and
// native its Settled is true only in the first: an open or about-to-withdraw
// turn is still going, by definition, the instant Cancel returns.
func TestStubCancelOutcome(t *testing.T) {
	t.Run("nothing running", func(t *testing.T) {
		s := NewStub()
		t.Cleanup(func() { _ = s.Close() })
		outcome, err := s.Cancel(context.Background())
		if want := (agent.CancelOutcome{Wrote: true, Settled: true}); err != nil || outcome != want {
			t.Fatalf("Cancel with nothing running = %+v, %v; want %+v, nil", outcome, err, want)
		}
	})
	t.Run("claimed, not yet open", func(t *testing.T) {
		s := NewStub()
		t.Cleanup(func() { _ = s.Close() })
		parked := s.ParkNext()
		run := s.Begin("hi")
		go func() { _, _ = run(context.Background()) }()
		awaitBarrier(t, parked, "the prompt reaching the park")
		outcome, err := s.Cancel(context.Background())
		if want := (agent.CancelOutcome{Withdrew: true}); err != nil || outcome != want {
			t.Fatalf("Cancel on a parked prompt = %+v, %v; want %+v, nil", outcome, err, want)
		}
	})
	t.Run("running", func(t *testing.T) {
		s := NewStub()
		t.Cleanup(func() { _ = s.Close() })
		s.HangNext()
		probe := &stubOpenedProbe{Context: context.Background(), opened: make(chan struct{})}
		run := s.Begin("hi")
		go func() { _, _ = run(probe) }()
		awaitBarrier(t, probe.opened, "the turn opening")
		outcome, err := s.Cancel(context.Background())
		if want := (agent.CancelOutcome{Wrote: true}); err != nil || outcome != want {
			t.Fatalf("Cancel on a running turn = %+v, %v; want %+v, nil", outcome, err, want)
		}
	})
}

// TestStubForeignTurnAgreesWithSnapshot: the leaf accessor and Snapshot's own
// field report the same thing across a foreign turn's start and end (plan 021
// §3.3): they read the same field under the same lock, so this is mostly a
// statement that ForeignTurn exists and is wired to it, not a race test.
func TestStubForeignTurnAgreesWithSnapshot(t *testing.T) {
	s := NewStub()
	t.Cleanup(func() { _ = s.Close() })
	if s.ForeignTurn() || s.Snapshot().ForeignTurn {
		t.Fatal("a fresh Stub reports a foreign turn")
	}
	s.SetForeignTurn(agent.ForeignTurnInfo{ID: "p-1", Running: true})
	if !s.ForeignTurn() || s.ForeignTurn() != s.Snapshot().ForeignTurn {
		t.Fatalf("ForeignTurn() = %v, Snapshot().ForeignTurn = %v; want both true", s.ForeignTurn(), s.Snapshot().ForeignTurn)
	}
	s.SetForeignTurn(agent.ForeignTurnInfo{ID: "p-1", Running: false})
	if s.ForeignTurn() || s.ForeignTurn() != s.Snapshot().ForeignTurn {
		t.Fatalf("ForeignTurn() = %v, Snapshot().ForeignTurn = %v; want both false", s.ForeignTurn(), s.Snapshot().ForeignTurn)
	}
}
