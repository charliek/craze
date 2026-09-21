package tool

// TodoStatus is one of todo_write's four states (plan 023 §3.4).
type TodoStatus string

const (
	TodoPending    TodoStatus = "pending"
	TodoInProgress TodoStatus = "in_progress"
	TodoCompleted  TodoStatus = "completed"
	TodoCancelled  TodoStatus = "cancelled"
)

// Valid reports whether s is one of the four statuses todo_write's schema
// promises. Prepare uses it to turn anything else into invalid_input before
// the store ever sees it.
func (s TodoStatus) Valid() bool {
	switch s {
	case TodoPending, TodoInProgress, TodoCompleted, TodoCancelled:
		return true
	}
	return false
}

// Todo is one item of the session's todo list, as the harness keeps it and
// as todo_write's result and the harness.Event{Todos} that follows every
// write show it.
type Todo struct {
	ID      string
	Content string
	Status  TodoStatus
}

// TodoUpdate is one entry of a todo_write call, as Prepare has already
// validated it: a non-empty ID, and Content and Status left nil for a field
// the call omitted — which is what lets a merge flip a status without
// repeating the content back, and what the store's auto-upgrade (grok-build's
// todo/mod.rs:334-348) looks for.
type TodoUpdate struct {
	ID      string
	Content *string
	Status  *TodoStatus
}

// The caps todo_write's store enforces (plan 023 §3.4): craze's own addition,
// not grok-build's or opencode's, so both the store (internal/harness/
// todos.go) and the tool's result text (which names them) share these rather
// than each holding its own copy of "64" and "200".
const (
	// TodoCap is how many items the list holds; a write that would leave more
	// drops the overflow and says how many.
	TodoCap = 64
	// TodoContentBytes is how many bytes of a todo's content are kept,
	// truncated on a UTF-8 boundary with "…".
	TodoContentBytes = 200
)

// TodoStore is the harness's todo list (plan 023 §3.4): the seam that lets
// todo_write (internal/harness/tool/opencode) reach it without this package
// importing internal/harness, which owns the concrete type and publishes the
// event that follows every write.
//
// merge follows grok-build's tri-state: nil or a true value merges updates
// into the list by id, keeping what an update omits; false replaces the
// whole list — unless every update names an id already in the list and
// carries no content, which is grok-build's auto-upgrade to a merge even
// then (a status-only call the model forgot to flag as one). A duplicate id
// within updates is resolved before either applies: the last occurrence's
// values win, kept at the position the id first appeared. The result is
// capped at TodoCap items, each content at TodoContentBytes.
//
// Write never fails: an empty id or an unknown status is Prepare's job to
// catch, before the call reaches here, so a violation of either is the
// caller's bug, not something Write reports.
type TodoStore interface {
	Write(merge *bool, updates []TodoUpdate) (list []Todo, dropped int)
}
