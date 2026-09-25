package protocol

import (
	"encoding/json"
	"slices"
	"time"
)

// Every method's params and result, every notification's params, and the
// documents they share (plan 027 §3.3, §3.4), one Go type each. The schema
// that describes each is schema/<method>.json's $defs params and result,
// schema/notification.<name>.json's $defs params, and info.json for the
// shared documents; TestSchemaCoversEveryWireField holds the two to each other,
// field for field, both ways.
//
// A json.RawMessage here is a payload another codec writes — the server fills
// it from the lossless event codec's encoder or its exported leaf wrappers, or
// from the snapshot codec — and its schema is a $ref into event.json or
// snapshot.json (doc.go, "Shapes that come from the codecs").

// ------------------------------------------------------------------ hello

// HelloParams is hello's params, which open a connection (plan 027 §3.2,
// §3.3, §3.6): the protocol versions the client speaks, who it is, and
// optionally the client id it resumes. hello is the one tolerant method — a host ignores fields of these
// params it does not know, at every depth — because it is where versions
// meet.
type HelloParams struct {
	// Protocols is every protocol version the client speaks, e.g. [1]. The
	// host answers with the highest it shares, or refuses bad_request, reason
	// protocol_version, with data.result a HelloErrorResult.
	Protocols []int      `json:"protocols"`
	Client    ClientInfo `json:"client"`
	// Resume asks to take back a client id this host minted, with the token
	// it was handed (§3.6). A matching token in the same engine incarnation
	// resumes (resumed: true); a retired client or another incarnation gets a
	// fresh id (resumed: false); a wrong token is bad_request, reason
	// bad_token.
	Resume *Resume `json:"resume,omitempty"`
	// Auth is reserved for S6's scopes: absent or null in protocol 1.
	Auth json.RawMessage `json:"auth,omitempty"`
	// Via is reserved for a hub or relay that originates the host connection
	// itself (S4/S7): absent or null in protocol 1.
	Via json.RawMessage `json:"via,omitempty"`
}

// ClientInfo is who is connecting: a kind ("tui", "shed", …) and, optionally,
// a name, a version and the client's own capabilities.
type ClientInfo struct {
	Kind    string `json:"kind"`
	Name    string `json:"name,omitempty"`
	Version string `json:"version,omitempty"`
	// Capabilities is the client's own capability set (05), carried and
	// empty in protocol 1.
	Capabilities *ClientCapabilities `json:"capabilities,omitempty"`
}

// ClientCapabilities is a client's capability set: empty in protocol 1. A
// later client announces new behaviour here, and a host ignores what it does
// not know.
type ClientCapabilities struct{}

// Resume is the client id a reconnecting client takes back and the resume
// token hello handed it (plan 027 §3.6): 128 random bits, hex, kept only in the
// host's binding table and never logged.
type Resume struct {
	ClientID string `json:"clientId"`
	Token    string `json:"token"`
}

// hello's result is one of two documents, told apart by endpoint.kind (plan
// 027 X5): a host's (HelloResult) and a hub's (HubHelloResult). The four
// members that belong to a host's commands — clientId, token, resumed and
// retryHorizon — are a host's alone: client ids, tokens and receipts are
// always the host's, even through a hub's splice (§3.3), so a hub's hello
// carries none of them and a client reads them only from a host. The schema
// is hello.json's $defs result, a oneOf of hostResult and hubResult, each
// held to its own Go type.

// HelloResult is a host's answer to hello (endpoint.kind "host"): the
// protocol chosen, the host's identity, the connection's client id and
// resume token, whether a resume took, and everything a client needs before
// its first command. A host always writes every member — resumed: false
// included — so none of them is optional here; the hub's answer is
// HubHelloResult.
type HelloResult struct {
	// Protocol is the highest version both sides speak.
	Protocol int `json:"protocol"`
	// Endpoint is the host; its Kind is EndpointHost.
	Endpoint Endpoint `json:"endpoint"`
	// ClientID is the connection's client id, bound to it and never sent per
	// call; with Resumed it is the id the client asked to resume. Host-only
	// (X5).
	ClientID string `json:"clientId"`
	// Token resumes ClientID on a later connection to this host, within this
	// engine incarnation. Host-only (X5).
	Token string `json:"token"`
	// Resumed says the resume took: the same client id, the same engine
	// incarnation. Only then does a client resend its in-flight commands
	// under their own ids (§3.14); otherwise their outcome is unknown.
	// Host-only (X5), and always written, false included.
	Resumed      bool                   `json:"resumed"`
	Capabilities ConnectionCapabilities `json:"capabilities"`
	// Codecs is the event and snapshot codecs' own versions. A client that
	// does not know one must not fold that stream (§3.3).
	Codecs Codecs `json:"codecs"`
	// Limits is the two line limits; clients must accept OutboundLine.
	Limits Limits `json:"limits"`
	// RetryHorizon is the host's command-id table's bound. Host-only (X5).
	RetryHorizon RetryHorizon `json:"retryHorizon"`
}

// HubHelloResult is a hub's answer to hello (endpoint.kind "hub"; plan 027
// §3.3's hub mode, X5): the protocol, the hub's identity and what the
// connection can do there. It carries no client id, token, resumed or retry
// horizon — a hub mints no client ids and keeps no receipts — and a hub result
// that carries any of them is invalid. A client that goes on to a session is
// spliced to its host (session.connect) and says hello to the host itself.
// No hub exists before S4; protocol 1 defines the shape so S4 needs no change.
type HubHelloResult struct {
	Protocol int `json:"protocol"`
	// Endpoint is the hub; its Kind is EndpointHub.
	Endpoint     Endpoint               `json:"endpoint"`
	Capabilities ConnectionCapabilities `json:"capabilities"`
	Codecs       Codecs                 `json:"codecs"`
	Limits       Limits                 `json:"limits"`
}

// HelloErrorResult is data.result beside hello's protocol_version refusal:
// the versions the host does speak.
type HelloErrorResult struct {
	Supported []int `json:"supported"`
}

// The endpoint kinds (plan 027 X5): what answered a hello, and so which of
// the two results it is.
const (
	// EndpointHost is a session host: its hello result is a HelloResult.
	EndpointHost = "host"
	// EndpointHub is the hub (S4): its hello result is a HubHelloResult.
	EndpointHub = "hub"
)

// Endpoint is who answered: a host or the hub, its id (a host's is 12 hex
// digits, minted at process start, and sessions.list's epoch), craze's version
// and its pid.
type Endpoint struct {
	Kind         string `json:"kind"`
	HostID       string `json:"hostId"`
	CrazeVersion string `json:"crazeVersion"`
	PID          int    `json:"pid"`
}

// ConnectionCapabilities is the connection's capability set (plan 027 §3.3,
// SD-28): what the endpoint itself can do, as opposed to one session. A
// client hides what a capability says it cannot do (05).
type ConnectionCapabilities struct {
	RosterSubscribe bool `json:"rosterSubscribe"`
	SessionCreate   bool `json:"sessionCreate"`
	Multiplex       bool `json:"multiplex"`
	Connect         bool `json:"connect"`
	Snapshot        bool `json:"snapshot"`
	AttachWhenNow   bool `json:"attachWhenNow"`
}

// HostCapabilities is a session host's connection capabilities in protocol
// 1: the roster subscription, session creation, multiplexing and the splice
// are the hub's (all false), and a snapshot and an attach with when: "now"
// are served.
func HostCapabilities() ConnectionCapabilities {
	return ConnectionCapabilities{Snapshot: true, AttachWhenNow: true}
}

// Codecs is the version of each codec whose output the protocol carries
// verbatim: agent.EventCodecVersion and transcript.SnapshotVersion.
type Codecs struct {
	Event    int `json:"event"`
	Snapshot int `json:"snapshot"`
}

// Limits is the two line limits a host holds, in bytes (InboundLineMax,
// OutboundLineMax).
type Limits struct {
	InboundLine  int `json:"inboundLine"`
	OutboundLine int `json:"outboundLine"`
}

// HostLimits is Limits as protocol 1 sets them.
func HostLimits() Limits {
	return Limits{InboundLine: InboundLineMax, OutboundLine: OutboundLineMax}
}

// RetryHorizon is the command-id table's bound (engine.RetryHorizon): within
// it a resent command id is answered from the table and never re-executes;
// past it, unknown_command. Commands is how many answers the table keeps at
// most, AgeMs how long, in milliseconds.
type RetryHorizon struct {
	Commands int   `json:"commands"`
	AgeMs    int64 `json:"ageMs"`
}

// ------------------------------------------------------ the session's facts

// SessionInfo is the session info document (plan 027 §3.3, §3.13): the
// session's static facts, final once the session is up. It is the attach
// reply's session, a ready notification's session, and the first half of a
// sessions.list row. Before the session is ready, its catalogs are empty and
// its providerSessionId may be "".
type SessionInfo struct {
	// SessionID is the durable craze session id (SD-22): what every
	// session-scoped method names at params.sessionId.
	SessionID string `json:"sessionId"`
	// ProviderSessionID is the agent's own id for the session, which a
	// session/load changes.
	ProviderSessionID string `json:"providerSessionId"`
	// Incarnation is the event log's id: the scope of seqs, turn ids and ask
	// ids, and a cursor's first half.
	Incarnation string `json:"incarnation"`
	// HostID is the host serving the session (Endpoint.HostID).
	HostID string `json:"hostId"`
	// Workspace is the session's working directory.
	Workspace    string              `json:"workspace"`
	Provider     Provider            `json:"provider"`
	Catalogs     Catalogs            `json:"catalogs"`
	Capabilities SessionCapabilities `json:"capabilities"`
	RetryHorizon RetryHorizon        `json:"retryHorizon"`
}

// Provider is the session's provider: its id ("cursor", "grok", "gx",
// "native") and the label a client shows.
type Provider struct {
	Name  string `json:"name"`
	Label string `json:"label"`
}

// Catalogs is what the session can be switched to: its models and its modes,
// empty until the session is ready.
type Catalogs struct {
	Models []CatalogModel `json:"models"`
	Modes  []CatalogMode  `json:"modes"`
}

// CatalogModel is one model a session.set of kind model can name.
type CatalogModel struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// CatalogMode is one mode a session.set of kind mode can name.
type CatalogMode struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

// SessionCapabilities is the session's capability set (plan 027 §3.3,
// SD-28): every agent.Capabilities field in its wire name — the mapping from
// the struct is internal/control's, since this package imports no agent, and
// TestEveryCapabilityIsOnTheWire (C6) holds it field for field — plus four
// the protocol states for every host of protocol 1: cancel, approvals and
// historyCursor true, and stop false on a TUI-hosted session (§3.9).
type SessionCapabilities struct {
	Interject           bool `json:"interject"`
	SubagentCancel      bool `json:"subagentCancel"`
	SubagentBackground  bool `json:"subagentBackground"`
	Modes               bool `json:"modes"`
	Effort              bool `json:"effort"`
	FastToggle          bool `json:"fastToggle"`
	SubagentRows        bool `json:"subagentRows"`
	SubagentTranscript  bool `json:"subagentTranscript"`
	Todos               bool `json:"todos"`
	AskCards            bool `json:"askCards"`
	PlanCards           bool `json:"planCards"`
	ParameterizedPicker bool `json:"parameterizedPicker"`
	Cancel              bool `json:"cancel"`
	Approvals           bool `json:"approvals"`
	HistoryCursor       bool `json:"historyCursor"`
	Stop                bool `json:"stop"`
}

// SessionRow is one sessions.list row (plan 027 §3.3): the info document
// and the session's live roster facts — shed's two signals among them, "is it
// running" (activity == working, or foreignTurn) and "is it blocked on me"
// (pendingAsks, headAsk).
type SessionRow struct {
	SessionInfo
	Title    string   `json:"title"`
	Activity Activity `json:"activity"`
	// ForeignTurn says the agent is running a turn of its own (grok's
	// interjection fallback, a native wake): the engine's activity reads idle
	// during one.
	ForeignTurn bool `json:"foreignTurn"`
	PendingAsks int  `json:"pendingAsks"`
	// HeadAsk is the first open ask, the one a client would show; absent
	// when none is open.
	HeadAsk *HeadAsk `json:"headAsk,omitempty"`
}

// HeadAsk names the ask at the head of the open ones: its id, its kind
// (permission, question or plan) and its label (agent.AskLabel: the text a
// card draws — "permission <tool>", "question", "plan <name>").
type HeadAsk struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"`
	Label string `json:"label"`
}

// Activity is what the engine is doing, in the gate table's words
// (engine.Activity). Ask pending and foreign turn are not activities: they
// overlay any of these, and are read from their own fields.
type Activity string

const (
	ActivityStarting  Activity = "starting"
	ActivityReplaying Activity = "replaying"
	ActivityIdle      Activity = "idle"
	ActivityWorking   Activity = "working"
	ActivityError     Activity = "error"
	ActivityClosing   Activity = "closing"
)

var activities = []Activity{
	ActivityStarting, ActivityReplaying, ActivityIdle, ActivityWorking, ActivityError, ActivityClosing,
}

// Activities is every activity, in the engine's order.
func Activities() []Activity { return slices.Clone(activities) }

// -------------------------------------------------------------- the roster

// SessionsListParams takes nothing.
type SessionsListParams struct{}

// SessionsListResult is the roster (plan 027 §3.3, SD-28). On a host, Epoch
// is its hostId — a new host process is a new epoch, and a client reseeds —
// and Cursor is the log's committed seq when the rows were read. The hub (S4)
// keeps both fields with meanings of its own.
type SessionsListResult struct {
	Epoch    string       `json:"epoch"`
	Cursor   uint64       `json:"cursor"`
	Sessions []SessionRow `json:"sessions"`
}

// SessionsSubscribeParams takes nothing. A host answers unsupported, reason
// roster_unsupported (rosterSubscribe: false); the hub defines the method and
// its result (S4), which protocol 1 does not specify.
type SessionsSubscribeParams struct{}

// ConnectParams names the session a hub splices this connection to (plan 027
// §3.3's hub splice): the hub dials that host's socket, answers {}, and from
// the next byte on the client speaks to the host itself, starting with its
// own hello. A per-session host answers unsupported, reason hub_only.
type ConnectParams struct {
	SessionID string `json:"sessionId"`
}

// ------------------------------------------------------------------ attach

// When is when an attach answers (plan 027 §3.4).
type When string

const (
	// WhenReady (the default) waits until the session's start has run: a
	// load replay reaches the client as the snapshot, never as a live burst.
	WhenReady When = "ready"
	// WhenNow attaches at once, the start included, and answers ready: false
	// while the session is starting; one ready notification follows.
	WhenNow When = "now"
)

// AttachParams is session.attach's params, which attach this connection to
// the session (plan 027 §3.4). A connection holds at most one live
// attachment (SQ14).
type AttachParams struct {
	SessionID string `json:"sessionId"`
	// Cursor is where the client already is, {incarnation, last folded seq};
	// absent asks for a snapshot.
	Cursor *Cursor `json:"cursor,omitempty"`
	// When is "ready" when absent.
	When   When          `json:"when,omitempty"`
	Budget *AttachBudget `json:"budget,omitempty"`
}

// Cursor is a place in a session's stream: the log incarnation and the seq
// of the last event the client folded.
type Cursor struct {
	Incarnation string `json:"incarnation"`
	Seq         uint64 `json:"seq"`
}

// AttachBudget is an attachment's budget, each member absent for the host's
// default: the subscription's MaxItems and MaxBytes (1,024 records and 8 MiB
// by default; a client may lower either, and raise it up to the host's
// MaxBudget) and the snapshot's byte budget (4 MiB by default, capped at
// SnapshotBytesMax).
type AttachBudget struct {
	MaxItems      int `json:"maxItems,omitempty"`
	MaxBytes      int `json:"maxBytes,omitempty"`
	SnapshotBytes int `json:"snapshotBytes,omitempty"`
}

// AttachResult is the attach reply (plan 027 §3.4). It is queued before the
// subscription's first event, so every event follows the reply that carries
// its snapshot.
type AttachResult struct {
	// Subscription is the server-assigned id ("s-1", "s-2", …) every
	// notification of this attachment carries.
	Subscription string      `json:"subscription"`
	Session      SessionInfo `json:"session"`
	// Ready says the session's start has run. False only for when: "now"
	// while it is starting; a ready notification follows.
	Ready bool `json:"ready"`
	// After is where the stream continues: the client's cursor when it was
	// honoured, else the snapshot's {incarnation, seq}. Everything at or below
	// it is covered.
	After Cursor `json:"after"`
	// Snapshot is transcript.EncodeSnapshot's JSON, embedded raw
	// (snapshot.json); absent when the cursor was honoured.
	Snapshot json.RawMessage `json:"snapshot,omitempty"`
	// Reset is why a cursor the client gave was refused — the first refusal's
	// reason — and absent when it was honoured or none was given.
	Reset CursorReason `json:"reset,omitempty"`
}

// CursorReason is why a cursor was not honoured (agent.CursorReason): the
// attach reply's reset.
type CursorReason string

const (
	CursorForeignIncarnation CursorReason = "foreign_incarnation"
	CursorFutureSeq          CursorReason = "future_seq"
	CursorBacklogTooLarge    CursorReason = "backlog_too_large"
	CursorNoJournal          CursorReason = "no_journal"
	CursorJournalGap         CursorReason = "journal_gap"
	CursorJournalBehind      CursorReason = "journal_behind"
	CursorEvicted            CursorReason = "evicted"
)

var cursorReasons = []CursorReason{
	CursorForeignIncarnation, CursorFutureSeq, CursorBacklogTooLarge, CursorNoJournal,
	CursorJournalGap, CursorJournalBehind, CursorEvicted,
}

// CursorReasons is every cursor reason, in the log's order.
func CursorReasons() []CursorReason { return slices.Clone(cursorReasons) }

// DetachParams is session.detach's params, which end this connection's
// attachment. The reply is the attachment's terminal acknowledgement (§3.7):
// nothing of it follows.
type DetachParams struct {
	SessionID    string `json:"sessionId"`
	Subscription string `json:"subscription"`
}

// -------------------------------------------------------- reading a session

// StateParams is session.state's params.
type StateParams struct {
	SessionID string `json:"sessionId"`
}

// StateResult is the engine's state projection (engine.State; plan 027
// §3.3) and the host's live settings (§3.12's Settings). It is a read, not a
// cut through the stream: a client folds events for order and reads this for
// where things stand.
type StateResult struct {
	Activity Activity `json:"activity"`
	// ForeignTurn says the agent is running a turn of its own.
	ForeignTurn bool `json:"foreignTurn"`
	// Turn is the current turn's id, "" when none is.
	Turn string `json:"turn"`
	// Waiting says the current turn's claim was refused by a foreign turn and
	// waits to be claimed again (craze prompt's policy); false does not mean
	// "running".
	Waiting bool `json:"waiting"`
	// SendNow is the armed send-now, absent when nothing is armed.
	SendNow *ArmedSend `json:"sendNow,omitempty"`
	// Queue is the message queue in send order, each row the event codec's
	// queue row (event.json#/$defs/queued).
	Queue       []json.RawMessage `json:"queue"`
	PendingAsks int               `json:"pendingAsks"`
	// HeadAsk is the first open ask, absent when none is open.
	HeadAsk *HeadAsk `json:"headAsk,omitempty"`
	// Err is the failure an activity of error stands on, "" when none.
	Err string `json:"err"`
	// StartFailed says the session's start failed; Err carries it.
	StartFailed bool `json:"startFailed"`
	// Prompted says a turn has been started at all, and Cancelled that the
	// last turn to settle ended cancelled.
	Prompted  bool     `json:"prompted"`
	Cancelled bool     `json:"cancelled"`
	Settings  Settings `json:"settings"`
}

// ArmedSend is an armed send-now (engine.ArmedSend): its text, the queued row
// it re-takes ("" for text the client holds itself), the turn it replaces,
// and the command that armed it, as Event.Cause spells it.
type ArmedSend struct {
	Text    string `json:"text"`
	FromRow string `json:"fromRow"`
	Turn    string `json:"turn"`
	Cause   string `json:"cause"`
}

// Settings is the session's live settings as the host reads them: the
// current model and mode ("" when the session has none), and the provider's
// config options as the event codec's config section
// (event.json#/$defs/config: {"options": […]}, or {} for none).
type Settings struct {
	Model  string          `json:"model"`
	Mode   string          `json:"mode"`
	Config json.RawMessage `json:"config"`
}

// SnapshotParams is session.snapshot's params, which ask for one bounded
// snapshot with no subscription (plan 027 §3.4): of the main transcript, or
// of the child AgentID names.
type SnapshotParams struct {
	SessionID string          `json:"sessionId"`
	AgentID   string          `json:"agentId,omitempty"`
	Budget    *SnapshotBudget `json:"budget,omitempty"`
}

// SnapshotBudget is a snapshot's byte budget, absent for the default (4
// MiB) and capped at SnapshotBytesMax: an object with the attach budget's own
// member name, snapshotBytes (plan 027 X6).
type SnapshotBudget struct {
	SnapshotBytes int `json:"snapshotBytes,omitempty"`
}

// SnapshotResult is one snapshot, transcript.EncodeSnapshot's JSON embedded
// raw (snapshot.json), its own cut {incarnation, seq} inside it.
type SnapshotResult struct {
	Snapshot json.RawMessage `json:"snapshot"`
}

// SyncParams is session.sync's params: the reply barrier with no command
// (plan 027 §3.6).
type SyncParams struct {
	SessionID string `json:"sessionId"`
}

// SyncResult is the log's committed seq at the call: every event committed
// before it has been written to this connection's subscription before this
// reply.
type SyncResult struct {
	Seq uint64 `json:"seq"`
}

// ---------------------------------------------------------------- commands

// PromptMode is how a prompt is admitted (plan 027 §3.3; 05).
type PromptMode string

const (
	// PromptQueue starts the prompt now when the engine is admitting and
	// queues it otherwise: what Enter does.
	PromptQueue PromptMode = "queue"
	// PromptSendNow starts it now; while a turn is working it arms the send,
	// cancels that turn, and fires when it settles.
	PromptSendNow PromptMode = "send_now"
	// PromptInterject folds the text into the running turn without
	// cancelling it, where the provider can (capability interject).
	PromptInterject PromptMode = "interject"
)

// PromptParams is session.prompt's params. FromRow names a queued row to
// start instead of text (queue and send_now); the row's own text is what
// goes. Interject takes text alone — the engine's Interject has no row — so
// the server refuses mode interject with a fromRow, -32602, bad_request (plan
// 027 X6); the schema allows the combination, and the server is what refuses
// it.
type PromptParams struct {
	SessionID string     `json:"sessionId"`
	CommandID string     `json:"commandId"`
	Text      string     `json:"text,omitempty"`
	FromRow   string     `json:"fromRow,omitempty"`
	Mode      PromptMode `json:"mode"`
}

// PromptResult is what became of a prompt: for queue and send_now, exactly
// one of a started turn with the text it started with, the row it became
// (or, for a fromRow that cannot start yet, that row unchanged), or armed;
// for interject, nothing ({}).
type PromptResult struct {
	// Turn is the turn the prompt started, in this call: the one started
	// event whose effect the client has already applied (engine.SubmitResult).
	Turn string `json:"turn,omitempty"`
	// Text is the text Turn started with, present exactly when Turn is: a
	// fromRow's text as the host read it, which another client may have edited
	// since this one looked (§3.13). It is a pointer so an empty text is still
	// sent.
	Text *string `json:"text,omitempty"`
	// Queued is the row, the event codec's queue row
	// (event.json#/$defs/queued).
	Queued json.RawMessage `json:"queued,omitempty"`
	// Armed says a send-now was armed against the running turn.
	Armed bool `json:"armed,omitempty"`
}

// CancelParams is session.cancel's params: the current turn, or TurnID's
// while it is still the current one (a named stale turn is stale_turn).
type CancelParams struct {
	SessionID string `json:"sessionId"`
	CommandID string `json:"commandId"`
	TurnID    string `json:"turnId,omitempty"`
}

// CancelOutcome is what an accepted cancel came to (engine.CancelOutcome).
type CancelOutcome string

const (
	CancelRequested CancelOutcome = "requested"
	CancelSettled   CancelOutcome = "settled"
	CancelUnknown   CancelOutcome = "unknown"
)

// CancelResult is a cancel's outcome (engine.CancelResult): the result, and
// data.result beside a cancel's error. Turn is the turn it was held against,
// "" with no turn of craze's own running; Reported says the failure the error
// beside it names was also published, as a send-now delta, so a client that
// words failures from deltas must not word this one again.
type CancelResult struct {
	Outcome  CancelOutcome `json:"outcome"`
	Turn     string        `json:"turn"`
	Reported bool          `json:"reported"`
}

// DisarmParams is session.disarm's params, which take back an armed
// send-now, leaving its text where it was.
type DisarmParams struct {
	SessionID string `json:"sessionId"`
	CommandID string `json:"commandId"`
}

// QueueAddParams is session.queue.add's params: a prompt to queue without
// starting anything.
type QueueAddParams struct {
	SessionID string `json:"sessionId"`
	CommandID string `json:"commandId"`
	Text      string `json:"text"`
}

// QueueAddResult is the row the text became (event.json#/$defs/queued).
type QueueAddResult struct {
	Row json.RawMessage `json:"row"`
}

// QueueEditParams is session.queue.edit's params, which rewrite a queued row
// in place. ExpectedVersion is the
// check-and-edit: absent is unconditional, and a version the row no longer
// has is stale_version (§3.13: two editors never overwrite each other).
type QueueEditParams struct {
	SessionID       string `json:"sessionId"`
	CommandID       string `json:"commandId"`
	RowID           string `json:"rowId"`
	Text            string `json:"text"`
	ExpectedVersion *int   `json:"expectedVersion,omitempty"`
}

// QueueRemoveParams is session.queue.remove's params: the row to take out of
// the queue.
type QueueRemoveParams struct {
	SessionID string `json:"sessionId"`
	CommandID string `json:"commandId"`
	RowID     string `json:"rowId"`
}

// QueueRemoveResult is the row removed (event.json#/$defs/queued).
type QueueRemoveResult struct {
	Row json.RawMessage `json:"row"`
}

// QueueClearParams is session.queue.clear's params, which empty the queue.
type QueueClearParams struct {
	SessionID string `json:"sessionId"`
	CommandID string `json:"commandId"`
}

// QueueClearResult is the rows removed, in queue order
// (event.json#/$defs/queued each).
type QueueClearResult struct {
	Removed []json.RawMessage `json:"removed"`
}

// SettingKind is which of the session's settings a Setting names
// (engine.SettingKind).
type SettingKind string

const (
	SettingModel  SettingKind = "model"
	SettingMode   SettingKind = "mode"
	SettingConfig SettingKind = "config"
)

// SetParams is session.set's params: one setting to change.
type SetParams struct {
	SessionID string  `json:"sessionId"`
	CommandID string  `json:"commandId"`
	Setting   Setting `json:"setting"`
}

// Setting is one settings change (engine.Setting): for kind config, ID is the
// option's id; ForModel binds a config change to the model it was chosen for
// (stale_model when the session has left it). Value may be "".
type Setting struct {
	Kind     SettingKind `json:"kind"`
	ID       string      `json:"id,omitempty"`
	Value    string      `json:"value"`
	ForModel string      `json:"forModel,omitempty"`
}

// SetResult is the confirmed value — what the session is now at, not an
// echo of the request — and its revision: the seq of the state delta that
// carried the change, 0 when it could not be learned (engine.SetResult).
type SetResult struct {
	Value string `json:"value"`
	Rev   uint64 `json:"rev"`
}

// SetTitleParams is session.setTitle's params, which rename the session. The
// delta, not the reply, is how a client learns the title.
type SetTitleParams struct {
	SessionID string `json:"sessionId"`
	CommandID string `json:"commandId"`
	Title     string `json:"title"`
}

// SubagentCancelParams is session.subagent.cancel's params: the one running
// sub-agent to stop, the rest of the turn going on (plan 026 §3.10, plan 027
// §3.17). Its outcome is the child's finished roster row, as an event.
type SubagentCancelParams struct {
	SessionID string `json:"sessionId"`
	CommandID string `json:"commandId"`
	AgentID   string `json:"agentId"`
}

// StopParams is session.stop's params, which end the session and its host
// (plan 027 §3.9). A TUI-hosted
// session's host answers unsupported, reason stop_unsupported (capability
// stop: false); S4's headless host implements it, answering {}.
type StopParams struct {
	SessionID string `json:"sessionId"`
	CommandID string `json:"commandId"`
}

// ------------------------------------------------------------------- asks

// AsksListParams is asks.list's params.
type AsksListParams struct {
	SessionID string `json:"sessionId"`
}

// AsksListResult is every open ask as a summary, in opening order: the body
// is asks.get's, so the list stays small whatever an ask carries (§3.2).
type AsksListResult struct {
	Asks []AskSummary `json:"asks"`
}

// AskSummary is one ask in a list: its id, kind, label (HeadAsk's) and when
// it opened.
type AskSummary struct {
	ID       string    `json:"id"`
	Kind     string    `json:"kind"`
	Label    string    `json:"label"`
	OpenedAt time.Time `json:"openedAt"`
}

// AsksGetParams is asks.get's params: one ask, by id.
type AsksGetParams struct {
	SessionID string `json:"sessionId"`
	AskID     string `json:"askId"`
}

// AsksGetResult is the ask's record.
type AsksGetResult struct {
	Ask AskRecord `json:"ask"`
}

// AskStatus is whether an ask is still waiting (agent.AskStatus).
type AskStatus string

const (
	AskOpen     AskStatus = "open"
	AskResolved AskStatus = "resolved"
)

// AskRecord is one ask as the host's registry holds it (agent.AskRecord),
// on the wire: what was asked — the body, each string capped at AskItemCap,
// Truncated set when one was cut — and what became of it. Outcome, By,
// Answer and ResolvedAt are absent while the ask is open, and Answer too when
// it ended with no answer. The registry's turn token, the provider-delivery
// fields (consumed, delivered, lost) and the incarnation are left out: the
// first two are the host's own bookkeeping, and the incarnation is the info
// document's (plan 027 X6).
type AskRecord struct {
	ID     string    `json:"id"`
	Kind   string    `json:"kind"`
	Status AskStatus `json:"status"`
	// Outcome is how it ended: answered, cancelled, turn_ended, closing or
	// automatic; By is whose doing it was.
	Outcome string  `json:"outcome,omitempty"`
	By      string  `json:"by,omitempty"`
	Answer  *Answer `json:"answer,omitempty"`
	// Body is the opening exactly as offered, the event codec's ask body
	// (event.json#/$defs/askBody): one of permission, question and plan.
	Body       json.RawMessage `json:"body"`
	OpenedAt   time.Time       `json:"openedAt"`
	ResolvedAt time.Time       `json:"resolvedAt,omitzero"`
	Truncated  bool            `json:"truncated"`
}

// AsksAnswerParams is asks.answer's params. The first valid answer wins; an
// invalid one is bad_request, reason bad_answer, and leaves the ask open.
type AsksAnswerParams struct {
	SessionID string `json:"sessionId"`
	CommandID string `json:"commandId"`
	AskID     string `json:"askId"`
	Answer    Answer `json:"answer"`
}

// Answer is an answer to one ask (agent.AskAnswer), each kind reading its
// own members and refusing any other kind's:
//
//   - permission: OptionID, an option the request offered (the exact id,
//     never a kind), or Cancel;
//   - question: Answers, each question's id to the option ids chosen (a null
//     list is not an empty one), or Skip; a question that asks nothing takes
//     no answers at all;
//   - plan: exactly one of Accept and Reject.
type Answer struct {
	OptionID string              `json:"optionId,omitempty"`
	Cancel   bool                `json:"cancel,omitempty"`
	Answers  map[string][]string `json:"answers,omitempty"`
	Skip     bool                `json:"skip,omitempty"`
	Accept   bool                `json:"accept,omitempty"`
	Reject   bool                `json:"reject,omitempty"`
}

// Empty is the result of a method that answers nothing but success: {}.
type Empty struct{}

// ----------------------------------------------------------- notifications

// EventParams is one committed event (plan 027 §3.3): its seq and its body,
// Record.Body verbatim — the lossless event codec's JSON (event.json), never
// re-encoded.
type EventParams struct {
	Subscription string          `json:"subscription"`
	Seq          uint64          `json:"seq"`
	Event        json.RawMessage `json:"event"`
}

// SynchronizedParams is a synchronized notification's params: the stream has
// delivered through the attach's cutoff, Seq (§3.4). It is sent once per
// attachment.
type SynchronizedParams struct {
	Subscription string `json:"subscription"`
	Seq          uint64 `json:"seq"`
}

// ReadyParams is a ready notification's params: the session's start has
// completed (StartFailed false) or failed (StartFailed true, Err its text),
// sent once, on an attachment made before readiness (§3.4). Session is the final info document: its catalogs
// and provider session id are set now.
type ReadyParams struct {
	Subscription string      `json:"subscription"`
	Session      SessionInfo `json:"session"`
	StartFailed  bool        `json:"startFailed"`
	Err          string      `json:"err,omitempty"`
}

// ResetParams is a reset notification's params, which end the subscription
// (plan 027 §3.4). A reset is not a gap: nothing was silently lost.
type ResetParams struct {
	Subscription string      `json:"subscription"`
	Reason       ResetReason `json:"reason"`
}

// ResetReason is why a subscription ended, and what the client does next
// (plan 027 §3.4's table).
type ResetReason string

const (
	// ResetSlowConsumer: the client fell behind its budget. After
	// readiness it re-attaches with its cursor; before, when: "ready" with no
	// cursor.
	ResetSlowConsumer ResetReason = "slow_consumer"
	// ResetOmitted: a record no client can fold (over the record limit). It
	// re-attaches with no cursor.
	ResetOmitted ResetReason = "omitted"
	// ResetReplayFailed: the journal leg of a cursor replay failed; the
	// client discards what it folded since the cursor and re-attaches with
	// no cursor.
	ResetReplayFailed ResetReason = "replay_failed"
	// ResetSessionReplaced: the host swapped its engine and closes the
	// connection; the client reconnects, says hello afresh (its token is
	// void) and attaches the new session.
	ResetSessionReplaced ResetReason = "session_replaced"
	// ResetSessionClosed: the session is over, its final records delivered;
	// the host closes the connection.
	ResetSessionClosed ResetReason = "session_closed"
)

var resetReasons = []ResetReason{
	ResetSlowConsumer, ResetOmitted, ResetReplayFailed, ResetSessionReplaced, ResetSessionClosed,
}

// ResetReasons is every reset reason, in §3.4's order.
func ResetReasons() []ResetReason { return slices.Clone(resetReasons) }
