package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/charliek/craze/internal/agent"
)

const (
	// statusSep joins the row-1 segments, statusDot the row-2 groups.
	statusSep = " │ "
	statusDot = " · "
	// statusProvider is the only backend this cut speaks to.
	statusProvider = "cursor"
)

// statusKindOrder is the fixed order the in-flight counts are listed in, so a
// row does not reshuffle between frames; statusKindName maps an ACP tool kind
// onto the word the row uses.
var statusKindOrder = []string{"shell", "read", "edit", "search", "fetch"}

func statusKindName(kind string) string {
	switch kind {
	case "execute":
		return "shell"
	case "read", "edit", "search", "fetch":
		return kind
	}
	return ""
}

// statusPart is one segment of a status row. drop is the order it disappears
// in when the row is too narrow: 1 goes first, 0 never goes (it is clamped
// with an ellipsis instead).
type statusPart struct {
	text   string
	style  lipgloss.Style
	drop   int
	hidden bool
}

// statusView is the two pinned status rows, which replaced the old footer.
func (m Model) statusView(lay frameLayout) string {
	return m.statusRow1() + "\n" + m.statusRow2(lay)
}

// statusRow1 is workspace · branch · provider · model (effort) · mode ·
// elapsed. It drops from the right in the pinned order — elapsed, branch,
// mode, provider — and gives the model up last, so the narrowest row is still
// the workspace name.
func (m Model) statusRow1() string {
	dim := styleFG(m.theme.Dim)
	ws := statusPart{text: workspaceName(m.cwd), style: styleFG(m.theme.Bright).Bold(true)}
	if !m.started && m.status != statusError {
		return fitStatus([]statusPart{
			ws,
			{text: "starting…", style: dim, drop: 1},
		}, statusSep, dim, m.width)
	}
	return fitStatus([]statusPart{
		ws,
		{text: m.branch, style: dim, drop: 2},
		{text: statusProvider, style: styleFG(m.theme.Provider), drop: 4},
		{text: m.modelLabel(), style: styleFG(m.theme.FG), drop: 5},
		{text: sanitizeLine(m.snap.CurrentMode), style: dim, drop: 3},
		{text: m.sessionElapsed(), style: dim, drop: 1},
	}, statusSep, dim, m.width)
}

// statusRow2 is the permission chip, the in-flight tool counts and the
// sub-agent count. The agent count goes first when the row is narrow, then
// the counts; the chip stays and truncates instead.
func (m Model) statusRow2(lay frameLayout) string {
	dim := styleFG(m.theme.Dim)
	chip, chipStyle := m.permissionChip()
	parts := make([]statusPart, 0, 4)
	if lay.SpinnerMerged {
		// Degradation step 6 took the spinner line away; this is all that is
		// left of it, and it is worth more than either neighbour.
		parts = append(parts, statusPart{
			text:  m.spinnerGlyph() + " " + m.turnElapsed(),
			style: styleFG(m.theme.Accent),
		})
	}
	parts = append(parts,
		statusPart{text: chip, style: chipStyle},
		statusPart{text: m.inFlightCounts(), style: dim, drop: 2},
		statusPart{text: m.agentCount(), style: styleFG(m.theme.Accent), drop: 1},
	)
	return fitStatus(parts, statusDot, dim, m.width)
}

// permissionChip is the pinned permission state: red bypass under --force,
// amber prompting without it.
func (m Model) permissionChip() (string, lipgloss.Style) {
	if m.yolo {
		return "▸▸ bypass permissions on", styleFG(m.theme.ChipBypass)
	}
	return "▸ prompting for permissions", styleFG(m.theme.ChipPrompt)
}

// modelLabel is the advertised display name of the current model, plus the
// effort when the agent offers one.
func (m Model) modelLabel() string {
	id := m.snap.CurrentModel
	if id == "" {
		id = m.model
	}
	name := id
	for _, md := range m.snap.Models {
		if md.ID == id && md.Name != "" {
			name = md.Name
			break
		}
	}
	name = sanitizeLine(name)
	if opt := agent.EffortOption(m.snap); opt != nil && opt.Current != "" {
		name += " (" + sanitizeLine(opt.Current) + ")"
	}
	return name
}

// inFlightCounts names what is running right now, by kind. Sub-agents are
// counted by agentCount instead, and the todo writer is never shown.
func (m Model) inFlightCounts() string {
	counts := make(map[string]int, len(statusKindOrder))
	for i := range m.snap.Tools {
		t := &m.snap.Tools[i]
		if !toolInFlight(t.Status) || t.IsTask() || t.IsTodoTool() {
			continue
		}
		if name := statusKindName(t.Kind); name != "" {
			counts[name]++
		}
	}
	out := make([]string, 0, len(statusKindOrder))
	for _, name := range statusKindOrder {
		if n := counts[name]; n > 0 {
			out = append(out, fmt.Sprintf("%d %s%s", n, name, plural(n)))
		}
	}
	return strings.Join(out, ", ")
}

// agentCount is the `← n agents` marker pointing at the rows below.
func (m Model) agentCount() string {
	n := 0
	for i := range m.snap.Tools {
		if t := &m.snap.Tools[i]; toolInFlight(t.Status) && t.IsTask() {
			n++
		}
	}
	if n == 0 {
		return ""
	}
	return fmt.Sprintf("← %d agent%s", n, plural(n))
}

// sessionElapsed is how long the session has been up, in the pinned coarse
// form: whole minutes, then hours and minutes.
func (m Model) sessionElapsed() string {
	if m.sessStart.IsZero() {
		return formatCoarse(0)
	}
	return formatCoarse(m.now().Sub(m.sessStart))
}

func formatCoarse(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	mins := int(d / time.Minute)
	if mins < 60 {
		return fmt.Sprintf("%dm", mins)
	}
	return fmt.Sprintf("%dh%02dm", mins/60, mins%60)
}

// fitStatus hides parts in their pinned drop order until the row fits, then
// renders what is left joined by sep.
func fitStatus(parts []statusPart, sep string, sepStyle lipgloss.Style, width int) string {
	last := 0
	for _, p := range parts {
		if p.drop > last {
			last = p.drop
		}
	}
	for step := 1; step <= last && statusWidth(parts, sep) > width; step++ {
		for i := range parts {
			if parts[i].drop == step {
				parts[i].hidden = true
			}
		}
	}
	segs := make([]seg, 0, 2*len(parts))
	for _, p := range parts {
		if p.hidden || p.text == "" {
			continue
		}
		if len(segs) > 0 {
			segs = append(segs, seg{sep, sepStyle})
		}
		segs = append(segs, seg{p.text, p.style})
	}
	return renderSegs(width, segs...)
}

func statusWidth(parts []statusPart, sep string) int {
	w, n := 0, 0
	for _, p := range parts {
		if p.hidden || p.text == "" {
			continue
		}
		if n > 0 {
			w += lipgloss.Width(sep)
		}
		w += lipgloss.Width(p.text)
		n++
	}
	return w
}
