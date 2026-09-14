package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/craze/internal/agent"
)

func clickAt(t *testing.T, m Model, y int) Model {
	t.Helper()
	return clickXY(t, m, 1, y)
}

func clickXY(t *testing.T, m Model, x, y int) Model {
	t.Helper()
	tm, cmd := m.Update(tea.MouseMsg{
		Action: tea.MouseActionPress,
		Button: tea.MouseButtonLeft,
		X:      x,
		Y:      y,
	})
	m = tm.(Model)
	if cmd != nil {
		// A click that changed the session (the mode chip) leaves its work in
		// a command; running it is what makes the stub agree with the model.
		runCmd(cmd)
	}
	return m
}

// modeChipClick is the middle of row 2's mode chip, from the row's own spans.
func modeChipClick(t *testing.T, m Model) (x, y int) {
	t.Helper()
	_, spans := m.statusRow2(m.lay)
	s := spanRange(t, spans, spanMode)
	return (s.x0 + s.x1) / 2, m.lay.Region(regionStatus).Top + 1
}

// TestClickOnTheModeChipCycles: the chip is shift+tab under the pointer, and
// the note it writes carries the agent's description of the new mode.
func TestClickOnTheModeChipCycles(t *testing.T) {
	m := sized(t)
	x, y := modeChipClick(t, m)
	next := clickXY(t, m, x, y)
	if next.snap.CurrentMode != "plan" {
		t.Fatalf("clicking the chip cycled to %q", next.snap.CurrentMode)
	}
	notes := texts(next, entryNote)
	want := "mode → plan · Read-only mode for planning and designing before implementation"
	if len(notes) == 0 || notes[len(notes)-1] != want {
		t.Fatalf("note %q, want %q", notes, want)
	}
	if !strings.Contains(plainView(next), "◆ plan") {
		t.Fatalf("the chip should follow the mode:\n%s", plainView(next))
	}

	// Just past the chip is the hint, which is not clickable.
	_, spans := m.statusRow2(m.lay)
	past := spanRange(t, spans, spanMode).x1
	if same := clickXY(t, m, past, y); same.snap.CurrentMode != "agent" {
		t.Fatalf("a click on the separator cycled to %q", same.snap.CurrentMode)
	}
	// Neither is row 1, where the model span lives and V3 has not landed yet.
	if same := clickXY(t, m, x, y-1); same.snap.CurrentMode != "agent" {
		t.Fatalf("a click on row 1 cycled to %q", same.snap.CurrentMode)
	}
}

// TestClickOnTheModeChipWhileWorking mirrors shift+tab's gating: a turn in
// flight does not block a mode change, a card does.
func TestClickOnTheModeChipWhileWorking(t *testing.T) {
	m := sized(t)
	m.status = statusWorking
	x, y := modeChipClick(t, m)
	if got := clickXY(t, m, x, y); got.snap.CurrentMode != "plan" {
		t.Fatalf("working must not block the chip, mode %q", got.snap.CurrentMode)
	}

	card, stub := sizedCards(t)
	card = cardEvent(t, card, stub, agent.Event{Type: agent.EventQuestion, Question: stubQuestion()})
	if !card.cardOpen() {
		t.Fatal("expected a card")
	}
	x, y = modeChipClick(t, card)
	if got := clickXY(t, card, x, y); got.snap.CurrentMode != "agent" {
		t.Fatalf("a card owns the mouse, mode %q", got.snap.CurrentMode)
	}
}

func wheel(t *testing.T, m Model, button tea.MouseButton) Model {
	t.Helper()
	tm, _ := m.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: button, X: 1, Y: 1})
	return tm.(Model)
}

// scrollable fills the transcript so the viewport has somewhere to go.
func scrollable(t *testing.T) Model {
	t.Helper()
	m := sized(t)
	var b strings.Builder
	for i := 0; i < 60; i++ {
		fmt.Fprintf(&b, "line %02d\n\n", i)
	}
	tm, _ := m.Update(eventMsg{agent.Event{Type: agent.EventText, Text: b.String()}})
	return tm.(Model)
}

func TestWheelScrollsTheTranscript(t *testing.T) {
	m := scrollable(t)
	if !m.vp.AtBottom() {
		t.Fatal("a fresh transcript sticks to the bottom")
	}
	bottom := m.vp.YOffset
	if bottom < 2*wheelLines {
		t.Fatalf("not enough scrollback to test with: offset %d", bottom)
	}

	m = wheel(t, m, tea.MouseButtonWheelUp)
	if got, want := m.vp.YOffset, bottom-wheelLines; got != want {
		t.Fatalf("wheel up moved to %d, want %d (%d lines per notch)", got, want, wheelLines)
	}
	m = wheel(t, m, tea.MouseButtonWheelDown)
	if got := m.vp.YOffset; got != bottom {
		t.Fatalf("wheel down moved to %d, want %d", got, bottom)
	}

	// Motion with nothing pressed is not a drag: the selection needs a press to
	// anchor it, so a bare motion report changes nothing at all.
	tm, _ := m.Update(tea.MouseMsg{Action: tea.MouseActionMotion, Button: tea.MouseButtonLeft, Y: m.lay.Region(regionStatus).Top})
	next := tm.(Model)
	if got := next.vp.YOffset; got != bottom {
		t.Fatalf("a motion with no press moved the viewport to %d", got)
	}
	if next.sel.on {
		t.Fatal("a motion with no press started a selection")
	}
}

func TestClickHitTestsAgentRowsAfterAResize(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	m := agentModel(t, &now)
	m = applyInFlight(t, m, []agent.ToolEvent{
		taskTool("task-a", "job a", "in_progress"),
		taskTool("task-b", "job b", "in_progress"),
	})
	// Rows sit in spawn order: row 0 is task-a and row 1 is task-b.
	rowsTop := m.lay.Region(regionAgents).Top
	if got := clickAt(t, m, rowsTop+1); got.agentID != "task-b" || got.viewing != "task-b" {
		t.Fatalf("click on row 1 selected %q (view %q)", got.agentID, got.viewing)
	}

	tm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	m = tm.(Model)
	if m.lay.Region(regionAgents).Top == rowsTop {
		t.Fatal("the resize should have moved the agent rows")
	}
	// The old coordinate is transcript now, and hit-testing follows the layout.
	if got := clickAt(t, m, rowsTop); got.viewing != "" {
		t.Fatalf("a stale row coordinate selected %q", got.agentID)
	}
	if got := clickAt(t, m, m.lay.Region(regionAgents).Top+1); got.agentID != "task-b" || got.viewing != "task-b" {
		t.Fatalf("after the resize row 1 selected %q (view %q)", got.agentID, got.viewing)
	}
	// The overflow row is not a sub-agent and selects nothing.
	if got := clickAt(t, m, m.lay.Region(regionAgents).Bottom); got.viewing != "" {
		t.Fatal("a click below the rows selected one")
	}
}

func TestClickOnTheTasksHeaderCycles(t *testing.T) {
	m := sized(t)
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = tm.(Model)
	todos := stubTodos()
	m.sess.(*Stub).SetTodos(todos)
	tm, _ = m.Update(eventMsg{agent.Event{Type: agent.EventTodos, Todos: todos}})
	m = tm.(Model)
	if m.lay.Region(regionTasks).Empty() {
		t.Fatalf("expected a tasks panel:\n%s", plainView(m))
	}

	next := clickAt(t, m, m.lay.Region(regionTasks).Top)
	if next.tasksState != tasksExpanded {
		t.Fatalf("clicking the header should cycle, state %v", next.tasksState)
	}
	// Rows are not clickable.
	same := clickAt(t, m, m.lay.Region(regionTasks).Top+1)
	if same.tasksState != m.tasksState {
		t.Fatalf("a task row cycled the panel to %v", same.tasksState)
	}
}

// TestClickPicksAModelRow drives the modal layer with the mouse: a press on a
// list row picks that model, a press on the box border picks nothing, and a
// press outside the box closes it without applying anything.
func TestClickPicksAModelRow(t *testing.T) {
	m := sized(t)
	m.input.SetValue("/model")
	tm, _ := m.Update(enter())
	m = tm.(Model)
	if m.dialog != dialogModel {
		t.Fatal("expected the model dialog")
	}
	r := m.lay.Dialog
	if r.Empty() {
		t.Fatalf("the dialog has no rectangle: %+v", m.lay)
	}
	// Row 0 is the border, row 1 the title, row 2 the filter, so row 3 is the
	// first model and row 4 the second.
	next := clickXY(t, m, r.X+2, r.Y+4)
	if next.snap.CurrentModel != "fast" {
		t.Fatalf("clicking the second model chose %q", next.snap.CurrentModel)
	}
	if next.dialog != dialogNone {
		t.Fatal("picking a model closes the dialog")
	}
	same := clickXY(t, m, r.X+2, r.Y)
	if same.snap.CurrentModel != "grok" || same.dialog != dialogModel {
		t.Fatalf("the box border is not an option: %q %v", same.snap.CurrentModel, same.dialog)
	}
	out := clickXY(t, m, 0, r.Y)
	if out.dialog != dialogNone || out.snap.CurrentModel != "grok" {
		t.Fatalf("a press outside closes without applying: %v %q", out.dialog, out.snap.CurrentModel)
	}
}

// TestClickModelSpanOpensTheDialog is the third opener (§3.4): the model name
// in status row 1.
func TestClickModelSpanOpensTheDialog(t *testing.T) {
	m := sized(t)
	x, y := modelSpanClick(t, m)
	next := clickXY(t, m, x, y)
	if next.dialog != dialogModel {
		t.Fatalf("clicking the model span left dialog=%v", next.dialog)
	}
	if !strings.Contains(plainView(next), modelDialogHint) {
		t.Fatalf("the dialog is not on screen:\n%s", plainView(next))
	}
	// Just past the span is not the model.
	s := spanRange(t, spans1(m), spanModel)
	if after := clickXY(t, m, s.x1, y); after.dialog != dialogNone {
		t.Fatal("a press past the model span opened the dialog")
	}
}

// modelSpanClick is the middle of row 1's model name, from the row's own spans.
func modelSpanClick(t *testing.T, m Model) (x, y int) {
	t.Helper()
	s := spanRange(t, spans1(m), spanModel)
	return (s.x0 + s.x1) / 2, m.lay.Region(regionStatus).Top
}

func spans1(m Model) []segSpan {
	_, spans := m.statusRow1()
	return spans
}

func TestClicksAreIgnoredWhileACardIsUp(t *testing.T) {
	m := loadedModel(t, 100, 30)
	if !m.cardOpen() {
		t.Fatal("the fixture should have a card up")
	}
	next := clickAt(t, m, m.lay.Region(regionTasks).Top)
	if next.tasksState != m.tasksState {
		t.Fatal("a card owns the keyboard and the mouse")
	}
}

// TestWheelIsIgnoredWhileACardIsUp: the card owns the whole mouse, not only
// the clicks, so the transcript does not scroll away underneath it.
func TestWheelIsIgnoredWhileACardIsUp(t *testing.T) {
	m, stub := sizedCards(t)
	var b strings.Builder
	for i := 0; i < 60; i++ {
		fmt.Fprintf(&b, "line %02d\n\n", i)
	}
	tm, _ := m.Update(eventMsg{agent.Event{Type: agent.EventText, Text: b.String()}})
	m = cardEvent(t, tm.(Model), stub, agent.Event{Type: agent.EventQuestion, Question: stubQuestion()})
	bottom := m.vp.YOffset
	if bottom < 2*wheelLines {
		t.Fatalf("not enough scrollback to test with: offset %d", bottom)
	}
	if got := wheel(t, m, tea.MouseButtonWheelUp).vp.YOffset; got != bottom {
		t.Fatalf("the wheel scrolled to %d behind the card, want %d", got, bottom)
	}
	if got := wheel(t, m, tea.MouseButtonWheelDown).vp.YOffset; got != bottom {
		t.Fatalf("the wheel scrolled to %d behind the card, want %d", got, bottom)
	}
}

// Clicking the mode chip is shift+tab under the pointer, so an overlay that
// swallows the key has to swallow the click too.
func TestChipClickBlockedByOverlays(t *testing.T) {
	for _, tc := range []struct {
		name string
		open func(*Model)
	}{
		{"help", func(m *Model) { *m = m.openHelp() }},
		{"model dialog", func(m *Model) { *m = m.openModelDialog() }},
		{"theme dialog", func(m *Model) { *m = m.openThemePicker() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := sized(t)
			tm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
			m = tm.(Model)
			before := m.snap.CurrentMode
			tc.open(&m)
			_, spans := m.statusRow2(m.lay)
			x := -1
			for _, s := range spans {
				if s.id == spanMode {
					x = s.x0
				}
			}
			if x < 0 {
				t.Fatal("no mode span to click")
			}
			out, _ := m.clickStatus(x, 1, m.lay)
			if got := out.(Model).snap.CurrentMode; got != before {
				t.Fatalf("%s open: click cycled mode %q → %q; shift+tab would not have",
					tc.name, before, got)
			}
		})
	}
}

// ------------------------------------------------------------------ §3.5

// TestClickOnASlashRowAcceptsTheScrolledRow is the bug the test exists to
// catch: the row under the pointer is slashTop+row, not row. Row 2 of a band
// that has scrolled off the top is a different command from items[2], and a
// click that forgot slashTop would accept the wrong one silently.
func TestClickOnASlashRowAcceptsTheScrolledRow(t *testing.T) {
	m, _ := slashScrolled(t, 10)
	if m.slashTop == 0 {
		t.Fatal("fixture: the window should have scrolled off the top")
	}
	items := m.filteredSlash()
	if items[2].Name == items[m.slashTop+2].Name {
		t.Fatal("fixture: row 2 and slashTop+2 must name different commands")
	}
	want := items[m.slashTop+2].Name
	top := m.lay.Region(regionOverlay).Top
	m = clickAt(t, m, top+2)
	if got := m.input.Value(); got != "see /"+want+" " {
		t.Fatalf("click on row 2 accepted %q, want /%s (items[2] would be /%s)",
			got, want, items[2].Name)
	}
}

// TestClickPastTheLastSlashRowIsANoOp: the band's own region never draws more
// rows than the layout granted, so a click below it — where a row "past the
// last item" would land — never reaches acceptSlash at all.
func TestClickPastTheLastSlashRowIsANoOp(t *testing.T) {
	m, _ := slashScrolled(t, 10)
	before, sel, top := m.input.Value(), m.slashSel, m.slashTop
	bottom := m.lay.Region(regionOverlay).Bottom
	m = clickAt(t, m, bottom)
	if m.input.Value() != before || m.slashSel != sel || m.slashTop != top {
		t.Fatalf("a click past the band's last row changed something: draft %q sel %d top %d",
			m.input.Value(), m.slashSel, m.slashTop)
	}
}

// TestWheelOverTheBandMovesTheSlashSelection is §3.5's other gesture: one row
// per notch, clamped rather than wrapped like ↑/↓ — the menu is a selection,
// not a viewport, so wheelLines' three-at-a-time would jump past rows nobody
// saw highlighted, and a wrap would fling the highlight across the whole
// catalog on one notch too many.
func TestWheelOverTheBandMovesTheSlashSelection(t *testing.T) {
	m, _ := slashScrolled(t, 3)
	if m.slashSel != 3 {
		t.Fatalf("fixture: selection %d, want 3", m.slashSel)
	}
	y := m.lay.Region(regionOverlay).Top
	down := tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonWheelDown, Y: y}
	up := tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonWheelUp, Y: y}

	tm, _ := m.Update(down)
	m = tm.(Model)
	if m.slashSel != 4 {
		t.Fatalf("one notch down moved to %d, want 4", m.slashSel)
	}
	tm, _ = m.Update(up)
	m = tm.(Model)
	if m.slashSel != 3 {
		t.Fatalf("one notch up moved to %d, want 3", m.slashSel)
	}
	// Clamped, not wrapped: enough notches to run off either end stop at the
	// edge instead of wrapping the way ↑/↓ does.
	for i := 0; i < 10; i++ {
		tm, _ = m.Update(up)
		m = tm.(Model)
	}
	if m.slashSel != 0 {
		t.Fatalf("wheel up clamped at %d, want 0", m.slashSel)
	}
	items := m.filteredSlash()
	for i := 0; i < len(items)+5; i++ {
		tm, _ = m.Update(down)
		m = tm.(Model)
	}
	if m.slashSel != len(items)-1 {
		t.Fatalf("wheel down clamped at %d, want %d", m.slashSel, len(items)-1)
	}
}

// TestWheelOverTheTranscriptStillScrollsWithTheSlashMenuOpen: away from the
// band, the wheel is unchanged — the new branch in handleMouse must not steal
// the gesture just because a menu happens to be open somewhere else on screen.
func TestWheelOverTheTranscriptStillScrollsWithTheSlashMenuOpen(t *testing.T) {
	m := scrollable(t)
	m.snap.Commands = []agent.CommandInfo{{Name: "research", Description: "d"}}
	m = draft(m, "see /res")
	if !m.slashActive() {
		t.Fatal("fixture: the menu should be open")
	}
	if !m.vp.AtBottom() {
		t.Fatal("fixture: a fresh transcript sticks to the bottom")
	}
	bottom := m.vp.YOffset
	if bottom < wheelLines {
		t.Fatalf("not enough scrollback to test with: offset %d", bottom)
	}
	if !m.lay.Region(regionTranscript).Contains(1) {
		t.Fatal("fixture: y=1 should be inside the transcript, not the band")
	}
	m = wheel(t, m, tea.MouseButtonWheelUp)
	if got, want := m.vp.YOffset, bottom-wheelLines; got != want {
		t.Fatalf("wheel up over the transcript moved to %d, want %d (%d lines per notch)", got, want, wheelLines)
	}
	if m.slashSel != 0 {
		t.Fatalf("the wheel over the transcript must not touch the slash selection: %d", m.slashSel)
	}
}
