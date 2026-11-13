package modeltable

import (
	"cmp"
	"fmt"
	"slices"
)

// What `craze models` lists (plan 050 C2): the catalog as a table of its own,
// where each model's alias comes from, how each provider's models reach the
// table, the retired rows, and the one sentence craze says of a deprecated
// model.

// How a provider's models reach the table (Table.AccountModels).
const (
	// AccountModelsNone: the catalog's and the user's files' models alone.
	AccountModelsNone = "none"
	// AccountModelsAlso: the catalog's seeds, and the account's own list
	// when there is one (the Anthropic provider).
	AccountModelsAlso = "also"
	// AccountModelsOnly: the account's own list alone (the ChatGPT plan).
	AccountModelsOnly = "only"
)

// CatalogTable is cat as a table with nothing else read (plan 050 C2's
// `craze models --shipped`): no directory, no environment, no account list —
// the models a load of an empty directory with no key would have, and the
// [anthropic_defaults] it would apply, every entry's origin shipped. cat
// itself is never changed. A table built so funds nothing a key funds unless
// its caller's getenv does, and nothing the ChatGPT plan funds.
func CatalogTable(cat *Catalog) (*Table, error) {
	if cat.empty() {
		return nil, fmt.Errorf("modeltable: %s ships nothing", CatalogFile)
	}
	c := cat.Clone()
	var pd providersOverlay
	// No directory: the defaults apply and no list is read
	// (applyAnthropicList, withDiscovered).
	withAnthropicList(c, "", &pd)
	learned := learnedOf(c)
	t := merge(c, "", "", &pd, &modelsOverlay{})
	t.learned = learned
	t.noteAccountLists(c, nil)
	if err := validate(t, CatalogFile, CatalogFile, false); err != nil {
		return nil, err
	}
	return t, nil
}

// noteAccountLists records, from c — the load's own copy of the catalog, the
// account lists applied to it — and disc, what the ChatGPT plan's list added,
// which providers an account's list brings models for and whether the load
// used one (accountLists), and the Anthropic list's own models.
func (t *Table) noteAccountLists(c *Catalog, disc *discovered) {
	t.accountLists = map[string]bool{}
	if p, ok := c.Providers[ChatGPTProvider]; ok && p.Driver == DriverChatGPT {
		// Read: bound to the account, at least one entry accepted, and none
		// left out for its client version. A list that accepted nothing —
		// empty, or every entry rejected — tells a missing model from nothing.
		t.accountLists[ChatGPTProvider] = disc != nil && len(disc.rank) > 0 && disc.newer == 0
	}
	if c.shipsAnthropic() {
		t.accountLists[AnthropicProvider] = c.anthropicListed != nil
	}
	for alias := range c.anthropicListed {
		if hasModel(t.Models, alias) {
			if t.anthropicListed == nil {
				t.anthropicListed = map[string]bool{}
			}
			t.anthropicListed[alias] = true
		}
	}
}

// AccountModels is how provider's models reach t (plan 050 C2):
// AccountModelsOnly for the ChatGPT plan, whose models are the account's own
// list's; AccountModelsAlso for craze's Anthropic provider, whose seeds the
// account's list replaces when there is one; AccountModelsNone for every
// other provider, and for one t does not have.
func (t *Table) AccountModels(provider string) string {
	if _, ok := t.accountLists[provider]; !ok {
		return AccountModelsNone
	}
	switch t.Providers[provider].Driver {
	case DriverChatGPT:
		return AccountModelsOnly
	case DriverAnthropic:
		return AccountModelsAlso
	}
	return AccountModelsNone
}

// AccountListRead says the load used provider's account list (plan 050 C2):
// false for a provider whose models an account's list brings when the load
// had no list it could use — no file, one it cannot read, one bound to
// another account or key, one that gave no model (empty, or every entry
// rejected), one holding a model this craze is too old for, or no key or
// sign-in to bind one to — and so cannot tell a model the account does not
// list from one it never saw. false for every other provider.
func (t *Table) AccountListRead(provider string) bool {
	return t.accountLists[provider]
}

// FromAccount says alias is a model the load learned from an account's own
// list that the catalog has no seed for (plan 050 C2): every ChatGPT plan
// model, and an Anthropic model the account lists that craze does not ship —
// while it is still the list's model. One the user's models.toml points at
// another provider or wire model is the user's own (CatalogModel), whatever
// the list said of its alias (plan 050 review).
func (t *Table) FromAccount(alias string) bool {
	_, discovered := t.discovered[alias]
	return (discovered || t.anthropicListed[alias]) && t.catalogModels[alias]
}

// CatalogModel says alias is the catalog's own model (Resolved.CatalogModel):
// a shipped alias, or one an account's list brought, that the user's
// models.toml has not pointed at another provider or wire model.
func (t *Table) CatalogModel(alias string) bool { return t.catalogModels[alias] }

// RetiredRows are the catalog's [[retired]] rows, sorted by alias, each the
// caller's own (plan 050 C2). A table loaded without a catalog, or built in
// memory, has none.
func (t *Table) RetiredRows() []Retired {
	out := make([]Retired, 0, len(t.retired))
	for _, r := range t.retired {
		r.WireModels = slices.Clone(r.WireModels)
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b Retired) int { return cmp.Compare(a.Alias, b.Alias) })
	return out
}

// DeprecatedNotice is the one sentence craze says of alias when it is a
// deprecated model of t (plan 050 C2) — "<alias> is deprecated; use
// <replacement>" — and its replacement: a waiting command's stderr line and
// its warning's message, and the TUI's note at a session's open. ok is false
// for a model that is not deprecated, and for one t does not have.
func (t *Table) DeprecatedNotice(alias string) (replacedBy, sentence string, ok bool) {
	m, has := t.Models[alias]
	if !has || !m.Deprecated {
		return "", "", false
	}
	return m.ReplacedBy, alias + " is deprecated; use " + m.ReplacedBy, true
}
