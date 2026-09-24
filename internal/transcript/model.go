package transcript

import (
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/charliek/craze/internal/agent"
)

// Options configures a Model.
type Options struct {
	// Bounds are what the model retains; a zero field is DefaultBounds'.
	Bounds Bounds
	// Clock stamps an event whose At is zero, which only a unit test's
	// fixture carries — every production event is stamped. The engine's
	// instance passes nil, so no callback ever runs inside the log's
	// publishing boundary and a zero At stays zero; the TUI's passes its own
	// clock, which keeps today's behaviour for unstamped fixtures (§3.2).
	Clock func() time.Time
	// ErrText reads an EventError's text, for a model that is NOT folded under
	// the log's publishing boundary and is handed the publisher's own error
	// values — the TUI's, which folds its primary (plan 024 §3.8). It is called
	// once per error event that is not an *agent.RemoteError, on the folding
	// goroutine, and its answer is the entry's text as given.
	//
	// nil means the fold never reads an error that is not an
	// *agent.RemoteError. The engine's instance passes nil: the log hands its
	// observer a *agent.RemoteError for every error (agent.EventLog.Observe),
	// as every decoding client receives one, and a RemoteError's text is its
	// Message, read with a plain type assertion. With neither — a unit test's
	// arbitrary error and a nil ErrText — the error is held unread, its entry
	// is kept whatever its text, and it is charged errValueBytes (Entry.Err).
	//
	// What a decoding client holds is the message as the JSON carries it,
	// each invalid UTF-8 byte replaced by U+FFFD; the log makes the same
	// replacement in the observer's RemoteError, so the engine's instance and
	// a decoding client account the same bytes. An ErrText answer is not
	// normalised: a client that must agree byte for byte with them returns
	// valid UTF-8 (every message craze itself builds is; a path inside an os
	// error need not be).
	ErrText func(error) string
	// Incarnation is the event log's incarnation the model is folded from,
	// carried for the snapshot a client attaches from.
	Incarnation string
}

// Model is one session as every client agrees on it (plan 024 §3.2): the
// main transcript and one per sub-agent, the roster, the open asks, the turn,
// the settings, the queue and the todo list, folded from the event stream by
// Fold. Every method takes mu and nothing else; a reader gets immutable
// pointers or copies, so nothing it holds changes under it.
type Model struct {
	mu sync.Mutex

	// Main is the main session's transcript. It is fixed at New.
	Main *Transcript

	clock       func() time.Time
	readErr     func(error) string // Options.ErrText
	bounds      Bounds
	incarnation string

	// seq is the Seq of the last event folded that carried one past it: the
	// model is the folded prefix up to seq.
	seq uint64
	// local is the X2 counter: the last {0, n} id handed out.
	local uint32
	// idSeq and idN name the current event's entries (X2): {idSeq, idN++},
	// or {0, ++local} when idSeq is 0.
	idSeq uint64
	idN   uint32
	// fc is what the current Fold changed, turned into its Change at the end.
	fc foldCtx
	// lockHeld, when a test sets it, is told how long each Snapshot held mu
	// (TestSnapshotHoldsTheModelLockBriefly). Production leaves it nil.
	lockHeld func(time.Duration)

	subs     map[string]*Transcript
	subOrder []string

	agents     map[string]rosterRow
	agentOrder []string
	// finishSeq is the roster's finish counter (subagents.go's
	// subagentFinishSeq): a row's place in finish order breaks EndedAt ties.
	finishSeq uint64

	todos []agent.Todo
	// todosTruncated and queueTruncated are the marks a snapshot this model
	// was restored from left on the todo list and on queue rows by id (State's
	// TodosTruncated and TruncatedQueue); a model folded from the event stream
	// never sets them, and the event that replaces the list or the row clears
	// its mark.
	todosTruncated bool
	queueTruncated map[string]bool
	// asks are the open asks and ended the last-ended list, each keyed by
	// id in the order they opened or ended (keyedList): no scan under the
	// boundary, however many asks are open.
	asks      keyedList[Ask]
	ended     keyedList[AskEnding]
	turn      Turn
	replaying bool
	settings  Settings
	queue     []agent.QueuedPrompt
}

// rosterRow is one roster entry: the last SubagentInfo the stream carried for
// it, its place in finish order (0 while it has not finished this run), and
// whether a snapshot this model was restored from carried only the head of its
// Prompt or Output (the next roster event for it replaces the row whole).
type rosterRow struct {
	info      agent.SubagentInfo
	finish    uint64
	truncated bool
}

// Ask is one open ask: what was asked, and when.
type Ask struct {
	ID   string
	Kind agent.AskKind
	// Body is the opening: exactly one of its pointers is set, the event's own.
	Body agent.AskBody
	At   time.Time
	// Truncated reports that the snapshot this model was restored from carried
	// only the head of some text of the ask — any string its body carries (a
	// plan's, a question's, a permission's, every one of them) or its ID, over
	// ItemCap (plan 024 §3.5) — so a client can say so. A model folded from
	// the event stream never sets it; a new opening of the same id replaces the
	// ask whole.
	Truncated bool
}

// AskEnding is one ask's ending, as the last-ended list keeps it: who ended it
// and how, for a client that wants it. The answer notes a client writes are
// its own (§3.3), so no entry is ever drawn from one.
type AskEnding struct {
	ID      string
	Kind    agent.AskKind
	Outcome agent.AskOutcome
	By      string
	At      time.Time
}

// maxEnded bounds the last-ended ask list.
const maxEnded = 256

// Turn is the turn the stream last started, and the agent's own turn if one
// is running.
type Turn struct {
	// ID is the running turn's id, "" once it ended; Text, Origin and At are
	// the last started's.
	ID, Text, Origin string
	At               time.Time
	// Foreign is the last foreign-turn bracket: Running while the agent runs
	// a turn of its own.
	Foreign *agent.ForeignTurnInfo
	// Truncated reports that the snapshot this model was restored from
	// carried only the head of Text, over ItemCap (plan 024 §3.5), so a
	// client can say so. A model folded from the event stream never sets it;
	// the next turn started replaces Text whole.
	Truncated bool
}

// Settings is every StateDelta section as the last delta that carried it left
// it: each a full replacement, nil in a delta meaning "untouched" and a
// non-nil empty value "cleared". An empty list is kept as nil.
type Settings struct {
	Title, Mode, Model string
	Config             []agent.ConfigOption
	Commands           []agent.CommandInfo
	Plugins            []agent.PluginCommand
	// SendNow is the engine's armed send-now; Armed false means none.
	SendNow agent.SendNowState
	// Truncated names the sections the snapshot this model was restored from
	// carried only the head of some string of (over ItemCap, plan 024 §3.5),
	// so a client can say so. A model folded from the event stream never sets
	// it; the next delta that carries a section replaces it whole and clears
	// that section's mark.
	Truncated SettingsTruncated
}

// SettingsTruncated is Settings' truncation mark, one per section.
type SettingsTruncated struct {
	Title, Mode, Model        bool
	Config, Commands, Plugins bool
	SendNow                   bool
}

// New builds an empty model.
func New(o Options) *Model {
	m := &Model{
		clock:       o.Clock,
		readErr:     o.ErrText,
		bounds:      o.Bounds.withDefaults(),
		incarnation: o.Incarnation,
		subs:        make(map[string]*Transcript),
		agents:      make(map[string]rosterRow),
	}
	m.Main = newTranscript(m, "", m.bounds.MainEntries, m.bounds.MainBytes)
	m.fc.reset()
	return m
}

// now is the model's clock, and the zero time when it has none — the engine's
// instance, where nothing may run a callback.
func (m *Model) now() time.Time {
	if m.clock == nil {
		return time.Time{}
	}
	return m.clock()
}

// errText is an error's text, and whether the fold may know it: a
// *agent.RemoteError's, by a plain type assertion — its Error is agent's own
// and returns its Message, so no foreign code runs, and errors.As, which
// would run the chain's methods, is never used — else Options.ErrText's
// answer when the model has one, else unknown (nothing is read).
func (m *Model) errText(err error) (string, bool) {
	if re, ok := err.(*agent.RemoteError); ok {
		return re.Error(), true
	}
	if m.readErr != nil {
		return m.readErr(err), true
	}
	return "", false
}

// stamp is the time a row drawn from an event is written at: the event's own
// At, and the clock only for an event that carries none.
func (m *Model) stamp(at time.Time) time.Time {
	if !at.IsZero() {
		return at
	}
	return m.now()
}

// newID names the next entry the current event creates (X2).
func (m *Model) newID() EntryID {
	if m.idSeq != 0 {
		id := EntryID{Seq: m.idSeq, N: m.idN}
		m.idN++
		return id
	}
	m.local++
	return EntryID{N: m.local}
}

// begin opens one event's fold: an event past the model's Seq advances it and
// names its entries after itself; any other keeps the Seq and names them from
// the local counter.
func (m *Model) begin(seq uint64) {
	if seq > m.seq {
		m.seq = seq
		m.idSeq = seq
	} else {
		m.idSeq = 0
	}
	m.idN = 0
	m.fc.reset()
}

// ensureSub is the child's transcript, created on first use with the sub
// bounds; "" is the main transcript (the TUI's ensureSub).
func (m *Model) ensureSub(id string) *Transcript {
	if id == "" {
		return m.Main
	}
	if t := m.subs[id]; t != nil {
		return t
	}
	t := newTranscript(m, id, m.bounds.SubEntries, m.bounds.SubBytes)
	m.subs[id] = t
	m.subOrder = append(m.subOrder, id)
	return t
}

// dropSub forgets a child's transcript, which goes with its evicted roster row.
func (m *Model) dropSub(id string) {
	if _, ok := m.subs[id]; !ok {
		return
	}
	delete(m.subs, id)
	if i := slices.Index(m.subOrder, id); i >= 0 {
		m.subOrder = slices.Delete(m.subOrder, i, i+1)
	}
}

// ---------------------------------------------------------------- Change

// Change is what one Fold did to the model's transcripts, so a client
// re-renders only that (plan 024 §3.2). It is a value with no slices, maps or
// pointers: the engine's fold discards it without allocating.
type Change struct {
	// Scope names the transcript whose entries changed: "" for the main
	// transcript, a child's id for that child's. It means nothing unless one
	// of the entry fields below is set — a fold that changed only state (the
	// roster, an ask, a section, the queue, the turn) or nothing at all has
	// Scope "" and every entry field zero (see Entries).
	Scope string
	// Touched are the entries replaced in place, the zero id for none: a
	// chunk touches one, and a tool update two — its row and the run it
	// closed above it.
	Touched [2]EntryID
	// AppendedFrom and AppendedTo are the first and last entry appended, both
	// zero for none; the ones between are the entries between them in the
	// transcript.
	AppendedFrom, AppendedTo EntryID
	// Dropped is how many entries were trimmed off the front.
	Dropped int
	// State reports that the state projection (State) may have changed.
	State bool
}

// Entries reports whether the fold changed any entry of Scope's transcript.
func (c Change) Entries() bool {
	return !c.Touched[0].IsZero() || !c.AppendedFrom.IsZero() || c.Dropped > 0
}

// foldCtx accumulates one Fold's Change. Ordinals, not ids, track the
// appended range, so an entry appended and then trimmed by the same fold is
// clipped out of it.
type foldCtx struct {
	t        *Transcript
	touched  [2]EntryID
	from, to int
	dropped  int
	state    bool
}

func (fc *foldCtx) reset() { *fc = foldCtx{from: -1, to: -1} }

func (m *Model) noteAppended(t *Transcript, ord int) {
	m.fc.t = t
	if m.fc.from < 0 {
		m.fc.from = ord
	}
	m.fc.to = ord
}

func (m *Model) noteTouched(t *Transcript, id EntryID) {
	m.fc.t = t
	switch {
	case m.fc.touched[0] == id || m.fc.touched[1] == id:
	case m.fc.touched[0].IsZero():
		m.fc.touched[0] = id
	case m.fc.touched[1].IsZero():
		m.fc.touched[1] = id
	}
}

func (m *Model) noteDropped(t *Transcript) {
	if m.fc.t == nil {
		m.fc.t = t
	}
	if m.fc.t == t {
		m.fc.dropped++
	}
}

// finish turns the fold's record into its Change: touched entries a trim has
// since dropped are left out, and the appended range is clipped to what the
// transcript still holds.
func (m *Model) finish() Change {
	fc := &m.fc
	c := Change{State: fc.state}
	t := fc.t
	if t == nil {
		return c
	}
	c.Scope = t.agent
	c.Dropped = fc.dropped
	if fc.dropped == 0 {
		// Nothing left the transcript, so every touched entry is still in it.
		c.Touched = fc.touched
	} else {
		n := 0
		for _, id := range fc.touched {
			if id.IsZero() {
				continue
			}
			if _, e := t.lookup(id); e != nil {
				c.Touched[n] = id
				n++
			}
		}
	}
	if fc.from >= 0 {
		first := t.base + t.head
		from := max(fc.from, first)
		if from <= fc.to {
			c.AppendedFrom = t.ents[from-t.base].ID
			c.AppendedTo = t.ents[fc.to-t.base].ID
		}
	}
	return c
}

// ----------------------------------------------------------------- reads

// Incarnation is the log incarnation the model was built for.
func (m *Model) Incarnation() string { return m.incarnation }

// Seq is the Seq of the last event the model folded.
func (m *Model) Seq() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.seq
}

// Sub is a child's transcript, nil when the model holds none for id.
func (m *Model) Sub(id string) *Transcript {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.subs[id]
}

// Subs is the ids of the child transcripts, in the order they were created.
func (m *Model) Subs() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.subOrder...)
}

// EndedAsks is the last-ended list, oldest first, at most 256.
func (m *Model) EndedAsks() []AskEnding {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ended.values()
}

// State is the model's state projection (plan 024 §3.3, §3.7): everything
// that converges when an event is re-applied. It is a value — slices and maps
// of its own over immutable payloads — compared with reflect.DeepEqual by the
// convergence test and by the engine's exactness test.
type State struct {
	// Seq is the last event folded.
	Seq uint64
	// Asks are the open asks, in the order they opened.
	Asks     []Ask
	Settings Settings
	// Queue is the message queue, front first.
	Queue []agent.QueuedPrompt
	// Agents is the roster, in the order the rows first appeared.
	Agents []agent.SubagentInfo
	Todos  []agent.Todo
	// TodosTruncated reports that the snapshot this model was restored from
	// carried only the head of some todo's text (over ItemCap, plan 024 §3.5);
	// never so for a model folded from the event stream. The next todo list
	// replaces the list whole and clears it.
	TodosTruncated bool
	// TruncatedQueue names the queue rows whose Text the snapshot this model
	// was restored from carried only the head of (over ItemCap); nil when
	// there is none, which is always so for a model folded from the event
	// stream. The next queue event for a row replaces or removes it and takes
	// it out of this set.
	TruncatedQueue map[string]bool
	Turn           Turn
	// Replaying is true inside a session/load replay bracket.
	Replaying bool
	// Tools is every tool's last state by id: the payload of each tool entry
	// a transcript still holds under its id. nil when there is none. A model
	// restored from a windowed snapshot holds no payload for a tool whose row
	// the window omitted (its placeholder carries only an id and a size, X23),
	// so its Tools is the first model's restricted to the rows it shares with
	// it: the exactness of a windowed restore is the suffix, the non-tool
	// state, and the tools of the suffix (plan 024 §3.5, X23).
	Tools map[ToolKey]*agent.ToolEvent
	// TruncatedAgents names the roster rows whose Prompt or Output the
	// snapshot this model was restored from carried only the head of (over
	// ItemCap, plan 024 §3.5); nil when there is none, which is always so for
	// a model folded from the event stream. The next roster event for a row
	// replaces it whole and takes it out of this set.
	TruncatedAgents map[string]bool
}

// ToolKey names one tool call: the transcript it is in ("" main, else the
// child's id) and its id.
type ToolKey struct{ Agent, ID string }

// History is the model's history projection: every transcript's entries as
// values, with the open entry's Text resolved to its tail, and each one's
// continuation state. It is what the engine's exactness test (C3) compares
// beside State. An error value is carried as it was folded: the engine's
// instance and a decoding client both hold *agent.RemoteError values with the
// same fields (agent.EventLog.Observe), which compare by value.
type History struct {
	Main TranscriptHistory
	// Subs are the child transcripts, in the order they were created.
	Subs []SubHistory
}

// TranscriptHistory is one transcript in the history projection.
type TranscriptHistory struct {
	Entries []Entry
	Trimmed bool
	// Windowed reports that the transcript was restored from a snapshot that
	// omitted its older entries to fit its byte budget (plan 024 §3.5): a
	// client draws the trim note for Trimmed || Windowed.
	Windowed    bool
	StreamOpen  bool
	TodoPlanned int
	TodoDone    bool
}

// SubHistory is one child transcript in the history projection.
type SubHistory struct {
	ID string
	TranscriptHistory
}

// State returns the state projection, cut under the lock and built after it.
func (m *Model) State() State {
	c := m.cut()
	return c.state()
}

// History returns the history projection, cut under the lock and built after
// it.
func (m *Model) History() History {
	c := m.cut()
	return c.history()
}

// ----------------------------------------------------------------- the cut

// cut is the model at one Seq, taken under mu with pointer copies only —
// entries, roster and asks by value or pointer, string headers, immutable
// payload pointers, never bytes — except each open entry's tail (≤
// StreamText), copied once into an entry of the cut's own. It holds the
// continuation state a restored model needs too (§3.5): each transcript's
// StreamOpen, whether its tail was cut, the todo-note dedupe, the Seq, the X2
// counter and the roster's finish order. The snapshot (C2) is built from it
// after the lock is released; State and History are too.
type cut struct {
	seq         uint64
	local       uint32
	incarnation string
	main        transcriptCut
	subs        []subCut
	agents      []rosterRow
	finishSeq   uint64
	asks        []Ask
	ended       []AskEnding
	todos       []agent.Todo
	turn        Turn
	replaying   bool
	settings    Settings
	queue       []agent.QueuedPrompt
	// todosTruncated and queueTruncated are the model's marks (the map is
	// the cut's own copy).
	todosTruncated bool
	queueTruncated map[string]bool
}

type transcriptCut struct {
	// entries is oldest first; an open stream entry is a copy carrying its
	// tail as Text, still marked Streaming.
	entries    []*Entry
	trimmed    bool
	streamOpen bool
	// tailCut reports that the open entry's tail lost the run's beginning (it
	// is led by "…"), which a restored builder has to know.
	tailCut     bool
	todoPlanned int
	todoDone    bool
	// The window a restored transcript carries (Transcript.windowed and the
	// rest): what the snapshot it came from omitted. omitted is the ledger's
	// live placeholders (X23), copied: the fold re-accounts them in place.
	windowed   bool
	dropped    int
	omitted    []Omitted
	omittedRun Kind
}

type subCut struct {
	id string
	t  transcriptCut
}

func (m *Model) cut() cut {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cutLocked()
}

// snapshotCut is Snapshot's critical section, and all of it (plan 024 §3.4):
// the cut, under mu, timed for a test that asks.
func (m *Model) snapshotCut() cut {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.lockHeld == nil {
		return m.cutLocked()
	}
	start := time.Now()
	c := m.cutLocked()
	m.lockHeld(time.Since(start))
	return c
}

func (m *Model) cutLocked() cut {
	c := cut{
		seq:         m.seq,
		local:       m.local,
		incarnation: m.incarnation,
		main:        m.Main.cutLocked(),
		finishSeq:   m.finishSeq,
		asks:        m.asks.values(),
		ended:       m.ended.values(),
		todos:       m.todos,
		turn:        m.turn,
		replaying:   m.replaying,
		settings:    m.settings,
		queue:       append([]agent.QueuedPrompt(nil), m.queue...),
	}
	c.todosTruncated = m.todosTruncated
	if len(m.queueTruncated) > 0 {
		c.queueTruncated = maps.Clone(m.queueTruncated)
	}
	if len(m.subOrder) > 0 {
		c.subs = make([]subCut, 0, len(m.subOrder))
		for _, id := range m.subOrder {
			c.subs = append(c.subs, subCut{id: id, t: m.subs[id].cutLocked()})
		}
	}
	if len(m.agentOrder) > 0 {
		c.agents = make([]rosterRow, 0, len(m.agentOrder))
		for _, id := range m.agentOrder {
			c.agents = append(c.agents, m.agents[id])
		}
	}
	return c
}

func (t *Transcript) cutLocked() transcriptCut {
	tc := transcriptCut{
		entries:     append([]*Entry(nil), t.live()...),
		trimmed:     t.trimmed,
		streamOpen:  t.streamOpen,
		todoPlanned: t.todoPlanned,
		todoDone:    t.todoDone,
		windowed:    t.windowed,
		dropped:     t.dropped,
		omittedRun:  t.omittedRun,
	}
	if t.held() > 0 {
		tc.omitted = append([]Omitted(nil), t.ledger[t.lhead:]...)
	}
	if n := len(tc.entries); t.streamOpen && n > 0 && tc.entries[n-1].Streaming {
		// A fresh copy carrying the run's end, accounting and tail: the
		// stored entry's End and Bytes are its opening's (X24).
		open := *tc.entries[n-1]
		open.End = t.openEnd
		open.Bytes = t.bytesOf(tc.entries[n-1])
		open.Text = t.tail()
		_, tc.tailCut = t.tailStart()
		tc.entries[n-1] = &open
	}
	return tc
}

func (c *cut) state() State {
	s := State{
		Seq:       c.seq,
		Asks:      c.asks,
		Settings:  c.settings,
		Queue:     c.queue,
		Todos:     c.todos,
		Turn:      c.turn,
		Replaying: c.replaying,
	}
	s.TodosTruncated = c.todosTruncated
	for _, q := range c.queue {
		if c.queueTruncated[q.ID] {
			if s.TruncatedQueue == nil {
				s.TruncatedQueue = make(map[string]bool)
			}
			s.TruncatedQueue[q.ID] = true
		}
	}
	if len(s.Asks) == 0 {
		s.Asks = nil
	}
	if len(s.Queue) == 0 {
		s.Queue = nil
	}
	if len(c.agents) > 0 {
		s.Agents = make([]agent.SubagentInfo, len(c.agents))
		for i := range c.agents {
			s.Agents[i] = c.agents[i].info
			if c.agents[i].truncated {
				if s.TruncatedAgents == nil {
					s.TruncatedAgents = make(map[string]bool)
				}
				s.TruncatedAgents[c.agents[i].info.ID] = true
			}
		}
	}
	addTools := func(agentID string, tc *transcriptCut) {
		for _, e := range tc.entries {
			if e.Kind != KindTool || e.Tool == nil || e.Tool.ID == "" {
				continue
			}
			if s.Tools == nil {
				s.Tools = make(map[ToolKey]*agent.ToolEvent)
			}
			s.Tools[ToolKey{Agent: agentID, ID: e.Tool.ID}] = e.Tool
		}
	}
	addTools("", &c.main)
	for i := range c.subs {
		addTools(c.subs[i].id, &c.subs[i].t)
	}
	return s
}

func (c *cut) history() History {
	h := History{Main: c.main.history()}
	if len(c.subs) > 0 {
		h.Subs = make([]SubHistory, len(c.subs))
		for i := range c.subs {
			h.Subs[i] = SubHistory{ID: c.subs[i].id, TranscriptHistory: c.subs[i].t.history()}
		}
	}
	return h
}

func (tc *transcriptCut) history() TranscriptHistory {
	th := TranscriptHistory{
		Trimmed:     tc.trimmed,
		Windowed:    tc.windowed,
		StreamOpen:  tc.streamOpen,
		TodoPlanned: tc.todoPlanned,
		TodoDone:    tc.todoDone,
	}
	if len(tc.entries) > 0 {
		th.Entries = make([]Entry, len(tc.entries))
		for i, e := range tc.entries {
			th.Entries[i] = *e
		}
	}
	return th
}
