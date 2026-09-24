package harness

import (
	"time"

	"github.com/charliek/craze/internal/harness/store"
	"github.com/charliek/craze/internal/harness/tool"
)

// Event is one thing a running turn reports to Run's sink, as it happens.
// The set is closed; the adapter switches on the concrete type:
//
//   - the answer: TextDelta, ThoughtDelta;
//   - tool calls: ToolStarted, ToolCalled, ToolProgress, ToolFinished;
//   - the turn's course: StepDone, Steered, Retrying, Diag;
//   - the harness's own state, projected: Todos;
//   - sub-agents (plan 026 §3.9): SubagentStarted, SubagentEvent — one of a
//     child's own events, wrapped — and SubagentFinished.
//
// Every field is a plain value that survives a JSON round trip, so a
// journal can record exactly what the sink was handed (plan 019 §3.5);
// SubagentEvent's one interface field holds another of these. Text from a
// tool — a result, a request, progress — is redacted of every provider key
// (plan 019 §3.8); text from the model is raw, and the adapter sanitizes it
// before a terminal sees it. A child's events keep the contract they had in
// the child: its text raw, its tools' text redacted by its own dispatcher.
//
// A tool call is identified by its ID, the harness's own "t<turn>.<step>.<n>":
// unique in the session, and the name of the call's spill file. The
// provider's id for it (CallID) is provider text — it may repeat or be empty
// — and only joins the call to the transcript's message parts. Every call
// the runner reports goes ToolStarted, then ToolCalled once its arguments
// are complete and Fantasy announces it (a call cut off before that, or left
// unannounced by an abnormal finish, never has one), then any number of
// ToolProgress, then exactly one ToolFinished, and all of them before the
// StepDone of their step, or, for a step that never finished, before Run
// returns.
type Event interface{ isEvent() }

// TextDelta is the next piece of the answer's text, as the provider streamed
// it. It is the model's raw output: the adapter sanitizes it before a
// terminal sees it (plan 018 §3.8).
type TextDelta struct{ Text string }

// ThoughtDelta is the next piece of the model's reasoning ("thinking"), raw
// like TextDelta.
type ThoughtDelta struct{ Text string }

// ToolStarted reports that the model began a tool call: its name is known,
// its arguments are still streaming (they are not forwarded). Kind and
// ReadOnly are the profile's for Tool, zero for a name the profile does not
// have.
type ToolStarted struct {
	ID       string
	Step     int // the step's number in the turn, from 1
	Tool     string
	Kind     tool.Kind
	ReadOnly bool
}

// ToolCalled reports a call whose arguments are complete, as prepared for
// the gate and the call's card. Calls that will not run — an unknown tool,
// arguments the tool refused — are reported too; their ToolFinished says
// why.
type ToolCalled struct {
	ID      string
	CallID  string // the provider's id, redacted
	Request ToolRequest
	At      time.Time
}

// ToolRequest is a prepared call as the dispatcher describes it (tool.Request),
// every text field redacted. Input is the raw arguments as a string: they
// may not be valid JSON.
type ToolRequest struct {
	Tool     string
	Kind     tool.Kind
	ReadOnly bool
	Title    string   // the relative path, the command, the pattern
	Paths    []string // resolved absolute paths the call touches
	Command  string   // execute
	Workdir  string   // execute
	Input    string
}

// ToolProgress is a snapshot of a running call's output: the whole of what
// its card should show now, not a delta, redacted. Progress is lossy by
// contract (plan 019 §3.5): a snapshot is dropped rather than wait on
// anything, and none arrives after the call's ToolFinished.
type ToolProgress struct {
	ID     string
	Output string
}

// ToolFinished reports a call's result: what the model is sent (Result.Text)
// and what a card shows, redacted and, for the model's text, truncated.
// Every call a ToolStarted announced gets exactly one, whether or not it
// ran. Duration runs from its ToolCalled, and is zero for a call that never
// had one.
type ToolFinished struct {
	ID       string
	Result   tool.Result
	At       time.Time
	Duration time.Duration
}

// StepDone reports a finished model step: what it was, what it cost, and
// whether it was persisted. It comes after the step's text and tool calls,
// once the step's append was tried.
//
// The persistence outcome is explicit. Saved says the step's entries were
// written, and Entries are their ids in file order (the held switches and
// user entries written with it included) — only then. With Saved false,
// SaveError says why the append failed, or is "" when the step had nothing
// to persist (thinking alone) or was refused unwritten (ErrBadToolCalls).
type StepDone struct {
	Step int // the step's number in the turn, from 1
	// The model the step ran on: the table's provider id, alias and wire id.
	Provider, Model, WireModel string
	// Finish is the step's finish reason as Fantasy saw it; FinishRaw is what
	// the provider sent, before the harness's wrapper turned a "stop" with
	// tool calls into "tool-calls" (D-21). They differ only then.
	Finish, FinishRaw string
	// StopReason is the step's own, as its entry records it: tool_use for a
	// step whose tool calls ran and continue the turn.
	StopReason string
	// TimeToFirstToken runs from the request to the first text, reasoning or
	// tool call it streamed; zero when it streamed none.
	TimeToFirstToken time.Duration
	Usage            Usage
	Saved            bool
	Entries          []string
	SaveError        string
	// SubagentUsage is what the step's sub-agents spent, a row per model
	// (plan 026 §3.7): the rows its tool entry carries as subagent_usage when
	// it is Saved, and reported here whether or not it was, so a consumer can
	// keep a durable record of the ones no entry holds — an append that failed
	// (SaveError), or a step refused for its call ids — which the transcript
	// never will (panel P40). nil for a step with no sub-agent.
	SubagentUsage []ModelUsage
}

// Steered reports a steer the turn accepted (Session.Steer): text the user
// interjected into the running turn. It is emitted when a step takes the steer
// up, and, for one no step reached, as the turn settles — always by the turn's
// own goroutine and always before Run returns, so nothing an adapter shows for
// an interjection can follow the turn's ending event.
//
// Text is the caller's own, unchanged: it never went near the model or a
// provider. A Steered says the turn took the text, not that it was persisted;
// Result.Unanswered says which ones were not.
type Steered struct{ Text string }

// Retrying reports that the step failed before producing any output and will
// be sent again after Delay (at most once a step: the runner allows one
// retry). Attempt counts the retries of this step, from 1; Reason is the
// failure, on one line. Nothing streamed before it belongs to the answer.
type Retrying struct {
	Delay   time.Duration
	Attempt int
	Reason  string
}

// Todos reports the harness's todo list, in full, after a todo_write call
// (plan 023 §3.4): never a delta, so an adapter that only ever applies the
// latest one received cannot drift from the store even if two calls'
// events somehow reached it apart — and, since sessionTodos.Write (todos.go)
// makes the mutation and this emit one critical section, they never do:
// the events a session emits arrive in exactly the order Write produced the
// lists. Items is a copy the store made for this event alone. Not persisted
// in the transcript (H7 may add it).
type Todos struct{ Items []tool.Todo }

// Diag reports what has no other event: results the runner wrote for calls
// that never ran, a save that failed, a step refused for its call ids, a
// doom-loop nudge and the stop that follows it.
// Kind names the case (the Diag* constants); Fields are its facts. The
// adapter shows none of it; it is for the journal and for diagnosis.
type Diag struct {
	Kind   string
	Fields map[string]string
}

// The Diag kinds.
const (
	// DiagNotExecuted: a step finished abnormally ("length", "error",
	// "content-filter", "unknown") with tool calls, and the runner wrote a
	// not_executed result for each (D-43).
	DiagNotExecuted = "not_executed"
	// DiagSynthesized: a turn ended without its step finishing, after tool
	// calls had been announced, and the runner wrote the step itself from
	// what it had seen, an aborted result for every call without one.
	DiagSynthesized = "synthesized"
	// DiagSaveFailed: a finished step could not be persisted; the turn stops
	// before another request.
	DiagSaveFailed = "save_failed"
	// DiagBadToolCalls: a step's tool calls had an empty or repeated provider
	// id; nothing in it ran and it was not persisted (ErrBadToolCalls).
	DiagBadToolCalls = "bad_tool_call_ids"
	// DiagDoomLoop: the doom-loop guard refused a call the model had made
	// identically three times or more in a row (§3.7). Fields name the step,
	// the call, the tool, the count, and whether this one also ended the turn
	// ("stopped"), which the fifth does.
	DiagDoomLoop = "doom_loop"
)

// SubagentStarted reports that the agent call CallID (its ToolStarted.ID)
// opened a sub-agent: a child session with the id ID, which names its
// transcript and tags every SubagentEvent and the SubagentFinished of it
// (plan 026 §3.9). It is emitted once the child has opened, and never for a
// call that waited for a slot and then did not get one, or whose child failed
// to open: that call's result says why.
//
// Type is the agent type the call resolved to; Description and Prompt are
// the call's, the prompt exactly as the child was sent it. Model and Effort
// are the alias and effort the child runs on (§3.6), and Mode the mode it
// opened in, its parent's then (§3.5). The three texts from the call are
// redacted of every provider key; the rest are the model table's and craze's
// own words. At is the parent's clock (Options.Now).
type SubagentStarted struct {
	ID, CallID, Type, Description, Prompt, Model, Effort, Mode string
	At                                                         time.Time
}

// SubagentEvent is one of the child ID's own events, exactly as the child's
// turn handed it to its sink: its text and thinking raw, its tool events
// redacted by its own dispatcher (plan 026 §3.9). Event is never another
// SubagentEvent: a sub-agent starts none of its own.
type SubagentEvent struct {
	ID    string
	Event Event
}

// SubagentFinished reports that the child ID has ended and been closed. It
// comes after every SubagentEvent of that child and before the parent's
// ToolFinished for the call that started it (plan 026 §3.9).
//
// Status is SubagentCompleted, SubagentFailed or SubagentCancelled; Error is
// what failed, "" otherwise. Text is the child's final message — its last
// step's text that had any — or, for one that failed or was stopped, its last
// output; Error and Text are redacted. Usage is observed: the sum of the
// child's StepDone usages, so every step it was billed for counts, however it
// ended (§3.7). Provider, Model and WireModel name the model it ran on;
// ToolCalls counts its announced tool calls and Steps its finished steps.
// Duration runs from its SubagentStarted, and At is the parent's clock.
type SubagentFinished struct {
	ID, Status, Error, Text    string
	Usage                      Usage
	Model, Provider, WireModel string
	ToolCalls, Steps           int
	Duration                   time.Duration
	At                         time.Time
}

// The statuses a SubagentFinished reports. A child that finished its turn —
// cut off, refused and stopped by a limit included, which its result
// explains — completed; one whose turn failed, or that could not run, failed;
// one its parent's cancel or close, or the user, stopped cancelled.
const (
	SubagentCompleted = "completed"
	SubagentFailed    = "failed"
	SubagentCancelled = "cancelled"
)

func (TextDelta) isEvent()        {}
func (ThoughtDelta) isEvent()     {}
func (ToolStarted) isEvent()      {}
func (ToolCalled) isEvent()       {}
func (ToolProgress) isEvent()     {}
func (ToolFinished) isEvent()     {}
func (StepDone) isEvent()         {}
func (Steered) isEvent()          {}
func (Retrying) isEvent()         {}
func (Diag) isEvent()             {}
func (Todos) isEvent()            {}
func (SubagentStarted) isEvent()  {}
func (SubagentEvent) isEvent()    {}
func (SubagentFinished) isEvent() {}

// Usage is a step's or a turn's token counts: input, output, reasoning, and
// the prompt-cache reads and writes, which show whether a provider's prefix
// cache hit. It is the transcript's own shape (store.Usage), so what a turn
// reports and what it records cannot drift apart.
type Usage = store.Usage

// ModelUsage is one model's row of sub-agent usage (StepDone.SubagentUsage):
// the transcript's own shape (store.ModelUsage), as Usage is, so what a step
// reports and what its tool entry records cannot drift apart.
type ModelUsage = store.ModelUsage
