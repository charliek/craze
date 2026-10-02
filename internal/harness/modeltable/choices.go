package modeltable

import (
	"cmp"
	"slices"
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
// /model (the advertised list, internal/agent's native adapter, computed once
// when the session starts — §3.4: a switch reorders the next session's
// picker, not this one's) and plan 030's session list (§3.12, with no current
// model). The order is the whole of how a picker says what is recent: no
// label (owner, Q4).

// Choice is one model a picker offers: its alias, its display name (the
// table's name, or the alias when it has none), its provider, and its rank in
// the model memory — 1 for the newest remembered model the list offers, 2 for
// the next, and so on; 0 for a model the memory does not hold.
type Choice struct {
	Alias    string
	Name     string
	Provider string
	Recent   int
}

// Choices is the models a picker offers from t, in order (plan 031 §3.6): every
// model whose provider has a usable key — judged exactly as Resolve judges it,
// with getenv (nil is os.Getenv) — plus current, the model the session runs on,
// whether its provider has a key or not (P7); "" is no current model, and an
// alias t does not have adds nothing.
//
// The order is the model memory's first — recent, through Recent, so a renamed
// model keeps its place and a re-pointed alias does not — newest first, each
// with its rank; then every other model by display name, then alias — but the
// ChatGPT plan's models, which keep the order of the account's own list
// (priority, plan 033 §7 A19) as one block, placed by its provider's display
// name. The ranks count only the models listed, so they run 1, 2, 3… with no
// gap where an unfunded remembered model was passed over.
func (t *Table) Choices(recent []RecentEntry, getenv func(string) string, current string) []Choice {
	listed := func(alias string) bool {
		if alias == current {
			return true
		}
		_, err := t.Resolve(alias, getenv)
		return err == nil
	}
	out := make([]Choice, 0, len(t.Models))
	seen := make(map[string]bool, len(t.Models))
	for _, e := range t.Recent(recent) {
		if !listed(e.Alias) {
			continue
		}
		seen[e.Alias] = true
		c := t.choice(e.Alias)
		c.Recent = len(out) + 1
		out = append(out, c)
	}
	var rest []Choice
	for _, alias := range t.Aliases() {
		if !seen[alias] && listed(alias) {
			rest = append(rest, t.choice(alias))
		}
	}
	slices.SortFunc(rest, func(a, b Choice) int {
		ka, ra, da := t.sortKey(a)
		kb, rb, db := t.sortKey(b)
		return cmp.Or(cmp.Compare(ka, kb), compareBool(da, db), cmp.Compare(ra, rb), cmp.Compare(a.Alias, b.Alias))
	})
	return append(out, rest...)
}

// sortKey is how Choices orders c among the models the memory does not hold:
// by its display name; a model of the ChatGPT plan's list by its provider's
// display name instead, and then by its rank in the list, so the plan's
// models stay one block in the account's own order (discovered says whether
// c is one).
func (t *Table) sortKey(c Choice) (key string, rank int, discovered bool) {
	if r, ok := t.discovered[c.Alias]; ok {
		return cmp.Or(t.Providers[c.Provider].Name, c.Provider), r, true
	}
	return c.Name, 0, false
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

// choice is alias's Choice with no rank: t has alias.
func (t *Table) choice(alias string) Choice {
	m := t.Models[alias]
	return Choice{Alias: alias, Name: cmp.Or(m.Name, alias), Provider: m.Provider}
}
