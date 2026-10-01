package opencode

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/charliek/craze/internal/harness/tool"
)

// newBashOutput builds the bash_output tool (plan 033 §3.7): the model reads
// a background job's output since its last read — or, once the job has ended,
// its result, delivered once — by the id the job's receipt named, waiting up
// to wait_ms for it to end. Like agent_output it only prepares the call; the
// session's jobs (tool.Env.Jobs) own the output, the read cursor and the
// result, and answer.
//
// Its description is craze's own (NOTICE). The profile offers it right after
// bash, the tool whose jobs it reads. It is ReadOnly — it changes nothing, so
// every mode lets it through — Parallel, and Truncate None: what it returns
// is cut to the model's limits and redacted already, and a second cut would
// add a spill path the session's redaction never saw (agent_output's reason).
func newBashOutput() (tool.Tool, error) {
	desc, err := description("bash_output", nil)
	if err != nil {
		return nil, err
	}
	return &bashOutputTool{spec: tool.Spec{
		ID:          tool.BashOutputTool,
		Description: desc,
		Parameters: map[string]any{
			"id": map[string]any{
				"type":        "string",
				"description": "The background job's id, as the bash call that started it returned it.",
			},
			"wait_ms": map[string]any{
				"type": "integer", "minimum": 0, "maximum": maxSafeInteger,
				"description": "Optional. How long to wait for the job to finish, in milliseconds " +
					"(default 0, which does not wait; at most 600000).",
			},
		},
		Required: []string{"id"},
		Kind:     tool.KindRead,
		ReadOnly: true,
		Parallel: true,
		Truncate: tool.None,
	}}, nil
}

type bashOutputTool struct{ spec tool.Spec }

func (t *bashOutputTool) Spec() tool.Spec { return t.spec }

// WithoutJobs is nothing (tool.JobsAware, plan 033 X101): a session that runs
// no background jobs — headless, or a sub-agent's — is not offered
// bash_output, so its model never reads of a job it cannot start.
func (t *bashOutputTool) WithoutJobs() tool.Tool { return nil }

// Prepare reads the id — required, a string, trimmed and not blank — and
// wait_ms, an integer: absent or null is 0, a negative is 0 and one past the
// maximum is the maximum, as agent_output reads its own. What the id names
// is the session's to say.
func (t *bashOutputTool) Prepare(_ tool.Env, c tool.Call) (tool.Prepared, error) {
	a, err := parseArgs(c.Input)
	if err != nil {
		return nil, err
	}
	id, err := jobID(a)
	if err != nil {
		return nil, err
	}
	var wait time.Duration
	if raw, present := a["wait_ms"]; present && jsonType(raw) != "null" {
		ms, _, err := a.integer("wait_ms", -maxSafeInteger)
		if err != nil {
			return nil, err
		}
		wait = time.Duration(min(max(ms, 0), tool.JobOutputMaxWait.Milliseconds())) * time.Millisecond
	}
	return &bashOutputCall{call: tool.JobOutputCall{CallID: c.ID, ID: id, Wait: wait}}, nil
}

// jobID reads a job tool's id: required, a string, trimmed and not blank.
func jobID(a args) (string, error) {
	id, _, err := a.str("id", true)
	if err != nil {
		return "", err
	}
	if id = strings.TrimSpace(id); id == "" {
		return "", errors.New("id must not be empty: give the id the bash call returned")
	}
	return id, nil
}

type bashOutputCall struct{ call tool.JobOutputCall }

// Request titles the call for its row: what the read is of.
func (c *bashOutputCall) Request() tool.Request {
	return tool.Request{Title: "background job output " + c.call.ID}
}

// Run hands the call to the session's jobs, which answer from the job's
// state and wait only for a job still running. A call cancelled before it
// got here is handed over too, and answered aborted there, redacted as every
// answer of theirs is (agent_output's rule, astra r17). Only a session with
// no jobs answers here: it started none.
func (c *bashOutputCall) Run(ctx context.Context, env tool.Env) tool.Result {
	if env.Jobs == nil {
		return noJobs(ctx)
	}
	return env.Jobs.Output(ctx, c.call)
}

// noJobs answers a job tool's call in a session that runs no background jobs
// (Env.Jobs nil): headless, or a sub-agent's, neither of which is offered the
// tools (WithoutJobs, X101) — a defence, not a path. A cancelled call is
// aborted.
func noJobs(ctx context.Context) tool.Result {
	if ctx.Err() != nil {
		return aborted()
	}
	return tool.Result{Text: noJobsText, IsError: true, Class: tool.ClassToolError}
}

// noJobsText is noJobs' answer.
const noJobsText = "This session runs no background jobs."
