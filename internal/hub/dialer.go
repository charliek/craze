package hub

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/rundir"
)

// Dialer is an Options.Dial for internal/remote (plan 032 §3.8): the dial of
// a client that reaches a session through the hub (remote.Options.Connect,
// the hub's socket as its path), which brings the hub back when it is gone.
// A remote client never spawns anything — it redials the same path, three
// times within ten seconds — so a hub that died under a spliced client (a
// crash, a kill -9, an idle exit racing the client's dial) is this dial's to
// replace, inside that window: the respawned hub resolves the session from
// the registry at once (session.connect), and the client says its resume
// hello to the same host it left.
//
// Each dial connects to path, past a full listen backlog. A socket that
// refuses or is absent — a dead hub's, or none — is no hub: the dial runs
// Ensure, which finds the hub serving the namespace now or starts one, and
// connects to the socket it answers. The connection it returns does the same
// on EOF (or a reset, or a broken pipe) before the first byte of the hub's
// first reply — a hub that closed the connection unanswered, its idle decision
// or its teardown having come first: Ensure, a connection to the hub it
// answers, everything written so far written again there, once, and the
// caller reads on as if nothing had happened. A hub that answered is the
// hub; what happens after its first reply is the caller's.
//
// Ensure is never unbounded (it loops while a hub is leaving): its deadline is
// the dial context's — remote gives a redial's the reconnect episode's end, so
// the respawn takes what is left of the episode's window — and for the EOF
// before the first reply the connection's read deadline, which the client's
// handshake sets to the same end; with neither, DialerBudget from now.
//
// Every connection it makes is peer-checked here (rundir.DialCheck: the hub
// runs as this user), the respawned one included: the client's own
// Options.PeerCheck must be nil, since what it is handed is not a
// *net.UnixConn. Ensure's rules hold — in a Go test binary it spawns no hub
// unless the test installs Command, so a dial there that finds no hub fails
// at once.
func Dialer(env rundir.Env) func(ctx context.Context, path string) (net.Conn, error) {
	d := &dialer{env: env}
	return d.dial
}

// DialerBudget bounds a Dialer's Ensure when nothing else does: 10 s, a
// reconnect episode's default window.
const DialerBudget = 10 * time.Second

// The Dialer's seams: variables only so a test can replace them (never in
// parallel).
var (
	// dialerEnsure is Ensure: a test watches the bound it is given, or
	// answers a hub of its own.
	dialerEnsure = Ensure
	// dialerDial is a Dialer's connect.
	dialerDial = func(ctx context.Context, path string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", path)
	}
)

// sentMax bounds what a Dialer's connection keeps of what was written before
// the hub's first reply, to write again to a respawned hub: far more than the
// hello it is in practice. More than that, and no respawn is tried.
const sentMax = 64 << 10

// dialer is one Dialer's.
type dialer struct {
	env rundir.Env
}

// dial is the Dialer (its comment).
func (d *dialer) dial(ctx context.Context, path string) (net.Conn, error) {
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(DialerBudget)
	}
	dctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	uc, err := d.connect(dctx, path)
	if err == nil {
		return newDialerConn(d, uc), nil
	}
	if !noHub(err) {
		return nil, err
	}
	uc, rerr := d.respawn(dctx, deadline)
	if rerr != nil {
		return nil, fmt.Errorf("hub: %s: %w, and no hub could be found or started: %w", path, err, rerr)
	}
	return newDialerConn(d, uc), nil
}

// respawn is Ensure within deadline (and ctx), and a connection to the
// socket it answers.
func (d *dialer) respawn(ctx context.Context, deadline time.Time) (*net.UnixConn, error) {
	ectx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	sock, err := dialerEnsure(ectx, d.env, protocol.ConnectionCapabilities{Connect: true})
	if err != nil {
		return nil, err
	}
	return d.connect(ectx, sock)
}

// connect dials socket, past a full backlog, and checks its peer.
func (d *dialer) connect(ctx context.Context, socket string) (*net.UnixConn, error) {
	nc, err := dialPastBacklog(ctx, dialerDial, socket)
	if err != nil {
		return nil, err
	}
	uc, ok := nc.(*net.UnixConn)
	if !ok {
		_ = nc.Close()
		return nil, errors.New("hub: not a unix socket connection")
	}
	if err := rundir.DialCheck(os.Geteuid())(uc); err != nil {
		_ = uc.Close()
		return nil, fmt.Errorf("hub: the socket %s: %w", socket, err)
	}
	return uc, nil
}

// noHub says a dial's error is a socket with no hub behind it: refused, or
// absent.
func noHub(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ENOENT)
}

// unanswered says a read's or a write's error is the hub having closed the
// connection: EOF, a reset, a broken pipe.
func unanswered(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE)
}

// dialerConn is a Dialer's connection (Dialer's comment): the hub's, which
// one respawn may replace until the first byte of a reply has been read.
type dialerConn struct {
	d *dialer
	// ctx ends with Close: a respawn under way ends with it.
	ctx    context.Context
	cancel context.CancelFunc

	// wmu orders every write, and a respawn's swap, against each other.
	wmu sync.Mutex

	mu sync.Mutex
	nc *net.UnixConn
	// sent is what was written before a reply's first byte, to write again
	// to a respawned hub; over says it outgrew sentMax.
	sent []byte
	over bool
	// replied: a byte has been read; respawned: the one respawn was tried.
	replied, respawned, closed bool
	// rdl and wdl are the deadlines set, carried to a respawned hub's
	// connection.
	rdl, wdl time.Time
}

func newDialerConn(d *dialer, uc *net.UnixConn) *dialerConn {
	ctx, cancel := context.WithCancel(context.Background())
	return &dialerConn{d: d, ctx: ctx, cancel: cancel, nc: uc}
}

func (c *dialerConn) current() *net.UnixConn {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.nc
}

// Read reads the hub's connection: an EOF, a reset or a broken pipe before
// any byte has been read is the respawn's, once.
func (c *dialerConn) Read(p []byte) (int, error) {
	for {
		nc := c.current()
		n, err := nc.Read(p)
		if n > 0 {
			c.mu.Lock()
			c.replied, c.sent = true, nil
			c.mu.Unlock()
			return n, err
		}
		if err == nil || !unanswered(err) || !c.unreplied() {
			return n, err
		}
		c.wmu.Lock()
		rerr := c.respawnLocked(nc)
		c.wmu.Unlock()
		if rerr != nil {
			return n, err
		}
	}
}

// unreplied says no byte of a reply has been read, and no respawn tried.
func (c *dialerConn) unreplied() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.replied && !c.respawned
}

// Write writes the hub's connection, keeping what it wrote until a reply's
// first byte has been read: a write that meets the hub gone before then is the
// respawn's, once, and the rest of p goes to the respawned hub.
func (c *dialerConn) Write(p []byte) (int, error) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	nc := c.current()
	n, err := nc.Write(p)
	c.keep(p[:n])
	if err == nil || !unanswered(err) {
		return n, err
	}
	if c.respawnLocked(nc) != nil {
		return n, err
	}
	m, err := c.current().Write(p[n:])
	c.keep(p[n : n+m])
	return n + m, err
}

// keep notes b as written, while no reply has been read.
func (c *dialerConn) keep(b []byte) {
	if len(b) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case c.replied:
	case len(c.sent)+len(b) > sentMax:
		c.over, c.sent = true, nil
	default:
		c.sent = append(c.sent, b...)
	}
}

// respawnLocked replaces old — the hub's connection that ended unanswered —
// with one to the hub Ensure answers, and writes it what was written to old:
// nil once the connection is the new one. Only once, only before a reply, and
// never after Close; wmu is held.
func (c *dialerConn) respawnLocked(old *net.UnixConn) error {
	c.mu.Lock()
	if c.closed || c.replied || c.respawned || c.over || c.nc != old {
		c.mu.Unlock()
		return errors.New("hub: no respawn")
	}
	c.respawned = true
	sent := append([]byte(nil), c.sent...)
	rdl, wdl := c.rdl, c.wdl
	c.mu.Unlock()
	deadline := rdl
	if deadline.IsZero() {
		deadline = time.Now().Add(DialerBudget)
	}
	uc, err := c.d.respawn(c.ctx, deadline)
	if err != nil {
		return err
	}
	_ = uc.SetReadDeadline(rdl)
	_ = uc.SetWriteDeadline(wdl)
	if len(sent) > 0 {
		if _, err := uc.Write(sent); err != nil {
			_ = uc.Close()
			return err
		}
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		_ = uc.Close()
		return net.ErrClosed
	}
	c.nc = uc
	c.mu.Unlock()
	_ = old.Close()
	return nil
}

// Close closes the connection, and ends a respawn under way.
func (c *dialerConn) Close() error {
	c.mu.Lock()
	c.closed = true
	nc := c.nc
	c.mu.Unlock()
	c.cancel()
	return nc.Close()
}

func (c *dialerConn) LocalAddr() net.Addr  { return c.current().LocalAddr() }
func (c *dialerConn) RemoteAddr() net.Addr { return c.current().RemoteAddr() }

func (c *dialerConn) SetDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rdl, c.wdl = t, t
	return c.nc.SetDeadline(t)
}

func (c *dialerConn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rdl = t
	return c.nc.SetReadDeadline(t)
}

func (c *dialerConn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.wdl = t
	return c.nc.SetWriteDeadline(t)
}
