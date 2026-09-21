package opencode

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/charliek/craze/internal/harness/tool"
)

// The texts exit_plan_mode answers the model with (plan 023 §3.4). Rejected
// is grok-build's no-feedback revise text (tool_calls.rs:317-326): the ask
// registry's plan answer is accept or reject and carries no feedback, so the
// person's next message does. Approved and EmptyPlan are craze's: in
// grok-build the turn goes on to implement and an empty plan leaves plan mode,
// where here approving ends the turn — the TUI's offer implements — and the
// mode is the person's to change, so neither of grok-build's sentences would
// be true.
const (
	PlanApprovedText = "The user approved the plan. Your turn ends here; the user will start implementation."
	PlanRejectedText = "The user wants to revise the plan. Ask the user what changes they would like to make."
	EmptyPlanText    = "No plan was found: the plan file (`%s`) is empty or missing. Write your plan to it, then call exit_plan_mode again."
	// PlanUnansweredText is the plan's twin of UnansweredText: the card was
	// dismissed, its turn ended, or the session is closing.
	PlanUnansweredText = "The user did not decide on the plan. Plan mode is still active; do not make any edits. Ask the user how they would like to proceed."
)

// maxPlanBytes bounds the plan file exit_plan_mode reads: a plan is prose, and
// the whole of it is shown on a card and sent to the model again.
const maxPlanBytes = 256 << 10

// newExitPlanMode builds exit_plan_mode (plan 023 §3.4): grok-build's tool,
// which takes no arguments — the plan is the plan file, never something the
// model passes (D-50). The mode gate refuses it by name outside plan mode
// (tool.ModeGate), so Run only ever sees plan mode.
func newExitPlanMode() (tool.Tool, error) {
	desc, err := description("exit_plan_mode", nil)
	if err != nil {
		return nil, err
	}
	return &exitPlanTool{spec: tool.Spec{
		ID:          "exit_plan_mode",
		Description: desc,
		Parameters:  map[string]any{},
		Required:    []string{},
		Kind:        tool.KindAsk,
		// It writes nothing, and it is not Parallel: it blocks on the person,
		// and every call of its step after it must wait for the decision, since
		// an approval vetoes them (plan 023 §3.4).
		ReadOnly: true,
		Truncate: tool.Head,
	}}, nil
}

type exitPlanTool struct{ spec tool.Spec }

func (t *exitPlanTool) Spec() tool.Spec { return t.spec }

// Prepare takes any JSON object and reads none of it: the tool has no
// parameters, and a model that sends some anyway (Claude Code's ExitPlanMode
// takes the plan as an argument) is still asking for the same thing.
func (t *exitPlanTool) Prepare(env tool.Env, c tool.Call) (tool.Prepared, error) {
	if s := strings.TrimSpace(string(c.Input)); s != "" {
		if _, err := parseArgs(c.Input); err != nil {
			return nil, err
		}
	}
	return &exitPlanCall{path: env.PlanPath}, nil
}

type exitPlanCall struct{ path string }

func (c *exitPlanCall) Request() tool.Request {
	// Paths stays empty: the card's line is enough, and the path is the
	// harness's own file, not something the call was pointed at.
	return tool.Request{Title: "present the plan"}
}

func (c *exitPlanCall) Run(ctx context.Context, env tool.Env) tool.Result {
	if ctx.Err() != nil {
		return aborted()
	}
	if c.path == "" {
		return tool.Result{Text: "This session has no plan file.", IsError: true, Class: tool.ClassToolError}
	}
	text, err := readPlan(ctx, env, c.path)
	if err != nil {
		return errorResult(err)
	}
	if strings.TrimSpace(text) == "" {
		return tool.Result{Text: fmt.Sprintf(EmptyPlanText, c.path)}
	}
	if env.Asker == nil {
		return tool.Result{Text: PlanUnansweredText}
	}
	// The plan is the model's writing about files it has read, and the asker's
	// side of the seam has no redactor (ask_user_question says the same).
	out, err := env.Asker.PresentPlan(ctx, tool.PlanOffer{Path: c.path, Text: env.Redactor.String(text)})
	switch {
	case err != nil && askStopped(ctx, env):
		return unanswered(ctx, env, PlanUnansweredText)
	case err != nil:
		return tool.Result{Text: "The plan could not be presented: " + err.Error(), IsError: true, Class: tool.ClassToolError}
	}
	switch out {
	case tool.PlanApproved:
		return tool.Result{Text: PlanApprovedText}
	case tool.PlanRejected:
		return tool.Result{Text: PlanRejectedText}
	}
	return unanswered(ctx, env, PlanUnansweredText)
}

// readPlan reads the plan file the way the file tools read a file (file.go):
// under craze's path lock, so a write still landing is over; through openFile,
// which does not block on a FIFO; and only if what it opened is a regular file
// of at most maxPlanBytes — checked on the descriptor, so the file cannot be
// swapped between the check and the read. Nothing else may be at that path:
// exit_plan_mode runs on one of Fantasy's tool goroutines, which the turn
// joins, so a read that never returned would hang the turn and Close with it.
// A missing file reads as empty. The lock is released before the person is
// asked, since nobody should wait on a card to write a file.
func readPlan(ctx context.Context, env tool.Env, path string) (string, error) {
	real, err := realPath(path)
	if err != nil {
		return "", err
	}
	unlock, err := env.Locks.Lock(ctx, real)
	if err != nil {
		return "", err
	}
	defer unlock()
	f, info, err := openFile(real)
	switch {
	case missing(err):
		return "", nil
	case err != nil:
		return "", err
	}
	defer func() { _ = f.Close() }()
	switch {
	case isCredentials(env, real, info):
		// The plan file's path is craze's own, but what is at it is not: made a
		// symlink or a hard link to the key file, it would be shown to the
		// person and sent to the model as a plan. Redaction is no answer — a
		// key rotated on disk since the table loaded is one the redactor has
		// never seen — so it is refused as every file tool refuses it, on the
		// descriptor that was opened.
		return "", fail(tool.ClassToolError, credentialsText)
	case !info.Mode().IsRegular():
		return "", fail(tool.ClassToolError, "The plan file is not a regular file: "+path)
	case info.Size() > maxPlanBytes:
		return "", fail(tool.ClassOutputLimit, fmt.Sprintf("The plan file is %d bytes, larger than the %d a plan may be: %s", info.Size(), maxPlanBytes, path))
	}
	// One byte past the bound, so a file that grew after the stat is caught
	// rather than cut.
	b, err := io.ReadAll(io.NewSectionReader(f, 0, maxPlanBytes+1))
	switch {
	case err != nil:
		return "", err
	case len(b) > maxPlanBytes:
		return "", fail(tool.ClassOutputLimit, fmt.Sprintf("The plan file is larger than the %d bytes a plan may be: %s", maxPlanBytes, path))
	case ctx.Err() != nil:
		return "", ctx.Err()
	}
	text, _ := splitBOM(string(b))
	return text, nil
}
