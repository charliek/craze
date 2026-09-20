package tui

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/engine"
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
// That is deliberately indifferent to *how* a turn ends, which is why the
// driver moving into internal/engine changed the helpers here and almost none
// of the tests: the ending used to arrive as a message from the prompt's own
// Cmd, and now it is an EventTurn on the session's stream. Both are "a message
// the pump feeds back", so the tests do not have to know which.
//
// Three rules keep it honest:
//
//  1. Every wait is a blocking read. Wall clock appears exactly once, in
//     deadline(), as a watchdog that turns a deadlock into a readable failure
//     rather than a hung run. There is no polling anywhere: a test that wants
//     to be sure a prompt has reached a known point waits on a barrier the
//     session hands it, not on a sleep.
//  2. Timer commands are never run. tea.Tick's command blocks on a real timer,
//     so executing the spinner chain would make every pumped test wait out
//     beats it does not care about, and re-arm one per beat for ever. The event
//     reader is skipped for a different reason: exactly one goroutine may
//     receive from Events(), and two readers would steal each other's events,
//     so the pump owns the one reader and drops the model's own waitEvent.
//  3. A predicate says when a state has been *reached*, never that nothing more
//     is coming. Stopping on one can leave the command that ran the turn still
//     to report, so a test whose claim is "that turn is completely over" — one
//     about to start another, or asserting that nothing else happened — says so
//     with pumpSettled.

// pumpWatchdog is how long a pumped wait may make no progress before the test
// fails. It is not a timing assertion — nothing in a passing run waits this
// long — it is the difference between a readable failure and `go test` hanging
// until its own timeout kills the whole package.
const pumpWatchdog = 10 * time.Second

// deadline is the one place this file reads the clock.
func deadline() <-chan time.Time { return time.After(pumpWatchdog) }

// pumpItem is one message on its way to Update, and whether resolving it
// resolves a dispatched command: pumpSettled counts commands whose message has
// not been applied yet, and it can only do that if the queue says which is
// which.
type pumpItem struct {
	msg tea.Msg
	// fromCmd marks a command's own report. An event the reader delivered is
	// not one: the session produces those on its own account, not because the
	// pump asked for anything.
	fromCmd bool
}

// pump is one test's runtime: the message queue every goroutine reports into,
// the single reader of the session's event stream, and the bookkeeping that
// stops any of it outliving the test.
type pump struct {
	// ctrl is the engine the model drives its session through, which is also
	// where the one event stream comes from: the engine publishes into the
	// session's own log, so the reader below sees the agent's events and the
	// engine's in one order.
	ctrl *engine.Engine
	// msgs carries every message bound for Update, from the event reader and
	// from the commands alike, in the order they were produced. Buffered so a
	// command that has finished never holds its goroutine open waiting for the
	// test to get round to it.
	msgs chan pumpItem
	// dead is closed at cleanup: a goroutine still holding a message gives up
	// on delivering it instead of blocking on a queue nobody reads again.
	dead chan struct{}
	wg   sync.WaitGroup

	// stateMu guards what pumpSettled reads and the received hook. It is a leaf:
	// nothing is held across it.
	stateMu sync.Mutex
	// outstanding is how many dispatched commands have not been fully accounted
	// for: a command is outstanding from the moment it is dispatched until the
	// message it produced has been through Update (or until it turns out to
	// produce none, or to be a batch, whose members take over the count).
	outstanding int
	// afterReceive is a hook the reader calls in the window between taking an
	// event off the stream and queueing it. Only the pump's own tests set it, to
	// pin the reader in exactly that window.
	afterReceive func()
	// quietened is a one-slot wake-up: a goroutine that resolved the last
	// outstanding command signals it, so pumpSettled can block instead of
	// polling. One slot is enough — it is a "look again", not a queue.
	quietened chan struct{}

	// parked and resume are the rendezvous with the reader. The reader offers
	// parked only from the top of its loop, where it holds no event — whatever
	// it received last is already in msgs — and then waits on resume. Both are
	// unbuffered, so taking the offer *is* the proof: pumpSettled cannot inspect
	// the pump while an event is in the reader's hands, which no flag set after
	// the receive could promise, because the receive and the flag are two steps.
	// readerGone is closed when the reader has ended, so a rendezvous nobody can
	// keep is not waited for.
	parked     chan struct{}
	resume     chan struct{}
	readerGone chan struct{}
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
		ctrl:       m.eng,
		msgs:       make(chan pumpItem, 256),
		dead:       make(chan struct{}),
		quietened:  make(chan struct{}, 1),
		parked:     make(chan struct{}),
		resume:     make(chan struct{}),
		readerGone: make(chan struct{}),
	}
	pumps[t] = p
	pumpsMu.Unlock()
	if p.ctrl == nil {
		t.Fatal("pump: the model has no session to drive")
	}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		defer close(p.readerGone)
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
		// Then the engine, which closes the session — releasing a prompt still
		// parked or hung inside the stub, and reaping a real agent — and joins
		// its own goroutines. Then the join, which is the assertion that nothing
		// the pump started outlives the test.
		_ = p.ctrl.Close()
		p.wg.Wait()
	})
	return p
}

// read is the single consumer of the session's event stream, which is why the
// model's own waitEvent is skipped (pumpSkips).
//
// The top of the loop is the one place the reader holds nothing: whatever it
// received last is in msgs by then. That is where it offers the rendezvous, and
// it stays there until it is resumed — which is what lets pumpSettled inspect
// the pump and know that an event is either still on the stream or already
// queued, with nowhere else to be.
func (p *pump) read() {
	ch := p.ctrl.Events()
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return
			}
			if h := p.receivedHook(); h != nil {
				h()
			}
			if !p.deliver(pumpItem{msg: eventMsg{ev}}) {
				return
			}
		case p.parked <- struct{}{}:
			select {
			case <-p.resume:
			case <-p.dead:
				return
			}
		case <-p.dead:
			return
		}
	}
}

// receivedHook is the pump's own tests' way into the window between taking an
// event off the stream and queueing it. It is read under the lock because the
// reader is already running when a test sets it.
func (p *pump) receivedHook() func() {
	p.stateMu.Lock()
	defer p.stateMu.Unlock()
	return p.afterReceive
}

func (p *pump) setReceivedHook(h func()) {
	p.stateMu.Lock()
	p.afterReceive = h
	p.stateMu.Unlock()
}

// deliver queues a message for Update, or reports that the test is over.
func (p *pump) deliver(item pumpItem) bool {
	select {
	case p.msgs <- item:
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
	p.began()
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		msg := cmd()
		if msg == nil {
			// Nothing to apply, so nothing is owed: the command is accounted
			// for here.
			p.resolved()
			return
		}
		if batch, ok := msg.(tea.BatchMsg); ok {
			// tea.Batch's message is "run these too", which the runtime does
			// concurrently. Each member is counted before this one is released,
			// so the count never dips through zero in the middle of a batch —
			// and the join at cleanup cannot miss a child.
			for _, c := range batch {
				p.dispatch(c)
			}
			p.resolved()
			return
		}
		if !p.deliver(pumpItem{msg: msg, fromCmd: true}) {
			p.resolved()
		}
	}()
}

// began records a dispatched command; resolved accounts for one, waking a
// pumpSettled that was waiting for the last of them.
func (p *pump) began() {
	p.stateMu.Lock()
	p.outstanding++
	p.stateMu.Unlock()
}

func (p *pump) resolved() {
	p.stateMu.Lock()
	p.outstanding--
	p.stateMu.Unlock()
	select {
	case p.quietened <- struct{}{}:
	default:
	}
}

// quiet reports that there is nothing left for the pump to do: no dispatched
// command is owed an Update, nothing is queued and the session has published
// nothing the reader has not passed on.
//
// It is only sound with the reader parked, which is why only pumpSettled calls
// it, and only from inside the rendezvous. With the reader stopped at the top of
// its loop there are exactly two places an event can be — still on the session's
// stream, or already in msgs — and both are counted here. Everything that could
// publish an event is likewise accounted for: a command's goroutine is
// outstanding until its message has been applied, and the only other publisher
// is the test's own goroutine, which is inside pumpSettled.
func (p *pump) quiet() bool {
	if len(p.msgs) != 0 || len(p.ctrl.Events()) != 0 {
		return false
	}
	p.stateMu.Lock()
	defer p.stateMu.Unlock()
	return p.outstanding == 0
}

// sync runs the engine's own barrier on a goroutine of its own and answers with
// what it came to.
//
// It has to be a goroutine, and it has to be one the reader is not: an engine
// event is published by the log's outbox, so it trails the state it describes,
// and Sync is what waits for the outbox to have delivered. The outbox delivers by
// sending on the primary, which only the reader drains — so a Sync called from
// the reader with the primary full would be waiting for a slot only it can free
// (plan 021 amendment X14). Here the pump's reader is still reading while this
// waits, which is exactly the shape the contract asks for.
//
// A closing log answers ErrLogClosing rather than waiting, which is not a
// failure: everything enqueued is committed by the close itself.
func (p *pump) sync() <-chan error {
	out := make(chan error, 1)
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		// Cleanup closes dead before it closes the engine, so a Sync waiting on
		// a primary nobody will read again gives up rather than outliving the
		// test.
		go func() {
			select {
			case <-p.dead:
				cancel()
			case <-ctx.Done():
			}
		}()
		out <- p.ctrl.Sync(ctx)
	}()
	return out
}

// resumeReader lets a parked reader go on. The reader is waiting for it, unless
// cleanup has already told it to stop.
func (p *pump) resumeReader() {
	select {
	case p.resume <- struct{}{}:
	case <-p.dead:
	}
}

// pending is the quiet() terms as text, for the watchdog's message.
func (p *pump) pending() string {
	p.stateMu.Lock()
	defer p.stateMu.Unlock()
	return fmt.Sprintf("commands outstanding=%d queued=%d unread events=%d",
		p.outstanding, len(p.msgs), len(p.ctrl.Events()))
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
		case item := <-p.msgs:
			m = p.apply(m, item)
			if pred(m) {
				return m
			}
		case <-timeout:
			t.Fatalf("pumpUntil: no state satisfied the predicate in %s\n"+
				"status=%s err=%q cancelled=%v prompted=%v queued=%q armed=%v %s\n%s",
				pumpWatchdog, m.status, m.err, m.cancelled, m.prompted,
				queueTexts(m), sendNowArmed(m), p.pending(), plainView(m))
		}
	}
}

// pumpSettled returns once nothing the pump dispatched is still owed an Update
// and nothing is queued. It is the barrier for the claim "that turn is
// completely over": stopping on a predicate can return the moment the state the
// predicate names is reached, with the command that ran the turn still to
// report, and a test that then started another turn would never have applied the
// old turn's completion — so a regression in which it lands on the new turn
// would pass.
//
// It is a real barrier, not a poll. Two things make it one. The counters are
// signalled by the goroutines that resolve a command and by the apply step
// below; and the state is only ever inspected with the event reader stopped at
// a point where it holds nothing, so an event cannot be in flight in a place
// the inspection does not look. A flag the reader set after its receive could
// not promise that: the receive and the flag are two steps, and an event taken
// off the stream between them is in neither channel.
//
// A turn held open has its prompt command outstanding on purpose, so this is
// something a test calls where "the turn is over" is the claim, never while a
// turn is held. Called then, the watchdog fails the test and says what is
// outstanding rather than hanging.
//
// The driver is the engine's now, so a turn's ending is not a command's report at
// all: it is an event the log's outbox publishes, and an engine event trails the
// state it describes (plan 021 R2). "No command outstanding and both queues
// empty" can therefore be true a moment before the ending is published, which is
// why every pass runs the engine's own Sync first — on a goroutine, while this
// one keeps reading — and only then asks whether the pump is quiet.
func pumpSettled(t *testing.T, m Model) Model {
	t.Helper()
	p := pumpFor(t, m)
	timeout := deadline()
	for {
		// Anything already queued is applied first: it may be the very message
		// the last outstanding command is waiting to have accounted for, and it
		// is what frees a reader — or a command — blocked on a full queue, which
		// is how the rendezvous below is always reachable.
		m = p.drain(m)

		// The outbox barrier, before the reader is stopped: everything the engine
		// had enqueued by now is in the primary's buffer when this returns, so
		// the rendezvous below can see it. Messages keep being applied while it
		// waits — the reader is what frees the primary.
		synced := p.sync()
	settling:
		for {
			select {
			case err := <-synced:
				if err != nil && !errors.Is(err, agent.ErrLogClosing) && !errors.Is(err, agent.ErrClosed) &&
					!errors.Is(err, context.Canceled) {
					t.Fatalf("pumpSettled: the engine's Sync came back with %v", err)
				}
				break settling
			case item := <-p.msgs:
				m = p.apply(m, item)
			case <-timeout:
				t.Fatalf("pumpSettled: the engine's outbox was not delivered in %s (%s)\n%s",
					pumpWatchdog, p.pending(), plainView(m))
			}
		}

		parked := false
	rendezvous:
		for {
			select {
			case <-p.parked:
				parked = true
				break rendezvous
			case <-p.readerGone:
				// There is no reader to stop, so there is no reader holding
				// anything either.
				break rendezvous
			case item := <-p.msgs:
				m = p.apply(m, item)
			case <-timeout:
				t.Fatalf("pumpSettled: the event reader never came to a stop in %s (%s)\n%s",
					pumpWatchdog, p.pending(), plainView(m))
			}
		}
		quiet := p.quiet()
		if parked {
			p.resumeReader()
		}
		if quiet {
			return m
		}
		select {
		case item := <-p.msgs:
			m = p.apply(m, item)
		case <-p.quietened:
		case <-timeout:
			t.Fatalf("pumpSettled: the pump was still busy after %s (%s)\n"+
				"a turn held open keeps its prompt command outstanding — release it first\n%s",
				pumpWatchdog, p.pending(), plainView(m))
		}
	}
}

// drain applies everything already queued, without waiting for more.
func (p *pump) drain(m Model) Model {
	for {
		select {
		case item := <-p.msgs:
			m = p.apply(m, item)
		default:
			return m
		}
	}
}

// apply is the one place a message reaches Update: it runs the handler,
// dispatches what it asked for, and accounts for the command that reported it.
func (p *pump) apply(m Model, item pumpItem) Model {
	tm, cmd := m.Update(item.msg)
	next := tm.(Model)
	p.dispatch(cmd)
	if item.fromCmd {
		p.resolved()
	}
	return next
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
	if _, err := sess.Cancel(context.Background()); err != nil {
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

// turnsDrawn is how many turns the model has drawn a user block for, which is the
// barrier for "the model has applied that turn's start". Counting the prompts the
// session was given is not: the engine claims a turn — which is where the prompt is
// recorded — a few statements before it enqueues the started that tells a client
// about it, so turnsReached can be true with nothing drawn yet. Use this wherever
// the next assertion is about the model rather than about the session, and
// turnsReached where "these texts reached the agent" is the claim.
func turnsDrawn(n int) func(Model) bool {
	return func(m Model) bool { return len(texts(m, entryUser)) == n }
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

// enqueueRow puts a row in the band without pressing the keys for it, and
// unqueueRow takes one out. Both go through the engine, because from C4 there is
// one queue owner per caller: the Stub keeps a queue of its own until the queue
// leaves the provider seam, but nothing the TUI draws comes from it.
func enqueueRow(t *testing.T, m Model, text string) {
	t.Helper()
	if m.eng == nil {
		t.Fatal("the model has no engine to queue through")
	}
	if _, err := m.eng.Queue(engine.Command{}, text); err != nil {
		t.Fatalf("queueing %q: %v", text, err)
	}
}

func unqueueRow(t *testing.T, m Model, id string) {
	t.Helper()
	if m.eng == nil {
		t.Fatal("the model has no engine to unqueue through")
	}
	if _, err := m.eng.Unqueue(engine.Command{}, id); err != nil {
		t.Fatalf("unqueueing %q: %v", id, err)
	}
}

// queuedRows is craze's message queue, in send order: the engine's, which is the
// one authority on it now.
func queuedRows(m Model) []agent.QueuedPrompt {
	if m.eng == nil {
		return nil
	}
	return m.eng.State().Queue
}

// queueTexts is the queued messages, in order. The band draws them, but a test
// that wants to name them needs the list.
func queueTexts(m Model) []string {
	rows := queuedRows(m)
	out := make([]string, 0, len(rows))
	for _, p := range rows {
		out = append(out, p.Text)
	}
	return out
}

// queueIDs is the same list by id. Ids are not drawn — they are what a verb
// names a row by — so there is nothing on screen to read them from.
func queueIDs(m Model) []string {
	rows := queuedRows(m)
	out := make([]string, 0, len(rows))
	for _, p := range rows {
		out = append(out, p.ID)
	}
	return out
}

// sendNowArmed is a confirmed send-now waiting for the cancelled turn to
// settle. Nothing is drawn for it — that is the point of it: the row stays in
// the band and the draft stays in the composer until it fires — so the state
// itself is the only witness, and the state is the engine's.
func sendNowArmed(m Model) bool { return m.sendNowPending() }

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
	eng, err := engine.New(stub, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = eng.Close() })
	reader := waitEvent(eng)
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

// TestPumpSettledWaitsForACommandThatReportsLate is the barrier's own test: a
// command that reports at a moment the test does not control still has its
// message applied before pumpSettled returns. A barrier that returned first
// would let the next turn start over a message belonging to the last one, which
// is the whole reason it exists.
func TestPumpSettledWaitsForACommandThatReportsLate(t *testing.T) {
	m, _ := scriptedModel(t)
	release := make(chan struct{})
	m = pumpCmd(t, m, func() tea.Msg {
		<-release
		return actionErrMsg{err: errors.New("late")}
	})
	go func() { close(release) }()
	m = pumpSettled(t, m)
	if got := texts(m, entryError); len(got) != 1 || got[0] != "late" {
		t.Fatalf("the late message was not applied: %q", got)
	}
}

// TestPumpSettledWaitsForAnEventTheReaderHasInHand is the other half, and the
// one a flag could not hold. The reader is pinned in the window between taking
// an event off the stream and queueing it: the session's channel is empty, the
// message queue is empty, and no command is outstanding — so every counter says
// "quiet" while an event is on its way to the transcript. Only a rendezvous with
// the reader itself can tell the difference, and this is the test that it does.
func TestPumpSettledWaitsForAnEventTheReaderHasInHand(t *testing.T) {
	m, sess := scriptedModel(t)
	p := pumpFor(t, m)
	pinned, released := make(chan struct{}), make(chan struct{})
	var once sync.Once
	p.setReceivedHook(func() {
		once.Do(func() {
			close(pinned)
			<-released
		})
	})

	sess.Emit(agent.Event{Type: agent.EventText, Text: "IN HAND"})
	awaitBarrier(t, pinned, "the reader taking the event off the stream")
	// Freed at a moment this test does not control, so pumpSettled has to wait
	// for the reader and then for the event it was holding.
	go func() { close(released) }()
	m = pumpSettled(t, m)
	if !strings.Contains(plainView(m), "IN HAND") {
		t.Fatalf("pumpSettled returned before the event the reader held was applied:\n%s", plainView(m))
	}
}
