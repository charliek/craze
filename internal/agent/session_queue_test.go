package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// waitUntil polls for a condition the test cannot be handed as a barrier —
// a scripted agent's own progress, mostly. Its deadline is a deadlock
// watchdog, never a timing assertion, so it is deliberately far longer than
// any wait that is working: a scripted multi-step turn on a box that is also
// running a live smoke and another -race suite took more than the 10 seconds
// this used to allow, and failed a test that had nothing wrong with it. A
// genuine hang still fails well inside the package's own timeout.
func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// foreignTurnCounts is how many foreign turns the log says started and ended.
// Both are read from the same snapshot, so a turn cannot be counted as having
// ended in a pass that never saw it start.
func foreignTurnCounts(evs []Event) (started, ended int) {
	for _, ev := range evs {
		if ev.Type != EventForeignTurn || ev.ForeignTurn == nil {
			continue
		}
		if ev.ForeignTurn.Running {
			started++
		} else {
			ended++
		}
	}
	return started, ended
}

// countType is how many events of one type the log holds.
func countType(evs []Event, typ EventType) int {
	n := 0
	for _, ev := range evs {
		if ev.Type == typ {
			n++
		}
	}
	return n
}

// TestInterjectOnCursorFailsBeforeTheWire: the capability decides, so nothing
// is written and the text stays with the caller.
func TestInterjectOnCursorFailsBeforeTheWire(t *testing.T) {
	s := startScript(t, "long-turn", true)
	if err := s.Interject(t.Context(), "BANANA"); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("err %v", err)
	}
}

// TestInterjectLandsAsAUserEventFromTheBroadcast: the ack only says the text
// was accepted; the transcript entry comes from the broadcast.
func TestInterjectLandsAsAUserEventFromTheBroadcast(t *testing.T) {
	s := startGrokScript(t, "grok-long-turn", true)
	log := collect(t, s)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := s.Prompt(context.Background(), "do the steps"); err != nil {
			t.Errorf("prompt: %v", err)
		}
	}()
	waitUntil(t, "the turn to start", s.promptInFlight)
	if err := s.Interject(t.Context(), "BANANA"); err != nil {
		t.Fatalf("interject: %v", err)
	}
	waitUntil(t, "the interjection user event", func() bool {
		for _, ev := range log.snapshot() {
			if ev.Type == EventUser && ev.Interjection && ev.Text == "BANANA" {
				return true
			}
		}
		return false
	})
	<-done
	waitUntil(t, "the reply carrying the interjection", func() bool {
		return strings.Contains(texts(log.snapshot()), "DONE step1 BANANA step2")
	})
}

// TestInterjectRefusedInEveryStrandedState covers the three cases grok turns
// into a turn of its own: idle, already done, and cancelling.
func TestInterjectRefusedInEveryStrandedState(t *testing.T) {
	t.Run("idle", func(t *testing.T) {
		s := startGrokScript(t, "grok-long-turn", true)
		if err := s.Interject(t.Context(), "x"); !errors.Is(err, ErrNotInTurn) {
			t.Fatalf("err %v", err)
		}
	})
	t.Run("after done", func(t *testing.T) {
		s := startGrokScript(t, "grok-long-turn", true)
		if _, err := s.Prompt(t.Context(), "do the steps"); err != nil {
			t.Fatal(err)
		}
		if err := s.Interject(t.Context(), "x"); !errors.Is(err, ErrNotInTurn) {
			t.Fatalf("err %v", err)
		}
	})
	t.Run("cancelling", func(t *testing.T) {
		s := startGrokScript(t, "grok-long-turn", true)
		done := make(chan struct{})
		go func() {
			defer close(done)
			if _, err := s.Prompt(context.Background(), "do the steps"); err != nil {
				t.Errorf("prompt: %v", err)
			}
		}()
		waitUntil(t, "the turn to start", s.promptInFlight)
		// Cancel blocks until the turn ends, and the turn ends fast enough
		// that racing it would test nothing. The window it opens is the
		// cancelling flag, so that is what is put in place here.
		s.mu.Lock()
		s.cancelling = true
		s.mu.Unlock()
		err := s.Interject(context.Background(), "x")
		if !errors.Is(err, ErrNotInTurn) {
			t.Fatalf("err %v", err)
		}
		_, _ = s.Cancel(context.Background())
		<-done
	})
}

// TestForeignTurnAccessorAgreesWithSnapshot: ForeignTurn() is the same flag
// Snapshot() reports (plan 021's leaf accessor, §3.3) — both read s.foreign
// under s.mu — driven through a real grok foreign-turn fallback so the
// property is checked against the wire, not just the field.
func TestForeignTurnAccessorAgreesWithSnapshot(t *testing.T) {
	s := startGrokScript(t, "grok-long-turn-fallback", true)
	log := collect(t, s)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := s.Prompt(context.Background(), "do the steps"); err != nil {
			t.Errorf("prompt: %v", err)
		}
	}()
	waitUntil(t, "the turn to start", s.promptInFlight)
	if err := s.Interject(t.Context(), "BANANA"); err != nil {
		t.Fatalf("interject: %v", err)
	}
	<-done
	waitUntil(t, "the foreign turn to start", func() bool { return s.Snapshot().ForeignTurn })
	// ForeignTurn is the same flag Snapshot() reports, plan 021's leaf
	// accessor (§3.3): once the snapshot has caught the flag, the leaf must
	// agree — both read s.foreign under s.mu.
	if !s.ForeignTurn() {
		t.Fatal("ForeignTurn() disagrees with Snapshot().ForeignTurn while a foreign turn is running")
	}
	waitUntil(t, "the foreign turn to end", func() bool { return !s.Snapshot().ForeignTurn })
	if s.ForeignTurn() {
		t.Fatal("ForeignTurn() disagrees with Snapshot().ForeignTurn once the foreign turn ended")
	}

	// The snapshot flag is not the sync point for anything read out of the
	// log. onForeignTurn flips s.foreign under the lock and emits only after
	// unlocking, and emit merely buffers — the collector appends later still.
	// So the wait above returns in a window where the end event has not been
	// emitted at all, and counting there reads one event short. The log is
	// what the assertions are about, so the log is what they wait on.
	waitUntil(t, "the foreign turn's end event", func() bool {
		_, ended := foreignTurnCounts(log.snapshot())
		return ended > 0
	})
	started, ended := foreignTurnCounts(log.snapshot())
	if started != 1 || ended != 1 {
		t.Fatalf("foreign turn events: %d started, %d ended", started, ended)
	}
	// Same shape: <-done says Prompt returned, which says EventDone was
	// emitted — not that the collector has appended it.
	waitUntil(t, "the turn's done event", func() bool { return countType(log.snapshot(), EventDone) > 0 })
	if dones := countType(log.snapshot(), EventDone); dones != 1 {
		t.Fatalf("%d EventDone for one craze prompt", dones)
	}
}

// TestInterjectRefusedAfterAFailedTurn is the window an error leaves open if
// the turn is only marked over beside EventDone: the error path emits no
// EventDone at all, so an interjection sent while its events drain would
// reach grok after the turn and mint one of grok's own.
func TestInterjectRefusedAfterAFailedTurn(t *testing.T) {
	s := startGrokScript(t, "grok-long-turn", true)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Prompt(ctx, "do the steps"); err == nil {
		t.Fatal("the prompt must fail")
	}
	if err := s.Interject(context.Background(), "BANANA"); !errors.Is(err, ErrNotInTurn) {
		t.Fatalf("err %v", err)
	}
}

// TestForeignTurnRefusalIsNotATurnThatFailed: the prompt never left craze, so
// nothing about it belongs in the stream. Emitting an error for it would put an
// error line in front of a caller that is about to retry and succeed.
func TestForeignTurnRefusalIsNotATurnThatFailed(t *testing.T) {
	s := startGrokScript(t, "grok-long-turn-fallback", true)
	log := collect(t, s)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := s.Prompt(context.Background(), "do the steps"); err != nil {
			t.Errorf("prompt: %v", err)
		}
	}()
	waitUntil(t, "the turn to start", s.promptInFlight)
	if err := s.Interject(t.Context(), "BANANA"); err != nil {
		t.Fatalf("interject: %v", err)
	}
	<-done
	// The start event, not the snapshot flag: the flag flips before the event
	// is emitted, so waiting on it would leave the "no error reached the
	// stream" check below reading a log the collector has not caught up with —
	// which is a refusal passing for want of evidence rather than on it. The
	// event is emitted after the flag, so this waits for both.
	waitUntil(t, "the foreign turn's start event", func() bool {
		started, _ := foreignTurnCounts(log.snapshot())
		return started > 0
	})

	s.mu.Lock()
	turnBefore := s.turn
	s.mu.Unlock()
	if _, err := s.Prompt(context.Background(), "next"); !errors.Is(err, ErrForeignTurn) {
		t.Fatalf("prompt during a foreign turn: %v", err)
	}
	s.mu.Lock()
	turnAfter := s.turn
	s.mu.Unlock()
	if turnAfter != turnBefore {
		t.Fatalf("a refused prompt spent turn %d (was %d)", turnAfter, turnBefore)
	}
	for _, ev := range log.snapshot() {
		if ev.Type == EventError {
			t.Fatalf("a refusal must not reach the stream: %v", ev.Err)
		}
	}
	waitUntil(t, "the foreign turn to end", func() bool { return !s.Snapshot().ForeignTurn })
}
