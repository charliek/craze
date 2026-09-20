package engine

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/charliek/craze/internal/agent"
)

// TestACancelNamingAStaleTurnIsRefused: the cancel names the turn the client
// displayed. By the time it arrives that turn has settled and its successor is
// running; the cancel is refused, writes nothing, and the successor completes.
func TestACancelNamingAStaleTurnIsRefused(t *testing.T) {
	r := newRig(t, Options{})
	first := r.s.script(held())
	second := r.s.script(held())
	r.submit("one")
	await(t, first.opened, "the first turn to open")
	r.submit("two")
	r.sync()
	first.release()
	await(t, second.opened, "the second turn to open")
	_, err := r.e.Cancel(context.Background(), Command{}, "turn-1")
	if !errors.Is(err, ErrStaleTurn) || Code(err) != "stale_turn" {
		t.Fatalf("a cancel for a settled turn: %v (%s)", err, Code(err))
	}
	if n := r.s.cancelsWritten(); n != 0 {
		t.Fatalf("a refused cancel wrote %d", n)
	}
	second.release()
	got := r.until(lastEnding)
	if last := got[len(got)-1].Turn; last.ID != "turn-2" || last.StopReason != stopEndTurn {
		t.Fatalf("the turn that was not cancelled ended %+v", last)
	}
}

// TestACancelHeldBeforeTheSessionAdmitsNothing is the schedule the hold exists
// for. A cancel is validated against turn N and pauses before it reaches the
// session. N comes back meanwhile. Without the hold, N would settle, a row
// behind it — or a fresh submit, or an armed send — would start N+1, and the
// paused cancel would then land on N+1: Esc for the turn the user stopped would
// stop the one they did not.
//
// Barriers: the cancel's validation (it has returned from holdCancel when the
// session's Cancel is entered), Session.Cancel's entry, and admission, which is
// observed by what the session was handed.
func TestACancelHeldBeforeTheSessionAdmitsNothing(t *testing.T) {
	r := newRig(t, Options{})
	first := r.s.script(held())
	r.submit("one")
	await(t, first.opened, "the first turn to open")
	r.submit("two")
	r.sync()

	entered, release := r.s.holdNextCancel()
	cancelled := make(chan error, 1)
	go func() {
		_, err := r.e.Cancel(context.Background(), Command{}, "turn-1")
		cancelled <- err
	}()
	await(t, entered, "the cancel to reach the session")

	// N returns on its own while the cancel is parked at the session's door.
	first.release()
	got := r.until(func(ev agent.Event) bool { return ev.Type == agent.EventDone })
	if last := got[len(got)-1]; last.StopReason != stopEndTurn {
		t.Fatalf("the turn ended %q before the cancel reached it, want its own ending", last.StopReason)
	}

	// Every admission path is shut: the drain has a row and starts nothing, a
	// fresh submit queues instead of starting, and the turn has not settled.
	res, err := r.e.Submit(Command{}, "three", SubmitQueue, "")
	if err != nil || res.Queued == nil {
		t.Fatalf("a submit under a cancel hold answered %+v, %v: want a queued row", res, err)
	}
	r.sync()
	r.wantPrompts("one")
	if st := r.e.State(); st.Turn != "turn-1" || st.Activity != ActivityWorking {
		t.Fatalf("the turn settled under a cancel hold: %+v", st)
	}

	// The cancel goes through. It finds nothing running — N is over and nothing
	// was admitted behind it — and only now does N settle, with its successor
	// decided in the same step.
	release()
	if err := <-cancelled; err != nil {
		t.Fatalf("cancel: %v", err)
	}
	got = r.until(lastEnding)
	r.wantShapes(got,
		`queue queued "three"`,
		`queue sent "two"`,
		`ended turn-1 stop="end_turn" next="turn-2" pending=1`,
		`started turn-2 drain "two"`,
		`text "echo: two"`,
		`done end_turn`,
		`queue sent "three"`,
		`ended turn-2 stop="end_turn" next="turn-3" pending=0`,
		`started turn-3 drain "three"`,
		`text "echo: three"`,
		`done end_turn`,
		`ended turn-3 stop="end_turn" next="" pending=0`,
	)
	// The turns that started next were never cancelled.
	for _, ev := range r.seen {
		if ev.Type == agent.EventDone && ev.StopReason == stopCancelled {
			t.Fatalf("a later turn was cancelled: %s", describe(r.seen))
		}
	}
}

// TestCancelOutcomes: requested while the turn is still ending, settled once it
// is no longer current, unknown when the call gave up.
func TestCancelOutcomes(t *testing.T) {
	t.Run("settled", func(t *testing.T) {
		r := newRig(t, Options{})
		turn := r.s.script(held())
		res := r.submit("one")
		await(t, turn.opened, "the turn to open")
		// The turn is over by the time the session's cancel returns: the
		// cancel is held at the session's exit until the turn has come back.
		back := make(chan struct{})
		var once sync.Once
		r.e.hooks = &hooks{
			turnReturned:       func(string) { once.Do(func() { close(back) }) },
			afterSessionCancel: func(string) { await(t, back, "the cancelled turn to come back") },
		}
		got, err := r.e.Cancel(context.Background(), Command{}, res.Turn)
		if err != nil || got.Outcome != CancelSettled || got.Turn != "turn-1" {
			t.Fatalf("cancel answered %+v, %v", got, err)
		}
	})
	t.Run("requested", func(t *testing.T) {
		r := newRig(t, Options{})
		// A turn that does not end when it is cancelled: the cancel is written
		// and the turn is still current when the call returns.
		turn := r.s.script(held())
		res := r.submit("one")
		await(t, turn.opened, "the turn to open")
		r.s.mu.Lock()
		r.s.ignoreCancel = true
		r.s.mu.Unlock()
		got, err := r.e.Cancel(context.Background(), Command{}, res.Turn)
		if err != nil || got.Outcome != CancelRequested {
			t.Fatalf("cancel answered %+v, %v", got, err)
		}
		turn.release()
		r.until(lastEnding)
	})
	t.Run("unknown", func(t *testing.T) {
		r := newRig(t, Options{})
		turn := r.s.script(held())
		res := r.submit("one")
		await(t, turn.opened, "the turn to open")
		boom := errors.New("the pipe is gone")
		r.s.mu.Lock()
		r.s.cancelErr = boom
		r.s.mu.Unlock()
		got, err := r.e.Cancel(context.Background(), Command{}, res.Turn)
		if !errors.Is(err, boom) || got.Outcome != CancelUnknown {
			t.Fatalf("cancel answered %+v, %v", got, err)
		}
		// A cancel that failed holds nothing afterwards: the turn still ends
		// and the engine still admits.
		turn.release()
		r.until(lastEnding)
		r.submit("two")
		r.until(lastEnding)
	})
	t.Run("no turn of craze's own", func(t *testing.T) {
		r := newRig(t, Options{})
		got, err := r.e.Cancel(context.Background(), Command{}, "")
		if err != nil || got.Outcome != CancelSettled || got.Turn != "" {
			t.Fatalf("cancel answered %+v, %v", got, err)
		}
		if n := r.s.cancelsWritten(); n != 1 {
			t.Fatalf("a cancel with no turn is written at once: %d written", n)
		}
	})
}

// TestStopClearsTheQueueThenCancels is what a signal does to `craze prompt`:
// the rows go first, so nothing starts behind the cancel, and each removal is
// an event, so the stream says where the follow-ups went.
func TestStopClearsTheQueueThenCancels(t *testing.T) {
	r := newRig(t, Options{Chain: ChainPolicy{StopOnNonEndTurn: true}})
	turn := r.s.script(held())
	r.submit("one")
	await(t, turn.opened, "the turn to open")
	r.submit("two")
	r.submit("three")
	r.sync()
	if err := r.e.Stop(context.Background(), Command{}); err != nil {
		t.Fatalf("stop: %v", err)
	}
	got := r.until(lastEnding)
	r.wantShapes(got,
		`started turn-1 submit "one"`,
		`queue queued "two"`,
		`queue queued "three"`,
		`queue removed "two"`,
		`queue removed "three"`,
		`done cancelled`,
		`ended turn-1 stop="cancelled" next="" pending=0`,
	)
	r.wantPrompts("one")
}
