package agent

import (
	"errors"
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
	s := newTestSession(t, opts)
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

// waitAsk is the one ending an ask gets, and it fails if there are two: "every
// ask ends exactly once" is the property, not "an ending arrives".
func (e *eventLog) waitAsk(t *testing.T, id string) *AskUpdate {
	t.Helper()
	var found *AskUpdate
	waitFor(t, "the ending of "+id, func() bool {
		found = nil
		for _, ev := range e.snapshot() {
			if ev.Type == EventAsk && ev.Ask != nil && ev.Ask.ID == id {
				if found != nil {
					t.Fatalf("%s ended twice: %+v then %+v", id, found, ev.Ask)
				}
				found = ev.Ask
			}
		}
		return found != nil
	})
	return found
}

// askEndings is every ending for id that has been published so far.
func (e *eventLog) askEndings(id string) []*AskUpdate {
	var out []*AskUpdate
	for _, ev := range e.snapshot() {
		if ev.Type == EventAsk && ev.Ask != nil && ev.Ask.ID == id {
			out = append(out, ev.Ask)
		}
	}
	return out
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
	drainEvents(s)
	go s.onTaskReceipt(acpTask("t1", "model-a", "agent-a", 1))
	waitEventType(t, s, EventTool)
	go s.onTaskReceipt(acpTask("t1", "model-b", "agent-b", 2))
	waitEventType(t, s, EventTool)
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

// answerAsk is how a client answers now: through the registry, with the
// command that caused it (plan 021 §3.6). The tests below go straight to the
// registry rather than through engine.Control, because internal/agent cannot
// import the engine and Control.Answer is this call and nothing else.
func answerAsk(s *session, id string, a AskAnswer) error {
	_, err := s.asks.Answer("", id, a)
	return err
}

// newAskSession is newSession with the Close every test that parks an ask owes
// it: the registry publishes through the log's outbox, and only the log's Close
// joins the goroutine that drains it.
func newAskSession(t *testing.T, opts Options) *session {
	t.Helper()
	s := newSession(opts)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// inTurn is the Arrival of a request that reached craze inside a turn of its
// own. With no client of its own a session reads "there is one turn and it is
// turn 0" (turnLiveLocked), which is what a handler called directly is.
func inTurn() acp.Arrival { return acp.Arrival{InTurn: true} }

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
	if err := answerAsk(s, q.ID, AskAnswer{Answers: map[string][]string{"q1": {"opt-b"}, "q2": {"opt-y", "opt-z"}}}); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	log.waitTexts(t, "asked:answered:q1=opt-b;q2=opt-y,opt-z")
	if err := answerAsk(s, q.ID, AskAnswer{Skip: true}); !errors.Is(err, ErrAlreadyResolved) {
		t.Fatalf("answering twice must fail with ErrAlreadyResolved, got %v", err)
	}
	// One ending, and it is the answer.
	if u := log.waitAsk(t, q.ID); u.Outcome != AskAnswered || u.By != AskByClient {
		t.Fatalf("ending %+v", u)
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
	if err := answerAsk(s, ev.Plan.ID, AskAnswer{Reject: true}); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	log.waitTexts(t, "planned:rejected")
	if err := answerAsk(s, ev.Plan.ID, AskAnswer{Accept: true}); !errors.Is(err, ErrAlreadyResolved) {
		t.Fatalf("answering twice must fail with ErrAlreadyResolved, got %v", err)
	}
	if u := log.waitAsk(t, ev.Plan.ID); u.Outcome != AskAnswered || u.Accepted {
		t.Fatalf("ending %+v", u)
	}
}

func TestAnswerWrongKindRejected(t *testing.T) {
	s := startScriptOpts(t, "ask", Options{Force: true, Interactive: true})
	log := collect(t, s)
	go func() { _, _ = s.Prompt(t.Context(), "q") }()
	q := log.waitQuestion(t)
	if err := answerAsk(s, q.ID, AskAnswer{Accept: true}); !errors.Is(err, ErrBadAnswer) {
		t.Fatalf("a question id must not answer a plan, got %v", err)
	}
	if err := answerAsk(s, q.ID, AskAnswer{OptionID: "opt-a"}); !errors.Is(err, ErrBadAnswer) {
		t.Fatalf("a question id must not answer a permission, got %v", err)
	}
	// Both were refused with nothing emitted and the ask still open, which is
	// what the valid answer below proves: today's AnswerPermission cancelled
	// the request before it validated (plan 021 §4's first recorded change).
	if err := answerAsk(s, q.ID, AskAnswer{Skip: true}); err != nil {
		t.Fatal(err)
	}
	for _, ev := range log.snapshot() {
		if ev.Type == EventAsk && ev.Ask.ID == q.ID && ev.Ask.Outcome != AskAnswered {
			t.Fatalf("a refused answer ended the ask: %+v", ev.Ask)
		}
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
	if _, err := s.Cancel(t.Context()); err != nil {
		t.Fatal(err)
	}
	res := <-done
	if res.StopReason != "cancelled" {
		t.Fatalf("stop %q", res.StopReason)
	}
	if err := answerAsk(s, q.ID, AskAnswer{Skip: true}); !errors.Is(err, ErrAlreadyResolved) {
		t.Fatalf("cancel must have consumed the pending question, got %v", err)
	}
	if u := log.waitAsk(t, q.ID); u.Outcome != AskCancelled || u.By != AskByCancel {
		t.Fatalf("ending %+v", u)
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
	s := newAskSession(t, Options{})
	token := beginTurn(s)
	perm := openAsk(t, s, token, AskPermission)
	ask := openAsk(t, s, token, AskQuestion)
	plan := openAsk(t, s, token, AskPlan)
	if perm.ID() != "perm-1" || ask.ID() != "ask-1" || plan.ID() != "plan-1" {
		t.Fatalf("ids %q %q %q", perm.ID(), ask.ID(), plan.ID())
	}
	s.asks.CancelTurn(token)
	// Each waiter wakes with its own kind's cancelled decision, which is what
	// the three decision builders make of a cancelled record.
	if d := permissionDecision(perm.Wait()); !d.Cancelled {
		t.Fatalf("permission decision %+v", d)
	}
	if d := askDecision(ask.Wait()); !d.Cancelled {
		t.Fatalf("question decision %+v", d)
	}
	if d := planDecision(plan.Wait()); !d.Cancelled {
		t.Fatalf("plan decision %+v", d)
	}
	if left := s.asks.Asks(); len(left) != 0 {
		t.Fatalf("%d requests still waiting: %+v", len(left), left)
	}
	if err := answerAsk(s, perm.ID(), AskAnswer{Cancel: true}); !errors.Is(err, ErrAlreadyResolved) {
		t.Fatalf("cancel must consume the pending permission, got %v", err)
	}
}

// A second local id per kind keeps its own counter, never a JSON-RPC id.
func TestLocalIDsAreNumberedPerKind(t *testing.T) {
	s := newAskSession(t, Options{})
	token := beginTurn(s)
	openAsk(t, s, token, AskQuestion)
	if id := openAsk(t, s, token, AskQuestion).ID(); id != "ask-2" {
		t.Fatalf("id %q", id)
	}
	// An automatic resolution spends a number of its kind's counter too, as
	// nextID did for the headless path.
	if got := s.asks.Automatic(token, AskRequest{Kind: AskPlan, Body: AskBody{Plan: &PlanEvent{}}}, AskAnswer{Accept: true}); got.ID != "plan-1" {
		t.Fatalf("id %q", got.ID)
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

// beginTurn opens a turn without spawning an agent: the registry's token and
// the session bookkeeping prompt() sets around it, so a handler called with an
// inTurn Arrival parks against this turn exactly as a real one would.
func beginTurn(s *session) TurnToken {
	token := s.asks.BeginTurn()
	s.mu.Lock()
	s.turn++
	s.token = token
	s.inPrompt = true
	s.mu.Unlock()
	return token
}

// endTurn is beginTurn's counterpart: the turn is over for the registry and
// for the session, as the deferred release leaves it.
func endTurn(s *session, token TurnToken) {
	s.mu.Lock()
	s.inPrompt = false
	s.token = TurnToken{}
	s.mu.Unlock()
	s.asks.EndTurn(token)
}

// askBody is one ask's opening, minimal but of the right kind.
func askBody(kind AskKind) AskBody {
	switch kind {
	case AskQuestion:
		return AskBody{Question: &QuestionEvent{Title: "Question"}}
	case AskPlan:
		return AskBody{Plan: &PlanEvent{Name: "Plan"}}
	default:
		return AskBody{Permission: &PermissionEvent{Tool: "Shell"}}
	}
}

// openAsk parks one ask of kind against token and fails if it was refused: a
// test that means to park has to know that it did.
func openAsk(t *testing.T, s *session, token TurnToken, kind AskKind) *Ask {
	t.Helper()
	a, err := s.asks.Open(s.askCtx, token, AskRequest{Kind: kind, Body: askBody(kind)})
	if err != nil {
		t.Fatalf("open %s: %v", kind, err)
	}
	if rec := a.Record(); rec.Status != AskOpen {
		t.Fatalf("open %s was refused: %+v", kind, rec)
	}
	return a
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
		{"question", func(s *session) bool { return s.onAskQuestion(inTurn(), askReq()).Cancelled }},
		{"plan", func(s *session) bool { return s.onCreatePlan(inTurn(), acp.CreatePlanRequest{Name: "P"}).Cancelled }},
		{"permission", func(s *session) bool {
			return s.onPermission(inTurn(), acp.PermissionRequest{
				Options: []acp.PermissionOption{{OptionID: "yes", Kind: acp.KindAllowOnce}},
			}).Cancelled
		}},
	}
	for _, k := range kinds {
		t.Run(k.name, func(t *testing.T) {
			s := newAskSession(t, Options{Interactive: true})
			token := beginTurn(s)
			s.asks.CancelTurn(token)
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
			if left := s.asks.Asks(); len(left) != 0 {
				t.Fatalf("%d requests parked after cancel: %+v", len(left), left)
			}
		})
	}
}

// A request that reaches the registry once its turn has been cancelled raises
// no card at all: the opening it would have published is never written, and the
// one self-contained ending — with the body, so the record still says what was
// asked — is the whole of what it leaves behind. Before the registry this was
// two steps (park, then emitParked) with a window between them; the check and
// the insert are one section now, so the window is gone rather than guarded.
func TestCancelledRequestRaisesNoCard(t *testing.T) {
	for _, kind := range []AskKind{AskPermission, AskQuestion, AskPlan} {
		t.Run(string(kind), func(t *testing.T) {
			s := newAskSession(t, Options{Interactive: true})
			log := collect(t, s)
			token := beginTurn(s)
			s.asks.CancelTurn(token)

			a, err := s.asks.Open(s.askCtx, token, AskRequest{Kind: kind, Body: askBody(kind)})
			if err != nil {
				t.Fatal(err)
			}
			rec := a.Wait()
			if rec.Outcome != AskCancelled || rec.By != AskByCancel {
				t.Fatalf("a request a cancel already answered must not park: %+v", rec)
			}
			u := log.waitAsk(t, rec.ID)
			if u.Body == nil {
				t.Fatalf("a refused open owes a self-contained ending: %+v", u)
			}
			for _, ev := range log.snapshot() {
				if ev.Type == EventPermission || ev.Type == EventQuestion || ev.Type == EventPlan {
					t.Fatalf("a cancelled request published a card: %+v", ev)
				}
			}

			// A request in a fresh turn does park and does publish its card, so
			// the guard is not simply "never open".
			next := beginTurn(s)
			live := openAsk(t, s, next, kind)
			waitFor(t, "the live card", func() bool {
				for _, ev := range log.snapshot() {
					switch ev.Type {
					case EventPermission:
						return ev.Permission.ID == live.ID()
					case EventQuestion:
						return ev.Question.ID == live.ID()
					case EventPlan:
						return ev.Plan.ID == live.ID()
					}
				}
				return false
			})
			if got := log.askEndings(live.ID()); len(got) != 0 {
				t.Fatalf("a live card is not ended: %+v", got)
			}
		})
	}
}

// A new turn is a new token, so the next request parks normally.
func TestNextTurnParksAgain(t *testing.T) {
	s := newAskSession(t, Options{Interactive: true})
	token := beginTurn(s)
	s.asks.CancelTurn(token)
	openAsk(t, s, beginTurn(s), AskQuestion)
}

// A card belongs to the turn that asked for it. A handler goroutine the runtime
// delayed can start after its turn was cancelled and the next one began, and
// the turn it arrived in is the only thing that says so: the session's own
// "current turn" is the new one by then, and the request it is holding is the
// old one's. Publishing it would put a plan the user cancelled in front of a
// turn that never made it, and answering that card would uncover the new turn's
// plan offer underneath it.
//
// Two guards, one per window: park refuses a request whose turn is already
// over, and emitParked refuses one whose turn ended between the park and the
// emit.
func TestCardFromAnEndedTurnIsNeverPublished(t *testing.T) {
	s := startScriptOpts(t, "echo", Options{Interactive: true})
	log := collect(t, s)
	if _, err := s.Prompt(t.Context(), "one"); err != nil {
		t.Fatal(err)
	}

	// Turn 0 is the session before that prompt: over, and not the turn running.
	// It carries InTurn, because that is what a request of a turn looks like,
	// and the counter is what says the turn has gone.
	if dec := s.onCreatePlan(acp.Arrival{Turn: 0, InTurn: true}, acp.CreatePlanRequest{Name: "stale"}); !dec.Cancelled {
		t.Fatal("a handler from an ended turn must answer cancelled")
	}
	if left := s.asks.Asks(); len(left) != 0 {
		t.Fatalf("%d requests parked for an ended turn", len(left))
	}
	for _, ev := range log.snapshot() {
		if ev.Type == EventPlan {
			t.Fatalf("an ended turn published %+v", ev.Plan)
		}
	}

	// A request the client's counter still calls turn 1, but which arrived
	// between craze's turns — the prompt above has returned — belongs to no turn
	// of craze's own: it parks, publishes its card and is answerable. That is the
	// guard not being "never publish", and it is the distinction ACP's counter
	// alone cannot make (acp.Arrival).
	done := make(chan acp.PlanDecision, 1)
	go func() {
		done <- s.onCreatePlan(acp.Arrival{Turn: 1}, acp.CreatePlanRequest{Name: "live"})
	}()
	ev := log.waitType(t, EventPlan)
	if ev.Plan == nil || ev.Plan.Name != "live" {
		t.Fatalf("published %+v", ev.Plan)
	}
	if err := answerAsk(s, ev.Plan.ID, AskAnswer{Reject: true}); err != nil {
		t.Fatal(err)
	}
	if dec := <-done; dec.Cancelled {
		t.Fatal("a card of a live request was answered as cancelled")
	}

	// The window the two-step park/emit had: a handler goroutine of turn 1 that
	// the runtime did not schedule until turn 1 was over. Its Arrival says it
	// belonged to a turn, and no turn of craze's is open, so it raises no card
	// at all — and the one ending it owes carries the body.
	before := len(log.askEndings(""))
	_ = before
	if dec := s.onCreatePlan(acp.Arrival{Turn: 1, InTurn: true}, acp.CreatePlanRequest{Name: "late"}); !dec.Cancelled {
		t.Fatal("a handler whose own turn has ended must answer cancelled")
	}
	for _, ev := range log.snapshot() {
		if ev.Type == EventPlan && ev.Plan.Name == "late" {
			t.Fatalf("a card whose turn ended must not publish: %+v", ev.Plan)
		}
	}
	if left := s.asks.Asks(); len(left) != 0 {
		t.Fatalf("a request nobody can answer was left parked: %+v", left)
	}
}

// Both histories ACP's turn counter cannot tell apart, and the opposite answers
// they deserve (acp.Arrival; panel r16 finding 3). The counter is the same in
// each: a prompt returning clears the client's in-turn flag without moving it.
func TestArrivalTellsARetiredTurnFromNoTurn(t *testing.T) {
	t.Run("registered during the turn, handled after it", func(t *testing.T) {
		s := startScriptOpts(t, "echo", Options{Interactive: true})
		log := collect(t, s)
		if _, err := s.Prompt(t.Context(), "one"); err != nil {
			t.Fatal(err)
		}
		dec := s.onAskQuestion(acp.Arrival{Turn: 1, InTurn: true}, askReq())
		if !dec.Cancelled {
			t.Fatalf("a retired turn's request must be cancelled: %+v", dec)
		}
		for _, ev := range log.snapshot() {
			if ev.Type == EventQuestion {
				t.Fatalf("a retired turn's request raised a card: %+v", ev.Question)
			}
		}
		// A hidden id: no card was raised for it, and at the baseline such a
		// request never reached the session's counter at all, so it must not
		// spend the number the next visible question would have had (review
		// r17, finding 8).
		u := log.waitAsk(t, "ask-x1")
		if u.Outcome != AskTurnEnded || u.Body == nil {
			t.Fatalf("ending %+v", u)
		}
	})
	t.Run("registered between turns", func(t *testing.T) {
		s := startScriptOpts(t, "echo", Options{Interactive: true})
		log := collect(t, s)
		if _, err := s.Prompt(t.Context(), "one"); err != nil {
			t.Fatal(err)
		}
		done := make(chan acp.AskDecision, 1)
		go func() { done <- s.onAskQuestion(acp.Arrival{Turn: 1}, askReq()) }()
		ev := log.waitType(t, EventQuestion)
		if ev.Question == nil || ev.Question.ID != "ask-1" {
			t.Fatalf("published %+v", ev.Question)
		}
		if err := answerAsk(s, "ask-1", AskAnswer{Answers: map[string][]string{"q1": {"opt-a"}}}); err != nil {
			t.Fatal(err)
		}
		if dec := <-done; dec.Cancelled {
			t.Fatalf("a between-turns request must be answerable: %+v", dec)
		}
	})
}

// An ask opened between turns survives a whole turn: no turn's end may take
// away what belongs to no turn (A-X4).
func TestAskBetweenTurnsSurvivesATurn(t *testing.T) {
	s := newAskSession(t, Options{Interactive: true})
	log := collect(t, s)
	a, err := s.asks.Open(s.askCtx, TurnToken{}, AskRequest{Kind: AskQuestion, Body: askBody(AskQuestion)})
	if err != nil {
		t.Fatal(err)
	}
	token := beginTurn(s)
	endTurn(s, token)
	if got := log.askEndings(a.ID()); len(got) != 0 {
		t.Fatalf("a turn ended an ask that belonged to no turn: %+v", got)
	}
	if err := answerAsk(s, a.ID(), AskAnswer{Skip: true}); err != nil {
		t.Fatal(err)
	}
	if rec := a.Wait(); rec.Outcome != AskAnswered {
		t.Fatalf("record %+v", rec)
	}
}

// A turn that ends with an ask still open ends that ask, and the ending is in
// the record before the turn's own terminal event (§3.6's flush barrier).
func TestTurnEndingEndsItsOpenAsk(t *testing.T) {
	s := newAskSession(t, Options{Interactive: true})
	log := collect(t, s)
	token := beginTurn(s)
	a := openAsk(t, s, token, AskPlan)
	s.endAskTurn(token)
	s.emit(Event{Type: EventDone, StopReason: "end_turn"})
	rec := a.Wait()
	if rec.Outcome != AskTurnEnded || rec.By != AskByTurn {
		t.Fatalf("record %+v", rec)
	}
	log.waitType(t, EventDone)
	seen := false
	for _, ev := range log.snapshot() {
		switch {
		case ev.Type == EventAsk && ev.Ask.ID == a.ID():
			seen = true
		case ev.Type == EventDone:
			if !seen {
				t.Fatal("the turn's done overtook the ask's ending")
			}
			return
		}
	}
	t.Fatal("no EventDone")
}

func TestParkRefusedAfterClose(t *testing.T) {
	s := newSession(Options{Interactive: true})
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	a, err := s.asks.Open(s.askCtx, TurnToken{}, AskRequest{Kind: AskQuestion, Body: askBody(AskQuestion)})
	if err != nil {
		t.Fatal(err)
	}
	if rec := a.Wait(); rec.Status != AskResolved || rec.Outcome != AskClosing {
		t.Fatalf("open must be refused on a closed session: %+v", rec)
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
