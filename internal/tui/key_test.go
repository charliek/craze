package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"
)

// testKey constructs the same named press the terminal and frame driver send.
func testKey(name string) tea.KeyPressMsg {
	if letter, ok := strings.CutPrefix(name, "ctrl+"); ok && len(letter) == 1 {
		return tea.KeyPressMsg{Code: rune(letter[0]), Mod: tea.ModCtrl}
	}
	for _, k := range simpleFrameKeys {
		if k.String() == name {
			return k
		}
	}
	switch name {
	case "f2":
		return tea.KeyPressMsg{Code: tea.KeyF2}
	case "f4":
		return tea.KeyPressMsg{Code: tea.KeyF4}
	case "home":
		return tea.KeyPressMsg{Code: tea.KeyHome}
	case "end":
		return tea.KeyPressMsg{Code: tea.KeyEnd}
	}
	panic("unknown test key " + name)
}
