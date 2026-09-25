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
	// accepted is every connection this listener has ever accepted,
	// monotonic — incremented in the same critical section Accept uses to
	// add the connection to conns, so a snapshot of it taken under the same
	// lock (dropAll) is consistent with which connections that snapshot's
	// conns actually holds. DropConnections' wait uses it to tell a
	// connection accepted after a drop (a reconnecting client) from one the
	// drop is actually responsible for closing.
	accepted int64
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
	l.accepted++
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
// (Write's closed case) instead of sleeping out whatever stall remains.
// Host.DropConnections waits on control.Server.OpenConns falling to the
// number of connections accepted after the snapshot this returns —
// proof every dropped connection's cleanup, unbind included, has actually
// run, not just that its socket is gone — rather than on any count of
// closed connections this call itself makes, since a connection open when
// dropAll ran can finish closing on its own and be mistaken for one of
// these (C9a review item 2). The snapshot is taken under the same lock that
// clears conns, so it is exactly "every connection accepted up to and
// including this call" — a connection accepted after this point (a client
// that reconnects while DropConnections is still waiting, C9b) is never one
// this call is responsible for, and must not be dropped or waited on.
func (l *stallListener) dropAll() int64 {
	l.mu.Lock()
	conns := make([]*stallConn, 0, len(l.conns))
	for c := range l.conns {
		conns = append(conns, c)
	}
	l.conns = map[*stallConn]struct{}{}
	snapshot := l.accepted
	l.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
	return snapshot
}

// acceptedTotal is the number of connections this listener has ever
// accepted, monotonic. DropConnections re-reads it during its wait: the
// difference between this and dropAll's snapshot is how many connections
// have been accepted since the drop — reconnects the drop is not
// responsible for and must not wait on.
func (l *stallListener) acceptedTotal() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.accepted
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
