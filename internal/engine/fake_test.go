package engine

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
)

// fakeSession is the session the driver's schedules are pinned over. The
// engine is tested over tui.Stub too (engine_stub_test.go), which is the
// session every golden runs on, but the Stub cannot fail a turn, refuse a
// prompt, end a held turn cleanly, or hold a Cancel at its entry, and those are
// the schedules the driver exists for. So this double keeps the seam's
// contract where the engine leans on it — Begin claims on the caller's
// goroutine and waits on nothing, a second claim is refused, a cancel between
// the claim and the opening makes the prompt withdraw, the continuation runs
// once and releases the claim before it returns, and every event goes through
// a real agent.EventLog — and scripts everything else.
//
// Every wait in it is a barrier a test closes. Nothing here sleeps.
type fakeSession struct {
	log  *agent.EventLog
	done chan struct{}

	mu         sync.Mutex
	claimed    bool
	open       bool
	cancelling bool
	foreign    bool
	closed     bool
	scripts    []*script
	begun      []string
	wrote      int
	cancelSig  chan struct{}
	cancelErr  error
	// cancelEntered, when set, is closed by the next Cancel as it is entered,
	// which then waits for cancelRelease: the barrier at Session.Cancel's
	// entry.
	cancelEntered chan struct{}
	cancelRelease chan struct{}
	// ignoreCancel makes a cancel of an open turn a write the turn does not
	// act on: an agent that has been told and has not stopped yet.
	ignoreCancel bool
	beginPanic   bool
	startErr     error
	clock        func() time.Time
	closeOnce    sync.Once
}

// script is one prompt's answer. It is built whole before it is queued.
type script struct {
	// refuse is returned with no turn and no event: the shape of
	// ErrForeignTurn and ErrPromptInFlight. refusing and gate, when set, hold
	// the continuation just short of that return.
	refuse   error
	refusing chan struct{}
	gate     chan struct{}
	// fail is published as EventError and then returned, in the live
	// session's order.
	fail error
	// stop is a clean ending's stop reason; "" is end_turn.
	stop string
	// text, when set, is published as the turn's one chunk before its ending.
	text string
	// opened closes once the turn is open. hold, when non-nil, keeps the turn
	// open until release closes it; a cancel releases it too, and the turn
	// then ends cancelled, as a turn on the wire does.
	opened chan struct{}
	hold   chan struct{}
	once   sync.Once
	// unanswered is what the turn reports it accepted and could not answer.
	unanswered []string
	// silent makes the turn publish nothing at all — no chunk, no ending — and
	// report its result by returning alone. A publish takes the log's publishing
	// boundary, which the outbox's drainer holds while it waits for room in the
	// primary, so a turn that says anything cannot come back while the outbox is
	// undeliverable. A test about what the engine does *while* the outbox is over
	// its bound therefore needs a turn that says nothing; no real session is
	// silent, and nothing but those tests uses it.
	silent bool
}

// silently makes a script's turn end without publishing anything.
func silently(sc *script) *script { sc.silent = true; return sc }

// refusedAtAGate is a refusal held until the test opens its gate.
func refusedAtAGate(err error) *script {
	return &script{refuse: err, refusing: make(chan struct{}), gate: make(chan struct{})}
}

func held() *script {
	return &script{opened: make(chan struct{}), hold: make(chan struct{})}
}

func (sc *script) release() { sc.once.Do(func() { close(sc.hold) }) }

func newFake(t *testing.T, o agent.EventLogOptions) *fakeSession {
	t.Helper()
	s := &fakeSession{
		log:       agent.NewEventLog(o),
		done:      make(chan struct{}),
		cancelSig: make(chan struct{}, 1),
		clock:     func() time.Time { return time.Unix(1_700_000_000, 0).UTC() },
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func (s *fakeSession) script(sc *script) *script {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.scripts = append(s.scripts, sc)
	return sc
}

func (s *fakeSession) EventLog() *agent.EventLog { return s.log }
func (s *fakeSession) Now() time.Time            { return s.clock() }

func (s *fakeSession) emit(ev agent.Event) {
	ev.At = s.clock()
	s.log.Publish(context.Background(), s.done, ev)
}

func (s *fakeSession) Start(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.startErr
}

func (s *fakeSession) Prompt(ctx context.Context, text string) (agent.Result, error) {
	return s.Begin(text)(ctx)
}

func (s *fakeSession) Begin(text string) func(context.Context) (agent.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.beginPanic {
		panic("fakeSession: Begin")
	}
	s.begun = append(s.begun, text)
	if s.claimed || s.open {
		return func(context.Context) (agent.Result, error) { return agent.Result{}, agent.ErrPromptInFlight }
	}
	s.claimed = true
	s.cancelling = false
	select {
	case <-s.cancelSig:
	default:
	}
	var sc *script
	if len(s.scripts) > 0 {
		sc, s.scripts = s.scripts[0], s.scripts[1:]
	}
	if sc == nil {
		sc = &script{}
	}
	return func(ctx context.Context) (agent.Result, error) { return s.run(ctx, text, sc) }
}

func (s *fakeSession) run(ctx context.Context, text string, sc *script) (agent.Result, error) {
	defer func() {
		s.mu.Lock()
		s.claimed, s.open, s.cancelling = false, false, false
		s.mu.Unlock()
	}()
	if sc.refuse != nil {
		// A refusal a test wants to stand beside: refusing closes as the
		// continuation gets here, and it then waits for the gate.
		if sc.refusing != nil {
			close(sc.refusing)
			select {
			case <-sc.gate:
			case <-s.done:
			}
		}
		return agent.Result{}, sc.refuse
	}
	s.mu.Lock()
	if s.cancelling {
		s.mu.Unlock()
		return agent.Result{}, agent.ErrPromptCancelled
	}
	s.open = true
	s.mu.Unlock()
	if sc.opened != nil {
		close(sc.opened)
	}
	cancelled := false
	if sc.hold != nil {
		select {
		case <-sc.hold:
		case <-s.cancelSig:
			cancelled = true
		case <-s.done:
			cancelled = true
		case <-ctx.Done():
			cancelled = true
		}
	}
	res := agent.Result{Unanswered: sc.unanswered}
	emit := s.emit
	if sc.silent {
		emit = func(agent.Event) {}
	}
	switch {
	case cancelled:
		emit(agent.Event{Type: agent.EventDone, StopReason: stopCancelled})
		res.StopReason = stopCancelled
		return res, nil
	case sc.fail != nil:
		emit(agent.Event{Type: agent.EventError, Err: sc.fail})
		return res, sc.fail
	}
	reply := sc.text
	if reply == "" {
		reply = "echo: " + text
	}
	emit(agent.Event{Type: agent.EventText, Text: reply})
	stop := sc.stop
	if stop == "" {
		stop = stopEndTurn
	}
	emit(agent.Event{Type: agent.EventDone, StopReason: stop})
	res.StopReason = stop
	return res, nil
}

func (s *fakeSession) Events() <-chan agent.Event { return s.log.Primary() }

func (s *fakeSession) Cancel(ctx context.Context) (agent.CancelOutcome, error) {
	s.mu.Lock()
	entered, release := s.cancelEntered, s.cancelRelease
	s.cancelEntered, s.cancelRelease = nil, nil
	s.mu.Unlock()
	if entered != nil {
		close(entered)
		select {
		case <-release:
		case <-s.done:
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.cancelErr; err != nil {
		s.cancelErr = nil
		return agent.CancelOutcome{}, err
	}
	out := agent.CancelOutcome{}
	switch {
	case s.claimed && !s.open:
		s.cancelling = true
		out.Withdrew = true
	case s.open:
		s.cancelling = true
		s.wrote++
		out.Wrote = true
		if !s.ignoreCancel {
			select {
			case s.cancelSig <- struct{}{}:
			default:
			}
		}
	default:
		s.wrote++
		out.Wrote, out.Settled = true, true
	}
	return out, nil
}

// holdNextCancel arms the barrier at Cancel's entry: entered closes as the next
// Cancel is entered, and that Cancel then waits for release.
func (s *fakeSession) holdNextCancel() (entered <-chan struct{}, release func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, r := make(chan struct{}), make(chan struct{})
	s.cancelEntered, s.cancelRelease = e, r
	var once sync.Once
	return e, func() { once.Do(func() { close(r) }) }
}

// setForeign moves the foreign-turn flag and then tells everyone, in the order
// the sessions do: the flag first, so whoever the event wakes reads it fresh.
func (s *fakeSession) setForeign(running bool) {
	s.mu.Lock()
	s.foreign = running
	s.mu.Unlock()
	s.emit(agent.Event{Type: agent.EventForeignTurn, ForeignTurn: &agent.ForeignTurnInfo{Running: running}})
}

// setForeignSilently moves the flag alone: the lag between the client's own
// knowledge of a foreign turn and the session's.
func (s *fakeSession) setForeignSilently(running bool) {
	s.mu.Lock()
	s.foreign = running
	s.mu.Unlock()
}

func (s *fakeSession) ForeignTurn() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.foreign
}

func (s *fakeSession) prompts() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.begun...)
}

func (s *fakeSession) cancelsWritten() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.wrote
}

func (s *fakeSession) Snapshot() agent.Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return agent.Snapshot{SessionID: "fake-1", ForeignTurn: s.foreign}
}

func (s *fakeSession) Close() error {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
		close(s.done)
		s.log.Close(context.Background())
	})
	return nil
}

var errFakeUnused = errors.New("fakeSession: not part of the driver's seam")

// The rest of the seam is not the driver's: the queue still on it goes when the
// queue leaves the provider seam, and asks and settings move later.
func (s *fakeSession) Queue(string) (agent.QueuedPrompt, error) {
	return agent.QueuedPrompt{}, errFakeUnused
}
func (s *fakeSession) EditQueued(string, string) error           { return errFakeUnused }
func (s *fakeSession) Unqueue(string) (agent.QueuedPrompt, bool) { return agent.QueuedPrompt{}, false }
func (s *fakeSession) TakeQueued(string) (agent.QueuedPrompt, bool) {
	return agent.QueuedPrompt{}, false
}
func (s *fakeSession) PopQueue() (agent.QueuedPrompt, bool)    { return agent.QueuedPrompt{}, false }
func (s *fakeSession) ClearQueue() int                         { return 0 }
func (s *fakeSession) Interject(context.Context, string) error { return agent.ErrUnsupported }
func (s *fakeSession) AnswerPermission(string, string) error   { return errFakeUnused }
func (s *fakeSession) AnswerQuestion(string, map[string][]string, bool) error {
	return errFakeUnused
}
func (s *fakeSession) AnswerPlan(string, bool) error                   { return errFakeUnused }
func (s *fakeSession) SetModel(context.Context, string) error          { return errFakeUnused }
func (s *fakeSession) SetMode(context.Context, string) error           { return errFakeUnused }
func (s *fakeSession) SetConfig(context.Context, string, string) error { return errFakeUnused }
func (s *fakeSession) SetTitle(string)                                 {}

var (
	_ agent.Session  = (*fakeSession)(nil)
	_ agent.LogOwner = (*fakeSession)(nil)
	_ agent.Clocked  = (*fakeSession)(nil)
)
