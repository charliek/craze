package transcript

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charliek/craze/internal/agent"
)

// Progress (plan 030 §3.10) is what a sessions.list row's Doing and LastReply
// are read from: the running turn's newest running tool, an open assistant
// run, and the last completed reply — each as the fold left the main
// transcript.

func started(m *Model, id, text string) {
	m.Fold(agent.Event{Type: agent.EventTurn, Turn: &agent.TurnInfo{ID: id, Phase: agent.TurnStarted, Text: text}})
}

func tool(m *Model, id, title, status string) {
	m.Fold(agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: id, Name: "shell", Title: title, Status: status}})
}

func TestProgressOfAFreshModelIsNothing(t *testing.T) {
	if p := New(Options{}).Progress(); p != (Progress{}) {
		t.Fatalf("an empty transcript's progress %+v", p)
	}
}

// TestProgressNamesTheNewestRunningTool: of the running turn's tools, the
// most recently started one still running — not the newest row, not one that
// has settled, and its name when it has no title.
func TestProgressNamesTheNewestRunningTool(t *testing.T) {
	m := New(Options{})
	started(m, "turn-1", "fix it")
	tool(m, "t-1", "Read main.go", "in_progress")
	tool(m, "t-2", "Run `go test`", "pending")
	if got := m.Progress().Tool; got != "Run `go test`" {
		t.Fatalf("tool %q, want the newest running one", got)
	}
	tool(m, "t-2", "Run `go test`", "completed")
	if got := m.Progress().Tool; got != "Read main.go" {
		t.Fatalf("tool %q once the newest settled, want the one still running", got)
	}
	tool(m, "t-1", "Read main.go", "failed")
	if got := m.Progress().Tool; got != "" {
		t.Fatalf("tool %q with every tool settled", got)
	}
	m.Fold(agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: "t-3", Name: "grep", Status: "in_progress"}})
	if got := m.Progress().Tool; got != "grep" {
		t.Fatalf("tool %q: an untitled tool is named by its name", got)
	}
}

// TestProgressIsTheCurrentTurnsOnly: a tool left running by an earlier turn —
// a cancelled one whose status never settled — is not what the session is
// doing once a new turn has started, nor once a foreign turn has; an
// interjection does not start a turn.
func TestProgressIsTheCurrentTurnsOnly(t *testing.T) {
	m := New(Options{})
	started(m, "turn-1", "fix it")
	tool(m, "t-1", "Stuck", "in_progress")
	m.Fold(agent.Event{Type: agent.EventUser, Interjection: true, Text: "and the docs"})
	if got := m.Progress().Tool; got != "Stuck" {
		t.Fatalf("tool %q across an interjection, want the turn's running tool", got)
	}
	started(m, "turn-2", "next")
	if got := m.Progress().Tool; got != "" {
		t.Fatalf("tool %q: an earlier turn's unsettled tool leaked into the new one", got)
	}
	tool(m, "t-2", "Edit a.go", "in_progress")
	m.Fold(agent.Event{Type: agent.EventForeignTurn, ForeignTurn: &agent.ForeignTurnInfo{ID: "wake-1", Running: true}})
	if got := m.Progress().Tool; got != "" {
		t.Fatalf("tool %q in a foreign turn, want none of the turn before it", got)
	}
	tool(m, "t-3", "Wake work", "in_progress")
	if got := m.Progress().Tool; got != "Wake work" {
		t.Fatalf("tool %q, want the foreign turn's", got)
	}
}

// TestProgressRespondsWhileTextStreams: an open assistant run is Responding
// and not yet a reply; the run closing — the wire's ending — makes it the last
// reply, whole, which a thought or a tool after it leaves in place.
func TestProgressRespondsWhileTextStreams(t *testing.T) {
	m := New(Options{})
	started(m, "turn-1", "fix it")
	m.Fold(agent.Event{Type: agent.EventThought, Text: "hmm"})
	if p := m.Progress(); p.Responding || p.LastReply != "" {
		t.Fatalf("a thought streaming: %+v", p)
	}
	m.Fold(agent.Event{Type: agent.EventText, Text: "Fixed the "})
	m.Fold(agent.Event{Type: agent.EventText, Text: "test.\nIt was the clock."})
	if p := m.Progress(); !p.Responding || p.LastReply != "" {
		t.Fatalf("text streaming: %+v, want responding and no completed reply", p)
	}
	m.Fold(agent.Event{Type: agent.EventDone, StopReason: "end_turn"})
	if p := m.Progress(); p.Responding || p.LastReply != "Fixed the test.\nIt was the clock." {
		t.Fatalf("after the ending: %+v", p)
	}
	started(m, "turn-2", "more")
	tool(m, "t-1", "Read a.go", "completed")
	m.Fold(agent.Event{Type: agent.EventThought, Text: "and now"})
	if p := m.Progress(); p.Responding || p.LastReply != "Fixed the test.\nIt was the clock." {
		t.Fatalf("a later turn with no reply yet: %+v, want the last completed reply", p)
	}
	m.Fold(agent.Event{Type: agent.EventText, Text: "Second."})
	tool(m, "t-2", "Run it", "in_progress")
	if p := m.Progress(); p.Responding || p.LastReply != "Second." || p.Tool != "Run it" {
		t.Fatalf("a run a tool closed: %+v", p)
	}
}

// TestProgressOfALongReplyIsItsTail: a reply longer than the stream cap is
// its tail, led by "…" — the entry's own text — and the scan past thousands of
// rows after it still finds it.
func TestProgressOfALongReplyIsItsTail(t *testing.T) {
	m := New(Options{Bounds: Bounds{StreamText: 16}})
	started(m, "turn-1", "fix it")
	m.Fold(agent.Event{Type: agent.EventText, Text: strings.Repeat("a", 40) + "tail"})
	m.Fold(agent.Event{Type: agent.EventDone, StopReason: "end_turn"})
	for i := range 3000 {
		tool(m, fmt.Sprintf("t-%d", i), "t", "completed")
	}
	if got := m.Progress().LastReply; !strings.HasPrefix(got, "…") || !strings.HasSuffix(got, "tail") {
		t.Fatalf("last reply %q, want the capped tail", got)
	}
}

// TestProgressWakesOpenTheirTurn: every wake's opening note is a turn's
// boundary, as grok's interjection fallback's is — a sub-agent's wake's and a
// background job's (plan 033 §3.8, NoteJobWake) alike — so a tool the turn
// before it left running is not what the session is doing in the wake.
func TestProgressWakesOpenTheirTurn(t *testing.T) {
	for _, reason := range []string{"", agent.ReasonSubagentWake, agent.ReasonJobWake} {
		m := New(Options{})
		started(m, "turn-1", "fix it")
		tool(m, "t-1", "Stuck", "in_progress")
		if got := m.Progress().Tool; got != "Stuck" {
			t.Fatalf("control (%q): tool %q before the wake, want the turn's running tool", reason, got)
		}
		m.Fold(agent.Event{Type: agent.EventForeignTurn, ForeignTurn: &agent.ForeignTurnInfo{ID: "wake-1", Reason: reason, Running: true}})
		if got := m.Progress().Tool; got != "" {
			t.Fatalf("reason %q: tool %q in the wake, want none of the turn before it", reason, got)
		}
	}
}
