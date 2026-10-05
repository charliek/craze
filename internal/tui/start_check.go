package tui

import (
	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/craze/internal/agent"
)

// A start's own feedback (LM-2(a), plan 037 §3.5). A provider chosen without
// the picker — an explicit --provider, --continue's row, the resume picker's
// row — starts whatever availability says (plan 036 decision 3), and one that
// cannot reach what it needs may sit at starting… with nothing to say why:
// cursor outside the macOS login session, blocked on the login keychain. So
// when this process spawns that session's agent or its host, the start's
// provider is checked off the Update (Config.StartCheck), and a reason the
// check gives is one local note, the start going on. An attach to a host that
// was running already is never checked: that host may have been born in the
// login session, and works.
//
// The answer is taken for one start only — the session generation it runs
// under and its provider (startIdentity) — and only while that start has
// neither come up nor failed: a late answer, a stale one (the session or the
// provider changed since), a duplicate and one after the start's failure row
// each add nothing. Over a socket the host's Ready — a start that came up, or
// one that failed — retires it too (retireStartCheck), whichever of the
// Ready and the start's own answer lands first.
//
// A note taken is the start's (startWarn): over a socket the check answers
// beside the stream's reader, so it can land before the attach's first
// restore, whose rebuild takes every local row with it; that restore draws it
// again (redrawStartWarning), as it draws a start failure's row again.

// startIdentity is the start a check's answer is taken for: the session
// generation it runs under and its provider's id. The zero value is none.
type startIdentity struct {
	gen      uint64
	provider string
}

// startCheckMsg is Config.StartCheck's answer, stamped with the start it was
// asked for: the session generation (issued) and the provider. label is the
// provider as a note names it.
type startCheckMsg struct {
	issued
	provider    string
	label       string
	reason, fix string
}

// startWarnNote is the note a start's check drew (startChecked), and the
// session generation of the start it was for: the first restore of that
// start's session draws it again (redrawStartWarning). The zero value is
// none.
type startWarnNote struct {
	gen  uint64
	note string
}

// hostSpawner is a launch flow backend that says whether this process spawned
// the host it is dialled to — so the session's agent starts in this process's
// login session — rather than attaching to a host that was running already
// (internal/cli's launchedBackend). A launch flow backend that does not say is
// taken as an attach, which is never checked.
type hostSpawner interface {
	SpawnedHost() bool
}

// recordStartCheck makes the start the model now holds — p's, under the
// current session generation — the one a check's answer is taken for. Nothing
// with no Config.StartCheck, or no backend to start.
func (m *Model) recordStartCheck(p agent.Provider) {
	if m.startCheck == nil || m.eng == nil {
		return
	}
	m.startAsk = startIdentity{gen: m.sessGen, provider: p.Name()}
}

// askStartCheck records the start (recordStartCheck) and answers its check's
// call: a picker's choice, made in an Update that can record it.
func (m *Model) askStartCheck(p agent.Provider) tea.Cmd {
	m.recordStartCheck(p)
	return m.startCheckCmd(p)
}

// startCheckCmd is the check of p for the start recorded — Init's, since Init
// cannot record one (New does) — run off the Update, since it reads the disk
// and asks the login session. nil when the start recorded is none, or
// another provider's.
func (m Model) startCheckCmd(p agent.Provider) tea.Cmd {
	check, ask := m.startCheck, m.startAsk
	if check == nil || ask == (startIdentity{}) || ask.provider != p.Name() {
		return nil
	}
	iss, label := issued{sessGen: ask.gen}, sanitizeLine(p.DisplayName())
	return func() tea.Msg {
		reason, fix := check(p)
		return startCheckMsg{issued: iss, provider: p.Name(), label: label, reason: reason, fix: fix}
	}
}

// startChecked takes a check's answer: one for the start the model is waiting
// on — its session generation and provider — while that start has neither
// come up nor failed, nor the session ended, is taken once, and a reason in
// it is one note. Any other answer changes nothing.
func (m Model) startChecked(msg startCheckMsg) Model {
	ask := startIdentity{gen: msg.sessGen, provider: msg.provider}
	if m.startAsk == (startIdentity{}) || ask != m.startAsk || ask.gen != m.sessGen {
		return m
	}
	if m.started || m.startErr != nil || m.ended || m.quitting {
		return m
	}
	m.startAsk = startIdentity{}
	if reason := sanitizeLine(msg.reason); reason != "" {
		note := startCheckNote(msg.label, reason, sanitizeLine(msg.fix))
		m.startWarn = startWarnNote{gen: m.sessGen, note: note}
		m.addNote(note)
	}
	return m
}

// retireStartCheck is the start's check retired: no answer is taken from here
// on. The backend's Ready says the host's start is over — it came up, or it
// failed — which over a socket can land before the start's own answer
// (startedMsg, errMsg), and an answer after it would warn about a start that
// has already answered.
func (m *Model) retireStartCheck() { m.startAsk = startIdentity{} }

// redrawStartWarning draws again, once, the note the start's check drew, when
// the first restore of that start's session (its generation) has taken it away
// with every local row (applyRestore's rebuild); a note drawn after that
// restore is on screen already, and no later restore draws it.
func (m *Model) redrawStartWarning() {
	w := m.startWarn
	m.startWarn = startWarnNote{}
	if w.note != "" && w.gen == m.sessGen {
		m.addNote(w.note)
	}
}

// startCheckNote is the note: `<provider> may not start here: <reason>;
// <fix>` — the words `craze prompt` writes to stderr for the same answer.
func startCheckNote(label, reason, fix string) string {
	note := label + " may not start here: " + reason
	if fix != "" {
		note += "; " + fix
	}
	return note
}
