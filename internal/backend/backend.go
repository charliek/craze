package backend

import (
	"context"
	"errors"
	"time"

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
// A socket backend (PR 4) whose reconnect could not resume — another client
// identity: a retired client, a replaced engine, a restarted host — answers a
// command's refusal as its ErrOutcomeUnknown with the reason resume_lost,
// wrapping this sentinel, so a caller matching either finds it: the command was
// not sent on the new binding, and what became of anything sent on the old one
// is unknown. A read's refusal is this sentinel alone, as in process: a read
// has no outcome to be unknown.
var ErrStaleEpoch = errors.New("backend: the call was for a session this backend is no longer bound to")

// ErrOutcomeUnknown is what a backend answers for a command that may have run
// and whose answer can no longer be learned: it was not refused, and nothing
// says whether it executed, so the caller re-reads state rather than trusting
// either outcome (plan 027 §3.14). A socket backend answers it after a
// reconnect the host answered resumed: false (reason resume_lost), once its
// redials are spent (disconnected), and for a call refused before sending
// because its epoch is stale (resume_lost, matching ErrStaleEpoch as well). An
// in-process backend never answers it: the engine call it makes always
// returns. Its match is errors.Is, never the text.
var ErrOutcomeUnknown = errors.New("backend: the command's outcome is unknown: it may have run")

// ErrStopUnsupported is Stop on a session whose host cannot stop it (plan 030
// §3.6a): a host whose session capability stop is false — a TUI-hosted
// session, or a host from before plan 030 — which answered session.stop
// unsupported, reason stop_unsupported. Nothing was stopped and nothing ran;
// the caller detaches instead and says the session runs on (§3.6). A socket
// backend's refusal matches it by errors.Is, reconstructed from the wire's
// reason (internal/remote's sentinel table); an in-process backend never
// answers it.
var ErrStopUnsupported = errors.New("backend: the session's host cannot stop it")

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
	// Stop asks for the session itself to end — the explicit quit (plan 030
	// §3.6a), as opposed to Close, which over the socket is a view close and
	// never stops a session. c is the caller's own command, as for every
	// other command: its id is the caller's to mint, so the ids a client
	// sends stay one sequence whoever sends them. In process it is the
	// engine's close — today's quit path, unchanged: it returns once the
	// session has closed, and answers what the close said. Over the socket it
	// is session.stop, answered once the host has taken the stop — a receipt;
	// the session's end follows on the stream (its closing records, then the
	// stream's end) — and it is ErrStopUnsupported on a host that cannot stop
	// its session. It is fenced by the epoch ctx carries, like every command,
	// and it may block — in process, until the agent is reaped — so it is
	// never called from Update.
	Stop(ctx context.Context, c engine.Command) error
	// ClientID is this client's id on the session, the Client of every
	// engine.Command it sends. It is read per command, never cached by the
	// caller (§3.12): after a reconnect that could not resume, a remote
	// client has a new id. "" means commands name no client (the zero
	// engine.Command).
	ClientID() string
	// Epoch names the session the backend is bound to now (§3.12, "Chains
	// are fenced in the backend too"). It is constant in process — one
	// engine for the backend's life; a socket backend (PR 4) moves it when a
	// reconnect could not resume (another client identity — the same
	// incarnation included), before any Restore is delivered, and binds
	// every command and read to it: none is ever written under another. A
	// caller reads it when it dispatches a call and passes it
	// with every call of that operation (WithEpoch); a command or read whose
	// ctx carries another epoch is refused with ErrStaleEpoch before
	// anything is sent (CheckEpoch). It waits on nothing.
	Epoch() uint64
	// Info is the session's static facts (§3.13), fixed once the session is
	// up: the provider's name and label, its capabilities as advertised
	// (never rebuilt from the client binary's own provider table — astra
	// 25), the provider and craze session ids, the model and mode catalogs,
	// the incarnation and the retry horizon. In process it reads
	// State().Snapshot's static fields; over the socket it is the attach
	// reply's copy, replaced by each Ready and Restore as the stream receives
	// them. It waits on nothing, and before Start (over the socket: before
	// the first attach reply) it reflects the configured provider, as the
	// TUI does today (GLM 11).
	Info() SessionInfo

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
	// LastTurn is how the session's last turn ended (plan 030 §3.7, SF-57;
	// engine.State.LastTurn): nil while a turn runs, before any has ended,
	// and from a host that does not say (one from before plan 030). It is
	// what a client reads once after a restore, since the snapshot a restore
	// carries keeps no turn's ending (its codec stays at version 1). A
	// blocking read, for tea.Cmds only, never Update: in process it is
	// State()'s, over the socket session.state's lastTurn. It is fenced by
	// the epoch ctx carries, like every read.
	LastTurn(ctx context.Context) (*engine.LastTurn, error)
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
	// ItemReady says the session's start has completed (remote only): Info
	// is its facts as the host read them once it was ready (§3.4), and Err
	// the start's failure — nil when the session came up.
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
	// Err is, on ItemEnd, why the stream ended (nil for the session's own
	// end, reset{session_closed}); on ItemReady, the start's failure (nil when
	// the session came up), whose Error() is the host's start error text.
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
	// PermissionMode is how whatever serves the session spawned its agent
	// (plan 030 §3.7, SF-60): the wire's permissionMode, the host's --force
	// or --no-force. PermissionUnsaid is a host that does not say — one from
	// before plan 030 — and the in-process backend, whose TUI's own config
	// is the mode; a client then shows its own config's, as it always has.
	PermissionMode PermissionMode
	// StartedAt is when the host started serving the session, on its clock
	// (plan 030 §3.7, SF-63): what a client counts the session's elapsed time
	// from. Zero when the host does not say — one from before plan 030 — and
	// in process, where the session's own start is this client's; a client
	// then counts from its own start, as it always has.
	StartedAt time.Time
}

// PermissionMode is how a session's agent handles permission requests (plan
// 030 §3.7, SF-60): the wire's protocol.PermissionMode in the backend's
// words.
type PermissionMode string

const (
	// PermissionUnsaid is a session whose host does not say (see
	// SessionInfo.PermissionMode).
	PermissionUnsaid PermissionMode = ""
	// PermissionBypass is --force: the agent runs its tools unasked.
	PermissionBypass PermissionMode = "bypass"
	// PermissionPrompt is --no-force: the agent asks, and a client answers.
	PermissionPrompt PermissionMode = "prompt"
)
