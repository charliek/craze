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
	model store.Model
	usage store.Usage
}

// noteUnsaved records usage the session was billed for in turn, on m, that no
// entry holds, so its spend still counts it. A usage of nothing records
// nothing.
func (s *Session) noteUnsaved(turn int, m store.Model, u store.Usage) {
	if u == (store.Usage{}) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.unsaved = append(s.unsaved, unsavedUsage{turn: turn, model: m, usage: u})
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
	tokens, _ := s.contextTokensOn(idOf(next))
	inTurn, session := s.spend(turn)
	return Spent{ContextTokens: tokens, ContextWindow: int64(next.ContextWindow), Turn: inTurn, Session: session}
}

// spend is turn's spend and the session's, by the rule above: every usage on
// the transcript's path, and every one this incarnation observed unsaved,
// priced through the session's table. It takes the session's lock and the
// store's, one at a time, and holds neither while it prices.
func (s *Session) spend(turn int) (inTurn, session Spend) {
	s.mu.Lock()
	p := pricer{table: s.table}
	unsaved := slices.Clone(s.unsaved)
	s.mu.Unlock()

	tr := s.store.Transcript()
	path, _ := tr.Branch(tr.Leaf()) // the leaf is always known
	var turns turnReader
	for i := range path {
		e := &path[i]
		turns.read(e)
		entryUsage(e, func(m store.Model, u store.Usage) {
			rates, ok := p.price(m)
			session.add(u, rates, ok)
			if turns.turn == turn {
				inTurn.add(u, rates, ok)
			}
		})
	}
	for _, u := range unsaved {
		rates, ok := p.price(u.model)
		session.add(u.usage, rates, ok)
		if u.turn == turn {
			inTurn.add(u.usage, rates, ok)
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
// every call (X42), and one Spent may price many records of a few models.
type pricer struct {
	table *modeltable.Table
	seen  map[[2]string]priced
}

// priced is one identity's lookup: its rates, and whether it has any.
type priced struct {
	rates modeltable.Rates
	ok    bool
}

// price is m's identity's rates, and whether the table prices it.
func (p *pricer) price(m store.Model) (modeltable.Rates, bool) {
	key := [2]string{m.Provider, m.WireModel}
	if pr, ok := p.seen[key]; ok {
		return pr.rates, pr.ok
	}
	var pr priced
	pr.rates, pr.ok = p.table.Price(m.Provider, m.WireModel)
	if p.seen == nil {
		p.seen = make(map[[2]string]priced)
	}
	p.seen[key] = pr
	return pr.rates, pr.ok
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
