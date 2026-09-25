package control

import (
	"encoding/json"
	"errors"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/protocol"
)

// The forwarder (plan 027 §3.7 role 3): one goroutine per live attachment,
// started once the attach reply is queued. It reads the subscription's
// Records and queues, to the connection's one FIFO writer:
//
//   - event{subscription, seq, event} per record, event being Record.Body
//     VERBATIM (raw JSON, never re-encoded). Its position — the seq of the last
//     event it has queued — starts at the reply's after, and a reply barrier
//     reads it (awaitAttachment): it moves in the same conn.mu section that
//     queues the event (conn.enqueue);
//   - synchronized{subscription, seq} once its position has reached the
//     subscription's Cutoff — at once, before any event, when the cutoff is
//     the reply's after (nothing to replay) (§3.4);
//   - ready, when one is owed, once its position has reached the seq the
//     ready watcher read after the start (attach.go, watchReady);
//   - and, when the subscription ends, its terminal acknowledgement: the final
//     records, if any, then reset{subscription, reason} (terminal).
//
// It waits for room in the writer queue (the outbox's budget), so a stuck
// socket backs the subscription up in the log, and the log drops it
// slow_consumer without ever waiting: a publish only offers a record to a
// subscription's buffer (agent.EventLog). A wait for room also ends when the
// subscription does (Subscription.Done, astra r8 6), and the end wins over
// room made by then (outbox.await, astra r10 4): a full queue never hides the
// end from the forwarder, which then drains Records to its close and reads why
// it ended. The final reset uses the queue's 1 KiB reserve (queueReset),
// so it is always queued at once.
//
// How a subscription ended decides its reset (§3.4's table):
//
//   - a record with Omitted set (one no client can fold): reset{omitted}, and
//     the subscription is closed — the client re-attaches with no cursor;
//   - ErrSlowConsumer: reset{slow_consumer}, AT ONCE — a record the forwarder
//     was blocked on, and any it drains after, is not sent (the client's
//     re-attach with its cursor replays them);
//   - an ErrCursorUnresolvable from the journal leg of a cursor replay (the
//     asynchronous one; the synchronous refusals are the attach reply's reset):
//     reset{replay_failed}, at once;
//   - ErrClosed with Rest answering — the log closed: NOTHING IS ABANDONED. The
//     record the forwarder was blocked on, any it drained after, then Rest's
//     records — the session's closing records — go out as events, waiting for
//     room, with synchronized and an owed ready where the position reaches
//     them, then reset{session_closed}; and the connection ends (conn.end),
//     closing once all of it is written. ErrRestUnavailable (a journal leg
//     outstanding at the close): the same without Rest — never a suffix
//     posing as the tail;
//   - ErrClosed with ErrNoRest: the subscription was closed by this side — a
//     detach, whose reply is the terminal acknowledgement; the engine's
//     replacement, whose reset is session_replaced; or the connection's close,
//     after which nothing is owed;
//   - anything else — a hole the log refused to deliver, or a closing tail that
//     is not whole — is a bug: it is logged, and the client is sent the reset
//     that makes it start again with nothing it folded trusted (replay_failed;
//     session_closed at a log's close). Never a partial tail.
//
// A connection whose engine has been replaced queues nothing more of the
// forwarder's but reset{session_replaced}, WHATEVER reset the forwarder had
// chosen: every push is refused once replaced is set (forwarder.push), and the
// reset's reason is decided in the conn.mu section that queues it (queueReset).
// That reset is the connection's terminal line (plan 027 X25): what the
// forwarder had queued and the writer had not yet taken was dropped by the
// replacement (conn.replace).

// forwarded is what became of one line the forwarder tried to queue.
type forwarded uint8

const (
	forwardedOK forwarded = iota
	// forwardedStopped: nothing more can be queued — the connection has gone,
	// its engine was replaced, or the attachment's context was cancelled (a
	// detach).
	forwardedStopped
	// forwardedEnded: the subscription ended while the push waited for room
	// (its Done); the line was not queued.
	forwardedEnded
	// forwardedUnfoldable: the record is one no client can fold — Omitted — or
	// one that could not be put on a line.
	forwardedUnfoldable
)

// noWait is a stop channel already closed: a push handed it takes room that
// is there and never waits for more (outbox.await).
var noWait = func() chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}()

// forwarder is one attachment's forwarder: the state its goroutine alone
// keeps.
type forwarder struct {
	c *conn
	a *attachment
	// pos is the seq of the last event queued (a.queued's own copy: only this
	// goroutine moves it).
	pos uint64
	// synced says synchronized has been queued.
	synced bool
	// ready is the ready notification the watcher handed over, not yet
	// queued.
	ready *readyNote
}

// forward is the attachment's forwarder.
func (c *conn) forward(a *attachment) {
	defer close(a.stopped)
	// The ready watcher's wait ends with the forwarder.
	defer a.cancel()
	f := &forwarder{c: c, a: a, pos: a.after}
	done, records := a.sub.Done(), a.sub.Records()
	for {
		// What the position owes first: synchronized at once when there is
		// nothing to replay, and after each record or ready note.
		switch f.catchUp(done) {
		case forwardedStopped:
			f.halt()
			return
		case forwardedEnded:
			f.ended(nil)
			return
		}
		select {
		case rec, open := <-records:
			if !open {
				f.ended(nil)
				return
			}
			switch f.record(rec, done) {
			case forwardedStopped:
				f.halt()
				return
			case forwardedEnded:
				f.ended([]agent.Record{rec})
				return
			case forwardedUnfoldable:
				a.sub.Close()
				f.terminal(protocol.ResetOmitted, nil, false)
				return
			}
		case r := <-a.readyCh:
			f.ready = &r
		}
	}
}

// record queues rec as an event notification and moves the position a
// barrier reads to it, in one conn.mu section (push). stop ends a wait for
// room: the subscription's Done while it runs, nil for the final records of a
// closed log, which wait for room.
func (f *forwarder) record(rec agent.Record, stop <-chan struct{}) forwarded {
	c, a := f.c, f.a
	if rec.Omitted != nil {
		return forwardedUnfoldable
	}
	if h := c.srv.hooks.beforeForward; h != nil {
		h(a.id, rec.Seq)
	}
	line, err := notificationLine(protocol.NotifyEvent,
		protocol.EventParams{Subscription: a.id, Seq: rec.Seq, Event: json.RawMessage(rec.Body)})
	if err != nil {
		// A body the codec wrote is always JSON; one that is not is a bug, and
		// the client is told as it is of an omitted record: nothing past it
		// can be folded.
		c.srv.logf("control: conn %d %s: event %d cannot be put on a line: %v", c.id, a.id, rec.Seq, err)
		return forwardedUnfoldable
	}
	r := f.push(line, stop, func() {
		a.queued = rec.Seq
		a.changedLocked()
	})
	if r == forwardedOK {
		f.pos = rec.Seq
	}
	return r
}

// catchUp queues what the position now owes: synchronized once it has reached
// the cutoff, then the ready note once it has reached the note's seq. stop is
// as record's.
func (f *forwarder) catchUp(stop <-chan struct{}) forwarded {
	a := f.a
	if !f.synced && f.pos >= a.cutoff {
		if r := f.notify(protocol.NotifySynchronized, protocol.SynchronizedParams{Subscription: a.id, Seq: a.cutoff}, stop); r != forwardedOK {
			return r
		}
		f.synced = true
	}
	if f.ready != nil && f.pos >= f.ready.seq {
		if r := f.notify(protocol.NotifyReady, f.ready.params, stop); r != forwardedOK {
			return r
		}
		f.ready = nil
	}
	return forwardedOK
}

// notify queues one notification of the attachment's.
func (f *forwarder) notify(method string, params any, stop <-chan struct{}) forwarded {
	line, err := notificationLine(method, params)
	if err != nil {
		// Only the server's own documents are in these; one that cannot be
		// encoded is a bug, logged, and the notification is left out.
		f.c.srv.logf("control: conn %d %s: %s cannot be put on a line: %v", f.c.id, f.a.id, method, err)
		return forwardedOK
	}
	return f.push(line, stop, nil)
}

// push queues one line of the attachment's with commit (conn.enqueue). It is
// refused once the attachment's context has ended (a detach, the connection's
// close) or the connection's engine has been replaced — read under conn.mu,
// where replace sets it, so nothing of the forwarder's is queued after the
// replacement but its reset — and a wait for room ends when stop closes.
func (f *forwarder) push(line []byte, stop <-chan struct{}, commit func()) forwarded {
	c, a := f.c, f.a
	err := c.enqueue(a.ctx, stop, line, nil, func() error {
		if c.replaced || a.ctx.Err() != nil {
			return errGone
		}
		return nil
	}, commit, ordinaryLine)
	switch {
	case err == nil:
		return forwardedOK
	case errors.Is(err, errStopped):
		return forwardedEnded
	}
	return forwardedStopped
}

// halt is a forwarder that can queue nothing more before its subscription
// ended: a detach (whose reply is the terminal acknowledgement), the engine's
// replacement (reset{session_replaced}), or the connection's close (nothing is
// owed).
func (f *forwarder) halt() {
	c, a := f.c, f.a
	c.mu.Lock()
	detach, replaced := a.detach, c.replaced
	c.mu.Unlock()
	switch {
	case detach:
	case c.ctx.Err() == nil && replaced:
		a.sub.Close()
		f.terminal(protocol.ResetSessionReplaced, nil, false)
	default:
		a.sub.Close()
		c.markClosed(a)
	}
}

// ended is a's subscription over — Records closed, or its Done seen while a
// push waited for room, when pending holds the record that push was for — and
// the terminal acknowledgement its end is owed (the package's table above).
// Records is drained to its close first: a record the owner handed over
// meanwhile is kept, in order, after pending, and Err and Rest are final once
// it has closed.
func (f *forwarder) ended(pending []agent.Record) {
	c, a := f.c, f.a
	for rec := range a.sub.Records() {
		pending = append(pending, rec)
	}
	err := a.sub.Err()
	c.mu.Lock()
	detach, replaced := a.detach, c.replaced
	if a.state == attLive {
		a.state = attClosing
		a.changedLocked()
	}
	c.mu.Unlock()
	switch {
	case detach:
		return
	case c.ctx.Err() != nil:
		c.markClosed(a)
		return
	case replaced:
		f.terminal(protocol.ResetSessionReplaced, nil, false)
		return
	}
	if f.ready == nil {
		select {
		case r := <-a.readyCh:
			f.ready = &r
		default:
		}
	}
	var unresolvable agent.ErrCursorUnresolvable
	switch {
	case errors.Is(err, agent.ErrSlowConsumer):
		f.terminal(protocol.ResetSlowConsumer, nil, false)
	case errors.As(err, &unresolvable):
		f.terminal(protocol.ResetReplayFailed, nil, false)
	case errors.Is(err, agent.ErrClosed):
		rest, rerr := a.sub.Rest()
		switch {
		case rerr == nil:
			f.terminal(protocol.ResetSessionClosed, append(pending, rest...), true)
		case errors.Is(rerr, agent.ErrRestUnavailable):
			// What Records handed over is contiguous from the position, a
			// prefix of the tail; the rest of it is unknown.
			f.terminal(protocol.ResetSessionClosed, pending, true)
		case errors.Is(rerr, agent.ErrNoRest):
			// Closed by this side, and every such close is one of those
			// handled above; nothing is owed.
			c.markClosed(a)
		default:
			c.srv.logf("control: conn %d %s: a bug: the closing tail is not whole: %v", c.id, a.id, rerr)
			f.terminal(protocol.ResetSessionClosed, nil, true)
		}
	default:
		c.srv.logf("control: conn %d %s: a bug: the subscription ended with %v", c.id, a.id, err)
		f.terminal(protocol.ResetReplayFailed, nil, false)
	}
}

// terminal queues a's terminal acknowledgement, unless a detach has claimed it
// first: for a closed log (endsSession), final — the closing records — and
// then the reset, the connection ending with it (conn.end); for any other end,
// the reset at once. A final record no client can fold makes the reset
// omitted.
func (f *forwarder) terminal(reason protocol.ResetReason, final []agent.Record, endsSession bool) {
	c, a := f.c, f.a
	if h := c.srv.hooks.beforeTerminal; h != nil {
		h(a.id, reason)
	}
	c.mu.Lock()
	if a.detach {
		c.mu.Unlock()
		return
	}
	a.ending = true
	if a.state == attLive {
		a.state = attClosing
		a.changedLocked()
	}
	c.unwritten++
	c.mu.Unlock()
	if endsSession {
		reason = f.tail(reason, final)
	} else {
		// Nothing holds this reset back: a synchronized or ready already due
		// goes before it only if it fits without waiting (a client
		// re-attaching learns readiness from the reply, plan 027 X17 6).
		f.catchUp(noWait)
	}
	if h := c.srv.hooks.beforeReset; h != nil {
		h(a.id, reason)
	}
	f.queueReset(reason, endsSession)
}

// tail queues a closed log's final records as events, waiting for room, with
// synchronized and the ready note where the position reaches them (astra r8
// 5: the note is never lost to the close once its seq is reached), and
// returns the reason the reset keeps: omitted after a record no client can
// fold. A push refused because the connection closed or its engine was
// replaced ends the tail; queueReset decides what that makes the reset.
func (f *forwarder) tail(reason protocol.ResetReason, final []agent.Record) protocol.ResetReason {
	f.awaitReadyNote()
	if f.catchUp(nil) != forwardedOK {
		return reason
	}
	for _, rec := range final {
		switch f.record(rec, nil) {
		case forwardedUnfoldable:
			return protocol.ResetOmitted
		case forwardedStopped:
			return reason
		}
		if f.catchUp(nil) != forwardedOK {
			return reason
		}
	}
	return reason
}

// awaitReadyNote settles, before a closed log's tail, whether a ready is owed
// and at what seq: a watcher (watchReady) still deciding is waited for. It
// decides promptly — its SyncSeq finds the log closing, or has already
// answered — once Ready has closed, which the engine's Close does before it
// closes the log; with Ready still open the session never started, and from
// the log's close on no ready can be owed. The wait also ends with the
// attachment's context (a detach, the replacement, the connection's close).
func (f *forwarder) awaitReadyNote() {
	a := f.a
	if f.ready != nil || a.watched == nil {
		return
	}
	select {
	case <-a.eng.Ready():
	default:
		return
	}
	select {
	case <-a.watched:
	case <-a.ctx.Done():
		return
	}
	select {
	case r := <-a.readyCh:
		f.ready = &r
	default:
	}
}

// queueReset queues a's reset and closes a in ONE conn.mu section — the
// terminal acknowledgement and the step it makes visible are one to reserve, a
// detach and a barrier (astra r8): a client that re-attaches the instant it
// reads the reset finds the place free, and a barrier sees the attachment
// closed only once its reset is queued. The reason is decided in that section
// too, under the flag replace sets there: a connection whose engine has been
// replaced gets reset{session_replaced}, whatever reason the forwarder had,
// and that reset is its TERMINAL line (plan 027 X25): the outbox, terminal-only
// since the replacement, admits it, and it seals it — nothing is written after
// it but the connection's close (astra r8 7). A reset queued before a
// replacement is an ordinary line, which the replacement drops. The reset
// takes the queue's reserve, which every other push leaves free, so the offer
// never waits under conn.mu.
func (f *forwarder) queueReset(reason protocol.ResetReason, endsSession bool) {
	c, a := f.c, f.a
	c.mu.Lock()
	replaced := c.replaced
	kind := ordinaryLine
	if replaced {
		reason, kind = protocol.ResetSessionReplaced, terminalLine
	}
	line, err := notificationLine(protocol.NotifyReset, protocol.ResetParams{Subscription: a.id, Reason: reason})
	if err == nil {
		_, err = c.out.offer(line, c.resetWritten, protocol.WriterQueueBytes, kind)
	}
	pushed := err == nil
	if !pushed {
		c.unwritten--
	}
	a.state = attClosed
	a.changedLocked()
	if endsSession && !replaced {
		c.endLocked("session ended")
	}
	c.mu.Unlock()
	if errors.Is(err, errNoRoom) {
		// The reserve is what every other line leaves free, and one reset is
		// owed at a time: a bug. The client is not left waiting for a reset
		// that never comes; it re-dials.
		c.srv.logf("control: conn %d %s: a bug: the final reset found no room", c.id, a.id)
		c.close("a final reset found no room")
	}
	if h := c.srv.hooks.ackQueued; h != nil && pushed {
		h(a.id, protocol.NotifyReset)
	}
	c.settle()
}

// resetWritten is a final reset on the socket — or given up: its write
// failed, or a replacement dropped it (plan 027 X25) — so the connection may
// close now if nothing else is left of it.
func (c *conn) resetWritten() {
	if h := c.srv.hooks.resetWritten; h != nil {
		h()
	}
	c.mu.Lock()
	c.unwritten--
	c.mu.Unlock()
	c.settle()
}

// notification is a notification as the wire writes it, its params typed so
// an event's raw body is encoded once, verbatim.
type notification[P any] struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  P      `json:"params"`
}

// notificationLine is one notification as a line. Only codec output and the
// server's own documents are ever in params (plan 027 X6).
func notificationLine[P any](method string, params P) ([]byte, error) {
	return protocol.MarshalLine(notification[P]{JSONRPC: protocol.JSONRPCVersion, Method: method, Params: params})
}
