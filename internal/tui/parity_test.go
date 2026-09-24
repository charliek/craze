package tui

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"weak"

	"charm.land/fantasy"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/transcript"
)

// The parity watch (plan 024 A11) is the net under the TUI's adoption of the
// shared model. TestMain installs it for the whole package (installParityWatch),
// so every fold any test in internal/tui causes — the frame goldens' scripts
// and every other RunFrameScript, the scripted and pumped sessions, the live
// fake-agent ones, the direct-Update fixtures — is checked against a model of
// its own, folded from exactly the events the TUI folded:
//
//   - P0: a shadow model folded from the recorded events alone, fold by fold —
//     handed the instant and the error text the TUI's fold was handed —
//     returns the same Change for every event and is at the same Seq, and
//     holds the same History and State as m.shared; and at every power-of-two
//     fold a model folded from scratch from the whole recorded prefix holds
//     them too;
//   - P1: every shared row of every pane shows its entry as the model holds it
//     now — no stale row, and no row for an entry the model has let go;
//   - P2: every pane holds exactly the rows the watch's own account says it
//     must, row by row (paneWant): the rows of the entries each fold appended,
//     less the echoes it hid; the rows re-shown by the rules for a touch with
//     no row (X29, X30); the local rows written between folds; less exactly
//     the rows the model's trims (X28) and the pane's own caps take — not one
//     more; and every row of a child the model evicted let go (detach);
//   - P3: a pane's shared rows are in the model's order, re-appended ones
//     (X29) excepted;
//   - P4: a local row carries no entry and a row that carries one is not local,
//     so no local row is in the model — which P0's model, folded from events
//     alone, holds exactly.
//
// The Change, the Seq and P2 are checked at every fold. The whole of P0, P1,
// P3 and P4 is checked at every fold of the first 64, at every power of two and
// every 256th, and at every fold while the session is small (parityFullSize) —
// which is every fold of nearly every test; the few that fill a pane to its
// cap stay linear. strict checks everything at every fold.
//
// A broken rule panics in the Update that broke it, which fails that test (a
// frame script's program reports the panic as its error), and is kept, so
// TestMain fails the run even if a test swallowed the panic.

// parityFullSize is the size — entries plus rows, over every transcript and
// pane — up to which every fold gets the whole check.
const parityFullSize = 512

type parityWatch struct {
	mu   sync.Mutex
	recs map[weak.Pointer[transcript.Model]]*parityRec
	// strict checks everything at every fold, whatever the size.
	strict bool
	failed error
	stats  parityStats
}

// parityStats is what the watch has checked, for its report.
type parityStats struct {
	models, folds, full, fresh, rows int
	// sampled counts the folds the whole check was not due at (wholeDue).
	sampled int
}

// paritySample is one fold as the TUI made it: the event, the instant the
// fold's clock read if it read one, and the error text it was handed if the
// event carries an error (foldInputs).
type paritySample struct {
	ev      agent.Event
	at      time.Time
	errText string
}

// parityRec is one shared model's record: its shadow, the events, and what
// each of the TUI's panes must hold.
type parityRec struct {
	shadow *transcript.Model
	// replay is what the shadow's fold is handed: the sample the TUI's fold
	// was handed for the same event.
	replay paritySample
	events []paritySample
	folds  int
	panes  map[*pane]*paneWant
}

// replayOptions is sharedOptions over a sample the caller keeps current: a
// model folded with it is handed, for each event, what the TUI's fold was.
func replayOptions(s *paritySample) transcript.Options {
	return sharedOptions(func() time.Time { return s.at }, func(error) string { return s.errText })
}

var parity = &parityWatch{recs: map[weak.Pointer[transcript.Model]]*parityRec{}}

// installParityWatch hooks the watch into every fold (TestMain). With
// CRAZE_PARITY_STRICT set, every fold of every test gets the whole check.
func installParityWatch() {
	parity.strict = os.Getenv("CRAZE_PARITY_STRICT") != ""
	foldHook = parity.hook
}

// err is the first rule the watch saw broken, if any.
func (w *parityWatch) err() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.failed
}

// snapshot is the watch's totals so far.
func (w *parityWatch) snapshot() parityStats {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.stats
}

// setStrict switches the whole check at every fold on or off, and says what it
// was.
func (w *parityWatch) setStrict(on bool) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	was := w.strict
	w.strict = on
	return was
}

// report writes the watch's totals to stderr when CRAZE_PARITY_STATS is set.
func (w *parityWatch) report() {
	if os.Getenv("CRAZE_PARITY_STATS") == "" {
		return
	}
	s := w.snapshot()
	fmt.Fprintf(os.Stderr, "parity: %d shared models, %d folds, %d whole checks (%d rows), %d from-scratch folds, %d folds sampled out\n",
		s.models, s.folds, s.full, s.rows, s.fresh, s.sampled)
}

// fail records a broken rule and panics with it, in the Update that broke it.
// The caller holds mu.
func (w *parityWatch) fail(format string, args ...any) {
	err := fmt.Errorf("parity (plan 024 A11): "+format, args...)
	if w.failed == nil {
		w.failed = err
	}
	panic(err)
}

// rec is the model's record. A model is only ever folded through the hook,
// so a model the watch has no record of is folding its first event; the
// record goes when the model does.
func (w *parityWatch) rec(m *transcript.Model) *parityRec {
	key := weak.Make(m)
	if r := w.recs[key]; r != nil {
		return r
	}
	r := &parityRec{panes: map[*pane]*paneWant{}}
	r.shadow = transcript.New(replayOptions(&r.replay))
	w.recs[key] = r
	w.stats.models++
	runtime.AddCleanup(m, func(k weak.Pointer[transcript.Model]) {
		w.mu.Lock()
		delete(w.recs, k)
		w.mu.Unlock()
	}, key)
	return r
}

// panesOf is every pane the Model holds, by scope ("" is main).
func panesOf(m *Model) map[string]*pane {
	out := map[string]*pane{}
	if m.main != nil {
		out[""] = m.main
	}
	for id, p := range m.subs {
		if p != nil {
			out[id] = p
		}
	}
	return out
}

// ---------------------------------------------------------------- paneWant

// paneWant is the watch's own account of one pane (P2): the rows it must hold,
// in order and by identity; the entry each shared row must show; which rows
// were re-appended out of the model's order (X29); and the clear mark, which
// the watch takes itself, from the shadow, when it sees the pane emptied.
type paneWant struct {
	emptied int
	rows    []*entry
	shows   map[*entry]transcript.EntryID
	byID    map[transcript.EntryID]*entry
	re      map[*entry]bool
	mark    transcript.EntryID
	from    int
}

// newPaneWant is the account of a pane as it stands when the watch first sees
// it, or sees it emptied: whatever it holds by then was written outside any
// fold, so it is all local.
func newPaneWant(p *pane) (*paneWant, error) {
	w := &paneWant{
		emptied: p.emptied,
		rows:    slices.Clone(p.rows),
		shows:   map[*entry]transcript.EntryID{},
		byID:    map[transcript.EntryID]*entry{},
		re:      map[*entry]bool{},
	}
	for _, r := range p.rows {
		if !r.local || !r.id.IsZero() {
			return w, fmt.Errorf("a row no fold drew names entry %v (local=%v): %+v", r.id, r.local, shownOf(r))
		}
	}
	return w, nil
}

// paneCap is the pane's row cap.
func paneCap(p *pane) int {
	if p.entryCap > 0 {
		return p.entryCap
	}
	return maxEntries
}

func rowsText(rows []*entry, text func(*entry) int) int {
	n := 0
	for _, r := range rows {
		n += text(r)
	}
	return n
}

func textOf(r *entry) int { return len(r.text) }

// between is the account of the rows p gained and lost since the watch last
// saw it, with no fold in between: only local rows can have been written, at
// the tail, and only the caps can have taken rows, off the front and only as
// many as those appends forced — the exact minimum (a pane at its row cap
// after it dropped anything; over its text budget with the last row it
// dropped put back).
func (w *paneWant) between(p *pane) error {
	got, base := p.rows, w.rows
	d := 0
	if len(got) > 0 {
		for d < len(base) && base[d] != got[0] {
			d++
		}
	} else {
		d = len(base)
	}
	kept := len(base) - d
	if kept > len(got) || !slices.Equal(got[:kept], base[d:]) {
		return fmt.Errorf("between folds the pane's rows are not its last rows less a front prefix, then new local rows")
	}
	fresh := got[kept:]
	for _, r := range fresh {
		if !r.local || !r.id.IsZero() {
			return fmt.Errorf("between folds a row naming entry %v (local=%v) was written: %+v", r.id, r.local, shownOf(r))
		}
	}
	if d > 0 {
		if len(fresh) == 0 {
			return fmt.Errorf("between folds %d rows left the pane and none was written", d)
		}
		forced := len(got) == paneCap(p)
		if p.textBudget > 0 && !forced {
			// Put the last row dropped back: the budget must not hold then.
			// (Exact while no row written since was dropped as well, which
			// takes more local text between two folds than a budget holds; a
			// sub-agent's pane gains local rows only from a receipt rebuild,
			// which empties it and starts the account over.)
			forced = rowsText(got, textOf)+textOf(base[d-1]) > p.textBudget
		}
		if !forced {
			return fmt.Errorf("between folds %d rows left the pane off its front, and no cap forced the last of them", d)
		}
	}
	if err := capsHold(p, got, textOf); err != nil {
		return err
	}
	w.forget(base[:d])
	w.rows = slices.Clone(got)
	return nil
}

// capsHold reports a pane over its caps.
func capsHold(p *pane, rows []*entry, text func(*entry) int) error {
	if len(rows) > paneCap(p) {
		return fmt.Errorf("the pane holds %d rows, over its cap of %d", len(rows), paneCap(p))
	}
	if p.textBudget > 0 && len(rows) > 1 && rowsText(rows, text) > p.textBudget {
		return fmt.Errorf("the pane holds %d bytes of row text, over its budget of %d", rowsText(rows, text), p.textBudget)
	}
	return nil
}

// forget takes rows that left the pane out of the account's indexes.
func (w *paneWant) forget(rows []*entry) {
	for _, r := range rows {
		if id, ok := w.shows[r]; ok {
			if w.byID[id] == r {
				delete(w.byID, id)
			}
			delete(w.shows, r)
		}
		delete(w.re, r)
	}
}

// predicted is one row a fold's consumption must add at the tail: the entry
// it shows, whether it was re-appended out of the model's order (X29), and a
// scratch copy of what it must display, which its text is weighed by.
type predicted struct {
	id      transcript.EntryID
	re      bool
	scratch *entry
}

// consumed is the account of one fold's consumption by p — tr is the shadow's
// transcript of p's scope, hide the kind the fold hid — predicted from the
// watch's own rules and compared with p's rows exactly, row by row:
//
//   - the model's trims (X28): the longest front prefix whose shared rows the
//     model no longer holds, local rows among them, then any re-appended row
//     whose entry went;
//   - a touch with no row: a tool is re-appended (X29), the run open at the
//     clear continues in a row of its own (X30), anything else draws nothing;
//   - the appended entries, less those of kind hide;
//   - and, if any row was added, the caps over the result: rows off the front
//     to the row cap, then to the text budget.
func (w *paneWant) consumed(p *pane, tr *transcript.Transcript, ch transcript.Change, hide transcript.Kind) error {
	held := func(id transcript.EntryID) bool { _, ok := tr.Entry(id); return ok }
	list := slices.Clone(w.rows)
	var gone []*entry
	if ch.Dropped > 0 {
		k := -1
		for i, r := range list {
			id := w.shows[r]
			if id.IsZero() {
				continue
			}
			if held(id) {
				break
			}
			k = i
		}
		gone = append(gone, list[:k+1]...)
		list = list[k+1:]
		list = slices.DeleteFunc(list, func(r *entry) bool {
			if w.re[r] && !held(w.shows[r]) {
				gone = append(gone, r)
				return true
			}
			return false
		})
	}
	var adds []predicted
	for _, id := range ch.Touched {
		if id.IsZero() {
			continue
		}
		e, ok := tr.Entry(id)
		if !ok || w.byID[id] != nil {
			continue
		}
		switch {
		case e.Kind == transcript.KindTool:
			adds = append(adds, predicted{id: id, re: true, scratch: &entry{}})
		case id == w.mark && e.Streaming:
			adds = append(adds, predicted{id: id, scratch: &entry{cont: true, contFrom: w.from, at: e.End}})
		}
		if id == w.mark {
			w.mark, w.from = transcript.EntryID{}, 0
		}
	}
	if !ch.AppendedFrom.IsZero() {
		for _, e := range tr.Range(ch.AppendedFrom, ch.AppendedTo) {
			if hide != 0 && e.Kind == hide {
				continue
			}
			adds = append(adds, predicted{id: e.ID, scratch: &entry{}})
		}
	}
	scratch := map[*entry]predicted{}
	seq := list
	for _, a := range adds {
		e, _ := tr.Entry(a.id)
		a.scratch.show(e, tr)
		scratch[a.scratch] = a
		seq = append(seq, a.scratch)
	}
	if len(adds) > 0 {
		if n := len(seq) - paneCap(p); n > 0 {
			gone = append(gone, seq[:n]...)
			seq = seq[n:]
		}
		for p.textBudget > 0 && len(seq) > 1 && rowsText(seq, textOf) > p.textBudget {
			gone = append(gone, seq[0])
			seq = seq[1:]
		}
	}
	// seq is the pane now, with each row this fold added still its scratch.
	if len(p.rows) != len(seq) {
		return fmt.Errorf("the pane holds %d rows, and its account %d (%d added, %d gone)", len(p.rows), len(seq), len(adds), len(gone))
	}
	for i, r := range seq {
		got := p.rows[i]
		a, isNew := scratch[r]
		switch {
		case !isNew && got != r:
			return fmt.Errorf("row %d of the pane is not the row its account holds there", i)
		case isNew && (slices.Contains(w.rows, got) || got.id != a.id || got.local):
			return fmt.Errorf("row %d of the pane should be a new row for entry %v, and is %+v", i, a.id, shownOf(got))
		}
	}
	w.forget(gone)
	for i, r := range seq {
		if a, isNew := scratch[r]; isNew {
			got := p.rows[i]
			w.shows[got] = a.id
			w.byID[a.id] = got
			if a.re {
				w.re[got] = true
			}
		}
	}
	w.rows = slices.Clone(p.rows)
	return nil
}

// detached is the account of a pane whose child the model evicted: its rows
// stay, and every one is local now.
func (w *paneWant) detached(p *pane) error {
	if !slices.Equal(p.rows, w.rows) {
		return fmt.Errorf("the pane of an evicted child lost or gained rows")
	}
	for _, r := range p.rows {
		if !r.local || !r.id.IsZero() {
			return fmt.Errorf("the pane of an evicted child still shows entry %v", r.id)
		}
	}
	if len(p.ids) != 0 {
		return fmt.Errorf("the pane of an evicted child still indexes %d entries", len(p.ids))
	}
	clear(w.shows)
	clear(w.byID)
	clear(w.re)
	return nil
}

// ---------------------------------------------------------------- the hook

// hook is foldHook: the half before the pane consumes the Change, which
// returns the half after it.
func (w *parityWatch) hook(m *Model, ev agent.Event, ch transcript.Change, hide transcript.Kind) func() {
	w.mu.Lock()
	defer w.mu.Unlock()
	r := w.rec(m.shared)
	r.folds++
	w.stats.folds++

	// Every pane as it stands before this fold: what happened to it since the
	// last one. A pane first seen, or emptied since (/clear, a receipt
	// rebuild), starts over; /clear marks the run it left open, which the
	// shadow — not yet folded past the moment of the clear — holds open still.
	for scope, p := range panesOf(m) {
		want := r.panes[p]
		if want != nil && p.emptied == want.emptied {
			if err := want.between(p); err != nil {
				w.fail("before fold %d (%s), pane %q: %v", r.folds, ev.Type, scope, err)
			}
			continue
		}
		want, err := newPaneWant(p)
		if err != nil {
			w.fail("before fold %d (%s), pane %q: %v", r.folds, ev.Type, scope, err)
		}
		if p == m.main {
			want.mark, want.from = openRun(r.shadow.Main)
		}
		r.panes[p] = want
	}

	in := m.foldIn
	s := paritySample{ev: ev, at: in.at, errText: in.errText}
	r.events = append(r.events, s)
	r.replay = s
	if got := r.shadow.Fold(ev); got != ch {
		w.fail("fold %d (%s): the TUI's model changed %+v; a model folded from the same events, %+v", r.folds, ev.Type, ch, got)
	}
	if got, want := m.shared.Seq(), r.shadow.Seq(); got != want {
		w.fail("fold %d (%s): the TUI's model is at seq %d, the events' at %d", r.folds, ev.Type, got, want)
	}
	return func() {
		w.mu.Lock()
		defer w.mu.Unlock()
		w.consumed(m, r, ev, ch, hide)
	}
}

// consumed is the half after the pane consumed the fold: the pane's rows
// against the watch's account of them (P2), the rows the fold named (P1), the
// panes of children the model evicted, and — when it is due — the whole check.
func (w *parityWatch) consumed(m *Model, r *parityRec, ev agent.Event, ch transcript.Change, hide transcript.Kind) {
	live := panesOf(m)
	if p, tr := live[ch.Scope], scopeOf(r.shadow, ch.Scope); ch.Entries() && p != nil && tr != nil {
		want := r.panes[p]
		if want == nil {
			// Made by this fold's consumption (ensureSub).
			want = &paneWant{emptied: p.emptied, shows: map[*entry]transcript.EntryID{},
				byID: map[transcript.EntryID]*entry{}, re: map[*entry]bool{}}
			r.panes[p] = want
		}
		if err := want.consumed(p, tr, ch, hide); err != nil {
			w.fail("fold %d (%s), pane %q: %v", r.folds, ev.Type, ch.Scope, err)
		}
		for _, id := range [...]transcript.EntryID{ch.Touched[0], ch.Touched[1], ch.AppendedFrom, ch.AppendedTo} {
			if row := p.ids[id]; !id.IsZero() && row != nil {
				w.checkRow(r.folds, ev, ch.Scope, row, m.shared)
			}
		}
	}
	if ev.Type == agent.EventSubagent {
		for scope, p := range live {
			if want := r.panes[p]; scope != "" && want != nil && r.shadow.Sub(scope) == nil {
				if err := want.detached(p); err != nil {
					w.fail("fold %d (%s), pane %q: %v", r.folds, ev.Type, scope, err)
				}
			}
		}
	}
	if w.wholeDue(m, r, live) {
		w.whole(m, r, ev, live)
	} else {
		w.stats.sampled++
	}
}

// wholeDue is the whole check's schedule. live is panesOf(m).
func (w *parityWatch) wholeDue(m *Model, r *parityRec, live map[string]*pane) bool {
	k := r.folds
	if w.strict || k <= 64 || k&(k-1) == 0 || k%256 == 0 {
		return true
	}
	size := m.shared.Main.Len()
	for _, id := range m.shared.Subs() {
		if t := m.shared.Sub(id); t != nil {
			size += t.Len()
		}
	}
	for _, p := range live {
		size += len(p.rows)
	}
	return size <= parityFullSize
}

// rowShown is a row's display value: everything show writes.
type rowShown struct {
	Kind      entryKind
	Text      string
	Tool      *agent.ToolEvent
	Plan      *agent.PlanEvent
	At, End   time.Time
	Open      bool
	Interject bool
	Streaming bool
	Local     bool
	ID        transcript.EntryID
	Cont      bool
	ContFrom  int
}

func shownOf(e *entry) rowShown {
	return rowShown{
		Kind: e.kind, Text: e.text, Tool: e.tool, Plan: e.plan, At: e.at, End: e.end,
		Open: e.open, Interject: e.interject, Streaming: e.streaming, Local: e.local,
		ID: e.id, Cont: e.cont, ContFrom: e.contFrom,
	}
}

// checkRow is P1 for one row of a pane of scope: it shows its entry as sm
// holds it now.
func (w *parityWatch) checkRow(fold int, ev agent.Event, scope string, row *entry, sm *transcript.Model) {
	tr := scopeOf(sm, scope)
	if tr == nil {
		w.fail("fold %d (%s): pane %q shows entry %v of a transcript the model does not hold", fold, ev.Type, scope, row.id)
	}
	e, ok := tr.Entry(row.id)
	if !ok {
		w.fail("fold %d (%s): pane %q shows entry %v, which the model no longer holds: %+v", fold, ev.Type, scope, row.id, shownOf(row))
	}
	want := *row
	want.show(e, tr)
	if got, exp := shownOf(row), shownOf(&want); !reflect.DeepEqual(got, exp) {
		w.fail("fold %d (%s): pane %q holds a stale row\n got %+v\nwant %+v", fold, ev.Type, scope, got, exp)
	}
}

// whole is the whole check — P0, and P1, P3 and P4 over every pane (live,
// panesOf(m)).
func (w *parityWatch) whole(m *Model, r *parityRec, ev agent.Event, live map[string]*pane) {
	w.stats.full++
	fold := r.folds
	if got, want := m.shared.History(), r.shadow.History(); !reflect.DeepEqual(got, want) {
		w.fail("fold %d (%s): the TUI's model's history is not the events'\n got %+v\nwant %+v", fold, ev.Type, got, want)
	}
	if got, want := m.shared.State(), r.shadow.State(); !reflect.DeepEqual(got, want) {
		w.fail("fold %d (%s): the TUI's model's state is not the events'\n got %+v\nwant %+v", fold, ev.Type, got, want)
	}
	if fold&(fold-1) == 0 {
		w.fresh(m, r, ev)
	}
	alive := slices.Collect(maps.Values(live))
	maps.DeleteFunc(r.panes, func(p *pane, _ *paneWant) bool { return !slices.Contains(alive, p) })
	for scope, p := range live {
		w.wholePane(m, r, ev, scope, p)
	}
}

// fresh folds the recorded prefix from scratch into a model of its own, and
// holds the TUI's against it.
func (w *parityWatch) fresh(m *Model, r *parityRec, ev agent.Event) {
	w.stats.fresh++
	var cur paritySample
	f := transcript.New(replayOptions(&cur))
	for _, s := range r.events {
		cur = s
		f.Fold(s.ev)
	}
	if got, want := m.shared.History(), f.History(); !reflect.DeepEqual(got, want) {
		w.fail("fold %d (%s): the TUI's model's history is not a model's folded from scratch from its %d events\n got %+v\nwant %+v",
			r.folds, ev.Type, len(r.events), got, want)
	}
	if got, want := m.shared.State(), f.State(); !reflect.DeepEqual(got, want) {
		w.fail("fold %d (%s): the TUI's model's state is not a model's folded from scratch from its %d events\n got %+v\nwant %+v",
			r.folds, ev.Type, len(r.events), got, want)
	}
}

// wholePane is P1, P3 and P4 over one pane, and the account's own indexes
// against the pane's.
func (w *parityWatch) wholePane(m *Model, r *parityRec, ev agent.Event, scope string, p *pane) {
	fold := r.folds
	want := r.panes[p]
	shared := 0
	for _, row := range p.rows {
		w.stats.rows++
		// P4.
		if row.local != row.id.IsZero() {
			w.fail("fold %d (%s): pane %q holds a row marked local=%v with entry %v: %+v", fold, ev.Type, scope, row.local, row.id, shownOf(row))
		}
		if want != nil && row.id != want.shows[row] {
			w.fail("fold %d (%s): pane %q has a row showing entry %v where its account says %v", fold, ev.Type, scope, row.id, want.shows[row])
		}
		if row.id.IsZero() {
			continue
		}
		shared++
		if p.ids[row.id] != row {
			w.fail("fold %d (%s): pane %q shows entry %v twice, or indexes another row for it", fold, ev.Type, scope, row.id)
		}
		w.checkRow(fold, ev, scope, row, m.shared)
	}
	// The index is exactly the shown rows, one row per entry.
	if len(p.ids) != shared {
		w.fail("fold %d (%s): pane %q indexes %d rows and shows %d", fold, ev.Type, scope, len(p.ids), shared)
	}
	tr := scopeOf(m.shared, scope)
	if tr == nil || want == nil {
		return
	}
	// P3: in the model's order, re-appended rows excepted.
	pos := map[transcript.EntryID]int{}
	for i, e := range tr.Entries() {
		pos[e.ID] = i
	}
	last := -1
	for _, row := range p.rows {
		if row.id.IsZero() || want.re[row] {
			continue
		}
		if pos[row.id] <= last {
			w.fail("fold %d (%s): pane %q shows entry %v out of the model's order", fold, ev.Type, scope, row.id)
		}
		last = pos[row.id]
	}
}

// stateMirrors is what the TUI mirrors from State() of what the shared model
// folds too (plan 024 §3.8): the settings, the todo list, the roster and the
// queue. An empty list is nil on both sides (X10).
type stateMirrors struct {
	Title, Mode, Model string
	Config             []agent.ConfigOption
	Commands           []agent.CommandInfo
	Plugins            []agent.PluginCommand
	Todos              []agent.Todo
	Agents             []agent.SubagentInfo
	Queue              []agent.QueuedPrompt
}

func noneIsNil[S ~[]E, E any](s S) S {
	if len(s) == 0 {
		return nil
	}
	return s
}

// tuiMirrors is the TUI's side: m.snap and m.queue, read from State().
func tuiMirrors(m Model) stateMirrors {
	return stateMirrors{
		Title: m.snap.Title, Mode: m.snap.CurrentMode, Model: m.snap.CurrentModel,
		Config: noneIsNil(m.snap.Config), Commands: noneIsNil(m.snap.Commands), Plugins: noneIsNil(m.snap.Plugins),
		Todos: noneIsNil(m.snap.Todos), Agents: noneIsNil(m.snap.Subagents), Queue: noneIsNil(m.queue),
	}
}

// modelMirrors is the shared model's side.
func modelMirrors(st transcript.State) stateMirrors {
	return stateMirrors{
		Title: st.Settings.Title, Mode: st.Settings.Mode, Model: st.Settings.Model,
		Config: st.Settings.Config, Commands: st.Settings.Commands, Plugins: st.Settings.Plugins,
		Todos: st.Todos, Agents: st.Agents, Queue: st.Queue,
	}
}

// mirrorDiff names every mirror on which the two sides differ.
func mirrorDiff(tui, model stateMirrors) []string {
	var out []string
	tv, mv := reflect.ValueOf(tui), reflect.ValueOf(model)
	for i := 0; i < tv.NumField(); i++ {
		if !reflect.DeepEqual(tv.Field(i).Interface(), mv.Field(i).Interface()) {
			out = append(out, fmt.Sprintf("%s: TUI %+v, model %+v", tv.Type().Field(i).Name, tv.Field(i).Interface(), mv.Field(i).Interface()))
		}
	}
	return out
}

// ------------------------------------------------------------------ the tests

// TestTheTUIsTranscriptIsTheSharedModel is plan 024 A11's first half. The
// parity watch is installed for the whole package, so every other test in
// internal/tui is already a parity fixture — every frame golden's script, every
// scripted and pumped session, every direct-Update fixture — checked at every
// fold (in whole while the session is small, which is nearly every test). This
// test holds the watch to being there, and adds fixtures of its own for the
// rules the rest of the suite reaches least — every kind of event on main and
// on a child, /clear in the middle of a run and a tool updated after it, a
// model trim with a re-appended row, a child's pane pruned and made again, a
// receipt rebuild, a session replaced under its rows — plus one frame script,
// one pumped session and one replay, all with the whole check at every fold.
func TestTheTUIsTranscriptIsTheSharedModel(t *testing.T) {
	if foldHook == nil {
		t.Fatal("the parity watch is not installed: TestMain installs it for the whole package")
	}
	was := parity.setStrict(true)
	t.Cleanup(func() { parity.setStrict(was) })

	for _, sc := range []struct {
		name string
		run  func(*testing.T)
	}{
		{"every kind of event, on main and on a child", parityEveryKind},
		{"a clear mid-run, a cleared tool updated, a model trim", parityClearAndTrim},
		{"a child's pane pruned and made again, and a receipt rebuild", parityChildren},
		{"a session replaced under its rows", paritySessionSwap},
		{"a frame script", func(t *testing.T) {
			runStubFrame(t, 80, 24, "<wait:idle>hello<enter><wait:text:echo:><wait:idle>/clear<enter><wait:idle>again<enter><wait:text:follow-up: again><wait:idle>")
		}},
		{"a pumped session", func(t *testing.T) {
			m := sized(t)
			m = pumpEnter(t, m, "hello")
			m = pumpSettled(t, m)
			m = pumpEnter(t, m, "and again")
			pumpSettled(t, m)
		}},
		{"a replay", func(t *testing.T) {
			m, _ := loadedStub(t, nil)
			feed(t, m,
				replayEvent(agent.ReplayStart),
				replayed(agent.Event{Type: agent.EventUser, Text: "an old prompt"}),
				replayed(agent.Event{Type: agent.EventThought, Text: "old thinking"}),
				replayed(agent.Event{Type: agent.EventText, Text: "an old reply"}),
				replayEvent(agent.ReplayEnd),
			)
		}},
	} {
		t.Run(sc.name, func(t *testing.T) {
			before := parity.snapshot()
			sc.run(t)
			after := parity.snapshot()
			folds := after.folds - before.folds
			if folds == 0 {
				t.Fatal("the scenario folded nothing the watch saw")
			}
			if whole := after.full - before.full; whole != folds {
				t.Fatalf("%d folds, %d checked whole: strict checks every one", folds, whole)
			}
			t.Logf("%d folds, every one checked whole", folds)
		})
	}
	if err := parity.err(); err != nil {
		t.Fatal(err)
	}
	s := parity.snapshot()
	t.Logf("so far in this run the watch has checked %d folds of %d shared models: %d in whole, %d from scratch",
		s.folds, s.models, s.full, s.fresh)
}

var parityBase = time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)

func foldAt(sec int) time.Time { return parityBase.Add(time.Duration(sec) * time.Second) }

// parityEveryKind folds every kind of event, stamped and unstamped, on main and
// on a child.
func parityEveryKind(t *testing.T) {
	m := withChild(t, sized(t), "task-1")
	tool := func(ev agent.ToolEvent) agent.Event { return agent.Event{Type: agent.EventTool, Tool: &ev} }
	perm := stubPermissionEvent(false)
	q := stubQuestion()
	plan := stubPlanEvent()
	title, mode, model := "a title", "plan", "fast"
	m = feed(t, m,
		agent.Event{Type: agent.EventTurn, Turn: &agent.TurnInfo{ID: "t-1", Phase: agent.TurnStarted, Text: "from elsewhere", Origin: agent.TurnOriginSubmit}, At: foldAt(1)},
		agent.Event{Type: agent.EventCommand, Command: &agent.ExpandedCommand{PluginCommand: agent.PluginCommand{Qualified: "p:c", Kind: agent.PluginKindCommand}}},
		agent.Event{Type: agent.EventThought, Text: "weighing ", At: foldAt(2)},
		tool(agent.ToolEvent{ID: "todo-1", Kind: "other", Title: "Update TODOs: a", ToolName: "updateTodos"}),
		agent.Event{Type: agent.EventThought, Text: "it"},
		tool(agent.ToolEvent{ID: "r1", Kind: "read", Status: "pending", Title: "Read main.go", Locations: []string{"/w/a/main.go"}, At: foldAt(3)}),
		tool(agent.ToolEvent{ID: "r1", Kind: "read", Status: "completed", Title: "Read main.go", Locations: []string{"/w/a/main.go"}}),
		tool(agent.ToolEvent{Kind: "search", Title: "Search", RawInput: "needle"}),
		tool(agent.ToolEvent{ID: "r2", Kind: "read", Status: "completed", Title: "Read main.go", Locations: []string{"/w/b/main.go"}}),
		agent.Event{Type: agent.EventText, Text: "an answer", At: foldAt(4)},
		agent.Event{Type: agent.EventText, Text: ""},
		agent.Event{Type: agent.EventTodos, Todos: []agent.Todo{{ID: "1", Content: "a", Status: "pending"}, {ID: "2", Content: "b", Status: "pending"}}},
		agent.Event{Type: agent.EventTodos, Todos: []agent.Todo{{ID: "1", Content: "a", Status: "completed"}, {ID: "2", Content: "b", Status: "cancelled"}}},
		agent.Event{Type: agent.EventPermission, Permission: perm, At: foldAt(5)},
		agent.Event{Type: agent.EventAsk, Ask: &agent.AskUpdate{ID: perm.ID, Kind: agent.AskPermission, Outcome: agent.AskCancelled}},
		agent.Event{Type: agent.EventQuestion, Question: q},
		agent.Event{Type: agent.EventAsk, Ask: &agent.AskUpdate{ID: q.ID, Kind: agent.AskQuestion, Outcome: agent.AskAnswered,
			Answers: map[string][]string{"q1": {"opt-b"}}}},
		agent.Event{Type: agent.EventPlan, Plan: plan, At: foldAt(6)},
		agent.Event{Type: agent.EventAsk, Ask: &agent.AskUpdate{ID: plan.ID, Kind: agent.AskPlan, Outcome: agent.AskAnswered, Accepted: true}},
		agent.Event{Type: agent.EventUser, Interjection: true, Text: "and also"},
		agent.Event{Type: agent.EventUser, Text: "a live echo"},
		agent.Event{Type: agent.EventMeta, Seq: 0, State: &agent.StateDelta{
			Title: &title, Mode: &mode, Model: &model,
			Config:   &agent.ConfigState{Options: []agent.ConfigOption{{ID: "effort", Current: "high"}}},
			Commands: &agent.CommandsState{Commands: []agent.CommandInfo{{Name: "research"}}},
			Plugins:  &agent.PluginsState{Plugins: []agent.PluginCommand{{Qualified: "p:c"}}},
			SendNow:  &agent.SendNowState{},
			Detail:   "cancel failed", IndexErr: "index write failed",
		}, Mode: "plan", Text: "a title"},
		agent.Event{Type: agent.EventQueue, QueueChange: agent.QueueQueued, Queue: &agent.QueuedPrompt{ID: "q-1", Text: "later"}},
		agent.Event{Type: agent.EventQueue, QueueChange: agent.QueueEdited, Queue: &agent.QueuedPrompt{ID: "q-1", Text: "later, edited"}},
		agent.Event{Type: agent.EventQueue, QueueChange: agent.QueueSent, Queue: &agent.QueuedPrompt{ID: "q-1"}},
		agent.Event{Type: agent.EventForeignTurn, ForeignTurn: &agent.ForeignTurnInfo{ID: "ft-1", Running: true}, At: foldAt(7)},
		agent.Event{Type: agent.EventText, Text: "on its own"},
		agent.Event{Type: agent.EventForeignTurn, ForeignTurn: &agent.ForeignTurnInfo{ID: "ft-1"}},
		agent.Event{Type: agent.EventError, Err: errors.New("the agent fell over"), At: foldAt(8)},
		agent.Event{Type: agent.EventError, Err: errors.New("")},
		agent.Event{Type: agent.EventDone, StopReason: stopCancelled},
		agent.Event{Type: agent.EventTurn, Turn: &agent.TurnInfo{ID: "t-2", Phase: agent.TurnEnded, Synthetic: true, StopReason: stopCancelled}},
		agent.Event{Type: agent.EventTurn, Turn: &agent.TurnInfo{ID: "t-3", Phase: agent.TurnEnded, Synthetic: true, Err: "refused"}},
		agent.Event{Type: agent.EventTurn, Turn: &agent.TurnInfo{ID: "t-1", Phase: agent.TurnEnded, StopReason: "end_turn"}},
		agent.Event{Type: agent.EventText, Agent: "task-1", Text: "the child says", At: foldAt(9)},
		agent.Event{Type: agent.EventThought, Agent: "task-1", Text: "the child thinks"},
		agent.Event{Type: agent.EventUser, Agent: "task-1", Text: "the child's prompt"},
		agent.Event{Type: agent.EventTool, Agent: "task-1", Tool: &agent.ToolEvent{ID: "c1", Kind: "read", Status: "completed", Title: "Read a.go", Locations: []string{"/w/a.go"}}},
		agent.Event{Type: agent.EventCommand, Agent: "task-1", Command: &agent.ExpandedCommand{PluginCommand: agent.PluginCommand{Qualified: "p:c"}}},
		agent.Event{Type: agent.EventTodos, Agent: "task-1", Todos: []agent.Todo{{ID: "1", Content: "ignored", Status: "pending"}}},
		agent.Event{Type: "no-such-kind"},
	)
	fin := subagentsFromTools([]agent.ToolEvent{finishedTaskTool("task-1", "count lines")})[0]
	m.sess.(*Stub).SetSubagents([]agent.SubagentInfo{fin})
	feed(t, m, agent.Event{Type: agent.EventSubagent, Subagent: &fin, SubagentChange: agent.SubagentChangeFinished, At: foldAt(10)})
}

// parityClearAndTrim is X30, X29 and X28 in one session.
func parityClearAndTrim(t *testing.T) {
	m := sized(t)
	tool := func(id, status string, payload int) agent.Event {
		return agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{
			ID: id, Kind: "execute", Status: status, Title: "Shell", RawInput: "echo " + id, ContentText: strings.Repeat("x", payload),
		}}
	}
	m = feed(t, m, agent.Event{Type: agent.EventThought, Text: "before ", At: foldAt(1)})
	m = runSlash(t, m, "/clear")
	m = feed(t, m, agent.Event{Type: agent.EventThought, Text: "after", At: foldAt(2)}, tool("old", "pending", 10))
	m.addNote("a local row")
	m = runSlash(t, m, "/clear")
	m = feed(t, m, tool("old", "completed", 10), agent.Event{Type: agent.EventText, Text: "streaming "})
	m = runSlash(t, m, "/clear")
	m = feed(t, m, agent.Event{Type: agent.EventText, Text: "on"})
	for i := 0; i < 12; i++ {
		m = feed(t, m, tool(fmt.Sprintf("big-%d", i), "completed", 1<<20))
		m.addError(fmt.Sprintf("local %d", i))
	}
}

// parityChildren prunes a child's pane, which the next child event makes
// again, and rebuilds a receipt-mode child's pane under events of its own.
func parityChildren(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	m := agentModel(t, &now)
	m = applyInFlight(t, m, []agent.ToolEvent{taskTool("task-1", "count lines", "in_progress")})
	m = feed(t, m,
		agent.Event{Type: agent.EventThought, Agent: "task-1", Text: "thinking"},
		agent.Event{Type: agent.EventTool, Agent: "task-1", Tool: &agent.ToolEvent{ID: "c1", Kind: "read", Status: "pending", Title: "Read a.go"}},
	)
	// What pruneSubs does to a child that has left the roster's window.
	delete(m.subs, "task-1")
	m = feed(t, m,
		agent.Event{Type: agent.EventTool, Agent: "task-1", Tool: &agent.ToolEvent{ID: "c1", Kind: "read", Status: "completed", Title: "Read a.go"}},
		agent.Event{Type: agent.EventText, Agent: "task-1", Text: "done"},
	)
	m = openView(t, m)
	m = feed(t, m,
		agent.Event{Type: agent.EventText, Agent: "task-1", Text: " and more"},
		agent.Event{Type: agent.EventTool, Agent: "task-1", Tool: &agent.ToolEvent{ID: "c1", Kind: "read", Status: "failed", Title: "Read a.go"}},
	)
}

// paritySessionSwap replaces the session under rows the old one drew: they stay
// on screen as this client's own, and the new model folds from nothing.
func paritySessionSwap(t *testing.T) {
	m := sized(t)
	m = feed(t, m,
		agent.Event{Type: agent.EventText, Text: "the old session's reply"},
		agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: "t1", Kind: "read", Status: "pending", Title: "Read"}},
	)
	old := m.eng
	m.setSession(NewStub(), "")
	_ = old.Close()
	t.Cleanup(func() { _ = m.eng.Close() })
	for _, e := range m.main.rows {
		if !e.local || !e.id.IsZero() {
			t.Fatalf("a row the old session drew still names an entry of a model: %+v", shownOf(e))
		}
	}
	feed(t, m,
		agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: "t1", Kind: "read", Status: "completed", Title: "Read"}},
		agent.Event{Type: agent.EventText, Text: "the new session's reply"},
	)
}

// TestTheStateMirrorsMatchTheModelWhenQuiet is plan 024 A11's second half:
// with everything the session published consumed (pumpSettled, and pumpDrained
// under a held turn), what the TUI mirrors from State() — m.snap's title, mode,
// model, config, commands and plugins, the todo list, the sub-agent roster, and
// m.queue — equals what the shared model folded from the stream.
//
// Two documented exemptions, both of the session and neither of the fold:
//
//   - the Stub publishes no install delta at Start, where the live session and
//     native both do (live.go installDeltaLocked, native.go's Start): its
//     start-up catalog is test set-up, like its Set* helpers. The stub cases
//     publish that delta themselves, as a live session's Start would;
//   - native's title: native sets it from the first prompt without publishing
//     it (SF-01). C8 publishes it and removes the exemption.
func TestTheStateMirrorsMatchTheModelWhenQuiet(t *testing.T) {
	check := func(t *testing.T, m Model, when string, exempt ...string) {
		t.Helper()
		for _, d := range mirrorDiff(tuiMirrors(m), modelMirrors(m.shared.State())) {
			if slices.ContainsFunc(exempt, func(f string) bool { return strings.HasPrefix(d, f+":") }) {
				continue
			}
			t.Fatalf("%s: the TUI's mirror and the shared model differ on %s", when, d)
		}
	}

	t.Run("a stub session, publishing as a live session does", func(t *testing.T) {
		m := sized(t)
		stub := m.sess.(*Stub)
		stub.Emit(agent.Event{Type: agent.EventMeta, State: installDelta(stub.Snapshot())})
		m = pumpSettled(t, m)
		check(t, m, "started")

		m = pumpEnter(t, m, "/model fast")
		m = pumpSettled(t, m)
		check(t, m, "after /model")

		m = pumpKey(t, m, tea.KeyMsg{Type: tea.KeyShiftTab})
		m = pumpSettled(t, m)
		check(t, m, "after a mode change")

		m = pumpEnter(t, m, "/rename a better title")
		m = pumpSettled(t, m)
		check(t, m, "after /rename")

		m = pumpEnter(t, m, "hello")
		m = pumpSettled(t, m)
		check(t, m, "after a turn")

		todos := []agent.Todo{{ID: "1", Content: "Read", Status: "in_progress"}, {ID: "2", Content: "Edit", Status: "pending"}}
		stub.SetTodos(todos)
		stub.Emit(agent.Event{Type: agent.EventTodos, Todos: todos})
		m = pumpSettled(t, m)
		check(t, m, "after a todo list")

		cmds := []agent.CommandInfo{{Name: "research"}, {Name: "review", Description: "a new one"}}
		stub.SetCommands(cmds)
		stub.Emit(agent.Event{Type: agent.EventMeta, State: &agent.StateDelta{Commands: &agent.CommandsState{Commands: cmds}}})
		plugins := []agent.PluginCommand{{Qualified: "p:c", Kind: agent.PluginKindCommand}}
		stub.SetPlugins(plugins)
		stub.Emit(agent.Event{Type: agent.EventMeta, State: &agent.StateDelta{Plugins: &agent.PluginsState{Plugins: plugins}}})
		m = pumpSettled(t, m)
		check(t, m, "after the catalogs moved")

		spawned := subagentsFromTools([]agent.ToolEvent{taskTool("task-1", "count lines", "in_progress")})[0]
		stub.SetSubagents([]agent.SubagentInfo{spawned})
		stub.Emit(agent.Event{Type: agent.EventSubagent, Subagent: &spawned, SubagentChange: agent.SubagentChangeSpawned})
		m = pumpSettled(t, m)
		check(t, m, "after a sub-agent spawned")
		fin := subagentsFromTools([]agent.ToolEvent{finishedTaskTool("task-1", "count lines")})[0]
		stub.SetSubagents([]agent.SubagentInfo{fin})
		stub.Emit(agent.Event{Type: agent.EventSubagent, Subagent: &fin, SubagentChange: agent.SubagentChangeFinished})
		m = pumpSettled(t, m)
		check(t, m, "after it finished")

		// The queue, while a turn is held and after it is let go.
		parked := stub.ParkNext()
		m = pumpEnter(t, m, "held")
		awaitBarrier(t, parked, "the prompt reaching the stub's wait")
		m = pumpEnter(t, m, "queued behind it")
		m = pumpDrained(t, m)
		if len(m.queue) != 1 {
			t.Fatalf("fixture: the queue holds %d rows", len(m.queue))
		}
		check(t, m, "with a row queued")
		m = pumpEsc(t, m)
		m = pumpSettled(t, m)
		check(t, m, "after the queue drained")
	})

	t.Run("a native session", func(t *testing.T) {
		isolateSkillsHome(t)
		model := &nativeScriptedModel{provider: "test", wire: "wire-echo"}
		model.steps = [][]fantasy.StreamPart{append(nativeTextParts("hi there"), nativeFinishParts()...)}
		ws := t.TempDir()
		sess := agent.NewNative(agent.Options{Workspace: ws, ContentHome: t.TempDir()}, nativeSessionTweak(t.TempDir(), nativeOneModelTable(), model))
		if err := sess.Start(context.Background()); err != nil {
			t.Fatalf("Start: %v", err)
		}
		m := New(Config{Session: sess, Theme: "tokyo-night", Workspace: ws, Yolo: true, ProviderLocked: true})
		tm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
		tm, _ = tm.(Model).Update(startedMsg{})
		m = pumpSettled(t, tm.(Model))
		check(t, m, "started")
		m = pumpEnter(t, m, "hello")
		m = pumpSettled(t, m)
		check(t, m, "after a turn")
		// A mode change is a delta, which re-reads the snapshot — and with it
		// the title native took from the first prompt.
		m = pumpKey(t, m, tea.KeyMsg{Type: tea.KeyShiftTab})
		m = pumpSettled(t, m)
		// SF-01: native names the session from its first prompt without
		// publishing the name, so the TUI's mirror has it and the stream does
		// not. C8 publishes it: then this exemption, and the two lines that
		// say it is in force, go.
		if m.snap.Title != "hello" || m.shared.State().Settings.Title != "" {
			t.Fatalf("SF-01 is no longer what this exempts: the TUI's title %q, the model's %q", m.snap.Title, m.shared.State().Settings.Title)
		}
		check(t, m, "after a mode change", "Title")
	})
}

// installDelta is the delta a live session's Start publishes (live.go
// installDeltaLocked): every section of the snapshot, as it stands.
func installDelta(s agent.Snapshot) *agent.StateDelta {
	title, mode, model := s.Title, s.CurrentMode, s.CurrentModel
	return &agent.StateDelta{
		Title:    &title,
		Mode:     &mode,
		Model:    &model,
		Config:   &agent.ConfigState{Options: append([]agent.ConfigOption(nil), s.Config...)},
		Commands: &agent.CommandsState{Commands: append([]agent.CommandInfo(nil), s.Commands...)},
		Plugins:  &agent.PluginsState{Plugins: append([]agent.PluginCommand(nil), s.Plugins...)},
	}
}

// TestTheParityWatchCountsEveryDrop is review r17's fifth finding: the watch's
// account of a pane (paneWant) takes exactly the rows the caps and the model's
// trims take, so a pane that loses one surviving row more than that — after a
// trim that was legitimate — fails it, where holding the rows to a suffix of
// the expected ones would pass it. Each case runs the pane's own code for the
// legitimate part and plants the extra drop by hand.
func TestTheParityWatchCountsEveryDrop(t *testing.T) {
	local := func(p *pane, text string) { p.appendLocal(entry{kind: entryNote, text: text}, time.Time{}) }
	account := func(t *testing.T, p *pane) *paneWant {
		t.Helper()
		w, err := newPaneWant(p)
		if err != nil {
			t.Fatal(err)
		}
		return w
	}

	t.Run("the row cap, between folds", func(t *testing.T) {
		p := &pane{entryCap: 3}
		for _, s := range []string{"a", "b", "c"} {
			local(p, s)
		}
		w := account(t, p)
		local(p, "d") // the cap takes "a"
		if err := w.between(p); err != nil {
			t.Fatalf("a legitimate trim failed the watch: %v", err)
		}
		local(p, "e") // the cap takes "b"
		p.dropFirst(1)
		if err := w.between(p); err == nil {
			t.Fatal("a pane that lost a surviving row past its cap's trim passed the watch")
		}
	})

	t.Run("the text budget, between folds", func(t *testing.T) {
		p := &pane{entryCap: 10, textBudget: 10}
		local(p, "aaaa")
		local(p, "bbbb")
		w := account(t, p)
		local(p, "cccc") // 12 bytes: the budget takes "aaaa"
		if err := w.between(p); err != nil {
			t.Fatalf("a legitimate trim failed the watch: %v", err)
		}
		local(p, "eeee") // 12 again: "bbbb" goes, and "cccc" + "eeee" fit
		p.dropFirst(1)   // "cccc" as well, which fitted
		if err := w.between(p); err == nil {
			t.Fatal("a pane that lost a row its text budget did not need to drop passed the watch")
		}
	})

	fold := func(sm *transcript.Model, q string) transcript.Change {
		return sm.Fold(agent.Event{Type: agent.EventCommand, Command: &agent.ExpandedCommand{
			PluginCommand: agent.PluginCommand{Qualified: q},
		}})
	}
	opts := func(b transcript.Bounds) transcript.Options {
		o := sharedOptions(time.Now, func(err error) string { return err.Error() })
		o.Bounds = b
		return o
	}

	t.Run("the row cap, in a fold", func(t *testing.T) {
		sm := transcript.New(opts(transcript.Bounds{}))
		p := &pane{entryCap: 3}
		w := account(t, p)
		for i := range 4 { // the fourth row is a legitimate trim
			ch := fold(sm, fmt.Sprintf("p:c%d", i))
			p.consume(ch, sm.Main, 0)
			if err := w.consumed(p, sm.Main, ch, 0); err != nil {
				t.Fatalf("fold %d failed the watch: %v", i, err)
			}
		}
		ch := fold(sm, "p:c4")
		p.consume(ch, sm.Main, 0)
		p.dropFirst(1)
		if err := w.consumed(p, sm.Main, ch, 0); err == nil {
			t.Fatal("a fold that dropped a surviving row past the cap's trim passed the watch")
		}
	})

	t.Run("the model's trim, in a fold", func(t *testing.T) {
		sm := transcript.New(opts(transcript.Bounds{MainEntries: 2}))
		p := &pane{}
		w := account(t, p)
		for i := range 3 { // the third fold makes the model drop the first
			ch := fold(sm, fmt.Sprintf("p:c%d", i))
			p.consume(ch, sm.Main, 0)
			if err := w.consumed(p, sm.Main, ch, 0); err != nil {
				t.Fatalf("fold %d failed the watch: %v", i, err)
			}
		}
		if !p.trimmed {
			t.Fatal("fixture: the model's trim did not reach the pane")
		}
		ch := fold(sm, "p:c3")
		p.consume(ch, sm.Main, 0)
		p.dropFirst(1) // a row whose entry the model still holds
		if err := w.consumed(p, sm.Main, ch, 0); err == nil {
			t.Fatal("a fold that dropped a row the model still holds passed the watch")
		}
	})
}
