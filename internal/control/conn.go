package control

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

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
	// connection's client to another connection.
	closing    atomic.Bool
	superseded atomic.Bool
	closeOnce  sync.Once

	// bound is the connection's hello, guarded by Server.bindMu: written once,
	// by the reader, in the binding section; the reader reads it after, and a
	// handler is handed it.
	bound *bound

	mu sync.Mutex
	// inflight is how many requests are admitted and their replies not yet
	// written; readEOF says the peer has half-closed (no more requests).
	inflight int
	readEOF  bool
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
		line, err := lr.ReadLine()
		switch {
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
// once the connection has closed.
func (c *conn) acquire() bool {
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
	return true
}

// release gives a slot back: its reply is written, or will never be. The
// connection closes here if the peer has half-closed and nothing is left.
func (c *conn) release() {
	<-c.slots
	c.mu.Lock()
	c.inflight--
	idle := c.idleLocked()
	c.mu.Unlock()
	if idle {
		c.close("closed by the peer")
	}
}

// eof marks the read side done and closes the connection if nothing is in
// flight.
func (c *conn) eof() {
	c.mu.Lock()
	c.readEOF = true
	idle := c.idleLocked()
	c.mu.Unlock()
	if idle {
		c.close("closed by the peer")
	}
}

// idleLocked is THE half-close predicate (§3.7, astra 11): the peer has
// half-closed, nothing admitted is still unwritten, and no subscription is
// live. C7 adds its attachment to the last clause, here and nowhere else.
func (c *conn) idleLocked() bool {
	return c.readEOF && c.inflight == 0 && !c.subscriptionLiveLocked()
}

// subscriptionLiveLocked reports a live subscription on the connection: none
// before C7, which adds attach.
func (c *conn) subscriptionLiveLocked() bool { return false }

// ------------------------------------------------------------------ writer

// writeChunk is how much of a line one socket write is handed, so the stall
// bound measures progress rather than the time a whole 16 MiB line takes.
const writeChunk = 64 << 10

// write is the connection's one writer: it drains the outbox in order and
// gives each line's bytes back to the budget once they are on the socket.
func (c *conn) write() {
	defer c.srv.transport.Done()
	for {
		ln, ok := c.out.next()
		if !ok {
			return
		}
		err := c.writeLine(ln.b)
		c.out.written(len(ln.b))
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

// writeLine writes b whole. Each chunk's write gets a fresh deadline of the
// stall bound; a deadline that passes having written part of a chunk is
// progress and gets another, and one that passes having written nothing
// closes the connection — the session is untouched (§3.7).
func (c *conn) writeLine(b []byte) error {
	for len(b) > 0 {
		chunk := b[:min(len(b), writeChunk)]
		_ = c.nc.SetWriteDeadline(time.Now().Add(c.srv.stall))
		n, err := c.nc.Write(chunk)
		b = b[n:]
		if err == nil {
			continue
		}
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			if n > 0 {
				continue
			}
			return errStalled
		}
		return errors.New("write failed: " + err.Error())
	}
	return nil
}

// ------------------------------------------------------------------- close

// close closes the connection once: every wait of its ends, the outbox drops
// what it holds, the socket closes (so the reader and writer return), and
// then — on the goroutine that closed it, outside every lock — its client is
// released if the binding still names it (bind.go) and the close is noted.
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

// outLine is one line queued for the writer, and what to do once it is on
// the socket (or never will be).
type outLine struct {
	b    []byte
	done func()
}

// outbox is a connection's byte-counted FIFO (§3.7, astra 10). Every line
// queued counts against WriterQueueBytes until the writer has written it;
// ResetReserveBytes of that is held back for a final reset (pushReserved,
// C7), so a reset always fits; a line that fits an empty queue always gets
// in; and every wait ends when the connection closes.
type outbox struct {
	mu     sync.Mutex
	q      []outLine
	bytes  int
	high   int
	closed bool
	// ready wakes the writer (one slot); room is closed and replaced whenever
	// bytes go down or the outbox closes, waking everyone waiting for room.
	ready chan struct{}
	room  chan struct{}
	// full is a test's barrier (hooks.outboxFull), nil in production.
	full func()
}

func newOutbox(full func()) *outbox {
	return &outbox{ready: make(chan struct{}, 1), room: make(chan struct{}), full: full}
}

// errOutboxClosed is a push to a closed connection: the line is dropped.
var errOutboxClosed = errors.New("control: the connection is closed")

// push queues b, waiting for room within the budget less the reset reserve.
// It fails, having queued nothing, once ctx ends or the outbox closes.
func (o *outbox) push(ctx context.Context, b []byte, done func()) error {
	return o.pushWithin(ctx, b, done, protocol.WriterQueueBytes-protocol.ResetReserveBytes)
}

// pushReserved queues b within the whole budget, the reset reserve included:
// a subscription's final reset (C7), which is at most ResetReserveBytes and so
// always fits beside everything push lets in.
func (o *outbox) pushReserved(ctx context.Context, b []byte, done func()) error {
	return o.pushWithin(ctx, b, done, protocol.WriterQueueBytes)
}

func (o *outbox) pushWithin(ctx context.Context, b []byte, done func(), limit int) error {
	for {
		o.mu.Lock()
		if o.closed {
			o.mu.Unlock()
			return errOutboxClosed
		}
		if o.bytes == 0 || o.bytes+len(b) <= limit {
			o.q = append(o.q, outLine{b: b, done: done})
			o.bytes += len(b)
			o.high = max(o.high, o.bytes)
			o.mu.Unlock()
			select {
			case o.ready <- struct{}{}:
			default:
			}
			return nil
		}
		room := o.room
		o.mu.Unlock()
		if o.full != nil {
			o.full()
		}
		select {
		case <-room:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
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

// written gives n bytes back to the budget: a line is on the socket, or the
// write failed.
func (o *outbox) written(n int) {
	o.mu.Lock()
	o.bytes -= n
	close(o.room)
	o.room = make(chan struct{})
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
	o.q = nil
	close(o.room)
	o.room = make(chan struct{})
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
