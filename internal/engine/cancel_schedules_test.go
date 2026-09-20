package engine

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
)

// TestOverlappingCancelsEachHoldTheirOwn: two cancels are validated against one
// turn. The second goes through and ends it; the first is still parked at the
// session's door. One hold still stands, so the turn does not settle and nothing
// is admitted until that cancel, too, has returned.
func TestOverlappingCancelsEachHoldTheirOwn(t *testing.T) {
	returned := make(chan string, 8)
	r := newRigHooked(t, Options{}, agent.EventLogOptions{NoPrimary: true},
		&hooks{turnReturned: func(id string) { returned <- id }})
	turn := r.s.script(held())
	r.submit("one")
	await(t, turn.opened, "the turn to open")
	r.submit("two")
	r.sync()

	entered, release := r.s.holdNextCancel()
	first := make(chan error, 1)
	go func() {
		_, err := r.e.Cancel(context.Background(), Command{}, "turn-1")
		first <- err
	}()
	await(t, entered, "the first cancel to reach the session")
	if _, err := r.e.Cancel(context.Background(), Command{}, "turn-1"); err != nil {
		t.Fatalf("the second cancel: %v", err)
	}
	select {
	case <-returned:
	case <-time.After(watchdog):
		t.Fatal("the cancelled turn never came back")
	}
	if st := r.e.State(); st.Turn != "turn-1" {
		t.Fatalf("the turn settled with a cancel still on its way to the session: %+v", st)
	}
	r.wantPrompts("one")
	release()
	if err := <-first; err != nil {
		t.Fatalf("the first cancel: %v", err)
	}
	r.until(lastEnding)
	r.wantPrompts("one", "two")
}

// TestASubmitUnderACancelWithNoTurnQueues: a cancel with no turn of craze's own
// holds admission like any other — it is on its way to the agent, and a prompt
// started now could be what it lands on. Such a cancel is accepted only when
// there is something for it to do (§3.7), which here is an ask the agent is
// waiting on.
func TestASubmitUnderACancelWithNoTurnQueues(t *testing.T) {
	r := newRig(t, Options{})
	r.s.openAsk(t)
	entered, release := r.s.holdNextCancel()
	done := make(chan error, 1)
	go func() {
		_, err := r.e.Cancel(context.Background(), Command{}, "")
		done <- err
	}()
	await(t, entered, "the cancel to reach the session")
	res := r.submit("wait for it")
	if res.Queued == nil {
		t.Fatalf("a submit under a cancel hold answered %+v, want a queued row", res)
	}
	r.sync()
	r.wantPrompts()
	release()
	if err := <-done; err != nil {
		t.Fatalf("cancel: %v", err)
	}
	// The release is a driver pass, and the row goes.
	r.until(lastEnding)
	r.wantPrompts("wait for it")
}

// TestAPanicOnTheWayToTheSessionGivesTheHoldBack: a session double, or a hook,
// panics under a caller that recovers. The hold it had taken is released on the
// way out, so the turn still settles and the engine still admits.
func TestAPanicOnTheWayToTheSessionGivesTheHoldBack(t *testing.T) {
	var once sync.Once
	r := newRigHooked(t, Options{}, agent.EventLogOptions{NoPrimary: true}, &hooks{
		beforeSessionCancel: func(string) { once.Do(func() { panic("a hook that panics") }) },
	})
	turn := r.s.script(held())
	r.submit("one")
	await(t, turn.opened, "the turn to open")
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("the panic did not reach Cancel's caller")
			}
		}()
		_, _ = r.e.Cancel(context.Background(), Command{}, "")
	}()
	r.e.mu.Lock()
	holds := r.e.cancelsInFlight
	r.e.mu.Unlock()
	if holds != 0 {
		t.Fatalf("%d holds stand after a cancel that panicked", holds)
	}
	turn.release()
	r.until(lastEnding)
	r.submit("two")
	r.until(lastEnding)
}

// TestStopWithAnExpiredContextMakesNoCancel: Stop waits for its removals to be
// delivered before it cancels, because the cancelled turn's done must not
// overtake them. A caller whose context has ended has given that wait up, and
// the cancel is not made at all rather than made out of order. The engine stays
// stopped, the queue stays cleared, and the hold is given back.
func TestStopWithAnExpiredContextMakesNoCancel(t *testing.T) {
	// A primary nobody reads: once it is full the outbox cannot deliver.
	s := newFake(t, agent.EventLogOptions{})
	e, err := New(s, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	if err := e.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	turn := s.script(held())
	go func() {
		for ev := range e.Events() {
			if started("turn-1")(ev) {
				return
			}
		}
	}()
	if _, err := e.Submit(Command{}, "one", SubmitQueue, ""); err != nil {
		t.Fatal(err)
	}
	await(t, turn.opened, "the turn to open")
	if _, err := e.Submit(Command{}, "two", SubmitQueue, ""); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 300; i++ {
		e.log.Enqueue(agent.Event{Type: agent.EventText, Text: "fill"})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := e.Stop(ctx, Command{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("stop with an ended context: %v", err)
	}
	if n := s.cancelsWritten(); n != 0 {
		t.Fatalf("%d cancels were made behind a barrier that was given up", n)
	}
	e.mu.Lock()
	holds, stopped, queued := e.cancelsInFlight, e.stopped, e.queue.Len()
	e.mu.Unlock()
	if holds != 0 || !stopped || queued != 0 {
		t.Fatalf("after the abandoned stop: holds %d stopped %v queued %d", holds, stopped, queued)
	}
	turn.release()
}

// TestCloseFromTheTurnReturnedHook: the hook runs with the turn's goroutine
// already counted out, so closing the engine from inside it is not a wait on
// oneself.
func TestCloseFromTheTurnReturnedHook(t *testing.T) {
	var e *Engine
	closed := make(chan error, 1)
	s := newFake(t, agent.EventLogOptions{NoPrimary: true})
	e, err := newEngine(s, Options{}, &hooks{turnReturned: func(string) { closed <- e.Close() }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	if err := e.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Submit(Command{}, "one", SubmitQueue, ""); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("close: %v", err)
		}
	case <-time.After(watchdog):
		t.Fatal("Close from the hook is waiting on the goroutine that called it")
	}
}
