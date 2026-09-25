package control_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"

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

// TestAStalledClientIsResetWithoutDelayingTheAgent (A5; §3.7, astra 10, 31):
// a client that never reads fills its connection's writer queue — each record
// is taken by the forwarder before the next is published, until the forwarder
// is seen waiting for room — and from then on its forwarder can make no
// progress at all: its subscription backs up in the log, and the log drops it
// slow_consumer. Every publish is made under a watchdog while the forwarder is
// stuck — one that waited on the stalled subscription would wait for ever — so
// no publish ever waited on it. When the client reads again it gets every
// event it was sent, contiguous, and then reset{slow_consumer}; the connection
// goes on. Latency is V7's, never an assertion here.
func TestAStalledClientIsResetWithoutDelayingTheAgent(t *testing.T) {
	var full atomic.Int32
	var taken atomic.Uint64
	h := newHost(t, withHooks(control.TestHooks{
		OutboxFull:    func() { full.Add(1) },
		BeforeForward: func(_ string, seq uint64) { taken.Store(seq) },
	}))
	a := h.dial()
	a.sayHello(nil)
	p := attachParams(h)
	p.Budget = &protocol.AttachBudget{MaxItems: 4}
	r := a.attach(p)
	a.note(protocol.NotifySynchronized)

	payload := strings.Repeat("x", 1<<20)
	publish := func(i int) {
		typ := agent.EventText
		if i%2 == 1 {
			typ = agent.EventThought
		}
		published := make(chan struct{})
		go func() {
			h.publish(agent.Event{Type: typ, Text: payload})
			close(published)
		}()
		await(t, published, fmt.Sprintf("publish %d beside a stalled client", i))
	}
	i := 0
	for ; full.Load() == 0; i++ {
		if i == 128 {
			t.Fatal("128 MiB queued and the writer queue is not full")
		}
		publish(i)
		seq := h.head()
		waitFor(t, "the forwarder to take the record, or find no room for it", func() bool {
			return taken.Load() >= seq || full.Load() > 0
		})
	}
	if h.dropped() != 0 {
		t.Fatal("the subscription was dropped before its forwarder was stuck")
	}
	for stuck := 0; h.dropped() == 0; stuck++ {
		if stuck == 16 {
			t.Fatal("the stalled subscription was never dropped")
		}
		publish(i)
		i++
	}

	next := r.After.Seq + 1
	for {
		n := a.anyNote()
		if n.Method == protocol.NotifyReset {
			if rp := paramsOf[protocol.ResetParams](t, n); rp != (protocol.ResetParams{Subscription: "s-1", Reason: protocol.ResetSlowConsumer}) {
				t.Fatalf("the reset: %+v", rp)
			}
			break
		}
		if seq, _ := eventOf(t, n); seq != next {
			t.Fatalf("event %d, want %d: a gap", seq, next)
		}
		next++
	}
	if next == r.After.Seq+1 {
		t.Fatal("the reset came before any event")
	}
	ok[protocol.StateResult](t, a.call(protocol.MethodSessionState, protocol.StateParams{SessionID: sid(h)}))
	if re := a.attach(attachParams(h)); re.Subscription != "s-2" {
		t.Fatalf("the re-attach: %+v", re)
	}
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
