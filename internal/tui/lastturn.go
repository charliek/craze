package tui

import (
	"context"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/craze/internal/engine"
)

// The last turn's ending after a restore (plan 030 §3.7, SF-57 in part;
// round-2 finding R2-7).
//
// A restore replaces what the model held with the session's snapshot, and the
// snapshot keeps no turn's ending — its codec stays at version 1, so a client
// and a host of different builds still attach — so a turn that failed while
// this client was away (its terminal closed, its connection lost) restored as
// idle: the error row was in the transcript, and nothing else said it failed.
// The session's last ending travels beside the snapshot instead, as
// session.state's lastTurn, and the model reads it once after every restore
// (readLastTurn): a failed ending puts the model in the error state a live
// failure leaves it in (applyTurnEnded's) — the failed status the tab title
// marks and a roost or herdr host reports, with the ending's error — and any
// other ending takes an error state the restore kept away, since the turn it
// was about is no longer the last one. Nothing is drawn: the failure's row is
// the fold's, and the snapshot holds it.
//
// The read is a round trip, and a turn can begin and end while it is on its
// way, so its answer is tagged (lastTurnTag) with what it was sent under — the
// backend generation, the backend's epoch, the restore it follows and the
// turns the model had seen begin — and applied only while all four still
// stand. An answer that finds another backend adopted (a switch, plan 030
// §3.11), the backend bound to another session (its epoch moved: a reconnect
// that could not resume), another restore applied, or any turn begun since,
// is dropped: the stream has said, or is about to say, more than it can, and a
// delayed answer never puts back an ending the session has moved past. "Any
// turn begun since" is how the answer's TurnID is held against the fold's
// turns: those ids are not ordered — craze's own are "turn-N", the agent's
// own turns carry ids of the agent's — and the fold keeps no id of a turn once
// it has ended, so rather than ask whether the fold has seen a turn later than
// the answer's, the model counts the turns it has seen begin since the
// restore, and any at all means the answer may name one the fold has gone
// past. One it drops that way named at most the turn the fold has just seen
// end, whose ending the live stream has already applied. A session the model
// has left altogether is the session generation's (issued, outdated), as for
// every result.
//
// In process no restore ever comes, and the model never reads it: the fold
// sees every ending as it happens.

// lastTurnDeadline bounds the read: a host that does not answer leaves the
// model as the restore left it. A variable so a test can shorten it.
var lastTurnDeadline = 10 * time.Second

// lastTurnTag is what a read after a restore was sent under (see above).
type lastTurnTag struct {
	// bgen is the backend generation (Model.bgen, plan 030 §3.11; X52): a
	// read of a backend a switch has left answers for a session the model
	// no longer shows, whatever its epoch — two backends' epochs are two
	// counts, and can meet.
	bgen uint64
	// epoch is the backend's Epoch when the read was sent.
	epoch uint64
	// restore is the restore it follows (Model.restores).
	restore uint64
	// starts is the turns the model had seen begin (Model.turnStarts).
	starts uint64
}

// lastTurnMsg is the read's answer: the session's last ending (nil for none —
// a turn running, none ended yet, a host from before plan 030), or why there
// is no answer.
type lastTurnMsg struct {
	issued
	tag  lastTurnTag
	last *engine.LastTurn
	err  error
}

// readLastTurn is the read after the restore the model has just applied: a
// tea.Cmd, never a gated call — nothing waits for it — whose answer carries
// the tag (lastTurnTag) and the session generation. The backend refuses it
// before sending if its epoch has moved by the time it runs (dispatchCtx).
func (m *Model) readLastTurn() tea.Cmd {
	b := m.eng
	if b == nil {
		return nil
	}
	iss := m.issue()
	tag := lastTurnTag{bgen: m.bgen, epoch: b.Epoch(), restore: m.restores, starts: m.turnStarts}
	ctx := dispatchCtx(b)
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, lastTurnDeadline)
		defer cancel()
		last, err := b.LastTurn(ctx)
		return lastTurnMsg{issued: iss, tag: tag, last: last, err: err}
	}
}

// applyLastTurn applies a read's answer when its tag still stands (see
// above), and when nothing the model now shows outranks it: a session that
// never started (its start failure is the error), and a turn on screen as
// working — its ending is the stream's to deliver — take nothing from it. A
// read that failed says nothing, and neither does an ending of a kind this
// build does not know.
func (m *Model) applyLastTurn(msg lastTurnMsg) {
	lt := msg.last
	switch {
	case msg.err != nil, lt == nil, m.eng == nil:
		return
	case msg.tag.bgen != m.bgen, msg.tag.epoch != m.eng.Epoch(), msg.tag.restore != m.restores, msg.tag.starts != m.turnStarts:
		return
	case m.startErr != nil, m.status == statusWorking:
		return
	}
	switch lt.Outcome {
	case engine.TurnFailed:
		// What a live failure leaves (applyTurnEnded): the error state, and
		// the ending's error for what reports it.
		m.status, m.err = statusError, lt.Err
	case engine.TurnDone, engine.TurnCancelled:
		// A later ending than the failure the model was showing: the turn
		// that failed is not the last one any more.
		if m.status == statusError {
			m.status, m.err = statusIdle, ""
		}
	}
}
