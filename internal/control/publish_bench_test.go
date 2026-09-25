package control_test

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/control"
	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/tui"
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

	srv := control.New(control.Options{Workspace: "/bench"})
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
		benchAttach(b, c, sessionID, nil)
		synced := make(chan struct{})
		wg.Add(1)
		go func(c *benchClient) {
			defer wg.Done()
			drain(c, synced)
		}(c)
		<-synced
		conns = append(conns, c)
	}

	if stalled {
		sc := dialBenchClient(b, path)
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
}

// benchClient is a minimal raw NDJSON client for the publish benchmark: no
// schema checking (the timed loop never touches the wire; only setup and the
// drain goroutines do) — just enough to hello, attach and read notifications.
type benchClient struct {
	nc     *net.UnixConn
	lr     *protocol.LineReader
	nextID int
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

// drain reads every line c gets and discards it, until the connection
// closes. The attach point is always the snapshot's head here (no cursor),
// so the very first notification is synchronized (§3.4); synced closes once
// it has been seen, which is what setup waits on before starting the timer.
func drain(c *benchClient, synced chan struct{}) {
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
				close(synced)
			}
		}
	}
}
