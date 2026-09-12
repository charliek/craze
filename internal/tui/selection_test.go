package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/charliek/craze/internal/agent"
)

// selModel is a 100x30 model with a transcript and an injected clock, which is
// what the double-click window is measured against.
func selModel(t *testing.T, now *time.Time, reply string) Model {
	t.Helper()
	isolateSkillsHome(t)
	stub := NewStub()
	stub.Clock = func() time.Time { return *now }
	m := New(Config{
		Session:   stub,
		Theme:     "tokyo-night",
		Workspace: t.TempDir(),
		Model:     "grok",
		Yolo:      true,
	})
	m.clock = func() time.Time { return *now }
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = tm.(Model)
	tm, _ = m.Update(startedMsg{})
	m = tm.(Model)
	tm, _ = m.Update(eventMsg{agent.Event{Type: agent.EventText, Text: reply}})
	m = tm.(Model)
	tm, _ = m.Update(eventMsg{agent.Event{Type: agent.EventDone, StopReason: "end_turn"}})
	return tm.(Model)
}

// captureCopies swaps the seam for a recorder. The copy runs on a command
// goroutine, so nothing here may be parallel.
func captureCopies(t *testing.T) *copyRecorder {
	t.Helper()
	rec, restore := recordCopies()
	t.Cleanup(restore)
	return rec
}

// press, motion and release are the three halves of a gesture as the terminal
// reports them.
func mousePress(t *testing.T, m Model, x, y int) Model {
	t.Helper()
	return mouse(t, m, tea.MouseMsg{X: x, Y: y, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
}

func motion(t *testing.T, m Model, x, y int) Model {
	t.Helper()
	return mouse(t, m, tea.MouseMsg{X: x, Y: y, Button: tea.MouseButtonLeft, Action: tea.MouseActionMotion})
}

// release reports ButtonNone, which is what an X10 terminal sends and what the
// tracked pressed button exists to cope with.
func release(t *testing.T, m Model, x, y int) Model {
	t.Helper()
	return mouse(t, m, tea.MouseMsg{X: x, Y: y, Button: tea.MouseButtonNone, Action: tea.MouseActionRelease})
}

func mouse(t *testing.T, m Model, msg tea.MouseMsg) Model {
	t.Helper()
	tm, cmd := m.Update(msg)
	next := tm.(Model)
	if msg := runCmd(cmd); msg != nil {
		if done, ok := msg.(clipboardDoneMsg); ok {
			tm, _ = next.Update(done)
			next = tm.(Model)
		}
	}
	return next
}

// drag is press, one motion and the release, the way <drag:> scripts it.
func drag(t *testing.T, m Model, x1, y1, x2, y2 int) Model {
	t.Helper()
	m = mousePress(t, m, x1, y1)
	m = motion(t, m, x2, y2)
	return release(t, m, x2, y2)
}

// twoRows is a reply the transcript draws on two consecutive rows: a bullet
// list, because a markdown paragraph would join the two lines into one.
const twoRows = "- alpha bravo\n- charlie delta"

// manyRows is a reply long enough to scroll the viewport, one row per item.
func manyRows(n int) string {
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, fmt.Sprintf("- line %02d", i))
	}
	return strings.Join(out, "\n")
}

// TestDragCopiesTheSelectedCells is the whole gesture: the seam sees exactly the
// plain text under the highlight, and the note counts the rows.
func TestDragCopiesTheSelectedCells(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	rec := captureCopies(t)
	m := selModel(t, &now, twoRows)
	if got := m.transcriptPlain[0]; got != "• alpha bravo" {
		t.Fatalf("transcript row 0 is %q", got)
	}
	top := m.lay.Region(regionTranscript).Top
	m = drag(t, m, 0, top, 6, top+1)

	// The whole of row 0, then row 1 up to and including the cell under the
	// release: the range is inclusive of both endpoints.
	want := "• alpha bravo\n• charl"
	if copies := rec.copies(); len(copies) != 1 || copies[0] != want {
		t.Fatalf("clipboard got %q, want [%q]", copies, want)
	}
	if !strings.Contains(plainView(m), "copied 2 lines") {
		t.Fatalf("missing the copy note:\n%s", plainView(m))
	}
	// The highlight is still up: a copy does not retire it.
	if m.sel.empty() {
		t.Fatal("the selection should survive the release")
	}
}

// TestReverseDragAcrossViewportsCopiesInReadingOrder drags up and to the left
// from a line the viewport has scrolled past, which is the case vp.View() could
// not answer: the anchor is off-screen, and the copy still comes out in reading
// order.
func TestReverseDragAcrossViewportsCopiesInReadingOrder(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	rec := captureCopies(t)
	m := selModel(t, &now, manyRows(60))
	if m.vp.YOffset == 0 {
		t.Fatal("the fixture should have scrolled")
	}
	tr := m.lay.Region(regionTranscript)
	// Press on the last visible row, release two rows above it and further
	// left: head is before anchor, so the range has to normalise.
	last := tr.Bottom - 1
	m = mousePress(t, m, 6, last)
	m = motion(t, m, 2, last-2)
	m = release(t, m, 2, last-2)

	first := m.vp.YOffset + (last - 2 - tr.Top)
	// From the head's column to the end of its row, the row between whole, and
	// the anchor's row up to and including the anchor cell.
	want := cutCells(m.transcriptPlain[first], 2, 100) + "\n" +
		m.transcriptPlain[first+1] + "\n" +
		cutCells(m.transcriptPlain[first+2], 0, 7)
	if copies := rec.copies(); len(copies) != 1 || copies[0] != want {
		t.Fatalf("clipboard got %q, want [%q]", copies, want)
	}
	if !strings.Contains(plainView(m), "copied 3 lines") {
		t.Fatalf("missing the note:\n%s", plainView(m))
	}
}

// TestSelectionSnapsOutwardOnWideRunes: a drag whose endpoints land in the
// middle of a double-width grapheme takes the whole character, because half of
// one is not a character.
func TestSelectionSnapsOutwardOnWideRunes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		lo, hi int
		want   string
	}{
		// "日本語" is cells 0-5. Starting inside 日 and ending inside 語 has to
		// take both whole.
		{"both edges", 1, 4, "日本語"},
		{"left edge", 1, 1, "日"},
		{"clean", 2, 3, "本"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := cutCells("日本語", tc.lo, tc.hi+1); got != tc.want {
				t.Fatalf("cells [%d,%d] = %q, want %q", tc.lo, tc.hi, got, tc.want)
			}
		})
	}
}

// TestDragAtTheEdgeAutoScrolls: a motion report on the top row of the band
// scrolls one line and the selection keeps growing.
func TestDragAtTheEdgeAutoScrolls(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	captureCopies(t)
	m := selModel(t, &now, manyRows(60))
	tr := m.lay.Region(regionTranscript)
	start := m.vp.YOffset

	m = mousePress(t, m, 4, tr.Top+3)
	m = motion(t, m, 0, tr.Top)
	if m.vp.YOffset != start-1 {
		t.Fatalf("a motion on the top row scrolled to %d, want %d", m.vp.YOffset, start-1)
	}
	if m.sel.head.line != m.vp.YOffset {
		t.Fatalf("the head is line %d, want the new top line %d", m.sel.head.line, m.vp.YOffset)
	}
	// Another report on the same row scrolls one more line.
	m = motion(t, m, 0, tr.Top)
	if m.vp.YOffset != start-2 {
		t.Fatalf("the second motion scrolled to %d, want %d", m.vp.YOffset, start-2)
	}

	// Downwards, from a scrolled position, the bottom row scrolls the other way.
	m = motion(t, m, 0, tr.Bottom-1)
	if m.vp.YOffset != start-1 {
		t.Fatalf("a motion on the bottom row scrolled to %d, want %d", m.vp.YOffset, start-1)
	}
}

// TestPressOnAnotherBandStartsNoSelection: only the transcript is selectable.
// The tasks header keeps its click, and the press leaves no highlight behind.
func TestPressOnAnotherBandStartsNoSelection(t *testing.T) {
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

	next := mousePress(t, m, 1, m.lay.Region(regionTasks).Top)
	if next.tasksState != tasksExpanded {
		t.Fatalf("the header click was swallowed, state %v", next.tasksState)
	}
	if next.sel.on {
		t.Fatal("a press outside the transcript must not start a selection")
	}
	// The status rows and the composer are the same: no selection, no copy.
	for _, y := range []int{m.lay.Region(regionStatus).Top, m.lay.Region(regionComposer).Top} {
		if got := mousePress(t, m, 1, y); got.sel.on {
			t.Fatalf("a press on row %d started a selection", y)
		}
	}
}

// TestReleaseWithButtonNoneFinalises: X10 terminals report no button on the
// release, so the button that went down is what finalises the drag.
func TestReleaseWithButtonNoneFinalises(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	rec := captureCopies(t)
	m := selModel(t, &now, "alpha bravo")
	top := m.lay.Region(regionTranscript).Top
	m = mousePress(t, m, 0, top)
	m = release(t, m, 4, top)
	if copies := rec.copies(); len(copies) != 1 || copies[0] != "alpha" {
		t.Fatalf("a ButtonNone release copied %q", copies)
	}
	if m.pressed != tea.MouseButtonNone {
		t.Fatalf("the pressed button survived the release: %v", m.pressed)
	}
	// A second release with nothing pressed is not a second copy.
	m = release(t, m, 8, top)
	if copies := rec.copies(); len(copies) != 1 {
		t.Fatalf("a release with no press copied again: %q", copies)
	}
}

// TestPlainClickCopiesNothing: press and release on one cell is a click, and a
// click is not a selection.
func TestPlainClickCopiesNothing(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	rec := captureCopies(t)
	m := selModel(t, &now, "alpha bravo")
	top := m.lay.Region(regionTranscript).Top
	m = mousePress(t, m, 3, top)
	m = release(t, m, 3, top)
	if copies := rec.copies(); len(copies) != 0 {
		t.Fatalf("a click copied %q", copies)
	}
	if !m.sel.empty() {
		t.Fatal("a click leaves no highlight")
	}
}

// TestDoubleClickSelectsAWord drives the state machine on the injected clock:
// inside the window on the same cell picks the word, outside it does not, a
// different cell does not, and a third press starts over.
func TestDoubleClickSelectsAWord(t *testing.T) {
	base := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		gap  time.Duration
		dx   int
		want string
	}{
		{"fast, same cell", 100 * time.Millisecond, 0, "bravo"},
		{"slow, same cell", 500 * time.Millisecond, 0, ""},
		{"fast, different cell", 100 * time.Millisecond, 4, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := base
			rec := captureCopies(t)
			m := selModel(t, &now, "alpha bravo charlie")
			top := m.lay.Region(regionTranscript).Top
			// Cell 8 is inside "bravo" (cells 6-10).
			m = mousePress(t, m, 8, top)
			m = release(t, m, 8, top)
			now = now.Add(tc.gap)
			m = mousePress(t, m, 8+tc.dx, top)

			if tc.want == "" {
				if !m.sel.empty() {
					t.Fatalf("expected no word selection, got %+v", m.sel)
				}
				if copies := rec.copies(); len(copies) != 0 {
					t.Fatalf("copied %q", copies)
				}
				return
			}
			if got := m.selectionText(); got != tc.want {
				t.Fatalf("double-click selected %q, want %q", got, tc.want)
			}
			// The press selects, the release copies — once, not twice, and the
			// release must not pull the head back to the cell under it.
			if copies := rec.copies(); len(copies) != 0 {
				t.Fatalf("the press copied before the release: %q", copies)
			}
			m = release(t, m, 8, top)
			if copies := rec.copies(); len(copies) != 1 || copies[0] != tc.want {
				t.Fatalf("clipboard got %q, want [%q]", copies, tc.want)
			}
			if got := m.selectionText(); got != tc.want {
				t.Fatalf("the release shrank the word to %q", got)
			}

			// A third press inside the window starts over rather than
			// re-selecting the word.
			now = now.Add(100 * time.Millisecond)
			m = mousePress(t, m, 8, top)
			if !m.sel.empty() {
				t.Fatalf("a third press should restart, got %+v", m.sel)
			}
		})
	}
}

// TestDoubleClickOnWhitespaceSelectsNothing: there is no word under a blank.
func TestDoubleClickOnWhitespaceSelectsNothing(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	captureCopies(t)
	m := selModel(t, &now, "alpha bravo")
	top := m.lay.Region(regionTranscript).Top
	m = mousePress(t, m, 5, top)
	m = release(t, m, 5, top)
	now = now.Add(100 * time.Millisecond)
	m = mousePress(t, m, 5, top)
	if !m.sel.empty() {
		t.Fatalf("whitespace has no word: %+v", m.sel)
	}
}

// TestDblClickMsgMatchesTheGesture: the frame runner's token does exactly what
// two real presses do, so a golden does not have to depend on the clock.
func TestDblClickMsgMatchesTheGesture(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	rec := captureCopies(t)
	m := selModel(t, &now, "alpha bravo charlie")
	top := m.lay.Region(regionTranscript).Top
	tm, cmd := m.Update(dblClickMsg{X: 8, Y: top})
	m = tm.(Model)
	if msg := runCmd(cmd); msg != nil {
		tm, _ = m.Update(msg)
		m = tm.(Model)
	}
	if got := m.selectionText(); got != "bravo" {
		t.Fatalf("<dblclick:> selected %q", got)
	}
	if copies := rec.copies(); len(copies) != 1 || copies[0] != "bravo" {
		t.Fatalf("clipboard got %q", copies)
	}
	if !strings.Contains(plainView(m), `copied "bravo"`) {
		t.Fatalf("a one-line copy quotes what it took:\n%s", plainView(m))
	}
}

// TestCtrlYCopiesTheLastReply: with no selection Ctrl+Y takes the reply's own
// text, not the rows it was wrapped into, and it works with --no-mouse because
// it is a keyboard feature.
func TestCtrlYCopiesTheLastReply(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	// A reply long enough that the transcript had to wrap it: the copy must not
	// carry the wrap.
	reply := strings.Repeat("wrapped words ", 20)
	rec := captureCopies(t)
	m := selModel(t, &now, reply)
	m.mouseEnabled = false
	if len(m.transcriptPlain) < 3 {
		t.Fatalf("the fixture should have wrapped: %d rows", len(m.transcriptPlain))
	}
	tm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlY})
	m = tm.(Model)
	if msg := runCmd(cmd); msg != nil {
		tm, _ = m.Update(msg)
		m = tm.(Model)
	}
	if copies := rec.copies(); len(copies) != 1 || copies[0] != reply {
		t.Fatalf("Ctrl+Y copied %q, want the raw reply", copies)
	}
	if !strings.Contains(plainView(m), "copied last reply") {
		t.Fatalf("missing the note:\n%s", plainView(m))
	}
}

// TestCtrlYCopiesTheSelectionAndKeepsIt: with a selection Ctrl+Y copies that,
// and it is the one key that does not retire the highlight.
func TestCtrlYCopiesTheSelectionAndKeepsIt(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	rec := captureCopies(t)
	m := selModel(t, &now, "alpha bravo")
	top := m.lay.Region(regionTranscript).Top
	m = drag(t, m, 0, top, 4, top)
	tm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlY})
	m = tm.(Model)
	if msg := runCmd(cmd); msg != nil {
		tm, _ = m.Update(msg)
		m = tm.(Model)
	}
	if copies := rec.copies(); len(copies) != 2 || copies[1] != "alpha" {
		t.Fatalf("Ctrl+Y copied %q", copies)
	}
	if m.sel.empty() {
		t.Fatal("Ctrl+Y is the one key that keeps the highlight")
	}
	// Any other key retires it.
	tm, _ = m.Update(runeKey('x'))
	if !tm.(Model).sel.empty() {
		t.Fatal("a key should retire the highlight")
	}
}

// TestNoMouseIgnoresInjectedMouseMessages: --no-mouse is kept on the model, so
// a mouse message that reached craze anyway does nothing at all.
func TestNoMouseIgnoresInjectedMouseMessages(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	rec := captureCopies(t)
	m := selModel(t, &now, manyRows(60))
	m.mouseEnabled = false
	offset := m.vp.YOffset
	top := m.lay.Region(regionTranscript).Top

	m = drag(t, m, 0, top, 6, top+1)
	if m.sel.on {
		t.Fatal("--no-mouse must not select")
	}
	if copies := rec.copies(); len(copies) != 0 {
		t.Fatalf("--no-mouse copied %q", copies)
	}
	m = mouse(t, m, tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonWheelUp, X: 1, Y: top})
	if m.vp.YOffset != offset {
		t.Fatalf("--no-mouse scrolled to %d, want %d", m.vp.YOffset, offset)
	}
}

// TestCardArrivalDiscardsTheDrag: a card owns the mouse, so the gesture under
// way is dropped and the release that follows it does nothing.
func TestCardArrivalDiscardsTheDrag(t *testing.T) {
	m, stub := sizedCards(t)
	rec := captureCopies(t)
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = tm.(Model)
	tm, _ = m.Update(eventMsg{agent.Event{Type: agent.EventText, Text: twoRows}})
	m = tm.(Model)
	top := m.lay.Region(regionTranscript).Top
	m = mousePress(t, m, 0, top)
	m = motion(t, m, 6, top+1)
	if m.sel.empty() {
		t.Fatal("the drag should be live")
	}
	m = cardEvent(t, m, stub, agent.Event{Type: agent.EventQuestion, Question: stubQuestion()})
	if m.sel.on || m.pressed != tea.MouseButtonNone {
		t.Fatalf("the card should have dropped the drag: %+v %v", m.sel, m.pressed)
	}
	m = release(t, m, 6, top+1)
	if copies := rec.copies(); len(copies) != 0 {
		t.Fatalf("the release after a card copied %q", copies)
	}
}

// TestTranscriptChangeClearsTheSelection: the rows under a highlight moved, so
// the highlight goes rather than pointing at text that is no longer there.
func TestTranscriptChangeClearsTheSelection(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	captureCopies(t)
	for _, tc := range []struct {
		name string
		poke func(Model) Model
	}{
		{"streaming chunk", func(m Model) Model {
			tm, _ := m.Update(eventMsg{agent.Event{Type: agent.EventText, Text: "more"}})
			return tm.(Model)
		}},
		{"resize", func(m Model) Model {
			tm, _ := m.Update(tea.WindowSizeMsg{Width: 90, Height: 28})
			return tm.(Model)
		}},
		{"clear", func(m Model) Model {
			m.clearTranscript()
			return m
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := selModel(t, &now, twoRows)
			top := m.lay.Region(regionTranscript).Top
			m = mousePress(t, m, 0, top)
			m = motion(t, m, 6, top+1)
			if m.sel.empty() {
				t.Fatal("the drag should be live")
			}
			if got := tc.poke(m); !got.sel.empty() {
				t.Fatalf("%s kept the selection: %+v", tc.name, got.sel)
			}
		})
	}
}

// TestSelectingDoesNotReRenderTheTranscript: the highlight is applied over the
// cached rows at View() time, so no entry is rendered again.
func TestSelectingDoesNotReRenderTheTranscript(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	captureCopies(t)
	m := selModel(t, &now, twoRows)
	top := m.lay.Region(regionTranscript).Top
	before := m.renders
	m = mousePress(t, m, 0, top)
	m = motion(t, m, 6, top+1)
	_ = m.View()
	if m.renders != before {
		t.Fatalf("selecting re-rendered %d entries", m.renders-before)
	}
	// And the highlight really is on the screen.
	if !strings.Contains(m.View(), selectionSeq(m.theme.SelectionBG)) {
		t.Fatal("the selection background is missing from the frame")
	}
}

// selectedCells reports, per display cell, whether the selection background was
// the last sequence emitted before it — which is the only thing that makes it
// the background in force, whatever the row had of its own.
func selectedCells(s, bg string) []bool {
	var out []bool
	on := false
	walkANSI(s, func(chunk string, esc bool) {
		if esc {
			on = chunk == bg
			return
		}
		for i, w := 0, max(ansi.StringWidth(chunk), 1); i < w; i++ {
			out = append(out, on)
		}
	})
	return out
}

// TestHighlightSpanPaintsExactlyTheSpan is the highlight's contract: the row
// keeps its width and its text, every cell of the span carries the selection
// background — through a reset, a second styled run or the row's own
// background — and no cell outside it does.
func TestHighlightSpanPaintsExactlyTheSpan(t *testing.T) {
	th := Preset("tokyo-night")
	bg := selectionSeq(th.SelectionBG)
	if bg == "" {
		t.Skip("no colour profile")
	}
	red := lipgloss.NewStyle().Foreground(lipgloss.Color("#ff0000"))
	diff := lipgloss.NewStyle().Foreground(th.DiffAdd).Background(th.DiffAddBG)
	link := "\x1b]8;;https://example.com\x1b\\linked\x1b]8;;\x1b\\ text"
	for _, tc := range []struct {
		name   string
		line   string
		lo, hi int
	}{
		{"plain", "alpha bravo charlie", 6, 10},
		// Two styled runs, so the reset between them falls inside the span.
		{"two styled runs", red.Render("alpha") + " " + red.Render("bravo"), 0, 10},
		{"whole row", red.Render("alpha bravo"), 0, 10},
		// A diff row carries a background of its own; the selection's has to win.
		{"diff row", diff.Render("+     fmt.Println(\"hi\")"), 2, 12},
		{"osc-8 link", link, 0, 5},
		{"wide runes", "日本語 text", 2, 5},
		{"past the end", "alpha", 0, 20},
		{"one cell", "alpha bravo", 3, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := lipgloss.Width(tc.line)
			got := highlightSpan(tc.line, tc.lo, tc.hi, bg)
			if w := lipgloss.Width(got); w != want {
				t.Fatalf("width %d, want %d: %q", w, want, got)
			}
			if plain(got) != plain(tc.line) {
				t.Fatalf("text changed: %q vs %q", plain(got), plain(tc.line))
			}
			cells := selectedCells(got, bg)
			if len(cells) != want {
				t.Fatalf("walked %d cells, want %d", len(cells), want)
			}
			for x, on := range cells {
				inSpan := x >= tc.lo && x <= tc.hi
				if on != inSpan {
					t.Fatalf("cell %d selected=%v, want %v (span %d..%d) in %q",
						x, on, inSpan, tc.lo, tc.hi, got)
				}
			}
		})
	}
}

// TestWordAt is the double-click's idea of a word: a run of non-space cells.
func TestWordAt(t *testing.T) {
	const line = "  alpha bravo  charlie"
	for _, tc := range []struct {
		col    int
		lo, hi int
		ok     bool
	}{
		{0, 0, 0, false},
		{2, 2, 6, true},
		{6, 2, 6, true},
		{7, 0, 0, false},
		{8, 8, 12, true},
		{15, 15, 21, true},
		{100, 0, 0, false},
	} {
		lo, hi, ok := wordAt(line, tc.col)
		if ok != tc.ok || (ok && (lo != tc.lo || hi != tc.hi)) {
			t.Fatalf("wordAt(%d) = %d,%d,%v; want %d,%d,%v", tc.col, lo, hi, ok, tc.lo, tc.hi, tc.ok)
		}
	}
}
