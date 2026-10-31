package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"charm.land/fantasy"

	"github.com/charliek/craze/internal/journal"
)

// transientFail is a scripted reset, its wait cut short (a model's real reset is
// the harness's and llm's to test).
func transientFail() []fantasy.StreamPart {
	return []fantasy.StreamPart{{Type: fantasy.StreamPartTypeError, Error: &fantasy.ProviderError{
		Title: "stream transport error", Message: "INTERNAL_ERROR (received from peer)", TransientError: true,
		ResponseHeaders: map[string]string{"retry-after-ms": "1"},
	}}}
}

// TestNativeStepRetryNamesWhatItRetracts: the event says where the attempt began,
// and that is exactly the line between what a consumer keeps and what it drops:
// whatever the attempt published is at or past From and before the event, and
// everything of the step before it and of the attempt after it is not. The
// abandoned call leaves the snapshot's tools.
func TestNativeStepRetryNamesWhatItRetracts(t *testing.T) {
	f := newNativeFixture(t)
	ws := nativeWorkspaceWith(t, map[string]string{"main.go": "package main\n"})
	dir := filepath.Join(t.TempDir(), "journal")
	s := f.started(Options{Workspace: ws, JournalDir: dir})
	jw := journalOf(t, s.log)
	f.models["test/a"].push(
		reply(textParts("first step"), nativeCallParts("c1", "read", nativeArgs(t, map[string]any{"filePath": filepath.Join(ws, "main.go")})), finishParts(fantasy.FinishReasonToolCalls)),
		reply(openTextParts("abandoned"), []fantasy.StreamPart{{Type: fantasy.StreamPartTypeToolInputStart, ID: "c2", ToolCallName: "read"}}, transientFail()),
		reply(textParts("whole"), finishParts(fantasy.FinishReasonStop)),
	)
	if _, err := s.Prompt(context.Background(), "go"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	evs := drained(s)
	retries := ofType(evs, EventStepRetry)
	if len(retries) != 1 {
		t.Fatalf("%d step_retry events; want 1:\n%s", len(retries), strings.Join(loadLines(evs), "\n"))
	}
	r := retries[0]
	info := r.StepRetry
	if info == nil || info.Turn != 1 || info.Step != 2 || info.Attempt != 1 || info.Total != 1 || !info.Discarded || info.DelayMs != 1 ||
		!strings.Contains(info.Reason, "INTERNAL_ERROR") || r.Agent != "" {
		t.Fatalf("the event: %+v, %+v", r, info)
	}
	for _, ev := range evs {
		var attempt bool
		switch ev.Type {
		case EventText:
			attempt = ev.Text == "abandoned"
			if ev.Text == "first step" && ev.Seq >= info.From {
				t.Fatalf("the step before the attempt is at or past From %d: %+v", info.From, ev)
			}
		case EventTool:
			attempt = ev.Tool != nil && ev.Tool.ID == "t1.2.1"
			if ev.Tool != nil && ev.Tool.ID == "t1.1.1" && ev.Seq >= info.From {
				t.Fatalf("the first step's call is at or past From %d: %+v", info.From, ev)
			}
		}
		if attempt && (ev.Seq < info.From || ev.Seq >= r.Seq) {
			t.Fatalf("an event of the attempt, seq %d, is outside [%d, %d): %+v", ev.Seq, info.From, r.Seq, ev)
		}
		if ev.Type == EventText && ev.Text == "whole" && ev.Seq <= r.Seq {
			t.Fatalf("the next attempt's text, seq %d, is not past the retry's %d", ev.Seq, r.Seq)
		}
	}
	for _, row := range s.Snapshot().Tools {
		if row.ID == "t1.2.1" {
			t.Fatalf("the snapshot lists the abandoned call: %+v", row)
		}
	}
	closeJournaled(t, s, jw)
	diags, events := 0, 0
	for _, file := range journalFiles(t, dir) {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for line := range strings.Lines(string(raw)) {
			var rec struct {
				Kind   string         `json:"kind"`
				Fields map[string]any `json:"fields"`
				Event  struct {
					Type string `json:"type"`
				} `json:"event"`
			}
			if err := json.Unmarshal([]byte(line), &rec); err != nil {
				t.Fatalf("a journal line: %v: %s", err, line)
			}
			if rec.Event.Type == string(EventStepRetry) {
				events++
			}
			if rec.Kind == journal.DiagStepRetry {
				diags++
				f := rec.Fields
				if f["attempt"] != float64(1) || f["step"] != float64(2) || f["discarded"] != true || f["from"] != float64(info.From) {
					t.Fatalf("the journal's record: %s", line)
				}
			}
		}
	}
	if diags != 1 || events != 1 {
		t.Fatalf("%d step_retry notes and %d events in the journal; want one of each", diags, events)
	}
}

// TestNativeRetryThenSuccessKeepsTheAnswer: the budget that lets the attempt go
// again, and the row of the replacement under the id the abandoned call's
// provider used is a new one.
func TestNativeRetryThenSuccess(t *testing.T) {
	f := newNativeFixture(t)
	s := f.started(Options{})
	f.models["test/a"].push(
		reply(openTextParts("abandoned"), []fantasy.StreamPart{{Type: fantasy.StreamPartTypeToolInputStart, ID: "same", ToolCallName: "nope"}}, transientFail()),
		reply(nativeCallParts("same", "nope", `{}`), finishParts(fantasy.FinishReasonToolCalls)),
		reply(textParts("whole"), finishParts(fantasy.FinishReasonStop)),
	)
	res, err := s.Prompt(context.Background(), "go")
	if err != nil || res.StopReason != "end_turn" {
		t.Fatalf("Prompt = %+v, %v", res, err)
	}
	evs := drained(s)
	rows := rowsOf(evs)
	var ids []string
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	if !slices.Equal(ids, []string{"t1.1.1", "t1.1.2"}) {
		t.Fatalf("rows = %v; want the abandoned call's and the replacement's, apart", ids)
	}
	var snapIDs []string
	for _, row := range s.Snapshot().Tools {
		snapIDs = append(snapIDs, row.ID)
	}
	if !slices.Equal(snapIDs, []string{"t1.1.2"}) {
		t.Fatalf("the snapshot's tools = %v; want the replacement's alone", snapIDs)
	}
	lines := strings.Join(f.transcripts(), "\n")
	if strings.Contains(lines, "abandoned") {
		t.Fatalf("the transcript holds the failed attempt:\n%s", lines)
	}
}

// TestNativeRetryOfNothing is a request that failed before any output: the
// event counts it, and retracts nothing.
func TestNativeRetryOfNothing(t *testing.T) {
	f := newNativeFixture(t)
	s := f.started(Options{})
	f.models["test/a"].push(reply(transientFail()), reply(textParts("whole"), finishParts(fantasy.FinishReasonStop)))
	if _, err := s.Prompt(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	retries := ofType(drained(s), EventStepRetry)
	if len(retries) != 1 || retries[0].StepRetry.Discarded {
		t.Fatalf("step_retry = %+v; want one that discards nothing", retries)
	}
}

// TestNativeBudgetReachesTheHarness: Options.ProviderRetries is the turn's.
func TestNativeBudgetReachesTheHarness(t *testing.T) {
	f := newNativeFixture(t)
	none := 0
	s := f.started(Options{ProviderRetries: &none})
	f.models["test/a"].push(reply(openTextParts("half"), transientFail()), reply(textParts("never"), finishParts(fantasy.FinishReasonStop)))
	if _, err := s.Prompt(context.Background(), "go"); err == nil {
		t.Fatal("the turn succeeded")
	}
	if n := f.models["test/a"].callCount(); n != 1 {
		t.Fatalf("%d requests; want 1", n)
	}
	if retries := ofType(drained(s), EventStepRetry); len(retries) != 0 {
		t.Fatalf("step_retry = %+v", retries)
	}
}

// TestNativeChildRetryIsTheChilds: a sub-agent's attempt is retracted on its own
// stream, its Agent named, with From marking its own attempt: the parent's rows
// between are not in it, by the transcript they fold into.
func TestNativeChildRetryIsTheChilds(t *testing.T) {
	f, routers := routedNative(t)
	s := f.started(Options{})
	a := routers["test/a"]
	a.route("go", callsStep(agentCall(t, "c1", "child", "child runs")), reply(textParts("parent done"), finishParts(fantasy.FinishReasonStop)))
	a.route("child runs",
		reply(openTextParts("child's abandoned"), transientFail()),
		reply(textParts("child's whole"), finishParts(fantasy.FinishReasonStop)))
	if _, err := s.Prompt(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	evs := drained(s)
	retries := ofType(evs, EventStepRetry)
	if len(retries) != 1 || retries[0].Agent == "" || !retries[0].StepRetry.Discarded {
		t.Fatalf("step_retry = %+v; want the child's", retries)
	}
	r := retries[0]
	for _, ev := range evs {
		if ev.Type == EventText && ev.Agent == r.Agent && ev.Text == "child's abandoned" && (ev.Seq < r.StepRetry.From || ev.Seq >= r.Seq) {
			t.Fatalf("the child's attempt, seq %d, is outside [%d, %d)", ev.Seq, r.StepRetry.From, r.Seq)
		}
	}
	found := false
	for _, ev := range ofType(evs, EventSubagent) {
		if ev.SubagentChange == SubagentChangeFinished && ev.Subagent != nil {
			found = true
			if strings.Contains(ev.Subagent.Output, "abandoned") || !strings.Contains(ev.Subagent.Output, "whole") {
				t.Fatalf("the child's output: %q", ev.Subagent.Output)
			}
		}
	}
	if !found {
		t.Fatal("the child did not finish")
	}
}

// TestNativeASecondRetryStartsWhereTheFirstEnded: each attempt is its own: the
// second retry retracts from after the first's event, so the first attempt's rows,
// retracted once, are not named again, and a row of the note between stands.
func TestNativeASecondRetryStartsWhereTheFirstEnded(t *testing.T) {
	f := newNativeFixture(t)
	two := 2
	s := f.started(Options{ProviderRetries: &two})
	f.models["test/a"].push(
		reply(openTextParts("first try"), transientFail()),
		reply(openTextParts("second try"), transientFail()),
		reply(textParts("whole"), finishParts(fantasy.FinishReasonStop)),
	)
	if _, err := s.Prompt(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	evs := drained(s)
	retries := ofType(evs, EventStepRetry)
	if len(retries) != 2 {
		t.Fatalf("%d step_retry events; want 2", len(retries))
	}
	first, second := retries[0], retries[1]
	if want := first.Seq + 1; second.StepRetry.From != want {
		t.Fatalf("the second retry's From = %d; want %d, just past the first's event", second.StepRetry.From, want)
	}
	if second.StepRetry.Attempt != 2 || second.StepRetry.Total != 2 {
		t.Fatalf("the second retry: %+v", second.StepRetry)
	}
}

// heldOutbox holds the log's drainer until released: a usage report sits enqueued
// while the turn goes on, unless something waits for it. A Flush parking releases
// it, which is the barrier doing its work; release is the test's own last resort,
// from a step the barrier should have kept from starting.
func heldOutbox(s *nativeSession) (release func()) {
	gate := make(chan struct{})
	var once sync.Once
	release = func() { once.Do(func() { close(gate) }) }
	s.log.hooks = &logHooks{
		outboxAdmitting: func(int) { <-gate },
		flushParked:     func(uint64) { release() },
	}
	return release
}

func firstIndex(evs []Event, ok func(Event) bool) int { return slices.IndexFunc(evs, ok) }

func isUsageDelta(ev Event) bool {
	return ev.Type == EventMeta && ev.State != nil && ev.State.Usage != nil
}

// TestAStepsReportPrecedesTheNextStep forces the drainer late: step 1's usage
// sits enqueued as step 2 starts. Whether step 2 goes on to finish, or fails and is
// retried, its report is on the stream before anything of step 2, so it can never
// read as step 2's.
func TestAStepsReportPrecedesTheNextStep(t *testing.T) {
	for _, tc := range []struct {
		name string
		fail bool
	}{{"no retry", false}, {"a retry", true}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newNativeFixture(t)
			ws := nativeWorkspaceWith(t, map[string]string{"main.go": "package main\n"})
			s := f.started(Options{Workspace: ws})
			release := heldOutbox(s)
			defer release()
			// Without the barrier, step 2's stream starting is what lets the
			// report go, after the step's first event.
			second := func(parts ...[]fantasy.StreamPart) step {
				inner := reply(parts...)
				return func(ctx context.Context, yield func(fantasy.StreamPart) bool) {
					inner(ctx, yield)
					release()
				}
			}
			m := f.models["test/a"]
			m.push(nativeCallStep("c1", "read", nativeArgs(t, map[string]any{"filePath": filepath.Join(ws, "main.go")})))
			if tc.fail {
				m.push(second(textParts("step two"), transientFail()), reply(textParts("whole"), finishParts(fantasy.FinishReasonStop)))
			} else {
				m.push(second(textParts("step two"), finishParts(fantasy.FinishReasonStop)))
			}
			if _, err := s.Prompt(context.Background(), "go"); err != nil {
				t.Fatal(err)
			}
			evs := drained(s)
			report := firstIndex(evs, isUsageDelta)
			text := firstIndex(evs, func(ev Event) bool { return ev.Type == EventText && ev.Text == "step two" })
			if report < 0 || text < 0 || report > text {
				t.Fatalf("step 1's report at %d, step 2's text at %d; want the report first:\n%s", report, text, strings.Join(loadLines(evs), "\n"))
			}
		})
	}
}
