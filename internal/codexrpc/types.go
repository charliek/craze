package codexrpc

import "encoding/json"

// JSON-RPC method constants for Codex app-server v2.
const (
	MethodInitialize            = "initialize"
	MethodInitialized           = "initialized"
	MethodAccountRead           = "account/read"
	MethodAccountRateLimitsRead = "account/rateLimits/read"
	MethodModelList             = "model/list"
	MethodSkillsList            = "skills/list"
	MethodCollaborationModeList = "collaborationMode/list"
	MethodThreadStart           = "thread/start"
	MethodThreadResume          = "thread/resume"
	MethodThreadTurnsList       = "thread/turns/list"
	MethodThreadItemsList       = "thread/items/list"
	MethodThreadNameSet         = "thread/name/set"
	MethodTurnStart             = "turn/start"
	MethodTurnInterrupt         = "turn/interrupt"
	MethodTurnSteer             = "turn/steer"

	// Server -> client notifications.
	NotifyThreadStarted           = "thread/started"
	NotifyThreadStatusChanged     = "thread/status/changed"
	NotifyThreadNameUpdated       = "thread/name/updated"
	NotifyThreadTokenUsageUpdated = "thread/tokenUsage/updated"
	NotifyTurnStarted             = "turn/started"
	NotifyTurnCompleted           = "turn/completed"
	NotifyTurnPlanUpdated         = "turn/plan/updated"
	NotifyTurnDiffUpdated         = "turn/diff/updated"
	NotifyItemStarted             = "item/started"
	NotifyItemCompleted           = "item/completed"
	NotifyAgentMessageDelta       = "item/agentMessage/delta"
	NotifyPlanDelta               = "item/plan/delta"
	NotifyReasoningSummaryDelta   = "item/reasoning/summaryTextDelta"
	NotifyReasoningSummaryPart    = "item/reasoning/summaryPartAdded"
	NotifyReasoningTextDelta      = "item/reasoning/textDelta"
	NotifyCommandOutputDelta      = "item/commandExecution/outputDelta"
	NotifyFileChangeOutputDelta   = "item/fileChange/outputDelta"
	NotifySubAgentActivity        = "subAgentActivity"
	NotifyAccountUpdated          = "account/updated"
	NotifyRateLimitsUpdated       = "account/rateLimits/updated"
	NotifySkillsChanged           = "skills/changed"
	NotifyServerRequestResolved   = "serverRequest/resolved"
	NotifyError                   = "error"
	NotifyConfigWarning           = "configWarning"
	NotifyDeprecationNotice       = "deprecationNotice"

	// Server -> client requests.
	RequestCommandApproval     = "item/commandExecution/requestApproval"
	RequestFileChangeApproval  = "item/fileChange/requestApproval"
	RequestPermissionsApproval = "item/permissions/requestApproval"
	RequestUserInput           = "item/tool/requestUserInput"
	RequestMCPElicitation      = "mcpServer/elicitation/request"
)

// Message is one NDJSON frame on the codex app-server stdio transport.
// Server->client frames omit `"jsonrpc":"2.0"` and may use integer ID 0, so ID
// is a pointer to json.RawMessage: nil means absent (notification), non-nil
// preserves the exact JSON token (including `0` or `"abc"`).
type Message struct {
	JSONRPC string           `json:"jsonrpc,omitempty"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method,omitempty"`
	Params  json.RawMessage  `json:"params,omitempty"`
	Result  json.RawMessage  `json:"result,omitempty"`
	Error   *RPCError        `json:"error,omitempty"`
}

func (m *Message) IsNotification() bool {
	return m != nil && m.Method != "" && m.ID == nil
}

func (m *Message) IsRequest() bool {
	return m != nil && m.Method != "" && m.ID != nil
}

func (m *Message) IsResponse() bool {
	return m != nil && m.Method == "" && m.ID != nil
}

// --- Handshake ---

type ClientInfo struct {
	Name    string `json:"name"`
	Title   string `json:"title,omitempty"`
	Version string `json:"version"`
}

type ClientCapabilities struct {
	ExperimentalAPI bool `json:"experimentalApi"`
}

type InitializeParams struct {
	ClientInfo   ClientInfo         `json:"clientInfo"`
	Capabilities ClientCapabilities `json:"capabilities"`
}

type InitializeResult struct {
	UserAgent      string `json:"userAgent"`
	CodexHome      string `json:"codexHome"`
	PlatformFamily string `json:"platformFamily"`
	PlatformOS     string `json:"platformOs"`
}

// --- Account & Rate Limits ---

type AccountReadParams struct {
	RefreshToken bool `json:"refreshToken"`
}

type Account struct {
	Type          string `json:"type"`
	Email         string `json:"email,omitempty"`
	PlanType      string `json:"planType,omitempty"`
	WorkspaceName string `json:"workspaceName,omitempty"`
}

type AccountReadResult struct {
	Account            *Account `json:"account"`
	RequiresOpenaiAuth bool     `json:"requiresOpenaiAuth"`
}

type RateLimitWindow struct {
	UsedPercent        *float64 `json:"usedPercent,omitempty"`
	WindowDurationMins *int     `json:"windowDurationMins,omitempty"`
	ResetsAt           *int64   `json:"resetsAt,omitempty"`
}

type CreditsSnapshot struct {
	HasCredits bool   `json:"hasCredits"`
	Unlimited  bool   `json:"unlimited"`
	Balance    string `json:"balance,omitempty"`
}

type RateLimitsSnapshot struct {
	LimitID             string           `json:"limitId,omitempty"`
	LimitName           string           `json:"limitName,omitempty"`
	PlanType            string           `json:"planType,omitempty"`
	Primary             *RateLimitWindow `json:"primary,omitempty"`
	Secondary           *RateLimitWindow `json:"secondary,omitempty"`
	Credits             *CreditsSnapshot `json:"credits,omitempty"`
	SpendControlReached *bool            `json:"spendControlReached,omitempty"`
}

type RateLimitsReadResult struct {
	RateLimits          *RateLimitsSnapshot            `json:"rateLimits,omitempty"`
	RateLimitsByLimitID map[string]*RateLimitsSnapshot `json:"rateLimitsByLimitId,omitempty"`
}

type RateLimitsUpdatedParams struct {
	RateLimits *RateLimitsSnapshot `json:"rateLimits,omitempty"`
}

// --- Models ---

type ModelListParams struct {
	IncludeHidden bool   `json:"includeHidden,omitempty"`
	Cursor        string `json:"cursor,omitempty"`
	Limit         int    `json:"limit,omitempty"`
}

type ReasoningEffortOption struct {
	ReasoningEffort string `json:"reasoningEffort"`
	Description     string `json:"description,omitempty"`
}

type ModelEntry struct {
	ID                        string                  `json:"id"`
	Model                     string                  `json:"model"`
	DisplayName               string                  `json:"displayName"`
	Description               string                  `json:"description,omitempty"`
	IsDefault                 bool                    `json:"isDefault"`
	Hidden                    bool                    `json:"hidden"`
	DefaultReasoningEffort    string                  `json:"defaultReasoningEffort,omitempty"`
	SupportedReasoningEfforts []ReasoningEffortOption `json:"supportedReasoningEfforts,omitempty"`
}

type ModelListResult struct {
	Data       []ModelEntry `json:"data"`
	NextCursor *string      `json:"nextCursor,omitempty"`
}

// --- Skills ---

type SkillsListParams struct {
	Cwds        []string `json:"cwds,omitempty"`
	ForceReload bool     `json:"forceReload,omitempty"`
}

type SkillMetadata struct {
	Name             string `json:"name"`
	Description      string `json:"description,omitempty"`
	ShortDescription string `json:"shortDescription,omitempty"`
	Path             string `json:"path"`
	Scope            string `json:"scope,omitempty"`
	Enabled          bool   `json:"enabled"`
}

type SkillsListEntry struct {
	Cwd    string          `json:"cwd"`
	Skills []SkillMetadata `json:"skills"`
}

type SkillsListResult struct {
	Data []SkillsListEntry `json:"data"`
}

// --- Sandbox & Collaboration Mode ---

// SandboxMode is the kebab-case string accepted by `thread/start` and
// `thread/resume` (`"read-only"`, `"workspace-write"`, `"danger-full-access"`).
type SandboxMode string

const (
	SandboxReadOnly         SandboxMode = "read-only"
	SandboxWorkspaceWrite   SandboxMode = "workspace-write"
	SandboxDangerFullAccess SandboxMode = "danger-full-access"
)

// ApprovalPolicy is the approval policy string (`"never"`, `"on-request"`,
// `"on-failure"`, `"untrusted"`).
type ApprovalPolicy string

const (
	ApprovalNever     ApprovalPolicy = "never"
	ApprovalOnRequest ApprovalPolicy = "on-request"
)

// SandboxPolicy is the camelCase tagged JSON object accepted by `turn/start`
// and returned in `thread/start` / `thread/resume` (`sandbox` field).
type SandboxPolicy struct {
	Type                string   `json:"type"`
	WritableRoots       []string `json:"writableRoots,omitempty"`
	NetworkAccess       *bool    `json:"networkAccess,omitempty"`
	ExcludeTmpdirEnvVar *bool    `json:"excludeTmpdirEnvVar,omitempty"`
	ExcludeSlashTmp     *bool    `json:"excludeSlashTmp,omitempty"`
}

const (
	SandboxPolicyReadOnly       = "readOnly"
	SandboxPolicyWorkspaceWrite = "workspaceWrite"
)

// Clone returns a deep copy of p.
func (p *SandboxPolicy) Clone() *SandboxPolicy {
	if p == nil {
		return nil
	}
	cp := *p
	if p.WritableRoots != nil {
		cp.WritableRoots = append([]string(nil), p.WritableRoots...)
	}
	if p.NetworkAccess != nil {
		v := *p.NetworkAccess
		cp.NetworkAccess = &v
	}
	if p.ExcludeTmpdirEnvVar != nil {
		v := *p.ExcludeTmpdirEnvVar
		cp.ExcludeTmpdirEnvVar = &v
	}
	if p.ExcludeSlashTmp != nil {
		v := *p.ExcludeSlashTmp
		cp.ExcludeSlashTmp = &v
	}
	return &cp
}

// CollaborationSettings uses snake_case keys required by codex app-server v2.
// DeveloperInstructions omits `omitempty` so nil marshals as explicit `null`.
type CollaborationSettings struct {
	Model                 string  `json:"model"`
	ReasoningEffort       *string `json:"reasoning_effort,omitempty"`
	DeveloperInstructions *string `json:"developer_instructions"`
}

// CollaborationMode is sent on every `turn/start` so exiting `plan` mode never
// leaves a thread stuck in plan instructions.
type CollaborationMode struct {
	Mode     string                `json:"mode"`
	Settings CollaborationSettings `json:"settings"`
}

type CollaborationModeEntry struct {
	Name            string  `json:"name"`
	Mode            string  `json:"mode"`
	Model           *string `json:"model,omitempty"`
	ReasoningEffort *string `json:"reasoning_effort,omitempty"`
}

type CollaborationModeListResult struct {
	Data []CollaborationModeEntry `json:"data"`
}

// --- Threads & Turns ---

type ThreadStartParams struct {
	Cwd            string         `json:"cwd,omitempty"`
	Sandbox        SandboxMode    `json:"sandbox,omitempty"`
	ApprovalPolicy ApprovalPolicy `json:"approvalPolicy,omitempty"`
	Model          string         `json:"model,omitempty"`
	Ephemeral      *bool          `json:"ephemeral,omitempty"`
}

type Thread struct {
	ID              string  `json:"id"`
	Name            *string `json:"name,omitempty"`
	Cwd             string  `json:"cwd,omitempty"`
	Model           string  `json:"model,omitempty"`
	ReasoningEffort *string `json:"reasoningEffort,omitempty"`
	Preview         string  `json:"preview,omitempty"`
	Turns           []Turn  `json:"turns,omitempty"`
}

type ThreadStartResult struct {
	Thread          Thread         `json:"thread"`
	Model           string         `json:"model"`
	ModelProvider   string         `json:"modelProvider,omitempty"`
	ReasoningEffort *string        `json:"reasoningEffort,omitempty"`
	Sandbox         *SandboxPolicy `json:"sandbox,omitempty"`
	ApprovalPolicy  string         `json:"approvalPolicy,omitempty"`
}

type ThreadResumeParams struct {
	ThreadID       string         `json:"threadId"`
	ExcludeTurns   bool           `json:"excludeTurns,omitempty"`
	Cwd            string         `json:"cwd,omitempty"`
	Sandbox        SandboxMode    `json:"sandbox,omitempty"`
	ApprovalPolicy ApprovalPolicy `json:"approvalPolicy,omitempty"`
	Model          string         `json:"model,omitempty"`
}

type ThreadResumeResult struct {
	Thread               Thread             `json:"thread"`
	Model                string             `json:"model"`
	ModelProvider        string             `json:"modelProvider,omitempty"`
	ReasoningEffort      *string            `json:"reasoningEffort,omitempty"`
	Sandbox              *SandboxPolicy     `json:"sandbox,omitempty"`
	ApprovalPolicy       string             `json:"approvalPolicy,omitempty"`
	CollaborationMode    *CollaborationMode `json:"collaborationMode,omitempty"`
	TurnsBackwardsCursor *string            `json:"turnsBackwardsCursor,omitempty"`
	ItemsBackwardsCursor *string            `json:"itemsBackwardsCursor,omitempty"`
}

type ThreadTurnsListParams struct {
	ThreadID      string `json:"threadId"`
	SortDirection string `json:"sortDirection,omitempty"`
	ItemsView     string `json:"itemsView,omitempty"`
	Limit         int    `json:"limit,omitempty"`
	Cursor        string `json:"cursor,omitempty"`
}

type ThreadTurnsListResult struct {
	Data            []Turn  `json:"data"`
	NextCursor      *string `json:"nextCursor,omitempty"`
	BackwardsCursor *string `json:"backwardsCursor,omitempty"`
}

type ThreadItemsListParams struct {
	ThreadID string `json:"threadId"`
	Limit    int    `json:"limit,omitempty"`
	Cursor   string `json:"cursor,omitempty"`
}

type ThreadItemEntry struct {
	TurnID        string          `json:"turnId"`
	Item          json.RawMessage `json:"item"`
	StartedAtMs   *int64          `json:"startedAtMs,omitempty"`
	CompletedAtMs *int64          `json:"completedAtMs,omitempty"`
}

type ThreadItemsListResult struct {
	Data            []ThreadItemEntry `json:"data"`
	NextCursor      *string           `json:"nextCursor,omitempty"`
	BackwardsCursor *string           `json:"backwardsCursor,omitempty"`
}

type ThreadNameSetParams struct {
	ThreadID string `json:"threadId"`
	Name     string `json:"name"`
}

type UserInput struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
	Path string `json:"path,omitempty"`
	Name string `json:"name,omitempty"`
}

type TurnStartParams struct {
	ThreadID          string             `json:"threadId"`
	Input             []UserInput        `json:"input"`
	Model             string             `json:"model,omitempty"`
	Effort            string             `json:"effort,omitempty"`
	Summary           string             `json:"summary,omitempty"`
	SandboxPolicy     *SandboxPolicy     `json:"sandboxPolicy,omitempty"`
	ApprovalPolicy    ApprovalPolicy     `json:"approvalPolicy,omitempty"`
	CollaborationMode *CollaborationMode `json:"collaborationMode,omitempty"`
}

type Turn struct {
	ID        string            `json:"id"`
	Status    string            `json:"status"`
	Items     []json.RawMessage `json:"items,omitempty"`
	ItemsView string            `json:"itemsView,omitempty"`
	Error     *TurnError        `json:"error,omitempty"`
}

type TurnStartResult struct {
	Turn Turn `json:"turn"`
}

type TurnInterruptParams struct {
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId"`
}

type TurnSteerParams struct {
	ThreadID       string      `json:"threadId"`
	ExpectedTurnID string      `json:"expectedTurnId"`
	Input          []UserInput `json:"input"`
}

type TurnSteerResult struct {
	TurnID string `json:"turnId"`
}

// --- Notifications ---

type TurnEventParams struct {
	ThreadID string `json:"threadId"`
	Turn     Turn   `json:"turn"`
}

type ItemNotificationParams struct {
	ThreadID string          `json:"threadId"`
	TurnID   string          `json:"turnId"`
	Item     json.RawMessage `json:"item"`
}

type ItemDeltaParams struct {
	ThreadID     string `json:"threadId"`
	TurnID       string `json:"turnId"`
	ItemID       string `json:"itemId"`
	Delta        string `json:"delta"`
	SummaryIndex int    `json:"summaryIndex,omitempty"`
}

type ReasoningSummaryPartParams struct {
	ThreadID     string `json:"threadId"`
	TurnID       string `json:"turnId"`
	ItemID       string `json:"itemId"`
	SummaryIndex int    `json:"summaryIndex"`
}

// ThreadItemHeader inspects the discriminator and ID of any item JSON object.
type ThreadItemHeader struct {
	Type   string `json:"type"`
	ID     string `json:"id"`
	Status string `json:"status,omitempty"`
}

type UserMessageItem struct {
	Type    string      `json:"type"`
	ID      string      `json:"id"`
	Content []UserInput `json:"content"`
}

type AgentMessageItem struct {
	Type  string `json:"type"`
	ID    string `json:"id"`
	Text  string `json:"text"`
	Phase string `json:"phase,omitempty"`
}

type PlanItem struct {
	Type string `json:"type"`
	ID   string `json:"id"`
	Text string `json:"text"`
}

type ReasoningItem struct {
	Type    string   `json:"type"`
	ID      string   `json:"id"`
	Summary []string `json:"summary,omitempty"`
	Content []string `json:"content,omitempty"`
}

type CommandExecutionItem struct {
	Type             string `json:"type"`
	ID               string `json:"id"`
	Command          string `json:"command"`
	Cwd              string `json:"cwd,omitempty"`
	Status           string `json:"status"`
	AggregatedOutput string `json:"aggregatedOutput,omitempty"`
	ExitCode         *int   `json:"exitCode,omitempty"`
	DurationMs       *int   `json:"durationMs,omitempty"`
}

type FileChangeKind struct {
	Type          string `json:"type"` // "add", "delete", "update"
	MovePath      string `json:"move_path,omitempty"`
	MovePathCamel string `json:"movePath,omitempty"`
}

// MoveTarget returns the rename destination path from either the upstream
// snake_case `move_path` field (codex-cli 0.172.0) or legacy `movePath`.
func (k FileChangeKind) MoveTarget() string {
	if k.MovePath != "" {
		return k.MovePath
	}
	return k.MovePathCamel
}

type FileUpdateChange struct {
	Path string         `json:"path"`
	Kind FileChangeKind `json:"kind"`
	Diff string         `json:"diff"`
}

type FileChangeItem struct {
	Type    string             `json:"type"`
	ID      string             `json:"id"`
	Changes []FileUpdateChange `json:"changes"`
	Status  string             `json:"status"`
}

type MCPToolCallItem struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	Server    string          `json:"server"`
	Tool      string          `json:"tool"`
	Status    string          `json:"status"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

type WebSearchItem struct {
	Type   string `json:"type"`
	ID     string `json:"id"`
	Query  string `json:"query"`
	Status string `json:"status,omitempty"`
}

type CollabAgentToolCallItem struct {
	Type             string          `json:"type"`
	ID               string          `json:"id"`
	Tool             string          `json:"tool,omitempty"`
	Status           string          `json:"status,omitempty"`
	SenderThreadID   string          `json:"senderThreadId,omitempty"`
	ReceiverThreadID []string        `json:"receiverThreadIds,omitempty"`
	Prompt           string          `json:"prompt,omitempty"`
	AgentsStates     json.RawMessage `json:"agentsStates,omitempty"`
}

type SubAgentActivityParams struct {
	ThreadID       string `json:"threadId"`
	ParentThreadID string `json:"parentThreadId,omitempty"`
	SubAgentID     string `json:"subAgentId,omitempty"`
	AgentID        string `json:"agentId,omitempty"`
	Title          string `json:"title,omitempty"`
	Status         string `json:"status,omitempty"`
	Activity       string `json:"activity,omitempty"`
	Output         string `json:"output,omitempty"`
	Error          string `json:"error,omitempty"`
}

type PlanStep struct {
	Step   string `json:"step"`
	Status string `json:"status"`
}

type TurnPlanUpdatedParams struct {
	ThreadID    string     `json:"threadId"`
	TurnID      string     `json:"turnId"`
	Explanation string     `json:"explanation,omitempty"`
	Plan        []PlanStep `json:"plan"`
}

type TokenUsageBreakdown struct {
	TotalTokens           int64 `json:"totalTokens"`
	InputTokens           int64 `json:"inputTokens"`
	CachedInputTokens     int64 `json:"cachedInputTokens"`
	CacheWriteInputTokens int64 `json:"cacheWriteInputTokens"`
	OutputTokens          int64 `json:"outputTokens"`
	ReasoningOutputTokens int64 `json:"reasoningOutputTokens"`
}

type ThreadTokenUsage struct {
	Total              TokenUsageBreakdown `json:"total"`
	Last               TokenUsageBreakdown `json:"last"`
	ModelContextWindow int64               `json:"modelContextWindow"`
}

type TokenUsageUpdatedParams struct {
	ThreadID   string           `json:"threadId"`
	TurnID     string           `json:"turnId"`
	TokenUsage ThreadTokenUsage `json:"tokenUsage"`
}

type ThreadNameUpdatedParams struct {
	ThreadID   string `json:"threadId"`
	ThreadName string `json:"threadName"`
}

type ErrorNotificationParams struct {
	ThreadID  string    `json:"threadId"`
	TurnID    string    `json:"turnId"`
	WillRetry bool      `json:"willRetry"`
	Error     TurnError `json:"error"`
}

type ConfigWarningParams struct {
	Summary string `json:"summary"`
	Details string `json:"details,omitempty"`
}

type DeprecationNoticeParams struct {
	Summary string `json:"summary"`
	Details string `json:"details,omitempty"`
}

type ServerRequestResolvedParams struct {
	ThreadID  string          `json:"threadId"`
	RequestID json.RawMessage `json:"requestId"`
}

// --- Server -> Client Requests ---

type CommandApprovalParams struct {
	ThreadID           string            `json:"threadId"`
	TurnID             string            `json:"turnId"`
	ItemID             string            `json:"itemId"`
	Reason             string            `json:"reason,omitempty"`
	Command            string            `json:"command,omitempty"`
	Cwd                string            `json:"cwd,omitempty"`
	AvailableDecisions []json.RawMessage `json:"availableDecisions,omitempty"`
}

type FileChangeApprovalParams struct {
	ThreadID  string `json:"threadId"`
	TurnID    string `json:"turnId"`
	ItemID    string `json:"itemId"`
	Reason    string `json:"reason,omitempty"`
	GrantRoot string `json:"grantRoot,omitempty"`
}

type PermissionsApprovalParams struct {
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId"`
	ItemID   string `json:"itemId"`
	Reason   string `json:"reason,omitempty"`
}

type ApprovalDecisionReply struct {
	Decision string `json:"decision"`
}

// PermissionsApprovalReply is the typed wire reply for
// `item/permissions/requestApproval` (codex-cli 0.172.0). An empty
// Permissions map explicitly denies the permission grant for the scope.
type PermissionsApprovalReply struct {
	Permissions map[string]any `json:"permissions"`
	Scope       string         `json:"scope"`
}

type UserInputOption struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

type UserInputQuestion struct {
	ID       string            `json:"id"`
	Header   string            `json:"header,omitempty"`
	Question string            `json:"question"`
	IsOther  bool              `json:"isOther"`
	IsSecret bool              `json:"isSecret"`
	Options  []UserInputOption `json:"options"`
}

type RequestUserInputParams struct {
	ThreadID  string              `json:"threadId"`
	TurnID    string              `json:"turnId"`
	ItemID    string              `json:"itemId"`
	Questions []UserInputQuestion `json:"questions"`
}

type UserInputAnswer struct {
	Answers []string `json:"answers"`
}

type RequestUserInputReply struct {
	Answers map[string]UserInputAnswer `json:"answers"`
}
