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
	asks *agent.AskRegistry
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
	// snap is what the settings verbs mutate and Snapshot reports; sets counts
	// the ones that changed something.
	snap agent.Snapshot
	sets int
	// setHold, when non-nil, holds every settings verb at its "provider" call,
	// and setsHeld counts the ones parked there now: the barrier a test uses to
	// know the worker has taken one and nothing has changed yet. setErr fails
	// the call there instead.
	setHold  chan struct{}
	setsHeld int
	setErr   error
	// setResolve is a provider that answers with something other than what it
	// was asked for (resolveSets), and setSilent one whose deltas carry no
	// ticket (setsWithoutATicket).
	setResolve func(string) string
	setSilent  bool
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
	log := agent.NewEventLog(o)
	s := &fakeSession{
		log:       log,
		asks:      agent.NewAskRegistry(log, nil),
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
	out := s.snap
	out.SessionID = "fake-1"
	out.ForeignTurn = s.foreign
	return out
}

// holdNextSets makes every settings verb from now wait at the "provider" call
// until the returned barrier is closed: the point at which a Set has been taken
// by the worker and nothing has changed yet.
func (s *fakeSession) holdNextSets() (release func()) {
	hold := make(chan struct{})
	s.mu.Lock()
	s.setHold = hold
	s.mu.Unlock()
	return sync.OnceFunc(func() {
		s.mu.Lock()
		s.setHold = nil
		s.mu.Unlock()
		close(hold)
	})
}

// failSets makes every settings verb from now return err from its "provider"
// call, with nothing mutated and nothing published.
func (s *fakeSession) failSets(err error) {
	s.mu.Lock()
	s.setErr = err
	s.mu.Unlock()
}

// setCalls is how many settings verbs have got past the provider and changed
// something.
func (s *fakeSession) setCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sets
}

// providerMode is an update the agent made itself — live.go's onUpdate for a
// current_mode_update — with exactly that site's shape: the mutation and the
// delta in one locked section, Event.Mode filled beside the section because
// this one IS the agent's own, and the flush afterwards, on the read loop, with
// the lock released.
func (s *fakeSession) providerMode(id string) {
	s.mu.Lock()
	s.snap.CurrentMode = id
	s.log.Enqueue(agent.Event{
		Type: agent.EventMeta, Mode: id, State: &agent.StateDelta{Mode: &id}, At: s.clock(),
	})
	s.mu.Unlock()
	_ = s.log.Flush(context.Background(), s.done)
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

// The rest of the seam the engine does not drive.
func (s *fakeSession) Interject(context.Context, string) error { return agent.ErrUnsupported }

// Asks is the session's agent.AskSource, which the engine refuses a session
// without (plan 021 §3.6). The driver's schedules are about turns, so it is
// empty unless a test parks one with openAsk.
func (s *fakeSession) Asks() *agent.AskRegistry { return s.asks }

// openAsk parks one ask against no turn, which is what makes a cancel with no
// turn of craze's own something the engine accepts (§3.7).
func (s *fakeSession) openAsk(t *testing.T) *agent.Ask {
	t.Helper()
	a, err := s.asks.Open(context.Background(), agent.TurnToken{}, agent.AskRequest{
		Kind: agent.AskPermission,
		Body: agent.AskBody{Permission: &agent.PermissionEvent{Tool: "Shell"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// The settings verbs, with the live session's shape: the "provider" is asked
// first (setErr stands in for it), and a change it took is written into the
// snapshot and its delta enqueued in one locked section. Nothing here is a
// stand-in for the ordering rule itself — that is the session's, and the live
// session and the Stub are where it is tested — but the engine's own tests need
// a session whose setters can be made to fail, to succeed, and to be held.
func (s *fakeSession) SetModel(ctx context.Context, cause, id string) (agent.SetOutcome, error) {
	return s.set(ctx, cause, id, func(v string, st *agent.StateDelta) { st.Model = &v }, func(v string, snap *agent.Snapshot) {
		snap.CurrentModel = v
	})
}

func (s *fakeSession) SetMode(ctx context.Context, cause, id string) (agent.SetOutcome, error) {
	return s.set(ctx, cause, id, func(v string, st *agent.StateDelta) { st.Mode = &v }, func(v string, snap *agent.Snapshot) {
		snap.CurrentMode = v
	})
}

func (s *fakeSession) SetConfig(ctx context.Context, cause, id, value string) (agent.SetOutcome, error) {
	return s.set(ctx, cause, value, func(v string, st *agent.StateDelta) {
		st.Config = &agent.ConfigState{Options: []agent.ConfigOption{{ID: id, Current: v}}}
	}, func(v string, snap *agent.Snapshot) {
		snap.Config = []agent.ConfigOption{{ID: id, Current: v}}
	})
}

// set is the three verbs' one body: the hook a test uses to hold or fail the
// "provider" call, then the mutation and its delta under s.mu. value is what
// the session is now at, answered as the real ones answer it — from inside the
// locked section that wrote it (agent.SetOutcome).
//
// The hold is a provider call this fake is parked in, and it ends three ways:
// the test releasing it, the caller's context, and **the session closing**,
// which is what a real provider call does when its client is torn down. That
// last one is what makes engine.Close's promise testable — a request in flight
// is freed by the session's close rather than held by the worker's loop.
func (s *fakeSession) set(ctx context.Context, cause, value string, delta func(string, *agent.StateDelta), apply func(string, *agent.Snapshot)) (agent.SetOutcome, error) {
	s.mu.Lock()
	hold, fail, resolve, silent := s.setHold, s.setErr, s.setResolve, s.setSilent
	if hold != nil {
		s.setsHeld++
	}
	s.mu.Unlock()
	if hold != nil {
		var err error
		select {
		case <-hold:
		case <-ctx.Done():
			err = ctx.Err()
		case <-s.done:
			err = errSessionClosed
		}
		s.mu.Lock()
		s.setsHeld--
		s.mu.Unlock()
		if err != nil {
			return agent.SetOutcome{}, err
		}
	}
	if fail != nil {
		return agent.SetOutcome{}, fail
	}
	if resolve != nil {
		// A provider resolving what it was sent, as native does with an empty
		// effort and a model alias: the value the session ends at, and the one
		// its delta carries, is this one and not the request.
		value = resolve(value)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sets++
	apply(value, &s.snap)
	st := &agent.StateDelta{}
	delta(value, st)
	ev := agent.Event{Type: agent.EventMeta, State: st, Cause: cause, At: s.clock()}
	if silent {
		// A change that took with no revision to show for it: what a flush that
		// gave up leaves behind (engine.SetResult.Rev's "no revision").
		s.log.Enqueue(ev)
		return agent.SetOutcome{Value: value}, nil
	}
	return agent.SetOutcome{Value: value, Ticket: s.log.EnqueueTicket(ev)}, nil
}

// resolveSets makes every later settings verb answer with f(value) rather than
// with what it was asked for, and setsWithoutATicket makes them publish their
// delta with no ticket at all — "the change happened, the revision could not be
// learned".
func (s *fakeSession) resolveSets(f func(string) string) {
	s.mu.Lock()
	s.setResolve = f
	s.mu.Unlock()
}

func (s *fakeSession) setsWithoutATicket() {
	s.mu.Lock()
	s.setSilent = true
	s.mu.Unlock()
}

// errSessionClosed is what a provider call parked in this fake answers when the
// session closes under it: the shape of a real client whose transport has gone.
var errSessionClosed = errors.New("fake: session closed")

func (s *fakeSession) SetTitle(cause, title string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.log.OutboxRoom() {
		return agent.ErrSetUnavailable
	}
	s.snap.Title = title
	s.log.Enqueue(agent.Event{
		Type: agent.EventMeta, State: &agent.StateDelta{Title: &title}, Cause: cause, At: s.clock(),
	})
	return nil
}

var (
	_ agent.Session   = (*fakeSession)(nil)
	_ agent.LogOwner  = (*fakeSession)(nil)
	_ agent.Clocked   = (*fakeSession)(nil)
	_ agent.AskSource = (*fakeSession)(nil)
)
