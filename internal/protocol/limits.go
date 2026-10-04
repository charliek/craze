package protocol

import "slices"

// ProtocolVersion is protocol 1: one integer, decoupled from the binary's
// version (plan 027 §3.3). A client sends every version it speaks in hello's
// protocols, and the host answers with the highest it shares.
const ProtocolVersion = 1

// SupportedProtocols is every protocol version this build speaks, lowest
// first: [1]. A hello that shares none is refused bad_request, reason
// protocol_version, with data.result {supported: SupportedProtocols()}.
func SupportedProtocols() []int { return slices.Clone(supportedProtocols) }

var supportedProtocols = []int{ProtocolVersion}

// The limits (plan 027 §3.2, §3.6, §3.7). Each is a number a peer is told
// (hello's limits) or a bound the server and the client hold; a byte count is
// in bytes.
const (
	// InboundLineMax is the longest line a host reads: 4 MiB, "\n" (and a
	// "\r" before it) not counted. A longer one is discarded up to its newline
	// and answered -32600, reason line_too_long, id null; the connection
	// stays open (§3.2).
	InboundLineMax = 4 << 20
	// OutboundLineMax is the longest line a host writes and a client must
	// accept: 16 MiB, for every line. Every source of a line is bounded under
	// it (§3.2); a reply that would still be longer is replaced by failed,
	// reason response_too_large, never truncated.
	OutboundLineMax = 16 << 20
	// SnapshotBytesMax caps a snapshot's byte budget, the attach budget's
	// snapshotBytes and session.snapshot's alike: 8 MiB (§3.2). A request for
	// more is held to it.
	SnapshotBytesMax = 8 << 20
	// AskItemCap is the per-string cap on an asks.get record's body: the
	// snapshot's own ItemCap, 256 KiB (§3.2; transcript.ItemCap, which the
	// snapshot codec's twin test holds this equal to). A record whose body was
	// cut says so in truncated.
	AskItemCap = 256 << 10
	// WriterQueueBytes is a connection's writer budget: 32 MiB, twice the
	// longest line, so any legal line fits in an empty queue. Every outbound
	// byte counts, replies and resets included (§3.7).
	WriterQueueBytes = 32 << 20
	// ResetReserveBytes is held back in the writer budget for a
	// subscription's final reset, so the reset can always be queued (§3.7).
	ResetReserveBytes = 1 << 10
	// RequestsPerConnection is how many requests a connection may have
	// admitted and not yet answered on the socket: its reader stops reading
	// at 16, and a request's slot is held until its reply is written (§3.7).
	RequestsPerConnection = 16
	// CommandsPerHost is how many commands may be in flight across all of a
	// host's connections: past 64 a command is refused unavailable, reason
	// busy — not run, never stored, resent with the same id (§3.6).
	CommandsPerHost = 64
	// ReattachesPerEpisode bounds a client's re-attaches after resets: 8 per
	// episode, the count starting again whenever its stream reaches
	// synchronized (§3.14).
	ReattachesPerEpisode = 8
	// NativeDirMax bounds session.models.refresh's nativeDir (plan 034 §3.4):
	// 4096 characters, Linux's PATH_MAX, as session.create's cwd is bounded.
	NativeDirMax = 4096
)

// The hub roster's bounds (plan 032 §3.6), which the hub enforces (C11) and
// the schema states (info.json's rosterRow, rosterHost and forwardedRow;
// maxItems on every roster list), so that a whole roster — RosterRowsMax
// roster rows of at most RosterEntryBytesMax encoded bytes each, in a reply
// whose request id is at most RosterRequestIDBytesMax encoded bytes, or a
// roster notification, the hub's own epoch and subscription id each at most
// 64 bytes — is always one line under OutboundLineMax
// (TestAFullRosterFitsOneLine builds it). The hub keeps every roster row
// within them, in this order, marking each roster row it changes
// approximate:
//
//  1. a host string over its bound (RosterCrazeVersionMax, RosterProviderMax,
//     RosterWorkspaceMax — characters, as JSON Schema's maxLength counts
//     them) is cut to it at a character boundary;
//  2. a host row whose JSON encoding is over RosterRowBytesMax is dropped
//     (the roster row's row absent);
//  3. a roster row whose JSON encoding is still over RosterEntryBytesMax —
//     only a row near its bound beside host strings that encode to six
//     bytes a character (control characters, escaped) can be — has its row
//     dropped. Without its row a roster row always fits: its strings at
//     their bounds encode to at most 6 × (128 + 64 + 4096) bytes.
//
// A host id is 12 hex digits and a craze session id a token of at most 128
// characters ([A-Za-z0-9._-]): a host whose ids are not is not listed.
const (
	// RosterCrazeVersionMax bounds a roster row's host.crazeVersion: 128
	// characters.
	RosterCrazeVersionMax = 128
	// RosterProviderMax bounds a roster row's host.provider: 64 characters.
	RosterProviderMax = 64
	// RosterWorkspaceMax bounds a roster row's host.workspace: 4096
	// characters, Linux's PATH_MAX.
	RosterWorkspaceMax = 4096
	// RosterRowBytesMax bounds the host row a roster row carries, as JSON:
	// 16 KiB.
	RosterRowBytesMax = 16 << 10
	// RosterEntryBytesMax bounds a whole roster row, as JSON: 32,000 bytes.
	RosterEntryBytesMax = 32_000
	// RosterRowsMax is the most rows a hub roster holds (plan 032 §3.6): 512,
	// in hostId order, the result's truncated set beyond — and the most a
	// roster notification's upserts and removes each hold.
	RosterRowsMax = 512
	// RosterRequestIDBytesMax is the longest request id, as JSON, the
	// roster's size guarantee covers: 1 KiB. A client that sends a longer one
	// for sessions.list or sessions.subscribe may meet response_too_large,
	// as any reply over OutboundLineMax does.
	RosterRequestIDBytesMax = 1 << 10
)
