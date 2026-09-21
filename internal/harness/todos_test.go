package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"charm.land/fantasy"
	"github.com/charliek/craze/internal/harness/tool"
	"github.com/charliek/craze/internal/harness/tool/opencode"
)

func str(s string) *string                      { return &s }
func status(s tool.TodoStatus) *tool.TodoStatus { return &s }

func todoItem(id, content string, status tool.TodoStatus) tool.Todo {
	return tool.Todo{ID: id, Content: content, Status: status}
}

func equalTodos(t *testing.T, got, want []tool.Todo) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("todos = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("todo %d = %+v, want %+v (full: got %+v want %+v)", i, got[i], want[i], got, want)
		}
	}
}

// TestSessionTodosMergeDefault: a nil merge patches by id, keeping an
// omitted content and creating an unknown id with the id as its content
// (plan 023 §3.4).
func TestSessionTodosMergeDefault(t *testing.T) {
	b := newSessionTodos()
	list, dropped := b.Write(nil, []tool.TodoUpdate{
		{ID: "1", Content: str("explore"), Status: status(tool.TodoInProgress)},
		{ID: "2", Content: str("write tests")},
	})
	if dropped != 0 {
		t.Fatalf("dropped = %d, want 0", dropped)
	}
	equalTodos(t, list, []tool.Todo{
		todoItem("1", "explore", tool.TodoInProgress),
		todoItem("2", "write tests", tool.TodoPending),
	})

	// A merge that only flips status keeps the content; a merge that names an
	// id the list does not have creates it, with the id as content (no
	// content was given).
	list, dropped = b.Write(nil, []tool.TodoUpdate{
		{ID: "1", Status: status(tool.TodoCompleted)},
		{ID: "3"},
	})
	if dropped != 0 {
		t.Fatalf("dropped = %d, want 0", dropped)
	}
	equalTodos(t, list, []tool.Todo{
		todoItem("1", "explore", tool.TodoCompleted), // content preserved
		todoItem("2", "write tests", tool.TodoPending),
		todoItem("3", "3", tool.TodoPending), // id used as fallback content
	})
}

// TestSessionTodosReplace: an explicit merge:false rebuilds the list from
// the call alone.
func TestSessionTodosReplace(t *testing.T) {
	b := newSessionTodos()
	b.Write(nil, []tool.TodoUpdate{{ID: "old", Content: str("stale"), Status: status(tool.TodoCompleted)}})

	replace := false
	list, dropped := b.Write(&replace, []tool.TodoUpdate{
		{ID: "new", Content: str("fresh")},
		{ID: "bare"}, // no content: falls back to the id; no status: pending
	})
	if dropped != 0 {
		t.Fatalf("dropped = %d, want 0", dropped)
	}
	equalTodos(t, list, []tool.Todo{
		todoItem("new", "fresh", tool.TodoPending),
		todoItem("bare", "bare", tool.TodoPending),
	})
}

// TestSessionTodosAutoUpgrade: grok-build's regression fix (todo/mod.rs:
// 334-348) — an explicit merge:false is still a merge when every update
// names an existing id and carries no content; it is a real replace the
// moment either condition fails.
func TestSessionTodosAutoUpgrade(t *testing.T) {
	replace := false

	t.Run("applies: every update is a status-only existing id", func(t *testing.T) {
		b := newSessionTodos()
		b.Write(nil, []tool.TodoUpdate{
			{ID: "1", Content: str("explore codebase"), Status: status(tool.TodoInProgress)},
			{ID: "2", Content: str("review tools"), Status: status(tool.TodoPending)},
		})
		list, _ := b.Write(&replace, []tool.TodoUpdate{
			{ID: "1", Status: status(tool.TodoCompleted)},
			{ID: "2", Status: status(tool.TodoCompleted)},
		})
		equalTodos(t, list, []tool.Todo{
			todoItem("1", "explore codebase", tool.TodoCompleted), // content survives: it upgraded to a merge
			todoItem("2", "review tools", tool.TodoCompleted),
		})
	})

	t.Run("does not apply: one update carries content", func(t *testing.T) {
		b := newSessionTodos()
		b.Write(nil, []tool.TodoUpdate{{ID: "1", Content: str("explore"), Status: status(tool.TodoInProgress)}})
		list, _ := b.Write(&replace, []tool.TodoUpdate{
			{ID: "1", Content: str("explore, reworded"), Status: status(tool.TodoCompleted)},
		})
		// A real replace: the list is exactly the call, nothing more.
		equalTodos(t, list, []tool.Todo{todoItem("1", "explore, reworded", tool.TodoCompleted)})
	})

	t.Run("does not apply: one update names an unknown id", func(t *testing.T) {
		b := newSessionTodos()
		b.Write(nil, []tool.TodoUpdate{{ID: "1", Content: str("explore"), Status: status(tool.TodoInProgress)}})
		list, _ := b.Write(&replace, []tool.TodoUpdate{
			{ID: "1", Status: status(tool.TodoCompleted)},
			{ID: "unknown", Status: status(tool.TodoPending)},
		})
		// A real replace: "1" loses its content (none given) and falls back
		// to its own id, but keeps the status the call gave it, and
		// "unknown" is created the same way.
		equalTodos(t, list, []tool.Todo{
			todoItem("1", "1", tool.TodoCompleted),
			todoItem("unknown", "unknown", tool.TodoPending),
		})
	})

	t.Run("does not apply: the list is empty", func(t *testing.T) {
		b := newSessionTodos()
		list, _ := b.Write(&replace, []tool.TodoUpdate{{ID: "1", Status: status(tool.TodoCompleted)}})
		equalTodos(t, list, []tool.Todo{todoItem("1", "1", tool.TodoCompleted)})
	})
}

// TestSessionTodosDuplicateIDsLastWins: a duplicate id within one call keeps
// the last occurrence's values, at the position the id first appeared.
func TestSessionTodosDuplicateIDsLastWins(t *testing.T) {
	b := newSessionTodos()
	replace := false
	list, dropped := b.Write(&replace, []tool.TodoUpdate{
		{ID: "1", Content: str("first")},
		{ID: "2", Content: str("middle")},
		{ID: "1", Content: str("last")}, // repeats "1": wins, but stays first
	})
	if dropped != 0 {
		t.Fatalf("dropped = %d, want 0", dropped)
	}
	equalTodos(t, list, []tool.Todo{
		todoItem("1", "last", tool.TodoPending),
		todoItem("2", "middle", tool.TodoPending),
	})
}

// TestSessionTodosCap: the flood — 1000 items in one call — is capped at
// tool.TodoCap, with the overflow's count in dropped.
func TestSessionTodosCap(t *testing.T) {
	b := newSessionTodos()
	const flood = 1000
	updates := make([]tool.TodoUpdate, flood)
	for i := range updates {
		id := fmt.Sprintf("t%d", i)
		updates[i] = tool.TodoUpdate{ID: id, Content: str(id)}
	}
	replace := false
	list, dropped := b.Write(&replace, updates)
	if len(list) != tool.TodoCap {
		t.Fatalf("len(list) = %d, want %d", len(list), tool.TodoCap)
	}
	if want := flood - tool.TodoCap; dropped != want {
		t.Fatalf("dropped = %d, want %d", dropped, want)
	}
	for i, it := range list {
		if it.ID != fmt.Sprintf("t%d", i) {
			t.Fatalf("kept item %d has id %q, want the first %d in call order", i, it.ID, tool.TodoCap)
		}
	}
}

// TestSessionTodosContentTruncation: content is kept to tool.TodoContentBytes
// bytes, cut on a UTF-8 boundary with a trailing "…" that itself counts
// toward the cap — proved with two-byte (é) and four-byte (an emoji)
// characters straddling byte 200.
func TestSessionTodosContentTruncation(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{"two-byte runes straddling the boundary", strings.Repeat("é", 150)},            // 300 bytes
		{"a four-byte emoji straddling the boundary", strings.Repeat("a", 197) + "🎉🎉🎉"}, // 197 + 12 bytes
		{"already within the cap", strings.Repeat("x", 50)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := newSessionTodos()
			list, _ := b.Write(nil, []tool.TodoUpdate{{ID: "1", Content: str(tc.content)}})
			got := list[0].Content
			if len(got) > tool.TodoContentBytes {
				t.Fatalf("content is %d bytes, want at most %d: %q", len(got), tool.TodoContentBytes, got)
			}
			if !utf8.ValidString(got) {
				t.Fatalf("content is not valid UTF-8: %q", got)
			}
			if len(tc.content) <= tool.TodoContentBytes {
				if got != tc.content {
					t.Fatalf("content = %q, want it untouched: %q", got, tc.content)
				}
			} else if !strings.HasSuffix(got, "…") {
				t.Fatalf("content = %q, want it to end in the ellipsis", got)
			}
		})
	}
}

// TestSessionTodosNilEmitIsSafe: Write works with no turn attached (between
// turns, or a harness test that never attaches one) — the mutation still
// happens, only the event is skipped.
func TestSessionTodosNilEmitIsSafe(t *testing.T) {
	b := newSessionTodos()
	list, _ := b.Write(nil, []tool.TodoUpdate{{ID: "1", Content: str("a")}})
	equalTodos(t, list, []tool.Todo{todoItem("1", "a", tool.TodoPending)})
}

// TestSessionTodosOrderedEventsUnderParallelWrites (plan 023 §3.4, run under
// -race): N goroutines write through the closest seam to the real turn path
// — sessionTodos.attach wired to a real turn's emitLocked, exactly as
// turn.go's Run does it, without needing the tool registered in a profile or
// a Fantasy turn actually running one. Each goroutine's own id is new, so
// each Write adds exactly one item; the assertions are what the doc on
// sessionTodos promises: the sink sees a strictly growing, never-reordered
// sequence of full lists — event k has exactly k+1 items and is event k-1
// plus one more — and each goroutine's own return value from Write is
// exactly one of those events, proving the return and the emit are the same
// snapshot for that call.
func TestSessionTodosOrderedEventsUnderParallelWrites(t *testing.T) {
	b := newSessionTodos()

	var mu sync.Mutex
	var got []Todos
	tn := &turn{sink: func(ev Event) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, ev.(Todos))
	}}
	release := b.attach(tn.emitLocked)
	defer release()

	const n = 32
	returned := make([][]tool.Todo, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("t%d", i)
			list, dropped := b.Write(nil, []tool.TodoUpdate{{ID: id, Content: str(id)}})
			if dropped != 0 {
				t.Errorf("goroutine %d: dropped = %d, want 0", i, dropped)
			}
			returned[i] = list
		}(i)
	}
	wg.Wait()

	mu.Lock()
	events := append([]Todos(nil), got...)
	mu.Unlock()

	if len(events) != n {
		t.Fatalf("the sink saw %d events, want %d", len(events), n)
	}
	for i, ev := range events {
		if len(ev.Items) != i+1 {
			t.Fatalf("event %d has %d items, want %d (the sequence must grow by exactly one each write)", i, len(ev.Items), i+1)
		}
		if i > 0 {
			prev := events[i-1].Items
			for j, it := range prev {
				if ev.Items[j] != it {
					t.Fatalf("event %d does not extend event %d at position %d: %+v vs %+v", i, i-1, j, ev.Items[j], it)
				}
			}
		}
	}
	for i, list := range returned {
		found := false
		for _, ev := range events {
			if todosEqual(ev.Items, list) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("goroutine %d's returned list %+v is not any emitted event", i, list)
		}
	}
}

func todosEqual(a, b []tool.Todo) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// echoTodoTool is a parallel tool that writes one item to env.Todos and
// returns the store's list — a stand-in for todo_write, which is not
// registered until C4, so a real turn can still exercise the production
// wiring end to end: the dispatcher's fixed Env.Todos, turn.go's attach of
// emitLocked, and sessionTodos.Write's own lock.
type echoTodoTool struct{}

func (echoTodoTool) Spec() tool.Spec {
	return tool.Spec{
		ID: "echo_todo", Description: "Writes one todo item, for a test.",
		Parameters: map[string]any{"id": map[string]any{"type": "string"}},
		Required:   []string{"id"},
		Kind:       tool.KindTodo, ReadOnly: true, Parallel: true, Truncate: tool.None,
	}
}

func (echoTodoTool) Prepare(_ tool.Env, c tool.Call) (tool.Prepared, error) {
	var in struct{ ID string }
	if err := json.Unmarshal(c.Input, &in); err != nil {
		return nil, err
	}
	return echoTodoCall(in.ID), nil
}

type echoTodoCall string

func (c echoTodoCall) Request() tool.Request { return tool.Request{Title: string(c)} }

func (c echoTodoCall) Run(_ context.Context, env tool.Env) tool.Result {
	if env.Todos == nil {
		return tool.Result{Text: "no store", IsError: true, Class: tool.ClassToolError}
	}
	content := string(c)
	list, _ := env.Todos.Write(nil, []tool.TodoUpdate{{ID: content, Content: &content}})
	b, _ := json.Marshal(list)
	return tool.Result{Text: string(b)}
}

// TestTodoEventsOrderedThroughARealTurn (plan 023 §3.4, run under -race): the
// same guarantee as TestSessionTodosOrderedEventsUnderParallelWrites, proved
// instead through an actual Session.Run — Fantasy dispatching echo_todo calls
// on its own parallel tool goroutines, the real dispatcher, the real
// turn.go's attach of emitLocked — with no direct call to sessionTodos at
// all. todo_write itself is not registered until C4 (Profile's builders,
// tools.go), so the test seam adds echoTodoTool to the profile the way
// race_test.go's spinTool does.
func TestTodoEventsOrderedThroughARealTurn(t *testing.T) {
	f := newFixture(t, "http://127.0.0.1:1/v1")
	opts := f.options()
	opts.tools.profiles = func() (*tool.Registry, error) {
		p, err := opencode.Profile()
		if err != nil {
			return nil, err
		}
		p.Tools = append(p.Tools, echoTodoTool{})
		var reg tool.Registry
		return &reg, reg.Register(p)
	}
	s := f.open(opts)

	const n = 6 // more than Fantasy's five parallel tool-goroutine slots
	var parts [][]fantasy.StreamPart
	for i := range n {
		id := fmt.Sprintf("e%d", i)
		parts = append(parts, callParts(id, "echo_todo", fmt.Sprintf(`{"id":%q}`, id)))
	}
	f.models["test/a"].push(callStep(parts...), answerWith("done"))

	var mu sync.Mutex
	var todosEvents []Todos
	sink := func(ev Event) {
		if e, ok := ev.(Todos); ok {
			mu.Lock()
			todosEvents = append(todosEvents, e)
			mu.Unlock()
		}
	}
	res, err := s.Run(context.Background(), "write todos", sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.StopReason != StopEndTurn {
		t.Fatalf("Run stop reason = %q, want end_turn", res.StopReason)
	}

	mu.Lock()
	events := append([]Todos(nil), todosEvents...)
	mu.Unlock()

	if len(events) != n {
		t.Fatalf("the sink saw %d Todos events, want %d", len(events), n)
	}
	for i, ev := range events {
		if len(ev.Items) != i+1 {
			t.Fatalf("event %d has %d items, want %d (the sequence must grow by exactly one each write)", i, len(ev.Items), i+1)
		}
		if i > 0 {
			prev := events[i-1].Items
			for j, it := range prev {
				if ev.Items[j] != it {
					t.Fatalf("event %d does not extend event %d at position %d: %+v vs %+v", i, i-1, j, ev.Items[j], it)
				}
			}
		}
	}
}
