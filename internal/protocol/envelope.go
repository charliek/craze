package protocol

import (
	"encoding/json"
	"fmt"
)

// JSONRPCVersion is the "jsonrpc" member every message carries (plan 027
// §3.2: JSON-RPC 2.0, one object per line).
const JSONRPCVersion = "2.0"

// The JSON-RPC error integers, the envelope's own (plan 027 §3.2). A client
// decides nothing from them — data.code is what it reads — and they are here so
// a generic JSON-RPC client still sees the right family: a line that is not
// JSON, a message that is not a request, a method the host does not know,
// params it refuses, and every craze-level refusal.
const (
	// RPCParseError answers a line that is not JSON, with data {code:
	// bad_request, reason: bad_request} and id null (plan 027 X6).
	RPCParseError = -32700
	// RPCInvalidRequest answers a line that is JSON and not a request: a batch
	// array, an object with no method — data {code: bad_request, reason:
	// bad_request} (X6) — or an over-long line, reason line_too_long; an id
	// that was never read is null.
	RPCInvalidRequest = -32600
	// RPCMethodNotFound answers a method the host does not know (reason
	// unknown_method).
	RPCMethodNotFound = -32601
	// RPCInvalidParams answers params the host refuses: malformed, of the wrong
	// type, a combination the method does not take (session.prompt's interject
	// with a fromRow, reason bad_request, X6), or carrying a field it does not
	// know (reason unknown_field).
	RPCInvalidParams = -32602
	// RPCRefused is every craze-level refusal: the engine's, and the
	// protocol's own (unknown_session, hello_required, bad_token, …).
	RPCRefused = -32000
)

// Request is a client's call (plan 027 §3.2): {"jsonrpc":"2.0","id":…,
// "method":…,"params":{…}}. ID is the client's own, a number or a string,
// echoed back byte for byte in the Response and unique among the
// connection's requests in flight; it is not a command id (SD-11), which
// travels in params. Params is always an object; a method that takes nothing
// may send {} or leave it out.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// Response is the host's answer to one Request: exactly one of Result and
// Error. ID is the request's, verbatim, and null only when the request's id
// could not be read — a line that is not JSON, or one too long to parse (§3.2:
// answered -32600 with id null, and the connection stays open). A nil ID
// marshals as null.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// Notification is a message with no id: the host's event, synchronized,
// ready and reset (plan 027 §3.3). Clients send none in protocol 1.
type Notification struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

// Error is a Response's error object (plan 027 §3.2): the JSON-RPC integer,
// a message for people, and data, which is what a client decides from.
type Error struct {
	Code    int       `json:"code"`
	Message string    `json:"message"`
	Data    ErrorData `json:"data"`
}

// ErrorData is an error's craze half (plan 027 §3.2). Code is the closed set
// a client decides from, alone (Retry says what it may resend). Reason names
// the exact sentinel under that code, for wording and for a craze client's
// reconstruction of the Go error — never for retry. Result carries a result
// returned beside the error: session.cancel's CancelResult (with reported),
// and hello's HelloErrorResult on protocol_version. Cause carries a wrapped
// failure's text: ErrIndexWrite's, and a start failure's.
type ErrorData struct {
	Code   Code            `json:"code"`
	Reason Reason          `json:"reason"`
	Result json.RawMessage `json:"result,omitempty"`
	Cause  string          `json:"cause,omitempty"`
}

// Error is e as one line of text: its code, reason and message.
func (e *Error) Error() string {
	if e == nil {
		return "protocol: error"
	}
	if e.Data.Reason != "" && string(e.Data.Reason) != string(e.Data.Code) {
		return fmt.Sprintf("%s (%s): %s", e.Data.Code, e.Data.Reason, e.Message)
	}
	return fmt.Sprintf("%s: %s", e.Data.Code, e.Message)
}
