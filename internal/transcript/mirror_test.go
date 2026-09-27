package transcript

import (
	"reflect"
	"strings"
	"testing"

	"github.com/charliek/craze/internal/agent"
)

// mirrorPrefix is a stream touching every field Mirror carries: the settings
// sections, queue rows added, edited and removed, a roster with a child that
// spawned and one that finished, a todo list, a turn started and a foreign
// turn running, and tools on main and on a child, one updated in place.
func mirrorPrefix() []agent.Event {
	title, mode, model := "a title", "plan", "grok"
	return []agent.Event{
		{Type: agent.EventMeta, State: &agent.StateDelta{
			Title: &title, Mode: &mode, Model: &model,
			Config:   &agent.ConfigState{Options: []agent.ConfigOption{{ID: "effort", Current: "high", SelectValues: []agent.SelectValue{{Value: "high"}}}}},
			Commands: &agent.CommandsState{Commands: []agent.CommandInfo{{Name: "research"}}},
			Plugins:  &agent.PluginsState{Plugins: []agent.PluginCommand{{Qualified: "p:c"}}},
			SendNow:  &agent.SendNowState{Armed: true, Text: "now", Turn: "turn-1"},
		}},
		{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: "t1", Status: "pending", Locations: []string{"a.go"}}},
		{Type: agent.EventQueue, QueueChange: agent.QueueQueued, Queue: &agent.QueuedPrompt{ID: "q1", Text: "one"}},
		{Type: agent.EventQueue, QueueChange: agent.QueueQueued, Queue: &agent.QueuedPrompt{ID: "q2", Text: "two"}, QueuePos: 1},
		{Type: agent.EventQueue, QueueChange: agent.QueueEdited, Queue: &agent.QueuedPrompt{ID: "q1", Text: "one, edited", Version: 1}},
		{Type: agent.EventQueue, QueueChange: agent.QueueQueued, Queue: &agent.QueuedPrompt{ID: "q3", Text: "three"}, QueuePos: 2},
		{Type: agent.EventQueue, QueueChange: agent.QueueRemoved, Queue: &agent.QueuedPrompt{ID: "q2", Text: "two"}},
		{Type: agent.EventSubagent, SubagentChange: agent.SubagentChangeSpawned, Subagent: &agent.SubagentInfo{ID: "c1", Status: agent.SubagentRunning}},
		{Type: agent.EventSubagent, SubagentChange: agent.SubagentChangeSpawned, Subagent: &agent.SubagentInfo{ID: "c2", Status: agent.SubagentRunning}},
		{Type: agent.EventTool, Agent: "c1", Tool: &agent.ToolEvent{ID: "ct1", Status: "pending"}},
		{Type: agent.EventSubagent, SubagentChange: agent.SubagentChangeFinished, Subagent: &agent.SubagentInfo{ID: "c2", Status: agent.SubagentCompleted}},
		{Type: agent.EventTodos, Todos: []agent.Todo{{ID: "1", Content: "Read", Status: "in_progress"}}},
		{Type: agent.EventTurn, Turn: &agent.TurnInfo{ID: "turn-2", Phase: agent.TurnStarted, Text: "go", Origin: agent.TurnOriginSubmit}},
		{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: "t2", Status: "pending"}},
		{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: "t1", Status: "completed", Locations: []string{"a.go"}}},
		{Type: agent.EventText, Text: strings.Repeat("streaming ", 20)},
		{Type: agent.EventForeignTurn, ForeignTurn: &agent.ForeignTurnInfo{ID: "f1", Running: true}},
	}
}

// TestTheMirrorIsStatesFieldsAndTools (plan 027 §3.13): Mirror is State's
// settings, queue, roster, todo list and turn and Tools' ordered list, at the
// same Seq — after every event of a prefix that moves each of them, so a field
// Mirror reads from the wrong place, or stops following, is caught where it
// first differs. Negative control: Mirror leaving the roster out (or reading
// the queue before an edit) goes red at the first event that moves it.
func TestTheMirrorIsStatesFieldsAndTools(t *testing.T) {
	m := New(Options{})
	for i, ev := range sequenced(mirrorPrefix()) {
		m.Fold(ev)
		st, mr := m.State(), m.Mirror()
		want := Mirror{
			Seq: st.Seq, Settings: st.Settings, Queue: st.Queue, Agents: st.Agents,
			Todos: st.Todos, Turn: st.Turn, Tools: m.Tools(),
		}
		if !reflect.DeepEqual(mr, want) {
			t.Fatalf("after event %d (%s) the mirror differs from State and Tools:\nmirror %+v\nwant   %+v", i, ev.Type, mr, want)
		}
	}
	mr := m.Mirror()
	if len(mr.Queue) != 2 || len(mr.Agents) != 2 || len(mr.Tools) != 3 || mr.Turn.Foreign == nil || !mr.Settings.SendNow.Armed {
		t.Fatalf("fixture: the prefix did not reach every field: %+v", mr)
	}
}

// TestTheMirrorIsTheCallersOwn: the slices Mirror hands out that a client
// writes into — the options (a status test sets one's Current), the queue, the
// roster, the tools — are its own, so nothing it writes reaches the model.
// Negative control: Mirror handing out m.settings.Config or m.queue itself
// goes red on the next State().
func TestTheMirrorIsTheCallersOwn(t *testing.T) {
	m := New(Options{})
	foldAll(t, m, false, sequenced(mirrorPrefix())...)
	before := m.State()
	mr := m.Mirror()
	mr.Settings.Config[0].Current = "scribbled"
	mr.Queue[0].Text = "scribbled"
	mr.Agents[0].Status = agent.SubagentFailed
	mr.Tools[0].Locations[0] = "scribbled"
	if after := m.State(); !reflect.DeepEqual(after, before) {
		t.Fatalf("writing into the mirror reached the model:\nbefore %+v\nafter  %+v", before, after)
	}
}
