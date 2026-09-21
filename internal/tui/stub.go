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
	mu sync.Mutex
	// log is the live session's event log, the same code: emit publishes
	// into it and Events is its primary, so the goldens run on numbered
	// events exactly as a real session produces them.
	log *agent.EventLog
	// asks is the Stub's ask registry — the same component the live session
	// parks its blocking requests in, so a card a test emits is opened,
	// answered and ended by the code every golden runs on (plan 021 §3.6).
	asks   *agent.AskRegistry
	closed chan struct{}
	cancel chan struct{}
	// hang makes the next prompt a turn that stays open until cancelled; hung is
	// the barrier that turn closes once it is open (HangNext).
	hang bool
	hung chan struct{}
	// park is the live session's catalog wait (§3.3): a prompt held back
	// before any of the turn's bookkeeping, which a Cancel ends by handing it
	// agent.ErrPromptCancelled. parked is the barrier that prompt closes on
	// its way into the wait, so a test can be sure the Cancel it sends next
	// lands on a parked prompt and not ahead of one.
	park       bool
	parked     chan struct{}
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
	// token is the running turn's registry token, the no-turn token between
	// turns: what a card the test emits now is parked against, and therefore
	// what a cancel or that turn's end takes away.
	token agent.TurnToken
	// Clock stamps Event.At; tests inject one to drive lingers and elapsed
	// times without sleeping. It is also what the Stub answers agent.Clocked
	// with, so a component above the seam that stamps its own events reads this
	// clock and not the wall one (plan 021 §3.9).
	//
	// It is read unlocked, by emit and by Now, from whatever goroutine publishes
	// or asks the time — so it must be set before the Stub is used concurrently,
	// and never while a turn, a component above the seam or another goroutine is
	// running. The closure it holds must itself be safe to call from several
	// goroutines.
	Clock func() time.Time
	// NoPrimary reports that this Stub's log was built with
	// agent.EventLogOptions.NoPrimary — NewStubNoPrimary — so nothing is ever
	// put on Events() and no publisher can be held by a reader that is not
	// there. It is fixed at construction, because that is where the log is
	// built; assigning it afterwards changes nothing.
	NoPrimary bool

	// Replay is the transcript Start hands back before the session is up, as
	// a loaded session's replay does. Start emits agent.EventReplay{start},
	// then each of these with Replayed set, then agent.EventReplay{end} —
	// the same bracket the live session's session/load path emits, so a test
	// can drive replay rendering without an agent.
	Replay []agent.Event
	// titlePinned is /rename's pin, as on the live session: once set, a
	// title the agent produces no longer replaces the user's.
	titlePinned bool

	// inPrompt, doneEmitted and cancelling mirror the live session's turn
	// state, which is what Interject is decided from.
	// claimed is the live session's claim: Begin takes the prompt slot before
	// the prompt's own goroutine opens the turn, and a Cancel in between
	// withdraws the prompt instead of reaching the agent.
	inPrompt     bool
	claimed      bool
	doneEmitted  bool
	cancelling   bool
	foreign      bool
	interjectErr error
	// prompts is every prompt handed to Begin, in order, and a withdrawn one
	// stays recorded, as its row stays in the transcript. cancelsSent counts
	// the cancels the live session would have written to the agent: every
	// Cancel but one that a claimed, unopened prompt withdraws for.
	prompts []string
	// interjections is every text handed to Interject, in order; see
	// Interjections in stub_queue.go.
	interjections []string
	cancelsSent   int
}

// stubCall is one answer the UI sent, or the cancelled outcome Cancel/Close
// produced for a request nobody answered. It is a projection of the registry's
// own terminal records (Calls), kept in this shape because it is what every
// card test reads.
type stubCall struct {
	Method    string // permission | question | plan
	ID        string
	Option    string
	Answers   map[string][]string
	Skip      bool
	Accept    bool
	Cancelled bool
}

// NewStub is the Stub every test and the TUI's own fallback session use.
func NewStub() *Stub { return newStub(false) }

// NewStubNoPrimary is NewStub with an event log that has no primary send
// (agent.EventLogOptions.NoPrimary): nothing is ever put on Events(), so a test
// can drive a session nobody reads without a publisher ever blocking. It is a
// constructor rather than a field on the Stub because the log is built here, and
// what a caller passes after construction would be read too late.
func NewStubNoPrimary() *Stub { return newStub(true) }

func newStub(noPrimary bool) *Stub {
	s := &Stub{
		failConfigAt: -1,
		NoPrimary:    noPrimary,
		log:          agent.NewEventLog(agent.EventLogOptions{NoPrimary: noPrimary}),
		closed:       make(chan struct{}),
		cancel:       make(chan struct{}, 1),
		snap: agent.Snapshot{
			// A session id, as a started live session has: the index writes
			// are guarded by one, so a stub without it could never exercise
			// them.
			SessionID:    "stub-session-1",
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
	// Beside the log, stamped from the Stub's own clock, so an ask's record
	// carries the time a test injected and not the wall's.
	s.asks = agent.NewAskRegistry(s.log, s.now)
	return s
}

// HangNext makes the next prompt a turn that stays open until it is cancelled
// or the session closes.
//
// The channel it returns closes once that turn is OPEN. A cancel that lands
// between the claim and the opening withdraws the prompt instead — no turn, no
// event, agent.ErrPromptCancelled, as the live session has it — so a test that
// goes on to wait for the hung turn's own cancelled ending has to receive from
// it before it cancels, or it is waiting for an event a withdrawn prompt never
// publishes whenever the cancel wins that race. A test that cancels without
// caring which of the two it gets may ignore it.
func (s *Stub) HangNext() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Armed already: the same next prompt, so the same barrier. A second channel
	// would strand whoever holds the first.
	if !s.hang {
		s.hang = true
		s.hung = make(chan struct{})
	}
	return s.hung
}

// ParkNext makes the next prompt behave as one the live session holds back for
// the agent's first command catalog: it opens no turn, emits no event of any
// kind, and a Cancel while it is parked hands it agent.ErrPromptCancelled.
// HangNext cannot stand in for it — a hung prompt is a turn on the wire, and
// what a cancelled wait leaves the UI is an error and nothing else.
//
// The channel it returns closes as that prompt goes into the wait. A test that
// cancels without receiving from it is not testing a cancelled wait at all: the
// Cancel would find the prompt claimed and not yet parked, and the prompt would
// withdraw without parking, which passes whether or not Cancel can reach a
// prompt that is already parked.
func (s *Stub) ParkNext() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.park = true
	s.parked = make(chan struct{})
	return s.parked
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

// SetTitle is /rename, with the live session's semantics: it replaces
// Snapshot.Title, pins it against a later agent one, and says so in a Title
// delta with no Event.Text — so a rename still prints no title line. It waits
// on nothing (it is called from a UI's Update), and it is the one setter that
// can refuse for want of room in the log, because its check is atomic with its
// mutation.
func (s *Stub) SetTitle(cause, title string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.log.OutboxRoom() {
		return agent.ErrSetUnavailable
	}
	s.snap.Title = title
	s.titlePinned = true
	s.enqueueDeltaLocked(cause, &agent.StateDelta{Title: &title})
	return nil
}

// AgentTitle is a session_info_update: the agent's own title, which a pin from
// SetTitle refuses. It is how a test reaches the live session's rule without
// an agent.
//
// Like SetCommands, SetPlugins and the rest of the Set* helpers on this type it
// publishes nothing: it is test set-up — how a test builds the session it wants
// before the model looks at it — and not a change a session made while a client
// was watching. The live session's own equivalent is the read loop's
// session_info_update, which does publish a delta (live.go's onUpdate); the one
// helper here that is a *seam* method, SetTitle, publishes one too.
func (s *Stub) AgentTitle(title string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.titlePinned {
		return
	}
	s.snap.Title = title
}

// Emit publishes an event as the live session would, stamped with Clock.
//
// An event that opens a blocking request — a permission, or a question or plan
// that is not already auto-answered — is routed through the ask registry
// instead of published directly, exactly as the live session's handlers route
// one: the registry publishes the opening, and the ask can then be answered,
// cancelled or ended with its turn like any other (plan 021 §3.6). The test's
// own id is **adopted**, because some forty test sites choose their ids and
// read them back, and the flush before this returns keeps "emitted means
// buffered" true for a test that emits and then looks.
//
// It is called from test goroutines and from a turn's own continuation, never
// from the primary's reader, which is what makes the flush safe.
func (s *Stub) Emit(ev agent.Event) {
	req, ok := stubAskRequest(ev)
	if !ok {
		s.emit(ev)
		return
	}
	s.mu.Lock()
	token := s.token
	s.mu.Unlock()
	// A refused open — the turn ended or was cancelled, the session closed —
	// still writes its one self-contained ending, which is the answer to "what
	// became of this card"; there is nothing here to hand it to.
	if _, err := s.asks.Open(context.Background(), token, req); err != nil {
		return
	}
	// Bounded by the Stub's own close, like every other flush a session makes on
	// its own goroutines (agent.EventLog.Flush): a test that emits while another
	// goroutine closes the Stub gets the flush's semantics it had, and cannot be
	// left waiting for a barrier the close is on its way to freeing.
	_ = s.log.Flush(context.Background(), s.closed)
}

// stubAskRequest is the ask an event opens, and whether it opens one at all. An
// Auto question or plan is a request craze has already answered: it carries no
// card and opens nothing, exactly as the live session's automatic path does not
// park one.
func stubAskRequest(ev agent.Event) (agent.AskRequest, bool) {
	switch {
	case ev.Permission != nil:
		return agent.AskRequest{
			ID:   ev.Permission.ID,
			Kind: agent.AskPermission,
			Body: agent.AskBody{Permission: ev.Permission},
		}, true
	case ev.Question != nil && !ev.Question.Auto:
		return agent.AskRequest{
			ID:   ev.Question.ID,
			Kind: agent.AskQuestion,
			Body: agent.AskBody{Question: ev.Question},
		}, true
	case ev.Plan != nil && !ev.Plan.Auto:
		return agent.AskRequest{
			ID:   ev.Plan.ID,
			Kind: agent.AskPlan,
			Body: agent.AskBody{Plan: ev.Plan},
		}, true
	default:
		return agent.AskRequest{}, false
	}
}

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
	// nil Replay is "this is a new session"; a non-nil Replay is "this is a
	// load", empty or not. append flattens both to nil, so the distinction has
	// to be taken before it: the live session brackets a session/load whose
	// transcript turned out to be empty just the same, and a model built with
	// Config.Loading would otherwise sit in the restoring state forever.
	loaded := s.Replay != nil
	replay := append([]agent.Event(nil), s.Replay...)
	s.mu.Unlock()
	if d > 0 {
		time.Sleep(d)
	}
	if !loaded {
		return nil
	}
	// Bracketed exactly as the live session brackets a session/load: the end
	// phase goes out only once everything the replay carried has, so a
	// consumer that treats it as "the restored session is ready" is right.
	s.emit(agent.Event{Type: agent.EventReplay, Replay: &agent.ReplayInfo{Phase: agent.ReplayStart}})
	for _, ev := range replay {
		ev.Replayed = true
		s.emit(ev)
	}
	s.emit(agent.Event{Type: agent.EventReplay, Replay: &agent.ReplayInfo{Phase: agent.ReplayEnd}})
	return nil
}

func (s *Stub) Events() <-chan agent.Event { return s.log.Primary() }

// Subscribe and Incarnation are the Stub's agent.EventSource: its log's.
func (s *Stub) Subscribe(o agent.SubscribeOptions) (*agent.Subscription, error) {
	return s.log.Subscribe(o)
}
func (s *Stub) Incarnation() string { return s.log.Incarnation() }

// EventLog is the Stub's agent.LogOwner (plan 021 §3.3): the log a component
// above the seam publishes into, the same one the Stub's own emit uses, so a
// test sees one sequence whoever produced an event.
func (s *Stub) EventLog() *agent.EventLog { return s.log }

// Now is the Stub's agent.Clocked (plan 021 §3.9): the injected Clock, falling
// back to time.Now exactly as emit does, so a component that stamps its own
// events stamps them from the clock the test set and the goldens stay put. Like
// emit it reads Clock unlocked, so Clock must be configured before the Stub is
// used concurrently (Clock).
func (s *Stub) Now() time.Time { return s.now() }

// Prompt is Begin and its continuation back to back, as on the live session.
func (s *Stub) Prompt(ctx context.Context, text string) (agent.Result, error) {
	return s.Begin(text)(ctx)
}

// Begin is the live session's claim, taken on the caller's goroutine. It
// clears the cancel mark and any cancellation token a Cancel left buffered:
// a cancel asked before the claim is not for this prompt. A Begin while the
// slot is claimed or a turn is open claims nothing and is refused.
func (s *Stub) Begin(text string) func(context.Context) (agent.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prompts = append(s.prompts, text)
	if s.claimed || s.inPrompt {
		return func(context.Context) (agent.Result, error) { return agent.Result{}, agent.ErrPromptInFlight }
	}
	s.claimed = true
	s.cancelling = false
	select {
	case <-s.cancel:
	default:
	}
	return func(ctx context.Context) (agent.Result, error) { return s.run(ctx, text) }
}

// Prompts is every prompt handed to Begin, in order.
func (s *Stub) Prompts() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.prompts...)
}

// CancelsSent is how many cancels would have reached the agent.
func (s *Stub) CancelsSent() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cancelsSent
}

// InTurn reports that a claimed prompt has opened its turn — run's own
// bookkeeping, which happens on the driver's goroutine some moment after Begin
// returned the continuation.
//
// It is the barrier a test needs before it asks what a cancel did: a cancel
// that lands on a claim whose turn is not open yet withdraws the prompt and
// writes nothing to the agent, and one that lands a moment later writes
// (Cancel). Which of the two a test gets is otherwise a coin toss.
func (s *Stub) InTurn() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inPrompt
}

// run is Begin's continuation. Like the live session's, it withdraws — no
// turn, no event, agent.ErrPromptCancelled — when it finds itself cancelled
// before its turn is open: it does not park, and the opening withdraws it.
func (s *Stub) run(ctx context.Context, text string) (agent.Result, error) {
	opened := false
	defer func() {
		s.mu.Lock()
		s.claimed = false
		s.cancelling = false
		if opened {
			s.inPrompt = false
			s.token = agent.TurnToken{}
		}
		s.mu.Unlock()
	}()
	s.mu.Lock()
	// Cancelled since the claim: a prompt the live session would have held
	// for the catalog does not park once it is cancelled, and goes straight
	// on to the opening, which withdraws it.
	park, parked := s.park && !s.cancelling, s.parked
	s.park, s.parked = false, nil
	s.mu.Unlock()
	if park {
		// Before the bookkeeping, where the live session's wait also sits: no
		// turn is open, so the cancelled ending is the whole of what this
		// prompt leaves behind.
		if parked != nil {
			// The barrier goes down one statement short of the select, which
			// is as close as Go gets. The gap is not observable: s.cancel is
			// buffered, so a Cancel that lands in it is taken by this select
			// the moment it runs — and it is this prompt that takes it, which
			// is the whole of what the barrier promises.
			close(parked)
		}
		select {
		case <-s.cancel:
		case <-s.closed:
		case <-ctx.Done():
		}
		return agent.Result{}, agent.ErrPromptCancelled
	}
	// The registry's name for this turn, minted before the section that opens
	// it — the registry is never called with s.mu held — and ended on every
	// return path, the withdrawals included (endAskTurn is idempotent).
	token := s.beginAskTurn()
	defer s.asks.EndTurn(token)
	s.mu.Lock()
	// The hang belongs to the prompt it was armed for and is consumed by it
	// whatever becomes of that prompt, which is why it is taken here rather than
	// past the checks below: a cancel that reached the claim before this opening
	// withdraws the prompt, and a flag left armed would hang the *next* one
	// instead — a test that cancelled a hung turn and then sent again would hang
	// or not depending on which won, which is a coin toss and not a test (plan
	// 021 amendment X12). park is taken the same way, above.
	hang, hung := s.hang, s.hung
	s.hang, s.hung = false, nil
	if s.cancelling {
		// Cancelled since the claim, and the turn is not open: withdraw.
		s.mu.Unlock()
		return agent.Result{}, agent.ErrPromptCancelled
	}
	// One prompt at a time, as the live session has it: without the guard a
	// second prompt's deferred clear would report the first one's turn over
	// while it is still running, and the queue would drain into it.
	if s.inPrompt {
		s.mu.Unlock()
		return agent.Result{}, agent.ErrPromptInFlight
	}
	s.n++
	n := s.n
	s.inPrompt = true
	s.token = token
	opened = true
	s.doneEmitted = false
	s.mu.Unlock()

	if hang {
		if hung != nil {
			// The turn is open: from here a Cancel finds a turn to end, and this
			// prompt is the one that takes it (s.cancel is buffered).
			close(hung)
		}
		select {
		case <-s.cancel:
		case <-s.closed:
		case <-ctx.Done():
		}
		s.markDone()
		s.endAskTurn(token)
		s.emit(agent.Event{Type: agent.EventDone, StopReason: "cancelled"})
		return agent.Result{StopReason: "cancelled"}, nil
	}

	reply := "echo: " + text
	if n >= 2 {
		reply = "follow-up: " + text
	}
	s.emit(agent.Event{Type: agent.EventText, Text: reply})
	s.markDone()
	s.endAskTurn(token)
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

// beginAskTurn mints the registry token this turn's asks park against. It is
// called with s.mu released, before the section that opens the turn, and the
// Stub's own run and the scripted decorator both go through it.
func (s *Stub) beginAskTurn() agent.TurnToken { return s.asks.BeginTurn() }

// endAskTurn ends the turn for the registry and then waits for the outbox, so
// every ask that turn still held has ended — and every opening still in flight
// has been delivered — before the caller publishes the turn's terminal event
// (plan 021 §3.6). It is idempotent, and safe on a closing log, where the flush
// returns at once; the flush is bounded by the Stub's close as the live
// session's is bounded by its done.
//
// It takes the turn's token down with it, so a card emitted after the ending
// belongs to no turn of craze's own and is raised like any other between-turns
// request — which is what the live session does with one, since a request that
// reaches it then carries no turn membership (acp.Arrival).
func (s *Stub) endAskTurn(token agent.TurnToken) {
	s.mu.Lock()
	if s.token == token {
		s.token = agent.TurnToken{}
	}
	s.mu.Unlock()
	s.asks.EndTurn(token)
	_ = s.log.Flush(context.Background(), s.closed)
}

// Cancel mirrors the live session's CancelOutcome (plan 021 §3.7), fired and
// forgotten rather than waited on: the Stub's Cancel never blocks, so it can
// only ever report what is already known at the moment it is called. Wrote is
// exactly the condition that already drove cancelsSent — nothing claimed (a
// foreign turn or idle), or a turn genuinely open — and Withdrew its
// complement, a claim not yet open. Settled is true only when nothing is
// claimed or open: an open or about-to-withdraw turn is still going, by
// definition, the instant this returns.
func (s *Stub) Cancel(context.Context) (agent.CancelOutcome, error) {
	s.mu.Lock()
	s.cancelling = true
	// A prompt claimed and not yet open finds the mark and withdraws, or is
	// parked where the live session aborts its wait: either way nothing
	// reaches the agent for it. Every other cancel would.
	wrote := !s.claimed || s.inPrompt
	withdrew := s.claimed && !s.inPrompt
	settled := !s.claimed
	if wrote {
		s.cancelsSent++
	}
	token := s.token
	s.mu.Unlock()
	// Every open ask is answered cancelled and the running turn is marked, as
	// the live session's Cancel does: with s.mu released, because s.mu and the
	// registry's are never nested.
	s.asks.CancelTurn(token)
	select {
	case s.cancel <- struct{}{}:
	default:
	}
	return agent.CancelOutcome{Wrote: wrote, Withdrew: withdrew, Settled: settled}, nil
}

// Asks is the Stub's agent.AskSource (plan 021 §3.6): the registry a client
// above the seam lists and answers through engine.Control, the same one the
// live session has.
func (s *Stub) Asks() *agent.AskRegistry { return s.asks }

// Calls is every answer the stub has taken, in the order the asks were
// resolved: each of the registry's terminal records as the answer that produced
// it, plus the cancelled outcome a Cancel or a Close produced for a request
// nobody answered. It is rebuilt from the registry rather than kept beside it,
// so there is exactly one account of what became of a card.
func (s *Stub) Calls() []stubCall {
	resolved := s.asks.Resolved()
	out := make([]stubCall, 0, len(resolved))
	for _, rec := range resolved {
		out = append(out, stubCallOf(rec))
	}
	return out
}

// stubCallOf is one terminal record as a stubCall. Anything but an answer —
// cancelled, ended with its turn, closing, and the permission the policy could
// not answer — is Cancelled, which is what the agent was told in each case.
func stubCallOf(rec agent.AskRecord) stubCall {
	c := stubCall{ID: rec.ID, Method: string(rec.Kind)}
	if rec.Outcome != agent.AskAnswered {
		c.Cancelled = true
		return c
	}
	c.Option = rec.Answer.OptionID
	c.Answers = rec.Answer.Answers
	c.Skip = rec.Answer.Skip
	c.Accept = rec.Answer.Accept
	return c
}

// The three settings verbs, with the live session's shape (agent.Session): a
// refusal changes nothing and says nothing, and a change that took is mutated
// into the snapshot and its delta enqueued in one locked section, so state
// order is event order here as it is on a real session (plan 021 §3.8). The
// ticket is the delta's receipt: its Seq, once the log has committed it, is the
// change's revision.
func (s *Stub) SetModel(_ context.Context, cause, id string) (agent.SetOutcome, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fail := s.failModel
	s.failModel = false
	if fail {
		return agent.SetOutcome{}, fmt.Errorf("stub: set model failed")
	}
	s.snap.CurrentModel = id
	return agent.SetOutcome{
		Value:  s.snap.CurrentModel,
		Ticket: s.enqueueDeltaLocked(cause, &agent.StateDelta{Model: &id}),
	}, nil
}

func (s *Stub) SetMode(_ context.Context, cause, id string) (agent.SetOutcome, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fail := s.failMode
	s.failMode = false
	if fail {
		return agent.SetOutcome{}, fmt.Errorf("stub: set mode failed")
	}
	s.snap.CurrentMode = id
	// Event.Mode stays empty, as on the live session: it is an agent-initiated
	// update's field, and a client retires a plan offer on it.
	return agent.SetOutcome{
		Value:  s.snap.CurrentMode,
		Ticket: s.enqueueDeltaLocked(cause, &agent.StateDelta{Mode: &id}),
	}, nil
}

// SetConfig is the live session's, including its model rule: setting the option
// a provider keeps its MODEL in (agent.IsModelConfigOption) moves CurrentModel
// too and says both sections in the one delta, so `/model`'s fallback — the
// model set as a config option — reaches the model section's revision and the
// status row exactly as a session/set_model would (plan 021 §3.8, r23 finding
// 3). A test builds such an option with ModelConfigOption below; the Stub's own
// default config has none, as cursor's captures have one and grok's do not.
func (s *Stub) SetConfig(_ context.Context, cause, id, value string) (agent.SetOutcome, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.configCalls
	s.configCalls++
	if s.failConfigAt == n {
		s.failConfigAt = -1
		return agent.SetOutcome{}, fmt.Errorf("stub: set config failed")
	}
	model := false
	for i := range s.snap.Config {
		if s.snap.Config[i].ID == id {
			s.snap.Config[i].Current = value
			model = agent.IsModelConfigOption(s.snap.Config[i])
			break
		}
	}
	st := &agent.StateDelta{Config: &agent.ConfigState{Options: cloneStubConfig(s.snap.Config)}}
	if model {
		s.snap.CurrentModel = value
		st.Model = &value
	}
	return agent.SetOutcome{Value: value, Ticket: s.enqueueDeltaLocked(cause, st)}, nil
}

// ModelConfigOption gives this Stub the config-backed model a provider without
// session/set_model has: an option of category "model" whose values are the
// models it already advertises, currently on CurrentModel. It is set-up, so it
// publishes nothing — like every other Set* helper here, it is how a test
// builds the session it wants **before the model looks at it**, and not a
// change made while a client was watching.
func (s *Stub) ModelConfigOption(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	values := make([]agent.SelectValue, 0, len(s.snap.Models))
	for _, m := range s.snap.Models {
		values = append(values, agent.SelectValue{Value: m.ID, Name: m.Name})
	}
	s.snap.Config = append(cloneStubConfig(s.snap.Config), agent.ConfigOption{
		ID:           id,
		Name:         "Model",
		Category:     "model",
		Type:         "select",
		Current:      s.snap.CurrentModel,
		SelectValues: values,
	})
}

// enqueueDeltaLocked is the live session's own helper (live.go): one EventMeta
// carrying st, stamped from the Stub's injected clock, enqueued under mu in the
// section that changed the snapshot. mu is held, and the outbox's mutex is a
// leaf, so nothing blocks here.
func (s *Stub) enqueueDeltaLocked(cause string, st *agent.StateDelta) *agent.Ticket {
	return s.log.EnqueueTicket(agent.Event{
		Type: agent.EventMeta, State: st, Cause: cause, At: s.now(),
	})
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
	return out
}

// ForeignTurn is the Session leaf accessor (plan 021 §3.3): s.foreign under
// s.mu alone, the same field Snapshot reports.
func (s *Stub) ForeignTurn() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.foreign
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

// Close answers what is still open, closes the signal every emit and wait
// selects on, and then closes the event log, as the live session's Close
// does: the log last, and with mu released, because its boundary is never
// taken under mu. It is idempotent; the log's Close is too.
func (s *Stub) Close() error {
	// Every parked ask ends as closing, before the log's own close, so those
	// endings are in the record (plan 021 §3.3's close phases).
	s.asks.Close()
	s.mu.Lock()
	select {
	case <-s.closed:
	default:
		close(s.closed)
	}
	s.mu.Unlock()
	s.log.Close(context.Background())
	return nil
}

// now reads Clock through an indirection so tests can inject one.
func (s *Stub) now() time.Time {
	if s.Clock != nil {
		return s.Clock()
	}
	return time.Now()
}

// emit publishes ev through the log, as the live session's emit does: on
// this goroutine, so an emit that returned has its event in Events()'s
// buffer, and never with mu held.
func (s *Stub) emit(ev agent.Event) {
	if ev.At.IsZero() {
		ev.At = s.now()
	}
	select {
	case <-s.closed:
		s.log.Abandoned()
		return
	default:
	}
	s.log.Publish(context.Background(), s.closed, ev)
}

var (
	_ agent.Session     = (*Stub)(nil)
	_ agent.EventSource = (*Stub)(nil)
	_ agent.LogOwner    = (*Stub)(nil)
	_ agent.Clocked     = (*Stub)(nil)
	_ agent.AskSource   = (*Stub)(nil)
)
