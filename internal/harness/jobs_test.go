package harness

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/charliek/craze/internal/harness/tool"
	"github.com/charliek/craze/internal/harness/tool/opencode"
)

// Background jobs (plan 033 §3.7–§3.8, §7 A11–A14). Most tests here drive a
// job whose body is the test's own (fakeBody): it ends exactly when the test
// says, so a job's end lands where a test needs it — at a step's boundary, in
// a bash_output wait, during Close — and the session's machinery around it,
// the registry, the delivery, the wake chain, the cursor and the stop, is the
// real one; the bash side of the hand-over has its own tests
// (tool/opencode/bash_job_test.go). Every schedule is forced with a seam or a
// held step; nothing sleeps to win a race. The lifetime tests at the end run
// real commands through the real bash tool, end to end.

// fakeBody is a job's body the test drives (tool.JobBody). Its Wait ends when
// the test sends an end, or — as a real body's does — stopped, when the job's
// context is cancelled or the session's closing channel closes; unless it is
// deaf, when only the test's end ends it: a command whose leader exits by
// itself just as the session closes, which the body reports as an exit.
type fakeBody struct {
	closing <-chan struct{}
	deaf    bool
	end     chan tool.JobEnd
	waiting chan struct{} // closed as Wait begins

	mu    sync.Mutex
	out   string
	limit time.Duration
	prog  tool.Progress // the progress Wait was handed
}

func (b *fakeBody) write(text string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.out += text
}

func (b *fakeBody) output() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.out
}

func (b *fakeBody) Output(from int64) tool.JobOutput {
	b.mu.Lock()
	defer b.mu.Unlock()
	total := int64(len(b.out))
	from = min(max(from, 0), total)
	return tool.JobOutput{Text: b.out[from:], Total: total}
}

func (b *fakeBody) Wait(ctx context.Context, limit time.Duration, progress tool.Progress) tool.JobEnd {
	b.mu.Lock()
	b.limit, b.prog = limit, progress
	b.mu.Unlock()
	close(b.waiting)
	done, closing := ctx.Done(), b.closing
	if b.deaf {
		done, closing = nil, nil
	}
	select {
	case e := <-b.end:
		return e
	case <-done:
	case <-closing:
	}
	return tool.JobEnd{Status: tool.JobStopped, ExitCode: -1, Text: b.output()}
}

// jobBegan is when every fake job's command started: 4m12s before the
// session's fixed clock (testNow), so a job's duration is §3.7's example.
var jobBegan = testNow().Add(-(4*time.Minute + 12*time.Second))

// fakeJob starts a job of the session's with a fake body, through its jobs —
// a slot reserved, then handed over, as the bash tool does — and waits for
// its Wait to begin.
func fakeJob(t *testing.T, s *Session, id, command string) *fakeBody {
	t.Helper()
	return fakeJobWith(t, s, id, command, false)
}

// fakeJobWith is fakeJob, its body deaf to every stop when deaf is set.
func fakeJobWith(t *testing.T, s *Session, id, command string, deaf bool) *fakeBody {
	t.Helper()
	body := &fakeBody{closing: s.tools.closing, deaf: deaf, end: make(chan tool.JobEnd, 1), waiting: make(chan struct{})}
	slot, err := jobs{r: s.subs}.Reserve(id)
	if err != nil {
		t.Fatalf("Reserve(%s): %v", id, err)
	}
	slot.Start(tool.JobSpec{ID: id, Command: command, Workdir: "/work", Limit: time.Hour, Began: jobBegan}, body)
	await(t, body.waiting, "the job's body running")
	return body
}

// endJob ends body with end and waits for the session to say its result is
// pending (OnPending).
func (b *bg) endJob(t *testing.T, body *fakeBody, end tool.JobEnd) {
	t.Helper()
	body.end <- end
	if !await(t, b.pending, "the job's result waiting to be delivered") {
		t.Fatal("OnPending's handler found nothing pending")
	}
}

// exited is a job's end: exited with code, its output text.
func exited(code int, text string) tool.JobEnd {
	return tool.JobEnd{Status: tool.JobExited, ExitCode: code, Text: text,
		Output: &tool.ExecOutput{ExitCode: code, Output: text}}
}

// openJobs is a background session whose first turn, "go", has run, so every
// later turn — a wake's included — is routed under it.
func openJobs(t *testing.T, mutate ...func(*Options)) *bg {
	t.Helper()
	b := openBG(t, mutate...)
	b.routers["test/a"].route("go", answerWith("ready"))
	if res, err := b.s.Run(context.Background(), "go", nil); err != nil || res.StopReason != StopEndTurn {
		t.Fatalf("the first turn = %+v, %v", res, err)
	}
	return b
}

// jobBlockOf is §3.7's delivered result, spelled out: the attributes given,
// in order, then "$ <command>" and the body.
func jobBlockOf(id, attrs, command, body string) string {
	return `<background_command id="` + id + `" ` + attrs + ">\n$ " + command + "\n" + body + "\n</background_command>"
}

// bashOutput is one bash_output call for the job id, waiting up to waitMs.
func bashOutput(t *testing.T, callID, job string, waitMs int) []fantasy.StreamPart {
	t.Helper()
	in := map[string]any{"id": job}
	if waitMs > 0 {
		in["wait_ms"] = waitMs
	}
	return callParts(callID, "bash_output", input(t, in))
}

// bashStop is one bash_stop call for the job id.
func bashStop(t *testing.T, callID, job string) []fantasy.StreamPart {
	t.Helper()
	return callParts(callID, "bash_stop", input(t, map[string]any{"id": job}))
}

// jobView is a copy of a job's own part, as a test reads it.
type jobView struct {
	read  int64
	reads []jobRead
	attrs jobAttrs
}

// jobResultOf is a copy of the job id's result and its job part.
func jobResultOf(t *testing.T, s *Session, id string) (bgResult, jobView) {
	t.Helper()
	s.subs.regMu.Lock()
	defer s.subs.regMu.Unlock()
	res := s.subs.results[id]
	if res == nil || res.job == nil {
		t.Fatalf("no background job %s", id)
	}
	j := jobView{read: res.job.read, reads: slices.Clone(res.job.reads), attrs: res.job.attrs}
	cp := *res
	cp.job = nil
	return cp, j
}

// blocksIn counts the job id's result blocks in the session's transcript, in
// any entry, a tool result included.
func blocksIn(t *testing.T, s *Session, id string) int {
	t.Helper()
	n := 0
	for _, e := range transcript(t, s).Entries {
		n += strings.Count(messageText(e.Message), `<background_command id="`+id+`"`)
	}
	return n
}

// TestJobResultDelivered (A11, A12, §3.7's delivered result): a job's result
// is §3.7's block — the id, the status, the exit code, the duration, then the
// command and the output — and it is delivered once, whichever way: at the
// step 0 of the next turn, by a wake, or at a step boundary of a running
// turn, committed by the entry that writes it. JobStarted and JobFinished go
// to the session's sink, before and after; nothing about a job is a steer.
func TestJobResultDelivered(t *testing.T) {
	want := jobBlockOf("t9.1.1", `status="exited" exit_code="1" duration="4m12s"`, "npm test", "FAIL x_test\nboom")

	t.Run("at the next turn's step 0", func(t *testing.T) {
		b := openJobs(t)
		a := b.routers["test/a"]
		body := fakeJob(t, b.s, "t9.1.1", "npm test")
		b.endJob(t, body, exited(1, "FAIL x_test\nboom\n"))
		a.route("go", answerWith("seen"))
		var ev events
		if res, err := b.s.Run(context.Background(), "next", ev.sink); err != nil || res.StopReason != StopEndTurn || len(res.Unanswered) != 0 {
			t.Fatalf("Run = %+v, %v", res, err)
		}
		reqs := a.requests("go")
		prompt := promptOf(reqs[len(reqs)-1])
		if prompt[len(prompt)-2] != "user: next" || prompt[len(prompt)-1] != "user: "+want {
			t.Fatalf("the turn's request: %v\nwant the prompt, then:\n%s", prompt, want)
		}
		tr := transcript(t, b.s)
		n := len(tr.Entries)
		if lines := entries(tr); lines[n-2] != "user test/a high: "+want || !tr.Entries[n-2].SubagentResults || tr.Entries[n-2].SubagentUsage != nil {
			t.Fatalf("transcript:\n%s", strings.Join(lines, "\n"))
		}
		if r, _ := jobResultOf(t, b.s, "t9.1.1"); r.state != resultCommitted || r.entry != tr.Entries[n-2].ID {
			t.Fatalf("the result is %v by %q; want committed by its entry", r.state, r.entry)
		}
		if len(of[Steered](ev.list())) != 0 {
			t.Fatal("a job's result was a steer")
		}
		own := b.own.list()
		st, fin := of[JobStarted](own), of[JobFinished](own)
		if len(st) != 1 || st[0] != (JobStarted{ID: "t9.1.1", CallID: "t9.1.1", Command: "npm test", Workdir: "/work", Limit: time.Hour, At: testNow(), Began: jobBegan}) {
			t.Fatalf("JobStarted = %+v", st)
		}
		if len(fin) != 1 || fin[0] != (JobFinished{ID: "t9.1.1", Status: tool.JobExited, Error: "exit code 1", ExitCode: 1,
			Output: "FAIL x_test\nboom\n", Duration: 4*time.Minute + 12*time.Second, At: testNow()}) {
			t.Fatalf("JobFinished = %+v", fin)
		}
		if i, j := slices.IndexFunc(own, func(e Event) bool { _, ok := e.(JobStarted); return ok }),
			slices.IndexFunc(own, func(e Event) bool { _, ok := e.(JobFinished); return ok }); i > j {
			t.Fatal("JobFinished came before JobStarted")
		}
		if b.s.HasPending() || b.s.BackgroundOwed() {
			t.Fatal("a delivered job is still pending or owed")
		}
	})

	t.Run("by a wake", func(t *testing.T) {
		b := openJobs(t)
		a := b.routers["test/a"]
		body := fakeJob(t, b.s, "t9.1.1", "npm test")
		if !b.s.BackgroundOwed() || b.s.HasPending() {
			t.Fatal("a running job is not owed, or is pending already")
		}
		b.endJob(t, body, exited(1, "FAIL x_test\nboom\n"))
		if subs, jobs := b.s.PendingKinds(); subs || !jobs {
			t.Fatalf("PendingKinds = %v, %v; want a job's alone", subs, jobs)
		}
		a.route("go", answerWith("reacting"))
		if res, err := b.s.Wake(context.Background(), nil); err != nil || res.StopReason != StopEndTurn {
			t.Fatalf("Wake = %+v, %v", res, err)
		}
		if got := lastUser(t, a.requests("go")[1]); got != want {
			t.Fatalf("the wake's prompt = %q\nwant %q", got, want)
		}
		if blocksIn(t, b.s, "t9.1.1") != 1 {
			t.Fatal("the result is not in the transcript once")
		}
		if _, err := b.s.Wake(context.Background(), nil); !errors.Is(err, ErrNothingPending) {
			t.Fatalf("a second Wake = %v; want nothing pending", err)
		}
	})

	t.Run("at a step boundary", func(t *testing.T) {
		b := openJobs(t)
		a := b.routers["test/a"]
		body := fakeJob(t, b.s, "t9.1.1", "npm test")
		atStep2, ready := make(chan struct{}), make(chan struct{})
		a.route("go",
			callStep(globPart("g1")),
			func(ctx context.Context, yield func(fantasy.StreamPart) bool) {
				close(atStep2)
				<-ready
				callStep(globPart("g2"))(ctx, yield)
			},
			answerWith("thanks"))
		out := start(context.Background(), b.s, "next", nil)
		await(t, atStep2, "the turn's second step")
		b.endJob(t, body, exited(1, "FAIL x_test\nboom\n"))
		close(ready)
		if got := await(t, out, "the turn"); got.err != nil || got.res.StopReason != StopEndTurn {
			t.Fatalf("Run = %+v, %v", got.res, got.err)
		}
		reqs := a.requests("go")
		if got := lastUser(t, reqs[len(reqs)-1]); got != want {
			t.Fatalf("the third step's request ends %q; want the result", got)
		}
		if blocksIn(t, b.s, "t9.1.1") != 1 {
			t.Fatal("the result is not in the transcript once")
		}
	})
}

// TestJobFinishesDuringAStepBoundary (§5 C9: "during a step-boundary
// reservation"): a job's result that is published while a step's boundary
// takes up what is waiting is either before the boundary's regMu section —
// and that step carries it — or after it, and the next step does; never
// neither, never both. Each order is forced with the reservation's seams.
func TestJobFinishesDuringAStepBoundary(t *testing.T) {
	want := jobBlockOf("t9.1.1", `status="exited" exit_code="0" duration="4m12s"`, "make", "ok")
	for _, before := range []bool{true, false} {
		t.Run(fmt.Sprintf("published before the section %v", before), func(t *testing.T) {
			b := openJobs(t)
			a := b.routers["test/a"]
			body := fakeJob(t, b.s, "t9.1.1", "make")
			var once sync.Once
			publish := func(own owner) {
				if own.turn == 2 && own.step == 2 {
					once.Do(func() { b.endJob(t, body, exited(0, "ok\n")) })
				}
			}
			if before {
				b.s.subs.seams.reserving = publish
			} else {
				b.s.subs.seams.reserved = publish
			}
			a.route("go", callStep(globPart("g1")), callStep(globPart("g2")), answerWith("thanks"))
			if res, err := b.s.Run(context.Background(), "next", nil); err != nil || res.StopReason != StopEndTurn {
				t.Fatalf("Run = %+v, %v", res, err)
			}
			reqs := a.requests("go")
			carried := func(i int) bool { return strings.Contains(requestText(reqs[i], false), "<background_command") }
			// requests: the first turn's, then this turn's three.
			step2, step3 := carried(2), carried(3)
			if step2 != before || !step3 || lastUser(t, reqs[2+boolInt(!before)]) != want {
				t.Fatalf("carried at step 2: %v, at step 3: %v; want it from step %d on", step2, step3, 2+boolInt(!before))
			}
			if blocksIn(t, b.s, "t9.1.1") != 1 {
				t.Fatal("the result is not in the transcript once")
			}
		})
	}
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// TestBashOutputReads (A12, P13): bash_output on a running job returns the
// output since the last read whose result an append wrote — the first from
// the job's start — "No new output" when there is none, and once the job has
// ended its result block, delivered once; after that, "already delivered".
// A snapshot is not observed; seeing the end is. A sub-agent's id is an
// unknown job, and a job's id an unknown sub-agent (the kinds' filters).
func TestBashOutputReads(t *testing.T) {
	b := openJobs(t)
	a := b.routers["test/a"]
	body := fakeJob(t, b.s, "t9.1.1", "npm run dev")
	body.write("listening on :3000\n")
	// The reads alternate two spellings of a snapshot — wait_ms absent, and
	// 0 — so the doom-loop guard, which three identical snapshots in a row
	// meet (TestJobObservedExemptsWaits), sees no repeat here.
	read := func(i int) []fantasy.StreamPart {
		in := map[string]any{"id": "t9.1.1"}
		if i%2 == 1 {
			in["wait_ms"] = 0
		}
		return callParts("o", "bash_output", input(t, in))
	}
	write := func(text string) step {
		return func(ctx context.Context, yield func(fantasy.StreamPart) bool) {
			body.write(text)
			callStep(read(1))(ctx, yield)
		}
	}
	a.route("go",
		callStep(read(0)),
		write("GET /\n"),
		callStep(read(2)),
		func(ctx context.Context, yield func(fantasy.StreamPart) bool) {
			b.endJob(t, body, exited(0, "listening on :3000\nGET /\nbye\n"))
			callStep(read(3))(ctx, yield)
		},
		callStep(read(4)),
		callStep(bashOutput(t, "o", "nope", 0), callParts("ao", "agent_output", input(t, map[string]any{"id": "t9.1.1", "wait_ms": 0}))),
		answerWith("done"))
	var ev events
	if res, err := b.s.Run(context.Background(), "next", ev.sink); err != nil || res.StopReason != StopEndTurn {
		t.Fatalf("Run = %+v, %v", res, err)
	}
	fins := of[ToolFinished](ev.list())
	running := "Job `t9.1.1` is still running (4m12s)."
	want := []struct {
		text     string
		observed bool
	}{
		{running + " Output since your last read:\nlistening on :3000\n", false},
		{running + " Output since your last read:\nGET /\n", false},
		{running + " No new output since your last read.", false},
		{jobBlockOf("t9.1.1", `status="exited" exit_code="0" duration="4m12s"`, "npm run dev", "listening on :3000\nGET /\nbye"), true},
		{outputDelivered, false},
	}
	for i, w := range want {
		if r := fins[i].Result; r.Text != w.text || r.IsError || r.Observed != w.observed {
			t.Fatalf("read %d = {Text:%q IsError:%v Observed:%v}\nwant %q, observed %v", i+1, r.Text, r.IsError, r.Observed, w.text, w.observed)
		}
	}
	evs := ev.list()
	unknown := callResult(t, evs, "t2.6.1")
	if unknown.Class != tool.ClassInvalidInput || unknown.Text != "Unknown job id `nope`. No background job's result is still to be delivered." {
		t.Fatalf("an unknown id = %+v", unknown)
	}
	if ao := callResult(t, evs, "t2.6.2"); ao.Class != tool.ClassInvalidInput || !strings.HasPrefix(ao.Text, "Unknown sub-agent id `t9.1.1`.") {
		t.Fatalf("agent_output on a job's id = %+v; want an unknown sub-agent", ao)
	}
	if r, j := jobResultOf(t, b.s, "t9.1.1"); r.state != resultCommitted || j.read != int64(len("listening on :3000\nGET /\n")) || len(j.reads) != 0 {
		t.Fatalf("the job is %v, its cursor %d with %d reads open; want committed, the cursor past the two reads", r.state, j.read, len(j.reads))
	}
}

// TestJobUnknownIDsListed: an unknown id's refusal names the jobs whose
// results are still to be delivered, and never a sub-agent.
func TestJobUnknownIDsListed(t *testing.T) {
	b := openJobs(t)
	fakeJob(t, b.s, "t9.1.1", "a")
	fakeJob(t, b.s, "t9.1.2", "b")
	if got := unknownJob("x", []string{"t9.1.1", "t9.1.2"}); got.Text != "Unknown job id `x`. The background jobs whose results are still to be delivered: `t9.1.1`, `t9.1.2`." ||
		got.Class != tool.ClassInvalidInput || !got.IsError {
		t.Fatalf("unknownJob = %+v", got)
	}
	b.s.subs.regMu.Lock()
	jobsWaiting, subsWaiting := b.s.subs.undeliveredLocked(kindJob), b.s.subs.undeliveredLocked(kindSubagent)
	b.s.subs.regMu.Unlock()
	if !slices.Equal(jobsWaiting, []string{"t9.1.1", "t9.1.2"}) || len(subsWaiting) != 0 {
		t.Fatalf("undelivered: jobs %v, sub-agents %v", jobsWaiting, subsWaiting)
	}
}

// TestJobFinishesDuringAnOutputWait (§5 C9): a bash_output call waiting on a
// running job — found running, the wait begun (the outputWaiting seam) — is
// answered by the job's end with its result block, observed, and the step's
// tool entry commits it.
func TestJobFinishesDuringAnOutputWait(t *testing.T) {
	b := openJobs(t)
	a := b.routers["test/a"]
	body := fakeJob(t, b.s, "t9.1.1", "go test ./...")
	b.s.subs.seams.outputWaiting = func(id string) {
		if id == "t9.1.1" {
			b.endJob(t, body, exited(2, "FAIL\n"))
		}
	}
	a.route("go", callStep(bashOutput(t, "o", "t9.1.1", 600000)), answerWith("done"))
	var ev events
	if res, err := b.s.Run(context.Background(), "next", ev.sink); err != nil || res.StopReason != StopEndTurn {
		t.Fatalf("Run = %+v, %v", res, err)
	}
	r := of[ToolFinished](ev.list())[0].Result
	if want := jobBlockOf("t9.1.1", `status="exited" exit_code="2" duration="4m12s"`, "go test ./...", "FAIL"); r.Text != want || !r.Observed {
		t.Fatalf("the waiting call = %+v\nwant the block, observed:\n%s", r, want)
	}
	tr := transcript(t, b.s)
	toolEntry := tr.Entries[len(tr.Entries)-2]
	if res, _ := jobResultOf(t, b.s, "t9.1.1"); res.state != resultCommitted || res.entry != toolEntry.ID {
		t.Fatalf("the result is %v by %q; want committed by the tool entry %s", res.state, res.entry, toolEntry.ID)
	}
	if b.s.HasPending() {
		t.Fatal("the delivered result is still pending")
	}
}

// TestJobReadsAndResultsRestored (§3.8: "restored if the append fails or the
// turn is cancelled first"): a read whose step's append fails moves no
// cursor — the next read shows the same output — and a result a bash_output
// call took in a step whose append failed goes back to pending, said so,
// and is delivered again; so is one a cancelled turn took. The written reads
// of TestBashOutputReads are the control.
func TestJobReadsAndResultsRestored(t *testing.T) {
	t.Run("a failed append", func(t *testing.T) {
		w := &toggledWrites{}
		b := openJobs(t, func(o *Options) { o.storeOpenFile = w.open })
		a := b.routers["test/a"]
		body := fakeJob(t, b.s, "t9.1.1", "tail -f log")
		body.write("one\n")
		failing := func(parts ...[]fantasy.StreamPart) step {
			return func(ctx context.Context, yield func(fantasy.StreamPart) bool) {
				w.fail.Store(true) // the step's append, once its stream ends, fails
				callStep(parts...)(ctx, yield)
			}
		}
		a.route("go", failing(bashOutput(t, "o", "t9.1.1", 0)))
		if _, err := b.s.Run(context.Background(), "next", nil); err == nil {
			t.Fatal("the turn whose append failed did not fail")
		}
		w.fail.Store(false)
		if _, j := jobResultOf(t, b.s, "t9.1.1"); j.read != 0 || len(j.reads) != 0 {
			t.Fatalf("after the failed append the cursor is %d with %d reads open; want 0, none", j.read, len(j.reads))
		}
		var ev events
		a.route("go", callStep(bashOutput(t, "o", "t9.1.1", 0)), answerWith("ok"))
		if _, err := b.s.Run(context.Background(), "again", ev.sink); err != nil {
			t.Fatal(err)
		}
		if got := of[ToolFinished](ev.list())[0].Result.Text; got != "Job `t9.1.1` is still running (4m12s). Output since your last read:\none\n" {
			t.Fatalf("the read after the failed one = %q; want the same output again", got)
		}

		b.endJob(t, body, exited(0, "one\n"))
		// The next turn takes the result at its step 0; its append fails.
		a.route("go", failing(globPart("g1")))
		if _, err := b.s.Run(context.Background(), "third", nil); err == nil {
			t.Fatal("the turn whose append failed did not fail")
		}
		w.fail.Store(false)
		if !await(t, b.pending, "the result given back") {
			t.Fatal("OnPending found nothing pending after the restore")
		}
		if r, _ := jobResultOf(t, b.s, "t9.1.1"); r.state != resultPending {
			t.Fatalf("the result is %v; want pending again", r.state)
		}
		a.route("go", answerWith("ok"))
		if _, err := b.s.Wake(context.Background(), nil); err != nil {
			t.Fatal(err)
		}
		if blocksIn(t, b.s, "t9.1.1") != 1 {
			t.Fatal("the result is not in the transcript exactly once")
		}
	})
	t.Run("a cancelled turn", func(t *testing.T) {
		b := openJobs(t)
		a := b.routers["test/a"]
		body := fakeJob(t, b.s, "t9.1.1", "make")
		b.endJob(t, body, exited(0, "ok\n"))
		g := newGate()
		a.route("go", g.hold(nil, finish(fantasy.FinishReasonStop)))
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		out := start(ctx, b.s, "next", nil)
		await(t, g.reached, "the step carrying the result")
		if r, _ := jobResultOf(t, b.s, "t9.1.1"); r.state != resultReserved {
			t.Fatalf("mid-step the result is %v; want reserved", r.state)
		}
		cancel()
		if got := await(t, out, "the cancelled turn"); got.err != nil || got.res.StopReason != StopCancelled {
			t.Fatalf("Run = %+v, %v", got.res, got.err)
		}
		if !await(t, b.pending, "the result given back") {
			t.Fatal("OnPending found nothing pending")
		}
	})
}

// toggledWrites is a session file whose writes fail, writing nothing, while
// fail is set: the append in flight fails alone, and the store stays usable
// (the store's zero-byte rule).
type toggledWrites struct {
	f    *os.File
	fail atomic.Bool
}

func (w *toggledWrites) open(name string, flag int, perm os.FileMode) (io.WriteCloser, error) {
	f, err := os.OpenFile(name, flag, perm)
	if err != nil {
		return nil, err
	}
	w.f = f
	return w, nil
}

func (w *toggledWrites) Write(p []byte) (int, error) {
	if w.fail.Load() {
		return 0, errors.New("injected write failure")
	}
	return w.f.Write(p)
}

func (w *toggledWrites) Close() error { return w.f.Close() }

// TestJobDeliveredOnceAmongOutputStopAndWake (§5 C9: "output vs stop vs
// wake — one delivery wins, exactly once"). A finished job's result is taken
// by whichever consumer reserves it first, under regMu: a wake whose prompt
// it is leaves bash_output and bash_stop in its own step "already included";
// in a person's turn a bash_output and a bash_stop racing in one step deliver
// it once between them, the other answering "already included", and no wake
// finds anything after. Run with -race -count; nothing in it may differ but
// which of the two won.
func TestJobDeliveredOnceAmongOutputStopAndWake(t *testing.T) {
	t.Run("the wake first", func(t *testing.T) {
		b := openJobs(t)
		a := b.routers["test/a"]
		body := fakeJob(t, b.s, "t9.1.1", "make")
		b.endJob(t, body, exited(0, "ok\n"))
		a.route("go", callStep(bashOutput(t, "o", "t9.1.1", 0), bashStop(t, "s", "t9.1.1")), answerWith("ok"))
		var ev events
		if _, err := b.s.Wake(context.Background(), ev.sink); err != nil {
			t.Fatal(err)
		}
		for _, f := range of[ToolFinished](ev.list()) {
			if f.Result.Text != outputIncluded {
				t.Fatalf("%s = %q; want already included", f.ID, f.Result.Text)
			}
		}
		if blocksIn(t, b.s, "t9.1.1") != 1 {
			t.Fatal("not delivered exactly once")
		}
	})
	t.Run("a read and a stop in one step", func(t *testing.T) {
		b := openJobs(t)
		a := b.routers["test/a"]
		body := fakeJob(t, b.s, "t9.1.1", "make")
		atStep, ready := make(chan struct{}), make(chan struct{})
		a.route("go", func(ctx context.Context, yield func(fantasy.StreamPart) bool) {
			close(atStep)
			<-ready
			callStep(bashOutput(t, "o", "t9.1.1", 0), bashStop(t, "s", "t9.1.1"))(ctx, yield)
		}, answerWith("ok"))
		var ev events
		out := start(context.Background(), b.s, "next", ev.sink)
		await(t, atStep, "the step after its boundary")
		b.endJob(t, body, exited(0, "ok\n"))
		close(ready)
		if got := await(t, out, "the turn"); got.err != nil {
			t.Fatal(got.err)
		}
		block := jobBlockOf("t9.1.1", `status="exited" exit_code="0" duration="4m12s"`, "make", "ok")
		var texts []string
		for _, f := range of[ToolFinished](ev.list()) {
			texts = append(texts, f.Result.Text)
		}
		slices.Sort(texts)
		if want := []string{block, outputIncluded}; !slices.Equal(texts, want) {
			t.Fatalf("the two calls answered %q; want one block and one already included", texts)
		}
		if blocksIn(t, b.s, "t9.1.1") != 1 {
			t.Fatal("not delivered exactly once")
		}
		if _, err := b.s.Wake(context.Background(), nil); !errors.Is(err, ErrNothingPending) {
			t.Fatalf("Wake = %v; want nothing pending", err)
		}
	})
}

// TestJobDeliveredByAPartialStep (§5 C9: "a partial-step save"): a step a
// cancel cuts short after it announced its calls is synthesized with each
// call's recorded result (synthesizeStep): a bash_output call whose result was
// recorded — the block — commits it by that tool entry; one that ran and
// reserved a result its step never recorded gives it back. A Fantasy that
// returns without finishing a dispatched step is the defence's case (it never
// does under v0.43.2), so the course is scripted with the turn's callbacks.
func TestJobDeliveredByAPartialStep(t *testing.T) {
	b := openJobs(t)
	one, two := fakeJob(t, b.s, "t9.1.1", "make one"), fakeJob(t, b.s, "t9.1.2", "make two")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	in := func(id string) string { return input(t, map[string]any{"id": id}) }
	b.s.newAgent = func(_ fantasy.LanguageModel, _ string, tools []fantasy.AgentTool) fantasy.Agent {
		return fakeAgent{tools: tools, run: func(ctx context.Context, tools []fantasy.AgentTool, c fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
			_, _, _ = c.PrepareStep(ctx, fantasy.PrepareStepFunctionOptions{StepNumber: 0, Messages: []fantasy.Message{fantasy.NewUserMessage("next")}})
			b.endJob(t, one, exited(0, "one\n"))
			b.endJob(t, two, exited(0, "two\n"))
			_ = c.OnStepStart(0)
			_ = c.OnTextDelta("0", "checking")
			_ = c.OnToolCall(fantasy.ToolCallContent{ToolCallID: "c1", ToolName: "bash_output", Input: in("t9.1.1")})
			_ = c.OnToolCall(fantasy.ToolCallContent{ToolCallID: "c2", ToolName: "bash_output", Input: in("t9.1.2")})
			out := tools[slices.IndexFunc(tools, func(tl fantasy.AgentTool) bool { return tl.Info().Name == "bash_output" })]
			r1, _ := out.Run(ctx, fantasy.ToolCall{ID: "c1", Name: "bash_output", Input: in("t9.1.1")})
			_ = c.OnToolResult(fantasy.ToolResultContent{ToolCallID: "c1", ToolName: "bash_output",
				Result: fantasy.ToolResultOutputContentText{Text: r1.Content}, ClientMetadata: r1.Metadata})
			_, _ = out.Run(ctx, fantasy.ToolCall{ID: "c2", Name: "bash_output", Input: in("t9.1.2")})
			cancel()
			return nil, ctx.Err()
		}}
	}
	if res, err := b.s.Run(ctx, "next", nil); err != nil || res.StopReason != StopCancelled {
		t.Fatalf("Run = %+v, %v; want cancelled", res, err)
	}
	tr := transcript(t, b.s)
	last := tr.Entries[len(tr.Entries)-1]
	if r, _ := jobResultOf(t, b.s, "t9.1.1"); r.state != resultCommitted || r.entry != last.ID {
		t.Fatalf("the recorded call's result is %v by %q; want committed by the synthesized tool entry", r.state, r.entry)
	}
	if r, _ := jobResultOf(t, b.s, "t9.1.2"); r.state != resultPending {
		t.Fatalf("the unrecorded call's result is %v; want pending again", r.state)
	}
	if !await(t, b.pending, "the second result given back") {
		t.Fatal("OnPending found nothing pending")
	}
}

// TestJobFinishesDuringClose (§5 C9, §3.8: /exit, session.stop and the
// session's end kill through Close, nothing delivered): a job running when
// Close begins is killed — its body sees the closing channel — and one whose
// command exits by itself while Close joins the workers is published as Close
// is closing; either way its JobFinished is the record, its result is never
// pending, owed or delivered, nothing is said (OnPending), and Close reports
// no SubagentUndelivered for it. Close returns.
func TestJobFinishesDuringClose(t *testing.T) {
	t.Run("killed", func(t *testing.T) {
		b := openJobs(t)
		fakeJob(t, b.s, "t9.1.1", "npm run dev")
		if err := b.s.Close(); err != nil {
			t.Fatal(err)
		}
		fin := of[JobFinished](b.own.list())
		if len(fin) != 1 || fin[0].Status != tool.JobStopped || fin[0].By != "" || fin[0].Error != "stopped: the session closed" {
			t.Fatalf("JobFinished = %+v; want stopped by the close", fin)
		}
		b.noPending(t)
		closedQuietly(t, b, "t9.1.1")
	})
	t.Run("exiting as Close joins", func(t *testing.T) {
		b := openJobs(t)
		body := fakeJobWith(t, b.s, "t9.1.1", "make", true) // its leader exits by itself, whatever the close
		done := make(chan error, 1)
		go func() { done <- b.s.Close() }()
		waitFor(t, joining, "Close joining the workers")
		body.end <- exited(0, "ok\n")
		if err := await(t, done, "Close"); err != nil {
			t.Fatal(err)
		}
		if fin := of[JobFinished](b.own.list()); len(fin) != 1 || fin[0].Status != tool.JobExited {
			t.Fatalf("JobFinished = %+v; want the exit", fin)
		}
		b.noPending(t)
		closedQuietly(t, b, "t9.1.1")
	})
	t.Run("ended, its result waiting", func(t *testing.T) {
		b := openJobs(t)
		b.endJob(t, fakeJob(t, b.s, "t9.1.1", "make"), exited(0, "ok\n"))
		if err := b.s.Close(); err != nil {
			t.Fatal(err)
		}
		closedQuietly(t, b, "t9.1.1")
	})
}

// closedQuietly checks that after Close the job id's result is reported,
// never delivered, neither pending nor owed, and that Close reported nothing
// undelivered.
func closedQuietly(t *testing.T, b *bg, id string) {
	t.Helper()
	if r, _ := jobResultOf(t, b.s, id); !r.reported || r.state == resultCommitted {
		t.Fatalf("after Close the result is %v (reported %v); want reported, never delivered", r.state, r.reported)
	}
	if b.s.HasPending() || b.s.BackgroundOwed() {
		t.Fatal("a closed session's job is pending or owed")
	}
	if u := of[SubagentUndelivered](b.own.list()); len(u) != 0 {
		t.Fatalf("Close reported %+v undelivered; a job reports nothing", u)
	}
}

// TestCancelSubagentStopsAJob (§3.8's table: the stop key): the user's stop
// of a job's id stops it, by="the user", and races its natural end
// harmlessly — a stop that lands after the body returned and before its end
// is latched is taken and changes nothing, one after the latch is refused —
// each order forced with the job's seams. An id that names nothing, or a
// delivered job, keeps the stored refusal.
func TestCancelSubagentStopsAJob(t *testing.T) {
	t.Run("running", func(t *testing.T) {
		b := openJobs(t)
		fakeJob(t, b.s, "t9.1.1", "npm run dev")
		if err := b.s.CancelSubagent("t9.1.1"); err != nil {
			t.Fatalf("CancelSubagent = %v", err)
		}
		if !await(t, b.pending, "the stopped job's result") {
			t.Fatal("nothing pending")
		}
		r, j := jobResultOf(t, b.s, "t9.1.1")
		if r.status != tool.JobStopped || j.attrs.by != tool.JobStoppedByUser || !strings.HasPrefix(r.text, "$ npm run dev") {
			t.Fatalf("result %q %q by %q", r.status, r.text, j.attrs.by)
		}
		if got := jobBlock("t9.1.1", r.status, j.attrs, r.text); got != jobBlockOf("t9.1.1", `status="stopped" by="the user" duration="4m12s"`, "npm run dev", "") {
			t.Fatalf("block = %q", got)
		}
		if fin := of[JobFinished](b.own.list()); len(fin) != 1 || fin[0].Error != "stopped by the user" || fin[0].By != tool.JobStoppedByUser {
			t.Fatalf("JobFinished = %+v", fin)
		}
	})
	for _, seam := range []string{"returned", "ended"} {
		t.Run("against the natural end, "+seam, func(t *testing.T) {
			b := openJobs(t)
			body := fakeJob(t, b.s, "t9.1.1", "make")
			var err error
			stop := func(id string) { err = b.s.CancelSubagent(id) }
			if seam == "returned" {
				b.s.subs.seams.jobReturned = stop
			} else {
				b.s.subs.seams.jobEnded = stop
			}
			b.endJob(t, body, exited(0, "ok\n"))
			want := error(nil) // taken: the end is not latched yet
			if seam == "ended" {
				want = ErrNoSuchSubagent
			}
			if !errors.Is(err, want) {
				t.Fatalf("CancelSubagent = %v; want %v", err, want)
			}
			if r, j := jobResultOf(t, b.s, "t9.1.1"); r.status != tool.JobExited || j.attrs.by != "" {
				t.Fatalf("the job is %s by %q; want its exit to stand", r.status, j.attrs.by)
			}
		})
	}
	t.Run("nothing to stop", func(t *testing.T) {
		b := openJobs(t)
		body := fakeJob(t, b.s, "t9.1.1", "make")
		b.endJob(t, body, exited(0, "ok\n"))
		for _, id := range []string{"t9.1.1", "t9.9.9", ""} {
			if err := b.s.CancelSubagent(id); !errors.Is(err, ErrNoSuchSubagent) {
				t.Fatalf("CancelSubagent(%q) = %v; want ErrNoSuchSubagent", id, err)
			}
		}
	})
}

// TestJobCap (A11b, P14): at most maxJobs slots are held — reserved or
// running — and the next reservation is refused with §3.7's text, starting
// nothing; a job that ends gives its slot back, and so does a slot released
// unused. A refused reservation counts nothing.
func TestJobCap(t *testing.T) {
	b := openJobs(t)
	var bodies []*fakeBody
	for i := range maxJobs - 1 {
		bodies = append(bodies, fakeJob(t, b.s, fmt.Sprintf("t9.1.%d", i+1), "sleep"))
	}
	spare, err := jobs{r: b.s.subs}.Reserve("t9.2.1")
	if err != nil {
		t.Fatalf("the eighth slot was refused: %v", err)
	}
	// A reserved slot holds Close's join until it is given back: given back
	// on every way out of the test (Release after Release does nothing).
	defer spare.Release()
	ninth, err := jobs{r: b.s.subs}.Reserve("t9.2.2")
	if err == nil {
		ninth.Release() // so a failure here does not leave Close's join waiting on it
	}
	var full tool.JobsFull
	if !errors.As(err, &full) || err.Error() != "8 background jobs are already running; stop one with bash_stop first." {
		t.Fatalf("a ninth Reserve = %v; want the cap's refusal", err)
	}
	spare.Release()
	again, err := jobs{r: b.s.subs}.Reserve("t9.2.3")
	if err != nil {
		t.Fatalf("a slot given back was not free: %v", err)
	}
	again.Release()
	b.endJob(t, bodies[0], exited(0, ""))
	for i := range 2 {
		s, err := jobs{r: b.s.subs}.Reserve(fmt.Sprintf("t9.3.%d", i))
		if err != nil {
			t.Fatalf("after one job ended, slot %d was refused: %v", i, err)
		}
		defer s.Release()
	}
	b.s.subs.regMu.Lock()
	held := b.s.subs.jobsHeld
	b.s.subs.regMu.Unlock()
	if held != maxJobs {
		t.Fatalf("%d slots held; want %d", held, maxJobs)
	}
}

// TestJobWakeChainCapped (A13b, P14): three wakes in a row carry a job's
// result; the fourth job's result is suspended instead — it wakes nothing,
// is not pending and keeps no host alive (owed), so a detached host with
// nothing else running goes idle — and the next turn a person starts
// delivers it at its step 0 and starts the chain again: a fifth job's result
// is pending. A sub-agent's result still wakes as before (the chain is
// jobs').
func TestJobWakeChainCapped(t *testing.T) {
	b := openJobs(t)
	a := b.routers["test/a"]
	for i := 1; i <= 3; i++ {
		id := fmt.Sprintf("t9.1.%d", i)
		b.endJob(t, fakeJob(t, b.s, id, "job"), exited(0, ""))
		a.route("go", answerWith("ok"))
		if _, err := b.s.Wake(context.Background(), nil); err != nil {
			t.Fatalf("wake %d: %v", i, err)
		}
	}
	fourth := fakeJob(t, b.s, "t9.1.4", "job four")
	if !b.s.BackgroundOwed() {
		t.Fatal("control: a running job is not owed")
	}
	fourth.end <- exited(0, "four\n")
	waitFor(t, func() bool { r, _ := jobResultOf(t, b.s, "t9.1.4"); return r.state != resultRunning }, "the fourth job's end")
	if r, _ := jobResultOf(t, b.s, "t9.1.4"); r.state != resultSuspended {
		t.Fatalf("the fourth result is %v; want suspended", r.state)
	}
	b.noPending(t)
	if b.s.HasPending() || b.s.BackgroundOwed() {
		t.Fatal("a capped result is pending or owed: a detached host would stay up")
	}
	if _, err := b.s.Wake(context.Background(), nil); !errors.Is(err, ErrNothingPending) {
		t.Fatalf("a wake after the cap = %v; want nothing pending", err)
	}

	// The next turn a person starts delivers it at its step 0.
	a.route("go", answerWith("seen"))
	if _, err := b.s.Run(context.Background(), "back", nil); err != nil {
		t.Fatal(err)
	}
	reqs := a.requests("go")
	if got := lastUser(t, reqs[len(reqs)-1]); got != jobBlockOf("t9.1.4", `status="exited" exit_code="0" duration="4m12s"`, "job four", "four") {
		t.Fatalf("the person's turn's request ends %q; want the capped result", got)
	}
	// And the chain starts again.
	b.endJob(t, fakeJob(t, b.s, "t9.1.5", "job five"), exited(0, ""))
	if r, _ := jobResultOf(t, b.s, "t9.1.5"); r.state != resultPending {
		t.Fatalf("after a person's turn a job's result is %v; want pending", r.state)
	}
}

// TestJobWakeChainCountsOnlyJobs: a wake whose prompt holds no job's result —
// a sub-agent's alone — neither counts nor resets the chain; a person's turn
// resets it. Driven on the counter, beside the end-to-end test above.
func TestJobWakeChainCountsOnlyJobs(t *testing.T) {
	b := openJobs(t)
	a := b.routers["test/a"]
	ws, _ := b.spawn(t, "child one")
	chain := func() int {
		b.s.subs.regMu.Lock()
		defer b.s.subs.regMu.Unlock()
		return b.s.subs.wakeChain
	}
	b.endJob(t, fakeJob(t, b.s, "t9.1.1", "job"), exited(0, ""))
	a.route("go", answerWith("ok"))
	if _, err := b.s.Wake(context.Background(), nil); err != nil || chain() != 1 {
		t.Fatalf("after a job's wake: %v, chain %d; want 1", err, chain())
	}
	b.finish(t, ws[0])
	a.route("go", answerWith("ok"))
	if _, err := b.s.Wake(context.Background(), nil); err != nil || chain() != 1 {
		t.Fatalf("after a sub-agent's wake: %v, chain %d; want 1 still", err, chain())
	}
	a.route("go", answerWith("ok"))
	if _, err := b.s.Run(context.Background(), "hi", nil); err != nil || chain() != 0 {
		t.Fatalf("after a person's turn: %v, chain %d; want 0", err, chain())
	}
}

// TestJobObservedExemptsWaits (A14, P13): three sequential bash_output calls
// that each wait out their whole wait on a running job are not refused by the
// doom-loop guard — each is observed — while three identical snapshots are:
// the third is refused. Identical calls announced in one step are counted at
// announce time, before any of them is observed, so three of them in one step
// are refused as before.
func TestJobObservedExemptsWaits(t *testing.T) {
	b := openJobs(t)
	a := b.routers["test/a"]
	fakeJob(t, b.s, "t9.1.1", "make")
	wait := func() step { return callStep(bashOutput(t, "o", "t9.1.1", 1)) }
	snap := func() step { return callStep(bashOutput(t, "o", "t9.1.1", 0)) }
	a.route("go", wait(), wait(), wait(), snap(), snap(), snap(),
		callStep(bashOutput(t, "p1", "t9.1.1", 1), bashOutput(t, "p2", "t9.1.1", 1), bashOutput(t, "p3", "t9.1.1", 1)),
		answerWith("done"))
	var ev events
	if res, err := b.s.Run(context.Background(), "next", ev.sink); err != nil || res.StopReason != StopEndTurn {
		t.Fatalf("Run = %+v, %v", res, err)
	}
	fins := of[ToolFinished](ev.list())
	for i := range 3 {
		if r := fins[i].Result; r.IsError || !r.Observed {
			t.Fatalf("wait %d = %+v; want a read, observed", i+1, r)
		}
	}
	if r := fins[3].Result; r.IsError || r.Observed {
		t.Fatalf("the first snapshot = %+v; want a read, not observed", r)
	}
	if r := fins[4].Result; r.IsError {
		t.Fatalf("the second snapshot = %+v; want a read", r)
	}
	if r := fins[5].Result; r.Class != tool.ClassDoomLoop || r.Text != nudge("bash_output", 3) {
		t.Fatalf("the third snapshot = %+v; want the doom-loop refusal", r)
	}
	refused := 0
	for _, f := range fins[6:9] {
		if f.Result.Class == tool.ClassDoomLoop {
			refused++
		}
	}
	if refused != 1 {
		t.Fatalf("of three identical waits in one step, %d were refused; want the third alone, counted as announced", refused)
	}
}

// TestAgentOutputObserved (P13): agent_output gets the same flag — a wait
// that ran out on a running child, or a child that ended, is observed; a
// snapshot is not.
func TestAgentOutputObserved(t *testing.T) {
	b := openBG(t)
	ws, ids := b.spawn(t, "child one")
	link := &turnLink{number: 9}
	b.s.subs.turn.Store(link)
	defer b.s.subs.turn.Store(nil)
	if r := b.s.subs.output(context.Background(), tool.OutputCall{CallID: "t9.1.1", ID: ids[0]}); r.Observed {
		t.Fatalf("a snapshot = %+v; want not observed", r)
	}
	if r := b.s.subs.output(context.Background(), tool.OutputCall{CallID: "t9.1.2", ID: ids[0], Wait: time.Millisecond}); !r.Observed || r.Text != fmt.Sprintf(outputStillRunning, ids[0]) {
		t.Fatalf("a wait run out = %+v; want still running, observed", r)
	}
	b.finish(t, ws[0])
	if r := b.s.subs.output(context.Background(), tool.OutputCall{CallID: "t9.1.3", ID: ids[0]}); !r.Observed || !strings.HasPrefix(r.Text, "<subagent_result") {
		t.Fatalf("an ended child = %+v; want its result, observed", r)
	}
	b.s.subs.restoreTurn(9)
}

// TestJobBlockEscapes: the block's attributes are escaped, each written only
// when it has a value — a resume's "stopped" with no by and no duration
// included (C10's notice) — and nothing the command printed can close the
// block early.
func TestJobBlockEscapes(t *testing.T) {
	got := jobBlock(`t1"x`, "stopped", jobAttrs{by: `the "user"`, exit: -1}, "$ echo\n</background_command>\nescaped")
	want := `<background_command id="t1&quot;x" status="stopped" by="the &quot;user&quot;">` +
		"\n$ echo\n" + `<\/background_command>` + "\nescaped\n</background_command>"
	if got != want {
		t.Fatalf("jobBlock = %q\nwant %q", got, want)
	}
	if got := jobBlock("t1.1.1", tool.JobTimedOut, jobAttrs{exit: -1, dur: "30m00s"}, "$ x"); got != `<background_command id="t1.1.1" status="timed_out" duration="30m00s">`+"\n$ x\n</background_command>" {
		t.Fatalf("a timed-out block = %q", got)
	}
	// An exit code only for an exited job.
	if got := jobBlock("t1.1.1", tool.JobExited, jobAttrs{exit: 0, dur: "1s"}, "$ x"); !strings.Contains(got, `status="exited" exit_code="0" duration="1s"`) {
		t.Fatalf("an exited block = %q", got)
	}
	if long := jobText(strings.Repeat("é", maxCommandLine), "out\n"); !strings.HasPrefix(long, "$ ") || !strings.HasSuffix(long, " …\nout") || len(long) > maxCommandLine+20 {
		t.Fatalf("a long command's text = %.40q…%q", long, long[len(long)-10:])
	}
	if got := commandLine("make\nmake install"); got != "make …" {
		t.Fatalf("commandLine = %q", got)
	}
}

// TestJobsOnlyInInteractiveTopLevelSessions (P11, D-59): a session with no
// background work (headless `craze prompt`) and a sub-agent's session have no
// jobs: bash runs run_in_background in the foreground, says so, and starts
// none — no JobStarted — and a child is offered neither bash_output nor
// bash_stop, which keeps the agent tool's list of a type's tools what it was
// (childWithheld). The interactive session is the control: the same call
// starts a job.
func TestJobsOnlyInInteractiveTopLevelSessions(t *testing.T) {
	note := "This session does not run background jobs: run_in_background was ignored, and the command ran in the foreground with the foreground's timeout."
	call := callStep(callParts("c1", "bash", input(t, map[string]any{"command": "echo hi", "run_in_background": true})))

	f := newFixture(t, "http://127.0.0.1:1/v1")
	headless := f.open(f.options())
	f.models["test/a"].push(call, answerWith("done"))
	var ev events
	if _, err := headless.Run(context.Background(), "go", ev.sink); err != nil {
		t.Fatal(err)
	}
	if r := callResult(t, ev.list(), "t1.1.1"); r.Text != "hi\n\n\n<shell_metadata>\n"+note+"\n</shell_metadata>" {
		t.Fatalf("headless: %q; want the command run in the foreground, and the note", r.Text)
	}

	child := f.open(childOf(f, headless, ChildOptions{ID: "child-jobs", ParentCall: "t1.1.1", Type: "general-purpose", AllTools: true, Mode: "agent"}))
	if ids := specIDs(child); slices.Contains(ids, "bash_output") || slices.Contains(ids, "bash_stop") {
		t.Fatalf("a child is offered the job tools: %v", ids)
	}
	f.models["test/a"].push(call, answerWith("done"))
	var cev events
	if _, err := child.Run(context.Background(), "child task", cev.sink); err != nil {
		t.Fatal(err)
	}
	if r := callResult(t, cev.list(), "t1.1.1"); !strings.HasSuffix(r.Text, note+"\n</shell_metadata>") {
		t.Fatalf("a child: %q; want the command run in the foreground, and the note", r.Text)
	}

	b := openBG(t)
	if slices.Contains(b.s.tools.offered, "bash_output") || slices.Contains(b.s.tools.offered, "bash_stop") {
		t.Fatalf("the tools a child may be given include the job tools: %v", b.s.tools.offered)
	}
	b.routers["test/a"].route("go", call, answerWith("started"))
	var iev events
	if _, err := b.s.Run(context.Background(), "go", iev.sink); err != nil {
		t.Fatal(err)
	}
	if r := callResult(t, iev.list(), "t1.1.1"); !strings.HasPrefix(r.Text, "Started the command in the background as job `t1.1.1`.") {
		t.Fatalf("control: an interactive session's call = %q; want a job started", r.Text)
	}
	if !await(t, b.pending, "the job's result") {
		t.Fatal("nothing pending")
	}
	if len(of[JobStarted](b.own.list())) != 1 || len(of[JobStarted](ev.list())) != 0 || len(of[JobStarted](cev.list())) != 0 {
		t.Fatal("a job started where none may, or none where one should")
	}
}

// TestJobLifetimeEndToEnd (A11, A12, §3.8's table): real commands through the
// real bash tool and the session's jobs. run_in_background starts a job and
// returns §3.7's receipt, whose result a wake delivers when it exits; a
// command still running at its timeout is promoted, its receipt shown, its
// cursor past the output the receipt showed, and its result delivered when it
// exits; bash_stop stops one, by="you"; the stop key, by="the user"; its
// limit times one out; and Close kills one, delivering nothing. Each job's
// process is gone once it has ended. The session's clock is fixed earlier
// than the commands' own, so every duration reads 0s.
func TestJobLifetimeEndToEnd(t *testing.T) {
	inBG := func(command string, more ...any) []fantasy.StreamPart {
		in := map[string]any{"command": command, "run_in_background": true}
		for i := 0; i+1 < len(more); i += 2 {
			in[more[i].(string)] = more[i+1]
		}
		return callParts("c1", "bash", input(t, in))
	}
	spill := func(b *bg, id string) string { return b.home + "/" + tool.SpillDir + "/tool_" + id }
	receipt := func(b *bg, id, limit string) string {
		return "Started the command in the background as job `" + id + "`. It runs until it exits, until you stop it with bash_stop, " +
			"or for at most " + limit + "; the session closing stops it too. Its output is saved to: " + spill(b, id) + "\n" +
			"Its result is delivered to you when it finishes; do not poll it or sleep waiting for it. " +
			"Call bash_output with its id to read its output so far.\n" + tool.JobMarker(id)
	}
	wake := func(t *testing.T, b *bg) string {
		t.Helper()
		if !await(t, b.pending, "the job's result") {
			t.Fatal("nothing pending")
		}
		a := b.routers["test/a"]
		a.route("go", answerWith("ok"))
		if _, err := b.s.Wake(context.Background(), nil); err != nil {
			t.Fatal(err)
		}
		reqs := a.requests("go")
		return lastUser(t, reqs[len(reqs)-1])
	}

	t.Run("started, exits, delivered by a wake", func(t *testing.T) {
		b := openJobs(t)
		gate := makeFIFO(t, b.workspace, "gate")
		cmd := "echo started; read line < gate; echo got $line"
		b.routers["test/a"].route("go", callStep(inBG(cmd)), answerWith("waiting"))
		var ev events
		if _, err := b.s.Run(context.Background(), "next", ev.sink); err != nil {
			t.Fatal(err)
		}
		if r := callResult(t, ev.list(), "t2.1.1"); r.Text != receipt(b, "t2.1.1", "30 minutes") || r.IsError {
			t.Fatalf("the receipt = %q\nwant %q", r.Text, receipt(b, "t2.1.1", "30 minutes"))
		}
		st := of[JobStarted](b.own.list())
		if len(st) != 1 || st[0].ID != "t2.1.1" || st[0].Command != cmd || st[0].Workdir != b.workspace || st[0].Limit != 30*time.Minute || st[0].Promoted {
			t.Fatalf("JobStarted = %+v", st)
		}
		openFIFO(t, gate)
		if got, want := wake(t, b), jobBlockOf("t2.1.1", `status="exited" exit_code="0" duration="0s"`, cmd, "started\ngot x"); got != want {
			t.Fatalf("the wake's prompt = %q\nwant %q", got, want)
		}
	})

	t.Run("promoted at its timeout", func(t *testing.T) {
		b := openJobs(t)
		gate := makeFIFO(t, b.workspace, "gate")
		cmd := "echo before; read line < gate; echo after $line"
		b.routers["test/a"].route("go",
			callStep(callParts("c1", "bash", input(t, map[string]any{"command": cmd, "timeout": 300}))),
			callStep(bashOutput(t, "o1", "t2.1.1", 0)),
			answerWith("moved on"))
		var ev events
		if _, err := b.s.Run(context.Background(), "next", ev.sink); err != nil {
			t.Fatal(err)
		}
		evs := ev.list()
		r := callResult(t, evs, "t2.1.1")
		if id, ok := tool.ParseJobMarker(r.Text); r.IsError || !ok || id != "t2.1.1" || !strings.HasPrefix(r.Text, "before\n\n<shell_metadata>\nThe command did not finish within its timeout of 300 ms. It was not stopped: it was moved to the background as job `t2.1.1`") ||
			!strings.Contains(r.Text, "all of it is saved to: "+spill(b, "t2.1.1")+". ") {
			t.Fatalf("the promotion receipt = %q", r.Text)
		}
		if read := callResult(t, evs, "t2.2.1"); read.Text != "Job `t2.1.1` is still running (0s). No new output since your last read." {
			t.Fatalf("a read after the promotion = %q; want nothing new: the receipt showed the output so far", read.Text)
		}
		if st := of[JobStarted](b.own.list()); len(st) != 1 || !st[0].Promoted || st[0].Limit != 30*time.Minute {
			t.Fatalf("JobStarted = %+v; want promoted, 30 minutes", st)
		}
		openFIFO(t, gate)
		if got, want := wake(t, b), jobBlockOf("t2.1.1", `status="exited" exit_code="0" duration="0s"`, cmd, "before\nafter x"); got != want {
			t.Fatalf("the wake's prompt = %q\nwant %q", got, want)
		}
	})

	t.Run("bash_stop", func(t *testing.T) {
		b := openJobs(t)
		b.routers["test/a"].route("go", callStep(inBG("echo $$ > pid; exec sleep 651")),
			func(ctx context.Context, yield func(fantasy.StreamPart) bool) {
				readPID(t, b, "pid") // stopped once it runs as the sleep
				callStep(bashStop(t, "s1", "t2.1.1"))(ctx, yield)
			},
			answerWith("stopped"))
		var ev events
		if _, err := b.s.Run(context.Background(), "next", ev.sink); err != nil {
			t.Fatal(err)
		}
		r := callResult(t, ev.list(), "t2.2.1")
		if want := jobBlockOf("t2.1.1", `status="stopped" by="you" duration="0s"`, "echo $$ > pid; exec sleep 651", "(no output)"); r.Text != want || !r.Observed {
			t.Fatalf("bash_stop = %+v\nwant %q, observed", r, want)
		}
		goneProcess(t, b, "pid")
	})

	t.Run("the stop key", func(t *testing.T) {
		b := openJobs(t)
		b.routers["test/a"].route("go", callStep(inBG("echo $$ > pid; exec sleep 652")), answerWith("started"))
		if _, err := b.s.Run(context.Background(), "next", nil); err != nil {
			t.Fatal(err)
		}
		readPID(t, b, "pid")
		if err := b.s.CancelSubagent("t2.1.1"); err != nil {
			t.Fatal(err)
		}
		if got, want := wake(t, b), jobBlockOf("t2.1.1", `status="stopped" by="the user" duration="0s"`, "echo $$ > pid; exec sleep 652", "(no output)"); got != want {
			t.Fatalf("the wake's prompt = %q\nwant %q", got, want)
		}
		goneProcess(t, b, "pid")
	})

	t.Run("its limit", func(t *testing.T) {
		b := openJobs(t)
		b.routers["test/a"].route("go", callStep(inBG("echo $$ > pid; exec sleep 653", "timeout", 300)), answerWith("started"))
		var ev events
		if _, err := b.s.Run(context.Background(), "next", ev.sink); err != nil {
			t.Fatal(err)
		}
		if r := callResult(t, ev.list(), "t2.1.1"); r.Text != receipt(b, "t2.1.1", "300 ms") {
			t.Fatalf("the receipt = %q", r.Text)
		}
		if got, want := wake(t, b), jobBlockOf("t2.1.1", `status="timed_out" duration="0s"`, "echo $$ > pid; exec sleep 653", "(no output)"); got != want {
			t.Fatalf("the wake's prompt = %q\nwant %q", got, want)
		}
		goneProcess(t, b, "pid")
	})

	t.Run("Close kills it", func(t *testing.T) {
		b := openJobs(t)
		b.routers["test/a"].route("go", callStep(inBG("echo $$ > pid; exec sleep 654")), answerWith("started"))
		if _, err := b.s.Run(context.Background(), "next", nil); err != nil {
			t.Fatal(err)
		}
		readPID(t, b, "pid")
		if err := b.s.Close(); err != nil {
			t.Fatal(err)
		}
		if fin := of[JobFinished](b.own.list()); len(fin) != 1 || fin[0].Status != tool.JobStopped || fin[0].Error != "stopped: the session closed" {
			t.Fatalf("JobFinished = %+v", fin)
		}
		goneProcess(t, b, "pid")
		b.noPending(t)
		closedQuietly(t, b, "t2.1.1")
	})
}

// makeFIFO makes a named pipe in dir: a command that reads it waits until the
// test opens it to write (openFIFO).
func makeFIFO(t *testing.T, dir, name string) string {
	t.Helper()
	path := dir + "/" + name
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// openFIFO writes a line to the named pipe path, waiting for its reader.
func openFIFO(t *testing.T, path string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("x\n")
	_ = f.Close()
}

// readPID waits for the command to write its pid to name in the workspace.
func readPID(t *testing.T, b *bg, name string) int {
	t.Helper()
	var pid int
	waitFor(t, func() bool {
		raw, err := os.ReadFile(b.workspace + "/" + name)
		if err != nil || !strings.HasSuffix(string(raw), "\n") {
			return false
		}
		_, err = fmt.Sscan(string(raw), &pid)
		return err == nil
	}, "the command's pid")
	return pid
}

// goneProcess checks that the process whose pid the command wrote to name has
// been reaped: a job's leader is reaped before its end is reported.
func goneProcess(t *testing.T, b *bg, name string) {
	t.Helper()
	pid := readPID(t, b, name)
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("the job's process %d is still there (kill 0: %v)", pid, err)
	}
}

// TestJobOutputEvents: a running job's progress reaches the session's sink as
// JobOutput, between its JobStarted and its JobFinished, and never after: a
// snapshot the body sends once it has returned is dropped.
func TestJobOutputEvents(t *testing.T) {
	b := openJobs(t)
	body := fakeJob(t, b.s, "t9.1.1", "npm run dev")
	body.mu.Lock()
	progress := body.prog
	body.mu.Unlock()
	progress("ready on :3000")
	b.endJob(t, body, exited(0, "ready on :3000\n"))
	progress("late")
	var kinds []string
	for _, ev := range b.own.list() {
		switch e := ev.(type) {
		case JobStarted:
			kinds = append(kinds, "started")
		case JobOutput:
			kinds = append(kinds, "output "+e.ID+": "+e.Output)
		case JobFinished:
			kinds = append(kinds, "finished")
		}
	}
	if want := []string{"started", "output t9.1.1: ready on :3000", "finished"}; !slices.Equal(kinds, want) {
		t.Fatalf("the session's sink saw %q; want %q", kinds, want)
	}
}

// TestJobListedInTheCompactionState (plan 033 §3.8 "Compaction"): a running
// job is in a compaction's running-tasks list as `- <id> (bash job): <the
// command's first line>` — one of several lines marked cut — in the order the
// jobs started, and leaves it when it ends: the list reads "None." once
// nothing runs, the negative control at both ends.
func TestJobListedInTheCompactionState(t *testing.T) {
	b := openJobs(t)
	section := func() string { return b.s.stateSection(b.s.redactor()) }
	const none = "Running background tasks: None."
	if got := section(); !strings.HasSuffix(got, none) {
		t.Fatalf("control: with no job running the section ends %q", got)
	}
	one := fakeJob(t, b.s, "t9.1.1", "npm run dev")
	two := fakeJob(t, b.s, "t9.1.2", "cat > notes.txt <<'EOF'\nhello\nEOF")
	want := "Running background tasks:\n- t9.1.1 (bash job): npm run dev\n- t9.1.2 (bash job): cat > notes.txt <<'EOF' …"
	if got := section(); !strings.HasSuffix(got, want) {
		t.Fatalf("the section is\n%s\nwant it to end\n%s", got, want)
	}
	b.endJob(t, one, exited(0, ""))
	want = "Running background tasks:\n- t9.1.2 (bash job): cat > notes.txt <<'EOF' …"
	if got := section(); !strings.HasSuffix(got, want) {
		t.Fatalf("after the first job ended the section is\n%s\nwant it to end\n%s", got, want)
	}
	b.endJob(t, two, exited(0, ""))
	if got := section(); !strings.HasSuffix(got, none) {
		t.Fatalf("with every job ended the section ends %q", got)
	}
}

// promotingBash stands in for the bash tool: its every call is a command that
// reached its timeout with "before\n" printed and was promoted — a job whose
// body the test drives, handed over as opencode's promote hands one (Seen
// the bytes its receipt showed), and the receipt returned.
type promotingBash struct{ bodies chan *fakeBody }

func (promotingBash) Spec() tool.Spec {
	return tool.Spec{ID: tool.BashTool, Description: "Runs a command, which reaches its timeout and is promoted.",
		Parameters: map[string]any{"command": map[string]any{"type": "string"}}, Required: []string{"command"},
		Kind: tool.KindExecute, Truncate: tool.None}
}

func (p promotingBash) Prepare(_ tool.Env, c tool.Call) (tool.Prepared, error) {
	return promotingCall{bodies: p.bodies, id: c.ID}, nil
}

type promotingCall struct {
	bodies chan *fakeBody
	id     string
}

func (c promotingCall) Request() tool.Request { return tool.Request{Title: "promoted"} }

func (c promotingCall) Run(_ context.Context, env tool.Env) tool.Result {
	body := &fakeBody{closing: env.Closing, end: make(chan tool.JobEnd, 1), waiting: make(chan struct{}), out: "before\n"}
	slot, err := env.Jobs.Reserve(c.id)
	if err != nil {
		return tool.Result{Text: err.Error(), IsError: true, Class: tool.ClassToolError}
	}
	slot.Start(tool.JobSpec{ID: c.id, Command: "make", Workdir: env.Workspace, Limit: time.Hour, Promoted: true,
		Began: jobBegan, Seen: int64(len(body.out))}, body)
	c.bodies <- body
	return tool.Result{Text: "before\n\n<shell_metadata>\nMoved to the background.\n</shell_metadata>\n" + tool.JobMarker(c.id)}
}

// promotingProfile is opencode's profile with its bash replaced by
// promotingBash.
func promotingProfile(bodies chan *fakeBody) func() (*tool.Registry, error) {
	return func() (*tool.Registry, error) {
		p, err := opencode.Profile()
		if err != nil {
			return nil, err
		}
		for i, tl := range p.Tools {
			if tl.Spec().ID == tool.BashTool {
				p.Tools[i] = promotingBash{bodies: bodies}
			}
		}
		var reg tool.Registry
		return &reg, reg.Register(p)
	}
}

// TestPromotionReceiptOwnsItsRead (plan 033 C10r, review r7 finding 3): the
// output a promotion's receipt showed is a read the bash call made, as a
// bash_output's is. The append that writes the receipt commits it — the next
// bash_output shows only what came after — and one that fails gives it back:
// the receipt is not in the transcript, so the model was never shown that
// output for good, and the next bash_output shows it. Before the fix the
// cursor started past the receipt's output whatever became of the receipt,
// and the failed case read "No new output". The written receipt is the
// control.
func TestPromotionReceiptOwnsItsRead(t *testing.T) {
	const after = "Job `t2.1.1` is still running (4m12s)."
	for _, tc := range []struct {
		name     string
		fail     bool
		read     int64  // the committed cursor after the promotion's turn
		nextRead string // the next bash_output's answer
	}{
		{"the receipt written", false, int64(len("before\n")), after + jobNoNewOutput},
		{"the receipt's append failed", true, 0, after + jobNewOutput + "before\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := &toggledWrites{}
			bodies := make(chan *fakeBody, 1)
			b := openJobs(t, func(o *Options) {
				o.storeOpenFile = w.open
				o.tools.profiles = promotingProfile(bodies)
			})
			a := b.routers["test/a"]
			promote := callStep(callParts("c1", "bash", input(t, map[string]any{"command": "make"})))
			if tc.fail {
				a.route("go", func(ctx context.Context, yield func(fantasy.StreamPart) bool) {
					w.fail.Store(true) // the step's append, once its stream ends, fails
					promote(ctx, yield)
				})
				if _, err := b.s.Run(context.Background(), "next", nil); err == nil {
					t.Fatal("the turn whose append failed did not fail")
				}
				w.fail.Store(false)
			} else {
				a.route("go", promote, answerWith("moved on"))
				if _, err := b.s.Run(context.Background(), "next", nil); err != nil {
					t.Fatal(err)
				}
			}
			await(t, bodies, "the promoted job")
			// Its row counts from the command's start, the foreground phase
			// included (V3 F4), not from the promotion.
			if st := of[JobStarted](b.own.list()); len(st) != 1 || !st[0].Began.Equal(jobBegan) || !st[0].Promoted {
				t.Fatalf("JobStarted = %+v; want the command's start, %v", st, jobBegan)
			}
			if _, j := jobResultOf(t, b.s, "t2.1.1"); j.read != tc.read || len(j.reads) != 0 {
				t.Fatalf("after the promotion's turn the cursor is %d with %d reads open; want %d, none", j.read, len(j.reads), tc.read)
			}
			var ev events
			a.route("go", callStep(bashOutput(t, "o", "t2.1.1", 0)), answerWith("ok"))
			if _, err := b.s.Run(context.Background(), "again", ev.sink); err != nil {
				t.Fatal(err)
			}
			if got := of[ToolFinished](ev.list())[0].Result.Text; got != tc.nextRead {
				t.Fatalf("the next read = %q; want %q", got, tc.nextRead)
			}
		})
	}
}

// TestBashStopNeverQueuesBehindWaits (plan 033 C10r, review r7 finding 5): a
// step of five bash_output calls each waiting up to 600 s on a running job of
// its own — five ids, so the doom-loop guard sees no repeat — and a bash_stop
// of a sixth job, in either order. Fantasy runs a step's Parallel calls in
// five slots (agent.go:1671), so the five waits would hold them all and the
// stop would wait for one to end. Because the step stops a job, its waits are
// snapshots that say so: the stop's job ends at once, its block is the stop's
// answer, and the step — the turn — completes within the test's bound, far
// short of 600 s; nothing in it was observed. Before the fix it hung there.
// TestJobFinishesDuringAnOutputWait is the control: a step with no stop waits.
func TestBashStopNeverQueuesBehindWaits(t *testing.T) {
	for _, stopFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("the stop first %v", stopFirst), func(t *testing.T) {
			b := openJobs(t)
			for i := 1; i <= 6; i++ {
				fakeJob(t, b.s, fmt.Sprintf("t9.1.%d", i), "serve")
			}
			var parts [][]fantasy.StreamPart
			for i := 1; i <= 5; i++ {
				parts = append(parts, bashOutput(t, fmt.Sprintf("o%d", i), fmt.Sprintf("t9.1.%d", i), 600000))
			}
			stop := bashStop(t, "s", "t9.1.6")
			stopID, firstOutput := "t2.1.6", 1
			if stopFirst {
				parts, stopID, firstOutput = append([][]fantasy.StreamPart{stop}, parts...), "t2.1.1", 2
			} else {
				parts = append(parts, stop)
			}
			b.routers["test/a"].route("go", callStep(parts...), answerWith("done"))
			var ev events
			got := await(t, start(context.Background(), b.s, "next", ev.sink), "the step with five waits and a stop")
			if got.err != nil || got.res.StopReason != StopEndTurn {
				t.Fatalf("Run = %+v, %v", got.res, got.err)
			}
			evs := ev.list()
			if r, want := callResult(t, evs, stopID), jobBlockOf("t9.1.6", `status="stopped" by="you" duration="4m12s"`, "serve", ""); r.Text != want {
				t.Fatalf("bash_stop = %q\nwant %q", r.Text, want)
			}
			for i := range 5 {
				id := fmt.Sprintf("t2.1.%d", firstOutput+i)
				want := fmt.Sprintf(jobStillRunning, fmt.Sprintf("t9.1.%d", i+1), "4m12s") + stopStepNoWait + jobNoNewOutput
				if r := callResult(t, evs, id); r.Text != want || r.Observed || r.IsError {
					t.Fatalf("bash_output %s = %+v\nwant %q, not observed", id, r, want)
				}
			}
		})
	}
}

// TestAgentOutputDoesNotWaitInAStopStep: agent_output, whose waits share
// Fantasy's slots with bash_stop too, does the same — in a step that stops a
// job it answers at once, not observed, and says why; in another step of the
// same turn it waits (the control: its wait runs out, observed).
func TestAgentOutputDoesNotWaitInAStopStep(t *testing.T) {
	b := openBG(t)
	_, ids := b.spawn(t, "child one")
	b.s.subs.turn.Store(&turnLink{number: 9})
	defer b.s.subs.turn.Store(nil)
	b.s.subs.stopAnnounced(9, 1)
	r := b.s.subs.output(context.Background(), tool.OutputCall{CallID: "t9.1.2", ID: ids[0], Wait: tool.AgentOutputMaxWait})
	if want := fmt.Sprintf(outputStillRunning, ids[0]) + stopStepNoWait; r.Text != want || r.Observed {
		t.Fatalf("agent_output in a stop step = %+v; want %q at once, not observed", r, want)
	}
	if r := b.s.subs.output(context.Background(), tool.OutputCall{CallID: "t9.2.1", ID: ids[0], Wait: time.Millisecond}); r.Text != fmt.Sprintf(outputStillRunning, ids[0]) || !r.Observed {
		t.Fatalf("control: agent_output in another step = %+v; want its wait run out, observed", r)
	}
	b.s.subs.restoreTurn(9)
}
