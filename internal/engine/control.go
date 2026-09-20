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
}

// The refusals the engine adds to the session's own (agent.ErrQueueFull,
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
// read to release. Submit, Disarm, the queue verbs, State, NewClientID and
// Events wait on nothing: no channel, no provider call, no Publish. Start,
// Subscribe, Interject, Cancel, Stop, Sync and Close block and belong on a
// goroutine that is not the primary's reader — a tea.Cmd. Subscribe is among
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
