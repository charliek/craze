package tui

import "github.com/charliek/craze/internal/agent"

func (m Model) caps() agent.Capabilities {
	return m.snap.Provider.Capabilities()
}

// The provider's Effort and FastToggle bits gate three things, and no control
// the catalog advertises is among them (plan 025 design 4, CodeRabbit 11). A
// bit is fixed per provider, and on cursor what a model takes changes with the
// model, so the catalog is the one authority on which controls there are: the
// model dialog draws a tab for every option the current catalog advertises and
// reads neither bit (catalogTabs). What the bits still gate:
//
//   - showEffort and showFast: the status row's effort and fast chips
//     (modelLabel), and nothing else. A chip is a summary of a value, not a
//     way to change one, so gating it hides no control.
//   - effortShorthand: `/model <id> <effort>`'s reading of a last word as an
//     effort. It is a second way to type what the dialog's effort tab sets;
//     with it off the dialog still offers every effort the catalog has.

// showFast is the fast chip's gate: the provider has the toggle and the current
// catalog advertises it.
func (m Model) showFast() bool {
	return m.caps().FastToggle && agent.FastOption(m.snap) != nil
}

// showEffort is the effort chip's gate: the provider has efforts and the
// current catalog advertises one.
func (m Model) showEffort() bool {
	return m.caps().Effort && agent.EffortOption(m.snap) != nil
}

// effortShorthand is whether `/model <id> <effort>` may read its last word as
// an effort (agent.MatchModelEffort). It is the capability bit alone and not
// showEffort: the shorthand is judged against the model it switches TO, and
// whether the model the session is on now has an effort says nothing about
// that one (plan 025 design 5) — gated on the current catalog, `/model grok-4.6
// high` typed on composer-2.5 would be the unknown model "grok-4.6 high" again.
// Off, the whole argument is the model's name, never split, which is what the
// shorthand always came to on a session with no effort to match.
func (m Model) effortShorthand() bool {
	return m.caps().Effort
}

func (m Model) showModes() bool {
	return m.caps().Modes && len(m.snap.Modes) > 0
}

func (m Model) showSubagents() bool {
	return m.caps().SubagentRows
}

func (m Model) showSubagentTranscript() bool {
	return m.caps().SubagentTranscript
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
