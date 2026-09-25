package control_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charliek/craze/internal/control"
	"github.com/charliek/craze/internal/control/wiretest"
	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/sessions"
	"github.com/charliek/craze/internal/tui"
)

// watchdog bounds every wait a test makes on the server.
const watchdog = 10 * time.Second

// The tests run the real server over a real Unix socket (net.Listen("unix"))
// in a SHORT temporary directory — t.TempDir() overflows sun_path on macOS —
// in front of a real engine over tui's Stub with no primary, as
// internal/engine's own tests drive it, and talk to it with a raw NDJSON
// client built on internal/protocol's framing (not internal/remote). Every
// line the client reads is held to the schema (wiretest).

// clock is the receipts table's clock (engine.Options.ReceiptClock): the wall
// clock plus an offset a test moves past the table's age bound.
type clock struct{ offset atomic.Int64 }

func (c *clock) now() time.Time { return time.Now().Add(time.Duration(c.offset.Load())) }

func (c *clock) advance(d time.Duration) { c.offset.Add(int64(d)) }

// logSink collects Options.Log's lines and lets a test wait for one.
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
			if containsAll(line, subs) {
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

// waitCount waits until n lines contain every one of subs.
func (l *logSink) waitCount(t *testing.T, n int, subs ...string) {
	t.Helper()
	deadline := time.After(watchdog)
	for {
		l.mu.Lock()
		got := 0
		for _, line := range l.lines {
			if containsAll(line, subs) {
				got++
			}
		}
		grew := l.grew
		l.mu.Unlock()
		if got >= n {
			return
		}
		select {
		case <-grew:
		case <-deadline:
			l.mu.Lock()
			defer l.mu.Unlock()
			t.Fatalf("%d log lines with %q in %s, want %d; the log:\n%s", got, subs, watchdog, n, strings.Join(l.lines, "\n"))
		}
	}
}

func containsAll(s string, subs []string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}

// hostConfig is what newHost builds from.
type hostConfig struct {
	opts    control.Options
	hooks   control.TestHooks
	stall   time.Duration
	maxLine int
	engine  engine.Options
	noSet   bool
	noStart bool
}

type hostOpt func(*hostConfig)

func withHooks(h control.TestHooks) hostOpt { return func(c *hostConfig) { c.hooks = h } }
func withStall(d time.Duration) hostOpt     { return func(c *hostConfig) { c.stall = d } }
func withMaxLine(n int) hostOpt             { return func(c *hostConfig) { c.maxLine = n } }
func withIndex(o engine.IndexOptions) hostOpt {
	return func(c *hostConfig) { c.engine.Index = o }
}

// withoutEngine serves nothing: SetEngine is never called.
func withoutEngine() hostOpt { return func(c *hostConfig) { c.noSet = true } }

// withoutStart serves an engine whose start has not run.
func withoutStart() hostOpt { return func(c *hostConfig) { c.noStart = true } }

// host is a server on a socket in front of an engine over a Stub.
type host struct {
	t     *testing.T
	stub  *tui.Stub
	eng   *engine.Engine
	srv   *control.Server
	path  string
	clock *clock
	logs  *logSink
}

func newHost(t *testing.T, opts ...hostOpt) *host {
	t.Helper()
	cfg := hostConfig{}
	for _, o := range opts {
		o(&cfg)
	}
	dir, err := os.MkdirTemp("", "czc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	h := &host{t: t, clock: &clock{}, logs: newLogSink(), path: filepath.Join(dir, "s")}
	h.stub = tui.NewStubNoPrimary()
	cfg.engine.ReceiptClock = h.clock.now
	h.eng, err = engine.New(h.stub, cfg.engine)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.eng.Close() })
	if !cfg.noStart {
		if err := h.eng.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	cfg.opts.Log = h.logs.log
	cfg.opts.Workspace = "/work"
	h.srv = control.NewForTest(cfg.opts, cfg.hooks, cfg.stall, cfg.maxLine)
	if !cfg.noSet {
		h.srv.SetEngine(h.eng)
	}
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

// sessionID is the durable craze session id the host serves.
func (h *host) sessionID() string { return h.eng.State().CrazeSessionID }

// client is a raw NDJSON client: one connection, its requests' methods by id
// (a response carries no method), and every line it reads schema-checked.
type client struct {
	t       *testing.T
	h       *host
	nc      *net.UnixConn
	lr      *protocol.LineReader
	mu      sync.Mutex
	nextID  int
	methods map[string]string
	nextCmd int
	hello   protocol.HelloResult
}

func (h *host) dial() *client {
	h.t.Helper()
	nc, err := net.Dial("unix", h.path)
	if err != nil {
		h.t.Fatal(err)
	}
	c := &client{t: h.t, h: h, nc: nc.(*net.UnixConn), methods: map[string]string{}}
	c.lr = protocol.NewLineReader(c.nc, protocol.OutboundLineMax)
	h.t.Cleanup(func() { _ = c.nc.Close() })
	return c
}

// send writes one request and returns its id.
func (c *client) send(method string, params any) string {
	c.t.Helper()
	id, err := c.trySend(method, params)
	if err != nil {
		c.t.Fatalf("write: %v", err)
	}
	return id
}

// trySend is send, with a failed write — the host has closed the
// connection — returned.
func (c *client) trySend(method string, params any) (string, error) {
	c.t.Helper()
	c.mu.Lock()
	c.nextID++
	id := strconv.Itoa(c.nextID)
	c.methods[id] = method
	c.mu.Unlock()
	raw, err := json.Marshal(params)
	if err != nil {
		c.t.Fatal(err)
	}
	line, err := protocol.MarshalLine(protocol.Request{JSONRPC: protocol.JSONRPCVersion, ID: json.RawMessage(id), Method: method, Params: raw})
	if err != nil {
		c.t.Fatal(err)
	}
	_ = c.nc.SetWriteDeadline(time.Now().Add(watchdog))
	_, err = c.nc.Write(line)
	return id, err
}

// sendRaw writes a line as it is, a newline appended; method is what its
// response is checked against ("" for the envelope alone), under id.
func (c *client) sendRaw(id, method, line string) {
	c.t.Helper()
	if id != "" {
		c.mu.Lock()
		c.methods[id] = method
		c.mu.Unlock()
	}
	c.write([]byte(line + "\n"))
}

func (c *client) write(b []byte) {
	c.t.Helper()
	_ = c.nc.SetWriteDeadline(time.Now().Add(watchdog))
	if _, err := c.nc.Write(b); err != nil {
		c.t.Fatalf("write: %v", err)
	}
}

// readLine is the next line from the host, or io.EOF.
func (c *client) readLine() ([]byte, error) {
	_ = c.nc.SetReadDeadline(time.Now().Add(watchdog))
	return c.lr.ReadLine()
}

// read is the next response, schema-checked against its request's method.
func (c *client) read() *protocol.Response {
	c.t.Helper()
	resp, err := c.tryRead()
	if err != nil {
		c.t.Fatalf("read: %v", err)
	}
	return resp
}

// tryRead is read, with EOF (or any read error) returned.
func (c *client) tryRead() (*protocol.Response, error) {
	c.t.Helper()
	line, err := c.readLine()
	if err != nil {
		return nil, err
	}
	var resp protocol.Response
	if err := json.Unmarshal(line, &resp); err != nil {
		c.t.Fatalf("a line that is not a response: %s", line)
	}
	c.mu.Lock()
	method := c.methods[string(resp.ID)]
	c.mu.Unlock()
	if err := wiretest.Default().Server(method, line); err != nil {
		c.t.Fatalf("a %q reply off the schema: %v", method, err)
	}
	return &resp, nil
}

// expectEOF reads until the host closes the connection, failing on anything
// but responses before it.
func (c *client) expectEOF() {
	c.t.Helper()
	for {
		if _, err := c.tryRead(); err != nil {
			if !errors.Is(err, io.EOF) && !isReset(err) {
				c.t.Fatalf("want the host to close the connection; read: %v", err)
			}
			return
		}
	}
}

func isReset(err error) bool {
	return strings.Contains(err.Error(), "connection reset")
}

// call sends one request and reads its reply, which must be the next line.
func (c *client) call(method string, params any) *protocol.Response {
	c.t.Helper()
	id := c.send(method, params)
	resp := c.read()
	if string(resp.ID) != id {
		c.t.Fatalf("%s: the reply is to %s, want %s", method, resp.ID, id)
	}
	return resp
}

// ok is resp's result decoded into v, failing on an error reply.
func ok[T any](t *testing.T, resp *protocol.Response) T {
	t.Helper()
	var v T
	if resp.Error != nil {
		t.Fatalf("want a result; got the error %v", resp.Error)
	}
	if err := json.Unmarshal(resp.Result, &v); err != nil {
		t.Fatalf("decode %s: %v", resp.Result, err)
	}
	return v
}

// refusedWith fails unless resp is the error rpc, code and reason.
func refusedWith(t *testing.T, resp *protocol.Response, rpc int, code protocol.Code, reason protocol.Reason) *protocol.Error {
	t.Helper()
	e := resp.Error
	if e == nil {
		t.Fatalf("want %d %s/%s; got the result %s", rpc, code, reason, resp.Result)
	}
	if e.Code != rpc || e.Data.Code != code || e.Data.Reason != reason {
		t.Fatalf("want %d %s/%s; got %d %s/%s: %s", rpc, code, reason, e.Code, e.Data.Code, e.Data.Reason, e.Message)
	}
	return e
}

// sayHello says hello, resuming r when it is not nil, and returns the result.
func (c *client) sayHello(r *protocol.Resume) protocol.HelloResult {
	c.t.Helper()
	res := ok[protocol.HelloResult](c.t, c.call(protocol.MethodHello, helloParams(r)))
	c.hello = res
	return res
}

func helloParams(r *protocol.Resume) protocol.HelloParams {
	return protocol.HelloParams{Protocols: []int{protocol.ProtocolVersion},
		Client: protocol.ClientInfo{Kind: "test", Name: "control_test"}, Resume: r}
}

// resume is this client's hello as a resume for another connection.
func (c *client) resume() *protocol.Resume {
	return &protocol.Resume{ClientID: c.hello.ClientID, Token: c.hello.Token}
}

// cmd is this client's next command id.
func (c *client) cmd() string {
	c.nextCmd++
	return strconv.Itoa(c.nextCmd)
}

// lastCmd is the command id cmd handed out last.
func (c *client) lastCmd() string { return strconv.Itoa(c.nextCmd) }

// close drops the whole connection.
func (c *client) close() { _ = c.nc.Close() }

// waitFor polls cond until it holds, failing at the watchdog.
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

// await waits for ch to close or deliver.
func await(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(watchdog):
		t.Fatalf("%s: not in %s", what, watchdog)
	}
}

func sid(h *host) string { return h.sessionID() }

// gateStore is a session index (engine.Index) whose every Upsert parks until
// the gate opens: a command held in the index as a contended flock would hold
// it, without the file.
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

// open lets every parked Upsert through, and every later one at once.
func (g *gateStore) open() { g.once.Do(func() { close(g.gate) }) }

// waitEntered waits until n Upserts have reached the gate.
func (g *gateStore) waitEntered(t *testing.T, n int) {
	t.Helper()
	waitFor(t, fmt.Sprintf("%d index writes at the gate", n), func() bool { return int(g.entered.Load()) >= n })
}
