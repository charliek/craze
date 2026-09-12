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

	mu        sync.Mutex
	sessionID string
	inPrompt  bool
	// turn counts prompts. It is the identity of a turn: a blocking request
	// records the turn it arrived in, so a handler that starts late can tell
	// that the turn it belongs to is over.
	turn           int
	permHandler    func(turn int, req PermissionRequest) PermissionDecision
	askHandler     func(turn int, req AskQuestionRequest) AskDecision
	planHandler    func(turn int, req CreatePlanRequest) PlanDecision
	todosHandler   func(UpdateTodosRequest) []TodoItem
	taskHandler    func(TaskRequest)
	onUpdate       func(SessionNotification)
	pendingUpdates []SessionNotification

	incomingMu sync.Mutex
	incoming   map[string]*pendingReq
}

// pendingReq is one blocking agent→client request. decide carries the kind's
// own decision type (PermissionDecision, AskDecision, PlanDecision); replied
// makes sure cancel, close and the handler between them answer exactly once.
// turn is the turn the read loop registered it in.
type pendingReq struct {
	id      json.RawMessage
	method  string
	turn    int
	decide  chan any
	replied bool
}

func newClient(conn *Conn, child *Child) *Client {
	c := &Client{
		conn:     conn,
		child:    child,
		incoming: make(map[string]*pendingReq),
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

// SetPermissionHandler installs a session/request_permission handler. Every
// blocking handler is handed the turn its request arrived in, so anything it
// puts on screen can be dropped once that turn is over (see TurnLive).
func (c *Client) SetPermissionHandler(h func(turn int, req PermissionRequest) PermissionDecision) {
	c.mu.Lock()
	c.permHandler = h
	c.mu.Unlock()
}

// SetAskHandler installs a cursor/ask_question handler. Without one the client
// auto-answers with each question's first option.
func (c *Client) SetAskHandler(h func(turn int, req AskQuestionRequest) AskDecision) {
	c.mu.Lock()
	c.askHandler = h
	c.mu.Unlock()
}

// SetPlanHandler installs a cursor/create_plan handler. Without one the client
// auto-accepts.
func (c *Client) SetPlanHandler(h func(turn int, req CreatePlanRequest) PlanDecision) {
	c.mu.Lock()
	c.planHandler = h
	c.mu.Unlock()
}

// SetTodosHandler installs a cursor/update_todos handler; it returns the
// merged list craze echoes back. Without one the request's own list is echoed.
func (c *Client) SetTodosHandler(h func(UpdateTodosRequest) []TodoItem) {
	c.mu.Lock()
	c.todosHandler = h
	c.mu.Unlock()
}

// SetTaskHandler installs a cursor/task receipt handler.
func (c *Client) SetTaskHandler(h func(TaskRequest)) {
	c.mu.Lock()
	c.taskHandler = h
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

func (c *Client) SetUpdateHandler(h func(SessionNotification)) {
	c.mu.Lock()
	c.onUpdate = h
	c.mu.Unlock()
}

func (c *Client) Prompt(ctx context.Context, text string) (*PromptResult, error) {
	c.mu.Lock()
	if c.inPrompt {
		c.mu.Unlock()
		return nil, ErrPromptInFlight
	}
	c.inPrompt = true
	// A new turn: every blocking request registered from here on belongs to it,
	// and every request of the turn before it is now stale.
	c.turn++
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

// onRequest and onNotify route the cursor extension methods identically; the
// only difference is that a request gets a reply and a notification does not.
//
// Params are validated and the blocking kinds are registered here, on the read
// loop, before their handler goroutine starts: a cancel landing in between
// must still find the request and answer it exactly once.
func (c *Client) onRequest(msg *Message) {
	switch msg.Method {
	case MethodRequestPermission:
		var req PermissionRequest
		if err := decodeObject(msg.Params, &req); err != nil {
			c.replyInvalidParams(msg.ID, "invalid permission request")
			return
		}
		c.dispatch(msg, func(in *pendingReq) { c.handlePermission(in, req) })
	case MethodCursorAskQuestion:
		var req AskQuestionRequest
		if err := decodeObject(msg.Params, &req); err != nil {
			c.replyInvalidParams(msg.ID, "invalid cursor/ask_question params")
			return
		}
		c.dispatch(msg, func(in *pendingReq) { c.handleAskQuestion(in, req) })
	case MethodCursorCreatePlan:
		var req CreatePlanRequest
		if err := decodeObject(msg.Params, &req); err != nil {
			c.replyInvalidParams(msg.ID, "invalid cursor/create_plan params")
			return
		}
		c.dispatch(msg, func(in *pendingReq) { c.handleCreatePlan(in, req) })
	case MethodCursorUpdateTodos:
		c.handleUpdateTodos(msg.ID, msg.Params)
	case MethodCursorTask:
		c.handleTask(msg.ID, msg.Params)
	default:
		_ = c.conn.ReplyErr(msg.ID, MethodNotFound(msg.Method))
	}
}

// dispatch registers a blocking request, then runs its handler off the read
// loop so the connection keeps draining while the user decides.
func (c *Client) dispatch(msg *Message, run func(*pendingReq)) {
	in := c.register(msg)
	go c.runIncoming(in, run)
}

// register records a blocking request on the read loop, stamped with the turn
// it arrived in.
func (c *Client) register(msg *Message) *pendingReq {
	c.mu.Lock()
	turn := c.turn
	c.mu.Unlock()
	in := &pendingReq{id: msg.ID, method: msg.Method, turn: turn, decide: make(chan any, 1)}
	key := idKey(msg.ID)
	c.incomingMu.Lock()
	c.incoming[key] = in
	c.incomingMu.Unlock()
	return in
}

// runIncoming is the handler goroutine's body. A goroutine the runtime did not
// schedule for a while can wake up long after its request stopped mattering:
// a cancel may have answered it already, or its whole turn may have ended and
// another one begun. Either way the handler must not run, because a handler is
// what raises a card, and a card from a turn that is over would be answered
// into the turn now running. The request is answered cancelled instead — for an
// already-answered one that is a no-op, and for the rest it is the reply the
// agent is still waiting for.
func (c *Client) runIncoming(in *pendingReq, run func(*pendingReq)) {
	defer c.dropIncoming(idKey(in.id))
	if !c.liveIncoming(in) {
		c.replyIncoming(in, cancelledOutcome())
		return
	}
	run(in)
}

// liveIncoming reports whether a registered request still deserves its handler:
// nothing has answered it yet and the turn it arrived in is still the one
// running.
func (c *Client) liveIncoming(in *pendingReq) bool {
	c.mu.Lock()
	turn := c.turn
	c.mu.Unlock()
	c.incomingMu.Lock()
	defer c.incomingMu.Unlock()
	return !in.replied && in.turn == turn
}

// TurnLive reports whether turn is still the one the client is running. A
// blocking request is handed the turn it arrived in, so its handler can ask
// this before it publishes anything for a turn that has since ended.
func (c *Client) TurnLive(turn int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return turn == c.turn
}

func (c *Client) onNotify(msg *Message) {
	switch msg.Method {
	case MethodSessionUpdate:
		c.handleSessionUpdate(msg)
	case MethodCursorUpdateTodos:
		c.handleUpdateTodos(nil, msg.Params)
	case MethodCursorTask:
		c.handleTask(nil, msg.Params)
	}
}

func (c *Client) handleSessionUpdate(msg *Message) {
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

// handleUpdateTodos is shared by both envelopes: id is nil for a notification.
// Malformed params leave the todo list untouched, and only a request gets the
// InvalidParams reply.
func (c *Client) handleUpdateTodos(id json.RawMessage, params json.RawMessage) {
	req, err := parseUpdateTodos(params)
	if err != nil {
		c.replyInvalidParams(id, "invalid cursor/update_todos params")
		return
	}
	c.mu.Lock()
	h := c.todosHandler
	c.mu.Unlock()
	merged := req.Todos
	if h != nil {
		merged = h(req)
	}
	if len(id) > 0 {
		_ = c.conn.Reply(id, todosAcceptedOutcome(merged))
	}
}

func (c *Client) handleTask(id json.RawMessage, params json.RawMessage) {
	req, err := parseTaskRequest(params)
	if err != nil {
		c.replyInvalidParams(id, "invalid cursor/task params")
		return
	}
	c.mu.Lock()
	h := c.taskHandler
	c.mu.Unlock()
	if h != nil {
		h(req)
	}
	if len(id) > 0 {
		_ = c.conn.Reply(id, taskCompletedOutcome(req.AgentID, req.DurationMs))
	}
}

func (c *Client) replyInvalidParams(id json.RawMessage, msg string) {
	if len(id) == 0 {
		return
	}
	_ = c.conn.ReplyErr(id, &RPCError{Code: CodeInvalidParams, Message: msg})
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

func (c *Client) handlePermission(in *pendingReq, req PermissionRequest) {
	c.mu.Lock()
	h := c.permHandler
	c.mu.Unlock()
	if h != nil {
		c.finishPermission(in, req, h(in.turn, req))
		return
	}
	select {
	case dec := <-in.decide:
		p, _ := dec.(PermissionDecision)
		c.finishPermission(in, req, p)
	case <-c.conn.Done():
		c.finishPermission(in, req, PermissionDecision{Cancelled: true})
	}
}

func (c *Client) finishPermission(in *pendingReq, req PermissionRequest, dec PermissionDecision) {
	if dec.Cancelled || !optionIDInRequest(req.Options, dec.OptionID) {
		c.replyIncoming(in, cancelledOutcome())
		return
	}
	c.replyIncoming(in, selectedOutcome(dec.OptionID))
}

func (c *Client) handleAskQuestion(in *pendingReq, req AskQuestionRequest) {
	c.mu.Lock()
	h := c.askHandler
	c.mu.Unlock()
	if h != nil {
		c.replyIncoming(in, askOutcome(req, h(in.turn, req)))
		return
	}
	select {
	case <-c.conn.Done():
		c.replyIncoming(in, cancelledOutcome())
	default:
		c.replyIncoming(in, askOutcome(req, AskDecision{Answers: AskAutoAnswers(req)}))
	}
}

func (c *Client) handleCreatePlan(in *pendingReq, req CreatePlanRequest) {
	c.mu.Lock()
	h := c.planHandler
	c.mu.Unlock()
	if h != nil {
		c.replyIncoming(in, planOutcome(h(in.turn, req)))
		return
	}
	select {
	case <-c.conn.Done():
		c.replyIncoming(in, cancelledOutcome())
	default:
		c.replyIncoming(in, planOutcome(PlanDecision{Accept: true}))
	}
}

func (c *Client) replyIncoming(in *pendingReq, result any) {
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

// completeIncomingCancelled answers every blocking request exactly once, with
// the cancelled decision its own kind understands.
func (c *Client) completeIncomingCancelled() {
	c.incomingMu.Lock()
	pending := make([]*pendingReq, 0, len(c.incoming))
	for _, in := range c.incoming {
		pending = append(pending, in)
	}
	c.incomingMu.Unlock()
	for _, in := range pending {
		if in.decide != nil {
			select {
			case in.decide <- cancelDecision(in.method):
			default:
			}
		}
		c.replyIncoming(in, cancelledOutcome())
	}
}

func cancelDecision(method string) any {
	switch method {
	case MethodCursorAskQuestion:
		return AskDecision{Cancelled: true}
	case MethodCursorCreatePlan:
		return PlanDecision{Cancelled: true}
	default:
		return PermissionDecision{Cancelled: true}
	}
}
