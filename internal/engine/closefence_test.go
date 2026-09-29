package engine

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
)

// The close fence (closefence.go; plan 030 §3.6, R2-1). Every schedule here is
// forced with the fake session's own barriers — a held turn, a held Set, a
// pending wake — never left to the scheduler, and every wait is bounded on its
// own (watchdog).

// fenced raises one close fence on r's engine, released when the test ends
// if the test has not released it itself.
func fenced(t *testing.T, e *Engine) (release func(), busy bool) {
	t.Helper()
	release, busy = e.FenceClose()
	t.Cleanup(release)
	return release, busy
}

// wantClosing: err is the fence's refusal — ErrClosing, code unavailable,
// reason closing, never stored.
func wantClosing(t *testing.T, what string, err error) {
	t.Helper()
	if !errors.Is(err, ErrClosing) {
		t.Fatalf("%s under a close fence: %v, want ErrClosing", what, err)
	}
	if Code(err) != "unavailable" || Reason(err) != "closing" || !gateRefusal(err) {
		t.Fatalf("%s: code %q reason %q forgotten %v, want unavailable/closing, never stored", what, Code(err), Reason(err), gateRefusal(err))
	}
}

// TestACloseFenceRefusesEveryAdmission: with a close fence up on an idle
// engine, every admission the engine has — a prompt (queue and send-now), the
// four queue verbs, Disarm, Interject, Set and SetTitle — is refused
// ErrClosing, having changed nothing; nothing is busy; and once the fence is
// released the same command ids run, as a gate refusal's may (never stored).
func TestACloseFenceRefusesEveryAdmission(t *testing.T) {
	r := newRig(t, Options{})
	e := r.e
	row := r.queue("waiting")
	if _, err := e.Unqueue(Command{}, row.ID); err != nil {
		t.Fatal(err)
	}
	client := e.NewClientID()
	cmd := func(id string) Command { return Command{Client: client, ID: id} }

	release, busy := fenced(t, e)
	if busy {
		t.Fatal("an idle engine is busy under its close fence")
	}
	_, err := e.Submit(cmd("1"), "hello", SubmitQueue, "")
	wantClosing(t, "Submit", err)
	_, err = e.Submit(cmd("2"), "now", SubmitSendNow, "")
	wantClosing(t, "a send-now", err)
	_, err = e.Queue(cmd("3"), "later")
	wantClosing(t, "Queue", err)
	wantClosing(t, "EditQueued", e.EditQueued(cmd("4"), "q-1", "x", nil))
	_, err = e.Unqueue(cmd("5"), "q-1")
	wantClosing(t, "Unqueue", err)
	_, err = e.ClearQueue(cmd("6"))
	wantClosing(t, "ClearQueue", err)
	wantClosing(t, "Disarm", e.Disarm(cmd("7")))
	wantClosing(t, "Interject", e.Interject(context.Background(), cmd("8"), "also"))
	_, err = e.Set(context.Background(), cmd("9"), Setting{Kind: SettingMode, Value: "plan"})
	wantClosing(t, "Set", err)
	wantClosing(t, "SetTitle", e.SetTitle(cmd("10"), "renamed"))
	_, _, err = e.GiveUpDrain(cmd("11"))
	wantClosing(t, "GiveUpDrain", err)
	if got := r.s.prompts(); len(got) != 0 {
		t.Fatalf("the session was prompted under the fence: %q", got)
	}
	if st := e.State(); len(st.Queue) != 0 || st.Activity != ActivityIdle || st.Title == "renamed" {
		t.Fatalf("the fence let a change through: %+v", st)
	}

	release()
	release() // idempotent
	res, err := e.Submit(cmd("1"), "hello", SubmitQueue, "")
	if err != nil || res.Turn == "" {
		t.Fatalf("the same id once the fence is down: %+v, %v", res, err)
	}
	r.until(lastEnding)
	if err := e.SetTitle(cmd("10"), "renamed"); err != nil {
		t.Fatalf("SetTitle's id once the fence is down: %v", err)
	}
}

// TestACloseFenceStacks: two fences up, one released, still refuses; the
// engine admits once both are down.
func TestACloseFenceStacks(t *testing.T) {
	r := newRig(t, Options{})
	one, _ := fenced(t, r.e)
	two, _ := fenced(t, r.e)
	one()
	_, err := r.e.Queue(Command{}, "x")
	wantClosing(t, "Queue with one fence of two down", err)
	two()
	r.queue("x")
}

// TestTheCloseFenceSeesEverythingInFlight is the busy verdict, one condition
// at a time, each against FenceClose and the unfenced sample (Busy): the
// engine starting, replaying, working (with a cancel on its way, and a
// send-now armed), a turn of the agent's own, an ask open, a sub-agent
// running, rows queued, a settings command running and one
// queued behind it, and a closed engine. An idle engine is not busy, nor is
// one whose last turn failed.
func TestTheCloseFenceSeesEverythingInFlight(t *testing.T) {
	check := func(t *testing.T, e *Engine, want bool, what string) {
		t.Helper()
		if got := e.Busy(); got != want {
			t.Fatalf("%s: Busy = %v, want %v", what, got, want)
		}
		release, got := e.FenceClose()
		release()
		if got != want {
			t.Fatalf("%s: FenceClose busy = %v, want %v", what, got, want)
		}
	}

	t.Run("idle", func(t *testing.T) {
		r := newRig(t, Options{})
		check(t, r.e, false, "idle")
	})
	t.Run("a failed turn", func(t *testing.T) {
		r := newRig(t, Options{})
		r.s.script(&script{fail: errors.New("the agent fell over")})
		r.submit("x")
		r.until(ended(""))
		if st := r.e.State(); st.Activity != ActivityError {
			t.Fatalf("the premise: %s", st.Activity)
		}
		check(t, r.e, false, "an error state")
	})
	t.Run("starting", func(t *testing.T) {
		s := newFake(t, agent.EventLogOptions{NoPrimary: true})
		e, err := New(s, Options{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = e.Close() })
		check(t, e, true, "not yet started")
	})
	t.Run("replaying", func(t *testing.T) {
		r := newRig(t, Options{})
		r.s.emit(agent.Event{Type: agent.EventReplay, Replay: &agent.ReplayInfo{Phase: agent.ReplayStart}})
		r.until(func(ev agent.Event) bool { return ev.Type == agent.EventReplay })
		check(t, r.e, true, "replaying")
	})
	t.Run("working", func(t *testing.T) {
		r := newRig(t, Options{})
		sc := r.s.script(held())
		r.submit("x")
		await(t, sc.opened, "the turn to open")
		check(t, r.e, true, "a turn working")
		sc.release()
		r.until(lastEnding)
		check(t, r.e, false, "the turn over")
	})
	t.Run("a send-now armed", func(t *testing.T) {
		r := newRig(t, Options{})
		r.ignoreCancels()
		sc := r.s.script(held())
		r.submit("x")
		await(t, sc.opened, "the turn to open")
		r.sendNow("instead", "")
		r.until(armedNow)
		check(t, r.e, true, "a send-now armed")
		sc.release()
		r.until(lastEnding)
	})
	t.Run("a turn of the agent's own", func(t *testing.T) {
		r := newRig(t, Options{})
		r.s.setForeign(true)
		check(t, r.e, true, "a foreign turn")
		r.s.setForeign(false)
		check(t, r.e, false, "the foreign turn over")
	})
	t.Run("an ask open", func(t *testing.T) {
		// An ask is opened inside a turn and ended with it (the registry's
		// turn_ended), so it never stands alone in an engine; the registry's
		// half of the verdict is held on its own below, and here it is read
		// with the engine working.
		r := newRig(t, Options{})
		sc := r.s.script(asking(held(), true))
		r.submit("x")
		await(t, sc.ask.opened, "the ask to open")
		if st := r.e.State(); st.PendingAsks != 1 {
			t.Fatalf("the premise: %d asks open", st.PendingAsks)
		}
		check(t, r.e, true, "an ask open")
		if !busyElsewhere(agent.Snapshot{}, r.e.Asks()) {
			t.Fatal("the registry's half of the verdict misses an open ask")
		}
		if busyElsewhere(agent.Snapshot{}, nil) {
			t.Fatal("the registry's half of the verdict is busy with nothing open")
		}
		sc.release()
	})
	t.Run("a sub-agent running", func(t *testing.T) {
		r := newRig(t, Options{})
		r.s.mu.Lock()
		r.s.snap.Subagents = []agent.SubagentInfo{{ID: "c-1", Status: agent.SubagentCompleted}, {ID: "c-2", Status: agent.SubagentRunning, Background: true}}
		r.s.mu.Unlock()
		check(t, r.e, true, "a background child running")
		r.s.mu.Lock()
		r.s.snap.Subagents[1].Status = agent.SubagentCompleted
		r.s.mu.Unlock()
		check(t, r.e, false, "every child done")
	})
	t.Run("rows queued", func(t *testing.T) {
		r := newRig(t, Options{})
		r.queue("later")
		check(t, r.e, true, "a row queued")
	})
	t.Run("a settings command", func(t *testing.T) {
		r := newRig(t, Options{})
		release := r.s.holdNextSets()
		first := make(chan error, 1)
		go func() {
			_, err := r.e.Set(context.Background(), Command{}, Setting{Kind: SettingMode, Value: "plan"})
			first <- err
		}()
		waitFor(t, func() bool { // the worker has taken the Set
			r.s.mu.Lock()
			defer r.s.mu.Unlock()
			return r.s.setsHeld == 1
		})
		check(t, r.e, true, "a Set running")
		second := make(chan error, 1)
		go func() {
			_, err := r.e.Set(context.Background(), Command{}, Setting{Kind: SettingMode, Value: "ask"})
			second <- err
		}()
		waitFor(t, func() bool { // the second Set is queued
			r.e.mu.Lock()
			defer r.e.mu.Unlock()
			return len(r.e.sets) == 1
		})
		release()
		for _, ch := range []chan error{first, second} {
			select {
			case err := <-ch:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(watchdog):
				t.Fatal("a Set never answered")
			}
		}
		check(t, r.e, false, "both Sets answered")
	})
	t.Run("closed", func(t *testing.T) {
		r := newRig(t, Options{})
		_ = r.e.Close()
		check(t, r.e, true, "a closed engine")
	})
}

// TestASetTakenAfterTheFenceIsRefused: a Set queued before the fence and
// taken by the worker after it is refused in the worker's claim, ErrClosing,
// having asked the provider nothing — the second of Set's two gates.
func TestASetTakenAfterTheFenceIsRefused(t *testing.T) {
	h := &hooks{}
	parked := make(chan struct{})
	resume := make(chan struct{})
	h.beforeRunSet = func() {
		close(parked)
		<-resume
	}
	r := newRigHooked(t, Options{}, agent.EventLogOptions{NoPrimary: true}, h)
	done := make(chan error, 1)
	go func() {
		_, err := r.e.Set(context.Background(), Command{}, Setting{Kind: SettingMode, Value: "plan"})
		done <- err
	}()
	await(t, parked, "the worker to take the Set")
	release, busy := fenced(t, r.e)
	if !busy {
		t.Fatal("a Set the worker has taken is not a settings command in progress")
	}
	close(resume)
	select {
	case err := <-done:
		wantClosing(t, "a Set claimed under the fence", err)
	case <-time.After(watchdog):
		t.Fatal("the Set never answered")
	}
	if r.s.setCalls() != 0 {
		t.Fatal("the provider was asked under the fence")
	}
	release()
}

// TestASuccessorTheFenceHeldBackStartsWhenItComesDown: a turn working with a
// row queued behind it, a close fence raised (busy), the turn then settling
// under it — its successor is not claimed while the fence stands — and the
// fence released: the successor starts then, from the replayed kick, with no
// other event to wake the driver.
func TestASuccessorTheFenceHeldBackStartsWhenItComesDown(t *testing.T) {
	r, returned := newRigReturning(t, Options{})
	sc := r.s.script(held())
	first := r.submit("first").Turn
	await(t, sc.opened, "the turn to open")
	r.queue("second")
	release, busy := fenced(t, r.e)
	if !busy {
		t.Fatal("a working turn is not busy")
	}
	sc.release()
	awaitTurn(t, returned, first)
	r.until(ended(first))
	r.sync()
	if st := r.e.State(); st.Turn != "" || len(st.Queue) != 1 {
		t.Fatalf("under the fence the settlement claimed its successor: turn %q, queue %d", st.Turn, len(st.Queue))
	}
	release()
	r.until(started(""))
	r.until(lastEnding)
	r.wantPrompts("first", "second")
}

// TestAnIdleQueueStaysUndrainedAcrossAFence: rows the Queue verb put on an
// idle engine are not drained by anything but a kick, and a fence that held
// no pass back replays none: its release starts nothing.
func TestAnIdleQueueStaysUndrainedAcrossAFence(t *testing.T) {
	r := newRig(t, Options{})
	r.queue("waiting")
	release, busy := fenced(t, r.e)
	if !busy {
		t.Fatal("a queued row is not busy")
	}
	release()
	r.sync()
	if got := r.s.prompts(); len(got) != 0 {
		t.Fatalf("the fence's release drained the idle queue: %q", got)
	}
}

// TestACloseFenceKeepsTheSessionsFenceUp: over a session that can start a
// turn of its own (native's wake), a close fence keeps the session's
// admission fence up on an idle engine — a wake pending then cannot start —
// and its release lets the wake start.
func TestACloseFenceKeepsTheSessionsFenceUp(t *testing.T) {
	fr := newFenceRig(t, ChainPolicy{})
	fr.want("idle", false)
	release, busy := fenced(t, fr.e)
	if busy {
		t.Fatal("an idle engine is busy")
	}
	fr.want("fenced", true, "up", "foreign")
	fr.fs.pendWake()
	if fr.fs.fakeSession.ForeignTurn() {
		t.Fatal("a wake started under the close fence")
	}
	release()
	fr.want("released", false, "down", "wake")
	if !fr.fs.fakeSession.ForeignTurn() {
		t.Fatal("the wake did not start once the fence came down")
	}
	if busy := fr.e.Busy(); !busy {
		t.Fatal("the wake's turn is not busy")
	}
}

// TestAnAdmittedCommandIsBusyUntilItsWorkIsOver (plan 030 C5r, astra r8-c5
// 2): a command admitted before the close fence whose work runs after e.mu is
// released — a rename (its session call and index row), an interjection (its
// session call), a submit's first-prompt seed — is in flight to the fence from
// its admission until that work is over, so the verdict cannot come between
// them. Each is paused right after its admission, at its own barrier; the
// fence then reads busy (and so does the sample), the host stays and lowers
// it, the work finishes, and only then is the engine idle.
func TestAnAdmittedCommandIsBusyUntilItsWorkIsOver(t *testing.T) {
	// paused runs cmd on a goroutine, waits for it to reach its barrier and
	// for settled (when set: whatever else the command did to be over in its
	// own right), and checks the verdict there and once cmd has returned;
	// resume lets it go.
	paused := func(t *testing.T, e *Engine, parked <-chan struct{}, settled, resume func(), what string, cmd func() error) {
		t.Helper()
		done := make(chan error, 1)
		go func() { done <- cmd() }()
		await(t, parked, what+" to be admitted and paused")
		if settled != nil {
			settled()
		}
		release, busy := fenced(t, e)
		if !busy {
			t.Fatalf("%s admitted before the close fence and not yet over: the fence reads idle", what)
		}
		release() // the host stays, as its watcher does on a busy verdict
		if !e.Busy() {
			t.Fatalf("%s admitted and not yet over: Busy is false", what)
		}
		resume()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, agent.ErrUnsupported) {
				t.Fatalf("%s: %v", what, err)
			}
		case <-time.After(watchdog):
			t.Fatalf("%s never returned", what)
		}
		if e.Busy() {
			t.Fatalf("%s over: still busy", what)
		}
		release, busy = fenced(t, e)
		release()
		if busy {
			t.Fatalf("%s over: the fence reads busy", what)
		}
	}

	t.Run("a rename", func(t *testing.T) {
		r := newRig(t, Options{})
		entered, release := r.s.holdNextTitle()
		t.Cleanup(release)
		paused(t, r.e, entered, nil, release, "a rename", func() error { return r.e.SetTitle(Command{}, "renamed") })
		if st := r.e.State(); st.Title != "renamed" {
			t.Fatalf("the rename: title %q", st.Title)
		}
	})
	t.Run("an interjection", func(t *testing.T) {
		r := newRig(t, Options{})
		entered, release := r.s.holdNextInterject()
		t.Cleanup(release)
		paused(t, r.e, entered, nil, release, "an interjection", func() error {
			return r.e.Interject(context.Background(), Command{}, "also")
		})
	})
	t.Run("a submit's seed", func(t *testing.T) {
		h := &hooks{}
		parked, resume := make(chan struct{}), make(chan struct{})
		var once sync.Once
		h.beforeInlineSeed = func(string) {
			once.Do(func() { close(parked) })
			<-resume
		}
		r := newRigHooked(t, Options{}, agent.EventLogOptions{NoPrimary: true}, h)
		var resumed sync.Once
		let := func() { resumed.Do(func() { close(resume) }) }
		t.Cleanup(let)
		paused(t, r.e, parked, func() {
			// The turn itself ends meanwhile, in its own right: only the seed
			// the submit still owes is in flight.
			r.until(lastEnding)
			if st := r.e.State(); st.Turn != "" || len(st.Queue) != 0 {
				t.Fatalf("the premise: %+v", st)
			}
		}, let, "a submit", func() error {
			_, err := r.e.Submit(Command{}, "hello", SubmitQueue, "")
			return err
		})
	})
}

// TestAFailedLoadIsNotReplayingForEver (plan 030 C5r, astra r8-c5 3): a load
// publishes its replay's start bracket and, failing, never its end (every
// provider's load); the start's failure (Started) retires it, so the engine is
// not busy — the fence's verdict and the sample both — and a start bracket
// arriving after the failure opens nothing.
func TestAFailedLoadIsNotReplayingForEver(t *testing.T) {
	s := newFake(t, agent.EventLogOptions{NoPrimary: true})
	e, err := New(s, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	s.emit(agent.Event{Type: agent.EventReplay, Replay: &agent.ReplayInfo{Phase: agent.ReplayStart}})
	if !e.isReplaying() {
		t.Fatal("the premise: the load's start bracket opened no replay")
	}
	e.Started(errors.New("agent: session/load: Session not found"))
	if st := e.State(); !st.StartFailed || st.Activity != ActivityError {
		t.Fatalf("the premise: %+v", st)
	}
	idle := func(what string) {
		t.Helper()
		if e.Busy() {
			t.Fatalf("%s: Busy", what)
		}
		release, busy := e.FenceClose()
		release()
		if busy {
			t.Fatalf("%s: the fence reads busy", what)
		}
	}
	idle("a failed load")
	s.emit(agent.Event{Type: agent.EventReplay, Replay: &agent.ReplayInfo{Phase: agent.ReplayStart}})
	idle("a start bracket after the failure")
}

// TestWorkTheSessionOwesOutsideATurnIsBusy (plan 030 C5r, astra r8-c5 1): a
// session's own work outside any turn (agent.OwedWork) is in flight to the
// fence — a background child whose roster row already reads finished but whose
// result is not yet published, and a published result whose wake the close
// fence itself holds back — though the roster shows nothing running and the
// agent runs no turn. With the fence released the wake starts, and its turn is
// busy in its own right.
func TestWorkTheSessionOwesOutsideATurnIsBusy(t *testing.T) {
	fr := newFenceRig(t, ChainPolicy{})
	fr.s.mu.Lock()
	fr.s.snap.Subagents = []agent.SubagentInfo{{ID: "c-1", Status: agent.SubagentCompleted, Background: true}}
	fr.s.mu.Unlock()

	// Between the child's finish and its result's publication.
	fr.fs.owe(true)
	if !fr.e.Busy() {
		t.Fatal("a finished child's unpublished result: Busy is false")
	}
	release, busy := fenced(t, fr.e)
	release()
	if !busy {
		t.Fatal("a finished child's unpublished result: the fence reads idle")
	}

	// Published while a fence is up: its wake waits for the fence to come
	// down, and a second verdict taken meanwhile must see it.
	fr.fs.owe(false)
	release, busy = fenced(t, fr.e)
	if busy {
		t.Fatal("the premise: nothing owed")
	}
	fr.fs.pendWake()
	if !fr.e.Busy() {
		t.Fatal("a result pending for a wake the fence holds back: Busy is false")
	}
	again, busy := fenced(t, fr.e)
	again()
	if !busy {
		t.Fatal("a result pending for a wake the fence holds back: the fence reads idle")
	}
	release()
	if !fr.fs.fakeSession.ForeignTurn() {
		t.Fatal("the wake did not start once the fence came down")
	}
	if !fr.e.Busy() {
		t.Fatal("the wake's turn is not busy")
	}
}
