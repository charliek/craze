package fakehost

import (
	"net"
	"sync"
	"time"
)

// stallListener wraps a net.Listener so the Host can hold up every byte its
// server writes (StallWrites) or drop every connection it has accepted so far,
// behind an accept gate that holds any redial until the drop is over
// (DropConnections) — the two seams the wire fixtures need for a
// slow_consumer reset and a retired client, from the host side, with no
// assertion ever made on a duration (plan 027 §3.11's determinism; see
// doc.go). It never touches a connection the fixture runner itself dials —
// only the ones this server accepts.
type stallListener struct {
	net.Listener

	mu    sync.Mutex
	until time.Time
	// wake is closed, and replaced, every time until changes (stall or
	// resume): a Write already asleep on the old channel wakes at once and
	// re-reads until, rather than sleeping out a stall duration nothing
	// still wants — resume's whole point.
	wake  chan struct{}
	conns map[*stallConn]struct{}

	// shut is the accept gate (DropConnections): non-nil while the gate is
	// shut, and closed when it reopens. While it is shut, a connection the
	// wrapped listener accepts is held in Accept — never handed to the
	// server, never in conns — until it reopens: a client that dials during a
	// drop waits, exactly as it would in the kernel's backlog, and the server
	// counts nothing new until the drop is over.
	shut chan struct{}
	// held is how many connections Accept is holding at the shut gate, and
	// onHeld (a test's seam, host_test.go) runs each time one is held.
	held   int
	onHeld func()
	// handedOff is true from the moment Accept hands a connection to the
	// server until the server's accept loop calls Accept again. The loop
	// (control.Server.Serve) registers each connection it is handed — its
	// OpenConns count — before it asks for the next one, on the same
	// goroutine, so false means every connection this listener ever handed
	// over is already counted by OpenConns, and with the gate shut none can be
	// handed over next. handoffPending reads it for DropConnections' wait.
	handedOff bool
	// closed is closed by Close, so a connection held at the shut gate is
	// dropped then instead of waiting on a gate nothing will reopen.
	closed    chan struct{}
	closeOnce sync.Once
}

func newStallListener(l net.Listener) *stallListener {
	return &stallListener{
		Listener: l,
		wake:     make(chan struct{}),
		conns:    map[*stallConn]struct{}{},
		closed:   make(chan struct{}),
	}
}

// Accept is the wrapped listener's, with the gate between it and the server:
// a connection accepted while the gate is shut is handed over only once it
// reopens (or dropped, if the listener closes first).
func (l *stallListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	// The server is back for another connection: the last one handed over is
	// registered (handedOff's comment).
	l.handedOff = false
	l.mu.Unlock()
	nc, err := l.Listener.Accept()
	if err != nil {
		return nc, err
	}
	c := &stallConn{Conn: nc, l: l, closed: make(chan struct{})}
	for {
		l.mu.Lock()
		shut := l.shut
		if shut == nil {
			l.conns[c] = struct{}{}
			l.handedOff = true
			l.mu.Unlock()
			return c, nil
		}
		l.held++
		onHeld := l.onHeld
		l.mu.Unlock()
		if onHeld != nil {
			onHeld()
		}
		select {
		case <-shut:
			l.mu.Lock()
			l.held--
			l.mu.Unlock()
		case <-l.closed:
			l.mu.Lock()
			l.held--
			l.mu.Unlock()
			_ = nc.Close()
			return nil, net.ErrClosed
		}
	}
}

// Close closes the wrapped listener and drops a connection held at the shut
// gate (Accept), so control.Server.Close never waits on a drop's gate.
func (l *stallListener) Close() error {
	l.closeOnce.Do(func() { close(l.closed) })
	return l.Listener.Close()
}

// shutGate shuts the accept gate: from here until openGate, no connection is
// handed to the server (Accept). Idempotent.
func (l *stallListener) shutGate() {
	l.mu.Lock()
	if l.shut == nil {
		l.shut = make(chan struct{})
	}
	l.mu.Unlock()
}

// openGate reopens it, handing over whatever Accept held meanwhile, one at a
// time as the server asks. Idempotent.
func (l *stallListener) openGate() {
	l.mu.Lock()
	if l.shut != nil {
		close(l.shut)
		l.shut = nil
	}
	l.mu.Unlock()
}

// handoffPending reports whether a connection has been handed to the server
// that its OpenConns may not count yet (handedOff).
func (l *stallListener) handoffPending() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.handedOff
}

// heldAtGate is how many connections Accept is holding at the shut gate.
func (l *stallListener) heldAtGate() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.held
}

// setOnHeld installs a test's onHeld seam; set before the gate shuts.
func (l *stallListener) setOnHeld(f func()) {
	l.mu.Lock()
	l.onHeld = f
	l.mu.Unlock()
}

// stall holds up every Write on every connection this listener has accepted,
// present and future, until d has passed — long enough for a script to push
// events past a small subscription budget before any of them reaches the
// wire.
func (l *stallListener) stall(d time.Duration) {
	l.mu.Lock()
	l.until = time.Now().Add(d)
	close(l.wake)
	l.wake = make(chan struct{})
	l.mu.Unlock()
}

// resume clears an active stall at once: a Write already asleep on the old
// stall wakes immediately (the closed channel below) instead of sleeping out
// whatever duration StallWrites was given, and the next Write goes straight
// through.
func (l *stallListener) resume() {
	l.mu.Lock()
	l.until = time.Time{}
	close(l.wake)
	l.wake = make(chan struct{})
	l.mu.Unlock()
}

// state is the current stall deadline and the channel that closes the moment
// it changes.
func (l *stallListener) state() (time.Time, chan struct{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.until, l.wake
}

// dropAll closes every connection accepted so far, as a network drop would:
// no detach, no FIN the server's reader can see as a half-close — just gone.
// The set is cleared first, so a later dropAll only touches what is live
// then. It closes each through the stallConn wrapper (Close), not the raw
// net.Conn, so a write of its own stalled on this listener wakes at once
// (Write's closed case) instead of sleeping out whatever stall remains.
// Host.DropConnections calls it with the accept gate shut, so the set it
// closes is every connection the server has been handed, and none can join
// it until the gate reopens.
func (l *stallListener) dropAll() {
	l.mu.Lock()
	conns := make([]*stallConn, 0, len(l.conns))
	for c := range l.conns {
		conns = append(conns, c)
	}
	l.conns = map[*stallConn]struct{}{}
	l.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
}

func (l *stallListener) forget(c *stallConn) {
	l.mu.Lock()
	delete(l.conns, c)
	l.mu.Unlock()
}

// stallConn is one accepted connection: an ordinary net.Conn (every method
// but Write and Close promoted) whose Write waits out its listener's stall,
// if one is active, before writing a byte — so a whole line the server's
// writer goroutine sends is delayed as one, never split mid-line. closed is
// closed once, by Close, so a Write already waiting out a stall wakes at
// once when the connection closes, rather than only at the stall's deadline
// or a resume (fixture: stall_writes then quit, C9a review item 5).
type stallConn struct {
	net.Conn
	l *stallListener

	closeOnce sync.Once
	closed    chan struct{}
}

func (c *stallConn) Write(b []byte) (int, error) {
	for {
		until, wake := c.l.state()
		if until.IsZero() {
			break
		}
		d := time.Until(until)
		if d <= 0 {
			break
		}
		timer := time.NewTimer(d)
		select {
		case <-wake:
			timer.Stop()
			// Re-check: a stall (not a resume) may have replaced it with a
			// new, later deadline.
		case <-timer.C:
		case <-c.closed:
			timer.Stop()
			// The connection is closing: writing now fails at once (the
			// socket is already closed, or closing), instead of waiting out
			// a stall nothing still wants — a wedged host.Quit's whole point.
			return c.Conn.Write(b)
		}
	}
	return c.Conn.Write(b)
}

func (c *stallConn) Close() error {
	c.l.forget(c)
	c.closeOnce.Do(func() { close(c.closed) })
	return c.Conn.Close()
}
