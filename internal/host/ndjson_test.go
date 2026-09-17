package host

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"testing"
	"time"
)

// TestRoundTripRejectsOversizedReply: a reply frame over 16 MiB with no
// newline is rejected without buffering the rest of it, and identifiably so
// (errFrameTooLarge), well before the ctx deadline — not merely because the
// deadline eventually fired on a connection stuck reading. Negative control
// checked: with the `len(line) > max` check in readBoundedLine commented
// out, the client instead read the full 17 MiB and then hung against <-stop
// until the 5 s ctx deadline, so the test failed on the elapsed-time bound
// (and, separately, errors.Is would have failed too, since the eventual
// error would wrap context.DeadlineExceeded, not errFrameTooLarge); the
// check was restored afterwards.
func TestRoundTripRejectsOversizedReply(t *testing.T) {
	srv := newFakeUDS(t, func(conn net.Conn, _ int, _ map[string]any, stop <-chan struct{}) {
		big := bytes.Repeat([]byte("a"), 17<<20) // over the 16 MiB cap, no '\n'
		if _, err := conn.Write(big); err != nil {
			return // the client closed once it hit the cap; nothing left to do
		}
		<-stop
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	called := false
	start := time.Now()
	err := roundTrip(ctx, srv.socket, map[string]string{"id": "t1"}, func(line []byte) (bool, error) {
		called = true
		return true, nil
	})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("want an error for a reply frame over the cap")
	}
	if !errors.Is(err, errFrameTooLarge) {
		t.Fatalf("want an error wrapping errFrameTooLarge, got %v", err)
	}
	if called {
		t.Fatal("accept must not be called with a partial oversized frame")
	}
	// The cap must trip well inside the 5 s ctx deadline; a generous bound so
	// a loaded machine cannot flake it, but tight enough to catch the size
	// check silently turning into a deadline wait.
	if elapsed > 2*time.Second {
		t.Fatalf("rejecting an oversized frame took %v, suspiciously close to the 5s ctx deadline", elapsed)
	}
}

// TestRoundTripReadsSplitReply: a reply written across several small writes
// still parses as one line. Negative control checked: writing the halves in
// the other order (second half first) made the JSON invalid and the test
// fail on decode, confirming the assembled bytes are actually asserted.
func TestRoundTripReadsSplitReply(t *testing.T) {
	srv := newFakeUDS(t, func(conn net.Conn, _ int, req map[string]any, _ <-chan struct{}) {
		id, _ := req["id"].(string)
		full, err := json.Marshal(okReply(id))
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		// Several short writes, not one: proves readBoundedLine accumulates
		// ReadSlice chunks rather than assuming one Read yields one line.
		for _, b := range [][]byte{full[:3], full[3:7], full[7:]} {
			if _, err := conn.Write(b); err != nil {
				t.Fatalf("write chunk: %v", err)
			}
		}
		if _, err := conn.Write([]byte("\n")); err != nil {
			t.Fatalf("write newline: %v", err)
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var got map[string]any
	err := roundTrip(ctx, srv.socket, map[string]string{"id": "split-1"}, func(line []byte) (bool, error) {
		dec := json.NewDecoder(bytes.NewReader(line))
		dec.UseNumber()
		if err := dec.Decode(&got); err != nil {
			return true, err
		}
		return true, nil
	})
	if err != nil {
		t.Fatalf("roundTrip: %v", err)
	}
	want := map[string]any{"id": "split-1", "result": map[string]any{"type": "ok"}}
	assertJSONEqual(t, got, want)
}

// TestRoundTripAcceptAsksForMoreLines: accept can decline a line and read
// another on the same connection — the mechanism roost (Commit 4) uses to
// skip "event" frames and mismatched ids. Negative control checked: making
// accept report done=true on the first line made the test assert on the
// event frame's own body and fail, confirming the second call really reads
// past the first line rather than re-parsing it.
func TestRoundTripAcceptAsksForMoreLines(t *testing.T) {
	srv := newFakeUDS(t, func(conn net.Conn, _ int, req map[string]any, _ <-chan struct{}) {
		id, _ := req["id"].(string)
		writeLine(t, conn, map[string]any{"type": "event", "name": "progress"})
		writeLine(t, conn, okReply(id))
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var lines []map[string]any
	err := roundTrip(ctx, srv.socket, map[string]string{"id": "multi-1"}, func(line []byte) (bool, error) {
		var m map[string]any
		dec := json.NewDecoder(bytes.NewReader(line))
		dec.UseNumber()
		if err := dec.Decode(&m); err != nil {
			return true, err
		}
		lines = append(lines, m)
		_, isEvent := m["type"]
		return !isEvent, nil
	})
	if err != nil {
		t.Fatalf("roundTrip: %v", err)
	}
	if len(lines) != 2 {
		t.Fatalf("accept saw %d lines, want 2 (the event, then the reply)", len(lines))
	}
	want := map[string]any{"id": "multi-1", "result": map[string]any{"type": "ok"}}
	assertJSONEqual(t, lines[1], want)
}
