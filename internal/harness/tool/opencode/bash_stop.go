package opencode

import (
	"context"

	"github.com/charliek/craze/internal/harness/tool"
)

// newBashStop builds the bash_stop tool (plan 033 §3.7): the model stops a
// background job it started — SIGTERM, then SIGKILL 3 s later — and reads
// its result, delivered once, which the call waits up to 7 s for.
//
// Its description is craze's own (NOTICE), and the profile offers it after
// bash_output. It is KindExecute and not ReadOnly — it ends a process — but
// ModeGate allows it by name in every mode: it only ends what this session
// started. Parallel, and Truncate None for bash_output's reason.
func newBashStop() (tool.Tool, error) {
	desc, err := description("bash_stop", nil)
	if err != nil {
		return nil, err
	}
	return &bashStopTool{spec: tool.Spec{
		ID:          tool.BashStopTool,
		Description: desc,
		Parameters: map[string]any{
			"id": map[string]any{
				"type":        "string",
				"description": "The background job's id, as the bash call that started it returned it.",
			},
		},
		Required: []string{"id"},
		Kind:     tool.KindExecute,
		Parallel: true,
		Truncate: tool.None,
	}}, nil
}

type bashStopTool struct{ spec tool.Spec }

func (t *bashStopTool) Spec() tool.Spec { return t.spec }

// WithoutJobs is nothing, as bash_output's is (plan 033 X101).
func (t *bashStopTool) WithoutJobs() tool.Tool { return nil }

// Prepare reads the id, as bash_output does.
func (t *bashStopTool) Prepare(_ tool.Env, c tool.Call) (tool.Prepared, error) {
	a, err := parseArgs(c.Input)
	if err != nil {
		return nil, err
	}
	id, err := jobID(a)
	if err != nil {
		return nil, err
	}
	return &bashStopCall{call: tool.JobStopCall{CallID: c.ID, ID: id}}, nil
}

type bashStopCall struct{ call tool.JobStopCall }

// Request titles the call for its row.
func (c *bashStopCall) Request() tool.Request {
	return tool.Request{Title: "stop background job " + c.call.ID}
}

// Run hands the call to the session's jobs, as bash_output's does.
func (c *bashStopCall) Run(ctx context.Context, env tool.Env) tool.Result {
	if env.Jobs == nil {
		return noJobs(ctx)
	}
	return env.Jobs.Stop(ctx, c.call)
}
