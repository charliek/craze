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
type Mirror struct {
	// Seq is the last event folded.
	Seq uint64
	// Settings is every StateDelta section as the last delta that carried it
	// left it (Model's Settings). Config is the caller's own slice of
	// options; Commands and Plugins are the model's, never written after the
	// delta that replaced them, as State's are.
	Settings Settings
	// Queue is the message queue, front first, in a slice of the caller's own.
	Queue []agent.QueuedPrompt
	// Agents is the roster, in the order the rows first appeared, in a slice
	// of the caller's own.
	Agents []agent.SubagentInfo
	// Todos is the todo list, as the last EventTodos carried it.
	Todos []agent.Todo
	// Turn is the turn the stream last started, and the agent's own turn if
	// one is running.
	Turn Turn
	// Tools is Tools(): every retained tool's last state in entry order, main
	// first and then each child's in creation order, each the caller's own.
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
		Todos:    m.todos,
		Turn:     m.turn,
	}
	if len(m.settings.Config) > 0 {
		mr.Settings.Config = slices.Clone(m.settings.Config)
	}
	if u := m.settings.Usage; u != nil {
		// The caller's own copy, as every cut's is (cut's Usage): a value
		// type, so one copy owns it whole.
		cu := *u
		mr.Settings.Usage = &cu
	}
	if len(m.queue) > 0 {
		mr.Queue = slices.Clone(m.queue)
	}
	if len(m.agentOrder) > 0 {
		mr.Agents = make([]agent.SubagentInfo, 0, len(m.agentOrder))
		for _, id := range m.agentOrder {
			mr.Agents = append(mr.Agents, m.agents[id].info)
		}
	}
	mr.Tools = appendTools(mr.Tools, m.Main.live())
	for _, id := range m.subOrder {
		mr.Tools = appendTools(mr.Tools, m.subs[id].live())
	}
	return mr
}
