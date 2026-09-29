package engine

import (
	"time"

	"github.com/charliek/craze/internal/agent"
)

// Activity is what the engine is doing, in the words the protocol's gate table
// uses. "Blocked on an ask" and "the agent is running a turn of its own" are
// not activities: both overlay any of these, and are read from State's other
// fields.
type Activity string

const (
	ActivityStarting  Activity = "starting"
	ActivityReplaying Activity = "replaying"
	ActivityIdle      Activity = "idle"
	ActivityWorking   Activity = "working"
	ActivityError     Activity = "error"
	ActivityClosing   Activity = "closing"
)

// State is the session's snapshot with the engine's own state merged in:
// everything a client needs to draw the session and everything host.Derive
// reads, so that a host with no TUI can publish the same status a TUI would.
//
// It is a read of two authorities and is not a cut through the event stream:
// the snapshot is read first and outside e.mu, the engine's fields after it,
// and an engine event trails the state it describes. A client folds events for
// order and reads State for where things stand.
type State struct {
	agent.Snapshot
	// Queue is craze's own message queue in send order. The queue left the
	// provider seam in plan 021's C6, so this is the only place it lives —
	// nothing here shadows a field on the embedded Snapshot any more.
	Queue    []agent.QueuedPrompt
	Activity Activity
	// Turn is the current turn's id, "" when none is.
	Turn string
	// Waiting says the current turn's claim was refused because the agent is
	// running a turn of its own, and the turn is waiting to be claimed again
	// under ChainPolicy.RetryForeignTurn. It is the wait a client that owns the
	// budget bounds (§3.4): the engine claims again for as long as the client
	// lets it, and the client ends the wait with GiveUp.
	//
	// The one subtlety, and it matters: Waiting is false for the instant a
	// re-claim is in flight — a refused claim's continuation comes back at once,
	// but not instantly — so **false does not mean "running"**. A client may act
	// on Waiting == true (start timing, and give up once its budget is spent) and
	// must never read a single false as "the prompt went through": it would clear
	// a budget that is still being spent, and a genuinely running foreign turn —
	// where the engine rightly declines to claim again and this stays true for as
	// long as it lasts — would never be given up on. GiveUp is conditional for
	// the other half of the same reason: by the time a client acts, the claim may
	// have gone through.
	Waiting bool
	// SendNow is the armed send-now, nil when nothing is armed. Nothing it
	// names has been consumed: the row it points at is still in Queue and a
	// draft is still in the client's composer.
	SendNow *ArmedSend
	// Err is the failure an Activity of error stands on.
	Err string
	// Cancelled says the last turn to settle ended cancelled, and Prompted
	// that a turn has been started at all: together they are what tells an
	// idle that followed Esc from one that followed an answer, and from one
	// that has seen nothing yet.
	Cancelled bool
	Prompted  bool
	// PendingAsks is how many blocking requests the agent is waiting on, and
	// HeadAsk is the first of them — the one a client would show. Both are read
	// from the registry, **outside e.mu**, because the engine's mutex and the
	// registry's are never nested in either direction (plan 021 §3.6); like the
	// snapshot, they are a read of another authority and not a cut through the
	// event stream.
	//
	// An ask overlays every activity: a session blocked on one may be idle,
	// working, or watching a turn the agent started itself, and 05's gate table
	// reads it from here rather than from Activity.
	PendingAsks int
	HeadAsk     HeadAsk
	// StartFailed says Start returned an error; Err carries it.
	StartFailed bool
	// Incarnation is the log's id: the scope of turn ids and sequence numbers.
	Incarnation string
	// CrazeSessionID is the durable craze session id (session control SD-22):
	// the identity of this thread of work, which survives a session/load into
	// a new agent session and a host restart. It is fixed for the engine's
	// life. The other two identities are beside it: Snapshot.SessionID is the
	// PROVIDER's, which changes with every load, and Incarnation is this one
	// process lifetime.
	CrazeSessionID string
	// RetryHorizon is the command-id table's bound (receipts.go): within it, a
	// resent command id is answered from the table and never re-executes;
	// past it, ErrUnknownCommand.
	RetryHorizon RetryHorizon
	// LastTurn is how the session's most recent turn ended — craze's own or
	// the agent's (a foreign turn) — and nil while a turn runs, before any
	// has ended, and after a turn has started since (plan 030 §3.7, SF-57).
	// It is the observer's, kept in commit order from the ending and
	// starting events themselves (observe), so it agrees with what a client
	// folding the stream has seen up to the same seq; like the snapshot and
	// the asks, it is a read of its own and not a cut with the rest of State.
	LastTurn *LastTurn
}

// LastTurn is how one turn ended (plan 030 §3.7): session.state's and a
// sessions.list row's lastTurn. It is kept from the stream's own events —
// never the snapshot, whose codec stays at version 1 — so the one field
// about the previous ending survives a client's reconnect as a read.
type LastTurn struct {
	// Outcome is done, failed or cancelled (TurnOutcome).
	Outcome TurnOutcome
	// Err is a failed turn's error text, the ending's own (TurnInfo.Err); ""
	// for the other outcomes.
	Err string
	// EndedAt is the ending event's time (Event.At, the session's clock for
	// an ending the engine authored).
	EndedAt time.Time
	// TurnID is the ended turn's id as the stream names it: the engine's
	// "turn-N" for a turn of craze's own — the id its started and ended
	// events carry, which a client's fold sees too — and the agent's own id
	// for a foreign turn (ForeignTurnInfo.ID, "" if the agent gave none).
	TurnID string
}

// TurnOutcome is how a turn ended, in the protocol's words.
type TurnOutcome string

const (
	// TurnDone is a turn that ran to its end with no error and no cancel —
	// and every foreign turn: its closing bracket carries no outcome.
	TurnDone TurnOutcome = "done"
	// TurnFailed is a turn whose ending carried an error (TurnInfo.Err): the
	// agent's failure, a refusal the engine turned into the ending, and the
	// restoring refusal (SF-21) too, whose ending carries the refusal's
	// error even though the engine is idle and the row is back in the queue
	// — the same error row a client's fold draws for it.
	TurnFailed TurnOutcome = "failed"
	// TurnCancelled is a turn stopped before its end: the agent's own
	// cancelled stop, a prompt withdrawn before it was sent (synthetic,
	// cancelled), and the turn Close ended (synthetic, closing).
	TurnCancelled TurnOutcome = "cancelled"
)

// turnEnding is what one main-transcript event says about the last turn:
// starts when it starts a turn (craze's started, a foreign turn's running
// bracket), which supersedes the last ending, and ended when it ends one, the
// ending to keep. A replayed event is history, not this session's turn, and
// says nothing; so does every other event.
func turnEnding(ev agent.Event) (starts bool, ended *LastTurn) {
	if ev.Replayed {
		return false, nil
	}
	at := ev.At
	if at.IsZero() {
		// Every ending the engine authors is stamped (stamp), and every
		// session stamps what it emits; this is for one that did not.
		at = time.Now()
	}
	switch ev.Type {
	case agent.EventTurn:
		tu := ev.Turn
		if tu == nil {
			return false, nil
		}
		switch tu.Phase {
		case agent.TurnStarted:
			return true, nil
		case agent.TurnEnded:
			lt := &LastTurn{Outcome: TurnDone, EndedAt: at, TurnID: tu.ID}
			switch {
			case tu.Err != "":
				lt.Outcome, lt.Err = TurnFailed, tu.Err
			case tu.StopReason == stopCancelled, tu.StopReason == stopClosing:
				lt.Outcome = TurnCancelled
			}
			return false, lt
		}
	case agent.EventForeignTurn:
		ft := ev.ForeignTurn
		if ft == nil {
			return false, nil
		}
		if ft.Running {
			return true, nil
		}
		return false, &LastTurn{Outcome: TurnDone, EndedAt: at, TurnID: ft.ID}
	}
	return false, nil
}

// HeadAsk is the ask at the head of the queue of open ones, as a status line
// names it. The zero value — an empty Kind — means nothing is pending.
//
// Label is agent.AskLabel: `permission <tool>`, `question`, `plan <name>`, the
// same text the TUI's own card draws, so a host with no TUI publishes the same
// reason a TUI would (host.Derive's CardLabel).
type HeadAsk struct {
	ID    string
	Kind  agent.AskKind
	Label string
}

// State reads the session's snapshot outside e.mu — it copies a good deal, and
// takes the session's lock — and merges the engine's fields in under it. The
// registry is read outside e.mu for the same reason, and because the two
// mutexes are never nested.
func (e *Engine) State() State {
	st := State{
		Snapshot:       e.sess.Snapshot(),
		Incarnation:    e.log.Incarnation(),
		CrazeSessionID: e.craze,
		RetryHorizon:   e.receipts.horizon(),
	}
	if asks := e.asks.Asks(); len(asks) > 0 {
		st.PendingAsks = len(asks)
		st.HeadAsk = HeadAsk{ID: asks[0].ID, Kind: asks[0].Kind, Label: asks[0].Label()}
	}
	replaying, last := e.observed()
	st.LastTurn = last
	e.mu.Lock()
	defer e.mu.Unlock()
	st.Queue = e.queue.List()
	st.Activity = e.activity
	if replaying && (st.Activity == ActivityStarting || st.Activity == ActivityIdle) {
		st.Activity = ActivityReplaying
	}
	if e.cur != nil {
		st.Turn = e.cur.id
		st.Waiting = e.cur.retry
	}
	if a := e.armed; a != nil {
		st.SendNow = &ArmedSend{Text: a.text, FromRow: a.from, Turn: a.turn, Cause: a.cause}
	}
	st.Err = e.err
	st.Cancelled = e.cancelled
	st.Prompted = e.prompted
	st.StartFailed = e.startFailed
	return st
}
