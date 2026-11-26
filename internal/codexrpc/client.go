package codexrpc

import (
	"bufio"
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

// MaxLineBytes is the hard per-line frame cap enforced while reading stdout.
const MaxLineBytes = 16 << 20 // 16 MiB

const jsonrpcVersion = "2.0"

// Decoder reads newline-delimited JSON-RPC frames from codex app-server with a
// hard streaming line-length cap.
type Decoder struct {
	r        *bufio.Reader
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
		r:        bufio.NewReaderSize(r, 64<<10),
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
	var buf []byte
	for {
		chunk, err := d.r.ReadSlice('\n')
		if len(buf)+len(chunk) > d.maxBytes {
			return nil, fmt.Errorf("%w: exceeded %d bytes", ErrLineTooLong, d.maxBytes)
		}
		buf = append(buf, chunk...)
		if err == nil {
			break
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) {
			if len(bytes.TrimSpace(buf)) == 0 {
				return nil, io.EOF
			}
			// Allow a complete JSON frame without a trailing newline at EOF.
			break
		}
		return nil, err
	}
	buf = bytes.TrimSuffix(buf, []byte("\n"))
	buf = bytes.TrimSuffix(buf, []byte("\r"))
	return buf, nil
}

func truncateForError(b []byte) string {
	const max = 80
	if len(b) <= max {
		return string(b)
	}
	return string(b[:max]) + "..."
}

// Encoder serializes JSON-RPC frames as NDJSON under a mutex.
type Encoder struct {
	mu sync.Mutex
	w  io.Writer
}

func NewEncoder(w io.Writer) *Encoder {
	return &Encoder{w: w}
}

func (e *Encoder) WriteMessage(msg *Message) error {
	if msg.JSONRPC == "" {
		msg.JSONRPC = jsonrpcVersion
	}
	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	_, err = e.w.Write(append(b, '\n'))
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
// enqueues notifications to an ordered worker goroutine, and spawns a dedicated
// goroutine per server->client request. Blocking inside a server-request
// handler (such as a parked approval or user-input ask) can never block
// reading responses (e.g. turn/interrupt), serverRequest/resolved, or EOF.
type Client struct {
	dec     *Decoder
	enc     *Encoder
	child   *agentproc.Child
	wCloser io.Closer
	rCloser io.Closer
	ended   func(error) error

	mu      sync.Mutex
	nextID  int64
	pending map[string]chan rpcResp
	err     error
	closed  bool
	done    chan struct{}

	readDone   chan struct{}
	notifyDone chan struct{}

	// Ordered notification queue (never blocks reader goroutine).
	notifyMu     sync.Mutex
	notifyCond   *sync.Cond
	notifyQueue  []*Message
	notifyClosed bool

	handlerMu sync.RWMutex
	onNotify  func(*Message)
	onRequest func(context.Context, *Message)

	reqCtx    context.Context
	reqCancel context.CancelFunc
	reqWG     sync.WaitGroup

	closeOnce sync.Once
	closeDone chan struct{}
	closeErr  error
}

// NewClient constructs a Client over raw io.Reader / io.Writer streams (for
// tests or custom transports) and starts its background loops. Call
// SetNotifyHandler / SetRequestHandler before Start() if you need to observe
// the very first frame; NewClient calls Start() automatically only in Spawn,
// whereas NewInProcess leaves Start() to the caller or starts immediately.
func NewInProcess(in io.Reader, out io.Writer) *Client {
	return newClient(in, out, nil)
}

func newClient(in io.Reader, out io.Writer, child *agentproc.Child) *Client {
	reqCtx, reqCancel := context.WithCancel(context.Background())
	c := &Client{
		dec:        NewDecoder(in),
		enc:        NewEncoder(out),
		child:      child,
		pending:    make(map[string]chan rpcResp),
		done:       make(chan struct{}),
		readDone:   make(chan struct{}),
		notifyDone: make(chan struct{}),
		reqCtx:     reqCtx,
		reqCancel:  reqCancel,
		closeDone:  make(chan struct{}),
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
		Binary:     opts.Binary,
		Candidates: opts.Candidates,
		Args:       opts.Args,
		Dir:        opts.Dir,
		Env:        opts.Env,
		Stderr:     opts.Stderr,
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
		c.failAll(child.EndErr("codexrpc", ErrClosed))
		c.closeWriter()
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
// server->client request. When nil, unknown requests immediately receive
// JSON-RPC -32601 (Method not found).
func (c *Client) SetRequestHandler(fn func(context.Context, *Message)) {
	c.handlerMu.Lock()
	c.onRequest = fn
	c.handlerMu.Unlock()
}

// Start begins reading stdout and dispatching notifications. Spawn calls Start
// automatically; callers of NewInProcess call Start after setting handlers.
func (c *Client) Start() {
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

// Call sends a JSON-RPC request and waits for its response, unmarshaling
// result into out (when non-nil).
func (c *Client) Call(ctx context.Context, method string, params, out any) error {
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
	if err := c.enc.WriteMessage(msg); err != nil {
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
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	var paramRaw json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return err
		}
		paramRaw = b
	}
	return c.enc.WriteMessage(&Message{
		JSONRPC: jsonrpcVersion,
		Method:  method,
		Params:  paramRaw,
	})
}

// Reply sends a JSON-RPC success response preserving the raw server request ID
// verbatim (including integer 0).
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
	return c.enc.WriteMessage(&Message{
		JSONRPC: jsonrpcVersion,
		ID:      &idCopy,
		Result:  raw,
	})
}

// ReplyErr sends a JSON-RPC error response preserving the raw request ID.
func (c *Client) ReplyErr(id json.RawMessage, rpcErr *RPCError) error {
	idCopy := append(json.RawMessage(nil), id...)
	return c.enc.WriteMessage(&Message{
		JSONRPC: jsonrpcVersion,
		ID:      &idCopy,
		Error:   rpcErr,
	})
}

func (c *Client) closeWriter() {
	if c.wCloser != nil {
		_ = c.wCloser.Close()
	}
}

// Close shuts down the child process (if any), closes pipes, cancels in-flight
// server-request handlers, and waits for all background goroutines. Idempotent.
// Returns an error wrapping ErrAgentExited if the child had already exited
// before Close was called.
func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		defer close(c.closeDone)
		exitedFirst := false
		if c.child != nil {
			select {
			case <-c.child.Exited():
				exitedFirst = true
			default:
			}
		}
		c.reqCancel()
		// Close stdin first so codex app-server sees EOF cleanly before SIGTERM.
		c.closeWriter()
		if c.child != nil {
			c.child.Shutdown()
			c.failAll(c.child.EndErr("codexrpc", ErrClosed))
		}
		c.failAll(ErrClosed)
		if c.rCloser != nil {
			_ = c.rCloser.Close()
		}
		<-c.done
		<-c.readDone
		<-c.notifyDone
		c.reqWG.Wait()
		if c.child != nil {
			c.child.Wait()
			if exitedFirst {
				c.closeErr = agentproc.AgentExitedErr(c.child.ExitErr())
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
				err = ErrClosed
			}
			if c.ended != nil {
				err = c.ended(err)
			}
			c.failAll(err)
			return
		}
		switch {
		case msg.IsResponse():
			c.deliverResponse(msg)
		case msg.IsRequest():
			c.dispatchRequest(msg)
		case msg.IsNotification():
			c.enqueueNotify(msg)
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

func (c *Client) enqueueNotify(msg *Message) {
	c.notifyMu.Lock()
	if !c.notifyClosed {
		c.notifyQueue = append(c.notifyQueue, msg)
		c.notifyCond.Signal()
	}
	c.notifyMu.Unlock()
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
		if len(c.notifyQueue) == 0 {
			c.notifyQueue = nil
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

func (c *Client) dispatchRequest(msg *Message) {
	c.handlerMu.RLock()
	fn := c.onRequest
	c.handlerMu.RUnlock()

	c.reqWG.Add(1)
	go func() {
		defer c.reqWG.Done()
		if fn == nil {
			if msg.ID != nil {
				_ = c.ReplyErr(*msg.ID, MethodNotFound(msg.Method))
			}
			return
		}
		fn(c.reqCtx, msg)
	}()
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
