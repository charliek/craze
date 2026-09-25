package protocol

import "slices"

// Code is an error's data.code: the closed set a client decides from, alone
// (plan 027 §3.2; 05's codes plus unknown_subagent). Every code the engine
// answers with (engine.Code) is one of these, and so is unknown_session, the
// protocol's own. A client that meets a code it does not know treats it as
// failed: the command ran and this is its stored answer.
type Code string

const (
	CodeBadRequest       Code = "bad_request"
	CodeUnknownSession   Code = "unknown_session"
	CodeUnknownAsk       Code = "unknown_ask"
	CodeAlreadySubmitted Code = "already_submitted"
	CodeAlreadyResolved  Code = "already_resolved"
	CodeNotAccepting     Code = "not_accepting"
	CodeUnsupported      Code = "unsupported"
	CodeUnavailable      Code = "unavailable"
	CodeQueueFull        Code = "queue_full"
	CodeTextTooLong      Code = "text_too_long"
	CodePromptInFlight   Code = "prompt_in_flight"
	CodeForeignTurn      Code = "foreign_turn"
	CodePromptCancelled  Code = "prompt_cancelled"
	CodeStaleVersion     Code = "stale_version"
	CodeStaleTurn        Code = "stale_turn"
	CodeUnknownRow       Code = "unknown_row"
	CodeUnknownCommand   Code = "unknown_command"
	CodeInProgress       Code = "in_progress"
	CodeStaleModel       Code = "stale_model"
	CodeAborted          Code = "aborted"
	CodeFailed           Code = "failed"
	CodeIndexWrite       Code = "index_write"
	CodeUnknownSubagent  Code = "unknown_subagent"
)

var codes = []Code{
	CodeBadRequest, CodeUnknownSession, CodeUnknownAsk, CodeAlreadySubmitted, CodeAlreadyResolved,
	CodeNotAccepting, CodeUnsupported, CodeUnavailable, CodeQueueFull, CodeTextTooLong,
	CodePromptInFlight, CodeForeignTurn, CodePromptCancelled, CodeStaleVersion, CodeStaleTurn,
	CodeUnknownRow, CodeUnknownCommand, CodeInProgress, CodeStaleModel, CodeAborted, CodeFailed,
	CodeIndexWrite, CodeUnknownSubagent,
}

// Codes is the closed set, in a fixed order: every code a host may send.
func Codes() []Code { return slices.Clone(codes) }

// Known reports whether c is one of the closed set.
func (c Code) Known() bool { return slices.Contains(codes, c) }

// Retry reports whether a client may resend a command answered with c under
// the SAME command id (plan 027 §3.6 "Retry by code"; 05's retry table). It
// is true for exactly the four codes the engine never stores —
// unavailable, not_accepting, in_progress and stale_model: nothing ran, and
// the id is as unseen as before the attempt, so a resend is a genuine first
// attempt (mandatory for in_progress, which is its command still running).
// For every other code, the answer is the command's own stored one and a
// resend replays it: a client that wants another attempt at a command that
// ran (aborted, failed) sends a NEW id, and after aborted re-reads state
// first. Resending is also bounded by the connection: after a reconnect, a
// client resends nothing unless the host answered resumed: true (§3.14).
func Retry(c Code) bool {
	switch c {
	case CodeUnavailable, CodeNotAccepting, CodeInProgress, CodeStaleModel:
		return true
	default:
		return false
	}
}

// Reason is an error's data.reason (plan 027 §3.2): a second closed set,
// finer than the code, naming the exact sentinel — the engine's own reasons
// (engine.Reason), the protocol's, and three a client makes up for itself and
// no host ever sends. It is for wording and for a craze client's
// reconstruction of the Go error, never for retry: that is the code's alone.
type Reason string

// The engine's reasons: classify's third column (internal/engine/control.go),
// the sentinel's own name in snake case. A code with one sentinel has the
// code itself as its one reason.
const (
	ReasonNotAccepting      Reason = "not_accepting"
	ReasonNotInTurn         Reason = "not_in_turn"
	ReasonCommandAborted    Reason = "command_aborted"
	ReasonSetOutcomeUnknown Reason = "set_outcome_unknown"
	ReasonBadCatalog        Reason = "bad_catalog"
	ReasonContext           Reason = "context"
	ReasonLogBackedUp       Reason = "log_backed_up"
	ReasonAskUnavailable    Reason = "ask_unavailable"
	ReasonSetUnavailable    Reason = "set_unavailable"
	ReasonNotRun            Reason = "not_run"
	ReasonAttachRaced       Reason = "attach_raced"
	ReasonOptionGone        Reason = "option_gone"
	ReasonFailed            Reason = "failed"
	ReasonBadRequest        Reason = "bad_request"
	ReasonBadAnswer         Reason = "bad_answer"
	ReasonUnsupported       Reason = "unsupported"
	ReasonUnknownAsk        Reason = "unknown_ask"
	ReasonAlreadySubmitted  Reason = "already_submitted"
	ReasonAlreadyResolved   Reason = "already_resolved"
	ReasonQueueFull         Reason = "queue_full"
	ReasonTextTooLong       Reason = "text_too_long"
	ReasonPromptInFlight    Reason = "prompt_in_flight"
	ReasonForeignTurn       Reason = "foreign_turn"
	ReasonPromptCancelled   Reason = "prompt_cancelled"
	ReasonStaleVersion      Reason = "stale_version"
	ReasonStaleTurn         Reason = "stale_turn"
	ReasonUnknownRow        Reason = "unknown_row"
	ReasonUnknownCommand    Reason = "unknown_command"
	ReasonInProgress        Reason = "in_progress"
	ReasonStaleModel        Reason = "stale_model"
	ReasonIndexWrite        Reason = "index_write"
	ReasonUnknownSubagent   Reason = "unknown_subagent"
)

// The protocol's own reasons: refusals no engine is involved in (plan 027
// §3.2–§3.9).
const (
	// ReasonUnknownSession is a sessionId this endpoint does not serve.
	ReasonUnknownSession Reason = "unknown_session"
	// ReasonStartFailed answers a when: "ready" attach, or a command, on a
	// session whose start failed; data.cause carries the failure (§3.4).
	ReasonStartFailed Reason = "start_failed"
	// ReasonNotReady is a when: "ready" attach that waited the server's own
	// bound, 10 minutes, for the session to come up (§3.4). Never stored.
	ReasonNotReady Reason = "not_ready"
	// ReasonBusy is a command past the host's cap of commands in flight
	// (CommandsPerHost, §3.6): not run, never stored.
	ReasonBusy Reason = "busy"
	// ReasonResponseTooLarge replaces a reply longer than OutboundLineMax
	// (§3.2); nothing is ever truncated silently.
	ReasonResponseTooLarge Reason = "response_too_large"
	// ReasonSnapshotTooLarge is a snapshot the budget cannot hold
	// (transcript.ErrSnapshotTooLarge): the mandatory state alone, or it and
	// the newest entry, over the byte budget (§3.4).
	ReasonSnapshotTooLarge Reason = "snapshot_too_large"
	// ReasonHelloRequired is any method but hello before hello (§3.2).
	ReasonHelloRequired Reason = "hello_required"
	// ReasonUnknownField is a params field the method does not define: -32602,
	// with the field named in the message (§3.2). hello never answers it.
	ReasonUnknownField Reason = "unknown_field"
	// ReasonLineTooLong is a line over InboundLineMax: -32600, id null, and
	// the connection stays open (§3.2).
	ReasonLineTooLong Reason = "line_too_long"
	// ReasonProtocolVersion is a hello that shares no protocol version with
	// the host; data.result is {supported} (HelloErrorResult, §3.3).
	ReasonProtocolVersion Reason = "protocol_version"
	// ReasonBadToken is a hello resume whose token does not match (§3.6).
	ReasonBadToken Reason = "bad_token"
	// ReasonAlreadyAttached is a second session.attach while the connection's
	// attachment is not yet closed: one live attachment per connection
	// (SQ14, §3.3, §3.7).
	ReasonAlreadyAttached Reason = "already_attached"
	// ReasonUnknownMethod is a method the host does not know: -32601.
	ReasonUnknownMethod Reason = "unknown_method"
	// ReasonStopUnsupported is session.stop on a TUI-hosted session, whose
	// capability says stop: false (§3.9).
	ReasonStopUnsupported Reason = "stop_unsupported"
	// ReasonRosterUnsupported is sessions.subscribe on a host
	// (rosterSubscribe: false); the hub defines it (S4).
	ReasonRosterUnsupported Reason = "roster_unsupported"
	// ReasonHubOnly is session.connect on a per-session host — the client is
	// already there (§3.3's hub splice) — and session.create, which only the
	// hub serves (S4; X6).
	ReasonHubOnly Reason = "hub_only"
)

// The client-side reasons (plan 027 §3.2): a client names these outcomes
// itself, for wording, and no host ever sends one — they are listed so a
// client never mistakes one for a host's answer. None has a code of its own
// (X6): nothing on the wire said anything, and they are the client's
// ErrOutcomeUnknown's and the TUI gate's.
const (
	// ReasonResumeLost is internal/remote's ErrOutcomeUnknown after a
	// reconnect the host answered resumed: false — or a call refused before
	// sending because its chain's backend epoch is stale: the command may have
	// run, nothing refused it, and the client re-reads state (§3.14, §3.12).
	ReasonResumeLost Reason = "resume_lost"
	// ReasonDisconnected is ErrOutcomeUnknown once the client's redials are
	// spent (§3.14).
	ReasonDisconnected Reason = "disconnected"
	// ReasonNoAnswer is the TUI gate's deadline passing with no reply:
	// outcome unknown (§3.12).
	ReasonNoAnswer Reason = "no_answer"
)

// ReasonInfo is one row of the reason table: a reason, the one code a host
// sends it under, and whether it belongs to the engine or to the protocol.
// A client-side reason has no code and ClientSide set.
type ReasonInfo struct {
	Reason Reason
	// Code is the code a host sends Reason beside, "" for a client-side
	// reason.
	Code Code
	// Engine says the reason is one of classify's (engine.Reason); otherwise
	// it is the protocol's own, or the client's.
	Engine bool
	// ClientSide says no host ever sends it.
	ClientSide bool
}

// reasons is plan 027 §3.2's table, every row: the reasons a host sends,
// under their codes, then the client-side ones. A code with a single reason
// sends the code itself.
var reasons = []ReasonInfo{
	{ReasonNotAccepting, CodeNotAccepting, true, false},
	{ReasonNotInTurn, CodeNotAccepting, true, false},
	{ReasonStartFailed, CodeNotAccepting, false, false},

	{ReasonCommandAborted, CodeAborted, true, false},
	{ReasonSetOutcomeUnknown, CodeAborted, true, false},
	{ReasonBadCatalog, CodeAborted, true, false},
	{ReasonContext, CodeAborted, true, false},

	{ReasonLogBackedUp, CodeUnavailable, true, false},
	{ReasonAskUnavailable, CodeUnavailable, true, false},
	{ReasonSetUnavailable, CodeUnavailable, true, false},
	{ReasonNotRun, CodeUnavailable, true, false},
	{ReasonAttachRaced, CodeUnavailable, true, false},
	{ReasonNotReady, CodeUnavailable, false, false},
	{ReasonBusy, CodeUnavailable, false, false},

	{ReasonOptionGone, CodeFailed, true, false},
	{ReasonFailed, CodeFailed, true, false},
	{ReasonResponseTooLarge, CodeFailed, false, false},
	{ReasonSnapshotTooLarge, CodeFailed, false, false},

	{ReasonBadRequest, CodeBadRequest, true, false},
	{ReasonBadAnswer, CodeBadRequest, true, false},
	{ReasonHelloRequired, CodeBadRequest, false, false},
	{ReasonUnknownField, CodeBadRequest, false, false},
	{ReasonLineTooLong, CodeBadRequest, false, false},
	{ReasonProtocolVersion, CodeBadRequest, false, false},
	{ReasonBadToken, CodeBadRequest, false, false},
	{ReasonAlreadyAttached, CodeBadRequest, false, false},

	{ReasonUnsupported, CodeUnsupported, true, false},
	{ReasonUnknownMethod, CodeUnsupported, false, false},
	{ReasonStopUnsupported, CodeUnsupported, false, false},
	{ReasonRosterUnsupported, CodeUnsupported, false, false},
	{ReasonHubOnly, CodeUnsupported, false, false},

	// Every other code: the code itself, its one reason.
	{ReasonUnknownSession, CodeUnknownSession, false, false},
	{ReasonUnknownAsk, CodeUnknownAsk, true, false},
	{ReasonAlreadySubmitted, CodeAlreadySubmitted, true, false},
	{ReasonAlreadyResolved, CodeAlreadyResolved, true, false},
	{ReasonQueueFull, CodeQueueFull, true, false},
	{ReasonTextTooLong, CodeTextTooLong, true, false},
	{ReasonPromptInFlight, CodePromptInFlight, true, false},
	{ReasonForeignTurn, CodeForeignTurn, true, false},
	{ReasonPromptCancelled, CodePromptCancelled, true, false},
	{ReasonStaleVersion, CodeStaleVersion, true, false},
	{ReasonStaleTurn, CodeStaleTurn, true, false},
	{ReasonUnknownRow, CodeUnknownRow, true, false},
	{ReasonUnknownCommand, CodeUnknownCommand, true, false},
	{ReasonInProgress, CodeInProgress, true, false},
	{ReasonStaleModel, CodeStaleModel, true, false},
	{ReasonIndexWrite, CodeIndexWrite, true, false},
	{ReasonUnknownSubagent, CodeUnknownSubagent, true, false},

	{ReasonResumeLost, "", false, true},
	{ReasonDisconnected, "", false, true},
	{ReasonNoAnswer, "", false, true},
}

// Reasons is the whole table, in a fixed order: every reason a host sends,
// each under its one code, then the client-side ones.
func Reasons() []ReasonInfo { return slices.Clone(reasons) }

// Lookup is r's row of the table, and false for a reason the table does not
// hold.
func (r Reason) Lookup() (ReasonInfo, bool) {
	for _, info := range reasons {
		if info.Reason == r {
			return info, true
		}
	}
	return ReasonInfo{}, false
}

// Code is the code a host sends r beside, and "" for a client-side reason or
// one the table does not hold.
func (r Reason) Code() Code {
	info, _ := r.Lookup()
	return info.Code
}

// ClientSide reports whether r is one no host ever sends.
func (r Reason) ClientSide() bool {
	info, _ := r.Lookup()
	return info.ClientSide
}
