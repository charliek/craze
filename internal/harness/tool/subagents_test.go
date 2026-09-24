package tool

import (
	"context"
	"slices"
	"strings"
	"testing"
)

// The tool framework's half of sub-agents (plan 026 §3.3): the task kind, and
// the gate's rule for the agent tool. The tool itself is opencode's
// (opencode/agent_test.go); the persona tool-name map is tested with the
// harness's half of what it feeds (TestPersonaToolMapping).

// TestKindTaskIsValid: a task-kind spec registers, and a kind nobody declared
// still does not — the control that the check is still a check.
func TestKindTaskIsValid(t *testing.T) {
	spec := func(k Kind) Tool {
		f := newFake(AgentTool, nil)
		f.spec.Kind, f.spec.ReadOnly = k, false
		return f
	}
	if _, err := validateTools([]Tool{spec(KindTask)}); err != nil {
		t.Fatalf("a task-kind tool was refused: %v", err)
	}
	if KindTask != "task" {
		t.Fatalf("KindTask = %q, want grok-build's word, task", KindTask)
	}
	if _, err := validateTools([]Tool{spec("think")}); err == nil || !strings.Contains(err.Error(), "unknown kind") {
		t.Fatalf("control: an undeclared kind registered (err %v)", err)
	}
}

// TestModeGateAllowsAgentInEveryMode (A5): the agent tool is allowed by name in
// agent, plan and ask mode — ahead of the mode's rules, so neither plan mode's
// judgement by kind nor ask mode's by ReadOnly reaches it, even for a request
// that would fail both — and it still goes to the inner gate, whose refusal
// stands. The rule over every mode: every other tool is judged as before —
// another task-kind tool that is not read-only is refused in ask mode, an edit
// in plan and ask, a command in ask, exit_plan_mode outside plan — and the
// rule is the exact id, so "Agent" is judged like any other name.
func TestModeGateAllowsAgentInEveryMode(t *testing.T) {
	f := newPlanFixture(t)
	other := target(t, f.other)
	planRefusal := planEditRejected(mustReal(t, f.plan))

	agentCall := Request{Tool: AgentTool, Kind: KindTask}
	// A request no real agent call makes, which plan mode and ask mode would
	// both refuse if the mode's rules were read before the name.
	agentAsEdit := Request{Tool: AgentTool, Kind: KindEdit, Targets: other}

	cases := []struct {
		name string
		req  Request
		deny map[string]string // by mode; a mode not in it allows, and the inner gate sees the call
	}{
		{name: "agent", req: agentCall},
		{name: "agent, whatever its kind says", req: agentAsEdit},
		{name: "another task-kind tool", req: Request{Tool: "agent_output", Kind: KindTask},
			deny: map[string]string{ModeAsk: askRejectedText}},
		{name: "a case-changed name is not the agent tool", req: Request{Tool: "Agent", Kind: KindTask},
			deny: map[string]string{ModeAsk: askRejectedText}},
		{name: "an edit", req: Request{Tool: "write", Kind: KindEdit, Targets: other},
			deny: map[string]string{ModePlan: planRefusal, ModeAsk: askRejectedText}},
		{name: "a command", req: Request{Tool: "bash", Kind: KindExecute, Command: "ls"},
			deny: map[string]string{ModeAsk: askRejectedText}},
		{name: "a read", req: Request{Tool: "read", Kind: KindRead, ReadOnly: true}},
		{name: "exit_plan_mode", req: Request{Tool: ExitPlanModeTool, Kind: KindAsk, ReadOnly: true},
			deny: map[string]string{ModeAgent: planDisabledText, ModeAsk: planDisabledText}},
	}
	for _, mode := range []string{ModeAgent, ModePlan, ModeAsk} {
		for _, tc := range cases {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				inner := &recordingGate{}
				g := NewModeGate(mode, inner)
				g.SetPlanPath(f.plan)
				dec, err := g.Check(context.Background(), tc.req)
				if err != nil {
					t.Fatalf("Check: %v", err)
				}
				want, denied := tc.deny[mode]
				if !denied {
					if _, ok := dec.(Allow); !ok || inner.calls() != 1 {
						t.Fatalf("decision = %#v, inner gate saw %d calls; want Allow, through the inner gate", dec, inner.calls())
					}
					return
				}
				if d, ok := dec.(Deny); !ok || d.Reason != want || inner.calls() != 0 {
					t.Fatalf("decision = %#v, inner gate saw %d calls; want Deny{%q} from the mode", dec, inner.calls(), want)
				}
			})
		}
	}

	// Allowed by the mode is not allowed: the inner gate — H3's evaluator,
	// later — still judges the agent call, in every mode, and its answer is
	// the gate's.
	for _, mode := range []string{ModeAgent, ModePlan, ModeAsk} {
		inner := &recordingGate{dec: Deny{Reason: "inner says no"}}
		g := NewModeGate(mode, inner)
		dec, err := g.Check(context.Background(), agentCall)
		if d, ok := dec.(Deny); err != nil || !ok || d.Reason != "inner says no" || inner.calls() != 1 {
			t.Fatalf("%s: decision = %#v, %v; want the inner gate's refusal", mode, dec, err)
		}
	}
}

// TestMapClaudeDisallowed: a deny list reads a restricted name as the whole of
// the tool it restricts (review r2 of C3a, finding 2) — Bash(rm:*) takes bash
// away, where the allow list reports it unknown and grants nothing — while
// everything else is the allow list's reading: the same vocabulary, the same
// silent drops (a restricted one included), the same unknown names, both
// results deduplicated in order. The allow list's own reading of the same
// names is the control that the difference is the deny mode and nothing else.
func TestMapClaudeDisallowed(t *testing.T) {
	for _, tc := range []struct {
		name               string
		in                 []string
		denyIDs, denyUnk   []string
		allowIDs, allowUnk []string
	}{
		{"a restriction takes the whole tool", []string{"Bash(rm:*)"},
			[]string{"bash"}, nil, nil, []string{"Bash(rm:*)"}},
		{"any case, any restriction, deduplicated",
			[]string{"Bash(rm:*)", "bash(git push:*)", "Read(./secrets/**)", "Edit", "MultiEdit(x)", "LS(/)"},
			[]string{"bash", "read", "edit"}, nil,
			[]string{"edit"}, []string{"Bash(rm:*)", "bash(git push:*)", "Read(./secrets/**)", "MultiEdit(x)", "LS(/)"}},
		{"a restricted drop stays dropped", []string{"Agent(explore)", "WebFetch(domain:example.com)", "Task(x)", "mcp__x(y)"},
			nil, nil, nil, nil},
		{"an unknown name is unknown either way", []string{"Frobnicate", "Frobnicate(x)", "Write"},
			[]string{"write"}, []string{"Frobnicate", "Frobnicate(x)"}, []string{"write"}, []string{"Frobnicate", "Frobnicate(x)"}},
		{"nothing", nil, nil, nil, nil, nil},
	} {
		ids, unknown := MapClaudeDisallowed(tc.in)
		if !slices.Equal(ids, tc.denyIDs) || !slices.Equal(unknown, tc.denyUnk) {
			t.Errorf("%s: MapClaudeDisallowed(%q) = %q, %q; want %q, %q", tc.name, tc.in, ids, unknown, tc.denyIDs, tc.denyUnk)
		}
		ids, unknown = MapClaudeTools(tc.in)
		if !slices.Equal(ids, tc.allowIDs) || !slices.Equal(unknown, tc.allowUnk) {
			t.Errorf("%s: MapClaudeTools(%q) = %q, %q; want %q, %q", tc.name, tc.in, ids, unknown, tc.allowIDs, tc.allowUnk)
		}
	}
}
