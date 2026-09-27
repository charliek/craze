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
