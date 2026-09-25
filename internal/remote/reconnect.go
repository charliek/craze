package remote

import (
	"context"
	"errors"
	"time"

	"github.com/charliek/craze/internal/protocol"
)

// Reconnect (plan 027 §3.14; astra 13). When a connection's reader ends — EOF
// or a read error — the client redials, says hello{resume: {clientId, token}},
// re-attaches its stream, and only then sends its commands again:
//
//  1. REDIAL, bounded: at most Options.Redials dial attempts within any
//     Options.RedialWindow (3 within 10 s); past that the client stops
//     (ErrDisconnected): every command still waiting resolves
//     ErrOutcomeUnknown, reason disconnected, and the stream hands up an
//     Error item. A failed attempt waits Options.RedialBackoff, doubling,
//     before the next.
//  2. HELLO. With the token — unless the stream saw reset{session_replaced}
//     (the old token is void; §3.4) or the host refused it (bad_token), when
//     the hello is a fresh one. The host answers resumed: true (the same client
//     id, the same engine incarnation) or a fresh id; resumed: false moves the
//     client's identity on, and every command that may have run under the old
//     one resolves ErrOutcomeUnknown, reason resume_lost, at once: nothing
//     that may have run is resent (command.go, "THE RESEND RULE").
//  3. RE-ATTACH, before the connection is used for anything else: after
//     resumed: true with the stream's cursor (a silent resume: the host answers
//     from its ring or journal and the stream goes on with no item, no gap and
//     no duplicate), and after resumed: false with no cursor (the host answers
//     with a snapshot: a Restore item) — the session may even be another, so
//     the client asks sessions.list first. Before readiness the re-attach is
//     when: "ready" with no cursor, as after a slow_consumer (§3.4, astra 22).
//  4. COMMANDS. Once the re-attach is answered, so that a command's reply
//     cannot overtake, on the wire, the events the re-attached stream replays
//     (§3.6's reply barrier holds per attachment): every command whose reply
//     the lost connection owed is resent under its SAME id — its stored answer
//     comes back, or in_progress, waited out — and every command that was
//     waiting for a connection is sent.
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

// reconnect redials until a connection is adopted, the redials are spent, or
// the client stops.
func (c *Client) reconnect() {
	defer c.wg.Done()
	backoff := time.Duration(0)
	for {
		if !c.redialAllowed() {
			c.terminate(ErrDisconnected)
			return
		}
		if backoff > 0 {
			t := time.NewTimer(backoff)
			select {
			case <-t.C:
			case <-c.ctx.Done():
				t.Stop()
				return
			}
		}
		c.mu.Lock()
		var resume *protocol.Resume
		if !c.voidToken {
			resume = &protocol.Resume{ClientID: c.hello.ClientID, Token: c.hello.Token}
		}
		c.mu.Unlock()
		w, h, err := c.open(c.ctx, resume)
		if err == nil && c.adopt(w, h, resume) {
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
// (steps 2–4 above). false says w went before it could be adopted, and the
// reconnect redials; true says it was adopted, or the client stopped
// meanwhile.
func (c *Client) adopt(w *wire, h protocol.HelloResult, resume *protocol.Resume) bool {
	// Resumed only as the host says so, and only as the client it asked to
	// be: an answer that resumed something else — or anything, to a hello
	// that asked for nothing — is a fresh client as far as the resend rule
	// goes.
	resumed := h.Resumed && resume != nil && h.ClientID == resume.ClientID
	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		_ = w.nc.Close()
		return true
	}
	c.hello = h
	c.voidToken = false
	if !resumed {
		// Another client id: another identity. Nothing that may have run
		// under the old one is ever sent again (the resend rule).
		c.identity++
		for _, cmd := range c.cmds {
			if cmd.want && cmd.ranUnder != 0 {
				c.resolveLocked(cmd, protocol.ReasonResumeLost)
			}
		}
	}
	s := c.stream
	c.mu.Unlock()

	var reattached <-chan struct{}
	if s != nil {
		sid := ""
		if !resumed {
			// A fresh client may be speaking to another session (a replaced
			// engine): the host serves one, and says which.
			var list protocol.SessionsListResult
			err := c.exchange(w, func() error {
				return c.syncCall(w, protocol.MethodSessionsList, protocol.SessionsListParams{}, &list)
			})
			var e *Error
			switch {
			case err == nil && len(list.Sessions) == 1:
				sid = list.Sessions[0].SessionID
			case err != nil && !errors.As(err, &e):
				// The connection failed under the exchange: redial.
				_ = w.nc.Close()
				return false
			}
		}
		reattached = s.reconnected(w, resumed, sid)
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
	c.cur = w
	c.changedLocked()
	var sends []*command
	for _, cmd := range c.cmds {
		if !cmd.want || cmd.resolved {
			continue
		}
		if cmd.out {
			// Its attempt was on a lost connection, unanswered: the host may
			// still run that one, whatever a later attempt is told.
			cmd.stray = true
		}
		sends = append(sends, cmd)
	}
	c.mu.Unlock()
	for _, cmd := range sends {
		c.attempt(cmd)
	}
	return true
}

// exchange runs fn — synchronous calls on w, whose reader has not started —
// bounded by the handshake timeout and the client's life.
func (c *Client) exchange(w *wire, fn func() error) error {
	stop := context.AfterFunc(c.ctx, func() { _ = w.nc.Close() })
	_ = w.nc.SetDeadline(time.Now().Add(c.opts.HandshakeTimeout))
	err := fn()
	stop()
	_ = w.nc.SetDeadline(time.Time{})
	return err
}
