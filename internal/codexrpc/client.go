package codexrpc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/charliek/craze/internal/agentproc"
	"github.com/charliek/craze/internal/version"
)

const (
	// MaxLineBytes is the hard per-line frame cap enforced while reading stdout.
	MaxLineBytes = 16 << 20 // 16 MiB
	// MaxNotifyQueueBytes bounds buffered unread server notifications.
	MaxNotifyQueueBytes = 32 << 20 // 32 MiB
	// MaxNotifyQueueItems bounds buffered unread server notifications by count.
	MaxNotifyQueueItems = 8192
	// MaxInFlightRequests bounds concurrent server->client request goroutines.
	MaxInFlightRequests = 256

	jsonrpcVersion = "2.0"
	exitDrainGrace = 250 * time.Millisecond
)

// Decoder reads newline-delimited JSON-RPC frames from codex app-server with a
// hard streaming line-length cap enforced byte-for-byte as input arrives.
type Decoder struct {
	r        io.Reader
	buf      []byte
	pos      int
	end      int
	maxBytes int
}

// NewDecoder returns a Decoder enforcing MaxLineBytes per frame.
func NewDecoder(r io.Reader) *Decoder {
	return NewDecoderWithLimit(r, MaxLineBytes)
}

// NewDecoderWithLimit returns a Decoder enforcing maxBytes per frame.
func NewDecoderWithLimit(r io.Reader, maxBytes int) *Decoder {
	if maxBytes <= 0 {
		maxBytes = MaxLineBytes
	}
	return &Decoder{
		r:        r,
		buf:      make([]byte, 64<<10),
		maxBytes: maxBytes,
	}
}

// ReadMessage reads and decodes the next JSON object frame. It tolerates
// missing `"jsonrpc":"2.0"` and preserves JSON ID tokens (including integer 0).
func (d *Decoder) ReadMessage() (*Message, error) {
	for {
		line, err := d.readLine()
		if err != nil {
			return nil, err
		}
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 {
			continue
		}
		if trimmed[0] != '{' {
			return nil, fmt.Errorf("codexrpc ndjson: expected JSON object, got %q", truncateForError(trimmed))
		}
		var msg Message
		if err := json.Unmarshal(trimmed, &msg); err != nil {
			return nil, fmt.Errorf("codexrpc ndjson: %w", err)
		}
		return &msg, nil
	}
}

func (d *Decoder) readLine() ([]byte, error) {
	var line []byte
	for {
		if d.pos < d.end {
			avail := d.buf[d.pos:d.end]
			if idx := bytes.IndexByte(avail, '\n'); idx >= 0 {
				seg := avail[:idx]
				d.pos += idx + 1
				if len(line)+len(seg) > d.maxBytes {
					return nil, fmt.Errorf("%w: exceeded %d bytes", ErrLineTooLong, d.maxBytes)
				}
				line = append(line, seg...)
				line = bytes.TrimSuffix(line, []byte("\r"))
				return line, nil
			}
			if len(line)+len(avail) > d.maxBytes {
				d.pos, d.end = 0, 0
				return nil, fmt.Errorf("%w: exceeded %d bytes", ErrLineTooLong, d.maxBytes)
			}
			line = append(line, avail...)
			d.pos, d.end = 0, 0
		}

		// Read at most enough bytes to cross maxBytes by 1 so an open peer
		// that exceeds the cap without sending a newline fails immediately.
		rem := d.maxBytes - len(line) + 1
		if rem > len(d.buf) {
			rem = len(d.buf)
		}
		n, err := d.r.Read(d.buf[:rem])
		if n > 0 {
			d.pos = 0
			d.end = n
			avail := d.buf[:n]
			if idx := bytes.IndexByte(avail, '\n'); idx >= 0 {
				seg := avail[:idx]
				d.pos = idx + 1
				if len(line)+len(seg) > d.maxBytes {
					return nil, fmt.Errorf("%w: exceeded %d bytes", ErrLineTooLong, d.maxBytes)
				}
				line = append(line, seg...)
				line = bytes.TrimSuffix(line, []byte("\r"))
				return line, nil
			}
			if len(line)+len(avail) > d.maxBytes {
				d.pos, d.end = 0, 0
				return nil, fmt.Errorf("%w: exceeded %d bytes", ErrLineTooLong, d.maxBytes)
			}
			line = append(line, avail...)
			d.pos, d.end = 0, 0
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				if len(bytes.TrimSpace(line)) == 0 {
					return nil, io.EOF
				}
				line = bytes.TrimSuffix(line, []byte("\r"))
				return line, nil
			}
			return nil, err
		}
	}
}

func truncateForError(b []byte) string {
	const max = 80
	if len(b) <= max {
		return string(b)
	}
	return string(b[:max]) + "..."
}

func marshalFrame(msg *Message) ([]byte, error) {
	cp := *msg
	if cp.JSONRPC == "" {
		cp.JSONRPC = jsonrpcVersion
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(&cp); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Encoder serializes JSON-RPC frames as NDJSON under a mutex without mutating
// the caller's Message or HTML-escaping raw JSON IDs.
type Encoder struct {
	mu sync.Mutex
	w  io.Writer
}

func NewEncoder(w io.Writer) *Encoder {
	return &Encoder{w: w}
}

func (e *Encoder) WriteMessage(msg *Message) error {
	b, err := marshalFrame(msg)
	if err != nil {
		return err
	}
	return e.writeRaw(b)
}

func (e *Encoder) writeRaw(b []byte) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	_, err := e.w.Write(b)
	return err
}

type rpcResp struct {
	result json.RawMessage
	err    error
}

// SpawnOptions configures spawning `codex app-server --listen stdio://`.
type SpawnOptions struct {
	Binary     string
	Candidates []string
	Args       []string
	Dir        string
	Env        []string
	Stderr     io.Writer
}

// Client is a concurrent JSON-RPC v2 client for `codex app-server`.
//
// The stdout reader goroutine ONLY decodes frames, correlates RPC responses,
// enqueues notifications to a bounded ordered worker goroutine, and spawns a
// bounded goroutine per server->client request. Blocking inside a server-request
// handler (such as a parked approval or user-input ask) can never block
// reading responses (e.g. turn/interrupt), serverRequest/resolved, or EOF.
type Client struct {
	dec     *Decoder
	enc     *Encoder
	child   *agentproc.Child
	wCloser io.Closer
	rCloser io.Closer
	wOnce   sync.Once
	rOnce   sync.Once
	ended   func(error) error

	writeSem chan struct{}

	mu      sync.Mutex
	started bool
	nextID  int64
	pending map[string]chan rpcResp
	err     error
	closed  bool
	done    chan struct{}

	readDone   chan struct{}
	notifyDone chan struct{}

	// Bounded ordered notification queue (never blocks reader goroutine).
	notifyMu       sync.Mutex
	notifyCond     *sync.Cond
	notifyQueue    []*Message
	notifyBytes    int
	maxNotifyBytes int
	maxNotifyItems int
	notifyClosed   bool

	handlerMu sync.RWMutex
	onNotify  func(*Message)
	onRequest func(context.Context, *Message) bool

	reqCtx    context.Context
	reqCancel context.CancelFunc
	reqSem    chan struct{}
	reqWG     sync.WaitGroup

	childShutOnce sync.Once
	childShutDone chan struct{}

	closeOnce sync.Once
	closeDone chan struct{}
	closeErr  error
}

// NewInProcess constructs a Client over raw io.Reader / io.Writer streams (for
// tests or custom transports). Call SetNotifyHandler / SetRequestHandler before
// Start() if you need to observe the very first frame.
func NewInProcess(in io.Reader, out io.Writer) *Client {
	return newClient(in, out, nil)
}

func newClient(in io.Reader, out io.Writer, child *agentproc.Child) *Client {
	reqCtx, reqCancel := context.WithCancel(context.Background())
	c := &Client{
		dec:            NewDecoder(in),
		enc:            NewEncoder(out),
		child:          child,
		writeSem:       make(chan struct{}, 1),
		pending:        make(map[string]chan rpcResp),
		done:           make(chan struct{}),
		readDone:       make(chan struct{}),
		notifyDone:     make(chan struct{}),
		maxNotifyBytes: MaxNotifyQueueBytes,
		maxNotifyItems: MaxNotifyQueueItems,
		reqCtx:         reqCtx,
		reqCancel:      reqCancel,
		reqSem:         make(chan struct{}, MaxInFlightRequests),
		childShutDone:  make(chan struct{}),
		closeDone:      make(chan struct{}),
	}
	c.notifyCond = sync.NewCond(&c.notifyMu)
	if cl, ok := out.(io.Closer); ok {
		c.wCloser = cl
	}
	if cl, ok := in.(io.Closer); ok {
		c.rCloser = cl
	}
	return c
}

// Spawn starts `codex app-server --listen stdio://` via agentproc and begins
// reading frames.
func Spawn(opts SpawnOptions) (*Client, error) {
	child, stdout, stdin, err := agentproc.Start(agentproc.StartOptions{
		Binary:      opts.Binary,
		Candidates:  opts.Candidates,
		Args:        opts.Args,
		Dir:         opts.Dir,
		Env:         opts.Env,
		Stderr:      opts.Stderr,
		OwnedStdout: true,
	})
	if err != nil {
		return nil, err
	}
	c := newClient(stdout, stdin, child)
	c.ended = func(def error) error {
		return child.EndedWith("codexrpc", ErrClosed, def)
	}
	c.Start()
	go func() {
		<-child.StatusReady()
		// Close stdin once the child is gone, but let readLoop finish draining
		// any final replies already buffered in the owned stdout pipe before
		// failing remaining pending calls.
		c.closeWriter()
		<-child.Reaped()
		t := time.NewTimer(exitDrainGrace)
		defer t.Stop()
		select {
		case <-c.readDone:
			return
		case <-t.C:
			// A surviving descendant inherited stdout; force teardown now.
			c.abortTransport(child.EndErr("codexrpc", ErrClosed))
		}
	}()
	return c, nil
}

// SetNotifyHandler registers the handler invoked on the ordered notification
// goroutine for every server notification.
func (c *Client) SetNotifyHandler(fn func(*Message)) {
	c.handlerMu.Lock()
	c.onNotify = fn
	c.handlerMu.Unlock()
}

// SetRequestHandler registers the handler invoked on a dedicated goroutine per
// server->client request. Returning false (or leaving handler nil) causes the
// client to immediately reply with JSON-RPC -32601 (Method not found).
func (c *Client) SetRequestHandler(fn func(context.Context, *Message) bool) {
	c.handlerMu.Lock()
	c.onRequest = fn
	c.handlerMu.Unlock()
}

// Start begins reading stdout and dispatching notifications. Safe to call more
// than once; no-op if already started or closed. Spawn calls Start automatically.
func (c *Client) Start() {
	c.mu.Lock()
	if c.started || c.closed {
		c.mu.Unlock()
		return
	}
	c.started = true
	c.mu.Unlock()
	go c.notifyLoop()
	go c.readLoop()
}

// Done is closed when no further RPC calls can be answered.
func (c *Client) Done() <-chan struct{} { return c.done }

// ReadDone is closed when the stdout reader goroutine exits.
func (c *Client) ReadDone() <-chan struct{} { return c.readDone }

// NotifyDone is closed once all queued notifications read before EOF have been
// dispatched.
func (c *Client) NotifyDone() <-chan struct{} { return c.notifyDone }

// Err returns the terminal transport error, if any.
func (c *Client) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// PID returns the child process ID, or 0 for an in-process test client.
func (c *Client) PID() int { return c.child.PID() }

// ProcessGroup returns the child's process group ID, or 0 for an in-process
// client.
func (c *Client) ProcessGroup() int { return c.child.PGID() }

// Exited returns a channel closed when the child exits, or nil for in-process.
func (c *Client) Exited() <-chan struct{} { return c.child.Exited() }

// StderrTail returns the bounded tail of the child's stderr.
func (c *Client) StderrTail(wait time.Duration) []byte { return c.child.StderrTail(wait) }

// SnapshotTree captures the child's process tree for descendant cleanup.
func (c *Client) SnapshotTree() agentproc.TreeSnapshot { return c.child.SnapshotTree() }

// KillTree kills captured descendants and the child process group.
func (c *Client) KillTree(earlier agentproc.TreeSnapshot) { c.child.KillTree(earlier) }

func (c *Client) closedErr() error {
	c.mu.Lock()
	err := c.err
	c.mu.Unlock()
	if err == nil {
		return ErrClosed
	}
	return err
}

func (c *Client) writeFrame(ctx context.Context, msg *Message) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-c.done:
		return c.closedErr()
	default:
	}
	b, err := marshalFrame(msg)
	if err != nil {
		return err
	}
	select {
	case c.writeSem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		return c.closedErr()
	}
	if err := ctx.Err(); err != nil {
		<-c.writeSem
		return err
	}
	select {
	case <-c.done:
		<-c.writeSem
		return c.closedErr()
	default:
	}

	writeDone := make(chan error, 1)
	go func() {
		err := c.enc.writeRaw(b)
		<-c.writeSem
		writeDone <- err
	}()
	select {
	case err := <-writeDone:
		return err
	case <-ctx.Done():
		// A write may be partially flushed; tear down the transport so no
		// corrupted frame stays on the wire and the write goroutine unblocks.
		c.abortTransport(ctx.Err())
		return ctx.Err()
	case <-c.done:
		c.closeWriter()
		return c.closedErr()
	}
}

// Call sends a JSON-RPC request and waits for its response, unmarshaling
// result into out (when non-nil). Context cancellation covers both sending the
// request and waiting for its reply.
func (c *Client) Call(ctx context.Context, method string, params, out any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var paramRaw json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return err
		}
		paramRaw = b
	}
	idNum := atomic.AddInt64(&c.nextID, 1)
	idRaw := json.RawMessage(strconv.FormatInt(idNum, 10))
	key := string(idRaw)

	ch := make(chan rpcResp, 1)
	c.mu.Lock()
	if c.closed {
		err := c.err
		if err == nil {
			err = ErrClosed
		}
		c.mu.Unlock()
		return err
	}
	c.pending[key] = ch
	c.mu.Unlock()

	msg := &Message{
		JSONRPC: jsonrpcVersion,
		ID:      &idRaw,
		Method:  method,
		Params:  paramRaw,
	}
	if err := c.writeFrame(ctx, msg); err != nil {
		c.mu.Lock()
		_, mine := c.pending[key]
		delete(c.pending, key)
		c.mu.Unlock()
		if !mine {
			resp := <-ch
			if resp.err != nil {
				return resp.err
			}
			return decodeResult(resp.result, out)
		}
		return err
	}

	select {
	case resp := <-ch:
		if resp.err != nil {
			return resp.err
		}
		return decodeResult(resp.result, out)
	case <-ctx.Done():
		c.mu.Lock()
		_, mine := c.pending[key]
		delete(c.pending, key)
		c.mu.Unlock()
		if !mine {
			resp := <-ch
			if resp.err != nil {
				return resp.err
			}
			return decodeResult(resp.result, out)
		}
		return ctx.Err()
	case <-c.done:
		resp := <-ch
		if resp.err != nil {
			return resp.err
		}
		return decodeResult(resp.result, out)
	}
}

func decodeResult(raw json.RawMessage, out any) error {
	if out == nil || len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// Notify sends a JSON-RPC notification (no ID).
func (c *Client) Notify(ctx context.Context, method string, params any) error {
	var paramRaw json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return err
		}
		paramRaw = b
	}
	return c.writeFrame(ctx, &Message{
		JSONRPC: jsonrpcVersion,
		Method:  method,
		Params:  paramRaw,
	})
}

// Reply sends a JSON-RPC success response preserving the raw server request ID
// verbatim (including integer 0 and special characters).
func (c *Client) Reply(id json.RawMessage, result any) error {
	var raw json.RawMessage
	if result == nil {
		raw = json.RawMessage("null")
	} else {
		b, err := json.Marshal(result)
		if err != nil {
			return err
		}
		raw = b
	}
	idCopy := append(json.RawMessage(nil), id...)
	return c.writeFrame(c.reqCtx, &Message{
		JSONRPC: jsonrpcVersion,
		ID:      &idCopy,
		Result:  raw,
	})
}

// ReplyErr sends a JSON-RPC error response preserving the raw request ID.
func (c *Client) ReplyErr(id json.RawMessage, rpcErr *RPCError) error {
	idCopy := append(json.RawMessage(nil), id...)
	return c.writeFrame(c.reqCtx, &Message{
		JSONRPC: jsonrpcVersion,
		ID:      &idCopy,
		Error:   rpcErr,
	})
}

func (c *Client) closeWriter() {
	c.wOnce.Do(func() {
		if c.wCloser != nil {
			_ = c.wCloser.Close()
		}
	})
}

func (c *Client) closeReader() {
	c.rOnce.Do(func() {
		if c.rCloser != nil {
			_ = c.rCloser.Close()
		}
	})
}

func (c *Client) shutdownChild(tree agentproc.TreeSnapshot) {
	c.childShutOnce.Do(func() {
		defer close(c.childShutDone)
		if c.child == nil {
			return
		}
		c.child.Shutdown()
		c.child.KillTree(tree)
	})
	<-c.childShutDone
}

// abortTransport performs a non-blocking idempotent transport teardown for EOF,
// framing errors, queue/request overflow, or cancelled partial writes. It
// preserves err as the terminal transport error before stopping the child.
func (c *Client) abortTransport(err error) {
	c.failAll(err)
	c.reqCancel()
	var tree agentproc.TreeSnapshot
	if c.child != nil {
		tree = c.child.SnapshotTree()
	}
	c.closeWriter()
	c.closeReader()
	if c.child != nil {
		go c.shutdownChild(tree)
	}
}

// CloseAsync triggers Close in the background without waiting for worker
// goroutines. Safe to call from inside onNotify or onRequest callbacks.
func (c *Client) CloseAsync() {
	go func() { _ = c.Close() }()
}

// Close shuts down the child process (if any), closes pipes, cancels in-flight
// server-request handlers, and waits for all background goroutines. Idempotent.
// Safe to call even if Start was never called. Returns an error wrapping
// ErrAgentExited if the child had already exited before Close was called.
func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		defer close(c.closeDone)
		exitedFirst := false
		var tree agentproc.TreeSnapshot
		if c.child != nil {
			select {
			case <-c.child.Exited():
				exitedFirst = true
			default:
			}
			// Snapshot descendants BEFORE closing stdin or signaling the leader
			// so even descendants that changed process group are reaped cleanly.
			tree = c.child.SnapshotTree()
		}
		c.mu.Lock()
		wasStarted := c.started
		c.mu.Unlock()

		c.reqCancel()
		// Close stdin first so codex app-server sees EOF cleanly before SIGTERM.
		c.closeWriter()
		if c.child != nil {
			c.shutdownChild(tree)
			c.failAll(c.child.EndErr("codexrpc", ErrClosed))
		}
		c.failAll(ErrClosed)
		c.closeReader()
		if wasStarted {
			<-c.done
			<-c.readDone
			<-c.notifyDone
			c.reqWG.Wait()
		} else {
			c.closeNotifyQueue()
			close(c.readDone)
			close(c.notifyDone)
		}
		if c.child != nil {
			c.child.Wait()
			if exitedFirst {
				c.closeErr = agentproc.WrapAgentExited(ErrAgentExited, c.child.ExitErr())
			}
		}
	})
	<-c.closeDone
	return c.closeErr
}

func (c *Client) readLoop() {
	defer func() {
		c.closeNotifyQueue()
		close(c.readDone)
	}()
	for {
		msg, err := c.dec.ReadMessage()
		if err != nil {
			if errors.Is(err, io.EOF) {
				termErr := ErrClosed
				if c.ended != nil {
					termErr = c.ended(ErrClosed)
				}
				c.abortTransport(termErr)
				return
			}
			// Preserve framing/line-length errors verbatim before any child exit.
			c.abortTransport(err)
			return
		}
		switch {
		case msg.IsResponse():
			c.deliverResponse(msg)
		case msg.IsRequest():
			if !c.dispatchRequest(msg) {
				c.abortTransport(ErrTooManyRequests)
				return
			}
		case msg.IsNotification():
			if !c.enqueueNotify(msg) {
				c.abortTransport(ErrQueueOverflow)
				return
			}
		}
	}
}

func (c *Client) deliverResponse(msg *Message) {
	if msg.ID == nil {
		return
	}
	key := string(bytes.TrimSpace(*msg.ID))
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

func approxMessageBytes(msg *Message) int {
	if msg == nil {
		return 0
	}
	n := len(msg.Method) + len(msg.Params) + len(msg.Result) + 64
	if msg.ID != nil {
		n += len(*msg.ID)
	}
	return n
}

func (c *Client) enqueueNotify(msg *Message) bool {
	sz := approxMessageBytes(msg)
	c.notifyMu.Lock()
	defer c.notifyMu.Unlock()
	if c.notifyClosed {
		return true
	}
	if (c.maxNotifyItems > 0 && len(c.notifyQueue) >= c.maxNotifyItems) ||
		(c.maxNotifyBytes > 0 && c.notifyBytes+sz > c.maxNotifyBytes) {
		return false
	}
	c.notifyQueue = append(c.notifyQueue, msg)
	c.notifyBytes += sz
	c.notifyCond.Signal()
	return true
}

func (c *Client) closeNotifyQueue() {
	c.notifyMu.Lock()
	c.notifyClosed = true
	c.notifyCond.Broadcast()
	c.notifyMu.Unlock()
}

func (c *Client) notifyLoop() {
	defer close(c.notifyDone)
	for {
		c.notifyMu.Lock()
		for len(c.notifyQueue) == 0 && !c.notifyClosed {
			c.notifyCond.Wait()
		}
		if len(c.notifyQueue) == 0 && c.notifyClosed {
			c.notifyMu.Unlock()
			return
		}
		msg := c.notifyQueue[0]
		c.notifyQueue[0] = nil
		c.notifyQueue = c.notifyQueue[1:]
		c.notifyBytes -= approxMessageBytes(msg)
		if c.notifyBytes < 0 {
			c.notifyBytes = 0
		}
		if len(c.notifyQueue) == 0 {
			c.notifyQueue = nil
			c.notifyBytes = 0
		}
		c.notifyMu.Unlock()

		c.handlerMu.RLock()
		fn := c.onNotify
		c.handlerMu.RUnlock()
		if fn != nil {
			fn(msg)
		}
	}
}

func (c *Client) dispatchRequest(msg *Message) bool {
	select {
	case c.reqSem <- struct{}{}:
	default:
		return false
	}
	c.handlerMu.RLock()
	fn := c.onRequest
	c.handlerMu.RUnlock()

	c.reqWG.Add(1)
	go func() {
		defer func() {
			<-c.reqSem
			c.reqWG.Done()
		}()
		if fn == nil || !fn(c.reqCtx, msg) {
			if msg.ID != nil {
				_ = c.ReplyErr(*msg.ID, MethodNotFound(msg.Method))
			}
		}
	}()
	return true
}

func (c *Client) failAll(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.closed = true
	if c.err == nil {
		c.err = err
	}
	for k, ch := range c.pending {
		ch <- rpcResp{err: c.err}
		delete(c.pending, k)
	}
	close(c.done)
}

// --- High-level Codex app-server v2 RPC helpers ---

// Initialize performs the `initialize` request followed by the `initialized`
// notification with `experimentalApi: true`.
func (c *Client) Initialize(ctx context.Context) (*InitializeResult, error) {
	var res InitializeResult
	params := InitializeParams{
		ClientInfo: ClientInfo{
			Name:    "craze",
			Title:   "craze",
			Version: version.Version,
		},
		Capabilities: ClientCapabilities{
			ExperimentalAPI: true,
		},
	}
	if err := c.Call(ctx, MethodInitialize, params, &res); err != nil {
		return nil, err
	}
	if err := c.Notify(ctx, MethodInitialized, nil); err != nil {
		return nil, err
	}
	return &res, nil
}

func (c *Client) AccountRead(ctx context.Context, refreshToken bool) (*AccountReadResult, error) {
	var res AccountReadResult
	if err := c.Call(ctx, MethodAccountRead, AccountReadParams{RefreshToken: refreshToken}, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

func (c *Client) AccountRateLimitsRead(ctx context.Context) (*RateLimitsReadResult, error) {
	var res RateLimitsReadResult
	if err := c.Call(ctx, MethodAccountRateLimitsRead, struct{}{}, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

func (c *Client) ModelList(ctx context.Context, params ModelListParams) (*ModelListResult, error) {
	var res ModelListResult
	if err := c.Call(ctx, MethodModelList, params, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

func (c *Client) SkillsList(ctx context.Context, params SkillsListParams) (*SkillsListResult, error) {
	var res SkillsListResult
	if err := c.Call(ctx, MethodSkillsList, params, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

func (c *Client) CollaborationModeList(ctx context.Context) (*CollaborationModeListResult, error) {
	var res CollaborationModeListResult
	if err := c.Call(ctx, MethodCollaborationModeList, struct{}{}, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

func (c *Client) ThreadStart(ctx context.Context, params ThreadStartParams) (*ThreadStartResult, error) {
	var res ThreadStartResult
	if err := c.Call(ctx, MethodThreadStart, params, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

func (c *Client) ThreadResume(ctx context.Context, params ThreadResumeParams) (*ThreadResumeResult, error) {
	var res ThreadResumeResult
	if err := c.Call(ctx, MethodThreadResume, params, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

func (c *Client) ThreadTurnsList(ctx context.Context, params ThreadTurnsListParams) (*ThreadTurnsListResult, error) {
	var res ThreadTurnsListResult
	if err := c.Call(ctx, MethodThreadTurnsList, params, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

func (c *Client) ThreadItemsList(ctx context.Context, params ThreadItemsListParams) (*ThreadItemsListResult, error) {
	var res ThreadItemsListResult
	if err := c.Call(ctx, MethodThreadItemsList, params, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

func (c *Client) ThreadNameSet(ctx context.Context, threadID, name string) error {
	return c.Call(ctx, MethodThreadNameSet, ThreadNameSetParams{ThreadID: threadID, Name: name}, nil)
}

func (c *Client) TurnStart(ctx context.Context, params TurnStartParams) (*TurnStartResult, error) {
	var res TurnStartResult
	if err := c.Call(ctx, MethodTurnStart, params, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

func (c *Client) TurnInterrupt(ctx context.Context, threadID, turnID string) error {
	return c.Call(ctx, MethodTurnInterrupt, TurnInterruptParams{ThreadID: threadID, TurnID: turnID}, nil)
}

func (c *Client) TurnSteer(ctx context.Context, params TurnSteerParams) (*TurnSteerResult, error) {
	var res TurnSteerResult
	if err := c.Call(ctx, MethodTurnSteer, params, &res); err != nil {
		return nil, err
	}
	return &res, nil
}
