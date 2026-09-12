package agent

import (
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/charliek/craze/internal/acp"
)

func startScriptOpts(t *testing.T, script string, opts Options) *session {
	t.Helper()
	opts.Binary = fakeAgentPath(t)
	opts.ExtraArgs = append(opts.ExtraArgs, "-script="+script)
	opts.Workspace = t.TempDir()
	opts.Stderr = io.Discard
	s := newSession(opts)
	if err := s.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// waitFor polls until want is satisfied, so tests never sleep a fixed time.
func waitFor(t *testing.T, what string, want func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if want() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func (e *eventLog) waitQuestion(t *testing.T) *QuestionEvent {
	t.Helper()
	ev := e.waitType(t, EventQuestion)
	return ev.Question
}

func TestTodosBothEnvelopes(t *testing.T) {
	for _, script := range []string{"todos", "todos-notify"} {
		t.Run(script, func(t *testing.T) {
			s := startScript(t, script, true)
			log := collect(t, s)
			if _, err := s.Prompt(t.Context(), "go"); err != nil {
				t.Fatal(err)
			}
			waitFor(t, "merged todos", func() bool {
				todos := s.Snapshot().Todos
				return len(todos) == 3 && todos[0].Status == "completed" && todos[1].Status == "in_progress"
			})
			todos := s.Snapshot().Todos
			if todos[2].Status != "pending" || todos[2].Content != "Run go vet" {
				t.Fatalf("todo 3 %+v", todos[2])
			}
			waitFor(t, "two EventTodos", func() bool {
				n := 0
				for _, ev := range log.snapshot() {
					if ev.Type == EventTodos {
						n++
					}
				}
				return n == 2
			})
			if s.Snapshot().TodosUpdatedAt.IsZero() {
				t.Fatal("TodosUpdatedAt not stamped")
			}
		})
	}
}

// The todo tool call itself is still recorded, but flagged so the transcript
// can hide it in favour of the request stream.
func TestTodoToolClassified(t *testing.T) {
	s := newSession(Options{})
	title := "Update TODOs: Read main.go"
	tool, _ := s.mergeTool(toolDelta{
		id:          "t1",
		title:       &title,
		rawInput:    mustJSON(map[string]any{"_toolName": "updateTodos"}),
		hasRawInput: true,
	})
	if !tool.IsTodoTool() || tool.IsTask() {
		t.Fatalf("classification %+v", tool)
	}
}

func TestTaskReceiptJoinsBothOrders(t *testing.T) {
	for _, script := range []string{"task", "task-late"} {
		t.Run(script, func(t *testing.T) {
			s := startScript(t, script, true)
			collect(t, s)
			if _, err := s.Prompt(t.Context(), "go"); err != nil {
				t.Fatal(err)
			}
			waitFor(t, "joined task receipt", func() bool {
				for _, tool := range s.Snapshot().Tools {
					if tool.Task != nil && tool.Task.Receipt && tool.Task.Model != "" {
						return true
					}
				}
				return false
			})
			tools := s.Snapshot().Tools
			if len(tools) != 1 {
				t.Fatalf("tools %+v", tools)
			}
			task := tools[0].Task
			if task.Model != "cursor-grok-4.6-high-fast" || task.AgentID != "agent-1234" {
				t.Fatalf("task %+v", task)
			}
			if task.DurationMs != 8010 || !task.Receipt {
				t.Fatalf("task %+v", task)
			}
			if task.Description != "Count main.go lines" || !strings.Contains(task.Prompt, "Count the number of lines") {
				t.Fatalf("task %+v", task)
			}
			if !tools[0].IsTask() || tools[0].ToolName != "task" {
				t.Fatalf("not classified as a task: %+v", tools[0])
			}
			if tools[0].Status != "completed" {
				t.Fatalf("status %q", tools[0].Status)
			}
		})
	}
}

func TestDuplicateTaskReceiptOverwrites(t *testing.T) {
	s := newSession(Options{})
	title := "Task: work"
	s.mergeTool(toolDelta{id: "t1", title: &title})
	go s.onTaskReceipt(acpTask("t1", "model-a", "agent-a", 1))
	<-s.Events()
	go s.onTaskReceipt(acpTask("t1", "model-b", "agent-b", 2))
	<-s.Events()
	task := s.Snapshot().Tools[0].Task
	if task.Model != "model-b" || task.AgentID != "agent-b" || task.DurationMs != 2 {
		t.Fatalf("task %+v", task)
	}
}

func TestTaskReceiptBeforeToolIsParked(t *testing.T) {
	s := newSession(Options{})
	s.onTaskReceipt(acpTask("t1", "model-a", "agent-a", 9))
	if len(s.Snapshot().Tools) != 0 {
		t.Fatal("receipt must not create a tool row on its own")
	}
	title := "Task: work"
	tool, changed := s.mergeTool(toolDelta{id: "t1", title: &title})
	if !changed || tool.Task == nil || tool.Task.Model != "model-a" || !tool.Task.Receipt {
		t.Fatalf("tool %+v", tool.Task)
	}
}

func TestDiffToolCountsAndOutput(t *testing.T) {
	s := startScript(t, "diff", true)
	collect(t, s)
	if _, err := s.Prompt(t.Context(), "go"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "diff tool completed", func() bool {
		for _, tool := range s.Snapshot().Tools {
			if len(tool.Diffs) > 0 {
				return true
			}
		}
		return false
	})
	var read, edit ToolEvent
	for _, tool := range s.Snapshot().Tools {
		switch tool.Kind {
		case "read":
			read = tool
		case "edit":
			edit = tool
		}
	}
	if read.Output == nil || !strings.Contains(read.Output.Content, "hello") {
		t.Fatalf("read output %+v", read.Output)
	}
	if len(edit.Diffs) != 1 {
		t.Fatalf("diffs %+v", edit.Diffs)
	}
	d := edit.Diffs[0]
	if d.Added != 1 || d.Removed != 1 || d.Truncated {
		t.Fatalf("diff %+v", d)
	}
	if d.Path != "/tmp/ws/main.go" {
		t.Fatalf("path %q", d.Path)
	}
}

func TestBigDiffCountsBeforeCap(t *testing.T) {
	s := startScript(t, "bigdiff", true)
	collect(t, s)
	if _, err := s.Prompt(t.Context(), "go"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "bigdiff completed", func() bool {
		for _, tool := range s.Snapshot().Tools {
			if len(tool.Diffs) > 0 {
				return true
			}
		}
		return false
	})
	d := s.Snapshot().Tools[0].Diffs[0]
	if !d.Truncated {
		t.Fatal("200 KiB sides must set Truncated")
	}
	if d.Added != 1 || d.Removed != 0 {
		t.Fatalf("counts must be computed before capping: +%d -%d", d.Added, d.Removed)
	}
	if len(d.OldText) > diffTextCap+len(ellipsis) || len(d.NewText) > diffTextCap+len(ellipsis) {
		t.Fatalf("text not capped: %d / %d", len(d.OldText), len(d.NewText))
	}
}

func TestBashToolOutput(t *testing.T) {
	s := startScript(t, "bash", true)
	collect(t, s)
	if _, err := s.Prompt(t.Context(), "go"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "exit code", func() bool {
		tools := s.Snapshot().Tools
		return len(tools) == 1 && tools[0].Output != nil && tools[0].Output.ExitCode != nil
	})
	out := s.Snapshot().Tools[0].Output
	if *out.ExitCode != 127 {
		t.Fatalf("exit %d", *out.ExitCode)
	}
	if !strings.HasPrefix(out.StderrHead, "Command 'go' not found") {
		t.Fatalf("stderr head %q", out.StderrHead)
	}
	if out.Stdout != "" || out.Truncated {
		t.Fatalf("output %+v", out)
	}
}

func TestSessionTitleUpdate(t *testing.T) {
	s := startScript(t, "title", true)
	log := collect(t, s)
	if _, err := s.Prompt(t.Context(), "hi"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "session title", func() bool { return s.Snapshot().Title == "Fake Title" })
	waitFor(t, "title on EventMeta", func() bool {
		for _, ev := range log.snapshot() {
			if ev.Type == EventMeta && ev.Text == "Fake Title" {
				return true
			}
		}
		return false
	})
}

func TestHeadlessAutoAnswersLogged(t *testing.T) {
	t.Run("ask", func(t *testing.T) {
		s := startScript(t, "ask", true)
		log := collect(t, s)
		if _, err := s.Prompt(t.Context(), "q"); err != nil {
			t.Fatal(err)
		}
		q := log.waitQuestion(t)
		if !q.Auto || q.ID != "ask-1" {
			t.Fatalf("question %+v", q)
		}
		if len(q.Answers["q1"]) != 1 || q.Answers["q1"][0] != "opt-a" {
			t.Fatalf("answers %+v", q.Answers)
		}
		if len(q.Questions) != 2 || !q.Questions[1].AllowMultiple {
			t.Fatalf("questions %+v", q.Questions)
		}
		log.waitTexts(t, "asked:answered:q1=opt-a;q2=opt-x")
	})
	t.Run("plan", func(t *testing.T) {
		s := startScript(t, "plan", true)
		log := collect(t, s)
		if _, err := s.Prompt(t.Context(), "p"); err != nil {
			t.Fatal(err)
		}
		ev := log.waitType(t, EventPlan)
		p := ev.Plan
		if !p.Auto || !p.Accepted || p.ID != "plan-1" {
			t.Fatalf("plan %+v", p)
		}
		if p.Name != "Fake Plan" || !strings.Contains(p.Plan, "## Steps") || len(p.Todos) != 2 {
			t.Fatalf("plan %+v", p)
		}
		log.waitTexts(t, "planned:accepted")
	})
}

func TestInteractiveQuestionAnswered(t *testing.T) {
	s := startScriptOpts(t, "ask", Options{Force: true, Interactive: true})
	log := collect(t, s)
	done := make(chan error, 1)
	go func() {
		_, err := s.Prompt(t.Context(), "q")
		done <- err
	}()
	q := log.waitQuestion(t)
	if q.Auto || q.ID != "ask-1" {
		t.Fatalf("question %+v", q)
	}
	if err := s.AnswerQuestion(q.ID, map[string][]string{"q1": {"opt-b"}, "q2": {"opt-y", "opt-z"}}, false); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	log.waitTexts(t, "asked:answered:q1=opt-b;q2=opt-y,opt-z")
	if err := s.AnswerQuestion(q.ID, nil, false); err == nil {
		t.Fatal("answering twice must fail")
	}
}

func TestInteractivePlanRejected(t *testing.T) {
	s := startScriptOpts(t, "plan", Options{Force: true, Interactive: true})
	log := collect(t, s)
	done := make(chan error, 1)
	go func() {
		_, err := s.Prompt(t.Context(), "p")
		done <- err
	}()
	ev := log.waitType(t, EventPlan)
	if ev.Plan.Auto || ev.Plan.ID != "plan-1" {
		t.Fatalf("plan %+v", ev.Plan)
	}
	if err := s.AnswerPlan(ev.Plan.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	log.waitTexts(t, "planned:rejected")
	if err := s.AnswerPlan(ev.Plan.ID, true); err == nil {
		t.Fatal("answering twice must fail")
	}
}

func TestAnswerWrongKindRejected(t *testing.T) {
	s := startScriptOpts(t, "ask", Options{Force: true, Interactive: true})
	log := collect(t, s)
	go func() { _, _ = s.Prompt(t.Context(), "q") }()
	q := log.waitQuestion(t)
	if err := s.AnswerPlan(q.ID, true); err == nil {
		t.Fatal("a question id must not answer a plan")
	}
	if err := s.AnswerPermission(q.ID, "opt-a"); err == nil {
		t.Fatal("a question id must not answer a permission")
	}
	if err := s.AnswerQuestion(q.ID, map[string][]string{"q1": {"opt-a"}}, true); err != nil {
		t.Fatal(err)
	}
}

func TestCancelAnswersBlockedQuestionOnce(t *testing.T) {
	s := startScriptOpts(t, "ask", Options{Force: true, Interactive: true})
	log := collect(t, s)
	done := make(chan Result, 1)
	go func() {
		res, _ := s.Prompt(t.Context(), "q")
		done <- res
	}()
	q := log.waitQuestion(t)
	if err := s.Cancel(t.Context()); err != nil {
		t.Fatal(err)
	}
	res := <-done
	if res.StopReason != "cancelled" {
		t.Fatalf("stop %q", res.StopReason)
	}
	if err := s.AnswerQuestion(q.ID, nil, false); err == nil {
		t.Fatal("cancel must have consumed the pending question")
	}
}

func TestCloseAnswersBlockedPlan(t *testing.T) {
	s := startScriptOpts(t, "plan", Options{Force: true, Interactive: true})
	log := collect(t, s)
	done := make(chan struct{})
	go func() {
		_, _ = s.Prompt(t.Context(), "p")
		close(done)
	}()
	log.waitType(t, EventPlan)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close left the plan request blocked")
	}
}

func acpTask(id, model, agentID string, ms int) acp.TaskRequest {
	return acp.TaskRequest{
		ToolCallID:  id,
		Description: "Count lines",
		Model:       model,
		AgentID:     agentID,
		DurationMs:  ms,
	}
}

func TestCancelWaitingAnswersEachKindWithItsOwnDecision(t *testing.T) {
	s := newSession(Options{})
	permID, permCh, okPerm := s.park(askPermission, pendingAsk{})
	askID, askCh, okAsk := s.park(askQuestion, pendingAsk{})
	planID, planCh, okPlan := s.park(askPlan, pendingAsk{})
	if !okPerm || !okAsk || !okPlan {
		t.Fatal("park refused outside a cancelled turn")
	}
	if permID != "perm-1" || askID != "ask-1" || planID != "plan-1" {
		t.Fatalf("ids %q %q %q", permID, askID, planID)
	}
	s.cancelWaiting()
	if d, ok := (<-permCh).(acp.PermissionDecision); !ok || !d.Cancelled {
		t.Fatalf("permission decision %v %T", ok, d)
	}
	if d, ok := (<-askCh).(acp.AskDecision); !ok || !d.Cancelled {
		t.Fatalf("question decision %v %T", ok, d)
	}
	if d, ok := (<-planCh).(acp.PlanDecision); !ok || !d.Cancelled {
		t.Fatalf("plan decision %v %T", ok, d)
	}
	s.mu.Lock()
	left := len(s.waiting)
	s.mu.Unlock()
	if left != 0 {
		t.Fatalf("%d requests still waiting", left)
	}
	if err := s.AnswerPermission(permID, ""); err == nil {
		t.Fatal("cancel must consume the pending permission")
	}
}

// A second local id per kind keeps its own counter, never a JSON-RPC id.
func TestLocalIDsAreNumberedPerKind(t *testing.T) {
	s := newSession(Options{})
	s.park(askQuestion, pendingAsk{})
	id, _, _ := s.park(askQuestion, pendingAsk{})
	if id != "ask-2" {
		t.Fatalf("id %q", id)
	}
	if got := s.nextID(askPlan); got != "plan-1" {
		t.Fatalf("id %q", got)
	}
}

func TestIdenticalOutputAndDiffNotReEmitted(t *testing.T) {
	s := newSession(Options{})
	d := toolDelta{
		id:           "t1",
		rawOutput:    mustJSON(map[string]any{"exitCode": 127, "stdout": "", "stderr": "boom"}),
		hasRawOutput: true,
		content:      mustJSON([]map[string]any{{"type": "diff", "path": "/a", "oldText": "a\n", "newText": "b\n"}}),
		hasContent:   true,
	}
	tool, changed := s.mergeTool(d)
	if !changed {
		t.Fatal("first merge must emit")
	}
	if tool.At.IsZero() {
		t.Fatal("ToolEvent.At not stamped")
	}
	if _, changed := s.mergeTool(d); changed {
		t.Fatal("an identical update must not re-emit")
	}
	d.rawOutput = mustJSON(map[string]any{"exitCode": 0, "stdout": "", "stderr": "boom"})
	if _, changed := s.mergeTool(d); !changed {
		t.Fatal("a changed exit code must emit")
	}
	d.content = mustJSON([]map[string]any{{"type": "diff", "path": "/b", "oldText": "a\n", "newText": "b\n"}})
	if _, changed := s.mergeTool(d); !changed {
		t.Fatal("a changed diff path must emit")
	}
}

// beginTurn opens a turn without spawning an agent, so the cancel-window
// tests can drive park() directly.
func beginTurn(s *session) {
	s.mu.Lock()
	s.turn++
	s.mu.Unlock()
}

func askReq() acp.AskQuestionRequest {
	return acp.AskQuestionRequest{
		ToolCallID: "t1",
		Questions: []acp.AskQuestion{{
			ID:      "q1",
			Prompt:  "Pick one",
			Options: []acp.AskOption{{ID: "opt-a", Label: "A"}},
		}},
	}
}

// A blocking request that arrives after the turn was cancelled must be
// answered immediately: cancelWaiting only drains what is already parked, so
// otherwise the handler blocks forever and cursor never gets its reply.
func TestHandlerArrivingAfterCancelDoesNotPark(t *testing.T) {
	kinds := []struct {
		name string
		run  func(s *session) bool
	}{
		{"question", func(s *session) bool { return s.onAskQuestion(askReq()).Cancelled }},
		{"plan", func(s *session) bool { return s.onCreatePlan(acp.CreatePlanRequest{Name: "P"}).Cancelled }},
		{"permission", func(s *session) bool {
			return s.onPermission(acp.PermissionRequest{
				Options: []acp.PermissionOption{{OptionID: "yes", Kind: acp.KindAllowOnce}},
			}).Cancelled
		}},
	}
	for _, k := range kinds {
		t.Run(k.name, func(t *testing.T) {
			s := newSession(Options{Interactive: true})
			beginTurn(s)
			s.cancelWaiting()
			done := make(chan bool, 1)
			go func() { done <- k.run(s) }()
			select {
			case cancelled := <-done:
				if !cancelled {
					t.Fatal("want a cancelled decision")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("handler parked into a cancelled turn")
			}
			s.mu.Lock()
			waiting := len(s.waiting)
			s.mu.Unlock()
			if waiting != 0 {
				t.Fatalf("%d requests parked after cancel", waiting)
			}
		})
	}
}

// A cancel can land after a request parked and before its card event was
// published. It has already answered the request and taken it out of waiting,
// so publishing anyway would put a card on screen that nobody could answer:
// every pick on it would come back as an unknown request.
func TestCancelledRequestRaisesNoCard(t *testing.T) {
	for _, kind := range []string{askPermission, askQuestion, askPlan} {
		t.Run(kind, func(t *testing.T) {
			s := newSession(Options{Interactive: true})
			beginTurn(s)
			id, _, ok := s.park(kind, pendingAsk{})
			if !ok {
				t.Fatal("park refused")
			}
			// The whole window, forced open.
			s.cancelWaiting()
			if s.emitParked(id, Event{Type: EventQuestion, Question: &QuestionEvent{ID: id}}) {
				t.Fatal("a request a cancel already answered must not raise a card")
			}
			select {
			case ev := <-s.events:
				t.Fatalf("a cancelled request published %+v", ev)
			default:
			}

			// A request that is still parked does publish, so the guard is not
			// simply "never emit".
			beginTurn(s)
			live, _, ok := s.park(kind, pendingAsk{})
			if !ok {
				t.Fatal("park refused in a fresh turn")
			}
			if !s.emitParked(live, Event{Type: EventQuestion, Question: &QuestionEvent{ID: live}}) {
				t.Fatal("a live request must publish its card")
			}
			select {
			case <-s.events:
			default:
				t.Fatal("nothing was published for a live request")
			}
		})
	}
}

// A new turn clears the cancel, so the next request parks normally.
func TestNextTurnParksAgain(t *testing.T) {
	s := newSession(Options{Interactive: true})
	beginTurn(s)
	s.cancelWaiting()
	beginTurn(s)
	id, ch, ok := s.park(askQuestion, pendingAsk{})
	if !ok || id == "" || ch == nil {
		t.Fatal("park refused in a fresh turn")
	}
}

func TestParkRefusedAfterClose(t *testing.T) {
	s := newSession(Options{Interactive: true})
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := s.park(askQuestion, pendingAsk{}); ok {
		t.Fatal("park must refuse on a closed session")
	}
}

func TestTaskReceiptsBoundedAndClearedAtTurnEnd(t *testing.T) {
	s := newSession(Options{})
	for i := 0; i < taskReceiptCap+10; i++ {
		s.onTaskReceipt(acpTask(fmt.Sprintf("id-%d", i), "m", "a", 1))
	}
	s.mu.Lock()
	n, order := len(s.taskReceipts), len(s.taskReceiptOrder)
	_, oldestKept := s.taskReceipts["id-0"]
	_, newestKept := s.taskReceipts[fmt.Sprintf("id-%d", taskReceiptCap+9)]
	s.mu.Unlock()
	if n != taskReceiptCap || order != taskReceiptCap {
		t.Fatalf("parked %d receipts (order %d), cap %d", n, order, taskReceiptCap)
	}
	if oldestKept {
		t.Fatal("the oldest receipt should have been evicted")
	}
	if !newestKept {
		t.Fatal("the newest receipt should have been kept")
	}

	s.mu.Lock()
	s.clearTaskReceiptsLocked()
	n, order = len(s.taskReceipts), len(s.taskReceiptOrder)
	s.mu.Unlock()
	if n != 0 || order != 0 {
		t.Fatalf("turn end left %d receipts (order %d)", n, order)
	}
}

func TestJoinedReceiptLeavesNoOrderEntry(t *testing.T) {
	s := newSession(Options{})
	s.onTaskReceipt(acpTask("t1", "m", "a", 1))
	title := "Task: work"
	s.mergeTool(toolDelta{id: "t1", title: &title})
	s.mu.Lock()
	n, order := len(s.taskReceipts), len(s.taskReceiptOrder)
	s.mu.Unlock()
	if n != 0 || order != 0 {
		t.Fatalf("joined receipt left %d entries (order %d)", n, order)
	}
}

func TestKindAndStatusSanitised(t *testing.T) {
	s := newSession(Options{})
	kind := "\x1b[2Jexecute"
	status := "\x1b]0;x\x07completed"
	tool, _ := s.mergeTool(toolDelta{id: "t1", kind: &kind, status: &status})
	if tool.Kind != "execute" || tool.Status != "completed" {
		t.Fatalf("kind %q status %q", tool.Kind, tool.Status)
	}
}

func TestCatalogStringsSanitised(t *testing.T) {
	esc := "\x1b[2J"
	res := &acp.NewSessionResult{
		Models: mustJSON(map[string]any{
			"currentModelId": esc + "m1",
			"availableModels": []map[string]string{
				{"modelId": esc + "m1", "name": esc + "Model One"},
			},
		}),
		Modes: mustJSON(map[string]any{
			"currentModeId": esc + "agent",
			"availableModes": []map[string]string{
				{"id": esc + "agent", "name": esc + "Agent"},
			},
		}),
		ConfigOptions: mustJSON([]map[string]any{{
			"id":           esc + "effort",
			"name":         esc + "Effort",
			"category":     esc + "thought_level",
			"type":         "select",
			"currentValue": esc + "medium",
			"options":      []map[string]string{{"value": esc + "high", "name": esc + "High"}},
		}}),
	}
	snap := snapshotFromNew(res)
	if snap.CurrentModel != "m1" || snap.Models[0].ID != "m1" || snap.Models[0].Name != "Model One" {
		t.Fatalf("models %+v / %q", snap.Models, snap.CurrentModel)
	}
	if snap.CurrentMode != "agent" || snap.Modes[0].ID != "agent" || snap.Modes[0].Name != "Agent" {
		t.Fatalf("modes %+v / %q", snap.Modes, snap.CurrentMode)
	}
	c := snap.Config[0]
	if c.ID != "effort" || c.Name != "Effort" || c.Category != "thought_level" || c.Current != "medium" {
		t.Fatalf("config %+v", c)
	}
	if c.SelectValues[0].Value != "high" || c.SelectValues[0].Name != "High" {
		t.Fatalf("select values %+v", c.SelectValues)
	}
	cmds := commandsFromUpdate([]acp.AvailableCommand{{Name: esc + "research", Description: esc + "Do it"}})
	if cmds[0].Name != "research" || cmds[0].Description != "Do it" {
		t.Fatalf("commands %+v", cmds)
	}
}

func TestCurrentModeUpdateSanitised(t *testing.T) {
	s := newSession(Options{})
	go s.onUpdate(acp.SessionNotification{Update: mustJSON(map[string]any{
		"sessionUpdate": acp.UpdateCurrentMode,
		"currentModeId": "\x1b[2Jplan",
	})})
	ev := <-s.Events()
	if got := s.Snapshot().CurrentMode; got != "plan" {
		t.Fatalf("mode %q", got)
	}
	// The event carries the mode as well as the snapshot: a mode that changed
	// and changed back leaves the snapshot exactly as it was, so only the event
	// can tell the UI it moved at all.
	if ev.Type != EventMeta || ev.Mode != "plan" {
		t.Fatalf("event %+v, want a meta event naming the mode", ev)
	}
}
