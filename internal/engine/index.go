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
// ever sent one, must leave nothing behind. seeded says the first prompt's
// fallback title has been written, so later prompts do not rewrite the whole
// file for a title no rule would keep any more. Only a write that LANDED sets
// either, which is what makes a failed seed retry on the next turn.
//
// seeding covers the window in which one path's seed is in flight, and beside
// it one DEFERRED seed opportunity is retained: the text and cause of the
// earliest turn that reached the first prompt while that attempt was parked.
// Without it "a failed seed is retried by the next turn" is not true of turns
// that OVERLAP (r29 finding 2): the second turn's seed would find seeding, do
// nothing, and be gone by the time the first attempt failed, so a session whose
// first write lost its race could go unindexed however many turns it ran.
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
	// seed is the first-prompt fallback title owed by a turn the ENGINE
	// started — a drain, an armed send firing — which has no caller's
	// goroutine to write on. seedText is that turn's prompt, and is a field of
	// its own because an empty prompt still creates a row: "" cannot double as
	// "nothing owed".
	seed     bool
	seedText string
	// seedCause is the command that caused that turn, "" when the drain took
	// it: a failed seed goes out as a StateDelta{IndexErr}, and the client
	// whose command armed the send-now or re-claimed the turn is the one that
	// wants to hear about it (r29 finding 4). It travels with seedText and is
	// merged with it, first-wins.
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

	mu      sync.Mutex
	pending indexWork
	row     bool
	seeded  bool
	// seeding says a seed is in flight on some goroutine, so a second path
	// that reaches the first prompt while it is parked does not write a second
	// row. A seed that fails leaves both false and the next turn tries again.
	seeding bool
	// deferred is the one seed opportunity retained behind an attempt that is
	// in flight, with the text and cause of the turn that retained it: see the
	// file's doc comment, and seed below for the rule.
	deferred      bool
	deferredText  string
	deferredCause string

	// closeOnce guards the stop signal, so close is idempotent for a caller
	// that is not Engine.Close's own closeOnce.
	closeOnce sync.Once

	// beforeFinish is a test barrier, nil in every build but a test's, in the
	// spirit of the engine's own hooks: it is called on the worker's goroutine
	// as its exit begins, before the drain looks at anything, so a test can
	// release a write parked in Upsert knowing it is the SHUTDOWN and not an
	// ordinary pass that will pick up what is left. It is set before the worker
	// is started and never written again, so it needs no lock.
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
	if work.seed && !w.pending.seed {
		// FIRST wins here, where the title's latest does: the seed is the FIRST
		// prompt's title, so two turns starting before the worker looks owe the
		// earlier one's text, not the later one's. The cause travels with the
		// text it belongs to, so the report of a failure names the command that
		// caused the turn whose prompt is being written.
		w.pending.seed, w.pending.seedText, w.pending.seedCause = true, work.seedText, work.seedCause
	}
	w.mu.Unlock()
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// take is everything the worker owes, cleared in the same section. Merging and
// taking under one mutex is what makes "a title is never lost behind a touch"
// hold: both fields are read and cleared together, so a post that lands in
// between is simply the next pass's work.
func (w *indexWriter) take() indexWork {
	w.mu.Lock()
	defer w.mu.Unlock()
	work := w.pending
	w.pending = indexWork{}
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
// bounded by closeWait, after which both are abandoned to their own goroutines.
// Nothing is waited for at all when nothing must be written, so a quit with an
// Upsert parked in a flock and nothing owed returns at once, as it did before.
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
	// Taken after the wait, not before: a seed that failed in the write above
	// posts the deferred one from inside it, and that is exactly the retry this
	// drain exists to catch.
	work := w.take()
	if !work.mustWrite() {
		return
	}
	select {
	case <-w.begin(work):
	case <-bound.C:
	}
}

// owed reports whether the worker still has to write something before it may
// exit: work in the slot that is not a plain touch, or a deferred seed that an
// attempt still in flight will post if it fails.
func (w *indexWriter) owed() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.pending.mustWrite() || w.deferred
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
		landed = w.seed(work.seedCause, work.seedText)
	}
	if work.title != "" {
		landed = w.write("", work.title, sessions.TitleKindAgent)
	}
	if work.load && !landed {
		// The row a load's id came from: a touch that is allowed to create,
		// which is what the TUI's own sessionUp did.
		landed = w.write("", "", sessions.TitleKindNone)
	}
	if work.touch && !landed {
		w.touch("")
	}
}

// touch bumps the row's UpdatedAt, and is a no-op until a row exists: a turn
// the agent ran on its own before craze ever sent a prompt must not conjure a
// titleless row.
func (w *indexWriter) touch(cause string) {
	w.mu.Lock()
	row := w.row
	w.mu.Unlock()
	if !row {
		return
	}
	w.write(cause, "", sessions.TitleKindNone)
}

// seed writes the first prompt's fallback title, once. It is what CREATES the
// row: a session is worth showing in a picker from its first prompt on, and
// that prompt's first line is what a picker shows until the agent or the user
// names it.
//
// Only a write that LANDED retires the attempt. A session whose very first
// write lost a race for the lock would otherwise never appear in a picker
// again, however many turns it went on to run — so a failure leaves seeded
// false and the next turn to start tries again.
//
// seeding covers the window in which one path's seed is still in flight and
// another turn starts: the second path writes no second row — and RETAINS
// itself as the one deferred seed opportunity, which is what makes "a failed
// seed is retried by the next turn" true of turns that overlap (r29 finding 2).
// Without it, a second turn whose seed found a parked first attempt would do
// nothing and be gone: the first attempt then failing leaves a session that has
// run two turns with no row at all, and only a THIRD turn could give it one.
//
// The retained one is the FIRST to find the attempt in flight, matching the
// slot's own first-wins rule for a seed: it is the earliest prompt after the
// one being attempted, and the earliest prompt is what a fallback title is.
// The attempt SUCCEEDING discards it — there is a row, and its title is the
// first prompt's. The attempt FAILING posts it to the worker, which is a
// non-blocking merge and a kick: this may be running on a client's own
// goroutine inside Submit, and that caller has waited for one write already.
//
// cause is the command that started the turn, "" when the drain took it. It
// reports whether it wrote a row, so a caller composing a pass knows whether a
// touch beside it is still owed.
func (w *indexWriter) seed(cause, prompt string) bool {
	w.mu.Lock()
	if w.seeded {
		w.mu.Unlock()
		return false
	}
	if w.seeding {
		if !w.deferred {
			w.deferred, w.deferredText, w.deferredCause = true, prompt, cause
		}
		w.mu.Unlock()
		return false
	}
	w.seeding = true
	w.mu.Unlock()

	ok := w.write(cause, fallbackTitle(prompt), sessions.TitleKindFallback)

	w.mu.Lock()
	w.seeding = false
	retry := indexWork{}
	switch {
	case ok:
		w.seeded = true
		w.deferred, w.deferredText, w.deferredCause = false, "", ""
	case w.deferred:
		retry = indexWork{seed: true, seedText: w.deferredText, seedCause: w.deferredCause}
		w.deferred, w.deferredText, w.deferredCause = false, "", ""
	}
	w.mu.Unlock()
	// Outside the mutex: post takes it again.
	w.post(retry)
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
	w.mu.Unlock()
	return nil
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
