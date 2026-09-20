package acp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"sync"
	"time"

	"github.com/charliek/craze/internal/version"
)

type Client struct {
	conn  *Conn
	child *Child

	closeOnce sync.Once
	closeDone chan struct{}
	closeErr  error

	mu        sync.Mutex
	sessionID string
	inPrompt  bool
	dialect   DialectID
	// promptWait is the grok prompt_complete racer. First of the RPC reply
	// or a matching notify wins; the other is abandoned.
	promptWait chan promptResult
	// donePromptID is the grok promptId of the last turn the RPC reply
	// ended. A prompt_complete carrying it is that turn's late twin and
	// must not end the turn now running.
	donePromptID string
	// promptID is the grok promptId of the prompt in flight, learned from
	// the queue/changed broadcast that names it (§3.2). promptText is what
	// was sent, which is how the broadcast is recognised. Both are reset by
	// every Prompt.
	promptID   string
	promptText string
	// foreignSeen records that some other running promptId was broadcast
	// since the prompt was sent. Until promptID is known it is the only
	// reason to distrust an unmatched prompt_complete.
	foreignSeen bool
	// foreignID is the turn the agent is running without a craze prompt —
	// grok's interject fallback is the only known producer. While it is set,
	// Prompt is refused.
	foreignID   string
	foreignText string

	interjectHandler func(InterjectionNotification)
	foreignHandler   func(ForeignTurn)
	// interjectSeen dedups the interjection broadcast by id, bounded in
	// arrival order. A broadcast with no id is never deduped.
	interjectSeen  map[string]struct{}
	interjectOrder []string
	// turn counts prompts. It is the identity of a turn: a blocking request
	// records the turn it arrived in, so a handler that starts late can tell
	// that the turn it belongs to is over.
	turn            int
	permHandler     func(turn int, req PermissionRequest) PermissionDecision
	askHandler      func(turn int, req AskQuestionRequest) AskDecision
	planHandler     func(turn int, req CreatePlanRequest) PlanDecision
	todosHandler    func(UpdateTodosRequest) []TodoItem
	taskHandler     func(TaskRequest)
	earlyHandler    func(EarlyAnswer)
	onUpdate        func(SessionNotification)
	subagentHandler func(SubagentNotification)
	pendingUpdates  []SessionNotification
	// children is the routed-child allowlist, in registration order. A
	// subagent_spawned registers its child_session_id; subagent_finished
	// deregisters after the handler ran. The 65th concurrent registration
	// is refused, never evicting a registered child.
	children   map[string]struct{}
	childOrder []string
	dropped    int64

	incomingMu sync.Mutex
	incoming   map[string]*pendingReq
}

// pendingReq is one blocking agent→client request. decide carries the kind's
// own decision type (PermissionDecision, AskDecision, PlanDecision); replied
// makes sure cancel, close and the handler between them answer exactly once.
// turn is the turn the read loop registered it in, and params is the request as
// it was decoded, kept for the path that answers one without ever running its
// handler (EarlyAnswer). Both are written before the request is published to
// c.incoming and never afterwards.
type pendingReq struct {
	id      json.RawMessage
	method  string
	turn    int
	params  RequestParams
	decide  chan any
	replied bool
	// repliedBy is who wrote the reply, under incomingMu with replied itself:
	// one of the repliedBy constants. It is what tells a cancel's answer from a
	// close's, for the early-answer reason and for a lost reply's Lost alike.
	repliedBy string
}

// Who answered a blocking request, on pendingReq.repliedBy. The first three
// spell the ReplyLost / EarlyAnswerReason value they become when another
// reply loses to them.
const (
	repliedByCancel  = ReplyLostCancelled
	repliedByClose   = ReplyLostClosed
	repliedByStale   = string(EarlyStaleTurn)
	repliedByHandler = "handler"
)

type promptResult struct {
	res PromptResult
	err error
}

func newClient(conn *Conn, child *Child) *Client {
	c := &Client{
		conn:      conn,
		child:     child,
		dialect:   DialectCursor,
		incoming:  make(map[string]*pendingReq),
		closeDone: make(chan struct{}),
	}
	conn.SetRequestHandler(c.onRequest)
	conn.SetNotifyHandler(c.onNotify)
	return c
}

// Dialect is the provider dialect the client speaks.
func (c *Client) Dialect() DialectID {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dialect
}

func Dial(in io.Reader, out io.Writer) *Client {
	return DialWithDialect(in, out, DialectCursor)
}

// DialWithDialect is Dial for a provider dialect. Tests default to cursor.
func DialWithDialect(in io.Reader, out io.Writer, dialect DialectID) *Client {
	conn := NewConn(in, out)
	c := newClient(conn, nil)
	c.dialect = dialect
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

// SetEarlyAnswerHandler installs the handler for a blocking request the client
// answered before any handler of its own ran — a cancel or a close that got
// there first, or a turn that was over by the time the handler goroutine was
// scheduled. Without one nothing changes: those requests are answered exactly
// as they always were and nobody is told.
//
// It runs on the request's own handler goroutine, never the read loop, after
// the client's reply has been written or skipped and with no client lock held,
// exactly once per such request and never for a request whose handler ran.
func (c *Client) SetEarlyAnswerHandler(h func(EarlyAnswer)) {
	c.mu.Lock()
	c.earlyHandler = h
	c.mu.Unlock()
}

// SetTodosHandler installs a cursor/update_todos handler; it returns the
// merged list craze echoes back. Without one the request's own list is echoed.
func (c *Client) SetTodosHandler(h func(UpdateTodosRequest) []TodoItem) {
	c.mu.Lock()
	c.todosHandler = h
	c.mu.Unlock()
}

// childRouteCap bounds concurrently routed children. A 65th registration is
// refused (dropped and counted); no registered child is ever evicted.
const childRouteCap = 64

// SetSubagentHandler installs the subagent lifecycle handler. It runs on the
// read goroutine in wire order; it must not go async.
func (c *Client) SetSubagentHandler(h func(SubagentNotification)) {
	c.mu.Lock()
	c.subagentHandler = h
	c.mu.Unlock()
}

// DroppedUpdates counts updates dropped by the child router: unknown or
// unregistered session ids, refused registrations, and malformed lifecycle
// notifications.
func (c *Client) DroppedUpdates() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dropped
}

// registerChild records a routed child. Re-registering a known id keeps its
// original order slot. A new id past the cap is refused.
func (c *Client) registerChild(id string) bool {
	if _, ok := c.children[id]; ok {
		return true
	}
	if len(c.childOrder) >= childRouteCap {
		c.dropped++
		return false
	}
	if c.children == nil {
		c.children = make(map[string]struct{})
	}
	c.children[id] = struct{}{}
	c.childOrder = append(c.childOrder, id)
	return true
}

func (c *Client) deregisterChild(id string) {
	if _, ok := c.children[id]; !ok {
		return
	}
	delete(c.children, id)
	for i, v := range c.childOrder {
		if v == id {
			c.childOrder = append(c.childOrder[:i], c.childOrder[i+1:]...)
			break
		}
	}
}

func (c *Client) isChildLocked(id string) bool {
	_, ok := c.children[id]
	return ok
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

func (c *Client) Authenticate(ctx context.Context, methodID string, meta map[string]any) error {
	return c.conn.Call(ctx, MethodAuthenticate, AuthenticateParams{MethodID: methodID, Meta: meta}, nil)
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
	// A new session does not inherit the previous session's child
	// allowlist: those ids belong to a session that is gone. Neither the
	// foreign turn nor the interjection ids survive it.
	c.children = nil
	c.childOrder = nil
	c.foreignID = ""
	c.foreignText = ""
	c.foreignSeen = false
	c.interjectSeen = nil
	c.interjectOrder = nil
	h := c.onUpdate
	c.mu.Unlock()
	if dropped := flushSessionUpdates(result.SessionID, pending, h); dropped > 0 {
		c.mu.Lock()
		c.dropped += dropped
		c.mu.Unlock()
	}
	return &result, nil
}

// LoadSession reloads an existing agent session by id. The agent answers by
// replaying the whole transcript as session/update notifications and only then
// returning the result, so replay end *is* the RPC result.
//
// The session id is installed *before* the call, unlike NewSession, which
// learns it from the reply. Replay notifications therefore route through
// handleSessionUpdate's live path instead of the pre-session pendingUpdates
// buffer, which is unbounded and whose flush would otherwise hand a whole
// session to the update handler in one burst from this goroutine. Any
// pendingUpdates already held are discarded: nothing before a load can belong
// to the session being loaded. The rest of the reset is NewSession's, for the
// same reason — the child allowlist, the foreign turn and the interjection
// ids all belong to a session that is gone.
//
// INVARIANT this depends on: Conn.readLoop dispatches each notification
// synchronously through onNotify *before* it delivers an RPC result (conn.go,
// readLoop). So every replay notification has reached the session's update
// handler by the time LoadSession returns, with no idle-gap race to guess at
// and nothing left in flight to bracket. Cursor's synthetic replay tool ids
// (replay-N-M) are ordinary tool ids on this path and flow through the tool
// merge unchanged.
//
// On an RPC error or a timeout the id is cleared back to "", so anything the
// agent is still streaming lands in pendingUpdates again. That is only safe
// because Start closes the client immediately on this error — the buffer, and
// any child a replayed subagent_spawned registered, go with it.
func (c *Client) LoadSession(ctx context.Context, id, cwd string) (*NewSessionResult, error) {
	if id == "" {
		return nil, errors.New("acp: load session has no id")
	}
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.sessionID = id
	c.pendingUpdates = nil
	c.children = nil
	c.childOrder = nil
	c.foreignID = ""
	c.foreignText = ""
	c.foreignSeen = false
	c.interjectSeen = nil
	c.interjectOrder = nil
	c.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, loadSessionDeadline())
	defer cancel()
	params := LoadSessionParams{SessionID: id, CWD: abs, MCPServers: []any{}}
	var result NewSessionResult
	if err := c.conn.Call(ctx, MethodSessionLoad, params, &result); err != nil {
		c.mu.Lock()
		c.sessionID = ""
		c.mu.Unlock()
		return nil, err
	}
	// The wire does not echo the id (grok returns models alone); the caller
	// asked for this one and got it.
	result.SessionID = id
	return &result, nil
}

func (c *Client) SetUpdateHandler(h func(SessionNotification)) {
	c.mu.Lock()
	c.onUpdate = h
	c.mu.Unlock()
}

// Prompt sends one text block: the draft, exactly as the user typed it.
func (c *Client) Prompt(ctx context.Context, text string) (*PromptResult, error) {
	return c.PromptBlocks(ctx, []ContentBlock{{Type: "text", Text: text}}, nil, nil)
}

// PromptBlocks sends one session/prompt carrying every block, in order, and
// blocks on the RPC for the whole turn while agent updates stream on the
// reader goroutine. Block 1 is the draft; the blocks after it are craze's own
// plugin expansions, which cursor reads in order like any other block.
//
// accepted runs once the prompt is craze's to send — after the in-flight and
// foreign-turn checks have passed and the turn has been opened — and before
// the request is written, so whatever it emits precedes the turn's first agent
// update. It never runs on a refusal: nothing reached the wire, so nothing
// happened.
//
// sent runs after the session/prompt bytes were written to the agent's stdin
// and before the call's reply wait begins, so a session/cancel written after it
// can only land behind the prompt. It never runs on a refusal, and never when
// the write failed or the connection was already closed. It does no I/O and
// must not block: it only publishes. On cursor it runs on the caller's own
// goroutine, inside this call. On grok the writer is the goroutine below, which
// this call abandons once prompt_complete has settled the turn, so sent may run
// after PromptBlocks has returned; a caller must tolerate a late call.
func (c *Client) PromptBlocks(ctx context.Context, blocks []ContentBlock, accepted, sent func()) (*PromptResult, error) {
	if len(blocks) == 0 {
		return nil, errors.New("acp: prompt has no content")
	}
	// The caller's slice is its own from here: accepted hands control back to
	// it for a moment, and a prompt whose blocks changed between the text
	// correlation below and the marshal would send one thing and remember
	// another.
	blocks = append([]ContentBlock(nil), blocks...)
	c.mu.Lock()
	if c.inPrompt {
		c.mu.Unlock()
		return nil, ErrPromptInFlight
	}
	if c.foreignID != "" {
		// The agent is running a turn of its own; a prompt now would be
		// queued server-side and its completion would be told apart from
		// that turn's only by ids craze has not learned yet.
		c.mu.Unlock()
		return nil, ErrForeignTurn
	}
	c.inPrompt = true
	// A new turn: every blocking request registered from here on belongs to it,
	// and every request of the turn before it is now stale.
	c.turn++
	// The new turn's identity is not known yet: queue/changed teaches it, and
	// until then nothing the last turn learned may speak for this one.
	c.promptID = ""
	// Block 1 and nothing else: grok's queue correlation compares this against
	// the text the user typed, which is all block 1 ever holds.
	c.promptText = firstBlockText(blocks)
	c.foreignSeen = false
	sid := c.sessionID
	dialect := c.dialect
	wait := make(chan promptResult, 1)
	c.promptWait = wait
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.inPrompt = false
		c.promptWait = nil
		c.promptID = ""
		c.promptText = ""
		c.mu.Unlock()
	}()

	// The turn is open and nothing can refuse it any more: the hook runs here,
	// one step before the bytes go out.
	if accepted != nil {
		accepted()
	}

	params := PromptParams{
		SessionID: sid,
		Prompt:    blocks,
	}
	if dialect != DialectGrok {
		var result PromptResult
		if err := c.conn.callSent(ctx, MethodSessionPrompt, params, &result, sent); err != nil {
			return nil, err
		}
		return &result, nil
	}

	rpcCtx, rpcCancel := context.WithCancel(ctx)
	defer rpcCancel()
	rpcCh := make(chan promptResult, 1)
	go func() {
		var result PromptResult
		err := c.conn.callSent(rpcCtx, MethodSessionPrompt, params, &result, sent)
		rpcCh <- promptResult{res: result, err: err}
	}()
	select {
	case w := <-wait:
		rpcCancel()
		go func() { <-rpcCh }()
		return promptResultOrErr(w)
	case w := <-rpcCh:
		// Both channels can be ready at once; Go's select would pick
		// either. If the notify already landed it was first, so it wins.
		select {
		case n := <-wait:
			return promptResultOrErr(n)
		default:
			if w.err == nil {
				c.notePromptDone(w.res.PromptID())
			}
			return promptResultOrErr(w)
		}
	case <-ctx.Done():
		rpcCancel()
		go func() { <-rpcCh }()
		return nil, ctx.Err()
	}
}

// firstBlockText is the draft out of a prompt's blocks: block 1 when it is
// text, and "" for the empty prompt nobody sends.
func firstBlockText(blocks []ContentBlock) string {
	if len(blocks) == 0 || blocks[0].Type != "text" {
		return ""
	}
	return blocks[0].Text
}

func promptResultOrErr(w promptResult) (*PromptResult, error) {
	if w.err != nil {
		return nil, w.err
	}
	return &w.res, nil
}

func (c *Client) failPromptWaiters(res PromptResult, err error) {
	c.mu.Lock()
	ch := c.promptWait
	c.mu.Unlock()
	if ch == nil {
		return
	}
	select {
	case ch <- promptResult{res: res, err: err}:
	default:
	}
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

// Cancel answers every blocking request cancelled and tells the agent to stop
// the turn. It does not end the prompt itself: the turn is over when the
// agent says so (the RPC reply, or grok's prompt_complete), the same as
// cursor. Ending it here would let a follow-up prompt start while the agent
// is still winding the old turn down, and grok's late prompt_complete for
// that old turn would then end the new one. Close is the only path that
// fails a waiter outright.
func (c *Client) Cancel(ctx context.Context) error {
	return c.CancelHeld(ctx, c.Held())
}

// HeldRequests is the set of requests a client was holding at one moment: what
// a cancel made at that moment is a cancel of. It is opaque, and good for one
// CancelHeld.
type HeldRequests struct{ reqs []*pendingReq }

// Held snapshots the requests the client holds now. It takes incomingMu briefly
// and waits on nothing else, so a caller may take it on a goroutine that must
// not block.
//
// It exists for a caller that makes its cancel on another goroutine so as to
// bound it (the session's Cancel: a write into an agent that has stopped
// reading blocks for as long as the pipe does). Such a caller can give the
// cancel up and carry on, and a prompt can then be admitted while the abandoned
// cancel has yet to run; a cancel that gathered the held requests only then
// would answer the *next* turn's permission request cancelled, for a turn
// nobody cancelled. Taking the snapshot on the caller's goroutine, before it
// hands the rest over, fixes what the cancel is a cancel of at the moment it
// was made.
func (c *Client) Held() HeldRequests {
	c.incomingMu.Lock()
	defer c.incomingMu.Unlock()
	reqs := make([]*pendingReq, 0, len(c.incoming))
	for _, in := range c.incoming {
		reqs = append(reqs, in)
	}
	return HeldRequests{reqs: reqs}
}

// CancelHeld is Cancel for a snapshot already taken: it answers exactly those
// requests cancelled — one that has been answered meanwhile is left alone, as
// ever — and tells the agent to stop the turn. Both steps write, and so both can
// block.
func (c *Client) CancelHeld(ctx context.Context, held HeldRequests) error {
	c.completeCancelled(held.reqs, repliedByCancel)
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

// closeCancelWait bounds how long Close waits to write what it still owes the
// agent — cancelled answers and its own session/cancel — before it shuts the
// agent down regardless.
const closeCancelWait = 500 * time.Millisecond

// Close shuts the agent down and reports whether it had already exited on
// its own: a non-blocking probe of c.child.waitCh, at the very top, before
// anything else — including the pre-close session/cancel below — so an agent
// that exits *because of* that cancel is never misread as having exited
// first. If the channel is already closed the agent's exit had been reaped
// before this probe sampled it, and Close returns an error wrapping
// ErrAgentExited; a craze-initiated shutdown returns nil, as before. The rest
// of Close is unchanged and still runs either way — only the return value
// depends on the probe.
//
// Close is idempotent with a stored result: the first call is authoritative
// and every later call blocks on closeDone and returns the same value,
// exactly the shape session.Close already has. Without this, a repeated
// Close — spawnScript's own t.Cleanup, or requestQuit followed by
// finishRun — would find waitCh already closed by the first call's own
// Shutdown and misreport a craze-initiated close as a self-exit.
func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		defer close(c.closeDone)
		var exited error
		if c.child != nil {
			select {
			case <-c.child.waitCh:
				exited = agentExitedErr(c.child.waitErr)
			default:
			}
		}
		c.failPromptWaiters(PromptResult{StopReason: StopCancelled}, nil)
		c.mu.Lock()
		inFlight := c.inPrompt
		sid := c.sessionID
		c.mu.Unlock()
		// What Close still owes the agent — the cancelled answer to every
		// request it is blocked on, then a cancel for a prompt in flight — is
		// written best effort, and is never a reason to stay: an agent that
		// has stopped reading its stdin would hold these writes forever, and
		// the shutdown below is the only thing that ends such an agent. The
		// wait is bounded; Conn.Close then closes the writer, which fails any
		// write still blocked, so the goroutine does not outlive the close.
		owed := make(chan struct{})
		go func() {
			defer close(owed)
			c.completeIncomingCancelled()
			if inFlight && sid != "" {
				_ = c.conn.Notify(context.Background(), MethodSessionCancel, CancelParams{SessionID: sid})
			}
		}()
		select {
		case <-owed:
		case <-time.After(closeCancelWait):
		}
		if c.child != nil {
			c.child.Shutdown()
		}
		_ = c.conn.Close()
		<-c.conn.Done()
		if c.child != nil {
			c.child.Wait()
		}
		c.closeErr = exited
	})
	<-c.closeDone
	return c.closeErr
}

// PID is the agent child's process id, or 0 for an in-process test client
// with no child. It exists for anything that needs to reach the process
// directly rather than through Close.
func (c *Client) PID() int {
	return c.child.PID()
}

// onRequest and onNotify route the cursor extension methods identically; the
// only difference is that a request gets a reply and a notification does not.
//
// Params are validated and the blocking kinds are registered here, on the read
// loop, before their handler goroutine starts: a cancel landing in between
// must still find the request and answer it exactly once.
func (c *Client) onRequest(msg *Message) {
	d := c.Dialect()
	switch msg.Method {
	case MethodRequestPermission:
		var req PermissionRequest
		if err := decodeObject(msg.Params, &req); err != nil {
			c.replyInvalidParams(msg.ID, "invalid permission request")
			return
		}
		c.dispatch(msg, RequestParams{Permission: &req}, func(in *pendingReq) { c.handlePermission(in, req) })
	case MethodCursorAskQuestion:
		if d == DialectGrok {
			_ = c.conn.ReplyErr(msg.ID, MethodNotFound(msg.Method))
			return
		}
		var req AskQuestionRequest
		if err := decodeObject(msg.Params, &req); err != nil {
			c.replyInvalidParams(msg.ID, "invalid cursor/ask_question params")
			return
		}
		c.dispatch(msg, RequestParams{Ask: &req}, func(in *pendingReq) { c.handleAskQuestion(in, req) })
	case MethodCursorCreatePlan:
		if d == DialectGrok {
			_ = c.conn.ReplyErr(msg.ID, MethodNotFound(msg.Method))
			return
		}
		var req CreatePlanRequest
		if err := decodeObject(msg.Params, &req); err != nil {
			c.replyInvalidParams(msg.ID, "invalid cursor/create_plan params")
			return
		}
		c.dispatch(msg, RequestParams{Plan: &req}, func(in *pendingReq) { c.handleCreatePlan(in, req) })
	case MethodGrokAskUserQuestion, MethodGrokAskUserQuestionWrapped:
		if d != DialectGrok {
			_ = c.conn.ReplyErr(msg.ID, MethodNotFound(msg.Method))
			return
		}
		req, err := parseGrokAsk(msg.Params)
		if err != nil {
			c.replyInvalidParams(msg.ID, "invalid x.ai/ask_user_question params")
			return
		}
		c.dispatch(msg, RequestParams{Ask: &req}, func(in *pendingReq) { c.handleAskQuestion(in, req) })
	case MethodGrokExitPlanMode, MethodGrokExitPlanModeWrapped:
		if d != DialectGrok {
			_ = c.conn.ReplyErr(msg.ID, MethodNotFound(msg.Method))
			return
		}
		req, err := parseGrokPlan(msg.Params)
		if err != nil {
			c.replyInvalidParams(msg.ID, "invalid x.ai/exit_plan_mode params")
			return
		}
		c.dispatch(msg, RequestParams{Plan: &req}, func(in *pendingReq) { c.handleCreatePlan(in, req) })
	case MethodCursorUpdateTodos:
		if d == DialectGrok {
			_ = c.conn.ReplyErr(msg.ID, MethodNotFound(msg.Method))
			return
		}
		c.handleUpdateTodos(msg.ID, msg.Params)
	case MethodCursorTask:
		if d == DialectGrok {
			_ = c.conn.ReplyErr(msg.ID, MethodNotFound(msg.Method))
			return
		}
		c.handleTask(msg.ID, msg.Params)
	default:
		_ = c.conn.ReplyErr(msg.ID, MethodNotFound(msg.Method))
	}
}

// dispatch registers a blocking request with the parameters just decoded from
// it, then runs its handler off the read loop so the connection keeps draining
// while the user decides. The typed request is registered rather than left to
// the run closure alone, because the early-answer path never calls that closure
// and still has to say what was asked.
func (c *Client) dispatch(msg *Message, params RequestParams, run func(*pendingReq)) {
	in := c.register(msg, params)
	go c.runIncoming(in, run)
}

// register records a blocking request on the read loop, stamped with the turn
// it arrived in and carrying its decoded parameters.
func (c *Client) register(msg *Message, params RequestParams) *pendingReq {
	c.mu.Lock()
	turn := c.turn
	c.mu.Unlock()
	in := &pendingReq{id: msg.ID, method: msg.Method, turn: turn, params: params, decide: make(chan any, 1)}
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
//
// Nothing above the client would otherwise hear that such a request existed, so
// this is also where the early-answer handler is told about it.
func (c *Client) runIncoming(in *pendingReq, run func(*pendingReq)) {
	defer c.dropIncoming(idKey(in.id))
	if !c.liveIncoming(in) {
		disp := c.replyIncomingBy(in, cancelledResult(c.Dialect(), in.method), repliedByStale)
		c.reportEarlyAnswer(in, disp)
		return
	}
	run(in)
}

// reportEarlyAnswer hands the early-answer handler one request answered before
// any handler of the client's ran. The reason is read off the reply that was
// actually written: a cancel's or a close's answer that got there first says so
// through the disposition, and anything else — this path's own cancelled reply,
// or a write that failed — means nothing had answered it and the stale turn is
// why its handler never ran. The handler is read under c.mu and called without
// it, on this request's own handler goroutine.
func (c *Client) reportEarlyAnswer(in *pendingReq, disp ReplyDisposition) {
	c.mu.Lock()
	h := c.earlyHandler
	c.mu.Unlock()
	if h == nil {
		return
	}
	reason := EarlyStaleTurn
	switch disp.Lost {
	case ReplyLostCancelled:
		reason = EarlyCancelled
	case ReplyLostClosed:
		reason = EarlyClosed
	}
	h(EarlyAnswer{Reason: reason, Turn: in.turn, Params: in.params})
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
		if c.Dialect() != DialectGrok {
			c.handleUpdateTodos(nil, msg.Params)
		}
	case MethodCursorTask:
		if c.Dialect() != DialectGrok {
			c.handleTask(nil, msg.Params)
		}
	case MethodGrokPromptComplete, MethodGrokPromptCompleteWrapped:
		c.handlePromptComplete(msg)
	case MethodGrokSessionNotification, MethodGrokSessionNotificationWrapped:
		if c.Dialect() != DialectGrok {
			return
		}
		// turn_completed rides the same notification as the sub-agent
		// lifecycle and is not one: it ends the foreign turn it names and is
		// not a lifecycle event, so it never reaches the subagent router.
		if n, ok := parseGrokTurnCompleted(msg.Params); ok {
			c.handleTurnCompleted(n)
			return
		}
		c.handleSubagentNotification(msg)
	case MethodGrokInterjection, MethodGrokInterjectionWrapped:
		if c.Dialect() == DialectGrok {
			c.handleInterjection(msg)
		}
	case MethodGrokQueueChanged, MethodGrokQueueChangedWrapped:
		if c.Dialect() == DialectGrok {
			c.handleQueueChanged(msg)
		}
	}
}

// notePromptDone records the promptId of a turn the RPC reply ended, so
// the notify for that same turn, arriving late, is recognised as stale.
func (c *Client) notePromptDone(promptID string) {
	if promptID == "" {
		return
	}
	c.mu.Lock()
	c.donePromptID = promptID
	c.mu.Unlock()
}

func (c *Client) handlePromptComplete(msg *Message) {
	n, ok := parseGrokPromptComplete(msg.Params)
	if !ok {
		return
	}
	stop := n.StopReason
	if stop == "" {
		stop = StopEndTurn
	}
	c.mu.Lock()
	if c.dialect != DialectGrok || n.SessionID != c.sessionID {
		c.mu.Unlock()
		return
	}
	// A completion for a turn craze did not start ends that turn, so the
	// drain waiting on it can go ahead. The fallback is not observed to send
	// one — turn_completed is its only ending — but a future grok might.
	end := c.endForeignLocked(n.PromptID)
	h := c.foreignHandler
	ch := c.promptWait
	in := c.inPrompt
	stale := n.PromptID != "" && n.PromptID == c.donePromptID
	settles := c.settlesLocked(n.PromptID)
	if in && !stale && !settles {
		// A completion that could have ended this turn and does not belong
		// to it is the one worth counting.
		c.dropped++
	}
	c.mu.Unlock()
	fireForeign(h, nil, end)
	if !in || ch == nil || stale || !settles {
		return
	}
	select {
	case ch <- promptResult{res: PromptResult{StopReason: stop}}:
	default:
	}
}

// settlesLocked decides whether a prompt_complete may end the turn in flight.
// Once the turn's own promptId is known only that id settles it. Until then a
// completion settles the turn unless it names an interject fallback or some
// other running turn has been broadcast since the prompt went out — the two
// ways a completion craze did not ask for can reach it.
func (c *Client) settlesLocked(promptID string) bool {
	if c.promptID != "" {
		return promptID == c.promptID
	}
	if IsInterjectFallback(promptID) {
		return false
	}
	return !c.foreignSeen
}

func (c *Client) handleSessionUpdate(msg *Message) {
	var n SessionNotification
	if err := json.Unmarshal(msg.Params, &n); err != nil {
		return
	}
	// dialect is written once, before the read loop starts, so the dialect
	// check and grokToolName's own json.Unmarshal stay off c.mu.
	if c.dialect == DialectGrok {
		n.ToolName = grokToolName(n.Update)
	}
	c.mu.Lock()
	if c.sessionID == "" {
		// Pre-session/new: keep today's active-session filter at flush time.
		c.pendingUpdates = append(c.pendingUpdates, n)
		c.mu.Unlock()
		return
	}
	active := c.sessionID
	routed := n.SessionID == active || c.isChildLocked(n.SessionID)
	h := c.onUpdate
	c.mu.Unlock()
	if !routed {
		c.mu.Lock()
		c.dropped++
		c.mu.Unlock()
		return
	}
	if n.SessionID != active {
		n.Child = n.SessionID
	}
	if h != nil {
		h(n)
	}
}

// handleSubagentNotification routes one x.ai/session_notification. Spawned
// registers the child when the outer id is the active session or a
// registered child; finished deregisters after the handler ran. The handler
// always runs for a well-formed lifecycle event, even when registration is
// refused, so a row can still show.
func (c *Client) handleSubagentNotification(msg *Message) {
	n, ok, drop := parseSubagentNotification(msg.Params)
	if !ok {
		if drop {
			c.mu.Lock()
			c.dropped++
			c.mu.Unlock()
		}
		return
	}
	c.mu.Lock()
	active := c.sessionID
	// Only the active session or a registered child may introduce a new
	// child, so a spawn is gated on the outer id alone. progress and
	// finished also route when the child they are about is registered: a
	// grandchild outlives its own parent child, whose finished already
	// deregistered the outer id this notification arrives on.
	outerRouted := active != "" && (n.SessionID == active || c.isChildLocked(n.SessionID))
	childRouted := active != "" && c.isChildLocked(n.ChildSessionID)
	parentRouted := outerRouted || childRouted
	var h func(SubagentNotification)
	switch n.Kind {
	case SubagentSpawned:
		if !outerRouted {
			c.dropped++
			c.mu.Unlock()
			return
		}
		// Cap-refuse still delivers the notification so a row can show;
		// registerChild counts the refusal. The child's stream is not routed.
		c.registerChild(n.ChildSessionID)
		h = c.subagentHandler
	case SubagentFinished:
		if !parentRouted {
			c.dropped++
			c.mu.Unlock()
			return
		}
		h = c.subagentHandler
		c.mu.Unlock()
		if h != nil {
			h(n)
		}
		c.mu.Lock()
		c.deregisterChild(n.ChildSessionID)
		c.mu.Unlock()
		return
	default:
		if !parentRouted {
			c.dropped++
			c.mu.Unlock()
			return
		}
		h = c.subagentHandler
	}
	c.mu.Unlock()
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

// flushSessionUpdates forwards the updates buffered before session/new that
// belong to the session that came back, and returns how many the
// active-session filter discarded so the caller can count them as dropped.
func flushSessionUpdates(sid string, pending []SessionNotification, h func(SessionNotification)) int64 {
	var dropped int64
	for _, n := range pending {
		// Today's active-session filter: only the active session flushes.
		if n.SessionID != sid {
			dropped++
			continue
		}
		if h == nil {
			continue
		}
		n.Child = ""
		h(n)
	}
	return dropped
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
	disp := c.replyPermission(in, req, dec)
	if dec.Replied != nil {
		dec.Replied(disp)
	}
}

// replyPermission writes the one reply a permission decision earns and reports
// what became of the decision, which is not always what became of the reply: a
// deliberate cancel is delivered as the cancelled outcome, and that is delivery
// of that decision, while an option id the request never offered is replaced by
// the same outcome, and that is the decision being lost. A reply that never
// reached the agent at all says so instead — it is the more immediate truth,
// and the option id stopped mattering the moment nobody heard it.
func (c *Client) replyPermission(in *pendingReq, req PermissionRequest, dec PermissionDecision) ReplyDisposition {
	if dec.Cancelled {
		return c.replyIncoming(in, cancelledOutcome())
	}
	if !optionIDInRequest(req.Options, dec.OptionID) {
		disp := c.replyIncoming(in, cancelledOutcome())
		if disp.Delivered {
			return ReplyDisposition{Lost: ReplyLostInvalidOption}
		}
		return disp
	}
	return c.replyIncoming(in, selectedOutcome(dec.OptionID))
}

func (c *Client) handleAskQuestion(in *pendingReq, req AskQuestionRequest) {
	c.mu.Lock()
	h := c.askHandler
	d := c.dialect
	c.mu.Unlock()
	if h != nil {
		dec := h(in.turn, req)
		disp := c.replyIncoming(in, askOutcome(d, req, dec))
		if dec.Replied != nil {
			dec.Replied(disp)
		}
		return
	}
	select {
	case <-c.conn.Done():
		c.replyIncoming(in, cancelledResult(d, in.method))
	default:
		c.replyIncoming(in, askOutcome(d, req, AskDecision{Answers: AskAutoAnswers(req)}))
	}
}

func (c *Client) handleCreatePlan(in *pendingReq, req CreatePlanRequest) {
	c.mu.Lock()
	h := c.planHandler
	d := c.dialect
	c.mu.Unlock()
	if h != nil {
		dec := h(in.turn, req)
		disp := c.replyIncoming(in, planOutcome(d, dec))
		if dec.Replied != nil {
			dec.Replied(disp)
		}
		return
	}
	select {
	case <-c.conn.Done():
		c.replyIncoming(in, cancelledResult(d, in.method))
	default:
		c.replyIncoming(in, planOutcome(d, PlanDecision{Accept: true}))
	}
}

// replyIncoming answers a blocking request on behalf of its handler, and says
// what became of that reply.
func (c *Client) replyIncoming(in *pendingReq, result any) ReplyDisposition {
	return c.replyIncomingBy(in, result, repliedByHandler)
}

// replyIncomingBy is replyIncoming naming who is answering, which is recorded
// with the answer itself so that whatever loses this race can be told why.
//
// The write error is still dropped, as it always was — there is nobody to hand
// it to and nothing to do about it — but it is no longer silent: a caller that
// wants to know learns that its decision never left.
func (c *Client) replyIncomingBy(in *pendingReq, result any, by string) ReplyDisposition {
	c.incomingMu.Lock()
	if in.replied {
		lost := in.repliedBy
		c.incomingMu.Unlock()
		return ReplyDisposition{Lost: lost}
	}
	in.replied = true
	in.repliedBy = by
	c.incomingMu.Unlock()
	if err := c.conn.Reply(in.id, result); err != nil {
		return ReplyDisposition{Lost: ReplyLostWriteFailed}
	}
	return ReplyDisposition{Delivered: true}
}

func (c *Client) dropIncoming(key string) {
	c.incomingMu.Lock()
	delete(c.incoming, key)
	c.incomingMu.Unlock()
}

// completeIncomingCancelled answers every blocking request exactly once, with
// the cancelled decision its own kind understands. It is Close's, and answers
// as the close it is.
func (c *Client) completeIncomingCancelled() {
	c.completeCancelled(c.Held().reqs, repliedByClose)
}

// completeCancelled answers pending cancelled, recording by as who answered so
// that a handler whose own reply arrives too late can be told which of the two
// it lost to. A request already answered is left alone: replyIncomingBy answers
// each one once.
func (c *Client) completeCancelled(pending []*pendingReq, by string) {
	d := c.Dialect()
	for _, in := range pending {
		if in.decide != nil {
			select {
			case in.decide <- cancelDecision(in.method):
			default:
			}
		}
		c.replyIncomingBy(in, cancelledResult(d, in.method), by)
	}
}

func cancelDecision(method string) any {
	switch method {
	case MethodCursorAskQuestion, MethodGrokAskUserQuestion, MethodGrokAskUserQuestionWrapped:
		return AskDecision{Cancelled: true}
	case MethodCursorCreatePlan, MethodGrokExitPlanMode, MethodGrokExitPlanModeWrapped:
		return PlanDecision{Cancelled: true}
	default:
		return PermissionDecision{Cancelled: true}
	}
}
