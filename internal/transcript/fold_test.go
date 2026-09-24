package transcript

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
)

// The §3.3 table, row by row: what each kind does to the main transcript and
// model, and to a child's transcript where the table says something different.

// TestFoldIsTotalOverNilPayloads: every kind, with every payload pointer nil,
// nil Todos, nil Err and no text, in the main session and a child's, is a
// no-op or a defined effect and never a panic — and draws no entry, which is
// exactly what the TUI draws for each of them. The one effect it may have is
// the TUI's: a done or a foreign-turn bracket closes the main session's open
// run whatever its payload says. An unknown kind is a no-op.
func TestFoldIsTotalOverNilPayloads(t *testing.T) {
	closes := map[agent.EventType]bool{agent.EventDone: true, agent.EventForeignTurn: true}
	all := append(tableKinds(), "a_kind_nobody_declared")
	for _, k := range all {
		for _, who := range []string{"", "sub-1"} {
			name := string(k) + "/main"
			if who != "" {
				name = string(k) + "/child"
			}
			t.Run(name, func(t *testing.T) {
				m := New(Options{})
				m.Fold(agent.Event{Type: agent.EventThought, Agent: who, Text: "open", At: at(1), Seq: 1})
				tr := m.ensureSub(who)
				before := m.History()

				m.Fold(agent.Event{Type: agent.EventType(k), Agent: who, Seq: 2})
				checkInvariants(t, m)

				if got := len(tr.live()); got != 1 {
					t.Fatalf("a nil-payload %s drew an entry: %v", k, facts(tr))
				}
				if n := len(m.Main.live()); who != "" && n != 0 {
					t.Fatalf("a child's nil-payload %s reached the main transcript: %v", k, facts(m.Main))
				}
				wantOpen := who != "" || !closes[agent.EventType(k)]
				if streamOpen(tr) != wantOpen {
					t.Fatalf("a nil-payload %s left the run open=%v, want %v", k, streamOpen(tr), wantOpen)
				}
				if wantOpen && !reflect.DeepEqual(before.Main, m.History().Main) {
					t.Fatalf("a nil-payload %s changed the main transcript", k)
				}
			})
		}
	}
}

func TestEntryIDsFollowTheEventThatCreatedThem(t *testing.T) {
	m := New(Options{})
	// Seq past the model's: {seq, 0..}, and the model's Seq moves.
	m.Fold(agent.Event{Type: agent.EventMeta, State: &agent.StateDelta{Detail: "a", IndexErr: "b"}, Seq: 5})
	es := m.Main.Entries()
	if len(es) != 2 || es[0].ID != (EntryID{5, 0}) || es[1].ID != (EntryID{5, 1}) {
		t.Fatalf("one event's entries are {5,0},{5,1}: %v %v", es[0].ID, es[1].ID)
	}
	if m.Seq() != 5 {
		t.Fatalf("the model's Seq is %d, want 5", m.Seq())
	}
	// Seq 0, and a Seq already reached: {0, n} from the model's own counter,
	// and the Seq stays.
	m.Fold(agent.Event{Type: agent.EventDone, StopReason: "cancelled"})
	m.Fold(agent.Event{Type: agent.EventDone, StopReason: "cancelled", Seq: 5})
	m.Fold(agent.Event{Type: agent.EventDone, StopReason: "cancelled", Seq: 3})
	es = m.Main.Entries()
	want := []EntryID{{5, 0}, {5, 1}, {0, 1}, {0, 2}, {0, 3}}
	var got []EntryID
	for _, e := range es {
		got = append(got, e.ID)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("ids %v, want %v", got, want)
	}
	if m.Seq() != 5 {
		t.Fatalf("an event at or behind the model's Seq moved it to %d", m.Seq())
	}
	m.Fold(agent.Event{Type: agent.EventDone, StopReason: "cancelled", Seq: 6})
	if id := m.Main.Entries()[5].ID; id != (EntryID{6, 0}) || m.Seq() != 6 {
		t.Fatalf("the next sequenced event names %v at Seq %d", id, m.Seq())
	}
	checkInvariants(t, m)
}

func TestTextAndThoughtStreamPerTranscript(t *testing.T) {
	m := New(Options{})
	foldAll(t, m, true,
		agent.Event{Type: agent.EventText, Text: "hel", At: at(1)},
		agent.Event{Type: agent.EventText, Agent: "sub", Text: "child ", At: at(2)},
		agent.Event{Type: agent.EventText, Text: "lo", At: at(3)},
		agent.Event{Type: agent.EventThought, Agent: "sub", Text: "thinks", At: at(4)},
		agent.Event{Type: agent.EventText, Text: "", At: at(5)},
	)
	main := facts(m.Main)
	if len(main) != 1 || main[0].Text != "hello" || main[0].Open || !main[0].End.Equal(at(3)) {
		t.Fatalf("the main reply is one entry, ending at its last chunk: %v", main)
	}
	if !streamOpen(m.Main) {
		t.Fatal("a child's chunks and an empty chunk must not close the main run")
	}
	sub := facts(m.Sub("sub"))
	if len(sub) != 2 || sub[0].Text != "child " || sub[1].Kind != "thought" || !sub[1].Open {
		t.Fatalf("the child streams on its own: %v", sub)
	}
}

func TestAToolRowDropsTheTodoWriterBeforeItClosesTheRun(t *testing.T) {
	m := New(Options{})
	foldAll(t, m, true,
		agent.Event{Type: agent.EventThought, Text: "planning", At: at(1)},
		agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: "todo", ToolName: "updateTodos", At: at(2)}},
		agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: "todo2", Title: "Update TODOs: x", At: at(2)}},
		agent.Event{Type: agent.EventThought, Text: " more", At: at(3)},
	)
	if got := facts(m.Main); len(got) != 1 || got[0].Text != "planning more" || !got[0].Open {
		t.Fatalf("cursor's todo writer fired mid-thought and must not end the run: %v", got)
	}
	if s := m.State(); s.Tools != nil {
		t.Fatalf("the todo writer is not a tool of the model's: %v", s.Tools)
	}
}

func TestToolsUpsertByIDAndAnIDlessToolAppends(t *testing.T) {
	m := New(Options{})
	first := &agent.ToolEvent{ID: "t1", Status: "pending", At: at(1)}
	second := &agent.ToolEvent{ID: "t1", Status: "completed", At: at(2)}
	foldAll(t, m, true,
		agent.Event{Type: agent.EventTool, Tool: first},
		agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{Title: "anon", At: at(3)}},
		agent.Event{Type: agent.EventTool, Tool: second},
		agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{Title: "anon", At: at(4)}},
		agent.Event{Type: agent.EventTool, Agent: "sub", Tool: &agent.ToolEvent{ID: "t1", Status: "child"}},
	)
	got := factsOf(m.Main, "tool")
	if len(got) != 3 || got[0].ToolID != "t1" || !got[0].At.Equal(at(1)) {
		t.Fatalf("t1 is one row keeping its first stamp; the id-less tools two more: %v", got)
	}
	if m.Main.live()[0].Tool != second {
		t.Fatal("the row must hold the update's payload, by pointer")
	}
	st := m.State()
	if len(st.Tools) != 2 || st.Tools[ToolKey{ID: "t1"}] != second || st.Tools[ToolKey{Agent: "sub", ID: "t1"}].Status != "child" {
		t.Fatalf("every tool's last state, by transcript and id: %v", st.Tools)
	}
}

func TestTodosAreStateAndTheirNotesTheMainTranscripts(t *testing.T) {
	m := New(Options{})
	list := []agent.Todo{{ID: "1", Status: "pending"}, {ID: "2", Status: "completed"}}
	foldAll(t, m, true,
		agent.Event{Type: agent.EventTodos, Agent: "sub", Todos: list, At: at(1)},
	)
	if st := m.State(); st.Todos != nil || len(m.Main.live()) != 0 || len(m.Sub("sub").live()) != 0 {
		t.Fatalf("a child's todos are ignored: %v %v", st.Todos, facts(m.Sub("sub")))
	}
	foldAll(t, m, true, agent.Event{Type: agent.EventTodos, Todos: list, At: at(2)})
	if st := m.State(); !slices.Equal(st.Todos, list) {
		t.Fatalf("todos %v", st.Todos)
	}
	if got := factTexts(m.Main, "note"); !slices.Equal(got, []string{"tasks: 2 planned"}) {
		t.Fatalf("notes %q", got)
	}
	foldAll(t, m, true, agent.Event{Type: agent.EventTodos, Todos: []agent.Todo{}, At: at(3)})
	if st := m.State(); st.Todos != nil {
		t.Fatalf("an empty list clears: %v", st.Todos)
	}
	if got := factTexts(m.Main, "note"); len(got) != 1 {
		t.Fatalf("an empty list writes no note: %q", got)
	}
}

func TestAskOpeningsCloseTheRunUnlessAuto(t *testing.T) {
	type tc struct {
		name   string
		ev     agent.Event
		closes bool
		entry  string // the entry kind it draws, "" none
	}
	for _, c := range []tc{
		{name: "permission", ev: agent.Event{Type: agent.EventPermission, Permission: &agent.PermissionEvent{ID: "perm-1"}}, closes: true},
		{name: "question", ev: agent.Event{Type: agent.EventQuestion, Question: &agent.QuestionEvent{ID: "ask-1"}}, closes: true},
		{name: "auto question", ev: agent.Event{Type: agent.EventQuestion, Question: &agent.QuestionEvent{ID: "ask-1", Auto: true}}},
		{name: "plan", ev: agent.Event{Type: agent.EventPlan, Plan: &agent.PlanEvent{ID: "plan-1", Plan: "do it"}}, closes: true, entry: "plan"},
		{name: "auto plan", ev: agent.Event{Type: agent.EventPlan, Plan: &agent.PlanEvent{ID: "plan-1", Auto: true}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := New(Options{})
			ev := c.ev
			ev.At = at(5)
			foldAll(t, m, true, agent.Event{Type: agent.EventThought, Text: "hm", At: at(1)}, ev)
			if streamOpen(m.Main) == c.closes {
				t.Fatalf("the run open=%v; an opening closes it=%v", streamOpen(m.Main), c.closes)
			}
			if f := factsOf(m.Main, "thought")[0]; c.closes && !f.End.Equal(at(5)) {
				t.Fatalf("the run ends at %v, want the opening's At", f.End)
			}
			es := facts(m.Main)
			if c.entry == "" && len(es) != 1 || c.entry != "" && (len(es) != 2 || es[1].Kind != c.entry) {
				t.Fatalf("entries %v, want the thought and %q", es, c.entry)
			}
			st := m.State()
			if len(st.Asks) != 1 || !st.Asks[0].At.Equal(at(5)) {
				t.Fatalf("the ask is open, at its opening's At: %+v", st.Asks)
			}
			if c.entry == "plan" && m.Main.live()[1].Plan != ev.Plan {
				t.Fatal("the plan entry holds the event's payload")
			}
			// A child's opening is ignored.
			ch := ev
			ch.Agent = "sub"
			foldAll(t, m, true, ch)
			if len(m.State().Asks) != 1 || len(m.Sub("sub").live()) != 0 {
				t.Fatal("a child's ask opening is ignored")
			}
		})
	}
}

func TestAnAsksEndingDrawsNoEntryAndIsRecorded(t *testing.T) {
	m := New(Options{})
	foldAll(t, m, true,
		agent.Event{Type: agent.EventQuestion, Question: &agent.QuestionEvent{ID: "ask-1"}, At: at(1)},
		agent.Event{Type: agent.EventPermission, Permission: &agent.PermissionEvent{ID: "perm-1"}, At: at(2)},
		agent.Event{Type: agent.EventAsk, Ask: &agent.AskUpdate{ID: "ask-1", Kind: agent.AskQuestion, Outcome: agent.AskAnswered, By: agent.AskByClient, Skip: true}, At: at(3)},
		agent.Event{Type: agent.EventAsk, Ask: &agent.AskUpdate{ID: "perm-9", Kind: agent.AskPermission, Outcome: agent.AskAutomatic, By: agent.AskByPolicy,
			Body: &agent.AskBody{Permission: &agent.PermissionEvent{ID: "perm-9"}}}, At: at(4)},
	)
	if n := len(m.Main.live()); n != 0 {
		t.Fatalf("an ending draws nothing — the notes are the answering client's: %v", facts(m.Main))
	}
	st := m.State()
	if len(st.Asks) != 1 || st.Asks[0].ID != "perm-1" {
		t.Fatalf("only perm-1 is still open: %+v", st.Asks)
	}
	ended := m.EndedAsks()
	if len(ended) != 2 || ended[0] != (AskEnding{ID: "ask-1", Kind: agent.AskQuestion, Outcome: agent.AskAnswered, By: agent.AskByClient, At: at(3)}) || ended[1].ID != "perm-9" {
		t.Fatalf("the last-ended list: %+v", ended)
	}
	// Bounded at 256, oldest out, keyed by id.
	for i := range maxEnded + 10 {
		m.Fold(agent.Event{Type: agent.EventAsk, Ask: &agent.AskUpdate{ID: fmt.Sprintf("x-%d", i), Kind: agent.AskQuestion}})
	}
	if got := m.EndedAsks(); len(got) != maxEnded || got[0].ID == "ask-1" {
		t.Fatalf("the list is bounded at %d, oldest out: %d, first %q", maxEnded, len(got), got[0].ID)
	}
}

// TestAsksAreKeyedAndStayCompact (r2 finding 5): the open asks and the
// last-ended list are id-keyed with O(1) upserts and removals under the
// boundary, and read exactly as C1's ordered slices did — an opening of a
// present id replaces it where it stands, an ending removes it, an ending
// replaces its own earlier ending in place, and the ended list keeps the
// newest 256 — over thousands of asks in a random interleaving, held against
// a reference that is those slices; the holes removals leave are squeezed out,
// so the storage stays within twice what is held.
func TestAsksAreKeyedAndStayCompact(t *testing.T) {
	m := New(Options{})
	var open []string
	var ended []string
	r := rand.New(rand.NewPCG(24, 5))
	for i := range 20000 {
		id := fmt.Sprintf("ask-%d", r.IntN(3000))
		if r.IntN(100) < 55 {
			m.Fold(agent.Event{Type: agent.EventQuestion, Question: &agent.QuestionEvent{ID: id, Auto: true}, At: at(i)})
			if !slices.Contains(open, id) {
				open = append(open, id)
			}
		} else {
			m.Fold(agent.Event{Type: agent.EventAsk, Ask: &agent.AskUpdate{ID: id, Kind: agent.AskQuestion}, At: at(i)})
			if j := slices.Index(open, id); j >= 0 {
				open = slices.Delete(open, j, j+1)
			}
			if !slices.Contains(ended, id) {
				if len(ended) >= maxEnded {
					ended = ended[1:]
				}
				ended = append(ended, id)
			}
		}
		if i%101 != 0 && i != 19999 {
			continue
		}
		var gotOpen, gotEnded []string
		for _, a := range m.State().Asks {
			gotOpen = append(gotOpen, a.ID)
		}
		for _, e := range m.EndedAsks() {
			gotEnded = append(gotEnded, e.ID)
		}
		if !slices.Equal(gotOpen, open) || !slices.Equal(gotEnded, ended) {
			t.Fatalf("op %d: open %d asks (want %d), ended %d (want %d), or out of order", i, len(gotOpen), len(open), len(gotEnded), len(ended))
		}
		for name, l := range map[string][2]int{"open": {len(m.asks.items), m.asks.len()}, "ended": {len(m.ended.items), m.ended.len()}} {
			if l[0] > 2*l[1]+32 {
				t.Fatalf("op %d: the %s list's storage is %d slots for %d asks: its holes are not squeezed out", i, name, l[0], l[1])
			}
		}
	}
	// An opening replaces its ask where it stands, and the list empties clean.
	a, b := at(1), at(2)
	c := New(Options{})
	foldAll(t, c, true,
		agent.Event{Type: agent.EventQuestion, Question: &agent.QuestionEvent{ID: "x"}, At: a},
		agent.Event{Type: agent.EventQuestion, Question: &agent.QuestionEvent{ID: "y"}, At: a},
		agent.Event{Type: agent.EventQuestion, Question: &agent.QuestionEvent{ID: "x"}, At: b},
	)
	if asks := c.State().Asks; len(asks) != 2 || asks[0].ID != "x" || !asks[0].At.Equal(b) || asks[1].ID != "y" {
		t.Fatalf("a second opening replaces the first where it stands: %+v", asks)
	}
	foldAll(t, c, true,
		agent.Event{Type: agent.EventAsk, Ask: &agent.AskUpdate{ID: "x"}},
		agent.Event{Type: agent.EventAsk, Ask: &agent.AskUpdate{ID: "y"}},
	)
	if c.State().Asks != nil || len(c.asks.items) != 0 {
		t.Fatalf("every ask ended: %+v, %d slots", c.State().Asks, len(c.asks.items))
	}
}

func TestDoneClosesTheRunAndACancelLeavesItsNote(t *testing.T) {
	m := New(Options{})
	foldAll(t, m, true,
		agent.Event{Type: agent.EventThought, Text: "hm", At: at(1)},
		agent.Event{Type: agent.EventDone, StopReason: "end_turn", At: at(2)},
		agent.Event{Type: agent.EventThought, Agent: "sub", Text: "child", At: at(3)},
		agent.Event{Type: agent.EventDone, Agent: "sub", StopReason: "cancelled", At: at(4)},
	)
	if streamOpen(m.Main) || factsOf(m.Main, "thought")[0].End != at(2) {
		t.Fatalf("done closes the main run at its At: %v", facts(m.Main))
	}
	if !streamOpen(m.Sub("sub")) || len(factsOf(m.Main, "note")) != 0 {
		t.Fatal("a child's done is ignored")
	}
}

func TestAnErrorEventHoldsItsValue(t *testing.T) {
	m := New(Options{})
	boom := errors.New("boom")
	foldAll(t, m, true,
		agent.Event{Type: agent.EventError, Err: boom, At: at(1)},
		agent.Event{Type: agent.EventError, At: at(2)},
		agent.Event{Type: agent.EventError, Agent: "sub", Err: boom, At: at(3)},
	)
	es := m.Main.live()
	if len(es) != 1 || es[0].Err != boom || es[0].Text != "" || es[0].Kind != KindError || es[0].Bytes != errValueBytes {
		t.Fatalf("one error entry holding the value, its text unread: %+v", es)
	}
	if len(m.Sub("sub").live()) != 0 {
		t.Fatal("a child's error is ignored")
	}
}

// panicError is an error whose Error() may not be called by the fold.
type panicError struct{}

func (panicError) Error() string { panic("the fold called Error()") }

// TestTheFoldNeverCallsError: with no Options.ErrText — the engine's
// configuration — an error that is not a RemoteError is held unread: its entry
// is kept, with no text, charged errValueBytes (TestAnErrorEventHoldsItsValue),
// and nothing reading the model calls it either.
func TestTheFoldNeverCallsError(t *testing.T) {
	m := New(Options{})
	foldAll(t, m, true, agent.Event{Type: agent.EventError, Err: panicError{}, At: at(1)})
	_ = m.State()
	_ = m.History()
	_ = m.cut()
	if got := m.Main.live()[0].Err; got != (panicError{}) {
		t.Fatalf("held %v", got)
	}
}

// TestARemoteErrorIsAccountedByItsText (r2 finding 1): the engine's instance
// is handed *agent.RemoteError values (agent.EventLog.Observe), whose text the
// fold reads as its Message: the entry carries it as Text and accounts its
// real length, so ten 1 MiB failures are trimmed to the 8 MiB budget like any
// other rows rather than retained at a fixed 256 bytes each.
func TestARemoteErrorIsAccountedByItsText(t *testing.T) {
	m := New(Options{})
	for i := range 10 {
		big := strings.Repeat(string(rune('a'+i)), 1<<20)
		foldAll(t, m, true, agent.Event{Type: agent.EventError, Err: &agent.RemoteError{Message: big, Class: agent.EventErrOther}, At: at(i), Seq: uint64(i + 1)})
	}
	es := m.Main.live()
	if len(es) != 8 || m.Main.bytes != 8<<20 || !trimmed(m.Main) {
		t.Fatalf("10 MiB of failures under an 8 MiB budget: %d entries, %d bytes, trimmed %v", len(es), m.Main.bytes, trimmed(m.Main))
	}
	first := es[0]
	if first.Text != strings.Repeat("c", 1<<20) || first.Bytes != 1<<20 || first.Kind != KindError {
		t.Fatalf("the oldest kept failure is the third, its text held and accounted: %q… (%d bytes)", first.Text[:1], first.Bytes)
	}
	if re, ok := first.Err.(*agent.RemoteError); !ok || re.Message != first.Text {
		t.Fatalf("the entry still holds the error value: %T", first.Err)
	}
}

// TestAnEmptyErrorDrawsNothingAndLeavesTheRunOpen (r2 finding 3): an error
// whose known text is empty draws no row and closes nothing — today's rule, so
// the thoughts around it stay one run — whether the fold knows the text from a
// RemoteError or from Options.ErrText; and it changes nothing a client would
// re-render.
func TestAnEmptyErrorDrawsNothingAndLeavesTheRunOpen(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts Options
		err  error
	}{
		{name: "an empty RemoteError", err: &agent.RemoteError{Class: agent.EventErrOther}},
		{name: "an empty error read by ErrText", opts: Options{ErrText: func(err error) string { return err.Error() }}, err: errors.New("")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := New(tc.opts)
			foldAll(t, m, true, agent.Event{Type: agent.EventThought, Text: "a", At: at(1), Seq: 1})
			c := m.Fold(agent.Event{Type: agent.EventError, Err: tc.err, At: at(2), Seq: 2})
			checkInvariants(t, m)
			foldAll(t, m, true, agent.Event{Type: agent.EventThought, Text: "b", At: at(3), Seq: 3})
			if got := facts(m.Main); len(got) != 1 || got[0].Kind != "thought" || got[0].Text != "ab" || !got[0].Open {
				t.Fatalf("thought a, an empty error, thought b is one open \"ab\" thought: %v", got)
			}
			if c != (Change{}) {
				t.Fatalf("the empty error changed %+v", c)
			}
		})
	}
}

// TestErrTextReadsAnErrorForAClientOutsideTheBoundary: Options.ErrText is how a
// client folding the publisher's own error values (the TUI's primary) gives
// them their text — called once per error that is not a RemoteError, its
// answer held as the entry's Text and accounted — and a RemoteError never
// reaches it.
func TestErrTextReadsAnErrorForAClientOutsideTheBoundary(t *testing.T) {
	calls := 0
	m := New(Options{ErrText: func(err error) string {
		calls++
		return "read: " + err.Error()
	}})
	boom := errors.New("boom")
	foldAll(t, m, true,
		agent.Event{Type: agent.EventError, Err: boom, At: at(1)},
		agent.Event{Type: agent.EventError, Err: &agent.RemoteError{Message: "remote"}, At: at(2)},
	)
	es := m.Main.live()
	if len(es) != 2 || es[0].Text != "read: boom" || es[0].Err != boom || es[0].Bytes != len("read: boom") {
		t.Fatalf("ErrText's answer is the entry's text, accounted, beside the held value: %+v", es[0])
	}
	if es[1].Text != "remote" || es[1].Bytes != len("remote") {
		t.Fatalf("a RemoteError's text is its Message: %+v", es[1])
	}
	if calls != 1 {
		t.Fatalf("ErrText was called %d times, want once: never for a RemoteError", calls)
	}
}

func TestADeltaReplacesItsSectionsAndDrawsItsReports(t *testing.T) {
	m := New(Options{})
	full := agent.StateDelta{
		Title: strp("t"), Mode: strp("agent"), Model: strp("m1"),
		Config:   &agent.ConfigState{Options: []agent.ConfigOption{{ID: "effort"}}},
		Commands: &agent.CommandsState{Commands: []agent.CommandInfo{{Name: "c"}}},
		Plugins:  &agent.PluginsState{Plugins: []agent.PluginCommand{{Qualified: "p:q"}}},
		SendNow:  &agent.SendNowState{Armed: true, Text: "now"},
	}
	foldAll(t, m, true, agent.Event{Type: agent.EventMeta, State: &full, At: at(1)})
	want := Settings{Title: "t", Mode: "agent", Model: "m1", Config: full.Config.Options,
		Commands: full.Commands.Commands, Plugins: full.Plugins.Plugins, SendNow: *full.SendNow}
	if got := m.State().Settings; !reflect.DeepEqual(got, want) {
		t.Fatalf("settings %+v, want %+v", got, want)
	}
	// nil is untouched; a non-nil empty value clears.
	foldAll(t, m, true, agent.Event{Type: agent.EventMeta, State: &agent.StateDelta{
		Model: strp(""), Config: &agent.ConfigState{}, SendNow: &agent.SendNowState{},
	}, At: at(2)})
	want.Model, want.Config, want.SendNow = "", nil, agent.SendNowState{}
	if got := m.State().Settings; !reflect.DeepEqual(got, want) {
		t.Fatalf("settings %+v, want %+v", got, want)
	}
	if n := len(m.Main.live()); n != 0 {
		t.Fatalf("a settings delta draws nothing: %v", facts(m.Main))
	}
	// Reason alone draws nothing; Detail and IndexErr draw a row each, in the
	// TUI's order; Event.Mode and Event.Text add nothing.
	foldAll(t, m, true,
		agent.Event{Type: agent.EventMeta, State: &agent.StateDelta{Reason: agent.SendNowOtherTurn}, At: at(3)},
		agent.Event{Type: agent.EventMeta, State: &agent.StateDelta{Detail: "cancel failed", IndexErr: "index gone"}, At: at(4)},
		agent.Event{Type: agent.EventMeta, Mode: "plan", Text: "a title", At: at(5)},
	)
	if got := factTexts(m.Main, "error"); !slices.Equal(got, []string{"cancel failed", "index gone"}) {
		t.Fatalf("report rows %q", got)
	}
	if got := m.State().Settings; !reflect.DeepEqual(got, want) {
		t.Fatalf("Event.Mode/Text changed the settings: %+v", got)
	}
	// A Mode section inside a replay bracket is a replacement like any other.
	foldAll(t, m, true,
		replayEvent(agent.ReplayStart),
		agent.Event{Type: agent.EventMeta, State: &agent.StateDelta{Mode: strp("plan")}, Replayed: true, At: at(6)},
	)
	if got := m.State(); got.Settings.Mode != "plan" || !got.Replaying {
		t.Fatalf("mode %q replaying %v", got.Settings.Mode, got.Replaying)
	}
	// A child's delta is ignored.
	foldAll(t, m, true, agent.Event{Type: agent.EventMeta, Agent: "sub", State: &agent.StateDelta{Title: strp("child"), Detail: "x"}})
	if m.State().Settings.Title != "t" || len(m.Sub("sub").live()) != 0 {
		t.Fatal("a child's delta is ignored")
	}
}

func TestUserEvents(t *testing.T) {
	shell := "<shell_context>\n<command exit=\"0\">ls</command>\n</shell_context>\n\n"
	cases := []struct {
		name  string
		evs   []agent.Event
		agent string
		want  []fact
	}{
		{name: "a live echo draws nothing", evs: []agent.Event{{Type: agent.EventUser, Text: "hi"}}},
		{name: "an interjection", evs: []agent.Event{{Type: agent.EventUser, Interjection: true, Text: shell + "also", At: at(1)}},
			want: []fact{{Kind: "user", Text: "also", Interject: true, At: at(1), End: at(1)}}},
		{name: "an empty interjection draws nothing", evs: []agent.Event{{Type: agent.EventUser, Interjection: true, Text: shell}}},
		{name: "a replayed prompt", evs: []agent.Event{{Type: agent.EventUser, Replayed: true, Text: shell + "old", At: at(1)}},
			want: []fact{{Kind: "user", Text: "old", At: at(1), End: at(1)}}},
		{name: "an empty replayed prompt still draws its row", evs: []agent.Event{{Type: agent.EventUser, Replayed: true}},
			want: []fact{{Kind: "user"}}},
		{name: "any prompt while replaying", evs: []agent.Event{replayEvent(agent.ReplayStart), {Type: agent.EventUser, Text: "old", At: at(1)}},
			want: []fact{{Kind: "user", Text: "old", At: at(1), End: at(1)}}},
		{name: "a child's user event streams", agent: "sub", evs: []agent.Event{
			{Type: agent.EventUser, Agent: "sub", Text: shell, At: at(1)},
			{Type: agent.EventUser, Agent: "sub", Text: "rest", At: at(2)},
		}, want: []fact{{Kind: "user", Text: shell + "rest", At: at(1), End: at(2)}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := New(Options{})
			foldAll(t, m, true, c.evs...)
			if got := facts(m.ensureSub(c.agent)); !slices.Equal(got, c.want) {
				t.Fatalf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestTheRosterIsUpsertedAndAFinishClosesTheChildsRun(t *testing.T) {
	m := New(Options{})
	foldAll(t, m, true,
		agent.Event{Type: agent.EventSubagent},
		agent.Event{Type: agent.EventSubagent, Subagent: &agent.SubagentInfo{}},
		agent.Event{Type: agent.EventSubagent, Subagent: &agent.SubagentInfo{ID: "a", Status: agent.SubagentRunning}, SubagentChange: agent.SubagentChangeSpawned},
		// Routed to the roster whatever Agent says.
		agent.Event{Type: agent.EventSubagent, Agent: "a", Subagent: &agent.SubagentInfo{ID: "b", Status: agent.SubagentRunning}, SubagentChange: agent.SubagentChangeSpawned},
		agent.Event{Type: agent.EventThought, Agent: "a", Text: "hm", At: at(1)},
		agent.Event{Type: agent.EventSubagent, Subagent: &agent.SubagentInfo{ID: "a", Status: agent.SubagentRunning, Activity: "x"}, SubagentChange: agent.SubagentChangeProgress},
	)
	st := m.State()
	if len(st.Agents) != 2 || st.Agents[0].ID != "a" || st.Agents[0].Activity != "x" || st.Agents[1].ID != "b" {
		t.Fatalf("roster %+v", st.Agents)
	}
	if !slices.Equal(m.Subs(), []string{"a", "b"}) || len(m.Main.live()) != 0 {
		t.Fatalf("each roster row has its transcript (the TUI's ensureSub); nothing reaches main: %v", m.Subs())
	}
	foldAll(t, m, true, agent.Event{Type: agent.EventSubagent, Subagent: &agent.SubagentInfo{ID: "a", Status: agent.SubagentCompleted}, SubagentChange: agent.SubagentChangeFinished, At: at(4)})
	if f := facts(m.Sub("a"))[0]; f.Open || streamOpen(m.Sub("a")) || !f.End.Equal(at(4)) {
		t.Fatalf("a finish closes the child's run at its At: %v", f)
	}
	// An unstamped finish closes the run at Options.Clock's fallback, like
	// every other event-driven close (r1: the TUI's applySubagentEvent
	// carried the same fix; clock_test.go's TestAChildFinishedIsStampedByIt-
	// sEvent holds the full matrix, including the no-clock case).
	c := New(Options{Clock: func() time.Time { return at(50) }})
	foldAll(t, c, true,
		agent.Event{Type: agent.EventThought, Agent: "a", Text: "hm", At: at(1)},
		agent.Event{Type: agent.EventSubagent, Subagent: &agent.SubagentInfo{ID: "a", Status: agent.SubagentCompleted}, SubagentChange: agent.SubagentChangeFinished},
	)
	if f := facts(c.Sub("a"))[0]; f.Open || !f.End.Equal(at(50)) {
		t.Fatalf("an unstamped finish falls back to the clock: %v", f)
	}
}

func TestTheQueueIsKeyedByID(t *testing.T) {
	m := New(Options{})
	q := func(id, text string, change agent.QueueChange, pos int) agent.Event {
		return agent.Event{Type: agent.EventQueue, Queue: &agent.QueuedPrompt{ID: id, Text: text}, QueueChange: change, QueuePos: pos}
	}
	ids := func() (out []string) {
		for _, p := range m.State().Queue {
			out = append(out, p.ID+"="+p.Text)
		}
		return out
	}
	foldAll(t, m, true, q("a", "1", agent.QueueQueued, 0), q("b", "2", agent.QueueQueued, 1), q("c", "3", agent.QueueQueued, 0), q("d", "4", agent.QueueQueued, 99))
	if got := ids(); !slices.Equal(got, []string{"c=3", "a=1", "b=2", "d=4"}) {
		t.Fatalf("inserted at QueuePos (clamped): %v", got)
	}
	foldAll(t, m, true, q("b", "2'", agent.QueueQueued, 0), q("a", "1'", agent.QueueEdited, 3), q("zz", "?", agent.QueueEdited, 0))
	if got := ids(); !slices.Equal(got, []string{"c=3", "a=1'", "b=2'", "d=4"}) {
		t.Fatalf("a present id is replaced where it stands: %v", got)
	}
	foldAll(t, m, true, q("c", "", agent.QueueSent, 0), q("d", "", agent.QueueRemoved, 0), q("zz", "", agent.QueueRemoved, 0), q("a", "", "bogus", 0))
	if got := ids(); !slices.Equal(got, []string{"a=1'", "b=2'"}) {
		t.Fatalf("sent and removed delete by id: %v", got)
	}
	if len(m.Main.live()) != 0 {
		t.Fatal("the queue draws nothing")
	}
}

func TestCommandLines(t *testing.T) {
	cases := []struct {
		name string
		cmd  *agent.ExpandedCommand
		want []string
	}{
		{name: "command", cmd: &agent.ExpandedCommand{PluginCommand: agent.PluginCommand{Qualified: "p:c", Kind: "command"}}, want: []string{"⤷ p:c (command)"}},
		{name: "no kind", cmd: &agent.ExpandedCommand{PluginCommand: agent.PluginCommand{Qualified: "p:c"}}, want: []string{"⤷ p:c"}},
		{name: "sanitised", cmd: &agent.ExpandedCommand{PluginCommand: agent.PluginCommand{Qualified: "\x1b[31mp:\nc\x1b[0m  x", Kind: "\tskill\x07"}}, want: []string{"⤷ p: c x (skill)"}},
		{name: "an empty name draws nothing", cmd: &agent.ExpandedCommand{PluginCommand: agent.PluginCommand{Qualified: "\x1b[0m \n", Kind: "command"}}},
	}
	for _, c := range cases {
		for _, who := range []string{"", "sub"} {
			t.Run(c.name+"/"+who, func(t *testing.T) {
				m := New(Options{})
				foldAll(t, m, true, agent.Event{Type: agent.EventCommand, Agent: who, Command: c.cmd})
				if got := factTexts(m.ensureSub(who), "note"); !slices.Equal(got, c.want) {
					t.Fatalf("lines %q, want %q", got, c.want)
				}
			})
		}
	}
}

func TestSanitizeLineFastPathMatchesTheFullOne(t *testing.T) {
	full := func(s string) string {
		if s == "" {
			return ""
		}
		return strings.Join(strings.Fields(dropControls(stripANSI(s))), " ")
	}
	for _, s := range []string{"a", "p:c", "a b c", "a  b", " a", "a ", "tab\tx", "é", "~!@#$%^&*()_+{}|:\"<>?`-=[]\\;',./", "\x7f"} {
		if got, want := sanitizeLine(s), full(s); got != want {
			t.Fatalf("sanitizeLine(%q) = %q, the full path %q", s, got, want)
		}
	}
}

func TestForeignTurnsAndReplayBrackets(t *testing.T) {
	m := New(Options{})
	ft := &agent.ForeignTurnInfo{ID: "ft", Running: true}
	foldAll(t, m, true,
		agent.Event{Type: agent.EventText, Text: "a", At: at(1)},
		agent.Event{Type: agent.EventForeignTurn, ForeignTurn: ft, At: at(2)},
		agent.Event{Type: agent.EventText, Text: "b", At: at(3)},
		agent.Event{Type: agent.EventForeignTurn, ForeignTurn: &agent.ForeignTurnInfo{ID: "ft"}, At: at(4)},
		agent.Event{Type: agent.EventText, Text: "c", At: at(5)},
	)
	if got := factTexts(m.Main, "assistant"); !slices.Equal(got, []string{"a", "b", "c"}) {
		t.Fatalf("each bracket ends the run: %q", got)
	}
	if got := factTexts(m.Main, "note"); !slices.Equal(got, []string{NoteForeignTurn}) {
		t.Fatalf("only the start is noted: %q", got)
	}
	if f := m.State().Turn.Foreign; f == nil || f.Running {
		t.Fatalf("the last bracket is the turn's Foreign: %+v", f)
	}

	r := New(Options{})
	foldAll(t, r, true,
		replayEvent(agent.ReplayStart),
		agent.Event{Type: agent.EventThought, Text: "old thought", Replayed: true, At: at(1)},
		agent.Event{Type: agent.EventReplay, Replay: &agent.ReplayInfo{Phase: "sideways"}},
	)
	if !r.State().Replaying || !streamOpen(r.Main) {
		t.Fatal("start opens the bracket; an unknown phase does nothing")
	}
	foldAll(t, r, true, agent.Event{Type: agent.EventReplay, Replay: &agent.ReplayInfo{Phase: agent.ReplayEnd}, At: at(2)})
	if r.State().Replaying || streamOpen(r.Main) {
		t.Fatal("the end closes the bracket and the run")
	}
	if got := facts(r.Main); len(got) != 2 || got[0].End != at(2) || got[1].Text != NoteRestored {
		t.Fatalf("the last replayed run closes before the restored note: %v", got)
	}
}

func TestTurns(t *testing.T) {
	m := New(Options{})
	foldAll(t, m, true,
		agent.Event{Type: agent.EventTurn, Turn: &agent.TurnInfo{ID: "turn-1", Phase: agent.TurnStarted, Text: "<shell_context>\n<command exit=\"0\">ls</command>\n</shell_context>\n\ngo", Origin: agent.TurnOriginDrain}, At: at(1)},
		agent.Event{Type: agent.EventTurn, Turn: &agent.TurnInfo{ID: "turn-2", Phase: agent.TurnStarted, Origin: agent.TurnOriginSubmit}, At: at(2)},
	)
	if got := facts(m.Main); len(got) != 2 || got[0].Text != "go" || got[1].Text != "" {
		t.Fatalf("a started is its user row, shell context stripped, drawn even empty: %v", got)
	}
	if tu := m.State().Turn; tu.ID != "turn-2" || tu.Origin != agent.TurnOriginSubmit || !tu.At.Equal(at(2)) {
		t.Fatalf("turn %+v", tu)
	}
	// A late ending of an earlier turn does not end the running one.
	foldAll(t, m, true, agent.Event{Type: agent.EventTurn, Turn: &agent.TurnInfo{ID: "turn-1", Phase: agent.TurnEnded}, At: at(3)})
	if m.State().Turn.ID != "turn-2" {
		t.Fatal("turn-1's ending ended turn-2")
	}
	foldAll(t, m, true,
		agent.Event{Type: agent.EventTurn, Turn: &agent.TurnInfo{ID: "turn-2", Phase: agent.TurnEnded, StopReason: "cancelled"}, At: at(4)},
		agent.Event{Type: agent.EventTurn, Turn: &agent.TurnInfo{ID: "turn-3", Phase: agent.TurnEnded, StopReason: "closing", Synthetic: true}, At: at(5)},
		agent.Event{Type: agent.EventTurn, Turn: &agent.TurnInfo{ID: "turn-4", Phase: agent.TurnEnded, StopReason: "cancelled", Synthetic: true}, At: at(6)},
		agent.Event{Type: agent.EventTurn, Turn: &agent.TurnInfo{ID: "turn-5", Phase: agent.TurnEnded, StopReason: "cancelled", Synthetic: true, Err: "in flight"}, At: at(7)},
		agent.Event{Type: agent.EventTurn, Turn: &agent.TurnInfo{ID: "turn-6", Phase: "neither"}, At: at(8)},
	)
	if m.State().Turn.ID != "" {
		t.Fatal("its own ending ends the turn")
	}
	got := facts(m.Main)[2:]
	want := []fact{{Kind: "note", Text: NoteCancelled, At: at(6), End: at(6)}, {Kind: "error", Text: "in flight", At: at(7), End: at(7)}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("a wire cancel and a closing draw nothing; a synthetic cancel its note, a refusal its error: %v", got)
	}
}

func TestChangeSaysWhatTheFoldTouched(t *testing.T) {
	m := New(Options{})
	c := m.Fold(agent.Event{Type: agent.EventThought, Text: "a", At: at(1), Seq: 1})
	if c.Scope != "" || c.AppendedFrom != (EntryID{1, 0}) || c.AppendedTo != (EntryID{1, 0}) || c.Touched != [2]EntryID{} || !c.Entries() || c.State {
		t.Fatalf("a first chunk appends one entry: %+v", c)
	}
	c = m.Fold(agent.Event{Type: agent.EventThought, Text: "b", At: at(2), Seq: 2})
	if c.Touched != [2]EntryID{{1, 0}} || !c.AppendedFrom.IsZero() {
		t.Fatalf("a chunk touches its entry: %+v", c)
	}
	c = m.Fold(agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: "t", At: at(3)}, Seq: 3})
	if c.Touched != [2]EntryID{{1, 0}} || c.AppendedFrom != (EntryID{3, 0}) || !c.State {
		t.Fatalf("a new tool closes the run and appends: %+v", c)
	}
	m.Fold(agent.Event{Type: agent.EventThought, Text: "c", At: at(4), Seq: 4})
	c = m.Fold(agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: "t", Status: "done", At: at(5)}, Seq: 5})
	if c.Touched != [2]EntryID{{4, 0}, {3, 0}} || !c.AppendedFrom.IsZero() {
		t.Fatalf("a tool update touches the run it closed and its row: %+v", c)
	}
	c = m.Fold(agent.Event{Type: agent.EventMeta, State: &agent.StateDelta{Detail: "x", IndexErr: "y"}, Seq: 6})
	if c.AppendedFrom != (EntryID{6, 0}) || c.AppendedTo != (EntryID{6, 1}) {
		t.Fatalf("two rows from one event: %+v", c)
	}
	c = m.Fold(agent.Event{Type: agent.EventText, Agent: "sub", Text: "x", Seq: 7})
	if c.Scope != "sub" || c.AppendedFrom != (EntryID{7, 0}) {
		t.Fatalf("a child's chunk is the child's scope: %+v", c)
	}
	c = m.Fold(agent.Event{Type: agent.EventQueue, Queue: &agent.QueuedPrompt{ID: "q"}, QueueChange: agent.QueueQueued, Seq: 8})
	if c.Scope != "" || c.Entries() || !c.State {
		t.Fatalf("a state-only fold: %+v", c)
	}
	c = m.Fold(agent.Event{Type: agent.EventText, Seq: 9})
	if c != (Change{}) {
		t.Fatalf("a no-op: %+v", c)
	}
	// Trimming counts what fell off the front.
	s := New(Options{Bounds: Bounds{MainEntries: 2}})
	s.Fold(agent.Event{Type: agent.EventDone, StopReason: "cancelled", Seq: 1})
	s.Fold(agent.Event{Type: agent.EventDone, StopReason: "cancelled", Seq: 2})
	c = s.Fold(agent.Event{Type: agent.EventMeta, State: &agent.StateDelta{Detail: "x", IndexErr: "y"}, Seq: 3})
	if c.Dropped != 2 || c.AppendedFrom != (EntryID{3, 0}) || c.AppendedTo != (EntryID{3, 1}) {
		t.Fatalf("two appended, two dropped: %+v", c)
	}
	c = s.Fold(agent.Event{Type: agent.EventMeta, State: &agent.StateDelta{Detail: "x", IndexErr: "y"}, Seq: 4})
	if c.Dropped != 2 {
		t.Fatalf("two dropped: %+v", c)
	}
	// An appended entry the same fold trimmed again is clipped out of the range.
	one := New(Options{Bounds: Bounds{MainEntries: 1}})
	c = one.Fold(agent.Event{Type: agent.EventMeta, State: &agent.StateDelta{Detail: "x", IndexErr: "y"}, Seq: 1})
	if c.AppendedFrom != (EntryID{1, 1}) || c.AppendedTo != (EntryID{1, 1}) || c.Dropped != 1 {
		t.Fatalf("the first row was trimmed by the second: %+v", c)
	}
	// A trim that drops a named tool takes its last state out of the state
	// projection, so the fold that trimmed says the state changed — a chunk
	// here, which is no state row at all (r2 finding 6). Dropping history does
	// not, and neither does dropping an id-less tool, which is history too.
	tl := New(Options{Bounds: Bounds{MainEntries: 1}})
	tl.Fold(agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: "t"}, Seq: 1})
	c = tl.Fold(agent.Event{Type: agent.EventText, Text: "x", Seq: 2})
	if !c.State || c.Dropped != 1 || tl.State().Tools != nil {
		t.Fatalf("a chunk that trimmed a named tool must report the state change: %+v (tools %v)", c, tl.State().Tools)
	}
	tl.Fold(agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{Title: "anonymous"}, Seq: 3})
	for i, ev := range []agent.Event{
		{Type: agent.EventText, Text: "y"}, // drops the id-less tool
		{Type: agent.EventCommand, Command: &agent.ExpandedCommand{PluginCommand: agent.PluginCommand{Qualified: "p:c"}}}, // drops the reply
	} {
		ev.Seq = uint64(4 + i)
		if c = tl.Fold(ev); c.State || c.Dropped != 1 {
			t.Fatalf("seq %d dropped history and reported a state change: %+v", ev.Seq, c)
		}
	}
	// The child's transcript trims the same way, and its tools are state too.
	ch := New(Options{Bounds: Bounds{SubEntries: 1}})
	ch.Fold(agent.Event{Type: agent.EventTool, Agent: "sub", Tool: &agent.ToolEvent{ID: "c"}, Seq: 1})
	if c = ch.Fold(agent.Event{Type: agent.EventText, Agent: "sub", Text: "x", Seq: 2}); !c.State || c.Scope != "sub" || c.Dropped != 1 {
		t.Fatalf("a child's chunk that trimmed its named tool: %+v", c)
	}
}
