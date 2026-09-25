package control_test

import (
	"bytes"
	"context"
	"encoding/json"
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
// benchmark. subs=4+stalled adds one more attachment that reads its attach
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

	// SubscribersDropped (used above to drive the stall) is a log-wide
	// count: a draining subscriber can overflow and be dropped too, during
	// the rapid 1 MiB setup publishes or during the timed loop itself,
	// which would satisfy that check without the STALLED client being the
	// one actually dropped, and would silently measure fewer live
	// subscribers than the sub-benchmark names. Verify precisely instead of
	// trusting the count: the stalled client specifically must have been
	// reset slow_consumer, and every draining subscriber must have stayed
	// live for the whole benchmark (none of them ever saw a reset).
	if stalled {
		reason := sc.readReset(b, time.Now().Add(5*time.Second))
		if reason != protocol.ResetSlowConsumer {
			b.Fatalf("stalled client's reset reason = %q, want %q", reason, protocol.ResetSlowConsumer)
		}
	}
	for i, c := range conns {
		if r, ok := c.resetSeen(); ok {
			b.Fatalf("draining subscriber %d saw reset{%s}; it should have stayed live for the whole benchmark", i, r)
		}
	}
}

// benchClient is a minimal raw NDJSON client for the publish benchmark: no
// schema checking (the timed loop never touches the wire; only setup and the
// drain goroutines do) — just enough to hello, attach and read notifications.
type benchClient struct {
	nc     *net.UnixConn
	lr     *protocol.LineReader
	nextID int
	reset  atomic.Value // protocol.ResetReason, set once if drain ever sees a reset notification
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

// readReset reads notifications until it finds a reset, returning its
// reason, or fails the benchmark once deadline passes — a client that was
// never actually reset must not hang the benchmark waiting for one.
func (c *benchClient) readReset(b *testing.B, deadline time.Time) protocol.ResetReason {
	b.Helper()
	if err := c.nc.SetReadDeadline(deadline); err != nil {
		b.Fatalf("SetReadDeadline: %v", err)
	}
	defer func() { _ = c.nc.SetReadDeadline(time.Time{}) }()
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
	return &benchClient{nc: nc.(*net.UnixConn), lr: protocol.NewLineReader(nc, protocol.OutboundLineMax)}
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
// reading, and the caller checks c.resetSeen() once the benchmark is done.
//
// Past the first line, a line is fully decoded only when it might be a
// reset: a cheap byte scan first, json.Unmarshal only on what that scan
// flags. A reset is rare (only when this subscriber is actually being
// dropped) next to the flood of ordinary event lines the timed loop
// produces, and decoding every one of those unconditionally is expensive
// enough on its own to starve this reader loop — a self-inflicted overflow
// the benchmark cannot tell apart from the real thing this check is
// supposed to catch. Discarding an ordinary line un-decoded, as the
// original drain did, is what keeps this loop cheap enough to actually
// keep up.
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
	first := true
	for {
		raw, err := c.lr.ReadLine()
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
		if !bytes.Contains(raw, resetMethodMarker) {
			continue
		}
		var n protocol.Notification
		if json.Unmarshal(raw, &n) != nil || n.Method != protocol.NotifyReset {
			continue
		}
		var p protocol.ResetParams
		if json.Unmarshal(n.Params, &p) == nil {
			c.reset.Store(p.Reason)
		}
	}
}

// resetMethodMarker is the substring a reset notification's line always
// contains (Notification's field order puts "method" right after
// "jsonrpc", and encoding/json's Marshal — protocol.MarshalLine's own —
// never inserts whitespace) — drain's cheap pre-filter before paying for a
// full decode.
var resetMethodMarker = []byte(`"method":"reset"`)
