package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
)

func TestThemePickerMovesTheTerminalColours(t *testing.T) {
	m := themeModel(t, "craze-dark")
	// Even a 16-color terminal accepts a full RGB value in OSC 10/11.
	m.theme = presetFor("craze-dark", colorprofile.ANSI)
	m.background = true
	assertColors := func(bg, fg string) {
		t.Helper()
		v := m.View()
		if v.BackgroundColor == nil || v.ForegroundColor == nil || depthHex(v.BackgroundColor) != bg || depthHex(v.ForegroundColor) != fg {
			t.Fatalf("terminal colors = %v / %v, want %s / %s", v.BackgroundColor, v.ForegroundColor, bg, fg)
		}
		if m.theme.profile != colorprofile.ANSI {
			t.Fatal("changing theme lost the terminal color profile")
		}
	}
	m = m.openThemePicker()
	assertColors("#141419", "#dedee8")
	tm, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	m = tm.(Model)
	assertColors("#f7f5ef", "#292834")
	tm, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	m = tm.(Model)
	assertColors("#141419", "#dedee8")
}

func TestTerminalColorOptOut(t *testing.T) {
	for _, tc := range []struct {
		name    string
		enabled bool
		profile colorprofile.Profile
	}{
		{"explicitly disabled", false, colorprofile.TrueColor}, {"no colors", true, colorprofile.ASCII}, {"no tty", true, colorprofile.NoTTY},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newModel(Config{Session: NewStub(), Workspace: t.TempDir(), Background: tc.enabled}, tc.profile)
			v := m.View()
			if v.ForegroundColor != nil || v.BackgroundColor != nil {
				t.Fatal("view requested terminal colors")
			}
		})
	}
}
