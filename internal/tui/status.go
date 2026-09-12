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
	// modeHint is the dim reminder that the chip beside it has a key too. It
	// is the first thing row 2 gives up, and with its separator it costs 12
	// cells, so it only ever appears on a row with the room to spare.
	modeHint = "shift+tab"
)

// spanID names a status segment a click can land on. spanNone is the zero
// value, so a seg is unclickable unless it says otherwise.
type spanID int

const (
	spanNone spanID = iota
	spanMode
	spanModel
)

// segSpan is where one identified segment was drawn: [x0, x1) in display cells
// of its row. renderSegSpans fills these in as it writes the row.
type segSpan struct {
	id     spanID
	x0, x1 int
}

// spanAt is the segment under x, or spanNone between and beyond them.
func spanAt(spans []segSpan, x int) spanID {
	for _, s := range spans {
		if x >= s.x0 && x < s.x1 {
			return s.id
		}
	}
	return spanNone
}

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
	id     spanID
}

// statusView is the two pinned status rows, which replaced the old footer.
// The spans each row reports are thrown away here and asked for again by the
// hit-tester: both callers run the same fitting pass over the same state.
func (m Model) statusView(lay frameLayout) string {
	row1, _ := m.statusRow1()
	row2, _ := m.statusRow2(lay)
	return row1 + "\n" + row2
}

// statusRow1 is workspace · branch · provider · model (effort) · elapsed. It
// drops from the right in the pinned order — elapsed, branch, provider — and
// gives the model up last, so the narrowest row is still the workspace name.
// The mode is not here: it is row 2's chip, and it appears in exactly one
// place.
func (m Model) statusRow1() (string, []segSpan) {
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
		{text: m.snap.Provider.Label(), style: styleFG(m.theme.Provider), drop: 3},
		// The model span is what V3's dialog will open from; nothing
		// hit-tests it yet.
		{text: m.modelLabel(), style: styleFG(m.theme.FG), drop: 4, id: spanModel},
		{text: m.sessionElapsed(), style: dim, drop: 1},
	}, statusSep, dim, m.width)
}

// statusRow2 is the mode chip, the permission chip, the in-flight tool counts
// and the sub-agent count. The hint beside the mode goes first when the row is
// narrow, then the agent count, then the counts; neither chip ever drops, and
// when the two of them alone do not fit it is the permission chip that
// truncates, because it comes second.
func (m Model) statusRow2(lay frameLayout) (string, []segSpan) {
	dim := styleFG(m.theme.Dim)
	chip, chipStyle := m.permissionChip()
	mode, modeStyle := m.modeChip()
	hint := ""
	if mode != "" {
		hint = modeHint
	}
	parts := make([]statusPart, 0, 6)
	if lay.SpinnerMerged {
		// Degradation step 6 took the spinner line away; this is all that is
		// left of it, and it is worth more than any of its neighbours.
		parts = append(parts, statusPart{
			text:  m.spinnerGlyph() + " " + m.turnElapsed(),
			style: styleFG(m.theme.Accent),
		})
	}
	parts = append(parts,
		statusPart{text: mode, style: modeStyle, id: spanMode},
		statusPart{text: hint, style: dim, drop: 1},
		// The copy note is here for two seconds and gone; it never drops,
		// because the row it is crowding is the only feedback a copy gets.
		statusPart{text: m.copyChip(), style: styleFG(m.theme.Accent)},
		statusPart{text: chip, style: chipStyle},
		statusPart{text: m.inFlightCounts(), style: dim, drop: 3},
		statusPart{text: m.agentCount(), style: styleFG(m.theme.Accent), drop: 2},
	)
	return fitStatus(parts, statusDot, dim, m.width)
}

// modeChip is the current mode, coloured by what the provider says it means
// rather than by its id, so an agent that calls plan mode "architect" still
// gets the plan colour. Clicking it cycles the mode, as shift+tab does.
func (m Model) modeChip() (string, lipgloss.Style) {
	id := sanitizeLine(m.snap.CurrentMode)
	if id == "" {
		return "", lipgloss.NewStyle()
	}
	return "◆ " + id, styleFG(m.modeColor(m.snap.Provider.Kind(m.snap.CurrentMode)))
}

// modeColor is the chip colour for a mode kind. A mode craze does not
// recognise is dim rather than miscoloured.
func (m Model) modeColor(kind agent.ModeKind) lipgloss.Color {
	switch kind {
	case agent.ModeImplement:
		return m.theme.ModeImplement
	case agent.ModePlan:
		return m.theme.ModePlan
	case agent.ModeReadOnly:
		return m.theme.ModeReadOnly
	}
	return m.theme.Dim
}

// copyChip is what the last copy did, for as long as the note lingers. It is
// the only acknowledgement a copy gets, so it outranks the counts beside it.
func (m Model) copyChip() string {
	if !m.copyLingering() {
		return ""
	}
	return sanitizeLine(m.copyNote)
}

// permissionChip is the pinned permission state: red bypass under --force,
// amber prompting without it.
func (m Model) permissionChip() (string, lipgloss.Style) {
	if m.yolo {
		return "▸▸ bypass permissions on", styleFG(m.theme.ChipBypass)
	}
	return "▸ prompting for permissions", styleFG(m.theme.ChipPrompt)
}

// modelLabel is the advertised display name of the current model, plus what
// the agent lets craze set beside it: the effort when one is offered, and
// "fast" only when that toggle exists and is on — off is the quiet default and
// says nothing.
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
	var bits []string
	if opt := agent.EffortOption(m.snap); opt != nil && opt.Current != "" {
		bits = append(bits, sanitizeLine(opt.Current))
	}
	if agent.FastOn(m.snap) {
		bits = append(bits, "fast")
	}
	if len(bits) > 0 {
		name += " (" + strings.Join(bits, statusDot) + ")"
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
// renders what is left joined by sep. It returns the spans of the parts that
// asked for one, measured on the row it just wrote.
func fitStatus(parts []statusPart, sep string, sepStyle lipgloss.Style, width int) (string, []segSpan) {
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
	segs := make([]idSeg, 0, 2*len(parts))
	for _, p := range parts {
		if p.hidden || p.text == "" {
			continue
		}
		if len(segs) > 0 {
			segs = append(segs, idSeg{seg: seg{sep, sepStyle}})
		}
		segs = append(segs, idSeg{seg: seg{p.text, p.style}, id: p.id})
	}
	return renderSegSpans(width, segs)
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
