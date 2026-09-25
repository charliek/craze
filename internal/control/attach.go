package control

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/transcript"
)

// Attachments (plan 027 §3.4, §3.7).
//
// session.attach puts a connection on the session's stream: engine.Attach cuts
// a snapshot (or honours the client's cursor) and subscribes after it, and the
// connection's forwarder (forward.go) turns the subscription into event,
// synchronized, ready and reset notifications. A connection holds at most one
// attachment that is not yet closed (SQ14).
//
// # The lifecycle: pending → live → closing → closed
//
//   - PENDING from the moment an attach reserves the connection's one place
//     (reserve) until its reply is queued: a second attach meanwhile is
//     already_attached, and a reply barrier waits for it (awaitAttachment) —
//     its position will start at the reply's after, which the barrier cannot
//     yet know is past its seq.
//   - LIVE once the attach reply is QUEUED — before the forwarder starts, so
//     every event follows the reply that carries its snapshot (§3.7).
//   - CLOSING when its subscription ends (Records closes) or on a detach.
//   - CLOSED once the forwarder can queue nothing more AND the attachment's
//     terminal acknowledgement is queued to the writer (astra r3 15): for an
//     ended subscription, its final records and its reset; for a detach, the
//     detach's own reply. A pending attach that fails is closed at once.
//
// Each step a wire-visible acknowledgement announces is taken in the SAME
// conn.mu section that queues it (conn.enqueue; the reset's queueReset): the
// attach reply and LIVE, the detach reply or the reset and CLOSED (astra r8).
// So nothing the client can read is ever ahead of the step its next request
// depends on — a detach sent the instant the attach reply is read finds the
// attachment live, an attach sent the instant a reset or a detach reply is read
// finds the place free — and a barrier still sees CLOSED only once the
// acknowledgement is queued.
//
// Exactly one side queues the terminal acknowledgement: the forwarder claims
// it (attachment.ending) or a detach does (attachment.detach), each under
// conn.mu, and whichever claims second queues nothing of it. A new attach is
// already_attached until the old one is closed, which serialises the old
// stream's tail before the new reply (TestAReattachWaitsForTheOldReset). A
// reply barrier waits for a closing attachment exactly as for a live one, so a
// detach racing a command releases that command's barrier with its own reply
// (TestABarrierAwaitsAClosingAttachment); a detach answered before the command
// commits leaves it an attachment already closed, owed no events, and its
// reply is queued without waiting (TestADetachBeforeTheCommandOwesItNothing).
//
// # Readiness
//
// when: "ready" (the default) waits for Control.Ready — bounded by the
// connection's context (the attachment's, a child of it) and the server's own
// bound (readyWait, 10 minutes: then unavailable, reason not_ready) — and then
// answers ready: true, or refuses a start that failed (not_accepting, reason
// start_failed, data.cause its text) or a session that has closed
// (not_accepting). when: "now" attaches at once; it answers ready: false while
// the session starts, and one ready notification follows (watchReady). An
// attach of EITHER kind made once the start has failed is refused
// start_failed: an attach reply has no field that could say so, and a client
// learns that its session failed from the attach's answer or from a ready
// notification, never only from a session.state read.

// attState is an attachment's place in its lifecycle.
type attState uint8

const (
	attPending attState = iota
	attLive
	attClosing
	attClosed
)

// attachment is one session.attach on a connection.
type attachment struct {
	id  string // s-1, s-2, … per connection
	eng *engine.Engine
	// ctx bounds the attach's waits and the forwarder's pushes: a child of
	// the connection's, cancelled by a detach and by the engine's replacement
	// (conn.replace), and by the forwarder when it stops.
	ctx    context.Context
	cancel context.CancelFunc
	// stopped is closed once the forwarder has returned — or at once for an
	// attach that never went live (abandon).
	stopped chan struct{}
	// readyCh carries the one ready notification owed to an attachment made
	// before readiness, and the seq it waits behind (watchReady).
	readyCh chan readyNote
	// watched is closed once the ready watcher has returned — having handed
	// over its note or decided none is owed — for an attachment made before
	// readiness; nil for one that has no watcher. Set before the forwarder
	// starts.
	watched chan struct{}

	// sub, after and cutoff are set under conn.mu before the reply is queued,
	// and never written again: the forwarder reads them after it starts.
	sub    *agent.Subscription
	after  uint64
	cutoff uint64

	// Guarded by conn.mu.
	state attState
	// queued is the forwarder's position: the seq of the last event it has
	// queued to the writer, starting at the reply's after.
	queued uint64
	// detach and ending say who queues the terminal acknowledgement: a detach
	// (its reply), or the forwarder (the final records and reset).
	detach bool
	ending bool
	// changed is closed and replaced whenever queued or state moves: what a
	// reply barrier waits on.
	changed chan struct{}
}

// readyNote is a ready notification and the seq it is queued behind: the
// log's committed head once the start had run.
type readyNote struct {
	seq    uint64
	params protocol.ReadyParams
}

// changedLocked wakes every wait on the attachment; conn.mu is held.
func (a *attachment) changedLocked() {
	close(a.changed)
	a.changed = make(chan struct{})
}

// sessionAttach is session.attach (§3.3, §3.4): it reserves the connection's
// one attachment, waits for readiness when asked to, attaches, and queues its
// own reply — then, and only then, starts the forwarder.
func (c *conn) sessionAttach(b *bound, info protocol.MethodInfo, req *request) outcome {
	var p protocol.AttachParams
	check := func() *protocol.Error {
		switch p.When {
		case "", protocol.WhenReady, protocol.WhenNow:
		default:
			return badParams("params.when %q is neither ready nor now", p.When)
		}
		if bu := p.Budget; bu != nil && (bu.MaxItems < 0 || bu.MaxBytes < 0 || bu.SnapshotBytes < 0) {
			return badParams("params.budget's members must be positive")
		}
		return nil
	}
	if perr := c.params(b, info, req, &p, check); perr != nil {
		return refusal(perr)
	}
	if h := c.srv.hooks.beforeReserve; h != nil {
		h()
	}
	a, o := c.reserve(b.eng)
	if a == nil {
		return o
	}
	if h := c.srv.hooks.reserved; h != nil {
		h(a.id)
	}
	res, att, o := c.attach(a, b.eng, p)
	if att == nil {
		c.abandon(a, nil)
		return o
	}
	return c.answerAttach(a, att, res, req)
}

// reserve takes the connection's one attachment for a new attach, pending, or
// refuses already_attached while the last one is not yet closed (§3.7). Once
// the connection is replaced it refuses too, but with no reply at all (a nil
// attachment and a zero outcome): the attach is not run, so no new pending
// attachment is installed to hold the replaced connection open behind the
// replacement's own terminal line (plan 027 X16 9, X19, X22).
func (c *conn) reserve(eng *engine.Engine) (*attachment, outcome) {
	// The context is made outside c.mu, which is held across no other lock.
	ctx, cancel := context.WithCancel(c.ctx)
	a := &attachment{
		eng: eng, ctx: ctx, cancel: cancel,
		stopped: make(chan struct{}), readyCh: make(chan readyNote, 1), changed: make(chan struct{}),
	}
	c.mu.Lock()
	if c.replaced {
		c.mu.Unlock()
		cancel()
		return nil, outcome{}
	}
	if old := c.att; old != nil && old.state != attClosed {
		c.mu.Unlock()
		cancel()
		return nil, refusal(refused(protocol.CodeBadRequest, protocol.ReasonAlreadyAttached,
			"this connection's attachment %s is not yet closed: detach it, or wait for its reset", old.id))
	}
	c.nextSub++
	a.id = fmt.Sprintf("s-%d", c.nextSub)
	c.att = a
	c.mu.Unlock()
	return a, outcome{}
}

// attach is the attach's work up to its reply (§3.4): the wait for readiness,
// the refusals readiness decides, and engine.Attach on the attachment's
// context — the connection's, so a closed connection abandons an attach still
// waiting for the log's boundary. It returns the reply and the engine's
// attachment, or the outcome that answers the request instead (att nil).
func (c *conn) attach(a *attachment, eng *engine.Engine, p protocol.AttachParams) (protocol.AttachResult, *engine.Attachment, outcome) {
	var none protocol.AttachResult
	if p.When != protocol.WhenNow {
		if o, ok := c.awaitReady(a, eng); !ok {
			return none, nil, o
		}
	}
	// The one reading of readiness the reply's ready and its document's
	// catalogs share: an attach that read the session not yet ready is owed a
	// ready notification, whenever the start then completes.
	info, st, ready := c.srv.sessionInfoReady(eng)
	if ready {
		switch {
		case st.StartFailed:
			e := refused(protocol.CodeNotAccepting, protocol.ReasonStartFailed, "the session failed to start: %s", st.Err)
			e.Data.Cause = st.Err
			return none, nil, refusal(e)
		case st.Activity == engine.ActivityClosing:
			// Ready is closed by a close too, so this never waited.
			return none, nil, refusal(refused(protocol.CodeNotAccepting, protocol.ReasonNotAccepting, "the session has closed"))
		}
	}
	opts := engine.AttachOptions{}
	if bu := p.Budget; bu != nil {
		// A client may lower its subscription's budget, and raise it up to
		// the host's MaxBudget; an absent member is the log's default. The
		// snapshot's budget is capped at SnapshotBytesMax (§3.2).
		if bu.MaxItems > 0 {
			opts.MaxItems = min(bu.MaxItems, c.srv.maxBudget.MaxItems)
		}
		if bu.MaxBytes > 0 {
			opts.MaxBytes = min(bu.MaxBytes, c.srv.maxBudget.MaxBytes)
		}
		opts.SnapshotBytes = min(bu.SnapshotBytes, protocol.SnapshotBytesMax)
	}
	if p.Cursor != nil {
		opts.Cursor = &agent.Cursor{Incarnation: p.Cursor.Incarnation, Seq: p.Cursor.Seq}
	}
	att, err := eng.Attach(a.ctx, opts)
	switch {
	case err == nil:
	case a.ctx.Err() != nil:
		// The connection closed, or its engine was replaced, while the attach
		// waited: no reply is owed.
		return none, nil, outcome{}
	case errors.Is(err, agent.ErrClosed):
		return none, nil, refusal(refused(protocol.CodeNotAccepting, protocol.ReasonNotAccepting, "the session has closed"))
	case errors.Is(err, transcript.ErrSnapshotTooLarge):
		return none, nil, refusal(refused(protocol.CodeFailed, protocol.ReasonSnapshotTooLarge, "%v", err))
	default:
		// ErrAttachRaced is unavailable, reason attach_raced: ask again.
		return none, nil, refusal(engineError(err))
	}
	res := protocol.AttachResult{Subscription: a.id, Session: info, Ready: ready, Reset: protocol.CursorReason(att.Reset)}
	if att.Snapshot == nil {
		// The cursor was honoured: the stream continues from it.
		res.After = *p.Cursor
		return res, att, outcome{}
	}
	raw, err := transcript.EncodeSnapshot(att.Snapshot)
	if err != nil {
		att.Sub.Close()
		return none, nil, refusal(failed(err))
	}
	res.Snapshot = raw
	res.After = protocol.Cursor{Incarnation: att.Snapshot.Incarnation, Seq: att.Snapshot.Seq}
	return res, att, outcome{}
}

// awaitReady is when: "ready"'s wait for the session's start (§3.4), bounded
// by the attachment's context and the server's own bound. false with the
// outcome that answers the attach instead: not_ready past the bound, and no
// reply at all once the connection has gone (or its engine was replaced).
func (c *conn) awaitReady(a *attachment, eng *engine.Engine) (outcome, bool) {
	select {
	case <-eng.Ready():
		return outcome{}, true
	default:
	}
	bound := time.NewTimer(c.srv.readyWait)
	defer bound.Stop()
	select {
	case <-eng.Ready():
		return outcome{}, true
	case <-a.ctx.Done():
		return outcome{}, false
	case <-bound.C:
		return refusal(refused(protocol.CodeUnavailable, protocol.ReasonNotReady,
			"the session did not come up within %s; attach again", c.srv.readyWait)), false
	}
}

// answerAttach queues the attach reply and makes the attachment live in one
// conn.mu section (conn.enqueue), and then starts its forwarder (and, for an
// attach made before readiness, the watcher that owes it its ready
// notification). A reply that cannot go out whole — over the outbound limit —
// closes the subscription and answers the error instead; one whose connection
// has gone, or whose engine has been replaced, goes nowhere.
func (c *conn) answerAttach(a *attachment, att *engine.Attachment, res protocol.AttachResult, req *request) outcome {
	raw, err := rawJSON(res)
	if err != nil {
		c.abandon(a, att.Sub)
		return refusal(failed(err))
	}
	line, whole := c.responseLine(protocol.Response{JSONRPC: protocol.JSONRPCVersion, ID: req.id, Result: raw})
	if !whole {
		c.abandon(a, att.Sub)
		if line == nil || c.out.push(c.ctx, line, c.release) != nil {
			return outcome{}
		}
		return replied()
	}
	c.mu.Lock()
	a.sub, a.after, a.cutoff = att.Sub, res.After.Seq, att.Sub.Cutoff()
	c.mu.Unlock()
	if !res.Ready {
		a.watched = make(chan struct{})
	}
	// close reads a.sub after it sets closing, and replace reads it after it
	// sets replaced: the admit below reads both under c.mu after a.sub is set,
	// so one side or the other closes the subscription. The reply queued,
	// every notification of the attachment follows it; and it is live by then,
	// so a detach sent the instant the reply is read finds it so.
	err = c.enqueue(a.ctx, nil, line, c.release, func() error {
		if c.closing.Load() || c.replaced || a.ctx.Err() != nil {
			return errGone
		}
		return nil
	}, func() {
		a.state, a.queued = attLive, a.after
		a.changedLocked()
	}, false)
	if err != nil {
		c.abandon(a, att.Sub)
		return outcome{}
	}
	if h := c.srv.hooks.ackQueued; h != nil {
		h(a.id, protocol.MethodSessionAttach)
	}
	if w := a.watched; w != nil && !c.srv.goTransport(func() { defer close(w); c.watchReady(a) }) {
		close(w)
	}
	if !c.srv.goTransport(func() { c.forward(a) }) {
		// The server has closed, and with it the connection.
		c.abandon(a, att.Sub)
		return replied()
	}
	return replied()
}

// abandon closes an attachment no forwarder ever ran for — an attach that was
// refused, dropped, or could not start one — and its subscription, if it got
// that far.
func (c *conn) abandon(a *attachment, sub *agent.Subscription) {
	a.cancel()
	if sub != nil {
		sub.Close()
	}
	c.mu.Lock()
	a.state = attClosed
	a.changedLocked()
	c.mu.Unlock()
	close(a.stopped)
	c.settle()
}

// watchReady owes an attachment made before readiness its one ready
// notification (§3.3, §3.4; astra 15): once Ready closes it takes the log's
// committed head S (SyncSeq) — off the forwarder's goroutine, which must not
// block on the log — and hands the forwarder the notification and S; the
// forwarder queues it once its position has reached S, so it follows every
// record committed before the start completed (an install delta still in the
// outbox included: Ready is not an outbox barrier) — among a closed log's final
// records too, where the forwarder waits for this watcher's decision before
// its tail (forward.go, awaitReadyNote) — or drops it if the subscription's
// reset comes before S is reached. session is the FINAL info document, read
// after Ready.
//
// A session that closed rather than started (Ready closes on Close too) owes
// no ready: its attachment ends reset{session_closed} (forward.go), and a
// log already closing has no seq to order one by either. A start that failed
// is owed one, startFailed and its err.
func (c *conn) watchReady(a *attachment) {
	select {
	case <-a.eng.Ready():
	case <-a.ctx.Done():
		return
	case <-a.stopped:
		return
	}
	seq, err := a.eng.SyncSeq(a.ctx)
	if err != nil {
		return
	}
	info, st := c.srv.sessionInfo(a.eng)
	if st.Activity == engine.ActivityClosing && !st.StartFailed {
		return
	}
	p := protocol.ReadyParams{Subscription: a.id, Session: info, StartFailed: st.StartFailed}
	if st.StartFailed {
		p.Err = st.Err
	}
	if h := c.srv.hooks.readyDecided; h != nil {
		h(a.id, seq)
	}
	a.readyCh <- readyNote{seq: seq, params: p}
	if h := c.srv.hooks.readyOwed; h != nil {
		h(a.id, seq)
	}
}

// sessionDetach is session.detach (§3.3): it ends the connection's attachment
// by closing its subscription, waits for the forwarder to stop, and queues the
// reply {} — the attachment's terminal acknowledgement, after which nothing of
// it follows — and only then is the attachment closed. A subscription id that
// names no attachment of this connection that is live or closing — unknown,
// stale, or not yet answered — is bad_request. A detach of an attachment
// whose own end is already on its way (its forwarder's reset, or another
// detach) is answered once that end is queued: the subscription is over
// either way.
func (c *conn) sessionDetach(b *bound, info protocol.MethodInfo, req *request) outcome {
	var p protocol.DetachParams
	if perr := c.params(b, info, req, &p, nil); perr != nil {
		return refusal(perr)
	}
	c.mu.Lock()
	a := c.att
	if a == nil || a.id != p.Subscription || a.state == attPending || a.state == attClosed {
		c.mu.Unlock()
		return refusal(refused(protocol.CodeBadRequest, protocol.ReasonBadRequest,
			"no attachment %q is live on this connection", p.Subscription))
	}
	claimed := !a.ending && !a.detach
	if claimed {
		a.detach = true
		if a.state == attLive {
			a.state = attClosing
			a.changedLocked()
		}
	}
	c.mu.Unlock()
	if !claimed {
		if !c.awaitClosed(a) {
			return outcome{}
		}
		return answer(protocol.Empty{})
	}
	// The forwarder stops: a push it is blocked in is abandoned (cancel), and
	// Records closes (Close). A client that detached asked for nothing more,
	// so what the subscription had not delivered is dropped with it.
	a.cancel()
	a.sub.Close()
	if h := c.srv.hooks.detaching; h != nil {
		h(a.id)
	}
	select {
	case <-a.stopped:
	case <-c.ctx.Done():
		c.markClosed(a)
		return outcome{}
	}
	empty, err := rawJSON(protocol.Empty{})
	if err != nil {
		c.markClosed(a)
		return refusal(failed(err))
	}
	line, _ := c.responseLine(protocol.Response{JSONRPC: protocol.JSONRPCVersion, ID: req.id, Result: empty})
	// The reply is queued and the attachment closed in one conn.mu section
	// (conn.enqueue): an attach sent the instant the reply is read finds the
	// place free. The attachment's terminal acknowledgement, it counts as a
	// final reset does (unwritten) until it is on the socket (detachWritten):
	// an engine replaced while this detach held the attachment's end — its
	// forwarder, stopped by the detach, queues no reset — closes the connection
	// only once this reply is written (astra r10 8). On a connection already
	// replaced this reply is the connection's own terminal line, so the same
	// section seals the outbox (sealIfReplaced): no later reply — a duplicate
	// detach's, any handler's — queues behind it (plan 027 X22, r12 finding 3).
	if line == nil || c.enqueue(c.ctx, nil, line, c.detachWritten, nil, func() {
		a.state = attClosed
		a.changedLocked()
		c.unwritten++
	}, true) != nil {
		c.markClosed(a)
		return outcome{}
	}
	if h := c.srv.hooks.ackQueued; h != nil {
		h(a.id, protocol.MethodSessionDetach)
	}
	c.settle()
	return replied()
}

// detachWritten is a detach's reply on the socket, or a write that failed: its
// request's slot is given back, and the connection may close now if nothing
// else is left of it (release settles).
func (c *conn) detachWritten() {
	c.mu.Lock()
	c.unwritten--
	c.mu.Unlock()
	c.release()
}

// awaitClosed waits until a is closed; false if the connection closes first.
func (c *conn) awaitClosed(a *attachment) bool {
	c.mu.Lock()
	for a.state != attClosed {
		changed := a.changed
		c.mu.Unlock()
		select {
		case <-changed:
		case <-c.ctx.Done():
			return false
		}
		c.mu.Lock()
	}
	c.mu.Unlock()
	return true
}

// markClosed closes a: its terminal acknowledgement is queued, or the
// connection is gone.
func (c *conn) markClosed(a *attachment) {
	c.mu.Lock()
	a.state = attClosed
	a.changedLocked()
	c.mu.Unlock()
	c.settle()
}
