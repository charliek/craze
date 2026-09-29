package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
)

// The last turn's ending (plan 030 §3.7, SF-57): State.LastTurn is kept from
// the stream's own turn events, in commit order — craze's endings and a
// foreign turn's alike — and a turn starting supersedes it.

// fakeClock is the fakeSession's fixed clock, which every ending the engine
// authors is stamped with (Clocked).
var fakeClock = time.Unix(1_700_000_000, 0).UTC()

// lastTurn is State.LastTurn once everything enqueued so far is committed —
// and so observed: the drainer commits a batch, the observer inside it, before
// a Flush it answers returns (agent.EventLog.FlushSeq).
func (r *rig) lastTurn() *LastTurn {
	r.t.Helper()
	r.sync()
	return r.e.State().LastTurn
}

// wantLastTurn fails unless the last turn is want (nil: none).
func (r *rig) wantLastTurn(want *LastTurn) {
	r.t.Helper()
	got := r.lastTurn()
	switch {
	case want == nil && got == nil:
		return
	case want == nil || got == nil:
		r.t.Fatalf("the last turn is %+v, want %+v", got, want)
	case *got != *want:
		r.t.Fatalf("the last turn is %+v, want %+v", *got, *want)
	}
}

// foreignTurn brackets a foreign turn with an id of the agent's own, as a
// native wake or grok's own turn does: the flag, then the event.
func (r *rig) foreignTurn(id string, running bool) {
	r.s.mu.Lock()
	r.s.foreign = running
	r.s.mu.Unlock()
	r.s.emit(agent.Event{Type: agent.EventForeignTurn, ForeignTurn: &agent.ForeignTurnInfo{ID: id, Running: running}})
}

// TestTheLastTurnIsEachTurnsEnding: every way a turn ends, each the one field
// State carries about it — its outcome, a failure's text, its ending's time
// on the session's clock, and the turn's own id as its events name it.
func TestTheLastTurnIsEachTurnsEnding(t *testing.T) {
	t.Run("none before any turn", func(t *testing.T) {
		r := newRig(t, Options{})
		r.wantLastTurn(nil)
	})
	t.Run("done, named by the turn's own id", func(t *testing.T) {
		r := newRig(t, Options{})
		res := r.submit("one")
		got := r.until(ended(res.Turn))
		if id := got[len(got)-1].Turn.ID; id != res.Turn {
			t.Fatalf("the ending names %s, the submit started %s", id, res.Turn)
		}
		r.wantLastTurn(&LastTurn{Outcome: TurnDone, EndedAt: fakeClock, TurnID: res.Turn})
	})
	t.Run("failed, with the ending's error", func(t *testing.T) {
		r := newRig(t, Options{})
		boom := errors.New("the agent fell over")
		r.s.script(&script{fail: boom})
		res := r.submit("one")
		r.until(ended(res.Turn))
		r.wantLastTurn(&LastTurn{Outcome: TurnFailed, Err: boom.Error(), EndedAt: fakeClock, TurnID: res.Turn})
		if st := r.e.State(); st.Activity != ActivityError || st.Err != boom.Error() {
			t.Fatalf("the engine is %s with %q, want error with the turn's", st.Activity, st.Err)
		}
	})
	t.Run("cancelled by the agent's own cancelled stop", func(t *testing.T) {
		r := newRig(t, Options{})
		sc := r.s.script(held())
		res := r.submit("one")
		await(t, sc.opened, "the turn to open")
		if _, err := r.e.Cancel(context.Background(), Command{}, res.Turn); err != nil {
			t.Fatal(err)
		}
		r.until(ended(res.Turn))
		r.wantLastTurn(&LastTurn{Outcome: TurnCancelled, EndedAt: fakeClock, TurnID: res.Turn})
	})
	t.Run("cancelled by the close that ended it", func(t *testing.T) {
		r := newRig(t, Options{})
		sc := r.s.script(held())
		res := r.submit("one")
		await(t, sc.opened, "the turn to open")
		r.until(started(res.Turn))
		// Close authors the running turn's ending (synthetic, closing) and
		// closes the log, committing it: the observer has seen it by the time
		// Close returns.
		if err := r.e.Close(); err != nil {
			t.Fatal(err)
		}
		got := r.e.State().LastTurn
		if want := (LastTurn{Outcome: TurnCancelled, EndedAt: fakeClock, TurnID: res.Turn}); got == nil || *got != want {
			t.Fatalf("after the close the last turn is %+v, want %+v", got, want)
		}
	})
	t.Run("a foreign turn's ending, named by the agent's id", func(t *testing.T) {
		r := newRig(t, Options{})
		r.foreignTurn("wake-1", true)
		r.wantLastTurn(nil)
		r.foreignTurn("wake-1", false)
		r.wantLastTurn(&LastTurn{Outcome: TurnDone, EndedAt: fakeClock, TurnID: "wake-1"})
	})
	t.Run("a replayed bracket is history, not this session's turn", func(t *testing.T) {
		r := newRig(t, Options{})
		res := r.submit("one")
		r.until(ended(res.Turn))
		want := &LastTurn{Outcome: TurnDone, EndedAt: fakeClock, TurnID: res.Turn}
		r.wantLastTurn(want)
		for _, running := range []bool{true, false} {
			r.s.emit(agent.Event{Type: agent.EventForeignTurn, Replayed: true,
				ForeignTurn: &agent.ForeignTurnInfo{ID: "old", Running: running}})
			r.wantLastTurn(want)
		}
	})
}

// TestTheNextTurnsStartSupersedesTheLastTurn: a turn starting — craze's own
// or the agent's — clears the last ending, so a present one always names the
// latest turn with nothing run since; the new turn's own ending replaces it.
func TestTheNextTurnsStartSupersedesTheLastTurn(t *testing.T) {
	r := newRig(t, Options{})
	boom := errors.New("the agent fell over")
	r.s.script(&script{fail: boom})
	first := r.submit("one")
	r.until(ended(first.Turn))
	r.wantLastTurn(&LastTurn{Outcome: TurnFailed, Err: boom.Error(), EndedAt: fakeClock, TurnID: first.Turn})

	sc := r.s.script(held())
	second := r.submit("two")
	await(t, sc.opened, "the second turn to open")
	r.until(started(second.Turn))
	r.wantLastTurn(nil)
	if st := r.e.State(); st.Turn != second.Turn {
		t.Fatalf("the current turn is %q, want %s", st.Turn, second.Turn)
	}
	sc.release()
	r.until(ended(second.Turn))
	r.wantLastTurn(&LastTurn{Outcome: TurnDone, EndedAt: fakeClock, TurnID: second.Turn})

	// A foreign turn starting supersedes craze's own ending too.
	r.foreignTurn("grok-7", true)
	r.wantLastTurn(nil)
	r.foreignTurn("grok-7", false)
	r.wantLastTurn(&LastTurn{Outcome: TurnDone, EndedAt: fakeClock, TurnID: "grok-7"})
}
