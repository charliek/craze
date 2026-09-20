// Package journal is craze's session journal: one append-only JSONL file per
// host incarnation (one process lifetime of one session), holding the
// session's sequenced events in the lossless codec and journal-only notes
// (the provider session id, prompts as typed and how they ended, and
// diagnostics). It serves two readers: replay, for a subscriber whose cursor
// has left the in-memory ring, and a person or a script mining a finished
// session for errors (plan 020 §3.4; discovery/session-control/04).
//
// The package imports nothing from craze. It is handed record envelopes
// (Record, whose body internal/agent's codec produced) and notes, so the
// native harness's depguard rule and the agent package's import graph are
// both unaffected by it.
//
// # Files
//
// A journal lives at <dir>/<cwd slug>/<UTC yyyymmddThhmmssZ>_<incarnation>.jsonl,
// the harness store's slug and stamp. Nothing ever opens an existing journal
// to append: a torn last line is tolerated on read and never becomes interior
// corruption, because the next incarnation starts a new file. The file is
// created with O_CREATE|O_EXCL, so a path that already exists fails the
// journal rather than being overwritten. Files are 0600 and new directories
// 0700, both "no broader than": the umask may narrow them. A directory that
// already exists is used as it is and never tightened, so a user who made
// the journal directory group-readable on purpose keeps that choice.
//
// The file is created lazily, with the first line worth writing, and a
// closing diag on its own is not worth writing: a session built and closed
// without doing anything (a picker's discarded session) leaves nothing on
// disk, not even a directory.
//
// # The writer
//
// Append and Note never block and never do I/O: they stamp the entry and
// push it onto one bounded queue under a mutex that is never held across
// I/O. One goroutine, started by the first entry accepted, owns the file:
// it encodes entries into whole lines, writes them in batches, and only then
// publishes the complete-record boundary (Flushed) a live reader stops at.
//
// When the queue is full the entry is not queued but counted into a gap
// marker at the queue's tail, so what is lost is recorded in order: events
// 1–10 queued, 11 dropped and 12 queued are written as 1–10, a gap line for
// 11, then 12. A write, sync or create error fails the writer for good:
// everything accepted and not written is accounted in Health as an
// open-ended gap, and nothing more is written. Either way the user is told
// once, on Options.Diag, by the writer goroutine; the session is never
// affected.
//
// Durability is stated, not implied: lines reach the OS every 250 ms while
// any are buffered, whenever 1 MiB or 512 lines are, and on a flush or sync
// request; a sync (flush and fsync) follows every prompt_end note, and
// Close. A crash can lose the last unflushed window, which the session's
// in-memory ring covered while the process lived.
package journal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Defaults for Options' limits (plan 020 §3.4).
const (
	DefaultQueueEntries   = 4096
	DefaultQueueBytes     = 16 << 20
	DefaultMaxRecordBytes = 8 << 20
)

// The writer's schedule, fixed in production and overridable only by this
// package's tests (Options' unexported fields).
const (
	defaultFlushInterval = 250 * time.Millisecond
	defaultFlushBytes    = 1 << 20
	defaultFlushLines    = 512
	defaultCloseWait     = 500 * time.Millisecond
)

// minRecordBytes is the smallest MaxRecordBytes New accepts: room for any
// note's replacement line when even its large field emptied does not fit.
const minRecordBytes = 4 << 10

// envelopeBytes is the room MaxRecordBytes must leave under MaxLineBytes for
// a record's envelope (ts, seq, at, eventType and the keys), so a body at
// the cap still makes a line the reader accepts.
const envelopeBytes = 64 << 10

// entryOverhead is what an entry costs the queue's byte budget beyond its
// payload: the entry itself and the line's keys.
const entryOverhead = 256

// maxRetainedBuffer bounds the line buffer the writer keeps between
// batches: one large record grows it, and it is let go afterwards rather
// than held for the session's life.
const maxRetainedBuffer = 4 << 20

// cutSlack is what a truncated note's line gains besides its shorter field:
// ,"truncated":true and a margin.
const cutSlack = 32

// maxHeaderField caps each of the header's strings at New: a version, a
// provider, a binary, a workspace and a mode. Each is short in practice; the
// cap is what keeps a pathological one, escaped, from making a header line
// the reader refuses, which would make the whole file unreadable.
const maxHeaderField = 4 << 10

// maxCuts bounds re-encoding a note that is over the cap. One pass is
// enough when the large field can give the bytes; the bound is only a
// guarantee.
const maxCuts = 4

var (
	// ErrNoJournal is what a nil *Writer's ReadRange and WaitFlushed return,
	// so a session without a journal needs no branch to ask.
	ErrNoJournal = errors.New("journal: no journal")
	// ErrFailed wraps the error that failed the writer: nothing after it
	// will be written.
	ErrFailed = errors.New("journal: the writer failed")
	// ErrClosed is WaitFlushed's error when the writer has finished (or was
	// closed before it ever started) without reaching the seq asked for.
	ErrClosed = errors.New("journal: closed")
	// ErrStalled is Close's error when its bound ran out while the writer
	// was still busy, typically inside a Write or Sync that has not
	// returned. The writer keeps the file and finishes on its own.
	ErrStalled = errors.New("journal: close stopped waiting for a stalled writer")
)

// Options are a journal's fixed facts. Everything the header records is
// taken here, at New, so a session's later changes (a new mode, a resolved
// binary) go in later lines, never in a rewritten header.
type Options struct {
	// Dir is the journal directory, paths.JournalDir(); absolute.
	Dir string
	// Incarnation names the file and is recorded in the header: the
	// session's event log's id (a UUIDv7). Letters, digits, '.', '_' and
	// '-' only, since it is part of a file name.
	Incarnation string
	// Cwd is the session's workspace. New makes it absolute; its slug is
	// the journal's subdirectory.
	Cwd string

	// The rest of the header: craze's version, the provider's name, the
	// agent binary requested (the one the spawn resolved goes in the
	// session note), and the options that shape the session's behavior.
	CrazeVersion string
	Provider     string
	AgentBinary  string
	Mode         string
	Force        bool
	Interactive  bool
	// EventCodec is the codec version of every event body,
	// agent.EventCodecVersion. 0 means 1, the only version so far.
	EventCodec int

	// Diag receives the one notice the journal ever prints, the first time
	// it degrades. nil discards it.
	Diag io.Writer
	// Now stamps every line; nil means time.Now.
	Now func() time.Time

	// Limits; zero means the default. QueueEntries counts records and notes
	// accepted and not yet written, one slot of it reserved for a gap
	// marker; QueueBytes weighs them. MaxRecordBytes is the largest body
	// Append journals (a larger one becomes an oversized omitted record)
	// and the most any note's line may take.
	QueueEntries   int
	QueueBytes     int
	MaxRecordBytes int

	// Test seams, settable only inside the package; zero means production.
	openFile      func(name string, flag int, perm os.FileMode) (file, error)
	newTimer      func(time.Duration) flushTimer
	flushInterval time.Duration
	flushBytes    int
	flushLines    int
	closeWait     time.Duration
	// maxLineBytes lowers the longest event line the writer lets through
	// (MaxLineBytes), so a test can reach the check behind the caps.
	maxLineBytes int
}

// file is what the writer needs of its descriptor: the seam a test stalls,
// fails or counts.
type file interface {
	Write(p []byte) (int, error)
	Sync() error
	Close() error
}

func openOSFile(name string, flag int, perm os.FileMode) (file, error) {
	return os.OpenFile(name, flag, perm)
}

// flushTimer is the writer's 250 ms timer, a seam so a test fires it by hand
// instead of sleeping.
type flushTimer interface {
	C() <-chan time.Time
	Stop()
}

type realTimer struct{ t *time.Timer }

func newRealTimer(d time.Duration) flushTimer { return realTimer{time.NewTimer(d)} }
func (r realTimer) C() <-chan time.Time       { return r.t.C }
func (r realTimer) Stop()                     { r.t.Stop() }

// entryKind is what a queued entry or a buffered line is.
type entryKind uint8

const (
	kindEvent entryKind = iota + 1
	kindNote
	kindGap
	kindHeader // a line only: the header is never queued
)

// entry is one accepted Append or Note, or a gap marker.
type entry struct {
	kind entryKind
	ts   time.Time // when it was accepted: the line's ts
	size int       // its weight in the byte budget; 0 for a marker
	rec  Record
	note Note
	gap  *gapMarker
}

// gapMarker accumulates what was dropped while it was the queue's tail.
// Append and Note mutate it only under mu and only while it is the last
// pending entry; once the writer has taken it, nothing does.
type gapMarker struct {
	hasRange      bool // an event was dropped into it: from and to are set
	from, to      uint64
	events, notes int
	closings      int // how many of notes are closing diags
}

// onlyClosing reports whether every entry dropped into the marker was a
// closing diag. Such a gap is as little worth a file as the closing note it
// stands for.
func (m *gapMarker) onlyClosing() bool {
	return m.events == 0 && m.notes == m.closings
}

// lineMeta is one line in the writer's buffer: where it ends, and what its
// being written settles.
type lineMeta struct {
	end     int       // offset just past the line's newline in the buffer
	seq     uint64    // the highest seq it accounts for; 0 for notes and the header
	kind    entryKind // what the line is
	size    int       // queue bytes it releases
	counted bool      // a record or note: releases one queue entry
	omitted bool      // an event written as an omitted record
}

// boundary is the complete-record boundary: bytes of whole lines written to
// the OS, and the highest seq those lines account for, events and gap lines
// alike. The two change together, under mu.
type boundary struct {
	seq   uint64
	bytes int64
}

// Writer is one incarnation's journal. Append, Note, the requests, Health,
// Flushed, ReadRange and WaitFlushed are safe from any goroutine; every
// method is safe on a nil *Writer, which is a session without a journal. A
// Writer that has accepted an entry owns one goroutine until Close (and,
// past Close's bound, until a stalled Write or Sync returns); one that never
// has owns none.
type Writer struct {
	// Fixed at New.
	path          string
	header        []byte // the header line, newline included
	now           func() time.Time
	diag          io.Writer
	maxEntries    int
	maxBytes      int
	maxRecord     int
	flushInterval time.Duration
	flushBytes    int
	flushLines    int
	closeWait     time.Duration
	maxLine       int // the longest event line written: MaxLineBytes but in tests
	openFile      func(name string, flag int, perm os.FileMode) (file, error)
	newTimer      func(time.Duration) flushTimer

	wake     chan struct{} // cap 1: there is something for the writer to look at
	exited   chan struct{} // closed when the writer goroutine has returned
	syncReq  atomic.Bool   // a sync was asked for: RequestSync, prompt_end, Close
	flushReq atomic.Bool   // a flush was asked for: RequestFlush, WaitFlushed

	// mu guards everything below. It is never held across I/O or a call
	// out of the package.
	mu          sync.Mutex
	pending     []entry // accepted, not yet taken by the writer
	spare       []entry // the writer's last batch, emptied, for reuse
	queued      int     // records and notes accepted and not yet written, taken or not
	queuedBytes int
	markers     int  // gap markers not yet written
	started     bool // the writer goroutine has been started
	closed      bool // Close was called: admission has stopped
	failed      bool
	finished    bool // the writer goroutine has returned
	flushed     boundary
	advanced    chan struct{} // closed and replaced when flushed, failed or finished changes
	health      Health        // State is derived on read

	// Owned by the writer goroutine; nothing else touches them.
	f            file
	out          bytes.Buffer // lines not yet written
	enc          *json.Encoder
	lines        []lineMeta
	written      int64 // the file offset the buffer starts at
	headerQueued bool  // the header is in the buffer or the file: the file exists or is about to
	unsynced     bool  // written since the last fsync
	gapEncoded   bool  // a gap line has been encoded: the notice is owed
	noticed      bool
}

// New prepares a journal. It does no I/O: the directory and the file appear
// with the first line worth writing, so a session that never produces one
// leaves nothing behind.
func New(opts Options) (*Writer, error) {
	if opts.Dir == "" || !filepath.IsAbs(opts.Dir) {
		return nil, fmt.Errorf("journal: directory %q is not an absolute path", opts.Dir)
	}
	if !validIncarnation(opts.Incarnation) {
		return nil, fmt.Errorf("journal: incarnation %q cannot be part of a file name", opts.Incarnation)
	}
	cwd, err := filepath.Abs(opts.Cwd)
	if err != nil {
		return nil, fmt.Errorf("journal: workspace: %w", err)
	}
	w := &Writer{
		now:           opts.Now,
		diag:          opts.Diag,
		maxEntries:    orDefault(opts.QueueEntries, DefaultQueueEntries),
		maxBytes:      orDefault(opts.QueueBytes, DefaultQueueBytes),
		maxRecord:     orDefault(opts.MaxRecordBytes, DefaultMaxRecordBytes),
		flushInterval: orDefault(opts.flushInterval, defaultFlushInterval),
		flushBytes:    orDefault(opts.flushBytes, defaultFlushBytes),
		flushLines:    orDefault(opts.flushLines, defaultFlushLines),
		closeWait:     orDefault(opts.closeWait, defaultCloseWait),
		maxLine:       min(orDefault(opts.maxLineBytes, MaxLineBytes), MaxLineBytes),
		openFile:      opts.openFile,
		newTimer:      opts.newTimer,
		wake:          make(chan struct{}, 1),
		exited:        make(chan struct{}),
		advanced:      make(chan struct{}),
	}
	if w.maxEntries < 2 {
		return nil, fmt.Errorf("journal: QueueEntries %d leaves no room besides the gap marker's slot", w.maxEntries)
	}
	if w.maxRecord < minRecordBytes || w.maxRecord > MaxLineBytes-envelopeBytes {
		return nil, fmt.Errorf("journal: MaxRecordBytes %d is outside [%d, %d]", w.maxRecord, minRecordBytes, MaxLineBytes-envelopeBytes)
	}
	if w.now == nil {
		w.now = time.Now
	}
	if w.diag == nil {
		w.diag = io.Discard
	}
	if w.openFile == nil {
		w.openFile = openOSFile
	}
	if w.newTimer == nil {
		w.newTimer = newRealTimer
	}
	w.enc = json.NewEncoder(&w.out)
	// Prompts and tool output are read with jq and grep, where a plain "<"
	// reads better than its six-byte escape, and a body's bytes stay exactly
	// the codec's.
	w.enc.SetEscapeHTML(false)

	started := w.now()
	codec := opts.EventCodec
	if codec == 0 {
		codec = 1
	}
	// The header's strings are capped (the file's path keeps the whole
	// workspace, through its slug): the reader must be able to read line 1
	// whatever the options held, or it can read nothing.
	field := func(s string) string { return cutUTF8(s, maxHeaderField) }
	if err := w.enc.Encode(headerLine{
		TS: stamp(started), Type: typeHeader, Format: FormatVersion, EventCodec: codec,
		Incarnation: opts.Incarnation, CrazeVersion: field(opts.CrazeVersion),
		OS: runtime.GOOS, Arch: runtime.GOARCH,
		Provider: field(opts.Provider), AgentBinary: field(opts.AgentBinary), Cwd: field(cwd),
		Force: opts.Force, Interactive: opts.Interactive, Mode: field(opts.Mode), PID: os.Getpid(),
	}); err != nil {
		return nil, fmt.Errorf("journal: header: %w", err)
	}
	w.header = bytes.Clone(w.out.Bytes())
	w.out.Reset()
	w.path = journalPath(filepath.Clean(opts.Dir), cwd, opts.Incarnation, started)
	return w, nil
}

func orDefault[T int | time.Duration](v, def T) T {
	if v <= 0 {
		return def
	}
	return v
}

// validIncarnation keeps the incarnation a plain file-name component: no
// separator, no "." or "..", nothing a shell or a filesystem treats
// specially.
func validIncarnation(s string) bool {
	if s == "" || len(s) > 128 || s[0] == '.' {
		return false
	}
	for _, r := range s {
		ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.'
		if !ok {
			return false
		}
	}
	return true
}

// Path is where the journal is, or will be once a line is written. "" for a
// nil *Writer.
func (w *Writer) Path() string {
	if w == nil {
		return ""
	}
	return w.path
}

// MaxRecordBytes is the largest body Append journals whole: Options'
// MaxRecordBytes, or its default. 0 for a nil *Writer. The event log lowers
// its own limit to it, so the ring and the file omit the same records.
func (w *Writer) MaxRecordBytes() int {
	if w == nil {
		return 0
	}
	return w.maxRecord
}

// Append queues one event record. It never blocks and never does I/O. It
// must be called in increasing Seq order, which the event log's publishing
// boundary guarantees; a body larger than MaxRecordBytes is journaled as an
// oversized omitted record, and an EventType longer than MaxEventTypeBytes
// (it is a type's name) makes the record an encode_error omitted record under
// OverlongEventType. An Omitted's Error is kept to its first 4 KiB and its
// Reason to 256 bytes. The event log applies both rules itself, so what it
// hands over is already what a ring replay carries; these are the defence
// against any other caller, and against the two limits differing.
func (w *Writer) Append(rec Record) {
	if w == nil {
		return
	}
	ts := w.now()
	rec = w.acceptRecord(rec)
	size := entryOverhead + len(rec.EventType) + len(rec.Body)
	if o := rec.Omitted; o != nil {
		size += len(o.Reason) + len(o.Error)
	}
	w.admit(entry{kind: kindEvent, ts: ts, rec: rec, size: size})
}

// acceptRecord normalizes a record as it is accepted: an Omitted is copied
// (the caller's pointer is not kept) and its strings capped, a body over the
// cap is replaced by an oversized marker, and a type too long to be a type
// replaces the whole record, so the queue's byte budget weighs what is
// really held and no line the record makes is too long to read back.
func (w *Writer) acceptRecord(rec Record) Record {
	if len(rec.EventType) > maxIdentifier {
		return Record{Seq: rec.Seq, At: rec.At, EventType: OverlongEventType, Omitted: &Omitted{
			Reason: OmittedEncodeError,
			Error: "journal: the event type is " + strconv.Itoa(len(rec.EventType)) +
				" bytes, over the " + strconv.Itoa(maxIdentifier) + "-byte cap",
		}}
	}
	switch {
	case rec.Omitted != nil:
		o := *rec.Omitted
		if len(o.Reason) > maxIdentifier {
			o.Reason = strings.Clone(cutUTF8(o.Reason, maxIdentifier))
		}
		if len(o.Error) > maxOmittedError {
			o.Error = strings.Clone(cutUTF8(o.Error, maxOmittedError))
		}
		rec.Omitted, rec.Body = &o, ""
	case len(rec.Body) > w.maxRecord:
		rec.Omitted, rec.Body = &Omitted{Reason: OmittedOversized, Bytes: len(rec.Body)}, ""
	}
	return rec
}

// Note queues one journal-only note. It never blocks and never does I/O. A
// DiagNote's fields are encoded here, on the caller's goroutine, so the map
// is the caller's again when Note returns; the encoding calls no method of
// any value in it and its work is bounded by the note cap (DiagNote says
// which values are kept). A note whose large field is over
// MaxRecordBytes is cut here, so the queue holds no more than will be
// written. A prompt_end asks for a sync even when the queue has no room for
// the note itself: a turn's end is when durability matters.
func (w *Writer) Note(n Note) {
	if w == nil {
		return
	}
	ts := w.now()
	if n = w.freeze(n); n == nil {
		return
	}
	if excess := n.size() - w.maxRecord; excess > 0 {
		// By raw bytes, so what the queue holds is bounded; the writer
		// cuts again by encoded size if escaping still makes it too long.
		if smaller, ok := n.cut(func(s string) string { return strings.Clone(cutUTF8(s, len(s)-excess)) }); ok {
			n = smaller
		}
	}
	w.admit(entry{kind: kindNote, ts: ts, note: n, size: entryOverhead + n.size()})
	if isPromptEnd(n) {
		w.RequestSync()
	}
}

// freeze turns a note into the value the queue holds: a pointer form is
// copied (nil is nothing to note), and a DiagNote's fields are encoded. What
// comes back owns nothing the caller can still change.
func (w *Writer) freeze(n Note) Note {
	switch v := n.(type) {
	case DiagNote:
		return encodeDiag(v, w.maxRecord)
	case *DiagNote:
		if v == nil {
			return nil
		}
		return encodeDiag(*v, w.maxRecord)
	case *SessionNote:
		if v == nil {
			return nil
		}
		return *v
	case *PromptNote:
		if v == nil {
			return nil
		}
		return *v
	case *PromptEndNote:
		if v == nil {
			return nil
		}
		return *v
	}
	return n
}

func isPromptEnd(n Note) bool {
	_, ok := n.(PromptEndNote)
	return ok
}

func isClosing(n Note) bool {
	d, ok := n.(encodedDiag)
	return ok && d.kind == DiagClosing
}

// admit queues e, or counts it into the tail gap marker when the queue is
// full, or counts it as lost once the writer has failed or been closed. It
// starts the writer goroutine with the first entry.
func (w *Writer) admit(e entry) {
	w.mu.Lock()
	if w.closed || w.failed {
		w.countLostLocked(e.kind)
		w.mu.Unlock()
		return
	}
	idle := len(w.pending) == 0
	// One slot is reserved: a record or note may take the queue to
	// maxEntries-1, so a gap marker always has somewhere to go.
	if w.queued < w.maxEntries-1 && w.queuedBytes+e.size <= w.maxBytes {
		w.pending = append(w.pending, e)
		w.queued++
		w.queuedBytes += e.size
	} else {
		w.dropLocked(e)
	}
	start := !w.started
	w.started = true
	w.mu.Unlock()
	if start {
		go w.run()
	}
	// A non-empty queue has already woken the writer, which takes the
	// whole queue once it runs.
	if idle {
		w.signal()
	}
}

// dropLocked counts e into the gap marker at the queue's tail, creating one
// if the tail is not a marker, and records the dropped range in Health at
// once, so a subscriber deciding what the file can serve sees it before the
// gap line is written. The caller holds mu.
func (w *Writer) dropLocked(e entry) {
	var m *gapMarker
	if n := len(w.pending); n > 0 && w.pending[n-1].kind == kindGap {
		m = w.pending[n-1].gap
	} else {
		m = &gapMarker{}
		w.pending = append(w.pending, entry{kind: kindGap, ts: e.ts, gap: m})
		w.markers++
	}
	if e.kind == kindNote {
		m.notes++
		if isClosing(e.note) {
			m.closings++
		}
		w.health.DroppedNotes++
		return
	}
	seq := e.rec.Seq
	m.events++
	w.health.DroppedEvents++
	if !m.hasRange {
		m.hasRange, m.from, m.to = true, seq, seq
		w.health.addGap(SeqRange{From: seq, To: seq})
		return
	}
	m.from, m.to = min(m.from, seq), max(m.to, seq)
	w.health.extendLastGap(seq)
}

// countLostLocked counts an entry that will never be written. The caller
// holds mu.
func (w *Writer) countLostLocked(k entryKind) {
	switch k {
	case kindEvent:
		w.health.DroppedEvents++
	case kindNote:
		w.health.DroppedNotes++
	}
}

// signal wakes the writer without blocking; a wake-up already pending
// covers this one.
func (w *Writer) signal() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// RequestFlush asks the writer to hand every buffered line to the OS now,
// moving the complete-record boundary. It never blocks.
func (w *Writer) RequestFlush() {
	if w == nil {
		return
	}
	w.flushReq.Store(true)
	w.signal()
}

// RequestSync asks the writer to flush and fsync now. It never blocks, and
// it is a flag independent of the queue: a full queue cannot lose it.
func (w *Writer) RequestSync() {
	if w == nil {
		return
	}
	w.syncReq.Store(true)
	w.signal()
}

// Flushed is the complete-record boundary: the highest seq the written lines
// account for (events and gap lines alike) and the bytes of whole lines
// written. A live reader never reads past bytes.
func (w *Writer) Flushed() (seq uint64, bytes int64) {
	if w == nil {
		return 0, 0
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.flushed.seq, w.flushed.bytes
}

// Health is a copy of the journal's state. A nil *Writer reports StateOff.
func (w *Writer) Health() Health {
	if w == nil {
		return Health{State: StateOff}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	h := w.health
	h.Gaps = append([]SeqRange(nil), h.Gaps...)
	switch {
	case w.failed:
		h.State = StateFailed
	case w.markers > 0:
		h.State = StateGap
	default:
		h.State = StateOK
	}
	return h
}

// WaitFlushed requests a flush and waits, without polling, until the file
// accounts for seq, the writer fails (ErrFailed), the writer finishes
// without reaching it (ErrClosed), or ctx ends (its error). A seq dropped
// into a gap counts as reached once the gap line is written; Health.Gaps
// says it is not in the file.
func (w *Writer) WaitFlushed(ctx context.Context, seq uint64) error {
	if w == nil {
		return ErrNoJournal
	}
	w.RequestFlush()
	for {
		w.mu.Lock()
		reached := w.flushed.seq >= seq
		failed, cause := w.failed, w.health.Err
		over := w.finished || (w.closed && !w.started)
		advanced := w.advanced
		w.mu.Unlock()
		switch {
		case reached:
			return nil
		case failed:
			return fmt.Errorf("%w: %s", ErrFailed, cause)
		case over:
			return ErrClosed
		}
		select {
		case <-advanced:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// broadcastLocked wakes every WaitFlushed. The caller holds mu.
func (w *Writer) broadcastLocked() {
	close(w.advanced)
	w.advanced = make(chan struct{})
}

// Close stops admission and asks the writer to write what is queued, sync,
// close the file and exit, waiting at most 500 ms and at most until ctx
// ends. That bounds the caller only: a writer stuck inside a Write or Sync
// keeps the file and finishes, or fails, on its own when the call returns,
// and Close never closes the descriptor itself (a regular file offers no
// lever to release a stalled write). ErrStalled says the bound ran out; a
// failed writer's error wraps ErrFailed. Close is idempotent, and every
// Append and Note after it is counted as lost.
func (w *Writer) Close(ctx context.Context) error {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	w.closed = true
	started := w.started
	if !started {
		w.broadcastLocked()
	}
	w.mu.Unlock()
	if !started {
		return nil
	}
	w.syncReq.Store(true)
	w.signal()
	select {
	case <-w.exited:
	default:
		bound := time.NewTimer(w.closeWait)
		defer bound.Stop()
		select {
		case <-w.exited:
		case <-bound.C:
			return ErrStalled
		case <-ctx.Done():
			return fmt.Errorf("%w: %w", ErrStalled, ctx.Err())
		}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.failed {
		return fmt.Errorf("%w: %s", ErrFailed, w.health.Err)
	}
	return nil
}

// run is the writer goroutine. It takes the whole queue, encodes it into
// lines (writing at the size thresholds), then honors a sync or flush
// request, and sleeps until woken or until the flush timer fires while
// lines are buffered. Closing is one more batch, a sync, and the exit. Any
// error has already failed the writer when it returns here.
//
// The requests are read before the queue is taken, so a request covers
// everything accepted before it was made: Note queues a prompt_end and only
// then asks for the sync, and WaitFlushed asks for its flush after the seq
// it waits for was appended. A request made after the read wakes the writer
// for another round.
func (w *Writer) run() {
	var timer flushTimer
	var timerC <-chan time.Time
	defer func() {
		if timer != nil {
			timer.Stop()
		}
		w.finish()
	}()
	for {
		flush, sync := w.flushReq.Swap(false), w.syncReq.Swap(false)
		batch, closing := w.take()
		if w.writeBatch(batch) != nil {
			return
		}
		w.recycle(batch)
		var err error
		switch {
		case sync || closing:
			err = w.sync()
		case flush:
			err = w.flush()
		}
		if err != nil || closing {
			return
		}
		w.notice()
		if w.out.Len() > 0 && timer == nil {
			timer = w.newTimer(w.flushInterval)
			timerC = timer.C()
		}
		select {
		case <-w.wake:
		case <-timerC:
			timer, timerC = nil, nil
			if w.flush() != nil {
				return
			}
		}
	}
}

// take hands the writer everything queued, and whether Close has been
// called: a batch taken after Close is the last one, since nothing more is
// admitted.
func (w *Writer) take() ([]entry, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	batch := w.pending
	w.pending, w.spare = w.spare, nil
	return batch, w.closed
}

// recycle returns an emptied batch's storage for the next one, so a steady
// session does not allocate a queue per batch.
func (w *Writer) recycle(batch []entry) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.spare == nil {
		w.spare = batch[:0]
	}
}

// writeBatch encodes a batch into the buffer, writing whenever the buffer
// reaches a size threshold. On a failed write the rest of the batch is
// counted as lost.
func (w *Writer) writeBatch(batch []entry) error {
	for i := range batch {
		w.encode(&batch[i])
		batch[i] = entry{} // let go of the payload; the line holds it now
		if w.out.Len() >= w.flushBytes || len(w.lines) >= w.flushLines {
			if err := w.flush(); err != nil {
				w.lose(batch[i+1:])
				return err
			}
		}
	}
	return nil
}

// encode appends e's line to the buffer, the header first if the file has
// no line yet. A closing diag that would be the file's first line is
// discarded instead, and so is a gap line standing only for dropped closing
// diags: a session that did nothing leaves no file, however small its queue.
func (w *Writer) encode(e *entry) {
	if !w.headerQueued {
		switch {
		case e.kind == kindNote && isClosing(e.note):
			w.release(e.size)
			return
		case e.kind == kindGap && e.gap.onlyClosing():
			w.discardGap(e.gap)
			return
		}
		w.out.Write(w.header)
		w.lines = append(w.lines, lineMeta{end: w.out.Len(), kind: kindHeader})
		w.headerQueued = true
	}
	switch e.kind {
	case kindEvent:
		w.encodeEvent(e)
	case kindNote:
		w.encodeNote(e)
	case kindGap:
		w.encodeGap(e)
	}
}

// encodeEvent writes an event line with the body embedded as it is. A body
// that is not one JSON object is journaled as an encode_error omitted
// record instead, so the line stays one line of valid JSON and the seq is
// still there. A line that would still be longer than the reader accepts is
// journaled as an oversized omitted record: Append's caps already rule that
// out, and this check is what makes it a guarantee.
func (w *Writer) encodeEvent(e *entry) {
	rec := e.rec
	l := eventLine{TS: stamp(e.ts), Type: typeEvent, Seq: rec.Seq, At: stamp(rec.At), EventType: rec.EventType}
	if rec.Omitted == nil && !looksLikeObject(rec.Body) {
		rec.Omitted = &Omitted{Reason: OmittedEncodeError, Error: "journal: the event body is not a JSON object"}
	}
	if rec.Omitted != nil {
		l.Omitted = &omittedJSON{Reason: rec.Omitted.Reason, Bytes: rec.Omitted.Bytes, Error: rec.Omitted.Error}
	} else {
		l.Event = json.RawMessage(rec.Body)
	}
	start := w.out.Len()
	if err := w.enc.Encode(l); err != nil {
		// The body is not valid JSON; the encoder wrote nothing.
		w.out.Truncate(start)
		l.Event = nil
		l.Omitted = &omittedJSON{Reason: OmittedEncodeError, Error: cutUTF8("journal: "+err.Error(), maxOmittedError)}
		_ = w.enc.Encode(l) // strings, numbers and a struct: cannot fail
	}
	if w.out.Len()-start-1 > w.maxLine {
		w.out.Truncate(start)
		l.Event = nil
		l.Omitted = &omittedJSON{Reason: OmittedOversized, Bytes: len(rec.Body)}
		_ = w.enc.Encode(l)
	}
	w.lines = append(w.lines, lineMeta{end: w.out.Len(), seq: rec.Seq, kind: kindEvent, size: e.size,
		counted: true, omitted: l.Omitted != nil})
}

// looksLikeObject reports whether body starts, after whitespace, with '{':
// the encoder validates the rest.
func looksLikeObject(body string) bool {
	for i := 0; i < len(body); i++ {
		switch body[i] {
		case ' ', '\t', '\r', '\n':
			continue
		case '{':
			return true
		}
		return false
	}
	return false
}

// encodeNote writes a note's line, cutting its large field until the line
// fits MaxRecordBytes, and replacing the note with a note_too_large diag if
// it still does not: the reader's line limit is never what discovers a large
// note.
func (w *Writer) encodeNote(e *entry) {
	n, ts, start := e.note, stamp(e.ts), w.out.Len()
	for tries := 0; ; tries++ {
		err := w.enc.Encode(n.line(ts))
		over := w.out.Len() - start - 1 - w.maxRecord
		if err == nil && over <= 0 {
			break
		}
		w.out.Truncate(start)
		smaller, ok := n.cut(func(s string) string { return cutEncoded(s, over+cutSlack) })
		if err != nil || !ok || tries == maxCuts {
			fields, _ := json.Marshal(map[string]any{"type": lineType(n), "bytes": n.size()})
			_ = w.enc.Encode(encodedDiag{kind: diagNoteTooLarge, fields: fields, truncated: true}.line(ts))
			break
		}
		n = smaller
	}
	w.lines = append(w.lines, lineMeta{end: w.out.Len(), kind: kindNote, size: e.size, counted: true})
}

// lineType names a frozen note's line type for a note_too_large diag.
func lineType(n Note) string {
	switch n.(type) {
	case SessionNote:
		return typeSession
	case PromptNote:
		return typePrompt
	case PromptEndNote:
		return typePromptEnd
	default:
		return typeDiag
	}
}

// encodeGap writes a gap marker's line. Its range, if it has one, counts
// toward the boundary's seq: the file accounts for those seqs, as missing.
func (w *Writer) encodeGap(e *entry) {
	g := e.gap
	l := gapLine{TS: stamp(e.ts), Type: typeGap, DroppedEvents: g.events, DroppedNotes: g.notes, Error: gapQueueFull}
	var seq uint64
	if g.hasRange {
		l.FromSeq, l.ToSeq, seq = g.from, g.to, g.to
	}
	_ = w.enc.Encode(l) // numbers and strings: cannot fail
	w.lines = append(w.lines, lineMeta{end: w.out.Len(), seq: seq, kind: kindGap})
	w.gapEncoded = true
}

// discardGap drops a gap marker that stands only for closing diags, when it
// would be the file's first line. It is settled as a discarded closing note
// is: the state returns to ok, and those notes are not counted as lost.
func (w *Writer) discardGap(g *gapMarker) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.markers--
	w.health.DroppedNotes -= g.closings
}

// flush writes the buffer, creating the file first if this is its first
// write, and publishes the boundary through the last whole line that
// reached the OS. A short write counts as unwritten from the first
// incomplete line, and fails the writer.
func (w *Writer) flush() error {
	if w.out.Len() == 0 {
		return nil
	}
	if w.f == nil {
		f, err := w.create()
		if err != nil {
			return w.fail(err, 0)
		}
		w.f = f
	}
	buf := w.out.Bytes()
	n, err := w.f.Write(buf)
	n = max(0, min(n, len(buf)))
	if err == nil && n < len(buf) {
		err = io.ErrShortWrite
	}
	k := 0
	for k < len(w.lines) && w.lines[k].end <= n {
		k++
	}
	if k > 0 {
		w.publish(w.lines[:k])
		w.unsynced = true
	}
	if err != nil {
		return w.fail(err, k)
	}
	w.written += int64(len(buf))
	w.out.Reset()
	w.lines = w.lines[:0]
	if w.out.Cap() > maxRetainedBuffer {
		w.out = bytes.Buffer{}
	}
	return nil
}

// create makes the journal's directory (0700, parents included; an existing
// one is left as it is) and the file, exclusively (0600).
func (w *Writer) create() (file, error) {
	if err := os.MkdirAll(filepath.Dir(w.path), 0o700); err != nil {
		return nil, err
	}
	return w.openFile(w.path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
}

// publish settles lines that reached the OS whole: their queue slots are
// released, gap lines return the state toward ok, and the boundary moves
// past them. WaitFlushed callers are woken.
func (w *Writer) publish(lines []lineMeta) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, l := range lines {
		if l.counted {
			w.queued--
			w.queuedBytes -= l.size
		}
		switch {
		case l.kind == kindGap:
			w.markers--
		case l.omitted:
			w.health.Omitted++
		}
		w.flushed.seq = max(w.flushed.seq, l.seq)
	}
	w.flushed.bytes = w.written + int64(lines[len(lines)-1].end)
	w.broadcastLocked()
}

// release gives back a queue slot for an entry that was discarded rather
// than written (a closing diag with no file to go in).
func (w *Writer) release(size int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.queued--
	w.queuedBytes -= size
}

// sync flushes, then fsyncs if anything was written since the last one.
func (w *Writer) sync() error {
	if err := w.flush(); err != nil {
		return err
	}
	if w.f == nil || !w.unsynced {
		return nil
	}
	if err := w.f.Sync(); err != nil {
		return w.fail(err, 0)
	}
	w.unsynced = false
	return nil
}

// fail moves the writer to failed for good. Buffered lines from the k-th on
// and everything still queued are counted as lost, and Health gains an
// open-ended gap from the first seq no written line accounts for. Queued
// entries are let go; later ones are counted as they arrive. It returns err.
func (w *Writer) fail(err error, k int) error {
	lost := w.lines[k:]
	w.mu.Lock()
	w.failed = true
	w.health.Err = err.Error()
	for _, l := range lost {
		if l.counted {
			w.countLostLocked(l.kind)
		}
	}
	for _, e := range w.pending {
		w.countLostLocked(e.kind)
	}
	w.pending, w.spare = nil, nil
	w.markers = 0
	w.health.addOpenGap(w.flushed.seq + 1)
	w.broadcastLocked()
	w.mu.Unlock()
	w.out.Reset()
	w.lines = w.lines[:0]
	return err
}

// lose counts taken entries that will never be encoded because the writer
// failed mid-batch.
func (w *Writer) lose(rest []entry) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, e := range rest {
		w.countLostLocked(e.kind)
	}
}

// finish runs as the writer goroutine returns: it closes the file, the one
// place that ever does, prints the notice if one is owed, and marks the
// writer finished.
func (w *Writer) finish() {
	if w.f != nil {
		err := w.f.Close()
		w.f = nil
		w.mu.Lock()
		failed := w.failed
		w.mu.Unlock()
		if err != nil && !failed {
			_ = w.fail(err, 0)
		}
	}
	w.notice()
	w.mu.Lock()
	w.finished = true
	w.broadcastLocked()
	w.mu.Unlock()
	close(w.exited)
}

// notice prints the journal's one notice the first time it finds the state
// has left ok, describing the state as it is when printed: a writer that
// dropped entries and then failed reports the failure. A drop is noticed
// once its gap line is encoded, so a gap that was discarded (only closing
// diags, and no file) is never announced as one the file has.
func (w *Writer) notice() {
	if w.noticed {
		return
	}
	w.mu.Lock()
	failed, cause := w.failed, w.health.Err
	w.mu.Unlock()
	var msg string
	switch {
	case failed:
		msg = cause + "; nothing more is journaled to " + w.path + " (the session is unaffected)"
	case w.gapEncoded:
		msg = "the journal fell behind and dropped entries; " + w.path + " has a gap (the session is unaffected)"
	default:
		return
	}
	w.noticed = true
	fmt.Fprintln(w.diag, "journal: "+msg)
}
