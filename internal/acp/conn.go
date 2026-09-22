package acp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"sync/atomic"
)

var (
	ErrClosed         = errors.New("acp: connection closed")
	ErrPromptInFlight = errors.New("acp: prompt already in flight")
)

type rpcResp struct {
	result json.RawMessage
	err    error
}

// pendingCall is one request waiting for its reply. ch is buffered for the one
// answer it will ever carry, which is deliver's or failAll's — whichever takes
// the entry out of Conn.pending, and only that one. onReply, when set, is the
// request's reply hook (callReply).
type pendingCall struct {
	ch      chan rpcResp
	onReply func(json.RawMessage) error
}

type Conn struct {
	dec *Decoder
	enc *Encoder

	mu      sync.Mutex
	nextID  int64
	pending map[string]*pendingCall
	err     error
	done    chan struct{}
	closed  bool

	onRequest func(*Message)
	onNotify  func(*Message)

	// takenWait is a test seam, nil in production: it runs in callRaw's wait
	// when the call's context has ended, or the connection has closed, and the
	// request turns out to have been taken already — just before the call
	// receives the answer it is owed. It is the one point a test can hold to
	// prove a reply whose hook ran is never abandoned for either (callReply).
	// Set before the call is made and never written again.
	takenWait func()

	wCloser io.Closer
	rCloser io.Closer
}

func NewConn(in io.Reader, out io.Writer) *Conn {
	c := &Conn{
		dec:     NewDecoder(in),
		enc:     NewEncoder(out),
		pending: make(map[string]*pendingCall),
		done:    make(chan struct{}),
	}
	if closer, ok := out.(io.Closer); ok {
		c.wCloser = closer
	}
	if closer, ok := in.(io.Closer); ok {
		c.rCloser = closer
	}
	return c
}

func (c *Conn) SetRequestHandler(fn func(*Message)) {
	c.onRequest = fn
}

func (c *Conn) SetNotifyHandler(fn func(*Message)) {
	c.onNotify = fn
}

func (c *Conn) Start() {
	go c.readLoop()
}

func (c *Conn) Done() <-chan struct{} {
	return c.done
}

func (c *Conn) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

func (c *Conn) Call(ctx context.Context, method string, params, result any) error {
	return c.callSent(ctx, method, params, result, nil)
}

// callSent is Call with a signal for the one caller that has to know when its
// request left: sent runs once, after the request's bytes were written and
// before the reply is waited on, and never when the write failed or the
// connection was already closed — nothing reached the agent then. It runs on
// the goroutine that made the call, inside it, so it must do no I/O and must
// not block: a hook that blocked would hold this call's reply wait with it.
// It publishes, and that is all.
func (c *Conn) callSent(ctx context.Context, method string, params, result any, sent func()) error {
	raw, err := c.callRaw(ctx, method, params, sent, nil)
	if err != nil {
		return err
	}
	if result == nil || len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	return json.Unmarshal(raw, result)
}

// callReply is Call for a request whose successful reply has to be acted on
// in arrival order: onReply runs on the read goroutine, inside deliver, with
// the reply's result, BEFORE the reply is handed to this caller — so whatever
// it does is ordered against every notification the read loop dispatches, the
// ones ahead of the reply and the ones behind it, by where they sit on the
// wire and never by when this caller's goroutine is next scheduled. It runs at
// most once, only for a request still pending when its reply arrives, and
// never for an error reply. An error it returns is the call's answer in the
// reply's place.
//
// It runs on the read loop, so it must not block and must do no I/O on this
// connection: a hook that waited for the wire would be waiting for itself.
//
// Once onReply has run, this call answers with that reply — never with its
// context's error and never with the connection's close — because what the
// hook did is then a fact its caller has to be told about (callRaw's wait).
func (c *Conn) callReply(ctx context.Context, method string, params any, onReply func(json.RawMessage) error) (json.RawMessage, error) {
	return c.callRaw(ctx, method, params, nil, onReply)
}

func (c *Conn) callRaw(ctx context.Context, method string, params any, sent func(), onReply func(json.RawMessage) error) (json.RawMessage, error) {
	paramRaw, err := marshalRaw(params)
	if err != nil {
		return nil, err
	}
	idNum := atomic.AddInt64(&c.nextID, 1)
	id, err := json.Marshal(idNum)
	if err != nil {
		return nil, err
	}
	ch := make(chan rpcResp, 1)
	key := idKey(id)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, ErrClosed
	}
	c.pending[key] = &pendingCall{ch: ch, onReply: onReply}
	c.mu.Unlock()

	msg := &Message{JSONRPC: jsonrpcVersion, ID: id, Method: method, Params: paramRaw}
	if err := c.enc.WriteMessage(msg); err != nil {
		c.mu.Lock()
		delete(c.pending, key)
		c.mu.Unlock()
		return nil, err
	}
	// The encoder writes each frame whole under its own lock, so from here
	// every later write on this connection — a session/cancel included — is
	// behind this request in the pipe.
	if sent != nil {
		sent()
	}

	// The wait. Whoever takes this request out of c.pending — deliver with its
	// reply, failAll with the connection's error, or this call giving up — is
	// the one who answers it, and deliver and failAll both send on ch right
	// after taking it (deliver once the reply hook has run). So once the entry
	// is gone, ch is about to carry the answer, and that answer is the one to
	// return: a reply whose hook has acted must reach its caller, or the caller
	// would report as failed a change the read loop has already installed, and
	// whatever it owes that change — the delta announcing it — would never be
	// written.
	//
	// That is why neither branch below may simply give up. With the reply
	// already in ch and the context done, or the connection closed, a select is
	// free to pick either case, so each one checks whether the request is still
	// this call's to abandon.
	select {
	case resp := <-ch:
		return resp.result, resp.err
	case <-ctx.Done():
		c.mu.Lock()
		_, mine := c.pending[key]
		delete(c.pending, key)
		c.mu.Unlock()
		if !mine {
			return c.takenAnswer(ch)
		}
		return nil, ctx.Err()
	case <-c.done:
		// failAll empties c.pending before it closes done, answering every
		// entry it finds there, and an entry deliver took before it is
		// answered by deliver: either way ch is carrying this call's answer —
		// the reply, or the close's error — and nobody else will take it.
		return c.takenAnswer(ch)
	}
}

// takenAnswer is the answer a call is owed once its request has been taken,
// received from the call's channel. The send it waits for is deliver's, which
// follows a reply hook that does not block, or failAll's, which has been made
// already, so the wait is short and bounded.
func (c *Conn) takenAnswer(ch chan rpcResp) (json.RawMessage, error) {
	if c.takenWait != nil {
		c.takenWait()
	}
	resp := <-ch
	return resp.result, resp.err
}

func (c *Conn) Notify(ctx context.Context, method string, params any) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	paramRaw, err := marshalRaw(params)
	if err != nil {
		return err
	}
	return c.enc.WriteMessage(&Message{JSONRPC: jsonrpcVersion, Method: method, Params: paramRaw})
}

func (c *Conn) Reply(id json.RawMessage, result any) error {
	raw, err := marshalRaw(result)
	if err != nil {
		return err
	}
	if raw == nil {
		raw = json.RawMessage("null")
	}
	return c.enc.WriteMessage(&Message{JSONRPC: jsonrpcVersion, ID: id, Result: raw})
}

func (c *Conn) ReplyErr(id json.RawMessage, rpcErr *RPCError) error {
	return c.enc.WriteMessage(&Message{JSONRPC: jsonrpcVersion, ID: id, Error: rpcErr})
}

func (c *Conn) Close() error {
	c.failAll(ErrClosed)
	if c.wCloser != nil {
		_ = c.wCloser.Close()
	}
	if c.rCloser != nil {
		_ = c.rCloser.Close()
	}
	return nil
}

func (c *Conn) readLoop() {
	defer c.failAll(io.EOF)
	for {
		msg, err := c.dec.ReadMessage()
		if err != nil {
			if errors.Is(err, io.EOF) {
				c.failAll(ErrClosed)
				return
			}
			c.failAll(err)
			return
		}
		switch {
		case msg.IsResponse():
			c.deliver(msg)
		case msg.IsRequest():
			if c.onRequest != nil {
				c.onRequest(msg)
			} else {
				_ = c.ReplyErr(msg.ID, MethodNotFound(msg.Method))
			}
		case msg.IsNotification():
			if c.onNotify != nil {
				c.onNotify(msg)
			}
		}
	}
}

// deliver hands a reply to the call waiting for it. The entry is taken out of
// c.pending first, so a reply that arrives after its caller gave up, or a
// second reply to one id, is dropped, and one taken here is this call's for
// good: its reply hook runs now — on the read goroutine, before anything behind
// the reply on the wire is read — and its answer, the reply or the hook's error
// in its place, is sent the moment the hook returns (callRaw's wait).
func (c *Conn) deliver(msg *Message) {
	key := idKey(msg.ID)
	c.mu.Lock()
	p, ok := c.pending[key]
	if ok {
		delete(c.pending, key)
	}
	c.mu.Unlock()
	if !ok {
		return
	}
	if msg.Error != nil {
		p.ch <- rpcResp{err: msg.Error}
		return
	}
	if p.onReply != nil {
		if err := p.onReply(msg.Result); err != nil {
			p.ch <- rpcResp{err: err}
			return
		}
	}
	p.ch <- rpcResp{result: msg.Result}
}

func (c *Conn) failAll(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.closed = true
	if c.err == nil {
		c.err = err
	}
	for key, p := range c.pending {
		p.ch <- rpcResp{err: c.err}
		delete(c.pending, key)
	}
	close(c.done)
}
