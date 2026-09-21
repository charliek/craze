package engine

import (
	"fmt"

	"github.com/charliek/craze/internal/agent"
)

// The queue verbs. Each is a rejectable admission and each is one section under
// e.mu: the gate, the room in the outbox — checked *before* anything is mutated,
// because agent.PromptQueue's methods mutate and then hand back the events that
// describe it, so by the time there is an event to enqueue the change has
// happened — the queue's own transaction, and the events it returned, enqueued
// with the command that caused them. None of them waits on anything: the caller
// is a bubbletea Update, which is the primary's own reader (§3.2).
//
// None of them starts a turn either, Queue included and however idle the engine
// is. Starting is Submit's, and a client that means "send this" says so: a verb
// that sometimes started a turn would make the band's own buttons a second
// admission path, with a second echo rule to match.
//
// They refuse everything refusalLocked refuses, the removals as much as the
// additions: a stopped or closing engine's record is closed, and a queue nothing
// will ever drain is not a queue. Stop has cleared it by then in any case.

// Queue puts text at the back of the queue. It never starts a turn.
func (e *Engine) Queue(c Command, text string) (agent.QueuedPrompt, error) {
	hash := receiptHash("Queue", text)
	return withSyncReceipt(e.receipts, c, hash, func() (agent.QueuedPrompt, error) {
		e.mu.Lock()
		defer e.mu.Unlock()
		if err := e.refusalLocked(); err != nil {
			return agent.QueuedPrompt{}, err
		}
		if !e.log.OutboxRoom() {
			return agent.QueuedPrompt{}, ErrUnavailable
		}
		row, qev, err := e.queue.Add(text, e.now())
		if err != nil {
			// A full queue or an oversized message, refused with nothing mutated,
			// so the client still has the draft it tried to queue.
			return agent.QueuedPrompt{}, err
		}
		e.log.Enqueue(e.stamp(qev.Event(), c.Cause()))
		return row, nil
	})
}

// EditQueued rewrites a queued row in place: the id and the position are the
// row's identity and do not move, and the row's Version records that it changed.
//
// expectedVersion is the check-and-edit: nil edits unconditionally, and a
// non-nil version the row no longer has is ErrStaleVersion, so two clients
// editing one row cannot silently overwrite each other. It is a pointer and not
// a zero sentinel because a newly queued row's version *is* zero (agent's
// PromptQueue.Add), so zero as the wildcard would make every first edit
// unconditional — which is exactly the pair of edits the check exists for.
//
// The check and the edit are one section, which is what makes the version mean
// anything: every path that touches the queue does so under e.mu, so no other
// client's edit, drain or clear can land between reading the version and
// writing the text.
func (e *Engine) EditQueued(c Command, id, text string, expectedVersion *int) error {
	hash := receiptHash("EditQueued", id, text, versionSpelling(expectedVersion))
	return withSyncReceiptErr(e.receipts, c, hash, func() error {
		e.mu.Lock()
		defer e.mu.Unlock()
		if err := e.refusalLocked(); err != nil {
			return err
		}
		if !e.log.OutboxRoom() {
			return ErrUnavailable
		}
		row, ok := e.rowLocked(id)
		if !ok {
			return fmt.Errorf("%w: %s", ErrUnknownRow, id)
		}
		if expectedVersion != nil && *expectedVersion != row.Version {
			return fmt.Errorf("%w: %s is at version %d, not %d", ErrStaleVersion, id, row.Version, *expectedVersion)
		}
		qev, err := e.queue.Edit(id, text)
		if err != nil {
			return err
		}
		e.log.Enqueue(e.stamp(qev.Event(), c.Cause()))
		return nil
	})
}

// Unqueue drops a row the user cancelled and answers with the row that went.
func (e *Engine) Unqueue(c Command, id string) (agent.QueuedPrompt, error) {
	hash := receiptHash("Unqueue", id)
	return withSyncReceipt(e.receipts, c, hash, func() (agent.QueuedPrompt, error) {
		e.mu.Lock()
		defer e.mu.Unlock()
		if err := e.refusalLocked(); err != nil {
			return agent.QueuedPrompt{}, err
		}
		if !e.log.OutboxRoom() {
			return agent.QueuedPrompt{}, ErrUnavailable
		}
		qev, ok := e.queue.Remove(id)
		if !ok {
			return agent.QueuedPrompt{}, fmt.Errorf("%w: %s", ErrUnknownRow, id)
		}
		e.log.Enqueue(e.stamp(qev.Event(), c.Cause()))
		return qev.Prompt, nil
	})
}

// ClearQueue empties the queue, head first, and answers with how many rows went.
// Each is its own removal event, so a client can say where every follow-up went.
func (e *Engine) ClearQueue(c Command) (int, error) {
	hash := receiptHash("ClearQueue")
	return withSyncReceipt(e.receipts, c, hash, func() (int, error) {
		e.mu.Lock()
		defer e.mu.Unlock()
		if err := e.refusalLocked(); err != nil {
			return 0, err
		}
		if !e.log.OutboxRoom() {
			return 0, ErrUnavailable
		}
		qevs := e.queue.Clear()
		batch := make([]agent.Event, 0, len(qevs))
		for _, qev := range qevs {
			batch = append(batch, e.stamp(qev.Event(), c.Cause()))
		}
		e.log.Enqueue(batch...)
		return len(qevs), nil
	})
}
