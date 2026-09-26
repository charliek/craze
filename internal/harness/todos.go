package harness

import (
	"context"
	"slices"
	"strconv"
	"sync"
	"unicode/utf8"

	"github.com/charliek/craze/internal/harness/redact"
	"github.com/charliek/craze/internal/harness/store"
	"github.com/charliek/craze/internal/harness/tool"
)

// The harness owns the session's todo list (plan 023 §3.4): todo_write
// (internal/harness/tool/opencode/todo_write.go) only shapes and validates
// one call — the id, and the status against the four the store accepts —
// and hands it to sessionTodos.Write, which is where merge and replace, the
// auto-upgrade, the caps and the event that follows every write all live.
// The tool package may not import this one (deps_test.go), so the two meet
// only through tool.TodoStore and tool.TodoUpdate: pure data, going one way.
//
// # The lock, and why the event order is guaranteed
//
// sessionTodos has its own lock, a leaf like modes' and the steer box's
// (reminders.go, steer.go) — not Session.mu, which a running turn's
// callbacks never touch (Session's own doc says so), so taking it here would
// risk exactly the nesting those two avoid, for a store that has nothing to
// do with claiming or releasing a turn. It is also not the turn's own mu:
// Write is called from a tool's Run, which toolbridge.go's runTool already
// executes outside t.mu (so a slow write cannot hold up every other
// callback), and Write must still serialize against a concurrent Write on
// another tool goroutine — Parallel is true for todo_write, and two calls of
// a step run at once.
//
// So: b.mu is the outermost lock a write takes, and it wraps both the
// mutation and the one emit that follows it. attach hands Write the
// running turn's emitLocked — turn.mu taken and released inside that one
// call, which is what lets the emit obey the sink's single-caller contract
// (turn.go's Run doc) without this file needing to know anything about
// Fantasy or the sink itself. Because both the mutation and the emit happen
// under the same critical section, a second Write cannot begin — so cannot
// mutate, and cannot emit — until the first has done both: the sequence of
// list states Write produces and the sequence of events the sink receives
// are the same sequence, in the same order, however many tool goroutines
// call Write at once (plan 023 §3.4, "two parallel todo_write calls must
// not publish out of order").
//
// The nesting (b.mu, then t.mu, inside it) is one-directional: nothing
// elsewhere in the harness takes t.mu and then reaches for b.mu — turn.go's
// callbacks that already hold t.mu (OnTextDelta and the rest) never call
// into this file for anything but the record below, which has a lock of its
// own — so the two locks cannot deadlock on each other.
//
// # The record, and resume
//
// The transcript keeps the list (plan 028 §3.2, P7): a step that left it
// different writes the list as it stood after the step on its tool entry,
// redacted (redactTodos), and a resumed session restores the last one on its
// path (restore). The step's append runs under t.mu, where b.mu may not be
// taken, so a Write that changes the list also leaves a copy of it in rec —
// under recMu, a leaf lock that is held across nothing and taken under b.mu
// by Write and under t.mu by the step (unrecorded, recordedAt). A step
// attaches the list when it differs from the one the last written tool entry
// carried — not from the list at the step's start — so calls that change an
// item and change it back leave nothing to write, and a list whose append
// failed is carried to the next tool entry written (X13).
type sessionTodos struct {
	mu    sync.Mutex
	items []tool.Todo
	emit  func(Event) // the running turn's emitLocked; nil between turns

	recMu sync.Mutex
	rec   todoRecord
}

// todoRecord is what the transcript knows of the list: latest is the list as
// the last Write left it, and written is the list the last tool entry written
// carried, as the harness holds it (unredacted) — for a resumed session the
// one it restored, and empty before either.
type todoRecord struct {
	latest, written []tool.Todo
}

func newSessionTodos() *sessionTodos { return &sessionTodos{} }

// restore makes items the list, written: a resumed session's, from the last
// tool entry on its transcript's path that carries one (plan 028 §3.3). Open
// calls it before the session is handed out, so no Write or turn races it.
func (b *sessionTodos) restore(items []tool.Todo) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.items = cloneTodos(items)
	b.recMu.Lock()
	defer b.recMu.Unlock()
	b.rec = todoRecord{latest: cloneTodos(items), written: cloneTodos(items)}
}

// snapshot is a copy of the list as it is now.
func (b *sessionTodos) snapshot() []tool.Todo {
	b.mu.Lock()
	defer b.mu.Unlock()
	return cloneTodos(b.items)
}

// unrecorded is the list a step's tool entry should carry — the list as the
// last Write left it, a copy to hand recordedAt once that entry is written —
// with ok false when it is the list the last written entry carried, however
// many Writes changed it since. The caller may hold t.mu: recMu is a leaf.
func (b *sessionTodos) unrecorded() (list []tool.Todo, ok bool) {
	b.recMu.Lock()
	defer b.recMu.Unlock()
	if slices.Equal(b.rec.latest, b.rec.written) {
		return nil, false
	}
	return cloneTodos(b.rec.latest), true
}

// recordedAt says a written tool entry carries list (unrecorded's).
func (b *sessionTodos) recordedAt(list []tool.Todo) {
	b.recMu.Lock()
	defer b.recMu.Unlock()
	b.rec.written = cloneTodos(list)
}

// todoMark is the list a step's tool entry was handed (turn.todosOn), as the
// harness holds it, for turn.todosWritten to record once the entry is
// written; the zero mark is none. It is a type of this file's so that
// turn.go, which may not import the tool framework (TestSeamOne), can hold
// one.
type todoMark struct {
	list []tool.Todo
	set  bool
}

// redactTodos is items with every key red covers redacted from each item's id
// and content: the list as a tool entry stores it and a replay shows it (plan
// 028 §3.2, §3.4). The model wrote both, as it wrote the todo_write call's
// arguments, which are redacted in the same places.
//
// Every item is kept, so the stored list is as long as the live one and a
// resume restores every task the model had. Two distinct ids can redact alike
// — a key in each, at the same place — which would make a list the store
// refuses (a repeated id), so every repeat after the first is disambiguated
// (uniqueTodoIDs).
func redactTodos(red *redact.Replacer, items []tool.Todo) []tool.Todo {
	out := make([]tool.Todo, len(items))
	for i, it := range items {
		it.ID, it.Content = red.String(it.ID), red.String(it.Content)
		out[i] = it
	}
	uniqueTodoIDs(red, out)
	return out
}

// uniqueTodoIDs makes items' ids unique in place: the first item with an id
// keeps it, and each later one with the same id takes the first "<id>-<n>",
// n from 2, that no item of the list has — checked against the whole list,
// so a suffixed id never lands on one a later item holds — and that red
// leaves as it is, so a suffix can never complete a key. It is deterministic:
// the same list comes out the same every time, whoever redacts it.
func uniqueTodoIDs(red *redact.Replacer, items []tool.Todo) {
	taken := make(map[string]bool, len(items))
	for _, it := range items {
		taken[it.ID] = true
	}
	seen := make(map[string]bool, len(items))
	for i := range items {
		id := items[i].ID
		if !seen[id] {
			seen[id] = true
			continue
		}
		for n := 2; ; n++ {
			cand := id + "-" + strconv.Itoa(n)
			if !taken[cand] && red.String(cand) == cand {
				items[i].ID = cand
				taken[cand], seen[cand] = true, true
				break
			}
		}
	}
}

// storeTodos and toolTodos convert a list between the harness's shape and
// the transcript's (store.Todo), which carries the status as a plain string.
// storeTodos never returns nil, so an emptied list is written as [] — which a
// resume tells from "never set".
func storeTodos(items []tool.Todo) []store.Todo {
	out := make([]store.Todo, len(items))
	for i, it := range items {
		out[i] = store.Todo{ID: it.ID, Content: it.Content, Status: string(it.Status)}
	}
	return out
}

func toolTodos(items []store.Todo) []tool.Todo {
	out := make([]tool.Todo, len(items))
	for i, it := range items {
		out[i] = tool.Todo{ID: it.ID, Content: it.Content, Status: tool.TodoStatus(it.Status)}
	}
	return out
}

// attach makes emit the target of every Write while a turn runs, and returns
// the func that detaches it once the turn ends. turn.go's Run calls it right
// after building the turn and defers the release, the same way it hands the
// turn a fixed reference to the session's modes and steer boxes.
func (b *sessionTodos) attach(emit func(Event)) (release func()) {
	b.mu.Lock()
	b.emit = emit
	b.mu.Unlock()
	return func() {
		b.mu.Lock()
		b.emit = nil
		b.mu.Unlock()
	}
}

// Write is tool.TodoStore's one method (plan 023 §3.4). See the type's doc
// above for the locking and ordering guarantee.
//
// ctx is checked again here, with the lock held and before anything changes:
// todo_write is Parallel, so a second call of the same step can pass the
// tool's own check and then wait behind the first one's critical section, and
// a call cancelled in that window must leave the list and the sink alone. The
// wait itself is not made cancellable — that would need a lock this file
// cannot also hold across the emit, which is what orders the events — and it
// does not need to be: the only thing held across is the mutation and one call
// to the running turn's sink, and a sink that blocks has already stopped the
// whole turn (turn.go's single-caller contract), so nothing this cancel could
// do would free it.
func (b *sessionTodos) Write(ctx context.Context, merge *bool, updates []tool.TodoUpdate) (list []tool.Todo, dropped int, ok bool) {
	updates = dedupUpdates(updates)

	b.mu.Lock()
	defer b.mu.Unlock()
	if ctx.Err() != nil {
		return nil, 0, false
	}

	useMerge := merge == nil || *merge
	if !useMerge && autoUpgrade(b.items, updates) {
		useMerge = true
	}
	// Both builders return a new slice, so prev is the list before this write
	// whatever happens to b.items below.
	prev := b.items
	if useMerge {
		b.items = applyMerge(b.items, updates)
	} else {
		b.items = applyReplace(updates)
	}
	if len(b.items) > tool.TodoCap {
		dropped = len(b.items) - tool.TodoCap
		b.items = b.items[:tool.TodoCap]
	}
	if !slices.Equal(prev, b.items) {
		b.recMu.Lock()
		b.rec.latest = cloneTodos(b.items)
		b.recMu.Unlock()
	}

	list = cloneTodos(b.items)
	if b.emit != nil {
		b.emit(Todos{Items: cloneTodos(b.items)})
	}
	return list, dropped, true
}

// dedupUpdates keeps one entry per id from updates: the last occurrence's
// values, at the position the id first appeared — so a call that repeats an
// id neither reorders the result nor applies the stale, earlier value
// (plan 023 §3.4, "duplicate ids within one call: last wins").
func dedupUpdates(updates []tool.TodoUpdate) []tool.TodoUpdate {
	pos := make(map[string]int, len(updates))
	out := make([]tool.TodoUpdate, 0, len(updates))
	for _, u := range updates {
		if i, ok := pos[u.ID]; ok {
			out[i] = u
			continue
		}
		pos[u.ID] = len(out)
		out = append(out, u)
	}
	return out
}

// hasContent reports whether u carries a real content value: grok-build
// treats "" the same as omitted (todo/mod.rs's has_no_content and its own
// regression tests for a model that sends content: "" instead of leaving it
// out), so a merge does not wipe a row's content down to nothing on either
// spelling.
func hasContent(u tool.TodoUpdate) bool { return u.Content != nil && *u.Content != "" }

// autoUpgrade reports grok-build's regression fix (todo/mod.rs:334-348, D-53
// / plan 023 §3.4): an explicit merge:false is still treated as a merge when
// every update names an id already in items and carries no content — a
// status-only call the model forgot to flag as a merge, which a literal
// replace would otherwise wipe down to bare ids.
func autoUpgrade(items []tool.Todo, updates []tool.TodoUpdate) bool {
	if len(items) == 0 || len(updates) == 0 {
		return false
	}
	have := make(map[string]bool, len(items))
	for _, it := range items {
		have[it.ID] = true
	}
	for _, u := range updates {
		if hasContent(u) || !have[u.ID] {
			return false
		}
	}
	return true
}

// applyReplace rebuilds the list from updates alone (merge:false, and the
// auto-upgrade did not apply): grok-build's apply_replace (todo/mod.rs:
// 40-63). A missing or empty content falls back to the id; a missing status
// is pending.
func applyReplace(updates []tool.TodoUpdate) []tool.Todo {
	out := make([]tool.Todo, len(updates))
	for i, u := range updates {
		out[i] = tool.Todo{ID: u.ID, Content: contentOf(u), Status: statusOf(u)}
	}
	return out
}

// applyMerge patches items by id: grok-build's apply_merge (todo/mod.rs:
// 65-95). An id already in items keeps its content when the update carries
// none, and its status changes only when the update gives one; an id not yet
// present is created exactly as applyReplace would, appended after the ids
// already tracked, so their order never moves.
func applyMerge(items []tool.Todo, updates []tool.TodoUpdate) []tool.Todo {
	idx := make(map[string]int, len(items))
	out := make([]tool.Todo, len(items))
	copy(out, items)
	for i, it := range out {
		idx[it.ID] = i
	}
	for _, u := range updates {
		if i, ok := idx[u.ID]; ok {
			if hasContent(u) {
				out[i].Content = truncateContent(*u.Content)
			}
			if u.Status != nil {
				out[i].Status = *u.Status
			}
			continue
		}
		idx[u.ID] = len(out)
		out = append(out, tool.Todo{ID: u.ID, Content: contentOf(u), Status: statusOf(u)})
	}
	return out
}

// contentOf is the content a new item gets: the update's own, truncated, or
// the id itself when the update carries none (also truncated — an id could
// in principle be longer than the cap).
func contentOf(u tool.TodoUpdate) string {
	if hasContent(u) {
		return truncateContent(*u.Content)
	}
	return truncateContent(u.ID)
}

func statusOf(u tool.TodoUpdate) tool.TodoStatus {
	if u.Status != nil {
		return *u.Status
	}
	return tool.TodoPending
}

// truncateContent keeps s to at most tool.TodoContentBytes bytes, cut on a
// UTF-8 boundary with a trailing "…" — itself counted in the cap, so the
// result (the ellipsis included) never exceeds it and never ends mid-rune.
func truncateContent(s string) string {
	if len(s) <= tool.TodoContentBytes {
		return s
	}
	const ellipsis = "…" // 3 bytes
	cut := tool.TodoContentBytes - len(ellipsis)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + ellipsis
}

// cloneTodos is a copy of items sharing no backing array with it, so a later
// Write's append cannot alias what an earlier call returned or emitted —
// each event and each Write's own return value is that write's state,
// frozen.
func cloneTodos(items []tool.Todo) []tool.Todo {
	out := make([]tool.Todo, len(items))
	copy(out, items)
	return out
}
