// Package tool is the native harness's tool framework: what a tool is, how a
// call is prepared, judged, run, redacted and truncated, and how a model's
// tool set is chosen (plan 019 §3.1).
//
// It is craze's own contract, not Fantasy's. A Tool never sees a Fantasy type
// and this package links no Fantasy code, even transitively; the harness's
// toolbridge.go is the one file that adapts a Tool to fantasy.AgentTool. So
// the tool contract survives replacing Fantasy's loop (plan 019 §3.4), and
// schemas are the hand-written maps each Spec carries, never reflection. Of
// craze it may import only internal/atomicfile and internal/harness/redact;
// depguard and deps_test.go hold that line.
//
// The three seams the plan requires live here: Tool itself (Seam 1), Profile
// and Registry (Seam 2), and Gate (Seam 3). A call goes through a Dispatcher,
// in this order: Prepare (parse and resolve the input once), the gate, Run,
// redaction of everything outward, truncation of the model's text.
package tool

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"time"

	"github.com/charliek/craze/internal/harness/redact"
)

// Kind is what a tool does, for the gate and for how a card renders.
type Kind string

const (
	KindRead    Kind = "read"
	KindEdit    Kind = "edit"
	KindExecute Kind = "execute"
	KindSearch  Kind = "search"
	// KindTodo is todo_write (plan 023 §3.4): the TUI's generic row prints a
	// kind verbatim, and --json does too, so this is its own kind rather than
	// KindOther, which would read "other" on both.
	KindTodo Kind = "todo"
	// KindAsk is a call that blocks on the person: ask_user_question and
	// exit_plan_mode (plan 023 §3.4). Its own kind for the same reason.
	KindAsk Kind = "ask"
	// KindTask is a call that runs a sub-agent: the agent tool (plan 026
	// §3.3). "task" is grok-build's word for its own spawn_subagent, and the
	// adapter draws a task-kind call as the TUI's existing task row
	// (ToolEvent.Task); discovery's 05 guessed "think" before the design.
	KindTask Kind = "task"
)

func (k Kind) valid() bool {
	switch k {
	case KindRead, KindEdit, KindExecute, KindSearch, KindTodo, KindAsk, KindTask:
		return true
	}
	return false
}

// Direction is which end of a long result the dispatcher keeps.
type Direction int

const (
	// Head keeps the start: a file, a listing, search results. It is the
	// zero value, so a tool that forgets to choose is still truncated.
	Head Direction = iota
	// Tail keeps the end: command output, where the error is last.
	Tail
	// None leaves Result.Text alone: the tool truncated it itself, spilled
	// the full text itself (OpenSpill), and filled Result.Trunc.
	None
)

func (d Direction) valid() bool { return d == Head || d == Tail || d == None }

// ErrorClass says why a call failed, for diagnosis (plan 019 §3.5). It is ""
// for a call that did not fail; a failed result a tool leaves unclassified
// is tool_error.
type ErrorClass string

const (
	ClassDenied       ErrorClass = "denied"        // the gate said no
	ClassInvalidInput ErrorClass = "invalid_input" // unknown tool, or Prepare refused the input
	ClassNotFound     ErrorClass = "not_found"     // the file or directory does not exist
	ClassTimeout      ErrorClass = "timeout"       // the tool's own deadline passed
	ClassAborted      ErrorClass = "aborted"       // the turn was cancelled
	ClassDoomLoop     ErrorClass = "doom_loop"     // the same call once too often (§3.7)
	ClassOutputLimit  ErrorClass = "output_limit"  // the input or output is too large to handle
	ClassNotExecuted  ErrorClass = "not_executed"  // an abnormal finish left the call unrun
	ClassToolError    ErrorClass = "tool_error"    // anything else, including a panic
)

// Spec is a tool's model-facing contract and how the harness treats it.
type Spec struct {
	// ID is the name the model calls the tool by.
	ID string
	// Description is what the model reads, rendered (see Render).
	Description string
	// Parameters are the JSON Schema properties of the tool's one object
	// argument, by name. The wire schema is {type: object, properties:
	// Parameters, required: Required}.
	Parameters map[string]any
	// Required names the parameters the model must send. It is never nil,
	// so it marshals as [] and never null: registration refuses a nil one.
	Required []string
	Kind     Kind
	// ReadOnly promises the call changes nothing on disk or elsewhere.
	ReadOnly bool
	// Parallel lets Fantasy run the call alongside others (up to five).
	// Calls that are not parallel run one at a time (plan 019 §2.4).
	Parallel bool
	// Truncate is which end of a long Result.Text the dispatcher keeps.
	Truncate Direction
}

// Call is one tool call as the model made it.
type Call struct {
	// ID is the harness's id for the call, "t<turn>.<step>.<n>": unique in a
	// session, safe as a file name, and what events and spill files use.
	ID string
	// CallID is the provider's id. It is provider text — possibly empty,
	// repeated, or "../" — so it lives only in message parts (plan 019 §3.5).
	CallID string
	// Tool is the name the model called.
	Tool string
	// Input is the raw JSON arguments, exactly as the model sent them; it
	// may not be valid JSON.
	Input json.RawMessage
}

// Tool is one tool: its contract, and how it turns a call into something
// that can run.
type Tool interface {
	Spec() Spec
	// Prepare validates and resolves the raw input once — types, signs,
	// ranges, and relative paths against env.Workspace — and returns what
	// will run. It must not act on anything. An error becomes an
	// invalid_input result and the tool never runs; its text goes to the
	// model, so it should say what to fix.
	Prepare(env Env, call Call) (Prepared, error)
}

// Prepared is a call that passed Prepare. The dispatcher keeps it from
// Prepare to Run, so the gate judges exactly what runs.
type Prepared interface {
	// Request is what the gate and the call's card need, before anything
	// runs. The tool fills Title, Paths, Command and Workdir; the
	// dispatcher fills the rest from the Call and the Spec.
	Request() Request
	// Run does the work. It never returns a Go error: Fantasy treats one as
	// fatal to the whole turn (plan 019 §2.4), so every failure is a Result
	// with IsError set. It must return promptly once ctx is done, with class
	// aborted and the text AbortedText.
	Run(ctx context.Context, env Env) Result
}

// Request describes a prepared call. The copy the dispatcher returns and
// shows the gate is redacted.
type Request struct {
	ID       string // the harness id (Call.ID)
	CallID   string // the provider's id (Call.CallID)
	Tool     string
	Kind     Kind
	ReadOnly bool
	Title    string   // the relative path, the command, the pattern: a card's one line
	Paths    []string // resolved absolute paths the call touches
	// Targets are the paths an edit-kind call will really write to: absolute,
	// and resolved through RealPath, so a relative spelling, a symlink, a
	// dangling one and a file that does not exist yet all normalize to what
	// the eventual open lands on. Prepare fills it for the tools that write;
	// a gate enforcing where a call may write judges this and never Paths,
	// which is cleaned but not resolved and is redacted for the card.
	//
	// It is the one field of a Request that is not redacted, and it never
	// leaves the dispatcher: the copy handed to the gate carries it and the
	// copy returned for an event does not (plan 023 §3.1).
	Targets []string
	Command string // execute: the raw command
	Workdir string // execute: the resolved directory it runs in
	// Input is the call's raw arguments; it may not be valid JSON, so a
	// consumer that marshals a Request must not assume it is.
	Input json.RawMessage
}

// Result is what a call produced.
type Result struct {
	// Text is what the model sees.
	Text    string
	IsError bool
	Class   ErrorClass
	// StopTurn asks the runner to end the turn after this step. The
	// dispatcher clears it on whatever a tool returns: only the harness
	// decides that (the doom-loop guard, §3.7).
	StopTurn bool
	Output   *ExecOutput // execute
	Content  string      // read: the text a card may expand
	Edits    []FileEdit  // edit, write: the adapter diffs them
	Trunc    Truncation
	// Child is what a sub-agent spent, for the agent tool alone (plan 026
	// §3.7): the sum of its steps' usage and the model they ran on, on every
	// path it ended by — a failure and a cancel included, since a billed step
	// is billed however the child ended. The harness records it on the step's
	// tool entry as subagent_usage, priced by its own model rather than the
	// parent's, which stamps that entry. nil for every other tool, and for an
	// agent call that never opened a child.
	Child *ChildUsage
}

// ExecOutput is a command's outcome, for its card.
type ExecOutput struct {
	ExitCode int
	Output   string // the merged stdout and stderr, as kept
	Duration time.Duration
}

// FileEdit is one file's content before and after an edit or a write. Old is
// "" for a new file.
type FileEdit struct {
	Path     string
	Old, New string
}

// Truncation reports how much of a result's text the model got, and where
// the rest went. It is zero when nothing measured the text.
type Truncation struct {
	KeptBytes, TotalBytes int
	KeptLines, TotalLines int
	// Spill is the file holding the full text, or "" when there is none:
	// nothing was cut, or the file could not be written.
	Spill string
}

// Truncated reports whether the model got less than the whole text.
func (t Truncation) Truncated() bool {
	return t.KeptBytes < t.TotalBytes || t.KeptLines < t.TotalLines
}

// Progress takes a snapshot of a running call's output — the whole of what
// the card should show now, not a delta. It is lossy by contract: the
// dispatcher's Progress drops a snapshot sent within 100 ms of the last one
// or while the consumer is still taking the last one, and never blocks.
//
// The consumer at the other end — the Progress handed to Dispatcher.Run —
// must return promptly: the runner drops progress for a finished call and
// sends the rest without blocking. One that blocks never holds up Run or
// the tool; it holds only the one goroutine delivering to it, at most one
// per call, until it returns.
type Progress func(snapshot string)

// Env is what a call needs from its session. The dispatcher hands every
// Prepare and Run a copy, with Progress set for that call.
type Env struct {
	// Workspace is the session's working directory, absolute. Relative
	// paths in a call resolve against it (Resolve).
	Workspace string
	// Home is the harness's directory. Spill files go under it, and the file
	// tools refuse its providers.toml by resolved path.
	Home string
	// Progress reports a running call's output. It is never nil in an Env
	// the dispatcher passes; in Prepare it discards. A tool may call it as
	// often as it likes: it returns at once, whatever the consumer is doing
	// (see Progress), and does nothing once Run has returned.
	Progress Progress
	// Redactor holds every loaded provider key. The dispatcher redacts every
	// result on its own; a tool uses this only where text leaves it before
	// the result does (bash wraps its output in NewWriter, so the progress
	// snapshot and the spill file are redacted too).
	Redactor *redact.Replacer
	// Environ is the environment a child process gets: the user's, less
	// craze's own provider keys (ChildEnviron). nil is no environment
	// configured, and a tool that starts processes refuses to run rather
	// than fall back to craze's own, provider keys and all (plan 019 §3.8).
	Environ []string
	// Locks serializes craze's own writes to a file (PathLocks).
	Locks *PathLocks
	// Closing is closed when the session is closing. A tool that runs
	// processes must stop them at once when it closes, with no grace, even
	// if its ctx was already cancelled for another reason: a ctx's cause is
	// fixed by its first cancel, so this is how a cancel becomes a close. A
	// ctx cancelled with ErrClosing means the same (SessionClosing). nil
	// never closes.
	Closing <-chan struct{}
	// Todos is the harness's todo list, reached only by todo_write
	// (plan 023 §3.4): a narrow seam of pure data, since this package may
	// not import internal/harness, which owns the concrete store and
	// publishes the harness.Event{Todos} that follows every write. nil when
	// no caller wired one (a harness test, or a build with no todos) —
	// todo_write then returns a tool_error result rather than panic.
	Todos TodoStore
	// Asker is how a call reaches the person (asker.go, plan 023 §3.4). nil
	// when the session was opened with none: ask_user_question and
	// exit_plan_mode then answer at once that nobody answered.
	Asker Asker
	// PlanPath is the session's plan file, absolute: what exit_plan_mode
	// reads. The dispatcher sets it per call (SetPlanPath), since a session
	// learns it only once its transcript is named. "" is no plan file.
	PlanPath string
	// Subagents runs the agent tool's calls (plan 026 §3.1): the harness's
	// runner, which opens a child session per call, behind the same kind of
	// narrow seam as Todos and Asker, since this package may not import the
	// harness. nil when the session starts no sub-agents — a sub-agent's own
	// session, whose depth is 1, or a build that wired no runner — and the
	// agent tool then answers with a tool_error result.
	Subagents Subagents
}

// Resolve returns path as an absolute, cleaned path: a relative path is taken
// against the workspace, an absolute one as it is. A leading "~" is not
// expanded, as opencode does not expand it (plan 019 §3.2). Symlinks are not
// resolved; a file tool does that itself, before it locks or checks the path.
func (e Env) Resolve(path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Join(e.Workspace, path)
}

// AbortedText is opencode's result text for a call cut short by a cancel
// (session/processor.ts:585-608).
const AbortedText = "Tool execution aborted"

// ChildEnviron returns environ (os.Environ's form, NAME=value) without the
// variables that carry craze's own provider keys: every name in keyNames —
// the env_keys of every provider in providers.toml — and every OPENAI_*
// variable, which the OpenAI SDK reads (plan 019 §3.8). Everything else
// stays, other credentials included: gh, git push and cloud CLIs are the
// point of a shell. Names match exactly, as the environment does. It returns
// a new slice and leaves environ alone.
func ChildEnviron(environ, keyNames []string) []string {
	drop := make(map[string]bool, len(keyNames))
	for _, n := range keyNames {
		drop[n] = true
	}
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if drop[name] || strings.HasPrefix(name, "OPENAI_") {
			continue
		}
		out = append(out, kv)
	}
	return out
}
