package engine

import (
	"context"

	"github.com/charliek/craze/internal/agent"
)

// Cancel cancels the current turn, and answers with what came of it. turn
// names the turn the client displayed when it asked, or "" for whichever is
// current: a named turn that is no longer current is refused with
// ErrStaleTurn, which is what stops a cancel delayed across a queue transition
// from stopping the turn the user did not mean.
//
// The hold is what stops it landing there by another route. In the section
// that validates the cancel, cancelsInFlight is raised, and while it stands no
// turn is started by any path — Submit, the drain, a settlement's successor, a
// claim taken again — so the Session.Cancel made with the lock released lands
// on the turn it was validated for or on none: never ahead of its own prompt,
// because a reserved turn is always a claimed one (plan 017's rule), and never
// on a later prompt, because there cannot be one. When it returns the hold is
// released and the driver passes again: a turn that came back meanwhile
// settles then, with its successor decided in the same step.
//
// With no turn of craze's own the cancel is still accepted, and written at
// once: the agent may be running a turn it started itself, or holding an ask
// that arrived between turns, and the engine cannot yet see either.
func (e *Engine) Cancel(ctx context.Context, _ Command, turn string) (CancelResult, error) {
	id, err := e.holdCancel(turn, false)
	if err != nil {
		return CancelResult{}, err
	}
	return e.cancelHeld(ctx, id)
}

// Stop refuses every later admission, clears the queue, and cancels what is
// running, in that order, so nothing starts behind the cancel. The queue's
// removals are events like any other, which is what lets a client that is
// shutting down say where its follow-ups went.
//
// Between the two it waits for the outbox. The removals are the engine's and
// trail the state they describe; the cancelled turn's ending is the session's
// own and is published directly, so without the barrier it could overtake
// them, and `craze prompt --json` has always printed a signal's removed lines
// ahead of the turn's done. Stop blocks already, and is never called from the
// primary's reader. A log that is closing has nothing left to order: the
// cancel goes ahead.
func (e *Engine) Stop(ctx context.Context, _ Command) error {
	id, err := e.holdCancel("", true)
	if err != nil {
		return err
	}
	_ = e.log.Flush(ctx)
	_, err = e.cancelHeld(ctx, id)
	return err
}

// holdCancel validates a cancel and takes its hold, in one section.
func (e *Engine) holdCancel(turn string, stop bool) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return "", ErrNotAccepting
	}
	id := ""
	if e.cur != nil {
		id = e.cur.id
	}
	if turn != "" && turn != id {
		return "", ErrStaleTurn
	}
	if stop && !e.stopped {
		e.stopped = true
		var batch []agent.Event
		for _, qev := range e.queue.Clear() {
			batch = append(batch, e.stamp(qev.Event(), ""))
		}
		// A mandatory completion: the rows are gone whether or not the outbox
		// reports room.
		e.log.Enqueue(batch...)
	}
	e.cancelsInFlight++
	return id, nil
}

// cancelHeld makes the session's cancel for a hold already taken, releases the
// hold, and lets the driver pass.
func (e *Engine) cancelHeld(ctx context.Context, id string) (CancelResult, error) {
	if h := e.hooks; h != nil && h.beforeSessionCancel != nil {
		h.beforeSessionCancel(id)
	}
	out, err := e.sess.Cancel(ctx)
	if h := e.hooks; h != nil && h.afterSessionCancel != nil {
		h.afterSessionCancel(id)
	}
	var next []launch
	settled := false
	func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		e.cancelsInFlight--
		next = e.passLocked()
		// Settled is the engine's own fact, read after the pass the release
		// allowed: the turn the cancel was held against is no longer current.
		// What the session calls settled is about its wire, and a prompt that
		// withdrew has not necessarily returned yet.
		settled = id == "" && out.Settled || id != "" && (e.cur == nil || e.cur.id != id)
	}()
	e.run(next)

	res := CancelResult{Turn: id, Outcome: CancelRequested}
	switch {
	case err != nil:
		// The call gave up: its context ended, or the write failed. Whether a
		// cancel reached the agent is then not something this call can say,
		// and a timeout past the write is not a cancel that failed.
		res.Outcome = CancelUnknown
	case settled:
		res.Outcome = CancelSettled
	}
	return res, err
}
