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
//     reads it (awaitAttachment);
//   - synchronized{subscription, seq} once it has queued the record whose Seq
//     is the subscription's Cutoff — at once, before any event, when the cutoff
//     is the reply's after (nothing to replay) (§3.4);
//   - ready, when one is owed, once its position has reached the seq the
//     ready watcher read after the start (attach.go, watchReady);
//   - and, when the subscription ends, its terminal acknowledgement: the final
//     records, if any, then reset{subscription, reason} (terminal).
//
// It waits for room in the writer queue (the outbox's budget), so a stuck
// socket backs the subscription up in the log, and the log drops it
// slow_consumer without ever waiting: a publish only offers a record to a
// subscription's buffer (agent.EventLog). The final reset uses the queue's 1
// KiB reserve (pushReserved), so it is always queued.
//
// How a subscription ended decides its reset (§3.4's table):
//
//   - a record with Omitted set (one no client can fold): reset{omitted}, and
//     the subscription is closed — the client re-attaches with no cursor;
//   - ErrSlowConsumer: reset{slow_consumer};
//   - an ErrCursorUnresolvable from the journal leg of a cursor replay (the
//     asynchronous one; the synchronous refusals are the attach reply's reset):
//     reset{replay_failed};
//   - ErrClosed with Rest answering — the log closed: Rest's records, the
//     session's closing records, as events, then reset{session_closed}; and the
//     connection ends (conn.end), closing once all of it is written.
//     ErrRestUnavailable (a journal leg outstanding at the close):
//     reset{session_closed} without them — never a suffix posing as the tail;
//   - ErrClosed with ErrNoRest: the subscription was closed by this side — a
//     detach, whose reply is the terminal acknowledgement; the engine's
//     replacement, whose reset is session_replaced; or the connection's close,
//     after which nothing is owed;
//   - anything else — a hole the log refused to deliver, or a closing tail that
//     is not whole — is a bug: it is logged, and the client is sent the reset
//     that makes it start again with nothing it folded trusted (replay_failed;
//     session_closed at a log's close). Never a partial tail.

// forwarded is what became of one record the forwarder tried to queue.
type forwarded uint8

const (
	forwardedOK forwarded = iota
	// forwardedStopped: nothing more can be queued — the connection has gone,
	// or the attachment's context was cancelled (a detach, the engine's
	// replacement).
	forwardedStopped
	// forwardedUnfoldable: the record is one no client can fold — Omitted — or
	// one that could not be put on a line.
	forwardedUnfoldable
)

// forward is the attachment's forwarder.
func (c *conn) forward(a *attachment) {
	defer close(a.stopped)
	// The ready watcher's wait ends with the forwarder.
	defer a.cancel()
	pos := a.after
	var ready *readyNote
	if a.cutoff == pos {
		// Nothing to replay: the stream is synchronized at once, right after
		// the reply.
		if !c.notify(a, protocol.NotifySynchronized, protocol.SynchronizedParams{Subscription: a.id, Seq: pos}) {
			c.halt(a, &pos)
			return
		}
	}
	records := a.sub.Records()
	for {
		select {
		case rec, open := <-records:
			if !open {
				c.subscriptionEnded(a, &pos, ready)
				return
			}
			switch c.forwardRecord(a, rec, &pos) {
			case forwardedStopped:
				c.halt(a, &pos)
				return
			case forwardedUnfoldable:
				a.sub.Close()
				c.terminal(a, protocol.ResetOmitted, nil, false, &pos)
				return
			}
		case r := <-a.readyCh:
			ready = &r
		}
		if ready != nil && pos >= ready.seq {
			if !c.notify(a, protocol.NotifyReady, ready.params) {
				c.halt(a, &pos)
				return
			}
			ready = nil
		}
	}
}

// forwardRecord queues rec as an event notification, moves the position a
// barrier reads to it, and queues synchronized when rec is at the cutoff.
func (c *conn) forwardRecord(a *attachment, rec agent.Record, pos *uint64) forwarded {
	if rec.Omitted != nil {
		return forwardedUnfoldable
	}
	if h := c.srv.hooks.beforeForward; h != nil {
		h(a.id, rec.Seq)
	}
	if a.ctx.Err() != nil {
		return forwardedStopped
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
	if c.out.push(a.ctx, line, nil) != nil {
		return forwardedStopped
	}
	c.mu.Lock()
	a.queued = rec.Seq
	a.changedLocked()
	c.mu.Unlock()
	*pos = rec.Seq
	if rec.Seq == a.cutoff &&
		!c.notify(a, protocol.NotifySynchronized, protocol.SynchronizedParams{Subscription: a.id, Seq: rec.Seq}) {
		return forwardedStopped
	}
	return forwardedOK
}

// notify queues one notification of a's; false once nothing more can be
// queued.
func (c *conn) notify(a *attachment, method string, params any) bool {
	if a.ctx.Err() != nil {
		return false
	}
	line, err := notificationLine(method, params)
	if err != nil {
		// Only the server's own documents are in these; one that cannot be
		// encoded is a bug, logged, and the notification is left out.
		c.srv.logf("control: conn %d %s: %s cannot be put on a line: %v", c.id, a.id, method, err)
		return true
	}
	return c.out.push(a.ctx, line, nil) == nil
}

// halt is a forwarder that can queue nothing more before its subscription
// ended: a detach (whose reply is the terminal acknowledgement), the engine's
// replacement (reset{session_replaced}), or the connection's close (nothing is
// owed).
func (c *conn) halt(a *attachment, pos *uint64) {
	c.mu.Lock()
	detach, replaced := a.detach, c.replaced
	c.mu.Unlock()
	switch {
	case detach:
	case c.ctx.Err() == nil && replaced:
		a.sub.Close()
		c.terminal(a, protocol.ResetSessionReplaced, nil, false, pos)
	default:
		a.sub.Close()
		c.markClosed(a)
	}
}

// subscriptionEnded is a's subscription ending — Records closed — and the
// reset its end is owed (the package's table above). ready is a ready
// notification the forwarder holds and has not queued: one already due is
// queued first, since the subscription did deliver through its seq.
func (c *conn) subscriptionEnded(a *attachment, pos *uint64, ready *readyNote) {
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
		c.terminal(a, protocol.ResetSessionReplaced, nil, false, pos)
		return
	}
	if ready == nil {
		select {
		case r := <-a.readyCh:
			ready = &r
		default:
		}
	}
	if ready != nil && *pos >= ready.seq {
		_ = c.notify(a, protocol.NotifyReady, ready.params)
	}
	var unresolvable agent.ErrCursorUnresolvable
	switch {
	case errors.Is(err, agent.ErrSlowConsumer):
		c.terminal(a, protocol.ResetSlowConsumer, nil, false, pos)
	case errors.As(err, &unresolvable):
		c.terminal(a, protocol.ResetReplayFailed, nil, false, pos)
	case errors.Is(err, agent.ErrClosed):
		rest, rerr := a.sub.Rest()
		switch {
		case rerr == nil:
			c.terminal(a, protocol.ResetSessionClosed, rest, true, pos)
		case errors.Is(rerr, agent.ErrRestUnavailable):
			c.terminal(a, protocol.ResetSessionClosed, nil, true, pos)
		case errors.Is(rerr, agent.ErrNoRest):
			// Closed by this side, and every such close is one of those
			// handled above; nothing is owed.
			c.markClosed(a)
		default:
			c.srv.logf("control: conn %d %s: a bug: the closing tail is not whole: %v", c.id, a.id, rerr)
			c.terminal(a, protocol.ResetSessionClosed, nil, true, pos)
		}
	default:
		c.srv.logf("control: conn %d %s: a bug: the subscription ended with %v", c.id, a.id, err)
		c.terminal(a, protocol.ResetReplayFailed, nil, false, pos)
	}
}

// terminal queues a's terminal acknowledgement — final, the closing records,
// as events, then reset{reason} in the queue's reserve — unless a detach has
// claimed it first, and closes a once it is queued. endsSession says the log
// has closed: the connection then ends (conn.end), closing once everything is
// written. A final record no client can fold makes the reset omitted, and one
// that cannot be queued because the engine was replaced meanwhile makes it
// session_replaced: a reset never claims records were delivered that were
// not.
func (c *conn) terminal(a *attachment, reason protocol.ResetReason, final []agent.Record, endsSession bool, pos *uint64) {
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
final:
	for _, rec := range final {
		switch c.forwardRecord(a, rec, pos) {
		case forwardedUnfoldable:
			reason = protocol.ResetOmitted
			break final
		case forwardedStopped:
			c.mu.Lock()
			if c.replaced {
				reason = protocol.ResetSessionReplaced
			}
			c.mu.Unlock()
			break final
		}
	}
	line, err := notificationLine(protocol.NotifyReset, protocol.ResetParams{Subscription: a.id, Reason: reason})
	pushed := err == nil && c.out.pushReserved(c.ctx, line, c.resetWritten) == nil
	c.mu.Lock()
	if !pushed {
		c.unwritten--
	}
	a.state = attClosed
	a.changedLocked()
	if endsSession && reason != protocol.ResetSessionReplaced {
		c.endLocked("session ended")
	}
	c.mu.Unlock()
	c.settle()
}

// resetWritten is a final reset on the socket: the connection may close now
// if nothing else is left of it.
func (c *conn) resetWritten() {
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
