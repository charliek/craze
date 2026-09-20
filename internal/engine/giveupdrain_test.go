package engine

import (
	"errors"
	"testing"

	"github.com/charliek/craze/internal/agent"
)

// heldBehindAForeignTurn is an engine whose first turn has ended with one row
// queued behind a turn of the agent's own: idle, nothing current, the row
// waiting, the session's flag up.
func heldBehindAForeignTurn(t *testing.T) *rig {
	t.Helper()
	r := newRig(t, Options{Chain: ChainPolicy{StopOnNonEndTurn: true, RetryForeignTurn: true}})
	first := r.s.script(held())
	r.submit("one")
	await(t, first.opened, "the first turn to open")
	if _, err := r.e.Queue(Command{}, "two"); err != nil {
		t.Fatal(err)
	}
	r.s.setForeign(true)
	r.sync()
	first.release()
	got := r.until(ended("turn-1"))
	if last := got[len(got)-1].Turn; last.Next != "" || last.Pending != 1 {
		t.Fatalf("an ending behind a foreign turn: %+v", last)
	}
	return r
}

// TestGiveUpDrainAbandonsARowTheAgentIsStillHolding: the agent's turn is still
// running, so the pass claims nothing, and the drain is abandoned for good: the
// row stays where it was, nothing is cancelled, and the engine admits nothing
// further — so the row cannot start behind a client that has been told it will
// not.
func TestGiveUpDrainAbandonsARowTheAgentIsStillHolding(t *testing.T) {
	r := heldBehindAForeignTurn(t)
	turn, pending, err := r.e.GiveUpDrain(Command{})
	if err != nil || turn != "" || pending != 1 {
		t.Fatalf("GiveUpDrain behind a running foreign turn answered %q, %d, %v", turn, pending, err)
	}
	if st := r.e.State(); len(st.Queue) != 1 || st.Queue[0].Text != "two" {
		t.Fatalf("the abandoned row: %+v", st.Queue)
	}
	if n := r.s.cancelsWritten(); n != 0 {
		t.Fatalf("abandoning a drain wrote %d cancels", n)
	}
	// The agent's turn ends now. Nothing drains: the abandonment is not
	// overtaken by the drain it gave up on.
	r.s.setForeign(false)
	r.until(func(ev agent.Event) bool { return ev.Type == agent.EventForeignTurn && !ev.ForeignTurn.Running })
	r.sync()
	r.wantPrompts("one")
	if _, err := r.e.Submit(Command{}, "late", SubmitQueue, ""); !errors.Is(err, ErrNotAccepting) {
		t.Fatalf("a submit after an abandoned drain: %v", err)
	}
}

// TestGiveUpDrainTakesTheRowOnceTheAgentHasLetGo is the instant a client cannot
// see from outside: the session's flag is down and nobody has woken the driver
// yet, so nothing is current and the row is still queued. The operation makes
// the pass itself, reading the flag where it would claim, and the row's turn is
// current when it answers — nothing was given up.
func TestGiveUpDrainTakesTheRowOnceTheAgentHasLetGo(t *testing.T) {
	r := heldBehindAForeignTurn(t)
	// The flag alone, with no event: the agent's turn is over as far as the
	// session knows, and the engine's driver has not been told.
	r.s.setForeignSilently(false)
	turn, pending, err := r.e.GiveUpDrain(Command{})
	if err != nil || turn != "turn-2" || pending != 0 {
		t.Fatalf("GiveUpDrain with the flag down answered %q, %d, %v: want the row's turn", turn, pending, err)
	}
	got := r.until(lastEnding)
	r.wantShapes(got,
		`queue sent "two"`,
		`started turn-2 drain "two"`,
		`text "echo: two"`,
		`done end_turn`,
		`ended turn-2 stop="end_turn" next="" pending=0`,
	)
	r.wantPrompts("one", "two")
}

// TestGiveUpDrainLeavesARunningTurnAlone: the drain went while the client was
// making up its mind. The operation answers with the turn that is running and
// stops nothing.
func TestGiveUpDrainLeavesARunningTurnAlone(t *testing.T) {
	r := newRig(t, Options{})
	turn := r.s.script(held())
	res := r.submit("one")
	await(t, turn.opened, "the turn to open")
	got, pending, err := r.e.GiveUpDrain(Command{})
	if err != nil || got != res.Turn || pending != 0 {
		t.Fatalf("GiveUpDrain with a turn running answered %q, %d, %v", got, pending, err)
	}
	turn.release()
	r.until(lastEnding)
	// Nothing was stopped: the engine still admits.
	r.submit("two")
	r.until(lastEnding)
	r.wantPrompts("one", "two")
}

func TestGiveUpDrainOnAClosedEngine(t *testing.T) {
	r := newRig(t, Options{})
	if err := r.e.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.e.GiveUpDrain(Command{}); !errors.Is(err, ErrNotAccepting) {
		t.Fatalf("GiveUpDrain after Close: %v", err)
	}
}
