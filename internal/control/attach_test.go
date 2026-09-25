package control_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/control"
	"github.com/charliek/craze/internal/protocol"
)

// TestAnAttachIsAnsweredBeforeItsStream (§3.4, §3.7): an attach with no cursor
// is answered with the session's info document, ready, a snapshot and the
// snapshot's cut as after — queued before the forwarder starts, so it is the
// first line of the attachment — and synchronized follows at once when the
// cutoff is the attach point itself; every event after it carries
// Record.Body verbatim, from after + 1 on.
func TestAnAttachIsAnsweredBeforeItsStream(t *testing.T) {
	h := newHost(t)
	h.publish(agent.Event{Type: agent.EventText, Text: "before the attach"})
	a := h.dial()
	a.sayHello(nil)
	r := a.attach(attachParams(h))
	st := h.eng.State()
	switch {
	case r.Subscription != "s-1", !r.Ready, r.Reset != "":
		t.Fatalf("the reply: %+v", r)
	case r.Session.SessionID != st.CrazeSessionID, len(r.Session.Catalogs.Models) == 0:
		t.Fatalf("the reply's info document: %+v", r.Session)
	case r.Snapshot == nil, r.After != protocol.Cursor{Incarnation: st.Incarnation, Seq: h.head()}:
		t.Fatalf("the reply's snapshot and after: %v, %+v (head %d)", r.Snapshot != nil, r.After, h.head())
	}
	if s := paramsOf[protocol.SynchronizedParams](t, a.note(protocol.NotifySynchronized)); s != (protocol.SynchronizedParams{Subscription: "s-1", Seq: r.After.Seq}) {
		t.Fatalf("synchronized: %+v, want at the attach point %d", s, r.After.Seq)
	}

	sub, err := h.eng.Subscribe(agent.SubscribeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	h.publish(agent.Event{Type: agent.EventText, Text: "a && b <c> é 😀"})
	var rec agent.Record
	select {
	case rec = <-sub.Records():
	case <-time.After(watchdog):
		t.Fatal("no record")
	}
	n := a.note(protocol.NotifyEvent)
	p := paramsOf[protocol.EventParams](t, n)
	if p.Subscription != "s-1" || p.Seq != r.After.Seq+1 || p.Seq != rec.Seq || string(p.Event) != rec.Body {
		t.Fatalf("the event %+v, want seq %d and the record's body verbatim %s", p, rec.Seq, rec.Body)
	}
}

// TestSynchronizedMarksTheCutoff (§3.4): synchronized is sent once the
// forwarder has queued the record at the subscription's cutoff — after a
// cursor replay's last record — and at once, right after the reply, when the
// cutoff is the attach point: a snapshot attach, and a cursor at the head.
func TestSynchronizedMarksTheCutoff(t *testing.T) {
	h := newHost(t)
	a := h.dial()
	a.sayHello(nil)
	r1 := a.attach(attachParams(h))
	if s := paramsOf[protocol.SynchronizedParams](t, a.note(protocol.NotifySynchronized)); s.Seq != r1.After.Seq {
		t.Fatalf("a snapshot attach's synchronized: %+v, after %d", s, r1.After.Seq)
	}
	ok[protocol.Empty](t, a.detach(h, r1.Subscription))
	for i := range 3 {
		h.publish(agent.Event{Type: agent.EventText, Text: strings.Repeat("r", i+1)})
	}
	head := h.head()
	if head != r1.After.Seq+3 {
		t.Fatalf("the premise: head %d, after %d", head, r1.After.Seq)
	}

	// A cursor replay: the three records, then synchronized at the cutoff.
	cursor := r1.After
	p := attachParams(h)
	p.Cursor = &cursor
	r2 := a.attach(p)
	if r2.Snapshot != nil || r2.Reset != "" || r2.After != cursor || r2.Subscription != "s-2" {
		t.Fatalf("an honoured cursor's reply: %+v", r2)
	}
	for want := cursor.Seq + 1; want <= head; want++ {
		if seq, _ := eventOf(t, a.note(protocol.NotifyEvent)); seq != want {
			t.Fatalf("replayed seq %d, want %d", seq, want)
		}
	}
	if s := paramsOf[protocol.SynchronizedParams](t, a.note(protocol.NotifySynchronized)); s != (protocol.SynchronizedParams{Subscription: "s-2", Seq: head}) {
		t.Fatalf("a cursor replay's synchronized: %+v, want at the cutoff %d", s, head)
	}
	ok[protocol.Empty](t, a.detach(h, r2.Subscription))

	// A cursor at the head: nothing to replay, so synchronized at once.
	at := protocol.Cursor{Incarnation: cursor.Incarnation, Seq: head}
	p.Cursor = &at
	r3 := a.attach(p)
	if r3.Snapshot != nil || r3.After != at {
		t.Fatalf("a cursor at the head: %+v", r3)
	}
	if s := paramsOf[protocol.SynchronizedParams](t, a.note(protocol.NotifySynchronized)); s.Seq != head {
		t.Fatalf("a cursor at the head's synchronized: %+v", s)
	}
}

// TestASecondAttachIsAlreadyAttached (SQ14, §3.3): a connection holds one
// attachment; a second attach while it is live is bad_request, reason
// already_attached; a detach naming anything but the live attachment is
// bad_request; after the detach a new attach is fine and gets the next id.
func TestASecondAttachIsAlreadyAttached(t *testing.T) {
	h := newHost(t)
	a := h.dial()
	a.sayHello(nil)
	a.attach(attachParams(h))
	a.note(protocol.NotifySynchronized)
	refusedWith(t, a.call(protocol.MethodSessionAttach, attachParams(h)), protocol.RPCRefused, protocol.CodeBadRequest, protocol.ReasonAlreadyAttached)
	refusedWith(t, a.detach(h, "s-9"), protocol.RPCRefused, protocol.CodeBadRequest, protocol.ReasonBadRequest)
	ok[protocol.Empty](t, a.detach(h, "s-1"))
	refusedWith(t, a.detach(h, "s-1"), protocol.RPCRefused, protocol.CodeBadRequest, protocol.ReasonBadRequest)
	if r := a.attach(attachParams(h)); r.Subscription != "s-2" {
		t.Fatalf("the attach after a detach: %+v", r)
	}
	a.note(protocol.NotifySynchronized)
	// Another connection's attachment is its own.
	b := h.dial()
	b.sayHello(nil)
	if r := b.attach(attachParams(h)); r.Subscription != "s-1" {
		t.Fatalf("another connection's attach: %+v", r)
	}
}

// TestAttachParamsAreChecked (§3.2, §3.4): when is ready or now; a budget's
// members are positive; a snapshot the budget cannot hold is failed, reason
// snapshot_too_large, and frees the connection's attachment for the next
// attach.
func TestAttachParamsAreChecked(t *testing.T) {
	h := newHost(t)
	h.publish(agent.Event{Type: agent.EventText, Text: "something to snapshot"})
	a := h.dial()
	a.sayHello(nil)
	p := attachParams(h)
	p.When = "later"
	refusedWith(t, a.call(protocol.MethodSessionAttach, p), protocol.RPCInvalidParams, protocol.CodeBadRequest, protocol.ReasonBadRequest)
	p = attachParams(h)
	p.Budget = &protocol.AttachBudget{MaxItems: -1}
	refusedWith(t, a.call(protocol.MethodSessionAttach, p), protocol.RPCInvalidParams, protocol.CodeBadRequest, protocol.ReasonBadRequest)
	p.Budget = &protocol.AttachBudget{SnapshotBytes: 1}
	refusedWith(t, a.call(protocol.MethodSessionAttach, p), protocol.RPCRefused, protocol.CodeFailed, protocol.ReasonSnapshotTooLarge)
	p.SessionID = "not-this-one"
	refusedWith(t, a.call(protocol.MethodSessionAttach, p), protocol.RPCRefused, protocol.CodeUnknownSession, protocol.ReasonUnknownSession)
	if r := a.attach(attachParams(h)); r.Snapshot == nil {
		t.Fatalf("an attach after the refusals: %+v", r)
	}
}

// TestAWhenReadyAttachWaitsForTheStart (§3.4): when: "ready" — the default —
// waits for the session's start, the connection going on meanwhile, and then
// answers ready: true with the final info document.
func TestAWhenReadyAttachWaitsForTheStart(t *testing.T) {
	h := newHost(t, withoutStart())
	a := h.dial()
	a.sayHello(nil)
	id := a.send(protocol.MethodSessionAttach, attachParams(h))
	if st := ok[protocol.StateResult](t, a.call(protocol.MethodSessionState, protocol.StateParams{SessionID: sid(h)})); st.Activity != protocol.ActivityStarting {
		t.Fatalf("the state while the attach waits: %+v", st)
	}
	if err := h.eng.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	r := ok[protocol.AttachResult](t, a.reply(id))
	if !r.Ready || len(r.Session.Catalogs.Models) == 0 || r.Snapshot == nil {
		t.Fatalf("the attach after the start: %+v", r)
	}
	a.note(protocol.NotifySynchronized)
}

// TestAFailedStartIsStartFailed (§3.4): a when: "ready" attach waiting on a
// start that fails is not_accepting, reason start_failed, data.cause the
// failure; a when: "now" attach made before it gets a ready notification with
// startFailed and err; and an attach made after it — of either kind — is
// start_failed too, so a client learns of the failure from an attach's answer
// or a ready notification, never only from session.state.
func TestAFailedStartIsStartFailed(t *testing.T) {
	h := newHost(t, withoutStart())
	a, b := h.dial(), h.dial()
	a.sayHello(nil)
	b.sayHello(nil)
	id := a.send(protocol.MethodSessionAttach, attachParams(h))
	now := attachParams(h)
	now.When = protocol.WhenNow
	rb := b.attach(now)
	if rb.Ready {
		t.Fatalf("a when: now attach while starting: %+v", rb)
	}
	b.note(protocol.NotifySynchronized)

	h.eng.Started(errors.New("the agent would not start"))
	e := refusedWith(t, a.reply(id), protocol.RPCRefused, protocol.CodeNotAccepting, protocol.ReasonStartFailed)
	if e.Data.Cause != "the agent would not start" {
		t.Fatalf("the cause: %+v", e.Data)
	}
	ready := paramsOf[protocol.ReadyParams](t, b.note(protocol.NotifyReady))
	if ready.Subscription != rb.Subscription || !ready.StartFailed || ready.Err != "the agent would not start" {
		t.Fatalf("the ready notification: %+v", ready)
	}

	c := h.dial()
	c.sayHello(nil)
	refusedWith(t, c.call(protocol.MethodSessionAttach, now), protocol.RPCRefused, protocol.CodeNotAccepting, protocol.ReasonStartFailed)
	refusedWith(t, c.call(protocol.MethodSessionAttach, attachParams(h)), protocol.RPCRefused, protocol.CodeNotAccepting, protocol.ReasonStartFailed)
}

// TestAClosedEngineIsNotAccepting (§3.4, §3.7): Ready closes when the engine
// closes, so an attach waiting for a start that never comes is answered
// not_accepting as the engine closes, and the connection closes after it.
func TestAClosedEngineIsNotAccepting(t *testing.T) {
	h := newHost(t, withoutStart())
	a := h.dial()
	a.sayHello(nil)
	id := a.send(protocol.MethodSessionAttach, attachParams(h))
	// A request read after it is answered, so the attach has been admitted:
	// the session's end answers what was admitted before it.
	ok[protocol.StateResult](t, a.call(protocol.MethodSessionState, protocol.StateParams{SessionID: sid(h)}))
	if err := h.eng.Close(); err != nil {
		t.Fatal(err)
	}
	refusedWith(t, a.reply(id), protocol.RPCRefused, protocol.CodeNotAccepting, protocol.ReasonNotAccepting)
	a.expectClosed()
}

// TestAWhenReadyAttachGivesUpAtItsBound (§3.4): the server's own bound on the
// wait for readiness (10 minutes; a test seam here) answers unavailable,
// reason not_ready — never stored — and the connection attaches once the
// session is up.
func TestAWhenReadyAttachGivesUpAtItsBound(t *testing.T) {
	h := newHost(t, withoutStart(), withReadyWait(20*time.Millisecond))
	a := h.dial()
	a.sayHello(nil)
	refusedWith(t, a.call(protocol.MethodSessionAttach, attachParams(h)), protocol.RPCRefused, protocol.CodeUnavailable, protocol.ReasonNotReady)
	if err := h.eng.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r := a.attach(attachParams(h)); !r.Ready || r.Subscription != "s-2" {
		t.Fatalf("the attach once up: %+v", r)
	}
}

// TestReadyCarriesTheFinalInfo (A25; §3.4, astra 15): a when: "now" attach
// before the start is answered ready: false with the info document as it
// stands — no catalogs, no provider session id — and then gets exactly one
// ready notification when the start completes, carrying the FINAL document,
// queued after every record committed before the start completed: a forwarder
// held on the first of them holds the ready back too.
func TestReadyCarriesTheFinalInfo(t *testing.T) {
	gate := newForwardGate()
	owed := newSignal()
	h := newHost(t, withoutStart(), withLateID(), withOnClose(gate.open),
		withHooks(control.TestHooks{BeforeForward: gate.hook, ReadyOwed: func(_ string, seq uint64) { owed.fire(seq) }}))
	a := h.dial()
	a.sayHello(nil)
	p := attachParams(h)
	p.When = protocol.WhenNow
	r := a.attach(p)
	switch {
	case r.Ready, len(r.Session.Catalogs.Models) != 0, len(r.Session.Catalogs.Modes) != 0, r.Session.ProviderSessionID != "":
		t.Fatalf("the reply before the start: %+v", r)
	}
	a.note(protocol.NotifySynchronized)

	gate.arm(r.After.Seq)
	for _, text := range []string{"starting", "still starting", "nearly"} {
		h.publish(agent.Event{Type: agent.EventText, Text: text})
	}
	gate.awaitHeld(t)
	if err := h.eng.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	s := owed.await(t, "the ready to be owed")
	if s < r.After.Seq+3 {
		t.Fatalf("the ready waits behind %d, before the start's records (after %d)", s, r.After.Seq)
	}
	gate.open()
	for i := range 3 {
		if seq, _ := eventOf(t, a.note(protocol.NotifyEvent)); seq != r.After.Seq+uint64(i)+1 {
			t.Fatalf("event %d is seq %d", i, seq)
		}
	}
	ready := paramsOf[protocol.ReadyParams](t, a.note(protocol.NotifyReady))
	switch {
	case ready.Subscription != r.Subscription, ready.StartFailed, ready.Err != "":
		t.Fatalf("the ready notification: %+v", ready)
	case len(ready.Session.Catalogs.Models) != 2 || len(ready.Session.Catalogs.Modes) != 3:
		t.Fatalf("the ready's catalogs: %+v", ready.Session.Catalogs)
	case ready.Session.ProviderSessionID != "stub-session-1":
		t.Fatalf("the ready's provider session id: %q", ready.Session.ProviderSessionID)
	}
	// Exactly one: nothing more is owed.
	notes, head := a.sync(h)
	if head < s {
		t.Fatalf("the sync %d is behind the ready's %d", head, s)
	}
	for _, n := range notes {
		if n.Method == protocol.NotifyReady {
			t.Fatalf("a second ready: %s", n.Params)
		}
	}
}

// TestAReplayLargerThanTheBudgetEndsInASnapshot (A25; §3.4, astra 22): a
// session/load-style replay larger than the largest budget a client may ask
// for — the host's MaxBudget, which caps a larger ask — outruns a when: "now"
// attachment: it ends reset{slow_consumer} before readiness, with no ready
// notification, and never delays the start; a re-attach when: "ready" with no
// cursor gets a snapshot after the replay.
func TestAReplayLargerThanTheBudgetEndsInASnapshot(t *testing.T) {
	gate := newForwardGate()
	h := newHost(t, withoutStart(), withMaxBudget(control.Budget{MaxItems: 16, MaxBytes: 1 << 20}),
		withOnClose(gate.open), withHooks(control.TestHooks{BeforeForward: gate.hook}))
	for i := range 64 {
		h.stub.Replay = append(h.stub.Replay, agent.Event{Type: agent.EventText, Text: strings.Repeat("l", i%7+1)})
	}
	a := h.dial()
	a.sayHello(nil)
	p := attachParams(h)
	p.When = protocol.WhenNow
	p.Budget = &protocol.AttachBudget{MaxItems: 1 << 20, MaxBytes: 1 << 30}
	r := a.attach(p)
	if r.Ready {
		t.Fatalf("the premise: %+v", r)
	}
	a.note(protocol.NotifySynchronized)

	// The forwarder is held on the first record it is handed — if the replay
	// leaves its subscription time to hand it one before overflowing it — so
	// the replay outruns the budget either way, and the start never waits.
	gate.arm(r.After.Seq)
	started := make(chan error, 1)
	go func() { started <- h.eng.Start(context.Background()) }()
	select {
	case err := <-started:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(watchdog):
		t.Fatal("the start waited on a held attachment")
	}
	if h.dropped() != 1 {
		t.Fatalf("the replay did not outrun the budget: %d dropped", h.dropped())
	}
	head := h.head()
	gate.open()
	var notes []*protocol.Notification
	for {
		n := a.next().note
		if n == nil {
			t.Fatal("a reply on a stream with no request")
		}
		if n.Method == protocol.NotifyReset {
			if rp := paramsOf[protocol.ResetParams](t, n); rp != (protocol.ResetParams{Subscription: "s-1", Reason: protocol.ResetSlowConsumer}) {
				t.Fatalf("the reset: %+v", rp)
			}
			break
		}
		if n.Method != protocol.NotifyEvent {
			t.Fatalf("before the reset: %s %s", n.Method, n.Params)
		}
		notes = append(notes, n)
	}
	if last := lastEvent(t, notes, r.After.Seq); last >= head || last != r.After.Seq+uint64(len(notes)) {
		t.Fatalf("%d events before the reset, the last %d of %d: not a contiguous prefix", len(notes), last, head)
	}

	re := a.attach(attachParams(h))
	if !re.Ready || re.Snapshot == nil || re.After.Seq != head || re.Subscription != "s-2" {
		t.Fatalf("the re-attach: ready %v, snapshot %v, after %+v (head %d)", re.Ready, re.Snapshot != nil, re.After, head)
	}
	a.note(protocol.NotifySynchronized)
}
