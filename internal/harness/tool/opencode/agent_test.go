package opencode

import (
	"context"
	"maps"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/charliek/craze/internal/harness/redact"
	"github.com/charliek/craze/internal/harness/tool"
)

// The agent tool (plan 026 §3.3, §7 A5). It is built and tested here but the
// profile does not register it until the runner that answers it exists
// (C3b), so these tests build a dispatcher over it alone, with a fake runner
// behind Env.Subagents.

// fakeSubagents is a runner that records every call it is handed and answers
// each with res.
type fakeSubagents struct {
	res tool.Result

	mu    sync.Mutex
	calls []tool.SubagentCall
	live  []bool // whether each call's ctx was still live when it arrived
}

func (f *fakeSubagents) Run(ctx context.Context, c tool.SubagentCall) tool.Result {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, c)
	f.live = append(f.live, ctx.Err() == nil)
	return f.res
}

func (f *fakeSubagents) seen() []tool.SubagentCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// newAgentFixture is a session's worth of the agent tool alone: a dispatcher
// over it whose Env hands calls to sub (nil is a session with no runner).
func newAgentFixture(t *testing.T, sub tool.Subagents) *fixture {
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
	a, err := newAgent()
	if err != nil {
		t.Fatal(err)
	}
	d, err := tool.NewDispatcher(tool.Options{Tools: []tool.Tool{a}, Env: env})
	if err != nil {
		t.Fatalf("the agent tool does not pass the dispatcher's checks: %v", err)
	}
	return &fixture{env: env, d: d}
}

// TestAgentSpec (A5): the tool's contract — id agent, kind task, parallel, not
// read-only (a child may edit), head truncation; description and prompt
// required, and subagent_type, model and effort optional strings, with no
// run_in_background declared until background children exist (plan 026 PR 3).
// Its description opens with the alias sentence, says the five things §3.3
// lists, renders with nothing left to fill, ends in one newline like every
// other, and names no tool the harness lacks. The profile does not offer it
// yet (C3a), which is what keeps specs.golden where it is.
func TestAgentSpec(t *testing.T) {
	a, err := newAgent()
	if err != nil {
		t.Fatal(err)
	}
	s := a.Spec()
	if s.ID != "agent" || s.Kind != tool.KindTask || !s.Parallel || s.ReadOnly || s.Truncate != tool.Head {
		t.Fatalf("spec = id %q kind %q parallel %v readOnly %v truncate %d; want agent, task, parallel, not read-only, head",
			s.ID, s.Kind, s.Parallel, s.ReadOnly, s.Truncate)
	}
	if !slices.Equal(s.Required, []string{"description", "prompt"}) {
		t.Fatalf("required = %q, want description and prompt", s.Required)
	}
	params := slices.Sorted(maps.Keys(s.Parameters))
	if !slices.Equal(params, []string{"description", "effort", "model", "prompt", "subagent_type"}) {
		t.Fatalf("parameters = %q; want exactly description, prompt, subagent_type, model and effort", params)
	}
	for name, p := range s.Parameters {
		if typ := p.(map[string]any)["type"]; typ != "string" {
			t.Errorf("%s is a %v, want a string", name, typ)
		}
	}

	const alias = "Claude Code calls this tool `Agent` (formerly `Task`); grok calls it `task`.\n"
	d := s.Description
	if !strings.HasPrefix(d, alias) {
		t.Fatalf("the description does not open with the alias sentence:\n%s", d)
	}
	for _, says := range []string{
		"Delegate work that is self-contained",                     // when to delegate
		"Several agent calls in one message run in parallel.",      // parallel
		"The sub-agent cannot see this conversation.",              // self-contained prompt
		"The result is the sub-agent's final message.",             // the result
		"Do not use it to read a single file or to run one search", // not for a single read
		"listed below", // the per-session tail follows
	} {
		if !strings.Contains(d, says) {
			t.Errorf("the description does not say %q", says)
		}
	}
	if strings.Contains(d, "${") || !strings.HasSuffix(d, "\n") || strings.HasSuffix(d, "\n\n") {
		t.Errorf("want a rendered description with one final newline:\n%q", d)
	}
	for _, absent := range []string{"Task tool", "TodoWrite", "WebFetch", "opencode"} {
		if strings.Contains(d, absent) {
			t.Errorf("the description mentions %q", absent)
		}
	}

	p, err := Profile()
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(names(p), "agent") {
		t.Fatal("the profile offers the agent tool before a runner exists to answer it")
	}
}

// TestRunInBackgroundTolerated (A5): Claude Code's run_in_background, which the
// tool does not declare, is accepted like any unknown key and the call runs in
// the foreground: the runner is handed the call at once, on the call's live
// context, and its result is the call's — its usage included, which the
// dispatcher carries through. The same holds with the key false, absent, or
// beside another unknown one. The call the runner gets is the model's
// arguments, trimmed, with the default type filled in; an explicit null for an
// optional field reads as an omission.
func TestRunInBackgroundTolerated(t *testing.T) {
	child := &tool.ChildUsage{Provider: "test", Model: "test/b", WireModel: "wire-b", Usage: tool.Usage{Input: 120, Output: 30}}
	sub := &fakeSubagents{res: tool.Result{Text: "the child's final message", Child: child}}
	f := newAgentFixture(t, sub)

	inputs := []string{
		`{"description":"scan the repo","prompt":"List the packages.","run_in_background":true}`,
		`{"description":"scan the repo","prompt":"List the packages.","run_in_background":false,"extra":{"x":1}}`,
		`{"description":"  scan the repo ","prompt":"List the packages.","subagent_type":null,"model":null,"effort":null}`,
	}
	for i, in := range inputs {
		_, res := f.call(t, "agent", in)
		if got := ok(t, res); got != "the child's final message" {
			t.Fatalf("input %d: result %q, want the runner's", i, got)
		}
		if res.Child == nil || *res.Child != *child {
			t.Fatalf("input %d: the child's usage came back as %+v, want %+v", i, res.Child, child)
		}
	}
	calls := sub.seen()
	if len(calls) != len(inputs) || slices.Contains(sub.live, false) {
		t.Fatalf("the runner was handed %d calls (live %v), want %d, each on a live context", len(calls), sub.live, len(inputs))
	}
	for i, c := range calls {
		want := tool.SubagentCall{ID: calls[i].ID, Description: "scan the repo", Prompt: "List the packages.", Type: tool.DefaultAgentType}
		if c != want || c.ID == "" {
			t.Fatalf("call %d reached the runner as %+v, want %+v", i, c, want)
		}
	}
	if calls[0].ID == calls[1].ID {
		t.Fatal("two calls reached the runner under one harness id")
	}

	// Every field as the model sent it, the prompt verbatim.
	_, res := f.call(t, "agent", map[string]any{
		"description": "review", "prompt": "  Review the diff.\n", "subagent_type": " Explore ", "model": "haiku", "effort": " low",
	})
	ok(t, res)
	got := sub.seen()[len(inputs)]
	if want := (tool.SubagentCall{ID: got.ID, Description: "review", Prompt: "  Review the diff.\n", Type: "Explore", Model: "haiku", Effort: "low"}); got != want {
		t.Fatalf("the call reached the runner as %+v, want %+v", got, want)
	}
}

// TestAgentPrepare: what Prepare refuses — a missing or blank description or
// prompt, and an optional field of the wrong type — is invalid_input naming
// the field, and never reaches the runner; the controls are the accepted calls
// of TestRunInBackgroundTolerated.
func TestAgentPrepare(t *testing.T) {
	sub := &fakeSubagents{res: tool.Result{Text: "unreachable"}}
	f := newAgentFixture(t, sub)
	for _, tc := range []struct{ input, field string }{
		{`{"prompt":"p"}`, "description"},
		{`{"description":"d"}`, "prompt"},
		{`{"description":" \n","prompt":"p"}`, "description must not be empty"},
		{`{"description":"d","prompt":"  "}`, "prompt must not be empty"},
		{`{"description":5,"prompt":"p"}`, "description"},
		{`{"description":"d","prompt":"p","subagent_type":5}`, "subagent_type"},
		{`{"description":"d","prompt":"p","model":true}`, "model"},
		{`{"description":"d","prompt":"p","effort":["high"]}`, "effort"},
		{`["d","p"]`, "JSON object"},
	} {
		_, res := f.call(t, "agent", tc.input)
		if !res.IsError || res.Class != tool.ClassInvalidInput || !strings.Contains(res.Text, tc.field) {
			t.Errorf("%s: result %+v, want invalid_input naming %q", tc.input, res, tc.field)
		}
	}
	if n := len(sub.seen()); n != 0 {
		t.Fatalf("the runner was handed %d refused calls", n)
	}
}

// TestAgentWithoutSubagents: a session with no runner — a sub-agent's own, or
// a build that wired none — answers an agent call with a tool_error saying so,
// rather than panicking on the nil seam.
func TestAgentWithoutSubagents(t *testing.T) {
	f := newAgentFixture(t, nil)
	_, res := f.call(t, "agent", `{"description":"d","prompt":"p"}`)
	failed(t, res, tool.ClassToolError, "Sub-agents are not available in this session.")
}
