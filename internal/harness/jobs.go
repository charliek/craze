package harness

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/charliek/craze/internal/harness/tool"
)

// Background jobs (plan 033 §3.7–§3.8, owner decisions 2 and 3). A job is a
// bash command the session owns rather than the call that started it: one the
// model started with run_in_background, or one that reached its timeout in the
// foreground and was moved to the background (promotion, opencode's
// bash_job.go). The bash tool hands it over through tool.Env.Jobs, which is
// this file's jobs, on the sub-agent runner's registry (background.go): a job
// is a background result of a second kind, kindJob, in the same states, under
// the same lock, delivered the same three ways — at a step boundary, through
// bash_output or bash_stop, or by the wake — exactly once, committed when the
// append that carries it succeeds and given back when none does.
//
// # Lifetime (owner decision 3, §3.8's table)
//
// A job runs on a goroutine of the runner's, counted in workers from its
// slot's reservation on, under a context of its own derived from the
// session's background context — never a turn's or a call's, so Esc and a
// turn's cancel reach only a command's foreground phase. It ends at the first
// of:
//
//	its leader exits                     exited, with its exit code
//	bash_stop                            stopped by="you"          (errJobStoppedByAgent)
//	the stop key (CancelSubagent)        stopped by="the user"     (errJobStoppedByUser)
//	its limit                            timed_out
//	/exit, session.stop, the session end  killed through Close: nothing is delivered
//
// A session's close signals the tools' closing channel, which kills every
// job's command at once, and closeBackground joins their goroutines; a result
// that leaves running then is marked reported and never delivered, and Close
// reports nothing undelivered for a job — it spent nothing — so its
// JobFinished, which the adapter journals, is its one record.
//
// # The cap and the wake chain (P14)
//
// At most maxJobs jobs run at once; a slot is reserved before a command
// starts (an explicit run_in_background) or at its timeout (a promotion), so a
// refusal means nothing started, or a command killed as before (P26). And at
// most maxJobWakes wakes in a row may carry a job's result with no turn a
// person started between them: the counter (wakeChain) counts each wake whose
// own reservation — its prompt — holds a job's result, and every turn a
// person starts (Run, which the engine admits: a submit, a queue's drain, a
// send-now) resets it, a wake never. Once it has reached the cap, a job result
// that becomes ready is suspended rather than pending: it wakes nothing,
// keeps no host alive (owed) and is delivered at the step 0 of the next turn
// a person starts, as a result a failed wake set aside is. So the cap needs
// nothing of the adapter's wake but what it already reads, HasPending.
//
// # Reads
//
// bash_output reads a running job's output since a cursor: where the model's
// own reads have taken it — from 0 — moved on by every read whose result an
// append writes. A read records how far it read, owned by its call; the
// append that writes the call's result commits it (commitCalls), and a turn
// that ends without writing it forgets it (restoreTurn), so the next read
// shows the same output again. A promotion's receipt is the first such read,
// owned by the bash call that returned it (promotionRead): the output so far
// counts as seen only once the receipt is written (review r7 finding 3).
// Reads of one job are serialized (readMu), so two reads in one step show
// consecutive output, the second from where the first stopped.
//
// # Texts
//
// Every text a job gives the model — its receipts, its JobOutput snapshots,
// its result block, bash_output's and bash_stop's answers — is redacted as it
// is made with the session's widest redaction (union, P19). The command's
// live stream, and so its output, its spill file and every read of it, is
// redacted with the session's key set as it grows (Jobs.Track; plan 033 C10r,
// superseding §3.7's "a running job's stream and spill file keep their
// call-time Replacer"): a key learned while the job runs is caught in it
// whole however the command splits it across its writes and the model's
// reads.

// maxJobs is how many jobs one session runs at once (P14).
const maxJobs = 8

// maxJobWakes is how many wakes in a row may deliver a job's result with no
// turn a person started between them (P14).
const maxJobWakes = 3

// The causes a job's context is cancelled with when it is stopped (the
// errStoppedByUser pattern, subagents.go): who stopped it, which its result
// block says (by=).
var (
	errJobStoppedByAgent = errors.New("harness: the model stopped this background job (bash_stop)")
	errJobStoppedByUser  = errors.New("harness: the user stopped this background job")
)

// resultKind is what a background result is the result of.
type resultKind int

const (
	kindSubagent resultKind = iota // a background sub-agent (plan 026 §3.11)
	kindJob                        // a background bash job (plan 033 §3.8)
)

// jobState is a job's own part of its result (bgResult.job). The handle, the
// body, the command and the times are fixed at Start; the rest is guarded by
// regMu, but readMu, which serializes reads.
type jobState struct {
	h     *jobHandle
	body  tool.JobBody
	cmd   string // the command, redacted when the job started
	began time.Time

	// read is the committed read cursor: the bytes of output the model has
	// been shown for good. reads are the reads made since, each owned by the
	// bash_output call that made it, until an append commits it or its turn
	// ends without one.
	read  int64
	reads []jobRead
	// attrs are the result block's attributes, once the job has ended.
	attrs jobAttrs

	readMu sync.Mutex
}

// jobRead is one bash_output read of a running job: the call that made it
// (own) and the cursor it read to.
type jobRead struct {
	own owner
	to  int64
}

// jobAttrs are a job result block's attributes beside its id and status
// (§3.7): who stopped it, its exit code (-1 none) and how long it ran ("",
// none, for a result made without one).
type jobAttrs struct {
	by   string
	exit int
	dur  string
}

// jobHandle is a running job as a stop reaches it: its context's cancel, and
// the latch its end sets (the childHandle pattern, subagents.go).
type jobHandle struct {
	cancel context.CancelCauseFunc
	mu     sync.Mutex
	ended  bool
}

// stop cancels the job's context with cause unless its end is latched, and
// reports whether it did. The first cause stands.
func (h *jobHandle) stop(cause error) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.ended {
		return false
	}
	h.cancel(cause)
	return true
}

// end latches the job's end: its body has returned, and a stop from here on
// is refused. It bounds when a stop is taken; a stop taken before it claims
// only a job its body ended as stopped (runJob).
func (h *jobHandle) end() {
	h.mu.Lock()
	h.ended = true
	h.mu.Unlock()
}

// jobs is a session's tool.Jobs (Env.Jobs): the runner's registry seen from
// the bash tools. A session has one only when it runs background work at all
// (Options.Background) and is not a sub-agent (P11, D-59).
type jobs struct{ r *subagents }

var _ tool.Jobs = jobs{}

// Reserve is tool.Jobs': a slot taken under regMu, while the registry is
// unsealed and fewer than maxJobs jobs hold one, its goroutine counted
// (workers) before anything starts — so Close's join, which follows its seal,
// waits for whatever the slot's holder goes on to start, and never meets an
// Add.
func (j jobs) Reserve(id string) (tool.JobSlot, error) {
	r := j.r
	r.regMu.Lock()
	defer r.regMu.Unlock()
	switch {
	case r.sealed:
		return nil, fmt.Errorf("harness: %w", tool.ErrClosing)
	case r.jobsHeld >= maxJobs:
		return nil, tool.JobsFull{Max: maxJobs}
	case r.results[id] != nil:
		return nil, fmt.Errorf("harness: a background job %q exists already", id) // cannot happen: call ids are unique
	}
	r.jobsHeld++
	r.workers.Add(1)
	return &jobSlot{r: r, id: id}, nil
}

// Redact is tool.Jobs': the session's widest redaction (P19).
func (j jobs) Redact(text string) string { return j.r.union(nil).String(text) }

// Track is tool.Jobs': the stream kept widened to the session's key set while
// it runs (toolset.track, plan 033 C10r).
func (j jobs) Track(s tool.KeyedStream) (untrack func()) { return j.r.s.tools.track(s) }

// jobSlot is one reserved slot (tool.JobSlot): Start or Release, once.
type jobSlot struct {
	r    *subagents
	id   string
	used atomic.Bool
}

// Release is tool.JobSlot's: the slot and its goroutine's count given back.
func (s *jobSlot) Release() {
	if !s.used.CompareAndSwap(false, true) {
		return
	}
	s.r.regMu.Lock()
	s.r.jobsHeld--
	s.r.regMu.Unlock()
	s.r.workers.Done()
}

// Start is tool.JobSlot's (the hand-over, §3.8): the job registered as
// running, its goroutine started — counted at the reservation, and gated so
// that its every event follows its JobStarted — and JobStarted reported
// through the session's sink. Nothing here can refuse: a session that began
// to close meanwhile has closed its tools' closing channel, or will, and the
// job's command is killed at once.
func (s *jobSlot) Start(spec tool.JobSpec, body tool.JobBody) {
	if !s.used.CompareAndSwap(false, true) {
		return
	}
	r := s.r
	ctx, cancel := context.WithCancelCause(r.bgCtx)
	red := r.union(nil)
	cmd := red.String(spec.Command)
	res := &bgResult{kind: kindJob, id: s.id, typ: tool.JobType, desc: commandLine(cmd), callID: s.id,
		state: resultRunning, done: make(chan struct{}),
		job: &jobState{h: &jobHandle{cancel: cancel}, body: body, cmd: cmd, began: spec.Began,
			reads: promotionRead(r.turn.Load(), s.id, spec.Seen), attrs: jobAttrs{exit: -1}}}
	r.regMu.Lock()
	r.results[s.id] = res
	r.order = append(r.order, s.id)
	r.regMu.Unlock()
	gate := make(chan struct{})
	go r.runJob(res, ctx, spec.Limit, gate)
	// The gate opens however this returns, a panicking sink included, so the
	// goroutine counted at the reservation always runs to its end.
	defer close(gate)
	r.emit(JobStarted{ID: s.id, CallID: s.id, Command: cmd, Workdir: red.String(spec.Workdir), Limit: spec.Limit,
		Promoted: spec.Promoted, At: r.s.now(), Began: spec.Began})
}

// promotionRead is where a job's reads start (plan 033 C10r, review r7
// finding 3): a promoted command's receipt shows the output so far — seen
// bytes of it — and that is a read like any bash_output's, owned by the bash
// call that returned the receipt, in the turn link names. The append that
// writes the call's result commits it (commitCalls: the bash call is among a
// tool entry's output calls), and a turn that ends without one forgets it
// (restoreTurn), so a receipt the transcript never got leaves the cursor at 0
// and the next bash_output shows that output again. A started job's receipt
// shows none: nothing to read. With no turn — a bash call runs only in one —
// there is no append to wait for either, and nothing is taken as read.
func promotionRead(link *turnLink, id string, seen int64) []jobRead {
	if seen <= 0 || link == nil {
		return nil
	}
	return []jobRead{{own: owner{turn: link.number, step: stepOfCall(id), call: id, wake: link.wake}, to: seen}}
}

// commandLine is a job's command as a compaction's running-tasks list names
// it (desc): its first line, and at most maxCommandLine bytes of it.
func commandLine(cmd string) string {
	line, _, more := strings.Cut(cmd, "\n")
	if len(line) > maxCommandLine {
		line, more = cutRunes(line, maxCommandLine), true
	}
	if more {
		line += " …"
	}
	return line
}

// maxCommandLine bounds the command as a job's texts quote it: a
// running-tasks line (commandLine) and a result block's "$ <command>" line
// (jobText), which a heredoc of any size could otherwise fill.
const maxCommandLine = 2000

// cutRunes is s cut to at most n bytes, at a character's start.
func cutRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// runJob is a job's goroutine (§3.8's table): it runs the body to its end
// under the job's context, latches the end, decides the status from how the
// body ended and, for a stop, from the context's cause, reports JobFinished
// through the session's sink, publishes the result and, when it is pending,
// says so (OnPending): not for one the wake chain's cap suspended, nor for one
// the session's close ended, whose result nobody will read.
func (r *subagents) runJob(res *bgResult, ctx context.Context, limit time.Duration, gate <-chan struct{}) {
	defer r.workers.Done()
	j := res.job
	defer j.h.cancel(nil)
	<-gate
	// The body's progress goes out as JobOutput until the body has returned,
	// and never after: a snapshot being delivered as it returns is let finish
	// first (progressMu), so every JobOutput precedes the JobFinished. Each is
	// redacted as it goes with the session's widest redaction (union, P19),
	// as every text a job gives the model is: the stream redacts what the
	// command prints after the session learns a key, and this catches one
	// it printed before, which the snapshot — the output's tail, not a read
	// from a cursor — holds whole (plan 033 C10r, review r7 finding 9).
	var progressMu sync.Mutex
	ended := false
	progress := func(snapshot string) {
		snapshot = r.union(nil).String(snapshot) // before progressMu: union takes the toolset's and the registry's locks
		progressMu.Lock()
		defer progressMu.Unlock()
		if !ended {
			r.emit(JobOutput{ID: res.id, Output: snapshot})
		}
	}
	end := r.waitJob(ctx, res, limit, progress)
	progressMu.Lock()
	ended = true
	progressMu.Unlock()
	if r.seams.jobReturned != nil {
		r.seams.jobReturned(res.id)
	}
	j.h.end()
	ran := max(r.s.now().Sub(j.began), 0) // the session's clock, as JobStarted's At is
	if r.seams.jobEnded != nil {
		r.seams.jobEnded(res.id)
	}
	cause := context.Cause(ctx)
	closing := errors.Is(cause, errClosing) || isClosed(r.s.tools.closing)
	attrs := jobAttrs{exit: -1, dur: tool.JobDuration(ran)}
	var label string // JobFinished's Error: the row's word for an ending that was not a clean exit
	switch end.Status {
	case tool.JobExited:
		attrs.exit = end.ExitCode
		if end.ExitCode != 0 {
			label = "exit code " + strconv.Itoa(end.ExitCode)
		}
	case tool.JobStopped:
		switch {
		case errors.Is(cause, errJobStoppedByAgent):
			attrs.by, label = tool.JobStoppedByYou, "stopped by the agent"
		case errors.Is(cause, errJobStoppedByUser):
			attrs.by, label = tool.JobStoppedByUser, "stopped by the user"
		default:
			label = "stopped: the session closed"
		}
	case tool.JobTimedOut:
		label = "time limit reached"
	default:
		label = "failed"
	}
	red := r.union(nil)
	text := red.String(jobText(j.cmd, end.Text))
	var out string
	if end.Output != nil {
		out = red.String(end.Output.Output)
	}
	r.emit(JobFinished{ID: res.id, Status: end.Status, By: attrs.by, Error: label, ExitCode: attrs.exit, Output: out,
		Duration: ran, At: r.s.now()})
	if r.publishJob(res, end.Status, text, attrs, closing) {
		r.notifyPending(true)
	}
}

// waitJob is the body's Wait with a panic recovered as a failed job: a panic
// on a goroutine nobody recovers would end the process. progress is what the
// body's own throttle sends its snapshots to (runJob's JobOutput).
func (r *subagents) waitJob(ctx context.Context, res *bgResult, limit time.Duration, progress tool.Progress) (end tool.JobEnd) {
	defer func() {
		if v := recover(); v != nil {
			end = tool.JobEnd{Status: tool.JobFailed, ExitCode: -1, Text: fmt.Sprintf("The job failed: it panicked: %v", v)}
		}
	}()
	return res.job.body.Wait(ctx, limit, progress)
}

// jobText is a job's result as its block holds it (§3.7): "$ <command>", the
// command cut at maxCommandLine, then the body, less the newline the output's
// last line ends with, which the block's closing line supplies.
func jobText(cmd, body string) string {
	if len(cmd) > maxCommandLine {
		cmd = cutRunes(cmd, maxCommandLine) + " …"
	}
	return "$ " + cmd + "\n" + strings.TrimSuffix(body, "\n")
}

// publishJob makes a finished job's result deliverable and gives its slot
// back, in one regMu section: pending — or suspended, once the wake chain has
// reached its cap (P14), so that it wakes nothing and is delivered at the next
// turn a person starts. A job the session's close ended is reported at once:
// nothing delivers its result, and Close reports nothing for it. It reports
// whether the result is pending.
func (r *subagents) publishJob(res *bgResult, status, text string, attrs jobAttrs, closing bool) (pending bool) {
	r.regMu.Lock()
	defer r.regMu.Unlock()
	res.status, res.text = status, text
	res.job.attrs = attrs
	r.finished++
	res.seq = r.finished
	res.state = resultPending
	switch {
	case closing:
		res.reported = true
	case r.wakeChain >= maxJobWakes:
		res.state = resultSuspended
	}
	close(res.done)
	r.jobsHeld--
	return res.state == resultPending && !res.reported
}

// The texts bash_output and bash_stop answer with (§3.7), but the delivered
// result, which is deliver's (jobBlock).
const (
	// jobStillRunning heads a running job's read: its id and how long it has
	// run.
	jobStillRunning = "Job `%s` is still running (%s)."
	// jobNoNewOutput and jobNewOutput follow it.
	jobNoNewOutput = " No new output since your last read."
	jobNewOutput   = " Output since your last read:\n"
	// jobEarlier says how much output since the last read the answer leaves
	// out, and where it is (P13: up to the tail, plus the rest's place).
	jobEarlier      = "%d earlier bytes are in %s.\n"
	jobEarlierLost  = "%d earlier bytes could not be saved.\n"
	jobStopNotEnded = "Job `%s` was stopped but has not ended yet; its result is delivered to you when it does."
	jobsNoTurn      = "internal error: the job tool's call arrived while no turn was running."
)

// Output is tool.Jobs' bash_output (§3.7): by the state of the job call.ID
// names, never waiting on a reservation (P41, P47), its answer redacted last
// with a replacer built as it returns (union, P19):
//
//   - no job of this session by that id — a sub-agent's included — is
//     invalid_input, naming the jobs whose results are still to be delivered;
//   - committed: delivered already, and not repeated;
//   - reserved: already in this turn's input;
//   - pending or suspended: taken for this call, whose step commits it when
//     its append writes this call's result; the block is the answer, and the
//     call saw the job end (Observed);
//   - running: with call.Wait 0, a read (jobRead); otherwise the call waits —
//     holding no lock, and stopping nothing — for the job to end, for the
//     wait, its own cancel or the session's close, and then judges again: a
//     wait that ran out is a read the doom-loop guard counts as observed
//     (P13). In a step that also stops a job it does not wait, and says so
//     (stopAnnounced).
func (j jobs) Output(ctx context.Context, call tool.JobOutputCall) tool.Result {
	res := j.r.jobOutput(ctx, call)
	res.Text = j.r.union(nil).String(res.Text)
	return res
}

func (r *subagents) jobOutput(ctx context.Context, call tool.JobOutputCall) tool.Result {
	if ctx.Err() != nil {
		return abortedResult()
	}
	link := r.turn.Load()
	if link == nil {
		return tool.Result{Text: jobsNoTurn, IsError: true, Class: tool.ClassToolError}
	}
	own := owner{turn: link.number, step: stepOfCall(call.CallID), call: call.CallID, wake: link.wake}
	// A step that stops a job waits for nothing (stopAnnounced): its stop
	// must not queue behind this call in Fantasy's slots.
	skipped := call.Wait > 0 && r.stopping(own)
	if skipped {
		call.Wait = 0
	}
	closing := r.s.tools.closing
	var timeout <-chan time.Time
	waited := false
	for {
		res, answer, ok := r.jobAnswer(call.ID, own)
		if ok {
			return answer
		}
		if call.Wait <= 0 || waited {
			if read, ok := r.readJob(res, own, waited, skipped); ok {
				return read
			}
			continue // it ended as it was read: its result is the answer
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
		case <-res.done:
		case <-timeout:
			waited = true
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

// jobAnswer is the answer the job id's state gives, for own (Output's list),
// and ok; or, for a job still running, its result and ok false. One regMu
// section.
func (r *subagents) jobAnswer(id string, own owner) (res *bgResult, answer tool.Result, ok bool) {
	r.regMu.Lock()
	res = r.results[id]
	if res == nil || res.kind != kindJob {
		waiting := r.undeliveredLocked(kindJob)
		r.regMu.Unlock()
		return nil, unknownJob(r.s.quoteRaw(id), waiting), true
	}
	switch res.state {
	case resultCommitted:
		r.regMu.Unlock()
		return res, tool.Result{Text: outputDelivered}, true
	case resultReserved:
		r.regMu.Unlock()
		return res, tool.Result{Text: outputIncluded}, true
	case resultPending, resultSuspended:
		got := r.reserveLocked(res, own)
		r.regMu.Unlock()
		return res, tool.Result{Text: r.deliver([]taken{got}).text, Observed: true}, true
	}
	r.regMu.Unlock()
	return res, tool.Result{}, false
}

// readJob is a read of the running job res for own: the output after the
// cursor — the committed one, or the furthest this turn's reads have taken it
// — recorded as own's read, and the answer, observed when the call waited out
// its wait, and saying so when a stop in its step skipped the wait it asked
// for (skipped). ok is false, and nothing recorded, when the job has ended
// since it was found running: its result is then the answer. Reads of one job
// are serialized (readMu, then regMu inside it), and the output is read with
// neither of the registry's locks held but the job's own.
func (r *subagents) readJob(res *bgResult, own owner, waited, skipped bool) (tool.Result, bool) {
	j := res.job
	j.readMu.Lock()
	defer j.readMu.Unlock()
	r.regMu.Lock()
	from := j.read
	for _, rd := range j.reads {
		if rd.own.turn == own.turn {
			from = max(from, rd.to)
		}
	}
	r.regMu.Unlock()
	out := j.body.Output(from)
	r.regMu.Lock()
	running := res.state == resultRunning
	if running {
		j.reads = append(j.reads, jobRead{own: own, to: out.Total})
	}
	r.regMu.Unlock()
	if !running {
		return tool.Result{}, false
	}
	text := fmt.Sprintf(jobStillRunning, res.id, tool.JobDuration(r.s.now().Sub(j.began)))
	if skipped {
		text += stopStepNoWait
	}
	switch {
	case out.Total <= from:
		text += jobNoNewOutput
	default:
		text += jobNewOutput
		switch {
		case out.Skipped > 0 && out.Spill != "":
			text += fmt.Sprintf(jobEarlier, out.Skipped, out.Spill)
		case out.Skipped > 0:
			text += fmt.Sprintf(jobEarlierLost, out.Skipped)
		}
		text += out.Text
	}
	return tool.Result{Text: text, Observed: waited}, true
}

// Stop is tool.Jobs' bash_stop (§3.7): the job call.ID names stopped —
// SIGTERM, then SIGKILL 3 s later, its body's supervise's — with the model's
// cause, then a wait of at most tool.JobStopWait, holding no lock, for it to
// end, and the answer by its state, as Output's: the block, delivered once,
// or why there is none; a job still running after the wait — a process in an
// uninterruptible sleep — says so, and its result comes when it ends. A job
// that had ended already keeps how it ended. Redacted last, as Output's.
func (j jobs) Stop(ctx context.Context, call tool.JobStopCall) tool.Result {
	res := j.r.jobStop(ctx, call)
	res.Text = j.r.union(nil).String(res.Text)
	return res
}

func (r *subagents) jobStop(ctx context.Context, call tool.JobStopCall) tool.Result {
	if ctx.Err() != nil {
		return abortedResult()
	}
	link := r.turn.Load()
	if link == nil {
		return tool.Result{Text: jobsNoTurn, IsError: true, Class: tool.ClassToolError}
	}
	own := owner{turn: link.number, step: stepOfCall(call.CallID), call: call.CallID, wake: link.wake}
	r.regMu.Lock()
	res := r.results[call.ID]
	if res == nil || res.kind != kindJob {
		waiting := r.undeliveredLocked(kindJob)
		r.regMu.Unlock()
		return unknownJob(r.s.quoteRaw(call.ID), waiting)
	}
	// Only a job that runs has a handle to stop; one that has ended — or a
	// result a resumed session set aside for a job its last incarnation
	// started, which has none — is answered from its state.
	var h *jobHandle
	if res.state == resultRunning {
		h = res.job.h
	}
	r.regMu.Unlock()
	if h != nil {
		h.stop(errJobStoppedByAgent)
	}
	if r.seams.stopWaiting != nil {
		r.seams.stopWaiting(call.ID)
	}
	timer := time.NewTimer(tool.JobStopWait)
	defer timer.Stop()
	closing := r.s.tools.closing
	select {
	case <-res.done:
	case <-timer.C:
	case <-ctx.Done():
		return abortedResult()
	case <-closing:
		return abortedResult()
	}
	if ctx.Err() != nil || isClosed(closing) {
		return abortedResult()
	}
	if _, answer, ok := r.jobAnswer(call.ID, own); ok {
		return answer
	}
	return tool.Result{Text: fmt.Sprintf(jobStopNotEnded, res.id)}
}

// stepRef names one step of one turn: a call's, by its harness id
// ("t<turn>.<step>.<n>").
type stepRef struct{ turn, step int }

// stopStepNoWait follows the head of an agent_output or bash_output answer
// that would have waited, in a step that also stops a job (stopAnnounced).
const stopStepNoWait = " It was not waited for, because this step also stops a job (bash_stop)."

// stopAnnounced records that step of turn has announced a bash_stop (plan 033
// C10r, review r7 finding 5), which the turn does as the call is announced
// (toolCall): Fantasy announces every call of a step before it runs any
// (agent.go:1755-1782), so by the time one of the step's calls runs, the
// mark is in place for all of them.
//
// What it buys is that a stop never queues behind a wait. Fantasy runs a
// step's Parallel calls — agent_output, bash_output and bash_stop all are —
// in five slots, in the order the model placed them (agent.go:1671): five
// waits of up to 600 s placed before a bash_stop would hold every slot, and
// the stop — its 7 s bound not yet started — would wait for one of them to
// end. So in a step that stops a job, agent_output and bash_output answer at
// once, as their wait 0 does (stopping), and say so: the slots they take are
// free again at once, and the stop runs. The mark is the step's, whatever its
// stop turns out to do — a gate or the doom-loop guard may still refuse it —
// and a stop is never begun here, before its call is gated; a retried attempt
// of the step keeps it, which costs that attempt's waits a snapshot, never a
// stop. A nil runner (a sub-agent's, which has neither tool) records nothing.
func (r *subagents) stopAnnounced(turn, step int) {
	if r == nil {
		return
	}
	r.regMu.Lock()
	r.stopStep = stepRef{turn: turn, step: step}
	r.regMu.Unlock()
}

// stopping reports whether own's step announced a bash_stop (stopAnnounced):
// an output call of that step does not wait.
func (r *subagents) stopping(own owner) bool {
	r.regMu.Lock()
	defer r.regMu.Unlock()
	return own.step > 0 && r.stopStep == stepRef{turn: own.turn, step: own.step}
}

// unknownJob refuses a job tool's id that names no job of this session,
// quoted as unknownSubagent quotes one, and lists the ones it could have
// meant (§3.7).
func unknownJob(quoted string, waiting []string) tool.Result {
	text := "Unknown job id `" + cutRawValue(quoted) + "`."
	switch {
	case len(waiting) == 0:
		text += " No background job's result is still to be delivered."
	default:
		shown := waiting[:min(len(waiting), outputUnknownCap)]
		text += " The background jobs whose results are still to be delivered: `" + strings.Join(shown, "`, `") + "`"
		if more := len(waiting) - len(shown); more > 0 {
			text += fmt.Sprintf(", … and %d more", more)
		}
		text += "."
	}
	return tool.Result{Text: text, IsError: true, Class: tool.ClassInvalidInput}
}

// jobBlock is one job's result in its wrapper (§3.7):
//
//	<background_command id="<id>" status="<status>" [by="<who>"] [exit_code="<n>"] [duration="<d>"]>
//	$ <command>
//	<tail, truncation notice and the saved file>
//	</background_command>
//
// The attributes are escaped, each written only when it has a value — by for
// a stopped job, exit_code for one that exited, duration for one that ran —
// and a closing tag inside the text is written <\/background_command, so
// nothing the command printed can end its own wrapper and speak outside it.
func jobBlock(id, status string, a jobAttrs, text string) string {
	b := jobBlockOpen + attrEscaper.Replace(id) + `" status="` + attrEscaper.Replace(status) + `"`
	if a.by != "" {
		b += ` by="` + attrEscaper.Replace(a.by) + `"`
	}
	if status == tool.JobExited && a.exit >= 0 {
		b += ` exit_code="` + strconv.Itoa(a.exit) + `"`
	}
	if a.dur != "" {
		b += ` duration="` + attrEscaper.Replace(a.dur) + `"`
	}
	return b + ">\n" + strings.ReplaceAll(text, "</background_command", `<\/background_command`) + "\n" + jobBlockClose
}

// commitReadsLocked commits the reads of turn's calls among calls
// (commitCalls):
// each job's cursor moves to the furthest of them, and they are forgotten.
// regMu is held.
func (r *subagents) commitReadsLocked(turn int, calls []string) {
	for _, res := range r.results {
		if res.job == nil || len(res.job.reads) == 0 {
			continue
		}
		j := res.job
		kept := j.reads[:0]
		for _, rd := range j.reads {
			switch {
			case rd.own.turn == turn && slices.Contains(calls, rd.own.call):
				j.read = max(j.read, rd.to)
			default:
				kept = append(kept, rd)
			}
		}
		j.reads = kept
	}
}

// forgetReadsLocked forgets every read turn made that no append committed
// (restoreTurn): the next read shows that output again. regMu is held.
func (r *subagents) forgetReadsLocked(turn int) {
	for _, res := range r.results {
		if res.job == nil || len(res.job.reads) == 0 {
			continue
		}
		kept := res.job.reads[:0]
		for _, rd := range res.job.reads {
			if rd.own.turn != turn {
				kept = append(kept, rd)
			}
		}
		res.job.reads = kept
	}
}

// resetWakeChain is a turn a person started (Run; P14): the wake chain starts
// again. A nil runner has none.
func (r *subagents) resetWakeChain() {
	if r == nil {
		return
	}
	r.regMu.Lock()
	r.wakeChain = 0
	r.regMu.Unlock()
}

// pendingKinds reports which kinds of result are waiting to be delivered, as
// hasPending counts them.
func (r *subagents) pendingKinds() (subagents, jobs bool) {
	if r == nil {
		return false, false
	}
	r.regMu.Lock()
	defer r.regMu.Unlock()
	for _, res := range r.results {
		if res.state == resultPending && !res.reported {
			switch res.kind {
			case kindJob:
				jobs = true
			default:
				subagents = true
			}
		}
	}
	return subagents, jobs
}

// PendingKinds reports which kinds of background result HasPending's are:
// a sub-agent's, a job's, or both — for the adapter's wake, whose reason says
// which (plan 033 §3.8: job_wake when only jobs' are). It takes one leaf lock
// and never waits, as HasPending does, and like it answers none in the refusal
// state (ErrStoredKeyFrozen).
func (s *Session) PendingKinds() (subagents, jobs bool) {
	if s.tools.refusing.Load() {
		return false, false
	}
	return s.subs.pendingKinds()
}
