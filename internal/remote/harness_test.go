package remote_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	"github.com/charliek/craze/internal/control"
	"github.com/charliek/craze/internal/control/wiretest"
	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/journal"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/remote"
	"github.com/charliek/craze/internal/sessions"
	"github.com/charliek/craze/internal/transcript"
	"github.com/charliek/craze/internal/tui"
)

// The tests run the client against the real server (internal/control) over
// real Unix sockets in a SHORT temporary directory (t.TempDir() overflows
// sun_path on macOS), in front of a real engine over tui's Stub with no
// primary. Every connection the client makes goes through a tap
// (Options.Dial): it holds every line either side wrote to the schema
// (wiretest), records them, and lets a test kill a connection, hold or fail a
// dial, hold a request on its way out, and rewrite what the client reads.

// watchdog bounds every wait a test makes.
const watchdog = 10 * time.Second

// pastRetirement is past the receipts table's age bound (10 minutes): a
// released client holding no entry is retired by then; a binding lives twice
// that, so its token is still known.
const pastRetirement = 11 * time.Minute

// ------------------------------------------------------------------ host

// clock is the receipts table's clock and the binding table's: the wall
// clock plus an offset a test moves past their age bounds.
type clock struct{ offset atomic.Int64 }

func (c *clock) now() time.Time              { return time.Now().Add(time.Duration(c.offset.Load())) }
func (c *clock) advance(d time.Duration)     { c.offset.Add(int64(d)) }
func (h *host) advanceClock(d time.Duration) { h.clock.advance(d) }

// logSink collects the server's Options.Log lines and lets a test wait for
// one.
type logSink struct {
	mu    sync.Mutex
	lines []string
	grew  chan struct{}
}

func newLogSink() *logSink { return &logSink{grew: make(chan struct{})} }

func (l *logSink) log(s string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, s)
	close(l.grew)
	l.grew = make(chan struct{})
}

// wait returns the first line containing every one of subs, waiting for it.
func (l *logSink) wait(t *testing.T, subs ...string) string {
	t.Helper()
	deadline := time.After(watchdog)
	for {
		l.mu.Lock()
		for _, line := range l.lines {
			all := true
			for _, sub := range subs {
				all = all && strings.Contains(line, sub)
			}
			if all {
				l.mu.Unlock()
				return line
			}
		}
		grew := l.grew
		l.mu.Unlock()
		select {
		case <-grew:
		case <-deadline:
			l.mu.Lock()
			defer l.mu.Unlock()
			t.Fatalf("no log line with %q in %s; the log:\n%s", subs, watchdog, strings.Join(l.lines, "\n"))
		}
	}
}

// hostConfig is what newHost builds from. The server is control.New's: its
// test hooks live in its own package's tests, so every schedule here is made
// from the client's side (the tap) and the engine's public seams.
type hostConfig struct {
	engine    engine.Options
	noStart   bool
	session   func(*tui.Stub) agent.Session
	workspace string
}

type hostOpt func(*hostConfig)

func withoutStart() hostOpt                   { return func(c *hostConfig) { c.noStart = true } }
func withIndex(o engine.IndexOptions) hostOpt { return func(c *hostConfig) { c.engine.Index = o } }

// withWorkspace is the host's workspace, which every info document carries.
func withWorkspace(ws string) hostOpt { return func(c *hostConfig) { c.workspace = ws } }

// host is a server on a socket in front of an engine over a Stub.
type host struct {
	t     *testing.T
	stub  *tui.Stub
	sess  agent.Session
	eng   *engine.Engine
	srv   *control.Server
	path  string
	clock *clock
	logs  *logSink
}

// shortDir is a fresh directory short enough for a socket path.
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "czr-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func newHost(t *testing.T, opts ...hostOpt) *host {
	t.Helper()
	cfg := hostConfig{workspace: "/work"}
	for _, o := range opts {
		o(&cfg)
	}
	h := &host{t: t, clock: &clock{}, logs: newLogSink(), path: filepath.Join(shortDir(t), "s")}
	h.stub = tui.NewStubNoPrimary()
	h.sess = h.stub
	if cfg.session != nil {
		h.sess = cfg.session(h.stub)
	}
	cfg.engine.ReceiptClock = h.clock.now
	var err error
	h.eng, err = engine.New(h.sess, cfg.engine)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.eng.Close() })
	if !cfg.noStart {
		if err := h.eng.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	h.srv = control.New(control.Options{Log: h.logs.log, Workspace: cfg.workspace, Clock: h.clock.now})
	h.srv.SetEngine(h.eng)
	l, err := net.Listen("unix", h.path)
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- h.srv.Serve(l) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), watchdog)
		defer cancel()
		if err := h.srv.Close(ctx); err != nil {
			t.Errorf("server close: %v", err)
		}
		if err := <-served; err != nil {
			t.Errorf("serve: %v", err)
		}
	})
	return h
}

// sid is the durable craze session id the host serves.
func (h *host) sid() string { return h.eng.State().CrazeSessionID }

// head is the log's committed head.
func (h *host) head() uint64 {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), watchdog)
	defer cancel()
	seq, err := h.eng.SyncSeq(ctx)
	if err != nil {
		h.t.Fatal(err)
	}
	return seq
}

// publish publishes evs into the log the engine observes: the Stub's, or a
// logSession's.
func (h *host) publish(evs ...agent.Event) {
	h.t.Helper()
	for _, ev := range evs {
		ls, ok := h.sess.(*logSession)
		if !ok {
			h.stub.Emit(ev)
			continue
		}
		if ev.At.IsZero() {
			ev.At = time.Now()
		}
		if !ls.log.Publish(context.Background(), nil, ev) {
			h.t.Fatal("a publish was refused")
		}
	}
}

// text publishes one text event.
func (h *host) text(s string) {
	h.t.Helper()
	h.publish(agent.Event{Type: agent.EventText, Text: s})
}

// dropped is how many subscriptions the host's log has dropped slow.
func (h *host) dropped() int {
	if ls, ok := h.sess.(*logSession); ok {
		return ls.log.Health().SubscribersDropped
	}
	return h.stub.EventLog().Health().SubscribersDropped
}

// logSession is the host's Stub over an event log the test built — a small
// ring, a small record limit, a journal — so the log's own edges can be
// reached over the socket. The engine publishes into it and observes it.
type logSession struct {
	*tui.Stub
	log  *agent.EventLog
	asks *agent.AskRegistry
}

// withLog serves a logSession over a log built with lo (always NoPrimary).
func withLog(lo agent.EventLogOptions) hostOpt {
	return func(c *hostConfig) {
		c.session = func(s *tui.Stub) agent.Session {
			lo.NoPrimary = true
			l := agent.NewEventLog(lo)
			return &logSession{Stub: s, log: l, asks: agent.NewAskRegistry(l, time.Now)}
		}
	}
}

func (s *logSession) EventLog() *agent.EventLog  { return s.log }
func (s *logSession) Asks() *agent.AskRegistry   { return s.asks }
func (s *logSession) Events() <-chan agent.Event { return s.log.Primary() }
func (s *logSession) Incarnation() string        { return s.log.Incarnation() }
func (s *logSession) Subscribe(o agent.SubscribeOptions) (*agent.Subscription, error) {
	return s.log.Subscribe(o)
}

func (s *logSession) Close() error {
	s.asks.Close()
	err := s.Stub.Close()
	s.log.Close(context.Background())
	return err
}

// testJournal is a journal in a fresh directory, and the log options that
// feed it.
func testJournal(t *testing.T) (*journal.Writer, agent.EventLogOptions) {
	t.Helper()
	inc := agent.NewIncarnation()
	w, err := journal.New(journal.Options{
		Dir: filepath.Join(t.TempDir(), "journal"), Incarnation: inc, Cwd: t.TempDir(),
		Provider: "test", EventCodec: agent.EventCodecVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = w.Close(context.Background())
		ctx, cancel := context.WithTimeout(context.Background(), watchdog)
		defer cancel()
		_ = w.WaitFlushed(ctx, journal.MaxSeq)
	})
	return w, agent.EventLogOptions{Journal: w, Incarnation: inc}
}

// flushed waits until w has written seq.
func flushed(t *testing.T, w *journal.Writer, seq uint64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), watchdog)
	defer cancel()
	if err := w.WaitFlushed(ctx, seq); err != nil {
		t.Fatal(err)
	}
}

// corruptJournalLine breaks seq's event line in w's file in place, so the
// reader finds it malformed only when it gets there.
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

// gateStore is a session index whose every Upsert parks until the gate
// opens: a command held in the index as a contended flock would hold it.
type gateStore struct {
	gate    chan struct{}
	once    sync.Once
	entered atomic.Int32
}

func newGateStore() *gateStore { return &gateStore{gate: make(chan struct{})} }

func (g *gateStore) Upsert(sessions.Row) error {
	g.entered.Add(1)
	<-g.gate
	return nil
}

func (g *gateStore) open() { g.once.Do(func() { close(g.gate) }) }

// ------------------------------------------------------------------- tap

// wireLine is one line on one of the tap's connections.
type wireLine struct {
	conn   int
	out    bool // the client wrote it
	raw    []byte
	id     string
	method string // a request's or a notification's; a reply's request's
	params json.RawMessage
	resp   *protocol.Response
}

// tap is the client's Dial: it wraps every connection it opens.
type tap struct {
	t  *testing.T
	mu sync.Mutex
	// conns is every connection opened, in order.
	conns []*tapConn
	lines []wireLine
	errs  []string
	dials int
	// dialGate, when set, holds every dial until it is closed; dialFail,
	// when set, fails every dial with it.
	dialGate chan struct{}
	dialHeld chan struct{}
	dialFail error
	// dialTo, when set, is where every dial goes instead of the path asked.
	dialTo string
	// holdOut, when set, holds a line the client is writing for which it
	// returns true, until the channel it returns is closed.
	holdOut func(l wireLine) <-chan struct{}
	// rewriteIn replaces a line the host wrote with the lines it returns,
	// before the client reads them (after they are held to the schema);
	// rewriteOut does the same for what the client writes (before it is
	// sent, after it is held to the schema).
	rewriteIn  func(l wireLine) [][]byte
	rewriteOut func(l wireLine) []byte
	// stallOut, when set, makes a line the client is writing for which it
	// returns true block as a write to a full socket does — until the
	// connection is closed or its write deadline passes, when it fails
	// having written nothing: a host that has stopped reading. The line is
	// recorded as sent, and never reaches the host.
	stallOut func(l wireLine) bool
	changed  chan struct{}
}

func newTap(t *testing.T) *tap {
	tp := &tap{t: t, changed: make(chan struct{})}
	// Registered first, so it runs last: after every client and server is
	// closed, nothing more is read.
	t.Cleanup(func() {
		tp.mu.Lock()
		defer tp.mu.Unlock()
		for _, e := range tp.errs {
			t.Error(e)
		}
	})
	return tp
}

func (tp *tap) changedLocked() {
	close(tp.changed)
	tp.changed = make(chan struct{})
}

func (tp *tap) dial(ctx context.Context, path string) (net.Conn, error) {
	tp.mu.Lock()
	tp.dials++
	gate := tp.dialGate
	if gate != nil && tp.dialHeld != nil {
		close(tp.dialHeld)
		tp.dialHeld = nil
	}
	tp.changedLocked()
	tp.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	tp.mu.Lock()
	fail := tp.dialFail
	if tp.dialTo != "" {
		path = tp.dialTo
	}
	tp.mu.Unlock()
	if fail != nil {
		return nil, fail
	}
	var d net.Dialer
	nc, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		return nil, err
	}
	tp.mu.Lock()
	defer tp.mu.Unlock()
	c := &tapConn{Conn: nc, tp: tp, idx: len(tp.conns), methods: map[string]string{},
		closed: make(chan struct{}), wdlMoved: make(chan struct{})}
	tp.conns = append(tp.conns, c)
	tp.changedLocked()
	return c, nil
}

// holdDials makes every later dial wait until releaseDials; the channel it
// returns is closed when the first of them has begun.
func (tp *tap) holdDials() <-chan struct{} {
	tp.mu.Lock()
	defer tp.mu.Unlock()
	tp.dialGate = make(chan struct{})
	tp.dialHeld = make(chan struct{})
	return tp.dialHeld
}

func (tp *tap) releaseDials() {
	tp.mu.Lock()
	defer tp.mu.Unlock()
	if tp.dialGate != nil {
		close(tp.dialGate)
		tp.dialGate = nil
	}
}

func (tp *tap) failDials(err error) {
	tp.mu.Lock()
	defer tp.mu.Unlock()
	tp.dialFail = err
}

// redirect sends every later dial to path.
func (tp *tap) redirect(path string) {
	tp.mu.Lock()
	defer tp.mu.Unlock()
	tp.dialTo = path
}

func (tp *tap) dialCount() int {
	tp.mu.Lock()
	defer tp.mu.Unlock()
	return tp.dials
}

// kill closes the newest connection under the client, as a dead link would.
func (tp *tap) kill() {
	tp.mu.Lock()
	c := tp.conns[len(tp.conns)-1]
	tp.mu.Unlock()
	_ = c.Close()
}

// killConn closes connection i under the client.
func (tp *tap) killConn(i int) {
	tp.mu.Lock()
	c := tp.conns[i]
	tp.mu.Unlock()
	_ = c.Close()
}

// setRewriteIn and setHoldOut install the tap's hooks.
func (tp *tap) setRewriteIn(f func(wireLine) [][]byte) {
	tp.mu.Lock()
	defer tp.mu.Unlock()
	tp.rewriteIn = f
}

func (tp *tap) setHoldOut(f func(wireLine) <-chan struct{}) {
	tp.mu.Lock()
	defer tp.mu.Unlock()
	tp.holdOut = f
}

func (tp *tap) setRewriteOut(f func(wireLine) []byte) {
	tp.mu.Lock()
	defer tp.mu.Unlock()
	tp.rewriteOut = f
}

func (tp *tap) setStallOut(f func(wireLine) bool) {
	tp.mu.Lock()
	defer tp.mu.Unlock()
	tp.stallOut = f
}

// connCount is how many connections the tap has opened.
func (tp *tap) connCount() int {
	tp.mu.Lock()
	defer tp.mu.Unlock()
	return len(tp.conns)
}

// sent is every request the client wrote with method, in order.
func (tp *tap) sent(method string) []wireLine {
	tp.mu.Lock()
	defer tp.mu.Unlock()
	var out []wireLine
	for _, l := range tp.lines {
		if l.out && l.method == method {
			out = append(out, l)
		}
	}
	return out
}

// sentOn is every request the client wrote with method on connection conn,
// in order.
func (tp *tap) sentOn(conn int, method string) []wireLine {
	var out []wireLine
	for _, l := range tp.sent(method) {
		if l.conn == conn {
			out = append(out, l)
		}
	}
	return out
}

// received is every line the host wrote that matches, in order.
func (tp *tap) received(match func(wireLine) bool) []wireLine {
	tp.mu.Lock()
	defer tp.mu.Unlock()
	var out []wireLine
	for _, l := range tp.lines {
		if !l.out && match(l) {
			out = append(out, l)
		}
	}
	return out
}

// await waits until cond holds over the tap's lines.
func (tp *tap) await(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.After(watchdog)
	for {
		tp.mu.Lock()
		ch := tp.changed
		tp.mu.Unlock()
		if cond() {
			return
		}
		select {
		case <-ch:
		case <-deadline:
			t.Fatalf("%s: not in %s", what, watchdog)
		}
	}
}

func (tp *tap) fail(format string, args ...any) {
	tp.mu.Lock()
	defer tp.mu.Unlock()
	tp.errs = append(tp.errs, fmt.Sprintf(format, args...))
}

// tapConn is one connection under the tap.
type tapConn struct {
	net.Conn
	tp  *tap
	idx int
	// methods is each request id's method, for a reply's schema.
	mu      sync.Mutex
	methods map[string]string
	// The read side's own: bytes read and not yet split into lines, lines
	// ready for the client, and the error that ended the read.
	rbuf []byte
	out  []byte
	rerr error
	// closed is closed by Close; wdl is the write deadline last set (zero:
	// none), and wdlMoved is closed and replaced whenever one is set.
	closed    chan struct{}
	closeOnce sync.Once
	dmu       sync.Mutex
	wdl       time.Time
	wdlMoved  chan struct{}
}

func (c *tapConn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

func (c *tapConn) SetDeadline(t time.Time) error {
	c.setWriteDeadline(t)
	return c.Conn.SetDeadline(t)
}

func (c *tapConn) SetWriteDeadline(t time.Time) error {
	c.setWriteDeadline(t)
	return c.Conn.SetWriteDeadline(t)
}

func (c *tapConn) setWriteDeadline(t time.Time) {
	c.dmu.Lock()
	defer c.dmu.Unlock()
	c.wdl = t
	close(c.wdlMoved)
	c.wdlMoved = make(chan struct{})
}

// stall is a write to a full socket: it blocks until the connection is
// closed or its write deadline — whatever it is set to meanwhile — passes,
// and then fails as a socket's write does, having written nothing.
func (c *tapConn) stall() error {
	for {
		c.dmu.Lock()
		dl, moved := c.wdl, c.wdlMoved
		c.dmu.Unlock()
		var expired <-chan time.Time
		var timer *time.Timer
		if !dl.IsZero() {
			timer = time.NewTimer(time.Until(dl))
			expired = timer.C
		}
		select {
		case <-c.closed:
			return net.ErrClosed
		case <-expired:
			return os.ErrDeadlineExceeded
		case <-moved:
			if timer != nil {
				timer.Stop()
			}
		}
	}
}

func (c *tapConn) Write(b []byte) (int, error) {
	l := wireLine{conn: c.idx, out: true, raw: append([]byte(nil), b...)}
	var req protocol.Request
	if err := json.Unmarshal(b, &req); err != nil {
		c.tp.fail("conn %d: the client wrote a line that is not a request: %s", c.idx, b)
	} else {
		l.id, l.method, l.params = string(req.ID), req.Method, req.Params
		c.mu.Lock()
		c.methods[l.id] = req.Method
		c.mu.Unlock()
	}
	if err := wiretest.Default().Request(b); err != nil {
		c.tp.fail("conn %d: a request off the schema: %v", c.idx, err)
	}
	c.tp.mu.Lock()
	c.tp.lines = append(c.tp.lines, l)
	c.tp.changedLocked()
	hold, rewrite, stall := c.tp.holdOut, c.tp.rewriteOut, c.tp.stallOut
	c.tp.mu.Unlock()
	if stall != nil && stall(l) {
		return 0, c.stall()
	}
	if hold != nil {
		if ch := hold(l); ch != nil {
			<-ch
		}
	}
	w := b
	if rewrite != nil {
		if nb := rewrite(l); nb != nil {
			w = nb
		}
	}
	if n, err := c.Conn.Write(w); err != nil {
		return min(n, len(b)), err
	}
	return len(b), nil
}

func (c *tapConn) Read(p []byte) (int, error) {
	for len(c.out) == 0 {
		if c.rerr != nil {
			return 0, c.rerr
		}
		buf := make([]byte, 64<<10)
		n, err := c.Conn.Read(buf)
		c.rbuf = append(c.rbuf, buf[:n]...)
		for {
			i := bytes.IndexByte(c.rbuf, '\n')
			if i < 0 {
				break
			}
			line := append([]byte(nil), c.rbuf[:i]...)
			c.rbuf = c.rbuf[i+1:]
			c.out = append(c.out, c.inbound(line)...)
		}
		if err != nil {
			c.rerr = err
		}
	}
	n := copy(p, c.out)
	c.out = c.out[n:]
	return n, nil
}

// inbound records and checks one line the host wrote, and is what the client
// reads in its place.
func (c *tapConn) inbound(raw []byte) []byte {
	l := wireLine{conn: c.idx, raw: raw}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		c.tp.fail("conn %d: the host wrote a line that is not JSON: %s", c.idx, raw)
	}
	if _, isNote := probe["method"]; isNote {
		var n protocol.Notification
		_ = json.Unmarshal(raw, &n)
		l.method, l.params = n.Method, n.Params
	} else {
		var r protocol.Response
		_ = json.Unmarshal(raw, &r)
		l.id, l.resp = string(r.ID), &r
		c.mu.Lock()
		l.method = c.methods[l.id]
		c.mu.Unlock()
	}
	if err := wiretest.Default().Server(l.method, raw); err != nil {
		c.tp.fail("conn %d: a %q line off the schema: %v", c.idx, l.method, err)
	}
	c.tp.mu.Lock()
	c.tp.lines = append(c.tp.lines, l)
	c.tp.changedLocked()
	rewrite := c.tp.rewriteIn
	c.tp.mu.Unlock()
	if rewrite != nil {
		if lines := rewrite(l); lines != nil {
			var out []byte
			for _, nl := range lines {
				out = append(append(out, nl...), '\n')
			}
			return out
		}
	}
	return append(raw, '\n')
}

// --------------------------------------------------------------- the hub

// hub is a test-only proxy implementing exactly a hub's opening — hello
// (answered as a hub, X5) and session.connect — and then the splice (plan 027
// §3.3): it dials the host's socket, answers {}, and from the next byte on
// copies both ways. Anything else is refused unsupported.
type hub struct {
	t    *testing.T
	path string
	host string
	mu   sync.Mutex
	// accepted is every connection accepted, and pairs every spliced pair,
	// client side and host side.
	accepted []net.Conn
	pairs    [][2]net.Conn
	connect  []string // every session.connect's sessionId
}

func newHub(t *testing.T, hostPath string) *hub {
	t.Helper()
	hb := &hub{t: t, path: filepath.Join(shortDir(t), "h"), host: hostPath}
	l, err := net.Listen("unix", hb.path)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	t.Cleanup(func() {
		_ = l.Close()
		hb.kill()
		hb.mu.Lock()
		for _, nc := range hb.accepted {
			_ = nc.Close()
		}
		hb.mu.Unlock()
		wg.Wait()
	})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			nc, err := l.Accept()
			if err != nil {
				return
			}
			hb.mu.Lock()
			hb.accepted = append(hb.accepted, nc)
			hb.mu.Unlock()
			wg.Add(1)
			go func() {
				defer wg.Done()
				hb.serve(nc, &wg)
			}()
		}
	}()
	return hb
}

func (hb *hub) serve(nc net.Conn, wg *sync.WaitGroup) {
	br := bufio.NewReader(nc)
	reply := func(id json.RawMessage, result any, perr *protocol.Error) {
		resp := protocol.Response{JSONRPC: protocol.JSONRPCVersion, ID: id, Error: perr}
		if perr == nil {
			raw, _ := json.Marshal(result)
			resp.Result = raw
		}
		_ = protocol.WriteLine(nc, resp)
	}
	for {
		b, err := br.ReadBytes('\n')
		if err != nil {
			_ = nc.Close()
			return
		}
		var req protocol.Request
		if json.Unmarshal(b, &req) != nil {
			continue
		}
		switch req.Method {
		case protocol.MethodHello:
			reply(req.ID, protocol.HubHelloResult{
				Protocol: protocol.ProtocolVersion,
				Endpoint: protocol.Endpoint{Kind: protocol.EndpointHub, HostID: "0123456789ab", CrazeVersion: "test", PID: os.Getpid()},
				Capabilities: protocol.ConnectionCapabilities{Connect: true, Multiplex: false, RosterSubscribe: false,
					SessionCreate: false, Snapshot: true, AttachWhenNow: true},
				Codecs: protocol.Codecs{Event: agent.EventCodecVersion, Snapshot: transcript.SnapshotVersion},
				Limits: protocol.HostLimits(),
			}, nil)
		case protocol.MethodSessionConnect:
			var p protocol.ConnectParams
			_ = json.Unmarshal(req.Params, &p)
			hc, err := net.Dial("unix", hb.host)
			if err != nil {
				reply(req.ID, nil, &protocol.Error{Code: protocol.RPCRefused, Message: err.Error(),
					Data: protocol.ErrorData{Code: protocol.CodeUnavailable, Reason: protocol.ReasonNotReady}})
				continue
			}
			hb.mu.Lock()
			hb.pairs = append(hb.pairs, [2]net.Conn{nc, hc})
			hb.connect = append(hb.connect, p.SessionID)
			hb.mu.Unlock()
			reply(req.ID, protocol.Empty{}, nil)
			// The splice: from the next byte on, the client speaks to the
			// host itself (what the reader holds of it included).
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, _ = io.Copy(nc, hc)
				_ = nc.Close()
			}()
			_, _ = io.Copy(hc, br)
			_ = hc.Close()
			return
		default:
			reply(req.ID, nil, &protocol.Error{Code: protocol.RPCRefused, Message: "not served in hub mode",
				Data: protocol.ErrorData{Code: protocol.CodeUnsupported, Reason: protocol.ReasonUnsupported}})
		}
	}
}

// kill drops every spliced pair, both sides.
func (hb *hub) kill() {
	hb.mu.Lock()
	defer hb.mu.Unlock()
	for _, p := range hb.pairs {
		_ = p[0].Close()
		_ = p[1].Close()
	}
}

// ---------------------------------------------------------------- client

// dialClient dials h (or path, when not "") through tp, and closes the
// client when the test ends.
func dialClient(t *testing.T, path string, tp *tap, o remote.Options) *remote.Client {
	t.Helper()
	c, err := tryDial(t, path, tp, o)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return c
}

func tryDial(t *testing.T, path string, tp *tap, o remote.Options) (*remote.Client, error) {
	t.Helper()
	return tryDialHooked(t, path, tp, o, remote.TestHooks{})
}

// dialHooked is dialClient with the client's schedule hooks h in place.
func dialHooked(t *testing.T, path string, tp *tap, o remote.Options, h remote.TestHooks) *remote.Client {
	t.Helper()
	c, err := tryDialHooked(t, path, tp, o, h)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return c
}

func tryDialHooked(t *testing.T, path string, tp *tap, o remote.Options, h remote.TestHooks) (*remote.Client, error) {
	t.Helper()
	o.Dial = tp.dial
	if o.Client.Kind == "" {
		o.Client = protocol.ClientInfo{Kind: "test", Name: "remote_test"}
	}
	if o.RedialBackoff == 0 {
		o.RedialBackoff = 5 * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(context.Background(), watchdog)
	defer cancel()
	c, err := remote.DialForTest(ctx, path, o, h)
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, nil
}

func tctx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), watchdog)
	t.Cleanup(cancel)
	return ctx
}

// attach attaches c and returns the stream.
func attach(t *testing.T, c *remote.Client, o remote.AttachOptions) *remote.Stream {
	t.Helper()
	s, err := c.Attach(tctx(t), o)
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	return s
}

// next is the stream's next item.
func next(t *testing.T, s *remote.Stream) remote.Item {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), watchdog)
	defer cancel()
	it, err := s.Next(ctx)
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	return it
}

// nextKind is the stream's next item, which must be of kind k.
func nextKind(t *testing.T, s *remote.Stream, k remote.Kind) remote.Item {
	t.Helper()
	it := next(t, s)
	if it.Kind != k {
		t.Fatalf("want a %s item; got %s", k, describe(it))
	}
	return it
}

// until reads items up to and including the first for which stop is true.
func until(t *testing.T, s *remote.Stream, stop func(remote.Item) bool) []remote.Item {
	t.Helper()
	var items []remote.Item
	for {
		it := next(t, s)
		items = append(items, it)
		if stop(it) {
			return items
		}
		if it.Kind == remote.KindEnd || it.Kind == remote.KindError {
			t.Fatalf("the stream stopped before the item waited for: %s", describe(it))
		}
	}
}

// isText says it is an event whose text is s.
func isText(t *testing.T, s string) func(remote.Item) bool {
	return func(it remote.Item) bool {
		if it.Kind != remote.KindEvent {
			return false
		}
		ev := decode(t, it)
		return ev.Type == agent.EventText && ev.Text == s
	}
}

func ofKind(k remote.Kind) func(remote.Item) bool {
	return func(it remote.Item) bool { return it.Kind == k }
}

// decode is an event item's event.
func decode(t *testing.T, it remote.Item) agent.Event {
	t.Helper()
	ev, err := agent.DecodeEvent(string(it.Body))
	if err != nil {
		t.Fatalf("event %d: %v", it.Seq, err)
	}
	return ev
}

func describe(it remote.Item) string {
	switch it.Kind {
	case remote.KindEvent:
		return fmt.Sprintf("event %d %s", it.Seq, it.Body)
	case remote.KindSynchronized:
		return fmt.Sprintf("synchronized %d", it.Seq)
	case remote.KindAttached, remote.KindRestore:
		return fmt.Sprintf("%s %s after %+v snapshot %v reset %q ready %v", it.Kind, it.Reply.Subscription,
			it.Reply.After, it.Reply.Snapshot != nil, it.Reply.Reset, it.Reply.Ready)
	case remote.KindReady:
		return fmt.Sprintf("ready %+v", *it.Ready)
	case remote.KindError:
		return fmt.Sprintf("error %v", it.Err)
	}
	return it.Kind.String()
}

// contiguous fails unless items' events run from first on, one seq after
// another, with no gap and no duplicate — across any re-attach — and returns
// the last seq.
func contiguous(t *testing.T, first uint64, items []remote.Item) uint64 {
	t.Helper()
	want := first
	for _, it := range items {
		switch it.Kind {
		case remote.KindEvent:
			if it.Seq != want {
				t.Fatalf("event %d, want %d: a gap or a duplicate", it.Seq, want)
			}
			want++
		case remote.KindRestore:
			want = it.Reply.After.Seq + 1
		}
	}
	return want - 1
}

// kinds counts items of each kind.
func kinds(items []remote.Item) map[remote.Kind]int {
	n := map[remote.Kind]int{}
	for _, it := range items {
		n[it.Kind]++
	}
	return n
}

// command runs one command and fails on its error.
func command(t *testing.T, c *remote.Client, method string, params, result any) string {
	t.Helper()
	id, err := c.Command(tctx(t), method, params, result, remote.CommandOptions{})
	if err != nil {
		t.Fatalf("%s: %v", method, err)
	}
	return id
}

// attachParams decodes an attach request's params.
func attachParams(t *testing.T, l wireLine) protocol.AttachParams {
	t.Helper()
	var p protocol.AttachParams
	if err := json.Unmarshal(l.params, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

// commandID is a command request's commandId.
func commandID(t *testing.T, l wireLine) string {
	t.Helper()
	var p struct {
		CommandID string `json:"commandId"`
	}
	if err := json.Unmarshal(l.params, &p); err != nil {
		t.Fatal(err)
	}
	return p.CommandID
}

// errorCode is a reply line's error code, "" for a result.
func errorCode(l wireLine) protocol.Code {
	if l.resp == nil || l.resp.Error == nil {
		return ""
	}
	return l.resp.Error.Data.Code
}

// await waits for ch to close or deliver.
func await(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(watchdog):
		t.Fatalf("%s: not in %s", what, watchdog)
	}
}

// waitFor polls cond until it holds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(watchdog)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s: not in %s", what, watchdog)
		}
		time.Sleep(time.Millisecond)
	}
}

// outcomeUnknown fails unless err is ErrOutcomeUnknown for reason.
func outcomeUnknown(t *testing.T, err error, reason protocol.Reason) {
	t.Helper()
	var oe *remote.OutcomeUnknownError
	if !errors.Is(err, remote.ErrOutcomeUnknown) || !errors.As(err, &oe) || oe.Reason != reason {
		t.Fatalf("want ErrOutcomeUnknown (%s); got %v", reason, err)
	}
	if !oe.Reason.ClientSide() {
		t.Fatalf("the reason %s is not a client-side one", oe.Reason)
	}
}

// refusedLine is the host's reply l rewritten into a refusal of code and
// reason, with l's id: what the client reads in its place.
func refusedLine(t *testing.T, l wireLine, code protocol.Code, reason protocol.Reason) []byte {
	t.Helper()
	b, err := json.Marshal(protocol.Response{JSONRPC: protocol.JSONRPCVersion, ID: json.RawMessage(l.id),
		Error: &protocol.Error{Code: protocol.RPCRefused, Message: "refused by the tap",
			Data: protocol.ErrorData{Code: code, Reason: reason}}})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// blackHole is a Unix socket that accepts every connection and reads it, and
// never writes a byte: a host that never answers hello.
func blackHole(t *testing.T) string {
	t.Helper()
	path := filepath.Join(shortDir(t), "b")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	var conns []net.Conn
	t.Cleanup(func() {
		_ = l.Close()
		mu.Lock()
		for _, nc := range conns {
			_ = nc.Close()
		}
		mu.Unlock()
		wg.Wait()
	})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			nc, err := l.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, nc)
			mu.Unlock()
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, _ = io.Copy(io.Discard, nc)
			}()
		}
	}()
	return path
}
