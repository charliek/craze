package modeltable

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
)

// Anthropic's Claude API in the model table (plan 044 §3.3, §3.5).
//
// Its provider, "anthropic" on the anthropic driver, is funded by an API key
// like any other, and its driver is craze's own Messages API client (package
// llm/messagesapi) at Anthropic's fixed endpoint. Like the ChatGPT plan's,
// the driver belongs to the one provider made for it (validateProvider). The
// id is not reserved the other way (plan 044 X1): a provider of the user's own
// named anthropic, on another driver, is theirs — where the catalog does not
// ship the provider, or the catalog is off — and funds what they configured.
// Once the catalog ships the provider its entry is locked (lockAnthropic): a
// user's providers.toml may rename it and say where its key comes from, and
// nothing else, so the key goes to Anthropic's API and nowhere else (P22).

const (
	// AnthropicProvider is the Anthropic provider's id: the one id a provider
	// on DriverAnthropic may have (validateProvider). A provider of the user's
	// own on that driver under another id is dropped with a warning.
	AnthropicProvider = "anthropic"

	// ThinkingAdaptive and ThinkingNone are a model's thinking on the
	// anthropic driver (Model.Thinking, Resolved.Thinking): adaptive thinking,
	// which every request on it then asks for with its display and its
	// block_binding (plan 044 §3.3), or none at all — a model that takes no
	// thinking field (Opus 4.5, Haiku 4.5).
	ThinkingAdaptive = "adaptive"
	ThinkingNone     = "none"
)

// anthropicEfforts are the efforts an Anthropic request can carry
// (output_config.effort), lowest first: the API's own levels, which the
// account's model list reports per model in this order. Package llm's
// EffortOptions holds its own copy, and a test there pins the two together,
// and to the levels the Messages API client reads from a model's
// capabilities.
var anthropicEfforts = []string{"low", "medium", "high", "xhigh", "max"}

// AnthropicEfforts are the efforts an Anthropic request can carry, lowest
// first.
func AnthropicEfforts() []string { return slices.Clone(anthropicEfforts) }

// forgetLearned clears what craze learned of a model's identity — the fields
// only its own knowledge sets, never a file (P11) — so a model an entry
// points at another provider or wire model is looked up again by its new
// identity (thinkingFor) rather than keeping what was true of the old one: a
// ChatGPT plan model's parallel tool calls (nil, the driver's default) and an
// Anthropic model's thinking. Every learned field of an identity belongs
// here: plan 044 §3.5's Family and Released join them when they land.
func (m *Model) forgetLearned() {
	m.Thinking = ""
	m.ParallelToolCalls = nil
}

// thinkingProblem is why v is not a model's thinking, "" when it is: "" (not
// known), ThinkingAdaptive or ThinkingNone.
func thinkingProblem(v string) string {
	switch v {
	case "", ThinkingAdaptive, ThinkingNone:
		return ""
	}
	return fmt.Sprintf("unknown thinking %q: want %q or %q", v, ThinkingAdaptive, ThinkingNone)
}

// learnedFacts are what craze learned of one (provider, wire model) identity
// (P11): facts only its own knowledge holds, never a key of a file. Plan 044
// §3.5's Family and Released join Thinking here when they land.
type learnedFacts struct {
	Thinking string
}

// learnedOf is what cat's models say of their identities — the catalog's own
// models, and whatever a load added to its copy before the user's files are
// laid over it (the account's model list, §3.5) — keyed by identity: the
// snapshot a model is looked up in by its identity (thinkingFor), whatever
// the user's entries make of the aliases that carried it. nil when no model
// says anything. Catalog identities are unique; where two models share one
// anyway, the first alias in sorted order says.
func learnedOf(cat *Catalog) map[identity]learnedFacts {
	var out map[identity]learnedFacts
	for _, alias := range slices.Sorted(maps.Keys(cat.Models)) {
		m := cat.Models[alias]
		f, id := learnedFacts{Thinking: m.Thinking}, identity{m.Provider, m.WireModel}
		if _, seen := out[id]; seen || f == (learnedFacts{}) {
			continue
		}
		if out == nil {
			out = make(map[identity]learnedFacts)
		}
		out[id] = f
	}
	return out
}

// thinkingOf is Resolved.Thinking for m on p (plan 044 §3.5's tri-state):
// "" for a model on any other driver, where it means nothing; otherwise the
// model's own Thinking when it is known, else what craze learned of its
// identity (thinkingFor), else ThinkingAdaptive — the current family's
// behaviour, so a hand-added id craze has never seen asks for thinking.
func (t *Table) thinkingOf(m Model, p Provider) string {
	switch {
	case p.Driver != DriverAnthropic:
		return ""
	case m.Thinking != "":
		return m.Thinking
	}
	return cmp.Or(t.thinkingFor(m.Provider, m.WireModel), ThinkingAdaptive)
}

// thinkingFor is the thinking craze learned the (provider, wire model)
// identity takes, "" when it learned none: from the table's snapshot
// (Table.learned), taken before the user's files were laid over the catalog,
// so an alias the user points at an identity finds what craze knew of it
// even when the user also pointed the alias that carried it elsewhere (P11).
// A table with no catalog, or built in memory, has no snapshot.
func (t *Table) thinkingFor(provider, wire string) string {
	return t.learned[identity{provider, wire}].Thinking
}

// anthropicLockReason ends the warning for each key of the Anthropic
// provider's entry the lock drops: what the entry may set instead.
const anthropicLockReason = "craze's own Anthropic provider takes only name, env_keys and api_key"

// lockAnthropic locks pd's entries for the providers cat ships on the
// anthropic driver (P22): every key but name, env_keys and api_key is dropped
// — a driver other than the anthropic driver, base_url, source — with one
// warning line per key, naming it and never its value. It runs before the
// user's files are checked, so a key the lock drops cannot fail the load
// first: a gateway's base_url, an unknown driver. An env_keys of the user's
// stays, an explicit empty one included: that is how an exported
// ANTHROPIC_API_KEY is kept from funding craze (P24), and a source would let
// an old import's path restore the shipped variables over it (merger.provider).
func lockAnthropic(cat *Catalog, ppath string, pd *providersOverlay) []string {
	var warnings []string
	for _, id := range slices.Sorted(maps.Keys(pd.Providers)) {
		if shipped, ok := cat.Providers[id]; !ok || shipped.Driver != DriverAnthropic {
			continue
		}
		o := pd.Providers[id]
		drop := func(key, reason string) {
			fe := &FileError{File: ppath, Table: providerTable(id), Key: key, Reason: reason + "; " + anthropicLockReason}
			warnings = append(warnings, fe.Error()+"; craze's own Anthropic provider is used")
		}
		if o.Driver != nil && *o.Driver != DriverAnthropic {
			drop("driver", fmt.Sprintf("the Anthropic provider is always driver %q", DriverAnthropic))
		}
		o.Driver = nil
		if o.BaseURL != nil {
			drop("base_url", fixedEndpoint(DriverAnthropic))
			o.BaseURL = nil
		}
		if o.Source != nil {
			drop("source", "the Anthropic provider's entry is never an old import's")
			o.Source = nil
		}
		pd.Providers[id] = o
	}
	return warnings
}
