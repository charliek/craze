package engine

import "github.com/charliek/craze/internal/agent"

// CancelSubagent stops one of the session's running sub-agents — the roster
// row whose id is id — and nothing else: the turn it belongs to goes on, and
// the parent's model reads that the user stopped that child (plan 026 §3.10).
// It is the session's own verb (agent.SubagentCanceller), and the engine adds
// nothing but the door, as it does for Answer.
//
// The answers:
//
//   - nil: the stop was delivered. What it came to is event-only, and the
//     session's: the child's finished roster row (EventSubagent{finished}) is
//     cancelled with the Error "stopped by the user" when the stop alone
//     decided it; a child whose turn had already ended on its own keeps its
//     outcome, and one whose turn's own cancel or whose session's close
//     outranked the stop is cancelled with no such Error. The row is the
//     session's own event and carries no Cause, so the command's cause does
//     not travel onto it (S2's question, 13-follow-ups.md SF-46).
//   - agent.ErrNoSuchSubagent, code unknown_subagent, STORED: the session holds
//     no running child by that id — it never existed, or it has already
//     finished. After a stop the client itself sent, that is the race with the
//     child's own end, not a failure: a client shows nothing for it (at most
//     "already finished"), never an error row, and re-reads the roster if it
//     needs to know how the child ended. The TUI ignores it.
//   - agent.ErrUnsupported, code unsupported, STORED: the session has no
//     per-child stop (Capabilities.SubagentCancel is false on it).
//   - ErrNotAccepting, code not_accepting, NEVER STORED: the engine is closed,
//     the answer GiveUp and Cancel give then (X24).
//
// That last is the engine's one gate. Every other activity — starting,
// replaying, stopped, working, idle, a foreign turn — is the session's to
// judge, because a child exists only inside a turn and the session is the one
// authority on which of its children are running: it answers
// agent.ErrNoSuchSubagent for an id it holds no running child for, whatever
// the engine is doing. So there is no refusalLocked here, and no outbox-room
// check either: the stop publishes nothing itself.
//
// It waits on nothing, so a client may call it from the primary's own reader —
// the TUI does, from Update. e.mu is taken for the one read of closed and
// released before the session is called, never held across it; the session's
// side reads its harness under its own lock, released before the harness is
// asked, and the harness only cancels the child's context. A Close that
// begins after the read is the session's to answer, and it answers
// ErrNoSuchSubagent, or nil for a child still unwinding, whose outcome the
// close then decides.
func (e *Engine) CancelSubagent(c Command, id string) error {
	hash := receiptHash("CancelSubagent", id)
	return withSyncReceiptErr(e.receipts, c, hash, func() error {
		e.mu.Lock()
		closed := e.closed
		e.mu.Unlock()
		if closed {
			return ErrNotAccepting
		}
		canceller, ok := e.sess.(agent.SubagentCanceller)
		if !ok {
			return agent.ErrUnsupported
		}
		return canceller.CancelSubagent(id)
	})
}
