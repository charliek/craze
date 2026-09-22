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
// to the front in place — one StreamText copy per StreamText of input. bufCut
// records that the run is longer than buf holds, so its tail is led by "…".
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

	bytes      int
	trimmed    bool
	streamOpen bool
	buf        []byte
	bufCut     bool

	// The todo-note dedupe (the TUI's todoPlanned / todoDone): the largest
	// list a "planned" note was written for, and whether the "done" note has
	// been written for the list as it stands. The main transcript's only.
	todoPlanned int
	todoDone    bool
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
// entry (Streaming) has an empty Text; its text is Tail.
func (t *Transcript) Entries() []*Entry {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]*Entry(nil), t.live()...)
}

// Entry is the entry id names, if the transcript still holds it.
func (t *Transcript) Entry(id EntryID) (*Entry, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	_, e := t.lookup(id)
	return e, e != nil
}

// Len is how many entries the transcript holds.
func (t *Transcript) Len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.len()
}

// Tail is the open stream entry's text so far — at most StreamText bytes, led
// by "…" once the beginning was dropped — or "" when no run is open. It
// copies: the pane calls it once per chunk, the fold never does.
func (t *Transcript) Tail() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.streamOpen {
		return ""
	}
	return t.tail()
}

// Trimmed reports whether the transcript has dropped entries off its front;
// a client draws TrimmedNote above it.
func (t *Transcript) Trimmed() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.trimmed
}

// StreamOpen reports whether a run is open: the next chunk of the last entry's
// kind grows that entry instead of starting one.
func (t *Transcript) StreamOpen() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.streamOpen
}

// Bytes is the retained bytes the transcript accounts for.
func (t *Transcript) Bytes() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.bytes
}

// ------------------------------------------------------ the deque, unlocked

func (t *Transcript) len() int { return len(t.ents) - t.head }

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
	t.bytes += ne.Bytes - t.ents[i].Bytes
	t.ents[i] = ne
	t.model.noteTouched(t, ne.ID)
}

func (t *Transcript) replaceLast(ne *Entry) { t.replace(t.base+len(t.ents)-1, ne) }

// trim is today's trimEntries over the deque: the entry cap, then the byte
// budget, each dropping from the front.
func (t *Transcript) trim() {
	for t.len() > t.maxEntries {
		t.dropHead()
	}
	t.trimBytes()
}

// trimBytes drops from the front until the retained bytes fit the budget,
// never dropping the last entry (today's guard).
func (t *Transcript) trimBytes() {
	for t.bytes > t.maxBytes && t.len() > 1 {
		t.dropHead()
	}
}

// dropHead is today's dropFirst(1): the oldest entry goes, and so does the
// tool index's name for it, so a later update to that id appends a new row.
func (t *Transcript) dropHead() {
	e := t.ents[t.head]
	t.ents[t.head] = nil
	t.head++
	t.bytes -= e.Bytes
	delete(t.slot, e.ID)
	if e.Kind == KindTool && e.Tool != nil && e.Tool.ID != "" {
		if id, ok := t.tools[e.Tool.ID]; ok && id == e.ID {
			delete(t.tools, e.Tool.ID)
		}
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

// ---------------------------------------------------------- the builder

// tailStart is where the open run's tail starts in buf, and whether the run
// is longer than the cap, so the tail is led by "…". It is capText's cut,
// taken on buf: when the run was compacted buf still holds its last StreamText
// bytes, which is more than the StreamText - len("…") the tail keeps, so the
// cut — and the scan forward to a rune start — land on the same bytes.
func (t *Transcript) tailStart() (int, bool) {
	n := len(t.buf)
	if !t.bufCut && n <= t.streamCap {
		return 0, false
	}
	start := n - (t.streamCap - len(ellipsis))
	if start < 0 {
		start = 0
	}
	for start < n && !utf8.RuneStart(t.buf[start]) {
		start++
	}
	return start, true
}

// tailLen is len(tail()) without building it: what the open entry accounts.
func (t *Transcript) tailLen() int {
	start, cut := t.tailStart()
	n := len(t.buf) - start
	if cut {
		n += len(ellipsis)
	}
	return n
}

// tail is the open run's text as today's capEntryText(whole) gives it.
func (t *Transcript) tail() string {
	start, cut := t.tailStart()
	if !cut {
		return string(t.buf)
	}
	return ellipsis + string(t.buf[start:])
}

// bufAppend adds a chunk to the open run.
func (t *Transcript) bufAppend(s string) {
	limit := 2 * t.streamCap
	if len(t.buf)+len(s) <= limit {
		t.bufReserve(len(t.buf) + len(s))
		t.buf = append(t.buf, s...)
		return
	}
	// Past 2 × StreamText: keep the last StreamText bytes of buf+s, at the
	// front of the same array. Nothing aliases buf, so moving bytes in place
	// is safe; the copy is at most StreamText bytes, and the next one is at
	// least StreamText bytes of input away.
	keep := t.streamCap
	if len(s) >= keep {
		t.buf = t.buf[:0]
		t.bufReserve(keep)
		t.buf = append(t.buf, s[len(s)-keep:]...)
	} else {
		from := keep - len(s)
		n := copy(t.buf, t.buf[len(t.buf)-from:])
		t.buf = append(t.buf[:n], s...)
	}
	t.bufCut = true
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

// bufReset empties the builder for the next run. A small buffer is kept for
// it; a large one is let go, so a transcript whose long reply has closed does
// not keep up to 2 × StreamText of capacity for the rest of the session.
func (t *Transcript) bufReset() {
	if cap(t.buf) > t.streamCap/4 {
		t.buf = nil
	} else {
		t.buf = t.buf[:0]
	}
	t.bufCut = false
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
	e.Bytes = entryBytes(e)
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
	if ne.Kind == KindThought && ne.Open {
		ne.Open = false
		if !at.IsZero() {
			ne.End = at
		}
	}
	ne.Bytes = entryBytes(&ne)
	t.bufReset()
	t.replaceLast(&ne)
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
	if t.streamOpen {
		if last := t.lastEntry(); last != nil && last.Kind == kind {
			t.bufAppend(text)
			ne := *last
			ne.End = at
			ne.Bytes = t.tailLen()
			t.replaceLast(&ne)
			t.trimBytes()
			return
		}
	}
	// appendEntry's order: the previous run ends at this chunk's At, and only
	// then does this one open, in a builder that closing emptied.
	e := &Entry{Kind: kind, At: at, End: at, Open: kind == KindThought, Streaming: true}
	t.endRun(at)
	t.bufAppend(text)
	e.Bytes = t.tailLen()
	t.push(e)
	t.trim()
	t.streamOpen = true
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
		if id, ok := t.tools[tool.ID]; ok {
			if ord, e := t.lookup(id); e != nil && e.Kind == KindTool {
				ne := *e
				ne.Tool = tool
				ne.Bytes = entryBytes(&ne)
				t.replace(ord, &ne)
				t.trimBytes()
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

// addErrValue is EventError's row: the error value, held. A nil error draws
// nothing (today's rule); an error whose text is empty — which today draws
// nothing either — cannot be told apart without calling Error(), so the
// reader decides that one at render time.
func (t *Transcript) addErrValue(err error, now time.Time) {
	if err == nil {
		return
	}
	t.appendEntry(&Entry{Kind: KindError, Err: err}, now)
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
