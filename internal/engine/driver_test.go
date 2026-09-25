package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
)

// TestSubmitStartsATurnAndSettlesIt is the driver's plain case, and pins the
// order of one turn's record: started, the agent's own events, then the ending
// — the last event a chain with nothing behind it produces.
func TestSubmitStartsATurnAndSettlesIt(t *testing.T) {
	r := newRig(t, Options{})
	res := r.submit("hello")
	if res.Turn != "turn-1" || res.Queued != nil || res.Armed {
		t.Fatalf("submit answered %+v, want the turn it started", res)
	}
	got := r.until(lastEnding)
	r.wantShapes(got,
		`started turn-1 submit "hello"`,
		`text "echo: hello"`,
		`done end_turn`,
		`ended turn-1 stop="end_turn" next="" pending=0`,
	)
	st := r.e.State()
	if st.Activity != ActivityIdle || st.Turn != "" || !st.Prompted || st.Cancelled {
		t.Fatalf("state after a settled turn: %+v", st)
	}
}

// TestSubmitWhileWorkingQueuesAndTheSettlementDrains: a row submitted behind a
// running turn starts exactly when that turn settles, and the settlement is one
// batch — the row leaving the queue, the ending naming its successor, the
// successor's started — so no client ever sees an idle between the two.
func TestSubmitWhileWorkingQueuesAndTheSettlementDrains(t *testing.T) {
	r := newRig(t, Options{})
	first := r.s.script(held())
	r.submit("one")
	await(t, first.opened, "the first turn to open")
	res := r.submit("two")
	if res.Queued == nil || res.Queued.Text != "two" || res.Turn != "" {
		t.Fatalf("a submit behind a running turn answered %+v, want the queued row", res)
	}
	// An engine event trails the state it describes: the row is queued, and
	// its event is still on its way. A test that pins the order of the record
	// lets the outbox through before it lets the agent speak.
	r.sync()
	first.release()
	got := r.until(lastEnding)
	r.wantShapes(got,
		`started turn-1 submit "one"`,
		`queue queued "two"`,
		`text "echo: one"`,
		`done end_turn`,
		`queue sent "two"`,
		`ended turn-1 stop="end_turn" next="turn-2" pending=0`,
		`started turn-2 drain "two"`,
		`text "echo: two"`,
		`done end_turn`,
		`ended turn-2 stop="end_turn" next="" pending=0`,
	)
	r.wantPrompts("one", "two")
}

// TestASessionWithNoClientDrainsItself: one submit and three rows behind it run
// to idle with nobody reading a primary — the log has none — and nobody
// driving. It is what a headless host is.
func TestASessionWithNoClientDrainsItself(t *testing.T) {
	r := newRig(t, Options{})
	first := r.s.script(held())
	r.submit("one")
	await(t, first.opened, "the first turn to open")
	for _, text := range []string{"two", "three", "four"} {
		r.submit(text)
	}
	first.release()
	r.until(lastEnding)
	r.wantPrompts("one", "two", "three", "four")
	if st := r.e.State(); st.Activity != ActivityIdle || len(st.Queue) != 0 {
		t.Fatalf("state: %+v", st)
	}
}

// TestSubmitNeverWaitsOnAFullPrimary: the caller of Submit is, in the TUI, the
// primary's own reader. With the primary full and nobody reading, Submit and a
// queued Submit both return; what they caused is delivered once somebody reads.
func TestSubmitNeverWaitsOnAFullPrimary(t *testing.T) {
	s := newFake(t, agent.EventLogOptions{})
	e, err := New(s, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	if err := e.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	const primaryCap = 256
	for i := 0; i < primaryCap; i++ {
		s.emit(agent.Event{Type: agent.EventText, Text: "fill"})
	}
	turn := s.script(held())
	done := make(chan SubmitResult, 2)
	go func() {
		for _, text := range []string{"one", "two"} {
			res, err := e.Submit(Command{}, text, SubmitQueue, "")
			if err != nil {
				t.Errorf("submit %q: %v", text, err)
			}
			done <- res
		}
	}()
	for _, want := range []string{"the first submit", "the second submit"} {
		select {
		case <-done:
		case <-time.After(watchdog):
			t.Fatalf("%s is waiting on a primary nobody reads", want)
		}
	}
	// Reading releases the outbox, then the turn's own Flush, then the turn.
	var sawStarted bool
	for !sawStarted {
		select {
		case ev := <-e.Events():
			sawStarted = started("turn-1")(ev)
		case <-time.After(watchdog):
			t.Fatal("the started never reached the primary")
		}
	}
	turn.release()
}

// TestCancelRightAfterSubmitWithdrawsThePrompt: the claim is taken inside
// Submit, so a cancel that arrives before the continuation has run finds a
// claimed prompt, which withdraws — nothing reaches the agent, and the turn
// ends cancelled with an ending the engine authored, since the session says
// nothing at all about it.
func TestCancelRightAfterSubmitWithdrawsThePrompt(t *testing.T) {
	// A primary nobody reads holds the turn's goroutine at its Flush, which is
	// the window: the turn is reserved and claimed, and has not opened.
	s := newFake(t, agent.EventLogOptions{})
	e, err := New(s, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	sub, err := e.Subscribe(agent.SubscribeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sub.Close)
	if err := e.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 256; i++ {
		s.emit(agent.Event{Type: agent.EventText, Text: "fill"})
	}
	r := &rig{t: t, s: s, e: e, sub: sub}
	for i := 0; i < 256; i++ {
		r.next()
	}
	res := r.submit("stop me")
	cres, err := e.Cancel(context.Background(), Command{}, res.Turn)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if cres.Turn != "turn-1" {
		t.Fatalf("the cancel was held against %q", cres.Turn)
	}
	if n := s.cancelsWritten(); n != 0 {
		t.Fatalf("%d cancels reached the agent for a prompt that never did", n)
	}
	// Now let the outbox through: the turn's goroutine runs the continuation,
	// which withdraws.
	go func() {
		for range e.Events() {
		}
	}()
	got := r.until(lastEnding)
	r.wantShapes(got,
		`started turn-1 submit "stop me"`,
		`ended turn-1 stop="cancelled" next="" pending=0 synthetic`,
	)
	if st := e.State(); st.Activity != ActivityIdle || !st.Cancelled {
		t.Fatalf("state: %+v", st)
	}
}

// TestACancelledTurnWithARowBehindItNeverReportsIdle: the TUI's chain carries
// on after Esc. The cancelled turn's ending names its successor, so neither a
// client's status nor a host's ever passes through idle on the way.
func TestACancelledTurnWithARowBehindItNeverReportsIdle(t *testing.T) {
	r := newRig(t, Options{})
	first := r.s.script(held())
	r.submit("one")
	await(t, first.opened, "the first turn to open")
	r.submit("two")
	r.sync()
	if _, err := r.e.Cancel(context.Background(), Command{}, ""); err != nil {
		t.Fatal(err)
	}
	got := r.until(lastEnding)
	r.wantShapes(got,
		`started turn-1 submit "one"`,
		`queue queued "two"`,
		`done cancelled`,
		`queue sent "two"`,
		`ended turn-1 stop="cancelled" next="turn-2" pending=0`,
		`started turn-2 drain "two"`,
		`text "echo: two"`,
		`done end_turn`,
		`ended turn-2 stop="end_turn" next="" pending=0`,
	)
}

// TestChainPolicy is §3.4's table, row by row, for both policies.
func TestChainPolicy(t *testing.T) {
	tui, cli := ChainPolicy{}, ChainPolicy{StopOnNonEndTurn: true, RetryForeignTurn: true}
	boom := errors.New("the turn failed")
	cases := []struct {
		name   string
		chain  ChainPolicy
		first  *script
		want   []string
		state  Activity
		queued int
		sent   []string
	}{
		{
			name: "end_turn drains, tui", chain: tui, first: held(),
			want: []string{
				`started turn-1 submit "one"`, `queue queued "two"`, `text "echo: one"`, `done end_turn`,
				`queue sent "two"`, `ended turn-1 stop="end_turn" next="turn-2" pending=0`,
				`started turn-2 drain "two"`, `text "echo: two"`, `done end_turn`,
				`ended turn-2 stop="end_turn" next="" pending=0`,
			},
			state: ActivityIdle, sent: []string{"one", "two"},
		},
		{
			name: "end_turn drains, cli", chain: cli, first: held(),
			want: []string{
				`started turn-1 submit "one"`, `queue queued "two"`, `text "echo: one"`, `done end_turn`,
				`queue sent "two"`, `ended turn-1 stop="end_turn" next="turn-2" pending=0`,
				`started turn-2 drain "two"`, `text "echo: two"`, `done end_turn`,
				`ended turn-2 stop="end_turn" next="" pending=0`,
			},
			state: ActivityIdle, sent: []string{"one", "two"},
		},
		{
			name: "another clean stop drains, tui", chain: tui, first: stopping(held(), "max_turn_requests"),
			want: []string{
				`started turn-1 submit "one"`, `queue queued "two"`, `text "echo: one"`, `done max_turn_requests`,
				`queue sent "two"`, `ended turn-1 stop="max_turn_requests" next="turn-2" pending=0`,
				`started turn-2 drain "two"`, `text "echo: two"`, `done end_turn`,
				`ended turn-2 stop="end_turn" next="" pending=0`,
			},
			state: ActivityIdle, sent: []string{"one", "two"},
		},
		{
			name: "another clean stop ends the chain, cli", chain: cli, first: stopping(held(), "max_turn_requests"),
			want: []string{
				`started turn-1 submit "one"`, `queue queued "two"`, `text "echo: one"`, `done max_turn_requests`,
				`queue removed "two"`, `ended turn-1 stop="max_turn_requests" next="" pending=0`,
			},
			state: ActivityIdle, sent: []string{"one"},
		},
		{
			name: "an error clears the queue, tui", chain: tui, first: failing(held(), boom),
			want: []string{
				`started turn-1 submit "one"`, `queue queued "two"`, `error`,
				`queue removed "two"`, `ended turn-1 stop="" next="" pending=0 class=other`,
			},
			state: ActivityError, sent: []string{"one"},
		},
		{
			name: "an error clears the queue, cli", chain: cli, first: failing(held(), boom),
			want: []string{
				`started turn-1 submit "one"`, `queue queued "two"`, `error`,
				`queue removed "two"`, `ended turn-1 stop="" next="" pending=0 class=other`,
			},
			state: ActivityError, sent: []string{"one"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, Options{Chain: tc.chain})
			r.s.script(tc.first)
			r.submit("one")
			await(t, tc.first.opened, "the first turn to open")
			r.submit("two")
			r.sync()
			tc.first.release()
			got := r.until(lastEnding)
			r.wantShapes(got, tc.want...)
			r.wantPrompts(tc.sent...)
			if st := r.e.State(); st.Activity != tc.state || len(st.Queue) != tc.queued {
				t.Fatalf("state: activity %s queue %d, want %s and %d", st.Activity, len(st.Queue), tc.state, tc.queued)
			}
		})
	}
}

func stopping(sc *script, stop string) *script { sc.stop = stop; return sc }
func failing(sc *script, err error) *script    { sc.fail = err; return sc }

// TestAnArmedRowSurvivesTheChainPolicysClear is the chain policy against a
// send-now that named a queued row. The policy is about the follow-ups queued
// *behind* a turn; the row an armed send is about is not one of those — it is
// what the client cancelled the turn for — so the send takes its row before the
// clear and the ordinary follow-up is cleared without it. A policy that took the
// row with the rest would leave the send disarmed for a reason the client never
// caused, with the text gone: the one thing a send-now promises is that nothing
// is consumed until it fires.
//
// Under the TUI's policy nothing is cleared at all, and the follow-up simply
// waits its turn behind the send. Both are driven by a turn that stops
// `cancelled`, which is what a send-now's own cancel makes of it.
func TestAnArmedRowSurvivesTheChainPolicysClear(t *testing.T) {
	for _, tc := range []struct {
		name   string
		chain  ChainPolicy
		want   []string
		sent   []string
		queued int
	}{
		{
			name:  "the TUI keeps the queue",
			chain: ChainPolicy{},
			want: []string{
				`queue sent "the row"`,
				`ended turn-1 stop="cancelled" next="turn-2" pending=1`,
				`started turn-2 send_now "the row"`,
			},
			sent: []string{"one", "the row", "a follow-up"},
		},
		{
			name:  "the CLI ends the chain",
			chain: ChainPolicy{StopOnNonEndTurn: true},
			want: []string{
				// The row the send is about goes out; the follow-up behind it is
				// cleared, in the same batch and after it.
				`queue sent "the row"`,
				`queue removed "a follow-up"`,
				`ended turn-1 stop="cancelled" next="turn-2" pending=0`,
				`started turn-2 send_now "the row"`,
			},
			sent: []string{"one", "the row"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, Options{Chain: tc.chain})
			// A turn that stops cancelled when the test lets it, and takes its
			// cancel without acting on it, so the order of the record is the
			// test's: the settlement's input is a clean stop whose reason is
			// cancelled, exactly as a send-now's own cancel leaves it.
			turn := r.s.script(stopping(held(), stopCancelled))
			r.submit("one")
			await(t, turn.opened, "the turn to open")
			row := r.queue("the row")
			r.queue("a follow-up")
			r.ignoreCancels()
			r.sendNow(row.Text, row.ID)
			r.until(armedNow)
			turn.release()
			got := r.until(started("turn-2"))
			r.wantShapes(got[len(got)-len(tc.want):], tc.want...)
			r.until(lastEnding)
			r.wantPrompts(tc.sent...)
			if st := r.e.State(); len(st.Queue) != tc.queued || st.SendNow != nil {
				t.Fatalf("state after the chain settled: %+v", st)
			}
		})
	}
}

// The three tests below are what was TestARefusedTurnEndsSyntheticallyAndKeeps-
// TheQueue, split by plan 026 PR 3 (C8b, SF-21) because its first case changed
// meaning. A refusal emits nothing, so in every case the engine authors the
// ending, and it is the same shape in every case — synthetic, the refusal's class
// and text, no successor. What differs is what the refusal reached: a turn the
// agent's own turn refused whose text came from a queued row reached nothing and
// still has a row to go back to; anything else is the TUI's error state, with the
// queue left alone because a refusal reached nothing — only a turn that ran and
// failed clears it.

// restoreRig is a rig under the TUI's policy whose driver makes a restored row's
// recheck only when the test sends a tick, and which reports every continuation
// that comes back and every pass the driver makes (ticked says a tick caused it).
func restoreRig(t *testing.T) (*rig, chan<- time.Time, <-chan string, <-chan bool) {
	t.Helper()
	ticks := make(chan time.Time)
	returned := make(chan string, 16)
	// Never full in these tests, and never waited on by the driver if it were: a
	// pass the test does not wait for must not hold the driver.
	passes := make(chan bool, 64)
	h := &hooks{
		retryTick:    ticks,
		turnReturned: func(id string) { returned <- id },
		drivePassed: func(ticked bool) {
			select {
			case passes <- ticked:
			default:
			}
		},
	}
	return newRigHooked(t, Options{}, agent.EventLogOptions{NoPrimary: true}, h), ticks, returned, passes
}

// awaitPass waits for the driver to finish a pass; ticked says which kind: one a
// tick caused, or one a kick did. Passes of the other kind are skipped.
func awaitPass(t *testing.T, passes <-chan bool, ticked bool) {
	t.Helper()
	for {
		select {
		case got := <-passes:
			if got == ticked {
				return
			}
		case <-time.After(watchdog):
			t.Fatalf("the driver made no pass (ticked=%v) in %s", ticked, watchdog)
		}
	}
}

// sameRow is a queued row's whole identity: id, text, version and queued time.
func sameRow(a, b agent.QueuedPrompt) bool {
	return a.ID == b.ID && a.Text == b.Text && a.Version == b.Version && a.QueuedAt.Equal(b.QueuedAt)
}

// wantRestored checks what a restoring settlement leaves: the engine idle and
// not failed, nothing current, and the queue — as State and as the engine's own
// transcript model (what Attach snapshots) both hold it — exactly want, the
// restored row first as the row it was.
func (r *rig) wantRestored(turn string, want ...agent.QueuedPrompt) {
	r.t.Helper()
	st := r.e.State()
	if st.Activity != ActivityIdle || st.Err != "" || st.Turn != "" || st.Cancelled || st.SendNow != nil {
		r.t.Fatalf("state after a restoring settlement: %+v", st)
	}
	for _, q := range []struct {
		name string
		rows []agent.QueuedPrompt
	}{{"State", st.Queue}, {"the model", r.e.model.State().Queue}} {
		ok := len(q.rows) == len(want)
		for i := 0; ok && i < len(want); i++ {
			ok = sameRow(q.rows[i], want[i])
		}
		if !ok {
			r.t.Fatalf("%s's queue after the restore is %+v, want %+v", q.name, q.rows, want)
		}
	}
	if !errors.Is(r.e.TurnErr(turn), agent.ErrForeignTurn) {
		r.t.Fatalf("the refused turn's own error: %v", r.e.TurnErr(turn))
	}
}

// wantRestoreEvents checks the restoring settlement's two events in got: the
// row's `queued` at position 0, as the row it was, then the ending in today's
// refusal shape, both carrying the refused turn's own cause.
func wantRestoreEvents(t *testing.T, got []agent.Event, turn string, row agent.QueuedPrompt, pending int, cause string) {
	t.Helper()
	for i, ev := range got {
		if !ended(turn)(ev) {
			continue
		}
		if i == 0 {
			break
		}
		q := got[i-1]
		if q.Type != agent.EventQueue || q.QueueChange != agent.QueueQueued || q.QueuePos != 0 || !sameRow(*q.Queue, row) || q.Cause != cause {
			t.Fatalf("the event before %s's ending is %+v (cause %q), want the row %+v queued at 0 with cause %q", turn, q, q.Cause, row, cause)
		}
		end := ev.Turn
		if !end.Synthetic || end.ErrClass != agent.EventErrForeignTurn || end.Err != agent.ErrForeignTurn.Error() ||
			end.StopReason != "" || end.Next != "" || end.Pending != pending || ev.Cause != cause {
			t.Fatalf("the restoring ending: %+v (cause %q)", end, ev.Cause)
		}
		return
	}
	t.Fatalf("no restoring settlement for %s in %s", turn, describe(got))
}

// TestRefusedDrainedRowRunsAfterWake is SF-21's fix (plan 026 §3.11, §7 A14,
// X32). The TUI's drain took the head of the queue and the session refused it
// because the agent was running a turn of its own. Nothing reached the agent, so
// the row is still the user's message: it goes back to the head as the row it
// was — its id, its version (an edit made it 1 here), its queued time — in the
// same batch as the ending and ahead of it; the engine is idle, not failed; and
// the row runs once the agent's turn is over, with the rest of the queue behind
// it. The ending itself is the refusal's, unchanged on the wire (SF-47).
//
// The settlement that puts the row back takes nothing — the session's flag can
// lag its client's refusal, and a claim there and then would be refused again as
// fast as the two could go — and asks for a paced recheck of its own (astra
// r14's blocker). So the row runs wherever the agent's turn ends relative to the
// settlement, and those are the three schedules:
//
//   - before the settlement: the agent's turn starts and ends while the refused
//     claim is out, and the kick its end gave the driver is spent on a pass that
//     finds the continuation still out. Nothing else is coming; the
//     restoration's own tick drains the row.
//   - during the recheck's delay: the row is back and its tick not yet sent, and
//     the end's kick drains it. The tick is never sent at all.
//   - after the recheck: the tick's pass finds the agent's turn still running,
//     drains nothing and arms nothing further; the end's kick drains it.
func TestRefusedDrainedRowRunsAfterWake(t *testing.T) {
	const (
		before = "before the settlement"
		during = "during the recheck's delay"
		after  = "after the recheck"
	)
	for _, schedule := range []string{before, during, after} {
		t.Run(schedule, func(t *testing.T) {
			r, ticks, returned, passes := restoreRig(t)
			first := r.s.script(held())
			r.submit("one")
			await(t, first.opened, "the first turn to open")
			two := r.submit("two").Queued
			r.submit("three")
			if err := r.e.EditQueued(Command{}, two.ID, "two edited", nil); err != nil {
				t.Fatal(err)
			}
			rows := r.e.State().Queue
			if len(rows) != 2 || rows[0].ID != two.ID || rows[0].Version != 1 {
				t.Fatalf("the queue before the drain: %+v", rows)
			}
			refusal := r.s.script(refusedAtAGate(agent.ErrForeignTurn))
			r.sync()
			first.release()
			await(t, refusal.refusing, "the drained claim to reach its refusal")
			awaitTurn(t, returned, "turn-1")

			if schedule == before {
				r.s.setForeign(true)
				r.s.setForeign(false)
				awaitPass(t, passes, false)
			} else {
				// The session catches up with the client that refused the claim.
				r.s.setForeignSilently(true)
			}
			close(refusal.gate)
			awaitTurn(t, returned, "turn-2")
			r.sync()
			r.wantRestored("turn-2", rows...)
			r.wantPrompts("one", "two edited")

			switch schedule {
			case before:
				// The one wake-up the agent's turn gave is gone: only the tick
				// can drain the row, and nothing drains it before the tick.
				tick(t, ticks, returned)
			case during:
				r.s.setForeign(false)
			case after:
				tick(t, ticks, returned)
				awaitPass(t, passes, true)
				r.e.mu.Lock()
				again := r.e.recheck
				r.e.mu.Unlock()
				if again {
					t.Fatal("a recheck that found the agent's turn running armed another")
				}
				r.wantPrompts("one", "two edited")
				r.wantRestored("turn-2", rows...)
				r.s.setForeign(false)
			}
			r.until(lastEnding)
			wantRestoreEvents(t, r.seen, "turn-2", rows[0], 2, "")
			var foreign []string
			if schedule == before {
				foreign = []string{`foreign running=true`, `foreign running=false`}
			}
			record := append([]string{
				`started turn-1 submit "one"`,
				`queue queued "two"`,
				`queue queued "three"`,
				`queue edited "two edited"`,
				`text "echo: one"`,
				`done end_turn`,
				`queue sent "two edited"`,
				`ended turn-1 stop="end_turn" next="turn-2" pending=1`,
				`started turn-2 drain "two edited"`,
			}, foreign...)
			record = append(record,
				`queue queued "two edited"`,
				`ended turn-2 stop="" next="" pending=2 synthetic class=foreign_turn`,
			)
			if schedule != before {
				record = append(record, `foreign running=false`)
			}
			record = append(record,
				`queue sent "two edited"`,
				`started turn-3 drain "two edited"`,
				`text "echo: two edited"`,
				`done end_turn`,
				`queue sent "three"`,
				`ended turn-3 stop="end_turn" next="turn-4" pending=0`,
				`started turn-4 drain "three"`,
				`text "echo: three"`,
				`done end_turn`,
				`ended turn-4 stop="end_turn" next="" pending=0`,
			)
			r.wantShapes(r.seen, record...)
			r.wantPrompts("one", "two edited", "two edited", "three")
		})
	}
}

// TestRefusedRowSourcedTurnRestoresItsRow is SF-21 for the two other ways a
// turn's text comes from a queued row: a Submit that names the row, and a
// send-now armed from one. Each claim reaches Begin while the session's flag
// still reads clear — the lag between a client that knows of the agent's turn
// and a session that does not yet — and is refused; the row goes back to the head
// as the row it was, its `queued` and the ending carrying the command that
// started the turn, and it runs once the agent's turn is over.
func TestRefusedRowSourcedTurnRestoresItsRow(t *testing.T) {
	t.Run("a submit from a row", func(t *testing.T) {
		r, _, returned, _ := restoreRig(t)
		row := r.queue("the row")
		behind := r.queue("behind it")
		refusal := r.s.script(refusedAtAGate(agent.ErrForeignTurn))
		c := Command{Client: r.e.NewClientID(), ID: "1"}
		res, err := r.e.Submit(c, "the row", SubmitQueue, row.ID)
		if err != nil || res.Turn != "turn-1" {
			t.Fatalf("the submit from a row answered %+v, %v", res, err)
		}
		await(t, refusal.refusing, "the claim to reach its refusal")
		r.s.setForeignSilently(true)
		close(refusal.gate)
		awaitTurn(t, returned, "turn-1")
		r.sync()
		r.wantRestored("turn-1", row, behind)
		r.s.setForeign(false)
		r.until(lastEnding)
		wantRestoreEvents(t, r.seen, "turn-1", row, 2, c.Cause())
		r.wantShapes(r.seen,
			`queue queued "the row"`,
			`queue queued "behind it"`,
			`queue sent "the row"`,
			`started turn-1 submit "the row"`,
			`queue queued "the row"`,
			`ended turn-1 stop="" next="" pending=2 synthetic class=foreign_turn`,
			`foreign running=false`,
			`queue sent "the row"`,
			`started turn-2 drain "the row"`,
			`text "echo: the row"`,
			`done end_turn`,
			`queue sent "behind it"`,
			`ended turn-2 stop="end_turn" next="turn-3" pending=0`,
			`started turn-3 drain "behind it"`,
			`text "echo: behind it"`,
			`done end_turn`,
			`ended turn-3 stop="end_turn" next="" pending=0`,
		)
		r.wantPrompts("the row", "the row", "behind it")
	})
	t.Run("an armed send from a row", func(t *testing.T) {
		r, _, returned, _ := restoreRig(t)
		// A turn that stops cancelled when the test lets it, and takes its cancel
		// without acting on it: the send-now's cancel leaves the settlement a
		// clean stop, cancelled, which fires the send (as in
		// TestAnArmedRowSurvivesTheChainPolicysClear).
		turn := r.s.script(stopping(held(), stopCancelled))
		r.submit("one")
		await(t, turn.opened, "the turn to open")
		row := r.queue("the row")
		behind := r.queue("a follow-up")
		r.ignoreCancels()
		refusal := r.s.script(refusedAtAGate(agent.ErrForeignTurn))
		c := Command{Client: r.e.NewClientID(), ID: "1"}
		if res, err := r.e.Submit(c, "the row", SubmitSendNow, row.ID); err != nil || !res.Armed {
			t.Fatalf("the send-now answered %+v, %v", res, err)
		}
		r.until(armedNow)
		turn.release()
		await(t, refusal.refusing, "the send's claim to reach its refusal")
		awaitTurn(t, returned, "turn-1")
		r.s.setForeignSilently(true)
		close(refusal.gate)
		awaitTurn(t, returned, "turn-2")
		r.sync()
		r.wantRestored("turn-2", row, behind)
		r.s.setForeign(false)
		got := r.until(lastEnding)
		wantRestoreEvents(t, r.seen, "turn-2", row, 2, c.Cause())
		r.wantShapes(got,
			`text "echo: one"`,
			`done cancelled`,
			`queue sent "the row"`,
			`ended turn-1 stop="cancelled" next="turn-2" pending=1`,
			`started turn-2 send_now "the row"`,
			`queue queued "the row"`,
			`ended turn-2 stop="" next="" pending=2 synthetic class=foreign_turn`,
			`foreign running=false`,
			`queue sent "the row"`,
			`started turn-3 drain "the row"`,
			`text "echo: the row"`,
			`done end_turn`,
			`queue sent "a follow-up"`,
			`ended turn-3 stop="end_turn" next="turn-4" pending=0`,
			`started turn-4 drain "a follow-up"`,
			`text "echo: a follow-up"`,
			`done end_turn`,
			`ended turn-4 stop="end_turn" next="" pending=0`,
		)
		r.wantPrompts("one", "the row", "the row", "a follow-up")
	})
}

// TestACancelledRowSourcedRefusalIsNotRestored is the session-control R1: a
// cancel validated against a row-sourced turn wins over the restore. The cancel
// is taken while the turn's claim is out — the session withdraws it, but the
// agent's own turn refuses it first — so the continuation comes back refused,
// and the turn ends exactly as a cancel before the turn opened does: synthetic,
// stopped `cancelled`, the engine idle. Its row is not put back and never runs
// again; the queue behind it follows the ordinary rule after a cancel (the TUI's
// carries on); and a send armed against it — whose own cancel is the one
// validated — fires, as a send-now does.
func TestACancelledRowSourcedRefusalIsNotRestored(t *testing.T) {
	// drainedAndRefusing is a drained row's turn (turn-2, "two") held at its
	// refusal, with "three" queued behind it.
	drainedAndRefusing := func(t *testing.T) (*rig, *script, <-chan string) {
		t.Helper()
		r, _, returned, _ := restoreRig(t)
		first := r.s.script(held())
		r.submit("one")
		await(t, first.opened, "the first turn to open")
		r.submit("two")
		r.submit("three")
		refusal := r.s.script(refusedAtAGate(agent.ErrForeignTurn))
		r.sync()
		first.release()
		await(t, refusal.refusing, "the drained claim to reach its refusal")
		awaitTurn(t, returned, "turn-1")
		return r, refusal, returned
	}
	wantCancelled := func(t *testing.T, got []agent.Event, turn string) {
		t.Helper()
		for _, ev := range got {
			if ended(turn)(ev) {
				if end := ev.Turn; !end.Synthetic || end.StopReason != stopCancelled || end.Err != "" || end.ErrClass != "" {
					t.Fatalf("the cancelled refusal's ending: %+v", end)
				}
				return
			}
		}
		t.Fatalf("no ending for %s in %s", turn, describe(got))
	}

	t.Run("a drained row", func(t *testing.T) {
		r, refusal, _ := drainedAndRefusing(t)
		if _, err := r.e.Cancel(context.Background(), Command{}, "turn-2"); err != nil {
			t.Fatalf("cancel: %v", err)
		}
		close(refusal.gate)
		got := r.until(ended("turn-2"))
		wantCancelled(t, got, "turn-2")
		r.until(lastEnding)
		r.wantShapes(r.seen,
			`started turn-1 submit "one"`,
			`queue queued "two"`,
			`queue queued "three"`,
			`text "echo: one"`,
			`done end_turn`,
			`queue sent "two"`,
			`ended turn-1 stop="end_turn" next="turn-2" pending=1`,
			`started turn-2 drain "two"`,
			`queue sent "three"`,
			`ended turn-2 stop="cancelled" next="turn-3" pending=0 synthetic`,
			`started turn-3 drain "three"`,
			`text "echo: three"`,
			`done end_turn`,
			`ended turn-3 stop="end_turn" next="" pending=0`,
		)
		r.wantPrompts("one", "two", "three")
	})

	t.Run("a submit from a row", func(t *testing.T) {
		r, _, returned, _ := restoreRig(t)
		row := r.queue("the row")
		r.queue("behind it")
		refusal := r.s.script(refusedAtAGate(agent.ErrForeignTurn))
		c := Command{Client: r.e.NewClientID(), ID: "1"}
		if res, err := r.e.Submit(c, "the row", SubmitQueue, row.ID); err != nil || res.Turn != "turn-1" {
			t.Fatalf("the submit from a row answered %+v, %v", res, err)
		}
		await(t, refusal.refusing, "the claim to reach its refusal")
		if _, err := r.e.Cancel(context.Background(), Command{}, "turn-1"); err != nil {
			t.Fatalf("cancel: %v", err)
		}
		close(refusal.gate)
		awaitTurn(t, returned, "turn-1")
		wantCancelled(t, r.until(ended("turn-1")), "turn-1")
		r.until(lastEnding)
		r.wantShapes(r.seen,
			`queue queued "the row"`,
			`queue queued "behind it"`,
			`queue sent "the row"`,
			`started turn-1 submit "the row"`,
			`queue sent "behind it"`,
			`ended turn-1 stop="cancelled" next="turn-2" pending=0 synthetic`,
			`started turn-2 drain "behind it"`,
			`text "echo: behind it"`,
			`done end_turn`,
			`ended turn-2 stop="end_turn" next="" pending=0`,
		)
		r.wantPrompts("the row", "behind it")
	})

	t.Run("a send armed against it fires", func(t *testing.T) {
		r, refusal, _ := drainedAndRefusing(t)
		r.sendNow("instead", "")
		r.until(armedNow)
		close(refusal.gate)
		got := r.until(ended("turn-2"))
		wantCancelled(t, got, "turn-2")
		got = append(got, r.until(lastEnding)...)
		want := []string{
			`ended turn-2 stop="cancelled" next="turn-3" pending=1 synthetic`,
			`started turn-3 send_now "instead"`,
			`text "echo: instead"`,
			`done end_turn`,
			`queue sent "three"`,
			`ended turn-3 stop="end_turn" next="turn-4" pending=0`,
			`started turn-4 drain "three"`,
			`text "echo: three"`,
			`done end_turn`,
			`ended turn-4 stop="end_turn" next="" pending=0`,
		}
		r.wantShapes(got[len(got)-len(want):], want...)
		r.wantPrompts("one", "two", "instead", "three")
	})
}

// TestARestorationBesideAStandingSend is astra r16's retained-arm gap, as far
// as R1 leaves it reachable. A send armed against the refused turn itself cannot
// be standing at a restore: arming validates a cancel against the turn, and a
// cancelled turn is not restored (TestACancelledRowSourcedRefusalIsNotRestored's
// last case, where the arm fires first). A send armed against ANOTHER turn can
// be: it was left standing when the turn it replaced settled behind the agent's
// own turn, and the session's flag then cleared ahead of its client's refusal
// while a submit took a row. The restore puts the row back and takes that send
// with it, as any settlement does for a send armed against a turn that is not
// the one ending.
func TestARestorationBesideAStandingSend(t *testing.T) {
	r, _, returned, _ := restoreRig(t)
	// The send-now's cancel leaves turn-1 a clean stop, cancelled, and is not
	// acted on: the order is the test's.
	turn := r.s.script(stopping(held(), stopCancelled))
	r.submit("one")
	await(t, turn.opened, "the turn to open")
	row := r.queue("the row")
	r.ignoreCancels()
	r.sendNow("instead", "")
	r.until(armedNow)
	// The agent's turn starts: the settlement cannot fire the send.
	r.s.setForeignSilently(true)
	turn.release()
	awaitTurn(t, returned, "turn-1")
	if st := r.e.State(); st.SendNow == nil || st.SendNow.Turn != "turn-1" {
		t.Fatalf("the send standing behind the agent's turn: %+v", st.SendNow)
	}
	// The session's flag clears ahead of its client's refusal.
	r.s.setForeignSilently(false)
	r.s.script(&script{refuse: agent.ErrForeignTurn})
	if res, err := r.e.Submit(Command{}, "the row", SubmitQueue, row.ID); err != nil || res.Turn != "turn-2" {
		t.Fatalf("the submit from a row answered %+v, %v", res, err)
	}
	awaitTurn(t, returned, "turn-2")
	r.sync()
	r.wantRestored("turn-2", row)
	got := r.until(func(ev agent.Event) bool { return disarmed(ev) })
	r.wantShapes(got[len(got)-3:],
		`queue queued "the row"`,
		`ended turn-2 stop="" next="" pending=1 synthetic class=foreign_turn`,
		`disarmed `+agent.SendNowOtherTurn,
	)
	r.wantPrompts("one", "the row")
}

// TestARestorationRacingStopOrClose is astra r16's shutdown gap: a restoration
// against Stop and against Close, each before the refusal settles and after the
// row is back with its recheck owed. Nothing drains after either, and nothing
// leaks — the engine's goroutines are joined by Close, which returns.
//
//   - Stop before the settlement is a cancel validated against the turn: it ends
//     cancelled (R1) and nothing is put back — the queue was cleared by the stop.
//   - Stop after the restoration clears the restored row with the rest, and the
//     recheck's tick drains nothing from a stopped engine.
//   - Close before the settlement ends the turn `closing` and the refusal that
//     comes back afterwards settles nothing: no row is put back.
//   - Close after the restoration leaves the row where it is, and the recheck
//     never runs: the driver is gone.
func TestARestorationRacingStopOrClose(t *testing.T) {
	// refusing is a drained row's turn (turn-2, "two") held at its refusal, with
	// "three" behind it, on a rig with a primary — the one reader that survives a
	// close (rig.committed).
	type restoring struct {
		r        *rig
		refusal  *script
		ticks    chan<- time.Time
		returned <-chan string
		passes   <-chan bool
	}
	refusing := func(t *testing.T) restoring {
		t.Helper()
		ticks := make(chan time.Time)
		returned := make(chan string, 16)
		passes := make(chan bool, 64)
		r := newRigHooked(t, Options{}, agent.EventLogOptions{}, &hooks{
			retryTick:    ticks,
			turnReturned: func(id string) { returned <- id },
			drivePassed: func(ticked bool) {
				select {
				case passes <- ticked:
				default:
				}
			},
		})
		first := r.s.script(held())
		r.submit("one")
		await(t, first.opened, "the first turn to open")
		r.submit("two")
		r.submit("three")
		refusal := r.s.script(refusedAtAGate(agent.ErrForeignTurn))
		first.release()
		await(t, refusal.refusing, "the drained claim to reach its refusal")
		awaitTurn(t, returned, "turn-1")
		return restoring{r: r, refusal: refusal, ticks: ticks, returned: returned, passes: passes}
	}
	// restored lets the refusal settle, and the row is back.
	restored := func(t *testing.T, x restoring) {
		t.Helper()
		x.r.s.setForeignSilently(true)
		close(x.refusal.gate)
		awaitTurn(t, x.returned, "turn-2")
		if rows := x.r.rows(); len(rows) != 2 || rows[0] != "two" {
			t.Fatalf("the restored queue: %q", rows)
		}
	}
	closes := func(t *testing.T, e *Engine) {
		t.Helper()
		done := make(chan error, 1)
		go func() { done <- e.Close() }()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("close: %v", err)
			}
		case <-time.After(watchdog):
			t.Fatal("Close did not return: something the engine owns is still running")
		}
	}
	endings := func(t *testing.T, r *rig) []string {
		t.Helper()
		return turnRecord(r.committed())
	}

	t.Run("Stop before the settlement", func(t *testing.T) {
		x := refusing(t)
		if err := x.r.e.Stop(context.Background(), Command{}); err != nil {
			t.Fatalf("stop: %v", err)
		}
		close(x.refusal.gate)
		awaitTurn(t, x.returned, "turn-2")
		got := endings(t, x.r)
		if last := got[len(got)-1]; last != `ended turn-2 stop="cancelled" next="" pending=0 synthetic` {
			t.Fatalf("the stopped refusal ended %q\n%q", last, got)
		}
		if rows := x.r.rows(); len(rows) != 0 {
			t.Fatalf("a row after the stop: %q", rows)
		}
		closes(t, x.r.e)
		x.r.wantPrompts("one", "two")
	})

	t.Run("Stop after the restoration", func(t *testing.T) {
		x := refusing(t)
		restored(t, x)
		if err := x.r.e.Stop(context.Background(), Command{}); err != nil {
			t.Fatalf("stop: %v", err)
		}
		if rows := x.r.rows(); len(rows) != 0 {
			t.Fatalf("the stop left rows: %q", rows)
		}
		// The recheck the restoration owed is made, and drains nothing.
		x.r.s.setForeignSilently(false)
		tick(t, x.ticks, x.returned)
		awaitPass(t, x.passes, true)
		closes(t, x.r.e)
		x.r.wantPrompts("one", "two")
	})

	t.Run("Close before the settlement", func(t *testing.T) {
		x := refusing(t)
		// Close ends the turn and closes the session, which lets the refusal come
		// back; it settles nothing.
		closes(t, x.r.e)
		awaitTurn(t, x.returned, "turn-2")
		got := endings(t, x.r)
		if last := got[len(got)-1]; last != `ended turn-2 stop="closing" next="" pending=1 synthetic` {
			t.Fatalf("the closed turn ended %q\n%q", last, got)
		}
		if rows := x.r.rows(); len(rows) != 1 || rows[0] != "three" {
			t.Fatalf("the queue after the close: %q", rows)
		}
		x.r.wantPrompts("one", "two")
	})

	t.Run("Close after the restoration", func(t *testing.T) {
		x := refusing(t)
		restored(t, x)
		x.r.s.setForeignSilently(false)
		closes(t, x.r.e)
		if rows := x.r.rows(); len(rows) != 2 || rows[0] != "two" {
			t.Fatalf("the queue after the close: %q", rows)
		}
		x.r.wantPrompts("one", "two")
	})
}

// TestADirectSubmitRefusedByAForeignTurnKeepsTheQueue is the TUI's row for a
// foreign-turn refusal of text a client held itself: there is no row to put
// back, so it is today's ending. The session emits nothing for it, so the engine
// authors the ending; the state is an error; and the queue is left alone,
// because a refusal reached nothing — only a turn that ran and failed clears it.
func TestADirectSubmitRefusedByAForeignTurnKeepsTheQueue(t *testing.T) {
	r := newRig(t, Options{})
	r.queue("three")
	r.s.script(&script{refuse: agent.ErrForeignTurn})
	res := r.submit("two")
	if res.Turn != "turn-1" {
		t.Fatalf("the submit answered %+v, want the turn it started", res)
	}
	got := r.until(ended("turn-1"))
	last := got[len(got)-1].Turn
	if !last.Synthetic || last.ErrClass != agent.EventErrForeignTurn || last.Err != agent.ErrForeignTurn.Error() || last.Next != "" || last.Pending != 1 {
		t.Fatalf("the refusal's ending: %+v", last)
	}
	st := r.e.State()
	if st.Activity != ActivityError || st.Err != agent.ErrForeignTurn.Error() || len(st.Queue) != 1 || st.Queue[0].Text != "three" {
		t.Fatalf("state: %+v", st)
	}
	// Nothing drains from an error; the next direct submit clears it and runs
	// ahead of the queue, which then carries on.
	r.submit("four")
	r.until(lastEnding)
	r.wantPrompts("two", "four", "three")
}

// TestAPromptInFlightRefusalEndsSyntheticallyAndKeepsTheQueue is the TUI's row
// for the other refusal, ErrPromptInFlight, of a drained row: the session emits
// nothing for it, so the engine authors the ending; the state is an error; and
// the queue is left alone, because a refusal reached nothing — only a turn that
// ran and failed clears it. The drained row is not put back: that is the
// foreign-turn refusal's settlement, and this one is a claim the session held
// for another prompt of craze's own.
func TestAPromptInFlightRefusalEndsSyntheticallyAndKeepsTheQueue(t *testing.T) {
	refusal := agent.ErrPromptInFlight
	r := newRig(t, Options{})
	first := r.s.script(held())
	r.submit("one")
	await(t, first.opened, "the first turn to open")
	r.submit("two")
	r.submit("three")
	r.s.script(&script{refuse: refusal})
	first.release()
	got := r.until(ended("turn-2"))
	want := agent.ClassifyEventErr(refusal)
	last := got[len(got)-1].Turn
	if !last.Synthetic || last.ErrClass != want || last.Err != refusal.Error() || last.Next != "" || last.Pending != 1 {
		t.Fatalf("the refusal's ending: %+v", last)
	}
	st := r.e.State()
	if st.Activity != ActivityError || st.Err != refusal.Error() || len(st.Queue) != 1 || st.Queue[0].Text != "three" {
		t.Fatalf("state: %+v", st)
	}
	// Nothing drains from an error; the next direct submit clears it and runs
	// ahead of the queue, which then carries on.
	r.submit("four")
	r.until(lastEnding)
	r.wantPrompts("one", "two", "four", "three")
}

// retryRig is a rig whose driver takes a refused claim again only when the test
// sends a tick, and which reports every continuation that comes back.
func retryRig(t *testing.T, chain ChainPolicy) (*rig, chan<- time.Time, <-chan string) {
	t.Helper()
	ticks := make(chan time.Time)
	returned := make(chan string, 16)
	h := &hooks{retryTick: ticks, turnReturned: func(id string) { returned <- id }}
	return newRigHooked(t, Options{Chain: chain}, agent.EventLogOptions{NoPrimary: true}, h), ticks, returned
}

// tick sends one tick, and fails if a continuation comes back first: a claim
// taken again with no tick is the spin the pacing exists to prevent. The send
// itself is a barrier: the driver offers to receive a tick only once a pass of
// its own has seen the turn waiting, so when the send returns, the refusal
// before it has been through the driver and was not acted on.
func tick(t *testing.T, ticks chan<- time.Time, returned <-chan string) {
	t.Helper()
	select {
	case ticks <- time.Time{}:
	case id := <-returned:
		t.Fatalf("%s was claimed again with no tick", id)
	case <-time.After(watchdog):
		t.Fatal("the driver never armed its tick for a refused claim")
	}
}

// TestARefusedClaimIsTakenAgainOnATickWithNoEvent is `craze prompt`'s row: the
// session's flag lags the client's, admission passes, the claim is refused, and
// the engine keeps the turn current and takes the claim again — no ending, no
// second started, and the row the turn came from is not lost. It is taken again
// on a tick and at no other time: the refusal here comes with the session's flag
// already clear, so nothing but time is left to wait for, and a wake-up that
// authorised the retry would claim as fast as the session could refuse.
func TestARefusedClaimIsTakenAgainOnATickWithNoEvent(t *testing.T) {
	r, ticks, returned := retryRig(t, ChainPolicy{RetryForeignTurn: true})
	r.s.script(&script{refuse: agent.ErrForeignTurn})
	r.s.script(&script{refuse: agent.ErrForeignTurn})
	r.submit("go")
	for refusal := 1; refusal <= 2; refusal++ {
		if id := <-returned; id != "turn-1" {
			t.Fatalf("%s came back, want turn-1's refusal", id)
		}
		if n := len(r.s.prompts()); n != refusal {
			t.Fatalf("after refusal %d the session had been handed %d claims", refusal, n)
		}
		tick(t, ticks, returned)
	}
	got := r.until(lastEnding)
	r.wantShapes(got,
		`started turn-1 submit "go"`,
		`text "echo: go"`,
		`done end_turn`,
		`ended turn-1 stop="end_turn" next="" pending=0`,
	)
	r.wantPrompts("go", "go", "go")
}

// TestARetriedTurnWaitsOutTheForeignTurn: while the session says the agent has
// the session the claim is not taken again, tick or no tick, and the event that
// ends the foreign turn is what lets it through.
func TestARetriedTurnWaitsOutTheForeignTurn(t *testing.T) {
	r, ticks, returned := retryRig(t, ChainPolicy{RetryForeignTurn: true})
	refusal := r.s.script(refusedAtAGate(agent.ErrForeignTurn))
	r.submit("go")
	await(t, refusal.refusing, "the claim to reach its refusal")
	// The session catches up with its client before the refusal is in.
	r.s.setForeign(true)
	close(refusal.gate)
	<-returned
	// Two ticks: the second can only be received once the pass the first one
	// caused is over, so by then the driver has looked, with the retry due, and
	// declined.
	tick(t, ticks, returned)
	tick(t, ticks, returned)
	r.wantPrompts("go")
	r.s.setForeign(false)
	r.until(lastEnding)
	r.wantPrompts("go", "go")
	for _, ev := range r.seen {
		if started("")(ev) && ev.Turn.ID != "turn-1" {
			t.Fatalf("a retried claim must not be a second turn: %s", describe(r.seen))
		}
	}
}

// TestStopEndsARetriedTurnAsTheRefusalItWas: the client owns the budget. When
// it gives up it calls Stop, and the turn that was waiting ends synthetically,
// with the refusal, as the chain's last event.
func TestStopEndsARetriedTurnAsTheRefusalItWas(t *testing.T) {
	r, _, returned := retryRig(t, ChainPolicy{RetryForeignTurn: true, StopOnNonEndTurn: true})
	refusal := r.s.script(refusedAtAGate(agent.ErrForeignTurn))
	r.submit("go")
	await(t, refusal.refusing, "the claim to reach its refusal")
	r.s.setForeign(true)
	close(refusal.gate)
	// The refusal is in and the turn is waiting: current, working, unclaimed.
	<-returned
	if st := r.e.State(); st.Turn != "turn-1" || st.Activity != ActivityWorking {
		t.Fatalf("a turn waiting out a foreign turn: %+v", st)
	}
	if err := r.e.Stop(context.Background(), Command{}); err != nil {
		t.Fatalf("stop: %v", err)
	}
	got := r.until(lastEnding)
	last := got[len(got)-1].Turn
	if last.ID != "turn-1" || !last.Synthetic || last.ErrClass != agent.EventErrForeignTurn {
		t.Fatalf("the ending: %+v", last)
	}
	r.wantPrompts("go")
	if _, err := r.e.Submit(Command{}, "after", SubmitQueue, ""); !errors.Is(err, ErrNotAccepting) {
		t.Fatalf("a stopped engine admitted a prompt: %v", err)
	}
}

// TestADirectSubmitClaimsThroughAForeignTurnUnderTheRetryingPolicy: the
// session's flag gates the drain and not a direct submit under a policy that
// waits a foreign-turn refusal out. `craze prompt` has always sent its prompt
// into a session the agent was holding and waited the refusal out; a queued row
// in its place would be a message that run never sends and a pair of lines its
// JSON never had.
//
// State.Waiting is what the client bounding that wait reads, and this is the
// whole of what it promises: it is true while the refused turn waits, it stays
// true for as long as the agent's own turn lasts — which is the schedule where a
// count would freeze and a client timing it would wait for ever — and it is gone
// with the turn it was about.
func TestADirectSubmitClaimsThroughAForeignTurnUnderTheRetryingPolicy(t *testing.T) {
	r, ticks, returned := retryRig(t, ChainPolicy{RetryForeignTurn: true, StopOnNonEndTurn: true})
	r.s.setForeign(true)
	r.s.script(&script{refuse: agent.ErrForeignTurn})
	res := r.submit("go")
	if res.Turn == "" || res.Queued != nil {
		t.Fatalf("a direct submit was queued behind the agent's own turn: %+v", res)
	}
	awaitTurn(t, returned, "turn-1")
	if st := r.e.State(); st.Turn != "turn-1" || st.Activity != ActivityWorking || !st.Waiting {
		t.Fatalf("a turn waiting out a foreign turn: %+v", st)
	}
	// The claim is not taken again while the session says the agent has it, tick
	// or no tick, and the wait is still reported as one: the tick has made the
	// retry due for the moment the flag clears, and nothing else has changed.
	tick(t, ticks, returned)
	if st := r.e.State(); !st.Waiting {
		t.Fatalf("a claim was taken again during the foreign turn: %+v", st)
	}
	r.s.setForeign(false)
	r.until(lastEnding)
	r.wantPrompts("go", "go")
	if st := r.e.State(); st.Waiting || st.Turn != "" {
		t.Fatalf("the wait outlived the turn it was about: %+v", st)
	}
}

// TestGiveUpEndsAWaitingTurnAsTheRefusalItWas: the client's half of the wait.
// The turn settles through the ordinary settlement — the synthetic refusal, the
// error state — and nothing else of the session's is touched: no cancel is
// written, and the queue behind it is KEPT, because a refusal reached nothing.
func TestGiveUpEndsAWaitingTurnAsTheRefusalItWas(t *testing.T) {
	r, _, returned := retryRig(t, ChainPolicy{RetryForeignTurn: true, StopOnNonEndTurn: true})
	r.s.setForeign(true)
	r.s.script(&script{refuse: agent.ErrForeignTurn})
	r.submit("go")
	r.queue("behind it")
	awaitTurn(t, returned, "turn-1")
	if st := r.e.State(); !st.Waiting {
		t.Fatalf("the turn is not waiting: %+v", st)
	}
	if err := r.e.GiveUp(Command{}, "turn-1"); err != nil {
		t.Fatalf("give up: %v", err)
	}
	// Not lastEnding: this ending has a row still pending, which is the point of
	// it — a refusal reached nothing, so the chain policy leaves the queue alone.
	got := r.until(ended("turn-1"))
	last := got[len(got)-1].Turn
	if last.ID != "turn-1" || !last.Synthetic || last.ErrClass != agent.EventErrForeignTurn {
		t.Fatalf("the ending: %+v", last)
	}
	if last.Next != "" || last.Pending != 1 {
		t.Fatalf("a refusal keeps its queue and starts nothing: %+v", last)
	}
	r.wantShapes(got,
		`foreign running=true`,
		`started turn-1 submit "go"`,
		`queue queued "behind it"`,
		`ended turn-1 stop="" next="" pending=1 synthetic class=foreign_turn`,
	)
	st := r.e.State()
	if st.Activity != ActivityError || st.Turn != "" || st.Waiting || len(st.Queue) != 1 {
		t.Fatalf("state after the give-up: %+v", st)
	}
	if !errors.Is(r.e.TurnErr("turn-1"), agent.ErrForeignTurn) {
		t.Fatalf("the turn's own error: %v", r.e.TurnErr("turn-1"))
	}
	// No cancel was written: the session was handed one claim and nothing else.
	r.wantPrompts("go")
	if n := r.s.cancelsWritten(); n != 0 {
		t.Fatalf("a give-up wrote %d cancels", n)
	}
}

// TestGiveUpIsRefusedOnceTheClaimGoesThrough: the race finding 3 of sol's r9
// review found. A client decides to give up from an observation, and by the time
// it calls, the foreign turn may have ended and the claim gone through — the
// prompt it was waiting for is running. The give-up is refused and changes
// nothing; the turn runs to its end.
func TestGiveUpIsRefusedOnceTheClaimGoesThrough(t *testing.T) {
	r, ticks, returned := retryRig(t, ChainPolicy{RetryForeignTurn: true, StopOnNonEndTurn: true})
	refusal := r.s.script(refusedAtAGate(agent.ErrForeignTurn))
	second := r.s.script(held())
	r.submit("go")
	await(t, refusal.refusing, "the claim to reach its refusal")
	close(refusal.gate)
	<-returned
	// The claim is taken again and this time it runs: the turn opens and holds
	// there, so a give-up decided a moment ago arrives against a running prompt.
	tick(t, ticks, returned)
	await(t, second.opened, "the re-claimed turn to open")
	if st := r.e.State(); st.Waiting {
		t.Fatalf("the re-claimed turn still reports a wait: %+v", st)
	}
	if err := r.e.GiveUp(Command{}, "turn-1"); !errors.Is(err, ErrNotAccepting) || Code(err) != "not_accepting" {
		t.Fatalf("a give-up against a running turn: %v", err)
	}
	if err := r.e.GiveUp(Command{}, "turn-9"); !errors.Is(err, ErrStaleTurn) {
		t.Fatalf("a give-up against a turn that is not current: %v", err)
	}
	second.release()
	got := r.until(lastEnding)
	if last := got[len(got)-1].Turn; last.StopReason != "end_turn" || last.Err != "" {
		t.Fatalf("the turn the give-up did not touch: %+v", last)
	}
	r.wantPrompts("go", "go")
}

// TestGiveUpUnderACancelHoldWaitsForIt: a turn does not settle while a cancel is
// on its way to the session, and a give-up is no exception — it is committed
// (the turn is not claimed again), and the ending follows when the hold is
// released, exactly as a stop's does.
func TestGiveUpUnderACancelHoldWaitsForIt(t *testing.T) {
	r, _, returned := retryRig(t, ChainPolicy{RetryForeignTurn: true, StopOnNonEndTurn: true})
	r.s.setForeign(true)
	r.s.script(&script{refuse: agent.ErrForeignTurn})
	r.submit("go")
	awaitTurn(t, returned, "turn-1")

	entered, release := r.s.holdNextCancel()
	done := make(chan error, 1)
	go func() {
		_, err := r.e.Cancel(context.Background(), Command{}, "turn-1")
		done <- err
	}()
	await(t, entered, "the cancel to reach the session")
	if err := r.e.GiveUp(Command{}, "turn-1"); err != nil {
		t.Fatalf("give up: %v", err)
	}
	r.sync()
	if st := r.e.State(); st.Turn != "turn-1" || st.Waiting {
		t.Fatalf("the turn settled under a cancel hold, or is still waiting: %+v", st)
	}
	release()
	if err := <-done; err != nil {
		t.Fatalf("cancel: %v", err)
	}
	got := r.until(lastEnding)
	if last := got[len(got)-1].Turn; last.ID != "turn-1" || last.ErrClass != agent.EventErrForeignTurn {
		t.Fatalf("the ending the released hold produced: %+v", last)
	}
	r.wantPrompts("go")
}

// TestAForeignTurnHoldsTheDrainAndItsEndReleasesIt: the ending of a turn with a
// row behind it says Next is empty and Pending is one, and the drain's started
// arrives when the agent's own turn is over. An empty queue waits for nothing.
func TestAForeignTurnHoldsTheDrainAndItsEndReleasesIt(t *testing.T) {
	r := newRig(t, Options{})
	first := r.s.script(held())
	r.submit("one")
	await(t, first.opened, "the first turn to open")
	r.submit("two")
	r.s.setForeign(true)
	first.release()
	got := r.until(ended("turn-1"))
	if last := got[len(got)-1].Turn; last.Next != "" || last.Pending != 1 {
		t.Fatalf("an ending behind a foreign turn: %+v", last)
	}
	r.sync()
	if st := r.e.State(); st.Activity != ActivityIdle || len(st.Queue) != 1 {
		t.Fatalf("state while the foreign turn runs: %+v", st)
	}
	r.wantPrompts("one")
	r.s.setForeign(false)
	got = r.until(lastEnding)
	r.wantShapes(got,
		`foreign running=false`,
		`queue sent "two"`,
		`started turn-2 drain "two"`,
		`text "echo: two"`,
		`done end_turn`,
		`ended turn-2 stop="end_turn" next="" pending=0`,
	)
}

// TestForeignTurnAEndsAsBBegins: the wake-up for A's end runs a pass that finds
// B already running and drains nothing; B's end is a wake-up of its own. The
// row is never lost and never sent into a foreign turn.
func TestForeignTurnAEndsAsBBegins(t *testing.T) {
	r := newRig(t, Options{})
	r.s.setForeign(true)
	res := r.submit("queued behind A")
	if res.Queued == nil {
		t.Fatalf("a submit during a foreign turn answered %+v, want a queued row", res)
	}
	// A ends and B begins before anyone can act on A's end: the flag is
	// already up again when the event for A's end is published.
	r.s.setForeignSilently(true)
	r.s.emit(agent.Event{Type: agent.EventForeignTurn, ForeignTurn: &agent.ForeignTurnInfo{Running: false}})
	r.s.setForeign(true)
	r.until(func(ev agent.Event) bool {
		return ev.Type == agent.EventForeignTurn && ev.ForeignTurn.Running
	})
	r.sync()
	r.wantPrompts()
	r.s.setForeign(false)
	r.until(lastEnding)
	r.wantPrompts("queued behind A")
}
