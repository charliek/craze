package remote

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/charliek/craze/internal/protocol"
)

// Reconnect (plan 027 §3.14; astra 13; X18). When a connection's reader ends —
// EOF, a read error, or a line that breaks the protocol — the client redials,
// says hello{resume: {clientId, token}}, re-attaches its stream, and only then
// sends its commands again:
//
//  1. REDIAL, bounded (X18 5, X21): an episode — from the loss until a
//     connection is adopted — makes at most Options.Redials attempts and ends
//     Options.RedialWindow after the loss (3 and 10 s); each dial, handshake
//     and write of it — the adoption's re-attach and resends included — is
//     bounded by what is left of it (a write that runs out fails its attempt,
//     as a lost connection does), and at most Redials attempts are made within
//     any RedialWindow across episodes. The one wait it does not bound is the
//     adoption's for its re-attach's reply (X20: a when: "ready" attach may
//     wait out a slow load): the episode's clock stops meanwhile — unless it
//     is spent already, when it never stops and the adoption ends — and runs
//     again for any write made during that wait (a retried re-attach, a
//     detach), so every write is bounded by it (C8c). Past that
//     the client stops (ErrDisconnected): every command still waiting
//     resolves ErrOutcomeUnknown, reason disconnected, and the stream hands up
//     an Error item. A failed attempt waits Options.RedialBackoff, doubling,
//     before the next. Close closes the connection being adopted too, so
//     nothing of an episode outlives it.
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
//     that comes meanwhile is held, and sent after them. A command whose
//     wait-out's backoff is running is not held: its backoff sends it (X21).
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

// episode is a reconnect episode's bound (step 1): RedialWindow from the
// loss. Its clock stops while an adoption waits for its re-attach's reply
// (X20, X23) and runs again for as long as a write on the connection being
// adopted is in progress (C8c): only the wait for a reply is unbounded, never
// a write — not even one still in progress as the connection is published,
// which keeps the episode's end as its deadline until it returns (C8d). ctx
// ends once it is spent, or with the client. The reconnect goroutine drives
// it; the adopted connection's writes (send) report to it.
type episode struct {
	ctx    context.Context
	cancel context.CancelFunc

	mu    sync.Mutex
	timer *time.Timer
	// end is when it is spent, while its clock runs; left is what was left
	// of it when the clock stopped (halted).
	end    time.Time
	left   time.Duration
	halted bool
	// w is the connection being adopted, whose write deadline the episode
	// keeps (its end while the clock runs, none while it is stopped); waiting
	// says the adoption waits for its re-attach's reply, when the clock stops
	// unless one of w's writes is in progress (inWrite).
	w       *wire
	waiting bool
	inWrite bool
	// lifting is a connection published while one of its writes was in
	// progress: that write keeps the episode's end as its deadline, which is
	// lifted once it has returned (lift).
	lifting *wire
}

func (c *Client) newEpisode() *episode {
	ctx, cancel := context.WithCancel(c.ctx)
	return &episode{ctx: ctx, cancel: cancel, end: time.Now().Add(c.opts.RedialWindow),
		timer: time.AfterFunc(c.opts.RedialWindow, cancel)}
}

// spent says the episode is over: its time is up, or the client stopped.
func (e *episode) spent() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.ctx.Err() != nil || (!e.halted && !time.Now().Before(e.end))
}

// deadline is when the episode is spent, its clock running.
func (e *episode) deadline() time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.end
}

// adopt makes w the connection the episode bounds: every write on it from
// here carries the episode's end, until release.
func (e *episode) adopt(w *wire) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.w, e.waiting, e.inWrite = w, false, false
	w.ep.Store(e)
	_ = w.nc.SetWriteDeadline(e.end)
}

// release is w's adoption over (published, or given up): its writes no longer
// report to the episode, and its clock runs.
func (e *episode) release(w *wire) {
	w.ep.Store(nil)
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.w == w {
		e.w, e.waiting, e.inWrite = nil, false, false
		e.runLocked()
	}
}

// wait is the adoption beginning its wait for the re-attach's reply: the
// clock stops, unless a write is in progress (it stops when that ends) or
// the episode is spent already (it never stops: the wait, which also watches
// ctx, ends at once).
func (e *episode) wait() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.waiting = true
	if !e.inWrite {
		e.haltLocked()
	}
}

// resume is the adoption's wait over, however it ended: the clock runs again
// with what was left, and the write deadline applies again — to a write
// already blocked too.
func (e *episode) resume() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.waiting = false
	e.runLocked()
}

// writing is a write on w beginning (on) or ending: while the adoption waits
// for its reply the clock runs for the write, bounding it with what is left,
// and stops again after it. A nil episode, or another connection's write,
// counts for nothing.
func (e *episode) writing(w *wire, on bool) {
	if e == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if !on && e.lifting == w {
		// w was published while this write was in progress: now it has
		// returned, w's writes have no deadline.
		e.lifting = nil
		_ = w.nc.SetWriteDeadline(time.Time{})
	}
	if e.w != w {
		return
	}
	e.inWrite = on
	switch {
	case !e.waiting:
	case on:
		e.runLocked()
	default:
		e.haltLocked()
	}
}

// haltLocked stops the clock, and lifts the write deadline, only if the
// timer is stopped before it fires with time still left: an episode that has
// run out is spent (ctx ends), its deadline kept. e.mu is held.
func (e *episode) haltLocked() {
	if e.halted || !e.timer.Stop() {
		return
	}
	left := time.Until(e.end)
	if left <= 0 {
		e.cancel()
		return
	}
	e.left, e.halted = left, true
	if e.w != nil {
		_ = e.w.nc.SetWriteDeadline(time.Time{})
	}
}

// runLocked starts the clock again with what was left, and applies its end
// to the adopted connection's writes; e.mu is held.
func (e *episode) runLocked() {
	if e.halted {
		e.halted = false
		e.end = time.Now().Add(e.left)
		e.timer.Reset(e.left)
	}
	if e.w != nil {
		_ = e.w.nc.SetWriteDeadline(e.end)
	}
}

// lift is the adoption publishing w: its writes are no longer the episode's
// to bound. Its write deadline goes at once — or, while one of its writes is
// in progress, only once that write has returned (writing): publication, and
// the reconnect's end after it, never unbound a write already under way, which
// ends by the episode's end at the latest (C8d; astra r15 2).
func (e *episode) lift(w *wire) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.w == w && e.inWrite {
		e.lifting = w
		return
	}
	_ = w.nc.SetWriteDeadline(time.Time{})
}

func (e *episode) close() {
	e.mu.Lock()
	e.timer.Stop()
	e.mu.Unlock()
	e.cancel()
}

// reconnect is one reconnect episode: it redials until a connection is
// adopted, the episode is spent, or the client stops.
func (c *Client) reconnect() {
	defer c.wg.Done()
	// Every dial, handshake, write and backoff of the episode ends with it:
	// RedialWindow after the loss, or with the client.
	ep := c.newEpisode()
	defer ep.close()
	backoff := time.Duration(0)
	for attempt := 1; ; attempt++ {
		if attempt > c.opts.Redials || ep.spent() || !c.redialAllowed() {
			if c.ctx.Err() == nil {
				c.terminate(ErrDisconnected)
			}
			return
		}
		if backoff > 0 {
			t := time.NewTimer(backoff)
			select {
			case <-t.C:
			case <-ep.ctx.Done():
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
		w, h, err := c.open(ep.ctx, ep.deadline(), resume)
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
// (steps 2–4 above); ep is the episode, which bounds its sessions.list and
// every write it makes. false says w went before it could be adopted, and the
// reconnect redials; true says it was adopted, or the client stopped
// meanwhile.
func (c *Client) adopt(ep *episode, w *wire, h protocol.HelloResult, resume *protocol.Resume) bool {
	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		_ = w.nc.Close()
		return true
	}
	// Close closes w from here on, whatever it is blocked in (X21).
	c.adopting = w
	defer c.adopted(w)
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

	sid := ""
	if s != nil && fresh {
		// A fresh client may be speaking to another session (a replaced
		// engine): the host serves one, and says which.
		var list protocol.SessionsListResult
		err := c.exchange(ep.ctx, ep.deadline(), w, func() error {
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
	// Every write the adoption makes — the re-attach here, the resends at
	// publication, and whatever the reader posts meanwhile — ends with the
	// episode: a host that answers and then stops reading fails the
	// attempt, and cannot keep a command waiting past the episode (X21, C8c).
	ep.adopt(w)
	defer ep.release(w)
	var reattached <-chan struct{}
	if s != nil {
		reattached = s.reconnected(w, !fresh, sid)
	}
	if h := c.hooks.starting; h != nil {
		h()
	}
	// A client closed meanwhile starts no reader: w is hung up, and the
	// re-attach's reply callback told so, releasing its hold (C8d).
	c.startReader(w)
	if reattached != nil {
		if h := c.hooks.pausing; h != nil {
			h(ep.ctx.Done())
		}
		// The re-attach's reply may take minutes (a when: "ready" attach
		// during a slow load, X20): the episode's clock stops until the wait
		// ends — however it ends, so a redial after it has what was left —
		// except while a write is in progress, which the clock bounds; and
		// an episode already spent never stops, and ends the wait (C8c).
		ep.wait()
		var gone, spent bool
		select {
		case <-reattached:
		case <-w.gone:
			gone = true
		case <-ep.ctx.Done():
			select {
			case <-reattached:
			default:
				spent = true
			}
		}
		ep.resume()
		switch {
		case c.ctx.Err() != nil:
			_ = w.nc.Close()
			return true
		case gone:
			return false
		case spent:
			// Spent while it waited, or before it began: the reconnect,
			// finding it so, gives up.
			_ = w.nc.Close()
			return false
		}
	}
	// The stream has re-attached on the session the host serves now (or
	// stopped, or owes a re-attach with no cursor once its caller drains).
	c.mu.Lock()
	c.unresumed = false
	c.mu.Unlock()
	return c.publish(ep, w)
}

// adopted is adopt having returned: w, published or given up, is no longer
// the reconnect's to close.
func (c *Client) adopted(w *wire) {
	c.mu.Lock()
	if c.adopting == w {
		c.adopting = nil
	}
	c.mu.Unlock()
}

// publish sends every command held — in wire order, the resends first — on
// w, and makes w the connection calls go to once none is left: a command
// registered, retried or waited out meanwhile finds no connection and is held,
// so the next round sends it, and nothing overtakes a resend on w (X18 3).
// Its writes carry ep's deadline, which it lifts as it publishes w — once a
// write still in progress has returned, if one is (episode.lift). false says
// w went first.
func (c *Client) publish(ep *episode, w *wire) bool {
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
		// Held: waiting for a connection, and not for its wait-out's backoff,
		// which sends it when it runs out (X21).
		type heldCmd struct {
			cmd *command
			seq uint64
		}
		var held []heldCmd
		for _, cmd := range c.cmds {
			if cmd.want && !cmd.out && !cmd.waiting && !cmd.resolved && !cmd.gone {
				held = append(held, heldCmd{cmd, cmd.key()})
			}
		}
		if len(held) == 0 {
			ep.lift(w)
			c.cur, c.adopting = w, nil
			c.changedLocked()
			c.mu.Unlock()
			if h := c.hooks.published; h != nil {
				h()
			}
			return true
		}
		c.mu.Unlock()
		// Wire order: the order of first writes — each keyed as its write
		// began, before its first byte, so one a writer is still inside (or
		// has not yet returned from) is keyed already (C8c); a command never
		// written goes after every one that was, in the order it was issued
		// (c.cmds is in mint order, and the sort is stable).
		slices.SortStableFunc(held, func(a, b heldCmd) int {
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
		for _, x := range held {
			err := c.attemptOn(x.cmd, w)
			if h := c.hooks.resent; h != nil {
				h(x.cmd.id)
			}
			if errors.Is(err, ErrConnectionLost) {
				// w is going — it was gone already, or a write failed or ran
				// out of the episode (its reader is on its way out): the
				// command is held again if none of it was written, and the
				// next connection sends it.
				<-w.gone
				return false
			}
		}
	}
}
