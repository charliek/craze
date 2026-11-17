package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"

	"github.com/charliek/craze/internal/acp"
	"github.com/charliek/craze/internal/errkind"
)

// claude's prompt errors (plan 052 § D4). claude-agent-acp fails a turn with
// -32603 and `data.errorKind`, the Agent SDK's categorical error
// (SDKAssistantMessageError, sdk.d.ts) or one of the adapter's own, and with
// `data.rateLimit` for a rate_limit (result-end.js turnErrorData); an expired
// or missing login is ACP's authRequired, -32000.

// The JSON-RPC codes claude's errors come with.
const (
	codeInternalError = -32603
	codeAuthRequired  = -32000
)

// ClaudeErrorInfo is what a claude prompt error is (§ D4's table), and
// whether the table says: -32000 is auth; -32603 with data.errorKind is read
// by its kind, anything not tabled being provider; everything else is left
// to what the error says of itself.
func ClaudeErrorInfo(code int, data json.RawMessage, message string) (errkind.Info, bool) {
	if code == codeAuthRequired {
		return errkind.New(errkind.Auth), true
	}
	if code != codeInternalError {
		return errkind.Info{}, false
	}
	kind, rl, ok := claudeErrorData(data)
	if !ok {
		return errkind.Info{}, false
	}
	switch kind {
	case "billing_error":
		return errkind.New(errkind.Quota), true
	case "rate_limit":
		if rl != nil && rl.Status == "rejected" {
			return errkind.New(errkind.Quota), true
		}
		return errkind.Info{Kind: errkind.Provider, Retryable: true}, true
	case "authentication_failed", "oauth_org_not_allowed", "account_on_hold", "verification_required":
		return errkind.New(errkind.Auth), true
	case "model_not_found":
		return errkind.New(errkind.ModelNotFound), true
	case "invalid_request":
		if strings.Contains(strings.ToLower(message), "prompt is too long") {
			return errkind.New(errkind.ContextTooLarge), true
		}
		return errkind.New(errkind.Provider), true
	case "max_output_tokens":
		return errkind.New(errkind.Agent), true
	case "overloaded", "server_error":
		return errkind.Info{Kind: errkind.Provider, Retryable: true}, true
	}
	return errkind.New(errkind.Provider), true
}

// claudeErrorData is an error's data.errorKind and data.rateLimit, and
// whether it carried an errorKind.
func claudeErrorData(data json.RawMessage) (kind string, rl *acp.RateLimit, ok bool) {
	raw := bytes.TrimSpace(data)
	if len(raw) == 0 || raw[0] != '{' {
		return "", nil, false
	}
	var w struct {
		ErrorKind string `json:"errorKind"`
	}
	if json.Unmarshal(raw, &w) != nil || w.ErrorKind == "" {
		return "", nil, false
	}
	if r, has := acp.ErrorRateLimit(raw); has {
		rl = &r
	}
	return w.ErrorKind, rl, true
}

// claudePromptError is a claude prompt's RPC error as § D4 classifies it: its
// words are the agent's, its kind the table's.
type claudePromptError struct {
	err  *acp.RPCError
	info errkind.Info
}

func (e *claudePromptError) Error() string           { return e.err.Error() }
func (e *claudePromptError) Unwrap() error           { return e.err }
func (e *claudePromptError) ErrorInfo() errkind.Info { return e.info }

// classifyPromptErr is a failed claude prompt's error as the turn ends with
// it: a trip of the overage guard's in its rate limit first (§ D3) — the
// session stops, and the trip's error is the turn's — then § D4's kind. Every
// other provider's error is left as it is.
func (s *session) classifyPromptErr(err error) error {
	if !s.provider().isClaude() {
		return err
	}
	if trip := s.claudePromptTrip(err); trip != nil {
		return trip
	}
	var rpc *acp.RPCError
	if !errors.As(err, &rpc) || rpc == nil {
		return err
	}
	info, ok := ClaudeErrorInfo(rpc.Code, rpc.Data, rpc.Message)
	if !ok {
		return err
	}
	return &claudePromptError{err: rpc, info: info}
}
