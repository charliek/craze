package tui

import (
	"context"

	"github.com/charmbracelet/x/ansi"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/host"
)

// publishHost derives the host status from the model and hands it to the hub
// when it differs from the last one handed over. It is the Update wrapper's,
// beside the tab title and for the same reason: every transition passes
// through there, and comparing here first keeps a tick — or any Update that
// changes nothing a host sees — off the hub's lock. The wrapper calls it only
// when there is a host.
func (m *Model) publishHost() {
	s, ok := host.Derive(m.hostInput())
	if !ok || s == m.lastHost {
		return
	}
	m.lastHost = s
	m.host.Publish(s)
}

// closeHost releases the host within the hub's own budget (plan 015 §3.2),
// passed as a deadline so a Host that honours ctx stops there too. It is the
// one way the TUI closes a host — requestQuit and finishRun both call it, in
// both cases before the session is closed — and a nil host is nothing to close.
func closeHost(h Host) {
	if h == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), host.DefaultCloseTimeout)
	defer cancel()
	h.Close(ctx)
}

// hostInput reduces the model to what the host status depends on. The
// priority rule lives in host.Derive; this only reads the model.
func (m Model) hostInput() host.Input {
	in := host.Input{
		Ready:       m.sessionReady(),
		StartFailed: m.startErr != nil,
		Working:     m.status == statusWorking,
		Errored:     m.status == statusError,
		ForeignTurn: m.snap.ForeignTurn,
		Cancelled:   m.cancelled,
		NoTurnYet:   !m.prompted,
		SessionID:   m.snap.SessionID,
		Provider:    hostMeta(m.sessProvider),
		Model:       hostMeta(m.snap.CurrentModel),
	}
	if in.Errored {
		// The first line before sanitising: sanitizeLine folds a newline into
		// a space, which would carry the error's detail into the headline.
		in.Err = hostMessage(host.FirstLine(m.err))
	}
	if c, ok := m.headCard(); ok {
		in.Card, in.CardLabel = hostCard(c)
		in.CardLabel = hostMessage(in.CardLabel)
	}
	return in
}

// hostCard is the head card as a host sees it: its kind, and the header the
// card itself draws — `permission <tool>` (permissionView), `question`, and
// `plan <name>` (planCardView). The question card's own `question 1/2` counter
// is left out: a host shows one reason, not the card's progress through it.
//
// The label comes from agent.AskLabel, which is also what the engine merges
// into State.HeadAsk (plan 021 §3.2): one derivation, so a host driven by a
// session with no TUI at all publishes the same reason this one does. The
// caller sanitises it (hostMessage), as it sanitises every other message.
func hostCard(c card) (host.Card, string) {
	switch c.kind {
	case cardPermission:
		return host.PermissionCard, agent.AskLabel(agent.AskPermission, agent.AskBody{Permission: c.perm})
	case cardQuestion:
		return host.QuestionCard, agent.AskLabel(agent.AskQuestion, agent.AskBody{Question: c.ask})
	case cardPlan:
		return host.PlanCard, agent.AskLabel(agent.AskPlan, agent.AskBody{Plan: c.plan})
	}
	return host.NoCard, ""
}

// hostMessage is a one-line host message: controls and escapes gone,
// whitespace collapsed, and at most host.MessageCap cells (plan 015 §3.1).
// Derive applies the same bound again; the TUI applies it first so nothing
// unsanitised ever enters host.Input.
func hostMessage(s string) string {
	return ansi.Truncate(sanitizeLine(s), host.MessageCap, "…")
}

// hostMeta is a provider or model id as a host token: sanitised the same way,
// which also trims it, and at most host.MetaCap cells, herdr's cap on a
// metadata token value.
func hostMeta(s string) string {
	return ansi.Truncate(sanitizeLine(s), host.MetaCap, "…")
}
