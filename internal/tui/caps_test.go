package tui

import (
	"strings"
	"testing"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/backend"
	"github.com/charliek/craze/internal/engine"
)

// capsOverrideBackend wraps a Backend and answers Info with a different
// Capabilities than the wrapped backend's own — standing in for a host over
// the socket, whose advertised set is never rebuilt from the client binary's
// own provider table (§3.13, astra 25). engine() forwards to whatever the
// wrapped backend names, as jitterBackend's does, so engineOf/engineBehind
// still reach it.
type capsOverrideBackend struct {
	backend.Backend
	caps agent.Capabilities
}

func (b *capsOverrideBackend) engine() *engine.Engine { return engineBehind(b.Backend) }

func (b *capsOverrideBackend) Info() backend.SessionInfo {
	info := b.Backend.Info()
	info.Capabilities = b.caps
	return info
}

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
	// caps() now reads the backend's advertised set (§3.13), so the stub's
	// own provider must change too, not only the mirror's.
	stubOf(t, m).SetProvider(agent.GrokProvider())
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

// TestAHostWithDifferentCapabilitiesDrivesTheControls (§3.13, "The provider's
// local vocabulary"; X33 4): a backend whose Info() advertises capabilities
// different from its provider table's — standing in for a host over the
// socket, whose advertised set is never rebuilt from the client binary's own
// table (astra 25) — drives every control m.caps() gates from the advertised
// set, while the provider's local vocabulary (a mode id's kind, the implement
// prompt) still comes from the name, untouched by the override.
func TestAHostWithDifferentCapabilitiesDrivesTheControls(t *testing.T) {
	m, _ := cursorStub(t, "claude-opus-5")
	real := agent.CursorProvider().Capabilities()
	if got := m.caps(); got != real {
		t.Fatalf("setup: caps() is %+v, want cursor's own table %+v", got, real)
	}
	if !m.showFast() || !m.showEffort() {
		t.Fatal("setup: claude-opus-5's catalog should show both chips on cursor's table")
	}
	m.status = statusWorking
	if hint := m.composerHint(); hint != queueSendNowHint {
		t.Fatalf("setup: cursor's hint is %q, want the send-now hint", hint)
	}
	child := agent.SubagentInfo{ID: "t1", Status: agent.SubagentRunning}
	if m.canStopSubagent(child) {
		t.Fatal("setup: cursor's table has no per-child stop")
	}
	wantKind := m.snap.Provider.Kind("plan")
	wantPrompt := m.snap.Provider.ImplementPrompt()

	// The advertised set is cursor's exact opposite on every bit this test
	// drives a control from: FastToggle and Effort go off (cursor has both
	// on), Interject and SubagentCancel go on (cursor has neither). A reader
	// still keyed to the table, not the backend, would fail every assertion
	// below.
	advertised := agent.Capabilities{Interject: true, SubagentCancel: true}
	m.eng = &capsOverrideBackend{Backend: m.eng, caps: advertised}

	if got := m.caps(); got != advertised {
		t.Fatalf("caps() answered %+v, want the backend's advertised %+v", got, advertised)
	}
	if m.showFast() {
		t.Fatal("showFast must follow the advertised capabilities, not cursor's table")
	}
	if m.showEffort() {
		t.Fatal("showEffort must follow the advertised capabilities, not cursor's table")
	}
	if hint := m.composerHint(); hint != queueInterjectHint {
		t.Fatalf("composerHint is %q, want the interject hint from the advertised Interject bit", hint)
	}
	if !m.canStopSubagent(child) {
		t.Fatal("canStopSubagent must follow the advertised SubagentCancel bit, not cursor's table")
	}

	// The vocabulary rides the name alone, never the advertised Capabilities:
	// unchanged by the override above.
	if kind := m.snap.Provider.Kind("plan"); kind != wantKind {
		t.Fatalf("the mode vocabulary changed under the override: got %v, want %v", kind, wantKind)
	}
	if p := m.snap.Provider.ImplementPrompt(); p != wantPrompt {
		t.Fatalf("the implement prompt changed under the override: got %q, want %q", p, wantPrompt)
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
