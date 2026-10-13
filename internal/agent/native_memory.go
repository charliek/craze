package agent

import (
	"slices"

	"github.com/charliek/craze/internal/harness/modeltable"
)

// The native session's own model memory (plan 047 D1, D2): the switches and
// effort changes it confirmed, which a switch lands on before anything
// recent.json says and which its list is ranked by before their writes land.
//
// Each confirmation names its model as recent.json does — by alias, and by
// identity, provider and wire model, in the table the session held when it
// confirmed it — so a table taken up later judges it by the file's own rule
// (modeltable.Table.RecentAlias, remapConfirmed): a model a release renamed
// keeps it under its new alias; an alias re-pointed at another model, or a
// model gone, drops it (C1 review F1). And each carries its number in the
// order the session's confirmations took, which their writes land in
// (remember).
//
// A reload ranks the list by the file alone — but for the one a switch makes
// in its own section (reloadLeft), which ranks that switch first too. A
// confirmation whose write failed, or is still pending when a reload reads
// the file, is then ranked as the file ranks it until the next switch — the
// rank alone lags: its switch effort is still the session's (memoryView over
// the remapped confirmations), and a failed write has its note.

// confirmed is one switch or effort change a native session confirmed: the
// model and the effort as recent.json would record them (entry; At is not
// set), and the confirmation's number (seq, the session's memSeq).
type confirmed struct {
	entry modeltable.RecentEntry
	seq   uint64
}

// withConfirmed is confs — newest first, one per alias — with c its newest:
// any older confirmation of c's alias goes. confs is not written to.
func withConfirmed(confs []confirmed, c confirmed) []confirmed {
	out := make([]confirmed, 0, len(confs)+1)
	out = append(out, c)
	for _, o := range confs {
		if o.entry.Alias != c.entry.Alias {
			out = append(out, o)
		}
	}
	return out
}

// confirmedEffort is the effort confs hold for alias — the session's own
// last choice on it — and whether they hold one.
func confirmedEffort(confs []confirmed, alias string) (string, bool) {
	for _, c := range confs {
		if c.entry.Alias == alias {
			return c.entry.Effort, true
		}
	}
	return "", false
}

// memoryView is confs as modeltable's memory view: alias → the effort
// confirmed on it (modeltable.Table.SwitchEffort); nil for none.
func memoryView(confs []confirmed) map[string]string {
	if len(confs) == 0 {
		return nil
	}
	out := make(map[string]string, len(confs))
	for _, c := range confs {
		if _, ok := out[c.entry.Alias]; !ok {
			out[c.entry.Alias] = c.entry.Effort
		}
	}
	return out
}

// remapConfirmed is confs as t names their models, newest first: each under
// the alias its identity has in t, by modeltable.Table.RecentAlias — the
// rule Table.Recent judges recent.json's entries by — so a rename keeps a
// confirmation under the model's new alias, and an alias re-pointed at
// another model, or a model t no longer has, drops it. Two that come to name
// one alias keep the newer. confs is not written to.
func remapConfirmed(t *modeltable.Table, confs []confirmed) []confirmed {
	var out []confirmed
	for _, c := range confs {
		alias, ok := t.RecentAlias(c.entry)
		if !ok || slices.ContainsFunc(out, func(o confirmed) bool { return o.entry.Alias == alias }) {
			continue
		}
		c.entry.Alias = alias
		out = append(out, c)
	}
	return out
}
