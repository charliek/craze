package tui

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/backend"
)

// The command gate's own tests (plan 027 §3.12 (d), X34): the mechanism, over
// gated calls of the tests' own and over /rename, its one production site in
// C17. The site-specific schedules land with their sites (C18a–c).

// asyncGate is m with its gated calls asynchronous. A unit fixture runs them
// inline (TestMain pins gateSyncDefault), so a test that exercises the gate
// itself says so.
func asyncGate(t *testing.T, m Model) Model {
	t.Helper()
	m.gateSync = false
	return m
}

// gateOpMsg is a test-only message withGateOps turns into a gated call, so the
// gate's tests open gates of their own — a call and a continuation they
// choose — through the production gate (Model.gated) exactly.
type gateOpMsg struct {
	deadline time.Duration
	call     gateCall
	cont     gateCont
}

// withGateOps is the production handler, plus gateOpMsg.
func withGateOps(m Model, msg tea.Msg) (tea.Model, tea.Cmd) {
	if op, ok := msg.(gateOpMsg); ok {
		d := op.deadline
		if d == 0 {
			d = gateDeadline
		}
		return m.run(d, op.call, op.cont)
	}
	return m.update(msg)
}

// blockedCall is a gated call that answers result once release is closed, and
// never before: the gate stays open until the test says so.
func blockedCall(release <-chan struct{}, result any) gateCall {
	return func(context.Context, backend.Backend) (any, error) {
		<-release
		return result, nil
	}
}

// noteCont is a continuation that draws what its reply said, so a test can
// see it ran, and when.
func noteCont(label string) gateCont {
	return func(m Model, r gateReply) (Model, tea.Cmd) {
		m.addNote(fmt.Sprintf("%s: %v %v", label, r.result, r.err))
		return m, nil
	}
}

// gateRig drives a model through the production gate the way bubbletea would,
// one message per Update, with every command the model hands back sorted: the
// stream's reads, the gated calls, the drains, and the rest. Nothing runs on
// its own — the test says when a read lands, when a call answers, when a drain
// is applied — so each schedule is exactly the one written down. The rig fails
// the test the moment two reads of the stream are outstanding at once.
type gateRig struct {
	t        *testing.T
	m        Model
	handle   handler
	reads    []tea.Cmd
	maxReads int
	calls    []tea.Cmd
	drains   int
	// lingers are the waits for calls a gate stopped waiting for at their
	// deadline (lingerOn), not run until a test runs one.
	lingers []tea.Cmd
}

// newGateRig takes over m with the one read Init armed outstanding, as New
// recorded it.
func newGateRig(t *testing.T, m Model) *gateRig {
	t.Helper()
	r := &gateRig{t: t, m: m, handle: withGateOps}
	if m.reading {
		r.reads = append(r.reads, waitEvent(m.eng))
		r.maxReads = 1
	}
	return r
}

func (r *gateRig) send(msg tea.Msg) {
	r.t.Helper()
	tm, cmd := r.m.gated(msg, r.handle)
	r.m = tm.(Model)
	r.sort(cmd)
}

const (
	tuiPkg = "github.com/charliek/craze/internal/tui."
	teaPkg = "github.com/charmbracelet/bubbletea."
)

func (r *gateRig) sort(cmd tea.Cmd) {
	r.t.Helper()
	if cmd == nil {
		return
	}
	switch name := cmdFuncName(cmd); {
	case strings.HasPrefix(name, teaPkg+"compactCmds"), strings.HasPrefix(name, teaPkg+"Batch"):
		// Running a batch's own closure only hands back its members.
		if b, ok := cmd().(tea.BatchMsg); ok {
			for _, c := range b {
				r.sort(c)
			}
		}
	case strings.HasPrefix(name, tuiPkg+"waitEvent"):
		r.reads = append(r.reads, cmd)
		r.maxReads = max(r.maxReads, len(r.reads))
		if len(r.reads) > 1 {
			r.t.Fatalf("%d reads of the stream are outstanding at once: exactly one ever may be (§3.12)", len(r.reads))
		}
	case name == tuiPkg+"drainNext":
		r.drains++
	case strings.HasPrefix(name, tuiPkg+"Model.run"):
		r.calls = append(r.calls, cmd)
	case strings.HasPrefix(name, tuiPkg+"lingerOn"):
		r.lingers = append(r.lingers, cmd)
	}
	// Anything else — a timer above all — is never run: the tests deliver
	// their own ticks.
}

// read lands the outstanding read: it blocks until the stream has an item,
// and hands the model what it read.
func (r *gateRig) read() agent.Event {
	r.t.Helper()
	if len(r.reads) == 0 {
		r.t.Fatal("no read of the stream is outstanding")
	}
	cmd := r.reads[0]
	r.reads = r.reads[1:]
	msg := runWatched(r.t, cmd)
	ev, ok := msg.(eventMsg)
	if !ok {
		r.t.Fatalf("the read answered %#v, want an event", msg)
	}
	r.send(ev)
	return ev.ev
}

// answer runs the oldest gated call and hands the model its reply.
func (r *gateRig) answer() gateReply {
	r.t.Helper()
	if len(r.calls) == 0 {
		r.t.Fatal("no gated call is waiting to answer")
	}
	cmd := r.calls[0]
	r.calls = r.calls[1:]
	rep, ok := runWatched(r.t, cmd).(gateReply)
	if !ok {
		r.t.Fatal("a gated call answered something that is not its reply")
	}
	r.send(rep)
	return rep
}

// drain applies one owed drain.
func (r *gateRig) drain() {
	r.t.Helper()
	if r.drains == 0 {
		r.t.Fatal("no drain is owed")
	}
	r.drains--
	r.send(drainMsg{})
}

// drainAll applies drains until none is owed.
func (r *gateRig) drainAll() {
	r.t.Helper()
	for r.drains > 0 {
		r.drain()
	}
}

// runWatched runs a command on a goroutine and fails the test if it has not
// answered within the watchdog: a gated call whose deadline is not honoured,
// or a read of an empty stream, is a readable failure rather than a hung run.
func runWatched(t *testing.T, cmd tea.Cmd) tea.Msg {
	t.Helper()
	out := make(chan tea.Msg, 1)
	go func() { out <- cmd() }()
	select {
	case msg := <-out:
		return msg
	case <-time.After(pumpWatchdog):
		t.Fatalf("a command did not answer in %s", pumpWatchdog)
		return nil
	}
}

// heldKinds is the held queue's messages' types, in order.
func heldKinds(m Model) []string {
	out := make([]string, len(m.held))
	for i, h := range m.held {
		out[i] = fmt.Sprintf("%T", h.msg)
	}
	return out
}

// gatedModel is a started Stub model, sized, running its gated calls
// asynchronously.
func gatedModel(t *testing.T) (Model, *Stub) {
	t.Helper()
	isolateSkillsHome(t)
	stub := NewStub()
	t.Cleanup(func() { _ = stub.Close() })
	// A fixed workspace basename, so two models' frames can be compared.
	m := asyncGate(t, startStub(t, stub, frameWorkspace(t), 80, 24))
	return m, stub
}

// TestTheGateHoldsEveryMessageKind: while a gate is open every kind of message
// that reaches Update is held, in arrival order, and changes nothing — a key,
// the mouse, both pastes, a resize, a tick, an event, the start's answer, a
// settings reply, the hidden-answer beat — and so is the frame harness's sync
// token, arriving behind them (C17c): not acknowledged at the release, which
// applies the reply alone, but in its own turn as the held messages drain,
// each in its own Update, in order.
func TestTheGateHoldsEveryMessageKind(t *testing.T) {
	m, stub := gatedModel(t)
	r := newGateRig(t, m)
	release := make(chan struct{})
	r.send(gateOpMsg{call: blockedCall(release, "done"), cont: noteCont("continued")})
	if r.m.gate == nil || len(r.calls) != 1 {
		t.Fatalf("the gated call opened no gate (gate %v, calls %d)", r.m.gate, len(r.calls))
	}
	issued := digestModel(&r.m)
	width := r.m.width

	stub.Emit(agent.Event{Type: agent.EventText, Text: "a held reply"})
	arrivals := []tea.Msg{
		runeKey('x'),
		tea.MouseMsg{Button: tea.MouseButtonWheelDown, Action: tea.MouseActionPress},
		pasteMsg{text: "pasted"},
		tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(" more"), Paste: true},
		tea.WindowSizeMsg{Width: 90, Height: 24},
		tickMsg{gen: r.m.tickGen},
		nil, // the event, read off the stream here
		startedMsg{},
		modeAppliedMsg{gen: r.m.modeGen},
		hiddenRetryMsg{},
		frameSyncMsg{n: 7},
	}
	var want []string
	for _, msg := range arrivals {
		if msg == nil {
			r.read()
			want = append(want, "tui.eventMsg")
		} else {
			r.send(msg)
			want = append(want, fmt.Sprintf("%T", msg))
		}
		if changed := issued.diff(digestModel(&r.m)); len(changed) != 0 {
			t.Fatalf("holding %T changed %v", msg, changed)
		}
	}
	if got := heldKinds(r.m); !slices.Equal(got, want) {
		t.Fatalf("held %v, want every arrival in order %v", got, want)
	}
	if r.m.syncAck == 7 || r.m.syncPending != 0 {
		t.Fatalf("the sync token that arrived behind held messages is acked %d, pending %d: want it held behind them", r.m.syncAck, r.m.syncPending)
	}
	if len(r.reads) != 1 {
		t.Fatalf("%d reads outstanding while the gate is open, want the one that keeps the primary draining", len(r.reads))
	}

	close(release)
	r.answer()
	if r.m.gate != nil {
		t.Fatal("the reply left the gate open")
	}
	if notes := texts(r.m, entryNote); !slices.Contains(notes, "continued: done <nil>") {
		t.Fatalf("the continuation did not run at the release: notes %q", notes)
	}
	// Nothing held was applied in the release's Update.
	if got := heldKinds(r.m); !slices.Equal(got, want) {
		t.Fatalf("the release applied held messages: still held %v, want %v", got, want)
	}
	if r.m.input.Value() != "" || r.m.width != width {
		t.Fatalf("the release applied a held key or resize: composer %q, width %d", r.m.input.Value(), r.m.width)
	}
	if r.m.syncAck == 7 {
		t.Fatal("the release acknowledged a token held behind messages it did not apply")
	}
	if r.drains != 1 {
		t.Fatalf("the release owes %d drains, want one", r.drains)
	}

	// Each drain applies exactly one held message, in order.
	for i := range want {
		r.drain()
		if n := len(r.m.held); n != len(want)-i-1 {
			t.Fatalf("drain %d left %d held, want %d", i+1, n, len(want)-i-1)
		}
		switch want[i] {
		case "tea.KeyMsg":
			if i == 0 && r.m.input.Value() != "x" {
				t.Fatalf("the first drain typed %q, want the held key", r.m.input.Value())
			}
		case "tea.WindowSizeMsg":
			if r.m.width != 90 {
				t.Fatalf("the drained resize left width %d", r.m.width)
			}
		case "tui.frameSyncMsg":
			if r.m.syncAck != 7 || r.m.input.Value() != "xpasted more" {
				t.Fatalf("the drained token acked %d with composer %q: want it acknowledged after every message before it", r.m.syncAck, r.m.input.Value())
			}
		}
		if want[i] != "tui.frameSyncMsg" && i < len(want)-1 && r.m.syncAck == 7 {
			t.Fatalf("drain %d (%s) acknowledged the token ahead of its turn", i+1, want[i])
		}
	}
	if r.m.input.Value() != "xpasted more" {
		t.Fatalf("the composer is %q after the drain, want the key and both pastes in order", r.m.input.Value())
	}
	if !strings.Contains(plainView(r.m), "a held reply") {
		t.Fatalf("the drained event was not applied:\n%s", plainView(r.m))
	}
	if r.drains != 0 || len(r.m.held) != 0 {
		t.Fatalf("the drain did not finish: %d owed, %d held", r.drains, len(r.m.held))
	}
	if len(r.reads) != 1 {
		t.Fatalf("%d reads outstanding after the drain, want one", len(r.reads))
	}
}

// TestANestedGateAtReleaseIsKept (astra 3): a continuation that opens the next
// link of a chain. The release cleared the gate before running it, so the
// successor is open when the release Update ends, its call is issued, nothing
// held drains behind it, and its own reply runs its own continuation — then,
// and only then, the held messages drain.
func TestANestedGateAtReleaseIsKept(t *testing.T) {
	m, _ := gatedModel(t)
	r := newGateRig(t, m)
	releaseA, releaseB := make(chan struct{}), make(chan struct{})
	var clearedFirst bool
	r.send(gateOpMsg{call: blockedCall(releaseA, "a"), cont: func(m Model, rep gateReply) (Model, tea.Cmd) {
		clearedFirst = m.gate == nil
		m.addNote("link a")
		return m.run(gateDeadline, blockedCall(releaseB, "b"), noteCont("link b"))
	}})
	first := r.m.gate
	r.send(runeKey('y'))

	close(releaseA)
	r.answer()
	if !clearedFirst {
		t.Fatal("the continuation ran with its own gate still open: the release must clear it first")
	}
	if r.m.gate == nil || r.m.gate == first {
		t.Fatalf("the successor gate was erased by its own release (gate %v)", r.m.gate)
	}
	if len(r.calls) != 1 || r.drains != 0 {
		t.Fatalf("after the first link: %d calls waiting, %d drains owed; want the successor's call and no drain", len(r.calls), r.drains)
	}
	if got := heldKinds(r.m); !slices.Equal(got, []string{"tea.KeyMsg"}) || r.m.input.Value() != "" {
		t.Fatalf("the held key moved during the chain: held %v, composer %q", got, r.m.input.Value())
	}
	if len(r.reads) != 1 {
		t.Fatalf("%d reads outstanding under the successor gate, want one", len(r.reads))
	}

	close(releaseB)
	r.answer()
	if notes := texts(r.m, entryNote); !slices.Equal(notes[len(notes)-2:], []string{"link a", "link b: b <nil>"}) {
		t.Fatalf("the chain's notes are %q, want both links in order", notes)
	}
	r.drainAll()
	if r.m.input.Value() != "y" || r.m.gate != nil || len(r.m.held) != 0 {
		t.Fatalf("after the chain: composer %q, gate %v, held %d", r.m.input.Value(), r.m.gate, len(r.m.held))
	}
}

// TestASyncTokenWaitsForTheChainsEnd: a sync token that arrived while the
// first link of a chain was open is not acknowledged by a release that opens
// the next link — its frame would show the chain half done, which the gateSync
// baseline, running every link inside the key's Update, never shows. It is
// acknowledged by the release that leaves no gate open, and every frame
// between is gated, so no wait can match one.
func TestASyncTokenWaitsForTheChainsEnd(t *testing.T) {
	chain := func(releaseA, releaseB chan struct{}) gateOpMsg {
		return gateOpMsg{call: blockedCall(releaseA, "a"), cont: func(m Model, rep gateReply) (Model, tea.Cmd) {
			m.addNote("link a")
			return m.run(gateDeadline, blockedCall(releaseB, "b"), noteCont("link b"))
		}}
	}

	m, _ := gatedModel(t)
	r := newGateRig(t, m)
	f := frameModel{inner: r.m, bus: newFrameBus(nil)}
	step := func(msg tea.Msg) frameState {
		r.m = f.inner
		r.send(msg)
		f.inner = r.m
		f.publish()
		return f.bus.last()
	}
	releaseA, releaseB := make(chan struct{}), make(chan struct{})
	close(releaseA)
	close(releaseB)
	if s := step(chain(releaseA, releaseB)); !s.gated {
		t.Fatal("the frame of the issuing Update is not gated")
	}
	if s := step(frameSyncMsg{n: 3}); s.sync == 3 || !s.gated {
		t.Fatalf("the token was acked while the chain's first link was open (sync %d, gated %v)", s.sync, s.gated)
	}
	rep, ok := runWatched(t, r.calls[0]).(gateReply)
	r.calls = r.calls[1:]
	if !ok {
		t.Fatal("the first link answered no reply")
	}
	s := step(rep)
	if s.sync == 3 || !s.gated || f.inner.syncPending != 3 {
		t.Fatalf("the release that opened the next link acked the token (sync %d, gated %v, pending %d)", s.sync, s.gated, f.inner.syncPending)
	}
	if (waitSpec{kind: "text", needle: "link a"}).match(s) {
		t.Fatal("a wait matched the half-done chain's frame")
	}
	rep, ok = runWatched(t, r.calls[0]).(gateReply)
	r.calls = r.calls[1:]
	if !ok {
		t.Fatal("the second link answered no reply")
	}
	s = step(rep)
	if s.sync != 3 || s.gated || !strings.Contains(s.plain, "link b: b") {
		t.Fatalf("the chain's end: sync %d, gated %v, want the token acked on the frame that shows both links:\n%s", s.sync, s.gated, s.plain)
	}

	// The baseline runs both links inside the issuing Update, and acks the
	// token on the next frame: the same frame the async chain's end acks it on.
	base, _ := gatedModel(t)
	base.gateSync = true
	bf := frameModel{inner: base, bus: newFrameBus(nil)}
	bstep := func(msg tea.Msg) frameState {
		tm, _ := bf.inner.gated(msg, withGateOps)
		bf.inner = tm.(Model)
		bf.publish()
		return bf.bus.last()
	}
	bstep(chain(releaseA, releaseB))
	bs := bstep(frameSyncMsg{n: 3})
	if bs.sync != 3 || bs.plain != s.plain {
		t.Fatalf("the baseline acked %d on a different frame\n--- gateSync ---\n%s\n--- async ---\n%s", bs.sync, bs.plain, s.plain)
	}
}

// TestHeldEventsStayInSeqOrderThroughAFanOut: many events published at once —
// four emitters fanning out on goroutines of their own — while a gate is open.
// The reader keeps exactly one read outstanding throughout, so the primary
// keeps draining; every event is held in Seq order; and the drain applies them
// in that order, with the reader parked until the last one is applied.
func TestHeldEventsStayInSeqOrderThroughAFanOut(t *testing.T) {
	m, stub := gatedModel(t)
	r := newGateRig(t, m)
	release := make(chan struct{})
	r.send(gateOpMsg{call: blockedCall(release, nil), cont: noteCont("released")})

	const emitters, each = 4, 16
	var wg sync.WaitGroup
	for e := range emitters {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range each {
				stub.Emit(agent.Event{Type: agent.EventText, Text: fmt.Sprintf("<%d.%d>", e, i)})
			}
		}()
	}
	wg.Wait()
	var seqs []uint64
	for range emitters * each {
		ev := r.read()
		seqs = append(seqs, ev.Seq)
		if len(r.reads) != 1 {
			t.Fatalf("%d reads outstanding while the gate is open, want exactly one", len(r.reads))
		}
	}
	if r.maxReads != 1 {
		t.Fatalf("%d reads were outstanding at once", r.maxReads)
	}
	if !slices.IsSorted(seqs) || len(slices.Compact(slices.Clone(seqs))) != len(seqs) {
		t.Fatalf("the reader delivered seqs %v, want strictly increasing", seqs)
	}
	for i, h := range r.m.held {
		if ev, ok := h.msg.(eventMsg); !ok || ev.ev.Seq != seqs[i] {
			t.Fatalf("held[%d] is %#v, want the event with seq %d", i, h.msg, seqs[i])
		}
	}

	close(release)
	r.answer()
	// The read started when the last event arrived is still in flight, with
	// nothing left to deliver: no drained event may start another beside it.
	for i, seq := range seqs {
		r.drain()
		if got := r.m.shared.Seq(); got != seq {
			t.Fatalf("drain %d folded up to seq %d, want %d: events applied out of order", i+1, got, seq)
		}
		if len(r.reads) != 1 {
			t.Fatalf("drain %d left %d reads outstanding, want the one already in flight", i+1, len(r.reads))
		}
	}
	s := strings.Join(texts(r.m, entryAssistant), "")
	for e := range emitters {
		prev := -1
		for i := range each {
			at := strings.Index(s, fmt.Sprintf("<%d.%d>", e, i))
			if at < 0 || at < prev {
				t.Fatalf("emitter %d's event %d is missing or out of order in %q", e, i, s)
			}
			prev = at
		}
	}
}

// TestAParkedReaderRestartsWhenADrainedKeyOpensAGate (astra r2 8): gate A
// releases with messages held; the read that was outstanding lands during the
// drain, so the reader parks — no gate open, messages still held. A drained
// Enter then opens gate B (a /rename), and exactly one read starts in that
// Update: a command waiting on primary delivery must never wait for its
// deadline because nobody is reading.
func TestAParkedReaderRestartsWhenADrainedKeyOpensAGate(t *testing.T) {
	m, stub := gatedModel(t)
	r := newGateRig(t, m)
	release := make(chan struct{})
	r.send(gateOpMsg{call: blockedCall(release, nil), cont: noteCont("a")})

	stub.Emit(agent.Event{Type: agent.EventText, Text: "while a is open"})
	r.read()
	for _, k := range "/rename b" {
		r.send(runeKey(k))
	}
	r.send(enter())
	stub.Emit(agent.Event{Type: agent.EventText, Text: "during the drain"})

	close(release)
	r.answer()
	if r.drains != 1 || len(r.reads) != 1 {
		t.Fatalf("after a's release: %d drains owed, %d reads outstanding", r.drains, len(r.reads))
	}
	// The outstanding read lands while the drain runs: held, and the reader
	// parks.
	r.read()
	if len(r.reads) != 0 || r.m.reading {
		t.Fatalf("the reader did not park during the drain: %d reads outstanding, reading %v", len(r.reads), r.m.reading)
	}
	// Drain until the Enter opens gate B.
	for r.m.gate == nil {
		if len(r.reads) != 0 {
			t.Fatalf("a read started while draining with no gate open (%d held)", len(r.m.held))
		}
		r.drain()
	}
	if len(r.reads) != 1 || !r.m.reading {
		t.Fatalf("gate b opened in a drained Update with %d reads outstanding (reading %v): want exactly one started", len(r.reads), r.m.reading)
	}
	if got := heldKinds(r.m); !slices.Equal(got, []string{"tui.eventMsg"}) || r.drains != 0 {
		t.Fatalf("with b open: held %v, drains owed %d; want the event that parked the reader, and the drain stopped", got, r.drains)
	}
	// B's call is the real SetTitle; while it waits the primary is read.
	stub.Emit(agent.Event{Type: agent.EventText, Text: "while b is open"})
	r.read()
	if len(r.reads) != 1 {
		t.Fatalf("%d reads outstanding under b", len(r.reads))
	}
	r.answer()
	r.drainAll()
	if !strings.Contains(strings.Join(texts(r.m, entryNote), "|"), "renamed to b") {
		t.Fatalf("b's continuation did not run: notes %q", texts(r.m, entryNote))
	}
	for _, want := range []string{"while a is open", "during the drain", "while b is open"} {
		if !strings.Contains(plainView(r.m), want) {
			t.Fatalf("the event %q was never applied:\n%s", want, plainView(r.m))
		}
	}
}

// TestAGatedCallThatNeverAnswersReleasesAtItsDeadline (astra 7): a call that
// never returns releases its gate at its deadline with ErrNoAnswer, the held
// messages drain behind it, and a reply that turns up for a gate no longer
// open is dropped. A call that honours its context and gives up at the
// deadline is the same outcome, never the context's own error.
func TestAGatedCallThatNeverAnswersReleasesAtItsDeadline(t *testing.T) {
	never := make(chan struct{})
	t.Cleanup(func() { close(never) })

	m, _ := gatedModel(t)
	r := newGateRig(t, m)
	var got []error
	cont := func(m Model, rep gateReply) (Model, tea.Cmd) {
		got = append(got, rep.err)
		return m, nil
	}
	r.send(gateOpMsg{deadline: 20 * time.Millisecond, call: blockedCall(never, "late"), cont: cont})
	id := r.m.gate.id
	r.send(runeKey('z'))
	start := time.Now()
	r.answer()
	if len(got) != 1 || !errors.Is(got[0], ErrNoAnswer) {
		t.Fatalf("the continuation saw %v, want ErrNoAnswer", got)
	}
	if waited := time.Since(start); waited < 20*time.Millisecond {
		t.Fatalf("released after %s, before its deadline", waited)
	}
	if r.m.gate != nil {
		t.Fatal("the deadline left the gate open")
	}
	r.drainAll()
	if r.m.input.Value() != "z" {
		t.Fatalf("the held key did not drain after the deadline: composer %q", r.m.input.Value())
	}

	// A late reply for that gate: dropped, with nothing applied.
	before := digestModel(&r.m)
	r.send(gateReply{id: id, result: "late"})
	if len(got) != 1 {
		t.Fatalf("a late reply ran the continuation again: %v", got)
	}
	if changed := before.diff(digestModel(&r.m)); len(changed) != 0 {
		t.Fatalf("a late reply changed %v", changed)
	}

	// A call that gives up at the deadline, with the context's error.
	r.send(gateOpMsg{deadline: 20 * time.Millisecond, cont: cont, call: func(ctx context.Context, _ backend.Backend) (any, error) {
		<-ctx.Done()
		return nil, fmt.Errorf("gave up: %w", ctx.Err())
	}})
	r.answer()
	if len(got) != 2 || !errors.Is(got[1], ErrNoAnswer) {
		t.Fatalf("a call that gave up at its deadline answered %v, want ErrNoAnswer", got)
	}
	// The deadline is the asynchronous gate's alone, and it reaches the call
	// as its context's. A call that answers with that context's error, once
	// the context has ended, answered nothing (callGated) — whichever of the
	// call and the deadline the command sees first.
	var asyncCtx context.Context
	r.send(gateOpMsg{deadline: time.Minute, cont: cont, call: func(ctx context.Context, _ backend.Backend) (any, error) {
		asyncCtx = ctx
		return nil, nil
	}})
	r.answer()
	if _, ok := asyncCtx.Deadline(); !ok {
		t.Fatal("the asynchronous call's context carries no deadline")
	}
	expired, cancel := context.WithTimeout(context.Background(), -time.Second)
	defer cancel()
	if rep := callGated(expired, nil, 9, func(ctx context.Context, _ backend.Backend) (any, error) {
		return nil, fmt.Errorf("gave up: %w", ctx.Err())
	}); !errors.Is(rep.err, ErrNoAnswer) || rep.id != 9 {
		t.Fatalf("a call that answered its expired context's error came back %+v, want ErrNoAnswer", rep)
	}

	// The gateSync baseline is today's control flow exactly (C17a, note 9):
	// the call inline, with the context today's direct calls had — one that
	// never ends — and its own answer, never ErrNoAnswer.
	r.m.gateSync = true
	var inlineCtx context.Context
	r.send(gateOpMsg{deadline: 20 * time.Millisecond, cont: cont, call: func(ctx context.Context, _ backend.Backend) (any, error) {
		inlineCtx = ctx
		return nil, errors.New("its own answer")
	}})
	if _, ok := inlineCtx.Deadline(); ok || inlineCtx.Done() != nil {
		t.Fatal("the baseline's inline call was given a context that ends: the deadline is the asynchronous gate's alone")
	}
	if last := got[len(got)-1]; last == nil || last.Error() != "its own answer" {
		t.Fatalf("the baseline's continuation saw %v, want the call's own answer", last)
	}
}

// TestAHoldOfLargeRecordsReleasesAtItsBytes (astra r2 17, r3 17): the held
// queue is bounded by bytes as well as by count. Records near the size limit
// reach 64 MiB long before 10,000 messages: the message that crosses it
// releases the gate with ErrNoAnswer; nothing is dropped and nothing applied;
// the reader parks while the backlog drains. A paste is counted as an event
// is. The count bound releases at 10,000 messages the same way.
func TestAHoldOfLargeRecordsReleasesAtItsBytes(t *testing.T) {
	never := make(chan struct{})
	t.Cleanup(func() { close(never) })

	t.Run("bytes", func(t *testing.T) {
		m, _ := gatedModel(t)
		var got []error
		cont := func(m Model, rep gateReply) (Model, tea.Cmd) {
			got = append(got, rep.err)
			return m, nil
		}
		send := func(msg tea.Msg) tea.Cmd {
			tm, cmd := m.gated(msg, withGateOps)
			m = tm.(Model)
			return cmd
		}
		send(gateOpMsg{call: blockedCall(never, nil), cont: cont})
		big := strings.Repeat("x", 8<<20)
		const records = 7
		for i := range records {
			send(eventMsg{agent.Event{Type: agent.EventText, Text: big, Seq: uint64(i + 1)}})
			if m.gate == nil {
				t.Fatalf("released after %d records (%d bytes held), before the bound", i+1, m.heldBytes)
			}
			if !m.reading {
				t.Fatal("the reader stopped while the gate was open")
			}
		}
		// The paste is the record that crosses 64 MiB.
		send(pasteMsg{text: big})
		if m.gate != nil || len(got) != 1 || !errors.Is(got[0], ErrNoAnswer) {
			t.Fatalf("at %d bytes held: gate %v, continuation saw %v; want released with ErrNoAnswer", m.heldBytes, m.gate, got)
		}
		if n := len(m.held); n != records+1 {
			t.Fatalf("%d messages held after the release, want all %d", n, records+1)
		}
		// The reader parks: the next event is held, and no read follows it.
		cmd := send(eventMsg{agent.Event{Type: agent.EventText, Text: "after", Seq: records + 1}})
		if m.reading || cmdHas(cmd, tuiPkg+"waitEvent") {
			t.Fatal("the reader was re-armed while the backlog drains")
		}
		for i, h := range m.held {
			switch msg := h.msg.(type) {
			case eventMsg:
				want := uint64(i + 1)
				if i == records+1 {
					want = records + 1
				}
				if msg.ev.Seq != want {
					t.Fatalf("held[%d] has seq %d, want %d", i, msg.ev.Seq, want)
				}
			case pasteMsg:
				if i != records {
					t.Fatalf("the paste is held at %d, want %d", i, records)
				}
			default:
				t.Fatalf("held[%d] is %T", i, h.msg)
			}
		}
	})

	t.Run("count", func(t *testing.T) {
		m, _ := gatedModel(t)
		var got []error
		tm, _ := m.gated(gateOpMsg{call: blockedCall(never, nil), cont: func(m Model, rep gateReply) (Model, tea.Cmd) {
			got = append(got, rep.err)
			return m, nil
		}}, withGateOps)
		m = tm.(Model)
		for i := range heldMaxMessages {
			if m.gate == nil {
				t.Fatalf("released after %d messages, before the bound", i)
			}
			tm, _ = m.gated(tickMsg{gen: -1 - i}, withGateOps)
			m = tm.(Model)
		}
		if m.gate != nil || len(got) != 1 || !errors.Is(got[0], ErrNoAnswer) || len(m.held) != heldMaxMessages {
			t.Fatalf("at %d held: gate %v, continuation saw %v; want released with ErrNoAnswer and nothing dropped", len(m.held), m.gate, got)
		}
	})
}

// cmdHas reports whether cmd, or any member of a batch it is, is the function
// the linker named name.
func cmdHas(cmd tea.Cmd, name string) bool { return len(cmdsNamed(cmd, name)) > 0 }

// cmdsNamed is every command cmd is, or a batch it is holds, that is the
// function the linker named name, in order.
func cmdsNamed(cmd tea.Cmd, name string) []tea.Cmd {
	if cmd == nil {
		return nil
	}
	fn := cmdFuncName(cmd)
	if strings.HasPrefix(fn, teaPkg+"compactCmds") || strings.HasPrefix(fn, teaPkg+"Batch") {
		var out []tea.Cmd
		if b, ok := cmd().(tea.BatchMsg); ok {
			for _, c := range b {
				out = append(out, cmdsNamed(c, name)...)
			}
		}
		return out
	}
	if strings.HasPrefix(fn, name) {
		return []tea.Cmd{cmd}
	}
	return nil
}

// TestACallThatPanicsAfterItsDeadlinePanicsInUpdate (astra C18a r42 5): a
// call still running when its deadline releases the gate is still waited for
// — by the release, or by the drop of a late reply after the held queue's
// bound released it — in a command (lingerOn) whose panic message panics in
// the Update it reaches, held or not, where bubbletea recovers it. A lingering
// call that returns produces no message at all.
func TestACallThatPanicsAfterItsDeadlinePanicsInUpdate(t *testing.T) {
	lateCall := func(late <-chan struct{}, panics bool) gateCall {
		return func(context.Context, backend.Backend) (any, error) {
			<-late
			if panics {
				panic("a panic after the deadline")
			}
			return "too late", nil
		}
	}
	// panicsInUpdate hands msg to the model with a gate open, and answers what
	// the Update panicked with.
	panicsInUpdate := func(t *testing.T, r *gateRig, msg tea.Msg) (v any) {
		t.Helper()
		never := make(chan struct{})
		t.Cleanup(func() { close(never) })
		r.send(gateOpMsg{call: blockedCall(never, nil), cont: noteCont("second")})
		defer func() { v = recover() }()
		r.send(msg)
		return nil
	}
	assertTheCallsPanic := func(t *testing.T, v any) {
		t.Helper()
		p, ok := v.(callPanic)
		if !ok || p.value != "a panic after the deadline" {
			t.Fatalf("the Update panicked with %#v, want the lingering call's own panic", v)
		}
	}

	t.Run("released by its deadline", func(t *testing.T) {
		m, _ := gatedModel(t)
		r := newGateRig(t, m)
		late := make(chan struct{})
		r.send(gateOpMsg{deadline: 20 * time.Millisecond, call: lateCall(late, true), cont: noteCont("released")})
		rep := r.answer()
		if !errors.Is(rep.err, ErrNoAnswer) || rep.linger == nil {
			t.Fatalf("the deadline's reply is %+v, want ErrNoAnswer carrying the call's outcome", rep)
		}
		if len(r.lingers) != 1 {
			t.Fatalf("the release waits for the lingering call %d times, want once", len(r.lingers))
		}
		close(late)
		msg := runWatched(t, r.lingers[0])
		if _, ok := msg.(callPanicMsg); !ok {
			t.Fatalf("the lingering call's wait answered %#v, want its panic", msg)
		}
		assertTheCallsPanic(t, panicsInUpdate(t, r, msg))
	})

	t.Run("its gate released by the bound first", func(t *testing.T) {
		m, _ := gatedModel(t)
		r := newGateRig(t, m)
		late := make(chan struct{})
		r.send(gateOpMsg{deadline: 20 * time.Millisecond, call: lateCall(late, true), cont: noteCont("released")})
		big := strings.Repeat("p", 8<<20)
		for r.m.gate != nil {
			r.send(pasteMsg{text: big})
		}
		// The command is still in its select: its deadline passes now, and
		// its reply finds no gate open.
		rep := r.answer()
		if !errors.Is(rep.err, ErrNoAnswer) || len(r.lingers) != 1 {
			t.Fatalf("the late reply %+v left %d waits for its call, want one", rep, len(r.lingers))
		}
		close(late)
		msg := runWatched(t, r.lingers[0])
		assertTheCallsPanic(t, panicsInUpdate(t, r, msg))
	})

	t.Run("a lingering call that returns is discarded", func(t *testing.T) {
		m, _ := gatedModel(t)
		r := newGateRig(t, m)
		late := make(chan struct{})
		r.send(gateOpMsg{deadline: 20 * time.Millisecond, call: lateCall(late, false), cont: noteCont("released")})
		r.answer()
		close(late)
		if msg := runWatched(t, r.lingers[0]); msg != nil {
			t.Fatalf("a lingering call that returned produced %#v, want nothing", msg)
		}
	})
}

// stallsThenPanics is a session whose Begin stalls past the gate's deadline
// and then panics: the gated Submit's call is still running when its gate is
// released, and panics after. closes counts Close calls.
type stallsThenPanics struct {
	*Stub
	stall  time.Duration
	closes *atomic.Int32
}

func (s stallsThenPanics) Begin(string) func(context.Context) (agent.Result, error) {
	time.Sleep(s.stall)
	panic("Begin panicked after the gate's deadline")
}

func (s stallsThenPanics) Close() error {
	s.closes.Add(1)
	return s.Stub.Close()
}

// TestAGatedCallThatPanicsAfterItsDeadlineEndsTheProgram (astra C18a r42 5)
// through the real program: Submit's Begin stalls past the gate's deadline,
// the gate releases with ErrNoAnswer, and then the call panics. bubbletea
// recovers it — through Update, where the lingering call's wait delivers it —
// and ends the program with ErrProgramPanic, the session closed exactly once;
// the test binary does not crash. The gateSync baseline panics inside Enter's
// Update, as the synchronous call always did.
func TestAGatedCallThatPanicsAfterItsDeadlineEndsTheProgram(t *testing.T) {
	prev := gateDeadline
	gateDeadline = 50 * time.Millisecond
	t.Cleanup(func() { gateDeadline = prev })
	for _, mode := range frameGateModes {
		t.Run(mode.name, func(t *testing.T) {
			isolateSkillsHome(t)
			var closes atomic.Int32
			_, _, err := RunFrameScript(Config{
				Session:   stallsThenPanics{Stub: NewStub(), stall: 300 * time.Millisecond, closes: &closes},
				Theme:     "tokyo-night",
				Workspace: frameWorkspace(t),
				Model:     "grok",
				Yolo:      true,
			}, 80, 24, "<wait:idle>hi<enter><sleep:1500ms>", FrameOpts{Timeout: 5 * time.Second, gateSync: mode.sync})
			if !errors.Is(err, tea.ErrProgramPanic) {
				t.Fatalf("RunFrameScript returned %v, want the program's panic", err)
			}
			if n := closes.Load(); n != 1 {
				t.Fatalf("the session was closed %d times, want once", n)
			}
		})
	}
}

// TestPumpUntilWaitsOutAnOpenGate (astra C17 11): pumpUntil answers only a
// model with no gated call waiting and nothing held, however early its
// predicate holds: a model mid-gate is its issuing Update's half-done frame.
func TestPumpUntilWaitsOutAnOpenGate(t *testing.T) {
	m := asyncGate(t, sized(t))
	m.input.SetValue("/rename pumped")
	tm, cmd := m.Update(enter())
	m = tm.(Model)
	if m.gate == nil {
		t.Fatal("/rename opened no gate")
	}
	m = pumpCmd(t, m, cmd)
	m = pumpUntil(t, m, func(Model) bool { return true })
	if m.gate != nil || len(m.held) != 0 {
		t.Fatalf("pumpUntil answered a model mid-gate (gate %v, %d held)", m.gate, len(m.held))
	}
	if !slices.Contains(texts(m, entryNote), "renamed to pumped") {
		t.Fatalf("the rename's continuation had not run: notes %q", texts(m, entryNote))
	}
}
