package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/charliek/craze/internal/harness/tool"
)

// newTodoWrite builds todo_write (plan 023 §3.4). Its ids and description
// are grok-build's — the reference for the three tools D-53 ports, ask mode
// and the plan tool among them — never opencode's, whose own todowrite
// carries no id field at all in either direction: craze's schema and result
// both do (below), since the model needs it back to update the right row.
//
// It is built here, and tested directly, but C3 does not add it to Profile's
// builders: C4 registers all three of D-53's tools in the one commit that
// moves specs.golden and the pytest tool list.
func newTodoWrite() (tool.Tool, error) {
	desc, err := description("todo_write", nil)
	if err != nil {
		return nil, err
	}
	return &todoWriteTool{spec: tool.Spec{
		ID:          "todo_write",
		Description: desc,
		Parameters: map[string]any{
			"merge": map[string]any{
				"type": "boolean",
				"description": "Optional. When true (default), merges the provided todos into the existing list by id " +
					"— send only the items you are changing, and to flip status without changing content send just id and " +
					"status. When false, the provided todos replace the existing list.",
			},
			"todos": map[string]any{
				"type":        "array",
				"description": "The todo items to write.",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"id":      map[string]any{"type": "string", "description": "Unique identifier for the todo item"},
						"content": map[string]any{"type": "string", "description": "The description of the todo item"},
						"status": map[string]any{"type": "string",
							"enum":        []any{string(tool.TodoPending), string(tool.TodoInProgress), string(tool.TodoCompleted), string(tool.TodoCancelled)},
							"description": "The status of the todo item: pending, in_progress, completed, or cancelled"},
					},
					"required": []any{"id"},
				},
			},
		},
		Required: []string{"todos"},
		Kind:     tool.KindTodo,
		ReadOnly: true,
		Parallel: true,
		// The store's own caps (tool.TodoCap items, tool.TodoContentBytes a
		// content) keep the result far under Head's limits, so which
		// direction is kept never actually matters; Head is chosen anyway,
		// as the useful end of anything that somehow did grow past them.
		Truncate: tool.Head,
	}}, nil
}

// What one call may be, before any of it is decoded (plan 023 §3.4).
// The store's caps bound the list a call leaves behind, not the work of
// getting there: without these, a request of a million duplicate
// {"id":"x"} objects would be decoded into a million raw messages, copied into
// a million updates and indexed by a map sized for them, all to leave one item
// in the list — and none of that work is interruptible.
const (
	// maxTodoInputBytes is the largest raw argument object todo_write reads.
	// It is checked first, against the bytes Fantasy handed over, so nothing
	// larger is ever decoded. Well past any real call: the whole list at its
	// caps is about 64 × (64 + 200) bytes of ids and contents.
	maxTodoInputBytes = 256 << 10
	// maxTodoUpdates is how many entries the todos array may have. Four times
	// TodoCap, so a call that rewrites the whole list, even one that repeats
	// every id, is comfortably inside it.
	maxTodoUpdates = 4 * tool.TodoCap
)

type todoWriteTool struct{ spec tool.Spec }

func (w *todoWriteTool) Spec() tool.Spec { return w.spec }

// Prepare shapes and validates one call: its size, before anything is decoded
// (maxTodoInputBytes, maxTodoUpdates); the merge flag, tri-state so the store
// can tell "omitted" from "explicitly false" (its auto-upgrade needs the
// difference); and each update's id (required, non-empty, at most
// tool.TodoIDBytes) and status (one of the four, when given). Content is taken
// as sent, unexamined — the store is where a value is finally kept, truncated
// or replaced by the id, since only it knows whether an id is new or already
// in the list. Duplicate ids within the call are not rejected here either: the
// store resolves them (last one wins), which is a semantics only it can apply
// consistently with merge and replace both.
func (w *todoWriteTool) Prepare(_ tool.Env, c tool.Call) (tool.Prepared, error) {
	if len(c.Input) > maxTodoInputBytes {
		return nil, fmt.Errorf("the arguments are %d bytes, larger than the %d this tool reads; send fewer todo items", len(c.Input), maxTodoInputBytes)
	}
	a, err := parseArgs(c.Input)
	if err != nil {
		return nil, err
	}
	merge, hasMerge, err := a.boolean("merge")
	if err != nil {
		return nil, err
	}
	var mergePtr *bool
	if hasMerge {
		mergePtr = &merge
	}
	raw, _, err := a.array("todos", true)
	if err != nil {
		return nil, err
	}
	if len(raw) > maxTodoUpdates {
		return nil, fmt.Errorf("todos has %d items, more than the %d this tool takes in one call; the list itself holds %d", len(raw), maxTodoUpdates, tool.TodoCap)
	}
	updates := make([]tool.TodoUpdate, len(raw))
	for i, r := range raw {
		u, err := parseTodoUpdate(r)
		if err != nil {
			return nil, fmt.Errorf("todos[%d]: %w", i, err)
		}
		updates[i] = u
	}
	return &todoWriteCall{merge: mergePtr, updates: updates}, nil
}

// parseTodoUpdate parses one element of the todos array.
func parseTodoUpdate(raw json.RawMessage) (tool.TodoUpdate, error) {
	if t := jsonType(raw); t != "object" {
		return tool.TodoUpdate{}, fmt.Errorf("must be an object, got a JSON %s", t)
	}
	a, err := parseArgs(raw)
	if err != nil {
		return tool.TodoUpdate{}, err
	}
	id, _, err := a.str("id", true)
	if err != nil {
		return tool.TodoUpdate{}, err
	}
	if strings.TrimSpace(id) == "" {
		return tool.TodoUpdate{}, fmt.Errorf("id must not be empty")
	}
	// Refused rather than cut: an id is what a later call names a row by, and
	// two ids truncated to the same prefix would be one row (tool.TodoIDBytes).
	if len(id) > tool.TodoIDBytes {
		return tool.TodoUpdate{}, fmt.Errorf("id is %d bytes, longer than the %d an id may be", len(id), tool.TodoIDBytes)
	}
	u := tool.TodoUpdate{ID: id}
	// content and status take grok-build's Option semantics: an explicit null
	// is the field left out, not a wrong type (strOrNull).
	if content, ok, err := a.strOrNull("content"); err != nil {
		return tool.TodoUpdate{}, err
	} else if ok {
		u.Content = &content
	}
	if s, ok, err := a.strOrNull("status"); err != nil {
		return tool.TodoUpdate{}, err
	} else if ok {
		st := tool.TodoStatus(s)
		if !st.Valid() {
			return tool.TodoUpdate{}, fmt.Errorf("status must be one of pending, in_progress, completed, cancelled, not %q", s)
		}
		u.Status = &st
	}
	return u, nil
}

// todoWriteCall is a prepared todo_write call: nothing more can fail once
// this is built, so Run only asks the store to apply it.
type todoWriteCall struct {
	merge   *bool
	updates []tool.TodoUpdate
}

func (c *todoWriteCall) Request() tool.Request {
	return tool.Request{Title: todoTitle(c.updates)}
}

// todoTitle is the call's card line: opencode's own count of what the call
// itself is not marking completed (todo.ts:37), not the resulting list's
// full size, which Prepare cannot know before Run reaches the store.
func todoTitle(updates []tool.TodoUpdate) string {
	n := 0
	for _, u := range updates {
		if u.Status == nil || *u.Status != tool.TodoCompleted {
			n++
		}
	}
	if n == 1 {
		return "1 todo"
	}
	return fmt.Sprintf("%d todos", n)
}

// Run hands the call to the harness's store (plan 023 §3.4). A nil
// Env.Todos — a harness test that wired none, or a build that never will —
// is a clear tool_error result, never a panic: the seam is the one place
// this package depends on something outside it that may not be there.
func (c *todoWriteCall) Run(ctx context.Context, env tool.Env) tool.Result {
	if ctx.Err() != nil {
		return aborted()
	}
	if env.Todos == nil {
		return tool.Result{Text: "The todo list is not available in this session.", IsError: true, Class: tool.ClassToolError}
	}
	// The store checks ctx once more, with its own lock held: two calls of a
	// step run at once, and the one that waited must not change a list the
	// user has already stopped (tool.TodoStore).
	list, dropped, ok := env.Todos.Write(ctx, c.merge, c.updates)
	if !ok {
		return aborted()
	}
	return tool.Result{Text: todoResultText(list, dropped)}
}

// aborted is what every tool answers a cancelled call with.
func aborted() tool.Result {
	return tool.Result{Text: tool.AbortedText, IsError: true, Class: tool.ClassAborted}
}

// wireTodo is one item of the result's JSON, camelCase like every other
// field craze's tools send.
type wireTodo struct {
	ID      string `json:"id"`
	Content string `json:"content"`
	Status  string `json:"status"`
}

// todoResultText is opencode's own result shape (todo.ts:38): the list,
// pretty-printed with json.MarshalIndent the way JSON.stringify(_, null, 2)
// indents it — an empty list is "[]", not a "no tasks" sentence, since
// opencode's own tool sends none either. craze's addition is the id field:
// neither opencode's Todo.Info nor grok-build's TodoItem carries one back to
// the model at all (only the request's own key), which would leave the
// model with nothing but content to recognise a row by; plan 023 §3.4 asks
// for it here. dropped, when the store's cap cut the list, is said in a note
// after the JSON.
func todoResultText(list []tool.Todo, dropped int) string {
	wire := make([]wireTodo, len(list))
	for i, it := range list {
		wire[i] = wireTodo{ID: it.ID, Content: it.Content, Status: string(it.Status)}
	}
	b, err := json.MarshalIndent(wire, "", "  ")
	text := string(b)
	if err != nil { // wireTodo always marshals; kept for the same reason dispatch.go keeps its own such checks
		text = "[]"
	}
	if dropped > 0 {
		plural := "s"
		if dropped == 1 {
			plural = ""
		}
		text += fmt.Sprintf("\n\n(%d todo item%s dropped: the list is capped at %d items.)", dropped, plural, tool.TodoCap)
	}
	return text
}
