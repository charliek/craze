package harness

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"uuid"

	"github.com/charliek/craze/internal/harness/store"
	"github.com/charliek/craze/internal/harness/tool"
)

// Background children (plan 026 §3.11). A session opened with
// Options.Background runs an agent call that asks for it (run_in_background)
// in the background: the call opens its child, reports it started and returns
// an acknowledgement, and the child's turn runs on a goroutine of the
// runner's under the session's own context, so a turn's cancel never reaches
// it — only its user's stop (CancelSubagent) or Close.
//
// # A result's delivery
//
// When the child ends, its result — built exactly as a foreground call's
// answer would be, and cut to the shared limits then, since no dispatcher
// will cut it later — waits to be read by the parent's model, once. Under
// regMu each result is in one state:
//
//   - running: the child has not ended;
//   - pending: ended, waiting to be delivered;
//   - reserved: taken by one consumer — a step of a turn (its request carries
//     the result), a wake's first step (its prompt is the result), or an
//     agent_output call (its tool result is the result) — which writes it or
//     gives it back;
//   - committed: written, for good, by the append that delivered it;
//   - suspended: a wake took it and wrote nothing. No wake takes it again
//     (no automatic retry loop, P44, P51); the next turn a person starts, or
//     an agent_output call, delivers it.
//
// A consumer is a step, and one turn runs at a time, its steps in order, so a
// reservation is always the running step's or else already committed or given
// back before any tool of a later step runs (P47): nothing ever waits on a
// reservation. A step takes every pending result at its boundary — step 0
// included, and at the step 0 of a turn a person started every suspended one
// as well — as one user part, which it re-inserts into every later request of
// the turn, never through the steer box: a result is never in
// Result.Unanswered, never counts against the steer cap and emits no Steered
// (turn.internal). The append that writes a step, a partial answer or a
// synthesized step commits exactly the reservations it wrote — the parts it
// carries as leading entries, a wake's prompt, an agent_output call whose own
// result is in its tool entry — and nothing else; when the turn has ended,
// every reservation it made that no append wrote goes back to what it was, or
// to suspended when the turn was a wake (restoreTurn). An append that
// succeeded wins over the cancel that follows it.
//
// A result's usage is attributed once, to the entry that commits it: the
// user entry that carries the result (subagent_usage), or agent_output's
// tool entry through its Result.Child. The spawning call's entry carries none.
// A result the session closes before delivering is reported instead, once,
// as a SubagentUndelivered (closeBackground). Nothing persists the results
// themselves: a session that closes forgets them.
//
// # Locks
//
// regMu guards every result's state, as it guards the registry, and is held
// across nothing but those fields: a result's text is redacted and formatted
// after it is taken, never under the lock (the redactor's own keys take
// regMu), and no sink, store write, wait or child teardown happens under it.
// A turn takes regMu under its own lock (t.mu → regMu), never the other way
// round.

// delivery is where a background child's result is. See the file's comment.
type delivery int

const (
	resultRunning delivery = iota
	resultPending
	resultReserved
	resultCommitted
	resultSuspended
)

// owner is the consumer a reservation belongs to: the step of turn that took
// it — its number from 1, t.step's, which names the step its append writes —
// and, for an agent_output call, the call's harness id. wake says the turn
// was a wake, so a reservation it leaves unwritten suspends (restoreTurn).
type owner struct {
	turn, step int
	call       string
	wake       bool
}

// bgResult is one background child's result and its delivery state; every
// field but the fixed ones is guarded by regMu.
type bgResult struct {
	id, typ string // the child's id, and its agent type, redacted
	callID  string // the agent call that started it: its result's spill file is named for it

	state, prior delivery // prior: what a reservation gives back to (pending or suspended)
	own          owner    // a reservation's
	// status, text and usage are the result, once the child has ended: its
	// SubagentFinished status, the text the parent's model reads — redacted,
	// cut to the shared limits, its spill path redacted with it — and the
	// usage observed on the child's steps, its model's names redacted.
	status string
	text   string
	usage  *tool.ChildUsage
	// keys are the keys the child knew, covered by the session's redaction
	// until the result is committed — moved into spent then, for the rest of
	// the delivering turn — or reported undelivered (childKeys).
	keys  []string
	seq   int    // the order it finished in, from 1
	entry string // the store id of the entry that committed it
	// done is closed when the child's result leaves running: what an
	// agent_output call waiting for it waits on.
	done     chan struct{}
	reported bool // Close has reported it undelivered
}

// taken is a result as a consumer took it: what it formats the model's text
// from.
type taken struct {
	id, typ, status, text string
	usage                 *tool.ChildUsage
	seq                   int
}

// batch is results one consumer took, as the model reads them: their ids, the
// text of the one user part (or tool result) they make, and their usage, a
// row per model.
type batch struct {
	ids  []string
	text string
	rows []ModelUsage
}

// The texts a background call and an agent_output call answer with, craze's
// own (plan 026 §3.11). None is an error: the model can act on each.
const (
	backgroundStarted  = "Started sub-agent %s (%s) in the background. Its result will be delivered to you when it finishes; call agent_output with its id to wait for it now."
	outputStillRunning = "Sub-agent %s is still running."
	outputIncluded     = "That result is already included in this turn's input."
	outputDelivered    = "That result was already delivered to you."
	// outputUnknownCap bounds the ids an unknown id's refusal lists.
	outputUnknownCap = 8
)

// bgChild is what a background child's goroutine owns: the child, its handle,
// its context and its result, and what it needs to report and cut them.
type bgChild struct {
	h      *childHandle
	res    *bgResult
	child  *Session
	ctx    context.Context
	cancel context.CancelCauseFunc
	prompt string      // the call's prompt, as the model wrote it: redacted just before the child is sent it
	ran    store.Model // the model the child runs on
	start  time.Time
	// gate is closed once the call has reported the child started, so every
	// event of the child comes after its SubagentStarted (§3.9's order).
	gate chan struct{}
}

// runBackground is a background call from its slot on (§3.11): it takes a
// slot, never waiting for one; registers the child and opens it, on the
// call's goroutine, under the session's background context; hands it to a
// goroutine of the runner's, counted before it starts; reports it started
// through the turn's lock; and returns the acknowledgement. Everything before
// the handover is undone as a foreground call's is — an Open that fails is
// that call's failure, a cancel or a Close before it is aborted — and nothing
// is undone after it: the child, its handle, its slot and its context are the
// goroutine's (work).
func (r *subagents) runBackground(ctx context.Context, link *turnLink, call tool.SubagentCall,
	persona tool.Persona, alias, effort string, c *childCall) tool.Result {
	parent := r.s
	switch r.take(ctx, call, true) {
	case slotBusy:
		return busyResult()
	case slotAborted:
		return abortedResult()
	}
	var b *bgChild // set once the goroutine owns the child: the deferred undoing below does nothing then
	defer func() {
		if b == nil {
			r.releaseSlot(true)
		}
	}()
	if r.seams.acquired != nil {
		r.seams.acquired(call)
	}
	id := uuid.NewV4().String()
	childCtx, cancel := context.WithCancelCause(r.bgCtx)
	defer func() {
		if b == nil {
			cancel(nil)
		}
	}()
	h, mode, ok := r.register(id, cancel)
	if !ok {
		return abortedResult()
	}
	c.h = h
	defer func() {
		if b == nil {
			r.retire(h)
		}
	}()
	child, err := r.openChild(h, call, persona, alias, effort, mode)
	if err != nil {
		return failedResult(err.Error(), "")
	}
	c.child = child
	defer func() {
		if b == nil {
			_ = child.Close()
		}
	}()
	if !h.attachChild(child) || ctx.Err() != nil {
		return abortedResult()
	}
	if r.seams.opened != nil {
		r.seams.opened(h.id, child)
	}
	child.mu.Lock()
	ran := child.cur.id()
	child.mu.Unlock()
	red := r.union(child)
	typ := red.String(persona.Name)
	res, ok := r.launch(id, typ, call.ID)
	if !ok {
		return abortedResult() // Close sealed the registry since the child registered
	}
	b = &bgChild{h: h, res: res, child: child, ctx: childCtx, cancel: cancel, prompt: call.Prompt, ran: ran,
		start: time.Now(), gate: make(chan struct{})}
	go r.work(b)
	// The gate opens however this returns, a panicking sink included, so the
	// goroutine counted in launch always runs to its end.
	defer close(b.gate)
	c.final = true
	link.emit(SubagentStarted{
		ID: h.id, CallID: call.ID, Type: typ, Description: red.String(call.Description),
		Prompt: red.String(call.Prompt), Model: red.String(alias), Effort: red.String(effort), Mode: mode,
		At: parent.now(), Background: true,
	})
	return tool.Result{Text: fmt.Sprintf(backgroundStarted, id, typ)}
}

// launch records a background child's result as running and counts its
// goroutine, both under regMu and only while the registry is unsealed: once
// Close has sealed it, it refuses, and the call is aborted before anything
// was reported. So every goroutine Close's join waits for was counted before
// the seal.
func (r *subagents) launch(id, typ, callID string) (*bgResult, bool) {
	r.regMu.Lock()
	defer r.regMu.Unlock()
	if r.sealed {
		return nil, false
	}
	res := &bgResult{id: id, typ: typ, callID: callID, state: resultRunning, done: make(chan struct{})}
	r.results[id] = res
	r.order = append(r.order, id)
	r.workers.Add(1)
	return res, true
}

// work is a background child's goroutine: it runs the child's turn to its end
// (runToEnd), makes its result deliverable, retires the child and gives its
// slot back — in that order, so a result is pending before its slot frees —
// and then says so (OnPending), holding no lock.
func (r *subagents) work(b *bgChild) {
	defer r.workers.Done()
	defer b.cancel(nil)
	<-b.gate
	text, usage, status := r.runToEnd(b)
	r.publish(b, text, usage, status)
	r.notifyPending(true)
}

// runToEnd runs the child's turn and ends it, in the pinned order (§3.11):
// the end latched, the outcome decided — the parent gone when a Close has
// signalled the child or the session is closing, there being no call to be
// cancelled — the child closed, its SubagentFinished reported through the
// session's sink while it is still registered (so Session.Redact covers its
// keys), and its result built, as a foreground call's answer is, and cut
// (deliverable). A panic anywhere in it — the child's own turn is runTurn's to
// recover, so this is the sink's or the runner's — fails the child, with what
// it spent: a panic on a goroutine nobody recovers would end the process.
func (r *subagents) runToEnd(b *bgChild) (text string, usage *tool.ChildUsage, status string) {
	var obs childObserver
	defer func() {
		if v := recover(); v != nil {
			b.h.end()
			_ = b.child.Close()
			spent, _, _ := obs.totals()
			text, usage = r.deliverable(b, failedResult(childPanic{value: v}.Error(), obs.lastOutput()), spent)
			status = SubagentFailed
		}
	}()
	// The prompt is redacted by a replacer built after the call reported the
	// child started (the gate), as a foreground call's is (review r4).
	prompt := r.union(b.child).String(b.prompt)
	res, runErr := runTurn(b.ctx, b.child, prompt, func(ev Event) {
		obs.observe(ev)
		r.emit(SubagentEvent{ID: b.h.id, Event: ev})
	})
	b.h.end()
	out := subagentOutcome{
		res: res, err: runErr,
		parentGone: b.h.isClosing() || isClosed(r.s.tools.closing),
		stopped:    errors.Is(context.Cause(b.ctx), errStoppedByUser),
	}.decide(&obs)
	_ = b.child.Close()
	spent, calls, steps := obs.totals()
	red := r.union(b.child)
	r.emit(SubagentFinished{
		ID: b.h.id, Status: out.status, Error: red.String(out.errText), Text: red.String(out.text), Usage: spent,
		Model: red.String(b.ran.Alias), Provider: red.String(b.ran.Provider), WireModel: red.String(b.ran.WireModel),
		ToolCalls: calls, Steps: steps, Duration: time.Since(b.start), At: r.s.now(),
	})
	text, usage = r.deliverable(b, out.result, spent)
	return text, usage, out.status
}

// deliverable is a background child's result as the parent's model will read
// it, cut before it can be delivered, since no dispatcher cuts it later
// (panel astra r2-10): the answer redacted with a replacer built now, cut by
// the shared truncator at its limits — the head kept, the whole in a spill
// file named for the spawning call — and redacted again with a fresh one,
// spill path and notice included (X14's order), as settle finishes a
// foreground call's. The usage is what the child's steps were billed for
// (§3.7), its model's names redacted with the text.
func (r *subagents) deliverable(b *bgChild, res tool.Result, spent Usage) (string, *tool.ChildUsage) {
	text := r.union(b.child).String(res.Text)
	text, _ = tool.TruncateRedacted(r.s.base.home, b.res.callID, text, tool.Head, r.union(b.child))
	red := r.union(b.child)
	return red.String(text), &tool.ChildUsage{
		Provider: red.String(b.ran.Provider), Model: red.String(b.ran.Alias), WireModel: red.String(b.ran.WireModel),
		Usage: tool.Usage{Input: spent.Input, Output: spent.Output, Reasoning: spent.Reasoning,
			CacheRead: spent.CacheRead, CacheCreation: spent.CacheCreation},
	}
}

// publish makes a finished background child's result pending and retires the
// child — in that order, in one regMu section — and then gives its slot back.
// The child's keys go with its result (childKeys) — read before regMu, as
// retire reads them — and not into spent, which the running turn's end
// forgets: the result can wait for many turns. Its end was latched when its
// Run returned (runToEnd), so a stop is refused from before this.
func (r *subagents) publish(b *bgChild, text string, usage *tool.ChildUsage, status string) {
	keys := b.child.tools.knownKeys()
	r.regMu.Lock()
	res := b.res
	res.text, res.usage, res.status, res.keys = text, usage, status, keys
	r.finished++
	res.seq = r.finished
	res.state = resultPending
	close(res.done)
	delete(r.live, b.h.id)
	r.regMu.Unlock()
	r.releaseSlot(true)
}

// emit hands ev to the session's sink (Options.Sink), holding no lock of the
// runner's; nil discards.
func (r *subagents) emit(ev Event) {
	if r.sink != nil {
		r.sink(ev)
	}
}

// notifyPending calls Options.OnPending, holding no lock. From a background
// child's goroutine (recovered) a panic in it is dropped: nothing up that
// goroutine could recover it, and the result it announced is pending
// already, which the caller's next check (HasPending) finds.
func (r *subagents) notifyPending(recovered bool) {
	if r == nil || r.onPending == nil {
		return
	}
	if recovered {
		defer func() { _ = recover() }()
	}
	r.onPending()
}

// hasPending reports whether a result is waiting to be delivered: pending, not
// suspended nor reserved, nor reported undelivered by Close, after which
// nothing delivers one.
func (r *subagents) hasPending() bool {
	if r == nil {
		return false
	}
	r.regMu.Lock()
	defer r.regMu.Unlock()
	for _, res := range r.results {
		if res.state == resultPending && !res.reported {
			return true
		}
	}
	return false
}

// HasPending reports whether a background sub-agent's result is waiting to be
// delivered (plan 026 §3.11): a child finished, and no turn, wake or
// agent_output call has taken its result since. A result a failed wake set
// aside does not count: only the next turn a person starts, or agent_output,
// delivers it. It takes one leaf lock and never waits, so Options.OnPending's
// handler may call it; a caller that delivers checks it again whenever a turn
// ends, since a result can become pending while one runs.
func (s *Session) HasPending() bool { return s.subs.hasPending() }

// reserve takes, for own, every result waiting to be delivered — every
// suspended one as well when withSuspended — in the order they finished, and
// returns them as the model reads them (deliver), or nil when there was none.
// The taking is one regMu section; the text is made after it.
func (r *subagents) reserve(own owner, withSuspended bool) *batch {
	if r == nil {
		return nil
	}
	r.regMu.Lock()
	var got []taken
	for _, id := range r.order {
		res := r.results[id]
		if res.state == resultPending || (withSuspended && res.state == resultSuspended) {
			got = append(got, r.reserveLocked(res, own))
		}
	}
	r.regMu.Unlock()
	return r.deliver(got)
}

// reserveLocked takes res for own and returns it as taken. regMu is held.
func (r *subagents) reserveLocked(res *bgResult, own owner) taken {
	res.prior, res.state, res.own = res.state, resultReserved, own
	return taken{id: res.id, typ: res.typ, status: res.status, text: res.text, usage: res.usage, seq: res.seq}
}

// deliver is taken results as the model reads them: each in its wrapper, in
// the order they finished, a blank line between two, the whole redacted once
// more with a replacer built now (astra r14, major 3): a result was redacted
// when it became deliverable, but a key the session learned while it waited
// must not reach the model. Their usage is merged into a row per model, the
// names redacted by the same replacer.
func (r *subagents) deliver(got []taken) *batch {
	if len(got) == 0 {
		return nil
	}
	slices.SortFunc(got, func(a, b taken) int { return a.seq - b.seq })
	red := r.union(nil)
	out := &batch{}
	blocks := make([]string, len(got))
	usages := make([]*tool.ChildUsage, 0, len(got))
	for i, t := range got {
		out.ids = append(out.ids, t.id)
		blocks[i] = resultBlock(t.id, t.typ, t.status, red.String(t.text))
		if u := t.usage; u != nil {
			usages = append(usages, &tool.ChildUsage{Provider: red.String(u.Provider), Model: red.String(u.Model),
				WireModel: red.String(u.WireModel), Usage: u.Usage})
		}
	}
	out.text = red.String(strings.Join(blocks, "\n\n"))
	out.rows = mergeUsage(usages)
	return out
}

// resultBlock is one result in its wrapper (§3.11's format):
//
//	<subagent_result id="<id>" type="<type>" status="<status>">
//	<text>
//	</subagent_result>
//
// The attributes are escaped, and a closing tag inside the text is written
// <\/subagent_result, so nothing a child wrote can end its own wrapper and
// speak outside it.
func resultBlock(id, typ, status, text string) string {
	return `<subagent_result id="` + attrEscaper.Replace(id) + `" type="` + attrEscaper.Replace(typ) +
		`" status="` + attrEscaper.Replace(status) + `">` + "\n" +
		strings.ReplaceAll(text, "</subagent_result", `<\/subagent_result`) + "\n</subagent_result>"
}

// attrEscaper escapes a wrapper attribute's value.
var attrEscaper = strings.NewReplacer("&", "&amp;", `"`, "&quot;", "<", "&lt;", ">", "&gt;")

// commit marks as committed, by the store entry that wrote them, the results
// turn reserved that pick chooses and that are still reserved: an append
// wrote them, which no cancel after it undoes. Each one's keys move into spent,
// covered for the rest of the turn that delivered it.
func (r *subagents) commit(turn int, entry string, pick func(*bgResult) bool) {
	if r == nil {
		return
	}
	r.regMu.Lock()
	defer r.regMu.Unlock()
	for _, res := range r.results {
		if res.state == resultReserved && res.own.turn == turn && pick(res) {
			res.state, res.entry = resultCommitted, entry
			r.spendLocked(res.keys)
			res.keys = nil
		}
	}
}

// commitIDs commits the results turn's steps reserved among ids (commit): the
// ones a user part or a wake's prompt carried.
func (r *subagents) commitIDs(turn int, entry string, ids []string) {
	r.commit(turn, entry, func(res *bgResult) bool { return res.own.call == "" && slices.Contains(ids, res.id) })
}

// commitCalls commits the results turn's agent_output calls among calls
// reserved (commit): the calls whose own result part is in the tool entry the
// append wrote, and no other — not merely because some tool entry was saved.
func (r *subagents) commitCalls(turn int, entry string, calls []string) {
	r.commit(turn, entry, func(res *bgResult) bool { return res.own.call != "" && slices.Contains(calls, res.own.call) })
}

// restoreTurn gives back every reservation turn still holds, once it has
// ended: no append wrote it. It goes back to what it was — pending, or
// suspended — unless the turn was a wake, when it is suspended whatever it
// was (§3.11: no automatic retry loop). It reports whether one went back to
// pending, for the caller to say so (OnPending) once it holds no lock.
func (r *subagents) restoreTurn(turn int) (gave bool) {
	if r == nil {
		return false
	}
	r.regMu.Lock()
	defer r.regMu.Unlock()
	for _, res := range r.results {
		if res.state != resultReserved || res.own.turn != turn {
			continue
		}
		res.state = res.prior
		if res.own.wake {
			res.state = resultSuspended
		}
		res.own = owner{}
		gave = gave || res.state == resultPending
	}
	return gave
}

// Output is tool.Subagents' agent_output (§3.11): by the state of the result
// call.ID names, and never waiting on a reservation (P41, P47):
//
//   - no background child of this session by that id — a foreground child's
//     id included, which is not addressable — is invalid_input, naming the
//     ones whose results are still to be delivered;
//   - committed: it was delivered, and is not repeated;
//   - reserved: by this call's own step, by another agent_output call of it,
//     or — impossible, by P47 — by anything else, it is already in this
//     turn's input, and is not repeated;
//   - pending or suspended: taken for this call, whose step commits it when
//     its append writes this call's result, and its text is the answer, with
//     its usage as the result's Child;
//   - running: the call waits — holding no lock, and never cancelling the
//     child — for it to end, for call.Wait, for its own cancel or for the
//     session's closing, and then judges again; a child still running when
//     the wait runs out is not an error.
func (r *subagents) Output(ctx context.Context, call tool.OutputCall) tool.Result {
	parent := r.s
	switch {
	case parent.child:
		return tool.Result{Text: subagentNested, IsError: true, Class: tool.ClassToolError}
	case ctx.Err() != nil:
		return abortedResult()
	}
	link := r.turn.Load()
	if link == nil {
		return tool.Result{Text: subagentNoTurn, IsError: true, Class: tool.ClassToolError}
	}
	own := owner{turn: link.number, step: stepOfCall(call.CallID), call: call.CallID, wake: link.wake}
	closing := parent.tools.closing
	var timeout <-chan time.Time
	for {
		r.regMu.Lock()
		res := r.results[call.ID]
		if res == nil {
			waiting := r.undeliveredLocked()
			r.regMu.Unlock()
			return unknownSubagent(parent.quoteRaw(call.ID), waiting)
		}
		switch res.state {
		case resultCommitted:
			r.regMu.Unlock()
			return tool.Result{Text: outputDelivered}
		case resultReserved:
			r.regMu.Unlock()
			return tool.Result{Text: outputIncluded}
		case resultPending, resultSuspended:
			got := r.reserveLocked(res, own)
			r.regMu.Unlock()
			b := r.deliver([]taken{got})
			out := tool.Result{Text: b.text}
			if u := got.usage; u != nil {
				red := r.union(nil)
				out.Child = &tool.ChildUsage{Provider: red.String(u.Provider), Model: red.String(u.Model),
					WireModel: red.String(u.WireModel), Usage: u.Usage}
			}
			return out
		}
		done := res.done
		r.regMu.Unlock()
		still := tool.Result{Text: fmt.Sprintf(outputStillRunning, call.ID)}
		if call.Wait <= 0 {
			return still
		}
		if timeout == nil {
			timer := time.NewTimer(call.Wait)
			defer timer.Stop()
			timeout = timer.C
			if r.seams.outputWaiting != nil {
				r.seams.outputWaiting(call.ID)
			}
		}
		select {
		case <-done:
		case <-timeout:
			return still
		case <-ctx.Done():
			return abortedResult()
		case <-closing:
			return abortedResult()
		}
		if ctx.Err() != nil || isClosed(closing) {
			return abortedResult()
		}
	}
}

// undeliveredLocked are the ids of the background children whose results
// have not been delivered, in launch order. regMu is held.
func (r *subagents) undeliveredLocked() []string {
	var ids []string
	for _, id := range r.order {
		if r.results[id].state != resultCommitted {
			ids = append(ids, id)
		}
	}
	return ids
}

// unknownSubagent refuses an agent_output id that names no background child
// of this session, quoted as the refusals of the agent call quote what the
// model sent (quoteRaw, then cut), and lists the ones it could have meant.
func unknownSubagent(quoted string, waiting []string) tool.Result {
	text := "Unknown sub-agent id `" + cutRawValue(quoted) + "`."
	switch {
	case len(waiting) == 0:
		text += " No background sub-agent's result is still to be delivered."
	default:
		shown := waiting[:min(len(waiting), outputUnknownCap)]
		text += " The background sub-agents whose results are still to be delivered: `" + strings.Join(shown, "`, `") + "`"
		if more := len(waiting) - len(shown); more > 0 {
			text += fmt.Sprintf(", … and %d more", more)
		}
		text += "."
	}
	return tool.Result{Text: text, IsError: true, Class: tool.ClassInvalidInput}
}

// stepOfCall is the step a harness call id ("t<turn>.<step>.<n>") names, or 0
// for an id that is not one.
func stepOfCall(id string) int {
	parts := strings.Split(strings.TrimPrefix(id, "t"), ".")
	if len(parts) != 3 {
		return 0
	}
	n, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0
	}
	return n
}

// closeBackground is Close's end of the background children, after it has
// sealed the registry, signalled every child and joined the parent's turn:
// the session's background context is cancelled, every child's goroutine is
// joined — through its result's settlement and its slot's release — and then
// every result that was never committed is reported, once, as a
// SubagentUndelivered through the session's sink: the terminal owner of what
// those children spent (astra r14, major 4), since nothing will deliver them
// now. A nil runner has none.
func (r *subagents) closeBackground() {
	if r == nil {
		return
	}
	r.bgCancel(errClosing)
	r.workers.Wait()
	r.regMu.Lock()
	var out []SubagentUndelivered
	for _, id := range r.order {
		res := r.results[id]
		if res.state == resultCommitted || res.reported {
			continue
		}
		res.reported = true
		var usages []*tool.ChildUsage
		if res.usage != nil {
			usages = append(usages, res.usage)
		}
		out = append(out, SubagentUndelivered{ID: res.id, Type: res.typ, Usage: mergeUsage(usages)})
	}
	r.regMu.Unlock()
	for _, ev := range out {
		r.emit(ev)
	}
}
