package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
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
	// statusRows is one today (footer) and becomes two in U3b.
	statusRows = 1
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

// frameLayout is the whole screen for one frame: the row range of every
// region, computed once per Update from the current state. View() draws from
// it and the mouse hit-tester reads the same struct, so what was drawn and
// what is clickable cannot drift apart.
//
// Regions are listed top to bottom and their ranges tile [0, Height).
type frameLayout struct {
	Width, Height int

	// TooSmall replaces every region with the centred minimum-size message.
	TooSmall bool

	Transcript yRange // scrollback viewport
	Overlay    yRange // help, model picker or slash menu
	Tasks      yRange // pinned tasks panel
	Spinner    yRange // spinner line
	Composer   yRange // rule + input rows + rule
	Agents     yRange // sub-agent rows (U3b; today's tool strip)
	Peek       yRange // sub-agent prompt peek
	Modal      yRange // permission line, later the card stack
	Status     yRange // status rows (U3b; today's footer)

	// What degradation left of the regions that can shrink.
	TasksRows     int  // task rows under the header; 0 means header-only
	ComposerRows  int  // input rows between the two rules
	AgentRows     int  // agent rows drawn
	SpinnerMerged bool // the spinner folded into status row 2
	Degraded      int  // how many degradation steps were applied
}

// frameSizes is the height of every chrome region before degradation.
type frameSizes struct {
	tasksOpen bool
	tasksBody int
	spinner   int
	input     int // composer content rows, without the two rules
	agents    int
	peek      int
	modal     int
	status    int
	merged    bool
}

func (s frameSizes) tasks() int {
	if !s.tasksOpen {
		return 0
	}
	return 1 + s.tasksBody
}

func (s frameSizes) composer() int { return s.input + 2 }

func (s frameSizes) chrome() int {
	return s.tasks() + s.spinner + s.composer() + s.agents + s.peek + s.modal + s.status
}

// degrade applies the first n steps of the pinned degradation order. It is
// cumulative and idempotent, so the caller can re-run it from the natural
// sizes for each candidate n.
func degrade(s frameSizes, n int) frameSizes {
	if n >= 1 && s.agents > agentRowsShort {
		s.agents = agentRowsShort
	}
	if n >= 2 {
		s.tasksBody = 0
	}
	if n >= 3 && s.input > composerShortRows {
		s.input = composerShortRows
	}
	if n >= 4 {
		// The peek belongs to an agent row, so it goes with the rows.
		s.agents, s.peek = 0, 0
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
		case s.agents > 0:
			s.agents = 0
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
func (m Model) computeLayout() frameLayout {
	lay := frameLayout{Width: m.width, Height: m.height}
	if m.width < minFrameCols || m.height < minFrameRows {
		lay.TooSmall = true
		return lay
	}

	base := frameSizes{
		tasksOpen: m.tasksPanelVisible(),
		tasksBody: m.tasksBodyRows(),
		input:     m.composerRows(),
		agents:    len(m.stripItems()),
		peek:      m.peekRows(),
		status:    statusRows,
	}
	if m.spinnerVisible() {
		base.spinner = 1
	}
	if m.pending != nil {
		base.modal = 1
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
	overlay := 0
	if nat := m.overlayRows(); nat > 0 && rest > minTranscriptRows {
		overlay = min(nat, rest-minTranscriptRows)
	}

	y := 0
	put := func(n int) yRange {
		r := yRange{Top: y, Bottom: y + n}
		y += n
		return r
	}
	lay.Transcript = put(rest - overlay)
	lay.Overlay = put(overlay)
	lay.Tasks = put(s.tasks())
	lay.Spinner = put(s.spinner)
	lay.Composer = put(s.composer())
	lay.Agents = put(s.agents)
	lay.Peek = put(s.peek)
	lay.Modal = put(s.modal)
	lay.Status = put(s.status)

	lay.TasksRows = s.tasksBody
	lay.ComposerRows = s.input
	lay.AgentRows = s.agents
	lay.SpinnerMerged = s.merged
	lay.Degraded = steps
	return lay
}

// chromeHeight is every row the transcript does not get, derived from the
// layout rather than counted a second time.
func (m Model) chromeHeight() int {
	return m.lay.Height - m.lay.Transcript.Height()
}

// relayout recomputes the frame and resizes the widgets that own their own
// height. It runs once per Update, after the state has settled.
func (m *Model) relayout(stick bool) {
	if m.width <= 0 || m.height <= 0 {
		return
	}
	m.lay = m.computeLayout()
	if m.lay.TooSmall {
		return
	}
	m.vp.Width = m.width
	m.vp.Height = max(1, m.lay.Transcript.Height())
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

// tooSmallView is the whole screen below the minimum size. Keys still reach
// the model, so Ctrl+C and Ctrl+D quit from here.
func tooSmallView(width, height int) string {
	if width <= 0 || height <= 0 {
		return "craze"
	}
	msg := clampWidth(fmt.Sprintf("craze: terminal too small (need %d×%d)", minFrameCols, minFrameRows), width)
	pad := max(0, (width-lipgloss.Width(msg))/2)
	rows := make([]string, height)
	mid := (height - 1) / 2
	for i := range rows {
		if i == mid {
			rows[i] = padRow(strings.Repeat(" ", pad)+msg, width)
			continue
		}
		rows[i] = padRow("", width)
	}
	return strings.Join(rows, "\n")
}
