package engine

import (
	"context"

	"github.com/charliek/craze/internal/agent"
)

// RefreshesModels reports whether the session can take up models funded while
// it runs (plan 034 §3.4, Q17): it implements agent.ModelsRefresher, which a
// native session does and an ACP session does not. It is a property of the
// session the engine was built over, fixed for the engine's life — the
// session capability modelsRefresh on the wire, and the TUI's in-process
// Info alike — and it waits on nothing.
func (e *Engine) RefreshesModels() bool {
	_, ok := e.sess.(agent.ModelsRefresher)
	return ok
}

// RefreshModels asks the session to take up the models funded since it opened
// — a key saved, the ChatGPT plan signed in to or out of — now rather than at
// its next turn (plan 034 §3.4, Q14's trigger b). It is the session's own verb
// (agent.ModelsRefresher), and the engine adds only the door, as it does for
// CancelSubagent; nativeDir is the caller's own native directory, "" for
// none, which the answer's SameDir compares with the session's.
//
// The answers:
//
//   - the session's ModelsRefresh: applied (the reload published the list as
//     a catalog delta before it returned), current, pending (a turn or a
//     load's replay is running; the session takes the change up as it ends),
//     or failed (the files could not be read; the list is as it was);
//   - ModelsUnsupported, nil: the session has no models to refresh while it
//     runs (an ACP session; RefreshesModels is false). Nothing was asked;
//   - ErrNotAccepting, code not_accepting: the engine is closed or stopped,
//     or its session has not started (starting, or its start failed). Nothing
//     was asked, and a client may ask again once the session is up.
//
// It is not a command: it carries no Command, keeps no receipt and is
// idempotent — a second call with nothing changed answers current — so a
// resend asks again. It is gated on nothing else: a replay, a turn and a close
// fence are the session's to answer (pending, or the refresh is simply taken
// up). It blocks while the session reads its files — local file I/O, never the
// network, never a turn — so it is called from a tea.Cmd or a handler, never
// from the primary's own reader; e.mu is taken for the one read of the gate
// and released before the session is called.
func (e *Engine) RefreshModels(ctx context.Context, nativeDir string) (agent.ModelsRefresh, error) {
	e.mu.Lock()
	refused := e.closed || e.stopped || e.startFailed || e.activity == ActivityStarting
	e.mu.Unlock()
	if refused {
		return agent.ModelsRefresh{}, ErrNotAccepting
	}
	r, ok := e.sess.(agent.ModelsRefresher)
	if !ok {
		return agent.ModelsRefresh{Status: agent.ModelsUnsupported}, nil
	}
	return r.RefreshModels(ctx, nativeDir)
}
