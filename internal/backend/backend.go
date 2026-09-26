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

	// Read is the stream: one item at a time, in order, from one reader. A
	// cancelled ctx returns ctx.Err() without consuming an item. ErrClosed
	// once the stream has ended.
	Read(ctx context.Context) (Item, error)

	// Commands: engine.Control's, ctx first. In process the ones that wait
	// on nothing (Submit, Answer, Unqueue, EditQueued, ClearQueue, Disarm,
	// SetTitle, CancelSubagent) ignore ctx; Interject, Set and Cancel wait,
	// and are bounded by it.
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
