package tui

import (
	"slices"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/craze/internal/agent"
)

// A native session's background bash job is a roster row (plan 033 P12): the
// type agent.BashJobType, background, no model. It draws as a background
// sub-agent's row does — the bg mark, its elapsed time, del to stop, its own
// view — but it is no sub-agent waited for (§3.8 "Display"): a dev server can
// run for hours, so a running job neither keeps the working line and its fast
// tick on nor refuses /connect.

// jobRow is a job's roster row as the native adapter publishes it.
func jobRow(id, cmd string, status agent.SubagentStatus) agent.SubagentInfo {
	return agent.SubagentInfo{ID: id, ToolCallID: id, Status: status, Description: cmd, Prompt: cmd,
		SubagentType: agent.BashJobType, Background: true, Transcript: true}
}

// TestBashJobRowLeavesTheSpinnerAndConnect: with only a running job's row,
// nothing is working — no spinner, the slow tick, /connect free — while its
// row still draws with its bg mark and an elapsed time the slow tick moves on;
// a running background sub-agent's row in its place keeps all of them busy:
// the negative control. Opened, the job's own view keeps its spinner moving,
// the one place one is drawn for it.
func TestBashJobRowLeavesTheSpinnerAndConnect(t *testing.T) {
	kid := stopKid("kid-1", "scan", agent.SubagentRunning)
	kid.Background = true
	for _, c := range []struct {
		name string
		row  agent.SubagentInfo
		busy bool
	}{
		{"a bash job", jobRow("t1.1.1", "npm run dev", agent.SubagentRunning), false},
		{"a background sub-agent", kid, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			m, _ := stopModel(t, agent.NativeProvider(), c.row)
			if got := []bool{m.anySubagentRunning(), m.spinnerVisible(), m.wantFastTick(), m.connectBusy()}; !slices.Equal(got, []bool{c.busy, c.busy, c.busy, c.busy}) {
				t.Fatalf("running %v, spinner %v, fast tick %v, /connect busy %v; want all %v", got[0], got[1], got[2], got[3], c.busy)
			}
			if !strings.Contains(m.View(), c.row.Description) {
				t.Fatalf("the row is not drawn:\n%s", m.View())
			}
			later := m.now().Add(2 * time.Minute)
			m.clock = func() time.Time { return later }
			if got, want := m.agentSuffix(c.row), "bg · "+formatElapsed(2*time.Minute); got != want {
				t.Fatalf("the row's suffix two minutes on is %q; want %q", got, want)
			}
		})
	}

	m, _ := stopModel(t, agent.NativeProvider(), jobRow("t1.1.1", "npm run dev", agent.SubagentRunning))
	m = pressKey(t, m, tea.KeyDown)
	m = pressKey(t, m, tea.KeyEnter)
	if m.viewing != "t1.1.1" {
		t.Fatalf("viewing %q; want the job's view", m.viewing)
	}
	if !m.spinnerVisible() || !m.wantFastTick() {
		t.Fatalf("in the job's view: spinner %v, fast tick %v; want both", m.spinnerVisible(), m.wantFastTick())
	}
}

// TestConnectOpensWhileAJobRuns: /connect, refused while a sub-agent runs
// (TestConnectRefusedWhileWorkRuns), opens while only a bash job runs.
func TestConnectOpensWhileAJobRuns(t *testing.T) {
	dir, getenv := connectFixture(t, nil)
	stub := nativeStub()
	m := connectModel(t, stub, dir, getenv)
	job := jobRow("t1.1.1", "npm run dev", agent.SubagentRunning)
	stub.SetSubagents([]agent.SubagentInfo{job})
	tm, _ := m.Update(eventMsg{ev: agent.Event{Type: agent.EventSubagent, Subagent: &job, SubagentChange: agent.SubagentChangeSpawned}})
	m = tm.(Model)
	if len(m.snap.Subagents) != 1 {
		t.Fatalf("premise: the job's row is not in the snapshot: %+v", m.snap.Subagents)
	}
	m, _ = typeCommand(t, m, "/connect")
	if m.dialog != dialogConnect {
		t.Fatalf("/connect opened dialog %v with only a job running; want the connect dialog (errors %q)", m.dialog, texts(m, entryError))
	}
}

// TestStopKeyOnAJobRow (§3.8's table): del on a running job's row asks the
// session to stop that id, as on a sub-agent's — the native session routes it
// to the job, whose result then says the user stopped it.
func TestStopKeyOnAJobRow(t *testing.T) {
	m, rec := stopModel(t, agent.NativeProvider(), jobRow("t1.1.1", "npm run dev", agent.SubagentRunning))
	m = pressKey(t, m, tea.KeyDown)
	m, cmd := pressKeyCmd(t, m, tea.KeyDelete)
	if cmd == nil {
		t.Fatal("del on the job's row returned no Cmd; want the stop")
	}
	runCmd(cmd)
	if got := rec.stopped(); !slices.Equal(got, []string{"t1.1.1"}) {
		t.Fatalf("the session was asked to stop %q; want the job's id", got)
	}
	if !m.agentFocus || m.agentID != "t1.1.1" {
		t.Fatalf("after the stop: focused %v on %q; want the job's row kept", m.agentFocus, m.agentID)
	}
}
