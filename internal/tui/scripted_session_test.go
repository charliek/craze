package tui

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/charliek/craze/internal/agent"
)

// scriptedSession is a test-only decorator over *Stub. The Stub itself drives
// the frame goldens and is production code, so the endings it cannot produce —
// a turn that fails, a prompt the session refuses, a cancel that never reaches
// the agent, a turn held open at a barrier and then released *cleanly* — are
// added out here instead.
//
// It embeds *Stub rather than reimplementing it, so every method it does not
// name passes straight through: Emit, the setters, Snapshot, Events, Prompts,
// CancelsSent, and anything later commits promote (the event log accessor,
// for one). It overrides exactly two: Begin, to substitute a scripted
// continuation, and Cancel, to fail one.
//
// What it must not do is lie about the Stub's own turn bookkeeping, because
// Interject and Cancel are both decided from it. The decorator lives in
// package tui, so it
// sets the same fields under the same mutex the Stub's own run does — claimed,
// inPrompt, doneEmitted — and the claim itself is always the Stub's: Begin calls
// through, so Prompts() records the prompt, a second Begin is refused, and a
// cancel that lands before the continuation runs still withdraws. Nothing here
// is faked.
type scriptedSession struct {
	*Stub

	// admit makes the whole of Begin one critical section: the busy check, the
	// Stub's own claim and the script dequeue. Without it two concurrent Begins
	// can both read "not busy", one claims and the other takes the Stub's
	// refusal — and then swaps it for a scripted continuation, so two turns run
	// where the Stub allows one. The model calls Begin from one goroutine today;
	// the engine will call it from its own.
	//
	// Lock order: admit → Stub.mu, and admit → mu. Nothing under either of those
	// ever calls back into the decorator, so it cannot cycle.
	admit sync.Mutex

	mu        sync.Mutex
	scripts   []*scriptedTurn
	cancelErr error
	// cancels closes on the first Cancel the session is given, so a test can
	// order a cancel it did not make itself — the engine makes them now, on
	// goroutines of its own — against a continuation it is holding.
	cancels     chan struct{}
	cancelsOnce sync.Once
	// cancelHold, when non-nil, makes the next Cancel wait before it does
	// anything. It is what keeps a turn alive across the arm of a send-now: the
	// cancel the engine makes for the arm is the one thing that would otherwise
	// end that turn at once, on a goroutine no test can hold back.
	cancelHold chan struct{}
}

func newScriptedSession() *scriptedSession {
	return &scriptedSession{Stub: NewStub(), cancels: make(chan struct{})}
}

// Cancels closes once the session has been handed a Cancel. It is the barrier
// for the one window a cancel's own command used to pin from the test's
// goroutine: the model no longer cancels anything itself, so "the cancel has
// reached the session" is something only the session can say.
func (s *scriptedSession) Cancels() <-chan struct{} { return s.cancels }

// Script queues a script for the next prompt and hands it back, so the test can
// wait on its barriers. Prompts with no script left fall straight through to the
// Stub. A script is built whole before it is queued: this mutex is the
// happens-before edge to the turn's own goroutine, which reads it through take.
func (s *scriptedSession) Script(sc *scriptedTurn) *scriptedTurn {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.scripts = append(s.scripts, sc)
	return sc
}

// FailNextCancel makes the next Cancel come back with err, having done nothing:
// the turn keeps running, which is exactly what a cancel that never reached the
// agent leaves behind.
func (s *scriptedSession) FailNextCancel(err error) {
	s.mu.Lock()
	s.cancelErr = err
	s.mu.Unlock()
}

// HoldNextCancel makes the next Cancel stop on arrival, before it touches the
// turn, and answers with the func that lets it go on. Cancels() closes first, so
// a test knows the cancel is there and held; the turn it was for keeps running
// until then, which is how a test stands in the window between an arm and its
// cancel, or lets that turn end some way other than by the cancel. The release is
// idempotent, and closing the session frees a held cancel too.
func (s *scriptedSession) HoldNextCancel() func() {
	hold := make(chan struct{})
	var once sync.Once
	s.mu.Lock()
	s.cancelHold = hold
	s.mu.Unlock()
	return func() { once.Do(func() { close(hold) }) }
}

func (s *scriptedSession) takeCancelHold() chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	hold := s.cancelHold
	s.cancelHold = nil
	return hold
}

func (s *scriptedSession) takeScript() *scriptedTurn {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.scripts) == 0 {
		return nil
	}
	sc := s.scripts[0]
	s.scripts = s.scripts[1:]
	return sc
}

// Begin is the Stub's claim with a scripted continuation behind it. A Begin the
// Stub would refuse — a prompt already claimed, or a turn already open — keeps
// its refusal and consumes no script: a script may not override the
// one-in-flight rule the model is tested against. The three steps are one
// critical section under admit, so concurrent callers cannot both be admitted.
func (s *scriptedSession) Begin(text string) func(context.Context) (agent.Result, error) {
	s.admit.Lock()
	defer s.admit.Unlock()
	busy := s.stubBusy()
	run := s.Stub.Begin(text)
	if busy {
		return run
	}
	sc := s.takeScript()
	if sc == nil {
		return run
	}
	return func(ctx context.Context) (agent.Result, error) { return sc.run(ctx, s.Stub) }
}

// stubBusy reports whether the Stub is already holding a prompt, in which case
// its Begin refuses and no script may override that. It is only ever called with
// admit held. The qualifier on the mutex is not decoration: the decorator has
// one of its own.
func (s *scriptedSession) stubBusy() bool {
	s.Stub.mu.Lock()
	defer s.Stub.mu.Unlock()
	return s.claimed || s.inPrompt
}

// Cancel fails once when a test armed a failure — reporting a zero
// CancelOutcome alongside it, since a cancel that never ran cannot know
// anything — and otherwise is the Stub's.
func (s *scriptedSession) Cancel(ctx context.Context) (agent.CancelOutcome, error) {
	s.mu.Lock()
	err := s.cancelErr
	s.cancelErr = nil
	s.mu.Unlock()
	// Before the Stub's own Cancel, and before a failure is returned: the barrier
	// says a cancel arrived, not what became of it.
	s.cancelsOnce.Do(func() { close(s.cancels) })
	if hold := s.takeCancelHold(); hold != nil {
		select {
		case <-hold:
		case <-s.closed:
		case <-ctx.Done():
		}
	}
	if err != nil {
		return agent.CancelOutcome{}, err
	}
	return s.Stub.Cancel(ctx)
}

var (
	_ agent.Session     = (*scriptedSession)(nil)
	_ agent.EventSource = (*scriptedSession)(nil)
)

// scriptedTurn is one prompt's answer. Every field is set before the script is
// queued; nothing writes it afterwards.
type scriptedTurn struct {
	// claimed and openWhen are the window before the turn is open: claimed closes
	// as the continuation is entered, before it has looked at anything, and it
	// then waits on openWhen. The prompt is claimed and nothing is on the wire,
	// which is what a cancel landing right after Enter has to find — it marks the
	// claim, and the continuation then withdraws. The engine runs the continuation
	// on a goroutine of its own, so a test cannot hold it back by not running a
	// command any more; this is where it holds it instead.
	claimed  chan struct{}
	openWhen chan struct{}
	openOnce sync.Once
	// opened closes once the continuation has opened the turn. It is the
	// handshake a test waits on before it emits into the turn or cancels it:
	// without it a cancel could reach a claim that has not opened yet and the
	// prompt would withdraw instead, which is a different scenario.
	opened chan struct{}
	// release, when non-nil, holds the turn open until Release closes it. A
	// cancel — or the session closing — releases it too, and then the turn ends
	// cancelled, exactly as a turn on the wire does.
	release     chan struct{}
	releaseOnce sync.Once
	// events go out when the turn is released, before its ending. A test that
	// wants them to land *while* the turn is held emits them itself: the log
	// orders them, so anything published before Release is applied before the
	// ending.
	events []agent.Event
	// fail is emitted as EventError and then returned, which is the live
	// session's order: it publishes the error and Prompt returns it, so the TUI
	// hears about one failure twice.
	fail error
	// refuse is returned with no event and no turn at all — the shape of
	// ErrForeignTurn and ErrPromptInFlight, which the wire never reports.
	refuse error
	// stop is the clean ending's stop reason; "" means end_turn.
	stop string
	// ended closes once the turn's one terminal event has been published, and
	// pauseReturn then holds the continuation just short of returning. Together
	// they are the window the model's two-ending rule exists for: the stream has
	// ended and the prompt has not returned. They also make the turn's own
	// terminal event the only one there is, so a test cannot paper over a lost
	// first ending with a second one emitted by hand.
	ended       chan struct{}
	pauseReturn chan struct{}
	returnOnce  sync.Once
}

// scriptHeld is a turn held open at a barrier: the test decides when it ends,
// and it ends cleanly unless a cancel gets there first.
func scriptHeld() *scriptedTurn {
	return &scriptedTurn{opened: make(chan struct{}), release: make(chan struct{})}
}

// scriptFailed is a turn that fails as soon as it opens.
func scriptFailed(err error) *scriptedTurn {
	return &scriptedTurn{opened: make(chan struct{}), fail: err}
}

// scriptWithheld is a turn held before it opens: claimed, nothing on the wire,
// and going nowhere until Open. Released, it opens and ends cleanly; cancelled
// while it waits, it withdraws with nothing ever sent.
func scriptWithheld() *scriptedTurn {
	return &scriptedTurn{
		claimed:  make(chan struct{}),
		openWhen: make(chan struct{}),
		opened:   make(chan struct{}),
	}
}

// Open lets a withheld continuation go on to its opening. It is idempotent.
func (sc *scriptedTurn) Open() {
	if sc.openWhen == nil {
		return
	}
	sc.openOnce.Do(func() { close(sc.openWhen) })
}

// scriptRefused is a prompt the session never accepts: no turn, no event, and
// the claim released.
func scriptRefused(err error) *scriptedTurn { return &scriptedTurn{refuse: err} }

// held makes a script wait at a barrier before it produces its ending, so a
// failing turn can be held open long enough for a test to queue behind it.
func (sc *scriptedTurn) held() *scriptedTurn {
	sc.release = make(chan struct{})
	return sc
}

// endsThenWaits makes the turn stop between its ending and its return: ended
// closes when the one terminal event has gone out — through the Stub's own
// markDone and Emit, so doneEmitted is honest — and Return lets the continuation
// finish. A test that needs "the stream has ended, the prompt has not" uses this
// rather than publishing an ending of its own, because a hand-emitted ending
// beside the turn's own would let a model that dropped the first be repaired by
// the second.
func (sc *scriptedTurn) endsThenWaits() *scriptedTurn {
	sc.ended = make(chan struct{})
	sc.pauseReturn = make(chan struct{})
	return sc
}

// Release ends a held turn. It is idempotent, so a test may release a turn the
// cleanup would have closed anyway.
func (sc *scriptedTurn) Release() {
	if sc.release == nil {
		return
	}
	sc.releaseOnce.Do(func() { close(sc.release) })
}

// Return lets a continuation paused after its ending go on and return.
func (sc *scriptedTurn) Return() {
	if sc.pauseReturn == nil {
		return
	}
	sc.returnOnce.Do(func() { close(sc.pauseReturn) })
}

// run is the continuation. It keeps the Stub's turn state honest at every step,
// because that is what the queue guards, Interject and Cancel read.
func (sc *scriptedTurn) run(ctx context.Context, s *Stub) (agent.Result, error) {
	if sc.claimed != nil {
		// Claimed and not open: the prompt is the session's, nothing has reached
		// the agent, and a cancel arriving now is this prompt's.
		close(sc.claimed)
		select {
		case <-sc.openWhen:
		case <-s.closed:
		case <-ctx.Done():
		}
	}
	if sc.refuse != nil {
		// Nothing opened, so nothing to close: only the claim is given back,
		// the way the Stub's own withdraw path gives it back.
		s.mu.Lock()
		s.claimed, s.cancelling = false, false
		s.mu.Unlock()
		return agent.Result{}, sc.refuse
	}
	// The registry's name for this turn, as the Stub's own run mints it: OUTSIDE
	// s.mu, because s.mu and the registry's are never nested (plan 021 §3.3),
	// and before the locked section that installs it — which is the window a
	// cancel can land in, and the one this decorator has to be able to reproduce.
	// A token nothing is ever parked against costs one number and resolves
	// nothing.
	token := s.beginAskTurn()
	s.mu.Lock()
	if s.cancelling {
		// Cancelled between the claim and here: withdraw, as the Stub does. The
		// deferred EndTurn below never runs for this path, so the token is
		// retired here: an ask can no longer reach it.
		s.claimed, s.cancelling = false, false
		s.mu.Unlock()
		s.asks.EndTurn(token)
		return agent.Result{}, agent.ErrPromptCancelled
	}
	// A card a test emits while this turn runs is parked against it, and the
	// turn's end — or a cancel — takes exactly those away.
	s.inPrompt = true
	s.token = token
	s.doneEmitted = false
	s.mu.Unlock()
	defer func() {
		s.asks.EndTurn(token)
		s.mu.Lock()
		s.claimed, s.cancelling, s.inPrompt = false, false, false
		s.token = agent.TurnToken{}
		s.mu.Unlock()
	}()
	if sc.opened != nil {
		close(sc.opened)
	}
	cancelled := false
	if sc.release != nil {
		select {
		case <-sc.release:
		case <-s.cancel:
			cancelled = true
		case <-s.closed:
			cancelled = true
		case <-ctx.Done():
			cancelled = true
		}
	}
	for _, ev := range sc.events {
		s.Emit(ev)
	}
	switch {
	case cancelled:
		sc.end(ctx, s, token, agent.Event{Type: agent.EventDone, StopReason: stopCancelled})
		return agent.Result{StopReason: stopCancelled}, nil
	case sc.fail != nil:
		sc.end(ctx, s, token, agent.Event{Type: agent.EventError, Err: sc.fail})
		return agent.Result{}, sc.fail
	default:
		stop := sc.stop
		if stop == "" {
			stop = "end_turn"
		}
		sc.end(ctx, s, token, agent.Event{Type: agent.EventDone, StopReason: stop})
		return agent.Result{StopReason: stop}, nil
	}
}

// end publishes the turn's one terminal event and, when the script asked for it,
// holds the continuation there — after the ending, before the return. The turn
// ends for the registry first, so a card it still held ends before the turn's
// own ending says the turn is over (plan 021 §3.6).
func (sc *scriptedTurn) end(ctx context.Context, s *Stub, token agent.TurnToken, ev agent.Event) {
	s.endAskTurn(token)
	s.markDone()
	s.Emit(ev)
	if sc.ended != nil {
		close(sc.ended)
	}
	if sc.pauseReturn == nil {
		return
	}
	select {
	case <-sc.pauseReturn:
	case <-s.closed:
	case <-ctx.Done():
	}
}

// ------------------------------------------------------------------ fixtures

// scriptedModel is sized(t) over a scripted session.
func scriptedModel(t *testing.T) (Model, *scriptedSession) {
	t.Helper()
	isolateSkillsHome(t)
	s := newScriptedSession()
	return startSession(t, s, t.TempDir(), 80, 24), s
}

// startScripted sends text with sc queued for it and waits for the turn to be
// open, so what the test does next lands inside the turn and not beside it.
func startScripted(t *testing.T, m Model, s *scriptedSession, text string, sc *scriptedTurn) Model {
	t.Helper()
	s.Script(sc)
	m = pumpEnter(t, m, text)
	if m.status != statusWorking {
		t.Fatalf("the scripted send left the model %s, not working", m.status)
	}
	if sc.opened != nil {
		awaitBarrier(t, sc.opened, "the scripted turn opening")
	}
	return m
}

// ---------------------------------------------------------- the decorator's own

// TestScriptedSessionAdmitsOneBeginAtATime is the admission mutex's test. Two
// Begins race for one claim: whichever wins gets the queued script, and the
// other must keep the Stub's refusal — with the busy check and the claim in
// separate critical sections, both could read "not busy" and the loser would
// take the script too, running a second turn the Stub allows no room for.
func TestScriptedSessionAdmitsOneBeginAtATime(t *testing.T) {
	sess := newScriptedSession()
	t.Cleanup(func() { _ = sess.Close() })
	scripted := errors.New("the scripted refusal")
	sess.Script(scriptRefused(scripted))

	// The two admissions race; the continuations are run afterwards, so the claim
	// the winner took is still held while the loser is admitted — which is the
	// state the mutex is there to make unambiguous.
	start := make(chan struct{})
	runs := make([]func(context.Context) (agent.Result, error), 2)
	var wg sync.WaitGroup
	for i := range runs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			runs[i] = sess.Begin(fmt.Sprintf("prompt %d", i))
		}(i)
	}
	close(start)
	wg.Wait()

	errs := make([]error, len(runs))
	for i, run := range runs {
		_, errs[i] = run(context.Background())
	}

	var tookScript, refused int
	for _, err := range errs {
		switch {
		case errors.Is(err, scripted):
			tookScript++
		case errors.Is(err, agent.ErrPromptInFlight):
			refused++
		default:
			t.Fatalf("a Begin came back with %v", err)
		}
	}
	if tookScript != 1 || refused != 1 {
		t.Fatalf("two concurrent Begins: %d took the script, %d were refused", tookScript, refused)
	}
	// Both are recorded: the claim is the Stub's, so a refused prompt is still a
	// prompt the session was handed.
	if got := sess.Prompts(); len(got) != 2 {
		t.Fatalf("prompts %q, want both recorded", got)
	}
}
