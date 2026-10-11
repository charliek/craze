package cli

import (
	"context"
	"os"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/remote"
	"github.com/charliek/craze/internal/rundir"
	"github.com/charliek/craze/internal/sessions"
	"github.com/charliek/craze/internal/version"
)

// Session ids on the command line (plan 041 §3.1 "Ids"): any form craze ps
// prints — the last eight characters of a craze id, a prefix, the whole id,
// a provider session id or a host id — resolved over the live hosts first, by
// matchSession's tiers (bridge.go), and the full craze id is what goes on the
// wire (G9). An id that names no live host is the session index's to place:
// a session it has a row for has ended (and `craze wait` says how to reload
// it), one it has none for is no session at all. The index's own resolution
// for a reload is `craze send`'s (§3.4), not this file's.

// sessionArg is the argument check of a command that takes one session id
// (wait, cancel, stop): more than one is a usage error here, before the
// root's hook runs; none is RunE's (sessionID), so the removed config
// variable's refusal, which the hook makes, still answers a bare `craze wait`
// (root.go: arguments are validated before the hook).
func sessionArg(cmd *cobra.Command, args []string) error {
	if len(args) <= 1 {
		return nil
	}
	return usagef("%s: takes one session id; see %s --help", cmd.CommandPath(), cmd.CommandPath())
}

// sessionID is the one session id args holds, or the usage error for none.
func sessionID(cmd *cobra.Command, args []string) (string, error) {
	if len(args) == 0 || strings.TrimSpace(args[0]) == "" {
		return "", usagef("%s: needs a session id (craze ps lists them); see %s --help", cmd.CommandPath(), cmd.CommandPath())
	}
	return args[0], nil
}

// resolveLive is the one live host serving the session id names (the file's
// comment), or why there is none: an id that is no valid token is a usage
// error; several live sessions matching is exit 1, naming them; none is
// liveGone's answer. what is the command, "craze wait", as its errors start;
// reload says whether the refusal of an ended session names `craze send`.
func resolveLive(env rundir.Env, what, id string, reload bool) (rundir.Entry, error) {
	if !rundir.ValidToken(id) {
		// Refused before a registry read builds a path (the bridge's rule).
		return rundir.Entry{}, usagef("%s: that is not a session id (craze ps lists them)", what)
	}
	entries, err := rundir.Hosts(env)
	if err != nil {
		return rundir.Entry{}, exitf(1, "%s: %s", what, sanitizeLine(err.Error()))
	}
	matches := matchSession(entries, id)
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return rundir.Entry{}, liveGone(what, id, reload)
	}
	return rundir.Entry{}, exitf(1, "%s: %d sessions match %s: %s", what, len(matches), id, sanitizeLine(formatEntries(matches)))
}

// liveGone is the refusal of an id no live host serves (§3.1): `the session
// has ended` when the session index has a row for it — with, when reload,
// `craze send <short> <prompt>`, which reloads it (§3.4) — and `no session
// <id>` when it has none (a gx session, which craze cannot load again, has no
// row). An index that cannot be read places nothing: the id is no session.
func liveGone(what, id string, reload bool) error {
	matches := matchCrazeID(indexCrazeIDs(), id)
	switch {
	case len(matches) == 0:
		return exitf(1, "%s: no session %s", what, id)
	case len(matches) > 1:
		return exitf(1, "%s: the session has ended: %d ended sessions match %s", what, len(matches), id)
	case reload:
		return exitf(1, "%s: the session has ended%s", what, reloadHint(matches))
	}
	return exitf(1, "%s: the session has ended", what)
}

// reloadHint is how an ended session is loaded again — ` (craze send <short>
// <prompt> reloads it)` (§3.4) — when the session index's craze ids matching
// it are exactly one; "" otherwise.
func reloadHint(matches []string) string {
	if len(matches) != 1 {
		return ""
	}
	return " (craze send " + psShort(matches[0]) + " <prompt> reloads it)"
}

// sessionLive reports whether a live host still serves session id.
func sessionLive(id string) bool {
	entries, err := rundir.Hosts(rundir.ProcessEnv())
	if err != nil {
		// Cannot tell: say the connection was lost, which sends the user to
		// the handle, not to a reload.
		return true
	}
	return slices.ContainsFunc(entries, func(e rundir.Entry) bool { return e.CrazeSessionID == id })
}

// indexCrazeIDs is every craze id the session index holds a row for.
func indexCrazeIDs() []string {
	rows, err := (&sessions.Store{}).ByCrazeID()
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(rows))
	for id := range rows {
		out = append(out, id)
	}
	return out
}

// matchCrazeID is every craze id among ids that id names, by matchSession's
// tiers for a craze id (matchTiers): exactly, else as a suffix, else as a
// prefix.
func matchCrazeID(ids []string, id string) []string {
	return matchTiers(ids, id,
		func(c string) bool { return c == id },
		func(c string) bool { return strings.HasSuffix(c, id) },
		func(c string) bool { return strings.HasPrefix(c, id) },
	)
}

// entrySession is the craze session id a resolved entry serves, for the
// wire: its registry entry's, which a host writes once its engine has one —
// an entry without one serves a session not yet started, and names nothing a
// session-scoped method could take.
func entrySession(what string, e rundir.Entry) (string, error) {
	if e.CrazeSessionID == "" {
		return "", exitf(1, "%s: session %s has not started yet; try again in a moment", what, sanitizeLine(entryID(e)))
	}
	return e.CrazeSessionID, nil
}

// dialCLI dials the host e names as client what ("craze wait"), its peer
// checked, within dialTimeout: wait's, cancel's and stop's one connection.
func dialCLI(ctx context.Context, e rundir.Entry, what string) (*remote.Client, error) {
	ctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	return remote.Dial(ctx, e.Socket, remote.Options{
		Client:    protocol.ClientInfo{Kind: "cli", Name: what, Version: version.Version},
		PeerCheck: rundir.DialCheck(os.Geteuid()),
	})
}

// unreachable is a resolved session whose socket did not answer.
func unreachable(what string, e rundir.Entry, err error) error {
	return exitf(1, "%s: session %s is unreachable: %s", what, psShort(entryID(e)), sanitizeLine(err.Error()))
}
