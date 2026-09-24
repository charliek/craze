package tui

import (
	"reflect"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/charliek/craze/internal/agent"
)

// rows splits a frame and is the shorthand every layer test below reads with.
func rows(s string) []string { return strings.Split(s, "\n") }

// cellAt is the one display cell at x of an already-plain row.
func cellAt(line string, x int) string { return ansi.Cut(line, x, x+1) }

// baseFrame is n rows of w cells, each one a run of the same digit, so a splice
// that lands one cell out is obvious.
func baseFrame(w, n int) string {
	out := make([]string, n)
	for i := range out {
		out[i] = strings.Repeat(string(rune('0'+i%10)), w)
	}
	return strings.Join(out, "\n")
}

func boxOf(w, h int) string {
	out := make([]string, h)
	for i := range out {
		out[i] = strings.Repeat("#", w)
	}
	return strings.Join(out, "\n")
}

func TestOverlayPlacesTheBoxExactly(t *testing.T) {
	got := rows(overlay(baseFrame(20, 5), boxOf(6, 2), rect{X: 7, Y: 1, W: 6, H: 2}))
	want := []string{
		"00000000000000000000",
		"1111111######1111111",
		"2222222######2222222",
		"33333333333333333333",
		"44444444444444444444",
	}
	for i, w := range want {
		if plain(got[i]) != w {
			t.Fatalf("row %d = %q, want %q", i, plain(got[i]), w)
		}
	}
}

// TestOverlayKeepsEveryRowsWidth is the invariant the height contract rests on:
// a spliced row owes the frame exactly as many cells as the row it replaced.
func TestOverlayKeepsEveryRowsWidth(t *testing.T) {
	base := baseFrame(20, 4)
	for _, r := range []rect{
		{X: 0, Y: 0, W: 6, H: 2},  // flush left
		{X: 14, Y: 1, W: 6, H: 2}, // flush right
		{X: 7, Y: 0, W: 6, H: 4},  // every row
		{X: 18, Y: 0, W: 6, H: 1}, // past the right edge
		{X: 7, Y: 3, W: 6, H: 3},  // past the bottom
		{X: 0, Y: 0, W: 20, H: 4}, // the whole frame
	} {
		box := boxOf(r.W, r.H)
		for i, ln := range rows(overlay(base, box, r)) {
			if w := ansi.StringWidth(ln); w != 20 {
				t.Fatalf("%+v: row %d is %d cells, want 20 (%q)", r, i, w, plain(ln))
			}
		}
	}
}

// TestOverlayRestoresTheBaseStyle: the box's own colours stop at its right
// edge, and whatever the base line was painting resumes after it.
func TestOverlayRestoresTheBaseStyle(t *testing.T) {
	base := styleFG("#ff0000").Render("aaaaaaaaaaaaaaaaaaaa")
	box := styleFG("#00ff00").Render("####")
	out := overlay(base, box, rect{X: 8, Y: 0, W: 4, H: 1})
	if plain(out) != "aaaaaaaa####aaaaaaaa" {
		t.Fatalf("text %q", plain(out))
	}
	tail := out[strings.LastIndex(out, "####")+len("####"):]
	if !strings.Contains(tail, ansiFG("#ff0000")) {
		t.Fatalf("the base style was not re-emitted after the box: %q", tail)
	}
	if strings.Contains(tail, ansiFG("#00ff00")) {
		t.Fatalf("the box style ran on past the box: %q", tail)
	}
}

// TestOverlaySplicesInsideResets: a base line that resets in the middle, and a
// box that does too, still come out the right width with the right text.
func TestOverlaySplicesInsideResets(t *testing.T) {
	base := styleFG("#ff0000").Render("aaaa") + "bbbb" + styleFG("#0000ff").Render("cccc") + "dddd"
	box := styleFG("#00ff00").Render("##") + "@@"
	out := overlay(base, box, rect{X: 6, Y: 0, W: 4, H: 1})
	if plain(out) != "aaaabb##@@ccdddd" {
		t.Fatalf("text %q", plain(out))
	}
	if w := ansi.StringWidth(out); w != 16 {
		t.Fatalf("width %d, want 16", w)
	}
}

// TestOverlayWideGraphemesAtBothEdges: half a wide character is not a
// character, so the cell it half-filled becomes a blank and the row keeps its
// width either way.
func TestOverlayWideGraphemesAtBothEdges(t *testing.T) {
	// Four double-width cells, so every odd column splits one in half.
	base := "日本語で"
	if w := ansi.StringWidth(base); w != 8 {
		t.Fatalf("fixture is %d cells", w)
	}
	for _, tc := range []struct{ x, w int }{{1, 4}, {2, 3}, {3, 4}, {0, 3}} {
		out := overlay(base, boxOf(tc.w, 1), rect{X: tc.x, Y: 0, W: tc.w, H: 1})
		if got := ansi.StringWidth(out); got != 8 {
			t.Fatalf("x=%d w=%d: %d cells, want 8 (%q)", tc.x, tc.w, got, plain(out))
		}
		if !strings.Contains(plain(out), strings.Repeat("#", tc.w)) {
			t.Fatalf("x=%d w=%d: the box did not land whole: %q", tc.x, tc.w, plain(out))
		}
	}
}

// TestDialogLayerOwnsItsRect is TestEveryDrawnRegionOwnsItsRows for the layer:
// the box is drawn exactly where frameLayout put it, it is inside the
// transcript region at every size craze draws, and no band under it lost a row.
func TestDialogLayerOwnsItsRect(t *testing.T) {
	for _, size := range [][2]int{{100, 30}, {80, 24}, {40, 12}, {60, 14}} {
		m := sized(t)
		tm, _ := m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		m = tm.(Model)
		m = m.openModelDialog()
		tm, _ = m.Update(refreshSnapMsg{})
		m = tm.(Model)

		r := m.lay.Dialog
		tr := m.lay.Region(regionTranscript)
		if r.Empty() {
			t.Fatalf("%v: no dialog rectangle: %+v", size, m.lay)
		}
		if r.Y < tr.Top || r.Y+r.H > tr.Bottom {
			t.Fatalf("%v: the box %+v is not inside the transcript %+v", size, r, tr)
		}
		if r.X < 0 || r.X+r.W > size[0] {
			t.Fatalf("%v: the box %+v is not inside the frame", size, r)
		}
		lines := rows(plainView(m))
		if len(lines) != size[1] {
			t.Fatalf("%v: frame is %d rows", size, len(lines))
		}
		// The box's corners appear on no row outside the rect, and every row
		// inside it opens and closes with the border at the rect's own columns.
		// (The status rows draw a "│" of their own, so only the corners can
		// be searched for across a whole line.)
		for y, ln := range lines {
			inside := y >= r.Y && y < r.Y+r.H
			if strings.ContainsAny(ln, "╭╮╰╯") && !inside {
				t.Fatalf("%v: row %d is outside the rect and has a box corner:\n%s", size, y, plainView(m))
			}
			if !inside {
				continue
			}
			wantL, wantR := "│", "│"
			switch y {
			case r.Y:
				wantL, wantR = "╭", "╮"
			case r.Y + r.H - 1:
				wantL, wantR = "╰", "╯"
			}
			if l := cellAt(ln, r.X); l != wantL {
				t.Fatalf("%v: row %d starts %q at x=%d, want %q:\n%s", size, y, l, r.X, wantL, plainView(m))
			}
			if rr := cellAt(ln, r.X+r.W-1); rr != wantR {
				t.Fatalf("%v: row %d ends %q at x=%d, want %q:\n%s", size, y, rr, r.X+r.W-1, wantR, plainView(m))
			}
		}
		// The status rows are the band directly under the transcript, and the
		// one a box that overflowed would eat first.
		if !strings.Contains(lines[m.lay.Region(regionStatus).Top], "cursor") {
			t.Fatalf("%v: status row 1 was covered:\n%s", size, plainView(m))
		}
	}
}

// TestDialogClosesOnCardArrival: a card outranks the layer, and the model
// dialog leaves without applying anything.
func TestDialogClosesOnCardArrival(t *testing.T) {
	m, stub := sizedCards(t)
	m = m.openModelDialog()
	m = pressKey(t, m, tea.KeyDown)
	m = cardEvent(t, m, stub, agent.Event{Type: agent.EventQuestion, Question: stubQuestion()})
	if m.dialog != dialogNone {
		t.Fatalf("a card closes the model dialog, dialog=%v", m.dialog)
	}
	if m.snap.CurrentModel != "grok" {
		t.Fatalf("the card applied the dialog's selection: %q", m.snap.CurrentModel)
	}
	if !m.lay.Dialog.Empty() {
		t.Fatalf("the layer kept its rectangle: %+v", m.lay.Dialog)
	}
}

// TestThemeDialogOutsideClickReverts: a press outside the box is Esc, preview
// and all.
func TestThemeDialogOutsideClickReverts(t *testing.T) {
	m := sized(t)
	m = m.openThemePicker()
	tm, _ := m.Update(refreshSnapMsg{})
	m = tm.(Model)
	m = pressKey(t, m, tea.KeyDown)
	if m.theme.Name == "tokyo-night" {
		t.Fatal("moving the cursor should have previewed another theme")
	}
	out := clickXY(t, m, 0, m.lay.Dialog.Y)
	if out.dialog != dialogNone {
		t.Fatal("a press outside closes the theme dialog")
	}
	if out.theme.Name != "tokyo-night" {
		t.Fatalf("a press outside reverts like esc, theme is %q", out.theme.Name)
	}
	if notes := texts(out, entryNote); len(notes) != 0 {
		t.Fatalf("a reverted preview leaves no note: %v", notes)
	}
}

// TestThemeDialogClickKeeps is the other half: a press on a name is Enter on it.
func TestThemeDialogClickKeeps(t *testing.T) {
	m := sized(t)
	m = m.openThemePicker()
	tm, _ := m.Update(refreshSnapMsg{})
	m = tm.(Model)
	r := m.lay.Dialog
	// Row 0 is the border and row 1 the title, so row 2 is the first name.
	out := clickXY(t, m, r.X+2, r.Y+3)
	if out.dialog != dialogNone {
		t.Fatal("picking a name closes the dialog")
	}
	if out.theme.Name != m.themeNames[1] {
		t.Fatalf("clicked %q, theme is %q", m.themeNames[1], out.theme.Name)
	}
}

// TestFastOnSendsTheAdvertisedValue: the dialog's "on" is whatever the agent
// called the value that is not "Off" — the string "true" in every capture.
func TestFastOnSendsTheAdvertisedValue(t *testing.T) {
	m := sized(t)
	m = m.openModelDialog()
	m = pressKey(t, m, tea.KeyTab)
	m = pressKey(t, m, tea.KeyTab)
	if m.mdlg.focus != dialogFocus("fast") {
		t.Fatalf("focus %v", m.mdlg.focus)
	}
	m = pressKey(t, m, tea.KeyRight)
	if m.mdlg.chosen["fast"] != "true" {
		t.Fatalf("the on value is %q, want the advertised \"true\"", m.mdlg.chosen["fast"])
	}
	tm, cmd := m.Update(enter())
	m = flushCmd(t, tm.(Model), cmd)
	if got := agent.FastOption(stubOf(t, m).Snapshot()); got == nil || got.Current != "true" {
		t.Fatalf("the agent was sent %+v", got)
	}
	if got := texts(m, entryNote); len(got) != 1 || got[0] != "fast → on" {
		t.Fatalf("notes %v", got)
	}
}

// TestOverlayClosesTheBoxStyleWithoutASuffix is the right-edge leak: with
// nothing after the box there was no reset either, so a box that painted its
// last cell painted every row after it too.
func TestOverlayClosesTheBoxStyleWithoutASuffix(t *testing.T) {
	out := overlay("abcd\nEFGH", "\x1b[31mXX", rect{X: 2, Y: 0, W: 2, H: 1})
	first, second := rows(out)[0], rows(out)[1]
	if plain(first) != "abXX" || second != "EFGH" {
		t.Fatalf("rows %q and %q", plain(first), second)
	}
	if !strings.HasSuffix(first, ansi.ResetStyle) {
		t.Fatalf("the box's styling ran off the end of the row: %q", first)
	}
}

// TestSpliceRowEdgeBlanksKeepTheBaseBackground: half a wide character becomes a
// blank, and the blank belongs to the base line, so it is drawn in the base's
// own colours and not in the terminal's default.
func TestSpliceRowEdgeBlanksKeepTheBaseBackground(t *testing.T) {
	const red = "\x1b[41m"
	base := red + "日本語で" + "\x1b[0m"
	// Right edge: the splice cuts 本 in half, so the cell after the box is a
	// blank that still owes the row a red background.
	out := spliceRow(base, "###", 0, 3)
	if w := ansi.StringWidth(out); w != 8 {
		t.Fatalf("row is %d cells: %q", w, plain(out))
	}
	if !strings.Contains(out, red+" ") {
		t.Fatalf("the right-edge blank lost the base background: %q", out)
	}
	// Left edge: the same cut on the other side, where the blank is the last
	// cell before the box.
	out = spliceRow(base, "###", 3, 3)
	if w := ansi.StringWidth(out); w != 8 {
		t.Fatalf("row is %d cells: %q", w, plain(out))
	}
	if !strings.Contains(out, "日 ") {
		t.Fatalf("the left-edge blank lost the base background: %q", out)
	}
}

// TestModelDialogDrawsEveryRowAsOneLine: a model name with a newline in it drew
// two rows, so the box was a row taller than the rectangle the layout measured
// — the overlay dropped the bottom border and a click landed a row out.
func TestModelDialogDrawsEveryRowAsOneLine(t *testing.T) {
	m := sized(t)
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = tm.(Model)
	stub := stubOf(t, m)
	// Agent sanitisation keeps \n: it is a line break in a reply, and only the
	// rows that must not wrap fold it away.
	stub.snap.Models = []agent.ModelInfo{{ID: "a", Name: "A\nB"}, {ID: "b", Name: "C"}}
	stub.snap.CurrentModel = "a"
	// A toggle row's values are the agent's text too, and the row has the same
	// one line to fit in.
	stub.snap.Config = []agent.ConfigOption{{
		ID: "effort", Name: "Effort", Category: "thought_level", Type: "select", Current: "low",
		SelectValues: []agent.SelectValue{{Value: "low", Name: "Low"}, {Value: "hi\ngh", Name: "High"}},
	}}
	tm, _ = m.Update(refreshSnapMsg{})
	m = tm.(Model)
	m = m.openModelDialog()
	tm, _ = m.Update(refreshSnapMsg{})
	m = tm.(Model)

	r := m.lay.Dialog
	if h := lipgloss.Height(m.dialogView(r)); h != r.H {
		t.Fatalf("the box drew %d rows into a %d-row rectangle", h, r.H)
	}
	lines := rows(plainView(m))
	if len(lines) != 30 {
		t.Fatalf("frame is %d rows", len(lines))
	}
	if c := cellAt(lines[r.Y+r.H-1], r.X); c != "╰" {
		t.Fatalf("the bottom border is %q, not ╰:\n%s", c, plainView(m))
	}
	if !strings.Contains(lines[r.Y+3], "A B") {
		t.Fatalf("the name should be folded onto its own row:\n%s", plainView(m))
	}
	// The row under it is model b's, and it is the row a click there applies.
	if !strings.Contains(lines[r.Y+4], "C") {
		t.Fatalf("row %d should show model b:\n%s", r.Y+4, plainView(m))
	}
	out := clickXY(t, m, r.X+2, r.Y+4)
	if out.snap.CurrentModel != "b" {
		t.Fatalf("the click applied %q, but the row shows model b", out.snap.CurrentModel)
	}
}

// TestFastRowReadsWhatTheValuesMean is finding 7 in the dialog: on and off are
// read off the values' names and spellings, never off their order, so an agent
// that renames or shortens its list cannot invert the row.
func TestFastRowReadsWhatTheValuesMean(t *testing.T) {
	for _, tc := range []struct {
		name   string
		values []agent.SelectValue
		want   string
	}{
		{
			"renamed and reversed",
			[]agent.SelectValue{{Value: "true", Name: "Turbo"}, {Value: "false", Name: "Disabled"}},
			"fast  [on]  off",
		},
		{
			"only the on value advertised",
			[]agent.SelectValue{{Value: "true", Name: "Fast"}},
			"fast  [on]",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := sized(t)
			stub := stubOf(t, m)
			stub.snap.Config = []agent.ConfigOption{{
				ID: "fast", Name: "Fast", Category: "model_config", Type: "select",
				Current: "true", SelectValues: tc.values,
			}}
			tm, _ := m.Update(refreshSnapMsg{})
			m = tm.(Model)
			m = m.openModelDialog()
			tm, _ = m.Update(refreshSnapMsg{})
			m = tm.(Model)
			view := plainView(m)
			if !strings.Contains(view, tc.want) {
				t.Fatalf("want the row %q:\n%s", tc.want, view)
			}
			// The status row names fast only when it is on, which is the same
			// reading of the same values.
			if !strings.Contains(view, "(fast)") {
				t.Fatalf("status row 1 should name fast:\n%s", view)
			}
		})
	}
}

// dialogGutterAt is the two cells a dialog row opens with, read off a drawn
// frame with the escapes stripped. It is where the focus mark lives.
func dialogGutterAt(m Model, view string, y int) string {
	r := m.lay.Dialog
	return ansi.Cut(rows(view)[y], r.X+1, r.X+3)
}

// markedDialogRows is every content row of the box carrying the "> " cursor
// gutter. Exactly one of them is the whole point: it is the answer to "which
// row do the arrows move", and before this change there was none.
func markedDialogRows(m Model) []string {
	r := m.lay.Dialog
	view := plainView(m)
	var out []string
	for y := r.Y + 1; y < r.Y+r.H-1; y++ {
		if dialogGutterAt(m, view, y) == dialogCursorMark {
			// The text after the gutter, which is what the row says it is.
			out = append(out, strings.TrimRight(ansi.Cut(rows(view)[y], r.X+3, r.X+r.W-1), " "))
		}
	}
	return out
}

// TestModelDialogFocusSurvivesAnANSIStrip is the regression this task exists
// for: Tab moved the focus and the only cue was the label's colour, so the
// stripped frame — which is exactly what the goldens and a diff review see —
// came out byte-identical for all three focus states. The gutter is the cue
// now, one row carries it at a time, and the list gives up its cursor mark and
// its band while a toggle row has the keys.
func TestModelDialogFocusSurvivesAnANSIStrip(t *testing.T) {
	m := sized(t)
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = tm.(Model)
	m = m.openModelDialog()
	tm, _ = m.Update(refreshSnapMsg{})
	m = tm.(Model)

	seen := map[string]string{}
	for _, tc := range []struct {
		name, want string
		tabs       int
	}{
		{"list", "Grok", 0},
		{"effort", "effort  low  [medium]  high", 1},
		{"fast", "fast  [off]  on", 2},
	} {
		f := m
		for i := 0; i < tc.tabs; i++ {
			f = pressKey(t, f, tea.KeyTab)
		}
		marked := markedDialogRows(f)
		if len(marked) != 1 {
			t.Fatalf("focus %s marks %d rows, want exactly one: %q\n%s", tc.name, len(marked), marked, plainView(f))
		}
		if !strings.HasPrefix(marked[0], tc.want) {
			t.Fatalf("focus %s marks %q, want %q", tc.name, marked[0], tc.want)
		}
		// Three focus states, three different frames. The bug was that they
		// were one frame three times.
		view := plainView(f)
		if prev, dup := seen[view]; dup {
			t.Fatalf("focus %s draws the same frame as %s:\n%s", tc.name, prev, view)
		}
		seen[view] = tc.name
		// The selected model is still marked as the selection, just not as the
		// focus: "[value]" and the gutter answer two different questions.
		gutter := dialogGutterAt(f, view, f.lay.Dialog.Y+3)
		if tc.tabs == 0 && gutter != dialogCursorMark {
			t.Fatalf("the focused list should carry the cursor, got %q", gutter)
		}
		if tc.tabs > 0 && gutter != dialogSelMark {
			t.Fatalf("an unfocused list should keep its selection mark, got %q", gutter)
		}
	}
}

// TestModelDialogFocusPaintsTheFocusedRow is the styling half, the way
// status_test.go asserts chip colours: SelectionBG reinforces the gutter on a
// real terminal, and a change that keeps the marks but drops the paint (or
// paints two rows at once) fails here rather than silently.
func TestModelDialogFocusPaintsTheFocusedRow(t *testing.T) {
	m := themeModel(t, "craze-dark")
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = tm.(Model)
	m = m.openModelDialog()
	tm, _ = m.Update(refreshSnapMsg{})
	m = tm.(Model)
	band := ansiBG(string(m.theme.SelectionBG))

	for _, tc := range []struct {
		name, want string
		tabs       int
	}{
		{"list", "Grok", 0},
		{"effort", "effort", 1},
		{"fast", "fast  [off]", 2},
	} {
		f := m
		for i := 0; i < tc.tabs; i++ {
			f = pressKey(t, f, tea.KeyTab)
		}
		raw, plainRows := rows(f.View()), rows(plainView(f))
		painted := 0
		for y := range raw {
			if !strings.Contains(raw[y], band) {
				continue
			}
			painted++
			if !strings.Contains(plainRows[y], tc.want) {
				t.Fatalf("focus %s paints row %d (%q), want the %s row", tc.name, y, plainRows[y], tc.want)
			}
		}
		if painted != 1 {
			t.Fatalf("focus %s paints %d rows with SelectionBG, want exactly one:\n%s", tc.name, painted, plainView(f))
		}
	}
}

// TestModelDialogFooterTellsTheTruth: the pinned footer promises ↑↓ and the
// filter, which are still live from a toggle row, but says nothing about ←/→ —
// the only keys that do anything there. Focus swaps the hint.
func TestModelDialogFooterTellsTheTruth(t *testing.T) {
	m := sized(t)
	m = m.openModelDialog()
	tm, _ := m.Update(refreshSnapMsg{})
	m = tm.(Model)
	if !strings.Contains(plainView(m), modelDialogHint) {
		t.Fatalf("the list's footer is missing:\n%s", plainView(m))
	}
	f := pressKey(t, m, tea.KeyTab)
	if !strings.Contains(plainView(f), modelValueHint) {
		t.Fatalf("a toggle row's footer should name ←→:\n%s", plainView(f))
	}
	// Typing still filters whatever has the focus, which is why the hint keeps
	// saying so.
	for _, r := range "fas" {
		tm, _ := f.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		f = tm.(Model)
	}
	if got := f.dialogModelList(); len(got) != 1 || got[0].ID != "fast" {
		t.Fatalf("typing on a toggle row stopped filtering: %+v", got)
	}
	if f.mdlg.focus != dialogFocus("effort") {
		t.Fatalf("typing moved the focus: %v", f.mdlg.focus)
	}
}

// helpModel is the help box open at a size, laid out and ready to draw.
func helpModel(t *testing.T, cols, rows int) Model {
	t.Helper()
	m := sized(t)
	tm, _ := m.Update(tea.WindowSizeMsg{Width: cols, Height: rows})
	m = tm.(Model)
	m.input.SetValue("/help")
	tm, _ = m.Update(enter())
	m = tm.(Model)
	if m.dialog != dialogHelp {
		t.Fatalf("/help left dialog=%v", m.dialog)
	}
	return m
}

// TestHelpDialogIsCentredInTheSameFrame: help is a modal layer like the other
// two, so it inherits the centring, the rectangle and the "never covers another
// band" guarantee instead of being a full-width band above the composer.
func TestHelpDialogIsCentredInTheSameFrame(t *testing.T) {
	for _, size := range [][2]int{{100, 30}, {80, 24}, {40, 12}} {
		m := helpModel(t, size[0], size[1])
		r, tr := m.lay.Dialog, m.lay.Region(regionTranscript)
		if r.Empty() {
			t.Fatalf("%v: no rectangle for the help box: %+v", size, m.lay)
		}
		if r.X != (size[0]-r.W)/2 {
			t.Fatalf("%v: the box is at x=%d, not centred (%d wide)", size, r.X, r.W)
		}
		if r.Y < tr.Top || r.Y+r.H > tr.Bottom {
			t.Fatalf("%v: the box %+v escapes the transcript %+v", size, r, tr)
		}
		// The band that used to host help is the slash menu's alone now.
		if !m.lay.Region(regionOverlay).Empty() {
			t.Fatalf("%v: help still draws as a band: %+v", size, m.lay.Region(regionOverlay))
		}
		lines := rows(plainView(m))
		if len(lines) != size[1] {
			t.Fatalf("%v: frame is %d rows", size, len(lines))
		}
		if !strings.Contains(lines[m.lay.Region(regionStatus).Top], "cursor") {
			t.Fatalf("%v: status row 1 was covered:\n%s", size, plainView(m))
		}
		if !strings.Contains(lines[r.Y+1], helpDialogTitle) {
			t.Fatalf("%v: no title row:\n%s", size, plainView(m))
		}
	}
}

// TestHelpDialogScrollsRatherThanOverflowing: the content is taller than any
// terminal craze draws in, so the box clips to the transcript region, says so
// with ▲/▼, and the keys move the window instead of growing the box.
func TestHelpDialogScrollsRatherThanOverflowing(t *testing.T) {
	m := helpModel(t, 100, 30)
	if m.helpShown() >= len(m.helpLines()) {
		t.Fatalf("fixture: the box is not clipped (%d of %d rows)", m.helpShown(), len(m.helpLines()))
	}
	if !strings.Contains(plainView(m), "▼") {
		t.Fatalf("a clipped box owes the reader a ▼:\n%s", plainView(m))
	}
	if strings.Contains(plainView(m), "▲") {
		t.Fatalf("nothing is above the first row yet:\n%s", plainView(m))
	}
	down := pressKey(t, m, tea.KeyDown)
	if down.helpTop != 1 {
		t.Fatalf("↓ moved the window to %d", down.helpTop)
	}
	if !strings.Contains(plainView(down), "▲") {
		t.Fatalf("a scrolled box owes the reader a ▲:\n%s", plainView(down))
	}
	// Up from the top and down past the bottom both clamp, so the box can never
	// scroll past its own content.
	if up := pressKey(t, m, tea.KeyUp); up.helpTop != 0 {
		t.Fatalf("↑ at the top scrolled to %d", up.helpTop)
	}
	end := m
	for i := 0; i < 20; i++ {
		end = pressKey(t, end, tea.KeyPgDown)
	}
	if want := len(end.helpLines()) - end.helpShown(); end.helpTop != want {
		t.Fatalf("pgdn settled at %d, want the last page at %d", end.helpTop, want)
	}
	view := plainView(end)
	if !strings.Contains(view, "▲") || strings.Contains(view, "▼") {
		t.Fatalf("the last page marks only ▲:\n%s", view)
	}
	// The last row of the content is the last row of the last page.
	last := end.helpLines()[len(end.helpLines())-1]
	if !strings.Contains(view, last.desc) {
		t.Fatalf("the bottom of the box is missing %q:\n%s", last.desc, view)
	}
}

// TestHelpDialogFloorIsTitleOnly: the box degrades to the same floor the other
// dialogs respect — one row, the title — rather than covering a band.
func TestHelpDialogFloorIsTitleOnly(t *testing.T) {
	m := helpModel(t, 100, 30)
	for budget, want := range map[int]int{1: 1, 2: 2, 3: 3} {
		if got := len(m.helpDialogBody(helpDialogWidth-dialogBorder, budget)); got != want {
			t.Fatalf("a %d-row budget drew %d rows, want %d", budget, got, want)
		}
	}
	body := m.helpDialogBody(helpDialogWidth-dialogBorder, 1)
	if !strings.Contains(plain(body[0]), helpDialogTitle) {
		t.Fatalf("the one row a squeezed box keeps is the title, got %q", plain(body[0]))
	}
}

// TestHelpDialogGroupsAndNamesItsSections: one key per row, aligned in two
// columns under a heading, and the session's own commands kept apart from
// craze's builtins because they vary by session.
func TestHelpDialogGroupsAndNamesItsSections(t *testing.T) {
	m := helpModel(t, 100, 30)
	lines := m.helpLines()
	var headings []string
	for _, l := range lines {
		if l.heading() {
			headings = append(headings, l.desc)
			continue
		}
		if l.desc == "" {
			t.Fatalf("key %q has nothing beside it", l.key)
		}
		if w := lipgloss.Width(l.key); w > helpKeyCol-1 {
			t.Fatalf("key %q is %d cells, past the %d-cell gutter", l.key, w, helpKeyCol)
		}
	}
	want := []string{
		"sending and editing", "queued messages", "mode", "moving and scrolling",
		"panels and views", "selection and clipboard", "commands",
		"this session's commands",
	}
	if !reflect.DeepEqual(headings, want) {
		t.Fatalf("headings %q, want %q", headings, want)
	}
	// Every row is rendered one line wide and no wider than the box.
	inner := helpDialogWidth - dialogBorder
	for _, l := range lines {
		row := m.helpRow(l, "", inner)
		if strings.Contains(row, "\n") {
			t.Fatalf("row %q drew two lines", plain(row))
		}
		if w := lipgloss.Width(row); w > inner {
			t.Fatalf("row %q is %d cells, the box has %d", plain(row), w, inner)
		}
	}
	// The agent's command is under its own heading, after the builtins.
	agentAt, sessionAt := -1, -1
	for i, l := range lines {
		switch {
		case l.desc == "this session's commands":
			sessionAt = i
		case l.key == "/research":
			agentAt = i
		}
	}
	if sessionAt < 0 || agentAt < sessionAt {
		t.Fatalf("/research is at %d, the session heading at %d", agentAt, sessionAt)
	}
}

// TestHelpDialogListsOneNamePerCommand: /models and /quit are gone, so help has
// no duplicate rows to collapse and neither spelling exists anywhere.
func TestHelpDialogListsOneNamePerCommand(t *testing.T) {
	m := helpModel(t, 100, 30)
	seen := map[string]int{}
	for _, l := range m.helpLines() {
		if !l.heading() {
			seen[l.desc]++
		}
	}
	for desc, n := range seen {
		if n > 1 {
			t.Fatalf("%d rows say %q; a command belongs on one row", n, desc)
		}
	}
	for _, gone := range []string{"/models", "/quit"} {
		for _, l := range m.helpLines() {
			if l.key == gone {
				t.Fatalf("%s is not a command any more", gone)
			}
		}
		if builtinNamed(strings.TrimPrefix(gone, "/")) {
			t.Fatalf("%s is still a builtin", gone)
		}
	}
}

// TestHelpDialogClosesLikeTheOthers: Esc closes it, a click outside closes it,
// and a card arriving takes it down with the rest of the stack.
func TestHelpDialogClosesLikeTheOthers(t *testing.T) {
	m := helpModel(t, 100, 30)
	if out := pressKey(t, m, tea.KeyEsc); out.dialog != dialogNone {
		t.Fatalf("esc left dialog=%v", out.dialog)
	}
	if out := clickXY(t, m, 0, m.lay.Dialog.Y); out.dialog != dialogNone {
		t.Fatal("a press outside closes the help box")
	}
	// A press inside is inert: there is nothing in there to pick.
	if out := clickXY(t, m, m.lay.Dialog.X+2, m.lay.Dialog.Y+2); out.dialog != dialogHelp {
		t.Fatal("a press inside the help box must not close it")
	}
}

// TestHelpAndTheOtherDialogsAreExclusive: one modal layer at a time, whichever
// order they are opened in.
func TestHelpAndTheOtherDialogsAreExclusive(t *testing.T) {
	m := helpModel(t, 100, 30)
	if got := m.openModelDialog(); got.dialog != dialogModel || got.helpTop != 0 {
		t.Fatalf("/model over help left dialog=%v helpTop=%d", got.dialog, got.helpTop)
	}
	if got := m.openThemePicker(); got.dialog != dialogTheme {
		t.Fatalf("/theme over help left dialog=%v", got.dialog)
	}
	back := m.openModelDialog().openHelp()
	if back.dialog != dialogHelp || back.mdlg.filter.Value() != "" {
		t.Fatalf("help over /model left dialog=%v filter=%q", back.dialog, back.mdlg.filter.Value())
	}
}
