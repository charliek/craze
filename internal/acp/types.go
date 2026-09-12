package acp

import "encoding/json"

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

	UpdateAgentMessage      = "agent_message_chunk"
	UpdateAgentThought      = "agent_thought_chunk"
	UpdateToolCall          = "tool_call"
	UpdateToolCallUpd       = "tool_call_update"
	UpdateAvailableCommands = "available_commands_update"
	UpdateCurrentMode       = "current_mode_update"

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
	SessionID string          `json:"sessionId"`
	Models    json.RawMessage `json:"models,omitempty"`
	Modes     json.RawMessage `json:"modes,omitempty"`
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
