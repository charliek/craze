package tui

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/charliek/craze/internal/agent"
)

// queueWorking is a model with a hung turn, so Enter queues.
func queueWorking(t *testing.T) (Model, *Stub) {
	t.Helper()
	isolateSkillsHome(t)
	stub := NewStub()
	stub.HangNext()
	m := startStub(t, stub, t.TempDir(), 80, 24)
	m.input.SetValue("go")
	tm, _ := m.Update(enter())
	m = tm.(Model)
	if m.status != statusWorking {
		t.Fatal("want working")
	}
	return m, stub
}

// queueWorkingLive is queueWorking with the prompt actually running, so the
// session is in a turn and not only the status. Interject is decided from
// that; tests that hand-feed a turn's endings use queueWorking instead,
// because a hung prompt has not returned and its queue is rightly still held.
func queueWorkingLive(t *testing.T) (Model, *Stub) {
	t.Helper()
	isolateSkillsHome(t)
	stub := NewStub()
	stub.HangNext()
	m := startStub(t, stub, t.TempDir(), 80, 24)
	m.input.SetValue("go")
	tm, cmd := m.Update(enter())
	m = tm.(Model)
	go runCmd(cmd)
	t.Cleanup(func() { _ = stub.Close() })
	deadline := time.Now().Add(3 * time.Second)
	for {
		stub.mu.Lock()
		in := stub.inPrompt
		stub.mu.Unlock()
		if in {
			return m, stub
		}
		if time.Now().After(deadline) {
			t.Fatal("the stub never entered its turn")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// typeInto sets the draft and presses Enter.
func typeEnter(t *testing.T, m Model, text string) Model {
	t.Helper()
	m.input.SetValue(text)
	tm, _ := m.Update(enter())
	return tm.(Model)
}

func queueTexts(m Model) []string {
	out := make([]string, 0, len(m.snap.Queue))
	for _, p := range m.snap.Queue {
		out = append(out, p.Text)
	}
	return out
}

func TestEnterQueuesDuringATurnAndClearsTheDraft(t *testing.T) {
	m, _ := queueWorking(t)
	m = typeEnter(t, m, "PINEAPPLE")
	if got := queueTexts(m); len(got) != 1 || got[0] != "PINEAPPLE" {
		t.Fatalf("queue %q", got)
	}
	if m.input.Value() != "" {
		t.Fatalf("the draft must be cleared: %q", m.input.Value())
	}
	if got := texts(m, entryUser); len(got) != 1 {
		t.Fatalf("a queued message is not in the transcript yet: %q", got)
	}
	if m.status != statusWorking {
		t.Fatalf("queueing does not end the turn: %s", m.status)
	}
	if !strings.Contains(plainView(m), "#1 PINEAPPLE") {
		t.Fatalf("the row is missing:\n%s", plainView(m))
	}
	if !strings.Contains(plainView(m), "⧗ 1 queued") {
		t.Fatalf("the count is missing:\n%s", plainView(m))
	}
}

func TestEnterOnAnIdleSessionStillSends(t *testing.T) {
	m := sized(t)
	m = typeEnter(t, m, "hello")
	if len(m.snap.Queue) != 0 {
		t.Fatalf("an idle Enter sends rather than queues: %+v", m.snap.Queue)
	}
	if got := texts(m, entryUser); len(got) != 1 || got[0] != "hello" {
		t.Fatalf("user entries %q", got)
	}
}

func TestQueueFullAndTooLongKeepTheDraft(t *testing.T) {
	m, _ := queueWorking(t)
	for i := 0; i < 32; i++ {
		m = typeEnter(t, m, fmt.Sprintf("row %d", i))
	}
	m.input.SetValue("one too many")
	tm, _ := m.Update(enter())
	m = tm.(Model)
	if m.input.Value() != "one too many" {
		t.Fatalf("a refused message keeps the draft: %q", m.input.Value())
	}
	if len(m.snap.Queue) != 32 {
		t.Fatalf("queue length %d", len(m.snap.Queue))
	}
	if !strings.Contains(plainView(m), "queue full") {
		t.Fatalf("the note is missing:\n%s", plainView(m))
	}

	m2, _ := queueWorking(t)
	m2.input.SetValue(strings.Repeat("x", 32<<10+1))
	tm, _ = m2.Update(enter())
	m2 = tm.(Model)
	if len(m2.input.Value()) != 32<<10+1 {
		t.Fatal("an oversized message keeps the draft")
	}
	if len(m2.snap.Queue) != 0 {
		t.Fatal("an oversized message is not queued")
	}
	if !strings.Contains(plainView(m2), "message too long") {
		t.Fatalf("the note is missing:\n%s", plainView(m2))
	}
}

// TestFocusMatrix walks §3.4's six cases: which band ↑ and ↓ reach from the
// composer, given what is on screen.
func TestFocusMatrix(t *testing.T) {
	subs := []agent.SubagentInfo{{ID: "sub-1", Description: "one", Status: agent.SubagentRunning}}
	for _, tc := range []struct {
		name   string
		key    tea.KeyType
		queued bool
		agents bool
		want   string // "queue", "rows" or "composer"
	}{
		{"up with a queue", tea.KeyUp, true, false, "queue"},
		{"up with a queue and rows", tea.KeyUp, true, true, "queue"},
		{"up with rows only", tea.KeyUp, false, true, "rows"},
		{"up with neither", tea.KeyUp, false, false, "composer"},
		{"down with rows", tea.KeyDown, true, true, "rows"},
		{"down with a queue only", tea.KeyDown, true, false, "queue"},
		{"down with neither", tea.KeyDown, false, false, "composer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, stub := queueWorking(t)
			if tc.agents {
				stub.SetSubagents(subs)
				stub.SetProvider(agent.GrokProvider())
			}
			if tc.queued {
				m = typeEnter(t, m, "PINEAPPLE")
			}
			tm, _ := m.Update(refreshSnapMsg{})
			m = tm.(Model)
			tm, _ = m.Update(tea.KeyMsg{Type: tc.key})
			m = tm.(Model)
			got := "composer"
			switch {
			case m.queueFocus:
				got = "queue"
			case m.agentFocus:
				got = "rows"
			}
			if got != tc.want {
				t.Fatalf("focus %q, want %q", got, tc.want)
			}
		})
	}
}

// TestQueueBandAndRowsCross: ↓ past the last queued row reaches the sub-agent
// rows, and ↑ past the first sub-agent row comes back to the band.
func TestQueueBandAndRowsCross(t *testing.T) {
	m, stub := queueWorking(t)
	stub.SetSubagents([]agent.SubagentInfo{{ID: "sub-1", Description: "one", Status: agent.SubagentRunning}})
	stub.SetProvider(agent.GrokProvider())
	m = typeEnter(t, m, "PINEAPPLE")
	tm, _ := m.Update(refreshSnapMsg{})
	m = tm.(Model)

	tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyUp})
	m = tm.(Model)
	if !m.queueFocus {
		t.Fatal("↑ reaches the band")
	}
	tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = tm.(Model)
	if m.queueFocus || !m.agentFocus {
		t.Fatal("↓ past the last row reaches the sub-agent rows")
	}
	tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyUp})
	m = tm.(Model)
	if !m.queueFocus || m.agentFocus {
		t.Fatal("↑ past the first sub-agent row comes back to the band")
	}
	tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyUp})
	m = tm.(Model)
	if m.queueFocus {
		t.Fatal("↑ past the first queued row returns to the composer")
	}
}

func TestBackspaceRemovesAndClampsTheSelection(t *testing.T) {
	m, _ := queueWorking(t)
	m = typeEnter(t, m, "one")
	m = typeEnter(t, m, "two")
	tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyUp})
	m = tm.(Model)
	if got := m.queueID; got == "" {
		t.Fatal("nothing selected")
	}
	tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyBackspace})
	m = tm.(Model)
	if got := queueTexts(m); len(got) != 1 || got[0] != "one" {
		t.Fatalf("queue %q", got)
	}
	if m.queueSel != 0 {
		t.Fatalf("the selection is clamped: %d", m.queueSel)
	}
	tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyBackspace})
	m = tm.(Model)
	if len(m.snap.Queue) != 0 {
		t.Fatalf("queue %+v", m.snap.Queue)
	}
	if m.queueFocus {
		t.Fatal("an emptied band returns the keyboard to the composer")
	}
}

func TestEnterEditsInPlaceAndEscRestores(t *testing.T) {
	m, _ := queueWorking(t)
	m = typeEnter(t, m, "one")
	m = typeEnter(t, m, "two")
	before := m.snap.Queue[1]
	m.input.SetValue("a draft in progress")
	tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyUp})
	m = tm.(Model)
	tm, _ = m.Update(enter())
	m = tm.(Model)
	if m.queueEdit != before.ID {
		t.Fatalf("edit mode is on %q, want %q", m.queueEdit, before.ID)
	}
	if m.input.Value() != "two" {
		t.Fatalf("the row's text loads into the composer: %q", m.input.Value())
	}
	if !strings.Contains(plainView(m), "editing #2") {
		t.Fatalf("the chip is missing:\n%s", plainView(m))
	}
	m.input.SetValue("TWO!")
	tm, _ = m.Update(enter())
	m = tm.(Model)
	got := m.snap.Queue
	if len(got) != 2 || got[1].ID != before.ID || got[1].Text != "TWO!" {
		t.Fatalf("the row keeps its id and position: %+v", got)
	}
	if got[1].Version != 1 {
		t.Fatalf("version %d", got[1].Version)
	}
	if m.input.Value() != "a draft in progress" {
		t.Fatalf("the draft comes back: %q", m.input.Value())
	}

	// Esc leaves the row untouched and puts the draft back.
	tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyUp})
	m = tm.(Model)
	tm, _ = m.Update(enter())
	m = tm.(Model)
	m.input.SetValue("not saved")
	tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = tm.(Model)
	if m.queueEdit != "" {
		t.Fatal("esc leaves edit mode")
	}
	if m.snap.Queue[1].Text != "TWO!" {
		t.Fatalf("the row is untouched: %+v", m.snap.Queue)
	}
	if m.input.Value() != "a draft in progress" {
		t.Fatalf("the draft comes back: %q", m.input.Value())
	}
}

func TestCtrlLInterjectsOnGrok(t *testing.T) {
	m, stub := queueWorkingLive(t)
	stub.SetProvider(agent.GrokProvider())
	tm, _ := m.Update(refreshSnapMsg{})
	m = tm.(Model)
	m.input.SetValue("BANANA")
	tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlL})
	m = tm.(Model)
	if m.status != statusWorking {
		t.Fatalf("an interjection does not change the status: %s", m.status)
	}
	if m.confirm != nil {
		t.Fatal("an interjection is never confirmed")
	}
	if m.input.Value() != "" {
		t.Fatalf("the draft went: %q", m.input.Value())
	}
	// The transcript entry comes from the broadcast, not from the send.
	m = feed(t, m, agent.Event{Type: agent.EventUser, Text: "BANANA", Interjection: true})
	rows := strings.Join(m.main.entries[len(m.main.entries)-1].rendered, "")
	if got := texts(m, entryUser); len(got) != 2 || got[1] != "BANANA" {
		t.Fatalf("user entries %q", got)
	}
	if !strings.Contains(plain(rows), "↳ BANANA") {
		t.Fatalf("the interjection is marked: %q", plain(rows))
	}
}

func TestCtrlLRefusedAfterTheTurnIsDone(t *testing.T) {
	// The turn is running as far as the status goes, but the session says it
	// has nothing left to merge into — which is exactly the state that makes
	// grok mint a turn of its own instead.
	m, stub := queueWorking(t)
	stub.SetProvider(agent.GrokProvider())
	tm, _ := m.Update(refreshSnapMsg{})
	m = tm.(Model)
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

// TestCtrlLRefusedWhenTheAckFails: a refused ack keeps the text too.
func TestCtrlLRefusedWhenTheAckFails(t *testing.T) {
	m, stub := queueWorkingLive(t)
	stub.SetProvider(agent.GrokProvider())
	stub.FailNextInterject(errors.New("interject not queued"))
	tm, _ := m.Update(refreshSnapMsg{})
	m = tm.(Model)
	m.input.SetValue("BANANA")
	tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlL})
	m = tm.(Model)
	if m.input.Value() != "BANANA" {
		t.Fatalf("a failed interjection keeps the draft: %q", m.input.Value())
	}
	if !strings.Contains(plainView(m), "interject failed") {
		t.Fatalf("the note is missing:\n%s", plainView(m))
	}
	if m.status != statusWorking {
		t.Fatalf("the turn is untouched: %s", m.status)
	}
}

func TestCtrlLOnCursorConfirmsThenSendsAfterSettle(t *testing.T) {
	m, _ := queueWorking(t)
	m.input.SetValue("PINEAPPLE")
	tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlL})
	m = tm.(Model)
	if m.confirm == nil {
		t.Fatal("cursor asks first")
	}
	if m.lay.ComposerRows != 1 {
		t.Fatalf("the confirm is one line: %d", m.lay.ComposerRows)
	}
	if !strings.Contains(plainView(m), confirmLine) {
		t.Fatalf("the confirm is missing:\n%s", plainView(m))
	}

	// Esc declines and loses nothing.
	tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = tm.(Model)
	if m.confirm != nil {
		t.Fatal("esc declines")
	}
	if m.input.Value() != "PINEAPPLE" {
		t.Fatalf("the draft is untouched: %q", m.input.Value())
	}
	if m.status != statusWorking {
		t.Fatalf("declining cancels nothing: %s", m.status)
	}

	// Enter confirms: the turn is cancelled and nothing is sent yet.
	tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlL})
	m = tm.(Model)
	seq := m.turnSeq
	tm, cmd := m.Update(enter())
	m = tm.(Model)
	if m.strong == nil {
		t.Fatal("the send is armed")
	}
	if cmd == nil {
		t.Fatal("the cancel runs")
	}
	if m.turnSeq != seq {
		t.Fatalf("nothing may be prompted before the cancelled turn settles: turnSeq %d", m.turnSeq)
	}
	if got := texts(m, entryUser); len(got) != 1 {
		t.Fatalf("no user entry until it is sent: %q", got)
	}
	// Both endings land; the armed send is what starts next.
	m = feed(t, m, agent.Event{Type: agent.EventDone, StopReason: stopCancelled})
	tm, _ = m.Update(promptDoneMsg{res: agent.Result{StopReason: stopCancelled}})
	m = tm.(Model)
	if m.strong != nil {
		t.Fatal("the armed send fired")
	}
	if m.turnSeq != seq+1 {
		t.Fatalf("exactly one turn started: turnSeq %d, was %d", m.turnSeq, seq)
	}
	if got := texts(m, entryUser); len(got) != 2 || got[1] != "PINEAPPLE" {
		t.Fatalf("user entries %q", got)
	}
}

func TestOnlyOneStrongSendIsPending(t *testing.T) {
	m, _ := queueWorking(t)
	m.input.SetValue("PINEAPPLE")
	tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlL})
	m = tm.(Model)
	tm, _ = m.Update(enter())
	m = tm.(Model)
	if m.strong == nil {
		t.Fatal("armed")
	}
	m.input.SetValue("MANGO")
	tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlL})
	m = tm.(Model)
	if m.confirm != nil {
		t.Fatal("a second send now is refused, not queued")
	}
	if !strings.Contains(plainView(m), "send now already pending") {
		t.Fatalf("the note is missing:\n%s", plainView(m))
	}
}

func TestEscWhilePendingDropsItAndRestoresTheText(t *testing.T) {
	m, _ := queueWorking(t)
	m.input.SetValue("PINEAPPLE")
	tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlL})
	m = tm.(Model)
	tm, _ = m.Update(enter())
	m = tm.(Model)
	tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = tm.(Model)
	if m.strong != nil {
		t.Fatal("esc drops the pending send")
	}
	if m.input.Value() != "PINEAPPLE" {
		t.Fatalf("the text comes back: %q", m.input.Value())
	}
}

func TestCancelFailedDropsThePendingSend(t *testing.T) {
	m, _ := queueWorking(t)
	m.input.SetValue("PINEAPPLE")
	tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlL})
	m = tm.(Model)
	tm, _ = m.Update(enter())
	m = tm.(Model)
	tm, _ = m.Update(cancelFailedMsg{seq: m.turnSeq, err: errors.New("nope")})
	m = tm.(Model)
	if m.strong != nil {
		t.Fatal("a failed cancel drops the armed send")
	}
	if m.input.Value() != "PINEAPPLE" {
		t.Fatalf("the text comes back: %q", m.input.Value())
	}
	if !strings.Contains(plainView(m), "cancel failed") {
		t.Fatalf("the note is missing:\n%s", plainView(m))
	}
}

func TestCardHidesTheConfirm(t *testing.T) {
	m, stub := queueWorking(t)
	m.input.SetValue("PINEAPPLE")
	tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlL})
	m = tm.(Model)
	if m.confirm == nil {
		t.Fatal("the confirm is up")
	}
	_ = stub
	m = feed(t, m, agent.Event{Type: agent.EventPermission, Permission: &agent.PermissionEvent{
		ID: "p1", Tool: "bash", Options: []agent.PermissionOption{{OptionID: "a", Name: "Allow", Kind: "allow_once"}},
	}})
	if !m.cardOpen() {
		t.Fatal("the card is up")
	}
	if m.confirm != nil {
		t.Fatal("a card owns the keyboard, so the confirm is discarded")
	}
	if m.input.Value() != "PINEAPPLE" {
		t.Fatalf("the draft comes back: %q", m.input.Value())
	}
}

func TestCtrlCClearsEverythingPendingThenCancels(t *testing.T) {
	m, _ := queueWorking(t)
	m = typeEnter(t, m, "one")
	m = typeEnter(t, m, "two")
	m.input.SetValue("PINEAPPLE")
	tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlL})
	m = tm.(Model)
	tm, _ = m.Update(enter())
	m = tm.(Model)
	if m.strong == nil || len(m.snap.Queue) != 2 {
		t.Fatalf("set up: strong %v queue %+v", m.strong, m.snap.Queue)
	}
	tm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	m = tm.(Model)
	if cmd == nil {
		t.Fatal("ctrl+c cancels")
	}
	if len(m.snap.Queue) != 0 {
		t.Fatalf("the queue is cleared: %+v", m.snap.Queue)
	}
	if m.strong != nil || m.confirm != nil {
		t.Fatal("the confirm and the armed send go too")
	}
}

func TestEscLetsTheQueueContinue(t *testing.T) {
	m, _ := queueWorking(t)
	m = typeEnter(t, m, "PINEAPPLE")
	tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = tm.(Model)
	if len(m.snap.Queue) != 1 {
		t.Fatalf("esc does not touch the queue: %+v", m.snap.Queue)
	}
	seq := m.turnSeq
	m = feed(t, m, agent.Event{Type: agent.EventDone, StopReason: stopCancelled})
	tm, _ = m.Update(promptDoneMsg{res: agent.Result{StopReason: stopCancelled}})
	m = tm.(Model)
	if m.turnSeq != seq+1 {
		t.Fatalf("the head runs once the cancelled turn settles: turnSeq %d", m.turnSeq)
	}
	if got := texts(m, entryUser); len(got) != 2 || got[1] != "PINEAPPLE" {
		t.Fatalf("user entries %q", got)
	}
	if len(m.snap.Queue) != 0 {
		t.Fatalf("the row left the queue: %+v", m.snap.Queue)
	}
}

func TestClearEmptiesEverything(t *testing.T) {
	m, _ := queueWorking(t)
	m = typeEnter(t, m, "one")
	m.input.SetValue("/clear")
	tm, _ := m.Update(enter())
	m = tm.(Model)
	if len(m.snap.Queue) != 0 {
		t.Fatalf("/clear empties the queue: %+v", m.snap.Queue)
	}
}

// TestFinishTurnBothEndingOrders is the drain in every shape §3.5 names.
func TestFinishTurnBothEndingOrders(t *testing.T) {
	for _, stop := range []string{"end_turn", stopCancelled} {
		for _, doneFirst := range []bool{true, false} {
			name := fmt.Sprintf("%s/done-first=%v", stop, doneFirst)
			t.Run(name, func(t *testing.T) {
				m, _ := queueWorking(t)
				m = typeEnter(t, m, "PINEAPPLE")
				seq := m.turnSeq
				done := agent.Event{Type: agent.EventDone, StopReason: stop}
				if doneFirst {
					m = feed(t, m, done)
					if m.status != statusWorking {
						t.Fatalf("one ending is not the turn: %s", m.status)
					}
					tm, _ := m.Update(promptDoneMsg{res: agent.Result{StopReason: stop}})
					m = tm.(Model)
				} else {
					tm, _ := m.Update(promptDoneMsg{res: agent.Result{StopReason: stop}})
					m = tm.(Model)
					if m.status != statusWorking {
						t.Fatalf("one ending is not the turn: %s", m.status)
					}
					m = feed(t, m, done)
				}
				if m.turnSeq != seq+1 {
					t.Fatalf("exactly one turn started: turnSeq %d", m.turnSeq)
				}
				if len(m.snap.Queue) != 0 {
					t.Fatalf("the head went: %+v", m.snap.Queue)
				}
			})
		}
	}
}

func TestErroredTurnDrainsNothing(t *testing.T) {
	m, _ := queueWorking(t)
	m = typeEnter(t, m, "PINEAPPLE")
	seq := m.turnSeq
	m = feed(t, m, agent.Event{Type: agent.EventError, Err: errors.New("boom")})
	tm, _ := m.Update(promptDoneMsg{res: agent.Result{}, err: errors.New("boom")})
	m = tm.(Model)
	if m.status != statusError {
		t.Fatalf("status %s", m.status)
	}
	if m.turnSeq != seq {
		t.Fatalf("nothing drains from an error state: turnSeq %d", m.turnSeq)
	}
}

func TestForeignTurnHoldsTheDrainAndNotesItself(t *testing.T) {
	m, stub := queueWorking(t)
	stub.SetProvider(agent.GrokProvider())
	m = typeEnter(t, m, "PINEAPPLE")
	seq := m.turnSeq
	stub.SetForeignTurn(agent.ForeignTurnInfo{ID: "interject-fallback-1", Text: "note", Running: true})
	m = feed(t, m, agent.Event{Type: agent.EventForeignTurn, ForeignTurn: &agent.ForeignTurnInfo{
		ID: "interject-fallback-1", Text: "note", Running: true,
	}})
	m = feed(t, m, agent.Event{Type: agent.EventDone, StopReason: "end_turn"})
	tm, _ := m.Update(promptDoneMsg{res: agent.Result{StopReason: "end_turn"}})
	m = tm.(Model)
	if m.turnSeq != seq {
		t.Fatalf("the drain waits out the foreign turn: turnSeq %d", m.turnSeq)
	}
	if !strings.Contains(plainView(m), foreignTurnNote) {
		t.Fatalf("the note is missing:\n%s", plainView(m))
	}
	stub.SetForeignTurn(agent.ForeignTurnInfo{ID: "interject-fallback-1", Running: false})
	m = feed(t, m, agent.Event{Type: agent.EventForeignTurn, ForeignTurn: &agent.ForeignTurnInfo{
		ID: "interject-fallback-1", Running: false,
	}})
	if m.turnSeq != seq+1 {
		t.Fatalf("the drain re-runs when the foreign turn ends: turnSeq %d", m.turnSeq)
	}
	if got := texts(m, entryUser); len(got) != 2 || got[1] != "PINEAPPLE" {
		t.Fatalf("user entries %q", got)
	}
}

// TestHoverNeedsAButtonlessMotion: <motion:> holds the left button, which is a
// drag; only a hover shows the actions.
func TestHoverOnlyOnButtonlessMotion(t *testing.T) {
	m, _ := queueWorking(t)
	m = typeEnter(t, m, "PINEAPPLE")
	band := m.lay.Region(regionQueue)
	if band.Empty() {
		t.Fatal("the band is drawn")
	}
	tm, _ := m.Update(tea.MouseMsg{X: 4, Y: band.Top, Button: tea.MouseButtonLeft, Action: tea.MouseActionMotion})
	m = tm.(Model)
	if m.queueHov.row != -1 {
		t.Fatalf("a held button is a drag, not a hover: %+v", m.queueHov)
	}
	tm, _ = m.Update(tea.MouseMsg{X: 4, Y: band.Top, Button: tea.MouseButtonNone, Action: tea.MouseActionMotion})
	m = tm.(Model)
	if m.queueHov.row != 0 {
		t.Fatalf("hover %+v", m.queueHov)
	}
	if !strings.Contains(plainView(m), "[send now] [edit] [cancel]") {
		t.Fatalf("the actions are missing:\n%s", plainView(m))
	}
	// Off the band again.
	tm, _ = m.Update(tea.MouseMsg{X: 4, Y: 0, Button: tea.MouseButtonNone, Action: tea.MouseActionMotion})
	m = tm.(Model)
	if m.queueHov.row != -1 {
		t.Fatalf("hover %+v", m.queueHov)
	}
}

func TestHoverChangesNoFrameSizes(t *testing.T) {
	m, _ := queueWorking(t)
	m = typeEnter(t, m, "PINEAPPLE")
	before := m.lay
	tm, _ := m.Update(tea.MouseMsg{X: 4, Y: before.Region(regionQueue).Top, Button: tea.MouseButtonNone, Action: tea.MouseActionMotion})
	m = tm.(Model)
	if m.lay.regions != before.regions {
		t.Fatalf("a hover relaid the frame:\n%+v\n%+v", before.regions, m.lay.regions)
	}
	if m.lay.ComposerRows != before.ComposerRows || m.lay.QueueRows != before.QueueRows {
		t.Fatal("a hover changed the sizes")
	}
}

func TestQueueActionClicks(t *testing.T) {
	sendNowX := 80 - queueActionStripWidth() + 2
	editX := 80 - queueActionStripWidth() + len("[send now] ") + 2
	cancelX := 80 - len("[cancel]") + 2

	// A real pointer moves before it presses, and the strip is only on the
	// row it is over: hovering first is what a mouse does, and what makes the
	// buttons exist to be clicked.
	hoverRow := func(t *testing.T, m Model, row int) Model {
		t.Helper()
		y := m.lay.Region(regionQueue).Top + row
		tm, _ := m.Update(tea.MouseMsg{X: 4, Y: y, Button: tea.MouseButtonNone, Action: tea.MouseActionMotion})
		return tm.(Model)
	}
	press := func(t *testing.T, m Model, x, row int) Model {
		t.Helper()
		y := m.lay.Region(regionQueue).Top + row
		tm, _ := m.Update(tea.MouseMsg{X: x, Y: y, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
		return tm.(Model)
	}

	t.Run("cancel", func(t *testing.T) {
		m, _ := queueWorking(t)
		m = typeEnter(t, m, "PINEAPPLE")
		m = press(t, hoverRow(t, m, 0), cancelX, 0)
		if len(m.snap.Queue) != 0 {
			t.Fatalf("[cancel] removes the row: %+v", m.snap.Queue)
		}
	})
	t.Run("edit", func(t *testing.T) {
		m, _ := queueWorking(t)
		m = typeEnter(t, m, "PINEAPPLE")
		m = press(t, hoverRow(t, m, 0), editX, 0)
		if m.queueEdit == "" || m.input.Value() != "PINEAPPLE" {
			t.Fatalf("[edit] loads the row: edit %q draft %q", m.queueEdit, m.input.Value())
		}
	})
	t.Run("send now", func(t *testing.T) {
		m, _ := queueWorking(t)
		m = typeEnter(t, m, "PINEAPPLE")
		m = press(t, hoverRow(t, m, 0), sendNowX, 0)
		if m.confirm == nil {
			t.Fatal("[send now] asks first")
		}
		if len(m.snap.Queue) != 1 {
			t.Fatalf("the row stays until it actually goes: %+v", m.snap.Queue)
		}
	})
	t.Run("row text selects", func(t *testing.T) {
		m, _ := queueWorking(t)
		m = typeEnter(t, m, "one")
		m = typeEnter(t, m, "two")
		m = press(t, m, 4, 1)
		if !m.queueFocus || m.queueSel != 1 {
			t.Fatalf("a click on the text selects the row: focus %v sel %d", m.queueFocus, m.queueSel)
		}
		if len(m.snap.Queue) != 2 {
			t.Fatalf("nothing else happened: %+v", m.snap.Queue)
		}
	})
	t.Run("a click where no strip was drawn only selects", func(t *testing.T) {
		// The pointer never moved over this row, so it has no buttons: a
		// click at the strip's coordinates must not cancel it.
		m, _ := queueWorking(t)
		m = typeEnter(t, m, "one")
		m = typeEnter(t, m, "two")
		m = press(t, m, cancelX, 1)
		if len(m.snap.Queue) != 2 {
			t.Fatalf("a button that was not drawn cannot be clicked: %+v", m.snap.Queue)
		}
		if !m.queueFocus || m.queueSel != 1 {
			t.Fatalf("the click selects the row instead: focus %v sel %d", m.queueFocus, m.queueSel)
		}
	})
}

// TestMouseModeFollowsTheQueue: all-motion is what hover needs, and neither
// escape disables the other, so every transition goes through DisableMouse.
func TestMouseModeFollowsTheQueue(t *testing.T) {
	m, _ := queueWorking(t)
	m.input.SetValue("PINEAPPLE")
	tm, cmd := m.Update(enter())
	m = tm.(Model)
	if !hasMouseSequence(t, cmd, "all") {
		t.Fatal("0 → 1 rows enables all motion through DisableMouse")
	}
	// Still one row: nothing is issued.
	tm, cmd = m.Update(refreshSnapMsg{})
	m = tm.(Model)
	if hasMouseSequence(t, cmd, "all") || hasMouseSequence(t, cmd, "cell") {
		t.Fatal("no transition, no command")
	}
	tm, cmd = m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	m = tm.(Model)
	if !hasMouseSequence(t, cmd, "cell") {
		t.Fatal("1 → 0 rows restores cell motion through DisableMouse")
	}
}

func TestMouseModeNeverIssuedUnderNoMouse(t *testing.T) {
	isolateSkillsHome(t)
	stub := NewStub()
	stub.HangNext()
	m := New(Config{
		Session:   stub,
		Theme:     "tokyo-night",
		Workspace: t.TempDir(),
		Yolo:      true,
		NoMouse:   true,
	})
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = tm.(Model)
	tm, _ = m.Update(startedMsg{})
	m = tm.(Model)
	m.input.SetValue("go")
	tm, _ = m.Update(enter())
	m = tm.(Model)
	m.input.SetValue("PINEAPPLE")
	tm, cmd := m.Update(enter())
	m = tm.(Model)
	if len(m.snap.Queue) != 1 {
		t.Fatalf("the queue still works: %+v", m.snap.Queue)
	}
	if hasMouseSequence(t, cmd, "all") || hasMouseSequence(t, cmd, "cell") {
		t.Fatal("--no-mouse asked the terminal for no reporting; craze must not start now")
	}
}

// hasMouseSequence walks a batched command for the motion-mode transition.
// bubbletea's own mode messages are unexported types, so the test identifies
// them by the escape sequence they carry — which is what the terminal sees,
// and what the PTY test asserts on the other side.
func hasMouseSequence(t *testing.T, cmd tea.Cmd, want string) bool {
	t.Helper()
	var disable, mode bool
	var walk func(tea.Cmd, int)
	walk = func(c tea.Cmd, depth int) {
		if c == nil || depth > 4 {
			return
		}
		msg := c()
		if batch, ok := msg.(tea.BatchMsg); ok {
			for _, sub := range batch {
				walk(sub, depth+1)
			}
			return
		}
		// tea.Sequence returns an unexported []tea.Cmd type; reflection is
		// the only way in, and it is the same shape a batch has.
		if v := reflect.ValueOf(msg); v.Kind() == reflect.Slice {
			for i := 0; i < v.Len(); i++ {
				if sub, ok := v.Index(i).Interface().(tea.Cmd); ok {
					walk(sub, depth+1)
				}
			}
			return
		}
		name := fmt.Sprintf("%T", msg)
		switch {
		case strings.Contains(name, "disableMouse"):
			disable = true
		case want == "all" && strings.Contains(name, "enableMouseAllMotion"):
			mode = true
		case want == "cell" && strings.Contains(name, "enableMouseCellMotion"):
			mode = true
		}
	}
	walk(cmd, 0)
	if mode && !disable {
		t.Fatal("a mode change must go through DisableMouse: neither escape disables the other")
	}
	return mode && disable
}

// TestEmptyQueueChangesNoFrameSizes is the compatibility contract: the new
// region and the new degradation step must be invisible until something is
// queued, at every step, or every pre-existing golden would have moved.
func TestEmptyQueueChangesNoFrameSizes(t *testing.T) {
	base := frameSizes{
		tasksOpen: true,
		tasksBody: 3,
		input:     4,
		agentsAll: 5,
		agentsCap: agentRowsMax,
		modal:     6,
		spinner:   1,
		status:    statusRows,
	}
	// The degradation order with the queue step in it, for an empty queue.
	for n := 0; n <= maxDegrade; n++ {
		got := degrade(base, n)
		if got.queue() != 0 {
			t.Fatalf("step %d drew %d queue rows for an empty queue", n, got.queue())
		}
		// Every other size is what the pre-queue order produced: the queue
		// step sits between "agent rows to 0" and "tasks closed", so the
		// steps before it are unchanged and the ones after it shift by one.
		want := degradeWithoutQueue(base, n)
		if got.chrome() != want.chrome() {
			t.Fatalf("step %d: chrome %d, want %d (%+v vs %+v)", n, got.chrome(), want.chrome(), got, want)
		}
	}
}

// degradeWithoutQueue is the pre-008 degradation order, spelled out so the
// test above compares against something other than the code it is checking.
func degradeWithoutQueue(s frameSizes, n int) frameSizes {
	if n >= 1 && s.agentsCap > agentRowsShort {
		s.agentsCap = agentRowsShort
	}
	if n >= 2 {
		s.tasksBody = 0
	}
	if n >= 3 && s.input > composerShortRows {
		s.input = composerShortRows
	}
	if n >= 4 {
		s.agentsCap = 0
	}
	if n >= 6 {
		s.tasksOpen = false
	}
	if n >= 7 && s.spinner > 0 {
		s.spinner, s.merged = 0, true
	}
	if n >= 8 && s.modal > 1 {
		s.modal = 1
	}
	return s
}

// TestFitChromeGivesUpTheQueueBeforeTheTasksBody is the backstop's order: the
// band is chrome the user can still read in status row 2, so it goes early.
func TestFitChromeGivesUpTheQueueBeforeTheTasksBody(t *testing.T) {
	s := frameSizes{tasksOpen: true, tasksBody: 3, input: 1, queueAll: 3, queueCap: queueRowsMax, status: statusRows}
	got := fitChrome(s, 6)
	if got.queueCap != 0 {
		t.Fatalf("the band should have gone: %+v", got)
	}
	if got.chrome() > 6 {
		t.Fatalf("chrome %d still over the limit: %+v", got.chrome(), got)
	}
}

// TestQueueFrameHeightInEveryState holds the height contract with the band on
// screen, selected, hovered, confirming and being edited.
func TestQueueFrameHeightInEveryState(t *testing.T) {
	for _, size := range []struct{ cols, rows int }{{40, 12}, {80, 24}, {100, 30}} {
		for _, state := range []string{"rows", "selected", "hover", "edit", "confirm"} {
			t.Run(fmt.Sprintf("%dx%d/%s", size.cols, size.rows, state), func(t *testing.T) {
				m, _ := queueWorking(t)
				tm, _ := m.Update(tea.WindowSizeMsg{Width: size.cols, Height: size.rows})
				m = tm.(Model)
				m = typeEnter(t, m, "one")
				m = typeEnter(t, m, "two")
				switch state {
				case "selected":
					tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyUp})
					m = tm.(Model)
				case "hover":
					band := m.lay.Region(regionQueue)
					tm, _ = m.Update(tea.MouseMsg{X: 4, Y: band.Top, Button: tea.MouseButtonNone, Action: tea.MouseActionMotion})
					m = tm.(Model)
				case "edit":
					tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyUp})
					m = tm.(Model)
					tm, _ = m.Update(enter())
					m = tm.(Model)
				case "confirm":
					m.input.SetValue("PINEAPPLE")
					tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlL})
					m = tm.(Model)
				}
				view := plainView(m)
				if h := lipgloss.Height(view); h != size.rows {
					t.Fatalf("frame is %d rows, want %d:\n%s", h, size.rows, view)
				}
				for _, ln := range strings.Split(view, "\n") {
					if w := lipgloss.Width(ln); w > size.cols {
						t.Fatalf("line is %d wide, max %d: %q", w, size.cols, ln)
					}
				}
			})
		}
	}
}

// TestQueueLaysOutOncePerUpdate: every queue action is one message, and the
// frame is computed once for it.
func TestQueueLaysOutOncePerUpdate(t *testing.T) {
	m, _ := queueWorking(t)
	for _, msg := range []tea.Msg{
		tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")},
		enter(),
		tea.KeyMsg{Type: tea.KeyUp},
		tea.MouseMsg{X: 4, Y: 20, Button: tea.MouseButtonNone, Action: tea.MouseActionMotion},
		tea.KeyMsg{Type: tea.KeyCtrlL},
		tea.KeyMsg{Type: tea.KeyEsc},
	} {
		before := m.layouts
		tm, _ := m.Update(msg)
		m = tm.(Model)
		if got := m.layouts - before; got != 1 {
			t.Fatalf("%T laid out %d times", msg, got)
		}
	}
}

// TestEditedRowSentUnderTheEditor: the drain sends the head when the turn
// settles, and it does not wait for an edit nobody saved. The edit ends rather
// than writing into a row that is gone.
func TestEditedRowSentUnderTheEditor(t *testing.T) {
	m, _ := queueWorking(t)
	m = typeEnter(t, m, "PINEAPPLE")
	tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyUp})
	m = tm.(Model)
	tm, _ = m.Update(enter())
	m = tm.(Model)
	if m.queueEdit == "" {
		t.Fatal("edit mode is on")
	}
	m.input.SetValue("never saved")
	m = feed(t, m, agent.Event{Type: agent.EventDone, StopReason: "end_turn"})
	tm, _ = m.Update(promptDoneMsg{res: agent.Result{StopReason: "end_turn"}})
	m = tm.(Model)
	if m.queueEdit != "" {
		t.Fatal("the edit ends with the row")
	}
	if got := texts(m, entryUser); len(got) != 2 || got[1] != "PINEAPPLE" {
		t.Fatalf("the row is sent as it was queued: %q", got)
	}
	if !strings.Contains(plainView(m), "is gone") {
		t.Fatalf("the note is missing:\n%s", plainView(m))
	}
}

// TestActionStripHitTestMatchesTheDraw walks every column of a selected row
// and checks the button the hit test names is the one drawn there.
func TestActionStripHitTestMatchesTheDraw(t *testing.T) {
	for _, width := range []int{40, 80, 100} {
		t.Run(fmt.Sprintf("%d", width), func(t *testing.T) {
			m, _ := queueWorking(t)
			tm, _ := m.Update(tea.WindowSizeMsg{Width: width, Height: 24})
			m = tm.(Model)
			m = typeEnter(t, m, "PINEAPPLE")
			tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyUp})
			m = tm.(Model)
			row := plain(m.queueRow(0, m.snap.Queue[0], true, false))
			if !strings.Contains(row, "[send now] [edit] [cancel]") {
				t.Fatalf("width %d: no strip drawn: %q", width, row)
			}
			for x := 0; x < width; x++ {
				got := queueActionAt(x, width)
				want := actionNone
				switch {
				case strings.HasPrefix(string([]rune(row)[x:]), "[send now]"):
					want = actionSendNow
				case strings.HasPrefix(string([]rune(row)[x:]), "[edit]"):
					want = actionEdit
				case strings.HasPrefix(string([]rune(row)[x:]), "[cancel]"):
					want = actionCancel
				}
				if want != actionNone && got != want {
					t.Fatalf("width %d col %d: hit test says %d, the row starts %q there", width, x, got, string([]rune(row)[x:x+8]))
				}
			}
		})
	}
}

// TestSendNowOnARowSendsItExactlyOnce is the double-send the row's id exists
// to prevent: the row leaves the queue when it actually goes, not when it is
// confirmed, so the drain behind it cannot send it again.
func TestSendNowOnARowSendsItExactlyOnce(t *testing.T) {
	m, _ := queueWorking(t)
	m = typeEnter(t, m, "PINEAPPLE")
	m = typeEnter(t, m, "MANGO")
	// Select the first row and send it now.
	tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyUp})
	m = tm.(Model)
	tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyUp})
	m = tm.(Model)
	if got, _ := m.queueSelected(); got.Text != "PINEAPPLE" {
		t.Fatalf("selected %+v", got)
	}
	tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlL})
	m = tm.(Model)
	if m.confirm == nil {
		t.Fatal("send now on a row asks first")
	}
	if len(m.snap.Queue) != 2 {
		t.Fatalf("the row stays until it goes: %+v", m.snap.Queue)
	}
	tm, _ = m.Update(enter())
	m = tm.(Model)
	if len(m.snap.Queue) != 2 {
		t.Fatalf("and it is still there while the cancel is in flight: %+v", m.snap.Queue)
	}
	m = feed(t, m, agent.Event{Type: agent.EventDone, StopReason: stopCancelled})
	tm, _ = m.Update(promptDoneMsg{res: agent.Result{StopReason: stopCancelled}})
	m = tm.(Model)
	if got := texts(m, entryUser); len(got) != 2 || got[1] != "PINEAPPLE" {
		t.Fatalf("user entries %q", got)
	}
	if got := queueTexts(m); len(got) != 1 || got[0] != "MANGO" {
		t.Fatalf("exactly one row went: %q", got)
	}
	// The turn that started now settles too; the row behind it drains once.
	m = feed(t, m, agent.Event{Type: agent.EventDone, StopReason: "end_turn"})
	tm, _ = m.Update(promptDoneMsg{res: agent.Result{StopReason: "end_turn"}})
	m = tm.(Model)
	if got := texts(m, entryUser); len(got) != 3 || got[2] != "MANGO" {
		t.Fatalf("user entries %q", got)
	}
	if len(m.snap.Queue) != 0 {
		t.Fatalf("queue %+v", m.snap.Queue)
	}
}

// TestConfirmDroppedWhenTheTurnEndsFirst: the question was about a turn that
// is over, so it is not asked of the next one.
func TestConfirmDroppedWhenTheTurnEndsFirst(t *testing.T) {
	m, _ := queueWorking(t)
	m.input.SetValue("PINEAPPLE")
	tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlL})
	m = tm.(Model)
	if m.confirm == nil {
		t.Fatal("the confirm is up")
	}
	seq := m.turnSeq
	m = feed(t, m, agent.Event{Type: agent.EventDone, StopReason: "end_turn"})
	tm, _ = m.Update(promptDoneMsg{res: agent.Result{StopReason: "end_turn"}})
	m = tm.(Model)
	if m.confirm != nil {
		t.Fatal("the confirm goes with the turn it was about")
	}
	if m.turnSeq != seq {
		t.Fatalf("nothing was sent: turnSeq %d", m.turnSeq)
	}
	if m.input.Value() != "PINEAPPLE" {
		t.Fatalf("the draft is untouched: %q", m.input.Value())
	}
	if !strings.Contains(plainView(m), "the turn ended first") {
		t.Fatalf("the note is missing:\n%s", plainView(m))
	}
}

// TestArmedSendDroppedByAnErroredTurn: nothing drains from an error state, so
// an armed send must not survive to fire behind a later turn.
func TestArmedSendDroppedByAnErroredTurn(t *testing.T) {
	m, _ := queueWorking(t)
	m.input.SetValue("PINEAPPLE")
	tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlL})
	m = tm.(Model)
	tm, _ = m.Update(enter())
	m = tm.(Model)
	if m.strong == nil {
		t.Fatal("armed")
	}
	m = feed(t, m, agent.Event{Type: agent.EventError, Err: errors.New("boom")})
	if m.strong != nil {
		t.Fatal("an errored turn takes the armed send with it")
	}
	// A later turn settles without it firing.
	m.status = statusIdle
	m = typeEnter(t, m, "next")
	seq := m.turnSeq
	m = feed(t, m, agent.Event{Type: agent.EventDone, StopReason: "end_turn"})
	tm, _ = m.Update(promptDoneMsg{res: agent.Result{StopReason: "end_turn"}})
	m = tm.(Model)
	if m.turnSeq != seq {
		t.Fatalf("nothing fired behind the next turn: turnSeq %d", m.turnSeq)
	}
}

// TestEditingASecondRowKeepsTheOriginalDraft: only the first edit displaces a
// draft, so moving between rows cannot overwrite it with a row.
func TestEditingASecondRowKeepsTheOriginalDraft(t *testing.T) {
	m, _ := queueWorking(t)
	m = typeEnter(t, m, "one")
	m = typeEnter(t, m, "two")
	m.input.SetValue("my draft")
	tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyUp})
	m = tm.(Model)
	tm, _ = m.Update(enter())
	m = tm.(Model)
	m.input.SetValue("edited two")
	// Switch to the other row without saving.
	tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyUp})
	m = tm.(Model)
	tm, _ = m.Update(enter())
	m = tm.(Model)
	tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = tm.(Model)
	if m.input.Value() != "my draft" {
		t.Fatalf("the original draft comes back: %q", m.input.Value())
	}
}

// TestEditSurvivesTheBandBeingDegradedAway: an edit belongs to a row, not to
// a band, and a resize is not a decision about it.
func TestEditSurvivesTheBandBeingDegradedAway(t *testing.T) {
	m, _ := queueWorking(t)
	for _, text := range []string{"one", "two", "three", "four"} {
		m = typeEnter(t, m, text)
	}
	tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyUp})
	m = tm.(Model)
	tm, _ = m.Update(enter())
	m = tm.(Model)
	id := m.queueEdit
	if id == "" {
		t.Fatal("edit mode is on")
	}
	tm, _ = m.Update(tea.WindowSizeMsg{Width: 40, Height: 12})
	m = tm.(Model)
	if m.lay.QueueRows != 0 {
		t.Fatalf("the band should be degraded away: %d rows", m.lay.QueueRows)
	}
	if m.queueEdit != id {
		t.Fatalf("the edit survives: %q", m.queueEdit)
	}
	m.input.SetValue("still saved")
	tm, _ = m.Update(enter())
	m = tm.(Model)
	// ↑ selects the newest row the band is drawing, which is #3 of four.
	texts := queueTexts(m)
	if len(texts) != 4 || texts[2] != "still saved" {
		t.Fatalf("the edit saved into its row: %q", texts)
	}
}

// TestErroredTurnSaysTheQueueWasCleared: the session clears the queue after
// it has reported the error, so the status is already error when the
// removals land and the note can say why the band emptied.
func TestErroredTurnSaysTheQueueWasCleared(t *testing.T) {
	m, stub := queueWorking(t)
	m = typeEnter(t, m, "PINEAPPLE")
	m = feed(t, m, agent.Event{Type: agent.EventError, Err: errors.New("boom")})
	if m.status != statusError {
		t.Fatalf("status %s", m.status)
	}
	n := stub.ClearQueue()
	m = feed(t, m, agent.Event{
		Type:        agent.EventQueue,
		Queue:       &agent.QueuedPrompt{ID: "q-1", Text: "PINEAPPLE"},
		QueueChange: agent.QueueRemoved,
	})
	if n != 1 {
		t.Fatalf("the session cleared %d rows", n)
	}
	if !strings.Contains(plainView(m), "queue cleared") {
		t.Fatalf("the note is missing:\n%s", plainView(m))
	}
}

// TestQueueEditChipSurvivesTheNarrowestRule: the rule drops a title it cannot
// fit, so at 40 columns the whole chip would vanish and nothing would say the
// composer is holding a row rather than a draft.
func TestQueueEditChipSurvivesTheNarrowestRule(t *testing.T) {
	m, _ := queueWorking(t)
	tm, _ := m.Update(tea.WindowSizeMsg{Width: minFrameCols, Height: 24})
	m = tm.(Model)
	m = typeEnter(t, m, "PINEAPPLE")
	tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyUp})
	m = tm.(Model)
	tm, _ = m.Update(enter())
	m = tm.(Model)
	if m.queueEdit == "" {
		t.Fatal("edit mode is on")
	}
	view := plainView(m)
	if !strings.Contains(view, "editing") {
		t.Fatalf("the chip is gone at %d columns:\n%s", minFrameCols, view)
	}
	for _, ln := range strings.Split(view, "\n") {
		if w := lipgloss.Width(ln); w > minFrameCols {
			t.Fatalf("line is %d wide: %q", w, ln)
		}
	}
}

// TestStubRefusesAConcurrentPrompt keeps the double honest: the live session
// refuses one, and a stub that did not would let a test pass against turn
// state craze can never reach.
func TestStubRefusesAConcurrentPrompt(t *testing.T) {
	_, stub := queueWorkingLive(t)
	if _, err := stub.Prompt(context.Background(), "second"); !errors.Is(err, agent.ErrPromptInFlight) {
		t.Fatalf("err %v", err)
	}
}
