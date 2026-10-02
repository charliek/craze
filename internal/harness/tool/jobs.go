package tool

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charliek/craze/internal/harness/redact"
)

// Background jobs (plan 033 §3.7–§3.8, owner decisions 2 and 3). An
// interactive session's bash command can outlive its call: one the model
// starts with run_in_background, and one that reaches its timeout and is
// moved to the background instead of being killed (promotion). Either is a
// job: the command, its process group and its output, owned by the session
// rather than the call, running until it exits, until the model stops it
// (bash_stop) or the user does (the stop key), until its limit, or until the
// session closes. Its result is delivered to the model once, as a background
// sub-agent's is (plan 026 §3.11): at a step boundary, through bash_output or
// bash_stop, or by the wake.
//
// Everything here is pure data on both sides, like the sub-agent runner's
// seam (subagents.go): the bash tool, which runs the command, hands the
// running command to the harness through Env.Jobs, and the harness, which
// this package may not import, owns the registry, the delivery and the
// lifetime. The harness's side is internal/harness/jobs.go; the tool's is
// opencode's bash.go, bash_output.go and bash_stop.go.

// The jobs' tools' ids, beside AgentTool and AgentOutputTool: the profile
// offers bash_output and bash_stop right after bash (opencode.go), a child is
// given neither (MapClaudeTools drops both, and the harness withholds both),
// and ModeGate knows bash_stop by name.
const (
	BashTool       = "bash"
	BashOutputTool = "bash_output"
	BashStopTool   = "bash_stop"
)

// JobsAware is a tool whose offer depends on whether the session runs
// background jobs (plan 033 X101). A session that runs none — headless `craze
// prompt`, and every sub-agent, whose Env.Jobs is nil — is offered
// WithoutJobs() in the tool's place, or nothing when that is nil: bash
// without run_in_background or a word of jobs in its description, and no
// bash_output or bash_stop. Draft 1 offered every session the whole surface,
// each background line qualified "when the session supports it", and headless
// models asked for the background all the same — a dev server then ran in the
// foreground until its timeout killed it (the D-69 A/B, verify/v3-ab).
//
// The choice is the harness's, made once at Open from whether the session runs
// jobs, and never changes within the session: the specs are part of every
// request's prefix (D-30). A variant keeps the tool's id and is held to the
// same rules: Registry.Register validates it as it does the tool. What the
// variant's Prepare and Run do with a field its spec no longer offers is the
// tool's own defence — bash still reads run_in_background, and runs the
// command in the foreground and says so.
type JobsAware interface {
	WithoutJobs() Tool
}

// JobType is a job's agent type wherever a background child's would be: the
// row a client draws for it (P12: "bash job" — persona names are slugs, so no
// agent type can be spelled with a space), and the running-tasks list of a
// compaction's state section.
const JobType = "bash job"

// A job's limits (plan 033 §3.7).
const (
	// JobDefaultLimit and JobMaxLimit are a run_in_background call's timeout
	// — the job's limit — when it gives none, and the most it may give; a
	// longer one is reduced to it, and the receipt says so.
	JobDefaultLimit = 30 * time.Minute
	JobMaxLimit     = 2 * time.Hour
	// JobPromotedLimit is a promoted command's limit, counted from the
	// promotion, not from the command's start.
	JobPromotedLimit = 30 * time.Minute
	// JobOutputMaxWait is the most a bash_output call waits for its job to
	// end; its default is 0, a snapshot (P13).
	JobOutputMaxWait = 600 * time.Second
	// JobStopWait is the most a bash_stop call waits for its job to end once
	// it has stopped it: the command's 3 s between SIGTERM and SIGKILL, the 2
	// s a shutdown may take after that, and the reading of what is left of
	// its output, with room to spare.
	JobStopWait = 7 * time.Second
)

// The statuses a job ends with (§3.7): exited by itself, with its exit code;
// stopped — by the model (bash_stop), the user (the stop key) or the session
// closing; timed out at its limit; or failed, when how it ended could not be
// learned.
const (
	JobExited   = "exited"
	JobStopped  = "stopped"
	JobTimedOut = "timed_out"
	JobFailed   = "failed"
)

// JobUnknown is the status a resumed session's notice gives a job its last
// incarnation started and never delivered (plan 033 C10r, V3 F1): the session
// closed before the job's result came through, so whether the command had
// finished by then, and how, is not known — it may have exited, its result
// waiting (P14's suspended results among them), been killed by the close, or,
// when craze crashed rather than closed, still be running, no longer managed
// (C11r2, review r8 finding 8). It is no ending a running job reports.
const JobUnknown = "unknown"

// Who stopped a job, as the delivered result's by attribute says it (§3.7):
// the model reading the result is "you".
const (
	JobStoppedByYou  = "you"
	JobStoppedByUser = "the user"
)

// JobsFull is Jobs.Reserve's refusal when the session already runs as many
// jobs as it may (P14): its text is what the model reads — as the error of a
// run_in_background call, which then starts nothing, and in the
// <shell_metadata> of a command that reached its timeout and was killed for
// want of a slot (P26).
type JobsFull struct{ Max int }

func (e JobsFull) Error() string {
	return fmt.Sprintf("%d background jobs are already running; stop one with bash_stop first.", e.Max)
}

// JobSpec is a job as the bash tool hands it over (Jobs.Reserve, then
// JobSlot.Start).
type JobSpec struct {
	// ID is the bash call's harness id ("t4.2.1"): the job's id, which the
	// model names it by, and its spill file's name.
	ID string
	// Command is the command as the model wrote it, and Workdir the
	// directory it runs in, resolved.
	Command, Workdir string
	// Limit is how long the job may run from Start: a run_in_background
	// call's timeout, or JobPromotedLimit for a promoted command.
	Limit time.Duration
	// Promoted says the command reached its timeout in the foreground and was
	// moved to the background, rather than started there.
	Promoted bool
	// Began is when the command started, a promoted one's foreground time
	// included: what the job's duration counts from.
	Began time.Time
	// Seen is how many bytes of output the call's own result covered — a
	// promotion's shows the output so far — and so where the job's reads go
	// on from once that result is written: the harness counts it a read the
	// bash call made, committed with the receipt, and forgotten when no
	// append writes it, as a bash_output read is. 0 for a run_in_background
	// call.
	Seen int64
}

// JobOutput is what a running job has written so far, after a read cursor
// (JobBody.Output).
type JobOutput struct {
	// Text is the output after the cursor that the model is shown: its last
	// lines, within the same limits as a finished command's tail (MaxLines,
	// MaxBytes), from a line's start.
	Text string
	// Skipped counts the bytes after the cursor that Text leaves out: older
	// output the tail no longer holds, or that did not fit.
	Skipped int64
	// Total is how many bytes the command has written in all: the cursor a
	// read that commits moves to.
	Total int64
	// Spill is the file that holds the output from its first byte, "" when
	// there is none: it could not be opened, or was given up.
	Spill string
}

// JobEnd is how a job ended, as its body reports it (JobBody.Wait).
type JobEnd struct {
	// Status is JobExited, JobStopped, JobTimedOut or JobFailed. Who stopped
	// a stopped job is the harness's to say: it holds the stop's cause.
	Status string
	// ExitCode is the leader's exit status as a shell reports one, -1 when
	// there is none: the command was stopped, or its leader never reaped.
	ExitCode int
	// Text is the result's body as the model reads it after the command's
	// line: the tail of the output, with the notice of what was cut and the
	// file that holds the rest, as a finished command's result has them.
	Text string
	// Output is the outcome for the job's card.
	Output *ExecOutput
}

// JobBody is a started command as the bash tool hands it to the harness: the
// process group, its output pipe, the reader and the redaction and
// escape-sequence stages between them, the progress and the spill file. From
// JobSlot.Start on it is the harness's, which calls Wait exactly once, on a
// goroutine of its own, and Output whenever bash_output reads it.
type JobBody interface {
	// Wait runs the job to its end and returns how it ended. ctx is the
	// job's: the session's, never a turn's, cancelled by a stop or by the
	// session closing (ErrClosing, which kills at once). limit is how long it
	// may run from now; progress takes snapshots of its output, as a call's
	// Progress does. Wait releases everything the body holds before it
	// returns — the command's group and its reaping, the pipe, the stages,
	// the spill file — within the bounds a foreground call has.
	Wait(ctx context.Context, limit time.Duration, progress Progress) JobEnd
	// Output is the output after byte from, as JobOutput describes it. It
	// takes only memory locks, and is safe while Wait runs and after.
	Output(from int64) JobOutput
}

// JobSlot is one reserved job slot: Start hands it a started command, or
// Release gives it back unused. Exactly one of the two is called, once.
type JobSlot interface {
	// Start adopts body as the job spec describes: registered, reported
	// (JobStarted) and run to its end on a goroutine of the harness's. It
	// never fails: from the call on, body is the harness's, and a session
	// that began to close meanwhile kills it at once.
	Start(spec JobSpec, body JobBody)
	// Release gives the slot back: the command never started.
	Release()
}

// Jobs is a session's background jobs, behind Env.Jobs (plan 033 §3.8). nil
// in a session that runs none — headless `craze prompt`, and a sub-agent's
// session (P11, D-59) — which is offered no jobs surface (JobsAware, X101): a
// run_in_background its model sends anyway runs in the foreground, and a
// timeout kills.
type Jobs interface {
	// Reserve takes a job slot for the bash call id before anything is
	// started (§3.8): an explicit run_in_background reserves before it
	// launches its command, so a refusal means nothing started, and a
	// promotion reserves at the timeout, so a refusal kills as before (P26).
	// The error is JobsFull at the cap, or wraps ErrClosing once the session
	// has begun to close.
	Reserve(id string) (JobSlot, error)
	// Output answers a bash_output call, Stop a bash_stop call; neither
	// returns a Go error, and both answer aborted once ctx is done. Their
	// texts are capped and redacted already, so both tools' specs are
	// Truncate None.
	Output(ctx context.Context, call JobOutputCall) Result
	Stop(ctx context.Context, call JobStopCall) Result
	// Redact is the session's widest redaction (its Session.Redact): what a
	// job's model-facing text — the receipts the bash tool writes — goes
	// through as it is produced (P19), since the call's own redactor was
	// fixed when its turn began.
	Redact(text string) string
	// Track registers a command's output stream for as long as it runs, so
	// that its redaction learns every key the session learns meanwhile (plan
	// 033 C10r, review r7 finding 9): Track widens s to the session's widest
	// key set at once, and again each time that set grows — a stored key at a
	// turn's start, a provider's on a model switch, a token as it is minted —
	// until untrack is called. The bash tool tracks every command of a
	// session that runs jobs from the moment its output is read (any of them
	// may become a job: promotion), and untracks it once the stream is done
	// with; untrack may be called more than once. A command's text decided
	// before a key was learned stays as it was (redact.Writer.Widen).
	Track(s KeyedStream) (untrack func())
}

// KeyedStream is a command's output stream as Jobs.Track holds it: one whose
// redaction can be widened while it runs. Widen is called from the session's
// goroutines, concurrently with the stream's own writes, and must take r's
// keys up between two writes — for every stage of the stream at once — and
// never write, drop or reorder a byte (opencode's modelStream).
type KeyedStream interface {
	Widen(r *redact.Replacer)
}

// JobOutputCall is one bash_output call as the tool prepared it.
type JobOutputCall struct {
	// CallID is the bash_output call's harness id, which names the step it
	// runs in: what it delivers, or the read it makes, is committed by that
	// step's append.
	CallID string
	// ID is the job's id as the model gave it, trimmed.
	ID string
	// Wait is how long to wait for the job to end, from 0 (a snapshot) to
	// JobOutputMaxWait.
	Wait time.Duration
}

// JobStopCall is one bash_stop call as the tool prepared it.
type JobStopCall struct {
	CallID string // the bash_stop call's harness id (JobOutputCall.CallID)
	ID     string // the job's id as the model gave it, trimmed
}

// The fixed marker line that ends both receipts — a run_in_background call's
// and a promotion's (P15): `<background_job id="t4.2.1"/>`, nothing before or
// after it on its line. A resumed session finds the jobs its last incarnation
// started by this line in its tool results, and nowhere else.
const (
	jobMarkerOpen  = `<background_job id="`
	jobMarkerClose = `"/>`
)

// JobMarker is the marker line for the job id, a harness call id.
func JobMarker(id string) string { return jobMarkerOpen + id + jobMarkerClose }

// ParseJobMarker reads the marker off text's last line: the job id it names,
// when that line is exactly a marker naming a harness call id
// ("t<turn>.<step>.<n>"), and ok false otherwise — a marker quoted inside a
// line, one with anything around it on its line, or one naming anything else.
// A caller that knows which call wrote text compares the id with that call's
// own, since a command's output can print any line.
func ParseJobMarker(text string) (id string, ok bool) {
	line := text[strings.LastIndexByte(text, '\n')+1:]
	id, found := strings.CutPrefix(line, jobMarkerOpen)
	if !found {
		return "", false
	}
	id, found = strings.CutSuffix(id, jobMarkerClose)
	if !found || !harnessCallID(id) {
		return "", false
	}
	return id, true
}

// harnessCallID reports whether id has the shape of a harness call id:
// "t", then three dot-separated runs of ASCII digits.
func harnessCallID(id string) bool {
	rest, found := strings.CutPrefix(id, "t")
	if !found {
		return false
	}
	parts := strings.Split(rest, ".")
	if len(parts) != 3 {
		return false
	}
	for _, p := range parts {
		if p == "" || len(p) > 9 || strings.Trim(p, "0123456789") != "" {
			return false
		}
	}
	return true
}

// JobDuration is how a job's running time is written in its texts: whole
// seconds, as "45s", "3m05s" or "1h02m03s" (§3.7's duration="4m12s" and "is
// still running (3m05s)").
func JobDuration(d time.Duration) string {
	s := int64(d.Round(time.Second) / time.Second)
	if s < 0 {
		s = 0
	}
	h, m := s/3600, s/60%60
	s %= 60
	switch {
	case h > 0:
		return fmt.Sprintf("%dh%02dm%02ds", h, m, s)
	case m > 0:
		return fmt.Sprintf("%dm%02ds", m, s)
	}
	return fmt.Sprintf("%ds", s)
}

// JobLimit is a job's limit as its receipt says it: in hours, minutes or
// seconds when it is a whole number of them — "30 minutes", "2 hours" — and in
// milliseconds otherwise.
func JobLimit(d time.Duration) string {
	unit := func(n int64, one string) string {
		if n == 1 {
			return "1 " + one
		}
		return fmt.Sprintf("%d %ss", n, one)
	}
	switch {
	case d > 0 && d%time.Hour == 0:
		return unit(int64(d/time.Hour), "hour")
	case d > 0 && d%time.Minute == 0:
		return unit(int64(d/time.Minute), "minute")
	case d > 0 && d%time.Second == 0:
		return unit(int64(d/time.Second), "second")
	}
	return fmt.Sprintf("%d ms", d.Milliseconds())
}
