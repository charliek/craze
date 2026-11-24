package acp

import (
	"context"
	"encoding/json"
	"strings"
)

// The claude dialect's own wire (plan 052 § A4, § B1): the adapter's
// auth-status pushes and the tool name it stamps on every tool update.

const (
	// MethodClaudeAuthStatus is claude-agent-acp's _auth/status_update, sent
	// whenever the login it would bill a turn to changes: once after
	// initialize (its own `claude auth status --json` probe), and again when a
	// session's account says more. It carries no sessionId.
	MethodClaudeAuthStatus = "_auth/status_update"
	// MethodAuthStatusUpdate is ACP's auth/status_update, the adapter's twin of
	// the push above in the protocol's {state, accounts} shape. Every change it
	// reports went out as MethodClaudeAuthStatus first, with more in it, so
	// craze reads that one and ignores this.
	MethodAuthStatusUpdate = "auth/status_update"
	// MethodElicitationCreate is the RFD form request AskUserQuestion uses. It
	// reaches a client only once that client declares elicitation.form, which
	// the claude dialect alone does (plan 052 § B2); every other dialect
	// refuses it.
	MethodElicitationCreate = "elicitation/create"
)

// The auth-status kinds the adapter reports (auth-status.js): only an account
// login — a claude.ai subscription — is what craze runs claude on.
const (
	AuthKindAccount = "account"
	AuthKindAPIKey  = "api_key"
	AuthKindNone    = "none"
)

// AuthStatus is one _auth/status_update's authStatus. Only what the login gate
// judges is kept: the account's email and organisation are never read.
type AuthStatus struct {
	Kind   string `json:"kind"`
	Label  string `json:"label,omitempty"`
	Detail string `json:"detail,omitempty"`
	// Plan is authStatus.account.plan: the subscription's raw name ("max",
	// or a label such as "Claude Ultra"), "" when there is no account.
	Plan string `json:"-"`
}

// parseAuthStatus reads an _auth/status_update's params. A payload with no
// authStatus object, or one with no kind, is not a status at all.
func parseAuthStatus(params json.RawMessage) (AuthStatus, bool) {
	var w struct {
		AuthStatus *struct {
			Kind    string `json:"kind"`
			Label   string `json:"label"`
			Detail  string `json:"detail"`
			Account *struct {
				Plan string `json:"plan"`
			} `json:"account"`
		} `json:"authStatus"`
	}
	if decodeObject(params, &w) != nil || w.AuthStatus == nil || strings.TrimSpace(w.AuthStatus.Kind) == "" {
		return AuthStatus{}, false
	}
	st := AuthStatus{Kind: w.AuthStatus.Kind, Label: w.AuthStatus.Label, Detail: w.AuthStatus.Detail}
	if w.AuthStatus.Account != nil {
		st.Plan = w.AuthStatus.Account.Plan
	}
	return st, true
}

// SetAuthStatusHandler installs the handler for the claude dialect's auth
// pushes (MethodClaudeAuthStatus). It runs on the read goroutine in wire
// order, must not block and must not call back into this client's wire. It is
// installed before initialize is written, so no push can precede it.
func (c *Client) SetAuthStatusHandler(h func(AuthStatus)) {
	c.mu.Lock()
	c.authHandler = h
	c.mu.Unlock()
}

func (c *Client) handleAuthStatus(msg *Message) {
	st, ok := parseAuthStatus(msg.Params)
	if !ok {
		return
	}
	c.mu.Lock()
	h := c.authHandler
	c.mu.Unlock()
	if h != nil {
		h(st)
	}
}

// SetPromptGate installs the caller's last check of a prompt (plan 052 review
// C2b 2): it runs in PromptBlocks and ResendBlocks once the prompt is admitted
// and its accepted hook has run, immediately before the bytes are written, on
// the prompting goroutine with no client lock held. It may wait; an error it
// returns is the call's, and the prompt is never written. The claude session
// holds a prompt there while its fast mode is being reset, and refuses one
// once the session has stopped.
func (c *Client) SetPromptGate(g func(ctx context.Context) error) {
	c.mu.Lock()
	c.promptGate = g
	c.mu.Unlock()
}

// Halt stops the client writing, at once and from any goroutine, the read
// loop included: the agent's stdin is closed, so a write in flight or any
// later one fails, while what the agent has already written is still read.
// It is what a session stopping for a reason of its own does before it closes
// the client, which is the rest of the shutdown.
func (c *Client) Halt() {
	c.conn.closeWriter()
}

// claudeToolName is the tool's own name the adapter stamps on every tool_call
// and tool_call_update, _meta.claudeCode.toolName ("Write", "Bash", "Agent").
func claudeToolName(update json.RawMessage) string {
	var w struct {
		Meta struct {
			ClaudeCode struct {
				ToolName string `json:"toolName"`
			} `json:"claudeCode"`
		} `json:"_meta"`
	}
	if err := json.Unmarshal(update, &w); err != nil {
		return ""
	}
	return w.Meta.ClaudeCode.ToolName
}

// decision is what a dialect does with a method it may receive (plan 052 §
// B1). The zero value is no decision at all, which the dispatch test refuses.
type decision int

const (
	decisionNone decision = iota
	// decisionHandle: the client acts on it.
	decisionHandle
	// decisionIgnore: a notification the client reads and drops.
	decisionIgnore
	// decisionRefuse: a request answered -32601, as for a method nobody
	// knows; a notification of it is dropped.
	decisionRefuse
)

// updateMethod is how a session/update kind is named in the dispatch table:
// the method and the kind, so the table has one row per thing that arrives.
func updateMethod(kind string) string { return MethodSessionUpdate + "#" + kind }

// The session/update kinds craze knows of besides those it names elsewhere:
// claude-agent-acp's own (evidence 05) — its subagent_update is SubagentUpdate
// (types.go) — and usage and the user's echo (heard.go).
const (
	UpdateSessionMessage = "session_message"
	UpdateNotice         = "notice"
	UpdateCompaction     = "compaction_update"
	updateCompactionSum  = "compaction_summary_chunk"
)

// inboundMethods is every method and session/update kind an agent may send
// that the client knows of. dialectHandles has a decision for each of them in
// every dialect, and TestDialectHandlesEveryMethod holds every Method and
// Update constant of this package to being here or outbound only.
var inboundMethods = []string{
	MethodRequestPermission, MethodSessionUpdate,
	MethodCursorAskQuestion, MethodCursorCreatePlan, MethodCursorUpdateTodos, MethodCursorTask,
	MethodGrokAskUserQuestion, MethodGrokAskUserQuestionWrapped,
	MethodGrokExitPlanMode, MethodGrokExitPlanModeWrapped,
	MethodGrokPromptComplete, MethodGrokPromptCompleteWrapped,
	MethodGrokSessionNotification, MethodGrokSessionNotificationWrapped,
	MethodGrokInterjection, MethodGrokInterjectionWrapped,
	MethodGrokQueueChanged, MethodGrokQueueChangedWrapped,
	MethodGrokModelsUpdate, MethodGrokModelsUpdateWrapped,
	MethodGrokSessionsChanged, MethodGrokSessionsChangedWrapped,
	MethodClaudeAuthStatus, MethodAuthStatusUpdate, MethodElicitationCreate, MethodCancelRequest,
	updateMethod(UpdateAgentMessage), updateMethod(UpdateAgentThought),
	updateMethod(UpdateToolCall), updateMethod(UpdateToolCallUpd),
	updateMethod(UpdateAvailableCommands), updateMethod(UpdateCurrentMode),
	updateMethod(UpdateConfigOption), updateMethod(UpdateSessionInfo), updateMethod(UpdatePlan),
	updateMethod(UpdateUsage), updateMethod(updateUserMessageChunk),
	updateMethod(SubagentUpdate), updateMethod(UpdateSessionMessage), updateMethod(UpdateNotice),
	updateMethod(UpdateCompaction), updateMethod(updateCompactionSum),
}

// dialectHandles is the one table of what each dialect does with each method
// it may receive (plan 052 § B1). The client's dispatch asks it before its own
// switch, so a method a dialect has no business sending is refused or ignored
// in one place. Cursor's and grok's rows are what their branches always did:
// cursor/* is cursor's (and, as before, any dialect but grok's), x.ai/* is
// grok's. Claude refuses or ignores both families, reads its auth pushes, and
// handles elicitation/create, the form it declares it renders. Antigravity
// (plan 053 § B1) is claude's row without the auth pushes or the request
// withdrawal: it refuses cursor/* and x.ai's ask and plan, ignores the rest of
// both families, and handles elicitation/create, which it too is told it may
// send.
func dialectHandles(d DialectID, method string) decision {
	if kind, ok := strings.CutPrefix(method, MethodSessionUpdate+"#"); ok {
		return updateDecision(kind)
	}
	switch method {
	case MethodRequestPermission, MethodSessionUpdate:
		return decisionHandle
	case MethodCursorAskQuestion, MethodCursorCreatePlan, MethodCursorUpdateTodos, MethodCursorTask:
		if d == DialectCursor {
			return decisionHandle
		}
		return decisionRefuse
	case MethodGrokAskUserQuestion, MethodGrokAskUserQuestionWrapped,
		MethodGrokExitPlanMode, MethodGrokExitPlanModeWrapped:
		if d == DialectGrok {
			return decisionHandle
		}
		return decisionRefuse
	case MethodGrokPromptComplete, MethodGrokPromptCompleteWrapped,
		MethodGrokSessionNotification, MethodGrokSessionNotificationWrapped,
		MethodGrokInterjection, MethodGrokInterjectionWrapped,
		MethodGrokQueueChanged, MethodGrokQueueChangedWrapped:
		if d == DialectGrok {
			return decisionHandle
		}
		return decisionIgnore
	case MethodGrokModelsUpdate, MethodGrokModelsUpdateWrapped,
		MethodGrokSessionsChanged, MethodGrokSessionsChangedWrapped:
		return decisionIgnore
	case MethodClaudeAuthStatus:
		if d == DialectClaude {
			return decisionHandle
		}
		return decisionIgnore
	case MethodAuthStatusUpdate:
		return decisionIgnore
	case MethodElicitationCreate:
		if d == DialectClaude || d == DialectAntigravity {
			return decisionHandle
		}
		return decisionRefuse
	case MethodCancelRequest:
		// A notification: claude withdraws a request with it; nobody else
		// sends one, and it is dropped there.
		if d == DialectClaude {
			return decisionHandle
		}
		return decisionIgnore
	}
	return decisionNone
}

// updateDecision is dialectHandles for one session/update kind. Every kind is
// the session's to read, on every dialect alike: a kind it does not know it
// drops itself (live.go's onUpdate).
func updateDecision(kind string) decision {
	switch kind {
	case UpdateAgentMessage, UpdateAgentThought, UpdateToolCall, UpdateToolCallUpd,
		UpdateAvailableCommands, UpdateCurrentMode, UpdateConfigOption, UpdateSessionInfo, UpdatePlan,
		UpdateUsage, updateUserMessageChunk:
		return decisionHandle
	case SubagentUpdate, UpdateSessionMessage, UpdateNotice, UpdateCompaction, updateCompactionSum:
		// claude-agent-acp's own: handed to the session, which drops them
		// until later milestones read them. No other dialect sends them.
		return decisionHandle
	}
	return decisionNone
}
