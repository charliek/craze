package engine

import "github.com/charliek/craze/internal/agent"

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
	// Queue is craze's own message queue in send order. It shadows the
	// snapshot's, which goes when the queue leaves the provider seam.
	Queue    []agent.QueuedPrompt
	Activity Activity
	// Turn is the current turn's id, "" when none is.
	Turn string
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
	// StartFailed says Start returned an error; Err carries it.
	StartFailed bool
	// Incarnation is the log's id: the scope of turn ids and sequence numbers.
	Incarnation string
}

// State reads the session's snapshot outside e.mu — it copies a good deal, and
// takes the session's lock — and merges the engine's fields in under it.
func (e *Engine) State() State {
	st := State{Snapshot: e.sess.Snapshot(), Incarnation: e.log.Incarnation()}
	replaying := e.isReplaying()
	e.mu.Lock()
	defer e.mu.Unlock()
	st.Queue = e.queue.List()
	st.Activity = e.activity
	if replaying && (st.Activity == ActivityStarting || st.Activity == ActivityIdle) {
		st.Activity = ActivityReplaying
	}
	if e.cur != nil {
		st.Turn = e.cur.id
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
