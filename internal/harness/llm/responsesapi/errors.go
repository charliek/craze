package responsesapi

import (
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"unicode/utf8"
)

// ErrIncomplete is a stream that ended — the connection closed, or the
// server sent [DONE] — before response.completed, response.failed or
// response.incomplete arrived. Success is response.completed alone (OpenAI's
// Sign in with ChatGPT docs: "Treat inference as successful only after
// receiving response.completed"), so whatever was streamed before it is not
// an answer. It wraps io.ErrUnexpectedEOF: a truncated stream, which may be
// tried again before any output (D-32).
var ErrIncomplete = fmt.Errorf("responsesapi: the stream ended without response.completed: %w", io.ErrUnexpectedEOF)

// Error is a failed request: an HTTP status before the stream, or a failure
// the stream reported (response.failed, response.incomplete, or an error
// event).
//
// The route answers in two shapes (OpenAI's docs, errors-and-recovery): a
// direct-admission refusal before a request starts is {"detail": …}, whose
// text is diagnostic and carries no code; a Responses error is the standard
// {"error": {message, type, param, code}}, whose code says what to do. Code,
// Type and Param are kept only as the short identifiers they are meant to be
// (ident, param), so they can never carry free text — or a token — into what
// decides on them.
type Error struct {
	// StatusCode is the HTTP status; 0 for a failure inside the stream.
	StatusCode int
	// Event is the stream event that reported the failure:
	// "response.failed", "response.incomplete" or "error"; "" for an HTTP
	// status.
	Event string
	// Code, Type and Param are error.code, error.type and error.param.
	Code, Type, Param string
	// Reason is a response.incomplete's incomplete_details.reason, e.g.
	// "max_output_tokens" or "content_filter".
	Reason string
	// Message is error.message, or a {"detail": …} body's detail, or what
	// else the body said, cut to maxMessage bytes.
	Message string
	// Detail is set when Message came from a {"detail": …} body.
	Detail bool
	// Usage is what a failed or incomplete response reported it used, nil
	// when it reported nothing: the tokens were spent all the same.
	Usage *Usage
	// Header is an HTTP failure's response headers (Retry-After among them);
	// nil for a failure inside the stream.
	Header http.Header
}

// maxMessage bounds Error.Message: one line of display text, not a page.
const maxMessage = 2048

func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString("responsesapi: ")
	switch {
	case e.StatusCode != 0:
		fmt.Fprintf(&b, "HTTP %d", e.StatusCode)
	case e.Event == "response.incomplete":
		b.WriteString("the response ended incomplete")
	default:
		b.WriteString("the response failed")
	}
	if name := cmp.Or(e.Code, e.Reason); name != "" {
		b.WriteString(" (" + name + ")")
	}
	if e.Message != "" {
		b.WriteString(": " + e.Message)
	}
	return b.String()
}

// finalCodes are the codes that end a request for good wherever they
// arrive, with the HTTP statuses Final names: OpenAI's docs say not to repeat
// any of them (errors-and-recovery), and a usage limit says to pause new
// requests altogether (P33). rate_limit_exceeded is OpenAI's own code for a
// 429, final here inside a stream as every 429 is in a status.
var finalCodes = map[string]bool{
	"subscription_sharing_usage_limit_exceeded":   true,
	"subscription_sharing_user_not_eligible":      true,
	"subscription_sharing_unsupported_capability": true,
	"subscription_sharing_route_not_supported":    true,
	"rate_limit_exceeded":                         true,
}

// transientCodes are the codes and types that may be tried again before any
// output (D-32): usage or account information that could not be checked
// just now (errors-and-recovery: "retry later with bounded backoff"), and a
// server's own fault, as Fantasy's TransientStreamErrorTypes lists it — less
// rate_limit_error, which is a 429 and final here.
var transientCodes = map[string]bool{
	"subscription_sharing_usage_unavailable": true,
	"subscription_sharing_user_unavailable":  true,
	"server_error":                           true,
	"internal_error":                         true,
	"overloaded_error":                       true,
	"api_error":                              true,
}

// Final reports whether the failure must never be retried (plan 033 §3.9):
// every 429 and every 400 — the same body would be refused again — and the
// codes OpenAI's docs say not to repeat (finalCodes, and every
// chatpass_v2_* code), in a status or inside the stream alike.
func (e *Error) Final() bool {
	if e.StatusCode == http.StatusTooManyRequests || e.StatusCode == http.StatusBadRequest {
		return true
	}
	return finalCodes[e.Code] || strings.HasPrefix(e.Code, "chatpass_v2_")
}

// Transient reports whether the failure may be tried again before any
// output: a 503 (direct routing unavailable, or a usage check that could not
// run), or a code or type transientCodes names. Other HTTP statuses are left
// to the caller's own rule for them (Fantasy retries 408, 409 and 5xx). A
// final failure is never transient.
func (e *Error) Transient() bool {
	if e.Final() {
		return false
	}
	return e.StatusCode == http.StatusServiceUnavailable || transientCodes[e.Code] || transientCodes[e.Type]
}

// contextPattern matches a provider's words for a request larger than the
// model's context window, for an overflow sent with no code: the Responses
// API's "Your input exceeds the context window of this model", and the
// older "maximum context length".
var contextPattern = regexp.MustCompile(`(?i)exceeds the context window|maximum context length`)

// ContextTooLarge reports whether the failure says the request exceeded the
// model's context window: the harness compacts and sends a smaller request
// for it, final as the request that failed is (plan 028).
func (e *Error) ContextTooLarge() bool {
	return e.Code == "context_length_exceeded" || contextPattern.MatchString(e.Message)
}

// statusError reads a non-2xx response into an *Error and closes its body.
func statusError(resp *http.Response) *Error {
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	e := &Error{StatusCode: resp.StatusCode, Header: resp.Header.Clone()}
	var body struct {
		Detail json.RawMessage `json:"detail"`
		Error  *wireError      `json:"error"`
	}
	if json.Unmarshal(raw, &body) == nil {
		switch {
		case body.Error != nil:
			e.setError(body.Error)
		case len(body.Detail) > 0 && string(body.Detail) != "null":
			e.Detail = true
			var s string
			if json.Unmarshal(body.Detail, &s) == nil {
				e.Message = cut(s)
			} else {
				e.Message = cut(string(body.Detail))
			}
		}
	}
	if e.Message == "" && e.Code == "" && !e.Detail {
		e.Message = cut(strings.TrimSpace(string(raw)))
	}
	return e
}

// wireError is the standard error object. Its code and param may be null or
// a number on some failures, so each is read raw and kept only as a string.
type wireError struct {
	Code    json.RawMessage `json:"code"`
	Type    json.RawMessage `json:"type"`
	Param   json.RawMessage `json:"param"`
	Message string          `json:"message"`
}

func (e *Error) setError(w *wireError) {
	e.Code = ident(jsonString(w.Code))
	e.Type = ident(jsonString(w.Type))
	e.Param = param(jsonString(w.Param))
	e.Message = cut(w.Message)
}

// identPattern is what an error code or type must look like to be kept: a
// short lowercase identifier, as every code the docs list is
// ("subscription_sharing_usage_limit_exceeded", "chatpass_v2_scope_not_authorized").
var identPattern = regexp.MustCompile(`^[a-z][a-z0-9_.]{0,63}$`)

func ident(v string) string {
	if !identPattern.MatchString(v) {
		return ""
	}
	return v
}

// paramPattern is what error.param must look like to be kept: a field path,
// "max_output_tokens" or "input[3].content[0].image_url".
var paramPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.\[\]-]{0,127}$`)

func param(v string) string {
	if !paramPattern.MatchString(v) {
		return ""
	}
	return v
}

// jsonString is raw as a string when it is a JSON string, else "".
func jsonString(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

// cut is s cut to maxMessage bytes at a rune boundary, invalid UTF-8
// dropped.
func cut(s string) string {
	s = strings.ToValidUTF8(s, "")
	if len(s) <= maxMessage {
		return s
	}
	n := maxMessage
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}
