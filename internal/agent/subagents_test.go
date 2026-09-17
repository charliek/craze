package agent

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

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
	// The late order (cancel2.out) cancels a child mid-tool; the early order
	// (cancel.out) cancels the parent after the child already completed, so
	// the child's finish is `completed` and its tool settles the same way.
	for script, want := range map[string]SubagentStatus{
		"grok-subagent-cancel":       SubagentCancelled,
		"grok-subagent-cancel-early": SubagentCompleted,
	} {
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
					if ev.Subagent == nil || ev.Subagent.Status != want {
						t.Fatalf("status %+v, want %s", ev.Subagent, want)
					}
				}
				if ev.Type == EventTool && ev.Agent == "sub-1" && ev.Tool != nil && ev.Tool.ID == "call-1" && ev.Tool.Status == string(want) {
					settled = true
				}
			}
			if n != 1 {
				t.Fatalf("exactly one finished, got %d", n)
			}
			if !settled {
				t.Fatalf("child tool must be settled %s with EventTool{Agent}", want)
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

// TestDivergentIDsKeyByChildSession pins the routing seam: records key by
// child_session_id (what routed updates are tagged with), so a provider that
// ever sends subagent_id != child_session_id still streams into its record.
func TestDivergentIDsKeyByChildSession(t *testing.T) {
	grok := GrokProvider()
	s := newSession(Options{Provider: &grok})
	s.sessionID = "main"
	s.onSubagent(acp.SubagentNotification{
		Kind: acp.SubagentSpawned, SubagentID: "logical-7", ChildSessionID: "session-abc",
		AttemptID: "at1", Description: "d", Model: "m",
	})
	if got := s.Snapshot().Subagents; len(got) != 1 || got[0].ID != "session-abc" {
		t.Fatalf("records %+v", got)
	}
	s.onChildUpdate("session-abc", "", sessionUpdateWire{
		SessionUpdate: updateUserMessage,
		Content:       mustJSON(map[string]any{"type": "text", "text": "prompt"}),
	})
	if got := s.Snapshot().Subagents[0]; got.Prompt != "prompt" {
		t.Fatalf("child update missed its record: %+v", got)
	}
	s.onSubagent(acp.SubagentNotification{
		Kind: acp.SubagentFinished, SubagentID: "logical-7", ChildSessionID: "session-abc",
		AttemptID: "at1", Status: "completed",
	})
	if got := s.Snapshot().Subagents[0]; got.Status != SubagentCompleted {
		t.Fatalf("finish missed its record: %+v", got)
	}
}

// TestRunningRecordCapDrops65th pins both session-side bounds. §3.2/§9 say a
// 65th concurrent child still gets a row, so past the routing cap the record
// is created with no transcript and no tool map; past subagentUnroutedCap of
// those the spawned is dropped, and a freed routing slot is reusable.
func TestRunningRecordCapDrops65th(t *testing.T) {
	grok := GrokProvider()
	s := newSession(Options{Provider: &grok})
	s.sessionID = "main"
	for i := 0; i < subagentRunCap; i++ {
		s.onSubagent(spawnNotif(fmt.Sprintf("sub-%d", i), "d"))
	}
	drainEvents(s)
	if got := len(s.Snapshot().Subagents); got != subagentRunCap {
		t.Fatalf("records %d, want %d", got, subagentRunCap)
	}
	s.onSubagent(spawnNotif("sub-65", "d"))
	rec, ok := s.subagents["sub-65"]
	if !ok {
		t.Fatal("the 65th concurrent child must still get a row")
	}
	if rec.info.Transcript || rec.tools != nil || !rec.unrouted {
		t.Fatalf("the 65th gets a row but no transcript: %+v", rec.info)
	}
	st, title := "completed", "list"
	if _, changed, _ := s.applyToolDelta("sub-65", toolDelta{id: "c1", status: &st, title: &title}); changed {
		t.Fatal("an unrouted child owns no tool store")
	}
	for i := 0; i < subagentUnroutedCap+4; i++ {
		s.onSubagent(spawnNotif(fmt.Sprintf("over-%d", i), "d"))
	}
	drainEvents(s)
	if got := len(s.Snapshot().Subagents); got != subagentRunCap+subagentUnroutedCap {
		t.Fatalf("rows %d, want the hard cap %d", got, subagentRunCap+subagentUnroutedCap)
	}
	s.onSubagent(finishNotif("sub-0", "completed"))
	s.onSubagent(spawnNotif("sub-fresh", "d"))
	fresh, ok := s.subagents["sub-fresh"]
	if !ok || fresh.unrouted || !fresh.info.Transcript {
		t.Fatalf("a finished slot must free a routed spawn: ok=%v %+v", ok, fresh)
	}
}

// TestFinishedEvictionDropsOldestFinish pins that eviction is by finish time,
// not spawn order: a long-runner spawned first, and finishing after 32 newer
// children came and went, must survive its own finished event.
func TestFinishedEvictionDropsOldestFinish(t *testing.T) {
	grok := GrokProvider()
	s := newSession(Options{Provider: &grok})
	s.sessionID = "main"
	s.onSubagent(spawnNotif("long", "the long runner"))
	for i := 0; i < subagentFinishedCap; i++ {
		id := fmt.Sprintf("fin-%d", i)
		s.onSubagent(spawnNotif(id, id))
		s.onSubagent(finishNotif(id, "completed"))
		drainEvents(s)
	}
	s.onSubagent(finishNotif("long", "completed"))

	var fin *SubagentInfo
	for _, ev := range pendingEvents(s) {
		if ev.Type == EventSubagent && ev.SubagentChange == SubagentChangeFinished && ev.Subagent != nil {
			fin = ev.Subagent
		}
	}
	if fin == nil || fin.ID != "long" {
		t.Fatalf("finished event %+v", fin)
	}
	ids := map[string]bool{}
	for _, a := range s.Snapshot().Subagents {
		ids[a.ID] = true
	}
	if !ids["long"] {
		t.Fatal("a finished event must be about a record the snapshot still holds")
	}
	if ids["fin-0"] {
		t.Fatal("the oldest finish is the one to evict")
	}
	if !ids["fin-31"] || len(ids) != subagentFinishedCap {
		t.Fatalf("records %d %v", len(ids), ids)
	}
}

// TestJoinByLongDescriptionMatchesCapped pins that the description join caps
// both sides: the record's went through capSubagent, so an uncapped compare
// could never match a description over subagentDescCap.
func TestJoinByLongDescriptionMatchesCapped(t *testing.T) {
	grok := GrokProvider()
	s := newSession(Options{Provider: &grok})
	s.sessionID = "main"
	desc := strings.Repeat("d", 400)
	title := "spawn_subagent"
	s.mergeTool(toolDelta{
		id: "call-a", title: &title, wireName: "spawn_subagent",
		rawInput:    mustJSON(map[string]any{"description": desc, "prompt": "p", "subagent_type": "explore"}),
		hasRawInput: true,
	})
	drainEvents(s)
	s.onSubagent(spawnNotif("sub-1", desc))
	got := s.Snapshot().Subagents[0]
	if got.ToolCallID != "call-a" {
		t.Fatalf("a %d-byte description must still join, got %q", len(desc), got.ToolCallID)
	}
}

// TestRawOutputJoinEmitsSubagentProgress pins that a join reaches --json: the
// record gains a ToolCallID, so it has to be re-emitted rather than waiting
// for the child to finish.
func TestRawOutputJoinEmitsSubagentProgress(t *testing.T) {
	grok := GrokProvider()
	s := newSession(Options{Provider: &grok})
	s.sessionID = "main"
	title := "spawn_subagent"
	s.mergeTool(toolDelta{
		id: "call-a", title: &title, wireName: "spawn_subagent",
		rawInput:    mustJSON(map[string]any{"description": "Tool side", "prompt": "p", "subagent_type": "explore"}),
		hasRawInput: true,
	})
	s.onSubagent(spawnNotif("sub-1", "Notification side"))
	if got := s.Snapshot().Subagents[0].ToolCallID; got != "" {
		t.Fatalf("the descriptions differ, nothing should have joined yet: %q", got)
	}
	drainEvents(s)
	_, _, extras := s.applyToolDelta("", toolDelta{
		id: "call-a", rawOutput: grokSpawnOutput("sub-1"), hasRawOutput: true, wireName: "spawn_subagent",
	})
	s.emitAll(extras)

	var progress *SubagentInfo
	for _, ev := range pendingEvents(s) {
		if ev.Type == EventSubagent && ev.SubagentChange == SubagentChangeProgress && ev.Subagent != nil {
			progress = ev.Subagent
		}
	}
	if progress == nil || progress.ToolCallID != "call-a" || progress.ID != "sub-1" {
		t.Fatalf("the rawOutput join must emit a progress carrying toolCallId, got %+v", progress)
	}
}

// TestDetachRestoresToolsOwnTask pins that detaching a child takes back
// everything the join stamped — status, model, type — and leaves the spawn
// tool with what it parsed out of its own rawInput.
func TestDetachRestoresToolsOwnTask(t *testing.T) {
	grok := GrokProvider()
	s := newSession(Options{Provider: &grok})
	s.sessionID = "main"
	title := "spawn_subagent"
	s.mergeTool(toolDelta{
		id: "call-a", title: &title, wireName: "spawn_subagent",
		rawInput:    mustJSON(map[string]any{"description": "Own", "prompt": "own prompt", "subagent_type": "explore"}),
		hasRawInput: true,
	})
	s.mergeTool(toolDelta{
		id: "call-b", title: &title, wireName: "spawn_subagent",
		rawInput:    mustJSON(map[string]any{"description": "Own", "prompt": "own prompt", "subagent_type": "explore"}),
		hasRawInput: true,
	})
	s.onSubagent(spawnNotif("sub-1", "Own"))
	s.onSubagent(finishNotif("sub-1", "failed"))
	// The rawOutput is authoritative and moves sub-1 from call-b to call-a.
	s.applyToolDelta("", toolDelta{
		id: "call-a", rawOutput: grokSpawnOutput("sub-1"), hasRawOutput: true, wireName: "spawn_subagent",
	})
	drainEvents(s)

	s.mu.Lock()
	detached := s.tools["call-b"]
	s.mu.Unlock()
	if detached.Task == nil {
		t.Fatal("a detached spawn tool is still a spawn tool")
	}
	if detached.Task.AgentID != "" || detached.Task.Status != "" || detached.Task.Model != "" {
		t.Fatalf("the child's stamped fields must go with it: %+v", detached.Task)
	}
	if detached.Task.Description != "Own" || detached.Task.Prompt != "own prompt" {
		t.Fatalf("the tool's own rawInput must survive: %+v", detached.Task)
	}
}

// TestEvictedChildToolIDsStayBounded pins that the tombstones of evicted
// child tool ids are bounded; they used to grow one entry per evicted tool
// for the whole session.
func TestEvictedChildToolIDsStayBounded(t *testing.T) {
	grok := GrokProvider()
	s := newSession(Options{Provider: &grok})
	s.sessionID = "main"
	s.onSubagent(spawnNotif("sub-1", "d"))
	completed, title := "completed", "t"
	const want = 2000
	for i := 0; i < want+childToolCap; i++ {
		s.applyToolDelta("sub-1", toolDelta{
			id: fmt.Sprintf("t-%d", i), status: &completed, title: &title, wireName: "list_dir",
		})
		s.mu.Lock()
		n := len(s.subagents["sub-1"].evicted)
		s.mu.Unlock()
		if n > childEvictedCap {
			t.Fatalf("tombstones grew to %d, cap %d", n, childEvictedCap)
		}
	}
	s.mu.Lock()
	rec := s.subagents["sub-1"]
	n, order := len(rec.evicted), len(rec.evictedOrder)
	newest := rec.isEvicted(fmt.Sprintf("t-%d", want-1))
	s.mu.Unlock()
	if n != order {
		t.Fatalf("set and order disagree: %d vs %d", n, order)
	}
	if !newest {
		t.Fatal("the trim must keep the newest tombstones")
	}
}

// TestCursorTaskRecordsCapped pins the cursor side of the running bound: task
// tools synthesise records, so a runaway turn must not grow the map without
// end. Cursor has no transcript, so a record past the cap is simply dropped.
func TestCursorTaskRecordsCapped(t *testing.T) {
	s := newSession(Options{})
	inProgress := "in_progress"
	for i := 0; i < subagentRunCap+4; i++ {
		id := fmt.Sprintf("task-%d", i)
		title := "Task: " + id
		s.mergeTool(toolDelta{
			id: id, title: &title, status: &inProgress,
			rawInput:    mustJSON(map[string]any{"_toolName": "task", "description": id}),
			hasRawInput: true,
		})
		drainEvents(s)
	}
	if got := len(s.Snapshot().Subagents); got != subagentRunCap {
		t.Fatalf("cursor records %d, want %d", got, subagentRunCap)
	}
	if _, ok := s.subagents[fmt.Sprintf("task-%d", subagentRunCap)]; ok {
		t.Fatal("a task past the running cap must leave no record")
	}
}

// TestSubagentStringCaps pins the §3.1 caps. Output keeps its tail — the
// child's answer is at the end of what it wrote, not the start.
func TestSubagentStringCaps(t *testing.T) {
	grok := GrokProvider()
	s := newSession(Options{Provider: &grok})
	s.sessionID = "main"
	s.onSubagent(acp.SubagentNotification{
		Kind: acp.SubagentSpawned, SubagentID: "sub-1", ChildSessionID: "sub-1", AttemptID: "a1",
		ParentSessionID: "main",
		Description:     strings.Repeat("d", 4000),
		SubagentType:    strings.Repeat("t", 400),
		Model:           strings.Repeat("m", 900),
	})
	for i := 0; i < 100; i++ {
		s.onChildUpdate("sub-1", "", sessionUpdateWire{
			SessionUpdate: updateUserMessage,
			Content:       mustJSON(map[string]any{"type": "text", "text": strings.Repeat("p", 64)}),
		})
		drainEvents(s)
	}
	longTitle, done := strings.Repeat("T", 900), "completed"
	s.applyToolDelta("sub-1", toolDelta{id: "c1", status: &done, title: &longTitle, wireName: "list_dir"})
	var used []string
	for i := 0; i < 20; i++ {
		used = append(used, fmt.Sprintf("tool-%d", i))
	}
	s.onSubagent(acp.SubagentNotification{
		Kind: acp.SubagentProgress, SubagentID: "sub-1", ChildSessionID: "sub-1", ToolsUsed: used,
	})
	s.onSubagent(acp.SubagentNotification{
		Kind: acp.SubagentFinished, SubagentID: "sub-1", ChildSessionID: "sub-1", Status: "failed",
		Error:  strings.Repeat("e", 4000),
		Output: strings.Repeat("o", 9000) + "THE-TAIL",
	})
	drainEvents(s)

	got := s.Snapshot().Subagents[0]
	for _, c := range []struct {
		name string
		s    string
		max  int
	}{
		{"Description", got.Description, subagentDescCap},
		{"SubagentType", got.SubagentType, subagentTypeCap},
		{"Model", got.Model, subagentModelCap},
		{"Error", got.Error, subagentErrorCap},
		{"Prompt", got.Prompt, taskPromptCap},
		{"Output", got.Output, subagentOutputCap},
		{"Activity", got.Activity, subagentActivityCap},
	} {
		if len(c.s) > c.max {
			t.Fatalf("%s is %d bytes, cap %d", c.name, len(c.s), c.max)
		}
		if !utf8.ValidString(c.s) {
			t.Fatalf("%s is not valid utf-8", c.name)
		}
	}
	if len(got.Prompt) != taskPromptCap {
		t.Fatalf("chunks must accumulate up to the cap, got %d bytes", len(got.Prompt))
	}
	if !strings.HasPrefix(got.Output, ellipsis) || !strings.HasSuffix(got.Output, "THE-TAIL") {
		t.Fatalf("Output must keep the tail, got %d bytes ending %q", len(got.Output), got.Output[len(got.Output)-16:])
	}
	if len(got.ToolsUsed) != subagentToolsUsedCap || got.ToolsUsed[0] != "tool-12" {
		t.Fatalf("ToolsUsed %v", got.ToolsUsed)
	}
}

// TestLastFinishedWins pins §3.3: a second finished with a different status is
// applied and re-emitted. A child tool that already settled on the first
// finish keeps that first terminal status — craze never re-opens a settled
// tool.
func TestLastFinishedWins(t *testing.T) {
	grok := GrokProvider()
	s := newSession(Options{Provider: &grok})
	s.sessionID = "main"
	s.onSubagent(spawnNotif("sub-1", "d"))
	inflight, title := "in_progress", "list"
	s.applyToolDelta("sub-1", toolDelta{id: "c1", status: &inflight, title: &title, wireName: "list_dir"})
	s.onSubagent(finishNotif("sub-1", "completed"))
	drainEvents(s)
	s.onSubagent(finishNotif("sub-1", "cancelled"))

	var last *SubagentInfo
	for _, ev := range pendingEvents(s) {
		if ev.Type == EventSubagent && ev.Subagent != nil {
			last = ev.Subagent
		}
	}
	if last == nil || last.Status != SubagentCancelled {
		t.Fatalf("the second finished must be re-emitted, got %+v", last)
	}
	if got := s.Snapshot().Subagents[0].Status; got != SubagentCancelled {
		t.Fatalf("record status %q", got)
	}
	s.mu.Lock()
	tool := s.subagents["sub-1"].tools["c1"]
	s.mu.Unlock()
	if tool.Status != "completed" {
		t.Fatalf("a settled child tool keeps its first terminal status, got %q", tool.Status)
	}
}

// TestUnknownSubagentNotificationsDropped pins §3.3: progress and finished for
// an id no spawned introduced never make a half-initialized row.
func TestUnknownSubagentNotificationsDropped(t *testing.T) {
	grok := GrokProvider()
	s := newSession(Options{Provider: &grok})
	s.sessionID = "main"
	s.onSubagent(acp.SubagentNotification{
		Kind: acp.SubagentProgress, SubagentID: "ghost", ChildSessionID: "ghost", TokensUsed: 9,
	})
	s.onSubagent(finishNotif("ghost", "completed"))
	if evs := pendingEvents(s); len(evs) != 0 {
		t.Fatalf("no event may escape for an unknown id: %v", typesOf(evs))
	}
	if got := s.Snapshot().Subagents; len(got) != 0 {
		t.Fatalf("records %+v", got)
	}
}

// TestActivityIsMostRecentChildTool pins §3.1's Activity.
func TestActivityIsMostRecentChildTool(t *testing.T) {
	grok := GrokProvider()
	s := newSession(Options{Provider: &grok})
	s.sessionID = "main"
	s.onSubagent(spawnNotif("sub-1", "d"))
	for i, title := range []string{"Read main.go", "List directory files", "Grep for TODO"} {
		name, done := title, "completed"
		s.applyToolDelta("sub-1", toolDelta{
			id: fmt.Sprintf("c-%d", i), status: &done, title: &name, wireName: "list_dir",
		})
	}
	if got := s.Snapshot().Subagents[0].Activity; got != "Grep for TODO" {
		t.Fatalf("Activity should be the newest child tool title, got %q", got)
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
	waitDone(t, &wg)
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

func finishNotif(id string, status string) acp.SubagentNotification {
	return acp.SubagentNotification{
		Kind:           acp.SubagentFinished,
		SubagentID:     id,
		ChildSessionID: id,
		AttemptID:      "at1",
		Status:         status,
	}
}

// pendingEvents takes everything already queued without blocking.
func pendingEvents(s *session) []Event {
	var out []Event
	for {
		select {
		case ev := <-s.Events():
			out = append(out, ev)
		default:
			return out
		}
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

func attemptNotif(n acp.SubagentNotification, attempt string) acp.SubagentNotification {
	n.AttemptID = attempt
	return n
}

// TestRestartRecomputesRouting pins that a new attempt of a retained record
// re-decides routing against the 64 routed slots, both ways.
func TestRestartRecomputesRouting(t *testing.T) {
	grok := GrokProvider()
	s := newSession(Options{Provider: &grok})
	s.sessionID = "main"
	// A routed child finishes; 64 others then fill the routed slots; its
	// restart cannot be routed any more.
	s.onSubagent(spawnNotif("sub-a", "d"))
	s.onSubagent(finishNotif("sub-a", "completed"))
	for i := 0; i < subagentRunCap; i++ {
		s.onSubagent(spawnNotif(fmt.Sprintf("fill-%d", i), "d"))
	}
	s.onSubagent(attemptNotif(spawnNotif("sub-a", "d"), "at2"))
	drainEvents(s)
	rec := s.subagents["sub-a"]
	if !rec.unrouted || rec.info.Transcript || rec.tools != nil || rec.info.Status != SubagentRunning {
		t.Fatalf("restart with the routed slots full must be unrouted: %+v tools=%v", rec.info, rec.tools != nil)
	}
	// Slots free up, it finishes and restarts again: routed this time.
	for i := 0; i < subagentRunCap; i++ {
		s.onSubagent(finishNotif(fmt.Sprintf("fill-%d", i), "completed"))
	}
	s.onSubagent(attemptNotif(finishNotif("sub-a", "completed"), "at2"))
	s.onSubagent(attemptNotif(spawnNotif("sub-a", "d"), "at3"))
	drainEvents(s)
	rec = s.subagents["sub-a"]
	if rec.unrouted || !rec.info.Transcript || rec.tools == nil {
		t.Fatalf("restart with a free slot must be routed: %+v tools=%v", rec.info, rec.tools != nil)
	}
	st, title := "completed", "list"
	if _, changed, _ := s.applyToolDelta("sub-a", toolDelta{id: "c1", status: &st, title: &title}); !changed {
		t.Fatal("a re-routed child owns a tool store again")
	}
}

// TestStaleAttemptLifecycleIgnored pins that a late progress or finished
// from a previous attempt never touches the current one.
func TestStaleAttemptLifecycleIgnored(t *testing.T) {
	grok := GrokProvider()
	s := newSession(Options{Provider: &grok})
	s.sessionID = "main"
	s.onSubagent(spawnNotif("sub-a", "d"))
	s.onSubagent(attemptNotif(spawnNotif("sub-a", "d"), "at2"))
	drainEvents(s)
	st, title := "in_progress", "list"
	s.applyToolDelta("sub-a", toolDelta{id: "c1", status: &st, title: &title})
	drainEvents(s)

	prog := acp.SubagentNotification{Kind: acp.SubagentProgress, SubagentID: "sub-a", ChildSessionID: "sub-a", AttemptID: "at1", TokensUsed: 999}
	s.onSubagent(prog)
	s.onSubagent(finishNotif("sub-a", "failed"))
	if evs := pendingEvents(s); len(evs) != 0 {
		t.Fatalf("stale attempt emitted %d events: %+v", len(evs), evs)
	}
	rec := s.subagents["sub-a"]
	if rec.info.Status != SubagentRunning || rec.info.AttemptID != "at2" || rec.info.TokensUsed != 0 {
		t.Fatalf("stale attempt changed the record: %+v", rec.info)
	}
	if rec.tools["c1"].Status != "in_progress" {
		t.Fatalf("stale finish settled the current attempt's tool: %+v", rec.tools["c1"])
	}
	// The current attempt's own finish still lands.
	s.onSubagent(attemptNotif(finishNotif("sub-a", "completed"), "at2"))
	if rec.info.Status != SubagentCompleted {
		t.Fatalf("the current attempt's finish must land: %+v", rec.info)
	}
}
