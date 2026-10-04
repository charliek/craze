package tui

import (
	"context"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/backend"
)

// Models picked up live, the TUI's half (plan 034 §3.4 "TUI (C6)", Q13/Q14):
// a native session takes up the models funded while it runs — a key saved, the
// ChatGPT plan signed in to or out of — when the TUI asks it to
// (Backend.RefreshModels, the method session.models.refresh over the socket).
// The TUI asks in three places: after /connect saved a key, after a sign-in's
// model list was written (signInFinishedMsg, not signInDoneMsg), and when
// /model opens. The model dialog and the picker read the folded catalog
// (recompute), so a list the session publishes re-renders them with no help.
//
// Every ask is conditional on the session's own word: SessionInfo.ModelsRefresh,
// true for a native session however it is hosted and false for an ACP session,
// a host from before the method and a bare Model a test built — all of which
// get today's note and no call (X57, A25). The call is a blocking read, so it
// runs as a gated call, off the Update, and what it answers is only ever a
// note: the list itself arrives on the stream.

// canRefreshModels reports whether the session takes up models while it runs.
func (m Model) canRefreshModels() bool {
	return m.eng != nil && m.info().ModelsRefresh
}

// refreshModelsCall is the gated call: the session's RefreshModels with this
// TUI's native directory, whose answer is an agent.ModelsRefresh.
func refreshModelsCall(dir string) gateCall {
	return func(ctx context.Context, b backend.Backend) (any, error) {
		return b.RefreshModels(ctx, dir)
	}
}

// thenRefreshModels runs cont with what a refresh came to: nil — today's
// note — when the session cannot refresh, the call failed or no answer came
// in time, and the answer otherwise. With a session that can, it is a gated
// call, so cont runs in its continuation; a continuation may open the next
// link (run), and this is the last of a chain, never one in the middle of an
// Update that has a gate open already.
func (m Model) thenRefreshModels(cont func(Model, *agent.ModelsRefresh) Model) (Model, tea.Cmd) {
	if !m.canRefreshModels() {
		return cont(m, nil), nil
	}
	return m.run(gateDeadline, refreshModelsCall(m.nativeDir), func(m Model, r gateReply) (Model, tea.Cmd) {
		if res, ok := r.result.(agent.ModelsRefresh); ok && r.err == nil {
			return cont(m, &res), nil
		}
		return cont(m, nil), nil
	})
}

// sessionHomeDiffers reports whether the host said it reads another craze
// directory than this TUI's (A26): this TUI wrote the key or the sign-in into
// its own CRAZE_HOME, which that session's host never reads. Its note stands
// alone, whatever else the answer said (plan 034 C6r, from V2): the old
// note's "/exit and run craze -c" is wrong advice there — for a TUI attached
// to that host, /exit ends the shared session, and craze -c from this home
// finds nothing of it.
func sessionHomeDiffers(r *agent.ModelsRefresh) bool {
	return r != nil && r.SameDir != nil && !*r.SameDir
}

// connectedNote is the notice after a save (plan 031 §3.9): no key material,
// and what the save means for this conversation. r is what the refresh that
// followed came to, nil when none was asked or none answered:
//
//   - a host reading another craze directory, whatever else it said: the key
//     is in this TUI's directory and not the session's, and nothing more —
//     no /exit and craze -c, which would not reach it either
//     (sessionHomeDiffers);
//   - applied: the session offers the provider's models now;
//   - pending: a turn is running, and the session takes them up as it ends;
//   - anything else (current, failed, unsupported, no refresh): the old note,
//     plan 031's P8 — new sessions offer them, this one after /exit and
//     craze -c.
func connectedNote(name string, r *agent.ModelsRefresh) string {
	head := "Connected " + name + ". "
	old := head + "New sessions offer its models; to use them in this conversation, /exit and run craze -c."
	switch {
	case sessionHomeDiffers(r):
		return "Connected " + name + " in this craze directory, but this session's host reads another one, so it does not see its models."
	case r != nil && r.Status == agent.ModelsApplied:
		return head + "Its models are in /model now."
	case r != nil && r.Status == agent.ModelsPending:
		return head + "Its models will be in /model after this turn."
	}
	return old
}

// signedInSessionNote is what a sign-in means for this conversation, as
// connectedNote is for a key's save: the ChatGPT plan's models for the plan.
func signedInSessionNote(r *agent.ModelsRefresh) string {
	old := "New sessions offer the ChatGPT plan's models; to use them in this conversation, /exit and run craze -c."
	switch {
	case sessionHomeDiffers(r):
		return "The sign-in is saved in this craze directory, but this session's host reads another one, so it does not see the plan's models."
	case r != nil && r.Status == agent.ModelsApplied:
		return "The plan's models are in /model now."
	case r != nil && r.Status == agent.ModelsPending:
		return "The plan's models will be in /model after this turn."
	}
	return old
}
