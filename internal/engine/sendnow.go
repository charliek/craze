package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/charliek/craze/internal/agent"
)

// armedCancelTimeout bounds the cancel an armed send-now asks for. It is the
// bound the TUI's own cancel has always had (app.go's cancelTurn): a cancel
// that has not reached the agent in two seconds is not going to, and the send
// waiting on it disarms rather than wait for an ending that is not coming.
const armedCancelTimeout = 2 * time.Second

// armedSend is a send-now waiting for the turn it cancelled to settle. There is
// only ever one: a second is refused with ErrAlreadyPending rather than queued
// behind the first, because two turns cannot both be "the one that replaces
// this".
//
// Nothing it names is consumed while it waits. A row is held by id and stays in
// the queue until the send actually fires, so a send that never fires loses
// nothing and one that does cannot be drained a second time; a draft is held as
// text and stays in the client's composer, which is where the user can still
// see it. That is why every disarm below can say "the text is where it was" and
// mean it.
type armedSend struct {
	text string
	// from is the queued row the send is, "" for text the client is holding.
	from string
	// turn is the turn it was armed against, whose settlement fires it. A turn
	// it was not armed against takes it with it instead.
	turn string
	// cause is the command that armed it, which every event about it names.
	cause string
}

// state is the send as a StateDelta's section reports it.
func (a *armedSend) state() *agent.SendNowState {
	return &agent.SendNowState{Armed: true, Text: a.text, FromRow: a.from, Turn: a.turn}
}

// ArmedSend is State's view of the armed send-now: what will be sent, where it
// will come from, and the turn whose settlement fires it.
type ArmedSend struct {
	Text string
	// FromRow is the queued row the send will re-take, "" for text the client
	// is holding itself.
	FromRow string
	Turn    string
	// Cause is the command that armed it, as Event.Cause spells it.
	Cause string
}

// armLocked arms a send-now against the running turn and takes the hold for the
// cancel that makes room for it. It mutates nothing else: until the send fires
// there is still a chance it never will, and text that exists in exactly one
// place cannot be lost by a path that forgot to put it back.
//
// The hold is the same one Cancel takes, reserved in this section, so between
// the arm and the cancel reaching the session nothing can be admitted by any
// path — and the cancel therefore lands on the turn the send was armed against
// or on none, never on one that started behind it (§3.7).
//
// It returns the turn to cancel, for the caller to cancel with the lock
// released.
func (e *Engine) armLocked(c Command, text, fromRow string) (SubmitResult, string, error) {
	t := e.cur
	if t == nil {
		// Nothing is running to be replaced, and nothing may start: a cancel is
		// on its way to the session and the hold it took shuts every admission
		// path until it returns.
		return SubmitResult{}, "", ErrNotAccepting
	}
	if t.retry {
		// The turn is waiting out a turn the agent is running of its own: it
		// has nothing on the wire for a cancel to stop, so there is nothing for
		// a send to take the place of.
		return SubmitResult{}, "", agent.ErrForeignTurn
	}
	if fromRow != "" {
		// Read, not taken. A row that is not there is a stale view of the
		// queue, and arming against it would be arming against nothing.
		if _, ok := e.rowLocked(fromRow); !ok {
			return SubmitResult{}, "", fmt.Errorf("%w: %s", ErrUnknownRow, fromRow)
		}
	}
	// The turn to cancel is named, so the no-turn gate above cannot fire and
	// the ask count is never consulted: it is not read here, which is what keeps
	// e.mu and the registry's mutex from ever being nested.
	id, err := e.holdCancelLocked(t.id, false, 0, c.Cause())
	if err != nil {
		return SubmitResult{}, "", err
	}
	// Counted here, under e.mu, and not where the goroutine starts, for the
	// reason launchLocked gives: Close sets closed under this same lock before
	// it waits, so the cancel is either counted before that wait begins or
	// never made.
	e.wg.Add(1)
	a := &armedSend{text: text, from: fromRow, turn: t.id, cause: c.Cause()}
	e.armed = a
	e.log.Enqueue(e.stamp(agent.Event{Type: agent.EventMeta, State: &agent.StateDelta{SendNow: a.state()}}, a.cause))
	return SubmitResult{Armed: true}, id, nil
}

// cancelArmed makes the cancel an armed send-now asked for, on a goroutine of
// the engine's own — Submit waits on nothing — with the hold already taken by
// the section that armed. No result is returned to the caller: the client has
// its Armed already, and what became of the send reaches it as the started that
// fires it or the delta that disarms it. A cancel that failed disarms inside
// cancelHeld, in the section that releases the hold, and the delta carries the
// failure's text — this is the one cancel a client did not make itself, so that
// delta is the only place its error can reach one.
func (e *Engine) cancelArmed(turn, cause string) {
	defer e.wg.Done()
	ctx, cancel := context.WithTimeout(context.Background(), armedCancelTimeout)
	defer cancel()
	// own: there is no caller to answer, so cancelHeld reports a failure as an
	// event instead — and does so whether or not the arm is still standing.
	_, _ = e.cancelHeld(ctx, turn, cause, true)
}

// Disarm takes back an armed send-now. The text is wherever it was — a client's
// composer for a draft, the queue for a row — so nothing is restored and
// nothing is lost, and the turn that was cancelled to make room for it simply
// settles into whatever was queued behind it.
//
// Nothing to disarm is ErrNotAccepting, as a cancel with nothing to cancel is:
// the answer to "undo what I asked for" has to distinguish "done" from "there
// was nothing there". It waits on nothing.
func (e *Engine) Disarm(c Command) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.refusalLocked(); err != nil {
		return err
	}
	if !e.log.OutboxRoom() {
		return ErrUnavailable
	}
	if e.armed == nil {
		return ErrNotAccepting
	}
	ev, _ := e.disarmLocked(agent.SendNowWithdrawn, c.Cause(), "")
	e.log.Enqueue(ev)
	return nil
}

// disarmLocked clears the armed send-now, if there is one, and returns the
// delta that says so: the send-now section as it now stands — nothing armed —
// with the reason it went. Every path that can lose an armed send goes through
// here, so each has its own reason and none can clear the field silently.
//
// cause is the command that caused the disarm where one did — Disarm, a failed
// Cancel, Stop — and "" where none did, as at a settlement or at Close; the
// delta then names the command that armed the send, which is the only one in
// the picture. detail is the failure behind the reason where the reason has one
// (a cancel that failed), as text and never an error value, because an enqueued
// event is encoded without anything being called on it. The event is returned
// rather than enqueued so that a caller composing a batch can place it where
// that batch wants it: a settlement puts it between the turn's ending and its
// successor's started.
func (e *Engine) disarmLocked(reason, cause, detail string) (agent.Event, bool) {
	a := e.armed
	if a == nil {
		return agent.Event{}, false
	}
	e.armed = nil
	if cause == "" {
		cause = a.cause
	}
	return e.stamp(agent.Event{Type: agent.EventMeta, State: &agent.StateDelta{
		SendNow: &agent.SendNowState{}, Reason: reason, Detail: detail,
	}}, cause), true
}
