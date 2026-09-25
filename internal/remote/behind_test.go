package remote_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

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
