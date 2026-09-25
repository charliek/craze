package engine

import (
	"errors"
	"strings"
	"sync"
	"time"
	"uuid"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/sessions"
)

// The session index, and the durable craze session id (plan 021 §3.8, A15).
//
// # Why the engine owns it
//
// ~/.craze/sessions.jsonl is what --continue and --resume read, and every
// moment worth recording in it is a moment the engine already owns: the first
// prompt of a session (whether a client typed it or the drain took it off the
// queue), the agent naming the session, a turn ending, a /rename, a loaded
// session coming up. The TUI wrote it because the TUI was the driver; a
// session with no client wrote nothing at all. Now one component decides, and
// two clients cannot each write a row for the same event.
//
// # Where each write runs, and under which lock
//
// **Never under e.mu**, whichever path it is on (plan 021 correction 13): a
// row is written through sessions.Store.Upsert, which takes a file lock and
// rewrites a file, and flock is not bounded by anything. A Control method that
// held e.mu across one would stall every other client, the driver and State.
//
//   - COMMAND-DRIVEN writes run on the caller's own goroutine, outside e.mu,
//     exactly as they did in the TUI (§3.2's one documented exception to
//     "waits on nothing"): Submit's first-prompt seed, after the locked
//     section that admitted the turn, and SetTitle's rename. They run inside
//     the command's receipt, so a resend replays the first call's answer and
//     never writes twice (receipts.go). A stalled write there blocks that
//     caller and a same-id duplicate, and nothing else: the receipts table's
//     mutex is a leaf held only to admit and to finish.
//   - EVENT-DRIVEN writes go to the worker below, fed by the log's observer
//     through a merged pending struct and a one-slot kick. The observer runs
//     inside the log's publishing boundary and may not block or do I/O, so all
//     it does is merge and kick (indexWriter.post).
//
// # The pending work, and why a title cannot be lost behind a touch
//
// The slot is NOT a channel of rows, latest-wins. A channel of rows would drop
// a title the moment a turn ended behind it: the touch would be the latest
// value and the title would simply be gone. What is merged under the leaf
// mutex is a STRUCT with one field per kind of work — a pending touch, the
// newest agent title, a pending loaded-row touch — and the kick is a separate
// one-slot channel that only says "there is work". Merging is per field:
//
//   - touch is OR'd. Two turns ending while a write is in flight owe one
//     touch: the row carries a single UpdatedAt and writing it twice says
//     nothing more.
//   - title is LATEST-WINS, and that loses nothing. sessions.applyTitle gives
//     an agent title the same treatment whatever came before it (it overwrites
//     unless the row is pinned), so writing only the newest of two is exactly
//     the state writing both in order would leave.
//   - a write that LANDED bumps UpdatedAt, so it subsumes the touch or the load
//     pending beside it. Subsumption is conditional on SUCCESS: a write that
//     failed performed nothing at all, so the work beside it is still attempted
//     rather than consumed by it (r29 finding 3). The baseline wrote each kind
//     separately and so never lost one to another's failure.
//
// # The bookkeeping flags
//
// row says a row exists, so a touch has something to touch: a session started
// and quit with no prompt, or a turn the agent ran on its own before craze
// ever sent one, must leave nothing behind. A write that LANDED sets it — and
// so does PROCESSING a load, whatever that attempt comes to, because a loaded
// session's row is the very row its id came from and it exists however the
// write for it goes (r30 finding 3).
//
// seeded says the row no longer wants a first-prompt fallback title: either
// that title has been written, or something of higher precedence has named the
// row — a successful agent title or a /rename — so a fallback write would only
// rewrite the whole file for a name no rule would keep (r30 finding 5). A
// fallback write that FAILED sets nothing, which is what makes it retry on the
// next turn.
//
// seeding covers the window in which one path's seed is in flight, and beside
// it ONE seed opportunity is retained: the earliest prompt that has reached the
// first-prompt rule and not yet been attempted. Every opportunity — Submit's
// own, written inline on a client's goroutine, and the engine's own turns,
// posted by the observer — goes through the same locked admission, and the
// FIRST to ARRIVE wins the slot (admitSeedLocked, r30 finding 4).
//
// ONE slot, rather than one per path, is what makes that order total. With a
// pending seed and a deferred seed kept apart, the retry a failed attempt hands
// back could find the pending slot already taken by a LATER prompt and be
// dropped in its favour, and the session would end up named by the wrong turn.
//
// Retaining an opportunity at all is what makes "a failed seed is retried by
// the next turn" true of turns that OVERLAP (r29 finding 2): the second turn's
// seed would otherwise find seeding, do nothing, and be gone by the time the
// first attempt failed, so a session whose first write lost its race could go
// unindexed however many turns it ran.
//
// An attempt in flight is itself owed at close, with or without an opportunity
// behind it (finish), and a touch that meets one before there is a row is
// retained behind it rather than dropped (touch): plan 027 §3.7, SF-15 and
// SF-16.
//
// All of it lives under this file's own mutex, a LEAF: it is taken to merge, to
// take and to record a result, never across the Upsert itself, and never while
// e.mu, s.mu, registry.mu or the receipts table's mutex is held.

// Index is the session index as the engine needs it: one method, the write
// half of sessions.Store. It is an interface so that a caller which persists
// nothing — `craze prompt`, every TUI unit test, every golden — supplies nil
// and nothing can reach a developer's real ~/.craze/sessions.jsonl.
//
// It takes sessions.Row rather than a row type of the engine's own because
// Row IS the merge instruction: its TitleKind says which of the four title
// rules applies, and those rules are sessions' to own. A second spelling of
// them here would be a second thing to keep in step. internal/sessions is a
// leaf that imports nothing of craze's but internal/atomicfile and
// internal/paths, and the engine's depguard rule denies only the two client
// packages and the two provider transports (.golangci.yml).
type Index interface {
	Upsert(sessions.Row) error
}

// IndexOptions is what writing a row needs that the engine cannot know: the
// store, and the four things about the run that live in the client.
type IndexOptions struct {
	// Store persists the rows. nil writes nothing at all — not an error, and
	// not a retry: there is simply no index in this run.
	Store Index
	// CWD is the absolute workspace the rows are keyed by. The TUI absolutises
	// it (tui.New) and internal/cli asks the index for the same string, so the
	// engine takes it as given rather than resolving a second one.
	CWD string
	// Provider is the provider id to record before the session has reported
	// one of its own: the resolved default the session was started as.
	Provider string
	// Hidden reports whether a provider id is one whose sessions stay out of
	// the shared index until it has a loader (plan 018 §3.4), so --continue
	// and --resume never offer a row nothing can load. nil means none is.
	// Skipping counts as DONE, never as failed: no retry, and no error row.
	Hidden func(provider string) bool
	// TitleLine folds a title onto the one line an index row holds and caps
	// it. It is the client's, not the engine's: how a title is made safe to
	// draw is a rendering rule, and the TUI's own is what every row in the
	// file was written with. nil leaves a title exactly as it came.
	TitleLine func(string) string
}

// newCrazeSessionID mints a durable craze session id: a UUIDv7, as the host
// incarnation is (agent.NewIncarnation). The uuid package is the standard
// library's, so this adds no dependency.
func newCrazeSessionID() string { return uuid.NewV7().String() }

// indexWork is what the worker owes, merged field by field. See the file's
// doc comment for why it is a struct and not a row.
type indexWork struct {
	// touch bumps the row's UpdatedAt, so --resume orders by when a session
	// was last used. It is a no-op until a row exists.
	touch bool
	// load is the touch a loaded session owes when it comes up. It is not
	// gated on row: a load's row is the very row its id came from, so there is
	// always one, and writing it is how a resumed session sorts to the front
	// before it has done anything.
	load bool
	// title is the newest title the agent has named the session, "" for none.
	title string
	// seed is the first-prompt fallback title this pass is to write. Unlike
	// the three fields above it is never MERGED here: a seed opportunity is
	// admitted into the writer's own one-slot sequence (admitSeedLocked) and
	// only ever reaches an indexWork when a pass CLAIMS it (take), so that the
	// claim and the attempt it begins are one locked decision. seedText is the
	// turn's prompt, and is a field of its own because an empty prompt still
	// creates a row: "" cannot double as "nothing owed".
	seed     bool
	seedText string
	// seedCause is the command that caused that turn, "" when the drain took
	// it: a failed seed goes out as a StateDelta{IndexErr}, and the client
	// whose command armed the send-now or re-claimed the turn is the one that
	// wants to hear about it (r29 finding 4). It travels with seedText.
	seedCause string
}

func (w indexWork) any() bool { return w.touch || w.load || w.seed || w.title != "" }

// mustWrite reports whether this work has to be ATTEMPTED even at close: a
// seed, an agent title or a loaded-row touch. A plain touch on its own is the
// one kind that may be dropped there — it records nothing but an UpdatedAt the
// next run's first write bumps anyway (indexWriter.finish).
func (w indexWork) mustWrite() bool { return w.seed || w.load || w.title != "" }

// indexWriter is the engine's half of the session index: the options, the
// bookkeeping, the pending work and the one worker goroutine that drains it.
type indexWriter struct {
	opts  IndexOptions
	craze string
	// snap reads the session's snapshot for the provider id and the provider's
	// own session id. It is the session's Snapshot, called with no lock of the
	// engine's held.
	snap func() agent.Snapshot
	// report publishes a failed write as a StateDelta{IndexErr}. cause is the
	// command that caused the write, "" for the event-driven ones.
	report func(cause, msg string)

	// wake is the worker's kick: one slot, because the work itself is merged
	// under mu and every pass re-reads all of it.
	wake chan struct{}
	done chan struct{}
	// exited is closed when the worker's loop returns.
	exited chan struct{}
	// closeWait bounds the worker's last write (finish). It is
	// indexCloseWait in every build but a test's.
	closeWait time.Duration

	mu sync.Mutex
	// pending is the work MERGED field by field, and it never holds a seed:
	// seeds go through the one-slot admission below instead (indexWork.seed).
	pending indexWork
	row     bool
	seeded  bool
	// seeding says a seed is in flight on some goroutine, so a second path
	// that reaches the first prompt while it is parked does not write a second
	// row. seedDone is that attempt's completion, closed once its result has
	// been recorded and whatever it owes is back in the slot; it is nil when no
	// attempt is in flight. The worker's exit waits on it whenever it is set
	// (finish; r30 finding 2, and plan 027 §3.7 / SF-15), because an attempt on
	// a CLIENT's goroutine is the one piece of owed work the slot cannot show on
	// its own.
	seeding  bool
	seedDone chan struct{}
	// touchAfterSeed is a touch that met the attempt in flight with no row yet
	// (plan 027 §3.7 / SF-16): the row that attempt is creating is the one it
	// would have touched. It is RETAINED here rather than dropped, and handed
	// back to pending when the attempt ends (writeSeed), so the row's UpdatedAt
	// is the turn's end and not the seed's slightly earlier instant. It is kept
	// out of pending itself because a worker pass would take it straight back
	// and spin on it for as long as the seed is parked.
	touchAfterSeed bool
	// seedNext is the ONE retained seed opportunity — the earliest prompt that
	// has arrived and not yet been attempted — with the text and cause of the
	// turn it belongs to. See the file's doc comment, and admitSeedLocked.
	seedNext  bool
	seedText  string
	seedCause string

	// closeOnce guards the stop signal, so close is idempotent for a caller
	// that is not Engine.Close's own closeOnce.
	closeOnce sync.Once

	// beforeFinish is a test barrier, nil in every build but a test's, in the
	// spirit of the engine's own hooks: it is called on the worker's goroutine
	// as its exit begins, before the drain looks at anything, so a test can
	// release a write parked in Upsert knowing it is the SHUTDOWN and not an
	// ordinary pass that will pick up what is left. It is written once, before
	// the stop is signalled, and never again; the worker reads it only after it
	// has observed done, so that ordering is what makes it safe without a lock.
	beforeFinish func()
}

// indexCloseWait is how long the worker's LAST write is waited for before it is
// abandoned: the same bound the journal's Close keeps (journal's
// defaultCloseWait), and for the same reason — a quit may not be held by
// something nothing can interrupt, and a flock has neither timeout nor context.
const indexCloseWait = 500 * time.Millisecond

func newIndexWriter(opts IndexOptions, craze string, snap func() agent.Snapshot, report func(cause, msg string)) *indexWriter {
	return &indexWriter{
		opts:      opts,
		craze:     craze,
		snap:      snap,
		report:    report,
		wake:      make(chan struct{}, 1),
		done:      make(chan struct{}),
		exited:    make(chan struct{}),
		closeWait: indexCloseWait,
	}
}

// post merges work into what the worker owes and kicks it. It is the
// observer's one call, so it must not block or do I/O: it takes the leaf mutex
// and makes a non-blocking send, and nothing else.
func (w *indexWriter) post(work indexWork) {
	if !work.any() {
		return
	}
	w.mu.Lock()
	w.pending.touch = w.pending.touch || work.touch
	w.pending.load = w.pending.load || work.load
	if work.title != "" {
		w.pending.title = work.title
	}
	if work.seed {
		// Not merged into pending: a seed goes through the one admission every
		// seed opportunity goes through, whichever goroutine it arrived on
		// (r30 finding 4). run is false because the observer may not do I/O.
		w.admitSeedLocked(work.seedCause, work.seedText, false)
	}
	w.mu.Unlock()
	w.kick()
}

// kick wakes the worker, without waiting and without ever blocking: one slot,
// because the work itself is merged under mu and every pass re-reads all of it.
// wake is never closed, so a kick from a goroutine that outlived the worker —
// an abandoned write's failure — neither blocks nor panics; it simply goes into
// a slot nobody will drain.
func (w *indexWriter) kick() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// admitSeedLocked places ONE seed opportunity in the writer's single slot: the
// one locked decision every one of them goes through, whether it arrived on a
// client's own goroutine (Submit's, seed below) or was posted by the observer
// for a turn the ENGINE started (post above). w.mu is held.
//
// FIRST by ARRIVAL wins, and every later arrival is simply dropped: a fallback
// title is the FIRST prompt's, and with one slot rather than one per path there
// is no way for a retry handed back by a failed attempt to be dropped in favour
// of a prompt that arrived after it (r30 finding 4).
//
// run says the arriving goroutine is willing to make the write itself. It
// reports whether this caller has CLAIMED the attempt and must now make it: a
// posted opportunity never claims, and an inline one claims only when no other
// attempt is in flight.
func (w *indexWriter) admitSeedLocked(cause, prompt string, run bool) bool {
	if w.seeded || w.seedNext {
		return false
	}
	w.seedNext, w.seedText, w.seedCause = true, prompt, cause
	if !run {
		return false
	}
	_, _, claimed := w.claimSeedLocked()
	return claimed
}

// claimSeedLocked begins the attempt on the retained opportunity, taking it out
// of the slot: it reports nothing to write when the slot is empty, when a row is
// already named, or when another attempt is in flight. w.mu is held.
//
// Claiming here, in the section that empties the slot, is what closes the gap a
// two-step "take it, then start it" left: an opportunity arriving in between
// would find the slot empty and no attempt in flight, and make itself a second,
// LATER first prompt.
func (w *indexWriter) claimSeedLocked() (cause, prompt string, ok bool) {
	if !w.seedNext || w.seeded || w.seeding {
		return "", "", false
	}
	cause, prompt = w.seedCause, w.seedText
	w.seedNext, w.seedText, w.seedCause = false, "", ""
	w.seeding, w.seedDone = true, make(chan struct{})
	return cause, prompt, true
}

// take is everything the worker owes, cleared in the same section — and, with
// it, the CLAIM of the seed the worker is about to attempt. Merging and taking
// under one mutex is what makes "a title is never lost behind a touch" hold:
// both fields are read and cleared together, so a post that lands in between is
// simply the next pass's work.
func (w *indexWriter) take() indexWork {
	w.mu.Lock()
	defer w.mu.Unlock()
	work := w.pending
	w.pending = indexWork{}
	work.seedCause, work.seedText, work.seed = w.claimSeedLocked()
	return work
}

// serve is the worker: on every kick, do everything that is owed. Its passes
// are level-triggered like the driver's — each re-reads all of the pending
// work — so two kicks collapsed into one lose nothing.
//
// Each write runs on a goroutine of its OWN, which the worker waits for
// alongside done. That is what keeps Close bounded: sessions.Store.Upsert
// takes a flock, flock has no timeout and no context, and a wedged holder (a
// second craze in the same workspace, a stalled filesystem) would otherwise
// make a quit wait for it with nothing to do about it. When the engine closes,
// a write still in flight is ABANDONED by the worker and finishes on its own
// goroutine: what it writes is state the engine had already decided, so
// letting it land is right, and the process is going away in any case.
//
// done is checked BEFORE each pass and not only in a select beside wake. Two
// ready channels in one select are chosen between at random, and a shutdown
// that picked wake would start a write it was about to abandon; one that picked
// done would drop whatever was pending without looking at it (r29 finding 1).
// Every exit goes through finish instead, which drains the slot one last time.
func (w *indexWriter) serve() {
	defer close(w.exited)
	for {
		select {
		case <-w.done:
			w.finish(nil)
			return
		default:
		}
		work := w.take()
		if !work.any() {
			select {
			case <-w.wake:
			case <-w.done:
				w.finish(nil)
				return
			}
			continue
		}
		res := w.begin(work)
		select {
		case <-res:
		case <-w.done:
			w.finish(res)
			return
		}
	}
}

// begin runs one pass on a goroutine of its own and reports when it is over.
func (w *indexWriter) begin(work indexWork) <-chan struct{} {
	res := make(chan struct{})
	go func() {
		defer close(res)
		w.runPending(work)
	}()
	return res
}

// finish is the worker's exit: the last drain of the slot, and the one write it
// may still owe.
//
// By the time Engine.Close reaches idx.close every producer has stopped — the
// session is closed, so no event can reach the observer, e.done is closed and
// the engine's goroutines are joined — so what the slot holds here is final.
// Dropping it is what r29 finding 1 is about: a first prompt the DRAIN started,
// an agent title, or a loaded row's touch could disappear from --continue
// entirely, where the TUI writing synchronously in Update never lost one.
//
// So a pending seed, title or load is always ATTEMPTED. A plain touch alone is
// still dropped: it records nothing but an UpdatedAt that the next run's first
// write bumps anyway, and it is not worth holding a quit for.
//
// What keeps Close bounded is that the attempt is itself bounded. inFlight is
// the write the worker was waiting for when close began, if there was one: the
// final write queues behind it, because one engine never runs two Upserts on
// this worker at once, and the whole of that — the wait and the write — is
// bounded by ONE closeWait, after which everything is abandoned to its own
// goroutine. Nothing is waited for at all when nothing must be written, so a
// quit with an Upsert parked in a flock and nothing owed returns at once, as it
// did before.
//
// # The seed on somebody else's goroutine
//
// There is one piece of owed work the slot cannot show: a seed in flight on a
// CLIENT's goroutine (Submit's own inline write). It is owed whether or not
// anything is retained behind it, so the exit waits for ANY attempt in flight,
// inside the same bound (plan 027 §3.7, SF-15):
//
//   - With an opportunity retained behind it (r30 finding 2), that opportunity
//     only reaches the slot if the attempt FAILS, and by then the worker would
//     be gone, so a session that ran two prompts could end up with no row at
//     all. A failure hands the retained seed over and it becomes this last
//     write; a success means there is a row already and nothing is owed.
//   - A LONE attempt, with nothing retained, is the session's first row. The
//     TUI could never close under one (its Update is inside that very Submit),
//     but a server's handler goroutine can be writing it when the engine
//     closes, and a quit then left the session out of --continue. It is waited
//     for exactly as the other: the write is the caller's, and the exit simply
//     does not return before it lands or the bound runs out.
//
// The attempt in flight may also be the worker's OWN — inFlight is then a pass
// that claimed a seed — and it is owed just the same: it is waited for as
// inFlight, inside the one bound, where before a quit with nothing else owed
// abandoned it at once.
//
// After the bound everything is abandoned, and a failure that lands later is
// harmless: recording it takes the leaf mutex and kicks a channel that is never
// closed (kick), so it neither blocks nor panics — the opportunity, and a touch
// retained behind the attempt (SF-16), simply return to a slot nobody will
// drain again.
func (w *indexWriter) finish(inFlight <-chan struct{}) {
	if w.beforeFinish != nil {
		w.beforeFinish()
	}
	if !w.owed() {
		return
	}
	bound := time.NewTimer(w.closeWait)
	defer bound.Stop()
	if inFlight != nil {
		select {
		case <-inFlight:
		case <-bound.C:
			return
		}
	}
	if seed := w.activeSeed(); seed != nil {
		select {
		case <-seed:
		case <-bound.C:
			return
		}
	}
	// Taken after the waits, not before: a seed that failed in one of them
	// leaves the retained opportunity in the slot, and that is exactly the
	// retry this drain exists to catch.
	work := w.take()
	if !work.mustWrite() {
		return
	}
	select {
	case <-w.begin(work):
	case <-bound.C:
	}
}

// owed reports whether the worker still has to write, or see written, something
// before it may exit: work in the slot that is not a plain touch; a retained
// seed opportunity — owed whether it is waiting for the worker or waiting on an
// attempt in flight that may yet fail and hand it over; or a seed attempt in
// flight on ANY goroutine, with or without an opportunity behind it (plan 027
// §3.7, SF-15). A plain touch on its own, retained behind a seed or not, is
// still never owed.
func (w *indexWriter) owed() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.pending.mustWrite() || w.seedNext || w.seeding
}

// activeSeed is the completion of a seed attempt in flight, on whichever
// goroutine it runs, and nil when none is (finish). It no longer asks whether an
// opportunity is retained behind the attempt: a lone attempt is the session's
// first row, and the exit waits for it too (plan 027 §3.7, SF-15).
func (w *indexWriter) activeSeed() <-chan struct{} {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.seeding {
		return nil
	}
	return w.seedDone
}

// stop signals the worker to finish, without waiting for it.
func (w *indexWriter) stop() { w.closeOnce.Do(func() { close(w.done) }) }

// close stops the worker and waits for the loop to return. The loop's own exit
// is bounded (finish), so this is bounded too: a write parked in a flock never
// holds it for longer than closeWait.
func (w *indexWriter) close() {
	w.stop()
	<-w.exited
}

// runPending does one pass of owed work, in ONE write wherever it can be one.
//
// A write that lands bumps UpdatedAt as every upsert does, so it IS the touch
// owed beside it, and it creates the row if there is none — which is what a
// loaded session's touch needs too. So a seed or a title that LANDED subsumes
// the load and the touch behind it, and a load that landed subsumes the touch.
//
// Subsumption is conditional on success (r29 finding 3). A title write that
// FAILED performed nothing: it neither bumped UpdatedAt nor created the row, so
// consuming the load beside it would lose a durable id the load was about to
// write and leave a newly loaded session unsorted in the picker, where the
// baseline — which wrote each kind separately — would have got it on the second
// write. So the kinds are tried in order, each skipped only once an earlier one
// has actually landed. A seed is first because it is what creates the row every
// other kind here needs; a pending AGENT title is never subsumed by it, because
// it says something the seed's fallback title does not.
//
// The touch is the only write here that is a no-op without a row.
func (w *indexWriter) runPending(work indexWork) {
	landed := false
	if work.seed {
		landed = w.writeSeed(work.seedCause, work.seedText)
	}
	if work.title != "" {
		landed = w.write("", work.title, sessions.TitleKindAgent)
	}
	if work.load {
		// The row a load's id came from EXISTS — it is the very row --continue
		// or --resume read the id out of — so it is marked known HERE, before
		// and independent of what this attempt comes to (r30 finding 3). A load
		// whose write loses a race for the lock otherwise left row false, and
		// the touch merged beside it, and every ordinary touch after it, wrote
		// nothing at all: the promised retry never happened, and the durable id
		// and the recency a resumed session sorts by could stay unwritten
		// indefinitely.
		w.knownRow()
		if !landed {
			// A touch that is allowed to create, which is what the TUI's own
			// sessionUp did.
			landed = w.write("", "", sessions.TitleKindNone)
		}
	}
	if work.touch && !landed {
		w.touch("")
	}
}

// knownRow records that a row exists without having written one.
func (w *indexWriter) knownRow() {
	w.mu.Lock()
	w.row = true
	w.mu.Unlock()
}

// touch bumps the row's UpdatedAt, and is a no-op until a row exists: a turn
// the agent ran on its own before craze ever sent a prompt must not conjure a
// titleless row.
//
// The one exception is a touch that meets a seed attempt in flight (plan 027
// §3.7, SF-16). A turn's end can reach here while its own first prompt is still
// being written — inside Upsert on the submitting client's goroutine, or
// claimed and not yet there — and dropping it left the row carrying the seed's
// instant, a few milliseconds before the turn it records ended. So it is
// RETAINED (touchAfterSeed) in the same locked section that finds no row, and
// the attempt hands it back when it ends (writeSeed). The attempt clears
// seeding under this same mutex, so a touch either sees it in flight and is
// retained, or sees what it came to.
func (w *indexWriter) touch(cause string) {
	w.mu.Lock()
	row := w.row
	if !row && w.seeding {
		w.touchAfterSeed = true
	}
	w.mu.Unlock()
	if !row {
		return
	}
	w.write(cause, "", sessions.TitleKindNone)
}

// admitSeed is the ADMISSION half of Submit's own first-prompt seed: the
// opportunity goes into the one slot like every other, and this caller is told
// whether the admission CLAIMED the attempt for it, in which case it owes the
// write (writeSeed — §3.2's documented exception, file I/O on a client's
// goroutine).
//
// Nothing is claimed when a row is already named, and nothing when another
// attempt is in flight — the opportunity is simply retained, and this caller
// returns at once rather than waiting for somebody else's write.
//
// The two halves are separate because the caller has something to do BETWEEN
// them: Submit launches the turn there (engine.go's runOwn, r31 finding 1). A
// turn is counted by e.wg before it is launched and gives that count back when
// it ends, so an opportunity admitted after the launch can arrive after Close
// has passed e.wg.Wait() and looked at the slot — and then nothing is left to
// write it. Admitting first is what makes "every turn has made its seed
// opportunity visible before it can end" true.
//
// cause is the command that started the turn, "" when the drain took it.
func (w *indexWriter) admitSeed(cause, prompt string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.admitSeedLocked(cause, prompt, true)
}

// seed is admitSeed and the write a claim owes, one after the other, for a
// caller with nothing to do in between. It reports whether it wrote a row, so a
// caller composing a pass knows whether a touch beside it is still owed.
func (w *indexWriter) seed(cause, prompt string) bool {
	if !w.admitSeed(cause, prompt) {
		return false
	}
	return w.writeSeed(cause, prompt)
}

// writeSeed makes the attempt on a seed this goroutine has CLAIMED — Submit's
// own admission, or a worker pass's take — and records what it came to. It is
// what CREATES the row: a session is worth showing in a picker from its first
// prompt on, and that prompt's first line is what a picker shows until the
// agent or the user names it.
//
// Only a write that LANDED retires the fallback. A session whose very first
// write lost a race for the lock would otherwise never appear in a picker
// again, however many turns it went on to run — so a failure leaves seeded
// false, and whatever opportunity is retained behind this attempt is handed to
// the worker (a kick, never a wait: this may be running on a client's own
// goroutine inside Submit, and that caller has waited for one write already).
// A failure with nothing retained owes nothing: the next turn to start admits
// its own opportunity.
//
// A touch that met this attempt with no row yet (touch, SF-16) is handed back
// to pending whatever the attempt came to, with a kick: after a success it now
// has the row to touch, and after a failure the next pass's touch finds what it
// finds — no row, and it writes nothing, or a retry retained here that it rides
// behind and is subsumed by.
//
// The completion is closed LAST, after the result is recorded and the retained
// opportunity and touch are back in the slot, so a worker exit that waits on it
// (finish) finds everything this attempt owes already there.
//
// All of that bookkeeping is a DEFER keyed on ok, so it runs on the way out of
// a PANIC too (r31 finding 3): sessions.Store.Upsert, the snapshot, the hidden
// predicate, the title fold and the report callback are all somebody else's
// code, and one of them blowing up under a caller that recovers — Submit's
// own — used to leave seeding true and seedDone open for good. Every later turn
// was then retained behind an attempt that had already unwound, and the
// worker's exit waited out its whole bound on a channel nothing would ever
// close. A panic is a failure like any other here: nothing was written, so the
// opportunity retained behind it is handed on exactly as a failed write hands
// it on, and the panic goes on unwinding.
func (w *indexWriter) writeSeed(cause, prompt string) (ok bool) {
	defer func() {
		w.mu.Lock()
		w.seeding = false
		done := w.seedDone
		w.seedDone = nil
		handOn := false
		if ok {
			// A row, named by the first prompt: a retained opportunity is a RETRY
			// and nothing more, so it is discarded rather than written as a second,
			// later fallback title.
			w.seeded = true
			w.seedNext, w.seedText, w.seedCause = false, "", ""
		} else {
			handOn = w.seedNext
		}
		if w.touchAfterSeed {
			w.touchAfterSeed = false
			w.pending.touch = true
			handOn = true
		}
		w.mu.Unlock()
		if handOn {
			w.kick()
		}
		if done != nil {
			close(done)
		}
	}()
	ok = w.write(cause, fallbackTitle(prompt), sessions.TitleKindFallback)
	return ok
}

// rename writes a /rename's row and RETURNS what the write came to, because
// that command's caller is still there to be told (Control.SetTitle) and is
// the one index write with somebody to answer. A user title always wins and
// pins the row, so no agent title this session or a later --continue produces
// can take the name back.
func (w *indexWriter) rename(cause, title string) error {
	err := w.upsert(cause, title, sessions.TitleKindUser, false)
	if errors.Is(err, errNoSessionID) {
		// Not something to tell a user about: the rename itself stands, and
		// there was simply no row to key it to yet. It is what the TUI's own
		// writeIndex did with a session that had not learned its id — no error
		// row, no retry — and a /rename is gated on the session being up in
		// any case.
		return nil
	}
	return err
}

// write is upsert with the failure published as a delta rather than returned,
// which is what every path but a rename does with one. It reports whether the
// index now holds the row.
func (w *indexWriter) write(cause, title string, kind sessions.TitleKind) bool {
	return w.upsert(cause, title, kind, true) == nil
}

// upsert is the one place the engine persists a session.
//
// It runs with NO lock of the engine's held, and takes this file's own leaf
// mutex only to record that a row now exists — never across the Upsert, which
// takes a file lock and rewrites a file.
//
// Two things count as DONE without writing anything, and neither is an error
// or a reason to retry: no store at all, and a hidden provider (plan 018 §3.4).
// A third writes nothing and is NOT done — a session that has not learned its
// id yet — and it is the one "not done" that is not a failure either: nothing
// is reported and nothing is drawn, and the next write tries again.
func (w *indexWriter) upsert(cause, title string, kind sessions.TitleKind, report bool) error {
	if w.opts.Store == nil {
		return nil
	}
	snap := w.snap()
	provider := snap.Provider.Name
	if provider == "" {
		// Only reachable before a session has answered with its own provider;
		// the resolved default is the one it was started as.
		provider = w.opts.Provider
	}
	if w.opts.Hidden != nil && w.opts.Hidden(provider) {
		return nil
	}
	if snap.SessionID == "" {
		// Nothing to key a row on yet. Not a failure and not an error row, but
		// not done either: a caller that only writes once tries again.
		return errNoSessionID
	}
	row := sessions.Row{
		SessionID: snap.SessionID,
		Provider:  provider,
		CWD:       w.opts.CWD,
		CrazeID:   w.craze,
		Title:     w.titleLine(title),
		TitleKind: kind,
	}
	if err := w.opts.Store.Upsert(row); err != nil {
		if report {
			w.report(cause, err.Error())
		}
		return err
	}
	w.mu.Lock()
	w.row = true
	if retiresFallback(kind) {
		// The row has a name of HIGHER precedence than a first prompt's, so no
		// fallback is wanted any more: the attempt is retired and a retained
		// opportunity discarded (r30 finding 5). Without this, a seed that
		// failed before an agent title landed left seeded false for ever, and
		// every later turn retried a write that could only be overruled by
		// applyTitle — reporting an IndexErr each time it failed, for a row
		// that is safely indexed and correctly named.
		w.seeded = true
		w.seedNext, w.seedText, w.seedCause = false, "", ""
	}
	w.mu.Unlock()
	return nil
}

// retiresFallback reports whether a title of this kind, once written, leaves
// the row wanting no first-prompt fallback: its own (which IS the fallback),
// the agent's own name for the session, and a /rename, which pins.
// TitleKindNone is a touch and names nothing.
func retiresFallback(kind sessions.TitleKind) bool {
	switch kind {
	case sessions.TitleKindFallback, sessions.TitleKindAgent, sessions.TitleKindUser:
		return true
	default:
		return false
	}
}

// indexWriteError is a failed index write as a command's answer: ErrIndexWrite
// for a client that wants to know what KIND of failure this is, and the store's
// own error underneath for one that wants to show the user what went wrong.
//
// It is a type rather than fmt.Errorf("%w: %w", …) because the two have to be
// separable: a client draws the row from the cause alone — the text the TUI has
// always drawn — and decides what to say about the rename from the sentinel. A
// multi-%w wrap satisfies errors.Is for both and leaves errors.Unwrap answering
// nil, so there would be no way back to the cause.
type indexWriteError struct{ err error }

func (e *indexWriteError) Error() string { return ErrIndexWrite.Error() + ": " + e.err.Error() }

// Is makes this an ErrIndexWrite; Unwrap keeps the cause matchable too, so a
// caller testing for fs.ErrPermission still finds it.
func (e *indexWriteError) Is(target error) bool { return target == ErrIndexWrite }

func (e *indexWriteError) Unwrap() error { return e.err }

func (w *indexWriter) titleLine(s string) string {
	if w.opts.TitleLine == nil {
		return s
	}
	return w.opts.TitleLine(s)
}

// errNoSessionID stands for "there is nothing to key a row on yet". It is the
// one "not done" that is not a failure: no path publishes it and no path
// returns it to a user — SetTitle's caller sees it only as the honest report
// that the rename was not written down, which is true. What the paths that
// care read is whether the write landed.
var errNoSessionID = errors.New("engine: the session has no id to index yet")

// fallbackTitle is the title a session carries until the agent names it or the
// user renames it: the first line of the first prompt. It is not a pin — an
// agent title replaces it, and /rename replaces either.
//
// The prompt's shell context is not part of that first line. A picker row
// reading "<shell_context>" would name every session that opened with a
// command the same thing, and none of them by what was asked (plan 022 §3.6).
func fallbackTitle(prompt string) string {
	_, prompt = agent.SplitShellContext(prompt)
	first, _, _ := strings.Cut(prompt, "\n")
	return first
}
