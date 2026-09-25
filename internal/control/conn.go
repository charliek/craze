package control

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/protocol"
)

// conn is one accepted connection and its roles (the package doc): one
// reader, a handler per admitted request, and one writer draining its
// outbox. It is closed once, by whichever of them — or the server — gets
// there first.
type conn struct {
	srv *Server
	nc  net.Conn
	id  uint64
	// ctx is the connection's own: cancelled when it closes, which ends every
	// wait of its — for an admission slot, for room in the outbox, for the
	// reply barrier. A command's context is never this one (§3.6).
	ctx    context.Context
	cancel context.CancelFunc
	out    *outbox
	// slots is the admission semaphore: RequestsPerConnection tokens, one
	// taken by the reader before it reads a line and given back once that
	// line's reply is written to the socket, or dropped with the connection.
	slots chan struct{}

	// closing is set first thing in close, before its cleanup takes bindMu
	// (bind.go reads it there). superseded says a transfer moved this
	// connection's client to another connection. ending says the session is
	// over for this connection — its engine ended (end) or was replaced
	// (replace) — so it admits nothing more.
	closing    atomic.Bool
	superseded atomic.Bool
	ending     atomic.Bool
	closeOnce  sync.Once

	// bound is the connection's hello, guarded by Server.bindMu: written once,
	// by the reader, in the binding section; the reader reads it after, and a
	// handler is handed it.
	bound *bound

	mu sync.Mutex
	// inflight is how many requests are admitted and their replies not yet
	// written — the reader's own slot, taken before it reads a line, included;
	// reading says the reader holds that slot for a line not yet read.
	// readEOF says the peer has half-closed (no more requests).
	inflight int
	reading  bool
	readEOF  bool
	// ended says the session is over for this connection (end): like readEOF,
	// no more requests, and the connection closes once nothing admitted is
	// unwritten and no attachment is open. endReason is the close's reason.
	// replaced says its engine was replaced (replace): from the section that
	// sets it the connection is terminal-only (plan 027 X25), and it closes as
	// soon as the one terminal line it owes, if any, is written
	// (replacedDoneLocked), whatever is still in flight — a handler's reply
	// then goes nowhere (§3.6).
	ended     bool
	endReason string
	replaced  bool
	// att is the connection's attachment (attach.go): the open one, or the
	// last, closed; nil before the first attach. At most one is ever open
	// (SQ14). nextSub numbers them: s-1, s-2, …
	att     *attachment
	nextSub int
	// unwritten is how many terminal acknowledgements of attachments — a final
	// reset, a detach's reply — are claimed or queued and not yet on the
	// socket (nor given up): the connection stays open for them (idleLocked).
	// A replaced connection waits only for its terminal line, which the
	// outbox counts itself (outbox.owesTerminal).
	unwritten int
}

func newConn(s *Server, nc net.Conn) *conn {
	ctx, cancel := context.WithCancel(context.Background())
	return &conn{
		srv:    s,
		nc:     nc,
		ctx:    ctx,
		cancel: cancel,
		out:    newOutbox(s.hooks.outboxFull),
		slots:  make(chan struct{}, protocol.RequestsPerConnection),
	}
}

// ------------------------------------------------------------------ reader

// read is the connection's one reader. It takes an admission slot before it
// reads each line, so it stops reading while RequestsPerConnection requests
// are admitted and unwritten.
func (c *conn) read() {
	defer c.srv.transport.Done()
	lr := protocol.NewLineReader(c.nc, protocol.InboundLineMax)
	for {
		if !c.acquire() {
			return
		}
		c.setReading(true)
		line, err := lr.ReadLine()
		c.setReading(false)
		switch {
		case err == nil && !c.admitting():
			// Superseded or closing while the line was read: the line — one
			// the reader had buffered before the socket closed — is no
			// request of this connection's any more. The reader stops.
			c.release()
			return
		case err == nil:
			c.dispatch(line)
		case errors.Is(err, protocol.ErrLineTooLong):
			// Discarded up to its newline; the connection goes on (§3.2).
			c.replyErr(nil, lineTooLong())
		case errors.Is(err, io.EOF):
			// Half-close: no more requests, and nothing is lost (§3.7).
			c.release()
			c.eof()
			return
		default:
			c.release()
			if !c.closing.Load() {
				c.close("read failed: " + err.Error())
			}
			return
		}
	}
}

// acquire takes an admission slot, waiting while every one is taken; false
// once the connection is closing or superseded (admitting), checked after the
// slot is taken, so a slot that came free as the connection was superseded
// admits nothing (astra r5 3).
func (c *conn) acquire() bool {
	if h := c.srv.hooks.beforeAcquire; h != nil {
		h(c.id)
	}
	select {
	case c.slots <- struct{}{}:
	default:
		if h := c.srv.hooks.admissionFull; h != nil {
			h()
		}
		select {
		case c.slots <- struct{}{}:
		case <-c.ctx.Done():
			return false
		}
	}
	c.mu.Lock()
	c.inflight++
	c.mu.Unlock()
	if !c.admitting() {
		c.release()
		return false
	}
	return true
}

// admitting reports whether the connection may still admit a request: it is
// neither closing, superseded, nor ending. A transfer marks a connection
// superseded under bindMu before it is closed, so from that moment its reader
// admits nothing more, whatever it had buffered; the end of its session (end,
// replace) does the same. It stops NEW requests only: a request already
// admitted is held back from the engine only if its binding has moved on
// (conn.command), and one on a connection that merely closed still runs.
func (c *conn) admitting() bool {
	return !c.closing.Load() && !c.superseded.Load() && !c.ending.Load()
}

// setReading marks the reader as holding its slot for a line not yet read
// (true), or as having read one (false). Taking its slot out of the count can
// make the connection idle — an end decided while the reader was between its
// admission check and here counted that slot — so it settles then.
func (c *conn) setReading(on bool) {
	c.mu.Lock()
	c.reading = on
	c.mu.Unlock()
	// Only an ending connection can be made idle by it (readEOF is never set
	// while the reader reads), and end/replace set ending before their own
	// settle, which reads reading after it: one of the two settles sees both.
	if on && c.ending.Load() {
		c.settle()
	}
}

// release gives a slot back: its reply is written, or will never be. The
// connection closes here if nothing is left of it (settle).
func (c *conn) release() {
	<-c.slots
	c.mu.Lock()
	c.inflight--
	c.mu.Unlock()
	c.settle()
}

// eof marks the read side done and closes the connection if nothing is in
// flight.
func (c *conn) eof() {
	c.mu.Lock()
	c.readEOF = true
	c.mu.Unlock()
	c.settle()
}

// end is the session ending for this connection (plan 027 §3.7, astra r5 13):
// its engine has closed. It is the half-close's rule with the session in the
// peer's place: the connection admits nothing more, every request already
// admitted is answered, an attachment delivers its final records and
// reset{session_closed} (forward.go), and the connection closes once all of
// that is on the socket (idleLocked). Its client is released by the close, as
// ever.
func (c *conn) end(reason string) {
	c.mu.Lock()
	c.endLocked(reason)
	c.mu.Unlock()
	c.settle()
}

// endLocked marks the session over for this connection; c.mu is held.
func (c *conn) endLocked(reason string) {
	c.ending.Store(true)
	if !c.ended {
		c.ended, c.endReason = true, reason
	}
}

// replace is its engine being replaced (§3.6, astra 14; Server.SetEngine). It
// makes the connection TERMINAL-ONLY (plan 027 X25) in the one conn.mu section
// that sets replaced: the connection admits no more requests, and from that
// section on its outbox admits only a TERMINAL line —
// reset{session_replaced}, or a claimed detach's `{}` — and that line seals
// it. Every ordinary line is dropped: one offered later is refused at its
// offer (the offerer gives its slot back), and every one still queued is
// dropped here (outbox.replace), its callback run once conn.mu is let go so
// its request's slot is given back too; a claimed detach's `{}` queued before
// the replacement is terminal, and kept. A handler still running keeps the
// engine it captured, and its reply goes nowhere (§3.6, astra r8 7): the
// client learns what became of it by resending after a fresh hello (resumed:
// false, the outcome unknown).
//
// What the connection owes is then at most ONE terminal line. A live or
// closing attachment whose end nobody has claimed ends with its forwarder's
// reset{session_replaced} — its subscription is closed here, and the reset's
// reason is decided in the conn.mu section that queues it (forward.go's
// queueReset), so a replacement always wins. One whose end a detach has
// claimed ends with that detach's `{}` instead, queued already or still to
// be (sessionDetach). A PENDING attachment (reserved, not yet answered:
// reserve installed it, but no forwarder runs for it and no detach can claim
// it) is owed neither, so it is abandoned outright, right here, and reserve
// refuses every later attach. The connection closes once that terminal line
// is written (replacedDoneLocked; astra r10 8), and at once when it owes none.
func (c *conn) replace() {
	c.ending.Store(true)
	c.mu.Lock()
	c.replaced = true
	dropped := c.out.replace()
	var sub *agent.Subscription
	var cancel func()
	if a := c.att; a != nil {
		switch a.state {
		case attPending:
			cancel = a.cancel
			a.state = attClosed
			a.changedLocked()
		case attLive, attClosing:
			// Its terminal line is still to come: the forwarder's
			// reset{session_replaced} (queueReset), or, if a detach has
			// claimed its end, that detach's `{}`.
			sub, cancel = a.sub, a.cancel
		}
	}
	c.mu.Unlock()
	if cancel != nil {
		// Ends a pending attach's wait, and a push its forwarder is blocked
		// in, so neither holds a terminal line back.
		cancel()
	}
	if sub != nil {
		sub.Close()
	}
	for _, ln := range dropped {
		if ln.done != nil {
			ln.done()
		}
	}
	c.settle()
}

// settle closes the connection if nothing is left of it: replaced, with its
// attachment's terminal acknowledgement written (replacedDoneLocked), or idle
// (idleLocked). Every change either predicate reads is followed by a settle.
func (c *conn) settle() {
	c.mu.Lock()
	reason := ""
	switch {
	case c.replacedDoneLocked():
		reason = "session replaced"
	case c.idleLocked():
		reason = "closed by the peer"
		if c.ended {
			reason = c.endReason
		}
	}
	c.mu.Unlock()
	if reason != "" {
		c.close(reason)
	}
}

// idleLocked is THE half-close predicate (§3.7, astra 11): the peer has
// half-closed — or the session has ended for this connection (end), which
// closes it by the same rule — nothing admitted is still unwritten (the
// reader's own slot aside, while it waits for a line nobody will admit), and
// no subscription is live.
func (c *conn) idleLocked() bool {
	if !c.readEOF && !c.ended {
		return false
	}
	pending := c.inflight
	if c.reading {
		pending--
	}
	return pending == 0 && !c.subscriptionLiveLocked()
}

// subscriptionLiveLocked reports a live subscription on the connection: an
// attachment that is not yet closed — pending (its attach not yet answered),
// live, or closing (§3.7's lifecycle) — or the terminal acknowledgement of one
// (its final reset, a detach's reply) still queued for the socket.
func (c *conn) subscriptionLiveLocked() bool {
	return (c.att != nil && c.att.state != attClosed) || c.unwritten > 0
}

// replacedDoneLocked reports a replaced connection that owes nothing more
// (plan 027 X25; astra r10 8): no attachment whose terminal line is still to
// be queued — a live or closing one, owed its forwarder's
// reset{session_replaced} or its claimed detach's `{}` — and no terminal line
// queued or being written (outbox.owesTerminal). No ordinary line holds it
// open: the replacement dropped every one still queued, and one the writer
// had already taken finishes ahead of the terminal line, or is cut by the
// close when none is owed.
func (c *conn) replacedDoneLocked() bool {
	return c.replaced && (c.att == nil || c.att.state == attClosed) && !c.out.owesTerminal()
}

// enqueue queues line for the writer in ONE conn.mu section with commit — the
// lifecycle step the line makes visible (an attachment made live by its attach
// reply, closed by its detach reply; a forwarder's position moved by an event)
// — so no holder of conn.mu ever sees the line queued without its step, or the
// step without its line (astra r8): a client that acts on a line the instant
// it reads it — detaches, re-attaches — finds the step already taken, and a
// reply barrier sees a position only once its event is queued. admit, checked
// first in the same section, may refuse the line: its error is returned. Room
// is never waited for under conn.mu: without it enqueue waits outside it
// (outbox.await) until room is made, ctx ends, or stop closes (errStopped,
// which wins over room), then tries again, admit included. kind is what line
// is to a replaced connection (plan 027 X25; the outbox applies it): a
// claimed detach's `{}` is terminalLine, every other line ordinaryLine.
func (c *conn) enqueue(ctx context.Context, stop <-chan struct{}, line []byte, done func(), admit func() error, commit func(), kind lineKind) error {
	for {
		var room <-chan struct{}
		var err error
		c.mu.Lock()
		if admit != nil {
			err = admit()
		}
		if err == nil {
			room, err = c.out.offer(line, done, ordinaryLimit, kind)
			if err == nil && commit != nil {
				commit()
			}
		}
		c.mu.Unlock()
		if !errors.Is(err, errNoRoom) {
			return err
		}
		if err := c.out.await(ctx, stop, room); err != nil {
			return err
		}
	}
}

// ------------------------------------------------------------------ writer

// writeChunk is how much of a line one socket write is handed, so the stall
// bound measures progress rather than the time a whole 16 MiB line takes.
const writeChunk = 64 << 10

// writeAttempts is how many attempts the stall bound is cut into: each socket
// write waits at most a writeAttempts'th of it (a second, of 60 s) before the
// writer looks at its progress again, so bytes that moved are noticed within
// one attempt of moving.
const writeAttempts = 60

// write is the connection's one writer: it drains the outbox in order and
// gives each line's bytes back to the budget once they are on the socket.
func (c *conn) write() {
	defer c.srv.transport.Done()
	for {
		ln, ok := c.out.next()
		if !ok {
			return
		}
		if h := c.srv.hooks.beforeWrite; h != nil {
			h(ln.b)
		}
		err := c.writeLine(ln.b)
		c.out.written(ln)
		if err != nil {
			// Closed for the write's own reason before the line's slot is
			// given back, which could otherwise close it as idle.
			c.close(err.Error())
		}
		if ln.done != nil {
			ln.done()
		}
		if err != nil {
			return
		}
	}
}

// errStalled is a write that made no progress for the stall bound.
var errStalled = errors.New("write stalled: the peer is not reading")

// writeLine writes b whole, or fails once no byte of it has moved for the
// stall bound — measured from the last byte that moved, never from the start
// of a write (astra r5): each socket write gets a short deadline (a
// writeAttempts'th of the bound, and never past the bound's end), the time
// of the last write that moved bytes is kept, and a write that times out
// closes the connection once the bound has passed since then — the session is
// untouched (§3.7). Progress is noticed when its write returns, so the close
// comes between the bound and the bound plus one attempt after the last byte
// moved (60–61 s in production).
//
// A deadline that cannot be set is a failed write (plan 027 X15, astra r6 5):
// nothing is written without one in place, since a write with none could
// block forever on a peer that stops reading, where the stall bound never
// looks. The connection closes as for any failed write.
func (c *conn) writeLine(b []byte) error {
	stall := c.srv.stall
	attempt := max(stall/writeAttempts, time.Millisecond)
	last := time.Now()
	for len(b) > 0 {
		chunk := b[:min(len(b), writeChunk)]
		deadline := time.Now().Add(attempt)
		if end := last.Add(stall); end.Before(deadline) {
			deadline = end
		}
		if err := c.nc.SetWriteDeadline(deadline); err != nil {
			return errors.New("write failed: its deadline could not be set: " + err.Error())
		}
		n, err := c.nc.Write(chunk)
		b = b[n:]
		if n > 0 {
			last = time.Now()
		}
		if err == nil {
			continue
		}
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			if time.Since(last) >= stall {
				return errStalled
			}
			continue
		}
		return errors.New("write failed: " + err.Error())
	}
	return nil
}

// ------------------------------------------------------------------- close

// close closes the connection once: every wait of its ends, the outbox drops
// what it holds, the socket closes (so the reader and writer return), its
// attachment's subscription is closed (so its forwarder returns: a transport
// close ends the subscription, §3.9), and then — on the goroutine that closed
// it, outside every lock — its client is released if the binding still names
// it (bind.go) and the close is noted.
func (c *conn) close(reason string) {
	first := false
	c.closeOnce.Do(func() {
		first = true
		c.closing.Store(true)
		c.cancel()
		c.out.close()
		_ = c.nc.Close()
	})
	if !first {
		return
	}
	// closing is set before this section, and an attach reads it after it
	// sets its subscription under c.mu (attach.go), so one of the two closes
	// the subscription.
	c.mu.Lock()
	var sub *agent.Subscription
	if a := c.att; a != nil {
		sub = a.sub
	}
	c.mu.Unlock()
	if sub != nil {
		sub.Close()
	}
	c.srv.unbind(c)
	c.srv.forget(c)
	fields := map[string]any{"event": "close", "reason": reason}
	if c.superseded.Load() {
		fields["superseded"] = true
	}
	c.srv.bindMu.Lock()
	if c.bound != nil {
		fields["clientId"] = c.bound.client
	}
	c.srv.bindMu.Unlock()
	c.srv.connNote(c.id, fields)
}

// ------------------------------------------------------------------ outbox

// outLine is one line queued for the writer, what it is to a replaced
// connection, and what to do once it is on the socket (or never will be).
type outLine struct {
	b    []byte
	kind lineKind
	done func()
}

// lineKind is what a queued line is to a replaced connection (plan 027 X25).
type lineKind uint8

const (
	// ordinaryLine is every line but a terminal one — a reply, an event, a
	// synchronized or a ready, a reset queued before the replacement: a
	// replaced connection writes none of them.
	ordinaryLine lineKind = iota
	// terminalLine is an attachment's terminal line that a replaced connection
	// still writes: its reset{session_replaced}, or a claimed detach's `{}`,
	// queued before the replacement or after it.
	terminalLine
)

// outbox is a connection's byte-counted FIFO (§3.7, astra 10). Every line
// queued counts against WriterQueueBytes until the writer has written it;
// ResetReserveBytes of that is held back for a final reset (offer with the
// whole budget, forward.go's queueReset), so a reset always fits; a line that
// fits an empty queue always gets in; and every wait ends when the connection
// closes.
//
// A REPLACED outbox (replace: its connection's engine was replaced, plan 027
// §3.6, X25) is TERMINAL-ONLY. Every ordinary line it held was dropped then,
// and every one offered since is refused (errOutboxReplaced), whoever offers
// it — a handler's reply included (astra r8 7). It admits a terminal line —
// the replacement's reset{session_replaced}, or a claimed detach's `{}` — and
// that line SEALS it: nothing more is admitted (errOutboxSealed). A terminal
// line it already held when it was replaced (a claimed detach's `{}`) is kept.
type outbox struct {
	mu     sync.Mutex
	q      []outLine
	bytes  int
	high   int
	closed bool
	// terminalOnly says the outbox was replaced; sealed, that a terminal
	// line has been admitted since.
	terminalOnly bool
	sealed       bool
	// terminals is how many terminal lines are queued or being written: what
	// a replaced connection still waits for (conn.replacedDoneLocked).
	terminals int
	// ready wakes the writer (one slot); room is closed and replaced whenever
	// bytes go down or the outbox closes, is replaced, or seals, waking
	// everyone waiting for room.
	ready chan struct{}
	room  chan struct{}
	// full is a test's barrier (hooks.outboxFull), nil in production.
	full func()
}

func newOutbox(full func()) *outbox {
	return &outbox{ready: make(chan struct{}, 1), room: make(chan struct{}), full: full}
}

var (
	// errOutboxClosed is a push to a closed connection: the line is dropped.
	errOutboxClosed = errors.New("control: the connection is closed")
	// errOutboxReplaced is an ordinary line offered to a replaced connection:
	// the line is dropped (plan 027 X25).
	errOutboxReplaced = errors.New("control: the connection was replaced: it writes only its terminal line")
	// errOutboxSealed is a push after a replaced connection's terminal line is
	// queued: the line is dropped.
	errOutboxSealed = errors.New("control: the connection's last line is queued")
	// errNoRoom is an offer the budget cannot take yet (offer).
	errNoRoom = errors.New("control: no room in the writer queue")
	// errStopped is a wait for room given up because its stop channel closed
	// (await).
	errStopped = errors.New("control: the wait for room was stopped")
	// errGone is a line an attachment may no longer queue (conn.enqueue's
	// admit): its connection is closing or replaced, or it was detached.
	errGone = errors.New("control: the attachment queues nothing more")
)

// ordinaryLimit is the budget every line but a final reset is held to: the
// whole of it less the reset reserve.
const ordinaryLimit = protocol.WriterQueueBytes - protocol.ResetReserveBytes

// offer queues b if it fits within limit, and NEVER WAITS, so it may be called
// under conn.mu (conn.enqueue, forward.go's queueReset: conn.mu → outbox.mu is
// the one lock edge). It is nil once b is queued; errNoRoom, with the channel
// closed when room is next made, when b does not fit; and when b is refused —
// the caller drops it, and gives back what it held for it — errOutboxClosed,
// errOutboxSealed, or errOutboxReplaced for an ordinary line once the outbox
// is terminal-only. A terminal line a terminal-only outbox admits seals it
// (plan 027 X25).
func (o *outbox) offer(b []byte, done func(), limit int, kind lineKind) (<-chan struct{}, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	switch {
	case o.closed:
		return nil, errOutboxClosed
	case o.sealed:
		return nil, errOutboxSealed
	case o.terminalOnly && kind != terminalLine:
		return nil, errOutboxReplaced
	case o.bytes != 0 && o.bytes+len(b) > limit:
		return o.room, errNoRoom
	}
	o.q = append(o.q, outLine{b: b, kind: kind, done: done})
	o.bytes += len(b)
	o.high = max(o.high, o.bytes)
	if kind == terminalLine {
		o.terminals++
		if o.terminalOnly {
			o.sealed = true
			o.wakeLocked()
		}
	}
	select {
	case o.ready <- struct{}{}:
	default:
	}
	return nil, nil
}

// replace makes the outbox terminal-only (plan 027 X25; conn.replace, under
// conn.mu, as offer may be): from here it admits only a terminal line, which
// seals it. Every ordinary line it holds is dropped and returned, in order,
// for the caller to run their callbacks outside its locks — a reply's gives
// its request's slot back — and a terminal one it holds (a claimed detach's
// `{}` queued before the replacement) is kept where it is. A line the writer
// has already taken is no longer the queue's: it finishes, ahead of any
// terminal line, or is cut by the connection's close when none is owed. Every
// wait for room is woken, to be refused or, for a terminal line, to find the
// room the drop made. A no-op once closed or already replaced.
func (o *outbox) replace() []outLine {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed || o.terminalOnly {
		return nil
	}
	o.terminalOnly = true
	var kept, dropped []outLine
	for _, ln := range o.q {
		if ln.kind == terminalLine {
			kept = append(kept, ln)
			continue
		}
		dropped = append(dropped, ln)
		o.bytes -= len(ln.b)
	}
	o.q = kept
	o.wakeLocked()
	return dropped
}

// owesTerminal reports a terminal line queued or being written. It may be
// called under conn.mu, as offer may (conn.replacedDoneLocked).
func (o *outbox) owesTerminal() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.terminals > 0
}

// push queues b, an ordinary line, within the ordinary budget, waiting —
// under no lock — for room. It fails, having queued nothing, once ctx ends or
// the outbox closes or is replaced.
func (o *outbox) push(ctx context.Context, b []byte, done func()) error {
	for {
		room, err := o.offer(b, done, ordinaryLimit, ordinaryLine)
		if !errors.Is(err, errNoRoom) {
			return err
		}
		if err := o.await(ctx, nil, room); err != nil {
			return err
		}
	}
}

// await waits, after an offer that found no room, until room is made (and the
// caller offers again), ctx ends, or stop closes (errStopped). A stop already
// closed never waits; a nil one never ends the wait. Stop wins over room: a
// wait that finds room made and stop closed — both ready at once, of which
// select picks either — is errStopped, so a line whose stop has closed by the
// time its wait ends is not offered again (a forwarder's blocked event once
// its subscription has ended, astra r10 4). It holds no lock.
func (o *outbox) await(ctx context.Context, stop, room <-chan struct{}) error {
	if isClosed(stop) {
		return errStopped
	}
	if o.full != nil {
		o.full()
	}
	select {
	case <-room:
		if isClosed(stop) {
			return errStopped
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-stop:
		return errStopped
	}
}

// isClosed reports whether ch has closed, without waiting; never for nil.
func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// wakeLocked wakes every wait for room; o.mu is held.
func (o *outbox) wakeLocked() {
	close(o.room)
	o.room = make(chan struct{})
}

// next is the line at the head, waiting for one; false once closed.
func (o *outbox) next() (outLine, bool) {
	for {
		o.mu.Lock()
		if o.closed {
			o.mu.Unlock()
			return outLine{}, false
		}
		if len(o.q) > 0 {
			ln := o.q[0]
			o.q[0] = outLine{}
			o.q = o.q[1:]
			o.mu.Unlock()
			return ln, true
		}
		o.mu.Unlock()
		<-o.ready
	}
}

// written gives ln's bytes back to the budget: the line the writer took is on
// the socket, or its write failed.
func (o *outbox) written(ln outLine) {
	o.mu.Lock()
	o.bytes -= len(ln.b)
	if ln.kind == terminalLine {
		o.terminals--
	}
	o.wakeLocked()
	o.mu.Unlock()
}

// close drops everything queued and ends every wait.
func (o *outbox) close() {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return
	}
	o.closed = true
	for _, ln := range o.q {
		if ln.kind == terminalLine {
			o.terminals--
		}
	}
	o.q = nil
	o.wakeLocked()
	select {
	case o.ready <- struct{}{}:
	default:
	}
}

// highWater is the most bytes the outbox has held at once.
func (o *outbox) highWater() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.high
}
