package store

import (
	"bytes"
	"errors"
	"fmt"
	"os"

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
// the line before it when it was written.
//
// Then the incomplete step at the tail, if any, is rolled back
// (dropIncompleteTurn). The store writes each step as one append — held
// changes, held user entries, steers, the assistant message, and the tool
// message when there is one — but a crash can persist any prefix of it,
// including one that ends on a line boundary. After the malformed last line
// is skipped, what can remain of a cut step is exactly its first lines, so
// the tail is the entries after the last complete step: changes, user
// entries and steers with no answer after them, possibly followed by an
// assistant message whose tool calls have no line after it at all, because
// the tool message was the line the cut removed. That missing line, as the
// end of the file, is the only unpaired state rolled back rather than
// refused. None of the tail is history, so a file holding only a header, or
// a header and a user entry, loads with no entries.
func Load(path string) (*Transcript, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
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
	t := newTranscript(h)
	var pair pairing
	for i, line := range lines[1:] {
		e, err := decodeEntry(line)
		if err == nil {
			err = t.check(e)
		}
		if err != nil {
			if i == len(lines)-2 {
				break // the last line: a torn tail
			}
			return nil, fmt.Errorf("%w: %s: line %d: %v", ErrCorrupt, path, i+2, err)
		}
		if err := pair.next(e, t.entry(e.ParentID)); err != nil {
			return nil, fmt.Errorf("%w: %s: line %d: %w", ErrCorrupt, path, i+2, err)
		}
		t.add(e)
	}
	t.dropIncompleteTurn()
	return t, nil
}

// dropIncompleteTurn removes every entry after the last complete step: an
// assistant message with no calls left to answer, or a tool message, which
// Load has checked answers the assistant message just before it. It runs
// only once Load has refused every line that breaks the invariant, so the
// one unpaired entry it can meet is an assistant message with open calls as
// the last line — its tool message cut off — and that goes, together with
// the steers, user entries and changes written ahead of it (see Load).
func (t *Transcript) dropIncompleteTurn() {
	keep := 0
	for i := range t.Entries {
		e := &t.Entries[i]
		if e.Type != TypeMessage {
			continue
		}
		switch e.Message.Role {
		case fantasy.MessageRoleTool:
			keep = i + 1
		case fantasy.MessageRoleAssistant:
			if !hasOpenCalls(e) {
				keep = i + 1
			}
		}
	}
	for _, e := range t.Entries[keep:] {
		delete(t.index, e.ID)
	}
	t.Entries = t.Entries[:keep]
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

// Context is ContextAt from the leaf; see there.
func (t *Transcript) Context(current Model) []fantasy.Message {
	msgs, _ := t.ContextAt(t.Leaf(), current) // the leaf is always known
	return msgs
}

// ContextAt is the history a request from leaf sends: the message entries on
// the path from the root to leaf, in order. It builds new messages and never
// changes an entry (the file is never rewritten), applying these rules, each
// of which keeps tool calls paired with their results:
//
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
//   - The result never ends with a user message, nor with an assistant
//     message whose calls are still to be answered (leaf is that message).
//     A trailing user message has no answer on this path (its answer was
//     dropped above, or leaf is not an answer), and the caller is about to
//     append its own.
//
// The parts themselves are shared with the transcript, as Fantasy's part
// values are; a caller must not mutate their ProviderOptions maps.
func (t *Transcript) ContextAt(leaf string, current Model) ([]fantasy.Message, error) {
	msgs, _, err := t.contextAt(leaf, current)
	return msgs, err
}

// ContextWithResults is Context, and beside it, message for message, whether
// each is an entry of background sub-agents' results (MessageEntry's
// SubagentResults): what a caller replaying the history needs to redact such
// an entry as it redacts a tool result, since a child wrote it (plan 026
// §3.11). The messages are exactly Context's.
func (t *Transcript) ContextWithResults(current Model) ([]fantasy.Message, []bool) {
	msgs, results, _ := t.contextAt(t.Leaf(), current) // the leaf is always known
	return msgs, results
}

// contextAt is ContextAt, with each message's SubagentResults mark beside it.
func (t *Transcript) contextAt(leaf string, current Model) ([]fantasy.Message, []bool, error) {
	if leaf == "" {
		return nil, nil, nil
	}
	i, ok := t.index[leaf]
	if !ok {
		return nil, nil, fmt.Errorf("%w %q", ErrUnknownEntry, leaf)
	}
	var path []int
	for {
		path = append(path, i)
		parent := t.Entries[i].ParentID
		if parent == "" {
			break
		}
		i = t.index[parent] // check guaranteed it exists and comes earlier
	}
	var msgs []fantasy.Message
	var results []bool
	dropped := false // the message before this one was an assistant message replayed as nothing
	for k := len(path) - 1; k >= 0; k-- {
		e := &t.Entries[path[k]]
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
			results = append(results, e.SubagentResults)
		}
	}
	for len(msgs) > 0 {
		last := msgs[len(msgs)-1]
		unanswered := last.Role == fantasy.MessageRoleUser ||
			(last.Role == fantasy.MessageRoleAssistant && len(openCalls(last)) > 0)
		if !unanswered {
			break
		}
		msgs, results = msgs[:len(msgs)-1], results[:len(results)-1]
	}
	return msgs, results, nil
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
