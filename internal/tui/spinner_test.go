package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/craze/internal/agent"
)

// mixedEvents is a turn's worth of traffic: chunks, thoughts, tool updates and
// a todo list, none of which should start a tick chain of its own.
func mixedEvents() []tea.Msg {
	tool := agent.ToolEvent{ID: "sh-1", Kind: "execute", Title: "`go vet ./...`", Status: "in_progress", RawInput: "go vet ./..."}
	todos := []agent.Todo{{ID: "1", Content: "Read main.go", Status: "in_progress"}}
	var out []tea.Msg
	for i := 0; i < 5; i++ {
		out = append(out,
			eventMsg{agent.Event{Type: agent.EventThought, Text: "thinking "}},
			eventMsg{agent.Event{Type: agent.EventText, Text: "chunk "}},
			eventMsg{agent.Event{Type: agent.EventTool, Tool: &tool}},
			eventMsg{agent.Event{Type: agent.EventTodos, Todos: todos}},
		)
	}
	return out
}

func TestOneTickChainAcrossManyEvents(t *testing.T) {
	m := hangWorking(t)
	if !m.tickLive || !m.tickFast {
		t.Fatalf("a working turn runs the fast chain: live=%v fast=%v", m.tickLive, m.tickFast)
	}
	gen := m.tickGen

	msgs := mixedEvents()
	if len(msgs) != 20 {
		t.Fatalf("fixture has %d events, want 20", len(msgs))
	}
	for _, msg := range msgs {
		tm, _ := m.Update(msg)
		m = tm.(Model)
	}
	if m.tickGen != gen {
		t.Fatalf("20 events started %d extra chains", m.tickGen-gen)
	}
	if !m.tickLive || !m.tickFast {
		t.Fatal("the chain should still be the live fast one")
	}
}

func TestStaleTickGenerationsAreDropped(t *testing.T) {
	m := hangWorking(t)
	gen := m.tickGen
	frame := m.spinFrame

	tm, cmd := m.Update(tickMsg{gen: gen - 1})
	m = tm.(Model)
	if m.spinFrame != frame {
		t.Fatal("a stale beat must not move the spinner")
	}
	if m.tickGen != gen || !m.tickLive {
		t.Fatalf("a stale beat must not restart the chain: gen %d live %v", m.tickGen, m.tickLive)
	}
	if cmd != nil {
		t.Fatal("a stale beat must not arm a second chain")
	}

	tm, cmd = m.Update(tickMsg{gen: gen})
	m = tm.(Model)
	if m.spinFrame != frame+1 {
		t.Fatal("a live beat advances the spinner glyph")
	}
	if m.tickGen != gen+1 || cmd == nil {
		t.Fatalf("a live beat re-arms the chain: gen %d cmd %v", m.tickGen, cmd != nil)
	}
}

func TestIdleUsesTheSlowChain(t *testing.T) {
	m := sized(t)
	if !m.tickLive || m.tickFast {
		t.Fatalf("idle runs the slow chain: live=%v fast=%v", m.tickLive, m.tickFast)
	}
	gen := m.tickGen

	// A turn switches to the fast chain exactly once.
	m.input.SetValue("go")
	tm, _ := m.Update(enter())
	m = tm.(Model)
	if !m.tickFast || m.tickGen != gen+1 {
		t.Fatalf("working: fast=%v gen=%d", m.tickFast, m.tickGen)
	}
	tm, _ = m.Update(promptDoneMsg{res: agent.Result{StopReason: "end_turn"}})
	m = tm.(Model)
	if m.tickFast || m.tickGen != gen+2 {
		t.Fatalf("back to idle: fast=%v gen=%d", m.tickFast, m.tickGen)
	}

	if d := untilNextMinute(time.Date(2026, 9, 12, 10, 0, 15, 0, time.UTC)); d != 45*time.Second {
		t.Fatalf("slow beat in %s, want 45s", d)
	}
	if d := untilNextMinute(time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)); d != time.Minute {
		t.Fatalf("on the boundary: %s", d)
	}
}

func TestSpinnerVisibilityAndText(t *testing.T) {
	m := sized(t)
	if m.spinnerVisible() {
		t.Fatal("no spinner while idle")
	}
	if strings.Contains(plainView(m), "esc to interrupt") {
		t.Fatalf("idle frame has a spinner line:\n%s", plainView(m))
	}

	m = hangWorking(t)
	view := plainView(m)
	if !m.spinnerVisible() || !strings.Contains(view, "esc to interrupt") {
		t.Fatalf("missing the spinner while working:\n%s", view)
	}
	if !strings.Contains(view, spinnerGlyphs[0]) {
		t.Fatalf("missing the spinner glyph:\n%s", view)
	}

	tool := agent.ToolEvent{ID: "sh-1", Kind: "execute", Title: "`go vet ./...`", Status: "in_progress", RawInput: "go vet ./..."}
	m = applyInFlight(t, m, []agent.ToolEvent{tool})
	if got := m.spinnerActivity(); got != "Running go vet ./..." {
		t.Fatalf("activity %q", got)
	}

	task := agent.ToolEvent{ID: "task-1", Kind: "other", ToolName: "task", Title: "Task: count", Status: "in_progress"}
	m = applyInFlight(t, m, []agent.ToolEvent{tool, task})
	if got := m.spinnerActivity(); got != "Waiting for 1 sub-agent" {
		t.Fatalf("a sub-agent wins: %q", got)
	}

	m.sess.(*Stub).SetTools(nil)
	tm, _ := m.Update(refreshSnapMsg{})
	m = tm.(Model)
	tm, _ = m.Update(eventMsg{agent.Event{Type: agent.EventThought, Text: "hm"}})
	m = tm.(Model)
	if got := m.spinnerActivity(); got != "Thinking…" {
		t.Fatalf("activity %q", got)
	}
	tm, _ = m.Update(eventMsg{agent.Event{Type: agent.EventText, Text: "reply"}})
	m = tm.(Model)
	if got := m.spinnerActivity(); got != "Working" {
		t.Fatalf("activity %q", got)
	}
}

func TestSpinnerWaitsOnACard(t *testing.T) {
	m := sized(t)
	tm, _ := m.Update(eventMsg{agent.Event{
		Type:       agent.EventPermission,
		Permission: &agent.PermissionEvent{ID: "perm-1", Tool: "bash"},
	}})
	m = tm.(Model)
	view := plainView(m)
	if !strings.Contains(view, "Waiting for your answer") {
		t.Fatalf("a pending card shows the spinner:\n%s", view)
	}
	if !m.wantFastTick() {
		t.Fatal("a pending card keeps the fast chain")
	}
}
