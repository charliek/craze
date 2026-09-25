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
// it. regMu guards it, sealed and the keys of the children the running turn
// has retired (spent), and nothing else. It is a leaf: it is never held
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
//  1. the slot is taken by occupancy (take): at once when one is free, or by
//     waiting on a change of occupancy, the call's context and the session's
//     closing channel; one won while either of the other two is also ready
//     is given back, and the call is aborted;
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
// The call's last word — the redaction and the truncation of its result, and
// then, last of all, the final arbitration of its outcome (settle) — is
// deferred before step 1, so it runs after everything the steps defer, any of
// which can wait (review r4); and the arbitration follows every step of its
// own that can (review r6). It is deferred before the call's first return, so
// a refusal that needs no slot goes through it too (review r7).
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
//
// # Stopping one child (§3.10)
//
// Session.CancelSubagent is the user's stop of one child: a lookup under
// regMu, released before anything else, and one cancel of the child's context
// with errStoppedByUser, made under the handle's own lock — no wait, no child
// turn lock, no sink, no event. What the stop comes to is the runner's to say,
// through the same precedence as any other cause (decide, then settle): a child
// whose turn ended on its own keeps its outcome, a parent's cancel or a closing
// session outranks the stop, a child whose Run failed stays failed, and the
// stop alone is §3.7's not-an-error row.
//
// Two different things keep a stop to the ending it caused (review r12). The
// handle's ended latch, set under that same lock once the child's Run has
// returned and before the runner reads the cause its outcome turns on, bounds
// when a stop is taken: from the latch on, a stop is refused with nothing
// changed (ErrNoSuchSubagent). It is not one step with Run's return, so a stop
// can still land between the two, and is taken. decide is what makes that
// harmless: a stop claims only an ending that is a cancel, which with the
// parent live nothing but a stop can have caused, so a child that completed
// keeps its answer and one whose Run failed stays failed, whatever stop landed
// after (§3.10: a stop racing the finish is harmless).
//
// # Background children (§3.11)
//
// A call that asks for the background, in a session opened with
// Options.Background, takes a slot the same way — one of the same four, never
// waiting for one — opens its child on the call's goroutine and returns once
// the child has started; the child's turn runs on a goroutine of the runner's,
// under the session's context rather than the call's, and its result waits in
// a delivery state until the parent's model has read it once (background.go).

// maxChildren is how many sub-agents one session runs at once (owner decision
// 7): a fifth call waits for a slot. Fantasy's own five parallel tool slots
// sit above it, so which four of five calls run first is Fantasy's scheduling
// and unordered (§3.8).
const maxChildren = 4

// errStoppedByUser is the cause a child's context is cancelled with when the
// user stops that one child (plan 026 §3.10: Session.CancelSubagent). A parent
// cancel or close outranks it (subagentOutcome), it claims only a turn that
// ended cancelled (decide), and then it is a stop, not a failure: the parent's
// model reads what the child had got to, and the child's SubagentFinished says
// who stopped it (subagentStoppedLabel).
var errStoppedByUser = errors.New("harness: the user stopped this sub-agent")

// ErrNoSuchSubagent is CancelSubagent's refusal (plan 026 §3.10): this session
// has no child by that id whose turn is still running — the id was never
// issued, the child's turn has ended, or the session is a sub-agent's, which
// starts none. Nothing was changed.
var ErrNoSuchSubagent = errors.New("harness: no such sub-agent")

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
	// subagentStoppedLabel is the Error of the SubagentFinished a child the
	// user stopped leaves behind (§3.10): the finished row says who stopped it,
	// which its status, cancelled, cannot. The call's result is subagentStopped,
	// which is not an error (§3.7) — unless the parent's cancel or its close
	// lands after the row went out and before the call returns, when settle
	// answers the call aborted and the row, already delivered, still says what
	// ended the child (X13's one arbitration point is the call's, not the
	// row's).
	subagentStoppedLabel = "stopped by the user"
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
	s *Session // the parent: this runner's session, fixed
	// slots are the slots held, a value each: its length is always fg+bg. It
	// is sent to and received from only under regMu, with fg or bg, so it
	// never blocks there; it is a channel so that its length can be read
	// without the lock.
	slots chan struct{}
	// turn is the running turn, attached for its life (attach): the sink and
	// the model a call's children report to and start from.
	turn atomic.Pointer[turnLink]

	// background, sink and onPending are Options.Background, Options.Sink and
	// Options.OnPending, fixed at Open (plan 026 §3.11).
	background bool
	sink       func(Event)
	onPending  func()
	// bgCtx is every background child's context's parent: the session's, not
	// a turn's or a call's, so a turn's cancel never reaches one;
	// closeBackground cancels it with errClosing. workers counts the
	// background children's goroutines, each added under regMu before it
	// starts and only while the registry is unsealed (launch), so Close's
	// Wait never meets an Add.
	bgCtx    context.Context
	bgCancel context.CancelCauseFunc
	workers  sync.WaitGroup

	regMu  sync.Mutex
	sealed bool                    // Close has begun: no child registers from here
	live   map[string]*childHandle // the registered children, by id
	// spent are the keys of the children the running turn has retired, kept
	// until the turn detaches (childKeys, review r8): a call's ToolFinished
	// follows its child's retirement and carries what the child wrote.
	spent []string
	// fg and bg are the slots held by foreground and by background children;
	// changed is closed, and replaced, on every take and every release, which
	// is what a foreground call waiting for a slot waits on (take).
	fg, bg  int
	changed chan struct{}
	// results are the background children's results, by id, in their
	// delivery states (background.go); order is their ids in launch order,
	// and finished counts the ones that have left running, numbering them in
	// the order they finished.
	results  map[string]*bgResult
	order    []string
	finished int

	// seams are the runner's test seams, set before the first turn; zero is
	// production.
	seams subagentSeams
}

// subagentSeams are the points a test needs to reach inside a call: to fail
// the child's Open, to know a call is waiting for a slot or holds one, to see
// the child session a call opened, to act the instant the child's Run has
// returned and again once its end is latched, to hold a call in its
// retirement, and to act as its last word begins.
type subagentSeams struct {
	open     func(Options) (*Session, error) // opens a child; nil is Open
	waiting  func(tool.SubagentCall)         // the call found no free slot and is about to wait for one
	acquired func(tool.SubagentCall)         // the call holds a slot, rechecked, and is about to register
	opened   func(id string, child *Session) // the child opened and is attached, before it runs
	returned func(id string)                 // the child's Run returned; its end is not yet latched, and a stop is still taken
	ended    func(id string)                 // the child's Run returned and its end is latched; its cause is not yet read
	retiring func(id string)                 // the call is about to take regMu to retire its child
	settling func(id string)                 // a registered call's last word (settle) begins: its child closed, retired, its slot given back
	// outputWaiting: an agent_output call found its child running and is about
	// to wait for it, holding no lock.
	outputWaiting func(id string)
}

// turnLink is what a call needs of the turn it runs in: the turn's locked
// emit, for the lifecycle events, its sink itself, for the children's own
// events, and its model, which a child's model and effort resolve against
// (§3.6: the running turn's, not the session's current one). number and wake
// are the turn's number and whether it is a wake: what an agent_output call's
// reservation is owned by (§3.11).
type turnLink struct {
	emit   func(Event)
	sink   func(Event)
	model  model
	number int
	wake   bool
}

// childHandle is one registered child: what Close and SetMode reach it by.
type childHandle struct {
	id string
	// strictness is the child's monotonic strictness (ChildOptions.Strictness):
	// set from the mode it registered in, raised by the parent's SetMode under
	// regMu (tool.Raise), and read by the child's gate at every check.
	strictness atomic.Int32
	// cancel is the child's context's: the runner's own, derived from the
	// call's. A close cancels it with tool.ErrClosing (signalClose); the user's
	// stop of this one child cancels it with errStoppedByUser, under mu, and
	// only while the child's end is not latched (stop).
	cancel context.CancelCauseFunc

	// mu guards the two latches and the opened session, which Close's signal,
	// a stop and the runner set from their own goroutines. A leaf: nothing is
	// taken under it but the context package's own locks, in stop's cancel.
	mu      sync.Mutex
	closing bool     // the latch: a Close has signalled this child, opened or not
	ended   bool     // the latch: the child's Run has returned, or it never ran; a stop is refused (stop)
	sess    *Session // the child, once opened and attached; nil before
}

// newSubagents is s's runner, with every slot free and nothing registered.
func newSubagents(s *Session) *subagents {
	r := &subagents{s: s, slots: make(chan struct{}, maxChildren), live: map[string]*childHandle{},
		changed: make(chan struct{}), results: map[string]*bgResult{}}
	r.bgCtx, r.bgCancel = context.WithCancelCause(context.Background())
	return r
}

// attach makes t the turn this runner's calls report to, for the turn's life,
// and returns what detaches it (Run defers it, as it does the todo list's).
// The detach forgets the keys of the children the turn retired: every call of
// the turn, and so every ToolFinished that could carry a child's text, has
// returned by then (childKeys). A background child's keys are its result's
// until that is delivered, and the detach does not touch them.
func (r *subagents) attach(t *turn) (release func()) {
	r.turn.Store(&turnLink{emit: t.emitLocked, sink: t.sink, model: t.model, number: t.number, wake: t.wake})
	return func() {
		r.turn.Store(nil)
		r.regMu.Lock()
		r.spent = nil
		r.regMu.Unlock()
	}
}

// childKeys are the keys of this session's sub-agents that Session.Redact
// covers besides its own (review r8, finding 1): every registered child's once
// it has opened — a child is still registered when its SubagentFinished
// reaches the sink — and every child's the running turn has retired, since
// the call's ToolFinished, which the adapter publishes after sanitizing, holds
// the child's text and comes after the retirement. A child that never opened
// knows none. And every background child's whose result has not been
// delivered (plan 026 §3.11, astra r14): the result waits, with the child's
// text in it, for however many turns, and its keys go with it — into spent
// once it is committed, for the rest of the turn that delivered it — whatever
// an unrelated turn's detach forgets.
//
// regMu is taken to copy the handles and the retired keys, and released before
// any handle's lock or any child's key lock is taken: each is a leaf, none is
// held across another or across a sink. A nil runner — a sub-agent's — has
// none.
func (r *subagents) childKeys() []string {
	if r == nil {
		return nil
	}
	r.regMu.Lock()
	handles := make([]*childHandle, 0, len(r.live))
	for _, h := range r.live {
		handles = append(handles, h)
	}
	keys := slices.Clone(r.spent)
	for _, res := range r.results {
		keys = append(keys, res.keys...)
	}
	r.regMu.Unlock()
	for _, h := range handles {
		if child := h.session(); child != nil {
			keys = append(keys, child.tools.knownKeys()...)
		}
	}
	return keys
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

// session is the child attached to this handle, nil before it opened.
func (h *childHandle) session() *Session {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.sess
}

// CancelSubagent stops one of this session's sub-agents — the child
// SubagentStarted named id — and nothing else of the session (plan 026
// §3.10). It cancels the child's context with errStoppedByUser and returns: it
// waits for nothing, takes no turn's lock and calls no sink, so a caller on the
// UI's own goroutine may make it. What the stop comes to is reported as the
// child's SubagentFinished and its call's result, by §3.8's precedence as the
// runner builds it (decide, then settle):
//
//   - a child whose turn had ended on its own keeps its outcome — a stop that
//     lands after its final step was persisted, before its Run returned,
//     included;
//   - the parent's cancel or a closing session outranks the stop: aborted;
//   - a stop claims only a turn that ended cancelled, which with the parent
//     live nothing but a stop can have done, and it is then not an error: the
//     finished row is cancelled, its Error "stopped by the user", and the
//     parent's model reads what the child had got to (§3.7);
//   - a child whose Run failed — its provider, a panic, a failed save — is
//     failed, whatever stop landed.
//
// It is idempotent: a second stop of a child still running returns nil and
// changes nothing — the first cause stands. It answers ErrNoSuchSubagent,
// having changed nothing, for an id never issued; for a child whose end the
// runner has latched, just after its Run returned (the handle's ended latch,
// stop); and on a sub-agent's own session, which has no runner. A stop that
// lands between the child's Run returning and that latch is taken — it returns
// nil — and changes nothing: the ending it meets completed or failed, which
// decide never gives a stop, or was a cancel already made — the parent's
// cancel or a Close, which outrank it, or an earlier stop's, whose cause
// stands (§3.10: a stop racing the finish is harmless).
//
// Against Close: Close signals every registered child's closing, a cancel of
// the same context with tool.ErrClosing, and whichever of the two causes
// reaches the context first, the child is aborted — decide reads the handle's
// closing latch as the parent gone, and settle, the call's last act, reads it
// again. A stop during a Close returns nil, or ErrNoSuchSubagent once the
// child's end is latched; either is right, and neither changes what the call
// answers.
//
// A child still opening is registered already, and a stop reaches the context
// its turn will run under, though no client knows its id yet (SubagentStarted
// follows the Open): its turn starts cancelled and reads stopped, and an Open
// that fails keeps its failure, which the stop cannot have caused.
func (s *Session) CancelSubagent(id string) error {
	r := s.subs
	if r == nil {
		return ErrNoSuchSubagent
	}
	// The handle is copied out and regMu released before its own lock is
	// taken: the two are never held together (childKeys, closeChildren).
	r.regMu.Lock()
	h := r.live[id]
	r.regMu.Unlock()
	if h == nil || !h.stop() {
		return ErrNoSuchSubagent
	}
	return nil
}

// stop cancels the child's context with errStoppedByUser unless its end has
// been latched, and reports whether it did (CancelSubagent). The cancel is
// made holding mu, the lock end latches under, so a stop is wholly before the
// latch — a cause the runner then reads — or after it, and refused. The latch
// bounds when a stop is taken, not what it comes to: it follows the child's
// Run returning rather than being one step with it, so a stop can land in
// between and be taken; decide lets a stop claim only a turn that ended
// cancelled, so a child that completed or failed keeps its outcome, and that
// stop has changed nothing (review r12).
//
// Holding mu across the cancel is safe: a context's cancel takes only the
// context package's own locks — each context's, parent before child — and runs
// none of craze's code on the caller's goroutine (an AfterFunc's function is
// started on a goroutine of its own, context.go's afterFuncCtx.cancel), so
// nothing it does can come back for mu. A second stop cancels a context
// already cancelled, which changes nothing: the first cause stands.
func (h *childHandle) stop() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.ended {
		return false
	}
	h.cancel(errStoppedByUser)
	return true
}

// end latches the child's end: its Run has returned, or it never ran, and a
// stop from here on is refused (stop). It bounds when a stop is taken; what a
// stop taken before it comes to is decide's.
func (h *childHandle) end() {
	h.mu.Lock()
	h.ended = true
	h.mu.Unlock()
}

// slotOutcome is what a call's attempt at a slot came to (take).
type slotOutcome int

const (
	slotTaken   slotOutcome = iota // the call holds a slot: its release is the call's
	slotBusy                       // every slot is held and waiting could not free one: busyText
	slotAborted                    // the call's context is done or the session is closing
)

// busyText answers a call that found every slot held with nothing to wait for
// (plan 026 §3.11): a background call, which never waits, or a foreground one
// when every holder is a background child, which does not end with a step.
// Something the model can act on, so an error it reads, not an abort.
var busyText = fmt.Sprintf("All %d sub-agent slots are in use; wait for one with agent_output or stop one.", maxChildren)

// busyResult is busyText as a call's result.
func busyResult() tool.Result {
	return tool.Result{Text: busyText, IsError: true, Class: tool.ClassToolError}
}

// take takes a slot for a call, foreground or background, by occupancy (plan
// 026 §3.11, panel CodeRabbit 10, astra r2-15). Four slots are shared by both
// kinds:
//
//   - a background call takes one if one is free, and otherwise fails fast
//     (slotBusy): it never waits;
//   - a foreground call takes one if one is free; if none is and every holder
//     is a background child, it fails fast as well, since nothing it could
//     wait for ends with its step; otherwise it waits — for a change of
//     occupancy, its context or the session's closing — and judges again at
//     every change, since the holder that leaves may be the last foreground
//     one, or a background call may take the slot one gave back.
//
// Reading the occupancy and taking the change signal it waits on are one
// regMu section (astra r14, P26), so a release between the two cannot be
// missed. A slot won while the context or closing is also done is given back
// and the call aborted (step 1's rule, P2): a select picks among ready cases
// at random, and a cancelled call must not start a child because a slot
// happened to free. The waiting seam is told once, when the call first waits.
func (r *subagents) take(ctx context.Context, call tool.SubagentCall, background bool) slotOutcome {
	closing := r.s.tools.closing
	for waited := false; ; waited = true {
		r.regMu.Lock()
		if r.fg+r.bg < maxChildren {
			r.holdLocked(background)
			r.regMu.Unlock()
			break
		}
		if background || r.fg == 0 {
			r.regMu.Unlock()
			if ctx.Err() != nil || isClosed(closing) {
				return slotAborted // a cancelled call reads aborted, the tool contract, whatever else is true
			}
			return slotBusy
		}
		changed := r.changed
		r.regMu.Unlock()
		if !waited && r.seams.waiting != nil {
			r.seams.waiting(call)
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return slotAborted
		case <-closing:
			return slotAborted
		}
	}
	if ctx.Err() != nil || isClosed(closing) {
		r.releaseSlot(background)
		return slotAborted
	}
	return slotTaken
}

// holdLocked counts a slot taken by a call of the given kind and signals the
// change. regMu is held, and a slot is free.
func (r *subagents) holdLocked(background bool) {
	if background {
		r.bg++
	} else {
		r.fg++
	}
	r.slots <- struct{}{}
	r.changedLocked()
}

// releaseSlot gives back a slot a call of the given kind held, and signals
// the change: a foreground call waiting for a slot judges again.
func (r *subagents) releaseSlot(background bool) {
	r.regMu.Lock()
	defer r.regMu.Unlock()
	if background {
		r.bg--
	} else {
		r.fg--
	}
	<-r.slots
	r.changedLocked()
}

// changedLocked wakes every waiter on the occupancy: the channel they hold is
// closed, and the next waiter takes a new one. regMu is held.
func (r *subagents) changedLocked() {
	close(r.changed)
	r.changed = make(chan struct{})
}

// acquire takes a foreground slot and reports whether the call holds one: the
// slot protocol's step 1 (take), with a busy and an aborted call alike false.
func (r *subagents) acquire(ctx context.Context, call tool.SubagentCall) bool {
	return r.take(ctx, call, false) == slotTaken
}

// release gives a foreground slot back.
func (r *subagents) release() { r.releaseSlot(false) }

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

// retire removes a child from the registry once its call is done with it,
// and keeps its keys for the rest of the running turn (childKeys). They are
// read before regMu is taken: a child's key lock is a leaf, never taken under
// the registry's, and a child's keys are those of its Open — nothing switches
// a child's model, which is the only way a session learns one.
//
// Before regMu it latches the child's end, for a child that never ran — its
// Open failed, or its attach was refused — and so never reached the runner's
// own latch after its Run (runChild): a stop from here on is refused.
func (r *subagents) retire(h *childHandle) {
	if r.seams.retiring != nil {
		r.seams.retiring(h.id)
	}
	h.end()
	var keys []string
	if child := h.session(); child != nil {
		keys = child.tools.knownKeys()
	}
	r.regMu.Lock()
	defer r.regMu.Unlock()
	delete(r.live, h.id)
	r.spendLocked(keys)
}

// spendLocked keeps keys for the rest of the running turn (spent), each once.
// regMu is held.
func (r *subagents) spendLocked(keys []string) {
	for _, k := range keys {
		if !slices.Contains(r.spent, k) {
			r.spent = append(r.spent, k)
		}
	}
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
func (r *subagents) Run(ctx context.Context, call tool.SubagentCall) (res tool.Result) {
	// The call's last word (settle, reviews r4, r6 and r7), deferred first:
	// before anything else the call defers, so that it runs after all of it,
	// and before the first return, so that every result the call returns — a
	// refusal that needs no child included — is redacted and cut by it. Only
	// the agent tool cuts nothing (its Truncate is None), and the dispatcher
	// never cuts an error: a refusal quoting 60 KiB of what the model sent
	// reached the model whole (review r7, finding 2).
	var c childCall
	defer func() { res = r.settle(ctx, call.ID, &c, res) }()

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

	// A call that asks for the background, in a session that runs background
	// children, goes its own way from here (background.go, §3.11); anywhere
	// else it is an ordinary call, and blocks until its child has ended.
	if call.Background && r.background {
		return r.runBackground(ctx, link, call, persona, alias, effort, &c)
	}

	// Steps 1–3: a slot, its release deferred at once, and the recheck.
	switch r.take(ctx, call, false) {
	case slotBusy:
		return busyResult()
	case slotAborted:
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
	c.h = h
	defer r.retire(h)
	return r.runChild(ctx, childCtx, link, call, persona, alias, effort, &c, mode)
}

// childCall is what a registered call's last word (settle) needs of it, set
// as the call learns it.
type childCall struct {
	h     *childHandle // the registered child's handle; nil before registration
	child *Session     // the child, once opened; nil before, and for one whose Open failed
	// completed: the child's turn ended on its own, so its outcome stands
	// whatever cause lands after (decide's first rule).
	completed bool
	// final: the call's answer is final whatever cause lands after — a
	// background call's acknowledgement, which says the child started; the
	// child is the session's from then on, not the call's (§3.11).
	final bool
}

// openChild is step 5's Open: the child session for h, with the parent's own
// Options and the call's type, model, effort and mode (§3.2).
func (r *subagents) openChild(h *childHandle, call tool.SubagentCall, persona tool.Persona, alias, effort, mode string) (*Session, error) {
	parent := r.s
	all, ids := childToolSet(persona, parent.tools.offered)
	open := r.seams.open
	if open == nil {
		open = Open
	}
	return open(parent.childOpenOptions(alias, effort, &ChildOptions{
		ID: h.id, ParentSession: parent.ID(), ParentCall: call.ID,
		Type: persona.Name, PersonaPath: persona.Path, Role: persona.Role,
		AllTools: all, Tools: ids,
		BaseSystem: parent.system, BaseProfile: parent.tools.profile,
		Mode: mode, Strictness: &h.strictness, Locks: parent.tools.locks,
	}))
}

// runChild is steps 5 and 6 and the child's turn: open it, defer its Close,
// attach it, report it started, run it, and answer. ctx is the call's and
// childCtx the child's own. The answer is the child's own, raw and whole:
// settle redacts it, cuts it and arbitrates it once everything the call
// defers has run.
func (r *subagents) runChild(ctx, childCtx context.Context, link *turnLink, call tool.SubagentCall,
	persona tool.Persona, alias, effort string, c *childCall, mode string) tool.Result {
	parent, h := r.s, c.h
	child, err := r.openChild(h, call, persona, alias, effort, mode)
	if err != nil {
		// Aborted instead when the parent is gone by the time the call
		// returns (settle).
		return failedResult(err.Error(), "")
	}
	c.child = child
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
	child.mu.Lock()
	ran := child.cur.id()
	child.mu.Unlock()

	// Everything the runner reports of this call is redacted first, each
	// string by a replacer built where it is used (union). The model's names
	// are the table's, which is text like any other; the dispatcher redacts
	// them on the result (Result.Child), and these events never pass through
	// it (review r3).
	//
	// Started and Finished are redacted as they are built, and each then waits
	// for the parent turn's lock and its sink before the adapter has it. A key
	// the parent learns in that wait is the limit Session.Redact states — a
	// switch that lands after a text was redacted and before it is used cannot
	// be covered — and the adapter redacts every lifecycle payload again (C5),
	// which covers a distinct key. It cannot repair a key that extends one
	// already replaced: a prompt holding sk-abcdefgh-new-secret, redacted
	// while only sk-abcdefgh was known, keeps -new-secret after the marker
	// once the parent learns the longer key, since neither whole key occurs
	// any more. An accepted residual (review r6, finding 3): it needs a key
	// learned mid-call that extends a known one, with the call's text holding
	// the longer. The prompt the child is sent is redacted afresh from the
	// call's own text (below), and the call's result in settle, so a key
	// learned while Started or Finished waited reaches neither.
	started := time.Now()
	red := r.union(child)
	link.emit(SubagentStarted{
		ID: h.id, CallID: call.ID, Type: red.String(persona.Name), Description: red.String(call.Description),
		Prompt: red.String(call.Prompt), Model: red.String(alias), Effort: red.String(effort), Mode: mode, At: parent.now(),
	})
	// The prompt goes to the child's model and its transcript, and Run sends
	// and persists a prompt as it is given, which is right for text a person
	// typed and wrong for text a model wrote (§3.9, panel P9). It is redacted
	// again, from the call's own text, by a replacer built after Started was
	// delivered: that can wait on the parent's turn lock and its sink, and a
	// key the parent learned meanwhile must not reach the child's model. It is
	// Started's prompt unless the parent learned a key in that window, which
	// this one is redacted of and Started's is not.
	prompt := r.union(child).String(call.Prompt)
	var obs childObserver
	res, runErr := runTurn(childCtx, child, prompt, func(ev Event) {
		obs.observe(ev)
		link.sink(SubagentEvent{ID: h.id, Event: ev})
	})
	if r.seams.returned != nil {
		r.seams.returned(h.id)
	}
	// The child's end, latched before the cause its outcome turns on is read
	// (§3.10): a stop from here is refused (childHandle.stop). The latch bounds
	// when a stop is taken, not what one comes to: a stop that landed since
	// runTurn returned was taken, and decide keeps it from relabelling this
	// ending — a stop claims only a turn that ended cancelled, so a child that
	// completed or failed on its own keeps its outcome (review r12).
	h.end()
	if r.seams.ended != nil {
		r.seams.ended(h.id)
	}
	// The handle's closing latch is read before the call's context and the
	// session's closing channel: it takes the handle's lock, which a stop can
	// hold, and the reads that cannot wait come last, so a cause that lands
	// while it waits is seen (settle's rule, review r12). This picks the
	// finished row; settle reads all three again, in the same order, and its
	// answer is the call's final word.
	closing := h.isClosing()
	out := subagentOutcome{
		res: res, err: runErr,
		parentGone: closing || ctx.Err() != nil || isClosed(parent.tools.closing),
		stopped:    errors.Is(context.Cause(childCtx), errStoppedByUser),
	}.decide(&obs)
	c.completed = out.status == SubagentCompleted

	// Finished says the child has ended and closed: its Close is idempotent,
	// and the deferred one then does nothing.
	_ = child.Close()
	usage, calls, steps := obs.totals()
	red = r.union(child)
	link.emit(SubagentFinished{
		ID: h.id, Status: out.status, Error: red.String(out.errText), Text: red.String(out.text), Usage: usage,
		Model: red.String(ran.Alias), Provider: red.String(ran.Provider), WireModel: red.String(ran.WireModel),
		ToolCalls: calls, Steps: steps, Duration: time.Since(started), At: parent.now(),
	})
	// The child's usage, observed, rides on the result whichever way it
	// ended: the parent's step records it per model (§3.7). The model names
	// are the child's; settle redacts them with the text, and the dispatcher
	// again with the rest.
	out.result.Child = &tool.ChildUsage{Provider: ran.Provider, Model: ran.Alias, WireModel: ran.WireModel, Usage: tool.Usage{
		Input: usage.Input, Output: usage.Output, Reasoning: usage.Reasoning,
		CacheRead: usage.CacheRead, CacheCreation: usage.CacheCreation,
	}}
	return out.result
}

// union is one replacer over every key the parent knows now — installed and
// pending, the ones Session.Redact covers — and every key child knows, which
// can be one more when the environment gained a key between the two Opens; a
// nil child, one that never opened, adds none.
//
// It is built at each use and never kept (review r4). The parent learns a key
// whenever its SetModel resolves one, and a child runs as long as it runs: a
// replacer kept from the child's Open would miss a key the parent learned
// meanwhile and let it through everything the runner reports after — the
// child's answer, which the parent's own dispatcher then redacts with the
// narrower redactor its running turn keeps.
//
// One pass, never the parent's and then the child's (review r3): a replacer
// covers every byte of overlapping occurrences only of its own keys, so a
// parent's key that begins a longer key of the child's would leave, after the
// first pass's marker, the rest of the longer key for the second pass not to
// recognise.
func (r *subagents) union(child *Session) *redact.Replacer {
	// The session-wide set Session.Redact covers — the parent's keys, every
	// registered child's and a retired child's until the turn ends — not the
	// parent's alone: a refusal with no child of its own can still quote a key
	// only an earlier child of this turn learned, and the adapter's wider pass
	// would find it already cut (review r10).
	keys := append(r.s.tools.knownKeys(), r.childKeys()...)
	if child != nil {
		keys = append(keys, child.tools.knownKeys()...)
	}
	return redact.New(keys...)
}

// settle is every call's last word, the runner's outermost deferred function
// (review r4): it runs after the child's Close, its retirement and the slot's
// release, any of which can wait — the retirement on regMu, which a SetMode or
// a Close holds while it walks the registry. It finishes the result the call
// built — the child's answer, its failure with its last output, or a result
// the call returned before any child registered, a refusal or aborted — in
// this order (review r6):
//
//  1. It redacts the text, with a replacer built here (union): the text is
//     the child's, which can hold a key the child learned and the parent does
//     not know, or one the parent learned while the child ran, and the
//     dispatcher redacts with the parent's installed keys alone (reviews r3,
//     r4). A call with no child has the parent's keys alone, every one it
//     knows (union of none).
//  2. It cuts the text, a success and an error alike (§3.7's one cap), with
//     the shared truncator at its limits: the head kept, and the whole text,
//     redacted, in a spill file under the parent's home named for the call,
//     whose path and notice a fresh union redacts as they are added. The
//     dispatcher's cut of a success, and the agent tool's of an error, added
//     that path after the runner's last redaction, redacted by the parent's
//     installed keys or by none: a key spelled across the path's fixed part,
//     or one the parent learned that its home holds, reached the parent's
//     model. The agent tool's spec is Truncate None, so nothing cuts the
//     answer twice — nor anything else the call returns: a refusal made
//     before registration is cut here too, since nothing after the runner
//     would cut it (review r7, finding 2).
//  3. It redacts the whole text once more, and the spill path, with a fresh
//     union: a key spelled across the notice, or across the path and the
//     text beside it, is replaced — which can leave the path one that opens
//     nothing, a key on the wire being the worse of the two. The model names
//     on the usage are redacted with them, and so is the aborted result the
//     arbitration can pick, prepared here: the tool contract's text is fixed,
//     but a key can equal it, and one the child alone knew went out raw when
//     the arbitration swapped it in after this pass (review r7, finding 1).
//  4. It arbitrates, last, after everything above that can wait — each union
//     takes both sessions' key locks, which a SetModel holds while it
//     resolves a key, and the cut writes a file — and it only picks one of
//     the two results prepared: nothing that can wait follows it (review r6:
//     a cause that landed while a union waited was missed by an arbitration
//     made before it). A child that did not finish on its own — stopped,
//     failed, cancelled, or never opened — reads aborted, its usage kept,
//     when the call's context is done, the session is closing or a Close has
//     latched the child by now: decide would have had it so had the cause
//     landed before the child's Run returned, and one that lands before the
//     call returns is no later as far as the parent can tell. A child that
//     finished keeps its outcome, which is persisted (decide's first rule,
//     X6). A cause that lands after the return — while the dispatcher
//     finishes the call — meets any tool's race with a cancel, which is
//     inherent.
//
// The arbitration's own reads keep step 4's rule (review r12): the handle's
// closing latch first, since it takes the handle's lock, which a second stop
// of the child can hold while it cancels; the call's context and the session's
// closing channel last, since they wait for nothing. A parent's cancel that
// landed while the latch's read waited was missed by a check that had read the
// context before it.
//
// A call that never registered has no child to arbitrate: its result is a
// refusal or aborted as it stands, redacted and cut.
func (r *subagents) settle(ctx context.Context, callID string, c *childCall, res tool.Result) tool.Result {
	if c.h != nil && r.seams.settling != nil {
		r.seams.settling(c.h.id)
	}
	res.Text = r.union(c.child).String(res.Text)
	res.Text, res.Trunc = tool.TruncateRedacted(r.s.base.home, callID, res.Text, tool.Head, r.union(c.child))
	red := r.union(c.child)
	res.Text, res.Trunc.Spill = red.String(res.Text), red.String(res.Trunc.Spill)
	if u := res.Child; u != nil {
		res.Child = &tool.ChildUsage{Provider: red.String(u.Provider), Model: red.String(u.Model),
			WireModel: red.String(u.WireModel), Usage: u.Usage}
	}
	if c.h == nil {
		return res
	}
	aborted := tool.Result{Text: red.String(tool.AbortedText), IsError: true, Class: tool.ClassAborted, Child: res.Child}
	// A background call's acknowledgement is final (§3.11): it says the child
	// started, which it did, and the child is the session's, not the call's —
	// a turn cancelled right after the spawn must not read "aborted" of a child
	// that runs on.
	if c.completed || c.final {
		return res
	}
	// The one read that can wait comes first; nothing that can follows the
	// last two.
	closing := c.h.isClosing()
	if closing || ctx.Err() != nil || isClosed(r.s.tools.closing) {
		return aborted
	}
	return res
}

// runTurn is the child's Run, with a panic that unwinds through it recovered
// as its error, a childPanic (review r3). The runner then answers the call as
// for any other failure: the child closed, its SubagentFinished failed, and
// the call's result a tool_error that carries the child's observed usage —
// which the parent's step records (subagent_usage) — and goes through the
// runner's cap (settle). Left to the dispatcher's recovery, outside the
// runner, the result lost both: a fresh error with no usage, and no cap.
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
//  3. otherwise a stop by the user claims the ending when it is a cancel —
//     Run returned no error, and every such ending that was not a cancel is
//     rule 1's — and is not an error: the stop's text and what the child had
//     got to, and a SubagentFinished whose error says who stopped it (§3.10);
//  4. otherwise a failure is an error with its message and the child's last
//     output — a panic recovered from its turn (childPanic) included, whose
//     SubagentFinished says only that it panicked — whatever stop landed;
//  5. and a cancel nobody claims is aborted.
//
// Rule 3 asks for a cancel because that is the one ending a stop can cause
// (review r12). A child's context is cancelled only by the parent (the call's
// context, parentGone), by a Close (the handle's closing latch, parentGone)
// or by a stop; and a turn whose context was cancelled ends cancelled with no
// error — unless its final step was already persisted, rule 1, or saving its
// interrupted step failed, which is an error (turn.go's finish). So a
// cancelled ending with the parent live is the stop's doing, and no other
// ending is a stop's: a provider's failure or a panic is the child's own, a
// failed save — even of a step a stop interrupted — is a failure the parent's
// model must read as one, and a stop that landed after any of them — between
// runTurn's return and the handle's ended latch, where one is still taken
// (childHandle.stop) — cannot relabel it.
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
	case o.stopped && o.err == nil:
		return decided{result: tool.Result{Text: withLastOutput(subagentStopped, last)}, status: SubagentCancelled,
			errText: subagentStoppedLabel, text: last}
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
// saying what failed, with the child's last output when it had some. settle
// cuts it, as it cuts a success (§3.7's one cap, review r6).
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
