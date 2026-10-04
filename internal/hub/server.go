package hub

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/charliek/craze/internal/protocol"
)

// The hub's server (plan 032 §3.5, §3.6): its socket's connections. Each is
// checked as it is accepted — the peer must run as this user
// (rundir.PeerCheck, SD-04), the hub must not be closing, and at most
// connsMax are open (more are closed at accept, unanswered) — and must say
// hello within helloWait or is closed. A hello answered is the connection's
// admission (lifecycle.admitClient): from then until it closes it is a
// client, which keeps the hub from its idle exit. One goroutine per
// connection reads a line, answers it and reads the next.
//
// What the hub serves, method by method: hello (HubHelloResult, the hub's
// capabilities); sessions.list and sessions.subscribe, the roster (roster.go;
// one subscription per connection, whose notifications its own flusher
// writes); session.connect, which hands the whole connection to a session's
// host (splice.go); every other session-scoped method is a host's, refused
// unsupported, reason host_only — the hub routes by splicing, never method by
// method (SQ14); session.create, which starts a session in a new host
// (create.go) — on a hub given the means to (Options.Creates), and refused
// unsupported on any other; and sessions.createOptions, what a create can
// start (options.go) — on a hub given its answer (Creates.Options), and
// refused unsupported on any other.
//
// Writes: one line at a time (wmu), each bounded — a reply by writeWait, a
// roster notification by slowWait (conn.notifyLocked) — and every deadline
// capped by the teardown's (capWrites), so its last word to a subscriber is
// bounded whatever a write in flight was given. A line cut part way leaves the
// connection broken: nothing more is written on it, and it closes. A line
// that carries the roster's cursor — a sessions.list reply, a roster
// notification — takes its roster and is written in one section under wmu
// (conn.list, conn.flush): the lock order is wmu, then the roster's, and
// nothing waits for wmu holding the roster's.

// The server's bounds: variables only so a test can shorten them (never in
// parallel).
var (
	// helloWait is how long a connection has to say hello.
	helloWait = 5 * time.Second
	// connsMax is how many connections the hub keeps open at once.
	connsMax = 256
	// writeWait bounds one reply's write: a peer that reads nothing for
	// that long is closed.
	writeWait = 10 * time.Second
)

// server is the hub's listener and its connections.
type server struct {
	h  *hub
	ln *net.UnixListener
	// peer is the accept check (rundir.PeerCheck).
	peer func(*net.UnixConn) (pid, uid int, err error)
	// acceptDone is closed once the accept loop has returned.
	acceptDone chan struct{}
	// conns is every connection's goroutine, admitted under the lifecycle
	// lock (lifecycle.admitConn) and done when it has closed.
	conns sync.WaitGroup
}

// accept is the accept loop, until the listener is closed.
func (s *server) accept() {
	defer close(s.acceptDone)
	for {
		uc, err := s.ln.AcceptUnix()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			// A transient failure (out of descriptors): never a spin.
			time.Sleep(50 * time.Millisecond)
			continue
		}
		if _, _, err := s.peer(uc); err != nil {
			s.h.logf("refused a connection: %v", err)
			_ = uc.Close()
			continue
		}
		c := &conn{s: s, uc: uc, lr: protocol.NewLineReader(uc, protocol.InboundLineMax)}
		if !s.h.life.admitConn(c, &s.conns) {
			// Closing — the idle decision, or a stop — or at the cap:
			// closed unanswered.
			_ = uc.Close()
			continue
		}
		if s.h.hk.accepted != nil {
			s.h.hk.accepted(uc)
		}
		go c.serve()
	}
}

// closeAll ends every connection still open: teardown's last word to its
// clients (§3.5). A splice is half-closed first — both legs' writing halves,
// so each peer reads everything already forwarded and then its end: the host
// sees its client go, as when the client half-closes, and the client sees the
// hub go — and closed once it has ended on its own or spliceDrain has passed,
// whichever is first (never past deadline, the teardown's). Every other
// connection is closed at once. Each one's goroutine then ends.
func (s *server) closeAll(deadline time.Time) {
	var splices []*conn
	for _, c := range s.h.life.openConns() {
		if c.halfClose() {
			splices = append(splices, c)
			continue
		}
		c.close()
	}
	if len(splices) == 0 {
		return
	}
	until := time.Now().Add(spliceDrain)
	if deadline.Before(until) {
		until = deadline
	}
	for _, c := range splices {
		t := time.NewTimer(time.Until(until))
		select {
		case <-c.splice().done:
		case <-t.C:
		}
		t.Stop()
	}
	for _, c := range splices {
		c.close()
	}
	s.h.logf("closed %d splice(s)", len(splices))
}

// conn is one accepted connection.
type conn struct {
	s  *server
	uc *net.UnixConn
	lr *protocol.LineReader
	// client says hello was answered: the connection counts as a client
	// until it closes (lifecycle.dropConn). Read and written on the
	// connection's own goroutine, and under the lifecycle lock.
	client bool
	wmu    sync.Mutex
	once   sync.Once
	// writing is set while a write is in the kernel's hands: a test's view
	// of an answer stuck behind a peer that does not read.
	writing atomic.Bool
	// broken says a line was cut part way (under wmu): nothing more can be
	// written that a peer could read as lines.
	broken bool
	// capAt is the teardown's cap on every write deadline, zero for none
	// (dmu, which orders a deadline's setting against the cap's).
	dmu   sync.Mutex
	capAt time.Time
	// subs is the connection's latest roster subscription — its current one,
	// unless it has ended — and none before it: a new subscribe joins and
	// drops the ended ones first (r23 3). The connection's own goroutine's.
	subs []*subscription
	// answered counts the lines answered since hello was — whatever they
	// were — so session.connect knows it is the first (§3.7): the
	// connection's own goroutine's.
	answered int
	// handoff is the splice session.connect handed the connection to, which
	// serve runs once the reading has stopped: the connection's own
	// goroutine's.
	handoff *splice

	// smu orders the teardown's close against a splice's start: shut once
	// the connection has been closed or half-closed, sp the splice it is
	// handed to (set before session.connect's {} is written).
	smu  sync.Mutex
	shut bool
	sp   *splice
}

// serve reads and answers the connection's lines until it closes, the hello
// deadline passes, or an answer ends it — and then, for a connection
// session.connect handed to a host, runs the splice until both its legs are
// closed: until then the connection is a client (lifecycle.dropConn runs
// last).
func (c *conn) serve() {
	defer c.s.conns.Done()
	defer c.s.h.life.dropConn(c)
	// After the close, which ends a notification's write in flight: each
	// subscription ended (unless it has) and its flusher joined.
	defer c.endSubs()
	defer c.close()
	c.read()
	if sp := c.handoff; sp != nil {
		sp.run()
		c.s.h.logf("the splice to host %s (session %s) ended: %d bytes to the host, %d to the client",
			sp.hostID, sp.sessionID, sp.up.written.Load(), sp.down.written.Load())
	}
}

// read is serve's reading: one line at a time, each answered before the next
// is read, until the connection ends or an answer says to stop.
func (c *conn) read() {
	_ = c.uc.SetReadDeadline(time.Now().Add(helloWait))
	for {
		line, err := c.lr.ReadLine()
		if errors.Is(err, protocol.ErrLineTooLong) {
			if !c.counted(func() bool {
				c.replyErr(nil, &protocol.Error{Code: protocol.RPCInvalidRequest, Message: "line too long",
					Data: protocol.ErrorData{Code: protocol.CodeBadRequest, Reason: protocol.ReasonLineTooLong}})
				return true
			}) {
				return
			}
			continue
		}
		if err != nil {
			return
		}
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		if !c.counted(func() bool { return c.dispatch(line) }) {
			return
		}
	}
}

// counted is answer, counting the line as answered after hello when hello was
// answered before it.
func (c *conn) counted(fn func() bool) bool {
	after := c.client
	ok := c.answer(fn)
	if after {
		c.answered++
	}
	return ok
}

// answer runs fn — one line's answer — as in-flight work the teardown waits
// for (lifecycle.begin): false, ending the connection, when the hub is
// closing and does no more, or when fn says so.
func (c *conn) answer(fn func() bool) bool {
	if !c.s.h.life.begin() {
		return false
	}
	defer c.s.h.life.end()
	return fn()
}

// close closes the connection, once — and, once it is handed to a splice, the
// splice's host leg with it.
func (c *conn) close() {
	c.smu.Lock()
	c.shut = true
	sp := c.sp
	c.smu.Unlock()
	if sp != nil {
		sp.close()
	}
	c.once.Do(func() { _ = c.uc.Close() })
}

// halfClose is the teardown's first word to a splice (server.closeAll): both
// legs' writing halves closed, and true. A connection that is not a splice is
// left to close, and false; either way nothing can be handed to a splice
// after it.
func (c *conn) halfClose() bool {
	c.smu.Lock()
	c.shut = true
	sp := c.sp
	c.smu.Unlock()
	if sp == nil {
		return false
	}
	sp.halfClose()
	return true
}

// handTo makes sp the connection's splice: false, sp untouched, once the
// teardown has closed the connection.
func (c *conn) handTo(sp *splice) bool {
	c.smu.Lock()
	defer c.smu.Unlock()
	if c.shut {
		return false
	}
	c.sp = sp
	return true
}

// splice is the connection's splice, nil before session.connect has handed it
// to one.
func (c *conn) splice() *splice {
	c.smu.Lock()
	defer c.smu.Unlock()
	return c.sp
}

// dispatch answers one request line: false ends the connection.
func (c *conn) dispatch(line []byte) bool {
	req, id, perr := parseRequest(line)
	if perr != nil {
		return c.replyErr(id, perr)
	}
	info, known := protocol.Method(req.method)
	switch {
	case !known:
		return c.replyErr(req.id, &protocol.Error{Code: protocol.RPCMethodNotFound,
			Message: fmt.Sprintf("unknown method %q", req.method),
			Data:    protocol.ErrorData{Code: protocol.CodeUnsupported, Reason: protocol.ReasonUnknownMethod}})
	case req.method == protocol.MethodHello:
		return c.hello(req)
	case !c.client:
		return c.replyErr(req.id, refused(protocol.CodeBadRequest, protocol.ReasonHelloRequired,
			"%s before hello: every method but hello needs one first", req.method))
	case req.method == protocol.MethodSessionsList:
		return c.list(req)
	case req.method == protocol.MethodSessionsSubscribe:
		return c.subscribe(req)
	case req.method == protocol.MethodSessionConnect:
		return c.connect(req)
	case req.method == protocol.MethodSessionCreate:
		return c.create(req)
	case req.method == protocol.MethodSessionsCreateOptions && c.s.h.options() != nil:
		// A hub without the answer falls through to unsupported (options.go).
		return c.createOptions(req)
	case info.SessionScoped:
		return c.replyErr(req.id, refused(protocol.CodeUnsupported, protocol.ReasonHostOnly,
			"%s is a session host's: connect to the session (session.connect) and send it there", req.method))
	}
	return c.replyErr(req.id, refused(protocol.CodeUnsupported, protocol.ReasonUnsupported,
		"%s is not served by the hub", req.method))
}

// hello opens the connection (plan 032 §3.6): the one tolerant method,
// validated as a host validates it. Its admission (lifecycle.admitClient) is
// the instant the hub counts the connection as a client; a hub that has
// decided to close answers unavailable, reason closing, and ends the
// connection — the answer Ensure takes for no hub at all.
func (c *conn) hello(req *request) bool {
	if c.client {
		return c.replyErr(req.id, refused(protocol.CodeBadRequest, protocol.ReasonBadRequest,
			"hello was already answered on this connection"))
	}
	var p protocol.HelloParams
	if len(bytes.TrimSpace(req.params)) > 0 {
		if err := json.Unmarshal(req.params, &p); err != nil {
			return c.replyErr(req.id, badParams("params: %v", err))
		}
	}
	switch {
	case len(p.Protocols) == 0:
		return c.replyErr(req.id, badParams("params.protocols is required: every protocol version the client speaks"))
	case p.Client.Kind == "":
		return c.replyErr(req.id, badParams("params.client.kind is required"))
	case !isNull(p.Auth):
		return c.replyErr(req.id, badParams("params.auth is reserved and must be absent or null in protocol 1"))
	case !isNull(p.Via):
		return c.replyErr(req.id, badParams("params.via is reserved and must be absent or null in protocol 1"))
	}
	version := 0
	for _, v := range p.Protocols {
		if slices.Contains(protocol.SupportedProtocols(), v) && v > version {
			version = v
		}
	}
	if version == 0 {
		e := refused(protocol.CodeBadRequest, protocol.ReasonProtocolVersion,
			"no protocol version in common: this hub speaks %v", protocol.SupportedProtocols())
		e.Data.Result, _ = json.Marshal(protocol.HelloErrorResult{Supported: protocol.SupportedProtocols()})
		return c.replyErr(req.id, e)
	}
	if !c.s.h.life.admitClient(c) {
		c.replyErr(req.id, refused(protocol.CodeUnavailable, protocol.ReasonClosing, "the hub is closing"))
		return false
	}
	_ = c.uc.SetReadDeadline(time.Time{})
	h := c.s.h
	caps := protocol.HubCapabilities()
	// A hub given no way to start a host creates nothing (Options.Creates),
	// and one given no answer to sessions.createOptions says nothing of it:
	// createOptions is omitted when false (plan 036 §3.4).
	caps.SessionCreate = h.cr != nil
	caps.CreateOptions = h.options() != nil
	return c.reply(req.id, protocol.HubHelloResult{
		Protocol: version,
		Endpoint: protocol.Endpoint{Kind: protocol.EndpointHub, HostID: h.id,
			CrazeVersion: h.version, PID: h.pid},
		Capabilities: caps,
		Codecs:       h.o.Codecs,
		Limits:       protocol.HostLimits(),
	})
}

// list answers sessions.list (roster.go): once the open run has polled every
// host — at most listWait — the roster as it stands, or its refusal while
// the run has read no registry. Its snapshot and its write are one section
// under the connection's write lock, taken before the roster's (the
// flusher's order, conn.flush): on a connection that holds a subscription the
// reply is never written behind a notification of a later cursor, nor a
// notification behind it of an earlier one (r19 1), and the reply's rows are
// what the subscription's client holds from then on (rebaseLocked, r23 1) —
// when they are written: a reply over the line limit changes nothing (r25
// 1). The roster's lock is not held across the write.
func (c *conn) list(req *request) bool {
	if perr := emptyParams(req.params); perr != nil {
		return c.replyErr(req.id, perr)
	}
	rs := c.s.h.rs
	run := rs.acquire()
	rs.await(run)
	if f := c.s.h.hk.rosterLocking; f != nil {
		f(c, "list")
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	line, ok := rs.listLine(run, c.lastSub(), req.id)
	rs.release()
	if !ok {
		c.writeLocked(errorLine(req.id, refused(protocol.CodeUnavailable, protocol.ReasonClosing, "the hub is closing")))
		return false
	}
	if f := c.s.h.hk.rosterTaken; f != nil {
		f(c, "list")
	}
	return c.writeLocked(line)
}

// lastSub is the connection's latest subscription, nil when it has made
// none: its current one, unless that has ended.
func (c *conn) lastSub() *subscription {
	if n := len(c.subs); n > 0 {
		return c.subs[n-1]
	}
	return nil
}

// subscribe answers sessions.subscribe (roster.go): one per connection; once
// the open run has polled every host — at most listWait — the subscription
// is registered as its reply is built (or refused while the run has read no
// registry, registering nothing), the reply written, and its flusher writes
// its notifications from then on. The subscription keeps its demand on the
// poll until it ends. A reply that cannot be written as a result — over the
// line limit, answered response_too_large — tells the client of no
// subscription, so none is kept (abandon, r23 5). The subscriptions this
// connection made before, every one ended, are joined and dropped first, so
// one that subscribes again and again holds one (r23 3).
func (c *conn) subscribe(req *request) bool {
	if perr := emptyParams(req.params); perr != nil {
		return c.replyErr(req.id, perr)
	}
	rs := c.s.h.rs
	if last := c.lastSub(); last != nil && rs.live(last) {
		return c.replyErr(req.id, refused(protocol.CodeBadRequest, protocol.ReasonAlreadySubscribed,
			"this connection holds roster subscription %s already", last.id))
	}
	// Each ended one's flusher returns at once, or after the line it is
	// writing, which is bounded (slowWait, writeWait).
	for _, s := range c.subs {
		<-s.done
	}
	c.subs = nil
	run := rs.acquire()
	rs.await(run)
	sub, res, perr, ok := rs.subscribe(c, run)
	if !ok {
		rs.release()
		c.replyErr(req.id, refused(protocol.CodeUnavailable, protocol.ReasonClosing, "the hub is closing"))
		return false
	}
	if perr != nil {
		rs.release()
		return c.replyErr(req.id, perr)
	}
	line, fits := encodeReply(req.id, res)
	if !fits {
		sub.abandon()
		return c.writeLine(line)
	}
	c.subs = append(c.subs, sub)
	if f := c.s.h.hk.subscriptions; f != nil {
		f(len(c.subs))
	}
	go sub.run()
	if f := c.s.h.hk.subscribed; f != nil {
		f()
	}
	written := c.writeLine(line)
	close(sub.replied)
	return written
}

// endSubs ends every subscription the connection made — the connection has
// closed — and joins their flushers.
func (c *conn) endSubs() {
	for _, s := range c.subs {
		c.s.h.rs.end(s, "")
		<-s.done
	}
}

// emptyParams holds params to {} — absent, or an object with no member — as a
// host holds a method's params (strict): a member is unknown_field.
func emptyParams(raw json.RawMessage) *protocol.Error {
	b := bytes.TrimSpace(raw)
	if len(b) == 0 {
		return nil
	}
	if string(b) == "null" {
		return badParams("params must be an object, not null")
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return badParams("params must be an object")
	}
	if len(m) > 0 {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		return &protocol.Error{Code: protocol.RPCInvalidParams, Message: "unknown field params." + keys[0],
			Data: protocol.ErrorData{Code: protocol.CodeBadRequest, Reason: protocol.ReasonUnknownField}}
	}
	return nil
}

// reply writes result as the answer to id (replyLine): false when the write
// failed, which ends the connection.
func (c *conn) reply(id json.RawMessage, result any) bool {
	return c.writeLine(replyLine(id, result))
}

// writeLine writes one line under the write lock, as reply does.
func (c *conn) writeLine(line []byte) bool {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return c.writeLocked(line)
}

// replyErr writes e as the answer to id (nil: null), as reply does.
func (c *conn) replyErr(id json.RawMessage, e *protocol.Error) bool {
	line := errorLine(id, e)
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return c.writeLocked(line)
}

// replyLine is the line answering id with result, in MarshalLine's encoding
// (HTML not escaped). A result that does not encode is answered failed, and
// one whose line would be over the outbound line limit failed, reason
// response_too_large — a roster stays under it for a request id within
// RosterRequestIDBytesMax (limits.go). Nil only when not even that encodes.
func replyLine(id json.RawMessage, result any) []byte {
	line, _ := encodeReply(id, result)
	return line
}

// encodeReply is replyLine's line, and whether it is result's — false when
// it answers id with a failure in its place.
func encodeReply(id json.RawMessage, result any) ([]byte, bool) {
	raw, err := protocol.MarshalLine(result)
	if err != nil {
		return errorLine(id, refused(protocol.CodeFailed, protocol.ReasonFailed, "%v", err)), false
	}
	if len(`{"jsonrpc":"2.0","id":,"result":}`)+len(id)+len(raw)-1 > protocol.OutboundLineMax {
		return errorLine(id, refused(protocol.CodeFailed, protocol.ReasonResponseTooLarge,
			"the reply would be over the %d-byte line limit", protocol.OutboundLineMax)), false
	}
	line, err := protocol.MarshalLine(protocol.Response{JSONRPC: protocol.JSONRPCVersion, ID: id,
		Result: bytes.TrimSuffix(raw, []byte{'\n'})})
	if err != nil {
		return nil, false
	}
	return line, true
}

// errorLine is the line answering id (nil: null) with e; nil when it does not
// encode.
func errorLine(id json.RawMessage, e *protocol.Error) []byte {
	line, err := protocol.MarshalLine(protocol.Response{JSONRPC: protocol.JSONRPCVersion, ID: id, Error: e})
	if err != nil {
		return nil
	}
	return line
}

// writeLocked writes one line (nil: none, a failure), wmu held, bounded by
// writeWait: a peer that does not read it in that time is a connection that
// ends.
func (c *conn) writeLocked(line []byte) bool {
	if line == nil || c.broken {
		return false
	}
	_ = c.setWriteDeadline(time.Now().Add(writeWait))
	c.writing.Store(true)
	n, err := c.uc.Write(line)
	c.writing.Store(false)
	if err != nil && n > 0 {
		c.broken = true
	}
	return err == nil
}

// flush writes s's next notification, if it has one: false ends the flusher.
// Its take and its write are one section under the connection's write lock,
// taken before the roster's — a sessions.list's snapshot and its write are
// the same (conn.list) — so the lines one connection is written carry
// cursors that never go back; a write that blocks holds the write lock, never
// the roster's.
func (c *conn) flush(s *subscription) bool {
	if f := c.s.h.hk.rosterLocking; f != nil {
		f(c, "flush")
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.broken {
		return false
	}
	p, ok := s.rs.take(s)
	if !ok {
		return true
	}
	if f := c.s.h.hk.rosterTaken; f != nil {
		f(c, "flush")
	}
	return c.notifyLocked(s, p)
}

// notifyLocked writes p, s's roster notification, wmu held, bounded by
// slowWait: false ends the flusher. A write that blocks that long ends the
// subscription — unless it has ended meanwhile, the teardown's cap having cut
// the write — with the rest of the line and a reset{slow_consumer} written
// after it, bounded by writeWait; the connection stays for a new
// subscription. A write that fails otherwise, or the rest that cannot be
// written, ends the connection.
func (c *conn) notifyLocked(s *subscription, p protocol.RosterParams) bool {
	line, err := notificationLine(protocol.NotifyRoster, p)
	if err != nil {
		c.s.h.logf("roster subscription %s: %v", s.id, err)
		c.close()
		return false
	}
	_ = c.setWriteDeadline(time.Now().Add(slowWait))
	c.writing.Store(true)
	n, err := c.uc.Write(line)
	c.writing.Store(false)
	if err == nil {
		return true
	}
	var ne net.Error
	if !errors.As(err, &ne) || !ne.Timeout() {
		c.broken = c.broken || n > 0
		c.close()
		return false
	}
	if !s.rs.end(s, protocol.ResetSlowConsumer) {
		// It ended meanwhile: the teardown's cap cut the write, the
		// connection closed, or the roster's completeness changed (omitted,
		// whose reset the flusher writes next). A line cut part way leaves
		// nothing more to write, and the connection closes.
		if n > 0 {
			c.broken = true
			c.close()
		}
		return false
	}
	c.s.h.logf("roster subscription %s: a notification's write blocked for %v; reset slow_consumer", s.id, slowWait)
	reset, err := notificationLine(protocol.NotifyReset, protocol.ResetParams{Subscription: s.id, Reason: protocol.ResetSlowConsumer})
	if err == nil {
		rest := append(append(make([]byte, 0, len(line)-n+len(reset)), line[n:]...), reset...)
		_ = c.setWriteDeadline(time.Now().Add(writeWait))
		c.writing.Store(true)
		var m int
		m, err = c.uc.Write(rest)
		c.writing.Store(false)
		if err == nil {
			return false
		}
		n += m
	}
	c.broken = c.broken || n > 0
	c.close()
	return false
}

// writeReset writes reset{why} for subscription sub, bounded by writeWait and
// the teardown's cap. A reset that cannot be written closes the connection: a
// subscriber is never left holding a subscription that is over.
func (c *conn) writeReset(sub string, why protocol.ResetReason) {
	line, err := notificationLine(protocol.NotifyReset, protocol.ResetParams{Subscription: sub, Reason: why})
	if err != nil {
		return
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.broken {
		return
	}
	_ = c.setWriteDeadline(time.Now().Add(writeWait))
	c.writing.Store(true)
	n, err := c.uc.Write(line)
	c.writing.Store(false)
	if err != nil {
		c.broken = c.broken || n > 0
		c.close()
	}
}

// setWriteDeadline sets the connection's write deadline to d, or the
// teardown's cap if that is sooner.
func (c *conn) setWriteDeadline(d time.Time) error {
	c.dmu.Lock()
	defer c.dmu.Unlock()
	if !c.capAt.IsZero() && c.capAt.Before(d) {
		d = c.capAt
	}
	return c.uc.SetWriteDeadline(d)
}

// capWrites caps every write deadline at at, the write in flight's included.
func (c *conn) capWrites(at time.Time) {
	c.dmu.Lock()
	defer c.dmu.Unlock()
	if c.capAt.IsZero() || at.Before(c.capAt) {
		c.capAt = at
	}
	_ = c.uc.SetWriteDeadline(c.capAt)
}

// notificationLine is one notification's line: method, params.
func notificationLine(method string, params any) ([]byte, error) {
	raw, err := protocol.MarshalLine(params)
	if err != nil {
		return nil, err
	}
	return protocol.MarshalLine(protocol.Notification{JSONRPC: protocol.JSONRPCVersion, Method: method,
		Params: bytes.TrimSuffix(raw, []byte{'\n'})})
}

// request is one line parsed as a request: its id verbatim, its method and
// its params, raw.
type request struct {
	id     json.RawMessage
	method string
	params json.RawMessage
}

// envelopeMembers are the members a request may carry (envelope.json's
// request is strict).
var envelopeMembers = []string{"jsonrpc", "id", "method", "params"}

// parseRequest reads one line as a request, or the error that answers it, as
// a host reads one (internal/control's parseRequest): -32700 for a line that
// is not JSON, -32600 for a batch, a value that is not an object or an object
// that is not a request. The id is echoed whenever it could be read.
func parseRequest(line []byte) (*request, json.RawMessage, *protocol.Error) {
	if !json.Valid(line) {
		return nil, nil, &protocol.Error{Code: protocol.RPCParseError, Message: "not JSON",
			Data: protocol.ErrorData{Code: protocol.CodeBadRequest, Reason: protocol.ReasonBadRequest}}
	}
	if protocol.IsBatch(line) {
		return nil, nil, invalidRequest("a batch: protocol 1 takes one request per line")
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(line, &m); err != nil {
		return nil, nil, invalidRequest("not a JSON object")
	}
	var id json.RawMessage
	if raw, ok := m["id"]; ok {
		if b := bytes.TrimSpace(raw); len(b) > 0 && (b[0] == '"' || b[0] == '-' || (b[0] >= '0' && b[0] <= '9')) {
			id = b
		}
	}
	if id == nil {
		return nil, nil, invalidRequest("no id: a request carries a number or a string")
	}
	for k := range m {
		if !slices.Contains(envelopeMembers, k) {
			return nil, id, invalidRequest(fmt.Sprintf("unknown member %q", k))
		}
	}
	var version, method string
	if json.Unmarshal(m["jsonrpc"], &version) != nil || version != protocol.JSONRPCVersion {
		return nil, id, invalidRequest(`jsonrpc must be "2.0"`)
	}
	if json.Unmarshal(m["method"], &method) != nil || method == "" {
		return nil, id, invalidRequest("no method")
	}
	return &request{id: id, method: method, params: m["params"]}, id, nil
}

func invalidRequest(msg string) *protocol.Error {
	return &protocol.Error{Code: protocol.RPCInvalidRequest, Message: msg,
		Data: protocol.ErrorData{Code: protocol.CodeBadRequest, Reason: protocol.ReasonBadRequest}}
}

func badParams(format string, args ...any) *protocol.Error {
	return &protocol.Error{Code: protocol.RPCInvalidParams, Message: fmt.Sprintf(format, args...),
		Data: protocol.ErrorData{Code: protocol.CodeBadRequest, Reason: protocol.ReasonBadRequest}}
}

// refused is a craze-level refusal: -32000 with its code and reason.
func refused(code protocol.Code, reason protocol.Reason, format string, args ...any) *protocol.Error {
	return &protocol.Error{Code: protocol.RPCRefused, Message: fmt.Sprintf(format, args...),
		Data: protocol.ErrorData{Code: code, Reason: reason}}
}

func isNull(raw json.RawMessage) bool {
	b := bytes.TrimSpace(raw)
	return len(b) == 0 || string(b) == "null"
}
