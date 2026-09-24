package harness

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/charliek/craze/internal/harness/store"
	"github.com/charliek/craze/internal/harness/tool"
	"github.com/charliek/craze/internal/harness/tool/opencode"
)

// The runner with tools switched on (plan 019 §3.5): scripted models call
// the opencode profile's real tools, in a real workspace.

// seq summarizes events as "<Type> <id or step>", so a test can pin their
// order without their payloads.
func seq(evs []Event) []string {
	var out []string
	for _, ev := range evs {
		switch e := ev.(type) {
		case TextDelta:
			out = append(out, "text "+e.Text)
		case ThoughtDelta:
			out = append(out, "thought "+e.Text)
		case ToolStarted:
			out = append(out, "started "+e.ID)
		case ToolCalled:
			out = append(out, "called "+e.ID)
		case ToolProgress:
			out = append(out, "progress "+e.ID)
		case ToolFinished:
			out = append(out, fmt.Sprintf("finished %s %s", e.ID, e.Result.Class))
		case StepDone:
			out = append(out, fmt.Sprintf("step %d %s %s saved=%v", e.Step, e.Finish, e.StopReason, e.Saved))
		case Steered:
			out = append(out, "steered "+e.Text)
		case Retrying:
			out = append(out, "retrying")
		case Diag:
			out = append(out, "diag "+e.Kind)
		}
	}
	return out
}

// put writes a file in the fixture's workspace.
func (f *fixture) put(name, content string) string {
	f.t.Helper()
	p := filepath.Join(f.workspace, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
	return p
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// TestToolLoop is the runner's main path (§7.5): the model reads a file,
// edits it, and answers. Each tool step persists as an assistant message
// with its call and a tool message with its result — tool_use, with the held
// user entry ahead of the first — and the answer ends the turn; the events
// report each call started, called and finished, in order, before its step's
// StepDone; the next turn replays all of it. The negative control is the
// file itself: the edit really ran, and only on the second step.
func TestToolLoop(t *testing.T) {
	f := newFixture(t, "http://127.0.0.1:1/v1")
	s := f.open(f.options())
	notes := f.put("notes.txt", "hello world\n")
	a := f.models["test/a"]
	readIn := input(t, map[string]any{"filePath": "notes.txt"})
	editIn := input(t, map[string]any{"filePath": "notes.txt", "oldString": "world", "newString": "craze"})
	var afterRead string
	a.push(
		callStep(callParts("c1", "read", readIn)),
		func(ctx context.Context, yield func(fantasy.StreamPart) bool) {
			b, _ := os.ReadFile(notes)
			afterRead = string(b) // the file before the edit's step: the read changed nothing
			reply(callParts("c2", "edit", editIn), finish(fantasy.FinishReasonToolCalls))(ctx, yield)
		},
		answerWith("done"),
		answerWith("ok"),
	)

	var ev events
	res, err := s.Run(context.Background(), "fix the greeting", ev.sink)
	if err != nil || res.StopReason != StopEndTurn {
		t.Fatalf("Run = %+v, %v; want end_turn", res, err)
	}
	if res.Usage != (Usage{Input: 30, Output: 15, CacheRead: 12}) {
		t.Errorf("the turn's usage = %+v, want the three steps' summed", res.Usage)
	}
	if afterRead != "hello world\n" {
		t.Fatalf("the file before the edit's step = %q", afterRead)
	}
	if got, _ := os.ReadFile(notes); string(got) != "hello craze\n" {
		t.Fatalf("the file after the turn = %q, want the edit applied", got)
	}

	evs := ev.list()
	equal(t, "event order", seq(evs), []string{
		"started t1.1.1", "called t1.1.1", "finished t1.1.1 ", "step 1 tool-calls tool_use saved=true",
		"started t1.2.1", "called t1.2.1", "finished t1.2.1 ", "step 2 tool-calls tool_use saved=true",
		"text done", "step 3 stop end_turn saved=true",
	})
	started := of[ToolStarted](evs)
	equal(t, "ToolStarted", started, []ToolStarted{
		{ID: "t1.1.1", Step: 1, Tool: "read", Kind: tool.KindRead, ReadOnly: true},
		{ID: "t1.2.1", Step: 2, Tool: "edit", Kind: tool.KindEdit},
	})
	called := plain(toEvents(of[ToolCalled](evs)))
	equal(t, "ToolCalled", called, []Event{
		ToolCalled{ID: "t1.1.1", CallID: "c1", Request: ToolRequest{Tool: "read", Kind: tool.KindRead, ReadOnly: true,
			Title: "notes.txt", Paths: []string{notes}, Input: readIn}},
		ToolCalled{ID: "t1.2.1", CallID: "c2", Request: ToolRequest{Tool: "edit", Kind: tool.KindEdit,
			Title: "notes.txt", Paths: []string{notes}, Input: editIn}},
	})
	finished := of[ToolFinished](evs)
	if r := finished[0].Result; r.IsError || !strings.Contains(r.Text, "1: hello world") || !strings.Contains(r.Content, "hello world") {
		t.Errorf("the read's result = %+v", r)
	}
	if r := finished[1].Result; r.IsError || r.Text != "Edit applied successfully." ||
		!slices.Equal(r.Edits, []tool.FileEdit{{Path: notes, Old: "hello world\n", New: "hello craze\n"}}) {
		t.Errorf("the edit's result = %+v", r)
	}
	for _, d := range finished {
		if d.Duration <= 0 || d.At.IsZero() {
			t.Errorf("%s finished at %v after %v; want both measured", d.ID, d.At, d.Duration)
		}
	}
	// Each StepDone names the entries its step wrote: step 1 the held user
	// entry, its call and its result; step 2 its call and result; step 3 the
	// answer.
	dones, ids := of[StepDone](evs), firstIDs(t, s, 6)
	for i, want := range [][]string{ids[0:3], ids[3:5], ids[5:6]} {
		if !slices.Equal(dones[i].Entries, want) {
			t.Errorf("step %d's entry ids = %v, want %v, the file's", i+1, dones[i].Entries, want)
		}
	}

	tr := transcript(t, s)
	lines := entries(tr)
	if len(lines) != 6 {
		t.Fatalf("transcript:\n%s", strings.Join(lines, "\n"))
	}
	equal(t, "transcript heads", heads(lines), []string{
		"user test/a high: fix the greeting",
		"assistant test/a high tool_use: [call c1 read " + readIn + "]",
		"tool test/a high: [result c1:",
		"assistant test/a high tool_use: [call c2 edit " + editIn + "]",
		"tool test/a high: [result c2: Edit applied successfully.]",
		"assistant test/a high end_turn: done",
	})
	// The results carry the harness's id, joining them to their events.
	for i, id := range map[int]string{2: "t1.1.1", 4: "t1.2.1"} {
		r, _ := fantasy.AsMessagePart[fantasy.ToolResultPart](tr.Entries[i].Message.Content[0])
		if r.ClientMetadata != `{"id":"`+id+`"}` {
			t.Errorf("entry %d's result metadata = %q, want the id %s", i, r.ClientMetadata, id)
		}
	}

	run(t, s, "thanks")
	calls := a.requests()
	if len(calls) != 4 {
		t.Fatalf("test/a saw %d requests, want 4", len(calls))
	}
	replay := promptOf(calls[3])[1:]
	if len(replay) != 7 || replay[1] != "assistant: [call c1 read "+readIn+"]" || !strings.HasPrefix(replay[2], "tool: [result c1: ") ||
		replay[5] != "assistant: done" || replay[6] != "user: thanks" {
		t.Fatalf("the next turn's history:\n%s", strings.Join(replay, "\n"))
	}
	// Every request offered the profile's tools, in its order.
	for i, c := range calls {
		var names []string
		for _, tl := range c.Tools {
			names = append(names, tl.GetName())
		}
		if !slices.Equal(names, []string{"bash", "read", "glob", "grep", "edit", "write", "agent", "todo_write", "ask_user_question", "exit_plan_mode"}) {
			t.Fatalf("request %d offered %v", i+1, names)
		}
	}
}

// heads cuts each transcript line at the first result's colon, so a test can
// pin a line whose result is a whole file.
func heads(lines []string) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		if j := strings.Index(l, "[result "); j >= 0 && !strings.HasSuffix(l, ".]") {
			if k := strings.Index(l[j:], ":"); k >= 0 {
				l = l[:j+k+1]
			}
		}
		out[i] = l
	}
	return out
}

// firstIDs is the ids of the transcript's first n entries.
func firstIDs(t *testing.T, s *Session, n int) []string {
	t.Helper()
	var ids []string
	for _, e := range transcript(t, s).Entries[:n] {
		ids = append(ids, e.ID)
	}
	return ids
}

func toEvents[T Event](evs []T) []Event {
	out := make([]Event, len(evs))
	for i, e := range evs {
		out[i] = e
	}
	return out
}

// TestAbnormalFinishes (D-43): on "length", "error", "content-filter" and
// "unknown" Fantasy records a step's calls and runs none. The runner answers
// each, in call order, with a not_executed result — the output limit named
// for "length", the finish reason for the others — persists the pair, ends
// the turn with the finish's own stop reason, and says so in a Diag; the
// next turn replays it paired. The negative control is the same step on
// "tool-calls": its command runs.
func TestAbnormalFinishes(t *testing.T) {
	cases := []struct {
		finish fantasy.FinishReason
		stop   string
		why    string
	}{
		{fantasy.FinishReasonLength, StopMaxTokens, "the response hit the output token limit"},
		{fantasy.FinishReasonError, StopEndTurn, `the response ended with finish reason "error"`},
		{fantasy.FinishReasonContentFilter, StopRefusal, `the response ended with finish reason "content-filter"`},
		{fantasy.FinishReasonUnknown, StopEndTurn, `the response ended with finish reason "unknown"`},
	}
	for _, tc := range cases {
		t.Run(string(tc.finish), func(t *testing.T) {
			f := newFixture(t, "http://127.0.0.1:1/v1")
			s := f.open(f.options())
			marker := filepath.Join(f.workspace, "ran")
			touch := input(t, map[string]any{"command": "touch " + marker})
			readIn := input(t, map[string]any{"filePath": "x"})
			f.models["test/a"].push(
				reply(textParts("trying"), callParts("c1", "bash", touch), callParts("c2", "read", readIn), finish(tc.finish)),
				answerWith("next"),
			)
			var ev events
			res, err := s.Run(context.Background(), "go", ev.sink)
			if err != nil || res.StopReason != tc.stop {
				t.Fatalf("Run = %+v, %v; want %s", res, err, tc.stop)
			}
			if exists(marker) {
				t.Fatal("a call ran on an abnormal finish")
			}
			sentence := func(name string) string {
				return fmt.Sprintf("Tool call %q was not executed: %s, so its arguments may be truncated. Re-issue the tool call with complete arguments.", name, tc.why)
			}
			evs := ev.list()
			equal(t, "events", seq(evs), []string{
				"text trying", "started t1.1.1", "started t1.1.2",
				"finished t1.1.1 not_executed", "finished t1.1.2 not_executed", "diag not_executed",
				"step 1 " + string(tc.finish) + " " + tc.stop + " saved=true",
			})
			fin := of[ToolFinished](evs)
			if fin[0].Result.Text != sentence("bash") || fin[1].Result.Text != sentence("read") || !fin[0].Result.IsError {
				t.Errorf("results: %q / %q", fin[0].Result.Text, fin[1].Result.Text)
			}
			equal(t, "diag", of[Diag](evs)[0], Diag{Kind: DiagNotExecuted, Fields: map[string]string{
				"step": "1", "finish": string(tc.finish), "calls": "2",
			}})
			equal(t, "transcript", entries(transcript(t, s)), []string{
				"user test/a high: go",
				fmt.Sprintf("assistant test/a high %s: trying [call c1 bash %s] [call c2 read %s]", tc.stop, touch, readIn),
				fmt.Sprintf("tool test/a high: [error c1: %s] [error c2: %s]", sentence("bash"), sentence("read")),
			})
			run(t, s, "again")
			replay := promptOf(f.models["test/a"].requests()[1])[1:]
			if len(replay) != 4 || !strings.HasPrefix(replay[2], "tool: [error c1: ") || replay[3] != "user: again" {
				t.Fatalf("the next turn's history:\n%s", strings.Join(replay, "\n"))
			}
		})
	}
	t.Run("control: tool-calls runs the call", func(t *testing.T) {
		f := newFixture(t, "http://127.0.0.1:1/v1")
		s := f.open(f.options())
		marker := filepath.Join(f.workspace, "ran")
		f.models["test/a"].push(callStep(callParts("c1", "bash", input(t, map[string]any{"command": "touch " + marker}))), answerWith("ok"))
		if res := run(t, s, "go"); res.StopReason != StopEndTurn || !exists(marker) {
			t.Fatalf("stop %q, marker made %v; want end_turn and the command run", res.StopReason, exists(marker))
		}
	})
}

// A call begun and never completed — the OpenAI-compatible provider drops a
// call cut off by the output limit after its input start has streamed
// (plan 019 §2.4) — still gets a terminal event, so no card is left
// pending, and is not persisted: the assistant message holds no call for it.
// The control is the answer's text, persisted as usual.
func TestStartedCallThatNeverArrives(t *testing.T) {
	f := newFixture(t, "http://127.0.0.1:1/v1")
	s := f.open(f.options())
	cut := []fantasy.StreamPart{
		{Type: fantasy.StreamPartTypeToolInputStart, ID: "c1", ToolCallName: "write"},
		{Type: fantasy.StreamPartTypeToolInputDelta, ID: "c1", Delta: `{"filePath":"a","content":"he`},
	}
	f.models["test/a"].push(reply(textParts("writing"), cut, finish(fantasy.FinishReasonLength)))
	var ev events
	if res, err := s.Run(context.Background(), "go", ev.sink); err != nil || res.StopReason != StopMaxTokens {
		t.Fatalf("Run = %+v, %v", res, err)
	}
	evs := ev.list()
	equal(t, "events", seq(evs), []string{"text writing", "started t1.1.1", "finished t1.1.1 not_executed", "step 1 length max_tokens saved=true"})
	if got := of[ToolFinished](evs)[0].Result.Text; !strings.HasPrefix(got, `Tool call "write" was not executed: the response hit the output token limit`) {
		t.Errorf("the settled call's text = %q", got)
	}
	equal(t, "transcript", entries(transcript(t, s)), []string{"user test/a high: go", "assistant test/a high max_tokens: writing"})
}

// TestStepLimit: a model that keeps calling tools is stopped after step
// 200 with max_turn_requests, and the step after is never requested. The
// negative control, 199 tool steps and a final answer as the 200th, ends
// end_turn: the limit stops a turn that had results left to read, not one
// that had simply reached it. (Each call names a tool that does not exist,
// with arguments of its own, so nothing runs and no two calls repeat.)
func TestStepLimit(t *testing.T) {
	steps := func(n int) []step {
		var out []step
		for i := range n {
			out = append(out, callStep(bareCall(fmt.Sprintf("c%d", i), "nope", fmt.Sprintf(`{"n":%d}`, i))))
		}
		return out
	}
	t.Run("stopped at 200", func(t *testing.T) {
		f := newFixture(t, "http://127.0.0.1:1/v1")
		s := f.open(f.options())
		f.models["test/a"].push(steps(201)...)
		res := run(t, s, "loop")
		if res.StopReason != StopMaxTurnRequests {
			t.Fatalf("stop reason %q, want %q", res.StopReason, StopMaxTurnRequests)
		}
		if n := len(f.models["test/a"].requests()); n != maxSteps || maxSteps != 200 {
			t.Fatalf("%d requests, want 200", n)
		}
		if n := len(transcript(t, s).Entries); n != 1+2*200 {
			t.Fatalf("the transcript holds %d entries, want the prompt and 200 paired steps", n)
		}
	})
	t.Run("control: a final 200th step", func(t *testing.T) {
		f := newFixture(t, "http://127.0.0.1:1/v1")
		s := f.open(f.options())
		f.models["test/a"].push(append(steps(199), answerWith("finally"))...)
		if res := run(t, s, "loop"); res.StopReason != StopEndTurn {
			t.Fatalf("stop reason %q, want end_turn", res.StopReason)
		}
	})
}

// TestSaveFailureStopsTheTurn: Fantasy ignores OnStepFinish's error, so a
// step that cannot be saved must stop the turn by a condition of its own —
// before another paid request, and before another tool touches a file. The
// control, the same turn with nothing failing, makes that second request.
func TestSaveFailureStopsTheTurn(t *testing.T) {
	for _, fail := range []bool{true, false} {
		t.Run(fmt.Sprintf("fail=%v", fail), func(t *testing.T) {
			f := newFixture(t, "http://127.0.0.1:1/v1")
			s := f.open(f.options())
			f.put("a.txt", "a\n")
			f.models["test/a"].push(callStep(callParts("c1", "read", input(t, map[string]any{"filePath": "a.txt"}))), answerWith("done"))
			var ev events
			res, err := s.Run(context.Background(), "go", func(e Event) {
				ev.sink(e)
				if _, ok := e.(ToolFinished); ok && fail {
					_ = s.store.Close() // the step's append will fail
				}
			})
			n := len(f.models["test/a"].requests())
			if !fail {
				if err != nil || res.StopReason != StopEndTurn || n != 2 {
					t.Fatalf("Run = %+v, %v after %d requests; want end_turn after 2", res, err, n)
				}
				return
			}
			if !errors.Is(err, store.ErrClosed) || !empty(res) || n != 1 {
				t.Fatalf("Run = %+v, %v after %d requests; want the store's error after 1", res, err, n)
			}
			d := of[StepDone](ev.list())
			if len(d) != 1 || d[0].Saved || d[0].Entries != nil || d[0].SaveError == "" {
				t.Fatalf("StepDone = %+v; want one, not saved, no ids, the error named", d)
			}
			if len(of[Diag](ev.list())) != 1 {
				t.Fatalf("events %v; want one save_failed Diag", seq(ev.list()))
			}
		})
	}
}

// TestRepeatedProviderIDs: a provider that numbers its calls per response
// (Fantasy's openai provider invents tool-call-<n>) repeats ids from one
// step to the next. Harness ids never repeat: each call has its own events
// and its own spill file, and the transcript pairs each step. The control
// is the provider's ids, which are the same.
func TestRepeatedProviderIDs(t *testing.T) {
	f := newFixture(t, "http://127.0.0.1:1/v1")
	s := f.open(f.options())
	long := func(n int) string { return input(t, map[string]any{"command": fmt.Sprintf("seq %d", n)}) }
	f.models["test/a"].push(
		callStep(callParts("tool-call-0", "bash", long(3000))),
		callStep(callParts("tool-call-0", "bash", long(3001))),
		answerWith("done"),
	)
	var ev events
	if res, err := s.Run(context.Background(), "count", ev.sink); err != nil || res.StopReason != StopEndTurn {
		t.Fatalf("Run = %+v, %v", res, err)
	}
	called := of[ToolCalled](ev.list())
	if len(called) != 2 || called[0].CallID != "tool-call-0" || called[1].CallID != called[0].CallID {
		t.Fatalf("control: the provider ids are %+v", called)
	}
	fin := of[ToolFinished](ev.list())
	if fin[0].ID != "t1.1.1" || fin[1].ID != "t1.2.1" {
		t.Fatalf("finished ids %q, %q; want t1.1.1 and t1.2.1", fin[0].ID, fin[1].ID)
	}
	for i, r := range fin {
		spill := r.Result.Trunc.Spill
		if filepath.Base(spill) != "tool_"+r.ID || !exists(spill) {
			t.Errorf("call %d spilled to %q; want tool_%s, on disk", i+1, spill, r.ID)
		}
	}
	lines := entries(transcript(t, s))
	if len(lines) != 6 || !strings.HasPrefix(lines[2], "tool test/a high: [result tool-call-0:") || !strings.HasPrefix(lines[4], "tool test/a high: [result tool-call-0:") {
		t.Fatalf("transcript:\n%s", strings.Join(lines, "\n"))
	}
}

// TestBadCallIDs: a step whose calls have a repeated or an empty provider id
// cannot be answered pairably. Nothing in it runs, nothing of it is
// persisted, and the turn fails with ErrBadToolCalls, a provider error,
// before another request; every call gets a terminal event. The control is
// the same step with distinct ids, which runs.
func TestBadCallIDs(t *testing.T) {
	cases := []struct {
		name     string
		id1, id2 string
		finish   fantasy.FinishReason
	}{
		{"repeated", "dup", "dup", fantasy.FinishReasonToolCalls},
		{"empty", "", "c2", fantasy.FinishReasonToolCalls},
		{"repeated on an abnormal finish", "dup", "dup", fantasy.FinishReasonLength},
		{"control: distinct", "c1", "c2", fantasy.FinishReasonToolCalls},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, "http://127.0.0.1:1/v1")
			s := f.open(f.options())
			m1, m2 := filepath.Join(f.workspace, "one"), filepath.Join(f.workspace, "two")
			f.models["test/a"].push(
				reply(callParts(tc.id1, "bash", input(t, map[string]any{"command": "touch " + m1})),
					callParts(tc.id2, "bash", input(t, map[string]any{"command": "touch " + m2})), finish(tc.finish)),
				answerWith("done"),
			)
			var ev events
			res, err := s.Run(context.Background(), "go", ev.sink)
			if tc.id1 != tc.id2 && tc.id1 != "" {
				if err != nil || res.StopReason != StopEndTurn || !exists(m1) || !exists(m2) {
					t.Fatalf("Run = %+v, %v; want both commands run", res, err)
				}
				return
			}
			var pe *ProviderError
			if !errors.Is(err, ErrBadToolCalls) || !errors.As(err, &pe) || pe.Provider != "test" || !empty(res) {
				t.Fatalf("Run = %+v, %v; want ErrBadToolCalls as a provider error", res, err)
			}
			if exists(m1) || exists(m2) {
				t.Fatal("a call of the bad step ran")
			}
			if n := len(f.models["test/a"].requests()); n != 1 {
				t.Fatalf("%d requests, want 1", n)
			}
			noTranscript(t, s)
			evs := ev.list()
			fin := of[ToolFinished](evs)
			if len(fin) != 2 || fin[0].ID == fin[1].ID {
				t.Fatalf("events %v; want each call finished once", seq(evs))
			}
			for _, r := range fin {
				if r.Result.Class != tool.ClassInvalidInput || !strings.Contains(r.Result.Text, "missing or repeated ids") {
					t.Errorf("%s's result = %+v", r.ID, r.Result)
				}
			}
			d := of[StepDone](evs)
			if len(d) != 1 || d[0].Saved || d[0].SaveError != "" || len(of[Diag](evs)) != 1 {
				t.Fatalf("events %v, StepDone %+v; want a bad-ids Diag and an unsaved step", seq(evs), d)
			}
			if s.tools.d.Pending() != 0 {
				t.Fatalf("the dispatcher still holds %d prepared calls", s.tools.d.Pending())
			}
		})
	}
}

// TestCancelDuringATool (§7.6): a cancel while a command runs aborts it; the
// step still finishes and persists — the call, and its aborted result — and
// the turn is cancelled, not end_turn, with no further request, although
// Fantasy finished the step normally. The control is the same turn left
// alone, which runs the next step and ends end_turn (and
// TestCancelAfterTheStepIsPersisted: a cancel after a final step keeps its
// stop reason).
func TestCancelDuringATool(t *testing.T) {
	for _, cancelIt := range []bool{true, false} {
		t.Run(fmt.Sprintf("cancel=%v", cancelIt), func(t *testing.T) {
			f := newFixture(t, "http://127.0.0.1:1/v1")
			s := f.open(f.options())
			started := filepath.Join(f.workspace, "started")
			cmd := ": > " + started + "; sleep 0.3"
			if cancelIt {
				cmd = ": > " + started + "; sleep 30"
			}
			f.models["test/a"].push(callStep(callParts("c1", "bash", input(t, map[string]any{"command": cmd}))), answerWith("done"))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			out := start(ctx, s, "wait", nil)
			if cancelIt {
				waitFor(t, func() bool { return exists(started) }, "the command to start")
				cancel()
			}
			got := await(t, out, "the turn")
			n := len(f.models["test/a"].requests())
			if !cancelIt {
				if got.err != nil || got.res.StopReason != StopEndTurn || n != 2 {
					t.Fatalf("Run = %+v, %v after %d requests; want end_turn after 2", got.res, got.err, n)
				}
				return
			}
			if got.err != nil || !only(got.res, StopCancelled) || n != 1 {
				t.Fatalf("Run = %+v, %v after %d requests; want cancelled after 1", got.res, got.err, n)
			}
			lines := entries(transcript(t, s))
			if len(lines) != 3 || !strings.HasPrefix(lines[1], "assistant test/a high tool_use: [call c1 bash") ||
				!strings.HasPrefix(lines[2], "tool test/a high: [error c1: "+tool.AbortedText) {
				t.Fatalf("transcript:\n%s", strings.Join(lines, "\n"))
			}
		})
	}
}

// waitFor polls cond until it holds, failing the test after waitTimeout.
func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s", waitTimeout, what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestCloseAfterACancelKillsAtOnce (§7.7, C6's hand-off): a command that
// ignores SIGTERM keeps an ordinarily cancelled turn for the whole grace —
// the control shows the grace is real — but Close during that grace kills it
// at once, through the closing channel, although the turn's context was
// already cancelled for another reason; Close returns well within the 3 s
// bound, where without the channel it would wait out the rest of the grace.
func TestCloseAfterACancelKillsAtOnce(t *testing.T) {
	// The command floods its output before it says it is ready, and that is
	// the whole of what keeps this test honest.
	//
	// Both cases need the call to be *supervised* when they act, and a file
	// the command wrote does not say that it is. bash starts a command on a
	// goroutine of its own and waits for it in launch; a cancel that lands
	// while that goroutine is still on its way back abandons the start, which
	// SIGKILLs what it began — no SIGTERM, no grace — and Run returns an
	// aborted result at once (bashCall.launch, launched.discard, which calls
	// the window "microseconds, or the scheduler's delay under load"). The
	// command has by then run, so the file is there, and a cancel fired on the
	// strength of the file alone can fall into that window: the control saw
	// its turn come back 1.6 ms after the cancel instead of after the ~3 s
	// grace, on a CI runner loaded enough to deschedule that goroutine for
	// longer than the child took to reach its first write.
	//
	// Nothing drains the command's output pipe until Run is past launch and
	// has started its reader, and a pipe holds 64 KiB at most, so a command
	// that has written a quarter of a megabyte proves its call is being
	// supervised — the state both cases here are about. Held inside launch,
	// the flooding command never reaches the line below it, and the waits
	// below simply wait, instead of racing.
	stubborn := func(f *fixture) (string, step) {
		ready := filepath.Join(f.workspace, "ready")
		cmd := "trap '' TERM; seq 1 40000; : > " + ready + "; sleep 30"
		return ready, callStep(callParts("c1", "bash", input(t, map[string]any{"command": cmd})))
	}
	t.Run("control: a cancel alone waits out the grace", func(t *testing.T) {
		f := newFixture(t, "http://127.0.0.1:1/v1")
		s := f.open(f.options())
		ready, st := stubborn(f)
		f.models["test/a"].push(st)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		out := start(ctx, s, "go", nil)
		waitFor(t, func() bool { return exists(ready) }, "the command's output to be drained, which says its call is supervised")
		at := time.Now()
		cancel()
		got := await(t, out, "the cancelled turn")
		if took := time.Since(at); got.res.StopReason != StopCancelled || took < 2500*time.Millisecond {
			t.Fatalf("the cancelled turn returned %+v after %v; want cancelled after the ~3 s grace", got.res, took)
		}
	})
	t.Run("close during the grace", func(t *testing.T) {
		f := newFixture(t, "http://127.0.0.1:1/v1")
		s := f.open(f.options())
		ready, st := stubborn(f)
		f.models["test/a"].push(st)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		out := start(ctx, s, "go", nil)
		waitFor(t, func() bool { return exists(ready) }, "the command's output to be drained, which says its call is supervised")
		cancel()
		time.Sleep(200 * time.Millisecond) // SIGTERM sent, and ignored
		at := time.Now()
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		if took := time.Since(at); took > 2*time.Second {
			t.Fatalf("Close took %v; want the command killed at once, not after its grace", took)
		}
		if got := await(t, out, "the turn"); got.res.StopReason != StopCancelled {
			t.Fatalf("Run = %+v, %v; want cancelled", got.res, got.err)
		}
	})
}

// fakeAgent stands in for Fantasy's: a test scripts, with the turn's own
// callbacks and tools, a course Fantasy v0.43.2 never takes.
type fakeAgent struct {
	tools []fantasy.AgentTool
	run   func(ctx context.Context, tools []fantasy.AgentTool, c fantasy.AgentStreamCall) (*fantasy.AgentResult, error)
}

func (a fakeAgent) Generate(context.Context, fantasy.AgentCall) (*fantasy.AgentResult, error) {
	return nil, errors.New("fake: Generate is not used")
}

func (a fakeAgent) Stream(ctx context.Context, c fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
	return a.run(ctx, a.tools, c)
}

// TestSynthesisDefence: a Fantasy that returns without OnStepFinish after it
// announced tool calls — which v0.43.2 never does once tools are dispatched
// — leaves the runner to write the step itself: the streamed text and the
// calls as announced, each with its recorded result or an aborted one, both
// lines marked interrupted, and a Diag. The control is the same course with
// no call announced: only the text is saved, as H1 saves it.
func TestSynthesisDefence(t *testing.T) {
	for _, announce := range []bool{true, false} {
		t.Run(fmt.Sprintf("announce=%v", announce), func(t *testing.T) {
			f := newFixture(t, "http://127.0.0.1:1/v1")
			s := f.open(f.options())
			f.put("a.txt", "alpha\n")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			in1 := input(t, map[string]any{"filePath": "a.txt"})
			s.newAgent = func(_ fantasy.LanguageModel, _ string, tools []fantasy.AgentTool) fantasy.Agent {
				return fakeAgent{tools: tools, run: func(ctx context.Context, tools []fantasy.AgentTool, c fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
					_ = c.OnStepStart(0)
					_ = c.OnTextDelta("0", "looking")
					if announce {
						_ = c.OnToolInputStart("c1", "read")
						_ = c.OnToolCall(fantasy.ToolCallContent{ToolCallID: "c1", ToolName: "read", Input: in1})
						_ = c.OnToolCall(fantasy.ToolCallContent{ToolCallID: "c2", ToolName: "read", Input: in1})
						resp, _ := tools[1].Run(ctx, fantasy.ToolCall{ID: "c1", Name: "read", Input: in1})
						_ = c.OnToolResult(fantasy.ToolResultContent{ToolCallID: "c1", ToolName: "read",
							Result: fantasy.ToolResultOutputContentText{Text: resp.Content}, ClientMetadata: resp.Metadata})
					}
					cancel()
					return nil, ctx.Err()
				}}
			}
			var ev events
			res, err := s.Run(ctx, "look", ev.sink)
			if err != nil || res.StopReason != StopCancelled {
				t.Fatalf("Run = %+v, %v; want cancelled", res, err)
			}
			lines := entries(transcript(t, s))
			if !announce {
				equal(t, "transcript", lines, []string{"user test/a high: look", "assistant test/a high cancelled interrupted: looking"})
				return
			}
			if len(lines) != 3 ||
				lines[1] != fmt.Sprintf("assistant test/a high cancelled interrupted: looking [call c1 read %s] [call c2 read %s]", in1, in1) ||
				!strings.HasPrefix(lines[2], "tool test/a high interrupted: [result c1: ") ||
				!strings.HasSuffix(lines[2], "[error c2: "+tool.AbortedText+"]") {
				t.Fatalf("transcript:\n%s", strings.Join(lines, "\n"))
			}
			equal(t, "events", seq(ev.list()), []string{
				"text looking", "started t1.1.1", "called t1.1.1", "started t1.1.2", "called t1.1.2",
				"finished t1.1.1 ", "finished t1.1.2 aborted", "diag synthesized",
			})
		})
	}
}

// checkedAgent is Fantasy's agent with every callback of the turn wrapped
// to record any that returns an error: none may (an error from OnToolCall
// leaks Fantasy's tool coordinator, agent.go:1744, 1758).
type checkedAgent struct {
	inner fantasy.Agent
	mu    *sync.Mutex
	errs  *[]string
}

func (a checkedAgent) Generate(ctx context.Context, c fantasy.AgentCall) (*fantasy.AgentResult, error) {
	return a.inner.Generate(ctx, c)
}

func (a checkedAgent) Stream(ctx context.Context, c fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
	note := func(name string, err error) error {
		if err != nil {
			a.mu.Lock()
			*a.errs = append(*a.errs, name+": "+err.Error())
			a.mu.Unlock()
		}
		return err
	}
	if cb := c.OnToolCall; cb != nil {
		c.OnToolCall = func(tc fantasy.ToolCallContent) error { return note("OnToolCall", cb(tc)) }
	}
	if cb := c.OnToolResult; cb != nil {
		c.OnToolResult = func(r fantasy.ToolResultContent) error { return note("OnToolResult", cb(r)) }
	}
	if cb := c.OnToolInputStart; cb != nil {
		c.OnToolInputStart = func(id, name string) error { return note("OnToolInputStart", cb(id, name)) }
	}
	if cb := c.OnStepFinish; cb != nil {
		c.OnStepFinish = func(s fantasy.StepResult) error { return note("OnStepFinish", cb(s)) }
	}
	if cb := c.OnStepStart; cb != nil {
		c.OnStepStart = func(n int) error { return note("OnStepStart", cb(n)) }
	}
	if cb := c.OnTextDelta; cb != nil {
		c.OnTextDelta = func(id, d string) error { return note("OnTextDelta", cb(id, d)) }
	}
	return a.inner.Stream(ctx, c)
}

// TestCallbacksReturnNil: across valid, invalid, unknown-tool and bad-id
// calls and abnormal finishes, every callback returns nil, and a turn leaves
// no goroutine behind. The control is a bare Fantasy agent whose OnToolCall
// returns an error: its tool coordinator is left blocked for good — the leak
// the runner's callbacks must never cause.
func TestCallbacksReturnNil(t *testing.T) {
	f := newFixture(t, "http://127.0.0.1:1/v1")
	baseline := goroutines()
	s := f.open(f.options())
	var mu sync.Mutex
	var errs []string
	s.newAgent = func(lm fantasy.LanguageModel, system string, tools []fantasy.AgentTool) fantasy.Agent {
		return checkedAgent{inner: defaultAgent(lm, system, tools), mu: &mu, errs: &errs}
	}
	f.put("a.txt", "a\n")
	read := input(t, map[string]any{"filePath": "a.txt"})
	f.models["test/a"].push(
		callStep(callParts("c1", "read", read), callParts("c2", "read", `{"filePath":`), callParts("c3", "nope", `{}`),
			callParts("c4", "read", `{"offset":"x"}`)),
		answerWith("ok"),
		reply(callParts("c5", "read", read), finish(fantasy.FinishReasonLength)),
		callStep(callParts("d", "read", read), callParts("d", "read", read)),
	)
	run(t, s, "one")
	run(t, s, "two")
	if _, err := s.Run(context.Background(), "three", nil); !errors.Is(err, ErrBadToolCalls) {
		t.Fatalf("the bad-ids turn = %v", err)
	}
	if len(errs) != 0 {
		t.Fatalf("callbacks returned errors: %v", errs)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return goroutines() <= baseline }, fmt.Sprintf("the goroutines to return to %d", baseline))

	t.Run("control: an erring OnToolCall leaks Fantasy's coordinator", func(t *testing.T) {
		m := &scripted{provider: "test", wire: "wire-a"}
		m.push(callStep(callParts("c1", "read", read)))
		before := goroutines()
		agent := fantasy.NewAgent(m, fantasy.WithTools(newBridged(s.tools.specs[1], func(context.Context, fantasy.ToolCall) fantasy.ToolResponse {
			return fantasy.ToolResponse{Content: "x"}
		})))
		_, err := agent.Stream(context.Background(), fantasy.AgentStreamCall{
			Prompt:     "go",
			OnToolCall: func(fantasy.ToolCallContent) error { return errors.New("refused") },
		})
		if err == nil {
			t.Fatal("the stream did not fail")
		}
		time.Sleep(100 * time.Millisecond)
		if goroutines() <= before {
			t.Fatal("no goroutine leaked: the control no longer shows why callbacks return nil")
		}
	})
}

// goroutines counts the process's goroutines, after letting any that are
// finishing finish.
func goroutines() int {
	time.Sleep(10 * time.Millisecond)
	return runtime.NumGoroutine()
}

// TestProfileSeam (§7.3): a second registered profile and a denying gate,
// neither touching the runner. A session opened on a model whose profile is
// the second one gets its tools, its prompt and its name in the header; a
// switch across profiles is refused with ErrProfileMismatch and leaves the
// model be, while one within the profile is allowed (the control); and a
// call the gate denies reaches the model as an error result, where the
// control — the default gate — runs it.
func TestProfileSeam(t *testing.T) {
	probe := &probeTool{}
	profiles := func() (*tool.Registry, error) {
		p, err := opencode.Profile()
		if err != nil {
			return nil, err
		}
		var reg tool.Registry
		if err := reg.Register(p); err != nil {
			return nil, err
		}
		second := tool.Profile{Name: "second", Tools: []tool.Tool{probe}, System: func(e tool.SystemEnv) string { return "second prompt for " + e.Workspace }}
		return &reg, reg.Register(second)
	}
	f := newFixture(t, "http://127.0.0.1:1/v1")
	m := f.table.Models["test/b"]
	m.ToolProfile = "second"
	f.table.Models["test/b"] = m

	t.Run("switching across profiles", func(t *testing.T) {
		opts := f.options()
		opts.tools.profiles = profiles
		s := f.open(opts)
		if err := s.SetModel("test/b"); !errors.Is(err, ErrProfileMismatch) {
			t.Fatalf("SetModel to the second profile's model = %v, want ErrProfileMismatch", err)
		}
		if cur, _ := s.Current(); cur != "test/a" {
			t.Fatalf("Current() = %q after a refused switch", cur)
		}
		if err := s.SetModel("other/c"); err != nil {
			t.Fatalf("control: a switch within the profile = %v", err)
		}
	})
	t.Run("a session on the second profile", func(t *testing.T) {
		opts := f.options()
		opts.tools.profiles = profiles
		opts.Model = "test/b"
		s := f.open(opts)
		f.models["test/b"].push(callStep(callParts("p1", "probe", `{}`)), answerWith("ok"))
		run(t, s, "probe it")
		calls := f.models["test/b"].requests()
		if len(calls[0].Tools) != 1 || calls[0].Tools[0].GetName() != "probe" || promptOf(calls[0])[0] != "system: second prompt for "+f.workspace {
			t.Fatalf("the request offered %v with %q", calls[0].Tools, promptOf(calls[0])[0])
		}
		if h := transcript(t, s).Header; h.ToolProfile != "second" {
			t.Fatalf("header profile %q", h.ToolProfile)
		}
		if promptOf(calls[1])[3] != "tool: [result p1: probed]" {
			t.Fatalf("the probe's result: %v", promptOf(calls[1]))
		}
	})
	for _, deny := range []bool{true, false} {
		t.Run(fmt.Sprintf("gate denies=%v", deny), func(t *testing.T) {
			f := newFixture(t, "http://127.0.0.1:1/v1")
			opts := f.options()
			if deny {
				opts.tools.gate = tool.GateFunc(func(context.Context, tool.Request) (tool.Decision, error) {
					return tool.Deny{Reason: "no reads today"}, nil
				})
			}
			s := f.open(opts)
			f.put("a.txt", "alpha\n")
			f.models["test/a"].push(callStep(callParts("c1", "read", input(t, map[string]any{"filePath": "a.txt"}))), answerWith("ok"))
			run(t, s, "read it")
			got := promptOf(f.models["test/a"].requests()[1])[3]
			if deny && got != "tool: [error c1: no reads today]" {
				t.Fatalf("the model read %q", got)
			}
			if !deny && !strings.Contains(got, "1: alpha") {
				t.Fatalf("control: the model read %q", got)
			}
		})
	}
}

// Open sweeps the spill directory: a spill file older than seven days is
// removed; the control, a fresh one, is kept.
func TestOpenSweepsOldSpillFiles(t *testing.T) {
	f := newFixture(t, "http://127.0.0.1:1/v1")
	dir := filepath.Join(f.home, tool.SpillDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	old, fresh := filepath.Join(dir, "tool_t1.1.1"), filepath.Join(dir, "tool_t1.1.2")
	for _, p := range []string{old, fresh} {
		if err := os.WriteFile(p, []byte("output"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	week := time.Now().Add(-8 * 24 * time.Hour)
	if err := os.Chtimes(old, week, week); err != nil {
		t.Fatal(err)
	}
	f.open(f.options())
	if exists(old) || !exists(fresh) {
		t.Fatalf("after Open: the old spill file exists %v, the fresh one %v; want only the fresh one", exists(old), exists(fresh))
	}
}

// probeTool is a second profile's one tool.
type probeTool struct{}

func (*probeTool) Spec() tool.Spec {
	return tool.Spec{ID: "probe", Description: "Probes.", Parameters: map[string]any{}, Required: []string{},
		Kind: tool.KindRead, ReadOnly: true, Truncate: tool.Head}
}

func (*probeTool) Prepare(tool.Env, tool.Call) (tool.Prepared, error) { return probeRun{}, nil }

type probeRun struct{}

func (probeRun) Request() tool.Request { return tool.Request{Title: "probe"} }

func (probeRun) Run(context.Context, tool.Env) tool.Result { return tool.Result{Text: "probed"} }
