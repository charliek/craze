package acp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"testing"
	"time"
)

// Fixes from the astra review of C5 (review C5 items 5, 7, 9): the fence
// against a write already past its check, the sever against a frame waiting
// for the dispatch, and a trip read before session/new returns.

// TestFenceWaitsForAWriteInFlight (review C5 item 5): a frame that passed the
// fence check before the fence went up is written before fence returns, and
// nothing but a cancel is written after.
func TestFenceWaitsForAWriteInFlight(t *testing.T) {
	r, w := io.Pipe()
	t.Cleanup(func() { _ = r.Close(); _ = w.Close() })
	go func() { _, _ = io.Copy(io.Discard, r) }()
	enc := NewEncoder(w)
	parked := make(chan struct{})
	release := make(chan struct{})
	prev := testFenceChecked
	testFenceChecked = func(m *Message) {
		if m.Method == "x/park" {
			close(parked)
			<-release
		}
	}
	t.Cleanup(func() { testFenceChecked = prev })
	wrote := make(chan error, 1)
	go func() { wrote <- enc.WriteMessage(&Message{Method: "x/park"}) }()
	<-parked
	fenced := make(chan bool, 1)
	go func() { fenced <- enc.fence(5 * time.Second) }()
	select {
	case <-fenced:
		t.Fatal("fence returned while a write that passed its check was still to be written")
	case <-time.After(150 * time.Millisecond):
	}
	close(release)
	if err := <-wrote; err != nil {
		t.Fatalf("the write in flight: %v", err)
	}
	if ok := <-fenced; !ok {
		t.Fatal("fence gave up on a write that ended")
	}
	if err := enc.WriteMessage(&Message{Method: "session/prompt", ID: json.RawMessage(`1`)}); !errors.Is(err, ErrFenced) {
		t.Fatalf("a prompt after the fence: %v", err)
	}
	if err := enc.WriteMessage(&Message{Method: MethodSessionCancel}); err != nil {
		t.Fatalf("the cancel after the fence: %v", err)
	}
}

// TestFenceFailsAWriteTheAgentHolds (review C5 item 5): a write held by an
// agent that stopped reading is failed by the client's Fence within its
// bound, so it can never land after.
func TestFenceFailsAWriteTheAgentHolds(t *testing.T) {
	prevWait := fenceWait
	fenceWait = 50 * time.Millisecond
	t.Cleanup(func() { fenceWait = prevWait })
	r := newClaudeRig(t)
	r.ready(t)
	// Nobody reads the agent's stdin from here: a prompt's write blocks.
	wrote := make(chan error, 1)
	go func() {
		_, err := r.client.PromptBlocks(context.Background(), []ContentBlock{{Type: "text", Text: "x"}}, nil, nil)
		wrote <- err
	}()
	time.Sleep(100 * time.Millisecond)
	done := make(chan struct{})
	go func() { r.client.Fence(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Fence waited on a write the agent never reads")
	}
	select {
	case err := <-wrote:
		if err == nil {
			t.Fatal("the held prompt was written")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the held write never failed")
	}
}

// TestClaudeSeverChecksAgainUnderTheDispatch (review C5 item 7): a frame read
// before the sever and waiting for the dispatch is handed to no one.
func TestClaudeSeverChecksAgainUnderTheDispatch(t *testing.T) {
	r := newClaudeRig(t)
	r.ready(t)
	waiting := make(chan struct{})
	release := make(chan struct{})
	prev := testClaudeDispatch
	armed := true
	testClaudeDispatch = func(method string) {
		if method == MethodSessionUpdate && armed {
			armed = false
			close(waiting)
			<-release
		}
	}
	t.Cleanup(func() { testClaudeDispatch = prev })
	r.pushUpdate(t, rigRoot, chunk("late"))
	<-waiting
	r.client.conn.sever()
	close(release)
	time.Sleep(100 * time.Millisecond)
	if got := r.log.get(); slices.Contains(got, "agent_message_chunk") {
		t.Fatalf("a frame waiting for the dispatch reached the session after the sever: %q", got)
	}
}

// TestClaudeTripBeforeAFailedSetup (review C5 item 9): a trip the adapter
// sends before session/new's reply is handed to the handler at once, so a
// setup that then fails does not lose it.
func TestClaudeTripBeforeAFailedSetup(t *testing.T) {
	r := newClaudeRig(t)
	tripped := make(chan RateLimit, 4)
	r.client.SetRateLimitHandler(func(rl RateLimit) error {
		if rl.Trips() {
			tripped <- rl
			return errors.New("tripped")
		}
		return nil
	})
	done := make(chan error, 1)
	go func() {
		_, err := r.client.NewSession(context.Background(), t.TempDir(), SessionParams{})
		done <- err
	}()
	req := r.readWithin(t, 3*time.Second, "session/new")
	r.pushUpdate(t, rigRoot, usageWith(`{"status":"allowed","isUsingOverage":true}`, false))
	if err := r.enc.WriteMessage(&Message{JSONRPC: jsonrpcVersion, ID: req.ID, Error: &RPCError{Code: -32603, Message: "Internal error"}}); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err == nil {
		t.Fatal("the setup succeeded")
	}
	select {
	case <-tripped:
	case <-time.After(3 * time.Second):
		t.Fatal("the trip before a failed setup never reached the handler")
	}
}

// TestClaudeSeverDropsWhatTheLoadHeld (review C5 item 7): updates and
// requests held behind a load's reply are released by ReplayDone to no one
// once the connection is severed.
func TestClaudeSeverDropsWhatTheLoadHeld(t *testing.T) {
	r := newClaudeRig(t)
	done := make(chan error, 1)
	go func() {
		_, err := r.client.LoadSession(context.Background(), rigRoot, t.TempDir(), SessionParams{})
		done <- err
	}()
	req := r.readWithin(t, 3*time.Second, "session/load")
	r.answerWith(t, req, `{}`)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	r.pushUpdate(t, rigRoot, chunk("held"))
	r.roundTrip(t)
	r.client.conn.sever()
	if err := r.client.ReplayDone(); err != nil {
		t.Fatal(err)
	}
	if got := r.log.get(); len(got) != 0 {
		t.Fatalf("held frames reached the session after the sever: %q", got)
	}
}

// TestClaudeTripDuringThePostLoadHold (review C4c finding 2): a live report
// that trips, arriving while the load's hold keeps updates back, reaches the
// handler at once — before the hold drains, and whether or not it ever does —
// and closes the machine; one the replay itself carries does not.
func TestClaudeTripDuringThePostLoadHold(t *testing.T) {
	r := newClaudeRig(t)
	tripped := make(chan struct{}, 4)
	r.client.SetRateLimitHandler(func(rl RateLimit) error {
		if rl.Trips() {
			tripped <- struct{}{}
			return errors.New("tripped")
		}
		return nil
	})
	done := make(chan error, 1)
	go func() {
		_, err := r.client.LoadSession(context.Background(), rigRoot, t.TempDir(), SessionParams{})
		done <- err
	}()
	req := r.readWithin(t, 3*time.Second, "session/load")
	// History: a trip the replay carries is not acted on.
	r.pushUpdate(t, rigRoot, usageWith(`{"status":"allowed","isUsingOverage":true}`, false))
	r.answerWith(t, req, `{}`)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	r.roundTrip(t)
	if len(tripped) != 0 {
		t.Fatal("a replayed report tripped")
	}
	// Live, held.
	r.pushUpdate(t, rigRoot, usageWith(`{"status":"allowed","isUsingOverage":true}`, false))
	r.roundTrip(t)
	select {
	case <-tripped:
	default:
		t.Fatal("a live report held behind the load's reply did not reach the handler")
	}
	// The hold fails; the trip has been acted on all the same.
	r.client.mu.Lock()
	gen := r.client.cf.holdGen
	r.client.mu.Unlock()
	r.client.failHold(gen, ErrReplayHold)
	if got := r.client.ForeignPhase(); got != "closed" {
		t.Fatalf("phase %s after a trip in the hold; want closed", got)
	}
}
