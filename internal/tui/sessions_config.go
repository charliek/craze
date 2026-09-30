package tui

import (
	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/backend"
	"github.com/charliek/craze/internal/roster"
	"github.com/charliek/craze/internal/sessions"
)

// Sessions is the session list's source (plan 030 §3.9): Config.Sessions.
// Production's (internal/cli, the launch path only) is internal/roster for the
// list, a socket dial for Open and a spawned craze serve for Spawn; goldens
// and unit tests hand in fakes — an in-process Open answers an engine
// backend, so an opened session's goldens run under both transports.
//
// nil — the in-process opt-out (detach = false, CRAZE_DETACH=0, the control
// socket off), craze attach, every test Config and the frame runner — means
// no session list: no ← binding, no /sessions builtin, no new help line,
// every existing frame unchanged (§3.17).
//
// Every method but Roster and LeaveRunning may take seconds and is called
// from a tea.Cmd, never from Update.
type Sessions interface {
	// Roster starts the list's poller as the list opens; the list closes it
	// (SessionRoster.Close) as it closes.
	Roster() SessionRoster
	// Open answers a backend for ref's session, which the model adopts and
	// starts as it does a launch's: a running one's host dialled and attached
	// — a host that is stopping is Open's error, never a failed start — and a
	// saved one's loaded by the host serving it already, else by one spawned
	// for it. It is the caller's to Close.
	Open(ref roster.Ref) (backend.Backend, error)
	// Spawn starts a new session as spec says and answers its ref, which Open
	// then dials (PR 3's dispatch). The host is this craze's to stop: when
	// craze quits it goes on only if a session opened on it (Open) came up in
	// the TUI, as a launch's does, or it was left running (LeaveRunning); an
	// Open that cannot reach it stops it at once unless another Open holds it
	// (a backend the TUI has closed holds nothing) or a session on it came up
	// in the TUI.
	Spawn(spec SpawnSpec) (roster.Ref, error)
	// LeaveRunning keeps ref's host, which Spawn started, running when craze
	// quits (PR 3's background dispatch, its prompt accepted — or sent with
	// no answer, when it may have been), opened or not.
	// Its error says it cannot: craze is already exiting and has decided, or
	// the host was stopped because an Open could not reach it. A ref Spawn
	// did not answer is not craze's to stop, and is left as it is (nil). It
	// returns at once.
	LeaveRunning(ref roster.Ref) error
	// Stop stops ref's running session on its host (session.stop): the
	// list's close of a session it is not showing.
	Stop(ref roster.Ref) error
	// Cancel is the list's ctrl+x on a working or asking row (plan 030
	// §3.10): ref's queue cleared and then its running turn cancelled
	// (session.queue.clear, then session.cancel), so the cancelled turn
	// settles into an empty queue and nothing starts behind it — over a
	// connection of its own that never attaches, as Stop's does. A cancel
	// that found nothing left to cancel — the turn ended on its own first —
	// is not an error: the queue is cleared and nothing runs. A saved
	// session is an error.
	Cancel(ref roster.Ref) error
}

// SessionStarter is what a Sessions has when new sessions can be started
// from the list (plan 030 §3.13, PR 3): the list's input, under the rows, and
// everything it brings — the `@` directory picker, the rule naming where a
// new session would run, the empty list's wording that points at the input —
// exists only when Config.Sessions has it (openSessions asks, by a type
// assertion). Production's has it (internal/cli's sessionList). A Sessions
// without it draws the list exactly as PR 2 drew it, which is how the list's
// merged frames — made against a fake without it — stay what they were
// (§3.17).
//
// Its methods may read the disk and are called from a tea.Cmd, never from
// Update.
type SessionStarter interface {
	Sessions
	// RecentDirs is up to n directories sessions have run in, newest first,
	// that are still directories (sessions.Store.RecentDirs): the `@`
	// picker's recent places, after the session the list came from and the
	// directories with a running session.
	RecentDirs(n int) ([]sessions.RecentDir, error)
}

// SessionRoster is a running session-list poller (*roster.Roster).
type SessionRoster interface {
	// Updates is the latest-snapshot slot: capacity one, replaced, never
	// waited on by the poller, and closed once the poller has stopped.
	Updates() <-chan roster.Snapshot
	// Close stops the poller and returns once it has; idempotent.
	Close()
}

// SpawnSpec is a new session a list starts (plan 030 §3.13): the directory it
// runs in, its provider and model ("" the provider's default), and the
// permission mode of the session the list was opened from
// (backend.PermissionUnsaid: the launch's own flags).
type SpawnSpec struct {
	Workspace      string
	Provider       agent.Provider
	Model          string
	PermissionMode backend.PermissionMode
}
