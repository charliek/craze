package codexrpc

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/charliek/craze/internal/agentproc"
	"github.com/charliek/craze/internal/errkind"
)

const (
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInvalidRequest = -32600
	CodeInternalError  = -32603
)

var (
	// ErrClosed is returned when the codex app-server transport is closed.
	ErrClosed = errors.New("codexrpc: connection closed")
	// ErrLineTooLong is returned when an incoming NDJSON frame exceeds MaxLineBytes.
	ErrLineTooLong = errors.New("codexrpc: frame exceeds maximum line length")
	// ErrAgentExited aliases agentproc.ErrAgentExited so all process-backed
	// transports share one sentinel.
	ErrAgentExited = agentproc.ErrAgentExited
)

// ExitError aliases agentproc.ExitError so errors.As matches across transports.
type ExitError = agentproc.ExitError

// RPCError is a JSON-RPC 2.0 error object returned by codex app-server.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	if e == nil {
		return "json-rpc error"
	}
	return fmt.Sprintf("json-rpc error %d: %s", e.Code, e.Message)
}

// ErrorInfo classifies the RPC error for Craze's error-reporting layer.
func (e *RPCError) ErrorInfo() errkind.Info {
	if e == nil {
		return errkind.New(errkind.Agent)
	}
	if IsAuthMessage(e.Message) {
		return errkind.New(errkind.Auth)
	}
	if IsQuotaMessage(e.Message) {
		return errkind.New(errkind.Quota)
	}
	if IsContextTooLargeMessage(e.Message) {
		return errkind.New(errkind.ContextTooLarge)
	}
	if IsModelNotFoundMessage(e.Message) {
		return errkind.New(errkind.ModelNotFound)
	}
	info := errkind.New(errkind.Agent)
	var data struct {
		Retryable bool `json:"retryable"`
	}
	if len(e.Data) > 0 && json.Unmarshal(e.Data, &data) == nil {
		info.Retryable = data.Retryable
	}
	return info
}

// MethodNotFound returns a standard JSON-RPC -32601 error for unhandled server
// requests.
func MethodNotFound(method string) *RPCError {
	return &RPCError{Code: CodeMethodNotFound, Message: "Method not found: " + method}
}

// CodexErrorInfo decodes Codex's polymorphic `codexErrorInfo` payload, which on
// the wire may be a bare JSON string (e.g. "unauthorized",
// "contextWindowExceeded", "usageLimitExceeded"), a tagged object with a
// "type" field, or a single-key Rust externally-tagged enum object such as
// `{"httpConnectionFailed":{"httpStatusCode":401}}` or
// `{"responseStreamDisconnected":{"httpStatusCode":401}}`.
type CodexErrorInfo struct {
	Kind           string
	HTTPStatusCode int
	Raw            json.RawMessage
}

func (c *CodexErrorInfo) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		*c = CodexErrorInfo{}
		return nil
	}
	c.Raw = append(json.RawMessage(nil), trimmed...)
	var s string
	if json.Unmarshal(trimmed, &s) == nil {
		c.Kind = s
		return nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &obj); err != nil {
		return nil
	}
	if rawType, ok := obj["type"]; ok {
		var t string
		if json.Unmarshal(rawType, &t) == nil {
			c.Kind = t
		}
		if rawCode, ok := obj["httpStatusCode"]; ok {
			var code int
			if json.Unmarshal(rawCode, &code) == nil {
				c.HTTPStatusCode = code
			}
		}
		return nil
	}
	// Rust externally-tagged enum: single key naming the variant.
	for k, v := range obj {
		c.Kind = k
		var nested struct {
			HTTPStatusCode int `json:"httpStatusCode"`
			StatusCode     int `json:"statusCode"`
		}
		if json.Unmarshal(v, &nested) == nil {
			if nested.HTTPStatusCode != 0 {
				c.HTTPStatusCode = nested.HTTPStatusCode
			} else if nested.StatusCode != 0 {
				c.HTTPStatusCode = nested.StatusCode
			}
		}
		break
	}
	return nil
}

// TurnError is the structured error object carried by `error` notifications and
// `turn/completed` when `turn.status == "failed"`.
type TurnError struct {
	Message           string          `json:"message"`
	CodexErrorInfo    *CodexErrorInfo `json:"codexErrorInfo,omitempty"`
	AdditionalDetails string          `json:"additionalDetails,omitempty"`
}

// ClassifyTurnError maps a Codex TurnError into Craze's errkind.Info.
func ClassifyTurnError(te *TurnError) errkind.Info {
	if te == nil {
		return errkind.New(errkind.Agent)
	}
	kind := ""
	status := 0
	if te.CodexErrorInfo != nil {
		kind = strings.ToLower(strings.TrimSpace(te.CodexErrorInfo.Kind))
		status = te.CodexErrorInfo.HTTPStatusCode
	}
	switch {
	case status == 401 || kind == "unauthorized" || IsAuthMessage(te.Message):
		info := errkind.New(errkind.Auth)
		info.Status = status
		info.ProviderCode = kind
		return info
	case status == 429 ||
		kind == "usagelimitexceeded" ||
		kind == "apikeyspendlimitexceeded" ||
		kind == "ratelimitexceeded" ||
		kind == "sessionbudgetexceeded" ||
		IsQuotaMessage(te.Message):
		info := errkind.New(errkind.Quota)
		info.Status = status
		info.ProviderCode = kind
		return info
	case kind == "contextwindowexceeded" || IsContextTooLargeMessage(te.Message):
		info := errkind.New(errkind.ContextTooLarge)
		info.Status = status
		info.ProviderCode = kind
		return info
	case kind == "modelnotfound" || IsModelNotFoundMessage(te.Message):
		info := errkind.New(errkind.ModelNotFound)
		info.Status = status
		info.ProviderCode = kind
		return info
	default:
		info := errkind.New(errkind.Agent)
		info.Status = status
		info.ProviderCode = kind
		return info
	}
}

// IsAuthMessage reports whether msg indicates an authentication failure.
func IsAuthMessage(msg string) bool {
	l := strings.ToLower(msg)
	return strings.Contains(l, "401 unauthorized") ||
		strings.Contains(l, "unauthorized") ||
		strings.Contains(l, "not signed in") ||
		strings.Contains(l, "authentication required") ||
		strings.Contains(l, "invalid api key")
}

// IsQuotaMessage reports whether msg indicates a quota/rate-limit failure.
func IsQuotaMessage(msg string) bool {
	l := strings.ToLower(msg)
	return strings.Contains(l, "usage limit") ||
		strings.Contains(l, "rate limit") ||
		strings.Contains(l, "spend limit") ||
		strings.Contains(l, "quota exceeded") ||
		strings.Contains(l, "429 too many requests")
}

// IsContextTooLargeMessage reports whether msg indicates a context-window overflow.
func IsContextTooLargeMessage(msg string) bool {
	l := strings.ToLower(msg)
	return strings.Contains(l, "context window") ||
		strings.Contains(l, "context_length_exceeded") ||
		strings.Contains(l, "maximum context length")
}

// IsModelNotFoundMessage reports whether msg indicates an unknown/unsupported model.
func IsModelNotFoundMessage(msg string) bool {
	l := strings.ToLower(msg)
	return strings.Contains(l, "model not found") ||
		strings.Contains(l, "does not exist") && strings.Contains(l, "model") ||
		strings.Contains(l, "unknown model")
}

// IsNotSteerable reports whether an RPC error from `turn/steer` indicates the
// thread has no active steerable turn (so the caller should fall back to
// queueing via agent.ErrPromptRequired).
func IsNotSteerable(err error) bool {
	var rpc *RPCError
	if !errors.As(err, &rpc) || rpc == nil {
		return false
	}
	l := strings.ToLower(rpc.Message)
	if strings.Contains(l, "no active turn") ||
		strings.Contains(l, "not steerable") ||
		strings.Contains(l, "activeturnnotsteerable") ||
		strings.Contains(l, "expected turn") ||
		strings.Contains(l, "expectedturnid") ||
		strings.Contains(l, "turn id mismatch") ||
		strings.Contains(l, "turn_id_mismatch") {
		return true
	}
	if len(rpc.Data) > 0 {
		dl := strings.ToLower(string(rpc.Data))
		if strings.Contains(dl, "activeturnnotsteerable") || strings.Contains(dl, "not_steerable") {
			return true
		}
	}
	return false
}
