package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"charm.land/fantasy"
	"github.com/charliek/craze/internal/harness/store"
	"github.com/charliek/craze/internal/harness/tool"
)

// This file is the one place that knows both Fantasy and craze's tool
// framework (plan 019 §3.1, Seam 1): it offers the session's tools to
// Fantasy as fantasy.AgentTool, and it keeps the turn's side of every tool
// call — the harness's id for it, its events, its result — from Fantasy's
// tool callbacks. Replacing Fantasy's loop (plan 019 §3.4) rewrites this
// file and turn.go, and nothing under internal/harness/tool.
//
// A call's life, as Fantasy drives it (plan 019 §2.4):
//
//   - OnToolInputStart, on the stream goroutine, as the model begins it:
//     the call gets its id, "t<turn>.<step>.<n>", and a ToolStarted.
//   - OnToolCall, on the stream goroutine, once the stream has ended and
//     only for a finish Fantasy trusts: the dispatcher prepares it (parse,
//     resolve) and ToolCalled reports it — calls Fantasy found invalid too.
//   - Run, on a tool goroutine, only for a "tool-calls" finish: the
//     dispatcher gates, runs, redacts and truncates it, with progress.
//   - OnToolResult, on that goroutine: ToolFinished.
//   - OnStepFinish: any call Fantasy did not run gets a result of the
//     runner's own, so the step persists with every call answered, and any
//     call that never arrived is settled.

// calls is the turn's record of the step's tool calls. Its fields are
// guarded by the turn's mu.
type calls struct {
	tools *toolset // the session's, fixed

	list     []*toolCall          // the step's calls, in the order first seen
	byCallID map[string]*toolCall // announced calls by provider id; the first, for a repeated one
	n        int                  // the step's calls numbered so far
	bad      bool                 // an announced call's provider id was empty or repeated
	// announcedN counts the step's announced calls, which is the order
	// Fantasy dispatches them in (toolCall.order).
	announcedN int
}

// toolCall is one call the turn has reported. id is the harness's; callID
// is the provider's, which may be empty or repeated (plan 019 §3.5).
type toolCall struct {
	id, callID string
	name       string
	input      string    // the raw arguments, once announced
	called     bool      // OnToolCall announced it (ToolCalled)
	order      int       // its place among the step's announced calls, from 1: the order Fantasy dispatches them in
	running    bool      // the dispatcher is running it now (runTool)
	at         time.Time // when it was announced
	// veto, when set, is the result that stands in for running the call:
	// the seam the doom-loop guard (plan 019 §3.7) refuses a call through.
	veto   *tool.Result
	res    *tool.Result                    // the result Run handed Fantasy, once run
	output fantasy.ToolResultOutputContent // what Fantasy recorded for the model, once finished
	done   bool                            // ToolFinished was sent
}

// resetCalls empties the step's record: for a new step, or for a retried
// attempt of the same one, which keeps numbering from where the failed
// attempt left off so no id is used twice. mu is held.
func (t *turn) resetCalls(newStep bool) {
	t.list, t.byCallID, t.bad, t.announcedN = nil, map[string]*toolCall{}, false, 0
	if newStep {
		t.n = 0
	}
}

// newCall numbers a call and reports it started. mu is held.
func (t *turn) newCall(callID, name string) *toolCall {
	t.n++
	c := &toolCall{id: fmt.Sprintf("t%d.%d.%d", t.number, t.step, t.n), callID: callID, name: name}
	t.list = append(t.list, c)
	spec := t.tools.byID[name]
	t.emit(ToolStarted{ID: c.id, Step: t.step, Tool: name, Kind: spec.Kind, ReadOnly: spec.ReadOnly})
	return c
}

// toolInputStart is OnToolInputStart: the model began a call.
func (t *turn) toolInputStart(callID, name string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.firstOutput()
	t.newCall(callID, name)
	return nil
}

// match finds the recorded call c stands for: the first not yet taken with
// its provider id, announced ones first when announced is set, else a
// started one; with neither, a new call, reported started. taken marks it.
// mu is held.
func (t *turn) match(callID, name string, announced bool, taken map[*toolCall]bool) *toolCall {
	for _, want := range []bool{true, false} {
		if want && !announced {
			continue
		}
		for _, c := range t.list {
			if !taken[c] && c.called == want && c.callID == callID {
				taken[c] = true
				return c
			}
		}
	}
	c := t.newCall(callID, name)
	taken[c] = true
	return c
}

// toolCall is OnToolCall: a call whose arguments are complete. It is
// prepared now, in call order, so ToolCalled carries what the gate will
// judge; a call Fantasy found invalid will never run, so it is released at
// once. An empty or repeated provider id marks the step bad: its calls
// cannot be paired with their results, so none of them runs (Run checks),
// and it is not persisted. It always returns nil (see call).
func (t *turn) toolCall(tc fantasy.ToolCallContent) error {
	if tc.ProviderExecuted {
		// The provider ran it and returned its result in the same message;
		// Fantasy records both there, exempt from pairing.
		return nil
	}
	t.mu.Lock()
	taken := map[*toolCall]bool{}
	for _, c := range t.list {
		taken[c] = c.called
	}
	c := t.match(tc.ToolCallID, tc.ToolName, false, taken)
	c.called, c.name, c.input = true, tc.ToolName, tc.Input
	t.announcedN++
	c.order = t.announcedN
	if _, dup := t.byCallID[tc.ToolCallID]; dup || tc.ToolCallID == "" {
		t.bad = true
	} else {
		t.byCallID[tc.ToolCallID] = c
	}
	t.mu.Unlock()

	// Prepare parses and resolves, and never acts; outside the lock, since
	// only this goroutine touches c meanwhile and no tool runs until every
	// call of the step is announced.
	req, _, _ := t.tools.d.Prepare(tool.Call{ID: c.id, CallID: tc.ToolCallID, Tool: tc.ToolName, Input: json.RawMessage(tc.Input)})
	if tc.Invalid {
		t.tools.d.Discard(c.id)
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	c.at = time.Now()
	t.emit(ToolCalled{ID: c.id, CallID: req.CallID, Request: requestOf(req), At: c.at})
	// The doom-loop guard (doomloop.go) sees every announced call here, in
	// call order across steps, invalid ones included, and refuses one by
	// setting its veto, which runTool returns instead of running it. Fantasy
	// runs nothing until every call of the step has been through this
	// callback (agent.go:1755-1782), so a veto is always in place in time.
	t.vet(c)
	return nil
}

// planApprovedVeto answers a call that would have run after the person
// approved the plan, in the same step (runTool).
const planApprovedVeto = "Not executed: the plan was approved and the turn ended."

// refusedByApproval reports whether c is refused because the person approved
// the plan: the plan was approved, and the model placed c after the call that
// asked (planWasApproved fixes that place). mu is held.
func (t *turn) refusedByApproval(c *toolCall) bool {
	return t.planApproved && c.order > t.approvedAt
}

// runTool is every bridged tool's Run, on Fantasy's tool goroutines. The
// dispatcher runs the call under the id OnToolCall prepared it with; a call
// of a bad step, or one the guard refused, is released unrun. It never
// fails: Fantasy treats a tool's Go error as fatal to the turn.
func (t *turn) runTool(ctx context.Context, call fantasy.ToolCall) fantasy.ToolResponse {
	t.mu.Lock()
	c, bad := t.byCallID[call.ID], t.bad
	var veto *tool.Result
	if c != nil {
		// A plan approved in this step refuses every call the model placed
		// after the one that asked, so [exit_plan_mode, write(plan)] cannot
		// change a plan the person has just approved (plan 023 §3.4). It is the
		// call's place in the step that decides, not when its goroutine got
		// here: exit_plan_mode is not Parallel, so Fantasy runs it on the
		// goroutine that dispatches the step's calls (agent.go:1675-1704) and
		// nothing after it starts until it has returned — but a Parallel call
		// placed before it may still be waiting for its goroutine, or for one
		// of Fantasy's five slots, when the approval lands, and that call is
		// the model's own reading before it asked: it runs. The guard's own
		// veto, when there is one, stands.
		if t.refusedByApproval(c) && c.veto == nil {
			c.veto = &tool.Result{Text: planApprovedVeto, IsError: true, Class: tool.ClassNotExecuted}
		}
		veto = c.veto
		c.running = veto == nil && !bad
	}
	t.mu.Unlock()

	var res tool.Result
	switch {
	case c == nil: // unreachable: Fantasy runs only calls OnToolCall announced
		return toResponse(tool.Result{IsError: true, Class: tool.ClassToolError,
			Text: t.redactor().String(fmt.Sprintf("internal error: no tool call is recorded under id %q", call.ID))}, "")
	case bad:
		t.tools.d.Discard(c.id)
		res = badIDsResult(t, call.Name)
	case veto != nil:
		t.tools.d.Discard(c.id)
		res = *veto
	default:
		res = t.tools.d.Run(ctx, c.id, func(snapshot string) { t.progress(c, snapshot) })
	}
	t.mu.Lock()
	c.res, c.running = &res, false
	t.mu.Unlock()
	return toResponse(res, c.id)
}

// progress is a running call's progress: sent while the call is running,
// and dropped once it has finished — a snapshot can arrive after its Run
// returned — and whenever the turn's lock is busy, rather than wait for it:
// the lock may be held by a sink call blocked on its consumer, and progress
// must never wait on one (plan 019 §3.5). A snapshot is the whole output so
// far, so the next one makes up for any dropped.
func (t *turn) progress(c *toolCall, snapshot string) {
	if !t.mu.TryLock() {
		return
	}
	defer t.mu.Unlock()
	if !c.done {
		t.emit(ToolProgress{ID: c.id, Output: snapshot})
	}
}

// toolResult is OnToolResult, on the goroutine that ran the call (or, for a
// call Fantasy refused as invalid, the one that would have): the call's
// ToolFinished, with the dispatcher's result, or for a refused call the
// text Fantasy sends the model. A bad step's calls are settled at the step's
// end instead.
func (t *turn) toolResult(r fantasy.ToolResultContent) error {
	if r.ProviderExecuted {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	c := t.byCallID[r.ToolCallID]
	if t.bad || c == nil || c.done {
		return nil
	}
	c.output = r.Result
	res := c.res
	if res == nil {
		text, _ := outputText(r.Result)
		res = &tool.Result{Text: t.redactor().String(text), IsError: true, Class: tool.ClassInvalidInput}
	}
	t.finishCall(c, *res)
	return nil
}

// finishCall sends c's ToolFinished, once. mu is held.
func (t *turn) finishCall(c *toolCall, res tool.Result) {
	if c.done {
		return
	}
	c.done = true
	now := time.Now()
	var d time.Duration
	if !c.at.IsZero() {
		d = now.Sub(c.at)
	}
	t.emit(ToolFinished{ID: c.id, Result: res, At: now, Duration: d})
}

// announced counts the step's calls OnToolCall announced. mu is held.
func (t *turn) announced() int {
	n := 0
	for _, c := range t.list {
		if c.called {
			n++
		}
	}
	return n
}

// settleWhy is why a call is settled without a result of its own.
type settleWhy int

const (
	// incomplete: the call never arrived whole — its step ended, failed or
	// was retried before its arguments were complete.
	incomplete settleWhy = iota
	// aborted: the turn was cancelled.
	aborted
)

// settleCalls finishes every call of the step that has no ToolFinished yet,
// and releases every call the dispatcher still holds for it, so no row is
// left pending and no prepared call leaks (a no-op for a call that ran).
// mu is held.
func (t *turn) settleCalls(why settleWhy) {
	t.settle(func(c *toolCall) tool.Result {
		if why == aborted {
			return tool.Result{Text: tool.AbortedText, IsError: true, Class: tool.ClassAborted}
		}
		return notRun(t, c.name, "the response ended before the call was complete.")
	})
}

func (t *turn) settle(result func(*toolCall) tool.Result) {
	for _, c := range t.list {
		t.tools.d.Discard(c.id)
		if !c.done {
			t.finishCall(c, result(c))
		}
	}
}

// stepResults answers a finished step's calls and settles the rest. open are
// the calls of the step's assistant message the next message must answer.
// It returns the tool message the step persists — Fantasy's results, and
// for each call Fantasy did not run a not_executed result of the runner's
// own, all in call order — or nil when there are no calls.
//
// Fantasy runs a step's calls only on a "tool-calls" finish; on "length",
// "error", "content-filter" or "unknown" it records them and neither
// announces nor runs them (agent.go:1707-1734), and on any other finish it
// announces but does not run them. Each such call gets the result D-43
// words, which the model reads on its next turn.
//
// bad reports a step that cannot be persisted at all: a call whose provider
// id is empty or repeats another's cannot be paired with its result (plan
// 019 §3.6). None of its calls ran (runTool); each is settled invalid_input.
//
// answered are the recorded calls whose own result — the one Fantasy recorded
// from running it — is in results, rather than one the runner wrote for a
// call that did not run: what an agent_output call's reservation commits by
// (plan 026 §3.11). mu is held.
func (t *turn) stepResults(step fantasy.StepResult, open []fantasy.ToolCallPart) (results *fantasy.Message, bad bool, answered []*toolCall) {
	finish := step.FinishReason
	defer t.settle(func(c *toolCall) tool.Result {
		// A call begun and never completed: under an abnormal finish, the
		// provider dropped its cut-off arguments (openaicompat does).
		if abnormal(finish) {
			return notExecuted(t, c.name, finish)
		}
		return notRun(t, c.name, "the response ended before the call was complete.")
	})

	taken := map[*toolCall]bool{}
	recorded := make([]*toolCall, len(open))
	for i, oc := range open {
		recorded[i] = t.match(oc.ToolCallID, oc.ToolName, true, taken)
	}
	if t.bad || !pairable(open) {
		for i, oc := range open {
			t.finishCall(recorded[i], badIDsResult(t, oc.ToolName))
		}
		return nil, true, nil
	}
	if len(open) == 0 {
		return nil, false, nil
	}

	ran := map[string]fantasy.ToolResultPart{}
	if tm := toolMessageOf(step.Messages); tm != nil {
		for _, p := range tm.Content {
			if r, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](p); ok && !r.ProviderExecuted {
				ran[r.ToolCallID] = r
			}
		}
	}
	parts := make([]fantasy.MessagePart, 0, len(open))
	unrun := 0
	for i, oc := range open {
		if r, ok := ran[oc.ToolCallID]; ok {
			parts = append(parts, r)
			answered = append(answered, recorded[i])
			continue
		}
		// A call the guard refused is answered with the guard's own text,
		// whichever finish carried it: Fantasy dispatches only on a
		// "tool-calls" finish, so under any other one the veto never reached
		// runTool, and D-43's "not executed, its arguments may be truncated"
		// would tell the model the opposite of what happened. The refusal is
		// already in the guard's own Diag (plan 019 §3.7), so it is not
		// counted among the calls a finish left unrun.
		res := notExecuted(t, oc.ToolName, finish)
		if v := recorded[i].veto; v != nil {
			res = *v
		} else {
			unrun++
		}
		t.finishCall(recorded[i], res)
		parts = append(parts, resultPart(oc.ToolCallID, recorded[i].id, res))
	}
	if unrun > 0 {
		t.emit(Diag{Kind: DiagNotExecuted, Fields: map[string]string{
			"step": strconv.Itoa(t.step), "finish": string(finish), "calls": strconv.Itoa(unrun),
		}})
	}
	return &fantasy.Message{Role: fantasy.MessageRoleTool, Content: parts}, false, answered
}

// synthesizeStep persists the step a cancel or a failure cut short after it
// announced tool calls: an assistant message built from what streamed and
// the calls as announced, and a tool message with each call's result as
// Fantasy recorded it, or aborted for a call that has none — both lines
// marked interrupted. Under Fantasy v0.43.2 it never runs: once tools are
// dispatched the step always finishes (OnStepFinish), and the step is
// persisted there; it is the defence for a Fantasy that returns without
// that. done is false, and nothing written, when the step announced no
// call, or none that can be paired — then only its text is saved. mu is
// held.
func (t *turn) synthesizeStep(stop string) (done bool, err error) {
	var announced []*toolCall
	for _, c := range t.list {
		if c.called {
			announced = append(announced, c)
		}
	}
	ids := make([]fantasy.ToolCallPart, len(announced))
	for i, c := range announced {
		ids[i] = fantasy.ToolCallPart{ToolCallID: c.callID}
	}
	if len(announced) == 0 || !pairable(ids) {
		return false, nil
	}

	var parts []fantasy.MessagePart
	if r := t.reasoning.String(); r != "" {
		parts = append(parts, fantasy.ReasoningPart{Text: r})
	}
	if s := t.text.String(); s != "" {
		parts = append(parts, fantasy.TextPart{Text: s})
	}
	results := make([]fantasy.MessagePart, 0, len(announced))
	aborts := 0
	var answered []*toolCall // the calls whose own recorded result is written (stepResults)
	for _, c := range announced {
		parts = append(parts, fantasy.ToolCallPart{ToolCallID: c.callID, ToolName: c.name, Input: c.input})
		if c.output != nil {
			results = append(results, fantasy.ToolResultPart{ToolCallID: c.callID, Output: c.output, ClientMetadata: metadata(c.id)})
			answered = append(answered, c)
			continue
		}
		aborts++
		res := tool.Result{Text: tool.AbortedText, IsError: true, Class: tool.ClassAborted}
		t.finishCall(c, res)
		results = append(results, resultPart(c.callID, c.id, res))
	}
	assistant := fantasy.Message{Role: fantasy.MessageRoleAssistant, Content: parts}
	toolMsg := fantasy.Message{Role: fantasy.MessageRoleTool, Content: results}
	t.emit(Diag{Kind: DiagSynthesized, Fields: map[string]string{
		"step": strconv.Itoa(t.step), "calls": strconv.Itoa(len(announced)), "aborted": strconv.Itoa(aborts),
	}})
	// A child that ran for one of these calls was billed whether or not its
	// step finished, so its usage is written with the results as it is on a
	// finished step's tool entry (plan 026 §3.7) — and a background result an
	// agent_output call read, only when its own result is written here
	// (§3.11). The step's request carried the background results its
	// boundary took up, and they lead the append as they would a finished
	// step's; the user's steers do not, and go back to them (Unanswered), as
	// before.
	writes := func(c *toolCall) bool { return slices.Contains(answered, c) }
	leading, lead := t.internalEntries()
	entries, err := t.store.AppendStep(leading,
		store.MessageEntry{Message: redactCalls(t.redactor(), assistant), Model: t.model.id(), Effort: t.model.effort, StopReason: stop, Interrupted: true},
		&store.MessageEntry{Message: redactResults(t.redactor(), toolMsg), Model: t.model.id(), Effort: t.model.effort, Interrupted: true,
			SubagentUsage: subagentUsage(announced, writes)})
	if err == nil {
		t.wrote(entries, lead, true, outputCalls(answered))
	}
	return true, err
}

// outputCalls are the harness ids of the agent_output calls among answered:
// the calls whose own result a written tool entry holds, and so the ones
// whose reservations it commits (plan 026 §3.11).
func outputCalls(answered []*toolCall) []string {
	var ids []string
	for _, c := range answered {
		if c.name == tool.AgentOutputTool {
			ids = append(ids, c.id)
		}
	}
	return ids
}

// subagentUsage is what the sub-agents of calls spent, one row per model —
// provider, alias and wire id together — in the order the first call that
// ran on each was placed, each row the sum of every such child's usage (plan
// 026 §3.7). A call's usage is its result's Child, which the runner fills from
// the StepDones it observed, so a child that failed or was cancelled counts
// for every step it was billed for; a child that spent nothing adds no row,
// and calls with no child give nil. mu is held.
//
// written says whether an agent_output call's result counts (plan 026
// §3.11): its Child is a background child's usage, which belongs to the entry
// that delivers the result — the tool entry holding that call's own result —
// and to no other, so a StepDone, whose rows a restore would deliver again,
// counts none (nil), and an entry counts only the calls whose results it
// writes. An agent call's child counts wherever it ran, as it always has.
func subagentUsage(calls []*toolCall, written func(*toolCall) bool) []store.ModelUsage {
	var children []*tool.ChildUsage
	for _, c := range calls {
		if c.res == nil || c.res.Child == nil {
			continue
		}
		if c.name == tool.AgentOutputTool && (written == nil || !written(c)) {
			continue
		}
		children = append(children, c.res.Child)
	}
	return mergeUsage(children)
}

// mergeUsage is children's usage as rows per model (subagentUsage), in the
// order each model first appears; a child that spent nothing adds no row, and
// no row at all is nil.
func mergeUsage(children []*tool.ChildUsage) []store.ModelUsage {
	var rows []store.ModelUsage
	for _, ch := range children {
		if ch == nil || ch.Usage == (tool.Usage{}) {
			continue
		}
		u := store.Usage{Input: ch.Usage.Input, Output: ch.Usage.Output, Reasoning: ch.Usage.Reasoning,
			CacheRead: ch.Usage.CacheRead, CacheCreation: ch.Usage.CacheCreation}
		i := slices.IndexFunc(rows, func(r store.ModelUsage) bool {
			return r.Provider == ch.Provider && r.Model == ch.Model && r.WireModel == ch.WireModel
		})
		if i < 0 {
			rows = append(rows, store.ModelUsage{Provider: ch.Provider, Model: ch.Model, WireModel: ch.WireModel})
			i = len(rows) - 1
		}
		r := &rows[i].Usage
		r.Input, r.Output, r.Reasoning = r.Input+u.Input, r.Output+u.Output, r.Reasoning+u.Reasoning
		r.CacheRead, r.CacheCreation = r.CacheRead+u.CacheRead, r.CacheCreation+u.CacheCreation
	}
	return rows
}

// pairable reports whether calls can be answered pairably: every provider
// id present, none repeated (the store's pairing invariant, plan 019 §3.6).
func pairable(calls []fantasy.ToolCallPart) bool {
	seen := make(map[string]bool, len(calls))
	for _, c := range calls {
		if c.ToolCallID == "" || seen[c.ToolCallID] {
			return false
		}
		seen[c.ToolCallID] = true
	}
	return true
}

// abnormal reports a finish on which Fantasy records a step's calls but
// neither announces nor runs them (agent.go:1712-1715).
func abnormal(r fantasy.FinishReason) bool {
	switch r {
	case fantasy.FinishReasonLength, fantasy.FinishReasonError, fantasy.FinishReasonContentFilter, fantasy.FinishReasonUnknown:
		return true
	}
	return false
}

// notExecuted is the result of a call a finish left unrun (D-43): the
// model reads it, and is told to try again with the call whole.
func notExecuted(t *turn, name string, finish fantasy.FinishReason) tool.Result {
	why := "the response hit the output token limit"
	if finish != fantasy.FinishReasonLength {
		why = fmt.Sprintf("the response ended with finish reason %q", string(finish))
	}
	return notRun(t, name, why+", so its arguments may be truncated. Re-issue the tool call with complete arguments.")
}

// notRun is a not_executed result: `Tool call "<name>" was not executed: `
// and why. The name is the model's text, so the result is redacted.
func notRun(t *turn, name, why string) tool.Result {
	return tool.Result{
		Text:    t.redactor().String(fmt.Sprintf("Tool call %q was not executed: %s", name, why)),
		IsError: true,
		Class:   tool.ClassNotExecuted,
	}
}

// badIDsResult is the result of a call in a bad step (ErrBadToolCalls). The
// model never reads it — the turn ends and the step is not persisted — but
// the call's card does.
func badIDsResult(t *turn, name string) tool.Result {
	return tool.Result{
		Text: t.redactor().String(fmt.Sprintf("Tool call %q was not run: the response's tool calls had missing or repeated ids, "+
			"so none of them could be answered.", name)),
		IsError: true,
		Class:   tool.ClassInvalidInput,
	}
}

// resultPart is a tool result part for res, answering callID, with the
// harness id in its client metadata, as a bridged run's has.
func resultPart(callID, id string, res tool.Result) fantasy.ToolResultPart {
	r := toResponse(res, id)
	var out fantasy.ToolResultOutputContent = fantasy.ToolResultOutputContentText{Text: r.Content}
	if r.IsError {
		out = fantasy.ToolResultOutputContentError{Error: errors.New(r.Content)}
	}
	return fantasy.ToolResultPart{ToolCallID: callID, Output: out, ClientMetadata: r.Metadata}
}

// toResponse is res as Fantasy takes it. An error result must carry text:
// Fantasy stores it as an error whose text reads back empty as no error at
// all, which the store refuses (it would panic a replay). Metadata carries
// the harness id, into the transcript's result part, where it joins the
// result to the call's events; no provider sends it.
func toResponse(res tool.Result, id string) fantasy.ToolResponse {
	text := res.Text
	if res.IsError && strings.TrimSpace(text) == "" {
		text = "The tool call failed and said nothing more."
	}
	return fantasy.ToolResponse{Type: "text", Content: text, IsError: res.IsError, StopTurn: res.StopTurn, Metadata: metadata(id)}
}

// metadata is a result's client metadata: {"id":"<harness id>"}, or "" for
// none.
func metadata(id string) string {
	if id == "" {
		return ""
	}
	b, _ := json.Marshal(struct {
		ID string `json:"id"`
	}{id}) // a struct of one string always marshals
	return string(b)
}

// outputText is the text of a result as Fantasy recorded it, and whether it
// is an error.
func outputText(o fantasy.ToolResultOutputContent) (string, bool) {
	if e, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentError](o); ok {
		if e.Error == nil {
			return "", true
		}
		return e.Error.Error(), true
	}
	if s, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentText](o); ok {
		return s.Text, false
	}
	if m, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentMedia](o); ok {
		return m.Text, false
	}
	return "", false
}

// requestOf is the event's copy of the dispatcher's redacted Request.
func requestOf(r tool.Request) ToolRequest {
	return ToolRequest{
		Tool:     r.Tool,
		Kind:     r.Kind,
		ReadOnly: r.ReadOnly,
		Title:    r.Title,
		Paths:    slices.Clone(r.Paths),
		Command:  r.Command,
		Workdir:  r.Workdir,
		Input:    string(r.Input),
	}
}

// agentTools are the session's tools as Fantasy takes them, in the
// profile's order, each running through this turn.
func (t *turn) agentTools() []fantasy.AgentTool {
	out := make([]fantasy.AgentTool, len(t.tools.specs))
	for i, s := range t.tools.specs {
		out[i] = newBridged(s, t.runTool)
	}
	return out
}

// bridged is a tool as fantasy.AgentTool, implemented directly rather than
// with NewAgentTool, which would derive the schema by reflection: the schema
// is the Spec's own, hand-written.
type bridged struct {
	spec   tool.Spec
	params map[string]any // the Spec's parameters, canonical (see newBridged)
	run    func(context.Context, fantasy.ToolCall) fantasy.ToolResponse
}

// newBridged offers spec to Fantasy. The Spec's parameters are canonicalized
// once here, so Info can copy them wholly and cheaply for every request
// (canonicalSchema, Info).
func newBridged(spec tool.Spec, run func(context.Context, fantasy.ToolCall) fantasy.ToolResponse) *bridged {
	return &bridged{spec: spec, params: canonicalSchema(spec.Parameters), run: run}
}

// Info is the tool as the model is offered it. The parameters are a deep
// copy every time: Fantasy normalizes the schema it sends in place
// (schema.Normalize, agent.go:1131-1137), and a Spec's maps are shared by
// every step and turn of the session — and, since a profile may build one
// however it likes, by anything else holding the same values. Required is
// never nil, so the wire carries [], never null.
func (b *bridged) Info() fantasy.ToolInfo {
	params, _ := cloneSchema(b.params).(map[string]any)
	if params == nil {
		params = map[string]any{}
	}
	req := slices.Clone(b.spec.Required)
	if req == nil {
		req = []string{}
	}
	return fantasy.ToolInfo{
		Name:        b.spec.ID,
		Description: b.spec.Description,
		Parameters:  params,
		Required:    req,
		Parallel:    b.spec.Parallel,
	}
}

// Run runs the call through the turn. Its error is always nil (runTool).
func (b *bridged) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	return b.run(ctx, call), nil
}

// ProviderOptions is nil: craze's tools carry no provider options, so the
// request's tools are the specs and nothing else.
func (b *bridged) ProviderOptions() fantasy.ProviderOptions { return nil }

// SetProviderOptions is ignored, for the same reason.
func (b *bridged) SetProviderOptions(fantasy.ProviderOptions) {}

// canonicalSchema returns a Spec's parameters as JSON's own values —
// map[string]any, []any, string, json.Number, bool, nil — through one round
// trip. A Spec may hold any value that marshals (a []map[string]any, a named
// map type, a struct), and a copy that walked only Go's canonical shapes
// would share whatever else it met, so the round trip is what makes
// cloneSchema total. It runs once per tool per turn; after it, every copy is
// a few map allocations.
//
// The bytes are unchanged by it, which is why the header's hash of the specs
// describes what is sent: every number is kept as the literal it was written
// as (UseNumber), so one past float64's exact range, or written 1.0, goes
// out as it came in. A value that does not marshal cannot be here (the
// registry refuses such a Spec); if one ever is, the tool is offered with no
// parameters rather than with a shared map.
func canonicalSchema(params map[string]any) map[string]any {
	if len(params) == 0 {
		return map[string]any{}
	}
	b, err := json.Marshal(params)
	if err != nil {
		return map[string]any{}
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var out map[string]any
	if err := dec.Decode(&out); err != nil || out == nil {
		return map[string]any{}
	}
	return plainNumbers(out).(map[string]any)
}

// plainNumbers rewrites every json.Number UseNumber left behind as an int64
// or a float64. The decoder keeps a number's literal, which is what makes the
// copy faithful, but a json.Number is a string underneath and only Go's own
// encoder knows to write it as a number: a provider SDK that marshals the
// schema itself sends "9007199254740991", and a provider that validates its
// tool schemas rejects the call (Fireworks: 'is not of type number'), which is
// how the live smoke found this. int64 keeps every integer literal a provider
// can hold exactly; anything else becomes the float64 it parses as, so an
// integer past int64 (a `maximum` of 2^64-1, say) is rounded — the price of
// being a number at all, since the SDK writes the exact json.Number as a
// string the provider then rejects (CodeRabbit finding, checked against
// openai-go v3.54.0's own encoder). A literal that is neither is left as it
// is: no wire format could carry it anyway.
func plainNumbers(v any) any {
	switch v := v.(type) {
	case map[string]any:
		for k, x := range v {
			v[k] = plainNumbers(x)
		}
		return v
	case []any:
		for i, x := range v {
			v[i] = plainNumbers(x)
		}
		return v
	case json.Number:
		if i, err := v.Int64(); err == nil {
			return i
		}
		if f, err := v.Float64(); err == nil {
			return f
		}
		return v
	}
	return v
}

// cloneSchema is a deep copy of a canonical JSON Schema value
// (canonicalSchema): objects and arrays are copied all the way down;
// strings, numbers and booleans are values already.
func cloneSchema(v any) any {
	switch v := v.(type) {
	case map[string]any:
		if v == nil {
			return map[string]any(nil)
		}
		out := make(map[string]any, len(v))
		for k, x := range v {
			out[k] = cloneSchema(x)
		}
		return out
	case []any:
		if v == nil {
			return []any(nil)
		}
		out := make([]any, len(v))
		for i, x := range v {
			out[i] = cloneSchema(x)
		}
		return out
	}
	return v
}
