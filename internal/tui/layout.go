package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

const (
	// minFrameCols / minFrameRows are the smallest terminal craze draws in;
	// below either the whole view is the too-small message.
	minFrameCols = 40
	minFrameRows = 12
	// shortFrameRows is the pinned "short terminal" threshold: under it the
	// first three degradation steps apply whether or not they are needed.
	shortFrameRows = 30
	// minTranscriptRows is the floor degradation works towards.
	minTranscriptRows = 3
	forcedDegrade     = 3
	maxDegrade        = 7
	// agentRowsShort is the agent-row cap of degradation step 1.
	agentRowsShort = 2
	// statusRows is the pinned pair: the session line and the chip line.
	statusRows = 2
)

// yRange is a half-open range of screen rows, [Top, Bottom). An empty range is
// a region this frame did not draw, so a click can never land in it.
type yRange struct{ Top, Bottom int }

func (r yRange) Height() int {
	if r.Bottom > r.Top {
		return r.Bottom - r.Top
	}
	return 0
}

func (r yRange) Empty() bool { return r.Bottom <= r.Top }

func (r yRange) Contains(y int) bool { return y >= r.Top && y < r.Bottom }

// Row is the 0-based row inside the region, or -1 when y falls outside it.
func (r yRange) Row(y int) int {
	if !r.Contains(y) {
		return -1
	}
	return y - r.Top
}

// regionID names one horizontal band of the frame. The values are the order
// the bands appear on screen, top to bottom.
type regionID int

const (
	regionTranscript regionID = iota // scrollback viewport
	regionOverlay                    // help, model picker or slash menu
	regionTasks                      // pinned tasks panel
	regionSpinner                    // spinner line
	regionComposer                   // rule + input rows + rule
	regionPeek                       // sub-agent prompt peek
	regionModal                      // the blocking-card band (§3.11)
	regionStatus                     // the two status rows
	regionAgents                     // sub-agent rows, under the status rows
	regionCount
)

// frameRegion says how tall a band is once degradation has run and how it is
// drawn. Its regionID is its index in frameRegions, so there is no second
// place for the identity to be stated.
type frameRegion struct {
	rows func(frameSizes) int
	view func(Model, frameLayout) string
}

// frameRegions is the single ordered list of bands. It drives both the ranges
// computeLayout assigns and the order View draws them in, so the rows a click
// hit-tests against and the rows that were printed cannot drift apart. Adding
// a region means adding one entry here and nothing else.
var frameRegions = [regionCount]frameRegion{
	regionTranscript: {
		rows: func(s frameSizes) int { return s.transcript },
		view: func(m Model, _ frameLayout) string { return m.vp.View() },
	},
	regionOverlay: {
		rows: func(s frameSizes) int { return s.overlay },
		view: func(m Model, _ frameLayout) string { return m.overlayView() },
	},
	regionTasks: {
		rows: frameSizes.tasks,
		view: Model.tasksView,
	},
	regionSpinner: {
		rows: func(s frameSizes) int { return s.spinner },
		view: func(m Model, _ frameLayout) string { return m.spinnerView() },
	},
	regionComposer: {
		rows: frameSizes.composer,
		view: func(m Model, _ frameLayout) string { return m.composerView() },
	},
	regionPeek: {
		rows: func(s frameSizes) int { return s.peek },
		view: func(m Model, _ frameLayout) string { return m.agentPeekView() },
	},
	regionModal: {
		rows: func(s frameSizes) int { return s.modal },
		view: func(m Model, lay frameLayout) string { return m.modalBandView(lay) },
	},
	regionStatus: {
		rows: func(s frameSizes) int { return s.status },
		view: Model.statusView,
	},
	regionAgents: {
		rows: frameSizes.agents,
		view: func(m Model, _ frameLayout) string { return m.agentRowsView() },
	},
}

// frameLayout is the whole screen for one frame: the row range of every
// region, computed once per Update from the current state. View() draws from
// it and the mouse hit-tester read the same struct, so what was drawn and what
// is clickable cannot drift apart.
//
// The ranges tile [0, Height) in frameRegions order.
type frameLayout struct {
	Width, Height int

	// TooSmall replaces every region with the centred minimum-size message.
	TooSmall bool

	regions [regionCount]yRange

	// What degradation left of the regions that can shrink.
	TasksRows     int  // task rows under the header; 0 means header-only
	ComposerRows  int  // input rows between the two rules
	AgentRows     int  // agent rows drawn, without the "… +n more" row
	SpinnerMerged bool // the spinner folded into status row 2
	Degraded      int  // how many degradation steps were applied
}

// Region is the row range one band occupies this frame; an empty range means
// the band was not drawn, so a click can never land in it.
func (l frameLayout) Region(id regionID) yRange {
	if id < 0 || id >= regionCount {
		return yRange{}
	}
	return l.regions[id]
}

// frameSizes is the height of every region before degradation.
type frameSizes struct {
	transcript int // filled in after degradation, from what is left over
	overlay    int
	tasksOpen  bool
	tasksBody  int
	spinner    int
	input      int // composer content rows, without the two rules
	agentsAll  int // sub-agents with a row to draw
	agentsCap  int // how many of them degradation still allows
	peek       int
	modal      int
	status     int
	merged     bool
}

func (s frameSizes) tasks() int {
	if !s.tasksOpen {
		return 0
	}
	return 1 + s.tasksBody
}

func (s frameSizes) composer() int { return s.input + 2 }

// agents is the whole region: the rows that fit plus the overflow row.
func (s frameSizes) agents() int { return agentRegionRows(s.agentsAll, s.agentsCap) }

// agentRows is how many sub-agents are actually listed.
func (s frameSizes) agentRows() int { return min(s.agentsAll, s.agentsCap) }

// chrome is every region except the transcript and the overlay, which take
// what the others leave.
func (s frameSizes) chrome() int {
	return s.tasks() + s.spinner + s.composer() + s.agents() + s.peek + s.modal + s.status
}

// degrade applies the first n steps of the pinned degradation order. It is
// cumulative and idempotent, so the caller can re-run it from the natural
// sizes for each candidate n.
func degrade(s frameSizes, n int) frameSizes {
	if n >= 1 && s.agentsCap > agentRowsShort {
		s.agentsCap = agentRowsShort
	}
	if n >= 2 {
		s.tasksBody = 0
	}
	if n >= 3 && s.input > composerShortRows {
		s.input = composerShortRows
	}
	if n >= 4 {
		// The peek belongs to an agent row, so it goes with the rows.
		s.agentsCap, s.peek = 0, 0
	}
	if n >= 5 {
		s.tasksOpen = false
	}
	if n >= 6 && s.spinner > 0 {
		s.spinner, s.merged = 0, true
	}
	if n >= 7 && s.modal > 1 {
		s.modal = 1
	}
	return s
}

// fitChrome is the backstop the height contract rests on: the degradation
// steps always fit at minFrameRows today, but a region a later unit grows must
// still never push the frame past the bottom of the screen.
func fitChrome(s frameSizes, limit int) frameSizes {
	for s.chrome() > limit {
		switch {
		case s.peek > 0:
			s.peek = 0
		case s.agentsCap > 0:
			s.agentsCap = 0
		case s.tasksBody > 0:
			s.tasksBody = 0
		case s.tasksOpen:
			s.tasksOpen = false
		case s.spinner > 0:
			s.spinner, s.merged = 0, true
		case s.modal > 1:
			s.modal--
		case s.input > 1:
			s.input--
		case s.status > 0:
			s.status--
		case s.modal > 0:
			s.modal = 0
		default:
			return s
		}
	}
	return s
}

// computeLayout is the single layout computation. Every region's height is
// decided here; View() only places what this returned.
func (m *Model) computeLayout() frameLayout {
	m.layouts++
	lay := frameLayout{Width: m.width, Height: m.height}
	if m.width < minFrameCols || m.height < minFrameRows {
		lay.TooSmall = true
		return lay
	}

	base := frameSizes{
		tasksOpen: m.tasksPanelVisible(),
		tasksBody: m.tasksBodyRows(),
		input:     m.composerRows(),
		agentsAll: len(m.agentItems()),
		agentsCap: agentRowsMax,
		peek:      m.peekRows(),
		modal:     m.modalRows(),
		status:    statusRows,
	}
	if m.spinnerVisible() {
		base.spinner = 1
	}

	steps := 0
	if m.height < shortFrameRows {
		steps = forcedDegrade
	}
	s := degrade(base, steps)
	for s.chrome() > m.height-minTranscriptRows && steps < maxDegrade {
		steps++
		s = degrade(base, steps)
	}
	s = fitChrome(s, m.height-1)

	// The overlay takes what is left over the transcript minimum; it is capped
	// rather than degraded, so a long help box crops instead of squeezing the
	// transcript away.
	rest := m.height - s.chrome()
	if nat := m.overlayRows(); nat > 0 && rest > minTranscriptRows {
		s.overlay = min(nat, rest-minTranscriptRows)
	}
	s.transcript = rest - s.overlay

	y := 0
	for i := range frameRegions {
		n := frameRegions[i].rows(s)
		lay.regions[i] = yRange{Top: y, Bottom: y + n}
		y += n
	}

	lay.TasksRows = s.tasksBody
	lay.ComposerRows = s.input
	lay.AgentRows = s.agentRows()
	lay.SpinnerMerged = s.merged
	lay.Degraded = steps
	return lay
}

// chromeHeight is every row the transcript does not get, derived from the
// layout rather than counted a second time.
func (m Model) chromeHeight() int {
	return m.lay.Height - m.lay.Region(regionTranscript).Height()
}

// relayout recomputes the frame and resizes the widgets that own their own
// height. It runs once per Update, after the state has settled.
func (m *Model) relayout(stick bool) {
	if m.width <= 0 || m.height <= 0 {
		return
	}
	m.lay = m.computeLayout()
	// The selection has to be re-found against the rows this frame will
	// actually draw, so it is synced here rather than in the event handler,
	// where the row cap is still the previous frame's.
	m.syncAgents()
	if m.lay.TooSmall {
		return
	}
	m.vp.Width = m.width
	m.vp.Height = max(1, m.lay.Region(regionTranscript).Height())
	// bubbles never grows the textarea on its own, so the height the layout
	// decided has to be pushed into it explicitly.
	m.input.SetWidth(max(1, m.width))
	m.input.SetHeight(max(1, m.lay.ComposerRows))
	if stick {
		m.vp.GotoBottom()
	}
}

// fitRows forces a rendered block to exactly n rows, padded to the full width
// so a short line cannot leave the previous frame's characters behind.
func fitRows(block string, n, width int) []string {
	if n <= 0 {
		return nil
	}
	out := make([]string, 0, n)
	if block != "" {
		for _, ln := range strings.Split(block, "\n") {
			if len(out) == n {
				break
			}
			out = append(out, padRow(ln, width))
		}
	}
	for len(out) < n {
		out = append(out, padRow("", width))
	}
	return out
}

// padRow pads or truncates one line to exactly width cells, which is where the
// "no line is wider than the terminal" invariant is finally enforced.
func padRow(s string, width int) string {
	if width <= 0 {
		return ""
	}
	switch w := lipgloss.Width(s); {
	case w > width:
		return clampWidth(s, width)
	case w < width:
		return s + strings.Repeat(" ", width-w)
	}
	return s
}

// blankFrame is exactly height rows of exactly width cells. A frame with no
// usable width still owes the terminal its rows, so the height contract holds
// for every size and not only the drawable ones.
func blankFrame(width, height int) string {
	if height <= 0 {
		return ""
	}
	rows := make([]string, height)
	for i := range rows {
		rows[i] = padRow("", width)
	}
	return strings.Join(rows, "\n")
}

// tooSmallView is the whole screen below the minimum size. Keys still reach
// the model, so Ctrl+C and Ctrl+D quit from here.
//
// The message wraps rather than truncating: the size the user has to reach is
// the one thing this screen exists to say, and at 30 columns it does not fit
// on one line.
func tooSmallView(width, height int) string {
	if width <= 0 || height <= 0 {
		return blankFrame(width, height)
	}
	msg := fmt.Sprintf("craze: terminal too small (need %d×%d)", minFrameCols, minFrameRows)
	lines := strings.Split(ansi.Hardwrap(ansi.Wordwrap(msg, width, ""), width, true), "\n")
	if len(lines) > height {
		lines = lines[:height]
	}
	rows := make([]string, height)
	top := max(0, (height-len(lines))/2)
	for i := range rows {
		rows[i] = padRow("", width)
	}
	for i, ln := range lines {
		pad := max(0, (width-lipgloss.Width(ln))/2)
		rows[top+i] = padRow(strings.Repeat(" ", pad)+ln, width)
	}
	return strings.Join(rows, "\n")
}
