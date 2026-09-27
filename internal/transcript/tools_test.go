package transcript

import (
	"reflect"
	"slices"
	"testing"

	"github.com/charliek/craze/internal/agent"
)

// Tools' order (plan 027 §3.13, "Ordered tools" — SF-43): every retained
// tool's last state, main's then each child's in creation order, each
// transcript's own tools in entry order. The negative control for every test
// below is the same mutation: build Tools() from State().Tools (the existing
// map) instead of walking each transcript's own entries. A map has no order,
// so TestToolsOrderSurvivesAnInterleavedUpdate goes red — not always on the
// first run, since a two-entry map can land right by chance, which is why it
// uses three tools and an update in the middle, cutting that chance to 1 in 6.

// TestToolsOrderSurvivesAnInterleavedUpdate: creation order is kept even when
// the middle tool is the one updated — an update replaces the row in place
// (Transcript.upsertTool), it does not move it to the end.
func TestToolsOrderSurvivesAnInterleavedUpdate(t *testing.T) {
	m := New(Options{})
	foldAll(t, m, true,
		agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: "a", Status: "pending"}},
		agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: "b", Status: "pending"}},
		agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: "c", Status: "pending"}},
		agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: "b", Status: "completed"}},
	)
	got := m.Tools()
	ids := make([]string, len(got))
	for i, tv := range got {
		ids[i] = tv.ID
	}
	if want := []string{"a", "b", "c"}; !slices.Equal(ids, want) {
		t.Fatalf("Tools() order is %v, want %v (creation order, b's update kept in place)", ids, want)
	}
	if got[1].Status != "completed" {
		t.Fatalf("b's row is %+v, want its updated state", got[1])
	}
}

// TestToolsMainThenChildrenInCreationOrder: main's own tools come first, in
// their own entry order, then each child's, the children themselves in the
// order they were first created.
func TestToolsMainThenChildrenInCreationOrder(t *testing.T) {
	m := New(Options{})
	foldAll(t, m, true,
		agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: "m1"}},
		agent.Event{Type: agent.EventTool, Agent: "subA", Tool: &agent.ToolEvent{ID: "sa1"}},
		agent.Event{Type: agent.EventTool, Agent: "subB", Tool: &agent.ToolEvent{ID: "sb1"}},
		agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: "m2"}},
		agent.Event{Type: agent.EventTool, Agent: "subA", Tool: &agent.ToolEvent{ID: "sa2"}},
	)
	got := m.Tools()
	ids := make([]string, len(got))
	for i, tv := range got {
		ids[i] = tv.ID
	}
	want := []string{"m1", "m2", "sa1", "sa2", "sb1"}
	if !slices.Equal(ids, want) {
		t.Fatalf("Tools() order is %v, want main's own order then each child's, children in creation order: %v", ids, want)
	}
}

// TestATrimmedToolIsGoneFromTools: a tool the bound trims is gone from
// Tools(), exactly as it leaves State's Tools (fold_test.go's
// TestChangeSaysWhatTheFoldTouched pins the same drop on the map).
func TestATrimmedToolIsGoneFromTools(t *testing.T) {
	m := New(Options{Bounds: Bounds{MainEntries: 1}})
	m.Fold(agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: "t"}, Seq: 1})
	if got := m.Tools(); len(got) != 1 || got[0].ID != "t" {
		t.Fatalf("setup: Tools() is %+v, want the one tool row", got)
	}
	m.Fold(agent.Event{Type: agent.EventText, Text: "x", Seq: 2})
	if got := m.Tools(); len(got) != 0 {
		t.Fatalf("a trimmed tool must be gone from Tools(): %+v", got)
	}
}

// TestARestoredModelsToolsKeepOrder: a model restored from a snapshot, in
// process and through the codec, answers Tools() in the same order as the
// model it was restored from.
func TestARestoredModelsToolsKeepOrder(t *testing.T) {
	m := New(Options{})
	foldAll(t, m, true,
		agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: "a", Status: "pending"}},
		agent.Event{Type: agent.EventTool, Agent: "sub", Tool: &agent.ToolEvent{ID: "s1"}},
		agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: "a", Status: "completed"}},
	)
	want := m.Tools()
	for i, r := range restoreBoth(t, m, Options{}) {
		if got := r.Tools(); !reflect.DeepEqual(got, want) {
			t.Fatalf("restored twin %d's Tools() is %+v, want the first model's %+v", i, got, want)
		}
	}
}

// ownedTool is a tool payload with every nested field set: the slices, the
// Output and its ExitCode, the Task. Each call builds a fresh one, so a test
// can hold one as the expected value while the model holds another.
func ownedTool(id string) *agent.ToolEvent {
	code := 3
	return &agent.ToolEvent{
		ID: id, Status: "completed", Kind: "edit", Title: "Edit a.go",
		Locations: []string{"a.go", "b.go"},
		Output:    &agent.ToolOutput{ExitCode: &code, Stdout: "out", Stderr: "err"},
		Diffs:     []agent.ToolDiff{{Path: "a.go", OldText: "x", NewText: "y", Added: 1, Removed: 1}},
		Task:      &agent.TaskInfo{Description: "count", AgentID: "child-1", Status: agent.SubagentRunning},
	}
}

// scribble writes over every field of tv a caller could reach, the nested
// ones included.
func scribble(tv *agent.ToolEvent) {
	tv.Status = "scribbled"
	tv.Locations[0] = "scribbled"
	tv.Diffs[0].Path = "scribbled"
	tv.Output.Stdout = "scribbled"
	*tv.Output.ExitCode = 99
	tv.Task.Status = agent.SubagentFailed
	tv.Task.AgentID = "scribbled"
}

// TestToolsAreTheCallersOwn (sol r50 5): what Tools() answers is the caller's
// to change, the nested fields included — Locations, Diffs, the Output and
// its ExitCode, the Task — and nothing a caller writes into it reaches the
// model, the folded event's own payload, or what the next Tools() answers.
// Negative control: cut.tools copying the struct alone (`*e.Tool`) shares
// every nested field with the model, and this test goes red on the first of
// them.
func TestToolsAreTheCallersOwn(t *testing.T) {
	m := New(Options{})
	folded := ownedTool("t1")
	foldAll(t, m, true,
		agent.Event{Type: agent.EventTool, Tool: folded, Seq: 1},
		agent.Event{Type: agent.EventTool, Agent: "sub", Tool: ownedTool("s1"), Seq: 2},
	)
	got := m.Tools()
	if len(got) != 2 {
		t.Fatalf("setup: Tools() is %+v, want two rows", got)
	}
	for i := range got {
		scribble(&got[i])
	}
	if !reflect.DeepEqual(folded, ownedTool("t1")) {
		t.Fatalf("writing into Tools()' answer changed the folded event's payload: %+v", folded)
	}
	again := m.Tools()
	if want := []agent.ToolEvent{*ownedTool("t1"), *ownedTool("s1")}; !reflect.DeepEqual(again, want) {
		t.Fatalf("writing into Tools()' answer reached the model: Tools() now %+v, want %+v", again, want)
	}
	if st := m.State(); !reflect.DeepEqual(st.Tools[ToolKey{ID: "t1"}], ownedTool("t1")) {
		t.Fatalf("writing into Tools()' answer reached the state projection: %+v", st.Tools[ToolKey{ID: "t1"}])
	}
}

// TestToolsAnswerDoesNotRaceTheModel is TestToolsAreTheCallersOwn under the
// race detector: a caller writing into what Tools() answered while the model
// is read — a snapshot encodes every tool's nested fields — and folded is no
// data race, because nothing it holds is the model's. Negative control: as
// above; with -race this test reports the race between the write and the
// snapshot's read of the shared slice.
func TestToolsAnswerDoesNotRaceTheModel(t *testing.T) {
	m := New(Options{})
	m.Fold(agent.Event{Type: agent.EventTool, Tool: ownedTool("t1"), Seq: 1})
	got := m.Tools()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 200 {
			scribble(&got[0])
		}
	}()
	for i := range 200 {
		if _, err := m.Snapshot(0); err != nil {
			t.Errorf("Snapshot: %v", err)
			break
		}
		m.Fold(agent.Event{Type: agent.EventText, Text: "x", Seq: uint64(2 + i)})
		_ = m.Tools()
	}
	<-done
}
