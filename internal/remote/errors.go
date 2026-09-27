package remote

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/charliek/craze/internal/backend"
	"github.com/charliek/craze/internal/protocol"
)

// Error is a host's error reply (plan 027 §3.2): the JSON-RPC integer, the
// craze code a client decides from, the reason naming the exact sentinel,
// the host's message verbatim, and the two things an error may carry beside
// it — a result (session.cancel's CancelResult, hello's supported versions)
// and a wrapped failure's text.
//
// It reconstructs the host's Go error for a caller that matches by sentinel
// (plan 027 §3.14, A16): Is maps (Code, Reason) to the engine's or agent's
// sentinel through one table (sentinels, sentinels.go), so errors.Is(err,
// agent.ErrQueueFull) holds over the socket exactly where it holds in process;
// Unwrap is the wrapped failure's text (Cause) as an error, so errors.Unwrap
// reaches ErrIndexWrite's cause as it does in process; and Error is the
// host's message verbatim, the text the host's own error had.
type Error struct {
	// RPC is the JSON-RPC error integer: -32000 for every craze-level
	// refusal, -32600/-32601/-32602/-32700 for the envelope's own.
	RPC int
	// Code is data.code, the closed set a client decides from, alone
	// (Retry says what may be resent).
	Code protocol.Code
	// Reason is data.reason: for wording and for reconstructing the Go
	// error, never for retry.
	Reason protocol.Reason
	// Message is the host's message, verbatim.
	Message string
	// Result is data.result, raw; nil when the error carries none.
	Result json.RawMessage
	// Cause is data.cause: a wrapped failure's text ("" for none).
	Cause string
}

// Error is the host's message, verbatim: the text the host's own error had.
func (e *Error) Error() string { return e.Message }

// Is reports whether target is a sentinel the host's error was (or wrapped):
// the table's answer for (Code, Reason) — sentinels, the one table.
func (e *Error) Is(target error) bool { return slices.Contains(sentinels(e.Code, e.Reason), target) }

// Unwrap is the wrapped failure the host sent beside its error (Cause) — the
// store's own message behind an engine.ErrIndexWrite, a start's failure — as
// an error whose text is the host's, or nil when it sent none. In process
// errors.Unwrap of an ErrIndexWrite is the store's error, whose text is this
// one (engine/index.go's indexWriteError).
func (e *Error) Unwrap() error {
	if e.Cause == "" {
		return nil
	}
	return errors.New(e.Cause)
}

// newError is a response's error object as an *Error.
func newError(pe *protocol.Error) *Error {
	return &Error{
		RPC:     pe.Code,
		Code:    pe.Data.Code,
		Reason:  pe.Data.Reason,
		Message: pe.Message,
		Result:  pe.Data.Result,
		Cause:   pe.Data.Cause,
	}
}

// ErrOutcomeUnknown is a command whose outcome the client cannot learn (plan
// 027 §3.14): it may have run, and nothing refused it. Its reason — one of
// protocol's client-side reasons, which no host ever sends — says why:
// resume_lost (a reconnect the host answered resumed: false: nothing is
// resent, since a resend under a fresh client id could run a completed command
// a second time; or a command bound to a client identity the client has since
// left, never sent under another) or disconnected (the client's redials are
// spent, or it was closed). The client re-reads state. errors.Is(err,
// ErrOutcomeUnknown) matches every *OutcomeUnknownError.
//
// It IS backend.ErrOutcomeUnknown — one sentinel, so a caller of the Backend
// seam (the TUI) matches it without importing this package, and one that holds
// a *Client matches the same value.
var ErrOutcomeUnknown = backend.ErrOutcomeUnknown

// OutcomeUnknownError is ErrOutcomeUnknown for one command: its method, its
// command id, the client-side reason, and — for a command refused because the
// client identity it was bound to is gone (a stale backend epoch) —
// backend.ErrStaleEpoch in Err, which errors.Is finds through Unwrap.
type OutcomeUnknownError struct {
	Method    string
	CommandID string
	Reason    protocol.Reason
	// Err is what else the outcome is (nil for none): backend.ErrStaleEpoch
	// for a command bound to an identity the client has left, never sent
	// under the one it holds now.
	Err error
}

func (e *OutcomeUnknownError) Error() string {
	s := fmt.Sprintf("remote: %s (command %s): the outcome is unknown (%s): it may have run", e.Method, e.CommandID, e.Reason)
	if e.Err != nil {
		s += ": " + e.Err.Error()
	}
	return s
}

// Is makes errors.Is(err, ErrOutcomeUnknown) true — and so errors.Is(err,
// backend.ErrOutcomeUnknown), the same value.
func (e *OutcomeUnknownError) Is(target error) bool { return target == ErrOutcomeUnknown }

// Unwrap is Err: errors.Is(err, backend.ErrStaleEpoch) for a stale one.
func (e *OutcomeUnknownError) Unwrap() error { return e.Err }

// TooLargeError is a command the client settled not run because the host a
// reconnect reached reads shorter lines than the one it was sized against
// (hello's limits.inboundLine): not a byte of it was written (X21).
// errors.Is(err, ErrNotRun) and errors.Is(err, ErrRequestTooLarge) are both
// true of it.
type TooLargeError struct {
	Method    string
	CommandID string
	// Size is the command's request line, at its longest; Limit the host's
	// inbound limit, both in bytes.
	Size, Limit int
}

func (e *TooLargeError) Error() string {
	return fmt.Sprintf("remote: %s (command %s) did not run: it is %d bytes, over the %d the host now reached reads",
		e.Method, e.CommandID, e.Size, e.Limit)
}

// Is makes errors.Is(err, ErrNotRun) and errors.Is(err, ErrRequestTooLarge)
// true.
func (e *TooLargeError) Is(target error) bool {
	return target == ErrNotRun || target == ErrRequestTooLarge
}

// The client's own errors.
var (
	// ErrClosed is a call on a client that has been closed, or whose
	// connection is gone for good.
	ErrClosed = errors.New("remote: the client is closed")
	// ErrDisconnected is why a client stopped when a reconnect episode was
	// spent (Options.Redials attempts, or Options.RedialWindow since the
	// loss): the stream's final Error item carries it, and calls made after it
	// fail with it.
	ErrDisconnected = errors.New("remote: disconnected: the redials are spent")
	// ErrSessionEnded is why a client stopped once its stream saw
	// reset{session_closed} and the host closed the connection: there is no
	// session to reconnect to.
	ErrSessionEnded = errors.New("remote: the session has ended")
	// ErrConnectionLost answers a call (not a command) whose connection went
	// before its reply came. A read may be sent again; the client is
	// reconnecting.
	ErrConnectionLost = errors.New("remote: the connection was lost before the reply")
	// ErrNotRun answers a command the client settled knowing it did not run:
	// it was never sent (it waited for a connection), or its last answer said
	// nothing ran, and the client stopped before another attempt. A
	// *TooLargeError is ErrNotRun too.
	ErrNotRun = errors.New("remote: the command did not run: the client stopped before it was sent")
	// ErrAlreadyAttached is an Attach on a client whose stream is still open:
	// a connection holds one attachment (SQ14).
	ErrAlreadyAttached = errors.New("remote: this client's stream is still open")
	// ErrStreamClosed is Next on a stream that has handed up its last item, or
	// that was closed.
	ErrStreamClosed = errors.New("remote: the stream is closed")
	// ErrRequestTooLarge is a request longer than the host reads (its
	// inbound line limit): it is never sent.
	ErrRequestTooLarge = errors.New("remote: the request is over the host's inbound line limit")
	// ErrEndpoint is a hello answered by the wrong kind of endpoint: a hub
	// where a session's host belongs (a hub is accepted only for the hub hop,
	// Options.Connect), or a host where the hub hop expected a hub.
	ErrEndpoint = errors.New("remote: the wrong kind of endpoint answered")
)

// VersionError is a hello the endpoint refused because it shares no protocol
// version with the client (bad_request, reason protocol_version): Supported is
// what the endpoint does speak (data.result.supported).
type VersionError struct {
	Supported []int
	Err       *Error
}

func (e *VersionError) Error() string {
	return fmt.Sprintf("remote: no protocol version in common: the endpoint speaks %v, this client %v",
		e.Supported, protocol.SupportedProtocols())
}

func (e *VersionError) Unwrap() error { return e.Err }

// helloError is a refused hello as the error Dial returns: a VersionError for
// protocol_version, the *Error otherwise.
func helloError(e *Error) error {
	if e.Code == protocol.CodeBadRequest && e.Reason == protocol.ReasonProtocolVersion {
		var r protocol.HelloErrorResult
		_ = json.Unmarshal(e.Result, &r)
		return &VersionError{Supported: r.Supported, Err: e}
	}
	return e
}
