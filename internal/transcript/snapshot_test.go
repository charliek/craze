package transcript

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/charliek/craze/internal/agent"
)

// remoteErr is an error as the engine's observer and every decoding client
// hold one (X14): a *agent.RemoteError, which the fold reads and the history
// projection compares by value.
func remoteErr(msg string) error {
	return &agent.RemoteError{Message: msg, Class: agent.EventErrOther}
}

// sessionScript is a session with something in every part of the model: runs
// open and closed in the main transcript and two children's, tools and their
// updates (an id-less one too), the todo notes, an ask of each kind and an
// ending, the roster with a finish, the queue, every settings section, an
// error, a foreign turn, a replay bracket, a command line, an interjection, a
// cancel and a synthetic failure — and it ends mid-thought. Its times are UTC
// with no monotonic reading, so the codec gives them back equal.
func sessionScript() []agent.Event {
	cfg := []agent.ConfigOption{{ID: "effort", Name: "Effort", Current: "high", SelectValues: []agent.SelectValue{{Value: "high"}}}}
	return sequenced([]agent.Event{
		{Type: agent.EventMeta, State: &agent.StateDelta{
			Title: strp("session"), Mode: strp("agent"), Model: strp("grok-4.6"),
			Config:   &agent.ConfigState{Options: cfg},
			Commands: &agent.CommandsState{Commands: []agent.CommandInfo{{Name: "review"}}},
			Plugins:  &agent.PluginsState{Plugins: []agent.PluginCommand{{Qualified: "p:x"}}},
			SendNow:  &agent.SendNowState{Armed: true, Text: "now", Turn: "turn-1"},
		}, At: at(1)},
		{Type: agent.EventTurn, Turn: &agent.TurnInfo{ID: "turn-1", Phase: agent.TurnStarted, Text: "fix <the> tests", Origin: agent.TurnOriginSubmit}, At: at(2)},
		{Type: agent.EventCommand, Command: &agent.ExpandedCommand{PluginCommand: agent.PluginCommand{Qualified: "p:x", Kind: "skill"}}, At: at(2)},
		{Type: agent.EventThought, Text: "weighing ", At: at(3)},
		{Type: agent.EventThought, Text: "the options", At: at(4)},
		{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: "t1", Status: "pending", Title: "Shell", RawInput: "make test", At: at(5)}, At: at(5)},
		{Type: agent.EventText, Text: "Running the tests.", At: at(6)},
		{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: "t1", Status: "completed", Title: "Shell", RawInput: "make test", ContentText: "ok", At: at(7)}, At: at(7)},
		{Type: agent.EventTool, Tool: &agent.ToolEvent{Title: "an id-less tool"}, At: at(8)},
		{Type: agent.EventTodos, Todos: []agent.Todo{{ID: "1", Content: "one", Status: "pending"}, {ID: "2", Content: "two", Status: "pending"}}, At: at(9)},
		{Type: agent.EventSubagent, Subagent: &agent.SubagentInfo{ID: "sub-1", Status: agent.SubagentRunning, Prompt: "look", StartedAt: at(10)}, SubagentChange: agent.SubagentChangeSpawned, At: at(10)},
		{Type: agent.EventUser, Agent: "sub-1", Text: "look around", At: at(11)},
		{Type: agent.EventThought, Agent: "sub-1", Text: "reading", At: at(12)},
		{Type: agent.EventTool, Agent: "sub-1", Tool: &agent.ToolEvent{ID: "c1", Status: "pending", Title: "Read"}, At: at(13)},
		{Type: agent.EventText, Agent: "sub-1", Text: "found it", At: at(14)},
		{Type: agent.EventSubagent, Subagent: &agent.SubagentInfo{ID: "sub-1", Status: agent.SubagentCompleted, Output: "found it", EndedAt: at(15)}, SubagentChange: agent.SubagentChangeFinished, At: at(15)},
		{Type: agent.EventSubagent, Subagent: &agent.SubagentInfo{ID: "sub-2", Status: agent.SubagentRunning}, SubagentChange: agent.SubagentChangeSpawned, At: at(16)},
		{Type: agent.EventThought, Agent: "sub-2", Text: "still going", At: at(17)},
		{Type: agent.EventQueue, Queue: &agent.QueuedPrompt{ID: "q1", Text: "first", QueuedAt: at(18)}, QueueChange: agent.QueueQueued, At: at(18)},
		{Type: agent.EventQueue, Queue: &agent.QueuedPrompt{ID: "q2", Text: "second", QueuedAt: at(19)}, QueueChange: agent.QueueQueued, QueuePos: 1, At: at(19)},
		{Type: agent.EventQueue, Queue: &agent.QueuedPrompt{ID: "q1", Text: "first, edited", QueuedAt: at(18), Version: 1}, QueueChange: agent.QueueEdited, At: at(20)},
		{Type: agent.EventPermission, Permission: stubPermissionEvent(true), At: at(21)},
		{Type: agent.EventQuestion, Question: stubQuestion(), At: at(22)},
		{Type: agent.EventAsk, Ask: &agent.AskUpdate{ID: "ask-1", Kind: agent.AskQuestion, Outcome: agent.AskAnswered, By: agent.AskByClient}, At: at(23)},
		{Type: agent.EventPlan, Plan: stubPlanEvent(), At: at(24)},
		{Type: agent.EventError, Err: remoteErr("the agent stumbled"), At: at(25)},
		{Type: agent.EventUser, Text: "also the docs", Interjection: true, At: at(26)},
		{Type: agent.EventDone, StopReason: "cancelled", At: at(27)},
		{Type: agent.EventTurn, Turn: &agent.TurnInfo{ID: "turn-1", Phase: agent.TurnEnded, Synthetic: true, Err: "agent: prompt failed"}, At: at(28)},
		{Type: agent.EventForeignTurn, ForeignTurn: &agent.ForeignTurnInfo{ID: "f-1", Text: "on my own", Running: true}, At: at(29)},
		{Type: agent.EventText, Text: "continuing", At: at(30)},
		{Type: agent.EventForeignTurn, ForeignTurn: &agent.ForeignTurnInfo{ID: "f-1"}, At: at(31)},
		{Type: agent.EventReplay, Replay: &agent.ReplayInfo{Phase: agent.ReplayStart}, At: at(32)},
		{Type: agent.EventUser, Text: "an old prompt", Replayed: true, At: at(33)},
		{Type: agent.EventText, Text: "an old answer", At: at(34)},
		{Type: agent.EventReplay, Replay: &agent.ReplayInfo{Phase: agent.ReplayEnd}, At: at(35)},
		{Type: agent.EventTodos, Todos: []agent.Todo{{ID: "1", Content: "one", Status: "completed"}, {ID: "2", Content: "two", Status: "completed"}}, At: at(36)},
		{Type: agent.EventTurn, Turn: &agent.TurnInfo{ID: "turn-2", Phase: agent.TurnStarted, Text: "next", Origin: agent.TurnOriginDrain}, At: at(37)},
		{Type: agent.EventQueue, Queue: &agent.QueuedPrompt{ID: "q2", Text: "second"}, QueueChange: agent.QueueSent, At: at(37)},
		{Type: agent.EventThought, Text: "thinking again", At: at(38)},
	})
}

// laterScript is what the session does next, sequenced after n events: chunks
// into the open runs, tool updates old and new, a finish, notes, a delta.
func laterScript(n int) []agent.Event {
	evs := []agent.Event{
		{Type: agent.EventThought, Text: " and again", At: at(40)},
		{Type: agent.EventThought, Agent: "sub-2", Text: " still", At: at(41)},
		{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: "t1", Status: "failed", Title: "Shell", At: at(42)}, At: at(42)},
		{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: "t2", Status: "pending", Title: "Edit", At: at(43)}, At: at(43)},
		{Type: agent.EventTodos, Todos: []agent.Todo{{ID: "1", Content: "one", Status: "completed"}, {ID: "2", Content: "two", Status: "completed"}}, At: at(44)},
		{Type: agent.EventTool, Agent: "sub-2", Tool: &agent.ToolEvent{ID: "c9", Status: "pending"}, At: at(45)},
		{Type: agent.EventSubagent, Subagent: &agent.SubagentInfo{ID: "sub-2", Status: agent.SubagentCompleted, EndedAt: at(46)}, SubagentChange: agent.SubagentChangeFinished, At: at(46)},
		{Type: agent.EventAsk, Ask: &agent.AskUpdate{ID: "perm-1", Kind: agent.AskPermission, Outcome: agent.AskAnswered, By: agent.AskByClient}, At: at(47)},
		{Type: agent.EventMeta, State: &agent.StateDelta{Title: strp("renamed"), SendNow: &agent.SendNowState{}, Reason: agent.SendNowWithdrawn}, At: at(48)},
		{Type: agent.EventText, Text: "done", At: at(49)},
		{Type: agent.EventDone, StopReason: "end_turn", At: at(50)},
		{Type: agent.EventTurn, Turn: &agent.TurnInfo{ID: "turn-2", Phase: agent.TurnEnded}, At: at(50)},
	}
	for i := range evs {
		evs[i].Seq = uint64(n + 1 + i)
	}
	return evs
}

// modelView is everything two models must agree on: both projections and the
// last-ended ask list.
type modelView struct {
	H     History
	S     State
	Ended []AskEnding
}

func viewOf(m *Model) modelView { return modelView{m.History(), m.State(), m.EndedAsks()} }

// assertSameModel fails unless a and b agree on every projection.
func assertSameModel(t *testing.T, what string, want, got *Model) {
	t.Helper()
	w, g := viewOf(want), viewOf(got)
	if !reflect.DeepEqual(w.H, g.H) {
		t.Fatalf("%s: the histories differ:\nwant %+v\n got %+v", what, w.H, g.H)
	}
	if !reflect.DeepEqual(w.S, g.S) {
		t.Fatalf("%s: the states differ:\nwant %+v\n got %+v", what, w.S, g.S)
	}
	if !reflect.DeepEqual(w.Ended, g.Ended) {
		t.Fatalf("%s: the last-ended lists differ:\nwant %+v\n got %+v", what, w.Ended, g.Ended)
	}
}

// snapshotOf is m's snapshot at budget, its encoding checked against the size
// the window counted and against the budget.
func snapshotOf(t testing.TB, m *Model, budget int) (*Snapshot, []byte) {
	t.Helper()
	s, size, err := m.snapshotSized(budget)
	if err != nil {
		t.Fatalf("Snapshot(%d): %v", budget, err)
	}
	b, err := EncodeSnapshot(s)
	if err != nil {
		t.Fatalf("EncodeSnapshot: %v", err)
	}
	if len(b) != size {
		t.Fatalf("the window counted %d bytes, the encoding has %d", size, len(b))
	}
	if eff := max(budget, 0); eff == 0 && len(b) > DefaultSnapshotBytes || eff > 0 && len(b) > eff {
		t.Fatalf("the encoding has %d bytes, over the budget %d", len(b), budget)
	}
	return s, b
}

// restoredBoth is s restored in process and through the codec.
func restoredBoth(t testing.TB, s *Snapshot, b []byte, o Options) (*Model, *Model) {
	t.Helper()
	d, err := DecodeSnapshot(b)
	if err != nil {
		t.Fatalf("DecodeSnapshot: %v", err)
	}
	r1, r2 := Restore(s, o), Restore(d, o)
	checkInvariants(t, r1)
	checkInvariants(t, r2)
	return r1, r2
}

// TestRestoreIsTheModelAtEveryCut is A2's property at the package level (its
// engine-level form is C3's): at every Seq of a session with something in
// every part of the model, a snapshot restored — in process and through the
// codec — equals the model on both projections and the last-ended list, and
// the three, folding the rest of the session, stay equal after every event.
func TestRestoreIsTheModelAtEveryCut(t *testing.T) {
	evs := sessionScript()
	evs = append(evs, laterScript(len(evs))...)
	for cutAt := 0; cutAt <= len(evs); cutAt++ {
		m := New(Options{})
		foldAll(t, m, false, evs[:cutAt]...)
		s, b := snapshotOf(t, m, 0)
		if s.Seq != uint64(cutAt) || s.Main.Windowed {
			t.Fatalf("cut %d: snapshot at %d, windowed %v", cutAt, s.Seq, s.Main.Windowed)
		}
		r1, r2 := restoredBoth(t, s, b, Options{})
		assertSameModel(t, fmt.Sprintf("restored in process at %d", cutAt), m, r1)
		assertSameModel(t, fmt.Sprintf("restored through the codec at %d", cutAt), m, r2)
		for _, ev := range evs[cutAt:] {
			m.Fold(ev)
			r1.Fold(ev)
			r2.Fold(ev)
			assertSameModel(t, fmt.Sprintf("cut %d, in process, after %d", cutAt, ev.Seq), m, r1)
			assertSameModel(t, fmt.Sprintf("cut %d, through the codec, after %d", cutAt, ev.Seq), m, r2)
		}
		checkInvariants(t, r1)
		checkInvariants(t, r2)
	}
}

// continueBoth folds evs into the first model and its restored twins, and
// fails at the first event after which they differ.
func continueBoth(t *testing.T, what string, m *Model, rs []*Model, evs ...agent.Event) {
	t.Helper()
	for _, ev := range evs {
		m.Fold(ev)
		for i, r := range rs {
			r.Fold(ev)
			assertSameModel(t, fmt.Sprintf("%s (twin %d) after seq %d", what, i, ev.Seq), m, r)
		}
	}
	for _, r := range rs {
		checkInvariants(t, r)
	}
}

// restoreBoth snapshots m unwindowed and restores it both ways.
func restoreBoth(t *testing.T, m *Model, o Options) []*Model {
	t.Helper()
	s, b := snapshotOf(t, m, 1<<40)
	r1, r2 := restoredBoth(t, s, b, o)
	assertSameModel(t, "restored in process", m, r1)
	assertSameModel(t, "restored through the codec", m, r2)
	return []*Model{r1, r2}
}

// TestRestoreContinuesEveryContinuationState (plan 024 §3.5): what decides the
// next event's effect survives a snapshot — an open run of each stream kind
// grows from its tail exactly as the first model's does, at and across the
// stream cap (so the closed text equals the first model's capEntryText of the
// whole run); a repeated todo list draws no second note; an update to a tool
// the model trimmed appends a row on both; later unsequenced entries take the
// same local ids (X2); the roster evicts the same row by finish order; the
// last-ended list evicts the same ending; a replay bracket ends the same way.
func TestRestoreContinuesEveryContinuationState(t *testing.T) {
	t.Run("an open run at and across the stream cap", func(t *testing.T) {
		chunks := map[string]string{"ascii": "abcdefgh", "two-byte": "é", "four-byte": "😀", "mixed": "a😀é⤷b"}
		for _, limit := range []int{16, DefaultBounds().StreamText} {
			for name, unit := range chunks {
				for _, kind := range []string{"thought", "assistant", "child user"} {
					for _, over := range []int{-3, -2, -1, 0, 1, 2, 3, limit - 1, limit, limit + 1, 2 * limit} {
						o := Options{Bounds: Bounds{StreamText: limit, MainBytes: 1 << 30, SubBytes: 1 << 30}}
						m := New(o)
						ev := func(text string) agent.Event {
							switch kind {
							case "thought":
								return agent.Event{Type: agent.EventThought, Text: text, At: at(1)}
							case "assistant":
								return agent.Event{Type: agent.EventText, Text: text, At: at(1)}
							}
							return agent.Event{Type: agent.EventUser, Agent: "sub", Text: text, At: at(1)}
						}
						// A run of about limit+over bytes, in whole units — every chunk
						// an event carries is valid UTF-8 — sent as two chunks split at
						// a rune boundary.
						var run strings.Builder
						for run.Len()+len(unit) <= limit+over {
							run.WriteString(unit)
						}
						for i := 0; run.Len() < limit+over; i++ {
							run.WriteByte("xyz"[i%3])
						}
						whole := run.String()
						half := len(whole) / 2
						for half > 0 && !utf8.RuneStart(whole[half]) {
							half--
						}
						seq := uint64(0)
						next := func(e agent.Event) agent.Event { seq++; e.Seq = seq; return e }
						foldAll(t, m, true, next(ev(whole[:half])), next(ev(whole[half:])))
						if got := len(whole); got != limit+over {
							t.Fatalf("the run is %d bytes, want %d", got, limit+over)
						}
						rs := restoreBoth(t, m, o)
						what := fmt.Sprintf("cap %d, %s, %s, cap%+d", limit, name, kind, over)
						// The next chunks: a byte, a rune, a unit, and a whole cap.
						more := []string{"x", "é", unit, strings.Repeat("z", limit)}
						for _, c := range more {
							continueBoth(t, what, m, rs, next(ev(c)))
						}
						// Closing the run fixes its text: a note after it. On the
						// restored models it is exactly capEntryText of the whole
						// run, which only the first model ever held.
						continueBoth(t, what+", closed", m, rs, next(agent.Event{Type: agent.EventCommand, Agent: map[bool]string{true: "sub"}[kind == "child user"],
							Command: &agent.ExpandedCommand{PluginCommand: agent.PluginCommand{Qualified: "p:x"}}, At: at(2)}))
						tr := rs[1].Main
						if kind == "child user" {
							tr = rs[1].Sub("sub")
						}
						if got, want := tr.live()[0].Text, capText(whole+strings.Join(more, ""), limit); got != want {
							t.Fatalf("%s: the restored run closed as %q…, capEntryText of the whole run is %q…", what, got[:min(len(got), 20)], want[:min(len(want), 20)])
						}
					}
				}
			}
		}
	})

	t.Run("the todo-note dedupe", func(t *testing.T) {
		m := New(Options{})
		list := []agent.Todo{{ID: "1", Status: "pending"}, {ID: "2", Status: "completed"}}
		foldAll(t, m, true, sequenced([]agent.Event{{Type: agent.EventTodos, Todos: list, At: at(1)}})...)
		rs := restoreBoth(t, m, Options{})
		done := []agent.Todo{{ID: "1", Status: "completed"}, {ID: "2", Status: "completed"}}
		more := append(list, agent.Todo{ID: "3", Status: "pending"})
		evs := []agent.Event{
			{Type: agent.EventTodos, Todos: list, At: at(2)},
			{Type: agent.EventTodos, Todos: done, At: at(3)},
			{Type: agent.EventTodos, Todos: done, At: at(4)},
			{Type: agent.EventTodos, Todos: more, At: at(5)},
		}
		for i := range evs {
			evs[i].Seq = uint64(2 + i)
		}
		continueBoth(t, "todos", m, rs, evs...)
		if got := factTexts(rs[0].Main, "note"); len(got) != 3 {
			t.Fatalf("a repeated list drew a second note: %q", got)
		}
		rs = restoreBoth(t, m, Options{})
		continueBoth(t, "todos, done then repeated", m, rs, agent.Event{Type: agent.EventTodos, Todos: done, At: at(6), Seq: 6},
			agent.Event{Type: agent.EventTodos, Todos: done, At: at(7), Seq: 7})
	})

	t.Run("an update to a tool the model trimmed appends on both", func(t *testing.T) {
		o := Options{Bounds: Bounds{MainEntries: 3}}
		m := New(o)
		foldAll(t, m, true, sequenced([]agent.Event{
			{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: "old", Status: "pending"}, At: at(1)},
			note(1), note(2), note(3),
		})...)
		if _, ok := toolIndexed(m.Main, "old"); ok || !trimmed(m.Main) {
			t.Fatal("the fixture no longer trims the tool")
		}
		rs := restoreBoth(t, m, o)
		continueBoth(t, "trimmed", m, rs, agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: "old", Status: "completed"}, At: at(5), Seq: 5})
		if got := factsOf(rs[0].Main, "tool"); len(got) != 1 || got[0].ToolID != "old" {
			t.Fatalf("the update to a trimmed tool appends a row: %v", facts(rs[0].Main))
		}
	})

	t.Run("an update to a tool the window dropped applies to nothing", func(t *testing.T) {
		m := New(Options{})
		foldAll(t, m, true, sequenced([]agent.Event{
			{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: "early", Status: "pending", RawInput: strings.Repeat("r", 500)}, At: at(1)},
			note(1), note(2),
		})...)
		// The budget of the snapshot without the tool's row.
		full, _ := snapshotOf(t, m, 1<<40)
		s, b := snapshotOf(t, m, encodedLen(t, windowedTo(full, 2)))
		if len(s.Main.Entries) != 2 || s.Main.OmittedTools["early"] != (EntryID{Seq: 1}) {
			t.Fatalf("the window: %d entries, omitted %v", len(s.Main.Entries), s.Main.OmittedTools)
		}
		r1, r2 := restoredBoth(t, s, b, Options{})
		update := agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: "early", Status: "completed"}, At: at(5), Seq: 4}
		for i, r := range []*Model{r1, r2} {
			before := viewOf(r)
			r.Fold(update)
			after := viewOf(r)
			after.S.Seq = before.S.Seq
			if !reflect.DeepEqual(before, after) {
				t.Fatalf("twin %d: an update to an omitted tool changed the restored model", i)
			}
		}
		m.Fold(update)
		if got := factsOf(m.Main, "tool"); len(got) != 1 || m.Main.live()[0].Tool.Status != "completed" {
			t.Fatalf("the first model updates its own row: %v", facts(m.Main))
		}
	})

	t.Run("unsequenced entries take the same local ids", func(t *testing.T) {
		m := New(Options{})
		foldAll(t, m, true, note(1), note(2), agent.Event{Type: agent.EventMeta, State: &agent.StateDelta{Detail: "seq 5"}, Seq: 5}, note(3))
		if m.local != 3 {
			t.Fatalf("the fixture hands out %d local ids", m.local)
		}
		rs := restoreBoth(t, m, Options{})
		continueBoth(t, "local ids", m, rs, note(4), agent.Event{Type: agent.EventText, Text: "x", Seq: 5}, note(5))
		if got := rs[0].Main.lastEntry().ID; got != (EntryID{N: 6}) {
			t.Fatalf("the restored model named its entry %v", got)
		}
	})

	t.Run("the roster evicts the same row", func(t *testing.T) {
		o := Options{Bounds: Bounds{Agents: 2}}
		m := New(o)
		var evs []agent.Event
		for i := range 3 {
			id := fmt.Sprintf("sub-%d", i)
			evs = append(evs,
				agent.Event{Type: agent.EventSubagent, Subagent: &agent.SubagentInfo{ID: id, Status: agent.SubagentRunning}, SubagentChange: agent.SubagentChangeSpawned, At: at(i)},
				agent.Event{Type: agent.EventText, Agent: id, Text: "hi", At: at(i)},
			)
		}
		// Two finish at the same instant: only the finish order tells them apart.
		for _, i := range []int{1, 0} {
			evs = append(evs, agent.Event{Type: agent.EventSubagent, Subagent: &agent.SubagentInfo{ID: fmt.Sprintf("sub-%d", i), Status: agent.SubagentCompleted, EndedAt: at(9)}, SubagentChange: agent.SubagentChangeFinished, At: at(9)})
		}
		evs = sequenced(evs)
		foldAll(t, m, true, evs...)
		rs := restoreBoth(t, m, o)
		continueBoth(t, "roster", m, rs, agent.Event{Type: agent.EventSubagent, Subagent: &agent.SubagentInfo{ID: "sub-2", Status: agent.SubagentFailed, EndedAt: at(9)},
			SubagentChange: agent.SubagentChangeFinished, At: at(10), Seq: uint64(len(evs) + 1)})
		if got := rs[0].Subs(); !reflect.DeepEqual(got, []string{"sub-0", "sub-2"}) {
			t.Fatalf("the restored roster evicted by finish order to %v", got)
		}
	})

	t.Run("the last-ended list evicts the same ending", func(t *testing.T) {
		m := New(Options{})
		var evs []agent.Event
		for i := range maxEnded {
			evs = append(evs, agent.Event{Type: agent.EventAsk, Ask: &agent.AskUpdate{ID: fmt.Sprintf("ask-%d", i), Outcome: agent.AskAnswered}, At: at(i)})
		}
		foldAll(t, m, false, sequenced(evs)...)
		rs := restoreBoth(t, m, Options{})
		continueBoth(t, "ended", m, rs,
			agent.Event{Type: agent.EventAsk, Ask: &agent.AskUpdate{ID: "ask-7", Outcome: agent.AskCancelled}, At: at(300), Seq: 300},
			agent.Event{Type: agent.EventAsk, Ask: &agent.AskUpdate{ID: "ask-new", Outcome: agent.AskAnswered}, At: at(301), Seq: 301})
	})

	t.Run("a replay bracket open at the cut", func(t *testing.T) {
		m := New(Options{})
		foldAll(t, m, true, sequenced([]agent.Event{
			replayEvent(agent.ReplayStart),
			{Type: agent.EventUser, Text: "old", At: at(1)},
			{Type: agent.EventThought, Text: "old thought", At: at(2)},
		})...)
		rs := restoreBoth(t, m, Options{})
		continueBoth(t, "replay", m, rs,
			agent.Event{Type: agent.EventUser, Text: "older", At: at(3), Seq: 4},
			agent.Event{Type: agent.EventReplay, Replay: &agent.ReplayInfo{Phase: agent.ReplayEnd}, At: at(4), Seq: 5})
	})

	t.Run("a truncated row or ask is replaced whole", func(t *testing.T) {
		m := New(Options{})
		big := strings.Repeat("o", ItemCap+1)
		foldAll(t, m, true, sequenced([]agent.Event{
			{Type: agent.EventSubagent, Subagent: &agent.SubagentInfo{ID: "sub", Status: agent.SubagentRunning, Output: big}, SubagentChange: agent.SubagentChangeSpawned},
			{Type: agent.EventPermission, Permission: &agent.PermissionEvent{ID: "perm", Tool: big}},
		})...)
		s, b := snapshotOf(t, m, 0)
		for _, r := range func() []*Model { a, c := restoredBoth(t, s, b, Options{}); return []*Model{a, c} }() {
			st := r.State()
			if !st.TruncatedAgents["sub"] || len(st.Agents[0].Output) != ItemCap || !st.Asks[0].Truncated || len(st.Asks[0].Body.Permission.Tool) != ItemCap {
				t.Fatalf("the restored model does not say what was truncated: %+v", st)
			}
			r.Fold(agent.Event{Type: agent.EventSubagent, Subagent: &agent.SubagentInfo{ID: "sub", Status: agent.SubagentRunning, Output: "short"}, SubagentChange: agent.SubagentChangeProgress, Seq: 3})
			r.Fold(agent.Event{Type: agent.EventPermission, Permission: &agent.PermissionEvent{ID: "perm", Tool: "short"}, Seq: 4})
			if st := r.State(); st.TruncatedAgents != nil || st.Asks[0].Truncated {
				t.Fatalf("a whole row and a new opening are not truncated: %+v", st)
			}
		}
	})
}

// windowOf is an independent statement of the window rule, for the tests to
// hold Snapshot to: full's transcript (from an unwindowed snapshot) with only
// its newest k entries — Windowed when any is dropped, Dropped counting them,
// OmittedTools naming the dropped tool rows (the oldest row of an id first),
// OmittedRun the open run's kind when its entry is dropped, TailCut only while
// that entry is in.
func windowOf(full TranscriptSnap, k int) TranscriptSnap {
	n := len(full.Entries)
	d := n - k
	w := full
	w.Entries = nil
	if k > 0 {
		w.Entries = full.Entries[d:]
	}
	w.Windowed = full.Windowed || d > 0
	w.Dropped = full.Dropped + d
	openLast := full.StreamOpen && n > 0 && full.Entries[n-1].Streaming
	w.TailCut = full.TailCut && openLast && k > 0
	if w.OmittedRun == 0 && openLast && k == 0 {
		w.OmittedRun = full.Entries[n-1].Kind
	}
	w.OmittedTools = nil
	add := func(tid string, id EntryID) {
		if w.OmittedTools == nil {
			w.OmittedTools = make(map[string]EntryID)
		}
		if _, ok := w.OmittedTools[tid]; !ok {
			w.OmittedTools[tid] = id
		}
	}
	for tid, id := range full.OmittedTools {
		add(tid, id)
	}
	for i := range d {
		if tid := entryToolID(&full.Entries[i]); tid != "" {
			add(tid, full.Entries[i].ID)
		}
	}
	return w
}

// transcriptsOf is a snapshot's transcripts in filling order.
func transcriptsOf(s *Snapshot) []*TranscriptSnap {
	out := []*TranscriptSnap{&s.Main}
	for i := range s.Subs {
		out = append(out, &s.Subs[i].TranscriptSnap)
	}
	return out
}

// assertTheWindowIsTight holds a snapshot at budget to the window rule
// against the model's full snapshot: each transcript is windowOf(full, k) for
// its k, and each windowed one is maximal — its next entry would take the
// encoding over the budget.
func assertTheWindowIsTight(t *testing.T, what string, full, s *Snapshot, budget int) {
	t.Helper()
	fulls, got := transcriptsOf(full), transcriptsOf(s)
	if len(fulls) != len(got) {
		t.Fatalf("%s: %d transcripts, the model has %d", what, len(got), len(fulls))
	}
	for i := range got {
		n, k := len(fulls[i].Entries), len(got[i].Entries)
		if want := windowOf(*fulls[i], k); !reflect.DeepEqual(*got[i], want) {
			t.Fatalf("%s: transcript %d with %d of %d entries is\n%+v\nwant\n%+v", what, i, k, n, *got[i], want)
		}
		if k == n {
			continue
		}
		grown := *s
		grown.Subs = append([]SubSnap(nil), s.Subs...)
		*transcriptsOf(&grown)[i] = windowOf(*fulls[i], k+1)
		b, err := EncodeSnapshot(&grown)
		if err != nil {
			t.Fatal(err)
		}
		if len(b) <= budget {
			t.Fatalf("%s: transcript %d stopped at %d of %d entries, but one more encodes to %d, within %d", what, i, k, n, len(b), budget)
		}
	}
}

// TestTheWindowCountsTheEncodingExactly: over sessions that trim, evict, open
// runs and carry ids that need escaping, and over every budget from nothing
// to the whole, a snapshot either is refused with ErrSnapshotTooLarge or
// encodes to exactly the size its window counted, within the budget, with
// every transcript the window rule's suffix and every windowed one unable to
// take its next entry; and a larger budget never keeps fewer entries.
func TestTheWindowCountsTheEncodingExactly(t *testing.T) {
	odd := sequenced([]agent.Event{
		{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: "t\"<é>\x01\\", Title: "odd"}, At: at(1)},
		{Type: agent.EventSubagent, Subagent: &agent.SubagentInfo{ID: "sub \"<&>\" é ", Status: agent.SubagentRunning}, SubagentChange: agent.SubagentChangeSpawned, At: at(2)},
		{Type: agent.EventTool, Agent: "sub \"<&>\" é ", Tool: &agent.ToolEvent{ID: "c\xff", Title: "bad bytes"}, At: at(3)},
		{Type: agent.EventThought, Agent: "sub \"<&>\" é ", Text: strings.Repeat("th", 300), At: at(4)},
		{Type: agent.EventText, Text: strings.Repeat("reply ", 100), At: at(5)},
	})
	scripts := map[string]struct {
		evs []agent.Event
		o   Options
	}{
		"session":       {append(sessionScript(), laterScript(len(sessionScript()))...), Options{}},
		"race":          {raceScript(700), Options{Bounds: raceBounds}},
		"escaped ids":   {odd, Options{}},
		"session, open": {sessionScript(), Options{}},
	}
	for name, sc := range scripts {
		m := New(sc.o)
		foldAll(t, m, false, sc.evs...)
		full, fb := snapshotOf(t, m, 1<<40)
		prev := -1
		refused := 0
		for budget := 1; budget <= len(fb)+64; budget += max(1, len(fb)/200) {
			s, size, err := m.snapshotSized(budget)
			if errors.Is(err, ErrSnapshotTooLarge) {
				refused++
				if prev >= 0 {
					t.Fatalf("%s: a budget of %d is refused where a smaller one was not", name, budget)
				}
				continue
			}
			if err != nil {
				t.Fatalf("%s: Snapshot(%d): %v", name, budget, err)
			}
			b, err := EncodeSnapshot(s)
			if err != nil {
				t.Fatal(err)
			}
			if len(b) != size || size > budget {
				t.Fatalf("%s: budget %d: the window counted %d, the encoding has %d", name, budget, size, len(b))
			}
			assertTheWindowIsTight(t, fmt.Sprintf("%s at %d", name, budget), full, s, budget)
			// The main transcript is filled first, so a larger budget never
			// keeps fewer of its entries. The children, filled with what it
			// leaves, can: one more main entry can leave a child less room.
			kept := len(s.Main.Entries)
			if kept < prev {
				t.Fatalf("%s: a budget of %d keeps %d main entries, a smaller one kept %d", name, budget, kept, prev)
			}
			prev = kept
		}
		if refused == 0 || prev < 0 {
			t.Fatalf("%s: the sweep never crossed the refusal edge (%d refused, last kept %d)", name, refused, prev)
		}
	}
}

// TestWindowedRestoreContinuesTheSuffix is the package-level twin of A2's
// windowed case (C3's TestAWindowedSnapshotReproducesTheSuffix): a snapshot
// windowed to a budget restores to the first model's suffix — the entries it
// kept, equal, under the same ids — with Windowed set; folding on, the
// restored model's entries stay the first model's newest; an update to a tool
// whose row the window dropped leaves the restored model unchanged (the first
// model updates its row), while an update to a kept tool updates both; and a
// child the window emptied mid-run lets that run's chunks go by until
// something ends it, then draws what the first model draws.
func TestWindowedRestoreContinuesTheSuffix(t *testing.T) {
	var evs []agent.Event
	for i := range 6 {
		evs = append(evs,
			agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: fmt.Sprintf("t%d", i), Status: "pending", RawInput: strings.Repeat("r", 400)}, At: at(i)},
			agent.Event{Type: agent.EventText, Text: strings.Repeat("a", 300), At: at(i)},
		)
	}
	evs = append(evs,
		agent.Event{Type: agent.EventSubagent, Subagent: &agent.SubagentInfo{ID: "sub", Status: agent.SubagentRunning}, SubagentChange: agent.SubagentChangeSpawned, At: at(10)},
		agent.Event{Type: agent.EventTool, Agent: "sub", Tool: &agent.ToolEvent{ID: "c0", Status: "pending"}, At: at(11)},
		agent.Event{Type: agent.EventThought, Agent: "sub", Text: strings.Repeat("s", 2000), At: at(12)},
		agent.Event{Type: agent.EventThought, Text: "main thinks", At: at(13)},
	)
	evs = sequenced(evs)
	m := New(Options{})
	foldAll(t, m, true, evs...)
	full, fb := snapshotOf(t, m, 1<<40)
	// A budget that keeps the main transcript's newest four entries and none
	// of the child's.
	budget := len(fb)
	for {
		s, _, err := m.snapshotSized(budget)
		if err != nil {
			t.Fatal(err)
		}
		if len(s.Main.Entries) <= 4 {
			break
		}
		budget -= 50
	}
	s, b := snapshotOf(t, m, budget)
	assertTheWindowIsTight(t, "the fixture", full, s, budget)
	if !s.Main.Windowed || len(s.Subs[0].Entries) != 0 || s.Subs[0].OmittedRun != KindThought || len(s.Main.OmittedTools) == 0 {
		t.Fatalf("the fixture's window: main %d entries (windowed %v, omitted %v), child %d (run %v)",
			len(s.Main.Entries), s.Main.Windowed, s.Main.OmittedTools, len(s.Subs[0].Entries), s.Subs[0].OmittedRun)
	}
	omitted := ""
	for tid := range s.Main.OmittedTools {
		omitted = tid
	}
	r1, r2 := restoredBoth(t, s, b, Options{})

	// assertSuffix: the restored model's entries are the first model's newest,
	// equal and under the same ids; Windowed is set; the state agrees but for
	// the tools whose rows only the first model holds.
	assertSuffix := func(what string) {
		t.Helper()
		want := m.History()
		for i, r := range []*Model{r1, r2} {
			got := r.History()
			pairs := []struct{ w, g TranscriptHistory }{{want.Main, got.Main}}
			for j := range got.Subs {
				pairs = append(pairs, struct{ w, g TranscriptHistory }{want.Subs[j].TranscriptHistory, got.Subs[j].TranscriptHistory})
			}
			for j, p := range pairs {
				if len(p.g.Entries) > len(p.w.Entries) {
					t.Fatalf("%s (twin %d, transcript %d): %d entries, the first model has %d", what, i, j, len(p.g.Entries), len(p.w.Entries))
				}
				if tail := p.w.Entries[len(p.w.Entries)-len(p.g.Entries):]; len(p.g.Entries) > 0 && !reflect.DeepEqual(tail, p.g.Entries) {
					t.Fatalf("%s (twin %d, transcript %d): not the first model's suffix:\nwant %+v\n got %+v", what, i, j, tail, p.g.Entries)
				}
				if !p.g.Windowed || p.g.StreamOpen != p.w.StreamOpen || p.g.TodoPlanned != p.w.TodoPlanned {
					t.Fatalf("%s (twin %d, transcript %d): windowed %v, stream %v/%v", what, i, j, p.g.Windowed, p.g.StreamOpen, p.w.StreamOpen)
				}
			}
			ws, gs := m.State(), r.State()
			for k, tool := range gs.Tools {
				if !reflect.DeepEqual(ws.Tools[k], tool) {
					t.Fatalf("%s (twin %d): tool %v is %+v, the first model's %+v", what, i, k, tool, ws.Tools[k])
				}
			}
			ws.Tools, gs.Tools = nil, nil
			if !reflect.DeepEqual(ws, gs) {
				t.Fatalf("%s (twin %d): the states differ:\nwant %+v\n got %+v", what, i, ws, gs)
			}
			checkInvariants(t, r)
		}
	}
	assertSuffix("restored")

	seq := uint64(len(evs))
	fold := func(what string, ev agent.Event) {
		t.Helper()
		seq++
		ev.Seq = seq
		m.Fold(ev)
		r1.Fold(ev)
		r2.Fold(ev)
		assertSuffix(what)
	}
	fold("a chunk into the main run", agent.Event{Type: agent.EventThought, Text: " more", At: at(20)})
	fold("a chunk into the child's omitted run", agent.Event{Type: agent.EventThought, Agent: "sub", Text: " more", At: at(21)})
	if got := r1.Sub("sub").Len(); got != 0 {
		t.Fatalf("a chunk into an omitted run drew %d entries", got)
	}
	fold("a new tool closes the main run", agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: "new", Status: "pending"}, At: at(22)})
	before := []modelView{viewOf(r1), viewOf(r2)}
	fold("an update to an omitted tool", agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: omitted, Status: "completed"}, At: at(23)})
	for i, r := range []*Model{r1, r2} {
		after := viewOf(r)
		after.S.Seq = before[i].S.Seq
		if !reflect.DeepEqual(before[i], after) {
			t.Fatalf("an update to an omitted tool changed the restored model (twin %d)", i)
		}
	}
	if ts := m.State().Tools[ToolKey{ID: omitted}]; ts == nil || ts.Status != "completed" {
		t.Fatalf("the first model updates its own row of %q: %+v", omitted, ts)
	}
	fold("an update to a kept tool", agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: "new", Status: "completed"}, At: at(24)})
	fold("a tool ends the child's omitted run", agent.Event{Type: agent.EventTool, Agent: "sub", Tool: &agent.ToolEvent{ID: "c1"}, At: at(25)})
	fold("a new run in the child", agent.Event{Type: agent.EventText, Agent: "sub", Text: "after", At: at(26)})
	if got := r1.Sub("sub").Len(); got != 2 {
		t.Fatalf("the child's entries after its omitted run: %d", got)
	}
	fold("an update to the child's omitted tool", agent.Event{Type: agent.EventTool, Agent: "sub", Tool: &agent.ToolEvent{ID: "c0", Status: "done"}, At: at(27)})
}

// TestASnapshotsEncodingIsUnchangedByLaterFolds (A6, GLM 2): a snapshot's
// encoding is byte for byte the same after the model folds on — chunks into
// the open runs it holds tails of, updates to tools it holds rows of, trims of
// the entries it holds, evictions and state changes — because every entry it
// holds is immutable and every tail a copy. The folds run while another
// goroutine encodes the same snapshot, so -race sees any shared write.
func TestASnapshotsEncodingIsUnchangedByLaterFolds(t *testing.T) {
	o := Options{Bounds: Bounds{MainEntries: 12, SubEntries: 4, Agents: 1, StreamText: 64}}
	m := New(o)
	evs := sessionScript()
	foldAll(t, m, false, evs...)
	s, first := snapshotOf(t, m, 0)
	if !s.Main.StreamOpen || len(s.Subs) == 0 {
		t.Fatal("the fixture holds no open run or child")
	}
	later := laterScript(len(evs))
	for i := range 40 {
		later = append(later,
			agent.Event{Type: agent.EventThought, Text: strings.Repeat("u", 50), Seq: uint64(len(evs) + len(later) + 1)},
			agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: "t1", Status: fmt.Sprint(i)}, Seq: uint64(len(evs) + len(later) + 2)},
		)
	}
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			if b, err := EncodeSnapshot(s); err != nil || !bytes.Equal(b, first) {
				t.Errorf("the encoding changed while the model folded on (%v)", err)
				return
			}
		}
	})
	foldAll(t, m, true, later...)
	close(stop)
	wg.Wait()
	if !m.Main.trimmed {
		t.Fatal("the folds no longer trim the entries the snapshot holds")
	}
	again, err := EncodeSnapshot(s)
	if err != nil || !bytes.Equal(again, first) {
		t.Fatalf("the snapshot's encoding changed after the model folded on (%v)", err)
	}
}
