package transcript

import (
	"reflect"
	"slices"
	"testing"
	"time"

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

// ownedTool is a tool payload with every field set, every nested one
// included — the slices (two elements each), the Output and its ExitCode, the
// Task — by reflection (fillAll), so a field added to agent.ToolEvent later is
// set too, or fails the test that builds it. Each call builds a fresh one, so
// a test can hold one as the expected value while the model holds another.
func ownedTool(t testing.TB, id string) *agent.ToolEvent {
	t.Helper()
	tv := &agent.ToolEvent{}
	fillAll(t, reflect.ValueOf(tv).Elem(), "orig", true)
	tv.ID = id
	return tv
}

// scribble writes over every field of tv a caller can reach, through the
// memory tv already points at: each element of each slice in place, each
// pointee in place, never a new allocation — so whatever tv shares with the
// model is written, and a later field is reached by the same walk.
func scribble(t testing.TB, tv *agent.ToolEvent) {
	t.Helper()
	fillAll(t, reflect.ValueOf(tv).Elem(), "scribbled", false)
}

// fillAll sets every leaf reachable from v to a value derived from word. With
// alloc it makes what is missing — a nil pointer's pointee, two elements for
// an empty slice — so every leaf exists; without it, it writes only through
// what is there. A kind it does not know how to fill (a map, an interface, a
// func) fails the test, so a field of a new shape cannot slip past it.
func fillAll(t testing.TB, v reflect.Value, word string, alloc bool) {
	t.Helper()
	switch v.Kind() {
	case reflect.String:
		v.SetString(word + "-" + v.Type().Name())
	case reflect.Bool:
		v.SetBool(!v.Bool() || alloc)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(int64(len(word)) + v.Int() + 1)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(uint64(len(word)) + v.Uint() + 1)
	case reflect.Float32, reflect.Float64:
		v.SetFloat(float64(len(word)) + v.Float() + 1)
	case reflect.Pointer:
		if v.IsNil() {
			if !alloc {
				t.Fatalf("scribble: a nil %s the filled payload should have set", v.Type())
			}
			v.Set(reflect.New(v.Type().Elem()))
		}
		fillAll(t, v.Elem(), word, alloc)
	case reflect.Slice:
		if v.Len() == 0 {
			if !alloc {
				t.Fatalf("scribble: an empty %s the filled payload should have set", v.Type())
			}
			v.Set(reflect.MakeSlice(v.Type(), 2, 2))
		}
		for i := range v.Len() {
			fillAll(t, v.Index(i), word, alloc)
		}
	case reflect.Struct:
		if v.Type() == reflect.TypeFor[time.Time]() {
			v.Set(reflect.ValueOf(time.Date(2026, 9, 26, 10, len(word), 0, 0, time.UTC)))
			return
		}
		for i := range v.NumField() {
			if !v.Type().Field(i).IsExported() {
				t.Fatalf("fillAll: %s has an unexported field %s it cannot set", v.Type(), v.Type().Field(i).Name)
			}
			fillAll(t, v.Field(i), word, alloc)
		}
	default:
		t.Fatalf("fillAll: %s is a %s, which this walk does not fill: teach it, so the ownership tests cover it", v.Type(), v.Kind())
	}
}

// TestToolsAreTheCallersOwn (sol r50 5): what Tools() answers is the caller's
// to change, every nested field included, and nothing a caller writes into it
// reaches the model: not the folded event's own payload, not what the next
// Tools() or State() answers, not a snapshot. Negative control: cut.tools
// copying the struct alone (`*e.Tool`) shares every nested field with the
// model, and this test goes red on the first of them; dropping any one field
// from cloneTool (the Task's copy, say) goes red too, because scribble writes
// every field there is.
func TestToolsAreTheCallersOwn(t *testing.T) {
	m := New(Options{})
	folded := ownedTool(t, "t1")
	foldAll(t, m, true,
		agent.Event{Type: agent.EventTool, Tool: folded, Seq: 1},
		agent.Event{Type: agent.EventTool, Agent: "sub", Tool: ownedTool(t, "s1"), Seq: 2},
	)
	snapBefore, err := m.Snapshot(0)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	got := m.Tools()
	if len(got) != 2 {
		t.Fatalf("setup: Tools() is %+v, want two rows", got)
	}
	for i := range got {
		scribble(t, &got[i])
	}
	checkToolsUntouched(t, m, folded, snapBefore, "Tools()' answer")
}

// TestStateToolsAreTheCallersOwn (sol r51 4): State().Tools' values are the
// caller's own too — pointers to copies, never the model's retained payloads —
// so a caller writing through one changes nothing the model holds. Negative
// control: addTools storing e.Tool itself (the pointer the model retains) goes
// red on the folded event's payload.
func TestStateToolsAreTheCallersOwn(t *testing.T) {
	m := New(Options{})
	folded := ownedTool(t, "t1")
	foldAll(t, m, true,
		agent.Event{Type: agent.EventTool, Tool: folded, Seq: 1},
		agent.Event{Type: agent.EventTool, Agent: "sub", Tool: ownedTool(t, "s1"), Seq: 2},
	)
	snapBefore, err := m.Snapshot(0)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	st := m.State()
	if len(st.Tools) != 2 {
		t.Fatalf("setup: State().Tools is %+v, want two rows", st.Tools)
	}
	for _, tv := range st.Tools {
		scribble(t, tv)
	}
	checkToolsUntouched(t, m, folded, snapBefore, "State().Tools' values")
}

// checkToolsUntouched holds the model of the two ownership tests to what it
// folded: the folded payload as it was built, Tools() and State().Tools
// answering it, and a snapshot equal to the one taken before the writes.
func checkToolsUntouched(t *testing.T, m *Model, folded *agent.ToolEvent, snapBefore *Snapshot, what string) {
	t.Helper()
	if !reflect.DeepEqual(folded, ownedTool(t, "t1")) {
		t.Fatalf("writing into %s changed the folded event's payload: %+v", what, folded)
	}
	if got, want := m.Tools(), []agent.ToolEvent{*ownedTool(t, "t1"), *ownedTool(t, "s1")}; !reflect.DeepEqual(got, want) {
		t.Fatalf("writing into %s reached the model: Tools() now %+v, want %+v", what, got, want)
	}
	st := m.State()
	if !reflect.DeepEqual(st.Tools[ToolKey{ID: "t1"}], ownedTool(t, "t1")) || !reflect.DeepEqual(st.Tools[ToolKey{Agent: "sub", ID: "s1"}], ownedTool(t, "s1")) {
		t.Fatalf("writing into %s reached the state projection: %+v", what, st.Tools)
	}
	snapAfter, err := m.Snapshot(0)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if !reflect.DeepEqual(snapAfter, snapBefore) {
		t.Fatalf("writing into %s reached a snapshot:\nbefore %+v\nafter  %+v", what, snapBefore, snapAfter)
	}
}

// TestToolsAnswerDoesNotRaceTheModel is the ownership tests under the race
// detector, with the writes overlapping the model's own work on the same tool:
// the reader folds an update of that tool, takes Tools() and State() and hands
// both answers to a writer goroutine, then goes straight on to snapshot the
// model — which encodes every nested field of the payloads it retains — and to
// fold the next update, while the writer writes every field of what it was
// handed. Nothing orders the writer's writes before the reader's next reads, so
// if any answer shared memory with the model the race detector would report it.
// Both goroutines start at one barrier and the writer keeps writing the last
// answers until the reader is done, so they run side by side for the whole
// loop. Negative control: either projection sharing its payloads (cut.tools
// copying `*e.Tool`, or addTools storing e.Tool) reports a data race here
// under -race.
func TestToolsAnswerDoesNotRaceTheModel(t *testing.T) {
	m := New(Options{})
	m.Fold(agent.Event{Type: agent.EventTool, Tool: ownedTool(t, "t1"), Seq: 1})
	type answers struct {
		tools []agent.ToolEvent
		state *agent.ToolEvent
	}
	handed := make(chan answers, 1)
	start, stop, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	// The writer reports nothing (scribbleNoT): Fatalf is the test
	// goroutine's alone.
	go func() {
		defer close(done)
		<-start
		var last answers
		for {
			select {
			case <-stop:
				return
			case a := <-handed:
				last = a
			default:
			}
			for i := range last.tools {
				scribbleNoT(&last.tools[i])
			}
			if last.state != nil {
				scribbleNoT(last.state)
			}
		}
	}()
	close(start)
	for i := range 300 {
		m.Fold(agent.Event{Type: agent.EventTool, Tool: ownedTool(t, "t1"), Seq: uint64(2 + i)})
		a := answers{tools: m.Tools(), state: m.State().Tools[ToolKey{ID: "t1"}]}
		select {
		case handed <- a:
		default:
		}
		if _, err := m.Snapshot(0); err != nil {
			t.Errorf("Snapshot: %v", err)
			break
		}
		_ = m.Tools()
		_ = m.State()
	}
	close(stop)
	<-done
}

// scribbleNoT is scribble for a goroutine that is not the test's: the same
// walk, reporting nothing (the payloads it is handed were built by ownedTool,
// which has already proved the walk fills every field).
func scribbleNoT(tv *agent.ToolEvent) {
	fillAll(noT{}, reflect.ValueOf(tv).Elem(), "scribbled", false)
}

// noT is a testing.TB that ignores a failure: scribbleNoT's walk never fails
// on a payload ownedTool built.
type noT struct{ testing.TB }

func (noT) Helper()               {}
func (noT) Fatalf(string, ...any) {}
