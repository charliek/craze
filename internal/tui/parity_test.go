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
//   - P0: a shadow model folded from the recorded events alone, fold by fold,
//     returns the same Change for every event and is at the same Seq, and
//     holds the same History and State as m.shared; and at every power-of-two
//     fold a model folded from scratch from the whole recorded prefix holds
//     them too;
//   - P1: every shared row of every pane shows its entry as the model holds it
//     now — no stale row, and no row for an entry the model has let go;
//   - P2: every entry appended in a pane's scope since that pane was made or
//     last emptied (/clear, a receipt rebuild) — not echo-hidden, and not taken
//     since by the pane's own cap — has exactly one row, and so has every entry
//     re-shown by the rules for a touch with no row (X29, X30); a pane that
//     lost a prefix to its cap draws the trim note;
//   - P3: a pane's shared rows are in the model's order, re-appended ones
//     (X29) excepted;
//   - P4: a local row carries no entry and a row that carries one is not local,
//     so no local row is in the model — which P0's model, folded from events
//     alone, holds exactly.
//
// The Change, the Seq and the rows a fold named are checked at every fold. The
// whole of P0 to P4 is checked at every fold of the first 64, at every power of
// two and every 256th, and at every fold while the session is small
// (parityFullSize) — which is every fold of nearly every test; the few that
// fill a pane to its cap stay linear. strict checks everything at every fold.
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

// paritySample is one fold as the TUI made it: the event, and the instant the
// fold's clock read, if it read one (foldClock).
type paritySample struct {
	ev agent.Event
	at time.Time
}

// parityRec is one shared model's record: its shadow, the events, and what
// each of the TUI's panes must show of them.
type parityRec struct {
	shadow *transcript.Model
	// replay is the instant the shadow's clock answers with: the one the TUI's
	// fold read for the same event.
	replay time.Time
	events []paritySample
	folds  int
	panes  map[*pane]*paneWant
}

// paneWant is what one pane must show of its scope's entries: their ids in
// row order — a prefix of which the pane's own cap may have taken since the
// last whole check — which of them were re-appended (X29), and the clear mark,
// which the watch takes itself, from the shadow, when it sees the pane emptied.
type paneWant struct {
	emptied int
	order   []transcript.EntryID
	in      map[transcript.EntryID]bool
	re      map[transcript.EntryID]bool
	mark    transcript.EntryID
}

func newPaneWant(emptied int) *paneWant {
	return &paneWant{emptied: emptied, in: map[transcript.EntryID]bool{}, re: map[transcript.EntryID]bool{}}
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
	r.shadow = transcript.New(sharedOptions(func() time.Time { return r.replay }))
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

func (r *parityRec) want(p *pane) *paneWant {
	w := r.panes[p]
	if w == nil {
		w = newPaneWant(p.emptied)
		r.panes[p] = w
	}
	return w
}

// add puts id at the end of the list, moving it there if it is in it already.
func (w *paneWant) add(id transcript.EntryID, re bool) {
	if w.in[id] {
		w.order = slices.DeleteFunc(w.order, func(x transcript.EntryID) bool { return x == id })
	}
	w.order = append(w.order, id)
	w.in[id] = true
	if re {
		w.re[id] = true
	}
}

// keep narrows the list to the ids keep says stay.
func (w *paneWant) keep(keep func(transcript.EntryID) bool) {
	w.order = slices.DeleteFunc(w.order, func(id transcript.EntryID) bool {
		if keep(id) {
			return false
		}
		delete(w.in, id)
		delete(w.re, id)
		return true
	})
}

// hook is foldHook: the half before the pane consumes the Change, which
// returns the half after it.
func (w *parityWatch) hook(m *Model, ev agent.Event, ch transcript.Change, hide bool) func() {
	w.mu.Lock()
	defer w.mu.Unlock()
	r := w.rec(m.shared)
	r.folds++
	w.stats.folds++

	// A pane emptied since the last fold starts over. /clear marks the run it
	// left open, which the shadow — not yet folded past the moment of the
	// clear — holds open still.
	live := panesOf(m)
	for _, p := range live {
		want := r.want(p)
		if p.emptied == want.emptied {
			continue
		}
		*want = *newPaneWant(p.emptied)
		if p == m.main {
			want.mark, _ = openRun(r.shadow.Main)
		}
	}

	s := paritySample{ev: ev, at: m.foldClock.at}
	r.events = append(r.events, s)
	r.replay = s.at
	if got := r.shadow.Fold(ev); got != ch {
		w.fail("fold %d (%s): the TUI's model changed %+v; a model folded from the same events, %+v", r.folds, ev.Type, ch, got)
	}
	if got, want := m.shared.Seq(), r.shadow.Seq(); got != want {
		w.fail("fold %d (%s): the TUI's model is at seq %d, the events' at %d", r.folds, ev.Type, got, want)
	}

	// Which touched entries had a row before the pane consumed the fold: what
	// a touch does depends on it (X29, X30).
	var had [2]bool
	if p := live[ch.Scope]; p != nil {
		for i, id := range ch.Touched {
			had[i] = !id.IsZero() && p.ids[id] != nil
		}
	}
	return func() {
		w.mu.Lock()
		defer w.mu.Unlock()
		w.consumed(m, r, ev, ch, hide, had)
	}
}

// consumed is the half after the pane consumed the fold: the watch's own
// account of what the pane must now show, the rows the fold named, and — when
// it is due — the whole check.
func (w *parityWatch) consumed(m *Model, r *parityRec, ev agent.Event, ch transcript.Change, hide bool, had [2]bool) {
	tr := scopeOf(r.shadow, ch.Scope)
	live := panesOf(m)
	p := live[ch.Scope]
	if ch.Entries() && p != nil && tr != nil {
		want := r.want(p)
		if ch.Dropped > 0 {
			want.keep(func(id transcript.EntryID) bool { _, ok := tr.Entry(id); return ok })
		}
		for i, id := range ch.Touched {
			if id.IsZero() {
				continue
			}
			e, ok := tr.Entry(id)
			switch {
			case !ok, had[i]:
			case e.Kind == transcript.KindTool:
				want.add(id, true)
			case id == want.mark && e.Streaming:
				want.add(id, false)
			}
			if id == want.mark {
				want.mark = transcript.EntryID{}
			}
		}
		if !ch.AppendedFrom.IsZero() {
			for _, e := range tr.Range(ch.AppendedFrom, ch.AppendedTo) {
				if hide && e.Kind == transcript.KindUser {
					continue
				}
				want.add(e.ID, false)
			}
		}
		// The rows this fold named show their entries now (P1).
		for _, id := range [...]transcript.EntryID{ch.Touched[0], ch.Touched[1], ch.AppendedFrom, ch.AppendedTo} {
			if row := p.ids[id]; !id.IsZero() && row != nil {
				w.checkRow(r.folds, ev, ch.Scope, row, m.shared)
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

// whole is the whole check, P0 to P4, over every transcript and every pane
// (live, panesOf(m)).
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
	var at time.Time
	f := transcript.New(sharedOptions(func() time.Time { return at }))
	for _, s := range r.events {
		at = s.at
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

// wholePane is P1 to P4 over one pane.
func (w *parityWatch) wholePane(m *Model, r *parityRec, ev agent.Event, scope string, p *pane) {
	fold := r.folds
	tr := scopeOf(m.shared, scope)
	var shown []*entry
	for _, row := range p.rows {
		w.stats.rows++
		// P4.
		if row.local != row.id.IsZero() {
			w.fail("fold %d (%s): pane %q holds a row marked local=%v with entry %v: %+v", fold, ev.Type, scope, row.local, row.id, shownOf(row))
		}
		if !row.id.IsZero() {
			shown = append(shown, row)
		}
	}
	// The index is exactly the shown rows, one row per entry (P2's "exactly").
	if len(p.ids) != len(shown) {
		w.fail("fold %d (%s): pane %q indexes %d rows and shows %d", fold, ev.Type, scope, len(p.ids), len(shown))
	}
	for _, row := range shown {
		if p.ids[row.id] != row {
			w.fail("fold %d (%s): pane %q shows entry %v twice, or indexes another row for it", fold, ev.Type, scope, row.id)
		}
		w.checkRow(fold, ev, scope, row, m.shared)
	}
	if tr == nil {
		return
	}
	// P2: the shown rows are the expected ones, less a prefix the pane's own
	// cap took — which the trim note then says.
	want := r.want(p)
	want.keep(func(id transcript.EntryID) bool { _, ok := tr.Entry(id); return ok })
	ids := make([]transcript.EntryID, len(shown))
	for i, row := range shown {
		ids[i] = row.id
	}
	n := len(want.order) - len(ids)
	if n < 0 || !slices.Equal(want.order[n:], ids) {
		w.fail("fold %d (%s): pane %q shows entries %v, want %v less a prefix its cap took", fold, ev.Type, scope, ids, want.order)
	}
	if n > 0 && !p.trimmed {
		w.fail("fold %d (%s): pane %q shows entries %v, want %v: %d rows are missing, and no cap trimmed the pane",
			fold, ev.Type, scope, ids, want.order, n)
	}
	want.keep(func(id transcript.EntryID) bool { return p.ids[id] != nil })
	// P3: in the model's order, re-appended rows excepted.
	pos := map[transcript.EntryID]int{}
	for i, e := range tr.Entries() {
		pos[e.ID] = i
	}
	last := -1
	for _, id := range ids {
		if want.re[id] {
			continue
		}
		if pos[id] <= last {
			w.fail("fold %d (%s): pane %q shows entry %v out of the model's order: %v", fold, ev.Type, scope, id, ids)
		}
		last = pos[id]
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
