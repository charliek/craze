package control_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/control"
	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/protocol"
)

// session.stop and the close fence (plan 030 §3.6, §3.6a, §3.18): the stop is
// a receipt handed to the host's coordinator once, later stops join it, a
// running turn does not hold it up, and an attach racing it is either
// counted by the fence or refused closing — never half-attached. A server
// without a coordinator refuses the stop exactly as every S2 host did.

// coordinator is a host's lifecycle coordinator as a test holds it
// (control.Options.Stop): it records every stop the server hands it, and runs
// the stop sequence's first step — the engine's close — on a goroutine of its
// own once the test opens its gate, as a real coordinator does its work off the
// handler's goroutine.
type coordinator struct {
	// heard carries every request handed over; it is deep enough that a
	// handler never waits on it.
	heard chan control.StopRequest
	gate  chan struct{}
	open  func()
	// eng is the engine the sequence closes, set once the host is built.
	eng     *engine.Engine
	started atomic.Bool
	done    chan struct{}
}

func newCoordinator() *coordinator {
	co := &coordinator{heard: make(chan control.StopRequest, 8), gate: make(chan struct{}), done: make(chan struct{})}
	var once sync.Once
	co.open = func() { once.Do(func() { close(co.gate) }) }
	return co
}

// stop is the StopFunc: it records the request and starts the sequence,
// parked at the gate, and returns at once.
func (co *coordinator) stop(r control.StopRequest) {
	co.heard <- r
	if !co.started.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer close(co.done)
		<-co.gate
		_ = co.eng.Close()
	}()
}

// next is the next request handed over, with the watchdog.
func (co *coordinator) next(t *testing.T) control.StopRequest {
	t.Helper()
	select {
	case r := <-co.heard:
		return r
	case <-time.After(watchdog):
		t.Fatalf("the coordinator was handed no stop in %s", watchdog)
	}
	panic("unreachable")
}

// none fails if the coordinator has been handed a request not yet read.
func (co *coordinator) none(t *testing.T) {
	t.Helper()
	select {
	case r := <-co.heard:
		t.Fatalf("the coordinator was handed a second stop: %+v", r)
	default:
	}
}

// withStop serves session.stop through co, and opens co's gate — then waits
// for its sequence — before the server closes.
func withStop(co *coordinator) hostOpt {
	return func(c *hostConfig) {
		c.opts.Stop = co.stop
		c.onClose = append(c.onClose, func() {
			co.open()
			if co.started.Load() {
				<-co.done
			}
		})
	}
}

// newStopHost is a host serving session.stop through a new coordinator.
func newStopHost(t *testing.T, opts ...hostOpt) (*host, *coordinator) {
	t.Helper()
	co := newCoordinator()
	h := newHost(t, append([]hostOpt{withStop(co)}, opts...)...)
	co.eng = h.eng
	return h, co
}

// stop sends session.stop on c and returns the request id.
func (c *client) stop(h *host, commandID string) string {
	c.t.Helper()
	return c.send(protocol.MethodSessionStop, protocol.StopParams{SessionID: sid(h), CommandID: commandID})
}

// untilReset reads c's stream to its reset, which must be reason, and returns
// the notifications before it.
func (c *client) untilReset(reason protocol.ResetReason) []*protocol.Notification {
	c.t.Helper()
	var notes []*protocol.Notification
	for {
		n := c.anyNote()
		if n.Method != protocol.NotifyReset {
			notes = append(notes, n)
			continue
		}
		if got := paramsOf[protocol.ResetParams](c.t, n).Reason; got != reason {
			c.t.Fatalf("the stream ended %s, want %s", got, reason)
		}
		return notes
	}
}

// TestAStopIsAReceiptTheCoordinatorHearsOnce (§3.6a): session.stop is
// answered {} at once; the coordinator is handed the first stop — who asked,
// under which id — and nothing of any later one: another client's stop and
// the same id resent are each answered {} and join it. The session goes on
// until the coordinator runs its sequence, whose engine close ends every
// connection as a session's end does.
func TestAStopIsAReceiptTheCoordinatorHearsOnce(t *testing.T) {
	h, co := newStopHost(t)
	a := h.dial()
	a.sayHello(nil)
	if res := a.attach(attachParams(h)); !res.Session.Capabilities.Stop {
		t.Fatal("a host that serves session.stop says capabilities.stop: false")
	}
	cmd := a.cmd()
	_, resp := a.until(a.stop(h, cmd))
	ok[protocol.Empty](t, resp)
	if r := co.next(t); r.Client != a.hello.ClientID || r.CommandID != cmd {
		t.Fatalf("the coordinator was handed %+v, want client %s command %s", r, a.hello.ClientID, cmd)
	}

	b := h.dial()
	b.sayHello(nil)
	ok[protocol.Empty](t, b.call(protocol.MethodSessionStop, protocol.StopParams{SessionID: sid(h), CommandID: b.cmd()}))
	_, resp = a.until(a.stop(h, cmd))
	ok[protocol.Empty](t, resp)
	// A later stop's receipt is queued only after the first's hand-over has
	// returned (stopOnce), so by now a second hand-over would be in heard.
	co.none(t)

	// Nothing is stopped until the coordinator's sequence runs.
	if st := ok[protocol.StateResult](t, b.call(protocol.MethodSessionState, protocol.StateParams{SessionID: sid(h)})); st.Activity == protocol.ActivityClosing {
		t.Fatal("the session closed before the coordinator ran its sequence")
	}
	co.open()
	a.untilReset(protocol.ResetSessionClosed)
	a.expectClosed()
	b.expectEOF()
}

// TestAStopDuringATurnIsAnsweredAtOnce (§3.6a): the receipt does not wait
// for the running turn, nor for the coordinator's sequence; the sequence's
// engine close then ends the turn (synthetic, closing) and the stream.
func TestAStopDuringATurnIsAnsweredAtOnce(t *testing.T) {
	h, co := newStopHost(t)
	a := h.dial()
	a.sayHello(nil)
	a.attach(attachParams(h))
	hangTurn(t, h, a)
	turn := h.eng.State().Turn

	_, resp := a.until(a.stop(h, a.cmd()))
	ok[protocol.Empty](t, resp)
	co.next(t)
	if st := h.eng.State(); st.Turn != turn || st.Activity != engine.ActivityWorking {
		t.Fatalf("after the receipt the engine is %s on %q, want the turn %s still running", st.Activity, st.Turn, turn)
	}

	co.open()
	var ending bool
	for _, n := range a.untilReset(protocol.ResetSessionClosed) {
		if n.Method != protocol.NotifyEvent {
			continue
		}
		if _, ev := eventOf(t, n); ev.Type == agent.EventTurn && ev.Turn.Phase == agent.TurnEnded && ev.Turn.ID == turn {
			ending = ev.Turn.StopReason == "closing"
		}
	}
	if !ending {
		t.Fatalf("the stream did not carry %s's closing ending before its reset", turn)
	}
	a.expectClosed()
	if lt := h.eng.State().LastTurn; lt == nil || lt.TurnID != turn || lt.Outcome != engine.TurnCancelled {
		t.Fatalf("the last turn is %+v, want %s cancelled by the close", lt, turn)
	}
}

// toTheEnd reads c's lines, in order, until the host closes the connection.
func (c *client) toTheEnd() []msg {
	c.t.Helper()
	var out []msg
	for {
		m, err := c.tryNext()
		if err != nil {
			if !errors.Is(err, io.EOF) && !isReset(err) {
				c.t.Fatalf("want the host to close the connection; read: %v", err)
			}
			return out
		}
		out = append(out, m)
	}
}

// TestAStopsReceiptPrecedesTheSessionsEnd (§3.6a; astra r2-c1 1): the first
// stop's receipt is on its connection ahead of everything the stop goes on to
// put there — the running turn's closing ending and reset{session_closed} —
// so a client that stops reading at the session's end has read its receipt.
// The schedule is forced: a hook just before the receipt is queued lets the
// coordinator's sequence run and, when the coordinator already has the stop,
// holds the receipt until that sequence has closed the engine and queued the
// stopping connection's reset. A server that handed the stop over before it
// queued the receipt puts the session's end first, every time; this one has
// set nothing going yet, so nothing can overtake the receipt.
func TestAStopsReceiptPrecedesTheSessionsEnd(t *testing.T) {
	co := newCoordinator()
	resets := newSignal()
	var ran, early atomic.Bool
	h := newHost(t, withStop(co), withHooks(control.TestHooks{
		BeforeReply: func(method string) {
			if method != protocol.MethodSessionStop {
				return
			}
			ran.Store(true)
			co.open()
			if !co.started.Load() {
				return
			}
			// The coordinator has the stop already: the receipt is held
			// while its sequence closes the engine and ends the stream.
			early.Store(true)
			select {
			case <-resets:
			case <-time.After(watchdog):
			}
		},
		AckQueued: func(_, method string) {
			if method == protocol.NotifyReset {
				resets.fire(0)
			}
		},
	}))
	co.eng = h.eng
	a := h.dial()
	a.sayHello(nil)
	a.attach(attachParams(h))
	hangTurn(t, h, a)
	turn := h.eng.State().Turn

	stop := a.send(protocol.MethodSessionStop, protocol.StopParams{SessionID: sid(h), CommandID: a.cmd()})
	receipt, ending, reset := -1, -1, -1
	for i, m := range a.toTheEnd() {
		switch {
		case m.resp != nil && string(m.resp.ID) == stop:
			ok[protocol.Empty](t, m.resp)
			receipt = i
		case m.note != nil && m.note.Method == protocol.NotifyReset:
			if rp := paramsOf[protocol.ResetParams](t, m.note); rp.Reason != protocol.ResetSessionClosed {
				t.Fatalf("the stream ended %s, want session_closed", rp.Reason)
			}
			reset = i
		case m.note != nil && m.note.Method == protocol.NotifyEvent:
			if _, ev := eventOf(t, m.note); ev.Type == agent.EventTurn && ev.Turn.Phase == agent.TurnEnded && ev.Turn.ID == turn {
				ending = i
			}
		}
	}
	switch {
	case !ran.Load():
		t.Fatal("the stop's receipt was queued without passing its BeforeReply hook")
	case receipt < 0 || ending < 0 || reset < 0:
		t.Fatalf("the connection closed with the receipt at %d, %s's ending at %d, the reset at %d", receipt, turn, ending, reset)
	case receipt > ending || receipt > reset:
		t.Fatalf("the receipt is line %d, after %s's closing ending (%d) or the reset (%d): the stop's end overtook it", receipt, turn, ending, reset)
	case early.Load():
		t.Fatal("the coordinator was handed the stop before its receipt was queued")
	}
	co.next(t)
}

// TestAStalledClientsStopIsNotHeldBack (§3.6a; astra r2-c1 1): a client that
// has stopped reading — its connection's writer queue full, its forwarder
// blocked for room — stops the session, under a request id so long that its
// receipt is larger than any room the queue has left. The receipt is queued at
// once all the same, outside the budget: the handler never waits for room,
// and the coordinator has the stop while the client has still read nothing.
// When the client reads again, the receipt comes right after what was queued
// before the stop — ahead of the record the forwarder was blocked on and of
// every record the engine's close then delivers — and reset{session_closed}
// ends the stream.
func TestAStalledClientsStopIsNotHeldBack(t *testing.T) {
	co := newCoordinator()
	s := newStall(t, nil, withStop(co))
	co.eng = s.h.eng
	// The forwarder is blocked on a 1 MiB record: less than that is free,
	// and the receipt, echoing a 2 MiB id, cannot fit in it.
	stop := `"` + strings.Repeat("s", 2<<20) + `"`
	line := requestLine(t, stop, protocol.MethodSessionStop, protocol.StopParams{SessionID: sid(s.h), CommandID: s.a.cmd()})
	s.a.sendRaw(stop, protocol.MethodSessionStop, strings.TrimSuffix(string(line), "\n"))
	co.next(t)
	waitFor(t, "the stop's handler to return, its receipt queued without room", func() bool { return s.h.srv.Handlers() == 0 })
	co.open()

	next, receipt := s.r.After.Seq+1, false
	for {
		m := s.a.next()
		if m.resp != nil {
			if string(m.resp.ID) != stop {
				t.Fatalf("a reply to %.40s…, want only the stop's", m.resp.ID)
			}
			ok[protocol.Empty](t, m.resp)
			if next != s.blocked {
				t.Fatalf("the receipt follows event %d; it was queued with the forwarder blocked on %d", next-1, s.blocked)
			}
			receipt = true
			continue
		}
		if m.note.Method == protocol.NotifyReset {
			if rp := paramsOf[protocol.ResetParams](t, m.note); rp.Reason != protocol.ResetSessionClosed {
				t.Fatalf("the stream ended %s, want session_closed", rp.Reason)
			}
			break
		}
		if seq, _ := eventOf(t, m.note); seq != next {
			t.Fatalf("event %d, want %d: a gap", seq, next)
		}
		next++
	}
	if !receipt {
		t.Fatal("the stream ended without the stop's receipt")
	}
	s.a.expectClosed()
}

// holdOnce is a hook that parks the first call only, until release: arrived
// hears it park.
type holdOnce struct {
	once     sync.Once
	arrived  chan struct{}
	released chan struct{}
	release  func()
}

func newHoldOnce() *holdOnce {
	h := &holdOnce{arrived: make(chan struct{}), released: make(chan struct{})}
	var once sync.Once
	h.release = func() { once.Do(func() { close(h.released) }) }
	return h
}

func (h *holdOnce) hold() {
	first := false
	h.once.Do(func() { first = true })
	if !first {
		return
	}
	close(h.arrived)
	<-h.released
}

// TestAStopRacingANewAttach (§3.6, §3.6a): the attach arrives exactly as the
// stop's fence goes up, forced on either side of it and at the one point in
// between — never half-attached: an attach that reserved before the fence is
// counted by it and goes live; one that reaches its reservation after is
// refused closing and holds nothing; and a fence raised while an attach is
// between its check and its install cannot go up until the install is done —
// it finds the attachment lock held and waits — and then counts it (astra
// r2-c1 2: exclusion proven, not left to the scheduler).
func TestAStopRacingANewAttach(t *testing.T) {
	t.Run("an attach that reaches its reservation after the stop is refused closing", func(t *testing.T) {
		gate := newHoldOnce()
		h, co := newStopHost(t, withHooks(control.TestHooks{BeforeReserve: gate.hold}), withOnClose(gate.release))
		b := h.dial()
		b.sayHello(nil)
		attach := b.send(protocol.MethodSessionAttach, attachParams(h))
		await(t, gate.arrived, "the attach to reach its reservation")

		a := h.dial()
		a.sayHello(nil)
		ok[protocol.Empty](t, a.call(protocol.MethodSessionStop, protocol.StopParams{SessionID: sid(h), CommandID: a.cmd()}))
		co.next(t)
		gate.release()
		refusedWith(t, b.reply(attach), protocol.RPCRefused, protocol.CodeUnavailable, protocol.ReasonClosing)
		// Nothing of it is held: no attachment on the connection, none in the
		// server's count, and a later attach is refused the same.
		refusedWith(t, b.detach(h, "s-1"), protocol.RPCRefused, protocol.CodeBadRequest, protocol.ReasonBadRequest)
		if n := h.srv.Attached(); n != 0 {
			t.Fatalf("the server counts %d attachments after the refusal", n)
		}
		refusedWith(t, b.call(protocol.MethodSessionAttach, attachParams(h)), protocol.RPCRefused, protocol.CodeUnavailable, protocol.ReasonClosing)
	})
	t.Run("an attach reserved before the stop is counted and goes live", func(t *testing.T) {
		gate := newHoldOnce()
		h, co := newStopHost(t, withHooks(control.TestHooks{Reserved: func(string) { gate.hold() }}), withOnClose(gate.release))
		b := h.dial()
		b.sayHello(nil)
		attach := b.send(protocol.MethodSessionAttach, attachParams(h))
		await(t, gate.arrived, "the attach to reserve")

		a := h.dial()
		a.sayHello(nil)
		ok[protocol.Empty](t, a.call(protocol.MethodSessionStop, protocol.StopParams{SessionID: sid(h), CommandID: a.cmd()}))
		co.next(t)
		n, release := h.srv.FenceAttaches()
		release()
		if n != 1 {
			t.Fatalf("a fence raised after the stop counts %d attachments, want the reserved one", n)
		}
		gate.release()
		if res := ok[protocol.AttachResult](t, b.reply(attach)); res.Snapshot == nil {
			t.Fatal("the reserved attach went live with no snapshot")
		}
		co.open()
		b.untilReset(protocol.ResetSessionClosed)
		b.expectClosed()
	})
	t.Run("a fence cannot go up while an attach is between its check and its install", func(t *testing.T) {
		gate := newHoldOnce()
		waits := make(chan struct{}, 1)
		h := newHost(t, withHooks(control.TestHooks{
			Reserving: gate.hold,
			FenceWaits: func() {
				select {
				case waits <- struct{}{}:
				default:
				}
			},
		}), withOnClose(gate.release))
		b := h.dial()
		b.sayHello(nil)
		attach := b.send(protocol.MethodSessionAttach, attachParams(h))
		await(t, gate.arrived, "the attach to pass its fence check")

		counted := make(chan int, 1)
		go func() {
			n, release := h.srv.FenceAttaches()
			counted <- n
			release()
		}()
		// The attach sits between its check and its install, holding the
		// attachment lock: the fence must find that lock held and wait. A
		// fence that goes up instead — the attach not yet installed, so
		// neither counted nor refused — is the half-attach the lock exists
		// to rule out.
		select {
		case <-waits:
		case n := <-counted:
			t.Fatalf("the fence went up, counting %d, while an attach sat between its fence check and its install", n)
		case <-time.After(watchdog):
			t.Fatalf("the fence neither waited for the attachment lock nor went up in %s", watchdog)
		}
		gate.release()
		select {
		case n := <-counted:
			if n != 1 {
				t.Fatalf("the fence counted %d attachments, want the one it waited for", n)
			}
		case <-time.After(watchdog):
			t.Fatalf("the fence did not go up in %s once the attach was installed", watchdog)
		}
		ok[protocol.AttachResult](t, b.reply(attach))
	})
}

// TestAServerWithoutStopRefusesItAsBefore (§3.6a, X1): a server built with no
// coordinator — a TUI-hosted session, the fake host by default — says
// capabilities.stop: false, leaves the host's facts out of its info document,
// and refuses session.stop unsupported, reason stop_unsupported, byte for byte
// as every S2 host did.
func TestAServerWithoutStopRefusesItAsBefore(t *testing.T) {
	h := newHost(t)
	c := h.dial()
	c.sayHello(nil)
	c.send(protocol.MethodSessionStop, protocol.StopParams{SessionID: sid(h), CommandID: c.cmd()})
	line, err := c.readLine()
	if err != nil {
		t.Fatal(err)
	}
	want := `{"jsonrpc":"2.0","id":2,"error":{"code":-32000,"message":"session.stop is not served by a session host","data":{"code":"unsupported","reason":"stop_unsupported"}}}`
	if string(line) != want {
		t.Fatalf("the refusal is\n %s\nwant\n %s", line, want)
	}
	c.send(protocol.MethodSessionAttach, attachParams(h))
	line, err = c.readLine()
	if err != nil {
		t.Fatal(err)
	}
	for _, absent := range []string{`"permissionMode"`, `"startedAt"`, `"lastTurn"`} {
		if bytes.Contains(line, []byte(absent)) {
			t.Errorf("the attach reply carries %s: %s", absent, line)
		}
	}
	if !bytes.Contains(line, []byte(`"stop":false`)) {
		t.Errorf("the attach reply does not say stop: false: %s", line)
	}
}

// TestTheCloseFenceIsReversible (§3.6's "The decision is atomic"): a fence
// counts every attachment not yet closed and refuses new ones closing; fences
// stack, a release is idempotent, and once none is up attaches go through
// again. An attachment leaves the count when it closes — detached, or its
// connection gone.
func TestTheCloseFenceIsReversible(t *testing.T) {
	h := newHost(t)
	a := h.dial()
	a.sayHello(nil)
	a.attach(attachParams(h))
	b := h.dial()
	b.sayHello(nil)

	n, release := h.srv.FenceAttaches()
	if n != 1 {
		t.Fatalf("the fence counts %d attachments, want 1", n)
	}
	refusedWith(t, b.call(protocol.MethodSessionAttach, attachParams(h)), protocol.RPCRefused, protocol.CodeUnavailable, protocol.ReasonClosing)
	n2, release2 := h.srv.FenceAttaches()
	if n2 != 1 {
		t.Fatalf("a second fence counts %d, want 1: a refused attach is not counted", n2)
	}
	release()
	release()
	refusedWith(t, b.call(protocol.MethodSessionAttach, attachParams(h)), protocol.RPCRefused, protocol.CodeUnavailable, protocol.ReasonClosing)
	release2()

	res := b.attach(attachParams(h))
	if got := h.srv.Attached(); got != 2 {
		t.Fatalf("with both attached the server counts %d", got)
	}
	_, resp := b.until(b.send(protocol.MethodSessionDetach, protocol.DetachParams{SessionID: sid(h), Subscription: res.Subscription}))
	ok[protocol.Empty](t, resp)
	// The detach's reply is queued and its attachment closed in one conn.mu
	// section, but the writer takes the reply without conn.mu: it can be on
	// the socket, and read, before that section has taken the attachment out
	// of the count. So the count is waited for, not read once.
	waitFor(t, "the detached attachment to leave the count", func() bool { return h.srv.Attached() != 2 })
	if got := h.srv.Attached(); got != 1 {
		t.Fatalf("after a detach the server counts %d, want 1", got)
	}
	// A connection the peer closed is a half-close to the server (plan 027
	// §3.7): its attachment stays live — and counted — until a write to it
	// fails, which the next event's forward does.
	a.close()
	h.publish(agent.Event{Type: agent.EventText, Text: "after the peer went"})
	waitFor(t, "the closed connection's attachment to leave the count", func() bool { return h.srv.Attached() == 0 })
}

// TestTheFenceCountsAReservedAttach (§3.6: "Attached counts every attachment
// the server holds in any state"): an attach reserved and not yet answered is
// in the count — the fence cannot tell the idle watcher nobody is coming — and
// it goes live once released, the fence having come and gone.
func TestTheFenceCountsAReservedAttach(t *testing.T) {
	gate := newHoldOnce()
	h := newHost(t, withHooks(control.TestHooks{Reserved: func(string) { gate.hold() }}), withOnClose(gate.release))
	b := h.dial()
	b.sayHello(nil)
	attach := b.send(protocol.MethodSessionAttach, attachParams(h))
	await(t, gate.arrived, "the attach to reserve")
	n, release := h.srv.FenceAttaches()
	release()
	if n != 1 {
		t.Fatalf("the fence counts %d attachments, want the reserved one", n)
	}
	gate.release()
	ok[protocol.AttachResult](t, b.reply(attach))
	if got := h.srv.Attached(); got != 1 {
		t.Fatalf("the live attachment is counted %d times", got)
	}
}

// armedHold holds an attach right after its reservation (TestHooks.Reserved)
// once armed, and only the first armed one: the attaches made before it is
// armed — a bystander's — go through.
type armedHold struct {
	armed atomic.Bool
	*holdOnce
}

func newArmedHold() *armedHold { return &armedHold{holdOnce: newHoldOnce()} }

func (g *armedHold) reserved(string) {
	if g.armed.Load() {
		g.hold()
	}
}

// heldAttach dials a connection, says hello and sends an attach that g holds
// pending, reserved and counted; it returns the connection.
func heldAttach(t *testing.T, h *host, g *armedHold) *client {
	t.Helper()
	b := h.dial()
	b.sayHello(nil)
	g.armed.Store(true)
	b.send(protocol.MethodSessionAttach, attachParams(h))
	await(t, g.arrived, "the attach to reserve")
	return b
}

// attachedIs fails unless the server counts want attachments.
func attachedIs(t *testing.T, h *host, want int, when string) {
	t.Helper()
	if n := h.srv.Attached(); n != want {
		t.Fatalf("%s the server counts %d attachments, want %d", when, n, want)
	}
}

// TestTheAttachmentCountFallsOnceOnEveryClose (§3.6: "Attached counts every
// attachment the server holds in any state"; astra r2-c1 3): an attachment
// leaves the count exactly once, by every route it can close by — besides a
// detach and a failed write (TestTheCloseFenceIsReversible): an attach
// abandoned while pending; one a replacement closed while pending and then
// abandoned (once, not twice); a live one's terminal reset, slow_consumer or
// session_closed; and the server's shutdown, live and pending alike. Each
// route ends with the count back where it was before the attachment was made:
// a missing decrement leaves the idle watcher (C5) seeing a client for ever,
// and a second one lets it miss a real one.
func TestTheAttachmentCountFallsOnceOnEveryClose(t *testing.T) {
	t.Run("an attach abandoned while pending", func(t *testing.T) {
		g := newArmedHold()
		h := newHost(t, withHooks(control.TestHooks{Reserved: g.reserved}), withOnClose(g.release))
		x := h.dial()
		x.sayHello(nil)
		x.attach(attachParams(h))
		attachedIs(t, h, 1, "with a bystander attached")
		b := heldAttach(t, h, g)
		attachedIs(t, h, 2, "with an attach pending")
		// Its client resumes on another connection: the pending attach's
		// connection is closed under it (hello's transfer closes it before
		// its answer), and the attach, let go, is abandoned.
		b2 := h.dial()
		b2.sayHello(b.resume())
		attachedIs(t, h, 2, "with the pending attach's connection closed and its handler held")
		g.release()
		waitFor(t, "the abandoned attach's handler to return", func() bool { return h.srv.Handlers() == 0 })
		attachedIs(t, h, 1, "after the abandon")
	})
	t.Run("a replacement closes a pending attach, and its abandon changes nothing", func(t *testing.T) {
		g := newArmedHold()
		h := newHost(t, withHooks(control.TestHooks{Reserved: g.reserved}), withOnClose(g.release))
		x := h.dial()
		x.sayHello(nil)
		x.attach(attachParams(h))
		heldAttach(t, h, g)
		attachedIs(t, h, 2, "with a live attachment and a pending one")
		h.srv.SetEngine(startedEngine(t))
		// The live attachment ends reset{session_replaced}; the pending one
		// was closed by the replacement itself.
		x.untilReset(protocol.ResetSessionReplaced)
		attachedIs(t, h, 0, "after the replacement")
		g.release()
		waitFor(t, "the replaced attach's handler to return", func() bool { return h.srv.Handlers() == 0 })
		attachedIs(t, h, 0, "after the replaced attach was abandoned too")
	})
	t.Run("a live attachment reset slow_consumer", func(t *testing.T) {
		gate := newForwardGate()
		h := newHost(t, withOnClose(gate.open), withHooks(control.TestHooks{BeforeForward: gate.hook}))
		a := h.dial()
		a.sayHello(nil)
		p := attachParams(h)
		p.Budget = &protocol.AttachBudget{MaxItems: 1}
		r := a.attach(p)
		a.note(protocol.NotifySynchronized)
		attachedIs(t, h, 1, "with the attachment live")
		gate.arm(r.After.Seq)
		for i := 0; h.dropped() == 0; i++ {
			if i == 16 {
				t.Fatal("the held subscription was never dropped")
			}
			h.publish(agent.Event{Type: agent.EventText, Text: fmt.Sprint(i)})
		}
		gate.open()
		a.untilReset(protocol.ResetSlowConsumer)
		attachedIs(t, h, 0, "after reset{slow_consumer}")
	})
	t.Run("live attachments reset session_closed", func(t *testing.T) {
		h := newHost(t)
		x, a := h.dial(), h.dial()
		x.sayHello(nil)
		a.sayHello(nil)
		x.attach(attachParams(h))
		a.attach(attachParams(h))
		attachedIs(t, h, 2, "with two attachments live")
		if err := h.eng.Close(); err != nil {
			t.Fatal(err)
		}
		x.untilReset(protocol.ResetSessionClosed)
		a.untilReset(protocol.ResetSessionClosed)
		attachedIs(t, h, 0, "after both reset{session_closed}")
		x.expectClosed()
		a.expectClosed()
	})
	t.Run("the server's shutdown, a live attachment and a pending one", func(t *testing.T) {
		g := newArmedHold()
		h := newHost(t, withHooks(control.TestHooks{Reserved: g.reserved}), withOnClose(g.release))
		x := h.dial()
		x.sayHello(nil)
		x.attach(attachParams(h))
		heldAttach(t, h, g)
		attachedIs(t, h, 2, "with a live attachment and a pending one")
		closed := make(chan error, 1)
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), watchdog)
			defer cancel()
			closed <- h.srv.Close(ctx)
		}()
		// Every connection is closed under the pending attach before it is
		// let go; the server's close then waits for its handler.
		waitFor(t, "the server to close every connection", func() bool { return h.srv.OpenConns() == 0 })
		g.release()
		select {
		case err := <-closed:
			if err != nil {
				t.Fatalf("server close: %v", err)
			}
		case <-time.After(watchdog):
			t.Fatalf("the server did not close in %s", watchdog)
		}
		attachedIs(t, h, 0, "after the server's shutdown")
	})
}

// TestTheInfoDocumentCarriesTheHostsFacts (§3.7, SF-60, SF-63): a host that
// sets its permission mode and start has them in every info document — the
// attach reply's and the roster row's — the start in UTC.
func TestTheInfoDocumentCarriesTheHostsFacts(t *testing.T) {
	started := time.Date(2026, 9, 28, 13, 0, 0, 0, time.FixedZone("CEST", 2*3600))
	h := newHost(t, func(c *hostConfig) {
		c.opts.PermissionMode = protocol.PermissionPrompt
		c.opts.StartedAt = started
	})
	c := h.dial()
	c.sayHello(nil)
	id := c.send(protocol.MethodSessionAttach, attachParams(h))
	line, err := c.readLine()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"permissionMode":"prompt"`, `"startedAt":"2026-09-28T11:00:00Z"`, `"stop":false`} {
		if !strings.Contains(string(line), want) {
			t.Errorf("the attach reply to %s does not carry %s: %s", id, want, line)
		}
	}
	d := h.dial()
	d.sayHello(nil)
	list := ok[protocol.SessionsListResult](t, d.call(protocol.MethodSessionsList, protocol.SessionsListParams{}))
	if row := list.Sessions[0]; row.PermissionMode != protocol.PermissionPrompt || !row.StartedAt.Equal(started) {
		t.Fatalf("the roster row carries %q and %v", row.PermissionMode, row.StartedAt)
	}
}

// TestStateAndRowsCarryTheLastTurn (§3.7, SF-57): session.state and a
// sessions.list row carry the last turn's ending — none before any turn, the
// turn's own id once it has ended, none again while the next runs.
func TestStateAndRowsCarryTheLastTurn(t *testing.T) {
	h := newHost(t)
	c := h.dial()
	c.sayHello(nil)
	state := func() protocol.StateResult {
		return ok[protocol.StateResult](t, c.call(protocol.MethodSessionState, protocol.StateParams{SessionID: sid(h)}))
	}
	row := func() protocol.SessionRow {
		return ok[protocol.SessionsListResult](t, c.call(protocol.MethodSessionsList, protocol.SessionsListParams{})).Sessions[0]
	}
	if st, r := state(), row(); st.LastTurn != nil || r.LastTurn != nil {
		t.Fatalf("before any turn: state %+v, row %+v", st.LastTurn, r.LastTurn)
	}

	prompt := func() string {
		res := ok[protocol.PromptResult](t, c.call(protocol.MethodSessionPrompt, protocol.PromptParams{SessionID: sid(h),
			CommandID: c.cmd(), Text: "hello", Mode: protocol.PromptQueue}))
		return res.Turn
	}
	first := prompt()
	waitFor(t, "the turn's ending", func() bool { return h.eng.State().LastTurn != nil })
	st, r := state(), row()
	if lt := st.LastTurn; lt == nil || lt.TurnID != first || lt.Outcome != protocol.TurnDone || lt.Err != "" ||
		lt.EndedAt.IsZero() || lt.EndedAt.Location() != time.UTC {
		t.Fatalf("the state's last turn is %+v, want %s done", lt, first)
	}
	if r.LastTurn == nil || *r.LastTurn != *st.LastTurn {
		t.Fatalf("the row's last turn is %+v, the state's %+v", r.LastTurn, st.LastTurn)
	}

	hung := h.stub.HangNext()
	second := prompt()
	await(t, hung, "the second turn to open")
	// The prompt's reply followed its started through the barrier: the
	// ending it supersedes is gone by now.
	if st, r := state(), row(); st.LastTurn != nil || r.LastTurn != nil || st.Turn != second {
		t.Fatalf("with %s running: state %+v on %q, row %+v", second, st.LastTurn, st.Turn, r.LastTurn)
	}
}
