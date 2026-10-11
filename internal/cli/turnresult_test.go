package cli

import (
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/rundir"
)

// TestAccumulatorMessageIsTheLastAssistantEntry: the message is the last run
// of the main session's text, split where the transcript splits assistant
// entries — a tool, a thought, a plan, an interjection, an ask's opening —
// never at a sub-agent's event or an empty thought; a sub-agent's text is
// never the message.
func TestAccumulatorMessageIsTheLastAssistantEntry(t *testing.T) {
	text := func(s string) agent.Event { return agent.Event{Type: agent.EventText, Text: s} }
	for _, c := range []struct {
		name  string
		split agent.Event
		want  string
	}{
		{"a tool", agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: "t1", Status: "completed"}}, "after"},
		{"a thought", agent.Event{Type: agent.EventThought, Text: "hmm"}, "after"},
		{"a plan", agent.Event{Type: agent.EventPlan, Plan: &agent.PlanEvent{ID: "plan-1", Plan: "p"}}, "after"},
		{"an interjection", agent.Event{Type: agent.EventUser, Text: "x", Interjection: true}, "after"},
		{"a permission", agent.Event{Type: agent.EventPermission, Permission: &agent.PermissionEvent{ID: "perm-1"}}, "after"},
		{"the wire's done", agent.Event{Type: agent.EventDone, StopReason: "end_turn"}, "after"},
		{"an empty thought", agent.Event{Type: agent.EventThought}, "before after"},
		{"a sub-agent's tool", agent.Event{Type: agent.EventTool, Agent: "child-1", Tool: &agent.ToolEvent{ID: "t2"}}, "before after"},
		{"cursor's todo writer", agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: "t3", ToolName: "updateTodos"}}, "before after"},
		{"a queue change", agent.Event{Type: agent.EventQueue, Queue: &agent.QueuedPrompt{ID: "q-1"}, QueueChange: agent.QueueQueued}, "before after"},
	} {
		a := newTurnAccumulator()
		a.fold(text("before "))
		a.fold(c.split)
		a.fold(agent.Event{Type: agent.EventText, Agent: "child-1", Text: "the child's"})
		a.fold(text("after"))
		if got, cut := a.message(); got != c.want || cut {
			t.Errorf("%s: message %q (cut %v), want %q", c.name, got, cut, c.want)
		}
	}
	a := newTurnAccumulator()
	a.fold(agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: "t1"}})
	if got, _ := a.message(); got != "" {
		t.Errorf("a turn that wrote nothing has message %q", got)
	}
}

// TestAccumulatorKeepsTheMessagesTail: past resultMessageMax the message is
// its tail, cut at a rune's start, and messageCut says so; a new entry starts
// whole again.
func TestAccumulatorKeepsTheMessagesTail(t *testing.T) {
	a := newTurnAccumulator()
	a.fold(agent.Event{Type: agent.EventText, Text: "é" + strings.Repeat("x", resultMessageMax)})
	a.fold(agent.Event{Type: agent.EventText, Text: "END"})
	got, cut := a.message()
	if !cut || len(got) > resultMessageMax || !strings.HasSuffix(got, "END") || !strings.HasPrefix(got, "x") {
		t.Fatalf("the long message is %d bytes, cut %v, ends %q", len(got), cut, got[len(got)-3:])
	}
	a.fold(agent.Event{Type: agent.EventThought, Text: "t"})
	a.fold(agent.Event{Type: agent.EventText, Text: "short"})
	if got, cut := a.message(); got != "short" || cut {
		t.Fatalf("the next entry is %q, cut %v", got, cut)
	}
}

// TestAccumulatorToolsFilesAndAsks: toolCalls counts distinct ids that
// finished (completed or failed); files are the edit-kind calls' paths in
// first-touch order, a kind learnt from a later update included; the plan is
// the last presented; automatic answers are listed, a sub-agent's tools are
// not counted.
func TestAccumulatorToolsFilesAndAsks(t *testing.T) {
	a := newTurnAccumulator()
	for _, ev := range []agent.Event{
		{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: "e1", Status: "pending", Locations: []string{"b.go"}}},
		{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: "r1", Kind: "read", Status: "completed", Locations: []string{"z.go"}}},
		{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: "e1", Kind: "edit", Status: "completed", Diffs: []agent.ToolDiff{{Path: "a.go"}, {Path: "b.go"}}}},
		{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: "x1", Kind: "execute", Status: "failed"}},
		{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: "x2", Kind: "execute", Status: "in_progress"}},
		{Type: agent.EventTool, Agent: "child", Tool: &agent.ToolEvent{ID: "c1", Kind: "edit", Status: "completed", Locations: []string{"child.go"}}},
		{Type: agent.EventQuestion, Question: &agent.QuestionEvent{ID: "ask-1", Auto: true, Questions: []agent.Question{{ID: "q1", Prompt: "Pick one",
			Options: []agent.Option{{ID: "a", Label: "A"}, {ID: "b", Label: "B"}}}}, Answers: map[string][]string{"q1": {"a"}}}},
		{Type: agent.EventPlan, Plan: &agent.PlanEvent{ID: "plan-1", Name: "Refactor", Plan: "1. do it", Auto: true, Accepted: true}},
	} {
		a.fold(ev)
	}
	if n := a.toolCalls(); n != 3 {
		t.Errorf("toolCalls = %d, want 3 (e1, r1, x1)", n)
	}
	if got := a.files(); !slices.Equal(got, []string{"b.go", "a.go"}) {
		t.Errorf("files = %q, want [b.go a.go]", got)
	}
	if a.plan != "1. do it" {
		t.Errorf("plan = %q", a.plan)
	}
	want := []autoAnswer{{Kind: "question", Summary: "Pick one", Choice: "A"}, {Kind: "plan", Summary: "Refactor", Choice: "accepted"}}
	if !slices.Equal(a.auto, want) {
		t.Errorf("autoAnswered = %+v, want %+v", a.auto, want)
	}
}

// TestAccumulatorUsageIsTheLastFolded: the usage is the last section folded,
// and the duration runs from the start to the end.
func TestAccumulatorUsageIsTheLastFolded(t *testing.T) {
	a := newTurnAccumulator()
	t0 := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	a.start(t0)
	for _, n := range []int64{1, 2} {
		a.fold(agent.Event{Type: agent.EventMeta, State: &agent.StateDelta{Usage: &agent.UsageState{ContextTokens: n}}})
	}
	a.fold(agent.Event{Type: agent.EventMeta, State: &agent.StateDelta{Title: new(string)}})
	a.end(t0.Add(2500 * time.Millisecond))
	if a.usage == nil || a.usage.ContextTokens != 2 || a.durationMs() != 2500 {
		t.Fatalf("usage %+v, duration %d", a.usage, a.durationMs())
	}
}

// TestTurnCostAndFooter: the footer's cost is the turn's — to the cent,
// `<$0.01` under half a cent, `+` with unpriced usage in it, `unpriced` with
// nothing priced — and its context is tokens over the window.
func TestTurnCostAndFooter(t *testing.T) {
	for _, c := range []struct {
		s    agent.Spend
		want string
	}{
		{agent.Spend{CostPicoUSD: 40_000_000_000}, "$0.04"},
		{agent.Spend{CostPicoUSD: 1}, "<$0.01"},
		{agent.Spend{CostPicoUSD: 0}, "$0.00"},
		{agent.Spend{CostPicoUSD: 1_234_567_000_000_000}, "$1234.57"},
		{agent.Spend{CostPicoUSD: 40_000_000_000, Unpriced: true}, "$0.04+"},
		{agent.Spend{Unpriced: true}, "unpriced"},
	} {
		if got := turnCost(c.s); got != c.want {
			t.Errorf("turnCost(%+v) = %q, want %q", c.s, got, c.want)
		}
	}
	res := newResult(resultSession{id: "0199c0de-1111-7000-8000-00000000a001", provider: "native", model: "m"}, statusCompleted)
	a := newTurnAccumulator()
	a.start(time.Unix(0, 0))
	a.end(time.Unix(12, 0))
	a.fold(agent.Event{Type: agent.EventText, Text: "done"})
	a.fold(agent.Event{Type: agent.EventMeta, State: &agent.StateDelta{Usage: &agent.UsageState{ContextTokens: 16590,
		ContextWindow: 272000, Turn: agent.Spend{Unpriced: true}}}})
	res.fill(a)
	want := "done\n---\ncompleted · 12s · 0 tool calls · unpriced · context 16.6k/272k\n" +
		"session 0000a001 · craze attach --session 0000a001\n"
	if got := res.plain(); got != want {
		t.Fatalf("plain:\n%s\nwant:\n%s", got, want)
	}
}

// TestCutMessage: --max-message counts characters, not bytes, and leaves a
// message that fits alone.
func TestCutMessage(t *testing.T) {
	if got, cut := cutMessage("héllo wörld", 5); got != "héllo" || !cut {
		t.Fatalf("cut to 5: %q %v", got, cut)
	}
	if got, cut := cutMessage("héllo", 5); got != "héllo" || cut {
		t.Fatalf("a message of 5 cut to 5: %q %v", got, cut)
	}
	if got, cut := cutMessage("anything", 0); got != "anything" || cut {
		t.Fatalf("no limit: %q %v", got, cut)
	}
}

// TestCutResult (C3 r3, 4; C8, 2): --max-message's budget over the message
// and the plan, the message first; the plan gets what is left — back to the
// end of its last whole line when one ends within it, else cut at a
// character — and none when nothing is left. The two together are never
// more than the budget: no first-line allowance (X29, amended).
func TestCutResult(t *testing.T) {
	const plan = "## Steps\n1. one\n2. two"
	long := strings.Repeat("p", 20000)
	for _, c := range []struct {
		name, msg, plan string
		n               int
		msgHead, head   string
		cut             bool
	}{
		{"both fit", "0123456789", plan, 40, "0123456789", plan, false},
		{"both fit exactly", "0123456789", plan, 10 + utf8.RuneCountInString(plan), "0123456789", plan, false},
		{"the message cut, no plan", "0123456789", plan, 5, "01234", "", true},
		{"the message whole, nothing left", "0123456789", plan, 10, "0123456789", "", true},
		{"what is left, within the first line", "0123456789", plan, 12, "0123456789", "##", true},
		{"what is left, back to a line's end", "ab", plan, 15, "ab", "## Steps", true},
		{"what is left, back to the last line's end", "", plan, 20, "", "## Steps\n1. one", true},
		{"a first line longer than the budget", "", "0123456789abcdef", 10, "", "0123456789", true},
		{"no plan", "0123456789", "", 5, "01234", "", true},
		{"characters, not bytes", "héllo", "wörld\nmore", 7, "héllo", "wö", true},
		{"the review's case", strings.Repeat("m", 19999), long, 20000, strings.Repeat("m", 19999), "p", true},
	} {
		msgHead, head, cut := cutResult(c.msg, c.plan, c.n)
		if msgHead != c.msgHead || head != c.head || cut != c.cut {
			t.Errorf("%s: %q %q %v, want %q %q %v", c.name, msgHead, head, cut, c.msgHead, c.head, c.cut)
		}
		if n := utf8.RuneCountInString(msgHead) + utf8.RuneCountInString(head); n > c.n {
			t.Errorf("%s: %d characters kept, over the budget of %d", c.name, n, c.n)
		}
	}
}

// TestTheResultsPathIsTheResolvedOne (C10): with HOME reached through a
// symlink — macOS's temp directories, /var → /private/var, the condition
// under which the --max-message tests failed there — craze writes, and
// names, a cut result's file under its HOME resolved; resultsPath, which
// every test comparing such a path builds its expectation with, is that
// path, and a join on the unresolved HOME is not.
func TestTheResultsPathIsTheResolvedOne(t *testing.T) {
	target := t.TempDir()
	if err := os.Chmod(target, 0o700); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(t.TempDir(), "home")
	if err := os.Symlink(target, home); err != nil {
		t.Fatal(err)
	}
	env := rundir.Env{Home: home, EUID: os.Geteuid()}
	res := newResult(resultSession{id: "0199c0de-1111-7000-8000-00000000a001", provider: "native", model: "m"}, statusCompleted)
	res.Turn, res.inc = "turn-1", "0199c0de-2222-7000-8000-00000000b001"
	a := newTurnAccumulator()
	a.fold(agent.Event{Type: agent.EventText, Text: "0123456789abcdefghij"})
	res.fill(a)
	var out, errs strings.Builder
	if err := writeResult(&out, &errs, resultOutput{what: waitWhat, maxMessage: 10, env: env}, res); err != nil {
		t.Fatalf("writeResult: %v, %q", err, errs.String())
	}
	const name = "0000a001-0000b001-turn-1.md"
	if want := resultsPath(t, env, name); res.MessageFile != want || !strings.Contains(out.String(), "[… cut: full result in "+want+"]") {
		t.Fatalf("the cut result's file is %q, printed %q; want %q", res.MessageFile, out.String(), want)
	}
	if naive := filepath.Join(home, ".cache", "craze", "results", name); res.MessageFile == naive {
		t.Fatalf("the file is named through the unresolved HOME %s: the symlink does not reproduce the macOS condition", naive)
	}
}

// TestAReloadsResultIsAFileOfItsOwn (C8, 1): turn ids restart at turn-1 with
// every incarnation, and a reloaded session keeps its short id; so two
// results for turn-1 of two incarnations of one session, both cut, are
// written to two files — named for the incarnation's last 8 characters, its
// random part — each holding its own result.
func TestAReloadsResultIsAFileOfItsOwn(t *testing.T) {
	home := t.TempDir()
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	o := resultOutput{what: waitWhat, maxMessage: 4, env: rundir.Env{Home: home, EUID: os.Geteuid()}}
	s := resultSession{id: "0199c0de-1111-7000-8000-00000000a001", provider: "native", model: "m"}
	paths := map[string]string{}
	for _, inc := range []string{"0199c0de-2222-7000-8000-00000000b001", "0199c0de-2222-7000-8000-00000000b002"} {
		res := newResult(s, statusCompleted)
		res.Turn, res.inc = "turn-1", inc
		a := newTurnAccumulator()
		a.fold(agent.Event{Type: agent.EventText, Text: "the answer of " + inc})
		res.fill(a)
		var out, errs strings.Builder
		if err := writeResult(&out, &errs, o, res); err != nil || res.MessageFile == "" {
			t.Fatalf("incarnation %s: %v, %q, file %q", inc, err, errs.String(), res.MessageFile)
		}
		if want := "0000a001-" + inc[len(inc)-8:] + "-turn-1.md"; filepath.Base(res.MessageFile) != want {
			t.Fatalf("incarnation %s's file: %s, want %s", inc, res.MessageFile, want)
		}
		paths[inc] = res.MessageFile
	}
	if len(paths) != 2 || paths["0199c0de-2222-7000-8000-00000000b001"] == paths["0199c0de-2222-7000-8000-00000000b002"] {
		t.Fatalf("two incarnations' turn-1 share a file: %v", paths)
	}
	for inc, path := range paths {
		if b, err := os.ReadFile(path); err != nil || !strings.HasPrefix(string(b), "the answer of "+inc+"\n") {
			t.Fatalf("incarnation %s's file holds %q, %v", inc, b, err)
		}
	}
}

// TestResultJSONKeys is §3.1's JSON keys, types and omission rules: the
// always-present keys; a terminal result's message, toolCalls, durationMs
// and files ([] when none); nothing absent written as null or false; a
// running result with no terminal facts and a handle.
func TestResultJSONKeys(t *testing.T) {
	s := resultSession{id: "0199c0de-1111-7000-8000-00000000a001", provider: "grok", model: "grok-4.7"}
	done := newResult(s, statusCompleted)
	done.Turn = "turn-2"
	done.fill(newTurnAccumulator())
	got := jsonKeys(t, done)
	want := map[string]string{"status": "string", "session": "string", "short": "string", "provider": "string",
		"model": "string", "attach": "string", "turn": "string", "message": "string", "toolCalls": "number",
		"durationMs": "number", "files": "array"}
	if !maps.Equal(got, want) {
		t.Errorf("a completed result's keys: %v, want %v", got, want)
	}
	running := newResult(s, statusRunning)
	running.Row, running.Handle, running.Partial = "q-1", "inc.3.row-q-1", true
	running.fill(newTurnAccumulator())
	got = jsonKeys(t, running)
	want = map[string]string{"status": "string", "session": "string", "short": "string", "provider": "string",
		"model": "string", "attach": "string", "row": "string", "handle": "string", "partial": "bool"}
	if !maps.Equal(got, want) {
		t.Errorf("a running result's keys: %v, want %v", got, want)
	}
}

// jsonKeys is a value's JSON object's keys and their JSON types.
func jsonKeys(t *testing.T, v any) map[string]string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for k, v := range m {
		switch v.(type) {
		case string:
			out[k] = "string"
		case float64:
			out[k] = "number"
		case bool:
			out[k] = "bool"
		case []any:
			out[k] = "array"
		case map[string]any:
			out[k] = "object"
		case nil:
			out[k] = "null"
		}
	}
	return out
}

// TestStatusExit is §3.1's table.
func TestStatusExit(t *testing.T) {
	for status, want := range map[string]int{statusCompleted: 0, statusIdle: 0, statusFailed: 3, statusCancelled: 3,
		statusDropped: 3, statusNeedsInput: 4, statusRunning: 5} {
		if got := statusExit(status); got != want {
			t.Errorf("statusExit(%s) = %d, want %d", status, got, want)
		}
	}
	err := writeResult(&strings.Builder{}, &strings.Builder{}, resultOutput{what: waitWhat}, newResult(resultSession{id: "x"}, statusNeedsInput))
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 4 || ee.msg != "" {
		t.Fatalf("a needs_input result's exit: %v", err)
	}
}

// TestUnsavedWhyIsOneShortLine pins the unsaved-result marker's reason: one
// line, never more than unsavedWhyMax characters with its ellipsis (C8 review
// round 2), and a short reason left whole.
func TestUnsavedWhyIsOneShortLine(t *testing.T) {
	long := errors.New("open /home/" + strings.Repeat("é", 300) + "/.cache/craze/results:\nnot a directory")
	got := unsavedWhy(long)
	if n := len([]rune(got)); n > unsavedWhyMax {
		t.Fatalf("the reason is %d characters, over %d: %q", n, unsavedWhyMax, got)
	}
	if !strings.HasSuffix(got, "…") || strings.Contains(got, "\n") {
		t.Fatalf("a cut reason is one line ending in an ellipsis: %q", got)
	}
	if got := unsavedWhy(errors.New("disk full")); got != "disk full" {
		t.Fatalf("a short reason is %q, want it whole", got)
	}
}
