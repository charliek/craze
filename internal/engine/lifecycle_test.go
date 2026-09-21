package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
)

// TestNewRefusesASessionWithNoLogAndASecondEngine: the engine publishes through
// the session's log and is its one observer, so a session without a log cannot
// be driven and a session that already has an engine cannot have another — two
// drivers on one session is the double drain this package exists to end.
func TestNewRefusesASessionWithNoLogAndASecondEngine(t *testing.T) {
	if _, err := New(logless{}, Options{}); err == nil {
		t.Fatal("New accepted a session with no event log")
	}
	s := newFake(t, agent.EventLogOptions{NoPrimary: true})
	e, err := New(s, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	if _, err := New(s, Options{}); !errors.Is(err, agent.ErrObserverSet) {
		t.Fatalf("a second engine on one session: %v, want agent.ErrObserverSet", err)
	}
}

// logless is a session with no log: the seam alone.
type logless struct{ agent.Session }

// TestNothingIsAdmittedBeforeTheSessionIsUp: every command is refused until
// Start has returned, refused for good when it failed, and refused again while
// a replay runs.
func TestNothingIsAdmittedBeforeTheSessionIsUp(t *testing.T) {
	s := newFake(t, agent.EventLogOptions{NoPrimary: true})
	e, err := New(s, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	if st := e.State(); st.Activity != ActivityStarting {
		t.Fatalf("activity before Start: %s", st.Activity)
	}
	if _, err := e.Submit(Command{}, "early", SubmitQueue, ""); !errors.Is(err, ErrNotAccepting) || Code(err) != "not_accepting" {
		t.Fatalf("a submit before Start: %v", err)
	}
	if err := e.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	sub, err := e.Subscribe(agent.SubscribeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sub.Close)
	r := &rig{t: t, s: s, e: e, sub: sub}
	s.emit(agent.Event{Type: agent.EventReplay, Replay: &agent.ReplayInfo{Phase: agent.ReplayStart}})
	r.until(func(ev agent.Event) bool { return ev.Type == agent.EventReplay })
	if st := e.State(); st.Activity != ActivityReplaying {
		t.Fatalf("activity during a replay: %s", st.Activity)
	}
	if _, err := e.Submit(Command{}, "mid-replay", SubmitQueue, ""); !errors.Is(err, ErrNotAccepting) {
		t.Fatalf("a submit during a replay: %v", err)
	}
	s.emit(agent.Event{Type: agent.EventReplay, Replay: &agent.ReplayInfo{Phase: agent.ReplayEnd}})
	r.until(func(ev agent.Event) bool { return ev.Type == agent.EventReplay })
	r.submit("after")
	r.until(lastEnding)
	if len(s.prompts()) != 1 {
		t.Fatalf("prompts: %q", s.prompts())
	}
}

func TestAFailedStartIsAnErrorThatAdmitsNothing(t *testing.T) {
	s := newFake(t, agent.EventLogOptions{NoPrimary: true})
	boom := errors.New("the agent would not start")
	s.startErr = boom
	e, err := New(s, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	if err := e.Start(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("start: %v", err)
	}
	st := e.State()
	if !st.StartFailed || st.Activity != ActivityError || st.Err != boom.Error() {
		t.Fatalf("state after a failed start: %+v", st)
	}
	if _, err := e.Submit(Command{}, "anyway", SubmitQueue, ""); !errors.Is(err, ErrNotAccepting) {
		t.Fatalf("a submit after a failed start: %v", err)
	}
}

// TestCloseEndsTheTurnAndJoinsEverything: closing the engine closes the
// session, which ends the turn that is running, and returns only once the
// driver and every turn goroutine has gone. Afterwards nothing is admitted, and
// a second Close is the first one's answer.
func TestCloseEndsTheTurnAndJoinsEverything(t *testing.T) {
	r := newRig(t, Options{})
	turn := r.s.script(held())
	r.submit("one")
	await(t, turn.opened, "the turn to open")
	r.submit("two")
	closed := make(chan error, 1)
	go func() { closed <- r.e.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("close: %v", err)
		}
	case <-time.After(watchdog):
		t.Fatal("Close is waiting on something a closed session does not release")
	}
	if err := r.e.Close(); err != nil {
		t.Fatalf("a second close: %v", err)
	}
	if _, err := r.e.Submit(Command{}, "late", SubmitQueue, ""); !errors.Is(err, ErrNotAccepting) {
		t.Fatalf("a submit after Close: %v", err)
	}
	if _, err := r.e.Cancel(context.Background(), Command{}, ""); !errors.Is(err, ErrNotAccepting) {
		t.Fatalf("a cancel after Close: %v", err)
	}
	// The row behind the turn Close ended was never sent.
	r.wantPrompts("one")
	if st := r.e.State(); st.Activity != ActivityClosing {
		t.Fatalf("activity after Close: %s", st.Activity)
	}
}

// TestCloseEndsTheTurnOnTheRecord is the live smoke's finding A1: a quit while
// a turn was running left that turn with a started and no ended — on every
// provider — and V3's first invariant ("every turn has exactly one started and
// one ended") did not hold for the journals it wrote. The stream is meant to be
// a complete record (SD-30).
//
// The settlement is not what was missing it: closed is set in the same locked
// section that refuses admission, and the driver's pass returns at once for a
// closed engine, so the continuation that comes back afterwards authors nothing
// at all. So Close authors the ending itself, there, before the session's close
// cuts the log — and because the whole decision is one section under e.mu, a
// settlement that got in first left no current turn for this to find, and this
// leaves no current turn for a settlement to find. Exactly one ending, whichever
// order the two arrive in.
//
// Every case below reads the PRIMARY (rig.committed), which is the reader that
// survives a close, and each reads it once, after Close has returned — which is
// after e.wg.Wait(), so the turn's own continuation has already come back and
// had whatever chance it had to author a second ending.
func TestCloseEndsTheTurnOnTheRecord(t *testing.T) {
	t.Run("a turn that was still running", func(t *testing.T) {
		r := newRigOn(t, Options{}, agent.EventLogOptions{})
		turn := r.s.script(held())
		r.submit("one")
		await(t, turn.opened, "the turn to open")

		if err := r.e.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
		got := turnRecord(r.committed())
		want := []string{`started turn-1 submit "one"`, `ended turn-1 stop="closing" next="" pending=0 synthetic`}
		if strings.Join(got, " | ") != strings.Join(want, " | ") {
			t.Fatalf("the turn's record is\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
		}
		// And State agrees with the stream it just wrote: the turn is over.
		if st := r.e.State(); st.Turn != "" || st.Activity != ActivityClosing {
			t.Fatalf("state after the close: %+v", st)
		}
	})

	t.Run("the rows behind it are counted and left where they are", func(t *testing.T) {
		r := newRigOn(t, Options{}, agent.EventLogOptions{})
		turn := r.s.script(held())
		r.submit("one")
		await(t, turn.opened, "the turn to open")
		r.queue("two")
		r.queue("three")

		if err := r.e.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
		got := turnRecord(r.committed())
		want := []string{`started turn-1 submit "one"`, `ended turn-1 stop="closing" next="" pending=2 synthetic`}
		if strings.Join(got, " | ") != strings.Join(want, " | ") {
			t.Fatalf("the turn's record is\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
		}
		// Close is not Stop: nothing is removed and nothing is said about the
		// rows, so Pending is the honest count of what is still there.
		r.wantRows("two", "three")
		r.wantPrompts("one")
	})

	t.Run("a turn that settled just before the close", func(t *testing.T) {
		r := newRigOn(t, Options{}, agent.EventLogOptions{})
		r.submit("one")
		r.until(lastEnding)

		if err := r.e.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
		got := turnRecord(r.committed())
		want := []string{`started turn-1 submit "one"`, `ended turn-1 stop="end_turn" next="" pending=0`}
		if strings.Join(got, " | ") != strings.Join(want, " | ") {
			t.Fatalf("the turn's record is\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
		}
	})

	t.Run("no turn at all", func(t *testing.T) {
		r := newRigOn(t, Options{}, agent.EventLogOptions{})
		if err := r.e.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
		if got := turnRecord(r.committed()); len(got) != 0 {
			t.Fatalf("closing an idle engine wrote %q", got)
		}
	})
}

// assertTurnRecordSelfConsistent is r32's "whatever the code does is
// self-consistent" bar for a schedule this file does not pin an exact shape
// for: every turn that started has EXACTLY one ending in the record, and none
// is missing one. It says nothing about ORDER or STOP REASON — a schedule that
// races Close against something else may settle a turn normally, or may have
// Close author its "closing" ending instead, and both are consistent answers —
// only that the stream never leaves a started turn open forever and never
// ends one twice (SD-30, plan 021's r32 review, hunt item A).
func assertTurnRecordSelfConsistent(t *testing.T, evs []agent.Event) {
	t.Helper()
	order := []string{}
	endings := map[string]int{}
	for _, ev := range evs {
		if ev.Type != agent.EventTurn || ev.Turn == nil {
			continue
		}
		id := ev.Turn.ID
		switch ev.Turn.Phase {
		case agent.TurnStarted:
			if _, seen := endings[id]; seen {
				t.Fatalf("%s started twice: %s", id, describe(evs))
			}
			endings[id] = 0
			order = append(order, id)
		case agent.TurnEnded:
			if _, started := endings[id]; !started {
				t.Fatalf("%s ended with no started in the record: %s", id, describe(evs))
			}
			endings[id]++
		}
	}
	for _, id := range order {
		if n := endings[id]; n != 1 {
			t.Fatalf("%s started but has %d endings, want exactly 1: %s", id, n, describe(evs))
		}
	}
}

// TestTwoClosesWithATurnRunningEndItOnce is r32's hunt item A / E: two Close
// calls on an engine with a turn running, released together off one closed
// channel so both are AT LEAST READY to enter Close at the same time — a
// start gun, not a guarantee that they actually overlap inside Close, since
// the scheduler may still run one to completion before the other is picked
// up. What this proves is the RECORD — the turn gets its closing ending
// exactly once, and both callers see the same nil answer — never that the two
// calls contended concurrently. Run under -race and -count so a data race in
// the shared batch or e.wg would show up as either a failure or the detector
// firing.
func TestTwoClosesWithATurnRunningEndItOnce(t *testing.T) {
	r := newRigOn(t, Options{}, agent.EventLogOptions{})
	turn := r.s.script(held())
	r.submit("one")
	await(t, turn.opened, "the turn to open")

	var wg sync.WaitGroup
	errs := make([]error, 2)
	begin := make(chan struct{})
	wg.Add(2)
	for i := range errs {
		go func(i int) {
			defer wg.Done()
			<-begin
			errs[i] = r.e.Close()
		}(i)
	}
	close(begin)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("close %d: %v", i, err)
		}
	}

	got := turnRecord(r.committed())
	want := []string{`started turn-1 submit "one"`, `ended turn-1 stop="closing" next="" pending=0 synthetic`}
	if strings.Join(got, " | ") != strings.Join(want, " | ") {
		t.Fatalf("the turn's record is\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
	if st := r.e.State(); st.Turn != "" || st.Activity != ActivityClosing {
		t.Fatalf("state after two concurrent closes: %+v", st)
	}
}

// TestACancelHoldReleasingAfterCloseSettlesOnce is r32's hunt item A: a cancel
// in flight — cancelsInFlight > 0 — whose hold is released only AFTER Close
// has already returned. The hold is forced open with afterSessionCancel,
// which runs on the Cancel caller's own goroutine after the session's own
// Cancel has already reached the agent (so the held turn's continuation
// returns and gives its e.wg count back) but before releaseHold gives the
// hold back — the exact gap r32's hunt list asks about: "the cancel path's
// 'settled exactly when the held turn is no longer current' — any wrong
// CancelResult.Outcome?".
//
// While the hold stands, the turn's continuation has returned but cannot
// settle (passLocked requires cancelsInFlight == 0), so Close finds it still
// current and authors the closing ending itself. When the hold is finally
// released, releaseHold must find e.cur already cleared, report the turn
// settled, and return with no panic and no second ending.
func TestACancelHoldReleasingAfterCloseSettlesOnce(t *testing.T) {
	atRelease, letGo := make(chan struct{}), make(chan struct{})
	r := newRigHooked(t, Options{}, agent.EventLogOptions{}, &hooks{
		afterSessionCancel: func(string) {
			close(atRelease)
			<-letGo
		},
	})
	turn := r.s.script(held())
	r.submit("one")
	await(t, turn.opened, "the turn to open")

	cancelDone := make(chan struct{})
	var cancelRes CancelResult
	var cancelErr error
	go func() {
		defer close(cancelDone)
		cancelRes, cancelErr = r.e.Cancel(context.Background(), Command{}, "turn-1")
	}()
	await(t, atRelease, "the cancel to reach the session and park before its hold is released")

	closed := make(chan error, 1)
	go func() { closed <- r.e.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("close: %v", err)
		}
	case <-time.After(watchdog):
		t.Fatal("Close is waiting on a cancel hold it does not own")
	}

	close(letGo)
	select {
	case <-cancelDone:
	case <-time.After(watchdog):
		t.Fatal("Cancel never returned once its hold was released after Close")
	}
	if cancelErr != nil {
		t.Fatalf("cancel: %v", cancelErr)
	}
	if cancelRes.Outcome != CancelSettled {
		t.Fatalf("cancel outcome after its hold released post-close: %+v, want %s", cancelRes, CancelSettled)
	}

	got := turnRecord(r.committed())
	want := []string{`started turn-1 submit "one"`, `ended turn-1 stop="closing" next="" pending=0 synthetic`}
	if strings.Join(got, " | ") != strings.Join(want, " | ") {
		t.Fatalf("the turn's record is\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

// TestStopRacesCloseAfterQueueClearingBeforeCancelRelease is r32's hunt item A:
// "Stop races Close ... including after queue clearing but before cancel
// release." beforeSessionCancel parks Stop's own cancel right after its
// locked section has stopped the engine, cleared the queue and taken its
// hold, and right before Session.Cancel is made — the window named in that
// sentence. Close is forced into exactly that window, so it must find the
// running turn still current (Stop's cancel has not settled it) and author
// the closing ending itself; Stop's own cancel, once let through, must land
// on an already-closed engine without a second ending or a panic.
func TestStopRacesCloseAfterQueueClearingBeforeCancelRelease(t *testing.T) {
	atCancel, letGo := make(chan struct{}), make(chan struct{})
	r := newRigHooked(t, Options{}, agent.EventLogOptions{}, &hooks{
		beforeSessionCancel: func(string) {
			close(atCancel)
			<-letGo
		},
	})
	turn := r.s.script(held())
	r.submit("one")
	await(t, turn.opened, "the turn to open")
	r.queue("two")
	r.queue("three")

	stopErr := make(chan error, 1)
	go func() { stopErr <- r.e.Stop(context.Background(), Command{}) }()
	await(t, atCancel, "Stop to reach the session's cancel with its queue already cleared")

	closed := make(chan error, 1)
	go func() { closed <- r.e.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("close: %v", err)
		}
	case <-time.After(watchdog):
		t.Fatal("Close is waiting on Stop's cancel, still parked before it reaches the session")
	}

	close(letGo)
	select {
	case err := <-stopErr:
		if err != nil {
			t.Fatalf("stop: %v", err)
		}
	case <-time.After(watchdog):
		t.Fatal("Stop never returned once its cancel was let through after Close")
	}

	got := r.committed()
	assertTurnRecordSelfConsistent(t, got)
	trec := turnRecord(got)
	want := []string{`started turn-1 submit "one"`, `ended turn-1 stop="closing" next="" pending=0 synthetic`}
	if strings.Join(trec, " | ") != strings.Join(want, " | ") {
		t.Fatalf("the turn's record is\n  %s\nwant\n  %s", strings.Join(trec, "\n  "), strings.Join(want, "\n  "))
	}
	// Stop's own queue clear ran before Close ever saw the engine, so both rows
	// are gone from it and nothing about them is said again by Close (Close is
	// not Stop, and Pending on the closing ending is 0 because the queue was
	// already empty by the time Close read it).
	removed := 0
	for _, ev := range got {
		if ev.Type == agent.EventQueue && ev.QueueChange == agent.QueueRemoved {
			removed++
		}
	}
	if removed != 2 {
		t.Fatalf("%d rows removed by Stop's own clear, want 2: %s", removed, describe(got))
	}
	r.wantRows()
}

// TestCloseRightAfterSettlementEndsTheSuccessorOnce is r32's first "Untested
// schedule": "Settlement reserves and enqueues a successor, then Close ends
// that successor before run(next) launches it." turnReturned fires on turn-1's
// own goroutine strictly after settleLocked has claimed turn-2 as the
// successor and enqueued its started — but this hook itself runs after
// e.run(next) has already made the `go e.runTurn(l)` call that launches
// turn-2's goroutine (engine.go's runTurn, the e.run(next) line), so by the
// time Close is called from here turn-2's continuation may already have been
// scheduled and entered (its script is held, so it can get no further; this
// test does not, and cannot, force it to still be un-started). What this
// schedule does force is a Close that lands on a successor which is current
// and whose started is already in the record, and it proves Close authors
// that successor's own closing ending rather than leaving it open or
// double-ending turn-1.
func TestCloseRightAfterSettlementEndsTheSuccessorOnce(t *testing.T) {
	var r *rig
	closed := make(chan error, 1)
	r = newRigHooked(t, Options{}, agent.EventLogOptions{}, &hooks{
		turnReturned: func(id string) {
			if id == "turn-1" {
				closed <- r.e.Close()
			}
		},
	})
	turnA := r.s.script(held())
	// turn-2's script is held too, and this test never releases it: nothing but
	// Close's own session-close (which fires s.done) can ever move it past its
	// own opening, so whatever Close does to it while it is current and
	// un-run is exactly what this test is about.
	r.s.script(held())

	r.submit("one")
	await(t, turnA.opened, "turn-1 to open")
	r.queue("two")
	turnA.release()

	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("close from the turnReturned hook: %v", err)
		}
	case <-time.After(watchdog):
		t.Fatal("Close from the hook is waiting on something")
	}

	got := r.committed()
	assertTurnRecordSelfConsistent(t, got)
	trec := turnRecord(got)
	want := []string{
		`started turn-1 submit "one"`,
		`ended turn-1 stop="end_turn" next="turn-2" pending=0`,
		`started turn-2 drain "two"`,
		`ended turn-2 stop="closing" next="" pending=0 synthetic`,
	}
	if strings.Join(trec, " | ") != strings.Join(want, " | ") {
		t.Fatalf("the turn's record is\n  %s\nwant\n  %s", strings.Join(trec, "\n  "), strings.Join(want, "\n  "))
	}
	if st := r.e.State(); st.Turn != "" || st.Activity != ActivityClosing {
		t.Fatalf("state after closing on a reserved-but-unrun successor: %+v", st)
	}
}

// TestCloseRacingSubmits: a claim is counted in the section that makes it, under
// the lock Close takes to refuse admission, so however a Close lands among
// submits every continuation that was claimed is run and joined, and none is
// claimed afterwards. Under -race this is also the WaitGroup's own rule: its
// count is never raised beside a Wait.
func TestCloseRacingSubmits(t *testing.T) {
	for i := 0; i < 20; i++ {
		s := newFake(t, agent.EventLogOptions{NoPrimary: true})
		e, err := New(s, Options{})
		if err != nil {
			t.Fatal(err)
		}
		if err := e.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		begin := make(chan struct{})
		for c := 0; c < 4; c++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-begin
				for n := 0; n < 8; n++ {
					if _, err := e.Submit(Command{}, "x", SubmitQueue, ""); err != nil && !errors.Is(err, ErrNotAccepting) {
						t.Errorf("submit: %v", err)
					}
				}
			}()
		}
		close(begin)
		if err := e.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
		wg.Wait()
	}
}

// TestAPanicInBeginLeavesTheEngineAsItFoundIt: a test double panics in Begin.
// Submit holds e.mu through a deferred unlock and claims before it changes
// anything, so the panic reaches the caller, no lock is left held, and no turn
// is left current.
//
// A submit that names a queued row is the same claim with a row behind it, and
// the row is the reason the order matters: taken before the claim it would be
// gone from a queue whose event stream still showed it waiting — a row nobody
// could ever send or cancel again. It is read and validated first, taken only
// once Begin has been through.
func TestAPanicInBeginLeavesTheEngineAsItFoundIt(t *testing.T) {
	for _, tc := range []struct{ name, fromRow string }{
		{name: "a draft"},
		{name: "a queued row", fromRow: "the row"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, Options{})
			from := ""
			if tc.fromRow != "" {
				from = r.queue(tc.fromRow).ID
			}
			r.s.mu.Lock()
			r.s.beginPanic = true
			r.s.mu.Unlock()
			func() {
				defer func() {
					if recover() == nil {
						t.Fatal("the panic did not reach Submit's caller")
					}
				}()
				_, _ = r.e.Submit(Command{}, "boom", SubmitQueue, from)
			}()
			r.s.mu.Lock()
			r.s.beginPanic = false
			r.s.mu.Unlock()
			st := r.e.State()
			if st.Activity != ActivityIdle || st.Turn != "" || st.Prompted {
				t.Fatalf("state after a panicking Begin: %+v", st)
			}
			if tc.fromRow != "" {
				if len(st.Queue) != 1 || st.Queue[0].ID != from {
					t.Fatalf("the row the panicking claim was for: %+v", st.Queue)
				}
			}
			// The engine is as it was, so the send goes through — as the turn
			// whose id the panicking claim did not spend.
			r.submit("fine")
			got := r.until(started(""))
			if last := got[len(got)-1].Turn; last.ID != "turn-1" || last.Text != "fine" {
				t.Fatalf("the panicking claim spent a turn id: %+v", last)
			}
			r.until(lastEnding)
		})
	}
}

// TestAPanicInAClaimTheSettlementMakesLeavesItsRowQueued is the same order at
// the other claim: the successor a settlement starts. A panic there takes the
// engine's goroutine with it, and the row must still be in the queue the panic
// left behind — recovered or not, the queue and the stream that describes it
// cannot disagree.
func TestAPanicInAClaimTheSettlementMakesLeavesItsRowQueued(t *testing.T) {
	r := newRig(t, Options{})
	turn := r.s.script(held())
	r.submit("one")
	await(t, turn.opened, "the turn to open")
	row := r.queue("the successor")
	r.sync()
	// The settlement claims the successor on the turn's own goroutine. Recovering
	// there is not the engine's business — a panicking session is craze's bug —
	// so the schedule is driven through drainLocked instead, which any wake-up
	// runs: the same claim, in the same order, with the panic in the test's own
	// stack.
	r.s.mu.Lock()
	r.s.beginPanic = true
	r.s.mu.Unlock()
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("the panic did not reach the caller")
			}
		}()
		r.e.mu.Lock()
		defer r.e.mu.Unlock()
		// Nothing is current and the queue has a head: exactly the state the
		// drain runs in, straight after a settlement.
		r.e.cur, r.e.activity = nil, ActivityIdle
		_ = r.e.drainLocked()
	}()
	r.s.mu.Lock()
	r.s.beginPanic = false
	r.s.mu.Unlock()
	if st := r.e.State(); len(st.Queue) != 1 || st.Queue[0].ID != row.ID {
		t.Fatalf("the row the panicking drain was for: %+v", st.Queue)
	}
	turn.release()
}

// TestASaturatedOutboxRefusesAdmissionsAndStillCompletesTurns: with the outbox
// over its bound a rejectable command is refused having changed nothing, and a
// mandatory completion — the running turn's settlement — is enqueued all the
// same, because a turn that has ended has ended.
//
// The settlement is asserted on the engine's own state while the outbox is still
// provably undeliverable, before anything reads an event: a reader started
// earlier would let the backlog through and leave the test proving only that a
// turn settles once the log is healthy again. The turn is silent for the same
// reason — a turn that published anything would wait for the publishing boundary
// the parked drainer holds, and never come back at all.
func TestASaturatedOutboxRefusesAdmissionsAndStillCompletesTurns(t *testing.T) {
	s, e, returned := saturableEngine(t)
	turn := s.script(silently(held()))
	if _, err := e.Submit(Command{}, "one", SubmitQueue, ""); err != nil {
		t.Fatal(err)
	}
	await(t, turn.opened, "the turn to open")
	saturate(t, e)

	before := e.State()
	if _, err := e.Submit(Command{}, "two", SubmitQueue, ""); !errors.Is(err, ErrUnavailable) || Code(err) != "unavailable" {
		t.Fatalf("a submit with no room in the outbox: %v", err)
	}
	if after := e.State(); len(after.Queue) != len(before.Queue) || after.Turn != before.Turn {
		t.Fatalf("a refused submit changed the engine: %+v → %+v", before, after)
	}

	turn.release()
	awaitTurn(t, returned, "turn-1")
	if e.log.OutboxRoom() {
		t.Fatal("the outbox came back under its bound before the settlement was checked")
	}
	if st := e.State(); st.Turn != "" || st.Activity != ActivityIdle {
		t.Fatalf("the turn did not settle under a saturated outbox: %+v", st)
	}

	// And it is in the record, once somebody reads.
	r := readerOn(t, s, e)
	got := r.until(lastEnding)
	if last := got[len(got)-1].Turn; last.ID != "turn-1" || last.StopReason != stopEndTurn {
		t.Fatalf("the settlement under a saturated outbox: %+v", last)
	}
}

// TestSyncDeliversEverythingEnqueuedSoFar: a caller that has to print trailing
// events runs Sync on a helper goroutine while it keeps reading, and when Sync
// returns every engine event caused so far is in the primary's buffer.
func TestSyncDeliversEverythingEnqueuedSoFar(t *testing.T) {
	s := newFake(t, agent.EventLogOptions{})
	e, err := New(s, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	if err := e.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	turn := s.script(held())
	var got []agent.Event
	read := func(until func(agent.Event) bool) {
		t.Helper()
		for {
			select {
			case ev := <-e.Events():
				got = append(got, ev)
				if until(ev) {
					return
				}
			case <-time.After(watchdog):
				t.Fatalf("no event: %s", describe(got))
			}
		}
	}
	if _, err := e.Submit(Command{}, "one", SubmitQueue, ""); err != nil {
		t.Fatal(err)
	}
	read(started("turn-1"))
	await(t, turn.opened, "the turn to open")
	for _, text := range []string{"two", "three"} {
		if _, err := e.Submit(Command{}, text, SubmitQueue, ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.Stop(context.Background(), Command{}); err != nil {
		t.Fatalf("stop: %v", err)
	}
	synced := make(chan error, 1)
	go func() { synced <- e.Sync(context.Background()) }()
	// Keep reading until Sync says everything is delivered, then sweep what is
	// buffered without waiting: exactly `craze prompt`'s final sweep.
	for done := false; !done; {
		select {
		case ev := <-e.Events():
			got = append(got, ev)
		case err := <-synced:
			if err != nil {
				t.Fatalf("sync: %v", err)
			}
			done = true
		case <-time.After(watchdog):
			t.Fatalf("Sync never returned: %s", describe(got))
		}
	}
	for swept := false; !swept; {
		select {
		case ev := <-e.Events():
			got = append(got, ev)
		default:
			swept = true
		}
	}
	removed := 0
	for _, ev := range got {
		if ev.Type == agent.EventQueue && ev.QueueChange == agent.QueueRemoved {
			removed++
		}
	}
	if removed != 2 {
		t.Fatalf("%d removals in the sweep after Sync, want both rows: %s", removed, describe(got))
	}
}

// TestTwoClientsSubmittingAtOnceStartEveryRowOnce: admission, removal-plus-claim
// and the successor decision are one critical section in one engine, so however
// two clients interleave, every prompt starts exactly one turn and the turns run
// one at a time.
func TestTwoClientsSubmittingAtOnceStartEveryRowOnce(t *testing.T) {
	r := newRig(t, Options{})
	const each = 12
	var wg sync.WaitGroup
	for c := 0; c < 2; c++ {
		client := r.e.NewClientID()
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 1; i <= each; i++ {
				cmd := Command{Client: client, ID: fmt.Sprint(i)}
				if _, err := r.e.Submit(cmd, fmt.Sprintf("%s-%d", client, i), SubmitQueue, ""); err != nil {
					t.Errorf("submit: %v", err)
				}
			}
		}()
	}
	wg.Wait()
	startedTexts := map[string]int{}
	open := ""
	ends := 0
	for ends < 2*each {
		ev := r.next()
		if ev.Type != agent.EventTurn {
			continue
		}
		switch ev.Turn.Phase {
		case agent.TurnStarted:
			if open != "" {
				t.Fatalf("%s started while %s was still running: %s", ev.Turn.ID, open, describe(r.seen))
			}
			open = ev.Turn.ID
			startedTexts[ev.Turn.Text]++
		case agent.TurnEnded:
			if ev.Turn.ID != open {
				t.Fatalf("%s ended while %s was the running turn", ev.Turn.ID, open)
			}
			open = ""
			ends++
		}
	}
	if len(startedTexts) != 2*each {
		t.Fatalf("%d distinct prompts started, want %d", len(startedTexts), 2*each)
	}
	for text, n := range startedTexts {
		if n != 1 {
			t.Fatalf("%q started %d turns", text, n)
		}
	}
	// The session was handed each of them exactly once, too.
	if got := r.s.prompts(); len(got) != 2*each {
		t.Fatalf("the session was handed %d prompts, want %d", len(got), 2*each)
	}
}

// TestASubmitNamesItsCause: a client's own started carries its command, which
// is informative — what a client skips is the turn id Submit handed it — and a
// drained row's started carries none, because nobody's command started it.
func TestASubmitNamesItsCause(t *testing.T) {
	r := newRig(t, Options{})
	turn := r.s.script(held())
	cmd := Command{Client: r.e.NewClientID(), ID: "1"}
	if _, err := r.e.Submit(cmd, "one", SubmitQueue, ""); err != nil {
		t.Fatal(err)
	}
	await(t, turn.opened, "the turn to open")
	if _, err := r.e.Submit(Command{Client: cmd.Client, ID: "2"}, "two", SubmitQueue, ""); err != nil {
		t.Fatal(err)
	}
	r.sync()
	turn.release()
	causes := map[string]string{}
	for _, ev := range r.until(lastEnding) {
		switch ev.Type {
		case agent.EventTurn:
			causes[ev.Turn.Phase+" "+ev.Turn.ID] = ev.Cause
		case agent.EventQueue:
			causes["queue "+string(ev.QueueChange)] = ev.Cause
		}
	}
	want := map[string]string{
		"started turn-1": "c-1/1", "ended turn-1": "c-1/1",
		"queue queued": "c-1/2", "queue sent": "",
		"started turn-2": "", "ended turn-2": "",
	}
	for k, v := range want {
		if causes[k] != v {
			t.Fatalf("%s has cause %q, want %q (all: %v)", k, causes[k], v, causes)
		}
	}
}
