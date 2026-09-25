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
