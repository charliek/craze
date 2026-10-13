package cli

import (
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/roster"
	"github.com/charliek/craze/internal/rundir"
	"github.com/charliek/craze/internal/sessions"
)

// The saved sessions (plan 046 B2): the hub's sessions.saved answer
// (hubCreates' Creates.Saved) and craze ps --saved's reading of the index
// with no hub are this one function, so the two never differ; the choice
// itself is internal/roster's SavedSessions, the session list's own.

// savedSessions is the saved sessions q keeps — q in the method's form (a
// workspace absolute and cleaned or "", a limit in range; 0 is the default)
// — of this process's session index (its CRAZE_HOME's sessions.jsonl, read
// whole: SavedSessions keeps the providers craze can resume itself, after it
// has taken each craze id's newest title from every row) less every session
// a live host in env's registry serves (rundir.HostsScan), a host marked
// stopping excepted (roster.RunningOfEntries): roster.SavedSessions' rows as
// the wire carries them (savedResultOf). The index is read first, then the
// registry, so a session that starts between the two is missing, never
// listed saved while it runs.
//
// An index that cannot be read is answered as sessions.createOptions answers
// one (its recentDirs): no sessions, and logf says why. A registry that
// cannot be read — or a live host's entry in it (rundir.HostsScan's
// unreadable: alive, or perhaps alive, and serving a session no one can
// name) — is refused unavailable, reason host_unreachable, as session.create
// refuses a scan it cannot vouch for and the hub a roster it cannot read
// (sessions.list): listed without the running taken out, a running session
// would be offered as saved. logf says why, since the refusal names no path.
func savedSessions(env rundir.Env, q protocol.SavedParams, logf func(string, ...any)) (protocol.SavedResult, *protocol.Error) {
	if q.Limit <= 0 {
		q.Limit = protocol.SavedLimitDefault
	}
	all, err := (&sessions.Store{}).All()
	if err != nil {
		logf("the session index cannot be read (%v): no saved sessions", err)
		return protocol.SavedResult{Sessions: []protocol.SavedSession{}}, nil
	}
	entries, unreadable, err := rundir.HostsScan(env)
	if err != nil {
		logf("the registry cannot be read (%v): no saved sessions can be told from the running", err)
		return protocol.SavedResult{}, &protocol.Error{Code: protocol.RPCRefused,
			Message: "the registry cannot be read, so the saved sessions cannot be told from the running ones",
			Data:    protocol.ErrorData{Code: protocol.CodeUnavailable, Reason: protocol.ReasonHostUnreachable}}
	}
	if len(unreadable) > 0 {
		logf("the registry entries of live hosts %v cannot be read: no saved sessions can be told from the running", unreadable)
		return protocol.SavedResult{}, &protocol.Error{Code: protocol.RPCRefused,
			Message: "the registry entry of a live session host cannot be read, so the saved sessions cannot be told from the running ones: try again",
			Data:    protocol.ErrorData{Code: protocol.CodeUnavailable, Reason: protocol.ReasonHostUnreachable}}
	}
	rows, more := roster.SavedSessions(all, roster.RunningOfEntries(entries),
		roster.SavedQuery{Workspace: q.Workspace, SubAgents: q.SubAgents, Limit: q.Limit})
	return savedResultOf(rows, more), nil
}

// savedResultOf is rows as sessions.saved carries them: each one's id
// (roster.SavedID), its title cut to protocol.SavedTitleMax characters, its
// times in UTC, a sub-agent's marked; then at most
// protocol.SavedResultBytesMax encoded in all, each row counted as the wire
// writes it (protocol.MarshalLine), the trailing rows dropped to keep within
// it, which more then says too. The rows fit the wire's row bounds already:
// SavedSessions skips any that does not.
func savedResultOf(rows []sessions.Row, more bool) protocol.SavedResult {
	// What the result costs with no session: {"sessions":[],"more":false}.
	size := len(`{"sessions":[],"more":false}`)
	out := protocol.SavedResult{Sessions: make([]protocol.SavedSession, 0, len(rows)), More: more}
	for _, r := range rows {
		s := protocol.SavedSession{
			ID:                roster.SavedID(r),
			Provider:          r.Provider,
			ProviderSessionID: r.SessionID,
			Workspace:         r.CWD,
			Title:             clipRunes(r.Title, protocol.SavedTitleMax),
			CreatedAt:         r.CreatedAt.UTC(),
			UpdatedAt:         r.UpdatedAt.UTC(),
			SubAgent:          r.Unattended != nil,
		}
		if r.CrazeID != "" {
			s.CrazeSessionID = r.CrazeID
		}
		b, err := protocol.MarshalLine(s)
		if err != nil {
			// SavedSessions offers no row the wire cannot write
			// (savedRowFits); one that cannot be written all the same is
			// left out, and said, never dropped silently.
			out.More = true
			break
		}
		cost := len(b) - 1 // its newline is not the result's
		if len(out.Sessions) > 0 {
			cost++ // the comma before it
		}
		if size+cost > protocol.SavedResultBytesMax {
			out.More = true
			break
		}
		size += cost
		out.Sessions = append(out.Sessions, s)
	}
	return out
}

// clipRunes is s cut to at most n runes — an invalid byte one, as the wire
// writes it (U+FFFD) and as a schema counts it.
func clipRunes(s string, n int) string {
	i := 0
	for at := range s {
		if i == n {
			return s[:at]
		}
		i++
	}
	return s
}
