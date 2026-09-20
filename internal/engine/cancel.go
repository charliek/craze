package engine

import (
	"context"
	"errors"

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
func (e *Engine) Cancel(ctx context.Context, c Command, turn string) (CancelResult, error) {
	id, err := e.holdCancel(turn, false, c.Cause())
	if err != nil {
		return CancelResult{}, err
	}
	return e.cancelHeld(ctx, id, c.Cause())
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
// primary's reader. A log that is closing has nothing left to order, and the
// cancel goes ahead; a context that ended while the removals were still waiting
// is the caller giving up, and the cancel is not made — made then, it could
// not keep the order it exists to keep. The engine stays stopped and its queue
// stays cleared either way, and the hold is given back.
func (e *Engine) Stop(ctx context.Context, c Command) error {
	id, err := e.holdCancel("", true, c.Cause())
	if err != nil {
		return err
	}
	if err := e.log.Flush(ctx); err != nil && !errors.Is(err, agent.ErrLogClosing) {
		e.releaseHold(id, c.Cause(), agent.CancelOutcome{}, nil)
		return err
	}
	_, err = e.cancelHeld(ctx, id, c.Cause())
	return err
}

// holdCancel validates a cancel and takes its hold, in one section.
func (e *Engine) holdCancel(turn string, stop bool, cause string) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.holdCancelLocked(turn, stop, cause)
}

// holdCancelLocked is holdCancel's section, for a caller that already holds
// e.mu and has to take the hold atomically with something else of its own —
// arming a send-now, which must not let anything be admitted between the arm
// and the cancel it asks for.
func (e *Engine) holdCancelLocked(turn string, stop bool, cause string) (string, error) {
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
		// An armed send goes with them: nothing may be admitted after a stop, so
		// the turn it was waiting for will settle into nothing at all.
		if ev, ok := e.disarmLocked(agent.SendNowStopped, cause); ok {
			batch = append(batch, ev)
		}
		// A mandatory completion: the rows are gone whether or not the outbox
		// reports room.
		e.log.Enqueue(batch...)
	}
	e.cancelsInFlight++
	return id, nil
}

// cancelHeld makes the session's cancel for a hold already taken, releases the
// hold, and lets the driver pass. cause is the command the cancel came from, for
// the one event this path can author: the delta for a send-now the failure of
// this cancel disarms.
func (e *Engine) cancelHeld(ctx context.Context, id, cause string) (res CancelResult, err error) {
	var out agent.CancelOutcome
	// The hold is given back however this ends. A hook or a session double that
	// panics, under a caller that recovers, would otherwise leave it standing
	// for good: nothing would settle and nothing would be admitted again.
	released := false
	defer func() {
		if !released {
			e.releaseHold(id, cause, out, errCancelAbandoned)
		}
	}()
	if h := e.hooks; h != nil && h.beforeSessionCancel != nil {
		h.beforeSessionCancel(id)
	}
	out, err = e.sess.Cancel(ctx)
	if h := e.hooks; h != nil && h.afterSessionCancel != nil {
		h.afterSessionCancel(id)
	}
	released = true
	settled := e.releaseHold(id, cause, out, err)

	res = CancelResult{Turn: id, Outcome: CancelRequested}
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

// errCancelAbandoned stands for the failure of a cancel that never returned:
// its hold is released on the way out of a panic, and a send armed behind it is
// disarmed as it would be behind any cancel that failed.
var errCancelAbandoned = errors.New("engine: the cancel did not complete")

// releaseHold gives a cancel's hold back and lets the driver pass, which is
// where a turn that came back meanwhile settles. cancelErr is what the
// session's cancel came to, nil when none was made. It reports whether the turn
// the hold was taken against is over.
func (e *Engine) releaseHold(id, cause string, out agent.CancelOutcome, cancelErr error) bool {
	var next []launch
	settled := false
	func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		e.cancelsInFlight--
		if err := cancelErr; err != nil && e.armed != nil && e.armed.turn == id {
			// The cancel never reached the agent, so the turn a send was armed
			// against is still running and its settlement is not coming. The
			// send disarms here, in the section that releases the hold and
			// before the pass that release allows: a send left armed through
			// that pass could fire into a turn this cancel did not stop. The
			// text stays where it was, which is what the TUI's own
			// cancelFailedMsg has always done with it.
			if ev, ok := e.disarmLocked(agent.SendNowCancelFailed, cause); ok {
				e.log.Enqueue(ev)
			}
		}
		next = e.passLocked()
		// Settled is the engine's own fact, read after the pass the release
		// allowed: the turn the cancel was held against is no longer current.
		// What the session calls settled is about its wire, and a prompt that
		// withdrew has not necessarily returned yet.
		settled = id == "" && out.Settled || id != "" && (e.cur == nil || e.cur.id != id)
	}()
	e.run(next)
	return settled
}
