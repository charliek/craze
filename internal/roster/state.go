package roster

import (
	"time"

	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/protocol"
)

// State is a running session's state, the session list's rule (plan 030
// §3.10's table) — what the TUI's list groups a row by, and what `craze ps`
// prints (plan 032 §3.12): one rule, here, for both. A host that answered is
// in the state the engine's own precedence gives its row (engine.RowStateOf,
// X64); one the roster has no answer from yet is starting; one whose socket
// does not answer is unreachable. The order is the one `craze ps` lists by.
type State int

const (
	// StateNeedsYou: an ask is open (pendingAsks > 0).
	StateNeedsYou State = iota
	// StateWorking: a turn runs — craze's own, or the agent's (foreignTurn)
	// — or the engine is starting, replaying or closing.
	StateWorking
	// StateStarting: no answer of the host's yet. Its registry entry names no
	// session (or one not ready), or the roster has not heard from the host
	// since it did (Connecting). The list draws it with the working rows,
	// Starting… or Connecting….
	StateStarting
	// StateFailed: the session's start failed, or its last turn did — or the
	// engine is in its error state with neither a last turn nor a foreign
	// turn, a failure whose ending is still on its way.
	StateFailed
	// StateIdle: anything else.
	StateIdle
	// StateUnreachable: the registry lists the host, and its socket does not
	// answer — never shown as saved (§3.9).
	StateUnreachable
)

// String is the state as `craze ps` prints it.
func (s State) String() string {
	switch s {
	case StateNeedsYou:
		return "needs you"
	case StateWorking:
		return "working"
	case StateStarting:
		return "starting"
	case StateFailed:
		return "failed"
	case StateIdle:
		return "idle"
	case StateUnreachable:
		return "unreachable"
	}
	return "state(?)"
}

// State is r's state: unreachable when its last attempt failed, starting
// while the roster has no answer of the session its entry names, and
// otherwise its session's (Session.State).
func (r Row) State() State {
	switch {
	case r.Status == Unreachable:
		return StateUnreachable
	case r.Session == nil || r.Status == Connecting:
		return StateStarting
	}
	return r.Session.State()
}

// Since is when r entered its state: its session's Since (a row fact, zero
// from an older host) — the last one read, for an unreachable host — and,
// while it is starting, when its host started.
func (r Row) Since() time.Time {
	switch r.State() {
	case StateUnreachable:
		if r.Session != nil {
			return r.Session.Since
		}
		return time.Time{}
	case StateStarting:
		return r.Host.StartedAt
	}
	return r.Session.Since
}

// State is an answered session's state, by the engine's own precedence
// (engine.RowStateOf): needs you, failed, working or idle. An older host's
// row (no row facts) has no StartFailed, which reads as false.
func (s *Session) State() State {
	in := engine.State{Activity: s.Activity, PendingAsks: s.PendingAsks, StartFailed: s.StartFailed,
		LastTurn: s.LastTurn, ForeignTurn: s.ForeignTurn}
	switch engine.RowStateOf(in) {
	case engine.RowNeedsYou:
		return StateNeedsYou
	case engine.RowFailed:
		return StateFailed
	case engine.RowWorking:
		return StateWorking
	}
	return StateIdle
}

// FromRosterRow is a hub's roster row (plan 032 §3.6) as the list's roster
// has a row: its host from row.host, its craze session id from sessionId,
// its status, the host's craze version, and its session decoded tolerantly
// from row (protocol.RosterRow.SessionRow) — nil while the hub has read
// none. Raw is the row as the hub forwarded it. No socket crosses the wire,
// so Host.Socket is "": a caller that dials resolves it from the registry by
// Host.ID. A row that is present and does not decode is an error.
func FromRosterRow(r protocol.RosterRow) (Row, error) {
	out := Row{
		Host: Host{ID: r.HostID, PID: r.Host.PID, StartedAt: r.Host.StartedAt, Protocol: r.Host.Protocol,
			CrazeSessionID: r.SessionID, Provider: r.Host.Provider, Workspace: r.Host.Workspace, Ready: r.Host.Ready},
		Status:  statusFromWire(r.Status),
		Version: r.Host.CrazeVersion,
		Raw:     r.Row,
	}
	row, ok, err := r.SessionRow()
	if err != nil {
		return Row{}, err
	}
	if ok {
		out.Session = sessionOf(row)
		out.Host.ProviderSessionID, out.Host.Incarnation = row.ProviderSessionID, row.Incarnation
	}
	return out, nil
}

// statusFromWire is a roster row's status in the roster's words: one this
// build does not know reads as connecting — not yet known either way.
func statusFromWire(s protocol.RosterStatus) Status {
	switch s {
	case protocol.RosterReachable:
		return Reachable
	case protocol.RosterUnreachable:
		return Unreachable
	}
	return Connecting
}
