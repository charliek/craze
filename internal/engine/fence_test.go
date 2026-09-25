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
	defer s.fmu.Unlock()
	if !s.up {
		s.bad = append(s.bad, "FenceDown while down")
	}
	s.up = false
	s.calls = append(s.calls, "down")
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
	return s.fakeSession.ForeignTurn()
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
		fr.want("queued behind the agent's turn", true, "up", "foreign", "foreign")
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
		c := fr.queue("c")
		if err := fr.e.EditQueued(Command{}, c.ID, "c'", nil); err != nil {
			t.Fatal(err)
		}
		fr.want("more rows, and an edit", true, "foreign", "foreign")
		if _, err := fr.e.Unqueue(Command{}, b.ID); err != nil {
			t.Fatal(err)
		}
		fr.want("a row removed with one left", true, "foreign")
		if _, err := fr.e.Unqueue(Command{}, c.ID); err != nil {
			t.Fatal(err)
		}
		fr.want("the last row removed", false, "down")
		fr.queue("d")
		fr.queue("e")
		fr.want("rows again", true, "up", "foreign", "foreign")
		if n, err := fr.e.ClearQueue(Command{}); err != nil || n != 2 {
			t.Fatalf("clear: %d, %v", n, err)
		}
		fr.want("the queue cleared", false, "down")
		if n := len(fr.s.prompts()); n != 0 {
			t.Fatalf("a queue verb started %d turns", n)
		}
	})
}

// TestEveryForeignReadAndClaimIsFenced holds the Engine doc's list of the
// admission fence's sections to the code, so a new read of the flag or a new
// claim cannot be added without the fence. It parses the package and finds:
//
//   - every method that calls e.sess.ForeignTurn() or e.sess.Begin — exactly
//     the doc's sites;
//   - every method that takes e.mu and reaches one of those, through methods
//     that take no lock of their own (a method that does is its own section,
//     checked on its own) and not through syncFenceLocked (owedDrainLocked
//     raises the fence itself before it reads) — each must raise the fence and
//     defer its sync;
//   - and the sections that change what the fence reads without reaching a
//     site — Started and the queue verbs — defer the sync, and Close raises it.
//
// The order inside a section — raised before the read — is what
// TestWakeFencedAtClaimAndValidation's barriers show.
func TestEveryForeignReadAndClaimIsFenced(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	type method struct {
		site, locks, raises, syncs bool
		calls                      []string
	}
	methods := map[string]*method{}
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
			m := &method{}
			methods[fd.Name.Name] = m
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				if d, ok := n.(*ast.DeferStmt); ok && selector(d.Call.Fun, recv) == "syncFenceLocked" {
					m.syncs = true
				}
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				switch s := selector(call.Fun, recv); s {
				case "sess.ForeignTurn", "sess.Begin":
					m.site = true
				case "mu.Lock":
					m.locks = true
				case "raiseFenceLocked":
					m.raises = true
				default:
					if s != "" && !strings.Contains(s, ".") {
						m.calls = append(m.calls, s)
					}
				}
				return true
			})
		}
	}

	var sites []string
	for name, m := range methods {
		if m.site {
			sites = append(sites, name)
		}
	}
	slices.Sort(sites)
	if want := []string{"canStartLocked", "claimLocked", "holdCancelLocked", "owedDrainLocked", "retryLocked", "submit"}; !slices.Equal(sites, want) {
		t.Fatalf("the methods that read the flag or claim are %q, want the Engine doc's %q", sites, want)
	}

	var reach func(name string, seen map[string]bool) bool
	reach = func(name string, seen map[string]bool) bool {
		m := methods[name]
		if m == nil || seen[name] {
			return false
		}
		seen[name] = true
		if m.site {
			return true
		}
		for _, c := range m.calls {
			if c == "syncFenceLocked" || methods[c] == nil || methods[c].locks {
				continue
			}
			if reach(c, seen) {
				return true
			}
		}
		return false
	}
	var sections []string
	for name, m := range methods {
		if m.locks && reach(name, map[string]bool{}) {
			sections = append(sections, name)
			if !m.raises || !m.syncs {
				t.Errorf("%s takes e.mu and reaches a read of the flag or a claim: raises the fence %v, defers its sync %v", name, m.raises, m.syncs)
			}
		}
	}
	slices.Sort(sections)
	if want := []string{"GiveUp", "GiveUpDrain", "drive", "holdCancel", "releaseHold", "runTurn", "submit"}; !slices.Equal(sections, want) {
		t.Errorf("the fenced sections are %q, want the Engine doc's %q", sections, want)
	}
	for _, name := range []string{"Started", "Queue", "EditQueued", "Unqueue", "ClearQueue"} {
		if m := methods[name]; m == nil || !m.syncs {
			t.Errorf("%s changes what the fence reads and does not sync it", name)
		}
	}
	if m := methods["Close"]; m == nil || !m.raises {
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
