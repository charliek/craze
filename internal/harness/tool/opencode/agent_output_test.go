package opencode

import (
	"context"
	"maps"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charliek/craze/internal/harness/redact"
	"github.com/charliek/craze/internal/harness/tool"
)

// The agent_output tool (plan 026 §3.11). As for the agent tool, the runner
// that answers it is the harness's, so these tests build a dispatcher over the
// tool alone, with a fake runner behind Env.Subagents.

// fakeOutputs is a runner that records every agent_output call it is handed
// and answers each with res.
type fakeOutputs struct {
	res tool.Result

	mu    sync.Mutex
	calls []tool.OutputCall
}

func (f *fakeOutputs) Run(context.Context, tool.SubagentCall) tool.Result { return tool.Result{} }

func (f *fakeOutputs) Output(_ context.Context, c tool.OutputCall) tool.Result {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, c)
	return f.res
}

func (f *fakeOutputs) seen() []tool.OutputCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// newOutputFixture is a session's worth of the agent_output tool alone.
func newOutputFixture(t *testing.T, sub tool.Subagents) *fixture {
	t.Helper()
	env := tool.Env{
		Workspace: t.TempDir(),
		Home:      t.TempDir(),
		Redactor:  redact.New(keyA),
		Environ:   tool.ChildEnviron(os.Environ(), nil),
		Locks:     &tool.PathLocks{},
	}
	if sub != nil {
		env.Subagents = sub
	}
	o, err := newAgentOutput()
	if err != nil {
		t.Fatal(err)
	}
	d, err := tool.NewDispatcher(tool.Options{Tools: []tool.Tool{o}, Env: env})
	if err != nil {
		t.Fatalf("the agent_output tool does not pass the dispatcher's checks: %v", err)
	}
	return &fixture{env: env, d: d}
}

// TestAgentOutputSpec: the tool's contract — id agent_output, kind read,
// read-only, parallel, never cut by the dispatcher (the runner cut the result
// when it became deliverable); id required, a string, and wait_ms an optional
// integer. Its description is rendered, craze's own and names only tools the
// harness has; the profile offers it right after agent.
func TestAgentOutputSpec(t *testing.T) {
	o, err := newAgentOutput()
	if err != nil {
		t.Fatal(err)
	}
	s := o.Spec()
	if tool.AgentOutputTool != "agent_output" {
		t.Fatalf("tool.AgentOutputTool = %q", tool.AgentOutputTool)
	}
	if s.ID != tool.AgentOutputTool || s.Kind != tool.KindRead || !s.ReadOnly || !s.Parallel || s.Truncate != tool.None {
		t.Fatalf("spec = id %q kind %q readOnly %v parallel %v truncate %d; want agent_output, read, read-only, parallel, none",
			s.ID, s.Kind, s.ReadOnly, s.Parallel, s.Truncate)
	}
	if !slices.Equal(s.Required, []string{"id"}) {
		t.Fatalf("required = %q, want id", s.Required)
	}
	if params := slices.Sorted(maps.Keys(s.Parameters)); !slices.Equal(params, []string{"id", "wait_ms"}) {
		t.Fatalf("parameters = %q; want exactly id and wait_ms", params)
	}
	if typ := s.Parameters["id"].(map[string]any)["type"]; typ != "string" {
		t.Errorf("id is a %v, want a string", typ)
	}
	if typ := s.Parameters["wait_ms"].(map[string]any)["type"]; typ != "integer" {
		t.Errorf("wait_ms is a %v, want an integer", typ)
	}
	d := s.Description
	for _, says := range []string{"run_in_background", "delivered to you on its own", "wait_ms", "A result is delivered once."} {
		if !strings.Contains(d, says) {
			t.Errorf("the description does not say %q", says)
		}
	}
	if strings.Contains(d, "${") || !strings.HasSuffix(d, "\n") || strings.HasSuffix(d, "\n\n") {
		t.Errorf("want a rendered description with one final newline:\n%q", d)
	}
	for _, absent := range []string{"Task tool", "TodoWrite", "WebFetch", "opencode", "TaskOutput"} {
		if strings.Contains(d, absent) {
			t.Errorf("the description mentions %q", absent)
		}
	}
	p, err := Profile()
	if err != nil {
		t.Fatal(err)
	}
	if got := names(p); slices.Index(got, "agent_output") != slices.Index(got, "agent")+1 || slices.Index(got, "agent") < 0 {
		t.Fatalf("the profile offers %q; want agent_output right after agent", got)
	}
}

// TestAgentOutputPrepare: the runner is handed the call's id, trimmed, and its
// wait — 30 s when wait_ms is absent or null, 0 for a negative, 600 s at most —
// under the call's own harness id; a missing, blank or non-string id and a
// wait_ms that is not an integer are invalid_input naming the field, and never
// reach the runner.
func TestAgentOutputPrepare(t *testing.T) {
	sub := &fakeOutputs{res: tool.Result{Text: "the result"}}
	f := newOutputFixture(t, sub)
	for _, tc := range []struct {
		input string
		wait  time.Duration
	}{
		{`{"id":" kid-1 "}`, 30 * time.Second},
		{`{"id":"kid-1","wait_ms":null}`, 30 * time.Second},
		{`{"id":"kid-1","wait_ms":0}`, 0},
		{`{"id":"kid-1","wait_ms":-5}`, 0},
		{`{"id":"kid-1","wait_ms":1500}`, 1500 * time.Millisecond},
		{`{"id":"kid-1","wait_ms":600000}`, 600 * time.Second},
		{`{"id":"kid-1","wait_ms":9000000}`, 600 * time.Second},
	} {
		n := len(sub.seen())
		req, res := f.call(t, "agent_output", tc.input)
		if ok(t, res) != "the result" {
			t.Fatalf("%s: result %q, want the runner's", tc.input, res.Text)
		}
		// The row's title says what the read is of (plan 026 X45): the TUI
		// labels a row by the tool's kind, so a bare id read `read <uuid>`.
		if req.Title != "sub-agent result kid-1" {
			t.Fatalf("%s: the request's title is %q, want the child named", tc.input, req.Title)
		}
		got := sub.seen()[n]
		if want := (tool.OutputCall{CallID: got.CallID, ID: "kid-1", Wait: tc.wait}); got != want || !strings.HasPrefix(got.CallID, "t1.1.") {
			t.Fatalf("%s: the runner was handed %+v, want %+v under the call's harness id", tc.input, got, want)
		}
	}
	n := len(sub.seen())
	for _, tc := range []struct{ input, field string }{
		{`{}`, "id is required"},
		{`{"id":"  "}`, "id must not be empty"},
		{`{"id":5}`, "id must be a string"},
		{`{"id":"k","wait_ms":"10"}`, "wait_ms must be an integer"},
		{`{"id":"k","wait_ms":1.5}`, "wait_ms must be an integer"},
		{`["k"]`, "JSON object"},
	} {
		_, res := f.call(t, "agent_output", tc.input)
		if !res.IsError || res.Class != tool.ClassInvalidInput || !strings.Contains(res.Text, tc.field) {
			t.Errorf("%s: result %+v, want invalid_input naming %q", tc.input, res, tc.field)
		}
	}
	if got := len(sub.seen()); got != n {
		t.Fatalf("the runner was handed %d refused calls", got-n)
	}
}

// TestAgentOutputWithoutSubagents: a session with no runner answers with a
// tool_error saying so; a cancelled call is aborted before it reaches one.
func TestAgentOutputWithoutSubagents(t *testing.T) {
	f := newOutputFixture(t, nil)
	_, res := f.call(t, "agent_output", `{"id":"k"}`)
	failed(t, res, tool.ClassToolError, "Sub-agents are not available in this session.")

	sub := &fakeOutputs{res: tool.Result{Text: "unreachable"}}
	g := newOutputFixture(t, sub)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, res = g.callCtx(t, ctx, "agent_output", `{"id":"k"}`)
	if !res.IsError || res.Class != tool.ClassAborted || len(sub.seen()) != 0 {
		t.Fatalf("a cancelled call = %+v, runner handed %d; want aborted, nothing handed", res, len(sub.seen()))
	}
}
