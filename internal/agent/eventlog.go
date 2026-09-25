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
// Replayed) and its own done fast-path, and then calls
// Publish where it used to send on its events channel; Events() returns
// Primary(). What Publish adds is a sequence number, a ring of recent records
// a resuming subscriber can replay from, any number of subscriptions beside
// the primary reader, and the journal's copy — all inline, on the publisher's
// own goroutine, so nothing is ever between an emit and the primary channel.
// That is what keeps "emitted means buffered" (internal/cli/prompt.go's
// drainBuffered) exactly true.
//
// Beside that path, and only for callers above the provider seam, is the outbox
// (plan 021 §3.3): Enqueue hands over a batch and returns, and one log-owned
// goroutine publishes it through that same commit path a moment later. It is the
// one thing in the log that is not inline, which is why "emitted means buffered"
// is a statement about what a *session* publishes directly, and why a primary
// client must keep reading until it closes the session. Flush is how a caller
// that needs the barrier back waits for it.
//
// A subscription resuming from further back than the ring reaches has the
// head of its range read from the journal's own file instead (headRange), so
// a cursor outlives an eviction for as long as the journal holds what it
// points at. The whole range is served or the subscription fails: nothing is
// ever truncated or skipped. A failure over the file may come after a prefix
// of the range has been delivered — headRange.serve says why, and what a
// consumer owes that prefix.

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
	// defaultOutboxEvents and defaultOutboxBytes are the outbox's soft bound
	// (plan 021 §3.3), the one OutboxRoom reports on: the same numbers as the
	// ring's, because the outbox is the same kind of buffer of the same kind of
	// records, and a caller over either is a caller nobody is reading.
	defaultOutboxEvents = 4096
	defaultOutboxBytes  = 8 << 20
)

// maxOmittedError caps an omitted record's error message. It is well under
// the journal's own cap, so the journal keeps the message exactly as the ring
// holds it and a ring replay and a file replay carry the same record.
const maxOmittedError = 1 << 10

// journalWaitBound is how long the file leg of a resume waits for a writer
// that has not yet written the head of the range (plan 020 §3.2). It is a var
// only so a test can shorten it; nothing in production changes it.
var journalWaitBound = 2 * time.Second

var (
	// ErrClosed ends a subscription that was closed, by its own Close or by
	// its log's, and is what Subscribe returns on a closed log. It is not
	// acp.ErrClosed: that one is an agent connection ending under a turn.
	// Subscription.Rest tells the two closes apart.
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
	// ErrLogClosing is Flush's answer once Close has begun: the outbox takes no
	// more work, so a barrier on it can no longer mean what it means. It is not
	// ErrClosed, which ends a subscription — a caller that sees this one has
	// simply asked for an ordering it can stop waiting for (plan 021 §3.3).
	ErrLogClosing = errors.New("agent: event log closing")
	// ErrFlushGaveUp is Flush's answer when the done channel its caller passed
	// closed first: the session that wanted the ordering is shutting down, so
	// the barrier is abandoned exactly as a Publish from the same goroutine is
	// abandoned (Publish). The events themselves are not lost — the log's own
	// Close commits what the outbox still holds — and every caller that passes a
	// done treats the barrier as an ordering nicety, so this is a fact about the
	// wait and not about the record.
	ErrFlushGaveUp = errors.New("agent: the flush was given up: its session is closing")
	// ErrRestUnavailable is Subscription.Rest's answer for a subscription whose
	// replay was still reading its head from the journal file when the log's
	// Close ended it — waiting for the writer, part-way through the range, or not
	// yet begun. What it had not delivered is partly in a file it no longer
	// reads, so any tail it could hand back would be a suffix posing as the
	// whole, and it hands back none (plan 027 §3.7).
	ErrRestUnavailable = errors.New("agent: no closing tail: the subscription's journal replay was outstanding when its log closed")
	// ErrNoRest is Subscription.Rest's answer for a subscription that did not
	// end by its log's Close — a detach (its own Close), ErrSlowConsumer, a
	// replay that could not be served, a hole — or has not ended yet. Only a log
	// closing under a subscription leaves it records it owes and cannot deliver.
	ErrNoRest = errors.New("agent: no closing tail: the subscription did not end by its log's close, or has not ended")
	// ErrObserverSet refuses a second Observe. The observer is one per log, set
	// before the session publishes anything, because it runs inside the
	// publishing boundary and two of them would be a fan-out with no budget.
	ErrObserverSet = errors.New("agent: the event log already has an observer")
	// errNilObserver refuses an observer that is nil: Observe would then read
	// as "unset", and the second, real call would be the one refused.
	errNilObserver = errors.New("agent: the event log's observer must not be nil")
	// errEnqueuedErr is what Enqueue puts in place of an Err an enqueued event
	// should never have carried (Enqueue's contract). It is a plain sentinel
	// created here, so encoding it runs nothing but this package's own code: its
	// Error is a constant, it wraps nothing, and it has no Is or As of its own,
	// so the codec's classification walks it without calling anything foreign.
	errEnqueuedErr = errors.New("agent: an enqueued event carried Err, which the outbox does not read")
)

// CursorReason is why a cursor cannot be resumed from.
type CursorReason string

// Reasons Subscribe gives for refusing a cursor. The last four belong to the
// journal leg of a resume (plan 020 §3.2): they are why a head that has left
// the ring could not be served from the journal file either, and the last
// three of those reach the caller through the subscription, since the reading
// is the owner's (headRange.unresolvable has the mapping).
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
	// Err is what the journal said, when the journal's leg of a resume is why
	// (headRange.unresolvable): a wrapped journal.ErrBehind, ErrGap, ErrFailed
	// or ErrClosed, the wait's own deadline, or an I/O error. It is nil for a
	// cursor refused without reading anything.
	Err error
}

func (e ErrCursorUnresolvable) Error() string {
	s := "agent: event log: cannot resume from the cursor: " + string(e.Reason)
	if e.Err != nil {
		s += ": " + e.Err.Error()
	}
	return s
}

// Unwrap gives the journal's own error, so a caller can tell a writer that
// failed from one that was merely behind.
func (e ErrCursorUnresolvable) Unwrap() error { return e.Err }

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

// recordFromJournal is a record read back from a journal file, as the log's
// own: the same envelope, and the same body or omitted marker the ring held
// when it was published. The reader builds an Omitted of its own per record,
// so nothing here is shared with anything.
func recordFromJournal(r journal.Record) Record {
	return Record{Seq: r.Seq, At: r.At, Type: EventType(r.EventType), Body: r.Body, Omitted: r.Omitted}
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
	// Ctx, when set, bounds the one wait Subscribe makes: for the publishing
	// boundary, which a publisher holds while it waits for room in the
	// primary (plan 024 §3.6). Ending it there returns Ctx.Err() with nothing
	// registered and no goroutine started, and so does a Ctx already ended when
	// the boundary is acquired. It is consulted nowhere else: not by the
	// cutoff, not by the subscription once Subscribe has returned it (Close
	// ends that), and not by the file leg of its replay. nil waits as a
	// publisher with no ctx does — until the boundary is free or the log
	// closes.
	Ctx context.Context
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

// LogOwner is a session that will hand its event log over: what a component
// above the provider seam needs in order to publish into the session's own
// sequence instead of beside it (plan 021 §3.3). Like EventSource it is optional
// and found by a type assertion, so agent.Session itself does not change; every
// Session in this repo implements it, and engine.New refuses a session that does
// not — a driver that cannot record what it does is not one craze will run.
type LogOwner interface {
	EventLog() *EventLog
}

// journalFile is what the file leg of a resume needs of the journal: the
// health it consults before it reads anything, the complete-record boundary
// it takes at the cutoff, the writer's own live file, and the wait for a
// writer that has not caught up. Every method is the *journal.Writer's, which
// is what production always passes; it is an interface so that a test can
// hold a writer still, which the journal's own stall seams (unexported, and
// its tests' alone) cannot do from here.
type journalFile interface {
	Health() journal.Health
	Flushed() (seq uint64, bytes int64)
	ReadRange(from, to uint64, fn func(journal.Record) error) error
	WaitFlushed(ctx context.Context, seq uint64) error
}

var _ journalFile = (*journal.Writer)(nil)

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
	// NoPrimary drops the primary send from every publish path: nothing is ever
	// put on Primary(), and so no publisher — not Publish, not the outbox's
	// drainer — can be held by a reader that is not there (plan 021 §3.3).
	// Everything else is identical: the sequence, the ring, every subscription,
	// the journal, the observer, and what Flush and Close mean by "committed".
	//
	// It is what makes a session with no client possible; today the 257th
	// unread event blocks the agent. S1b uses it in tests, S4 in earnest
	// (SD-33), where it becomes the normal mode and a client reads through a
	// budgeted subscription instead.
	NoPrimary bool
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
// boundary selects on something Close closes — closed for a publisher, the
// outbox's cut for the drainer — so Close can always acquire it.
//
// The boundary is a strict leaf. A session never acquires it with its s.mu
// held (already the emit rule: emitCtx). While it is held, nothing does
// I/O, calls into a session, or writes a diagnostic; the only locks taken
// under it are leaves:
// a subscription's mu and the journal's queue mutex. Note never takes it. The
// observer, when one is set, runs there too, under the same rules (Observe).
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
// Beside the boundary is the outbox (plan 021 §3.3): a FIFO of batches a caller
// hands over with Enqueue, and one log-owned goroutine — the drainer, started
// lazily and joined by Close — that publishes them through the ordinary commit
// path, so an enqueued event is numbered and reaches the primary, the ring,
// every subscription and the journal like any other. It exists because the
// engine and the ask registry publish from goroutines that must not block,
// often the primary's own reader, so *state order is event order* cannot be
// bought by holding a state lock across a blocking send. The outbox mutex is a
// strict leaf below every lock in craze: Enqueue may be called — is meant to be
// called — with the caller's own state lock held, and while that mutex is held
// nothing waits for the boundary, the primary, a channel or I/O. Encoding runs
// before it is taken, on the caller's goroutine, for the same reason Publish
// encodes before the boundary; since encoding runs the event's own code, an
// event enqueued under a state lock must be one whose encoding cannot re-enter
// that lock (Enqueue says what that means).
//
// The drainer takes the boundary once per batch and holds it across the whole
// batch, so a batch is contiguous in Seq: no other publisher's event ever lands
// inside one, and a caller that enqueued a transaction sees it come back as a
// transaction. The price is the boundary held across as many blocking primary
// sends as the batch has events — the same wedge one Publish already allows,
// and every waiter for the boundary still escapes on its own ctx, its session's
// done, or the cutoff. The drainer is deliberately *not* in the in-flight
// region: it never abandons an event, so it has nothing for the closing diag to
// count, and being refused at that region's door once closing was set is the one
// thing it must never be — what is in the outbox at close is committed, not
// dropped. Close joins it as a phase of its own instead.
//
// Lock order: the in-flight region → the boundary → a subscription's mu, and
// the boundary → the journal's queue mutex and the observer; noteMu → the
// journal's queue mutex, and only Close takes noteMu under the boundary. A
// session's own emit never holds its s.mu across the boundary (emitCtx says
// why), and there is no queue-transaction lock left on the provider seam
// to hold across it either: the queue and the events describing it are
// mutated and enqueued under one mutex in internal/engine now, with no lock
// held across a blocking send (plan 021 §3.5). inflightMu guards the counter
// alone and is a
// leaf below everything: it is never held across the boundary, a channel
// operation, a hook, or any other lock. outboxMu is a second such leaf, and a
// stricter one: it is never held across the boundary or a blocking channel
// operation, and it is never taken *under* the boundary either — the drainer
// takes it before a batch and after it, never inside, and an observer may not
// call the log at all — so it has no order against the boundary in either
// direction. Above the log, plan 021 §3.3 pins the callers' side of it:
// e.mu → outboxMu, registry.mu → outboxMu, s.mu → outboxMu; and e.mu and
// registry.mu are never held across a *blocking* call — a provider call,
// Cancel, a continuation, Publish, Flush, file I/O — which is what leaves
// Enqueue as the one thing a holder of a state lock may do to the log.
type EventLog struct {
	incarnation string
	primary     chan Event
	sem         chan struct{} // the publishing boundary; holding it is having sent on it
	closed      chan struct{} // closed by Close: the admission cutoff
	closeOnce   sync.Once
	journal     *journal.Writer
	// file is the journal again, as the file leg of a resume uses it; nil
	// without a journal, so a head that has left the ring is no_journal.
	file      journalFile
	maxRecord int
	// noPrimary is EventLogOptions.NoPrimary: no publish path ever sends on
	// primary, so nothing can be held by a reader that is not there.
	noPrimary bool

	// Guarded by the boundary.
	next     uint64          // the last seq committed
	ring     recordRing      // recent records, oldest first
	subs     []*Subscription // subscriptions offered each record; a terminated one is swept lazily
	observer func(Event)     // Observe's, nil when unset; runs at commit
	// committed is next again, stored beside it in commitLocked: the log's
	// committed head, readable without the boundary. It is what FlushSeq answers
	// with once its Flush has returned (plan 027 §3.6).
	committed atomic.Uint64

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
	// cutoff, ends the attempts still open and writes closing, so no note can
	// land after closing. It never spans anything that blocks. Nothing takes
	// it twice: the attempt helpers queue their notes themselves rather than
	// going back through Note, because a second RLock behind a waiting Lock
	// would deadlock.
	noteMu   sync.RWMutex
	notesCut bool

	// attempts are the prompt attempts whose prompt_end has not been written
	// (plan 020 §3.5), oldest first, so Close can end the one whose
	// continuation never ran and the one whose turn it is ending underneath.
	// Nothing is registered at all without a journal. attemptsMu guards them
	// and the id counter; it is a leaf under noteMu, held across nothing.
	attemptsMu sync.Mutex
	attempts   []openAttempt
	attemptSeq uint64

	// outboxMu guards the outbox and everything that accounts for it (see the
	// type's comment: a strict leaf, held across nothing). outbox is the FIFO of
	// batches; events and bytes are what is in it and not yet committed, which
	// is what OutboxRoom reports on; enqueued and drained are the running totals
	// Flush compares, so a Flush waits for a count rather than for an event it
	// would have to identify; waiters are the Flushes parked on them.
	// outboxMaxEvents and outboxMaxBytes are the soft bound — a test lowers them
	// directly, before anything enqueues, because the bound is the log's business
	// and no caller has a reason to choose it.
	outboxMu        sync.Mutex
	outbox          []outboxBatch
	outboxEvents    int
	outboxBytes     int
	outboxMaxEvents int
	outboxMaxBytes  int
	enqueued        uint64
	drained         uint64
	waiters         []*flushWaiter
	drainerStarted  bool
	// outboxWake tells the idle drainer there is a batch (capacity 1: a
	// collapsed pair of wake-ups is a drainer that finds both batches).
	outboxWake chan struct{}
	// outboxCut is closed, once, under outboxMu, by the first phase of Close: it
	// refuses every later Enqueue, makes OutboxRoom false for good, and is what
	// the drainer's blocking primary send escapes on so that it stops *waiting*
	// on the primary without giving up the record.
	outboxCut chan struct{}
	// drainer is the one drainer goroutine, for Close to join. Wait on a
	// WaitGroup nothing ever added to returns at once, which is a log whose
	// drainer never started. The Add is safe against the Wait without a second
	// lock: it happens under outboxMu and only before the cut, and the cut, also
	// under outboxMu, happens before Close waits.
	drainer sync.WaitGroup

	// owners counts subscription owner goroutines, so Close can wait for
	// every one of them; liveOwners is the same count, readable by tests.
	owners     sync.WaitGroup
	liveOwners atomic.Int64

	// Health counters.
	omitted              atomic.Int64
	subsDropped          atomic.Int64
	droppedAtClose       atomic.Int64
	notesDropped         atomic.Int64
	outboxSkippedPrimary atomic.Int64
	outboxErrReplaced    atomic.Int64

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
	// sending runs in a subscription's owner just before it blocks handing the
	// record with seq to Records: the record's charge is already released
	// (handOver) and it is not delivered yet — the record a Rest keeps first.
	sending func(seq uint64)
	// ownerStarts runs first thing in a subscription's owner goroutine, before
	// it delivers anything, with the subscription's kill: a test holds it there
	// until the subscription has ended, standing in for an owner the scheduler
	// had not yet run when its log closed.
	ownerStarts func(kill <-chan struct{})
	// outboxAdmitting runs in the drainer just before it waits for the boundary
	// with a batch of events events.
	outboxAdmitting func(events int)
	// outboxSending runs in the drainer inside the boundary, just before it
	// offers seq to the primary; waiting is false once the cut has been seen,
	// which is the at-close commit — the primary is tried, never waited for.
	outboxSending func(seq uint64, waiting bool)
	// flushParked runs in a Flush that has parked for target, so a test knows
	// the waiter is on the list rather than on its way to it.
	flushParked func(target uint64)
	// outboxDrained runs in Close once the drainer has been joined: everything
	// the outbox held is committed and offered, and no subscription has been
	// ended yet — the one place a test can hold Close while it reads what the
	// at-close commits put in a subscription.
	outboxDrained func()
}

// NewEventLog builds an event log. It starts nothing: the only goroutines a
// log ever has are one owner per subscription, the outbox's drainer once
// something is enqueued, and, with a journal, the journal's writer — and Close
// ends them all.
func NewEventLog(o EventLogOptions) *EventLog {
	inc := o.Incarnation
	if inc == "" {
		inc = NewIncarnation()
	}
	maxRecord := orDefault(o.MaxRecordBytes, defaultMaxRecordBytes)
	if j := o.Journal.MaxRecordBytes(); j > 0 {
		maxRecord = min(maxRecord, j)
	}
	l := &EventLog{
		incarnation: inc,
		primary:     make(chan Event, primaryCap),
		sem:         make(chan struct{}, 1),
		closed:      make(chan struct{}),
		idle:        make(chan struct{}),
		journal:     o.Journal,
		maxRecord:   maxRecord,
		noPrimary:   o.NoPrimary,
		ring: recordRing{
			maxItems: orDefault(o.RingEvents, defaultRingEvents),
			maxBytes: orDefault(o.RingBytes, defaultRingBytes),
		},
		outboxMaxEvents: defaultOutboxEvents,
		outboxMaxBytes:  defaultOutboxBytes,
		outboxWake:      make(chan struct{}, 1),
		outboxCut:       make(chan struct{}),
	}
	if o.Journal != nil {
		// Only when there is one: a nil *journal.Writer in the interface would
		// be a non-nil journalFile, and the file leg asks whether there is a
		// journal by asking whether this is nil.
		l.file = o.Journal
	}
	return l
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

// release leaves the boundary. Every scope that acquires the boundary releases
// it through a defer, so a panic inside it — an observer's, the journal's, a
// codec bug — unwinds with the boundary free. A boundary left held by a panic a
// caller then recovered would poison the log for good: the next publisher, the
// drainer and Close itself would all wait on it forever (plan 021 §3.3, Observe).
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
// Under EventLogOptions.NoPrimary there is no primary send at all, so the only
// way this blocks is waiting for the boundary, and a true return means the
// event is committed — which is what "committed" means everywhere else too.
//
// The event is encoded before the boundary and before the in-flight region,
// on the publisher's goroutine: the value is the publisher's own copy by then
// (cloneTool and its kin), encoding is the expensive part, and it runs the
// caller's own code (an Err's Error method), which must not run anywhere a
// Close is waiting for it.
//
// The boundary is released by a defer from the moment it is acquired, so every
// way out of the section — the abandon paths and a panic from inside a commit —
// leaves it free (release).
func (l *EventLog) Publish(ctx context.Context, done <-chan struct{}, ev Event) bool {
	rec, remote := l.record(ev)
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
	defer l.release()
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
	if !l.noPrimary {
		select {
		case l.primary <- ev:
		case <-done:
			return l.abandonLocked(true)
		case <-cancelled:
			return l.abandonLocked(false)
		case <-l.closed:
			return l.abandonLocked(true)
		}
	}
	rec.Seq = seq
	l.commitLocked(ev, rec, remote)
	return true
}

// abandonLocked gives the event up from inside the boundary: it is dropped for
// everyone and its number was never taken. counted says the escape was on done
// or the cutoff, which the closing diag reports; the count is taken while the
// boundary is still held, so a Close waiting for the boundary reads it. It
// returns false, Publish's answer. The boundary itself is released by the
// caller's defer, which is what keeps one release per acquisition however the
// section ends.
func (l *EventLog) abandonLocked(counted bool) bool {
	if h := l.hooks; h != nil && h.abandoning != nil {
		h.abandoning(true)
	}
	if counted {
		l.droppedAtClose.Add(1)
	}
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
// returns false. Under NoPrimary there is no primary to be full, so the only
// thing it can lose to is the boundary.
func (l *EventLog) TryPublish(ev Event) bool {
	rec, remote := l.record(ev)
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
	defer l.release()
	select {
	case <-l.closed:
		return l.abandonLocked(true)
	default:
	}
	seq := l.next + 1
	ev.Seq = seq
	if !l.noPrimary {
		select {
		case l.primary <- ev:
		default:
			return l.abandonLocked(false)
		}
	}
	rec.Seq = seq
	l.commitLocked(ev, rec, remote)
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
//
// It also hands back the codec's view of ev.Err, for the observer alone
// (Observe): the *RemoteError every decoder of the body builds, set exactly
// when ev.Err is, an omitted record's included. The encoding reads the error
// once — Error() and the classification's chain-walk — and this is where that
// one reading is kept, so nothing reads it again, under the boundary or
// anywhere else; an event the codec never reached (a type too long to record,
// or none) has its error read here instead, still once and still before the
// boundary. It travels beside the record to commitLocked as a value of its
// own — never in the Record, which the ring, every subscription and the
// journal keep — so no reader holds it and nothing retained grows by it.
func (l *EventLog) record(ev Event) (Record, *RemoteError) {
	rec, remote := l.encodeRecord(ev)
	if remote == nil && ev.Err != nil {
		remote = remoteError(toWireError(ev.Err))
	}
	return rec, remote
}

func (l *EventLog) encodeRecord(ev Event) (Record, *RemoteError) {
	rec := Record{At: ev.At, Type: ev.Type}
	if len(ev.Type) > journal.MaxEventTypeBytes {
		rec.Type = journal.OverlongEventType
		rec.Omitted = &Omitted{Reason: journal.OmittedEncodeError, Error: fmt.Sprintf(
			"agent: the event type is %d bytes, over the %d-byte cap", len(ev.Type), journal.MaxEventTypeBytes)}
		return rec, nil
	}
	body, remote, err := encodeEvent(ev)
	switch {
	case err != nil:
		rec.Omitted = &Omitted{Reason: journal.OmittedEncodeError, Error: omittedError(err)}
	case len(body) > l.maxRecord:
		rec.Omitted = &Omitted{Reason: journal.OmittedOversized, Bytes: len(body)}
	default:
		rec.Body = body
	}
	return rec, remote
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
//
// ev is rec's own event, with its Seq, and is what the observer is handed last
// of all — here rather than at the three call sites, so "once per committed
// event, in Seq order, never for an abandoned publish" is a property of the
// commit itself and not of remembering to call it (plan 021 §3.3). So is the
// observer's Err: the copy of ev it is handed carries remote — record's view
// of the error, built with rec — in place of the publisher's error, on every
// path that commits: Publish, TryPublish, the outbox and its at-close commits.
// remote is used for nothing else, and dropped with the call.
func (l *EventLog) commitLocked(ev Event, rec Record, remote *RemoteError) {
	l.next = rec.Seq
	l.committed.Store(rec.Seq)
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
	if l.observer != nil {
		if ev.Err != nil && remote != nil {
			// ev is this call's own copy: the primary already took the
			// publisher's value, and keeps it.
			ev.Err = remote
		}
		l.observer(ev)
	}
}

// outboxBatch is one Enqueue's events, encoded, in the order they were given,
// and their weight against the outbox's soft bound.
type outboxBatch struct {
	evs   []pendingEvent
	bytes int
	// ticket is the receipt EnqueueTicket handed its caller, nil for a plain
	// Enqueue: the drainer writes the batch's first sequence number into it
	// once the whole batch is committed.
	ticket *Ticket
}

// Ticket is the receipt of one EnqueueTicket: once the batch has been
// committed, Seq is the sequence number its first event was given.
//
// It exists for one caller: a session enqueuing the state delta of a settings
// change, whose **revision is that delta's Seq** (plan 021 §3.8). The delta is
// enqueued under the session's own lock, in the section that mutates the
// snapshot, so nothing about it can be read from the call — the number is
// assigned later, by the drainer, on another goroutine — and the engine's
// settings worker needs it to answer Control.Set with a revision a client can
// compare delayed replies against.
//
// The value is written and read through an atomic, so reading it is race-free
// whenever it happens; what a Flush that covers the batch adds is that the
// answer is *there*. Before that, and for a batch the log refused because Close
// had begun, it is 0 — which a client reads as "no revision", never as a
// revision older than every other.
type Ticket struct{ seq atomic.Uint64 }

// Seq is the sequence number the batch's first event was committed with, or 0
// when it has not been committed (yet, or at all). A nil Ticket answers 0, so a
// session that publishes no delta needs no special case at its caller.
func (t *Ticket) Seq() uint64 {
	if t == nil {
		return 0
	}
	return t.seq.Load()
}

// pendingEvent is one enqueued event with the record built for it at enqueue
// time, on the caller's goroutine — where Publish builds one too, and for the
// same two reasons: encoding is the expensive part, and it runs the caller's own
// code, which must not run under the log's own locks.
type pendingEvent struct {
	ev  Event
	rec Record
	// remote is record's view of ev.Err, the outbox's sentinel's (an enqueued
	// event never carries a caller's error), for the observer at commit.
	remote *RemoteError
}

// flushWaiter is one Flush parked until the drainer has committed target events
// in all. done has capacity 1 and is written exactly once, under outboxMu,
// before the waiter leaves the list, so an answer is never lost to a ctx that
// ended in the same instant.
type flushWaiter struct {
	target uint64
	done   chan error
}

// Enqueue appends evs to the log's outbox as one batch and returns at once: it
// waits for nothing — not the boundary, not the primary, not a channel, not I/O
// — and it always accepts. One log-owned goroutine publishes the batch through
// the ordinary commit path, so every enqueued event takes a sequence number and
// reaches the primary, the ring, every subscription and the journal exactly as a
// published one does (plan 021 §3.3).
//
// It is the one thing a caller may do to the log with its own state lock held,
// and that is the point: the engine's e.mu, the ask registry's mu and a
// session's s.mu can each hold across the enqueue of the events that describe
// the mutation they are making, so *state order is event order* with no lock
// held across a blocking send.
//
// **What runs under your lock**, exactly: the events are encoded here, before
// the outbox mutex is taken, and that encoding is this package's own code and
// nothing else. Every field the codec reaches on the way to the wire is a
// string, a number, a bool, a time, or a struct, slice or map of those
// (eventcodec.go's wire structs hold no `any`, no json.RawMessage and no type
// with a MarshalJSON or String of its own), so no method belonging to a caller
// or a provider is ever invoked — with one exception, which is why there is a
// rule about it:
//
//   - **An enqueued event must not carry Err.** Err is the one field whose
//     encoding calls foreign code: Error(), and the chain-walking of the
//     classification (Unwrap, Is, As). Under a state lock that is a loaded gun —
//     an Error method that published, took the same lock, or blocked would wedge
//     its own caller, and one that panicked would defeat the counted drop this
//     promises after Close. So Enqueue never calls anything on it: an event that
//     arrives with Err set has it replaced, in this function's own copy, by an
//     inert sentinel of this package's (errEnqueuedErr), and the substitution is
//     counted in Health.OutboxErrReplaced and in the closing diag. The caller's
//     own value is untouched. Engine- and registry-authored events carry a
//     failure as *text* in their payload for exactly this reason; an event
//     carrying a live provider error belongs on Publish, off the lock, as today.
//   - The events are the caller's own values from here on, as they are for
//     Publish: immutable once enqueued, because the drainer reads them later on
//     another goroutine. Hand over copies (cloneTool and its kin), never
//     something still being written.
//
// Batches are never interleaved: the drainer holds the publishing boundary
// across a whole batch, so one is contiguous in Seq against every other batch
// *and* against a session's own direct Publish. A caller therefore never has to
// reason about half a transaction.
//
// Enqueue does not stamp anything. Each session already stamps At (and
// Replayed) in its own emit before it publishes, and the log will not invent a
// second clock: the events arrive numbered by the log and timed by whoever made
// them — the engine from the session's own clock (Clocked) at admission, plan
// 021 §3.9. An event enqueued with a zero At is recorded with a zero At.
//
// It always accepts, even past the soft bound OutboxRoom reports on, because
// what reaches it is either a mandatory completion — a turn's settlement, an
// ask's ending, a requeue, shutdown — bounded by state rather than by a limit,
// or a command whose caller checked OutboxRoom before it mutated anything. The
// one thing it will not do is accept work after Close has begun: those events
// are counted in droppedAtClose, the same counter as an emit its session gave up
// on its own done, and dropped. They cannot be published — the drainer has been
// told to finish — and Enqueue may neither block nor panic, so counting the drop
// is all that is left; by then the engine has refused the command that would
// have caused one. The cut is checked *before* anything is encoded, so a refused
// batch costs the encoding of nothing.
//
// EnqueueTicket is this with a receipt, for the one caller that has to learn
// what number its event was given.
func (l *EventLog) Enqueue(evs ...Event) { l.enqueue(nil, evs) }

// EnqueueTicket is Enqueue with a receipt: the same append, under the same
// rules, plus a Ticket that resolves to the sequence number the batch's first
// event was committed with (Ticket says what that is for). A caller that does
// not need the number calls Enqueue and allocates nothing.
func (l *EventLog) EnqueueTicket(evs ...Event) *Ticket {
	t := &Ticket{}
	l.enqueue(t, evs)
	return t
}

func (l *EventLog) enqueue(t *Ticket, evs []Event) {
	if len(evs) == 0 {
		return
	}
	if l.outboxIsCut() {
		// Refused before anything is encoded, so a drop after Close reads nothing
		// on the events at all — an Err's Error method included, which is the
		// whole point of checking here and not only under the mutex. The locked
		// recheck below still covers a cut that lands during the encoding.
		l.droppedAtClose.Add(int64(len(evs)))
		return
	}
	b := outboxBatch{evs: make([]pendingEvent, len(evs)), ticket: t}
	for i, ev := range evs {
		if ev.Err != nil {
			// Never read: not Error(), not Unwrap, not Is or As. ev is this
			// loop's own copy, so the caller's event keeps its error.
			ev.Err = errEnqueuedErr
			l.outboxErrReplaced.Add(1)
		}
		rec, remote := l.record(ev)
		b.evs[i] = pendingEvent{ev: ev, rec: rec, remote: remote}
		b.bytes += rec.size()
	}
	l.outboxMu.Lock()
	defer l.outboxMu.Unlock()
	if l.outboxIsCut() {
		l.droppedAtClose.Add(int64(len(evs)))
		return
	}
	l.outbox = append(l.outbox, b)
	l.outboxEvents += len(b.evs)
	l.outboxBytes += b.bytes
	l.enqueued += uint64(len(b.evs))
	if !l.drainerStarted {
		// Lazily, so a log nothing enqueues to has no goroutine at all, and
		// under the same mutex as the cut, so a drainer is never started for
		// work that will not be taken.
		l.drainerStarted = true
		l.drainer.Add(1)
		go l.drain()
	}
	select {
	case l.outboxWake <- struct{}{}:
	default:
	}
}

// OutboxRoom reports whether the outbox is under its soft bound (4096 events or
// 8 MiB of encoded records, the ring's numbers). It is the advisory a
// *rejectable* command consults **before it mutates anything**: a Submit, a
// queue verb, an Answer, a Set, an ask opening. False once Close has begun.
//
// It is advisory in one direction only. Racing past it costs a few events over a
// soft limit, which is why Enqueue accepts them; being refused by it costs a
// command, which is why it is checked before the mutation rather than after. A
// mandatory completion never consults it (Enqueue says why).
func (l *EventLog) OutboxRoom() bool {
	l.outboxMu.Lock()
	defer l.outboxMu.Unlock()
	if l.outboxIsCut() {
		return false
	}
	return l.outboxEvents < l.outboxMaxEvents && l.outboxBytes < l.outboxMaxBytes
}

// Flush returns nil once everything enqueued before the call has been
// committed — numbered, in the ring, offered to every subscription, queued for
// the journal, and on the primary unless the primary refused it at close or
// NoPrimary means there is none. It returns ctx.Err() if ctx ends first,
// ErrFlushGaveUp if done closes first, and ErrLogClosing at once if Close has
// already begun, because from then on the outbox takes no work and the barrier
// can no longer mean what it means.
//
// A Flush already parked when Close begins is answered nil: Close's outbox phase
// commits what is left rather than abandoning it, so a nil return always means
// "your events are in the record" and ErrLogClosing always means "you asked too
// late", never "we lost them". With an empty outbox it returns nil without
// parking anything.
//
// It blocks exactly as a session's own emit blocks today — the drainer ahead of
// it is waiting for the primary's reader — so it may be called only from a
// goroutine that is **not** the primary's reader. A client that must print
// trailing events flushes on a helper goroutine and keeps reading until it
// returns (plan 021 §3.3, A19). A caller for which the barrier is an ordering
// nicety rather than a precondition carries on whatever it returns.
//
// **done is the session's own, exactly as it is on Publish**, and ctx and done
// may both be nil. A session flushing on one of its own goroutines passes it, so
// the barrier ends when the session closes — which is what keeps a Close that
// waits for such a goroutine before it closes the log from waiting for ever:
// with the primary full and its reader gone, only the log's own Close frees the
// drainer, and only that goroutine's return lets Close reach it (native.go's
// prompt and Close are one such pair).
// A caller with no session — the engine's own workers — passes nil, because the
// log outlives nothing there and its Close is what frees them.
func (l *EventLog) Flush(ctx context.Context, done <-chan struct{}) error {
	l.outboxMu.Lock()
	if l.outboxIsCut() {
		l.outboxMu.Unlock()
		return ErrLogClosing
	}
	target := l.enqueued
	if l.drained >= target {
		l.outboxMu.Unlock()
		return nil
	}
	w := &flushWaiter{target: target, done: make(chan error, 1)}
	l.waiters = append(l.waiters, w)
	l.outboxMu.Unlock()
	if h := l.hooks; h != nil && h.flushParked != nil {
		h.flushParked(target)
	}
	var cancelled <-chan struct{}
	if ctx != nil {
		cancelled = ctx.Done()
	}
	select {
	case err := <-w.done:
		return err
	case <-cancelled:
		if answered, err := l.unpark(w); answered {
			return err
		}
		return ctx.Err()
	case <-done:
		if answered, err := l.unpark(w); answered {
			return err
		}
		return ErrFlushGaveUp
	}
}

// FlushSeq is Flush with the sequence number the barrier reached: once Flush
// returns nil, the log's committed head read at that moment. It is the socket
// server's reply barrier (plan 027 §3.6) — a handler that ran a mutating command
// learns the seq S its subscription has to deliver up to before the command's
// reply may follow it on the wire.
//
// The seq is ≥ every seq of every event enqueued before the call, and of every
// Publish that had returned before it: the drainer commits a batch (commitLocked
// stores the head) before it advances the running total a Flush compares
// (commitBatch runs after publishBatch), and a direct Publish commits before it
// returns. It may be higher — whatever else was committed meanwhile — which a
// barrier only ever waits a little longer for.
//
// Its errors are Flush's, with seq 0: ErrLogClosing once Close has begun,
// ErrFlushGaveUp when done closes first, ctx.Err() when ctx ends first. It
// blocks exactly as Flush does, so the same rule holds: never from the
// primary's reader unless another goroutine is reading.
func (l *EventLog) FlushSeq(ctx context.Context, done <-chan struct{}) (uint64, error) {
	if err := l.Flush(ctx, done); err != nil {
		return 0, err
	}
	return l.committed.Load(), nil
}

// unpark takes w off the waiter list. It reports w's answer when the drainer
// answered it in the very instant ctx ended: an answered waiter is no longer
// listed, and its answer is already in its channel, so the truth is preferred to
// the deadline.
func (l *EventLog) unpark(w *flushWaiter) (bool, error) {
	l.outboxMu.Lock()
	defer l.outboxMu.Unlock()
	if i := slices.Index(l.waiters, w); i >= 0 {
		l.waiters = slices.Delete(l.waiters, i, i+1)
		return false, nil
	}
	select {
	case err := <-w.done:
		return true, err
	default:
		return false, nil
	}
}

// outboxIsCut reports whether Close has cut the outbox. It never blocks and the
// cut is permanent, so a true answer is final and a false one is only as fresh as
// the caller's own critical section. Under outboxMu — where Enqueue appends,
// takeBatch empties and Flush parks — it is exact, because Close closes the
// channel under that mutex too: no event is ever appended after the cut, and none
// is ever left behind by a drainer that has already stopped. Enqueue also reads
// it once *without* the mutex, purely to refuse a batch before encoding it, and
// rechecks under the mutex before it appends.
func (l *EventLog) outboxIsCut() bool {
	select {
	case <-l.outboxCut:
		return true
	default:
		return false
	}
}

// drain is the outbox's one goroutine: batches in the order they were enqueued,
// each published whole inside the boundary, until the outbox is both cut and
// empty. It never abandons an event — the most Close can make it do is stop
// *waiting* for the primary (sendOutbox) — so its own exit is the proof that
// everything accepted was recorded, which is what Close joins it for.
//
// It publishes with no session done channel and does not consult the admission
// cutoff: only the log's own cut changes what it does. That is deliberate. A
// session's Close closes its done first, before the agent's last words are
// written and long before it closes the log (live.go's Close), and the endings
// the ask registry enqueues from inside that teardown must reach the record all
// the same — a done the drainer honored would drop exactly them.
func (l *EventLog) drain() {
	defer func() {
		// The waiters first, then the join: a Close that has joined the drainer
		// knows every Flush has its answer.
		l.endWaiters()
		l.drainer.Done()
	}()
	// stopped is the at-close mode, latched: once the cut has been seen the
	// primary is only ever tried, so no later event of this or any batch can put
	// the drainer back into a wait.
	stopped := false
	for {
		b, taken, done := l.takeBatch()
		switch {
		case taken:
			l.publishBatch(b, &stopped)
			l.commitBatch(b)
		case done:
			return
		default:
			select {
			case <-l.outboxWake:
			case <-l.outboxCut:
			}
		}
	}
}

// takeBatch removes the oldest batch. done is set when there is none and the
// outbox has been cut, which is the drainer's only way out: nothing can be
// enqueued after the cut, so an empty outbox under it is empty for good.
func (l *EventLog) takeBatch() (b outboxBatch, taken, done bool) {
	l.outboxMu.Lock()
	defer l.outboxMu.Unlock()
	if len(l.outbox) == 0 {
		return outboxBatch{}, false, l.outboxIsCut()
	}
	b = l.outbox[0]
	l.outbox = slices.Delete(l.outbox, 0, 1)
	return b, true, false
}

// publishBatch commits a whole batch inside one hold of the boundary, so the
// batch is contiguous in Seq (see the type's comment). It waits for the boundary
// with no escape, which is bounded because every blocking point inside the
// boundary selects on the cutoff: a direct publisher wedged on a full primary is
// released by Close, and Close takes the boundary itself only after joining
// this goroutine. The release is deferred, so a panic from inside a commit — an
// observer's — unwinds the drainer with the boundary free rather than wedging a
// Close that is waiting for it. The batch's accounting is the caller's
// (commitBatch), outside the boundary and outside that unwinding.
func (l *EventLog) publishBatch(b outboxBatch, stopped *bool) {
	if h := l.hooks; h != nil && h.outboxAdmitting != nil {
		h.outboxAdmitting(len(b.evs))
	}
	l.sem <- struct{}{}
	defer l.release()
	first := l.next + 1
	for i := range b.evs {
		p := b.evs[i]
		b.evs[i] = pendingEvent{} // published: the batch no longer holds it
		seq := l.next + 1
		p.ev.Seq, p.rec.Seq = seq, seq
		l.sendOutbox(p.ev, seq, stopped)
		l.commitLocked(p.ev, p.rec, p.remote)
	}
	if b.ticket != nil {
		// The batch's first number, answered only now that the whole batch is
		// committed (Ticket.Seq) — never while the drainer waits for the
		// primary above, however long that is, since a number read then would
		// name an event nothing holds yet. Still inside the boundary and ahead
		// of commitBatch, so a Flush covering the batch always finds it.
		b.ticket.seq.Store(first)
	}
}

// sendOutbox offers ev to the primary from inside the boundary: blocking, as a
// publisher does, until Close cuts the outbox — and from then on a try-send,
// counted in outboxSkippedPrimary when the primary will not take it. That is the
// whole of what "a stopped reader can neither block shutdown nor cost the
// record" comes to: the event is committed either way, only unread. Under
// NoPrimary there is nothing to offer it to.
func (l *EventLog) sendOutbox(ev Event, seq uint64, stopped *bool) {
	if l.noPrimary {
		return
	}
	if h := l.hooks; h != nil && h.outboxSending != nil {
		h.outboxSending(seq, !*stopped)
	}
	if !*stopped {
		select {
		case l.primary <- ev:
			return
		case <-l.outboxCut:
			*stopped = true
		}
	}
	select {
	case l.primary <- ev:
	default:
		l.outboxSkippedPrimary.Add(1)
	}
}

// commitBatch accounts for a batch the boundary has released: its weight leaves
// the soft bound, the running total advances, and every Flush waiting for a
// count this reaches is answered. It is the one place drained moves, and it runs
// outside the boundary, so outboxMu keeps its independence from it.
func (l *EventLog) commitBatch(b outboxBatch) {
	l.outboxMu.Lock()
	defer l.outboxMu.Unlock()
	l.outboxEvents -= len(b.evs)
	l.outboxBytes -= b.bytes
	l.drained += uint64(len(b.evs))
	kept := l.waiters[:0]
	for _, w := range l.waiters {
		if w.target <= l.drained {
			w.done <- nil
			continue
		}
		kept = append(kept, w)
	}
	clear(l.waiters[len(kept):])
	l.waiters = kept
}

// endWaiters answers every Flush still parked as the drainer exits: nil for a
// target it committed, which is every target it was ever given, and
// ErrLogClosing for one it did not. Nothing today produces the second — the
// at-close path commits rather than abandons — and it is here so that a change
// which made it possible would fail a waiter rather than strand it.
func (l *EventLog) endWaiters() {
	l.outboxMu.Lock()
	defer l.outboxMu.Unlock()
	for _, w := range l.waiters {
		if w.target <= l.drained {
			w.done <- nil
			continue
		}
		w.done <- ErrLogClosing
	}
	clear(l.waiters)
	l.waiters = nil
}

// cutOutbox is Close's first phase: no later Enqueue is accepted, OutboxRoom is
// false from here on, and the drainer's blocking primary send has its escape.
// Closing the channel under outboxMu is what makes the cut one step for
// everything that reads it under the same mutex (outboxIsCut).
func (l *EventLog) cutOutbox() {
	l.outboxMu.Lock()
	defer l.outboxMu.Unlock()
	close(l.outboxCut)
}

// Observe installs the log's one observer: a function the commit of every event
// calls, inside the publishing boundary, after the primary send, once per
// committed event and in Seq order, with Seq set — and never for a publish that
// was abandoned or a TryPublish that found no room, since only a commit calls
// it. Events the at-close path commits reach it too. A second call is
// ErrObserverSet; a nil one is refused rather than read as "unset"; on a closed
// log it is ErrClosed.
//
// It is set once, before the session publishes anything, and it runs under the
// boundary's rules: it must not block, must not take anything but a leaf lock of
// its own, and must not call the log or the session — it is on the path of every
// event, and a publisher is holding the ordering boundary while it runs.
//
// A panic in it is a bug in craze's own code and the log does not recover it.
// Recovering at the top of the drainer would abandon the accepted events behind
// the one that panicked, and swallowing it in Publish would hide the bug in the
// one place a test would have caught it; on the drainer's goroutine it takes the
// process down, as any unrecovered goroutine panic does. What the log does
// guarantee is that the panic leaves it *usable*: the boundary is released by a
// defer in every scope that holds it (release), so a caller that recovers a panic
// from its own Publish finds a log that still publishes and a Close that still
// returns, rather than one wedged on a boundary nobody will ever give back.
//
// What it is for is deriving state from the sequence in the sequence's own order: the
// engine reads foreign-turn and replay brackets and title deltas from it and
// wakes its workers through one-slot channels. It is the seed of S1c's
// transcript fold, which is the same hook keeping a whole model rather than a
// handful of flags (plan 021 §3.3, SD-19).
//
// The event it is handed is the publisher's own value, the same one the primary
// took — read-only, exactly as the primary's reader must treat it — with one
// field changed: **Err, when set, is a *RemoteError**, the codec's view of the
// publisher's error (its message, class and code) that the encoding took before
// the boundary, and exactly what every subscriber decodes from the record. So
// an observer never runs an error's own code (its Error, Unwrap, Is or As)
// under the boundary, and a model folded here and one a client folds from a
// subscription hold the same error. The message is carried as the JSON carries
// it (remoteError: an invalid UTF-8 byte is U+FFFD), so the two agree byte for
// byte. An event whose record was omitted — a body over MaxRecordBytes — still
// has its RemoteError, since the encoding happened. An enqueued event carries
// the outbox's inert sentinel in place of any error (Enqueue), so its observer
// sees that sentinel's RemoteError, never a caller's error. The primary still
// gets the publisher's own error value. Cost is one nil check per commit when
// unset (R5, V7), and nothing more for an event with no Err.
func (l *EventLog) Observe(fn func(Event)) error {
	if fn == nil {
		return errNilObserver
	}
	select {
	case l.sem <- struct{}{}:
	case <-l.closed:
		return ErrClosed
	}
	defer l.release()
	select {
	case <-l.closed:
		return ErrClosed
	default:
	}
	if l.observer != nil {
		return ErrObserverSet
	}
	l.observer = fn
	return nil
}

// Subscribe opens a subscription: a replay of every record after o.After,
// then every record published from now on, on its own channel, in order and
// with nothing skipped. A cursor that cannot be served whole is refused here,
// synchronously, as ErrCursorUnresolvable, and nothing is registered.
//
// It waits for the boundary, like a publisher: while the primary is wedged
// it waits, closing the log releases it with ErrClosed, and o.Ctx, when set,
// releases it with Ctx.Err() — the one place Ctx is consulted, so a caller
// that must not wait on a primary only its own goroutine can drain has a way
// out (plan 024 §3.6). Either way nothing is registered and no owner is
// started. The cutoff is taken inside the boundary: the last seq N, and a
// pin on every ring record in (After.Seq, N] — references to immutable
// records, which the ring may evict afterwards without touching the pinned
// copies — and live delivery begins at N+1. N is the subscription's Cutoff.
func (l *EventLog) Subscribe(o SubscribeOptions) (*Subscription, error) {
	o.MaxItems = orDefault(o.MaxItems, defaultSubscribeItems)
	o.MaxBytes = orDefault(o.MaxBytes, defaultSubscribeBytes)
	var cancelled <-chan struct{}
	if o.Ctx != nil {
		cancelled = o.Ctx.Done()
	}
	if h := l.hooks; h != nil && h.admitting != nil {
		h.admitting(admitSubscribe)
	}
	select {
	case l.sem <- struct{}{}:
	case <-l.closed:
		return nil, ErrClosed
	case <-cancelled:
		return nil, o.Ctx.Err()
	}
	defer l.release()
	// A select picks at random among ready cases, so the boundary can be won
	// after the log closed or the caller gave up; either way nothing is
	// registered, and a Ctx that had already ended is refused every time
	// rather than one time in two.
	select {
	case <-l.closed:
		return nil, ErrClosed
	case <-cancelled:
		return nil, o.Ctx.Err()
	default:
	}
	n := l.next
	prev := n
	var head *headRange
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
			var err error
			if head, err = l.headFromFile(a.Seq+1, oldest-1); err != nil {
				return nil, err
			}
		}
		pinned, pinnedBytes = l.ring.copyOut(skip, count), bytes
	}
	s := l.newSubscription(o.MaxItems, o.MaxBytes, pinnedBytes)
	s.cutoff = n
	l.sweepLocked()
	l.subs = append(l.subs, s)
	l.startOwner(s, head, pinned, prev)
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

// startOwner starts s's owner goroutine, which delivers head (the journal
// file's part of the replay), then pinned (the ring's, oldest first) after
// prev, and then the live buffer. Subscribe calls it inside the boundary, so
// Close, which acquires the boundary before it waits, always finds the owner
// counted.
func (l *EventLog) startOwner(s *Subscription, head *headRange, pinned []Record, prev uint64) {
	l.owners.Add(1)
	l.liveOwners.Add(1)
	go s.run(head, pinned, prev)
}

// headFromFile is where a resume whose head, [from, to], has left the ring
// gets it: the journal's own file. What happens here, inside the boundary and
// at the same instant as the pin, is only the decision — is there a journal,
// and does a gap it has already recorded cross the range — and the capture of
// the complete-record boundary, so the pin and the file's position describe
// one instant. The reading itself is the owner's (headRange.serve), outside
// the boundary, because it is I/O.
func (l *EventLog) headFromFile(from, to uint64) (*headRange, error) {
	if l.file == nil {
		return nil, ErrCursorUnresolvable{Reason: CursorNoJournal}
	}
	h := l.file.Health()
	if h.State == journal.StateOff {
		return nil, ErrCursorUnresolvable{Reason: CursorNoJournal}
	}
	if h.Overlaps(from, to) {
		// Recorded missing: those seqs were dropped or lost to a failure, and
		// no file will ever hold them. Refuse without reading a byte.
		return nil, ErrCursorUnresolvable{Reason: CursorJournalGap}
	}
	flushed, _ := l.file.Flushed()
	return &headRange{file: l.file, from: from, to: to, flushed: flushed}, nil
}

// headRange is the head of a resume the ring no longer holds, [from, to], and
// what the cutoff learned about serving it: the journal to read it from, and
// the complete-record boundary as it stood at the cutoff. The records are
// streamed from the file one at a time, so the range costs the subscription
// nothing against its MaxBytes — only the pinned ring records count (plan 020
// §3.2).
type headRange struct {
	file     journalFile
	from, to uint64
	flushed  uint64 // the journal's boundary at the cutoff
}

// serve delivers [from, to] from the journal file, before the pinned records
// and before anything live. The whole range is delivered or the subscription
// fails: ReadRange refuses a range with a seq missing inside it rather than
// skipping one, and send refuses a record that does not follow the last. The
// owner calls it, and it is the only I/O any of this does.
//
// A subscription that fails here may already have delivered a prefix of its
// range, and that is the resolution of §3.2's two rules where they meet — the
// file range is "streamed, not held", so it cannot be buffered until it is
// known to be whole, and yet "the whole range is covered or the subscription
// fails". ReadRange calls back per record as it parses, so a malformed line, a
// line over the reader's limit or a seq missing late in the range is found
// only after the records before it have gone out. The terminal error is what
// makes the whole subscription void: a consumer must discard what it received
// rather than read the prefix as a partial transcript, because a prefix is
// never what a subscription promised. Nothing is truncated silently — the
// failure is always reported, as the subscription's Err and its closed
// Records channel, and a subscription that ends is never resumable from what
// it delivered.
func (h *headRange) serve(s *Subscription, prev *uint64) error {
	if h.flushed < h.to {
		// The writer had not written the range's end at the cutoff. Ask for a
		// flush and wait for it rather than read a file that cannot answer
		// yet: a --continue replay is a burst of thousands of small events,
		// enough to evict from the ring what is still in the writer's buffer.
		if err := h.await(s); err != nil {
			return err
		}
	}
	// A record is handed straight to send, which is where the contiguity
	// assertion and the escape on the subscription's end live. Its error is
	// kept apart from the journal's: it ends the subscription as it is, and is
	// not a reason the cursor was unresolvable.
	var sendErr error
	err := h.file.ReadRange(h.from, h.to, func(r journal.Record) error {
		sendErr = s.send(recordFromJournal(r), prev)
		return sendErr
	})
	switch {
	case sendErr != nil:
		return sendErr
	case err != nil:
		return h.unresolvable(err)
	}
	return nil
}

// await asks the writer to flush and waits until the file reaches the range's
// end, for at most journalWaitBound and no longer than the subscription
// itself: WaitFlushed returns as soon as the writer fails or finishes, so the
// bound is only ever reached by a writer that is genuinely stuck inside a
// write. The wait ends at once when the subscription does, so a stalled
// journal never holds up the log's Close, which waits for every owner.
func (h *headRange) await(s *Subscription) error {
	ctx, cancel := context.WithTimeout(context.Background(), journalWaitBound)
	defer cancel()
	ended := make(chan struct{})
	defer close(ended)
	go func() {
		select {
		case <-s.kill:
			cancel()
		case <-ended:
		}
	}()
	err := h.file.WaitFlushed(ctx, h.to)
	if cause := s.terminal(); cause != nil {
		return cause // the subscription ended under the wait; that is why.
	}
	if err != nil {
		return h.unresolvable(err)
	}
	return nil
}

// unresolvable is the cursor error for what the journal said while the file
// leg was serving the head. The mapping, in one place (plan 020 §3.2):
//
//	no journal at all, or a nil writer          no_journal
//	a gap line, a seq the file skips, or a
//	  gap recorded while we waited              journal_gap
//	the file never reached the range: still
//	  behind, the wait's bound ran out, or
//	  the writer failed or finished first       journal_behind
//	anything else: a malformed file, a line
//	  over the reader's limit, an I/O error     evicted
//
// evicted reads true for that last group and only there: the records are gone
// from the ring, and the file that held them cannot be read, so the range is
// lost rather than late. Whichever it is, the subscription fails and no part
// of the range is delivered as if it were whole.
func (h *headRange) unresolvable(err error) error {
	reason := CursorEvicted
	switch {
	case errors.Is(err, journal.ErrNoJournal):
		reason = CursorNoJournal
	case errors.Is(err, journal.ErrGap):
		reason = CursorJournalGap
	case errors.Is(err, journal.ErrBehind), errors.Is(err, journal.ErrFailed),
		errors.Is(err, journal.ErrClosed), errors.Is(err, context.DeadlineExceeded):
		reason = CursorJournalBehind
		// A writer that failed or dropped entries under the wait records what
		// it lost: that is a gap, not lateness, and it is permanent.
		if h.file.Health().Overlaps(h.from, h.to) {
			reason = CursorJournalGap
		}
	}
	return ErrCursorUnresolvable{Reason: reason, Err: err}
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

// openAttempt is one prompt attempt the journal is still following: the id its
// prompt note carries, and when that note was written, which is what a
// synthesized ending's duration is measured from.
type openAttempt struct {
	id    string
	start time.Time
}

// promptAttempt is what a wrapped prompt or interjection holds between its
// prompt note and its prompt_end. Its zero value journals nothing, which is
// what every session without a journal gets.
type promptAttempt struct {
	log     *EventLog
	id      string
	started time.Time
}

// wrapPrompt is the journal's half of Begin (plan 020 §3.5): the prompt note
// now, on the caller's goroutine, and the prompt_end once the continuation
// returns — with the Result and error it actually returned, so that every way
// a prompt can end is recorded, refusals and withdrawals included. A log with
// no journal hands the continuation straight back, so an unjournaled session
// pays nothing for this.
//
// The caller must not hold its session lock: no note is ever written under it.
func (l *EventLog) wrapPrompt(kind journal.PromptKind, text string, run func(context.Context) (Result, error)) func(context.Context) (Result, error) {
	a := l.beginAttempt(kind, text)
	if a.log == nil {
		return run
	}
	return func(ctx context.Context) (Result, error) {
		res, err := run(ctx)
		a.end(res.StopReason, err)
		return res, err
	}
}

// beginAttempt writes text's prompt note and registers the attempt as open. It
// never blocks, and is refused after the cutoff like any other note.
func (l *EventLog) beginAttempt(kind journal.PromptKind, text string) promptAttempt {
	if l.journal == nil {
		return promptAttempt{}
	}
	l.noteMu.RLock()
	defer l.noteMu.RUnlock()
	if l.notesCut {
		// Close cut the notes off between the prompt's claim and this
		// registration, so this attempt has neither a prompt note nor a
		// prompt_end: Close's endOpenAttemptsLocked had nothing to end,
		// because nothing was registered yet, and the zero attempt returned
		// here writes nothing later either. Accepted (plan 020 §3.5): the
		// drop is counted in the log's health (EventLogHealth.NotesDropped)
		// like any other note refused after the cutoff, and it happens only
		// when the session is being closed underneath the prompt that claimed
		// it — the same window a session or start note is lost in. Registering
		// before the cutoff check would be worse: an attempt could then be
		// registered after Close ended the open ones, and never be ended at
		// all.
		l.notesDropped.Add(1)
		return promptAttempt{}
	}
	now := time.Now()
	l.attemptsMu.Lock()
	l.attemptSeq++
	id := fmt.Sprintf("%s-%d", kind, l.attemptSeq)
	l.attempts = append(l.attempts, openAttempt{id: id, start: now})
	l.attemptsMu.Unlock()
	l.journal.Note(journal.PromptNote{Attempt: id, Kind: kind, Text: text})
	return promptAttempt{log: l, id: id, started: now}
}

// end writes the attempt's prompt_end: the stop reason of a turn that
// finished, or the class and message of whatever ended it instead
// (promptErrClass). Only the first caller writes one — a continuation run
// twice reports the run that happened, and an attempt Close has already ended
// is never reported twice.
func (a promptAttempt) end(stopReason string, err error) {
	if a.log == nil {
		return
	}
	n := journal.PromptEndNote{Attempt: a.id, StopReason: stopReason, Duration: time.Since(a.started)}
	if err != nil {
		n.ErrClass, n.ErrMessage = promptErrClass(err), err.Error()
	}
	a.log.endAttempt(n)
}

// endAttempt queues n for an attempt that is still open, and closes it.
func (l *EventLog) endAttempt(n journal.PromptEndNote) {
	l.noteMu.RLock()
	defer l.noteMu.RUnlock()
	if l.notesCut {
		// Close ended every attempt still open before it cut notes off, so
		// this one already has its ending.
		l.notesDropped.Add(1)
		return
	}
	if !l.takeAttempt(n.Attempt) {
		return
	}
	l.journal.Note(n)
}

// takeAttempt unregisters id and reports whether it was still open. The list
// holds the attempts of one session — one prompt and the interjections sent
// into it — so a scan is cheaper than a map.
func (l *EventLog) takeAttempt(id string) bool {
	l.attemptsMu.Lock()
	defer l.attemptsMu.Unlock()
	for i := range l.attempts {
		if l.attempts[i].id == id {
			l.attempts = slices.Delete(l.attempts, i, i+1)
			return true
		}
	}
	return false
}

// endOpenAttemptsLocked writes prompt_end{errClass: closed} for every attempt
// still open, oldest first. Close calls it inside the boundary, under noteMu
// and before the closing diag, so a prompt whose continuation never ran — and
// one whose turn Close is ending underneath, which the live session does not
// wait for — has an ending in the file all the same. It publishes nothing and
// never blocks.
func (l *EventLog) endOpenAttemptsLocked(now time.Time) {
	l.attemptsMu.Lock()
	open := l.attempts
	l.attempts = nil
	l.attemptsMu.Unlock()
	for _, a := range open {
		l.journal.Note(journal.PromptEndNote{Attempt: a.id, ErrClass: promptEndClosed, Duration: now.Sub(a.start)})
	}
}

// Close shuts the log (plan 020 §3.5, plan 021 §3.3), in phases:
//
//  1. The outbox is cut (cutOutbox). OutboxRoom is false from here on, so every
//     rejectable command above the seam refuses before it mutates anything, and
//     an Enqueue arriving now is counted in droppedAtClose and dropped — it may
//     neither block nor panic, and by then the engine has refused the command
//     that would have caused one.
//  2. The admission cutoff, as before: every later Publish returns false and
//     every later Note is counted and dropped. It comes *before* the drainer is
//     joined, deliberately, because it is the one thing that frees a direct
//     publisher blocked on a full primary while holding the boundary — which is
//     exactly what the drainer may be waiting behind. The outbox is made immune
//     to this cutoff rather than sequenced ahead of it: the drainer's commits do
//     not consult closed, and it is not in the in-flight region, so nothing here
//     can turn an accepted enqueue into a dropped one.
//  3. The drainer is joined. What was left in the outbox is committed, not
//     abandoned: the ring, every subscription and the journal get every event,
//     with a **non-blocking** primary send from the cut on. So a stopped reader
//     — frame teardown stops the program before closing the session, and craze
//     prompt's deferred Close runs on its own reader — can neither block
//     shutdown nor cost the record. Events the primary would not take are
//     counted in outboxSkippedPrimary, not droppedAtClose: they are recorded,
//     only unread. Every Flush still parked is answered first (endWaiters).
//  4. The rest, unchanged in meaning: the publishes in flight at the cutoff
//     waited for and counted; a prompt_end for every attempt still open; the
//     closing diag, counting the publishes abandoned on done or the cutoff —
//     every one that was in flight when the cutoff came included, every emit its
//     session dropped on its own done fast path (Abandoned), and every Enqueue
//     refused in phase 1 — beside outboxSkippedPrimary; every subscription ended
//     with ErrClosed and its owner goroutine waited for; and the journal closed,
//     waiting at most its own bound (500 ms) and at most until ctx ends.
//
// Ending a subscription does not wait for its reader, so what it had accepted
// and not yet delivered — the session's closing records among them, the
// synthetic `closing` ending and the asks' endings this very Close committed —
// never reaches its Records. It is not dropped either: the owner keeps it, in
// order, and the subscription's Rest hands it back once Records has closed
// (plan 027 §3.7), which is how a forwarder delivers a session's last words.
//
// Every phase is bounded with nobody reading the primary, and none of them
// leaves a goroutine behind. It is idempotent: a second call returns once the
// first has finished. It is safe on a log never published to, and on one whose
// drainer never started.
func (l *EventLog) Close(ctx context.Context) {
	l.closeOnce.Do(func() {
		l.cutOutbox()
		close(l.closed)
		l.drainer.Wait()
		if h := l.hooks; h != nil && h.outboxDrained != nil {
			h.outboxDrained()
		}
		l.awaitInFlight()
		subs := l.cutoffLocked()
		for _, s := range subs {
			s.closedByLog()
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

// cutoffLocked is Close's last act inside the boundary: it unregisters every
// subscription and hands them back for their owner to be ended, cuts the notes
// off, ends the prompt attempts still open, and writes the closing diag. It is a
// function of its own so that the boundary and noteMu are both released by
// defers — a panic in a note or an attempt leaves neither held — while keeping
// the release exactly where it was, before the subscriptions are terminated and
// their owners waited for.
//
// The closing diag's counters are a snapshot taken here. Health keeps the
// running totals, so an Enqueue or an emit that arrives after this line is
// counted in Health and is not in the file's diag (Health, DroppedAtClose).
func (l *EventLog) cutoffLocked() []*Subscription {
	// Serialize with a Subscribe mid-registration. No publisher holds the
	// boundary now, and Subscribe waits for nothing inside it.
	l.sem <- struct{}{}
	defer l.release()
	subs := l.subs
	l.subs = nil
	l.noteMu.Lock()
	defer l.noteMu.Unlock()
	l.notesCut = true
	// Every prompt attempt still open gets its prompt_end{errClass: closed}
	// here, before closing and under the same cutoff.
	l.endOpenAttemptsLocked(time.Now())
	if l.journal != nil {
		l.journal.Note(journal.DiagNote{Kind: journal.DiagClosing, Fields: map[string]any{
			"droppedAtClose":       l.droppedAtClose.Load(),
			"outboxSkippedPrimary": l.outboxSkippedPrimary.Load(),
			"outboxErrReplaced":    l.outboxErrReplaced.Load(),
		}})
	}
	return subs
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
	// DroppedAtClose counts publishes abandoned on done or the cutoff, emits
	// their session dropped on its own done fast path (Abandoned), and events
	// handed to Enqueue after Close cut the outbox. It is a running total, so it
	// can exceed the number the journal's closing diag recorded: that one is a
	// snapshot taken while Close held the boundary, and an emit or an Enqueue
	// that arrives after it is counted here and nowhere else (cutoffLocked).
	DroppedAtClose int
	// NotesDropped counts notes refused after Close.
	NotesDropped int
	// OutboxSkippedPrimary counts enqueued events Close committed with a
	// non-blocking primary send the primary did not take: recorded everywhere
	// else, and read by nobody (plan 021 §3.3). It is not a drop.
	OutboxSkippedPrimary int
	// OutboxErrReplaced counts enqueued events that carried Err, which Enqueue
	// replaced with an inert sentinel rather than call anything on it. Every one
	// is a bug in the enqueuing code — an enqueued event carries its failure as
	// text (Enqueue) — and the record says so in place of the message.
	OutboxErrReplaced int
	// Journal is the journal's health; StateOff when there is none.
	Journal journal.Health
}

// Health is the log's health. It never takes the boundary.
func (l *EventLog) Health() EventLogHealth {
	return EventLogHealth{
		Omitted:              int(l.omitted.Load()),
		SubscribersDropped:   int(l.subsDropped.Load()),
		DroppedAtClose:       int(l.droppedAtClose.Load()),
		NotesDropped:         int(l.notesDropped.Load()),
		OutboxSkippedPrimary: int(l.outboxSkippedPrimary.Load()),
		OutboxErrReplaced:    int(l.outboxErrReplaced.Load()),
		Journal:              l.journal.Health(),
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
//
// The end never waits for the reader, so a subscription usually ends with
// records it had accepted and not delivered. They are discarded with it —
// except when its log's Close is what ended it: then they are the session's
// closing records, which a client is owed, and the owner keeps them for Rest
// (plan 027 §3.7).
type Subscription struct {
	log      *EventLog
	hooks    *logHooks // the log's test seams when it was opened; nil in production
	out      chan Record
	kill     chan struct{} // closed, once, with cause set: the subscription is over
	notify   chan struct{} // capacity 1: the live buffer has something new
	maxItems int
	maxBytes int
	// cutoff is the head Subscribe read inside the boundary (Cutoff). It is set
	// before the owner starts and never written again, so it needs no lock.
	cutoff uint64

	// mu guards the rest. It is a leaf: publishers take it inside the
	// boundary, and nothing holding it waits on anything.
	mu        sync.Mutex
	live      []Record // published, not yet taken by the owner
	liveItems int      // live records not yet handed over: buffered, or taken by the owner and not yet being sent
	bytes     int      // their bytes, plus the pinned replay's not yet being sent
	cause     error    // the first terminal cause
	err       error    // cause, published once the owner has stopped
	// byLog says the first terminal cause was the log's Close (closedByLog),
	// which is the one ending Rest answers for. A detach ends with the same
	// ErrClosed, so the cause alone cannot say which it was.
	byLog bool
	// rest and restErr are Rest's answer, recorded by finish once the owner has
	// stopped, for a subscription ended by its log's Close: what it had
	// accepted and not delivered, in order, or why no whole tail can be given.
	rest    []Record
	restErr error
}

// Records is the subscription's stream. It is closed when the subscription
// ends, and Err then says why — and Rest, after the log's Close, what it
// accepted and never delivered.
func (s *Subscription) Records() <-chan Record { return s.out }

// Err is why the subscription ended: ErrClosed, ErrSlowConsumer, or an
// ErrCursorUnresolvable from its replay. It is stored before Records is
// closed and never changes after; before that it is nil. ErrClosed is the
// answer both for a detach (Close) and for the log's own Close: Rest is what
// tells them apart.
func (s *Subscription) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// Cutoff is the log's head when the subscription registered: the last seq
// committed then, read inside the boundary with the pin (Subscribe), 0 on a log
// that had committed nothing. Live delivery begins after it, so the record whose
// Seq equals the cutoff is the end of the catch-up — the replay, for a cursor
// subscription, runs to it — and a cutoff equal to the start position (the
// cursor's Seq, or the cutoff itself for a live-only subscription) means there
// was nothing to replay. It is the socket forwarder's `synchronized` point
// (plan 027 §3.4). It never changes, and reading it takes no lock.
func (s *Subscription) Cutoff() uint64 { return s.cutoff }

// Rest is what the subscription had accepted and not yet delivered when its
// log's Close ended it (plan 027 §3.7): exactly the undelivered tail, in order —
// contiguous after the last record Records delivered, or after the start
// position (the cursor, or the cutoff for a live-only subscription) when it
// delivered nothing. It is how a forwarder delivers a session's closing records,
// the ones the log committed while it closed and ended the subscription before
// any reader could take them.
//
// It answers from the subscription's own retained undelivered state, never from
// the ring: the ring may have evicted records a subscription with a larger
// budget still holds, and a journal-backed replay's head predates the ring
// altogether. For the same reason a subscription whose journal leg was
// outstanding at the close — waiting for the writer, part-way through the file,
// or not yet begun — is ErrRestUnavailable: the rest of its head is in a file it
// no longer reads, and a suffix is never presented as the whole tail. A tail
// that is not contiguous is a bug, and is an error rather than a partial answer.
//
// It is meaningful once Records has closed: the tail is recorded before the
// close, as Err is, so a reader that has seen the channel closed reads the final
// answer. Before that, and for a subscription that ended any other way — a
// detach, ErrSlowConsumer, a replay that could not be served, a hole — it is
// ErrNoRest. An empty tail is nil with a nil error. It may be called more than
// once and from any goroutine, and hands out a copy each time, each record
// detached as Records hands them out.
func (s *Subscription) Rest() ([]Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.err == nil || !s.byLog:
		return nil, ErrNoRest
	case s.restErr != nil:
		return nil, s.restErr
	case len(s.rest) == 0:
		return nil, nil
	}
	out := make([]Record, len(s.rest))
	for i, r := range s.rest {
		out[i] = r.detached()
	}
	return out, nil
}

// Close ends the subscription with ErrClosed: a detach. It only marks it and
// signals the owner, never taking the log's boundary, so it cannot block behind
// a wedged primary. It is idempotent, and does nothing to a subscription that
// already ended. What the subscription had not delivered is discarded — its
// reader asked for nothing more — so Rest is ErrNoRest for it.
func (s *Subscription) Close() { s.terminate(ErrClosed) }

// terminate ends the subscription with cause unless it has already ended.
func (s *Subscription) terminate(cause error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.endLocked(cause)
}

// closedByLog is the log's Close ending the subscription: ErrClosed, as a
// detach, and marked as ended by the log's close when — and only when — that is
// its first terminal cause, which is what leaves the owner's undelivered tail
// for Rest.
func (s *Subscription) closedByLog() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cause == nil {
		s.byLog = true
	}
	s.endLocked(ErrClosed)
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
func (s *Subscription) run(head *headRange, pinned []Record, prev uint64) {
	defer s.log.ownerDone()
	if h := s.hooks; h != nil && h.ownerStarts != nil {
		h.ownerStarts(s.kill)
	}
	t := ownerTail{prev: prev}
	err := s.pump(head, pinned, &t)
	s.finish(err, &t)
}

// ownerTail is what the owner holds of its stream outside the live buffer, and
// where it stands: what Rest is built from when the log's Close ends the
// subscription (plan 027 §3.7). Only the owner's goroutine touches it — pump
// writes it and finish reads it, one after the other — so it needs no lock.
type ownerTail struct {
	// prev is the seq the reader has: the last record delivered, or the start
	// position — the cursor, or the cutoff for a live-only subscription.
	prev uint64
	// sending is the record the owner was handing to Records when it stopped,
	// if hasSending: taken from the pin or the batch, and neither delivered nor
	// in the live buffer. Its charge against the budget was already released
	// (handOver), so it is accounted for here and nowhere else.
	sending    Record
	hasSending bool
	// local is the unsent remainder of what the owner had in hand: its ring pin,
	// or its live batch. Only one of the two can be non-empty, because the owner
	// is in one phase at a time — it takes its first live batch only once the
	// pin is spent — so one field holds whichever it was.
	local []Record
	// journal says the owner stopped with the journal leg of its replay
	// outstanding (headRange.serve, its wait included) — or not yet begun.
	journal bool
}

// stopped records what the owner had in hand when a send did not deliver rec:
// rec itself, then rest, the remainder of the pin or the batch it came from.
func (t *ownerTail) stopped(rec Record, rest []Record) {
	t.sending, t.hasSending, t.local = rec, true, rest
}

// closing is the undelivered tail as Rest hands it out: the record being sent,
// the rest of the pin or batch, then live, the live buffer — checked to follow
// prev one seq at a time, the way send checks a record before delivering it.
// A journal leg still outstanding is ErrRestUnavailable, and a hole is an error:
// nothing here is ever a partial tail.
func (t *ownerTail) closing(live []Record) ([]Record, error) {
	if t.journal {
		return nil, ErrRestUnavailable
	}
	n := len(t.local) + len(live)
	if t.hasSending {
		n++
	}
	if n == 0 {
		return nil, nil
	}
	tail := make([]Record, 0, n)
	if t.hasSending {
		tail = append(tail, t.sending)
	}
	tail = append(append(tail, t.local...), live...)
	next := t.prev + 1
	for _, rec := range tail {
		if rec.Seq != next {
			return nil, fmt.Errorf("%w: the closing tail has seq %d where %d follows the last delivered", errNotContiguous, rec.Seq, next)
		}
		next++
	}
	return tail, nil
}

// pump delivers the replay and then the live buffer until the subscription
// ends, and returns why it did. t.prev starts as the seq the reader already
// has — the cursor, or the cutoff for a live-only subscription — and pump keeps
// t describing what it holds, so that finish can build Rest from it.
//
// The replay is in two parts, in sequence order: the head the ring no longer
// holds, streamed from the journal file, and then the ring records the cutoff
// pinned. Each pinned record's charge against the budget is released just
// before its blocking send, not after it returns: once Records has taken a
// record, a publisher may offer the next before this goroutine runs again,
// and a subscription that kept within its budget must not be dropped for a
// record its reader already has. A record from the file was never charged —
// the range is streamed, never held — so it has nothing to release.
func (s *Subscription) pump(head *headRange, pinned []Record, t *ownerTail) error {
	if head != nil {
		// Outstanding until serve returns nil: whatever stops it — the end
		// arriving in its wait, part-way through the file, or before the
		// owner was ever scheduled — leaves the rest of the head unread.
		t.journal = true
		if err := head.serve(s, &t.prev); err != nil {
			return err
		}
		t.journal = false
	}
	for i := range pinned {
		rec := pinned[i]
		pinned[i] = Record{} // handed over: the pin no longer holds it
		s.handOver(0, rec.size())
		if err := s.send(rec, &t.prev); err != nil {
			t.stopped(rec, pinned[i+1:])
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
			if err := s.send(rec, &t.prev); err != nil {
				t.stopped(rec, batch[i+1:])
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
// delivered silently. A record the end kept from Records — the kill arm — is not
// lost to the caller: pump keeps it first in the owner's tail (ownerTail).
func (s *Subscription) send(rec Record, prev *uint64) error {
	if rec.Seq != *prev+1 {
		return fmt.Errorf("%w: seq %d after %d", errNotContiguous, rec.Seq, *prev)
	}
	select {
	case <-s.kill:
		return s.terminal()
	default:
	}
	if h := s.hooks; h != nil && h.sending != nil {
		h.sending(rec.Seq)
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
// the final Err. For a subscription its log's Close ended, the owner's
// undelivered tail is recorded in the same section, so the same reader reads
// the final Rest too (plan 027 §3.7); for any other ending it is discarded,
// with the live buffer, as it always was. Nothing can be offered to the live
// buffer by now: an ended subscription refuses every offer (offer).
func (s *Subscription) finish(err error, t *ownerTail) {
	s.mu.Lock()
	s.endLocked(err)
	s.err = s.cause
	if s.byLog {
		s.rest, s.restErr = t.closing(s.live)
	}
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
