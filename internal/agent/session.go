package agent

import (
	"context"
	"io"
	"os"
	"strings"
	"time"

	"github.com/charliek/craze/internal/acp"
)

// HomeDir is the home directory craze reads its own files out of: the config
// file, the user-level skills and the plugin caches. HOME wins over the account
// database so a test (and the frame runner) can isolate all of them with one
// variable.
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

// ErrForeignTurn refuses a prompt while the agent runs a turn of its own.
var ErrForeignTurn = acp.ErrForeignTurn

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
	// EventForeignTurn brackets a turn the agent started without a craze
	// prompt — grok's interject fallback. Nothing drains while one runs.
	EventForeignTurn EventType = "foreign_turn"
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
	CurrentModel   string
	CurrentMode    string
	// Provider is a value copy of the session's provider, so the UI can read
	// what a mode id means and what the agent is called without a session.
	Provider ProviderInfo
	// Queue is craze's own message queue in send order, cloned.
	Queue []QueuedPrompt
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
	// Interjection marks an EventUser that came from a grok interjection
	// broadcast rather than from a prompt craze sent.
	Interjection bool
	// ForeignTurn is set on EventForeignTurn.
	ForeignTurn *ForeignTurnInfo
	Err         error
	StopReason  string
	At          time.Time
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

// Option is one answer choice of a question.
type Option struct {
	ID    string
	Label string
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

type Result struct {
	StopReason string
}

type Options struct {
	Binary    string
	ExtraArgs []string
	Workspace string
	Force     bool
	Model     string
	Mode      string
	Stderr    io.Writer
	Env       []string
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
}

type Session interface {
	Start(ctx context.Context) error
	Prompt(ctx context.Context, text string) (Result, error)
	Events() <-chan Event
	Cancel(ctx context.Context) error
	// Queue appends a message to craze's own queue. It refuses a full queue
	// or an oversized message without mutating anything, so the caller keeps
	// the draft it tried to queue.
	Queue(text string) (QueuedPrompt, error)
	// EditQueued rewrites a row in place, keeping its id and position.
	EditQueued(id, text string) error
	// Unqueue drops a row the user cancelled.
	Unqueue(id string) (QueuedPrompt, bool)
	// TakeQueued removes any row so the caller can prompt it. It is a guard,
	// not a driver: it returns false while a prompt is in flight or the agent
	// is running a turn of its own, and it never prompts anything itself.
	TakeQueued(id string) (QueuedPrompt, bool)
	// PopQueue is TakeQueued of the head — the drain.
	PopQueue() (QueuedPrompt, bool)
	// ClearQueue empties the queue and returns how many rows went.
	ClearQueue() int
	// Interject merges text into the running turn without cancelling it.
	// Only grok can: everything else returns ErrUnsupported before the wire.
	Interject(ctx context.Context, text string) error
	AnswerPermission(id, optionID string) error
	AnswerQuestion(id string, answers map[string][]string, skip bool) error
	AnswerPlan(id string, accept bool) error
	SetModel(ctx context.Context, modelID string) error
	SetMode(ctx context.Context, modeID string) error
	SetConfig(ctx context.Context, id, value string) error
	Snapshot() Snapshot
	Close() error
}
