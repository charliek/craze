package tui

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"runtime/debug"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/craze/internal/backend"
	"github.com/charliek/craze/internal/engine"
)

// The command gate (plan 027 §3.12).
//
// A call the code after it needs the result of — for what it draws, or for its
// echo bookkeeping — used to run inside the Update that made it, and nothing
// else was processed until it returned. Over a socket that is a round trip, so
// the call moves into a tea.Cmd (m.run), and the model is frozen from the
// Update that issued it to the Update its reply lands in. Everything that
// arrives meanwhile is held, in arrival order, and applied afterwards, one
// message per Update — where bubbletea's channel would have put it while the
// old synchronous Update blocked. So the reducer sees today's message order:
//
//   - Hold: while a gate is open, or anything is still held, every message is
//     appended to held, except the awaited gateReply, a drainMsg, and the frame
//     harness's frameSyncMsg. A held message does not run the Update wrapper.
//   - Release: the reply for the open gate is applied the moment it arrives, in
//     an Update of its own: the gate is cleared, then the continuation runs,
//     then a drain is scheduled if anything is held and no new gate is open.
//     Nothing held is applied in that Update.
//   - Drain: each drainMsg applies exactly one held message, in its own Update,
//     through the handler and the wrapper (an event through applyEvent). A
//     drained message that opens a gate stops the drain until that gate's
//     reply.
//
// The reader keeps exactly one Read outstanding while a gate is open — the
// session's primary must keep draining, since a command can wait on primary
// room — and parks while held messages drain with no gate open, so the backlog
// stays in the primary, as it does today (readOn). What a socket's stream adds
// to the events — a restore, a ready, the end (PR 4, restore.go) — is placed
// exactly as an event is; a restore held behind the events of the stream it
// replaces drops them as it is held (dropSuperseded).

// ErrNoAnswer is a gated call that did not answer in time: its deadline passed
// or the held queue reached its bound first. The outcome is unknown — the
// command may have run — and each continuation says so honestly. It is a
// client-side outcome and never travels on the wire.
var ErrNoAnswer = errors.New("no answer from the session")

// noAnswerFor is err as the gate hands it to a continuation: a backend's
// outcome-unknown answer (backend.ErrOutcomeUnknown — a socket's resume loss,
// its redials spent, a call for an identity it has left) is ErrNoAnswer, the
// same "it may have run" every continuation already says honestly, so each
// handles it as it handles the deadline's: the same notes, the same card kept
// (plan 027 PR 4, C27). It is decided here, once, and never at the sites. The
// error stays the backend's underneath — errors.Is finds either sentinel, and
// the stale epoch a resume loss carries — and its text is ErrNoAnswer's, which
// is what a site that words a failure shows (failureText). Anything else is
// err, unchanged; in process nothing ever is outcome-unknown.
func noAnswerFor(err error) error {
	if err == nil || errors.Is(err, ErrNoAnswer) || !errors.Is(err, backend.ErrOutcomeUnknown) {
		return err
	}
	return &unknownOutcome{err: err}
}

// unknownOutcome is an outcome-unknown error seen as ErrNoAnswer (noAnswerFor).
type unknownOutcome struct{ err error }

func (e *unknownOutcome) Error() string        { return ErrNoAnswer.Error() }
func (e *unknownOutcome) Is(target error) bool { return target == ErrNoAnswer }
func (e *unknownOutcome) Unwrap() error        { return e.err }

// failureText is how a command's failure reads where the TUI words one
// itself — a fire-and-forget command's (a cancel's) and a settings chain's
// error row: its text, ErrNoAnswer's for an outcome that is unknown
// (noAnswerFor), exactly as a gated call's continuation hears it, and
// closingNote's for a command the session's close fence refused (plan 030
// §3.6, engine.ErrClosing), as the composer's refusals say it.
func failureText(err error) string {
	if errors.Is(err, engine.ErrClosing) {
		return closingNote
	}
	return noAnswerFor(err).Error()
}

// gateDeadline is the deadline a gated call is given (§3.12): fifteen seconds —
// long enough for the flock an index write can wait on, short enough that a
// stuck one does not freeze the TUI for good. It is a variable so a test can
// shorten it; nothing else writes it.
var gateDeadline = 15 * time.Second

// interjectDeadline is an Interject's (§3.12): it waits on the agent taking
// the message into its running turn — at its next tool result — so it is
// given a minute, passed to run by its site (interject). A variable for the
// same reason as gateDeadline.
var interjectDeadline = 60 * time.Second

// The held queue's bounds (§3.12, astra r2 17): a gate whose held messages
// reach either is released with ErrNoAnswer, and the reader parks while they
// drain, so no event is ever dropped and memory stays bounded.
const (
	heldMaxMessages = 10_000
	heldMaxBytes    = 64 << 20
)

// gateSyncDefault is the gate mode a new model starts in. Production leaves it
// false: every gated call is asynchronous. internal/tui's TestMain sets it, so
// a test that drives Update directly sees Enter's effect in the Update that
// pressed it, as it always has (§3.12 "Unit tests"); the frame runner sets
// the mode from FrameOpts, and a test that needs the asynchronous path says so
// (asyncGate). It is never a Config field: no production caller can choose
// the baseline.
var gateSyncDefault bool

// gateCall is a gated call's work, run on a goroutine of its own against the
// backend the issuing Update held. Every command id it spends is minted in
// that Update and captured by the closure, never minted here: the closure may
// not touch the model.
type gateCall func(ctx context.Context, b backend.Backend) (any, error)

// gateCont is everything today's code did after the call returned, at every
// level of the call chain up to Update: it runs on the model as the issuing
// Update left it, with the call's result, and may open the next link of a
// chain with m.run.
type gateCont func(m Model, r gateReply) (Model, tea.Cmd)

// gate is the one open gated call.
type gate struct {
	id       uint64
	cont     gateCont
	deadline time.Duration
}

// gateReply is a gated call's answer: its result and error, or ErrNoAnswer. A
// reply whose id is not the open gate's is late — its gate was released
// without it — and is dropped.
//
// linger is set on an ErrNoAnswer the deadline gave: the call is still
// running, and its outcome will arrive there. Whoever takes the reply — the
// release, or the drop of a late one — waits for it in a tea.Cmd (lingerOn),
// so a call that panics after its deadline panics inside Update, where
// bubbletea recovers it, never on a goroutine nothing recovers.
type gateReply struct {
	// issued is the session generation the call was issued under: a reply
	// for a session the model has since left releases its gate without
	// running the continuation (release).
	issued
	id     uint64
	result any
	err    error
	linger <-chan callOutcome
}

// drainMsg applies the next held message.
type drainMsg struct{}

func drainNext() tea.Msg { return drainMsg{} }

// heldMsg is a held message and the payload bytes it was charged.
type heldMsg struct {
	msg   tea.Msg
	bytes int
}

// gatePhase is a moment of a gate's life the invisibility watch looks at.
type gatePhase int

const (
	// gateOpened is the end of the Update that opened the gate, wrapper and
	// all: the state the model must hold until the reply.
	gateOpened gatePhase = iota + 1
	// gateHeld is a message held while the gate is open.
	gateHeld
	// gateReleasing is the reply's Update, before the continuation runs.
	gateReleasing
)

// gateHook, when a test sets it, is told of every gate's opening, of every
// message held while it is open and of its release, with the model at that
// moment; TestMain installs the invisibility watch here (TestTheGateIsInvisible).
// It is nil in production.
var gateHook func(phase gatePhase, m *Model)

func (m *Model) noteGate(phase gatePhase) {
	if gateHook != nil {
		gateHook(phase, m)
	}
}

// handler is the message handler the gate sits over: Model.update in
// production. The gate's own tests hand it one that can open a gate on a
// message of their own, and so exercise this code exactly.
type handler func(Model, tea.Msg) (tea.Model, tea.Cmd)

// run issues a gated call (§3.12 "The operation chain"): call runs against the
// backend, and cont — everything after the call, in every frame up to Update —
// runs with its reply. Everything before the call has already run in the
// issuing Update, and stays there.
//
// Asynchronously, the gate opens and the call runs in a tea.Cmd, bounded by
// deadline: its reply, or ErrNoAnswer when the deadline passes first (the
// call's goroutine lingers until it returns, and its answer is discarded). In
// the gateSync baseline the call runs inline and the continuation in this same
// Update — today's control flow exactly, which every golden is compared
// against.
//
// Exactly one gate is open at a time. A continuation may call run for the next
// link, because the release cleared the gate before running it; a second call
// in the same Update, with a gate already open, is a site whose post-call work
// was not moved into its continuation, and panics. So does a call with no
// backend — every site checks for one first, as every direct call did — here,
// in the Update, where bubbletea recovers it, rather than as a nil call on the
// command's goroutine, where nothing would.
func (m Model) run(deadline time.Duration, call gateCall, cont gateCont) (Model, tea.Cmd) {
	if m.gate != nil {
		panic("tui: a gated call was issued while another gate is open")
	}
	b := m.eng
	if b == nil {
		panic("tui: a gated call was issued with no backend")
	}
	// The call carries the backend epoch read now, in the issuing Update, so a
	// backend that has moved to another session by the time it runs refuses
	// it before sending anything (backend.ErrStaleEpoch; §3.12, "Chains are
	// fenced in the backend too"); its reply carries the session generation,
	// so one that lands after a replacement is dropped (issued, release).
	base, iss := dispatchCtx(b), m.issue()
	if m.gateSync {
		// Today's control flow exactly: the call inline, with the context
		// today's direct calls were given, which never ends. The deadline is
		// the asynchronous gate's alone.
		res, err := call(base, b)
		return cont(m, gateReply{issued: iss, result: res, err: noAnswerFor(err)})
	}
	m.gateSeq++
	g := &gate{id: m.gateSeq, cont: cont, deadline: deadline}
	m.gate = g
	return m, func() tea.Msg {
		ctx, cancel := context.WithTimeout(base, deadline)
		defer cancel()
		// Buffered, so the call's goroutine always parks its outcome and
		// ends, whoever is still there to take it: it never panics itself.
		out := make(chan callOutcome, 1)
		go func() { out <- runGated(ctx, b, g.id, call) }()
		select {
		case o := <-out:
			if o.panicked != nil {
				// The call panicked. Re-raised here, on the command's own
				// goroutine, bubbletea recovers it and ends the program with
				// ErrProgramPanic — what a panic inside the synchronous call
				// did from inside its Update — rather than a bare goroutine
				// taking the process down with the terminal still raw.
				panic(*o.panicked)
			}
			o.reply.issued = iss
			return o.reply
		case <-ctx.Done():
			// No answer in time. The call may still be running — or may
			// have panicked just now — so its outcome goes with the reply,
			// to be waited for where a panic is recovered (lingerOn).
			return gateReply{issued: iss, id: g.id, err: ErrNoAnswer, linger: out}
		}
	}
}

// callOutcome is how a gated call ended: its reply, or the panic it raised.
type callOutcome struct {
	reply    gateReply
	panicked *callPanic
}

// runGated runs the call on its own goroutine's behalf and answers how it
// ended, a panic included, with the stack it panicked on.
func runGated(ctx context.Context, b backend.Backend, id uint64, call gateCall) (o callOutcome) {
	defer func() {
		if v := recover(); v != nil {
			o = callOutcome{panicked: &callPanic{value: v, stack: debug.Stack()}}
		}
	}()
	return callOutcome{reply: callGated(ctx, b, id, call)}
}

// lingerOn waits for a call its gate stopped waiting for at the deadline. A
// call that returns is discarded — its answer came too late to mean anything
// — and one that panics comes back as callPanicMsg, which panics inside
// Update: bubbletea recovers that, restores the terminal and runs the exit
// tails, as it does for any Update panic.
func lingerOn(out <-chan callOutcome) tea.Cmd {
	return func() tea.Msg {
		if o := <-out; o.panicked != nil {
			return callPanicMsg{p: *o.panicked}
		}
		return nil
	}
}

// callPanicMsg is a lingering call's panic, on its way into Update. It is
// never held: it panics in the Update it reaches.
type callPanicMsg struct{ p callPanic }

// callPanic is a gated call's panic on its way to the command's goroutine,
// with the stack of the goroutine it happened on: re-raised there, the stack
// bubbletea prints would otherwise be the command's, which says nothing.
type callPanic struct {
	value any
	stack []byte
}

func (p callPanic) String() string { return fmt.Sprintf("%v\n\n%s", p.value, p.stack) }

// callGated runs call with ctx, which ends at the call's deadline, and answers
// its reply. A call that gave up because that deadline passed did not answer:
// that is ErrNoAnswer, the outcome the deadline promises, not the context's
// own error; and so is one whose outcome the backend says is unknown
// (noAnswerFor). The deadline is the asynchronous gate's: the baseline has
// none.
func callGated(ctx context.Context, b backend.Backend, id uint64, call gateCall) gateReply {
	res, err := call(ctx, b)
	return gateReply{id: id, result: res, err: unanswered(ctx, err)}
}

// unanswered is err, or ErrNoAnswer when it is ctx's own error once ctx has
// ended: a call that gave up because its deadline passed did not answer —
// nor did one whose outcome the backend says is unknown (noAnswerFor). A gated
// call that makes more than one backend call (clearPending's) judges each
// outcome by it.
func unanswered(ctx context.Context, err error) error {
	if err != nil && ctx.Err() != nil && errors.Is(err, ctx.Err()) {
		return ErrNoAnswer
	}
	return noAnswerFor(err)
}

// gated is Update: the gate over handle and the Update wrapper.
//
// Two kinds of message are another session's, and each is dropped here before
// the gate does anything with it (plan 030 §3.11; round-1 panel finding 3,
// R2-5): something a switch left behind (leftBehind) — a stream item or a
// start's answer of the backend it left (staleBackend), or a paste or a
// copy's note asked for in the session it no longer shows (staleShown; C11r,
// C11r2) — before it can clear the reader's flag, which from the switch on is
// the new backend's read, or be held and drained into the session that
// replaced it; and a gated call's reply issued for a session the model has
// left, before it can release a gate, acknowledge a sync token, owe a drain
// or arm a read. A switch leaves no gate open (withSession) and gate ids are
// never reused (gateSeq), so such a reply is never the open gate's own; one
// that named it anyway would still release nothing.
func (m Model) gated(msg tea.Msg, handle handler) (tea.Model, tea.Cmd) {
	if m.leftBehind(msg) {
		return m, nil
	}
	switch msg := msg.(type) {
	case callPanicMsg:
		panic(msg.p)
	case gateReply:
		if m.outdated(msg) || m.gate == nil || msg.id != m.gate.id {
			// Another session's (issued), or late: its gate was released
			// without it (the held queue's bound, its deadline). A call
			// still running past its deadline is still waited for, where its
			// panic would be recovered — that is no bookkeeping of the gate's.
			if msg.linger != nil {
				return m, lingerOn(msg.linger)
			}
			return m, nil
		}
		return m.release(msg)
	case drainMsg:
		return m.drain(handle)
	case frameSyncMsg:
		return m.syncFrame(msg)
	case eventMsg, restoreMsg, readyMsg, endMsg:
		// The one outstanding read is over; readOn decides whether another
		// starts, once this item is placed.
		m.reading = false
	}
	if m.gate != nil || len(m.held) > 0 {
		return m.hold(msg)
	}
	return m.apply(msg, handle, false)
}

// apply runs one message through the handler and the Update wrapper — an event
// through applyEvent, which never touches the reader — and then settles the
// gate's side of it: a gate the handler opened is watched from here, and the
// reader is reconciled after an event, an opening, and any drained message.
func (m Model) apply(msg tea.Msg, handle handler, drained bool) (tea.Model, tea.Cmd) {
	var next Model
	var cmd tea.Cmd
	ev, isEvent := msg.(eventMsg)
	if isEvent {
		// The reader goes where the event handler's own command always went,
		// ahead of the wrapper's: the batch an event's Update answers keeps its
		// shape. The event's own commands follow it: the hidden answers it
		// sends, and the gated read of a masked opening's ask — the one gate
		// an event opens (pushCard), watched from here like a handler's, with
		// the reader reconciled for it by readOn.
		evCmd := m.applyEvent(ev.ev)
		read := m.readOn()
		next, cmd = m.finish(tea.Batch(read, evCmd))
	} else {
		tm, c := handle(m, msg)
		n, ok := tm.(Model)
		if !ok {
			return tm, c
		}
		next, cmd = n.finish(c)
	}
	opened := next.gate != nil
	if opened {
		next.noteGate(gateOpened)
	}
	if !isEvent && (opened || drained || fromStream(msg)) {
		// A stream item the handler applied — a restore, a ready — ended the
		// read that delivered it, as an event does (readOn).
		read := next.readOn()
		cmd = tea.Batch(cmd, read)
	}
	return next, cmd
}

// fromStream reports whether msg is an item of the backend's stream other than
// an event — a restore, a ready or the end (waitEvent) — which the gate places
// as it places an event: holding it in arrival order behind whatever is held,
// and reconciling the reader once it is placed.
func fromStream(msg tea.Msg) bool {
	switch msg.(type) {
	case restoreMsg, readyMsg, endMsg:
		return true
	}
	return false
}

// hold appends msg to the held queue. While a gate is open that is watched,
// and a queue that reaches either bound releases the gate with ErrNoAnswer; an
// event or another stream item that arrived is followed by the reader's next
// read only while a gate is open (readOn). A restore drops, as it is held,
// what is held of the streams it replaces (dropSuperseded).
func (m Model) hold(msg tea.Msg) (tea.Model, tea.Cmd) {
	if r, ok := msg.(restoreMsg); ok {
		// Everything held of the streams it replaces is inside its snapshot,
		// or of a session that is gone: it is never applied (restore.go).
		m.dropSuperseded(r.gen)
	}
	n := payloadBytes(msg)
	m.pushHeld(heldMsg{msg: msg, bytes: n})
	m.heldBytes += n
	if m.gate != nil {
		m.noteGate(gateHeld)
		if len(m.held) >= heldMaxMessages || m.heldBytes >= heldMaxBytes {
			return m.release(gateReply{id: m.gate.id, err: ErrNoAnswer})
		}
	}
	if _, ok := msg.(eventMsg); ok || fromStream(msg) {
		read := m.readOn()
		return m, read
	}
	return m, nil
}

// release applies the open gate's reply, alone, in its own Update: the gate is
// cleared first — so a continuation that opens the next link is never erased —
// and the continuation runs with the wrapper after it, as today's post-call
// code ran inside the Update that made the call. A sync token that arrived
// while the gate was open is acknowledged here, once no gate is open: the
// token's barrier sees the continuation's frame, or a chain's end, as it would
// have seen the blocked Update's. Then the drain starts if anything is held.
//
// Every reply that reaches it is the open gate's own and current: gated has
// already turned away one issued for a session the model has left, before
// any of this bookkeeping (plan 030 §3.11, R2-5) — the session generation
// never moves while a gate is open, since whatever moves it (a restore, a
// session replaced or switched) is held behind the gate or leaves none open.
func (m Model) release(r gateReply) (tea.Model, tea.Cmd) {
	g := m.gate
	m.noteGate(gateReleasing)
	m.gate = nil
	next, cmd := g.cont(m, r)
	next, cmd = next.finish(cmd)
	if next.gate != nil {
		next.noteGate(gateOpened)
	} else if next.syncPending != 0 {
		next.syncAck = next.syncPending
		next.syncPending = 0
	}
	if next.gate == nil && len(next.held) > 0 {
		cmd = tea.Batch(cmd, drainNext)
	}
	if r.linger != nil {
		cmd = tea.Batch(cmd, lingerOn(r.linger))
	}
	read := next.readOn()
	return next, tea.Batch(cmd, read)
}

// drain applies the next held message in an Update of its own. A drained
// frameSyncMsg is acknowledged and runs no wrapper, as none ever has; anything
// else goes through apply. The drain goes on while messages are held and no
// gate is open.
func (m Model) drain(handle handler) (tea.Model, tea.Cmd) {
	if m.gate != nil || len(m.held) == 0 {
		return m, nil
	}
	h := m.held[0]
	// The drained slot is emptied before the queue moves past it: its payload
	// has been credited back, and the array must not keep it alive while
	// later messages stay held (astra C17 13).
	m.held[0] = heldMsg{}
	m.held = m.held[1:]
	m.heldDrained++
	m.heldBytes -= h.bytes
	switch {
	case len(m.held) == 0:
		// Let the array go.
		m.held, m.heldBytes, m.heldDrained = nil, 0, 0
	case 2*m.heldDrained > cap(m.held)+m.heldDrained:
		// More than half the array is drained slots: the rest moves to an
		// array of its own, so a long drain that reopens gates neither keeps
		// the old array nor grows it without bound.
		m.held = append([]heldMsg(nil), m.held...)
		m.heldDrained = 0
	}
	var next Model
	var cmd tea.Cmd
	if s, ok := h.msg.(frameSyncMsg); ok {
		m.syncAck = s.n
		cmd = m.readOn()
		next = m
	} else if m.leftBehind(h.msg) {
		// Held under a backend the model has since left, or asked for in a
		// session it no longer shows: another session's, never applied (plan
		// 030 §3.11). A switch takes each out of the queue as it is made
		// (dropStaleHeld); this is the rule held to wherever one is found.
		cmd = m.readOn()
		next = m
	} else {
		tm, c := m.apply(h.msg, handle, true)
		n, ok := tm.(Model)
		if !ok {
			return tm, c
		}
		next, cmd = n, c
	}
	if next.gate == nil && len(next.held) > 0 {
		cmd = tea.Batch(cmd, drainNext)
	}
	return next, cmd
}

// syncFrame is the frame harness's token (frame.go), through the model's FIFO:
// acknowledged after every message that arrived before it (C17c, astra C17b
// 1). Nothing open or held: at once, as it always was. A gate open and nothing
// held: the gate is its own key's — the frame runner hands the program a key
// and its token as one message, so nothing can come between them — and the
// token is remembered and acknowledged by the release that leaves no gate
// open, the key's whole chain done. Anything held — a gate an earlier or an
// asynchronous message opened, or a drain under way — and the token is held
// behind it, the key it came with included, and acknowledged when drained. It
// never runs the Update wrapper.
func (m Model) syncFrame(s frameSyncMsg) (tea.Model, tea.Cmd) {
	switch {
	case len(m.held) > 0:
		return m.hold(s)
	case m.gate != nil:
		m.syncPending = s.n
		m.noteGate(gateHeld)
	default:
		m.syncAck = s.n
	}
	return m, nil
}

// pushHeld appends h to the held queue. An append that moves the queue to a
// new array leaves the drained slots behind with the old one, so the count of
// drained slots the array holds starts again (drain's compaction).
func (m *Model) pushHeld(h heldMsg) {
	before := cap(m.held)
	m.held = append(m.held, h)
	if cap(m.held) != before {
		m.heldDrained = 0
	}
}

// readOn is the one reader rule (§3.12, astra 4, r2 8), run after every
// transition — an event arriving, a gate opening, a release, a drained
// message: if a gate is open, or nothing is held, and no read is in flight,
// start exactly one. So the primary keeps draining while a gate is open, the
// reader parks while held messages drain with no gate open, and a parked
// reader restarts the moment a drained message opens a gate or the last held
// one is drained.
//
// The read is never cancelled: waitEvent reads with a context that never
// ends, so an item Read takes is always delivered (backend.Backend.Read), and
// every delivered event is applied or held.
func (m *Model) readOn() tea.Cmd {
	if m.reading || m.eng == nil || m.ended || (m.gate == nil && len(m.held) > 0) {
		return nil
	}
	m.reading = true
	return waitEvent(m.eng, m.bgen)
}

// errValueBytes is what an error value inside a held message is charged: its
// text cannot be read without calling a foreign Error(), the rule
// internal/transcript's accounting keeps (transcript's errValueBytes).
const errValueBytes = 256

// heldCharge is what every held message costs before its payload — its place
// in the queue and its own words — so no held message is free.
const heldCharge = 64

var errorType = reflect.TypeFor[error]()

// payloadBytes estimates the bytes a held message retains (§3.12, astra r2 17,
// r3 17, C17 7): heldCharge, plus the message's own data, walked by
// reflection so a payload field added to any message is counted without being
// named here — an event's text and payloads, a key's runes (a bracketed
// paste), a clipboard paste, a settings result's values and notes, and what a
// later message carries (a restored snapshot, PR 4).
//
// The rule, which decides both what is counted and what is never read:
//
//   - a string counts its length, a slice of numbers (bytes, runes) its
//     length times the element's size;
//   - structs, arrays, slices and maps are walked, the message's own copy;
//   - an error value is charged errValueBytes and never walked;
//   - a pointer is followed only to plain data (plainData): a type built of
//     numbers, strings, errors and further plain data, not in the deny list;
//   - any other interface (a backend, a session, a message), a func, a chan,
//     and a pointer to anything else (the model, the engine, a controller, a
//     lock) is a handle, and is neither counted nor read: a live engine is
//     written by its own goroutines, so reading it would race as well as
//     miscount.
//
// internal/transcript accounts an entry's retained bytes the same way
// (entryBytes: its text and its payload's strings, an unreadable error at a
// fixed charge) but exports no estimator for a message or an event.
func payloadBytes(msg tea.Msg) int {
	if msg == nil {
		return heldCharge
	}
	return heldCharge + ownedBytes(reflect.ValueOf(msg), map[uintptr]bool{})
}

func ownedBytes(v reflect.Value, seen map[uintptr]bool) int {
	if payloadOpaque[v.Type()] {
		return 0
	}
	switch v.Kind() {
	case reflect.String:
		return v.Len()
	case reflect.Slice:
		if v.IsNil() || seen[v.Pointer()] {
			return 0
		}
		seen[v.Pointer()] = true
		if k := v.Type().Elem().Kind(); k <= reflect.Complex128 {
			// Bytes, runes and numbers: their size, element by element.
			return v.Len() * int(v.Type().Elem().Size())
		}
		n := 0
		for i := range v.Len() {
			n += ownedBytes(v.Index(i), seen)
		}
		return n
	case reflect.Array:
		n := 0
		for i := range v.Len() {
			n += ownedBytes(v.Index(i), seen)
		}
		return n
	case reflect.Pointer:
		if v.IsNil() || seen[v.Pointer()] || !plainData(v.Type().Elem()) {
			return 0
		}
		seen[v.Pointer()] = true
		return ownedBytes(v.Elem(), seen)
	case reflect.Interface:
		if v.IsNil() {
			return 0
		}
		if v.Type().Implements(errorType) || v.Elem().Type().Implements(errorType) {
			return errValueBytes
		}
		return 0
	case reflect.Struct:
		n := 0
		for i := range v.NumField() {
			n += ownedBytes(v.Field(i), seen)
		}
		return n
	case reflect.Map:
		if v.IsNil() || seen[v.Pointer()] {
			return 0
		}
		seen[v.Pointer()] = true
		n := 0
		it := v.MapRange()
		for it.Next() {
			n += ownedBytes(it.Key(), seen) + ownedBytes(it.Value(), seen)
		}
		return n
	}
	return 0
}

// payloadOpaque is the deny list: types a held message may carry that are
// never walked, by value or behind a pointer — the model itself, a time's
// zone (shared by every time in the process, its cache written lazily), and
// the locks.
var payloadOpaque = map[reflect.Type]bool{
	reflect.TypeFor[Model]():         true,
	reflect.TypeFor[time.Location](): true,
	reflect.TypeFor[sync.Mutex]():    true,
	reflect.TypeFor[sync.RWMutex]():  true,
	reflect.TypeFor[sync.Once]():     true,
}

var (
	plainMu    sync.Mutex
	plainTypes = map[reflect.Type]bool{}
)

// plainData reports whether t is plain data, which a held message's pointer
// may be followed to: numbers, strings, errors (charged, never walked), and
// structs, arrays, slices, maps and pointers of plain data — nothing in
// payloadOpaque or package sync, and no func, chan or other interface. An
// event's payloads (a tool, a plan, a question, a delta) are plain data; an
// engine, a controller or the model is not.
func plainData(t reflect.Type) bool {
	plainMu.Lock()
	defer plainMu.Unlock()
	return plainLocked(t)
}

func plainLocked(t reflect.Type) bool {
	if ok, known := plainTypes[t]; known {
		return ok
	}
	// A recursive type is plain unless some other part of it is not.
	plainTypes[t] = true
	ok := plainShape(t)
	plainTypes[t] = ok
	return ok
}

func plainShape(t reflect.Type) bool {
	if payloadOpaque[t] || t.PkgPath() == "sync" || t.PkgPath() == "sync/atomic" {
		return false
	}
	if t == reflect.TypeFor[time.Time]() {
		// Numbers and a zone pointer, and the zone is opaque: walked no
		// further.
		return true
	}
	switch t.Kind() {
	case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64, reflect.Complex64, reflect.Complex128, reflect.String:
		return true
	case reflect.Interface:
		return t == errorType
	case reflect.Pointer, reflect.Slice, reflect.Array:
		return plainLocked(t.Elem())
	case reflect.Map:
		return plainLocked(t.Key()) && plainLocked(t.Elem())
	case reflect.Struct:
		for i := range t.NumField() {
			if !plainLocked(t.Field(i).Type) {
				return false
			}
		}
		return true
	}
	return false
}
