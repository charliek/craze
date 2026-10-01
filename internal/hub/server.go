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
// capabilities); every session-scoped method but session.connect is a host's,
// refused unsupported, reason host_only — the hub routes by splicing, never
// method by method (SQ14). sessions.list and sessions.subscribe (C11),
// session.connect (C12) and session.create (C15) are refused unsupported
// until the commit that serves each.

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

// closeAll closes every connection still open: teardown's last word to its
// clients. Each one's goroutine then ends.
func (s *server) closeAll() {
	for _, c := range s.h.life.openConns() {
		c.close()
	}
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
}

// serve reads and answers the connection's lines until it closes, the hello
// deadline passes, or an answer ends it.
func (c *conn) serve() {
	defer c.s.conns.Done()
	defer c.s.h.life.dropConn(c)
	defer c.close()
	_ = c.uc.SetReadDeadline(time.Now().Add(helloWait))
	for {
		line, err := c.lr.ReadLine()
		if errors.Is(err, protocol.ErrLineTooLong) {
			if !c.answer(func() bool {
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
		if !c.answer(func() bool { return c.dispatch(line) }) {
			return
		}
	}
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

// close closes the connection, once.
func (c *conn) close() {
	c.once.Do(func() { _ = c.uc.Close() })
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
	case req.method == protocol.MethodSessionsList, req.method == protocol.MethodSessionsSubscribe,
		req.method == protocol.MethodSessionConnect, req.method == protocol.MethodSessionCreate:
		// Served by the commits that build them (plan 032 C11, C12, C15).
		return c.replyErr(req.id, refused(protocol.CodeUnsupported, protocol.ReasonUnsupported,
			"%s is not served by this hub yet", req.method))
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
	return c.reply(req.id, protocol.HubHelloResult{
		Protocol: version,
		Endpoint: protocol.Endpoint{Kind: protocol.EndpointHub, HostID: h.id,
			CrazeVersion: h.version, PID: h.pid},
		Capabilities: protocol.HubCapabilities(),
		Codecs:       h.o.Codecs,
		Limits:       protocol.HostLimits(),
	})
}

// reply writes result as the answer to id: false when the write failed, which
// ends the connection.
func (c *conn) reply(id json.RawMessage, result any) bool {
	// MarshalLine's encoding (HTML not escaped), without its newline.
	raw, err := protocol.MarshalLine(result)
	if err != nil {
		return c.replyErr(id, refused(protocol.CodeFailed, protocol.ReasonFailed, "%v", err))
	}
	return c.write(protocol.Response{JSONRPC: protocol.JSONRPCVersion, ID: id, Result: bytes.TrimSuffix(raw, []byte{'\n'})})
}

// replyErr writes e as the answer to id (nil: null), as reply does.
func (c *conn) replyErr(id json.RawMessage, e *protocol.Error) bool {
	return c.write(protocol.Response{JSONRPC: protocol.JSONRPCVersion, ID: id, Error: e})
}

// write writes one line, bounded by writeWait: a peer that does not read it
// in that time is a connection that ends.
func (c *conn) write(v any) bool {
	line, err := protocol.MarshalLine(v)
	if err != nil {
		return false
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_ = c.uc.SetWriteDeadline(time.Now().Add(writeWait))
	c.writing.Store(true)
	_, err = c.uc.Write(line)
	c.writing.Store(false)
	return err == nil
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
