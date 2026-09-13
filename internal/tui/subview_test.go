package tui

import (
	"fmt"
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
	tm, _ := m.Update(enter())
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

func TestEscAndLeftReturnAndRestoreOffset(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	m := agentModel(t, &now)
	for i := 0; i < 40; i++ {
		m.appendEntry(entry{kind: entryAssistant, text: fmt.Sprintf("line-%02d padding so the transcript is taller than the viewport", i)})
	}
	m.refreshViewport()
	tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyPgUp})
	m = tm.(Model)
	if m.vp.AtBottom() {
		t.Fatal("setup: page up should leave the main transcript scrolled")
	}
	offset := m.vp.YOffset
	m = applyInFlight(t, m, []agent.ToolEvent{taskTool("task-1", "count lines", "in_progress")})
	m.status = statusWorking
	m = openView(t, m)

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
	if m.overlayView() != "" {
		t.Fatalf("slash overlay drew inside the view: %q", m.overlayView())
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
	if m.main.streamOpen == false {
		t.Fatal("parent text should open the main stream")
	}
	tm, _ = m.Update(eventMsg{agent.Event{Type: agent.EventDone, StopReason: "end_turn"}})
	m = tm.(Model)
	if m.main.streamOpen {
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
	touch := append([]string(nil), m.agentTouch...)
	tm, _ := m.Update(eventMsg{agent.Event{Type: agent.EventThought, Agent: "task-1", Text: "thinking"}})
	m = tm.(Model)
	if m.lastThought {
		t.Fatal("child thought must not set lastThought")
	}
	tm, _ = m.Update(eventMsg{agent.Event{Type: agent.EventText, Agent: "task-1", Text: "child reply"}})
	m = tm.(Model)
	if m.sawAssistantSeq != 0 {
		t.Fatal("child text must not set sawAssistantSeq")
	}
	if got := strings.Join(m.agentTouch, ","); got != strings.Join(touch, ",") {
		t.Fatalf("child text touched agentTouch: %s -> %s", touch, m.agentTouch)
	}
	if len(m.main.pathDirs) != 0 {
		t.Fatalf("child events must not write main pathDirs: %v", m.main.pathDirs)
	}
	if m.subs["task-1"] == nil || len(m.subs["task-1"].entries) == 0 {
		t.Fatal("child events should land on the sub transcript")
	}
}

func TestParentDoneWhileChildRunsKeepsRowAndFastTick(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	m := agentModel(t, &now)
	m = applyInFlight(t, m, []agent.ToolEvent{taskTool("task-1", "count lines", "in_progress")})
	m.status = statusWorking
	m.promptEndSeq = m.turnSeq
	tm, _ := m.Update(eventMsg{agent.Event{Type: agent.EventDone, StopReason: "end_turn"}})
	m = tm.(Model)
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
	if len(m.main.entries) != 0 {
		t.Fatal("/clear should empty main")
	}
	if m.subs["task-1"] == nil || len(m.subs["task-1"].entries) == 0 {
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
	if tr == nil || len(tr.entries) == 0 {
		t.Fatal("expected a streamed entry")
	}
	if got := len(tr.entries[len(tr.entries)-1].text); got > entryTextCap {
		t.Fatalf("entry is %d bytes, want <= %d", got, entryTextCap)
	}
	if !strings.HasPrefix(tr.entries[len(tr.entries)-1].text, "…") {
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
	if len(tr.entries) > subMaxEntries {
		t.Fatalf("entries %d, want <= %d", len(tr.entries), subMaxEntries)
	}
	if !tr.trimmed {
		t.Fatal("2000 chunks should trim")
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
	for _, e := range tr.entries {
		if e.kind == entryUser {
			users = append(users, e.text)
		}
	}
	if len(users) != 1 || users[0] != "List the files in the current directory." {
		t.Fatalf("user entries %q", users)
	}
}

func TestChildFinishedClosesChildStream(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	m := agentModel(t, &now)
	m = applyInFlight(t, m, []agent.ToolEvent{taskTool("task-1", "count lines", "in_progress")})
	tm, _ := m.Update(eventMsg{agent.Event{Type: agent.EventText, Agent: "task-1", Text: "partial"}})
	m = tm.(Model)
	if !m.subs["task-1"].streamOpen {
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
	if m.subs["task-1"].streamOpen {
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
