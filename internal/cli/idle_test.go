package cli

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/control"
	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/remote"
	"github.com/charliek/craze/internal/tui"
)

// The idle watcher (idle.go; plan 030 §3.6), one look at a time: a real
// control server and an engine over a Stub, the watcher's clock a test's own
// and its looks made by the test itself (idleWatcher.look), so every schedule
// is forced — the clock moves only when the test moves it, and an attach, a
// prompt or a setting lands exactly between two looks, or between the clock's
// expiry and the close fence (beforeFence). Every wait is bounded on its own.

// idleRig is one host's watcher over a Stub, and a socket to reach it.
type idleRig struct {
	t    *testing.T
	stub *tui.Stub
	eng  *engine.Engine
	srv  *control.Server
	sock string
	w    *idleWatcher
	// offset is the watcher's clock: the rig's start plus offset.
	base   time.Time
	offset atomic.Int64
	// lost is what the socket-lost check answers; beforeFence runs just
	// before the watcher raises its close fence.
	lostMu      sync.Mutex
	lostWhy     string
	beforeFence func()
}

// idleServerHook is the rig's server as the watcher sees it: FenceClose runs
// the rig's beforeFence first — where a test puts an attach arriving at the
// clock's expiry.
type idleServerHook struct {
	*control.Server
	r *idleRig
}

func (s idleServerHook) FenceClose() (int, bool, func()) {
	if f := s.r.beforeFence; f != nil {
		s.r.beforeFence = nil
		f()
	}
	return s.Server.FenceClose()
}

// newIdleRig is a watcher over a new engine, with config as config.toml (""
// for none), started unless unstarted. The grace and the never-prompted cap
// are production's unless the test moves them.
func newIdleRig(t *testing.T, config string, started bool) *idleRig {
	t.Helper()
	stub := tui.NewStubNoPrimary()
	r := newIdleRigOver(t, config, started, stub)
	r.stub = stub
	return r
}

// newIdleRigOver is newIdleRig with sess as the engine's session — a Stub's,
// or a real one (idle_jobs_test.go) — and no stub of the rig's.
func newIdleRigOver(t *testing.T, config string, started bool, sess agent.Session) *idleRig {
	t.Helper()
	if config != "" {
		writeCrazeConfig(t, config)
	} else {
		crazeHome(t)
	}
	r := &idleRig{t: t, base: time.Now()}
	eng, err := engine.New(sess, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	r.eng = eng
	t.Cleanup(func() { _ = eng.Close() })
	if started {
		if err := eng.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	r.srv = control.New(control.Options{Workspace: "/w"})
	r.w = newIdleWatcher(idleServerHook{Server: r.srv, r: r}, eng, r.lost, r.now, io.Discard)
	r.srv.SetEngine(eng)
	dir, err := os.MkdirTemp("/tmp", "czi")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	r.sock = dir + "/s"
	l, err := net.Listen("unix", r.sock)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = r.srv.Serve(l) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), serveStep)
		defer cancel()
		_ = r.srv.Close(ctx)
	})
	return r
}

func (r *idleRig) now() time.Time { return r.base.Add(time.Duration(r.offset.Load())) }

func (r *idleRig) advance(d time.Duration) { r.offset.Add(int64(d)) }

func (r *idleRig) lost() string {
	r.lostMu.Lock()
	defer r.lostMu.Unlock()
	return r.lostWhy
}

func (r *idleRig) loseSocket(why string) {
	r.lostMu.Lock()
	r.lostWhy = why
	r.lostMu.Unlock()
}

// look is one of the watcher's ticks.
func (r *idleRig) look() idleLook { return r.w.look() }

// wantStay: the look did not stop the host.
func (r *idleRig) wantStay(what string) idleLook {
	r.t.Helper()
	l := r.look()
	if l.Stop != "" {
		r.t.Fatalf("%s: the host stops (%s): %+v", what, l.Stop, l)
	}
	return l
}

// wantStop: the look stopped the host, saying want.
func (r *idleRig) wantStop(what, want string) idleLook {
	r.t.Helper()
	l := r.look()
	if l.Stop == "" || !strings.Contains(l.Stop, want) {
		r.t.Fatalf("%s: the look is %+v, want a stop saying %q", what, l, want)
	}
	return l
}

// prompted runs one turn to its end, so the session has had a prompt.
func (r *idleRig) prompted() {
	r.t.Helper()
	if _, err := r.eng.Submit(engine.Command{}, "hi", engine.SubmitQueue, ""); err != nil {
		r.t.Fatal(err)
	}
	r.waitIdle()
}

// waitIdle waits, within serveStep, for the engine to be busy with nothing.
func (r *idleRig) waitIdle() {
	r.t.Helper()
	deadline := time.Now().Add(serveStep)
	for r.eng.Busy() {
		if time.Now().After(deadline) {
			r.t.Fatalf("the engine is still busy after %v: %+v", serveStep, r.eng.State())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// attach is a client attached to the rig's host, and a way to kill it — its
// connection closed with no detach, as a killed process's is.
func (r *idleRig) attach() (kill func()) {
	r.t.Helper()
	return r.attachWhen(protocol.WhenReady)
}

// attachWhen is attach with when: now attaches a session still starting.
func (r *idleRig) attachWhen(when protocol.When) (kill func()) {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), serveStep)
	defer cancel()
	c, err := remote.Dial(ctx, r.sock, remote.Options{})
	if err != nil {
		r.t.Fatal(err)
	}
	if _, err := c.Attach(ctx, remote.AttachOptions{SessionID: r.eng.State().CrazeSessionID, When: when}); err != nil {
		r.t.Fatal(err)
	}
	var once sync.Once
	kill = func() { once.Do(func() { _ = c.Close() }) }
	r.t.Cleanup(kill)
	return kill
}

// waitAttached waits, within serveStep, for the watcher to have heard n.
func (r *idleRig) waitAttached(n int) {
	r.t.Helper()
	deadline := time.Now().Add(serveStep)
	for {
		r.w.mu.Lock()
		got := r.w.attached
		r.w.mu.Unlock()
		if got == n {
			return
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("the watcher heard %d attached, want %d, after %v", got, n, serveStep)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// shorten sets v to d for the test.
func shorten(t *testing.T, v *time.Duration, d time.Duration) {
	t.Helper()
	prev := *v
	*v = d
	t.Cleanup(func() { *v = prev })
}

// TestAnUnattendedIdleHostExits (AC4): host_idle_exit = "2s" — nothing
// happens before the grace, then the clock runs from the look that armed it;
// at 2 s of nothing the close fence is raised and the verdict is the stop,
// with both fences left up: an attach and a prompt are refused closing.
func TestAnUnattendedIdleHostExits(t *testing.T) {
	r := newIdleRig(t, "host_idle_exit = \"2s\"\n", true)
	r.prompted()
	if l := r.wantStay("before the grace"); l.Armed {
		t.Fatalf("armed before the grace: %+v", l)
	}
	r.advance(hostStartupGrace)
	if l := r.wantStay("the grace over"); !l.Armed || l.Idle != 0 {
		t.Fatalf("the look that arms the clock: %+v", l)
	}
	r.advance(1999 * time.Millisecond)
	if l := r.wantStay("1.999 s idle"); l.Fenced || l.Limit != 2*time.Second || !l.Timed {
		t.Fatalf("%+v", l)
	}
	r.advance(time.Millisecond)
	if l := r.wantStop("2 s idle", "idle for 2s with no client attached"); !l.Fenced {
		t.Fatalf("a stop without the close fence: %+v", l)
	}
	ctx, cancel := context.WithTimeout(context.Background(), serveStep)
	defer cancel()
	c, err := remote.Dial(ctx, r.sock, remote.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	var e *remote.Error
	if _, err := c.Attach(ctx, remote.AttachOptions{SessionID: r.eng.State().CrazeSessionID}); !errors.As(err, &e) || e.Reason != protocol.ReasonClosing {
		t.Fatalf("an attach after the verdict: %v, want refused closing", err)
	}
	if _, err := r.eng.Submit(engine.Command{}, "late", engine.SubmitQueue, ""); !errors.Is(err, engine.ErrClosing) {
		t.Fatalf("a prompt after the verdict: %v, want ErrClosing", err)
	}
}

// TestTheIdleClockWaitsForTheStartAndTheGrace: a session still starting is
// never timed, however long; once it has started the clock waits for the
// grace — or for a first attach, which arms it at once (a host spawned for a
// client is not reaped before the client dials, and not kept past it).
func TestTheIdleClockWaitsForTheStartAndTheGrace(t *testing.T) {
	r := newIdleRig(t, "host_idle_exit = \"0\"\n", false)
	r.advance(time.Hour)
	if l := r.wantStay("starting"); l.Armed {
		t.Fatalf("a session still starting is timed: %+v", l)
	}
	if err := r.eng.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.wantStop("started, the grace long over", "never prompted")

	r = newIdleRig(t, "host_idle_exit = \"0\"\n", true)
	if l := r.wantStay("within the grace"); l.Armed {
		t.Fatalf("armed within the grace with nobody attached: %+v", l)
	}
	kill := r.attach()
	r.waitAttached(1)
	if l := r.wantStay("attached"); !l.Armed {
		t.Fatalf("a first attach does not arm the clock: %+v", l)
	}
	kill()
	r.waitAttached(0)
	r.wantStop("its client gone, within the grace", "never prompted")
}

// TestEachInFlightConditionKeepsTheHost: with host_idle_exit = "0" and the
// grace over, each thing in flight keeps the host however long the clock
// runs — a turn working, the agent's own turn, an ask open, a sub-agent
// running, a row queued, a settings command in progress, a replay — and the
// clock starts again from the moment it is over. Starting is the start's own
// test (TestTheIdleClockWaitsForTheStartAndTheGrace).
func TestEachInFlightConditionKeepsTheHost(t *testing.T) {
	for _, tc := range []struct {
		name string
		// hold puts the condition in flight and answers what ends it.
		hold func(t *testing.T, r *idleRig) (end func())
	}{
		{"a turn working", func(t *testing.T, r *idleRig) func() {
			hung := r.stub.HangNext()
			res, err := r.eng.Submit(engine.Command{}, "hang", engine.SubmitQueue, "")
			if err != nil {
				t.Fatal(err)
			}
			<-hung
			return func() {
				ctx, cancel := context.WithTimeout(context.Background(), serveStep)
				defer cancel()
				if _, err := r.eng.Cancel(ctx, engine.Command{}, res.Turn); err != nil {
					t.Fatal(err)
				}
			}
		}},
		{"the agent's own turn", func(t *testing.T, r *idleRig) func() {
			r.stub.SetForeignTurn(agent.ForeignTurnInfo{Running: true})
			return func() { r.stub.SetForeignTurn(agent.ForeignTurnInfo{Running: false}) }
		}},
		{"an ask open", func(t *testing.T, r *idleRig) func() {
			r.stub.Emit(agent.Event{Type: agent.EventPermission, Permission: &agent.PermissionEvent{ID: "perm-1", Tool: "Shell",
				Options: []agent.PermissionOption{{OptionID: "allow", Name: "Allow", Kind: "allow_once"}}}})
			if n := r.eng.State().PendingAsks; n != 1 {
				t.Fatalf("the premise: %d asks open", n)
			}
			return func() {
				if err := r.eng.Answer(engine.Command{}, "perm-1", agent.AskAnswer{OptionID: "allow"}); err != nil {
					t.Fatal(err)
				}
			}
		}},
		{"a sub-agent running", func(t *testing.T, r *idleRig) func() {
			r.stub.SetSubagents([]agent.SubagentInfo{{ID: "c-1", Status: agent.SubagentRunning, Background: true}})
			return func() {
				r.stub.SetSubagents([]agent.SubagentInfo{{ID: "c-1", Status: agent.SubagentCompleted, Background: true}})
			}
		}},
		{"a row queued", func(t *testing.T, r *idleRig) func() {
			row, err := r.eng.Queue(engine.Command{}, "later")
			if err != nil {
				t.Fatal(err)
			}
			return func() {
				if _, err := r.eng.Unqueue(engine.Command{}, row.ID); err != nil {
					t.Fatal(err)
				}
			}
		}},
		{"a settings command", func(t *testing.T, r *idleRig) func() {
			held, release := r.stub.HoldNextSet()
			done := make(chan error, 1)
			go func() {
				_, err := r.eng.Set(context.Background(), engine.Command{}, engine.Setting{Kind: engine.SettingModel, Value: "fast"})
				done <- err
			}()
			select {
			case <-held:
			case <-time.After(serveStep):
				t.Fatal("the Set never reached the provider")
			}
			return func() {
				release()
				if err := <-done; err != nil {
					t.Fatal(err)
				}
			}
		}},
		{"a replay", func(t *testing.T, r *idleRig) func() {
			r.stub.Emit(agent.Event{Type: agent.EventReplay, Replay: &agent.ReplayInfo{Phase: agent.ReplayStart}})
			if err := r.eng.Sync(context.Background()); err != nil {
				t.Fatal(err)
			}
			return func() {
				r.stub.Emit(agent.Event{Type: agent.EventReplay, Replay: &agent.ReplayInfo{Phase: agent.ReplayEnd}})
				if err := r.eng.Sync(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newIdleRig(t, "host_idle_exit = \"0\"\n", true)
			r.advance(hostStartupGrace)
			end := tc.hold(t, r)
			for range 3 {
				r.advance(time.Hour)
				if l := r.wantStay(tc.name); l.Fenced {
					t.Fatalf("%s: the clock ran out: %+v", tc.name, l)
				}
			}
			end()
			r.waitIdle()
			r.advance(time.Second)
			r.wantStop(tc.name+" over", "idle for")
		})
	}
}

// TestANeverPromptedHostUsesTheShorterLimit: host_idle_exit = "1h", and a
// session that has had no prompt — nothing to resume — exits at the
// never-prompted cap (5 minutes in production, shortened here); one that has
// had a prompt waits the hour.
func TestANeverPromptedHostUsesTheShorterLimit(t *testing.T) {
	shorten(t, &hostNeverPromptedIdle, 3*time.Second)
	r := newIdleRig(t, "host_idle_exit = \"1h\"\n", true)
	r.advance(hostStartupGrace)
	r.wantStay("armed")
	r.advance(3*time.Second - time.Millisecond)
	if l := r.wantStay("just short of the cap"); l.Limit != 3*time.Second {
		t.Fatalf("the never-prompted limit is %s", l.Limit)
	}
	r.advance(time.Millisecond)
	r.wantStop("at the cap", "never prompted")

	r = newIdleRig(t, "host_idle_exit = \"1h\"\n", true)
	r.prompted()
	r.advance(hostStartupGrace)
	r.wantStay("armed")
	r.advance(3 * time.Second)
	if l := r.wantStay("a prompted session at the cap"); l.Limit != time.Hour {
		t.Fatalf("a prompted session's limit is %s", l.Limit)
	}
	r.advance(time.Hour)
	r.wantStop("a prompted session at the hour", "idle for")
}

// TestAStartFailedHostExitsWhenItsClientLeaves: a session whose start failed
// is kept while its client — one that attached while it started, and was sent
// the failure — is attached, and ends as soon as the client goes, whatever
// host_idle_exit says, "never" included; but only once the grace is over when
// nobody has attached at all, so its launcher can still attach and be told
// (an attach after the failure is refused start_failed, and counts for
// nothing).
func TestAStartFailedHostExitsWhenItsClientLeaves(t *testing.T) {
	r := newIdleRig(t, "host_idle_exit = \"never\"\n", false)
	kill := r.attachWhen(protocol.WhenNow)
	r.waitAttached(1)
	r.eng.Started(errors.New("auth failed"))
	r.wantStay("its client attached, within the grace")
	r.advance(time.Hour)
	r.wantStay("its client attached")
	kill()
	r.waitAttached(0)
	r.wantStop("its client gone", "did not start")

	r = newIdleRig(t, "host_idle_exit = \"never\"\n", false)
	r.eng.Started(errors.New("auth failed"))
	r.advance(hostStartupGrace - time.Millisecond)
	r.wantStay("nobody came, within the grace")
	r.advance(time.Millisecond)
	r.wantStop("nobody came, the grace over", "did not start")
}

// TestNeverKeepsAnIdleHost: host_idle_exit = "never" keeps an idle,
// unattached, prompted host for as long as the clock runs.
func TestNeverKeepsAnIdleHost(t *testing.T) {
	r := newIdleRig(t, "host_idle_exit = \"never\"\n", true)
	r.prompted()
	r.advance(hostStartupGrace)
	for range 3 {
		r.advance(1000 * time.Hour)
		if l := r.wantStay("never"); l.Timed || l.Fenced {
			t.Fatalf("never is timed: %+v", l)
		}
	}
}

// TestAnAttachArrivingAtExpiryKeepsTheHost: the clock runs out, and in the
// instant before the close fence a client attaches (beforeFence): the fence
// counts it, the verdict is a stay, both fences come down — the client's
// session works — and the clock starts again.
func TestAnAttachArrivingAtExpiryKeepsTheHost(t *testing.T) {
	r := newIdleRig(t, "host_idle_exit = \"2s\"\n", true)
	r.prompted()
	r.advance(hostStartupGrace)
	r.wantStay("armed")
	r.advance(2 * time.Second)
	r.beforeFence = func() { r.attach() }
	l := r.wantStay("an attach at expiry")
	if !l.Fenced {
		t.Fatalf("the clock did not run out: %+v", l)
	}
	if _, err := r.eng.Submit(engine.Command{}, "still here", engine.SubmitQueue, ""); err != nil {
		t.Fatalf("a prompt once the host stayed: %v", err)
	}
	r.waitIdle()
	r.waitAttached(1)
	r.advance(time.Hour)
	r.wantStay("attached")
}

// TestAListPollerDoesNotKeepAHost: a connection kept open that says hello and
// lists at every look — the agent view's poller — keeps nothing: the host
// exits at its limit.
func TestAListPollerDoesNotKeepAHost(t *testing.T) {
	r := newIdleRig(t, "host_idle_exit = \"2s\"\n", true)
	r.prompted()
	ctx, cancel := context.WithTimeout(context.Background(), serveStep)
	defer cancel()
	c, err := remote.Dial(ctx, r.sock, remote.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	poll := func() {
		var list protocol.SessionsListResult
		if err := c.Call(ctx, protocol.MethodSessionsList, protocol.SessionsListParams{}, &list); err != nil || len(list.Sessions) != 1 {
			t.Fatalf("sessions.list: %+v, %v", list, err)
		}
	}
	r.advance(hostStartupGrace)
	poll()
	r.wantStay("armed")
	for range 3 {
		poll()
		r.advance(time.Second)
		poll()
		if l := r.look(); l.Stop != "" {
			return
		}
	}
	t.Fatal("a list poller kept the host past its limit")
}

// TestAKilledClientDoesNotPinAnIdleHost is C1's half-close hazard: a client
// attached to an idle session and killed — its connection closed with no
// detach, and nothing ever written to it after — leaves the host's count at
// its read EOF, and the host exits at its limit.
func TestAKilledClientDoesNotPinAnIdleHost(t *testing.T) {
	r := newIdleRig(t, "host_idle_exit = \"2s\"\n", true)
	r.prompted()
	kill := r.attach()
	r.waitAttached(1)
	r.advance(hostStartupGrace)
	r.wantStay("attached")
	kill()
	r.waitAttached(0)
	r.advance(2 * time.Second)
	r.wantStop("its only client killed", "idle for")
}

// TestALostSocketStopsTheHost (SF-66): the socket-lost check answers at every
// look, before anything else — a client attached, a turn working, the start
// still running — and its words are the stop's cause.
func TestALostSocketStopsTheHost(t *testing.T) {
	r := newIdleRig(t, "host_idle_exit = \"never\"\n", false)
	r.loseSocket("its control socket /run/x.sock is gone")
	r.wantStop("starting", "its control socket /run/x.sock is gone")
}
