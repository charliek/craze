package control

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/journal"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/version"
)

// Options configure a Server. The zero value serves a host with a fresh
// hostId, this build's version and this process's pid.
type Options struct {
	// Log receives one line per connection opened or closed, and why it
	// ended; nil logs nothing. It is never handed anything a client sent, and
	// never a token. The same facts go to the session's journal as a diag
	// note (journal.DiagControlConn).
	Log func(string)
	// PeerCheck is run on every accepted connection before a byte of it is
	// read (plan 027 §3.8; internal/rundir installs one in PR 2): an error
	// closes the connection and is noted. nil checks nothing — PR 1's tests.
	// A connection that is not a *net.UnixConn fails a non-nil check.
	PeerCheck func(*net.UnixConn) error
	// MaxBudget is the largest subscription budget a client may ask an attach
	// for (C7). The zero value is DefaultMaxBudget: four times the event
	// log's subscription defaults.
	MaxBudget Budget

	// The host's identity, as hello's endpoint and the info document carry
	// it. They exist as options so a deterministic host (the fake host and the
	// wire fixtures, plan 027 §3.11) can pin them; a real host leaves them
	// zero.
	//
	// HostID is the host's id: 12 hex digits, and sessions.list's epoch. ""
	// mints one from Tokens.
	HostID string
	// CrazeVersion is hello's endpoint.crazeVersion; "" is version.Version.
	CrazeVersion string
	// PID is hello's endpoint.pid; 0 is os.Getpid().
	PID int
	// Workspace is the session's working directory, the info document's
	// workspace.
	Workspace string
	// Tokens is where resume tokens (128 bits each) and a minted HostID come
	// from; nil is crypto/rand. A fixture's source is deterministic.
	Tokens io.Reader
}

// Budget is a subscription budget: the event log's SubscribeOptions MaxItems
// and MaxBytes.
type Budget struct {
	MaxItems int
	MaxBytes int
}

// DefaultMaxBudget is Options.MaxBudget's default: four times the event log's
// subscription defaults, 1,024 records and 8 MiB (plan 027 §3.7).
var DefaultMaxBudget = Budget{MaxItems: 4 * 1024, MaxBytes: 4 * (8 << 20)}

// The server's own bounds.
const (
	// writeStall is how long a writer may make no progress before its
	// connection is closed (§3.7).
	writeStall = 60 * time.Second
	// commandTimeout bounds each blocking command — Interject, Cancel and Set
	// — on its server-owned context (§3.6). The non-waiting verbs take none.
	commandTimeout = 30 * time.Second
)

// Server is the control socket's server: one session (SetEngine), any number
// of listeners (Serve) and connections. See the package doc for the roles,
// locks and lifecycle.
type Server struct {
	opts      Options
	hostID    string
	version   string
	pid       int
	maxBudget Budget

	// Bounds a test may lower (export_test.go); fixed before Serve.
	stall   time.Duration
	maxLine int
	hooks   hooks

	// bindMu guards the engine, the binding table, the token index and every
	// conn's bound state (bind.go). A leaf above the receipts table's.
	bindMu sync.Mutex
	eng    *engine.Engine
	// crazeID and incarnation are the engine's durable session id and its
	// log's incarnation, read once when it is set.
	crazeID     string
	incarnation string
	binds       map[string]*binding
	// tokens indexes every resume token this server has issued and still
	// remembers — the current engine's and the previous one's — by token, to
	// the client id it names and the incarnation it was issued in (bind.go).
	tokens map[string]tokenOwner

	connMu    sync.Mutex
	conns     map[*conn]struct{}
	listeners map[net.Listener]struct{}
	closed    bool
	nextConn  uint64

	// transport counts the goroutines Close joins: accept loops, readers and
	// writers. handlers counts the ones it does not (package doc, "Close").
	transport sync.WaitGroup
	handlers  counter
	// commands is how many mutating commands are in their engine call across
	// every connection: the host-wide cap (CommandsPerHost).
	commands atomic.Int64

	tokenMu sync.Mutex
}

// hooks are test barriers, nil in production and set before Serve
// (export_test.go).
type hooks struct {
	// admissionFull runs on a reader that found every admission slot taken,
	// just before it waits for one.
	admissionFull func()
	// beforeUnbind runs on a closing connection's cleanup before its
	// compare-and-release, with the client id it is bound to ("" for none).
	beforeUnbind func(client string)
	// beforeBind runs in hello before the binding section (bindMu).
	beforeBind func()
	// beforeBarrier runs on a handler after its command returned and before
	// the reply barrier's SyncSeq, with the method.
	beforeBarrier func(method string)
	// outboxFull runs on a push that found no room in the budget, just before
	// it waits for some.
	outboxFull func()
}

// New builds a server. It serves nothing until SetEngine and Serve.
func New(o Options) *Server {
	s := &Server{
		opts:      o,
		hostID:    o.HostID,
		version:   o.CrazeVersion,
		pid:       o.PID,
		maxBudget: o.MaxBudget,
		stall:     writeStall,
		maxLine:   protocol.OutboundLineMax,
		binds:     map[string]*binding{},
		tokens:    map[string]tokenOwner{},
		conns:     map[*conn]struct{}{},
		listeners: map[net.Listener]struct{}{},
	}
	if s.opts.Tokens == nil {
		s.opts.Tokens = rand.Reader
	}
	if s.hostID == "" {
		b := make([]byte, 6)
		if err := s.readTokens(b); err != nil {
			panic(fmt.Sprintf("control: minting a host id: %v", err))
		}
		s.hostID = hex.EncodeToString(b)
	}
	if s.version == "" {
		s.version = version.Version
	}
	if s.pid == 0 {
		s.pid = os.Getpid()
	}
	if s.maxBudget == (Budget{}) {
		s.maxBudget = DefaultMaxBudget
	}
	return s
}

// HostID is the host's id: hello's endpoint.hostId and sessions.list's epoch.
func (s *Server) HostID() string { return s.hostID }

// MaxBudget is the largest subscription budget an attach may ask for.
func (s *Server) MaxBudget() Budget { return s.maxBudget }

// readTokens fills b from the token source, one reader at a time (a
// deterministic source need not be safe for concurrent use).
func (s *Server) readTokens(b []byte) error {
	s.tokenMu.Lock()
	defer s.tokenMu.Unlock()
	_, err := io.ReadFull(s.opts.Tokens, b)
	return err
}

// newToken is a resume token: 128 random bits, hex (§3.6). It is kept in the
// binding table and the token index alone, and never logged.
func (s *Server) newToken() (string, error) {
	b := make([]byte, 16)
	if err := s.readTokens(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// SetEngine sets the one session the host serves. Until it is called, hello
// answers unavailable, reason not_ready, and nothing else is reachable.
//
// Replacing the engine (a second call with another engine, or nil) closes
// EVERY connection and clears EVERY binding (§3.6, astra 14): tokens and
// client ids are scoped to one engine incarnation, and a resume presenting a
// token of another incarnation is answered resumed: false. The replaced
// engine's tokens stay in the index as the PREVIOUS generation, and anything
// older is dropped (bind.go, "The token index"). A handler already running
// keeps the engine it captured at dispatch, and its reply goes nowhere. (C7
// sends every attached connection reset{session_replaced} first.)
func (s *Server) SetEngine(e *engine.Engine) {
	var crazeID, incarnation string
	if e != nil {
		st := e.State()
		crazeID, incarnation = st.CrazeSessionID, st.Incarnation
	}
	s.bindMu.Lock()
	if s.eng == e {
		s.bindMu.Unlock()
		return
	}
	replaced := s.eng != nil
	outgoing := s.incarnation
	s.eng, s.crazeID, s.incarnation = e, crazeID, incarnation
	if replaced {
		s.binds = map[string]*binding{}
		for tok, o := range s.tokens {
			if o.incarnation != outgoing {
				delete(s.tokens, tok)
			}
		}
	}
	s.bindMu.Unlock()
	if !replaced && e != nil {
		return
	}
	for _, c := range s.liveConns() {
		c.close("session replaced")
	}
}

// engine is the session the host serves now, and its durable id.
func (s *Server) engine() (*engine.Engine, string) {
	s.bindMu.Lock()
	defer s.bindMu.Unlock()
	return s.eng, s.crazeID
}

// Serve accepts connections on l until Close, and returns nil then; any other
// accept failure that is not transient is returned, having closed nothing. l is
// closed by Close. Serve may be called for several listeners.
func (s *Server) Serve(l net.Listener) error {
	s.connMu.Lock()
	if s.closed {
		s.connMu.Unlock()
		_ = l.Close()
		return nil
	}
	s.listeners[l] = struct{}{}
	s.transport.Add(1)
	s.connMu.Unlock()
	defer s.transport.Done()
	defer func() {
		s.connMu.Lock()
		delete(s.listeners, l)
		s.connMu.Unlock()
	}()
	var backoff time.Duration
	for {
		nc, err := l.Accept()
		if err != nil {
			if s.isClosed() {
				return nil
			}
			if errors.Is(err, net.ErrClosed) {
				return err
			}
			// Transient (EMFILE, ECONNABORTED, …): wait and go on, as
			// net/http's accept loop does.
			backoff = min(max(2*backoff, 5*time.Millisecond), time.Second)
			s.logf("control: accept: %v; retrying in %s", err, backoff)
			time.Sleep(backoff)
			continue
		}
		backoff = 0
		s.accept(nc)
	}
}

func (s *Server) isClosed() bool {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	return s.closed
}

// accept checks the peer, before a byte is read, and starts the connection's
// reader and writer.
func (s *Server) accept(nc net.Conn) {
	if check := s.opts.PeerCheck; check != nil {
		err := errors.New("not a unix socket connection")
		if uc, ok := nc.(*net.UnixConn); ok {
			err = check(uc)
		}
		if err != nil {
			_ = nc.Close()
			s.connNote(0, map[string]any{"event": "refused", "reason": "peer check: " + err.Error()})
			return
		}
	}
	c := newConn(s, nc)
	s.connMu.Lock()
	if s.closed {
		s.connMu.Unlock()
		_ = nc.Close()
		return
	}
	s.nextConn++
	c.id = s.nextConn
	s.conns[c] = struct{}{}
	s.transport.Add(2)
	s.connMu.Unlock()
	s.connNote(c.id, map[string]any{"event": "open"})
	go c.read()
	go c.write()
}

// forget takes a closed connection out of the set.
func (s *Server) forget(c *conn) {
	s.connMu.Lock()
	delete(s.conns, c)
	s.connMu.Unlock()
}

// liveConns is every connection not yet forgotten.
func (s *Server) liveConns() []*conn {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	out := make([]*conn, 0, len(s.conns))
	for c := range s.conns {
		out = append(out, c)
	}
	return out
}

// Close closes every listener and connection and joins every transport
// goroutine, then waits for the handlers — which it does not join — until ctx
// ends (package doc, "Close"). It returns nil once every handler has returned,
// and otherwise, at ctx's end, an error naming how many are still running:
// each still completes into the engine's receipts table, and its reply goes
// nowhere. It is idempotent; a later call waits the same way.
func (s *Server) Close(ctx context.Context) error {
	s.connMu.Lock()
	s.closed = true
	ls := make([]net.Listener, 0, len(s.listeners))
	for l := range s.listeners {
		ls = append(ls, l)
	}
	s.connMu.Unlock()
	for _, l := range ls {
		_ = l.Close()
	}
	for _, c := range s.liveConns() {
		c.close("server closed")
	}
	s.transport.Wait()
	if n, err := s.handlers.wait(ctx); err != nil {
		return fmt.Errorf("control: closed with %d handlers still running: %w", n, err)
	}
	return nil
}

// admitCommand takes one of the host's CommandsPerHost slots, or reports that
// every one is taken (busy).
func (s *Server) admitCommand() bool {
	if s.commands.Add(1) > protocol.CommandsPerHost {
		s.commands.Add(-1)
		return false
	}
	return true
}

func (s *Server) commandDone() { s.commands.Add(-1) }

// logf writes one line to Options.Log.
func (s *Server) logf(format string, args ...any) {
	if s.opts.Log != nil {
		s.opts.Log(fmt.Sprintf(format, args...))
	}
}

// connNote records one connection event: a diag note in the session's journal
// (when a session is set) and a line to Options.Log. fields must hold nothing
// a client sent and never a token.
func (s *Server) connNote(id uint64, fields map[string]any) {
	fields["conn"] = id
	if eng, _ := s.engine(); eng != nil {
		eng.Note(journal.DiagNote{Kind: journal.DiagControlConn, Fields: fields})
	}
	if s.opts.Log != nil {
		line := fmt.Sprintf("control: conn %d %v", id, fields["event"])
		if c, ok := fields["clientId"].(string); ok && c != "" {
			line += " client " + c
		}
		if r, ok := fields["reason"].(string); ok && r != "" {
			line += ": " + r
		}
		s.opts.Log(line)
	}
}

// counter counts goroutines that are waited for but never joined.
type counter struct {
	mu   sync.Mutex
	n    int
	zero chan struct{}
}

func (c *counter) add() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.n == 0 {
		c.zero = make(chan struct{})
	}
	c.n++
}

func (c *counter) done() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n--
	if c.n == 0 {
		close(c.zero)
	}
}

// running is the count now.
func (c *counter) running() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// wait returns once the count is zero, or ctx's error and the count then.
func (c *counter) wait(ctx context.Context) (int, error) {
	c.mu.Lock()
	if c.n == 0 {
		c.mu.Unlock()
		return 0, nil
	}
	zero := c.zero
	c.mu.Unlock()
	select {
	case <-zero:
		return 0, nil
	case <-ctx.Done():
		return c.running(), ctx.Err()
	}
}
