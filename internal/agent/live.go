package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/charliek/craze/internal/acp"
)

type session struct {
	opts   Options
	client *acp.Client
	events chan Event
	done   chan struct{}

	closeOnce sync.Once
	closeDone chan struct{}

	mu         sync.Mutex
	started    bool
	closed     bool
	inPrompt   bool
	promptDone chan struct{}
	waiting    map[string]pendingAsk
	seq        map[string]int
	// turn counts prompts; cancelledTurn records the one a cancel was issued
	// for, so a request that arrives while the turn is being cancelled is
	// answered instead of parked. Which turn a *card* belongs to is the client's
	// turn, not this one: only the client knows when a request arrived, and a
	// handler goroutine can start long after that (see park).
	turn          int
	cancelledTurn int
	snap          Snapshot
	tools         map[string]ToolEvent
	toolOrder     []string
	// taskReceipts holds cursor/task receipts whose tool_call has not landed
	// yet; it is bounded and cleared at turn end so an unmatched receipt can
	// never leak or attach itself to a later tool with a reused id.
	taskReceipts     map[string]TaskInfo
	taskReceiptOrder []string
}

// taskReceiptCap bounds the parked receipts of a single turn.
const taskReceiptCap = 32

// pendingAsk is one blocking request the UI still owes an answer to. kind is
// askPermission, askQuestion or askPlan; decide carries that kind's own
// acp decision type. turn is the client turn the request arrived in, which is
// what keeps a card out of a later turn.
type pendingAsk struct {
	kind    string
	turn    int
	options []acp.PermissionOption
	decide  chan any
}

const (
	askPermission = "perm"
	askQuestion   = "ask"
	askPlan       = "plan"
)

func New(opts Options) Session {
	return newSession(opts)
}

func newSession(opts Options) *session {
	s := &session{
		opts:      opts,
		events:    make(chan Event, 256),
		done:      make(chan struct{}),
		closeDone: make(chan struct{}),
		waiting:   make(map[string]pendingAsk),
		seq:       make(map[string]int),
		tools:     make(map[string]ToolEvent),

		taskReceipts: make(map[string]TaskInfo),
	}
	// The provider is decided once, here, so every snapshot — including one
	// taken before Start — names it.
	s.snap.Provider = s.provider().Info()
	return s
}

// provider is the agent behind this session; nil Options.Provider is cursor.
func (s *session) provider() Provider {
	if s.opts.Provider != nil {
		return *s.opts.Provider
	}
	return CursorProvider()
}

// clientRef reads the spawned client under the lock; it is nil before Start
// assigns it and after a concurrent Close.
func (s *session) clientRef() *acp.Client {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.client
}

func (s *session) Events() <-chan Event {
	return s.events
}

func (s *session) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return fmt.Errorf("agent: session already started")
	}
	if s.closed {
		s.mu.Unlock()
		return fmt.Errorf("agent: session closed")
	}
	s.started = true
	s.mu.Unlock()

	args := s.provider().Args(s.opts.ExtraArgs, s.opts.Force)

	cwd := s.opts.Workspace
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			s.unstart()
			return err
		}
	}
	cwd, err := filepath.Abs(cwd)
	if err != nil {
		s.unstart()
		return err
	}

	client, err := acp.Spawn(acp.SpawnOptions{
		Binary:     s.opts.Binary,
		Candidates: s.provider().Bins(),
		Args:       args,
		Dir:        cwd,
		Env:        s.opts.Env,
		Stderr:     s.opts.Stderr,
		Dialect:    s.provider().Dialect(),
	})
	if err != nil {
		s.unstart()
		return err
	}
	// Close may have run while we were spawning; adopt the child only if the
	// session is still open, otherwise reap it here so it cannot be orphaned.
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = client.Close()
		return fmt.Errorf("agent: session closed")
	}
	s.client = client
	s.mu.Unlock()
	client.SetUpdateHandler(s.onUpdate)
	client.SetPermissionHandler(s.onPermission)
	client.SetAskHandler(s.onAskQuestion)
	client.SetPlanHandler(s.onCreatePlan)
	client.SetTodosHandler(s.onUpdateTodos)
	client.SetTaskHandler(s.onTaskReceipt)

	initRes, err := client.Initialize(ctx)
	if err != nil {
		_ = s.Close()
		return err
	}
	if methodID, meta, ok := s.authMethod(initRes); ok {
		if err := client.Authenticate(ctx, methodID, meta); err != nil {
			_ = s.Close()
			return fmt.Errorf("%w (run `%s`)", err, s.provider().LoginHint())
		}
	} else if len(initRes.AuthMethods) > 0 && s.provider().Dialect() == acp.DialectGrok {
		// The daemon offered only methods craze will not start (interactive
		// browser login); say how to fix it instead of hanging later.
		_ = s.Close()
		return fmt.Errorf("agent: no supported auth method (run `%s`)", s.provider().LoginHint())
	}
	sess, err := client.NewSession(ctx, cwd)
	if err != nil {
		_ = s.Close()
		return err
	}
	snap := snapshotFromNew(sess)
	snap.Provider = s.provider().Info()
	if s.opts.Mode != "" {
		modeID, ok := ResolveMode(s.opts.Mode, modeIDs(snap.Modes))
		if !ok {
			_ = s.Close()
			return fmt.Errorf("agent: session did not advertise mode %q", s.opts.Mode)
		}
		if err := client.SetMode(ctx, modeID); err != nil {
			_ = s.Close()
			return err
		}
		snap.CurrentMode = modeID
	}
	if s.opts.Model != "" {
		if err := client.SetModel(ctx, s.opts.Model); err != nil {
			_ = s.Close()
			return err
		}
		snap.CurrentModel = s.opts.Model
	}
	s.mu.Lock()
	commands := s.snap.Commands
	s.snap = snap
	if len(s.snap.Commands) == 0 {
		s.snap.Commands = commands
	}
	s.mu.Unlock()
	return nil
}

// authMethod intersects what initialize advertised with the provider's
// preference order. No advertised methods means no login, for either
// provider. Otherwise cursor logs in only when cursor_login is offered, and
// grok prefers the env key over the daemon's default. Grok methods craze
// will not start (interactive browser login) are never picked.
func (s *session) authMethod(initRes *acp.InitializeResult) (string, map[string]any, bool) {
	p := s.provider()
	if len(initRes.AuthMethods) == 0 {
		return "", nil, false
	}
	if p.Dialect() == acp.DialectGrok {
		if hasAPIKeyEnv() && initRes.OffersAuthMethod(acp.AuthXAIAPIKey) {
			return acp.AuthXAIAPIKey, map[string]any{"headless": true}, true
		}
		if initRes.OffersAuthMethod(acp.AuthCachedToken) {
			return acp.AuthCachedToken, map[string]any{"headless": true}, true
		}
		return "", nil, false
	}
	for _, id := range p.AuthMethodIDs() {
		if initRes.OffersAuthMethod(id) {
			return id, nil, true
		}
	}
	return "", nil, false
}

// hasAPIKeyEnv reports whether a grok API key is set. Grok honours the
// legacy GROK_CODE_XAI_API_KEY alongside XAI_API_KEY.
func hasAPIKeyEnv() bool {
	return os.Getenv("XAI_API_KEY") != "" || os.Getenv("GROK_CODE_XAI_API_KEY") != ""
}

func (s *session) unstart() {
	s.mu.Lock()
	s.started = false
	s.mu.Unlock()
}

func (s *session) Prompt(ctx context.Context, text string) (Result, error) {
	s.mu.Lock()
	client := s.client
	if client == nil {
		s.mu.Unlock()
		return Result{}, fmt.Errorf("agent: session not started")
	}
	if s.inPrompt {
		s.mu.Unlock()
		return Result{}, acp.ErrPromptInFlight
	}
	s.inPrompt = true
	s.turn++
	done := make(chan struct{})
	s.promptDone = done
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.inPrompt = false
		s.clearTaskReceiptsLocked()
		close(done)
		s.mu.Unlock()
	}()

	res, err := client.Prompt(ctx, text)
	if err != nil {
		s.emit(Event{Type: EventError, Err: err})
		return Result{}, err
	}
	s.emit(Event{Type: EventDone, StopReason: res.StopReason})
	return Result{StopReason: res.StopReason}, nil
}

func (s *session) SetModel(ctx context.Context, modelID string) error {
	client := s.clientRef()
	if client == nil {
		return fmt.Errorf("agent: session not started")
	}
	if err := client.SetModel(ctx, modelID); err != nil {
		return err
	}
	s.mu.Lock()
	s.snap.CurrentModel = modelID
	s.mu.Unlock()
	return nil
}

func (s *session) SetMode(ctx context.Context, modeID string) error {
	client := s.clientRef()
	if client == nil {
		return fmt.Errorf("agent: session not started")
	}
	if err := client.SetMode(ctx, modeID); err != nil {
		return err
	}
	s.mu.Lock()
	s.snap.CurrentMode = modeID
	s.mu.Unlock()
	return nil
}

func (s *session) SetConfig(ctx context.Context, id, value string) error {
	client := s.clientRef()
	if client == nil {
		return fmt.Errorf("agent: session not started")
	}
	if err := client.SetConfig(ctx, id, value); err != nil {
		return err
	}
	s.mu.Lock()
	for i := range s.snap.Config {
		if s.snap.Config[i].ID == id {
			s.snap.Config[i].Current = value
			break
		}
	}
	s.mu.Unlock()
	return nil
}

func (s *session) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.snap
	out.Models = append([]ModelInfo(nil), s.snap.Models...)
	out.Modes = append([]ModeInfo(nil), s.snap.Modes...)
	out.Commands = append([]CommandInfo(nil), s.snap.Commands...)
	out.Config = cloneConfig(s.snap.Config)
	out.Todos = append([]Todo(nil), s.snap.Todos...)
	out.Tools = snapshotTools(s.toolOrder, s.tools)
	return out
}

func (s *session) Cancel(ctx context.Context) error {
	client := s.clientRef()
	if client == nil {
		return nil
	}
	s.cancelWaiting()
	if err := client.Cancel(ctx); err != nil {
		return err
	}
	s.mu.Lock()
	done := s.promptDone
	in := s.inPrompt
	s.mu.Unlock()
	if !in || done == nil {
		return nil
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// cancelWaiting answers every blocking request of every kind, exactly once:
// the map is swapped out under the lock so a concurrent Answer* finds nothing.
func (s *session) cancelWaiting() {
	s.mu.Lock()
	s.cancelledTurn = s.turn
	pending := s.waiting
	s.waiting = make(map[string]pendingAsk)
	s.mu.Unlock()
	for _, p := range pending {
		p.answer(cancelledFor(p.kind))
	}
}

func cancelledFor(kind string) any {
	switch kind {
	case askQuestion:
		return acp.AskDecision{Cancelled: true}
	case askPlan:
		return acp.PlanDecision{Cancelled: true}
	default:
		return acp.PermissionDecision{Cancelled: true}
	}
}

// answer never blocks: the channel is buffered and each pendingAsk is taken
// out of the map before it is answered, so at most one value is ever sent.
func (p pendingAsk) answer(v any) {
	select {
	case p.decide <- v:
	default:
	}
}

// take removes a waiting request of the expected kind.
func (s *session) take(id, kind string) (pendingAsk, error) {
	s.mu.Lock()
	p, ok := s.waiting[id]
	if ok && p.kind == kind {
		delete(s.waiting, id)
	}
	s.mu.Unlock()
	if !ok || p.kind != kind {
		return pendingAsk{}, fmt.Errorf("agent: unknown %s request %q", kind, id)
	}
	return p, nil
}

func (s *session) AnswerPermission(id, optionID string) error {
	p, err := s.take(id, askPermission)
	if err != nil {
		return fmt.Errorf("agent: unknown permission request %q", id)
	}
	if optionID == "" {
		p.answer(acp.PermissionDecision{Cancelled: true})
		return nil
	}
	found := false
	for _, o := range p.options {
		if o.OptionID == optionID {
			found = true
			break
		}
	}
	if !found {
		p.answer(acp.PermissionDecision{Cancelled: true})
		return fmt.Errorf("agent: optionId %q is not in the permission request", optionID)
	}
	p.answer(acp.PermissionDecision{OptionID: optionID})
	return nil
}

// AnswerQuestion answers a cursor/ask_question card. Option ids that the
// request did not offer are dropped when the reply is built.
func (s *session) AnswerQuestion(id string, answers map[string][]string, skip bool) error {
	p, err := s.take(id, askQuestion)
	if err != nil {
		return err
	}
	if skip {
		p.answer(acp.AskDecision{Skip: true})
		return nil
	}
	p.answer(acp.AskDecision{Answers: answers})
	return nil
}

// AnswerPlan accepts or rejects a cursor/create_plan card.
func (s *session) AnswerPlan(id string, accept bool) error {
	p, err := s.take(id, askPlan)
	if err != nil {
		return err
	}
	p.answer(acp.PlanDecision{Accept: accept})
	return nil
}

// Close reaps the child exactly once; later callers block until that reap has
// finished rather than returning while the child is still alive.
func (s *session) Close() error {
	s.closeOnce.Do(func() {
		defer close(s.closeDone)
		s.mu.Lock()
		s.closed = true
		close(s.done)
		client := s.client
		s.mu.Unlock()
		if client != nil {
			_ = client.Close()
		}
	})
	<-s.closeDone
	return nil
}

func (s *session) onPermission(turn int, req acp.PermissionRequest) acp.PermissionDecision {
	if s.opts.Force {
		id, ok := acp.PickYoloAllow(req.Options)
		if !ok {
			return acp.PermissionDecision{Cancelled: true}
		}
		return acp.PermissionDecision{OptionID: id}
	}
	id, ch, ok := s.park(askPermission, turn, pendingAsk{options: req.Options})
	if !ok {
		return acp.PermissionDecision{Cancelled: true}
	}

	opts := make([]PermissionOption, 0, len(req.Options))
	for _, o := range req.Options {
		opts = append(opts, PermissionOption{
			OptionID: o.OptionID,
			Name:     sanitizeText(o.Name),
			Kind:     o.Kind,
		})
	}
	if !s.emitParked(id, Event{
		Type: EventPermission,
		Permission: &PermissionEvent{
			ID:      id,
			Tool:    sanitizeText(req.ToolCall.Title),
			Options: opts,
		},
	}) {
		return acp.PermissionDecision{Cancelled: true}
	}
	return awaitDecision(s, ch, acp.PermissionDecision{Cancelled: true})
}

// onAskQuestion blocks on the UI when Interactive, and otherwise answers with
// each question's first option and reports what it sent.
func (s *session) onAskQuestion(turn int, req acp.AskQuestionRequest) acp.AskDecision {
	ev := &QuestionEvent{
		Title:     sanitizeText(req.Title),
		Questions: questionsFromRequest(req),
	}
	if !s.opts.Interactive {
		ev.ID = s.nextID(askQuestion)
		ev.Auto = true
		ev.Answers = acp.AskAutoAnswers(req)
		s.emit(Event{Type: EventQuestion, Question: ev})
		return acp.AskDecision{Answers: ev.Answers}
	}
	id, ch, ok := s.park(askQuestion, turn, pendingAsk{})
	if !ok {
		return acp.AskDecision{Cancelled: true}
	}
	ev.ID = id
	if !s.emitParked(id, Event{Type: EventQuestion, Question: ev}) {
		return acp.AskDecision{Cancelled: true}
	}
	return awaitDecision(s, ch, acp.AskDecision{Cancelled: true})
}

func questionsFromRequest(req acp.AskQuestionRequest) []Question {
	out := make([]Question, 0, len(req.Questions))
	for _, q := range req.Questions {
		opts := make([]Option, 0, len(q.Options))
		for _, o := range q.Options {
			opts = append(opts, Option{ID: o.ID, Label: sanitizeText(o.Label)})
		}
		if len(opts) == 0 {
			// A question with no options still needs something to press; the
			// empty id is dropped when the reply is built, so craze never
			// invents an option id.
			opts = append(opts, Option{Label: "OK"})
		}
		out = append(out, Question{
			ID:            q.ID,
			Prompt:        sanitizeText(q.Prompt),
			Options:       opts,
			AllowMultiple: q.AllowMultiple,
		})
	}
	return out
}

// onCreatePlan mirrors onAskQuestion: block when Interactive, accept and
// report otherwise.
func (s *session) onCreatePlan(turn int, req acp.CreatePlanRequest) acp.PlanDecision {
	ev := &PlanEvent{
		Name:     sanitizeText(req.DisplayName()),
		Overview: sanitizeText(req.Overview),
		Plan:     sanitizeText(req.PlanText()),
		Todos:    todosFromWire(req.Todos),
	}
	if !s.opts.Interactive {
		ev.ID = s.nextID(askPlan)
		ev.Auto = true
		ev.Accepted = true
		s.emit(Event{Type: EventPlan, Plan: ev})
		return acp.PlanDecision{Accept: true}
	}
	id, ch, ok := s.park(askPlan, turn, pendingAsk{})
	if !ok {
		return acp.PlanDecision{Cancelled: true}
	}
	ev.ID = id
	if !s.emitParked(id, Event{Type: EventPlan, Plan: ev}) {
		return acp.PlanDecision{Cancelled: true}
	}
	return awaitDecision(s, ch, acp.PlanDecision{Cancelled: true})
}

// park registers a blocking request and returns its craze-local id (perm-N,
// ask-N, plan-N — never a JSON-RPC id or a toolCallId) and decision channel.
// turn is the turn the request arrived in, handed to the handler by the client.
//
// It refuses once the request's own turn is over, the current turn has been
// cancelled, or the session closed. The turn check is what a handler goroutine
// the runtime delayed past the end of its turn runs into: cancelWaiting only
// drains what is already in the map, so without it a late request would park
// into the turn now running and raise a card for a plan that turn never made —
// and without the cancelled-turn check a request arriving while a cancel is in
// flight would park forever and leave the agent waiting on a reply that never
// comes.
func (s *session) park(kind string, turn int, p pendingAsk) (string, chan any, bool) {
	ch := make(chan any, 1)
	p.kind = kind
	p.turn = turn
	p.decide = ch
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || !s.turnLiveLocked(turn) || (s.turn > 0 && s.cancelledTurn == s.turn) {
		return "", nil, false
	}
	s.seq[kind]++
	id := fmt.Sprintf("%s-%d", kind, s.seq[kind])
	s.waiting[id] = p
	return id, ch, true
}

// emitParked publishes a card event only while its request is still parked and
// still belongs to the turn it arrived in. A cancel that landed between the
// park and the emit has already answered the request and taken it out of
// waiting, so emitting anyway would raise a card for a request nobody can
// answer any more; a turn that ended in that same window would put the card in
// front of the next turn, which is not the turn that asked for it. The waiting
// check takes the same lock cancelWaiting does; it cannot be held across the
// emit, because emit blocks on the event channel and the reader of that channel
// is the goroutine that answers cards.
func (s *session) emitParked(id string, ev Event) bool {
	s.mu.Lock()
	p, live := s.waiting[id]
	if live && !s.turnLiveLocked(p.turn) {
		// Nobody will ever answer it now, so it does not stay in the map: the
		// handler replies cancelled on its way out instead.
		delete(s.waiting, id)
		live = false
	}
	s.mu.Unlock()
	if !live {
		return false
	}
	s.emit(ev)
	return true
}

// turnLiveLocked reports whether the turn a request arrived in is still the one
// the client is running; callers hold s.mu. With no client — before Start, and
// in the unit tests — there is one turn and it is turn 0. The lock order is
// session then client: the client answers this without taking any lock the
// session holds, and it never calls back into the session under its own.
func (s *session) turnLiveLocked(turn int) bool {
	if s.client == nil {
		return turn == 0
	}
	return s.client.TurnLive(turn)
}

func (s *session) nextID(kind string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq[kind]++
	return fmt.Sprintf("%s-%d", kind, s.seq[kind])
}

// awaitDecision blocks for the UI's answer, falling back to onClose once the
// session is closing or if another kind's decision arrived.
func awaitDecision[T any](s *session, ch chan any, onClose T) T {
	select {
	case v := <-ch:
		if d, ok := v.(T); ok {
			return d
		}
		return onClose
	case <-s.done:
		return onClose
	}
}

// onUpdateTodos merges the request into Snapshot.Todos and returns the merged
// list, which the client echoes back to cursor.
func (s *session) onUpdateTodos(req acp.UpdateTodosRequest) []acp.TodoItem {
	s.mu.Lock()
	s.snap.Todos = mergeTodos(s.snap.Todos, todosFromWire(req.Todos), req.Merge)
	s.snap.TodosUpdatedAt = time.Now()
	todos := append([]Todo(nil), s.snap.Todos...)
	s.mu.Unlock()
	s.emit(Event{Type: EventTodos, Todos: todos})
	return todosToWire(todos)
}

// onTaskReceipt joins the sub-agent receipt onto its tool call, in either
// arrival order: the receipt is parked when the tool_call has not landed yet.
func (s *session) onTaskReceipt(req acp.TaskRequest) {
	if req.ToolCallID == "" {
		return
	}
	info := TaskInfo{
		Description:  sanitizeText(req.Description),
		Prompt:       truncateUTF8(sanitizeText(req.Prompt), taskPromptCap),
		Model:        sanitizeText(req.Model),
		AgentID:      sanitizeText(req.AgentID),
		SubagentType: subagentTypeName(req.SubagentType),
		DurationMs:   req.DurationMs,
		Receipt:      true,
	}
	s.mu.Lock()
	tool, ok := s.tools[req.ToolCallID]
	if !ok {
		s.parkTaskReceiptLocked(req.ToolCallID, info)
		s.mu.Unlock()
		return
	}
	tool.Task = mergeTaskInfo(tool.Task, info)
	s.tools[req.ToolCallID] = tool
	out := cloneTool(tool)
	s.mu.Unlock()
	s.emit(Event{Type: EventTool, Tool: &out})
}

type sessionUpdateWire struct {
	SessionUpdate     string                 `json:"sessionUpdate"`
	Content           json.RawMessage        `json:"content,omitempty"`
	ToolCallID        string                 `json:"toolCallId,omitempty"`
	Title             *string                `json:"title,omitempty"`
	Kind              *string                `json:"kind,omitempty"`
	Status            *string                `json:"status,omitempty"`
	RawInput          json.RawMessage        `json:"rawInput,omitempty"`
	RawOutput         json.RawMessage        `json:"rawOutput,omitempty"`
	Locations         json.RawMessage        `json:"locations,omitempty"`
	AvailableCommands []acp.AvailableCommand `json:"availableCommands,omitempty"`
	CurrentModeID     string                 `json:"currentModeId,omitempty"`
	ConfigOptions     json.RawMessage        `json:"configOptions,omitempty"`
}

func (s *session) onUpdate(n acp.SessionNotification) {
	var u sessionUpdateWire
	if err := json.Unmarshal(n.Update, &u); err != nil {
		return
	}
	switch u.SessionUpdate {
	case acp.UpdateAgentMessage:
		s.emit(Event{Type: EventText, Text: messageText(u.Content)})
	case acp.UpdateAgentThought:
		s.emit(Event{Type: EventThought, Text: messageText(u.Content)})
	case acp.UpdateToolCall, acp.UpdateToolCallUpd:
		delta, ok := toolDeltaFromWire(u)
		if !ok {
			return
		}
		tool, emit := s.mergeTool(delta)
		if emit {
			s.emit(Event{Type: EventTool, Tool: &tool})
		}
	case acp.UpdateAvailableCommands:
		s.mu.Lock()
		s.snap.Commands = commandsFromUpdate(u.AvailableCommands)
		s.mu.Unlock()
		s.emit(Event{Type: EventMeta})
	case acp.UpdateCurrentMode:
		if u.CurrentModeID != "" {
			mode := sanitizeText(u.CurrentModeID)
			s.mu.Lock()
			s.snap.CurrentMode = mode
			s.mu.Unlock()
			// The mode rides on the event: a mode that changed and changed
			// back is invisible in the snapshot, and the UI has to see it.
			s.emit(Event{Type: EventMeta, Mode: mode})
		}
	case acp.UpdateSessionInfo:
		// session_info_update reuses the tool title field on the wire.
		title := ""
		if u.Title != nil {
			title = sanitizeText(*u.Title)
		}
		if title == "" {
			return
		}
		s.mu.Lock()
		s.snap.Title = title
		s.mu.Unlock()
		// EventMeta carries the new title so `prompt --json` can emit a
		// title line; the TUI only re-reads the snapshot.
		s.emit(Event{Type: EventMeta, Text: title})
	case acp.UpdateConfigOption:
		cfg := parseConfigOptions(u.ConfigOptions)
		s.mu.Lock()
		s.snap.Config = cfg
		s.mu.Unlock()
		s.emit(Event{Type: EventMeta})
	}
}

func toolDeltaFromWire(u sessionUpdateWire) (toolDelta, bool) {
	if u.ToolCallID == "" {
		return toolDelta{}, false
	}
	d := toolDelta{
		id:     u.ToolCallID,
		title:  u.Title,
		kind:   u.Kind,
		status: u.Status,
	}
	if raw, ok := presentJSON(u.RawInput); ok {
		d.rawInput = raw
		d.hasRawInput = true
	}
	if raw, ok := presentJSON(u.RawOutput); ok {
		d.rawOutput = raw
		d.hasRawOutput = true
	}
	if raw, ok := presentJSON(u.Content); ok && raw[0] == '[' {
		d.content = raw
		d.hasContent = true
	}
	if raw, ok := presentJSON(u.Locations); ok {
		d.locations = raw
		d.hasLocations = true
	}
	return d, true
}

func messageText(raw json.RawMessage) string {
	raw, ok := presentJSON(raw)
	if !ok || raw[0] != '{' {
		return ""
	}
	var b acp.ContentBlock
	if err := json.Unmarshal(raw, &b); err != nil {
		return ""
	}
	return sanitizeText(b.Text)
}

func (s *session) emit(ev Event) {
	if ev.At.IsZero() {
		ev.At = time.Now()
	}
	select {
	case <-s.done:
		return
	default:
	}
	select {
	case s.events <- ev:
	case <-s.done:
	}
}

func (s *session) promptInFlight() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inPrompt
}

// mergeTodos applies one cursor/update_todos request. merge=false replaces the
// list in request order; merge=true upserts by id, keeping the existing order
// and appending ids it has not seen. Duplicate ids inside one request: last
// wins, at the position of the first occurrence.
func mergeTodos(prev, in []Todo, merge bool) []Todo {
	if !merge {
		return dedupeTodos(in)
	}
	out := append([]Todo(nil), prev...)
	index := make(map[string]int, len(out))
	for i, t := range out {
		index[t.ID] = i
	}
	for _, t := range in {
		if i, ok := index[t.ID]; ok {
			out[i] = t
			continue
		}
		index[t.ID] = len(out)
		out = append(out, t)
	}
	return out
}

func dedupeTodos(in []Todo) []Todo {
	out := make([]Todo, 0, len(in))
	index := make(map[string]int, len(in))
	for _, t := range in {
		if i, ok := index[t.ID]; ok {
			out[i] = t
			continue
		}
		index[t.ID] = len(out)
		out = append(out, t)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func todosFromWire(in []acp.TodoItem) []Todo {
	if len(in) == 0 {
		return nil
	}
	out := make([]Todo, 0, len(in))
	for _, t := range in {
		out = append(out, Todo{
			ID:      sanitizeText(t.ID),
			Content: sanitizeText(t.Content),
			Status:  normalizeTodoStatus(t.Status),
		})
	}
	return out
}

func todosToWire(in []Todo) []acp.TodoItem {
	out := make([]acp.TodoItem, 0, len(in))
	for _, t := range in {
		out = append(out, acp.TodoItem{ID: t.ID, Content: t.Content, Status: t.Status})
	}
	return out
}

// normalizeTodoStatus accepts the documented lowercase values, cursor's
// TODO_STATUS_* enum names and the camelCase inProgress spelling.
func normalizeTodoStatus(s string) string {
	v := strings.ToLower(strings.TrimSpace(sanitizeText(s)))
	v = strings.TrimPrefix(v, "todo_status_")
	v = strings.ReplaceAll(v, "-", "_")
	switch v {
	case "in_progress", "inprogress":
		return "in_progress"
	case "completed", "complete", "done":
		return "completed"
	case "cancelled", "canceled":
		return "cancelled"
	default:
		return "pending"
	}
}

// parkTaskReceiptLocked stores a receipt whose tool_call has not arrived,
// dropping the oldest once the turn's cap is reached.
func (s *session) parkTaskReceiptLocked(id string, info TaskInfo) {
	if _, dup := s.taskReceipts[id]; !dup {
		s.taskReceiptOrder = append(s.taskReceiptOrder, id)
	}
	s.taskReceipts[id] = info
	for len(s.taskReceiptOrder) > taskReceiptCap {
		oldest := s.taskReceiptOrder[0]
		s.taskReceiptOrder = s.taskReceiptOrder[1:]
		delete(s.taskReceipts, oldest)
	}
}

func (s *session) dropTaskReceiptLocked(id string) {
	delete(s.taskReceipts, id)
	for i, x := range s.taskReceiptOrder {
		if x == id {
			s.taskReceiptOrder = append(s.taskReceiptOrder[:i], s.taskReceiptOrder[i+1:]...)
			return
		}
	}
}

func (s *session) clearTaskReceiptsLocked() {
	if len(s.taskReceiptOrder) == 0 {
		return
	}
	s.taskReceipts = make(map[string]TaskInfo)
	s.taskReceiptOrder = nil
}
