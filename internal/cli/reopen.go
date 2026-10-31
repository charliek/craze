package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/charliek/craze/internal/errkind"
	"github.com/charliek/craze/internal/hub"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/roster"
	"github.com/charliek/craze/internal/rundir"
	"github.com/charliek/craze/internal/sessions"
)

// Reopening a saved session through the hub (plan 046 B3, B6): the hub's
// session.reopen resolves its id here (reopenTarget, hubCreates'
// Creates.Reopen) — the hub imports neither the session index nor the
// providers — and craze new --load asks for one (runNewLoad).

// reopenTarget resolves session.reopen's id against this process's session
// index — its CRAZE_HOME's, which in the hub is the hub's own — to the row a
// load of it loads, the row craze serve --load takes (loadRow): a craze id
// names the newest row that carries it (sessions.Store.ByCrazeID, every
// provider's), <provider>:<providerSessionId> the row of that key. A legacy
// key whose row has been given a craze id since (a host loaded it and minted
// one under its claim) answers that craze id, and the craze id's newest row:
// the session is loaded by it, so every reopen of it, by either id, contends
// for one claim. That row decides, as it decides whether sessions.saved lists
// the session (roster.SavedSessions): one that cannot be resumed — of a
// provider this craze cannot load, or malformed (roster.Unresumable) — is not
// reopened in an older row's place. The answer is what the hub's reopen joins
// or loads (hub.Reopening): the load's id (roster.SavedID), the session's
// keys and workspace, the row's reload settings — a sub-agent's (unattended)
// row's recorded start settings and --unattended, as craze send reloads it
// (reloadFlags), and none for a person's row, whose reloadFlags add nothing a
// host takes as a setting — and the spawn path's check of the world,
// savedRunnable, called there and only there. Nothing is stat'ed here.
//
// Refused: an id no row has, unknown_session; a deciding row that cannot be
// resumed, and an index that cannot be read, not_accepting, reason
// not_resumable — the index's error said through logf, not in the refusal,
// which names no path.
func reopenTarget(id string, logf func(string, ...any)) (hub.Reopening, *protocol.Error) {
	index := &sessions.Store{}
	rows, err := index.ByCrazeID()
	var (
		row sessions.Row
		ok  bool
	)
	if err == nil {
		if prov, sid, legacy := strings.Cut(id, ":"); legacy {
			row, ok, err = index.Find(prov, sid)
		} else {
			row, ok = rows[id]
		}
	}
	if err != nil {
		logf("the session index cannot be read: %v", err)
		return hub.Reopening{}, reopenRefusal(protocol.CodeNotAccepting, protocol.ReasonNotResumable,
			"the session index cannot be read, so that session cannot be reopened")
	}
	if !ok {
		return hub.Reopening{}, reopenRefusal(protocol.CodeUnknownSession, protocol.ReasonUnknownSession,
			"no saved session %s", sanitizeLine(id))
	}
	if newest, found := rows[row.CrazeID]; row.CrazeID != "" && found {
		row = newest
	}
	if why := roster.Unresumable(row); why != "" {
		return hub.Reopening{}, reopenRefusal(protocol.CodeNotAccepting, protocol.ReasonNotResumable, "%s", why)
	}
	flags := reloadFlags(row)
	spec := serveSpec(&flags, "", "", "")
	return hub.Reopening{
		Load:              loadArg(row),
		CrazeID:           row.CrazeID,
		Provider:          row.Provider,
		ProviderSessionID: row.SessionID,
		Workspace:         row.CWD,
		Model:             spec.Model,
		Effort:            spec.Effort,
		NoForce:           spec.NoForce,
		Ask:               spec.Ask,
		Plan:              spec.Plan,
		Unattended:        row.Unattended != nil,
		Runnable: func() error {
			if _, err := savedRunnable(row); err != nil {
				return errors.New(strings.TrimPrefix(err.Error(), "craze: "))
			}
			return nil
		},
	}, nil
}

// reopenRefusal is a craze-level refusal of session.reopen: -32000 with its
// code and reason.
func reopenRefusal(code protocol.Code, reason protocol.Reason, format string, args ...any) *protocol.Error {
	return &protocol.Error{Code: protocol.RPCRefused, Message: fmt.Sprintf(format, args...),
		Data: protocol.ErrorData{Code: code, Reason: reason}}
}

// craze new --load (plan 046 B6): a saved session brought back through the
// hub — Ensure, needing reopen (an older hub is its LacksError, exit 1) —
// by session.reopen, answered once its start is done: `reopened <id> in
// <dir>`, or `already running <id> in <dir>` when a host served it already.
// The id is the one craze ps --saved prints, or any the short-id tiers take
// (newLoadRow). It starts nothing new and sends no prompt: craze send, craze
// attach and the session list take the session from there.

const (
	// newLoadBudget bounds craze new --load: the hub's reopen takes up to
	// 180 s (its start up to 150 s), and its Ensure and connection their own.
	newLoadBudget = 4 * time.Minute
)

// newLoadNot is every flag craze new --load refuses (a usage error): what
// starts a new session, and what --wait takes. A reopen sends no prompt, and
// its session's provider, directory and settings are its own.
var newLoadNot = []string{"prompt-file", "remove-prompt-file", "provider", "model", "effort", "fast", "no-fast", "no-force",
	"title", "dir", "wait", "plan", "ask", "timeout", "max-message", "exit-after-turn", "session-file"}

// runNewLoad is craze new --load (the comment above).
func runNewLoad(cmd *cobra.Command, f *newFlags, args []string) error {
	if len(args) > 0 {
		return usagef("craze new: --load takes no prompt: reopen the session, then craze send it one")
	}
	for _, name := range newLoadNot {
		if cmd.Flags().Changed(name) {
			flag := "--" + name
			if name == "dir" {
				flag = "-C/--dir"
			}
			return usagef("craze new: --load does not take %s: a reopened session keeps its own", flag)
		}
	}
	id := strings.TrimSpace(f.load)
	if id == "" {
		return usagef("craze new: --load needs a session id (craze ps --saved lists them)")
	}
	out := cmd.OutOrStdout()
	ignoreSIGPIPE()
	fail := newFail(out, f.json)
	// The budget runs from here, the index's read under it.
	ctx, cancel := context.WithTimeout(context.Background(), newLoadBudget)
	defer cancel()
	row, err := newLoadRow(id)
	if err != nil {
		return fail(errkind.Config, err, "%s", err.Error())
	}
	env := rundir.ProcessEnv()
	raw, hello, err := reopenThroughHub(ctx, env, roster.SavedID(row))
	if err != nil {
		return fail(hubFailureKind(err), err, "%s", newFailure(err, hello))
	}
	var res protocol.ReopenResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return fail(errkind.Internal, nil, "the hub's answer: %v", err)
	}
	if f.json {
		_, err := fmt.Fprintf(out, "%s\n", raw)
		return err
	}
	ws := res.Session.Host.Workspace
	if ws == "" {
		ws = row.CWD
	}
	what := "reopened"
	if res.Joined {
		what = "already running"
	}
	_, err = fmt.Fprintf(out, "%s %s in %s\n", what, psShort(res.Session.SessionID), psLine(psTilde(env.Home, ws)))
	return err
}

// reopenThroughHub is id's reopen through the hub, as createThroughHub is a
// create's: Ensure, needing reopen, within hubBudget, then the reopen within
// ctx; a connection that ended before the answer is tried once more, the same
// id, through Ensure again — a reopen is safe to send again (its retry safety
// is the session's claim) — and only that. It answers too the hello of the
// hub that took the reopen's own connection, for newFailure (plan 046 U2).
func reopenThroughHub(ctx context.Context, env rundir.Env, id string) (json.RawMessage, protocol.HubHelloResult, error) {
	for retried := false; ; retried = true {
		ectx, cancel := context.WithTimeout(ctx, hubBudget)
		sock, err := hub.Ensure(ectx, env, hub.ReopenNeed())
		cancel()
		if err != nil {
			return nil, protocol.HubHelloResult{}, err
		}
		raw, hello, err := hub.Reopen(ctx, sock, newClient, id)
		if err == nil || retried || !errors.Is(err, hub.ErrUnanswered) || ctx.Err() != nil {
			return raw, hello, err
		}
	}
}

// newLoadRow is the saved session id names (plan 046 B6), from this
// CRAZE_HOME's session index: its candidates as sessions.saved's (one row per
// craze id, the newest; a legacy row as itself; of a provider craze can
// resume; sub-agents' too) — running sessions included, since a reopen of one
// joins it — matched by the short-session-id tiers, the first that matches
// deciding: an exact id — a craze id, the sessions.saved id, or a provider
// session id, whole or as <provider>:<providerSessionId>, of any of the
// session's rows (a legacy row given a craze id since is still named by its
// old key) — then the end of a craze id — or, for a legacy row with none, of
// its provider session id, the one craze ps --saved shows for it — then a
// craze id's beginning, each past the first needing four characters. None is
// `no saved session <id>`; several, exit 1 naming them.
func newLoadRow(id string) (sessions.Row, error) {
	all, err := (&sessions.Store{}).All()
	if err != nil {
		return sessions.Row{}, fmt.Errorf("the session index: %s", sanitizeLine(err.Error()))
	}
	rows, _ := roster.SavedSessions(all, roster.Running{}, roster.SavedQuery{SubAgents: true})
	// Every provider session id, and its <provider>:<id>, of each craze id's
	// rows: what names the session whole besides its craze id.
	aliases := map[string]map[string]bool{}
	for _, r := range all {
		if r.CrazeID == "" {
			continue
		}
		if aliases[r.CrazeID] == nil {
			aliases[r.CrazeID] = map[string]bool{}
		}
		aliases[r.CrazeID][r.SessionID] = true
		aliases[r.CrazeID][r.Provider+":"+r.SessionID] = true
	}
	matches := matchTiers(rows, id,
		func(r sessions.Row) bool {
			return roster.SavedID(r) == id || r.CrazeID == id || r.SessionID == id || aliases[r.CrazeID][id]
		},
		func(r sessions.Row) bool {
			if r.CrazeID != "" {
				return strings.HasSuffix(r.CrazeID, id)
			}
			return strings.HasSuffix(r.SessionID, id)
		},
		func(r sessions.Row) bool { return r.CrazeID != "" && strings.HasPrefix(r.CrazeID, id) },
	)
	switch len(matches) {
	case 0:
		return sessions.Row{}, fmt.Errorf("no saved session %s (craze ps --saved lists them)", sanitizeLine(id))
	case 1:
		return matches[0], nil
	}
	named := make([]string, len(matches))
	for i, m := range matches {
		short := psShort(m.CrazeID)
		if m.CrazeID == "" {
			short = psShortRunes(m.SessionID)
		}
		named[i] = fmt.Sprintf("%s (%s, %s)", short, m.Provider, m.CWD)
	}
	return sessions.Row{}, fmt.Errorf("%d saved sessions match %s: %s", len(matches), sanitizeLine(id), sanitizeLine(strings.Join(named, ", ")))
}
