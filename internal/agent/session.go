package agent

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"time"

	"github.com/charliek/craze/internal/acp"
)

// HomeDir is the home directory craze reads the user-level skills and the
// plugin caches out of. HOME wins over the account database so a test (and the
// frame runner) can isolate them with one variable. CRAZE_HOME never moves
// them: it relocates only craze's own directory (internal/paths).
func HomeDir() string {
	if home := strings.TrimSpace(os.Getenv("HOME")); home != "" {
		return home
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(home)
}

var ErrPromptInFlight = acp.ErrPromptInFlight

// ErrUnsupported is a feature this provider does not have; nothing was
// written to the wire.
var ErrUnsupported = acp.ErrUnsupported

// ErrNoSuchSubagent is SubagentCanceller's refusal: the session has no
// sub-agent by that id whose turn is still running — the id never existed, or
// the child has already finished (plan 026 §3.10). Nothing was changed. After a
// stop the caller itself sent, it means the child's own end won the race: its
// finished roster row says how it ended, and there is nothing to report.
var ErrNoSuchSubagent = errors.New("agent: no such sub-agent")

// ErrForeignTurn refuses a prompt while the agent runs a turn of its own.
var ErrForeignTurn = acp.ErrForeignTurn

// ErrAgentExited is session.Close's answer when the agent's own exit had
// been reaped before craze's first Close on it sampled the child's wait
// channel — not that Close failed. Match it with errors.Is; internal/tui
// imports this package rather than internal/acp directly, which make lint
// forbids (Makefile's acp-import rule).
var ErrAgentExited = acp.ErrAgentExited

// ErrBadCatalog answers a settings change whose reply could not be read: its
// configOptions is not a list, or holds a member that is not an option (plan
// 025 design 1, "malformed is an error"). Nothing of the reply was installed,
// so the snapshot is exactly as it was before the call — but the call RAN: the
// agent has answered, and may well have made the change. It is not a refusal,
// and a client must not treat it as one: no fallback write for a model change
// (a second write craze did not mean), and no "the model is still X" — only
// that craze could not read what the agent now holds. It is acp.ErrBadCatalog,
// re-exported so internal/tui can match it without importing internal/acp
// (Makefile's acp-import rule), exactly as ErrAgentExited is.
var ErrBadCatalog = acp.ErrBadCatalog

// ErrPromptCancelled is a prompt Cancel stopped before its turn opened: while it
// was still waiting for the agent's first command catalog, or once it had been
// claimed by Begin and before its continuation opened the turn. No turn was
// opened and nothing reached the wire. Like the two refusals above it has no
// ending of its own — no EventDone, no EventError — so a consumer that draws a
// turn has to settle it on this.
var ErrPromptCancelled = errors.New("agent: prompt cancelled before it was sent")

// ErrSetUnavailable refuses a settings change before it mutates anything
// because the event log's outbox has no room for the delta it would publish —
// the rejectable-admission rule of plan 021 §3.3, which the engine spells
// engine.ErrUnavailable and maps to the code "unavailable". internal/agent
// cannot see that sentinel (the engine imports this package, never the
// reverse), so this is its own, beside ErrAskUnavailable, and engine.Code
// answers "unavailable" for it.
//
// Only SetTitle returns it. The verbs that ask a provider are refused for room
// by their caller, before the provider is asked at all: after the agent has
// taken a change, refusing it here would say something untrue.
var ErrSetUnavailable = errors.New("agent: the event log is backed up")

// ErrOptionGone answers a SetConfig the agent TOOK whose answer no longer has
// the option it set: the reply carried the agent's catalog, and that catalog
// — the one now installed — lists no option by that id (plan 025 design 1).
// Cursor's options are per model, so a change of model between the choice and
// the call is the ordinary way to get here.
//
// It is not a refusal, and nothing is undone: what the reply installed is
// installed, and its delta is enqueued like any other setter's, so
// SetOutcome.Ticket is set beside the error. Only the value this caller asked
// about is not there to confirm.
var ErrOptionGone = errors.New("agent: the agent's catalog no longer has that option")

// ErrStaleModel refuses a SetConfig bound to a model (its forModel) when the
// session is on another one by the time the call is about to be made: the
// option was chosen from that model's catalog, and is not a choice anyone made
// for this one (plan 025 design 3). Nothing was sent. The engine's settings
// worker refuses the same thing before it claims a Set, and answers with this
// very value (engine.ErrStaleModel), so a client matches one error whichever
// of the two checks caught it.
var ErrStaleModel = errors.New("agent: the model that option was chosen for is no longer the session's")

type EventType string

const (
	EventText       EventType = "text"
	EventThought    EventType = "thought"
	EventTool       EventType = "tool"
	EventTodos      EventType = "todos"
	EventPermission EventType = "permission"
	EventQuestion   EventType = "question"
	EventPlan       EventType = "plan"
	EventDone       EventType = "done"
	EventError      EventType = "error"
	EventMeta       EventType = "meta"
	EventUser       EventType = "user"
	EventSubagent   EventType = "subagent"
	// EventQueue is one change to craze's own message queue.
	EventQueue EventType = "queue"
	// EventCommand is one plugin command or skill craze expanded into the
	// prompt it is about to send. It is emitted before the request reaches the
	// wire, so it always precedes the turn's first agent event, and never at
	// all for a prompt the client refused.
	EventCommand EventType = "command"
	// EventForeignTurn brackets a turn the agent started without a craze
	// prompt — grok's interject fallback. Nothing drains while one runs.
	EventForeignTurn EventType = "foreign_turn"
	// EventReplay brackets a session/load replay: Phase start before the
	// first replayed event, Phase end once the restored snapshot is
	// installed. Every event between the two carries Event.Replayed; the
	// brackets themselves do not.
	EventReplay EventType = "replay"
	// EventTurn is one end of a turn the engine drove (plan 021 §3.4): started
	// is "prompt accepted, with its text", and ended is the turn's one ordered
	// ending, including the endings the wire never reports — a prompt
	// withdrawn before it was sent, and a prompt the session refused. No
	// session emits it: the engine, above this seam, is its one author.
	EventTurn EventType = "turn"
	// EventAsk is one ask's ending (plan 021 §3.6): a permission, question or
	// plan answered, cancelled, ended with its turn, closed, or decided by
	// craze's own approval policy. The openings keep the three kinds they
	// always had (EventPermission, EventQuestion, EventPlan); this is the
	// ending every one of them now gets, from the ask registry
	// (AskRegistry, asks.go), which is its one author.
	EventAsk EventType = "ask"
)

// ReplayInfo is one end of the session/load replay bracket.
type ReplayInfo struct{ Phase string }

// The two phases of a replay. Which phase an event fell between is what marks
// it replayed: cursor's replay carries no _meta at all (plan 013 §2.1), so a
// wire tag cannot be the answer for every provider.
const (
	ReplayStart = "start"
	ReplayEnd   = "end"
)

// TurnInfo is Event.Turn: one phase of one engine-driven turn (plan 021
// §3.4). A turn's id is "turn-N" and is scoped to the log's incarnation, as
// sequence numbers are: the counter restarts with the process. Phase started
// carries what admitted the turn; phase ended carries how it finished,
// synthetic or not, and what the settlement decided to run next.
type TurnInfo struct {
	// ID is this turn's id, "turn-N".
	ID string
	// Phase is TurnStarted or TurnEnded.
	Phase string
	// Text is the prompt's text, set on TurnStarted.
	Text string
	// Origin is TurnOriginSubmit, TurnOriginDrain or TurnOriginSendNow, set
	// on TurnStarted: what admitted this turn, which a client that draws a
	// drained or armed row differently from a typed one reads instead of
	// guessing from other state.
	Origin string
	// StopReason is set on TurnEnded: the wire's own stop reason on a real
	// ending, or the synthetic reason Synthetic marks.
	StopReason string
	// ErrClass is set on TurnEnded when the turn failed or was refused
	// synthetically: the same EventErrClass vocabulary Event.Err's codec
	// uses (eventcodec.go), extended with the three endings the wire never
	// reports on its own — ErrPromptCancelled, ErrPromptInFlight,
	// ErrForeignTurn — which ClassifyEventErr answers for too.
	ErrClass EventErrClass
	// Err is the failure as text, set on TurnEnded alongside ErrClass. It is
	// text and never an error value: an event enqueued through the log's
	// outbox (eventlog.go's Enqueue) is immutable once accepted, and the
	// engine has no live error value for a synthetic ending in the first
	// place — only what ClassifyEventErr made of one.
	Err string
	// Synthetic marks a TurnEnded the wire never produced: a withdrawn
	// prompt (ErrPromptCancelled), or a refusal turned into an ending
	// (ErrPromptInFlight, ErrForeignTurn).
	Synthetic bool
	// Next is the id of the successor turn this same settlement started, ""
	// if none. It is set exactly when a successor was reserved, so a client
	// never sees an ended with an empty Next that a moment later turns out
	// to have had one after all (plan 021 §3.4).
	Next string
	// Pending is how many rows were still queued after this settlement.
	Pending int
}

// The two phases a TurnInfo carries.
const (
	TurnStarted = "started"
	TurnEnded   = "ended"
)

// The three origins a TurnStarted carries: what admitted the turn.
const (
	TurnOriginSubmit  = "submit"
	TurnOriginDrain   = "drain"
	TurnOriginSendNow = "send_now"
)

// StateDelta is Event.State: the sections of the session's shared state that
// one event changed, each carried in full (plan 021 §3.8). It is how a client
// that folds the stream learns a change it did not make, and why an EventMeta
// need never again mean nothing but "call Snapshot()".
//
// Every section is a pointer, and nil means "this event did not touch that
// section" — not "that section is now empty". A section that has become empty
// is a non-nil value saying so, which is the only way one event can say
// "cleared" and another "unchanged" in the same field.
//
// **A delta is enqueued by the session under s.mu, in the section that mutates
// the snapshot** — or by the engine under e.mu, for the send-now section it
// owns — so the order of the deltas is the order of the state they describe,
// whoever changed it: the agent on the read loop, a client through
// engine.Control.Set, or craze's own /rename (plan 021 §3.8, panel astra 14 /
// CodeRabbit 7). **The revision of a settings change is the delta's Seq**, and
// because one author writes each section under one lock, a client that folds
// the stream and a client that reads Snapshot can never disagree about which
// change was last.
//
// A craze-initiated change carries its payload here and nowhere else:
// Event.Mode and Event.Text stay empty on it, because they are what an
// *agent*-initiated update fills and a client reads them as exactly that — one
// retires a plan offer, the other writes the index and prints a title line
// (plan 021 correction 20).
type StateDelta struct {
	// Title is the session's name, as Snapshot.Title now stands: an agent's
	// session_info_update, craze's own /rename, or the name a native session
	// takes from its first prompt. A non-nil empty string is a title that has
	// been cleared.
	//
	// A craze-initiated title (SetTitle) carries it here and leaves Event.Text
	// empty, so `/rename` prints no title line and writes no agent title to the
	// index; an agent-initiated one fills Event.Text as well, exactly as it
	// always did.
	Title *string
	// Mode is Snapshot.CurrentMode, and Model is Snapshot.CurrentModel, as they
	// now stand. As with Title, Event.Mode is filled beside Mode only for an
	// agent-initiated update, because a client reads that field as "the agent
	// changed mode" and retires a plan offer on it.
	Mode  *string
	Model *string
	// Config is every option the session advertises, in full — not the one that
	// changed. The section is the list, because that is what the provider's own
	// config_option_update carries and what a client mirrors; a delta that
	// named one option would leave a client unable to tell an option that went
	// away from one this event did not mention.
	Config *ConfigState
	// Commands is the agent's command catalog and Plugins the on-disk entries
	// resolved against it, both in full and for the same reason Config is. They
	// change together, on the one update that brings a new catalog.
	Commands *CommandsState
	Plugins  *PluginsState
	// SendNow is the engine's armed send-now, in full: nil when this event did
	// not touch it, Armed true for a send just armed, and a value with Armed
	// false for one that is gone, with Reason saying why.
	SendNow *SendNowState
	// Reason names what happened, from the SendNow* constants below. It usually
	// stands beside the section it is about — a send-now that was lost — but it
	// may stand alone, with every section nil: that is a delta whose news is the
	// event itself and not a change to any state a client mirrors. The one such
	// delta today is a cancel the engine made for an armed send-now that failed
	// after the client had already taken that send back: nothing about the
	// send-now changed (it was gone), and the failure is still the client's to
	// hear. A client reads each section it knows, and the reason and its detail,
	// independently.
	Reason string
	// Detail is the failure behind a Reason that has one, as text, and "" for
	// every Reason that does not. Today that is SendNowCancelFailed alone, and
	// only for the cancel the ENGINE made: a cancel a client asked for answers
	// that client with its error directly, so a detail here too would be the same
	// failure reported twice. The engine's own has no caller to answer.
	//
	// It is text and never an error value, for the reason TurnInfo.Err is: an
	// event enqueued through the log's outbox is immutable once accepted and is
	// encoded without calling anything on it (eventlog.go's Enqueue).
	Detail string
	// IndexErr is a session-index write that failed, as text: craze could not
	// remember this session in ~/.craze/sessions.jsonl, which is a note to the
	// user and never a reason to stop running (plan 021 §3.8, §2.4's "local
	// config and index write errors").
	//
	// It is a plain string and not a pointer, because it is a REPORT and not a
	// section of mirrored state: there is nothing for a client to hold, and so
	// nothing that could be "cleared" as against "untouched" — the same shape,
	// and for the same reason, as Reason and Detail beside it. "" means this
	// delta is not about a failed write.
	//
	// It exists because the engine writes the index on behalf of a command
	// whose answer has already gone back: Submit's first-prompt seed runs after
	// the turn has been handed to the caller, and the event-driven writes (the
	// agent's title, a turn's end) have no caller at all. Event.Cause names the
	// command where there was one. SetTitle is the one index write whose
	// failure is RETURNED instead, because its caller is still there
	// (engine.ErrIndexWrite).
	IndexErr string
}

// The three list sections of a StateDelta. Each is a struct around one slice
// rather than the slice itself, because a section has to be a pointer: nil
// means "untouched" and a non-nil empty value means "there are none", and a
// bare slice cannot say both. Each carries what the session's own snapshot
// holds, cloned, so the event shares no memory with it (plan 021 §3.8).
type (
	ConfigState   struct{ Options []ConfigOption }
	CommandsState struct{ Commands []CommandInfo }
	PluginsState  struct{ Plugins []PluginCommand }
)

// SendNowState is the send-now section of a StateDelta: what the engine has
// armed, or — with Armed false — that it has nothing armed any more. The text
// is carried because an armed send is text the client has not consumed
// anywhere else: nothing leaves a queue or a composer until it fires, so this
// is the only record that it was ever waiting.
type SendNowState struct {
	// Armed says a send-now is waiting for the turn it cancelled to settle.
	Armed bool
	// Text is what will be sent, and FromRow the queued row it will be
	// re-taken from, "" for text a client is holding itself.
	Text    string
	FromRow string
	// Turn is the turn it was armed against: the one whose settlement fires
	// it.
	Turn string
}

// Why an armed send-now was lost, on StateDelta.Reason. Each is a distinct
// path, because a client turns them into distinct words for the user: today's
// TUI notes for a withdrawn send, a cancel that failed and a row that had
// already gone are three different sentences.
const (
	// SendNowWithdrawn: a client took it back (engine.Control.Disarm).
	SendNowWithdrawn = "withdrawn"
	// SendNowCancelFailed: the cancel that was to make room for it never
	// reached the agent, so the turn it would have replaced is still running.
	SendNowCancelFailed = "cancel_failed"
	// SendNowTurnFailed: the turn it was armed against ended in an error.
	// Nothing runs from an error state, and the queue is cleared with it.
	SendNowTurnFailed = "turn_failed"
	// SendNowOtherTurn: the turn that settled was not the one it was armed
	// against, so it is no longer that turn's business.
	SendNowOtherTurn = "other_turn"
	// SendNowRowGone: the queued row it named had already left the queue when
	// it came to fire, so there was nothing left to send.
	SendNowRowGone = "row_gone"
	// SendNowStopped: the engine was stopped, which refuses every later
	// admission.
	SendNowStopped = "stopped"
	// SendNowClosing: the session is closing.
	SendNowClosing = "closing"
)

const (
	SubagentChangeSpawned  = "spawned"
	SubagentChangeProgress = "progress"
	SubagentChangeFinished = "finished"
)

type SubagentStatus string

const (
	SubagentRunning   SubagentStatus = "running"
	SubagentCompleted SubagentStatus = "completed"
	SubagentFailed    SubagentStatus = "failed"
	SubagentCancelled SubagentStatus = "cancelled"
)

type ModelInfo struct {
	ID   string
	Name string
}

type ModeInfo struct {
	ID          string
	Name        string
	Description string
}

type CommandInfo struct {
	Name        string
	Description string
}

type Snapshot struct {
	Models         []ModelInfo
	Modes          []ModeInfo
	Commands       []CommandInfo
	Config         []ConfigOption
	Tools          []ToolEvent
	Subagents      []SubagentInfo
	Todos          []Todo
	TodosUpdatedAt time.Time
	Title          string
	// SessionID is the agent's own id for this session, learned from
	// session/new or session/load. "" until Start has one.
	SessionID    string
	CurrentModel string
	CurrentMode  string
	// Provider is a value copy of the session's provider, so the UI can read
	// what a mode id means and what the agent is called without a session.
	Provider ProviderInfo
	// ForeignTurn reports that the agent is running a turn of its own. The
	// drain waits it out: a prompt sent now would be queued behind it.
	ForeignTurn bool
	// Plugins are the plugin commands and skills craze found on disk for this
	// provider, already resolved to the names the menu and the wire both use,
	// in discovery order. Cloned by Snapshot.
	Plugins []PluginCommand
}

// SubagentInfo is one grok child or cursor task, in spawn order on Snapshot.
// The per-child tool map stays private; consumers see tools only as EventTool.
type SubagentInfo struct {
	ID           string
	AttemptID    string
	ParentID     string
	ToolCallID   string
	Description  string
	SubagentType string
	Model        string
	Status       SubagentStatus
	Error        string
	Prompt       string
	Output       string
	Activity     string
	StartedAt    time.Time
	EndedAt      time.Time
	DurationMs   int
	ToolCalls    int
	Turns        int
	TokensUsed   int
	ToolsUsed    []string
	Transcript   bool
	Background   bool
}

// Todo is one entry of the cursor todo list. Status is normalised to
// pending | in_progress | completed | cancelled.
type Todo struct {
	ID      string
	Content string
	Status  string
}

type ConfigOption struct {
	ID           string
	Name         string
	Category     string
	Type         string
	Current      string
	SelectValues []SelectValue
}

type SelectValue struct {
	Value string
	Name  string
}

type Event struct {
	Type EventType
	Text string
	// Mode is the mode an EventMeta reports, set only for a current-mode
	// update. The snapshot alone cannot say a mode changed — one that went
	// plan → ask → plan leaves it exactly as it was — so the event has to
	// carry the fact that it changed at all.
	Mode string
	// Agent is the child session id for EventText/Thought/Tool/User that
	// belong to a sub-agent. "" is the main session.
	Agent          string
	Tool           *ToolEvent
	Todos          []Todo
	Permission     *PermissionEvent
	Question       *QuestionEvent
	Plan           *PlanEvent
	Subagent       *SubagentInfo
	SubagentChange string
	// Queue, QueueChange and QueuePos describe one EventQueue: the row, what
	// happened to it, and the position it held when it happened.
	Queue       *QueuedPrompt
	QueueChange QueueChange
	QueuePos    int
	// Command is the expansion an EventCommand reports.
	Command *ExpandedCommand
	// Interjection marks an EventUser that came from a grok interjection
	// broadcast rather than from a prompt craze sent.
	Interjection bool
	// ForeignTurn is set on EventForeignTurn.
	ForeignTurn *ForeignTurnInfo
	// Replay is set on EventReplay and names which end of the bracket this
	// is.
	Replay *ReplayInfo
	// Replayed marks an event the agent replayed out of its own history
	// rather than produced now. It is stamped on every event emitted between
	// the two EventReplay phases.
	Replayed bool
	// Turn is set on EventTurn: one phase of one engine-driven turn.
	Turn *TurnInfo
	// Ask is set on EventAsk: how one ask ended (plan 021 §3.6). It carries
	// the opening itself when no opening was ever published — a permission
	// craze's policy allowed, a request the provider had already answered, one
	// the log had no room to raise — so such an ending is self-contained.
	Ask *AskUpdate
	// State is set on an EventMeta the engine or the session authored to say
	// which sections of the shared state changed, each in full (plan 021
	// §3.8). It is what makes a meta event carry its news rather than mean
	// "re-read the snapshot".
	State *StateDelta
	// Cause is the Command (client/id, plan 021 §3.2) that caused an
	// engine-authored event, "" when none: an EventTurn, an EventAsk ending,
	// a settings delta the engine itself enqueued. It lets a client that
	// already applied its own command's effect from that command's own
	// return value skip only that one event's echo, without matching on
	// anything the payload carries (§3.4, §3.8) — the same contract a
	// socket client will use once the return and the event no longer arrive
	// together. A session's own event never sets it: every event this plan
	// found before it (§2) is still the session's alone.
	Cause      string
	Err        error
	StopReason string
	At         time.Time
	// Seq is assigned by the session's event log; 0 on an event that never
	// passed through one. It is the log's record envelope's, not the event's
	// own: the codec does not carry it (eventcodec.go), and Record.Event
	// sets it back from the envelope.
	Seq uint64
}

// ExpandedCommand is one plugin entry craze expanded into a prompt: the row the
// menu offered it as, embedded rather than transcribed so a name the resolver
// learns to spell differently reaches the event without a second edit, plus
// where the content came from and Text — the whole block as it went on the
// wire, so a headless caller can read exactly what the agent was given. The
// transcript shows only the provenance, never the body.
type ExpandedCommand struct {
	PluginCommand
	Path string
	Text string
}

// ForeignTurnInfo is a turn the agent is running on craze's session without a
// craze prompt. Running is true when it starts and false when it ends.
type ForeignTurnInfo struct {
	ID      string
	Text    string
	Running bool
}

type ToolEvent struct {
	ID          string
	Name        string
	Status      string
	Kind        string
	Title       string
	ToolName    string // rawInput._toolName, the cursor-side tool identity
	RawInput    string
	ContentText string
	Locations   []string
	Output      *ToolOutput
	Diffs       []ToolDiff
	Task        *TaskInfo
	At          time.Time
}

// ToolOutput is a tool_call rawOutput. Stdout/Stderr/Content are the 8 KiB
// tail, the *Head fields the first 512 B; Truncated is set if any cap hit.
type ToolOutput struct {
	ExitCode   *int
	Stdout     string
	Stderr     string
	Content    string
	StdoutHead string
	StderrHead string
	Truncated  bool
}

// ToolDiff is one diff content item. Added/Removed are counted on the
// uncapped text; Truncated means the stored text was capped (or the diff was
// too large to compute), so callers must not diff OldText/NewText themselves.
type ToolDiff struct {
	Path      string
	OldText   string
	NewText   string
	Added     int
	Removed   int
	Truncated bool
}

// TaskInfo describes a sub-agent tool call. Receipt is set once the matching
// cursor/task receipt has been joined in. Status is the joined child's
// lifecycle, so the spawn tool's transcript row can follow the child.
type TaskInfo struct {
	Description  string
	Prompt       string
	Model        string
	AgentID      string
	SubagentType string
	DurationMs   int
	Receipt      bool
	Status       SubagentStatus
	Background   bool
}

type PermissionOption struct {
	OptionID string
	Name     string
	Kind     string
}

// OptionIDForKind picks the option that answers a permission kind, the way
// yolo does on the wire: an option whose id spells the kind wins over the
// first of that kind, so grok's prepended "enable-always-approve" (kind
// allow_once) is never chosen for a plain allow-once.
func OptionIDForKind(opts []PermissionOption, kind string) (string, bool) {
	wire := make([]acp.PermissionOption, 0, len(opts))
	for _, o := range opts {
		wire = append(wire, acp.PermissionOption{OptionID: o.OptionID, Kind: o.Kind})
	}
	return acp.PickKind(wire, kind)
}

type PermissionEvent struct {
	ID      string
	Tool    string
	Options []PermissionOption
}

// Option is one answer choice of a question. Description is what picking it
// means, when the request said (plan 023 §3.4): the native harness's
// ask_user_question offers one per option, and cursor's questions carry none,
// so it is empty for every ACP provider.
type Option struct {
	ID          string
	Label       string
	Description string
}

type Question struct {
	ID            string
	Prompt        string
	Options       []Option
	AllowMultiple bool
}

// QuestionEvent carries a cursor/ask_question request. Auto is set when craze
// answered it without a user (headless); Answers is then what it sent.
type QuestionEvent struct {
	ID        string
	Title     string
	Questions []Question
	Auto      bool
	Answers   map[string][]string
}

// PlanEvent carries a cursor/create_plan request. Auto/Accepted mirror
// QuestionEvent for the headless path.
type PlanEvent struct {
	ID       string
	Name     string
	Overview string
	Plan     string
	Todos    []Todo
	Auto     bool
	Accepted bool
}

// AskLabel is one ask as a status line names it: `permission <tool>`,
// `question`, `plan <name>`. It is the header the TUI's own card draws
// (internal/tui's permissionView and planCardView), and therefore what a host
// publishes as the reason a session is blocked (host.Derive's CardLabel).
//
// It lives here because both readers need it and neither may import the other:
// the engine merges it into State.HeadAsk so a session with no TUI publishes
// the same label, and the TUI derives it from the card it is drawing. A
// question's own `1/2` counter is deliberately left out — a host shows one
// reason, not the card's progress through it — and nothing here sanitises,
// because both callers sanitise what they render.
func AskLabel(kind AskKind, body AskBody) string {
	switch kind {
	case AskQuestion:
		return "question"
	case AskPlan:
		name := ""
		if body.Plan != nil {
			name = body.Plan.Name
		}
		if strings.TrimSpace(name) == "" {
			name = "plan"
		}
		return "plan " + name
	default:
		tool := ""
		if body.Permission != nil {
			tool = body.Permission.Tool
		}
		return "permission " + tool
	}
}

// Label is this ask's AskLabel.
func (r AskRecord) Label() string { return AskLabel(r.Kind, r.Body) }

type Result struct {
	StopReason string
	// Unanswered is text the turn accepted mid-run and could not answer, in
	// the order it arrived — an interjection the harness had no step left to
	// take up before its turn ended. The engine requeues each one, last
	// first, ahead of whatever it decides to run next, before that decision
	// is made (plan 021 §3.5): the steered text must be ahead of a queued
	// row, never behind it. Only the native session can have any.
	Unanswered []string
}

type Options struct {
	Binary    string
	ExtraArgs []string
	Workspace string
	Force     bool
	Model     string
	Mode      string
	// Stderr is the agent child's own stderr sink. Diag is where craze's own
	// notes about this session go — discoverPlugins' warn closure — and
	// falls back to Stderr when nil, so headless craze prompt and craze
	// frame, which never set it, keep printing those notes on the same
	// stream as the agent's own diagnostics (§3.7.1). A TUI splits the two:
	// the agent's stderr is deferred and gated on the run having failed,
	// craze's own notes are not.
	Stderr io.Writer
	Diag   io.Writer
	Env    []string
	// PluginDirs are extra plugin roots to read, cursor-agent's --plugin-dir
	// by another route. A relative path is the workspace's. Providers whose
	// PluginScan does not want them ignore them.
	PluginDirs []string
	// Provider is the agent behind the session; nil is cursor.
	Provider *Provider
	// Interactive makes question and plan requests block on the session so a
	// UI can answer them; headless callers leave it false and craze
	// auto-answers.
	Interactive bool
	// Approval is what the session does with each kind of ask (plan 021 §3.6,
	// SD-26): park it for whoever is attached, or answer it itself. nil — what
	// every caller that has not been given one passes — derives exactly
	// today's behaviour from Force and Interactive, so approval policy and
	// frontend presence stop being the same fact without any caller changing
	// what it does (EffectiveApproval, asks.go).
	Approval *ApprovalPolicy
	// LoadSessionID resumes an existing agent session by id — session/load
	// instead of session/new. The agent replays the whole transcript before
	// the call returns, bracketed by EventReplay; a load that fails fails
	// Start, and craze never falls back to session/new.
	LoadSessionID string
	// Title and TitlePinned seed the session's title and its /rename pin
	// before the replay. No agent replays a title (plan 013 §2.1), so the
	// index row craze loaded the id from is the only place a resumed
	// session's title can come from.
	Title       string
	TitlePinned bool
	// NoPrimary builds the session's event log with no primary send
	// (EventLogOptions.NoPrimary): nothing is ever put on Events(), so no
	// publisher — the read loop, a handler, the log's own outbox — can be held
	// by a reader that is not there. A caller that sets it reads the session
	// through a subscription instead. S1b uses it in tests; from S4 on a
	// detached host is its user (SD-33).
	NoPrimary bool
	// ContentHome is the home directory a native session reads the user's own
	// Claude content under — commands, skills and the installed plugins
	// (§3.1). "" is HomeDir(), which is what production wants and what every
	// other provider already does through discoverPlugins; a test sets it to a
	// directory of its own, because from plan 022 C3 a native session started
	// with an empty Options reads whatever the developer happens to have
	// installed and its result would depend on the machine it ran on.
	ContentHome string
	// Compat is craze's [compat.claude] table: which classes of that content
	// this session reads at all (§3.5). The zero value reads everything,
	// which is what an absent table means and what every caller that does not
	// care gets; the CLI fills it from config.toml. Only a native session
	// reads it — no other provider's content is craze's to turn off.
	Compat ClaudeCompat
	// JournalDir is the session journal's directory (plan 020 §3.4), which
	// the CLI resolves once per run (paths.JournalDir, less the opt-outs).
	// "" means no journal, and is what every caller that does not ask for
	// one gets: nothing is written to disk. When set it must be absolute; a
	// journal that cannot be built is one line on Diag (else Stderr) and the
	// session runs without one. The file itself appears only once there is
	// something to write, so a session that is built and closed without
	// starting leaves nothing behind.
	JournalDir string
}

// CancelOutcome is what one Session.Cancel call is known to have done, from
// exactly what that call itself observed — never inferred from the schedule
// that led to it (plan 021 §3.7). It exists because a bare error cannot say
// whether the agent ever heard about the cancel, and the driver above this
// seam has to answer a client honestly: requested, settled, or — when the call
// gave up after a write may have happened — unknown.
//
//   - Wrote is true when this call is the one that put a cancel where the
//     turn could see it: live, session/cancel actually went out on the wire
//     — on the "no prompt of craze's own claimed" path as much as on the
//     "a claimed prompt reached the wire" path, since both write; native, a
//     running turn's context was cancelled by this call; the Stub, its
//     cancelsSent counter was incremented for it.
//   - Withdrew is true when a prompt claimed and not yet on the wire (live)
//     or not yet running (native, the Stub) found itself cancelled by this
//     call and will return ErrPromptCancelled with nothing ever sent. Wrote
//     and Withdrew are never both true: a withdrawal is exactly the case in
//     which nothing was written.
//   - Settled is true when the turn this call was for — or the fact that
//     there was none — is known to be over by the time Cancel returned:
//     nothing of craze's own was running or claimed at all, a withdrawal
//     that leaves nothing to wait for, or a wait for the turn's own ending
//     that the ending itself won. It is false when Cancel gave up not
//     knowing: its context ended, or the session closed, before the turn's
//     ending did. False is not "still running" — only "this call cannot
//     say" — which is why a caller that needs to know for certain waits for
//     the turn's own ending event rather than trusting a false here.
//
// Every implementation's own Cancel documents its exact mapping onto these
// three fields; this type is the contract they all answer to.
type CancelOutcome struct {
	Wrote, Withdrew, Settled bool
}

type Session interface {
	// Start spawns or opens the session and installs its first snapshot.
	//
	// # What it asks of its caller
	//
	// Starting a NEW session enqueues its install delta and flushes nothing, so
	// nothing Start does of its own waits for the primary's reader: a caller
	// may call it and begin reading only afterwards, which is exactly what
	// `craze prompt` does (r25 finding 1).
	//
	// One window in a new session's start is not Start's own and is older than
	// this plan: an update the agent sends BEFORE its session/new reply is
	// buffered by the ACP client and dispatched inside NewSession, on Start's
	// goroutine (acp's flushSessionUpdates), and an update that moves a setting
	// waits for the primary there exactly as it does on the read loop. An agent
	// that fills the primary's 256 slots that way, with nobody reading, wedges
	// a start until the session closes. Nothing here narrows that; it is
	// recorded so the requirement below reads as the sharpest one rather than
	// the only one.
	//
	// A LOAD (Options.LoadSessionID) is different, and always has been: the
	// replayed transcript is published from the client's read loop **during**
	// Start, so a replay longer than the primary's buffer blocks that read loop,
	// the session/load result is never read, and Start never returns. **A caller
	// that loads must therefore be reading the primary while Start runs** — the
	// TUI arms its reader in the same batch as the start command (tui.Model.Init)
	// and is the only caller that loads. The two flushes inside a load, which
	// order the seeded title before EventReplay{start} and the restored snapshot
	// before EventReplay{end}, rest on that same requirement and add nothing to
	// it.
	Start(ctx context.Context) error
	// Prompt is Begin(text)(ctx): the claim and the prompt back to back.
	Prompt(ctx context.Context, text string) (Result, error)
	// Begin claims the prompt slot now and returns the prompt to run, so a
	// caller that answers Esc on the goroutine it prompts from can claim the
	// turn in the same step that shows it working. A Cancel after Begin has
	// returned is for this prompt even before the continuation runs: the
	// prompt then withdraws, returning ErrPromptCancelled with nothing sent,
	// and the cancel writes nothing. A cancel asked before Begin is not for
	// it. A Begin while another prompt holds the slot claims nothing, and its
	// continuation returns ErrPromptInFlight. The continuation must be run
	// exactly once; the slot stays claimed until it returns.
	Begin(text string) func(ctx context.Context) (Result, error)
	Events() <-chan Event
	// Cancel stops whatever prompt of craze's own is running or claimed, and
	// reports what this call is known to have done. Every implementation's
	// exact mapping is in its own Cancel; CancelOutcome documents the
	// contract they all answer to (plan 021 §3.7).
	Cancel(ctx context.Context) (CancelOutcome, error)
	// ForeignTurn is Snapshot().ForeignTurn, as a leaf: it takes the
	// session's own mutex briefly and waits on nothing else — no provider
	// call, no other lock, no clone of anything (plan 021 §3.3). It exists so
	// a component above the seam — the engine — can read this one flag from
	// inside its own admission check, under its own lock (e.mu), without
	// paying for a whole Snapshot's clones (Models, Modes, Commands, Config,
	// Todos, Tools, Subagents) just to read one bool. This and Begin
	// are the two calls the engine may make under e.mu, both taking s.mu
	// briefly and waiting on nothing: the order is e.mu → s.mu, and it
	// cannot cycle, because the session never calls back into the engine. A
	// session with no notion of a foreign turn (native) always answers
	// false.
	ForeignTurn() bool
	// Interject merges text into the running turn without cancelling it.
	// Only grok can: everything else returns ErrUnsupported before the wire.
	Interject(ctx context.Context, text string) error
	// The three settings verbs. Each asks the provider first, **outside the
	// session's lock**, and only a change the provider took mutates anything:
	// a refusal returns its error and publishes nothing at all. A change that
	// took is then mutated into the snapshot and its StateDelta enqueued in
	// **one** locked section, so the order of the deltas is the order of the
	// state — against each other, and against the agent's own updates on the
	// read loop, which enqueue theirs in the section that mutates too (plan 021
	// §3.8).
	//
	// Where the provider's REPLY says what it now holds — the live session's
	// set_config_option answers with the whole catalog — that part of the
	// mutation is the read loop's, made as the reply is read, so it is ordered
	// against the agent's own updates by the wire; the setter's section then
	// announces the snapshot as it finds it (plan 025 design 1). The one error
	// that still publishes is ErrOptionGone: the change took, and what it
	// installed is announced, but the option asked about is no longer there.
	//
	// cause is the command that asked, as Event.Cause spells it ("client/id",
	// engine.Command.Cause), and "" for a change nobody can claim. The
	// SetOutcome is what the change came to: the value the session is now at,
	// and the delta's receipt.
	//
	// They block on the provider and belong on a goroutine that is not the
	// primary's reader, exactly as they always have. They never refuse for want
	// of room in the log: the caller checks that *before* the provider is asked,
	// because a refusal after the agent has taken the change would be a lie.
	//
	// SetConfig's forModel binds the change to the model it was chosen for
	// (engine.Setting.ForModel, plan 025 design 3): when it is set and the
	// session is on any other model, the call is ErrStaleModel and nothing is
	// asked of the provider. The check is the session's own current model, read
	// under the session's lock at the last moment before the provider is asked,
	// so a model the agent moved on its own and the session has already heard
	// about is seen. "" binds it to nothing.
	SetModel(ctx context.Context, cause, modelID string) (SetOutcome, error)
	SetMode(ctx context.Context, cause, modeID string) (SetOutcome, error)
	SetConfig(ctx context.Context, cause, id, value, forModel string) (SetOutcome, error)
	// SetTitle renames the session in craze alone: ACP v1 has no rename verb.
	// It pins the title, so a later agent session_info_update no longer
	// replaces it, and it publishes the Title section of a StateDelta —
	// enqueued under the session's lock with the pin, never published — so it
	// still **waits on nothing** and may be called from a UI's own update
	// goroutine, as it always has been.
	//
	// It is the one settings verb that asks no provider, so its check for room
	// in the log is atomic with its mutation and it can honestly refuse:
	// ErrSetUnavailable, with nothing changed and nothing pinned.
	SetTitle(cause, title string) error
	Snapshot() Snapshot
	Close() error
}

// SetOutcome is what one settings verb came to: the value the session is now
// at, and the receipt of the delta that said so.
//
// Value is the **confirmed** value, captured in the very section that
// announced the change — never asked for afterwards with a second read, which
// could observe somebody else's change and answer this caller about it. It is not
// always the value that was asked for: the native session resolves an empty
// effort to the model's own default and a model alias to its canonical id, and
// a provider is free to do the same. A caller that echoes what it requested
// would then contradict the very delta this outcome carries the revision of
// (plan 021 §3.8, r23 finding 4).
//
// Ticket is the delta's receipt: after a Flush its Seq is the change's
// **revision**, what a client compares a delayed reply against. It is nil when
// nothing was published (a session that publishes no deltas, a log that is
// closing), and a nil Ticket answers Seq 0 — "no revision".
type SetOutcome struct {
	Value  string
	Ticket *Ticket
}

// Clocked is a session that will say what time it is. It exists so that a
// component above the seam stamps Event.At from the *session's* clock rather
// than from one of its own (plan 021 §3.9): the Stub's clock is injected by the
// tests that pin craze's frames, so an engine reading time.Now directly would
// make a golden depend on the wall clock. The live and native sessions answer
// with time.Now, the one source their own emits stamp from, so there is exactly
// one clock per session either way.
//
// Like LogOwner and EventSource it is optional and found by a type assertion; a
// session without it is read as time.Now.
type Clocked interface {
	Now() time.Time
}

// SubagentCanceller is a session that can stop one of its sub-agents — the
// row whose SubagentInfo.ID is id — and nothing else of the turn (plan 026
// §3.10): the parent's turn goes on, and its model reads that the user stopped
// that child. Like Clocked it is optional and found by a type assertion, so
// Session itself does not change; Capabilities.SubagentCancel is what a client
// reads to offer the stop at all. The native session implements it.
//
// The contract:
//
//   - It waits on nothing: no turn, no provider call, no Publish or Flush. A
//     client may call it from the primary's own reader.
//   - Its outcome is event-only. nil says the stop was delivered, not what it
//     came to: the child's finished roster row does — cancelled, its Error
//     "stopped by the user" — unless the child had already ended its turn on
//     its own (it keeps that outcome) or the parent's own cancel or close
//     outranked the stop (cancelled with no such Error).
//   - It is idempotent: a second stop of a child still running returns nil and
//     changes nothing.
//   - ErrNoSuchSubagent, with nothing changed, for an id that never existed or
//     a child whose turn has ended. After a stop the caller itself sent, that
//     is the race with the child's own end: a client shows nothing for it (at
//     most "already finished"), never an error row.
type SubagentCanceller interface {
	CancelSubagent(id string) error
}
