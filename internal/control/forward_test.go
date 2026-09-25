package control_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/control"
	"github.com/charliek/craze/internal/journal"
	"github.com/charliek/craze/internal/protocol"
)

// replyCase is one mutating method for TestAReplyFollowsItsEvents: what puts
// the session where the method has something to do, and the call.
type replyCase struct {
	name  string
	setup func(t *testing.T, h *host, c *client)
	call  func(h *host, c *client) string
	// quiet says the command emits nothing before it returns, so its barrier
	// has nothing to wait for: a refusal, or a cancel, whose turn's ending is
	// the turn's own, later. refused says it is answered with an error.
	quiet, refused bool
}

// hangTurn opens a turn that stays open, through the attachment.
func hangTurn(t *testing.T, h *host, c *client) {
	t.Helper()
	hung := h.stub.HangNext()
	id := c.send(protocol.MethodSessionPrompt, protocol.PromptParams{SessionID: sid(h), CommandID: c.cmd(), Text: "running", Mode: protocol.PromptQueue})
	_, resp := c.until(id)
	ok[protocol.PromptResult](t, resp)
	await(t, hung, "the turn to open")
}

// queueRow queues a row behind the open turn, through the attachment.
func queueRow(t *testing.T, h *host, c *client, text string) string {
	t.Helper()
	id := c.send(protocol.MethodQueueAdd, protocol.QueueAddParams{SessionID: sid(h), CommandID: c.cmd(), Text: text})
	_, resp := c.until(id)
	row, err := agent.DecodeQueuedPrompt(ok[protocol.QueueAddResult](t, resp).Row)
	if err != nil {
		t.Fatal(err)
	}
	return row.ID
}

// TestAReplyFollowsItsEvents (A11; §3.6, SF-10): for every mutating method,
// with the attachment's forwarder held unscheduled by a hook on the first
// event the command emitted — held while the handler's reply barrier waits,
// for as long as the test likes: nothing in the barrier times out — the reply
// is on the wire after every event committed before the command returned (S0,
// read by the test at the barrier's door), whatever the command's answer.
func TestAReplyFollowsItsEvents(t *testing.T) {
	var row string
	prompt := func(mode protocol.PromptMode, text string) func(*host, *client) string {
		return func(h *host, c *client) string {
			return c.send(protocol.MethodSessionPrompt, protocol.PromptParams{SessionID: sid(h), CommandID: c.cmd(), Text: text, Mode: mode})
		}
	}
	withRow := func(t *testing.T, h *host, c *client) {
		hangTurn(t, h, c)
		row = queueRow(t, h, c, "queued")
	}
	cases := []replyCase{
		{name: "prompt queue", call: prompt(protocol.PromptQueue, "hello")},
		{name: "prompt send_now", call: prompt(protocol.PromptSendNow, "now")},
		{name: "prompt interject", setup: func(t *testing.T, h *host, c *client) {
			h.stub.SetProvider(agent.GrokProvider())
			hangTurn(t, h, c)
		}, call: prompt(protocol.PromptInterject, "and also")},
		{name: "cancel", setup: hangTurn, quiet: true, call: func(h *host, c *client) string {
			return c.send(protocol.MethodSessionCancel, protocol.CancelParams{SessionID: sid(h), CommandID: c.cmd()})
		}},
		// Nothing is armed: a Stub's turn ends at the arm's own cancel, so a
		// disarm that finds one armed cannot be scheduled here. Its refusal
		// takes the same barrier.
		{name: "disarm", quiet: true, refused: true, call: func(h *host, c *client) string {
			return c.send(protocol.MethodSessionDisarm, protocol.DisarmParams{SessionID: sid(h), CommandID: c.cmd()})
		}},
		{name: "queue add", setup: hangTurn, call: func(h *host, c *client) string {
			return c.send(protocol.MethodQueueAdd, protocol.QueueAddParams{SessionID: sid(h), CommandID: c.cmd(), Text: "later"})
		}},
		{name: "queue edit", setup: withRow, call: func(h *host, c *client) string {
			return c.send(protocol.MethodQueueEdit, protocol.QueueEditParams{SessionID: sid(h), CommandID: c.cmd(), RowID: row, Text: "edited"})
		}},
		{name: "queue remove", setup: withRow, call: func(h *host, c *client) string {
			return c.send(protocol.MethodQueueRemove, protocol.QueueRemoveParams{SessionID: sid(h), CommandID: c.cmd(), RowID: row})
		}},
		{name: "queue clear", setup: withRow, call: func(h *host, c *client) string {
			return c.send(protocol.MethodQueueClear, protocol.QueueClearParams{SessionID: sid(h), CommandID: c.cmd()})
		}},
		{name: "set", call: func(h *host, c *client) string {
			return c.send(protocol.MethodSessionSet, protocol.SetParams{SessionID: sid(h), CommandID: c.cmd(),
				Setting: protocol.Setting{Kind: protocol.SettingMode, Value: "plan"}})
		}},
		{name: "setTitle", call: func(h *host, c *client) string {
			return c.send(protocol.MethodSessionSetTitle, protocol.SetTitleParams{SessionID: sid(h), CommandID: c.cmd(), Title: "renamed"})
		}},
		// The Stub stops no sub-agent: a refusal, through the same barrier.
		{name: "subagent cancel", quiet: true, refused: true, call: func(h *host, c *client) string {
			return c.send(protocol.MethodSubagentCancel, protocol.SubagentCancelParams{SessionID: sid(h), CommandID: c.cmd(), AgentID: "sub-1"})
		}},
		{name: "asks answer", setup: func(t *testing.T, h *host, c *client) {
			h.publish(agent.Event{Type: agent.EventPermission, Permission: &agent.PermissionEvent{ID: "perm-1", Tool: "Shell",
				Options: []agent.PermissionOption{{OptionID: "allow", Name: "Allow", Kind: "allow_once"}}}})
		}, call: func(h *host, c *client) string {
			return c.send(protocol.MethodAsksAnswer, protocol.AsksAnswerParams{SessionID: sid(h), CommandID: c.cmd(), AskID: "perm-1",
				Answer: protocol.Answer{OptionID: "allow"}})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gate := newForwardGate()
			parked := newSignal()
			doors := newSignal()
			var h *host
			h = newHost(t, withOnClose(gate.open), withHooks(control.TestHooks{
				BeforeForward: gate.hook,
				BarrierWaits:  parked.fire,
				BeforeBarrier: func(string) {
					// S0: everything the command caused before it returned
					// is committed at or below it.
					seq, _ := h.eng.SyncSeq(context.Background())
					doors.fire(seq)
				},
			}))
			c := h.dial()
			c.sayHello(nil)
			c.attach(attachParams(h))
			c.note(protocol.NotifySynchronized)
			if tc.setup != nil {
				tc.setup(t, h, c)
			}
			_, head := c.sync(h)
			// What the setup's own commands left in the signals.
			for len(doors) > 0 {
				<-doors
			}
			for len(parked) > 0 {
				<-parked
			}

			gate.arm(head)
			id := tc.call(h, c)
			s0 := doors.await(t, "the command's barrier")
			switch {
			case tc.quiet && s0 != head:
				t.Fatalf("a quiet command emitted: S0 %d, head %d", s0, head)
			case !tc.quiet && s0 <= head:
				t.Fatalf("the command emitted nothing before it returned: S0 %d, head %d", s0, head)
			case !tc.quiet:
				// The forwarder holds the command's first event, and the
				// barrier waits for it: the reply is not queued.
				if held := gate.awaitHeld(t); held != head+1 {
					t.Fatalf("the forwarder is held at %d, want %d", held, head+1)
				}
				if seq := parked.await(t, "the barrier to wait"); seq < s0 {
					t.Fatalf("the barrier waits for %d, below S0 %d", seq, s0)
				}
			}
			gate.open()
			notes, resp := c.until(id)
			if tc.refused {
				if resp.Error == nil {
					t.Fatalf("the refusal succeeded: %s", resp.Result)
				}
			} else if resp.Error != nil {
				t.Fatalf("the command failed: %v", resp.Error)
			}
			if last := lastEvent(t, notes, head); last < s0 {
				t.Fatalf("the reply came after event %d, before event %d: it overtook its events", last, s0)
			}
		})
	}
}

// stall is a host whose client never reads, attached with a budget of 4
// records, and whose forwarder is blocked waiting for room in a full writer
// queue: 1 MiB records were published, each taken by the forwarder before the
// next, until the forwarder was seen waiting for room. With a hold, the first
// push that finds no room — the forwarder's — is held in its wait
// (control.TestHooks.OutboxFull: past its first look at its stop channel, and
// before the select on room and stop) until the hold is released.
type stall struct {
	h *host
	a *client
	r protocol.AttachResult
	// blocked is the seq of the record the forwarder is blocked on.
	blocked uint64
	// resets reports each final reset queued (control.TestHooks.AckQueued).
	resets signal
	n      int
}

func newStall(t *testing.T, held *hold) *stall {
	t.Helper()
	var full atomic.Int32
	var taken atomic.Uint64
	s := &stall{resets: newSignal()}
	var opts []hostOpt
	if held != nil {
		opts = append(opts, withOnClose(held.release))
	}
	s.h = newHost(t, append(opts, withHooks(control.TestHooks{
		OutboxFull: func() {
			full.Add(1)
			if held != nil {
				held.wait()
			}
		},
		BeforeForward: func(_ string, seq uint64) { taken.Store(seq) },
		AckQueued: func(_, method string) {
			if method == protocol.NotifyReset {
				s.resets.fire(0)
			}
		},
	}))...)
	s.a = s.h.dial()
	s.a.sayHello(nil)
	p := attachParams(s.h)
	p.Budget = &protocol.AttachBudget{MaxItems: 4}
	s.r = s.a.attach(p)
	s.a.note(protocol.NotifySynchronized)
	for full.Load() == 0 {
		if s.n == 128 {
			t.Fatal("128 MiB queued and the writer queue is not full")
		}
		s.publish(t)
		seq := s.h.head()
		waitFor(t, "the forwarder to take the record, or find no room for it", func() bool {
			return taken.Load() >= seq || full.Load() > 0
		})
	}
	// BeforeForward runs only before an event's push: the forwarder is blocked
	// on the last record it took.
	s.blocked = taken.Load()
	if s.h.dropped() != 0 {
		t.Fatal("the subscription was dropped before its forwarder was stuck")
	}
	return s
}

// publish publishes one more 1 MiB record, under a watchdog: a publish that
// waited on the stalled subscription would wait for ever.
func (s *stall) publish(t *testing.T) {
	t.Helper()
	typ := agent.EventText
	if s.n%2 == 1 {
		typ = agent.EventThought
	}
	s.n++
	published := make(chan struct{})
	go func() {
		s.h.publish(agent.Event{Type: typ, Text: strings.Repeat("x", 1<<20)})
		close(published)
	}()
	await(t, published, fmt.Sprintf("publish %d beside a stalled client", s.n))
}

// events reads the client's events, which must run on from after + 1 with no
// gap, up to the reset, and returns the reset and the last event's seq.
func (s *stall) events(t *testing.T) (protocol.ResetParams, uint64) {
	t.Helper()
	next := s.r.After.Seq + 1
	for {
		n := s.a.anyNote()
		if n.Method == protocol.NotifyReset {
			return paramsOf[protocol.ResetParams](t, n), next - 1
		}
		if seq, _ := eventOf(t, n); seq != next {
			t.Fatalf("event %d, want %d: a gap", seq, next)
		}
		next++
	}
}

// TestAStalledClientIsResetWithoutDelayingTheAgent (A5; §3.7, astra 10, 31;
// astra r8 6): a client that never reads fills its connection's writer queue,
// and from then on its forwarder can make no progress at all: its
// subscription backs up in the log, and the log drops it slow_consumer. No
// publish ever waited on it. A full queue does not hide the drop from the
// forwarder blocked for room: it sees the subscription end and queues
// reset{slow_consumer} in the queue's reserve AT ONCE — while the queue is
// still full, before the client has read a byte. When the client reads again
// it gets every event queued before the one the forwarder was blocked on,
// contiguous — that one is not sent: the client's cursor re-attach replays it —
// and then the reset; the connection goes on. Latency is V7's, never an
// assertion here.
func TestAStalledClientIsResetWithoutDelayingTheAgent(t *testing.T) {
	s := newStall(t, nil)
	for stuck := 0; s.h.dropped() == 0; stuck++ {
		if stuck == 16 {
			t.Fatal("the stalled subscription was never dropped")
		}
		s.publish(t)
	}
	s.resets.await(t, "reset{slow_consumer} to be queued while the client reads nothing")

	rp, last := s.events(t)
	if rp != (protocol.ResetParams{Subscription: "s-1", Reason: protocol.ResetSlowConsumer}) {
		t.Fatalf("the reset: %+v", rp)
	}
	switch {
	case last == s.r.After.Seq:
		t.Fatal("the reset came before any event")
	case last != s.blocked-1:
		t.Fatalf("the events end at %d; the forwarder was blocked on %d, which is not sent", last, s.blocked)
	}
	ok[protocol.StateResult](t, s.a.call(protocol.MethodSessionState, protocol.StateParams{SessionID: sid(s.h)}))
	if re := s.a.attach(attachParams(s.h)); re.Subscription != "s-2" {
		t.Fatalf("the re-attach: %+v", re)
	}
}

// TestAStalledClientStillGetsTheClosingRecords (§3.7, astra r8 6): the
// engine's close ends a subscription whose forwarder is blocked for room in a
// full writer queue, and the forwarder sees it end as it would a drop — but a
// log's close is not abandoned: the record it was blocked on and every record
// the close left undelivered still go out, waiting for room as the client
// reads, then reset{session_closed}, and the host closes the connection.
func TestAStalledClientStillGetsTheClosingRecords(t *testing.T) {
	s := newStall(t, nil)
	s.publish(t)
	s.publish(t)
	head := s.h.head()
	if err := s.h.eng.Close(); err != nil {
		t.Fatal(err)
	}
	rp, last := s.events(t)
	if rp != (protocol.ResetParams{Subscription: "s-1", Reason: protocol.ResetSessionClosed}) {
		t.Fatalf("the reset: %+v", rp)
	}
	if last < head {
		t.Fatalf("the events end at %d, before the head %d at the close (blocked on %d)", last, head, s.blocked)
	}
	s.a.expectClosed()
}

// TestAnEndedSubscriptionsBlockedEventStaysUnsent (§3.7, astra r10 4): the
// forwarder blocked for room is held inside its wait — past its first look at
// the subscription's Done, before the select on room and Done — while the
// subscription is dropped slow_consumer (Done closes) and the client reads the
// whole queue (room is made). Let go, the forwarder finds both ready, and the
// end wins: the event it was blocked on is not sent — reset{slow_consumer}
// follows the last event queued before it — and the connection goes on.
func TestAnEndedSubscriptionsBlockedEventStaysUnsent(t *testing.T) {
	held := newHold()
	s := newStall(t, held)
	await(t, held.entered, "the forwarder to be held in its wait for room")
	for stuck := 0; s.h.dropped() == 0; stuck++ {
		if stuck == 16 {
			t.Fatal("the stalled subscription was never dropped")
		}
		s.publish(t)
	}
	// Every event queued before the blocked one is read, so the writer has
	// written them — each 1 MiB, of a 32 MiB queue — and made room.
	for next := s.r.After.Seq + 1; next < s.blocked; next++ {
		if seq, _ := eventOf(t, s.a.anyNote()); seq != next {
			t.Fatalf("event %d, want %d: a gap", seq, next)
		}
	}
	held.release()
	switch n := s.a.anyNote(); n.Method {
	case protocol.NotifyReset:
		if rp := paramsOf[protocol.ResetParams](t, n); rp != (protocol.ResetParams{Subscription: "s-1", Reason: protocol.ResetSlowConsumer}) {
			t.Fatalf("the reset: %+v", rp)
		}
	case protocol.NotifyEvent:
		seq, _ := eventOf(t, n)
		t.Fatalf("event %d sent after its subscription ended (the forwarder was blocked on %d)", seq, s.blocked)
	default:
		t.Fatalf("want reset{slow_consumer}; got %s %s", n.Method, n.Params)
	}
	ok[protocol.StateResult](t, s.a.call(protocol.MethodSessionState, protocol.StateParams{SessionID: sid(s.h)}))
}

// TestABarrierAwaitsAClosingAttachment (A12; §3.7, astra r2 15, r3 15): a
// command commits its event N; a concurrent detach makes the attachment
// closing before N is forwarded; the command's barrier, still waiting, is
// released by the detach's reply — the attachment's terminal acknowledgement
// — and so the command's reply follows the detach's, and N, which the client
// asked no longer to be sent, never comes.
func TestABarrierAwaitsAClosingAttachment(t *testing.T) {
	gate := newForwardGate()
	parked := newSignal()
	h := newHost(t, withOnClose(gate.open), withHooks(control.TestHooks{BeforeForward: gate.hook, BarrierWaits: parked.fire}))
	a := h.dial()
	a.sayHello(nil)
	r := a.attach(attachParams(h))
	a.note(protocol.NotifySynchronized)

	gate.arm(r.After.Seq)
	add := a.send(protocol.MethodQueueAdd, protocol.QueueAddParams{SessionID: sid(h), CommandID: a.cmd(), Text: "N"})
	gate.awaitHeld(t)
	parked.await(t, "the barrier to wait for N")
	detach := a.send(protocol.MethodSessionDetach, protocol.DetachParams{SessionID: sid(h), Subscription: r.Subscription})
	// The detach makes the attachment closing: the barrier wakes, and waits
	// on, for N or the terminal acknowledgement.
	parked.await(t, "the barrier to wait on a closing attachment")
	gate.open()

	ok[protocol.Empty](t, a.reply(detach))
	ok[protocol.QueueAddResult](t, a.reply(add))
	notes, _ := a.sync(h)
	if len(notes) != 0 {
		t.Fatalf("after the detach: %s %s", notes[0].Method, notes[0].Params)
	}
}

// TestAReattachWaitsForTheOldReset (A12; §3.7, astra r2 15): an attachment
// whose subscription has ended is closing until its reset is queued, and a
// new attach meanwhile is already_attached; once the reset is out, the new
// attach is answered — after the old stream's tail.
func TestAReattachWaitsForTheOldReset(t *testing.T) {
	gate := newForwardGate()
	ending := newSignal()
	release := make(chan struct{})
	var once atomic.Bool
	letGo := func() {
		if once.CompareAndSwap(false, true) {
			close(release)
		}
	}
	h := newHost(t, withOnClose(gate.open), withOnClose(letGo), withHooks(control.TestHooks{
		BeforeForward: gate.hook,
		BeforeTerminal: func(string, protocol.ResetReason) {
			ending.fire(0)
			<-release
		},
	}))
	a := h.dial()
	a.sayHello(nil)
	p := attachParams(h)
	p.Budget = &protocol.AttachBudget{MaxItems: 1}
	r := a.attach(p)
	a.note(protocol.NotifySynchronized)

	gate.arm(r.After.Seq)
	for i := 0; h.dropped() == 0; i++ {
		if i == 16 {
			t.Fatal("the held subscription was never dropped")
		}
		h.publish(agent.Event{Type: agent.EventText, Text: fmt.Sprint(i)})
	}
	gate.open()
	ending.await(t, "the forwarder to reach its terminal acknowledgement")
	refusedWith(t, a.replyThrough(a.send(protocol.MethodSessionAttach, attachParams(h))),
		protocol.RPCRefused, protocol.CodeBadRequest, protocol.ReasonAlreadyAttached)
	letGo()
	for {
		n := a.anyNote()
		if n.Method == protocol.NotifyReset {
			if rp := paramsOf[protocol.ResetParams](t, n); rp.Reason != protocol.ResetSlowConsumer {
				t.Fatalf("the reset: %+v", rp)
			}
			break
		}
	}
	if re := a.attach(attachParams(h)); re.Subscription != "s-2" {
		t.Fatalf("the attach after the reset: %+v", re)
	}
}

// replyThrough reads up to the reply to id, allowing events before it.
func (c *client) replyThrough(id string) *protocol.Response {
	c.t.Helper()
	notes, resp := c.until(id)
	for _, n := range notes {
		if n.Method != protocol.NotifyEvent {
			c.t.Fatalf("before the reply to %s: %s %s", id, n.Method, n.Params)
		}
	}
	return resp
}

// TestTheClosingRecordsReachAnUnscheduledForwarder (A21; §3.7, astra 9): a
// healthy forwarder held unscheduled across the engine's Close still delivers
// the session's closing records — the running turn's synthetic closing ending,
// an open ask's closing ending — from its subscription's Rest, then
// reset{session_closed}, and the host closes the connection.
func TestTheClosingRecordsReachAnUnscheduledForwarder(t *testing.T) {
	gate := newForwardGate()
	h := newHost(t, withOnClose(gate.open), withHooks(control.TestHooks{BeforeForward: gate.hook}))
	a := h.dial()
	a.sayHello(nil)
	a.attach(attachParams(h))
	a.note(protocol.NotifySynchronized)
	hangTurn(t, h, a)
	h.publish(agent.Event{Type: agent.EventPermission, Permission: &agent.PermissionEvent{ID: "perm-1", Tool: "Shell",
		Options: []agent.PermissionOption{{OptionID: "allow", Name: "Allow", Kind: "allow_once"}}}})
	_, head := a.sync(h)

	gate.arm(head)
	h.publish(agent.Event{Type: agent.EventText, Text: "the last before the close"})
	gate.awaitHeld(t)
	if err := h.eng.Close(); err != nil {
		t.Fatal(err)
	}
	gate.open()

	var turnEnded, askEnded bool
	next := head + 1
	for {
		m := a.next()
		if m.note == nil {
			t.Fatalf("a reply at the session's end: %s", describe(m))
		}
		if m.note.Method == protocol.NotifyReset {
			if rp := paramsOf[protocol.ResetParams](t, m.note); rp.Reason != protocol.ResetSessionClosed {
				t.Fatalf("the reset: %+v", rp)
			}
			break
		}
		seq, ev := eventOf(t, m.note)
		if seq != next {
			t.Fatalf("event %d, want %d", seq, next)
		}
		next++
		if ev.Type == agent.EventTurn && ev.Turn != nil && ev.Turn.Phase == agent.TurnEnded && ev.Turn.StopReason == "closing" {
			turnEnded = true
		}
		if ev.Type == agent.EventAsk && ev.Ask != nil && ev.Ask.ID == "perm-1" && ev.Ask.Outcome == agent.AskClosing {
			askEnded = true
		}
	}
	if !turnEnded || !askEnded {
		t.Fatalf("the closing records: the turn's ending %v, the ask's %v", turnEnded, askEnded)
	}
	a.expectClosed()
}

// TestASessionClosedWithoutItsRestIsSessionClosed (§3.7, astra r2 14): a
// subscription whose journal leg was still streaming when the log closed has
// no whole tail (ErrRestUnavailable), so its client is sent
// reset{session_closed} with no final records — never a suffix posing as the
// tail — and the connection closes.
func TestASessionClosedWithoutItsRestIsSessionClosed(t *testing.T) {
	w, lo := testJournal(t)
	lo.RingEvents = 4
	gate := newForwardGate()
	h := newHost(t, withLog(lo), withOnClose(gate.open), withHooks(control.TestHooks{BeforeForward: gate.hook}))
	for i := range 20 {
		h.publish(agent.Event{Type: agent.EventText, Text: fmt.Sprint(i)})
	}
	head := h.head()
	flushed(t, w, head)
	a := h.dial()
	a.sayHello(nil)
	gate.arm(0)
	p := attachParams(h)
	p.Cursor = &protocol.Cursor{Incarnation: h.eng.State().Incarnation, Seq: 0}
	r := a.attach(p)
	if r.Snapshot != nil || r.Reset != "" {
		t.Fatalf("the cursor was not honoured: %+v", r)
	}
	if held := gate.awaitHeld(t); held != 1 {
		t.Fatalf("held at %d", held)
	}
	if err := h.eng.Close(); err != nil {
		t.Fatal(err)
	}
	gate.open()
	if seq, _ := eventOf(t, a.note(protocol.NotifyEvent)); seq != 1 {
		t.Fatalf("the held event is %d", seq)
	}
	if rp := paramsOf[protocol.ResetParams](t, a.note(protocol.NotifyReset)); rp.Reason != protocol.ResetSessionClosed {
		t.Fatalf("the reset: %+v", rp)
	}
	a.expectClosed()
}

// TestAFailedJournalReplayIsReplayFailed (§3.4): a cursor whose head has left
// the ring is honoured from the journal, and when the file leg finds a line it
// cannot read after delivering a prefix, the subscription ends with an
// asynchronous ErrCursorUnresolvable: reset{replay_failed}.
func TestAFailedJournalReplayIsReplayFailed(t *testing.T) {
	w, lo := testJournal(t)
	lo.RingEvents = 8
	h := newHost(t, withLog(lo))
	for i := range 40 {
		h.publish(agent.Event{Type: agent.EventText, Text: fmt.Sprint(i)})
	}
	head := h.head()
	if head != 40 {
		t.Fatalf("the premise: head %d", head)
	}
	flushed(t, w, head)
	corruptJournalLine(t, w, 15)
	a := h.dial()
	a.sayHello(nil)
	p := attachParams(h)
	p.Cursor = &protocol.Cursor{Incarnation: h.eng.State().Incarnation, Seq: 5}
	if r := a.attach(p); r.Snapshot != nil || r.Reset != "" {
		t.Fatalf("the cursor was not honoured synchronously: %+v", r)
	}
	for want := uint64(6); want < 15; want++ {
		if seq, _ := eventOf(t, a.note(protocol.NotifyEvent)); seq != want {
			t.Fatalf("event %d, want %d", seq, want)
		}
	}
	if rp := paramsOf[protocol.ResetParams](t, a.note(protocol.NotifyReset)); rp.Reason != protocol.ResetReplayFailed {
		t.Fatalf("the reset: %+v", rp)
	}
	if re := a.attach(attachParams(h)); re.Snapshot == nil || re.After.Seq != head {
		t.Fatalf("the re-attach with no cursor: %+v", re)
	}
}

// corruptJournalLine breaks seq's event line in w's file in place, its first
// byte, so the reader finds it malformed only when it gets there.
func corruptJournalLine(t *testing.T, w *journal.Writer, seq uint64) {
	t.Helper()
	raw, err := os.ReadFile(w.Path())
	if err != nil {
		t.Fatal(err)
	}
	i := bytes.Index(raw, fmt.Appendf(nil, `"type":"event","seq":%d,`, seq))
	if i < 0 {
		t.Fatalf("seq %d has no line in the journal", seq)
	}
	start := bytes.LastIndexByte(raw[:i], '\n') + 1
	f, err := os.OpenFile(w.Path(), os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteAt([]byte("#"), int64(start)); err != nil {
		t.Fatal(err)
	}
}

// TestAnOmittedRecordIsReset (§3.4): a record with Omitted set — an event over
// the log's record limit, which no client can fold — is never sent as an
// event: the subscription ends reset{omitted}, after every record before it,
// and a re-attach with no cursor gets a snapshot past it.
func TestAnOmittedRecordIsReset(t *testing.T) {
	h := newHost(t, withLog(agent.EventLogOptions{MaxRecordBytes: 4 << 10}))
	a := h.dial()
	a.sayHello(nil)
	r := a.attach(attachParams(h))
	a.note(protocol.NotifySynchronized)
	h.publish(agent.Event{Type: agent.EventText, Text: "small"})
	h.publish(agent.Event{Type: agent.EventText, Text: strings.Repeat("o", 8<<10)})
	if seq, ev := eventOf(t, a.note(protocol.NotifyEvent)); seq != r.After.Seq+1 || ev.Text != "small" {
		t.Fatalf("the event before the omitted one: %d %+v", seq, ev)
	}
	if rp := paramsOf[protocol.ResetParams](t, a.note(protocol.NotifyReset)); rp != (protocol.ResetParams{Subscription: "s-1", Reason: protocol.ResetOmitted}) {
		t.Fatalf("the reset: %+v", rp)
	}
	if re := a.attach(attachParams(h)); re.Snapshot == nil || re.After.Seq != r.After.Seq+2 {
		t.Fatalf("the re-attach: %+v", re)
	}
}

// TestReplacingTheEngineClosesEveryConnection (A12; §3.6, astra 14): replacing
// the engine sends an attached connection reset{session_replaced} and then
// closes it; every other connection — bound or not — is closed at once; and
// the old binding is gone: its token is answered resumed: false.
func TestReplacingTheEngineClosesEveryConnection(t *testing.T) {
	h := newHost(t)
	a, b, u := h.dial(), h.dial(), h.dial()
	a.sayHello(nil)
	b.sayHello(nil)
	r := a.attach(attachParams(h))
	a.note(protocol.NotifySynchronized)

	h.srv.SetEngine(startedEngine(t))
	if rp := paramsOf[protocol.ResetParams](t, a.note(protocol.NotifyReset)); rp != (protocol.ResetParams{Subscription: r.Subscription, Reason: protocol.ResetSessionReplaced}) {
		t.Fatalf("the reset: %+v", rp)
	}
	a.expectClosed()
	b.expectClosed()
	u.expectClosed()
	x := h.dial()
	if hx := x.sayHello(a.resume()); hx.Resumed {
		t.Fatalf("a replaced engine's client was resumed: %+v", hx)
	}
}

// TestTheEngineEndingClosesEveryConnection (§3.7, astra r5 13): when the
// engine closes, every connection bound to it closes too, once what it owes is
// written — an idle one at once; an attached one after its final records and
// reset{session_closed}; a half-closed one with a command in flight after that
// command's reply — and its client is released; a connection saying hello to
// the ended engine is answered and closed.
func TestTheEngineEndingClosesEveryConnection(t *testing.T) {
	h := newHost(t)
	idle, att, half := h.dial(), h.dial(), h.dial()
	idle.sayHello(nil)
	att.sayHello(nil)
	half.sayHello(nil)
	att.attach(attachParams(h))
	att.note(protocol.NotifySynchronized)
	held, releaseSet := h.stub.HoldNextSet()
	t.Cleanup(releaseSet)
	set := half.send(protocol.MethodSessionSet, protocol.SetParams{SessionID: sid(h), CommandID: half.cmd(),
		Setting: protocol.Setting{Kind: protocol.SettingModel, Value: "fast"}})
	await(t, held, "the Set to park")
	if err := half.nc.CloseWrite(); err != nil {
		t.Fatal(err)
	}

	if err := h.eng.Close(); err != nil {
		t.Fatal(err)
	}
	idle.expectClosed()
	for {
		n := att.anyNote()
		if n.Method == protocol.NotifyReset {
			if rp := paramsOf[protocol.ResetParams](t, n); rp.Reason != protocol.ResetSessionClosed {
				t.Fatalf("the reset: %+v", rp)
			}
			break
		}
	}
	att.expectClosed()
	if m := half.next(); m.resp == nil || string(m.resp.ID) != set {
		t.Fatalf("the half-closed connection's reply: %s", describe(m))
	}
	half.expectClosed()
	// Each is closed, and so its client released (unbind precedes the note).
	// The half-closed one's reason is whichever of its two ends — the peer's,
	// the session's — was seen last.
	for _, c := range []*client{idle, att} {
		h.logs.wait(t, "close client "+c.hello.ClientID, "session ended")
	}
	h.logs.wait(t, "close client "+half.hello.ClientID)

	late := h.dial()
	late.sayHello(nil)
	late.expectClosed()
}

// TestAClosingLogsReplyFollowsTheFinalRecords (§3.6, §3.7, CodeRabbit 15): a
// command whose barrier meets a closing log has no seq to wait for, so it
// waits for the attachment's terminal acknowledgement — with the forwarder
// held on the command's own event across the close, as long as the test likes
// — and the command's event arrives among the session's final records, then
// reset{session_closed}, then the command's own successful reply — never an
// error — and then the host closes the connection.
func TestAClosingLogsReplyFollowsTheFinalRecords(t *testing.T) {
	gate := newForwardGate()
	parked := newSignal()
	var h *host
	var once atomic.Bool
	h = newHost(t, withOnClose(gate.open), withHooks(control.TestHooks{
		BeforeForward: gate.hook,
		BarrierWaits:  parked.fire,
		BeforeBarrier: func(method string) {
			if method == protocol.MethodQueueAdd && once.CompareAndSwap(false, true) {
				_ = h.eng.Close()
			}
		},
	}))
	a := h.dial()
	a.sayHello(nil)
	r := a.attach(attachParams(h))
	a.note(protocol.NotifySynchronized)
	gate.arm(r.After.Seq)
	id := a.send(protocol.MethodQueueAdd, protocol.QueueAddParams{SessionID: sid(h), CommandID: a.cmd(), Text: "at the end"})
	gate.awaitHeld(t)
	parked.await(t, "the barrier to wait for the terminal acknowledgement")
	gate.open()
	queued := false
	for {
		n := a.anyNote()
		if n.Method == protocol.NotifyReset {
			if rp := paramsOf[protocol.ResetParams](t, n); rp.Reason != protocol.ResetSessionClosed {
				t.Fatalf("the reset: %+v", rp)
			}
			break
		}
		if _, ev := eventOf(t, n); ev.Type == agent.EventQueue && ev.Queue != nil && ev.Queue.Text == "at the end" {
			queued = true
		}
	}
	if !queued {
		t.Fatal("the command's event was not among the final records")
	}
	row := ok[protocol.QueueAddResult](t, a.reply(id))
	if q, err := agent.DecodeQueuedPrompt(row.Row); err != nil || q.Text != "at the end" {
		t.Fatalf("the row: %+v, %v", q, err)
	}
	a.expectClosed()
}

// TestAHalfClosedConnectionKeepsDelivering (§3.7, astra 11): a read-side EOF
// is "no more requests": a live subscription keeps the connection open and
// delivering, and it closes when the session ends, after its final reset.
func TestAHalfClosedConnectionKeepsDelivering(t *testing.T) {
	h := newHost(t)
	a := h.dial()
	a.sayHello(nil)
	a.attach(attachParams(h))
	a.note(protocol.NotifySynchronized)
	if err := a.nc.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		h.publish(agent.Event{Type: agent.EventText, Text: fmt.Sprint("after the half-close ", i)})
	}
	for range 3 {
		a.note(protocol.NotifyEvent)
	}
	if err := h.eng.Close(); err != nil {
		t.Fatal(err)
	}
	if rp := paramsOf[protocol.ResetParams](t, a.note(protocol.NotifyReset)); rp.Reason != protocol.ResetSessionClosed {
		t.Fatalf("the reset: %+v", rp)
	}
	a.expectClosed()
}

// TestAnOwedReadyIsNotLostToTheClose (§3.4, astra r8 5): a when: "now" attach
// made before the start is owed a ready at N, the log's committed head once
// the start ran, and its forwarder is held below N, so N is among the records
// the engine's close leaves in the subscription's Rest. The closing tail still
// queues the ready as soon as its position reaches N: the wire shows the
// records up to N, then ready, then the rest, then reset{session_closed}. The
// same holds when the watcher has decided the note but not yet handed it over
// when the forwarder reaches its tail: the tail waits for its decision.
func TestAnOwedReadyIsNotLostToTheClose(t *testing.T) {
	for _, tc := range []struct {
		name string
		// inFlight holds the watcher between deciding the note and handing it
		// over until the forwarder has begun its terminal acknowledgement.
		inFlight bool
	}{
		{name: "the note handed over"},
		{name: "the note still being handed over", inFlight: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gate := newForwardGate()
			owed := newSignal()
			watcher := newHold()
			hooks := control.TestHooks{
				BeforeForward: gate.hook,
				ReadyOwed:     func(_ string, seq uint64) { owed.fire(seq) },
			}
			if tc.inFlight {
				hooks.ReadyDecided = func(_ string, seq uint64) {
					owed.fire(seq)
					watcher.wait()
				}
				// The forwarder has read Records closed, polled for a note and
				// found none: only now may the watcher hand it over.
				hooks.BeforeTerminal = func(string, protocol.ResetReason) { watcher.release() }
			}
			h := newHost(t, withoutStart(), withOnClose(gate.open), withOnClose(watcher.release), withHooks(hooks))
			a := h.dial()
			a.sayHello(nil)
			p := attachParams(h)
			p.When = protocol.WhenNow
			r := a.attach(p)
			if r.Ready {
				t.Fatalf("the premise: %+v", r)
			}
			a.note(protocol.NotifySynchronized)

			gate.arm(r.After.Seq)
			h.publish(agent.Event{Type: agent.EventText, Text: "held"}, agent.Event{Type: agent.EventText, Text: "before the start"})
			if held := gate.awaitHeld(t); held != r.After.Seq+1 {
				t.Fatalf("the forwarder is held at %d, want %d", held, r.After.Seq+1)
			}
			if err := h.eng.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			n := owed.await(t, "the ready to be owed")
			if n <= r.After.Seq+1 {
				t.Fatalf("the ready is owed at %d, not past the held record %d: the premise", n, r.After.Seq+1)
			}
			h.publish(agent.Event{Type: agent.EventText, Text: "after the start"}, agent.Event{Type: agent.EventText, Text: "the last"})
			if err := h.eng.Close(); err != nil {
				t.Fatal(err)
			}
			gate.open()

			next, readyAfter := r.After.Seq+1, uint64(0)
			for {
				m := a.anyNote()
				switch m.Method {
				case protocol.NotifyReady:
					if readyAfter != 0 {
						t.Fatal("a second ready")
					}
					if rp := paramsOf[protocol.ReadyParams](t, m); rp.Subscription != r.Subscription || rp.StartFailed {
						t.Fatalf("the ready: %+v", rp)
					}
					readyAfter = next - 1
					continue
				case protocol.NotifyReset:
					if rp := paramsOf[protocol.ResetParams](t, m); rp.Reason != protocol.ResetSessionClosed {
						t.Fatalf("the reset: %+v", rp)
					}
				default:
					if seq, _ := eventOf(t, m); seq != next {
						t.Fatalf("event %d, want %d", seq, next)
					}
					next++
					continue
				}
				break
			}
			switch {
			case readyAfter == 0:
				t.Fatalf("no ready before reset{session_closed}; it was owed at %d, and the events ran to %d", n, next-1)
			case readyAfter != n:
				t.Fatalf("the ready came after event %d, want right after %d", readyAfter, n)
			case next-1 <= n:
				t.Fatalf("the events end at %d: none after the ready's %d", next-1, n)
			}
			a.expectClosed()
		})
	}
}

// TestReplacementWinsTheTerminalReset (§3.6, §3.7; astra r8): a forwarder
// that has queued its closed log's final records and is about to queue
// reset{session_closed} is held there while the engine is replaced. The reset
// it then queues is session_replaced — its reason is decided in the section
// that queues it, under the flag the replacement sets — and the connection
// closes after it.
func TestReplacementWinsTheTerminalReset(t *testing.T) {
	gate := newForwardGate()
	atReset := make(chan protocol.ResetReason, 1)
	reset := newHold()
	h := newHost(t, withOnClose(gate.open), withOnClose(reset.release), withHooks(control.TestHooks{
		BeforeForward: gate.hook,
		BeforeReset: func(_ string, reason protocol.ResetReason) {
			atReset <- reason
			reset.wait()
		},
	}))
	a := h.dial()
	a.sayHello(nil)
	r := a.attach(attachParams(h))
	a.note(protocol.NotifySynchronized)
	gate.arm(r.After.Seq)
	h.publish(agent.Event{Type: agent.EventText, Text: "held"}, agent.Event{Type: agent.EventText, Text: "in the rest"})
	gate.awaitHeld(t)
	if err := h.eng.Close(); err != nil {
		t.Fatal(err)
	}
	gate.open()
	select {
	case reason := <-atReset:
		if reason != protocol.ResetSessionClosed {
			t.Fatalf("the premise: the forwarder was about to send %s", reason)
		}
	case <-time.After(watchdog):
		t.Fatal("the forwarder never reached its reset")
	}
	h.srv.SetEngine(startedEngine(t))
	reset.release()

	events := 0
	for {
		n := a.anyNote()
		if n.Method == protocol.NotifyReset {
			if rp := paramsOf[protocol.ResetParams](t, n); rp != (protocol.ResetParams{Subscription: r.Subscription, Reason: protocol.ResetSessionReplaced}) {
				t.Fatalf("the reset after the replacement: %+v", rp)
			}
			break
		}
		eventOf(t, n)
		events++
	}
	if events < 2 {
		t.Fatalf("%d events before the reset: the final records were not queued first", events)
	}
	a.expectClosed()
}

// TestNothingIsWrittenAfterSessionReplaced (§3.6, astra r8 7): once a
// replacement's reset{session_replaced} is queued, the connection admits no
// further line. A handler still running — a read held across the replacement
// and let go only once the reset is on the socket, or a command whose reply
// barrier the reset releases — has its reply dropped at the push, not queued:
// with the writer held right after writing the reset, nothing is in the
// queue behind it, and the client reads the reset and then the close.
func TestNothingIsWrittenAfterSessionReplaced(t *testing.T) {
	type schedule struct {
		hooks control.TestHooks
		// before runs once the client is attached (synchronized read): it puts
		// a handler where the replacement will find it.
		before func(t *testing.T, h *host, a *client)
		// replaced runs right after the replacement.
		replaced func()
		// written runs once the reset is on the socket and the writer held.
		written func()
		// release lets go of whatever the schedule holds, before the server's
		// close.
		release func()
	}
	for _, tc := range []struct {
		name string
		make func() schedule
	}{
		{name: "a read held across the replacement", make: func() schedule {
			reply := newHold()
			return schedule{
				hooks: control.TestHooks{BeforeReply: func(method string) {
					if method == protocol.MethodSessionState {
						reply.wait()
					}
				}},
				before: func(t *testing.T, h *host, a *client) {
					a.send(protocol.MethodSessionState, protocol.StateParams{SessionID: sid(h)})
					await(t, reply.entered, "the read to hold its reply")
				},
				replaced: func() {},
				written:  reply.release,
				release:  reply.release,
			}
		}},
		{name: "a command waiting on its barrier", make: func() schedule {
			gate := newForwardGate()
			parked := newSignal()
			return schedule{
				hooks: control.TestHooks{BeforeForward: gate.hook, BarrierWaits: parked.fire},
				before: func(t *testing.T, h *host, a *client) {
					gate.arm(h.head())
					a.send(protocol.MethodQueueAdd, protocol.QueueAddParams{SessionID: sid(h), CommandID: a.cmd(), Text: "N"})
					gate.awaitHeld(t)
					parked.await(t, "the barrier to wait for N")
				},
				// The forwarder's push of N is refused now, and it queues the
				// reset, which releases the barrier.
				replaced: gate.open,
				written:  func() {},
				release:  gate.open,
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := tc.make()
			writer := newHold()
			s.hooks.ResetWritten = writer.wait
			h := newHost(t, withOnClose(writer.release), withOnClose(s.release), withHooks(s.hooks))
			a := h.dial()
			a.sayHello(nil)
			r := a.attach(attachParams(h))
			a.note(protocol.NotifySynchronized)
			s.before(t, h, a)
			h.srv.SetEngine(startedEngine(t))
			s.replaced()
			await(t, writer.entered, "reset{session_replaced} to be written")
			s.written()
			waitFor(t, "the held handler to return", func() bool { return h.srv.Handlers() == 0 })
			if q := h.srv.Queued(); q != 0 {
				t.Fatalf("%d lines queued behind reset{session_replaced}", q)
			}
			writer.release()
			if rp := paramsOf[protocol.ResetParams](t, a.note(protocol.NotifyReset)); rp != (protocol.ResetParams{Subscription: r.Subscription, Reason: protocol.ResetSessionReplaced}) {
				t.Fatalf("the reset: %+v", rp)
			}
			a.expectClosed()
		})
	}
}

// TestAReplacementDuringADetachStillAnswersIt (§3.6, §3.7; astra r10 8): a
// detach claims the attachment's end, and the engine is replaced before the
// detach queues its reply. The forwarder, stopped by the detach, queues no
// reset, so the detach's reply — the attachment's terminal acknowledgement —
// is the last line the replaced connection owes, and it closes only once that
// reply is on the socket: with the writer held off the reply until the
// detach's handler has returned (and settled), the client still reads {} and
// then the close, and no reset{session_replaced}, which a client that
// detached is not owed.
func TestAReplacementDuringADetachStillAnswersIt(t *testing.T) {
	detaching, writer := newHold(), newHold()
	var detach atomic.Value // the detach's request id, once sent
	h := newHost(t, withOnClose(detaching.release), withOnClose(writer.release), withHooks(control.TestHooks{
		Detaching: func(string) { detaching.wait() },
		BeforeWrite: func(line []byte) {
			var r struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
			}
			if id, _ := detach.Load().(string); id != "" && json.Unmarshal(line, &r) == nil && r.Method == "" && string(r.ID) == id {
				writer.wait()
			}
		},
	}))
	a := h.dial()
	a.sayHello(nil)
	r := a.attach(attachParams(h))
	a.note(protocol.NotifySynchronized)
	id, line := a.encode(protocol.MethodSessionDetach, protocol.DetachParams{SessionID: sid(h), Subscription: r.Subscription})
	detach.Store(id)
	a.write(line)
	await(t, detaching.entered, "the detach to claim the attachment's end")
	h.srv.SetEngine(startedEngine(t))
	detaching.release()
	// The writer cannot put the reply on the socket before the handler that
	// queued it has returned: whatever that handler's settle decided about the
	// connection, it decided with the reply unwritten.
	waitFor(t, "the detach's handler to return", func() bool { return h.srv.Handlers() == 0 })
	writer.release()
	ok[protocol.Empty](t, a.reply(id))
	select {
	case <-writer.entered:
	default:
		t.Fatal("the premise: the writer was never held on the detach's reply")
	}
	a.expectClosed()
}

// TestADetachBeforeTheCommandOwesItNothing (§3.6, §3.7; astra r8 3): the other
// order of TestABarrierAwaitsAClosingAttachment. The detach is answered
// before the command commits, so the command's barrier finds the attachment
// closed — owed no events, since its client asked for none — and waits for
// nothing: the command's reply is the next line after the detach's, with no
// event of the command's before it.
func TestADetachBeforeTheCommandOwesItNothing(t *testing.T) {
	command := newHold()
	parked := newSignal()
	h := newHost(t, withOnClose(command.release), withHooks(control.TestHooks{
		BeforeCommand: func(method string) {
			if method == protocol.MethodQueueAdd {
				command.wait()
			}
		},
		BarrierWaits: parked.fire,
	}))
	a := h.dial()
	a.sayHello(nil)
	r := a.attach(attachParams(h))
	a.note(protocol.NotifySynchronized)
	add := a.send(protocol.MethodQueueAdd, protocol.QueueAddParams{SessionID: sid(h), CommandID: a.cmd(), Text: "N"})
	await(t, command.entered, "the command to reach its engine call")
	ok[protocol.Empty](t, a.detach(h, r.Subscription))
	command.release()
	row := ok[protocol.QueueAddResult](t, a.reply(add))
	if q, err := agent.DecodeQueuedPrompt(row.Row); err != nil || q.Text != "N" {
		t.Fatalf("the row: %+v, %v", q, err)
	}
	if len(parked) != 0 {
		t.Fatalf("the barrier waited on a closed attachment, for %d", <-parked)
	}
}
