package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/craze/internal/agent"
)

func sized(t *testing.T) Model {
	t.Helper()
	m := New(Config{
		Session:   NewStub(),
		Theme:     "tokyo-night",
		Workspace: t.TempDir(),
		Model:     "grok",
		Yolo:      true,
	})
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = tm.(Model)
	tm, _ = m.Update(startedMsg{})
	return tm.(Model)
}

func enter() tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyEnter} }

func TestViewFooterAndComposer(t *testing.T) {
	m := sized(t)
	view := m.View()
	if !strings.Contains(view, "idle") {
		t.Fatalf("missing idle in view:\n%s", view)
	}
	if !strings.Contains(view, "yolo") {
		t.Fatalf("missing yolo:\n%s", view)
	}
	if !strings.Contains(view, "grok") {
		t.Fatalf("missing model:\n%s", view)
	}
	if !strings.Contains(view, ">") {
		t.Fatalf("missing composer:\n%s", view)
	}
}

func TestEnterEmptyDoesNotSend(t *testing.T) {
	m := sized(t)
	tm, cmd := m.Update(enter())
	m = tm.(Model)
	if m.status != statusIdle {
		t.Fatalf("status %s", m.status)
	}
	if cmd != nil {
		t.Fatal("empty enter should not start a prompt")
	}
}

func TestEnterSendsAndFollowUp(t *testing.T) {
	m := sized(t)
	m.input.SetValue("hello")
	tm, cmd := m.Update(enter())
	m = tm.(Model)
	if m.status != statusWorking {
		t.Fatalf("status %s", m.status)
	}
	if cmd == nil {
		t.Fatal("expected prompt cmd")
	}
	if got := strings.Join(texts(m, "user"), ""); got != "hello" {
		t.Fatalf("user %q", got)
	}
	tm, cmd = m.Update(enter())
	m = tm.(Model)
	if cmd != nil {
		t.Fatal("enter while working should be ignored")
	}

	tm, _ = m.Update(eventMsg{agent.Event{Type: agent.EventText, Text: "echo: hello"}})
	m = tm.(Model)
	tm, _ = m.Update(promptDoneMsg{res: agent.Result{StopReason: "end_turn"}})
	m = tm.(Model)
	if m.status != statusIdle {
		t.Fatalf("status %s after done", m.status)
	}
	if !strings.Contains(m.View(), "echo: hello") {
		t.Fatalf("missing assistant text:\n%s", m.View())
	}

	m.input.SetValue("again")
	tm, cmd = m.Update(enter())
	m = tm.(Model)
	if m.status != statusWorking || cmd == nil {
		t.Fatal("follow-up should send")
	}
	tm, _ = m.Update(eventMsg{agent.Event{Type: agent.EventText, Text: "follow-up: again"}})
	m = tm.(Model)
	tm, _ = m.Update(promptDoneMsg{res: agent.Result{StopReason: "end_turn"}})
	m = tm.(Model)
	if !strings.Contains(m.View(), "follow-up: again") {
		t.Fatalf("missing follow-up:\n%s", m.View())
	}
}

func TestQQuitsWhenComposerEmpty(t *testing.T) {
	m := sized(t)
	tm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	m = tm.(Model)
	if !m.quitting {
		t.Fatal("expected quit")
	}
	if cmd == nil {
		t.Fatal("expected quit cmd")
	}
	m.input.SetValue("keep")
	m.quitting = false
	tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	m = tm.(Model)
	if m.quitting {
		t.Fatal("q with composer text must type, not quit")
	}
	if m.input.Value() == "keep" {
		t.Fatal("q should be inserted into composer")
	}
}

func TestEscCancelsWorkingTurn(t *testing.T) {
	stub := NewStub()
	stub.HangNext()
	m := New(Config{Session: stub, Workspace: t.TempDir(), Yolo: true})
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = tm.(Model)
	tm, _ = m.Update(startedMsg{})
	m = tm.(Model)
	m.input.SetValue("wait")
	tm, _ = m.Update(enter())
	m = tm.(Model)
	if m.status != statusWorking {
		t.Fatal("want working")
	}
	tm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = tm.(Model)
	if cmd == nil {
		t.Fatal("expected cancel cmd")
	}
	tm, _ = m.Update(eventMsg{agent.Event{Type: agent.EventDone, StopReason: "cancelled"}})
	m = tm.(Model)
	tm, _ = m.Update(promptDoneMsg{res: agent.Result{StopReason: "cancelled"}})
	m = tm.(Model)
	if m.status != statusIdle {
		t.Fatalf("status %s", m.status)
	}
}

func TestPermissionOverlayKeys(t *testing.T) {
	m := sized(t)
	m.yolo = false
	tm, _ := m.Update(eventMsg{agent.Event{
		Type: agent.EventPermission,
		Permission: &agent.PermissionEvent{
			ID:   "perm-1",
			Tool: "Shell",
			Options: []agent.PermissionOption{
				{OptionID: "opt-once", Kind: "allow_once"},
				{OptionID: "opt-reject", Kind: "reject_once"},
			},
		},
	}})
	m = tm.(Model)
	if m.pending == nil {
		t.Fatal("expected overlay")
	}
	if !strings.Contains(m.View(), "permission Shell") {
		t.Fatalf("missing overlay:\n%s", m.View())
	}
	tm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	m = tm.(Model)
	if m.pending != nil {
		t.Fatal("overlay should clear")
	}
	if cmd == nil {
		t.Fatal("expected answer cmd")
	}
}

func TestPresets(t *testing.T) {
	if Preset("tokyo-night").Name != "tokyo-night" {
		t.Fatal("default")
	}
	if Preset("dark").Name != "dark" || Preset("light").Name != "light" {
		t.Fatal("named presets")
	}
	if Preset("unknown").Name != "tokyo-night" {
		t.Fatal("unknown should fall back")
	}
}

func texts(m Model, kind string) []string {
	var out []string
	for _, ln := range m.lines {
		if ln.kind == kind {
			out = append(out, ln.text)
		}
	}
	return out
}
