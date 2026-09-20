package tui

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
)

// The Stub publishes through agent.EventLog, the live session's own code
// (plan 020 §3.1, commit C4), so the chrome tests and goldens run on numbered
// events exactly as a session produces them. These tests hold the Stub to the
// same EventSource contract internal/agent's eventsource_test.go holds the
// real sessions to.

// stubEventWait bounds every wait here; a correct run never comes near it.
const stubEventWait = 10 * time.Second

// stubBuffered is every event already in the Stub's primary, without waiting.
func stubBuffered(s *Stub) []agent.Event {
	var out []agent.Event
	for {
		select {
		case ev := <-s.Events():
			out = append(out, ev)
		default:
			return out
		}
	}
}

// stubRecords reads exactly n records, failing if the subscription ends or
// the wait runs out first.
func stubRecords(t *testing.T, sub *agent.Subscription, n int) []agent.Record {
	t.Helper()
	recs := make([]agent.Record, 0, n)
	deadline := time.After(stubEventWait)
	for len(recs) < n {
		select {
		case r, ok := <-sub.Records():
			if !ok {
				t.Fatalf("the subscription ended (%v) after %d of %d records", sub.Err(), len(recs), n)
			}
			recs = append(recs, r)
		case <-deadline:
			t.Fatalf("timed out after %d of %d records", len(recs), n)
		}
	}
	return recs
}

// stubRecordsToEnd reads until the subscription closes.
func stubRecordsToEnd(t *testing.T, sub *agent.Subscription) []agent.Record {
	t.Helper()
	var recs []agent.Record
	deadline := time.After(stubEventWait)
	for {
		select {
		case r, ok := <-sub.Records():
			if !ok {
				return recs
			}
			recs = append(recs, r)
		case <-deadline:
			t.Fatalf("the subscription was still open after %v", stubEventWait)
		}
	}
}

// assertContiguous fails unless evs carry seqs from, from+1, ….
func assertContiguous(t *testing.T, evs []agent.Event, from uint64) {
	t.Helper()
	for i, ev := range evs {
		if want := from + uint64(i); ev.Seq != want {
			t.Fatalf("event %d (%s) has seq %d, want %d", i, ev.Type, ev.Seq, want)
		}
	}
}

// TestStubIsAnEventSourceWithContiguousSeq: the Stub is an agent.EventSource;
// its events are numbered 1, 2, 3, … across a prompt and a second, unrelated
// emit; a Prompt that returned has its ending already buffered; a
// subscription taken first gets the same events in the same order; and Close
// ends it.
func TestStubIsAnEventSourceWithContiguousSeq(t *testing.T) {
	s := NewStub()
	t.Cleanup(func() { _ = s.Close() })
	src, ok := agent.Session(s).(agent.EventSource)
	if !ok {
		t.Fatal("the Stub is not an agent.EventSource")
	}
	inc := src.Incarnation()
	if inc == "" || inc == NewStub().Incarnation() {
		t.Fatalf("incarnation %q: want a non-empty id of the Stub's own", inc)
	}
	sub, err := src.Subscribe(agent.SubscribeOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.Prompt(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	turn := stubBuffered(s)
	if len(turn) == 0 || turn[len(turn)-1].Type != agent.EventDone {
		t.Fatalf("after Prompt returned, the buffered events do not end in EventDone: %+v", turn)
	}
	s.SetForeignTurn(agent.ForeignTurnInfo{ID: "later", Running: true})
	all := append(turn, stubBuffered(s)...)
	if len(all) != len(turn)+1 || all[len(all)-1].Type != agent.EventForeignTurn {
		t.Fatalf("the second emit is not the one event after the turn: %+v", all)
	}
	assertContiguous(t, all, 1)

	for i, rec := range stubRecords(t, sub, len(all)) {
		ev, err := rec.Event()
		if err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
		if ev.Seq != all[i].Seq || ev.Type != all[i].Type || ev.Text != all[i].Text {
			t.Fatalf("record %d is seq %d %s %q, the primary's is seq %d %s %q",
				i, ev.Seq, ev.Type, ev.Text, all[i].Seq, all[i].Type, all[i].Text)
		}
	}
	if got := src.Incarnation(); got != inc {
		t.Fatalf("Incarnation changed: %q, then %q", inc, got)
	}

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if rest := stubRecordsToEnd(t, sub); len(rest) != 0 {
		t.Fatalf("the subscription got %d records it had no event for", len(rest))
	}
	if err := sub.Err(); !errors.Is(err, agent.ErrClosed) {
		t.Fatalf("the subscription ended with %v, want agent.ErrClosed", err)
	}
}

// TestStubCloseCutsOffALateEmitter: an emit that outlives Close — one waiting
// on a full primary when Close began, or one that starts after — reaches
// neither the primary nor a subscription, and Close does not wait on it.
// Whether the waiting one got as far as the log before Close is the
// scheduler's choice; the outcome is the same either way, which is what is
// asserted (internal/agent pins the inside-the-log schedule with the log's
// hook).
func TestStubCloseCutsOffALateEmitter(t *testing.T) {
	s := NewStub()
	sub, err := s.Subscribe(agent.SubscribeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	const primaryCap = 256 // Events()'s buffer, as it always was
	for i := range primaryCap {
		s.Emit(agent.Event{Type: agent.EventText, Text: fmt.Sprintf("fill-%d", i)})
	}
	stubRecords(t, sub, primaryCap)

	emitted := make(chan struct{})
	go func() {
		defer close(emitted)
		s.Emit(agent.Event{Type: agent.EventText, Text: "late"})
	}()
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		_ = s.Close()
	}()
	for what, ch := range map[string]chan struct{}{"Close": closed, "the late emit": emitted} {
		select {
		case <-ch:
		case <-time.After(stubEventWait):
			t.Fatalf("%s did not return within %v", what, stubEventWait)
		}
	}
	s.Emit(agent.Event{Type: agent.EventText, Text: "after"})

	primary := stubBuffered(s)
	if len(primary) != primaryCap {
		t.Fatalf("the primary holds %d events, want the %d of the fill", len(primary), primaryCap)
	}
	assertContiguous(t, primary, 1)
	for _, ev := range primary {
		if ev.Text == "late" || ev.Text == "after" {
			t.Fatalf("an event emitted across or after Close reached the primary: %q", ev.Text)
		}
	}
	if rest := stubRecordsToEnd(t, sub); len(rest) != 0 {
		t.Fatalf("the subscription got %d records after Close", len(rest))
	}
	if err := sub.Err(); !errors.Is(err, agent.ErrClosed) {
		t.Fatalf("the subscription ended with %v, want agent.ErrClosed", err)
	}
	// Both were given up since Close began, and both are counted, whichever
	// way each went: in the log, or on the Stub's own closed fast path.
	if n := s.log.Health().DroppedAtClose; n != 2 {
		t.Fatalf("DroppedAtClose is %d, want 2: late and after", n)
	}
}
