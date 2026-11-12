package tui

import (
	"context"
	"fmt"
	"slices"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/backend"
	"github.com/charliek/craze/internal/harness/modeltable"
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
//
// cs is what the source order makes of the provider's models (plan 050 C5,
// connectSourcesOf): models of a maker another source supplies are not said
// to be in /model, and a maker whose models now come from this provider says
// which other source's no longer are.
func connectedNote(name string, cs connectSources, r *agent.ModelsRefresh) string {
	if sessionHomeDiffers(r) {
		return "Connected " + name + " in this craze directory, but this session's host reads another one, so it does not see its models."
	}
	return "Connected " + name + ". " + cs.sentence(r, "Its models", "its models", "its") + cs.takesText()
}

// signedInSessionNote is what a sign-in means for this conversation, as
// connectedNote is for a key's save: the ChatGPT plan's models for the plan,
// with the source order's say over them (cs) as there.
func signedInSessionNote(cs connectSources, r *agent.ModelsRefresh) string {
	if sessionHomeDiffers(r) {
		return "The sign-in is saved in this craze directory, but this session's host reads another one, so it does not see the plan's models."
	}
	return cs.sentence(r, "The plan's models", "the ChatGPT plan's models", "the plan's") + cs.takesText()
}

// connectSources is what the source order makes of one provider's models now
// (plan 050 C5): how many of its funded, active models the list offers;
// each maker whose models of it are hidden, with the provider they come from
// instead; and each maker whose models now come from it, with the providers
// whose models of that maker are no longer listed. Every name is sanitized.
// Its zero value is nothing hidden either way.
type connectSources struct {
	listed int
	hidden []hiddenMaker
	takes  []hiddenMaker
}

// hiddenMaker is one maker in connectSources: its name, and the providers on
// the other side (the source its models come from, or the ones it hides).
type hiddenMaker struct {
	maker     string
	providers []string
}

// connectSourcesOf is what the source order makes of provider id's models in
// table, its funding judged with getenv (Table.Offers, HiddenSources).
func connectSourcesOf(table *modeltable.Table, id string, getenv func(string) string) connectSources {
	var cs connectSources
	for alias, ok := range table.Offers(getenv).Offered {
		if ok && table.Models[alias].Provider == id {
			cs.listed++
		}
	}
	name := func(p string) string { return sanitizeLine(table.ProviderName(p)) }
	for _, ms := range table.HiddenSources(getenv) {
		maker := sanitizeLine(modeltable.MakerName(ms.Maker))
		if ms.Winner == id {
			hm := hiddenMaker{maker: maker}
			for _, h := range ms.Hidden {
				hm.providers = append(hm.providers, name(h.Provider))
			}
			cs.takes = append(cs.takes, hm)
			continue
		}
		if slices.ContainsFunc(ms.Hidden, func(h modeltable.HiddenSource) bool { return h.Provider == id }) {
			cs.hidden = append(cs.hidden, hiddenMaker{maker: maker, providers: []string{name(ms.Winner)}})
		}
	}
	return cs
}

// sourceOrderTail ends a sentence that says some models are not listed.
const sourceOrderTail = "they still run with --model (source_order in models.toml changes the order)."

// sentence is what the provider's models are now in this conversation:
// Subject ("Its models") and subject ("its models") are the provider's
// models, of the possessive ("its"). With none of them listed it says so and
// where they come from; with some hidden, where those come from; and
// otherwise the refresh's word, r, as it always was.
func (cs connectSources) sentence(r *agent.ModelsRefresh, Subject, subject, of string) string {
	if len(cs.hidden) > 0 && cs.listed == 0 {
		return "None of " + subject + " are listed: " + cs.comeFrom("") + "; " + sourceOrderTail
	}
	except := ""
	if len(cs.hidden) > 0 {
		except = ", except " + cs.comeFrom(of)
	}
	switch {
	case r != nil && r.Status == agent.ModelsApplied:
		if except != "" {
			return Subject + " are in /model now" + except + "; " + sourceOrderTail
		}
		return Subject + " are in /model now."
	case r != nil && r.Status == agent.ModelsPending:
		if except != "" {
			return Subject + " will be in /model after this turn" + except + "; " + sourceOrderTail
		}
		return Subject + " will be in /model after this turn."
	}
	if except != "" {
		return "New sessions offer " + subject + except + "; " + sourceOrderTail + " To use the listed ones in this conversation, /exit and run craze -c."
	}
	return "New sessions offer " + subject + "; to use them in this conversation, /exit and run craze -c."
}

// comeFrom is the hidden makers and where their models come from: with of
// ("its"), "its OpenAI models, which come from ChatGPT plan"; without,
// "OpenAI models come from ChatGPT plan". Several are joined with "and".
func (cs connectSources) comeFrom(of string) string {
	parts := make([]string, 0, len(cs.hidden))
	for _, h := range cs.hidden {
		if of != "" {
			parts = append(parts, fmt.Sprintf("%s %s models, which come from %s", of, h.maker, strings.Join(h.providers, " and ")))
		} else {
			parts = append(parts, fmt.Sprintf("%s models come from %s", h.maker, strings.Join(h.providers, " and ")))
		}
	}
	return strings.Join(parts, ", and ")
}

// takesText is one sentence for each maker whose models now come from the
// provider, naming the providers whose models of it are no longer listed,
// each after a space, and — unless sentence said it already — that those
// still run by name; "" for none.
func (cs connectSources) takesText() string {
	var out string
	for _, t := range cs.takes {
		out += fmt.Sprintf(" %s models come from it now; those of %s are no longer listed.", t.maker, strings.Join(t.providers, " and "))
	}
	if out != "" && len(cs.hidden) == 0 {
		out += " They still run with --model (source_order in models.toml changes the order)."
	}
	return out
}
