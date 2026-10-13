package modeltable

import (
	"cmp"
	"slices"
	"strings"
)

// Which models a picker offers, and in what order (plan 031 §3.6, owner
// decision Q4 "connected only"): the models of the providers that have a key,
// the ones picked most recently first. A model whose provider has none is not
// offered at all — nothing is shown about a provider that is not connected —
// with one exception, the model a session is running on (P7), which its own
// picker always lists: a list that left out the current model would have no
// row to pre-select.
//
// It is the rule every picker of native models goes through: a session's own
// /model (the advertised list, internal/agent's native adapter, computed when
// the session starts and again whenever it reloads its table — plan 034
// §3.4 — and its order and facts again, over the same models, after every
// switch or effort change it confirms: Relist, plan 047 D2) and plan 030's
// session list (§3.12, with no current model). The order is the whole of how
// a picker says what is recent: no label (owner, Q4).

// Choice is one model a picker offers: its alias, its display name (the
// table's name, or the alias when it has none), its provider, and its rank in
// the model memory — 1 for the newest remembered model the list offers, 2 for
// the next, and so on; 0 for a model the memory does not hold.
//
// Efforts, DefaultEffort and SwitchEffort are its effort facts (plan 047 D2):
// the levels it offers in the table's order (none for a model with no effort
// control), its default_effort, and the effort a switch to it would land on
// (Table.SwitchEffort) — so a picker can show a model's levels, and which
// one it would be at, before it is chosen.
type Choice struct {
	Alias         string
	Name          string
	Provider      string
	Recent        int
	Efforts       []string
	DefaultEffort string
	SwitchEffort  string
}

// Choices is the models a picker offers from t, in order (plan 031 §3.6): every
// model whose provider has a usable key — judged exactly as Resolve judges it,
// with getenv (nil is os.Getenv) — plus current, the model the session runs on,
// whether its provider has a key or not (P7); "" is no current model, and an
// alias t does not have adds nothing.
//
// The order is the model memory's first — recent, through Recent, so a renamed
// model keeps its place and a re-pointed alias does not — newest first, each
// with its rank; then every other model by display name, then alias — but two
// blocks, each placed by its provider's display name: the ChatGPT plan's
// models, which keep the order of the account's own list (priority, plan 033
// §7 A19), and the Anthropic provider's anthropic/<id> models, newest release
// first, then alias (plan 044 §3.5, P28: "Anthropic" sorts before every other
// provider's name today). The ranks count only the models listed,
// so they run 1, 2, 3… with no gap where an unfunded remembered model was
// passed over.
//
// Each choice's effort facts are the table's, its switch effort judged with
// recent and no memory view: what a switch made by a session that has
// confirmed nothing yet would land on (SessionChoices).
func (t *Table) Choices(recent []RecentEntry, getenv func(string) string, current string) []Choice {
	return t.SessionChoices(recent, nil, getenv, current)
}

// SessionChoices is Choices for a session whose memory view is memory — the
// alias → effort of every switch and effort change it has confirmed (plan
// 047 D1) — which each choice's switch effort is judged with
// (Table.SwitchEffort). memory is not written to.
func (t *Table) SessionChoices(recent []RecentEntry, memory map[string]string, getenv func(string) string, current string) []Choice {
	return t.choices(recent, memory, func(alias string) bool {
		if alias == current {
			return true
		}
		_, err := t.Resolve(alias, getenv)
		return err == nil
	})
}

// Relist is the choices of exactly aliases — the models a session's list
// offers now — in Choices' order, each with its rank in recent and its
// effort facts judged with recent and memory (plan 047 D2): what a native
// session publishes after a switch or an effort change it confirmed, with
// the list's members unchanged, since nothing is resolved — which models are
// funded is a reload's to judge, never a switch's. An alias t does not have
// is left out. aliases, recent and memory are not written to.
func (t *Table) Relist(aliases []string, recent []RecentEntry, memory map[string]string) []Choice {
	set := make(map[string]bool, len(aliases))
	for _, a := range aliases {
		set[a] = true
	}
	return t.choices(recent, memory, func(alias string) bool { return set[alias] })
}

// choices is the list of the models listed says are offered, in Choices'
// order, ranked by recent, their facts judged with recent and memory.
func (t *Table) choices(recent []RecentEntry, memory map[string]string, listed func(alias string) bool) []Choice {
	out := make([]Choice, 0, len(t.Models))
	seen := make(map[string]bool, len(t.Models))
	entries := t.Recent(recent)
	remembered := make(map[string]string, len(entries))
	for _, e := range entries {
		remembered[e.Alias] = e.Effort
	}
	for _, e := range entries {
		if !listed(e.Alias) {
			continue
		}
		seen[e.Alias] = true
		c := t.choice(e.Alias, remembered, memory)
		c.Recent = len(out) + 1
		out = append(out, c)
	}
	var rest []Choice
	for _, alias := range t.Aliases() {
		if !seen[alias] && listed(alias) {
			rest = append(rest, t.choice(alias, remembered, memory))
		}
	}
	slices.SortFunc(rest, func(a, b Choice) int {
		ka, kb := t.sortKey(a), t.sortKey(b)
		return cmp.Or(cmp.Compare(ka.key, kb.key), compareBool(ka.block, kb.block), cmp.Compare(ka.rank, kb.rank),
			newestFirst(ka.released, a.Alias, kb.released, b.Alias))
	})
	return append(out, rest...)
}

// choiceKey is how Choices orders a model among the ones the memory does not
// hold (sortKey).
type choiceKey struct {
	key      string // the display name, or a block's provider's
	block    bool   // one of a provider's block, after any model of the same name
	rank     int    // its place in the ChatGPT plan's list
	released string // its release, for the Anthropic block's newest first
}

// sortKey is how Choices orders c among the models the memory does not hold:
// by its display name; a model of the ChatGPT plan's list by its provider's
// display name instead, and then by its rank in the list, so the plan's
// models stay one block in the account's own order (discovered says whether
// c is one); and an anthropic/<id> model of the provider on the anthropic
// driver by its provider's display name too, then by its release, newest
// first (factsOf), so Anthropic's models are one block, the newest at its
// top. A model of the user's under another alias on that provider sorts by
// its name, as the TUI's ordering keeps the block by the alias's prefix
// (agent.OrderModels), so the two agree.
func (t *Table) sortKey(c Choice) choiceKey {
	provider := cmp.Or(t.Providers[c.Provider].Name, c.Provider)
	if r, ok := t.discovered[c.Alias]; ok {
		return choiceKey{key: provider, block: true, rank: r}
	}
	if strings.HasPrefix(c.Alias, AnthropicAliasPrefix) && t.Providers[c.Provider].Driver == DriverAnthropic {
		return choiceKey{key: provider, block: true, released: t.factsOf(t.Models[c.Alias]).Released}
	}
	return choiceKey{key: c.Name}
}

// compareBool orders false before true.
func compareBool(a, b bool) int {
	switch {
	case a == b:
		return 0
	case b:
		return -1
	}
	return 1
}

// choice is alias's Choice with no rank, its effort facts judged with the
// efforts the memory remembers by alias (rememberedEfforts) and the memory
// view: t has alias.
func (t *Table) choice(alias string, remembered, memory map[string]string) Choice {
	m := t.Models[alias]
	return Choice{
		Alias: alias, Name: cmp.Or(m.Name, alias), Provider: m.Provider,
		Efforts: slices.Clone(m.Efforts), DefaultEffort: m.DefaultEffort,
		SwitchEffort: t.switchEffort(remembered, memory, alias),
	}
}
