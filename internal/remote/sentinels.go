package remote

import (
	"context"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/transcript"
)

// sentinels is THE table (plan 027 §3.14, A16): the Go sentinels a host's
// error of (code, reason) is, as errors.Is sees the engine's own error in
// process. *Error.Is reads it and nothing else does, so a reconstructed error
// matches exactly what the in-process error it stands for matches:
// TestEveryReasonReconstructsItsSentinel holds it, row by row, against
// engine.Code and engine.Reason for every sentinel classify names
// (engine/control.go), and against errors.Is of the engine's own error for
// every sentinel of the set.
//
// Each engine reason is its sentinel's name (classify's third column), so a
// row is that sentinel — plus what the engine's error wraps beside it, where
// it wraps something a caller matches:
//
//   - set_outcome_unknown wraps the command's context error, and not_run (a
//     Set its dead context answered unrun, whose own sentinel is unexported)
//     is that error alone. Over the socket that context is the host's own
//     command bound (control's commandCtx: a timeout, never cancelled while
//     the call runs), so the error is context.DeadlineExceeded — and so is
//     the context reason's, a command that ran and gave up on that bound.
//   - agent.ErrStaleModel is engine.ErrStaleModel: one value.
//
// The protocol's own reasons have no engine error behind them, with two
// exceptions: start_failed is a gate refusal of a session whose start failed,
// which in process is engine.ErrNotAccepting (refusalLocked); and
// snapshot_too_large is transcript.ErrSnapshotTooLarge, which the host wraps.
// A reason sent under another code than the table's, a reason this build does
// not know, and the client-side reasons (never sent by a host) reconstruct
// nothing: the error is then its code alone, as an unknown code is failed.
func sentinels(code protocol.Code, reason protocol.Reason) []error {
	if info, ok := reason.Lookup(); !ok || info.ClientSide || info.Code != code {
		return nil
	}
	switch reason {
	// not_accepting
	case protocol.ReasonNotAccepting, protocol.ReasonStartFailed:
		return []error{engine.ErrNotAccepting}
	case protocol.ReasonNotInTurn:
		return []error{agent.ErrNotInTurn}
	// aborted
	case protocol.ReasonCommandAborted:
		return []error{engine.ErrCommandAborted}
	case protocol.ReasonSetOutcomeUnknown:
		return []error{engine.ErrSetOutcomeUnknown, context.DeadlineExceeded}
	case protocol.ReasonBadCatalog:
		return []error{agent.ErrBadCatalog}
	case protocol.ReasonContext:
		return []error{context.DeadlineExceeded}
	// unavailable
	case protocol.ReasonLogBackedUp:
		return []error{engine.ErrUnavailable}
	case protocol.ReasonAskUnavailable:
		return []error{agent.ErrAskUnavailable}
	case protocol.ReasonSetUnavailable:
		return []error{agent.ErrSetUnavailable}
	case protocol.ReasonNotRun:
		return []error{context.DeadlineExceeded}
	case protocol.ReasonAttachRaced:
		return []error{engine.ErrAttachRaced}
	// failed
	case protocol.ReasonOptionGone:
		return []error{agent.ErrOptionGone}
	case protocol.ReasonSnapshotTooLarge:
		return []error{transcript.ErrSnapshotTooLarge}
	// bad_request
	case protocol.ReasonBadRequest:
		return []error{engine.ErrBadRequest}
	case protocol.ReasonBadAnswer:
		return []error{agent.ErrBadAnswer}
	// unsupported
	case protocol.ReasonUnsupported:
		return []error{agent.ErrUnsupported}
	// every other code: the code itself, its one reason
	case protocol.ReasonUnknownAsk:
		return []error{agent.ErrUnknownAsk}
	case protocol.ReasonAlreadySubmitted:
		return []error{engine.ErrAlreadyPending}
	case protocol.ReasonAlreadyResolved:
		return []error{agent.ErrAlreadyResolved}
	case protocol.ReasonQueueFull:
		return []error{agent.ErrQueueFull}
	case protocol.ReasonTextTooLong:
		return []error{agent.ErrQueueTextTooLong}
	case protocol.ReasonPromptInFlight:
		return []error{agent.ErrPromptInFlight}
	case protocol.ReasonForeignTurn:
		return []error{agent.ErrForeignTurn}
	case protocol.ReasonPromptCancelled:
		return []error{agent.ErrPromptCancelled}
	case protocol.ReasonStaleVersion:
		return []error{engine.ErrStaleVersion}
	case protocol.ReasonStaleTurn:
		return []error{engine.ErrStaleTurn}
	case protocol.ReasonUnknownRow:
		return []error{engine.ErrUnknownRow}
	case protocol.ReasonUnknownCommand:
		return []error{engine.ErrUnknownCommand}
	case protocol.ReasonInProgress:
		return []error{engine.ErrCommandInProgress}
	case protocol.ReasonStaleModel:
		return []error{engine.ErrStaleModel}
	case protocol.ReasonIndexWrite:
		return []error{engine.ErrIndexWrite}
	case protocol.ReasonUnknownSubagent:
		return []error{agent.ErrNoSuchSubagent}
	}
	// failed (a provider's or the host's own failure), not_ready, busy,
	// response_too_large, the protocol's bad_request and unsupported reasons,
	// unknown_session: no sentinel stands behind them.
	return nil
}
