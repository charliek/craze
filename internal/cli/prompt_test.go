package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
)

func TestRootRefusesNonTTY(t *testing.T) {
	var stdout bytes.Buffer
	cmd := NewRootCmd()
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{})
	err := cmd.Execute()
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 2 {
		t.Fatalf("got %v", err)
	}
	if !strings.Contains(ee.msg, "refusing to start TUI on a non-tty") {
		t.Fatalf("msg %q", ee.msg)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout %q", stdout.String())
	}
}

func TestPromptUsage(t *testing.T) {
	t.Run("no text empty stdin", func(t *testing.T) {
		cmd := NewRootCmd()
		cmd.SetIn(&bytes.Buffer{})
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs([]string{"prompt"})
		err := cmd.Execute()
		var ee *exitError
		if !errors.As(err, &ee) || ee.code != 2 {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("ask and plan", func(t *testing.T) {
		cmd := NewRootCmd()
		cmd.SetIn(&bytes.Buffer{})
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs([]string{"prompt", "--ask", "--plan", "hi"})
		err := cmd.Execute()
		var ee *exitError
		if !errors.As(err, &ee) || ee.code != 2 {
			t.Fatalf("got %v", err)
		}
		if !strings.Contains(ee.msg, "mutually exclusive") {
			t.Fatalf("msg %q", ee.msg)
		}
	})

	t.Run("bad permission decision", func(t *testing.T) {
		cmd := NewRootCmd()
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs([]string{"prompt", "--permission-decision", "maybe", "hi"})
		err := cmd.Execute()
		var ee *exitError
		if !errors.As(err, &ee) || ee.code != 2 {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("workspace missing", func(t *testing.T) {
		cmd := NewRootCmd()
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs([]string{"prompt", "--workspace", t.TempDir() + "/no-such-dir", "hi"})
		err := cmd.Execute()
		var ee *exitError
		if !errors.As(err, &ee) || ee.code != 2 {
			t.Fatalf("got %v", err)
		}
	})
}

func TestDecisionKind(t *testing.T) {
	kind, err := decisionKind("allow-once")
	if err != nil || kind != "allow_once" {
		t.Fatalf("%q %v", kind, err)
	}
	kind, err = decisionKind("reject_once")
	if err != nil || kind != "reject_once" {
		t.Fatalf("%q %v", kind, err)
	}
}

func asJSONEvent(t *testing.T, v any) jsonEvent {
	t.Helper()
	j, ok := v.(jsonEvent)
	if !ok {
		t.Fatalf("want jsonEvent, got %T", v)
	}
	return j
}

func TestEventJSON(t *testing.T) {
	v, ok := eventJSON(agent.Event{Type: agent.EventThought, Text: "thinking"})
	j := asJSONEvent(t, v)
	if !ok || j.Type != "thought" || j.Text != "thinking" {
		t.Fatalf("thought json %+v %v", j, ok)
	}
	v, ok = eventJSON(agent.Event{Type: agent.EventText, Text: "hi"})
	j = asJSONEvent(t, v)
	if !ok || j.Type != "text" || j.Text != "hi" {
		t.Fatalf("%+v %v", j, ok)
	}
	v, ok = eventJSON(agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{
		ID: "call-1", Name: "Shell", Status: "pending", Kind: "execute", Title: "Shell", RawInput: "echo hi",
	}})
	j = asJSONEvent(t, v)
	if !ok || j.Kind != "execute" || j.Title != "Shell" || j.Name != "Shell" {
		t.Fatalf("tool json %+v %v", j, ok)
	}
	b, err := json.Marshal(j)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["rawInput"]; ok {
		t.Fatalf("must not dump rawInput: %s", b)
	}
	v, _ = eventJSON(agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: "c1", Name: "n", Status: "s"}})
	j = asJSONEvent(t, v)
	b, err = json.Marshal(j)
	if err != nil {
		t.Fatal(err)
	}
	m = map[string]any{}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["kind"]; ok {
		t.Fatalf("empty kind should omit: %s", b)
	}
	if _, ok := m["title"]; ok {
		t.Fatalf("empty title should omit: %s", b)
	}

	v, ok = eventJSON(agent.Event{Type: agent.EventUser, Agent: "sub-1", Text: "prompt"})
	j = asJSONEvent(t, v)
	if !ok || j.Type != "user" || j.Agent != "sub-1" || j.Text != "prompt" {
		t.Fatalf("user json %+v %v", j, ok)
	}
	v, ok = eventJSON(agent.Event{
		Type:           agent.EventSubagent,
		SubagentChange: agent.SubagentChangeSpawned,
		Subagent:       &agent.SubagentInfo{ID: "sub-1", Status: agent.SubagentRunning, Transcript: true},
	})
	if !ok {
		t.Fatal("eventJSON must encode EventSubagent")
	}
	b, err = json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"type":"subagent"`) || !strings.Contains(string(b), `"event":"spawned"`) {
		t.Fatalf("subagent json %s", b)
	}
}

func TestDrainSkippedWhenNothingSpawned(t *testing.T) {
	evs := make(chan agent.Event)
	start := time.Now()
	if err := drainSubagentEvents(evs, func() agent.Snapshot { return agent.Snapshot{} }, func(agent.Event) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 50*time.Millisecond {
		t.Fatalf("drain added latency %s", time.Since(start))
	}
}

func TestDrainWritesLateFinished(t *testing.T) {
	evs := make(chan agent.Event, 4)
	var mu sync.Mutex
	info := agent.SubagentInfo{ID: "sub-1", Status: agent.SubagentRunning}
	snap := agent.Snapshot{Subagents: []agent.SubagentInfo{info}}
	go func() {
		time.Sleep(400 * time.Millisecond)
		fin := info
		fin.Status = agent.SubagentCancelled
		mu.Lock()
		snap.Subagents[0] = fin
		mu.Unlock()
		evs <- agent.Event{Type: agent.EventSubagent, SubagentChange: agent.SubagentChangeFinished, Subagent: &fin}
	}()
	var got []agent.Event
	start := time.Now()
	if err := drainSubagentEvents(evs, func() agent.Snapshot {
		mu.Lock()
		defer mu.Unlock()
		out := snap
		out.Subagents = append([]agent.SubagentInfo(nil), snap.Subagents...)
		return out
	}, func(ev agent.Event) error {
		got = append(got, ev)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) < 350*time.Millisecond {
		t.Fatalf("drain returned too fast to be state-aware: %s", time.Since(start))
	}
	if len(got) != 1 || got[0].SubagentChange != agent.SubagentChangeFinished {
		t.Fatalf("got %+v", got)
	}
}

// TestDrainPropagatesWriteErrors pins the contract run()'s deferred drain
// relies on: a write that fails during the drain is an error, so a headless
// run can never exit 0 with incomplete JSON.
func TestDrainPropagatesWriteErrors(t *testing.T) {
	evs := make(chan agent.Event, 1)
	snap := agent.Snapshot{Subagents: []agent.SubagentInfo{{ID: "sub-1", Status: agent.SubagentCompleted}}}
	evs <- agent.Event{Type: agent.EventSubagent, SubagentChange: agent.SubagentChangeFinished, Subagent: &snap.Subagents[0]}
	close(evs)
	want := errors.New("pipe broke")
	err := drainSubagentEvents(evs, func() agent.Snapshot { return snap }, func(agent.Event) error {
		return want
	})
	if !errors.Is(err, want) {
		t.Fatalf("drain error %v, want %v", err, want)
	}
}

func TestPickPermissionKeepsUnusedDecisions(t *testing.T) {
	opts := []agent.PermissionOption{
		{OptionID: "opt-reject", Kind: "reject_once"},
	}
	id, rejected, rest, err := pickPermission(opts, []string{"allow-once"})
	if err != nil || !rejected || id != "opt-reject" {
		t.Fatalf("id=%q rejected=%v err=%v", id, rejected, err)
	}
	if len(rest) != 1 || rest[0] != "allow-once" {
		t.Fatalf("queue should keep unused allow-once, got %v", rest)
	}
}
