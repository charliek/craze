package control_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/control"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/tui"
)

// asidingSession is the host's Stub as a session that answers side questions
// (plan 040 §3.2) with answer, counting the calls that reached it.
type asidingSession struct {
	*tui.Stub
	calls  atomic.Int32
	answer func(ctx context.Context, req agent.AsideRequest, onDelta func(string)) (agent.AsideResult, error)
}

func (s *asidingSession) Asides() bool { return true }

func (s *asidingSession) Aside(ctx context.Context, req agent.AsideRequest, onDelta func(string)) (agent.AsideResult, error) {
	s.calls.Add(1)
	return s.answer(ctx, req, onDelta)
}

// withAsides builds the host over the Stub as s.
func withAsides(s *asidingSession) hostOpt {
	return withSession(func(st *tui.Stub) agent.Session {
		s.Stub = st
		return s
	})
}

func withAsideInterval(d time.Duration) hostOpt {
	return func(c *hostConfig) { c.opts.AsideInterval = d }
}

func withAsideCeiling(d time.Duration) hostOpt {
	return func(c *hostConfig) { c.asideCeiling = d }
}

// streamed answers with pieces, one delta each, and their concatenation.
func streamed(pieces ...string) func(context.Context, agent.AsideRequest, func(string)) (agent.AsideResult, error) {
	return func(_ context.Context, _ agent.AsideRequest, onDelta func(string)) (agent.AsideResult, error) {
		for _, p := range pieces {
			onDelta(p)
		}
		return agent.AsideResult{Text: strings.Join(pieces, ""), Model: "M",
			Usage: &agent.AsideUsage{Input: 10, Output: 2, CacheRead: 8}}, nil
	}
}

// holding is an answer that hands each call's context to entered and waits
// for a text on release — or for its context, answering then with an error
// of its own that does not wrap the context's (a closed pipe), as a live
// session's would.
type holding struct {
	entered chan context.Context
	release chan string
}

func newHolding() *holding {
	return &holding{entered: make(chan context.Context, 8), release: make(chan string, 8)}
}

func (h *holding) answer(ctx context.Context, _ agent.AsideRequest, _ func(string)) (agent.AsideResult, error) {
	h.entered <- ctx
	select {
	case text := <-h.release:
		return agent.AsideResult{Text: text}, nil
	case <-ctx.Done():
		return agent.AsideResult{}, errors.New("pipe closed")
	}
}

// enter is the context of the next call the session is in.
func (h *holding) enter(t *testing.T) context.Context {
	t.Helper()
	select {
	case ctx := <-h.entered:
		return ctx
	case <-time.After(watchdog):
		t.Fatal("no aside reached the session")
		return nil
	}
}

func asideParams(h *host, id, question string) protocol.AsideParams {
	return protocol.AsideParams{SessionID: sid(h), AsideID: id, Question: question}
}

func cancelParams(h *host, id string) protocol.AsideCancelParams {
	return protocol.AsideCancelParams{SessionID: sid(h), AsideID: id}
}

// asideNotices decodes notes, which must all be aside notifications.
func asideNotices(t *testing.T, notes []*protocol.Notification) []protocol.AsideNoticeParams {
	t.Helper()
	var out []protocol.AsideNoticeParams
	for _, n := range notes {
		if n.Method != protocol.NotifyAside {
			t.Fatalf("a %s notification among the aside's: %s", n.Method, n.Params)
		}
		var p protocol.AsideNoticeParams
		if err := json.Unmarshal(n.Params, &p); err != nil {
			t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

// checkNotices fails unless notices are id's, numbered from 1 in order, each
// at most AsideNoticeMax bytes of whole characters, and together text.
func checkNotices(t *testing.T, notices []protocol.AsideNoticeParams, id, text string) {
	t.Helper()
	var b strings.Builder
	for i, n := range notices {
		if n.AsideID != id || n.Seq != uint64(i+1) {
			t.Fatalf("notice %d: %s seq %d, want %s seq %d", i, n.AsideID, n.Seq, id, i+1)
		}
		if len(n.Text) > protocol.AsideNoticeMax || n.Text == "" || !utf8.ValidString(n.Text) {
			t.Fatalf("notice %d: %d bytes, valid %v", i, len(n.Text), utf8.ValidString(n.Text))
		}
		b.WriteString(n.Text)
	}
	if b.String() != text {
		t.Fatalf("the notices make %d bytes, the answer is %d", b.Len(), len(text))
	}
}

// TestAsideIsServedWhereTheSessionAnswersSideQuestions (plan 040 §3.2): a
// session that answers no side questions — the plain Stub, as cursor's — has
// no aside in its info document, and both methods are refused unsupported,
// reason aside_unsupported, whatever their params; one that does says aside:
// true and answers, its answer streamed as aside notices to the connection
// that asked — numbered from 1, at most 4 KiB each and cut between
// characters, one per piece with AsideEachDelta, every one before the reply
// — and whole in the reply, with its model and usage. A second client attached
// to the session sees no notice, and the aside publishes nothing: the
// session's head does not move. With the server's interval the pieces are
// sent together, and still make the answer.
func TestAsideIsServedWhereTheSessionAnswersSideQuestions(t *testing.T) {
	h := newHost(t)
	a := h.dial()
	a.sayHello(nil)
	if r := a.attach(attachParams(h)); r.Session.Capabilities.Aside {
		t.Fatalf("a session with no side questions says aside: %+v", r.Session.Capabilities)
	}
	a.note(protocol.NotifySynchronized)
	refusedWith(t, a.call(protocol.MethodSessionAside, asideParams(h, "not an id", "q")), protocol.RPCRefused,
		protocol.CodeUnsupported, protocol.ReasonAsideUnsupported)
	refusedWith(t, a.call(protocol.MethodSessionAsideCancel, cancelParams(h, "x")), protocol.RPCRefused,
		protocol.CodeUnsupported, protocol.ReasonAsideUnsupported)

	wide := strings.Repeat("é", 3000) // 6000 bytes: two notices, cut between characters
	pieces := []string{"a", wide, "z"}
	s := &asidingSession{answer: streamed(pieces...)}
	h = newHost(t, withAsides(s), withAsideInterval(control.AsideEachDelta))
	a, b := h.dial(), h.dial()
	a.sayHello(nil)
	b.sayHello(nil)
	if r := a.attach(attachParams(h)); !r.Session.Capabilities.Aside {
		t.Fatalf("a session with side questions does not say so: %+v", r.Session.Capabilities)
	}
	a.note(protocol.NotifySynchronized)
	b.attach(attachParams(h))
	b.note(protocol.NotifySynchronized)
	_, before := a.sync(h)

	id := a.send(protocol.MethodSessionAside, asideParams(h, "a1", "why?"))
	notes, resp := a.until(id)
	notices := asideNotices(t, notes)
	text := strings.Join(pieces, "")
	checkNotices(t, notices, "a1", text)
	if len(notices) != 4 || notices[0].Text != "a" || len(notices[1].Text) != protocol.AsideNoticeMax || notices[3].Text != "z" {
		t.Fatalf("one notice per piece, the wide one cut in two: %d notices", len(notices))
	}
	got := ok[protocol.AsideResult](t, resp)
	if got.Text != text || got.Model != "M" || got.Usage == nil || *got.Usage != (protocol.AsideUsageResult{Input: 10, Output: 2, CacheRead: 8}) {
		t.Fatalf("the answer: %d bytes, model %q, usage %+v", len(got.Text), got.Model, got.Usage)
	}
	bNotes, _ := b.sync(h)
	for _, n := range bNotes {
		if n.Method == protocol.NotifyAside {
			t.Fatalf("the other client was sent an aside notice: %s", n.Params)
		}
	}
	if _, after := a.sync(h); after != before {
		t.Fatalf("the session's head moved from %d to %d over an aside", before, after)
	}
	if n := h.srv.Asides(); n != 0 {
		t.Fatalf("%d asides reserved once answered", n)
	}

	// The server's interval: the pieces are coalesced, and still the answer.
	big := strings.Repeat("x", 10000)
	s = &asidingSession{answer: streamed("one ", big, " two", " three")}
	h = newHost(t, withAsides(s))
	a = h.dial()
	a.sayHello(nil)
	notes, resp = a.until(a.send(protocol.MethodSessionAside, asideParams(h, "a2", "q")))
	checkNotices(t, asideNotices(t, notes), "a2", "one "+big+" two three")
	if ok[protocol.AsideResult](t, resp).Text != "one "+big+" two three" {
		t.Fatal("the coalesced answer's reply is not the answer")
	}
}

// TestAsideIDsAreReservedOnTheReader (plan 040 §3.2, P1): an asideId that is
// not session.create's requestId shape — missing, empty, a space in it, 65
// characters — is -32602, bad_request, and one an aside of the connection in
// flight holds is bad_request too: none reaches the session. Once the aside
// has answered, its id is free again, and nothing stays reserved.
func TestAsideIDsAreReservedOnTheReader(t *testing.T) {
	held := newHolding()
	s := &asidingSession{answer: held.answer}
	h := newHost(t, withAsides(s))
	a := h.dial()
	a.sayHello(nil)
	for _, p := range []any{
		map[string]any{"sessionId": sid(h), "question": "q"},
		asideParams(h, "", "q"),
		asideParams(h, "a b", "q"),
		asideParams(h, strings.Repeat("a", protocol.RequestIDMax+1), "q"),
	} {
		refusedWith(t, a.call(protocol.MethodSessionAside, p), protocol.RPCInvalidParams, protocol.CodeBadRequest, protocol.ReasonBadRequest)
	}
	if n := s.calls.Load(); n != 0 {
		t.Fatalf("%d refused asides reached the session", n)
	}
	id := a.send(protocol.MethodSessionAside, asideParams(h, "x.1_-Z", "q"))
	held.enter(t)
	refusedWith(t, a.call(protocol.MethodSessionAside, asideParams(h, "x.1_-Z", "again")), protocol.RPCRefused,
		protocol.CodeBadRequest, protocol.ReasonBadRequest)
	held.release <- "first"
	if got := ok[protocol.AsideResult](t, a.reply(id)); got.Text != "first" {
		t.Fatalf("the first aside: %+v", got)
	}
	held.release <- "second"
	if got := ok[protocol.AsideResult](t, a.call(protocol.MethodSessionAside, asideParams(h, "x.1_-Z", "again"))); got.Text != "second" {
		t.Fatalf("the id used again: %+v", got)
	}
	if n, calls := h.srv.Asides(), s.calls.Load(); n != 0 || calls != 2 {
		t.Fatalf("%d asides reserved, %d asked; want 0 and 2", n, calls)
	}
}

// TestACancelOvertakesItsAsideBeforeTheEngine (plan 040 §3.2, P1): an aside
// held after its params are accepted and before it calls the engine
// (BeforeAside) is cancelled by a session.aside.cancel read behind it: it
// answers aborted, reason context, without the session being asked, and the
// cancel's {} follows its answer. Negative control: a reservation made by the
// handler rather than on the reader would let the cancel find nothing — its
// {} first, and the aside asked.
func TestACancelOvertakesItsAsideBeforeTheEngine(t *testing.T) {
	inHook := make(chan struct{}, 1)
	hooks := control.TestHooks{BeforeAside: func(ctx context.Context, _ string) {
		inHook <- struct{}{}
		select {
		case <-ctx.Done():
		case <-time.After(watchdog):
		}
	}}
	s := &asidingSession{answer: streamed("never")}
	h := newHost(t, withAsides(s), withHooks(hooks))
	a := h.dial()
	a.sayHello(nil)
	id := a.send(protocol.MethodSessionAside, asideParams(h, "x", "q"))
	await(t, inHook, "the aside's handler at the engine's door")
	cid := a.send(protocol.MethodSessionAsideCancel, cancelParams(h, "x"))
	refusedWith(t, a.reply(id), protocol.RPCRefused, protocol.CodeAborted, protocol.ReasonContext)
	ok[protocol.Empty](t, a.reply(cid))
	if n, r := s.calls.Load(), h.srv.Asides(); n != 0 || r != 0 {
		t.Fatalf("the session was asked %d times, %d asides reserved; want 0 and 0", n, r)
	}
}

// TestAsideCancelIsTheConnectionsOwn (plan 040 §3.2): a cancel cancels only
// an aside of its own connection — another connection's cancel of the same
// id answers {} and the aside goes on — and an unknown id answers {}, while
// one that is not an asideId's shape is -32602. The connection's own cancel
// of an aside the session is in ends it: aborted, reason context, though the
// session answered with an error of its own, and the cancel's {} after it.
func TestAsideCancelIsTheConnectionsOwn(t *testing.T) {
	held := newHolding()
	s := &asidingSession{answer: held.answer}
	h := newHost(t, withAsides(s))
	a, b := h.dial(), h.dial()
	a.sayHello(nil)
	b.sayHello(nil)
	id := a.send(protocol.MethodSessionAside, asideParams(h, "x", "q"))
	ctx := held.enter(t)
	ok[protocol.Empty](t, b.call(protocol.MethodSessionAsideCancel, cancelParams(h, "x")))
	ok[protocol.Empty](t, a.reply(a.send(protocol.MethodSessionAsideCancel, cancelParams(h, "unknown"))))
	refusedWith(t, a.reply(a.send(protocol.MethodSessionAsideCancel, cancelParams(h, "a b"))), protocol.RPCInvalidParams,
		protocol.CodeBadRequest, protocol.ReasonBadRequest)
	if ctx.Err() != nil {
		t.Fatal("another connection's cancel, or an unknown id's, ended the aside")
	}
	cid := a.send(protocol.MethodSessionAsideCancel, cancelParams(h, "x"))
	refusedWith(t, a.reply(id), protocol.RPCRefused, protocol.CodeAborted, protocol.ReasonContext)
	ok[protocol.Empty](t, a.reply(cid))
	if n := h.srv.Asides(); n != 0 {
		t.Fatalf("%d asides reserved", n)
	}
}

// TestADelayedCancelLeavesAReusedIDAlone (plan 040 §3.2, astra P2): a
// session.aside.cancel cancels the aside in flight under its id when its line
// was read, and no other. A cancel read while aside x is in flight, its
// handler held (BeforeAsideCancel) while x answers and the client asks a
// second aside under the same id, cancels nothing once it runs: the second
// aside's context lives on, the cancel's {} comes at once, and the second
// aside answers. Negative control: a cancel that looks its id up on its
// handler finds the second aside and cancels it — that aside's aborted comes
// where the cancel's {} belongs.
func TestADelayedCancelLeavesAReusedIDAlone(t *testing.T) {
	inHook, resume := make(chan struct{}, 1), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(resume) }) })
	hooks := control.TestHooks{BeforeAsideCancel: func(string) {
		inHook <- struct{}{}
		<-resume
	}}
	held := newHolding()
	s := &asidingSession{answer: held.answer}
	h := newHost(t, withAsides(s), withHooks(hooks))
	a := h.dial()
	a.sayHello(nil)
	id := a.send(protocol.MethodSessionAside, asideParams(h, "x", "first"))
	held.enter(t)
	cid := a.send(protocol.MethodSessionAsideCancel, cancelParams(h, "x"))
	await(t, inHook, "the cancel's handler, its line read")
	held.release <- "first"
	if got := ok[protocol.AsideResult](t, a.reply(id)); got.Text != "first" {
		t.Fatalf("the first aside: %+v", got)
	}
	id = a.send(protocol.MethodSessionAside, asideParams(h, "x", "second"))
	second := held.enter(t)
	once.Do(func() { close(resume) })
	ok[protocol.Empty](t, a.reply(cid))
	if second.Err() != nil {
		t.Fatal("the delayed cancel ended the aside that reused its id")
	}
	held.release <- "second"
	if got := ok[protocol.AsideResult](t, a.reply(id)); got.Text != "second" {
		t.Fatalf("the second aside: %+v", got)
	}
	if n, calls := h.srv.Asides(), s.calls.Load(); n != 0 || calls != 2 {
		t.Fatalf("%d asides reserved, %d asked; want 0 and 2", n, calls)
	}
}

// TestAsideCeilingEndsIt (plan 040 §3.2): an aside past the server's bound
// (10 minutes in production, moved here) ends on its context's deadline and
// answers aborted, reason context.
func TestAsideCeilingEndsIt(t *testing.T) {
	var deadline atomic.Bool
	s := &asidingSession{answer: func(ctx context.Context, _ agent.AsideRequest, _ func(string)) (agent.AsideResult, error) {
		select {
		case <-ctx.Done():
			deadline.Store(errors.Is(ctx.Err(), context.DeadlineExceeded))
			return agent.AsideResult{}, ctx.Err()
		case <-time.After(watchdog):
			return agent.AsideResult{Text: "the ceiling never came"}, nil
		}
	}}
	h := newHost(t, withAsides(s), withAsideCeiling(20*time.Millisecond))
	a := h.dial()
	a.sayHello(nil)
	refusedWith(t, a.call(protocol.MethodSessionAside, asideParams(h, "x", "q")), protocol.RPCRefused, protocol.CodeAborted, protocol.ReasonContext)
	if !deadline.Load() {
		t.Fatal("the aside ended, and not on its deadline")
	}
}

// TestAsideEndsWithItsConnection (plan 040 §3.2, P9): a connection the host
// finds closed — here a notice it could not write to a peer that has gone —
// cancels its asides: the session's call sees its context end, and nothing
// stays reserved. One whose peer only half-closes keeps its admitted aside
// running: the session's context is live after the server has read the EOF,
// and the answer is written before the connection closes.
func TestAsideEndsWithItsConnection(t *testing.T) {
	held := newHolding()
	gone := make(chan struct{})
	ended := make(chan struct{})
	s := &asidingSession{answer: func(ctx context.Context, req agent.AsideRequest, onDelta func(string)) (agent.AsideResult, error) {
		if req.Question != "hard" {
			return held.answer(ctx, req, onDelta)
		}
		held.entered <- ctx
		<-gone
		onDelta("to nobody")
		select {
		case <-ctx.Done():
			close(ended)
			return agent.AsideResult{}, errors.New("pipe closed")
		case <-time.After(watchdog):
			return agent.AsideResult{Text: "never cancelled"}, nil
		}
	}}
	h := newHost(t, withAsides(s))
	a := h.dial()
	a.sayHello(nil)
	a.send(protocol.MethodSessionAside, asideParams(h, "x", "hard"))
	held.enter(t)
	a.close()
	close(gone)
	await(t, ended, "a closed connection's aside cancelled")
	waitFor(t, "the closed connection's aside to leave its handler", func() bool { return h.srv.Asides() == 0 && h.srv.Handlers() == 0 })

	b := h.dial()
	b.sayHello(nil)
	id := b.send(protocol.MethodSessionAside, asideParams(h, "y", "q"))
	ctx := held.enter(t)
	if err := b.nc.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	// The reader's slot for its next line is given back once it reads the
	// EOF: then only the aside is admitted.
	waitFor(t, "the server to read the half-close", func() bool {
		ad := h.srv.Admitted()
		return len(ad) == 1 && ad[0] == 1
	})
	if ctx.Err() != nil {
		t.Fatal("a half-close cancelled the admitted aside")
	}
	held.release <- "answered"
	if got := ok[protocol.AsideResult](t, b.reply(id)); got.Text != "answered" {
		t.Fatalf("the half-closed connection's answer: %+v", got)
	}
	b.expectClosed()
}

// TestAsidesInASaturatedConnectionLeakNothing (plan 040 §3.2, capacity): with
// the writer held on an aside's first notice, a connection's admission slots
// fill — the aside, one more in the session, and fourteen refused busy (two
// in flight on the session), their answers unwritten — and the cancel behind
// them waits to be read. Once the writer goes on, every line is written in
// order (the aside's notice, then its answer, then the cancel's {}), the
// cancel ends the aside, and the connection is as it was: nothing reserved,
// only the reader's slot taken, and the next aside answered.
func TestAsidesInASaturatedConnectionLeakNothing(t *testing.T) {
	var once sync.Once
	holdWriter, writerHeld := make(chan struct{}), make(chan struct{})
	full := make(chan struct{}, 1)
	var refusals atomic.Int32
	refused := make(chan struct{})
	hooks := control.TestHooks{
		// Every busy refusal has left the engine before the cancel can end
		// the first aside and make room for one of them.
		BeforeReply: func(method string) {
			if method == protocol.MethodSessionAside && refusals.Add(1) == protocol.RequestsPerConnection-2 {
				close(refused)
			}
		},
		BeforeWrite: func(line []byte) {
			if bytes.Contains(line, []byte(`"method":"aside"`)) {
				once.Do(func() {
					close(writerHeld)
					<-holdWriter
				})
			}
		},
		AdmissionFull: func() {
			select {
			case full <- struct{}{}:
			default:
			}
		},
	}
	held := newHolding()
	s := &asidingSession{answer: func(ctx context.Context, req agent.AsideRequest, onDelta func(string)) (agent.AsideResult, error) {
		if req.Question == "first" {
			onDelta("piece")
		}
		return held.answer(ctx, req, onDelta)
	}}
	h := newHost(t, withAsides(s), withHooks(hooks), withOnClose(func() {
		select {
		case <-holdWriter:
		default:
			close(holdWriter)
		}
	}))
	a := h.dial()
	a.sayHello(nil)
	first := a.send(protocol.MethodSessionAside, asideParams(h, "a0", "first"))
	held.enter(t)
	await(t, writerHeld, "the writer held on the first notice")
	second := a.send(protocol.MethodSessionAside, asideParams(h, "a1", "second"))
	held.enter(t)
	var busy []string
	for i := range protocol.RequestsPerConnection - 2 {
		busy = append(busy, a.send(protocol.MethodSessionAside, asideParams(h, fmt.Sprintf("b%d", i), "q")))
	}
	await(t, full, "every admission slot taken")
	await(t, refused, "every busy refusal answered")
	cancel := a.send(protocol.MethodSessionAsideCancel, cancelParams(h, "a0"))
	close(holdWriter)

	// The notice first; then, in some order, the busy answers, the first's
	// answer, and the cancel's {} after it.
	if n := asideNotices(t, []*protocol.Notification{a.note(protocol.NotifyAside)}); n[0].Text != "piece" || n[0].Seq != 1 {
		t.Fatalf("the first notice: %+v", n[0])
	}
	pending := map[string]bool{first: true, cancel: true}
	for _, id := range busy {
		pending[id] = true
	}
	for len(pending) > 0 {
		m := a.next()
		if m.resp == nil {
			t.Fatalf("a notification among the answers: %s", describe(m))
		}
		id := string(m.resp.ID)
		switch {
		case !pending[id]:
			t.Fatalf("an answer to %s", id)
		case id == first:
			refusedWith(t, m.resp, protocol.RPCRefused, protocol.CodeAborted, protocol.ReasonContext)
		case id == cancel:
			if pending[first] {
				t.Fatal("the cancel's {} came before its aside's answer")
			}
			ok[protocol.Empty](t, m.resp)
		default:
			refusedWith(t, m.resp, protocol.RPCRefused, protocol.CodeUnavailable, protocol.ReasonAsideBusy)
		}
		delete(pending, id)
	}
	held.release <- "second"
	if got := ok[protocol.AsideResult](t, a.reply(second)); got.Text != "second" {
		t.Fatalf("the second aside: %+v", got)
	}
	waitFor(t, "only the reader's slot taken", func() bool {
		ad := h.srv.Admitted()
		return len(ad) == 1 && ad[0] == 1
	})
	if n := h.srv.Asides(); n != 0 {
		t.Fatalf("%d asides reserved", n)
	}
	held.release <- "next"
	if got := ok[protocol.AsideResult](t, a.call(protocol.MethodSessionAside, asideParams(h, "a0", "again"))); got.Text != "next" {
		t.Fatalf("the next aside: %+v", got)
	}
}
