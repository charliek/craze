package tui

import (
	"strings"
	"testing"

	"github.com/charliek/craze/internal/agent"
)

func TestGrokHidesFastKeepsSubagentRows(t *testing.T) {
	m := sized(t)
	for i, c := range m.snap.Config {
		if c.ID == "fast" {
			m.snap.Config[i].Current = "true"
		}
	}
	if !strings.Contains(m.modelLabel(), "fast") {
		t.Fatalf("setup: cursor should show fast, got %q", m.modelLabel())
	}
	m.snap.Tools = []agent.ToolEvent{taskTool("t1", "count main.go lines", "in_progress")}
	m.snap.Subagents = []agent.SubagentInfo{{
		ID: "t1", Status: agent.SubagentRunning, Description: "count main.go lines",
	}}
	if len(m.agentItems()) == 0 {
		t.Fatal("setup: cursor should show a sub-agent row")
	}
	m.snap.Provider = agent.GrokProvider().Info()
	if strings.Contains(m.modelLabel(), "fast") {
		t.Fatalf("grok must hide fast, got %q", m.modelLabel())
	}
	if !strings.Contains(m.modelLabel(), "medium") {
		t.Fatalf("grok should still show effort, got %q", m.modelLabel())
	}
	if len(m.agentItems()) == 0 {
		t.Fatal("grok now shows sub-agent rows")
	}
	if m.agentCount() == "" {
		t.Fatal("grok agent count should be set")
	}
}

func TestGrokHelpKeepsSubagentKeys(t *testing.T) {
	m := sized(t)
	m.snap.Provider = agent.GrokProvider().Info()
	keys := m.helpKeyLines()
	joined := ""
	for _, l := range keys {
		joined += l.key + " " + l.desc + "\n"
	}
	if !strings.Contains(joined, "shift+tab") {
		t.Fatal("grok still has modes; help must keep shift+tab")
	}
	if !strings.Contains(joined, "sub-agent") {
		t.Fatalf("grok help should mention sub-agents:\n%s", joined)
	}
}

func TestSlashHidesModeCommandsWithoutModes(t *testing.T) {
	m := sized(t)
	m.snap.Modes = nil
	m.snap.Provider = agent.GrokProvider().Info()
	for _, name := range []string{"plan", "ask", "agent"} {
		if _, ok := catalogByName(m, name); ok {
			t.Fatalf("%s still in catalog without modes", name)
		}
	}
}
