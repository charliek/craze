package transcript

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/charliek/craze/internal/agent"
)

// The per-item caps of the mandatory sections (ItemCap, plan 024 §3.5; r5
// findings 1 and 2): no one oversized string — in an open ask, a todo, a
// queue row or a settings section — makes every snapshot ErrSnapshotTooLarge.
// Each is carried as its head and marked, a restored model surfaces the mark,
// and the event that replaces what was cut clears it.

const (
	capBudget = 4 << 20
	huge      = 5 << 20
)

// hugeStrings hands out distinct strings of huge bytes, plain ASCII, all
// sharing one backing array: the n-th starts n bytes into a run of distinct
// letters, so they differ from their first byte on.
type hugeStrings struct {
	base string
	n    int
}

func newHugeStrings() *hugeStrings {
	const lead = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"
	return &hugeStrings{base: lead + strings.Repeat("x", huge)}
}

func (h *hugeStrings) next(t *testing.T) string {
	t.Helper()
	if h.n >= 62 {
		t.Fatal("hugeStrings: out of distinct strings")
	}
	s := h.base[h.n : h.n+huge]
	h.n++
	return s
}

// fillHuge sets every string reachable from v to a huge one of its own: a
// slice gets one element and a map one entry, each filled the same way. A
// kind it does not know is a failure, so a payload type that grows one is
// taught here rather than skipped.
func fillHuge(t *testing.T, h *hugeStrings, path string, v reflect.Value) {
	t.Helper()
	switch v.Kind() {
	case reflect.String:
		v.SetString(h.next(t))
	case reflect.Bool, reflect.Int, reflect.Int64:
		// Not text; the caller sets what the fixture needs.
	case reflect.Pointer:
		p := reflect.New(v.Type().Elem())
		fillHuge(t, h, path, p.Elem())
		v.Set(p)
	case reflect.Slice:
		s := reflect.MakeSlice(v.Type(), 1, 1)
		fillHuge(t, h, path+"[0]", s.Index(0))
		v.Set(s)
	case reflect.Map:
		m := reflect.MakeMap(v.Type())
		k := reflect.New(v.Type().Key()).Elem()
		fillHuge(t, h, path+"{key}", k)
		e := reflect.New(v.Type().Elem()).Elem()
		fillHuge(t, h, path+"{value}", e)
		m.SetMapIndex(k, e)
		v.Set(m)
	case reflect.Struct:
		for i := range v.NumField() {
			fillHuge(t, h, path+"."+v.Type().Field(i).Name, v.Field(i))
		}
	default:
		t.Fatalf("%s: fillHuge has no case for %s (%s)", path, v.Kind(), v.Type())
	}
}

// stringsOf calls f with every string reachable from v, map keys included,
// and its path.
func stringsOf(path string, v reflect.Value, f func(path, s string)) {
	switch v.Kind() {
	case reflect.String:
		f(path, v.String())
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			stringsOf(path, v.Elem(), f)
		}
	case reflect.Slice, reflect.Array:
		for i := range v.Len() {
			stringsOf(fmt.Sprintf("%s[%d]", path, i), v.Index(i), f)
		}
	case reflect.Map:
		for _, k := range v.MapKeys() {
			stringsOf(path+"{key}", k, f)
			stringsOf(path+"{value}", v.MapIndex(k), f)
		}
	case reflect.Struct:
		for i := range v.NumField() {
			stringsOf(path+"."+v.Type().Field(i).Name, v.Field(i), f)
		}
	}
}

// TestEveryStringOfAnOpenAskIsCapped (r5 finding 1): an open ask of each kind
// — a permission, a question, an Auto plan (the kind with no transcript entry
// of its own) — with EVERY string its payload carries filled with 5 MiB, by
// reflection over agent's payload types, so a string field added to them later
// is filled too: its snapshot fits a 4 MiB budget, every string of the ask is
// its ItemCap head, the ask is Truncated, a restored model says so, and the
// event's own payload is untouched.
func TestEveryStringOfAnOpenAskIsCapped(t *testing.T) {
	for _, tc := range []struct {
		name string
		ev   func(*testing.T, *hugeStrings) agent.Event
	}{
		{"a permission", func(t *testing.T, h *hugeStrings) agent.Event {
			p := &agent.PermissionEvent{}
			fillHuge(t, h, "PermissionEvent", reflect.ValueOf(p).Elem())
			return agent.Event{Type: agent.EventPermission, Permission: p}
		}},
		{"a question", func(t *testing.T, h *hugeStrings) agent.Event {
			q := &agent.QuestionEvent{}
			fillHuge(t, h, "QuestionEvent", reflect.ValueOf(q).Elem())
			// A sub-question's own id and the Answers map's key naming it
			// stay whole (r6 fix) rather than being capped, so at the
			// filler's 5 MiB they alone would blow the ask past capBudget.
			// Shrink them to a few KiB over ItemCap: still provably NOT an
			// ItemCap head, without dwarfing the ask's other, capped,
			// strings.
			q.Questions[0].ID = q.Questions[0].ID[:ItemCap+1024]
			for k, v := range q.Answers {
				q.Answers = map[string][]string{k[:ItemCap+1024]: v}
			}
			return agent.Event{Type: agent.EventQuestion, Question: q}
		}},
		{"an Auto plan", func(t *testing.T, h *hugeStrings) agent.Event {
			p := &agent.PlanEvent{}
			fillHuge(t, h, "PlanEvent", reflect.ValueOf(p).Elem())
			p.Auto = true
			return agent.Event{Type: agent.EventPlan, Plan: p}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHugeStrings()
			ev := tc.ev(t, h)
			ev.At, ev.Seq = at(1), 1
			m := New(Options{})
			foldAll(t, m, true, ev)

			s, b := snapshotOf(t, m, capBudget)
			if len(s.Asks) != 1 || !s.Asks[0].Truncated {
				t.Fatalf("the ask: %d open, truncated %v", len(s.Asks), len(s.Asks) == 1 && s.Asks[0].Truncated)
			}
			// Every string of the ask — its ID and each one the filler set in
			// its body — is its head, except a question's own sub-question
			// ids and its Answers map keys (r6 fix): those are the ids
			// capAnswers keys by, and stay whole rather than risk colliding
			// two into one.
			n := 0
			head := func(path, str string) {
				n++
				switch path {
				case "Ask.Body.Question.Questions[0].ID", "Ask.Body.Question.Answers{key}":
					if len(str) != ItemCap+1024 {
						t.Errorf("%s: %d bytes, want it kept whole at its shrunk %d bytes (an id, r6 fix)", path, len(str), ItemCap+1024)
					}
				default:
					if len(str) != ItemCap {
						t.Errorf("%s: %d bytes, want its %d-byte head", path, len(str), ItemCap)
					}
				}
			}
			head("Ask.ID", s.Asks[0].ID)
			stringsOf("Ask.Body", reflect.ValueOf(s.Asks[0].Body), head)
			if n != h.n+1 {
				t.Fatalf("%d strings in the snapshot's ask, want the %d the filler set and the ask's ID", n, h.n)
			}
			stringsOf("event", reflect.ValueOf(ev), func(path, str string) {
				if strings.HasPrefix(path, "event.Type") || str == "" {
					return
				}
				want := huge
				switch path {
				case "event.Question.Questions[0].ID", "event.Question.Answers{key}":
					want = ItemCap + 1024 // shrunk above, r6 fix
				}
				if len(str) != want {
					t.Errorf("%s: the event's own payload was written (%d bytes, want %d)", path, len(str), want)
				}
			})
			t.Logf("%d strings capped; the snapshot encodes to %d bytes", n, len(b))
			r1, r2 := restoredBoth(t, s, b, Options{})
			for _, r := range []*Model{r1, r2} {
				if st := r.State(); len(st.Asks) != 1 || !st.Asks[0].Truncated {
					t.Fatalf("a restored client cannot tell the ask was truncated: %+v", st.Asks)
				}
			}
		})
	}
}

// TestTwoAnswerKeysSharingAPrefixBothSurvive (r6 fix): a question's Answers
// map with two keys over ItemCap that share a common ItemCap-length prefix —
// the shape that used to lose one answer, since capping the keys collapsed
// both to that shared prefix and one overwrote the other in the copy, the
// survivor depending on Go's map order. Keys stay whole now, so both keys,
// and both answers, are exact through a snapshot and a restore, even though
// one answer's value is still capped (over ItemCap on its own).
func TestTwoAnswerKeysSharingAPrefixBothSurvive(t *testing.T) {
	prefix := strings.Repeat("k", ItemCap)
	k1, k2 := prefix+"-a", prefix+"-b"
	bigVal := strings.Repeat("v", huge)
	q := &agent.QuestionEvent{
		ID:    "ask-1",
		Title: "two big keys",
		Answers: map[string][]string{
			k1: {bigVal},
			k2: {"short"},
		},
	}
	ev := agent.Event{Type: agent.EventQuestion, Question: q}
	ev.At, ev.Seq = at(1), 1
	m := New(Options{})
	foldAll(t, m, true, ev)

	s, b := snapshotOf(t, m, capBudget)
	if len(s.Asks) != 1 || !s.Asks[0].Truncated {
		t.Fatalf("the ask: %d open, truncated %v", len(s.Asks), len(s.Asks) == 1 && s.Asks[0].Truncated)
	}
	ans := s.Asks[0].Body.Question.Answers
	if len(ans) != 2 {
		t.Fatalf("%d answers, want 2 (both keys survived the cap)", len(ans))
	}
	if got := ans[k1]; len(got) != 1 || len(got[0]) != ItemCap {
		t.Fatalf("k1's answer: %d values, %d bytes, want a %d-byte head", len(got), len(got), ItemCap)
	}
	if got := ans[k2]; len(got) != 1 || got[0] != "short" {
		t.Fatalf("k2's answer: %v, want [short] untouched", got)
	}
	if st := m.State(); len(st.Asks[0].Body.Question.Answers[k1][0]) != huge {
		t.Fatal("capping wrote the model's own answers")
	}

	r1, r2 := restoredBoth(t, s, b, Options{})
	for _, r := range []*Model{r1, r2} {
		st := r.State()
		if len(st.Asks) != 1 {
			t.Fatalf("a restored client: %d open asks, want 1", len(st.Asks))
		}
		rans := st.Asks[0].Body.Question.Answers
		if len(rans) != 2 || len(rans[k1]) != 1 || len(rans[k1][0]) != ItemCap || len(rans[k2]) != 1 || rans[k2][0] != "short" {
			t.Fatalf("a restored client's answers: %d keys", len(rans))
		}
	}
}

// TestOversizedSectionsAreCarriedAsTheirHeads (r5 finding 2): a 5 MiB
// requeued row (agent.PromptQueue.PushFront's shape: queued at position 0,
// past the queue's 32 KiB limit), a 5 MiB todo, and settings catalogs with 5
// MiB strings each fit a 4 MiB snapshot as their heads, with their marks —
// the row named in TruncatedQueue, TodosTruncated, and each cut section in
// Settings.Truncated — which a restored model surfaces in State, and which the
// next event replacing the row, the list or the section clears.
func TestOversizedSectionsAreCarriedAsTheirHeads(t *testing.T) {
	big := strings.Repeat("z", huge)

	t.Run("a 5 MiB requeued row", func(t *testing.T) {
		m := New(Options{})
		foldAll(t, m, true, sequenced([]agent.Event{
			{Type: agent.EventQueue, Queue: &agent.QueuedPrompt{ID: "q-1", Text: "queued by hand", QueuedAt: at(1)}, QueueChange: agent.QueueQueued, QueuePos: 0, At: at(1)},
			{Type: agent.EventQueue, Queue: &agent.QueuedPrompt{ID: "q-2", Text: big, QueuedAt: at(2)}, QueueChange: agent.QueueQueued, QueuePos: 0, At: at(2)},
		})...)
		s, b := snapshotOf(t, m, capBudget)
		if len(s.Queue) != 2 || s.Queue[0].ID != "q-2" || s.Queue[0].Text != big[:ItemCap] || s.Queue[1].Text != "queued by hand" {
			t.Fatalf("the queue: %d rows, the front %q with %d bytes", len(s.Queue), s.Queue[0].ID, len(s.Queue[0].Text))
		}
		if !slices.Equal(s.TruncatedQueue, []string{"q-2"}) {
			t.Fatalf("TruncatedQueue %v, want the requeued row alone", s.TruncatedQueue)
		}
		if st := m.State(); len(st.Queue[0].Text) != huge || st.TruncatedQueue != nil {
			t.Fatal("capping wrote the model's own queue, or marked it")
		}
		r1, r2 := restoredBoth(t, s, b, Options{})
		for _, r := range []*Model{r1, r2} {
			if st := r.State(); !reflect.DeepEqual(st.TruncatedQueue, map[string]bool{"q-2": true}) {
				t.Fatalf("a restored client's TruncatedQueue: %v", st.TruncatedQueue)
			}
			// A snapshot of the restored model keeps the mark.
			if rs, _ := snapshotOf(t, r, capBudget); !slices.Equal(rs.TruncatedQueue, []string{"q-2"}) {
				t.Fatalf("a restored model's own snapshot lost the mark: %v", rs.TruncatedQueue)
			}
		}
		r1.Fold(agent.Event{Type: agent.EventQueue, Queue: &agent.QueuedPrompt{ID: "q-2", Text: "edited", Version: 1}, QueueChange: agent.QueueEdited, At: at(3), Seq: 3})
		if st := r1.State(); st.TruncatedQueue != nil || st.Queue[0].Text != "edited" {
			t.Fatalf("an edit replaces the row whole: %v", st.TruncatedQueue)
		}
		r2.Fold(agent.Event{Type: agent.EventQueue, Queue: &agent.QueuedPrompt{ID: "q-2"}, QueueChange: agent.QueueSent, At: at(3), Seq: 3})
		if st := r2.State(); st.TruncatedQueue != nil || len(st.Queue) != 1 {
			t.Fatalf("a sent row takes its mark with it: %v", st.TruncatedQueue)
		}
	})

	t.Run("a 5 MiB todo", func(t *testing.T) {
		m := New(Options{})
		foldAll(t, m, true, sequenced([]agent.Event{
			{Type: agent.EventTodos, Todos: []agent.Todo{{ID: "1", Content: "short", Status: "pending"}, {ID: "2", Content: big, Status: "pending"}}, At: at(1)},
		})...)
		s, b := snapshotOf(t, m, capBudget)
		if !s.TodosTruncated || s.Todos[0].Content != "short" || s.Todos[1].Content != big[:ItemCap] {
			t.Fatalf("the todos: truncated %v, %d bytes", s.TodosTruncated, len(s.Todos[1].Content))
		}
		if st := m.State(); len(st.Todos[1].Content) != huge || st.TodosTruncated {
			t.Fatal("capping wrote the model's own todos, or marked them")
		}
		r1, r2 := restoredBoth(t, s, b, Options{})
		for _, r := range []*Model{r1, r2} {
			if st := r.State(); !st.TodosTruncated {
				t.Fatal("a restored client cannot tell the todos were truncated")
			}
			r.Fold(agent.Event{Type: agent.EventTodos, Todos: []agent.Todo{{ID: "1", Content: "short", Status: "completed"}}, At: at(2), Seq: 2})
			if st := r.State(); st.TodosTruncated {
				t.Fatal("a new todo list replaces the list whole")
			}
		}
	})

	t.Run("settings catalogs with 5 MiB strings", func(t *testing.T) {
		m := New(Options{})
		foldAll(t, m, true, sequenced([]agent.Event{
			{Type: agent.EventMeta, State: &agent.StateDelta{
				Title:    strp("a title"),
				Config:   &agent.ConfigState{Options: []agent.ConfigOption{{ID: "effort", Current: "high"}}},
				Commands: &agent.CommandsState{Commands: []agent.CommandInfo{{Name: "review", Description: big}, {Name: "fix"}}},
				Plugins:  &agent.PluginsState{Plugins: []agent.PluginCommand{{Qualified: "p:x", Description: big}}},
				SendNow:  &agent.SendNowState{Armed: true, Text: big, FromRow: "q-2"},
			}, At: at(1)},
		})...)
		s, b := snapshotOf(t, m, capBudget)
		want := SettingsTruncated{Commands: true, Plugins: true, SendNow: true}
		if st := s.Settings; st.Truncated != want || len(st.Commands[0].Description) != ItemCap || st.Commands[1].Name != "fix" ||
			len(st.Plugins[0].Description) != ItemCap || len(st.SendNow.Text) != ItemCap || st.Title != "a title" {
			t.Fatalf("the settings: marks %+v", st.Truncated)
		}
		if st := m.State().Settings; len(st.Commands[0].Description) != huge || st.Truncated != (SettingsTruncated{}) {
			t.Fatal("capping wrote the model's own settings, or marked them")
		}
		r1, r2 := restoredBoth(t, s, b, Options{})
		for _, r := range []*Model{r1, r2} {
			if st := r.State(); st.Settings.Truncated != want {
				t.Fatalf("a restored client's marks: %+v", st.Settings.Truncated)
			}
			r.Fold(agent.Event{Type: agent.EventMeta, State: &agent.StateDelta{
				Commands: &agent.CommandsState{Commands: []agent.CommandInfo{{Name: "review"}}},
			}, At: at(2), Seq: 2})
			if st := r.State(); st.Settings.Truncated != (SettingsTruncated{Plugins: true, SendNow: true}) {
				t.Fatalf("a delta carrying the commands clears that section's mark alone: %+v", st.Settings.Truncated)
			}
		}
	})
}
