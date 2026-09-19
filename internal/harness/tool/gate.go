package tool

import "context"

// Gate decides whether a prepared call may run (Seam 3, plan 019 §3.1). The
// dispatcher asks it once per call, after Prepare and before Run, with the
// call's redacted Request, so a gate judges exactly what will run and never
// parses input itself.
//
// H2 has no approval channel and ships AllowAll: the owner's direction is an
// evaluator over this seam that flags dangerous-looking calls, not a prompt
// on every action (§3.3). A future channel adds an approver to Env and turns
// Ask into a question; no tool changes.
//
// Check runs on Fantasy's tool goroutines, up to five at once, so it must be
// safe for concurrent use and must return once ctx is done.
type Gate interface {
	Check(ctx context.Context, req Request) (Decision, error)
}

// Decision is a gate's answer: Allow, Deny or Ask.
type Decision interface{ decision() }

// Allow lets the call run.
type Allow struct{}

// Deny refuses the call. Reason is the model-facing error text; empty, the
// model gets opencode's sentence for a rule that blocks a call.
type Deny struct{ Reason string }

// Ask wants a person to approve the call. This build has no one to ask, so
// the dispatcher treats it as Deny{NoApprovalChannel}.
type Ask struct{ Prompt string }

func (Allow) decision() {}
func (Deny) decision()  {}
func (Ask) decision()   {}

// NoApprovalChannel is the reason an Ask is denied with in H2.
const NoApprovalChannel = "no approval channel in this build"

// deniedText is opencode's DeniedError message without its rule list
// (packages/core/src/v1/permission.ts:25), for a Deny with no reason.
const deniedText = "The user has specified a rule which prevents you from using this specific tool call."

// GateFunc adapts a function to a Gate.
type GateFunc func(ctx context.Context, req Request) (Decision, error)

// Check calls f.
func (f GateFunc) Check(ctx context.Context, req Request) (Decision, error) { return f(ctx, req) }

// AllowAll is H2's gate: every call runs (owner decision 2).
var AllowAll Gate = GateFunc(func(context.Context, Request) (Decision, error) { return Allow{}, nil })
