package tui

import (
	"os"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
)

// terminalProfile preserves the existing any-nonempty-value NO_COLOR policy.
// The same profile selects the semantic colors and configures the renderer.
func terminalProfile() colorprofile.Profile {
	profile := colorprofile.Detect(os.Stdout, os.Environ())
	force := os.Getenv("CLICOLOR_FORCE")
	if os.Getenv("NO_COLOR") != "" || (os.Getenv("CLICOLOR") == "0" && (force == "" || force == "0")) {
		return min(profile, colorprofile.ASCII)
	}
	return profile
}

// terminalAppearance gives Bubble Tea the canonical RGB defaults even on an
// indexed-color terminal. It resets any colors it set when the renderer exits.
func (m Model) terminalAppearance(view *tea.View) {
	if m.background {
		view.ForegroundColor = m.theme.terminalFG
		view.BackgroundColor = m.theme.terminalBG
	}
}
