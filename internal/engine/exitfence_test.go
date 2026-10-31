package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/charliek/craze/internal/agent"
)

// The exit fence (exitfence.go): a prompt that is its session's last. Each
// schedule is forced with the fake's own barriers, and what never started is read
// from the prompts the session was handed.

// lastPrompt submits text as the session's last prompt.
func (r *rig) lastPrompt(text string) SubmitResult {
	r.t.Helper()
	res, err := r.e.SubmitExitAfterTurn(Command{}, text)
	if err != nil {
		r.t.Fatalf("the fenced prompt %q: %v", text, err)
	}
	return res
}

// wantFenced: err is the fence's refusal, a gate refusal never stored.
func wantFenced(t *testing.T, what string, err error) {
	t.Helper()
	if !errors.Is(err, ErrExitFenced) {
		t.Fatalf("%s under the fence: %v, want ErrExitFenced", what, err)
	}
	if Code(err) != "not_accepting" || Reason(err) != "exit_fenced" || !gateRefusal(err) {
		t.Fatalf("%s: code %q reason %q forgotten %v", what, Code(err), Reason(err), gateRefusal(err))
	}
}

// TestAFencedTurnRefusesEveryLaterPromptAndNothingStartsAfterIt: a prompt that
// starts at once raises the fence with its answer. From then on new text is
// refused, in each way a client has of sending it, while the turn runs and after
// it ends; the turn's ending claims nothing, and nothing later does.
func TestAFencedTurnRefusesEveryLaterPromptAndNothingStartsAfterIt(t *testing.T) {
	r := newRig(t, Options{})
	turn := r.s.script(held())
	if res := r.lastPrompt("the last"); res.Turn != "turn-1" {
		t.Fatalf("the fenced prompt answered %+v", res)
	}
	await(t, turn.opened, "the fenced turn to open")
	refused := func(when string) {
		t.Helper()
		_, err := r.e.Submit(Command{}, "another", SubmitQueue, "")
		wantFenced(t, when+": Submit", err)
		_, err = r.e.Submit(Command{}, "now", SubmitSendNow, "")
		wantFenced(t, when+": a send-now", err)
		_, err = r.e.Queue(Command{}, "later")
		wantFenced(t, when+": Queue", err)
		_, err = r.e.SubmitExitAfterTurn(Command{}, "a second last")
		wantFenced(t, when+": a second fenced prompt", err)
	}
	refused("the turn running")
	if !r.e.State().ExitFenced {
		t.Fatal("State does not say the fence stands")
	}
	turn.release()
	r.until(ended("turn-1"))
	refused("the turn over")
	r.sync()
	r.wantPrompts("the last")
	if st := r.e.State(); st.Turn != "" || st.Activity != ActivityIdle || len(st.Queue) != 0 || !st.ExitFenced {
		t.Fatalf("after the fenced turn: %+v", st)
	}
}

// TestARowAheadOfTheFenceRunsFirst: a prompt behind a running turn is a row, its
// fence raised; rows queued before it run in order, then it; a prompt after it is
// refused; and nothing runs past it.
func TestARowAheadOfTheFenceRunsFirst(t *testing.T) {
	r := newRig(t, Options{})
	first := r.s.script(held())
	r.submit("running")
	await(t, first.opened, "the running turn to open")
	ahead := r.submit("ahead")
	if ahead.Queued == nil {
		t.Fatalf("a row ahead: %+v", ahead)
	}
	last := r.lastPrompt("the last")
	if last.Queued == nil {
		t.Fatalf("the fenced prompt behind a turn answered %+v, want its row", last)
	}
	_, err := r.e.Submit(Command{}, "too late", SubmitQueue, "")
	wantFenced(t, "a prompt behind the fenced row", err)
	r.sync()
	first.release()
	got := r.until(ended("turn-3"))
	r.wantShapes(got,
		`started turn-1 submit "running"`,
		`queue queued "ahead"`,
		`queue queued "the last"`,
		`text "echo: running"`,
		`done end_turn`,
		`queue sent "ahead"`,
		`ended turn-1 stop="end_turn" next="turn-2" pending=1`,
		`started turn-2 drain "ahead"`,
		`text "echo: ahead"`,
		`done end_turn`,
		`queue sent "the last"`,
		`ended turn-2 stop="end_turn" next="turn-3" pending=0`,
		`started turn-3 drain "the last"`,
		`text "echo: the last"`,
		`done end_turn`,
		`ended turn-3 stop="end_turn" next="" pending=0`,
	)
	r.sync()
	r.wantPrompts("running", "ahead", "the last")
	_, err = r.e.Queue(Command{}, "after it all")
	wantFenced(t, "a row after the fenced turn", err)
}

// TestARowBehindTheFencedTurnNeverStarts: a row the Queue verb left on an idle
// engine, before the fence, is behind the prompt that starts at once. It is
// removed in the settlement, ahead of the ending, and never reaches the agent.
func TestARowBehindTheFencedTurnNeverStarts(t *testing.T) {
	r := newRig(t, Options{})
	row := r.queue("queued before the fence")
	turn := r.s.script(held())
	if res := r.lastPrompt("the last"); res.Turn == "" {
		t.Fatalf("the fenced prompt on an idle engine: %+v", res)
	}
	await(t, turn.opened, "the fenced turn to open")
	if _, err := r.e.Submit(Command{}, "", SubmitQueue, row.ID); !errors.Is(err, ErrExitFenced) {
		t.Fatalf("a row behind the fenced turn named by id: %v", err)
	}
	r.sync()
	turn.release()
	got := r.until(ended("turn-1"))
	r.wantShapes(got,
		`queue queued "queued before the fence"`,
		`started turn-1 submit "the last"`,
		`text "echo: the last"`,
		`done end_turn`,
		`queue removed "queued before the fence"`,
		`ended turn-1 stop="end_turn" next="" pending=0`,
	)
	r.sync()
	r.wantPrompts("the last")
	r.wantRows()
}

// TestEveryEndingEndsTheFencedTurn: failed and cancelled end it as a clean stop
// does, and nothing queued behind them starts.
func TestEveryEndingEndsTheFencedTurn(t *testing.T) {
	boom := errors.New("boom")
	for _, tc := range []struct {
		name string
		sc   func() *script
		wait func(*testing.T, *rig, *script)
	}{
		{"failed", func() *script { return &script{fail: boom} }, func(*testing.T, *rig, *script) {}},
		{"cancelled", held, func(t *testing.T, r *rig, sc *script) {
			await(t, sc.opened, "the fenced turn to open")
			if _, err := r.e.Cancel(context.Background(), Command{}, "turn-1"); err != nil {
				t.Fatal(err)
			}
		}},
		{"max tokens", func() *script { return &script{stop: "max_tokens"} }, func(*testing.T, *rig, *script) {}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, Options{})
			r.queue("queued before the fence")
			sc := r.s.script(tc.sc())
			r.lastPrompt("the last")
			tc.wait(t, r, sc)
			r.until(ended("turn-1"))
			r.sync()
			r.wantPrompts("the last")
			r.wantRows()
			_, err := r.e.Submit(Command{}, "after", SubmitQueue, "")
			wantFenced(t, "a prompt after the ending", err)
		})
	}
}

// TestASendArmedBeforeTheFenceFiresAheadOfIt: a send armed before the fence replaces
// its turn, ahead of the fenced row, which then runs. The cancel the send asked for is
// held, so the send is armed for certain when the fence is admitted. One armed behind
// the fenced turn cannot be made at all (TestAFencedTurnRefusesEveryLaterPrompt…).
func TestASendArmedBeforeTheFenceFiresAheadOfIt(t *testing.T) {
	r := newRig(t, Options{})
	first := r.s.script(held())
	r.submit("running")
	await(t, first.opened, "the running turn to open")
	entered, release := r.s.holdNextCancel()
	if res := r.sendNow("armed", ""); !res.Armed {
		t.Fatalf("the send-now: %+v", res)
	}
	await(t, entered, "the armed send's cancel")
	if st := r.e.State(); st.SendNow == nil {
		t.Fatalf("the send is not armed at the fence's admission: %+v", st)
	}
	if res := r.lastPrompt("the last"); res.Queued == nil {
		t.Fatalf("the fenced prompt behind an armed send: %+v", res)
	}
	release()
	r.until(ended("turn-3"))
	r.sync()
	r.wantPrompts("running", "armed", "the last")
}

// TestAnInterjectionIsRefusedUnderTheFence: during the fenced turn and after it, so
// none can arrive after the turn and become a turn of the agent's own. The fake session
// would answer anything it was asked otherwise. A close refuses it as it did, before the
// fence's word.
func TestAnInterjectionIsRefusedUnderTheFence(t *testing.T) {
	r := newRig(t, Options{})
	turn := r.s.script(held())
	r.lastPrompt("the last")
	await(t, turn.opened, "the fenced turn to open")
	wantFenced(t, "an interjection into the fenced turn", r.e.Interject(context.Background(), Command{}, "and also"))
	turn.release()
	r.until(ended("turn-1"))
	wantFenced(t, "an interjection after it", r.e.Interject(context.Background(), Command{}, "and also"))
	if err := r.e.Close(); err != nil {
		t.Fatal(err)
	}
	if err := r.e.Interject(context.Background(), Command{}, "and also"); !errors.Is(err, ErrNotAccepting) {
		t.Fatalf("after a close: %v", err)
	}
}

// TestTheFencedRowRemovedLiftsTheFence: a row taken away before it ran leaves no
// turn for the fence to end with, and a session left refusing every prompt for
// nothing; each removal path lifts it.
func TestTheFencedRowRemovedLiftsTheFence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		remove func(*testing.T, *rig, string)
	}{
		{"Unqueue", func(t *testing.T, r *rig, id string) {
			if _, err := r.e.Unqueue(Command{}, id); err != nil {
				t.Fatal(err)
			}
		}},
		{"ClearQueue", func(t *testing.T, r *rig, _ string) {
			if _, err := r.e.ClearQueue(Command{}); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, Options{})
			first := r.s.script(held())
			r.submit("running")
			await(t, first.opened, "the running turn to open")
			row := r.lastPrompt("the last").Queued
			tc.remove(t, r, row.ID)
			if r.e.State().ExitFenced {
				t.Fatal("the fence stands with its row gone")
			}
			if res := r.submit("next"); res.Queued == nil {
				t.Fatalf("a prompt after the lift: %+v", res)
			}
			first.release()
			r.until(ended("turn-2"))
			r.sync()
			r.wantPrompts("running", "next")
		})
	}
}

// TestARowTheAgentRefusedStaysFenced: the agent's own turn refusing the fenced
// row puts it back, and that ending is not the one the fence waits for: the row
// runs when the agent's turn is over, and only its ending ends the session's turns.
func TestARowTheAgentRefusedStaysFenced(t *testing.T) {
	r := newRig(t, Options{})
	first := r.s.script(held())
	r.submit("running")
	await(t, first.opened, "the running turn to open")
	row := r.lastPrompt("the last").Queued
	r.s.script(&script{refuse: agent.ErrForeignTurn})
	first.release()
	got := r.until(ended("turn-2"))
	if end := got[len(got)-1].Turn; !end.Synthetic || end.ErrClass != agent.EventErrForeignTurn {
		t.Fatalf("the refusal's ending: %+v", end)
	}
	st := r.e.State()
	if !st.ExitFenced || len(st.Queue) != 1 || st.Queue[0].ID != row.ID {
		t.Fatalf("after the refusal: %+v", st)
	}
	_, err := r.e.Submit(Command{}, "too late", SubmitQueue, "")
	wantFenced(t, "a prompt with the fenced row put back", err)
	r.until(ended("turn-3"))
	r.sync()
	r.wantRows()
	_, err = r.e.Queue(Command{}, "after")
	wantFenced(t, "a row after the fenced turn", err)
	r.e.mu.Lock()
	over := r.e.exitFenceEndedLocked()
	r.e.mu.Unlock()
	if !over {
		t.Fatal("the row's own turn did not end the fence")
	}
}

// TestTheFenceKeepsTheSessionsOwnTurnOut: with the fenced turn over, the admission
// fence a session starts its own turn behind stays up. A wake waiting for it never
// comes.
func TestTheFenceKeepsTheSessionsOwnTurnOut(t *testing.T) {
	fr := newFenceRig(t, ChainPolicy{})
	turn := fr.s.script(held())
	if _, err := fr.e.SubmitExitAfterTurn(Command{}, "the last"); err != nil {
		t.Fatal(err)
	}
	await(t, turn.opened, "the fenced turn to open")
	fr.fs.pendWake()
	turn.release()
	awaitTurn(t, fr.returned, "turn-1")
	fr.fs.take()
	fr.fs.fmu.Lock()
	up := fr.fs.up
	fr.fs.fmu.Unlock()
	if !up {
		t.Fatal("the admission fence came down behind the fenced turn: the session can start a turn of its own")
	}
	// Whatever the session still owes can then never be delivered: it must not pin
	// the host for ever behind a wake that cannot come.
	fr.fs.owe(true)
	if fr.e.Busy() {
		t.Fatal("owed work that can never claim keeps the session busy")
	}
	release, busy := fr.e.FenceClose()
	release()
	if busy {
		t.Fatal("the fence's verdict: owed work that can never claim keeps the session busy")
	}
}

// TestAnAskDoesNotPinWhatDoesNotWaitForIt: an open ask makes a session busy
// (an interactive host's rule), but not one whose asks do not pin it, nor one under
// its exit fence: its held turn and the rows behind it are what the ask holds up.
// Anything else in flight still counts.
func TestAnAskDoesNotPinWhatDoesNotWaitForIt(t *testing.T) {
	open := func(t *testing.T, opts Options, last bool) *rig {
		t.Helper()
		r := newRig(t, opts)
		sc := r.s.script(asking(held(), true))
		t.Cleanup(sc.release)
		var err error
		if last {
			_, err = r.e.SubmitExitAfterTurn(Command{}, "ask")
		} else {
			_, err = r.e.Submit(Command{}, "ask", SubmitQueue, "")
		}
		if err != nil {
			t.Fatal(err)
		}
		await(t, sc.ask.opened, "the ask to open")
		return r
	}
	t.Run("an ordinary session is pinned", func(t *testing.T) {
		r := open(t, Options{}, false)
		if !r.e.Busy() {
			t.Fatal("not busy with an ask open")
		}
		release, busy := r.e.FenceClose()
		release()
		if !busy {
			t.Fatal("the fence's verdict: not busy with an ask open")
		}
	})
	for _, tc := range []struct {
		name string
		opts Options
		last bool
	}{
		{"asks do not pin", Options{AsksDoNotPin: true}, false},
		{"under the exit fence", Options{}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := open(t, tc.opts, tc.last)
			if !tc.last {
				if res := r.submit("behind it"); res.Queued == nil {
					t.Fatalf("a row behind the ask: %+v", res)
				}
			}
			if r.e.Busy() {
				t.Fatal("busy with nothing but an open ask")
			}
			release, busy := r.e.FenceClose()
			release()
			if busy {
				t.Fatal("the fence's verdict: busy with nothing but an open ask")
			}
		})
	}
	t.Run("with no ask open the turn counts", func(t *testing.T) {
		r := newRig(t, Options{AsksDoNotPin: true})
		sc := r.s.script(held())
		r.submit("working")
		await(t, sc.opened, "the turn to open")
		if !r.e.Busy() {
			t.Fatal("a turn working with no ask open is not busy")
		}
	})
}
