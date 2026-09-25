package remote_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/remote"
)

// The stream rules of astra's r9 review (X18 4, 6–8), each on a schedule the
// tap and the client's hooks pin.

// TestASynchronizedPastTheLastEventIsAGap (X18 4; astra r9 4): a cursor
// attach replays up to its cutoff, and synchronized names the cutoff; the tap
// loses the replay's last event, so synchronized comes one past the last event
// the stream holds. That is a hole: an Error item carrying ErrStreamGap, never
// synchronized — and the attachment is detached first, so the client can
// attach again at once.
func TestASynchronizedPastTheLastEventIsAGap(t *testing.T) {
	h := newHost(t)
	h.text("one")
	h.text("two")
	h.text("three")
	head := h.head()
	tp := newTap(t)
	tp.setRewriteIn(func(l wireLine) [][]byte {
		var p protocol.EventParams
		if l.resp == nil && l.method == protocol.NotifyEvent && json.Unmarshal(l.params, &p) == nil && p.Seq == head {
			return [][]byte{}
		}
		return nil
	})
	c := dialClient(t, h.path, tp, remote.Options{})
	inc := h.eng.State().Incarnation
	s := attach(t, c, remote.AttachOptions{Cursor: &protocol.Cursor{Incarnation: inc, Seq: head - 3}})
	if r := nextKind(t, s, remote.KindAttached).Reply; r.Snapshot != nil || r.After.Seq != head-3 {
		t.Fatalf("the premise: the cursor honoured: %+v", r.After)
	}
	items := until(t, s, func(it remote.Item) bool {
		return it.Kind == remote.KindError || it.Kind == remote.KindSynchronized
	})
	if last := items[len(items)-1]; last.Kind != remote.KindError || !errors.Is(last.Err, remote.ErrStreamGap) {
		t.Fatalf("the stream's item after the replay: %s", describe(last))
	}
	if last := contiguous(t, head-2, items); last != head-1 {
		t.Fatalf("the events handed up reach %d, want %d", last, head-1)
	}
	s2 := attach(t, c, remote.AttachOptions{})
	if r := nextKind(t, s2, remote.KindAttached).Reply; r.After.Seq != head {
		t.Fatalf("the attach after the gap: %+v", r.After)
	}
}

// TestAnOldStreamsCloseNeverDetachesANewOne (X18 6; astra r9 6): the
// connection goes while a stream's subscription is s-1, and its Close takes
// that subscription (the hook holds it there). A new stream attaches on the
// next connection, where subscription ids start again: it is s-1 too. The old
// Close then sends nothing — its subscription went with its connection — so
// the new stream is still attached.
func TestAnOldStreamsCloseNeverDetachesANewOne(t *testing.T) {
	h := newHost(t)
	tp := newTap(t)
	t.Cleanup(tp.releaseDials)
	closing, proceed := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() { closeOnce(proceed) })
	var once sync.Once
	c := dialHooked(t, h.path, tp, remote.Options{}, remote.TestHooks{
		Closing: func() {
			once.Do(func() {
				close(closing)
				hold(tp, proceed, "the old Close's release")
			})
		},
	})
	old := attach(t, c, remote.AttachOptions{})
	if r := nextKind(t, old, remote.KindAttached).Reply; r.Subscription != "s-1" {
		t.Fatalf("the premise: the old stream is %s", r.Subscription)
	}
	nextKind(t, old, remote.KindSynchronized)
	held := tp.holdDials()
	tp.kill()
	await(t, held, "the redial")
	closed := make(chan error, 1)
	go func() { closed <- old.Close(tctx(t)) }()
	await(t, closing, "the old stream's Close to take its subscription")
	tp.releaseDials()
	s := attach(t, c, remote.AttachOptions{})
	if r := nextKind(t, s, remote.KindAttached).Reply; r.Subscription != "s-1" {
		t.Fatalf("the premise: the new stream is %s, want s-1 again", r.Subscription)
	}
	nextKind(t, s, remote.KindSynchronized)
	close(proceed)
	if err := recv(t, closed); err != nil {
		t.Fatalf("the old stream's Close: %v", err)
	}
	if n := len(tp.sentOn(1, protocol.MethodSessionDetach)); n != 0 {
		t.Fatalf("the old stream's Close sent %d detaches on the new connection", n)
	}
	h.text("still attached")
	until(t, s, isText(t, "still attached"))
}

// isReattach says l is an attach request with when: "ready", as every
// re-attach is (and no first attach in these tests).
func isReattach(l wireLine) bool {
	var p protocol.AttachParams
	return l.out && l.method == protocol.MethodSessionAttach && json.Unmarshal(l.params, &p) == nil && p.When == protocol.WhenReady
}

// answersReattach says l, a line the host wrote, is the reply to a re-attach.
func answersReattach(tp *tap, l wireLine) bool {
	if l.resp == nil || l.method != protocol.MethodSessionAttach {
		return false
	}
	for _, r := range tp.sentOn(l.conn, protocol.MethodSessionAttach) {
		if r.id == l.id {
			return isReattach(r)
		}
	}
	return false
}

// holdingCaller has a caller's write hold c's connection — a session.state
// the tap holds, as a full socket would — until release is closed; its
// call's answer comes on the channel returned.
func holdingCaller(t *testing.T, tp *tap, c *remote.Client, sid string, release <-chan struct{}) <-chan error {
	t.Helper()
	tp.setHoldOut(func(l wireLine) <-chan struct{} {
		if l.method == protocol.MethodSessionState {
			return release
		}
		return nil
	})
	called := make(chan error, 1)
	go func() {
		called <- c.Call(tctx(t), protocol.MethodSessionState, protocol.StateParams{SessionID: sid}, nil)
	}()
	tp.await(t, "the caller's write", func() bool { return len(tp.sent(protocol.MethodSessionState)) == 1 })
	return called
}

// closedOnceDetached closes old — once a Close whose context ends first has
// said it waits (ctx.Err()) — and checks that it returned only after the host
// had answered the detach that frees its place, and that nothing of the old
// stream's (an attach, a detach) was written after it returned. It returns
// the number of lines recorded when it returned.
func closedOnceDetached(t *testing.T, tp *tap, old *remote.Stream, released func()) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := old.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close, the host still holding the stream's attachment: %v; want it to wait, until its context ended", err)
	}
	released()
	type result struct {
		err   error
		lines int
	}
	closed := make(chan result, 1)
	go func() {
		err := old.Close(tctx(t))
		closed <- result{err, tp.lineCount()}
	}()
	r := recv(t, closed)
	if r.err != nil {
		t.Fatalf("Close: %v", r.err)
	}
	answers := tp.received(func(l wireLine) bool { return l.conn == 0 && l.method == protocol.MethodSessionDetach })
	if len(answers) != 1 || errorCode(answers[0]) != "" || lineIndex(tp, answers[0]) >= r.lines {
		t.Fatalf("Close returned before the host had answered the detach that frees its place (%d answers)", len(answers))
	}
	return r.lines
}

// nothingAfter fails if the client wrote a re-attach or a detach from line i
// on: nothing of a closed stream's reaches the host after its Close returned.
func nothingAfter(t *testing.T, tp *tap, i int) {
	t.Helper()
	for _, l := range tp.linesFrom(i) {
		if isReattach(l) || (l.out && l.method == protocol.MethodSessionDetach) {
			t.Fatalf("the closed stream wrote after its Close returned: %s", l.raw)
		}
	}
}

// TestAClosedStreamsQueuedReattachIsNeverSent (C8c; astra r13 3, the review's
// schedule): a caller's write holds the connection; the host ends the
// stream's subscription reset{omitted}, and the reader posts the re-attach,
// whose writer waits for the connection. Close, finding no subscription,
// returns at once. The caller's write goes through and the writer runs the
// re-attach it was handed: the stream is over, so not a byte of it is written
// — had it been, the host would hold the attachment it made (the tap loses its
// reply) and refuse a replacement's attach already_attached. A replacement
// stream attaches, and nothing of the old stream's reaches the host after its
// Close returned.
func TestAClosedStreamsQueuedReattachIsNeverSent(t *testing.T) {
	h := newHost(t, withLog(agent.EventLogOptions{MaxRecordBytes: 4 << 10}))
	tp := newTap(t)
	posted, ran := make(chan struct{}, 8), make(chan struct{}, 8)
	c := dialHooked(t, h.path, tp, remote.Options{}, remote.TestHooks{
		Posted:  func() { posted <- struct{}{} },
		PostRan: func() { ran <- struct{}{} },
	})
	writing := make(chan struct{})
	t.Cleanup(func() { closeOnce(writing) })
	old := attach(t, c, remote.AttachOptions{})
	nextKind(t, old, remote.KindAttached)
	nextKind(t, old, remote.KindSynchronized)
	tp.setRewriteIn(func(l wireLine) [][]byte {
		if answersReattach(tp, l) {
			return [][]byte{}
		}
		return nil
	})
	called := holdingCaller(t, tp, c, h.sid(), writing)
	h.text(big)
	recv(t, posted)
	if err := old.Close(tctx(t)); err != nil {
		t.Fatalf("Close: %v", err)
	}
	closedAt := tp.lineCount()
	select {
	case err := <-called:
		t.Fatalf("the premise: the caller's write was not held (%v)", err)
	default:
	}
	closeOnce(writing)
	if err := recv(t, called); err != nil {
		t.Fatalf("the caller's call: %v", err)
	}
	recv(t, ran)
	if n := len(tp.sentOn(0, protocol.MethodSessionAttach)); n != 1 {
		t.Fatalf("%d attaches written, want the old stream's first alone: its re-attach, posted before its Close, was written after it", n)
	}
	s := attach(t, c, remote.AttachOptions{SessionID: h.sid()})
	nextKind(t, s, remote.KindAttached)
	nextKind(t, s, remote.KindSynchronized)
	nothingAfter(t, tp, closedAt)
	h.text("still attached")
	until(t, s, isText(t, "still attached"))
}

// TestCloseWaitsForAnAttachOnTheWire (C8c; astra r13 3): the host ends the
// stream's subscription reset{omitted}, and the re-attach is already being
// written — the tap holds its write, as a full socket would — when the caller
// closes the stream. Close waits (one whose context ends first says so): once
// the write goes through, the host attaches it, its reply comes to a stream
// that is over, and the attachment it made is detached; Close returns only
// once the host has answered that detach. A replacement stream attaches, and
// nothing of the old stream's reaches the host after its Close returned.
func TestCloseWaitsForAnAttachOnTheWire(t *testing.T) {
	h := newHost(t, withLog(agent.EventLogOptions{MaxRecordBytes: 4 << 10}))
	tp := newTap(t)
	c := dialClient(t, h.path, tp, remote.Options{})
	sending := make(chan struct{})
	t.Cleanup(func() { closeOnce(sending) })
	old := attach(t, c, remote.AttachOptions{})
	nextKind(t, old, remote.KindAttached)
	nextKind(t, old, remote.KindSynchronized)
	tp.setHoldOut(func(l wireLine) <-chan struct{} {
		if isReattach(l) {
			return sending
		}
		return nil
	})
	h.text(big)
	tp.await(t, "the re-attach's write", func() bool { return len(tp.sentOn(0, protocol.MethodSessionAttach)) == 2 })
	closedAt := closedOnceDetached(t, tp, old, func() { closeOnce(sending) })
	s := attach(t, c, remote.AttachOptions{SessionID: h.sid()})
	nextKind(t, s, remote.KindAttached)
	nextKind(t, s, remote.KindSynchronized)
	nothingAfter(t, tp, closedAt)
	h.text("still attached")
	until(t, s, isText(t, "still attached"))
}

// TestCloseWaitsForAFallenBehindDetach (C8c): a caller's write holds the
// connection while the stream, unread, falls behind: its detach waits for the
// connection, and the host holds the attachment meanwhile. Close waits (one
// whose context ends first says so) and returns only once the host has
// answered that detach; a replacement stream attaches, and nothing of the old
// stream's reaches the host after its Close returned.
func TestCloseWaitsForAFallenBehindDetach(t *testing.T) {
	h := newHost(t)
	tp := newTap(t)
	posted := make(chan struct{}, 8)
	c := dialHooked(t, h.path, tp, remote.Options{StreamBytes: 4 << 10}, remote.TestHooks{
		Posted: func() { posted <- struct{}{} },
	})
	writing := make(chan struct{})
	t.Cleanup(func() { closeOnce(writing) })
	old := attach(t, c, remote.AttachOptions{})
	inc := nextKind(t, old, remote.KindAttached).Reply.After.Incarnation
	nextKind(t, old, remote.KindSynchronized)
	called := holdingCaller(t, tp, c, h.sid(), writing)
	for i := range 64 {
		h.text(fmt.Sprintf("%02d%s", i, strings.Repeat("w", 256)))
	}
	recv(t, posted)
	closedAt := closedOnceDetached(t, tp, old, func() {
		closeOnce(writing)
		if err := recv(t, called); err != nil {
			t.Fatalf("the caller's call: %v", err)
		}
	})
	// From the head, with a cursor: no snapshot to overrun the small bound.
	s := attach(t, c, remote.AttachOptions{SessionID: h.sid(), Cursor: &protocol.Cursor{Incarnation: inc, Seq: h.head()}})
	nextKind(t, s, remote.KindAttached)
	nextKind(t, s, remote.KindSynchronized)
	nothingAfter(t, tp, closedAt)
	h.text("still attached")
	until(t, s, isText(t, "still attached"))
}

// TestAMalformedAttachReplyAnswersAttach (X18 7; astra r9 7): the host
// answers an attach with a result that is not an attach reply. Attach returns
// an error — never stranded until its context ends — and the connection, whose
// host broke the protocol and whose attachment the client cannot name, is
// dropped; the client goes on, and attaches again.
func TestAMalformedAttachReplyAnswersAttach(t *testing.T) {
	h := newHost(t)
	tp := newTap(t)
	var once sync.Once
	tp.setRewriteIn(func(l wireLine) [][]byte {
		var out [][]byte
		if l.method == protocol.MethodSessionAttach && l.resp != nil {
			once.Do(func() { out = [][]byte{withMember(t, l.raw, `[]`, "result")} })
		}
		return out
	})
	c := dialClient(t, h.path, tp, remote.Options{})
	if _, err := c.Attach(tctx(t), remote.AttachOptions{}); err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Attach, its reply malformed: %v", err)
	}
	s := attach(t, c, remote.AttachOptions{})
	nextKind(t, s, remote.KindAttached)
	nextKind(t, s, remote.KindSynchronized)
	if n := tp.connCount(); n != 2 {
		t.Fatalf("%d connections, want the one the malformed reply dropped and the next", n)
	}
}

// TestACommandReplyArrivesWhileTheStreamIsUnread (X18 8; astra r9 9): a
// caller stops reading its stream, the host writes far more events than the
// stream's bound, and the caller waits on a command, whose reply the host
// writes after them. The reader never waits for the caller: the stream falls
// behind — it queues nothing more and detaches its subscription — and the
// command's reply arrives. When the caller reads again it gets every event,
// in order, with no gap and no duplicate: each time it has drained, the
// stream re-attaches with its cursor (the backlog is several bounds long, so
// it may fall behind again on the way).
func TestACommandReplyArrivesWhileTheStreamIsUnread(t *testing.T) {
	h := newHost(t)
	tp := newTap(t)
	const bound = 4 << 10
	c := dialClient(t, h.path, tp, remote.Options{StreamBytes: bound})
	s := attach(t, c, remote.AttachOptions{})
	after := nextKind(t, s, remote.KindAttached).Reply.After
	nextKind(t, s, remote.KindSynchronized)
	for i := range 64 {
		h.text(fmt.Sprintf("%02d%s", i, strings.Repeat("u", 256)))
	}
	command(t, c, protocol.MethodQueueAdd, protocol.QueueAddParams{SessionID: h.sid(), Text: "answered"}, nil)
	if n := s.QueuedBytes(); n > bound {
		t.Fatalf("the stream holds %d bytes, over its bound of %d", n, bound)
	}
	// The reader posts its detach (X21): it may go out after the reply.
	tp.await(t, "the fall's detach", func() bool { return len(tp.sent(protocol.MethodSessionDetach)) > 0 })
	if n := len(tp.sent(protocol.MethodSessionDetach)); n != 1 {
		t.Fatalf("the premise: the stream fell behind and detached (%d detaches)", n)
	}
	h.text("marker")
	items := until(t, s, isText(t, "marker"))
	contiguous(t, after.Seq+1, items)
	if k := kinds(items); k[remote.KindRestore] != 0 {
		t.Fatalf("%d restores: a re-attach went without its cursor, or it was refused", k[remote.KindRestore])
	}
	re := tp.sent(protocol.MethodSessionAttach)
	if len(re) < 2 || len(re) != 1+len(tp.sent(protocol.MethodSessionDetach)) {
		t.Fatalf("%d attach requests and %d detaches, want a re-attach after each fall", len(re), len(tp.sent(protocol.MethodSessionDetach)))
	}
	for _, l := range re[1:] {
		if p := attachParams(t, l); p.Cursor == nil || p.Cursor.Incarnation != after.Incarnation {
			t.Fatalf("a re-attach once drained: %+v, want its cursor", p.Cursor)
		}
	}
}

// TestCloseReturnsWithAFullQueueAfterAStartFailedReattach (X18 8; astra r9,
// further 1): a stream attached when: "now" before the start, its queue full
// — its bound is exactly what its first two items take, and nobody reads — is
// dropped slow_consumer by the host before readiness; its re-attach waits for
// the start, which fails: the refusal hands up the Ready the stream is owed
// and its Error, whatever the bound, on the reader, which never waits. Close
// then returns, and the caller reads every item.
func TestCloseReturnsWithAFullQueueAfterAStartFailedReattach(t *testing.T) {
	h := newHost(t, withoutStart())
	tp := newTap(t)
	probe := dialClient(t, h.path, tp, remote.Options{})
	ps := attach(t, probe, remote.AttachOptions{When: protocol.WhenNow, Budget: smallBudget})
	bound := remote.ItemSize(nextKind(t, ps, remote.KindAttached)) + remote.ItemSize(nextKind(t, ps, remote.KindSynchronized)) + 32
	if err := probe.Close(); err != nil {
		t.Fatal(err)
	}
	const conn = 1
	c := dialClient(t, h.path, tp, remote.Options{StreamBytes: bound})
	s := attach(t, c, remote.AttachOptions{When: protocol.WhenNow, Budget: smallBudget})
	h.text(big)
	tp.await(t, "the re-attach", func() bool { return len(tp.sentOn(conn, protocol.MethodSessionAttach)) == 2 })
	h.eng.Started(errors.New("the agent would not start"))
	waitFor(t, "the refusal's items", func() bool { return s.QueuedBytes() > bound })
	if n := len(tp.sentOn(conn, protocol.MethodSessionDetach)); n != 0 {
		t.Fatalf("the premise: the queue took the first two items (%d detaches: the stream fell behind)", n)
	}
	closed := make(chan error, 1)
	go func() { closed <- c.Close() }()
	if err := recv(t, closed); err != nil {
		t.Fatal(err)
	}
	var got []remote.Kind
	var last remote.Item
	for {
		it, err := s.Next(tctx(t))
		if errors.Is(err, remote.ErrStreamClosed) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		got, last = append(got, it.Kind), it
		if it.Kind == remote.KindReady && (!it.Ready.StartFailed || it.Ready.Err != "the agent would not start") {
			t.Fatalf("the ready owed: %s", describe(it))
		}
	}
	want := []remote.Kind{remote.KindAttached, remote.KindSynchronized, remote.KindReady, remote.KindError}
	var e *remote.Error
	if fmt.Sprint(got) != fmt.Sprint(want) || !errors.As(last.Err, &e) || e.Reason != protocol.ReasonStartFailed {
		t.Fatalf("the items %v, the last %s; want %v, the last start_failed", got, describe(last), want)
	}
}

// TestARestoreHeavyStreamStaysUnderItsByteBound (X18 8; astra r9, further 2):
// a caller stops reading, and every record no client can fold ends the
// subscription reset{omitted}; each re-attach's reply is a Restore, retaining
// its snapshot and its info document — here mostly the info document, whose
// workspace is long — until Next takes it. The queue counts every payload it
// retains, so what it holds, measured as the host encoded each reply, never
// goes over its bound: once the next Restore would, the stream falls behind.
func TestARestoreHeavyStreamStaysUnderItsByteBound(t *testing.T) {
	h := newHost(t, withLog(agent.EventLogOptions{MaxRecordBytes: 512}), withWorkspace("/"+strings.Repeat("w", 4<<10)))
	tp := newTap(t)
	const bound = 32 << 10
	c := dialClient(t, h.path, tp, remote.Options{StreamBytes: bound})
	s := attach(t, c, remote.AttachOptions{})
	nextKind(t, s, remote.KindAttached)
	nextKind(t, s, remote.KindSynchronized)
	detaches := func() int { return len(tp.sent(protocol.MethodSessionDetach)) }
	for i := 0; detaches() == 0; i++ {
		if i == 64 {
			t.Fatal("the stream never fell behind")
		}
		n := len(tp.sent(protocol.MethodSessionAttach))
		h.text(strings.Repeat("o", 600))
		tp.await(t, "the re-attach answered, or the fall", func() bool {
			re := tp.sent(protocol.MethodSessionAttach)
			return detaches() > 0 || (len(re) > n && len(tp.received(func(l wireLine) bool {
				return l.resp != nil && l.id == re[len(re)-1].id
			})) == 1)
		})
	}
	var fell protocol.DetachParams
	if err := json.Unmarshal(tp.sent(protocol.MethodSessionDetach)[0].params, &fell); err != nil {
		t.Fatal(err)
	}
	// What the queue holds: every Restore before the one that found no room.
	held, restores := 0, 0
	for _, l := range tp.received(func(l wireLine) bool {
		return l.method == protocol.MethodSessionAttach && l.resp != nil && l.resp.Error == nil
	}) {
		var r protocol.AttachResult
		if err := json.Unmarshal(l.resp.Result, &r); err != nil {
			t.Fatal(err)
		}
		if r.Subscription != "s-1" && r.Subscription != fell.Subscription {
			held += len(l.resp.Result)
			restores++
		}
	}
	if restores < 2 {
		t.Fatalf("the premise: %d restores held", restores)
	}
	if held > bound || s.QueuedBytes() > bound {
		t.Fatalf("the queue holds %d restores encoding to %d bytes and counts %d, over its bound of %d",
			restores, held, s.QueuedBytes(), bound)
	}
}

// TestAnItemCountsEveryPayloadItRetains (X18 8; astra r9, further 2): what an
// item counts against the stream's bound is at least what the payloads it
// retains encode to — an event's body, a reply's snapshot and info document
// (catalogs and all), a ready's, an error's message, result and cause.
func TestAnItemCountsEveryPayloadItRetains(t *testing.T) {
	info := protocol.SessionInfo{SessionID: "s", Incarnation: "i", HostID: "h", Workspace: "/w"}
	for i := range 200 {
		info.Catalogs.Models = append(info.Catalogs.Models, protocol.CatalogModel{ID: fmt.Sprintf("model-%d", i), Name: strings.Repeat("n", 40)})
	}
	long := strings.Repeat("x", 4096)
	for _, it := range []remote.Item{
		{Kind: remote.KindEvent, Seq: 1, Body: json.RawMessage(`{"type":"text","text":"` + long + `"}`)},
		{Kind: remote.KindRestore, Reply: &protocol.AttachResult{Subscription: "s-9", Session: info, Ready: true,
			After: protocol.Cursor{Incarnation: "i", Seq: 1 << 63}, Snapshot: json.RawMessage(`{"s":1}`), Reset: protocol.CursorBacklogTooLarge}},
		{Kind: remote.KindReady, Ready: &protocol.ReadyParams{Subscription: "s-9", Session: info, StartFailed: true, Err: long}},
		{Kind: remote.KindError, Err: &remote.Error{Message: long, Cause: long, Result: json.RawMessage(`{"r":"` + long + `"}`)}},
	} {
		n := len(it.Body)
		for _, doc := range []any{it.Reply, it.Ready} {
			if doc != nil {
				b, err := json.Marshal(doc)
				if err != nil {
					t.Fatal(err)
				}
				if string(b) != "null" {
					n += len(b)
				}
			}
		}
		var e *remote.Error
		if errors.As(it.Err, &e) {
			n += len(e.Message) + len(e.Result) + len(e.Cause)
		}
		if got := remote.ItemSize(it); got < n {
			t.Errorf("a %s item counts %d bytes; its payloads encode to %d", it.Kind, got, n)
		}
	}
}
