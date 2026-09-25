package transcript

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/charliek/craze/internal/agent"
)

// childScript is a main transcript far larger than a small budget, then two
// children with a few entries each, and a roster row with no transcript at all
// (a cursor task).
func childScript() []agent.Event {
	var evs []agent.Event
	big := strings.Repeat("m", 8<<10)
	for i := range 24 {
		typ := agent.EventText
		if i%2 == 1 {
			typ = agent.EventThought
		}
		evs = append(evs, agent.Event{Type: typ, Text: big, At: at(i)})
	}
	evs = append(evs,
		agent.Event{Type: agent.EventSubagent, Subagent: &agent.SubagentInfo{ID: "sub-1", Status: agent.SubagentRunning}, SubagentChange: agent.SubagentChangeSpawned, At: at(30)},
		agent.Event{Type: agent.EventUser, Agent: "sub-1", Text: "one: look", At: at(31)},
		agent.Event{Type: agent.EventText, Agent: "sub-1", Text: strings.Repeat("1", 4<<10), At: at(32)},
		agent.Event{Type: agent.EventSubagent, Subagent: &agent.SubagentInfo{ID: "sub-2", Status: agent.SubagentRunning}, SubagentChange: agent.SubagentChangeSpawned, At: at(33)},
		agent.Event{Type: agent.EventUser, Agent: "sub-2", Text: "two: look", At: at(34)},
		agent.Event{Type: agent.EventThought, Agent: "sub-2", Text: strings.Repeat("2", 8<<10), At: at(35)},
		agent.Event{Type: agent.EventText, Agent: "sub-2", Text: "two: found it", At: at(36)},
		agent.Event{Type: agent.EventSubagent, Subagent: &agent.SubagentInfo{ID: "task-3", Status: agent.SubagentRunning}, SubagentChange: agent.SubagentChangeSpawned, At: at(37)},
		// The main transcript's newest entry, after the children's.
		agent.Event{Type: agent.EventText, Text: "main: the newest", At: at(38)},
	)
	return sequenced(evs)
}

func subOf(s *Snapshot, id string) *TranscriptSnap {
	for i := range s.Subs {
		if s.Subs[i].ID == id {
			return &s.Subs[i].TranscriptSnap
		}
	}
	return nil
}

// TestASnapshotForAChildFillsItsWindowFirst: session.snapshot's agentId (plan
// 027 §3.4). With a budget the main transcript alone overflows, Snapshot
// leaves a child's entries out (main is filled first); SnapshotFor gives the
// child its window right after main's newest entry — which stays in, so the
// snapshot restores as any other — within the same budget, and through the
// codec. An id nothing in the model names is ErrNoSuchSubagent; a roster row
// with no transcript, and "", answer as Snapshot does.
func TestASnapshotForAChildFillsItsWindowFirst(t *testing.T) {
	m := New(Options{Incarnation: "inc-1"})
	for _, ev := range childScript() {
		m.Fold(ev)
	}
	const budget = 40 << 10

	plain, plainBytes := snapshotOf(t, m, budget)
	if sub := subOf(plain, "sub-2"); sub == nil || !sub.Windowed {
		t.Fatalf("the premise: main should take the budget and leave sub-2 windowed; sub-2 is %+v", sub)
	}

	focused, err := m.SnapshotFor("sub-2", budget)
	if err != nil {
		t.Fatalf("SnapshotFor(sub-2): %v", err)
	}
	b, err := EncodeSnapshot(focused)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) > budget {
		t.Fatalf("the focused snapshot is %d bytes, over its budget of %d", len(b), budget)
	}
	sub := subOf(focused, "sub-2")
	if sub == nil || len(sub.Entries) != 3 || sub.Windowed {
		t.Fatalf("sub-2 should be whole in its own snapshot: %+v", sub)
	}
	if n := len(focused.Main.Entries); n == 0 || focused.Main.Entries[n-1].Text != "main: the newest" {
		t.Fatalf("the main transcript's newest entry must stay in: %d entries", n)
	}
	if len(focused.Main.Entries) >= len(plain.Main.Entries) {
		t.Fatalf("main kept %d entries beside the child, as many as alone (%d)", len(focused.Main.Entries), len(plain.Main.Entries))
	}
	// It restores exactly as any snapshot does, in process and through the
	// codec, with the model's invariants.
	restoredBoth(t, focused, b, Options{})

	if _, err := m.SnapshotFor("sub-9", budget); !errors.Is(err, agent.ErrNoSuchSubagent) {
		t.Fatalf("an unknown id: %v, want agent.ErrNoSuchSubagent", err)
	}
	for _, id := range []string{"task-3", ""} {
		s, err := m.SnapshotFor(id, budget)
		if err != nil {
			t.Fatalf("SnapshotFor(%q): %v", id, err)
		}
		got, err := EncodeSnapshot(s)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, plainBytes) {
			t.Fatalf("SnapshotFor(%q) should be Snapshot's", id)
		}
	}
	// A budget the mandatory sections and main's newest entry do not fit is
	// refused as Snapshot refuses it.
	if _, err := m.SnapshotFor("sub-2", 64); !errors.Is(err, ErrSnapshotTooLarge) {
		t.Fatalf("a budget of 64 bytes: %v, want ErrSnapshotTooLarge", err)
	}
}
