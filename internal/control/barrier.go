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
//     terminal acknowledgement (awaitAttachment, C7's half).
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
// turned into an error (§3.7) — while session.sync, which has no seq to
// answer with, answers not_accepting.
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
		// closing log.
		c.srv.logf("control: conn %d: reply barrier: %v", c.id, err)
		closing = true
	}
	if !c.awaitAttachment(seq, closing) {
		return 0, false, false
	}
	return seq, closing, true
}

// awaitAttachment is the barrier's attachment half, and C7's to fill: while
// the connection has an attachment that is live OR closing, wait until its
// forwarder has queued the record with Seq ≥ seq, or the attachment's
// terminal acknowledgement (its final reset, or a detach's reply), and report
// false if the connection closes first. There is no timeout. closing says the
// log could vouch for no seq; what a closing attachment owes a reply then is
// C7's to decide with the final records.
//
// C6 serves no attachment, so there is nothing to wait for: it reports only
// whether the connection is still there to take a reply.
func (c *conn) awaitAttachment(seq uint64, closing bool) bool {
	_, _ = seq, closing
	return c.ctx.Err() == nil
}
