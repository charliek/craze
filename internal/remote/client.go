package remote

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/charliek/craze/internal/protocol"
)

// Options configure a Client (Dial).
type Options struct {
	// Client is who is connecting, as hello's client says it. An empty Kind
	// is "remote".
	Client protocol.ClientInfo
	// Resume is a resume state to start from (ResumeState, A4): the client id
	// a restarted process held, its token, and the next command id it would
	// have minted. Dial says hello{resume} with them, and the host answers
	// resumed: true (the same client, the same engine incarnation) or a fresh
	// id. nil says hello as a new client, its command ids counted from 1.
	Resume *ResumeState
	// Connect, when set, is the session to reach through a hub (plan 027
	// §3.3's splice): every connection first says hello to the hub —
	// accepting its hub-kind answer (X5), and only here — and asks
	// session.connect{sessionId: Connect}; on {} the hub splices the
	// connection to that session's host, and the client says hello to the
	// host itself, so client ids, tokens and receipts are always the host's.
	// Empty dials a host directly, and a hub answering there is an error
	// (ErrEndpoint).
	Connect string
	// PeerCheck is run on every connection before a byte is written (PR 2's
	// rundir installs one; nil checks nothing). A connection that is not a
	// *net.UnixConn fails a non-nil check.
	PeerCheck func(*net.UnixConn) error
	// Dial opens the transport to path. nil dials a Unix socket. A test's
	// seam: it may wrap the connection.
	Dial func(ctx context.Context, path string) (net.Conn, error)
	// Redials bounds reconnection (plan 027 §3.14): at most Redials dial
	// attempts within any RedialWindow; the next one gives up — outstanding
	// commands resolve ErrOutcomeUnknown (reason disconnected) and the stream
	// hands up an Error item carrying ErrDisconnected. Zero values are 3
	// within 10 s.
	Redials      int
	RedialWindow time.Duration
	// RedialBackoff is the wait before a redial that follows a failed one,
	// doubling up to 2 s; the first redial after a loss is immediate. Zero is
	// 100 ms.
	RedialBackoff time.Duration
	// HandshakeTimeout bounds a connection's opening exchange — the hub hop
	// and hello. Zero is 30 s.
	HandshakeTimeout time.Duration
	// StreamBytes bounds the stream's items not yet read by Next, in bytes
	// (an event's body, a snapshot): a reader that finds no room waits, so a
	// consumer that stops reading backs the connection up and the host drops
	// the subscription slow_consumer, as it would a socket nobody reads. One
	// item always fits in an empty queue. Zero is 16 MiB, the longest line a
	// host writes.
	StreamBytes int
}

// The defaults of Options' zero values, and the client's own waits.
const (
	defaultRedials          = 3
	defaultRedialWindow     = 10 * time.Second
	defaultRedialBackoff    = 100 * time.Millisecond
	maxRedialBackoff        = 2 * time.Second
	defaultHandshakeTimeout = 30 * time.Second
	// retryBackoffMin and retryBackoffMax bound the wait before a resend of
	// the same command id: a caller's retry by code (CommandOptions.Retry),
	// and a resend after a resume answered in_progress, waited out.
	retryBackoffMin = 10 * time.Millisecond
	retryBackoffMax = time.Second
	// longestRequestID is the longest request id the client can mint, for
	// sizing a request before it is given one.
	longestRequestID = "18446744073709551615"
)

// ResumeState is what a caller persists so a new process can pick up where
// this one was (A4's second leg): the client id and its resume token, the next
// command id this client will mint, and the stream's cursor — the
// {incarnation, seq} of the last event Next handed out, nil before any. Dial
// takes the first three (Options.Resume); the cursor is AttachOptions.Cursor's.
type ResumeState struct {
	ClientID    string
	Token       string
	NextCommand uint64
	Cursor      *protocol.Cursor
}

// Client is one client of a craze host over a Unix socket (plan 027 §3.14,
// PR 1's core). See the package doc for its roles and the resend rule.
type Client struct {
	path string
	opts Options

	// ctx is the client's life: Close and a terminal failure end it, which
	// ends every dial, handshake and wait of its own.
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// nextReq mints request ids (per client, so unique on every connection).
	nextReq atomic.Uint64

	mu sync.Mutex
	// cur is the connection calls go to, nil while the client reconnects.
	cur *wire
	// changed is closed and replaced whenever cur or err changes.
	changed chan struct{}
	// err is why the client stopped (Close, spent redials, the session's end);
	// nil while it runs. done is closed when it is set.
	err  error
	done chan struct{}
	// hello is the host's last hello answer.
	hello protocol.HelloResult
	// identity counts the client ids this client has held: it moves on every
	// hello that did not resume (a fresh id). A command that may have run
	// under one identity is never sent under another — resumed: true is the
	// one way two connections share an identity (the resend rule).
	identity uint64
	// nextCmd is the next command id to mint. It never goes back, across
	// client ids either, so no id is ever reused (the host allows gaps).
	nextCmd uint64
	// cmds is every command whose caller is waiting, in mint order.
	cmds []*command
	// stream is the open stream (Attach), nil when none.
	stream *Stream
	// redials is when each redial of the current window was attempted.
	redials []time.Time
	// voidToken says the next hello is a fresh one: the stream saw
	// reset{session_replaced}, or the host refused the token (bad_token).
	voidToken bool
	// ended says the stream saw reset{session_closed}: the session is over,
	// and a lost connection is not redialled.
	ended bool
}

// wire is one connection: its reader's framing, its writer's lock, and the
// replies it owes.
type wire struct {
	nc   net.Conn
	lr   *protocol.LineReader
	wmu  sync.Mutex
	gone chan struct{} // closed once the reader has exited
	// maxLine is the host's inbound line limit (hello's limits), which no
	// request may exceed.
	maxLine int

	mu      sync.Mutex
	pending map[string]replyFunc
	dead    bool
}

// replyFunc receives a request's reply — or, with err set, the news that the
// connection went first. It runs on the connection's reader.
type replyFunc func(resp *protocol.Response, err error)

func newWire(nc net.Conn) *wire {
	return &wire{
		nc:      nc,
		lr:      protocol.NewLineReader(nc, protocol.OutboundLineMax),
		gone:    make(chan struct{}),
		maxLine: protocol.InboundLineMax,
		pending: map[string]replyFunc{},
	}
}

// take removes and returns id's reply function, nil when none is owed.
func (w *wire) take(id string) replyFunc {
	w.mu.Lock()
	defer w.mu.Unlock()
	fn := w.pending[id]
	delete(w.pending, id)
	return fn
}

// Dial connects to the host at path — through a hub when opts.Connect names
// a session — and says hello, resuming opts.Resume's client when it is set.
// A hello the host refuses is its *Error (a *VersionError for
// protocol_version, with the versions it speaks).
func Dial(ctx context.Context, path string, opts Options) (*Client, error) {
	if opts.Client.Kind == "" {
		opts.Client.Kind = "remote"
	}
	if opts.Redials <= 0 {
		opts.Redials = defaultRedials
	}
	if opts.RedialWindow <= 0 {
		opts.RedialWindow = defaultRedialWindow
	}
	if opts.RedialBackoff <= 0 {
		opts.RedialBackoff = defaultRedialBackoff
	}
	if opts.HandshakeTimeout <= 0 {
		opts.HandshakeTimeout = defaultHandshakeTimeout
	}
	if opts.StreamBytes <= 0 {
		opts.StreamBytes = protocol.OutboundLineMax
	}
	cctx, cancel := context.WithCancel(context.Background())
	c := &Client{path: path, opts: opts, ctx: cctx, cancel: cancel,
		changed: make(chan struct{}), done: make(chan struct{}), nextCmd: 1, identity: 1}
	var resume *protocol.Resume
	if r := opts.Resume; r != nil {
		if r.ClientID != "" {
			resume = &protocol.Resume{ClientID: r.ClientID, Token: r.Token}
		}
		c.nextCmd = max(r.NextCommand, 1)
	}
	w, h, err := c.open(ctx, resume)
	if err != nil {
		cancel()
		return nil, err
	}
	c.mu.Lock()
	c.hello = h
	c.cur = w
	c.startReaderLocked(w)
	c.mu.Unlock()
	return c, nil
}

func dialUnix(ctx context.Context, path string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "unix", path)
}

// open dials and runs the opening exchange (handshake) on a new connection,
// whose reader is not started: the caller starts it.
func (c *Client) open(ctx context.Context, resume *protocol.Resume) (*wire, protocol.HelloResult, error) {
	var none protocol.HelloResult
	dial := c.opts.Dial
	if dial == nil {
		dial = dialUnix
	}
	nc, err := dial(ctx, c.path)
	if err != nil {
		return nil, none, err
	}
	if check := c.opts.PeerCheck; check != nil {
		err := errors.New("not a unix socket connection")
		if uc, ok := nc.(*net.UnixConn); ok {
			err = check(uc)
		}
		if err != nil {
			_ = nc.Close()
			return nil, none, fmt.Errorf("remote: peer check: %w", err)
		}
	}
	w := newWire(nc)
	// The exchange is bounded, and ends with ctx (the caller's, or the
	// client's life): closing the connection ends a read or write in flight.
	stop := context.AfterFunc(ctx, func() { _ = nc.Close() })
	_ = nc.SetDeadline(time.Now().Add(c.opts.HandshakeTimeout))
	h, err := c.handshake(w, resume)
	stopped := stop()
	_ = nc.SetDeadline(time.Time{})
	if err == nil && !stopped {
		err = ctx.Err()
	}
	if err != nil {
		_ = nc.Close()
		if ctx.Err() != nil {
			return nil, none, ctx.Err()
		}
		return nil, none, err
	}
	if h.Limits.InboundLine > 0 {
		w.maxLine = h.Limits.InboundLine
	}
	return w, h, nil
}

// handshake is a connection's opening exchange (§3.2, §3.3): the hub hop when
// Options.Connect is set — hello to the hub, which must answer as a hub, and
// session.connect — and then hello to the host, which must answer as one.
func (c *Client) handshake(w *wire, resume *protocol.Resume) (protocol.HelloResult, error) {
	var none protocol.HelloResult
	hp := protocol.HelloParams{Protocols: protocol.SupportedProtocols(), Client: c.opts.Client}
	if c.opts.Connect != "" {
		var hub protocol.HubHelloResult
		if err := c.syncCall(w, protocol.MethodHello, hp, &hub); err != nil {
			return none, helloFailed(err)
		}
		switch {
		case hub.Endpoint.Kind != protocol.EndpointHub:
			return none, fmt.Errorf("%w: the hub hop's hello was answered by a %q endpoint, not a hub", ErrEndpoint, hub.Endpoint.Kind)
		case !hub.Capabilities.Connect:
			return none, fmt.Errorf("%w: the hub says it cannot connect a session (capabilities.connect is false)", ErrEndpoint)
		}
		if err := c.syncCall(w, protocol.MethodSessionConnect, protocol.ConnectParams{SessionID: c.opts.Connect}, nil); err != nil {
			return none, err
		}
	}
	hp.Resume = resume
	var h protocol.HelloResult
	if err := c.syncCall(w, protocol.MethodHello, hp, &h); err != nil {
		return none, helloFailed(err)
	}
	switch {
	case h.Endpoint.Kind == protocol.EndpointHub:
		return none, fmt.Errorf("%w: a hub answered where a session's host belongs; name the session in Options.Connect to go through it", ErrEndpoint)
	case h.Endpoint.Kind != protocol.EndpointHost:
		return none, fmt.Errorf("%w: an endpoint of kind %q answered", ErrEndpoint, h.Endpoint.Kind)
	case !slices.Contains(protocol.SupportedProtocols(), h.Protocol):
		return none, fmt.Errorf("remote: the host chose protocol %d, which this client does not speak", h.Protocol)
	case h.ClientID == "" || h.Token == "":
		return none, errors.New("remote: the host's hello carries no client id or token")
	}
	return h, nil
}

// helloFailed is a refused hello as Dial returns it.
func helloFailed(err error) error {
	var e *Error
	if errors.As(err, &e) {
		return helloError(e)
	}
	return err
}

// syncCall is one call on a connection whose reader is not running: it writes
// the request and reads lines until its reply, ignoring anything else.
func (c *Client) syncCall(w *wire, method string, params, result any) error {
	raw, err := paramsJSON(params)
	if err != nil {
		return err
	}
	id, line, err := c.requestLine(method, raw)
	if err != nil {
		return err
	}
	if len(line)-1 > w.maxLine {
		return ErrRequestTooLarge
	}
	if _, err := w.nc.Write(line); err != nil {
		return err
	}
	for {
		b, err := w.lr.ReadLine()
		if errors.Is(err, protocol.ErrLineTooLong) {
			continue
		}
		if err != nil {
			return err
		}
		m, ok := parseLine(b)
		if !ok || m.isNotification() || string(m.ID) != id {
			continue
		}
		return decodeReply(m.response(), result)
	}
}

// requestLine mints a request id and marshals the request as one line.
func (c *Client) requestLine(method string, params json.RawMessage) (string, []byte, error) {
	id := strconv.FormatUint(c.nextReq.Add(1), 10)
	line, err := protocol.MarshalLine(protocol.Request{JSONRPC: protocol.JSONRPCVersion,
		ID: json.RawMessage(id), Method: method, Params: params})
	return id, line, err
}

// errUnsent is a send that wrote no byte of its request: the connection was
// already gone. It is ErrConnectionLost to a caller; a command knows from it
// that this attempt cannot have run.
var errUnsent = fmt.Errorf("%w (nothing was sent)", ErrConnectionLost)

// send writes one request on w; fn, when not nil, receives its reply on w's
// reader. It fails when w is gone (errUnsent), when the request is over the
// host's inbound limit (ErrRequestTooLarge; nothing sent), or when the write
// fails (ErrConnectionLost: some of it may have gone), which closes w — its
// reader then exits, and the client reconnects.
func (c *Client) send(w *wire, method string, params json.RawMessage, fn replyFunc) (string, error) {
	id, line, err := c.requestLine(method, params)
	if err != nil {
		return "", err
	}
	if len(line)-1 > w.maxLine {
		return "", ErrRequestTooLarge
	}
	w.mu.Lock()
	if w.dead {
		w.mu.Unlock()
		return "", errUnsent
	}
	if fn != nil {
		w.pending[id] = fn
	}
	w.mu.Unlock()
	w.wmu.Lock()
	n, err := w.nc.Write(line)
	w.wmu.Unlock()
	if err != nil {
		w.take(id)
		_ = w.nc.Close()
		if n == 0 {
			return "", errUnsent
		}
		return "", ErrConnectionLost
	}
	return id, nil
}

// startReaderLocked starts w's reader; c.mu is held (so Close, which sets err
// under it before it waits, never misses one).
func (c *Client) startReaderLocked(w *wire) {
	if c.err != nil {
		_ = w.nc.Close()
		close(w.gone)
		return
	}
	c.wg.Add(1)
	go c.read(w)
}

// read is a connection's one reader: it demultiplexes replies (by request id)
// and notifications (to the stream), in the order the host wrote them. When
// the connection ends — EOF, a read error, or a line over the 16 MiB a host
// may write — every reply it owed is told so, and the client reconnects.
func (c *Client) read(w *wire) {
	defer c.wg.Done()
	for {
		line, err := w.lr.ReadLine()
		if err != nil {
			break
		}
		m, ok := parseLine(line)
		if !ok {
			// Not a JSON object: nothing a host writes. Ignored, as anything
			// the client does not know is.
			continue
		}
		switch {
		case m.isNotification():
			c.notification(w, m.Method, m.Params)
		case m.Method == "" && len(m.ID) > 0:
			if fn := w.take(string(m.ID)); fn != nil {
				fn(m.response(), nil)
			}
		}
	}
	_ = w.nc.Close()
	w.mu.Lock()
	w.dead = true
	pending := w.pending
	w.pending = nil
	w.mu.Unlock()
	c.mu.Lock()
	lost := ErrConnectionLost
	if c.err != nil {
		lost = c.err
	}
	c.mu.Unlock()
	for _, fn := range pending {
		fn(nil, lost)
	}
	close(w.gone)
	c.lost(w)
}

// inMsg is one line a host wrote, decoded as far as routing needs: a reply
// (an id, no method) or a notification (a method, no id). Unknown members are
// ignored.
type inMsg struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  *protocol.Error `json:"error"`
}

func parseLine(b []byte) (inMsg, bool) {
	var m inMsg
	if err := json.Unmarshal(b, &m); err != nil {
		return inMsg{}, false
	}
	return m, true
}

func (m inMsg) isNotification() bool { return m.Method != "" && len(m.ID) == 0 }

func (m inMsg) response() *protocol.Response {
	return &protocol.Response{JSONRPC: protocol.JSONRPCVersion, ID: m.ID, Result: m.Result, Error: m.Error}
}

// notification routes one notification to the stream. A method protocol 1
// does not name is ignored (tolerant inbound, §3.2).
func (c *Client) notification(w *wire, method string, params json.RawMessage) {
	switch method {
	case protocol.NotifyEvent, protocol.NotifySynchronized, protocol.NotifyReady, protocol.NotifyReset:
	default:
		return
	}
	c.mu.Lock()
	s := c.stream
	c.mu.Unlock()
	if s != nil {
		s.note(w, method, params)
	}
}

// decodeReply is a reply's result decoded into result (nil: ignored), or its
// error as an *Error. Unknown result fields are ignored.
func decodeReply(resp *protocol.Response, result any) error {
	if resp.Error != nil {
		return newError(resp.Error)
	}
	if result == nil || len(resp.Result) == 0 {
		return nil
	}
	if err := json.Unmarshal(resp.Result, result); err != nil {
		return fmt.Errorf("remote: decoding the result: %w", err)
	}
	return nil
}

// paramsJSON is params as the object a request carries: nil is {}, a
// json.RawMessage is taken as it is.
func paramsJSON(params any) (json.RawMessage, error) {
	switch p := params.(type) {
	case nil:
		return json.RawMessage(`{}`), nil
	case json.RawMessage:
		return p, nil
	}
	b, err := protocol.MarshalLine(params)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(b[:len(b)-1]), nil
}

// changedLocked wakes every wait on cur or err; c.mu is held.
func (c *Client) changedLocked() {
	close(c.changed)
	c.changed = make(chan struct{})
}

// wire is the connection calls go to, waiting while the client reconnects.
func (c *Client) wire(ctx context.Context) (*wire, error) {
	for {
		c.mu.Lock()
		if c.err != nil {
			err := c.err
			c.mu.Unlock()
			return nil, err
		}
		if w := c.cur; w != nil {
			c.mu.Unlock()
			return w, nil
		}
		ch := c.changed
		c.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// Call makes one call that is not a command — a read (sessions.list,
// session.state, session.snapshot, session.sync, asks.list, asks.get) or
// session.detach — and decodes its result into result (nil: ignored). A
// refusal is an *Error. While the client reconnects, the call waits for the
// connection; one whose connection goes before its reply is ErrConnectionLost
// and may be sent again. Commands go through Command (their ids and resends
// are the client's), attaches through Attach, and hello is the client's own.
func (c *Client) Call(ctx context.Context, method string, params, result any) error {
	if info, ok := protocol.Method(method); ok && info.Mutating {
		return fmt.Errorf("remote: %s is a command: use Command", method)
	}
	switch method {
	case protocol.MethodHello, protocol.MethodSessionConnect:
		return fmt.Errorf("remote: %s is the client's own", method)
	case protocol.MethodSessionAttach:
		return fmt.Errorf("remote: %s is Attach's", method)
	}
	raw, err := paramsJSON(params)
	if err != nil {
		return err
	}
	w, err := c.wire(ctx)
	if err != nil {
		return err
	}
	ch := make(chan cmdResult, 1)
	id, err := c.send(w, method, raw, func(resp *protocol.Response, err error) { ch <- cmdResult{resp: resp, err: err} })
	if err != nil {
		return err
	}
	select {
	case r := <-ch:
		if r.err != nil {
			return r.err
		}
		return decodeReply(r.resp, result)
	case <-ctx.Done():
		w.take(id)
		return ctx.Err()
	}
}

// Hello is the host's last hello answer: its endpoint, the client id and
// token, whether that hello resumed, the capabilities, codecs, limits and
// retry horizon.
func (c *Client) Hello() protocol.HelloResult {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hello
}

// ClientID is the client id the host bound this client to last: it changes
// when a reconnect's hello did not resume.
func (c *Client) ClientID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hello.ClientID
}

// ResumeState is what a caller persists so a new process can Dial from it
// (A4): the client id, its token, the next command id, and the stream's
// cursor (nil before the stream has handed out an attach).
func (c *Client) ResumeState() ResumeState {
	c.mu.Lock()
	st := ResumeState{ClientID: c.hello.ClientID, Token: c.hello.Token, NextCommand: c.nextCmd}
	s := c.stream
	c.mu.Unlock()
	if s != nil {
		st.Cursor = s.Cursor()
	}
	return st
}

// Done is closed once the client has stopped: Close, spent redials
// (ErrDisconnected), or the session's end (ErrSessionEnded). Err says which.
func (c *Client) Done() <-chan struct{} { return c.done }

// Err is why the client stopped, nil while it runs.
func (c *Client) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// Close stops the client: its connection closes (the host releases its client
// id, which a later Dial may resume with its token), every command still
// waiting resolves — ErrOutcomeUnknown (disconnected) if it may have run,
// ErrNotRun if it cannot have — the stream hands up an Error item carrying
// ErrClosed, and Close returns once the client's goroutines have.
func (c *Client) Close() error {
	c.terminate(ErrClosed)
	c.wg.Wait()
	return nil
}

// terminate stops the client with err, once.
func (c *Client) terminate(err error) {
	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		return
	}
	c.err = err
	close(c.done)
	c.changedLocked()
	for _, cmd := range c.cmds {
		if cmd.want {
			c.resolveLocked(cmd, protocol.ReasonDisconnected)
		}
	}
	s := c.stream
	w := c.cur
	c.cur = nil
	c.mu.Unlock()
	c.cancel()
	if s != nil {
		s.fail(err)
	}
	if w != nil {
		_ = w.nc.Close()
	}
}
