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
// params or result for it and it has no schema, and the connection
// capability sessionCreate is false on a host. A host answers it like
// session.connect: unsupported, reason hub_only (plan 027 X6). It is in the
// method table, marked Reserved, so a server finds that answer there.
const MethodSessionCreate = "session.create"

// The host's notifications (plan 027 §3.3). Each carries its subscription id.
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
	// done with this connection.
	NotifyReset = "reset"
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
	// session id (SD-22), at params.sessionId, so a hub can route a method it
	// does not know (SQ14): every method but hello and sessions.*.
	SessionScoped bool
	// Tolerant says the host ignores params fields it does not know. Only
	// hello is (§3.2); every other method refuses one, reason unknown_field.
	Tolerant bool
	// HostUnsupported is the reason a TUI-hosted session's host — every host
	// in S2 — answers the method unsupported, whatever its params, and ""
	// for a method it serves. The method is the hub's (sessions.subscribe,
	// roster_unsupported: rosterSubscribe is false; session.connect and
	// session.create, hub_only, X6) or S4's headless host's (session.stop,
	// stop_unsupported: the session capability stop is false). Such a
	// method's schema is protocol 1's all the same, session.create's aside.
	HostUnsupported Reason
	// Reserved says protocol 1 names the method and defines nothing else of
	// it — no params, no result, no schema: session.create, the hub's (S4).
	Reserved bool
}

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
	{Name: MethodSessionStop, SessionScoped: true, Mutating: true, HostUnsupported: ReasonStopUnsupported},
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

var notifications = []string{NotifyEvent, NotifySynchronized, NotifyReady, NotifyReset}

// Notifications is every notification a host sends, in §3.3's order.
func Notifications() []string { return slices.Clone(notifications) }
