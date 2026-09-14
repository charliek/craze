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
	m, _ := queueWorking(t)
	m = typeEnter(t, m, "PINEAPPLE")
	tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyUp})
	m = tm.(Model)
	tm, _ = m.Update(enter())
	m = tm.(Model)
	if m.queueEdit == "" {
		t.Fatal("setup: expected edit mode")
	}
	m.input.SetValue("PINEAPPLE!")
	tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlL})
	m = tm.(Model)
	if m.queueEdit != "" {
		t.Fatal("ctrl+l saves the edit first")
	}
	if got := queueTexts(m); len(got) != 1 || got[0] != "PINEAPPLE!" {
		t.Fatalf("the row is saved in place: %q", got)
	}
	if m.confirm == nil {
		t.Fatal("cursor asks before cancelling the turn")
	}
	seq := m.turnSeq
	tm, _ = m.Update(enter())
	m = tm.(Model)
	m = feed(t, m, agent.Event{Type: agent.EventDone, StopReason: stopCancelled})
	tm, _ = m.Update(promptDoneMsg{res: agent.Result{StopReason: stopCancelled}})
	m = tm.(Model)
	if m.turnSeq != seq+1 {
		t.Fatalf("exactly one turn started: turnSeq %d, was %d", m.turnSeq, seq)
	}
	if got := texts(m, entryUser); len(got) != 2 || got[1] != "PINEAPPLE!" {
		t.Fatalf("user entries %q", got)
	}
	if len(m.snap.Queue) != 0 {
		t.Fatalf("the row left the queue when it was sent: %+v", m.snap.Queue)
	}
	// The next settle has nothing left to send.
	m = feed(t, m, agent.Event{Type: agent.EventDone, StopReason: "end_turn"})
	tm, _ = m.Update(promptDoneMsg{res: agent.Result{StopReason: "end_turn"}})
	m = tm.(Model)
	if got := texts(m, entryUser); len(got) != 2 {
		t.Fatalf("the text went out twice: %q", got)
	}
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
	if len(m.snap.Queue) != 0 || m.queueFocus || !m.input.Focused() {
		t.Fatalf("the emptied band returns the keyboard: queue=%d focus=%v composer=%v", len(m.snap.Queue), m.queueFocus, m.input.Focused())
	}
	m = typeEnter(t, m, "two")
	m.input.SetValue("ab")
	tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyBackspace})
	m = tm.(Model)
	if m.input.Value() != "a" || len(m.snap.Queue) != 1 {
		t.Fatalf("backspace belongs to the composer again: draft %q, queue %d", m.input.Value(), len(m.snap.Queue))
	}
}

// TestArmedSendNowWaitsOutAForeignTurn: a confirmed row is not "already gone"
// because the agent is talking on its own; it fires when that stops.
func TestArmedSendNowWaitsOutAForeignTurn(t *testing.T) {
	m, stub := queueWorking(t)
	stub.SetProvider(agent.GrokProvider())
	m = typeEnter(t, m, "PINEAPPLE")
	tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyUp})
	m = tm.(Model)
	tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlL})
	m = tm.(Model)
	tm, _ = m.Update(enter())
	m = tm.(Model)
	if m.strong == nil {
		t.Fatal("setup: the send is armed")
	}
	seq := m.turnSeq
	foreign := agent.ForeignTurnInfo{ID: "interject-fallback-1", Text: "note", Running: true}
	stub.SetForeignTurn(foreign)
	m = feed(t, m, agent.Event{Type: agent.EventForeignTurn, ForeignTurn: &foreign})
	m = feed(t, m, agent.Event{Type: agent.EventDone, StopReason: stopCancelled})
	tm, _ = m.Update(promptDoneMsg{res: agent.Result{StopReason: stopCancelled}})
	m = tm.(Model)
	if m.strong == nil {
		t.Fatal("the armed send must wait, not be dropped")
	}
	if len(m.snap.Queue) != 1 || strings.Contains(plainView(m), "already gone") {
		t.Fatalf("the row stays and no false note shows:\n%s", plainView(m))
	}
	ended := agent.ForeignTurnInfo{ID: "interject-fallback-1", Running: false}
	stub.SetForeignTurn(ended)
	m = feed(t, m, agent.Event{Type: agent.EventForeignTurn, ForeignTurn: &ended})
	if m.strong != nil || m.turnSeq != seq+1 {
		t.Fatalf("the armed send fires when the foreign turn ends: strong=%v turnSeq=%d", m.strong != nil, m.turnSeq)
	}
	if got := texts(m, entryUser); len(got) != 2 || got[1] != "PINEAPPLE" {
		t.Fatalf("user entries %q", got)
	}
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
	m, _ := queueWorking(t)
	m.input.SetValue("PINEAPPLE  ")
	tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlL})
	m = tm.(Model)
	tm, _ = m.Update(enter())
	m = tm.(Model)
	m = feed(t, m, agent.Event{Type: agent.EventDone, StopReason: stopCancelled})
	tm, _ = m.Update(promptDoneMsg{res: agent.Result{StopReason: stopCancelled}})
	m = tm.(Model)
	if m.input.Value() != "" {
		t.Fatalf("the sent draft must leave the composer: %q", m.input.Value())
	}
	if got := texts(m, entryUser); len(got) != 2 || got[1] != "PINEAPPLE" {
		t.Fatalf("user entries %q", got)
	}
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
	first := m.snap.Queue[0].ID
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
	m, stub := queueWorking(t)
	stub.SetProvider(agent.GrokProvider())
	foreign := agent.ForeignTurnInfo{ID: "interject-fallback-1", Text: "note", Running: true}
	stub.SetForeignTurn(foreign)
	m = feed(t, m, agent.Event{Type: agent.EventForeignTurn, ForeignTurn: &foreign})
	m = feed(t, m, agent.Event{Type: agent.EventDone, StopReason: "end_turn"})
	tm, _ := m.Update(promptDoneMsg{res: agent.Result{StopReason: "end_turn"}})
	m = tm.(Model)
	if m.status != statusIdle || !m.snap.ForeignTurn {
		t.Fatalf("setup: idle under a foreign turn, got %s foreign=%v", m.status, m.snap.ForeignTurn)
	}
	seq := m.turnSeq
	m = typeEnter(t, m, "PINEAPPLE")
	if m.turnSeq != seq || len(m.snap.Queue) != 1 {
		t.Fatalf("enter under a foreign turn queues: turnSeq %d queue %d", m.turnSeq, len(m.snap.Queue))
	}
	ended := agent.ForeignTurnInfo{ID: "interject-fallback-1", Running: false}
	stub.SetForeignTurn(ended)
	m = feed(t, m, agent.Event{Type: agent.EventForeignTurn, ForeignTurn: &ended})
	if m.turnSeq != seq+1 || len(m.snap.Queue) != 0 {
		t.Fatalf("the queue drains when the foreign turn ends: turnSeq %d queue %d", m.turnSeq, len(m.snap.Queue))
	}
}
