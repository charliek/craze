package tui

import (
	"testing"

	"github.com/charliek/craze/internal/agent"
)

// TestStubReplayIsBracketed: Stub.Start stands in for a loaded session, so it
// has to emit the same bracket the live session's session/load path does —
// start, every replayed event marked Replayed, end — and nothing at all when
// there is no replay to hand back.
func TestStubReplayIsBracketed(t *testing.T) {
	s := NewStub()
	t.Cleanup(func() { _ = s.Close() })
	s.Replay = []agent.Event{
		{Type: agent.EventUser, Text: "yesterday's prompt"},
		{Type: agent.EventThought, Text: "recalling"},
		{Type: agent.EventText, Text: "restored"},
	}
	if err := s.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	var got []agent.Event
	for len(got) < 5 {
		select {
		case ev := <-s.Events():
			got = append(got, ev)
		default:
			t.Fatalf("only %d events", len(got))
		}
	}
	if got[0].Type != agent.EventReplay || got[0].Replay.Phase != agent.ReplayStart {
		t.Fatalf("first event %+v", got[0])
	}
	if got[4].Type != agent.EventReplay || got[4].Replay.Phase != agent.ReplayEnd {
		t.Fatalf("last event %+v", got[4])
	}
	for _, ev := range got[1:4] {
		if !ev.Replayed {
			t.Fatalf("event %s is not marked replayed", ev.Type)
		}
	}
	if got[0].Replayed || got[4].Replayed {
		t.Fatal("the brackets must not be marked replayed")
	}
	if s.Snapshot().SessionID == "" {
		t.Fatal("a started stub session has an id, as a live one does")
	}
}

func TestStubStartWithoutReplayEmitsNothing(t *testing.T) {
	s := NewStub()
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case ev := <-s.Events():
		t.Fatalf("a new session emits nothing on start, got %+v", ev)
	default:
	}
}

// TestStubSetTitlePins mirrors the live session: /rename wins over a title the
// agent produces later, and neither call emits anything.
func TestStubSetTitlePins(t *testing.T) {
	s := NewStub()
	t.Cleanup(func() { _ = s.Close() })
	s.AgentTitle("the agent's own title")
	if got := s.Snapshot().Title; got != "the agent's own title" {
		t.Fatalf("title %q", got)
	}
	s.SetTitle("fix the flaky pty test")
	s.AgentTitle("a later agent title")
	if got := s.Snapshot().Title; got != "fix the flaky pty test" {
		t.Fatalf("the pin did not hold: %q", got)
	}
	select {
	case ev := <-s.Events():
		t.Fatalf("renaming emits nothing, got %+v", ev)
	default:
	}
}

// TestStubEmptyReplayStillBrackets: a session/load whose transcript turned out
// to be empty is still a load, and the live session brackets it. If the stub
// stayed silent, a model built with Config.Loading would never leave the
// restoring state and every wait on it would time out instead of failing with
// something a reader can act on.
func TestStubEmptyReplayStillBrackets(t *testing.T) {
	s := NewStub()
	t.Cleanup(func() { _ = s.Close() })
	s.Replay = []agent.Event{}
	if err := s.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{agent.ReplayStart, agent.ReplayEnd} {
		select {
		case ev := <-s.Events():
			if ev.Type != agent.EventReplay || ev.Replay == nil || ev.Replay.Phase != want {
				t.Fatalf("event %+v, want the %s bracket", ev, want)
			}
		default:
			t.Fatalf("an empty replay skipped the %s bracket", want)
		}
	}
	select {
	case ev := <-s.Events():
		t.Fatalf("an empty replay emitted more than its brackets: %+v", ev)
	default:
	}
}
