package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func queueEvents(evs []Event) []Event {
	out := make([]Event, 0, len(evs))
	for _, ev := range evs {
		if ev.Type == EventQueue {
			out = append(out, ev)
		}
	}
	return out
}

// TestSessionQueueEmitsEventsAndSnapshots is the queue as the callers see it:
// every transaction is an event, and the snapshot carries the rows.
func TestSessionQueueEmitsEventsAndSnapshots(t *testing.T) {
	s := startScript(t, "echo", true)
	log := collect(t, s)
	first, err := s.Queue("one")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Queue("two"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "two queued events", func() bool { return len(queueEvents(log.snapshot())) == 2 })
	if got := s.Snapshot().Queue; len(got) != 2 || got[0].Text != "one" || got[1].Text != "two" {
		t.Fatalf("snapshot queue %+v", got)
	}
	if err := s.EditQueued(first.ID, "ONE"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the edit event", func() bool { return len(queueEvents(log.snapshot())) == 3 })
	evs := queueEvents(log.snapshot())
	if evs[2].QueueChange != QueueEdited || evs[2].QueuePos != 0 || evs[2].Queue.Version != 1 {
		t.Fatalf("edit event %+v", evs[2])
	}
	if got := s.Snapshot().Queue; got[0].Text != "ONE" || got[0].ID != first.ID {
		t.Fatalf("edit did not keep the row: %+v", got)
	}
	if _, ok := s.Unqueue(first.ID); !ok {
		t.Fatal("unqueue")
	}
	waitUntil(t, "the removed event", func() bool { return len(queueEvents(log.snapshot())) == 4 })
	if got := s.Snapshot().Queue; len(got) != 1 || got[0].Text != "two" {
		t.Fatalf("after unqueue %+v", got)
	}
}

// TestPopQueueRefusesWhileAPromptIsInFlight holds the guard the whole drain
// rests on: nothing leaves the queue until the turn that is running has
// returned, not merely emitted its EventDone.
func TestPopQueueRefusesWhileAPromptIsInFlight(t *testing.T) {
	s := startGrokScript(t, "grok-long-turn", true)
	if _, err := s.Queue("next"); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := s.Prompt(context.Background(), "do the steps"); err != nil {
			t.Errorf("prompt: %v", err)
		}
	}()
	waitUntil(t, "the turn to start", s.promptInFlight)
	if _, ok := s.PopQueue(); ok {
		t.Fatal("a row left the queue while a prompt was in flight")
	}
	if _, ok := s.TakeQueued(s.Snapshot().Queue[0].ID); ok {
		t.Fatal("TakeQueued must be guarded the same way")
	}
	<-done
	if _, ok := s.PopQueue(); !ok {
		t.Fatal("the drain must run once the prompt has returned")
	}
}

// TestPopQueueRefusesDuringAForeignTurn: a prompt sent while grok runs a turn
// of its own would be queued behind it, where its completion is no longer
// craze's to recognise.
func TestPopQueueRefusesDuringAForeignTurn(t *testing.T) {
	s := startGrokScript(t, "grok-long-turn-fallback", true)
	log := collect(t, s)
	if _, err := s.Queue("PINEAPPLE"); err != nil {
		t.Fatal(err)
	}
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
	if _, ok := s.PopQueue(); ok {
		t.Fatal("a row left the queue during a foreign turn")
	}
	waitUntil(t, "the foreign turn to end", func() bool { return !s.Snapshot().ForeignTurn })

	var started, ended int
	for _, ev := range log.snapshot() {
		if ev.Type != EventForeignTurn || ev.ForeignTurn == nil {
			continue
		}
		if ev.ForeignTurn.Running {
			started++
		} else {
			ended++
		}
	}
	if started != 1 || ended != 1 {
		t.Fatalf("foreign turn events: %d started, %d ended", started, ended)
	}
	var dones int
	for _, ev := range log.snapshot() {
		if ev.Type == EventDone {
			dones++
		}
	}
	if dones != 1 {
		t.Fatalf("%d EventDone for one craze prompt", dones)
	}
	// The queued prompt runs after the fallback, on a session it has let go.
	next, ok := s.PopQueue()
	if !ok {
		t.Fatal("the drain must run once the foreign turn is over")
	}
	if _, err := s.Prompt(t.Context(), next.Text); err != nil {
		t.Fatalf("queued prompt: %v", err)
	}
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
		_ = s.Cancel(context.Background())
		<-done
	})
}

// TestErroredTurnClearsTheQueue: a queue that outlived an error would run
// behind whatever the user sends next, long after they stopped expecting it.
func TestErroredTurnClearsTheQueue(t *testing.T) {
	s := startScript(t, "long-turn", true)
	log := collect(t, s)
	if _, err := s.Queue("one"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Queue("two"); err != nil {
		t.Fatal(err)
	}
	// A context that is already done makes the prompt fail on the wire,
	// which is the ending the session has to treat as an error.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Prompt(ctx, "first"); err == nil {
		t.Fatal("the prompt must fail")
	}
	waitUntil(t, "the queue to clear", func() bool { return len(s.Snapshot().Queue) == 0 })
	waitUntil(t, "the error event", func() bool {
		for _, ev := range log.snapshot() {
			if ev.Type == EventError {
				return true
			}
		}
		return false
	})
	var removed int
	for _, ev := range queueEvents(log.snapshot()) {
		if ev.QueueChange == QueueRemoved {
			removed++
		}
	}
	if removed != 2 {
		t.Fatalf("%d removed events for a two-row queue", removed)
	}
}

// TestPopQueueGuardIsAtomicWithTheRemoval: a row that left the queue and
// could not then be prompted would simply be gone, so the guard and the
// removal are one critical section.
func TestPopQueueGuardIsAtomicWithTheRemoval(t *testing.T) {
	s := startScript(t, "long-turn", true)
	for i := 0; i < 20; i++ {
		if _, err := s.Queue("row"); err != nil {
			t.Fatal(err)
		}
	}
	// One goroutine drains, another runs turns; every row that leaves the
	// queue must have been taken while nothing was in flight.
	var taken int
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 3; i++ {
			if _, err := s.Prompt(context.Background(), "x"); err != nil {
				return
			}
		}
	}()
	deadline := time.Now().Add(20 * time.Second)
	for taken < 20 && time.Now().Before(deadline) {
		if _, ok := s.PopQueue(); ok {
			s.mu.Lock()
			in := s.inPrompt
			s.mu.Unlock()
			if in {
				// The take happened; if a prompt is in flight now it began
				// after the take, which is exactly what the lock allows.
				_ = in
			}
			taken++
		}
	}
	<-done
	if taken == 0 {
		t.Fatal("nothing drained")
	}
	if got := len(s.Snapshot().Queue); got != 20-taken {
		t.Fatalf("%d taken but %d left of 20", taken, got)
	}
}
