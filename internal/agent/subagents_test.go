package agent

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charliek/craze/internal/acp"
)

func TestGrokSubagentScriptSequence(t *testing.T) {
	s := startGrokScript(t, "grok-subagent", true)
	log := collect(t, s)
	if _, err := s.Prompt(t.Context(), "go"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "finished", func() bool { return hasSubagentChange(log, SubagentChangeFinished, "sub-1") })
	evs := log.snapshot()
	assertSpawnedThenStream(t, evs, "sub-1")
	if got := lastSubagent(t, evs, "sub-1"); got.Status != SubagentCompleted || got.Output == "" {
		t.Fatalf("finished %+v", got)
	}
}

func TestGrokSubagentFailMapsError(t *testing.T) {
	s := startGrokScript(t, "grok-subagent-fail", true)
	log := collect(t, s)
	if _, err := s.Prompt(t.Context(), "go"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "finished", func() bool { return hasSubagentChange(log, SubagentChangeFinished, "sub-1") })
	got := lastSubagent(t, log.snapshot(), "sub-1")
	if got.Status != SubagentFailed || got.Error != "boom" {
		t.Fatalf("fail %+v", got)
	}
}

func TestGrokSubagentTwoKeepsIdsApart(t *testing.T) {
	s := startGrokScript(t, "grok-subagent-two", true)
	log := collect(t, s)
	if _, err := s.Prompt(t.Context(), "go"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "both finished", func() bool {
		return hasSubagentChange(log, SubagentChangeFinished, "sub-1") &&
			hasSubagentChange(log, SubagentChangeFinished, "sub-2")
	})
	evs := log.snapshot()
	var spawned int
	for _, ev := range evs {
		if ev.Type == EventSubagent && ev.Subagent != nil && ev.Subagent.ID == "sub-1" && ev.SubagentChange == SubagentChangeSpawned {
			spawned++
		}
		if ev.Agent == "sub-9" || (ev.Subagent != nil && ev.Subagent.ID == "sub-9") {
			t.Fatal("sub-9 must never surface")
		}
	}
	if spawned != 1 {
		t.Fatalf("duplicate spawned must be a no-op, got %d", spawned)
	}
	var finishOrder []string
	tools := map[string]string{}
	for _, ev := range evs {
		if ev.Type == EventSubagent && ev.SubagentChange == SubagentChangeFinished && ev.Subagent != nil {
			finishOrder = append(finishOrder, ev.Subagent.ID)
		}
		if ev.Type == EventTool && ev.Agent != "" && ev.Tool != nil && ev.Tool.ID == "call-1" {
			if prev, ok := tools[ev.Agent]; ok && prev != ev.Tool.ToolName && ev.Tool.ToolName != "" {
				t.Fatalf("colliding tool id mixed %s %q vs %q", ev.Agent, prev, ev.Tool.ToolName)
			}
			if ev.Tool.ToolName != "" {
				tools[ev.Agent] = ev.Tool.ToolName
			}
		}
	}
	if len(finishOrder) < 2 || finishOrder[0] != "sub-2" {
		t.Fatalf("sub-2 should finish first, got %v", finishOrder)
	}
	if tools["sub-1"] == tools["sub-2"] {
		t.Fatalf("colliding call-1 must stay per-child: %v", tools)
	}
	snap := s.Snapshot()
	if len(snap.Subagents) != 2 || snap.Subagents[0].ID != "sub-1" || snap.Subagents[1].ID != "sub-2" {
		t.Fatalf("spawn order %+v", snap.Subagents)
	}
}

func TestGrokSubagentNestedJoin(t *testing.T) {
	s := startGrokScript(t, "grok-subagent-nested", true)
	log := collect(t, s)
	if _, err := s.Prompt(t.Context(), "go"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "both finished", func() bool {
		return hasSubagentChange(log, SubagentChangeFinished, "sub-1") &&
			hasSubagentChange(log, SubagentChangeFinished, "sub-1a")
	})
	found := false
	for _, ev := range log.snapshot() {
		if ev.Type == EventTool && ev.Agent == "sub-1" && ev.Tool != nil && ev.Tool.ID == "call-nested-spawn" {
			if ev.Tool.Task != nil && ev.Tool.Task.AgentID == "sub-1a" {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("grandchild join must land in sub-1's tool map")
	}
	ids := map[string]bool{}
	for _, a := range s.Snapshot().Subagents {
		ids[a.ID] = true
	}
	if !ids["sub-1"] || !ids["sub-1a"] {
		t.Fatalf("records %+v", s.Snapshot().Subagents)
	}
	if got := lastSubagent(t, log.snapshot(), "sub-1a"); got.ParentID != "sub-1" {
		t.Fatalf("parent %q", got.ParentID)
	}
}

func TestGrokSubagentLateAfterDone(t *testing.T) {
	s := startGrokScript(t, "grok-subagent-late", true)
	log := collect(t, s)
	if _, err := s.Prompt(t.Context(), "go"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "finished after done", func() bool { return hasSubagentChange(log, SubagentChangeFinished, "sub-1") })
	evs := log.snapshot()
	doneAt, lateText, finishedAt := -1, -1, -1
	for i, ev := range evs {
		if ev.Type == EventDone && doneAt < 0 {
			doneAt = i
		}
		if ev.Type == EventText && ev.Agent == "sub-1" && strings.Contains(ev.Text, "late") {
			lateText = i
		}
		if ev.Type == EventSubagent && ev.SubagentChange == SubagentChangeFinished {
			finishedAt = i
		}
	}
	if doneAt < 0 || finishedAt < 0 {
		t.Fatalf("missing done/finished in %#v", typesOf(evs))
	}
	if doneAt >= finishedAt {
		t.Fatalf("parent done should precede finished: done=%d finished=%d", doneAt, finishedAt)
	}
	if lateText >= 0 && lateText < doneAt {
		t.Fatalf("late child text before done")
	}
}

func TestGrokSubagentCancelOrders(t *testing.T) {
	for _, script := range []string{"grok-subagent-cancel", "grok-subagent-cancel-early"} {
		t.Run(script, func(t *testing.T) {
			s := startGrokScript(t, script, true)
			log := collect(t, s)
			errCh := make(chan error, 1)
			go func() {
				_, err := s.Prompt(t.Context(), "go")
				errCh <- err
			}()
			waitFor(t, "spawned", func() bool { return hasSubagentChange(log, SubagentChangeSpawned, "sub-1") })
			if err := s.Cancel(t.Context()); err != nil {
				t.Fatal(err)
			}
			if err := <-errCh; err != nil {
				t.Fatal(err)
			}
			waitFor(t, "finished", func() bool { return hasSubagentChange(log, SubagentChangeFinished, "sub-1") })
			n := 0
			settled := false
			for _, ev := range log.snapshot() {
				if ev.Type == EventSubagent && ev.SubagentChange == SubagentChangeFinished {
					n++
					if ev.Subagent == nil || ev.Subagent.Status != SubagentCancelled {
						t.Fatalf("status %+v", ev.Subagent)
					}
				}
				if ev.Type == EventTool && ev.Agent == "sub-1" && ev.Tool != nil && ev.Tool.ID == "call-1" && ev.Tool.Status == "cancelled" {
					settled = true
				}
			}
			if n != 1 {
				t.Fatalf("exactly one finished, got %d", n)
			}
			if !settled {
				t.Fatal("child tool must be settled cancelled with EventTool{Agent}")
			}
		})
	}
}

func TestJoinDescriptionThenRawOutputOverride(t *testing.T) {
	grok := GrokProvider()
	s := newSession(Options{Provider: &grok})
	s.sessionID = "main"
	raw := mustJSON(map[string]any{
		"description": "Same", "prompt": "p", "subagent_type": "explore", "background": true,
	})
	title := "spawn_subagent"
	s.mergeTool(toolDelta{id: "call-a", title: &title, wireName: "spawn_subagent", rawInput: raw, hasRawInput: true})
	s.mergeTool(toolDelta{id: "call-b", title: &title, wireName: "spawn_subagent", rawInput: raw, hasRawInput: true})
	drainEvents(s)
	s.onSubagent(spawnNotif("sub-1", "Same"))
	s.onSubagent(spawnNotif("sub-2", "Same"))
	outB := grokSpawnOutput("sub-2")
	outA := grokSpawnOutput("sub-1")
	_, _, extrasB := s.applyToolDelta("", toolDelta{id: "call-b", rawOutput: outB, hasRawOutput: true, wireName: "spawn_subagent"})
	s.emitAll(extrasB)
	_, _, extrasA := s.applyToolDelta("", toolDelta{id: "call-a", rawOutput: outA, hasRawOutput: true, wireName: "spawn_subagent"})
	s.emitAll(extrasA)

	snap := s.Snapshot()
	byID := map[string]SubagentInfo{}
	for _, a := range snap.Subagents {
		byID[a.ID] = a
	}
	if byID["sub-1"].ToolCallID != "call-a" || byID["sub-2"].ToolCallID != "call-b" {
		t.Fatalf("attachments %+v", byID)
	}
	tools := map[string]ToolEvent{}
	for _, t := range snap.Tools {
		tools[t.ID] = t
	}
	if tools["call-a"].Task == nil || tools["call-a"].Task.AgentID != "sub-1" {
		t.Fatalf("call-a %+v", tools["call-a"].Task)
	}
	if tools["call-b"].Task == nil || tools["call-b"].Task.AgentID != "sub-2" {
		t.Fatalf("call-b %+v", tools["call-b"].Task)
	}
	nTool := 0
	for {
		select {
		case ev := <-s.Events():
			if ev.Type == EventTool {
				nTool++
			}
		default:
			goto done
		}
	}
done:
	if nTool < 4 {
		t.Fatalf("want at least 4 EventTool from join, got %d", nTool)
	}
}

func TestJoinOwnerScopedCollidingToolIDs(t *testing.T) {
	grok := GrokProvider()
	s := newSession(Options{Provider: &grok})
	s.sessionID = "main"
	raw := mustJSON(map[string]any{
		"description": "Outer", "prompt": "p", "subagent_type": "explore", "background": true,
	})
	inner := mustJSON(map[string]any{
		"description": "Inner", "prompt": "q", "subagent_type": "explore", "background": true,
	})
	title := "spawn_subagent"
	s.mergeTool(toolDelta{id: "call-1", title: &title, wireName: "spawn_subagent", rawInput: raw, hasRawInput: true})
	s.onSubagent(spawnNotif("sub-1", "Outer"))
	s.applyToolDelta("sub-1", toolDelta{id: "call-1", title: &title, wireName: "spawn_subagent", rawInput: inner, hasRawInput: true})
	s.onSubagent(acp.SubagentNotification{
		Kind:            acp.SubagentSpawned,
		SubagentID:      "sub-1a",
		ChildSessionID:  "sub-1a",
		AttemptID:       "at1",
		ParentSessionID: "sub-1",
		Description:     "Inner",
		SubagentType:    "explore",
	})
	s.applyToolDelta("sub-1", toolDelta{id: "call-1", rawOutput: grokSpawnOutput("sub-1a"), hasRawOutput: true, wireName: "spawn_subagent"})
	s.applyToolDelta("", toolDelta{id: "call-1", rawOutput: grokSpawnOutput("sub-1"), hasRawOutput: true, wireName: "spawn_subagent"})

	snap := s.Snapshot()
	byID := map[string]SubagentInfo{}
	for _, a := range snap.Subagents {
		byID[a.ID] = a
	}
	if byID["sub-1"].ToolCallID != "call-1" {
		t.Fatalf("parent join %+v", byID["sub-1"])
	}
	if byID["sub-1a"].ToolCallID != "call-1" || byID["sub-1a"].ParentID != "sub-1" {
		t.Fatalf("grandchild join %+v", byID["sub-1a"])
	}
	if s.tools["call-1"].Task == nil || s.tools["call-1"].Task.AgentID != "sub-1" {
		t.Fatalf("main spawn tool %+v", s.tools["call-1"].Task)
	}
	childTool := s.subagents["sub-1"].tools["call-1"]
	if childTool.Task == nil || childTool.Task.AgentID != "sub-1a" {
		t.Fatalf("child spawn tool %+v", childTool.Task)
	}
}

func TestResumedSameIDNewAttemptResets(t *testing.T) {
	grok := GrokProvider()
	s := newSession(Options{Provider: &grok})
	s.sessionID = "main"
	s.onSubagent(acp.SubagentNotification{
		Kind: acp.SubagentSpawned, SubagentID: "sub-1", ChildSessionID: "sub-1",
		AttemptID: "a1", Description: "d", Model: "m",
	})
	s.onSubagent(acp.SubagentNotification{
		Kind: acp.SubagentFinished, SubagentID: "sub-1", ChildSessionID: "sub-1",
		AttemptID: "a1", Status: "failed", Error: "boom", Output: "out", DurationMs: 9, ToolCalls: 2, Turns: 3, TokensUsed: 4,
	})
	drainEvents(s)
	s.onSubagent(acp.SubagentNotification{
		Kind: acp.SubagentSpawned, SubagentID: "sub-1", ChildSessionID: "sub-1",
		AttemptID: "a2", Description: "d", Model: "m",
	})
	got := s.Snapshot().Subagents[0]
	if got.AttemptID != "a2" || got.Status != SubagentRunning || got.Error != "" || got.Output != "" {
		t.Fatalf("reset %+v", got)
	}
	if !got.EndedAt.IsZero() || got.ToolCalls != 0 || got.Turns != 0 || got.TokensUsed != 0 {
		t.Fatalf("counters %+v", got)
	}
}

func TestFinishedRetentionEvictsOldest(t *testing.T) {
	grok := GrokProvider()
	s := newSession(Options{Provider: &grok})
	s.sessionID = "main"
	s.onSubagent(spawnNotif("run", "running"))
	for i := 0; i < subagentFinishedCap; i++ {
		id := fmt.Sprintf("fin-%d", i)
		s.onSubagent(spawnNotif(id, id))
		s.onSubagent(acp.SubagentNotification{
			Kind: acp.SubagentFinished, SubagentID: id, ChildSessionID: id, Status: "completed",
		})
	}
	if n := countStatus(s, SubagentCompleted); n != subagentFinishedCap {
		t.Fatalf("finished %d", n)
	}
	s.onSubagent(spawnNotif("fin-new", "n"))
	s.onSubagent(acp.SubagentNotification{
		Kind: acp.SubagentFinished, SubagentID: "fin-new", ChildSessionID: "fin-new", Status: "completed",
	})
	ids := map[string]bool{}
	var running, finished int
	for _, a := range s.Snapshot().Subagents {
		ids[a.ID] = true
		if a.Status == SubagentRunning {
			running++
		} else {
			finished++
		}
	}
	if ids["fin-0"] {
		t.Fatal("oldest finished should be evicted")
	}
	if !ids["run"] || running != 1 {
		t.Fatalf("running must stay, running=%d", running)
	}
	if !ids["fin-new"] || finished != subagentFinishedCap {
		t.Fatalf("finished cap %d ids %v", finished, ids)
	}
}

func TestChildToolCapEvictsTerminalFirst(t *testing.T) {
	grok := GrokProvider()
	s := newSession(Options{Provider: &grok})
	s.sessionID = "main"
	s.onSubagent(spawnNotif("sub-1", "d"))
	completed, inflight := "completed", "in_progress"
	title := "t"
	for i := 0; i < childToolCap; i++ {
		st := completed
		if i >= childToolCap-8 {
			st = inflight
		}
		s.applyToolDelta("sub-1", toolDelta{id: fmt.Sprintf("t-%d", i), status: &st, title: &title, wireName: "list_dir"})
	}
	s.mu.Lock()
	n := len(s.subagents["sub-1"].tools)
	s.mu.Unlock()
	if n != childToolCap {
		t.Fatalf("filled %d", n)
	}
	s.applyToolDelta("sub-1", toolDelta{id: "t-new", status: &inflight, title: &title, wireName: "list_dir"})
	s.mu.Lock()
	rec := s.subagents["sub-1"]
	_, has0 := rec.tools["t-0"]
	_, hasNew := rec.tools["t-new"]
	_, evicted0 := rec.evicted["t-0"]
	n = len(rec.tools)
	s.mu.Unlock()
	if has0 || !evicted0 || !hasNew || n != childToolCap {
		t.Fatalf("evict terminal first has0=%v new=%v n=%d", has0, hasNew, n)
	}
	tool, changed, _ := s.applyToolDelta("sub-1", toolDelta{id: "t-0", status: &completed, title: &title})
	if changed {
		t.Fatalf("delta for evicted id must drop, got %+v", tool)
	}
}

func TestSnapshotSubagentsCloneIndependent(t *testing.T) {
	grok := GrokProvider()
	s := newSession(Options{Provider: &grok})
	s.sessionID = "main"
	s.onSubagent(acp.SubagentNotification{
		Kind: acp.SubagentSpawned, SubagentID: "sub-1", ChildSessionID: "sub-1",
		AttemptID: "a", Description: "d",
	})
	s.onSubagent(acp.SubagentNotification{
		Kind: acp.SubagentProgress, SubagentID: "sub-1", ChildSessionID: "sub-1",
		AttemptID: "a", TokensUsed: 7, ToolsUsed: []string{"list_dir"},
	})
	a := s.Snapshot()
	b := s.Snapshot()
	if len(a.Subagents) != 1 || len(b.Subagents) != 1 {
		t.Fatal("expected one")
	}
	a.Subagents[0].Description = "mutated"
	a.Subagents[0].ToolsUsed[0] = "mutated"
	if b.Subagents[0].Description == "mutated" || b.Subagents[0].ToolsUsed[0] == "mutated" {
		t.Fatal("clone shared storage")
	}
	if s.Snapshot().Subagents[0].Description == "mutated" {
		t.Fatal("mutation leaked into session")
	}
}

func TestCursorTaskSynthesis(t *testing.T) {
	for _, script := range []string{"task", "task-late"} {
		t.Run(script, func(t *testing.T) {
			s := startScript(t, script, true)
			log := collect(t, s)
			if _, err := s.Prompt(t.Context(), "go"); err != nil {
				t.Fatal(err)
			}
			waitFor(t, "finished", func() bool {
				return hasSubagentChange(log, SubagentChangeFinished, "")
			})
			var changes []string
			var last SubagentInfo
			for _, ev := range log.snapshot() {
				if ev.Type != EventSubagent || ev.Subagent == nil {
					continue
				}
				changes = append(changes, ev.SubagentChange)
				last = *ev.Subagent
			}
			if !containsSeq(changes, SubagentChangeSpawned, SubagentChangeProgress, SubagentChangeFinished) {
				t.Fatalf("changes %v", changes)
			}
			if last.Model != "cursor-grok-4.6-high-fast" || last.Description != "Count main.go lines" {
				t.Fatalf("receipt %+v", last)
			}
			if last.Status != SubagentCompleted {
				t.Fatalf("status %q", last.Status)
			}
		})
	}
}

func TestCursorTasksTitleDescription(t *testing.T) {
	s := startScript(t, "tasks", true)
	log := collect(t, s)
	if _, err := s.Prompt(t.Context(), "go"); err != nil {
		t.Fatal(err)
	}
	log.waitTexts(t, "done tasks")
	waitFor(t, "finished", func() bool { return hasSubagentChange(log, SubagentChangeFinished, "task-1") })
	got := lastSubagent(t, log.snapshot(), "task-1")
	if got.Description != "Subagent research" {
		t.Fatalf("title-derived description %q", got.Description)
	}
}

func TestCursorReceiptAfterTerminalStillProgress(t *testing.T) {
	s := newSession(Options{})
	title := "Task: work"
	completed := "completed"
	s.mergeTool(toolDelta{id: "t1", title: &title, status: &completed, rawInput: mustJSON(map[string]any{"_toolName": "task", "description": "work"}), hasRawInput: true})
	drainEvents(s)
	s.onTaskReceipt(acpTask("t1", "model-z", "agent-z", 11))
	var sawProgress bool
	deadline := time.After(time.Second)
	for !sawProgress {
		select {
		case ev := <-s.Events():
			if ev.Type == EventSubagent && ev.SubagentChange == SubagentChangeProgress {
				sawProgress = true
				if ev.Subagent == nil || ev.Subagent.Model != "model-z" {
					t.Fatalf("progress %+v", ev.Subagent)
				}
			}
		case <-deadline:
			t.Fatal("expected progress after terminal receipt")
		}
	}
}

func TestSubagentSnapshotRace(t *testing.T) {
	grok := GrokProvider()
	s := newSession(Options{Provider: &grok})
	s.sessionID = "main"
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			_ = s.Snapshot()
		}
	}()
	for i := 0; i < 50; i++ {
		id := fmt.Sprintf("sub-%d", i%3)
		s.onSubagent(spawnNotif(id, "d"))
		s.onChildUpdate(id, "list_dir", sessionUpdateWire{
			SessionUpdate: acp.UpdateAgentMessage,
			Content:       mustJSON(map[string]any{"type": "text", "text": "x"}),
		})
		st := "completed"
		title := "list"
		s.applyToolDelta(id, toolDelta{id: "call-1", status: &st, title: &title, wireName: "list_dir"})
		s.onSubagent(acp.SubagentNotification{
			Kind: acp.SubagentFinished, SubagentID: id, ChildSessionID: id, Status: "completed", Output: "o",
		})
	}
	wg.Wait()
}

func spawnNotif(id, desc string) acp.SubagentNotification {
	return acp.SubagentNotification{
		Kind:            acp.SubagentSpawned,
		SubagentID:      id,
		ChildSessionID:  id,
		AttemptID:       "at1",
		ParentSessionID: "main",
		Description:     desc,
		SubagentType:    "explore",
		Model:           "grok-4.6",
	}
}

func grokSpawnOutput(id string) json.RawMessage {
	return mustJSON(map[string]any{"type": "Text", "text": "Subagent started in background.\nsubagent_id: " + id})
}

func hasSubagentChange(log *eventLog, change, id string) bool {
	for _, ev := range log.snapshot() {
		if ev.Type != EventSubagent || ev.SubagentChange != change || ev.Subagent == nil {
			continue
		}
		if id == "" || ev.Subagent.ID == id {
			return true
		}
	}
	return false
}

func lastSubagent(t *testing.T, evs []Event, id string) SubagentInfo {
	t.Helper()
	var got SubagentInfo
	found := false
	for _, ev := range evs {
		if ev.Type == EventSubagent && ev.Subagent != nil && ev.Subagent.ID == id {
			got = *ev.Subagent
			found = true
		}
	}
	if !found {
		t.Fatalf("no subagent %s", id)
	}
	return got
}

func assertSpawnedThenStream(t *testing.T, evs []Event, id string) {
	t.Helper()
	var kinds []string
	for _, ev := range evs {
		if ev.Type == EventSubagent && ev.Subagent != nil && ev.Subagent.ID == id {
			kinds = append(kinds, ev.SubagentChange)
			continue
		}
		if ev.Agent != id {
			continue
		}
		switch ev.Type {
		case EventUser, EventThought, EventTool, EventText:
			kinds = append(kinds, string(ev.Type))
		}
	}
	if !containsSeq(kinds, SubagentChangeSpawned, string(EventUser), string(EventThought), string(EventTool), string(EventText), SubagentChangeProgress, SubagentChangeFinished) {
		t.Fatalf("sequence %v", kinds)
	}
}

func containsSeq(got []string, want ...string) bool {
	i := 0
	for _, g := range got {
		if i < len(want) && g == want[i] {
			i++
		}
	}
	return i == len(want)
}

func typesOf(evs []Event) []string {
	out := make([]string, 0, len(evs))
	for _, ev := range evs {
		out = append(out, string(ev.Type)+":"+ev.SubagentChange+":"+ev.Agent)
	}
	return out
}

func countStatus(s *session, st SubagentStatus) int {
	n := 0
	for _, a := range s.Snapshot().Subagents {
		if a.Status == st {
			n++
		}
	}
	return n
}
