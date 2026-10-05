package tui

import (
	"io"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/charliek/craze/internal/agent"
)

// ansiFG is the escape a hex colour renders as under the forced true-colour
// profile. The sequence is asked for rather than spelled out because termenv
// converts through a colour space on the way (#e8a33d lands on 232;163;60).
func ansiFG(hex string) string {
	return "\x1b[" + termenv.TrueColor.Color(hex).Sequence(false) + "m"
}

// ansiBG is the same for a background, which is the only way to tell
// SelectionBG apart from the foreground slot beside it.
func ansiBG(hex string) string {
	return termenv.TrueColor.Color(hex).Sequence(true)
}

// TestSelectionBGIsADerivedBackground: SelectionBG is a derived slot mixed off
// the palette's own background and accent, and the dialog's cursor row is what
// paints with it. Selection stays a foreground slot beside it.
func TestSelectionBGIsADerivedBackground(t *testing.T) {
	th := Preset("craze-dark")
	want := blend("#0c0c11", "#e8a33d", selectionMix)
	if th.SelectionBG.TrueColor != want {
		t.Fatalf("SelectionBG = %q, want %q", th.SelectionBG, want)
	}
	if th.Selection != th.Accent {
		t.Fatalf("Selection should still be the accent foreground, got %q", th.Selection)
	}
	m := themeModel(t, "craze-dark")
	m = m.openModelDialog()
	tm, _ := m.Update(refreshSnapMsg{})
	m = tm.(Model)
	if !strings.Contains(m.View(), ansiBG(want)) {
		t.Fatal("the dialog cursor row does not paint with SelectionBG")
	}
}

func themeModel(t *testing.T, name string) Model {
	t.Helper()
	isolateSkillsHome(t)
	m := New(Config{
		Session:   NewStub(),
		Theme:     name,
		Workspace: t.TempDir(),
		Model:     "grok",
		Yolo:      true,
	})
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	return startedLikeInit(t, tm.(Model))
}

var hexColor = regexp.MustCompile(`^#[0-9a-f]{6}$`)

// TestEveryPresetFillsEverySlot is the pinned invariant: a renderer may ask for
// any slot, so a preset that left one empty would render that element with no
// colour at all rather than fail anywhere near the table.
func TestEveryPresetFillsEverySlot(t *testing.T) {
	colorType := reflect.TypeOf(lipgloss.CompleteColor{})
	for _, name := range ThemeNames() {
		th := Preset(name)
		if th.Name != name {
			t.Fatalf("Preset(%q).Name = %q", name, th.Name)
		}
		v := reflect.ValueOf(th)
		slots := 0
		for i := 0; i < v.NumField(); i++ {
			if v.Type().Field(i).Type != colorType {
				continue
			}
			slots++
			got := v.Field(i).FieldByName("TrueColor").String()
			if !hexColor.MatchString(got) {
				t.Fatalf("%s: slot %s is %q, want a #rrggbb colour", name, v.Type().Field(i).Name, got)
			}
		}
		if slots < 20 {
			t.Fatalf("%s: only %d colour slots checked, the Theme struct should have more", name, slots)
		}
	}
	if len(ThemeNames()) != 7 {
		t.Fatalf("presets %v, want the seven pinned ones", ThemeNames())
	}
}

// TestPresetTableIsTheSpec spot-checks the hex table rather than restating it:
// the first row (the default) and the last, plus the derivations every other
// slot hangs off.
func TestPresetTableIsTheSpec(t *testing.T) {
	dark := Preset("craze-dark")
	for _, tc := range []struct{ name, got, want string }{
		{"bg", dark.BG.TrueColor, "#0c0c11"},
		{"fg", dark.FG.TrueColor, "#c9c9d4"},
		{"dim", dark.Dim.TrueColor, "#82829a"},
		{"bright", dark.Bright.TrueColor, "#e2e2ea"},
		{"border", dark.Border.TrueColor, "#24242f"},
		{"accent", dark.Accent.TrueColor, "#e8a33d"},
		{"teal", dark.Teal.TrueColor, "#3fc8c8"},
		{"ok", dark.OK.TrueColor, "#6fbf73"},
		{"warn", dark.Warn.TrueColor, "#e8a33d"},
		{"err", dark.Err.TrueColor, "#e06c75"},
		{"purple", dark.Purple.TrueColor, "#bb9af7"},
	} {
		if tc.got != tc.want {
			t.Fatalf("craze-dark %s = %s, want %s", tc.name, tc.got, tc.want)
		}
	}
	if got := Preset("gruvbox").Accent.TrueColor; got != "#fabd2f" {
		t.Fatalf("gruvbox accent %s", got)
	}

	// Derived slots name a role; they must stay tied to their source colour.
	for _, tc := range []struct {
		name       string
		got, want  lipgloss.CompleteColor
		derivation string
	}{
		{"User", dark.User, dark.Teal, "Teal"},
		{"UserMark", dark.UserMark, dark.Accent, "Accent"},
		{"Heading", dark.Heading, dark.OK, "OK"},
		{"Assistant", dark.Assistant, dark.FG, "FG"},
		{"Thought", dark.Thought, dark.Dim, "Dim"},
		{"ToolKind", dark.ToolKind, dark.Accent, "Accent"},
		{"DiffAdd", dark.DiffAdd, dark.OK, "OK"},
		{"DiffDel", dark.DiffDel, dark.Err, "Err"},
		{"LineNo", dark.LineNo, dark.Dim, "Dim"},
		{"TaskRail", dark.TaskRail, dark.Accent, "Accent"},
		{"Selection", dark.Selection, dark.Accent, "Accent"},
		{"ChipBypass", dark.ChipBypass, dark.Err, "Err"},
		{"ChipPrompt", dark.ChipPrompt, dark.Warn, "Warn"},
		{"Provider", dark.Provider, dark.Teal, "Teal"},
		{"ModeImplement", dark.ModeImplement, dark.Accent, "Accent"},
		{"ModePlan", dark.ModePlan, dark.Purple, "Purple"},
		{"ModeReadOnly", dark.ModeReadOnly, dark.Teal, "Teal"},
	} {
		if tc.got != tc.want {
			t.Fatalf("%s = %s, want %s (%s)", tc.name, tc.got, tc.want, tc.derivation)
		}
	}
	// The tinted diff backgrounds are 10% of the diff colour over the
	// background, so they sit beside BG and nowhere near OK/Err.
	if add, del := dark.DiffAddBG.TrueColor, dark.DiffDelBG.TrueColor; add == dark.BG.TrueColor || add == dark.OK.TrueColor || add == del {
		t.Fatalf("diff backgrounds are not blends: add=%s del=%s bg=%s", dark.DiffAddBG, dark.DiffDelBG, dark.BG)
	}
	if got, want := blend("#000000", "#ffffff", 10), "#1a1a1a"; got != want {
		t.Fatalf("blend 10%% = %s, want %s", got, want)
	}
	// The composer's two rules sit one step off Border, towards the text they
	// frame; Border itself is too dim to read as a frame.
	if got, want := dark.Rule.TrueColor, blend("#24242f", "#c9c9d4", ruleMix); got != want || got == dark.Border.TrueColor {
		t.Fatalf("Rule = %s, want Border blended %d%% towards FG (%s)", got, ruleMix, want)
	}

	// Every preset, not just craze-dark, has to keep these three slots apart:
	// a heading that reads as inline code, a user mark that vanishes into its
	// own text, or a user row indistinguishable from the assistant's would
	// defeat the point of splitting the slots out.
	for _, name := range ThemeNames() {
		th := Preset(name)
		if th.Heading.TrueColor == th.Accent.TrueColor {
			t.Fatalf("%s: Heading must not equal Accent (inline code / spinner / chips)", name)
		}
		if th.UserMark.TrueColor == th.User.TrueColor {
			t.Fatalf("%s: UserMark must not equal User", name)
		}
		if th.User.TrueColor == th.Assistant.TrueColor {
			t.Fatalf("%s: User must not equal Assistant", name)
		}
	}
}

// TestUserRowGlyphAndTextColour pins §U1: the ❯ that opens a prompt row is
// UserMark, and the prompt text beside it is bold User, not the old plain
// Bright that read as body text.
func TestUserRowGlyphAndTextColour(t *testing.T) {
	m := themeModel(t, "craze-dark")
	th := m.theme
	m.addUser("hello")
	m.refreshViewport()
	want := styleFG(th.UserMark).Render("❯ ") + styleFG(th.User).Bold(true).Render("hello")
	if !strings.Contains(m.View(), want) {
		t.Fatalf("user row missing mark+text styling:\n%s", m.View())
	}
}

// TestInterjectionRowGlyphAndTextColour is the same pairing for the ↳ mark an
// interjection draws instead of ❯.
func TestInterjectionRowGlyphAndTextColour(t *testing.T) {
	m := themeModel(t, "craze-dark")
	th := m.theme
	m.applyEvent(agent.Event{Type: agent.EventUser, Interjection: true, Text: "hi there"})
	m.refreshViewport()
	want := styleFG(th.UserMark).Render("↳ ") + styleFG(th.User).Bold(true).Render("hi there")
	if !strings.Contains(m.View(), want) {
		t.Fatalf("interjection row missing mark+text styling:\n%s", m.View())
	}
}

// TestUserRowContinuationIsUnstyled pins the other half of §U1: a wrapped
// user row's continuation indent carries no glyph, so it must not carry a
// colour either — only the text after it does.
func TestUserRowContinuationIsUnstyled(t *testing.T) {
	m := themeModel(t, "craze-dark")
	th := m.theme
	text := strings.Repeat("a", 40) + " " + strings.Repeat("b", 40)
	m.addUser(text)
	m.refreshViewport()
	wrapped := wrapProse(text, 80-2)
	lines := strings.Split(wrapped, "\n")
	if len(lines) < 2 {
		t.Fatalf("test text did not wrap onto a continuation row: %q", wrapped)
	}
	want := "  " + styleFG(th.User).Bold(true).Render(lines[1])
	if !strings.Contains(m.View(), want) {
		t.Fatalf("continuation row is not a plain indent plus styled text:\n%s", m.View())
	}
}

// TestMarkdownHeadingAndInlineCodeColours pins §U1's other half: a heading
// paints with Heading, not the Accent that inline code, the spinner and chips
// also use, so the two no longer collide in one reply.
func TestMarkdownHeadingAndInlineCodeColours(t *testing.T) {
	m := themeModel(t, "craze-dark")
	th := m.theme
	m.applyEvent(agent.Event{Type: agent.EventText, Text: "## Title\n\n`code`"})
	m.refreshViewport()
	view := m.View()
	wantHeading := lipgloss.NewStyle().Foreground(th.Heading).Bold(true).Render("Title")
	if !strings.Contains(view, wantHeading) {
		t.Fatalf("heading missing Heading colour:\n%s", view)
	}
	wantCode := styleFG(th.ToolKind).Render("code")
	if !strings.Contains(view, wantCode) {
		t.Fatalf("inline code missing ToolKind colour:\n%s", view)
	}
}

func TestPresetNamesAndFallback(t *testing.T) {
	if Preset("").Name != DefaultTheme || DefaultTheme != "craze-dark" {
		t.Fatalf("the default is %q", Preset("").Name)
	}
	if Preset("nonsense").Name != DefaultTheme {
		t.Fatal("an unknown name should fall back to the default")
	}
	for _, alias := range []string{"Tokyo_Night", "tokyonight", " TOKYO "} {
		if got := Preset(alias).Name; got != "tokyo-night" {
			t.Fatalf("alias %q resolved to %q", alias, got)
		}
	}
	if !KnownPreset("gruvbox") || KnownPreset("nonsense") {
		t.Fatal("KnownPreset")
	}
}

// TestThemePickerPreviewsLiveAndEscRestores is the pinned picker contract: the
// screen is re-themed while the cursor moves, and Esc puts back the theme that
// was active when it opened.
func TestThemePickerPreviewsLiveAndEscRestores(t *testing.T) {
	m := themeModel(t, "craze-dark")
	m.addUser("hello")

	tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlG})
	m = tm.(Model)
	if m.dialog != dialogTheme {
		t.Fatal("ctrl+g should open the theme picker")
	}
	if got := plainView(m); !strings.Contains(got, "craze-light") || !strings.Contains(got, "gruvbox") {
		t.Fatalf("the picker lists names only:\n%s", got)
	}
	if m.themeNames[0] != "craze-dark" {
		t.Fatalf("the current theme should be first, got %v", m.themeNames)
	}
	if !strings.Contains(m.View(), ansiFG("#e8a33d")) {
		t.Fatal("the craze-dark accent should be on screen before anything moves")
	}

	tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = tm.(Model)
	if m.theme.Name != "craze-light" {
		t.Fatalf("moving the cursor must re-theme the live model, got %q", m.theme.Name)
	}
	raw := m.View()
	if !strings.Contains(raw, ansiFG("#b8760f")) {
		t.Fatal("the craze-light accent is missing from the frame before Enter")
	}
	if strings.Contains(raw, ansiFG("#e8a33d")) {
		t.Fatal("the craze-dark accent survived the preview")
	}

	tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = tm.(Model)
	if m.dialog == dialogTheme {
		t.Fatal("esc should close the picker")
	}
	if m.theme.Name != "craze-dark" {
		t.Fatalf("esc must restore the theme the picker opened on, got %q", m.theme.Name)
	}
	if !strings.Contains(m.View(), ansiFG("#e8a33d")) {
		t.Fatal("esc restored the name but not the colours")
	}
	if notes := texts(m, entryNote); len(notes) != 0 {
		t.Fatalf("a reverted preview leaves no note: %v", notes)
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("HOME"), configDir, configName)); !os.IsNotExist(err) {
		t.Fatalf("esc must not persist anything: %v", err)
	}
}

func TestThemePickerEnterKeepsAndPersists(t *testing.T) {
	m := themeModel(t, "craze-dark")
	for _, key := range []tea.KeyMsg{{Type: tea.KeyCtrlG}, {Type: tea.KeyDown}, {Type: tea.KeyEnter}} {
		tm, _ := m.Update(key)
		m = tm.(Model)
	}
	if m.dialog == dialogTheme {
		t.Fatal("enter should close the picker")
	}
	if m.theme.Name != "craze-light" {
		t.Fatalf("enter should keep the previewed theme, got %q", m.theme.Name)
	}
	if got := texts(m, entryNote); len(got) != 1 || got[0] != "theme → craze-light" {
		t.Fatalf("notes %v", got)
	}
	if got := ConfigTheme(); got != "craze-light" {
		t.Fatalf("persisted theme %q", got)
	}
	b, err := os.ReadFile(filepath.Join(os.Getenv("HOME"), configDir, configName))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `theme = "craze-light"`) {
		t.Fatalf("config is %q", b)
	}
}

// TestThemeChangeRedrawsEveryEntry pins the render cache: the theme is part of
// the key, so a re-theme has to re-render every entry rather than leave the
// previous palette in the cache.
func TestThemeChangeRedrawsEveryEntry(t *testing.T) {
	m := themeModel(t, "craze-dark")
	m.addUser("one")
	m.addNote("two")
	m.addError("three")
	m.refreshViewport()
	before := m.main.renders
	if before < 3 {
		t.Fatalf("setup rendered %d entries", before)
	}
	for _, e := range m.main.entries() {
		if e.renderedFor.theme != "craze-dark" {
			t.Fatalf("entry cached against %q", e.renderedFor.theme)
		}
	}

	tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlG})
	m = tm.(Model)
	tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = tm.(Model)

	if got := m.main.renders - before; got != len(m.main.entries()) {
		t.Fatalf("a re-theme re-rendered %d of %d entries", got, len(m.main.entries()))
	}
	for _, e := range m.main.entries() {
		if e.renderedFor.theme != "craze-light" {
			t.Fatalf("entry still cached against %q", e.renderedFor.theme)
		}
	}
	// The composer keeps its own copy of the prompt style, so a re-theme that
	// only swapped m.theme would leave the ❯ in the old palette.
	if !strings.Contains(m.input.View(), ansiFG("#b8760f")) {
		t.Fatalf("the composer prompt was not repainted:\n%q", m.input.View())
	}
}

func TestSlashThemeSetsDirectlyAndRejectsUnknown(t *testing.T) {
	m := themeModel(t, "craze-dark")
	m.input.SetValue("/theme gruvbox")
	tm, _ := m.Update(enter())
	m = tm.(Model)
	if m.theme.Name != "gruvbox" {
		t.Fatalf("/theme <name> should set directly, got %q", m.theme.Name)
	}
	if m.dialog == dialogTheme {
		t.Fatal("/theme <name> should not open the picker")
	}
	if got := ConfigTheme(); got != "gruvbox" {
		t.Fatalf("persisted %q", got)
	}

	m.input.SetValue("/theme nonsense")
	tm, _ = m.Update(enter())
	m = tm.(Model)
	if m.theme.Name != "gruvbox" {
		t.Fatalf("an unknown name must leave the theme alone, got %q", m.theme.Name)
	}
	errs := texts(m, entryError)
	if len(errs) != 1 || !strings.Contains(errs[0], "nonsense") {
		t.Fatalf("expected one error row naming the theme, got %v", errs)
	}
	if got := ConfigTheme(); got != "gruvbox" {
		t.Fatalf("an unknown name must not be persisted, got %q", got)
	}
}

func TestSlashThemeNoArgsOpensThePicker(t *testing.T) {
	m := themeModel(t, "craze-dark")
	m.input.SetValue("/theme")
	tm, _ := m.Update(enter())
	m = tm.(Model)
	if m.dialog != dialogTheme {
		t.Fatal("/theme should open the picker")
	}
	if m.input.Value() != "" {
		t.Fatalf("composer %q", m.input.Value())
	}
}

func TestThemePickerNumberKeyPicksAndKeeps(t *testing.T) {
	m := themeModel(t, "craze-dark")
	tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlG})
	m = tm.(Model)
	want := m.themeNames[2]
	tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'3'}})
	m = tm.(Model)
	if m.dialog == dialogTheme || m.theme.Name != want {
		t.Fatalf("a number key picks and keeps: dialog=%v theme=%q want %q", m.dialog, m.theme.Name, want)
	}
	if got := ConfigTheme(); got != want {
		t.Fatalf("persisted %q, want %q", got, want)
	}
}

// TestCardClosesThePickerAndRevertsThePreview holds §3.11: a card owns the
// screen, and the preview under it is not the theme the user chose.
func TestCardClosesThePickerAndRevertsThePreview(t *testing.T) {
	m := themeModel(t, "craze-dark")
	m.yolo = false
	tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlG})
	m = tm.(Model)
	tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = tm.(Model)
	if m.theme.Name != "craze-light" {
		t.Fatalf("preview %q", m.theme.Name)
	}

	tm, _ = m.Update(eventMsg{ev: agent.Event{
		Type: agent.EventPermission,
		Permission: &agent.PermissionEvent{
			ID:      "perm-1",
			Tool:    "bash",
			Options: []agent.PermissionOption{{OptionID: "o1", Kind: "allow_once"}},
		},
	}})
	m = tm.(Model)
	if m.dialog == dialogTheme {
		t.Fatal("a card closes the picker")
	}
	if m.theme.Name != "craze-dark" {
		t.Fatalf("the preview should go back with the picker, got %q", m.theme.Name)
	}
}

// TestComposerTextCarriesTheThemeForeground: bubbles leaves FocusedStyle.Text
// an empty style and makes the blurred one an AdaptiveColor, so typed text
// would have no foreground of its own — and a light theme on a dark terminal
// would then paint the terminal's light default text on the theme's light
// background. styleComposer names the colour, which makes OSC 10 a
// synchronisation rather than a necessity (§3.3).
func TestComposerTextCarriesTheThemeForeground(t *testing.T) {
	for _, name := range []string{"craze-dark", "craze-light"} {
		th := Preset(name)
		ta := newComposer(th)
		if got := ta.FocusedStyle.Text.GetForeground(); got != th.FG {
			t.Fatalf("%s: focused composer text is %v, want FG %v", name, got, th.FG)
		}
		if got := ta.BlurredStyle.Text.GetForeground(); got != th.FG {
			t.Fatalf("%s: blurred composer text is %v, want FG %v", name, got, th.FG)
		}
		// And a live re-theme moves it, since that is all styleComposer is for.
		other := Preset("gruvbox")
		styleComposer(&ta, other)
		if got := ta.FocusedStyle.Text.GetForeground(); got != other.FG {
			t.Fatalf("%s: a re-theme left the composer text at %v", name, got)
		}
	}
}

// TestThemePickerIgnoredWhileACardIsUp keeps the card owning the keyboard.
func TestThemePickerIgnoredWhileACardIsUp(t *testing.T) {
	m := withOverlay(t)
	tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlG})
	m = tm.(Model)
	if m.dialog == dialogTheme {
		t.Fatal("ctrl+g must be ignored while a card is up")
	}
}

// The two tests below hold SF-142: every theme legible at every colour depth.
// They assert on the bytes a style renders, not on the Theme's field types, and
// render through a private renderer per profile, so they touch no global (the
// package's TestMain forces the default renderer to true colour) and are
// parallel-safe.

// depthSGR is one SGR sequence in a rendered string.
var depthSGR = regexp.MustCompile(`\x1b\[([0-9;]*)m`)

// depthPaint is what a style paints under one profile: the foreground and the
// background as a palette index (0..255, or -1 for none, which for the
// foreground means the terminal's default) or as an RGB hex at true colour,
// and whether the style reverses them.
type depthPaint struct {
	fgIdx, bgIdx int
	fgHex, bgHex string
	reverse      bool
}

// depthRender renders one cell in the style st builds, through a private
// renderer at profile p, and reads back what it paints.
func depthRender(p termenv.Profile, st func(lipgloss.Style) lipgloss.Style) depthPaint {
	r := lipgloss.NewRenderer(io.Discard)
	r.SetColorProfile(p)
	return depthParse(st(r.NewStyle()).Render("x"))
}

// depthParse reads the colours the SGR sequences in s leave in force.
func depthParse(s string) depthPaint {
	out := depthPaint{fgIdx: -1, bgIdx: -1}
	for _, m := range depthSGR.FindAllStringSubmatch(s, -1) {
		ps := strings.Split(m[1], ";")
		for i := 0; i < len(ps); i++ {
			n, _ := strconv.Atoi(ps[i])
			switch {
			case (n == 38 || n == 48) && i+2 < len(ps) && ps[i+1] == "5":
				k, _ := strconv.Atoi(ps[i+2])
				if n == 38 {
					out.fgIdx = k
				} else {
					out.bgIdx = k
				}
				i += 2
			case (n == 38 || n == 48) && i+4 < len(ps) && ps[i+1] == "2":
				rr, _ := strconv.Atoi(ps[i+2])
				gg, _ := strconv.Atoi(ps[i+3])
				bb, _ := strconv.Atoi(ps[i+4])
				h := string(rgb(uint8(rr), uint8(gg), uint8(bb)))
				if n == 38 {
					out.fgHex = h
				} else {
					out.bgHex = h
				}
				i += 4
			case n == 7:
				out.reverse = true
			case n >= 30 && n <= 37:
				out.fgIdx = n - 30
			case n >= 90 && n <= 97:
				out.fgIdx = n - 90 + 8
			case n >= 40 && n <= 47:
				out.bgIdx = n - 40
			case n >= 100 && n <= 107:
				out.bgIdx = n - 100 + 8
			}
		}
	}
	return out
}

// depthXterm is xterm's own default 0..15 (XTerm-col.ad), the reference
// palette for the 16-colour floor; depthVGA is termenv's table. 0..15 are the
// user's palette, so neither is the truth, and the 16-colour test is semantic
// with these two as an "is it invisible" floor.
var (
	depthXterm = [16]string{"#000000", "#cd0000", "#00cd00", "#cdcd00", "#0000ee", "#cd00cd", "#00cdcd", "#e5e5e5",
		"#7f7f7f", "#ff0000", "#00ff00", "#ffff00", "#5c5cff", "#ff00ff", "#00ffff", "#ffffff"}
	depthVGA = [16]string{"#000000", "#800000", "#008000", "#808000", "#000080", "#800080", "#008080", "#c0c0c0",
		"#808080", "#ff0000", "#00ff00", "#ffff00", "#0000ff", "#ff00ff", "#00ffff", "#ffffff"}
)

// depthIdxHex is a palette index's RGB: pal for 0..15, the fixed xterm cube
// and grey ramp above.
func depthIdxHex(i int, pal [16]string) string {
	if i < 16 {
		return pal[i]
	}
	if i >= 232 {
		v := uint8(8 + 10*(i-232))
		return string(rgb(v, v, v))
	}
	lv := [6]uint8{0, 0x5f, 0x87, 0xaf, 0xd7, 0xff}
	i -= 16
	return string(rgb(lv[i/36], lv[(i/6)%6], lv[i%6]))
}

// depthFGHex is a paint's foreground as RGB. No foreground is the terminal's
// default, which OSC 10 makes exactly the theme's FG while craze runs. With
// OSC off (--no-background, or a terminal that ignores OSC 10) it is the
// user's own default instead, which is intended: it is the readable choice on
// the user's own background, and no test can know that background.
func depthFGHex(pt depthPaint, th Theme, pal [16]string) string {
	switch {
	case pt.fgHex != "":
		return pt.fgHex
	case pt.fgIdx >= 0:
		return depthIdxHex(pt.fgIdx, pal)
	}
	return depthHex(th.FG)
}

// depthKey names what a paint's foreground is, for telling two slots apart.
func depthKey(pt depthPaint) string {
	if pt.fgHex != "" {
		return pt.fgHex
	}
	return strconv.Itoa(pt.fgIdx)
}

// depthHex is a theme colour's true-colour hex, whatever type the slot is.
func depthHex(c lipgloss.TerminalColor) string {
	v := reflect.ValueOf(c)
	if v.Kind() == reflect.String {
		return v.String()
	}
	return v.FieldByName("TrueColor").String()
}

func depthLum(hex string) float64 {
	r, g, b := hexRGB(hex)
	lin := func(v uint8) float64 {
		c := float64(v) / 255
		if c <= 0.04045 {
			return c / 12.92
		}
		return math.Pow((c+0.055)/1.055, 2.4)
	}
	return 0.2126*lin(r) + 0.7152*lin(g) + 0.0722*lin(b)
}

// depthContrast is the WCAG contrast ratio of two colours.
func depthContrast(a, b string) float64 {
	la, lb := depthLum(a), depthLum(b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

type depthSlot struct {
	name string
	line bool // a rule, not text: a lower floor
	get  func(Theme) lipgloss.TerminalColor
}

// depthSlots are the slots that paint text or a rule on the theme's
// background; every other foreground slot is one of these by derivation.
var depthSlots = []depthSlot{
	{"FG", false, func(t Theme) lipgloss.TerminalColor { return t.FG }},
	{"Dim", false, func(t Theme) lipgloss.TerminalColor { return t.Dim }},
	{"Bright", false, func(t Theme) lipgloss.TerminalColor { return t.Bright }},
	{"Accent", false, func(t Theme) lipgloss.TerminalColor { return t.Accent }},
	{"Teal", false, func(t Theme) lipgloss.TerminalColor { return t.Teal }},
	{"OK", false, func(t Theme) lipgloss.TerminalColor { return t.OK }},
	{"Warn", false, func(t Theme) lipgloss.TerminalColor { return t.Warn }},
	{"Err", false, func(t Theme) lipgloss.TerminalColor { return t.Err }},
	{"Purple", false, func(t Theme) lipgloss.TerminalColor { return t.Purple }},
	{"Rule", true, func(t Theme) lipgloss.TerminalColor { return t.Rule }},
	{"Shell", true, func(t Theme) lipgloss.TerminalColor { return t.Shell }},
}

func depthSlotNamed(n string) depthSlot {
	for _, s := range depthSlots {
		if s.name == n {
			return s
		}
	}
	panic("no slot " + n)
}

// depthWant is the contrast a converted colour must keep: three quarters of
// its true-colour contrast or AA (4.5), whichever is lower, and never under the
// floor, 2.0 for text and 1.5 for a rule.
func depthWant(tc float64, line bool) float64 {
	floor := 2.0
	if line {
		floor = 1.5
	}
	return math.Max(floor, math.Min(4.5, 0.75*tc))
}

// TestThemeLegibleAtEveryDepth: at true colour and at 256 colours, every
// preset's text and rules stay legible on its background, colours that differ
// in true colour still differ, and the two composites (text on the selection
// band, the chip's BG on Accent) keep their contrast. SF-142 was craze-dark's
// Err painting 256-colour 232, near-black on a near-black background.
func TestThemeLegibleAtEveryDepth(t *testing.T) {
	for _, name := range ThemeNames() {
		th := Preset(name)
		bg := depthHex(th.BG)
		for _, p := range []termenv.Profile{termenv.TrueColor, termenv.ANSI256} {
			paints := map[string]string{}
			for _, s := range depthSlots {
				c := s.get(th)
				pt := depthRender(p, func(st lipgloss.Style) lipgloss.Style { return st.Foreground(c) })
				got := depthFGHex(pt, th, depthXterm)
				tc := depthContrast(depthHex(c), bg)
				if cr := depthContrast(got, bg); cr < depthWant(tc, s.line) {
					t.Errorf("%s %s: %s %s paints %s, contrast %.2f against %s, want >= %.2f (true colour %.2f)",
						name, p.Name(), s.name, depthHex(c), got, cr, bg, depthWant(tc, s.line), tc)
				}
				paints[s.name] = depthKey(pt)
			}
			// Colours that differ in true colour keep differing: an error that
			// reads as a warning, or a rule that reads as body text, is the
			// collapse this test exists for.
			for i := range depthSlots {
				for j := i + 1; j < len(depthSlots); j++ {
					a, b := depthSlots[i], depthSlots[j]
					if depthHex(a.get(th)) != depthHex(b.get(th)) && paints[a.name] == paints[b.name] {
						t.Errorf("%s %s: %s and %s differ in true colour but paint the same", name, p.Name(), a.name, b.name)
					}
				}
			}
			for _, pr := range []struct {
				name   string
				fg, bk lipgloss.TerminalColor
			}{
				{"FG on SelectionBG", th.FG, th.SelectionBG},
				{"Bright on SelectionBG", th.Bright, th.SelectionBG},
				{"BG on Accent", th.BG, th.Accent},
			} {
				pt := depthRender(p, func(st lipgloss.Style) lipgloss.Style { return st.Foreground(pr.fg).Background(pr.bk) })
				f, b := depthFGHex(pt, th, depthXterm), pt.bgHex
				if pt.bgIdx >= 0 {
					b = depthIdxHex(pt.bgIdx, depthXterm)
				}
				tc := depthContrast(depthHex(pr.fg), depthHex(pr.bk))
				if cr := depthContrast(f, b); cr < depthWant(tc, false) {
					t.Errorf("%s %s: %s paints %s on %s, contrast %.2f, want >= %.2f (true colour %.2f)",
						name, p.Name(), pr.name, f, b, cr, depthWant(tc, false), tc)
				}
			}
		}
	}
}

// TestThemeSixteenColours: sixteen colours are the user's own palette, so each
// preset's picks are held to what a palette cannot undo: each role in its hue
// family, no slot in the background's own index, the bright half on a dark
// theme and the normal half on a light one, distinct roles distinct, plus a
// contrast floor against two reference palettes that only an invisible pick
// fails. FG paints the terminal's default here (see depthFGHex).
func TestThemeSixteenColours(t *testing.T) {
	family := map[string][]int{"Err": {1, 9}, "OK": {2, 10}, "Warn": {3, 11}, "Teal": {6, 14}, "Purple": {5, 13}, "Dim": {8}}
	for _, name := range ThemeNames() {
		th := Preset(name)
		bg := depthHex(th.BG)
		dark := depthLum(bg) < 0.18
		bgPick := depthRender(termenv.ANSI, func(st lipgloss.Style) lipgloss.Style { return st.Foreground(th.BG) }).fgIdx
		idx := map[string]int{}
		for _, s := range depthSlots {
			c := s.get(th)
			pt := depthRender(termenv.ANSI, func(st lipgloss.Style) lipgloss.Style { return st.Foreground(c) })
			idx[s.name] = pt.fgIdx
			if want, ok := family[s.name]; ok && !slices.Contains(want, pt.fgIdx) {
				t.Errorf("%s: %s paints 16-colour %d, want one of %v", name, s.name, pt.fgIdx, want)
			}
			if pt.fgIdx >= 0 && pt.fgIdx == bgPick {
				t.Errorf("%s: %s paints the background's own index %d", name, s.name, pt.fgIdx)
			}
			// 8 (Dim), 0 and 15 (black and white) sit in whichever half they
			// must; every other text colour picks its half by the theme.
			if !s.line && pt.fgIdx >= 0 && pt.fgIdx != 8 && pt.fgIdx != 0 && pt.fgIdx != 15 {
				if dark && pt.fgIdx < 8 {
					t.Errorf("%s: %s paints %d, the dim half, on a dark theme", name, s.name, pt.fgIdx)
				}
				if !dark && pt.fgIdx >= 8 {
					t.Errorf("%s: %s paints %d, the bright half, on a light theme", name, s.name, pt.fgIdx)
				}
			}
			for _, pal := range [][16]string{depthXterm, depthVGA} {
				if got := depthFGHex(pt, th, pal); depthContrast(got, bg) < 1.5 {
					t.Errorf("%s: %s paints %s, contrast %.2f against %s", name, s.name, got, depthContrast(got, bg), bg)
				}
			}
		}
		for _, pr := range [][2]string{{"FG", "Dim"}, {"FG", "Bright"}, {"Err", "Warn"}, {"Err", "OK"}, {"OK", "Warn"},
			{"Err", "Purple"}, {"Teal", "Accent"}, {"Shell", "Rule"}} {
			a, b := depthSlotNamed(pr[0]), depthSlotNamed(pr[1])
			if depthHex(a.get(th)) != depthHex(b.get(th)) && idx[pr[0]] == idx[pr[1]] {
				t.Errorf("%s: %s and %s both paint 16-colour %d", name, pr[0], pr[1], idx[pr[0]])
			}
		}
	}
}

// TestNearest256 pins craze's own 256-colour conversion: an exact cube or grey
// entry maps to itself, nothing maps into 0..15 (the user's palette), and
// craze-dark's Err, which termenv painted 232 (SF-142), is 167.
func TestNearest256(t *testing.T) {
	for _, tc := range []struct {
		hex  string
		want int
	}{
		{"#e06c75", 167},
		{"#d75f87", 168},
		{"#5f5f5f", 59},
		{"#080808", 232},
		{"#eeeeee", 255},
		{"#000000", 16},
		{"#ffffff", 231},
		{"#ff0000", 196},
	} {
		if got := nearest256(tc.hex); got != tc.want {
			t.Errorf("nearest256(%s) = %d, want %d", tc.hex, got, tc.want)
		}
	}
	if got := Preset("craze-dark").Err.ANSI256; got != "167" {
		t.Fatalf("craze-dark Err paints 256-colour %s, want 167", got)
	}
}
