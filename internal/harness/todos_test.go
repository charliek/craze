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

// writeTodos is Write with a live context, for every test here that has
// nothing to interrupt: it fails the test if the store reported a cancelled
// one, which only TestSessionTodosCancelledBehindTheLock arranges.
func writeTodos(t *testing.T, b *sessionTodos, merge *bool, updates []tool.TodoUpdate) ([]tool.Todo, int) {
	t.Helper()
	list, dropped, ok := b.Write(context.Background(), merge, updates)
	if !ok {
		t.Fatal("Write reported a cancelled context, which this test never arranges")
	}
	return list, dropped
}

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
	list, dropped := writeTodos(t, b, nil, []tool.TodoUpdate{
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
	list, dropped = writeTodos(t, b, nil, []tool.TodoUpdate{
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
	writeTodos(t, b, nil, []tool.TodoUpdate{{ID: "old", Content: str("stale"), Status: status(tool.TodoCompleted)}})

	replace := false
	list, dropped := writeTodos(t, b, &replace, []tool.TodoUpdate{
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
//
// "Carries no content" includes the spelling the model actually sends,
// content:null, which todo_write parses to the same nil Content as an omitted
// field: that is the other half of this case, pinned next door in opencode's
// TestTodoWriteNullIsOmitted, since the tool package may not import this one.
func TestSessionTodosAutoUpgrade(t *testing.T) {
	replace := false

	t.Run("applies: every update is a status-only existing id", func(t *testing.T) {
		b := newSessionTodos()
		writeTodos(t, b, nil, []tool.TodoUpdate{
			{ID: "1", Content: str("explore codebase"), Status: status(tool.TodoInProgress)},
			{ID: "2", Content: str("review tools"), Status: status(tool.TodoPending)},
		})
		list, _ := writeTodos(t, b, &replace, []tool.TodoUpdate{
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
		writeTodos(t, b, nil, []tool.TodoUpdate{{ID: "1", Content: str("explore"), Status: status(tool.TodoInProgress)}})
		list, _ := writeTodos(t, b, &replace, []tool.TodoUpdate{
			{ID: "1", Content: str("explore, reworded"), Status: status(tool.TodoCompleted)},
		})
		// A real replace: the list is exactly the call, nothing more.
		equalTodos(t, list, []tool.Todo{todoItem("1", "explore, reworded", tool.TodoCompleted)})
	})

	t.Run("does not apply: one update names an unknown id", func(t *testing.T) {
		b := newSessionTodos()
		writeTodos(t, b, nil, []tool.TodoUpdate{{ID: "1", Content: str("explore"), Status: status(tool.TodoInProgress)}})
		list, _ := writeTodos(t, b, &replace, []tool.TodoUpdate{
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
		list, _ := writeTodos(t, b, &replace, []tool.TodoUpdate{{ID: "1", Status: status(tool.TodoCompleted)}})
		equalTodos(t, list, []tool.Todo{todoItem("1", "1", tool.TodoCompleted)})
	})
}

// TestSessionTodosDuplicateIDsLastWins: a duplicate id within one call keeps
// the last occurrence's values, at the position the id first appeared.
func TestSessionTodosDuplicateIDsLastWins(t *testing.T) {
	b := newSessionTodos()
	replace := false
	list, dropped := writeTodos(t, b, &replace, []tool.TodoUpdate{
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
	list, dropped := writeTodos(t, b, &replace, updates)
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
		want    string // exactly what is kept, prefix and all
		bytes   int
	}{
		{
			// 300 bytes of two-byte runes. Byte 197, where the cut would go,
			// is a continuation byte, so the cut steps back to 196: 98 whole
			// runes and the ellipsis, 199 bytes.
			name: "two-byte runes straddling the boundary", content: strings.Repeat("é", 150),
			want: strings.Repeat("é", 98) + "…", bytes: 199,
		},
		{
			// 209 bytes, with the first emoji starting exactly at byte 197:
			// the cut lands on a rune start and nothing steps back, so this is
			// the boundary case that fills the cap exactly.
			name: "a four-byte emoji straddling the boundary", content: strings.Repeat("a", 197) + "🎉🎉🎉",
			want: strings.Repeat("a", 197) + "…", bytes: tool.TodoContentBytes,
		},
		{
			name: "already within the cap", content: strings.Repeat("x", 50),
			want: strings.Repeat("x", 50), bytes: 50,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := newSessionTodos()
			list, _ := writeTodos(t, b, nil, []tool.TodoUpdate{{ID: "1", Content: str(tc.content)}})
			got := list[0].Content
			switch {
			case got != tc.want:
				t.Fatalf("content = %q (%d bytes), want %q (%d)", got, len(got), tc.want, len(tc.want))
			case len(got) != tc.bytes || len(got) > tool.TodoContentBytes:
				t.Fatalf("content is %d bytes, want %d and at most %d", len(got), tc.bytes, tool.TodoContentBytes)
			case !utf8.ValidString(got):
				t.Fatalf("content is not valid UTF-8: %q", got)
			}
		})
	}
}

// TestSessionTodosCancelledBehindTheLock: todo_write is Parallel,
// so a second call of the same step can pass the tool's own check and then
// wait behind the first call's critical section — the mutation and the one
// event that follows it. A call cancelled in that window changes nothing,
// emits nothing and says so, and the tool answers the model with the aborted
// result.
//
// The first write is parked inside its emit, which runs with the store's lock
// held, so the second write is waiting for a lock it cannot have. Whichever
// way the two goroutines interleave once it is released, the answer is the
// same: the assertions do not depend on the parking, only on the cancel
// landing before the second write can mutate.
func TestSessionTodosCancelledBehindTheLock(t *testing.T) {
	b := newSessionTodos()
	writeTodos(t, b, nil, []tool.TodoUpdate{{ID: "1", Content: str("first")}})

	var once sync.Once
	held, release := make(chan struct{}), make(chan struct{})
	var events int
	var mu sync.Mutex
	detach := b.attach(func(Event) {
		mu.Lock()
		events++
		mu.Unlock()
		once.Do(func() { close(held) })
		<-release
	})
	defer detach()

	parked := make(chan struct{})
	go func() {
		defer close(parked)
		b.Write(context.Background(), nil, []tool.TodoUpdate{{ID: "2", Content: str("second")}})
	}()
	<-held // the lock is held, inside that write's emit

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	var list []tool.Todo
	var dropped int
	var ok bool
	go func() {
		defer close(done)
		list, dropped, ok = b.Write(ctx, nil, []tool.TodoUpdate{{ID: "3", Content: str("never")}})
	}()
	close(release)
	<-parked
	<-done

	if ok || list != nil || dropped != 0 {
		t.Fatalf("the cancelled write returned %+v, %d, %v; want nothing and false", list, dropped, ok)
	}
	// Nothing of it is in the list, and it published no event of its own.
	after, _ := writeTodos(t, b, nil, nil)
	equalTodos(t, after, []tool.Todo{
		todoItem("1", "first", tool.TodoPending),
		todoItem("2", "second", tool.TodoPending),
	})
	mu.Lock()
	defer mu.Unlock()
	// The sink was attached after the first item was seeded, so the events are
	// the parked write's and the empty one just above — not the cancelled
	// write's, which published nothing.
	if events != 2 {
		t.Fatalf("the sink saw %d events, want 2: the cancelled write emitted one", events)
	}
}

// TestSessionTodosNilEmitIsSafe: Write works with no turn attached (between
// turns, or a harness test that never attaches one) — the mutation still
// happens, only the event is skipped.
func TestSessionTodosNilEmitIsSafe(t *testing.T) {
	b := newSessionTodos()
	list, _ := writeTodos(t, b, nil, []tool.TodoUpdate{{ID: "1", Content: str("a")}})
	equalTodos(t, list, []tool.Todo{todoItem("1", "a", tool.TodoPending)})
}

// stepTodos is what a step's tool entry would carry now (turn.todosOn): ok is
// false when it would carry no list. written says the entry was then written
// (turn.todosWritten), or its append failed.
func stepTodos(b *sessionTodos, written bool) (list []tool.Todo, ok bool) {
	list, ok = b.unrecorded()
	if ok && written {
		b.recordedAt(list)
	}
	return list, ok
}

// TestSessionTodosRecordTheListLastWritten (plan 028 §3.2, P7): a tool entry
// carries the list when it differs from the one the last written entry
// carried — not when a call merely changed it: calls that change an item and
// change it back leave nothing to write. A list whose entry failed to append
// is carried to the next entry written (X13), and a change back to what is
// on disk after such a failure leaves nothing to write either. A restored
// list is on disk already.
func TestSessionTodosRecordTheListLastWritten(t *testing.T) {
	pending := []tool.Todo{todoItem("a", "alpha", tool.TodoPending)}
	done := []tool.Todo{todoItem("a", "alpha", tool.TodoCompleted)}
	set := func(b *sessionTodos, s tool.TodoStatus) {
		writeTodos(t, b, nil, []tool.TodoUpdate{{ID: "a", Content: str("alpha"), Status: status(s)}})
	}

	b := newSessionTodos()
	if _, ok := stepTodos(b, true); ok {
		t.Fatal("a list never written to carries something")
	}
	set(b, tool.TodoPending)
	if list, ok := stepTodos(b, true); !ok {
		t.Fatal("a new list carries nothing")
	} else {
		equalTodos(t, list, pending)
	}

	// A step whose calls change the item and change it back.
	set(b, tool.TodoCompleted)
	set(b, tool.TodoPending)
	if list, ok := stepTodos(b, true); ok {
		t.Fatalf("a step that left the list as written carries %+v", list)
	}

	// A change whose entry failed to append is carried to the next entry
	// written, from a step that changed nothing.
	set(b, tool.TodoCompleted)
	if _, ok := stepTodos(b, false); !ok {
		t.Fatal("a changed list carries nothing")
	}
	if list, ok := stepTodos(b, true); !ok {
		t.Fatal("a change whose append failed is not carried")
	} else {
		equalTodos(t, list, done)
	}
	if list, ok := stepTodos(b, true); ok {
		t.Fatalf("a list written already carries %+v again", list)
	}

	// A change whose append failed, then a step that changes it back to what
	// the last written entry holds: nothing to write.
	set(b, tool.TodoPending)
	if _, ok := stepTodos(b, false); !ok {
		t.Fatal("a changed list carries nothing")
	}
	set(b, tool.TodoCompleted)
	if list, ok := stepTodos(b, true); ok {
		t.Fatalf("a list back to the one written carries %+v", list)
	}

	// Emptied: carried, as an empty list.
	replace := false
	writeTodos(t, b, &replace, nil)
	if list, ok := stepTodos(b, true); !ok || len(list) != 0 {
		t.Fatalf("an emptied list carries %+v, %v; want an empty one", list, ok)
	}

	// A restored list is the one on disk.
	r := newSessionTodos()
	r.restore(pending)
	if list, ok := stepTodos(r, true); ok {
		t.Fatalf("a restored list carries %+v", list)
	}
	set(r, tool.TodoCompleted)
	set(r, tool.TodoPending)
	if list, ok := stepTodos(r, true); ok {
		t.Fatalf("a restored list changed and back carries %+v", list)
	}
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
			// Not writeTodos: this is another goroutine, where a t.Fatal is not
			// allowed.
			list, dropped, ok := b.Write(context.Background(), nil, []tool.TodoUpdate{{ID: id, Content: str(id)}})
			if dropped != 0 || !ok {
				t.Errorf("goroutine %d: dropped = %d, ok = %v; want 0, true", i, dropped, ok)
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

func (c echoTodoCall) Run(ctx context.Context, env tool.Env) tool.Result {
	if env.Todos == nil {
		return tool.Result{Text: "no store", IsError: true, Class: tool.ClassToolError}
	}
	content := string(c)
	list, _, ok := env.Todos.Write(ctx, nil, []tool.TodoUpdate{{ID: content, Content: &content}})
	if !ok {
		return tool.Result{Text: tool.AbortedText, IsError: true, Class: tool.ClassAborted}
	}
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
