package agent

import (
	"context"
	"io"
	"time"

	"github.com/charliek/craze/internal/acp"
)

var ErrPromptInFlight = acp.ErrPromptInFlight

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
	Todos          []Todo
	TodosUpdatedAt time.Time
	Title          string
	CurrentModel   string
	CurrentMode    string
	// Provider is a value copy of the session's provider, so the UI can read
	// what a mode id means and what the agent is called without a session.
	Provider ProviderInfo
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
	Mode       string
	Tool       *ToolEvent
	Todos      []Todo
	Permission *PermissionEvent
	Question   *QuestionEvent
	Plan       *PlanEvent
	Err        error
	StopReason string
	At         time.Time
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
// cursor/task receipt has been joined in.
type TaskInfo struct {
	Description  string
	Prompt       string
	Model        string
	AgentID      string
	SubagentType string
	DurationMs   int
	Receipt      bool
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
	AnswerPermission(id, optionID string) error
	AnswerQuestion(id string, answers map[string][]string, skip bool) error
	AnswerPlan(id string, accept bool) error
	SetModel(ctx context.Context, modelID string) error
	SetMode(ctx context.Context, modeID string) error
	SetConfig(ctx context.Context, id, value string) error
	Snapshot() Snapshot
	Close() error
}
