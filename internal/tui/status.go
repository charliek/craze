package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/backend"
	"github.com/charliek/craze/internal/transcript"
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

// statusRow1 is workspace · branch · provider · model (effort) · usage ·
// elapsed. It drops in the pinned order — elapsed, branch, usage, provider —
// and gives the model up last, so the narrowest row is still the workspace
// name. The usage part is a native session's alone (usagePart); every other
// session has none, and its row is what it always was. The mode is not here:
// it is row 2's chip, and it appears in exactly one place.
func (m Model) statusRow1() (string, []segSpan) {
	dim := styleFG(m.theme.Dim)
	ws := statusPart{text: workspaceName(m.cwd), style: styleFG(m.theme.Bright).Bold(true)}
	if m.picking() {
		return fitStatus([]statusPart{ws}, statusSep, dim, m.width)
	}
	if !m.sessionReady() && m.status != statusError {
		// A loaded session is doing something more specific than starting: it
		// is reading back a transcript, which is what the row says until the
		// replay ends (§3.5).
		what := "starting…"
		if m.replaying {
			what = "restoring…"
		}
		return fitStatus([]statusPart{
			ws,
			{text: what, style: dim, drop: 1},
		}, statusSep, dim, m.width)
	}
	return fitStatus([]statusPart{
		ws,
		{text: m.branch, style: dim, drop: 2},
		{text: m.snap.Provider.Label(), style: styleFG(m.theme.Provider), drop: 4},
		// The model span is what V3's dialog will open from; nothing
		// hit-tests it yet.
		{text: m.modelLabel(), style: styleFG(m.theme.FG), drop: 5, id: spanModel},
		{text: usagePart(m.snap.Usage), style: dim, drop: 3},
		{text: m.sessionElapsed(), style: dim, drop: 1},
	}, statusSep, dim, m.width)
}

// usagePart is row 1's usage part (plan 028 §3.14): how full the context is
// and what the session has spent, the turn's then the session's, as the
// snapshot's usage section last had it — read from m.snap, which a usage
// delta refreshes like any other (seam 8), never asked of the engine here.
//
//   - priced: `34% ctx · $0.04 / $1.20`, money to the cent, half up, and
//     `<$0.01` for an amount above nothing and under half a cent;
//   - an amount with some usage in it that had no price carries a `+` — it
//     is at least that much: `34% ctx · $0.04 / $1.20+`;
//   - nothing priced at all — the session's cost is nothing and some of its
//     usage had no price — says the billed tokens instead, input, cache and
//     output: `34% ctx · 12.3k / 1.21M tok`;
//   - a window craze does not know leaves the `NN% ctx · ` prefix out;
//   - no usage section — every ACP session, and a native one before its
//     first step — is no part at all.
func usagePart(u *agent.UsageState) string {
	if u == nil {
		return ""
	}
	var b strings.Builder
	if u.ContextWindow > 0 {
		fmt.Fprintf(&b, "%d%% ctx%s", contextPercent(u.ContextTokens, u.ContextWindow), statusDot)
	}
	if u.Session.CostPicoUSD == 0 && u.Session.Unpriced {
		b.WriteString(transcript.TokenCount(billedTokens(u.Turn)) + " / " + transcript.TokenCount(billedTokens(u.Session)) + " tok")
		return b.String()
	}
	b.WriteString(spendMoney(u.Turn) + " / " + spendMoney(u.Session))
	return b.String()
}

// contextPercent is tokens as a whole percentage of window, rounded half up;
// window is not zero. A context past its window says so, over 100.
func contextPercent(tokens, window int64) int64 {
	if tokens < 0 {
		tokens = 0
	}
	return (tokens*100 + window/2) / window
}

// billedTokens is what a spend was billed for, in tokens: its input, its
// cache reads and writes, and its output — reasoning is inside output, and
// is not counted twice.
func billedTokens(s agent.Spend) int64 {
	return s.Input + s.CacheRead + s.CacheCreation + s.Output
}

// spendMoney is a spend's cost as the row says it: formatUSD, and a `+` when
// some usage in it had no price.
func spendMoney(s agent.Spend) string {
	if s.Unpriced {
		return formatUSD(s.CostPicoUSD) + "+"
	}
	return formatUSD(s.CostPicoUSD)
}

// halfCentPicoUSD is half a cent in picodollars (10⁻¹² $): a cent is 10¹⁰.
const halfCentPicoUSD = 5_000_000_000

// formatUSD is an amount of picodollars in dollars and cents, rounded half
// up — $0.00, $0.04, $1.20, $1234.57 — and `<$0.01` for one above nothing that
// would round to nothing. A negative amount, which no pricing produces, reads
// as nothing.
func formatUSD(pico int64) string {
	if pico < 0 {
		pico = 0
	}
	if pico > 0 && pico < halfCentPicoUSD {
		return "<$0.01"
	}
	cents := pico/(2*halfCentPicoUSD) + (pico%(2*halfCentPicoUSD)+halfCentPicoUSD)/(2*halfCentPicoUSD)
	return fmt.Sprintf("$%d.%02d", cents/100, cents%100)
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
			text:  m.mergedSpinnerText(),
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
		statusPart{text: m.queueCount(), style: styleFG(m.theme.Accent), drop: 2},
		statusPart{text: m.agentCount(), style: styleFG(m.theme.Accent), drop: 2},
	)
	return fitStatus(parts, statusDot, dim, m.width)
}

// modeChip is the current mode, coloured by what the provider says it means
// rather than by its id, so an agent that calls plan mode "architect" still
// gets the plan colour. Clicking it cycles the mode, as shift+tab does.
func (m Model) modeChip() (string, lipgloss.Style) {
	if !m.showModes() {
		return "", lipgloss.NewStyle()
	}
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

// queueCount is what Esc is about to let run: the consequence of cancelling a
// turn with messages behind it, visible before the key is pressed. It stays
// when the band itself is degraded away.
func (m Model) queueCount() string {
	n := len(m.queueItems())
	if n == 0 {
		return ""
	}
	return fmt.Sprintf("⧗ %d queued", n)
}

// permissionChip is the pinned permission state: red bypass under --force,
// amber prompting without it.
func (m Model) permissionChip() (string, lipgloss.Style) {
	if m.bypassing() {
		return "▸▸ bypass permissions on", styleFG(m.theme.ChipBypass)
	}
	return "▸ prompting for permissions", styleFG(m.theme.ChipPrompt)
}

// bypassing is what the permission chip says: whether the session's agent
// runs its tools unasked. The host's word when it says (Info's
// PermissionMode, plan 030 §3.7, SF-60) — its own --force or --no-force,
// whatever this craze's command line said — and otherwise this TUI's own
// config (Config.Yolo): in process, where it spawned the agent itself, and
// over a host from before plan 030, which does not say.
func (m Model) bypassing() bool {
	switch m.hostPerm {
	case backend.PermissionBypass:
		return true
	case backend.PermissionPrompt:
		return false
	}
	return m.yolo
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
	if m.showEffort() {
		if opt := agent.EffortOption(m.snap); opt != nil && opt.Current != "" {
			bits = append(bits, sanitizeLine(opt.Current))
		}
	}
	if m.showFast() && agent.FastOn(m.snap) {
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
	if !m.showSubagents() {
		return ""
	}
	n := 0
	for i := range m.snap.Subagents {
		if subagentRunning(m.snap.Subagents[i]) {
			n++
		}
	}
	if n == 0 {
		return ""
	}
	return fmt.Sprintf("← %d agent%s", n, plural(n))
}

// sessionElapsed is how long the session has been up, in the pinned coarse
// form: whole minutes, then hours and minutes. It counts from the session's
// start as its host says it (Info's StartedAt, plan 030 §3.7, SF-63) — a
// session running for an hour reads 1h00m in a client that has just
// attached — and otherwise from this client's own start (sessStart): in
// process, where the two are one, and over a host from before plan 030,
// which does not say. A start on the host's clock that is ahead of this one
// reads 0m (formatCoarse).
func (m Model) sessionElapsed() string {
	start := m.sessStart
	if !m.hostStart.IsZero() {
		start = m.hostStart
	}
	if start.IsZero() {
		return formatCoarse(0)
	}
	return formatCoarse(m.now().Sub(start))
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
