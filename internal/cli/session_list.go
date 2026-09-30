package cli

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/backend"
	"github.com/charliek/craze/internal/roster"
	"github.com/charliek/craze/internal/rundir"
	"github.com/charliek/craze/internal/sessions"
	"github.com/charliek/craze/internal/tui"
)

// sessionList is tui.Config.Sessions on the launch path (plan 030 §3.9): the
// list's roster over this user's registry and this CRAZE_HOME's index; a
// running session opened over its host's socket; a saved one loaded by a
// host spawned for it (openSaved, as the resume picker's choice is loaded); a
// new one spawned; and a stop sent over a connection of its own. Only a launch
// has one: under the opt-out the TUI hosts its session in process, and
// closing a backend there would close its engine, so there is no list.
//
// It lives as long as its launcher: nothing starts after finish, whatever is
// in flight then is cancelled, and a backend it answered whose session never
// came up in the TUI is closed there (finish) — a running one's host, never
// this launch's, is left alone. A host it spawned is this launch's to stop
// (listHost): finish stops it unless a session opened on it came up in the
// TUI or the list left it running (LeaveRunning), as it stops a launch's
// spawn nobody adopted (sol r17-c9 1); whose it is is decided per host, under
// the launcher's lock, never by whichever caller got there first (sol r20-c9r
// 1–2).
type sessionList struct{ l *launcher }

// It starts new sessions from the list (plan 030 §3.13): the list's input is
// drawn under the rows.
var _ tui.SessionStarter = sessionList{}

// Roster opens the list's poller: the registry of this user (the launcher's
// env, the process's), the index of this CRAZE_HOME, and only the providers
// this build knows offered as saved.
func (s sessionList) Roster() tui.SessionRoster {
	return roster.Open(s.l.env, &sessions.Store{KnownProvider: knownProvider})
}

// RecentDirs is the `@` picker's recent directories (plan 030 §3.15): up to
// n workspaces of this CRAZE_HOME's index, newest first, that are still
// directories (sessions.Store.RecentDirs).
func (s sessionList) RecentDirs(n int) ([]sessions.RecentDir, error) {
	return (&sessions.Store{KnownProvider: knownProvider}).RecentDirs(n)
}

// Open is ref's session as a backend the TUI adopts: a running one's host
// dialled and attached (open) — a host Spawn started stays this launch's to
// decide (listHost) — a saved one's row loaded (openSaved: the host serving
// it already if there is one, else a spawn with --load in the row's own
// workspace).
func (s sessionList) Open(ref roster.Ref) (backend.Backend, error) {
	if ref.Saved != nil {
		return s.l.openSaved(*ref.Saved)
	}
	return s.l.open(ref.Host.Entry())
}

// Spawn starts a host for spec's new session and answers its ref, for Open
// (spawnFor): the host is recorded as this launch's to stop (listHost).
func (s sessionList) Spawn(spec tui.SpawnSpec) (roster.Ref, error) { return s.l.spawnFor(spec) }

// LeaveRunning keeps the host ref names, which Spawn started, running at
// quit: finish no longer stops it (leaveRunning). Its error says it cannot —
// craze is exiting and finish has decided, or the host was stopped because
// it could not be reached.
func (s sessionList) LeaveRunning(ref roster.Ref) error {
	if ref.Saved != nil {
		return nil
	}
	return s.l.leaveRunning(ref.Host.ID)
}

// errStopSaved is Stop of a saved session: nothing runs to stop.
var errStopSaved = errors.New("craze: that session is not running")

// Stop sends session.stop to ref's host over a connection of its own and
// answers once the host has taken it (stopHost): the host's own stop
// sequence ends the session. A host that cannot stop — an older craze — is
// the protocol's refusal, stop_unsupported.
func (s sessionList) Stop(ref roster.Ref) error {
	if ref.Saved != nil {
		return errStopSaved
	}
	return stopHost(ref.Host.Entry())
}

// Cancel clears ref's queue and cancels its running turn on its host, over a
// connection of its own (cancelHost): the list's ctrl+x on a working or
// asking row (plan 030 §3.10).
func (s sessionList) Cancel(ref roster.Ref) error {
	if ref.Saved != nil {
		return errStopSaved
	}
	return cancelHost(ref.Host.Entry())
}

// open dials the host e names as the TUI's client and attaches — as the
// direct reattach does, so a host that is stopping (it answers hello and
// refuses every attach closing) is an error here and never the TUI's failed
// start (X55) — within dialTimeout, and records it for finish: closed if its
// session never comes up in the TUI. A host this launch's Spawn started
// stays the launch's to decide (listHost): stopped at finish unless a session
// opened on it came up in the TUI or the list left it running, and stopped at
// once if it cannot be opened — nobody else would ever stop it (dialHost's
// rule) — but only when no other open of it is in flight or holds it open
// still, and no session on it came up in the TUI, so a caller that failed
// never stops a session another opened (sol r20-c9r 1). A backend it answered
// tells the launcher when it is closed (launchedBackend.release), so one the
// TUI has let go of holds nothing (sol r21-c10r 1). Any other host is never
// this launch's to stop.
func (l *launcher) open(e rundir.Entry) (backend.Backend, error) {
	done, err := l.begin()
	if err != nil {
		return nil, err
	}
	defer done()
	lh, err := l.openStart(e.HostID)
	if err != nil {
		return nil, &launchError{msg: "craze: that session cannot be reached: " + err.Error(), err: err}
	}
	ref := hostRef{entry: e, held: true}
	if lh != nil {
		ref = lh.ref
	}
	ctx, cancel := context.WithTimeout(l.ctx, dialTimeout)
	defer cancel()
	s, err := spawnDial(ctx, ref.entry.Socket, l.sessionOptions(ref))
	if err == nil {
		if err = s.Attach(ctx); err != nil {
			_ = s.Close()
		}
	}
	if err != nil {
		l.openEnd(lh, nil)
		return nil, &launchError{msg: "craze: that session cannot be reached: " + sanitizeLine(err.Error()), err: err}
	}
	b := &launchedBackend{Session: s, ref: ref}
	if lh != nil {
		b.released = func() { l.openClosed(lh) }
	}
	l.openEnd(lh, b)
	return b, nil
}

// openSaved resumes the saved session row names, the list's enter on a saved
// row (plan 030 §3.12): the session is loaded by the host serving it already
// — a row the list showed saved that has started running since is the
// holder's: attached to directly, or through the held answer's rendezvous
// (spawn) — or else by a host spawned for it with --load: the row's craze id,
// or for a legacy row <provider>:<sessionId> (loadArg, R2-6; its host gives it
// its craze id under its own claim, so two resumes of one legacy row end as
// one host, the other attached to it held). Either way the backend is this
// launch's as any launch's is (take): kept at quit once its session came up
// in the TUI, its host stopped otherwise — a holder's never.
//
// The row decides where and as what the session runs (X13): its workspace,
// its provider. The command line's session flags were for its own session,
// not for every session the list resumes, so a resume passes only what any
// spawn of this launch takes — the permission mode, the plugin directories,
// the host-status switch, and --agent-bin, the launch's agent binary for
// whichever ACP provider runs (as the provider picker's choice takes it), not
// for one craze runs in process, which refuses it — and never --provider (a
// filter on a load, which would refuse a row of another provider),
// --workspace, --model, --ask or --plan; a held session attached to leaves
// no note of flags ignored.
//
// A row this craze cannot run is refused before anything is spawned
// (savedRunnable), a *tui.Refusal as the host's own refusal of it would be.
func (l *launcher) openSaved(row sessions.Row) (backend.Backend, error) {
	p, err := savedRunnable(row)
	if err != nil {
		return nil, err
	}
	f := l.flags
	f.cont, f.resume = false, false
	f.workspace, f.provider, f.model = "", "", ""
	f.ask, f.plan = false, false
	if p.InProcess() {
		f.agentBin = ""
	}
	return l.spawn(spawnOptions{env: l.env, flags: f, load: loadArg(row)}, row.CrazeID, false)
}

// savedRunnable is the provider of a saved row this craze can resume, or the
// refusal of one it cannot, in the host's own words for it (loadID,
// loadWorkspace) — the list shows them on its hint line: a provider this
// build does not know, or cannot load (the index is read with the resume
// picker's filter, so the list offers none, but a row is held to it here
// again), and a workspace that is no longer a directory, where the session's
// agent could not load it again. An empty provider is cursor's, as ever.
func savedRunnable(row sessions.Row) (agent.Provider, error) {
	p, err := agent.ProviderByName(row.Provider)
	if err != nil || !p.Resumable() {
		return agent.Provider{}, &tui.Refusal{Err: fmt.Errorf("craze: that session's provider %q is not one this craze can resume", row.Provider)}
	}
	if st, err := os.Stat(row.CWD); err != nil || !st.IsDir() {
		return agent.Provider{}, &tui.Refusal{Err: fmt.Errorf("craze: that session ran in %s, which is no longer a directory", row.CWD)}
	}
	return p, nil
}

// listHost is a host the session list's Spawn started, by host id in
// launcher.spawned for the launcher's life: the launch's to stop, decided per
// host and never per caller — the TUI may open it more than once, at the same
// time too, leave it running, and quit while it does (sol r20-c9r 1–2). Every
// field is read and written under the launcher's lock.
//
//   - An open that cannot reach it stops it at once, since nobody else would
//     (openEnd) — only when no other open of it is in flight, none that
//     attached is still open (a backend the TUI closed — a switch away from
//     it, a dial it no longer wanted — holds nothing: openClosed), no session
//     on it came up in the TUI, and it was not left running.
//   - finish stops it unless it was left running, or a session opened on it
//     came up in the TUI (acknowledged): the launch's own rule, by host.
//   - LeaveRunning keeps it, unless finish has decided or it was stopped.
type listHost struct {
	ref hostRef
	// opening counts the opens of it in flight, opened the ones that
	// attached whose backends are not closed yet (their backends are the
	// launch's launched; each tells openClosed when it is closed).
	opening, opened int
	// left: LeaveRunning kept it. stopped: an open that could not reach it
	// stopped it (openEnd), and nothing opens it again.
	left, stopped bool
}

// errSpawnStopped is an open or a LeaveRunning of a host Spawn started that
// an earlier open could not reach, and so stopped.
var errSpawnStopped = errors.New("its host did not answer, and was stopped")

// openStart counts an open of the host hostID names in flight, answering its
// listHost when Spawn started it (nil for any other host): from here to
// openEnd no other caller can stop it.
func (l *launcher) openStart(hostID string) (*listHost, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	lh := l.spawned[hostID]
	switch {
	case lh == nil:
		return nil, nil
	case lh.stopped:
		return nil, errSpawnStopped
	}
	lh.opening++
	return lh, nil
}

// openEnd is an open's outcome: b the backend it attached, nil when it
// could not. The backend is recorded for finish; a host Spawn started that
// this open could not reach is stopped — decided under the lock, done outside
// it — when nothing else holds it: no open of it in flight, none that
// attached still open, no session on it come up in the TUI, not left running.
func (l *launcher) openEnd(lh *listHost, b *launchedBackend) {
	l.mu.Lock()
	if b != nil {
		l.launched = append(l.launched, b)
	}
	stop := false
	if lh != nil {
		lh.opening--
		switch {
		case b != nil:
			lh.opened++
		case lh.opening == 0 && lh.opened == 0 && !lh.left && !l.cameUpLocked(lh.ref.entry.HostID):
			lh.stopped, stop = true, true
		}
	}
	l.mu.Unlock()
	if stop {
		lh.ref.abandon()
	}
}

// openClosed is a backend open answered for lh's host being closed (sol
// r21-c10r 1): it holds the host no more. Nothing is stopped here — a host
// the TUI let go of is finish's to decide, or a later open's that cannot
// reach it (openEnd).
func (l *launcher) openClosed(lh *listHost) {
	l.mu.Lock()
	defer l.mu.Unlock()
	lh.opened--
}

// cameUpLocked says a session came up in the TUI on the host hostID names —
// on any backend of it the launch answered (finish's rule): such a host is
// the user's, and no failed open stops it. l.mu is held.
func (l *launcher) cameUpLocked(hostID string) bool {
	for _, b := range l.launched {
		if b.ref.entry.HostID == hostID && b.acked.Load() {
			return true
		}
	}
	return false
}

// spawnFor spawns a host for spec's new session (plan 030 §3.13): spec's
// workspace, provider and model, and its permission mode where it has one —
// the session the list was opened from's (Claude Code's rule), else the
// launch's --force/--no-force. It answers the host's ref as the roster names
// it, which Open dials; a spawn that failed is a start failure or a refusal
// as a launch's is (launchFailure).
//
// Of the command line's own session flags a new session from the list takes
// only what every spawn of this launch takes, as a resume from the list does
// (openSaved, X111): the plugin directories, the host-status switch, and
// --agent-bin, the launch's agent binary for whichever ACP provider runs —
// the provider picker's choice takes it the same way, and CRAZE_AGENT_BIN,
// which every host inherits, is read for any ACP provider too — but not for a
// provider craze runs in process, which refuses it. Never --ask or --plan:
// they were the command line's own session's mode, not every session the
// list starts (plan 030 C15; per-dispatch modes are not built, as effort is
// not).
func (l *launcher) spawnFor(spec tui.SpawnSpec) (roster.Ref, error) {
	done, err := l.begin()
	if err != nil {
		return roster.Ref{}, err
	}
	defer done()
	f := l.flags
	f.cont, f.resume = false, false
	f.workspace, f.provider, f.model = spec.Workspace, spec.Provider.Name(), spec.Model
	f.ask, f.plan = false, false
	if spec.Provider.InProcess() {
		f.agentBin = ""
	}
	switch spec.PermissionMode {
	case backend.PermissionBypass:
		f.force, f.noForce = true, false
	case backend.PermissionPrompt:
		f.force, f.noForce = false, true
	}
	ref, err := spawnHost(l.ctx, spawnOptions{env: l.env, flags: f})
	if err != nil {
		return roster.Ref{}, launchFailure(err)
	}
	if !ref.held {
		// Recorded before the call is done, so finish — which waits for it
		// — finds it.
		l.mu.Lock()
		l.spawned[ref.entry.HostID] = &listHost{ref: ref}
		l.mu.Unlock()
	}
	return roster.Ref{Host: roster.HostOf(ref.entry)}, nil
}

// leaveRunning keeps the host hostID names, which Spawn started, running
// at quit: one decision under the launcher's lock, the lock finish takes to
// decide, so either finish finds it left or this finds finish has begun and
// says so (errLaunchOver) — never a nil answer for a host finish then stops
// (sol r20-c9r 2). A host an open could not reach was stopped then
// (errSpawnStopped). Nothing for a host Spawn did not start: it is not the
// launch's to stop.
func (l *launcher) leaveRunning(hostID string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	lh := l.spawned[hostID]
	switch {
	case lh == nil, lh.left:
		return nil
	case lh.stopped:
		return errSpawnStopped
	case l.closed:
		return errLaunchOver
	}
	lh.left = true
	return nil
}

// begin counts one call of the launcher's in flight, which finish waits for,
// and refuses one after finish.
func (l *launcher) begin() (done func(), err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil, errLaunchOver
	}
	l.inflight.Add(1)
	return l.inflight.Done, nil
}
