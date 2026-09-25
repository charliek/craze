package tui

import (
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/craze/internal/agent"
)

// stopRecorder is a test-only decorator over *Stub with the per-child stop
// (agent.SubagentCanceller): it records every id the engine asks it to stop
// and changes nothing else, so a row it was asked about stays running and a
// second press asks again. Everything it does not name is the Stub's.
type stopRecorder struct {
	*Stub

	stopMu sync.Mutex
	stops  []string
}

func (s *stopRecorder) CancelSubagent(id string) error {
	s.stopMu.Lock()
	defer s.stopMu.Unlock()
	s.stops = append(s.stops, id)
	return nil
}

// stopped is every id the engine asked to stop, in order.
func (s *stopRecorder) stopped() []string {
	s.stopMu.Lock()
	defer s.stopMu.Unlock()
	return slices.Clone(s.stops)
}

// stopModel is a started model over a stopRecorder that says it is provider
// p, on a clock the test owns (so a finished row lingers), with one row per
// sub-agent in subs — spawned, or finished when its status is terminal.
func stopModel(t *testing.T, p agent.Provider, subs ...agent.SubagentInfo) (Model, *stopRecorder) {
	t.Helper()
	isolateSkillsHome(t)
	now := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	rec := &stopRecorder{Stub: NewStub()}
	rec.Clock = func() time.Time { return now }
	rec.SetProvider(p)
	m := New(Config{Session: rec, Theme: "tokyo-night", Workspace: t.TempDir(), Model: "grok", Yolo: true})
	m.clock = func() time.Time { return now }
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 30})
	m = tm.(Model)
	tm, _ = m.Update(startedMsg{})
	m = tm.(Model)
	rec.SetSubagents(subs)
	for i := range subs {
		info := subs[i]
		change := agent.SubagentChangeSpawned
		if subagentTerminal(info) {
			change = agent.SubagentChangeFinished
		}
		tm, _ = m.Update(eventMsg{agent.Event{Type: agent.EventSubagent, Subagent: &info, SubagentChange: change}})
		m = tm.(Model)
	}
	return m, rec
}

func stopKid(id, desc string, status agent.SubagentStatus) agent.SubagentInfo {
	return agent.SubagentInfo{ID: id, Status: status, Description: desc, SubagentType: "general-purpose", ToolCallID: "call-" + id}
}

// TestStopKeyFallsThroughWithoutCapability (A12, §3.10): the stop key is the
// stop only where the session has the verb and the child is running; anywhere
// else it does exactly what it did before. On grok's running row and on
// native's finished row, Backspace hands the keyboard back to the composer and
// the draft loses a character; in a grok child's view Backspace and Delete are
// swallowed as they always were. No stop reaches the session in any of them —
// which, with the verb, it would record.
func TestStopKeyFallsThroughWithoutCapability(t *testing.T) {
	for _, tc := range []struct {
		name string
		p    agent.Provider
		kid  agent.SubagentInfo
	}{
		{"grok, a running row", agent.GrokProvider(), stopKid("kid-1", "job one", agent.SubagentRunning)},
		{"native, a finished row", agent.NativeProvider(), stopKid("kid-1", "job one", agent.SubagentCompleted)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, rec := stopModel(t, tc.p, tc.kid)
			m.input.SetValue("hey")
			m = pressKey(t, m, tea.KeyDown)
			if !m.agentFocus || m.agentID != "kid-1" {
				t.Fatalf("control: down focused the rows %v on %q", m.agentFocus, m.agentID)
			}
			m = pressKey(t, m, tea.KeyBackspace)
			if m.agentFocus || !m.input.Focused() || m.input.Value() != "he" {
				t.Fatalf("backspace on the row: rows focused %v, composer focused %v, draft %q; want the composer's key",
					m.agentFocus, m.input.Focused(), m.input.Value())
			}
			if got := rec.stopped(); len(got) != 0 {
				t.Fatalf("the key stopped %q", got)
			}
		})
	}
	t.Run("grok, a running child's view", func(t *testing.T) {
		m, rec := stopModel(t, agent.GrokProvider(), stopKid("kid-1", "job one", agent.SubagentRunning))
		m.input.SetValue("hey")
		m = pressKey(t, pressKey(t, m, tea.KeyDown), tea.KeyEnter)
		if m.viewing != "kid-1" {
			t.Fatalf("control: viewing %q, want kid-1", m.viewing)
		}
		for _, k := range []tea.KeyType{tea.KeyBackspace, tea.KeyDelete} {
			m = pressKey(t, m, k)
		}
		if m.viewing != "kid-1" || m.input.Value() != "hey" {
			t.Fatalf("the view gave the key away: viewing %q, draft %q", m.viewing, m.input.Value())
		}
		if got := rec.stopped(); len(got) != 0 {
			t.Fatalf("the key stopped %q", got)
		}
	})
}

// TestStopKeyStopsTheRunningChild (A12, §3.10): with the verb, Backspace and
// Delete each stop the running child the keyboard is on — the focused row, or
// the child in view — through the engine, once per press with that child's id
// and a command of its own (a reused command would be replayed, not asked
// again). The keyboard stays where it was and the draft is untouched; with no
// engine the key is still the stop's, and does nothing.
func TestStopKeyStopsTheRunningChild(t *testing.T) {
	m, rec := stopModel(t, agent.NativeProvider(),
		stopKid("kid-1", "job one", agent.SubagentRunning), stopKid("kid-2", "job two", agent.SubagentRunning))
	m.input.SetValue("hey")
	m = pressKey(t, m, tea.KeyDown)
	want := []string{}
	for _, step := range []struct {
		key  tea.KeyType
		move tea.KeyType // pressed first; 0 for none
		id   string
	}{
		{tea.KeyBackspace, 0, "kid-1"},
		{tea.KeyDelete, 0, "kid-1"},
		{tea.KeyDelete, tea.KeyDown, "kid-2"},
		{tea.KeyBackspace, 0, "kid-2"},
	} {
		if step.move != 0 {
			m = pressKey(t, m, step.move)
		}
		m = pressKey(t, m, step.key)
		want = append(want, step.id)
		if got := rec.stopped(); !slices.Equal(got, want) {
			t.Fatalf("after %v on %s the session was asked to stop %q; want %q", step.key, step.id, got, want)
		}
		if !m.agentFocus || m.agentID != step.id || m.input.Focused() || m.input.Value() != "hey" {
			t.Fatalf("after the stop: rows focused %v on %q, composer focused %v, draft %q; want the rows kept, the draft untouched",
				m.agentFocus, m.agentID, m.input.Focused(), m.input.Value())
		}
	}
	m = pressKey(t, m, tea.KeyEnter)
	if m.viewing != "kid-2" {
		t.Fatalf("control: viewing %q, want kid-2", m.viewing)
	}
	for _, k := range []tea.KeyType{tea.KeyBackspace, tea.KeyDelete} {
		m = pressKey(t, m, k)
		want = append(want, "kid-2")
		if got := rec.stopped(); !slices.Equal(got, want) {
			t.Fatalf("after %v in the view the session was asked to stop %q; want %q", k, got, want)
		}
		if m.viewing != "kid-2" || m.input.Value() != "hey" {
			t.Fatalf("after the stop in the view: viewing %q, draft %q; want the view kept", m.viewing, m.input.Value())
		}
	}

	m.eng = nil
	if m = pressKey(t, m, tea.KeyDelete); m.viewing != "kid-2" || m.input.Value() != "hey" {
		t.Fatalf("with no engine: viewing %q, draft %q; want the key taken and nothing done", m.viewing, m.input.Value())
	}
}

// TestStopHintAndHelpNeedTheCapability (§3.10, V9): the banner hint shows on a
// running child's view where the session has the verb — at the end of the
// banner — and nowhere else: not on grok's running view, not on native's
// finished one; and the help dialog's line is the verb's alone too.
func TestStopHintAndHelpNeedTheCapability(t *testing.T) {
	banner := func(p agent.Provider, status agent.SubagentStatus) string {
		m, _ := stopModel(t, p, stopKid("kid-1", "job one", status))
		m = pressKey(t, pressKey(t, m, tea.KeyDown), tea.KeyEnter)
		if m.viewing != "kid-1" {
			t.Fatalf("control: viewing %q, want kid-1", m.viewing)
		}
		return plain(m.subagentBanner())
	}
	if got := banner(agent.NativeProvider(), agent.SubagentRunning); !strings.Contains(got, "○ @general-purpose · read-only · esc to return · del to stop") {
		t.Fatalf("native's running view: banner %q; want the hint at its end", got)
	}
	for name, got := range map[string]string{
		"grok's running view":    banner(agent.GrokProvider(), agent.SubagentRunning),
		"native's finished view": banner(agent.NativeProvider(), agent.SubagentCompleted),
	} {
		if strings.Contains(got, "del to stop") || !strings.Contains(got, "esc to return") {
			t.Fatalf("%s: banner %q; want no hint", name, got)
		}
	}

	helpHas := func(p agent.Provider) bool {
		m, _ := stopModel(t, p)
		return slices.Contains(m.helpKeyLines(), helpLine{key: "del, backspace", desc: "stop the selected running sub-agent, or the one in view"})
	}
	if !helpHas(agent.NativeProvider()) || helpHas(agent.GrokProvider()) || helpHas(agent.CursorProvider()) {
		t.Fatalf("the help line: native %v, grok %v, cursor %v; want native alone",
			helpHas(agent.NativeProvider()), helpHas(agent.GrokProvider()), helpHas(agent.CursorProvider()))
	}
}
