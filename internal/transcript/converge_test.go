package transcript

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
)

var convergeBase = time.Date(2026, 9, 21, 9, 0, 0, 0, time.UTC)

func strp(s string) *string { return &s }

func at(sec int) time.Time { return convergeBase.Add(time.Duration(sec) * time.Second) }

// priorEvents is a session with something in every part of the state: an open
// run, tools in the main transcript and a child's, an open ask of each kind,
// a roster, a queue, todos, settings and a running turn. Every convergence
// fixture is applied on top of it.
func priorEvents() []agent.Event {
	return sequenced([]agent.Event{
		{Type: agent.EventMeta, State: &agent.StateDelta{
			Title: strp("session"), Mode: strp("agent"), Model: strp("grok-4.6"),
			Config:   &agent.ConfigState{Options: []agent.ConfigOption{{ID: "effort", Current: "high"}}},
			Commands: &agent.CommandsState{Commands: []agent.CommandInfo{{Name: "review"}}},
			Plugins:  &agent.PluginsState{Plugins: []agent.PluginCommand{{Qualified: "p:x"}}},
		}, At: at(1)},
		{Type: agent.EventTurn, Turn: &agent.TurnInfo{ID: "turn-1", Phase: agent.TurnStarted, Text: "go", Origin: agent.TurnOriginSubmit}, At: at(2)},
		{Type: agent.EventSubagent, Subagent: &agent.SubagentInfo{ID: "sub-1", Status: agent.SubagentRunning}, SubagentChange: agent.SubagentChangeSpawned, At: at(3)},
		{Type: agent.EventTool, Agent: "sub-1", Tool: &agent.ToolEvent{ID: "c1", Status: "pending", Title: "Read"}, At: at(4)},
		{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: "t1", Status: "pending", Title: "Shell"}, At: at(5)},
		{Type: agent.EventTodos, Todos: []agent.Todo{{ID: "1", Content: "one", Status: "pending"}}, At: at(6)},
		{Type: agent.EventQueue, Queue: &agent.QueuedPrompt{ID: "q1", Text: "first"}, QueueChange: agent.QueueQueued, QueuePos: 0, At: at(7)},
		{Type: agent.EventQueue, Queue: &agent.QueuedPrompt{ID: "q2", Text: "second"}, QueueChange: agent.QueueQueued, QueuePos: 1, At: at(8)},
		{Type: agent.EventPermission, Permission: &agent.PermissionEvent{ID: "perm-1", Tool: "Shell"}, At: at(9)},
		{Type: agent.EventQuestion, Question: &agent.QuestionEvent{ID: "ask-1", Title: "Q"}, At: at(10)},
		{Type: agent.EventThought, Text: "weighing", At: at(11)},
	})
}

// A convergenceFixture is one state or marker event and the prior it lands on
// (priorEvents when prior is nil).
type convergenceFixture struct {
	name  string
	prior []agent.Event
	ev    agent.Event
}

func convergenceFixtures() []convergenceFixture {
	ev := func(e agent.Event) agent.Event { e.At = at(100); return e }
	meta := func(st agent.StateDelta) agent.Event { return ev(agent.Event{Type: agent.EventMeta, State: &st}) }
	queue := func(id, text string, change agent.QueueChange, pos int) agent.Event {
		return ev(agent.Event{Type: agent.EventQueue, Queue: &agent.QueuedPrompt{ID: id, Text: text, Version: 2}, QueueChange: change, QueuePos: pos})
	}
	roster := func(id string, status agent.SubagentStatus, change string, ended time.Time) agent.Event {
		return ev(agent.Event{Type: agent.EventSubagent, Subagent: &agent.SubagentInfo{ID: id, Status: status, EndedAt: ended}, SubagentChange: change})
	}
	// A roster already holding Bounds.Agents finished rows, so the next finish
	// evicts one.
	var full []agent.Event
	for i := range DefaultBounds().Agents {
		id := fmt.Sprintf("old-%02d", i)
		full = append(full,
			roster(id, agent.SubagentRunning, agent.SubagentChangeSpawned, time.Time{}),
			roster(id, agent.SubagentCompleted, agent.SubagentChangeFinished, at(i)))
	}
	full = sequenced(append(full, roster("new", agent.SubagentRunning, agent.SubagentChangeSpawned, time.Time{})))

	return []convergenceFixture{
		{name: "tool update", ev: ev(agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: "t1", Status: "completed", Title: "Shell"}})},
		{name: "tool new", ev: ev(agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: "t9", Status: "pending"}})},
		{name: "child tool", ev: ev(agent.Event{Type: agent.EventTool, Agent: "sub-1", Tool: &agent.ToolEvent{ID: "c1", Status: "completed"}})},
		{name: "todos", ev: ev(agent.Event{Type: agent.EventTodos, Todos: []agent.Todo{{ID: "1", Status: "completed"}, {ID: "2", Status: "pending"}}})},
		{name: "todos cleared", ev: ev(agent.Event{Type: agent.EventTodos})},
		{name: "permission opens", ev: ev(agent.Event{Type: agent.EventPermission, Permission: &agent.PermissionEvent{ID: "perm-2", Tool: "Write"}})},
		{name: "question opens", ev: ev(agent.Event{Type: agent.EventQuestion, Question: &agent.QuestionEvent{ID: "ask-2"}})},
		{name: "auto question", ev: ev(agent.Event{Type: agent.EventQuestion, Question: &agent.QuestionEvent{ID: "ask-3", Auto: true}})},
		{name: "plan opens", ev: ev(agent.Event{Type: agent.EventPlan, Plan: &agent.PlanEvent{ID: "plan-1", Plan: "steps"}})},
		{name: "auto plan", ev: ev(agent.Event{Type: agent.EventPlan, Plan: &agent.PlanEvent{ID: "plan-2", Auto: true}})},
		{name: "ask ended", ev: ev(agent.Event{Type: agent.EventAsk, Ask: &agent.AskUpdate{ID: "perm-1", Kind: agent.AskPermission, Outcome: agent.AskAnswered, By: agent.AskByClient}})},
		{name: "unknown ask ended", ev: ev(agent.Event{Type: agent.EventAsk, Ask: &agent.AskUpdate{ID: "perm-9", Kind: agent.AskPermission, Outcome: agent.AskAutomatic, By: agent.AskByPolicy}})},
		{name: "done", ev: ev(agent.Event{Type: agent.EventDone, StopReason: "end_turn"})},
		{name: "done cancelled", ev: ev(agent.Event{Type: agent.EventDone, StopReason: "cancelled"})},
		{name: "title", ev: meta(agent.StateDelta{Title: strp("renamed")})},
		{name: "title cleared", ev: meta(agent.StateDelta{Title: strp("")})},
		{name: "mode", ev: meta(agent.StateDelta{Mode: strp("plan")})},
		{name: "model", ev: meta(agent.StateDelta{Model: strp("composer-2.5")})},
		{name: "config", ev: meta(agent.StateDelta{Config: &agent.ConfigState{Options: []agent.ConfigOption{{ID: "fast", Current: "on"}}}})},
		{name: "config cleared", ev: meta(agent.StateDelta{Config: &agent.ConfigState{}})},
		{name: "commands", ev: meta(agent.StateDelta{Commands: &agent.CommandsState{Commands: []agent.CommandInfo{{Name: "simplify"}}}})},
		{name: "plugins", ev: meta(agent.StateDelta{Plugins: &agent.PluginsState{Plugins: []agent.PluginCommand{{Qualified: "p:y"}}}})},
		{name: "send now armed", ev: meta(agent.StateDelta{SendNow: &agent.SendNowState{Armed: true, Text: "now", Turn: "turn-1"}})},
		{name: "send now gone", ev: meta(agent.StateDelta{SendNow: &agent.SendNowState{}, Reason: agent.SendNowWithdrawn})},
		{name: "reason alone", ev: meta(agent.StateDelta{Reason: agent.SendNowCancelFailed, Detail: "cancel failed"})},
		{name: "index error", ev: meta(agent.StateDelta{IndexErr: "disk full"})},
		{name: "every section", ev: meta(agent.StateDelta{
			Title: strp("t"), Mode: strp("ask"), Model: strp("m"),
			Config:   &agent.ConfigState{Options: []agent.ConfigOption{{ID: "o"}}},
			Commands: &agent.CommandsState{}, Plugins: &agent.PluginsState{},
			SendNow: &agent.SendNowState{Armed: true, Text: "x"},
		})},
		{name: "agent-initiated mode", ev: func() agent.Event {
			e := meta(agent.StateDelta{Mode: strp("plan")})
			e.Mode = "plan"
			return e
		}()},
		{name: "subagent spawned", ev: roster("sub-2", agent.SubagentRunning, agent.SubagentChangeSpawned, time.Time{})},
		{name: "subagent progress", ev: func() agent.Event {
			e := roster("sub-1", agent.SubagentRunning, agent.SubagentChangeProgress, time.Time{})
			e.Subagent.Activity = "reading"
			return e
		}()},
		{name: "subagent finished", ev: roster("sub-1", agent.SubagentCompleted, agent.SubagentChangeFinished, at(99))},
		{name: "subagent finished evicts", prior: full, ev: roster("new", agent.SubagentCompleted, agent.SubagentChangeFinished, at(99))},
		{name: "queued", ev: queue("q3", "third", agent.QueueQueued, 0)},
		{name: "duplicate queued", ev: queue("q2", "second again", agent.QueueQueued, 0)},
		{name: "edited", ev: queue("q1", "first, edited", agent.QueueEdited, 0)},
		{name: "removed", ev: queue("q1", "first", agent.QueueRemoved, 0)},
		{name: "sent", ev: queue("q2", "second", agent.QueueSent, 0)},
		{name: "foreign turn starts", ev: ev(agent.Event{Type: agent.EventForeignTurn, ForeignTurn: &agent.ForeignTurnInfo{ID: "ft-1", Running: true}})},
		{name: "foreign turn ends", ev: ev(agent.Event{Type: agent.EventForeignTurn, ForeignTurn: &agent.ForeignTurnInfo{ID: "ft-1"}})},
		{name: "replay starts", ev: ev(replayEvent(agent.ReplayStart))},
		{name: "replay ends", ev: ev(replayEvent(agent.ReplayEnd))},
		{name: "turn started", ev: ev(agent.Event{Type: agent.EventTurn, Turn: &agent.TurnInfo{ID: "turn-2", Phase: agent.TurnStarted, Text: "next", Origin: agent.TurnOriginDrain}})},
		{name: "turn ended", ev: ev(agent.Event{Type: agent.EventTurn, Turn: &agent.TurnInfo{ID: "turn-1", Phase: agent.TurnEnded, StopReason: "end_turn"}})},
		{name: "turn ended cancelled", ev: ev(agent.Event{Type: agent.EventTurn, Turn: &agent.TurnInfo{ID: "turn-1", Phase: agent.TurnEnded, StopReason: "cancelled", Synthetic: true}})},
		{name: "turn ended refused", ev: ev(agent.Event{Type: agent.EventTurn, Turn: &agent.TurnInfo{ID: "turn-1", Phase: agent.TurnEnded, Synthetic: true, Err: "refused"}})},
	}
}

// TestStateCarryingKindsConvergeUnderReapplication (plan 024 A5, §3.7): for
// every state and marker row, a fixture e on a model S converges on the state
// projection — Fold(Fold(S, e), e).State() == Fold(S, e).State() — and so does
// a third application with no Seq at all (a unit fixture's). History is not
// compared: a row whose defined effect is to append appends again, which
// TestHistoryIsAppendOnly pins. Every state or marker kind of the table has a
// fixture here, and every fixture's kind is one.
func TestStateCarryingKindsConvergeUnderReapplication(t *testing.T) {
	covered := map[agent.EventType]bool{}
	for _, fx := range convergenceFixtures() {
		row := kinds[fx.ev.Type]
		if row.class&(classState|classMarker) == 0 {
			t.Errorf("%s: kind %q is neither state nor marker; it has no business here", fx.name, fx.ev.Type)
		}
		covered[fx.ev.Type] = true
		t.Run(fx.name, func(t *testing.T) {
			prior := fx.prior
			if prior == nil {
				prior = priorEvents()
			}
			m := New(Options{})
			foldAll(t, m, true, prior...)
			e := fx.ev
			e.Seq = uint64(len(prior) + 1)

			m.Fold(e)
			once := m.State()
			m.Fold(e)
			twice := m.State()
			checkInvariants(t, m)
			if !reflect.DeepEqual(once, twice) {
				t.Fatalf("re-applying moved the state:\nonce  %+v\ntwice %+v", once, twice)
			}
			e.Seq = 0
			m.Fold(e)
			if thrice := m.State(); !reflect.DeepEqual(once, thrice) {
				t.Fatalf("re-applying unsequenced moved the state:\nonce   %+v\nthrice %+v", once, thrice)
			}
			checkInvariants(t, m)
		})
	}
	for k, row := range kinds {
		if row.class&(classState|classMarker) != 0 && !covered[k] {
			t.Errorf("kind %q is a state or marker row with no convergence fixture", k)
		}
	}
}

// TestHistoryIsAppendOnly (plan 024 A5): history is not convergent and is not
// meant to be. A re-applied chunk is two chunks, a re-applied note, plan entry,
// bracket or started appends again, and an id-less tool — history, not state —
// appends a row each time; the todo notes, deduplicated by design, are the one
// stream effect a repeat does not redraw. Every second entry gets an id of its
// own (X2), and nothing indexed is disturbed.
func TestHistoryIsAppendOnly(t *testing.T) {
	cases := []struct {
		name string
		ev   agent.Event
		// kind and want: how many entries of kind there are after two, and the
		// last one's text ("" = not checked).
		kind, text string
		want       int
	}{
		{name: "a chunk", ev: agent.Event{Type: agent.EventText, Text: "ab"}, kind: "assistant", want: 1, text: "abab"},
		{name: "a thought chunk", ev: agent.Event{Type: agent.EventThought, Text: "hm"}, kind: "thought", want: 1, text: "hmhm"},
		{name: "a cancelled note", ev: agent.Event{Type: agent.EventDone, StopReason: "cancelled"}, kind: "note", want: 2, text: NoteCancelled},
		{name: "a plan entry", ev: agent.Event{Type: agent.EventPlan, Plan: stubPlanEvent()}, kind: "plan", want: 2},
		{name: "a replay end", ev: replayEvent(agent.ReplayEnd), kind: "note", want: 2, text: NoteRestored},
		{name: "a foreign turn", ev: agent.Event{Type: agent.EventForeignTurn, ForeignTurn: &agent.ForeignTurnInfo{ID: "ft", Running: true}}, kind: "note", want: 2, text: NoteForeignTurn},
		{name: "a started", ev: agent.Event{Type: agent.EventTurn, Turn: &agent.TurnInfo{ID: "turn-1", Phase: agent.TurnStarted, Text: "hi"}}, kind: "user", want: 2, text: "hi"},
		{name: "an id-less tool", ev: agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{Title: "anonymous"}}, kind: "tool", want: 2},
		{name: "a command line", ev: agent.Event{Type: agent.EventCommand, Command: &agent.ExpandedCommand{PluginCommand: agent.PluginCommand{Qualified: "p:c"}}}, kind: "note", want: 2, text: "⤷ p:c"},
		{name: "an error", ev: agent.Event{Type: agent.EventError, Err: errors.New("boom")}, kind: "error", want: 2, text: "boom"},
		{name: "an interjection", ev: agent.Event{Type: agent.EventUser, Interjection: true, Text: "also"}, kind: "user", want: 2, text: "also"},
		{name: "a replayed prompt", ev: agent.Event{Type: agent.EventUser, Replayed: true, Text: "old"}, kind: "user", want: 2, text: "old"},
		{name: "a detail", ev: agent.Event{Type: agent.EventMeta, State: &agent.StateDelta{Detail: "d"}}, kind: "error", want: 2, text: "d"},
		{name: "a synthetic cancel", ev: agent.Event{Type: agent.EventTurn, Turn: &agent.TurnInfo{Phase: agent.TurnEnded, Synthetic: true, StopReason: "cancelled"}}, kind: "note", want: 2, text: NoteCancelled},
		{name: "a child's chunk", ev: agent.Event{Type: agent.EventText, Agent: "sub", Text: "x"}, kind: "assistant", want: 1, text: "xx"},
		{name: "the todo notes (deduplicated)", ev: agent.Event{Type: agent.EventTodos, Todos: []agent.Todo{{ID: "1", Status: "pending"}}}, kind: "note", want: 1, text: "tasks: 1 planned"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := New(Options{})
			e := tc.ev
			e.Seq = 7
			m.Fold(e)
			m.Fold(e)
			checkInvariants(t, m)
			tr := m.ensureSub(tc.ev.Agent)
			got := factsOf(tr, tc.kind)
			if len(got) != tc.want {
				t.Fatalf("after two: %d %s entries, want %d: %v", len(got), tc.kind, tc.want, facts(tr))
			}
			if tc.text != "" && got[len(got)-1].Text != tc.text {
				t.Fatalf("after two: the last %s reads %q, want %q", tc.kind, got[len(got)-1].Text, tc.text)
			}
			es := tr.live()
			if n := len(es); n >= 2 && es[n-1].ID.Seq == es[n-2].ID.Seq && es[n-1].ID.N == es[n-2].ID.N {
				t.Fatalf("the re-applied entry reused its twin's id %v", es[n-1].ID)
			}
		})
	}
}
