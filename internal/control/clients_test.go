package control_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/control"
	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/tui"
)

// pastRetirement is past the receipts table's age bound (receiptAge, 10
// minutes): a released client holding no entry is retired by then.
const pastRetirement = 11 * time.Minute

func queueAdd(c *client, id, text string) *protocol.Response {
	return c.call(protocol.MethodQueueAdd, protocol.QueueAddParams{SessionID: sid(c.h), CommandID: id, Text: text})
}

// TestAClientIsReleasedAndReclaimed (A12): a connection's client is released
// when the connection closes, and the holder of its token takes it back on a
// new one — the same id, resumed: true, its command ids still answered from
// the receipts table, and never retired while it is bound again. A client
// nobody resumes is retired once released for the table's age bound, and a
// resume then gets a fresh id.
func TestAClientIsReleasedAndReclaimed(t *testing.T) {
	h := newHost(t)
	a := h.dial()
	ha := a.sayHello(nil)
	if ha.Resumed || ha.ClientID == "" || len(ha.Token) != 32 {
		t.Fatalf("a fresh hello: %+v", ha)
	}
	row := ok[protocol.QueueAddResult](t, queueAdd(a, "1", "one")).Row
	a.close()
	h.logs.wait(t, "conn 1 close", ha.ClientID)

	b := h.dial()
	hb := b.sayHello(a.resume())
	if !hb.Resumed || hb.ClientID != ha.ClientID || hb.Token != ha.Token {
		t.Fatalf("the resume: %+v, want resumed %s with its token", hb, ha.ClientID)
	}
	// The resend of a command the dropped connection ran is its stored
	// answer, not a second row.
	if again := ok[protocol.QueueAddResult](t, queueAdd(b, "1", "one")).Row; string(again) != string(row) {
		t.Fatalf("the resend replayed %s, want %s", again, row)
	}
	// Claimed again, it is live: the age bound retires nothing bound.
	h.clock.advance(pastRetirement)
	ok[protocol.QueueAddResult](t, queueAdd(b, "2", "two"))

	// The release half: a client whose connection closed, and that nobody
	// resumed, retires once it has been released for the age bound.
	c := h.dial()
	hc := c.sayHello(nil)
	c.close()
	h.logs.wait(t, "close", hc.ClientID)
	h.clock.advance(pastRetirement)
	d := h.dial()
	if hd := d.sayHello(c.resume()); hd.Resumed || hd.ClientID == hc.ClientID {
		t.Fatalf("a retired client resumed: %+v", hd)
	}
}

// TestAStaleConnectionsCleanupReleasesNothing (A12, astra 12; C2's review): a
// connection's close whose cleanup runs only after a resume has transferred
// its client to a new connection releases nothing — the binding names the new
// connection at a new generation — so the client, bound again, is never
// retired.
func TestAStaleConnectionsCleanupReleasesNothing(t *testing.T) {
	reached := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	var arm atomic.Bool
	h := newHost(t, withHooks(control.TestHooks{BeforeUnbind: func(client string) {
		if client != "" && arm.Load() {
			once.Do(func() {
				close(reached)
				<-release
			})
		}
	}}))
	a := h.dial()
	ha := a.sayHello(nil)
	arm.Store(true)
	a.close()
	await(t, reached, "the old connection's cleanup")

	b := h.dial()
	if hb := b.sayHello(a.resume()); !hb.Resumed || hb.ClientID != ha.ClientID {
		t.Fatalf("the resume: %+v", hb)
	}
	close(release)
	h.logs.wait(t, "conn 1 close", ha.ClientID)

	// Were the stale cleanup to have released the client, it would be retired
	// now, holding no entry, and its next command refused as no client of the
	// engine's.
	h.clock.advance(pastRetirement)
	ok[protocol.QueueAddResult](t, queueAdd(b, "1", "after"))
}

// TestSimultaneousResumesLeaveOneBinding (A12): two resumes of one client
// arriving together serialise on the binding section; the second transfers
// the binding from the first and closes it, and the first closed the original
// holder. One connection is left bound, it answers, and the client is not
// released by either loser's cleanup.
func TestSimultaneousResumesLeaveOneBinding(t *testing.T) {
	var arrived atomic.Int32
	both := make(chan struct{})
	var arm atomic.Bool
	h := newHost(t, withHooks(control.TestHooks{BeforeBind: func() {
		if !arm.Load() {
			return
		}
		if arrived.Add(1) == 2 {
			close(both)
		}
		<-both
	}}))
	a := h.dial()
	ha := a.sayHello(nil)
	arm.Store(true)

	b, c := h.dial(), h.dial()
	b.send(protocol.MethodHello, helloParams(a.resume()))
	c.send(protocol.MethodHello, helloParams(a.resume()))
	await(t, both, "both resumes at the binding section")

	// The original holder is superseded and closed by the first resume, and
	// the first resume by the second.
	a.expectEOF()
	h.logs.waitCount(t, 2, "close", "superseded by a resume")

	// Each resume answered resumed: true (the first perhaps never on the
	// wire, closed before its writer got to it); exactly one connection is
	// still bound, and it answers.
	type result struct {
		cl    *client
		alive bool
	}
	var results []result
	for _, cl := range []*client{b, c} {
		resp, err := cl.tryRead()
		if err == nil {
			res := ok[protocol.HelloResult](t, resp)
			if !res.Resumed || res.ClientID != ha.ClientID {
				t.Fatalf("a resume answered %+v", res)
			}
			cl.hello = res
		}
		alive := err == nil && func() bool {
			// The loser may be closed under this write: a failed write, or
			// EOF after it, is what being superseded looks like.
			id, err := cl.trySend(protocol.MethodSessionState, protocol.StateParams{SessionID: sid(h)})
			if err != nil {
				return false
			}
			r, err := cl.tryRead()
			return err == nil && string(r.ID) == id && r.Error == nil
		}()
		results = append(results, result{cl, alive})
	}
	var survivor *client
	for _, r := range results {
		if r.alive {
			if survivor != nil {
				t.Fatal("both resumed connections are still bound")
			}
			survivor = r.cl
		} else {
			r.cl.expectEOF()
		}
	}
	if survivor == nil {
		t.Fatal("no resumed connection is left")
	}
	h.clock.advance(pastRetirement)
	ok[protocol.QueueAddResult](t, queueAdd(survivor, "1", "still bound"))
}

// TestAClientWithARunningCommandIsNotRetired (A12, GLM 8): a Set parked in
// the session when its connection drops keeps its client alive past the age
// bound — the command's open reservation is an entry — and it ran on the
// server's context, never the connection's, so a resume's resend of the same
// command id still finds its answer.
//
// The peer's close is only a read EOF to the server — half-close: in-flight
// requests still get their replies (§3.7) — so the connection is closed, and
// its client released, when a reply's write fails: a SetTitle parked in the
// session index is let go after the peer has gone, and its reply is that
// write, while the Set is still parked.
func TestAClientWithARunningCommandIsNotRetired(t *testing.T) {
	store := newGateStore()
	h := newHost(t, withIndex(engine.IndexOptions{Store: store, CWD: "/w", Provider: "cursor"}))
	t.Cleanup(store.open)
	held, releaseSet := h.stub.HoldNextSet()
	t.Cleanup(releaseSet)
	a := h.dial()
	ha := a.sayHello(nil)
	set := protocol.SetParams{SessionID: sid(h), CommandID: "1",
		Setting: protocol.Setting{Kind: protocol.SettingModel, Value: "fast"}}
	a.send(protocol.MethodSessionSet, set)
	await(t, held, "the Set to park in the session")
	a.send(protocol.MethodSessionSetTitle, protocol.SetTitleParams{SessionID: sid(h), CommandID: "2", Title: "renamed"})
	store.waitEntered(t, 1)
	a.close()
	store.open()
	h.logs.wait(t, "conn 1 close", ha.ClientID, "write failed")

	h.clock.advance(pastRetirement)
	b := h.dial()
	if hb := b.sayHello(a.resume()); !hb.Resumed || hb.ClientID != ha.ClientID {
		t.Fatalf("a client with a running command was retired: %+v", hb)
	}
	releaseSet()
	res := ok[protocol.SetResult](t, b.call(protocol.MethodSessionSet, set))
	if res.Value != "fast" || res.Rev == 0 {
		t.Fatalf("the resend's answer: %+v", res)
	}
}

// TestARetiredClientCannotBeClaimed (A12): a released client that has waited
// out the age bound holding nothing is retired, and a resume presenting its
// token gets resumed: false and a fresh id — never the old one.
func TestARetiredClientCannotBeClaimed(t *testing.T) {
	h := newHost(t)
	a := h.dial()
	ha := a.sayHello(nil)
	a.close()
	h.logs.wait(t, "conn 1 close", ha.ClientID)
	h.clock.advance(pastRetirement)

	b := h.dial()
	hb := b.sayHello(a.resume())
	if hb.Resumed || hb.ClientID == ha.ClientID || hb.Token == ha.Token {
		t.Fatalf("a retired client was claimed: %+v", hb)
	}
	// The fresh client counts its commands from 1.
	ok[protocol.QueueAddResult](t, queueAdd(b, "1", "fresh"))
}

// startedEngine is a second engine over a Stub of its own, started.
func startedEngine(t *testing.T) *engine.Engine {
	t.Helper()
	e, err := engine.New(tui.NewStubNoPrimary(), engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	if err := e.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	return e
}

// TestASecondEngineClosesEveryConnectionAndClearsEveryBinding (§3.6, astra
// 14; C6's half of TestReplacingTheEngineClosesEveryConnection, whose reset
// is C7's): replacing the engine closes every connection, bound or not, and
// clears every binding — tokens are scoped to one engine incarnation, and
// client ids restart at c-1 per engine, so a token of the old engine is
// answered resumed: false even where its id is the new engine's too, never a
// binding. The replaced engine's tokens are remembered for one generation (a
// known token under the wrong id is still bad_token); two replacements on, an
// old token is unknown, and resumed: false whatever id it comes with.
func TestASecondEngineClosesEveryConnectionAndClearsEveryBinding(t *testing.T) {
	h := newHost(t)
	a, b, unbound := h.dial(), h.dial(), h.dial()
	ha := a.sayHello(nil)
	hb := b.sayHello(nil)

	eng2 := startedEngine(t)
	h.srv.SetEngine(eng2)
	a.expectEOF()
	b.expectEOF()
	unbound.expectEOF()

	x := h.dial()
	hx := x.sayHello(nil)
	if hx.ClientID != ha.ClientID {
		t.Fatalf("the premise: the new engine's first client is %s, the old engine's was %s", hx.ClientID, ha.ClientID)
	}
	y := h.dial()
	if hy := y.sayHello(a.resume()); hy.Resumed || hy.ClientID == ha.ClientID {
		t.Fatalf("an old engine's token bound: %+v", hy)
	}
	st := ok[protocol.StateResult](t, x.call(protocol.MethodSessionState, protocol.StateParams{SessionID: eng2.State().CrazeSessionID}))
	if st.Activity != protocol.ActivityIdle {
		t.Fatalf("the new engine's state: %+v", st)
	}
	refusedWith(t, x.call(protocol.MethodSessionState, protocol.StateParams{SessionID: h.sessionID()}),
		protocol.RPCRefused, protocol.CodeUnknownSession, protocol.ReasonUnknownSession)

	// The previous generation is remembered: its token under another id is
	// still a wrong token.
	wrong := &protocol.Resume{ClientID: hb.ClientID, Token: ha.Token}
	z := h.dial()
	refusedWith(t, z.call(protocol.MethodHello, helloParams(wrong)), protocol.RPCRefused, protocol.CodeBadRequest, protocol.ReasonBadToken)
	// Two replacements on it is not: unknown, and resumed: false.
	h.srv.SetEngine(startedEngine(t))
	w := h.dial()
	if hw := w.sayHello(wrong); hw.Resumed {
		t.Fatalf("a token two engines old: %+v", hw)
	}
}

// TestAWrongTokenNeverBinds (A12; bind.go's token index): a resume presenting
// a token this server issued for ANOTHER client id — a confused or hostile
// client — is bad_request, reason bad_token, and changes nothing: both holders
// stay bound, and the refused connection is bound to nothing (it may say hello
// afresh and gets a fresh id). An id that was never minted, under a known
// token, is the same wrong token.
func TestAWrongTokenNeverBinds(t *testing.T) {
	h := newHost(t)
	a, b := h.dial(), h.dial()
	ha, hb := a.sayHello(nil), b.sayHello(nil)

	x := h.dial()
	for _, wrong := range []*protocol.Resume{
		{ClientID: hb.ClientID, Token: ha.Token},
		{ClientID: ha.ClientID, Token: hb.Token},
		{ClientID: "c-99", Token: ha.Token},
	} {
		refusedWith(t, x.call(protocol.MethodHello, helloParams(wrong)), protocol.RPCRefused, protocol.CodeBadRequest, protocol.ReasonBadToken)
	}
	refusedWith(t, x.call(protocol.MethodSessionState, protocol.StateParams{SessionID: sid(h)}),
		protocol.RPCRefused, protocol.CodeBadRequest, protocol.ReasonHelloRequired)
	ok[protocol.QueueAddResult](t, queueAdd(a, "1", "still a's"))
	ok[protocol.QueueAddResult](t, queueAdd(b, "1", "still b's"))
	if hx := x.sayHello(nil); hx.Resumed || hx.ClientID == ha.ClientID || hx.ClientID == hb.ClientID {
		t.Fatalf("a fresh hello after a refused resume: %+v", hx)
	}
}

// TestATokenFromAnotherHostIsNotBadToken (§3.6: "a token from another
// incarnation is resumed: false and a fresh id"): client ids restart at c-1 in
// every host process, so a client that resumes against a host that did not
// issue its token can name an id that is bound here now. A token this server
// never issued is another incarnation's: the answer is resumed: false with a
// fresh id — never bad_token — and the live binding of that id is untouched.
func TestATokenFromAnotherHostIsNotBadToken(t *testing.T) {
	other := newHost(t)
	stranger := other.dial()
	hs := stranger.sayHello(nil)

	h := newHost(t)
	a := h.dial()
	ha := a.sayHello(nil)
	if ha.ClientID != hs.ClientID {
		t.Fatalf("the premise: both hosts' first client is c-1; got %s and %s", ha.ClientID, hs.ClientID)
	}
	x := h.dial()
	hx := x.sayHello(stranger.resume())
	if hx.Resumed || hx.ClientID == ha.ClientID || hx.Token == hs.Token {
		t.Fatalf("another host's token: %+v, want resumed: false and a fresh id", hx)
	}
	// The live binding is untouched: its holder is still bound and answered,
	// and its own token still resumes it.
	ok[protocol.QueueAddResult](t, queueAdd(a, "1", "still a's"))
	a.close()
	h.logs.wait(t, "close", "client "+ha.ClientID+":")
	y := h.dial()
	if hy := y.sayHello(a.resume()); !hy.Resumed || hy.ClientID != ha.ClientID {
		t.Fatalf("the holder's own resume after a stranger's: %+v", hy)
	}
}

// queued is how many rows of the engine's queue carry text.
func queued(h *host, text string) int {
	n := 0
	for _, q := range h.eng.State().Queue {
		if q.Text == text {
			n++
		}
	}
	return n
}

// closeOnce closes ch unless it is closed already.
func closeOnce(ch chan struct{}) {
	select {
	case <-ch:
	default:
		close(ch)
	}
}

// TestASupersededConnectionAdmitsNothingMore (astra r5 3): once a resume has
// transferred a client to a new connection, the old one runs nothing more
// under the client id it held — not a request its reader had already
// buffered, and not a request admitted before the transfer that had not yet
// reached the engine. The client's resend on its new connection runs such a
// command once.
func TestASupersededConnectionAdmitsNothingMore(t *testing.T) {
	t.Run("its reader admits no line it had buffered", func(t *testing.T) {
		reached := make(chan struct{})
		release := make(chan struct{})
		var acquires, commands atomic.Int32
		h := newHost(t, withHooks(control.TestHooks{
			// A is the first connection. Its second acquire is the one after
			// hello: the reader is held there with the rest buffered.
			BeforeAcquire: func(conn uint64) {
				if conn == 1 && acquires.Add(1) == 2 {
					close(reached)
					<-release
				}
			},
			BeforeCommand: func(string) { commands.Add(1) },
		}))
		t.Cleanup(func() { closeOnce(release) })
		a := h.dial()
		// hello and three commands in ONE write: the reader's first read
		// buffers all four lines; it answers hello and is held before it
		// takes the slot for the next.
		_, lines := a.encode(protocol.MethodHello, helloParams(nil))
		for range 3 {
			_, line := a.encode(protocol.MethodQueueAdd, protocol.QueueAddParams{SessionID: sid(h), CommandID: a.cmd(), Text: "from a"})
			lines = append(lines, line...)
		}
		a.write(lines)
		await(t, reached, "A's reader before its second acquire")
		a.hello = ok[protocol.HelloResult](t, a.read())

		b := h.dial()
		if hb := b.sayHello(a.resume()); !hb.Resumed || hb.ClientID != a.hello.ClientID {
			t.Fatalf("the resume: %+v", hb)
		}
		a.expectEOF()
		closeOnce(release)

		// Close joins A's reader and waits out every handler: whatever A was
		// going to admit, it has by then.
		ctx, cancel := context.WithTimeout(context.Background(), watchdog)
		defer cancel()
		if err := h.srv.Close(ctx); err != nil {
			t.Fatal(err)
		}
		if n := commands.Load(); n != 0 {
			t.Fatalf("A's reader admitted %d buffered commands after it was superseded", n)
		}
		if n := queued(h, "from a"); n != 0 {
			t.Fatalf("%d of A's buffered commands reached the engine after it was superseded", n)
		}
	})

	t.Run("an admitted command not yet at the engine does not run", func(t *testing.T) {
		held := make(chan struct{})
		release := make(chan struct{})
		var once sync.Once
		var arm atomic.Bool
		h := newHost(t, withHooks(control.TestHooks{BeforeCommand: func(string) {
			if arm.Load() {
				once.Do(func() {
					close(held)
					<-release
				})
			}
		}}))
		t.Cleanup(func() { closeOnce(release) })
		a := h.dial()
		a.sayHello(nil)
		arm.Store(true)
		a.send(protocol.MethodQueueAdd, protocol.QueueAddParams{SessionID: sid(h), CommandID: "1", Text: "from a"})
		await(t, held, "A's command before its engine call")

		b := h.dial()
		if hb := b.sayHello(a.resume()); !hb.Resumed || hb.ClientID != a.hello.ClientID {
			t.Fatalf("the resume: %+v", hb)
		}
		a.expectEOF()
		closeOnce(release)
		waitFor(t, "A's handler to return", func() bool { return h.srv.Handlers() == 0 })
		if n := queued(h, "from a"); n != 0 {
			t.Fatalf("A's command ran after A was superseded: %d rows", n)
		}
		// Its id was never used: the resend on B runs it, and a second resend
		// is answered from the receipts table.
		ok[protocol.QueueAddResult](t, queueAdd(b, "1", "from a"))
		ok[protocol.QueueAddResult](t, queueAdd(b, "1", "from a"))
		if n := queued(h, "from a"); n != 1 {
			t.Fatalf("the resends ran %d times, want once", n)
		}
	})

	// The other side of the re-check (§3.6, 03 §7): a connection that merely
	// closes — released, with no resume — has not moved on, so a command it
	// admitted still runs, once, and its receipt answers the resend after a
	// later resume.
	t.Run("an admitted command whose connection merely closed still runs", func(t *testing.T) {
		gates := map[string]chan struct{}{
			protocol.MethodQueueAdd:        make(chan struct{}),
			protocol.MethodSessionSetTitle: make(chan struct{}),
		}
		arrived := make(chan string, len(gates))
		var arm atomic.Bool
		h := newHost(t, withHooks(control.TestHooks{BeforeCommand: func(method string) {
			if gate, held := gates[method]; held && arm.Load() {
				arrived <- method
				<-gate
			}
		}}))
		t.Cleanup(func() {
			for _, gate := range gates {
				closeOnce(gate)
			}
		})
		a := h.dial()
		ha := a.sayHello(nil)
		arm.Store(true)
		a.send(protocol.MethodQueueAdd, protocol.QueueAddParams{SessionID: sid(h), CommandID: "1", Text: "from a"})
		a.send(protocol.MethodSessionSetTitle, protocol.SetTitleParams{SessionID: sid(h), CommandID: "2", Title: "renamed"})
		for range gates {
			select {
			case <-arrived:
			case <-time.After(watchdog):
				t.Fatal("both commands before their engine calls")
			}
		}
		// The peer goes away. A full close is noticed only when a write fails
		// (X12 13): the SetTitle runs and its reply's write is that failure,
		// which closes the connection and releases its client — plainly, with
		// no resume. The queue.add is still held before its engine call.
		a.close()
		closeOnce(gates[protocol.MethodSessionSetTitle])
		h.logs.wait(t, "conn 1 close", "client "+ha.ClientID+":", "write failed")

		closeOnce(gates[protocol.MethodQueueAdd])
		waitFor(t, "the held command to return", func() bool { return h.srv.Handlers() == 0 })
		if n := queued(h, "from a"); n != 1 {
			t.Fatalf("the command admitted before a plain close ran %d times, want once", n)
		}
		// A later resume's resend is answered from its receipt: the same row,
		// never a second one.
		b := h.dial()
		if hb := b.sayHello(a.resume()); !hb.Resumed || hb.ClientID != ha.ClientID {
			t.Fatalf("the resume: %+v", hb)
		}
		row, err := agent.DecodeQueuedPrompt(ok[protocol.QueueAddResult](t, queueAdd(b, "1", "from a")).Row)
		if err != nil {
			t.Fatal(err)
		}
		if st := h.eng.State(); queued(h, "from a") != 1 || st.Queue[0].ID != row.ID {
			t.Fatalf("the resend's row %s, the queue %+v: want the stored row, once", row.ID, st.Queue)
		}
	})
}

// TestABindingIdleForTwiceTheHorizonIsDropped (plan 027 X13, astra r5): a
// binding with no connection for twice the receipts table's age bound is
// dropped with its token, so neither the table nor the token index grows with
// every client that never comes back — and a binding just under the bound
// still resumes. The drop does not wait for the engine: a client whose command
// is still running (so not retired) loses its binding all the same, and its
// resume is answered resumed: false with a fresh id.
func TestABindingIdleForTwiceTheHorizonIsDropped(t *testing.T) {
	t.Run("clients that never come back are forgotten", func(t *testing.T) {
		h := newHost(t)
		idle := 2 * h.eng.State().RetryHorizon.Age
		const gone = 5
		for range gone {
			c := h.dial()
			hc := c.sayHello(nil)
			c.close()
			h.logs.wait(t, "close", "client "+hc.ClientID+":")
		}
		if binds, tokens := h.srv.Bindings(); binds != gone || tokens != gone {
			t.Fatalf("before the bound: %d bindings and %d tokens, want %d of each", binds, tokens, gone)
		}
		h.clock.advance(idle)
		x := h.dial()
		x.sayHello(nil)
		if binds, tokens := h.srv.Bindings(); binds != 1 || tokens != 1 {
			t.Fatalf("past the bound: %d bindings and %d tokens, want the new client's alone", binds, tokens)
		}
	})

	t.Run("just under the bound it resumes, at the bound it does not", func(t *testing.T) {
		h := newHost(t)
		idle := 2 * h.eng.State().RetryHorizon.Age
		held, releaseSet := h.stub.HoldNextSet()
		t.Cleanup(releaseSet)
		// A Set parked in the session keeps its client from retiring (an open
		// reservation is an entry), so only the binding's own bound is at work.
		a := h.dial()
		ha := a.sayHello(nil)
		a.send(protocol.MethodSessionSet, protocol.SetParams{SessionID: sid(h), CommandID: "1",
			Setting: protocol.Setting{Kind: protocol.SettingModel, Value: "fast"}})
		await(t, held, "the Set to park in the session")
		// Its client moves to b, which goes away cleanly: released now.
		b := h.dial()
		if hb := b.sayHello(a.resume()); !hb.Resumed {
			t.Fatalf("the first resume: %+v", hb)
		}
		b.close()
		h.logs.wait(t, "conn 2 close", "client "+ha.ClientID+":")

		h.clock.advance(idle - time.Second)
		c := h.dial()
		if hc := c.sayHello(a.resume()); !hc.Resumed || hc.ClientID != ha.ClientID {
			t.Fatalf("just under the bound: %+v, want resumed %s", hc, ha.ClientID)
		}
		c.close()
		h.logs.wait(t, "conn 3 close", "client "+ha.ClientID+":")

		h.clock.advance(idle)
		d := h.dial()
		hd := d.sayHello(a.resume())
		if hd.Resumed || hd.ClientID == ha.ClientID || hd.Token == ha.Token {
			t.Fatalf("at the bound: %+v, want resumed: false with a fresh id", hd)
		}
		if binds, tokens := h.srv.Bindings(); binds != 1 || tokens != 1 {
			t.Fatalf("at the bound: %d bindings and %d tokens, want the fresh client's alone", binds, tokens)
		}
		// The server dropped the binding; the engine had not retired the
		// client, whose Set is still running.
		if err := h.eng.ClaimClient(ha.ClientID); err != nil {
			t.Fatalf("the engine retired the client: %v", err)
		}
	})
}
