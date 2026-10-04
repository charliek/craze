package modeltable

import (
	"maps"
	"os"
	"reflect"
	"slices"
	"strings"
)

// A running session's table, reloaded (plan 034 §3.4, Q13–Q16). Plan 031's P8
// kept a session on the table it opened with; plan 034 lets a native session
// take a freshly loaded one while it runs, so a provider connected or a plan
// signed in to mid-session is offered at once. Two things a fresh Load cannot
// know have to be laid over it before a session takes it, and this file is
// both:
//
//   - the model the session runs on, whose entry goes into the new table
//     unchanged — profile, efforts, context window, cost and the account its
//     list was bound to — whether or not the new table still has its alias,
//     funds its provider or means the same model by it (Carry): the running
//     model is always listed (P7), and what the picker says of it is what its
//     client was built from — until the session switches away from it,
//     when a carry that stood over another entry of the files' own
//     (Shadowed) is what the session reads the files again for, so that
//     entry is what it offers from then on;
//   - the keys the session cannot take, being inside what it has already sent
//     unredacted with every request — its frozen prompt, its tools, its plan
//     path — whose providers it does not offer (WithholdFrozen, A22).

// Carry puts from's entry for alias — the model a running session is on — into
// t, in place of whatever t has under alias, as from holds it: profile,
// efforts, context window, cost, and the ChatGPT account from resolves it bound
// to (plan 034 §3.4, Q16). t's own provider of that id is kept when it has
// one, since every other model of the provider goes by it; from's is added
// when t has none, so the entry always names a provider t has (a user's
// provider removed from providers.toml while a session runs on it). Its
// origins come with it. It reports false, and changes nothing, when from has
// no such model.
//
// The entry is carried even when t has the alias funded and pointing at the
// same model: a fresher entry under the running model's alias would have the
// picker offer efforts — or a window, or a price — that the running client was
// not built with, and a switch to the alias the session is already on would
// then be refused for what the list just showed. A person who switches away
// and back takes the files' entry as it is then: a carry that stood over an
// entry t's own load read otherwise is marked (Shadowed), and the session
// that leaves it reads the files again.
func (t *Table) Carry(from *Table, alias string) bool {
	m, ok := from.Models[alias]
	if !ok {
		return false
	}
	if t.differs(from, alias) {
		put(&t.shadowed, alias, true)
	}
	acct, _ := from.boundAccount(alias)
	put(&t.Models, alias, cloneModel(m))
	if from.modelOrigins != nil {
		put(&t.modelOrigins, alias, from.ModelOrigin(alias))
	}
	if _, ok := t.Providers[m.Provider]; !ok {
		p := from.Providers[m.Provider] // present: from resolved the model
		p.EnvKeys = slices.Clone(p.EnvKeys)
		put(&t.Providers, m.Provider, p)
		if from.providerOrigins != nil {
			put(&t.providerOrigins, m.Provider, from.ProviderOrigin(m.Provider))
		}
		if p.Driver == DriverChatGPT && t.signIn == "" {
			t.signIn = from.signIn
		}
	}
	put(&t.carried, alias, acct)
	return true
}

// differs reports whether what t holds for alias says something else than
// from's entry, which Carry is about to put over it — so that, carried, t
// holds for alias what t's own load did not read (Shadowed): t has no such
// model, or another entry under it (any field: profile, efforts, window,
// cost, the model itself), or another origin — compared only when from
// keeps origins, as Carry writes one only then — or binds it to another
// account, or binds it where from does not or the other way about. A
// carry into a table that already holds the carried entry — the harness's
// own Carry after the adapter's, into the same table — differs in nothing.
// Read before Carry writes anything.
func (t *Table) differs(from *Table, alias string) bool {
	own, ok := t.Models[alias]
	if !ok || !reflect.DeepEqual(own, from.Models[alias]) {
		return true
	}
	if from.modelOrigins != nil && t.ModelOrigin(alias) != from.ModelOrigin(alias) {
		return true
	}
	ownAcct, ownBound := t.boundAccount(alias)
	acct, bound := from.boundAccount(alias)
	return ownAcct != acct || ownBound != bound
}

// Shadowed reports whether alias's entry in t is one Carry put over an entry
// of t's own that said something else — another account's model under the
// same name, the same model with other efforts or another price, or no entry
// at all — rather than the one t's load read (plan 034 C4r4, r15 #3). It is
// what a session that switches away from alias, the model it ran on, reads
// its files again for: the carry was there only for the running client
// (Q16), and the files' own entry — which no stamp says has changed, since
// it was on disk all along — is what the list should offer from then on, and
// what a switch back builds. A carry that differed in nothing is not
// shadowed: leaving it, t already holds what the files say.
func (t *Table) Shadowed(alias string) bool {
	return t.shadowed[alias]
}

// put sets (*m)[k] to v, making the map first when it is nil.
func put[V any](m *map[string]V, k string, v V) {
	if *m == nil {
		*m = make(map[string]V)
	}
	(*m)[k] = v
}

// boundAccount is the account alias resolves bound to, and whether it is
// bound to one: a carried model's own (Carry), else the account t's list was
// bound to when the model came from it (discovered), else none.
func (t *Table) boundAccount(alias string) (Account, bool) {
	if acct, ok := t.carried[alias]; ok {
		return acct, acct != (Account{})
	}
	if _, listed := t.discovered[alias]; listed {
		return t.discoveredFor, true
	}
	return Account{}, false
}

// WithholdFrozen keeps out of t every key a running session cannot take (plan
// 034 §3.4, A22): frozen reports a value the session has already sent
// unredacted with every request — inside its frozen prompt, its tools or its
// plan path — so learning it would refuse every later turn, and a switch to a
// model it funds would be refused. A provider any of whose keys frozen
// reports — an env_keys variable set to one, read as Keys reads it, or its
// inline key — is dropped with every model on it, so nothing offers it and
// nothing resolves it; and every variable whose value frozen reports reads as
// unset to t from then on (envOf) — it funds nothing and is no key Keys
// returns — while CredentialEnvNames still names it, so a command the session
// runs is still started without it. getenv is read as Keys reads it (nil is
// os.Getenv).
//
// keep is the model the session runs on, already carried in (Carry): it and
// its provider are never dropped, whatever the provider's keys hold — the
// running model is always listed (P7). It returns the providers dropped, in id
// order, for the caller's one note; nil when nothing was.
func (t *Table) WithholdFrozen(getenv func(string) string, frozen func(string) bool, keep string) []string {
	if getenv == nil {
		getenv = os.Getenv
	}
	// Taken before anything is dropped: a dropped provider's variables are
	// still credentials, and stay ones a command is started without.
	names := t.CredentialEnvNames()
	var unkeyed []string // sorted, as names is: envOf searches it
	for _, name := range names {
		if k, err := envKey(getenv, name); err == nil && k != "" && frozen(k.Reveal()) {
			unkeyed = append(unkeyed, name)
		}
	}
	keepProvider := ""
	if m, ok := t.Models[keep]; ok {
		keepProvider = m.Provider
	}
	var dropped []string
	for _, id := range slices.Sorted(maps.Keys(t.Providers)) {
		p := t.Providers[id]
		hit := slices.ContainsFunc(p.EnvKeys, func(n string) bool { return slices.Contains(unkeyed, n) })
		if v := strings.TrimSpace(p.APIKey.Reveal()); v != "" && frozen(v) {
			hit = true
		}
		if !hit || id == keepProvider {
			continue
		}
		dropped = append(dropped, id)
		for alias, m := range t.Models {
			if m.Provider != id {
				continue
			}
			delete(t.Models, alias)
			delete(t.modelOrigins, alias)
			delete(t.discovered, alias)
			delete(t.carried, alias)
			delete(t.shadowed, alias)
		}
		delete(t.Providers, id)
		delete(t.providerOrigins, id)
	}
	if len(unkeyed) == 0 && len(dropped) == 0 {
		return nil
	}
	t.credentialEnv = names
	t.unkeyed = unkeyed
	return dropped
}

// envOf is getenv (nil is os.Getenv) as t reads it: a variable WithholdFrozen
// marked reads as unset, so it funds nothing Resolve resolves — the model a
// session runs on included, which it keeps listed all the same (P7) — and is
// no key Keys returns.
func (t *Table) envOf(getenv func(string) string) func(string) string {
	if getenv == nil {
		getenv = os.Getenv
	}
	if len(t.unkeyed) == 0 {
		return getenv
	}
	unkeyed := t.unkeyed
	return func(name string) string {
		if _, ok := slices.BinarySearch(unkeyed, name); ok {
			return ""
		}
		return getenv(name)
	}
}
