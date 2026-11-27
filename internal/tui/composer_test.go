package tui

import (
	"io"
	"strings"
	"testing"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// composerBand is the rows this frame actually gave the composer: the top
// rule, the visible input rows, the bottom rule.
func composerBand(t *testing.T, m Model) []string {
	t.Helper()
	lines := strings.Split(plainView(m), "\n")
	r := m.lay.Region(regionComposer)
	if r.Empty() || r.Bottom > len(lines) {
		t.Fatalf("composer region %+v is not in a %d-row frame", r, len(lines))
	}
	return lines[r.Top:r.Bottom]
}

// wrapCases are the shapes the old ceil(width/inner) count got wrong, plus the
// ones a grapheme-unaware count would.
var wrapCases = []struct{ name, line string }{
	{"empty", ""},
	{"short", "hello"},
	{"exactly full", strings.Repeat("x", 18)},
	{"one under full", strings.Repeat("x", 17)},
	{"one over full", strings.Repeat("x", 19)},
	{"spaced words with slack", "alpha bravo charlie delta echo foxtrot"},
	{"trailing spaces", "alpha   "},
	{"long unbroken token", strings.Repeat("z", 250)},
	{"token then words", strings.Repeat("z", 40) + " tail words here"},
	{"wide graphemes", strings.Repeat("世界", 12)},
	{"wide graphemes with a space", "世界 世界 世界 世界 世界 世界"},
	{"combining marks", strings.Repeat("é", 20)},
	{"mixed widths", "aい" + strings.Repeat("bう", 9)},
}

// TestComposerRowsMatchTextareaLineInfo checks the layout’s requested height
// against the actual soft wraps on each line.
func TestComposerRowsMatchTextareaLineInfo(t *testing.T) {
	const inner = 18
	lines := make([]string, 0, len(wrapCases))
	for _, tc := range wrapCases {
		lines = append(lines, tc.line)
	}

	ta := textarea.New()
	ta.ShowLineNumbers = false
	ta.CharLimit = 0
	ta.MaxHeight = 0
	ta.SetPromptFunc(composerPromptW, composerPrompt)
	ta.SetWidth(inner + composerPromptW)
	ta.SetHeight(len(lines) * 20)

	total := 0
	for i, tc := range wrapCases {
		// LineInfo() describes the line the cursor is on, and SetValue leaves
		// the cursor at the end of what it was given: growing the buffer one
		// line at a time walks it onto every case in turn.
		ta.SetValue(strings.Join(lines[:i+1], "\n"))
		if ta.Line() != i {
			t.Fatalf("%s: cursor is on line %d, want %d", tc.name, ta.Line(), i)
		}
		total += ta.LineInfo().Height
	}

	// And the sum is what the composer asks the layout for.
	m := sized(t)
	tm, _ := m.Update(tea.WindowSizeMsg{Width: inner + composerPromptW, Height: 40})
	m = tm.(Model)
	m.input.SetValue(strings.Join(lines, "\n"))
	if got := m.composerRows(); got != total {
		t.Fatalf("composerRows = %d, want the %d rows bubbles wrapped to", got, total)
	}
}

// A completion or chip uses byte offsets while the textarea moves by visible
// characters. A ZWJ emoji occupies several runes at one cursor stop.
func TestComposerCursorAcrossEmoji(t *testing.T) {
	m, _ := press(sized(t), tea.PasteMsg{Content: "a👩‍💻界"})
	m, _ = press(m, tea.KeyPressMsg{Code: tea.KeyLeft})
	if got, want := m.composerCursorOffset(), len("a👩‍💻"); got != want {
		t.Fatalf("cursor after emoji at byte %d, want %d", got, want)
	}
	m, _ = press(m, tea.KeyPressMsg{Code: tea.KeyLeft})
	if got := m.composerCursorOffset(); got != len("a") {
		t.Fatalf("left through emoji landed at byte %d", got)
	}
	m, _ = press(m, tea.KeyPressMsg{Code: tea.KeyRight})
	m, _ = press(m, tea.KeyPressMsg{Code: tea.KeyBackspace})
	if got := m.input.Value(); got != "a界" || m.composerCursorOffset() != len("a") {
		t.Fatalf("backspace left %q with cursor at byte %d", got, m.composerCursorOffset())
	}
}

// composerKeys is one pass over everything that can change the draft's shape:
// typing, a newline, a wrap boundary, a paste, cursor movement and a resize.
func composerKeys() []tea.Msg {
	out := []tea.Msg{}
	for _, r := range "line one" {
		out = append(out, runeKey(r))
	}
	out = append(out, tea.KeyPressMsg{Code: 'j', Mod: tea.ModCtrl})
	for _, r := range "line two" {
		out = append(out, runeKey(r))
	}
	out = append(out,
		tea.PasteMsg{Content: strings.Repeat("w", 240)},
		tea.PasteMsg{Content: "a\nb\nc\nd\ne"},
		tea.KeyPressMsg{Code: tea.KeyUp},
		tea.KeyPressMsg{Code: tea.KeyUp},
		tea.KeyPressMsg{Code: tea.KeyHome},
		tea.WindowSizeMsg{Width: 60, Height: 24},
		runeKey('!'),
		tea.WindowSizeMsg{Width: 100, Height: 30},
		tea.KeyPressMsg{Code: tea.KeyBackspace},
	)
	return out
}

// TestComposerHeightTracksRowsAfterEveryUpdate checks the height the layout
// grants the growing draft after edits, cursor motion and resizing.
func TestComposerHeightTracksRowsAfterEveryUpdate(t *testing.T) {
	m := sized(t)
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = tm.(Model)
	for i, msg := range composerKeys() {
		tm, _ = m.Update(msg)
		m = tm.(Model)
		limit := composerMaxRows
		if m.height < shortFrameRows {
			limit = composerShortRows
		}
		if got, want := m.lay.ComposerRows, min(m.input.Height(), limit); got != want {
			t.Fatalf("after message %d (%T): composer band is %d rows, want %d", i, msg, got, want)
		}
	}
}

// TestComposerNeverScrollsWhileItFits is the pinned invariant: as long as the
// draft fits the band, the first row of the band is the first row of the
// draft, prompt included. That is the bug this unit exists for.
func TestComposerNeverScrollsWhileItFits(t *testing.T) {
	m := sized(t)
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = tm.(Model)
	for i, msg := range composerKeys() {
		tm, _ = m.Update(msg)
		m = tm.(Model)
		if m.composerRows() > m.lay.ComposerRows {
			continue
		}
		band := composerBand(t, m)
		first := strings.SplitN(m.input.Value(), "\n", 2)[0]
		if !strings.HasPrefix(band[1], "❯ ") {
			t.Fatalf("after message %d (%T): band starts %q, want the prompt", i, msg, band[1])
		}
		if want := clampWidth(first, m.width-composerPromptW); !strings.Contains(band[1], want) {
			t.Fatalf("after message %d (%T): band starts %q, want the first line %q", i, msg, band[1], want)
		}
	}
}

// TestComposerWindowFollowsTheCursor is the other half: past the cap the band
// tracks the cursor, and walking back up brings the first row home again.
func TestComposerWindowFollowsTheCursor(t *testing.T) {
	m := sized(t)
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = tm.(Model)
	for i := 0; i < 9; i++ {
		for _, r := range []rune{'l', rune('1' + i)} {
			tm, _ = m.Update(runeKey(r))
			m = tm.(Model)
		}
		if i < 8 {
			tm, _ = m.Update(tea.KeyPressMsg{Code: 'j', Mod: tea.ModCtrl})
			m = tm.(Model)
		}
	}
	if got := m.composerRows(); got != 9 {
		t.Fatalf("draft is %d rows, want 9", got)
	}
	if m.lay.ComposerRows != composerMaxRows {
		t.Fatalf("band is %d rows, want the %d cap", m.lay.ComposerRows, composerMaxRows)
	}
	band := composerBand(t, m)
	if strings.Contains(band[1], "l1") || !strings.Contains(band[len(band)-2], "l9") {
		t.Fatalf("the window did not follow the cursor to the last line:\n%s", strings.Join(band, "\n"))
	}

	for i := 0; i < 8; i++ {
		tm, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyUp})
		m = tm.(Model)
	}
	band = composerBand(t, m)
	if !strings.HasPrefix(band[1], "❯ l1") {
		t.Fatalf("eight ups did not bring the prompt back:\n%s", strings.Join(band, "\n"))
	}
}

// TestComposerRuleCarriesTheTitle pins the arithmetic: the rule is exactly the
// frame's width whatever the title's display width is.
func TestComposerRuleCarriesTheTitle(t *testing.T) {
	for _, tc := range []struct{ name, title, want string }{
		{"no title", "", " craze ─"},
		{"ascii", "Fix the composer", " Fix the composer ─"},
		{"wide", "日本語のタイトル", " 日本語のタイトル ─"},
		{"control characters", "one\ttwo\nthree", " one two three ─"},
		{"too long", strings.Repeat("wide title ", 20), " wide title wide title wide title wide title wide title… ─"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := sized(t)
			tm, _ := m.Update(tea.WindowSizeMsg{Width: 110, Height: 30})
			m = tm.(Model)
			m.snap.Title = tc.title

			if w, cap := lipgloss.Width(m.composerTitle()), 110/composerTitleShare; w > cap {
				t.Fatalf("title takes %d cells, want at most %d", w, cap)
			}
			top, bottom := m.composerRule(true), m.composerRule(false)
			for _, rule := range []string{top, bottom} {
				if w := lipgloss.Width(rule); w != 110 {
					t.Fatalf("rule is %d cells, want 110: %q", w, plain(rule))
				}
			}
			if got := plain(top); !strings.HasSuffix(got, tc.want) {
				t.Fatalf("top rule is %q, want it to end %q", got, tc.want)
			}
			if got := plain(bottom); got != strings.Repeat("─", 110) {
				t.Fatalf("the bottom rule carries a title: %q", got)
			}
		})
	}
}

// TestComposerRuleFallsBackWhenTheTitleCannotFit keeps the width contract at
// the narrow end, where there is no room for a title beside the dashes.
func TestComposerRuleFallsBackWhenTheTitleCannotFit(t *testing.T) {
	m := sized(t)
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 40, Height: 12})
	m = tm.(Model)
	m.width = 6
	if got, want := plain(m.composerRule(true)), strings.Repeat("─", 6); got != want {
		t.Fatalf("narrow top rule is %q, want %q", got, want)
	}
}

// TestPlanOfferPlaceholderFitsTheBand: the offer is longer than a narrow
// composer, so it is cut to the row's own width and the prompt survives.
func TestPlanOfferPlaceholderFitsTheBand(t *testing.T) {
	m := planOfferModel(t)
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 40, Height: 24})
	m = tm.(Model)
	band := composerBand(t, m)
	row := band[1]
	if !strings.HasPrefix(row, "❯ ") {
		t.Fatalf("the prompt is gone: %q", row)
	}
	if !strings.Contains(row, "…") {
		t.Fatalf("row %q should have been truncated", row)
	}
	if w := lipgloss.Width(row); w != 40 {
		t.Fatalf("row is %d cells, want 40: %q", w, row)
	}
	if m.lay.ComposerRows != 1 {
		t.Fatalf("the offer is one row, got %d", m.lay.ComposerRows)
	}
}

// A terminal wider than bubbles' default MaxWidth of 500 used to wrap the
// textarea at 498 cells while composerRows counted at the real width, so the
// count under-reported and the layout cut rows off the bottom.
func TestComposerCountsAtWideWidths(t *testing.T) {
	const width = 600
	m := sized(t)
	tm, _ := m.Update(tea.WindowSizeMsg{Width: width, Height: 30})
	m = tm.(Model)
	line := strings.Repeat("x", 499)
	m.input.SetValue(line)
	if got, want := m.composerRows(), 1; got != want {
		t.Fatalf("composerRows = %d, want %d at width %d", got, want, width)
	}
	// The textarea has to agree: one row, wrapped at the terminal's width.
	m.relayout(false)
	if got := m.input.LineInfo().Height; got != 1 {
		t.Fatalf("textarea wrapped a %d-cell line to %d rows at width %d", len(line), got, width)
	}
}

// The band owes the layout exactly the rows it was given, so the bottom rule
// stays at the bottom even when the textarea hands back fewer.
func TestComposerBandKeepsItsBottomRule(t *testing.T) {
	m := sized(t)
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 40, Height: 30})
	m = tm.(Model)
	m.input.SetValue("abc")
	m.input.SetHeight(1)
	lay := m.lay
	lay.ComposerRows = 3
	got := strings.Split(m.composerView(lay), "\n")
	if len(got) != 5 {
		t.Fatalf("band is %d rows, want 5 (rule + 3 + rule): %q", len(got), got)
	}
	if !strings.HasPrefix(ansi.Strip(got[4]), "─") {
		t.Fatalf("last band row is %q, want the bottom rule", got[4])
	}
}

// keyBound bounds one key's Update in TestWordMotionOverBlankTextReturns: a
// key takes microseconds, so reaching it means the key never returns.
const keyBound = 5 * time.Second

// updateWithin is m.Update(k) on a goroutine of its own, bounded by
// keyBound: a key that never returns fails the test there instead of hanging
// the suite (its goroutine is left spinning; the test has failed).
func updateWithin(t *testing.T, m Model, k tea.KeyPressMsg) Model {
	t.Helper()
	done := make(chan Model, 1)
	go func() {
		tm, _ := m.Update(k)
		done <- tm.(Model)
	}()
	select {
	case got := <-done:
		return got
	case <-time.After(keyBound):
		t.Fatalf("%s never returned: the update hangs", k)
		return m
	}
}

// parsedKey is the one key bubbletea reads from the terminal bytes in: what
// a real terminal's sequence arrives as.
func parsedKey(t *testing.T, in string) tea.KeyPressMsg {
	t.Helper()
	r := &keyRecorder{}
	p := tea.NewProgram(r, tea.WithInput(strings.NewReader(in)), tea.WithOutput(io.Discard),
		tea.WithoutRenderer(), tea.WithoutSignalHandler())
	done := make(chan struct{})
	go func() {
		_, _ = p.Run()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(keyBound):
		p.Kill()
		<-done
	}
	if len(r.keys) != 1 {
		t.Fatalf("%q was read as %v, want one key", in, r.keys)
	}
	return r.keys[0]
}

// keyRecorder is a program that keeps the first key it is sent, and quits.
type keyRecorder struct{ keys []tea.KeyPressMsg }

func (r *keyRecorder) Init() tea.Cmd { return nil }

func (r *keyRecorder) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if k, ok := msg.(tea.KeyPressMsg); ok {
		r.keys = append(r.keys, k)
		return r, tea.Quit
	}
	return r, nil
}

func (r *keyRecorder) View() tea.View { return tea.View{} }

// TestWordMotionOverBlankTextReturns (plan 030 C9r, found by C10): bubbles'
// word-left never returned with nothing but whitespace before the cursor, so
// alt+←, alt+b and macOS Terminal's ESC b (read as alt+b) hung craze on an
// empty composer — with a session list or without one. Every such key now
// returns and leaves the text and the cursor where they were, which is what
// word-left does wherever it returns there; with a word before the cursor it
// still moves to that word's start. ctrl+← is no word motion (it moves
// nothing). The forward motions, the word case changes and the word
// deletions return over blank text too — bubbles' own, unguarded.
func TestWordMotionOverBlankTextReturns(t *testing.T) {
	for _, binding := range []string{"alt+b", "alt+left"} {
		m := pasteText(t, sized(t), binding)
		if got := m.input.Value(); got != binding {
			t.Errorf("paste of %q left %q", binding, got)
		}
	}
	escB := parsedKey(t, "\x1bb")
	if !key.Matches(escB, textarea.DefaultKeyMap().WordBackward) {
		t.Fatalf("ESC b was read as %s, not a word-left key", escB)
	}
	back := []struct {
		name string
		k    tea.KeyPressMsg
		word bool // a word-left key
	}{
		{"alt+←", tea.KeyPressMsg{Code: tea.KeyLeft, Mod: tea.ModAlt}, true},
		{"alt+b", tea.KeyPressMsg{Code: 'b', Mod: tea.ModAlt}, true},
		{"ESC b", escB, true},
		{"ctrl+←", tea.KeyPressMsg{Code: tea.KeyLeft, Mod: tea.ModCtrl}, false},
	}
	texts := []struct {
		name, text string
		at         int // the cursor's byte offset
		wordAt     int // where word-left leaves it
	}{
		{"an empty composer", "", 0, 0},
		{"blanks before the cursor", "   ", 3, 3},
		{"blank lines before the cursor", "\n  \n", 4, 4},
		{"blanks before a word, the cursor at it", "  word", 2, 2},
		{"the cursor at the start of a word", "word", 0, 0},
		{"a word before the cursor", "fix the flake", 13, 8},
	}
	models := []struct {
		name string
		make func(t *testing.T) Model
	}{
		{"no session list", sized},
		{"a session list", func(t *testing.T) Model { m, _, _ := sessModel(t, 80, 24); return m }},
	}
	for _, mc := range models {
		for _, tc := range texts {
			for _, kc := range back {
				t.Run(mc.name+"/"+tc.name+"/"+kc.name, func(t *testing.T) {
					m := mc.make(t)
					m.input.SetValue(tc.text)
					m.setComposerCursor(tc.text, tc.at)
					if got := m.composerCursorOffset(); got != tc.at {
						t.Fatalf("the premise: the cursor at %d, want %d", got, tc.at)
					}
					m = updateWithin(t, m, kc.k)
					want := tc.at
					if kc.word {
						want = tc.wordAt
					}
					switch {
					case m.input.Value() != tc.text:
						t.Fatalf("the text became %q", m.input.Value())
					case m.composerCursorOffset() != want:
						t.Fatalf("the cursor at %d, want %d", m.composerCursorOffset(), want)
					case m.sessList.open:
						t.Fatal("the key opened the session list")
					}
				})
			}
		}
	}

	forward := []tea.KeyPressMsg{
		{Code: tea.KeyRight, Mod: tea.ModAlt},
		{Code: 'f', Mod: tea.ModAlt},
		{Code: 'u', Mod: tea.ModAlt},
		{Code: 'l', Mod: tea.ModAlt},
		{Code: 'c', Mod: tea.ModAlt},
		{Code: 'd', Mod: tea.ModAlt},
		{Code: tea.KeyDelete, Mod: tea.ModAlt},
		{Code: tea.KeyBackspace, Mod: tea.ModAlt},
		{Code: 'w', Mod: tea.ModCtrl},
		{Code: tea.KeyRight, Mod: tea.ModCtrl},
	}
	for _, tc := range []struct {
		name, text string
		at         int
	}{
		{"an empty composer", "", 0},
		{"blanks after the cursor", "   ", 0},
		{"blank lines after the cursor", "  \n \n", 0},
		{"blanks after a word", "word  ", 4},
	} {
		for _, k := range forward {
			t.Run("forward/"+tc.name+"/"+k.String(), func(t *testing.T) {
				m := sized(t)
				m.input.SetValue(tc.text)
				m.setComposerCursor(tc.text, tc.at)
				updateWithin(t, m, k)
			})
		}
	}
}
