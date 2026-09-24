package tui

import "time"

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
//   - a shared row is one the session's events drew, through appendShared:
//     every client folding the same events draws the same one. appendShared is
//     also where the echo rule is decided: a shared row whose local twin this
//     client already drew is given no row at all (hide);
//   - a local row is one this client wrote for a message of its own, at its own
//     clock, through appendLocal: a failure, a note about something it changed,
//     the optimistic user row at Enter, an ask's answer notes, a `!` row, a
//     receipt-mode sub-agent's rebuilt rows. No event carries it, so no other
//     client draws it. It is marked local.
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
	// that shifts it, and an index from an id to its row never has to be
	// rebased.
	rows    []*entry
	trimmed bool
	// pathDirs is the pane's own (notePath): the directories each basename
	// has been seen in, from the tool rows this pane was given.
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
	// sharedSeen counts the shared rows this pane has been given, hidden ones
	// included, and clearMark is what it stood at when /clear last emptied the
	// list: the shared rows numbered below the mark are the ones /clear took,
	// so an update to one of them finds no row because the user cleared it,
	// not because it was never drawn (clearRows).
	sharedSeen int
	clearMark  int

	// The old fold's, while it still writes through the pane (C5c removes them).
	toolLine   map[string]int
	streamOpen bool
}

// newSubPane is a sub-agent's pane, under the tighter caps.
func newSubPane() *pane { return &pane{entryCap: subMaxEntries, textBudget: subTextBudget} }

// reset empties the pane in place, keeping its caps: it is the same *pane
// m.subs and m.cur() hold, so it is emptied rather than replaced.
func (t *pane) reset() { *t = pane{entryCap: t.entryCap, textBudget: t.textBudget} }

func (m *Model) cur() *pane {
	if m.viewing != "" {
		if t := m.subs[m.viewing]; t != nil {
			return t
		}
	}
	return m.main
}

// appendLocal writes a local row at the tail, where it lands in order with
// everything around it, and marks it local.
func (t *pane) appendLocal(e entry, now time.Time) {
	e.local = true
	t.appendEntry(e, now)
}

// appendShared writes a row an event drew at the tail — unless hide says it is
// the echo of a row this client already drew for itself: the started of the
// turn Submit handed back, whose optimistic user row Enter put on screen. The
// local twin is the display then, and the shared row is given none. It is still
// counted (sharedSeen): the session holds that entry whether or not this client
// shows it.
//
// Nothing else of an append happens for a hidden row either — no run closes and
// nothing trims — which is exactly what skipping that started has always done.
func (t *pane) appendShared(e entry, now time.Time, hide bool) {
	t.sharedSeen++
	if hide {
		return
	}
	e.local = false
	t.appendEntry(e, now)
}

// appendEntry puts one row at the tail of the list as it is marked, holds the
// list to its caps, and is where an open run ends: a note or a tool row between
// two chunks means they are not one run, and a run that is not the last row can
// never be closed. That last rule is the old fold's, and it ends the run for a
// row of either kind (C5c keeps it for shared rows only, plan 024 §3.8).
func (t *pane) appendEntry(e entry, now time.Time) {
	e.dirty = true
	if e.at.IsZero() {
		e.at = now
	}
	if e.end.IsZero() {
		e.end = e.at
	}
	t.endRun(e.at)
	t.rows = append(t.rows, &e)
	t.enforceCaps()
	t.dirty = true
}

// enforceCaps holds the list to its caps — maxEntries rows on main, and
// subMaxEntries rows and subTextBudget bytes of row text on a sub-agent's pane —
// counting local and shared rows alike and dropping from the front whatever
// their kind; the trim note then leads the list (setViewportContent). The caps
// are the pane's and not the fold's: they bound what a frame shows, which is
// independent of what the session's model retains (plan 024 §3.8).
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
	kept := copy(t.rows, t.rows[n:])
	// The vacated tail would otherwise keep the rows it held reachable.
	clear(t.rows[kept:])
	t.rows = t.rows[:kept]
	t.trimmed = true
	t.dirty = true
	t.rebaseToolLine(n)
}

func (t *pane) rawTextLen() int {
	n := 0
	for _, e := range t.rows {
		n += len(e.text)
	}
	return n
}

// clearRows is /clear on the list: every row goes, local and shared alike, with
// the trim note and the paths only those rows had shown, and the clear is
// marked (clearMark).
func (t *pane) clearRows() {
	t.rows = nil
	t.trimmed = false
	t.pathDirs = nil
	t.clearMark = t.sharedSeen
	t.dirty = true
}

// clearTranscript is /clear: the main pane's list empties, and so does every
// cache this client keyed off those rows.
func (m *Model) clearTranscript() {
	t := m.main
	t.clearRows()
	// The old fold's: it forgets the tool index and the open run with the rows,
	// so an update to a tool the clear took appends it again at the tail, and
	// the next chunk starts a row of its own.
	t.toolLine = nil
	t.streamOpen = false
	// The todo-note dedupe is this client's, for what its list shows: a
	// repeated todos event after /clear notes "N planned" again.
	m.todoPlanned = 0
	m.todoDone = false
	// "the plan above" is gone, so there is nothing left to offer.
	m.retirePlanOffer()
	// The pending shell context is keyed off these entries as much as any
	// cache is: it describes `!` rows that are no longer on screen, and a
	// /clear is the user saying that conversation is over (plan 022 §3.6).
	m.dropShellContext()
}

// ---------------------------------------------------------------- local rows
//
// The wrappers that take no time are the local path into the main pane: each
// is a row the client writes for a message of its own, stamped at its own
// clock (plan 024 §2.4's first list). The rows drawn from an event are the old
// fold's, in transcript.go, and go through appendShared.

// appendEntry is a local row of the caller's own making: a `!` row.
func (m *Model) appendEntry(e entry) { m.main.appendLocal(e, m.now()) }

// addUser is the optimistic user row: text this client sent, drawn at Enter,
// before any event about that turn exists. The started the engine publishes
// for it is its echo, and is hidden (applyTurnStarted).
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
