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
// — every method documented below as waiting on nothing — is never waited for:
// the resend is told ErrUnavailable ("that command is still running"), at
// once, having changed nothing, and may simply ask again. That is what keeps
// "a duplicate of a synchronous command never waits" literally true even when
// the command itself stalls, which the index write inside Submit and SetTitle
// can. One client cannot reach it in process — the TUI makes every call from
// its one Update goroutine — and a socket client with two connections in
// flight can.
//
// A command that PANICS is not retryable under the same id: its outcome is
// unknown (it may already have mutated state), so ErrCommandAborted is stored
// and replayed for that id, and another attempt needs a new one.
//
// One refusal is never stored: a GATE refusal — the engine simply not
// admitting anything at all right now (ErrNotAccepting, ErrUnavailable, and
// their sibling spellings agent.ErrSetUnavailable / agent.ErrAskUnavailable),
// whatever the command's own arguments — leaves the id exactly as unseen as
// before the attempt, so a client that retries the same id once the outbox
// has room, or the engine is admitting again, gets a genuine attempt rather
// than a cached echo of finding the door shut: it was never told a result
// worth caching, only that nothing happened yet. Every other refusal a
// command reaches only after being hashed and reserved — a stale turn, a bad
// answer, a stale queue version, an unknown row, a full queue, an
// already-pending send, a foreign turn, … — is a genuine, stable fact about
// THIS request or the specific resource it named, and is stored and replayed
// like any success.
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
	// ErrCommandAborted answers a command id whose call did not return at all:
	// it panicked on the way through. Whether it changed anything is not
	// knowable, so the id is answered with this for as long as the table keeps
	// it — never re-executed — and a client that wants another attempt sends a
	// new id. Its code is "unavailable", the closed set's word for "you can
	// only try again", because 05 has no code for a craze bug.
	ErrCommandAborted = errors.New("engine: the command's outcome is unknown")
)

// Code is err's protocol error code, the closed set 05-protocol.md lists, and
// "" for nil. An error the engine does not know is "unavailable": a client
// can only retry it.
func Code(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrNotAccepting), errors.Is(err, agent.ErrNotInTurn):
		return "not_accepting"
	case errors.Is(err, ErrAlreadyPending):
		return "already_submitted"
	case errors.Is(err, ErrStaleTurn):
		return "stale_turn"
	case errors.Is(err, ErrStaleVersion):
		return "stale_version"
	case errors.Is(err, ErrUnknownRow):
		return "unknown_row"
	case errors.Is(err, agent.ErrBadAnswer):
		return "bad_request"
	case errors.Is(err, agent.ErrAlreadyResolved):
		return "already_resolved"
	case errors.Is(err, agent.ErrUnknownAsk):
		return "unknown_ask"
	case errors.Is(err, agent.ErrAskUnavailable), errors.Is(err, agent.ErrSetUnavailable), errors.Is(err, ErrUnavailable),
		errors.Is(err, ErrCommandAborted):
		return "unavailable"
	case errors.Is(err, ErrBadRequest):
		return "bad_request"
	case errors.Is(err, ErrUnknownCommand):
		return "unknown_command"
	case errors.Is(err, agent.ErrQueueFull):
		return "queue_full"
	case errors.Is(err, agent.ErrQueueTextTooLong):
		return "text_too_long"
	case errors.Is(err, agent.ErrPromptInFlight):
		return "prompt_in_flight"
	case errors.Is(err, agent.ErrForeignTurn):
		return "foreign_turn"
	case errors.Is(err, agent.ErrPromptCancelled):
		return "prompt_cancelled"
	case errors.Is(err, agent.ErrUnsupported):
		return "unsupported"
	default:
		return "unavailable"
	}
}

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
	// the answer carries the confirmed value and the delta's revision — and
	// SetTitle waits on nothing, because craze owns the title and no provider
	// is asked.
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
