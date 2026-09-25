package fakehost

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
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
// must return an error, not return silently once its deadline passes. A
// negative control forcing the count-based bug this replaces — an unrelated
// connection's own close satisfying a "before minus dropped" target while
// the one DropConnections dropped is still mid-cleanup — needs two
// connections closing on independently scheduled goroutines racing this
// wait, a schedule the synchronous, single-threaded wire fixture runner (one
// op at a time, run() in wire_test.go) cannot force; this test instead
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
