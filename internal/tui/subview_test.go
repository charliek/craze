package tui

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/charliek/craze/internal/agent"
)

func openView(t *testing.T, m Model) Model {
	t.Helper()
	m.input.SetValue("")
	tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = tm.(Model)
	tm, _ = m.Update(enter())
	m = tm.(Model)
	if m.viewing == "" {
		t.Fatal("expected the sub-agent view")
	}
	return m
}

func TestEnterOpensSubagentView(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	m := agentModel(t, &now)
	m = applyInFlight(t, m, []agent.ToolEvent{taskTool("task-1", "count lines", "in_progress")})
	m.status = statusWorking
	m = openView(t, m)
	if m.viewing != "task-1" {
		t.Fatalf("viewing %q", m.viewing)
	}
	if m.status != statusWorking {
		t.Fatal("entering must not cancel the turn")
	}
	if m.lay.ComposerRows != 1 {
		t.Fatalf("ComposerRows %d, want 1", m.lay.ComposerRows)
	}
	view := plainView(m)
	if !strings.Contains(view, "receipt only") || !strings.Contains(view, "esc to return") {
		t.Fatalf("missing the receipt banner:\n%s", view)
	}
}

// tallChild gives task-1 n one-row entries so its transcript is taller than
// the viewport (a single streamed reply collapses to a few rows).
func tallChild(t *testing.T, m Model, n int) Model {
	t.Helper()
	// Grok: a receipt-only provider would rebuild the view from the receipt.
	m.sess.(*Stub).SetProvider(agent.GrokProvider())
	m.refreshSnap()
	tr := m.ensureSub("task-1")
	for i := 0; i < n; i++ {
		tr.appendEntry(entry{kind: entryAssistant, text: fmt.Sprintf("childline%d", i)}, m.now())
	}
	return m
}

func TestEscAndLeftReturnAndRestoreOffset(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	m := agentModel(t, &now)
	// Two pages up on a 200-line main, so the offset is neither 0 nor the
	// bottom (a one-page scroll on a short transcript clamps to 0 and the
	// restore assertion is vacuous).
	for i := 0; i < 200; i++ {
		m.appendEntry(entry{kind: entryAssistant, text: fmt.Sprintf("line-%03d padding so the transcript is taller than the viewport", i)})
	}
	m.refreshViewport()
	for i := 0; i < 2; i++ {
		tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyPgUp})
		m = tm.(Model)
	}
	if m.vp.AtBottom() || m.vp.YOffset == 0 {
		t.Fatalf("setup: page up should leave the main transcript mid-way, offset %d", m.vp.YOffset)
	}
	offset := m.vp.YOffset
	m = applyInFlight(t, m, []agent.ToolEvent{taskTool("task-1", "count lines", "in_progress")})
	m.status = statusWorking
	// The child is taller than the main, so a stale "stick to bottom" would
	// land somewhere else entirely.
	m = tallChild(t, m, 300)
	m = openView(t, m)
	if m.vp.YOffset == offset {
		t.Fatal("setup: the child should open at its own bottom")
	}

	tm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = tm.(Model)
	if m.viewing != "" {
		t.Fatal("esc should return")
	}
	if cmd != nil {
		t.Fatal("esc must not cancel")
	}
	if m.vp.YOffset != offset {
		t.Fatalf("esc restored offset %d, want %d", m.vp.YOffset, offset)
	}

	m = openView(t, m)
	tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyLeft})
	m = tm.(Model)
	if m.viewing != "" {
		t.Fatal("left should return")
	}
	if m.vp.YOffset != offset {
		t.Fatalf("left restored offset %d, want %d", m.vp.YOffset, offset)
	}
	if m.status != statusWorking {
		t.Fatal("leaving must not cancel the turn")
	}

	// The child keeps its own position too: scrolled up inside, left, and
	// re-entered, it is where it was.
	m = openView(t, m)
	for i := 0; i < 2; i++ {
		tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyPgUp})
		m = tm.(Model)
	}
	childOff := m.vp.YOffset
	if childOff == 0 || m.vp.AtBottom() {
		t.Fatalf("setup: the child should be scrolled mid-way, offset %d", childOff)
	}
	tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = tm.(Model)
	m = openView(t, m)
	if m.vp.YOffset != childOff {
		t.Fatalf("re-entering restored child offset %d, want %d", m.vp.YOffset, childOff)
	}
}

func TestMouseAndPagingInsideTheView(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	m := agentModel(t, &now)
	m = applyInFlight(t, m, []agent.ToolEvent{taskTool("task-1", "count lines", "in_progress")})
	m.status = statusWorking
	m = tallChild(t, m, 300)
	m = openView(t, m)
	bottom := m.vp.YOffset
	if bottom < 3*wheelLines {
		t.Fatalf("setup: not enough child scrollback, offset %d", bottom)
	}
	m = wheel(t, m, tea.MouseButtonWheelUp)
	if got := m.vp.YOffset; got != bottom-wheelLines {
		t.Fatalf("wheel up inside the view moved to %d, want %d", got, bottom-wheelLines)
	}
	m = wheel(t, m, tea.MouseButtonWheelDown)
	if got := m.vp.YOffset; got != bottom {
		t.Fatalf("wheel down inside the view moved to %d, want %d", got, bottom)
	}
	tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyPgUp})
	m = tm.(Model)
	if got := m.vp.YOffset; got >= bottom {
		t.Fatalf("pgup inside the view did not page: %d", got)
	}
	tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyPgDown})
	m = tm.(Model)
	if got := m.vp.YOffset; got != bottom {
		t.Fatalf("pgdn inside the view moved to %d, want %d", got, bottom)
	}
	if m.viewing != "task-1" {
		t.Fatal("scrolling must not leave the view")
	}
	// A double-click selects from the child's transcript, not the main one.
	y := m.lay.Region(regionTranscript).Top
	m = clickXY(t, m, 1, y)
	m = clickXY(t, m, 1, y)
	if !m.sel.on {
		t.Fatal("double-click inside the view should start a selection")
	}
	if text := m.selectionText(); !strings.Contains(text, "childline") {
		t.Fatalf("selection came from the wrong transcript: %q", text)
	}
}

func TestClickOnRowAndBanner(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	m := agentModel(t, &now)
	m = applyInFlight(t, m, []agent.ToolEvent{
		taskTool("task-a", "job a", "in_progress"),
		taskTool("task-b", "job b", "in_progress"),
	})
	m.status = statusWorking
	row := m.lay.Region(regionAgents).Top
	got := clickAt(t, m, row)
	if got.viewing == "" {
		t.Fatal("click on a row should open the view")
	}
	banner := got.lay.Region(regionComposer).Top + 1
	got = clickAt(t, got, banner)
	if got.viewing != "" {
		t.Fatal("click on the banner should return")
	}
	if got.status != statusWorking {
		t.Fatal("returning must not cancel the turn")
	}
}

func TestArrowsScrollInsideTheView(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	m := agentModel(t, &now)
	m = applyInFlight(t, m, []agent.ToolEvent{taskTool("task-1", "count lines", "in_progress")})
	m = openView(t, m)
	tr := m.ensureSub("task-1")
	for i := 0; i < 40; i++ {
		tr.appendEntry(entry{kind: entryAssistant, text: fmt.Sprintf("child line %02d padding", i)}, now)
	}
	m.setViewportContent(true)
	if !m.vp.AtBottom() {
		t.Fatal("setup should stick to the bottom")
	}
	bottom := m.vp.YOffset
	tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyUp})
	m = tm.(Model)
	if m.viewing == "" {
		t.Fatal("up must not leave the view")
	}
	if m.vp.YOffset != bottom-1 {
		t.Fatalf("up scrolled to %d, want %d", m.vp.YOffset, bottom-1)
	}
	tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = tm.(Model)
	if m.vp.YOffset != bottom {
		t.Fatalf("down scrolled to %d, want %d", m.vp.YOffset, bottom)
	}
}

func TestTabSwitchDoesNotCycleMode(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	m := agentModel(t, &now)
	m = applyInFlight(t, m, []agent.ToolEvent{
		taskTool("task-a", "job a", "in_progress"),
		taskTool("task-b", "job b", "in_progress"),
	})
	mode := m.snap.CurrentMode
	m = openView(t, m)
	first := m.viewing
	tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = tm.(Model)
	if m.viewing == "" || m.viewing == first {
		t.Fatalf("tab should switch, viewing %q", m.viewing)
	}
	if m.snap.CurrentMode != mode {
		t.Fatalf("tab cycled the mode to %q", m.snap.CurrentMode)
	}
	tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyShiftTab})
	m = tm.(Model)
	if m.viewing != first {
		t.Fatalf("shift+tab should wrap back, viewing %q", m.viewing)
	}
	if m.snap.CurrentMode != mode {
		t.Fatalf("shift+tab cycled the mode to %q", m.snap.CurrentMode)
	}
}

func TestKeysPasteAndSlashInertInsideTheView(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	m := agentModel(t, &now)
	m = applyInFlight(t, m, []agent.ToolEvent{taskTool("task-1", "count lines", "in_progress")})
	m.input.SetValue("draft")
	m = clickAt(t, m, m.lay.Region(regionAgents).Top)
	if m.viewing == "" {
		t.Fatal("expected the sub-agent view")
	}
	if m.input.Value() != "draft" {
		t.Fatalf("entering should keep the draft: %q", m.input.Value())
	}
	tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
	m = tm.(Model)
	if m.input.Value() != "draft" {
		t.Fatalf("typing reached the composer: %q", m.input.Value())
	}
	if m.viewing == "" {
		t.Fatal("a rune left the view")
	}
	tm, _ = m.Update(enter())
	m = tm.(Model)
	if m.viewing == "" {
		t.Fatal("enter must be a no-op")
	}
	tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyRight})
	m = tm.(Model)
	if m.viewing == "" {
		t.Fatal("right must be a no-op")
	}
	tm, _ = m.Update(pasteMsg{text: "pasted"})
	m = tm.(Model)
	if m.input.Value() != "draft" {
		t.Fatalf("paste reached the composer: %q", m.input.Value())
	}
	m.input.SetValue("/")
	tm, _ = m.Update(refreshSnapMsg{})
	m = tm.(Model)
	if m.overlayView(m.lay) != "" {
		t.Fatalf("slash overlay drew inside the view: %q", m.overlayView(m.lay))
	}
}

func TestCardInsideTheView(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	m := agentModel(t, &now)
	m = applyInFlight(t, m, []agent.ToolEvent{taskTool("task-1", "count lines", "in_progress")})
	m = openView(t, m)
	tm, _ := m.Update(eventMsg{agent.Event{
		Type:       agent.EventPermission,
		Permission: &agent.PermissionEvent{ID: "perm-1", Tool: "bash"},
	}})
	m = tm.(Model)
	if !m.cardOpen() {
		t.Fatal("expected a card")
	}
	if m.viewing != "task-1" {
		t.Fatal("a card must leave the view open")
	}
}

func TestParentEventsWhileViewingDoNotMoveChildViewport(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	m := agentModel(t, &now)
	m = applyInFlight(t, m, []agent.ToolEvent{taskTool("task-1", "count lines", "in_progress")})
	m = openView(t, m)
	tr := m.ensureSub("task-1")
	for i := 0; i < 40; i++ {
		tr.appendEntry(entry{kind: entryAssistant, text: fmt.Sprintf("child line %02d padding", i)}, now)
	}
	m.setViewportContent(true)
	tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyUp})
	m = tm.(Model)
	offset := m.vp.YOffset
	if m.vp.AtBottom() {
		t.Fatal("setup: child should be scrolled up")
	}
	tm, _ = m.Update(eventMsg{agent.Event{Type: agent.EventText, Text: "parent chunk"}})
	m = tm.(Model)
	if m.vp.YOffset != offset {
		t.Fatalf("a parent chunk moved the child viewport: %d -> %d", offset, m.vp.YOffset)
	}
	if m.main.streamOpen() == false {
		t.Fatal("parent text should open the main stream")
	}
	tm, _ = m.Update(eventMsg{agent.Event{Type: agent.EventDone, StopReason: "end_turn"}})
	m = tm.(Model)
	if m.main.streamOpen() {
		t.Fatal("parent done must close the main stream")
	}
	if m.viewing != "task-1" {
		t.Fatal("parent done must not leave the view")
	}
	if m.vp.YOffset != offset {
		t.Fatalf("parent done moved the child viewport: %d -> %d", offset, m.vp.YOffset)
	}
}

func TestChildEventsWhileViewingMainDoNotTouchMainState(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	m := agentModel(t, &now)
	m = applyInFlight(t, m, []agent.ToolEvent{taskTool("task-1", "count lines", "in_progress")})
	m.lastThought = false
	m.sawAssistantSeq = 0
	offer, dead := m.planOfferSeq, m.planDeadSeq
	// A child read with a path is the only event that could write pathDirs.
	read := agent.ToolEvent{ID: "c-read", Kind: "read", Title: "read", Status: "completed", Locations: []string{"/ws/pkg/main.go"}}
	tm, _ := m.Update(eventMsg{agent.Event{Type: agent.EventTool, Agent: "task-1", Tool: &read}})
	m = tm.(Model)
	if m.planOfferSeq != offer || m.planDeadSeq != dead {
		t.Fatal("child events must not touch the plan offer")
	}
	tm, _ = m.Update(eventMsg{agent.Event{Type: agent.EventThought, Agent: "task-1", Text: "thinking"}})
	m = tm.(Model)
	if m.lastThought {
		t.Fatal("child thought must not set lastThought")
	}
	tm, _ = m.Update(eventMsg{agent.Event{Type: agent.EventText, Agent: "task-1", Text: "child reply"}})
	m = tm.(Model)
	if m.sawAssistantSeq != 0 {
		t.Fatal("child text must not set sawAssistantSeq")
	}
	if len(m.main.pathDirs) != 0 {
		t.Fatalf("child events must not write main pathDirs: %v", m.main.pathDirs)
	}
	if m.subs["task-1"] == nil || len(m.subs["task-1"].entries()) == 0 {
		t.Fatal("child events should land on the sub transcript")
	}
}

// TestParentDoneWhileChildRunsKeepsRowAndFastTick: the parent turn is over, so
// nothing keeps the row or the chain except the child that is still running.
// The turn is a real one, run to its end, so "over" means what it means to a
// user rather than a pair of ending marks set by the test.
func TestParentDoneWhileChildRunsKeepsRowAndFastTick(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	m := agentModel(t, &now)
	m = applyInFlight(t, m, []agent.ToolEvent{taskTool("task-1", "count lines", "in_progress")})
	m = pumpEnter(t, m, "spawn one")
	m = pumpUntil(t, m, isIdle)
	m = pumpSettled(t, m)
	if len(m.visibleAgents()) == 0 {
		t.Fatal("a running child must keep its row after parent done")
	}
	if !m.anySubagentRunning() {
		t.Fatal("expected a running sub-agent")
	}
	if !m.wantFastTick() {
		t.Fatal("a running child keeps the fast tick")
	}
}

func TestDegradationToZeroRowsKeepsTheViewOpen(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	m := agentModel(t, &now)
	m = applyInFlight(t, m, []agent.ToolEvent{taskTool("task-1", "count lines", "in_progress")})
	m = openView(t, m)
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 40, Height: 12})
	m = tm.(Model)
	if m.viewing != "task-1" {
		t.Fatal("degradation must not close the view")
	}
	if h := lipgloss.Height(plainView(m)); h != 12 {
		t.Fatalf("40x12 frame is %d rows", h)
	}
	for _, ln := range strings.Split(plainView(m), "\n") {
		if w := lipgloss.Width(ln); w != 40 {
			t.Fatalf("line is %d wide: %q", w, ln)
		}
	}
}

func TestEvictionWhileViewedKeepsTombstoneUntilEsc(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	m := agentModel(t, &now)
	m = applyInFlight(t, m, []agent.ToolEvent{taskTool("task-1", "count lines", "in_progress")})
	m = openView(t, m)
	m.sess.(*Stub).SetSubagents(nil)
	m = poke(t, m)
	if m.viewing != "task-1" {
		t.Fatal("eviction while viewed must keep the tombstone")
	}
	if m.tombstone == nil || m.tombstone.ID != "task-1" {
		t.Fatal("expected a tombstone copy")
	}
	if m.subs["task-1"] == nil {
		t.Fatal("the viewed transcript must stay")
	}
	tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = tm.(Model)
	if m.viewing != "" {
		t.Fatal("esc releases the tombstone")
	}
	if m.tombstone != nil {
		t.Fatal("tombstone should be gone")
	}
	if m.subs["task-1"] != nil {
		t.Fatal("the evicted transcript should be dropped")
	}
}

func TestUnviewedLingerWithInjectedClock(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	m := agentModel(t, &now)
	m = applyInFlight(t, m, []agent.ToolEvent{taskTool("task-1", "count lines", "in_progress")})
	m = applyInFlight(t, m, []agent.ToolEvent{finishedTaskTool("task-1", "count lines")})
	now = now.Add(agentLinger - time.Second)
	m = poke(t, m)
	if len(m.visibleAgents()) == 0 {
		t.Fatal("the row should linger")
	}
	now = now.Add(2 * time.Second)
	m = poke(t, m)
	if len(m.visibleAgents()) != 0 {
		t.Fatal("the unviewed row should be gone after the linger")
	}
}

func TestClearLeavesSubTranscriptsAlone(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	m := agentModel(t, &now)
	m = applyInFlight(t, m, []agent.ToolEvent{taskTool("task-1", "count lines", "in_progress")})
	tm, _ := m.Update(eventMsg{agent.Event{Type: agent.EventText, Agent: "task-1", Text: "child text"}})
	m = tm.(Model)
	m.addUser("main user")
	m.input.SetValue("/clear")
	tm, _ = m.Update(enter())
	m = tm.(Model)
	if len(m.main.entries()) != 0 {
		t.Fatal("/clear should empty main")
	}
	if m.subs["task-1"] == nil || len(m.subs["task-1"].entries()) == 0 {
		t.Fatal("/clear must leave sub transcripts alone")
	}
}

func TestSubagentByteBudgets(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	m := agentModel(t, &now)
	m = applyInFlight(t, m, []agent.ToolEvent{taskTool("task-1", "count lines", "in_progress")})
	huge := strings.Repeat("a", 200*1024)
	tm, _ := m.Update(eventMsg{agent.Event{Type: agent.EventText, Agent: "task-1", Text: huge}})
	m = tm.(Model)
	tr := m.subs["task-1"]
	if tr == nil || len(tr.entries()) == 0 {
		t.Fatal("expected a streamed entry")
	}
	if got := len(tr.entries()[len(tr.entries())-1].text); got > entryTextCap {
		t.Fatalf("entry is %d bytes, want <= %d", got, entryTextCap)
	}
	if !strings.HasPrefix(tr.entries()[len(tr.entries())-1].text, "…") {
		t.Fatal("a capped entry keeps a … prefix")
	}

	m = agentModel(t, &now)
	m = applyInFlight(t, m, []agent.ToolEvent{taskTool("task-1", "count lines", "in_progress")})
	for i := 0; i < 2000; i++ {
		tool := agent.ToolEvent{ID: fmt.Sprintf("c-%d", i), Kind: "read", Title: "f", Status: "completed"}
		tm, _ = m.Update(eventMsg{agent.Event{Type: agent.EventTool, Agent: "task-1", Tool: &tool}})
		m = tm.(Model)
	}
	tr = m.subs["task-1"]
	if tr == nil {
		t.Fatal("expected a sub transcript")
	}
	if len(tr.entries()) > subMaxEntries {
		t.Fatalf("entries %d, want <= %d", len(tr.entries()), subMaxEntries)
	}
	if !tr.trimmed {
		t.Fatal("2000 chunks should trim")
	}

	// The raw-text budget: 60 × 40 KiB entries (alternating kinds so they do
	// not merge) is 2.4 MiB, so the transcript has to have trimmed down to
	// the 1 MiB budget.
	m = agentModel(t, &now)
	m = applyInFlight(t, m, []agent.ToolEvent{taskTool("task-1", "count lines", "in_progress")})
	chunk := strings.Repeat("b", 40*1024)
	for i := 0; i < 60; i++ {
		kind := agent.EventText
		if i%2 == 1 {
			kind = agent.EventThought
		}
		tm, _ = m.Update(eventMsg{agent.Event{Type: kind, Agent: "task-1", Text: chunk}})
		m = tm.(Model)
	}
	tr = m.subs["task-1"]
	total := 0
	for _, e := range tr.entries() {
		total += len(e.text)
	}
	if total > subTextBudget {
		t.Fatalf("sub transcript holds %d bytes, budget %d", total, subTextBudget)
	}
	if !tr.trimmed || len(tr.entries()) >= 60 {
		t.Fatalf("the text budget should have trimmed: trimmed=%v entries=%d", tr.trimmed, len(tr.entries()))
	}
}

func TestLongUnicodeChipAndBanner(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	m := agentModel(t, &now)
	tool := taskTool("task-1", strings.Repeat("世界", 40), "in_progress")
	tool.Task.Model = "cursor-grok-4.6-high-fast"
	m = applyInFlight(t, m, []agent.ToolEvent{tool})
	m = openView(t, m)
	for _, size := range []struct{ cols, rows int }{{80, 24}, {40, 12}} {
		tm, _ := m.Update(tea.WindowSizeMsg{Width: size.cols, Height: size.rows})
		m = tm.(Model)
		view := plainView(m)
		if h := lipgloss.Height(view); h != size.rows {
			t.Fatalf("%dx%d frame is %d rows", size.cols, size.rows, h)
		}
		for _, ln := range strings.Split(view, "\n") {
			if w := lipgloss.Width(ln); w != size.cols {
				t.Fatalf("%dx%d line is %d wide: %q", size.cols, size.rows, w, ln)
			}
		}
		if !strings.Contains(view, "esc to return") {
			t.Fatalf("%dx%d dropped esc to return:\n%s", size.cols, size.rows, view)
		}
	}
}

func TestResizeWhileScrolledInsideTheView(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	m := agentModel(t, &now)
	m = applyInFlight(t, m, []agent.ToolEvent{taskTool("task-1", "count lines", "in_progress")})
	m = openView(t, m)
	tr := m.ensureSub("task-1")
	for i := 0; i < 40; i++ {
		tr.appendEntry(entry{kind: entryAssistant, text: fmt.Sprintf("child line %02d padding", i)}, now)
	}
	m.setViewportContent(true)
	tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyPgUp})
	m = tm.(Model)
	if m.vp.AtBottom() {
		t.Fatal("setup: should be scrolled up")
	}
	tm, _ = m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = tm.(Model)
	if m.viewing != "task-1" {
		t.Fatal("resize left the view")
	}
	if m.vp.AtBottom() {
		t.Fatal("resize jumped to the bottom while scrolled up")
	}
}

func TestExactHeightAt40x12InsideTheView(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	m := agentModel(t, &now)
	m = applyInFlight(t, m, []agent.ToolEvent{taskTool("task-1", "count lines", "in_progress")})
	m = openView(t, m)
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 40, Height: 12})
	m = tm.(Model)
	view := plainView(m)
	if h := lipgloss.Height(view); h != 12 {
		t.Fatalf("frame is %d rows:\n%s", h, view)
	}
	if m.lay.ComposerRows != 1 {
		t.Fatalf("ComposerRows %d, want 1", m.lay.ComposerRows)
	}
}

func TestLayoutComputedOncePerUpdateInsideTheView(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	m := agentModel(t, &now)
	m = applyInFlight(t, m, []agent.ToolEvent{taskTool("task-1", "count lines", "in_progress")})
	m = openView(t, m)
	for _, msg := range []tea.Msg{
		tea.WindowSizeMsg{Width: 100, Height: 30},
		tea.KeyMsg{Type: tea.KeyUp},
		refreshSnapMsg{},
	} {
		before := m.layouts
		tm, _ := m.Update(msg)
		m = tm.(Model)
		if got := m.layouts - before; got != 1 {
			t.Fatalf("%T computed the layout %d times, want 1", msg, got)
		}
	}
}

func TestConsecutiveUserChunksMerge(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	m := agentModel(t, &now)
	m = applyInFlight(t, m, []agent.ToolEvent{taskTool("task-1", "count lines", "in_progress")})
	tm, _ := m.Update(eventMsg{agent.Event{Type: agent.EventUser, Agent: "task-1", Text: "List the files in the"}})
	m = tm.(Model)
	tm, _ = m.Update(eventMsg{agent.Event{Type: agent.EventUser, Agent: "task-1", Text: " current directory."}})
	m = tm.(Model)
	tr := m.subs["task-1"]
	var users []string
	for _, e := range tr.entries() {
		if e.kind == entryUser {
			users = append(users, e.text)
		}
	}
	if len(users) != 1 || users[0] != "List the files in the current directory." {
		t.Fatalf("user entries %q", users)
	}
}

// TestChildCommandLineLandsInTheChildTranscript: nothing emits an expansion
// against a child today — craze expands its own prompts, and those are the main
// session's — but one that arrived would belong to the child's transcript for
// the same reason its user block does, and dropping it silently is the one
// thing a sub-agent view must not do with an event it was handed.
func TestChildCommandLineLandsInTheChildTranscript(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	m := agentModel(t, &now)
	m = applyInFlight(t, m, []agent.ToolEvent{taskTool("task-1", "count lines", "in_progress")})
	tm, _ := m.Update(eventMsg{agent.Event{Type: agent.EventCommand, Agent: "task-1", Command: &agent.ExpandedCommand{
		PluginCommand: agent.PluginCommand{
			Plugin: "probe-plugin", Bare: "probe-echo",
			Display: "probe-echo", Qualified: "probe-plugin:probe-echo",
			Kind: agent.PluginKindCommand,
		},
		Text: "the block, which the transcript never shows",
	}}})
	m = tm.(Model)
	var notes []string
	for _, e := range m.subs["task-1"].entries() {
		if e.kind == entryNote {
			notes = append(notes, e.text)
		}
	}
	if !slices.Contains(notes, "⤷ probe-plugin:probe-echo (command)") {
		t.Fatalf("child notes %q", notes)
	}
	// The main transcript is not where a child's event goes.
	for _, e := range m.main.entries() {
		if strings.Contains(e.text, "probe-plugin") {
			t.Fatalf("the child's line reached the main transcript: %q", e.text)
		}
	}
}

func TestChildFinishedClosesChildStream(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	m := agentModel(t, &now)
	m = applyInFlight(t, m, []agent.ToolEvent{taskTool("task-1", "count lines", "in_progress")})
	tm, _ := m.Update(eventMsg{agent.Event{Type: agent.EventText, Agent: "task-1", Text: "partial"}})
	m = tm.(Model)
	if !m.subs["task-1"].streamOpen() {
		t.Fatal("expected an open child stream")
	}
	fin := subagentsFromTools([]agent.ToolEvent{finishedTaskTool("task-1", "count lines")})[0]
	m.sess.(*Stub).SetSubagents([]agent.SubagentInfo{fin})
	tm, _ = m.Update(eventMsg{agent.Event{
		Type:           agent.EventSubagent,
		Subagent:       &fin,
		SubagentChange: agent.SubagentChangeFinished,
	}})
	m = tm.(Model)
	if m.subs["task-1"].streamOpen() {
		t.Fatal("finished must close the child stream")
	}
}

// TestRespawnedAttemptResetsRowTiming pins the resumed-attempt rule: a new
// spawned for the same id counts elapsed from this sighting and lingers from
// the new finish, not the previous attempt's stamps.
func TestRespawnedAttemptResetsRowTiming(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	m := agentModel(t, &now)
	m = applyInFlight(t, m, []agent.ToolEvent{taskTool("task-1", "count lines", "in_progress")})
	fin := subagentsFromTools([]agent.ToolEvent{finishedTaskTool("task-1", "count lines")})[0]
	m.sess.(*Stub).SetSubagents([]agent.SubagentInfo{fin})
	tm, _ := m.Update(eventMsg{agent.Event{
		Type:           agent.EventSubagent,
		Subagent:       &fin,
		SubagentChange: agent.SubagentChangeFinished,
	}})
	m = tm.(Model)
	if _, ok := m.agentDone["task-1"]; !ok {
		t.Fatal("finish must stamp agentDone")
	}
	now = now.Add(90 * time.Second)
	retry := fin
	retry.Status = agent.SubagentRunning
	retry.AttemptID = "at2"
	m.sess.(*Stub).SetSubagents([]agent.SubagentInfo{retry})
	tm, _ = m.Update(eventMsg{agent.Event{
		Type:           agent.EventSubagent,
		Subagent:       &retry,
		SubagentChange: agent.SubagentChangeSpawned,
	}})
	m = tm.(Model)
	if _, ok := m.agentDone["task-1"]; ok {
		t.Fatal("a new attempt must clear the old finish stamp")
	}
	if got := m.agentStart["task-1"]; !got.Equal(now) {
		t.Fatalf("elapsed must restart at the new sighting, got %v", got)
	}
}

func TestFinishOnlySightingLeavesNoStaleStamp(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	m := agentModel(t, &now)
	m = applyInFlight(t, m, []agent.ToolEvent{taskTool("task-1", "count lines", "in_progress")})
	ghost := agent.SubagentInfo{ID: "ghost", Status: agent.SubagentCompleted, Description: "never spawned here"}
	tm, _ := m.Update(eventMsg{agent.Event{Type: agent.EventSubagent, Subagent: &ghost, SubagentChange: agent.SubagentChangeFinished}})
	m = tm.(Model)
	if _, ok := m.agentDone["ghost"]; ok {
		t.Fatal("a record the snapshot never held must not keep a done stamp")
	}
}

func TestCursorFinishedWhileViewedGetsTheWarnBanner(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	m := agentModel(t, &now)
	m.sess.(*Stub).SetProvider(agent.CursorProvider())
	m.refreshSnap()
	m = applyInFlight(t, m, []agent.ToolEvent{taskTool("task-1", "count lines", "in_progress")})
	m.status = statusWorking
	m = openView(t, m)
	if v := plainView(m); !strings.Contains(v, "○ @task · receipt only · esc to return") {
		t.Fatalf("running receipt banner:\n%s", v)
	}
	subs := subagentsFromTools([]agent.ToolEvent{finishedTaskTool("task-1", "count lines")})
	subs[0].Status = agent.SubagentCompleted
	m.sess.(*Stub).SetSubagents(subs)
	tm, _ := m.Update(eventMsg{agent.Event{Type: agent.EventSubagent, Subagent: &subs[0], SubagentChange: agent.SubagentChangeFinished}})
	m = tm.(Model)
	if m.viewing != "task-1" {
		t.Fatal("finishing must not close the view")
	}
	if v := plainView(m); !strings.Contains(v, "✓ @task · completed · receipt only · esc to return") {
		t.Fatalf("finished receipt banner:\n%s", v)
	}
}

func TestSpinnerClockAfterEndTurnIsTheSubagents(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	m := agentModel(t, &now)
	m = applyInFlight(t, m, []agent.ToolEvent{taskTool("task-1", "count lines", "in_progress")})
	m.status = statusWorking
	m.turnStart = now.Add(-5 * time.Minute)
	m.agentStart["task-1"] = now.Add(-45 * time.Second)
	if v := m.spinnerView(); !strings.Contains(v, "5m") || !strings.Contains(v, "esc to interrupt") {
		t.Fatalf("while working the spinner shows the turn's clock:\n%s", v)
	}
	m.status = statusIdle
	v := m.spinnerView()
	if v == "" {
		t.Fatal("a running sub-agent keeps the spinner up after end_turn")
	}
	if strings.Contains(v, "5m") || strings.Contains(v, "esc to interrupt") {
		t.Fatalf("after end_turn the turn's clock must not keep growing:\n%s", v)
	}
	if !strings.Contains(v, "45s") {
		t.Fatalf("after end_turn the spinner shows the sub-agent's elapsed:\n%s", v)
	}
}
