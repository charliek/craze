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
// A transcript only ever grows by whole turns. The file and its directory
// are created lazily, by the first turn that produced output. The header,
// that turn's user entry and its assistant entry go out in one write(2) to a
// temporary file beside the session's, which is then hard-linked into place:
// the session path either does not exist or holds the complete first turn,
// so a turn cancelled before any output leaves nothing and a crashed process
// leaves no header with a dangling user message. After that, each turn's
// user entry is held in memory (AppendUser) and written in the same write as
// its assistant entry (AppendAssistant), on an O_APPEND descriptor.
//
// A model_change or effort_change is held the same way and goes out in the
// next turn's write, ahead of its user entry. So nothing is ever written
// ahead of a turn that produced no output: switching model and then
// cancelling before any output leaves no lone model_change in the file, and
// the change is written with the next turn that does produce output (or
// never, if the session closes first; the next turn's own message entries
// record the model either way). A later change of the same kind replaces an
// unwritten one.
//
// One write(2) is not atomic across a crash: what reaches the disk can be any
// prefix of it, possibly ending on a line boundary. There is no fsync (a
// transcript is not a database); Load instead drops a malformed last line
// and then everything after the last assistant message, which is exactly an
// incomplete turn. After a power loss the first turn's file can also be
// empty or zero-filled, which Load reports as ErrNoHeader.
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

	// ErrNoOutput is AppendAssistant's refusal of a message with no
	// non-blank text: an answer cut short while the model was still only
	// reasoning is not persisted, because replayed it would be an assistant
	// message with empty content, which some providers answer with a 400
	// (plan 018 §3.6). Nothing changes; the caller treats it as "nothing to
	// persist", not as a turn failure. H2 extends "output" to tool calls.
	ErrNoOutput = errors.New("store: assistant message has no text")

	// ErrNoUser is AppendAssistant's error before the file exists when no
	// user entry is waiting: the first write must carry the turn's user
	// entry, or the transcript would start with an answer to nothing.
	ErrNoUser = errors.New("store: no user entry to write with the first assistant entry")
)

// Options are a session's fixed facts.
type Options struct {
	Home         string // the harness's directory, <craze dir>/native; absolute
	Workspace    string // the session's working directory; absolute
	CrazeVersion string // recorded in the header
	SystemPrompt string // its SHA-256 goes in the header; the text is never stored
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
	user    *Entry  // the current turn's user entry, until its answer is written
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

// AppendUser holds the turn's user entry until AppendAssistant writes it
// with the answer. A second call before that replaces it: the earlier turn
// produced no output, and its prompt is not persisted.
func (s *Store) AppendUser(e MessageEntry) error {
	if e.Message.Role != fantasy.MessageRoleUser {
		return fmt.Errorf("store: AppendUser needs a user message, got role %q", e.Message.Role)
	}
	if err := e.Model.validate("a message entry"); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usable(); err != nil {
		return err
	}
	s.user = &Entry{Type: TypeMessage, Timestamp: s.stamp(), MessageEntry: e}
	return nil
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

// AppendAssistant writes the turn: any held changes, the held user entry,
// and e, in one write — the first one also carrying the header and creating
// the file. With no user entry held (a later step of a turn already
// written), it writes the changes and e. A message with no non-blank text is
// ErrNoOutput and changes nothing.
//
// Every entry is encoded and decoded back before anything is written, so an
// entry that Load could not read (say, provider metadata of a type Fantasy
// has no decoder registered for) fails here, with nothing written, and what
// Context replays is exactly what a later Load would.
func (s *Store) AppendAssistant(e MessageEntry) error {
	if e.Message.Role != fantasy.MessageRoleAssistant {
		return fmt.Errorf("store: AppendAssistant needs an assistant message, got role %q", e.Message.Role)
	}
	if err := e.Model.validate("a message entry"); err != nil {
		return err
	}
	if !hasText(e.Message) {
		return ErrNoOutput
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usable(); err != nil {
		return err
	}
	if s.f == nil && s.user == nil {
		return ErrNoUser
	}

	batch := append([]Entry(nil), s.changes...)
	if s.user != nil {
		batch = append(batch, *s.user)
	}
	batch = append(batch, Entry{Type: TypeMessage, Timestamp: s.stamp(), MessageEntry: e})

	var buf bytes.Buffer
	if s.f == nil {
		line, err := encodeHeader(s.t.Header)
		if err != nil {
			return fmt.Errorf("store: header: %w", err)
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	parent := s.t.Leaf()
	taken := map[string]bool{}
	for i := range batch {
		id, err := s.newEntryID(taken)
		if err != nil {
			return err
		}
		batch[i].ID, batch[i].ParentID = id, parent
		parent = id
		line, err := encodeEntry(batch[i])
		if err != nil {
			return fmt.Errorf("store: encode %s entry: %w", batch[i].Type, err)
		}
		back, err := decodeEntry(line)
		if err != nil {
			return fmt.Errorf("store: %s entry would not read back: %w", batch[i].Type, err)
		}
		batch[i] = back
		buf.Write(line)
		buf.WriteByte('\n')
	}

	if s.f == nil {
		if err := s.create(buf.Bytes()); err != nil {
			return err
		}
	} else if n, err := s.f.Write(buf.Bytes()); err != nil {
		if n == 0 {
			// Nothing reached the file (the disk was already full, say),
			// so it still ends on a whole turn: fail this turn only.
			return fmt.Errorf("store: append to %s: %w", s.path, err)
		}
		s.err = fmt.Errorf("%w: append to %s: %v", ErrFailed, s.path, err)
		return s.err
	}
	for _, b := range batch {
		s.t.add(b)
	}
	s.user, s.changes = nil, nil
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
	s.user, s.changes = nil, nil
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
