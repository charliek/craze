package protocol

import "slices"

// The methods of protocol 1 (plan 027 §3.3's table), exactly as a client
// spells them.
const (
	MethodHello             = "hello"
	MethodSessionsList      = "sessions.list"
	MethodSessionsSubscribe = "sessions.subscribe"
	MethodSessionConnect    = "session.connect"
	MethodSessionAttach     = "session.attach"
	MethodSessionDetach     = "session.detach"
	MethodSessionState      = "session.state"
	MethodSessionSnapshot   = "session.snapshot"
	MethodSessionSync       = "session.sync"
	MethodSessionPrompt     = "session.prompt"
	MethodSessionCancel     = "session.cancel"
	MethodSessionDisarm     = "session.disarm"
	MethodQueueAdd          = "session.queue.add"
	MethodQueueEdit         = "session.queue.edit"
	MethodQueueRemove       = "session.queue.remove"
	MethodQueueClear        = "session.queue.clear"
	MethodSessionSet        = "session.set"
	MethodSessionSetTitle   = "session.setTitle"
	MethodSubagentCancel    = "session.subagent.cancel"
	MethodSessionStop       = "session.stop"
	MethodAsksList          = "asks.list"
	MethodAsksGet           = "asks.get"
	MethodAsksAnswer        = "asks.answer"
)

// MethodSessionCreate is reserved for the hub (S4): protocol 1 defines no
// params or result for it yet and it has no schema, and the connection
// capability sessionCreate is false on a host and, until it serves it (plan
// 032 C15), on the hub. A host answers it like session.connect: unsupported,
// reason hub_only (plan 027 X6). It is in the method table, marked Reserved,
// so a server finds that answer there.
const MethodSessionCreate = "session.create"

// The notifications (plan 027 §3.3; plan 032 §3.6). Each carries its
// subscription id: an attachment's, from a host, or the roster subscription's,
// from the hub.
const (
	// NotifyEvent is one committed event: {subscription, seq, event}, event
	// being the record's body in the lossless codec, verbatim.
	NotifyEvent = "event"
	// NotifySynchronized says the stream has delivered through the attach's
	// cutoff: shed's Ready for the stream (§3.4).
	NotifySynchronized = "synchronized"
	// NotifyReady says the session's start has completed or failed, once, on
	// an attachment made before readiness, with the final info document.
	NotifyReady = "ready"
	// NotifyReset ends the subscription, with its reason (ResetReason); the
	// client re-attaches or, for session_closed and session_replaced, is
	// done with this connection. The hub's roster subscription ends with one
	// too: slow_consumer, or hub_closing.
	NotifyReset = "reset"
	// NotifyRoster is the hub's roster subscription's net change since the
	// subscriber's last cursor: {subscription, epoch, cursor, upserts,
	// removes} (RosterParams; plan 032 §3.6). Only the hub sends it.
	NotifyRoster = "roster"
)

// MethodInfo is one method's place on the wire.
type MethodInfo struct {
	// Name is the method, as a client sends it.
	Name string
	// Mutating says its params carry commandId (SF-12): a canonical positive
	// decimal string, per client, counted from 1. Resending a mutating
	// method's same commandId is how a client asks "did it happen?".
	Mutating bool
	// SessionScoped says its params carry sessionId, the durable craze
	// session id (SD-22), at params.sessionId: every method but hello and
	// sessions.*. The hub serves one of them, session.connect — the splice
	// that hands the connection to the session's host — and refuses every
	// other unsupported, reason host_only: it routes by splicing, never
	// method by method (plan 032 §3.6, SQ14).
	SessionScoped bool
	// Tolerant says the host ignores params fields it does not know. Only
	// hello is (§3.2); every other method refuses one, reason unknown_field.
	Tolerant bool
	// HostUnsupported is the reason a host that does not serve the method
	// answers it unsupported, whatever its params, and "" for a method every
	// host serves. The method is the hub's (sessions.subscribe,
	// roster_unsupported: rosterSubscribe is false; session.connect and
	// session.create, hub_only, X6) — which no host serves — or one a host
	// serves only where Capability says so (session.stop, stop_unsupported).
	// Such a method's schema is protocol 1's all the same, session.create's
	// aside.
	HostUnsupported Reason
	// Capability, when set, is the session capability (its wire name) whose
	// true says a host serves the method (plan 030 §3.6a): a host whose
	// capability is false answers HostUnsupported, one whose capability is
	// true serves it. session.stop's is stop — true on every `craze serve`,
	// false on a TUI-hosted session and on an older host. "" for a method
	// whose HostUnsupported, if any, holds on every host.
	Capability string
	// Reserved says protocol 1 names the method and defines nothing else of
	// it — no params, no result, no schema: session.create, the hub's (S4).
	Reserved bool
}

// CapabilityStop is the session capability that says a host serves
// session.stop (MethodInfo.Capability; SessionCapabilities.Stop's wire name).
const CapabilityStop = "stop"

var methods = []MethodInfo{
	{Name: MethodHello, Tolerant: true},
	{Name: MethodSessionsList},
	{Name: MethodSessionsSubscribe, HostUnsupported: ReasonRosterUnsupported},
	{Name: MethodSessionConnect, SessionScoped: true, HostUnsupported: ReasonHubOnly},
	{Name: MethodSessionAttach, SessionScoped: true},
	{Name: MethodSessionDetach, SessionScoped: true},
	{Name: MethodSessionState, SessionScoped: true},
	{Name: MethodSessionSnapshot, SessionScoped: true},
	{Name: MethodSessionSync, SessionScoped: true},
	{Name: MethodSessionPrompt, SessionScoped: true, Mutating: true},
	{Name: MethodSessionCancel, SessionScoped: true, Mutating: true},
	{Name: MethodSessionDisarm, SessionScoped: true, Mutating: true},
	{Name: MethodQueueAdd, SessionScoped: true, Mutating: true},
	{Name: MethodQueueEdit, SessionScoped: true, Mutating: true},
	{Name: MethodQueueRemove, SessionScoped: true, Mutating: true},
	{Name: MethodQueueClear, SessionScoped: true, Mutating: true},
	{Name: MethodSessionSet, SessionScoped: true, Mutating: true},
	{Name: MethodSessionSetTitle, SessionScoped: true, Mutating: true},
	{Name: MethodSubagentCancel, SessionScoped: true, Mutating: true},
	{Name: MethodSessionStop, SessionScoped: true, Mutating: true, HostUnsupported: ReasonStopUnsupported, Capability: CapabilityStop},
	{Name: MethodAsksList, SessionScoped: true},
	{Name: MethodAsksGet, SessionScoped: true},
	{Name: MethodAsksAnswer, SessionScoped: true, Mutating: true},
	{Name: MethodSessionCreate, Reserved: true, HostUnsupported: ReasonHubOnly},
}

// Methods is every method protocol 1 names, in §3.3's order, the reserved
// session.create last.
func Methods() []MethodInfo { return slices.Clone(methods) }

// Method is name's place on the wire, and false for a name protocol 1 does
// not know — which a host answers -32601, unsupported, reason
// unknown_method.
func Method(name string) (MethodInfo, bool) {
	for _, m := range methods {
		if m.Name == name {
			return m, true
		}
	}
	return MethodInfo{}, false
}

var notifications = []string{NotifyEvent, NotifySynchronized, NotifyReady, NotifyReset, NotifyRoster}

// Notifications is every notification protocol 1 names: a host's, in §3.3's
// order, then the hub's roster.
func Notifications() []string { return slices.Clone(notifications) }
