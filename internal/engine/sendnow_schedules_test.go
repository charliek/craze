package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/charliek/craze/internal/agent"
)

// The schedules an armed send-now can meet, beyond its own arm-and-fire: a row
// edited under it, every verb that can take that row away, the steers a turn
// gives back, a stop, a foreign turn, and a saturated outbox.

// armOnARow is the state most of these start from: a held turn that takes its
// cancel without acting on it — so the test says when the turn ends and the
// order of the record is the test's to pin — one queued row, and a send armed on
// that row, with the record read up to the arm.
func armOnARow(t *testing.T, r *rig, text string) (*script, agent.QueuedPrompt) {
	t.Helper()
	turn := r.s.script(held())
	r.submit("one")
	await(t, turn.opened, "the turn to open")
	row := r.queue(text)
	r.ignoreCancels()
	r.sendNow(row.Text, row.ID)
	r.until(armedNow)
	return turn, row
}

// TestAnArmedRowIsSentAsItStandsWhenItFires: a row is held by id while a send is
// armed on it, never by the text it held at the time. An edit that lands in
// between is what goes, because the row in the queue is the one thing the user
// can still see and change — and because the arm consumed nothing, there is no
// second copy of that text anywhere to go stale.
func TestAnArmedRowIsSentAsItStandsWhenItFires(t *testing.T) {
	r := newRig(t, Options{})
	turn, row := armOnARow(t, r, "the first draft")
	if err := r.e.EditQueued(Command{}, row.ID, "what I actually meant", nil); err != nil {
		t.Fatalf("edit: %v", err)
	}
	r.sync()
	turn.release()
	r.until(started("turn-2"))
	r.wantShapes(r.seen,
		`started turn-1 submit "one"`,
		`queue queued "the first draft"`,
		armedShape("the first draft", row.ID, "turn-1"),
		// The edit reaches the row the send is holding by id, and the send is
		// untouched by it: it still names the same row.
		`queue edited "what I actually meant"`,
		`text "echo: one"`,
		`done end_turn`,
		`queue sent "what I actually meant"`,
		`ended turn-1 stop="end_turn" next="turn-2" pending=0`,
		`started turn-2 send_now "what I actually meant"`,
	)
	r.until(lastEnding)
	r.wantPrompts("one", "what I actually meant")
}

// TestAnArmedRowGoesExactlyOnce is the exactly-once property of a row a send is
// armed on, against the verbs that can take it away. Whichever happens first, the
// row leaves the queue either sent or removed — never both, which would send text
// the user cancelled, and never neither, which would lose it.
//
// The two orders are pinned separately, because each has its own answer, and then
// run against each other under the race detector, where the assertion is the
// invariant and not an order.
func TestAnArmedRowGoesExactlyOnce(t *testing.T) {
	t.Run("the removal lands first", func(t *testing.T) {
		r := newRig(t, Options{})
		turn, row := armOnARow(t, r, "the row")
		if _, err := r.e.Unqueue(Command{}, row.ID); err != nil {
			t.Fatalf("unqueue: %v", err)
		}
		r.sync()
		turn.release()
		wantRowWentOnce(t, r, row, "removed")
	})

	t.Run("the send fires first", func(t *testing.T) {
		r := newRig(t, Options{})
		turn, row := armOnARow(t, r, "the row")
		turn.release()
		// The settlement is what takes the row, and it runs only once the
		// continuation has returned AND the arm's own cancel has given its hold
		// back — two goroutines, in no fixed order, because a cancel the session
		// writes without acting on takes its hold back whenever it gets there. A
		// continuation that has merely come back therefore settles nothing, so the
		// barrier is the send's own started: everything below it is a verb arriving
		// too late, which is the other order.
		r.until(started("turn-2"))
		if _, err := r.e.Unqueue(Command{}, row.ID); !errors.Is(err, ErrUnknownRow) {
			t.Fatalf("unqueue of a row the send took: %v", err)
		}
		if n, err := r.e.ClearQueue(Command{}); err != nil || n != 0 {
			t.Fatalf("clear after the send took the row: %d, %v", n, err)
		}
		if _, err := r.e.Submit(Command{}, row.Text, SubmitQueue, row.ID); !errors.Is(err, ErrUnknownRow) {
			t.Fatalf("a submit naming the row the send took: %v", err)
		}
		wantRowWentOnce(t, r, row, "sent")
	})

	for _, tc := range []struct {
		name string
		take func(*rig, agent.QueuedPrompt) error
	}{
		{"unqueue", func(r *rig, row agent.QueuedPrompt) error { return errRow(r.e.Unqueue(Command{}, row.ID)) }},
		{"clear", func(r *rig, _ agent.QueuedPrompt) error { return errCount(r.e.ClearQueue(Command{})) }},
	} {
		t.Run("racing a "+tc.name, func(t *testing.T) {
			// Both orders are legal; the invariant is not. Each round releases the
			// turn and the verb from one barrier, so the interleaving is the
			// scheduler's and the assertion is the property.
			for i := 0; i < 20; i++ {
				r := newRig(t, Options{})
				turn, row := armOnARow(t, r, "the row")
				start := make(chan struct{})
				done := make(chan error, 1)
				go func() {
					<-start
					done <- tc.take(r, row)
				}()
				close(start)
				turn.release()
				if err := <-done; err != nil && !errors.Is(err, ErrUnknownRow) {
					t.Fatalf("%s: %v", tc.name, err)
				}
				wantRowWentOnce(t, r, row, "")
			}
		})
	}
}

// wantRowWentOnce reads the chain to its end and checks that the row left the
// queue exactly once — sent or removed, never both, never neither — and that the
// session was handed its text exactly as often as it was sent. want, when set, is
// which of the two it must have been.
func wantRowWentOnce(t *testing.T, r *rig, row agent.QueuedPrompt, want string) {
	t.Helper()
	r.until(lastEnding)
	sent, removed := 0, 0
	for _, ev := range r.seen {
		if ev.Type != agent.EventQueue || ev.Queue.ID != row.ID {
			continue
		}
		switch ev.QueueChange {
		case agent.QueueSent:
			sent++
		case agent.QueueRemoved:
			removed++
		}
	}
	if sent+removed != 1 {
		t.Fatalf("the row was sent %d times and removed %d, want exactly one of them: %s",
			sent, removed, describe(r.seen))
	}
	switch {
	case want == "sent" && sent != 1, want == "removed" && removed != 1:
		t.Fatalf("the row was not %s: %s", want, describe(r.seen))
	}
	texts := 0
	for _, got := range r.s.prompts() {
		if got == row.Text {
			texts++
		}
	}
	if texts != sent {
		t.Fatalf("the session was handed the row's text %d times and it was sent %d: %q", texts, sent, r.s.prompts())
	}
}

// TestAnArmedSendFiresAheadOfTheSteersTheTurnGaveBack: a turn can end owing both
// — steers it accepted and could not answer, and a send armed against it. The
// steers go back to the head of the queue and the send still goes first: it is
// what the client cancelled this turn for, while the steers are text the turn
// took and could not use. Both are ahead of whatever was queued before them.
func TestAnArmedSendFiresAheadOfTheSteersTheTurnGaveBack(t *testing.T) {
	r := newRig(t, Options{})
	turn := r.s.script(steering(held(), "steer one", "steer two"))
	// The send's own turn is held too, so the queue can be read while it runs
	// rather than after the chain behind it has drained.
	fired := r.s.script(held())
	r.submit("one")
	await(t, turn.opened, "the turn to open")
	r.queue("older")
	r.ignoreCancels()
	r.sendNow("instead of this turn", "")
	r.until(armedNow)
	turn.release()
	r.until(started("turn-2"))
	await(t, fired.opened, "the armed send's turn to open")
	r.wantShapes(r.seen,
		`started turn-1 submit "one"`,
		`queue queued "older"`,
		armedShape("instead of this turn", "", "turn-1"),
		`text "echo: one"`,
		`done end_turn`,
		// The requeue is part of the settlement's own batch, last first, so the
		// steers end up in the order they were typed, ahead of "older".
		`queue queued "steer two"`,
		`queue queued "steer one"`,
		`ended turn-1 stop="end_turn" next="turn-2" pending=3`,
		`started turn-2 send_now "instead of this turn"`,
	)
	r.wantRows("steer one", "steer two", "older")
	fired.release()
	r.until(lastEnding)
	r.wantPrompts("one", "instead of this turn", "steer one", "steer two", "older")
}

// TestSteersGivenBackAfterAStopAreRequeuedThenCleared: Stop clears the queue
// before the turn it cancelled has returned, so the steers that turn gives back
// arrive after that clear. They are requeued all the same and then cleared by the
// settlement, which is what puts a removal in the record for each of them: text
// the session accepted is never dropped without the stream accounting for it.
func TestSteersGivenBackAfterAStopAreRequeuedThenCleared(t *testing.T) {
	r := newRig(t, Options{})
	turn := r.s.script(steering(held(), "steered"))
	r.submit("one")
	await(t, turn.opened, "the turn to open")
	r.queue("older")
	r.ignoreCancels()
	r.sync()
	if err := r.e.Stop(context.Background(), Command{}); err != nil {
		t.Fatalf("stop: %v", err)
	}
	turn.release()
	r.until(lastEnding)
	r.wantShapes(r.seen,
		`started turn-1 submit "one"`,
		`queue queued "older"`,
		`queue removed "older"`,
		`text "echo: one"`,
		`done end_turn`,
		`queue queued "steered"`,
		`queue removed "steered"`,
		`ended turn-1 stop="end_turn" next="" pending=0`,
	)
	r.wantRows()
	r.wantPrompts("one")
}

// TestAnArmedSendSurvivesAForeignTurnAndFiresWhenItEnds: the settlement of the
// turn a send was armed against could start nothing — the agent had taken the
// session for a turn of its own — so the send stays armed, and the event that
// ends the foreign turn is what fires it. It is still ahead of the queue then.
func TestAnArmedSendSurvivesAForeignTurnAndFiresWhenItEnds(t *testing.T) {
	r := newRig(t, Options{})
	turn := r.s.script(held())
	r.submit("one")
	await(t, turn.opened, "the turn to open")
	row := r.queue("queued behind it")
	r.ignoreCancels()
	r.sendNow("now", "")
	r.until(armedNow)
	// Silently, so the settlement is not racing a wake-up: the flag is what it
	// reads, and the event below is what releases the send.
	r.s.setForeignSilently(true)
	turn.release()
	got := r.until(ended("turn-1"))
	if last := got[len(got)-1].Turn; last.Next != "" || last.Pending != 1 {
		t.Fatalf("the settlement behind a foreign turn: %+v", last)
	}
	st := r.e.State()
	if st.SendNow == nil || st.SendNow.Text != "now" || len(st.Queue) != 1 || st.Queue[0].ID != row.ID {
		t.Fatalf("state while the foreign turn runs: %+v", st)
	}
	r.wantPrompts("one")
	r.s.setForeign(false)
	r.until(started("turn-2"))
	r.wantShapes(r.seen[len(r.seen)-2:],
		`foreign running=false`,
		`started turn-2 send_now "now"`,
	)
	r.until(lastEnding)
	r.wantPrompts("one", "now", "queued behind it")
}

// TestADisarmIsDeliveredWithTheOutboxSaturated: a disarm is a completion, not an
// admission. Stop takes the armed send with it whether or not the outbox has
// room — an armed send a client is never told is gone would be a pending-send
// chip with nothing behind it — and the delta reaches the record once anything
// reads.
func TestADisarmIsDeliveredWithTheOutboxSaturated(t *testing.T) {
	s, e, _ := saturableEngine(t)
	turn := s.script(silently(held()))
	if _, err := e.Submit(Command{}, "one", SubmitQueue, ""); err != nil {
		t.Fatal(err)
	}
	await(t, turn.opened, "the turn to open")
	s.mu.Lock()
	s.ignoreCancel = true
	s.mu.Unlock()
	if res, err := e.Submit(Command{}, "now", SubmitSendNow, ""); err != nil || !res.Armed {
		t.Fatalf("arm: %+v, %v", res, err)
	}
	saturate(t, e)

	// Stop's barrier for its own removals cannot be met with the outbox wedged,
	// so it gives that wait up and makes no cancel at all — but the disarm it had
	// already enqueued stands, which is what this is about.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := e.Stop(ctx, Command{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("stop with an ended context: %v", err)
	}
	if e.log.OutboxRoom() {
		t.Fatal("the outbox came back under its bound before the disarm was checked")
	}
	if st := e.State(); st.SendNow != nil {
		t.Fatalf("Stop left a send armed: %+v", st.SendNow)
	}
	r := readerOn(t, s, e)
	got := r.until(disarmed)
	if last := got[len(got)-1]; last.State.Reason != agent.SendNowStopped {
		t.Fatalf("the disarm enqueued over the bound: %+v", last.State)
	}
	turn.release()
}
