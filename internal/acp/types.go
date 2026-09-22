package acp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"
)

const (
	ProtocolVersion = 1
	AuthCursorLogin = "cursor_login"
	AuthXAIAPIKey   = "xai.api_key"
	AuthCachedToken = "cached_token"

	MethodInitialize        = "initialize"
	MethodAuthenticate      = "authenticate"
	MethodSessionNew        = "session/new"
	MethodSessionLoad       = "session/load"
	MethodSessionPrompt     = "session/prompt"
	MethodSessionCancel     = "session/cancel"
	MethodSessionSetModel   = "session/set_model"
	MethodSessionSetMode    = "session/set_mode"
	MethodSessionSetConfig  = "session/set_config_option"
	MethodSessionUpdate     = "session/update"
	MethodRequestPermission = "session/request_permission"
	MethodCursorAskQuestion = "cursor/ask_question"
	MethodCursorCreatePlan  = "cursor/create_plan"
	MethodCursorUpdateTodos = "cursor/update_todos"
	MethodCursorTask        = "cursor/task"

	MethodGrokAskUserQuestion        = "x.ai/ask_user_question"
	MethodGrokAskUserQuestionWrapped = "_x.ai/ask_user_question"
	MethodGrokExitPlanMode           = "x.ai/exit_plan_mode"
	MethodGrokExitPlanModeWrapped    = "_x.ai/exit_plan_mode"
	MethodGrokPromptComplete         = "x.ai/session/prompt_complete"
	MethodGrokPromptCompleteWrapped  = "_x.ai/session/prompt_complete"

	MethodGrokSessionNotification        = "x.ai/session_notification"
	MethodGrokSessionNotificationWrapped = "_x.ai/session_notification"

	// Interject is the only grok extension craze writes. Live grok 1.0.30
	// answers the wrapped name and 404s the bare one, so the wrapped name is
	// what goes out; both are accepted inbound.
	MethodGrokInterject        = "x.ai/interject"
	MethodGrokInterjectWrapped = "_x.ai/interject"
	// Interjection is the broadcast every client renders the user block from.
	// There is no user_message_chunk for an interjection.
	MethodGrokInterjection        = "x.ai/session/interjection"
	MethodGrokInterjectionWrapped = "_x.ai/session/interjection"
	// QueueChanged is grok's server-side queue broadcast. craze does not use
	// that queue; it reads this only to learn turn identity (§3.2).
	MethodGrokQueueChanged        = "x.ai/queue/changed"
	MethodGrokQueueChangedWrapped = "_x.ai/queue/changed"

	// UpdateTurnCompleted rides x.ai/session_notification and is the one
	// terminator grok sends for every turn, its own interject fallback
	// included — which sends no prompt_complete at all (captured live,
	// 2026-09-14). It is what ends a foreign turn.
	UpdateTurnCompleted = "turn_completed"

	// InterjectFallbackPrefix names the prompt grok mints when an
	// interjection cannot be merged into a running turn. Such a completion
	// never ends a craze turn.
	InterjectFallbackPrefix = "interject-fallback-"

	// InterjectStatusQueued is the only ack status craze accepts.
	InterjectStatusQueued = "queued"

	UpdateAgentMessage      = "agent_message_chunk"
	UpdateAgentThought      = "agent_thought_chunk"
	UpdateToolCall          = "tool_call"
	UpdateToolCallUpd       = "tool_call_update"
	UpdateAvailableCommands = "available_commands_update"
	UpdateCurrentMode       = "current_mode_update"
	UpdateConfigOption      = "config_option_update"
	UpdateSessionInfo       = "session_info_update"
	UpdatePlan              = "plan"

	// GrokEmptyPlanMarkdown is the plan card body when exit_plan_mode
	// omitted planContent.
	GrokEmptyPlanMarkdown = "# No plan written yet\n\n(The agent exited plan mode without writing a plan.)"

	KindAllowOnce    = "allow_once"
	KindAllowAlways  = "allow_always"
	KindRejectOnce   = "reject_once"
	KindRejectAlways = "reject_always"

	StopEndTurn   = "end_turn"
	StopCancelled = "cancelled"
)

// DialectID selects the provider-shaped branch of the ACP client. The client
// stays one NDJSON JSON-RPC connection; only method names and reply envelopes
// differ per dialect.
type DialectID string

const (
	DialectCursor DialectID = "cursor"
	DialectGrok   DialectID = "grok"
)

type Implementation struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type FSCapabilities struct {
	ReadTextFile  bool `json:"readTextFile"`
	WriteTextFile bool `json:"writeTextFile"`
}

type ClientCapabilities struct {
	Meta     map[string]any `json:"_meta,omitempty"`
	FS       FSCapabilities `json:"fs"`
	Terminal bool           `json:"terminal"`
}

type InitializeParams struct {
	ProtocolVersion    int                `json:"protocolVersion"`
	ClientInfo         Implementation     `json:"clientInfo"`
	ClientCapabilities ClientCapabilities `json:"clientCapabilities"`
}

type InitializeResult struct {
	ProtocolVersion   int             `json:"protocolVersion"`
	AgentInfo         *Implementation `json:"agentInfo,omitempty"`
	AuthMethods       []AuthMethod    `json:"authMethods,omitempty"`
	AgentCapabilities json.RawMessage `json:"agentCapabilities,omitempty"`
	Meta              json.RawMessage `json:"_meta,omitempty"`
}

// ModelState is initialize._meta.modelState, the fallback when session/new
// omits models.
func (r InitializeResult) ModelState() json.RawMessage {
	raw := bytes.TrimSpace(r.Meta)
	if len(raw) == 0 || raw[0] != '{' {
		return nil
	}
	var meta struct {
		ModelState json.RawMessage `json:"modelState"`
	}
	if err := json.Unmarshal(raw, &meta); err != nil {
		return nil
	}
	return bytes.TrimSpace(meta.ModelState)
}

// LoadSession is agentCapabilities.loadSession: whether the agent can reload a
// session by id and replay its transcript. Absent or malformed capabilities
// are a no — craze refuses the load rather than guessing.
func (r InitializeResult) LoadSession() bool {
	raw := bytes.TrimSpace(r.AgentCapabilities)
	if len(raw) == 0 || raw[0] != '{' {
		return false
	}
	var caps struct {
		LoadSession bool `json:"loadSession"`
	}
	if err := json.Unmarshal(raw, &caps); err != nil {
		return false
	}
	return caps.LoadSession
}

type AuthMethod struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (r InitializeResult) OffersCursorLogin() bool {
	return r.OffersAuthMethod(AuthCursorLogin)
}

// OffersAuthMethod reports whether initialize advertised an auth method id.
func (r InitializeResult) OffersAuthMethod(id string) bool {
	for _, m := range r.AuthMethods {
		if m.ID == id {
			return true
		}
	}
	return false
}

type AuthenticateParams struct {
	MethodID string `json:"methodId"`
	// Meta carries provider extras; grok authenticates with headless true.
	Meta map[string]any `json:"_meta,omitempty"`
}

type NewSessionParams struct {
	CWD        string `json:"cwd"`
	MCPServers []any  `json:"mcpServers"`
}

type NewSessionResult struct {
	SessionID     string          `json:"sessionId"`
	Models        json.RawMessage `json:"models,omitempty"`
	Modes         json.RawMessage `json:"modes,omitempty"`
	ConfigOptions json.RawMessage `json:"configOptions,omitempty"`
}

// LoadSessionParams is session/load: NewSessionParams plus the id, which is
// the shape all three agents were probed against. The result comes back in the
// NewSessionResult shape (models/modes/configOptions, each optional — cursor
// answers {modes, models}, grok and gx answer with models alone), with
// sessionId filled in by the client because the wire does not echo it.
type LoadSessionParams struct {
	SessionID  string `json:"sessionId"`
	CWD        string `json:"cwd"`
	MCPServers []any  `json:"mcpServers"`
}

// loadSessionTimeout is the session/load deadline. All three probed agents
// answer after the replay — under a second for a 172-event session — so this
// is only a backstop against an agent that leaves the RPC pending. 90 s is
// t3code's default, kept because the probes give no reason to shorten it.
const loadSessionTimeout = 90 * time.Second

// loadTimeoutOverride is the test hook for that deadline, in nanoseconds; zero
// means loadSessionTimeout. An atomic rather than a plain var because the
// tests that shorten it live in other packages (internal/agent) and run under
// -race beside a live read loop.
var loadTimeoutOverride atomic.Int64

// SetLoadSessionTimeout shortens the session/load deadline and returns the
// function that restores the previous value. It exists for tests; no
// production path calls it.
func SetLoadSessionTimeout(d time.Duration) func() {
	prev := loadTimeoutOverride.Swap(int64(d))
	return func() { loadTimeoutOverride.Store(prev) }
}

func loadSessionDeadline() time.Duration {
	if d := loadTimeoutOverride.Load(); d > 0 {
		return time.Duration(d)
	}
	return loadSessionTimeout
}

type ContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

type PromptParams struct {
	SessionID string         `json:"sessionId"`
	Prompt    []ContentBlock `json:"prompt"`
}

type PromptResult struct {
	StopReason string `json:"stopReason"`
	// Meta is the agent's extra; grok stamps promptId on it, which is what
	// lets a late prompt_complete for this turn be told from the next one's.
	Meta json.RawMessage `json:"_meta,omitempty"`
}

// PromptID is _meta.promptId, or empty when the agent sends none.
func (r PromptResult) PromptID() string {
	raw := bytes.TrimSpace(r.Meta)
	if len(raw) == 0 || raw[0] != '{' {
		return ""
	}
	var meta struct {
		PromptID string `json:"promptId"`
	}
	if err := json.Unmarshal(raw, &meta); err != nil {
		return ""
	}
	return meta.PromptID
}

type CancelParams struct {
	SessionID string `json:"sessionId"`
}

type SetModelParams struct {
	SessionID string `json:"sessionId"`
	ModelID   string `json:"modelId"`
}

type SetModeParams struct {
	SessionID string `json:"sessionId"`
	ModeID    string `json:"modeId"`
}

// SetConfigParams is the string variant of session/set_config_option.
// It must not include a type field (boolean variant is out this cut).
type SetConfigParams struct {
	SessionID string `json:"sessionId"`
	ConfigID  string `json:"configId"`
	Value     string `json:"value"`
}

// ConfigCatalog is the configOptions field of a settings reply with its
// presence kept, because the three shapes mean three different things (plan
// 025 design 1, panel astra 14):
//
//   - the key absent, or null: the reply carries no catalog — Present is false
//     and the catalog the client already has stands;
//   - an array, the empty one included: the agent's whole catalog as it now
//     is — Present is true and Options is that array verbatim, so `[]` clears;
//   - anything else is not a catalog at all, and the call that answered it is
//     an error (ErrBadCatalog) rather than a reply with nothing in it.
//
// ACP's SetSessionConfigOptionResponse is {configOptions}: cursor answers
// set_config_option with the current model's whole catalog, and set_model with
// {}. The options stay raw here; parsing them is internal/agent's, beside the
// parser every other catalog goes through.
type ConfigCatalog struct {
	Present bool
	Options json.RawMessage
}

// SettingsReply is one successful settings reply, as the read loop hands it to
// the client's settings handler (Client.SetSettingsHandler): the call it
// answers, what that call asked for, and the catalog it carried.
type SettingsReply struct {
	// Method is MethodSessionSetConfig or MethodSessionSetModel.
	Method string
	// ConfigID is the option set_config_option set, "" for set_model.
	ConfigID string
	// Value is what was asked for: the option's new value, or the model
	// set_model moved to.
	Value   string
	Catalog ConfigCatalog
}

type ToolCallLocation struct {
	Path string `json:"path"`
	Line int    `json:"line,omitempty"`
}

// ToolContent is one tool_call content[] item. Nested Content is a
// ContentBlock (type/text); non-text nested types are ignored by the agent.
// A "diff" item carries path/oldText/newText instead of a nested block.
type ToolContent struct {
	Type    string        `json:"type"`
	Content *ContentBlock `json:"content,omitempty"`
	Path    string        `json:"path,omitempty"`
	OldText string        `json:"oldText,omitempty"`
	NewText string        `json:"newText,omitempty"`
}

type SessionNotification struct {
	SessionID string          `json:"sessionId"`
	Update    json.RawMessage `json:"update"`
	// Child is the child session id for a routed child update, "" for the
	// active session. The session routes on Child, never on a session id of
	// its own, so the NewSession pendingUpdates flush cannot race Start.
	Child string `json:"-"`
	// ToolName is the dialect-normalized tool name: grok reads it from
	// update._meta["x.ai/tool"].name, cursor leaves it "" and the session
	// keeps reading rawInput._toolName.
	ToolName string `json:"-"`
}

type SessionUpdate struct {
	SessionUpdate     string             `json:"sessionUpdate"`
	Content           *ContentBlock      `json:"content,omitempty"`
	ToolCallID        string             `json:"toolCallId,omitempty"`
	Title             string             `json:"title,omitempty"`
	Kind              string             `json:"kind,omitempty"`
	Status            string             `json:"status,omitempty"`
	AvailableCommands []AvailableCommand `json:"availableCommands,omitempty"`
	CurrentModeID     string             `json:"currentModeId,omitempty"`
}

// Subagent kinds carried on x.ai/session_notification.
const (
	SubagentSpawned  = "subagent_spawned"
	SubagentProgress = "subagent_progress"
	SubagentFinished = "subagent_finished"
)

// SubagentNotification is one parsed subagent lifecycle event. SessionID is
// the outer session id the notification arrived on; ChildSessionID is the
// child it is about.
type SubagentNotification struct {
	SessionID       string
	Kind            string
	SubagentID      string
	AttemptID       string
	ChildSessionID  string
	ParentSessionID string
	SubagentType    string
	Description     string
	Model           string
	Role            string
	CapabilityMode  string
	ResumedFrom     string
	Status          string
	Error           string
	Output          string
	DurationMs      int
	ToolCalls       int
	Turns           int
	TokensUsed      int
	ToolsUsed       []string
	WillWake        bool
}

type AvailableCommand struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

type PermissionOption struct {
	OptionID string `json:"optionId"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
}

type ToolCall struct {
	ToolCallID string `json:"toolCallId"`
	Title      string `json:"title,omitempty"`
	Kind       string `json:"kind,omitempty"`
	Status     string `json:"status,omitempty"`
}

type PermissionRequest struct {
	SessionID string             `json:"sessionId"`
	Options   []PermissionOption `json:"options"`
	ToolCall  ToolCall           `json:"toolCall"`
}

type PermissionDecision struct {
	Cancelled bool
	OptionID  string
	// Replied, when set, is called exactly once with what became of the reply
	// this decision asked for: see ReplyDisposition.
	Replied func(ReplyDisposition)
}

type AskQuestionRequest struct {
	ToolCallID string        `json:"toolCallId"`
	Title      string        `json:"title,omitempty"`
	Questions  []AskQuestion `json:"questions"`
}

type AskQuestion struct {
	ID            string      `json:"id"`
	Prompt        string      `json:"prompt"`
	Options       []AskOption `json:"options"`
	AllowMultiple bool        `json:"allowMultiple,omitempty"`
}

type AskOption struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// AskDecision answers a cursor/ask_question request. Answers maps question id
// to the option ids picked from that question; ids never come from craze.
type AskDecision struct {
	Cancelled bool
	Skip      bool
	Answers   map[string][]string
	// Replied, when set, is called exactly once with what became of the reply
	// this decision asked for: see ReplyDisposition.
	Replied func(ReplyDisposition)
}

// CreatePlanRequest is parsed leniently: cursor sends name or title, and plan
// is usually a markdown string but may be an object.
type CreatePlanRequest struct {
	Name     string          `json:"name,omitempty"`
	Title    string          `json:"title,omitempty"`
	Overview string          `json:"overview,omitempty"`
	Plan     json.RawMessage `json:"plan,omitempty"`
	Todos    []TodoItem      `json:"todos,omitempty"`
}

// DisplayName prefers name and falls back to title.
func (r CreatePlanRequest) DisplayName() string {
	if r.Name != "" {
		return r.Name
	}
	return r.Title
}

// PlanText renders the plan field: a JSON string as-is, anything else through
// fmt.Sprint of the decoded value.
func (r CreatePlanRequest) PlanText() string {
	raw := bytes.TrimSpace(r.Plan)
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	return fmt.Sprint(v)
}

// PlanDecision answers a cursor/create_plan request.
type PlanDecision struct {
	Cancelled bool
	Accept    bool
	// Replied, when set, is called exactly once with what became of the reply
	// this decision asked for: see ReplyDisposition.
	Replied func(ReplyDisposition)
}

// ReplyDisposition is what became of one reply to a blocking agent request.
// The client answers each request exactly once, and the cancelled answer a
// cancel or a close writes while a handler is still deciding can win that race,
// so a handler that took a decision has no way of its own to know whether the
// agent ever heard it (plan 021 §3.6). Each decision type
// carries an optional Replied hook that is handed one of these.
type ReplyDisposition struct {
	// Delivered says this reply was the one written to the agent, and the
	// write returned no error.
	Delivered bool
	// Lost says why the decision never reached the agent, and is empty when it
	// did. The reasons are the ReplyLost constants.
	Lost string
}

// Why a decision never reached the agent, on ReplyDisposition.Lost.
const (
	// ReplyLostCancelled: a Cancel or CancelHeld had already answered the
	// request cancelled.
	ReplyLostCancelled = "cancelled"
	// ReplyLostClosed: Close had already answered the request cancelled.
	ReplyLostClosed = "closed"
	// ReplyLostWriteFailed: this reply was the one to write and the write
	// failed. The error itself is dropped, exactly as it always has been; only
	// the fact is reported.
	ReplyLostWriteFailed = "write_failed"
	// ReplyLostInvalidOption: the decision named an option the request never
	// offered, so the client replaced it with the cancelled outcome. The reply
	// reached the agent; the decision did not.
	ReplyLostInvalidOption = "invalid_option"
)

// RequestParams is one blocking agent request as it was decoded: exactly one
// field is set, and the method it arrived on says which. It travels with the
// registered request so a path that answers one without ever running its
// handler can still say what was asked (EarlyAnswer).
type RequestParams struct {
	Permission *PermissionRequest
	Ask        *AskQuestionRequest
	Plan       *CreatePlanRequest
}

// Arrival is when a blocking agent request reached craze, captured on the read
// loop in the one critical section that registers it, and handed to whoever
// answers it — a handler, or the early-answer hook.
//
// Both fields are needed and neither implies the other, because the counter
// alone cannot tell a retired turn's request from one that belongs to no turn
// at all. PromptBlocks bumps Turn when it accepts a prompt and clears InTurn
// when that prompt returns, without touching the counter: so a request
// registered *during* turn 1 whose handler goroutine the runtime delayed past
// the end of turn 1, and a request registered *between* turns after turn 1
// finished, both carry Turn == 1 and both pass TurnLive. Only InTurn tells them
// apart, and they deserve opposite answers — the first belongs to a turn that
// is over, the second to no turn of craze's own and may still be answered.
type Arrival struct {
	// Turn is the client's turn counter at the moment the request was
	// registered: what TurnLive compares against, and what decides whether a
	// later prompt has since made this request stale.
	Turn int
	// InTurn says a prompt of craze's own was in flight when the request was
	// registered, so the request belongs to that turn. False means it arrived
	// between craze's turns, or during a turn the agent started itself.
	InTurn bool
	// Call is this request's own lifetime, as a context: it ends the moment
	// something other than this handler answers the request — a Cancel, a
	// CancelHeld, a Close, the stale-turn reply — and again when the handler
	// returns. It is what a handler hands to whatever parks a decision on its
	// behalf, so an ask nobody can answer any more resolves at once instead of
	// waiting for a turn's end that may never come (plan 021 §3.6).
	//
	// Nothing about the wire changes with it: the client answers each request
	// exactly once, as it always did, and this only tells the handler that the
	// answer was not its own.
	//
	// It is nil on an Arrival built by hand — a test calling a handler
	// directly, or EarlyAnswer.Arrival, where the request is answered already —
	// and a caller treats nil as "no signal of its own".
	Call context.Context
}

// Ended reports whether a.Call has ended: the request has been answered by
// something other than its handler, so anything the handler is still deciding
// is moot. An Arrival with no context of its own is never ended.
func (a Arrival) Ended() bool {
	if a.Call == nil {
		return false
	}
	return a.Call.Err() != nil
}

// EarlyAnswerReason says why a blocking request was answered before any handler
// of the client's ran.
type EarlyAnswerReason string

const (
	// EarlyCancelled: a Cancel or CancelHeld answered it where it lay.
	EarlyCancelled EarlyAnswerReason = "cancelled"
	// EarlyClosed: Close answered it on the way down.
	EarlyClosed EarlyAnswerReason = "closed"
	// EarlyStaleTurn: nothing had answered it, but the turn it arrived in was
	// over by the time its handler goroutine ran, so the client answered it
	// cancelled itself rather than let a card be raised for a turn that has
	// ended.
	EarlyStaleTurn EarlyAnswerReason = "stale_turn"
)

// EarlyAnswer is one blocking request the client answered by itself, before any
// handler ran. Nothing above the client sees such a request otherwise — no
// handler runs, so nothing parks and nothing is published — and the agent's
// question then leaves no trace at all (plan 021 §2.3, §3.6).
// The decoded parameters travel with it so the session can write one
// self-contained record of what was asked and what became of it, without ever
// raising a card nobody could answer.
type EarlyAnswer struct {
	// Reason is who answered it: see the EarlyAnswerReason constants.
	Reason EarlyAnswerReason
	// Turn is the turn the request arrived in (Client.TurnLive), and InTurn
	// whether a prompt of craze's own was running then: together they are the
	// request's Arrival, spelled out here so that a caller reading Turn alone
	// keeps compiling.
	Turn   int
	InTurn bool
	// Params is the request itself, as decoded.
	Params RequestParams
}

// Arrival is the request's arrival as one value.
func (a EarlyAnswer) Arrival() Arrival { return Arrival{Turn: a.Turn, InTurn: a.InTurn} }

// TodoItem is one entry of a cursor/update_todos list, and of a plan's todos.
type TodoItem struct {
	ID      string `json:"id"`
	Content string `json:"content"`
	Status  string `json:"status"`
}

// UpdateTodosRequest arrives either as a JSON-RPC request or a notification.
// merge=false replaces the list, merge=true upserts by id.
type UpdateTodosRequest struct {
	ToolCallID string     `json:"toolCallId,omitempty"`
	Todos      []TodoItem `json:"todos"`
	Merge      bool       `json:"merge,omitempty"`
}

// TaskRequest is the sub-agent receipt cursor sends after it has already run
// the sub-agent itself; it arrives as a request or a notification.
type TaskRequest struct {
	ToolCallID   string          `json:"toolCallId,omitempty"`
	Description  string          `json:"description,omitempty"`
	Prompt       string          `json:"prompt,omitempty"`
	SubagentType json.RawMessage `json:"subagentType,omitempty"`
	Model        string          `json:"model,omitempty"`
	AgentID      string          `json:"agentId,omitempty"`
	DurationMs   int             `json:"durationMs,omitempty"`
}

// ErrBadParams is returned when an agent→client request carries params craze
// refuses to act on: JSON-RPC null and non-objects decode without error into a
// zero value, which would silently mutate state.
var ErrBadParams = errors.New("acp: params must be a JSON object")

// ErrUnsupported is what a dialect that has no such method returns before
// anything is written to the wire.
var ErrUnsupported = errors.New("acp: not supported by this agent")

// ErrForeignTurn refuses a prompt while the agent is running a turn craze did
// not start (grok's interject fallback). One prompt at a time is a session
// rule, not a client rule, so the guard belongs here as well.
var ErrForeignTurn = errors.New("acp: agent is running a turn of its own")

// ErrNoSession is a session-scoped call made before session/new.
var ErrNoSession = errors.New("acp: no session")

// InterjectParams is x.ai/interject. content is not sent: no images this cut.
type InterjectParams struct {
	SessionID      string `json:"sessionId"`
	Text           string `json:"text"`
	InterjectionID string `json:"interjectionId,omitempty"`
}

// interjectAck is the ack, nested one level under the JSON-RPC result:
// {"result":{"result":{"status":"queued"}}} on the wire.
type interjectAck struct {
	Result struct {
		Status string `json:"status"`
	} `json:"result"`
}

// InterjectionNotification is x.ai/session/interjection, the broadcast the
// user block is rendered from. InterjectionID is echoed back when the client
// sent one, and is empty otherwise.
type InterjectionNotification struct {
	SessionID      string `json:"sessionId"`
	Text           string `json:"text"`
	InterjectionID string `json:"interjectionId"`
}

// QueueEntry is one row of grok's own queue. craze never puts anything in it;
// the entries are read only to learn which promptId its prompt was given.
type QueueEntry struct {
	ID       string `json:"id"`
	Version  int    `json:"version"`
	Kind     string `json:"kind"`
	Text     string `json:"text"`
	Position int    `json:"position"`
}

// QueueChanged is x.ai/queue/changed. It is broadcast on every queue change
// and at turn start, where RunningPromptID names the turn now running.
type QueueChanged struct {
	SessionID       string       `json:"sessionId"`
	Entries         []QueueEntry `json:"entries"`
	RunningPromptID string       `json:"runningPromptId"`
	RunningText     string       `json:"runningText"`
	RunningKind     string       `json:"runningKind"`
}

// ForeignTurn is a turn the agent started on craze's session without a craze
// prompt. Running is true on the start sighting and false at its end.
type ForeignTurn struct {
	ID      string
	Text    string
	Running bool
}

// IsInterjectFallback reports whether a promptId is one grok minted for a
// stranded interjection.
func IsInterjectFallback(promptID string) bool {
	return strings.HasPrefix(promptID, InterjectFallbackPrefix)
}

func decodeObject(raw json.RawMessage, v any) error {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '{' {
		return ErrBadParams
	}
	return json.Unmarshal(raw, v)
}

// parseUpdateTodos requires a real todos array: a null or missing list would
// otherwise read as merge=false with nothing in it and wipe the todo list.
func parseUpdateTodos(params json.RawMessage) (UpdateTodosRequest, error) {
	var w struct {
		ToolCallID string          `json:"toolCallId"`
		Todos      json.RawMessage `json:"todos"`
		Merge      bool            `json:"merge"`
	}
	if err := decodeObject(params, &w); err != nil {
		return UpdateTodosRequest{}, err
	}
	todos := bytes.TrimSpace(w.Todos)
	if len(todos) == 0 || todos[0] != '[' {
		return UpdateTodosRequest{}, ErrBadParams
	}
	out := UpdateTodosRequest{ToolCallID: w.ToolCallID, Merge: w.Merge}
	if err := json.Unmarshal(todos, &out.Todos); err != nil {
		return UpdateTodosRequest{}, err
	}
	return out, nil
}

// parseTaskRequest requires the toolCallId the receipt has to be joined on.
func parseTaskRequest(params json.RawMessage) (TaskRequest, error) {
	var req TaskRequest
	if err := decodeObject(params, &req); err != nil {
		return TaskRequest{}, err
	}
	if req.ToolCallID == "" {
		return TaskRequest{}, ErrBadParams
	}
	return req, nil
}
