package harness

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"uuid"

	"github.com/charliek/craze/internal/harness/redact"
	"github.com/charliek/craze/internal/harness/tool"
)

// The sub-agent runner (plan 026 §3.1, §3.7–§3.9). A session that is not
// itself a sub-agent has one (Session.subs), and its agent tool hands every
// call to it (tool.Env.Subagents): the runner resolves the call's type, model
// and effort, takes one of the session's four slots, opens a child session
// for it (Options.Child, child.go), runs the child's one turn on the call's
// own goroutine, closes it, and answers the call with what the parent's model
// reads (§3.7). A call blocks until its child has ended, so nothing of a child
// outlives its call, and the turn — which joins its calls — outlives them all.
//
// # The registry and its lock
//
// live holds the children that are registered: running, or on their way to
// it. regMu guards it, sealed and nothing else. It is a leaf: it is never held
// across a child's Open, Run or Close, nor across a sink call; the only lock
// taken under it is the modes box's, to read or set the mode (s.mu → regMu →
// modes.mu for SetMode, regMu → modes.mu for a registration; never the other
// way round).
//
// # A slot's life (pinned; panel P2, P36)
//
// Every step that can fail comes after the step that undoes it is deferred,
// so a failed Open, a panic the dispatcher recovers outside the runner
// (dispatch.go's run) — one unwinding through the child's own Run is the
// runner's to recover (runTurn) — a cancel and a Close each give back exactly
// what they took:
//
//  1. the slot is acquired by a select on the semaphore, the call's context
//     and the session's closing channel; one won while either of the other two
//     is also ready is given back, and the call is aborted;
//  2. its release is deferred at once;
//  3. the context and closing are checked again;
//  4. the child is registered — refused, and the call aborted, once Close has
//     sealed the registry — and its retirement deferred, before anything
//     fallible;
//  5. the child is opened, and its Close deferred at once, so it runs before
//     the retirement and the release: a child that persisted a step and then
//     panicked still has its transcript closed;
//  6. the opened child is attached to its handle, which checks the handle's
//     closing latch: a Close that signalled while the child was opening is
//     honoured by the child it could not yet reach.
//
// No event is emitted while a call waits for a slot: its row is the call's
// own, running, and no sub-agent exists yet.
//
// # Events and locks
//
// SubagentStarted and SubagentFinished are the parent turn's own events,
// emitted on the call's goroutine through the turn's lock like every other
// event of its. A child's own events arrive at the runner under the child
// turn's lock and go on, wrapped in SubagentEvent, straight to the parent's
// sink, never through the parent turn's lock: nothing on the parent's side
// waits on a child while holding its turn's or its session's lock (§3.9).
// Started comes before every event of the child, since the child runs only
// after it; every event of the child before Finished, since the child's turn
// has ended — and drops even a late progress snapshot — before its Run
// returns; and Finished before the parent's ToolFinished for the call, which
// follows the call's return.

// maxChildren is how many sub-agents one session runs at once (owner decision
// 7): a fifth call waits for a slot. Fantasy's own five parallel tool slots
// sit above it, so which four of five calls run first is Fantasy's scheduling
// and unordered (§3.8).
const maxChildren = 4

// errStoppedByUser is the cause a child's context is cancelled with when the
// user stops that one child (plan 026 §3.10: PR 2's CancelSubagent). A parent
// cancel or close outranks it (subagentOutcome), and on its own it is a stop,
// not a failure: the parent's model reads what the child had got to.
var errStoppedByUser = errors.New("harness: the user stopped this sub-agent")

// The texts the parent's model reads for a child that did not simply answer
// (§3.7's table). They are craze's own words, fixed by the plan.
const (
	subagentNoMessage  = "The sub-agent finished without a final message."
	subagentCutOff     = "(The sub-agent's output was cut off.)"
	subagentRefused    = "(The sub-agent's model refused to continue.)"
	subagentLooped     = "(The sub-agent was stopped for repeating the same tool call.)"
	subagentStepLimit  = "(The sub-agent reached its step limit.)"
	subagentFailed     = "The sub-agent failed: "
	subagentLastOutput = "Its last output was:\n"
	subagentStopped    = "The user stopped this sub-agent before it finished."
	// subagentPanicked is the Error of the SubagentFinished a child's panic
	// leaves behind. The call's result is the runner's own failure, which
	// names the panic's value (childPanic; review r3): the event's error is a
	// row's label, and a panic's value can be any size.
	subagentPanicked = "the sub-agent panicked"
	// subagentNested refuses an agent call inside a sub-agent: depth is 1
	// (§3.2). A child is never offered the tool, so this is the guard behind
	// that structure, not a path.
	subagentNested = "Sub-agents cannot start sub-agents of their own."
	// subagentNoTurn answers a call that reached a runner no turn is attached
	// to: the agent tool runs only inside a turn, so this is a defence.
	subagentNoTurn = "internal error: the agent call arrived while no turn was running."
)

// subagents is a session's runner. See the file's comment.
type subagents struct {
	s     *Session      // the parent: this runner's session, fixed
	slots chan struct{} // the cap: a slot is a value in it, taken by a send
	// turn is the running turn, attached for its life (attach): the sink and
	// the model a call's children report to and start from.
	turn atomic.Pointer[turnLink]

	regMu  sync.Mutex
	sealed bool                    // Close has begun: no child registers from here
	live   map[string]*childHandle // the registered children, by id

	// seams are the runner's test seams, set before the first turn; zero is
	// production.
	seams subagentSeams
}

// subagentSeams are the points a test needs to reach inside a call: to fail
// the child's Open, to know a call is waiting for a slot or holds one, and to
// see the child session a call opened.
type subagentSeams struct {
	open     func(Options) (*Session, error) // opens a child; nil is Open
	waiting  func(tool.SubagentCall)         // the call found no free slot and is about to wait for one
	acquired func(tool.SubagentCall)         // the call holds a slot, rechecked, and is about to register
	opened   func(id string, child *Session) // the child opened and is attached, before it runs
}

// turnLink is what a call needs of the turn it runs in: the turn's locked
// emit, for the lifecycle events, its sink itself, for the children's own
// events, and its model, which a child's model and effort resolve against
// (§3.6: the running turn's, not the session's current one).
type turnLink struct {
	emit  func(Event)
	sink  func(Event)
	model model
}

// childHandle is one registered child: what Close and SetMode reach it by.
type childHandle struct {
	id string
	// strictness is the child's monotonic strictness (ChildOptions.Strictness):
	// set from the mode it registered in, raised by the parent's SetMode under
	// regMu (tool.Raise), and read by the child's gate at every check.
	strictness atomic.Int32
	// cancel is the child's context's: the runner's own, derived from the
	// call's. A close cancels it with tool.ErrClosing; PR 2's per-child stop
	// cancels it with errStoppedByUser.
	cancel context.CancelCauseFunc

	// mu guards the closing latch and the opened session, which Close's
	// signal and the runner's attach set from two goroutines.
	mu      sync.Mutex
	closing bool     // the latch: a Close has signalled this child, opened or not
	sess    *Session // the child, once opened and attached; nil before
}

// newSubagents is s's runner, with every slot free and nothing registered.
func newSubagents(s *Session) *subagents {
	return &subagents{s: s, slots: make(chan struct{}, maxChildren), live: map[string]*childHandle{}}
}

// attach makes t the turn this runner's calls report to, for the turn's life,
// and returns what detaches it (Run defers it, as it does the todo list's).
func (r *subagents) attach(t *turn) (release func()) {
	r.turn.Store(&turnLink{emit: t.emitLocked, sink: t.sink, model: t.model})
	return func() { r.turn.Store(nil) }
}

// setMode is SetMode's critical section under the registry lock: set the
// session's mode, then raise every registered child to it, both while no
// child can register (§3.5, panel P50). The caller holds s.mu; set takes
// modes.mu. On a nil runner — a sub-agent's session, whose SetMode refuses
// before it gets here — it only sets.
func (r *subagents) setMode(mode string, set func(string)) {
	if r == nil {
		set(mode)
		return
	}
	r.regMu.Lock()
	defer r.regMu.Unlock()
	set(mode)
	for _, h := range r.live {
		tool.Raise(&h.strictness, mode)
	}
}

// closeChildren is Close's first act (§3.8, panel P3): seal the registry and
// copy its handles under regMu, then, outside it and without waiting, signal
// each child's closing. A nil runner has no children.
func (r *subagents) closeChildren() {
	if r == nil {
		return
	}
	r.regMu.Lock()
	r.sealed = true
	handles := make([]*childHandle, 0, len(r.live))
	for _, h := range r.live {
		handles = append(handles, h)
	}
	r.regMu.Unlock()
	for _, h := range handles {
		h.signalClose()
	}
}

// signalClose tells the child it is closing, whether it has opened or not:
// the latch is set for an attach still to come, the child's context is
// cancelled with tool.ErrClosing, and an attached child is signalled itself
// (Session.signalClose: its turn cancelled with the same cause and its tools'
// closing channel closed, so a command it runs is killed with no grace even
// when an ordinary cancel reached it first). It waits for nothing.
func (h *childHandle) signalClose() {
	h.mu.Lock()
	h.closing = true
	sess := h.sess
	h.mu.Unlock()
	h.cancel(errClosing)
	if sess != nil {
		sess.signalClose()
	}
}

// attachChild records the opened child on its handle and reports whether it
// may run: false when a Close signalled the handle while the child was
// opening, in which case the child is signalled here, since the Close could
// not reach it. With signalClose this is one latch under one lock, so a Close
// and an Open that race reach the child exactly as if they had not.
func (h *childHandle) attachChild(sess *Session) bool {
	h.mu.Lock()
	h.sess = sess
	closing := h.closing
	h.mu.Unlock()
	if closing {
		sess.signalClose()
	}
	return !closing
}

// isClosing reports whether a Close has signalled this child.
func (h *childHandle) isClosing() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.closing
}

// acquire takes a slot, waiting for one while the call's context is live and
// the session is not closing, and reports whether it holds one (step 1). A
// slot won while the context or closing is also ready is given back: a
// select picks among ready cases at random, and a cancelled call must not
// start a child because the semaphore happened to win.
func (r *subagents) acquire(ctx context.Context, call tool.SubagentCall) bool {
	closing := r.s.tools.closing
	select {
	case r.slots <- struct{}{}:
	default:
		if r.seams.waiting != nil {
			r.seams.waiting(call)
		}
		select {
		case r.slots <- struct{}{}:
		case <-ctx.Done():
			return false
		case <-closing:
			return false
		}
	}
	if ctx.Err() != nil || isClosed(closing) {
		r.release()
		return false
	}
	return true
}

// release gives a slot back.
func (r *subagents) release() { <-r.slots }

// register adds a child under id and returns its handle and the mode it
// opens in — the parent's, read under regMu so a SetMode is wholly before or
// wholly after it (§3.5) — or false once Close has sealed the registry (step
// 4).
func (r *subagents) register(id string, cancel context.CancelCauseFunc) (*childHandle, string, bool) {
	r.regMu.Lock()
	defer r.regMu.Unlock()
	if r.sealed {
		return nil, "", false
	}
	mode := r.s.modes.current()
	h := &childHandle{id: id, cancel: cancel}
	h.strictness.Store(tool.Strictness(mode))
	r.live[id] = h
	return h, mode, true
}

// retire removes a child from the registry once its call is done with it.
func (r *subagents) retire(h *childHandle) {
	r.regMu.Lock()
	defer r.regMu.Unlock()
	delete(r.live, h.id)
}

// isClosed reports whether ch is closed, without waiting.
func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// abortedResult is the tool contract's answer for a call whose context is
// done or whose session is closing (tool.go's Prepared.Run).
func abortedResult() tool.Result {
	return tool.Result{Text: tool.AbortedText, IsError: true, Class: tool.ClassAborted}
}

// Run is tool.Subagents: one agent call, from the refusals that need no
// child to the child's result (see the file's comment for the slot's life).
// It never returns before the child it opened has run and closed.
func (r *subagents) Run(ctx context.Context, call tool.SubagentCall) tool.Result {
	parent := r.s
	switch {
	case parent.child:
		return tool.Result{Text: subagentNested, IsError: true, Class: tool.ClassToolError}
	case ctx.Err() != nil:
		return abortedResult()
	}
	link := r.turn.Load()
	if link == nil {
		return tool.Result{Text: subagentNoTurn, IsError: true, Class: tool.ClassToolError}
	}

	// The refusals the model can correct come before any slot is taken: the
	// type, the model and the effort, each resolved against what this session
	// offers and the running turn's model (§3.4, §3.6).
	persona, err := resolveAgentType(parent.tools.types, call.Type)
	if err != nil {
		var ce *callError
		if errors.As(err, &ce) {
			return ce.result()
		}
		return tool.Result{Text: err.Error(), IsError: true, Class: tool.ClassInvalidInput}
	}
	parentAlias, parentEffort := link.model.r.Alias, link.model.effort
	alias, err := parent.resolveChildModel(childModelInput{Call: call.Model, Persona: persona.Model,
		ParentAlias: parentAlias, ParentEffort: parentEffort}, parent.warn)
	if err != nil {
		return tool.Result{Text: err.Error(), IsError: true, Class: tool.ClassInvalidInput}
	}
	effort, err := parent.resolveChildEffort(alias, call.Effort, persona.Effort, parentAlias, parentEffort, parent.warn)
	if err != nil {
		return tool.Result{Text: err.Error(), IsError: true, Class: tool.ClassInvalidInput}
	}

	// Steps 1–3: a slot, its release deferred at once, and the recheck.
	if !r.acquire(ctx, call) {
		return abortedResult()
	}
	defer r.release()
	if ctx.Err() != nil || isClosed(parent.tools.closing) {
		return abortedResult()
	}
	if r.seams.acquired != nil {
		r.seams.acquired(call)
	}

	// Step 4: the child's id and context exist before it registers, so the
	// handle Close reaches carries both; the retirement is deferred before
	// anything that can fail. The context is derived from the call's, so a
	// parent cancel reaches it, with a cancel of its own for a close or a stop.
	id := uuid.NewV4().String()
	childCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	h, mode, ok := r.register(id, cancel)
	if !ok {
		return abortedResult()
	}
	defer r.retire(h)
	return r.runChild(ctx, childCtx, link, call, persona, alias, effort, h, mode)
}

// runChild is steps 5 and 6 and the child's turn: open it, defer its Close,
// attach it, report it started, run it, and answer. ctx is the call's and
// childCtx the child's own.
func (r *subagents) runChild(ctx, childCtx context.Context, link *turnLink, call tool.SubagentCall,
	persona tool.Persona, alias, effort string, h *childHandle, mode string) tool.Result {
	parent := r.s
	all, ids := childToolSet(persona, parent.tools.offered)
	open := r.seams.open
	if open == nil {
		open = Open
	}
	child, err := open(parent.childOpenOptions(alias, effort, &ChildOptions{
		ID: h.id, ParentSession: parent.ID(), ParentCall: call.ID,
		Type: persona.Name, PersonaPath: persona.Path, Role: persona.Role,
		AllTools: all, Tools: ids,
		BaseSystem: parent.system, BaseProfile: parent.tools.profile,
		Mode: mode, Strictness: &h.strictness, Locks: parent.tools.locks,
	}))
	if err != nil {
		if ctx.Err() != nil || isClosed(parent.tools.closing) || h.isClosing() {
			return abortedResult()
		}
		return failedResult(parent.Redact(err.Error()), "")
	}
	// Step 5: closed before the retirement and the release, whatever happens
	// from here — a panic included.
	defer func() { _ = child.Close() }()
	// Step 6: a Close that signalled while the child was opening is honoured
	// now; and a cancel that landed meanwhile starts nothing.
	if !h.attachChild(child) || ctx.Err() != nil {
		return abortedResult()
	}
	if r.seams.opened != nil {
		r.seams.opened(h.id, child)
	}

	// Everything the runner reports of this call is redacted first, with one
	// replacer over the union of the parent's keys — every one it knows, as
	// Session.Redact — and the child's, which can hold one more when the
	// environment gained a key between the two Opens. The prompt goes to the
	// child's model, its transcript and SubagentStarted, and Run sends and
	// persists a prompt as it is given, which is right for text a person typed
	// and wrong for text a model wrote (§3.9, panel P9). One pass, never the
	// parent's and then the child's (review r3): a replacer covers every byte
	// of overlapping occurrences only of its own keys, so a parent's key that
	// begins a longer key of the child's would leave, after the first pass's
	// marker, the rest of the longer key for the second pass not to recognise.
	red := redact.New(append(parent.tools.knownKeys(), child.tools.knownKeys()...)...)
	prompt := red.String(call.Prompt)
	child.mu.Lock()
	ran := child.cur.id()
	child.mu.Unlock()

	// The model's names are the table's, which is text like any other; the
	// dispatcher redacts them on the result (Result.Child), and these events
	// never pass through it (review r3).
	started := time.Now()
	link.emit(SubagentStarted{
		ID: h.id, CallID: call.ID, Type: red.String(persona.Name), Description: red.String(call.Description), Prompt: prompt,
		Model: red.String(alias), Effort: red.String(effort), Mode: mode, At: parent.now(),
	})
	var obs childObserver
	res, runErr := runTurn(childCtx, child, prompt, func(ev Event) {
		obs.observe(ev)
		link.sink(SubagentEvent{ID: h.id, Event: ev})
	})
	out := subagentOutcome{
		res: res, err: runErr,
		parentGone: ctx.Err() != nil || isClosed(parent.tools.closing) || h.isClosing(),
		stopped:    errors.Is(context.Cause(childCtx), errStoppedByUser),
	}.decide(&obs)

	// Finished says the child has ended and closed: its Close is idempotent,
	// and the deferred one then does nothing.
	_ = child.Close()
	usage, calls, steps := obs.totals()
	link.emit(SubagentFinished{
		ID: h.id, Status: out.status, Error: red.String(out.errText), Text: red.String(out.text), Usage: usage,
		Model: red.String(ran.Alias), Provider: red.String(ran.Provider), WireModel: red.String(ran.WireModel),
		ToolCalls: calls, Steps: steps, Duration: time.Since(started), At: parent.now(),
	})
	// Delivering Finished can block — on the parent's turn lock and on its
	// sink — and the parent's call can be cancelled, or its session begin to
	// close, meanwhile (review r3). A child that did not finish on its own —
	// stopped, failed or cancelled — then reads aborted, as decide would have
	// had it if the cause had landed first; a child that finished keeps its
	// outcome, which is persisted (decide's first rule, X6).
	if out.status != SubagentCompleted && (ctx.Err() != nil || isClosed(parent.tools.closing) || h.isClosing()) {
		out.result = abortedResult()
	}
	// The result is redacted with the same replacer: its text is the child's,
	// which can hold the key the child learned and the parent does not know,
	// and the agent tool and the dispatcher redact with the parent's alone
	// (review r3).
	out.result.Text = red.String(out.result.Text)
	// The child's usage, observed, rides on the result whichever way it
	// ended: the parent's step records it per model (§3.7). The model names
	// are the child's, redacted like the text; the dispatcher redacts them
	// again with the rest.
	out.result.Child = &tool.ChildUsage{Provider: red.String(ran.Provider), Model: red.String(ran.Alias), WireModel: red.String(ran.WireModel), Usage: tool.Usage{
		Input: usage.Input, Output: usage.Output, Reasoning: usage.Reasoning,
		CacheRead: usage.CacheRead, CacheCreation: usage.CacheCreation,
	}}
	return out.result
}

// runTurn is the child's Run, with a panic that unwinds through it recovered
// as its error, a childPanic (review r3). The runner then answers the call as
// for any other failure: the child closed, its SubagentFinished failed, and
// the call's result a tool_error that carries the child's observed usage —
// which the parent's step records (subagent_usage) — and goes through the
// agent tool's cap. Left to the dispatcher's recovery, outside the runner,
// the result lost both: a fresh error with no usage, and no cap.
func runTurn(ctx context.Context, child *Session, prompt string, sink func(Event)) (res Result, err error) {
	defer func() {
		if v := recover(); v != nil {
			res, err = Result{}, childPanic{value: v}
		}
	}()
	return child.Run(ctx, prompt, sink)
}

// childPanic is a panic recovered from a child's turn (runTurn), as the error
// decide reads.
type childPanic struct{ value any }

func (p childPanic) Error() string { return fmt.Sprintf("it panicked: %v", p.value) }

// childOpenOptions are the Options a child of s opens with (§3.2): the
// parent's own values, its extras cloned, with the child's model and effort
// and c. Its test seams go with them, so a test's profile and gate are the
// child's too.
func (s *Session) childOpenOptions(alias, effort string, c *ChildOptions) Options {
	s.mu.Lock()
	table := s.table
	s.mu.Unlock()
	return Options{
		Home: s.base.home, Workspace: s.base.workspace, Table: table,
		Model: alias, Effort: effort, Prompt: s.base.prompt.clone(), Child: c,
		NewModel: s.newModel, Getenv: s.getenv, Now: s.base.now, Version: s.base.version,
		MatchModel: s.matchModel, Warn: s.warn,
		tools: s.base.seams,
	}
}

// childBase is what a session keeps of its Options for the children it
// starts (§3.2: everything but the child's own is the parent's).
type childBase struct {
	home, workspace, version string
	now                      func() time.Time
	prompt                   PromptExtras // a clone, taken at Open
	seams                    toolSeams
}

// clone is p with slices of its own; their elements are strings.
func (p PromptExtras) clone() PromptExtras {
	return PromptExtras{Instructions: slices.Clone(p.Instructions), Catalog: slices.Clone(p.Catalog)}
}

// childObserver is what the runner sees of a child's events on their way to
// the parent (§3.7): the text rule, the usage, and the counts. Its own lock,
// though the child's turn already calls its sink one event at a time, so that
// what the call's goroutine reads once Run has returned is ordered by
// something the race detector can see.
type childObserver struct {
	mu sync.Mutex
	// cur is the step's text so far; last is the text of the last finished
	// step that had any. A retry discards cur, as the turn discards its own
	// deltas (turn.retry): what the failed attempt streamed is not the answer.
	cur         strings.Builder
	last        string
	usage       Usage
	calls       int
	steps       int
	doomStopped bool // the doom-loop guard ended the child's turn
}

// observe takes one of the child's events.
func (o *childObserver) observe(ev Event) {
	o.mu.Lock()
	defer o.mu.Unlock()
	switch e := ev.(type) {
	case TextDelta:
		o.cur.WriteString(e.Text)
	case Retrying:
		o.cur.Reset()
	case ToolCalled:
		o.calls++
	case Diag:
		if e.Kind == DiagDoomLoop && e.Fields["stopped"] == "true" {
			o.doomStopped = true
		}
	case StepDone:
		// Every step the child was billed for reports its usage here — a step
		// refused for its ids and one whose save failed included — so the sum
		// counts them all, however the child's turn then ended (panel P6).
		o.steps++
		u := &o.usage
		u.Input, u.Output, u.Reasoning = u.Input+e.Usage.Input, u.Output+e.Usage.Output, u.Reasoning+e.Usage.Reasoning
		u.CacheRead, u.CacheCreation = u.CacheRead+e.Usage.CacheRead, u.CacheCreation+e.Usage.CacheCreation
		if strings.TrimSpace(o.cur.String()) != "" {
			o.last = o.cur.String()
		}
		o.cur.Reset()
	}
}

// lastOutput is the child's text by §3.7's rule: the current step's partial
// text when it has any, else the last finished step's that had any. For a
// child that finished, the current step is empty, and this is its final
// message.
func (o *childObserver) lastOutput() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	if strings.TrimSpace(o.cur.String()) != "" {
		return o.cur.String()
	}
	return o.last
}

// totals are the usage, tool calls and steps observed.
func (o *childObserver) totals() (Usage, int, int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.usage, o.calls, o.steps
}

// subagentOutcome is how the child's turn ended, and what cut it short.
type subagentOutcome struct {
	res Result
	err error
	// parentGone: the call's context is done, or the session is closing.
	parentGone bool
	// stopped: the child's own context was cancelled with errStoppedByUser.
	stopped bool
}

// decided is an outcome as the parent sees it: the call's result, and the
// SubagentFinished's status, error and text.
type decided struct {
	result        tool.Result
	status        string
	errText, text string
}

// decide picks the call's result (§3.7, §3.8's precedence; panel P4):
//
//  1. a child whose turn ended on its own keeps that ending — a cancel or a
//     stop that landed after its final step was persisted included, since its
//     Run then reports that step's stop reason, as a parent's turn keeps an
//     outcome already persisted (turn.go's finish);
//  2. otherwise the parent's cancel or close outranks everything: aborted,
//     the tool contract, whatever else also happened to the child;
//  3. otherwise a stop by the user alone is not an error: the stop's text and
//     what the child had got to;
//  4. otherwise a failure is an error with its message and the child's last
//     output — a panic recovered from its turn (childPanic) included, whose
//     SubagentFinished says only that it panicked;
//  5. and a cancel nobody claims is aborted.
func (o subagentOutcome) decide(obs *childObserver) decided {
	last := obs.lastOutput()
	switch {
	case o.err == nil && o.res.StopReason != StopCancelled:
		obs.mu.Lock()
		doom := obs.doomStopped
		obs.mu.Unlock()
		return decided{result: completedResult(o.res.StopReason, last, doom), status: SubagentCompleted, text: last}
	case o.parentGone:
		return decided{result: abortedResult(), status: SubagentCancelled, text: last}
	case o.stopped:
		return decided{result: tool.Result{Text: withLastOutput(subagentStopped, last)}, status: SubagentCancelled, text: last}
	case o.err != nil:
		errText := o.err.Error()
		if errors.As(o.err, new(childPanic)) {
			errText = subagentPanicked
		}
		return decided{result: failedResult(o.err.Error(), last), status: SubagentFailed, errText: errText, text: last}
	}
	return decided{result: abortedResult(), status: SubagentCancelled, text: last}
}

// completedResult is a finished child's answer (§3.7): its text, with a note
// when the turn was cut short rather than finished — cut off, refused, or
// stopped by the doom-loop guard or the step limit, which one said by the
// guard's own Diag, since both end max_turn_requests — and the fixed text for
// one that ended with nothing to say. None is an error: the child ran.
func completedResult(stop, text string, doomStopped bool) tool.Result {
	var note string
	switch stop {
	case StopMaxTokens:
		note = subagentCutOff
	case StopRefusal:
		note = subagentRefused
	case StopMaxTurnRequests:
		note = subagentStepLimit
		if doomStopped {
			note = subagentLooped
		}
	}
	switch {
	case note != "" && strings.TrimSpace(text) == "":
		return tool.Result{Text: note}
	case note != "":
		return tool.Result{Text: text + "\n\n" + note}
	case strings.TrimSpace(text) == "":
		return tool.Result{Text: subagentNoMessage}
	}
	return tool.Result{Text: text}
}

// failedResult is a failed child's answer (§3.7): an error, class tool_error,
// saying what failed, with the child's last output when it had some. The
// agent tool caps it, as the dispatcher caps a success (§3.7's one cap).
func failedResult(msg, last string) tool.Result {
	text := subagentFailed + strings.TrimSuffix(strings.TrimSpace(msg), ".") + "."
	return tool.Result{Text: withLastOutput(text, last), IsError: true, Class: tool.ClassToolError}
}

// withLastOutput is text, then the child's last output when it had any.
func withLastOutput(text, last string) string {
	if strings.TrimSpace(last) == "" {
		return text
	}
	return text + "\n\n" + subagentLastOutput + last
}
