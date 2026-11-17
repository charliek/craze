package tui

import (
	"testing"

	"github.com/charliek/craze/internal/agent"
)

// claudeCatalog is claude-agent-acp's catalog, as its session/new answers it
// (L0's real frames): mode, model, effort, fast and ultracode.
func claudeCatalog() []agent.ConfigOption {
	sel := func(id, name, cat, cur string, vals ...string) agent.ConfigOption {
		opt := agent.ConfigOption{ID: id, Name: name, Category: cat, Type: "select", Current: cur}
		for _, v := range vals {
			opt.SelectValues = append(opt.SelectValues, agent.SelectValue{Value: v, Name: v})
		}
		return opt
	}
	return []agent.ConfigOption{
		sel("model", "Model", "model", "opus", "opus", "haiku"),
		sel("effort", "Effort", "thought_level", "high", "low", "high"),
		sel("fast", "Fast mode", "model_config", "off", "on", "off"),
		sel("ultracode", "Ultracode", "model_config", "off", "on", "off"),
	}
}

// TestClaudeModelDialogHidesFast (plan 052 § D3): claude's picker offers no
// fast tab — fast mode bills extra usage on a Claude plan — and keeps every
// other option the catalog has. Cursor's keeps its fast tab.
func TestClaudeModelDialogHidesFast(t *testing.T) {
	cfg := claudeCatalog()
	for _, tc := range []struct {
		p        agent.Provider
		wantFast bool
	}{{agent.ClaudeProvider(), false}, {agent.CursorProvider(), true}} {
		tabs := catalogTabs(agent.Snapshot{Provider: tc.p.Info(), Config: cfg})
		var fast, ultra bool
		for _, tab := range tabs {
			fast = fast || tab.opt.ID == "fast"
			ultra = ultra || tab.opt.ID == "ultracode"
		}
		if fast != tc.wantFast || !ultra {
			t.Fatalf("%s: fast tab %v, ultracode tab %v", tc.p.Name(), fast, ultra)
		}
	}
}

// TestClaudeImplementsInManualAndOffersNoPlan (plan 052 § C1, § B5): claude's
// implement mode is default (Manual), and its empty implement prompt is what
// keeps a plan-mode turn from offering one.
func TestClaudeImplementsInManualAndOffersNoPlan(t *testing.T) {
	m := Model{}
	m.snap.Provider = agent.ClaudeProvider().Info()
	for _, id := range []string{"default", "acceptEdits", "plan", "auto", "bypassPermissions"} {
		m.snap.Modes = append(m.snap.Modes, agent.ModeInfo{ID: id})
	}
	m.snap.CurrentMode = "plan"
	if got := m.implementModeID(); got != "default" {
		t.Fatalf("implement mode %q", got)
	}
	if m.snap.Provider.ImplementPrompt() != "" {
		t.Fatal("claude has an implement prompt")
	}
	if sessProviderDetail["claude"] != "your Claude Code, on your Claude plan" {
		t.Fatalf("detail %q", sessProviderDetail["claude"])
	}
}
