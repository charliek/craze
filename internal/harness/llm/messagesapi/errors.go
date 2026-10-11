package messagesapi

import (
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// ErrIncomplete is a stream that ended, or broke the protocol, before a
// complete message: message_stop after a stop reason (P4). Whatever was
// streamed before it is not an answer. It wraps io.ErrUnexpectedEOF: a
// truncated stream, which may be tried again before any output, as Fantasy
// treats an incomplete stream (fantasy.NewIncompleteStreamError). A
// protocol violation is an error that unwraps to it and says which rule
// broke, with no text of the server's.
var ErrIncomplete = fmt.Errorf("messagesapi: the stream ended before message_stop with a stop reason: %w", io.ErrUnexpectedEOF)

// violation is a stream that broke the protocol: ErrIncomplete, with the
// rule it broke.
type violation struct{ rule string }

func (v *violation) Error() string { return "messagesapi: the stream broke the protocol: " + v.rule }
func (v *violation) Unwrap() error { return ErrIncomplete }

func broke(format string, args ...any) error { return &violation{rule: fmt.Sprintf(format, args...)} }

// Error is a failed request: an HTTP status before the stream, or an error
// event inside it.
//
// The API answers {"type":"error","error":{"type","message"},"request_id"}
// before the stream, and an error event {"type":"error","error":{…}} inside
// it — before message_start too: a 200 that then fails is the streaming form
// of a 429 or 529. Type is kept only as the short identifier it is meant to
// be (ident), so it can never carry free text — or a key a server echoed —
// into what decides on it.
type Error struct {
	// StatusCode is the HTTP status; for an error event, the status its type
	// stands for (statusOfType), 0 for a type craze does not know.
	StatusCode int
	// InStream is set for an error event.
	InStream bool
	// Type is error.type: "overloaded_error", "rate_limit_error", …
	Type string
	// Message is error.message, or what else the body said, cut to
	// maxMessage bytes.
	Message string
	// RequestID is the request-id response header, else the body's
	// request_id; kept only when it looks like one.
	RequestID string
	// RetryAfter is how long the server asked to wait (retry-after-ms, else
	// retry-after); 0 when it did not say.
	RetryAfter time.Duration
	// ShouldRetry is the x-should-retry header: nil when absent.
	ShouldRetry *bool

	// overflow is ContextTooLarge's verdict, read once from the server's
	// message as it was decoded, and shownType is Type as Error shows it,
	// scrubbed. What decides is read from the server's data before anything
	// is made of it: scrubbed, a key that is also a word the decision reads
	// would change it (review c1r2 4); made valid, a dropped character could
	// join words that were apart (review c1r3 1). Only what is shown is
	// scrubbed, and a verdict is never read again from Message. The client
	// sets both; an Error built by hand has neither, so it is context too
	// large only by its 413, and Error shows its Type.
	overflow  bool
	shownType string
}

// The response headers RequestID, RetryAfter and ShouldRetry come from are
// the failed response's for an HTTP status, and for an error event the
// stream's own 200 response's: a server that said x-should-retry: false on
// the stream meant it for the failure inside it too (review c1 F3), and P2
// makes that failure final.

// maxMessage bounds Error.Message: one line of display text, not a page.
const maxMessage = 2048

func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString("messagesapi: ")
	if e.InStream {
		b.WriteString("the stream failed")
	} else {
		fmt.Fprintf(&b, "HTTP %d", e.StatusCode)
	}
	if t := cmp.Or(e.shownType, e.Type); t != "" {
		b.WriteString(" (" + t + ")")
	}
	if e.Message != "" {
		b.WriteString(": " + e.Message)
	}
	return b.String()
}

// statusOfType is the HTTP status each error type stands for (WF §6): an
// error event carries no status of its own, the response having been a 200.
var statusOfType = map[string]int{
	"invalid_request_error": http.StatusBadRequest,
	"authentication_error":  http.StatusUnauthorized,
	"billing_error":         http.StatusPaymentRequired,
	"permission_error":      http.StatusForbidden,
	"not_found_error":       http.StatusNotFound,
	"request_too_large":     http.StatusRequestEntityTooLarge,
	"rate_limit_error":      http.StatusTooManyRequests,
	"api_error":             http.StatusInternalServerError,
	"overloaded_error":      529,
}

// transientStatuses are the statuses that may be tried again before any
// output: the API's retry policy (408, 409, 429, 5xx it names, and 529,
// overloaded).
var transientStatuses = map[int]bool{
	http.StatusRequestTimeout:      true,
	http.StatusConflict:            true,
	http.StatusTooManyRequests:     true,
	http.StatusInternalServerError: true,
	http.StatusBadGateway:          true,
	http.StatusServiceUnavailable:  true,
	http.StatusGatewayTimeout:      true,
	529:                            true,
}

// transientTypes are the error types that name a temporary condition.
var transientTypes = map[string]bool{
	"overloaded_error": true,
	"rate_limit_error": true,
	"api_error":        true,
}

// Transient reports whether the failure may be tried again before any
// output. The server's x-should-retry decides when it was sent (the API's
// own SDKs honour it both ways); otherwise a transient status or type does.
func (e *Error) Transient() bool {
	if e.ShouldRetry != nil {
		return *e.ShouldRetry
	}
	return transientStatuses[e.StatusCode] || transientTypes[e.Type]
}

// AuthError reports whether the API refused the key: a 401, or an
// authentication_error event (whose status is 401).
func (e *Error) AuthError() bool { return e.StatusCode == http.StatusUnauthorized }

// contextPattern matches the API's words for a prompt larger than the
// model's context window: "prompt is too long: 1050000 tokens > 1000000
// maximum", and "input length and `max_tokens` exceed context limit".
var contextPattern = regexp.MustCompile(`(?i)prompt is too long|input length and .*max_tokens.* exceed context limit`)

// ContextTooLarge reports whether the failure says the request was too
// large for the model: a 400 whose message, as the server sent it, is in
// those words (overflow), or a 413 (request_too_large). The harness compacts
// and sends a smaller request for it.
func (e *Error) ContextTooLarge() bool {
	return e.StatusCode == http.StatusRequestEntityTooLarge ||
		(e.StatusCode == http.StatusBadRequest && e.overflow)
}

// wireError is the error object. Its fields are read raw, so a type or
// message of another JSON type costs that field, not the error.
type wireError struct {
	Type    json.RawMessage `json:"type"`
	Message json.RawMessage `json:"message"`
}

// replyFacts are what a response's headers say for an *Error made of it:
// its request id, the wait it asked for, and x-should-retry. Every text a
// server sent is scrubbed of the key before it is kept (review c1 F1).
func replyFacts(h http.Header, sec *secret) Error {
	return Error{
		RequestID:   requestID(sec.text(h.Get("Request-Id"))),
		RetryAfter:  retryAfter(h),
		ShouldRetry: shouldRetry(h),
	}
}

// statusError reads a non-2xx response into an *Error and closes its body.
func statusError(resp *http.Response, sec *secret) *Error {
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	e := replyFacts(resp.Header, sec)
	e.StatusCode = resp.StatusCode
	var body struct {
		Error     *wireError      `json:"error"`
		RequestID json.RawMessage `json:"request_id"`
	}
	if json.Unmarshal(raw, &body) == nil && body.Error != nil {
		e.setError(body.Error, sec)
		if e.RequestID == "" {
			e.RequestID = requestID(sec.text(jsonString(body.RequestID)))
		}
		return &e
	}
	e.setMessage(strings.TrimSpace(string(raw)), sec)
	return &e
}

// streamError is an error event's *Error: reply is what the stream's own
// response said (replyFacts), and the status is the event's type's.
func streamError(w *wireError, reply Error, sec *secret) *Error {
	e := reply
	e.InStream = true
	if w != nil {
		e.setError(w, sec)
	}
	e.StatusCode = statusOfType[e.Type]
	return &e
}

// setError keeps an error object's type, as an identifier, and its message
// (setMessage). The type decides as it came (review c1r2 4); Error shows it
// scrubbed.
func (e *Error) setError(w *wireError, sec *secret) {
	e.Type = ident(jsonString(w.Type))
	e.shownType = sec.text(e.Type)
	e.setMessage(jsonString(w.Message), sec)
}

// setMessage keeps a server's message: its overflow verdict read first,
// from msg as decoded (review c1r3 1), then the text made valid, scrubbed of
// the key, and cut — in that order (review c1r2 1): text made valid after
// the scrub could join a key the scrub never saw whole, and a cut before it
// could leave part of one.
func (e *Error) setMessage(msg string, sec *secret) {
	e.overflow = contextPattern.MatchString(msg)
	e.Message = cut(sec.text(valid(msg)))
}

// maxRetryAfter bounds RetryAfter: a server's wait past a day is a day.
const maxRetryAfter = 24 * time.Hour

// retryAfter is the wait a response asked for: retry-after-ms (a number of
// milliseconds), else retry-after (seconds, or an HTTP date); 0 when it
// asked for none, or for none craze can read.
func retryAfter(h http.Header) time.Duration {
	if d, ok := seconds(h.Get("Retry-After-Ms"), 1e-3); ok {
		return d
	}
	v := h.Get("Retry-After")
	if d, ok := seconds(v, 1); ok {
		return d
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return min(d, maxRetryAfter)
		}
	}
	return 0
}

// seconds is v, a number of units of scale seconds, as a positive duration
// within maxRetryAfter.
func seconds(v string, scale float64) (time.Duration, bool) {
	if v == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil || math.IsNaN(f) || f <= 0 {
		return 0, false
	}
	s := min(f*scale, maxRetryAfter.Seconds())
	return time.Duration(s * float64(time.Second)), true
}

// shouldRetry is the x-should-retry header: nil when it is absent or not a
// boolean.
func shouldRetry(h http.Header) *bool {
	b, err := strconv.ParseBool(h.Get("X-Should-Retry"))
	if err != nil {
		return nil
	}
	return &b
}

// keep is v when it has re's shape, else "": a server's value is kept only
// as what it should be, so free text — or a key a server echoed — never
// rides along in a field meant for an identifier.
func keep(re *regexp.Regexp, v string) string {
	if !re.MatchString(v) {
		return ""
	}
	return v
}

// identPattern is what an error type, a stop reason or a refusal category
// must look like to be kept: a short lowercase identifier, as every value
// the API documents is ("overloaded_error", "end_turn", "cyber").
var identPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

func ident(v string) string { return keep(identPattern, v) }

// requestIDPattern is what a request id (req_018EeW…) or a message id
// (msg_011Cfvn…) must look like to be kept.
var requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

func requestID(v string) string { return keep(requestIDPattern, v) }

// jsonString is raw as a string when it is a JSON string, else "".
func jsonString(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

// cut is s, valid text (setMessage), cut to maxMessage bytes at a rune
// boundary.
func cut(s string) string {
	if len(s) <= maxMessage {
		return s
	}
	n := maxMessage
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}
