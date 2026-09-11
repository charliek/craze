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

type Conn struct {
	dec *Decoder
	enc *Encoder

	mu      sync.Mutex
	nextID  int64
	pending map[string]chan rpcResp
	err     error
	done    chan struct{}
	closed  bool

	onRequest func(*Message)
	onNotify  func(*Message)

	wCloser io.Closer
	rCloser io.Closer
}

func NewConn(in io.Reader, out io.Writer) *Conn {
	c := &Conn{
		dec:     NewDecoder(in),
		enc:     NewEncoder(out),
		pending: make(map[string]chan rpcResp),
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
	raw, err := c.callRaw(ctx, method, params)
	if err != nil {
		return err
	}
	if result == nil || len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	return json.Unmarshal(raw, result)
}

func (c *Conn) callRaw(ctx context.Context, method string, params any) (json.RawMessage, error) {
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
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, ErrClosed
	}
	c.pending[idKey(id)] = ch
	c.mu.Unlock()

	msg := &Message{JSONRPC: jsonrpcVersion, ID: id, Method: method, Params: paramRaw}
	if err := c.enc.WriteMessage(msg); err != nil {
		c.mu.Lock()
		delete(c.pending, idKey(id))
		c.mu.Unlock()
		return nil, err
	}

	select {
	case resp := <-ch:
		return resp.result, resp.err
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, idKey(id))
		c.mu.Unlock()
		return nil, ctx.Err()
	case <-c.done:
		c.mu.Lock()
		err := c.err
		c.mu.Unlock()
		if err == nil {
			err = ErrClosed
		}
		return nil, err
	}
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

func (c *Conn) deliver(msg *Message) {
	key := idKey(msg.ID)
	c.mu.Lock()
	ch, ok := c.pending[key]
	if ok {
		delete(c.pending, key)
	}
	c.mu.Unlock()
	if !ok {
		return
	}
	if msg.Error != nil {
		ch <- rpcResp{err: msg.Error}
		return
	}
	ch <- rpcResp{result: msg.Result}
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
	for key, ch := range c.pending {
		ch <- rpcResp{err: c.err}
		delete(c.pending, key)
	}
	close(c.done)
}
