package engine

import (
	"context"
	"errors"

	"github.com/charliek/craze/internal/agent"
)

// Command names one mutating command: the client that issued it and that
// client's own number for it. The zero Command is a command nobody intends to
// resend and nobody needs to recognise the effects of: it is not idempotent,
// and the events it causes carry no Cause.
//
// It exists because a session host is born detached from S4 on and the TUI
// becomes a socket client for good (session control SD-33): a client that can
// lose its connection has to be able to ask "did my prompt go through?" by
// sending the command again, and a client that applies its own command's
// effect from the reply has to be able to tell that effect's echo from
// somebody else's change. In-process neither can happen yet, and the surface
// is already the one a socket will carry.
//
// # Resends and the receipts table (receipts.go)
//
// A non-zero Command is tracked by an in-memory table, bounded and per
// engine: it reserves an id while its command runs, matches a resend on a
// payload hash (the method plus its arguments) and, once the command has
// run, answers a matching resend with exactly what the first call returned —
// a refusal included — never a second execution. A mismatched resend (the
// same id, a different payload) is ErrBadRequest, and so is a Client this
// engine never minted. An id at or below its client's evicted high-water mark
// is ErrUnknownCommand: recognisably expired, never a fresh (and very
// different) command running under a number that used to mean something else.
//
// A resend that arrives while the first call is STILL RUNNING is answered
// according to what that command does. A blocking one (Cancel, Stop, Set,
// Interject) is waited for, and the resend gets its result. A synchronous one
// — every method documented below as waiting on nothing — is never waited
// for: the resend is refused with ErrCommandInProgress at once, having
// changed nothing, and may simply ask again. That is what keeps "a duplicate
// of a synchronous command never waits" literally true even when the command
// itself stalls, which the index write inside Submit and SetTitle can. One
// client cannot reach it in process — the TUI makes every call from its one
// Update goroutine — and a socket client with two connections in flight can.
//
// # The retry policy, by code
//
// 05-protocol.md's codes are a closed set, and a client decides what a
// resend means from the code ALONE, never by matching message text — the one
// thing a socket client (session control S2) needs from this table that the
// TUI, with every call running through its one Update goroutine, never has
// to ask:
//
//   - in_progress (ErrCommandInProgress): this id is RESERVED and its
//     command is still running — see the paragraph above. Resend THE SAME
//     id; a new id would be a SECOND command layered on top of the one still
//     in flight. Unreachable in process, so the TUI has, and needs, no
//     handling for it.
//   - unavailable (ErrUnavailable and its sibling gate sentinels,
//     agent.ErrSetUnavailable / agent.ErrAskUnavailable, and errNotRun for a
//     Set answered without running because its own ctx was already dead) or
//     not_accepting (ErrNotAccepting, and agent.ErrNotInTurn — an Interject
//     with no turn to merge into): a GATE refusal — the engine simply not
//     admitting anything at all right now, this command's own arguments
//     aside — and NEVER STORED: nothing ran, and the id is exactly as unseen
//     as before the attempt. Resend the same id, or send a new one; either
//     gets a genuine first attempt once the gate reopens rather than a cached
//     echo of finding it shut.
//   - stale_model (ErrStaleModel): a config change bound to a model the
//     session has since left, refused before the provider was asked — NEVER
//     STORED for the same reason as the gate refusals: nothing ran, and the
//     model can come back, so a resend of the same id is judged against the
//     model it finds. Unlike them it is about this command's own arguments,
//     which is why it has a code of its own: what a client says is "not
//     applied, the model changed", and it re-reads the catalog before
//     choosing again.
//   - aborted (ErrCommandAborted; ErrSetOutcomeUnknown for the one Set whose
//     own outcome a context ending after the settings worker's claim leaves
//     honestly unknown; and a plain context.Canceled or
//     context.DeadlineExceeded from a command that RAN — a Cancel or Stop
//     whose session/cancel gave up, an Interject whose deadline passed after
//     its request was written): a STORED answer whose outcome the engine
//     cannot itself vouch for — the command may already have mutated state,
//     or its request may already be on the wire — so this id will never run
//     again and replays the same answer for as long as the table keeps it.
//     Re-read state (or, for a Set, watch the stream for the change's own
//     delta) before deciding anything else; send a NEW id if the command is
//     still wanted, and never blindly resend the work under a new id without
//     looking, or an interjection the agent already queued goes in twice
//     (r30 finding 1).
//   - failed: every OTHER error craze does not classify by its own sentinel —
//     a provider/RPC refusal of a Set, an Interject the agent refused, a
//     Cancel that failed — reached this way because the command DID run and
//     its failure is what it ran into, and its failure is a definite one: a
//     STORED, stable answer, exactly like the named refusals below, and never
//     a reason to retry the same id. Send a NEW id for another attempt.
//   - anything else: the command's own stored, stable answer — a success, or
//     a refusal about THIS request or the specific resource it named (a
//     stale turn, a bad answer, a stale queue version, an unknown row, a
//     full queue, an already-pending send, a foreign turn, …) — replayed
//     exactly as the first call got it.
//
// classify (below) is the one table Code and gateRefusal (receipts.go) both
// consult for these two questions — the code, and whether the answer is
// stored — so the two can never again say different things about the same
// error (r28 finding 1).
type Command struct {
	// Client is an id NewClientID minted, unique in the incarnation.
	Client string
	// ID is the client's own number for the command. Clients count from 1, so
	// two clients never collide and an id at or below a client's evicted
	// high-water mark is recognisably expired.
	ID string
}

// IsZero reports whether c names no command.
func (c Command) IsZero() bool { return c == Command{} }

// Cause is c as Event.Cause spells it, "client/id", and "" for the zero
// Command.
func (c Command) Cause() string {
	if c.IsZero() {
		return ""
	}
	return c.Client + "/" + c.ID
}

// SubmitMode is how a prompt is admitted, in the protocol's words
// (discovery/session-control/05-protocol.md).
type SubmitMode string

const (
	// SubmitQueue starts the prompt now when the engine is admitting, and
	// queues it otherwise. It is what Enter does.
	SubmitQueue SubmitMode = "queue"
	// SubmitSendNow starts the prompt now; while a turn is working it arms
	// the send, cancels that turn, and fires when it settles.
	SubmitSendNow SubmitMode = "send_now"
)

// SubmitResult is what became of a submitted prompt: exactly one of its
// fields is set.
type SubmitResult struct {
	// Turn is the id of the turn the prompt started, in this call. It is the
	// one turn id a client is handed synchronously, and therefore the one
	// EventTurn{started} whose effect the client has already applied and must
	// skip: every other started — a drained row, its own included, or an armed
	// send firing — arrives only as an event.
	Turn string
	// Queued is the row the prompt became, or — for a submit that named a row
	// which cannot start yet — that row, unchanged and still waiting.
	Queued *agent.QueuedPrompt
	// Armed reports that a send-now was armed against the running turn: nothing
	// has left the queue or the composer, and the send fires, or disarms with a
	// reason, when that turn settles.
	Armed bool
}

// CancelOutcome is what a cancel the engine accepted came to.
type CancelOutcome string

const (
	// CancelRequested: the cancel reached the agent, or the claimed prompt
	// was withdrawn before it got there, and the turn had not ended by the
	// time the call returned. Its ending arrives as an EventTurn.
	CancelRequested CancelOutcome = "requested"
	// CancelSettled: the turn had ended by the time the call returned, or
	// there was no turn of craze's own to end.
	CancelSettled CancelOutcome = "settled"
	// CancelUnknown: the call gave up — its context ended, or the write
	// failed — at a point where a cancel may already have been written. It is
	// the honest answer a timeout past the write has.
	CancelUnknown CancelOutcome = "unknown"
)

// CancelResult is a cancel's outcome against the turn it was for. A cancel
// the engine refused has no result: it comes back as ErrStaleTurn or
// ErrNotAccepting.
type CancelResult struct {
	Outcome CancelOutcome
	// Turn is the engine turn the cancel was held against, "" when it was
	// accepted with no turn of craze's own running.
	Turn string
	// Reported says the failure this result accompanies — the error returned
	// beside it — has ALSO been published, as a state delta carrying the reason
	// cancel_failed and the failure in Detail. It is set only when this cancel
	// disarmed a send-now, because that is the one case where the engine owed an
	// event anyway: the delta says what was lost, and a client that words a
	// failure from a delta would otherwise word this one twice, once from the
	// delta and once from the error here — in whichever order the two reached it.
	//
	// A client that draws failures from deltas must not draw this one again. A
	// client that does not (`craze prompt` exits on it) may ignore the field: the
	// error is returned either way.
	Reported bool
}

// The refusals the engine adds to the ones internal/agent defines (agent.ErrQueueFull,
// agent.ErrQueueTextTooLong, agent.ErrPromptInFlight, agent.ErrForeignTurn,
// agent.ErrNotInTurn, agent.ErrUnsupported). Each has a protocol code, which
// is what a client matches on: never the message text.
var (
	// ErrNotAccepting refuses a command the engine's activity does not admit:
	// anything before the session is up or while it restores, anything after
	// Stop or Close, a cancel with nothing to cancel.
	ErrNotAccepting = errors.New("engine: not accepting that now")
	// ErrAlreadyPending refuses a second send-now while one is armed.
	ErrAlreadyPending = errors.New("engine: a send-now is already pending")
	// ErrStaleTurn refuses a cancel that names a turn which is no longer the
	// current one, so a cancel delayed across a queue transition cannot land
	// on the turn that started next.
	ErrStaleTurn = errors.New("engine: that turn is no longer current")
	// ErrStaleVersion refuses a queue edit conditioned on a version the row
	// no longer has.
	ErrStaleVersion = errors.New("engine: the queued message changed")
	// ErrStaleModel refuses a config change bound to a model (Setting.ForModel)
	// when the session is on another model by the time the settings worker
	// comes to it: the option was chosen from that model's catalog, and is not
	// a choice anyone made for this one (plan 025 design 3, panel astra 4).
	// Nothing was asked of the provider. Its code is "stale_model", and it is
	// NEVER STORED, like the gate refusals: nothing ran, and the condition it
	// names is one that can stop being true — the model can come back — so the
	// same id resent is a genuine attempt judged against the model then, never
	// a replay of this answer (Command's retry policy).
	ErrStaleModel = errors.New("engine: the model that option was chosen for is no longer the session's")
	// ErrUnknownRow answers a queued row id the queue does not hold: one the
	// drain already sent, one another client removed, or one that never
	// existed. It is its own code and not bad_request, because a client's
	// answer to it is to re-read the queue rather than to fix its call.
	ErrUnknownRow = errors.New("engine: no such queued message")
	// ErrUnavailable refuses a command before it mutates anything because the
	// event log's outbox has no room for the events it would cause.
	ErrUnavailable = errors.New("engine: the event log is backed up")
	// ErrBadRequest refuses a malformed command, and a command id resent with
	// a different payload: never a re-execution.
	ErrBadRequest = errors.New("engine: bad request")
	// ErrUnknownCommand answers a command id outside the retry horizon.
	ErrUnknownCommand = errors.New("engine: that command id has expired")
	// ErrCommandInProgress refuses a resend of a SYNCHRONOUS command (Command's
	// own doc says which) that found its reservation still open: the first
	// call has not returned, this attempt ran nothing and changed nothing, and
	// the id stays exactly as reserved as it was. Resend THE SAME id; a new id
	// would be a second command layered on top of the one still running. Its
	// code is "in_progress", the one code in the closed set that means that
	// rather than "this attempt is over, try again however you like"
	// (ErrUnavailable's and ErrNotAccepting's meaning) — see Command's own doc
	// for the whole retry policy, by code.
	ErrCommandInProgress = errors.New("engine: that command is still running")
	// ErrSetOutcomeUnknown answers a Set whose caller's context ended after the
	// settings worker had already CLAIMED the request: the provider has the
	// change, or is about to, and whether it landed cannot be said from the
	// caller's side — internal/acp writes a request before it can look at its
	// context at all (conn.go's callRaw), so a cancellation past that point
	// cannot honestly mean "nothing happened". It is the settings verb's
	// CancelUnknown: the stream is what says, and the change's own delta
	// arrives if it landed.
	//
	// It always WRAPS the context's own error, so errors.Is(err,
	// context.DeadlineExceeded) and errors.Is(err, context.Canceled) still hold
	// for a caller that matches on those. Its code is "aborted", shared with
	// ErrCommandAborted: both are STORED answers whose outcome the engine
	// cannot itself vouch for — see Command's own doc for the whole retry
	// policy, by code.
	ErrSetOutcomeUnknown = errors.New("engine: the settings change may or may not have landed")
	// ErrIndexWrite answers a command that DID what it was asked to do and
	// could not write it down: today the one such command is SetTitle, which
	// renames and pins the session in craze and then records the new name in
	// ~/.craze/sessions.jsonl for --continue and --resume to find. It always
	// WRAPS the write's own failure, so errors.Is on that still holds.
	//
	// It is its own sentinel because the two halves need different words from
	// a client: the rename HAPPENED and is to be said so, and beside it the
	// index could not be written, which is a note and never a reason to stop
	// (plan 021 §2.4). A client that could not tell them apart would have to
	// choose between claiming a rename that did not happen and hiding one that
	// did.
	//
	// Its code is "index_write", which 05-protocol.md does not list yet (C14
	// adds it, beside unknown_row, in_progress and aborted). A resend of the
	// SAME id replays this answer: the title is already set, so re-running the
	// command would be a second rename. A client that wants the write attempted
	// again sends a NEW id — or simply carries on, since the next index write
	// of any kind records the pinned title along with everything else.
	ErrIndexWrite = errors.New("engine: the session index could not be written")
	// ErrCommandAborted answers a command id whose call did not return at all:
	// it panicked on the way through. Whether it changed anything is not
	// knowable, so the id is answered with this for as long as the table keeps
	// it — never re-executed — and a client that wants another attempt sends a
	// new id. Its code is "aborted": the closed set's word for "this answer is
	// stored, but the outcome it stores is not knowable — re-read state before
	// deciding anything, and use a new id if you still want the command,"
	// because 05 has no code of its own for a craze bug.
	ErrCommandAborted = errors.New("engine: the command's outcome is unknown")
)

// classification is one error's place in the closed set: the protocol code a
// client matches on (05-protocol.md), and whether the receipts table STORES
// the answer. classify is the ONE table Code and gateRefusal (receipts.go)
// both consult — neither decides either question on its own — so they cannot
// drift apart again the way r28 finding 1 found them: Code said not_accepting
// for agent.ErrNotInTurn while gateRefusal did not know it, and a command
// whose docs promise "never stored" was stored anyway.
//
// The invariant this exists to hold, over the WHOLE set: stored is false if
// and only if code is one of unavailable, not_accepting, in_progress or
// stale_model — the four codes Command's own doc promises are never stored.
// Nothing here decides that per case; it falls out of which four codes appear
// with stored: false below, and classify_test.go asserts it holds for every
// sentinel this switch names.
type classification struct {
	code   string
	stored bool
}

func classify(err error) classification {
	switch {
	case err == nil:
		return classification{code: ""}
	case errors.Is(err, ErrIndexWrite):
		// First among the sentinels, because this one WRAPS a failure from
		// outside craze — a file error, whatever the filesystem said — and
		// nothing below may be allowed to answer for it. STORED: the rename
		// happened, and a resend must replay that fact rather than attempt a
		// second rename (ErrIndexWrite's own doc, C12).
		return classification{code: "index_write", stored: true}
	case errors.Is(err, ErrNotAccepting), errors.Is(err, agent.ErrNotInTurn):
		// agent.ErrNotInTurn is an Interject with no turn: a GATE refusal
		// exactly like ErrNotAccepting's — the engine has nothing to act on
		// right now, whatever this command's own arguments are — so it is
		// NEVER STORED (r28 finding 1).
		return classification{code: "not_accepting"}
	case errors.Is(err, ErrCommandInProgress):
		// Never stored, but by a different mechanism: a duplicate that finds
		// this reservation open is answered before finish is ever called, so
		// gateRefusal is never actually consulted for it in production — it is
		// classified false here anyway, for the one table's sake.
		return classification{code: "in_progress"}
	case errors.Is(err, ErrAlreadyPending):
		return classification{code: "already_submitted", stored: true}
	case errors.Is(err, ErrStaleTurn):
		return classification{code: "stale_turn", stored: true}
	case errors.Is(err, ErrStaleVersion):
		return classification{code: "stale_version", stored: true}
	case errors.Is(err, ErrUnknownRow):
		return classification{code: "unknown_row", stored: true}
	case errors.Is(err, agent.ErrBadAnswer):
		return classification{code: "bad_request", stored: true}
	case errors.Is(err, agent.ErrAlreadyResolved):
		return classification{code: "already_resolved", stored: true}
	case errors.Is(err, agent.ErrUnknownAsk):
		return classification{code: "unknown_ask", stored: true}
	case errors.Is(err, ErrCommandAborted), errors.Is(err, ErrSetOutcomeUnknown):
		// Both are STORED answers whose outcome the engine cannot itself vouch
		// for — a panic's, or a Set the worker had already claimed when its
		// caller's context ended — spelled out rather than left to the
		// default so a client can tell them from an ordinary "unavailable, try
		// again" without matching text: see Command's own doc for the policy.
		return classification{code: "aborted", stored: true}
	case errors.Is(err, errNotRun):
		// A Set answered WITHOUT running because its ctx was already dead when
		// takeSet or runSet looked (settings.go): nothing happened, so — like
		// every other gate refusal — it is NEVER STORED, and unavailable is
		// its code because a client can only retry it (r28 finding 1).
		return classification{code: "unavailable"}
	case errors.Is(err, ErrStaleModel):
		// Refused before the claim, with nothing asked of the provider
		// (runSet), about a condition that can clear — so NEVER STORED, like
		// the gate refusals, but with a code of its own: a client needs to say
		// "not applied, the model changed", which "unavailable" cannot.
		return classification{code: "stale_model"}
	case errors.Is(err, agent.ErrOptionGone):
		// A Set that RAN: the agent took it and answered with a catalog that
		// no longer lists the option, and that catalog is installed and
		// announced. A plain, definite failure of this request — STORED, the
		// code every other failed provider answer gets.
		return classification{code: "failed", stored: true}
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		// A command that RAN and then gave up on its own context: Cancel's or
		// Stop's session/cancel, an Interject whose deadline passed. Such a
		// call cannot say that nothing happened — internal/acp writes a request
		// to the pipe before callRaw can look at its context at all (conn.go),
		// so the interjection may already be queued and the cancel may already
		// have been taken, which is exactly what CancelUnknown says out loud —
		// and "failed" would invite a client to send the same work under a NEW
		// id and do it twice. aborted is the honest code: STORED, so this id
		// never runs again, and the client re-reads state before deciding
		// anything (r30 finding 1).
		//
		// Everything that ended on a dead context WITHOUT running is named
		// ABOVE and never reaches here: a Set the caller took back out of the
		// queue, or one takeSet or runSet found dead, wraps errNotRun, and one
		// the worker had already claimed wraps ErrSetOutcomeUnknown. Both wrap
		// the context's own error too, and both are matched first, so the order
		// of these cases is what keeps "not run" and "ran, outcome unknown"
		// apart.
		return classification{code: "aborted", stored: true}
	case errors.Is(err, agent.ErrAskUnavailable), errors.Is(err, agent.ErrSetUnavailable), errors.Is(err, ErrUnavailable):
		return classification{code: "unavailable"}
	case errors.Is(err, ErrBadRequest):
		return classification{code: "bad_request", stored: true}
	case errors.Is(err, ErrUnknownCommand):
		return classification{code: "unknown_command", stored: true}
	case errors.Is(err, agent.ErrQueueFull):
		return classification{code: "queue_full", stored: true}
	case errors.Is(err, agent.ErrQueueTextTooLong):
		return classification{code: "text_too_long", stored: true}
	case errors.Is(err, agent.ErrPromptInFlight):
		return classification{code: "prompt_in_flight", stored: true}
	case errors.Is(err, agent.ErrForeignTurn):
		return classification{code: "foreign_turn", stored: true}
	case errors.Is(err, agent.ErrPromptCancelled):
		return classification{code: "prompt_cancelled", stored: true}
	case errors.Is(err, agent.ErrUnsupported):
		return classification{code: "unsupported", stored: true}
	default:
		// Every error the switch above does not name is a command that RAN
		// and failed — a provider/RPC refusal of a Set, an Interject the
		// agent refused, a Cancel that failed — never a gate refusal (r28
		// finding 1). "failed" says so: STORED, because the command already
		// ran, and never "unavailable", which the docs promise a client is
		// never told about a command that actually happened.
		return classification{code: "failed", stored: true}
	}
}

// Code is err's protocol error code, the closed set 05-protocol.md lists, and
// "" for nil. An error the engine does not know is "failed": the command ran
// and this is its stored, stable answer, never a reason to retry the same id.
func Code(err error) string { return classify(err).code }

// Control is the surface a client drives a session through: the TUI and
// `craze prompt` hold one now, and the socket server and its clients will
// (session control S2). It grows with the engine; this is the driver's part.
//
// Which methods wait is part of the contract, because a bubbletea Update is
// the primary's own reader and must never wait on anything it would have to
// read to release. Submit, Disarm, GiveUp, GiveUpDrain, the queue verbs, Asks,
// Ask, Answer, SetTitle, State, NewClientID and Events wait on nothing: no
// channel, no provider call, no Publish. Start, Subscribe, Interject, Cancel,
// Stop, Set, Sync and Close block and belong on a goroutine that is not the
// primary's reader — a tea.Cmd. Subscribe is among
// them because it registers inside the log's publishing boundary, which a
// publisher holds while it waits for room in the primary: called by the
// primary's own reader with the primary full, it would wait for a slot only it
// can free. Interject is the one exception in use today: the TUI calls it from
// Update, as it always has.
//
// A send-now is the one command whose provider call the engine makes for the
// caller: Submit arms it and returns at once, and the cancel that makes room
// for it is made on a goroutine of the engine's own.
//
// A command's effects are events, and events are published by the log's
// outbox, so a command returning does not mean its events have been
// delivered. A client applies its own command's effect from the return value
// and skips exactly that effect's echo; a caller that needs "everything this
// caused is delivered" calls Sync.
type Control interface {
	Start(ctx context.Context) error
	// Events is the session's primary subscription, exactly as the session's
	// own Events() was. A primary client must keep reading until it closes
	// the engine: the outbox may still be publishing after a turn's ending.
	Events() <-chan agent.Event
	Subscribe(agent.SubscribeOptions) (*agent.Subscription, error)
	State() State
	NewClientID() string

	Submit(c Command, text string, mode SubmitMode, fromRow string) (SubmitResult, error)
	// Disarm takes back an armed send-now, leaving its text where it was.
	Disarm(c Command) error
	Interject(ctx context.Context, c Command, text string) error
	Cancel(ctx context.Context, c Command, turn string) (CancelResult, error)
	// Stop refuses every later admission, clears the queue, and cancels what
	// is running. It is what a signal does to `craze prompt`.
	Stop(ctx context.Context, c Command) error
	// GiveUp ends the engine's wait on a turn whose claim the agent keeps
	// refusing because it is running a turn of its own (State.Waiting): that
	// turn settles as the refusal it was, and nothing else of the session's is
	// touched — no cancel is written and the queue is kept. It is conditional
	// and atomic: ErrStaleTurn for a turn that is not current, ErrNotAccepting
	// for one that is no longer waiting, and in both cases nothing changes,
	// because a client decides from an observation and the claim may have gone
	// through since.
	GiveUp(c Command, turn string) error
	// GiveUpDrain is GiveUp's counterpart for rows held behind a turn of the
	// agent's own: the drain is made now, in this call, or it is abandoned for
	// good. It answers with the turn that is current when it returns — the
	// drain's, or one that was already running — and then nothing has been given
	// up; or with "" and how many rows are still queued, and then the engine
	// admits nothing further, so no row can start behind a client that is
	// leaving. It is conditional and atomic for the same reason GiveUp is: the
	// session's flag is read in the section that would claim, not by the client
	// beforehand. It waits on nothing.
	GiveUpDrain(c Command) (turn string, pending int, err error)

	// The asks the session is holding, one of them by id, and the one verb that
	// answers them. None waits, and none is gated on the engine's activity: an
	// agent blocked on a question is waiting whatever else is going on.
	Asks() []agent.AskRecord
	Ask(id string) (agent.AskRecord, bool)
	Answer(c Command, id string, a agent.AskAnswer) error

	// The session's settings. Set blocks — one FIFO worker asks the provider,
	// the session writes the change and its delta in one locked section, and
	// the answer carries the confirmed value and the delta's revision — and it
	// is bounded by its ctx throughout: a request still queued is answered with
	// that context's error having changed nothing, and one the worker has
	// claimed with ErrSetOutcomeUnknown, which is the honest answer once a
	// request may already be on the wire.
	//
	// SetTitle asks no provider, so it is not queued behind anything; it does
	// write the session's index row, which is file I/O on the caller's own
	// goroutine (§3.2's documented exception, where the TUI has always done
	// it). A write that failed comes back as ErrIndexWrite wrapping it, and
	// means the session WAS renamed and only the record of it was not written.
	Set(ctx context.Context, c Command, s Setting) (SetResult, error)
	SetTitle(c Command, title string) error

	// The queue's verbs. None of them starts a turn, and none of them waits.
	Queue(c Command, text string) (agent.QueuedPrompt, error)
	// EditQueued rewrites a row in place. expectedVersion is the
	// check-and-edit: nil is unconditional, and a version the row no longer
	// has is ErrStaleVersion.
	EditQueued(c Command, id, text string, expectedVersion *int) error
	Unqueue(c Command, id string) (agent.QueuedPrompt, error)
	ClearQueue(c Command) (int, error)

	// Sync returns once every event enqueued before the call has been
	// delivered: it is in the primary's buffer, or committed when there is no
	// primary. It must not be called from the primary's reader unless another
	// goroutine is reading.
	Sync(ctx context.Context) error
	Close() error
}

var _ Control = (*Engine)(nil)
