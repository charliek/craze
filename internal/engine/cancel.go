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
// settles then, with its successor decided in the same step. A turn the session
// would start of its own — native's wake — is held off by the same promise
// through the session's admission fence, which is raised before the cancel is
// validated and stays up while the hold does (engine.go's admission fence).
//
// With no turn of craze's own the cancel is accepted only when there is
// something for it to do: an ask the agent is waiting on, or a turn the agent
// started itself (§3.7, 05's gate table). Otherwise it is ErrNotAccepting and
// nothing is written — which is what stops an Esc with nothing running from
// putting a session/cancel on the wire.
//
// The pending-ask count is read BEFORE e.mu is taken, because the engine's
// mutex and the registry's are never nested in either direction. The residual
// window is both ways and harmless, and it is exactly what PR 1 did:
//
//   - an ask opened between the read and the lock makes this refuse a cancel
//     that would now have been accepted — the user presses Esc a second time
//     and it is accepted, because by then the ask is in the count;
//   - an ask resolved in between makes this accept a cancel that has nothing
//     left to cancel, which writes one session/cancel the agent drops as
//     naming no turn — the no-turn path's long-standing behaviour.
func (e *Engine) Cancel(ctx context.Context, c Command, turn string) (CancelResult, error) {
	hash := receiptHash("Cancel", turn)
	return withBlockingReceipt(ctx, e.receipts, c, hash, func() (CancelResult, error) {
		asks := len(e.asks.Asks())
		id, err := e.holdCancel(turn, false, asks, c.Cause())
		if err != nil {
			return CancelResult{}, err
		}
		return e.cancelHeld(ctx, id, c.Cause(), false)
	})
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
	hash := receiptHash("Stop")
	return withBlockingReceiptErr(ctx, e.receipts, c, hash, func() error {
		// Always accepted, whatever is or is not running: a client that is
		// shutting down is not asking for a turn to end, it is saying nothing
		// more may start.
		id, err := e.holdCancel("", true, 0, c.Cause())
		if err != nil {
			return err
		}
		if err := e.log.Flush(ctx, nil); err != nil && !errors.Is(err, agent.ErrLogClosing) {
			e.releaseHold(id, c.Cause(), agent.CancelOutcome{}, nil, false)
			return err
		}
		_, err = e.cancelHeld(ctx, id, c.Cause(), false)
		return err
	})
}

// holdCancel validates a cancel and takes its hold, in one section. The fence
// raised in holdCancelLocked stays up for as long as the hold does (a cancel in
// flight is one of the things it stands for); a refused cancel lowers it again
// on the way out.
func (e *Engine) holdCancel(turn string, stop bool, asks int, cause string) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	defer e.syncFenceLocked()
	e.raiseFenceLocked()
	return e.holdCancelLocked(turn, stop, asks, cause)
}

// holdCancelLocked is holdCancel's section, for a caller that already holds
// e.mu and has to take the hold atomically with something else of its own —
// arming a send-now, which must not let anything be admitted between the arm
// and the cancel it asks for.
//
// asks is how many asks were pending when the caller looked, read outside e.mu
// (Cancel). It is consulted only for a cancel with no turn of craze's own, and
// never for a stop.
//
// The admission fence goes up first, before anything is validated: a turn the
// session could start of its own must not start between the validation and the
// session's cancel, which would land on it. The caller's section syncs it.
func (e *Engine) holdCancelLocked(turn string, stop bool, asks int, cause string) (string, error) {
	e.raiseFenceLocked()
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
	if !stop && id == "" && asks == 0 && !e.sess.ForeignTurn() {
		// Nothing of craze's own is running, the agent is holding no ask and is
		// running no turn of its own: there is nothing to cancel, and a cancel
		// written now could only reach whatever starts next.
		return "", ErrNotAccepting
	}
	if stop && !e.stopped {
		e.stopped = true
		var batch []agent.Event
		for _, qev := range e.queue.Clear() {
			batch = append(batch, e.stamp(qev.Event(), ""))
		}
		// An armed send goes with them: nothing may be admitted after a stop, so
		// the turn it was waiting for will settle into nothing at all.
		if ev, ok := e.disarmLocked(agent.SendNowStopped, cause, ""); ok {
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
// the events this path can author.
//
// own says the engine made this cancel for an arm of its own (cancelArmed) rather
// than for a client that called Cancel or Stop. It decides who reports a failure
// the engine owed no event for: the engine's own cancel has no caller to answer,
// so its failure is reported as an event or not at all.
func (e *Engine) cancelHeld(ctx context.Context, id, cause string, own bool) (res CancelResult, err error) {
	var out agent.CancelOutcome
	// The hold is given back however this ends. A hook or a session double that
	// panics, under a caller that recovers, would otherwise leave it standing
	// for good: nothing would settle and nothing would be admitted again.
	released := false
	defer func() {
		if !released {
			e.releaseHold(id, cause, out, errCancelAbandoned, own)
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
	settled, reported := e.releaseHold(id, cause, out, err, own)

	res = CancelResult{Turn: id, Outcome: CancelRequested, Reported: reported}
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
// session's cancel came to, nil when none was made, and own says whose cancel it
// was (cancelHeld). It reports whether the turn the hold was taken against is
// over, and whether a failure was published as an event.
//
// # One owner per failure, and one moment
//
// A cancel that failed is told once. Which teller depends on whether this path
// owed an event anyway:
//
//   - It disarmed a send-now. The delta that says so is going out regardless, so
//     the failure rides on it, in Detail — and the caller is told, through
//     CancelResult.Reported, that it has been published. A client that words a
//     failure from a delta then words this one exactly once, from the delta, and
//     in the delta's own order: what was lost, then why. Returning the error as
//     well and leaving the client to draw it would put the two in whichever order
//     bubbletea happened to deliver them, and the baseline's order was fixed.
//   - It disarmed nothing and the cancel was the ENGINE's, made for an arm the
//     client had already taken back. Nobody is waiting on that error, so it is
//     published as a delta that touches no section at all — a reason and a
//     failure, which is what a StateDelta with a Reason and no section means.
//   - It disarmed nothing and the cancel was a CLIENT's. The client has the error
//     in its hand; nothing is published, and nothing would add anything.
func (e *Engine) releaseHold(id, cause string, out agent.CancelOutcome, cancelErr error, own bool) (bool, bool) {
	var next []launch
	settled, reported := false, false
	func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		// The pass below may settle and claim a successor, all with the fence up;
		// the release lowers it only if that leaves the engine idle.
		defer e.syncFenceLocked()
		e.raiseFenceLocked()
		e.cancelsInFlight--
		if err := cancelErr; err != nil {
			switch armed := e.armed != nil && e.armed.turn == id; {
			case armed:
				// The cancel never reached the agent, so the turn a send was armed
				// against is still running and its settlement is not coming. The
				// send disarms here, in the section that releases the hold and
				// before the pass that release allows: a send left armed through
				// that pass could fire into a turn this cancel did not stop. The
				// text stays where it was, which is what the TUI's own
				// cancelFailedMsg has always done with it — and the failure goes
				// out on the same event, so both halves of what that message wrote
				// arrive together and in its order.
				if ev, ok := e.disarmLocked(agent.SendNowCancelFailed, cause, err.Error()); ok {
					e.log.Enqueue(ev)
					reported = true
				}
			case own:
				e.log.Enqueue(e.stamp(agent.Event{Type: agent.EventMeta, State: &agent.StateDelta{
					Reason: agent.SendNowCancelFailed, Detail: err.Error(),
				}}, cause))
				reported = true
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
	return settled, reported
}
