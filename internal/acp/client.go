package acp

import (
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"sync"

	"github.com/charliek/craze/internal/version"
)

type Client struct {
	conn  *Conn
	child *Child

	mu             sync.Mutex
	sessionID      string
	inPrompt       bool
	permHandler    func(PermissionRequest) PermissionDecision
	onUpdate       func(SessionNotification)
	pendingUpdates []SessionNotification

	incomingMu sync.Mutex
	incoming   map[string]*incomingReq
}

type incomingReq struct {
	id      json.RawMessage
	method  string
	req     PermissionRequest
	decide  chan PermissionDecision
	replied bool
}

func newClient(conn *Conn, child *Child) *Client {
	c := &Client{
		conn:     conn,
		child:    child,
		incoming: make(map[string]*incomingReq),
	}
	conn.SetRequestHandler(c.onRequest)
	conn.SetNotifyHandler(c.onNotify)
	return c
}

func Dial(in io.Reader, out io.Writer) *Client {
	conn := NewConn(in, out)
	c := newClient(conn, nil)
	conn.Start()
	return c
}

func (c *Client) Conn() *Conn { return c.conn }

func (c *Client) SessionID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sessionID
}

func (c *Client) PromptInFlight() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.inPrompt
}

func (c *Client) SetPermissionHandler(h func(PermissionRequest) PermissionDecision) {
	c.mu.Lock()
	c.permHandler = h
	c.mu.Unlock()
}

func (c *Client) SetUpdateHandler(h func(SessionNotification)) {
	c.mu.Lock()
	c.onUpdate = h
	c.mu.Unlock()
}

func (c *Client) Initialize(ctx context.Context) (*InitializeResult, error) {
	params := InitializeParams{
		ProtocolVersion: ProtocolVersion,
		ClientInfo: Implementation{
			Name:    "craze",
			Version: version.Version,
		},
		ClientCapabilities: ClientCapabilities{
			Meta:     map[string]any{"parameterizedModelPicker": true},
			FS:       FSCapabilities{ReadTextFile: false, WriteTextFile: false},
			Terminal: false,
		},
	}
	var result InitializeResult
	if err := c.conn.Call(ctx, MethodInitialize, params, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *Client) Authenticate(ctx context.Context) error {
	return c.conn.Call(ctx, MethodAuthenticate, AuthenticateParams{MethodID: AuthCursorLogin}, nil)
}

func (c *Client) NewSession(ctx context.Context, cwd string) (*NewSessionResult, error) {
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return nil, err
	}
	params := NewSessionParams{CWD: abs, MCPServers: []any{}}
	var result NewSessionResult
	if err := c.conn.Call(ctx, MethodSessionNew, params, &result); err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.sessionID = result.SessionID
	pending := c.pendingUpdates
	c.pendingUpdates = nil
	h := c.onUpdate
	c.mu.Unlock()
	flushSessionUpdates(result.SessionID, pending, h)
	return &result, nil
}

func (c *Client) Prompt(ctx context.Context, text string) (*PromptResult, error) {
	c.mu.Lock()
	if c.inPrompt {
		c.mu.Unlock()
		return nil, ErrPromptInFlight
	}
	c.inPrompt = true
	sid := c.sessionID
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.inPrompt = false
		c.mu.Unlock()
	}()

	params := PromptParams{
		SessionID: sid,
		Prompt:    []ContentBlock{{Type: "text", Text: text}},
	}
	var result PromptResult
	if err := c.conn.Call(ctx, MethodSessionPrompt, params, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *Client) SetModel(ctx context.Context, modelID string) error {
	return c.conn.Call(ctx, MethodSessionSetModel, SetModelParams{
		SessionID: c.SessionID(),
		ModelID:   modelID,
	}, nil)
}

func (c *Client) SetMode(ctx context.Context, modeID string) error {
	return c.conn.Call(ctx, MethodSessionSetMode, SetModeParams{
		SessionID: c.SessionID(),
		ModeID:    modeID,
	}, nil)
}

func (c *Client) SetConfig(ctx context.Context, configID, value string) error {
	return c.conn.Call(ctx, MethodSessionSetConfig, SetConfigParams{
		SessionID: c.SessionID(),
		ConfigID:  configID,
		Value:     value,
	}, nil)
}

func (c *Client) Cancel(ctx context.Context) error {
	c.completeIncomingCancelled()
	sid := c.SessionID()
	if sid == "" {
		return nil
	}
	return c.conn.Notify(ctx, MethodSessionCancel, CancelParams{SessionID: sid})
}

func (c *Client) AnswerPermission(id string, dec PermissionDecision) {
	c.incomingMu.Lock()
	in, ok := c.incoming[id]
	c.incomingMu.Unlock()
	if !ok || in == nil {
		return
	}
	select {
	case in.decide <- dec:
	default:
	}
}

func (c *Client) Close() error {
	c.completeIncomingCancelled()
	c.mu.Lock()
	inFlight := c.inPrompt
	sid := c.sessionID
	c.mu.Unlock()
	if inFlight && sid != "" {
		_ = c.conn.Notify(context.Background(), MethodSessionCancel, CancelParams{SessionID: sid})
	}
	if c.child != nil {
		c.child.Shutdown()
	}
	_ = c.conn.Close()
	<-c.conn.Done()
	if c.child != nil {
		c.child.Wait()
	}
	return nil
}

func (c *Client) onRequest(msg *Message) {
	switch msg.Method {
	case MethodRequestPermission:
		go c.handlePermission(msg)
	case MethodCursorAskQuestion:
		go c.handleAskQuestion(msg)
	case MethodCursorCreatePlan:
		go c.handleCreatePlan(msg)
	default:
		_ = c.conn.ReplyErr(msg.ID, MethodNotFound(msg.Method))
	}
}

func (c *Client) onNotify(msg *Message) {
	if msg.Method != MethodSessionUpdate {
		return
	}
	var n SessionNotification
	if err := json.Unmarshal(msg.Params, &n); err != nil {
		return
	}
	c.mu.Lock()
	if c.sessionID == "" {
		c.pendingUpdates = append(c.pendingUpdates, n)
		c.mu.Unlock()
		return
	}
	active := c.sessionID
	h := c.onUpdate
	c.mu.Unlock()
	if n.SessionID != active {
		return
	}
	if h != nil {
		h(n)
	}
}

func flushSessionUpdates(sid string, pending []SessionNotification, h func(SessionNotification)) {
	if h == nil {
		return
	}
	for _, n := range pending {
		if n.SessionID == sid {
			h(n)
		}
	}
}

func (c *Client) handlePermission(msg *Message) {
	var req PermissionRequest
	if err := json.Unmarshal(msg.Params, &req); err != nil {
		_ = c.conn.ReplyErr(msg.ID, &RPCError{Code: codeInvalidParams, Message: "invalid permission request"})
		return
	}
	in := &incomingReq{
		id:     msg.ID,
		method: msg.Method,
		req:    req,
		decide: make(chan PermissionDecision, 1),
	}
	key := idKey(msg.ID)
	c.incomingMu.Lock()
	c.incoming[key] = in
	c.incomingMu.Unlock()
	defer c.dropIncoming(key)

	c.mu.Lock()
	h := c.permHandler
	c.mu.Unlock()
	if h != nil {
		c.finishPermission(in, h(req))
		return
	}
	select {
	case dec := <-in.decide:
		c.finishPermission(in, dec)
	case <-c.conn.Done():
		c.finishPermission(in, PermissionDecision{Cancelled: true})
	}
}

func (c *Client) finishPermission(in *incomingReq, dec PermissionDecision) {
	c.incomingMu.Lock()
	if in.replied {
		c.incomingMu.Unlock()
		return
	}
	in.replied = true
	c.incomingMu.Unlock()

	if dec.Cancelled || !optionIDInRequest(in.req.Options, dec.OptionID) {
		_ = c.conn.Reply(in.id, cancelledOutcome())
		return
	}
	_ = c.conn.Reply(in.id, selectedOutcome(dec.OptionID))
}

func (c *Client) handleAskQuestion(msg *Message) {
	var req AskQuestionRequest
	_ = json.Unmarshal(msg.Params, &req)
	in := &incomingReq{id: msg.ID, method: msg.Method}
	key := idKey(msg.ID)
	c.incomingMu.Lock()
	c.incoming[key] = in
	c.incomingMu.Unlock()
	defer c.dropIncoming(key)

	select {
	case <-c.conn.Done():
		c.replyIncoming(in, cancelledOutcome())
	default:
		c.replyIncoming(in, askAnsweredOutcome(req))
	}
}

func (c *Client) handleCreatePlan(msg *Message) {
	in := &incomingReq{id: msg.ID, method: msg.Method}
	key := idKey(msg.ID)
	c.incomingMu.Lock()
	c.incoming[key] = in
	c.incomingMu.Unlock()
	defer c.dropIncoming(key)

	select {
	case <-c.conn.Done():
		c.replyIncoming(in, cancelledOutcome())
	default:
		c.replyIncoming(in, planAcceptedOutcome())
	}
}

func (c *Client) replyIncoming(in *incomingReq, result any) {
	c.incomingMu.Lock()
	if in.replied {
		c.incomingMu.Unlock()
		return
	}
	in.replied = true
	c.incomingMu.Unlock()
	_ = c.conn.Reply(in.id, result)
}

func (c *Client) dropIncoming(key string) {
	c.incomingMu.Lock()
	delete(c.incoming, key)
	c.incomingMu.Unlock()
}

func (c *Client) completeIncomingCancelled() {
	c.incomingMu.Lock()
	pending := make([]*incomingReq, 0, len(c.incoming))
	for _, in := range c.incoming {
		pending = append(pending, in)
	}
	c.incomingMu.Unlock()
	for _, in := range pending {
		if in.method == MethodRequestPermission && in.decide != nil {
			select {
			case in.decide <- PermissionDecision{Cancelled: true}:
			default:
			}
		}
		c.replyIncoming(in, cancelledOutcome())
	}
}
