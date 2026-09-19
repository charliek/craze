package agent

import (
	"context"
	"errors"
	"fmt"
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

// TestTakeRefusesWhileAPromptIsClaimed: a prompt Begin has claimed is in flight
// before its turn opens, so no row may leave the queue then either — Begin
// would refuse the row's own prompt, and the row would simply be gone.
func TestTakeRefusesWhileAPromptIsClaimed(t *testing.T) {
	s := startScript(t, "echo", true)
	if _, err := s.Queue("next"); err != nil {
		t.Fatal(err)
	}
	run := s.Begin("claimed")
	if _, ok := s.PopQueue(); ok {
		t.Fatal("a row left the queue while a prompt was claimed")
	}
	if _, ok := s.TakeQueued(s.Snapshot().Queue[0].ID); ok {
		t.Fatal("TakeQueued must be guarded the same way")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	if _, err := run(ctx); err != nil {
		t.Fatalf("the claimed prompt: %v", err)
	}
	if _, ok := s.PopQueue(); !ok {
		t.Fatal("the drain must run once the claimed prompt has returned")
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
	waitUntil(t, "both removed events", func() bool {
		n := 0
		for _, ev := range queueEvents(log.snapshot()) {
			if ev.QueueChange == QueueRemoved {
				n++
			}
		}
		return n == 2
	})
	// The error goes out before the removals, so a consumer is already in its
	// error state when they arrive and can say why the queue emptied. Both
	// being in the log says nothing about that; the order is the contract.
	evs := log.snapshot()
	errAt, removedAt := -1, -1
	for i, ev := range evs {
		if ev.Type == EventError && errAt < 0 {
			errAt = i
		}
		if ev.Type == EventQueue && ev.QueueChange == QueueRemoved && removedAt < 0 {
			removedAt = i
		}
	}
	if errAt < 0 {
		t.Fatal("no error event for a failed turn")
	}
	if removedAt < 0 {
		t.Fatal("no removed event for the queue the error cleared")
	}
	if errAt > removedAt {
		t.Fatalf("the error event (%d) came after the first removal (%d)", errAt, removedAt)
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
		head := s.Snapshot().Queue
		if _, ok := s.PopQueue(); ok {
			// The take happened; if a prompt is in flight now it began after
			// the take, which is exactly what the lock allows.
			taken++
			continue
		}
		// A refused take must leave its row where it was: nothing else drains
		// here, so the head can only have moved if the guard let a row out it
		// then refused to hand over.
		if len(head) == 0 {
			continue
		}
		if got := s.Snapshot().Queue; len(got) != len(head) || got[0].ID != head[0].ID {
			t.Fatalf("a refused take moved the queue: head %q of %d became %+v", head[0].ID, len(head), got)
		}
	}
	<-done
	if taken == 0 {
		t.Fatal("nothing drained")
	}
	if got := len(s.Snapshot().Queue); got != 20-taken {
		t.Fatalf("%d taken but %d left of 20", taken, got)
	}

	// The same guard, without the race: a row cannot leave the queue while a
	// prompt is in flight, and the refusal leaves it listed for the drain to
	// come back to. inPrompt is set by hand because a real turn would have to
	// be raced to be observed in it.
	row, err := s.Queue("guarded")
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.inPrompt = true
	s.mu.Unlock()
	if _, ok := s.TakeQueued(row.ID); ok {
		t.Fatal("a row left the queue while a prompt was in flight")
	}
	if _, ok := s.PopQueue(); ok {
		t.Fatal("the head left the queue while a prompt was in flight")
	}
	listed := false
	for _, p := range s.Snapshot().Queue {
		listed = listed || p.ID == row.ID
	}
	if !listed {
		t.Fatalf("the refused row is gone: %+v", s.Snapshot().Queue)
	}
	s.mu.Lock()
	s.inPrompt = false
	s.mu.Unlock()
	if _, ok := s.TakeQueued(row.ID); !ok {
		t.Fatal("the row must be takeable once the turn has returned")
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

// TestTakeQueuedSendsANonHeadRowAndKeepsTheRest: the TUI's "send this one now"
// takes a row out of the middle. The event carries the position the row held,
// which is a fact about the change and not about the queue afterwards, and
// every other row stays queued in its own order.
func TestTakeQueuedSendsANonHeadRowAndKeepsTheRest(t *testing.T) {
	s := startScript(t, "echo", true)
	log := collect(t, s)
	var rows []QueuedPrompt
	for _, text := range []string{"one", "two", "three"} {
		p, err := s.Queue(text)
		if err != nil {
			t.Fatal(err)
		}
		rows = append(rows, p)
	}
	got, ok := s.TakeQueued(rows[1].ID)
	if !ok {
		t.Fatal("an idle session must hand over any row")
	}
	if got.ID != rows[1].ID || got.Text != "two" {
		t.Fatalf("took %+v", got)
	}
	waitUntil(t, "the sent event", func() bool {
		for _, ev := range queueEvents(log.snapshot()) {
			if ev.QueueChange == QueueSent {
				return true
			}
		}
		return false
	})
	var sent []Event
	for _, ev := range queueEvents(log.snapshot()) {
		if ev.QueueChange == QueueSent {
			sent = append(sent, ev)
		}
	}
	if len(sent) != 1 {
		t.Fatalf("%d sent events for one take", len(sent))
	}
	if sent[0].QueuePos != 1 || sent[0].Queue.ID != rows[1].ID {
		t.Fatalf("sent event %+v pos %d", sent[0].Queue, sent[0].QueuePos)
	}
	left := s.Snapshot().Queue
	if len(left) != 2 || left[0].ID != rows[0].ID || left[1].ID != rows[2].ID {
		t.Fatalf("the other rows did not stay queued in order: %+v", left)
	}
}

// TestClearQueueRemovesHeadFirstAndCountsTheRows: a clear is one removal per
// row, head first, so a consumer replaying the stream empties its queue the
// same way — and the count is what tells the caller anything was there.
func TestClearQueueRemovesHeadFirstAndCountsTheRows(t *testing.T) {
	s := startScript(t, "echo", true)
	log := collect(t, s)
	want := []string{"one", "two", "three"}
	for _, text := range want {
		if _, err := s.Queue(text); err != nil {
			t.Fatal(err)
		}
	}
	if n := s.ClearQueue(); n != len(want) {
		t.Fatalf("ClearQueue returned %d for %d rows", n, len(want))
	}
	if got := s.Snapshot().Queue; len(got) != 0 {
		t.Fatalf("the queue survived the clear: %+v", got)
	}
	waitUntil(t, "every removed event", func() bool {
		n := 0
		for _, ev := range queueEvents(log.snapshot()) {
			if ev.QueueChange == QueueRemoved {
				n++
			}
		}
		return n == len(want)
	})
	var texts []string
	for _, ev := range queueEvents(log.snapshot()) {
		if ev.QueueChange != QueueRemoved {
			continue
		}
		if ev.QueuePos != 0 {
			// Each row was the head when it was dropped.
			t.Fatalf("removed %q at position %d", ev.Queue.Text, ev.QueuePos)
		}
		texts = append(texts, ev.Queue.Text)
	}
	if strings.Join(texts, ",") != strings.Join(want, ",") {
		t.Fatalf("removed %v, want head first: %v", texts, want)
	}
	if n := s.ClearQueue(); n != 0 {
		t.Fatalf("clearing an empty queue reported %d rows", n)
	}
}

// TestErrorPathClearSurvivesAFullEventChannel: the error and the removals it
// drags behind it are emitted while the only consumer is still waiting for
// Prompt to return, so the buffer can be full when the clear runs. A
// transaction that held the queue lock across that blocked send would wedge
// every other queue caller behind it — Close included, which is what would
// have unblocked it.
func TestErrorPathClearSurvivesAFullEventChannel(t *testing.T) {
	s := newSession(Options{})
	t.Cleanup(func() { _ = s.Close() })
	for i := 0; i < 4; i++ {
		if _, err := s.Queue(fmt.Sprintf("row-%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	// Nothing has read the stream, so filling the rest of the buffer leaves
	// the clear's own removals nowhere to go.
	for len(s.events) < cap(s.events) {
		s.emit(Event{Type: EventText, Text: "filler"})
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.clearQueueOnError()
	}()
	waitUntil(t, "the queue to empty", func() bool { return len(s.Snapshot().Queue) == 0 })
	select {
	case <-done:
		t.Fatal("the clear cannot have finished: nothing has read the full event channel")
	default:
	}
	waitUntil(t, "the transaction lock to be free while the emit blocks", func() bool {
		if !s.queueOp.TryLock() {
			return false
		}
		s.queueOp.Unlock()
		return true
	})
	// Reading the stream is all it takes for the transaction to finish.
	var removed []string
	read := make(chan struct{})
	go func() {
		defer close(read)
		for ev := range s.Events() {
			if ev.Type == EventQueue && ev.QueueChange == QueueRemoved {
				removed = append(removed, ev.Queue.Text)
			}
			if len(removed) == 4 {
				return
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the error path's clear deadlocked on a full event channel")
	}
	<-read
	if strings.Join(removed, ",") != "row-0,row-1,row-2,row-3" {
		t.Fatalf("removals %v", removed)
	}
}

// TestForeignTurnRefusalIsNotATurnThatFailed: the prompt never left craze, so
// nothing about it belongs in the stream. Emitting an error for it would put an
// error line in front of a caller that is about to retry and succeed, and
// clearing the queue would lose messages nothing had even tried to send.
func TestForeignTurnRefusalIsNotATurnThatFailed(t *testing.T) {
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
	if got := s.Snapshot().Queue; len(got) != 1 || got[0].Text != "PINEAPPLE" {
		t.Fatalf("a refusal must leave the queue alone: %+v", got)
	}
	for _, ev := range log.snapshot() {
		if ev.Type == EventError {
			t.Fatalf("a refusal must not reach the stream: %v", ev.Err)
		}
	}
	waitUntil(t, "the foreign turn to end", func() bool { return !s.Snapshot().ForeignTurn })
}
