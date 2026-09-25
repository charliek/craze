package engine

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
)

// fencedSession is the fake session with an agent.AdmissionFence: the shape of
// a session that can start a turn of its own (native's wake, plan 026 §3.11). It
// records every fence transition and every call the fence exists to guard —
// ForeignTurn, Begin, Cancel — in order, and it is the barrier at the claim and
// at validation: each of those three looks at the fence as it is entered, which
// is exactly where a wake would try to claim, and a fence found down there is a
// wake that could have started in the engine's gap. That, a transition that is
// not one, is recorded as a violation.
//
// Its own mutex is a leaf, taken inside whatever the engine holds and released
// before the fake's own is taken, so it adds no edge the engine's order does not
// already have.
type fencedSession struct {
	*fakeSession

	fmu   sync.Mutex
	up    bool
	calls []string
	bad   []string
	// afterForeign, when set, runs once, after the next ForeignTurn read, with
	// its answer, inside whatever section of the engine's made the read: the
	// barrier a test ends the agent's turn at while the engine holds e.mu.
	afterForeign func(running bool)
	// wakePending is a turn the session is waiting to start of its own (a second
	// background result): it starts the moment the fence comes down — the flag
	// goes up and "wake" is recorded — as native's worker would.
	wakePending bool
}

func (s *fencedSession) FenceUp() {
	s.fmu.Lock()
	defer s.fmu.Unlock()
	if s.up {
		s.bad = append(s.bad, "FenceUp while up")
	}
	s.up = true
	s.calls = append(s.calls, "up")
}

func (s *fencedSession) FenceDown() {
	s.fmu.Lock()
	if !s.up {
		s.bad = append(s.bad, "FenceDown while down")
	}
	s.up = false
	s.calls = append(s.calls, "down")
	wake := s.wakePending
	if wake {
		s.wakePending = false
		s.calls = append(s.calls, "wake")
	}
	s.fmu.Unlock()
	if wake {
		// The fake's own lock after the leaf, never inside it.
		s.setForeignSilently(true)
	}
}

// pendWake makes a turn of the session's own wait for the fence to come down.
func (s *fencedSession) pendWake() {
	s.fmu.Lock()
	defer s.fmu.Unlock()
	s.wakePending = true
}

// onForeign sets afterForeign.
func (s *fencedSession) onForeign(f func(running bool)) {
	s.fmu.Lock()
	defer s.fmu.Unlock()
	s.afterForeign = f
}

// guard is the barrier: the call is recorded, and a fence found down is a wake
// that could claim here.
func (s *fencedSession) guard(call string) {
	s.fmu.Lock()
	defer s.fmu.Unlock()
	if !s.up {
		s.bad = append(s.bad, call+" with the fence down: a wake could claim here")
	}
	s.calls = append(s.calls, call)
}

func (s *fencedSession) ForeignTurn() bool {
	s.guard("foreign")
	running := s.fakeSession.ForeignTurn()
	s.fmu.Lock()
	after := s.afterForeign
	s.afterForeign = nil
	s.fmu.Unlock()
	if after != nil {
		after(running)
	}
	return running
}

func (s *fencedSession) Begin(text string) func(context.Context) (agent.Result, error) {
	s.guard("begin")
	return s.fakeSession.Begin(text)
}

func (s *fencedSession) Cancel(ctx context.Context) (agent.CancelOutcome, error) {
	s.guard("cancel")
	return s.fakeSession.Cancel(ctx)
}

// take is every call recorded since the last take.
func (s *fencedSession) take() []string {
	s.fmu.Lock()
	defer s.fmu.Unlock()
	out := s.calls
	s.calls = nil
	return out
}

func (s *fencedSession) isUp() bool {
	s.fmu.Lock()
	defer s.fmu.Unlock()
	return s.up
}

func (s *fencedSession) violations() []string {
	s.fmu.Lock()
	defer s.fmu.Unlock()
	return append([]string(nil), s.bad...)
}

var (
	_ agent.Session        = (*fencedSession)(nil)
	_ agent.AdmissionFence = (*fencedSession)(nil)
)

// fenceRig is an engine over a fencedSession, with the hooks a fence schedule
// needs: every continuation that comes back, every driver pass (ticked says a
// tick caused it), and a tick only the test sends.
type fenceRig struct {
	*rig
	fs       *fencedSession
	ticks    chan time.Time
	returned chan string
	passes   chan bool
}

func newFenceRig(t *testing.T, chain ChainPolicy) *fenceRig {
	t.Helper()
	fr := &fenceRig{ticks: make(chan time.Time), returned: make(chan string, 16), passes: make(chan bool, 64)}
	h := &hooks{
		retryTick:    fr.ticks,
		turnReturned: func(id string) { fr.returned <- id },
		drivePassed: func(ticked bool) {
			select {
			case fr.passes <- ticked:
			default:
			}
		},
	}
	fake := newFake(t, agent.EventLogOptions{NoPrimary: true})
	fr.fs = &fencedSession{fakeSession: fake}
	e, err := newEngine(fr.fs, Options{Chain: chain}, h)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	sub, err := e.Subscribe(agent.SubscribeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sub.Close)
	if err := e.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	fr.rig = &rig{t: t, s: fake, e: e, sub: sub}
	return fr
}

// awaitTurns waits for every one of ids to come back, in any order.
func awaitTurns(t *testing.T, returned <-chan string, ids ...string) {
	t.Helper()
	left := slices.Clone(ids)
	for len(left) > 0 {
		select {
		case id := <-returned:
			i := slices.Index(left, id)
			if i < 0 {
				t.Fatalf("%s came back, want one of %q", id, left)
			}
			left = slices.Delete(left, i, i+1)
		case <-time.After(watchdog):
			t.Fatalf("%q never came back", left)
		}
	}
}

// want checks the calls since the last check, and the fence's state now.
func (fr *fenceRig) want(what string, up bool, calls ...string) {
	fr.t.Helper()
	if got := fr.fs.take(); !slices.Equal(got, calls) {
		fr.t.Fatalf("%s: the session saw %q, want %q", what, got, calls)
	}
	if fr.fs.isUp() != up {
		fr.t.Fatalf("%s: the fence is up=%v, want %v", what, fr.fs.isUp(), up)
	}
	if bad := fr.fs.violations(); len(bad) > 0 {
		fr.t.Fatalf("%s: %q", what, bad)
	}
}

// TestWakeFencedAtClaimAndValidation is plan 026 §7 A14's fence test. The
// session is a fencedSession, whose ForeignTurn, Begin and Cancel are barriers
// at the claim and at validation — earlier than beforeSessionCancel — that see
// whether a wake could claim there. Each schedule pins the exact sequence of
// what the session was asked, so the fence is shown up BEFORE the flag is read
// and before Begin, in every admission path that reaches either: Submit, the
// drain, a settlement's successor, retryLocked, the armed send firing, a
// cancel's validation and GiveUpDrain. Around them:
//
//   - down whenever the engine is idle and owes nothing;
//   - balanced after a Begin that panics and after a Cancel whose context ended;
//   - up for good after Stop, after Close and after GiveUpDrain's abandonment;
//   - F3: up while the agent runs a turn of its own with a row queued behind it,
//     and down in the error state with a kept queue;
//   - down while idle with rows the Queue verb added and no turn of the agent's
//     own (astra r14: a wake must still be possible), and down the moment the
//     last queued row is removed.
//
// A queue verb on an idle engine with rows raises and lowers it in its one
// section: owedDrainLocked reads the flag with the fence up, as every read is.
func TestWakeFencedAtClaimAndValidation(t *testing.T) {
	t.Run("submit, and the settlement with nothing behind it", func(t *testing.T) {
		fr := newFenceRig(t, ChainPolicy{})
		fr.want("started", false)
		turn := fr.s.script(held())
		fr.submit("one")
		fr.want("the submit's claim", true, "up", "foreign", "begin")
		await(t, turn.opened, "the turn to open")
		turn.release()
		awaitTurn(t, fr.returned, "turn-1")
		fr.want("the settlement", false, "foreign", "down")
	})

	t.Run("a settlement's successor", func(t *testing.T) {
		fr := newFenceRig(t, ChainPolicy{})
		first, second := fr.s.script(held()), fr.s.script(held())
		fr.submit("one")
		await(t, first.opened, "the first turn to open")
		// Queued behind a turn of craze's own: no flag is read (cur decides).
		fr.submit("two")
		fr.want("the claim and the row behind it", true, "up", "foreign", "begin")
		first.release()
		awaitTurn(t, fr.returned, "turn-1")
		await(t, second.opened, "the successor to open")
		fr.want("the settlement and its successor, one section", true, "foreign", "begin")
		second.release()
		awaitTurn(t, fr.returned, "turn-2")
		fr.want("the last settlement", false, "foreign", "down")
	})

	t.Run("the drain, behind a turn of the agent's own", func(t *testing.T) {
		fr := newFenceRig(t, ChainPolicy{})
		fr.s.setForeign(true)
		res := fr.submit("behind the agent")
		if res.Queued == nil {
			t.Fatalf("a submit during the agent's turn answered %+v", res)
		}
		// F3: a drain is owed at the end of the agent's turn, so the fence stays up.
		// The admission check saw the agent's turn, which latches it: the sync
		// reads the flag no more (X39).
		fr.want("queued behind the agent's turn", true, "up", "foreign")
		turn := fr.s.script(held())
		fr.s.setForeign(false)
		awaitPass(t, fr.passes, false)
		await(t, turn.opened, "the drained turn to open")
		fr.want("the driver's drain", true, "foreign", "begin")
		turn.release()
		awaitTurn(t, fr.returned, "turn-1")
		fr.want("its settlement", false, "foreign", "down")
	})

	t.Run("retryLocked", func(t *testing.T) {
		fr := newFenceRig(t, ChainPolicy{RetryForeignTurn: true})
		fr.s.setForeign(true)
		fr.s.script(&script{refuse: agent.ErrForeignTurn})
		fr.submit("go")
		awaitTurn(t, fr.returned, "turn-1")
		// The retrying policy claims through the agent's turn without reading
		// the flag; the refused turn stays current, so the fence stays up.
		fr.want("the refused claim", true, "up", "begin")
		tick(t, fr.ticks, fr.returned)
		awaitPass(t, fr.passes, true)
		fr.want("a due retry while the agent's turn runs", true, "foreign")
		turn := fr.s.script(held())
		fr.s.setForeign(false)
		await(t, turn.opened, "the retried claim to open")
		fr.want("the retried claim", true, "foreign", "begin")
		turn.release()
		awaitTurn(t, fr.returned, "turn-1")
		fr.want("its settlement", false, "foreign", "down")
	})

	t.Run("the armed send firing", func(t *testing.T) {
		fr := newFenceRig(t, ChainPolicy{})
		first := fr.s.script(held())
		fr.submit("one")
		await(t, first.opened, "the turn to open")
		fr.want("the turn", true, "up", "foreign", "begin")
		second := fr.s.script(held())
		fr.sendNow("instead", "")
		await(t, second.opened, "the send to fire and open")
		// The send-now's own gate reads the flag; its cancel is validated against
		// the named turn (no flag); the session's cancel runs under the hold; the
		// settlement fires the send in one section.
		fr.want("the arm, its cancel and the send firing", true, "foreign", "cancel", "foreign", "begin")
		second.release()
		// Either order: the settlement that fired the send may be turn-1's own.
		awaitTurns(t, fr.returned, "turn-1", "turn-2")
		fr.want("the send's settlement", false, "foreign", "down")
	})

	t.Run("a cancel's validation, with no turn of craze's own", func(t *testing.T) {
		fr := newFenceRig(t, ChainPolicy{})
		fr.s.setForeign(true)
		if _, err := fr.e.Cancel(context.Background(), Command{}, ""); err != nil {
			t.Fatalf("cancel: %v", err)
		}
		// Raised before the validation reads the flag, up through the session's
		// cancel, and down with the release's pass: nothing is queued.
		fr.want("the cancel", false, "up", "foreign", "cancel", "foreign", "down")
	})

	t.Run("a Begin that panics", func(t *testing.T) {
		fr := newFenceRig(t, ChainPolicy{})
		fr.s.mu.Lock()
		fr.s.beginPanic = true
		fr.s.mu.Unlock()
		func() {
			defer func() {
				if recover() == nil {
					t.Fatal("the panic did not reach Submit's caller")
				}
			}()
			_, _ = fr.e.Submit(Command{}, "boom", SubmitQueue, "")
		}()
		fr.want("the panicking claim", false, "up", "foreign", "begin", "down")
	})

	t.Run("a cancel whose context ended", func(t *testing.T) {
		fr := newFenceRig(t, ChainPolicy{})
		// An ask makes a cancel with no turn acceptable without the flag.
		fr.s.openAsk(t)
		fr.s.mu.Lock()
		fr.s.cancelErr = context.DeadlineExceeded
		fr.s.mu.Unlock()
		if _, err := fr.e.Cancel(context.Background(), Command{}, ""); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("cancel: %v", err)
		}
		fr.want("the timed-out cancel", false, "up", "cancel", "foreign", "down")
	})

	t.Run("Stop holds it up for good", func(t *testing.T) {
		fr := newFenceRig(t, ChainPolicy{})
		turn := fr.s.script(held())
		fr.submit("one")
		await(t, turn.opened, "the turn to open")
		fr.want("the turn", true, "up", "foreign", "begin")
		if err := fr.e.Stop(context.Background(), Command{}); err != nil {
			t.Fatalf("stop: %v", err)
		}
		awaitTurn(t, fr.returned, "turn-1")
		fr.want("the stopped turn's settlement", true, "cancel")
	})

	t.Run("Close holds it up for good", func(t *testing.T) {
		fr := newFenceRig(t, ChainPolicy{})
		if err := fr.e.Close(); err != nil {
			t.Fatal(err)
		}
		fr.want("closed", true, "up")
	})

	t.Run("GiveUpDrain's abandonment holds it up for good", func(t *testing.T) {
		fr := newFenceRig(t, ChainPolicy{RetryForeignTurn: true, StopOnNonEndTurn: true})
		fr.s.setForeign(true)
		fr.queue("held")
		fr.want("a row behind the agent's turn", true, "up", "foreign")
		turn, pending, err := fr.e.GiveUpDrain(Command{})
		if err != nil || turn != "" || pending != 1 {
			t.Fatalf("GiveUpDrain answered %q, %d, %v", turn, pending, err)
		}
		fr.want("the abandoned drain", true, "foreign")
		fr.s.setForeign(false)
		awaitPass(t, fr.passes, false)
		fr.want("the agent's turn ending after it", true)
	})

	t.Run("an error state with a kept queue", func(t *testing.T) {
		fr := newFenceRig(t, ChainPolicy{})
		fr.queue("kept")
		// Idle with a row and the agent's session free: nothing is owed, and the
		// fence stays down (it is raised only to read the flag).
		fr.want("a row queued while idle", false, "up", "foreign", "down")
		fr.s.script(&script{refuse: agent.ErrPromptInFlight})
		fr.submit("refused")
		awaitTurn(t, fr.returned, "turn-1")
		if st := fr.e.State(); st.Activity != ActivityError || len(st.Queue) != 1 {
			t.Fatalf("state after the refusal: %+v", st)
		}
		// Nothing drains from an error, so nothing is owed: astra r4's rule.
		fr.want("the refusal", false, "up", "foreign", "begin", "down")
	})

	t.Run("the queue verbs", func(t *testing.T) {
		fr := newFenceRig(t, ChainPolicy{})
		a := fr.queue("a")
		fr.want("a row queued while idle", false, "up", "foreign", "down")
		if _, err := fr.e.Unqueue(Command{}, a.ID); err != nil {
			t.Fatal(err)
		}
		fr.want("the last row removed while idle", false)
		// The agent runs a turn of its own: a row behind it is a drain owed.
		fr.s.setForeign(true)
		b := fr.queue("b")
		fr.want("a row behind the agent's turn", true, "up", "foreign")
		// Latched (X39): the rows stay owed without the flag being read again.
		c := fr.queue("c")
		if err := fr.e.EditQueued(Command{}, c.ID, "c'", nil); err != nil {
			t.Fatal(err)
		}
		fr.want("more rows, and an edit", true)
		if _, err := fr.e.Unqueue(Command{}, b.ID); err != nil {
			t.Fatal(err)
		}
		fr.want("a row removed with one left", true)
		if _, err := fr.e.Unqueue(Command{}, c.ID); err != nil {
			t.Fatal(err)
		}
		fr.want("the last row removed", false, "down")
		fr.queue("d")
		fr.queue("e")
		fr.want("rows again", true, "up", "foreign")
		if n, err := fr.e.ClearQueue(Command{}); err != nil || n != 2 {
			t.Fatalf("clear: %d, %v", n, err)
		}
		fr.want("the queue cleared", false, "down")
		if n := len(fr.s.prompts()); n != 0 {
			t.Fatalf("a queue verb started %d turns", n)
		}
	})
}

// foreignEnded is the event a session publishes when the agent's own turn is
// over, with the flag already down: the observer's kick to the driver.
func foreignEnded() agent.Event {
	return agent.Event{Type: agent.EventForeignTurn, ForeignTurn: &agent.ForeignTurnInfo{Running: false}}
}

// TestAnOwedDrainOutlivesTheAgentsTurn is astra r16's finding (X39): a row
// queued behind a turn the session started itself (wake A) is a drain owed, and
// it stays owed — the fence stays up — until the drain runs, even when A ends
// while the engine holds e.mu and the section's sync would read the flag clear.
// A second turn the session has pending (wake B) claims the moment the fence
// comes down; it must come after the row, not before it.
//
// Both of astra's schedules are forced at the fenced session: A ends inside
// Submit's own section, between the admission check that saw it and the
// deferred sync (the hook runs in the read); and A ends after a Submit, with an
// EditQueued taking e.mu before the driver hears. A's ending event is delivered
// only afterwards, as a session flushes it outside its lock.
//
// The latch is also cleared, not masked (the session-control session's pin 2):
// latched, the queue emptied, A over, a row the Queue verb adds on the idle
// engine leaves the fence down, and B can start.
func TestAnOwedDrainOutlivesTheAgentsTurn(t *testing.T) {
	// drains runs the owed row, which the fence has held for it: A's ending
	// reaches the driver, the row's turn is claimed and runs, and only its
	// settlement lets B start.
	drains := func(t *testing.T, fr *fenceRig, text string) {
		t.Helper()
		turn := fr.s.script(held())
		fr.s.emit(foreignEnded())
		awaitPass(t, fr.passes, false)
		await(t, turn.opened, "the owed row's turn to open")
		fr.want("A's end: the driver drains the row, with the fence never down", true, "foreign", "begin")
		turn.release()
		awaitTurn(t, fr.returned, "turn-1")
		fr.want("the row's settlement, and then B", false, "foreign", "down", "wake")
		fr.wantPrompts(text)
	}

	t.Run("A ends between Submit's admission check and its sync", func(t *testing.T) {
		fr := newFenceRig(t, ChainPolicy{})
		fr.s.setForeign(true)
		fr.fs.pendWake()
		fr.fs.onForeign(func(running bool) {
			if running {
				fr.s.setForeignSilently(false)
			}
		})
		if res := fr.submit("the row"); res.Queued == nil {
			t.Fatalf("a submit behind wake A answered %+v, want a queued row", res)
		}
		fr.want("queued behind A, which ended inside the section", true, "up", "foreign")
		drains(t, fr, "the row")
	})

	t.Run("A ends before an edit takes e.mu", func(t *testing.T) {
		fr := newFenceRig(t, ChainPolicy{})
		fr.s.setForeign(true)
		row := fr.submit("the row").Queued
		// Which reads the setup made is the first subtest's business; here only
		// that the row is owed.
		fr.fs.take()
		fr.want("queued behind A", true)
		fr.fs.pendWake()
		fr.s.setForeignSilently(false)
		if err := fr.e.EditQueued(Command{}, row.ID, "the row, edited", nil); err != nil {
			t.Fatal(err)
		}
		fr.want("an edit after A ended, before the driver heard", true)
		drains(t, fr, "the row, edited")
	})

	t.Run("a latch cleared by an emptied queue stays cleared", func(t *testing.T) {
		fr := newFenceRig(t, ChainPolicy{})
		fr.s.setForeign(true)
		row := fr.queue("owed")
		fr.want("a row behind A", true, "up", "foreign")
		if _, err := fr.e.Unqueue(Command{}, row.ID); err != nil {
			t.Fatal(err)
		}
		fr.want("the row withdrawn", false, "down")
		fr.s.setForeign(false)
		awaitPass(t, fr.passes, false)
		fr.want("A's end, with nothing to drain", false, "up", "foreign", "down")
		fr.fs.pendWake()
		fr.queue("idle")
		// Nothing is owed: the row was queued with the agent's session free, and
		// nothing will ever kick a drain for it. B starts.
		fr.want("a Queue-verb row on the idle engine", false, "up", "foreign", "down", "wake")
		fr.wantPrompts()
	})
}

// fenceMethod is one method of *Engine as TestEveryForeignReadAndClaimIsFenced
// reads it.
type fenceMethod struct {
	// site: it reads the session's flag or claims (e.sess.ForeignTurn, Begin).
	// mutates: it changes an input of the fence — calls a mutator of e.queue, or
	// assigns e.activity, e.cur, e.cancelsInFlight, e.stopped or e.closed.
	// locks: it takes e.mu. raises, syncs: it calls raiseFenceLocked, and defers
	// syncFenceLocked. closes: it sets e.closed (a mutation too).
	site, mutates, locks, raises, syncs, closes bool
	// calls is every method of the receiver it calls, closures and go and defer
	// statements included.
	calls []string
}

// fenceInputs are what the fence reads, as selector spells them.
var (
	fenceQueueMutators = []string{"queue.Add", "queue.Take", "queue.Pop", "queue.PushFront", "queue.Clear", "queue.Restore", "queue.Edit", "queue.Remove"}
	fenceFields        = []string{"activity", "cur", "cancelsInFlight", "stopped"}
)

// parseEngineMethods parses the package's non-test files and returns every
// method of *Engine, by name.
func parseEngineMethods(t *testing.T) map[string]*fenceMethod {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	methods := map[string]*fenceMethod{}
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Recv == nil || len(fd.Recv.List) != 1 || len(fd.Recv.List[0].Names) != 1 {
				continue
			}
			star, ok := fd.Recv.List[0].Type.(*ast.StarExpr)
			if !ok {
				continue
			}
			if id, ok := star.X.(*ast.Ident); !ok || id.Name != "Engine" {
				continue
			}
			recv := fd.Recv.List[0].Names[0].Name
			m := &fenceMethod{}
			methods[fd.Name.Name] = m
			assigned := func(x ast.Expr) {
				switch s := selector(x, recv); {
				case s == "closed":
					// closed is an input of the fence too (astra r18): a method
					// that sets it is held to the sync, unless it is Close.
					m.closes, m.mutates = true, true
				case slices.Contains(fenceFields, s):
					m.mutates = true
				}
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.DeferStmt:
					if selector(n.Call.Fun, recv) == "syncFenceLocked" {
						m.syncs = true
					}
				case *ast.AssignStmt:
					for _, lhs := range n.Lhs {
						assigned(lhs)
					}
				case *ast.IncDecStmt:
					assigned(n.X)
				case *ast.CallExpr:
					switch s := selector(n.Fun, recv); {
					case s == "sess.ForeignTurn", s == "sess.Begin":
						m.site = true
					case s == "mu.Lock":
						m.locks = true
					case s == "raiseFenceLocked":
						m.raises = true
					case slices.Contains(fenceQueueMutators, s):
						m.mutates = true
					case s != "" && !strings.Contains(s, "."):
						m.calls = append(m.calls, s)
					}
				}
				return true
			})
		}
	}
	return methods
}

// TestEveryForeignReadAndClaimIsFenced holds the admission fence's coverage to
// the code, by parsing the package rather than from a list, so a new read of the
// flag, a new claim or a new change to what the fence reads cannot be added
// without the fence. It finds:
//
//   - every method that calls e.sess.ForeignTurn() or e.sess.Begin — exactly
//     the Engine doc's sites;
//   - every method that takes e.mu and reaches one of those, through methods
//     that take no lock of their own (a method that does is its own section,
//     checked on its own) and not through syncFenceLocked (owedDrainLocked
//     raises the fence itself before it reads) — each must raise the fence and
//     defer its sync, and they are exactly the doc's sections;
//   - every method that takes e.mu and reaches a change to an input of the fence
//     — a mutator of e.queue, or an assignment to e.activity, e.cur,
//     e.cancelsInFlight, e.stopped or e.closed — must defer the sync; Close
//     alone, by name, may raise it instead, because it sets e.closed and the
//     fence never comes down again;
//   - and every method that changes such an input without taking e.mu is reached
//     only from such sections: it has a caller in the package, and each caller
//     is held to the same rules.
//
// The order inside a section — raised before the read — is what
// TestWakeFencedAtClaimAndValidation's barriers show.
func TestEveryForeignReadAndClaimIsFenced(t *testing.T) {
	methods := parseEngineMethods(t)

	var sites []string
	for name, m := range methods {
		if m.site {
			sites = append(sites, name)
		}
	}
	slices.Sort(sites)
	if want := []string{"claimLocked", "foreignLocked", "retryLocked"}; !slices.Equal(sites, want) {
		t.Fatalf("the methods that read the flag or claim are %q, want the Engine doc's %q", sites, want)
	}

	// reaches reports whether name, or a method it calls that takes no lock of
	// its own, is hit — never through the sync, which is the fence itself.
	var reaches func(name string, hit func(*fenceMethod) bool, seen map[string]bool) bool
	reaches = func(name string, hit func(*fenceMethod) bool, seen map[string]bool) bool {
		m := methods[name]
		if m == nil || seen[name] {
			return false
		}
		seen[name] = true
		if hit(m) {
			return true
		}
		for _, c := range m.calls {
			if c == "syncFenceLocked" || methods[c] == nil || methods[c].locks {
				continue
			}
			if reaches(c, hit, seen) {
				return true
			}
		}
		return false
	}
	site := func(m *fenceMethod) bool { return m.site }
	mutation := func(m *fenceMethod) bool { return m.mutates }

	var sections []string
	callers := map[string][]string{}
	for name, m := range methods {
		for _, c := range m.calls {
			callers[c] = append(callers[c], name)
		}
		if !m.locks {
			continue
		}
		if reaches(name, site, map[string]bool{}) {
			sections = append(sections, name)
			if !m.raises || !m.syncs {
				t.Errorf("%s takes e.mu and reaches a read of the flag or a claim: raises the fence %v, defers its sync %v", name, m.raises, m.syncs)
			}
		}
		// Close alone may raise instead of syncing: it sets closed, for good.
		if reaches(name, mutation, map[string]bool{}) && !m.syncs && (name != "Close" || !m.closes || !m.raises) {
			t.Errorf("%s takes e.mu and changes what the fence reads, and does not defer its sync", name)
		}
	}
	slices.Sort(sections)
	if want := []string{"GiveUp", "GiveUpDrain", "drive", "holdCancel", "releaseHold", "runTurn", "submit"}; !slices.Equal(sections, want) {
		t.Errorf("the fenced sections are %q, want the Engine doc's %q", sections, want)
	}
	var helpers []string
	for name, m := range methods {
		if m.locks || !reaches(name, mutation, map[string]bool{}) {
			continue
		}
		helpers = append(helpers, name)
		if len(callers[name]) == 0 {
			t.Errorf("%s changes what the fence reads without e.mu, and nothing in the package calls it under a section", name)
		}
	}
	if len(helpers) == 0 {
		t.Fatal("found no method that changes what the fence reads under a caller's e.mu: the parse is broken")
	}
	if m := methods["Close"]; m == nil || !m.raises || !m.closes {
		t.Error("Close does not raise the fence for good")
	}
}

// selector spells a call's function relative to the receiver: "sess.Begin" for
// e.sess.Begin, "mu.Lock" for e.mu.Lock, "passLocked" for e.passLocked, and ""
// for anything not rooted at the receiver.
func selector(fun ast.Expr, recv string) string {
	var parts []string
	for {
		switch x := fun.(type) {
		case *ast.SelectorExpr:
			parts = append([]string{x.Sel.Name}, parts...)
			fun = x.X
		case *ast.Ident:
			if x.Name != recv {
				return ""
			}
			return strings.Join(parts, ".")
		default:
			return ""
		}
	}
}
