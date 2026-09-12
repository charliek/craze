package agent

import (
	"encoding/json"
	"testing"

	"github.com/charliek/craze/internal/acp"
)

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func todoStates(in []Todo) string {
	b, _ := json.Marshal(in)
	return string(b)
}

func TestNormalizeTodoStatus(t *testing.T) {
	cases := map[string]string{
		"pending":                 "pending",
		"in_progress":             "in_progress",
		"inProgress":              "in_progress",
		"in-progress":             "in_progress",
		"TODO_STATUS_IN_PROGRESS": "in_progress",
		"TODO_STATUS_PENDING":     "pending",
		"TODO_STATUS_COMPLETED":   "completed",
		"TODO_STATUS_CANCELLED":   "cancelled",
		"canceled":                "cancelled",
		"":                        "pending",
		"who knows":               "pending",
	}
	for in, want := range cases {
		if got := normalizeTodoStatus(in); got != want {
			t.Fatalf("normalizeTodoStatus(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMergeTodosReplace(t *testing.T) {
	prev := []Todo{{ID: "9", Content: "old", Status: "completed"}}
	in := []Todo{
		{ID: "1", Content: "a", Status: "in_progress"},
		{ID: "2", Content: "b", Status: "pending"},
	}
	got := mergeTodos(prev, in, false)
	if todoStates(got) != todoStates(in) {
		t.Fatalf("replace kept old entries: %s", todoStates(got))
	}
}

func TestMergeTodosUpsertKeepsOrderAndAppends(t *testing.T) {
	prev := []Todo{
		{ID: "1", Content: "a", Status: "in_progress"},
		{ID: "2", Content: "b", Status: "pending"},
		{ID: "3", Content: "c", Status: "pending"},
	}
	in := []Todo{
		{ID: "3", Content: "c", Status: "cancelled"},
		{ID: "1", Content: "a", Status: "completed"},
		{ID: "4", Content: "d", Status: "pending"},
	}
	got := mergeTodos(prev, in, true)
	want := []Todo{
		{ID: "1", Content: "a", Status: "completed"},
		{ID: "2", Content: "b", Status: "pending"},
		{ID: "3", Content: "c", Status: "cancelled"},
		{ID: "4", Content: "d", Status: "pending"},
	}
	if todoStates(got) != todoStates(want) {
		t.Fatalf("merge = %s", todoStates(got))
	}
}

func TestMergeTodosDuplicateIDsLastWins(t *testing.T) {
	in := []Todo{
		{ID: "1", Content: "first", Status: "pending"},
		{ID: "2", Content: "b", Status: "pending"},
		{ID: "1", Content: "second", Status: "completed"},
	}
	got := mergeTodos(nil, in, false)
	want := []Todo{
		{ID: "1", Content: "second", Status: "completed"},
		{ID: "2", Content: "b", Status: "pending"},
	}
	if todoStates(got) != todoStates(want) {
		t.Fatalf("replace dedupe = %s", todoStates(got))
	}
	got = mergeTodos([]Todo{{ID: "1", Content: "first", Status: "pending"}}, in, true)
	if todoStates(got) != todoStates(want) {
		t.Fatalf("merge dedupe = %s", todoStates(got))
	}
}

func TestOnUpdateTodosNormalisesAndEchoes(t *testing.T) {
	s := newSession(Options{})
	echo := s.onUpdateTodos(acp.UpdateTodosRequest{
		Todos: []acp.TodoItem{
			{ID: "1", Content: "\x1b[31mRead main.go", Status: "TODO_STATUS_IN_PROGRESS"},
			{ID: "2", Content: "Edit", Status: "pending"},
		},
	})
	if len(echo) != 2 || echo[0].Status != "in_progress" || echo[0].Content != "Read main.go" {
		t.Fatalf("echo %+v", echo)
	}
	snap := s.Snapshot()
	if len(snap.Todos) != 2 || snap.Todos[0].Status != "in_progress" {
		t.Fatalf("snapshot %+v", snap.Todos)
	}
	if snap.TodosUpdatedAt.IsZero() {
		t.Fatal("TodosUpdatedAt not stamped")
	}

	echo = s.onUpdateTodos(acp.UpdateTodosRequest{
		Merge: true,
		Todos: []acp.TodoItem{{ID: "1", Content: "Read main.go", Status: "completed"}},
	})
	if len(echo) != 2 || echo[0].Status != "completed" || echo[1].Status != "pending" {
		t.Fatalf("merged echo %+v", echo)
	}
}

func TestTodosEventEmitted(t *testing.T) {
	s := newSession(Options{})
	go s.onUpdateTodos(acp.UpdateTodosRequest{Todos: []acp.TodoItem{{ID: "1", Content: "a", Status: "pending"}}})
	ev := <-s.Events()
	if ev.Type != EventTodos || len(ev.Todos) != 1 || ev.Todos[0].ID != "1" {
		t.Fatalf("event %+v", ev)
	}
	if ev.At.IsZero() {
		t.Fatal("Event.At not stamped")
	}
}
