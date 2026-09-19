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
	// one. A torn append can only damage the tail, so damage anywhere else
	// means the file was edited or corrupted, and guessing past it could
	// silently drop half a conversation.
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
// Then every entry after the last assistant message is dropped. The store
// writes a turn as one write ending in its assistant entry, but a crash can
// persist any prefix of that write, including one that ends on a line
// boundary: a user entry, or a model or effort change, with no answer after
// it. Those entries are an incomplete turn, not history, so a file holding
// only a header, or a header and a user entry, loads with no entries. (H2
// revisits this rule when a turn can end in a tool result.)
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
		t.add(e)
	}
	t.dropIncompleteTurn()
	return t, nil
}

// dropIncompleteTurn removes every entry after the last assistant message
// (see Load).
func (t *Transcript) dropIncompleteTurn() {
	keep := 0
	for i, e := range t.Entries {
		if e.Type == TypeMessage && e.Message.Role == fantasy.MessageRoleAssistant {
			keep = i + 1
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
// changes an entry (the file is never rewritten), applying three rules:
//
//   - Reasoning is replayed only to the model that produced it: an assistant
//     message's reasoning parts are dropped when its entry's provider or wire
//     model differs from current's. A signed reasoning block is specific to
//     the upstream model, so two aliases on one provider are not
//     interchangeable (plan 018 §3.6).
//   - An assistant message with no parts left is dropped: replayed, it would
//     be an assistant message with empty content, which some providers
//     answer with a 400.
//   - The result never ends with a user message. A trailing user message has
//     no answer on this path (its answer was dropped above, or leaf is not
//     an answer), and the caller is about to append its own.
//
// The parts themselves are shared with the transcript, as Fantasy's part
// values are; a caller must not mutate their ProviderOptions maps.
func (t *Transcript) ContextAt(leaf string, current Model) ([]fantasy.Message, error) {
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
			break
		}
		i = t.index[parent] // check guaranteed it exists and comes earlier
	}
	var msgs []fantasy.Message
	for k := len(path) - 1; k >= 0; k-- {
		e := &t.Entries[path[k]]
		if e.Type != TypeMessage {
			continue
		}
		if m, keep := replayed(e, current); keep {
			msgs = append(msgs, m)
		}
	}
	for len(msgs) > 0 && msgs[len(msgs)-1].Role == fantasy.MessageRoleUser {
		msgs = msgs[:len(msgs)-1]
	}
	return msgs, nil
}

// replayed is e's message as a request to current sends it: a copy with its
// own parts slice, reasoning removed when another model produced it. keep is
// false for an assistant message left with nothing to send.
func replayed(e *Entry, current Model) (msg fantasy.Message, keep bool) {
	src := e.Message
	dropReasoning := src.Role == fantasy.MessageRoleAssistant &&
		(e.Model.Provider != current.Provider || e.Model.WireModel != current.WireModel)
	parts := make([]fantasy.MessagePart, 0, len(src.Content))
	for _, p := range src.Content {
		if dropReasoning && p.GetType() == fantasy.ContentTypeReasoning {
			continue
		}
		parts = append(parts, p)
	}
	if src.Role == fantasy.MessageRoleAssistant && len(parts) == 0 {
		return fantasy.Message{}, false
	}
	return fantasy.Message{Role: src.Role, Content: parts, ProviderOptions: src.ProviderOptions}, true
}
