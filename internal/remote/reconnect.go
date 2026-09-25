package remote

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/charliek/craze/internal/protocol"
)

// Reconnect (plan 027 §3.14; astra 13; X18). When a connection's reader ends —
// EOF, a read error, or a line that breaks the protocol — the client redials,
// says hello{resume: {clientId, token}}, re-attaches its stream, and only then
// sends its commands again:
//
//  1. REDIAL, bounded (X18 5): an episode — from the loss until a connection
//     is adopted — makes at most Options.Redials attempts and ends
//     Options.RedialWindow after the loss (3 and 10 s); each dial and
//     handshake is bounded by what is left of it, and at most Redials attempts
//     are made within any RedialWindow across episodes. Past that the client
//     stops (ErrDisconnected): every command still waiting resolves
//     ErrOutcomeUnknown, reason disconnected, and the stream hands up an Error
//     item. A failed attempt waits Options.RedialBackoff, doubling, before the
//     next.
//  2. HELLO. With the token — unless the stream saw reset{session_replaced}
//     (the old token is void; §3.4) or the host refused it (bad_token), when
//     the hello is a fresh one. The host answers resumed: true (the same client
//     id, the same engine incarnation) or a fresh id. resumed: true is trusted
//     only when it names the client id asked for, with the token sent, on the
//     host that minted it (endpoint.hostId, X18 2); anything else is a resume
//     loss: the client's identity moves on, and every command that may have
//     run under the old one resolves ErrOutcomeUnknown, reason resume_lost, at
//     once: nothing that may have run is resent (command.go, "THE RESEND
//     RULE").
//  3. RE-ATTACH, before the connection is used for anything else: after
//     resumed: true with the stream's cursor (a silent resume: the host answers
//     from its ring or journal and the stream goes on with no item, no gap and
//     no duplicate), and after a resume loss with no cursor (the host answers
//     with a snapshot: a Restore item) — the session may even be another, so
//     the client asks sessions.list first. Before readiness the re-attach is
//     when: "ready" with no cursor, as after a slow_consumer (§3.4, astra 22).
//     A stream whose caller has fallen behind (a local slow consumer) is
//     re-attached once its caller has drained, not here (attach.go).
//  4. COMMANDS. Once the re-attach is answered, so that a command's reply
//     cannot overtake, on the wire, the events the re-attached stream replays
//     (§3.6's reply barrier holds per attachment): every attempt the lost
//     connection owed an answer is counted unanswered for good, and every
//     command held — those whose reply was lost, resent under their SAME id
//     (the stored answer comes back, or in_progress, waited out), and those
//     that waited for a connection — is sent in wire order (X18 3), all of
//     them before the connection is published: a command, retry or wait-out
//     that comes meanwhile is held, and sent after them.
//
// A stream that saw reset{session_closed} is over, and so is the session: the
// host closes the connection, which is not redialled (ErrSessionEnded).

// lost is w's reader having ended. If w is the connection calls go to, the
// client reconnects — unless the session is over or the client has stopped.
// A connection still being opened (the reconnect's own) is the reconnect's to
// notice.
func (c *Client) lost(w *wire) {
	c.mu.Lock()
	if c.err != nil || c.cur != w {
		c.mu.Unlock()
		return
	}
	c.cur = nil
	c.changedLocked()
	if c.ended {
		c.mu.Unlock()
		c.terminate(ErrSessionEnded)
		return
	}
	c.wg.Add(1)
	c.mu.Unlock()
	go c.reconnect()
}

// reconnect is one reconnect episode: it redials until a connection is
// adopted, the episode is spent, or the client stops.
func (c *Client) reconnect() {
	defer c.wg.Done()
	// The episode's own context: every dial, handshake and backoff of it ends
	// RedialWindow after the loss, or with the client.
	ep, cancel := context.WithTimeout(c.ctx, c.opts.RedialWindow)
	defer cancel()
	backoff := time.Duration(0)
	for attempt := 1; ; attempt++ {
		if attempt > c.opts.Redials || ep.Err() != nil || !c.redialAllowed() {
			if c.ctx.Err() == nil {
				c.terminate(ErrDisconnected)
			}
			return
		}
		if backoff > 0 {
			t := time.NewTimer(backoff)
			select {
			case <-t.C:
			case <-ep.Done():
				t.Stop()
				continue
			}
		}
		c.mu.Lock()
		var resume *protocol.Resume
		if !c.voidToken {
			resume = &protocol.Resume{ClientID: c.hello.ClientID, Token: c.hello.Token}
		}
		c.mu.Unlock()
		w, h, err := c.open(ep, resume)
		if err == nil && c.adopt(ep, w, h, resume) {
			return
		}
		var e *Error
		if errors.As(err, &e) && e.Reason == protocol.ReasonBadToken {
			// The host says the token is not this client's: the resume is
			// lost, and the next hello is a fresh one.
			c.mu.Lock()
			c.voidToken = true
			c.mu.Unlock()
		}
		if c.ctx.Err() != nil {
			return
		}
		backoff = min(max(2*backoff, c.opts.RedialBackoff), maxRedialBackoff)
	}
}

// redialAllowed records one redial, or reports that the window's are spent.
func (c *Client) redialAllowed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	keep := c.redials[:0]
	for _, at := range c.redials {
		if now.Sub(at) < c.opts.RedialWindow {
			keep = append(keep, at)
		}
	}
	c.redials = keep
	if len(c.redials) >= c.opts.Redials {
		return false
	}
	c.redials = append(c.redials, now)
	return true
}

// adopt makes w — dialled, its hello (asking to resume resume, nil for a
// fresh one) answered h, its reader not started — the connection calls go to
// (steps 2–4 above); ep is the episode, which bounds its sessions.list. false
// says w went before it could be adopted, and the reconnect redials; true says
// it was adopted, or the client stopped meanwhile.
func (c *Client) adopt(ep context.Context, w *wire, h protocol.HelloResult, resume *protocol.Resume) bool {
	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		_ = w.nc.Close()
		return true
	}
	// Resumed only as the host says so, and only as the client it asked to
	// be, with the token it sent, on the host that minted it: an answer that
	// resumed something else — or anything, to a hello that asked for
	// nothing — is a fresh client as far as the resend rule goes (X18 2).
	resumed := h.Resumed && resume != nil && h.ClientID == resume.ClientID &&
		h.Token == resume.Token && h.Endpoint.HostID == c.hostID
	c.hello = h
	c.voidToken = false
	if !resumed {
		// Another client id, or one this client cannot trust to be its own:
		// another identity. Nothing that may have run under the old one is
		// ever sent again (the resend rule).
		c.identity++
		c.hostID = h.Endpoint.HostID
		c.unresumed = true
		for _, cmd := range c.cmds {
			if cmd.want && cmd.ranUnder != 0 {
				c.resolveLocked(cmd, protocol.ReasonResumeLost)
			}
		}
	}
	// Every attempt still outstanding was on a connection now gone,
	// unanswered: the host may still run it, whatever a later attempt is told
	// (it stays in unanswered), and the command is held for a resend.
	for _, cmd := range c.cmds {
		cmd.out = false
	}
	// The stream goes on from where it was only if no resume has been lost
	// since it last re-attached — this reconnect's, or one whose reconnect
	// failed before the stream re-attached.
	fresh := c.unresumed
	s := c.stream
	c.mu.Unlock()

	var reattached <-chan struct{}
	if s != nil {
		sid := ""
		if fresh {
			// A fresh client may be speaking to another session (a replaced
			// engine): the host serves one, and says which.
			var list protocol.SessionsListResult
			err := c.exchange(ep, w, func() error {
				return c.syncCall(w, protocol.MethodSessionsList, protocol.SessionsListParams{}, &list)
			})
			var e *Error
			switch {
			case err == nil && len(list.Sessions) == 1:
				sid = list.Sessions[0].SessionID
			case err != nil && !errors.As(err, &e):
				// The connection failed under the exchange, or the host broke
				// the protocol: redial.
				_ = w.nc.Close()
				return false
			}
		}
		reattached = s.reconnected(w, !fresh, sid)
	}
	c.mu.Lock()
	c.startReaderLocked(w)
	c.mu.Unlock()
	if reattached != nil {
		select {
		case <-reattached:
		case <-w.gone:
			return false
		case <-c.ctx.Done():
			_ = w.nc.Close()
			return true
		}
	}
	// The stream has re-attached on the session the host serves now (or
	// stopped, or owes a re-attach with no cursor once its caller drains).
	c.mu.Lock()
	c.unresumed = false
	c.mu.Unlock()
	return c.publish(w)
}

// publish sends every command held — in wire order, the resends first — on
// w, and makes w the connection calls go to once none is left: a command
// registered, retried or waited out meanwhile finds no connection and is held,
// so the next round sends it, and nothing overtakes a resend on w (X18 3).
// false says w went first.
func (c *Client) publish(w *wire) bool {
	for {
		c.mu.Lock()
		if c.err != nil {
			c.mu.Unlock()
			_ = w.nc.Close()
			return true
		}
		select {
		case <-w.gone:
			// Gone before it was adopted: its reader's lost found it was not
			// the current connection, so this reconnect goes on.
			c.mu.Unlock()
			return false
		default:
		}
		var held []*command
		for _, cmd := range c.cmds {
			if cmd.want && !cmd.out && !cmd.resolved && !cmd.gone {
				held = append(held, cmd)
			}
		}
		if len(held) == 0 {
			c.cur = w
			c.changedLocked()
			c.mu.Unlock()
			if h := c.hooks.published; h != nil {
				h()
			}
			return true
		}
		c.mu.Unlock()
		// Wire order: the order of first sends; a command never sent goes
		// after every one that was, in the order it was issued (c.cmds is in
		// mint order, and the sort is stable).
		slices.SortStableFunc(held, func(a, b *command) int {
			switch {
			case a.seq == b.seq:
				return 0
			case a.seq == 0:
				return 1
			case b.seq == 0:
				return -1
			case a.seq < b.seq:
				return -1
			}
			return 1
		})
		for _, cmd := range held {
			if errors.Is(c.attemptOn(cmd, w), errUnsent) {
				// w is going (its reader is on its way out): the command is
				// held again, and the next connection sends it.
				<-w.gone
				return false
			}
		}
	}
}
