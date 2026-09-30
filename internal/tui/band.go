package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// The session band (plan 030 §3.11): one row at the top of a session's frame,
// the first region (regionBand), saying which session this is — its title,
// provider and directory — and the way back to the list, `← sessions`, styled
// as the sub-agent view's top rule is (subagentTopRule): a rule with the
// session's chip in it.
//
// It has rows only once the session list has been opened in this TUI
// (Model.bandOn): before that nothing on screen says there are sessions to go
// back to, and every frame of a TUI that never opens the list — every frame
// without Config.Sessions, every existing golden — is exactly what it was.
// Once it has, every session's frame carries it, the session the list was
// opened from included. It is laid out as any region is (frameSizes.band:
// chrome, degrade's last step, fitChrome's first give).

// bandBack is the band's right-hand hint: the key that opens the list.
const bandBack = "← sessions"

// bandMinCells is the fewest cells the session's chip keeps beside the hint;
// narrower, the hint goes and the chip takes the row.
const bandMinCells = 12

// bandRows is the band's height: one row once the list has been opened in
// this TUI, none before.
func (m Model) bandRows() int {
	if !m.bandOn {
		return 0
	}
	return 1
}

// bandChip is the session as the band names it: `title · provider · dir` —
// its title (`new session` until it has one, as its list row says), the
// provider it runs as the status row names it, and its workspace's name.
func (m Model) bandChip() string {
	title := sanitizeLine(m.snap.Title)
	if title == "" {
		title = sessUntitled
	}
	parts := []string{title}
	if p := sanitizeLine(m.snap.Provider.Label()); p != "" {
		parts = append(parts, p)
	}
	if m.cwd != "" {
		parts = append(parts, sanitizeLine(workspaceName(m.cwd)))
	}
	return strings.Join(parts, " · ")
}

// bandView is the band's one row: `─ <chip> ──── ← sessions ─`, the chip in
// the accent as the sub-agent view's is, the rule and the hint dim. A narrow
// row clips the chip first, then drops the hint.
func (m Model) bandView() string {
	width := max(1, m.width)
	rule := styleFG(m.theme.Rule)
	chipSt := lipgloss.NewStyle().Foreground(m.theme.BG).Background(m.theme.Accent)
	hint := styleFG(m.theme.Dim)
	lead, tail := "─ ", " ─"
	back := " " + bandBack
	room := width - lipgloss.Width(lead) - lipgloss.Width(tail)
	chip := m.bandChip()
	if room-lipgloss.Width(back)-1 < bandMinCells {
		// No room for the hint beside a chip worth reading: the chip alone.
		back = ""
	}
	chipW := room - lipgloss.Width(back) - 1
	if chipW < 1 {
		return chipSt.Render(clampWidth(chip, width))
	}
	chip = clampWidth(chip, chipW)
	fill := room - lipgloss.Width(chip) - lipgloss.Width(back)
	return rule.Render(lead) + chipSt.Render(chip) + rule.Render(" "+strings.Repeat("─", max(0, fill-1))) +
		hint.Render(back) + rule.Render(tail)
}
