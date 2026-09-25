package remote_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/remote"
)

// The re-attach rules (§3.4's table, §3.14), each against the real server.
// A subscription is dropped slow_consumer deterministically by a budget of
// smallBudget bytes and one record larger than it: the log drops a
// subscription whose buffer would go over its bytes at the offer, whatever its
// reader is doing.

// smallBudget is a subscription budget of 4 KiB; big is a text over it.
var (
	smallBudget = &protocol.AttachBudget{MaxBytes: 4 << 10}
	big         = strings.Repeat("B", 8<<10)
)

// isAttach says l is a session.attach request.
func isAttach(l wireLine) bool { return l.out && l.method == protocol.MethodSessionAttach }

// withCursor says an attach request carries a cursor.
func withCursor(l wireLine) bool {
	var p protocol.AttachParams
	return json.Unmarshal(l.params, &p) == nil && p.Cursor != nil
}

// attachReply is the host's reply to attach request l.
func attachReply(t *testing.T, tp *tap, l wireLine) protocol.AttachResult {
	t.Helper()
	var got []wireLine
	tp.await(t, "the attach reply", func() bool {
		got = tp.received(func(r wireLine) bool { return r.conn == l.conn && r.id == l.id && r.resp != nil })
		return len(got) == 1
	})
	var res protocol.AttachResult
	if got[0].resp.Error != nil {
		t.Fatalf("the attach was refused: %v", got[0].resp.Error)
	}
	if err := json.Unmarshal(got[0].resp.Result, &res); err != nil {
		t.Fatal(err)
	}
	return res
}

// TestASlowConsumerAfterReadinessReattachesWithItsCursor (§3.4, X18 4): a
// subscription dropped slow_consumer once the session is ready is re-attached
// WITH the stream's cursor — exactly {incarnation, the last seq it holds}: the
// small event, received and queued though not yet handed out, and not the
// record the host dropped the subscription on (C7a sends the reset without
// it) — while the ResumeState cursor stays the last one handed out; the host
// answers from its journal and ring — here the dropped record has left the
// ring by the time the held re-attach arrives, so the head is the journal's —
// and the stream goes on with no Restore, no gap and no duplicate.
func TestASlowConsumerAfterReadinessReattachesWithItsCursor(t *testing.T) {
	w, lo := testJournal(t)
	lo.RingEvents = 2
	h := newHost(t, withLog(lo))
	tp := newTap(t)
	release := make(chan struct{})
	tp.setHoldOut(func(l wireLine) <-chan struct{} {
		if isAttach(l) && withCursor(l) {
			return release
		}
		return nil
	})
	c := dialClient(t, h.path, tp, remote.Options{})
	// Registered after the client, so it runs before the client's Close: a
	// failure while the re-attach is held lets it go before Close waits for
	// the reader it is held on.
	t.Cleanup(func() { closeOnce(release) })
	s := attach(t, c, remote.AttachOptions{Budget: smallBudget})
	after := nextKind(t, s, remote.KindAttached).Reply.After
	nextKind(t, s, remote.KindSynchronized)
	h.text("small")
	smallSeq := h.head()
	// The small event is on the wire before the record that drops the
	// subscription is published: the stream holds it when the reset comes.
	tp.await(t, "the small event", func() bool {
		return len(tp.received(func(l wireLine) bool {
			var p protocol.EventParams
			return l.method == protocol.NotifyEvent && json.Unmarshal(l.params, &p) == nil && p.Seq == smallSeq
		})) == 1
	})
	h.text(big)
	tp.await(t, "the re-attach", func() bool { return len(tp.sent(protocol.MethodSessionAttach)) == 2 })
	if h.dropped() != 1 {
		t.Fatalf("the premise: the host dropped %d subscriptions slow", h.dropped())
	}
	if st := c.ResumeState(); st.Cursor == nil || *st.Cursor != after {
		t.Fatalf("the persisted cursor before anything is read: %+v, want the attach's after %+v", st.Cursor, after)
	}
	// While the re-attach is held, the dropped record leaves the ring.
	h.text("s1")
	h.text("s2")
	flushed(t, w, h.head())
	closeOnce(release)

	items := until(t, s, isText(t, "s2"))
	items = append(items, until(t, s, ofKind(remote.KindSynchronized))...)
	if n := kinds(items)[remote.KindRestore]; n != 0 {
		t.Fatalf("%d restore items: the cursor was not honoured", n)
	}
	if last := contiguous(t, after.Seq+1, items); last != h.head() {
		t.Fatalf("the stream reached %d, want %d", last, h.head())
	}
	re := tp.sent(protocol.MethodSessionAttach)[1]
	p := attachParams(t, re)
	if want := (protocol.Cursor{Incarnation: after.Incarnation, Seq: smallSeq}); p.Cursor == nil || *p.Cursor != want {
		t.Fatalf("the re-attach's cursor: %+v, want %+v, the last seq the stream held", p.Cursor, want)
	}
	if st, last := c.ResumeState(), contiguous(t, after.Seq+1, items); st.Cursor == nil || st.Cursor.Seq != last {
		t.Fatalf("the persisted cursor: %+v, want the last seq handed out, %d", st.Cursor, last)
	}
	if r := attachReply(t, tp, re); r.Snapshot != nil || r.Reset != "" || r.After != *p.Cursor {
		t.Fatalf("the re-attach's reply: after %+v, snapshot %v, reset %q", r.After, r.Snapshot != nil, r.Reset)
	}
}

func closeOnce(ch chan struct{}) {
	select {
	case <-ch:
	default:
		close(ch)
	}
}

// TestASlowConsumerWhoseCursorIsRefusedIsRestored (§3.4): the same re-attach
// with its cursor, which the host refuses — the backlog, the dropped record
// among it, is over the budget — is answered with the refusal's reason and a
// snapshot: a Restore item, from which the stream goes on.
func TestASlowConsumerWhoseCursorIsRefusedIsRestored(t *testing.T) {
	h := newHost(t)
	tp := newTap(t)
	c := dialClient(t, h.path, tp, remote.Options{})
	s := attach(t, c, remote.AttachOptions{Budget: smallBudget})
	after := nextKind(t, s, remote.KindAttached).Reply.After
	nextKind(t, s, remote.KindSynchronized)
	h.text(big)
	head := h.head()
	items := until(t, s, ofKind(remote.KindRestore))
	r := items[len(items)-1].Reply
	if r.Reset != protocol.CursorBacklogTooLarge || r.Snapshot == nil || r.After.Seq != head || r.After.Incarnation != after.Incarnation {
		t.Fatalf("the restore: %s", describe(items[len(items)-1]))
	}
	contiguous(t, after.Seq+1, items)
	nextKind(t, s, remote.KindSynchronized)
	if p := attachParams(t, tp.sent(protocol.MethodSessionAttach)[1]); p.Cursor == nil || h.dropped() != 1 {
		t.Fatalf("the re-attach after readiness: cursor %+v, %d subscriptions dropped slow", p.Cursor, h.dropped())
	}
	h.text("after the restore")
	if it := next(t, s); !isText(t, "after the restore")(it) || it.Seq != head+1 {
		t.Fatalf("after the restore: %s", describe(it))
	}
}

// TestASlowConsumerBeforeReadinessReattachesWhenReady (§3.4, astra 22): a
// subscription made with when: "now" before the session's start, dropped
// slow_consumer before any ready, is re-attached when: "ready" with NO cursor
// — never a cursor inside the same burst — so the host answers once the start
// has run, with a snapshot: a Restore item, then the Ready the first reply
// owed (made from the re-attach's reply: the ready notification went with the
// dropped attachment), then synchronized.
func TestASlowConsumerBeforeReadinessReattachesWhenReady(t *testing.T) {
	h := newHost(t, withoutStart())
	tp := newTap(t)
	c := dialClient(t, h.path, tp, remote.Options{})
	s := attach(t, c, remote.AttachOptions{When: protocol.WhenNow, Budget: smallBudget})
	if r := nextKind(t, s, remote.KindAttached).Reply; r.Ready {
		t.Fatalf("the premise: the first reply says ready: %+v", r)
	}
	nextKind(t, s, remote.KindSynchronized)
	h.text(big)
	tp.await(t, "the re-attach", func() bool { return len(tp.sent(protocol.MethodSessionAttach)) == 2 })
	p := attachParams(t, tp.sent(protocol.MethodSessionAttach)[1])
	if p.Cursor != nil || p.When != protocol.WhenReady || h.dropped() != 1 {
		t.Fatalf("the re-attach before readiness: cursor %+v, when %q, %d dropped slow; want no cursor, when: ready, after a drop",
			p.Cursor, p.When, h.dropped())
	}
	if err := h.eng.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	head := h.head()
	items := until(t, s, ofKind(remote.KindSynchronized))
	var restore, ready *remote.Item
	for i := range items {
		switch items[i].Kind {
		case remote.KindRestore:
			restore = &items[i]
		case remote.KindReady:
			if restore == nil {
				t.Fatal("a ready before the restore")
			}
			ready = &items[i]
		case remote.KindEvent:
			if restore != nil {
				t.Fatalf("an event between the restore and synchronized: %s", describe(items[i]))
			}
		}
	}
	switch {
	case restore == nil || !restore.Reply.Ready || restore.Reply.Snapshot == nil || restore.Reply.After.Seq != head:
		t.Fatalf("the restore: %v (head %d)", restore, head)
	case ready == nil || ready.Ready.StartFailed || len(ready.Ready.Session.Catalogs.Models) == 0:
		t.Fatalf("the ready owed: %v", ready)
	}
}

// TestAnOmittedRecordReattachesWithNoCursor (§3.4): reset{omitted} — a record
// no client can fold — is re-attached with no cursor, and the snapshot past it
// is a Restore item.
func TestAnOmittedRecordReattachesWithNoCursor(t *testing.T) {
	h := newHost(t, withLog(agent.EventLogOptions{MaxRecordBytes: 4 << 10}))
	tp := newTap(t)
	c := dialClient(t, h.path, tp, remote.Options{})
	s := attach(t, c, remote.AttachOptions{})
	after := nextKind(t, s, remote.KindAttached).Reply.After
	nextKind(t, s, remote.KindSynchronized)
	h.text("small")
	h.text(big)
	head := h.head()
	items := until(t, s, ofKind(remote.KindRestore))
	if r := items[len(items)-1].Reply; r.Snapshot == nil || r.After.Seq != head || r.Reset != "" {
		t.Fatalf("the restore: %s", describe(items[len(items)-1]))
	}
	contiguous(t, after.Seq+1, items)
	if p := attachParams(t, tp.sent(protocol.MethodSessionAttach)[1]); p.Cursor != nil {
		t.Fatalf("the re-attach after omitted carried a cursor: %+v", p.Cursor)
	}
	nextKind(t, s, remote.KindSynchronized)
}

// TestAFailedReplayReattachesWithNoCursor (§3.4): a cursor honoured from the
// journal whose file leg fails part-way ends reset{replay_failed}; the stream
// re-attaches with no cursor and hands up a Restore item — the events it
// handed up before it contiguous from the cursor, none past the failure.
func TestAFailedReplayReattachesWithNoCursor(t *testing.T) {
	w, lo := testJournal(t)
	lo.RingEvents = 8
	h := newHost(t, withLog(lo))
	for i := range 40 {
		h.text(strings.Repeat("r", i%5+1))
	}
	head := h.head()
	flushed(t, w, head)
	corruptJournalLine(t, w, 15)
	tp := newTap(t)
	c := dialClient(t, h.path, tp, remote.Options{})
	inc := h.eng.State().Incarnation
	s := attach(t, c, remote.AttachOptions{Cursor: &protocol.Cursor{Incarnation: inc, Seq: 5}})
	if r := nextKind(t, s, remote.KindAttached).Reply; r.Snapshot != nil || r.After.Seq != 5 {
		t.Fatalf("the attach from the cursor: %+v", r.After)
	}
	items := until(t, s, ofKind(remote.KindRestore))
	if last := contiguous(t, 6, items[:len(items)-1]); last != 14 {
		t.Fatalf("the replay reached %d before failing, want 14", last)
	}
	if r := items[len(items)-1].Reply; r.Snapshot == nil || r.After.Seq != head {
		t.Fatalf("the restore: %s", describe(items[len(items)-1]))
	}
	if p := attachParams(t, tp.sent(protocol.MethodSessionAttach)[1]); p.Cursor != nil {
		t.Fatalf("the re-attach after replay_failed carried a cursor: %+v", p.Cursor)
	}
	nextKind(t, s, remote.KindSynchronized)
}

// TestAReplacedSessionIsReattachedAfresh (§3.4, §3.6): reset{session_replaced}
// — the host swapped its engine and closes the connection — is answered by a
// reconnect whose hello is a fresh one (the old token is void: no resume),
// which the host answers resumed: false; a command that was running in the
// replaced engine resolves ErrOutcomeUnknown (resume_lost) and is not resent;
// and the stream attaches the session the host now serves, with no cursor: a
// Restore item carrying the new session's info.
func TestAReplacedSessionIsReattachedAfresh(t *testing.T) {
	h := newHost(t)
	held, release := h.stub.HoldNextSet()
	t.Cleanup(release)
	tp := newTap(t)
	c := dialClient(t, h.path, tp, remote.Options{})
	first := c.Hello()
	s := attach(t, c, remote.AttachOptions{})
	nextKind(t, s, remote.KindAttached)
	nextKind(t, s, remote.KindSynchronized)
	done := make(chan error, 1)
	go func() {
		_, err := c.Command(tctx(t), protocol.MethodSessionSet, protocol.SetParams{SessionID: h.sid(),
			Setting: protocol.Setting{Kind: protocol.SettingModel, Value: "fast"}}, nil, remote.CommandOptions{})
		done <- err
	}()
	await(t, held, "the Set to park")

	next := startedEngine(t)
	h.srv.SetEngine(next)
	items := until(t, s, ofKind(remote.KindRestore))
	r := items[len(items)-1].Reply
	if want := next.State(); r.Session.SessionID != want.CrazeSessionID || r.After.Incarnation != want.Incarnation || r.Snapshot == nil {
		t.Fatalf("the restore: session %s (want %s), after %+v", r.Session.SessionID, want.CrazeSessionID, r.After)
	}
	if s.SessionID() != next.State().CrazeSessionID {
		t.Fatalf("the stream's session: %s", s.SessionID())
	}
	nextKind(t, s, remote.KindSynchronized)
	outcomeUnknown(t, recv(t, done), protocol.ReasonResumeLost)
	if hc := c.Hello(); hc.Resumed || hc.Token == first.Token {
		t.Fatalf("the reconnect's hello: %+v, want a fresh client", hc)
	}
	var hp protocol.HelloParams
	if hellos := tp.sent(protocol.MethodHello); len(hellos) != 2 || json.Unmarshal(hellos[1].params, &hp) != nil || hp.Resume != nil {
		t.Fatalf("the reconnect's hello asked to resume a void token: %+v", hp.Resume)
	}
	if n := len(tp.sent(protocol.MethodSessionSet)); n != 1 {
		t.Fatalf("the Set was sent %d times", n)
	}
	if p := attachParams(t, tp.sent(protocol.MethodSessionAttach)[1]); p.Cursor != nil || p.SessionID != next.State().CrazeSessionID {
		t.Fatalf("the attach anew: %+v", p)
	}
}

// TestASessionClosedEndsTheStream (§3.4): reset{session_closed} — after the
// session's final records — is an End item, the stream's last; the host
// closes the connection, which is not redialled; and the client has stopped.
func TestASessionClosedEndsTheStream(t *testing.T) {
	h := newHost(t)
	tp := newTap(t)
	c := dialClient(t, h.path, tp, remote.Options{})
	s := attach(t, c, remote.AttachOptions{})
	nextKind(t, s, remote.KindAttached)
	nextKind(t, s, remote.KindSynchronized)
	h.text("the last word")
	until(t, s, isText(t, "the last word"))
	if err := h.eng.Close(); err != nil {
		t.Fatal(err)
	}
	until(t, s, ofKind(remote.KindEnd))
	if _, err := s.Next(tctx(t)); !errors.Is(err, remote.ErrStreamClosed) {
		t.Fatalf("Next after the end: %v", err)
	}
	await(t, c.Done(), "the client to stop")
	if !errors.Is(c.Err(), remote.ErrSessionEnded) {
		t.Fatalf("the client stopped with %v", c.Err())
	}
	if n := tp.dialCount(); n != 1 {
		t.Fatalf("%d dials: the ended session was redialled", n)
	}
}

// TestASeqThatDoesNotFollowOnIsAnError (§3.14): the stream checks that each
// event's seq follows the last it handed up — a hole (an event the tap loses
// on the wire) or a duplicate (one it repeats) is an Error item, never
// swallowed — and it detaches the host's attachment before it says so, so the
// client can attach again at once.
func TestASeqThatDoesNotFollowOnIsAnError(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(line []byte) [][]byte
	}{
		{"a hole", func([]byte) [][]byte { return [][]byte{} }},
		{"a duplicate", func(line []byte) [][]byte { return [][]byte{line, line} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHost(t)
			tp := newTap(t)
			tp.setRewriteIn(func(l wireLine) [][]byte {
				var p protocol.EventParams
				if l.resp == nil && l.method == protocol.NotifyEvent && json.Unmarshal(l.params, &p) == nil &&
					strings.Contains(string(p.Event), "tampered") {
					return tc.edit(l.raw)
				}
				return nil
			})
			c := dialClient(t, h.path, tp, remote.Options{})
			s := attach(t, c, remote.AttachOptions{})
			nextKind(t, s, remote.KindAttached)
			nextKind(t, s, remote.KindSynchronized)
			h.text("kept")
			h.text("tampered")
			h.text("after")
			items := until(t, s, ofKind(remote.KindError))
			if last := items[len(items)-1]; !errors.Is(last.Err, remote.ErrStreamGap) {
				t.Fatalf("the stream's last item: %s", describe(last))
			}
			if slices.ContainsFunc(items, isText(t, "after")) {
				t.Fatal("an event past the break was handed up")
			}
			s2 := attach(t, c, remote.AttachOptions{})
			if r := nextKind(t, s2, remote.KindAttached).Reply; r.Subscription != "s-2" || r.After.Seq != h.head() {
				t.Fatalf("the attach after the error: %+v", r)
			}
		})
	}
}

// awaitAttaches waits until the client has written n attach requests.
func awaitAttaches(t *testing.T, tp *tap, n int) {
	t.Helper()
	tp.await(t, fmt.Sprintf("attach request %d", n), func() bool { return len(tp.sent(protocol.MethodSessionAttach)) >= n })
}

// pendingEpisode is a stream whose session has not started, dropped
// slow_consumer before readiness — its re-attach (when: "ready") waits for a
// start that never comes — and then made to re-attach again and again by
// killing each connection while its re-attach waits: n re-attaches in one
// episode, none reaching synchronized.
func pendingEpisode(t *testing.T, n int) (*host, *tap, *remote.Client, *remote.Stream) {
	t.Helper()
	h := newHost(t, withoutStart())
	tp := newTap(t)
	c := dialClient(t, h.path, tp, remote.Options{Redials: 100})
	s := attach(t, c, remote.AttachOptions{When: protocol.WhenNow, Budget: smallBudget})
	nextKind(t, s, remote.KindAttached)
	nextKind(t, s, remote.KindSynchronized)
	h.text(big)
	awaitAttaches(t, tp, 2) // re-attach 1: the reset's
	for i := 2; i <= n; i++ {
		tp.kill()
		awaitAttaches(t, tp, i+1) // re-attach i: a reconnect's
	}
	return h, tp, c, s
}

// TestReattachesAreBoundedPerEpisode (§3.14; attachprobe's bound): a stream
// re-attaches at most protocol.ReattachesPerEpisode (8) times without reaching
// synchronized — the reset's re-attach and the reconnects' alike — and past
// the bound hands up an Error item and stops, sending no ninth; the client
// itself goes on.
func TestReattachesAreBoundedPerEpisode(t *testing.T) {
	h, tp, c, s := pendingEpisode(t, protocol.ReattachesPerEpisode)
	tp.kill()
	items := until(t, s, ofKind(remote.KindError))
	if last := items[len(items)-1]; !errors.Is(last.Err, remote.ErrReattachBound) {
		t.Fatalf("the stream's last item: %s", describe(last))
	}
	if n := len(tp.sent(protocol.MethodSessionAttach)); n != 1+protocol.ReattachesPerEpisode {
		t.Fatalf("%d attach requests, want the first and %d re-attaches", n, protocol.ReattachesPerEpisode)
	}
	var st protocol.StateResult
	if err := c.Call(tctx(t), protocol.MethodSessionState, protocol.StateParams{SessionID: h.sid()}, &st); err != nil {
		t.Fatalf("the client after its stream stopped: %v", err)
	}
}

// TestTheEpisodeCountResetsAtSynchronized (§3.14): an episode of 8
// re-attaches — the bound, reached — that ends in synchronized (the start
// runs: the waiting re-attach is answered with a snapshot) starts the count
// again, so the next reset's re-attach, the ninth overall, is made, and the
// stream goes on.
func TestTheEpisodeCountResetsAtSynchronized(t *testing.T) {
	h, tp, _, s := pendingEpisode(t, protocol.ReattachesPerEpisode)
	if err := h.eng.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	items := until(t, s, ofKind(remote.KindSynchronized))
	if k := kinds(items); k[remote.KindRestore] != 1 || k[remote.KindReady] != 1 {
		t.Fatalf("the episode's end: %v", k)
	}
	h.text(big) // reset{slow_consumer}, now ready: a re-attach with the cursor
	head := h.head()
	items = until(t, s, ofKind(remote.KindRestore))
	if r := items[len(items)-1].Reply; r.After.Seq != head {
		t.Fatalf("the ninth re-attach's restore: %s", describe(items[len(items)-1]))
	}
	nextKind(t, s, remote.KindSynchronized)
	if n := len(tp.sent(protocol.MethodSessionAttach)); n != 1+protocol.ReattachesPerEpisode+1 {
		t.Fatalf("%d attach requests, want the first and 9 re-attaches", n)
	}
}
