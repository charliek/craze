package remote_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/backend"
	"github.com/charliek/craze/internal/control"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/remote"
	"github.com/charliek/craze/internal/tui"
)

// withAsiding serves the Stub as a session that answers side questions
// (tui.AsidingStub; plan 040 §3.8), each piece of its scripted answer sent as
// an aside notice of its own.
func withAsiding() hostOpt {
	return func(c *hostConfig) {
		c.session = func(s *tui.Stub) agent.Session { return tui.AsidingStub{Stub: s} }
		c.asideInterval = control.AsideEachDelta
	}
}

// deltas is an onDelta that records what it is handed; returned, once
// closed, says the Aside it was handed to has returned, and a call after it
// is recorded as late.
type deltas struct {
	mu       sync.Mutex
	got      []string
	late     []string
	returned chan struct{}
	// first, when set, runs on the first call, before it is recorded.
	first func()
	once  sync.Once
}

func newDeltas() *deltas { return &deltas{returned: make(chan struct{})} }

func (d *deltas) on(text string) {
	if d.first != nil {
		d.once.Do(d.first)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	select {
	case <-d.returned:
		d.late = append(d.late, text)
	default:
		d.got = append(d.got, text)
	}
}

func (d *deltas) snapshot() (got, late []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.got...), append([]string(nil), d.late...)
}

type asideAnswer struct {
	r   agent.AsideResult
	err error
}

// ask runs s.Aside on a goroutine of its own, closing d.returned once it has
// returned.
func ask(ctx context.Context, s *remote.Session, req agent.AsideRequest, d *deltas) <-chan asideAnswer {
	ch := make(chan asideAnswer, 1)
	go func() {
		r, err := s.Aside(ctx, req, d.on)
		close(d.returned)
		ch <- asideAnswer{r, err}
	}()
	return ch
}

// asideIDOf is the asideId of a session.aside or session.aside.cancel line.
func asideIDOf(t *testing.T, l wireLine) string {
	t.Helper()
	var p struct {
		AsideID string `json:"asideId"`
	}
	if err := json.Unmarshal(l.params, &p); err != nil {
		t.Fatal(err)
	}
	return p.AsideID
}

// asideNotice is an aside notification line.
func asideNotice(t *testing.T, id string, seq uint64, text string) []byte {
	t.Helper()
	params, err := json.Marshal(protocol.AsideNoticeParams{AsideID: id, Seq: seq, Text: text})
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(protocol.Notification{JSONRPC: protocol.JSONRPCVersion, Method: protocol.NotifyAside, Params: params})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// noSinks fails unless the session's client holds no aside sink.
func noSinks(t *testing.T, s *remote.Session, when string) {
	t.Helper()
	if n := s.Client().AsideSinks(); n != 0 {
		t.Fatalf("%s: the client holds %d aside sinks, want none", when, n)
	}
}

// TestAsideMakesNoCallWithoutTheCapability (plan 040 §3.7): a session whose
// info document does not say aside — before any attach, and a cursor-like
// session's (the plain Stub), which is what an older host's document looks
// like too — is answered agent.ErrAsideUnsupported with nothing written to
// the host. Negative control: an Aside that asks whatever the document says
// writes the request (and the host's refusal still reads as unsupported, so
// only the count catches it).
func TestAsideMakesNoCallWithoutTheCapability(t *testing.T) {
	h := newHost(t)
	tp := newTap(t)
	s := dialSession(t, h.path, tp, remote.SessionOptions{})
	for _, when := range []string{"before the attach", "after it"} {
		if when == "after it" {
			if err := s.Start(tctx(t)); err != nil {
				t.Fatalf("start: %v", err)
			}
			if s.Info().Aside {
				t.Fatal("a cursor-like session's info says aside")
			}
		}
		if _, err := s.Aside(tctx(t), agent.AsideRequest{Question: "q"}, nil); !errors.Is(err, agent.ErrAsideUnsupported) {
			t.Fatalf("%s: %v, want agent.ErrAsideUnsupported", when, err)
		}
		if n := sentMethod(tp, protocol.MethodSessionAside); n != 0 {
			t.Fatalf("%s: the client wrote %d session.aside requests, want none", when, n)
		}
	}
	noSinks(t, s, "after the refusals")
}

// TestAsideStreamsItsAnswerToItsCaller (plan 040 §3.7, §7.3): over the
// socket a session that answers side questions says so (Info().Aside) and an
// Aside hands its caller every piece of the answer, in order, before it
// returns the whole answer with its model and usage; a follow-up carries its
// prior pairs to the session. A second client attached to the same session
// is sent no aside notification — its connection's next line after the
// asides is its own read's reply — and the client holds no sink once each
// Aside has returned. Negative control: a sink that hands each piece over
// twice fails the pieces' comparison.
func TestAsideStreamsItsAnswerToItsCaller(t *testing.T) {
	h := newHost(t, withAsiding())
	tp, otherTap := newTap(t), newTap(t)
	s, _ := started(t, h, tp, remote.SessionOptions{})
	other, _ := started(t, h, otherTap, remote.SessionOptions{})
	if !s.Info().Aside || !other.Info().Aside {
		t.Fatal("a session that answers side questions does not say aside")
	}
	req := agent.AsideRequest{Question: "what is left?"}
	d := newDeltas()
	got := recv(t, ask(tctx(t), s, req, d))
	pieces := tui.StubAsidePieces(req)
	if got.err != nil || got.r.Text != strings.Join(pieces, "") || got.r.Model != "Grok" ||
		got.r.Usage == nil || *got.r.Usage != (agent.AsideUsage{Input: 1000, Output: 4, CacheRead: 800}) {
		t.Fatalf("the answer: %+v, %v", got.r, got.err)
	}
	if deltas, late := d.snapshot(); !reflect.DeepEqual(deltas, pieces) || len(late) != 0 {
		t.Fatalf("the pieces handed over: %q (late %q), want %q", deltas, late, pieces)
	}
	noSinks(t, s, "after the answer")

	follow := agent.AsideRequest{Question: "and then?", Prior: []agent.AsideTurn{{Question: req.Question, Answer: got.r.Text}}}
	d2 := newDeltas()
	got2 := recv(t, ask(tctx(t), s, follow, d2))
	if got2.err != nil || got2.r.Text != strings.Join(tui.StubAsidePieces(follow), "") {
		t.Fatalf("the follow-up: %+v, %v", got2.r, got2.err)
	}
	if asked := h.stub.Asked(); len(asked) != 2 || !reflect.DeepEqual(asked[1], follow) {
		t.Fatalf("the session was asked %+v, want the question and then %+v", asked, follow)
	}
	noSinks(t, s, "after the follow-up")

	// The other client's barrier: anything queued to it during the asides
	// is ahead of this read's reply on its connection.
	if _, err := other.Settings(tctx(t)); err != nil {
		t.Fatal(err)
	}
	if notes := otherTap.received(func(l wireLine) bool { return l.method == protocol.NotifyAside }); len(notes) != 0 {
		t.Fatalf("the other client was sent %d aside notices: %s", len(notes), notes[0].raw)
	}
	if n := sentMethod(tp, protocol.MethodSessionAside); n != 2 {
		t.Fatalf("the client wrote %d session.aside requests, want 2", n)
	}
}

// TestAnAsideIsNeverResent (plan 040 §3.7, §7.4): an aside whose connection
// goes after it was sent — the host answering it, held after its first piece
// — answers remote.ErrConnectionLost, and is not sent again on the next
// connection: the host's session was asked exactly once, and the client wrote
// session.aside once, though the client reconnected and the session serves
// reads again. The sink is gone with it. Negative control: an aside sent as
// a read is, again on ErrConnectionLost, answers the second sending (the hold
// is spent) and fails the error's check.
func TestAnAsideIsNeverResent(t *testing.T) {
	h := newHost(t, withAsiding())
	tp := newTap(t)
	s, _ := started(t, h, tp, remote.SessionOptions{})
	held, release := h.stub.HoldNextAside()
	t.Cleanup(release)
	d := newDeltas()
	first := make(chan struct{})
	d.first = func() { close(first) }
	done := ask(tctx(t), s, agent.AsideRequest{Question: "q"}, d)
	await(t, held, "the host's session to hold the aside")
	await(t, first, "the first piece to reach the caller")
	tp.kill()
	got := recv(t, done)
	if !errors.Is(got.err, remote.ErrConnectionLost) {
		t.Fatalf("an aside whose connection went: %+v, %v; want remote.ErrConnectionLost", got.r, got.err)
	}
	noSinks(t, s, "after the loss")
	// A read on the next connection: the client has reconnected, and any
	// resend would have gone before it.
	if _, err := s.Settings(tctx(t)); err != nil {
		t.Fatalf("a read after the reconnect: %v", err)
	}
	if n := len(h.stub.Asked()); n != 1 {
		t.Fatalf("the host's session was asked %d times, want once", n)
	}
	if sent := tp.sent(protocol.MethodSessionAside); len(sent) != 1 || sent[0].conn != 0 {
		t.Fatalf("the client wrote session.aside %d times, want once, on the first connection", len(sent))
	}
}

// TestAnEndedContextSendsNoAside (plan 040 §3.7): an Aside whose ctx has
// already ended, or carries an epoch the client has left, answers that
// (context.Canceled; backend.ErrStaleEpoch) with nothing sent and nothing
// asked. Negative control: an Aside that looks at its ctx only once the
// request is written sends it.
func TestAnEndedContextSendsNoAside(t *testing.T) {
	h := newHost(t, withAsiding())
	tp := newTap(t)
	s, _ := started(t, h, tp, remote.SessionOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Aside(ctx, agent.AsideRequest{Question: "q"}, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("an ended ctx: %v, want context.Canceled", err)
	}
	stale := backend.WithEpoch(tctx(t), s.Epoch()+1)
	if _, err := s.Aside(stale, agent.AsideRequest{Question: "q"}, nil); !errors.Is(err, backend.ErrStaleEpoch) {
		t.Fatalf("a stale epoch: %v, want backend.ErrStaleEpoch", err)
	}
	if n := sentMethod(tp, protocol.MethodSessionAside); n != 0 {
		t.Fatalf("the client wrote %d session.aside requests, want none", n)
	}
	if n := len(h.stub.Asked()); n != 0 {
		t.Fatalf("the host's session was asked %d times, want never", n)
	}
	noSinks(t, s, "after the refusals")
}

// TestACancelledAsideIsCancelledOnItsConnection (plan 040 §3.7): a ctx that
// ends while the host owes the answer makes Aside send session.aside.cancel
// naming the aside, on the connection the aside went out on, and answer
// context.Canceled once the host has answered the cancel — by which time the
// host has answered the aside aborted, reason context, and no callback runs
// after Aside has returned. Negative control: an Aside that only stops
// waiting sends no cancel, and fails the cancel's count.
func TestACancelledAsideIsCancelledOnItsConnection(t *testing.T) {
	h := newHost(t, withAsiding())
	tp := newTap(t)
	s, _ := started(t, h, tp, remote.SessionOptions{})
	held, release := h.stub.HoldNextAside()
	t.Cleanup(release)
	ctx, cancel := context.WithCancel(tctx(t))
	defer cancel()
	d := newDeltas()
	done := ask(ctx, s, agent.AsideRequest{Question: "q"}, d)
	await(t, held, "the host's session to hold the aside")
	cancel()
	if got := recv(t, done); !errors.Is(got.err, context.Canceled) {
		t.Fatalf("a cancelled aside: %+v, %v; want context.Canceled", got.r, got.err)
	}
	asides, cancels := tp.sent(protocol.MethodSessionAside), tp.sent(protocol.MethodSessionAsideCancel)
	if len(asides) != 1 || len(cancels) != 1 {
		t.Fatalf("the client wrote %d asides and %d cancels, want one of each", len(asides), len(cancels))
	}
	if cancels[0].conn != asides[0].conn || asideIDOf(t, cancels[0]) != asideIDOf(t, asides[0]) {
		t.Fatalf("the cancel names %s on conn %d, the aside was %s on conn %d",
			asideIDOf(t, cancels[0]), cancels[0].conn, asideIDOf(t, asides[0]), asides[0].conn)
	}
	aborted := tp.received(func(l wireLine) bool {
		return l.id == asides[0].id && l.resp != nil && l.resp.Error != nil &&
			l.resp.Error.Data.Code == protocol.CodeAborted && l.resp.Error.Data.Reason == protocol.ReasonContext
	})
	if len(aborted) != 1 {
		t.Fatal("the host had not answered the aside aborted, context when Aside returned")
	}
	if _, late := d.snapshot(); len(late) != 0 {
		t.Fatalf("pieces handed over after Aside returned: %q", late)
	}
	noSinks(t, s, "after the cancel")
}

// TestACancelHeldAtTheWriteLockIsBounded (plan 040 §3.7, astra P1): the
// cancel's whole cost is bounded from Aside's side. With the connection's
// write lock held when the caller cancels — as a request mid-write to a peer
// that has stopped reading holds it — Aside answers context.Canceled within
// AsideCancelWait (and slack), its sink retired and taken off, nothing yet
// written; once the lock is free the cancel goes out all the same, on the
// aside's connection, and the host answers the aside aborted. Negative
// control: a cancel sent on Aside's own goroutine waits at the lock, and
// Aside has not returned at the bound.
func TestACancelHeldAtTheWriteLockIsBounded(t *testing.T) {
	h := newHost(t, withAsiding())
	tp := newTap(t)
	s, _ := started(t, h, tp, remote.SessionOptions{})
	held, release := h.stub.HoldNextAside()
	t.Cleanup(release)
	ctx, cancel := context.WithCancel(tctx(t))
	defer cancel()
	d := newDeltas()
	done := ask(ctx, s, agent.AsideRequest{Question: "q"}, d)
	await(t, held, "the host's session to hold the aside")
	unlock := s.Client().HoldWrites()
	t.Cleanup(unlock) // before the client's own cleanup, which waits for the cancel's goroutine
	cancel()
	bound := remote.AsideCancelWait + 3*time.Second
	select {
	case got := <-done:
		if !errors.Is(got.err, context.Canceled) {
			t.Fatalf("a cancelled aside: %+v, %v; want context.Canceled", got.r, got.err)
		}
	case <-time.After(bound):
		t.Fatalf("Aside had not returned %s after its ctx ended, the write lock held", bound)
	}
	noSinks(t, s, "once Aside returned, its cancel unwritten")
	if n := sentMethod(tp, protocol.MethodSessionAsideCancel); n != 0 {
		t.Fatalf("%d cancels written with the write lock held", n)
	}
	unlock()
	tp.await(t, "the cancel written once the lock is free", func() bool {
		return sentMethod(tp, protocol.MethodSessionAsideCancel) == 1
	})
	asides, cancels := tp.sent(protocol.MethodSessionAside), tp.sent(protocol.MethodSessionAsideCancel)
	if len(asides) != 1 || cancels[0].conn != asides[0].conn || asideIDOf(t, cancels[0]) != asideIDOf(t, asides[0]) {
		t.Fatalf("%d asides; the cancel names %s on conn %d", len(asides), asideIDOf(t, cancels[0]), cancels[0].conn)
	}
	tp.await(t, "the host's aborted answer to the aside", func() bool {
		return len(tp.received(func(l wireLine) bool {
			return l.id == asides[0].id && l.resp != nil && l.resp.Error != nil &&
				l.resp.Error.Data.Code == protocol.CodeAborted && l.resp.Error.Data.Reason == protocol.ReasonContext
		})) == 1
	})
	if _, late := d.snapshot(); len(late) != 0 {
		t.Fatalf("pieces handed over after Aside returned: %q", late)
	}
}

// TestNoCallbackRunsOnceAsideHasReturned (plan 040 §3.7, P4): a notice the
// reader has matched to the aside's sink, read just behind the aside's reply,
// is held between that match and its hand-over until Aside has returned —
// Aside itself held, once the reply has reached it, until the reader holds
// that notice: the schedule where the sink is still found. The sink was
// retired before Aside returned, so the notice is dropped and the caller's
// callback never runs after the return. Negative control: a sink that is
// never retired hands the notice over after Aside has returned.
func TestNoCallbackRunsOnceAsideHasReturned(t *testing.T) {
	h := newHost(t, withAsiding())
	tp := newTap(t)
	matched, returned := make(chan struct{}), make(chan struct{})
	var armed atomic.Bool
	s := dialSessionHooked(t, h.path, tp, remote.SessionOptions{}, remote.TestHooks{
		Answered: func(m string) {
			if m == protocol.MethodSessionAside && armed.Load() {
				await(t, matched, "the late notice to be matched to its sink")
			}
		},
		Delivering: func(_ string, seq uint64) {
			if seq == 99 {
				close(matched)
				await(t, returned, "Aside to return")
			}
		},
	})
	if err := s.Start(tctx(t)); err != nil {
		t.Fatal(err)
	}
	readKind(t, s, backend.ItemRestore)
	var mu sync.Mutex
	var id string
	tp.setRewriteIn(func(l wireLine) [][]byte {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case l.method == protocol.NotifyAside && id == "":
			var p protocol.AsideNoticeParams
			_ = json.Unmarshal(l.params, &p)
			id = p.AsideID
		case l.method == protocol.MethodSessionAside && l.resp != nil && id != "":
			return [][]byte{l.raw, asideNotice(t, id, 99, "late")}
		}
		return nil
	})
	armed.Store(true)
	d := newDeltas()
	req := agent.AsideRequest{Question: "q"}
	got := recv(t, ask(tctx(t), s, req, d))
	close(returned)
	if got.err != nil {
		t.Fatalf("the aside: %v", got.err)
	}
	// The reader handles a connection's lines in order: once a read's reply
	// is in, the late notice has been dealt with.
	if _, err := s.Settings(tctx(t)); err != nil {
		t.Fatal(err)
	}
	deltas, late := d.snapshot()
	if len(late) != 0 {
		t.Fatalf("a callback ran after Aside returned: %q", late)
	}
	if !reflect.DeepEqual(deltas, tui.StubAsidePieces(req)) {
		t.Fatalf("the pieces: %q", deltas)
	}
	noSinks(t, s, "after the answer")
}

// TestASlowCallbackDelaysTheStreamAndLosesNothing (plan 040 §3.7, §8): the
// caller's onDelta runs inline on the connection's reader, so while it is
// slow — held here on its first piece while the host publishes on the
// session's stream — everything the connection carries waits behind it; once
// it returns the answer arrives whole, every piece handed over once and in
// order, and the stream hands up the events published meanwhile, in order, on
// the same connection. Negative control: a sink that hands each piece over on
// a goroutine of its own lets the later pieces overtake the held first one,
// and fails the pieces' comparison.
func TestASlowCallbackDelaysTheStreamAndLosesNothing(t *testing.T) {
	h := newHost(t, withAsiding())
	tp := newTap(t)
	s, _ := started(t, h, tp, remote.SessionOptions{})
	entered, slow := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() { closeOnce(slow) })
	d := newDeltas()
	d.first = func() {
		close(entered)
		<-slow
	}
	req := agent.AsideRequest{Question: "q"}
	done := ask(tctx(t), s, req, d)
	await(t, entered, "the first piece to reach the slow callback")
	for _, text := range []string{"main one", "main two", "main three"} {
		h.text(text)
	}
	h.head()
	close(slow)
	got := recv(t, done)
	if got.err != nil || got.r.Text != strings.Join(tui.StubAsidePieces(req), "") {
		t.Fatalf("the answer behind a slow callback: %+v, %v", got.r, got.err)
	}
	if deltas, _ := d.snapshot(); !reflect.DeepEqual(deltas, tui.StubAsidePieces(req)) {
		t.Fatalf("the pieces behind a slow callback: %q", deltas)
	}
	for _, text := range []string{"main one", "main two", "main three"} {
		it := readItem(t, s)
		if !textItem(text)(it) {
			t.Fatalf("the stream handed up %s, want the text %q", describeItem(it), text)
		}
	}
	if tp.dialCount() != 1 {
		t.Fatalf("the client dialled %d times: the slow callback cost the connection", tp.dialCount())
	}
	noSinks(t, s, "after the answer")
}

// TestStaleAsideNoticesAreDropped (plan 040 §3.7): a notice naming an aside
// this client has no sink for is dropped, the connection kept; and a notice
// naming a live aside read on another connection than the one the aside went
// out on — after a reconnect, while Aside is held between its connection's
// loss and its return — is never handed over: the sink is bound to its
// connection. Negative control: a sink that takes a notice from whichever
// connection reads it hands the other connection's text to the caller.
func TestStaleAsideNoticesAreDropped(t *testing.T) {
	h := newHost(t, withAsiding())
	tp := newTap(t)
	t.Cleanup(tp.releaseDials)
	var armed atomic.Bool
	readOther, otherRead := make(chan struct{}), make(chan struct{})
	s := dialSessionHooked(t, h.path, tp, remote.SessionOptions{}, remote.TestHooks{
		Answered: func(m string) {
			if m == protocol.MethodSessionAside && armed.Load() {
				await(t, otherRead, "the other connection's notice to be read")
			}
		},
		Delivering: func(_ string, seq uint64) {
			// The second of the other connection's notices: the reader has
			// dealt with the first.
			if seq == 8 {
				close(readOther)
			}
		},
	})
	if err := s.Start(tctx(t)); err != nil {
		t.Fatal(err)
	}
	readKind(t, s, backend.ItemRestore)

	// A notice for an aside the client never asked: dropped, the connection
	// and the answer intact.
	var mu sync.Mutex
	injected := false
	tp.setRewriteIn(func(l wireLine) [][]byte {
		mu.Lock()
		defer mu.Unlock()
		if l.method == protocol.NotifyAside && !injected {
			injected = true
			return [][]byte{asideNotice(t, "never-asked", 1, "stray"), l.raw}
		}
		return nil
	})
	req := agent.AsideRequest{Question: "q"}
	d := newDeltas()
	got := recv(t, ask(tctx(t), s, req, d))
	if deltas, _ := d.snapshot(); got.err != nil || !reflect.DeepEqual(deltas, tui.StubAsidePieces(req)) {
		t.Fatalf("an aside behind a stray notice: %v, pieces %q", got.err, deltas)
	}
	if tp.dialCount() != 1 {
		t.Fatal("a stray notice cost the connection")
	}

	// A live aside's id on the next connection.
	held, release := h.stub.HoldNextAside()
	t.Cleanup(release)
	var id string
	tp.setRewriteIn(func(l wireLine) [][]byte {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case l.conn == 0 && l.method == protocol.NotifyAside:
			var p protocol.AsideNoticeParams
			_ = json.Unmarshal(l.params, &p)
			id = p.AsideID
		case l.conn == 1 && id != "" && l.resp != nil && l.method == protocol.MethodSessionAttach:
			return [][]byte{l.raw, asideNotice(t, id, 7, "from another connection"), asideNotice(t, id, 8, "and again")}
		}
		return nil
	})
	armed.Store(true)
	d2 := newDeltas()
	first := make(chan struct{})
	d2.first = func() { close(first) }
	done := ask(tctx(t), s, req, d2)
	await(t, held, "the host's session to hold the aside")
	await(t, first, "the first piece to reach the caller")
	tp.kill()
	await(t, readOther, "the next connection to read the live aside's id")
	close(otherRead)
	if got := recv(t, done); !errors.Is(got.err, remote.ErrConnectionLost) {
		t.Fatalf("the aside whose connection went: %v", got.err)
	}
	if deltas, late := d2.snapshot(); len(deltas) != 1 || len(late) != 0 {
		t.Fatalf("the pieces handed over: %q, late %q; want the first connection's one", deltas, late)
	}
	noSinks(t, s, "after the loss")
}

// TestAsideAnswersAreJudgedAgainstTheirBinding (plan 040 §3.7): an aside
// whose answer has reached its caller — held there — while the connection
// goes and the reconnect adopts a new identity answers backend.ErrStaleEpoch:
// never an answer from a session the caller is no longer bound to. Negative
// control: an Aside with no post-answer identity check hands up the old
// session's answer.
func TestAsideAnswersAreJudgedAgainstTheirBinding(t *testing.T) {
	h := newHost(t, withAsiding())
	tp := newTap(t)
	t.Cleanup(tp.releaseDials)
	var arm atomic.Bool
	reached, resume := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() { closeOnce(resume) })
	s := dialSessionHooked(t, h.path, tp, remote.SessionOptions{}, remote.TestHooks{Answered: func(m string) {
		if m == protocol.MethodSessionAside && arm.Load() {
			close(reached)
			<-resume
		}
	}})
	if err := s.Start(tctx(t)); err != nil {
		t.Fatal(err)
	}
	readKind(t, s, backend.ItemRestore)
	epoch := s.Epoch()
	arm.Store(true)
	done := ask(backend.WithEpoch(tctx(t), epoch), s, agent.AsideRequest{Question: "q"}, newDeltas())
	await(t, reached, "the answer to reach its caller")
	arm.Store(false)
	retire(t, h, tp, s)()
	waitFor(t, "the adoption of a new identity", func() bool { return s.Epoch() != epoch })
	close(resume)
	if got := recv(t, done); !errors.Is(got.err, backend.ErrStaleEpoch) {
		t.Fatalf("an answer on E returned after E+1: %+v, %v; want backend.ErrStaleEpoch", got.r, got.err)
	}
	noSinks(t, s, "after the stale answer")
}

// TestCloseRetiresEveryAsideSink (plan 040 §3.7): a client closed while an
// aside is in flight retires and drops its sink at once, the Aside answers
// the client's close, and nothing is handed over after it returns. Negative
// control: a Close that leaves the sinks fails the count taken right after
// it, before the Aside has returned.
func TestCloseRetiresEveryAsideSink(t *testing.T) {
	h := newHost(t, withAsiding())
	tp := newTap(t)
	var arm atomic.Bool
	reached, resume := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() { closeOnce(resume) })
	s := dialSessionHooked(t, h.path, tp, remote.SessionOptions{}, remote.TestHooks{Answered: func(m string) {
		if m == protocol.MethodSessionAside && arm.Load() {
			close(reached)
			<-resume
		}
	}})
	if err := s.Start(tctx(t)); err != nil {
		t.Fatal(err)
	}
	readKind(t, s, backend.ItemRestore)
	held, release := h.stub.HoldNextAside()
	t.Cleanup(release)
	arm.Store(true)
	d := newDeltas()
	done := ask(tctx(t), s, agent.AsideRequest{Question: "q"}, d)
	await(t, held, "the host's session to hold the aside")
	if n := s.Client().AsideSinks(); n != 1 {
		t.Fatalf("an aside in flight has %d sinks, want 1", n)
	}
	closed := make(chan error, 1)
	go func() { closed <- s.Client().Close() }()
	await(t, reached, "the close to reach the aside")
	noSinks(t, s, "once the client has closed, before the Aside returns")
	close(resume)
	if got := recv(t, done); !errors.Is(got.err, remote.ErrClosed) {
		t.Fatalf("an aside its client's close cut off: %+v, %v; want remote.ErrClosed", got.r, got.err)
	}
	if err := recv(t, closed); err != nil {
		t.Fatal(err)
	}
	if _, late := d.snapshot(); len(late) != 0 {
		t.Fatalf("pieces handed over after Aside returned: %q", late)
	}
}
