package fakehost

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/charliek/craze/internal/protocol"
)

// TestStallWritesWakesOnQuit is C9a review item 5: a writer stalled inside
// stallConn.Write must wake at once when its connection closes, not only at
// the stall's own deadline or a resume — so a Host.Quit (cmd/craze-fake-
// host's "quit" op and stdin EOF both mean this) with a client's reply still
// stalled behind an hour-long StallWrites exits promptly, rather than
// waiting out that hour. The bound below (1s) is generous: the wake-on-close
// path never depends on wall time, so it fires within microseconds once the
// connection actually closes.
func TestStallWritesWakesOnQuit(t *testing.T) {
	h, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp("", "czfh-quit-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "s")
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- h.Serve(l) }()

	nc, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = nc.Close() })

	// Serve runs its wrapping (newStallListener) on its own goroutine, before
	// calling control.Server.Serve: wait for it, so StallWrites never races
	// h.ln's assignment.
	deadline := time.Now().Add(time.Second)
	for h.listener() == nil {
		if time.Now().After(deadline) {
			t.Fatal("Serve never installed its listener")
		}
		time.Sleep(time.Millisecond)
	}

	// Stall every write for an hour: long enough that only the wake-on-close
	// path, never the deadline, can end this test's own wait.
	h.StallWrites(time.Hour)

	const hello = `{"jsonrpc":"2.0","id":"1","method":"hello","params":{"protocols":[1],"client":{"kind":"test","name":"fakehost-wire"}}}` + "\n"
	if _, err := nc.Write([]byte(hello)); err != nil {
		t.Fatal(err)
	}
	// Give the writer a moment to actually reach the stalled Write (no hook
	// says so directly; hello's own round trip — parse, dispatch, queue,
	// wake the writer — is microseconds next to this): past this point the
	// reply is genuinely blocked inside stallConn.Write, not merely queued.
	time.Sleep(50 * time.Millisecond)

	// The client is now awaiting hello's reply, which the stall holds up
	// forever (its Write never returns on its own) — exactly the scenario
	// the review names. Quitting must still close the Host promptly.
	done := make(chan error, 1)
	go func() { done <- h.Quit(context.Background()) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Quit: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Quit did not return within 1s: a stalled write did not wake on close")
	}

	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Serve did not return within 1s")
	}
}

// TestDropConnectionsErrorsOnStuckConnection is C9a review item 2's
// loud-failure half: if a connection never finishes closing, DropConnections
// must return an error, not return silently once its deadline passes. It
// exercises the wait DropConnections itself uses (waitForOpenConnsZero)
// directly, with a stub openConns that never reports zero, standing in for
// that stuck connection.
func TestDropConnectionsErrorsOnStuckConnection(t *testing.T) {
	err := waitForOpenConnsZero(20*time.Millisecond, func() int { return 1 })
	if err == nil {
		t.Fatal("waitForOpenConnsZero: expected an error, got nil")
	}
}

// TestDropConnectionsWaitsForZero is the non-stuck half: once openConns
// reports zero, the wait returns nil at once rather than sleeping out its
// full deadline.
func TestDropConnectionsWaitsForZero(t *testing.T) {
	calls := 0
	start := time.Now()
	err := waitForOpenConnsZero(time.Hour, func() int {
		calls++
		if calls < 3 {
			return 1
		}
		return 0
	})
	if err != nil {
		t.Fatalf("waitForOpenConnsZero: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("waitForOpenConnsZero took %s to notice openConns reached zero", elapsed)
	}
}

// TestDropConnectionsHoldsARedial is r23 finding (b): a client that redials
// while DropConnections runs must neither fail the op nor let it return
// before the connection it dropped is forgotten. Client A holds c-1; the drop
// closes it, and B dials in the middle of the drop — after A's socket is
// closed, possibly before A's cleanup has run. The listener's accept gate
// holds B (onHeld), so the wait's OpenConns() == 0 is reached with B still
// outside the server (drained's check), and B is served only after the op
// returns. Then the clock moves to the binding table's idle bound (twice the
// receipts' 10-minute age, fixture 13's advance) and B resumes c-1: a fresh
// id, resumed false, says A's release was recorded before the advance —
// i.e. before DropConnections returned. A release that ran after the drop
// returned (the arithmetic's early return) would be recorded at the
// advanced clock, too recent to idle out, and the resume would take.
func TestDropConnectionsHoldsARedial(t *testing.T) {
	h, socket := serveTestHost(t)
	a := dialTestClient(t, socket)
	first := a.hello(t, nil)
	if first.ClientID != "c-1" || first.Resumed {
		t.Fatalf("A's hello: clientId %q resumed %v, want c-1, false", first.ClientID, first.Resumed)
	}

	l := h.listener()
	held := make(chan struct{}, 1)
	l.setOnHeld(func() {
		select {
		case held <- struct{}{}:
		default:
		}
	})
	var b *testClient
	h.drop.closed = func() {
		b = dialTestClient(t, socket)
		select {
		case <-held:
		case <-time.After(fixtureTimeout):
			t.Fatal("B's redial was never held at the accept gate")
		}
	}
	h.drop.drained = func() {
		if n := h.srv.OpenConns(); n != 0 {
			t.Fatalf("OpenConns = %d when the drop's wait ended, want 0", n)
		}
		if n := l.heldAtGate(); n != 1 {
			t.Fatalf("%d connection(s) held at the gate when the drop's wait ended, want B's 1", n)
		}
	}
	if err := h.DropConnections(); err != nil {
		t.Fatalf("DropConnections: %v", err)
	}

	h.AdvanceClock(20 * time.Minute)
	got := b.hello(t, &protocol.Resume{ClientID: first.ClientID, Token: first.Token})
	if got.Resumed || got.ClientID != "c-2" {
		t.Fatalf("B's resume of c-1: clientId %q resumed %v, want c-2, false — A's release was not recorded before DropConnections returned", got.ClientID, got.Resumed)
	}
}

// serveTestHost builds a Host serving a fresh Unix socket, closed with the
// test, and waits until Serve has installed its listener.
func serveTestHost(t *testing.T) (*Host, string) {
	t.Helper()
	h, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp("", "czfh-t-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "s")
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- h.Serve(l) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), fixtureTimeout)
		defer cancel()
		_ = h.Close(ctx)
		<-served
	})
	deadline := time.Now().Add(fixtureTimeout)
	for h.listener() == nil {
		if time.Now().After(deadline) {
			t.Fatal("Serve never installed its listener")
		}
		time.Sleep(time.Millisecond)
	}
	return h, socket
}

// testClient is a raw NDJSON client: enough to say hello.
type testClient struct {
	nc net.Conn
	lr *protocol.LineReader
}

func dialTestClient(t *testing.T, socket string) *testClient {
	t.Helper()
	nc, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = nc.Close() })
	return &testClient{nc: nc, lr: protocol.NewLineReader(nc, protocol.OutboundLineMax)}
}

// hello says hello, resuming resume when it is non-nil, and returns the
// host's result; a refusal, or no reply within fixtureTimeout, fails t.
func (c *testClient) hello(t *testing.T, resume *protocol.Resume) protocol.HelloResult {
	t.Helper()
	params, err := json.Marshal(protocol.HelloParams{
		Protocols: []int{protocol.ProtocolVersion},
		Client:    protocol.ClientInfo{Kind: "test", Name: "fakehost-host-test"},
		Resume:    resume,
	})
	if err != nil {
		t.Fatal(err)
	}
	line, err := protocol.MarshalLine(protocol.Request{
		JSONRPC: protocol.JSONRPCVersion, ID: json.RawMessage(`"1"`), Method: protocol.MethodHello, Params: params,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.nc.Write(line); err != nil {
		t.Fatalf("write hello: %v", err)
	}
	if err := c.nc.SetReadDeadline(time.Now().Add(fixtureTimeout)); err != nil {
		t.Fatal(err)
	}
	raw, err := c.lr.ReadLine()
	if err != nil {
		t.Fatalf("read hello reply: %v", err)
	}
	var resp protocol.Response
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode hello reply: %v: %s", err, raw)
	}
	if resp.Error != nil {
		t.Fatalf("hello refused: %s", raw)
	}
	var res protocol.HelloResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		t.Fatalf("decode hello result: %v: %s", err, raw)
	}
	return res
}
