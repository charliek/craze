package control_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/control"
	"github.com/charliek/craze/internal/control/wiretest"
	"github.com/charliek/craze/internal/journal"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/tui"
)

// The attach tests' harness: host options for the sessions and bounds they
// need, a client that reads notifications as well as replies (every line
// schema-checked), and the gates that hold a forwarder unscheduled.

func withSession(wrap func(*tui.Stub) agent.Session) hostOpt {
	return func(c *hostConfig) { c.session = wrap }
}

func withMaxBudget(b control.Budget) hostOpt {
	return func(c *hostConfig) { c.opts.MaxBudget = b }
}

func withReadyWait(d time.Duration) hostOpt {
	return func(c *hostConfig) { c.readyWait = d }
}

// logSession is the host's Stub over an event log the test built — a small
// ring, a small record limit, a journal — so the log's own edges can be
// reached over the socket. The engine publishes into it and observes it, and
// the test publishes into it (host.publish); the Stub's own turns publish
// into the Stub's log, which nothing observes, so a test on one runs no turn.
type logSession struct {
	*tui.Stub
	log  *agent.EventLog
	asks *agent.AskRegistry
}

// withLog serves a logSession over a log built with lo (always NoPrimary).
func withLog(lo agent.EventLogOptions) hostOpt {
	return withSession(func(s *tui.Stub) agent.Session {
		lo.NoPrimary = true
		l := agent.NewEventLog(lo)
		return &logSession{Stub: s, log: l, asks: agent.NewAskRegistry(l, time.Now)}
	})
}

func (s *logSession) EventLog() *agent.EventLog  { return s.log }
func (s *logSession) Asks() *agent.AskRegistry   { return s.asks }
func (s *logSession) Events() <-chan agent.Event { return s.log.Primary() }
func (s *logSession) Incarnation() string        { return s.log.Incarnation() }
func (s *logSession) Subscribe(o agent.SubscribeOptions) (*agent.Subscription, error) {
	return s.log.Subscribe(o)
}

// Close closes the asks, the Stub and the log, in the live session's order:
// the asks' endings before the log's close.
func (s *logSession) Close() error {
	s.asks.Close()
	err := s.Stub.Close()
	s.log.Close(context.Background())
	return err
}

// lateIDSession is the host's Stub with no provider session id until its
// start has run, as a live session has none before session/new answers.
type lateIDSession struct {
	*tui.Stub
	started atomic.Bool
}

func withLateID() hostOpt {
	return withSession(func(s *tui.Stub) agent.Session { return &lateIDSession{Stub: s} })
}

func (s *lateIDSession) Start(ctx context.Context) error {
	err := s.Stub.Start(ctx)
	s.started.Store(true)
	return err
}

func (s *lateIDSession) Snapshot() agent.Snapshot {
	snap := s.Stub.Snapshot()
	if !s.started.Load() {
		snap.SessionID = ""
	}
	return snap
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

// dropped is how many subscriptions the host's log has dropped slow.
func (h *host) dropped() int {
	if ls, ok := h.sess.(*logSession); ok {
		return ls.log.Health().SubscribersDropped
	}
	return h.stub.EventLog().Health().SubscribersDropped
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

// ------------------------------------------------------------ the client

// msg is one line the host wrote: a reply, or a notification.
type msg struct {
	resp *protocol.Response
	note *protocol.Notification
}

// next is the host's next line, schema-checked.
func (c *client) next() msg {
	c.t.Helper()
	m, err := c.tryNext()
	if err != nil {
		c.t.Fatalf("read: %v", err)
	}
	return m
}

func (c *client) tryNext() (msg, error) {
	c.t.Helper()
	raw, err := c.readLine()
	if err != nil {
		return msg{}, err
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		c.t.Fatalf("a line that is not JSON: %s", raw)
	}
	if _, ok := probe["method"]; ok {
		if err := wiretest.Default().Notification(raw); err != nil {
			c.t.Fatalf("a notification off the schema: %v", err)
		}
		var n protocol.Notification
		if err := json.Unmarshal(raw, &n); err != nil {
			c.t.Fatal(err)
		}
		return msg{note: &n}, nil
	}
	var r protocol.Response
	if err := json.Unmarshal(raw, &r); err != nil {
		c.t.Fatal(err)
	}
	c.mu.Lock()
	method := c.methods[string(r.ID)]
	c.mu.Unlock()
	if err := wiretest.Default().Response(method, raw); err != nil {
		c.t.Fatalf("a %q reply off the schema: %v", method, err)
	}
	return msg{resp: &r}, nil
}

// until reads up to and including the reply to id: the notifications before
// it, in order, and the reply.
func (c *client) until(id string) ([]*protocol.Notification, *protocol.Response) {
	c.t.Helper()
	var notes []*protocol.Notification
	for {
		m := c.next()
		if m.note != nil {
			notes = append(notes, m.note)
			continue
		}
		if string(m.resp.ID) != id {
			c.t.Fatalf("a reply to %s while waiting for the one to %s", m.resp.ID, id)
		}
		return notes, m.resp
	}
}

// note reads the next line, which must be a notification of method.
func (c *client) note(method string) *protocol.Notification {
	c.t.Helper()
	m := c.next()
	if m.note == nil || m.note.Method != method {
		c.t.Fatalf("want a %s notification; got %s", method, describe(m))
	}
	return m.note
}

// anyNote reads the next line, which must be a notification.
func (c *client) anyNote() *protocol.Notification {
	c.t.Helper()
	m := c.next()
	if m.note == nil {
		c.t.Fatalf("want a notification; got %s", describe(m))
	}
	return m.note
}

// reply reads the next line, which must be the reply to id.
func (c *client) reply(id string) *protocol.Response {
	c.t.Helper()
	m := c.next()
	if m.resp == nil || string(m.resp.ID) != id {
		c.t.Fatalf("want the reply to %s; got %s", id, describe(m))
	}
	return m.resp
}

func describe(m msg) string {
	if m.note != nil {
		return "the notification " + m.note.Method + " " + string(m.note.Params)
	}
	if m.resp.Error != nil {
		return "the reply to " + string(m.resp.ID) + ": " + m.resp.Error.Error()
	}
	return "the reply to " + string(m.resp.ID) + ": " + string(m.resp.Result)
}

// attach attaches with p and reads its reply, which must come before any
// notification.
func (c *client) attach(p protocol.AttachParams) protocol.AttachResult {
	c.t.Helper()
	id := c.send(protocol.MethodSessionAttach, p)
	return ok[protocol.AttachResult](c.t, c.reply(id))
}

// detach detaches sub; nothing of it may come after the reply.
func (c *client) detach(h *host, sub string) *protocol.Response {
	c.t.Helper()
	return c.call(protocol.MethodSessionDetach, protocol.DetachParams{SessionID: sid(h), Subscription: sub})
}

// sync is session.sync through the stream: the notifications before its
// reply, and its seq.
func (c *client) sync(h *host) ([]*protocol.Notification, uint64) {
	c.t.Helper()
	id := c.send(protocol.MethodSessionSync, protocol.SyncParams{SessionID: sid(h)})
	notes, resp := c.until(id)
	return notes, ok[protocol.SyncResult](c.t, resp).Seq
}

// expectClosed reads to the end of the connection, failing on anything
// before it.
func (c *client) expectClosed() {
	c.t.Helper()
	m, err := c.tryNext()
	if err == nil {
		c.t.Fatalf("want the host to close the connection; got %s", describe(m))
	}
	if !errors.Is(err, io.EOF) && !isReset(err) {
		c.t.Fatalf("want the host to close the connection; read: %v", err)
	}
}

// paramsOf is n's params.
func paramsOf[T any](t *testing.T, n *protocol.Notification) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(n.Params, &v); err != nil {
		t.Fatalf("%s params %s: %v", n.Method, n.Params, err)
	}
	return v
}

// eventOf is an event notification's seq and event, decoded.
func eventOf(t *testing.T, n *protocol.Notification) (uint64, agent.Event) {
	t.Helper()
	if n.Method != protocol.NotifyEvent {
		t.Fatalf("want an event; got %s %s", n.Method, n.Params)
	}
	p := paramsOf[protocol.EventParams](t, n)
	ev, err := agent.DecodeEvent(string(p.Event))
	if err != nil {
		t.Fatalf("event %d: %v", p.Seq, err)
	}
	return p.Seq, ev
}

// lastEvent is the highest seq among notes' events, or floor.
func lastEvent(t *testing.T, notes []*protocol.Notification, floor uint64) uint64 {
	t.Helper()
	for _, n := range notes {
		if n.Method == protocol.NotifyEvent {
			floor = max(floor, paramsOf[protocol.EventParams](t, n).Seq)
		}
	}
	return floor
}

func attachParams(h *host) protocol.AttachParams { return protocol.AttachParams{SessionID: sid(h)} }

// ------------------------------------------------------------ the gates

// forwardGate holds a forwarder unscheduled (control.TestHooks.BeforeForward):
// once armed, the first event it is about to queue with a seq above the armed
// floor is held, reported on held, until open. The host opens it before its
// server closes (withOnClose), so a failing test never leaves a forwarder the
// server's Close would join for ever.
type forwardGate struct {
	mu      sync.Mutex
	armed   bool
	above   uint64
	release chan struct{}
	opened  bool
	held    chan uint64
}

func newForwardGate() *forwardGate {
	return &forwardGate{held: make(chan uint64, 1), release: make(chan struct{})}
}

// withOnClose runs f before the host's server closes: what a test holds a
// server goroutine with is let go first.
func withOnClose(f func()) hostOpt {
	return func(c *hostConfig) { c.onClose = append(c.onClose, f) }
}

func (g *forwardGate) hook(_ string, seq uint64) {
	g.mu.Lock()
	if !g.armed || seq <= g.above {
		g.mu.Unlock()
		return
	}
	g.armed = false
	release := g.release
	g.mu.Unlock()
	g.held <- seq
	<-release
}

// arm holds the next event above seq.
func (g *forwardGate) arm(seq uint64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.armed, g.above, g.opened = true, seq, false
	g.release = make(chan struct{})
}

// open lets a held forwarder go, and disarms the gate.
func (g *forwardGate) open() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.armed = false
	if !g.opened {
		g.opened = true
		close(g.release)
	}
}

// awaitHeld waits for the forwarder to be held, and is the seq it holds.
func (g *forwardGate) awaitHeld(t *testing.T) uint64 {
	t.Helper()
	select {
	case seq := <-g.held:
		return seq
	case <-time.After(watchdog):
		t.Fatalf("no forwarder held in %s", watchdog)
		return 0
	}
}

// signal is a hook's report, buffered so the hook never waits on the test.
type signal chan uint64

func newSignal() signal { return make(signal, 64) }

func (s signal) fire(seq uint64) {
	select {
	case s <- seq:
	default:
	}
}

func (s signal) await(t *testing.T, what string) uint64 {
	t.Helper()
	select {
	case seq := <-s:
		return seq
	case <-time.After(watchdog):
		t.Fatalf("%s: not in %s", what, watchdog)
		return 0
	}
}
