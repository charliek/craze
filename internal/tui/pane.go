package tui

import (
	"slices"
	"time"

	"github.com/charliek/craze/internal/transcript"
)

// pane is one transcript as this client displays it: the display list — every
// row a frame draws, oldest first, each with its render cache — the rows last
// painted from it and the viewport position they were painted at, and the facts
// only this client keeps (pathDirs, the `!` rows). Mutation sets dirty; Model
// paints m.vp only when this pane is the one on screen (m.cur()).
//
// A row is one of two kinds, and the list holds both in the order they were
// written, which is the order today's single slice has always shown them in
// (plan 024 §3.8):
//
//   - a shared row shows an entry of the session's shared model (m.shared),
//     and carries that entry's EntryID. The model's fold says what each event
//     changed (transcript.Change) and the pane follows it (consume): an
//     appended entry gets a row at the tail — unless it is the echo of a row
//     this client already drew for itself (hide) — a touched one is re-read in
//     place, and a dropped one leaves. Every client folding the same events
//     holds the same entries under the same ids;
//   - a local row is one this client wrote for a message of its own, at its own
//     clock, through appendLocal: a failure, a note about something it changed,
//     the optimistic user row at Enter, an ask's answer notes, a `!` row, a
//     receipt-mode sub-agent's rebuilt rows. No event carries it, so it is in no
//     model and no other client draws it. It is marked local and has no id.
//
// A local row never ends the shared model's open run (execution amendment
// X27): the run is the model's, and a chunk after a local row grows the entry
// above it, which this pane shows as the row above the local one.
//
// A pane is shared, never copied. bubbletea copies Model by value on every
// Update, and m.main and every pane in m.subs are pointers that every copy
// holds, exactly as m.subs, m.shell and m.owner already are; so a row written
// through any copy is written for all of them, and code must never rely on a
// discarded Model copy discarding its rows. A value method that writes a row
// returns the Model it wrote through, and its caller keeps that Model — the
// descendant, never an older copy (plan 024 §3.8).
type pane struct {
	// rows is the display list. A row is held by pointer, so its address is
	// its identity: it survives the append that grows the list and the trim
	// that shifts it, and the index from an id to its row never has to be
	// rebased.
	rows []*entry
	// ids finds the row that shows a shared entry, so a Change's Touched
	// entries are re-read in place in O(1). A row leaves it when it leaves the
	// list.
	ids map[transcript.EntryID]*entry
	// reappended are the shared rows appended out of the model's order: a tool
	// entry updated after its row had gone (X29). They are the only rows a
	// model trim can leave behind the front of the list (dropGone).
	reappended []*entry
	trimmed    bool
	// pathDirs is the pane's own (notePath): the directories each basename
	// has been seen in, from the tool entries this pane was given.
	pathDirs        map[string]map[string]struct{}
	renders         int
	transcriptRows  []string
	transcriptPlain []string
	yOffset         int
	atBottom        bool
	dirty           bool
	// entryCap / textBudget are 0 on main (maxEntries, unlimited text).
	entryCap   int
	textBudget int
	// The clear mark (execution amendment X30): the shared model's open stream
	// entry when /clear last emptied the list, and the length of its text then.
	// The run goes on in the model, so the first chunk into it after the clear
	// touches an entry with no row; it is shown from where the clear left it,
	// as a row of its own (a continuation, see entry.cont), which is what the
	// next chunk has always done after a /clear.
	clearOpen transcript.EntryID
	clearFrom int
	// emptied counts the times the list was emptied whole — /clear, a receipt
	// rebuild — which is what the parity test (A11) reads to know that the rows
	// appended before it are no longer this pane's to show.
	emptied int
}

// newSubPane is a sub-agent's pane, under the tighter caps.
func newSubPane() *pane { return &pane{entryCap: subMaxEntries, textBudget: subTextBudget} }

// reset empties the pane in place, keeping its caps: it is the same *pane
// m.subs and m.cur() hold, so it is emptied rather than replaced.
func (t *pane) reset() {
	*t = pane{entryCap: t.entryCap, textBudget: t.textBudget, emptied: t.emptied + 1}
}

func (m *Model) cur() *pane {
	if m.viewing != "" {
		if t := m.subs[m.viewing]; t != nil {
			return t
		}
	}
	return m.main
}

// ---------------------------------------------------------------- local rows

// appendLocal writes a local row at the tail, where it lands in order with
// everything around it, marks it local, and holds the list to its caps. It
// ends no run: the open run is the shared model's (X27).
func (t *pane) appendLocal(e entry, now time.Time) {
	e.local = true
	e.id = transcript.EntryID{}
	if e.at.IsZero() {
		e.at = now
	}
	if e.end.IsZero() {
		e.end = e.at
	}
	t.push(&e)
	t.enforceCaps()
}

// appendEntry is appendLocal: a row of the caller's own making.
func (t *pane) appendEntry(e entry, now time.Time) { t.appendLocal(e, now) }

// push puts one row at the tail of the list, to be drawn.
func (t *pane) push(e *entry) {
	e.dirty = true
	t.rows = append(t.rows, e)
	t.dirty = true
}

// --------------------------------------------------------------- shared rows

// consume is one fold's Change, for the pane that shows its Scope's transcript
// tr (plan 024 §3.8): the entries the model dropped leave (X28), the touched
// ones are re-read where they stand, and the appended ones get rows at the
// tail, in order — except those of kind hide (zero for none), which the pane
// already shows: the user row of a started whose echo this client drew itself
// (X26), a todo note the pane's own dedupe does not owe (X31). Then, if a row
// was added, the pane's own caps apply, as they always have on an append and
// only then: a chunk that grows a row trims nothing.
func (t *pane) consume(ch transcript.Change, tr *transcript.Transcript, hide transcript.Kind) {
	if ch.Dropped > 0 {
		t.dropGone(tr)
	}
	rows := len(t.rows)
	for _, id := range ch.Touched {
		if !id.IsZero() {
			t.touch(tr, id)
		}
	}
	if !ch.AppendedFrom.IsZero() {
		for _, e := range tr.Range(ch.AppendedFrom, ch.AppendedTo) {
			if hide != 0 && e.Kind == hide {
				continue
			}
			t.showNew(tr, e, &entry{})
		}
	}
	if len(t.rows) > rows {
		t.enforceCaps()
	}
}

// showNew gives a shared entry a row at the tail. r is the row, blank or a
// continuation (touch) whose own fields are set already.
func (t *pane) showNew(tr *transcript.Transcript, e *transcript.Entry, r *entry) {
	r.show(e, tr)
	if t.ids == nil {
		t.ids = make(map[transcript.EntryID]*entry)
	}
	t.ids[e.ID] = r
	t.push(r)
	t.sawTool(e)
}

// touch re-reads one entry the fold replaced. With a row, the row shows it
// now. Without one — /clear took it, the pane's own cap dropped it, or the
// pane was made after it — the rule is by kind (X29): a tool entry is shown
// again at the tail, as an update to a cleared tool row always has been; the
// run that was open at /clear continues in a row of its own (X30); anything
// else was closed by this fold, and a row nobody is showing stays unshown.
func (t *pane) touch(tr *transcript.Transcript, id transcript.EntryID) {
	e, ok := tr.Entry(id)
	if !ok {
		return
	}
	if r := t.ids[id]; r != nil {
		r.show(e, tr)
		t.dirty = true
		t.sawTool(e)
		return
	}
	switch {
	case e.Kind == transcript.KindTool:
		r := &entry{}
		t.showNew(tr, e, r)
		t.reappended = append(t.reappended, r)
	case id == t.clearOpen && e.Streaming:
		// Dated at this chunk, which is the run's end now, and drawn from
		// where the clear left the run's text.
		t.showNew(tr, e, &entry{cont: true, contFrom: t.clearFrom, at: e.End})
	}
	if id == t.clearOpen {
		t.clearOpen, t.clearFrom = transcript.EntryID{}, 0
	}
}

// sawTool feeds a tool entry's payload to the pane's basename map: every tool
// entry the pane is given, appended or updated, as every tool event used to be.
func (t *pane) sawTool(e *transcript.Entry) {
	if e.Kind == transcript.KindTool && e.Tool != nil {
		t.notePath(*e.Tool)
	}
}

// dropGone is a model trim reaching the pane (execution amendment X28). The
// model drops from its front, and the pane's shared rows are in its order but
// for the re-appended ones, so the rows that go are a prefix of the list: the
// longest one whose shared rows the model no longer holds — the local rows
// among them are older than an entry that went, which the front trim of one
// list would have taken first — and then any re-appended row whose entry went.
// The trim note leads the list from then on.
func (t *pane) dropGone(tr *transcript.Transcript) {
	k := -1
	for i, r := range t.rows {
		if r.id.IsZero() {
			continue
		}
		if _, held := tr.Entry(r.id); held {
			break
		}
		k = i
	}
	if k >= 0 {
		t.dropFirst(k + 1)
	}
	for i := 0; i < len(t.reappended); {
		r := t.reappended[i]
		if _, held := tr.Entry(r.id); held {
			i++
			continue
		}
		t.removeRow(r)
	}
}

// removeRow takes one row out of the list, wherever it stands.
func (t *pane) removeRow(r *entry) {
	if i := slices.Index(t.rows, r); i >= 0 {
		// Delete clears the vacated last slot, which would otherwise keep a
		// row reachable.
		t.rows = slices.Delete(t.rows, i, i+1)
	}
	t.forget(r)
	t.trimmed = true
	t.dirty = true
}

// forget takes a row that left the list out of the indexes over it.
func (t *pane) forget(r *entry) {
	if r.id.IsZero() {
		return
	}
	if t.ids[r.id] == r {
		delete(t.ids, r.id)
	}
	if i := slices.Index(t.reappended, r); i >= 0 {
		t.reappended = slices.Delete(t.reappended, i, i+1)
	}
}

// ---------------------------------------------------------------- the caps

// enforceCaps holds the list to its caps — maxEntries rows on main, and
// subMaxEntries rows and subTextBudget bytes of row text on a sub-agent's pane —
// counting local and shared rows alike and dropping from the front whatever
// their kind; the trim note then leads the list (setViewportContent). The caps
// are the pane's and not the model's: they bound what a frame shows, and the
// model's own bounds reach the pane as the entries it drops (dropGone).
func (t *pane) enforceCaps() {
	maxE := t.entryCap
	if maxE <= 0 {
		maxE = maxEntries
	}
	if len(t.rows) > maxE {
		t.dropFirst(len(t.rows) - maxE)
	}
	if t.textBudget > 0 {
		for t.rawTextLen() > t.textBudget && len(t.rows) > 1 {
			t.dropFirst(1)
		}
	}
}

func (t *pane) dropFirst(n int) {
	if n <= 0 || n > len(t.rows) {
		return
	}
	for _, r := range t.rows[:n] {
		t.forget(r)
	}
	kept := copy(t.rows, t.rows[n:])
	// The vacated tail would otherwise keep the rows it held reachable.
	clear(t.rows[kept:])
	t.rows = t.rows[:kept]
	t.trimmed = true
	t.dirty = true
}

func (t *pane) rawTextLen() int {
	n := 0
	for _, e := range t.rows {
		n += len(e.text)
	}
	return n
}

// ---------------------------------------------------------------- /clear

// clearRows is /clear on the list: every row goes, local and shared alike, with
// the trim note and the paths only those rows had shown. The shared model is
// not cleared — it is the session's — so the entries stay where they are, and
// a later update to one the clear took finds no row (touch).
func (t *pane) clearRows() {
	t.rows = nil
	t.ids = nil
	t.reappended = nil
	t.trimmed = false
	t.pathDirs = nil
	t.clearOpen, t.clearFrom = transcript.EntryID{}, 0
	t.emptied++
	t.dirty = true
}

// clearTranscript is /clear: the main pane's list empties, and so does every
// cache this client keyed off those rows.
func (m *Model) clearTranscript() {
	t := m.main
	t.clearRows()
	// The run still streaming goes on in the model; the clear mark is where
	// its text stood, so what comes after the clear is shown on its own.
	if m.shared != nil {
		t.clearOpen, t.clearFrom = openRun(m.shared.Main)
	}
	// The todo-note dedupe is this client's, for what its list shows: a
	// repeated todos event after /clear notes "N planned" again (X31).
	m.todoPlanned = 0
	m.todoDone = false
	// "the plan above" is gone, so there is nothing left to offer.
	m.retirePlanOffer()
	// The pending shell context is keyed off these entries as much as any
	// cache is: it describes `!` rows that are no longer on screen, and a
	// /clear is the user saying that conversation is over (plan 022 §3.6).
	m.dropShellContext()
}

// openRun is tr's open stream entry and the length of its text so far, or the
// zero id when no run is open.
func openRun(tr *transcript.Transcript) (transcript.EntryID, int) {
	if !tr.StreamOpen() {
		return transcript.EntryID{}, 0
	}
	es := tr.Entries()
	if n := len(es); n > 0 && es[n-1].Streaming {
		return es[n-1].ID, len(tr.Tail())
	}
	return transcript.EntryID{}, 0
}

// detach lets go of the entries of a shared model that has been replaced: the
// rows stay on screen, but they are no event's of the session now folding, so
// they are this client's alone — local — and no id of the new model can find
// one of them.
func (t *pane) detach() {
	for _, r := range t.rows {
		if !r.id.IsZero() {
			r.id = transcript.EntryID{}
			r.local = true
		}
	}
	t.ids = nil
	t.reappended = nil
	t.clearOpen, t.clearFrom = transcript.EntryID{}, 0
}

// ---------------------------------------------------------------- local rows
//
// The wrappers that take no time are the local path into the main pane: each
// is a row the client writes for a message of its own, stamped at its own
// clock (plan 024 §2.4's first list). The rows drawn from an event are the
// shared model's.

// appendEntry is a local row of the caller's own making: a `!` row.
func (m *Model) appendEntry(e entry) { m.main.appendLocal(e, m.now()) }

// addUser is the optimistic user row: text this client sent, drawn at Enter,
// before any event about that turn exists. The started the engine publishes
// for it is its echo, and is hidden (applyEvent).
func (m *Model) addUser(text string) {
	m.main.appendLocal(entry{kind: entryUser, text: userText(text)}, m.now())
}

// addNote is a local note: a setting this client changed, an ask it answered,
// or another client's answer to a card this client had (plan 024 §3.3).
func (m *Model) addNote(text string) {
	if text == "" {
		return
	}
	m.main.appendLocal(entry{kind: entryNote, text: text}, m.now())
}

// addError is a local failure: something this client asked for that did not
// happen.
func (m *Model) addError(text string) {
	if text == "" {
		return
	}
	m.main.appendLocal(entry{kind: entryError, text: text}, m.now())
}
