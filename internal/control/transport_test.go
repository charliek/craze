package control_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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

// pipeListener is a listener over in-memory pipes (net.Pipe), which move
// exactly the bytes the peer reads: dial hands the server one end, wrapped so
// the time the server closes it is known, and returns the other.
type pipeListener struct {
	conns chan net.Conn
	done  chan struct{}
	once  sync.Once
}

// servePipes serves a pipeListener on srv until the test ends.
func servePipes(t *testing.T, srv *control.Server) *pipeListener {
	t.Helper()
	l := &pipeListener{conns: make(chan net.Conn), done: make(chan struct{})}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(l) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), watchdog)
		defer cancel()
		_ = srv.Close(ctx)
		if err := <-served; err != nil {
			t.Errorf("serve pipes: %v", err)
		}
	})
	return l
}

func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *pipeListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}

func (l *pipeListener) Addr() net.Addr { return testAddr{} }

// dial connects a pipe and returns the peer's end and when the server closed
// its own.
func (l *pipeListener) dial(t *testing.T) (net.Conn, <-chan time.Time) {
	t.Helper()
	tc := &timedConn{closed: make(chan time.Time, 1)}
	peer := l.dialWrapped(t, func(server net.Conn) net.Conn {
		tc.Conn = server
		return tc
	})
	return peer, tc.closed
}

// dialWrapped connects a pipe, hands the server its end as wrap returns it,
// and returns the peer's end.
func (l *pipeListener) dialWrapped(t *testing.T, wrap func(net.Conn) net.Conn) net.Conn {
	t.Helper()
	server, peer := net.Pipe()
	t.Cleanup(func() { _ = peer.Close() })
	select {
	case l.conns <- wrap(server):
	case <-time.After(watchdog):
		t.Fatal("the server did not accept the pipe")
	}
	return peer
}

// timedConn is the server's end of a pipe: it records when it was closed.
type timedConn struct {
	net.Conn
	closed chan time.Time
	once   sync.Once
}

func (c *timedConn) Close() error {
	c.once.Do(func() { c.closed <- time.Now() })
	return c.Conn.Close()
}

type testAddr struct{}

func (testAddr) Network() string { return "test" }
func (testAddr) String() string  { return "test" }

// TestAWriterThatMakesNoProgressIsClosed (§3.7; astra r5): a writer that
// moves no byte for the stall bound closes its connection — measured from the
// LAST BYTE THAT MOVED, never from the start of a write: a peer that reads a
// few bytes of a reply and then stops is closed one bound after those bytes,
// not two. The session and every other connection are untouched. The bound is
// 60 s; a test seam lowers it. The peer is a pipe, so the bytes it reads are
// exactly the bytes that moved.
func TestAWriterThatMakesNoProgressIsClosed(t *testing.T) {
	const stall = 500 * time.Millisecond
	h := newHost(t, withStall(stall))
	peer, closed := servePipes(t, h.srv).dial(t)
	_ = peer.SetDeadline(time.Now().Add(watchdog))
	lr := protocol.NewLineReader(peer, protocol.OutboundLineMax)
	if _, err := peer.Write(requestLine(t, "1", protocol.MethodHello, helloParams(nil))); err != nil {
		t.Fatal(err)
	}
	if _, err := lr.ReadLine(); err != nil {
		t.Fatal(err)
	}
	if _, err := peer.Write(requestLine(t, "2", protocol.MethodSessionState, protocol.StateParams{SessionID: sid(h)})); err != nil {
		t.Fatal(err)
	}
	// Ten bytes of the reply move, and then none.
	if _, err := io.ReadFull(peer, make([]byte, 10)); err != nil {
		t.Fatal(err)
	}
	moved := time.Now()
	select {
	case at := <-closed:
		if since := at.Sub(moved); since < stall/2 || since >= stall*3/2 {
			t.Fatalf("closed %s after the last byte moved; the bound is %s", since, stall)
		}
	case <-time.After(watchdog):
		t.Fatal("the stalled connection was never closed")
	}
	h.logs.wait(t, "close", "write stalled")
	b := h.dial()
	b.sayHello(nil)
	ok[protocol.QueueAddResult](t, queueAdd(b, b.cmd(), "untouched"))
}

// noDeadlineConn is the server's end of a pipe whose SetWriteDeadline fails
// once armed, leaving the pipe open — a net.Conn that cannot install a write
// deadline. It counts the writes made after it was armed.
type noDeadlineConn struct {
	net.Conn
	armed  atomic.Bool
	writes atomic.Int32
}

func (c *noDeadlineConn) SetWriteDeadline(t time.Time) error {
	if c.armed.Load() {
		return errors.New("write deadlines are not supported")
	}
	return c.Conn.SetWriteDeadline(t)
}

func (c *noDeadlineConn) Write(b []byte) (int, error) {
	if c.armed.Load() {
		c.writes.Add(1)
	}
	return c.Conn.Write(b)
}

// TestAWriteDeadlineThatCannotBeSetClosesTheConnection (plan 027 X15, astra r6
// 5): a SetWriteDeadline that fails is a failed write — the connection closes
// and its client is released — and nothing is written with no deadline in
// place, where a peer that stops reading could block the writer forever. A
// Unix socket's never fails so; the server takes any net.Listener, and a
// wrapper stands in for one whose conn does.
func TestAWriteDeadlineThatCannotBeSetClosesTheConnection(t *testing.T) {
	h := newHost(t)
	nd := &noDeadlineConn{}
	peer := servePipes(t, h.srv).dialWrapped(t, func(server net.Conn) net.Conn {
		nd.Conn = server
		return nd
	})
	_ = peer.SetDeadline(time.Now().Add(watchdog))
	lr := protocol.NewLineReader(peer, protocol.OutboundLineMax)
	if _, err := peer.Write(requestLine(t, "1", protocol.MethodHello, helloParams(nil))); err != nil {
		t.Fatal(err)
	}
	line, err := lr.ReadLine()
	if err != nil {
		t.Fatal(err)
	}
	var resp protocol.Response
	if err := json.Unmarshal(line, &resp); err != nil {
		t.Fatal(err)
	}
	hello := ok[protocol.HelloResult](t, &resp)

	nd.armed.Store(true)
	if _, err := peer.Write(requestLine(t, "2", protocol.MethodSessionState, protocol.StateParams{SessionID: sid(h)})); err != nil {
		t.Fatal(err)
	}
	if line, err := lr.ReadLine(); !errors.Is(err, io.EOF) {
		t.Fatalf("read %q, %v: want the host to close the connection", line, err)
	}
	if n := nd.writes.Load(); n != 0 {
		t.Fatalf("%d writes with no deadline in place", n)
	}
	h.logs.wait(t, "close", "client "+hello.ClientID+":", "write failed", "deadline")

	// Released: nobody resumes it, and it retires once released for the age
	// bound, holding nothing — a client still bound never would.
	h.clock.advance(pastRetirement)
	b := h.dial()
	if hb := b.sayHello(&protocol.Resume{ClientID: hello.ClientID, Token: hello.Token}); hb.Resumed || hb.ClientID == hello.ClientID {
		t.Fatalf("the client of the closed connection was never released: %+v", hb)
	}
}

// failingListener fails every Accept with a transient error until it is
// closed.
type failingListener struct {
	done chan struct{}
	once sync.Once
}

func (l *failingListener) Accept() (net.Conn, error) {
	select {
	case <-l.done:
		return nil, net.ErrClosed
	default:
		return nil, errors.New("too many open files")
	}
}

func (l *failingListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}

func (l *failingListener) Addr() net.Addr { return testAddr{} }

// TestCloseHonoursItsDeadlineDuringAcceptBackoff (astra r5 6): an accept loop
// waiting out its backoff after a transient accept failure is woken by Close,
// so Close returns by its deadline — here long before the backoff, raised to
// a minute by a test seam, would have ended.
func TestCloseHonoursItsDeadlineDuringAcceptBackoff(t *testing.T) {
	logs := newLogSink()
	srv := control.NewForTest(control.Options{Log: logs.log}, control.TestHooks{}, 0, 0)
	srv.SetAcceptBackoff(time.Minute, time.Minute)
	served := make(chan error, 1)
	go func() { served <- srv.Serve(&failingListener{done: make(chan struct{})}) }()
	logs.wait(t, "control: accept:", "retrying in 1m0s")

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	closed := make(chan error, 1)
	go func() { closed <- srv.Close(ctx) }()
	select {
	case err := <-closed:
		if err != nil || ctx.Err() != nil {
			t.Fatalf("Close: %v, with its context %v: want nil before its deadline", err, ctx.Err())
		}
	case <-time.After(watchdog):
		t.Fatal("Close waited out the accept loop's backoff")
	}
	if err := <-served; err != nil {
		t.Fatalf("Serve after Close: %v", err)
	}
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
