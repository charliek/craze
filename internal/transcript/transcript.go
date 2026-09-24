package transcript

import (
	"sync"
	"time"
	"unicode/utf8"

	"github.com/charliek/craze/internal/agent"
)

// Transcript is one conversation — the main session's, or one sub-agent's —
// as every client agrees on it: its entries, oldest first, and the little
// state that decides what the next event does to them. Its read methods take
// the model's mutex; its entries are immutable (see Entry).
//
// # Storage (plan 024 §3.4: no shifting, no scanning)
//
// The entries are a deque: a backing slice and a head index. Trimming advances
// the head (O(1) per entry); the live range is compacted to the front of the
// same backing array once the head has passed half of it, so the copy is
// amortised O(1) per trim — and it rewrites nothing a reader holds, because
// every reader copies the pointer slice rather than keeping it.
//
// Every entry has an ordinal, fixed when it is appended and stable across
// trims and compactions (ordinal - base is its index). slot maps an EntryID to
// its ordinal and tools a tool id to the EntryID of the entry it names, so an
// update finds its row in O(1) and a trim forgets it in O(1).
//
// bytes is the retained-bytes counter — Entry.Bytes added on append, adjusted
// on replace, subtracted on trim — never a scan.
//
// # The stream builder
//
// The open stream entry's text lives in buf, which only this transcript ever
// touches: a chunk appends to it, Tail copies out of it, and a closing run
// copies its tail into the replacing entry's Text. It grows to 2 × StreamText;
// a chunk that would take it past that keeps the last StreamText bytes, moved
// to the front in place — one StreamText copy per StreamText of input. runLen
// is the run's length, saturated just past the cap: while the run fits the
// cap buf is all of it, and once it does not, its tail ("…" and the run's
// last bytes from a rune start, today's capEntryText) is found in buf when it
// is read (tail), never by a chunk. buf keeps its capacity from one run to
// the next (bufReset), so each live transcript retains at most 2 ×
// StreamText of builder; a child's is let go when its roster row finishes.
//
// # The open run's accounting (execution amendment X25)
//
// A streamed entry, open or closed, accounts min(bytes streamed, StreamText)
// (Entry.Bytes): a chunk adds its length to runLen, and the run accounts
// runBytes. That depends on byte counts alone — not on where the tail's cut
// lands in a multi-byte rune — so a chunk does no rune work, and a restored
// model's placeholder for a run its window omitted follows the first model's
// entry exactly (X23).
//
// # The open run's end (execution amendment X24)
//
// A chunk into the open run allocates nothing: it appends to buf and records
// its stamp in openEnd, and the stored open entry — the one marked Streaming —
// is left as it was when the run opened. So while a run is open that stored
// pointer's End, Cut and Bytes are stale; the transcript holds the truth
// (openEnd, runLen) and the entry is never handed out as stored. Every reader
// gets a fresh copy carrying the current end, cut and accounting (current:
// Entries, Entry, the cut, and through the cut History, State and the
// snapshot, which also copies the tail in as Text), and the run's closing
// stores a new entry with the final end and tail, as before. This took V7's
// one allocation per chunk — a ~200-byte Entry built only to carry the new
// End — off the fold's hot path.
type Transcript struct {
	mu    *sync.Mutex // the model's
	model *Model
	agent string // "" for the main transcript, else the child's id

	maxEntries, maxBytes, streamCap int

	ents []*Entry
	head int
	base int // the ordinal of ents[0]
	slot map[EntryID]int
	// tools is the tool index: a tool id to the entry that holds it.
	tools map[string]EntryID

	bytes int
	// grown is what tool updates in place have added to bytes since the
	// budget was last enforced (trimBytes). An update in place never trims
	// (upsertTool), so between it and the next append or chunk the transcript
	// may hold more than maxBytes over more than one entry — by exactly grown:
	// bytes - grown fits the budget, or one entry is all there is
	// (checkInvariants).
	grown      int
	trimmed    bool
	streamOpen bool
	buf        []byte
	// runLen is the open run's length — every byte streamed into it, whether
	// its entry is held or a restored window omitted it (omittedRun) —
	// saturated at StreamText + 1: all that is read of it is whether the run
	// is longer than the cap (runCut) and min(runLen, StreamText), what the
	// run accounts (runBytes, X25). 0 while no run is open.
	runLen int
	// openEnd is the open run's end — its last chunk's stamp — while a run
	// is open (X24): the stored Streaming entry's End is its first chunk's.
	openEnd time.Time

	// The todo-note dedupe (the TUI's todoPlanned / todoDone): the largest
	// list a "planned" note was written for, and whether the "done" note has
	// been written for the list as it stands. The main transcript's only.
	todoPlanned int
	todoDone    bool

	// The window, set only by Restore from a snapshot that omitted older
	// entries to fit its byte budget (plan 024 §3.5), and carried forward by
	// this model's own snapshots. windowed and dropped say so and how many.
	windowed bool
	dropped  int
	// The ledger (execution amendment X23): one payload-free placeholder per
	// entry the window omitted and the first model still holds, oldest first,
	// live from lhead — its retained bytes as the first model accounts them,
	// and a tool's id. The placeholders sit at the head of the transcript,
	// before every real entry, and are counted in its entry cap and byte
	// budget exactly as the first model's real rows are (count, bytes), so
	// the trim drops them — oldest first, one at a time — exactly when the
	// first model drops the real rows. No reader ever sees one: live(), and so
	// Entries, Entry, Len, History, State and the pane, hold the real entries
	// only. ptools indexes the placeholders that name a tool (the tool id to
	// its index in ledger): an update to one draws nothing and re-accounts the
	// placeholder's bytes to the new payload's, as the first model re-accounts
	// its row; the id is forgotten when the placeholder is dropped, so a later
	// update appends a new row on both models. The ledger is never compacted
	// (only Restore fills it; it shrinks from the head and is let go when
	// empty), and a cut copies its live part.
	//
	// omittedRun is the kind of the open run's entry when the window dropped
	// that too (a child that fitted nothing, mid-stream): its placeholder is
	// the ledger's last, its next chunks of that kind draw nothing and grow
	// runLen, the placeholder accounting runBytes as the first model's open
	// entry does (X25) — exactly, the snapshot's record being the run's own
	// figure at the cut — and whatever ends the run clears it.
	ledger     []Omitted
	lhead      int
	ptools     map[string]int
	omittedRun Kind
}

func newTranscript(m *Model, agentID string, maxEntries, maxBytes int) *Transcript {
	return &Transcript{
		mu:         &m.mu,
		model:      m,
		agent:      agentID,
		maxEntries: maxEntries,
		maxBytes:   maxBytes,
		streamCap:  m.bounds.StreamText,
		slot:       make(map[EntryID]int),
		tools:      make(map[string]EntryID),
	}
}

// ---------------------------------------------------------------- read side

// Agent is the child's id this transcript belongs to, "" for the main one.
func (t *Transcript) Agent() string { return t.agent }

// Entries is every entry, oldest first: a copy of the pointer slice, so
// nothing the caller holds changes when the model folds on. The open stream
// entry (Streaming) is a copy carrying the run's current End with an empty
// Text; its text is Tail.
func (t *Transcript) Entries() []*Entry {
	t.mu.Lock()
	defer t.mu.Unlock()
	es := append([]*Entry(nil), t.live()...)
	if n := len(es); n > 0 && es[n-1].Streaming {
		es[n-1] = t.current(es[n-1])
	}
	return es
}

// Range is the entries from from to to, both included, oldest first — a
// Change's appended range (AppendedFrom..AppendedTo), which is how a client
// reads what one fold appended without copying the whole transcript. It is
// materialised like Entries: a copy of the pointers, the open stream entry a
// copy carrying the run's current End with an empty Text (its text is Tail).
// It is O(the range), and nil when the transcript no longer holds either end
// or to comes before from.
func (t *Transcript) Range(from, to EntryID) []*Entry {
	t.mu.Lock()
	defer t.mu.Unlock()
	first, fe := t.lookup(from)
	last, le := t.lookup(to)
	if fe == nil || le == nil || last < first {
		return nil
	}
	es := append([]*Entry(nil), t.ents[first-t.base:last-t.base+1]...)
	if n := len(es); es[n-1].Streaming {
		es[n-1] = t.current(es[n-1])
	}
	return es
}

// Entry is the entry id names, if the transcript still holds it.
func (t *Transcript) Entry(id EntryID) (*Entry, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	_, e := t.lookup(id)
	if e == nil {
		return nil, false
	}
	return t.current(e), true
}

// Len is how many entries the transcript holds.
func (t *Transcript) Len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.len()
}

// Tail is the open stream entry's text so far — at most StreamText bytes, led
// by "…" once the beginning was dropped — or "" when no run is open, or while
// the open run is a placeholder (omittedRun, X23): the window this model was
// restored from dropped its entry along with the rest of the child, so buf
// holds nothing of it and runLen is the ledger placeholder's own bookkeeping,
// not text — a reader must see nothing, the same "" that Entries and History
// already give it (r8: buf empty and runLen past the cap once made this
// "…"). It copies: the pane calls it once per chunk, the fold never does.
func (t *Transcript) Tail() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.streamOpen || t.omittedRun != 0 {
		return ""
	}
	return t.tail()
}

// Trimmed reports whether the transcript has dropped entries off its front;
// a client draws TrimmedNote above it for Trimmed || Windowed.
func (t *Transcript) Trimmed() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.trimmed
}

// Windowed reports whether the transcript was restored from a snapshot that
// omitted its older entries to fit the snapshot's byte budget (plan 024
// §3.5); a client draws TrimmedNote above it for Trimmed || Windowed.
func (t *Transcript) Windowed() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.windowed
}

// StreamOpen reports whether a run is open: the next chunk of the last entry's
// kind grows that entry instead of starting one.
func (t *Transcript) StreamOpen() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.streamOpen
}

// Bytes is the retained bytes the transcript accounts for, a restored
// window's placeholders included (X23): what its byte budget is held to.
func (t *Transcript) Bytes() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.bytes
}

// ------------------------------------------------------ the deque, unlocked

// current is e as a reader may see it: e itself, unless e is the open stream
// entry, whose stored End, Cut and Bytes a chunk leaves stale (X24) — then a
// fresh copy carrying the run's end, whether it is cut, and what it accounts.
// Its Text stays empty; the cut puts the tail in.
func (t *Transcript) current(e *Entry) *Entry {
	if !e.Streaming {
		return e
	}
	c := *e
	c.End = t.openEnd
	c.Cut = t.runCut()
	c.Bytes = t.bytesOf(e)
	return &c
}

// bytesOf is what e accounts now: its Bytes, or for the open stream entry,
// whose stored Bytes a chunk leaves stale (X24), what the run accounts,
// min(bytes streamed, StreamText) (X25) — read before the builder is reset.
func (t *Transcript) bytesOf(e *Entry) int {
	if e.Streaming {
		return t.runBytes() + toolBytes(e.Tool) + planBytes(e.Plan)
	}
	return e.Bytes
}

func (t *Transcript) len() int { return len(t.ents) - t.head }

// held is how many placeholders the ledger holds (X23).
func (t *Transcript) held() int { return len(t.ledger) - t.lhead }

// count is what the entry cap counts: the real entries and the placeholders
// before them — as many as the first model's real rows (X23).
func (t *Transcript) count() int { return t.len() + t.held() }

// live is the entries, oldest first, aliasing the backing array: the caller
// holds the lock and copies before letting go of it.
func (t *Transcript) live() []*Entry { return t.ents[t.head:] }

func (t *Transcript) lastEntry() *Entry {
	if t.len() == 0 {
		return nil
	}
	return t.ents[len(t.ents)-1]
}

// lookup finds the entry id names: its ordinal, and the entry, nil when the
// transcript no longer holds it.
func (t *Transcript) lookup(id EntryID) (int, *Entry) {
	ord, ok := t.slot[id]
	if !ok {
		return 0, nil
	}
	i := ord - t.base
	if i < t.head || i >= len(t.ents) {
		return 0, nil
	}
	return ord, t.ents[i]
}

// push appends e, which already carries its Bytes, under a fresh id.
func (t *Transcript) push(e *Entry) {
	e.ID = t.model.newID()
	ord := t.base + len(t.ents)
	t.ents = append(t.ents, e)
	t.slot[e.ID] = ord
	t.bytes += e.Bytes
	t.model.noteAppended(t, ord)
}

// replace swaps the entry at ord for ne, a new value with the same ID, and
// re-accounts its bytes.
func (t *Transcript) replace(ord int, ne *Entry) {
	i := ord - t.base
	t.bytes += ne.Bytes - t.bytesOf(t.ents[i])
	t.ents[i] = ne
	t.model.noteTouched(t, ne.ID)
}

func (t *Transcript) replaceLast(ne *Entry) { t.replace(t.base+len(t.ents)-1, ne) }

// trim is today's trimEntries over the deque: the entry cap, then the byte
// budget, each dropping from the front. Both count a restored window's
// placeholders (X23), which are the front.
func (t *Transcript) trim() {
	for t.count() > t.maxEntries {
		t.dropHead()
	}
	t.trimBytes()
}

// trimBytes drops from the front until the retained bytes fit the budget,
// never dropping the last entry (today's guard). It runs after every append
// and every chunk, never after a tool update in place (upsertTool), and it is
// where the budget is enforced again: grown starts over.
func (t *Transcript) trimBytes() {
	for t.bytes > t.maxBytes && t.count() > 1 {
		t.dropHead()
	}
	t.grown = 0
}

// dropHead is today's dropFirst(1): the oldest entry goes, and so does the
// tool index's name for it, so a later update to that id appends a new row. A
// named tool's row going takes that tool's last state out of the state
// projection (State().Tools), so the fold's Change says the state changed (r2
// finding 6). A restored window's placeholders are older than every real
// entry, so they go first (dropPlaceholder).
func (t *Transcript) dropHead() {
	if t.held() > 0 {
		t.dropPlaceholder()
		return
	}
	e := t.ents[t.head]
	t.ents[t.head] = nil
	t.head++
	t.bytes -= t.bytesOf(e)
	delete(t.slot, e.ID)
	if e.Kind == KindTool && e.Tool != nil && e.Tool.ID != "" {
		if id, ok := t.tools[e.Tool.ID]; ok && id == e.ID {
			delete(t.tools, e.Tool.ID)
		}
		t.model.fc.state = true
	}
	t.trimmed = true
	t.model.noteDropped(t)
	// Compact once the dead prefix is half the backing array: the live range
	// moves to the front in place. The copy is O(live) once per O(live) trims.
	if t.head >= 32 && 2*t.head >= len(t.ents) {
		n := copy(t.ents, t.ents[t.head:])
		clear(t.ents[n:])
		t.ents = t.ents[:n]
		t.base += t.head
		t.head = 0
	}
}

// dropPlaceholder is dropHead for the oldest placeholder (X23): the first
// model drops the real row it stands for at this same fold. Its bytes leave
// the counter, a tool's id leaves the index — so a later update to it appends
// a new row, as it does on the first model, which forgot the id with its row
// — and the transcript is Trimmed, as the first model's is.
//
// How the fold reports it: a placeholder is not an entry any reader was given
// (a client's display list never held one), so it is not counted in
// Change.Dropped, which says how many of the entries a client holds left the
// front; and the Change's Scope is not set by it. A placeholder that named a
// tool sets Change.State, as the first model's dropped tool row does: the
// state projection here never held that tool (State().Tools holds real rows
// only), but "may have changed" stays the conservative answer and matches the
// first model's Change.
func (t *Transcript) dropPlaceholder() {
	i := t.lhead
	r := t.ledger[i]
	t.ledger[i] = Omitted{} // a cut copies the ledger, so nothing else holds it
	t.lhead++
	t.bytes -= r.Bytes
	if r.Tool != "" {
		if j, ok := t.ptools[r.Tool]; ok && j == i {
			delete(t.ptools, r.Tool)
		}
		t.model.fc.state = true
	}
	t.trimmed = true
	if t.lhead == len(t.ledger) {
		t.ledger, t.lhead, t.ptools = nil, 0, nil
	}
}

// ---------------------------------------------------------- the builder

// streamed adds a chunk of n bytes to the open run's length, saturating just
// past the cap: nothing reads more than whether it is past (runLen).
func (t *Transcript) streamed(n int) { t.runLen = min(t.runLen+n, t.streamCap+1) }

// runBytes is what the open run accounts (X25): min(bytes streamed,
// StreamText).
func (t *Transcript) runBytes() int { return min(t.runLen, t.streamCap) }

// runCut reports whether the open run is longer than the cap, so its tail is
// led by "…" and its entry is Cut.
func (t *Transcript) runCut() bool { return t.runLen > t.streamCap }

// tail is the open run's text as today's capEntryText(whole run, StreamText)
// gives it, found here — on read, never by a chunk. While the run fits the
// cap buf is all of it. Past the cap the tail is "…" and buf's last
// StreamText − len("…") bytes, the cut moved forward to a rune start (or to
// the end, when there is none). When buf is not all of the run it holds the
// run's last StreamText bytes or more (bufAppend's compaction keeps them), or
// — restored from a snapshot — what came after the first model's cut, and
// the whole run's cut never moves back past that one; so the cut, clamped to
// buf's start, and the scan land where they would in the whole run
// (TestTheBuilderKeepsTodaysTail, TestRestoreContinuesEveryContinuation-
// State). The scan is at most StreamText bytes, the order of the copy it
// precedes: a run of continuation bytes (r2 finding 4) costs a read that, and
// a chunk nothing.
func (t *Transcript) tail() string {
	if !t.runCut() {
		return string(t.buf)
	}
	start := max(len(t.buf)-(t.streamCap-len(ellipsis)), 0)
	for start < len(t.buf) && !utf8.RuneStart(t.buf[start]) {
		start++
	}
	return ellipsis + string(t.buf[start:])
}

// bufAppend adds a chunk to the open run's builder.
func (t *Transcript) bufAppend(s string) {
	limit := 2 * t.streamCap
	if len(t.buf)+len(s) <= limit {
		t.bufReserve(len(t.buf) + len(s))
		t.buf = append(t.buf, s...)
		return
	}
	// Past 2 × StreamText — so the run is past the cap, and its tail is in
	// the last StreamText bytes: keep those of buf+s, at the front of the
	// same array. Nothing aliases buf, so moving bytes in place is safe; the
	// copy is at most StreamText bytes, and the next one is at least
	// StreamText bytes of input away.
	keep := t.streamCap
	if len(s) >= keep {
		t.buf = t.buf[:0]
		t.bufReserve(keep)
		t.buf = append(t.buf, s[len(s)-keep:]...)
	} else {
		from := keep - len(s)
		drop := len(t.buf) - from
		n := copy(t.buf, t.buf[drop:])
		t.buf = append(t.buf[:n], s...)
	}
}

// bufReserve grows buf's capacity to at least need, doubling, but never past
// 2 × StreamText (need itself never exceeds it).
func (t *Transcript) bufReserve(need int) {
	if need <= cap(t.buf) {
		return
	}
	nc := max(2*cap(t.buf), need, 256)
	if limit := 2 * t.streamCap; nc > limit {
		nc = max(limit, need)
	}
	nb := make([]byte, len(t.buf), nc)
	copy(nb, t.buf)
	t.buf = nb
}

// bufReset empties the builder for the next run and keeps its capacity: the
// next run of this transcript reuses it rather than growing a new one (r2
// finding 7), which is safe because nothing ever aliases buf (Tail and a
// closing run both copy out of it). So a live transcript retains at most
// 2 × StreamText of builder once one long run has closed; a child's is let go
// when its roster row finishes (bufRelease) or with the whole transcript when
// the row is evicted, since children are where transcripts are many. The
// run's length starts over with it.
func (t *Transcript) bufReset() {
	t.buf = t.buf[:0]
	t.runLen = 0
}

// bufRelease lets the builder's capacity go. The run must be closed.
func (t *Transcript) bufRelease() {
	t.bufReset()
	t.buf = nil
}

// capText is today's capEntryText with the cap as a parameter: s whole when it
// fits, else "…" and the last cap - len("…") bytes, cut forward to a rune
// start. The builder's tail equals capText(the whole run, StreamText) at every
// chunk (TestTheBuilderKeepsTodaysTail).
func capText(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	keep := limit - len(ellipsis)
	if keep < 0 {
		keep = 0
	}
	start := len(s) - keep
	if start < 0 {
		start = 0
	}
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	return ellipsis + s[start:]
}

// --------------------------------------------- the fold's mutations, unlocked
//
// These are internal/tui/transcript.go's mutation methods (:121-527 at plan
// 024's baseline), ported onto the deque, the builder and the accounting with
// their rules unchanged. Each takes the time the TUI's took: a stamp the
// caller has already resolved (the event's At, else Options.Clock).

// appendEntry is the only way a non-stream entry reaches the transcript, so it
// is also where an open run ends: a note or a tool row between two chunks
// means they are not one run.
func (t *Transcript) appendEntry(e *Entry, now time.Time) {
	if e.At.IsZero() {
		e.At = now
	}
	if e.End.IsZero() {
		e.End = e.At
	}
	t.endRun(e.At)
	e.Bytes = entryBytes(e, t.streamCap)
	t.push(e)
	t.trim()
}

// endRun ends the open run, and is today's endRun plus the builder: the open
// entry is replaced by one carrying its tail as Text, and a thought run is
// closed at at — left as it was when at is zero (today's rule).
func (t *Transcript) endRun(at time.Time) {
	open := t.streamOpen
	t.streamOpen = false
	if !open {
		return
	}
	if t.omittedRun != 0 {
		// A restored window dropped the open run's entry: the run ends here
		// as it does on the first client, with no entry to close. Its
		// placeholder stays, at the bytes it accounts: the first model's
		// closed entry accounts what its open one did (X25).
		t.omittedRun = 0
		t.bufReset()
		return
	}
	last := t.lastEntry()
	if last == nil || !last.Streaming {
		// Unreachable: an open run is always the last entry, and only a
		// stream entry opens one. Nothing to materialise.
		t.bufReset()
		return
	}
	ne := *last
	ne.Streaming = false
	ne.Text = t.tail()
	ne.Cut = t.runCut()
	ne.End = t.openEnd
	if ne.Kind == KindThought && ne.Open {
		ne.Open = false
		if !at.IsZero() {
			ne.End = at
		}
	}
	// What it accounts is what the open entry did (X25): StreamText when it
	// is Cut, else its whole text, which is the run.
	ne.Bytes = entryBytes(&ne, t.streamCap)
	// replaceLast re-accounts from the run the builder still holds (bytesOf),
	// so the builder is reset only after it.
	t.replaceLast(&ne)
	t.bufReset()
	t.openEnd = time.Time{}
}

// closeStream ends the open run at at (the TUI's closeStream and
// breakStream, which are the same call).
func (t *Transcript) closeStream(at time.Time) { t.endRun(at) }

// appendStream grows the open entry of the same kind, so a reply that arrives
// in five chunks stays one entry; otherwise it starts a run of its own. at is
// the chunk's own At, stamped from the clock only when it is zero.
func (t *Transcript) appendStream(kind Kind, text string, at time.Time) {
	if text == "" {
		return
	}
	at = t.model.stamp(at)
	if t.streamOpen && t.omittedRun == kind {
		// The run continues on the first client, in an entry the window this
		// model was restored from dropped: nothing here to draw, but its
		// placeholder — the ledger's last — accounts what the first model's
		// open entry does, min(bytes streamed, StreamText) (X25): exactly,
		// since that is the run's length alone, and the snapshot's record is
		// the run's figure at the cut (Restore). The budget is enforced after
		// it as after any chunk (X23).
		t.streamed(len(text))
		p := &t.ledger[len(t.ledger)-1]
		t.bytes += t.runBytes() - p.Bytes
		p.Bytes = t.runBytes()
		t.trimBytes()
		return
	}
	if t.streamOpen && t.omittedRun == 0 {
		if last := t.lastEntry(); last != nil && last.Kind == kind {
			// No new Entry (X24): the builder grows, the run's end moves on
			// the transcript, and the accounting follows the run's length
			// (X25). The stored entry is untouched; readers get it
			// materialised.
			before := t.runBytes()
			t.bufAppend(text)
			t.streamed(len(text))
			t.openEnd = at
			t.bytes += t.runBytes() - before
			t.model.noteTouched(t, last.ID)
			t.trimBytes()
			return
		}
	}
	// appendEntry's order: the previous run ends at this chunk's At, and only
	// then does this one open, in a builder that closing emptied.
	e := &Entry{Kind: kind, At: at, End: at, Open: kind == KindThought, Streaming: true}
	t.endRun(at)
	t.bufAppend(text)
	t.streamed(len(text))
	e.Bytes = t.runBytes()
	t.push(e)
	t.trim()
	t.streamOpen = true
	t.openEnd = at
}

// upsertTool keeps one row per tool call id, replaced in place (a new Entry,
// the same ID). Cursor's todo writer is dropped FIRST — before it can close
// the run it fires in the middle of (§3.3). A tool is stamped with its own At,
// else the envelope's, else the clock (execution amendment X1); that one stamp
// closes the run above it and dates a new row. An id-less tool always appends.
func (t *Transcript) upsertTool(tool *agent.ToolEvent, envelope time.Time) {
	if tool == nil || tool.IsTodoTool() {
		return
	}
	at := tool.At
	if at.IsZero() {
		at = t.model.stamp(envelope)
	}
	// A tool call ends the run above it either way: an update that lands in an
	// existing row still means the thinking before it is over.
	t.closeStream(at)
	if tool.ID != "" {
		if i, ok := t.ptools[tool.ID]; ok {
			// The row is one the window this model was restored from dropped,
			// and the first client still holds it (its placeholder remains):
			// the first client updates it in place, and this one has no row to
			// update — the suffix the two share is unchanged (plan 024 §3.5).
			// Its placeholder is re-accounted to the new payload's bytes, as
			// the first model's row is, and, as there, nothing is trimmed
			// (X23; X5 revised). The run above it closed all the same, on both.
			nb := entryBytes(&Entry{Kind: KindTool, Tool: tool}, t.streamCap)
			d := nb - t.ledger[i].Bytes
			t.ledger[i].Bytes = nb
			t.bytes += d
			t.grown += d
			return
		}
		if id, ok := t.tools[tool.ID]; ok {
			if ord, e := t.lookup(id); e != nil && e.Kind == KindTool {
				ne := *e
				ne.Tool = tool
				ne.Bytes = entryBytes(&ne, t.streamCap)
				// An update in place never trims — today's rule: the TUI trims
				// only on append (r2 finding 2, which revises execution amendment
				// X5 for this one path). A trim here could drop the very row it
				// updated, and the same update re-applied would then append the
				// row again: the tool's last state would not converge. The budget
				// is exceeded meanwhile by what updates in place added since it
				// was last enforced (grown) — at most one tool payload for each
				// row updated in place since then — until the next append or
				// chunk trims.
				t.grown += ne.Bytes - e.Bytes
				t.replace(ord, &ne)
				return
			}
		}
	}
	e := &Entry{Kind: KindTool, Tool: tool, At: at}
	t.appendEntry(e, at)
	if tool.ID != "" {
		t.tools[tool.ID] = e.ID
	}
}

// addUser writes a user row. Unlike the other rows it is written for empty
// text too (today's rule).
func (t *Transcript) addUser(text string, now time.Time) {
	t.appendEntry(&Entry{Kind: KindUser, Text: text}, now)
}

// addInterjection is the user row for text merged into the running turn,
// skipped when empty.
func (t *Transcript) addInterjection(text string, now time.Time) {
	if text == "" {
		return
	}
	t.appendEntry(&Entry{Kind: KindUser, Text: text, Interject: true}, now)
}

func (t *Transcript) addNote(text string, now time.Time) {
	if text == "" {
		return
	}
	t.appendEntry(&Entry{Kind: KindNote, Text: text}, now)
}

// addCommandLine records that craze expanded a plugin command or skill into
// the prompt above: a note, nothing for a nil command or a name that
// sanitises to empty.
func (t *Transcript) addCommandLine(cmd *agent.ExpandedCommand, now time.Time) {
	if cmd == nil {
		return
	}
	t.addNote(commandLine(cmd.Qualified, cmd.Kind), now)
}

// addPlan puts the plan the agent proposed into the transcript. The payload is
// the event's, retained by pointer (the TUI copied it; nothing here writes it).
func (t *Transcript) addPlan(p *agent.PlanEvent, now time.Time) {
	if p == nil {
		return
	}
	t.appendEntry(&Entry{Kind: KindPlan, Plan: p}, now)
}

// addError is an error row drawn from text, skipped when empty.
func (t *Transcript) addError(text string, now time.Time) {
	if text == "" {
		return
	}
	t.appendEntry(&Entry{Kind: KindError, Text: text}, now)
}

// addErrValue is EventError's row: the error value, held, with its text when
// the fold could read it ("" when it could not — foldError has already dropped
// a known empty one, today's rule). A nil error draws nothing.
func (t *Transcript) addErrValue(err error, text string, now time.Time) {
	if err == nil {
		return
	}
	t.appendEntry(&Entry{Kind: KindError, Err: err, Text: text}, now)
}

// noteTodos turns the todo stream into the two notes, under today's dedupe:
// "N planned" when the list grows past the largest it was noted at, "c/n
// done" once when every item is closed. at is the event's At.
func (t *Transcript) noteTodos(todos []agent.Todo, at time.Time) {
	if len(todos) == 0 {
		return
	}
	closed := 0
	for i := range todos {
		if s := todos[i].Status; s == "completed" || s == "cancelled" {
			closed++
		}
	}
	if closed == len(todos) {
		if !t.todoDone {
			t.todoDone = true
			t.addNote(todosDoneNote(closed, len(todos)), t.model.stamp(at))
		}
		return
	}
	t.todoDone = false
	if len(todos) > t.todoPlanned {
		t.todoPlanned = len(todos)
		t.addNote(todosPlannedNote(len(todos)), t.model.stamp(at))
	}
}
