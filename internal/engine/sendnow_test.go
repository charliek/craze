package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
)

// TestSendNowWhileNothingIsRunningIsAPlainStart: there is nothing to be strong
// about, so the send is simply admitted — with its own origin, because a client
// that draws a send-now row differently reads that rather than guessing.
func TestSendNowWhileNothingIsRunningIsAPlainStart(t *testing.T) {
	t.Run("a draft", func(t *testing.T) {
		r := newRig(t, Options{})
		res := r.sendNow("now", "")
		if res.Turn != "turn-1" || res.Armed || res.Queued != nil {
			t.Fatalf("a send-now on an idle engine answered %+v", res)
		}
		r.wantShapes(r.until(lastEnding),
			`started turn-1 send_now "now"`,
			`text "echo: now"`,
			`done end_turn`,
			`ended turn-1 stop="end_turn" next="" pending=0`,
		)
	})
	t.Run("a row", func(t *testing.T) {
		r := newRig(t, Options{})
		row := r.queue("the row")
		r.sync()
		res := r.sendNow(row.Text, row.ID)
		if res.Turn != "turn-1" || res.Armed {
			t.Fatalf("a send-now on a row with nothing running answered %+v", res)
		}
		r.wantShapes(r.until(lastEnding),
			`queue queued "the row"`,
			// The removal comes before the started: the band shrinks before the
			// user row appears, as it always has.
			`queue sent "the row"`,
			`started turn-1 send_now "the row"`,
			`text "echo: the row"`,
			`done end_turn`,
			`ended turn-1 stop="end_turn" next="" pending=0`,
		)
		r.wantRows()
		r.wantPrompts("the row")
	})
}

// TestOnlyOneSendNowIsArmedAtATime: two turns cannot both be the one that
// replaces this, so a second send-now is refused rather than queued behind the
// first — and the first is untouched by the refusal.
func TestOnlyOneSendNowIsArmedAtATime(t *testing.T) {
	r := newRig(t, Options{})
	turn := r.s.script(held())
	r.submit("one")
	await(t, turn.opened, "the turn to open")
	r.ignoreCancels()
	r.sendNow("first", "")
	r.until(armedNow)
	_, err := r.e.Submit(Command{}, "second", SubmitSendNow, "")
	if !errors.Is(err, ErrAlreadyPending) || Code(err) != "already_submitted" {
		t.Fatalf("a second send-now: %v (%s)", err, Code(err))
	}
	if st := r.e.State(); st.SendNow == nil || st.SendNow.Text != "first" {
		t.Fatalf("the refused send-now displaced the armed one: %+v", st.SendNow)
	}
	// The one that was armed is the one that fires; the refused text never
	// reaches the session at all.
	turn.release()
	r.until(lastEnding)
	r.wantPrompts("one", "first")
}

// TestAnArmedSendIsSilentUntilItFires: nothing leaves the queue, nothing is
// consumed, and no turn starts while a send is armed. The one event arming
// produces is the delta that says so, which is what a client turns into its
// pending-send chip.
func TestAnArmedSendIsSilentUntilItFires(t *testing.T) {
	r := newRig(t, Options{})
	turn := r.s.script(held())
	r.submit("one")
	await(t, turn.opened, "the turn to open")
	row := r.queue("the row")
	r.ignoreCancels()
	res := r.sendNow(row.Text, row.ID)
	if !res.Armed || res.Turn != "" || res.Queued != nil {
		t.Fatalf("arming answered %+v, want Armed alone", res)
	}
	r.sync()
	r.wantShapes(r.until(armedNow),
		`started turn-1 submit "one"`,
		`queue queued "the row"`,
		armedShape(row.Text, row.ID, "turn-1"),
	)
	// The row is still queued and the turn still running: the send has taken
	// nothing from anywhere.
	st := r.e.State()
	if st.Turn != "turn-1" || st.Activity != ActivityWorking || len(st.Queue) != 1 || st.Queue[0].ID != row.ID {
		t.Fatalf("state while a send is armed: %+v", st)
	}
	if st.SendNow == nil || st.SendNow.FromRow != row.ID || st.SendNow.Turn != "turn-1" {
		t.Fatalf("the armed send: %+v", st.SendNow)
	}
	r.wantPrompts("one")
}

// TestAnArmedSendFiresBeforeTheQueueHead is A10's order: the armed send is what
// the client cancelled a running turn for, so it goes ahead of everything that
// was merely waiting, and the row it named leaves the queue only now.
func TestAnArmedSendFiresBeforeTheQueueHead(t *testing.T) {
	r := newRig(t, Options{})
	turn := r.s.script(held())
	r.submit("one")
	await(t, turn.opened, "the turn to open")
	r.queue("queued first")
	row := r.queue("the row")
	// The turn takes its cancel without acting on it, so the order of the
	// record is the test's to pin: an engine event trails the state it
	// describes, and a cancel that ended the turn at once would race the arm's
	// own delta to the record.
	r.ignoreCancels()
	r.sendNow(row.Text, row.ID)
	r.until(armedNow)
	turn.release()
	r.until(lastEnding)
	r.wantShapes(r.seen,
		`started turn-1 submit "one"`,
		`queue queued "queued first"`,
		`queue queued "the row"`,
		armedShape(row.Text, row.ID, "turn-1"),
		`text "echo: one"`,
		`done end_turn`,
		// The armed send's row leaves the queue in the settlement's own batch,
		// before the ending that names the turn it started.
		`queue sent "the row"`,
		`ended turn-1 stop="end_turn" next="turn-2" pending=1`,
		`started turn-2 send_now "the row"`,
		`text "echo: the row"`,
		`done end_turn`,
		`queue sent "queued first"`,
		`ended turn-2 stop="end_turn" next="turn-3" pending=0`,
		`started turn-3 drain "queued first"`,
		`text "echo: queued first"`,
		`done end_turn`,
		`ended turn-3 stop="end_turn" next="" pending=0`,
	)
	// Exactly once: the session was handed the row's text one time, and the row
	// is gone from the queue.
	r.wantPrompts("one", "the row", "queued first")
}

// TestAFiredSendNowsPredecessorNamesItAsItsSuccessor is A7's half. The cancel a
// send-now issues is in flight exactly when its turn returns — the session's
// cancel waits for the prompt it cancelled — so a settlement that ran on the
// return alone would publish an ending with no successor, a client would go
// idle, and only then would the send fire. Settlement waits for the hold as
// well, so no ending between the cancelled turn and the armed send has an empty
// Next, and no status ever passes through idle.
func TestAFiredSendNowsPredecessorNamesItAsItsSuccessor(t *testing.T) {
	for i := 0; i < 20; i++ {
		r := newRig(t, Options{})
		turn := r.s.script(held())
		r.submit("one")
		await(t, turn.opened, "the turn to open")
		r.sendNow("now", "")
		for _, ev := range r.until(started("turn-2")) {
			if ended("")(ev) && ev.Turn.Next == "" {
				t.Fatalf("an ending with no successor came between the cancelled turn and the armed send: %s", describe(r.seen))
			}
		}
		if last := r.seen[len(r.seen)-1].Turn; last.Origin != agent.TurnOriginSendNow {
			t.Fatalf("the successor's origin is %q", last.Origin)
		}
		r.until(lastEnding)
	}
}

// TestEveryDisarmPathSaysWhy: each way an armed send can be lost is its own
// reason, because a client turns them into different words for the user, and
// each leaves the text where it was — the row still queued, nothing consumed,
// nothing prompted.
func TestEveryDisarmPathSaysWhy(t *testing.T) {
	// arm scripts a turn that takes a cancel without acting on it, submits it,
	// queues a row and arms a send-now on that row: the state every case below
	// starts from, with the record read up to the arm.
	arm := func(t *testing.T, r *rig) (*script, agent.QueuedPrompt) {
		t.Helper()
		turn := r.s.script(held())
		r.submit("one")
		await(t, turn.opened, "the turn to open")
		row := r.queue("the row")
		r.ignoreCancels()
		r.sendNow(row.Text, row.ID)
		r.until(armedNow)
		return turn, row
	}
	// wantDisarm reads to the disarm delta and checks its reason, the failure
	// behind it, and that nothing is armed any more. Each case then says for
	// itself where the text it was holding ended up.
	//
	// detail is what the delta must carry: the failure for a cancel the ENGINE
	// made, whose error reaches a client here or nowhere, and "" for everything
	// else — every other reason is a fact about the send rather than an error, and
	// a cancel a client asked for is answered with its error directly, so a detail
	// there too would be one failure reported twice.
	wantDisarm := func(t *testing.T, r *rig, reason, detail string) {
		t.Helper()
		got := r.until(disarmed)
		last := got[len(got)-1]
		if last.State.Reason != reason {
			t.Fatalf("the disarm's reason is %q, want %q\n%s", last.State.Reason, reason, describe(r.seen))
		}
		if last.State.Detail != detail {
			t.Fatalf("the disarm's detail is %q, want %q\n%s", last.State.Detail, detail, describe(r.seen))
		}
		if st := r.e.State(); st.SendNow != nil {
			t.Fatalf("something is still armed: %+v", st.SendNow)
		}
	}
	// wantRowUntouched is the claim every disarm makes about a row it named: it
	// is exactly where it was, and nothing was sent in its place.
	wantRowUntouched := func(t *testing.T, r *rig, row agent.QueuedPrompt) {
		t.Helper()
		r.wantPrompts("one")
		if st := r.e.State(); len(st.Queue) != 1 || st.Queue[0].ID != row.ID || st.Queue[0].Text != row.Text {
			t.Fatalf("the row the send named is not where it was: %+v", st.Queue)
		}
	}

	t.Run("a client took it back", func(t *testing.T) {
		r := newRig(t, Options{})
		turn := r.s.script(held())
		client := r.e.NewClientID()
		r.submit("one")
		await(t, turn.opened, "the turn to open")
		row := r.queue("the row")
		r.ignoreCancels()
		armCmd := Command{Client: client, ID: "1"}
		if _, err := r.e.Submit(armCmd, row.Text, SubmitSendNow, row.ID); err != nil {
			t.Fatalf("arm: %v", err)
		}
		if got := r.until(armedNow); got[len(got)-1].Cause != armCmd.Cause() {
			t.Fatalf("the arm's delta names %q, want the command that armed it", got[len(got)-1].Cause)
		}
		disarmCmd := Command{Client: client, ID: "2"}
		if err := r.e.Disarm(disarmCmd); err != nil {
			t.Fatalf("disarm: %v", err)
		}
		wantDisarm(t, r, agent.SendNowWithdrawn, "")
		if got := r.seen[len(r.seen)-1]; got.Cause != disarmCmd.Cause() {
			t.Fatalf("the disarm's delta names %q, want the command that withdrew it", got.Cause)
		}
		wantRowUntouched(t, r, row)
		// Nothing left to take back, and saying so is not the same as saying
		// "done".
		if err := r.e.Disarm(Command{}); !errors.Is(err, ErrNotAccepting) {
			t.Fatalf("a second disarm: %v", err)
		}
	})

	// A cancel that fails and one that gives up on its context are the same
	// disarm: both leave the turn the send was armed against running, so its
	// settlement — the moment the send was waiting for — is not coming. The
	// context case is what a live session now answers when it abandons a
	// session/cancel parked in an agent's pipe (live.go's writeCancel), which is
	// the failure mode that used to hold the hold for good.
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"the cancel it issued failed", errors.New("the pipe is gone")},
		{"the cancel it issued gave up on its context", context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, Options{})
			turn := r.s.script(held())
			r.submit("one")
			await(t, turn.opened, "the turn to open")
			row := r.queue("the row")
			r.s.mu.Lock()
			r.s.cancelErr = tc.err
			r.s.mu.Unlock()
			r.sendNow(row.Text, row.ID)
			// The turn the cancel did not stop is still running, so the send is
			// waiting for an ending that is not coming: it goes, and the text
			// stays where it was — the row in the queue, a draft in the client's
			// composer — as the TUI's cancelFailedMsg has always left it.
			// The delta carries the failure itself, as text. This is the one
			// cancel a client did not make, so a client that draws that failure —
			// as the TUI's cancelFailedMsg always has — can read it nowhere else.
			wantDisarm(t, r, agent.SendNowCancelFailed, tc.err.Error())
			wantRowUntouched(t, r, row)
			if st := r.e.State(); st.Turn != "turn-1" || st.Activity != ActivityWorking {
				t.Fatalf("the turn the failed cancel left running: %+v", st)
			}
			// And the hold that cancel took is back, whatever it answered: the
			// turn settles when it ends, and the row behind it runs.
			r.e.mu.Lock()
			holds := r.e.cancelsInFlight
			r.e.mu.Unlock()
			if holds != 0 {
				t.Fatalf("%d holds stand after a cancel that did not reach the agent", holds)
			}
			turn.release()
			r.until(lastEnding)
			r.wantPrompts("one", "the row")
		})
	}

	// The engine's own cancel is reported whether or not the arm it was made for
	// is still standing. A client that takes its send back does not make the
	// cancel's failure somebody else's news: it did not reach the agent, the turn
	// it was for is still running, and nobody is waiting on an error the engine
	// discarded. With no section to ride on, the delta carries the reason and the
	// failure alone.
	t.Run("the cancel it issued failed after the send was taken back", func(t *testing.T) {
		r := newRig(t, Options{})
		turn := r.s.script(held())
		r.submit("one")
		await(t, turn.opened, "the turn to open")
		row := r.queue("the row")
		boom := errors.New("the pipe is gone")
		entered, release := r.s.holdNextCancel()
		r.s.mu.Lock()
		r.s.cancelErr = boom
		r.s.mu.Unlock()
		r.sendNow(row.Text, row.ID)
		r.until(armedNow)
		await(t, entered, "the arm's cancel reaching the session")

		// Taken back while that cancel is still held, so there is no send-now
		// section left for its failure to be reported beside.
		if err := r.e.Disarm(Command{}); err != nil {
			t.Fatalf("disarm: %v", err)
		}
		r.until(disarmed)
		release()

		got := r.until(func(ev agent.Event) bool {
			return ev.Type == agent.EventMeta && ev.State != nil && ev.State.Detail != ""
		})
		last := got[len(got)-1]
		if last.State.SendNow != nil {
			t.Fatalf("the report touched the send-now section: %+v", last.State.SendNow)
		}
		if last.State.Reason != agent.SendNowCancelFailed || last.State.Detail != boom.Error() {
			t.Fatalf("the report is reason %q detail %q, want cancel_failed and the failure",
				last.State.Reason, last.State.Detail)
		}
		// The turn the cancel never stopped is still running, and the row is where
		// it was.
		if st := r.e.State(); st.Turn != "turn-1" || st.Activity != ActivityWorking {
			t.Fatalf("the turn the failed cancel left running: %+v", st)
		}
		wantRowUntouched(t, r, row)
		turn.release()
		r.until(lastEnding)
		r.wantPrompts("one", "the row")
	})

	// A cancel a CLIENT made answers that client with its error, so the disarm it
	// causes carries the reason and no detail: two reports of one failure would
	// draw the same row twice.
	t.Run("a client's failed cancel reports its failure once", func(t *testing.T) {
		r := newRig(t, Options{})
		turn := r.s.script(held())
		r.submit("one")
		await(t, turn.opened, "the turn to open")
		row := r.queue("the row")
		r.ignoreCancels()

		// Two cancels against the same turn, each held at the session's door so
		// that which of them the failure lands on is the test's choice and not a
		// race: the engine's own, for the arm, and the client's.
		armEntered, armRelease := r.s.holdNextCancel()
		r.sendNow(row.Text, row.ID)
		r.until(armedNow)
		await(t, armEntered, "the arm's cancel reaching the session")

		clientEntered, clientRelease := r.s.holdNextCancel()
		type answer struct {
			res CancelResult
			err error
		}
		cancelled := make(chan answer, 1)
		go func() {
			res, err := r.e.Cancel(context.Background(), Command{}, "turn-1")
			cancelled <- answer{res, err}
		}()
		await(t, clientEntered, "the client's cancel reaching the session")

		// The client's is the one that fails, and it is released first.
		boom := errors.New("the pipe is gone")
		r.s.mu.Lock()
		r.s.cancelErr = boom
		r.s.mu.Unlock()
		clientRelease()
		var got answer
		select {
		case got = <-cancelled:
			if !errors.Is(got.err, boom) {
				t.Fatalf("the client's cancel answered %v, want the failure", got.err)
			}
		case <-time.After(watchdog):
			t.Fatalf("the client's cancel never returned in %s", watchdog)
		}
		// The failure rides on the disarm the cancel owed anyway, so what was lost
		// and why arrive together and in one order — and the caller is told, so it
		// does not word the same failure a second time from the error it also has.
		if !got.res.Reported {
			t.Fatal("the cancel's answer does not say its failure was published")
		}
		wantDisarm(t, r, agent.SendNowCancelFailed, boom.Error())
		wantRowUntouched(t, r, row)
		armRelease()
		turn.release()
		r.until(lastEnding)
	})

	t.Run("the turn it was armed against failed", func(t *testing.T) {
		r := newRig(t, Options{})
		boom := errors.New("the turn failed")
		turn := r.s.script(failing(held(), boom))
		r.submit("one")
		await(t, turn.opened, "the turn to open")
		row := r.queue("the row")
		r.ignoreCancels()
		r.sendNow(row.Text, row.ID)
		r.until(armedNow)
		turn.release()
		got := r.until(disarmed)
		r.wantShapes(got[len(got)-4:],
			`error`,
			// The queue goes with the error, the row included, so this is the
			// one path where the row is not where it was: nothing drains from
			// an error state.
			`queue removed "the row"`,
			`ended turn-1 stop="" next="" pending=0 class=other`,
			`disarmed turn_failed`,
		)
		if st := r.e.State(); st.SendNow != nil || st.Activity != ActivityError || len(st.Queue) != 0 {
			t.Fatalf("state after a failed turn with a send armed: %+v", st)
		}
		r.wantPrompts("one")
	})

	t.Run("the turn that settled was not the one it was armed against", func(t *testing.T) {
		// A draft and an empty queue, so that nothing but the direct submit
		// below can start a turn: this schedule is about a send that outlived
		// the settlement it was armed for.
		r := newRig(t, Options{})
		turn := r.s.script(held())
		r.submit("one")
		await(t, turn.opened, "the turn to open")
		r.ignoreCancels()
		r.sendNow("still in the composer", "")
		r.until(armedNow)
		// The agent takes the session for a turn of its own, so the settlement
		// of the turn the send was armed against can start nothing at all and
		// the send survives it. Silently: an event would wake the driver, and
		// this schedule is about what happens when nothing does.
		r.s.setForeignSilently(true)
		turn.release()
		// Reading turn-1's ending proves no cancel is in flight any more:
		// settlement runs only when the hold has been released.
		got := r.until(ended("turn-1"))
		if last := got[len(got)-1].Turn; last.Next != "" || last.Pending != 0 {
			t.Fatalf("the settlement behind a foreign turn: %+v", last)
		}
		if st := r.e.State(); st.SendNow == nil {
			t.Fatalf("the armed send did not survive a settlement it could not fire in")
		}
		// The foreign turn ends with nothing said, and a direct submit — not the
		// drain, which would have fired the armed send first — takes the turn
		// that follows.
		r.s.setForeignSilently(false)
		second := r.s.script(held())
		if res, err := r.e.Submit(Command{}, "direct", SubmitQueue, ""); err != nil || res.Turn != "turn-2" {
			t.Fatalf("the direct submit answered %+v, %v", res, err)
		}
		await(t, second.opened, "the second turn to open")
		second.release()
		wantDisarm(t, r, agent.SendNowOtherTurn, "")
		// The armed text never went anywhere: it is still the client's, in the
		// composer it was typed in.
		r.wantPrompts("one", "direct")
	})

	t.Run("the row it named had gone", func(t *testing.T) {
		r := newRig(t, Options{})
		turn := r.s.script(held())
		r.submit("one")
		await(t, turn.opened, "the turn to open")
		gone := r.queue("the row that goes")
		stays := r.queue("the row that stays")
		r.ignoreCancels()
		r.sendNow(gone.Text, gone.ID)
		r.until(armedNow)
		if _, err := r.e.Unqueue(Command{}, gone.ID); err != nil {
			t.Fatalf("unqueue: %v", err)
		}
		r.sync()
		turn.release()
		r.until(started("turn-2"))
		r.wantShapes(r.seen,
			`started turn-1 submit "one"`,
			`queue queued "the row that goes"`,
			`queue queued "the row that stays"`,
			armedShape(gone.Text, gone.ID, "turn-1"),
			`queue removed "the row that goes"`,
			`text "echo: one"`,
			`done end_turn`,
			// The send falls through with its reason and the head of the queue
			// takes its place — as an ordinary drain, not as the send.
			`queue sent "the row that stays"`,
			`ended turn-1 stop="end_turn" next="turn-2" pending=0`,
			`disarmed row_gone`,
			`started turn-2 drain "the row that stays"`,
		)
		r.until(lastEnding)
		r.wantPrompts("one", stays.Text)
	})

	t.Run("the row it named went with a cleared queue", func(t *testing.T) {
		r := newRig(t, Options{})
		turn, _ := arm(t, r)
		if rows, err := r.e.ClearQueue(Command{}); err != nil || len(rows) != 1 {
			t.Fatalf("clear answered %+v, %v", rows, err)
		}
		r.sync()
		turn.release()
		wantDisarm(t, r, agent.SendNowRowGone, "")
		// The text is where the clear left it: gone, by the client's own command,
		// with a removal event for it. Nothing was sent in its place either — the
		// queue the send would have fallen through to is empty too.
		r.wantPrompts("one")
		r.wantRows()
	})

	t.Run("the engine was stopped", func(t *testing.T) {
		r := newRig(t, Options{})
		arm(t, r)
		if err := r.e.Stop(context.Background(), Command{}); err != nil {
			t.Fatalf("stop: %v", err)
		}
		got := r.until(disarmed)
		r.wantShapes(got[len(got)-2:],
			// Stop clears the queue and the armed send with it: nothing may be
			// admitted afterwards, so the turn it was waiting for will settle
			// into nothing at all. The row goes with the rest of the queue —
			// deliberately, and with its removal in the record, which is how a
			// client says where a follow-up went.
			`queue removed "the row"`,
			`disarmed stopped`,
		)
		if st := r.e.State(); st.SendNow != nil || len(st.Queue) != 0 {
			t.Fatalf("state after Stop with a send armed: %+v", st)
		}
		r.wantPrompts("one")
	})

	t.Run("the session is closing", func(t *testing.T) {
		// A primary, read after Close: the log's close commits what the outbox
		// still holds with a non-blocking send to a primary nobody has filled,
		// and never closes the channel, so the disarm is in its buffer when
		// Close returns. A subscription is cut off instead, which is the one
		// reader that cannot be relied on for an event enqueued inside Close.
		r := newRigOn(t, Options{}, agent.EventLogOptions{})
		_, row := arm(t, r)
		if err := r.e.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
		var last *agent.StateDelta
		for done := false; !done; {
			select {
			case ev := <-r.e.Events():
				if disarmed(ev) {
					last = ev.State
				}
			default:
				done = true
			}
		}
		if last == nil || last.Reason != agent.SendNowClosing {
			t.Fatalf("the disarm at Close: %+v", last)
		}
		if st := r.e.State(); st.SendNow != nil {
			t.Fatalf("something is still armed after Close: %+v", st.SendNow)
		}
		// The text is where it was: the row is still in the queue the session is
		// going away with — Close clears nothing, because there is nobody left to
		// tell — and the send never reached the session.
		if st := r.e.State(); len(st.Queue) != 1 || st.Queue[0].ID != row.ID || st.Queue[0].Text != row.Text {
			t.Fatalf("the row the send named is not where it was: %+v", st.Queue)
		}
		r.wantPrompts("one")
	})
}

// TestAnArmedSendFiresEvenWhereTheChainPolicyEndsTheChain: a chain policy that
// clears the queue on a stop other than end_turn is about the follow-ups queued
// behind a turn, not about text a client asked to run *instead* of it. An armed
// send that the policy swallowed would be left armed against a turn that has
// gone, with no reason event and no way ever to fire, so it fires — and the
// queue the policy cleared is still cleared.
func TestAnArmedSendFiresEvenWhereTheChainPolicyEndsTheChain(t *testing.T) {
	r := newRig(t, Options{Chain: ChainPolicy{StopOnNonEndTurn: true}})
	turn := r.s.script(stopping(held(), "max_turn_requests"))
	r.submit("one")
	await(t, turn.opened, "the turn to open")
	r.queue("a follow-up the policy drops")
	r.ignoreCancels()
	r.sendNow("instead of this turn", "")
	r.until(armedNow)
	turn.release()
	r.until(lastEnding)
	r.wantShapes(r.seen,
		`started turn-1 submit "one"`,
		`queue queued "a follow-up the policy drops"`,
		armedShape("instead of this turn", "", "turn-1"),
		`text "echo: one"`,
		`done max_turn_requests`,
		`queue removed "a follow-up the policy drops"`,
		`ended turn-1 stop="max_turn_requests" next="turn-2" pending=0`,
		`started turn-2 send_now "instead of this turn"`,
		`text "echo: instead of this turn"`,
		`done end_turn`,
		`ended turn-2 stop="end_turn" next="" pending=0`,
	)
	r.wantPrompts("one", "instead of this turn")
}

// TestASendNowsCancelGoesThroughTheHold: the cancel an arm issues is the same
// cancel Control.Cancel makes, with the same hold taken in the section that
// armed — so between the arm and the cancel reaching the session nothing is
// admitted by any path, and the cancel therefore lands on the turn the send was
// armed against or on none.
//
// Every admission path is tried under the hold: a plain submit, and a submit that
// names a queued row.
func TestASendNowsCancelGoesThroughTheHold(t *testing.T) {
	r, returned := newRigReturning(t, Options{})
	turn := r.s.script(held())
	r.submit("one")
	await(t, turn.opened, "the turn to open")
	row := r.queue("a row of its own")

	entered, release := r.s.holdNextCancel()
	r.sendNow("now", "")
	await(t, entered, "the arm's cancel to reach the session")

	// The turn comes back on its own while the cancel is parked at the session's
	// door. Its done says only that the session has finished with it; what this
	// test turns on is the engine having had its chance to settle it, so it waits
	// for the continuation to come back and the pass that return allows to be
	// over.
	turn.release()
	r.until(func(ev agent.Event) bool { return ev.Type == agent.EventDone })
	awaitTurn(t, returned, "turn-1")

	// Every admission path is shut: a submit queues instead of starting, a submit
	// that names a row leaves it queued and sends nothing, the armed send has not
	// fired, and the turn has not settled.
	res, err := r.e.Submit(Command{}, "three", SubmitQueue, "")
	if err != nil || res.Queued == nil {
		t.Fatalf("a submit under the arm's hold answered %+v, %v", res, err)
	}
	res, err = r.e.Submit(Command{}, row.Text, SubmitQueue, row.ID)
	if err != nil || res.Queued == nil || res.Queued.ID != row.ID {
		t.Fatalf("a row-sourced submit under the arm's hold answered %+v, %v", res, err)
	}
	r.sync()
	r.wantPrompts("one")
	if st := r.e.State(); st.Turn != "turn-1" || st.Activity != ActivityWorking || st.SendNow == nil {
		t.Fatalf("the turn settled under the arm's hold: %+v", st)
	}
	r.wantRows("a row of its own", "three")

	// The cancel goes through, the turn settles, and the armed send is the
	// successor — ahead of the rows that were queued behind it.
	release()
	got := r.until(started("turn-2"))
	r.wantShapes(got[len(got)-3:],
		`queue queued "three"`,
		`ended turn-1 stop="end_turn" next="turn-2" pending=2`,
		`started turn-2 send_now "now"`,
	)
	r.until(lastEnding)
	r.wantPrompts("one", "now", "a row of its own", "three")
}

// TestAnImmediateSendNowUnderACancelHoldIsRefused is the other half of the hold,
// where there is no turn to arm against: a cancel with no turn of craze's own is
// on its way to the agent, and a prompt started now could be what it lands on.
// There is nothing to be strong about and nothing may start, so the send-now is
// refused rather than quietly queued — the text stays the client's, which is
// what a send-now promises.
func TestAnImmediateSendNowUnderACancelHoldIsRefused(t *testing.T) {
	r := newRig(t, Options{})
	// A cancel with no turn of craze's own is accepted only when there is
	// something for it to do (§3.7); an ask the agent is waiting on is one.
	r.s.openAsk(t)
	entered, release := r.s.holdNextCancel()
	done := make(chan error, 1)
	go func() {
		_, err := r.e.Cancel(context.Background(), Command{}, "")
		done <- err
	}()
	await(t, entered, "the cancel to reach the session")
	if _, err := r.e.Submit(Command{}, "now", SubmitSendNow, ""); !errors.Is(err, ErrNotAccepting) || Code(err) != "not_accepting" {
		t.Fatalf("a send-now under a cancel with no turn: %v (%s)", err, Code(err))
	}
	if st := r.e.State(); st.SendNow != nil {
		t.Fatalf("it armed against nothing: %+v", st.SendNow)
	}
	release()
	if err := <-done; err != nil {
		t.Fatalf("cancel: %v", err)
	}
	r.sync()
	r.wantPrompts()
}

// TestCloseJoinsTheCancelASendNowAsked: the cancel runs on a goroutine the
// engine owns and counts, so Close waits for it — even parked inside
// Session.Cancel, which the session's own close releases.
func TestCloseJoinsTheCancelASendNowAsked(t *testing.T) {
	r := newRig(t, Options{})
	turn := r.s.script(held())
	r.submit("one")
	await(t, turn.opened, "the turn to open")
	entered, release := r.s.holdNextCancel()
	defer release()
	r.sendNow("now", "")
	await(t, entered, "the arm's cancel to reach the session")
	closed := make(chan error, 1)
	go func() { closed <- r.e.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("close: %v", err)
		}
	case <-time.After(watchdog):
		t.Fatal("Close is waiting on the cancel an arm asked for")
	}
	r.wantPrompts("one")
}

// TestSendNowDuringAForeignTurnIsRefused: the agent has the session for a turn
// of its own, and nothing can be sent into that — 05's gate table gives the
// refusal its own code, because a client keeps the user's draft on it.
func TestSendNowDuringAForeignTurnIsRefused(t *testing.T) {
	r := newRig(t, Options{})
	r.s.setForeign(true)
	r.until(func(ev agent.Event) bool { return ev.Type == agent.EventForeignTurn })
	if _, err := r.e.Submit(Command{}, "now", SubmitSendNow, ""); !errors.Is(err, agent.ErrForeignTurn) || Code(err) != "foreign_turn" {
		t.Fatalf("a send-now during a foreign turn: %v (%s)", err, Code(err))
	}
	if st := r.e.State(); st.SendNow != nil {
		t.Fatalf("the refused send-now armed anyway: %+v", st.SendNow)
	}
	// And while craze's own turn runs behind one, where there is no ending for
	// an armed send to fire after either.
	turn := r.s.script(held())
	r.s.setForeignSilently(false)
	r.submit("one")
	await(t, turn.opened, "the turn to open")
	r.s.setForeignSilently(true)
	if _, err := r.e.Submit(Command{}, "now", SubmitSendNow, ""); !errors.Is(err, agent.ErrForeignTurn) {
		t.Fatalf("a send-now while a foreign turn overlays a working turn: %v", err)
	}
	if n := r.s.cancelsWritten(); n != 0 {
		t.Fatalf("a refused send-now wrote %d cancels", n)
	}
	turn.release()
}

// TestSendNowOnARowThatIsNotThere: a row id the queue does not hold is refused
// at the arm, before anything is mutated, rather than discovered when the send
// comes to fire.
func TestSendNowOnARowThatIsNotThere(t *testing.T) {
	r := newRig(t, Options{})
	turn := r.s.script(held())
	r.submit("one")
	await(t, turn.opened, "the turn to open")
	_, err := r.e.Submit(Command{}, "gone", SubmitSendNow, "q-404")
	if !errors.Is(err, ErrUnknownRow) || Code(err) != "unknown_row" {
		t.Fatalf("a send-now on an unknown row: %v (%s)", err, Code(err))
	}
	if st := r.e.State(); st.SendNow != nil {
		t.Fatalf("it armed anyway: %+v", st.SendNow)
	}
	if n := r.s.cancelsWritten(); n != 0 {
		t.Fatalf("a refused send-now wrote %d cancels", n)
	}
	turn.release()
	r.until(lastEnding)
}

// TestASubmittedRowThatCannotStartStaysQueued: queue mode is "start now, else
// queue", and a row is already queued, so a row-sourced submit behind a running
// turn changes nothing and says nothing — the answer is the row itself, still
// waiting. A client that meant "instead of this turn" says send_now.
func TestASubmittedRowThatCannotStartStaysQueued(t *testing.T) {
	r := newRig(t, Options{})
	turn := r.s.script(held())
	r.submit("one")
	await(t, turn.opened, "the turn to open")
	row := r.queue("the row")
	r.sync()
	res, err := r.e.Submit(Command{}, row.Text, SubmitQueue, row.ID)
	if err != nil {
		t.Fatalf("a row-sourced submit behind a running turn: %v", err)
	}
	if res.Queued == nil || res.Queued.ID != row.ID || res.Turn != "" || res.Armed {
		t.Fatalf("it answered %+v, want the row still waiting", res)
	}
	r.sync()
	r.wantRows("the row")
	turn.release()
	r.until(lastEnding)
	r.wantPrompts("one", "the row")
}
