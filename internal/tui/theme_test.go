package tui

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
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
	if th.SelectionBG != want {
		t.Fatalf("SelectionBG = %q, want %q", th.SelectionBG, want)
	}
	if th.Selection != th.Accent {
		t.Fatalf("Selection should still be the accent foreground, got %q", th.Selection)
	}
	m := themeModel(t, "craze-dark")
	m = m.openModelDialog()
	tm, _ := m.Update(refreshSnapMsg{})
	m = tm.(Model)
	if !strings.Contains(m.View(), ansiBG(string(want))) {
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
	m = tm.(Model)
	tm, _ = m.Update(startedMsg{})
	return tm.(Model)
}

var hexColor = regexp.MustCompile(`^#[0-9a-f]{6}$`)

// TestEveryPresetFillsEverySlot is the pinned invariant: a renderer may ask for
// any slot, so a preset that left one empty would render that element with no
// colour at all rather than fail anywhere near the table.
func TestEveryPresetFillsEverySlot(t *testing.T) {
	colorType := reflect.TypeOf(lipgloss.Color(""))
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
			got := v.Field(i).String()
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
		{"bg", string(dark.BG), "#0c0c11"},
		{"fg", string(dark.FG), "#c9c9d4"},
		{"dim", string(dark.Dim), "#82829a"},
		{"bright", string(dark.Bright), "#e2e2ea"},
		{"border", string(dark.Border), "#24242f"},
		{"accent", string(dark.Accent), "#e8a33d"},
		{"teal", string(dark.Teal), "#3fc8c8"},
		{"ok", string(dark.OK), "#6fbf73"},
		{"warn", string(dark.Warn), "#e8a33d"},
		{"err", string(dark.Err), "#e06c75"},
		{"purple", string(dark.Purple), "#bb9af7"},
	} {
		if tc.got != tc.want {
			t.Fatalf("craze-dark %s = %s, want %s", tc.name, tc.got, tc.want)
		}
	}
	if got := string(Preset("gruvbox").Accent); got != "#fabd2f" {
		t.Fatalf("gruvbox accent %s", got)
	}

	// Derived slots name a role; they must stay tied to their source colour.
	for _, tc := range []struct {
		name       string
		got, want  lipgloss.Color
		derivation string
	}{
		{"User", dark.User, dark.Bright, "Bright"},
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
	if dark.DiffAddBG == dark.BG || dark.DiffAddBG == dark.OK || dark.DiffAddBG == dark.DiffDelBG {
		t.Fatalf("diff backgrounds are not blends: add=%s del=%s bg=%s", dark.DiffAddBG, dark.DiffDelBG, dark.BG)
	}
	if got, want := blend("#000000", "#ffffff", 10), lipgloss.Color("#1a1a1a"); got != want {
		t.Fatalf("blend 10%% = %s, want %s", got, want)
	}
	// The composer's two rules sit one step off Border, towards the text they
	// frame; Border itself is too dim to read as a frame.
	if got, want := dark.Rule, blend("#24242f", "#c9c9d4", ruleMix); got != want || got == dark.Border {
		t.Fatalf("Rule = %s, want Border blended %d%% towards FG (%s)", got, ruleMix, want)
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
	for _, e := range m.main.entries {
		if e.renderedFor.theme != "craze-dark" {
			t.Fatalf("entry cached against %q", e.renderedFor.theme)
		}
	}

	tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlG})
	m = tm.(Model)
	tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = tm.(Model)

	if got := m.main.renders - before; got != len(m.main.entries) {
		t.Fatalf("a re-theme re-rendered %d of %d entries", got, len(m.main.entries))
	}
	for _, e := range m.main.entries {
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

	tm, _ = m.Update(eventMsg{agent.Event{
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

// TestThemePickerIgnoredWhileACardIsUp keeps the card owning the keyboard.
func TestThemePickerIgnoredWhileACardIsUp(t *testing.T) {
	m := withOverlay(t)
	tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlG})
	m = tm.(Model)
	if m.dialog == dialogTheme {
		t.Fatal("ctrl+g must be ignored while a card is up")
	}
}
