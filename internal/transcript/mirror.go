package transcript

import (
	"slices"

	"github.com/charliek/craze/internal/agent"
)

// Mirror is what a client draws from beside its transcripts (plan 027 §3.13,
// "The mirror onto the fold"): the settings, the queue, the roster, the todo
// list, the turn and the ordered tools — State's fields and Tools' list, as
// one read.
//
// It is read under the model's lock without cutting any transcript: no entry
// list is copied and no open run's tail is built, so a client can take it
// after every fold — a streamed chunk included — without paying for the size
// of the transcripts it holds. The tools are walked, entry by entry, which is
// what their order is.
//
// Every value it holds is the caller's own, down to the last slice and
// pointer — the settings' sections and their options' values, the queue, the
// roster rows and what they list, the todo list, the foreign-turn bracket, the
// tools and their payloads: nothing a caller writes into one reaches the
// model, or races its next fold.
type Mirror struct {
	// Seq is the last event folded.
	Seq uint64
	// Settings is every StateDelta section as the last delta that carried it
	// left it (Model's Settings).
	Settings Settings
	// Queue is the message queue, front first.
	Queue []agent.QueuedPrompt
	// Agents is the roster, in the order the rows first appeared.
	Agents []agent.SubagentInfo
	// Todos is the todo list, as the last EventTodos carried it.
	Todos []agent.Todo
	// Turn is the turn the stream last started, and the agent's own turn if
	// one is running.
	Turn Turn
	// Tools is Tools(): every retained tool's last state in entry order, main
	// first and then each child's in creation order — a bash job's left out
	// (jobScope).
	Tools []agent.ToolEvent
}

// Mirror returns the mirror projection (see Mirror). It equals State's
// Settings, Queue, Agents, Todos and Turn and Tools' list at the same Seq,
// which TestTheMirrorIsStatesFieldsAndTools holds it to.
func (m *Model) Mirror() Mirror {
	m.mu.Lock()
	defer m.mu.Unlock()
	mr := Mirror{
		Seq:      m.seq,
		Settings: m.settings,
		Todos:    slices.Clone(m.todos),
		Turn:     m.turn,
	}
	if len(m.queue) > 0 {
		// An emptied queue is nil, as State's is.
		mr.Queue = slices.Clone(m.queue)
	}
	mr.Settings.Config = cloneOptions(m.settings.Config)
	mr.Settings.Commands = slices.Clone(m.settings.Commands)
	mr.Settings.Plugins = slices.Clone(m.settings.Plugins)
	if u := m.settings.Usage; u != nil {
		// A value type, so one copy owns it whole (as every cut's Usage is).
		cu := *u
		mr.Settings.Usage = &cu
	}
	if f := m.turn.Foreign; f != nil {
		cf := *f
		mr.Turn.Foreign = &cf
	}
	if len(m.agentOrder) > 0 {
		mr.Agents = make([]agent.SubagentInfo, 0, len(m.agentOrder))
		for _, id := range m.agentOrder {
			info := m.agents[id].info
			info.ToolsUsed = slices.Clone(info.ToolsUsed)
			mr.Agents = append(mr.Agents, info)
		}
	}
	mr.Tools = appendTools(mr.Tools, m.Main.live())
	for _, id := range m.subOrder {
		if jobScope(m.agents[id].info) {
			continue
		}
		mr.Tools = appendTools(mr.Tools, m.subs[id].live())
	}
	return mr
}

// jobScope reports whether the scope of the roster row info is a native
// session's background bash job's (plan 033 P12: background, of type
// agent.BashJobType, which no sub-agent's type can be): its one execute row
// stays in its own transcript, for its view, and out of the ordered tools a
// client reads what is working from (plan 033 C10r, V3 F2). A job is no work
// of the turn's — a dev server runs for hours — so its running command is
// neither counted in the session's in-flight tools ("· 2 shells") nor named
// by the working line ("Running sleep 900"), as its roster row is neither
// the spinner's nor /connect's (internal/tui's bashJobRow). A scope with no
// row is a child's.
func jobScope(info agent.SubagentInfo) bool {
	return info.Background && info.SubagentType == agent.BashJobType
}

// cloneOptions is a config catalog the caller owns: each option, and each
// option's own list of values, copied.
func cloneOptions(opts []agent.ConfigOption) []agent.ConfigOption {
	out := slices.Clone(opts)
	for i := range out {
		out[i].SelectValues = slices.Clone(out[i].SelectValues)
	}
	return out
}
