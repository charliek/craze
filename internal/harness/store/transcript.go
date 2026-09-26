package store

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"slices"

	"charm.land/fantasy"
)

var (
	// ErrNoHeader is Load's error for a file whose first line is missing or
	// is not a valid version-1 session header: without it nothing says whose
	// session this is, so the file is not a transcript.
	ErrNoHeader = errors.New("store: no valid session header")

	// ErrCorrupt is Load's error for a malformed line that is not the last
	// one, and for a line anywhere that breaks the pairing invariant (it then
	// also wraps ErrUnpaired). A torn append can only damage the tail, so
	// damage anywhere else means the file was edited or corrupted, and
	// guessing past it could silently drop half a conversation.
	ErrCorrupt = errors.New("store: malformed line")

	// ErrUnknownEntry is ContextAt's error for a leaf id the transcript does
	// not have.
	ErrUnknownEntry = errors.New("store: unknown entry")
)

// Transcript is a session file's contents: the header and the entries in
// file order. Entries form a tree through ParentID; the leaf H1 continues
// from is the last entry (Leaf). Entries is for reading: the context walk
// relies on an index built as the entries were added.
type Transcript struct {
	Header  Header
	Entries []Entry

	index map[string]int // entry id → position in Entries
	// render is the harness's renderer (Options.Render), for the entries
	// whose text the file does not hold: the Store's transcript has its own,
	// and one Load returns has none.
	render Renderer
}

// Renderer turns the entries whose text the store never keeps, or keeps only
// in part, back into the messages a request sends in their place (plan 028
// §3.15, §3.9). The store composes no text of its own and knows none: the
// harness hands it this at New or Open, and the context (ContextAt) calls it
// at each such entry's place on the path. Each field may be nil, and a
// transcript without a renderer — one Load returns — leaves those messages
// out of its context, as it leaves out a newer craze's types; the compaction
// a summary message would stand for still cuts the context there.
type Renderer struct {
	// Reminder is the message a reminder entry of variant stands for: byte
	// for byte the one the request that carried it sent. ok is false for a
	// variant the harness does not know (a newer craze's), which the context
	// then leaves out.
	Reminder func(variant string) (msg fantasy.Message, ok bool)
	// Summary is the summary message a successful compaction stands for at
	// the head of every context from it on (plan 028 §3.9): a user message
	// whose bytes are a function of c alone, so that every request built
	// after the compaction, in this incarnation or a later one, starts with
	// the same bytes (D-30).
	Summary func(c Compaction) fantasy.Message
}

// Load reads a session file. The header must be valid. A malformed last line
// — a torn tail from a crash mid-append, or the zero bytes some filesystems
// leave after one — is skipped; a malformed line anywhere else is
// ErrCorrupt. A line is malformed when it is not JSON, lacks the envelope
// (type, id, timestamp), repeats an id, names a parent that no earlier line
// has, or is a known entry type whose payload does not decode (including a
// message part whose provider metadata type is not registered with Fantasy).
// An entry type this version does not know is kept, so the parent chain
// through it stays whole, and never reaches the context.
//
// Every line that remains must keep the pairing invariant with the line
// before it (see pairing), and a line that breaks it is ErrCorrupt wherever
// it is — the last line included: a complete, well-formed final line that
// breaks it is not rolled back. No crash can produce one. AppendStep refuses
// to write a step that breaks the invariant, and a crash only takes lines
// off the end of an append, so every whole line left was checked against
// the line before it when it was written. A line whose turn or todos breaks
// its own rules (checkFields), and a reminder whose variant is not a name
// (checkVariant), is ErrCorrupt wherever it is, for the same reason (plan 028
// P14); and so is a compaction whose fields are not of their shapes
// (checkCompaction), or whose tail does not start a step before it on its
// path (checkTail).
//
// Then the incomplete step at the tail, if any, is rolled back
// (dropIncompleteTurn). The store writes each step as one append — held
// changes, held user entries, the reminders and steers that lead it, the
// assistant message, and the tool message when there is one — but a crash
// can persist any prefix of it, including one that ends on a line boundary.
// After the malformed last line is skipped, what can remain of a cut step is
// exactly its first lines, so the tail is the entries after the last complete
// step: changes, user entries, reminders and steers with no answer after
// them, possibly followed by an assistant message whose tool calls have no
// line after it at all, because the tool message was the line the cut
// removed. That missing line, as the
// end of the file, is the only unpaired state rolled back rather than
// refused. None of the tail is history, so a file holding only a header, or
// a header and a user entry, loads with no entries. A compaction entry is an
// append of its own (AppendCompaction), complete on its own line: one after
// the last complete step is kept.
func Load(path string) (*Transcript, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	p, err := parse(data, path)
	if err != nil {
		return nil, err
	}
	p.t.dropIncompleteTurn()
	return p.t, nil
}

// parsed is a session file as Load reads it, before the incomplete step at
// its tail is rolled back, and where in the bytes each line ends: what Open
// needs to cut the file back to the entries Load keeps.
type parsed struct {
	t *Transcript
	// headerEnd and ends[i] are the offsets just past the header's line and
	// entry i's, each line's newline included when it has one.
	headerEnd int
	ends      []int
}

// keptLen is the length of the file's prefix that holds the header and its
// first keep entries.
func (p *parsed) keptLen(keep int) int {
	if keep == 0 {
		return p.headerEnd
	}
	return p.ends[keep-1]
}

// parse is Load's reading of data, the contents of the file at path (named
// only in errors); see Load for every rule.
func parse(data []byte, path string) (*parsed, error) {
	lines := bytes.Split(data, []byte("\n"))
	// A file that ends in a newline splits into a final empty element that
	// is not a line.
	if n := len(lines); n > 0 && len(lines[n-1]) == 0 {
		lines = lines[:n-1]
	}
	if len(lines) == 0 {
		return nil, fmt.Errorf("%w: %s is empty", ErrNoHeader, path)
	}
	h, err := decodeHeader(lines[0])
	if err != nil {
		return nil, fmt.Errorf("%w: %s: line 1: %v", ErrNoHeader, path, err)
	}
	// end is the offset just past a line that starts at start: past its
	// newline, or at the end of data for a last line with none.
	end := func(start int, line []byte) int { return min(start+len(line)+1, len(data)) }
	p := &parsed{t: newTranscript(h), headerEnd: end(0, lines[0])}
	t, pos := p.t, p.headerEnd
	var pair pairing
	for i, line := range lines[1:] {
		e, err := decodeEntry(line)
		if err == nil {
			err = t.check(e)
		}
		if err != nil {
			if i == len(lines)-2 && !errors.Is(err, errInvalid) {
				break // the last line: a torn tail
			}
			return nil, fmt.Errorf("%w: %s: line %d: %v", ErrCorrupt, path, i+2, err)
		}
		// Every rule a whole line can break against the tree — pairing, and
		// where a compaction's tail starts — is checked here, outside the
		// torn-tail branch above, so a whole last line that breaks one is
		// refused, never trimmed (P14).
		parent := t.entry(e.ParentID)
		if err := pair.next(e, parent); err != nil {
			return nil, fmt.Errorf("%w: %s: line %d: %w", ErrCorrupt, path, i+2, err)
		}
		if err := t.checkTail(&e, parent); err != nil {
			return nil, fmt.Errorf("%w: %s: line %d: %w", ErrCorrupt, path, i+2, err)
		}
		t.add(e)
		pos = end(pos, line)
		p.ends = append(p.ends, pos)
	}
	return p, nil
}

// dropIncompleteTurn removes every entry after the last complete step — an
// assistant message with no calls left to answer, or a tool message, which
// Load has checked answers the assistant message just before it — or after
// the last compaction entry, if that comes later: a compaction is written
// alone, and complete on its own line, so it is kept though no step follows
// it (plan 028 §3.2). It runs only once Load has refused every line that
// breaks the invariant, so the one unpaired entry it can meet is an assistant
// message with open calls as the last line — its tool message cut off — and
// that goes, together with the steers, reminders, user entries, changes and
// resume entry written ahead of it (see Load). It returns how many entries it
// kept.
func (t *Transcript) dropIncompleteTurn() int {
	keep := 0
	for i := range t.Entries {
		if e := &t.Entries[i]; stepEnd(e) || e.Type == TypeCompaction {
			keep = i + 1
		}
	}
	for _, e := range t.Entries[keep:] {
		delete(t.index, e.ID)
	}
	t.Entries = t.Entries[:keep]
	return keep
}

func newTranscript(h Header) *Transcript {
	return &Transcript{Header: h, index: map[string]int{}}
}

// check reports why e cannot join the tree: a repeated id, or a parent that
// is not already in it. Requiring the parent to come first is what an
// append-only file guarantees, and it makes a cycle impossible.
func (t *Transcript) check(e Entry) error {
	if _, dup := t.index[e.ID]; dup {
		return fmt.Errorf("entry id %q repeats an earlier one", e.ID)
	}
	if e.ParentID != "" {
		if _, ok := t.index[e.ParentID]; !ok {
			return fmt.Errorf("entry %q names parent %q, which no earlier line has", e.ID, e.ParentID)
		}
	}
	return nil
}

func (t *Transcript) add(e Entry) {
	t.index[e.ID] = len(t.Entries)
	t.Entries = append(t.Entries, e)
}

func (t *Transcript) has(id string) bool {
	_, ok := t.index[id]
	return ok
}

// entry is the entry with id, or nil when there is none (or id is "").
func (t *Transcript) entry(id string) *Entry {
	if i, ok := t.index[id]; ok {
		return &t.Entries[i]
	}
	return nil
}

// last is the last entry, or nil when there is none.
func (t *Transcript) last() *Entry {
	if len(t.Entries) == 0 {
		return nil
	}
	return &t.Entries[len(t.Entries)-1]
}

// Leaf is the id H1 continues from: the last entry, or "" when there is
// none.
func (t *Transcript) Leaf() string {
	if len(t.Entries) == 0 {
		return ""
	}
	return t.Entries[len(t.Entries)-1].ID
}

// walk is the positions in Entries of leaf and each of its ancestors, leaf
// first and the root last; none for leaf "".
func (t *Transcript) walk(leaf string) ([]int, error) {
	if leaf == "" {
		return nil, nil
	}
	i, ok := t.index[leaf]
	if !ok {
		return nil, fmt.Errorf("%w %q", ErrUnknownEntry, leaf)
	}
	var path []int
	for {
		path = append(path, i)
		parent := t.Entries[i].ParentID
		if parent == "" {
			return path, nil
		}
		i = t.index[parent] // check guaranteed it exists and comes earlier
	}
}

// Branch is the entries on the path from the root to leaf, in that order:
// what a resumed session reads its state from and replays, never file order
// (plan 028, "the path"). Leaf "" is none; an unknown leaf is
// ErrUnknownEntry. The entries are copies (Entry.clone): only their messages'
// parts, and the provider options, are shared with the transcript, as
// Context's are, and a caller must not mutate those.
func (t *Transcript) Branch(leaf string) ([]Entry, error) {
	path, err := t.walk(leaf)
	if err != nil {
		return nil, err
	}
	out := make([]Entry, len(path))
	for k, i := range path {
		out[len(path)-1-k] = t.Entries[i].clone()
	}
	return out, nil
}

// clone is a copy of e that a caller can change without changing e: its own
// usage, sub-agent usage rows, todo list, and message parts list. What the
// parts and the message's provider options hold is still shared, as it is
// in every message Context builds: a caller must not mutate it.
func (e Entry) clone() Entry {
	if e.Usage != nil {
		u := *e.Usage
		e.Usage = &u
	}
	e.SubagentUsage = slices.Clone(e.SubagentUsage)
	if e.Todos != nil {
		items := slices.Clone(*e.Todos)
		e.Todos = &items
	}
	e.Message.Content = slices.Clone(e.Message.Content)
	return e
}

// Context is ContextAt from the leaf; see there.
func (t *Transcript) Context(current Model) []fantasy.Message {
	msgs, _ := t.ContextAt(t.Leaf(), current) // the leaf is always known
	return msgs
}

// ContextAt is the history a request from leaf sends: the message entries on
// the path from the root to leaf, in order, and at each reminder entry's place
// the message the harness's renderer makes of it (Renderer) — byte for byte
// the reminder that request carried there, so the history is what was sent
// (plan 028 §3.15). It builds new messages and never changes an entry (the
// file is never rewritten), applying these rules, each of which keeps tool
// calls paired with their results:
//
//   - The latest successful compaction C on the path cuts it (plan 028 §3.9):
//     the history is C's summary message (Renderer's Summary), then the
//     messages of C's tail — the entries from its FirstKeptID up to C — then
//     those of the entries after C. Everything before the tail is what the
//     summary stands for, and is not sent. A failed compaction cuts nothing,
//     and neither does a compaction entry inside a tail: only the latest
//     success's summary is sent. A tail starts a step (checkTail), so it never
//     starts between a call and its results.
//   - Reasoning, and calls the provider executed with their results, are
//     replayed only to the model that produced them: an assistant message's
//     reasoning and provider-executed parts are dropped when its entry's
//     provider or wire model differs from current's. A signed reasoning block
//     is specific to the upstream model, so two aliases on one provider are
//     not interchangeable (plan 018 §3.6). A provider-executed call is the
//     upstream's own tool, and another provider has no such tool: the
//     OpenAI-compatible provider would send it as a function call of ours
//     with no result after it, and ignores a result inside an assistant
//     message. The calls a tool message answers are never dropped, so an
//     assistant message with them keeps them, text or no text.
//   - An assistant message with no parts left is dropped: replayed, it would
//     be an assistant message with empty content, which some providers
//     answer with a 400. A tool message goes only with its assistant message.
//   - The result never ends with an entry's user message, nor with an
//     assistant message whose calls are still to be answered (leaf is that
//     message). A trailing user message has no answer on this path (its
//     answer was dropped above, or leaf is not an answer), and the caller is
//     about to append its own. The summary message is not an entry's and is
//     never removed: a history can end in it — a compaction with no tail and
//     nothing after it — and a request may then send no prompt of its own.
//
// The parts themselves are shared with the transcript, as Fantasy's part
// values are; a caller must not mutate their ProviderOptions maps.
func (t *Transcript) ContextAt(leaf string, current Model) ([]fantasy.Message, error) {
	msgs, _, err := t.contextAt(leaf, current)
	return msgs, err
}

// ContextWithResults is Context, and beside it, message for message, whether
// each is text neither a person nor the model wrote, which a caller replaying
// the history must redact as it redacts a tool result: an entry of background
// sub-agents' results (MessageEntry's SubagentResults), which a child wrote
// (plan 026 §3.11), or a compaction's summary message, which the summarizer
// wrote from the whole conversation, tool results included (plan 028 P30).
// The messages are exactly Context's.
func (t *Transcript) ContextWithResults(current Model) ([]fantasy.Message, []bool) {
	msgs, results, _ := t.contextAt(t.Leaf(), current) // the leaf is always known
	return msgs, results
}

// contextAt is ContextAt, with each message's mark beside it
// (ContextWithResults).
func (t *Transcript) contextAt(leaf string, current Model) ([]fantasy.Message, []bool, error) {
	c, entries, err := t.contextEntries(leaf)
	if err != nil {
		return nil, nil, err
	}
	var msgs []fantasy.Message
	var marks []bool
	if c != nil && t.render.Summary != nil {
		msgs, marks = []fantasy.Message{t.render.Summary(c.Compaction)}, []bool{true}
	}
	head := len(msgs) // the summary message, which the trim below never removes
	rest, restMarks := t.messages(entries, current)
	msgs, marks = trimUnanswered(append(msgs, rest...), append(marks, restMarks...), head)
	return msgs, marks, nil
}

// trimUnanswered is msgs, with marks beside them, less the trailing messages
// no answer follows on this path (ContextAt's last rule): an entry's user
// message, or an assistant message whose calls are still to be answered. The
// first head messages are never removed.
func trimUnanswered(msgs []fantasy.Message, marks []bool, head int) ([]fantasy.Message, []bool) {
	for len(msgs) > head {
		last := msgs[len(msgs)-1]
		unanswered := last.Role == fantasy.MessageRoleUser ||
			(last.Role == fantasy.MessageRoleAssistant && len(openCalls(last)) > 0)
		if !unanswered {
			break
		}
		msgs, marks = msgs[:len(msgs)-1], marks[:len(marks)-1]
	}
	return msgs, marks
}

// Frontier is where a context's size is counted from (plan 028 §3.7, P16):
// the last assistant entry of the context whose request's usage the
// provider reported, and what the context sends after it. Its usage covers
// everything that request sent and the answer it got, so the size of the
// context is that usage plus an estimate of After — the entry's own tool
// message, and the steers, results and reminders after it — and nothing
// before it is counted twice: the tool message a step's usage does not
// cover is After's first message, once.
type Frontier struct {
	// Entry is the assistant entry, a copy (Entry.clone): its Usage, and the
	// model that reported it.
	Entry Entry
	// After is what a request to current sends for the entries after Entry,
	// by ContextAt's rules, with Marks beside it (ContextWithResults).
	After []fantasy.Message
	Marks []bool
}

// FrontierAt is the frontier of the context at leaf (Frontier): the last
// assistant entry on the path that carries usage and was written after the
// latest successful compaction on it — never one of that compaction's tail,
// whose usage counted the context the compaction replaced. ok is false when
// there is none: no assistant entry since the latest compaction carries
// usage (an interrupted answer never does), or the path is empty. A failed
// compaction replaced nothing, so an entry before it still counts. Leaf ""
// is none; an unknown leaf is ErrUnknownEntry.
func (t *Transcript) FrontierAt(leaf string, current Model) (f Frontier, ok bool, err error) {
	path, err := t.walk(leaf) // leaf to root
	if err != nil {
		return Frontier{}, false, err
	}
	for k, i := range path {
		e := &t.Entries[i]
		if e.Type == TypeCompaction && e.Compaction.Succeeded() {
			return Frontier{}, false, nil
		}
		if e.Type != TypeMessage || e.Message.Role != fantasy.MessageRoleAssistant || e.Usage == nil {
			continue
		}
		after := make([]*Entry, 0, k)
		for j := k - 1; j >= 0; j-- {
			after = append(after, &t.Entries[path[j]])
		}
		msgs, marks := t.messages(after, current)
		msgs, marks = trimUnanswered(msgs, marks, 0)
		return Frontier{Entry: e.clone(), After: msgs, Marks: marks}, true, nil
	}
	return Frontier{}, false, nil
}

// contextEntries is what a request from leaf is built from (ContextAt): the
// latest successful compaction on the path (nil for none), and the entries
// after it, led by its tail — or, with none, every entry on the path — root
// to leaf. The compaction itself is not among them; an earlier one inside
// the tail, and a failed one anywhere, are, and are no messages.
func (t *Transcript) contextEntries(leaf string) (c *Entry, entries []*Entry, err error) {
	path, err := t.walk(leaf)
	if err != nil {
		return nil, nil, err
	}
	from := len(path) - 1 // the context's first entry, as a position in path: the root
	for k, i := range path {
		e := &t.Entries[i]
		if e.Type != TypeCompaction || !e.Compaction.Succeeded() {
			continue
		}
		c, from = e, k-1
		if id := e.Compaction.FirstKeptID; id != "" {
			// checkTail has made id an ancestor of e, so it is further up
			// the path.
			for j := k + 1; j < len(path); j++ {
				if t.Entries[path[j]].ID == id {
					from = j
					break
				}
			}
		}
		break
	}
	for k := from; k >= 0; k-- {
		if e := &t.Entries[path[k]]; e != c {
			entries = append(entries, e)
		}
	}
	return c, entries, nil
}

// messages is what the context sends for entries, in order, each message
// with its mark (ContextWithResults): the rules of ContextAt's second and
// third items, and the reminders rendered in place. entries are a context's,
// or one step of it (Steps).
func (t *Transcript) messages(entries []*Entry, current Model) ([]fantasy.Message, []bool) {
	var msgs []fantasy.Message
	var marks []bool
	dropped := false // the message before this one was an assistant message replayed as nothing
	for _, e := range entries {
		if e.Type == TypeReminder {
			// Pairing keeps a reminder from ever sitting between an assistant
			// message and its tool message, so it cannot separate the two.
			if m, ok := t.renderReminder(e.Variant); ok {
				msgs = append(msgs, m)
				marks = append(marks, false)
			}
			dropped = false
			continue
		}
		if e.Type != TypeMessage {
			continue
		}
		// replayed never drops an assistant message that has calls, so with
		// the invariant this never fires; it is here so that a tool message
		// cannot outlive its assistant message whatever replayed filters.
		if e.Message.Role == fantasy.MessageRoleTool && dropped {
			dropped = false
			continue
		}
		m, keep := replayed(e, current)
		dropped = !keep
		if keep {
			msgs = append(msgs, m)
			marks = append(marks, e.SubagentResults)
		}
	}
	return msgs, marks
}

// Step is one step of a context (Steps): the entries one step's append wrote
// — its changes and resume entry, user entries, reminders, steers and results
// entries, the assistant entry, and its tool entry when it called tools — as
// the path holds them, and what the context sends for them.
type Step struct {
	// First is the id of the step's first entry: a compaction's FirstKeptID
	// when its tail starts at this step.
	First string
	// Entries are the step's entries, in order, as copies (Entry.clone).
	Entries []Entry
	// Messages are what the context sends for Entries, by ContextAt's rules
	// for models, empty assistant messages and reminders. The context's
	// trailing trim does not apply: it only ever cuts what no step holds.
	Messages []fantasy.Message
}

// Steps is the context at leaf grouped into steps (plan 028 §3.9): ContextAt's
// entries — past the latest successful compaction, its tail and what came
// after it — in order, a step ending at a tool entry or at an assistant entry
// with no calls to answer, and the next one starting at the entry after it.
// Compaction entries are in no step: each is written alone, between two. Any
// entries after the last step's end are in none either; the store never
// leaves one there (AppendStep writes whole steps, and Open cuts what a crash
// left of one). Leaf "" is none; an unknown leaf is ErrUnknownEntry.
func (t *Transcript) Steps(leaf string, current Model) ([]Step, error) {
	_, entries, err := t.contextEntries(leaf)
	if err != nil {
		return nil, err
	}
	var steps []Step
	var run []*Entry
	for _, e := range entries {
		if e.Type == TypeCompaction {
			continue
		}
		if run = append(run, e); !stepEnd(e) {
			continue
		}
		st := Step{First: run[0].ID, Entries: make([]Entry, len(run))}
		for i, r := range run {
			st.Entries[i] = r.clone()
		}
		st.Messages, _ = t.messages(run, current)
		steps = append(steps, st)
		run = nil
	}
	return steps, nil
}

// Cut is where a compaction cuts steps, a context's (Steps), to keep a tail
// of them verbatim (plan 028 §3.9, owner decision 1, P13): the newest steps,
// whole, whose sizes together stay within budget, in tokens. It walks the
// steps newest first, adding each one's size — size is the estimate for one
// message, summed over the step's Messages — while the sum stays within
// budget; the first step that would take it over ends the walk and is not
// kept, and neither is any step before it. It returns k: steps[k:] is the
// tail, and steps[k].First the compaction's FirstKeptID; k == len(steps) is
// no tail — a budget of 0 or less, or a newest step alone over the budget. A
// budget of 0 keeps nothing even when the newest steps weigh nothing (P13's
// tail_tokens = 0): a step of another model's provider-executed parts alone
// sends this model no message, but its entries would still be the tail.
func Cut(steps []Step, budget int64, size func(fantasy.Message) int64) int {
	if budget <= 0 {
		return len(steps)
	}
	k, sum := len(steps), int64(0)
	for ; k > 0; k-- {
		n := int64(0)
		for _, m := range steps[k-1].Messages {
			n += size(m)
		}
		if sum+n > budget {
			break
		}
		sum += n
	}
	return k
}

// stepEnd reports whether e ends a step: a tool entry, or an assistant entry
// with no calls for a tool entry to answer.
func stepEnd(e *Entry) bool {
	if e == nil || e.Type != TypeMessage {
		return false
	}
	switch e.Message.Role {
	case fantasy.MessageRoleTool:
		return true
	case fantasy.MessageRoleAssistant:
		return !hasOpenCalls(e)
	}
	return false
}

// startsStep reports whether e is the first entry of a step (Steps): not a
// compaction entry, and either the root or right after the end of a step,
// with nothing between but compaction entries.
func (t *Transcript) startsStep(e *Entry) bool {
	if e.Type == TypeCompaction {
		return false
	}
	p := t.entry(e.ParentID)
	for p != nil && p.Type == TypeCompaction {
		p = t.entry(p.ParentID)
	}
	return p == nil || stepEnd(p)
}

// checkTail checks where e, when it is a compaction with a tail, starts it
// (plan 028 P14): its FirstKeptID must name an ancestor — parent, the entry e
// joins the tree under, or one of parent's — that starts a step, so that the
// tail is whole steps and a context never starts between a call and its
// results. It is errInvalid, like a field that breaks its rule: Load refuses
// a whole line that breaks it wherever it is, and the store never writes one
// (AppendCompaction).
func (t *Transcript) checkTail(e, parent *Entry) error {
	if e.Type != TypeCompaction || e.Compaction.FirstKeptID == "" {
		return nil
	}
	id := e.Compaction.FirstKeptID
	for p := parent; p != nil; p = t.entry(p.ParentID) {
		if p.ID != id {
			continue
		}
		if !t.startsStep(p) {
			return invalid("compaction %q keeps a tail from entry %q, which does not start a step", e.ID, id)
		}
		return nil
	}
	return invalid("compaction %q keeps a tail from entry %q, which is not before it on its path", e.ID, id)
}

// renderReminder is the message a reminder entry of variant stands for, from
// the harness's renderer; nothing when there is none, or it does not know
// the variant.
func (t *Transcript) renderReminder(variant string) (fantasy.Message, bool) {
	if t.render.Reminder == nil {
		return fantasy.Message{}, false
	}
	return t.render.Reminder(variant)
}

// replayed is e's message as a request to current sends it: a copy with its
// own parts slice, reasoning and provider-executed parts removed when
// another model produced it. keep is false for an assistant message left
// with nothing to send, which one with calls a tool message answers never
// is.
func replayed(e *Entry, current Model) (msg fantasy.Message, keep bool) {
	src := e.Message
	otherModel := src.Role == fantasy.MessageRoleAssistant &&
		(e.Model.Provider != current.Provider || e.Model.WireModel != current.WireModel)
	parts := make([]fantasy.MessagePart, 0, len(src.Content))
	for _, p := range src.Content {
		if otherModel && modelSpecific(p) {
			continue
		}
		parts = append(parts, p)
	}
	if src.Role == fantasy.MessageRoleAssistant && len(parts) == 0 {
		return fantasy.Message{}, false
	}
	return fantasy.Message{Role: src.Role, Content: parts, ProviderOptions: src.ProviderOptions}, true
}

// modelSpecific reports whether p means something only to the model that
// produced it: reasoning, or a call the provider executed, or its result.
func modelSpecific(p fantasy.MessagePart) bool {
	switch p.GetType() {
	case fantasy.ContentTypeReasoning:
		return true
	case fantasy.ContentTypeToolCall:
		c, _ := fantasy.AsMessagePart[fantasy.ToolCallPart](p)
		return c.ProviderExecuted
	case fantasy.ContentTypeToolResult:
		r, _ := fantasy.AsMessagePart[fantasy.ToolResultPart](p)
		return r.ProviderExecuted
	}
	return false
}
