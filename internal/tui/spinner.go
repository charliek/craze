package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/craze/internal/transcript"
)

// fastTick drives the spinner and the clock-based lingers; idle falls back to
// one beat per minute so the session elapsed stays right for free.
const fastTick = 250 * time.Millisecond

var spinnerGlyphs = [...]string{"✳", "✴", "✵", "✶"}

// tickMsg is one beat of the tick chain. gen identifies the chain that asked
// for it: a beat from a chain we abandoned carries an older generation and is
// dropped, so restarts never leave two chains running.
type tickMsg struct{ gen int }

// armTick keeps exactly one chain alive. It is a no-op while a chain of the
// right speed is in flight, which is what stops a burst of events from
// spawning a chain each.
func (m *Model) armTick() tea.Cmd {
	fast := m.wantFastTick()
	if m.tickLive && m.tickFast == fast {
		return nil
	}
	m.tickGen++
	m.tickLive = true
	m.tickFast = fast
	gen := m.tickGen
	d := untilNextMinute(m.now())
	if fast {
		d = fastTick
	}
	return tea.Tick(d, func(time.Time) tea.Msg { return tickMsg{gen: gen} })
}

// handleTick consumes one beat; the chain is re-armed by Update afterwards.
func (m *Model) handleTick(msg tickMsg) {
	if msg.gen != m.tickGen {
		return
	}
	m.tickLive = false
	if m.tickFast && !m.frozen {
		m.spinFrame++
		if m.shellRunning() {
			// The running `!` row draws the spinner from inside the transcript,
			// where the render cache would otherwise hold whichever frame it
			// was first drawn with. Marking the transcript dirty is what asks
			// for the rebuild; setViewportContent re-renders that one row and
			// reuses every other entry's cached lines.
			m.main.dirty = true
		}
	}
}

// wantFastTick is whether the tick chain beats at fastTick. viewedRunning
// keeps an open view's spinner moving — a running bash job's view included,
// which anySubagentRunning leaves out (plan 033 §3.8): the view is the one
// place a job's spinner is drawn.
func (m Model) wantFastTick() bool {
	return m.status == statusWorking || m.cardOpen() || m.tasksLingering() ||
		m.agentLingering() || m.copyLingering() || m.anySubagentRunning() || m.viewedRunning() ||
		m.shellRunning() || m.compacting("") || m.sessListSpinning()
}

// untilNextMinute lines the slow chain up with the minute boundary so a
// displayed "12m" flips over when the minute does.
func untilNextMinute(now time.Time) time.Duration {
	d := time.Minute - time.Duration(now.Second())*time.Second - time.Duration(now.Nanosecond())
	if d <= 0 {
		return time.Minute
	}
	return d
}

func (m Model) spinnerVisible() bool {
	if m.viewing != "" {
		return m.viewedRunning() || m.cardOpen()
	}
	// A foreign-turn or wake compaction can open main's fold while status
	// stays idle and nothing else is running (no turn, no child): the working
	// line still has to show while it does (plan 028 §3.13).
	return m.status == statusWorking || m.cardOpen() || m.anySubagentRunning() || m.compacting("")
}

// spinnerGlyph is the current frame of the cycle, shared with the merged form
// degradation step 6 leaves in status row 2.
func (m Model) spinnerGlyph() string {
	return spinnerGlyphs[m.spinFrame%len(spinnerGlyphs)]
}

func (m Model) spinnerView() string {
	if !m.spinnerVisible() {
		return ""
	}
	glyph := m.spinnerGlyph() + " "
	if m.cardOpen() {
		return renderSegs(m.width,
			seg{glyph, styleFG(m.theme.Accent)},
			seg{"Waiting for your answer", styleFG(m.theme.Warn)},
		)
	}
	if m.viewing != "" {
		return m.subagentSpinnerView()
	}
	text := m.spinnerActivity() + " · " + m.spinnerElapsed()
	if m.status == statusWorking {
		text += " · esc to interrupt"
	}
	return renderSegs(m.width,
		seg{glyph, styleFG(m.theme.Accent)},
		seg{sanitizeLine(text), styleFG(m.theme.Dim)},
	)
}

func (m Model) turnElapsed() string {
	if m.turnStart.IsZero() || m.frozen {
		return formatElapsed(0)
	}
	return formatElapsed(m.now().Sub(m.turnStart))
}

// spinnerActivity names what the turn is doing. A compaction of the
// session's context wins — nothing else of the turn moves while it runs —
// then a sub-agent in flight, then the tool kinds, then the thought stream.
func (m Model) spinnerActivity() string {
	if m.compacting("") {
		return transcript.CompactingLabel
	}
	var tasks, exec, edit, read int
	var execCmd, editPath, readPath string
	for i := range m.snap.Tools {
		t := &m.snap.Tools[i]
		if t.Status != "pending" && t.Status != "in_progress" {
			continue
		}
		switch {
		case t.IsTask():
			tasks++
		case t.Kind == "execute":
			exec++
			if execCmd == "" {
				execCmd = execCommand(t)
			}
		case t.Kind == "edit":
			edit++
			if editPath == "" {
				editPath = toolPath(t)
			}
		case t.Kind == "read":
			read++
			if readPath == "" {
				readPath = toolPath(t)
			}
		}
	}
	// After end_turn the tools are settled but the children may not be; the
	// rows are what is still running then — a bash job's aside (plan 033
	// §3.8): it is no sub-agent waited for.
	if m.status != statusWorking {
		running := 0
		for i := range m.snap.Subagents {
			if s := m.snap.Subagents[i]; subagentRunning(s) && !bashJobRow(s) {
				running++
			}
		}
		tasks = max(tasks, running)
	}
	switch {
	case tasks > 0:
		return fmt.Sprintf("Waiting for %d sub-agent%s", tasks, plural(tasks))
	case exec > 0:
		return strings.TrimSpace("Running " + firstNonEmpty(execCmd))
	case edit > 0:
		return strings.TrimSpace("Editing " + m.displayPath(m.main, editPath))
	case read > 0:
		return strings.TrimSpace("Reading " + m.displayPath(m.main, readPath))
	case m.lastThought:
		return "Thinking…"
	}
	return "Working"
}

// compacting reports whether the shared model has a compaction of scope's
// context open ("" the main session's, else a child's): the working line
// reads transcript.CompactingLabel while it does (plan 028 §3.13). It is the
// fold's, so a client restored mid-compaction reads the same (seam 7).
func (m Model) compacting(scope string) bool {
	if m.shared == nil {
		return false
	}
	tr := scopeOf(m.shared, scope)
	return tr != nil && tr.Compacting() != nil
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
