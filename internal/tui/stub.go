package tui

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/charliek/craze/internal/agent"
)

// Stub is an in-process Session used by TUI chrome tests. It echoes each
// prompt as assistant text and does not spawn cursor-agent.
type Stub struct {
	mu     sync.Mutex
	events chan agent.Event
	closed chan struct{}
	cancel chan struct{}
	hang   bool
	// park is the live session's catalog wait (§3.3): a prompt held back
	// before any of the turn's bookkeeping, which a Cancel ends by handing it
	// agent.ErrPromptCancelled.
	park       bool
	startDelay time.Duration
	n          int
	failMode   bool
	failModel  bool
	// failConfigAt is the SetConfig call that fails, counted from the next
	// one, or -1 for none. The dialog's apply chain sends more than one, so a
	// test has to be able to fail the second and not the first.
	failConfigAt int
	configCalls  int
	snap         agent.Snapshot
	// open are the blocking requests the stub has announced and is still
	// waiting on, in arrival order, and calls is every answer it received.
	// Together they are how a test holds §3.11's "every card answers exactly
	// once": an id can only be answered while it is open.
	open  []stubOpen
	calls []stubCall
	// Clock stamps Event.At; tests inject one to drive lingers and elapsed
	// times without sleeping.
	Clock func() time.Time

	// queue is craze's own message queue — the real one, so the chrome tests
	// run against the same transactions a live session does. queueOp orders
	// whole transactions as the live session's does; the lock order is
	// queueOp → mu → the queue's own lock.
	queueOp sync.Mutex
	queue   agent.PromptQueue
	// inPrompt, doneEmitted and cancelling mirror the live session's turn
	// state, which is what the queue guards and Interject are decided from.
	inPrompt     bool
	doneEmitted  bool
	cancelling   bool
	foreign      bool
	interjectErr error
}

type stubOpen struct{ id, method string }

// stubCall is one answer the UI sent, or the cancelled outcome Cancel/Close
// produced for a request nobody answered.
type stubCall struct {
	Method    string // permission | question | plan
	ID        string
	Option    string
	Answers   map[string][]string
	Skip      bool
	Accept    bool
	Cancelled bool
}

func NewStub() *Stub {
	return &Stub{
		failConfigAt: -1,
		events:       make(chan agent.Event, 256),
		closed:       make(chan struct{}),
		cancel:       make(chan struct{}, 1),
		snap: agent.Snapshot{
			CurrentModel: "grok",
			CurrentMode:  "agent",
			Models: []agent.ModelInfo{
				{ID: "grok", Name: "Grok"},
				{ID: "fast", Name: "Fast"},
			},
			// The descriptions mirror what cursor advertises, so the note a
			// mode change writes is exercised the way a live session writes it.
			Modes: []agent.ModeInfo{
				{ID: "agent", Name: "Agent", Description: "Full agent capabilities with tool access"},
				{ID: "plan", Name: "Plan", Description: "Read-only mode for planning and designing before implementation"},
				{ID: "ask", Name: "Ask", Description: "Q&A mode - no edits or command execution"},
			},
			Commands: []agent.CommandInfo{
				{Name: "research", Description: "Agent-advertised command"},
			},
			Config: []agent.ConfigOption{
				{
					ID:       "effort",
					Name:     "Effort",
					Category: "thought_level",
					Type:     "select",
					Current:  "medium",
					SelectValues: []agent.SelectValue{
						{Value: "low", Name: "Low"},
						{Value: "medium", Name: "Medium"},
						{Value: "high", Name: "High"},
					},
				},
				{
					// Cursor's fast toggle as the live captures advertise it:
					// string values, off by default, so the status row says
					// nothing about it until it is switched on.
					ID:       "fast",
					Name:     "Fast",
					Category: "model_config",
					Type:     "select",
					Current:  "false",
					SelectValues: []agent.SelectValue{
						{Value: "false", Name: "Off"},
						{Value: "true", Name: "Fast"},
					},
				},
			},
		},
	}
}

func (s *Stub) HangNext() {
	s.mu.Lock()
	s.hang = true
	s.mu.Unlock()
}

// ParkNext makes the next prompt behave as one the live session holds back for
// the agent's first command catalog: it opens no turn, emits no event of any
// kind, and a Cancel while it is parked hands it agent.ErrPromptCancelled.
// HangNext cannot stand in for it — a hung prompt is a turn on the wire, and
// what a cancelled wait leaves the UI is an error and nothing else.
func (s *Stub) ParkNext() {
	s.mu.Lock()
	s.park = true
	s.mu.Unlock()
}

// SetTools replaces Snapshot.Tools (copy-on-write). Tests send EventTool
// afterwards so the TUI refreshSnap() picks the in-flight set up.
func (s *Stub) SetTools(tools []agent.ToolEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snap.Tools = cloneStubTools(tools)
}

// SetSubagents replaces Snapshot.Subagents (copy-on-write).
func (s *Stub) SetSubagents(subs []agent.SubagentInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snap.Subagents = cloneStubSubagents(subs)
}

// SetTodos replaces Snapshot.Todos. Tests send EventTodos afterwards.
func (s *Stub) SetTodos(todos []agent.Todo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snap.Todos = append([]agent.Todo(nil), todos...)
	s.snap.TodosUpdatedAt = s.now()
}

// SetCommands replaces Snapshot.Commands, as an available_commands_update
// would. Tests poke the model afterwards so refreshSnap picks the set up.
func (s *Stub) SetCommands(cmds []agent.CommandInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snap.Commands = append([]agent.CommandInfo(nil), cmds...)
}

// SetPlugins replaces Snapshot.Plugins with an already-resolved list, as the
// live session does at Start and again on every available_commands_update.
// Tests build it through agent.ResolvePluginNames so the naming rule under test
// is the one the session applies.
func (s *Stub) SetPlugins(plugins []agent.PluginCommand) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snap.Plugins = append([]agent.PluginCommand(nil), plugins...)
}

// SetTitle replaces Snapshot.Title.
func (s *Stub) SetTitle(title string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snap.Title = title
}

// Emit publishes an event as the live session would, stamped with Clock.
func (s *Stub) Emit(ev agent.Event) { s.emit(ev) }

func (s *Stub) FailNextSetMode() {
	s.mu.Lock()
	s.failMode = true
	s.mu.Unlock()
}

func (s *Stub) FailNextSetModel() {
	s.mu.Lock()
	s.failModel = true
	s.mu.Unlock()
}

func (s *Stub) FailNextSetConfig() { s.FailNextSetConfigAfter(0) }

// FailNextSetConfigAfter makes the nth SetConfig from now fail, counting from
// zero.
func (s *Stub) FailNextSetConfigAfter(n int) {
	s.mu.Lock()
	s.failConfigAt = s.configCalls + n
	s.mu.Unlock()
}

// DelayStart makes Start take d, so tests can prove callers wait for it.
func (s *Stub) DelayStart(d time.Duration) {
	s.mu.Lock()
	s.startDelay = d
	s.mu.Unlock()
}

func (s *Stub) Start(context.Context) error {
	s.mu.Lock()
	d := s.startDelay
	s.mu.Unlock()
	if d > 0 {
		time.Sleep(d)
	}
	return nil
}

func (s *Stub) Events() <-chan agent.Event { return s.events }

func (s *Stub) Prompt(ctx context.Context, text string) (agent.Result, error) {
	s.mu.Lock()
	park := s.park
	s.park = false
	s.mu.Unlock()
	if park {
		// Before the bookkeeping, where the live session's wait also sits:
		// nothing has been claimed, so the cancelled ending is the whole of
		// what this prompt leaves behind.
		select {
		case <-s.cancel:
		case <-s.closed:
		case <-ctx.Done():
		}
		return agent.Result{}, agent.ErrPromptCancelled
	}
	s.mu.Lock()
	// One prompt at a time, as the live session has it: without the guard a
	// second prompt's deferred clear would report the first one's turn over
	// while it is still running, and the queue would drain into it.
	if s.inPrompt {
		s.mu.Unlock()
		return agent.Result{}, agent.ErrPromptInFlight
	}
	hang := s.hang
	s.hang = false
	s.n++
	n := s.n
	s.inPrompt = true
	s.doneEmitted = false
	s.cancelling = false
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.inPrompt = false
		s.cancelling = false
		s.mu.Unlock()
	}()

	if hang {
		select {
		case <-s.cancel:
		case <-s.closed:
		case <-ctx.Done():
		}
		s.markDone()
		s.emit(agent.Event{Type: agent.EventDone, StopReason: "cancelled"})
		return agent.Result{StopReason: "cancelled"}, nil
	}

	reply := "echo: " + text
	if n >= 2 {
		reply = "follow-up: " + text
	}
	s.emit(agent.Event{Type: agent.EventText, Text: reply})
	s.markDone()
	s.emit(agent.Event{Type: agent.EventDone, StopReason: "end_turn"})
	return agent.Result{StopReason: "end_turn"}, nil
}

// markDone records that this turn's EventDone has gone out: inPrompt is still
// true until the deferred clear, so it alone cannot say a turn is still open.
func (s *Stub) markDone() {
	s.mu.Lock()
	s.doneEmitted = true
	s.mu.Unlock()
}

func (s *Stub) Cancel(context.Context) error {
	s.mu.Lock()
	s.cancelling = true
	s.mu.Unlock()
	s.cancelOpen()
	select {
	case s.cancel <- struct{}{}:
	default:
	}
	return nil
}

func (s *Stub) AnswerPermission(id, optionID string) error {
	return s.answer(stubCall{Method: "permission", ID: id, Option: optionID, Cancelled: optionID == ""})
}

func (s *Stub) AnswerQuestion(id string, answers map[string][]string, skip bool) error {
	return s.answer(stubCall{Method: "question", ID: id, Answers: answers, Skip: skip})
}

func (s *Stub) AnswerPlan(id string, accept bool) error {
	return s.answer(stubCall{Method: "plan", ID: id, Accept: accept})
}

// Calls is every answer the stub has taken, in order.
func (s *Stub) Calls() []stubCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]stubCall(nil), s.calls...)
}

// answer records one answer. An id that is not waiting is an error, exactly as
// the live session reports one, which is what makes a second answer to the
// same card visible instead of silent.
func (s *Stub) answer(c stubCall) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, o := range s.open {
		if o.id != c.ID {
			continue
		}
		if o.method != c.Method {
			break
		}
		s.open = append(s.open[:i], s.open[i+1:]...)
		s.calls = append(s.calls, c)
		return nil
	}
	return fmt.Errorf("stub: unknown %s request %q", c.Method, c.ID)
}

// cancelOpen answers every request still waiting with its cancelled outcome,
// which is what the live session's Cancel and Close both do.
func (s *Stub) cancelOpen() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, o := range s.open {
		s.calls = append(s.calls, stubCall{Method: o.method, ID: o.id, Cancelled: true})
	}
	s.open = nil
}

// noteOpen registers a blocking request the stub has just announced, the way
// the live session parks one before emitting its event.
func (s *Stub) noteOpen(ev agent.Event) {
	var id, method string
	switch {
	case ev.Permission != nil:
		id, method = ev.Permission.ID, "permission"
	case ev.Question != nil && !ev.Question.Auto:
		id, method = ev.Question.ID, "question"
	case ev.Plan != nil && !ev.Plan.Auto:
		id, method = ev.Plan.ID, "plan"
	}
	if id == "" {
		return
	}
	s.mu.Lock()
	s.open = append(s.open, stubOpen{id: id, method: method})
	s.mu.Unlock()
}

func (s *Stub) SetModel(_ context.Context, id string) error {
	s.mu.Lock()
	fail := s.failModel
	s.failModel = false
	if fail {
		s.mu.Unlock()
		return fmt.Errorf("stub: set model failed")
	}
	s.snap.CurrentModel = id
	s.mu.Unlock()
	return nil
}

func (s *Stub) SetMode(_ context.Context, id string) error {
	s.mu.Lock()
	fail := s.failMode
	s.failMode = false
	if fail {
		s.mu.Unlock()
		return fmt.Errorf("stub: set mode failed")
	}
	s.snap.CurrentMode = id
	s.mu.Unlock()
	return nil
}

func (s *Stub) SetConfig(_ context.Context, id, value string) error {
	s.mu.Lock()
	n := s.configCalls
	s.configCalls++
	if s.failConfigAt == n {
		s.failConfigAt = -1
		s.mu.Unlock()
		return fmt.Errorf("stub: set config failed")
	}
	for i := range s.snap.Config {
		if s.snap.Config[i].ID == id {
			s.snap.Config[i].Current = value
			break
		}
	}
	s.mu.Unlock()
	return nil
}

func (s *Stub) SetProvider(p agent.Provider) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snap.Provider = p.Info()
}

func (s *Stub) Snapshot() agent.Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.snap
	out.Models = append([]agent.ModelInfo(nil), s.snap.Models...)
	out.Modes = append([]agent.ModeInfo(nil), s.snap.Modes...)
	out.Commands = append([]agent.CommandInfo(nil), s.snap.Commands...)
	out.Plugins = append([]agent.PluginCommand(nil), s.snap.Plugins...)
	out.Config = cloneStubConfig(s.snap.Config)
	out.Todos = append([]agent.Todo(nil), s.snap.Todos...)
	out.Tools = cloneStubTools(s.snap.Tools)
	out.Subagents = cloneStubSubagents(s.snap.Subagents)
	out.ForeignTurn = s.foreign
	// mu → the queue's lock is the order every transaction takes.
	out.Queue = s.queue.List()
	return out
}

func cloneStubSubagents(in []agent.SubagentInfo) []agent.SubagentInfo {
	if in == nil {
		return nil
	}
	out := make([]agent.SubagentInfo, len(in))
	for i, s := range in {
		out[i] = s
		if s.ToolsUsed != nil {
			out[i].ToolsUsed = append([]string(nil), s.ToolsUsed...)
		}
	}
	return out
}

func cloneStubConfig(in []agent.ConfigOption) []agent.ConfigOption {
	if in == nil {
		return nil
	}
	out := make([]agent.ConfigOption, len(in))
	for i, c := range in {
		out[i] = c
		if c.SelectValues != nil {
			out[i].SelectValues = append([]agent.SelectValue(nil), c.SelectValues...)
		}
	}
	return out
}

func cloneStubTools(in []agent.ToolEvent) []agent.ToolEvent {
	if in == nil {
		return nil
	}
	out := make([]agent.ToolEvent, len(in))
	for i, t := range in {
		out[i] = t
		if t.Locations != nil {
			out[i].Locations = append([]string(nil), t.Locations...)
		}
	}
	return out
}

func (s *Stub) Close() error {
	s.cancelOpen()
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-s.closed:
	default:
		close(s.closed)
	}
	return nil
}

// now reads Clock through an indirection so tests can inject one.
func (s *Stub) now() time.Time {
	if s.Clock != nil {
		return s.Clock()
	}
	return time.Now()
}

func (s *Stub) emit(ev agent.Event) {
	if ev.At.IsZero() {
		ev.At = s.now()
	}
	s.noteOpen(ev)
	select {
	case <-s.closed:
		return
	default:
	}
	select {
	case s.events <- ev:
	case <-s.closed:
	}
}

var _ agent.Session = (*Stub)(nil)
