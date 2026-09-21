package opencode

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/charliek/craze/internal/harness/redact"
	"github.com/charliek/craze/internal/harness/tool"
)

// stubTodoStore is a tool.TodoStore that records what it was called with and
// returns a fixed answer, for tests that only need to check what todo_write
// hands the store and how it renders what the store hands back — the
// store's own semantics (merge, replace, the auto-upgrade, the caps) are
// harness's to test (internal/harness/todos_test.go), since todo_write must
// not depend on any one implementation of the seam.
type stubTodoStore struct {
	mu      sync.Mutex
	calls   []stubCall
	list    []tool.Todo
	dropped int
}

type stubCall struct {
	merge   *bool
	updates []tool.TodoUpdate
}

func (s *stubTodoStore) Write(merge *bool, updates []tool.TodoUpdate) ([]tool.Todo, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, stubCall{merge, updates})
	return s.list, s.dropped
}

func (s *stubTodoStore) last() stubCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[len(s.calls)-1]
}

// newTodoFixture is a fixture over todo_write alone, with store wired as
// Env.Todos (nil is allowed: it is the "no store" case Run must not panic
// on).
func newTodoFixture(t *testing.T, store tool.TodoStore) *fixture {
	t.Helper()
	env := tool.Env{
		Workspace: t.TempDir(),
		Home:      t.TempDir(),
		Redactor:  redact.New(keyA),
		Environ:   tool.ChildEnviron(os.Environ(), nil),
		Locks:     &tool.PathLocks{},
		Todos:     store,
	}
	tw, err := newTodoWrite()
	if err != nil {
		t.Fatal(err)
	}
	d, err := tool.NewDispatcher(tool.Options{Tools: []tool.Tool{tw}, Env: env})
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{env: env, d: d}
}

func strPtr(s string) *string                      { return &s }
func statusPtr(s tool.TodoStatus) *tool.TodoStatus { return &s }
func boolPtr(b bool) *bool                         { return &b }

// TestTodoWriteAbsentFromProfile (plan 023 §5, C3): the tool is built and
// tested, but Profile does not offer it — C4 registers all three of D-53's
// tools together, and moves specs.golden and the pytest tool list once.
func TestTodoWriteAbsentFromProfile(t *testing.T) {
	p, err := Profile()
	if err != nil {
		t.Fatal(err)
	}
	if got := names(p); slices.Contains(got, "todo_write") {
		t.Fatalf("Profile's tools = %q, want no todo_write yet", got)
	}
}

// TestTodoWriteSpec: the row plan 023 §3.4 pins.
func TestTodoWriteSpec(t *testing.T) {
	tw, err := newTodoWrite()
	if err != nil {
		t.Fatal(err)
	}
	s := tw.Spec()
	if s.ID != "todo_write" || s.Kind != tool.KindTodo || !s.ReadOnly || !s.Parallel {
		t.Fatalf("spec = %+v, want {ID:todo_write Kind:todo ReadOnly:true Parallel:true}", s)
	}
	if got := s.Required; len(got) != 1 || got[0] != "todos" {
		t.Fatalf("Required = %v, want [todos]", got)
	}
}

// TestTodoWriteDescriptionAlias (D-53, plan 023 §7 A10): the description
// begins with the Claude Code alias sentence.
func TestTodoWriteDescriptionAlias(t *testing.T) {
	d, err := description("todo_write", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(d, "Claude Code calls this tool `TodoWrite`.") {
		t.Fatalf("description does not open with the alias sentence:\n%s", d)
	}
	if strings.Contains(d, "${") {
		t.Errorf("a placeholder is left: %q", d)
	}
}

// TestTodoWriteNilStore: Run with no Env.Todos wired is a clear tool_error,
// never a panic (plan 023 §3.4).
func TestTodoWriteNilStore(t *testing.T) {
	f := newTodoFixture(t, nil)
	_, res := f.call(t, "todo_write", map[string]any{"todos": []any{map[string]any{"id": "1"}}})
	failed(t, res, tool.ClassToolError, "The todo list is not available in this session.")
}

// TestTodoWritePrepareValidation: what Prepare itself must catch — an empty
// id, an unknown status, a non-object item, a non-array todos, a missing
// todos — each an invalid_input naming the field, before the store is ever
// asked (plan 023 §3.4).
func TestTodoWritePrepareValidation(t *testing.T) {
	f := newTodoFixture(t, &stubTodoStore{})
	cases := []struct {
		name, input, field string
	}{
		{"accepts a plain id", `{"todos":[{"id":"1"}]}`, ""},
		{"accepts merge true", `{"merge":true,"todos":[{"id":"1"}]}`, ""},
		{"accepts merge false", `{"merge":false,"todos":[{"id":"1","content":"a"}]}`, ""},
		{"accepts every status", `{"todos":[{"id":"1","status":"pending"},{"id":"2","status":"in_progress"},{"id":"3","status":"completed"},{"id":"4","status":"cancelled"}]}`, ""},
		{"accepts an empty todos array", `{"todos":[]}`, ""},
		{"refuses a missing todos", `{}`, "todos"},
		{"refuses a non-array todos", `{"todos":{}}`, "todos"},
		{"refuses a non-boolean merge", `{"merge":"yes","todos":[{"id":"1"}]}`, "merge"},
		{"refuses a non-object item", `{"todos":["x"]}`, "todos[0]"},
		{"refuses a missing id", `{"todos":[{"content":"a"}]}`, "todos[0]"},
		{"refuses an empty id", `{"todos":[{"id":""}]}`, "todos[0]"},
		{"refuses a blank id", `{"todos":[{"id":"  "}]}`, "todos[0]"},
		{"refuses an unknown status", `{"todos":[{"id":"1","status":"done"}]}`, "todos[0]"},
		{"refuses a non-string content", `{"todos":[{"id":"1","content":1}]}`, "todos[0]"},
		{"names the failing index", `{"todos":[{"id":"1"},{"id":""}]}`, "todos[1]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, res := f.call(t, "todo_write", tc.input)
			if tc.field == "" {
				ok(t, res)
				return
			}
			if !res.IsError || res.Class != tool.ClassInvalidInput || !strings.Contains(res.Text, tc.field) {
				t.Fatalf("result = %+v, want invalid_input naming %q", res, tc.field)
			}
		})
	}
}

// TestTodoWriteMergeTriState: the merge flag reaches the store as a tri-state
// — nil when the call omits it, &true and &false when it does not — which is
// what lets the store's auto-upgrade tell "the model said nothing" from "the
// model said false" (plan 023 §3.4).
func TestTodoWriteMergeTriState(t *testing.T) {
	cases := []struct {
		name, input string
		want        *bool
	}{
		{"omitted", `{"todos":[{"id":"1"}]}`, nil},
		{"true", `{"merge":true,"todos":[{"id":"1"}]}`, boolPtr(true)},
		{"false", `{"merge":false,"todos":[{"id":"1","content":"a"}]}`, boolPtr(false)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &stubTodoStore{}
			f := newTodoFixture(t, store)
			_, res := f.call(t, "todo_write", tc.input)
			ok(t, res)
			got := store.last().merge
			switch {
			case tc.want == nil && got != nil:
				t.Fatalf("merge = %v, want nil", *got)
			case tc.want != nil && (got == nil || *got != *tc.want):
				t.Fatalf("merge = %v, want %v", got, *tc.want)
			}
		})
	}
}

// TestTodoWriteUpdatesReachTheStore: content and status parse to nil when
// omitted (so the store can tell "omitted" from "empty"), and to the given
// value otherwise.
func TestTodoWriteUpdatesReachTheStore(t *testing.T) {
	store := &stubTodoStore{}
	f := newTodoFixture(t, store)
	_, res := f.call(t, "todo_write", `{"todos":[
		{"id":"1","content":"buy milk","status":"in_progress"},
		{"id":"2"}
	]}`)
	ok(t, res)
	got := store.last().updates
	want := []tool.TodoUpdate{
		{ID: "1", Content: strPtr("buy milk"), Status: statusPtr(tool.TodoInProgress)},
		{ID: "2"},
	}
	if len(got) != len(want) {
		t.Fatalf("updates = %+v, want %+v", got, want)
	}
	for i := range want {
		g, w := got[i], want[i]
		if g.ID != w.ID || !strEq(g.Content, w.Content) || !statusEq(g.Status, w.Status) {
			t.Errorf("update %d = %+v, want %+v", i, describeUpdate(g), describeUpdate(w))
		}
	}
}

func strEq(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func statusEq(a, b *tool.TodoStatus) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func describeUpdate(u tool.TodoUpdate) string {
	b, _ := json.Marshal(struct {
		ID      string
		Content *string
		Status  *tool.TodoStatus
	}{u.ID, u.Content, u.Status})
	return string(b)
}

// TestTodoWriteResultText: opencode's own shape — a pretty-printed JSON
// array of {id, content, status} — and the drop note appended when the
// store dropped anything (plan 023 §3.4).
func TestTodoWriteResultText(t *testing.T) {
	t.Run("the list, pretty-printed", func(t *testing.T) {
		store := &stubTodoStore{list: []tool.Todo{
			{ID: "1", Content: "buy milk", Status: tool.TodoPending},
			{ID: "2", Content: "walk the dog", Status: tool.TodoCompleted},
		}}
		f := newTodoFixture(t, store)
		_, res := f.call(t, "todo_write", `{"todos":[{"id":"1"}]}`)
		text := ok(t, res)
		var got []map[string]string
		if err := json.Unmarshal([]byte(text), &got); err != nil {
			t.Fatalf("result is not JSON: %v\n%s", err, text)
		}
		want := []map[string]string{
			{"id": "1", "content": "buy milk", "status": "pending"},
			{"id": "2", "content": "walk the dog", "status": "completed"},
		}
		if len(got) != len(want) || got[0]["id"] != want[0]["id"] || got[1]["status"] != want[1]["status"] {
			t.Fatalf("result = %s, want the two items rendered with id, content, status", text)
		}
		if !strings.HasPrefix(text, "[\n  {\n") {
			t.Errorf("result is not pretty-printed like JSON.stringify(_, null, 2):\n%s", text)
		}
	})

	t.Run("an empty list is []", func(t *testing.T) {
		f := newTodoFixture(t, &stubTodoStore{})
		_, res := f.call(t, "todo_write", `{"todos":[]}`)
		if text := ok(t, res); text != "[]" {
			t.Fatalf("result = %q, want []", text)
		}
	})

	t.Run("a drop note follows the list", func(t *testing.T) {
		store := &stubTodoStore{list: []tool.Todo{{ID: "1", Content: "a", Status: tool.TodoPending}}, dropped: 5}
		f := newTodoFixture(t, store)
		_, res := f.call(t, "todo_write", `{"todos":[{"id":"1"}]}`)
		text := ok(t, res)
		if !strings.Contains(text, "5 todo items dropped") || !strings.Contains(text, "capped at 64 items") {
			t.Fatalf("result lacks the drop note: %s", text)
		}
	})

	t.Run("one dropped item is singular", func(t *testing.T) {
		store := &stubTodoStore{list: []tool.Todo{{ID: "1", Content: "a", Status: tool.TodoPending}}, dropped: 1}
		f := newTodoFixture(t, store)
		_, res := f.call(t, "todo_write", `{"todos":[{"id":"1"}]}`)
		text := ok(t, res)
		if !strings.Contains(text, "1 todo item dropped") {
			t.Fatalf("result = %s, want the singular form", text)
		}
	})
}

// TestTodoWriteTitle: the card's line is opencode's own count of what the
// call is not marking completed, not the store's full list size.
func TestTodoWriteTitle(t *testing.T) {
	cases := []struct {
		name, input, want string
	}{
		{"one, not completed", `{"todos":[{"id":"1"}]}`, "1 todo"},
		{"three, one completed", `{"todos":[{"id":"1"},{"id":"2","status":"completed"},{"id":"3","status":"in_progress"}]}`, "2 todos"},
		{"none", `{"todos":[]}`, "0 todos"},
	}
	store := &stubTodoStore{}
	f := newTodoFixture(t, store)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, res := f.call(t, "todo_write", tc.input)
			ok(t, res)
			if req.Title != tc.want {
				t.Fatalf("title = %q, want %q", req.Title, tc.want)
			}
		})
	}
}

// TestTodoWriteAborted: a cancelled context is aborted before the store is
// ever asked, like every other tool.
func TestTodoWriteAborted(t *testing.T) {
	store := &stubTodoStore{}
	f := newTodoFixture(t, store)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, res := f.callCtx(t, ctx, "todo_write", `{"todos":[{"id":"1"}]}`)
	failed(t, res, tool.ClassAborted, tool.AbortedText)
	if len(store.calls) != 0 {
		t.Fatalf("the store was called on an aborted context: %+v", store.calls)
	}
}
