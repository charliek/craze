package opencode

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/charliek/craze/internal/harness/tool"
)

// newAgentOutput builds the agent_output tool (plan 026 §3.11): the model
// reads the result of a sub-agent it started in the background, by the id the
// agent call's acknowledgement named, waiting up to wait_ms for one still
// running. The tool only prepares the call; the harness's runner
// (tool.Env.Subagents) owns the results and answers.
//
// Its description is craze's own (NOTICE). The profile offers it right after
// agent, the tool whose background calls it reads. It is ReadOnly — it
// changes nothing, so every mode lets it through — Parallel, and Truncate
// None: the result was cut when it became deliverable, with the keys of the
// child that wrote it, and a second cut would add a spill path the runner's
// redaction never saw (the agent tool's reason, review r6).
func newAgentOutput() (tool.Tool, error) {
	desc, err := description("agent_output", nil)
	if err != nil {
		return nil, err
	}
	return &agentOutputTool{spec: tool.Spec{
		ID:          tool.AgentOutputTool,
		Description: desc,
		Parameters: map[string]any{
			"id": map[string]any{
				"type":        "string",
				"description": "The background sub-agent's id, as the agent call that started it returned it.",
			},
			"wait_ms": map[string]any{
				"type": "integer", "minimum": 0, "maximum": maxSafeInteger,
				"description": "Optional. How long to wait for a sub-agent that is still running, in milliseconds " +
					"(default 30000, at most 600000; 0 does not wait).",
			},
		},
		Required: []string{"id"},
		Kind:     tool.KindRead,
		ReadOnly: true,
		Parallel: true,
		Truncate: tool.None,
	}}, nil
}

type agentOutputTool struct{ spec tool.Spec }

func (t *agentOutputTool) Spec() tool.Spec { return t.spec }

// Prepare reads the id — required, a string, trimmed and not blank — and
// wait_ms, an integer: absent or null is the default, a negative is 0 (do not
// wait) and one past the maximum is the maximum, as bash reduces a timeout
// past its own. What the id names is the runner's to say.
func (t *agentOutputTool) Prepare(_ tool.Env, c tool.Call) (tool.Prepared, error) {
	a, err := parseArgs(c.Input)
	if err != nil {
		return nil, err
	}
	id, _, err := a.str("id", true)
	if err != nil {
		return nil, err
	}
	if id = strings.TrimSpace(id); id == "" {
		return nil, errors.New("id must not be empty: give the id the agent call returned")
	}
	wait := tool.AgentOutputDefaultWait
	if raw, present := a["wait_ms"]; present && jsonType(raw) != "null" {
		ms, _, err := a.integer("wait_ms", -maxSafeInteger)
		if err != nil {
			return nil, err
		}
		wait = time.Duration(min(max(ms, 0), tool.AgentOutputMaxWait.Milliseconds())) * time.Millisecond
	}
	return &agentOutputCall{call: tool.OutputCall{CallID: c.ID, ID: id, Wait: wait}}, nil
}

type agentOutputCall struct{ call tool.OutputCall }

func (c *agentOutputCall) Request() tool.Request {
	return tool.Request{Title: c.call.ID}
}

// Run hands the call to the session's runner, which answers from the result's
// delivery state and waits only for a child still running. A call cancelled
// before it got here is handed over too, and the runner answers it aborted at
// once: redacted, as its every answer is, with every child's keys, where an
// aborted made here would go out under the parent's alone (astra r17). Only a
// session with no runner, and so no child, answers aborted here.
func (c *agentOutputCall) Run(ctx context.Context, env tool.Env) tool.Result {
	if env.Subagents == nil {
		if ctx.Err() != nil {
			return aborted()
		}
		return tool.Result{Text: noSubagentsText, IsError: true, Class: tool.ClassToolError}
	}
	return env.Subagents.Output(ctx, c.call)
}
