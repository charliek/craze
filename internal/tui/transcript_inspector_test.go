package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/charliek/craze/internal/agent"
)

func TestTranscriptInspectorLocalDisclosure(t *testing.T) {
	m := sized(t)
	m.input.SetValue("unfinished draft")
	m.main.appendEntry(entry{kind: entryThought, text: "first private detail"}, m.now())
	m.main.appendEntry(entry{kind: entryThought, text: "second private detail"}, m.now())
	m.setViewportContent(true)
	m = pressKey(t, m, "f4")
	m = pressKey(t, m, "down")
	m = pressKey(t, m, "enter")
	view := ansi.Strip(m.transcriptView())
	if !strings.Contains(view, "second private detail") || strings.Contains(view, "first private detail") || !strings.Contains(view, "◀") {
		t.Fatalf("selected detail and focus marker: %q", view)
	}
	m = pressKey(t, m, "ctrl+o")
	if !strings.Contains(ansi.Strip(m.transcriptView()), "first private detail") {
		t.Fatal("global disclosure missed the first thought")
	}
	m = pressKey(t, m, "enter")
	m = pressKey(t, m, "ctrl+o")
	m = pressKey(t, m, "esc")
	view = ansi.Strip(m.transcriptView())
	if !strings.Contains(view, "second private detail") || strings.Contains(view, "first private detail") || strings.Contains(view, "◀") {
		t.Fatalf("local disclosure after global detail and Esc: %q", view)
	}
	if got := m.input.Value(); got != "unfinished draft" {
		t.Fatalf("draft after inspecting = %q", got)
	}
}

func TestTranscriptLayoutCycleAndPrecedence(t *testing.T) {
	m := sized(t)
	for _, want := range []transcriptLayout{layoutCompact, layoutClassic, layoutGrouped} {
		m = pressKey(t, m, "f2")
		if m.transcriptLayout != want || !strings.Contains(ansi.Strip(m.statusView(m.lay)), want.String()) {
			t.Fatalf("layout %v, status %q, want %v", m.transcriptLayout, m.statusView(m.lay), want)
		}
	}
	m = pressKey(t, m, "ctrl+g")
	m = pressKey(t, m, "f2")
	m = pressKey(t, m, "f4")
	if m.transcriptLayout != layoutGrouped || m.inspector.active || m.dialog != dialogTheme {
		t.Fatal("transcript shortcuts intercepted theme dialog keys")
	}
}

func TestTranscriptInspectorInSubagentAndUnderCard(t *testing.T) {
	m, stub := sizedCards(t)
	m = applyInFlight(t, m, []agent.ToolEvent{taskTool("task-1", "count", "in_progress")})
	m = openView(t, m)
	pane := m.cur()
	pane.appendEntry(entry{kind: entryThought, text: "child thought"}, m.now())
	m.setViewportContent(true)
	m = pressKey(t, m, "f4")
	m = pressKey(t, m, "enter")
	if !strings.Contains(ansi.Strip(m.transcriptView()), "child thought") {
		t.Fatal("child disclosure did not open")
	}
	m = cardEvent(t, m, stub, agent.Event{Type: agent.EventPermission, Permission: stubPermissionEvent(true)})
	m = pressKey(t, m, "f2")
	m = pressKey(t, m, "f4")
	if !m.cardOpen() || m.transcriptLayout != layoutGrouped || !m.inspector.active {
		t.Fatal("a pending card must keep its keyboard")
	}
	m, _ = press(m, runeKey('n'))
	m = pressKey(t, m, "esc")
	if m.inspector.active || m.viewing != "task-1" || m.cardOpen() {
		t.Fatal("Escape should release the inspector before leaving the child")
	}
	m = pressKey(t, m, "esc")
	if m.viewing != "" || !m.agentFocus {
		t.Fatal("the next Escape should restore subagent row focus")
	}
}

func TestInspectorPageTargetsVisibleHeader(t *testing.T) {
	m := sized(t)
	for range 60 {
		m.main.appendEntry(entry{kind: entryThought, text: "expand this row"}, m.now())
	}
	m.setViewportContent(true)
	m.vp.GotoTop()
	m = pressKey(t, m, "f4")
	first := m.inspector.entry
	m = pressKey(t, m, "pgdown")
	if idx := m.visibleTarget(); idx < 0 || m.vp.rows.span(idx).e == first {
		t.Fatal("paged inspector still targets a previous page")
	}
	m = pressKey(t, m, "enter")
	if first.detailOpen || !m.inspector.entry.detailOpen {
		t.Fatal("Enter should disclose the visible target")
	}
}
