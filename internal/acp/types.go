package acp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

const (
	ProtocolVersion = 1
	AuthCursorLogin = "cursor_login"

	MethodInitialize        = "initialize"
	MethodAuthenticate      = "authenticate"
	MethodSessionNew        = "session/new"
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

	UpdateAgentMessage      = "agent_message_chunk"
	UpdateAgentThought      = "agent_thought_chunk"
	UpdateToolCall          = "tool_call"
	UpdateToolCallUpd       = "tool_call_update"
	UpdateAvailableCommands = "available_commands_update"
	UpdateCurrentMode       = "current_mode_update"
	UpdateConfigOption      = "config_option_update"
	UpdateSessionInfo       = "session_info_update"

	KindAllowOnce    = "allow_once"
	KindAllowAlways  = "allow_always"
	KindRejectOnce   = "reject_once"
	KindRejectAlways = "reject_always"

	StopEndTurn   = "end_turn"
	StopCancelled = "cancelled"
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
}

type AuthMethod struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (r InitializeResult) OffersCursorLogin() bool {
	for _, m := range r.AuthMethods {
		if m.ID == AuthCursorLogin {
			return true
		}
	}
	return false
}

type AuthenticateParams struct {
	MethodID string `json:"methodId"`
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
}

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
