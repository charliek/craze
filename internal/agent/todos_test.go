package agent

import (
	"encoding/json"
	"testing"
	"time"

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

func TestCursorIgnoresPlanUpdates(t *testing.T) {
	s := newSession(Options{})
	s.onUpdate(acp.SessionNotification{Update: mustJSON(map[string]any{
		"sessionUpdate": acp.UpdatePlan,
		"entries": []map[string]string{
			{"content": "Read", "status": "pending"},
		},
	})})
	select {
	case ev := <-s.Events():
		t.Fatalf("cursor must ignore plan updates, got %+v", ev)
	default:
	}
	if s.Snapshot().Todos != nil {
		t.Fatalf("cursor todos %s", todoStates(s.Snapshot().Todos))
	}
}

func TestPlanUpdateReplacesTodos(t *testing.T) {
	p := GrokProvider()
	s := newSession(Options{Provider: &p})
	go s.onUpdate(acp.SessionNotification{Update: mustJSON(map[string]any{
		"sessionUpdate": acp.UpdatePlan,
		"entries": []map[string]string{
			{"content": "Read", "status": "inProgress"},
			{"id": "keep", "content": "Edit", "status": "pending"},
			{"content": "   ", "status": "completed"},
			{"content": "Vet", "status": "canceled"},
		},
	})})
	ev := <-s.Events()
	if ev.Type != EventTodos {
		t.Fatalf("event %+v", ev)
	}
	got := s.Snapshot().Todos
	want := []Todo{
		{ID: "plan-0", Content: "Read", Status: "in_progress"},
		{ID: "keep", Content: "Edit", Status: "pending"},
		{ID: "plan-3", Content: "Vet", Status: "cancelled"},
	}
	if todoStates(got) != todoStates(want) {
		t.Fatalf("first %s", todoStates(got))
	}

	go s.onUpdate(acp.SessionNotification{Update: mustJSON(map[string]any{
		"sessionUpdate": acp.UpdatePlan,
		"entries": []map[string]string{
			{"content": "Vet", "status": "completed"},
			{"content": "Read", "status": "completed"},
		},
	})})
	<-s.Events()
	got = s.Snapshot().Todos
	want = []Todo{
		{ID: "plan-0", Content: "Vet", Status: "completed"},
		{ID: "plan-1", Content: "Read", Status: "completed"},
	}
	if todoStates(got) != todoStates(want) {
		t.Fatalf("reorder %s", todoStates(got))
	}

	go s.onUpdate(acp.SessionNotification{Update: mustJSON(map[string]any{
		"sessionUpdate": acp.UpdatePlan,
		"entries":       []map[string]string{},
	})})
	<-s.Events()
	if s.Snapshot().Todos != nil {
		t.Fatalf("empty must clear, got %s", todoStates(s.Snapshot().Todos))
	}

	go s.onUpdate(acp.SessionNotification{Update: mustJSON(map[string]any{
		"sessionUpdate": acp.UpdatePlan,
		"entries": []map[string]string{
			{"content": "Again", "status": "pending"},
		},
	})})
	<-s.Events()
	if got := s.Snapshot().Todos; len(got) != 1 || got[0].ID != "plan-0" {
		t.Fatalf("repeat %s", todoStates(got))
	}

	s.onUpdate(acp.SessionNotification{Update: mustJSON(map[string]any{
		"sessionUpdate": acp.UpdatePlan,
		"entries":       "not-an-array",
	})})
	select {
	case ev := <-s.Events():
		t.Fatalf("malformed plan must not emit, got %+v", ev)
	default:
	}
	if got := s.Snapshot().Todos; len(got) != 1 || got[0].ID != "plan-0" {
		t.Fatalf("malformed plan must leave todos, got %s", todoStates(got))
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

// TestCancelledTurnClosesInFlightTools pins what grok leaves behind after a
// cancel: the interrupted tool never gets a terminal update from the agent,
// so the session settles it itself when the turn ends cancelled.
func TestCancelledTurnClosesInFlightTools(t *testing.T) {
	p := GrokProvider()
	s := newSession(Options{Provider: &p})
	running, done := "in_progress", "completed"
	s.mergeTool(toolDelta{id: "t-run", status: &running})
	s.mergeTool(toolDelta{id: "t-done", status: &done})
	s.mergeTool(toolDelta{id: "t-new"})
	go s.closeInFlightTools(acp.StopCancelled)
	select {
	case ev := <-s.Events():
		if ev.Type != EventTool || ev.Tool == nil || ev.Tool.ID != "t-run" || ev.Tool.Status != "cancelled" {
			t.Fatalf("unexpected event %+v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("running tool was not settled")
	}
	select {
	case ev := <-s.Events():
		t.Fatalf("only the running tool is settled: %+v", ev)
	case <-time.After(50 * time.Millisecond):
	}
	for _, tool := range s.Snapshot().Tools {
		if tool.ID == "t-done" && tool.Status != "completed" {
			t.Fatalf("completed tool touched: %+v", tool)
		}
		if tool.ID == "t-new" && tool.Status != "" {
			t.Fatalf("a tool that never reported in flight is left alone: %+v", tool)
		}
	}
}
