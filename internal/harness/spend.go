package harness

import (
	"fmt"
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
// A price can have a long-context tier (plan 047 D10): a request whose prompt
// — its input, cache reads and cache writes — is above the price's threshold
// is priced at the tier's rates, every token of it, and one at the threshold
// or below at the base rates. A tier is per request, so each request is
// classified when its usage arrives, before anything sums it (longOf), by
// the price that usage will be charged at:
//
//   - an assistant entry is one request, classified when the spend prices it,
//     by the price it records for the entry (prices) — nothing is persisted;
//   - a usage that sums requests — a subagent_usage row, a compaction entry,
//     a background child's carried usage, the unsaved usage of a step's rows
//     or of a compaction — carries its long-context subset beside it: the sum
//     of the usage of its long requests, classified one by one where they ran
//     (a child's observer, a compaction's attempts), persisted on a row
//     (store.ModelUsage.LongContext) and a compaction entry
//     (store.Compaction.LongContext), summed and subtracted with the totals.
//
// Such a usage is priced as two: its subset at the tier's rates and the rest
// at the base rates, each token counted once. A subset that is not inside its
// usage — a field above the usage's, or one below 0 — is not trusted: the
// whole usage is priced at the base rates, and the first report that counts a
// persisted one says so (DiagLongContextMalformed). A transcript written
// before the subset existed has none, and prices as it always did. A resumed
// session prices the subsets its transcript holds with the table it loaded,
// as it prices everything else (Replay): the same as the live session for as
// long as the configuration is the same.
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
//
// A background sub-agent's usage is used under the table it was opened with —
// the parent's when the call started it — and reaches an entry of the
// parent's only when its result is delivered, which can be after the parent
// has taken another table (plan 034 C4r2, r11 #5). So its rates travel with it:
// read when the child is opened, as the parent would have priced a usage of
// its model then (bgResult.price), and handed to the spend with the entry that
// commits the result (Session.carry), against the subagent_usage row its usage
// was merged into. A row's carried usages are priced at their own rates, and
// what is left of the row — a foreground child's usage in the same step, on
// the same model — at the row's price at the time of use, as before. A row is
// found by the names the entry wrote it under, which commit registers the
// carried usage by (usageNames, plan 034 C4r3, r13 #6): a key the session
// learned while the result waited is redacted from those names — one equal to
// a wire id, say — and the carried usage still finds its row, so it keeps its
// child's rates though the cut wire id prices nothing. Carried rates live in
// memory only, as every recorded price does: a resumed session prices its
// transcript's rows with the table it loaded (Replay).

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
	// long is the part of usage priced at the long-context tier (above).
	long store.Usage
	// price is m's rates as the session's table had them when the usage was
	// noted (the time of use, above).
	price priced
}

// noteUnsaved records usage the session was billed for in turn, on m, that no
// entry holds, so its spend still counts it, priced now (the time of use). A
// usage of nothing records nothing. long is u's long-context subset when u
// sums requests that were classified where they ran (a sub-agent row, a
// compaction's attempts), and nil when u is one request's, classified here by
// the price it is noted with.
func (s *Session) noteUnsaved(turn int, m store.Model, u store.Usage, long *store.Usage) {
	if u == (store.Usage{}) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p := pricer{table: s.table, opened: s.openTable}
	x := unsavedUsage{turn: turn, usage: u, price: p.lookup(m)}
	if long != nil {
		x.long = *long
	} else {
		x.long = x.price.longOf(u)
	}
	s.unsaved = append(s.unsaved, x)
}

// usageKey names one priced usage (Session.prices): the entry that holds it
// and the identity it is priced by — an entry's own usage and its sub-agent
// rows, one key per model among them, since one entry's usages of one
// identity were used under one table; all but a background child's delivered
// into a row, which carries its child's own (carry).
type usageKey struct {
	entry               string
	provider, wireModel string
}

// rowKey names one subagent_usage row of an entry: the entry, and the model
// the row names — provider, alias and wire model, the three mergeUsage keeps
// one row per — so two rows of one entry never share one.
type rowKey struct {
	entry                      string
	provider, alias, wireModel string
}

// carriedUsage is one background sub-agent's usage as its result was
// delivered, its long-context subset (classified by its observer, at price),
// and the price its child was opened under (bgResult.price).
type carriedUsage struct {
	usage store.Usage
	long  store.Usage
	price priced
}

// carriedRow is a carriedUsage and the row it was merged into, as commit
// hands it to carry.
type carriedRow struct {
	key   rowKey
	usage carriedUsage
}

// carry records background usages delivered into entries the store has just
// written, each with its child's price, for spend to price their rows by
// (above; plan 034 C4r2). commit calls it once the results are committed,
// with no lock of the runner's held; a usage of nothing is never handed here.
func (s *Session) carry(rows []carriedRow) {
	if len(rows) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.carried == nil {
		s.carried = make(map[rowKey][]carriedUsage)
	}
	for _, r := range rows {
		s.carried[r.key] = append(s.carried[r.key], r.usage)
	}
}

// priceAt is m's identity's rates as a usage of it is priced while table is
// the session's (the time of use, above): table's, or the open-time table's
// for an identity table does not price. openTable is fixed at Open.
func (s *Session) priceAt(table *modeltable.Table, m store.Model) priced {
	p := pricer{table: table, opened: s.openTable}
	return p.lookup(m)
}

// priceNow is m's identity's rates as a usage of it is priced now, under the
// session's current table: what a compaction classifies its attempts by.
func (s *Session) priceNow(m store.Model) priced {
	s.mu.Lock()
	table := s.table
	s.mu.Unlock()
	return s.priceAt(table, m)
}

// stepSpent is the Spent after turn's step d, which ran on m: the step
// first noted as observed and unsaved when no append wrote it — its usage on
// its model, and each of its sub-agents' rows on the row's model (the rows
// StepDone reports whether or not the step was saved, plan 026 §3.7) — then
// the session's spend as it now is, and the diags of what it distrusted
// (spent). The turn calls it under its lock (the turn's spent), after the
// step's StepDone. The step's own usage is one request, classified as it is
// noted; each row carries its subset.
func (s *Session) stepSpent(turn int, m model, d StepDone) (Spent, []Diag) {
	if !d.Saved {
		s.noteUnsaved(turn, store.Model{Provider: d.Provider, Alias: d.Model, WireModel: d.WireModel}, d.Usage, nil)
		for _, row := range d.SubagentUsage {
			s.noteUnsaved(turn, rowModel(row), row.Usage, &row.LongContext)
		}
	}
	return s.spent(turn, m.r)
}

// spent is the Spent the session reports now: turn's spend and the
// session's, and the context the next request sends to next's model — its
// size (§3.7) and that model's window. Its Seq is taken first, before
// anything is read (plan 040 §3.4; nextSeqLocked): turn and next are the
// caller's own — a turn's, or a Replay's read before it — so nothing the
// report is built from was read before it. The diags report each usage on
// the path whose long-context subset this report was the first to distrust
// (spend); the caller emits them after the Spent.
func (s *Session) spent(turn int, next modeltable.Resolved) (Spent, []Diag) {
	s.mu.Lock()
	seq := s.nextSeqLocked()
	s.mu.Unlock()
	return s.spentAt(seq, turn, next)
}

// spentAt is spent with its Seq already taken, by a caller that read turn and
// next in the same section of mu that took it (idleSpent).
func (s *Session) spentAt(seq uint64, turn int, next modeltable.Resolved) (Spent, []Diag) {
	tokens, _ := s.contextTokensOn(next)
	inTurn, session, diags := s.spend(turn)
	return Spent{ContextTokens: tokens, ContextWindow: int64(next.ContextWindow), Turn: inTurn, Session: session, Seq: seq}, diags
}

// nextSeqLocked numbers a report about to be built (Spent.Seq) and settles
// an owed one (spentOwed): everything noted before this section — an
// IdleSpent refused while a turn ran marked it after noting its usage — is
// read by this report, which reads after it. mu is held.
func (s *Session) nextSeqLocked() uint64 {
	s.spentSeq++
	s.spentOwed = false
	return s.spentSeq
}

// IdleSpent is the Spent the session reports now, when no turn and no Replay
// holds it (plan 040 §3.4): the last turn's spend, the session's — a side
// question's usage, which no turn's spend counts, among it — and the context
// the next request sends to the current model. ok is false for a sub-agent,
// which reports none (P33), and while a turn or a Replay runs: the report is
// then owed (spentOwed) — the turn's next report counts whatever was noted
// meanwhile, and when it has none left to make, OwedSpent makes this one once
// it has ended.
//
// It reads running, the turn count and the current model and takes the
// report's Seq in one section of mu, and computes the report with mu released
// (spentAt takes the store's lock, then mu): a turn that begins after that
// section takes every Seq of its own after this one, so a consumer that
// applies only a Seq above the last it applied drops this report if that
// turn's own lands first.
func (s *Session) IdleSpent() (Spent, bool) { return s.idleSpent(false) }

// OwedSpent is IdleSpent when one is owed, and only then (plan 040 §3.4): an
// IdleSpent was refused while a turn or a Replay ran, and no report has been
// numbered since — so whatever its caller noted may be in none. It is false
// while a turn or a Replay runs, and the debt stands; true at most once per
// debt. The native adapter asks it when a turn or a wake has ended, after its
// ending.
func (s *Session) OwedSpent() (Spent, bool) { return s.idleSpent(true) }

// idleSpent is IdleSpent, or OwedSpent when owed.
func (s *Session) idleSpent(owed bool) (Spent, bool) {
	if s.child {
		return Spent{}, false
	}
	s.mu.Lock()
	switch {
	case s.running:
		if !owed {
			s.spentOwed = true
		}
		s.mu.Unlock()
		return Spent{}, false
	case owed && !s.spentOwed:
		s.mu.Unlock()
		return Spent{}, false
	}
	seq := s.nextSeqLocked()
	turns, next := s.turns, s.cur.r
	s.mu.Unlock()
	if seam := s.idleSpentRead; seam != nil {
		seam()
	}
	sp, diags := s.spentAt(seq, turns, next)
	// No turn's sink is there to take a diag: the session's own is (Options.Sink).
	for _, d := range diags {
		s.subs.emit(d)
	}
	return sp, true
}

// spend is turn's spend and the session's, by the rule above: every usage on
// the transcript's path, and every one this incarnation observed unsaved,
// each at its price at the time of use — the one recorded when it was first
// counted, or, for a usage counted now for the first time, the session's
// table's (the open-time table's for an identity it does not price),
// recorded here — its long-context subset at the price's tier rates. It
// takes the store's lock and then the session's, one at a time: the walk
// holds neither, and under the session's only the prices not yet recorded
// are read, a few identities per call. diags are the usages on the path
// whose subset this call was the first to find malformed (distrusted).
func (s *Session) spend(turn int) (inTurn, session Spend, diags []Diag) {
	type use struct {
		key   usageKey
		where rowKey // the usage's entry and model, as a row would name it
		row   bool   // a subagent_usage row's, not an entry's own usage
		usage store.Usage
		// long is the persisted long-context subset of a usage that sums
		// requests (a row, a compaction); nil for an assistant entry's, one
		// request, classified below by its recorded price.
		long   *store.Usage
		inTurn bool
	}
	tr := s.store.Transcript()
	path, _ := tr.Branch(tr.Leaf()) // the leaf is always known
	var uses []use
	var turns turnReader
	for i := range path {
		e := &path[i]
		turns.read(e)
		entryUsage(e, func(m store.Model, u store.Usage, long *store.Usage, row bool) {
			x := use{
				key:   usageKey{entry: e.ID, provider: m.Provider, wireModel: m.WireModel},
				where: rowKey{entry: e.ID, provider: m.Provider, alias: m.Alias, wireModel: m.WireModel},
				row:   row, usage: u, inTurn: turns.turn == turn,
			}
			if long != nil {
				l := *long // a copy: the walk keeps nothing of the transcript's
				x.long = &l
			}
			uses = append(uses, x)
		})
	}

	// A usage whose subset is malformed is priced at base (above); each
	// persisted one is reported once, by the first call that meets it.
	// An empty subset is never malformed: a row or an entry with no long
	// request, or written before subsets existed, prices as it always did.
	malformed := make([]bool, len(uses))
	for i, u := range uses {
		malformed[i] = u.long != nil && *u.long != (store.Usage{}) && !within(u.usage, *u.long)
	}

	s.mu.Lock()
	p := pricer{table: s.table, opened: s.openTable}
	prices := make([]priced, len(uses))
	carried := make([][]carriedUsage, len(uses))
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
		if u.row {
			carried[i] = slices.Clone(s.carried[u.where])
		}
		if malformed[i] && !s.distrusted[u.where] {
			if s.distrusted == nil {
				s.distrusted = make(map[rowKey]bool)
			}
			s.distrusted[u.where] = true
			diags = append(diags, malformedDiag(u.where, u.row, u.usage, *u.long))
		}
	}
	unsaved := slices.Clone(s.unsaved)
	s.mu.Unlock()

	for i, u := range uses {
		// The usage's long-context subset: its own when it carries one, an
		// assistant entry's classified by its price, and none when it cannot
		// be trusted — the whole usage at base, carried parts included.
		var long store.Usage
		switch {
		case malformed[i]:
		case u.long != nil:
			long = *u.long
		default:
			long = prices[i].longOf(u.usage)
		}
		// A row's carried usages at their children's rates, and the rest of
		// the row at its own price (above). Carried usages are the row's by
		// construction (commit hands carry what the entry's rows were merged
		// from); ones that would not fit inside it — their totals, or their
		// subsets inside its subset, leaving a rest whose subset is inside it
		// — are not trusted, and the whole row is priced as one.
		rest, restLong := u.usage, long
		parts := carried[i]
		if !holds(rest, restLong, parts, malformed[i]) {
			parts = nil
		}
		for _, c := range parts {
			cl := c.long
			if malformed[i] {
				cl = store.Usage{}
			}
			session.add(c.usage, cl, c.price)
			if u.inTurn {
				inTurn.add(c.usage, cl, c.price)
			}
			rest, restLong = subUsage(rest, c.usage), subUsage(restLong, cl)
		}
		session.add(rest, restLong, prices[i])
		if u.inTurn {
			inTurn.add(rest, restLong, prices[i])
		}
	}
	for _, u := range unsaved {
		// Built in memory, inside its usage by construction: one that is
		// not is priced at base all the same.
		long := u.long
		if !within(u.usage, long) {
			long = store.Usage{}
		}
		session.add(u.usage, long, u.price)
		if u.turn == turn {
			inTurn.add(u.usage, long, u.price)
		}
	}
	return inTurn, session, diags
}

// malformedDiag is the DiagLongContextMalformed of the usage at where: an
// entry's subagent_usage row, or a compaction entry's usage (row false).
func malformedDiag(where rowKey, row bool, u, long store.Usage) Diag {
	kind := "compaction"
	if row {
		kind = "subagent_usage"
	}
	return Diag{Kind: DiagLongContextMalformed, Fields: map[string]string{
		"entry": where.entry, "usage_of": kind,
		"provider": where.provider, "model": where.alias, "wire_model": where.wireModel,
		"usage":        usageFields(u),
		"long_context": usageFields(long),
	}}
}

// usageFields is u as a diag field states a usage.
func usageFields(u store.Usage) string {
	return fmt.Sprintf("input=%d output=%d reasoning=%d cache_read=%d cache_creation=%d",
		u.Input, u.Output, u.Reasoning, u.CacheRead, u.CacheCreation)
}

// entryUsage hands visit each usage e holds by the rule, with the model that
// prices it, its long-context subset when it sums requests, and whether it is
// a subagent_usage row: an assistant entry's own usage on its model, one
// request with no subset (nil); each subagent_usage row of a message entry of
// any role on the row's model, with the row's subset; a compaction entry's
// usage — every attempt's — on the model its summarizer ran on, with the
// entry's subset. A tool entry carries no usage of its own (only rows), a user
// entry none, and a change, a resume, a reminder or a newer craze's entry
// nothing.
func entryUsage(e *store.Entry, visit func(m store.Model, u store.Usage, long *store.Usage, row bool)) {
	switch e.Type {
	case store.TypeCompaction:
		if e.Usage != nil {
			visit(e.Model, *e.Usage, &e.Compaction.LongContext, false)
		}
	case store.TypeMessage:
		if e.Message.Role == fantasy.MessageRoleAssistant && e.Usage != nil {
			visit(e.Model, *e.Usage, nil, false)
		}
		for i := range e.SubagentUsage {
			row := &e.SubagentUsage[i]
			visit(rowModel(*row), row.Usage, &row.LongContext, true)
		}
	}
}

// holds reports whether parts, summed, fit inside row and its subset rowLong
// field by field — what a row's carried usages must do, being what it was
// merged from — each part's subset inside its own usage, and leaving a rest
// whose subset is inside it. With malformed set the subsets are not used
// (spend prices the row at base), and only the totals must fit.
func holds(row, rowLong store.Usage, parts []carriedUsage, malformed bool) bool {
	if malformed {
		rowLong = store.Usage{}
	}
	for _, c := range parts {
		long := c.long
		if malformed {
			long = store.Usage{}
		} else if !within(c.usage, long) {
			return false
		}
		row, rowLong = subUsage(row, c.usage), subUsage(rowLong, long)
		if negative(row) || negative(rowLong) {
			return false
		}
	}
	return within(row, rowLong)
}

// within reports whether long is a subset of u: no field of it below 0 or
// above u's. What every long-context subset must be to be trusted.
func within(u, long store.Usage) bool {
	return !negative(long) && long.Input <= u.Input && long.Output <= u.Output &&
		long.Reasoning <= u.Reasoning && long.CacheRead <= u.CacheRead && long.CacheCreation <= u.CacheCreation
}

// negative reports whether any field of u is below 0.
func negative(u store.Usage) bool {
	return u.Input < 0 || u.Output < 0 || u.Reasoning < 0 || u.CacheRead < 0 || u.CacheCreation < 0
}

// subUsage is a less b, field by field.
func subUsage(a, b store.Usage) store.Usage {
	return store.Usage{
		Input:         a.Input - b.Input,
		Output:        a.Output - b.Output,
		Reasoning:     a.Reasoning - b.Reasoning,
		CacheRead:     a.CacheRead - b.CacheRead,
		CacheCreation: a.CacheCreation - b.CacheCreation,
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

// longOf is the long-context subset of u, the usage of one request (plan 047
// D10): all of it when p's tier prices its prompt — its input, cache reads
// and cache writes, every token it sent — as long, and none otherwise, so a
// prompt of exactly the threshold is base and one token more is long. With
// no price, or no tier, nothing is long.
func (p priced) longOf(u store.Usage) store.Usage {
	if p.ok && p.rates.LongPrompt(u.Input+u.CacheRead+u.CacheCreation) {
		return u
	}
	return store.Usage{}
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

// add adds u to sp: its tokens always, once, and, when p prices it, its cost
// — input, output (reasoning inside it, never priced again), cache reads and
// cache writes each at its rate: long, u's long-context subset, at p's tier
// rates, and the rest of u at its base rates (plan 047 D10). The caller hands
// a subset inside u (within) or none. A usage with no price and something in
// it marks sp Unpriced.
func (sp *Spend) add(u, long store.Usage, p priced) {
	sp.Input += u.Input
	sp.Output += u.Output
	sp.Reasoning += u.Reasoning
	sp.CacheRead += u.CacheRead
	sp.CacheCreation += u.CacheCreation
	switch {
	case p.ok:
		sp.CostPicoUSD += cost(subUsage(u, long), p.rates.Base()) + cost(long, p.rates.Long())
	case u != (store.Usage{}):
		sp.Unpriced = true
	}
}

// cost is u at r: input, output (reasoning inside it), cache reads and cache
// writes, each at its rate, in picodollars.
func cost(u store.Usage, r modeltable.TierRates) int64 {
	return u.Input*r.Input + u.Output*r.Output + u.CacheRead*r.CacheRead + u.CacheCreation*r.CacheWrite
}
