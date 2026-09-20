package tui

import (
	"context"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/craze/internal/agent"
)

// The pump is a miniature of the bubbletea runtime, and it is what lets a
// driver test read as "press a key, then wait for what a user would see"
// instead of "hand the model the two messages the driver happens to use".
//
// It does the three things the runtime does and nothing else: it calls Update
// on one goroutine (the test's), it runs the tea.Cmds Update returned *off*
// that goroutine, and it feeds every message they produce — plus the session's
// own events — back into Update. A test therefore never constructs a turn's
// ending: it starts a turn for real, the stub answers it on its own goroutine,
// and the test waits for a predicate over the model.
//
// That is deliberately indifferent to *how* a turn ends. Today the ending
// arrives as a promptDoneMsg from the prompt's Cmd; after the driver moves into
// internal/engine it arrives as an event on the session's stream. Both are "a
// message the pump feeds back", so the tests do not have to know which.
//
// Two rules keep it honest:
//
//  1. Every wait is a blocking read. Wall clock appears exactly once, in
//     deadline(), as a watchdog that turns a deadlock into a readable failure
//     rather than a hung run. There is no polling anywhere: a test that wants
//     to be sure a prompt has reached a known point waits on a barrier the Stub
//     hands it (ParkNext), not on a sleep.
//  2. Timer commands are never run. tea.Tick's command blocks on a real timer,
//     so executing the spinner chain would make every pumped test wait out
//     beats it does not care about, and re-arm one per beat for ever. The event
//     reader is skipped for a different reason: exactly one goroutine may
//     receive from Events(), and two readers would steal each other's events,
//     so the pump owns the one reader and drops the model's own waitEvent.

// pumpWatchdog is how long a pumped wait may make no progress before the test
// fails. It is not a timing assertion — nothing in a passing run waits this
// long — it is the difference between a readable failure and `go test` hanging
// until its own timeout kills the whole package.
const pumpWatchdog = 10 * time.Second

// deadline is the one place this file reads the clock.
func deadline() <-chan time.Time { return time.After(pumpWatchdog) }

// pump is one test's runtime: the message queue every goroutine reports into,
// the single reader of the session's event stream, and the bookkeeping that
// stops any of it outliving the test.
type pump struct {
	sess agent.Session
	// msgs carries every message bound for Update, from the event reader and
	// from the commands alike, in the order they were produced. Buffered so a
	// command that has finished never holds its goroutine open waiting for the
	// test to get round to it.
	msgs chan tea.Msg
	// dead is closed at cleanup: a goroutine still holding a message gives up
	// on delivering it instead of blocking on a queue nobody reads again.
	dead chan struct{}
	wg   sync.WaitGroup
}

// pumps is one pump per test. The helpers take the model by value — it is a
// value type, and every test carries its own copy — so the pump cannot live on
// it, and a test's Cmd goroutines and event reader have to be found again on
// the next call. A subtest that pumps gets its own pump, which is right: its
// model and its stub are its own too.
var (
	pumpsMu sync.Mutex
	pumps   = map[*testing.T]*pump{}
)

// pumpFor returns this test's pump, starting the event reader on the first
// call. The cleanup is registered there too: closing the session is what
// releases a reader blocked on Events() and a prompt parked in the stub, so
// nothing the pump started is still running when the test returns.
func pumpFor(t *testing.T, m Model) *pump {
	t.Helper()
	pumpsMu.Lock()
	if p, ok := pumps[t]; ok {
		pumpsMu.Unlock()
		return p
	}
	p := &pump{
		sess: m.sess,
		msgs: make(chan tea.Msg, 256),
		dead: make(chan struct{}),
	}
	pumps[t] = p
	pumpsMu.Unlock()
	if p.sess == nil {
		t.Fatal("pump: the model has no session to drive")
	}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		p.read()
	}()
	t.Cleanup(func() {
		pumpsMu.Lock()
		delete(pumps, t)
		pumpsMu.Unlock()
		// dead first: it is what stops the reader and stops a command that came
		// back with a message from waiting on a queue nobody reads again.
		// Closing the log does not close its primary channel — nothing does,
		// because in a real run the program exits instead — so the reader has
		// to be told, not starved.
		close(p.dead)
		// Then the session, which releases a prompt still parked or hung inside
		// the stub, and reaps a real agent. Then the join, which is the
		// assertion that nothing the pump started outlives the test.
		_ = p.sess.Close()
		p.wg.Wait()
	})
	return p
}

// read is the single consumer of the session's event stream, which is why the
// model's own waitEvent is skipped (pumpSkips).
func (p *pump) read() {
	ch := p.sess.Events()
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return
			}
			if !p.deliver(eventMsg{ev}) {
				return
			}
		case <-p.dead:
			return
		}
	}
}

// deliver queues a message for Update, or reports that the test is over.
func (p *pump) deliver(msg tea.Msg) bool {
	select {
	case p.msgs <- msg:
		return true
	case <-p.dead:
		return false
	}
}

// dispatch runs a command the way the runtime does: on its own goroutine, with
// a batch unwrapped into one goroutine per member. Nothing here touches the
// Model — only the test's goroutine calls Update — so -race has nothing to
// find.
func (p *pump) dispatch(cmd tea.Cmd) {
	if cmd == nil || pumpSkips(cmd) {
		return
	}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		msg := cmd()
		if msg == nil {
			return
		}
		if batch, ok := msg.(tea.BatchMsg); ok {
			// tea.Batch's message is "run these too", which the runtime does
			// concurrently. Add before this goroutine's own Done, so the join
			// at cleanup cannot miss a child.
			for _, c := range batch {
				p.dispatch(c)
			}
			return
		}
		p.deliver(msg)
	}()
}

// pumpSkips names the two commands the pump must not run, and why.
//
// A tea.Tick command blocks on a real timer — up to a minute for the idle
// chain — so running the spinner chain would make every pumped wait sit out
// beats and re-arm another one per beat. Dropping it leaves armTick's
// generation bookkeeping intact (tickLive stays set, so no second chain is
// armed) and no pumped test depends on a beat being delivered; the one that
// does delivers a tickMsg itself.
//
// waitEvent is skipped because exactly one goroutine may receive from
// Events(): a second reader would take events the first was going to deliver,
// and which reader got which event would decide what the test saw. The pump
// owns that one reader.
//
// It is matched on the function the linker named, which is why
// TestPumpSkipsTheTickChainAndTheEventReader exists: a bubbletea upgrade that
// renamed Tick's closure would otherwise quietly turn every pumped test into a
// timer race.
func pumpSkips(cmd tea.Cmd) bool {
	name := cmdFuncName(cmd)
	switch {
	case strings.HasPrefix(name, "github.com/charmbracelet/bubbletea.Tick"),
		strings.HasPrefix(name, "github.com/charmbracelet/bubbletea.Every"):
		return true
	case strings.HasPrefix(name, "github.com/charliek/craze/internal/tui.waitEvent"):
		return true
	}
	return false
}

// cmdFuncName is the command's function as the linker named it. Two closures
// made from one func literal share it, which is exactly the granularity
// pumpSkips wants.
func cmdFuncName(cmd tea.Cmd) string {
	if cmd == nil {
		return ""
	}
	fn := runtime.FuncForPC(reflect.ValueOf(cmd).Pointer())
	if fn == nil {
		return ""
	}
	return fn.Name()
}

// ------------------------------------------------------------------ the verbs

// pumpUntil runs the model until pred holds, applying whatever the session and
// the commands produce. It is the whole vocabulary of a driver test: press a
// key, then say what the user should end up seeing.
//
// The predicate is checked after each Update and once before the first, so a
// state already reached is not waited for.
func pumpUntil(t *testing.T, m Model, pred func(Model) bool) Model {
	t.Helper()
	p := pumpFor(t, m)
	if pred(m) {
		return m
	}
	timeout := deadline()
	for {
		select {
		case msg := <-p.msgs:
			tm, cmd := m.Update(msg)
			m = tm.(Model)
			p.dispatch(cmd)
			if pred(m) {
				return m
			}
		case <-timeout:
			t.Fatalf("pumpUntil: no state satisfied the predicate in %s\n"+
				"status=%s err=%q cancelled=%v prompted=%v queued=%q armed=%v messages waiting=%d\n%s",
				pumpWatchdog, m.status, m.err, m.cancelled, m.prompted,
				queueTexts(m), sendNowArmed(m), len(p.msgs), plainView(m))
		}
	}
}

// pumpApply hands the model one message and dispatches what it asked for,
// without waiting: the caller says next what it is waiting for.
func pumpApply(t *testing.T, m Model, msg tea.Msg) Model {
	t.Helper()
	p := pumpFor(t, m)
	tm, cmd := m.Update(msg)
	m = tm.(Model)
	p.dispatch(cmd)
	return m
}

// pumpKey presses a key the way a terminal delivers it.
func pumpKey(t *testing.T, m Model, k tea.KeyMsg) Model {
	t.Helper()
	return pumpApply(t, m, k)
}

// pumpMsg is pumpApply for a message that is not a key.
func pumpMsg(t *testing.T, m Model, msg tea.Msg) Model {
	t.Helper()
	return pumpApply(t, m, msg)
}

// pumpCmd dispatches a command the test is holding rather than one an Update
// just returned. It is how a test pins the window a command has not run in yet
// — Enter's prompt command against the Esc that cancels it — and still lets
// that command's result come back through the runtime like any other.
func pumpCmd(t *testing.T, m Model, cmd tea.Cmd) Model {
	t.Helper()
	p := pumpFor(t, m)
	if cmd == nil {
		t.Fatal("pumpCmd: nothing to dispatch")
	}
	p.dispatch(cmd)
	return m
}

// pumpEnter types into the composer and presses Enter, which is how every send
// in these tests starts: whether it becomes a turn, a queued row or a refusal
// is the model's decision, not the test's.
func pumpEnter(t *testing.T, m Model, text string) Model {
	t.Helper()
	m.input.SetValue(text)
	return pumpKey(t, m, enter())
}

// pumpEsc presses Esc, which is a cancel while a turn is running.
func pumpEsc(t *testing.T, m Model) Model {
	t.Helper()
	return pumpKey(t, m, tea.KeyMsg{Type: tea.KeyEsc})
}

// awaitBarrier blocks on a handshake the Stub hands out, under the same
// watchdog as pumpUntil.
func awaitBarrier(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-deadline():
		t.Fatalf("%s did not happen in %s", what, pumpWatchdog)
	}
}

// heldTurn holds a prompt *before* its turn opens, which is the live session's
// wait for the agent's first command catalog (ParkNext). The prompt is claimed —
// the model is working, so Enter queues and Ctrl+L confirms — but no turn exists
// on the wire, so the only ending it can have is agent.ErrPromptCancelled, with
// no event of any kind. That is the scenario for the tests that are about the
// pre-open window; anything that wants a turn that is really open, or one that
// ends cleanly or in an error, uses the scripted session instead
// (scripted_session_test.go).
//
// The barrier is the point: HangNext would hold a turn on the wire, but nothing
// observable says when a hung prompt reached its select, so a cancel could land
// ahead of it and the prompt would withdraw without hanging — leaving the hang
// flag for the *next* turn to consume, which is a coin toss, not a test.
func heldTurn(t *testing.T, m Model, stub *Stub, text string) Model {
	t.Helper()
	parked := stub.ParkNext()
	m = pumpEnter(t, m, text)
	if m.status != statusWorking {
		t.Fatalf("held turn: Enter left the model %s, not working", m.status)
	}
	awaitBarrier(t, parked, "the prompt reaching the stub's wait")
	return m
}

// heldWorking is queueWorking's pumped twin: a model whose turn is really open
// on the session and held there, so Enter queues, Ctrl+L asks the send-now
// confirm, and the turn ends — with the wire's own cancelled ending — when
// something cancels it. Because the turn is open rather than merely claimed, the
// queue's own guards are in force: nothing can be popped or taken until the turn
// has returned, which is exactly what the drain has to wait for.
func heldWorking(t *testing.T) (Model, *scriptedSession) {
	t.Helper()
	m, sess := scriptedModel(t)
	return startScripted(t, m, sess, "go", scriptHeld()), sess
}

// endHeldTurn cancels the held turn at the session, which is exactly what the
// command behind Esc does. A test calls it directly where Esc means something
// else — inside a queue-row editor it ends the edit — or where Ctrl+C would
// clear the queue the test is watching.
func endHeldTurn(t *testing.T, sess agent.Session) {
	t.Helper()
	if err := sess.Cancel(context.Background()); err != nil {
		t.Fatalf("cancelling the held turn: %v", err)
	}
}

// assertPrompts is the strongest statement a driver test can make: these texts,
// in this order, are what reached the session — no row sent twice, none lost,
// and nothing sent that the user did not ask for.
func assertPrompts(t *testing.T, stub promptSource, want ...string) {
	t.Helper()
	got := stub.Prompts()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("prompts %q, want %q", got, want)
	}
}

// ------------------------------------------------------------- the predicates

func isIdle(m Model) bool    { return m.status == statusIdle }
func isErrored(m Model) bool { return m.status == statusError }

// viewHas is the frame as a user reads it.
func viewHas(want string) func(Model) bool {
	return func(m Model) bool { return strings.Contains(plainView(m), want) }
}

// promptSource is whatever records the prompts a session was given: the Stub,
// and the scripted decorator that wraps one.
type promptSource interface{ Prompts() []string }

// turnsReached is how many prompts have reached the session. It is the one
// honest way to count turns from outside: a turn's identity on the model is
// client-local bookkeeping, and after the driver moves it stops counting turns
// at all.
func turnsReached(stub promptSource, n int) func(Model) bool {
	return func(Model) bool { return len(stub.Prompts()) == n }
}

// queueEmpty reads the band, which is what the user sees of the queue.
func queueEmpty(m Model) bool { return len(queueTexts(m)) == 0 }

// hasCard is a blocking request on screen.
func hasCard(m Model) bool { return m.cardOpen() }

// errorRows is how many error rows the transcript holds: the count matters as
// much as the text, because one failure reported twice used to draw two.
func errorRows(n int) func(Model) bool {
	return func(m Model) bool { return len(texts(m, entryError)) == n }
}

func allOf(preds ...func(Model) bool) func(Model) bool {
	return func(m Model) bool {
		for _, p := range preds {
			if !p(m) {
				return false
			}
		}
		return true
	}
}

// ------------------------------------------------------------- the accessors
//
// Three facts a driver test needs have no form on screen. Each is read here,
// in one place, so the commit that moves the driver re-points one line instead
// of a hundred assertions. Anything that *is* on screen is asserted from the
// frame instead.

// queueTexts is the queued messages, in order. The band draws them, but a test
// that wants to name them needs the list: today it is Snapshot.Queue, which
// moves onto the engine's state when the queue leaves the provider seam.
func queueTexts(m Model) []string {
	out := make([]string, 0, len(m.snap.Queue))
	for _, p := range m.snap.Queue {
		out = append(out, p.Text)
	}
	return out
}

// queueIDs is the same list by id. Ids are not drawn — they are what a verb
// names a row by — so there is nothing on screen to read them from.
func queueIDs(m Model) []string {
	out := make([]string, 0, len(m.snap.Queue))
	for _, p := range m.snap.Queue {
		out = append(out, p.ID)
	}
	return out
}

// sendNowArmed is a confirmed send-now waiting for the cancelled turn to
// settle. Nothing is drawn for it — that is the point of it: the row stays in
// the band and the draft stays in the composer until it fires — so the state
// itself is the only witness. Today it is the model's own field; it becomes
// the engine's armed send.
func sendNowArmed(m Model) bool { return m.strong != nil }

// turnsStarted is how many prompts the session was given, which is the number
// of turns that were really started, in order, with their text.
func turnsStarted(stub promptSource) int { return len(stub.Prompts()) }

// ------------------------------------------------------------- the pump's own

// TestPumpSkipsTheTickChainAndTheEventReader guards the two commands pumpSkips
// recognises by name. If a dependency bump renames either closure the pump
// would start running real timers and a second event reader, and every pumped
// test would turn into a race that only sometimes fails — so the recognition
// is asserted here, where the failure is one line and unmistakable.
func TestPumpSkipsTheTickChainAndTheEventReader(t *testing.T) {
	var m Model
	tick := m.armTick()
	if tick == nil {
		t.Fatal("a model with no chain in flight must arm one")
	}
	if !pumpSkips(tick) {
		t.Fatalf("the tick chain is not recognised: %q", cmdFuncName(tick))
	}
	stub := NewStub()
	t.Cleanup(func() { _ = stub.Close() })
	reader := waitEvent(stub)
	if reader == nil {
		t.Fatal("waitEvent must return a command for a live session")
	}
	if !pumpSkips(reader) {
		t.Fatalf("the event reader is not recognised: %q", cmdFuncName(reader))
	}
	// And nothing else is skipped: a command the pump refused to run would
	// silently strand whatever it was going to report.
	ordinary := tea.Cmd(func() tea.Msg { return nil })
	if pumpSkips(ordinary) {
		t.Fatalf("an ordinary command was skipped: %q", cmdFuncName(ordinary))
	}
}
