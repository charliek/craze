package modeltable

import (
	"cmp"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"time"
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
//
// Its models are the catalog's seeds — ordinary [models."anthropic/<id>"]
// rows, with the history and retirement rules of any other (P12) and three
// catalog-only keys, family, released and thinking — and the account's own
// model list, cached beside the model files (anthropic_list.go), which is
// authoritative where it is used: the seeds it names stand as craze ships
// them, the ones it does not are hidden, and an id with no seed becomes a
// model of its own, unpriced. The catalog's [anthropic_defaults] says the
// rest: the family a new session starts on the newest of, the effort every
// model starts at, and whether the picker offers every model or only each
// family's newest.

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
// identity (factsOf) rather than keeping what was true of the old one: a
// ChatGPT plan model's parallel tool calls (nil, the driver's default) and an
// Anthropic model's thinking, family and release.
func (m *Model) forgetLearned() {
	m.Thinking = ""
	m.Family = ""
	m.Released = ""
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

// familyPattern is what a model's family must look like: a lowercase word,
// as the account's list names its lines ("opus", "sonnet", "haiku",
// "fable"). A list's line that is not one is no family (withAnthropicList).
var familyPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// familyProblem is why v is not a model's family, "" when it is (or is "",
// not known).
func familyProblem(v string) string {
	if v != "" && !familyPattern.MatchString(v) {
		return "not a family: a lowercase word of letters, digits and hyphens, at most 32"
	}
	return ""
}

// releasedProblem is why v is not a model's release date, "" when it is (or
// is "", not known): a calendar date, YYYY-MM-DD, so that dates compare as
// strings.
func releasedProblem(v string) string {
	if _, err := time.Parse(time.DateOnly, v); v != "" && err != nil {
		return fmt.Sprintf("%q is not a date: want YYYY-MM-DD", v)
	}
	return ""
}

// learnedFacts are what craze learned of one (provider, wire model) identity
// (P11): facts only its own knowledge holds, never a key of a file.
type learnedFacts struct {
	Thinking, Family, Released string
}

// factsOfModel is what m itself says of its identity.
func factsOfModel(m Model) learnedFacts {
	return learnedFacts{Thinking: m.Thinking, Family: m.Family, Released: m.Released}
}

// learnedOf is what cat's models say of their identities — the catalog's own
// models, and whatever a load added to its copy before the user's files are
// laid over it (the account's model list, §3.5), and the Anthropic models the
// load took out of the copy (listHidden: a seed the account does not list, a
// model the newest-only rule trims), which are still models craze knows —
// keyed by identity: the snapshot a model is looked up in by its identity
// (factsOf), whatever the user's entries make of the aliases that carried it.
// nil when no model says anything. Catalog identities are unique; where two
// models share one anyway, the first alias in sorted order says, a model in
// the copy before a hidden one.
func learnedOf(cat *Catalog) map[identity]learnedFacts {
	var out map[identity]learnedFacts
	for _, models := range []map[string]Model{cat.Models, cat.listHidden} {
		for _, alias := range slices.Sorted(maps.Keys(models)) {
			m := models[alias]
			f, id := factsOfModel(m), identity{m.Provider, m.WireModel}
			if _, seen := out[id]; seen || f == (learnedFacts{}) {
				continue
			}
			if out == nil {
				out = make(map[identity]learnedFacts)
			}
			out[id] = f
		}
	}
	return out
}

// factsOf is what craze knows of m's identity (P11): each of m's own learned
// fields where it is set, else what the table's snapshot (Table.learned),
// taken before the user's files were laid over the catalog, says of m's
// (provider, wire model) — so an alias the user points at an identity finds
// what craze knew of it, even when the user also pointed the alias that
// carried it elsewhere. A table with no catalog, or built in memory, has no
// snapshot: its models say what they say.
func (t *Table) factsOf(m Model) learnedFacts {
	f := t.learned[identity{m.Provider, m.WireModel}]
	return learnedFacts{
		Thinking: cmp.Or(m.Thinking, f.Thinking),
		Family:   cmp.Or(m.Family, f.Family),
		Released: cmp.Or(m.Released, f.Released),
	}
}

// thinkingOf is Resolved.Thinking for m on p (plan 044 §3.5's tri-state):
// "" for a model on any other driver, where it means nothing; otherwise what
// craze knows of the model's identity (factsOf), else ThinkingAdaptive — the
// current family's behaviour, so a hand-added id craze has never seen asks
// for thinking.
func (t *Table) thinkingOf(m Model, p Provider) string {
	if p.Driver != DriverAnthropic {
		return ""
	}
	return cmp.Or(t.factsOf(m).Thinking, ThinkingAdaptive)
}

// newestFirst orders two models of a family, or of the picker's Anthropic
// block, by their release dates, newest first, then by alias: a model whose
// release is not known after every dated one.
func newestFirst(releasedA, aliasA, releasedB, aliasB string) int {
	return cmp.Or(cmp.Compare(releasedB, releasedA), cmp.Compare(aliasA, aliasB))
}

// familiesNewestFirst is provider's models in models by family, each
// family's aliases newest first (newestFirst), as facts says each model's
// family and release: the one rule a start, a tier word and the newest-only
// trim pick by. A model of no known family is in none.
func familiesNewestFirst(models map[string]Model, provider string, facts func(Model) learnedFacts) map[string][]string {
	out := map[string][]string{}
	released := map[string]string{}
	for alias, m := range models {
		if f := facts(m); m.Provider == provider && f.Family != "" {
			out[f.Family] = append(out[f.Family], alias)
			released[alias] = f.Released
		}
	}
	for _, aliases := range out {
		slices.SortFunc(aliases, func(a, b string) int { return newestFirst(released[a], a, released[b], b) })
	}
	return out
}

// NewestOfFamily is the alias of provider's newest model of family in t —
// the latest release craze knows of (factsOf), then the first alias — and
// false when t has none (plan 044 O6: an unmapped tier word on an Anthropic
// parent runs on the newest model of that family). Whether the model
// resolves is not judged here.
func (t *Table) NewestOfFamily(provider, family string) (string, bool) {
	if newest := familiesNewestFirst(t.Models, provider, t.factsOf)[family]; len(newest) > 0 {
		return newest[0], true
	}
	return "", false
}

// shipsAnthropic reports whether c ships the Anthropic provider on its own
// driver: the one provider [anthropic_defaults] and the account's list are
// for.
func (c *Catalog) shipsAnthropic() bool {
	p, ok := c.Providers[AnthropicProvider]
	return ok && p.Driver == DriverAnthropic
}

// anthropicStart is the Anthropic provider's start as c holds it (O1): its
// models of [anthropic_defaults] start_family, newest first; nil when c ships
// no such provider, names no family, or has no model of it. Over a load's
// copy that is the effective set — the account's list where it is used, else
// the seeds — so the start follows the list.
func (c *Catalog) anthropicStart() []string {
	if !c.shipsAnthropic() {
		return nil
	}
	return familiesNewestFirst(c.Models, AnthropicProvider, factsOfModel)[c.Anthropic.StartFamily]
}

// validateAnthropic checks [anthropic_defaults] (plan 044 §3.5): it is only
// for a catalog that ships the Anthropic provider on its driver; a start
// family is a family, and one of the catalog's own models' — the seeds are
// the start a table with no account list falls back on (O1); a default effort
// is one an Anthropic request can carry; and list is "all" or "newest".
func (c *Catalog) validateAnthropic(name string) error {
	d := c.Anthropic
	at := func(key, reason string) error {
		return &FileError{File: name, Table: "anthropic_defaults", Key: key, Reason: reason}
	}
	if d == (AnthropicDefaults{}) {
		return nil
	}
	if !c.shipsAnthropic() {
		return at("", fmt.Sprintf("no provider %q on driver %q ships for these settings to apply to", AnthropicProvider, DriverAnthropic))
	}
	if r := familyProblem(d.StartFamily); r != "" {
		return at("start_family", r)
	}
	if d.StartFamily != "" && len(c.anthropicStart()) == 0 {
		return at("start_family", fmt.Sprintf("no model of the catalog's %q provider is of family %q: a table with no account list would start on none", AnthropicProvider, d.StartFamily))
	}
	if e := d.DefaultEffort; e != "" && !slices.Contains(anthropicEfforts, e) {
		return at("default_effort", fmt.Sprintf("%q is not an effort an Anthropic request can carry", e))
	}
	switch d.List {
	case "", AnthropicListAll, AnthropicListNewest:
	default:
		return at("list", fmt.Sprintf("unknown list %q: want %q or %q", d.List, AnthropicListAll, AnthropicListNewest))
	}
	return nil
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
