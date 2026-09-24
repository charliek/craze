package tool

import (
	"context"
	"os"
	"strings"
	"sync/atomic"
)

// Modes are a Gate (plan 023 §3.1, owner decision 3). The tool set is fixed
// from Open to Close and the system prompt is frozen, so a mode cannot take a
// tool away from the model: every tool stays advertised in every mode and the
// dispatcher refuses the calls the mode does not allow, which is how
// grok-build enforces plan mode too. The model reads the refusal and the
// reminder that goes with it (reminders.go in the harness).

// The modes a session runs in. ModeAgent is the default — the mode a session
// with no mode set is in — and judges nothing itself.
const (
	ModeAgent = "agent"
	ModePlan  = "plan"
	ModeAsk   = "ask"
)

// ExitPlanModeTool is the id of the tool that presents a finished plan (C4).
// ModeGate knows the name because the tool is ReadOnly: nothing else about it
// would stop it running — and answering, with an empty plan — in a mode where
// there is no plan to present (panel correction 17).
const ExitPlanModeTool = "exit_plan_mode"

// The refusals, grok-build's word for word (plan_mode.rs:325-371), with
// craze's plan path spliced in. They are what the model reads back as the
// call's error result, so each says what the rule is rather than that a rule
// exists.
const (
	askRejectedText  = "Rejected: ask mode is read-only - no edits, writes, or shell commands."
	planDisabledText = "Plan mode has been disabled. Do not call exit_plan_mode again unless the user explicitly asks to re-enter plan mode."
)

// planEditRejected is the refusal of an edit-kind call in plan mode.
func planEditRejected(planPath string) string {
	return "Rejected: file edits are not allowed in plan mode - the only editable file is the plan file (`" + planPath + "`)."
}

// A sub-agent's refusals, craze's own (plan 026 §3.5). A child in plan mode
// has no plan file — every edit is refused, and there is no path to name — so
// grok-build's text, which offers the plan file as the one exception, would
// send it looking for a file it cannot have. The two "switched" texts are the
// refusals a child reads when its parent's later switch, not the mode it
// opened in, is what refuses the call: the model otherwise sees a tool that
// ran a step ago refused now, with nothing in its reminder to say why, since
// a tightening changes no reminder.
const (
	childPlanRejectedText = "Rejected: file edits are not allowed — the agent that started you is in plan mode."
	childPlanSwitchedText = "Rejected: file edits are not allowed — the agent that started you switched to plan mode."
	childAskSwitchedText  = "Rejected: no edits, writes, or shell commands — the agent that started you switched to ask mode, which is read-only."
)

// The ranks Strictness gives the modes.
const (
	strictnessAgent int32 = iota
	strictnessPlan
	strictnessAsk
)

// Strictness ranks a mode by what it refuses: agent (nothing) below plan
// (edits) below ask (everything that is not read-only). A sub-agent's gate
// judges each call under the stricter of the mode it opened in and the rank
// its parent has pushed since (plan 026 §3.5, panel P37, P50). An unknown mode
// ranks as agent, as ModeGate judges one.
func Strictness(mode string) int32 {
	switch mode {
	case ModePlan:
		return strictnessPlan
	case ModeAsk:
		return strictnessAsk
	}
	return strictnessAgent
}

// modeOf is Strictness's inverse over the three ranks.
func modeOf(rank int32) string {
	switch {
	case rank >= strictnessAsk:
		return ModeAsk
	case rank == strictnessPlan:
		return ModePlan
	}
	return ModeAgent
}

// Raise lifts s to mode's rank when it is below it, and never lowers it: a
// monotonic maximum, by compare-and-swap, so two raises racing each other
// settle on the stricter and a raise towards agent is a no-op. A parent's
// SetMode calls it for every running child (plan 026 §3.5): sampling the
// parent's live mode at each of a child's checks instead would miss an
// agent→ask→agent round trip made between two of them (panel P37, P50), and
// the maximum is what makes a switch back loosen nothing. A nil s is nothing
// to raise.
func Raise(s *atomic.Int32, mode string) {
	if s == nil {
		return
	}
	want := Strictness(mode)
	for {
		cur := s.Load()
		if cur >= want || s.CompareAndSwap(cur, want) {
			return
		}
	}
}

// ModeGate judges a call against the session's mode and hands everything it
// does not refuse to an inner gate — AllowAll today, H3's evaluator later,
// which wraps the same way. Its rules:
//
//   - plan: an edit-kind call runs only when every one of its Targets is the
//     plan file; one with no target at all is refused, since there is nothing
//     to judge. Everything else, bash included (owner decision 3), goes
//     inward: the dispatcher cannot see inside a shell, and the reminder
//     carries the rule.
//   - ask: a call that is not ReadOnly is refused.
//   - any mode but plan: exit_plan_mode is refused by name.
//
// The mode is read atomically on every call, so SetMode is effective at once:
// the next call whose Check reaches the read is judged under the new mode,
// and a call already past it runs. That is the boundary, and it is the only
// one available — the alternative, holding a lock across the tool, would let
// a mode switch wait on a command.
//
// # The race between the check and the open
//
// Targets are resolved when the call is prepared and the file is opened when
// it runs, so between the two a symlink can be re-pointed and the call can
// land somewhere else. craze accepts that, as H4 accepted the same race in
// the content loader's confinement: closing it needs the open itself to carry
// the check (openat2, O_NOFOLLOW on every component), which is a change to
// every file tool, not to the gate. What the gate buys is that a model
// spelling a path differently, through a link, or into a directory that does
// not exist yet cannot reach a file the mode forbids; what it does not buy is
// protection from another process rearranging the tree mid-call.
//
// Check runs on Fantasy's tool goroutines, up to five at once.
//
// # A sub-agent's gate
//
// NewChildModeGate builds the gate of a session another started (plan 026
// §3.5). Its mode is the parent's when the child opened and never changes by
// SetMode — the harness refuses a child's switch — but the parent's own later
// switches reach it as strictness: an atomic rank the parent only ever raises
// (Raise), which the gate reads at every Check and judges by whenever it is
// stricter than the mode. So a running child is tightened from its next call
// and never loosened. Its plan-mode refusal is the child's own text, since a
// child has no plan file to offer as the exception, and a refusal only the
// raised rank makes says the parent switched.
type ModeGate struct {
	mode     atomic.Pointer[string]
	planPath atomic.Pointer[string]
	inner    Gate
	// child and strictness are fixed at construction (NewChildModeGate) and
	// read without a lock; strictness is nil for a session no parent tightens.
	child      bool
	strictness *atomic.Int32
}

// NewModeGate is a gate in mode over inner (nil is AllowAll). The plan file is
// named later, with SetPlanPath: a session's transcript, and so its plan file,
// is named after its tools are built.
func NewModeGate(mode string, inner Gate) *ModeGate {
	if inner == nil {
		inner = AllowAll
	}
	g := &ModeGate{inner: inner}
	g.SetMode(mode)
	g.SetPlanPath("")
	return g
}

// NewChildModeGate is a sub-agent's gate in mode over inner (nil is
// AllowAll), tightened by strictness, which its parent raises (Raise); nil
// strictness tightens nothing. Its plan path stays unset: a child has no plan
// file, so in plan mode it refuses every edit (plan 026 §3.2, §3.5).
func NewChildModeGate(mode string, strictness *atomic.Int32, inner Gate) *ModeGate {
	g := NewModeGate(mode, inner)
	g.child, g.strictness = true, strictness
	return g
}

// SetMode makes mode the one every call from now on is judged under. An
// unknown mode judges like agent, which is what a caller that validates its
// modes — the harness does — never asks for.
func (g *ModeGate) SetMode(mode string) { g.mode.Store(&mode) }

// Mode is the mode calls are judged under now.
func (g *ModeGate) Mode() string { return *g.mode.Load() }

// SetPlanPath names the session's plan file: the one file an edit-kind call
// may touch in plan mode. It is canonicalized here, the way Targets are, so
// the two are compared like with like; a path that will not resolve is kept
// as it was, which no target then matches. The harness calls it once, as the
// session opens, and an unset path refuses every edit in plan mode.
func (g *ModeGate) SetPlanPath(path string) {
	if path != "" {
		if real, err := RealPath(path); err == nil {
			path = real
		}
	}
	g.planPath.Store(&path)
}

// PlanPath is the canonical plan file, or "" when none was set.
func (g *ModeGate) PlanPath() string { return *g.planPath.Load() }

// Check is the Gate.
func (g *ModeGate) Check(ctx context.Context, req Request) (Decision, error) {
	mode, raised := g.judgedMode()
	if mode != ModePlan && req.Tool == ExitPlanModeTool {
		return Deny{Reason: planDisabledText}, nil
	}
	switch mode {
	case ModePlan:
		if req.Kind == KindEdit && !g.onlyThePlan(req.Targets) {
			return Deny{Reason: g.planRefusal(raised)}, nil
		}
	case ModeAsk:
		if !req.ReadOnly {
			return Deny{Reason: g.askRefusal(raised)}, nil
		}
	}
	return g.inner.Check(ctx, req)
}

// judgedMode is the mode a call is judged under now, and whether it is
// stricter than the gate's own mode because a sub-agent's parent raised it.
// For every other gate it is the mode, unraised.
func (g *ModeGate) judgedMode() (mode string, raised bool) {
	mode = g.Mode()
	if g.strictness == nil {
		return mode, false
	}
	if r := g.strictness.Load(); r > Strictness(mode) {
		return modeOf(r), true
	}
	return mode, false
}

// planRefusal is plan mode's refusal of an edit: grok-build's, naming the plan
// file, for a session; a sub-agent's own, which names none, for a child — and
// the one saying its parent switched when that is what refused the call.
func (g *ModeGate) planRefusal(raised bool) string {
	switch {
	case raised:
		return childPlanSwitchedText
	case g.child:
		return childPlanRejectedText
	}
	return planEditRejected(g.PlanPath())
}

// askRefusal is ask mode's refusal of a call that is not read-only: the one
// text every session reads, unless a sub-agent's parent switched it to ask.
func (g *ModeGate) askRefusal(raised bool) string {
	if raised {
		return childAskSwitchedText
	}
	return askRejectedText
}

// onlyThePlan reports whether targets are the plan file and nothing else. An
// empty list is false: a call with no target is one nothing can judge, and in
// plan mode "every target is the plan file" would otherwise be vacuously true
// of it (panel correction 9).
func (g *ModeGate) onlyThePlan(targets []string) bool {
	plan := g.PlanPath()
	if plan == "" || len(targets) == 0 {
		return false
	}
	for _, t := range targets {
		if !isThePlan(t, plan) {
			return false
		}
	}
	return true
}

// isThePlan reports whether target, a path RealPath resolved, names the plan
// file, which RealPath resolved too: equal strings, which is the answer on a
// case-sensitive file system, or one file under two spellings of one name.
//
// On a case-insensitive file system (macOS's APFS, by default) a file has many
// spellings and a resolved path keeps the one it was given, so an edit of the
// plan file named <dir>/X.PLAN.MD would otherwise be refused as another file —
// the same reason the credentials check spells its comparison out
// (opencode/file.go:107-130). os.SameFile is what asks the file system whether
// the two spellings are one file; EqualFold is what keeps a hard link at
// another name out of it, since two names for one inode are one file to
// os.SameFile and only a case-equivalent spelling is the plan file under
// another name. Both must hold, and a target that does not exist can only be
// the plan file by its string (plan 023 §3.1).
func isThePlan(target, plan string) bool {
	if target == plan {
		return true
	}
	// Before the two stats, so a call the mode refuses anyway does no I/O.
	if !strings.EqualFold(target, plan) {
		return false
	}
	ti, err := os.Stat(target)
	if err != nil {
		return false
	}
	pi, err := os.Stat(plan)
	return err == nil && os.SameFile(ti, pi)
}
