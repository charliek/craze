package tui

import (
	"context"
	"errors"
	"reflect"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/backend"
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
// stays in the primary, as it does today (readOn).

// ErrNoAnswer is a gated call that did not answer in time: its deadline passed
// or the held queue reached its bound first. The outcome is unknown — the
// command may have run — and each continuation says so honestly. It is a
// client-side outcome and never travels on the wire.
var ErrNoAnswer = errors.New("no answer from the session")

// gateDeadline is the deadline a gated call is given (§3.12): fifteen seconds —
// long enough for the flock an index write can wait on, short enough that a
// stuck one does not freeze the TUI for good. An Interject waits on the agent
// taking it and is given a minute, passed to run by its own site (C18c). It is
// a variable so a test can shorten it; nothing else writes it.
var gateDeadline = 15 * time.Second

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
type gateReply struct {
	id     uint64
	result any
	err    error
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
	if m.gateSync {
		ctx, cancel := context.WithTimeout(context.Background(), deadline)
		r := callGated(ctx, b, 0, call)
		cancel()
		return cont(m, r)
	}
	m.gateSeq++
	g := &gate{id: m.gateSeq, cont: cont, deadline: deadline}
	m.gate = g
	return m, func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), deadline)
		defer cancel()
		done := make(chan gateReply, 1)
		go func() { done <- callGated(ctx, b, g.id, call) }()
		select {
		case r := <-done:
			return r
		case <-ctx.Done():
			return gateReply{id: g.id, err: ErrNoAnswer}
		}
	}
}

// callGated runs call with ctx, which ends at the call's deadline, and answers
// its reply. A call that gave up because that deadline passed did not answer:
// that is ErrNoAnswer, the outcome the deadline promises, not the context's
// own error.
func callGated(ctx context.Context, b backend.Backend, id uint64, call gateCall) gateReply {
	res, err := call(ctx, b)
	if err != nil && ctx.Err() != nil && errors.Is(err, ctx.Err()) {
		err = ErrNoAnswer
	}
	return gateReply{id: id, result: res, err: err}
}

// gated is Update: the gate over handle and the Update wrapper.
func (m Model) gated(msg tea.Msg, handle handler) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case gateReply:
		if m.gate == nil || msg.id != m.gate.id {
			return m, nil
		}
		return m.release(msg)
	case drainMsg:
		return m.drain(handle)
	case frameSyncMsg:
		return m.syncFrame(msg), nil
	case eventMsg:
		// The one outstanding read is over; readOn decides whether another
		// starts, once this event is placed.
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
		// shape.
		m.applyEvent(ev.ev)
		read := m.readOn()
		next, cmd = m.finish(read)
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
	if !isEvent && (opened || drained) {
		read := next.readOn()
		cmd = tea.Batch(cmd, read)
	}
	return next, cmd
}

// hold appends msg to the held queue. While a gate is open that is watched,
// and a queue that reaches either bound releases the gate with ErrNoAnswer; an
// event that arrived is followed by the reader's next read only while a gate
// is open (readOn).
func (m Model) hold(msg tea.Msg) (tea.Model, tea.Cmd) {
	n := payloadBytes(msg)
	m.held = append(m.held, heldMsg{msg: msg, bytes: n})
	m.heldBytes += n
	if m.gate != nil {
		m.noteGate(gateHeld)
		if len(m.held) >= heldMaxMessages || m.heldBytes >= heldMaxBytes {
			return m.release(gateReply{id: m.gate.id, err: ErrNoAnswer})
		}
	}
	if _, ok := msg.(eventMsg); ok {
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
	m.held = m.held[1:]
	m.heldBytes -= h.bytes
	if len(m.held) == 0 {
		// Let the array go: a hold of large records is not kept alive by the
		// slice that has drained them.
		m.held, m.heldBytes = nil, 0
	}
	var next Model
	var cmd tea.Cmd
	if s, ok := h.msg.(frameSyncMsg); ok {
		m.syncAck = s.n
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
// acknowledged at once when nothing is open or held, as it always was; while a
// gate is open, remembered and acknowledged by the release that leaves no gate
// open; while held messages drain, held behind them and acknowledged when
// drained. It never runs the Update wrapper.
func (m Model) syncFrame(s frameSyncMsg) Model {
	switch {
	case m.gate != nil:
		m.syncPending = s.n
		m.noteGate(gateHeld)
	case len(m.held) > 0:
		m.held = append(m.held, heldMsg{msg: s})
	default:
		m.syncAck = s.n
	}
	return m
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
	if m.reading || m.eng == nil || (m.gate == nil && len(m.held) > 0) {
		return nil
	}
	m.reading = true
	return waitEvent(m.eng)
}

// errValueBytes is what an error value inside a held message is charged: its
// text cannot be read without calling a foreign Error(), the rule
// internal/transcript's accounting keeps (transcript's errValueBytes).
const errValueBytes = 256

var errorType = reflect.TypeFor[error]()

// payloadBytes estimates the bytes a held message retains (§3.12, astra r2 17,
// r3 17). The messages that carry a payload are counted, and every other one
// is charged nothing and bounded by the message count alone:
//
//   - an event: every string and byte slice it reaches — its text, the tool's
//     input, output and diffs, a plan, a question, a delta's lists — with an
//     error value charged errValueBytes (eventBytes);
//   - a key: its runes, which is what a bracketed paste arrives as;
//   - a clipboard paste (Ctrl+V): its text.
//
// A restore item (PR 4) carries a snapshot; no message delivers one in process
// yet — waitEvent reads past it — and the message that does joins this switch.
func payloadBytes(msg tea.Msg) int {
	switch msg := msg.(type) {
	case eventMsg:
		return eventBytes(msg.ev)
	case tea.KeyMsg:
		return len(msg.Runes) * int(reflect.TypeFor[rune]().Size())
	case pasteMsg:
		return len(msg.text)
	}
	return 0
}

// eventBytes is an event's retained bytes. internal/transcript accounts an
// entry's retained bytes the same way (entryBytes: its text and its payload's
// strings, an unreadable error at a fixed charge) but exports no estimator for
// an event, so this is the TUI's own. It walks the event by reflection, so a
// payload field added to any event type is counted without being named here.
// An event is plain data — no handle, lock or channel — built once and never
// written again, so the walk reads nothing another goroutine is writing.
func eventBytes(ev agent.Event) int {
	return reachBytes(reflect.ValueOf(ev), map[uintptr]bool{})
}

func reachBytes(v reflect.Value, seen map[uintptr]bool) int {
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
			n += reachBytes(v.Index(i), seen)
		}
		return n
	case reflect.Array:
		n := 0
		for i := range v.Len() {
			n += reachBytes(v.Index(i), seen)
		}
		return n
	case reflect.Pointer:
		if v.IsNil() || seen[v.Pointer()] {
			return 0
		}
		seen[v.Pointer()] = true
		if v.Type() == reflect.TypeFor[*time.Location]() {
			// A time's zone is shared by every time in the process, and its
			// cache is written lazily: it is not the event's to count.
			return 0
		}
		return reachBytes(v.Elem(), seen)
	case reflect.Interface:
		if v.IsNil() {
			return 0
		}
		if v.Type().Implements(errorType) || v.Elem().Type().Implements(errorType) {
			return errValueBytes
		}
		return reachBytes(v.Elem(), seen)
	case reflect.Struct:
		n := 0
		for i := range v.NumField() {
			n += reachBytes(v.Field(i), seen)
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
			n += reachBytes(it.Key(), seen) + reachBytes(it.Value(), seen)
		}
		return n
	}
	return 0
}
