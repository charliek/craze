package modeltable

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// One source per maker (plan 050 C5). When one maker's models are reachable
// through several providers — OpenAI's through the ChatGPT plan, the OpenAI
// key and OpenRouter — craze offers them from one source: the first provider
// in the source order that funds one of the maker's active models. The rule
// changes what is offered, never what a name resolves to: a hidden model
// still runs by name (--model, the pin, a sub-agent's configured model), and
// craze never falls back from one source to another on its own.
//
// Three sets, kept apart (the plan's Concepts):
//
//   - runs: the models that resolve (Resolve);
//   - offered: the models a picker lists — they resolve, are not deprecated,
//     and are not hidden by the source order (Offered);
//   - eligible to start automatically: offered, less what the paid-source
//     guard holds back (Offers.Held), for one of three reasons:
//     1. a model whose maker has an earlier source whose credential is
//     there but broken for the maker — the ChatGPT plan signed in with no
//     usable list, another account's list, or plan usage off — so a plan
//     whose model list is missing never silently moves new sessions to a
//     key billed per token of the same maker;
//     2. any model billed per token — it, or any model of its provider, has
//     a price — of a provider that comes after such a broken sign-in in the
//     start order (StartOrder) or is not in it, whatever its maker: a
//     provider before the sign-in would have been chosen first anyway, and
//     a provider none of whose models has a price is not a bill;
//     3. a model of a provider the catalog marks env_key_by_name_only whose
//     key is found only in the environment: an exported OPENAI_API_KEY is
//     often there for other tools, and only a stored key is the owner's
//     consent to bill craze's sessions to it.
//     A key provider that funds none of a maker's active models holds
//     nothing back: a usable key resolves its models, so it has left that
//     maker (its models there are deprecated), and the replacement
//     elsewhere is the way on.
//
// Each is computed once per call from the table and the environment as they
// are now (Offers), never stored at load: a key stored or a sign-in completed
// changes them at the next call.

// ErrSourceHeld is StartPick's error when the guard left nothing to start on
// (HeldError): something is funded, but craze does not choose it for the
// owner. It is not ErrNothingFunded, and not ErrNoAPIKey: a key is there.
var ErrSourceHeld = errors.New("modeltable: craze does not choose a funded model for the owner while the paid-source guard holds it back")

// HeldError is why craze does not choose Alias for the owner (plan 050 C5's
// paid-source guard), of one of two kinds:
//
//   - the sign-in's (EnvVar ""): Source, the ChatGPT plan, is signed in but
//     resolves none of its models, and is an earlier source of the model's
//     Maker in the source order, or is in the start order before the
//     model's provider, which a model billed per token is on, or which the
//     order leaves out (Maker may then be "": a model of no maker);
//   - the environment's (EnvVar set): Source, the model's own provider, is
//     funded only by the variable EnvVar and has no key stored, and the
//     catalog marks it env_key_by_name_only. SourceName is its display name.
//
// It unwraps to ErrSourceHeld. Its text names a provider, a variable's name
// and an alias, never a token or a key.
type HeldError struct {
	Alias      string // the model craze would have chosen
	Maker      string
	Source     string // the provider to set up: the sign-in's, or the model's own
	SourceName string // Source's display name, for the environment's kind
	EnvVar     string // the variable that alone funds Source; "" for the sign-in's kind
}

func (e *HeldError) Error() string {
	return fmt.Sprintf("modeltable: %s, so craze does not choose %s on its own", e.Reason(), e.Alias)
}

func (e *HeldError) Unwrap() error { return ErrSourceHeld }

// Reason is why nothing started, for a person: "signed in to ChatGPT, but its
// models are not available", or "OPENAI_API_KEY is set in the environment,
// but no key is stored for OpenAI".
func (e *HeldError) Reason() string {
	if e.EnvVar != "" {
		return fmt.Sprintf("%s is set in the environment, but no key is stored for %s", e.EnvVar, cmp.Or(e.SourceName, e.Source))
	}
	return "signed in to " + chatgptShortName + ", but its models are not available"
}

// Fix is what to do about it, for either kind: sign in again or store the
// key, or name the model.
func (e *HeldError) Fix() string {
	return fmt.Sprintf("craze auth login %s, or start with --model %s", e.Source, e.Alias)
}

// chatgptShortName is the ChatGPT plan's provider as HeldError says it.
const chatgptShortName = "ChatGPT"

// makerNames are the shipped makers as a sentence says them.
var makerNames = map[string]string{
	"anthropic": "Anthropic",
	"deepseek":  "DeepSeek",
	"meta":      "Meta",
	"openai":    "OpenAI",
	"zai":       "GLM",
}

// MakerName is maker as a sentence names it ("OpenAI models"): the shipped
// makers by name, any other by its id.
func MakerName(maker string) string {
	return cmp.Or(makerNames[maker], maker)
}

// EffectiveSourceOrder is the source order in force (plan 050 C5): the
// user's source_order — each name a provider of t, the rest passed over —
// then the catalog's other providers, in the catalog's order; the catalog's
// alone when the user wrote none; and nil — the rule off — for `source_order
// = []`, and for a table whose catalog names no maker.
func (t *Table) EffectiveSourceOrder() []string {
	if t.SourceOrder != nil && len(t.SourceOrder) == 0 {
		return nil
	}
	return t.sourceOrder()
}

// sourceOrder is EffectiveSourceOrder whether the rule is on or off: with
// `source_order = []` the catalog's own, which the paid-source guard and
// OtherSources still read. nil when there is neither.
func (t *Table) sourceOrder() []string {
	var out []string
	for _, id := range slices.Concat(t.SourceOrder, t.catalogSources) {
		if hasProvider(t.Providers, id) && !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out
}

// Offers is the source rule's sets for one reading of the environment (plan
// 050 C5), computed once: what runs, what is offered, what the source order
// hides, and what the paid-source guard holds back. The caller owns the maps.
type Offers struct {
	// Runs is every model that resolves (Resolve).
	Runs map[string]bool
	// Offered is what a picker lists and craze may choose for the owner:
	// it runs, is not deprecated, and is not hidden.
	Offered map[string]bool
	// Hidden is every model that runs but that the source order hides: its
	// maker's winner is another provider. A deprecated model is judged too.
	Hidden map[string]bool
	// Held is why craze may not choose a model that runs and is not hidden
	// for the owner (the paid-source guard, for any of its three reasons),
	// by alias; a model not here is eligible. A model held for more than one
	// reason holds the sign-in's: the per-maker rule's, then the start
	// order's, then the environment's.
	Held map[string]*HeldError
	// Winners is each maker's source: the first provider in the source order
	// with an active model of the maker that resolves. A maker with none has
	// no key.
	Winners map[string]string
}

// Offers is t's sets with getenv (nil is os.Getenv): see the type.
func (t *Table) Offers(getenv func(string) string) Offers {
	o := Offers{Runs: map[string]bool{}, Offered: map[string]bool{}, Hidden: map[string]bool{}, Held: map[string]*HeldError{}, Winners: map[string]string{}}
	type source struct{ provider, maker string }
	funds := map[source]bool{}  // the provider funds an active model of the maker
	active := map[string]bool{} // the provider funds an active model of any maker
	for alias, m := range t.Models {
		if _, err := t.Resolve(alias, getenv); err == nil {
			o.Runs[alias] = true
			if !m.Deprecated {
				funds[source{m.Provider, m.Maker}] = true
				active[m.Provider] = true
			}
		}
	}
	order := t.EffectiveSourceOrder()
	sources := t.makerSources()
	for maker := range sources {
		if i := slices.IndexFunc(order, func(p string) bool { return funds[source{p, maker}] }); i >= 0 {
			o.Winners[maker] = order[i]
		}
	}
	for alias := range o.Runs {
		m := t.Models[alias]
		if w, ok := o.Winners[m.Maker]; ok && slices.Contains(order, m.Provider) && w != m.Provider {
			o.Hidden[alias] = true
			continue
		}
		if !m.Deprecated {
			o.Offered[alias] = true
		}
	}
	// The guard: for each alias that runs and is not hidden, the first source
	// of its maker before its own provider that is signed in (brokenSignIn)
	// and funds none of the maker's active models. "Before" is in sourceOrder, the catalog's
	// own while the user's `source_order = []` has the hiding off: turning
	// off what is hidden does not turn off the guard, which only ever
	// declines to choose for the owner.
	guard := t.sourceOrder()
	signedIn := map[string]bool{}
	isSignedIn := func(p string) bool {
		v, ok := signedIn[p]
		if !ok {
			v = t.signedIn(p)
			signedIn[p] = v
		}
		return v
	}
	for alias := range o.Runs {
		m := t.Models[alias]
		if m.Maker == "" || o.Hidden[alias] {
			continue
		}
		at := slices.Index(guard, m.Provider)
		for _, s := range guard[:max(at, 0)] {
			if !slices.Contains(sources[m.Maker], s) || funds[source{s, m.Maker}] || !isSignedIn(s) {
				continue
			}
			o.Held[alias] = &HeldError{Alias: alias, Maker: m.Maker, Source: s}
			break
		}
	}
	t.holdAfterBrokenSignIn(o, active, isSignedIn)
	t.holdEnvOnlyKeys(o, getenv)
	return o
}

// holdAfterBrokenSignIn is the guard's second reason (see the file's
// comment): the first sign-in in StartOrder that is signed in and resolves
// no active model of its own (active, by provider) holds back every model
// billed per token of a provider after it in StartOrder or not in it at
// all, deprecated ones included, which o does not hide or hold already. A
// model is billed per token when it has a price (its Cost set, as
// Route.Priced reads it) or any model of its provider does, in t whatever
// its state or in the catalog (billedProviders: the account's list can take
// every priced seed out of t): a model of a paid provider with no price of its own — one only
// the Anthropic account's list names, or an entry of the user's on a
// shipped provider — is billed all the same. A provider none of whose
// models has a price (a local server, a flat plan) is not. A sign-in
// StartOrder leaves out is never a start, so it holds nothing; a working
// one, or a registration a sign-out left, holds nothing either. The order is
// the start order, not the source order: `source_order = []` changes
// nothing here.
func (t *Table) holdAfterBrokenSignIn(o Offers, active map[string]bool, isSignedIn func(string) bool) {
	order := t.StartOrder()
	at := slices.IndexFunc(order, func(p string) bool { return !active[p] && isSignedIn(p) })
	if at < 0 {
		return
	}
	// A provider with a model that has a price: in the table, or in the
	// catalog (billedProviders), where the account's list may have taken the
	// priced ones out of the table.
	priced := maps.Clone(t.billedProviders)
	if priced == nil {
		priced = map[string]bool{}
	}
	for _, m := range t.Models {
		if m.Cost != nil {
			priced[m.Provider] = true
		}
	}
	for alias := range o.Runs {
		m := t.Models[alias]
		if (m.Cost == nil && !priced[m.Provider]) || o.Hidden[alias] || o.Held[alias] != nil {
			continue
		}
		if i := slices.Index(order, m.Provider); i < 0 || i > at {
			o.Held[alias] = &HeldError{Alias: alias, Maker: m.Maker, Source: order[at]}
		}
	}
}

// holdEnvOnlyKeys is the guard's third reason (see the file's comment): each
// model, deprecated ones included, of a provider marked env_key_by_name_only
// that is funded by a variable alone — one of its env_keys set to a usable
// key, as Resolve and Providers judge it, and no usable api_key stored —
// which o does not hide or hold already, so a sign-in's reason wins. A
// provider with a stored key is not held even where the variable is the key
// Resolve takes: storing one is the owner's consent.
func (t *Table) holdEnvOnlyKeys(o Offers, getenv func(string) string) {
	envOnly := map[string]string{} // provider id → the variable that alone funds it
	for id, on := range t.envKeyByNameOnly {
		p, ok := t.Providers[id]
		if !on || !ok || storedKeyUsable(p.APIKey) {
			continue
		}
		if name, _, _ := firstEnvKey(t.envOf(getenv), p.EnvKeys); name != "" {
			envOnly[id] = name
		}
	}
	if len(envOnly) == 0 {
		return
	}
	for alias := range o.Runs {
		m := t.Models[alias]
		name, ok := envOnly[m.Provider]
		if !ok || o.Hidden[alias] || o.Held[alias] != nil {
			continue
		}
		o.Held[alias] = &HeldError{Alias: alias, Maker: m.Maker, Source: m.Provider, SourceName: t.ProviderName(m.Provider), EnvVar: name}
	}
}

// storedKeyUsable says k, an inline api_key, is one Resolve would take and
// Providers would call stored: not blank once trimmed, and one KeyProblem
// accepts.
func storedKeyUsable(k Secret) bool {
	v := strings.TrimSpace(k.Reveal())
	return v != "" && KeyProblem(v) == nil
}

// makerSources is each maker's sources: every provider whose maker it is
// (providerMakers — so the ChatGPT plan is OpenAI's source even with no model
// list read), and the provider of every model of the maker, deprecated ones
// included. Its keys are every maker t names.
func (t *Table) makerSources() map[string][]string {
	out := map[string][]string{}
	add := func(maker, p string) {
		if !slices.Contains(out[maker], p) {
			out[maker] = append(out[maker], p)
		}
	}
	for p, maker := range t.providerMakers {
		add(maker, p)
	}
	for _, m := range t.Models {
		if m.Maker != "" {
			add(m.Maker, m.Provider)
		}
	}
	return out
}

// ProviderName is provider id as a sentence names it: its display name, else
// its id.
func (t *Table) ProviderName(id string) string {
	return cmp.Or(t.Providers[id].Name, id)
}

// signedIn says provider id is a sign-in source that is signed in, whatever
// its models resolve to: the ChatGPT plan with a registration and a token
// file — plan usage granted or not, the account's list read or not, another
// account's or not; not a registration a sign-out left behind. It reads the
// sign-in files Resolve reads, never a token. A key provider is never one.
func (t *Table) signedIn(id string) bool {
	return t.Providers[id].Driver == DriverChatGPT && signedInAtAll(t.signIn)
}

// Route is another source of a model's maker that funds active models of it
// (OtherSources): the provider, its display name, whether it is the ChatGPT
// plan's sign-in, whether any of its models here is priced per token, its
// models of the maker that resolve and are not deprecated (sorted), and the
// one to name first — the model of the same wire id when there is one, else
// the one whose alias shares the most words with the model's (gpt-6.1-sol's
// gpt-sol-latest, not gpt-astra-latest), the provider's start before the
// rest on a tie.
type Route struct {
	Provider     string
	ProviderName string
	SignIn       bool
	Priced       bool
	Aliases      []string
	First        string
}

// OtherSources is every source of alias's maker but alias's own provider
// that funds an active model of it, in the source order (the catalog's while
// the user's has the rule off): the routes a person on alias could continue
// on, each with its own payer (plan 050 C5's quota hint). nil for a model of
// no maker, or one t does not have. It never chooses one.
func (t *Table) OtherSources(alias string, getenv func(string) string) []Route {
	cur, ok := t.Models[alias]
	if !ok || cur.Maker == "" {
		return nil
	}
	o := t.Offers(getenv)
	var out []Route
	for _, p := range t.sourceOrder() {
		if p == cur.Provider {
			continue
		}
		var r Route
		best := -1
		for _, a := range t.Aliases() {
			m := t.Models[a]
			if m.Provider != p || m.Maker != cur.Maker || m.Deprecated || !o.Runs[a] {
				continue
			}
			r.Aliases = append(r.Aliases, a)
			r.Priced = r.Priced || m.Cost != nil
			score := 2 * sharedWords(alias, a)
			switch {
			case m.WireModel == cur.WireModel:
				score = 1 << 20
			case slices.Contains(t.starts[p], a):
				score++
			}
			if score > best {
				r.First, best = a, score
			}
		}
		if len(r.Aliases) == 0 {
			continue
		}
		r.Provider, r.ProviderName, r.SignIn = p, t.ProviderName(p), t.Providers[p].Driver == DriverChatGPT
		out = append(out, r)
	}
	return out
}

// MakerSource is one maker whose models the source order hides somewhere
// (HiddenSources): the provider they are offered from and every other
// provider with funded models of the maker that are hidden, in the source
// order, with how many each.
type MakerSource struct {
	Maker  string
	Winner string
	Hidden []HiddenSource
}

// HiddenSource is a provider whose funded models of a maker are hidden.
type HiddenSource struct {
	Provider string
	Count    int
}

// HiddenSources is every maker the source order hides funded models of now,
// by maker: what `craze auth list`, `craze models` and the TUI's /connect
// note say. nil when it hides nothing.
func (t *Table) HiddenSources(getenv func(string) string) []MakerSource {
	o := t.Offers(getenv)
	order := t.EffectiveSourceOrder()
	var out []MakerSource
	for _, maker := range slices.Sorted(maps.Keys(o.Winners)) {
		ms := MakerSource{Maker: maker, Winner: o.Winners[maker]}
		for _, p := range order {
			n := 0
			for alias := range o.Hidden {
				if m := t.Models[alias]; m.Provider == p && m.Maker == maker && !m.Deprecated {
					n++
				}
			}
			if n > 0 {
				ms.Hidden = append(ms.Hidden, HiddenSource{Provider: p, Count: n})
			}
		}
		if len(ms.Hidden) > 0 {
			out = append(out, ms)
		}
	}
	return out
}

// sharedWords is how many words the model parts of aliases a and b share —
// each cut at its first "/", then at "-" and "." — counting each word once.
func sharedWords(a, b string) int {
	words := func(alias string) map[string]bool {
		if _, rest, ok := strings.Cut(alias, "/"); ok {
			alias = rest
		}
		out := map[string]bool{}
		for _, w := range strings.FieldsFunc(alias, func(r rune) bool { return r == '-' || r == '.' }) {
			out[w] = true
		}
		return out
	}
	wa, n := words(a), 0
	for w := range words(b) {
		if wa[w] {
			n++
		}
	}
	return n
}
