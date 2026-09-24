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
// §3.5): an open ask's body text — a plan's Plan and Overview, a question's
// prompts and its options' labels and descriptions, a permission's tool text
// — a roster row's Prompt and Output, and the current turn's Text are each
// carried as their head, cut back to a rune boundary, when they are longer,
// and the ask, row or turn is marked Truncated (r3 finding 3: the turn's Text
// is otherwise mandatory and uncapped, so a multi-MiB prompt makes every
// snapshot ErrSnapshotTooLarge until the next turn).
const ItemCap = 256 << 10

// ErrSnapshotTooLarge is Snapshot's refusal: the mandatory sections alone, or
// they and the main transcript's newest entry, encode to more than the byte
// budget. It is never a silent drop (plan 024 §3.5).
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
	// Settings and Queue are the model's state (State), each ask capped at
	// ItemCap.
	Todos []agent.Todo
	Asks  []Ask
	// Ended is the last-ended ask list (Model.EndedAsks), carried so a
	// restored model's list and its later evictions are the first one's.
	Ended     []AskEnding
	Turn      Turn
	Replaying bool
	Settings  Settings
	Queue     []agent.QueuedPrompt
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
	// OmittedTools names the tool rows the window dropped, by tool id: an
	// update to one of them applies to nothing on the restored model.
	// OmittedRun is the kind of the open run's entry when the window dropped
	// it too (a child that fitted nothing, mid-stream): the run's next chunks
	// apply to nothing until something ends it.
	OmittedTools map[string]EntryID
	OmittedRun   Kind
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
// Under the model's lock it only takes the cut: pointer copies of the entries,
// the roster and the asks, and each open run's tail (≤ StreamText). Everything
// else runs after the lock is released — the per-item caps, every call to an
// error's methods, the encoding and the window — so a fold waits on a
// snapshot for the length of the cut alone (§3.4).
//
// The filling order is the plan's: first the mandatory sections — the roster,
// the todos, the asks, the ended list, the turn, the settings, the queue, and
// every transcript's continuation fields — and if they alone do not fit,
// ErrSnapshotTooLarge; then the main transcript's entries, newest first, until
// the next would not fit, the newest one being required (ErrSnapshotTooLarge);
// then each child's, in the order they were created, with what remains. Every
// entry is encoded once, and the window counts the bytes the encoding will
// have, not an estimate: the snapshot's encoding is at most budget bytes.
func (m *Model) Snapshot(budget int) (*Snapshot, error) {
	s, _, err := m.snapshotSized(budget)
	return s, err
}

// snapshotSized is Snapshot with the encoded size its window counted, which a
// test holds against the encoding's real length.
func (m *Model) snapshotSized(budget int) (*Snapshot, int, error) {
	if budget <= 0 {
		budget = DefaultSnapshotBytes
	}
	c := m.snapshotCut()
	return c.snapshot(budget)
}

// snapshot builds the snapshot from a cut, after the lock (see Snapshot).
func (c *cut) snapshot(budget int) (*Snapshot, int, error) {
	s := &Snapshot{
		Version:     SnapshotVersion,
		Incarnation: c.incarnation,
		Seq:         c.seq,
		Local:       c.local,
		FinishSeq:   c.finishSeq,
		Todos:       c.todos,
		Ended:       c.ended,
		Turn:        capTurn(c.turn),
		Replaying:   c.replaying,
		Settings:    c.settings,
		Queue:       c.queue,
	}
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
	// transcript at its continuation fields with no entries.
	total := len(hdr) - 1 + len(`,"main":`) + len(`}`)
	if len(c.subs) > 0 {
		total += len(`,"subs":[]`) + len(c.subs) - 1
	}
	for _, w := range wins {
		total += w.cur
	}
	if total > budget {
		return nil, 0, fmt.Errorf("%w: the mandatory sections encode to %d bytes, over a budget of %d", ErrSnapshotTooLarge, total, budget)
	}
	// (2) The main transcript's tail, newest first; its newest entry must fit.
	if total, err = wins[0].fill(total, budget); err != nil {
		return nil, 0, err
	}
	if main := wins[0]; main.n > 0 && main.k == 0 {
		return nil, 0, fmt.Errorf("%w: the main transcript's newest entry does not fit beside %d bytes of mandatory state in a budget of %d", ErrSnapshotTooLarge, total, budget)
	}
	// (3) Each child in the order it was created, with what remains.
	for _, w := range wins[1:] {
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
// written by its own appendScalars and appendToolMember — so the sum is the
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
	// toolLen is, per entry oldest first, the length of the "id":"entry"
	// member the entry adds to OmittedTools when the window drops it (0 for
	// none); restTools and restLen sum it over the entries not included.
	toolLen            []int
	restTools, restLen int
	// carried is what a restored transcript's own window already omits.
	carriedTools, carriedLen int
	openLast                 bool
	cur                      int
	scratch                  []byte
}

func newWindow(jw *jsonWriter, tc *transcriptCut, id string, sub bool) *window {
	w := &window{jw: jw, tc: tc, id: id, sub: sub, n: len(tc.entries)}
	w.openLast = tc.streamOpen && w.n > 0 && tc.entries[w.n-1].Streaming
	seen := make(map[string]bool, len(tc.omitted))
	for tid, eid := range tc.omitted {
		seen[tid] = true
		w.carriedTools++
		w.carriedLen += toolMemberLen(tid, eid)
	}
	w.toolLen = make([]int, w.n)
	for i, e := range tc.entries {
		tid := entryToolID(e)
		if tid == "" || seen[tid] {
			continue
		}
		seen[tid] = true
		w.toolLen[i] = toolMemberLen(tid, e.ID)
		w.restTools++
		w.restLen += w.toolLen[i]
	}
	w.cur = w.size(0, 0, w.restTools, w.restLen)
	return w
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
// entries in (their encodings entLen bytes long) and tools omitted tool
// members (toolsLen bytes) beside the carried ones: the scalar members as the
// encoder writes them, then OmittedTools and Entries, each member followed by
// a comma but the last.
func (w *window) size(k, entLen, tools, toolsLen int) int {
	var members int
	w.scratch, members = appendScalars(w.scratch[:0], w.scalars(k))
	sum := len(w.scratch) // each scalar member with its comma
	if t := w.carriedTools + tools; t > 0 {
		sum += len(`"omittedTools":{}`) + w.carriedLen + toolsLen + t - 1 + 1
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
// is the snapshot's size as it stands, and the result what it becomes.
func (w *window) fill(total, budget int) (int, error) {
	for w.k < w.n {
		i := w.n - 1 - w.k
		e := snapEntry(w.tc.entries[i])
		b, err := encodeEntry(w.jw, &e)
		if err != nil {
			return total, err
		}
		tools, toolsLen := w.restTools, w.restLen
		if w.toolLen[i] > 0 {
			tools--
			toolsLen -= w.toolLen[i]
		}
		next := w.size(w.k+1, w.entLen+len(b), tools, toolsLen)
		if total-w.cur+next > budget {
			break
		}
		total += next - w.cur
		w.cur = next
		w.k++
		w.entLen += len(b)
		w.ents = append(w.ents, e)
		w.restTools, w.restLen = tools, toolsLen
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
	if t := w.carriedTools + w.restTools; t > 0 {
		ts.OmittedTools = make(map[string]EntryID, t)
		for tid, eid := range w.tc.omitted {
			ts.OmittedTools[tid] = eid
		}
		for i := range w.n - w.k {
			if w.toolLen[i] > 0 {
				e := w.tc.entries[i]
				ts.OmittedTools[e.Tool.ID] = e.ID
			}
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

// capAsk is an open ask with its body's text capped at ItemCap: the payload
// is copied only where something is cut, so the event's own is never written.
func capAsk(a Ask) Ask {
	cut := false
	if p := a.Body.Plan; p != nil && (len(p.Plan) > ItemCap || len(p.Overview) > ItemCap) {
		cp := *p
		cp.Plan, _ = headOf(cp.Plan, ItemCap)
		cp.Overview, _ = headOf(cp.Overview, ItemCap)
		a.Body.Plan, cut = &cp, true
	}
	if q := a.Body.Question; q != nil && questionOverCap(q) {
		cp := *q
		cp.Questions = slices.Clone(q.Questions)
		for i := range cp.Questions {
			qq := &cp.Questions[i]
			qq.Prompt, _ = headOf(qq.Prompt, ItemCap)
			if optionsOverCap(qq.Options) {
				qq.Options = slices.Clone(qq.Options)
				for j := range qq.Options {
					o := &qq.Options[j]
					o.Label, _ = headOf(o.Label, ItemCap)
					o.Description, _ = headOf(o.Description, ItemCap)
				}
			}
		}
		a.Body.Question, cut = &cp, true
	}
	if p := a.Body.Permission; p != nil && len(p.Tool) > ItemCap {
		cp := *p
		cp.Tool, _ = headOf(cp.Tool, ItemCap)
		a.Body.Permission, cut = &cp, true
	}
	a.Truncated = a.Truncated || cut
	return a
}

func questionOverCap(q *agent.QuestionEvent) bool {
	for i := range q.Questions {
		if len(q.Questions[i].Prompt) > ItemCap || optionsOverCap(q.Questions[i].Options) {
			return true
		}
	}
	return false
}

func optionsOverCap(opts []agent.Option) bool {
	for i := range opts {
		if len(opts[i].Label) > ItemCap || len(opts[i].Description) > ItemCap {
			return true
		}
	}
	return false
}

// ------------------------------------------------------------------ Restore

// Restore builds the model a snapshot was cut from, as far as the snapshot
// carries it (plan 024 §3.5): on the history and state projections it equals
// the snapshotted model for the entries it retained, and folding the events
// after s.Seq gives what the first model gives folding them — the next chunk
// of an open run grows its tail (StreamOpen, TailCut), a repeated todo list
// draws no second note, an update to a tool whose row the window dropped
// applies to nothing (OmittedTools), an update to a tool the model itself
// trimmed appends a new row as it does on the first, and later entries take
// the ids the first model gives them (Seq, Local). The entries keep the
// snapshot's EntryIDs, and each accounts its bytes by this model's rule
// (Entry.Bytes is recomputed, not read).
//
// o is as for New; its Incarnation is replaced by the snapshot's, and its
// Bounds must be the first model's for the two to trim alike (DefaultBounds
// for every instance, §3.2 (b)). Restore trims nothing: a model's own entries
// always fit its entry cap, and its byte budget is enforced at the next append
// or chunk, as on the first model. Restore keeps what it needs of s — the
// payloads by pointer, which nothing writes — and s may be dropped after it.
// It never fails: what is inconsistent in a hand-built snapshot (an entry with
// the zero id or a duplicate one, a child or roster id repeated or empty, a
// streaming entry that is not the open run's) is skipped or taken as closed.
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
			e.Bytes = entryBytes(&e)
		}
		ne := &e
		t.slot[ne.ID] = t.base + len(t.ents)
		t.ents = append(t.ents, ne)
		t.bytes += ne.Bytes
		if tid := entryToolID(ne); tid != "" {
			t.tools[tid] = ne.ID
		}
	}
	if !t.streamOpen && ts.StreamOpen && !openLast && ts.OmittedRun != 0 {
		t.streamOpen = true
		t.omittedRun = ts.OmittedRun
	}
	if len(ts.OmittedTools) > 0 {
		t.omitted = make(map[string]EntryID, len(ts.OmittedTools))
		for tid, eid := range ts.OmittedTools {
			// A row this transcript holds answers its own updates.
			if _, held := t.tools[tid]; tid != "" && !held {
				t.omitted[tid] = eid
			}
		}
	}
	// What updates in place added since the first model last enforced its
	// budget is not known here, and only its invariant reads it (grown): the
	// next append or chunk trims on both models alike.
	if t.len() > 1 && t.bytes > t.maxBytes {
		t.grown = t.bytes - t.maxBytes
	}
}

// restoreRun makes e the open run: its tail goes back into the builder, led by
// "…" when the run's beginning was cut, so the next chunk's tail is the one
// the first model computes from the whole run (TestRestoreContinuesEvery-
// ContinuationState proves it at the cap).
func (t *Transcript) restoreRun(e *Entry, tailCut bool) {
	text := e.Text
	cut := tailCut && strings.HasPrefix(text, ellipsis)
	if cut {
		text = text[len(ellipsis):]
	}
	t.bufReserve(len(text))
	t.buf = append(t.buf[:0], text...)
	t.bufCut = cut
	t.tailAt = 0
	t.advanceTail()
	e.Text = ""
	e.Streaming = true
	e.Bytes = t.tailLen()
	t.streamOpen = true
	t.openEnd = e.End
}
