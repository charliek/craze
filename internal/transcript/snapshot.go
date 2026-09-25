package transcript

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/charliek/craze/internal/agent"
)

// SnapshotVersion is the version of the snapshot shape EncodeSnapshot writes
// and the only one DecodeSnapshot reads.
const SnapshotVersion = 1

// DefaultSnapshotBytes is the byte budget Snapshot applies when it is given
// none: Attach's SnapshotBytes default (plan 024 §3.5).
const DefaultSnapshotBytes = 4 << 20

// ItemCap is the per-item cap of a snapshot's mandatory sections (plan 024
// §3.5). Each of these strings is carried as its head, cut back to a rune
// boundary, when it is longer, and what carried it is marked:
//
//   - every string an open ask carries — its ID and every string of its body,
//     a permission's, a question's (its answer VALUES included — each
//     sub-question's own id and the answer map's keys are ids and stay
//     whole, like a queue row's id: capping them can collide two into one,
//     r6 fix) or a plan's (its todos included) — marking the ask Truncated
//     (r5 finding 1);
//   - a roster row's Prompt and Output, marking the row (AgentRow.Truncated);
//   - the current turn's Text, marking Turn.Truncated (r3 finding 3: the
//     turn's Text is otherwise mandatory and uncapped, so a multi-MiB prompt
//     makes every snapshot ErrSnapshotTooLarge until the next turn);
//   - every string of every todo, marking Snapshot.TodosTruncated;
//   - a queue row's Text, naming the row in Snapshot.TruncatedQueue (r5
//     finding 2: agent.PromptQueue.PushFront, a requeued interjection, is not
//     held to the queue's 32 KiB limit);
//   - every string of every settings section — the title, mode and model, the
//     config, command and plugin catalogs, the send-now — marking that
//     section in Settings.Truncated.
const ItemCap = 256 << 10

// ErrSnapshotTooLarge is Snapshot's refusal: the mandatory sections with the
// main transcript's newest entry — or alone, when it holds none — encode to
// more than the byte budget. It is never a silent drop (plan 024 §3.5). The
// mandatory sections' text is capped (ItemCap), so what can still reach it
// there is the NUMBER of items — asks, roster rows, todos, queue rows, catalog
// entries, ended asks — which no cap bounds (or an id or status no agent mints
// at that size: the strings ItemCap does not list are carried whole), and the
// ledger, a record per entry the window omits (X23), whose count the bounds do
// bound.
var ErrSnapshotTooLarge = errors.New("transcript: snapshot too large for its byte budget")

// Snapshot is one model at one Seq, bounded to a byte budget: what a client
// attaching mid-session restores from (Restore) before it folds the events
// after Seq (plan 024 §3.5). It is a value its holder owns — nothing the model
// does later changes it — and it shares the model's immutable payloads
// (tool, plan and ask bodies) by pointer.
type Snapshot struct {
	// Version is SnapshotVersion.
	Version int
	// Incarnation is the event log's incarnation the model was folded from,
	// and Seq the last event it folded: the cutoff the client folds on from.
	Incarnation string
	Seq         uint64
	// Local is the model's counter for ids of events that carry no Seq of
	// their own (execution amendment X2), so a restored model names later
	// such entries as the first one does.
	Local uint32
	// Main is the main transcript and Subs the children's, in the order the
	// model created them.
	Main TranscriptSnap
	Subs []SubSnap
	// Agents is the roster in the order its rows first appeared, and
	// FinishSeq the finish counter its eviction order is drawn from.
	Agents    []AgentRow
	FinishSeq uint64
	// Todos, Asks (open, in the order they opened), Turn, Replaying,
	// Settings and Queue are the model's state (State), their strings capped
	// at ItemCap. TodosTruncated marks a todo list carried as heads (State's
	// TodosTruncated), and TruncatedQueue names, in queue order, the rows
	// whose Text was (State's TruncatedQueue); an ask, the turn and the
	// settings carry their own marks.
	Todos          []agent.Todo
	TodosTruncated bool
	Asks           []Ask
	// Ended is the last-ended ask list (Model.EndedAsks), carried so a
	// restored model's list and its later evictions are the first one's.
	Ended     []AskEnding
	Turn      Turn
	Replaying bool
	Settings  Settings
	Queue     []agent.QueuedPrompt
	// TruncatedQueue is described with Todos above.
	TruncatedQueue []string
}

// TranscriptSnap is one transcript in a snapshot: a suffix of its entries and
// the state that decides what the next event does to them (plan 024 §3.5).
type TranscriptSnap struct {
	// Entries is the retained suffix, oldest first. An open stream entry
	// (Streaming, the last) carries its tail as Text; an error entry carries
	// its error's text as Text and a *agent.RemoteError as Err.
	Entries []Entry
	// Trimmed says the model itself dropped entries off the front (today's
	// flag); Windowed and Dropped that the snapshot omitted Dropped older
	// entries to fit its budget. A client draws the trim note for either.
	Trimmed  bool
	Windowed bool
	Dropped  int
	// StreamOpen is the continuation of the open run: the next chunk of the
	// last entry's kind grows it. TailCut says the open entry's tail lost the
	// run's beginning, so it is led by "…".
	StreamOpen bool
	TailCut    bool
	// TodoPlanned and TodoDone are the todo-note dedupe (the main
	// transcript's), so a repeated todo list draws no second note.
	TodoPlanned int
	TodoDone    bool
	// Omitted is the ledger (execution amendment X23): one record per entry
	// the window omitted, oldest first — the restored model's placeholders,
	// its own ledger's carried forward first when the model was itself
	// restored. Restore puts them at the head of the transcript, counted in
	// its entry cap and byte budget, so the restored model trims them exactly
	// when the first model trims the real rows; an update to a tool whose
	// placeholder remains draws nothing and re-accounts it.
	Omitted []Omitted
	// OmittedRun is the kind of the open run's entry when the window dropped
	// it too (a child that fitted nothing, mid-stream): the run's next chunks
	// draw nothing, and grow its placeholder — the ledger's last — until
	// something ends it.
	OmittedRun Kind
}

// Omitted is one ledger record: an entry a snapshot's window omitted, as a
// payload-free placeholder (X23). Bytes is its retained bytes as the model
// the snapshot was cut from accounts them (Entry.Bytes; for a streamed entry,
// open or closed, min(bytes streamed, StreamText), X25), and Tool the id a
// tool entry answers updates under, "" for any other entry. On the wire it is
// [bytes] or [bytes,"tool id"].
type Omitted struct {
	Bytes int
	Tool  string
}

// SubSnap is one child's transcript in a snapshot.
type SubSnap struct {
	ID string
	TranscriptSnap
}

// AgentRow is one roster row in a snapshot: the row, its place in finish order
// (0 while it has not finished this run), and whether its Prompt or Output was
// carried as a head (ItemCap).
type AgentRow struct {
	Info      agent.SubagentInfo
	Finish    uint64
	Truncated bool
}

// Snapshot cuts the model at its current Seq and bounds it to budget bytes of
// encoding (EncodeSnapshot) — envelope and metadata included; budget <= 0 is
// DefaultSnapshotBytes (plan 024 §3.5).
//
// The mandatory sections' text is carried as at most its ItemCap head, marked
// where it was cut (ItemCap), so no one oversized string makes every snapshot
// fail. What can still put the mandatory sections over the budget is an
// unbounded NUMBER of items — asks, roster rows, todos, queue rows, catalog
// entries — and that is ErrSnapshotTooLarge, never a drop.
//
// Under the model's lock it only takes the cut: pointer copies of the entries,
// the roster and the asks, and each open run's tail (≤ StreamText). Everything
// else runs after the lock is released — the per-item caps, every call to an
// error's methods, the encoding and the window — so a fold waits on a
// snapshot for the length of the cut alone (§3.4).
//
// The filling order is the plan's: first the mandatory sections — the roster,
// the todos, the asks, the ended list, the turn, the settings, the queue, and
// every transcript's continuation fields; then the main transcript's entries,
// newest first, until the next would not fit, the newest one being required
// — if the mandatory sections do not fit with it (or, when the main
// transcript has none, alone), ErrSnapshotTooLarge; then each child's, in the
// order they were created, with what remains. Every entry is encoded once,
// and the window counts the bytes the encoding will have, not an estimate:
// the snapshot's encoding is at most budget bytes.
//
// An entry the window leaves out is carried as its ledger record
// (TranscriptSnap.Omitted, X23), so the mandatory sections are counted with
// every entry as a record, and each entry taken in replaces its record — a
// few bytes against the entry's own encoding, so the window still converges.
// The last record taken out also takes the window's members ("windowed",
// "dropped", "omitted") with it, which can make the whole transcript smaller
// than the all-ledger form; so a refusal waits for the newest main entry to be
// weighed (review r7 finding 2).
// The ledger is budgeted like everything else: on A3's worst case it is
// ~33,000 records and ~0.5 MB of a 4 MiB snapshot.
func (m *Model) Snapshot(budget int) (*Snapshot, error) {
	s, _, err := m.snapshotSized(budget)
	return s, err
}

// SnapshotFor is Snapshot with one child's transcript given the room first:
// session.snapshot's agentId (plan 027 §3.4, "a snapshot of the main
// transcript or of a child's"). It is the same cut, codec, ledger and per-item
// caps as Snapshot, and differs only in the order the window is filled:
//
//  1. the mandatory sections, every transcript at its continuation fields;
//  2. the main transcript's NEWEST entry — still required, so a snapshot of a
//     child restores exactly as any other (Restore is unchanged), and it is
//     refused ErrSnapshotTooLarge exactly when Snapshot would be;
//  3. the child's entries, newest first, until the next would not fit;
//  4. the rest of the main transcript's, newest first, with what remains;
//  5. every other child's, in the order they were created.
//
// A child the roster names that has no transcript of its own (a cursor task)
// has nothing to put first, and its snapshot is Snapshot's. An id that neither
// a child transcript nor a roster row of the model names is refused with an
// error wrapping agent.ErrNoSuchSubagent — the engine's unknown_subagent. An
// empty agentID is Snapshot.
func (m *Model) SnapshotFor(agentID string, budget int) (*Snapshot, error) {
	if budget <= 0 {
		budget = DefaultSnapshotBytes
	}
	c := m.snapshotCut()
	if agentID != "" && !c.names(agentID) {
		return nil, fmt.Errorf("transcript: no sub-agent %q in the model: %w", agentID, agent.ErrNoSuchSubagent)
	}
	s, _, err := c.snapshot(budget, agentID)
	return s, err
}

// names reports whether a child transcript or a roster row of the cut is id's.
func (c *cut) names(id string) bool {
	return slices.ContainsFunc(c.subs, func(s subCut) bool { return s.id == id }) ||
		slices.ContainsFunc(c.agents, func(r rosterRow) bool { return r.info.ID == id })
}

// snapshotSized is Snapshot with the encoded size its window counted, which a
// test holds against the encoding's real length.
func (m *Model) snapshotSized(budget int) (*Snapshot, int, error) {
	if budget <= 0 {
		budget = DefaultSnapshotBytes
	}
	c := m.snapshotCut()
	return c.snapshot(budget, "")
}

// snapshot builds the snapshot from a cut, after the lock (see Snapshot).
// focus is the child whose transcript is filled right after the main
// transcript's newest entry (SnapshotFor), and "" for Snapshot's order.
func (c *cut) snapshot(budget int, focus string) (*Snapshot, int, error) {
	s := &Snapshot{
		Version:     SnapshotVersion,
		Incarnation: c.incarnation,
		Seq:         c.seq,
		Local:       c.local,
		FinishSeq:   c.finishSeq,
		Ended:       c.ended,
		Turn:        capTurn(c.turn),
		Replaying:   c.replaying,
		Settings:    capSettings(c.settings),
	}
	var tc capper
	s.Todos = capEach(&tc, c.todos, capTodo)
	s.TodosTruncated = c.todosTruncated || tc.cut
	s.Queue, s.TruncatedQueue = capQueue(c.queue, c.queueTruncated)
	if len(c.agents) > 0 {
		s.Agents = make([]AgentRow, len(c.agents))
		for i, r := range c.agents {
			info, cut := capAgent(r.info)
			s.Agents[i] = AgentRow{Info: info, Finish: r.finish, Truncated: r.truncated || cut}
		}
	}
	if len(c.asks) > 0 {
		s.Asks = make([]Ask, len(c.asks))
		for i := range c.asks {
			s.Asks[i] = capAsk(c.asks[i])
		}
	}

	jw := newJSONWriter()
	hdr, err := encodeHeader(jw, s)
	if err != nil {
		return nil, 0, err
	}
	wins := make([]*window, 0, 1+len(c.subs))
	wins = append(wins, newWindow(jw, &c.main, "", false))
	for i := range c.subs {
		wins = append(wins, newWindow(jw, &c.subs[i].t, c.subs[i].id, true))
	}

	// (1) The mandatory sections: the envelope, the header, and every
	// transcript at its continuation fields, every entry a ledger record.
	total := len(hdr) - 1 + len(`,"main":`) + len(`}`)
	if len(c.subs) > 0 {
		total += len(`,"subs":[]`) + len(c.subs) - 1
	}
	for _, w := range wins {
		total += w.cur
	}
	mandatory := total
	// (2) The main transcript's tail, newest first; its newest entry is
	// required. The refusal is decided with it weighed, not before: the size
	// above is not a lower bound, because taking an entry in can shrink the
	// encoding — the transcript's last record goes, and "windowed", "dropped"
	// and "omitted" with it (review r7 finding 2). fill takes the entry
	// whenever the snapshot is within the budget with it, whatever it was
	// before.
	main := wins[0]
	// A focused snapshot weighs only main's newest entry here — the one that
	// is required — and comes back for the rest after the child (SnapshotFor).
	var first *window
	if focus != "" {
		for _, w := range wins[1:] {
			if w.id == focus {
				first = w
			}
		}
	}
	upTo := main.n
	if first != nil {
		upTo = min(main.n, 1)
	}
	if total, err = main.fillTo(total, budget, upTo); err != nil {
		return nil, 0, err
	}
	switch {
	case main.k > 0 || main.n == 0 && total <= budget:
	case main.n == 0:
		return nil, 0, fmt.Errorf("%w: the mandatory sections encode to %d bytes, over a budget of %d", ErrSnapshotTooLarge, total, budget)
	case mandatory > budget:
		return nil, 0, fmt.Errorf("%w: the mandatory sections encode to %d bytes, and to %d with the main transcript's newest entry, over a budget of %d", ErrSnapshotTooLarge, mandatory, main.over, budget)
	default:
		return nil, 0, fmt.Errorf("%w: the main transcript's newest entry does not fit beside %d bytes of mandatory state in a budget of %d", ErrSnapshotTooLarge, total, budget)
	}
	// (2b) A focused snapshot: the child's tail, then the rest of main's.
	if first != nil {
		if total, err = first.fill(total, budget); err != nil {
			return nil, 0, err
		}
		if total, err = main.fill(total, budget); err != nil {
			return nil, 0, err
		}
	}
	// (3) Each child in the order it was created, with what remains.
	for _, w := range wins[1:] {
		if w == first {
			continue
		}
		if total, err = w.fill(total, budget); err != nil {
			return nil, 0, err
		}
	}

	s.Main = wins[0].snap()
	if len(wins) > 1 {
		s.Subs = make([]SubSnap, len(wins)-1)
		for i, w := range wins[1:] {
			s.Subs[i] = SubSnap{ID: w.id, TranscriptSnap: w.snap()}
		}
	}
	return s, total, nil
}

// window is one transcript's part of a snapshot as it is filled: the newest k
// of its n entries are in, and cur is the encoded size of its object as it
// stands. Every size it counts is the length of bytes the encoder writes —
// each entry encoded once, by the encoder's own encodeEntry, and the members
// written by its own appendScalars and appendOmitted — so the sum is the
// encoding's length, not an estimate.
type window struct {
	jw  *jsonWriter
	tc  *transcriptCut
	id  string
	sub bool

	n, k int
	// ents are the included entries, newest first, as the snapshot carries
	// them (an error resolved to its text), and entLen the sum of their
	// encodings' lengths.
	ents   []Entry
	entLen int
	// recLen is, per entry oldest first, the length of the ledger record the
	// entry becomes when the window drops it (X23: every entry becomes one);
	// restLen sums it over the entries not included, which are the oldest
	// n - k.
	recLen  []int
	restLen int
	// carried is what a restored transcript's own ledger already holds,
	// ahead of every record this window adds: its count and encoded length.
	carriedRecs, carriedLen int
	openLast                bool
	cur                     int
	// over is the snapshot's size the next entry would have made, when fill
	// stopped short of it: what a refusal reports.
	over    int
	scratch []byte
}

func newWindow(jw *jsonWriter, tc *transcriptCut, id string, sub bool) *window {
	w := &window{jw: jw, tc: tc, id: id, sub: sub, n: len(tc.entries)}
	w.openLast = tc.streamOpen && w.n > 0 && tc.entries[w.n-1].Streaming
	w.carriedRecs = len(tc.omitted)
	for _, r := range tc.omitted {
		w.carriedLen += omittedLen(r)
	}
	w.recLen = make([]int, w.n)
	for i, e := range tc.entries {
		w.recLen[i] = omittedLen(omittedOf(e))
		w.restLen += w.recLen[i]
	}
	w.cur = w.size(0, 0, w.n, w.restLen)
	return w
}

// omittedOf is the ledger record of an entry the window drops: what the model
// accounts for it — the cut's Bytes, which for the open run is the run's
// current figure (X24's current, X25's min(bytes streamed, StreamText)) — and
// the id a tool entry answers updates under.
func omittedOf(e *Entry) Omitted {
	return Omitted{Bytes: e.Bytes, Tool: entryToolID(e)}
}

// entryToolID is the id a tool entry answers updates under, "" for none.
func entryToolID(e *Entry) string {
	if e.Kind != KindTool || e.Tool == nil {
		return ""
	}
	return e.Tool.ID
}

// scalars is the transcript object's scalar members with the newest k
// entries in.
func (w *window) scalars(k int) transcriptScalars {
	d := w.n - k
	sc := transcriptScalars{
		id: w.id, sub: w.sub,
		trimmed:     w.tc.trimmed,
		windowed:    w.tc.windowed || d > 0,
		dropped:     w.tc.dropped + d,
		streamOpen:  w.tc.streamOpen,
		tailCut:     w.tc.tailCut && w.openLast && k > 0,
		todoPlanned: w.tc.todoPlanned,
		todoDone:    w.tc.todoDone,
		omittedRun:  w.tc.omittedRun,
	}
	if sc.omittedRun == 0 && w.openLast && k == 0 {
		sc.omittedRun = w.tc.entries[w.n-1].Kind
	}
	return sc
}

// size is the encoded length of the transcript's object with the newest k
// entries in (their encodings entLen bytes long) and recs ledger records
// (recsLen bytes) after the carried ones: the scalar members as the encoder
// writes them, then Omitted and Entries, each member followed by a comma but
// the last.
func (w *window) size(k, entLen, recs, recsLen int) int {
	var members int
	w.scratch, members = appendScalars(w.scratch[:0], w.scalars(k))
	sum := len(w.scratch) // each scalar member with its comma
	if r := w.carriedRecs + recs; r > 0 {
		sum += len(`"omitted":[]`) + w.carriedLen + recsLen + r - 1 + 1
		members++
	}
	if k > 0 {
		sum += len(`"entries":[]`) + entLen + k - 1 + 1
		members++
	}
	if members == 0 {
		return len(`{}`)
	}
	return 1 + sum // "{", the members and their commas, the last comma a "}"
}

// fill adds entries newest first while the snapshot stays within budget; total
// is the snapshot's size as it stands — for the main transcript it can be over
// the budget, which its newest entry may bring within it (Snapshot) — and the
// result what it becomes.
func (w *window) fill(total, budget int) (int, error) {
	return w.fillTo(total, budget, w.n)
}

// fillTo is fill that stops once upTo of the window's entries are in.
func (w *window) fillTo(total, budget, upTo int) (int, error) {
	for w.k < upTo {
		i := w.n - 1 - w.k
		e := snapEntry(w.tc.entries[i])
		b, err := encodeEntry(w.jw, &e)
		if err != nil {
			return total, err
		}
		// Taking entry i in takes its ledger record out: the record is far
		// shorter than the entry's encoding, so the window still converges.
		restLen := w.restLen - w.recLen[i]
		next := w.size(w.k+1, w.entLen+len(b), i, restLen)
		if total-w.cur+next > budget {
			w.over = total - w.cur + next
			break
		}
		total += next - w.cur
		w.cur = next
		w.k++
		w.entLen += len(b)
		w.ents = append(w.ents, e)
		w.restLen = restLen
	}
	return total, nil
}

// snap is the transcript as the window left it.
func (w *window) snap() TranscriptSnap {
	sc := w.scalars(w.k)
	ts := TranscriptSnap{
		Trimmed:     sc.trimmed,
		Windowed:    sc.windowed,
		Dropped:     sc.dropped,
		StreamOpen:  sc.streamOpen,
		TailCut:     sc.tailCut,
		TodoPlanned: sc.todoPlanned,
		TodoDone:    sc.todoDone,
		OmittedRun:  sc.omittedRun,
	}
	if w.k > 0 {
		ts.Entries = make([]Entry, w.k)
		for i, e := range w.ents {
			ts.Entries[w.k-1-i] = e
		}
	}
	if r := w.carriedRecs + w.n - w.k; r > 0 {
		ts.Omitted = make([]Omitted, 0, r)
		ts.Omitted = append(ts.Omitted, w.tc.omitted...)
		for _, e := range w.tc.entries[:w.n-w.k] {
			ts.Omitted = append(ts.Omitted, omittedOf(e))
		}
	}
	return ts
}

// snapEntry is an entry as a snapshot carries it: a copy, with an error
// value resolved to what the codec keeps of it — its text (the entry's own
// when the fold knew it, else the error's message) and a *agent.RemoteError
// with that message and the event codec's class and code. This is where an
// error's own methods run, outside every lock (plan 024 §3.2), so a snapshot
// used in process and one that went through the codec restore the same.
func snapEntry(p *Entry) Entry {
	e := *p
	if e.Err == nil {
		return e
	}
	re := agent.RemoteErrorOf(e.Err)
	if e.Text == "" {
		e.Text = re.Message
	}
	if held, ok := e.Err.(*agent.RemoteError); !ok || held == nil || *held != (agent.RemoteError{Message: e.Text, Class: re.Class, Code: re.Code}) {
		e.Err = &agent.RemoteError{Message: e.Text, Class: re.Class, Code: re.Code}
	}
	return e
}

// ------------------------------------------------------------ per-item caps

// headOf is s cut to at most limit bytes, back to a rune boundary, and
// whether it was cut.
func headOf(s string, limit int) (string, bool) {
	if len(s) <= limit {
		return s, false
	}
	n := limit
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n], true
}

// capAgent is a roster row with its Prompt and Output capped at ItemCap.
func capAgent(info agent.SubagentInfo) (agent.SubagentInfo, bool) {
	var a, b bool
	info.Prompt, a = headOf(info.Prompt, ItemCap)
	info.Output, b = headOf(info.Output, ItemCap)
	return info, a || b
}

// capTurn is the current turn with its Text capped at ItemCap (r3 finding
// 3): a running turn with a multi-MiB prompt is carried as its head rather
// than making the snapshot's mandatory sections ErrSnapshotTooLarge. t.
// Truncated is kept once set, since a snapshot may be built from a cut that
// already carries a prior snapshot's own truncation (Restore, then Snapshot
// again with no turn started since).
func capTurn(t Turn) Turn {
	cut := false
	t.Text, cut = headOf(t.Text, ItemCap)
	t.Truncated = t.Truncated || cut
	return t
}

// capper caps strings at ItemCap and remembers whether it cut any.
type capper struct{ cut bool }

func (c *capper) str(s string) string {
	h, cut := headOf(s, ItemCap)
	c.cut = c.cut || cut
	return h
}

// capEach is s with f applied to each element, and cut when f cut any: s
// itself when nothing was cut (nil stays nil, empty stays empty), else a copy
// — the elements are the event's own, and never written.
func capEach[E any](c *capper, s []E, f func(*capper, E) E) []E {
	var out []E
	for i := range s {
		var ec capper
		e := f(&ec, s[i])
		if !ec.cut {
			continue
		}
		if out == nil {
			out = slices.Clone(s)
		}
		out[i] = e
		c.cut = true
	}
	if out == nil {
		return s
	}
	return out
}

// capAsk is an open ask with its ID and every string of its body capped at
// ItemCap (r5 finding 1): the payload is copied only where something is cut,
// so the event's own is never written. The strings are agent's payload
// types' own, field by field; TestEveryStringOfAnOpenAskIsCapped fills each
// one reachable from an ask by reflection, so a string field added to them
// later and not capped here fails it.
func capAsk(a Ask) Ask {
	var c capper
	a.ID = c.str(a.ID)
	if p := a.Body.Permission; p != nil {
		a.Body.Permission = capPtr(&c, p, capPermission)
	}
	if q := a.Body.Question; q != nil {
		a.Body.Question = capPtr(&c, q, capQuestion)
	}
	if p := a.Body.Plan; p != nil {
		a.Body.Plan = capPtr(&c, p, capPlan)
	}
	a.Truncated = a.Truncated || c.cut
	return a
}

// CapAskBody is an ask's body with every string capped at ItemCap exactly as a
// snapshot caps an open ask's (capAsk, its ID aside), and whether any was cut:
// asks.get's record (plan 027 §3.2 — "body strings capped at the snapshot's
// ItemCap, truncated set"), so the socket and the snapshot hold one rule. The
// payload is copied only where something is cut; body's own is never written.
func CapAskBody(body agent.AskBody) (agent.AskBody, bool) {
	a := capAsk(Ask{Body: body})
	return a.Body, a.Truncated
}

// capPtr is *p capped by f: p itself when nothing was cut, else a pointer to
// the capped copy.
func capPtr[T any](c *capper, p *T, f func(*capper, T) T) *T {
	var pc capper
	v := f(&pc, *p)
	if !pc.cut {
		return p
	}
	c.cut = true
	return &v
}

func capPermission(c *capper, p agent.PermissionEvent) agent.PermissionEvent {
	p.ID, p.Tool = c.str(p.ID), c.str(p.Tool)
	p.Options = capEach(c, p.Options, func(c *capper, o agent.PermissionOption) agent.PermissionOption {
		o.OptionID, o.Name, o.Kind = c.str(o.OptionID), c.str(o.Name), c.str(o.Kind)
		return o
	})
	return p
}

// capQuestion is a question's ID and title, and each sub-question's prompt
// and options, capped at ItemCap; each sub-question's own ID stays whole (r6
// fix): it is the key capAnswers' map is keyed by (session.go: "Answers maps
// each question's id..."), and capping two that share a long prefix would
// collide them, silently dropping one's answer.
func capQuestion(c *capper, q agent.QuestionEvent) agent.QuestionEvent {
	q.ID, q.Title = c.str(q.ID), c.str(q.Title)
	q.Questions = capEach(c, q.Questions, func(c *capper, qq agent.Question) agent.Question {
		qq.Prompt = c.str(qq.Prompt)
		qq.Options = capEach(c, qq.Options, func(c *capper, o agent.Option) agent.Option {
			o.ID, o.Label, o.Description = c.str(o.ID), c.str(o.Label), c.str(o.Description)
			return o
		})
		return qq
	})
	q.Answers = capAnswers(c, q.Answers)
	return q
}

// capAnswers is a question's answers with every value capped: the map itself
// when nothing was cut, else a copy. The keys stay whole (r6 fix): they are
// each sub-question's id (capQuestion's own qq.ID, uncapped for the same
// reason), and capping two that share a long prefix would collide them,
// silently dropping one's answer — the strings ItemCap does not list are
// carried whole, and an id is one of those.
func capAnswers(c *capper, m map[string][]string) map[string][]string {
	over := false
	for _, v := range m {
		if slices.ContainsFunc(v, func(s string) bool { return len(s) > ItemCap }) {
			over = true
			break
		}
	}
	if !over {
		return m
	}
	out := make(map[string][]string, len(m))
	for k, v := range m {
		out[k] = capEach(c, v, func(c *capper, s string) string { return c.str(s) })
	}
	return out
}

func capPlan(c *capper, p agent.PlanEvent) agent.PlanEvent {
	p.ID, p.Name = c.str(p.ID), c.str(p.Name)
	p.Overview, p.Plan = c.str(p.Overview), c.str(p.Plan)
	p.Todos = capEach(c, p.Todos, capTodo)
	return p
}

func capTodo(c *capper, t agent.Todo) agent.Todo {
	t.ID, t.Content, t.Status = c.str(t.ID), c.str(t.Content), c.str(t.Status)
	return t
}

// capQueue is the queue with each row's Text capped at ItemCap (r5 finding 2),
// and the ids, in queue order, of the rows cut here or already marked (a
// model restored from a snapshot that cut them). q is the cut's own copy.
func capQueue(q []agent.QueuedPrompt, marked map[string]bool) ([]agent.QueuedPrompt, []string) {
	var ids []string
	for i := range q {
		var c capper
		q[i].Text = c.str(q[i].Text)
		if c.cut || marked[q[i].ID] {
			ids = append(ids, q[i].ID)
		}
	}
	return q, ids
}

// capSettings is the settings with every string of every section capped at
// ItemCap, each section that was cut marked in Truncated (r5 finding 2). A
// mark already set is kept, as capTurn keeps Turn.Truncated.
func capSettings(s Settings) Settings {
	t := &s.Truncated
	one := func(v *string, mark *bool) {
		var c capper
		*v = c.str(*v)
		*mark = *mark || c.cut
	}
	one(&s.Title, &t.Title)
	one(&s.Mode, &t.Mode)
	one(&s.Model, &t.Model)
	var c capper
	s.Config = capEach(&c, s.Config, func(c *capper, o agent.ConfigOption) agent.ConfigOption {
		o.ID, o.Name, o.Category = c.str(o.ID), c.str(o.Name), c.str(o.Category)
		o.Type, o.Current = c.str(o.Type), c.str(o.Current)
		o.SelectValues = capEach(c, o.SelectValues, func(c *capper, v agent.SelectValue) agent.SelectValue {
			v.Value, v.Name = c.str(v.Value), c.str(v.Name)
			return v
		})
		return o
	})
	t.Config = t.Config || c.cut
	c = capper{}
	s.Commands = capEach(&c, s.Commands, func(c *capper, cm agent.CommandInfo) agent.CommandInfo {
		cm.Name, cm.Description = c.str(cm.Name), c.str(cm.Description)
		return cm
	})
	t.Commands = t.Commands || c.cut
	c = capper{}
	s.Plugins = capEach(&c, s.Plugins, func(c *capper, p agent.PluginCommand) agent.PluginCommand {
		p.Plugin, p.Bare, p.Display = c.str(p.Plugin), c.str(p.Bare), c.str(p.Display)
		p.Qualified, p.Description, p.Kind = c.str(p.Qualified), c.str(p.Description), c.str(p.Kind)
		return p
	})
	t.Plugins = t.Plugins || c.cut
	c = capper{}
	sn := &s.SendNow
	sn.Text, sn.FromRow, sn.Turn = c.str(sn.Text), c.str(sn.FromRow), c.str(sn.Turn)
	t.SendNow = t.SendNow || c.cut
	return s
}

// ------------------------------------------------------------------ Restore

// Restore builds the model a snapshot was cut from, as far as the snapshot
// carries it (plan 024 §3.5): on the history and state projections it equals
// the snapshotted model for the entries it retained, and folding the events
// after s.Seq gives what the first model gives folding them — the next chunk
// of an open run grows its tail (StreamOpen, TailCut), a repeated todo list
// draws no second note, an update to a tool the model itself trimmed appends
// a new row as it does on the first, and later entries take the ids the first
// model gives them (Seq, Local). The entries keep the snapshot's EntryIDs,
// and each accounts its bytes by this model's rule (Entry.Bytes is
// recomputed, not read): a closed streamed entry StreamText when it is Cut,
// else its text's length — exactly what the first model accounts (X25).
//
// A windowed snapshot's ledger (TranscriptSnap.Omitted, X23) becomes the
// transcript's placeholders, ahead of its entries and counted in its caps, so
// this model trims exactly when the first one does: while a tool's
// placeholder remains, an update to it draws nothing (the first model updates
// the row this one never had) and re-accounts the placeholder; once the trim
// has dropped it — when the first model drops the row and forgets the id — an
// update appends a new row on both. A placeholder is never read: the
// projections and every reader hold the real entries only, and a windowed
// restore equals the first model on its suffix (see State.Tools).
//
// o is as for New; its Incarnation is replaced by the snapshot's, and its
// Bounds must be the first model's for the two to trim alike (DefaultBounds
// for every instance, §3.2 (b)). Restore trims nothing: a model's own entries
// always fit its entry cap, and its byte budget is enforced at the next append
// or chunk, as on the first model. Restore keeps what it needs of s — the
// payloads by pointer, which nothing writes — and s may be dropped after it.
// It never fails: what is inconsistent in a hand-built snapshot (an entry with
// the zero id or a duplicate one, a child or roster id repeated or empty, a
// streaming entry that is not the open run's, an omitted run with no
// placeholder or beside entries, a negative ledger size, a tool id named by
// two placeholders or by a placeholder and a row, a Cut mark on a text longer
// than StreamText, an omitted run's record over StreamText) is skipped, taken
// as closed, taken as zero, unset or capped, and a placeholder naming a tool a
// row or a newer placeholder also names keeps its size and loses the id.
func Restore(s *Snapshot, o Options) *Model {
	m := New(o)
	if s == nil {
		return m
	}
	m.incarnation = s.Incarnation
	m.seq, m.local, m.finishSeq = s.Seq, s.Local, s.FinishSeq
	m.Main.restore(&s.Main)
	for i := range s.Subs {
		id := s.Subs[i].ID
		if _, dup := m.subs[id]; id == "" || dup {
			continue
		}
		m.ensureSub(id).restore(&s.Subs[i].TranscriptSnap)
	}
	for _, r := range s.Agents {
		id := r.Info.ID
		if id == "" {
			continue
		}
		if _, ok := m.agents[id]; !ok {
			m.agentOrder = append(m.agentOrder, id)
		}
		m.agents[id] = rosterRow{info: r.Info, finish: r.Finish, truncated: r.Truncated}
	}
	m.todos = nilIfEmpty(slices.Clone(s.Todos))
	m.todosTruncated = s.TodosTruncated
	for _, a := range s.Asks {
		m.asks.upsert(a)
	}
	for _, e := range s.Ended {
		if !m.ended.has(e.ID) && m.ended.len() >= maxEnded {
			m.ended.dropOldest()
		}
		m.ended.upsert(e)
	}
	m.turn = s.Turn
	m.replaying = s.Replaying
	m.settings = s.Settings
	m.queue = nilIfEmpty(slices.Clone(s.Queue))
	for _, id := range s.TruncatedQueue {
		if !slices.ContainsFunc(m.queue, func(q agent.QueuedPrompt) bool { return q.ID == id }) {
			continue
		}
		if m.queueTruncated == nil {
			m.queueTruncated = make(map[string]bool)
		}
		m.queueTruncated[id] = true
	}
	return m
}

// restore fills an empty transcript from its snapshot.
func (t *Transcript) restore(ts *TranscriptSnap) {
	t.trimmed = ts.Trimmed
	t.windowed = ts.Windowed
	t.dropped = ts.Dropped
	t.todoPlanned, t.todoDone = ts.TodoPlanned, ts.TodoDone
	n := len(ts.Entries)
	openLast := ts.StreamOpen && n > 0 && ts.Entries[n-1].Streaming
	for i := range ts.Entries {
		e := ts.Entries[i]
		if _, dup := t.slot[e.ID]; e.ID.IsZero() || dup {
			continue
		}
		if openLast && i == n-1 {
			t.restoreRun(&e, ts.TailCut)
		} else {
			e.Streaming = false
			// Cut on a text longer than the cap is not a tail: it accounts
			// what it holds.
			e.Cut = e.Cut && len(e.Text) <= t.streamCap
			e.Bytes = entryBytes(&e, t.streamCap)
		}
		ne := &e
		t.slot[ne.ID] = t.base + len(t.ents)
		t.ents = append(t.ents, ne)
		t.bytes += ne.Bytes
		if tid := entryToolID(ne); tid != "" {
			t.tools[tid] = ne.ID
		}
	}
	// The ledger: placeholders at the head, the model's own copy (s's is the
	// caller's).
	if len(ts.Omitted) > 0 {
		t.ledger = make([]Omitted, len(ts.Omitted))
		for i, r := range ts.Omitted {
			r.Bytes = max(r.Bytes, 0)
			t.ledger[i] = r
			t.bytes += r.Bytes
		}
	}
	if !t.streamOpen && ts.StreamOpen && !openLast && ts.OmittedRun != 0 && t.len() == 0 && t.held() > 0 {
		// The open run's entry is the ledger's last placeholder: a stream
		// entry, which answers no tool update. Its record is the run's
		// figure at the cut, min(bytes streamed, StreamText) (X25): the
		// run's whole length while it fit the cap, the cap once it did not.
		// Either way a chunk of n bytes takes it to min(figure + n,
		// StreamText), as it takes the first model's entry, so the run's
		// length starts from the figure (a record over the cap, which no
		// model writes, is taken as the cap).
		t.streamOpen = true
		t.omittedRun = ts.OmittedRun
		run := &t.ledger[len(t.ledger)-1]
		run.Tool = ""
		if run.Bytes > t.streamCap {
			t.bytes -= run.Bytes - t.streamCap
			run.Bytes = t.streamCap
		}
		t.runLen = run.Bytes
	}
	for i := range t.ledger {
		tid := t.ledger[i].Tool
		if tid == "" {
			continue
		}
		// A row this transcript holds answers its own updates; of two
		// placeholders naming one id, the newer does (neither happens in a
		// snapshot a model cut: a model holds one row per tool id).
		if _, held := t.tools[tid]; held {
			t.ledger[i].Tool = ""
			continue
		}
		if t.ptools == nil {
			t.ptools = make(map[string]int)
		}
		if j, ok := t.ptools[tid]; ok {
			t.ledger[j].Tool = ""
		}
		t.ptools[tid] = i
	}
	// What updates in place added since the first model last enforced its
	// budget is not known here, and only its invariant reads it (grown): the
	// next append or chunk trims on both models alike.
	if t.count() > 1 && t.bytes > t.maxBytes {
		t.grown = t.bytes - t.maxBytes
	}
}

// restoreRun makes e the open run: its tail goes back into the builder, led by
// "…" when the run's beginning was cut, so the next chunk's tail is the one
// the first model computes from the whole run (TestRestoreContinuesEvery-
// ContinuationState proves it at the cap). The run's length is the text's
// when it was not cut, and past the cap when it was: all the accounting and
// the tail read of it (X25).
func (t *Transcript) restoreRun(e *Entry, tailCut bool) {
	text := e.Text
	cut := tailCut && strings.HasPrefix(text, ellipsis)
	if cut {
		text = text[len(ellipsis):]
	}
	t.bufReserve(len(text))
	t.buf = append(t.buf[:0], text...)
	t.runLen = 0
	t.streamed(len(text))
	if cut {
		t.runLen = t.streamCap + 1
	}
	e.Text = ""
	e.Streaming = true
	e.Cut = t.runCut()
	e.Bytes = t.bytesOf(e)
	t.streamOpen = true
	t.openEnd = e.End
}
