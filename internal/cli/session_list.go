package cli

import (
	"context"
	"errors"

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
// host spawned for it (loadBackend, as the resume picker's choice is); a new
// one spawned; and a stop sent over a connection of its own. Only a launch
// has one: under the opt-out the TUI hosts its session in process, and
// closing a backend there would close its engine, so there is no list.
//
// It lives as long as its launcher: nothing starts after finish, whatever is
// in flight then is cancelled, and a backend it answered whose session never
// came up in the TUI is closed there (finish) — a running one's host, never
// this launch's, is left alone.
type sessionList struct{ l *launcher }

var _ tui.Sessions = sessionList{}

// Roster opens the list's poller: the registry of this user (the launcher's
// env, the process's), the index of this CRAZE_HOME, and only the providers
// this build knows offered as saved.
func (s sessionList) Roster() tui.SessionRoster {
	return roster.Open(s.l.env, &sessions.Store{KnownProvider: knownProvider})
}

// Open is ref's session as a backend the TUI adopts: a running one's host
// dialled and attached (open), a saved one's row loaded (loadBackend: the
// host serving it already if there is one, else a spawn with --load in the
// row's own workspace).
func (s sessionList) Open(ref roster.Ref) (backend.Backend, error) {
	if ref.Saved != nil {
		return s.l.loadBackend(agent.Provider{}, *ref.Saved)
	}
	return s.l.open(ref.Host.Entry())
}

// Spawn starts a host for spec's new session and answers its ref, for Open
// (spawnFor).
func (s sessionList) Spawn(spec tui.SpawnSpec) (roster.Ref, error) { return s.l.spawnFor(spec) }

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

// open dials the host e names as the TUI's client and attaches — as the
// direct reattach does, so a host that is stopping (it answers hello and
// refuses every attach closing) is an error here and never the TUI's failed
// start (X55) — within dialTimeout, and records it for finish: never this
// launch's host to stop, closed if its session never comes up in the TUI.
func (l *launcher) open(e rundir.Entry) (backend.Backend, error) {
	done, err := l.begin()
	if err != nil {
		return nil, err
	}
	defer done()
	ref := hostRef{entry: e, held: true}
	ctx, cancel := context.WithTimeout(l.ctx, dialTimeout)
	defer cancel()
	s, err := spawnDial(ctx, e.Socket, l.sessionOptions(ref))
	if err == nil {
		if err = s.Attach(ctx); err != nil {
			_ = s.Close()
		}
	}
	if err != nil {
		return nil, &launchError{msg: "craze: that session cannot be reached: " + sanitizeLine(err.Error()), err: err}
	}
	b := &launchedBackend{Session: s, ref: ref}
	l.mu.Lock()
	l.launched = append(l.launched, b)
	l.mu.Unlock()
	return b, nil
}

// spawnFor spawns a host for spec's new session (plan 030 §3.13): the
// launch's session flags, with spec's workspace, provider and model, and its
// permission mode where it has one. It answers the host's ref as the roster
// names it, which Open dials; a spawn that failed is a start failure or a
// refusal as a launch's is (launchFailure).
func (l *launcher) spawnFor(spec tui.SpawnSpec) (roster.Ref, error) {
	done, err := l.begin()
	if err != nil {
		return roster.Ref{}, err
	}
	defer done()
	f := l.flags
	f.cont, f.resume = false, false
	f.workspace, f.provider, f.model = spec.Workspace, spec.Provider.Name(), spec.Model
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
	return roster.Ref{Host: roster.HostOf(ref.entry)}, nil
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
