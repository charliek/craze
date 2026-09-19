package agent

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
	"uuid"

	"github.com/charliek/craze/internal/journal"
)

// The event log is where a session's events are numbered, kept and fanned
// out (plan 020 §3.1–3.2). Each session's emit keeps its own stamping (At,
// Replayed, the stub's noteOpen) and its own done fast-path, and then calls
// Publish where it used to send on its events channel; Events() returns
// Primary(). What Publish adds is a sequence number, a ring of recent records
// a resuming subscriber can replay from, any number of subscriptions beside
// the primary reader, and the journal's copy — all inline, on the publisher's
// own goroutine, so nothing is ever between an emit and the primary channel.
// That is what keeps "emitted means buffered" (internal/cli/prompt.go's
// drainBuffered) exactly true.

// Defaults for EventLogOptions and SubscribeOptions (plan 020 §3.1–3.2).
const (
	defaultRingEvents     = 4096
	defaultRingBytes      = 8 << 20
	defaultMaxRecordBytes = journal.DefaultMaxRecordBytes
	defaultSubscribeItems = 1024
	defaultSubscribeBytes = 8 << 20
	// primaryCap is the primary channel's buffer: today's events channel,
	// unchanged, since every consumer's pacing was tuned against it.
	primaryCap = 256
)

// maxOmittedError caps an omitted record's error message. It is well under
// the journal's own cap, so the journal keeps the message exactly as the ring
// holds it and a ring replay and a file replay carry the same record.
const maxOmittedError = 1 << 10

var (
	// ErrClosed ends a subscription that was closed, by its own Close or by
	// its log's, and is what Subscribe returns on a closed log. It is not
	// acp.ErrClosed: that one is an agent connection ending under a turn.
	ErrClosed = errors.New("agent: event log closed")
	// ErrSlowConsumer ends a subscription whose reader fell further behind
	// than its buffer allows. The log never waits for a subscriber, so one
	// that cannot keep up is dropped whole rather than handed a hole.
	ErrSlowConsumer = errors.New("agent: subscription dropped: its reader fell behind its buffer")
	// errNotContiguous ends a subscription whose owner was handed a record
	// that does not follow the one it delivered last. Nothing in the log
	// produces that; it is the owner's guard against a hole ever reaching a
	// reader silently, and seeing it is a bug.
	errNotContiguous = errors.New("agent: event log: a subscription's records are not contiguous")
)

// CursorReason is why a cursor cannot be resumed from.
type CursorReason string

// Reasons Subscribe gives for refusing a cursor. The last four belong to the
// journal leg of a resume (plan 020 §3.2), which C5c adds; until then a head
// that has left the ring is always no_journal.
const (
	// CursorForeignIncarnation: the cursor names another incarnation (an
	// earlier process, another session), whose numbers mean nothing here.
	CursorForeignIncarnation CursorReason = "foreign_incarnation"
	// CursorFutureSeq: the cursor is past the last event published.
	CursorFutureSeq CursorReason = "future_seq"
	// CursorBacklogTooLarge: the records to replay from the ring are larger
	// than the subscription's MaxBytes. A backlog is never truncated.
	CursorBacklogTooLarge CursorReason = "backlog_too_large"
	// CursorNoJournal: the head of the range has left the ring and there is
	// no journal file to serve it from.
	CursorNoJournal CursorReason = "no_journal"
	// CursorJournalGap: the journal recorded a gap inside the range.
	CursorJournalGap CursorReason = "journal_gap"
	// CursorJournalBehind: the journal file did not reach the range in time.
	CursorJournalBehind CursorReason = "journal_behind"
	// CursorEvicted: the range was neither in the file nor in the ring.
	CursorEvicted CursorReason = "evicted"
)

// ErrCursorUnresolvable is a cursor Subscribe cannot resume from, or a
// subscription whose replay could not be served. Reason says which; use
// errors.As to read it. The range is never served in part.
type ErrCursorUnresolvable struct {
	Reason CursorReason
}

func (e ErrCursorUnresolvable) Error() string {
	return "agent: event log: cannot resume from the cursor: " + string(e.Reason)
}

// Omitted is why a record has no body: an event the codec could not encode
// (a programming error), or one larger than MaxRecordBytes (a legitimate
// large event: plan text is not capped). It is the journal's own type, so
// the ring, a subscription and the journal file all carry one shape.
type Omitted = journal.Omitted

// Record is one numbered event as the log keeps it: in the ring, handed to
// subscribers, and appended to the journal. Body is the lossless codec's JSON
// object (EncodeEvent). It is a string, built once and never written again,
// so a record can be shared by the ring, every subscriber and the journal
// with nothing any of them can change under another: a subscriber is handed
// its own copy of Omitted for the same reason.
type Record struct {
	Seq  uint64
	At   time.Time
	Type EventType
	Body string
	// Omitted is non-nil when the event has no body, and Body is then "".
	// An omitted record still takes its seq, so every reader sees the
	// sequence whole and ring replay and file replay agree (plan 020 §3.3).
	Omitted *Omitted
}

// Event decodes the record's body and sets Seq, which is the envelope's and
// not the codec's. An omitted record has no body to decode, and is an error;
// its Seq, Type and At are still on the record.
func (r Record) Event() (Event, error) {
	if r.Omitted != nil {
		return Event{}, fmt.Errorf("agent: event %d (%s) was recorded without its body: %s", r.Seq, r.Type, r.Omitted.Reason)
	}
	ev, err := DecodeEvent(r.Body)
	if err != nil {
		return Event{}, err
	}
	ev.Seq = r.Seq
	return ev, nil
}

// size is a record's weight against the ring's and a subscription's byte
// budgets: its encoded size.
func (r Record) size() int {
	n := len(r.Body)
	if r.Omitted != nil {
		n += len(r.Omitted.Error)
	}
	return n
}

// detached is r with its own copy of Omitted, the one part of a record that
// is not immutable by construction, so what one subscriber does to the
// record it was handed reaches no other holder.
func (r Record) detached() Record {
	if r.Omitted != nil {
		o := *r.Omitted
		r.Omitted = &o
	}
	return r
}

// journalRecord is r as the journal takes it. The journal copies Omitted on
// acceptance, so the ring's stays the ring's.
func (r Record) journalRecord() journal.Record {
	return journal.Record{Seq: r.Seq, At: r.At, EventType: string(r.Type), Body: r.Body, Omitted: r.Omitted}
}

// Cursor is a position in one incarnation's sequence: the last seq a reader
// has, in the log that numbered it.
type Cursor struct {
	Incarnation string
	Seq         uint64
}

// SubscribeOptions shape a subscription.
type SubscribeOptions struct {
	// After is where to resume: every record after After.Seq is delivered,
	// the replay first and then live records, with nothing skipped. nil
	// means live only, from the next event published.
	After *Cursor
	// MaxItems bounds the live buffer: records published and not yet
	// handed to Records. Default 1024.
	MaxItems int
	// MaxBytes bounds the live buffer and the pinned replay backlog
	// together, on encoded size. Default 8 MiB.
	//
	// A record stops counting against both as the owner begins to send it,
	// so a record the reader has taken never counts; at worst a subscription
	// holds its budget plus the one record it is blocked sending.
	MaxBytes int
}

// EventSource is a session that can be subscribed to beside its primary
// reader. It is optional, found by a type assertion, so agent.Session itself
// does not change; every Session in this repo implements it by delegating to
// its EventLog (plan 020 §3.2).
type EventSource interface {
	Subscribe(SubscribeOptions) (*Subscription, error)
	Incarnation() string
}

var _ EventSource = (*EventLog)(nil)

// EventLogOptions are an event log's bounds and its journal. Zero values mean
// the defaults.
type EventLogOptions struct {
	// RingEvents and RingBytes bound the ring of recent records a resuming
	// subscriber replays from, by count and by encoded size. Defaults 4096
	// and 8 MiB. The newest record is always kept, even one larger than
	// RingBytes on its own.
	RingEvents int
	RingBytes  int
	// MaxRecordBytes is the largest body kept: a larger event becomes an
	// oversized omitted record (the primary still gets it whole). Default
	// 8 MiB, the journal's own default. With a journal, the journal's
	// MaxRecordBytes caps it, so an event over either limit is the same
	// omitted record in the ring, every subscription and the file, and the
	// log counts and notes the omission.
	MaxRecordBytes int
	// Journal receives every record and every Note. nil: no journal.
	Journal *journal.Writer
	// Incarnation is the log's id. "" mints a fresh UUIDv7, which is what
	// every caller wants; a caller that builds a journal passes the id it
	// built the journal with (NewIncarnation), because the journal's file
	// name and header carry it and the journal is built first.
	Incarnation string
}

// NewIncarnation mints a host incarnation id, a UUIDv7 (plan 020 §3.7): one
// process lifetime of one session, the scope of its sequence numbers and
// its cursors.
func NewIncarnation() string {
	return uuid.NewV7().String()
}

// EventLog is one session's event log.
//
// The publishing boundary is a channel semaphore (sem, capacity 1), not a
// mutex, so that waiting for it can be abandoned: a publisher waiting behind
// another publisher's blocked primary send still honors its own ctx and its
// session's done (emitCtx's contract for the pre-wire EventCommand). Holding
// it across the blocking primary send is deliberate: the primary's order is
// the sequence's order with no second queue, and a Publish that returned true
// has its event in the primary's buffer. Every blocking point inside the
// boundary selects on closed, so Close can always acquire it.
//
// The boundary is a strict leaf. A session may acquire it with its emitMu
// held, never the reverse, and never with its s.mu held (already the emit
// rule: emitParked). While it is held, nothing does I/O, calls into a
// session, or writes a diagnostic; the only locks taken under it are leaves:
// a subscription's mu and the journal's queue mutex. Note never takes it.
//
// Around the boundary is the in-flight region: every Publish and TryPublish
// enters it before the boundary and leaves it as it returns, so the closing
// diag's count includes every publish that was in flight when the cutoff
// came. It is a counter, not a lock, and entering it never waits: a publisher
// either counts itself in or, once Close has begun, is refused on the spot.
// That is what makes Close safe to wait for it — every publisher inside
// escapes on closed — without a waiting Close ever holding a publisher off
// the wire, which is emitCtx's contract for the pre-wire EventCommand. An
// event is encoded before the region is entered, so no caller code (an Err's
// Error method, a classification over the caller's error type) ever runs
// where Close is waiting.
//
// Lock order: a session's queueOp → emitMu → the in-flight region → the
// boundary → a subscription's mu, and the boundary → the journal's queue
// mutex; noteMu → the journal's queue mutex, and only Close takes noteMu
// under the boundary. inflightMu guards the counter alone and is a leaf below
// everything: it is never held across the boundary, a channel operation, a
// hook, or any other lock.
type EventLog struct {
	incarnation string
	primary     chan Event
	sem         chan struct{} // the publishing boundary; holding it is having sent on it
	closed      chan struct{} // closed by Close: the admission cutoff
	closeOnce   sync.Once
	journal     *journal.Writer
	maxRecord   int

	// Guarded by the boundary.
	next uint64          // the last seq committed
	ring recordRing      // recent records, oldest first
	subs []*Subscription // subscriptions offered each record; a terminated one is swept lazily

	// inflightMu guards the in-flight region's state (see above): closing,
	// set once by Close, after which no publisher is admitted; inflight, the
	// publishers inside; and idle, closed by the last of them to leave once
	// closing is set, which is what Close waits on.
	inflightMu sync.Mutex
	closing    bool
	inflight   int
	idle       chan struct{}

	// noteMu orders Note against Close: Note holds it shared while it checks
	// the cutoff and queues its note, and Close holds it while it sets the
	// cutoff and writes closing, so no note can land after closing. It never
	// spans anything that blocks.
	noteMu   sync.RWMutex
	notesCut bool

	// owners counts subscription owner goroutines, so Close can wait for
	// every one of them; liveOwners is the same count, readable by tests.
	owners     sync.WaitGroup
	liveOwners atomic.Int64

	// Health counters.
	omitted        atomic.Int64
	subsDropped    atomic.Int64
	droppedAtClose atomic.Int64
	notesDropped   atomic.Int64

	// hooks are test seams; nil in production.
	hooks *logHooks
}

// admitKind says who is waiting to enter the boundary, for a test's hook.
type admitKind uint8

const (
	admitPublish admitKind = iota + 1
	admitSubscribe
)

// logHooks let a test know, rather than guess, where a goroutine is: waiting
// for the boundary, inside it about to block on the primary, giving an event
// up, or delivering one — and hold it there.
type logHooks struct {
	// admitting runs just before a Publish or Subscribe waits for the
	// boundary.
	admitting func(admitKind)
	// beforePrimarySend runs inside the boundary, just before Publish's
	// blocking primary send, with the seq the event would take.
	beforePrimarySend func(seq uint64)
	// abandoning runs when a Publish or TryPublish gives its event up, before
	// the drop is counted; inside says whether it holds the boundary.
	abandoning func(inside bool)
	// closeWaits runs inside Close's wait for the publishes in flight, with
	// how many there are — so it runs only while Close is really waiting for
	// one, never merely on its way to the wait.
	closeWaits func(inflight int)
	// delivered runs in a subscription's owner just after Records took the
	// record with seq. A subscription keeps the hooks its log had when it
	// was opened.
	delivered func(seq uint64)
}

// NewEventLog builds an event log. It starts nothing: the only goroutines a
// log ever has are one owner per subscription and, with a journal, the
// journal's writer, and Close ends them all.
func NewEventLog(o EventLogOptions) *EventLog {
	inc := o.Incarnation
	if inc == "" {
		inc = NewIncarnation()
	}
	maxRecord := orDefault(o.MaxRecordBytes, defaultMaxRecordBytes)
	if j := o.Journal.MaxRecordBytes(); j > 0 {
		maxRecord = min(maxRecord, j)
	}
	return &EventLog{
		incarnation: inc,
		primary:     make(chan Event, primaryCap),
		sem:         make(chan struct{}, 1),
		closed:      make(chan struct{}),
		idle:        make(chan struct{}),
		journal:     o.Journal,
		maxRecord:   maxRecord,
		ring: recordRing{
			maxItems: orDefault(o.RingEvents, defaultRingEvents),
			maxBytes: orDefault(o.RingBytes, defaultRingBytes),
		},
	}
}

func orDefault(v, def int) int {
	if v <= 0 {
		return def
	}
	return v
}

// Incarnation is the log's id, the scope of its sequence numbers.
func (l *EventLog) Incarnation() string { return l.incarnation }

// Primary is the session's own event stream, what Events() returns: every
// published event, in sequence order, with Seq set. Its buffer is 256 and it
// is never closed.
func (l *EventLog) Primary() <-chan Event { return l.primary }

// release leaves the boundary.
func (l *EventLog) release() { <-l.sem }

// enter joins the in-flight region, or refuses once Close has begun. It never
// waits: the only thing it takes is the counter's own leaf mutex, which
// nothing holds across anything. A publisher that entered must leave.
func (l *EventLog) enter() bool {
	l.inflightMu.Lock()
	defer l.inflightMu.Unlock()
	if l.closing {
		return false
	}
	l.inflight++
	return true
}

// leave leaves the in-flight region, waking a Close that is waiting for the
// last publisher out.
func (l *EventLog) leave() {
	l.inflightMu.Lock()
	defer l.inflightMu.Unlock()
	l.inflight--
	if l.closing && l.inflight == 0 {
		close(l.idle)
	}
}

// Publish numbers ev, hands it to the primary, and records it for the ring,
// every subscription and the journal. It blocks while the primary is full,
// exactly as a send on the session's events channel did, and returns false —
// the event dropped for everyone, consuming no number — when done closes,
// ctx ends, or the log is closed first, whether it was waiting for the
// boundary or for the primary. A true return means the event is in the
// primary's buffer. ctx and done may be nil.
//
// The event is encoded before the boundary and before the in-flight region,
// on the publisher's goroutine: the value is the publisher's own copy by then
// (cloneTool and its kin), encoding is the expensive part, and it runs the
// caller's own code (an Err's Error method), which must not run anywhere a
// Close is waiting for it.
func (l *EventLog) Publish(ctx context.Context, done <-chan struct{}, ev Event) bool {
	rec := l.record(ev)
	if !l.enter() {
		return l.abandon(true)
	}
	defer l.leave()
	var cancelled <-chan struct{}
	if ctx != nil {
		cancelled = ctx.Done()
	}
	if h := l.hooks; h != nil && h.admitting != nil {
		h.admitting(admitPublish)
	}
	select {
	case l.sem <- struct{}{}:
	case <-done:
		return l.abandon(true)
	case <-cancelled:
		return l.abandon(false)
	case <-l.closed:
		return l.abandon(true)
	}
	// A select picks at random among ready cases, so a publisher can be
	// admitted after the cutoff or after its session began closing; the
	// event is dropped all the same.
	select {
	case <-l.closed:
		return l.abandonLocked(true)
	case <-done:
		return l.abandonLocked(true)
	default:
	}
	seq := l.next + 1
	ev.Seq = seq
	if h := l.hooks; h != nil && h.beforePrimarySend != nil {
		h.beforePrimarySend(seq)
	}
	select {
	case l.primary <- ev:
	case <-done:
		return l.abandonLocked(true)
	case <-cancelled:
		return l.abandonLocked(false)
	case <-l.closed:
		return l.abandonLocked(true)
	}
	rec.Seq = seq
	l.commitLocked(rec)
	l.release()
	return true
}

// abandonLocked leaves the boundary without committing: the event is dropped
// for everyone and its number was never taken. counted says the escape was on
// done or the cutoff, which the closing diag reports; the count is taken
// before the boundary is released, so a Close waiting for the boundary reads
// it. It returns false, Publish's answer.
func (l *EventLog) abandonLocked(counted bool) bool {
	if h := l.hooks; h != nil && h.abandoning != nil {
		h.abandoning(true)
	}
	if counted {
		l.droppedAtClose.Add(1)
	}
	l.release()
	return false
}

// abandon is abandonLocked for a publisher that never entered the boundary.
func (l *EventLog) abandon(counted bool) bool {
	if h := l.hooks; h != nil && h.abandoning != nil {
		h.abandoning(false)
	}
	if counted {
		l.droppedAtClose.Add(1)
	}
	return false
}

// TryPublish is Publish for an event that is lossy by contract (plan 019's
// tool progress): it never blocks. When the boundary is held or the primary
// is full, the event is dropped for everyone, consumes no number, and it
// returns false.
func (l *EventLog) TryPublish(ev Event) bool {
	rec := l.record(ev)
	// Entering the in-flight region never waits, so this keeps its contract
	// trivially: refused means Close has begun, and the event is dropped.
	if !l.enter() {
		return l.abandon(true)
	}
	defer l.leave()
	select {
	case <-l.closed:
		return l.abandon(true)
	default:
	}
	select {
	case l.sem <- struct{}{}:
	default:
		return false
	}
	select {
	case <-l.closed:
		return l.abandonLocked(true)
	default:
	}
	seq := l.next + 1
	ev.Seq = seq
	select {
	case l.primary <- ev:
	default:
		return l.abandonLocked(false)
	}
	rec.Seq = seq
	l.commitLocked(rec)
	l.release()
	return true
}

// record builds ev's record, without its seq: the body, or the omitted
// marker for an event the codec refuses, one over MaxRecordBytes, or one
// whose type is too long to be a type.
//
// The journal refuses an EventType over journal.MaxEventTypeBytes as well —
// a line no reader would accept — and the rule is applied here, where the
// record is built, so the ring, every subscription and the file carry the one
// omitted record and the log counts and notes the omission like any other.
// The primary still gets the event whole; only the record is omitted.
func (l *EventLog) record(ev Event) Record {
	rec := Record{At: ev.At, Type: ev.Type}
	if len(ev.Type) > journal.MaxEventTypeBytes {
		rec.Type = journal.OverlongEventType
		rec.Omitted = &Omitted{Reason: journal.OmittedEncodeError, Error: fmt.Sprintf(
			"agent: the event type is %d bytes, over the %d-byte cap", len(ev.Type), journal.MaxEventTypeBytes)}
		return rec
	}
	body, err := EncodeEvent(ev)
	switch {
	case err != nil:
		rec.Omitted = &Omitted{Reason: journal.OmittedEncodeError, Error: omittedError(err)}
	case len(body) > l.maxRecord:
		rec.Omitted = &Omitted{Reason: journal.OmittedOversized, Bytes: len(body)}
	default:
		rec.Body = body
	}
	return rec
}

// omittedError is err's message as an omitted record keeps it: valid UTF-8
// (so the journal's JSON writes it byte for byte) and capped, cut between
// runes.
func omittedError(err error) string {
	s := strings.ToValidUTF8(err.Error(), "\uFFFD")
	if len(s) <= maxOmittedError {
		return s
	}
	n := maxOmittedError
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// commitLocked makes rec, whose event the primary has taken, part of the
// sequence: the ring, every subscription's live buffer, and the journal. It
// never blocks. A subscription whose buffer is full is marked dropped and
// unregistered here; its owner goroutine does the rest. The notes a commit
// owes are queued after the record, and only for a committed record, so an
// abandoned publish leaves no orphan. The caller holds the boundary.
func (l *EventLog) commitLocked(rec Record) {
	l.next = rec.Seq
	l.ring.push(rec)
	dropped := 0
	kept := l.subs[:0]
	for _, s := range l.subs {
		switch s.offer(rec) {
		case offerKept:
			kept = append(kept, s)
		case offerDropped:
			dropped++
		}
	}
	clear(l.subs[len(kept):])
	l.subs = kept
	l.journal.Append(rec.journalRecord())
	if rec.Omitted != nil {
		l.omitted.Add(1)
		if l.journal != nil {
			l.journal.Note(journal.DiagNote{Kind: journal.DiagRecordOmitted, Fields: map[string]any{
				"seq": rec.Seq, "eventType": string(rec.Type), "reason": rec.Omitted.Reason,
				"bytes": rec.Omitted.Bytes, "error": rec.Omitted.Error,
			}})
		}
	}
	if dropped > 0 {
		l.subsDropped.Add(int64(dropped))
		if l.journal != nil {
			l.journal.Note(journal.DiagNote{Kind: journal.DiagSubscriberDropped, Fields: map[string]any{
				"seq": rec.Seq, "subscribers": dropped,
			}})
		}
	}
}

// Subscribe opens a subscription: a replay of every record after o.After,
// then every record published from now on, on its own channel, in order and
// with nothing skipped. A cursor that cannot be served whole is refused here,
// synchronously, as ErrCursorUnresolvable, and nothing is registered.
//
// It waits for the boundary with no ctx, like a publisher with none: while
// the primary is wedged it waits, and closing the log releases it with
// ErrClosed. The cutoff is taken inside the boundary: the last seq N, and a
// pin on every ring record in (After.Seq, N] — references to immutable
// records, which the ring may evict afterwards without touching the pinned
// copies — and live delivery begins at N+1.
func (l *EventLog) Subscribe(o SubscribeOptions) (*Subscription, error) {
	o.MaxItems = orDefault(o.MaxItems, defaultSubscribeItems)
	o.MaxBytes = orDefault(o.MaxBytes, defaultSubscribeBytes)
	if h := l.hooks; h != nil && h.admitting != nil {
		h.admitting(admitSubscribe)
	}
	select {
	case l.sem <- struct{}{}:
	case <-l.closed:
		return nil, ErrClosed
	}
	defer l.release()
	select {
	case <-l.closed:
		return nil, ErrClosed
	default:
	}
	n := l.next
	// C5c reads the journal's complete-record boundary here, under the same
	// admission, so the file leg and the pin describe one instant.
	prev := n
	var pinned []Record
	var pinnedBytes int
	if a := o.After; a != nil {
		switch {
		case a.Incarnation != l.incarnation:
			return nil, ErrCursorUnresolvable{Reason: CursorForeignIncarnation}
		case a.Seq > n:
			return nil, ErrCursorUnresolvable{Reason: CursorFutureSeq}
		}
		prev = a.Seq
		skip, count, bytes := l.ring.after(a.Seq)
		if bytes > o.MaxBytes {
			return nil, ErrCursorUnresolvable{Reason: CursorBacklogTooLarge}
		}
		oldest := n + 1
		if count > 0 {
			oldest = l.ring.at(skip).Seq
		}
		if oldest > a.Seq+1 {
			if err := l.headFromFile(a.Seq+1, oldest-1); err != nil {
				return nil, err
			}
		}
		pinned, pinnedBytes = l.ring.copyOut(skip, count), bytes
	}
	s := l.newSubscription(o.MaxItems, o.MaxBytes, pinnedBytes)
	l.sweepLocked()
	l.subs = append(l.subs, s)
	l.startOwner(s, pinned, prev)
	return s, nil
}

// newSubscription is a subscription with its budgets, the pinned replay's
// bytes already counted against maxBytes until each is handed over.
func (l *EventLog) newSubscription(maxItems, maxBytes, pinnedBytes int) *Subscription {
	return &Subscription{
		log:      l,
		hooks:    l.hooks,
		out:      make(chan Record),
		kill:     make(chan struct{}),
		notify:   make(chan struct{}, 1),
		maxItems: maxItems,
		maxBytes: maxBytes,
		bytes:    pinnedBytes,
	}
}

// startOwner starts s's owner goroutine, which delivers pinned (the replay,
// oldest first) after prev and then the live buffer. Subscribe calls it
// inside the boundary, so Close, which acquires the boundary before it
// waits, always finds the owner counted.
func (l *EventLog) startOwner(s *Subscription, pinned []Record, prev uint64) {
	l.owners.Add(1)
	l.liveOwners.Add(1)
	go s.run(pinned, prev)
}

// headFromFile is where a resume whose head, [from, to], has left the ring
// gets it from the journal file. That leg is C5c's (plan 020 §3.2: ReadRange
// from the writer's own file, gaps and a stalled writer refused); until it
// lands the head is never servable, so the cursor is no_journal whether or
// not a journal is attached.
func (l *EventLog) headFromFile(from, to uint64) error {
	return ErrCursorUnresolvable{Reason: CursorNoJournal}
}

// sweepLocked unregisters subscriptions that have ended, so a log that sees
// subscriptions come and go between publishes does not accumulate them. The
// caller holds the boundary.
func (l *EventLog) sweepLocked() {
	l.subs = slices.DeleteFunc(l.subs, func(s *Subscription) bool { return s.terminal() != nil })
}

// Abandoned counts an emit its caller dropped on its own done fast path,
// before reaching Publish: an event given up since the session's teardown
// began, which the closing diag's droppedAtClose counts beside the publishes
// the log gave up itself (plan 020 §3.5). It never blocks and never takes
// the boundary. One that races Close may land after closing was written;
// Health counts it all the same.
func (l *EventLog) Abandoned() { l.droppedAtClose.Add(1) }

// Note queues a journal-only note (a session id, a prompt, a diagnostic). It
// never blocks and never takes the boundary, so a wedged primary never holds
// back a prompt note or the stderr tee. After Close it is counted and
// dropped; before Close, with no journal, it does nothing.
func (l *EventLog) Note(n journal.Note) {
	l.noteMu.RLock()
	defer l.noteMu.RUnlock()
	if l.notesCut {
		l.notesDropped.Add(1)
		return
	}
	l.journal.Note(n)
}

// Close shuts the log (plan 020 §3.5): the admission cutoff, so every later
// Publish returns false and every later Note is counted and dropped; a
// closing diag counting the publishes abandoned on done or the cutoff — every
// one that was in flight when the cutoff came included, and every emit its
// session dropped on its own done fast path before then (Abandoned); every
// subscription ended with ErrClosed and its owner goroutine waited for; and
// the journal closed, waiting at most its own bound (500 ms) and at most
// until ctx ends. It is idempotent: a second call returns once the first has
// finished. It is safe on a log never published to.
func (l *EventLog) Close(ctx context.Context) {
	l.closeOnce.Do(func() {
		close(l.closed)
		l.awaitInFlight()
		// Serialize with a Subscribe mid-registration. No publisher holds
		// the boundary now, and Subscribe waits for nothing inside it.
		l.sem <- struct{}{}
		subs := l.subs
		l.subs = nil
		l.noteMu.Lock()
		l.notesCut = true
		// C5b: prompt attempts still open get their prompt_end{errClass:
		// closed} here, before closing and under the same cutoff.
		if l.journal != nil {
			l.journal.Note(journal.DiagNote{Kind: journal.DiagClosing, Fields: map[string]any{
				"droppedAtClose": l.droppedAtClose.Load(),
			}})
		}
		l.noteMu.Unlock()
		l.release()
		for _, s := range subs {
			s.terminate(ErrClosed)
		}
		// Every owner's blocking point selects on its subscription's kill,
		// which is closed by now for every subscription there is: the ones
		// just ended, and the ones swept earlier, which were swept because
		// they had ended.
		l.owners.Wait()
		if ctx == nil {
			ctx = context.Background()
		}
		_ = l.journal.Close(ctx)
	})
}

// awaitInFlight shuts the in-flight region and waits for the publishes inside
// it to leave, counted if they gave their event up: each one's waits select on
// closed, which Close has closed by now, so each leaves at once. A publish
// that arrives from here on is refused at the region's door rather than made
// to wait; it is counted in Health, after closing.
//
// The wait is a channel receive, not a wait under the counter's mutex, so the
// mutex stays a leaf held across nothing — the hook included.
func (l *EventLog) awaitInFlight() {
	for {
		l.inflightMu.Lock()
		l.closing = true
		n := l.inflight
		l.inflightMu.Unlock()
		if n == 0 {
			return
		}
		if h := l.hooks; h != nil && h.closeWaits != nil {
			h.closeWaits(n)
		}
		// idle is closed by the last publisher out, which can only be once
		// closing is set, so this loop goes round at most twice.
		<-l.idle
	}
}

// EventLogHealth is a copy of the log's counters and its journal's health.
type EventLogHealth struct {
	// Omitted counts records published without their body.
	Omitted int
	// SubscribersDropped counts subscriptions ended as slow consumers.
	SubscribersDropped int
	// DroppedAtClose counts publishes abandoned on done or the cutoff, and
	// emits their session dropped on its own done fast path (Abandoned).
	DroppedAtClose int
	// NotesDropped counts notes refused after Close.
	NotesDropped int
	// Journal is the journal's health; StateOff when there is none.
	Journal journal.Health
}

// Health is the log's health. It never takes the boundary.
func (l *EventLog) Health() EventLogHealth {
	return EventLogHealth{
		Omitted:            int(l.omitted.Load()),
		SubscribersDropped: int(l.subsDropped.Load()),
		DroppedAtClose:     int(l.droppedAtClose.Load()),
		NotesDropped:       int(l.notesDropped.Load()),
		Journal:            l.journal.Health(),
	}
}

// ownerDone is a subscription owner's last act.
func (l *EventLog) ownerDone() {
	l.liveOwners.Add(-1)
	l.owners.Done()
}

// Subscription is one reader of a log beside its primary. Exactly one
// goroutine, its owner, sends on and closes Records: the replay first, then
// the live buffer, in order. Publishers only append to the live buffer,
// without blocking; a buffer that would overflow ends the subscription with
// ErrSlowConsumer instead of making a publisher wait.
type Subscription struct {
	log      *EventLog
	hooks    *logHooks // the log's test seams when it was opened; nil in production
	out      chan Record
	kill     chan struct{} // closed, once, with cause set: the subscription is over
	notify   chan struct{} // capacity 1: the live buffer has something new
	maxItems int
	maxBytes int

	// mu guards the rest. It is a leaf: publishers take it inside the
	// boundary, and nothing holding it waits on anything.
	mu        sync.Mutex
	live      []Record // published, not yet taken by the owner
	liveItems int      // live records not yet handed over: buffered, or taken by the owner and not yet being sent
	bytes     int      // their bytes, plus the pinned replay's not yet being sent
	cause     error    // the first terminal cause
	err       error    // cause, published once the owner has stopped
}

// Records is the subscription's stream. It is closed when the subscription
// ends, and Err then says why.
func (s *Subscription) Records() <-chan Record { return s.out }

// Err is why the subscription ended: ErrClosed, ErrSlowConsumer, or an
// ErrCursorUnresolvable from its replay. It is stored before Records is
// closed and never changes after; before that it is nil.
func (s *Subscription) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// Close ends the subscription with ErrClosed. It only marks it and signals
// the owner, never taking the log's boundary, so it cannot block behind a
// wedged primary. It is idempotent, and does nothing to a subscription that
// already ended.
func (s *Subscription) Close() { s.terminate(ErrClosed) }

// terminate ends the subscription with cause unless it has already ended.
func (s *Subscription) terminate(cause error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.endLocked(cause)
}

// endLocked sets the terminal cause and closes kill, unless the subscription
// has already ended: kill is closed once, and only with a cause set. The
// caller holds s.mu.
func (s *Subscription) endLocked(cause error) {
	if s.cause == nil {
		s.cause = cause
		close(s.kill)
	}
}

// terminal is the subscription's terminal cause: nil while it runs, set once
// kill is closed.
func (s *Subscription) terminal() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cause
}

// offerResult is what a publisher does with a subscription after offering
// it a record.
type offerResult uint8

const (
	offerKept    offerResult = iota // buffered; stays registered
	offerDropped                    // this record overflowed it: dropped now
	offerGone                       // it had already ended
)

// offer appends rec to the live buffer without blocking. Overflow, by items
// or by bytes (the pinned replay not yet handed over counts toward the bytes),
// marks the subscription dropped and signals its owner; the owner closes
// Records.
func (s *Subscription) offer(rec Record) offerResult {
	size := rec.size()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cause != nil {
		return offerGone
	}
	if s.liveItems >= s.maxItems || s.bytes+size > s.maxBytes {
		s.endLocked(ErrSlowConsumer)
		return offerDropped
	}
	s.live = append(s.live, rec)
	s.liveItems++
	s.bytes += size
	select {
	case s.notify <- struct{}{}:
	default:
	}
	return offerKept
}

// run is the owner goroutine: it delivers, then records why it stopped
// before closing Records.
func (s *Subscription) run(pinned []Record, prev uint64) {
	defer s.log.ownerDone()
	s.finish(s.pump(pinned, prev))
}

// pump delivers the replay and then the live buffer until the subscription
// ends, and returns why it did. prev is the seq the reader already has: the
// cursor, or the cutoff for a live-only subscription.
//
// Each record's charge against the budget is released just before its
// blocking send, not after it returns: once Records has taken a record, a
// publisher may offer the next before this goroutine runs again, and a
// subscription that kept within its budget must not be dropped for a record
// its reader already has.
func (s *Subscription) pump(pinned []Record, prev uint64) error {
	// C5c: the file leg, (After.Seq, R-1], is streamed here first.
	for i := range pinned {
		rec := pinned[i]
		pinned[i] = Record{} // handed over: the pin no longer holds it
		s.handOver(0, rec.size())
		if err := s.send(rec, &prev); err != nil {
			return err
		}
	}
	var batch []Record
	for {
		s.mu.Lock()
		if err := s.cause; err != nil {
			s.mu.Unlock()
			return err
		}
		// Take what the publishers buffered and hand them back the emptied
		// storage of the last batch, so a steady stream allocates nothing.
		batch, s.live = s.live, batch[:0]
		s.mu.Unlock()
		if len(batch) == 0 {
			select {
			case <-s.notify:
			case <-s.kill:
			}
			continue
		}
		for i := range batch {
			rec := batch[i]
			batch[i] = Record{}
			s.handOver(1, rec.size())
			if err := s.send(rec, &prev); err != nil {
				return err
			}
		}
	}
}

// handOver releases one record's charge as the owner begins to send it:
// items is 1 for a live record, 0 for a pinned one, which counts against
// the bytes only.
func (s *Subscription) handOver(items, bytes int) {
	s.mu.Lock()
	s.liveItems -= items
	s.bytes -= bytes
	s.mu.Unlock()
}

// send delivers one record, unless the subscription ends first. A record that
// does not follow the last one delivered ends it instead: a hole is never
// delivered silently.
func (s *Subscription) send(rec Record, prev *uint64) error {
	if rec.Seq != *prev+1 {
		return fmt.Errorf("%w: seq %d after %d", errNotContiguous, rec.Seq, *prev)
	}
	select {
	case <-s.kill:
		return s.terminal()
	default:
	}
	select {
	case s.out <- rec.detached():
		*prev = rec.Seq
		if h := s.hooks; h != nil && h.delivered != nil {
			h.delivered(rec.Seq)
		}
		return nil
	case <-s.kill:
		return s.terminal()
	}
}

// finish records why the subscription ended, then closes Records: the error
// is stored before the close, so a reader that sees the channel closed reads
// the final Err.
func (s *Subscription) finish(err error) {
	s.mu.Lock()
	s.endLocked(err)
	s.err = s.cause
	s.live = nil
	s.mu.Unlock()
	close(s.out)
}

// recordRing is the log's recent records, oldest first, and always
// contiguous: records arrive in seq order and leave from the front. It is a
// circular buffer that grows by doubling up to its count bound, and evicts
// by count and by bytes. Evicting a record drops the ring's reference only;
// a pinned copy keeps its own.
type recordRing struct {
	buf      []Record
	head     int // index of the oldest record
	n        int
	bytes    int
	maxItems int
	maxBytes int
}

// push appends rec, evicting from the front until it fits. The newest
// record is kept even when it alone is over maxBytes.
func (r *recordRing) push(rec Record) {
	size := rec.size()
	for r.n > 0 && (r.n >= r.maxItems || r.bytes+size > r.maxBytes) {
		r.evict()
	}
	if r.n == len(r.buf) {
		r.grow()
	}
	r.buf[(r.head+r.n)%len(r.buf)] = rec
	r.n++
	r.bytes += size
}

func (r *recordRing) evict() {
	old := &r.buf[r.head]
	r.bytes -= old.size()
	*old = Record{}
	r.head = (r.head + 1) % len(r.buf)
	r.n--
}

// grow doubles the buffer (from 16), up to maxItems, keeping the records in
// order from index 0. It is called only when the buffer is full, which push
// allows only below maxItems.
func (r *recordRing) grow() {
	size := min(max(16, 2*len(r.buf)), r.maxItems)
	buf := make([]Record, size)
	for i := range r.n {
		buf[i] = r.buf[(r.head+i)%len(r.buf)]
	}
	r.buf, r.head = buf, 0
}

// at is the i-th record from the oldest.
func (r *recordRing) at(i int) Record { return r.buf[(r.head+i)%len(r.buf)] }

// after locates the records with a seq after `after`: how many of the oldest
// to skip, how many there are, and their bytes.
func (r *recordRing) after(after uint64) (skip, count, bytes int) {
	if r.n == 0 {
		return 0, 0, 0
	}
	if first := r.at(0).Seq; after >= first {
		skip = int(min(after-first+1, uint64(r.n)))
	}
	for i := skip; i < r.n; i++ {
		bytes += r.at(i).size()
	}
	return skip, r.n - skip, bytes
}

// copyOut is count records from the skip-th oldest, copied: the pin.
func (r *recordRing) copyOut(skip, count int) []Record {
	if count == 0 {
		return nil
	}
	out := make([]Record, count)
	for i := range out {
		out[i] = r.at(skip + i)
	}
	return out
}
