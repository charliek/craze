package harness

import (
	"slices"

	"charm.land/fantasy"
	"github.com/charliek/craze/internal/harness/modeltable"
	"github.com/charliek/craze/internal/harness/store"
)

// What a session has spent (plan 028 §3.14, PD18, PD22), and the Spent event
// that reports it after every step, every compaction and a Replay — in a
// session that is not a sub-agent only (P33).
//
// The accounting rule is Plan 026 §3.7's, extended: a session's spend is the
// usage of its own assistant entries, each priced by that entry's model; plus
// the subagent_usage rows every entry that owns them carries — a step's tool
// entry, a results entry — each priced by its row's model; plus every
// compaction entry's usage, a success's or a failure's, every attempt summed,
// priced by its model; plus the usage this incarnation observed that no entry
// holds (unsavedUsage). A child's own transcript is never added in: what a
// child spent is the rows its parent's entries carry, its compactions
// included (§3.17). A background child whose result was never delivered is
// in no entry and not here either: SubagentUndelivered is journal-only (plan
// 026 §3.11, plan 028 §4). Every read walks the transcript's path, root to
// leaf, never the file's order.
//
// A turn's spend is the part of that which belongs to the turn: an entry
// belongs to the turn of the latest record at or before it on the path that
// numbers one (turnReader) — so a steer, a step, a mid-turn results entry and
// a mid-turn compaction belong to the turn they ran in — and a compaction entry
// to its own turn, so one before a turn's first request counts toward the turn
// it preceded, and a manual one is a turn of its own (R2-4). What no entry
// holds belongs to the turn that observed it.
//
// Money is integer picodollars (modeltable.Rates): a usage's cost is its input
// at the input rate, its output — reasoning inside it, as providers bill it —
// at the output rate, and its cache reads and writes at theirs, exact in int64.
// Every usage is priced by its own model's (provider, wire model), whatever
// alias it names (modeltable.Table.Price, R2-7); one with no price marks the
// sum Unpriced and adds its tokens and no cost.
//
// A usage is priced at the time of use, and keeps that price (plan 034 §3.4,
// A23; C4r, r9 #5): the first time a Spent counts it — the one after the
// step, the compaction or the Replay that wrote it, with no table swapped in
// meanwhile, since SetTable is refused while a turn or a Replay runs — its
// rates are read from the session's table as it is then, an identity that
// table does not price from the table the session opened with, and recorded
// against the entry that holds it (Session.prices); usage no entry holds is
// priced the same way as it is noted (noteUnsaved). A table swapped in later
// prices only what is used under it: usage on a model it dropped — a provider
// removed, an alias gone — is not unpriced, and usage on a model whose rates
// it changed is not repriced, so a swap never changes what the session has
// spent.

// unsavedUsage is usage this incarnation was billed for that no entry holds
// (§3.14): a step no append wrote — its save failed (DiagSaveFailed), it was
// refused for its call ids, or it had nothing to write — with the sub-agent
// rows it carried, and a compaction whose entry could not be written
// (DiagCompactionUnsaved). model prices it and turn is the turn that observed
// it. A resumed session starts with none: a transcript never holds these,
// which is why a live spend and the same session's after a resume can differ
// by exactly them.
type unsavedUsage struct {
	turn  int
	usage store.Usage
	// price is m's rates as the session's table had them when the usage was
	// noted (the time of use, above).
	price priced
}

// noteUnsaved records usage the session was billed for in turn, on m, that no
// entry holds, so its spend still counts it, priced now (the time of use). A
// usage of nothing records nothing.
func (s *Session) noteUnsaved(turn int, m store.Model, u store.Usage) {
	if u == (store.Usage{}) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p := pricer{table: s.table, opened: s.openTable}
	s.unsaved = append(s.unsaved, unsavedUsage{turn: turn, usage: u, price: p.lookup(m)})
}

// usageKey names one priced usage (Session.prices): the entry that holds it
// and the identity it is priced by — an entry's own usage and its sub-agent
// rows, one key per model among them, since one entry's usages of one
// identity were used under one table.
type usageKey struct {
	entry               string
	provider, wireModel string
}

// stepSpent is the Spent after turn's step d, which ran on m: the step
// first noted as observed and unsaved when no append wrote it — its usage on
// its model, and each of its sub-agents' rows on the row's model (the rows
// StepDone reports whether or not the step was saved, plan 026 §3.7) — then
// the session's spend as it now is. The turn calls it under its lock (the
// turn's spent), after the step's StepDone.
func (s *Session) stepSpent(turn int, m model, d StepDone) Spent {
	if !d.Saved {
		s.noteUnsaved(turn, store.Model{Provider: d.Provider, Alias: d.Model, WireModel: d.WireModel}, d.Usage)
		for _, row := range d.SubagentUsage {
			s.noteUnsaved(turn, rowModel(row), row.Usage)
		}
	}
	return s.spent(turn, m.r)
}

// spent is the Spent the session reports now: turn's spend and the
// session's, and the context the next request sends to next's model — its
// size (§3.7) and that model's window.
func (s *Session) spent(turn int, next modeltable.Resolved) Spent {
	tokens, _ := s.contextTokensOn(next)
	inTurn, session := s.spend(turn)
	return Spent{ContextTokens: tokens, ContextWindow: int64(next.ContextWindow), Turn: inTurn, Session: session}
}

// spend is turn's spend and the session's, by the rule above: every usage on
// the transcript's path, and every one this incarnation observed unsaved,
// each at its price at the time of use — the one recorded when it was first
// counted, or, for a usage counted now for the first time, the session's
// table's (the open-time table's for an identity it does not price),
// recorded here. It takes the store's lock and then the session's, one at a
// time: the walk holds neither, and under the session's only the prices not
// yet recorded are read, a few identities per call.
func (s *Session) spend(turn int) (inTurn, session Spend) {
	type use struct {
		key    usageKey
		usage  store.Usage
		inTurn bool
	}
	tr := s.store.Transcript()
	path, _ := tr.Branch(tr.Leaf()) // the leaf is always known
	var uses []use
	var turns turnReader
	for i := range path {
		e := &path[i]
		turns.read(e)
		entryUsage(e, func(m store.Model, u store.Usage) {
			uses = append(uses, use{key: usageKey{entry: e.ID, provider: m.Provider, wireModel: m.WireModel}, usage: u, inTurn: turns.turn == turn})
		})
	}

	s.mu.Lock()
	p := pricer{table: s.table, opened: s.openTable}
	prices := make([]priced, len(uses))
	for i, u := range uses {
		pr, ok := s.prices[u.key]
		if !ok {
			pr = p.lookup(store.Model{Provider: u.key.provider, WireModel: u.key.wireModel})
			if s.prices == nil {
				s.prices = make(map[usageKey]priced)
			}
			s.prices[u.key] = pr
		}
		prices[i] = pr
	}
	unsaved := slices.Clone(s.unsaved)
	s.mu.Unlock()

	for i, u := range uses {
		session.add(u.usage, prices[i].rates, prices[i].ok)
		if u.inTurn {
			inTurn.add(u.usage, prices[i].rates, prices[i].ok)
		}
	}
	for _, u := range unsaved {
		session.add(u.usage, u.price.rates, u.price.ok)
		if u.turn == turn {
			inTurn.add(u.usage, u.price.rates, u.price.ok)
		}
	}
	return inTurn, session
}

// entryUsage hands visit each usage e holds by the rule, with the model that
// prices it: an assistant entry's own usage on its model; each subagent_usage
// row of a message entry of any role on the row's model; a compaction
// entry's usage — every attempt's — on the model its summarizer ran on. A
// tool entry carries no usage of its own (only rows), a user entry none, and
// a change, a resume, a reminder or a newer craze's entry nothing.
func entryUsage(e *store.Entry, visit func(store.Model, store.Usage)) {
	switch e.Type {
	case store.TypeCompaction:
		if e.Usage != nil {
			visit(e.Model, *e.Usage)
		}
	case store.TypeMessage:
		if e.Message.Role == fantasy.MessageRoleAssistant && e.Usage != nil {
			visit(e.Model, *e.Usage)
		}
		for _, row := range e.SubagentUsage {
			visit(rowModel(row), row.Usage)
		}
	}
}

// rowModel is the model a sub-agent usage row names.
func rowModel(row store.ModelUsage) store.Model {
	return store.Model{Provider: row.Provider, Alias: row.Model, WireModel: row.WireModel}
}

// pricer prices usage through a model table by identity — (provider, wire
// model), never the alias a record names (modeltable.Table.Price, R2-7) —
// looking each identity up once: Price rebuilds the table's identity map on
// every call (X42), and one Spent may price many records of a few models. An
// identity table does not price is priced by opened, the table the session
// opened with, when that is another one (plan 034 §3.4); nil is none.
type pricer struct {
	table  *modeltable.Table
	opened *modeltable.Table
	seen   map[[2]string]priced
}

// priced is one identity's lookup: its rates, and whether it has any.
type priced struct {
	rates modeltable.Rates
	ok    bool
}

// lookup is m's identity's rates, and whether the table prices it.
func (p *pricer) lookup(m store.Model) priced {
	key := [2]string{m.Provider, m.WireModel}
	if pr, ok := p.seen[key]; ok {
		return pr
	}
	var pr priced
	pr.rates, pr.ok = p.table.Price(m.Provider, m.WireModel)
	if !pr.ok && p.opened != nil && p.opened != p.table {
		pr.rates, pr.ok = p.opened.Price(m.Provider, m.WireModel)
	}
	if p.seen == nil {
		p.seen = make(map[[2]string]priced)
	}
	p.seen[key] = pr
	return pr
}

// add adds u to sp: its tokens always, and, when it is priced at r, its cost
// — input, output (reasoning inside it, never priced again), cache reads and
// cache writes each at its rate. A usage with no price and something in it
// marks sp Unpriced.
func (sp *Spend) add(u store.Usage, r modeltable.Rates, priced bool) {
	sp.Input += u.Input
	sp.Output += u.Output
	sp.Reasoning += u.Reasoning
	sp.CacheRead += u.CacheRead
	sp.CacheCreation += u.CacheCreation
	switch {
	case priced:
		sp.CostPicoUSD += u.Input*r.Input + u.Output*r.Output + u.CacheRead*r.CacheRead + u.CacheCreation*r.CacheWrite
	case u != (store.Usage{}):
		sp.Unpriced = true
	}
}
