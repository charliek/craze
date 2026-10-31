package engine

import "errors"

// The exit fence: a prompt submitted to be its session's last (session.prompt's
// exitAfterTurn; craze new --wait and craze send --wait with --exit-after-turn).
// The host is to end as that prompt's turn does, and it is the waiter that ends
// it, once its result is written; what the engine owes is that no other turn
// starts first, whenever the waiter's session.stop arrives or if it never does.
//
//   - From the fenced prompt's admission no further prompt is admitted: Submit,
//     Queue and Interject answer ErrExitFenced, in the same section that would
//     have taken it. A prompt admitted then could only be thrown away when the
//     turn ended, after its sender had been told it was queued; an interjection
//     arriving after the turn could become a turn of the agent's own. An agent
//     that starts a turn entirely by itself cannot be fenced: its stop ends it.
//   - A row queued ahead of the fenced one was admitted first and runs first, in
//     order, and a client may still start it, or the fenced row itself, by its id.
//   - A row behind the fenced turn can exist only if it was queued before the
//     fence, with the session idle (Queue never starts a turn). It is removed in
//     the settlement that ends the fenced turn, ahead of the ending, as a chain
//     policy's clear is, so a client following it reads it dropped. A send cannot
//     be armed against the fenced turn: arming takes a prompt, and the fence
//     refuses it.
//   - The fenced turn's ending, whatever it was, ends it: nothing is claimed from
//     then on (canStartLocked), the session's admission fence stays up
//     (fenceWantLocked), and the fence is never lowered. A row the agent's own
//     turn refused goes back to the queue still fenced (restoreLocked): that turn
//     did not end what the waiter is waiting for.
//   - A fenced row removed before it ran lifts the fence: there is no turn left
//     for it to end with, and whoever removed it is using the session. The waiter
//     following the row reads it dropped and sends no stop, which would cancel
//     what that client runs next.
//
// It lasts for the engine's life and nothing records it: a host that loads the
// session again has no fence.

// ErrExitFenced is a prompt refused because the session's exit fence is up (the
// comment above).
var ErrExitFenced = errors.New("engine: this session ends with its current prompt (--exit-after-turn), and takes no other")

// exitFence is the fence's state: the fenced prompt's row while it waits in the
// queue ("" for one that started at once), its turn once it runs, and whether
// that turn has ended. e.mu guards it.
type exitFence struct {
	row, turn string
	ended     bool
}

// SubmitExitAfterTurn is Submit in queue mode that also raises the exit fence,
// in the section that admits the prompt (the comment above). A fence already up
// refuses it, as any prompt: the first prompt to ask is the session's last.
func (e *Engine) SubmitExitAfterTurn(c Command, text string) (SubmitResult, error) {
	hash := receiptHash("SubmitExitAfterTurn", text)
	return withSyncReceipt(e.receipts, c, hash, func() (SubmitResult, error) {
		admitted, err := e.admitText(text)
		if err != nil {
			return SubmitResult{}, err
		}
		return e.submit(c, admitted, SubmitQueue, "", true)
	})
}

// exitFenceRefusalLocked is why a prompt is refused under the exit fence, or nil
// for no fence or a prompt naming a row at or ahead of a fenced row still
// waiting. fromRow is the row the prompt names, "" for new text.
func (e *Engine) exitFenceRefusalLocked(fromRow string) error {
	f := e.exitFence
	if f == nil {
		return nil
	}
	if fromRow != "" && f.turn == "" && !f.ended {
		for _, row := range e.queue.List() {
			if row.ID == fromRow {
				return nil
			}
			if row.ID == f.row {
				break
			}
		}
	}
	return ErrExitFenced
}

// exitFenceRowTakenLocked is t, a turn just claimed from its row (t.row), told to
// the fence: it was the fenced row, and is now the fenced turn.
func (e *Engine) exitFenceRowTakenLocked(t *turn) {
	if f := e.exitFence; f != nil && f.turn == "" && t.row != nil && t.row.ID == f.row {
		f.turn = t.id
	}
}

// exitFenceEndsLocked says t's settlement ends the fenced turn, and marks it.
func (e *Engine) exitFenceEndsLocked(t *turn) bool {
	f := e.exitFence
	if f == nil || f.ended || f.turn != t.id {
		return false
	}
	f.ended = true
	return true
}

// exitFenceLiftLocked lowers a fence whose row has gone from the queue without
// becoming a turn. It runs on every section's way out (syncFenceLocked): by then
// a row that left for a turn has told the fence so.
func (e *Engine) exitFenceLiftLocked() {
	f := e.exitFence
	if f == nil || f.ended || f.turn != "" {
		return
	}
	if _, ok := e.rowLocked(f.row); !ok {
		e.exitFence = nil
	}
}

// exitFenceEndedLocked says the fenced turn has ended: nothing starts again.
func (e *Engine) exitFenceEndedLocked() bool {
	return e.exitFence != nil && e.exitFence.ended
}
