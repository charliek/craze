package tui

import (
	"context"
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
// name passes straight through: Emit, the queue verbs, the setters, Snapshot,
// Events, Prompts, CancelsSent, and anything later commits promote (the event
// log accessor, for one). It overrides exactly two: Begin, to substitute a
// scripted continuation, and Cancel, to fail one.
//
// What it must not do is lie about the Stub's own turn bookkeeping, because the
// queue guards (PopQueue and TakeQueued refuse while a turn is open), Interject
// and Cancel are all decided from it. The decorator lives in package tui, so it
// sets the same fields under the same mutex the Stub's own run does — claimed,
// inPrompt, doneEmitted — and the claim itself is always the Stub's: Begin calls
// through, so Prompts() records the prompt, a second Begin is refused, and a
// cancel that lands before the continuation runs still withdraws. Nothing here
// is faked.
type scriptedSession struct {
	*Stub

	mu        sync.Mutex
	scripts   []*scriptedTurn
	cancelErr error
}

func newScriptedSession() *scriptedSession { return &scriptedSession{Stub: NewStub()} }

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
// its refusal and consumes no script: a script may not override the one-in-flight
// rule the model is tested against.
func (s *scriptedSession) Begin(text string) func(context.Context) (agent.Result, error) {
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
// its Begin refuses and no script may override that. The qualifier on the mutex
// is not decoration: the decorator has one of its own.
func (s *scriptedSession) stubBusy() bool {
	s.Stub.mu.Lock()
	defer s.Stub.mu.Unlock()
	return s.claimed || s.inPrompt
}

// Cancel fails once when a test armed a failure, and otherwise is the Stub's.
func (s *scriptedSession) Cancel(ctx context.Context) error {
	s.mu.Lock()
	err := s.cancelErr
	s.cancelErr = nil
	s.mu.Unlock()
	if err != nil {
		return err
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

// scriptRefused is a prompt the session never accepts: no turn, no event, and
// the claim released.
func scriptRefused(err error) *scriptedTurn { return &scriptedTurn{refuse: err} }

// held makes a script wait at a barrier before it produces its ending, so a
// failing turn can be held open long enough for a test to queue behind it.
func (sc *scriptedTurn) held() *scriptedTurn {
	sc.release = make(chan struct{})
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

// run is the continuation. It keeps the Stub's turn state honest at every step,
// because that is what the queue guards, Interject and Cancel read.
func (sc *scriptedTurn) run(ctx context.Context, s *Stub) (agent.Result, error) {
	if sc.refuse != nil {
		// Nothing opened, so nothing to close: only the claim is given back,
		// the way the Stub's own withdraw path gives it back.
		s.mu.Lock()
		s.claimed, s.cancelling = false, false
		s.mu.Unlock()
		return agent.Result{}, sc.refuse
	}
	s.mu.Lock()
	if s.cancelling {
		// Cancelled between the claim and here: withdraw, as the Stub does.
		s.claimed, s.cancelling = false, false
		s.mu.Unlock()
		return agent.Result{}, agent.ErrPromptCancelled
	}
	s.inPrompt = true
	s.doneEmitted = false
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.claimed, s.cancelling, s.inPrompt = false, false, false
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
		s.markDone()
		s.Emit(agent.Event{Type: agent.EventDone, StopReason: stopCancelled})
		return agent.Result{StopReason: stopCancelled}, nil
	case sc.fail != nil:
		s.markDone()
		s.Emit(agent.Event{Type: agent.EventError, Err: sc.fail})
		return agent.Result{}, sc.fail
	default:
		stop := sc.stop
		if stop == "" {
			stop = "end_turn"
		}
		s.markDone()
		s.Emit(agent.Event{Type: agent.EventDone, StopReason: stop})
		return agent.Result{StopReason: stop}, nil
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
