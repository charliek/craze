package tui

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/backend"
	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/roster"
)

// Opening a session in place (plan 030 §3.11, C11): every schedule §3.18 PR 2
// names for switchBackend is forced here, one step at a time, never by
// repetition or sleeping — the old backend's stream items and start answer,
// its gated calls' replies and timeouts, the post-restore reads, overlapping
// dials and a dial answered after the user moved on — each step bounded on its
// own (runWatched), each run at a 5 % CPU quota as it was written.

// ---------------------------------------------------------------- fixtures

// laneBackend is a session served elsewhere as these tests drive it: a
// backend whose stream is a lane the test fills item by item (push), whose
// Start answers at once — or, with startWith, when the test says — and whose
// Close ends the lane — a view close, the session going on "on its host". A
// method the tests do not expect panics (the nil embedded interface).
type laneBackend struct {
	backend.Backend
	info   backend.SessionInfo
	items  chan backend.Item
	closed chan struct{}
	once   sync.Once
	closes atomic.Int32
	// startWith, when set, is Start's answer, handed over by the test: until
	// it is, Start waits — a host whose start is still running.
	startWith chan error
	// answer is Submit's answer, handed over by the test: until it is, the
	// call waits, whatever its context says — a call that outlives its gate.
	answer   chan engine.SubmitResult
	lastTurn atomic.Pointer[engine.LastTurn]
}

var _ backend.Backend = (*laneBackend)(nil)

func newLane(t *testing.T, id, name string) *laneBackend {
	t.Helper()
	return &laneBackend{
		info: backend.SessionInfo{CrazeSessionID: id, Incarnation: "inc-" + id, Workspace: laneWorkspace(t, name),
			Provider: "grok", Label: "Grok"},
		items:  make(chan backend.Item, 64),
		closed: make(chan struct{}),
		answer: make(chan engine.SubmitResult, 1),
	}
}

// laneWorkspace is a directory named name, so a frame names its session's
// workspace by it.
func laneWorkspace(t *testing.T, name string) string {
	t.Helper()
	ws := filepath.Join(t.TempDir(), name)
	if err := os.Mkdir(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	return ws
}

func (b *laneBackend) push(it backend.Item)      { b.items <- it }
func (b *laneBackend) Info() backend.SessionInfo { return b.info }
func (b *laneBackend) ClientID() string          { return "client-" + b.info.CrazeSessionID }
func (b *laneBackend) Epoch() uint64             { return 1 }
func (b *laneBackend) Started(error)             {}

func (b *laneBackend) Start(ctx context.Context) error {
	if b.startWith == nil {
		return nil
	}
	select {
	case err := <-b.startWith:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *laneBackend) Close() error {
	b.once.Do(func() { close(b.closed) })
	b.closes.Add(1)
	return nil
}

// Read hands up the lane's next item — one already there first, as a read
// that had taken it before a close — and backend.ErrClosed once the lane is
// closed and empty.
func (b *laneBackend) Read(ctx context.Context) (backend.Item, error) {
	select {
	case it := <-b.items:
		return it, nil
	default:
	}
	select {
	case it := <-b.items:
		return it, nil
	case <-b.closed:
		return backend.Item{}, backend.ErrClosed
	case <-ctx.Done():
		return backend.Item{}, ctx.Err()
	}
}

func (b *laneBackend) Submit(context.Context, engine.Command, string, engine.SubmitMode, string) (engine.SubmitResult, error) {
	return <-b.answer, nil
}

func (b *laneBackend) LastTurn(context.Context) (*engine.LastTurn, error) {
	return b.lastTurn.Load(), nil
}

// restoreItem is the lane's restore of its incarnation with evs folded, the
// stream's generation gen.
func (b *laneBackend) restoreItem(t *testing.T, gen uint64, evs ...agent.Event) backend.Item {
	t.Helper()
	return backend.Item{Kind: backend.ItemRestore, Info: b.info, Snapshot: snapshotFolded(t, b.info.Incarnation, evs...), Gen: gen}
}

// laneRow is the list's row of lane b's session, idle, listed ago.
func laneRow(b *laneBackend, title string, ago time.Duration) roster.Row {
	return answeredInc(b.info.CrazeSessionID, b.info.Incarnation, title, "grok", b.info.Workspace, ago, nil)
}

// armedRead is a read of a backend's stream the model armed, not yet run, and
// the backend generation it was armed under.
type armedRead struct {
	cmd  tea.Cmd
	bgen uint64
}

// switchRig drives a model through its Update as bubbletea would, one message
// per Update, with every command the model hands back sorted by what it is
// and none run until the test says: the stream reads (under the generation
// each was armed under), the gated calls, the starts, the dials, the retired
// backends' closes, the reads after a restore, the waits for a lingering call,
// the drains, and the terminal's own work — the composer's shell's runs, the
// pastes and the copies (C11r). So each schedule is exactly the one written
// down. The rig fails the test the moment two reads are armed under the
// model's current backend generation: exactly one ever may be (§3.12), a
// switch or not.
type switchRig struct {
	t         *testing.T
	m         Model
	reads     []armedRead
	calls     []tea.Cmd
	starts    []tea.Cmd
	opens     []tea.Cmd
	retires   []tea.Cmd
	lastTurns []tea.Cmd
	lingers   []tea.Cmd
	shells    []tea.Cmd
	pastes    []tea.Cmd
	copies    []tea.Cmd
	drains    int
	// last is the command the latest Update answered, whole.
	last tea.Cmd
}

func newSwitchRig(t *testing.T, m Model) *switchRig {
	t.Helper()
	r := &switchRig{t: t, m: m}
	if m.reading {
		r.reads = append(r.reads, armedRead{waitEvent(m.eng, m.bgen), m.bgen})
	}
	return r
}

func (r *switchRig) send(msg tea.Msg) {
	r.t.Helper()
	// The production gate over the production handler, plus the gate tests'
	// own gated calls (gateOpMsg).
	tm, cmd := r.m.gated(msg, withGateOps)
	r.m = tm.(Model)
	r.last = cmd
	r.sort(cmd)
	current := 0
	for _, rd := range r.reads {
		if rd.bgen == r.m.bgen {
			current++
		}
	}
	if current > 1 {
		r.t.Fatalf("%d reads armed under backend generation %d at once: exactly one ever may be", current, r.m.bgen)
	}
}

func (r *switchRig) sort(cmd tea.Cmd) {
	r.t.Helper()
	if cmd == nil {
		return
	}
	switch name := cmdFuncName(cmd); {
	case strings.HasPrefix(name, teaPkg+"compactCmds"), strings.HasPrefix(name, teaPkg+"Batch"):
		if b, ok := cmd().(tea.BatchMsg); ok {
			for _, c := range b {
				r.sort(c)
			}
		}
	case strings.HasPrefix(name, tuiPkg+"waitEvent"):
		r.reads = append(r.reads, armedRead{cmd, r.m.bgen})
	case name == tuiPkg+"drainNext":
		r.drains++
	case strings.HasPrefix(name, tuiPkg+"Model.run"):
		r.calls = append(r.calls, cmd)
	case strings.HasPrefix(name, tuiPkg+"lingerOn"):
		r.lingers = append(r.lingers, cmd)
	case strings.Contains(name, "readLastTurn"):
		r.lastTurns = append(r.lastTurns, cmd)
	case strings.Contains(name, "startCmd"):
		r.starts = append(r.starts, cmd)
	case strings.Contains(name, "sessOpen"):
		r.opens = append(r.opens, cmd)
	case strings.Contains(name, "Model.retire"):
		r.retires = append(r.retires, cmd)
	case strings.Contains(name, "shellController).start"):
		r.shells = append(r.shells, cmd)
	case strings.Contains(name, "pasteFromClipboard"):
		r.pastes = append(r.pastes, cmd)
	case strings.Contains(name, "sendCopy"):
		r.copies = append(r.copies, cmd)
	}
	// Anything else — a timer, a roster's read or close — is never run.
}

// drainAll applies the owed drains, one Update each, until none is owed.
func (r *switchRig) drainAll() {
	r.t.Helper()
	for r.drains > 0 {
		r.drains--
		r.send(drainMsg{})
	}
}

// later runs cmd on a goroutine of its own now — a command the program has
// started and that has not answered — and answers a wait for its message,
// bounded, for when the test delivers it.
func later(t *testing.T, cmd tea.Cmd) func() tea.Msg {
	t.Helper()
	out := make(chan tea.Msg, 1)
	go func() { out <- cmd() }()
	return func() tea.Msg {
		t.Helper()
		select {
		case msg := <-out:
			return msg
		case <-time.After(pumpWatchdog):
			t.Fatalf("a command did not answer in %s", pumpWatchdog)
			return nil
		}
	}
}

// pop takes the oldest command of cmds and runs it, bounded.
func (r *switchRig) pop(cmds *[]tea.Cmd, what string) tea.Msg {
	r.t.Helper()
	return runWatched(r.t, r.take(cmds, what))
}

// take takes the oldest command of cmds, unrun.
func (r *switchRig) take(cmds *[]tea.Cmd, what string) tea.Cmd {
	r.t.Helper()
	if len(*cmds) == 0 {
		r.t.Fatalf("no %s is waiting", what)
	}
	cmd := (*cmds)[0]
	*cmds = (*cmds)[1:]
	return cmd
}

// readOf runs the read armed under backend generation bgen, bounded, and
// answers what it read, undelivered.
func (r *switchRig) readOf(bgen uint64) tea.Msg {
	r.t.Helper()
	for i, rd := range r.reads {
		if rd.bgen == bgen {
			r.reads = slices.Delete(r.reads, i, i+1)
			return runWatched(r.t, rd.cmd)
		}
	}
	r.t.Fatalf("no read is armed under backend generation %d", bgen)
	return nil
}

// stream is lane b's next item, read by the model's current read and
// delivered.
func (r *switchRig) stream(b *laneBackend, it backend.Item) {
	r.t.Helper()
	b.push(it)
	msg := r.readOf(r.m.bgen)
	if msg == nil {
		r.t.Fatal("the current read answered nothing")
	}
	r.send(msg)
}

// up brings the model's current backend b up: its start answered, its first
// restore delivered (and the read after it held).
func (r *switchRig) up(b *laneBackend, evs ...agent.Event) {
	r.t.Helper()
	r.send(r.pop(&r.starts, "start"))
	r.stream(b, b.restoreItem(r.t, 1, evs...))
	if !r.m.sessionReady() || r.m.info().CrazeSessionID != b.info.CrazeSessionID {
		r.t.Fatalf("fixture: %s is not up (ready %v, session %q)", b.info.CrazeSessionID, r.m.sessionReady(), r.m.info().CrazeSessionID)
	}
}

// switchTo opens the list over rows, selects to's row and enters it, and
// answers the dial with to: the switch. The list's roster is the test's.
func (r *switchRig) switchTo(to *laneBackend, rows ...roster.Row) {
	r.t.Helper()
	r.openList(rows...)
	r.enterOn(to)
	r.send(r.pop(&r.opens, "dial"))
	if r.m.eng != to {
		r.t.Fatalf("the switch to %s did not adopt it", to.info.CrazeSessionID)
	}
}

// openList presses ← on the empty composer and hands the list rows.
func (r *switchRig) openList(rows ...roster.Row) {
	r.t.Helper()
	r.send(tea.KeyMsg{Type: tea.KeyLeft})
	if !r.m.sessList.open {
		r.t.Fatalf("← did not open the list:\n%s", plainView(r.m))
	}
	r.send(sessSnapMsg{gen: r.m.sessList.gen, snap: roster.Snapshot{Running: rows}})
}

// enterOn selects b's row and presses enter: its dial is asked for.
func (r *switchRig) enterOn(b *laneBackend) {
	r.t.Helper()
	r.m = selectKey(r.t, r.m, sessKey{id: b.info.CrazeSessionID, inc: b.info.Incarnation})
	before := len(r.opens)
	r.send(enter())
	if len(r.opens) != before+1 {
		r.t.Fatalf("enter on %s asked for no dial", b.info.CrazeSessionID)
	}
}

// laneSessions is the session list's Open over lanes: each host's next
// backend, in order (a session opened twice is two backends).
func laneSessions(lanes map[string][]*laneBackend) *fakeSessions {
	var mu sync.Mutex
	return &fakeSessions{open: func(ref roster.Ref) (backend.Backend, error) {
		mu.Lock()
		defer mu.Unlock()
		id := strings.TrimPrefix(ref.Host.ID, "host-")
		q := lanes[id]
		if len(q) == 0 {
			return nil, errors.New("no such session")
		}
		lanes[id] = q[1:]
		return q[0], nil
	}}
}

// laneModel is a TUI over lane a with a session list whose Open answers
// lanes, sized, its gated calls asynchronous, and a's session up.
func laneModel(t *testing.T, a *laneBackend, lanes map[string][]*laneBackend) (*switchRig, *fakeSessions) {
	t.Helper()
	isolateSkillsHome(t)
	fs := laneSessions(lanes)
	m := New(Config{Backend: a, Theme: "tokyo-night", Workspace: a.info.Workspace, Yolo: true, Sessions: fs})
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = asyncGate(t, tm.(Model))
	r := newSwitchRig(t, m)
	r.starts = append(r.starts, m.startCmd())
	r.up(a)
	return r, fs
}

// dropped asserts msg, delivered now, was dropped before the gate did
// anything with it: not the reader's flag, not the program's life, not one
// field of the model, no command but the wait for a call still lingering past
// its deadline (where its panic would be recovered).
func (r *switchRig) dropped(what string, msg tea.Msg) {
	r.t.Helper()
	if msg == nil {
		r.t.Fatalf("%s: the delayed read answered nothing", what)
	}
	before := digestModel(&r.m)
	reading, quitting, ended := r.m.reading, r.m.quitting, r.m.ended
	r.send(msg)
	if changed := before.diff(digestModel(&r.m)); len(changed) != 0 {
		r.t.Fatalf("%s from the backend the model left changed %v", what, changed)
	}
	if r.m.reading != reading || r.m.quitting != quitting || r.m.ended != ended ||
		(r.last != nil && !strings.HasPrefix(cmdFuncName(r.last), tuiPkg+"lingerOn")) {
		r.t.Fatalf("%s from the backend the model left: reading %v→%v, quitting %v→%v, ended %v→%v, command %v",
			what, reading, r.m.reading, quitting, r.m.quitting, ended, r.m.ended, cmdFuncName(r.last))
	}
}

// unapplied asserts msg — a result handled in Update, as every result is —
// wrote nothing of its own: the model moved no field but the layout its
// Update's wrapper recomputes.
func (r *switchRig) unapplied(what string, msg tea.Msg) {
	r.t.Helper()
	before := digestModel(&r.m)
	r.send(msg)
	for _, f := range before.diff(digestModel(&r.m)) {
		if f != "lay" && f != "layouts" {
			r.t.Fatalf("%s was applied: %s moved", what, f)
		}
	}
}

// ------------------------------------------------------------- the stream

// TestASwitchDropsTheStreamItLeft (plan 030 §3.11, AC9): A→B→A, with an item
// of each kind from the backend left behind — an event, a restore, a ready,
// the end — arriving after the switch that left it. Each arrives on the read
// the model armed before the switch, carrying the backend generation it was
// armed under, and each is dropped before the gate does anything: none clears
// the reader's flag (the new backend's read is still the one armed), none
// quits, none moves a field of the model. The backend left behind is closed
// off the Update, its lane ended, so a read of it that took nothing ends with
// nothing. And back on A — a new backend for the same session — the first
// restore is a first restore, and B's own late item is dropped as A's was.
func TestASwitchDropsTheStreamItLeft(t *testing.T) {
	for _, kind := range []string{"an event", "a restore", "a ready", "the end"} {
		t.Run(kind, func(t *testing.T) {
			a, b, a2 := newLane(t, "a", "alpha"), newLane(t, "b", "bravo"), newLane(t, "a", "alpha")
			r, _ := laneModel(t, a, map[string][]*laneBackend{"b": {b}, "a": {a2}})
			gA := r.m.bgen
			rows := []roster.Row{laneRow(a, "session a", time.Minute), laneRow(b, "session b", 2*time.Minute)}

			// A → B: A's read stays armed, never run.
			r.switchTo(b, rows...)
			if r.m.bgen == gA || !r.m.reading || r.m.sessList.open {
				t.Fatalf("after the switch: bgen %d (was %d), reading %v, list open %v", r.m.bgen, gA, r.m.reading, r.m.sessList.open)
			}
			r.up(b)
			gB := r.m.bgen

			// A's item, taken by the read armed before the switch.
			a.push(laneItem(t, kind, a))
			r.dropped(kind+" of A, after A→B", r.readOf(gA))

			// B → A: a new backend for the same session, whose first restore
			// is a first restore.
			r.switchTo(a2, rows...)
			first := r.m.restores
			r.up(a2)
			if slices.Contains(texts(r.m, entryNote), reloadedNote) || r.m.restores != first+1 {
				t.Fatalf("back on A the first restore was not a first restore: notes %q", texts(r.m, entryNote))
			}
			// B's item, taken by the read armed before the switch back.
			b.push(laneItem(t, kind, b))
			r.dropped(kind+" of B, after B→A", r.readOf(gB))

			// Each backend left behind is closed off the Update; its lane
			// ended, a read of it that took nothing answers nothing.
			for len(r.retires) > 0 {
				r.pop(&r.retires, "retired close")
			}
			if a.closes.Load() != 1 || b.closes.Load() != 1 || a2.closes.Load() != 0 {
				t.Fatalf("closes: a %d, b %d, a2 %d — want the two left behind closed once", a.closes.Load(), b.closes.Load(), a2.closes.Load())
			}
			if msg := runWatched(t, waitEvent(a, gA)); msg != nil {
				t.Fatalf("a read of a closed backend answered %#v", msg)
			}
			// A2's own stream goes on.
			r.stream(a2, backend.Item{Kind: backend.ItemEvent, Gen: 1,
				Event: seqd(1, agent.Event{Type: agent.EventText, Text: "back on a"})[0]})
			if r.m.foldedSeq() != 1 || r.m.quitting {
				t.Fatalf("A2's own event: folded %d, quitting %v", r.m.foldedSeq(), r.m.quitting)
			}
		})
	}
}

// TestASwitchDropsTheOldStartsAnswer: the start's answer of the backend a
// switch left — its success or its failure, answering after the switch —
// carries the backend generation it was dispatched under, and is dropped: it
// neither brings up the session that replaced it nor fails it. The answer is
// the real start command's (startCmd), over a Start its host has not answered
// when the user switches (astra r22-c11 3): a start command that forgot its
// stamp is caught here, not written into a hand-built message.
func TestASwitchDropsTheOldStartsAnswer(t *testing.T) {
	for _, failed := range []bool{false, true} {
		a, b := newLane(t, "a", "alpha"), newLane(t, "b", "bravo")
		a.startWith = make(chan error, 1)
		r := startingLaneModel(t, a, map[string][]*laneBackend{"b": {b}})
		// A's start is in flight, its host not yet answering, when the user
		// switches.
		answer := later(t, r.take(&r.starts, "A's start"))
		r.stream(a, a.restoreItem(t, 1))
		r.switchTo(b, laneRow(b, "session b", time.Minute))
		msg := startAnswer(t, a, answer, failed)
		r.dropped("A's start answer", msg)
		if r.m.started || r.m.startErr != nil || r.m.status == statusError {
			t.Fatalf("A's start answered for B: started %v, startErr %v, status %s", r.m.started, r.m.startErr, r.m.status)
		}
		r.up(b)
		if r.m.startErr != nil || r.m.status != statusIdle {
			t.Fatalf("B came up %s (%v)", r.m.status, r.m.startErr)
		}
	}
}

// startingLaneModel is laneModel with a's start left in flight: sized, its
// gated calls asynchronous, its start command waiting in the rig (r.starts),
// nothing of its stream read.
func startingLaneModel(t *testing.T, a *laneBackend, lanes map[string][]*laneBackend) *switchRig {
	t.Helper()
	isolateSkillsHome(t)
	m := New(Config{Backend: a, Theme: "tokyo-night", Workspace: a.info.Workspace, Yolo: true, Sessions: laneSessions(lanes)})
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	r := newSwitchRig(t, asyncGate(t, tm.(Model)))
	r.starts = append(r.starts, r.m.startCmd())
	return r
}

// startAnswer has lane a's host answer the start it was holding — a failure,
// or a success — and answers the message the start command then produced,
// undelivered.
func startAnswer(t *testing.T, a *laneBackend, answer func() tea.Msg, failed bool) tea.Msg {
	t.Helper()
	if failed {
		a.startWith <- errors.New("a failed to start")
	} else {
		a.startWith <- nil
	}
	msg := answer()
	switch msg.(type) {
	case errMsg:
		if !failed {
			t.Fatalf("fixture: A's start failed: %#v", msg)
		}
	case startedMsg:
		if failed {
			t.Fatalf("fixture: A's start succeeded: %#v", msg)
		}
	default:
		t.Fatalf("fixture: A's start answered %#v", msg)
	}
	return msg
}

// TestASwitchFromTheHeldQueueTakesTheOldStreamOut (§3.11, §3.12): the dial's
// answer arrives while a gate of A's is open, and is held; behind it A's
// reader — which keeps one read out while a gate is open — delivers an event,
// and a key arrives. The gate's release drains the answer first, and the
// switch is made there: A's event, still held, is another backend's and is
// taken out of the queue as the switch is made; the key stays, and is B's
// composer's once drained; B's read is armed only once nothing is held (the
// reader parks while held messages drain), and A's event is never folded.
func TestASwitchFromTheHeldQueueTakesTheOldStreamOut(t *testing.T) {
	a, b := newLane(t, "a", "alpha"), newLane(t, "b", "bravo")
	r, _ := laneModel(t, a, map[string][]*laneBackend{"b": {b}})
	gA := r.m.bgen
	r.openList(laneRow(b, "session b", time.Minute))
	r.enterOn(b)
	release := make(chan struct{})
	r.send(gateOpMsg{call: blockedCall(release, "done"), cont: noteCont("a's call")})
	if r.m.gate == nil {
		t.Fatal("fixture: no gate open on A")
	}
	r.send(r.pop(&r.opens, "dial"))
	a.push(backend.Item{Kind: backend.ItemEvent, Gen: 1, Event: seqd(1, agent.Event{Type: agent.EventText, Text: "a's words"})[0]})
	r.send(r.readOf(gA))
	r.send(runeKey('x'))
	if got := heldKinds(r.m); !slices.Equal(got, []string{"tui.sessOpenedMsg", "tui.eventMsg", "tea.KeyMsg"}) {
		t.Fatalf("fixture: held %v", got)
	}
	close(release)
	r.send(r.pop(&r.calls, "A's call"))
	if r.drains != 1 {
		t.Fatalf("the release owes %d drains", r.drains)
	}
	r.drains--
	r.send(drainMsg{})
	if r.m.eng != b {
		t.Fatal("the drained answer was not switched to")
	}
	if got := heldKinds(r.m); !slices.Equal(got, []string{"tea.KeyMsg"}) {
		t.Fatalf("after the switch the queue holds %v: A's event should be out of it, the key in it", got)
	}
	for _, rd := range r.reads {
		if rd.bgen == r.m.bgen {
			t.Fatal("B's read was armed while a held message still drains")
		}
	}
	r.drains--
	r.send(drainMsg{})
	if r.m.input.Value() != "x" || len(r.m.held) != 0 || !r.m.reading {
		t.Fatalf("after the drain: composer %q, %d held, reading %v", r.m.input.Value(), len(r.m.held), r.m.reading)
	}
	r.up(b)
	if r.m.foldedSeq() != 0 || slices.Contains(texts(r.m, entryAssistant), "a's words") {
		t.Fatalf("A's event reached B's fold (seq %d)", r.m.foldedSeq())
	}
}

// ---------------------------------------------------- replies and timeouts

// TestASwitchRejectsTheOldSessionsRepliesFirst (plan 030 §3.11, R2-5): a gated
// call of A's still running when the user switches — its gate released by the
// held queue's bound, so the switch could be made — answers after it, while
// B's own gate is open: A's reply, or its gate's timeout, is rejected before
// the gate's bookkeeping. B's gate stays open, owes no drain, acknowledges no
// pending sync token, and B's own reply releases it as ever. A reply that
// even names B's open gate is turned away the same (the order the plan fixes:
// the session generation before the gate's id); and gate ids are never reused
// across the switch.
func TestASwitchRejectsTheOldSessionsRepliesFirst(t *testing.T) {
	for _, tc := range []struct {
		name string
		// answer lets A's call answer before its reply is run, or leaves it
		// to its deadline (a timeout that lingers).
		answer bool
	}{{"a late reply", true}, {"a late timeout", false}} {
		t.Run(tc.name, func(t *testing.T) {
			a, b := newLane(t, "a", "alpha"), newLane(t, "b", "bravo")
			r, _ := laneModel(t, a, map[string][]*laneBackend{"b": {b}})

			// A's gated call: a prompt, whose Submit waits for the test — and,
			// for the timeout, whose gate gives up at once once it is run —
			// its gate released without it, so the switch can be made.
			callA, gA, sessA := r.unansweredPrompt("to a", !tc.answer)
			r.switchTo(b, laneRow(b, "session b", time.Minute))
			r.up(b)

			// B's own gate, a sync token pending under it.
			typeRunes(r, "to b")
			r.send(enter())
			gB := r.m.gate
			if gB == nil || gB.id <= gA || len(r.calls) != 1 {
				t.Fatalf("B's gate %v after A's %d: gate ids must keep counting across a switch", gB, gA)
			}
			r.send(frameSyncMsg{n: 41})
			if r.m.syncPending != 41 {
				t.Fatalf("fixture: the token is not pending (%d)", r.m.syncPending)
			}

			// A's call answers now — or its gate's deadline passes first.
			if tc.answer {
				a.answer <- engine.SubmitResult{Turn: "turn-9", Text: "to a"}
			}
			rep, ok := runWatched(t, callA).(gateReply)
			if !ok || rep.id != gA || rep.issuedUnder() != sessA {
				t.Fatalf("fixture: A's call answered %+v", rep)
			}
			if tc.answer == (rep.err != nil) || (!tc.answer && rep.linger == nil) {
				t.Fatalf("fixture: A's reply %+v, want %s", rep, tc.name)
			}
			r.dropped(tc.name+" of A's gate", rep)
			// A reply of the old session naming B's open gate is turned away
			// the same: the generation is judged before the gate's id.
			r.dropped("a reply of A's naming B's gate", gateReply{issued: issued{sessGen: sessA}, id: gB.id, result: engine.SubmitResult{Turn: "turn-1", Text: "forged"}})
			if r.m.gate != gB || r.drains != 0 || r.m.syncPending != 41 || r.m.syncAck == 41 {
				t.Fatalf("B's gate after A's replies: gate %v, drains %d, pending %d, ack %d", r.m.gate, r.drains, r.m.syncPending, r.m.syncAck)
			}
			if !tc.answer {
				// The lingering call is still waited for, where a panic of it
				// would be recovered: it ends with nothing.
				a.answer <- engine.SubmitResult{}
				if msg := r.pop(&r.lingers, "the linger's wait"); msg != nil {
					t.Fatalf("the lingering call answered %#v", msg)
				}
			}

			// B's own reply releases its gate, and the token.
			b.answer <- engine.SubmitResult{Turn: "turn-1", Text: "to b"}
			r.send(r.pop(&r.calls, "B's call"))
			if r.m.gate != nil || r.m.syncAck != 41 || r.m.status != statusWorking {
				t.Fatalf("B's reply: gate %v, ack %d, status %s", r.m.gate, r.m.syncAck, r.m.status)
			}
			if got := texts(r.m, entryUser); !slices.Equal(got, []string{"to b"}) {
				t.Fatalf("B's transcript holds %q: A's prompt followed the switch", got)
			}
		})
	}
}

// typeRunes types s into the composer, one key at a time.
func typeRunes(r *switchRig, s string) {
	for _, c := range s {
		r.send(runeKey(c))
	}
}

// unansweredPrompt sends text as a prompt from the session the rig shows and
// leaves its gated call out, unrun and unanswered — its Submit waits for the
// test (laneBackend.answer) — with its gate released by the held queue's
// bound, so a switch can be made, and the draft an unanswered prompt keeps
// taken away, so ← opens the list on an empty composer. With timeout the
// call's gate gives up at once once it is run: its reply is a timeout, the
// call lingering past it. It answers the call, its gate's id and the session
// generation it was issued under.
func (r *switchRig) unansweredPrompt(text string, timeout bool) (call tea.Cmd, gate, sessGen uint64) {
	r.t.Helper()
	prev := gateDeadline
	r.t.Cleanup(func() { gateDeadline = prev })
	if timeout {
		gateDeadline = time.Millisecond
	}
	typeRunes(r, text)
	r.send(enter())
	gateDeadline = prev
	g := r.m.gate
	if g == nil || len(r.calls) != 1 {
		r.t.Fatalf("fixture: the prompt opened no gate (%d calls)", len(r.calls))
	}
	call = r.calls[0]
	r.calls = nil
	// A burst holds the queue to its bound: the gate is released with no
	// answer, the call still out.
	r.send(hiddenRefusedMsg{issued: issued{sessGen: r.m.sessGen + 1000}, h: hiddenAnswer{id: strings.Repeat("x", heldMaxBytes)}})
	if r.m.gate != nil || r.m.copyNote != noAnswerSubmitNote {
		r.t.Fatalf("fixture: the bound left gate %v, note %q", r.m.gate, r.m.copyNote)
	}
	r.drainAll()
	r.m.input.SetValue("")
	return call, g.id, r.m.sessGen
}

// ------------------------------------------------ the read after a restore

// TestADelayedPostRestoreReadAcrossASwitch (plan 030 §3.7, R2-7, X52): the
// read of the last ending each restore sends is tagged with the backend
// generation too. Held while the model moves on — A's across a switch to B;
// B's first across a new turn and another restore of B's, and both of B's
// across a switch back to A — each lands after, reporting a failed ending,
// and is dropped: the model shows what it moved to. The one read still current
// — A2's own — applies. And an answer that matches the model's every other
// count but names another backend generation is dropped by that alone.
func TestADelayedPostRestoreReadAcrossASwitch(t *testing.T) {
	failed := &engine.LastTurn{Outcome: engine.TurnFailed, Err: "402 Payment Required", TurnID: "turn-1"}
	a, b, a2 := newLane(t, "a", "alpha"), newLane(t, "b", "bravo"), newLane(t, "a", "alpha")
	for _, l := range []*laneBackend{a, b, a2} {
		l.lastTurn.Store(failed)
	}
	r, _ := laneModel(t, a, map[string][]*laneBackend{"b": {b}, "a": {a2}})
	rows := []roster.Row{laneRow(a, "session a", time.Minute), laneRow(b, "session b", 2*time.Minute)}
	readA := r.pop(&r.lastTurns, "A's read")

	r.switchTo(b, rows...)
	r.up(b)
	readB1 := r.pop(&r.lastTurns, "B's first read")
	// A new turn on B, and another restore of it.
	r.stream(b, backend.Item{Kind: backend.ItemEvent, Gen: 1,
		Event: seqd(1, agent.Event{Type: agent.EventTurn, Turn: &agent.TurnInfo{Phase: agent.TurnStarted, ID: "turn-2", Text: "again"}})[0]})
	r.stream(b, b.restoreItem(t, 2, seqd(1,
		agent.Event{Type: agent.EventTurn, Turn: &agent.TurnInfo{Phase: agent.TurnStarted, ID: "turn-2", Text: "again"}},
		agent.Event{Type: agent.EventTurn, Turn: &agent.TurnInfo{Phase: agent.TurnEnded, ID: "turn-2"}})...))
	readB2 := r.pop(&r.lastTurns, "B's second read")
	if r.m.status == statusError {
		t.Fatal("fixture: B is failed before any read landed")
	}
	r.unapplied("A's read, after A→B", readA)
	r.unapplied("B's first read, after a new turn and another restore", readB1)

	r.switchTo(a2, rows...)
	r.up(a2)
	readA2 := r.pop(&r.lastTurns, "A2's read")
	r.unapplied("B's second read, after B→A", readB2)

	// A2's own read is current, and applies.
	msg, ok := readA2.(lastTurnMsg)
	if !ok {
		t.Fatalf("A2's read answered %#v", readA2)
	}
	// The same answer under another backend generation — every other count
	// its own — is dropped by the generation alone.
	forged := msg
	forged.issued = issued{}
	forged.tag.bgen--
	r.unapplied("an answer naming another backend generation", forged)
	r.send(msg)
	if r.m.status != statusError || r.m.err != failed.Err {
		t.Fatalf("A2's own read: %s (%q), want the failure", r.m.status, r.m.err)
	}
}

// ---------------------------------------------------------------- the dials

// TestOverlappingOpensTheLaterWins (§3.11): two opens asked for one after the
// other, both dialling: the later is the one the list waits for. Whichever
// order they answer in, the earlier's backend is closed and never adopted,
// and the later's is switched to.
func TestOverlappingOpensTheLaterWins(t *testing.T) {
	for _, laterFirst := range []bool{true, false} {
		a, b, c := newLane(t, "a", "alpha"), newLane(t, "b", "bravo"), newLane(t, "c", "charlie")
		r, fs := laneModel(t, a, map[string][]*laneBackend{"b": {b}, "c": {c}})
		r.openList(laneRow(b, "session b", time.Minute), laneRow(c, "session c", 2*time.Minute))
		r.enterOn(b)
		r.enterOn(c)
		if r.m.sessList.dialTitle != "session c" || !strings.Contains(plainView(r.m), "opening session c…") {
			t.Fatalf("the list waits for %q:\n%s", r.m.sessList.dialTitle, plainView(r.m))
		}
		dialB, dialC := r.opens[0], r.opens[1]
		r.opens = nil
		if laterFirst {
			r.send(runWatched(t, dialC))
			r.send(runWatched(t, dialB))
		} else {
			r.send(runWatched(t, dialB))
			if r.m.eng != a || !r.m.sessList.open || r.m.sessList.dialing == 0 {
				t.Fatalf("the earlier open's answer was acted on: backend %v, list open %v", r.m.eng, r.m.sessList.open)
			}
			r.send(runWatched(t, dialC))
		}
		if r.m.eng != c || r.m.sessList.open {
			t.Fatalf("the later open was not switched to (later first %v)", laterFirst)
		}
		if got := fs.opened(); len(got) != 2 {
			t.Fatalf("%d opens asked, want two", len(got))
		}
		for len(r.retires) > 0 {
			r.pop(&r.retires, "retired close")
		}
		if b.closes.Load() != 1 || c.closes.Load() != 0 || a.closes.Load() != 1 {
			t.Fatalf("closes: b %d, c %d, a %d — want the earlier open's and the session left closed", b.closes.Load(), c.closes.Load(), a.closes.Load())
		}
	}
}

// TestAnOpenAnsweredAfterTheUserMovedOnIsClosed (§3.11): a dial that answers
// once the user has moved on — back to the session behind the list, the list
// left and opened again, craze quitting — is closed and never adopted; a dial
// that fails leaves its error on the hint line and the list where it was.
func TestAnOpenAnsweredAfterTheUserMovedOnIsClosed(t *testing.T) {
	for _, tc := range []struct {
		name    string
		moveOn  func(r *switchRig)
		stayOn  bool
		listUp  bool
		quitted bool
	}{
		{"back to the session behind", func(r *switchRig) { r.send(tea.KeyMsg{Type: tea.KeyEsc}) }, true, false, false},
		{"the list left and opened again", func(r *switchRig) {
			r.send(tea.KeyMsg{Type: tea.KeyEsc})
			r.send(tea.KeyMsg{Type: tea.KeyLeft})
		}, true, true, false},
		{"craze quitting", func(r *switchRig) { r.send(tea.KeyMsg{Type: tea.KeyCtrlD}) }, true, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, b := newLane(t, "a", "alpha"), newLane(t, "b", "bravo")
			r, _ := laneModel(t, a, map[string][]*laneBackend{"b": {b}})
			r.openList(laneRow(b, "session b", time.Minute))
			r.enterOn(b)
			dial := r.opens[0]
			r.opens = nil
			tc.moveOn(r)
			r.send(runWatched(t, dial))
			if r.m.eng != a || r.m.sessList.open != tc.listUp || r.m.quitting != tc.quitted {
				t.Fatalf("after the late answer: backend a %v, list open %v, quitting %v", r.m.eng == a, r.m.sessList.open, r.m.quitting)
			}
			r.pop(&r.retires, "the late backend's close")
			if b.closes.Load() != 1 || a.closes.Load() != 0 {
				t.Fatalf("closes: b %d, a %d — want the late answer closed and the session kept", b.closes.Load(), a.closes.Load())
			}
		})
	}

	t.Run("a failed dial", func(t *testing.T) {
		a, b := newLane(t, "a", "alpha"), newLane(t, "b", "bravo")
		r, _ := laneModel(t, a, map[string][]*laneBackend{})
		r.openList(laneRow(b, "session b", time.Minute))
		r.enterOn(b)
		r.send(r.pop(&r.opens, "dial"))
		if r.m.eng != a || !r.m.sessList.open || r.m.sessList.dialing != 0 ||
			!strings.Contains(plainView(r.m), "could not open session b: no such session") {
			t.Fatalf("a failed dial:\n%s", plainView(r.m))
		}
	})
}

// ------------------------------------------------------- a session's end

// TestAViewedSessionEndingReturnsToTheList (§3.10, AC9): the session the TUI
// shows ends on its host — stopped by another client, the list's ctrl+x, an
// idle exit — and the TUI goes back to the list, not the shell: the row
// marked ended, the hint line saying so, craze still running. The row stays
// ended while its host is still listed (as it closes) and once it has left
// the registry (X94): esc stays on the list and says so, enter on the row
// opens nothing, ctrl+x on it does nothing. Without a session list it quits
// as it always has, and so it does when this client's own quit asked for the
// end. (Its connection lost is not its end:
// TestALostConnectionLeavesTheRowToTheRoster.)
func TestAViewedSessionEndingReturnsToTheList(t *testing.T) {
	t.Run("the session's own end", func(t *testing.T) {
		a, b := newLane(t, "a", "alpha"), newLane(t, "b", "bravo")
		r, fs := laneModel(t, a, map[string][]*laneBackend{"b": {b}})
		bRow := laneRow(b, "session b", time.Minute)
		r.switchTo(b, bRow)
		r.up(b)
		rosters, _, _ := fs.calls()
		r.stream(b, backend.Item{Kind: backend.ItemEnd})
		if r.m.quitting || !r.m.ended || !r.m.sessList.open || !r.m.sessList.hereEnded || r.m.sessList.hereLost {
			t.Fatalf("the end: quitting %v, ended %v, list open %v, marked %v, lost %v",
				r.m.quitting, r.m.ended, r.m.sessList.open, r.m.sessList.hereEnded, r.m.sessList.hereLost)
		}
		if n, _, _ := fs.calls(); n != rosters+1 {
			t.Fatalf("the list opened on no roster of its own (%d rosters, was %d)", n, rosters)
		}
		if r.m.sessList.note != sessEndedNote {
			t.Fatalf("the hint line says %q, want %q", r.m.sessList.note, sessEndedNote)
		}
		for _, snap := range []roster.Snapshot{{Running: []roster.Row{bRow}}, {}} {
			r.send(sessSnapMsg{gen: r.m.sessList.gen, snap: snap})
			row, ok := sessFind(r.m.sessLines(), r.m.sessList.here)
			if !ok || !row.ended || !row.here || row.key.id != "b" || row.closable() || row.cancellable() {
				t.Fatalf("the ended session's row, its host listed %v: %+v (listed %v)", len(snap.Running) > 0, row, ok)
			}
			for _, k := range []tea.KeyMsg{{Type: tea.KeyEsc}, {Type: tea.KeyLeft}, enter(), {Type: tea.KeyCtrlX}} {
				r.send(k)
				if !r.m.sessList.open || len(r.opens) != 0 || !r.m.sessList.armed.zero() {
					t.Fatalf("%v on the ended row: open %v, %d opens asked for, armed %+v",
						k, r.m.sessList.open, len(r.opens), r.m.sessList.armed)
				}
				if k.Type != tea.KeyCtrlX && r.m.sessList.note != sessEndedNote {
					t.Fatalf("%v on the ended row: the hint line says %q", k, r.m.sessList.note)
				}
			}
		}
		if got := fs.opened(); len(got) != 1 {
			t.Fatalf("Open was asked for %+v after the end, want only the switch to B", got)
		}
		if _, cancels, stops := fs.calls(); cancels != 0 || stops != 0 {
			t.Fatalf("ctrl+x on the ended row: %d cancels, %d stops", cancels, stops)
		}
	})

	t.Run("without a session list", func(t *testing.T) {
		m := sized(t)
		tm, cmd := m.Update(endMsg{})
		if m = tm.(Model); !m.quitting || runCmd(cmd) != (tea.QuitMsg{}) {
			t.Fatal("the end without a session list did not quit")
		}
	})

	t.Run("this client's own quit", func(t *testing.T) {
		a := newLane(t, "a", "alpha")
		r, _ := laneModel(t, a, map[string][]*laneBackend{})
		r.m.quitting = true
		r.stream(a, backend.Item{Kind: backend.ItemEnd})
		if r.m.sessList.open || !r.m.quitting {
			t.Fatal("the end a quit asked for went back to the list")
		}
	})
}

// TestALostConnectionLeavesTheRowToTheRoster (C12r2, r27-pr2 1): the stream
// of the session the TUI shows ends for its transport — its re-attaches
// spent, say — while its host serves the session still. The TUI goes back to
// the list as for the session's own end, the cursor on its row, but only the
// connection has ended: the hint line says it was lost, and why; nothing is
// behind the list any more (esc and ← stay on it and say so); and the row is
// the roster's like any other's, never `· ended` and no longer `· here`: idle
// while its host answers idle (ctrl+x arms a close, a second sends it),
// working while it works (ctrl+x stops its turn), unreachable while it does
// not answer (enter says so, without a dial) — and enter on it, answering
// again, opens it afresh (Sessions.Open): a new backend adopted as any switch
// adopts one, the lost one closed, the draft left in the composer back in it.
// The same when the connection is lost behind the list.
func TestALostConnectionLeavesTheRowToTheRoster(t *testing.T) {
	lost := errors.New("the socket went away")
	for _, behind := range []bool{false, true} {
		name := "the session shown"
		if behind {
			name = "the session behind the list"
		}
		t.Run(name, func(t *testing.T) {
			a, a2 := newLane(t, "a", "alpha"), newLane(t, "a", "alpha")
			r, fs := laneModel(t, a, map[string][]*laneBackend{"a": {a2}})
			r.m.clock = func() time.Time { return sessNow }
			key := sessKey{id: "a", inc: "inc-a"}
			row := func(status roster.Status, activity engine.Activity) roster.Row {
				rw := answeredInc("a", "inc-a", "session a", "grok", a.info.Workspace, time.Minute, func(s *roster.Session) {
					s.Activity = activity
					if activity == engine.ActivityWorking {
						s.Doing = engine.DoingThinking
					}
				})
				rw.Status = status
				return rw
			}
			draft := ""
			if behind {
				r.openList(row(roster.Reachable, engine.ActivityIdle))
			} else {
				draft = "half a thought"
				typeRunes(r, draft)
			}
			r.stream(a, backend.Item{Kind: backend.ItemEnd, Err: lost})
			l := r.m.sessList
			if r.m.quitting || !r.m.ended || !l.open || !l.hereLost || l.hereEnded || !l.here.zero() || l.sel != key {
				t.Fatalf("the loss: quitting %v, ended %v, list open %v, lost %v, marked ended %v, here %+v, selected %+v",
					r.m.quitting, r.m.ended, l.open, l.hereLost, l.hereEnded, l.here, l.sel)
			}
			if want := sessLostNote + ": the socket went away"; l.note != want {
				t.Fatalf("the hint line says %q, want %q", l.note, want)
			}

			// Idle on its host: an ordinary idle row, closed by ctrl+x twice.
			r.send(sessSnapMsg{gen: r.m.sessList.gen, snap: roster.Snapshot{Running: []roster.Row{row(roster.Reachable, engine.ActivityIdle)}}})
			got, ok := sessFind(r.m.sessLines(), key)
			if !ok || got.ended || got.here || got.state != sessIdle || !got.closable() {
				t.Fatalf("the row of the session whose connection was lost: %+v (listed %v)", got, ok)
			}
			// The note lasts until a key: ↓ (one row, so it stays selected)
			// shows what the row takes.
			r.send(tea.KeyMsg{Type: tea.KeyDown})
			view := plainView(r.m)
			if hint := plain(r.m.sessHintRow(r.m.sessLines())); r.m.sessList.sel != key || !strings.Contains(hint, "enter open") ||
				!strings.Contains(hint, "ctrl+x close") || strings.Contains(view, "ended") || strings.Contains(view, "· here") ||
				strings.Contains(view, sessEmptyNote) {
				t.Fatalf("the list after the loss (hint %q):\n%s", hint, view)
			}
			for _, k := range []tea.KeyMsg{{Type: tea.KeyEsc}, {Type: tea.KeyLeft}} {
				r.send(k)
				if !r.m.sessList.open || r.m.sessList.note != sessLostNote || len(r.opens) != 0 {
					t.Fatalf("%v after the loss: open %v, note %q, %d opens", k, r.m.sessList.open, r.m.sessList.note, len(r.opens))
				}
			}
			r.send(tea.KeyMsg{Type: tea.KeyCtrlX})
			if r.m.sessList.armed != key {
				t.Fatalf("ctrl+x on the idle row armed %+v", r.m.sessList.armed)
			}
			r.send(tea.KeyMsg{Type: tea.KeyCtrlX})
			calls := namedCmds(r.last, "sessAction")
			if len(calls) != 1 {
				t.Fatal("the second ctrl+x sent no close")
			}
			r.send(runWatched(t, calls[0]))
			if _, _, stops := fs.calls(); stops != 1 || fs.stops[0].Host.ID != "host-a" || r.m.sessList.note != "closed: session a" {
				t.Fatalf("the close: %d stops (%+v), note %q", stops, fs.stops, r.m.sessList.note)
			}

			// Working on its host: ctrl+x stops its turn.
			r.send(sessSnapMsg{gen: r.m.sessList.gen, snap: roster.Snapshot{Running: []roster.Row{row(roster.Reachable, engine.ActivityWorking)}}})
			r.send(tea.KeyMsg{Type: tea.KeyCtrlX})
			calls = namedCmds(r.last, "sessAction")
			if len(calls) != 1 {
				t.Fatal("ctrl+x on the working row sent nothing")
			}
			r.send(runWatched(t, calls[0]))
			if _, cancels, _ := fs.calls(); cancels != 1 || fs.cancels[0].Host.ID != "host-a" || r.m.sessList.note != "stopped: session a" {
				t.Fatalf("the cancel: %d cancels (%+v), note %q", cancels, fs.cancels, r.m.sessList.note)
			}

			// Not answering: said so, and not dialled.
			r.send(sessSnapMsg{gen: r.m.sessList.gen, snap: roster.Snapshot{Running: []roster.Row{row(roster.Unreachable, engine.ActivityIdle)}}})
			r.send(enter())
			if r.m.sessList.note != sessUnreachNote || len(r.opens) != 0 {
				t.Fatalf("enter on the unreachable row: note %q, %d opens", r.m.sessList.note, len(r.opens))
			}

			// Answering again: enter opens it afresh.
			r.send(sessSnapMsg{gen: r.m.sessList.gen, snap: roster.Snapshot{Running: []roster.Row{row(roster.Reachable, engine.ActivityIdle)}}})
			r.enterOn(a2)
			r.send(r.pop(&r.opens, "dial"))
			if r.m.eng != a2 || r.m.sessList.open || r.m.ended || r.m.input.Value() != draft {
				t.Fatalf("enter on the row: shows a2 %v, list open %v, ended %v, composer %q (want %q)",
					r.m.eng == a2, r.m.sessList.open, r.m.ended, r.m.input.Value(), draft)
			}
			if got := fs.opened(); len(got) != 1 || got[0].Host.ID != "host-a" {
				t.Fatalf("Open was asked for %+v, want the lost session's host once", got)
			}
			runWatched(t, r.take(&r.retires, "the lost backend's close"))
			if a.closes.Load() != 1 {
				t.Fatalf("the lost backend was closed %d times, want once", a.closes.Load())
			}
			r.up(a2)
		})
	}
}

// ------------------------------------------------------------- the drafts

// TestDraftsDoNotFollowYou (§3.11, AC9): the composer's text belongs to the
// session it was written for. A's draft — left in the composer when A ended
// and the TUI went back to the list — does not follow the user to B; B's own
// draft stays B's; and back on A (a new backend for the same session) A's is
// in the composer again, and on B again B's.
func TestDraftsDoNotFollowYou(t *testing.T) {
	a, b, a2, b2 := newLane(t, "a", "alpha"), newLane(t, "b", "bravo"), newLane(t, "a", "alpha"), newLane(t, "b", "bravo")
	r, _ := laneModel(t, a, map[string][]*laneBackend{"b": {b, b2}, "a": {a2}})
	rows := []roster.Row{laneRow(a, "session a", time.Minute), laneRow(b, "session b", 2*time.Minute)}
	typeRunes(r, "half a thought")
	r.stream(a, backend.Item{Kind: backend.ItemEnd})
	if !r.m.sessList.open {
		t.Fatal("fixture: A's end did not go back to the list")
	}
	r.send(sessSnapMsg{gen: r.m.sessList.gen, snap: roster.Snapshot{Running: rows[1:]}})
	r.enterOn(b)
	r.send(r.pop(&r.opens, "dial"))
	if got := r.m.input.Value(); got != "" {
		t.Fatalf("A's draft followed the switch to B: %q", got)
	}
	r.up(b)
	typeRunes(r, "b's own")
	r.stream(b, backend.Item{Kind: backend.ItemEnd})
	r.send(sessSnapMsg{gen: r.m.sessList.gen, snap: roster.Snapshot{Running: rows}})
	r.enterOn(a2)
	r.send(r.pop(&r.opens, "dial"))
	if got := r.m.input.Value(); got != "half a thought" {
		t.Fatalf("back on A the composer holds %q, want A's draft", got)
	}
	if _, kept := r.m.drafts["a"]; kept {
		t.Fatal("A's draft was put back and also kept in the stash")
	}
	r.up(a2)
	// ← needs an empty composer: the draft is sent away first.
	r.m.input.SetValue("")
	r.switchTo(b2, rows...)
	if got := r.m.input.Value(); got != "b's own" {
		t.Fatalf("back on B the composer holds %q, want B's draft", got)
	}
}

// ---------------------------------------------------------------- the band

// TestTheBandHasNoRowsUntilTheListOpens (§3.11, §3.17): with a session list
// and the list never opened, the band has no rows and the frame is the frame
// without a session list, byte for byte — every existing golden's contract.
// Once the list has opened, every session's frame carries it: one row at the
// top, the session's title, provider and directory and `← sessions`, the
// transcript one row shorter; degradation's last step and fitChrome give it
// up; every width draws it inside the row.
func TestTheBandHasNoRowsUntilTheListOpens(t *testing.T) {
	isolateSkillsHome(t)
	ws := frameWorkspace(t)
	build := func(s Sessions) Model {
		stub := NewStub()
		t.Cleanup(func() { _ = stub.Close() })
		m := New(Config{Session: stub, Theme: "tokyo-night", Workspace: ws, Model: "grok", Yolo: true, Sessions: s})
		tm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
		return startedLikeInit(t, tm.(Model))
	}
	with, without := build(&fakeSessions{}), build(nil)
	if with.bandRows() != 0 || !with.lay.Region(regionBand).Empty() {
		t.Fatalf("the band has rows before the list opened: %+v", with.lay.Region(regionBand))
	}
	if a, b := plainView(with), plainView(without); a != b {
		t.Fatalf("a session list moved a frame before the list opened (%s)", frameLineDiff(b, a))
	}

	opened, _ := press(with, tea.KeyMsg{Type: tea.KeyLeft})
	opened, _ = press(opened, tea.KeyMsg{Type: tea.KeyEsc})
	if opened.sessList.open || !opened.bandOn {
		t.Fatalf("fixture: list open %v, band on %v", opened.sessList.open, opened.bandOn)
	}
	band := opened.lay.Region(regionBand)
	if band.Top != 0 || band.Height() != 1 {
		t.Fatalf("the band is %+v, want the first row", band)
	}
	if got, was := opened.lay.Region(regionTranscript).Height(), without.lay.Region(regionTranscript).Height(); got != was-1 {
		t.Fatalf("the transcript has %d rows with the band, %d without: the band's row is not counted in the chrome", got, was)
	}
	top := strings.Split(plainView(opened), "\n")[0]
	if !strings.HasPrefix(top, "─ new session · cursor · ws ") || !strings.HasSuffix(top, bandBack+" ─") {
		t.Fatalf("the band reads %q", top)
	}

	s := frameSizes{band: 1, input: 1, status: statusRows, modal: 3}
	if degrade(s, 7).band != 1 || degrade(s, 8).band != 0 {
		t.Fatal("the band is given up by a step other than degradation's last")
	}
	if fitChrome(s, s.chrome()-1).band != 0 {
		t.Fatal("fitChrome did not give the band up first")
	}

	for w := minFrameCols; w <= 120; w++ {
		m := opened
		m.width = w
		row := m.bandView()
		if got := lipgloss.Width(row); got != w {
			t.Fatalf("at %d columns the band is %d wide: %q", w, got, plain(row))
		}
		if w >= 60 && !strings.Contains(plain(row), bandBack) {
			t.Fatalf("at %d columns the band dropped its hint: %q", w, plain(row))
		}
	}
	for _, size := range []struct{ w, h int }{{minFrameCols, minFrameRows}, {80, 24}} {
		m := applyMsg(t, opened, tea.WindowSizeMsg{Width: size.w, Height: size.h})
		if got := lipgloss.Height(plainView(m)); got != size.h {
			t.Fatalf("at %dx%d the frame is %d rows", size.w, size.h, got)
		}
	}
}

// TestTheSubAgentHintNamesItsKey (§3.11): with a session list ← is the list's
// key, so status row 2's marker names ↓, which reaches the rows; without one it
// reads ← as it always has.
func TestTheSubAgentHintNamesItsKey(t *testing.T) {
	running := []agent.SubagentInfo{{ID: "sa-1", SubagentType: "task", Status: agent.SubagentRunning}}
	for _, tc := range []struct {
		sessions Sessions
		want     string
	}{{nil, "← 1 agent"}, {&fakeSessions{}, "↓ 1 agent"}} {
		m := Model{sessions: tc.sessions}
		m.snap.Subagents = running
		m.snap.Provider = agent.GrokProvider().Info()
		if got := m.agentCount(); got != tc.want {
			t.Fatalf("with sessions %v the marker reads %q, want %q", tc.sessions != nil, got, tc.want)
		}
	}
}

// ------------------------------------------------------- the host's reports

// TestTheHostReportsTheSessionShown (§3.7): while the list is up the host
// status is the session the list was opened from — it is still the model's —
// and once another session is opened in place, and up, it is that one's.
func TestTheHostReportsTheSessionShown(t *testing.T) {
	a, b := newLane(t, "a", "alpha"), newLane(t, "b", "bravo")
	a.info.ProviderSessionID, b.info.ProviderSessionID = "prov-a", "prov-b"
	isolateSkillsHome(t)
	rec := &recHost{}
	m := New(Config{Backend: a, Theme: "tokyo-night", Workspace: a.info.Workspace, Yolo: true, Host: rec,
		Sessions: laneSessions(map[string][]*laneBackend{"b": {b}})})
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	r := newSwitchRig(t, asyncGate(t, tm.(Model)))
	r.starts = append(r.starts, r.m.startCmd())
	r.up(a)
	last := func() string {
		if len(rec.statuses) == 0 {
			return ""
		}
		return rec.statuses[len(rec.statuses)-1].SessionID
	}
	if last() != "prov-a" {
		t.Fatalf("fixture: the host reports %q", last())
	}
	r.openList(laneRow(b, "session b", time.Minute))
	if last() != "prov-a" {
		t.Fatalf("with the list up the host reports %q, want the session behind it", last())
	}
	r.enterOn(b)
	r.send(r.pop(&r.opens, "dial"))
	r.up(b)
	if last() != "prov-b" {
		t.Fatalf("after the switch the host reports %q, want the session shown", last())
	}
}

// ------------------------------------------------------------ the retired

// TestFinishRunClosesARetiredBackend (§3.11): the close of a backend a switch
// let go of is a command, which an exit the program never ran it past — a
// signal, a panic — leaves unrun; finishRun closes it, through the set every
// copy of the model shares.
func TestFinishRunClosesARetiredBackend(t *testing.T) {
	a, b := newLane(t, "a", "alpha"), newLane(t, "b", "bravo")
	r, _ := laneModel(t, a, map[string][]*laneBackend{"b": {b}})
	initial := r.m
	r.switchTo(b, laneRow(b, "session b", time.Minute))
	if len(r.retires) != 1 || a.closes.Load() != 0 {
		t.Fatalf("fixture: %d retired closes, a closed %d", len(r.retires), a.closes.Load())
	}
	_, _ = finishRun(io.Discard, nil, initial, nil)
	if a.closes.Load() != 1 || b.closes.Load() != 1 {
		t.Fatalf("after finishRun: a closed %d, b %d — want both", a.closes.Load(), b.closes.Load())
	}
}

// ------------------------------------------------------ the constructor

// sessionFields and tuiFields are every Model field, each decided once: the
// session's, made afresh by the shared constructor for every session New or a
// switch starts (withSession), or the TUI's, carried across every session it
// shows (plan 030 §3.11). A field added to Model fails this test until it is
// named in one of them.
var (
	tuiFields = []string{
		"theme", "vp", "input", "yolo", "width", "height", "ready", "expanded", "quitting", "mouseEnabled",
		"lay", "layouts", "mouseAll", "tickGen", "tickLive", "tickFast", "spinFrame", "hiddenRetryLive",
		"clock", "paintEveryEvent", "frozen", "frameStart", "terminalTitle", "lastTitle", "term", "shell",
		"owner", "exit",
		"host", "lastHost", "viewer", "sessionIndex", "crazeID", "resume", "loadSession", "claimSession",
		"refuseLoad", "onEngine", "providerLocked", "persistProvider", "fallbackDefault", "pickedExplicit",
		"providerDefault", "providers", "newSession", "spawnNew", "spawnLoad", "cont", "sessions", "sessList",
		"sessRosters", "bandOn", "drafts", "retired", "sessGen", "bgen", "shownGen", "gateSeq",
		"resumeAttempt", "spawnSeq", "restores", "turnStarts", "held", "heldBytes", "heldDrained", "syncAck",
		"syncPending", "gateSync", "harnessQuit", "completeLoads", "unstartedSeq", "sessPick",
		"connSeq", "nativeDir", "nativeEnv", "composerAt",
	}
	sessionFields = []string{
		"eng", "cmdSeq", "chains", "engErr", "cwd", "model", "status", "err", "startErr", "startInc", "git",
		"branch", "sessStart", "hostPerm", "hostStart", "shared", "foldIn", "main", "subs", "viewing",
		"tombstone", "started", "replaying", "replayFolded", "paintNow", "sel", "pressed", "clickPos", "clickAt",
		"clicks", "copyNote",
		"copyUntil", "cards", "cardMask", "cardMasking", "hiddenRetry", "snap", "queue",
		"sendNowArmed", "ov", "modeInFlight", "modeGen", "modeRev", "modelRev", "configRev", "dialog", "mdlg",
		"helpTop", "applyGen", "themeSel", "themeNames", "themePrev", "slashSel", "slashTop", "slashKey",
		"slashHideKey", "skills", "pickingProvider", "pickingResume", "resumeCursor", "resumeWaiting",
		"resumeErr", "providerCursor", "providerErr", "spawnWaiting", "spawnFrom", "todoPlanned", "todoDone",
		"agentSel", "agentID", "agentStart", "agentDone", "agentFocus", "queueSel", "queueID", "queueFocus",
		"queueHov", "queueEdit", "queueEditPos", "editDraft", "queueEditCtx", "queueEditVer", "confirm",
		"tasksState", "todosSeen", "todosClosedAt", "turnSeq", "sawAssistantSeq", "planApprovedSeq",
		"planOfferSeq", "planDeadSeq", "offerGen", "turnID", "ownTurn", "nextTurn", "armedDraft", "disarmed",
		"turnStart", "lastThought", "ctrlCDeadline", "shellCtx", "remote", "cancelled", "prompted",
		"sessProvider", "foreignEnded", "foreignNoted", "gate", "reading", "ended", "endErr", "infoPin",
		"upDone", "indexTitle", "unstarted", "first", "cdlg",
	}
)

// TestTheSessionConstructorDecidesEveryField (§3.11): every Model field is
// the session's or the TUI's, never both and never neither; and the shared
// constructor honours it. Given a model with every field it can fill filled —
// every scalar, string, pointer, map, slice and struct of it set to something
// no fresh model holds — withSession carries each of the TUI's fields exactly
// as it was, and makes each of the session's exactly as it makes them for a
// zero model: nothing of the last session survives it, a field the list
// forgot included.
func TestTheSessionConstructorDecidesEveryField(t *testing.T) {
	ty := reflect.TypeFor[Model]()
	all := map[string]bool{}
	for i := range ty.NumField() {
		all[ty.Field(i).Name] = true
	}
	decided := map[string]string{}
	for _, list := range []struct {
		name   string
		fields []string
	}{{"the TUI's", tuiFields}, {"the session's", sessionFields}} {
		for _, f := range list.fields {
			if !all[f] {
				t.Errorf("%s names %q, which Model has no field of", list.name, f)
			}
			if prev, dup := decided[f]; dup {
				t.Errorf("%q is both %s and %s", f, prev, list.name)
			}
			decided[f] = list.name
		}
	}
	for f := range all {
		if _, ok := decided[f]; !ok {
			t.Errorf("Model's field %q is neither the TUI's nor the session's: decide it (switch.go's withSession, and this test's lists)", f)
		}
	}

	seed := sessionSeed{workspace: t.TempDir(), model: "m", provider: "grok", loading: true}
	var busy Model
	fillEvery(reflect.ValueOf(&busy).Elem(), 0)
	fromBusy, fromZero := busy.withSession(seed), Model{}.withSession(seed)
	field := func(m *Model, name string) any {
		v := reflect.ValueOf(m).Elem().FieldByName(name)
		return reflect.NewAt(v.Type(), unsafe.Pointer(v.UnsafeAddr())).Elem().Interface()
	}
	for _, f := range tuiFields {
		if !reflect.DeepEqual(field(&fromBusy, f), field(&busy, f)) {
			t.Errorf("the TUI's field %q was not carried across the constructor", f)
		}
	}
	for _, f := range sessionFields {
		if !reflect.DeepEqual(field(&fromBusy, f), field(&fromZero, f)) {
			t.Errorf("the session's field %q survived the constructor: %v", f, field(&fromBusy, f))
		}
	}
}

// fillEvery sets v — addressable — to something a fresh value never holds:
// every scalar non-zero, every string non-empty, every nil pointer, map, slice
// and chan made, every struct's fields filled a few levels down. Interfaces and
// funcs are left alone: nothing of theirs can be made here.
func fillEvery(v reflect.Value, depth int) {
	if !v.CanSet() {
		v = reflect.NewAt(v.Type(), unsafe.Pointer(v.UnsafeAddr())).Elem()
	}
	switch v.Kind() {
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(7)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		v.SetUint(7)
	case reflect.Float32, reflect.Float64:
		v.SetFloat(7)
	case reflect.String:
		v.SetString("filled")
	case reflect.Pointer:
		if v.IsNil() {
			v.Set(reflect.New(v.Type().Elem()))
		}
	case reflect.Map:
		v.Set(reflect.MakeMap(v.Type()))
	case reflect.Slice:
		v.Set(reflect.MakeSlice(v.Type(), 1, 1))
	case reflect.Chan:
		v.Set(reflect.MakeChan(v.Type(), 0))
	case reflect.Array:
		if depth < 3 {
			for i := range v.Len() {
				fillEvery(v.Index(i), depth+1)
			}
		}
	case reflect.Struct:
		if depth < 3 {
			for i := range v.NumField() {
				fillEvery(v.Field(i), depth+1)
			}
		}
	}
}
