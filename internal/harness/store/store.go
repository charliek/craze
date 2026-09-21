// Package store is the native harness's session store: one JSONL file per
// session, a header line and then entries linked by id and parentId into a
// tree, from which a request's history is rebuilt by walking leaf to root
// (pi's format, D-03, as plan 018 §3.6 narrows it for H1). A message entry's
// payload is Fantasy's official message JSON inside craze's envelope, so
// every part's provider metadata survives the round trip.
//
// The store does not know providers: it records the provider id and wire
// model a message went to, as strings, and compares them. It never computes
// a home directory either; the harness hands it one.
//
// # Writes
//
// A transcript only ever grows by whole steps. A turn is one or more steps;
// a step is the model's assistant message and, when it called tools, the
// tool message holding their results, led by any steers (user messages
// interjected mid-turn) that the model first saw at that step. The file and
// its directory are created lazily, by the first step that produced output.
// The header, the turn's user entry and that step go out in one write(2) to
// a temporary file beside the session's, which is then hard-linked into
// place: the session path either does not exist or holds the complete first
// step, so a turn cancelled before any output leaves nothing and a crashed
// process leaves no header with a dangling user message. After that, a
// turn's user entry is held in memory (AppendUser) and written in the same
// write as the step that answers it (AppendStep), on an O_APPEND descriptor.
// User entries held for a step not yet written accumulate, in order, and
// all go out ahead of it; DiscardHeldUsers drops them instead.
//
// A model_change or effort_change is held the same way and goes out in the
// next step's write, ahead of its user entries. So nothing is ever written
// ahead of a turn that produced no output: switching model and then
// cancelling before any output leaves no lone model_change in the file, and
// the change is written with the next turn that does produce output (or
// never, if the session closes first; the next turn's own message entries
// record the model either way). A later change of the same kind replaces an
// unwritten one.
//
// A step's tool calls and results keep the pairing invariant (see pairing),
// which AppendStep checks before writing and Load checks on every line.
//
// One write(2) is not atomic across a crash: what reaches the disk can be any
// prefix of it, possibly ending on a line boundary. There is no fsync (a
// transcript is not a database); Load instead drops a malformed last line
// and then everything after the last complete step, which is exactly what a
// cut step leaves: a step's append is recovered as a transaction at load, not
// written as one. After a power loss the first turn's file can also be empty
// or zero-filled, which Load reports as ErrNoHeader.
//
// A later append that fails after writing some of its bytes leaves the store
// failed (ErrFailed) rather than appending after the torn line, which would
// turn the tear into mid-file corruption. One that fails before writing
// anything leaves the file as it was, so it fails only that turn. Files are
// 0600 and directories 0700: transcripts hold the user's prompts.
//
// A Store is safe for concurrent use; the harness drives it from one turn at
// a time, with Close possibly arriving from another goroutine.
package store

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"uuid"

	"charm.land/fantasy"
)

var (
	// ErrClosed is every Append's error after Close. The store never
	// reopens its file.
	ErrClosed = errors.New("store: closed")

	// ErrFailed wraps the cause of an earlier append that wrote part of its
	// bytes and then failed. The file may end in a torn line, which Load
	// tolerates only as the last line, so the store appends nothing after
	// it.
	ErrFailed = errors.New("store: an earlier write failed")

	// ErrNoOutput is AppendStep's refusal of an assistant message with no
	// output — no non-blank text and no tool call — and AppendAssistant's of
	// one with no non-blank text: an answer cut short while the model was
	// still only reasoning is not persisted, because replayed it would be an
	// assistant message with empty content, which some providers answer with
	// a 400 (plan 018 §3.6). Nothing changes; the caller treats it as
	// "nothing to persist", not as a turn failure.
	ErrNoOutput = errors.New("store: assistant message has no output")

	// ErrNoUser is AppendStep's error before the file exists when no user
	// entry is held: the first write must carry the turn's user entry, or
	// the transcript would start with an answer to nothing.
	ErrNoUser = errors.New("store: no user entry to write with the first step")
)

// Options are a session's fixed facts.
type Options struct {
	Home         string // the harness's directory, <craze dir>/native; absolute
	Workspace    string // the session's working directory; absolute
	CrazeVersion string // recorded in the header
	SystemPrompt string // its SHA-256 goes in the header; the text is never stored
	// ToolProfile names the session's tool contract and Tools is the tools
	// array its requests send, serialized; the header records the name and
	// the SHA-256 of Tools, never Tools itself. Both are empty for a session
	// with no tools, and the header then has neither field.
	ToolProfile string
	Tools       []byte
	// Now stamps the header and every entry; nil means time.Now.
	Now func() time.Time

	// Test seams, settable only inside the package; zero means production.
	sessionID string
	entryID   func() string
	openFile  func(name string, flag int, perm os.FileMode) (file, error)
}

// file is what the store needs of its descriptor: the seam a test counts
// writes through.
type file interface {
	Write(p []byte) (int, error)
	Close() error
}

func openOSFile(name string, flag int, perm os.FileMode) (file, error) {
	return os.OpenFile(name, flag, perm)
}

// maxIDAttempts bounds entry-id regeneration. With 32 random bits a
// collision is already rare; a second in a row means the source is broken.
const maxIDAttempts = 8

// Store is one session's transcript, being written.
type Store struct {
	mu       sync.Mutex
	path     string
	t        *Transcript // the header and every entry written so far, as read back
	now      func() time.Time
	entryID  func() string
	openFile func(name string, flag int, perm os.FileMode) (file, error)

	f       file    // nil until the first write, and after Close
	users   []Entry // held user entries, in order, until the step answering them is written
	changes []Entry // unwritten model and effort changes, in order
	closed  bool
	err     error // non-nil once a write to an existing file failed
}

// New starts a session's store. It does no I/O: the directory and the file
// appear with the first turn that produces output, so a session closed
// without one leaves nothing behind.
func New(opts Options) (*Store, error) {
	if !filepath.IsAbs(opts.Home) {
		return nil, fmt.Errorf("store: home %q is not an absolute path", opts.Home)
	}
	if !filepath.IsAbs(opts.Workspace) {
		return nil, fmt.Errorf("store: workspace %q is not an absolute path", opts.Workspace)
	}
	s := &Store{now: opts.Now, entryID: opts.entryID, openFile: opts.openFile}
	if s.now == nil {
		s.now = time.Now
	}
	if s.entryID == nil {
		s.entryID = randomEntryID
	}
	if s.openFile == nil {
		s.openFile = openOSFile
	}
	id := opts.sessionID
	if id == "" {
		id = uuid.NewV4().String()
	}
	cwd := filepath.Clean(opts.Workspace)
	sum := sha256.Sum256([]byte(opts.SystemPrompt))
	h := Header{
		Version:            FormatVersion,
		ID:                 id,
		Timestamp:          s.stamp(),
		Cwd:                cwd,
		CrazeVersion:       opts.CrazeVersion,
		SystemPromptSHA256: hex.EncodeToString(sum[:]),
		ToolProfile:        opts.ToolProfile,
	}
	if len(opts.Tools) > 0 {
		sum := sha256.Sum256(opts.Tools)
		h.ToolsSHA256 = hex.EncodeToString(sum[:])
	}
	s.t = newTranscript(h)
	s.path = sessionPath(filepath.Clean(opts.Home), cwd, id, h.Timestamp)
	return s, nil
}

// randomEntryID is 8 hex chars from 32 random bits.
func randomEntryID() string {
	var b [4]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never returns an error
	return hex.EncodeToString(b[:])
}

// stamp is the time an entry records, at the file's millisecond precision,
// so an entry in memory equals the same entry read back.
func (s *Store) stamp() time.Time {
	return s.now().UTC().Truncate(time.Millisecond)
}

// ID, Path and Header read what New fixed, so they take no lock.

// ID is the session id: a UUID, in the header and the file name.
func (s *Store) ID() string { return s.t.Header.ID }

// Path is where the transcript is, or will be once a turn is written.
func (s *Store) Path() string { return s.path }

// Header is the session's header line, written or not.
func (s *Store) Header() Header { return s.t.Header }

// usable is nil when an append may proceed. The caller holds mu.
func (s *Store) usable() error {
	if s.closed {
		return ErrClosed
	}
	return s.err
}

// checkMessage refuses e unless it has role and names its model in full.
func checkMessage(call string, e MessageEntry, role fantasy.MessageRole) error {
	if e.Message.Role != role {
		return fmt.Errorf("store: %s needs a %s message, got role %q", call, role, e.Message.Role)
	}
	return e.Model.validate("a message entry")
}

// AppendUser holds a user entry until AppendStep writes it, ahead of the
// step that answers it. A second call before that holds a second entry
// after the first; both are written, in order. A caller that wants an
// earlier turn's unanswered prompt left out of the transcript — because no
// request it sent since had that prompt in its history — calls
// DiscardHeldUsers first.
func (s *Store) AppendUser(e MessageEntry) error {
	if err := checkMessage("AppendUser", e, fantasy.MessageRoleUser); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usable(); err != nil {
		return err
	}
	s.users = append(s.users, Entry{Type: TypeMessage, Timestamp: s.stamp(), MessageEntry: e})
	return nil
}

// DiscardHeldUsers drops the user entries held for a step not yet written:
// they belong to a turn that produced no output. Held changes stay held.
func (s *Store) DiscardHeldUsers() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.users = nil
}

// AppendModelChange records a switch to m, written ahead of the next turn
// that produces output (see the package comment). It replaces an unwritten
// model change.
func (s *Store) AppendModelChange(m Model) error {
	if err := m.validate("a model change"); err != nil {
		return err
	}
	return s.holdChange(Entry{Type: TypeModelChange, MessageEntry: MessageEntry{Model: m}})
}

// AppendEffortChange records a switch to effort ("" for none), written
// ahead of the next turn that produces output. It replaces an unwritten
// effort change.
func (s *Store) AppendEffortChange(effort string) error {
	return s.holdChange(Entry{Type: TypeEffortChange, MessageEntry: MessageEntry{Effort: effort}})
}

// AppendModeChange records a switch to mode (agent, plan, ask), written ahead
// of the next turn that produces output, and replacing an unwritten mode
// change — the same holding rule as the other two. The caller hands one over
// at the step boundary where the model is told about the mode, so the entry
// lands where the change became visible in the conversation rather than at
// the turn's start (plan 023 §3.1).
func (s *Store) AppendModeChange(mode string) error {
	if mode == "" {
		return errors.New("store: a mode change needs a mode")
	}
	return s.holdChange(Entry{Type: TypeModeChange, Mode: mode})
}

func (s *Store) holdChange(e Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usable(); err != nil {
		return err
	}
	e.Timestamp = s.stamp()
	kept := s.changes[:0]
	for _, c := range s.changes {
		if c.Type != e.Type {
			kept = append(kept, c)
		}
	}
	s.changes = append(kept, e)
	return nil
}

// AppendStep writes a finished step in one append: any held changes, the
// held user entries, steers (user messages interjected mid-turn that the
// model first saw at this step), the assistant message, and tool — the tool
// message holding the results of the assistant message's tool calls — when
// the model called tools. The first append also carries the header and
// creates the file. It returns the ids of the entries it wrote, in file
// order: the assistant message's is the last, or the one before the tool
// message's.
//
// The assistant message must have output, non-blank text or a tool call,
// or AppendStep returns ErrNoOutput and changes nothing. The step must keep
// the pairing invariant (see pairing): tool must be there exactly when the
// assistant message has calls to answer, and answer them, in order.
// Breaking it is a bug in the caller, refused with ErrUnpaired, with nothing
// written.
//
// The append is one write, recovered as a transaction at load, not atomic:
// a crash can persist any prefix of it, which Load rolls back to the last
// complete step (see the package comment).
func (s *Store) AppendStep(steers []MessageEntry, assistant MessageEntry, tool *MessageEntry) ([]string, error) {
	for _, st := range steers {
		if err := checkMessage("a steer", st, fantasy.MessageRoleUser); err != nil {
			return nil, err
		}
	}
	if err := checkMessage("AppendStep", assistant, fantasy.MessageRoleAssistant); err != nil {
		return nil, err
	}
	if tool != nil {
		if err := checkMessage("AppendStep's tool entry", *tool, fantasy.MessageRoleTool); err != nil {
			return nil, err
		}
	}
	if !hasOutput(assistant.Message) {
		return nil, ErrNoOutput
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usable(); err != nil {
		return nil, err
	}
	if s.f == nil && len(s.users) == 0 {
		return nil, ErrNoUser
	}

	batch := append(append([]Entry(nil), s.changes...), s.users...)
	for _, st := range steers {
		batch = append(batch, Entry{Type: TypeMessage, Timestamp: s.stamp(), MessageEntry: st})
	}
	batch = append(batch, Entry{Type: TypeMessage, Timestamp: s.stamp(), MessageEntry: assistant})
	if tool != nil {
		batch = append(batch, Entry{Type: TypeMessage, Timestamp: s.stamp(), MessageEntry: *tool})
	}
	if err := s.write(batch); err != nil {
		return nil, err
	}
	s.users, s.changes = nil, nil
	ids := make([]string, len(batch))
	for i, b := range batch {
		ids[i] = b.ID
	}
	return ids, nil
}

// AppendAssistant writes a text-only answer as a step with no steers and no
// tool message: what the runner saves of a step a cancel or a failure cut
// short. A message with no non-blank text is ErrNoOutput and changes
// nothing.
func (s *Store) AppendAssistant(e MessageEntry) error {
	if err := checkMessage("AppendAssistant", e, fantasy.MessageRoleAssistant); err != nil {
		return err
	}
	if !hasText(e.Message) {
		return ErrNoOutput
	}
	_, err := s.AppendStep(nil, e, nil)
	return err
}

// write gives batch its ids and parents, as a chain from the leaf, and
// appends it in one write, the header first when the file does not exist
// yet; then batch holds the entries as they read back, and so does the
// transcript. The caller holds mu, and on success clears what it held.
//
// Every entry is encoded and decoded back before anything is written, so an
// entry that Load could not read (say, provider metadata of a type Fantasy
// has no decoder registered for) fails here, with nothing written, and the
// transcript keeps the decoded entry, so what Context replays is exactly
// what a later Load would. Nothing compares the decoded entry with the one
// handed in: they differ in harmless ways (an error result reads back as
// errors.New of its text; a number inside provider options held as
// map[string]any reads back as a float64), and refusing those would refuse
// entries H1 saves. The pairing invariant, and the result checks that go
// with it, run on the entries as they read back, from the transcript's last
// entry on.
func (s *Store) write(batch []Entry) error {
	var buf bytes.Buffer
	if s.f == nil {
		line, err := encodeHeader(s.t.Header)
		if err != nil {
			return fmt.Errorf("store: header: %w", err)
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	parent := s.t.last()
	pair := pairingAfter(parent)
	taken := map[string]bool{}
	for i := range batch {
		id, err := s.newEntryID(taken)
		if err != nil {
			return err
		}
		batch[i].ID, batch[i].ParentID = id, ""
		if parent != nil {
			batch[i].ParentID = parent.ID
		}
		line, err := encodeEntry(batch[i])
		if err != nil {
			return fmt.Errorf("store: encode %s entry: %w", batch[i].Type, err)
		}
		back, err := decodeEntry(line)
		if err != nil {
			return fmt.Errorf("store: %s entry would not read back: %w", batch[i].Type, err)
		}
		if err := pair.next(back, parent); err != nil {
			return err
		}
		batch[i] = back
		parent = &batch[i]
		buf.Write(line)
		buf.WriteByte('\n')
	}
	if pair.open != nil {
		return unpaired("assistant entry %q has tool calls and no tool message after it", pair.open.ID)
	}

	if s.f == nil {
		if err := s.create(buf.Bytes()); err != nil {
			return err
		}
	} else if n, err := s.f.Write(buf.Bytes()); err != nil {
		if n == 0 {
			// Nothing reached the file (the disk was already full, say),
			// so it still ends on a whole step: fail this step only.
			return fmt.Errorf("store: append to %s: %w", s.path, err)
		}
		s.err = fmt.Errorf("%w: append to %s: %v", ErrFailed, s.path, err)
		return s.err
	}
	for _, b := range batch {
		s.t.add(b)
	}
	return nil
}

// create makes the directory and the file and writes the first turn, header
// included, so that the session path either does not exist or holds the
// whole turn. The turn goes in one Write (on a regular file, one write(2))
// to a temporary file in the same directory, opened 0600 and O_APPEND, which
// is then hard-linked to the session path; the descriptor stays open for
// later appends and the temporary name is removed.
//
// A hard link, unlike a rename, fails rather than replace a file already at
// the path, so a session can never clobber another's transcript (the path
// holds a fresh UUID, so that is a corruption guard, not an expected case).
// Linux and macOS home filesystems all support hard links; one that does
// not fails every first write with the link's error.
//
// Any failure removes the temporary file (best effort; each attempt uses a
// fresh name, so a leftover cannot block the next) and returns an error with
// the session path untouched, so the next turn tries again from the start.
// The caller holds mu.
func (s *Store) create(first []byte) error {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("store: %w", err)
	}
	tmp := filepath.Join(dir, "."+filepath.Base(s.path)+"."+randomEntryID()+".tmp")
	f, err := s.openFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}
	if _, err := f.Write(first); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("store: first write to %s: %w", s.path, err)
	}
	if err := os.Link(tmp, s.path); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("store: %w", err)
	}
	// The transcript now has its real name; the temporary one is only a
	// second link to the same file, and a leftover is harmless.
	_ = os.Remove(tmp)
	s.f = f
	return nil
}

// newEntryID draws an id that neither the file nor this batch has. The
// caller holds mu.
func (s *Store) newEntryID(taken map[string]bool) (string, error) {
	for range maxIDAttempts {
		id := s.entryID()
		if id != "" && !s.t.has(id) && !taken[id] {
			taken[id] = true
			return id, nil
		}
	}
	return "", fmt.Errorf("store: no unused entry id in %d attempts", maxIDAttempts)
}

// hasText reports whether m has a text part with something besides
// whitespace in it.
func hasText(m fantasy.Message) bool {
	for _, p := range m.Content {
		if t, ok := fantasy.AsMessagePart[fantasy.TextPart](p); ok && strings.TrimSpace(t.Text) != "" {
			return true
		}
	}
	return false
}

// hasOutput reports whether m has text or a tool call: a step whose only
// content is tool calls is output, and replays as a message with calls.
func hasOutput(m fantasy.Message) bool {
	for _, p := range m.Content {
		if p != nil && p.GetType() == fantasy.ContentTypeToolCall {
			return true
		}
	}
	return hasText(m)
}

// Context is the history the next request sends to current: the written
// messages from the root to the last entry, with Transcript.ContextAt's
// rules applied. Held entries are not in it. It works after Close.
func (s *Store) Context(current Model) []fantasy.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.t.Context(current)
}

// Close releases the descriptor. Held entries are discarded: they belong to
// a turn that produced no output. Close is idempotent, and every Append
// after it is ErrClosed.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	s.users, s.changes = nil, nil
	if s.f == nil {
		return nil
	}
	err := s.f.Close()
	s.f = nil
	if err != nil {
		return fmt.Errorf("store: close %s: %w", s.path, err)
	}
	return nil
}
