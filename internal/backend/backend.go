package backend

import (
	"context"
	"errors"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/transcript"
)

// ErrClosed is what Read returns once the stream has ended: nothing more will
// ever be read from this backend.
var ErrClosed = errors.New("backend: the stream has ended")

// ErrStaleEpoch is a call refused before anything was sent, because the epoch
// it carries (WithEpoch) is not the one the backend is bound to now: it was
// dispatched for a session the backend has since left (plan 027 §3.12, "Chains
// are fenced in the backend too"; astra r3 13). A step of a chain issued for
// session A therefore never executes against session B.
//
// In process the epoch never moves, so an engine-backed call is never refused.
// A socket backend (PR 4) that reconnected to another incarnation answers the
// refusal as its ErrOutcomeUnknown with the reason resume_lost, wrapping this
// sentinel, so a caller matching either finds it: the command was not sent on
// the new binding, and what became of anything sent on the old one is unknown.
var ErrStaleEpoch = errors.New("backend: the call was for a session this backend is no longer bound to")

// epochKey is WithEpoch's context key.
type epochKey struct{}

// WithEpoch is ctx carrying epoch e: the Backend.Epoch its caller read when it
// dispatched the call, in the Update, so a call that runs later — a gated
// call's goroutine, a chain's next step — is refused if the backend has moved
// on since (CheckEpoch).
func WithEpoch(ctx context.Context, e uint64) context.Context {
	return context.WithValue(ctx, epochKey{}, e)
}

// EpochFrom is the epoch ctx carries, and whether it carries one.
func EpochFrom(ctx context.Context) (uint64, bool) {
	e, ok := ctx.Value(epochKey{}).(uint64)
	return e, ok
}

// CheckEpoch is the fence every backend call passes before it sends anything:
// ErrStaleEpoch when ctx carries an epoch that is not current, the epoch the
// backend is bound to now; nil otherwise. A ctx with no epoch is not fenced —
// a call whose caller captured none (a test's, a lifecycle call) goes as it
// always has.
func CheckEpoch(ctx context.Context, current uint64) error {
	if e, ok := EpochFrom(ctx); ok && e != current {
		return ErrStaleEpoch
	}
	return nil
}

// Backend is the session as the TUI drives it (plan 027 §3.12): its lifecycle,
// its one ordered stream, the commands engine.Control carries — with a
// context first — and the reads the TUI makes.
//
// The error a command answers is the engine's own: its sentinels
// (engine.ErrNotAccepting, agent.ErrAskUnavailable, …) and their wrapped
// causes reach the caller unchanged in process, and a socket backend
// reconstructs them from the wire's reasons, so a caller matches by sentinel
// exactly as it always has.
type Backend interface {
	// Start starts the session; until it returns, the backend admits no
	// command. It is called from a tea.Cmd, never from Update.
	Start(ctx context.Context) error
	// Started says the session is up (err nil) or failed to come up: the
	// route the model learns the start on, idempotent with Start's own
	// (engine.Engine.Started). It waits on nothing.
	Started(err error)
	// Close ends the session and answers with what the session's own close
	// said (agent.ErrAgentExited included). It is idempotent, and it may
	// block until the agent is reaped, so it is never called from Update.
	Close() error
	// ClientID is this client's id on the session, the Client of every
	// engine.Command it sends. It is read per command, never cached by the
	// caller (§3.12): after a reconnect that could not resume, a remote
	// client has a new id. "" means commands name no client (the zero
	// engine.Command).
	ClientID() string
	// Epoch names the session the backend is bound to now (§3.12, "Chains
	// are fenced in the backend too"). It is constant in process — one
	// engine for the backend's life; a socket backend (PR 4) bumps it when a
	// reconnect lands on another incarnation, before any Restore is
	// delivered. A caller reads it when it dispatches a call and passes it
	// with every call of that operation (WithEpoch); a command or read whose
	// ctx carries another epoch is refused with ErrStaleEpoch before
	// anything is sent (CheckEpoch). It waits on nothing.
	Epoch() uint64

	// Read is the stream: one item at a time, in order, from one reader.
	// ErrClosed once the stream has ended.
	//
	// A ctx that is done before an item is taken returns ctx.Err() and takes
	// nothing: the item is still there for the next Read. An item that was
	// taken is ALWAYS returned, with a nil error, even if ctx was cancelled
	// meanwhile — a done ctx and a ready item race, and either may win — so
	// a caller must never discard what a Read answers: an item it drops is
	// gone from the stream for good.
	Read(ctx context.Context) (Item, error)

	// Commands: engine.Control's, ctx first. In process the ones that wait
	// on nothing (Submit, Answer, Unqueue, EditQueued, ClearQueue, Disarm,
	// SetTitle, CancelSubagent) use ctx only for its epoch (CheckEpoch);
	// Interject, Set and Cancel wait, and are bounded by it. Every command
	// and read (Ask, Settings) is fenced by the epoch ctx carries.
	Submit(ctx context.Context, c engine.Command, text string, mode engine.SubmitMode, fromRow string) (engine.SubmitResult, error)
	Answer(ctx context.Context, c engine.Command, id string, a agent.AskAnswer) error
	Unqueue(ctx context.Context, c engine.Command, id string) (agent.QueuedPrompt, error)
	EditQueued(ctx context.Context, c engine.Command, id, text string, expectedVersion *int) error
	ClearQueue(ctx context.Context, c engine.Command) ([]agent.QueuedPrompt, error)
	Disarm(ctx context.Context, c engine.Command) error
	Interject(ctx context.Context, c engine.Command, text string) error
	SetTitle(ctx context.Context, c engine.Command, title string) error
	Set(ctx context.Context, c engine.Command, s engine.Setting) (engine.SetResult, error)
	Cancel(ctx context.Context, c engine.Command, turn string) (engine.CancelResult, error)
	CancelSubagent(ctx context.Context, c engine.Command, id string) error

	// Ask is the ask id names, and whether the session knows it. It waits on
	// nothing in process; over the socket it is asks.get, a round trip, so
	// the TUI calls it only through the command gate (C18c). The error is
	// the round trip's; (rec, false, nil) is "no such ask".
	Ask(ctx context.Context, id string) (agent.AskRecord, bool, error)
	// Settings is the session's current model, mode and config: a blocking
	// read, for tea.Cmds only, never Update (§3.12). In process it is
	// State().Snapshot's; over the socket, session.state's settings. It is
	// what the multi-Set chains judge their next step on.
	Settings(ctx context.Context) (Settings, error)

	// State is TRANSITIONAL, and in process only: the engine's live state,
	// which the TUI's mirror still reads until it is the fold. C21 deletes
	// it (§3.12 "Transitional", §3.13), and a socket backend never
	// implements it for real.
	State() engine.State
}

// Settings is the session's current settings (§3.12): the model, the mode and
// the config options the session holds now.
//
// It carries no provider. What an option means — which one is the effort,
// which the fast toggle — is the provider's local vocabulary, read from the
// provider table by name (§3.13), so a caller judging a step against these
// settings supplies the provider it captured itself.
type Settings struct {
	// Model is the session's current model id ("" before it has one).
	Model string
	// Mode is the session's current mode id ("" before it has one).
	Mode string
	// Config is the session's config options, in the order the agent
	// advertised them.
	Config []agent.ConfigOption
}

// ItemKind says what an Item carries.
type ItemKind int

const (
	// ItemEvent is one event of the session's stream: Event, and Gen.
	ItemEvent ItemKind = iota + 1
	// ItemReady says the session is up (remote only): Info is its facts as
	// the host read them once it was ready (§3.4).
	ItemReady
	// ItemRestore replaces what the client holds (remote only): Info, and
	// Snapshot, the transcript as the host holds it (§3.4, §3.14).
	ItemRestore
	// ItemEnd is the stream's last item (remote only): Err says why it ended.
	ItemEnd
)

// Item is one thing Read delivers.
//
// In process only ItemEvent is ever delivered: the engine's primary is the
// whole stream, and it never ends while the program reads it. PR 4's remote
// backend delivers the rest, and may add fields.
type Item struct {
	Kind ItemKind
	// Event is the event an ItemEvent carries.
	Event agent.Event
	// Gen is the stream generation Event came from: a remote client's
	// attachment, which a restore replaces. 0 in process.
	Gen uint64
	// Info is the session's facts, on ItemReady and ItemRestore.
	Info SessionInfo
	// Snapshot is the transcript an ItemRestore restores.
	Snapshot *transcript.Snapshot
	// Err is why the stream ended, on ItemEnd.
	Err error
}

// SessionInfo is the session's static facts (§3.13), fixed once the session
// is up. It mirrors the wire's session info document (protocol.SessionInfo)
// in engine and agent types; before the session is ready its catalogs are
// empty and its ProviderSessionID may be "".
type SessionInfo struct {
	// CrazeSessionID is the durable craze session id (SD-22): what every
	// session-scoped method names, surviving a session/load into a new agent
	// session and a host restart.
	CrazeSessionID string
	// ProviderSessionID is the agent's own id for the session, which a
	// session/load changes.
	ProviderSessionID string
	// Incarnation is the event log's id: the scope of seqs, turn ids and ask
	// ids.
	Incarnation string
	// Workspace is the session's working directory.
	Workspace string
	// Provider is the session's provider's name ("cursor", "grok", "gx",
	// "native"), and Label the name a client shows for it.
	Provider string
	Label    string
	// Capabilities is the session's capability set AS ADVERTISED by whatever
	// serves it — over the socket the host's — and never rebuilt from the
	// client binary's own provider table (§3.13, astra 25).
	Capabilities agent.Capabilities
	// Models and Modes are what the session can be switched to: its model
	// and mode catalogs, empty until the session is ready.
	Models []agent.ModelInfo
	Modes  []agent.ModeInfo
	// RetryHorizon is the command-id table's bound: within it a resent
	// command id is answered from the table and never re-executes.
	RetryHorizon engine.RetryHorizon
}
