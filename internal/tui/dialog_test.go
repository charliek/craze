package tui

import (
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

// TestModelsSlashOpensTheDialog: /models is the same opener as /model.
func TestModelsSlashOpensTheDialog(t *testing.T) {
	m := sized(t)
	m.input.SetValue("/models")
	tm, _ := m.Update(enter())
	m = tm.(Model)
	if m.dialog != dialogModel {
		t.Fatalf("/models left dialog=%v", m.dialog)
	}
	if m.input.Value() != "" {
		t.Fatalf("the command should have been consumed: %q", m.input.Value())
	}
}

// TestFastOnSendsTheAdvertisedValue: the dialog's "on" is whatever the agent
// called the value that is not "Off" — the string "true" in every capture.
func TestFastOnSendsTheAdvertisedValue(t *testing.T) {
	m := sized(t)
	m = m.openModelDialog()
	m = pressKey(t, m, tea.KeyTab)
	m = pressKey(t, m, tea.KeyTab)
	if m.mdlg.focus != focusFast {
		t.Fatalf("focus %v", m.mdlg.focus)
	}
	m = pressKey(t, m, tea.KeyRight)
	if m.mdlg.fast != "true" {
		t.Fatalf("the on value is %q, want the advertised \"true\"", m.mdlg.fast)
	}
	tm, cmd := m.Update(enter())
	m = flushCmd(t, tm.(Model), cmd)
	if got := agent.FastOption(m.sess.(*Stub).Snapshot()); got == nil || got.Current != "true" {
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
	stub := m.sess.(*Stub)
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
			stub := m.sess.(*Stub)
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
