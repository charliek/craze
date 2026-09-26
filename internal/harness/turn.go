package harness

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"charm.land/fantasy"
	"github.com/charliek/craze/internal/harness/llm"
	"github.com/charliek/craze/internal/harness/redact"
	"github.com/charliek/craze/internal/harness/store"
)

// The stop reasons a turn ends with. They are ACP's words for the same
// outcomes, so the adapter passes them through.
const (
	// StopEndTurn is a turn the model finished.
	StopEndTurn = "end_turn"
	// StopCancelled is a turn cut short by Run's context or by Close.
	StopCancelled = "cancelled"
	// StopMaxTokens is an answer cut off by the output ceiling ("length").
	StopMaxTokens = "max_tokens"
	// StopRefusal is an answer the provider's content filter stopped
	// ("content-filter"). It is not end_turn, so `craze prompt`, which exits
	// 1 on any stop reason but end_turn, does not pass a filtered answer off
	// as a clean one.
	StopRefusal = "refusal"
	// StopMaxTurnRequests is a turn the harness stopped while the model
	// still had tool results to read: at the step limit (maxSteps). `craze
	// prompt` exits 1 for it, as for any stop but end_turn.
	StopMaxTurnRequests = "max_turn_requests"
	// StopToolUse is the stop reason a step whose tool calls ran records in
	// the transcript: the model is not done, the next step reads the
	// results. A turn never ends with it; a turn stopped after such a step
	// ends cancelled or max_turn_requests.
	StopToolUse = "tool_use"
)

// maxRetries is Fantasy's retry budget for a step. Retrying has no surface
// in H1, so one retry bounds the worst silent stall to one backoff; and the
// wrapper only lets a step be retried before any of it reached the screen
// (plan 018 §3.5, §3.7).
const maxRetries = 1

// maxSteps bounds the model requests one turn makes: a model that keeps
// calling tools is stopped after this many steps, with max_turn_requests
// (plan 019 §3.5).
const maxSteps = 200

// Result is how a turn ended. StopReason and Usage — the turn's token counts,
// every step's summed, zero for a cancelled turn — are filled in for a turn
// that did not fail; Unanswered is filled in whatever the turn did.
type Result struct {
	StopReason string
	Usage      Usage
	// Unanswered is the text of every steer Steer accepted that no step
	// persisted, in the order it was accepted: one the turn ended before a
	// step could take it up, and one taken into a step whose append never
	// happened. It is returned alongside an error too — a turn that failed
	// still owes the user whatever they typed into it (plan 019 §3.10). A
	// steer a step did persist is not here: the model read it, and the
	// transcript holds it.
	Unanswered []string
}

// Run sends text as the next user turn and blocks until the turn ends. What
// the turn does streams to sink as it happens (see Event): the answer's text
// and thinking, the tool calls the model makes — which the session runs, a
// step at a time, until the model answers without one — and each step's
// end. A nil sink discards events.
//
// The turn's own events reach sink only while the turn holds its own lock, so
// two of them never arrive at once, though tool events come from Fantasy's
// tool goroutines; the lifecycle of a sub-agent — SubagentStarted and
// SubagentFinished — is the turn's own and arrives the same way. A
// sub-agent's own events do not (plan 026 §3.9): each child's turn hands them,
// wrapped in SubagentEvent, straight to this sink while it holds the child's
// lock and never this turn's, so that nothing on the parent's side ever waits
// on a child. While children run, sink is therefore entered concurrently — by
// this turn under its lock, and by each child under its own — and a consumer
// whose state both touch must guard it. The events of any one child still
// arrive one at a time and in its order. Nothing reaches sink after Run
// returns: a child is joined before its agent call returns, and the turn
// before its calls. It must not block for long, and it must never block on a
// ToolProgress, a child's included: progress is lossy by contract, and a
// snapshot the turn cannot hand over at once (its lock is busy) is dropped,
// never queued — a consumer that may block delivers ToolProgress without
// blocking, or drops it. A consumer that blocks on one child's event holds up
// that child, and neither the parent nor another child. sink may call the
// session's other methods, but not Close, which waits for this Run.
//
// It returns a Result when the model finished (end_turn, max_tokens,
// refusal), when the harness stopped it (max_turn_requests), or when the
// turn was cancelled — by ctx, or by Close — and an error, with a Result
// carrying nothing but Unanswered, when it failed (see errors.go). A second
// Run while one is live is ErrInTurn; a Run after Close is ErrClosed.
//
// Steer merges text into the turn while it runs; Result.Unanswered is
// whatever it accepted that no step wrote down (steer.go).
//
// The turn is persisted from Fantasy's callbacks, a step at a time (plan
// 019 §3.5, §3.6):
//
//   - The user entry is handed to the store at the start; the store holds it
//     and writes it together with the first step that has output, so a turn
//     that produced nothing leaves nothing in the file, and the next turn's
//     prompt replaces it.
//   - A finished step appends its assistant message (text, reasoning and
//     tool calls, as Fantasy recorded them) with its usage and stop reason,
//     and, when it called tools, the tool message holding their results, in
//     one append (store.AppendStep). A step whose calls Fantasy did not run —
//     an abnormal finish — gets a not_executed result per call first. When
//     the step's messages lack text the user saw stream — Fantasy commits a
//     text block only on its end part, which a provider can omit — the
//     answer's text is the streamed deltas, with the step's tool calls.
//   - A step that cannot be saved stops the turn before another request,
//     and Run returns the error. A step whose calls have an empty or repeated
//     provider id runs none of them, is not saved, and fails the turn with
//     ErrBadToolCalls.
//   - A cancel or failure mid-step appends what streamed, marked
//     interrupted, once Fantasy has returned; the append uses no context, so
//     the cancel that ended the turn cannot abort it. With no text streamed
//     (thinking alone counts as none) nothing is written. Tools honour the
//     cancel and report "Tool execution aborted"; their step then finishes
//     and is saved as usual, and the turn stops there, cancelled.
//   - A cancel that lands after the model's final step was persisted writes
//     nothing more, and the turn keeps that step's stop reason; a cancel
//     after a tool step is a cancelled turn, whatever Fantasy reports.
//
// A model or effort switch made since the last turn (SetModel, SetEffort) is
// handed to the store first, so it is written just ahead of this turn's
// prompt.
//
// In a session with background sub-agents (Options.Background, plan 026
// §3.11), every result waiting to be delivered — and every one a failed wake
// set aside — is taken up at the turn's first step, after the prompt, and one
// that becomes ready while the turn runs at its next step boundary: as a user
// part of their own, written with the step as an entry of results, never a
// steer. A result a step took up and did not write is given back when the
// turn ends, before Run returns; Options.OnPending is then called if one went
// back to waiting.
func (s *Session) Run(ctx context.Context, text string, sink func(Event)) (Result, error) {
	if strings.TrimSpace(text) == "" {
		return Result{}, ErrEmptyPrompt
	}
	return s.run(ctx, text, false, sink)
}

// Wake runs a turn of the session's own (plan 026 §3.11): Run with no prompt
// of the caller's, whose user message is the background sub-agents' results
// waiting to be delivered as it begins — each in its wrapper, in the order
// they finished — so the model reacts to its children without the user
// typing. It takes every pending result (never a suspended one), persists
// them as the turn's user entry — marked as results and carrying their usage
// — and is otherwise Run: the same history, reminders, tools, steers and
// ending, and a result that becomes ready while it runs is taken up at its
// next step boundary. A result it takes and does not write is set aside
// (suspended), whatever ended it — an error, a cancel, or a clean stop that
// persisted nothing — so a wake that keeps failing cannot wake again: the next
// turn a person starts delivers it, or agent_output.
//
// It returns ErrNothingPending, having written and emitted nothing, when no
// result was waiting as it began; ErrInTurn while a turn runs, and ErrClosed
// after Close, as Run does. The caller owns when to call it: typically when
// Options.OnPending has said a result is waiting and no turn of its own is
// running or about to start.
func (s *Session) Wake(ctx context.Context, sink func(Event)) (Result, error) {
	return s.run(ctx, "", true, sink)
}

// run is Run and Wake, one implementation (see both). For a wake text is
// empty until the results it takes are known.
func (s *Session) run(ctx context.Context, text string, wake bool, sink func(Event)) (Result, error) {
	if sink == nil {
		sink = func(Event) {}
	}
	m, changes, turnCtx, number, err := s.begin(ctx)
	if err != nil {
		return Result{}, err
	}
	// The turn's reservations of background results are settled once it has
	// ended, on every path — after the interrupted answer is saved (finish),
	// and on a return before the model was asked or a panic too — and before
	// the session is released; whoever is told a result is waiting again is
	// told once it has been, holding no lock.
	gave := false
	defer func() {
		if gave {
			s.subs.notifyPending(false)
		}
	}()
	defer s.end()
	defer func() { gave = s.subs.restoreTurn(number) }()

	// The entry the turn opens with records the turn's number (plan 028 §3.2,
	// P10): it is how a replay tells a prompt from a steer, and where a
	// resumed session's numbering continues from. A wake's is its results.
	user := store.MessageEntry{Model: m.id(), Effort: m.effort, Turn: number}
	var held []string
	if wake {
		// Taken under the runner's lock now the session is claimed, owned by
		// the wake's first step (P47) before anything is sent.
		b := s.subs.reserve(owner{turn: number, step: 1, wake: true}, false)
		if b == nil {
			// Nothing was waiting: no turn was, and its number is not spent.
			// The session is still claimed, so no other turn has begun.
			s.mu.Lock()
			s.turns--
			s.mu.Unlock()
			return Result{}, ErrNothingPending
		}
		text, held = b.text, b.ids
		user.SubagentUsage, user.SubagentResults = b.rows, true
	}

	if err := s.record(m, changes); err != nil {
		return Result{}, err
	}
	// The history the request replays, redacted: an entry written before the
	// session knew a key can hold one — a switch resolves keys mid-session,
	// and this turn may be the one that adopted them — and the replay would
	// send it to the model, the newly switched one included. The same places
	// are redacted as when a step is persisted (redactCalls, redactResults): a
	// tool call's arguments and a tool result's text, never the model's own
	// text or reasoning — and the text of an entry of background results,
	// which a child wrote (plan 026 §3.11), never a person's prompt.
	//
	// The replacer is the session's live union, the keys Session.Redact
	// covers — the parent's, every registered child's, and every background
	// child's whose result is not yet delivered — not the turn's own (astra
	// r15, finding 1): a result committed before anyone knew a key, which a
	// background child learned since and the parent never did, would
	// otherwise go out whole while that child runs or its result waits. It is
	// a superset of the turn's redactor, and redacting more is never a leak.
	//
	// It leaves the transcript on disk as it was written; and with no key in
	// the history — every other turn of every other session — it changes
	// nothing at all, however many keys it covers, so the request's bytes,
	// and the provider's prefix cache, are what they would have been.
	msgs, results := s.store.ContextWithResults(m.id())
	history := redactHistory(s.redactor(), msgs, results)
	// A prompt still held is an earlier turn's that produced nothing. It is
	// not in history, so this turn's request never sent it; written ahead of
	// this turn's answer, it would put in the transcript what the model never
	// saw.
	s.store.DiscardHeldUsers()
	user.Message = fantasy.NewUserMessage(text) // byte for byte what Fantasy sends for Prompt
	if err := s.store.AppendUser(user); err != nil {
		return Result{}, fmt.Errorf("harness: %w", s.tools.redactErr(err))
	}

	t := &turn{
		ctx:     turnCtx,
		store:   s.store,
		model:   m,
		sink:    sink,
		number:  number,
		calls:   calls{tools: s.tools},
		steers:  &s.steers,
		modes:   s.modes,
		logMode: s.recordMode,
		subs:    s.subs,
		wake:    wake,
		held:    held,
	}
	t.resetCalls(true) // Fantasy opens every step with OnStepStart; this is a defence
	// The session's todo store reaches this turn's sink only through here
	// (todos.go, plan 023 §3.4): attach for the turn's whole life, detached
	// once Run returns, the same way modes and the steer box are handed a
	// fixed reference at the start rather than looked up each time. A
	// sub-agent has no todo list (plan 026 §3.2).
	if s.tools.todos != nil {
		defer s.tools.todos.attach(t.emitLocked)()
	}
	// And the session's asker tells this turn, and no other, of a plan the
	// person approved (asker.go).
	if s.tools.asker != nil {
		defer s.tools.asker.attach(t.planWasApproved)()
	}
	// And the session's sub-agent runner reports to this turn, and resolves a
	// child's model against this turn's, for as long as it runs (plan 026
	// §3.6, §3.9): a call's children report their lifecycle through the
	// turn's lock and their own events straight to its sink, and one made
	// after a SetModel mid-turn still starts from the model the turn — and
	// so the call — runs on, not the session's current one (panel CodeRabbit
	// 12). Detached once Run returns, by which time every call, and so every
	// child, has.
	if s.subs != nil {
		defer s.subs.attach(t)()
	}
	// From here Steer is accepted, and only from here: a prompt the store
	// refused above never became a turn, so there was nothing to steer into.
	t.steers.begin()
	agent := s.newAgent(m.lm, s.system, t.agentTools())
	res, err := agent.Stream(turnCtx, t.call(text, history))
	return t.finish(res, err)
}

// begin claims the session for one turn: it refuses when closed or busy,
// fixes the model the turn runs on — and the redactor, taking up one a
// switch resolved while the turn before it ran (toolset.adopt), so a turn
// redacts everything it reports and persists with the same one — works out
// which switches the transcript has not been told about, numbers the turn
// (its tool calls' ids start with it), and registers the turn's cancel func
// before the lock is released, so a Close from then on finds it (crush's
// order).
func (s *Session) begin(ctx context.Context) (model, []func(*store.Store) error, context.Context, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.closed:
		return model{}, nil, nil, 0, ErrClosed
	case s.running:
		return model{}, nil, nil, 0, ErrInTurn
	}
	m := s.cur
	// A switch and a switch back before this turn leave nothing to record;
	// several switches record only the last. A change held for a turn that
	// then produced nothing stays held in the store, and a later change of
	// the same kind replaces it there.
	var changes []func(*store.Store) error
	if id := m.id(); id != s.logged.model {
		changes = append(changes, func(st *store.Store) error { return st.AppendModelChange(id) })
	}
	if effort := m.effort; effort != s.logged.effort {
		changes = append(changes, func(st *store.Store) error { return st.AppendEffortChange(effort) })
	}

	// No turn is running here, so this is the one point at which changing the
	// session's redactor cannot cut across a step.
	s.tools.adopt()

	// A cancel cause, so Close can tell a tool the session is closing
	// (tool.ErrClosing) rather than that the user stopped the turn.
	turnCtx, cancel := context.WithCancelCause(ctx)
	s.turns++
	s.running, s.begun, s.cancel, s.done = true, true, cancel, make(chan struct{})
	return m, changes, turnCtx, s.turns, nil
}

// record hands the turn's switches to the store and, only once all of them
// are there, marks m — the turn's model and effort, not whatever a SetModel
// since begin made current — as what the transcript was last told. A switch
// the store refused stays unlogged, so the next turn hands it over again.
func (s *Session) record(m model, changes []func(*store.Store) error) error {
	for _, c := range changes {
		if err := c(s.store); err != nil {
			return fmt.Errorf("harness: %w", s.tools.redactErr(err))
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// The mode is left alone: it is recorded at the step boundary that tells
	// the model about it, which is not this one (recordMode).
	s.logged.model, s.logged.effort = m.id(), m.effort
	return nil
}

// recordMode hands the store a mode_change when mode is not what the
// transcript was last told, and marks it held once the store has taken it —
// held or written, which is what logged means for the model and the effort
// too. The turn calls it as it appends the step whose request announced the
// mode, so the entry is held with that step's output and lands where the
// change became visible in the conversation (plan 023 §3.1). A store that
// refuses it stays untold, as a refused model change does.
func (s *Session) recordMode(mode string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.logged.mode == mode {
		return nil
	}
	if err := s.store.AppendModeChange(mode); err != nil {
		return fmt.Errorf("harness: %w", s.tools.redactErr(err))
	}
	s.logged.mode = mode
	return nil
}

// end releases the session after a turn and wakes a Close waiting on it. The
// steer box is shut here too: the turn shuts it as it finishes (settleSteers),
// and this catches a Run that returned before it ever opened one.
func (s *Session) end() {
	s.steers.close()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cancel(nil) // releases the turn context's resources
	close(s.done)
	s.running, s.cancel, s.done = false, nil, nil
}

// turn is one Run's state.
//
// Fantasy calls the stream callbacks, OnToolCall and OnStepFinish on the
// goroutine that called Stream, but it runs tools — and calls OnToolResult —
// on goroutines of its own, up to five at once, and a tool's progress
// arrives on the dispatcher's (plan 019 §2.4). So everything a callback
// touches is guarded by mu, and the sink is called only while mu is held:
// the sink keeps H1's single-caller contract. Steps do not overlap: Fantasy
// waits for every tool before it finishes a step.
type turn struct {
	ctx    context.Context // the turn's: its cancel is the turn's
	store  *store.Store
	model  model
	sink   func(Event)
	number int // the turn's number in the session, from 1

	mu sync.Mutex
	// ended is set as Run returns: nothing reaches the sink after it, not
	// even a progress snapshot a tool goroutine delivers late.
	ended bool

	// The deltas streamed since the step began: what the user has seen of an
	// answer that may never finish.
	text, reasoning strings.Builder

	step       int           // the step streaming or last finished, from 1
	stepStart  time.Time     // when its request went out (a retry's, after the backoff)
	firstToken time.Duration // from stepStart to its first text, thinking or tool call; 0 before
	retries    int           // the step's retries so far

	stepped bool   // a step finished
	stop    string // that step's stop reason
	usage   Usage  // that step's usage
	saveErr error  // the first failed append of a finished step
	badIDs  bool   // a step's tool calls had an unusable provider id (ErrBadToolCalls)

	calls // this step's tool calls (toolbridge.go)

	loop doomLoop // the doom-loop guard's count, across the turn's steps (doomloop.go)

	// planApproved is set when the person approved the plan exit_plan_mode
	// presented (planWasApproved): the turn ends with that step, as end_turn,
	// and every call the model placed after the asking one in the step is refused
	// (plan 023 §3.4, D-51).
	planApproved bool
	approvedAt   int // the asking call's place in its step (toolCall.order)

	// Interject's steers, spliced into every step's messages from the one that
	// first saw them (steer.go, plan 019 §3.10). steers is the session's box,
	// which has its own lock; spliced and written are this turn's, under mu.
	steers  *steerbox
	spliced []splice // the steers taken up, in order, each at a fixed index
	written int      // how many of spliced an AppendStep has written

	// The background sub-agents' results this turn delivers (delivery.go,
	// plan 026 §3.11): subs is the session's runner, nil for a sub-agent's;
	// wake says the turn is a wake, whose prompt is results; internal are the
	// parts of results its steps took up, a collection of their own and never
	// the steer box's; held are the results a wake was started with, which
	// its user entry carries, and heldWritten says an append has written it.
	// Under mu, but for the two fixed ones.
	subs        *subagents
	wake        bool
	internal    []internalPart
	held        []string
	heldWritten bool

	// The mode's reminders (reminders.go, plan 023 §3.3): modes is the
	// session's box, which has its own lock; the rest is this turn's, under
	// mu. reminders is a collection of its own — never spliced, never
	// persisted, never emitted — and pending is the one composed for the step
	// about to go out.
	//
	// carried and sent are the two halves of announcing a mode. carried is the
	// mode this turn's reminders already speak for, from the moment one is
	// composed: every later request of the turn carries it, so the turn never
	// composes it twice. sent is that mode once a request carrying it has
	// really gone out, and it is what the step that persists output records —
	// in the transcript (modeChangeHeld) and as what the model has been told
	// (modeHeard).
	modes      *modes
	logMode    func(mode string) error
	reminders  []reminder
	pending    pendingReminder
	hasPending bool
	carried    string
	sent       string
}

// redactor is the session's, as it is now — fixed for the whole turn, since
// only a turn's start adopts a new one (toolset.adopt): everything the turn
// reports and writes down is redacted with the same one. The history it
// replays is redacted with a superset of it, the session's live union (run).
func (t *turn) redactor() *redact.Replacer { return t.tools.redactor() }

// emit hands ev to the sink unless the turn has ended. mu is held.
func (t *turn) emit(ev Event) {
	if !t.ended {
		t.sink(ev)
	}
}

// emitLocked is emit for a caller outside toolbridge.go's callbacks that
// does not already hold mu: the session's todo store (todos.go), whose own
// lock wraps a call to this one so that a mutation and its event are one
// critical section from the store's side too (plan 023 §3.4). It takes and
// releases mu itself, so a caller must not already hold it.
func (t *turn) emitLocked(ev Event) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.emit(ev)
}

// call is the turn's request. MaxOutputTokens, effort and retries are per
// call; the system prompt and the tools are the agent's.
func (t *turn) call(text string, history []fantasy.Message) fantasy.AgentStreamCall {
	c := fantasy.AgentStreamCall{
		Prompt:          text,
		Messages:        history,
		ProviderOptions: t.model.effortOpts,
		StopWhen:        []fantasy.StopCondition{fantasy.StepCountIs(maxSteps), t.halted},

		PrepareStep: t.prepareStep,
		OnStepStart: t.stepStarted,
		OnTextDelta: func(_, delta string) error {
			t.mu.Lock()
			defer t.mu.Unlock()
			t.firstOutput()
			t.text.WriteString(delta)
			t.emit(TextDelta{Text: delta})
			return nil
		},
		// A reasoning block's first part may already carry text.
		OnReasoningStart: func(_ string, r fantasy.ReasoningContent) error {
			t.mu.Lock()
			defer t.mu.Unlock()
			t.firstOutput()
			if r.Text != "" {
				t.reasoning.WriteString(r.Text)
				t.emit(ThoughtDelta{Text: r.Text})
			}
			return nil
		},
		OnReasoningDelta: func(_, delta string) error {
			t.mu.Lock()
			defer t.mu.Unlock()
			t.firstOutput()
			t.reasoning.WriteString(delta)
			t.emit(ThoughtDelta{Text: delta})
			return nil
		},
		// Every callback returns nil: an error from OnToolCall leaks
		// Fantasy's tool coordinator (agent.go:1744, 1758), and the others'
		// would fail the turn over what is only a report.
		OnToolInputStart: t.toolInputStart,
		OnToolCall:       t.toolCall,
		OnToolResult:     t.toolResult,
		OnRetry:          t.retry,
		OnStepFinish:     t.stepFinished,
	}
	if n := t.model.r.MaxOutputTokens; n > 0 {
		ceiling := int64(n)
		c.MaxOutputTokens = &ceiling
	}
	return c
}

// halted is the turn's own stop condition, checked after every step
// alongside the step limit: a save failed (Fantasy ignores OnStepFinish's
// error, so without it the turn would go on paying for steps, and a tool
// changing files, that the transcript cannot hold), a step's call ids were
// unusable, the doom-loop guard refused a fifth identical call (plan 019
// §3.7), or the turn was cancelled — a cancel during tools lets their step
// finish, and no request may follow it.
//
// The guard's step ends with tool results the model never gets to read, so
// finish turns its tool_use into max_turn_requests, as it does for the step
// limit.
//
// An approved plan stops the turn here too (plan 023 §3.4): Fantasy joins
// every tool of the step, builds the step and runs OnStepFinish before it
// asks this, so by now the approval is recorded, the step — exit_plan_mode's
// call and its result — is persisted, and the only thing left to prevent is
// the next request. It is not done through a result's StopTurn, which the
// dispatcher clears on anything a tool returns and which would leave the
// step's tool_use to be read as a turn that ran out of requests.
func (t *turn) halted([]fantasy.StepResult) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.saveErr != nil || t.badIDs || t.loop.stopped || t.planApproved || t.ctx.Err() != nil
}

// planWasApproved records that the person approved the plan. The session's
// asker calls it, on the tool goroutine that asked, before exit_plan_mode has
// its answer (asker.go).
//
// It also fixes which call asked, as a place in the step: the last of the
// calls running now. The asking call is one of them, and it is not Parallel,
// so Fantasy dispatches nothing after it until it returns — whatever else is
// running was placed before it. Every call placed after it is refused
// (runTool).
func (t *turn) planWasApproved() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.planApproved {
		return
	}
	t.planApproved = true
	for _, c := range t.list {
		if c.running && c.order > t.approvedAt {
			t.approvedAt = c.order
		}
	}
}

// stepStarted opens step n (from 0): its number, its clock, and an empty
// set of tool calls. The step's request goes out next, so this is also where
// the reminder prepareStep composed for it becomes one the model will read
// (reminderSent) — for a turn whose context is still live, since Fantasy
// opens a step whether or not the request can be sent.
func (t *turn) stepStarted(n int) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.step = n + 1
	t.stepStart, t.firstToken, t.retries = time.Now(), 0, 0
	t.resetCalls(true)
	t.reminderSent()
	return nil
}

// firstOutput stamps the step's time to first token. mu is held.
func (t *turn) firstOutput() {
	if t.firstToken == 0 && !t.stepStart.IsZero() {
		t.firstToken = max(time.Since(t.stepStart), time.Nanosecond)
	}
}

// retry is OnRetry. A retry replays the step from the start and every
// callback fires again (plan 018 §2.4): what the failed attempt streamed is
// not part of the answer, and a call it began never arrives.
func (t *turn) retry(err *fantasy.ProviderError, delay time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.resetDeltas()
	t.settleCalls(incomplete)
	t.resetCalls(false)
	t.retries++
	t.stepStart, t.firstToken = time.Now().Add(delay), 0
	t.emit(Retrying{Delay: delay, Attempt: t.retries, Reason: t.redactor().String(retryReason(err))})
}

// retryReason is a retried failure on one line.
func retryReason(err *fantasy.ProviderError) string {
	if err == nil {
		return "the request failed before the provider answered"
	}
	msg := err.Message
	if msg == "" {
		msg = err.Title
	}
	msg = oneLine(msg, maxMessageBytes)
	switch {
	case err.StatusCode == 0:
		return msg
	case msg == "":
		return "HTTP " + strconv.Itoa(err.StatusCode)
	}
	return "HTTP " + strconv.Itoa(err.StatusCode) + ": " + msg
}

// stepFinished persists a finished step (the store writes the held user
// entries with the first) and forgets the deltas, which the step now holds;
// see Run. Fantasy ignores a callback's error here, so a failed append is
// kept for halted and Run.
func (t *turn) stepFinished(step fantasy.StepResult) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	text, reasoning := t.text.String(), t.reasoning.String()
	t.resetDeltas()
	t.stepped = true

	assistant, ok := assistantOf(step.Messages)
	if (!ok || !hasText(assistant)) && text != "" {
		assistant, ok = withStreamed(assistant, reasoning, text), true
	}
	open := openCalls(assistant)
	t.stop = stepStop(step.FinishReason, len(open))
	t.usage = *store.UsageOf(step.Usage)
	done := t.stepDone(step)
	// What the step's sub-agents spent, a row per model (plan 026 §3.7): on
	// the step's tool entry below, and on its StepDone whatever becomes of the
	// append — a step refused for its ids or whose save fails is persisted
	// nowhere, and the report is then the only record (panel P40). A
	// background result an agent_output call read is not among them: it is
	// given back unless this append writes the call's result, and would then
	// be counted again by whatever delivers it (§3.11).
	done.SubagentUsage = subagentUsage(t.list, nil)

	results, bad, answered := t.stepResults(step, open)
	if bad {
		t.badIDs = true
		t.emit(Diag{Kind: DiagBadToolCalls, Fields: map[string]string{
			"step": strconv.Itoa(t.step), "calls": strconv.Itoa(len(open)),
		}})
		t.emit(done)
		return nil
	}
	if ok {
		entry := store.MessageEntry{
			Message:    redactCalls(t.redactor(), assistant),
			Model:      t.model.id(),
			Effort:     t.model.effort,
			Usage:      store.UsageOf(step.Usage),
			StopReason: t.stop,
		}
		var toolEntry *store.MessageEntry
		todoMark := 0
		if results != nil {
			// The children's usage rides on the entry that holds their results,
			// each row priced by its own model; the entry itself is stamped with
			// the parent's model and carries no plain usage, which that model's
			// rate would price (plan 026 §3.7). A background result's rides on
			// it when an agent_output call's own result here delivers it
			// (§3.11).
			writes := func(c *toolCall) bool { return slices.Contains(answered, c) }
			toolEntry = &store.MessageEntry{Message: redactResults(t.redactor(), *results), Model: t.model.id(), Effort: t.model.effort,
				SubagentUsage: subagentUsage(t.list, writes)}
			todoMark = t.todosOn(toolEntry)
		}
		// The mode this step's request announced is handed over first, so the
		// store writes the mode_change ahead of this step's own entries
		// (reminders.go), and it is only ever handed over for a step there is
		// something to append with.
		t.modeChangeHeld()
		// The steers no step has written yet lead the append: this is the
		// first step that could write them, and the transcript then holds
		// them exactly where this step's request had them — and so do the
		// background results the step's requests carried (§3.11).
		leading, lead := t.leadingEntries()
		ids, err := t.store.AppendStep(leading, entry, toolEntry)
		switch {
		case err == nil:
			t.written = len(t.spliced)
			t.wrote(ids, lead, toolEntry != nil, outputCalls(answered))
			t.todosWritten(todoMark)
			done.Saved, done.Entries = true, ids
			// The conversation now holds the step the model read the notice
			// in, so the mode is told for good (plan 023 §3.3).
			t.modeHeard()
		case errors.Is(err, store.ErrNoOutput): // thinking alone: nothing to persist
		default:
			if t.saveErr == nil {
				t.saveErr = err
			}
			done.SaveError = t.redactor().String(err.Error())
			t.emit(Diag{Kind: DiagSaveFailed, Fields: map[string]string{"step": strconv.Itoa(t.step), "error": done.SaveError}})
		}
	}
	t.emit(done)
	return nil
}

// stepDone is the StepDone for step, with its persistence outcome still to
// fill in. mu is held.
func (t *turn) stepDone(step fantasy.StepResult) StepDone {
	id := t.model.id()
	raw, ok := llm.RawFinish(step.ProviderMetadata)
	if !ok {
		raw = step.FinishReason // nothing normalized it
	}
	return StepDone{
		Step:             t.step,
		Provider:         id.Provider,
		Model:            id.Alias,
		WireModel:        id.WireModel,
		Finish:           string(step.FinishReason),
		FinishRaw:        string(raw),
		StopReason:       t.stop,
		TimeToFirstToken: t.firstToken,
		Usage:            t.usage,
	}
}

// todosOn puts on e, a step's tool entry about to be appended, the session's
// todo list when a call changed it since a written tool entry last carried it
// — the list after the step: Fantasy has joined every call of the step, the
// parallel todo_write calls among them, before the step finishes (plan 028
// §3.2, P7). It returns the mark todosWritten commits once e is written; 0
// when nothing was put on it, and for a sub-agent, which has no list. mu is
// held; the list's record has a leaf lock of its own (todos.go).
func (t *turn) todosOn(e *store.MessageEntry) int {
	if t.tools.todos == nil {
		return 0
	}
	list, mark, ok := t.tools.todos.unrecorded()
	if !ok {
		return 0
	}
	e.Todos = &list
	return mark
}

// todosWritten records that the tool entry todosOn put the list on at mark
// has been written. mu is held.
func (t *turn) todosWritten(mark int) {
	if mark > 0 {
		t.tools.todos.recordedAt(mark)
	}
}

// finish is how Run ends once Fantasy has returned: the turn's steers are
// settled, the step a cancel or a failure cut short is persisted as far as it
// streamed, every tool call still open is settled, and the result is worked
// out (see Run).
func (t *turn) finish(res *fantasy.AgentResult, err error) (out Result, ferr error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	defer func() { t.ended = true }()
	// The steers are settled before anything else, and their text is put on
	// the Result by a defer rather than by each return below: a path that
	// forgot it would drop text the user typed. settleSteers shuts the box, so
	// from here no accept can land in a turn that has already reported.
	unanswered := t.settleSteers()
	defer func() { out.Unanswered = unanswered }()
	cancelled := t.ctx.Err() != nil

	if err == nil {
		// Fantasy finishes every step it starts, so there is nothing to
		// settle; this is a defence, not a path.
		t.settleCalls(incomplete)
		switch {
		case t.saveErr != nil:
			return Result{}, fmt.Errorf("harness: saving the answer: %w", t.tools.redactErr(t.saveErr))
		case t.badIDs:
			return Result{}, badToolCalls(t.model.id())
		}
		stop := StopEndTurn
		if t.stepped {
			stop = t.stop
		}
		if stop == StopToolUse || t.loop.stopped {
			// The model had results to read and the turn stopped anyway: the
			// user did, the step limit did, or the doom-loop guard did. The
			// guard is asked separately because a call it refused can arrive
			// under a finish that leaves its step's own stop reason end_turn
			// — Fantasy announces a call on any finish but the abnormal four,
			// and dispatches it only on "tool-calls" (plan 019 §3.7).
			if cancelled {
				return Result{StopReason: StopCancelled}, nil
			}
			stop = StopMaxTurnRequests
			// Unless what stopped it was the person approving the plan: that
			// turn is finished, not cut off, and `craze prompt` exits non-zero
			// on anything but end_turn. The transcript's step keeps its own
			// tool_use. A cancel (above), a save failure and unusable ids
			// (earlier) and the doom-loop guard all outrank it (plan 023 §3.4).
			if t.planApproved && !t.loop.stopped {
				stop = StopEndTurn
			}
		}
		var total Usage
		if res != nil {
			total = *store.UsageOf(res.TotalUsage)
		}
		return Result{StopReason: stop, Usage: total}, nil
	}

	// Fantasy discards everything on a cancel or a failure (plan 018 §2.4);
	// only what this runner kept says what the user saw. A context
	// cancelled for any reason is a cancel, whatever error it surfaced as.
	partial := t.text.Len() > 0 || t.reasoning.Len() > 0 || t.announced() > 0
	saveErr := t.saveInterrupted(cancelled)
	why := incomplete
	if cancelled {
		why = aborted
	}
	t.settleCalls(why)
	switch {
	case cancelled && saveErr != nil:
		return Result{}, fmt.Errorf("harness: saving the interrupted answer: %w", t.tools.redactErr(saveErr))
	case cancelled && t.stepped && !partial && t.stop != StopToolUse:
		// The model's last step was final and is persisted: the cancel came
		// too late to stop anything.
		return Result{StopReason: t.stop, Usage: t.usage}, nil
	case cancelled:
		return Result{StopReason: StopCancelled}, nil
	case saveErr != nil:
		return Result{}, errors.Join(classify(err, t.model.id()),
			fmt.Errorf("harness: saving the interrupted answer: %w", t.tools.redactErr(saveErr)))
	default:
		return Result{}, classify(err, t.model.id())
	}
}

// saveInterrupted appends the step a cancel or a failure cut short, marked
// interrupted, with the stop reason "cancelled" for a cancel and none for a
// failure: the tool calls it announced with their results, when there were
// any (synthesizeStep), otherwise the answer the deltas hold. Thinking with
// no text and no call is not persisted (the store's ErrNoOutput), and
// neither is nothing. mu is held.
func (t *turn) saveInterrupted(cancelled bool) error {
	stop := ""
	if cancelled {
		stop = StopCancelled
	}
	if done, err := t.synthesizeStep(stop); done {
		return err
	}
	if t.text.Len() == 0 && t.reasoning.Len() == 0 {
		return nil
	}
	// The background results the cut step's request carried lead the answer,
	// as they would a finished step's, and commit when it is written (plan 026
	// §3.11); its steers do not, and go back to the user.
	leading, lead := t.internalEntries()
	ids, err := t.store.AppendAnswer(leading, store.MessageEntry{
		Message:     streamed(t.reasoning.String(), t.text.String()),
		Model:       t.model.id(),
		Effort:      t.model.effort,
		StopReason:  stop,
		Interrupted: true,
	})
	if err == nil {
		t.wrote(ids, lead, false, nil)
	}
	if errors.Is(err, store.ErrNoOutput) {
		return nil
	}
	return err
}

func (t *turn) resetDeltas() {
	t.text.Reset()
	t.reasoning.Reset()
}

// stepStop is a finished step's stop reason: "length" is max_tokens and
// "content-filter" is refusal; a "tool-calls" finish with calls to answer is
// tool_use, the turn going on; every other finish, "error" and "unknown"
// included, is a turn the model ended.
func stepStop(r fantasy.FinishReason, calls int) string {
	switch {
	case r == fantasy.FinishReasonLength:
		return StopMaxTokens
	case r == fantasy.FinishReasonContentFilter:
		return StopRefusal
	case r == fantasy.FinishReasonToolCalls && calls > 0:
		return StopToolUse
	}
	return StopEndTurn
}

// streamed is an assistant message built from streamed deltas: the
// reasoning, when there was any, then the text.
func streamed(reasoning, text string) fantasy.Message {
	var parts []fantasy.MessagePart
	if reasoning != "" {
		parts = append(parts, fantasy.ReasoningPart{Text: reasoning})
	}
	parts = append(parts, fantasy.TextPart{Text: text})
	return fantasy.Message{Role: fantasy.MessageRoleAssistant, Content: parts}
}

// withStreamed is m, the step's assistant message (or the zero Message), with
// its text and reasoning replaced by the streamed deltas' — text the user
// saw that Fantasy never committed — and its other parts, the tool calls
// among them, kept after them in order.
func withStreamed(m fantasy.Message, reasoning, text string) fantasy.Message {
	out := streamed(reasoning, text)
	for _, p := range m.Content {
		switch p.GetType() {
		case fantasy.ContentTypeText, fantasy.ContentTypeReasoning:
		default:
			out.Content = append(out.Content, p)
		}
	}
	return out
}

// hasText reports whether m has a text part with something besides
// whitespace in it: the store's test for an answer worth persisting.
func hasText(m fantasy.Message) bool {
	for _, p := range m.Content {
		if t, ok := fantasy.AsMessagePart[fantasy.TextPart](p); ok && strings.TrimSpace(t.Text) != "" {
			return true
		}
	}
	return false
}

// assistantOf is the assistant message of a step's messages, whole: its
// text, reasoning and tool calls, as Fantasy recorded them.
func assistantOf(msgs []fantasy.Message) (fantasy.Message, bool) {
	for _, m := range msgs {
		if m.Role == fantasy.MessageRoleAssistant {
			return m, true
		}
	}
	return fantasy.Message{}, false
}

// toolMessageOf is the tool message of a step's messages, holding the
// results of the calls Fantasy ran, or nil.
func toolMessageOf(msgs []fantasy.Message) *fantasy.Message {
	for i := range msgs {
		if msgs[i].Role == fantasy.MessageRoleTool {
			return &msgs[i]
		}
	}
	return nil
}

// openCalls are the tool calls in m the next message must answer: all but
// the ones the provider executed itself, which carry their own results.
func openCalls(m fantasy.Message) []fantasy.ToolCallPart {
	var calls []fantasy.ToolCallPart
	for _, p := range m.Content {
		if c, ok := fantasy.AsMessagePart[fantasy.ToolCallPart](p); ok && !c.ProviderExecuted {
			calls = append(calls, c)
		}
	}
	return calls
}

// redactCalls is m with every provider key redacted from its tool calls'
// arguments: the model's own output, but what the transcript keeps must not
// hold a key verbatim (plan 019 §3.8). A message holding none is returned as
// it is, so its bytes do not change.
//
// Only the stored copy is redacted. Fantasy keeps the step's own messages
// and sends them again at the turn's next step (agent.go:943-944,
// 1072-1074), so an argument the model wrote goes back to the model as the
// model wrote it, and a later turn, replaying the transcript, sends the
// marker in its place. That is deliberate: what returns within the turn is
// what the model itself sent, so no key can be learned from it, while
// nothing craze keeps holds one. (The canary test checks what the model is
// sent: every tool-role message of every request.)
func redactCalls(red *redact.Replacer, m fantasy.Message) fantasy.Message {
	return mapParts(m, func(p fantasy.MessagePart) (fantasy.MessagePart, bool) {
		if c, ok := fantasy.AsMessagePart[fantasy.ToolCallPart](p); ok {
			if in := red.String(c.Input); in != c.Input {
				c.Input = in
				return c, true
			}
		}
		return p, false
	})
}

// redactResults is m with every provider key redacted from its results'
// text. The results a tool produced are redacted already; the ones Fantasy
// wrote itself — its refusal of an invalid call — are not. A message holding
// none is returned as it is.
func redactResults(red *redact.Replacer, m fantasy.Message) fantasy.Message {
	return mapParts(m, func(p fantasy.MessagePart) (fantasy.MessagePart, bool) {
		r, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](p)
		if !ok {
			return p, false
		}
		if o, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentText](r.Output); ok {
			if s := red.String(o.Text); s != o.Text {
				r.Output = fantasy.ToolResultOutputContentText{Text: s}
				return r, true
			}
		}
		if o, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentError](r.Output); ok && o.Error != nil {
			if s := red.String(o.Error.Error()); s != o.Error.Error() {
				r.Output = fantasy.ToolResultOutputContentError{Error: errors.New(s)}
				return r, true
			}
		}
		return p, false
	})
}

// redactHistory is msgs with every provider key gone from the tool calls'
// arguments and the tool results' text (see Run), and from the text of every
// message results marks as an entry of background sub-agents' results (plan
// 026 §3.11, astra r14): a child wrote it, as a tool wrote a result, so a key
// the session learned after it was committed must not go out in a later
// request. results is msgs' marks, message for message
// (store.ContextWithResults). A message holding none is carried over as it
// is, parts and all, so the bytes a provider sees do not change — a person's
// prompt among them, which is never marked; msgs itself, which the store
// owns, is never written to.
func redactHistory(red *redact.Replacer, msgs []fantasy.Message, results []bool) []fantasy.Message {
	out := slices.Clone(msgs)
	for i, m := range out {
		m = redactResults(red, redactCalls(red, m))
		if i < len(results) && results[i] {
			m = redactText(red, m)
		}
		out[i] = m
	}
	return out
}

// redactText is m with every provider key redacted from its text parts; m
// itself when they hold none.
func redactText(red *redact.Replacer, m fantasy.Message) fantasy.Message {
	return mapParts(m, func(p fantasy.MessagePart) (fantasy.MessagePart, bool) {
		if tp, ok := fantasy.AsMessagePart[fantasy.TextPart](p); ok {
			if s := red.String(tp.Text); s != tp.Text {
				tp.Text = s
				return tp, true
			}
		}
		return p, false
	})
}

// mapParts is m with f applied to each part, f reporting whether it changed
// one; m itself, its parts untouched, when f changes none. (Parts are not
// compared: some hold maps, and an interface holding one panics on ==.)
func mapParts(m fantasy.Message, f func(fantasy.MessagePart) (fantasy.MessagePart, bool)) fantasy.Message {
	var out []fantasy.MessagePart
	for i, p := range m.Content {
		q, changed := f(p)
		if out == nil && !changed {
			continue
		}
		if out == nil {
			out = append(make([]fantasy.MessagePart, 0, len(m.Content)), m.Content[:i]...)
		}
		out = append(out, q)
	}
	if out == nil {
		return m
	}
	m.Content = out
	return m
}
