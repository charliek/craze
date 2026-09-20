package engine

import (
	"context"
	"errors"
	"fmt"
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
func TestAPanicInBeginLeavesTheEngineAsItFoundIt(t *testing.T) {
	r := newRig(t, Options{})
	r.s.mu.Lock()
	r.s.beginPanic = true
	r.s.mu.Unlock()
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("the panic did not reach Submit's caller")
			}
		}()
		_, _ = r.e.Submit(Command{}, "boom", SubmitQueue, "")
	}()
	r.s.mu.Lock()
	r.s.beginPanic = false
	r.s.mu.Unlock()
	if st := r.e.State(); st.Activity != ActivityIdle || st.Turn != "" || st.Prompted {
		t.Fatalf("state after a panicking Begin: %+v", st)
	}
	r.submit("fine")
	got := r.until(lastEnding)
	if first := got[0].Turn; first.ID != "turn-1" {
		t.Fatalf("the panicking claim spent a turn id: %+v", first)
	}
}

// TestASaturatedOutboxRefusesAdmissionsAndStillCompletesTurns: with the outbox
// over its bound a rejectable command is refused having changed nothing, and a
// mandatory completion — the running turn's settlement — is enqueued all the
// same, because a turn that has ended has ended.
func TestASaturatedOutboxRefusesAdmissionsAndStillCompletesTurns(t *testing.T) {
	// A primary nobody reads wedges the drainer, so what is enqueued stays
	// enqueued.
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
	go func() {
		// The turn's first continuation waits for its started to be delivered,
		// so something has to read that much.
		for ev := range e.Events() {
			if started("turn-1")(ev) {
				return
			}
		}
	}()
	if _, err := e.Submit(Command{}, "one", SubmitQueue, ""); err != nil {
		t.Fatal(err)
	}
	await(t, turn.opened, "the turn to open")

	filler := make([]agent.Event, 0, 512)
	for i := 0; i < cap(filler); i++ {
		filler = append(filler, agent.Event{Type: agent.EventText, Text: fmt.Sprintf("fill-%d", i)})
	}
	for e.log.OutboxRoom() {
		e.log.Enqueue(filler...)
	}

	before := e.State()
	if _, err := e.Submit(Command{}, "two", SubmitQueue, ""); !errors.Is(err, ErrUnavailable) || Code(err) != "unavailable" {
		t.Fatalf("a submit with no room in the outbox: %v", err)
	}
	if after := e.State(); len(after.Queue) != len(before.Queue) || after.Turn != before.Turn {
		t.Fatalf("a refused submit changed the engine: %+v → %+v", before, after)
	}

	// The turn ends. Its settlement is accepted over the bound, and reaches
	// the record once somebody reads. The reader comes first: a Subscribe waits
	// for the publishing boundary, which the drainer holds while it is parked
	// on the full primary.
	var wg sync.WaitGroup
	wg.Add(1)
	stop := make(chan struct{})
	go func() {
		defer wg.Done()
		for {
			select {
			case <-e.Events():
			case <-stop:
				return
			}
		}
	}()
	defer func() { close(stop); wg.Wait() }()
	sub, err := e.Subscribe(agent.SubscribeOptions{MaxItems: 1 << 16, MaxBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sub.Close)
	turn.release()
	r := &rig{t: t, s: s, e: e, sub: sub}
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
