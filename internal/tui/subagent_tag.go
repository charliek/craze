package tui

import (
	"github.com/charmbracelet/lipgloss"

	"github.com/charliek/craze/internal/sessions"
)

// subAgentTag is the dim word a session the index marks as a sub-agent's
// (sessions.Row.Unattended: a session an unattended host started, plan 041
// §3.3) carries after its title in the resume picker and the session list's
// saved rows. It is read from the index row alone — no protocol field — so a
// running session with no row yet shows none.
const subAgentTag = "sub-agent"

// subAgentTitleMin is the fewest cells a title keeps when the tag is drawn
// beside it: a row too narrow for both loses the tag, never the title.
const subAgentTitleMin = 8

// isSubAgent says the index row is a sub-agent's session.
func isSubAgent(row sessions.Row) bool { return row.Unattended != nil }

// titleWithTag is a picker row's title in budget cells with the tag after it
// when marked: the title is clamped first so the tag survives a long one, and
// the tag is left off a budget too small to hold both.
func titleWithTag(title string, budget int, marked bool) string {
	if !marked {
		return clampWidth(title, max(budget, 0))
	}
	tagW := lipgloss.Width(subAgentTag) + 1
	if budget-tagW < subAgentTitleMin {
		return clampWidth(title, max(budget, 0))
	}
	return clampWidth(title, budget-tagW) + " " + subAgentTag
}

// sessTitleSegs is a session-list row's title column, tW cells wide: the
// title alone, or, for a saved sub-agent row, the title padded short of the
// column by the dim tag. An unmarked row draws exactly the one segment it
// always did, so no existing frame moves.
func (m Model) sessTitleSegs(r sessRow, tW int, titleSt lipgloss.Style) []seg {
	tagW := lipgloss.Width(subAgentTag) + 1
	if !r.subAgent || tW-tagW < subAgentTitleMin {
		return []seg{{padCells(r.title, tW), titleSt}}
	}
	return []seg{
		{padCells(r.title, tW-tagW), titleSt},
		{" " + subAgentTag, styleFG(m.theme.Dim)},
	}
}
