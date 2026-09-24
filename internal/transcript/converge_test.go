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

// capPrior is priorEvents behind three named tools, so the transcript's head
// is tool state: at a retention boundary, the next entries a trim drops are
// the last states of h0, h1 and h2.
func capPrior() []agent.Event {
	var head []agent.Event
	for i := range 3 {
		head = append(head, agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: fmt.Sprintf("h%d", i)}, At: at(0)})
	}
	return sequenced(append(head, priorEvents()...))
}

// A convergenceFixture is one state or marker event and the prior it lands on
// (priorEvents when prior is nil, and then at each retention boundary too),
// under bounds (DefaultBounds' when zero). appends says the event's
// re-application appends history by design (TestHistoryIsAppendOnly) — a
// note, a plan entry, a user or an error row — which the test checks against
// what the fold reports.
type convergenceFixture struct {
	name    string
	prior   []agent.Event
	bounds  Bounds
	ev      agent.Event
	appends bool
}

// A retention is where a fixture's prior leaves the transcripts: inside the
// bounds, or exactly at the entry caps or the byte budgets with named tools at
// the head (capPrior), so the next append trims tool state.
type retention struct {
	name   string
	prior  []agent.Event
	bounds Bounds
}

// retentionsOf is every retention a fixture runs at: its own prior and bounds
// when it has them, else priorEvents inside the bounds and capPrior at both
// boundaries, main and child alike.
func retentionsOf(fx convergenceFixture) []retention {
	if fx.prior != nil || fx.bounds != (Bounds{}) {
		prior := fx.prior
		if prior == nil {
			prior = priorEvents()
		}
		return []retention{{name: "its own", prior: prior, bounds: fx.bounds}}
	}
	capped := capPrior()
	probe := New(Options{})
	for _, ev := range capped {
		probe.Fold(ev)
	}
	sub := probe.Sub("sub-1")
	return []retention{
		{name: "inside the bounds", prior: priorEvents()},
		{name: "at the entry caps", prior: capped, bounds: Bounds{MainEntries: probe.Main.len(), SubEntries: sub.len()}},
		{name: "at the byte budgets", prior: capped, bounds: Bounds{MainBytes: probe.Main.bytes, SubBytes: sub.bytes}},
	}
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
		{name: "plan opens", ev: ev(agent.Event{Type: agent.EventPlan, Plan: &agent.PlanEvent{ID: "plan-1", Plan: "steps"}}), appends: true},
		{name: "auto plan", ev: ev(agent.Event{Type: agent.EventPlan, Plan: &agent.PlanEvent{ID: "plan-2", Auto: true}})},
		{name: "ask ended", ev: ev(agent.Event{Type: agent.EventAsk, Ask: &agent.AskUpdate{ID: "perm-1", Kind: agent.AskPermission, Outcome: agent.AskAnswered, By: agent.AskByClient}})},
		{name: "unknown ask ended", ev: ev(agent.Event{Type: agent.EventAsk, Ask: &agent.AskUpdate{ID: "perm-9", Kind: agent.AskPermission, Outcome: agent.AskAutomatic, By: agent.AskByPolicy}})},
		{name: "done", ev: ev(agent.Event{Type: agent.EventDone, StopReason: "end_turn"})},
		{name: "done cancelled", ev: ev(agent.Event{Type: agent.EventDone, StopReason: "cancelled"}), appends: true},
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
		{name: "reason alone", ev: meta(agent.StateDelta{Reason: agent.SendNowCancelFailed, Detail: "cancel failed"}), appends: true},
		{name: "index error", ev: meta(agent.StateDelta{IndexErr: "disk full"}), appends: true},
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
		// r2 finding 2's schedule: the budget is 10 bytes, a 1-byte tool at the
		// head and a 9-byte note fill it, and the update adds a byte. Were an
		// update in place to trim, it would drop its own row, and the update
		// re-applied would append the row again.
		{name: "an update past the byte budget, its row at the head", prior: sequenced([]agent.Event{
			{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: "t"}, At: at(1)},
			{Type: agent.EventDone, StopReason: "cancelled", At: at(2)},
		}), bounds: Bounds{MainBytes: 10}, ev: ev(agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: "t", RawInput: "x"}})},
		{name: "queued", ev: queue("q3", "third", agent.QueueQueued, 0)},
		{name: "duplicate queued", ev: queue("q2", "second again", agent.QueueQueued, 0)},
		{name: "edited", ev: queue("q1", "first, edited", agent.QueueEdited, 0)},
		{name: "removed", ev: queue("q1", "first", agent.QueueRemoved, 0)},
		{name: "sent", ev: queue("q2", "second", agent.QueueSent, 0)},
		{name: "foreign turn starts", ev: ev(agent.Event{Type: agent.EventForeignTurn, ForeignTurn: &agent.ForeignTurnInfo{ID: "ft-1", Running: true}}), appends: true},
		{name: "foreign turn ends", ev: ev(agent.Event{Type: agent.EventForeignTurn, ForeignTurn: &agent.ForeignTurnInfo{ID: "ft-1"}})},
		{name: "replay starts", ev: ev(replayEvent(agent.ReplayStart))},
		{name: "replay ends", ev: ev(replayEvent(agent.ReplayEnd)), appends: true},
		{name: "turn started", ev: ev(agent.Event{Type: agent.EventTurn, Turn: &agent.TurnInfo{ID: "turn-2", Phase: agent.TurnStarted, Text: "next", Origin: agent.TurnOriginDrain}}), appends: true},
		{name: "turn ended", ev: ev(agent.Event{Type: agent.EventTurn, Turn: &agent.TurnInfo{ID: "turn-1", Phase: agent.TurnEnded, StopReason: "end_turn"}})},
		{name: "turn ended cancelled", ev: ev(agent.Event{Type: agent.EventTurn, Turn: &agent.TurnInfo{ID: "turn-1", Phase: agent.TurnEnded, StopReason: "cancelled", Synthetic: true}}), appends: true},
		{name: "turn ended refused", ev: ev(agent.Event{Type: agent.EventTurn, Turn: &agent.TurnInfo{ID: "turn-1", Phase: agent.TurnEnded, Synthetic: true, Err: "refused"}}), appends: true},
	}
}

// TestStateCarryingKindsConvergeUnderReapplication (plan 024 A5, §3.7): for
// every state and marker row, a fixture e on a model S converges on the state
// projection — Fold(Fold(S, e), e).State() == Fold(S, e).State() — and so does
// a third application with no Seq at all (a unit fixture's). History is not
// compared: a row whose defined effect is to append appends again, which
// TestHistoryIsAppendOnly pins. Every state or marker kind of the table has a
// fixture here, and every fixture's kind is one.
//
// Each fixture on priorEvents runs inside the bounds and at both retention
// boundaries — the entry caps and the byte budgets, main and child — with
// named tools at the head, where the next append trims tool state (r2
// finding 2; r2 also added its own schedule as a fixture). A re-application
// that appends nothing converges on the whole projection there too. One whose
// defined effect is to append history (appends) converges on everything but
// the tools that history displaced off the front — trimming is history's
// effect, not the row's state: the re-application changes no tool's state and
// re-adds none, it only lets the oldest go, as any appended row would (see
// TestReappliedHistoryDisplacesToolStateAtTheCap). Every fold that changes the
// tools' states says so in Change.State (r2 finding 6).
func TestStateCarryingKindsConvergeUnderReapplication(t *testing.T) {
	covered := map[agent.EventType]bool{}
	displaced := 0
	for _, fx := range convergenceFixtures() {
		row := kinds[fx.ev.Type]
		if row.class&(classState|classMarker) == 0 {
			t.Errorf("%s: kind %q is neither state nor marker; it has no business here", fx.name, fx.ev.Type)
		}
		covered[fx.ev.Type] = true
		for _, r := range retentionsOf(fx) {
			t.Run(fx.name+"/"+r.name, func(t *testing.T) {
				m := New(Options{Bounds: r.bounds})
				foldAll(t, m, true, r.prior...)
				if r.bounds != (Bounds{}) && trimmed(m.Main) {
					t.Fatal("the prior itself trimmed: the retention is past its boundary, not at it")
				}
				fold := func(e agent.Event) (State, Change) {
					t.Helper()
					before := m.State()
					c := m.Fold(e)
					after := m.State()
					checkInvariants(t, m)
					if !reflect.DeepEqual(before.Tools, after.Tools) && !c.State {
						t.Fatalf("the fold changed the tools' states without saying so: %+v", c)
					}
					return after, c
				}
				e := fx.ev
				e.Seq = uint64(len(r.prior) + 1)
				once, _ := fold(e)
				twice, c2 := fold(e)
				if assertConverged(t, fx, "re-applying", once, twice, c2, c2.Dropped) {
					displaced++
				}
				e.Seq = 0
				thrice, c3 := fold(e)
				assertConverged(t, fx, "re-applying unsequenced", once, thrice, c3, c2.Dropped+c3.Dropped)
			})
		}
	}
	for k, row := range kinds {
		if row.class&(classState|classMarker) != 0 && !covered[k] {
			t.Errorf("kind %q is a state or marker row with no convergence fixture", k)
		}
	}
	if displaced == 0 {
		t.Error("no re-application displaced a tool: the retention fixtures no longer reach their boundaries, so nothing here tests them")
	}
}

// assertConverged holds again — the state after re-applying a fixture — to
// once, the state after its first application, and reports whether they
// differ by displaced tools alone. c is the re-application's Change, and
// dropped what the re-applications have trimmed between them.
func assertConverged(t *testing.T, fx convergenceFixture, what string, once, again State, c Change, dropped int) bool {
	t.Helper()
	if appended := !c.AppendedFrom.IsZero(); appended != fx.appends {
		t.Fatalf("%s appended=%v, but the fixture says its re-application appends=%v: %+v", what, appended, fx.appends, c)
	}
	if reflect.DeepEqual(once, again) {
		return false
	}
	if !fx.appends || dropped == 0 {
		t.Fatalf("%s moved the state:\nonce  %+v\nagain %+v", what, once, again)
	}
	o, a := once, again
	o.Tools, a.Tools = nil, nil
	if !reflect.DeepEqual(o, a) {
		t.Fatalf("%s moved the state beyond the tools its history displaced:\nonce  %+v\nagain %+v", what, o, a)
	}
	for k, tool := range again.Tools {
		if once.Tools[k] != tool {
			t.Fatalf("%s changed or re-added tool %v: it may only displace", what, k)
		}
	}
	return true
}

// TestReappliedHistoryDisplacesToolStateAtTheCap is the limit of the
// convergence claim, pinned so it is a decision and not a surprise (r2
// finding 2's broader schedule): at the entry cap, with named tools at the
// head, each re-application of a row that appends history — a cancelled
// done's note here — trims the oldest entry, and when that entry is a tool,
// the tool's last state leaves State().Tools. The first application drops h0,
// the second h1, the third h2: the projection a second application leaves is
// not the first's. The tool state is bounded by the transcript's retention,
// not kept beside it, so this holds for every row with a history half
// (convergenceFixtures' appends).
func TestReappliedHistoryDisplacesToolStateAtTheCap(t *testing.T) {
	m := New(Options{Bounds: Bounds{MainEntries: 3}})
	for i := range 3 {
		m.Fold(agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: fmt.Sprintf("h%d", i)}, Seq: uint64(i + 1)})
	}
	done := agent.Event{Type: agent.EventDone, StopReason: "cancelled", Seq: 4}
	var held []int
	for range 3 {
		c := m.Fold(done)
		checkInvariants(t, m)
		if !c.State || c.Dropped != 1 {
			t.Fatalf("the note displaced a tool and must say the state changed: %+v", c)
		}
		held = append(held, len(m.State().Tools))
	}
	if held[0] != 2 || held[1] != 1 || held[2] != 0 {
		t.Fatalf("each re-applied note displaces the oldest tool: %v tools left", held)
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
