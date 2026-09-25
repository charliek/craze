package control_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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

// drainerMaxItems and drainerMaxBytes are the subscription budget every
// non-stalled subscriber attaches with (and the server's own MaxBudget
// ceiling, raised to match) — deliberately far above agent.EventLog's
// production default (1024 items, 8 MiB, defaultSubscribeItems/Bytes): a
// b.Loop() calibrated for the usual ~1s runs log.Publish (a fast, in-process,
// non-blocking buffer offer) fast enough to queue upwards of a million
// records in that time, and the default budget overflows well before a real
// forwarder-plus-socket-plus-client chain — genuinely fast, just not
// enqueue-fast — can drain it, ending the "healthy" subscription as
// slow_consumer for real (V7 finding 2). This budget only needs enough
// slack that draining stays ahead of production on THIS machine's
// enqueue rate long enough to matter, not to survive an unbounded run.
// subs=4+stalled's stalled client attaches with its own separate, small
// budget (MaxItems: 4) specifically to force ITS overflow — this constant
// is for the subscribers that must never be dropped.
const (
	drainerMaxItems = 1 << 20
	drainerMaxBytes = 256 << 20
)

// BenchmarkPublishWithSocketSubscribers is V7 (plan 027 §8, §9 R7): Publish's
// cost with 0, 1 and 4 attached socket subscribers — the log's own fan-out
// (agent.EventLog.Publish, one non-blocking offer per subscription, R7),
// with the server's forwarders on the other end of each subscription
// actually reading and writing to a real Unix socket, as production runs it.
// A real control.Server sits in front of a real engine over
// tui.NewStubNoPrimary() (so nothing needs to drain the primary for Publish
// to proceed); each subscriber is a raw client that says hello, attaches
// (when: "now") and drains its socket on its own goroutine for the whole
// benchmark, and after the timed loop must be proven to have received every
// event through the log's head (benchClient.awaitLive). subs=4+stalled adds one more attachment that reads its attach
// reply and its first synchronized notification, then never reads again: its
// subscription is driven to slow_consumer during setup, outside the timer,
// so the timed loop measures that Publish's cost does not grow with a
// subscriber that never drains (R7's non-blocking offer).
func BenchmarkPublishWithSocketSubscribers(b *testing.B) {
	for _, tc := range []struct {
		name    string
		subs    int
		stalled bool
	}{
		{"subs=0", 0, false},
		{"subs=1", 1, false},
		{"subs=4", 4, false},
		{"subs=4+stalled", 4, true},
	} {
		b.Run(tc.name, func(b *testing.B) {
			runPublishWithSocketSubscribers(b, tc.subs, tc.stalled)
		})
	}
}

// runPublishWithSocketSubscribers is one sub-benchmark's body: setup (dialing,
// hello, attach, synchronized) outside the timer, the timed loop publishing
// one text delta per iteration — the same shape internal/agent's own
// BenchmarkEventLogPublish uses — and clean teardown (every drain goroutine
// joined, Server.Close waited on).
func runPublishWithSocketSubscribers(b *testing.B, subs int, stalled bool) {
	b.Helper()
	dir, err := os.MkdirTemp("", "czb-")
	if err != nil {
		b.Fatalf("MkdirTemp: %v", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	stub := tui.NewStubNoPrimary()
	eng, err := engine.New(stub, engine.Options{})
	if err != nil {
		b.Fatalf("engine.New: %v", err)
	}
	defer func() { _ = eng.Close() }()
	if err := eng.Start(context.Background()); err != nil {
		b.Fatalf("engine.Start: %v", err)
	}

	srv := control.New(control.Options{Workspace: "/bench", MaxBudget: control.Budget{MaxItems: drainerMaxItems, MaxBytes: drainerMaxBytes}})
	srv.SetEngine(eng)
	path := filepath.Join(dir, "s")
	l, err := net.Listen("unix", path)
	if err != nil {
		b.Fatalf("Listen: %v", err)
	}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(l) }()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Close(ctx); err != nil {
			b.Errorf("server close: %v", err)
		}
		if err := <-served; err != nil {
			b.Errorf("serve: %v", err)
		}
	}()

	sessionID := eng.State().CrazeSessionID
	log := stub.EventLog()

	var wg sync.WaitGroup
	var conns []*benchClient
	defer func() {
		for _, c := range conns {
			_ = c.nc.Close()
		}
		wg.Wait()
	}()
	for range subs {
		c := dialBenchClient(b, path)
		benchHello(b, c)
		benchAttach(b, c, sessionID, &protocol.AttachBudget{MaxItems: drainerMaxItems, MaxBytes: drainerMaxBytes})
		synced := make(chan bool, 1)
		wg.Add(1)
		go func(c *benchClient) {
			defer wg.Done()
			drain(c, synced)
		}(c)
		if !<-synced {
			b.Fatal("a drain goroutine exited before its first synchronized notification")
		}
		conns = append(conns, c)
	}

	var sc *benchClient
	if stalled {
		sc = dialBenchClient(b, path)
		defer func() { _ = sc.nc.Close() }()
		benchHello(b, sc)
		benchAttach(b, sc, sessionID, &protocol.AttachBudget{MaxItems: 4})
		sc.readNotification(b, protocol.NotifySynchronized)

		big := agent.Event{Type: agent.EventText, Text: strings.Repeat("x", 1<<20), At: time.Now()}
		for i := 0; log.Health().SubscribersDropped == 0; i++ {
			if i == 128 {
				b.Fatal("the stalled subscription was never dropped")
			}
			if !log.Publish(context.Background(), nil, big) {
				b.Fatal("a setup publish was refused")
			}
		}
	}

	ev := agent.Event{Type: agent.EventText, Text: "Here is the next chunk of the answer, ", At: time.Now()}
	b.ReportAllocs()
	for b.Loop() {
		if !log.Publish(context.Background(), nil, ev) {
			b.Fatal("Publish returned false")
		}
	}

	// The timer has stopped (b.Loop). SubscribersDropped (used above to drive
	// the stall) is a log-wide count: a draining subscriber can overflow and
	// be dropped too, during the rapid 1 MiB setup publishes or during the
	// timed loop itself — the very last publish included — which would
	// satisfy that check without the STALLED client being the one actually
	// dropped, and would silently measure fewer live subscribers than the
	// sub-benchmark names. Verify precisely instead of trusting the count:
	// the stalled client specifically must have been reset slow_consumer, and
	// every draining subscriber must have stayed live to the end — proved,
	// not sampled (r23 finding c): the log's committed head is read, and each
	// draining subscriber is waited on, with a deadline, until it has either
	// received the event at that seq (live: every event the loop published
	// reached it) or a reset (dropped: the benchmark fails). A reset still in
	// flight when the loop ended — its forwarder yet to deliver it — is
	// therefore waited for, never missed by a check made too early.
	ctx, cancel := context.WithTimeout(context.Background(), benchLiveWait)
	head, err := log.FlushSeq(ctx, nil)
	cancel()
	if err != nil {
		b.Fatalf("reading the log's committed head: %v", err)
	}
	if stalled {
		reason := sc.readReset(b, time.Now().Add(benchLiveWait))
		if reason != protocol.ResetSlowConsumer {
			b.Fatalf("stalled client's reset reason = %q, want %q", reason, protocol.ResetSlowConsumer)
		}
	}
	for i, c := range conns {
		if err := c.awaitLive(head, benchLiveWait); err != nil {
			b.Fatalf("draining subscriber %d did not stay live through seq %d: %v", i, head, err)
		}
	}
}

// benchReadTimeout bounds every socket read a benchClient makes
// (deadlineReader), so a wedged read fails the benchmark rather than hanging
// it: a drain idles only between setup and the timed loop and between the
// liveness check and teardown, both far shorter. benchLiveWait bounds each
// post-loop wait — the committed head, the stalled client's reset, each
// draining subscriber's catch-up to the head — generous next to how long a
// drainer takes to work through what it is behind by when the loop ends.
const (
	benchReadTimeout = 30 * time.Second
	benchLiveWait    = 30 * time.Second
)

// deadlineReader is a benchClient's socket as its LineReader reads it: every
// Read gets a deadline — benchReadTimeout from now, or the tighter cap a
// caller sets (readReset's) — so no read can block forever (r23 finding c).
// It is set per underlying Read, not per line: the LineReader's buffer takes
// whatever the socket has, many lines at a time when a drainer is behind, so
// the drain's hot loop pays for it per read, never per event. Only the one
// goroutine that reads the client touches it.
type deadlineReader struct {
	nc  *net.UnixConn
	cap time.Time
}

func (r *deadlineReader) Read(p []byte) (int, error) {
	d := time.Now().Add(benchReadTimeout)
	if !r.cap.IsZero() && r.cap.Before(d) {
		d = r.cap
	}
	if err := r.nc.SetReadDeadline(d); err != nil {
		return 0, err
	}
	return r.nc.Read(p)
}

// benchClient is a minimal raw NDJSON client for the publish benchmark: no
// schema checking (the timed loop never touches the wire; only setup and the
// drain goroutines do) — just enough to hello, attach and read notifications.
// Every read it makes has a deadline (deadlineReader).
type benchClient struct {
	nc     *net.UnixConn
	rd     *deadlineReader
	lr     *protocol.LineReader
	nextID int

	// What drain has seen, for awaitLive: seq is the seq of the last event
	// notification it read; want is the seq awaitLive waits for, and reached
	// closes once seq has reached it; reset holds a reset notification's
	// reason, and resetCh closes when one arrives; exited closes when drain
	// returns, exitErr (written before) saying why.
	seq       atomic.Uint64
	want      atomic.Uint64
	reached   chan struct{}
	reachOnce sync.Once
	reset     atomic.Value // protocol.ResetReason
	resetCh   chan struct{}
	resetOnce sync.Once
	exited    chan struct{}
	exitErr   error
}

// resetSeen reports whether drain ever saw a reset notification on this
// client's connection, and its reason.
func (c *benchClient) resetSeen() (protocol.ResetReason, bool) {
	v := c.reset.Load()
	if v == nil {
		return "", false
	}
	return v.(protocol.ResetReason), true
}

// awaitLive waits until drain has read the event at seq — every event up to
// the log's head has reached this subscriber, so it stayed live — and fails
// if a reset arrives instead, drain exits first, or wait passes. want is
// stored before seq is read, and drain stores seq before it reads want, so
// one of the two always sees the other's: no wake-up is lost between them.
func (c *benchClient) awaitLive(seq uint64, wait time.Duration) error {
	c.want.Store(seq)
	if r, ok := c.resetSeen(); ok {
		return fmt.Errorf("saw reset{%s}", r)
	}
	if c.seq.Load() >= seq {
		return nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-c.reached:
	case <-c.resetCh:
	case <-c.exited:
		return fmt.Errorf("its drain exited at seq %d: %v", c.seq.Load(), c.exitErr)
	case <-timer.C:
		return fmt.Errorf("it read only through seq %d within %s", c.seq.Load(), wait)
	}
	if r, ok := c.resetSeen(); ok {
		return fmt.Errorf("saw reset{%s}", r)
	}
	return nil
}

// readReset reads notifications until it finds a reset, returning its
// reason, or fails the benchmark once deadline passes — a client that was
// never actually reset must not hang the benchmark waiting for one. The
// deadline caps every read it makes (deadlineReader.cap).
func (c *benchClient) readReset(b *testing.B, deadline time.Time) protocol.ResetReason {
	b.Helper()
	c.rd.cap = deadline
	defer func() { c.rd.cap = time.Time{} }()
	for {
		raw, err := c.lr.ReadLine()
		if err != nil {
			b.Fatalf("reading for reset{slow_consumer}: %v", err)
		}
		var n protocol.Notification
		if json.Unmarshal(raw, &n) != nil {
			continue
		}
		if n.Method != protocol.NotifyReset {
			continue
		}
		var p protocol.ResetParams
		if err := json.Unmarshal(n.Params, &p); err != nil {
			b.Fatalf("decode reset params: %v", err)
		}
		return p.Reason
	}
}

func dialBenchClient(b *testing.B, path string) *benchClient {
	b.Helper()
	nc, err := net.Dial("unix", path)
	if err != nil {
		b.Fatalf("dial: %v", err)
	}
	uc := nc.(*net.UnixConn)
	rd := &deadlineReader{nc: uc}
	return &benchClient{
		nc:      uc,
		rd:      rd,
		lr:      protocol.NewLineReader(rd, protocol.OutboundLineMax),
		reached: make(chan struct{}),
		resetCh: make(chan struct{}),
		exited:  make(chan struct{}),
	}
}

// call sends one request and returns its reply, failing on a refusal.
func (c *benchClient) call(b *testing.B, method string, params any) *protocol.Response {
	b.Helper()
	c.nextID++
	id := strconv.Itoa(c.nextID)
	raw, err := json.Marshal(params)
	if err != nil {
		b.Fatalf("marshal %s: %v", method, err)
	}
	line, err := protocol.MarshalLine(protocol.Request{
		JSONRPC: protocol.JSONRPCVersion, ID: json.RawMessage(id), Method: method, Params: raw,
	})
	if err != nil {
		b.Fatalf("encode %s: %v", method, err)
	}
	if _, err := c.nc.Write(line); err != nil {
		b.Fatalf("write %s: %v", method, err)
	}
	raw, err = c.lr.ReadLine()
	if err != nil {
		b.Fatalf("read %s reply: %v", method, err)
	}
	var resp protocol.Response
	if err := json.Unmarshal(raw, &resp); err != nil {
		b.Fatalf("decode %s reply: %v", method, err)
	}
	if string(resp.ID) != id {
		b.Fatalf("%s: reply id %s, want %s", method, resp.ID, id)
	}
	if resp.Error != nil {
		b.Fatalf("%s refused: %+v", method, resp.Error)
	}
	return &resp
}

// readNotification reads the next line, which must be a notification of
// method.
func (c *benchClient) readNotification(b *testing.B, method string) *protocol.Notification {
	b.Helper()
	raw, err := c.lr.ReadLine()
	if err != nil {
		b.Fatalf("read %s: %v", method, err)
	}
	var n protocol.Notification
	if err := json.Unmarshal(raw, &n); err != nil {
		b.Fatalf("decode notification: %v", err)
	}
	if n.Method != method {
		b.Fatalf("notification %s, want %s", n.Method, method)
	}
	return &n
}

func benchHello(b *testing.B, c *benchClient) {
	b.Helper()
	c.call(b, protocol.MethodHello, protocol.HelloParams{
		Protocols: []int{protocol.ProtocolVersion},
		Client:    protocol.ClientInfo{Kind: "bench", Name: "publish_bench"},
	})
}

func benchAttach(b *testing.B, c *benchClient, sessionID string, budget *protocol.AttachBudget) protocol.AttachResult {
	b.Helper()
	resp := c.call(b, protocol.MethodSessionAttach, protocol.AttachParams{
		SessionID: sessionID, When: protocol.WhenNow, Budget: budget,
	})
	var ar protocol.AttachResult
	if err := json.Unmarshal(resp.Result, &ar); err != nil {
		b.Fatalf("decode attach result: %v", err)
	}
	return ar
}

// drain reads every line c gets, discarding events, until the connection
// closes. The attach point is always the snapshot's head here (no cursor),
// so the very first notification is synchronized (§3.4); synced receives
// exactly once — true once it has been seen (what setup waits on before
// starting the timer), or false on any exit that never saw one, so a drain
// that dies before its first synchronized can never hang setup's <-synced.
// A reset seen at any point (a slow_consumer drop reaching a subscriber
// that was supposed to keep draining, in particular) is recorded on c via
// c.reset rather than acted on here: the connection stays open after a
// reset that does not end the session (control/forward.go), so drain keeps
// reading; resetCh wakes an awaitLive already waiting. Each event
// notification's seq is recorded on c (c.seq), and reached closes once it
// has reached the seq awaitLive waits for. Every read has a deadline
// (deadlineReader), so a drain whose socket goes quiet for benchReadTimeout
// exits — never blocking the benchmark for ever — and says why (exitErr,
// then exited).
//
// Past the first line, a line is never fully decoded unless it might be a
// reset. An event notification's seq is read straight from its fixed prefix
// (eventSeq: a prefix match and a bounded scan, never the event body), and
// only a line that is not an event is scanned for the reset marker and then
// decoded. A reset is rare (only when this subscriber is actually being
// dropped) next to the flood of ordinary event lines the timed loop
// produces, and decoding every one of those unconditionally is expensive
// enough on its own to starve this reader loop — a self-inflicted overflow
// the benchmark cannot tell apart from the real thing this check is
// supposed to catch.
func drain(c *benchClient, synced chan<- bool) {
	sent := false
	send := func(ok bool) {
		if sent {
			return
		}
		sent = true
		synced <- ok
	}
	defer send(false)
	var err error
	defer func() {
		c.exitErr = err
		close(c.exited)
	}()
	first := true
	for {
		var raw []byte
		raw, err = c.lr.ReadLine()
		if err != nil {
			return
		}
		if first {
			first = false
			var n protocol.Notification
			if json.Unmarshal(raw, &n) == nil && n.Method == protocol.NotifySynchronized {
				send(true)
			}
			continue
		}
		if seq, ok := eventSeq(raw); ok {
			c.seq.Store(seq)
			if w := c.want.Load(); w != 0 && seq >= w {
				c.reachOnce.Do(func() { close(c.reached) })
			}
			continue
		}
		if !bytes.Contains(raw, resetMethodMarker) {
			continue
		}
		var n protocol.Notification
		if json.Unmarshal(raw, &n) != nil || n.Method != protocol.NotifyReset {
			continue
		}
		reason := protocol.ResetReason("(undecodable)")
		var p protocol.ResetParams
		if json.Unmarshal(n.Params, &p) == nil {
			reason = p.Reason
		}
		c.reset.Store(reason)
		c.resetOnce.Do(func() { close(c.resetCh) })
	}
}

// resetMethodMarker is the substring a reset notification's line always
// contains (Notification's field order puts "method" right after
// "jsonrpc", and encoding/json's Marshal — protocol.MarshalLine's own —
// never inserts whitespace) — drain's cheap pre-filter before paying for a
// full decode.
var resetMethodMarker = []byte(`"method":"reset"`)

// eventLinePrefix is how every event notification's line begins, up to its
// subscription id (the forwarder's notification[EventParams], fields in
// declaration order, no whitespace), and eventSeqMarker what follows that
// id: the id is a short server-minted string, so the seq sits within the
// first few dozen bytes of the line, whatever the event's own size.
var (
	eventLinePrefix = []byte(`{"jsonrpc":"2.0","method":"event","params":{"subscription":"`)
	eventSeqMarker  = []byte(`","seq":`)
)

// eventSeq is an event notification line's seq, read without decoding the
// line: false for any line that is not an event notification. A line whose
// shape it does not recognise is never counted as progress, so a change to
// the wire shape shows up as awaitLive's loud timeout, not a false pass.
func eventSeq(raw []byte) (uint64, bool) {
	if !bytes.HasPrefix(raw, eventLinePrefix) {
		return 0, false
	}
	rest := raw[len(eventLinePrefix):]
	window := rest
	if len(window) > 128 {
		window = window[:128]
	}
	i := bytes.Index(window, eventSeqMarker)
	if i < 0 {
		return 0, false
	}
	rest = rest[i+len(eventSeqMarker):]
	var seq uint64
	n := 0
	for n < len(rest) && rest[n] >= '0' && rest[n] <= '9' {
		seq = seq*10 + uint64(rest[n]-'0')
		n++
	}
	if n == 0 {
		return 0, false
	}
	return seq, true
}
