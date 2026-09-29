package control_test

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/control"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/tui"
)

// The host's close fence (Server.FenceClose; plan 030 §3.6, R2-1): the attach
// fence, then the engine's, and the verdict read under both — attached 0 and
// nothing busy is a stop, with both fences left up; anything else releases
// them. §3.18's forced races: an unattached prompt, a settings change, the
// session's own work starting and a reserved-but-not-live attach, each
// arriving at every step — before (1), between (1) and (2), between (2) and
// (3), after the decision — held there by the fence's own step hooks
// (TestHooks.CloseFenceStep) and the Stub's barriers, never by timing.

// wakeStub is a Stub with an agent.AdmissionFence (native's wake): a turn of
// its own the test asks for starts at once while the fence is down, and the
// moment it comes down otherwise — never while it is up. Its flag is its own,
// set silently (the engine calls FenceDown under e.mu, where nothing may be
// published), and read beside the Stub's.
type wakeStub struct {
	*tui.Stub
	mu      sync.Mutex
	up      bool
	pending bool
	running bool
}

func (s *wakeStub) FenceUp() {
	s.mu.Lock()
	s.up = true
	s.mu.Unlock()
}

func (s *wakeStub) FenceDown() {
	s.mu.Lock()
	s.up = false
	if s.pending {
		s.pending, s.running = false, true
	}
	s.mu.Unlock()
}

// wake asks for a turn of the session's own, and reports whether it started.
func (s *wakeStub) wake() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.up {
		s.pending = true
		return false
	}
	s.running = true
	return true
}

func (s *wakeStub) own() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

func (s *wakeStub) ForeignTurn() bool { return s.own() || s.Stub.ForeignTurn() }

func (s *wakeStub) Snapshot() agent.Snapshot {
	snap := s.Stub.Snapshot()
	snap.ForeignTurn = snap.ForeignTurn || s.own()
	return snap
}

var _ agent.AdmissionFence = (*wakeStub)(nil)

// fenceStep is where a racer arrives.
type fenceStep int

const (
	beforeFence   fenceStep = iota // before (1): before the attach fence
	betweenFences                  // between (1) and (2)
	beforeVerdict                  // between (2) and (3): both up, not yet decided
	afterDecision                  // after (3): the verdict taken
)

func (s fenceStep) String() string {
	return [...]string{"before the attach fence", "between the fences", "before the verdict", "after the decision"}[s]
}

// fenceRace is one host and what a racer needs of it.
type fenceRace struct {
	h  *host
	ws *wakeStub
	c  *client
	// holdReserve makes the next attach's reservation park (Reserved) until
	// the test ends; reserved is told when one does.
	holdReserve atomic.Bool
	reserved    chan struct{}
	letReserved func()
}

// fenceRacer is one thing that can arrive while a host decides. arrive sends
// it and checks it was admitted — in flight now, held there — when want, and
// refused closing (or, for the session's own work, held back) when not.
type fenceRacer struct {
	name   string
	arrive func(t *testing.T, r *fenceRace, want bool)
}

// fenceRacers are §3.18's four.
func fenceRacers() []fenceRacer {
	return []fenceRacer{
		{"an unattached prompt", func(t *testing.T, r *fenceRace, want bool) {
			hung := r.h.stub.HangNext()
			resp := r.c.call(protocol.MethodSessionPrompt, protocol.PromptParams{SessionID: sid(r.h), CommandID: r.c.cmd(), Text: "now", Mode: protocol.PromptQueue})
			if !want {
				refusedWith(t, resp, protocol.RPCRefused, protocol.CodeUnavailable, protocol.ReasonClosing)
				return
			}
			if ok[protocol.PromptResult](t, resp).Turn == "" {
				t.Fatal("the prompt was admitted and started no turn")
			}
			await(t, hung, "the prompt's turn to open")
		}},
		{"a settings change", func(t *testing.T, r *fenceRace, want bool) {
			held, release := r.h.stub.HoldNextSet()
			t.Cleanup(release)
			id := r.c.send(protocol.MethodSessionSet, protocol.SetParams{SessionID: sid(r.h), CommandID: r.c.cmd(),
				Setting: protocol.Setting{Kind: protocol.SettingModel, Value: "fast"}})
			if !want {
				refusedWith(t, r.c.reply(id), protocol.RPCRefused, protocol.CodeUnavailable, protocol.ReasonClosing)
				return
			}
			// Admitted, and parked in the provider's call: its reply comes
			// once it is let go, which the test never waits for.
			await(t, held, "the Set to reach the provider")
		}},
		{"the session's own work", func(t *testing.T, r *fenceRace, want bool) {
			if started := r.ws.wake(); started != want {
				t.Fatalf("a turn of the session's own started %v, want %v", started, want)
			}
		}},
		{"a reserved-but-not-live attach", func(t *testing.T, r *fenceRace, want bool) {
			r.holdReserve.Store(true)
			id := r.c.send(protocol.MethodSessionAttach, protocol.AttachParams{SessionID: sid(r.h)})
			if !want {
				refusedWith(t, r.c.reply(id), protocol.RPCRefused, protocol.CodeUnavailable, protocol.ReasonClosing)
				return
			}
			// Reserved, and held there: pending, not yet answered.
			await(t, r.reserved, "the attach to reserve")
		}},
	}
}

// runFenceRace is one schedule: r arrives at step, against a host with no
// client attached — or, with keeper, one whose attached client makes its
// verdict a stay. It checks the racer's own answer, then the verdict: a stop
// is what an idle host with nothing attached and nothing in flight comes to,
// and anything the racer got in before the fences keeps it.
func runFenceRace(t *testing.T, racer fenceRacer, step fenceStep, keeper bool) {
	at1, go1 := make(chan struct{}), make(chan struct{})
	at2, go2 := make(chan struct{}), make(chan struct{})
	r := &fenceRace{reserved: make(chan struct{}, 1)}
	let := make(chan struct{})
	var letOnce sync.Once
	r.letReserved = func() { letOnce.Do(func() { close(let) }) }
	var open1, open2 sync.Once
	openAll := func() {
		open1.Do(func() { close(go1) })
		open2.Do(func() { close(go2) })
	}
	r.h = newHost(t,
		withSession(func(s *tui.Stub) agent.Session { r.ws = &wakeStub{Stub: s}; return r.ws }),
		withHooks(control.TestHooks{
			CloseFenceStep: func(step string) {
				switch step {
				case "attaches fenced":
					close(at1)
					<-go1
				case "engine fenced":
					close(at2)
					<-go2
				}
			},
			Reserved: func(string) {
				if r.holdReserve.Load() {
					r.reserved <- struct{}{}
					<-let
				}
			},
		}))
	// Before the server's close: nothing the test holds outlives it.
	t.Cleanup(openAll)
	t.Cleanup(r.letReserved)
	if keeper {
		k := r.h.dial()
		k.sayHello(nil)
		k.attach(protocol.AttachParams{SessionID: sid(r.h)})
	}
	r.c = r.h.dial()
	r.c.sayHello(nil)

	// What the racer gets, and the verdict, as the schedule decides them.
	admitted := step == beforeFence || (step == betweenFences && racer.name != "a reserved-but-not-live attach") ||
		(step == afterDecision && keeper)
	// A stop unless a client is kept attached, or the racer got in before
	// the verdict (it arrived after it, the verdict was taken without it).
	wantStop := !keeper && (!admitted || step == afterDecision)

	if step == beforeFence {
		racer.arrive(t, r, admitted)
	}
	type verdict struct {
		n       int
		busy    bool
		release func()
	}
	got := make(chan verdict, 1)
	go func() {
		n, busy, release := r.h.srv.FenceClose()
		got <- verdict{n, busy, release}
	}()
	await(t, at1, "the attach fence")
	if step == betweenFences {
		racer.arrive(t, r, admitted)
	}
	open1.Do(func() { close(go1) })
	await(t, at2, "the engine's fence")
	if step == beforeVerdict {
		racer.arrive(t, r, admitted)
	}
	open2.Do(func() { close(go2) })
	var v verdict
	select {
	case v = <-got:
	case <-time.After(watchdog):
		t.Fatal("FenceClose never returned")
	}
	if stop := v.n == 0 && !v.busy; stop != wantStop {
		t.Fatalf("the verdict: attached %d, busy %v — stop %v, want %v", v.n, v.busy, stop, wantStop)
	}
	if !wantStop {
		v.release()
		v.release() // idempotent
	}
	if step == afterDecision {
		racer.arrive(t, r, admitted)
	}
	if wantStop {
		// The stop sequence's close, the fences still up: nothing the racer
		// was refused, or held back, gets in after it either.
		_ = r.h.eng.Close()
		if r.ws.own() {
			t.Fatal("a turn of the session's own started under the fences of a stop")
		}
	}
}

// TestTheCloseFenceRaces is every racer at every step (§3.18).
func TestTheCloseFenceRaces(t *testing.T) {
	for _, racer := range fenceRacers() {
		for _, step := range []fenceStep{beforeFence, betweenFences, beforeVerdict, afterDecision} {
			t.Run(racer.name+"/"+step.String(), func(t *testing.T) {
				runFenceRace(t, racer, step, false)
			})
		}
		t.Run(racer.name+"/after a decision to stay", func(t *testing.T) {
			runFenceRace(t, racer, afterDecision, true)
		})
	}
}

// TestAnIdleHostWithNobodyAttachedIsAStop: the verdict alone — no racer —
// is a stop, both fences up: an attach and a prompt are refused closing until
// the engine closes.
func TestAnIdleHostWithNobodyAttachedIsAStop(t *testing.T) {
	h := newHost(t)
	n, busy, _ := h.srv.FenceClose()
	if n != 0 || busy {
		t.Fatalf("an idle host with nobody attached: attached %d, busy %v", n, busy)
	}
	c := h.dial()
	c.sayHello(nil)
	resp := c.call(protocol.MethodSessionAttach, protocol.AttachParams{SessionID: sid(h)})
	refusedWith(t, resp, protocol.RPCRefused, protocol.CodeUnavailable, protocol.ReasonClosing)
	resp = c.call(protocol.MethodSessionPrompt, protocol.PromptParams{SessionID: sid(h), CommandID: c.cmd(), Text: "x", Mode: protocol.PromptQueue})
	refusedWith(t, resp, protocol.RPCRefused, protocol.CodeUnavailable, protocol.ReasonClosing)
}
