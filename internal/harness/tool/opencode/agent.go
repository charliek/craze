package opencode

import (
	"context"
	"errors"
	"strings"

	"github.com/charliek/craze/internal/harness/tool"
)

// noSubagentsText answers an agent call in a session that has no runner to
// hand it to (tool.Env.Subagents is nil).
const noSubagentsText = "Sub-agents are not available in this session."

// newAgent builds the agent tool (plan 026 §3.3): the model starts a
// sub-agent — a child session with its own context and tools — for one task,
// and reads back its final message. The tool only prepares the call; the
// harness's runner (tool.Env.Subagents) resolves the type, the model and the
// effort, opens the child and answers.
//
// Its id is agent, not grok-build's task: in craze's TUI "tasks" are the todo
// panel, and Claude Code's name for the tool is Agent; the description opens
// with the sentence naming both (D-53's alias pattern), so content written for
// either finds it. Its description is craze's own (NOTICE), and each session
// adds the agent types and models it offers after it (the harness's
// decorator); the static text is what this profile's specs pin.
//
// The profile offers it after write (Profile), opencode's own place for its
// task tool; the harness's runner answers it (plan 026 §3.8).
func newAgent() (tool.Tool, error) {
	desc, err := description("agent", nil)
	if err != nil {
		return nil, err
	}
	return &agentTool{spec: tool.Spec{
		ID:          tool.AgentTool,
		Description: desc,
		Parameters: map[string]any{
			"description": map[string]any{
				"type":        "string",
				"description": "A short (3-5 word) description of the task, which the user sees.",
			},
			"prompt": map[string]any{
				"type":        "string",
				"description": "The task for the sub-agent. It cannot see this conversation, so include everything it needs.",
			},
			"subagent_type": map[string]any{
				"type":        "string",
				"description": "Optional. The agent type to start, one of those listed in this tool's description (default: " + tool.DefaultAgentType + ").",
			},
			"model": map[string]any{
				"type": "string",
				"description": "Optional. The model the sub-agent runs on: one of the models listed in this tool's description, " +
					"a tier name (fable, opus, sonnet or haiku; one the Tiers line does not map is your own model), or inherit " +
					"for your own. Omit it to use the agent type's model, or else the configured default, or else your own.",
			},
			"effort": map[string]any{
				"type":        "string",
				"description": "Optional. The sub-agent's reasoning effort: one of the efforts its model offers, as listed in this tool's description.",
			},
		},
		Required: []string{"description", "prompt"},
		Kind:     tool.KindTask,
		// Several calls in one step start their children together: that is
		// what fanning out is (owner decision 7). Fantasy runs a step's
		// parallel calls up to five at once, and the runner's own cap of four
		// children sits under that (plan 026 §3.8).
		Parallel: true,
		// A child may edit, so the call is not ReadOnly; ask mode still lets it
		// through, by name (tool.ModeGate), and binds the child instead.
		ReadOnly: false,
		// The runner cuts the child's answer itself, a success and an error
		// alike, keeping its start, at the shared truncator's limits, and
		// redacts what that adds with the parent's keys and the child's
		// together (review r6): the dispatcher's cut would add the spill path
		// after the runner's last redaction, with the parent's installed keys
		// alone to redact it. None, so the dispatcher never cuts the answer a
		// second time.
		Truncate: tool.None,
	}}, nil
}

type agentTool struct{ spec tool.Spec }

func (t *agentTool) Spec() tool.Spec { return t.spec }

// Prepare reads the call's arguments; it resolves nothing, since what the
// type, the model and the effort name is the runner's to decide, and an
// unknown one is the runner's refusal (plan 026 §3.4, §3.6).
//
// The three optional strings read an explicit null as an omission (grok-build's
// Option semantics, args.strOrNull): a model that sends "model": null means it
// has no preference. An unknown key is tolerated, as by every tool here
// (parseArgs) — in particular Claude Code's run_in_background, which this
// tool does not declare until background children exist (plan 026 PR 3): a
// call that sends it runs in the foreground, like any other.
func (t *agentTool) Prepare(_ tool.Env, c tool.Call) (tool.Prepared, error) {
	a, err := parseArgs(c.Input)
	if err != nil {
		return nil, err
	}
	desc, _, err := a.str("description", true)
	if err != nil {
		return nil, err
	}
	prompt, _, err := a.str("prompt", true)
	if err != nil {
		return nil, err
	}
	var typ, model, effort string
	for _, f := range []struct {
		name string
		into *string
	}{{"subagent_type", &typ}, {"model", &model}, {"effort", &effort}} {
		if *f.into, _, err = a.strOrNull(f.name); err != nil {
			return nil, err
		}
		*f.into = strings.TrimSpace(*f.into)
	}
	// A blank description would leave the child's row without a label, and a
	// blank prompt would start an agent with nothing to do.
	switch desc = strings.TrimSpace(desc); {
	case desc == "":
		return nil, errors.New("description must not be empty: give the task a short (3-5 word) label")
	case strings.TrimSpace(prompt) == "":
		return nil, errors.New("prompt must not be empty: say what the sub-agent should do")
	}
	if typ == "" {
		typ = tool.DefaultAgentType
	}
	// The prompt is the child's first message exactly as the model wrote it.
	return &agentCall{call: tool.SubagentCall{
		ID: c.ID, Description: desc, Prompt: prompt, Type: typ, Model: model, Effort: effort,
	}}, nil
}

type agentCall struct{ call tool.SubagentCall }

func (c *agentCall) Request() tool.Request {
	return tool.Request{Title: c.call.Description}
}

// Run hands the call to the session's runner and returns what it answers:
// the call blocks until the child has ended (owner decision 7), and a cancel
// reaches the child through ctx.
//
// The answer comes back as the runner cut it (plan 026 §3.7's one cap, on
// both paths): a success and a failed child's error alike, which carries its
// last output and can be as long as any answer, are truncated there, with the
// spill path and the notice redacted by the keys of both sessions (review
// r6). The spec's Truncate is None, so the dispatcher only redacts it, the
// spill path included, as it redacts every tool's result; the class and the
// child's usage are the runner's.
func (c *agentCall) Run(ctx context.Context, env tool.Env) tool.Result {
	if ctx.Err() != nil {
		return aborted()
	}
	if env.Subagents == nil {
		return tool.Result{Text: noSubagentsText, IsError: true, Class: tool.ClassToolError}
	}
	return env.Subagents.Run(ctx, c.call)
}
