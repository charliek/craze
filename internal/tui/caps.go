package tui

import "github.com/charliek/craze/internal/agent"

func (m Model) caps() agent.Capabilities {
	return m.snap.Provider.Capabilities()
}

func (m Model) showFast() bool {
	return m.caps().FastToggle && agent.FastOption(m.snap) != nil
}

func (m Model) showEffort() bool {
	return m.caps().Effort && agent.EffortOption(m.snap) != nil
}

func (m Model) showModes() bool {
	return m.caps().Modes && len(m.snap.Modes) > 0
}

func (m Model) showSubagents() bool {
	return m.caps().SubagentRows
}

func (m Model) showTodos() bool {
	return m.caps().Todos
}

func (m Model) showAsk() bool {
	return m.caps().AskCards
}

func (m Model) showPlan() bool {
	return m.caps().PlanCards
}

func (m Model) providerValue() agent.Provider {
	p, err := agent.ProviderByName(m.snap.Provider.Name)
	if err != nil {
		return agent.CursorProvider()
	}
	return p
}

func (m Model) sessionBinary() string {
	if m.sess == nil {
		return ""
	}
	return m.sess.Binary()
}
