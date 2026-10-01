package opencode

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/charliek/craze/internal/harness/tool"
)

// Background jobs, bash's side (plan 033 §3.7–§3.8). In a session that runs
// jobs (tool.Env.Jobs) every foreground command is one object — its group,
// its output pipe, the reader and the redaction and escape-sequence stages
// between them, its output and spill file: a bashJob — which the call either
// waits on to its end, as it always has, or hands to the session:
//
//   - promotion: a command still running at its timeout, in a call that is
//     neither cancelled nor closing, takes a job slot (supervise's question,
//     process.go) and is handed over instead of being stopped (promote);
//   - run_in_background: the call takes a slot before it starts anything —
//     a refusal means nothing started — starts the command, and hands it
//     over at once (runBackground).
//
// # The hand-over (pinned: §3.8)
//
// It happens exactly once, in JobSlot.Start, and nothing before it can fail:
// from that call on the job's goroutine owns the pipe's read end, the reader
// and its stages — and their tracking by the session (Jobs.Track) — the
// output and its spill writer, the group — its signals, its reaping and its
// release channel — and the progress; the call closes, stops or signals none
// of them again, and returns its receipt. Before it the
// call owns them all, and a promotion whose slot it took but never handed
// over gives the slot back and kills the command (supervised's deferred
// clean-up). The job's own Wait is the foreground's end, under the job's
// context and limit: a second supervise that takes up the group where the
// first stopped, then the same collect, close of the stages, finish and spill
// wait (ended), and the pipe's close.
//
// # The texts (§3.7, exact modulo values)
//
// Both receipts end with the fixed marker line (tool.JobMarker), and both go
// through the session's widest redaction as they are made (Jobs.Redact, P19):
// the call's own redactor was fixed when its turn began.

// bashJob is a started command: what Run started, and what a job takes over.
// Its fields are fixed once attach has made it, but copyErr, which the reader
// sets before it closes copied.
type bashJob struct {
	c       *bashCall
	g       *group
	r       *os.File // the output pipe's read end
	out     *output
	stream  *modelStream // the reader's stages: redaction, escape sequences, redaction
	copied  chan struct{}
	copyErr error // the reader's, set before copied closes
	began   time.Time
	closing <-chan struct{} // the session's (Env.Closing)
	// untrack ends the session's tracking of stream (tool.Jobs.Track, from
	// attach), once, as the command is done with: by the call that never
	// handed it over (supervised), else by the job's Wait. A no-op where the
	// session runs no jobs.
	untrack func()
}

// Output is tool.JobBody's: the output after byte from (output.since).
func (j *bashJob) Output(from int64) tool.JobOutput { return j.out.since(from) }

// Wait is tool.JobBody's: the job run to its end under ctx — the job's,
// cancelled by a stop or the session's close — for at most limit from now,
// and everything released. It is the foreground's end, under the job's
// context and limit (see the file's comment), and keeps its bounds: a stop
// returns within 6.7 s, a close within 2.7 s.
func (j *bashJob) Wait(ctx context.Context, limit time.Duration, progress tool.Progress) tool.JobEnd {
	defer j.untrack() // however Wait returns: a panic the harness recovers included
	stopProgress := j.out.report(progress)
	why, reaped := j.g.supervise(ctx, j.closing, limit, nil, j.copied, nil)
	o := j.ended(ctx, why, reaped, stopProgress)
	_ = j.r.Close()
	return j.c.jobEnd(o)
}

// jobEnd is how a job ended (tool.JobEnd): its status from how supervise
// ended it — a leader that exited by itself is exited, with its exit code, or
// failed when its exit status could not be read; the limit is timed_out; a
// cancel is stopped, by whom being the harness's to say — and its body, the
// tail with the notices a foreground result has, the exit code and the
// timeout being the result block's attributes instead (§3.7). The one notice
// kept as metadata is that the output may be incomplete.
func (c *bashCall) jobEnd(o outcome) tool.JobEnd {
	end := tool.JobEnd{ExitCode: -1}
	switch o.why {
	case endExit:
		end.Status = tool.JobFailed
		if o.state != nil {
			end.Status, end.ExitCode = tool.JobExited, exitCode(o.state)
		}
	case endTimeout:
		end.Status = tool.JobTimedOut
	default:
		end.Status = tool.JobStopped
	}
	end.Text = c.text(o)
	if o.partial {
		end.Text += "\n\n<shell_metadata>\n" + partialText + "\n</shell_metadata>"
	}
	end.Output = &tool.ExecOutput{ExitCode: end.ExitCode, Output: o.kept, Duration: o.took}
	return end
}

// runBackground is a run_in_background call in a session that runs jobs
// (plan 033 §3.7): a job slot first — the cap's refusal is the call's error,
// and nothing has started — then the command, started within the
// foreground's default timeout, and handed to the session at once; the call
// returns the start receipt. A cancel or a close that lands while the command
// starts stops it as any cancelled call's command is stopped, and it never
// becomes a job.
func (c *bashCall) runBackground(ctx context.Context, env tool.Env) tool.Result {
	slot, err := env.Jobs.Reserve(c.id)
	if err != nil {
		if errors.Is(err, tool.ErrClosing) {
			return aborted()
		}
		return tool.Result{Text: refusal(err), IsError: true, Class: tool.ClassToolError}
	}
	// Given back on every way out but the hand-over, after which a Release
	// does nothing (JobSlot: Start or Release, once).
	defer slot.Release()
	began := time.Now()
	deadline := began.Add(defaultTimeout) // the start's own bound: the job's limit runs from the hand-over
	g, r, err := c.launch(ctx, env, deadline)
	if err != nil {
		if errors.Is(err, errLaunchTimeout) {
			return tool.Result{Text: fmt.Sprintf(startTimeoutText, defaultTimeout.Milliseconds()), IsError: true, Class: tool.ClassTimeout}
		}
		return errorResult(err)
	}
	j := c.attach(env, g, r, began)
	if ctx.Err() != nil || tool.SessionClosing(ctx, env.Closing) {
		slot.Release()
		return c.supervised(ctx, env, j, deadline, false)
	}
	j.out.startSpill()
	slot.Start(tool.JobSpec{ID: c.id, Command: c.command, Workdir: c.dir, Limit: c.timeout, Began: began}, j)
	path := j.out.spillName(spillWait, env.Closing)
	return tool.Result{Text: env.Jobs.Redact(c.startReceipt(path))}
}

// promote hands a command that reached its timeout to the session as a job,
// in the slot supervise took for it (plan 033 §3.8): the output so far is
// read — what the receipt shows, and where the job's read cursor starts — the
// spill file started, so it holds the output from its first byte, and the job
// started, its limit counted from now; handed is set just before, the
// hand-over being the one step after which supervised must touch nothing of
// j's. The call returns the promotion receipt, naming the spill file once it
// has opened. Its progress has been stopped already.
func (c *bashCall) promote(env tool.Env, j *bashJob, slot tool.JobSlot, handed *bool) tool.Result {
	kept, cut, seen := j.out.peek()
	j.out.startSpill()
	*handed = true
	slot.Start(tool.JobSpec{ID: c.id, Command: c.command, Workdir: c.dir, Limit: tool.JobPromotedLimit,
		Promoted: true, Began: j.began, Seen: seen}, j)
	path := j.out.spillName(spillWait, env.Closing)
	return tool.Result{Text: env.Jobs.Redact(c.promotionReceipt(kept, cut, path)),
		Output: &tool.ExecOutput{ExitCode: -1, Output: kept, Duration: time.Since(j.began)}}
}

// The texts of a job's start (§3.7) and craze's own beside them.
const (
	// startTimeoutText answers a run_in_background call whose command did not
	// start within the foreground's default timeout (a filesystem that does
	// not answer, launch): nothing runs.
	startTimeoutText = "The command did not start within %d ms, so nothing is running in the background."
	// closingRefusal is a refused promotion's reason when the session began
	// to close between the timeout and the slot (P26); nobody reads it.
	closingRefusal = "The session is closing."
)

// startReceipt is a run_in_background call's result (§3.7, exact modulo
// values), with the note of a reduced limit, as metadata, before the marker.
// path is the spill file, "" when it could not be opened in time.
func (c *bashCall) startReceipt(path string) string {
	saved := "Its output is " + tool.JobSavedTo + path
	if path == "" {
		saved = "Its output could not be saved to a file."
	}
	text := tool.JobStartedHead + c.id + "`. It runs until it exits, until you stop it with bash_stop, " +
		"or for at most " + tool.JobLimit(c.timeout) + "; the session closing stops it too. " + saved + "\n" +
		"Its result is delivered to you when it finishes; do not poll it or sleep waiting for it. " +
		"Call bash_output with its id to read its output so far."
	if c.requested > 0 {
		text += "\n\n<shell_metadata>\n" + c.reducedText() + "\n</shell_metadata>"
	}
	return text + "\n" + tool.JobMarker(c.id)
}

// promotionReceipt is a promoted command's result (§3.7, exact modulo
// values): the output so far — the tail a finished command's result would
// show, "(no output)" when there is none, the truncation notice when it is cut
// — then, after one blank line, the metadata, the note of a reduced timeout
// first when there was one, and the marker. One blank line whether or not the
// output so far ends its last line (plan 033 C10r, V3 F5: a foreground
// result's "\n\n" after a tail ending in a newline left two).
func (c *bashCall) promotionReceipt(kept string, cut bool, path string) string {
	text := kept
	if text == "" {
		text = "(no output)"
	}
	if cut {
		text = "...output truncated...\n\n" + text
	}
	saved := "all of it is " + tool.JobSavedTo + path + "."
	if path == "" {
		saved = "it could not be saved to a file."
	}
	var meta []string
	if c.requested > 0 {
		meta = append(meta, c.reducedText())
	}
	meta = append(meta, fmt.Sprintf("The command did not finish within its timeout of %d ms. It was not stopped: it was moved to the background as "+
		"job `%s` and is still running, for at most %s. Its output so far is above; %s "+
		strings.TrimPrefix(tool.JobPromotedSavedEnd, ". ")+"; do not poll it or sleep waiting for it. "+
		"Call bash_output with its id to read newer output, or bash_stop to stop it.",
		c.timeout.Milliseconds(), c.id, strings.Replace(tool.JobLimit(tool.JobPromotedLimit), " ", " more ", 1), saved))
	return strings.TrimSuffix(text, "\n") + "\n\n<shell_metadata>\n" + strings.Join(meta, "\n") + "\n</shell_metadata>\n" + tool.JobMarker(c.id)
}

// reducedText is the note of a timeout the model asked for above the most it
// may: the foreground's, or a job's limit's.
func (c *bashCall) reducedText() string {
	if c.background {
		return fmt.Sprintf("The requested timeout of %d ms is above the maximum of %d ms for a background job; it runs for at most %d ms.",
			c.requested, tool.JobMaxLimit.Milliseconds(), tool.JobMaxLimit.Milliseconds())
	}
	return fmt.Sprintf("The requested timeout of %d ms is above the maximum of %d ms; the command ran with a timeout of %d ms.",
		c.requested, maxTimeout.Milliseconds(), maxTimeout.Milliseconds())
}

// refusal is Jobs.Reserve's refusal as the model reads it: the cap's text
// (tool.JobsFull), or a close's.
func refusal(err error) string {
	var full tool.JobsFull
	switch {
	case errors.As(err, &full):
		return full.Error()
	case errors.Is(err, tool.ErrClosing):
		return closingRefusal
	}
	return err.Error()
}
