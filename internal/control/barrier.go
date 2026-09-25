package control

import (
	"errors"

	"github.com/charliek/craze/internal/agent"
)

// barrier is the reply barrier (plan 027 §3.6, SF-10; the panel's item 7): a
// handler that ran a mutating command — success or error — calls it before it
// queues the reply, and session.sync is it with no command.
//
//  1. SyncSeq → S: everything enqueued before the call is committed, and S is
//     the log's committed head, ≥ the seq of every event the command caused.
//  2. The connection's attachment, live OR closing, is waited for until its
//     forwarder has queued to the writer the record with Seq ≥ S or its
//     terminal acknowledgement (awaitAttachment).
//  3. The caller queues the reply to the one FIFO writer, so it is on the
//     wire after every event the command emitted.
//
// NO TIMEOUT ESCAPES THE ORDER (astra 8, GLM 4): each wait ends only as it
// says, or when the connection closes — the writer's stall close is what ends
// a wedged one — and then ok is false: the reply is dropped, and the receipt
// keeps the answer for the client's resend after a resume.
//
// A SyncSeq that fails because the log is closing (agent.ErrLogClosing) is
// closing = true with ok: the session is ending and the subscription with it,
// so a command's reply carries the command's own result — a success is never
// turned into an error (§3.7) — after the attachment's final records and
// reset (awaitAttachment), while session.sync, which has no seq to answer
// with, answers not_accepting.
func (c *conn) barrier(b *bound) (seq uint64, closing, ok bool) {
	seq, err := b.eng.SyncSeq(c.ctx)
	switch {
	case err == nil:
	case errors.Is(err, agent.ErrLogClosing):
		closing = true
	case c.ctx.Err() != nil:
		return 0, false, false
	default:
		// FlushSeq's errors are ErrLogClosing, ctx's, and ErrFlushGaveUp for a
		// done channel, which this call never passes: nothing else reaches
		// here. Were something to, the reply keeps its own answer, as on a
		// closing log — but with no seq, and no close of the log to end the
		// attachment it could wait for, it waits for nothing.
		c.srv.logf("control: conn %d: reply barrier: %v", c.id, err)
		return 0, true, c.ctx.Err() == nil
	}
	if !c.awaitAttachment(seq, closing) {
		return 0, false, false
	}
	return seq, closing, true
}

// awaitAttachment is the barrier's attachment half (§3.6 step 2; §3.7's
// lifecycle, astra r2 15, r3 15). The connection's attachment, as it stands
// when the wait begins, is waited for while it is not yet closed — live,
// closing, or pending (an attach not yet answered, whose reply's after is
// where its position will start) — until its forwarder has queued to the
// writer the record with Seq ≥ seq (its position, which starts at the reply's
// after), or the attachment is closed: its terminal acknowledgement — the
// final records and reset, or a detach's reply — is queued. With no
// attachment, or one already closed, nothing is waited for: a newer attach
// begun meanwhile is not this wait's. It reports false once the connection
// closes, the one other way the wait ends: there is NO timeout, and the
// writer's stall close is what ends a wedged one.
//
// closing says the log could vouch for no seq (agent.ErrLogClosing): the
// session is ending, the command's events are among what the log's close
// commits, and the subscription's final records carry them. So an attachment
// is then waited for until it is closed — its final records and
// reset{session_closed} queued — and the reply follows them. It is still
// written: a session's end closes the connection only once every admitted
// request's reply is on the socket (conn.end).
func (c *conn) awaitAttachment(seq uint64, closing bool) bool {
	c.mu.Lock()
	a := c.att
	for {
		switch {
		case c.ctx.Err() != nil:
			c.mu.Unlock()
			return false
		case a == nil || a.state == attClosed:
			c.mu.Unlock()
			return true
		case !closing && a.state != attPending && a.queued >= seq:
			c.mu.Unlock()
			return true
		}
		changed := a.changed
		c.mu.Unlock()
		if h := c.srv.hooks.barrierWaits; h != nil {
			h(seq)
		}
		select {
		case <-changed:
		case <-c.ctx.Done():
			return false
		}
		c.mu.Lock()
	}
}
