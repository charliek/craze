package control

import (
	"errors"
	"fmt"

	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/protocol"
)

// The error objects the server answers with (plan 027 §3.2). A client decides
// from data.code alone; data.reason names the sentinel; the JSON-RPC integer
// is the envelope's family.

// refused is a craze-level refusal (-32000) with the protocol's own code and
// reason: no engine was involved.
func refused(code protocol.Code, reason protocol.Reason, format string, args ...any) *protocol.Error {
	return &protocol.Error{Code: protocol.RPCRefused, Message: fmt.Sprintf(format, args...),
		Data: protocol.ErrorData{Code: code, Reason: reason}}
}

// invalidRequest is -32600 for a line that is JSON and not a request: data
// {code: bad_request, reason: bad_request} (plan 027 X6).
func invalidRequest(msg string) *protocol.Error {
	return &protocol.Error{Code: protocol.RPCInvalidRequest, Message: msg,
		Data: protocol.ErrorData{Code: protocol.CodeBadRequest, Reason: protocol.ReasonBadRequest}}
}

// lineTooLong is -32600, reason line_too_long, for a line over the inbound
// limit; its id was never read, so it is answered null (§3.2).
func lineTooLong() *protocol.Error {
	return &protocol.Error{Code: protocol.RPCInvalidRequest,
		Message: fmt.Sprintf("the line is over the %d-byte inbound limit and was discarded", protocol.InboundLineMax),
		Data:    protocol.ErrorData{Code: protocol.CodeBadRequest, Reason: protocol.ReasonLineTooLong}}
}

// badParams is -32602, reason bad_request: params the method cannot take.
func badParams(format string, args ...any) *protocol.Error {
	return &protocol.Error{Code: protocol.RPCInvalidParams, Message: fmt.Sprintf(format, args...),
		Data: protocol.ErrorData{Code: protocol.CodeBadRequest, Reason: protocol.ReasonBadRequest}}
}

// unknownField is -32602, reason unknown_field, naming the field (§3.2).
func unknownField(path string) *protocol.Error {
	return &protocol.Error{Code: protocol.RPCInvalidParams,
		Message: fmt.Sprintf("unknown field %s", path),
		Data:    protocol.ErrorData{Code: protocol.CodeBadRequest, Reason: protocol.ReasonUnknownField}}
}

// failed is failed/failed for a failure of the server's own — an encoding
// that could not be written — which is never a reason to retry.
func failed(err error) *protocol.Error {
	return refused(protocol.CodeFailed, protocol.ReasonFailed, "%v", err)
}

// engineError is an engine error on the wire (§3.2): -32000, data.code
// engine.Code(err), data.reason engine.Reason(err), the message err.Error()
// verbatim, and data.cause ErrIndexWrite's wrapped failure — the text a
// client draws for the index half of a rename that happened.
func engineError(err error) *protocol.Error {
	e := &protocol.Error{Code: protocol.RPCRefused, Message: err.Error(), Data: protocol.ErrorData{
		Code:   protocol.Code(engine.Code(err)),
		Reason: protocol.Reason(engine.Reason(err)),
	}}
	if errors.Is(err, engine.ErrIndexWrite) {
		if cause := errors.Unwrap(err); cause != nil {
			e.Data.Cause = cause.Error()
		}
	}
	return e
}
