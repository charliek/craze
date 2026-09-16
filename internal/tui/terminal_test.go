package tui

import (
	"bytes"
	"io"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// oscRecorder records both the bytes and how many Writes produced them: one
// Write per pair is the property, not just the bytes (§3.3). The writer Run
// hands the controller is the syncWriter, whose lock is held for the whole
// call, so a pair split across two Writes could have a frame land between its
// halves.
type oscRecorder struct {
	buf    bytes.Buffer
	writes int
}

func (w *oscRecorder) Write(p []byte) (int, error) {
	w.writes++
	return w.buf.Write(p)
}

func (w *oscRecorder) String() string { return w.buf.String() }

func (w *oscRecorder) take() string {
	got := w.buf.String()
	w.buf.Reset()
	w.writes = 0
	return got
}

// setPair is what the controller should write for a theme, spelled out the
// long way so the test does not reimplement the formatter it is checking.
func setPair(bg, fg string) string {
	return "\x1b]11;rgb:" + bg + "\a\x1b]10;rgb:" + fg + "\a"
}

const resetPair = "\x1b]111\a\x1b]110\a"

// TestTerminalColorsWritesOnePairPerWrite pins the bytes and the write count
// for the set pair and the reset pair.
func TestTerminalColorsWritesOnePairPerWrite(t *testing.T) {
	w := &oscRecorder{}
	tc := newTerminalColors(w)

	tc.apply(Preset("gruvbox"))
	want := setPair("28/28/28", "eb/db/b2")
	if got := w.String(); got != want {
		t.Fatalf("set pair = %q, want %q", got, want)
	}
	if w.writes != 1 {
		t.Fatalf("the set pair took %d writes, want exactly 1", w.writes)
	}

	w.take()
	tc.reset()
	if got := w.String(); got != resetPair {
		t.Fatalf("reset pair = %q, want %q", got, resetPair)
	}
	if w.writes != 1 {
		t.Fatalf("the reset pair took %d writes, want exactly 1", w.writes)
	}
}

// TestTerminalColorsResetIsOwedOnlyOnce: a reset is owed by a set and by
// nothing else, so an exit path that never themed the terminal — and a second
// trip down one that did — leaves the terminal alone.
func TestTerminalColorsResetIsOwedOnlyOnce(t *testing.T) {
	w := &oscRecorder{}
	tc := newTerminalColors(w)

	tc.reset()
	if got := w.String(); got != "" {
		t.Fatalf("a reset before any apply wrote %q", got)
	}

	tc.apply(Preset("craze-dark"))
	w.take()
	tc.reset()
	if got := w.take(); got != resetPair {
		t.Fatalf("first reset = %q", got)
	}
	tc.reset()
	if got := w.String(); got != "" {
		t.Fatalf("a second reset wrote %q", got)
	}
}

// TestTerminalColorsDiscardAndNilAreSilent: the controller every test and
// `craze frame` carry must emit nothing, and a nil one must not panic.
func TestTerminalColorsDiscardAndNilAreSilent(t *testing.T) {
	var nilTC *terminalColors
	nilTC.apply(Preset("gruvbox"))
	nilTC.reset()

	tc := newTerminalColors(nil)
	tc.apply(Preset("gruvbox"))
	tc.reset()
	if tc.out == nil {
		t.Fatal("a nil writer should have become io.Discard")
	}
}

// TestTerminalColorsSkipsAMalformedColour: a palette that is not #rrggbb is
// not a reason to write garbage at the terminal.
func TestTerminalColorsSkipsAMalformedColour(t *testing.T) {
	w := &oscRecorder{}
	tc := newTerminalColors(w)
	th := Preset("gruvbox")
	th.BG = "not-a-colour"
	tc.apply(th)
	if got := w.String(); got != "" {
		t.Fatalf("a malformed BG wrote %q", got)
	}
	tc.reset()
	if got := w.String(); got != "" {
		t.Fatalf("a skipped set still owed a reset: %q", got)
	}
}

// TestThemePickerMovesTheTerminalColours is the live-preview half of §3.3:
// every theme change goes through applyTheme, so the terminal's own default
// colours follow the cursor, Esc puts the original pair back, and the two
// no-op paths (Enter on the previewed theme, `/theme <current>`) write
// nothing.
func TestThemePickerMovesTheTerminalColours(t *testing.T) {
	m := themeModel(t, "craze-dark")
	w := &oscRecorder{}
	m.term = newTerminalColors(w)

	tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlG})
	m = tm.(Model)
	if got := w.String(); got != "" {
		t.Fatalf("opening the picker changes no theme, but wrote %q", got)
	}

	tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = tm.(Model)
	light := Preset("craze-light")
	want := setPair("f6/f6/f2", "2a/2a/33")
	if string(light.BG) != "#f6f6f2" || string(light.FG) != "#2a2a33" {
		t.Fatalf("craze-light bg/fg moved: %q/%q", light.BG, light.FG)
	}
	if got := w.take(); got != want {
		t.Fatalf("the preview wrote %q, want %q", got, want)
	}

	tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = tm.(Model)
	dark := Preset("craze-dark")
	want = setPair("0c/0c/11", "c9/c9/d4")
	if string(dark.BG) != "#0c0c11" || string(dark.FG) != "#c9c9d4" {
		t.Fatalf("craze-dark bg/fg moved: %q/%q", dark.BG, dark.FG)
	}
	if got := w.take(); got != want {
		t.Fatalf("esc wrote %q, want the original pair %q", got, want)
	}

	// Enter on the theme the cursor is already previewing is not a change.
	for _, key := range []tea.KeyMsg{{Type: tea.KeyCtrlG}, {Type: tea.KeyDown}, {Type: tea.KeyEnter}} {
		tm, _ = m.Update(key)
		m = tm.(Model)
	}
	if got := w.take(); got != setPair("f6/f6/f2", "2a/2a/33") {
		t.Fatalf("enter should emit only the one preview pair, got %q", got)
	}

	// And neither is naming the theme that is already on.
	m.input.SetValue("/theme " + m.theme.Name)
	tm, _ = m.Update(enter())
	m = tm.(Model)
	if got := w.String(); got != "" {
		t.Fatalf("/theme on the current theme wrote %q", got)
	}
}

// TestModelEmitsNoTerminalColoursByDefault: the seam is off unless Run turned
// it on, so every Go test, `craze frame` and any direct caller of New leaves
// the terminal's colours alone however hard the picker is driven.
func TestModelEmitsNoTerminalColoursByDefault(t *testing.T) {
	m := themeModel(t, "craze-dark")
	if m.term == nil {
		t.Fatal("New should always leave a controller behind")
	}
	if m.term.out != io.Discard {
		t.Fatalf("New's controller writes to %T, want io.Discard", m.term.out)
	}
	for _, key := range []tea.KeyMsg{{Type: tea.KeyCtrlG}, {Type: tea.KeyDown}, {Type: tea.KeyEnter}} {
		tm, _ := m.Update(key)
		m = tm.(Model)
	}
	if m.theme.Name != "craze-light" {
		t.Fatalf("the picker did not run: theme %q", m.theme.Name)
	}
	if m.term.out != io.Discard {
		t.Fatal("driving the picker must not attach a writer")
	}
	if strings.Contains(m.View(), "\x1b]1") {
		t.Fatal("a frame must never carry an OSC 1x sequence")
	}
}
