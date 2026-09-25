package harness

import (
	"charm.land/fantasy"
	"github.com/charliek/craze/internal/harness/store"
)

// A turn's side of delivering background sub-agents' results (plan 026
// §3.11; background.go has the runner's). At each step boundary the turn takes
// up every result waiting to be delivered as one user part — a collection of
// its own, t.internal, never the steer box's: whatever the steer box has not
// written goes back to the user (Unanswered) and from there to the engine's
// queue, where a result must never go (astra r14, P21) — spliced after the
// step's input and re-inserted at the same index into every later request.
// The step's append writes it as its own user entry among the leading ones,
// marked as results (store.MessageEntry.SubagentResults) and carrying the
// results' usage, and the reservations it holds commit by the store id of
// that entry. What no append writes is given back when the turn ends
// (restoreTurn, in run).

// internalPart is one part of background results a step took up: the user
// message it is — the results' text, byte for byte what the store writes — the
// index it is re-inserted at in every later step's input, the results it
// holds, their usage, a row per model, and whether an append has written it.
type internalPart struct {
	at      int
	msg     fantasy.Message
	ids     []string
	usage   []store.ModelUsage
	written bool
}

// takeResults is prepareStep's taking up of background results before step n
// (from 0), whose input is base: every pending one, and at the first step of a
// turn a person started — not a wake's — every suspended one too, reserved for
// this step (its number from 1, t.step's), and made one user part (§3.11). mu
// is held; the runner's regMu is taken under it, never the other way round.
func (t *turn) takeResults(n int, base []fantasy.Message) {
	if t.subs == nil {
		return
	}
	b := t.subs.reserve(owner{turn: t.number, step: n + 1, wake: t.wake}, n == 0 && !t.wake)
	if b == nil {
		return
	}
	t.internal = append(t.internal, internalPart{at: len(base), msg: fantasy.NewUserMessage(b.text), ids: b.ids, usage: b.rows})
}

// entry is p as the store takes it: a user entry marked as results, carrying
// their usage, stamped with the turn's model as every entry of the turn is.
func (t *turn) entry(p *internalPart) store.MessageEntry {
	return store.MessageEntry{Message: p.msg, Model: t.model.id(), Effort: t.model.effort,
		SubagentUsage: p.usage, SubagentResults: true}
}

// leadingEntries are the leading entries of a finished step's append: the
// steers and the parts of results no append has written yet, in the order the
// step's request had them — by index, and at a shared index the steers first
// (spliceInto) — each entry's part beside it (nil for a steer). With no part
// of results they are steerEntries, entry for entry. mu is held.
func (t *turn) leadingEntries() ([]store.MessageEntry, []*internalPart) {
	steers, parts := t.steerEntries(), t.unwrittenParts()
	if len(parts) == 0 {
		return steers, nil
	}
	pending := t.unwritten()
	entries := make([]store.MessageEntry, 0, len(steers)+len(parts))
	lead := make([]*internalPart, 0, len(steers)+len(parts))
	i, j := 0, 0
	for i < len(pending) || j < len(parts) {
		if j == len(parts) || (i < len(pending) && pending[i].at <= parts[j].at) {
			entries, lead = append(entries, steers[i]), append(lead, nil)
			i++
			continue
		}
		entries, lead = append(entries, t.entry(parts[j])), append(lead, parts[j])
		j++
	}
	return entries, lead
}

// internalEntries are the parts of results no append has written yet, as the
// leading entries of a save that carries no steer: the partial answer a
// cancel or a failure cut short, and the step the runner synthesizes — whose
// unwritten steers go back to the user, as they always have. mu is held.
func (t *turn) internalEntries() ([]store.MessageEntry, []*internalPart) {
	parts := t.unwrittenParts()
	if len(parts) == 0 {
		return nil, nil
	}
	entries := make([]store.MessageEntry, len(parts))
	for i, p := range parts {
		entries[i] = t.entry(p)
	}
	return entries, parts
}

// unwrittenParts are the parts of results no append has written, in order.
// mu is held.
func (t *turn) unwrittenParts() []*internalPart {
	var parts []*internalPart
	for i := range t.internal {
		if !t.internal[i].written {
			parts = append(parts, &t.internal[i])
		}
	}
	return parts
}

// wrote commits what a successful append wrote, by the store ids it returned
// (plan 026 §3.11, astra r14's mapping). The store returns an id for every
// entry it wrote, in file order: held changes and held user entries first,
// then the leading entries, the answer, and the tool entry when there is one.
// So with lead the leading entries' parts (nil for a steer) and withTool
// saying a tool entry was written, the leading entries' ids are the len(lead)
// just before the answer's; one part commits every result it holds, by its
// entry's id. The user entry of a wake — the results the wake was started
// with — is the held user entry written just ahead of them by the turn's
// first append (Run discards any other held user first), and commits its
// results once. And an agent_output call commits its reservation only when
// its own result is in the tool entry written — outputs are those calls'
// harness ids (outputCalls) — by that entry's id. An append that failed, or
// had nothing to write, commits nothing: it never gets here. mu is held.
func (t *turn) wrote(ids []string, lead []*internalPart, withTool bool, outputs []string) {
	tail := 1 // the answer
	if withTool {
		tail++
	}
	first := len(ids) - tail - len(lead) // the first leading entry's
	if first < 0 {
		return // cannot happen: the store wrote every entry it was handed
	}
	for i, p := range lead {
		if p == nil {
			continue
		}
		p.written = true
		t.subs.commitIDs(t.number, ids[first+i], p.ids)
	}
	if len(t.held) > 0 && !t.heldWritten && first >= 1 {
		t.heldWritten = true
		t.subs.commitIDs(t.number, ids[first-1], t.held)
	}
	if withTool && len(outputs) > 0 {
		t.subs.commitCalls(t.number, ids[len(ids)-1], outputs)
	}
}
