package engine

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
)

// modeSetting is the setting these tests drive: one kind is enough to hold the
// worker's own properties, and the three kinds' own paths are the session's
// (internal/agent's settings tests).
func modeSetting(id string) Setting { return Setting{Kind: SettingMode, Value: id} }

// TestSetAnswersWithTheValueAndItsRevision: a change the provider took comes
// back with the value and the Seq of the delta the session enqueued for it —
// the revision a client compares its delayed replies against (plan 021 §3.8) —
// and that delta is in the record, carrying the section, the cause, and
// **neither Event.Mode nor Event.Text**, because a craze-initiated change fills
// those for nobody (correction 20, A20).
func TestSetAnswersWithTheValueAndItsRevision(t *testing.T) {
	r := newRig(t, Options{})
	res, err := r.e.Set(context.Background(), Command{Client: r.e.NewClientID(), ID: "4"}, modeSetting("plan"))
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	if res.Value != "plan" || res.Rev == 0 {
		t.Fatalf("Set answered %+v, want the value and a revision", res)
	}
	ev := r.next()
	if ev.Type != agent.EventMeta || ev.Seq != res.Rev {
		t.Fatalf("the delta is %s at seq %d, want a meta at %d", ev.Type, ev.Seq, res.Rev)
	}
	if ev.Mode != "" || ev.Text != "" {
		t.Fatalf("a craze-initiated delta filled Mode %q / Text %q", ev.Mode, ev.Text)
	}
	if ev.Cause != "c-1/4" {
		t.Fatalf("the delta's cause is %q", ev.Cause)
	}
	if ev.State == nil || ev.State.Mode == nil || *ev.State.Mode != "plan" {
		t.Fatalf("the delta carries %+v", ev.State)
	}
	if got := r.e.State().CurrentMode; got != "plan" {
		t.Fatalf("the snapshot says %q", got)
	}
}

// TestSetIsServedOneAtATimeInArrivalOrder is the FIFO worker: a second Set made
// while the first is still at the provider waits for it, and the two are
// applied in the order they arrived, never interleaved. The barriers are the
// session's own — one Set is held at its provider call, and the other cannot
// have reached the provider until the first is let go.
func TestSetIsServedOneAtATimeInArrivalOrder(t *testing.T) {
	r := newRig(t, Options{})
	release := r.s.holdNextSets()
	first := make(chan SetResult, 1)
	go func() {
		res, err := r.e.Set(context.Background(), Command{}, modeSetting("first"))
		if err != nil {
			t.Errorf("first set: %v", err)
		}
		first <- res
	}()
	// The first has the worker: it is at the provider, holding the hold.
	waitFor(t, func() bool { return r.s.setCalls() == 0 && r.heldSets() })

	second := make(chan SetResult, 1)
	go func() {
		res, err := r.e.Set(context.Background(), Command{}, modeSetting("second"))
		if err != nil {
			t.Errorf("second set: %v", err)
		}
		second <- res
	}()
	// The second is in the queue, and nothing it does can reach the provider
	// while the first is held: one worker, one at a time. Being queued is the
	// barrier — from there only the worker can take it, and the worker is
	// parked.
	waitFor(t, func() bool { return r.queuedSets() == 1 })
	if n := r.s.setCalls(); n != 0 {
		t.Fatalf("%d settings changed while the first was still at the provider", n)
	}
	release()

	a := awaitSet(t, first)
	b := awaitSet(t, second)
	if a.Value != "first" || b.Value != "second" {
		t.Fatalf("answers %+v and %+v", a, b)
	}
	if a.Rev >= b.Rev {
		t.Fatalf("revisions %d then %d: the second must be the later delta", a.Rev, b.Rev)
	}
	got := r.until(func(ev agent.Event) bool { return ev.Type == agent.EventMeta && ev.Seq == b.Rev })
	r.wantShapes(got, `mode "first"`, `mode "second"`)
	if got := r.e.State().CurrentMode; got != "second" {
		t.Fatalf("the snapshot ended on %q, want the last delta's value", got)
	}
}

// TestSetRefusedByTheProviderChangesNothing: a refusal is returned as it came,
// with nothing mutated and no event at all.
func TestSetRefusedByTheProviderChangesNothing(t *testing.T) {
	r := newRig(t, Options{})
	boom := errors.New("the agent refused")
	r.s.failSets(boom)
	res, err := r.e.Set(context.Background(), Command{}, modeSetting("plan"))
	if !errors.Is(err, boom) {
		t.Fatalf("a refused Set = %+v, %v", res, err)
	}
	if res.Rev != 0 || res.Value != "" {
		t.Fatalf("a refused Set answered %+v", res)
	}
	r.s.failSets(nil)
	// The next one that works is the first event there is: the refusal said
	// nothing.
	if _, err := r.e.Set(context.Background(), Command{}, modeSetting("agent")); err != nil {
		t.Fatal(err)
	}
	if ev := r.next(); ev.State == nil || ev.State.Mode == nil || *ev.State.Mode != "agent" {
		t.Fatalf("the first event in the record is %s", shape(ev))
	}
}

// TestSetRefusesWhatTheEngineDoesNotAdmit is the gate (05's table, as C10 fills
// the session.set row in): nothing before the session is up, nothing after Stop
// or Close, and a malformed setting is bad_request. A turn working is NOT a
// refusal — craze has always allowed a mode click mid-turn.
func TestSetRefusesWhatTheEngineDoesNotAdmit(t *testing.T) {
	t.Run("before the session is up", func(t *testing.T) {
		s := newFake(t, agent.EventLogOptions{NoPrimary: true})
		e, err := New(s, Options{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = e.Close() })
		if _, err := e.Set(context.Background(), Command{}, modeSetting("plan")); !errors.Is(err, ErrNotAccepting) {
			t.Fatalf("Set before Start: %v", err)
		}
		if err := e.SetTitle(Command{}, "early"); !errors.Is(err, ErrNotAccepting) {
			t.Fatalf("SetTitle before Start: %v", err)
		}
	})
	t.Run("while a turn works", func(t *testing.T) {
		r := newRig(t, Options{})
		turn := r.s.script(held())
		r.submit("one")
		await(t, turn.opened, "the turn to open")
		if _, err := r.e.Set(context.Background(), Command{}, modeSetting("plan")); err != nil {
			t.Fatalf("Set during a turn: %v", err)
		}
		if err := r.e.SetTitle(Command{}, "named"); err != nil {
			t.Fatalf("SetTitle during a turn: %v", err)
		}
		turn.release()
	})
	t.Run("after Stop", func(t *testing.T) {
		r := newRig(t, Options{})
		if err := r.e.Stop(context.Background(), Command{}); err != nil {
			t.Fatal(err)
		}
		if _, err := r.e.Set(context.Background(), Command{}, modeSetting("plan")); !errors.Is(err, ErrNotAccepting) {
			t.Fatalf("Set after Stop: %v", err)
		}
		if err := r.e.SetTitle(Command{}, "late"); !errors.Is(err, ErrNotAccepting) {
			t.Fatalf("SetTitle after Stop: %v", err)
		}
	})
	t.Run("after Close", func(t *testing.T) {
		r := newRig(t, Options{})
		if err := r.e.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := r.e.Set(context.Background(), Command{}, modeSetting("plan")); !errors.Is(err, ErrNotAccepting) {
			t.Fatalf("Set after Close: %v", err)
		}
		if err := r.e.SetTitle(Command{}, "late"); !errors.Is(err, ErrNotAccepting) {
			t.Fatalf("SetTitle after Close: %v", err)
		}
	})
	t.Run("a malformed setting", func(t *testing.T) {
		r := newRig(t, Options{})
		for what, s := range map[string]Setting{
			"no kind":             {Value: "x"},
			"config with no id":   {Kind: SettingConfig, Value: "x"},
			"a mode with an id":   {Kind: SettingMode, ID: "effort", Value: "x"},
			"an unknown kind":     {Kind: "colour", Value: "x"},
			"a model with an id":  {Kind: SettingModel, ID: "effort", Value: "x"},
			"an unknown kind (2)": {Kind: "theme"},
		} {
			if _, err := r.e.Set(context.Background(), Command{}, s); !errors.Is(err, ErrBadRequest) || Code(err) != "bad_request" {
				t.Fatalf("%s: %v (%s)", what, err, Code(err))
			}
		}
		if n := r.s.setCalls(); n != 0 {
			t.Fatalf("a malformed setting reached the provider %d times", n)
		}
	})
}

// TestSetWhoseContextEndsInTheQueueChangesNothing: a Set that gives up while it
// is still waiting its turn returns the context's error and never reaches the
// provider; the one ahead of it is unaffected.
func TestSetWhoseContextEndsInTheQueueChangesNothing(t *testing.T) {
	r := newRig(t, Options{})
	release := r.s.holdNextSets()
	first := make(chan SetResult, 1)
	go func() {
		res, err := r.e.Set(context.Background(), Command{}, modeSetting("first"))
		if err != nil {
			t.Errorf("first set: %v", err)
		}
		first <- res
	}()
	waitFor(t, func() bool { return r.heldSets() })

	ctx, cancel := context.WithCancel(context.Background())
	queued := make(chan error, 1)
	go func() {
		_, err := r.e.Set(ctx, Command{}, modeSetting("never"))
		queued <- err
	}()
	// It is in the queue behind the held one; ending its context takes it out.
	waitFor(t, func() bool { return r.queuedSets() == 1 })
	cancel()
	select {
	case err := <-queued:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("a Set whose context ended in the queue: %v", err)
		}
	case <-time.After(watchdog):
		t.Fatal("a Set whose context ended is still waiting")
	}
	release()
	if got := awaitSet(t, first); got.Value != "first" {
		t.Fatalf("the Set ahead of it answered %+v", got)
	}
	if n := r.s.setCalls(); n != 1 {
		t.Fatalf("%d settings reached the provider, want the one that did not give up", n)
	}
	if got := r.e.State().CurrentMode; got != "first" {
		t.Fatalf("the snapshot says %q", got)
	}
}

// TestASetCancelledWhileQueuedIsAnsweredByTheWorkerWithoutRunning is r23
// finding 1, forced rather than sampled. The schedule the test above cannot
// reach is the one where the WORKER, not the caller, finds the dead context:
//
//	A is at the provider, holding the worker.
//	B is queued behind it; B's caller's context ends.
//	B's caller is parked (beforeDropSet) BEFORE it can dequeue itself.
//	A is released; the worker comes back for the next request and takes B.
//
// Before the fix neither takeSet nor runSet looked at B's context: the worker
// asked the provider for a change whose caller had already given up, and B was
// told "cancelled" while its change was being made. Now the claim and the check
// are one locked section, so the provider never sees B at all.
func TestASetCancelledWhileQueuedIsAnsweredByTheWorkerWithoutRunning(t *testing.T) {
	r := newRig(t, Options{})
	parked, held := make(chan struct{}), make(chan struct{})
	var once sync.Once
	// Set before any Set is made: the field is read on the caller's goroutine
	// and written here, before that goroutine exists.
	r.e.beforeDropSet = func() {
		once.Do(func() { close(parked) })
		<-held
	}
	release := r.s.holdNextSets()

	first := make(chan SetResult, 1)
	go func() {
		res, err := r.e.Set(context.Background(), Command{}, modeSetting("first"))
		if err != nil {
			t.Errorf("first set: %v", err)
		}
		first <- res
	}()
	waitFor(t, func() bool { return r.heldSets() })

	ctx, cancel := context.WithCancel(context.Background())
	queued := make(chan error, 1)
	go func() {
		_, err := r.e.Set(ctx, Command{}, modeSetting("never"))
		queued <- err
	}()
	waitFor(t, func() bool { return r.queuedSets() == 1 })
	cancel()
	await(t, parked, "the cancelled caller to reach its barrier")

	// Its caller is parked, so from here only the worker can take it out of the
	// queue — which is the point of the barrier.
	release()
	waitFor(t, func() bool { return r.queuedSets() == 0 })
	select {
	case err := <-queued:
		t.Fatalf("the cancelled Set answered itself: %v", err)
	default:
	}
	close(held)
	select {
	case err := <-queued:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("a Set claimed with a dead context: %v", err)
		}
	case <-time.After(watchdog):
		t.Fatal("the worker never answered the request it claimed")
	}
	if got := awaitSet(t, first); got.Value != "first" {
		t.Fatalf("the Set ahead of it answered %+v", got)
	}
	if n := r.s.setCalls(); n != 1 {
		t.Fatalf("%d settings reached the provider, want only the one whose caller was still waiting", n)
	}
	if got := r.e.State().CurrentMode; got != "first" {
		t.Fatalf("the snapshot says %q", got)
	}
}

// TestACancelledSetRunsOnAResend is r28 finding 1, over r23 finding 1's own
// schedule: a Set whose context is already dead is answered with that
// context's error, wrapped in errNotRun, WITHOUT reaching the provider — by
// the caller or by the worker, whichever gets to it (settings.go's dead-ctx
// branches) — and NOTHING RAN, so the id is exactly as unseen as before the
// attempt (classify, control.go).
//
// So a resend of the same id with a live context is a genuine FIRST attempt —
// it runs, and its answer is the change it actually made — never a replay of
// the cancellation, and never a second execution of anything: the first
// attempt executed nothing at all. Before r28's fix this was stored like an
// ordinary refusal and every resend replayed the plain cancellation for ever,
// which this test's old name and body asserted; the fix is the exact
// opposite, and the rename says so.
func TestACancelledSetRunsOnAResend(t *testing.T) {
	r := newRig(t, Options{})
	c := Command{Client: r.e.NewClientID(), ID: "7"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.e.Set(ctx, c, modeSetting("plan")); !errors.Is(err, context.Canceled) || Code(err) != "unavailable" {
		t.Fatalf("a Set made with a dead context: %v (%s), want context.Canceled coded unavailable", err, Code(err))
	}
	if n := r.s.setCalls(); n != 0 {
		t.Fatalf("%d settings reached the provider for a cancelled command", n)
	}
	// Nothing ran, so the id was forgotten: this resend is a genuine attempt,
	// not a replay.
	res, err := r.e.Set(context.Background(), c, modeSetting("plan"))
	if err != nil {
		t.Fatalf("the resend with a live context: %v, want it to run", err)
	}
	if res.Rev == 0 || res.Value != "plan" {
		t.Fatalf("the resend answered %+v, want the change it actually made", res)
	}
	if n := r.s.setCalls(); n != 1 {
		t.Fatalf("%d settings reached the provider, want exactly the resend's", n)
	}
	if got := r.e.State().CurrentMode; got != "plan" {
		t.Fatalf("the resend's change did not land: %q", got)
	}
}

// TestAClaimedSetWhoseContextEndsSaysTheOutcomeIsUnknown is r25 finding 2: a
// Set the worker has CLAIMED is bounded by its caller's context again, and
// honestly.
//
// The schedule is the reviewer's: the worker claims a request and blocks inside
// the provider — a real one blocks in the ACP encoder, where the request has
// already been written and the context cannot yet be looked at — and the
// caller's own deadline expires. Before this fix the caller waited
// unconditionally on the worker's reply, so the TUI's 15-second bound on a mode
// change bought nothing at all and modeInFlight could be set for ever.
//
// The caller now returns ErrSetOutcomeUnknown, which wraps its context's error,
// and that is the whole of what is true: the provider has the change or is
// about to. Everything else carries on exactly as it would have — the worker
// finishes, the change's own delta is published, the next queued Set runs after
// it, and the receipt keeps the answer the caller was actually given.
func TestAClaimedSetWhoseContextEndsSaysTheOutcomeIsUnknown(t *testing.T) {
	t.Run("the caller returns, the worker carries on", func(t *testing.T) {
		r := newRig(t, Options{})
		release := r.s.holdNextSetsIgnoringCtx()
		// Only so a failing run cannot strand the worker.
		t.Cleanup(release)

		ctx, cancel := context.WithCancel(context.Background())
		c := Command{Client: r.e.NewClientID(), ID: "3"}
		claimed := make(chan error, 1)
		go func() {
			_, err := r.e.Set(ctx, c, modeSetting("plan"))
			claimed <- err
		}()
		waitFor(t, func() bool { return r.heldSets() })

		// A second Set, queued behind the claimed one: FIFO has to survive the
		// first caller walking away.
		second := make(chan SetResult, 1)
		go func() {
			res, err := r.e.Set(context.Background(), Command{}, modeSetting("agent"))
			if err != nil {
				t.Errorf("the queued set: %v", err)
			}
			second <- res
		}()
		waitFor(t, func() bool { return r.queuedSets() == 1 })

		cancel()
		err := awaitErr(t, claimed, "the claimed Set")
		if !errors.Is(err, ErrSetOutcomeUnknown) {
			t.Fatalf("a claimed Set whose context ended: %v", err)
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("the sentinel dropped its context's error: %v", err)
		}
		if got := Code(err); got != "aborted" {
			t.Fatalf("the code is %q, want \"aborted\": a STORED answer whose outcome the engine cannot vouch for, same as ErrCommandAborted's (r26 finding 1)", got)
		}

		// The receipt keeps the answer the caller was GIVEN, not the success the
		// worker is about to have: a resend of the same id and payload replays
		// "the outcome is unknown" rather than a revision the caller never saw,
		// and never runs the change a second time.
		res, rerr := r.e.Set(context.Background(), c, modeSetting("plan"))
		if !errors.Is(rerr, ErrSetOutcomeUnknown) || res.Rev != 0 || res.Value != "" {
			t.Fatalf("the resend answered %+v, %v", res, rerr)
		}

		// The worker was never told to stop, and the change lands.
		release()
		if got := awaitSet(t, second); got.Value != "agent" {
			t.Fatalf("the queued Set answered %+v", got)
		}
		got := r.until(func(ev agent.Event) bool {
			return ev.Type == agent.EventMeta && ev.State != nil && ev.State.Mode != nil && *ev.State.Mode == "agent"
		})
		r.wantShapes(got, `mode "plan"`, `mode "agent"`)
		if got := r.e.State().CurrentMode; got != "agent" {
			t.Fatalf("the snapshot ended on %q, want the last delta's value", got)
		}
		if n := r.s.setCalls(); n != 2 {
			t.Fatalf("%d settings reached the provider, want the abandoned one and the queued one", n)
		}
	})
	t.Run("Close joins a worker whose provider ignores every context", func(t *testing.T) {
		r := newRig(t, Options{})
		release := r.s.holdNextSetsIgnoringCtx()
		t.Cleanup(release)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		claimed := make(chan error, 1)
		go func() {
			_, err := r.e.Set(ctx, Command{}, modeSetting("plan"))
			claimed <- err
		}()
		waitFor(t, func() bool { return r.heldSets() })
		cancel()
		if err := awaitErr(t, claimed, "the claimed Set"); !errors.Is(err, ErrSetOutcomeUnknown) {
			t.Fatalf("a claimed Set whose context ended: %v", err)
		}
		// Nothing releases the hold by hand: the SESSION closing is what frees a
		// provider call that ignores its context, and that is what Engine.Close
		// relies on before it joins the worker (live's Close tears the ACP
		// client down, which fails any write still blocked in the encoder).
		closed := make(chan error, 1)
		go func() { closed <- r.e.Close() }()
		select {
		case err := <-closed:
			if err != nil {
				t.Fatalf("close: %v", err)
			}
		case <-time.After(watchdog):
			t.Fatal("Close is waiting on a settings worker its own close should have freed")
		}
	})
}

// TestASetRefusedAfterTheDequeueIsNeverClaimed is r27 finding 3. Taking a
// request out of the queue is not yet a promise that it will run: the worker
// still has to find the engine admitting and the outbox with room, and each of
// those can have changed while the request waited. Closing `claimed` at the
// dequeue made the promise anyway, so a caller whose context ended in that gap
// was told ErrSetOutcomeUnknown — and the receipts table STORED it — for a
// change the worker then refused without asking the provider anything at all.
// A resend of that id replayed "outcome unknown" for ever, where a refusal
// about the engine's door is forgotten and leaves the id retryable.
//
// Two barriers force the schedule. The worker is parked in exactly that gap;
// the caller is parked between its context ending and its own dequeue attempt,
// so the refusal is in place before the caller decides anything, and it then
// has to WAIT for the worker rather than answer itself. In every case here the
// provider is never reached and the answer is the plain refusal — never the
// sentinel.
func TestASetRefusedAfterTheDequeueIsNeverClaimed(t *testing.T) {
	t.Run("the outbox went over its bound", func(t *testing.T) {
		park, parked, release := parkTheWorkerOnce()
		waitHere, callerParked, releaseCaller := parkTheCallerOnce()
		t.Cleanup(release)
		t.Cleanup(releaseCaller)
		// A primary nobody reads, so the outbox can really be saturated, and no
		// rig subscription yet, so nothing drains it behind the test's back.
		s := newFake(t, agent.EventLogOptions{})
		e, err := newEngine(s, Options{}, &hooks{beforeRunSet: park})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = e.Close() })
		e.beforeDropSet = waitHere
		if err := e.Start(context.Background()); err != nil {
			t.Fatal(err)
		}

		ctx, cancel := context.WithCancel(context.Background())
		c := Command{Client: e.NewClientID(), ID: "1"}
		answered := make(chan error, 1)
		go func() {
			_, err := e.Set(ctx, c, modeSetting("plan"))
			answered <- err
		}()
		await(t, parked, "the worker to take the request out of the queue")
		cancel()
		await(t, callerParked, "the caller to reach its barrier with a dead context")
		saturate(t, e)
		releaseCaller()
		release()

		err = awaitErr(t, answered, "the refused Set")
		if !errors.Is(err, ErrUnavailable) || Code(err) != "unavailable" {
			t.Fatalf("a Set refused for room after its caller gave up: %v (%s)", err, Code(err))
		}
		if errors.Is(err, ErrSetOutcomeUnknown) {
			t.Fatalf("a request that never reached the provider was told its outcome is unknown: %v", err)
		}
		if n := s.setCalls(); n != 0 {
			t.Fatalf("%d settings reached the provider", n)
		}
		// The id is left exactly as unseen as it was before the attempt.
		e.receipts.mu.Lock()
		_, reserved := e.receipts.byKey[receiptKey{client: c.Client, id: 1}]
		stored := len(e.receipts.order)
		e.receipts.mu.Unlock()
		if reserved || stored != 0 {
			t.Fatalf("the refusal kept a receipt (reserved=%v, stored=%d): a resend would replay it instead of trying again", reserved, stored)
		}

		// And a resend of the SAME id, once room returns, is a genuine attempt.
		readerOn(t, s, e)
		waitFor(t, func() bool { return e.log.OutboxRoom() })
		res, err := e.Set(context.Background(), c, modeSetting("plan"))
		if err != nil {
			t.Fatalf("the resend once room returned: %v", err)
		}
		if res.Value != "plan" {
			t.Fatalf("the resend answered %+v", res)
		}
		if n := s.setCalls(); n != 1 {
			t.Fatalf("%d settings reached the provider, want the resend alone", n)
		}
	})
	t.Run("the engine was stopped", func(t *testing.T) {
		park, parked, release := parkTheWorkerOnce()
		waitHere, callerParked, releaseCaller := parkTheCallerOnce()
		t.Cleanup(release)
		t.Cleanup(releaseCaller)
		r := newRigHooked(t, Options{}, agent.EventLogOptions{NoPrimary: true}, &hooks{beforeRunSet: park})
		r.e.beforeDropSet = waitHere
		ctx, cancel := context.WithCancel(context.Background())
		answered := make(chan error, 1)
		go func() {
			_, err := r.e.Set(ctx, Command{}, modeSetting("plan"))
			answered <- err
		}()
		await(t, parked, "the worker to take the request out of the queue")
		cancel()
		await(t, callerParked, "the caller to reach its barrier with a dead context")
		if err := r.e.Stop(context.Background(), Command{}); err != nil {
			t.Fatalf("stop: %v", err)
		}
		releaseCaller()
		release()

		err := awaitErr(t, answered, "the refused Set")
		if !errors.Is(err, ErrNotAccepting) || errors.Is(err, ErrSetOutcomeUnknown) {
			t.Fatalf("a Set refused by a stopped engine after its caller gave up: %v", err)
		}
		if n := r.s.setCalls(); n != 0 {
			t.Fatalf("%d settings reached the provider", n)
		}
	})
	t.Run("the engine was closed", func(t *testing.T) {
		park, parked, release := parkTheWorkerOnce()
		waitHere, callerParked, releaseCaller := parkTheCallerOnce()
		t.Cleanup(release)
		t.Cleanup(releaseCaller)
		r := newRigHooked(t, Options{}, agent.EventLogOptions{NoPrimary: true}, &hooks{beforeRunSet: park})
		r.e.beforeDropSet = waitHere
		ctx, cancel := context.WithCancel(context.Background())
		answered := make(chan error, 1)
		go func() {
			_, err := r.e.Set(ctx, Command{}, modeSetting("plan"))
			answered <- err
		}()
		await(t, parked, "the worker to take the request out of the queue")
		cancel()
		await(t, callerParked, "the caller to reach its barrier with a dead context")
		// Close joins the worker, which is parked at the barrier: it can only be
		// made from another goroutine, and the shut door is what the worker finds
		// when the barrier lets it go.
		closed := make(chan error, 1)
		go func() { closed <- r.e.Close() }()
		waitFor(t, func() bool {
			r.e.mu.Lock()
			defer r.e.mu.Unlock()
			return r.e.closed
		})
		releaseCaller()
		release()

		err := awaitErr(t, answered, "the refused Set")
		if !errors.Is(err, ErrNotAccepting) || errors.Is(err, ErrSetOutcomeUnknown) {
			t.Fatalf("a Set refused by a closing engine after its caller gave up: %v", err)
		}
		if n := r.s.setCalls(); n != 0 {
			t.Fatalf("%d settings reached the provider", n)
		}
		if err := awaitErr(t, closed, "Close"); err != nil {
			t.Fatalf("close: %v", err)
		}
	})
	// The same-instant case, forced rather than hoped for: the caller's context
	// has ended AND the worker's answer is already in the reply channel AND the
	// request is claimed. A reply is strictly more informative than the sentinel
	// and equally true, so it wins — whichever arm of the select wakes.
	t.Run("a reply ready in the same instant is preferred", func(t *testing.T) {
		// The barrier here is the SECOND request reaching the worker: the worker
		// sends the first's answer before it takes the next one, so this is proof
		// the first's reply is buffered and its claim closed.
		second := make(chan struct{})
		taken := 0
		r := newRigHooked(t, Options{}, agent.EventLogOptions{NoPrimary: true}, &hooks{beforeRunSet: func() {
			taken++
			if taken == 2 {
				close(second)
			}
		}})
		release := r.s.holdNextSetsIgnoringCtx()
		t.Cleanup(release)
		parked, held := make(chan struct{}), make(chan struct{})
		var once sync.Once
		r.e.beforeDropSet = func() {
			once.Do(func() { close(parked) })
			<-held
		}

		ctx, cancel := context.WithCancel(context.Background())
		answered := make(chan setAnswer, 1)
		go func() {
			res, err := r.e.Set(ctx, Command{}, modeSetting("plan"))
			answered <- setAnswer{res: res, err: err}
		}()
		waitFor(t, func() bool { return r.heldSets() })
		go func() { _, _ = r.e.Set(context.Background(), Command{}, modeSetting("agent")) }()
		waitFor(t, func() bool { return r.queuedSets() == 1 })

		cancel()
		await(t, parked, "the caller to reach its barrier with a dead context")
		release()
		await(t, second, "the worker to move on to the next request")
		close(held)

		got := <-answered
		if got.err != nil {
			t.Fatalf("a claimed Set whose reply was ready: %v", got.err)
		}
		if got.res.Value != "plan" {
			t.Fatalf("the answer is %+v, want the reply the worker had already sent", got.res)
		}
		// Rev is not asserted: the flush that learns it is made on the request's
		// own context, which ended here, so this change stands with Rev 0 — the
		// documented case (runSet, TestASetWhoseRevisionCouldNotBeLearnedStillSucceeded).
		// What is being pinned is that the caller got the ANSWER and not the
		// sentinel.
	})
}

// parkTheWorkerOnce is the beforeRunSet barrier the cases above share: the
// FIRST request the settings worker takes is parked in the gap where it is
// neither queued nor claimed, and every request after it goes straight through.
// The flag needs no lock — beforeRunSet is only ever called on the worker's one
// goroutine — and release is idempotent, so a t.Cleanup cannot double-close it.
func parkTheWorkerOnce() (hook func(), parked chan struct{}, release func()) {
	parked, held := make(chan struct{}), make(chan struct{})
	first := true
	hook = func() {
		if !first {
			return
		}
		first = false
		close(parked)
		<-held
	}
	return hook, parked, sync.OnceFunc(func() { close(held) })
}

// parkTheCallerOnce is the beforeDropSet barrier beside it: a Set whose context
// has ended is parked between that context ending and its own dequeue attempt,
// so a case can put the refusal in place while the caller is provably there and
// has decided nothing yet. Release is idempotent, for the same reason.
func parkTheCallerOnce() (hook func(), parked chan struct{}, release func()) {
	parked, held := make(chan struct{}), make(chan struct{})
	var once sync.Once
	hook = func() {
		once.Do(func() { close(parked) })
		<-held
	}
	return hook, parked, sync.OnceFunc(func() { close(held) })
}

// awaitErr is one goroutine's error, with the watchdog.
func awaitErr(t *testing.T, ch <-chan error, what string) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(watchdog):
		t.Fatalf("%s never came back", what)
	}
	panic("unreachable")
}

// TestSetAnswersWithTheValueTheSessionConfirmed is r23 finding 4: the answer is
// the value the SESSION is at, captured where the change was made — not an echo
// of the request. A provider that resolves what it is sent (native turns an
// empty effort into the model's default, and a model alias into its canonical
// id) would otherwise hand a client a value contradicting the very delta the
// Rev names.
func TestSetAnswersWithTheValueTheSessionConfirmed(t *testing.T) {
	r := newRig(t, Options{})
	r.s.resolveSets(func(v string) string {
		if v == "" {
			return "high"
		}
		return strings.TrimPrefix(v, "alias-")
	})
	res, err := r.e.Set(context.Background(), Command{}, Setting{Kind: SettingConfig, ID: "effort", Value: ""})
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	if res.Value != "high" {
		t.Fatalf("Set answered %q, want the value the session resolved it to", res.Value)
	}
	ev := r.next()
	if ev.Seq != res.Rev || ev.State == nil || ev.State.Config == nil ||
		ev.State.Config.Options[0].Current != res.Value {
		t.Fatalf("the delta at %d carries %+v, want the answered value %q", ev.Seq, ev.State, res.Value)
	}
	got, err := r.e.Set(context.Background(), Command{}, Setting{Kind: SettingModel, Value: "alias-composer"})
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	if got.Value != "composer" || r.e.State().CurrentModel != "composer" {
		t.Fatalf("Set answered %+v with the snapshot on %q", got, r.e.State().CurrentModel)
	}
}

// TestASetWhoseRevisionCouldNotBeLearnedStillSucceeded is hunt A's zero-Rev
// success: the change happened, the number did not arrive — a flush that gave
// up, or a log that was closing. The result stands with Rev 0, which a client
// reads as "no revision" and never as one older than every other (the TUI's
// mayApply; internal/tui's own test for the same shape).
func TestASetWhoseRevisionCouldNotBeLearnedStillSucceeded(t *testing.T) {
	r := newRig(t, Options{})
	r.s.setsWithoutATicket()
	res, err := r.e.Set(context.Background(), Command{}, modeSetting("plan"))
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	if res.Value != "plan" || res.Rev != 0 {
		t.Fatalf("Set answered %+v, want the value with no revision", res)
	}
	if got := r.e.State().CurrentMode; got != "plan" {
		t.Fatalf("the change did not land: %q", got)
	}
	if ev := r.next(); ev.State == nil || ev.State.Mode == nil || *ev.State.Mode != "plan" {
		t.Fatalf("the delta is %s", shape(ev))
	}
}

// TestASaturatedOutboxRefusesSetAndSetTitle is A5's settings half: over the
// bound both refuse having changed nothing, and the provider is never asked —
// a refusal after the agent had taken the change would be a lie.
func TestASaturatedOutboxRefusesSetAndSetTitle(t *testing.T) {
	// A primary nobody reads at all — no turn runs here, so nothing needs the
	// one reader saturableEngine allows — which is what makes the outbox
	// undeliverable: the drainer parks on the first send it cannot make.
	s := newFake(t, agent.EventLogOptions{})
	e, err := New(s, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	if err := e.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	saturate(t, e)
	before := e.State()

	if _, err := e.Set(context.Background(), Command{}, modeSetting("plan")); !errors.Is(err, ErrUnavailable) || Code(err) != "unavailable" {
		t.Fatalf("Set with no room in the outbox: %v (%s)", err, Code(err))
	}
	err = e.SetTitle(Command{}, "renamed")
	if !errors.Is(err, agent.ErrSetUnavailable) || Code(err) != "unavailable" {
		t.Fatalf("SetTitle with no room in the outbox: %v (%s)", err, Code(err))
	}
	if n := s.setCalls(); n != 0 {
		t.Fatalf("a refused Set reached the provider %d times", n)
	}
	after := e.State()
	if after.CurrentMode != before.CurrentMode || after.Title != before.Title {
		t.Fatalf("a refused settings change moved the session: %+v → %+v", before, after)
	}
}

// TestSetTitleWaitsOnNothingWithAFullPrimary is A4's shape for the one settings
// verb a UI calls from its own Update: with the primary full and its only
// reader inside the call, SetTitle still returns — it enqueues under the
// session's lock and never publishes.
func TestSetTitleWaitsOnNothingWithAFullPrimary(t *testing.T) {
	s := newFake(t, agent.EventLogOptions{})
	e, err := New(s, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	if err := e.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Fill the primary and stop reading it: the reader is "inside the call".
	for i := 0; i < 512; i++ {
		if !s.log.TryPublish(agent.Event{Type: agent.EventText, Text: "fill"}) {
			break
		}
	}
	done := make(chan error, 1)
	title := Command{Client: e.NewClientID(), ID: "9"}
	go func() { done <- e.SetTitle(title, "renamed") }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("SetTitle with a full primary: %v", err)
		}
	case <-time.After(watchdog):
		t.Fatal("SetTitle waited for a reader that is inside the call")
	}
	if got := e.State().Title; got != "renamed" {
		t.Fatalf("the title is %q", got)
	}
}

// TestCloseJoinsTheSettingsWorker: a Set held at the provider when Close begins
// is freed by THE SESSION CLOSING — nothing here releases it by hand — its
// caller is answered, and Close returns once the worker has gone. A Set queued
// behind it is answered too, never left waiting on a worker that is about to
// exit, and one made afterwards is refused.
//
// The hold is the fake's stand-in for a provider call, and it ends on the
// session's own done channel exactly as a real one ends when its client is torn
// down (fakeSession.set). That is what Close's comment claims — "a request in
// flight is freed by that close rather than held by this loop" — and releasing
// the call by hand, as this test used to, proved only that a test can release
// something (review r23, "Untested schedules").
func TestCloseJoinsTheSettingsWorker(t *testing.T) {
	r := newRig(t, Options{})
	release := r.s.holdNextSets()
	// Only so a failing run cannot strand the goroutines; a passing one never
	// reaches it, because the close has already freed them.
	t.Cleanup(release)
	running := make(chan error, 1)
	go func() {
		_, err := r.e.Set(context.Background(), Command{}, modeSetting("plan"))
		running <- err
	}()
	waitFor(t, func() bool { return r.heldSets() })
	queued := make(chan error, 1)
	go func() {
		_, err := r.e.Set(context.Background(), Command{}, modeSetting("agent"))
		queued <- err
	}()
	waitFor(t, func() bool { return r.queuedSets() == 1 })

	closed := make(chan error, 1)
	go func() { closed <- r.e.Close() }()
	select {
	case err := <-running:
		if err == nil {
			t.Fatal("a Set freed by the session closing must say the change did not land")
		}
	case <-time.After(watchdog):
		t.Fatal("the Set at the provider was never freed by the close")
	}
	select {
	case err := <-queued:
		// Answered, which is the whole of what is owed: whether the worker got
		// to it before Close took the lock that refuses admission, or after, is
		// the scheduler's business — nil means it was served, ErrNotAccepting
		// that the queue was refused as the engine closed. What must never
		// happen is that it is left waiting on a worker that has gone.
		if err != nil && !errors.Is(err, ErrNotAccepting) {
			t.Fatalf("a Set queued when Close began: %v", err)
		}
	case <-time.After(watchdog):
		t.Fatal("a Set queued when Close began was never answered")
	}
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("close: %v", err)
		}
	case <-time.After(watchdog):
		t.Fatal("Close is waiting on the settings worker")
	}
	if _, err := r.e.Set(context.Background(), Command{}, modeSetting("plan")); !errors.Is(err, ErrNotAccepting) {
		t.Fatalf("a Set after Close: %v", err)
	}
}

// TestTwoSetsAndAProviderUpdateAgreeOnTheLastDelta is A-X6: two concurrent Set
// calls and an update the provider made itself, racing each other. Whatever
// order they land in, two budgeted subscribers and State() end on the same
// value — the last delta by Seq — because every one of them is enqueued in the
// section that mutated the snapshot (plan 021 §3.8; panel astra 14).
func TestTwoSetsAndAProviderUpdateAgreeOnTheLastDelta(t *testing.T) {
	for i := 0; i < 20; i++ {
		r := newRig(t, Options{})
		second, err := r.e.Subscribe(agent.SubscribeOptions{})
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		wg.Add(3)
		go func() { defer wg.Done(); mustSet(t, r.e, modeSetting("plan")) }()
		go func() { defer wg.Done(); mustSet(t, r.e, modeSetting("agent")) }()
		go func() { defer wg.Done(); r.s.providerMode("ask") }()
		wg.Wait()
		r.sync()

		// Three mode deltas, no more and no fewer: the two Sets and the update
		// the provider made itself. Each subscriber is read until it has all
		// three, because a commit puts a record in a subscription's buffer and
		// its owner hands it over afterwards — "delivered" is not "already in
		// the channel".
		a := lastModeOf(t, r.sub, 3)
		b := lastModeOf(t, second, 3)
		second.Close()
		if a.mode != b.mode || a.seq != b.seq {
			t.Fatalf("two subscribers folded to %+v and %+v", a, b)
		}
		if got := r.e.State().CurrentMode; got != a.mode {
			t.Fatalf("State says %q, the last delta by Seq says %q", got, a.mode)
		}
		_ = r.e.Close()
	}
}

// TestTheLastDeltaWinsInBothForcedOrders is the test above's property with the
// sampling taken out: the two orders a Set and an update the provider made
// itself can land in, each forced once by the provider hold, which parks the
// Set between "the provider took it" and the locked section that mutates and
// enqueues — the exact window a non-atomic section would lose (r23's
// "Untested schedules": forced state-order races).
func TestTheLastDeltaWinsInBothForcedOrders(t *testing.T) {
	t.Run("the agent's update lands while the Set is at the provider", func(t *testing.T) {
		r := newRig(t, Options{})
		release := r.s.holdNextSets()
		done := make(chan SetResult, 1)
		go func() {
			res, err := r.e.Set(context.Background(), Command{}, modeSetting("plan"))
			if err != nil {
				t.Errorf("set: %v", err)
			}
			done <- res
		}()
		waitFor(t, func() bool { return r.heldSets() })
		// The whole of the agent's own update — mutation, delta, flush —
		// between the provider taking the change and the session writing it.
		r.s.providerMode("ask")
		release()
		res := awaitSet(t, done)
		got := r.until(func(ev agent.Event) bool { return ev.Type == agent.EventMeta && ev.Seq == res.Rev })
		r.wantShapes(got, `agent mode "ask"`, `mode "plan"`)
		if state := r.e.State().CurrentMode; state != "plan" {
			t.Fatalf("the snapshot says %q and the last delta %q", state, "plan")
		}
	})
	t.Run("the agent's update lands after the Set has landed", func(t *testing.T) {
		r := newRig(t, Options{})
		if _, err := r.e.Set(context.Background(), Command{}, modeSetting("plan")); err != nil {
			t.Fatalf("set: %v", err)
		}
		r.s.providerMode("ask")
		r.sync()
		got := r.until(func(ev agent.Event) bool {
			return ev.Type == agent.EventMeta && ev.State != nil && ev.State.Mode != nil && *ev.State.Mode == "ask"
		})
		r.wantShapes(got, `mode "plan"`, `agent mode "ask"`)
		if state := r.e.State().CurrentMode; state != "ask" {
			t.Fatalf("the snapshot says %q and the last delta %q", state, "ask")
		}
	})
}

// modeAt is a folded mode: the value, and the Seq of the delta that set it.
type modeAt struct {
	mode string
	seq  uint64
}

// lastModeOf folds a subscription until it has seen want mode deltas, and
// answers with the last one by Seq: the value a client that folds the stream
// ends on.
func lastModeOf(t *testing.T, sub *agent.Subscription, want int) modeAt {
	t.Helper()
	var out modeAt
	for n := 0; n < want; {
		select {
		case rec, ok := <-sub.Records():
			if !ok {
				t.Fatalf("the subscription ended after %d of %d mode deltas: %v", n, want, sub.Err())
			}
			ev, err := rec.Event()
			if err != nil {
				t.Fatal(err)
			}
			if ev.State == nil || ev.State.Mode == nil {
				continue
			}
			n++
			if ev.Seq > out.seq {
				out = modeAt{mode: *ev.State.Mode, seq: ev.Seq}
			}
		case <-time.After(watchdog):
			t.Fatalf("only %d of %d mode deltas arrived in %s", n, want, watchdog)
		}
	}
	return out
}

func mustSet(t *testing.T, e *Engine, s Setting) {
	t.Helper()
	if _, err := e.Set(context.Background(), Command{}, s); err != nil {
		t.Errorf("set %+v: %v", s, err)
	}
}

func awaitSet(t *testing.T, ch <-chan SetResult) SetResult {
	t.Helper()
	select {
	case res := <-ch:
		return res
	case <-time.After(watchdog):
		t.Fatal("a Set never came back")
	}
	panic("unreachable")
}

// heldSets reports whether a settings verb is parked at the session's provider
// call, and queuedSets how many are waiting behind it.
func (r *rig) heldSets() bool {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	return r.s.setsHeld > 0
}

func (r *rig) queuedSets() int {
	r.e.mu.Lock()
	defer r.e.mu.Unlock()
	return len(r.e.sets)
}

// TestAnOptionStepBoundToAnotherModelIsRefused is panel astra 4, with two
// clients: A picks a model and then an option for it, and B's model change
// runs between A's two steps. Every Set is ordered by the FIFO, but A's
// composite choice is not — so A's option step, bound to the model it was
// chosen for (Setting.ForModel), is refused ErrStaleModel when its turn comes
// on another model, before the provider is asked, even though the option id and
// value would be accepted there too (the fake takes any id: the binding alone
// refuses it).
//
// The check is made when the step RUNS, not when it is queued: B's change is
// held at the provider while A's step joins the queue behind it, so the model
// at queue time is still A's — a check made then would have let the step
// through to a model it was never chosen for.
func TestAnOptionStepBoundToAnotherModelIsRefused(t *testing.T) {
	r := newRig(t, Options{})
	ctx := context.Background()
	a, b := r.e.NewClientID(), r.e.NewClientID()
	if _, err := r.e.Set(ctx, Command{Client: a, ID: "1"}, Setting{Kind: SettingModel, Value: "grok-4.6"}); err != nil {
		t.Fatalf("A's model: %v", err)
	}

	release := r.s.holdNextSets()
	bDone := make(chan error, 1)
	go func() {
		_, err := r.e.Set(ctx, Command{Client: b, ID: "1"}, Setting{Kind: SettingModel, Value: "composer-2.5"})
		bDone <- err
	}()
	waitFor(t, r.heldSets)
	if got := r.e.State().CurrentModel; got != "grok-4.6" {
		t.Fatalf("B's change is still at the provider and the model is %q", got)
	}
	step := Setting{Kind: SettingConfig, ID: "fast", Value: "true", ForModel: "grok-4.6"}
	aDone := make(chan error, 1)
	go func() {
		_, err := r.e.Set(ctx, Command{Client: a, ID: "2"}, step)
		aDone <- err
	}()
	waitFor(t, func() bool { return r.queuedSets() == 1 })
	before := r.s.setCalls()
	release()

	if err := awaitErr(t, bDone, "B's model"); err != nil {
		t.Fatalf("B's model: %v", err)
	}
	err := awaitErr(t, aDone, "A's option step")
	if !errors.Is(err, ErrStaleModel) || Code(err) != "stale_model" {
		t.Fatalf("A's step bound to grok-4.6, run on composer-2.5, answered %v (%s), want ErrStaleModel", err, Code(err))
	}
	if got := r.s.setCalls(); got != before+1 {
		t.Fatalf("%d settings reached the provider after the release, want B's alone", got-before)
	}
	if got := r.e.State().CurrentModel; got != "composer-2.5" {
		t.Fatalf("the session is on %q", got)
	}

	// Unbound, the same change goes through: it is the binding that refused.
	if _, err := r.e.Set(ctx, Command{Client: a, ID: "3"}, Setting{Kind: SettingConfig, ID: "fast", Value: "true"}); err != nil {
		t.Fatalf("the same step unbound: %v", err)
	}
	// Bound to the model the session is on, it goes through too.
	if _, err := r.e.Set(ctx, Command{Client: a, ID: "4"}, Setting{Kind: SettingConfig, ID: "fast", Value: "false", ForModel: "composer-2.5"}); err != nil {
		t.Fatalf("a step bound to the current model: %v", err)
	}

	// Nothing ran, so nothing was stored: once the model is back, a resend of
	// A's refused command is a genuine attempt and lands.
	if _, err := r.e.Set(ctx, Command{Client: b, ID: "2"}, Setting{Kind: SettingModel, Value: "grok-4.6"}); err != nil {
		t.Fatalf("B's model back: %v", err)
	}
	calls := r.s.setCalls()
	if _, err := r.e.Set(ctx, Command{Client: a, ID: "2"}, step); err != nil {
		t.Fatalf("A's step resent on its own model: %v", err)
	}
	if got := r.s.setCalls(); got != calls+1 {
		t.Fatal("the resend was answered from the receipts table instead of running")
	}

	// The binding is part of the request: the same id with another binding is
	// a different request, never a replay.
	if _, err := r.e.Set(ctx, Command{Client: a, ID: "2"}, Setting{Kind: SettingConfig, ID: "fast", Value: "true", ForModel: "composer-2.5"}); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("the same id rebound answered %v, want ErrBadRequest", err)
	}
	// And only an option is bound to a model.
	for _, s := range []Setting{
		{Kind: SettingModel, Value: "grok-4.6", ForModel: "grok-4.6"},
		{Kind: SettingMode, Value: "plan", ForModel: "grok-4.6"},
	} {
		if _, err := r.e.Set(ctx, Command{}, s); !errors.Is(err, ErrBadRequest) {
			t.Fatalf("a %s setting bound to a model answered %v, want ErrBadRequest", s.Kind, err)
		}
	}
}

// TestAnOptionStepTheAgentMovedAwayFromIsRefusedAtTheSession is astra r2 item
// 3 through the engine: the FIFO orders every client's Set, and not the agent
// moving its model on its own, so the binding goes down with the change and
// the session checks it again just before the write.
//
// The schedule, forced with the fake's barrier at SetConfig's entry:
//
//  1. A's step, bound to grok-4.6, is taken by the worker. The session is on
//     grok-4.6, so the worker's own check passes and the step is claimed.
//  2. Before the session's check, the agent moves the model to composer-2.5 —
//     the barrier writes it, as the read loop applying the agent's push would.
//  3. The session's check finds composer-2.5 and refuses with ErrStaleModel,
//     which it can only do because runSet handed it the binding; the provider
//     is never asked.
//
// Nothing ran, so nothing is stored, exactly as for the worker's own refusal:
// once the model is back, the same id resent is a genuine attempt and lands.
func TestAnOptionStepTheAgentMovedAwayFromIsRefusedAtTheSession(t *testing.T) {
	r := newRig(t, Options{})
	ctx := context.Background()
	a := r.e.NewClientID()
	if _, err := r.e.Set(ctx, Command{Client: a, ID: "1"}, Setting{Kind: SettingModel, Value: "grok-4.6"}); err != nil {
		t.Fatalf("the model: %v", err)
	}
	var moved atomic.Bool
	r.s.mu.Lock()
	r.s.beforeConfigCheck = func() {
		if moved.CompareAndSwap(false, true) {
			r.s.mu.Lock()
			r.s.snap.CurrentModel = "composer-2.5"
			r.s.mu.Unlock()
		}
	}
	r.s.mu.Unlock()

	step := Setting{Kind: SettingConfig, ID: "fast", Value: "true", ForModel: "grok-4.6"}
	before := r.s.setCalls()
	_, err := r.e.Set(ctx, Command{Client: a, ID: "2"}, step)
	if !moved.Load() {
		t.Fatalf("the step never reached the session (%v): the worker refused it before the agent moved", err)
	}
	if !errors.Is(err, ErrStaleModel) || !errors.Is(err, agent.ErrStaleModel) || Code(err) != "stale_model" {
		t.Fatalf("a step bound to grok-4.6, sent on composer-2.5, answered %v (%s), want ErrStaleModel", err, Code(err))
	}
	if got := r.s.setCalls(); got != before {
		t.Fatalf("%d settings reached the provider, want none", got-before)
	}

	if _, err := r.e.Set(ctx, Command{Client: a, ID: "3"}, Setting{Kind: SettingModel, Value: "grok-4.6"}); err != nil {
		t.Fatalf("the model back: %v", err)
	}
	calls := r.s.setCalls()
	if _, err := r.e.Set(ctx, Command{Client: a, ID: "2"}, step); err != nil {
		t.Fatalf("the step resent on its own model: %v", err)
	}
	if got := r.s.setCalls(); got != calls+1 {
		t.Fatal("the resend was answered from the receipts table instead of running")
	}
}

// TestAnOptionGoneIsAnnouncedBeforeItsAnswer: a Set the agent took whose
// answer no longer lists the option (agent.ErrOptionGone) still installed
// something, and its delta is committed before the caller hears the refusal —
// its ticket has its revision by then, exactly as a success's has.
func TestAnOptionGoneIsAnnouncedBeforeItsAnswer(t *testing.T) {
	r := newRig(t, Options{})
	r.s.answerGone()
	_, err := r.e.Set(context.Background(), Command{}, Setting{Kind: SettingConfig, ID: "fast", Value: "true"})
	if !errors.Is(err, agent.ErrOptionGone) || Code(err) != "failed" {
		t.Fatalf("Set answered %v (%s), want ErrOptionGone", err, Code(err))
	}
	if r.s.ticket().Seq() == 0 {
		t.Fatal("ErrOptionGone was answered before its delta was committed")
	}
	if ev := r.next(); ev.Type != agent.EventMeta || ev.State == nil || ev.State.Config == nil {
		t.Fatalf("the install was announced as %+v", ev)
	}
}

// waitFor polls a predicate about another goroutine's progress, with the
// watchdog. It is not a timing assertion: the predicate is a barrier that
// becomes true once and stays true, and nothing in a passing run waits.
func waitFor(t *testing.T, pred func() bool) {
	t.Helper()
	deadline := time.Now().Add(watchdog)
	for !pred() {
		if time.Now().After(deadline) {
			t.Fatalf("the condition never became true in %s", watchdog)
		}
		time.Sleep(time.Millisecond)
	}
}
