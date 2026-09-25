package control_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/atomicfile"
	"github.com/charliek/craze/internal/control"
	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/paths"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/sessions"
)

// bigTranscript folds n entries of about 64 KiB each into the session's main
// transcript — text and thought alternating, so each is an entry of its own —
// so a snapshot of it is about n × 64 KiB.
func bigTranscript(h *host, n int) {
	text := strings.Repeat("x", 60<<10)
	for i := range n {
		typ := agent.EventText
		if i%2 == 1 {
			typ = agent.EventThought
		}
		h.stub.Emit(agent.Event{Type: typ, Text: text})
	}
}

func snapshotParams(h *host, budget int) protocol.SnapshotParams {
	p := protocol.SnapshotParams{SessionID: sid(h)}
	if budget > 0 {
		p.Budget = &protocol.SnapshotBudget{SnapshotBytes: budget}
	}
	return p
}

// TestARequestThenEOFStillGetsItsReply (§3.7, astra 11; A18): a read-side EOF
// is half-close — no more requests — and every request admitted before it
// still gets its reply, a slow one included; the host closes the connection
// once nothing is in flight.
func TestARequestThenEOFStillGetsItsReply(t *testing.T) {
	h := newHost(t)
	held, releaseSet := h.stub.HoldNextSet()
	t.Cleanup(releaseSet)
	a := h.dial()
	a.sayHello(nil)
	state := a.send(protocol.MethodSessionState, protocol.StateParams{SessionID: sid(h)})
	set := a.send(protocol.MethodSessionSet, protocol.SetParams{SessionID: sid(h), CommandID: a.cmd(),
		Setting: protocol.Setting{Kind: protocol.SettingModel, Value: "fast"}})
	await(t, held, "the Set to park")
	if err := a.nc.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	if r := a.read(); string(r.ID) != state || r.Error != nil {
		t.Fatalf("the state's reply after EOF: %+v", r)
	}
	releaseSet()
	if r := a.read(); string(r.ID) != set || r.Error != nil {
		t.Fatalf("the Set's reply after EOF: %+v", r)
	}
	a.expectEOF()
	h.logs.wait(t, "conn 1 close", "closed by the peer")
}

// TestANonReadingPeerStallsAtSixteen (§3.7, astra 10): a request's admission
// slot is held until its reply is WRITTEN, so a peer that sends and never
// reads stops the reader at RequestsPerConnection admitted requests; the host
// and every other connection go on; and once the peer reads, every reply
// arrives.
func TestANonReadingPeerStallsAtSixteen(t *testing.T) {
	full := make(chan struct{}, 1)
	h := newHost(t, withHooks(control.TestHooks{AdmissionFull: func() {
		select {
		case full <- struct{}{}:
		default:
		}
	}}))
	bigTranscript(h, 8)
	a := h.dial()
	a.sayHello(nil)
	const sent = 24
	for range sent {
		a.send(protocol.MethodSessionSnapshot, snapshotParams(h, 0))
	}
	await(t, full, "the reader to stop at the admission bound")
	// Every handler has queued its reply and returned, and still no slot is
	// given back: a slot is held until its reply is WRITTEN, not queued, and
	// none of these can be written while the peer does not read.
	waitFor(t, "every admitted request's reply to be queued", func() bool { return h.srv.Handlers() == 0 })
	if got := h.srv.Admitted(); len(got) != 1 || got[0] != protocol.RequestsPerConnection {
		t.Fatalf("admitted %v with every reply queued, want one connection still at %d", got, protocol.RequestsPerConnection)
	}
	// The host is unharmed: another connection is served, and a command runs.
	b := h.dial()
	b.sayHello(nil)
	ok[protocol.StateResult](t, b.call(protocol.MethodSessionState, protocol.StateParams{SessionID: sid(h)}))
	ok[protocol.QueueAddResult](t, queueAdd(b, b.cmd(), "unharmed"))
	for range sent {
		if r := a.read(); r.Error != nil {
			t.Fatalf("a stalled request's reply: %v", r.Error)
		}
	}
}

// TestTheByteBudgetBoundsAPeerThatNeverReads (§3.7, astra 10): every
// outbound byte counts against the connection's 32 MiB budget until it is on
// the socket, so replies a peer never reads never hold more than the budget
// less the reset reserve; the handlers past it wait for room, and deliver once
// the peer reads.
func TestTheByteBudgetBoundsAPeerThatNeverReads(t *testing.T) {
	full := make(chan struct{}, 1)
	h := newHost(t, withHooks(control.TestHooks{OutboxFull: func() {
		select {
		case full <- struct{}{}:
		default:
		}
	}}))
	bigTranscript(h, 40) // a snapshot of about 2.4 MB: sixteen are over the budget
	a := h.dial()
	a.sayHello(nil)
	for range protocol.RequestsPerConnection {
		a.send(protocol.MethodSessionSnapshot, snapshotParams(h, protocol.SnapshotBytesMax))
	}
	await(t, full, "a reply to wait for room in the budget")
	if high, limit := h.srv.OutboxHighWater(), protocol.WriterQueueBytes-protocol.ResetReserveBytes; high > limit || high < limit/2 {
		t.Fatalf("the outbox held %d bytes at most; the budget is %d", high, limit)
	}
	for range protocol.RequestsPerConnection {
		r := a.read()
		if s := ok[protocol.SnapshotResult](t, r); len(s.Snapshot)*protocol.RequestsPerConnection <= protocol.WriterQueueBytes {
			t.Fatalf("a snapshot of %d bytes: the premise wants sixteen of them over the budget", len(s.Snapshot))
		}
	}
	if high, limit := h.srv.OutboxHighWater(), protocol.WriterQueueBytes-protocol.ResetReserveBytes; high > limit {
		t.Fatalf("the outbox held %d bytes, over the budget of %d", high, limit)
	}
}

// TestAWriterThatMakesNoProgressIsClosed (§3.7): a writer that writes nothing
// for the stall bound closes its connection; the session and every other
// connection are untouched. The bound is 60 s; a test seam lowers it.
func TestAWriterThatMakesNoProgressIsClosed(t *testing.T) {
	h := newHost(t, withStall(200*time.Millisecond))
	bigTranscript(h, 8)
	a := h.dial()
	a.sayHello(nil)
	for range 4 {
		a.send(protocol.MethodSessionSnapshot, snapshotParams(h, 0))
	}
	h.logs.wait(t, "conn 1 close", "write stalled")
	b := h.dial()
	b.sayHello(nil)
	ok[protocol.QueueAddResult](t, queueAdd(b, b.cmd(), "untouched"))
}

// TestTheCommandCapAnswersBusy (§3.6, astra 7): past CommandsPerHost commands
// in flight across every connection a command is refused unavailable, reason
// busy — not run and never stored — and the same command id succeeds once
// there is room.
func TestTheCommandCapAnswersBusy(t *testing.T) {
	store := newGateStore()
	h := newHost(t, withIndex(engine.IndexOptions{Store: store, CWD: "/w", Provider: "cursor"}))
	t.Cleanup(store.open)
	var conns []*client
	for n := 0; n < protocol.CommandsPerHost; {
		c := h.dial()
		c.sayHello(nil)
		conns = append(conns, c)
		for i := 0; i < protocol.RequestsPerConnection-1 && n < protocol.CommandsPerHost; i++ {
			c.send(protocol.MethodSessionSetTitle, protocol.SetTitleParams{SessionID: sid(h), CommandID: c.cmd(), Title: "held"})
			n++
		}
	}
	waitFor(t, "every command at the cap", func() bool { return h.srv.Commands() == protocol.CommandsPerHost })

	x := h.dial()
	x.sayHello(nil)
	e := refusedWith(t, queueAdd(x, "1", "over the cap"), protocol.RPCRefused, protocol.CodeUnavailable, protocol.ReasonBusy)
	if !protocol.Retry(e.Data.Code) {
		t.Fatalf("busy must be resent under the same id: %s", e.Data.Code)
	}
	if st := ok[protocol.StateResult](t, x.call(protocol.MethodSessionState, protocol.StateParams{SessionID: sid(h)})); len(st.Queue) != 0 {
		t.Fatalf("a busy command ran: the queue holds %s", st.Queue)
	}
	store.open()
	for _, c := range conns {
		for range c.nextCmd {
			if r := c.read(); r.Error != nil {
				t.Fatalf("a held command: %v", r.Error)
			}
		}
	}
	ok[protocol.QueueAddResult](t, queueAdd(x, "1", "over the cap"))
}

// TestALogClosingBarrierLeavesASuccessASuccess (§3.7, CodeRabbit 15): a
// command that succeeded and then met a closing log at its reply barrier is
// answered with its own result, never an error; session.sync, which has no
// seq to answer with then, is not_accepting.
func TestALogClosingBarrierLeavesASuccessASuccess(t *testing.T) {
	var h *host
	var once sync.Once
	h = newHost(t, withHooks(control.TestHooks{BeforeBarrier: func(method string) {
		if method == protocol.MethodQueueAdd {
			once.Do(func() { _ = h.eng.Close() })
		}
	}}))
	a := h.dial()
	a.sayHello(nil)
	row := ok[protocol.QueueAddResult](t, queueAdd(a, a.cmd(), "made it"))
	if q, err := agent.DecodeQueuedPrompt(row.Row); err != nil || q.Text != "made it" {
		t.Fatalf("the row: %+v, %v", q, err)
	}
	refusedWith(t, a.call(protocol.MethodSessionSync, protocol.SyncParams{SessionID: sid(h)}),
		protocol.RPCRefused, protocol.CodeNotAccepting, protocol.ReasonNotAccepting)
}

// TestServerCloseReturnsWhileAnIndexLockIsHeld (§3.7, astra r2 16; A12): a
// SetTitle parked in the session index's file lock — which nothing can
// interrupt — does not hold Server.Close past its context: Close joins the
// transport goroutines, counts the handler, and returns at the deadline; the
// command still completes into the engine's receipts table once the lock is
// let go.
func TestServerCloseReturnsWhileAnIndexLockIsHeld(t *testing.T) {
	t.Setenv("CRAZE_HOME", t.TempDir())
	path := paths.SessionsPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	unlock, err := atomicfile.Lock(path + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	release := func() { once.Do(unlock) }
	t.Cleanup(release)
	h := newHost(t, withIndex(engine.IndexOptions{Store: &sessions.Store{}, CWD: "/w", Provider: "cursor"}))
	a := h.dial()
	a.sayHello(nil)
	a.send(protocol.MethodSessionSetTitle, protocol.SetTitleParams{SessionID: sid(h), CommandID: "1", Title: "parked"})
	waitFor(t, "the SetTitle in its engine call", func() bool { return h.srv.Commands() == 1 })

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	closed := make(chan error, 1)
	go func() { closed <- h.srv.Close(ctx) }()
	select {
	case err := <-closed:
		if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "1 handlers still running") {
			t.Fatalf("Close: %v, want its deadline with the one handler counted", err)
		}
	case <-time.After(watchdog):
		t.Fatal("Close waited on a command parked in the index lock")
	}
	a.expectEOF()
	release()
	waitFor(t, "the parked command to complete", func() bool { return h.srv.Handlers() == 0 })
	if st := h.eng.State(); st.Title != "parked" {
		t.Fatalf("the rename did not complete: title %q", st.Title)
	}
}
