package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/craze/internal/agent"
)

// TestCtrlLWhileEditingSavesThenSendsNowOnce: the composer holds a queued row,
// so the strong send is that row's send now — never a second copy of its text.
func TestCtrlLWhileEditingSavesThenSendsNowOnce(t *testing.T) {
	m, stub := heldWorking(t)
	m = pumpEnter(t, m, "PINEAPPLE")
	m = pumpKey(t, m, tea.KeyMsg{Type: tea.KeyUp})
	m = pumpKey(t, m, enter())
	if m.queueEdit == "" {
		t.Fatal("setup: expected edit mode")
	}
	m.input.SetValue("PINEAPPLE!")
	m = pumpKey(t, m, tea.KeyMsg{Type: tea.KeyCtrlL})
	if m.queueEdit != "" {
		t.Fatal("ctrl+l saves the edit first")
	}
	if got := queueTexts(m); len(got) != 1 || got[0] != "PINEAPPLE!" {
		t.Fatalf("the row is saved in place: %q", got)
	}
	if m.confirm == nil {
		t.Fatal("cursor asks before cancelling the turn")
	}
	m = pumpKey(t, m, enter())
	// The cancelled turn's successor is the armed row, and the turn it starts
	// ends by itself with nothing left to drain — so if the text had also gone
	// out as a draft there would be a third turn and a third user entry.
	m = pumpUntil(t, m, allOf(turnsReached(stub, 2), isIdle))
	if got := texts(m, entryUser); len(got) != 2 || got[1] != "PINEAPPLE!" {
		t.Fatalf("user entries %q", got)
	}
	if !queueEmpty(m) {
		t.Fatalf("the row left the queue when it was sent: %q", queueTexts(m))
	}
	assertPrompts(t, stub, "go", "PINEAPPLE!")
}

// TestEmptiedBandGivesTheKeyboardBackForGood: once the band empties under the
// keyboard, a later row must not take it again unasked.
func TestEmptiedBandGivesTheKeyboardBackForGood(t *testing.T) {
	m, _ := queueWorking(t)
	m = typeEnter(t, m, "one")
	tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyUp})
	m = tm.(Model)
	if !m.queueFocus {
		t.Fatal("setup: the band has the keyboard")
	}
	tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyBackspace})
	m = tm.(Model)
	if !queueEmpty(m) || m.queueFocus || !m.input.Focused() {
		t.Fatalf("the emptied band returns the keyboard: queue=%q focus=%v composer=%v", queueTexts(m), m.queueFocus, m.input.Focused())
	}
	m = typeEnter(t, m, "two")
	m.input.SetValue("ab")
	tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyBackspace})
	m = tm.(Model)
	if m.input.Value() != "a" || len(queueTexts(m)) != 1 {
		t.Fatalf("backspace belongs to the composer again: draft %q, queue %q", m.input.Value(), queueTexts(m))
	}
}

// TestArmedSendNowWaitsOutAForeignTurn: a confirmed row is not "already gone"
// because the agent is talking on its own; it fires when that stops.
func TestArmedSendNowWaitsOutAForeignTurn(t *testing.T) {
	m, stub := heldWorking(t)
	stub.SetProvider(agent.GrokProvider())
	m = pumpEnter(t, m, "PINEAPPLE")
	m = pumpKey(t, m, tea.KeyMsg{Type: tea.KeyUp})
	m = pumpKey(t, m, tea.KeyMsg{Type: tea.KeyCtrlL})
	m = pumpKey(t, m, enter())
	if !sendNowArmed(m) {
		t.Fatal("setup: the send is armed")
	}
	stub.SetForeignTurn(agent.ForeignTurnInfo{ID: "interject-fallback-1", Text: "note", Running: true})
	m = pumpUntil(t, m, allOf(isIdle, viewHas(foreignTurnNote)))
	if !sendNowArmed(m) {
		t.Fatal("the armed send must wait, not be dropped")
	}
	if got := queueTexts(m); len(got) != 1 || strings.Contains(plainView(m), "already gone") {
		t.Fatalf("the row stays and no false note shows:\n%s", plainView(m))
	}
	if n := turnsStarted(stub); n != 1 {
		t.Fatalf("nothing may start under a foreign turn: %d turns", n)
	}
	stub.SetForeignTurn(agent.ForeignTurnInfo{ID: "interject-fallback-1", Running: false})
	m = pumpUntil(t, m, turnsReached(stub, 2))
	if sendNowArmed(m) {
		t.Fatal("the armed send fires when the foreign turn ends")
	}
	if got := texts(m, entryUser); len(got) != 2 || got[1] != "PINEAPPLE" {
		t.Fatalf("user entries %q", got)
	}
	assertPrompts(t, stub, "go", "PINEAPPLE")
}

// TestPlaceholderSaysNothingUnderACard: a card owns the keyboard, so the
// composer must not advertise verbs it will not honour.
func TestPlaceholderSaysNothingUnderACard(t *testing.T) {
	m, stub := queueWorking(t)
	if m.composerHint() == "" {
		t.Fatal("setup: the mid-turn hint shows while working")
	}
	m = cardEvent(t, m, stub, agent.Event{Type: agent.EventPermission, Permission: stubPermissionEvent(true)})
	if !m.cardOpen() {
		t.Fatal("setup: expected a card")
	}
	if hint := m.composerHint(); hint != "" {
		t.Fatalf("hint under a card: %q", hint)
	}
}

// TestStrongSendClearsADraftWithTrailingWhitespace: the armed text is trimmed,
// the draft may not be; the draft still leaves with it.
func TestStrongSendClearsADraftWithTrailingWhitespace(t *testing.T) {
	m, stub := heldWorking(t)
	m.input.SetValue("PINEAPPLE  ")
	m = pumpKey(t, m, tea.KeyMsg{Type: tea.KeyCtrlL})
	m = pumpKey(t, m, enter())
	m = pumpUntil(t, m, allOf(turnsReached(stub, 2), isIdle))
	if m.input.Value() != "" {
		t.Fatalf("the sent draft must leave the composer: %q", m.input.Value())
	}
	if got := texts(m, entryUser); len(got) != 2 || got[1] != "PINEAPPLE" {
		t.Fatalf("user entries %q", got)
	}
	assertPrompts(t, stub, "go", "PINEAPPLE")
}

// TestCtrlLRefusedWhileCancelling: a cancel in progress is one of the states
// that strands an interjection, so it is refused with the draft kept.
func TestCtrlLRefusedWhileCancelling(t *testing.T) {
	m, stub := queueWorkingLive(t)
	stub.SetProvider(agent.GrokProvider())
	tm, _ := m.Update(refreshSnapMsg{})
	m = tm.(Model)
	stub.mu.Lock()
	stub.cancelling = true
	stub.mu.Unlock()
	m.input.SetValue("BANANA")
	tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlL})
	m = tm.(Model)
	if m.input.Value() != "BANANA" {
		t.Fatalf("a refused interjection keeps the draft: %q", m.input.Value())
	}
	if !strings.Contains(plainView(m), "nothing to interject into") {
		t.Fatalf("the note is missing:\n%s", plainView(m))
	}
}

// TestConfirmOutranksTheSubagentView: a click on a row while the confirm is
// up declines it rather than opening a view over an armed question.
func TestConfirmOutranksTheSubagentView(t *testing.T) {
	m, _ := queueWorking(t)
	m = applyInFlight(t, m, []agent.ToolEvent{taskTool("task-1", "count lines", "in_progress")})
	m.input.SetValue("PINEAPPLE")
	tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlL})
	m = tm.(Model)
	if m.confirm == nil {
		t.Fatal("setup: expected the confirm")
	}
	row := m.lay.Region(regionAgents).Top
	got := clickAt(t, m, row)
	if got.confirm != nil {
		t.Fatal("a click declines the confirm")
	}
	if got.input.Value() != "PINEAPPLE" {
		t.Fatalf("declining keeps the draft: %q", got.input.Value())
	}
	// Declining is all the click does: it opens nothing and acts on no row.
	if got.viewing != "" {
		t.Fatalf("the declining click opened a view: %q", got.viewing)
	}
	// The same for a row: select it (its action strip is drawn), arm its
	// send now, then click where [cancel] sits — the click declines and
	// does not remove the row.
	m = typeEnter(t, got, "MANGO")
	tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyUp})
	m = tm.(Model)
	tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlL})
	m = tm.(Model)
	if m.confirm == nil {
		t.Fatal("setup: expected the row's confirm")
	}
	y := m.lay.Region(regionQueue).Top
	got = clickXY(t, m, m.width-3, y)
	if got.confirm != nil || len(queueTexts(got)) != 1 || got.queueEdit != "" {
		t.Fatalf("a declining click on a row must not act on it: confirm=%v queue=%q edit=%q", got.confirm != nil, queueTexts(got), got.queueEdit)
	}
}

// TestBuiltinsRefusedDuringAForeignTurn: the agent is busy on its own, so
// the builtins that need an idle session are refused as they are mid-turn.
func TestBuiltinsRefusedDuringAForeignTurn(t *testing.T) {
	m, stub := heldWorking(t)
	stub.SetProvider(agent.GrokProvider())
	stub.SetForeignTurn(agent.ForeignTurnInfo{ID: "interject-fallback-1", Text: "note", Running: true})
	// craze's own turn ends, so the session is idle — but the agent is still
	// running a turn of its own.
	m = pumpEsc(t, m)
	m = pumpUntil(t, m, allOf(isIdle, viewHas(foreignTurnNote)))
	if !m.snap.ForeignTurn {
		t.Fatal("setup: the foreign turn should still be running")
	}
	m = typeEnter(t, m, "/model")
	if m.dialog == dialogModel {
		t.Fatal("/model must be refused while the agent runs a turn of its own")
	}
	if !queueEmpty(m) {
		t.Fatalf("a refused builtin is never queued: %q", queueTexts(m))
	}
	m = typeEnter(t, m, "/help")
	if m.dialog != dialogHelp {
		t.Fatal("/help still runs")
	}
}

// TestEditChipFollowsTheRow: the chip numbers the row as it is now, not as it
// was when the edit began.
func TestEditChipFollowsTheRow(t *testing.T) {
	m, stub := queueWorking(t)
	m = typeEnter(t, m, "one")
	m = typeEnter(t, m, "two")
	tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyUp})
	m = tm.(Model)
	tm, _ = m.Update(enter())
	m = tm.(Model)
	if !strings.Contains(plainView(m), "editing #2") {
		t.Fatalf("setup:\n%s", plainView(m))
	}
	first := queueIDs(m)[0]
	stub.Unqueue(first)
	tm, _ = m.Update(refreshSnapMsg{})
	m = tm.(Model)
	if v := plainView(m); !strings.Contains(v, "editing #1") {
		t.Fatalf("the chip must follow the row:\n%s", v)
	}
}

// TestEnterDuringAForeignTurnQueues: the agent is talking on its own, so a
// prompt would only be refused; the draft goes to the queue and drains when
// the foreign turn ends.
func TestEnterDuringAForeignTurnQueues(t *testing.T) {
	m, stub := heldWorking(t)
	stub.SetProvider(agent.GrokProvider())
	stub.SetForeignTurn(agent.ForeignTurnInfo{ID: "interject-fallback-1", Text: "note", Running: true})
	m = pumpEsc(t, m)
	m = pumpUntil(t, m, allOf(isIdle, viewHas(foreignTurnNote)))
	if !m.snap.ForeignTurn {
		t.Fatal("setup: the foreign turn should still be running")
	}
	m = pumpEnter(t, m, "PINEAPPLE")
	if n := turnsStarted(stub); n != 1 || len(queueTexts(m)) != 1 {
		t.Fatalf("enter under a foreign turn queues: %d turns, queue %q", n, queueTexts(m))
	}
	stub.SetForeignTurn(agent.ForeignTurnInfo{ID: "interject-fallback-1", Running: false})
	m = pumpUntil(t, m, allOf(turnsReached(stub, 2), isIdle))
	if !queueEmpty(m) {
		t.Fatalf("the queue drains when the foreign turn ends: %q", queueTexts(m))
	}
	assertPrompts(t, stub, "go", "PINEAPPLE")
}
