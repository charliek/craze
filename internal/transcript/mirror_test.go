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

// filled is a value of T with every field set, every nested slice holding two
// elements and every pointer pointing at a value filled the same way
// (fillAll, tools_test.go): a payload that reaches every field a projection
// can hand out.
func filled[T any](t testing.TB) T {
	t.Helper()
	var v T
	fillAll(t, reflect.ValueOf(&v).Elem(), "orig", true)
	return v
}

// fullMirrorPrefix is a stream after which every field of the model's Mirror
// holds something, down to the last nested slice and pointer: every settings
// section, usage included, full; a queued row; a roster row listing the tools
// it used; a todo list; a turn started; a foreign-turn bracket running; and
// fully populated tools on main and on a child.
func fullMirrorPrefix(t testing.TB) []agent.Event {
	t.Helper()
	title, mode, model := "a title", "plan", "grok"
	usage, sendNow := filled[agent.UsageState](t), filled[agent.SendNowState](t)
	row := filled[agent.QueuedPrompt](t)
	row.ID = "q1"
	child := filled[agent.SubagentInfo](t)
	child.ID, child.Status = "c1", agent.SubagentRunning
	foreign := filled[agent.ForeignTurnInfo](t)
	foreign.Running = true
	return sequenced([]agent.Event{
		{Type: agent.EventMeta, State: &agent.StateDelta{
			Title: &title, Mode: &mode, Model: &model,
			Config:   &agent.ConfigState{Options: filled[[]agent.ConfigOption](t)},
			Commands: &agent.CommandsState{Commands: filled[[]agent.CommandInfo](t)},
			Plugins:  &agent.PluginsState{Plugins: filled[[]agent.PluginCommand](t)},
			SendNow:  &sendNow,
			Usage:    &usage,
		}},
		{Type: agent.EventQueue, QueueChange: agent.QueueQueued, Queue: &row},
		{Type: agent.EventSubagent, SubagentChange: agent.SubagentChangeSpawned, Subagent: &child},
		{Type: agent.EventTodos, Todos: filled[[]agent.Todo](t)},
		{Type: agent.EventTurn, Turn: &agent.TurnInfo{ID: "turn-1", Phase: agent.TurnStarted, Text: "go", Origin: agent.TurnOriginSubmit}},
		{Type: agent.EventTool, Tool: ownedTool(t, "t1")},
		{Type: agent.EventTool, Agent: "c1", Tool: ownedTool(t, "ct1")},
		{Type: agent.EventForeignTurn, ForeignTurn: &foreign},
	})
}

// TestTheMirrorIsTheCallersOwn (astra r53 8): every value Mirror hands out is
// the caller's own, down to the last slice and pointer — the settings' sections
// and their options' values, the queue, the roster rows and what they list, the
// todo list, the foreign-turn bracket, the tools and their payloads. A caller
// writing through every field it can reach (scribble's walk, over the whole
// Mirror) changes nothing the model's next Mirror, State or Snapshot reports:
// no fact moves without an event. Negative control: Mirror handing out any one
// of them shared — Turn.Foreign's pointer, say, or a roster row's ToolsUsed —
// goes red here.
func TestTheMirrorIsTheCallersOwn(t *testing.T) {
	m := New(Options{})
	foldAll(t, m, true, fullMirrorPrefix(t)...)
	// The expected projections come from a twin folded from a prefix of its
	// own — fresh payloads, sharing nothing with m or anything m handed out —
	// so a write that reached m cannot reach them too.
	twin := New(Options{})
	foldAll(t, twin, false, fullMirrorPrefix(t)...)
	if !reflect.DeepEqual(m.Mirror(), twin.Mirror()) {
		t.Fatal("fixture: the twin folded from the same prefix differs")
	}
	mr := m.Mirror()
	// scribble's walk fails the test on a nil pointer or an empty slice it
	// would have to write through: the prefix reaches every field there is.
	fillAll(t, reflect.ValueOf(&mr).Elem(), "scribbled", false)
	if got, want := m.Mirror(), twin.Mirror(); !reflect.DeepEqual(got, want) {
		t.Fatalf("writing into a Mirror reached the next one:\nwant %+v\ngot  %+v", want, got)
	}
	if got, want := m.State(), twin.State(); !reflect.DeepEqual(got, want) {
		t.Fatalf("writing into a Mirror reached the state projection:\nwant %+v\ngot  %+v", want, got)
	}
	got, err := m.Snapshot(0)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	want, err := twin.Snapshot(0)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("writing into a Mirror reached a snapshot:\nwant %+v\ngot  %+v", want, got)
	}
}

// TestAMirrorDoesNotRaceTheModel is TestTheMirrorIsTheCallersOwn under the race
// detector: a writer goroutine writes every field of the Mirrors it is handed
// while the reader goes on folding, snapshotting — which encodes every section,
// roster row, todo and bracket the model retains — and taking the next Mirror.
// Both start at one barrier and the writer keeps writing the last Mirror it was
// handed until the reader is done. Nothing orders the writer's writes before
// the reader's reads, so any memory a Mirror shared with the model would be
// reported. Negative control: as above; under -race it reports the data race.
func TestAMirrorDoesNotRaceTheModel(t *testing.T) {
	m := New(Options{})
	prefix := fullMirrorPrefix(t)
	foldAll(t, m, false, prefix...)
	handed := make(chan Mirror, 1)
	start, stop, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		<-start
		var last *Mirror
		for {
			select {
			case <-stop:
				return
			case mr := <-handed:
				last = &mr
			default:
			}
			if last != nil {
				fillAll(noT{}, reflect.ValueOf(last).Elem(), "scribbled", false)
			}
		}
	}()
	close(start)
	seq := uint64(len(prefix))
	for range 200 {
		select {
		case handed <- m.Mirror():
		default:
		}
		if _, err := m.Snapshot(0); err != nil {
			t.Errorf("Snapshot: %v", err)
			break
		}
		seq++
		m.Fold(agent.Event{Type: agent.EventText, Text: "x", Seq: seq})
		_ = m.Mirror()
		_ = m.State()
	}
	close(stop)
	<-done
}
