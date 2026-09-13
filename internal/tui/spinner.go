package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
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
	if m.tickFast {
		m.spinFrame++
	}
}

func (m Model) wantFastTick() bool {
	return m.status == statusWorking || m.cardOpen() || m.tasksLingering() ||
		m.agentLingering() || m.copyLingering()
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
	return m.status == statusWorking || m.cardOpen()
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
	text := m.spinnerActivity() + " · " + m.turnElapsed() + " · esc to interrupt"
	return renderSegs(m.width,
		seg{glyph, styleFG(m.theme.Accent)},
		seg{sanitizeLine(text), styleFG(m.theme.Dim)},
	)
}

func (m Model) turnElapsed() string {
	if m.turnStart.IsZero() {
		return formatElapsed(0)
	}
	return formatElapsed(m.now().Sub(m.turnStart))
}

// spinnerActivity names what the turn is doing. A sub-agent in flight wins,
// then the tool kinds, then the thought stream.
func (m Model) spinnerActivity() string {
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
	switch {
	case tasks > 0:
		return fmt.Sprintf("Waiting for %d sub-agent%s", tasks, plural(tasks))
	case exec > 0:
		return strings.TrimSpace("Running " + firstNonEmpty(execCmd))
	case edit > 0:
		return strings.TrimSpace("Editing " + m.displayPath(m.cur(), editPath))
	case read > 0:
		return strings.TrimSpace("Reading " + m.displayPath(m.cur(), readPath))
	case m.lastThought:
		return "Thinking…"
	}
	return "Working"
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
