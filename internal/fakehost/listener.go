package fakehost

import (
	"net"
	"sync"
	"time"
)

// stallListener wraps a net.Listener so the Host can hold up every byte its
// server writes (StallWrites) or drop every connection it has accepted so far
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
}

func newStallListener(l net.Listener) *stallListener {
	return &stallListener{Listener: l, wake: make(chan struct{}), conns: map[*stallConn]struct{}{}}
}

func (l *stallListener) Accept() (net.Conn, error) {
	nc, err := l.Listener.Accept()
	if err != nil {
		return nc, err
	}
	c := &stallConn{Conn: nc, l: l, closed: make(chan struct{})}
	l.mu.Lock()
	l.conns[c] = struct{}{}
	l.mu.Unlock()
	return c, nil
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
// (Write's closed case) instead of sleeping out whatever stall remains; the
// count returned is how many it closed, for a caller (Host.DropConnections)
// that waits for the server's own count of open connections to fall by that
// many — proof its cleanup, unbind included, has actually run for each one,
// not just that the socket is gone.
func (l *stallListener) dropAll() int {
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
	return len(conns)
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
