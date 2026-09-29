package tui

import (
	"errors"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/backend"
	"github.com/charliek/craze/internal/sessions"
)

// The launch flow (plan 030 §3.5): the ordinary craze runs every session in a
// detached host (SD-33) and is that host's client. The TUI starts before any
// host: the frame is up at once, in the session's starting state — starting…,
// or restoring… for a load — and the session is spawned from a tea.Cmd
// (Config.NewBackend, Config.LoadBackend), by Init when it is known and by a
// picker's choice when it is not. The backend the spawn answers is adopted as
// a Config.Backend is (setBackend), and then started and read exactly as Init
// starts and reads one: Start observes the host's own start, and a start that
// fails there is the session's start failure, as it is in process.
//
// A spawn that fails is the session's start failure too — the error row, the
// failed status, craze's exit status — unless it is the host's refusal of a
// picker's choice (*Refusal): that picker comes back with the refusal as its
// error row, where a refusal of the choice has always been drawn, and the
// user chooses again. Nothing is running meanwhile to close or to fence: a
// spawn is started only before any session, and a picker is up only then.

// Refusal is a spawn the host itself turned down, as opposed to one that
// failed to come up (plan 030 §3.5): a row its host cannot load, a session
// another host holds that cannot be attached to, a provider it refuses — the
// host's answer about what was chosen, which choosing again may change. Err is
// the refusal as a start failure says it; a picker's error row drops its
// leading "craze: ", as the picker's own refusals have none.
type Refusal struct {
	Err error
}

func (r *Refusal) Error() string { return r.Err.Error() }
func (r *Refusal) Unwrap() error { return r.Err }

// startAcker is a backend told that its session came up in this TUI: the
// launch flow's (internal/cli's), whose launcher keeps a host it spawned
// running after the TUI has quit only when the TUI saw its session up — and
// stops it otherwise (plan 030 X27, astra r7-c4 1). The host's Start
// answering is not that: a SIGTERM or a hang-up can quit the program between
// that answer and its startedMsg reaching Update, and a host nobody saw come
// up would be left running.
//
// AckStarted is called from Update alone — where startedMsg is applied, and
// only while the program is not quitting — so it is fenced against every
// quit: the explicit one sets quitting in an Update before any later message
// is applied, and bubbletea acts on a SIGTERM's or a hang-up's quit without
// applying anything after it. Once Run has returned, no acknowledgement can
// follow.
type startAcker interface {
	AckStarted()
}

// spawnedMsg is a spawn's answer: the backend to adopt, or why there is none.
// attempt is the stamp spawn gave it, and from the picker whose choice it was
// (dialogNone for Init's own).
type spawnedMsg struct {
	attempt int
	from    dialogKind
	b       backend.Backend
	err     error
}

// launching is c as New reads it: a launch Config (NewBackend or LoadBackend
// set) has nothing of the in-process path's — no session to wrap, no builder
// of one, no claim, no engine hook, no index, no row id — since the host
// builds and owns all of them. Any other Config is returned as it is. A
// Backend outranks the launch: a session already served elsewhere is
// adopted, and nothing is spawned.
func (c Config) launching() Config {
	if c.NewBackend == nil && c.LoadBackend == nil {
		return c
	}
	c.Session, c.NewSession, c.LoadSession = nil, nil, nil
	c.ClaimSession, c.OnEngine = nil, nil
	c.SessionIndex = nil
	c.CrazeSessionID = ""
	if c.Backend != nil {
		c.NewBackend, c.LoadBackend, c.Continue = nil, nil, nil
	}
	return c
}

// launch reports whether the model is the launch flow's: a session it spawns
// rather than builds.
func (m Model) launch() bool { return m.spawnNew != nil || m.spawnLoad != nil }

// spawn starts a spawn for from's choice — p's new session, or a load of row
// when row is set — and records it as the one the model waits for.
func (m *Model) spawn(from dialogKind, p agent.Provider, row *sessions.Row, explicit bool) tea.Cmd {
	m.spawnSeq++
	m.spawnWaiting, m.spawnFrom = m.spawnSeq, from
	return m.spawnCmd(m.spawnWaiting, from, p, row, explicit)
}

// initSpawn is Init's spawn, which New recorded (spawnWaiting): Continue's
// row, or a new session of the provider it resolved.
func (m Model) initSpawn() tea.Cmd {
	return m.spawnCmd(m.spawnWaiting, dialogNone, m.providerDefault, m.cont, false)
}

// spawnCmd is the tea.Cmd that runs one spawn: the closure the Config gave,
// off the Update, its answer carried back as a spawnedMsg.
func (m Model) spawnCmd(attempt int, from dialogKind, p agent.Provider, row *sessions.Row, explicit bool) tea.Cmd {
	newB, loadB := m.spawnNew, m.spawnLoad
	var load sessions.Row
	if row != nil {
		load = *row
	}
	return func() tea.Msg {
		msg := spawnedMsg{attempt: attempt, from: from}
		switch {
		case row != nil && loadB != nil:
			msg.b, msg.err = loadB(p, load)
		case row == nil && newB != nil:
			msg.b, msg.err = newB(p, explicit)
		default:
			// A Config that asked for a spawn it gave no closure for: a
			// session that cannot start, said the way startCmd says it.
			msg.err = errors.New("craze: no session")
		}
		if msg.err == nil && msg.b == nil {
			msg.err = errors.New("craze: no session")
		}
		return msg
	}
}

// spawned is a spawn's answer. Only the one the model is waiting for, while it
// is not quitting, is acted on: a backend answered for any other is closed —
// off the Update, since a close may block — and never adopted.
//
// A backend is adopted as a Config.Backend is (setBackend) and then started
// and read as Init starts and reads one. A failure is the start failure
// errMsg records, unless it is a *Refusal of a picker's choice, which puts
// that picker back up with the refusal as its error row.
func (m Model) spawned(msg spawnedMsg) (tea.Model, tea.Cmd) {
	if msg.attempt != m.spawnWaiting || m.quitting {
		if b := msg.b; b != nil {
			return m, func() tea.Msg {
				_ = b.Close()
				return nil
			}
		}
		return m, nil
	}
	m.spawnWaiting = 0
	if msg.err != nil {
		var refused *Refusal
		if errors.As(msg.err, &refused) && m.repick(msg.from, refused) {
			return m, nil
		}
		return m.spawnFailed(msg.err), nil
	}
	m.setBackend(msg.b)
	m.recompute()
	if m.model == "" && m.snap.CurrentModel != "" {
		m.model = m.snap.CurrentModel
	}
	if m.model == "" {
		m.model = "default"
	}
	// Init's batch, for the backend Init could not start: the read it arms is
	// the one the command gate's reader rule counts (readOn).
	m.reading = true
	return m, tea.Batch(m.startCmd(), waitEvent(m.eng))
}

// repick brings back the picker a refused choice came from, with the refusal
// as its error row, and reports whether there was one: Init's own spawn —
// --continue's row, or a provider known from the start — has no picker to go
// back to, and its refusal is a start failure.
func (m *Model) repick(from dialogKind, r *Refusal) bool {
	text := strings.TrimPrefix(r.Error(), "craze: ")
	switch from {
	case dialogProvider:
		m.pickingProvider = true
		m.dialog = dialogProvider
		m.providerErr = text
	case dialogResume:
		m.pickingResume = true
		m.dialog = dialogResume
		m.resumeErr = text
		// The choice set it for the load it was about to make; the picker
		// is before any session again.
		m.replaying = false
	default:
		return false
	}
	return true
}

// spawnFailed records a spawn that found no session as the session's start
// failure: errMsg's arm, with no backend to tell. The mirror stays empty —
// there is no session for it to describe — so the status row is the
// workspace, the branch and the model's default.
func (m Model) spawnFailed(err error) Model {
	m.status = statusError
	m.err = err.Error()
	m.startErr = err
	m.addError(m.err)
	return m
}
