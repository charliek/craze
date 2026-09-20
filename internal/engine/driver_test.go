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

// TestARefusedTurnEndsSyntheticallyAndKeepsTheQueue is the TUI's row for a
// refusal: the session emits nothing for it, so the engine authors the ending;
// the state is an error; and the queue is left alone, because a refusal reached
// nothing — only a turn that ran and failed clears it.
func TestARefusedTurnEndsSyntheticallyAndKeepsTheQueue(t *testing.T) {
	for _, refusal := range []error{agent.ErrForeignTurn, agent.ErrPromptInFlight} {
		t.Run(refusal.Error(), func(t *testing.T) {
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
			// Nothing drains from an error; the next direct submit clears it
			// and runs ahead of the queue, which then carries on.
			r.submit("four")
			r.until(lastEnding)
			r.wantPrompts("one", "two", "four", "three")
		})
	}
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
// State.Retries is what the client bounding that wait reads, and this is the
// whole of what it promises: it counts every refusal, it stands still while the
// claim is not being taken again, and it is gone with the turn it was about.
func TestADirectSubmitClaimsThroughAForeignTurnUnderTheRetryingPolicy(t *testing.T) {
	r, ticks, returned := retryRig(t, ChainPolicy{RetryForeignTurn: true, StopOnNonEndTurn: true})
	r.s.setForeign(true)
	r.s.script(&script{refuse: agent.ErrForeignTurn})
	res := r.submit("go")
	if res.Turn == "" || res.Queued != nil {
		t.Fatalf("a direct submit was queued behind the agent's own turn: %+v", res)
	}
	awaitTurn(t, returned, "turn-1")
	if st := r.e.State(); st.Turn != "turn-1" || st.Activity != ActivityWorking || st.Retries != 1 {
		t.Fatalf("a turn waiting out a foreign turn: %+v", st)
	}
	// The claim is not taken again while the session says the agent has it, tick
	// or no tick, so the count stands still: the wait its client is bounding is
	// still on, and the tick has made the retry due for the moment it clears.
	tick(t, ticks, returned)
	if st := r.e.State(); st.Retries != 1 {
		t.Fatalf("a claim was taken again during the foreign turn: %+v", st)
	}
	r.s.setForeign(false)
	r.until(lastEnding)
	r.wantPrompts("go", "go")
	if st := r.e.State(); st.Retries != 0 {
		t.Fatalf("Retries outlived the turn it was about: %+v", st)
	}
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
