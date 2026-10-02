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
	"slices"
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
	// Log receives one line per connection opened, refused or closed — the
	// peer's pid and uid when PeerCheck named them, and why it ended; nil
	// logs nothing. It is never handed anything a client sent, and never a
	// token. The same facts go to the session's journal as a diag note
	// (journal.DiagControlConn).
	Log func(string)
	// PeerCheck is run on every accepted connection before a byte of it is
	// read, before the connection is counted or its reader started (plan 027
	// §3.8): internal/rundir's PeerCheck, which requires the host's own uid.
	// It returns the peer's pid (0 when the OS cannot say) and uid — negative
	// when it could not name the peer — which the connection's open note
	// carries, and so does a refusal's when the uid is known. An error closes
	// the connection unread and is noted as a refusal. nil checks nothing and
	// notes no pid or uid (the tests that dial a socket of their own, and the
	// fake host). A connection that is not a *net.UnixConn fails a non-nil
	// check.
	PeerCheck func(*net.UnixConn) (pid, uid int, err error)
	// MaxBudget is the largest subscription budget a client may ask an attach
	// for: a larger member is held to it, and an absent one is the log's own
	// default. The zero value is DefaultMaxBudget: four times the event log's
	// subscription defaults.
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
	// Clock is the binding table's clock: how long a binding with no
	// connection is kept (bind.go, "Lifetime") is measured on it. nil is
	// time.Now, which is what every host runs on. It is the seam a test moves
	// past the bound with, as engine.Options.ReceiptClock is the receipts
	// table's; a test hands both the same clock.
	Clock func() time.Time

	// Stop is the host's lifecycle coordinator as session.stop reaches it
	// (plan 030 §3.6a; stop.go): set, the server serves session.stop and its
	// session info document says capabilities.stop: true; nil — a TUI-hosted
	// session, the fake host by default — session.stop is refused
	// unsupported, reason stop_unsupported, exactly as before plan 030. See
	// StopFunc for what the server does before it calls it, and what it must
	// not do.
	Stop StopFunc
	// PermissionMode is the info document's permissionMode (plan 030 §3.7,
	// SF-60): bypass for a host that spawned its agent with --force, prompt
	// for --no-force. "" leaves it out, which is an older host's document.
	PermissionMode protocol.PermissionMode
	// StartedAt is the info document's startedAt (plan 030 §3.7, SF-63):
	// when the host started serving the session, which the host decides —
	// the TUI-hosted path the moment it bound its socket, the one instant its
	// registry entry's startedAt records too. It is fixed for the server's
	// life: a TUI-hosted server's pickers may replace the engine, and the
	// host's start is still when it started. It goes on the wire in UTC; the
	// zero time leaves it out, which is an older host's document.
	StartedAt time.Time
	// RowFacts puts the row facts on sessions.list's row (plan 030 §3.8) —
	// the head ask's summary, what a working session is doing, its last
	// reply, when it entered its row state, a failed start and its error,
	// whether it was ever prompted (engine.RowFacts) — and says so in the
	// info document's capabilities.rowFacts. Every craze host sets it,
	// detached or TUI-hosted; false — the fake host by default — answers the
	// row S2 answered, which is an older host's (X1).
	RowFacts bool
	// Presence counts the clients attached to the session for them (plan 032
	// §3.14, SF-64): the info document says capabilities.presence: true, its
	// sessions.list row carries attached, and every attachment is sent the
	// presence notification — once its synchronized is written, then on
	// every change, the latest count at most twice a second (conn.go's
	// "Presence"). Every craze host sets it, detached or TUI-hosted; false —
	// the fake host by default — sends none of it, which is an older host's
	// wire.
	Presence bool
	// LocalClient says a client of the server's own process is attached for
	// its whole life: the TUI that hosts the session (`detach = false`), whose
	// seat is the process's and no connection's (plan 032 §3.14, R3-1). It
	// adds one to the count every attachments listener, every presence
	// notification and every row's attached see — so a `craze attach` beside
	// the host TUI reads 2, as the host TUI does — and to nothing else: a
	// close fence counts connections (FenceAttaches). True only for the
	// TUI-hosted server.
	LocalClient bool
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
	// writeStall is how long a writer may go with no byte moving before its
	// connection is closed (§3.7), measured from the last byte that moved
	// (conn.writeLine).
	writeStall = 60 * time.Second
	// commandTimeout bounds each blocking command — Interject, Cancel and Set
	// — on its server-owned context (§3.6). The non-waiting verbs take none.
	commandTimeout = 30 * time.Second
	// acceptBackoffMin and acceptBackoffMax bound the accept loop's wait after
	// a transient accept failure, doubling from the one to the other, as
	// net/http's accept loop does.
	acceptBackoffMin = 5 * time.Millisecond
	acceptBackoffMax = time.Second
	// readyWait is the server's own bound on a when: "ready" attach's wait for
	// the session's start (§3.4): past it the attach is unavailable, reason
	// not_ready — a load can take minutes, so it is not a flat 30 s.
	readyWait = 10 * time.Minute
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

	// Bounds a test may move (export_test.go); fixed before Serve.
	stall      time.Duration
	maxLine    int
	backoffMin time.Duration
	backoffMax time.Duration
	readyWait  time.Duration
	hooks      hooks

	// bindMu guards the engine, the binding table, the token index, the
	// released list and every conn's bound state (bind.go). A leaf above the
	// receipts table's.
	bindMu sync.Mutex
	eng    *engine.Engine
	// crazeID and incarnation are the engine's durable session id and its
	// log's incarnation, read once when it is set; idle is how long a binding
	// of its may go with no connection before it is dropped: twice its
	// receipts table's age bound (bind.go, "Lifetime").
	crazeID     string
	incarnation string
	idle        time.Duration
	binds       map[string]*binding
	// engStop ends the current engine's watcher (watchEngine) when the engine
	// is replaced.
	engStop chan struct{}
	// tokens indexes every resume token this server has issued and still
	// remembers — the current engine's and the previous one's — by token, to
	// the client id it names and the incarnation it was issued in (bind.go).
	tokens map[string]tokenOwner
	// released is every release of the current engine's bindings, oldest
	// first, for the lazy drop of the idle ones (bind.go, "Lifetime").
	released []releaseRecord

	connMu    sync.Mutex
	conns     map[*conn]struct{}
	listeners map[net.Listener]struct{}
	closed    bool
	// done is closed when closed is set: it ends the accept loops' backoff.
	done     chan struct{}
	nextConn uint64

	// transport counts the goroutines Close joins: accept loops, readers,
	// writers, forwarders, ready watchers and the engine's watcher (every one
	// started by goTransport or under connMu). handlers counts the ones it
	// does not (package doc, "Close").
	transport sync.WaitGroup
	handlers  counter
	// commands is how many mutating commands are in their engine call across
	// every connection: the host-wide cap (CommandsPerHost).
	commands atomic.Int64

	tokenMu sync.Mutex

	// attachMu is the server's attachment lock (plan 030 §3.6; stop.go): an
	// attach holds it across its fence check and the install of its pending
	// attachment (conn.reserve), and a close fence goes up under it
	// (FenceAttaches), so no attach reserves between a fence going up and
	// the count it reads. It is a leaf above conn.mu — reserve takes it and
	// then conn.mu — and nothing under conn.mu, or any other lock, takes it.
	attachMu sync.Mutex
	// fences is how many close fences are up (FenceAttaches); while it is
	// not 0 a new attach is refused closing. Guarded by attachMu.
	fences int
	// countMu guards attached, onAttach and presence (plan 030 §3.6;
	// attach.go's "Counting attachments"; plan 032 §3.14). It is a leaf,
	// taken under attachMu and under conn.mu — every change to the count is
	// made in the section that decides it — and under connMu (accept's
	// watchPresence), and nothing is taken under it but
	// the attachments listeners, which take no lock of the server's: a
	// presence slot is set with an atomic store and a wake that never waits
	// (outbox.setPresence).
	countMu sync.Mutex
	// attached is how many attachments the server counts: every one it holds
	// that is not yet closed — pending (reserved), live or closing (attach.go's
	// lifecycle) — across every connection, less those whose connection's
	// peer has half-closed (a read EOF: attach.go's "Counting attachments").
	// It rises only in reserve, under attachMu, and falls where an attachment
	// closes (conn.closedLocked) or its connection reads EOF (conn.eof), under
	// conn.mu, so under attachMu with a fence up it can only fall.
	attached int
	// onAttach is AddAttachmentsListener's listeners, in the order they were
	// added: each called with the count they see (seenLocked), under countMu,
	// on every change, so each hears the changes in order.
	onAttach []*attachListener
	// presence is every open connection's presence slot on a Presence server
	// (conn.go's "Presence"): each change stores the count it sees in every
	// one and wakes its writer, never waiting for it.
	presence map[*outbox]struct{}
	// stopOnce hands the first session.stop to Options.Stop, and raises the
	// stop's own fence, once for the server's life (stop.go).
	stopOnce sync.Once
}

// hooks are test barriers, nil in production and set before Serve
// (export_test.go).
type hooks struct {
	// beforeAcquire runs on a reader before it takes each admission slot,
	// with the connection's id.
	beforeAcquire func(conn uint64)
	// admissionFull runs on a reader that found every admission slot taken,
	// just before it waits for one.
	admissionFull func()
	// beforeUnbind runs on a closing connection's cleanup before its
	// compare-and-release, with the client id it is bound to ("" for none).
	beforeUnbind func(client string)
	// beforeBind runs in hello before the binding section (bindMu).
	beforeBind func()
	// beforeCommand runs on a handler that is about to run a mutating
	// command, before the host-wide cap and the binding's re-check, with the
	// method.
	beforeCommand func(method string)
	// beforeBarrier runs on a handler after its command returned and before
	// the reply barrier's SyncSeq, with the method.
	beforeBarrier func(method string)
	// outboxFull runs on a push that found no room in the budget, just before
	// it waits for some.
	outboxFull func()
	// beforeForward runs on a forwarder just before it queues the event with
	// seq — a live record, or one of the final records — with the
	// subscription's id.
	beforeForward func(sub string, seq uint64)
	// beforeTerminal runs on a forwarder whose subscription has ended, just
	// before it claims and queues its final records and reset, with the
	// subscription's id and the reset's reason.
	beforeTerminal func(sub string, reason protocol.ResetReason)
	// barrierWaits runs on a handler whose reply barrier is about to wait for
	// the connection's attachment (awaitAttachment), with the seq it waits
	// for; once per wait.
	barrierWaits func(seq uint64)
	// readyOwed runs on a ready watcher once it has handed its attachment's
	// forwarder the ready notification, with the seq it waits behind.
	readyOwed func(sub string, seq uint64)
	// readyDecided runs on a ready watcher once it has decided its attachment
	// is owed a ready at seq — the seq and the final info document read — just
	// before it hands the notification over.
	readyDecided func(sub string, seq uint64)
	// beforeReset runs on a forwarder once its final records are queued, just
	// before it decides its reset's reason and queues the reset (queueReset),
	// with the subscription's id and the reason it has so far.
	beforeReset func(sub string, reason protocol.ResetReason)
	// ackQueued runs once an attachment's acknowledgement is queued together
	// with the lifecycle step it makes visible — its attach reply (live), its
	// detach reply or its final reset (closed) — on the goroutine that queued
	// it, with the subscription's id and the reply's method, or reset.
	ackQueued func(sub, method string)
	// resetWritten runs on the writer once a final reset is on the socket (or
	// its write failed), before the connection may close for it — or on a
	// replacement that dropped the reset, queued before it, unwritten.
	resetWritten func()
	// beforeWrite runs on the writer once it has taken line from the queue,
	// just before it writes it.
	beforeWrite func(line []byte)
	// beforeReply runs on a handler just before it queues its reply (not
	// attach's or detach's, which queue their own), with the method — the
	// first session.stop's receipt included, which sessionStop queues itself
	// (stop.go), with its fence up and before its coordinator hears of it.
	beforeReply func(method string)
	// detaching runs on a detach once it has claimed the attachment's end and
	// stopped its forwarder's pushes, before it waits for the forwarder to
	// return, with the subscription's id.
	detaching func(sub string)
	// beforeReserve runs on an attach just before it calls reserve: a test
	// that blocks in it holds the attach there, past a replacement.
	beforeReserve func()
	// reserved runs on an attach right after reserve has installed its
	// pending attachment, before it does anything else: a test that blocks
	// in it holds a pending attachment on the connection, past a
	// replacement, with nothing yet done about it but reserve itself.
	reserved func(sub string)
	// reserving runs on an attach inside reserve, holding the server's
	// attachment lock, once its fence check has passed and before it
	// installs its pending attachment: a test that blocks in it holds an
	// attach between the two, where no fence can go up (plan 030 §3.6).
	reserving func()
	// fenceWaits runs on FenceAttaches that found the attachment lock held,
	// just before it waits for it (lockAttachments): the fence is excluded —
	// by an attach between its fence check and its install, say.
	fenceWaits func()
	// closeFenceStep runs on FenceClose between its steps: "attaches fenced"
	// once the attach fence is up, before the engine's; "engine fenced" once
	// both are, before it returns (plan 030 §3.6's forced races).
	closeFenceStep func(step string)
	// presenceTaken runs on a writer that has taken a presence line, before
	// it writes it, with the connection's id, the line and when the outbox
	// handed it over: the instant the twice-a-second bound is kept by.
	presenceTaken func(conn uint64, line []byte, at time.Time)
	// writerIdle runs on a writer about to wait for a wake with nothing
	// queued, no presence owed and no wake pending, with the connection's id
	// and how many lines it has taken from its outbox.
	writerIdle func(conn, taken uint64)
	// beforeArm runs on a forwarder in the conn.mu section that has just
	// queued its attachment's synchronized, before it arms the presence
	// slot, with the subscription's id.
	beforeArm func(sub string)
}

// New builds a server. It serves nothing until SetEngine and Serve.
func New(o Options) *Server {
	s := &Server{
		opts:       o,
		hostID:     o.HostID,
		version:    o.CrazeVersion,
		pid:        o.PID,
		maxBudget:  o.MaxBudget,
		stall:      writeStall,
		maxLine:    protocol.OutboundLineMax,
		backoffMin: acceptBackoffMin,
		backoffMax: acceptBackoffMax,
		readyWait:  readyWait,
		binds:      map[string]*binding{},
		tokens:     map[string]tokenOwner{},
		conns:      map[*conn]struct{}{},
		listeners:  map[net.Listener]struct{}{},
		presence:   map[*outbox]struct{}{},
		done:       make(chan struct{}),
	}
	if s.opts.Clock == nil {
		s.opts.Clock = time.Now
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

// attachListener is one AddAttachmentsListener's f, by pointer so its remove
// finds it.
type attachListener struct{ f func(n int) }

// AddAttachmentsListener has f called, in order, on every change in the
// number of attached clients, with the new number (plan 030 §3.6; plan 032
// §3.14), until remove is called: an attachment counts from its reservation
// to its close — pending, live or closing — unless its connection's peer has
// half-closed (attach.go, "Counting attachments"), and a server with
// Options.LocalClient counts its own process's client too, so f never hears
// less than 1 there. A connection that only calls hello or sessions.list is
// not attached and moves nothing. f is not called with the number as it
// stands when it is added: it hears the changes from there on (the idle
// watcher and the host TUI are added before Serve, when nothing is attached).
//
// Every listener is called on the goroutine that changed the count, under the
// server's own locks, one call at a time: it must return promptly and must
// not block, nor call into the server, an engine or a connection — it records
// the number, and a goroutine of its own acts on it elsewhere (the idle
// watcher; the host TUI's latest-value channel). Listeners are independent:
// one added beside another leaves it hearing every change (the idle watcher's
// and the host TUI's, side by side). remove may be called any number of
// times and from anywhere but a listener; once it has returned, f is not
// called again. A nil f is never called.
func (s *Server) AddAttachmentsListener(f func(n int)) (remove func()) {
	if f == nil {
		return func() {}
	}
	l := &attachListener{f: f}
	s.countMu.Lock()
	s.onAttach = append(s.onAttach, l)
	s.countMu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			s.countMu.Lock()
			s.onAttach = slices.DeleteFunc(s.onAttach, func(x *attachListener) bool { return x == l })
			s.countMu.Unlock()
		})
	}
}

// countAttachment moves the count of attachments by d and reports the count
// clients see (seenLocked) to every attachments listener and every
// connection's presence slot, in one countMu section, so each hears every
// change in order. A presence slot is an atomic store and a wake that never
// waits (outbox.setPresence): a writer stalled on a peer that has stopped
// reading holds back its own connection's presence and nobody else's (plan
// 032 §3.14, R2-6). It is called only in the section that decides the change:
// reserve (attachMu, conn.mu), closedLocked and eof (conn.mu).
func (s *Server) countAttachment(d int) {
	s.countMu.Lock()
	defer s.countMu.Unlock()
	s.attached += d
	n := s.seenLocked()
	for _, l := range s.onAttach {
		l.f(n)
	}
	for o := range s.presence {
		o.setPresence(n)
	}
}

// seenLocked is the count clients see: the attachments, and the host TUI's
// own seat on a LocalClient server; countMu is held.
func (s *Server) seenLocked() int {
	if s.opts.LocalClient {
		return s.attached + 1
	}
	return s.attached
}

// attachedCount is the count of attachments now: what a close fence reads,
// with no LocalClient seat.
func (s *Server) attachedCount() int {
	s.countMu.Lock()
	defer s.countMu.Unlock()
	return s.attached
}

// presenceCount is the count clients see now (seenLocked): a sessions.list
// row's attached.
func (s *Server) presenceCount() int {
	s.countMu.Lock()
	defer s.countMu.Unlock()
	return s.seenLocked()
}

// watchPresence puts o, a new connection's outbox, among the presence slots
// of a Presence server, holding the count clients see now; a server without
// presence keeps none.
func (s *Server) watchPresence(o *outbox) {
	if !s.opts.Presence {
		return
	}
	s.countMu.Lock()
	o.setPresence(s.seenLocked())
	s.presence[o] = struct{}{}
	s.countMu.Unlock()
}

// forgetPresence takes a closed connection's outbox out of the presence
// slots.
func (s *Server) forgetPresence(o *outbox) {
	s.countMu.Lock()
	delete(s.presence, o)
	s.countMu.Unlock()
}

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
// older is dropped (bind.go, "The token index"). Every connection admits
// nothing more from that moment and writes only its terminal line (plan 027
// X25): an attached one is sent reset{session_replaced} — or the reply of a
// detach that had already claimed its attachment's end — and closed once that
// is on the socket (conn.replace), every other one at once. A handler already
// running keeps the engine it captured at dispatch, and its reply goes
// nowhere, as does every other line not yet written.
//
// The engine's end is watched (watchEngine): once it has closed, every
// connection bound to it is closed as its session ends (conn.end).
func (s *Server) SetEngine(e *engine.Engine) {
	var crazeID, incarnation string
	var idle time.Duration
	if e != nil {
		st := e.State()
		crazeID, incarnation = st.CrazeSessionID, st.Incarnation
		idle = 2 * st.RetryHorizon.Age
	}
	s.bindMu.Lock()
	if s.eng == e {
		s.bindMu.Unlock()
		return
	}
	replaced := s.eng != nil
	outgoing := s.incarnation
	s.eng, s.crazeID, s.incarnation, s.idle = e, crazeID, incarnation, idle
	if replaced {
		s.binds = map[string]*binding{}
		s.released = nil
		for tok, o := range s.tokens {
			if o.incarnation != outgoing {
				delete(s.tokens, tok)
			}
		}
	}
	oldStop := s.engStop
	s.engStop = nil
	var stop chan struct{}
	if e != nil {
		stop = make(chan struct{})
		s.engStop = stop
	}
	s.bindMu.Unlock()
	if oldStop != nil {
		close(oldStop)
	}
	if e != nil {
		s.goTransport(func() { s.watchEngine(e, stop) })
	}
	if !replaced {
		return
	}
	for _, c := range s.liveConns() {
		c.replace()
	}
}

// watchEngine is an engine's watcher (plan 027 §3.7, astra r5 13): once the
// engine has closed (engine.Done), every connection bound to it ends
// (conn.end) — it admits nothing more, answers what it admitted, an attached
// one delivers its final records and reset{session_closed}, and it closes
// once all of that is written, releasing its client. A connection that binds
// to it afterwards ends at its hello (conn.hello). The watcher returns then,
// or when the engine is replaced (stop), or when the server closes.
func (s *Server) watchEngine(e *engine.Engine, stop <-chan struct{}) {
	select {
	case <-e.Done():
	case <-stop:
		return
	case <-s.done:
		return
	}
	for _, c := range s.liveConns() {
		s.bindMu.Lock()
		mine := c.bound != nil && c.bound.eng == e
		s.bindMu.Unlock()
		if mine {
			c.end("session ended")
		}
	}
}

// goTransport runs f on a goroutine Close joins (transport), unless the
// server has closed — then it runs nothing and reports false. The check and
// the count are one connMu section, and Close sets closed in one before it
// waits, so nothing is added to the count once Close is waiting on it.
func (s *Server) goTransport(f func()) bool {
	s.connMu.Lock()
	if s.closed {
		s.connMu.Unlock()
		return false
	}
	s.transport.Add(1)
	s.connMu.Unlock()
	go func() {
		defer s.transport.Done()
		f()
	}()
	return true
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
			// net/http's accept loop does. Close ends the wait, so the
			// loop never holds Close past its deadline (astra r5 6).
			backoff = min(max(2*backoff, s.backoffMin), s.backoffMax)
			s.logf("control: accept: %v; retrying in %s", err, backoff)
			wait := time.NewTimer(backoff)
			select {
			case <-wait.C:
			case <-s.done:
				wait.Stop()
				return nil
			}
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

// accept checks the peer, before a byte is read — before the connection is
// built, counted or its reader started — and starts the connection's reader
// and writer.
func (s *Server) accept(nc net.Conn) {
	pid, uid := 0, -1 // the peer as PeerCheck named it; a negative uid is no one
	if check := s.opts.PeerCheck; check != nil {
		err := errors.New("not a unix socket connection")
		if uc, ok := nc.(*net.UnixConn); ok {
			pid, uid, err = check(uc)
		}
		if err != nil {
			_ = nc.Close()
			s.connNote(0, withPeer(map[string]any{"event": "refused", "reason": "peer check: " + err.Error()}, pid, uid))
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
	// In the section that makes the connection one Close closes, so its
	// close always forgets the slot; and before its reader starts, so the
	// slot holds the count before any attach of this connection's can arm it.
	s.watchPresence(c.out)
	s.connMu.Unlock()
	s.connNote(c.id, withPeer(map[string]any{"event": "open"}, pid, uid))
	go c.read()
	go c.write()
}

// withPeer is fields with the peer's pid and uid added when the peer check
// named the peer (a uid that is not negative).
func withPeer(fields map[string]any, pid, uid int) map[string]any {
	if uid >= 0 {
		fields["pid"], fields["uid"] = pid, uid
	}
	return fields
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

// OpenConns is how many connections are accepted and not yet forgotten: a
// connection's whole close (conn.close) — its unbind's compare-and-release
// among them — has already run for every one missing from this count, since
// forget is the last thing close does. Production-harmless: a read of connMu
// with no effect on the server's own behavior, exposed as the smallest seam
// a caller outside this package has for waiting until a connection it closed
// out-of-band (internal/fakehost's DropConnections wraps the listener, not
// this Server, so it has no other way to observe the cleanup its close
// triggers) has actually finished releasing its binding, rather than racing
// a later clock advance against that cleanup's own goroutine (plan 027 C9a
// review item 2).
func (s *Server) OpenConns() int {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	return len(s.conns)
}

// Close closes every listener and connection — and so every subscription —
// and joins every transport goroutine, the forwarders included, then waits for
// the handlers — which it does not join — until ctx ends (package doc,
// "Close"). It returns nil once every handler has returned,
// and otherwise, at ctx's end, an error naming how many are still running:
// each still completes into the engine's receipts table, and its reply goes
// nowhere. It is idempotent; a later call waits the same way.
func (s *Server) Close(ctx context.Context) error {
	s.connMu.Lock()
	if !s.closed {
		s.closed = true
		close(s.done)
	}
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
		if uid, ok := fields["uid"].(int); ok {
			line += fmt.Sprintf(" pid %v uid %d", fields["pid"], uid)
		}
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
