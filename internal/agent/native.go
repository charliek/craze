package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/charliek/craze/internal/harness"
	"github.com/charliek/craze/internal/harness/modeltable"
	"github.com/charliek/craze/internal/harness/redact"
	"github.com/charliek/craze/internal/journal"
	"github.com/charliek/craze/internal/paths"
	"github.com/charliek/craze/internal/version"
)

// The native adapter is agent.Session over craze's own harness (plan 018
// §3.8): no process, no wire, a model from ~/.craze/native's table. The live
// session is its behavioural contract — the claim, the withdraw, error XOR
// done, a Cancel that waits — and tui.Stub its structural template. It is the
// first file in internal/agent to import internal/paths
// (agent.HomeDir duplicates paths.HomeDir precisely so the ACP code never had
// to); paths imports nothing of craze's, so there is no cycle.

const (
	// nativeEffortID is the one config option the native session advertises,
	// shaped so EffortOption finds it: a select whose id says effort, in the
	// thought_level category ACP agents put their effort control in.
	nativeEffortID = "effort"
	// nativeTitleRuneCap is how long the title derived from the first prompt
	// may be: the same cap the TUI puts on an index title, so the two agree.
	nativeTitleRuneCap = 120
	// nativeAgentMode is the id of the mode that implements, which is also the
	// mode a session with no Options.Mode opens in (plan 023 §3.6).
	nativeAgentMode = "agent"
)

// errNoModels is Start's answer when the harness has never been set up: the
// one thing to do about it is import the model table.
var errNoModels = errors.New(`native: no models configured — run "craze import gx"`)

// nativeSession is the native adapter. Its turn state mirrors the live
// session's: claimed from Begin until the continuation returns, inPrompt
// while the harness runs the turn, cancelling from a Cancel until the claim
// is released (or the next Begin, for a cancel asked while idle).
type nativeSession struct {
	opts Options
	// tweak edits the harness's options just before Open: the test seam
	// NewNative exposes, nil in production.
	tweak func(*harness.Options)

	// log is where every event goes, as on the live session: emit publishes
	// into it and Events is its primary. events is the same channel, kept as
	// the live session keeps its own: receive-only, so nothing can send on it
	// past the log's numbering.
	log    *EventLog
	events <-chan Event
	// asks is this session's ask registry, built beside the log as on every
	// session. The harness's ask_user_question and exit_plan_mode open against
	// it, through the asker in native_asks.go (plan 023 §3.4).
	asks *AskRegistry
	// done is closed first by Close, so no emit — the harness's sink
	// included, which runs on Fantasy's callbacks — can block on a reader
	// that has gone.
	done chan struct{}

	closeOnce sync.Once
	closeDone chan struct{}

	// replaying is true between a load's two EventReplay phases (plan 028
	// §3.4), as on the live session: emitCtx stamps Event.Replayed from it, so
	// everything the replay publishes — the rows, the text, the restored todo
	// list — says it was replayed, and the brackets themselves and the title
	// seeded before them do not. The install delta, the one thing a load
	// enqueues inside the bracket, is stamped by name where it is enqueued
	// (load) rather than from this flag, so no other enqueue can ever carry
	// the stamp; and the flag goes down as soon as that delta's flush has
	// returned, so nothing published after it can either (astra r1-c3 F1). It
	// is written on Start's goroutine and read by publishers that take no lock
	// of the adapter's (emitCtx is lock-free), so it is an atomic rather than
	// a field under s.mu.
	replaying atomic.Bool

	// Interject adds no ordering: it reads the turn state under s.mu, releases
	// it, and then hands the text to the harness's steer box, which takes a
	// leaf lock of its own. So an interjection never waits on the turn it is
	// meant for, even while that turn is emitting into a consumer that is slow
	// — or that is the very goroutine interjecting. No lock is ever held
	// across an emit (plan 019 §3.10): a publish blocked on a full primary
	// under s.mu would stop Close from closing done, which is what releases
	// it.
	//
	// **s.mu → the log's outbox mutex**, which is a strict leaf (plan 021
	// §3.8): a settings delta is *enqueued*, not emitted, in the section that
	// mutates the snapshot — the setters, and the first prompt naming the
	// session — so the order of this session's state deltas is the order of its
	// state, and still nothing blocks under s.mu.

	// steerMu guards steerTexts, the running turn's interjections in both of
	// their spellings, and is a leaf: Interject takes it with nothing else
	// held, so does the sink, and so does the prompt on its way out. Never
	// under s.mu — an ordering is what a leaf is for not having. The pairs
	// belong to one turn and are dropped when its claim is released, so a turn
	// can never translate a row by the previous turn's expansions.
	steerMu    sync.Mutex
	steerTexts []steerText

	// toolMu guards the tool rows, which are merged from the harness's tool
	// events on Fantasy's tool goroutines as well as on its stream one
	// (native_tools.go). It is its own lock, not s.mu: the sink runs inside
	// the harness's turn, and nothing it touches may reach back into the
	// harness. tools is the parent's set, Snapshot().Tools; childTools holds
	// one set per sub-agent, by its id (plan 026 §3.9). A leaf: s.mu → toolMu,
	// and never nested with rosterMu either way.
	toolMu     sync.Mutex
	tools      nativeToolSet
	childTools map[string]*nativeToolSet

	// rosterMu guards the sub-agent roster (native_subagents.go): the rows in
	// spawn order, the finish counter and the last finish's EndedAt. A leaf
	// of the adapter's: s.mu → rosterMu → the log's outbox, and neither toolMu
	// nor s.mu is ever taken under it, nor it under toolMu (plan 026 §3.9,
	// panel P17). Every EventSubagent is enqueued inside the section that made
	// its change, and it is never held across Publish or Flush.
	rosterMu    sync.Mutex
	roster      map[string]*nativeChild
	rosterOrder []string
	finishSeq   uint64
	lastEnded   time.Time

	// s.mu is held across exactly one ask-registry call: the non-blocking
	// CancelTurn in Cancel, which has to be atomic with the read of the turn
	// it is cancelling (plan 023 §3.5). Every registry call that can block or
	// flush — Open's Wait, Flush — stays strictly outside it, and so do
	// BeginTurn and EndTurn. Plan 021 words the cross-provider rule as "never
	// for live or the Stub; native holds s.mu across the non-blocking
	// CancelTurn only".
	mu      sync.Mutex
	started bool
	closed  bool
	hs      *harness.Session
	// table is the model table the harness was opened with, kept to phrase a
	// switch's missing key the way Start phrases one.
	table *modeltable.Table
	// efforts are the effort levels each alias offers, from the harness's
	// model list at Start; the effort option is rebuilt from them whenever
	// the current model or effort changes.
	efforts map[string][]string
	// plugins is every content entry the scan at Start found, hidden ones
	// included, and snap.Plugins is the resolved row for each one that is not
	// hidden (§3.2). Both are set once, in Start, and never change: native
	// advertises no catalog of its own, so there is nothing that could rename
	// a row mid-session. Keeping the entries is what makes the expansion
	// possible at all — a row carries a name and a description, the body is
	// here.
	plugins     []PluginEntry
	snap        Snapshot
	titlePinned bool
	// loading is a load's (Options.LoadSessionID, plan 028 §3.4): true from
	// the section that marks the session started — before the title is
	// seeded and the bracket opens — until the replay's end bracket has gone
	// out (or the load has failed). Nothing but the replay may reach the
	// session's state in that window (astra r1-c3 F1), so every entry point
	// that would publish or enqueue refuses while it holds, as not started: a
	// prompt, and the four setters — SetModel, SetMode, SetConfig, SetTitle —
	// whose deltas would otherwise land inside the bracket, or behind the end
	// bracket and after Start's return. The harness is installed before the
	// replay so that the replay's rows are merged and redacted as a live
	// turn's are, not so that anything else can use it. The engine admits
	// nothing before Started, so only a raw caller ever meets the refusal.
	//
	// The rest need no check, as the review found: Interject refuses as no
	// turn (inPrompt is never set while loading); Cancel finds no claim to
	// cancel and publishes nothing (the mark it leaves is cleared by the next
	// claim); a wake cannot start, since a reopened harness has no child and
	// nothing pending (wakeReadyLocked's HasPending), and CancelSubagent has
	// no child to stop; no ask is open. None of them emits or enqueues here.
	loading    bool
	claimed    bool
	inPrompt   bool
	cancelling bool
	// doneEmitted marks that the turn's ending event has gone out while the
	// slot is still claimed, as on the live session: Interject is refused from
	// then on, so an interjection can never follow an EventDone.
	doneEmitted bool
	// turnCancel cancels the open turn's context; nil until the continuation
	// opens its turn. released is the claim's: closed when its continuation
	// returns, which is what Cancel and Close wait on. turnToken is the
	// registry's name for that same turn (plan 023 §3.5), installed in the
	// very section that registers turnCancel so that a Cancel reads the two
	// together and an ask can never be opened against a turn that has gone.
	turnCancel context.CancelFunc
	turnToken  TurnToken
	released   chan struct{}
	// The wake (native_wake.go, plan 026 §3.11): a turn of the session's own
	// that delivers a background sub-agent's result. wake says one holds the
	// claim — claimed, inPrompt, turnCancel, turnToken and released are then
	// the wake's, installed exactly as prompt installs a turn's, so Cancel and
	// Close treat it as one. foreign is ForeignTurn()'s answer, and
	// s.snap.ForeignTurn mirrors it in the same sections, so Snapshot() and
	// the leaf agree. fenced is the engine's admission fence (AdmissionFence):
	// while it is up the worker refuses to claim. wakeSeq numbers the wakes'
	// bracket ids. wakeKick is the worker's one-slot kick and wakeDone closes
	// when the worker has exited, which Close waits for.
	wake     bool
	foreign  bool
	fenced   bool
	wakeSeq  uint64
	wakeKick chan struct{}
	wakeDone chan struct{}
	// wakeEndingQueued says a wake's ending bracket is enqueued and not yet
	// committed: set in the ending's section, cleared once its flush returns.
	// A prompt that claims in between flushes the outbox before it says
	// anything, so its output cannot overtake the ending (prompt; astra r19).
	wakeEndingQueued bool
	// wakeSeam runs on the worker's goroutine at a recheck that found a wake
	// possible, between that reading and the claim, with no lock held: the
	// window a Begin can win (native_wake.go). wakeDecided is told each
	// recheck's outcome, claimed or stood down, once its section has released
	// s.mu. wakeEnded runs on the worker's goroutine right after the ending's
	// section has released s.mu — the claim released, the ending enqueued —
	// and before the flush: the instant an observer of the log can see the
	// ending. **All three are test seams: nil in production**, set only by a
	// test in this package and only under s.mu, before the first kick.
	wakeSeam    func()
	wakeDecided func(claimed bool)
	wakeEnded   func()
	// sinkSeam runs at the head of sink, on the goroutine that handed the
	// event over — a turn's, a tool's, a background child's — before the
	// adapter reads it: where a test holds a call's acknowledgement while the
	// child finishes (F5's schedule), or panics inside a wake. **A test seam:
	// nil in production**, set before Start and never written after.
	sinkSeam func(harness.Event)
	// cancelSeam runs inside Cancel's critical section, with s.mu held, the
	// turn's context already cancelled and the registry call still to come.
	// **It is a test seam: nil in production**, set only by a test in this
	// package and only under s.mu. It exists because "the read of the turn
	// and CancelTurn are ONE critical section" (plan 023 §3.5) is a fact
	// about a window, and a window can only be pinned from inside it.
	cancelSeam func()
	// modeSeam runs in SetMode between the harness taking the switch and the
	// announcement that publishes it, with s.mu released. **A test seam: nil
	// in production**, set only by a test in this package. It exists so a test
	// can hold two setters at that point and prove each announces what the
	// harness holds rather than what it asked for (r6 finding 3).
	modeSeam func()
}

// steerText is one interjection in both of its spellings: sent is what went
// into the turn, expansions and all, and typed is the line the user actually
// wrote. Wire content is never display content — the rule §3.6 pins for the
// composer's shell mode — and an interjection is the one place the two can
// differ without anything else in the adapter noticing, because the harness
// takes the sent text and hands it back twice: once as the echo the
// transcript's row is made of, and once in Result.Unanswered, which is what a
// turn could not answer and the queue then holds.
//
// Both of those have to be the typed text. The row for the display reason; the
// queue for three: a queued row is shown, a queued row is drained through
// Begin — where nativeRefs would find the /name still at the start of its line
// and expand a second time, sending the block twice and spending the 128 KiB
// total twice — and a queued row is refused over the queue's size cap, so an
// interjection that fitted as four words could vanish as an expansion.
type steerText struct{ sent, typed string }

// rememberSteer records that pairing, before Steer is called: the turn's
// goroutine may take the text up and echo it the instant Steer returns, or
// sooner, so a pair remembered afterwards could arrive too late for its own
// row. Identical spellings are not recorded — there is nothing to translate,
// and typedSteer's fallback answers them.
func (s *nativeSession) rememberSteer(sent, typed string) {
	if sent == typed {
		return
	}
	s.steerMu.Lock()
	s.steerTexts = append(s.steerTexts, steerText{sent: sent, typed: typed})
	s.steerMu.Unlock()
}

// dropSteer takes one pairing back, for a steer the harness refused: there
// will be no echo and no unanswered entry for it, and a caller that keeps
// interjecting into a turn already at the steer cap would otherwise grow the
// list without bound inside one turn. The first exact match, since two
// pairings of one sent text are identical by construction.
func (s *nativeSession) dropSteer(sent string) {
	s.steerMu.Lock()
	defer s.steerMu.Unlock()
	for i, st := range s.steerTexts {
		if st.sent == sent {
			s.steerTexts = append(s.steerTexts[:i], s.steerTexts[i+1:]...)
			return
		}
	}
}

// typedSteer is what the user typed for one of this turn's steers, found by
// what craze sent and deliberately not removed: one pairing answers both
// readings of the same steer — the harness's echo, which arrives during Run,
// and Result.Unanswered, which arrives after it — and a lookup that consumed
// the pairing would leave the second one with nothing. Duplicates cost
// nothing, because sent is a pure function of typed: two entries with one sent
// hold one typed.
//
// Anything unmatched comes back as itself, so an echo craze never recorded
// loses no row and an Unanswered element that was never expanded passes
// through untouched.
func (s *nativeSession) typedSteer(sent string) string {
	s.steerMu.Lock()
	defer s.steerMu.Unlock()
	for _, st := range s.steerTexts {
		if st.sent == sent {
			return st.typed
		}
	}
	return sent
}

// typedSteers is that lookup over the whole of Result.Unanswered, and it is
// its own function on purpose: the translation belongs to native, on the value
// the moment Run hands it over, not to whatever is downstream of it today —
// the engine, above the seam, which decides where these rows go (plan 021
// §3.5) and must never see the wire spelling.
//
// nil in, nil out: a turn that answered everything allocates nothing.
func (s *nativeSession) typedSteers(sent []string) []string {
	if len(sent) == 0 {
		return sent
	}
	out := make([]string, 0, len(sent))
	for _, text := range sent {
		out = append(out, s.typedSteer(text))
	}
	return out
}

// forgetSteers drops the turn's pairings, from the release of its claim: the
// next turn's expansions are its own, and a session that kept every turn's
// would grow for as long as it ran.
func (s *nativeSession) forgetSteers() {
	s.steerMu.Lock()
	s.steerTexts = nil
	s.steerMu.Unlock()
}

// NewNative is the native adapter with a seam: tweak, when non-nil, edits the
// harness's options just before the session is opened, so a test can hand in
// a scripted model (harness.Options.NewModel), its own environment, clock,
// directory or model table. New calls it with nil tweak for any in-process
// provider; nothing else in craze calls it.
//
// Whatever tweak sets is what the session opens on, Options.Prompt included:
// a tweak that supplies its own instruction documents or catalog keeps them,
// and only a Prompt it left empty is filled with what the adapter read off
// disk (open). The reading still happens — the menu and the expansion lookup
// are built from it — so the seam replaces what the model is told exists, not
// what the user can type.
func NewNative(opts Options, tweak func(*harness.Options)) Session {
	return newNative(opts, tweak)
}

func newNative(opts Options, tweak func(*harness.Options)) *nativeSession {
	// A journal's header names the native provider whatever the caller
	// passed, as the snapshot does, and no binary: there is no process.
	log := newSessionLog(opts, journalHeader{provider: NativeProvider().Name()})
	s := &nativeSession{
		opts:      opts,
		tweak:     tweak,
		log:       log,
		events:    log.Primary(),
		done:      make(chan struct{}),
		closeDone: make(chan struct{}),
		// One slot: the worker's rechecks are level-triggered, so two kicks
		// collapsed into one lose nothing (native_wake.go).
		wakeKick: make(chan struct{}, 1),
	}
	// Whatever provider the caller named, this session is the native one, and
	// every snapshot — one taken before Start included — says so.
	s.snap.Provider = NativeProvider().Info()
	// Beside the log, as on every session. It exists so that a client above the
	// seam holds one surface whatever the provider is, and it is what the
	// harness's two blocking tools open against, through the asker this session
	// hands in at Open (native_asks.go); a Gate's Ask reaches it the same way
	// when H3 gives it one, through this adapter and nowhere else.
	s.asks = NewAskRegistry(log, s.Now)
	return s
}

func (s *nativeSession) Events() <-chan Event { return s.log.Primary() }

// Subscribe and Incarnation are the session's EventSource: its log's.
func (s *nativeSession) Subscribe(o SubscribeOptions) (*Subscription, error) {
	return s.log.Subscribe(o)
}
func (s *nativeSession) Incarnation() string { return s.log.Incarnation() }

// EventLog is the session's LogOwner and Now its Clocked (plan 021 §3.3, §3.9),
// exactly as on the live session: one log and one clock per session, whoever
// publishes into it.
func (s *nativeSession) EventLog() *EventLog { return s.log }
func (s *nativeSession) Now() time.Time      { return time.Now() }

// Asks is the session's AskSource (plan 021 §3.6): the registry the harness's
// ask_user_question and exit_plan_mode park on (plan 023 §3.4). It holds no
// permission yet — the gate is still AllowAll (D-39).
func (s *nativeSession) Asks() *AskRegistry { return s.asks }

// Start loads the model table, opens the harness on the requested model (or
// the table's default) and publishes the first snapshot. It does no network
// I/O: the first request goes out with the first prompt.
//
// With Options.LoadSessionID set it resumes that stored session instead (plan
// 028 §3.4) — its transcript reopened, its conversation replayed inside the
// EventReplay bracket — and, as every load does, it then needs a caller that
// is reading the primary while it runs (Session.Start): load says why.
func (s *nativeSession) Start(ctx context.Context) error {
	err := s.start(ctx)
	// Noted before anything is torn down, as on the live session (plan 020
	// §3.5); nothing here closes the session, so the note is the whole of it.
	// A Close racing this start can cut the log's note admission first, which
	// noteStartFailed accepts and counts.
	s.log.noteStartFailed(err)
	return err
}

func (s *nativeSession) start(context.Context) error {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return fmt.Errorf("agent: session already started")
	}
	if s.closed {
		s.mu.Unlock()
		return fmt.Errorf("agent: session closed")
	}
	s.started = true
	// A load refuses everything but its replay from here until its end
	// bracket (s.loading): from before the title is seeded, so no setter can
	// slip in between the seed and the bracket, or into the harness's open.
	load := s.opts.LoadSessionID != ""
	s.loading = load
	s.mu.Unlock()

	if load {
		s.openReplay()
	}
	hs, table, content, err := s.open()
	if err != nil {
		// Nothing has been assigned yet — the content below is assigned only
		// once Open has succeeded — so a session whose harness would not open
		// has no plugins either, and the menu of the next attempt is built
		// from scratch. A load that fails here has opened its bracket and
		// never closes it, as the live session's failed session/load does
		// (live.go's loadSession): Start's error is the end of it, and the
		// flag goes down first so that nothing published after this is
		// stamped replayed (plan 028 §3.4, P23). The refusal goes down in the
		// section that unmarks the start, so a second Start sets it afresh.
		s.replaying.Store(false)
		s.mu.Lock()
		s.started, s.loading = false, false
		closing := s.closed
		s.mu.Unlock()
		if load && closing {
			// A Close interrupted the load as well: its answer, and its wait,
			// since the seeded title may still be in the outbox (loadClosed).
			return s.loadClosed()
		}
		return err
	}
	models := hs.Models()
	efforts := make(map[string][]string, len(models))
	infos := make([]ModelInfo, 0, len(models))
	for _, m := range models {
		efforts[m.Alias] = m.Efforts
		infos = append(infos, ModelInfo{ID: m.Alias, Name: sanitizeLine(m.Name)})
	}
	// The scan and the instruction loader ran inside open(), before the
	// harness was opened, because the prompt they feed is frozen there (§3.4)
	// — but still out here rather than under a lock, for the reason the tables
	// above are: s.mu is the lock a consumer's Snapshot takes, and holding it
	// across a walk of the owner's whole .claude tree would stall every frame
	// of the first second.
	//
	// What is left is what needs the session Open returned. Entries and rows
	// are redacted before anything is stored: a description and a when-to-use
	// travel into the menu, the snapshot and the journal by a road the block's
	// own redaction never touches, and a skill with no frontmatter description
	// has one taken from its body, where a provider key can be
	// (redactNativeEntries). The harness redacted the prompt's own copy of the
	// same strings as it froze them.
	entries := redactNativeEntries(content.entries, hs.Redact)
	// The menu's projection of the rows the catalog was built from: every row
	// but the hidden ones. Naming ran over every entry, hidden ones included,
	// so a visible row's spelling never depends on what is hidden.
	rows := visibleNativeRows(entries, redactNativeRows(content.rows, hs.Redact))

	s.mu.Lock()
	if s.closed {
		// Close ran while the table was loading and found no harness to
		// close; this one is closed here so it cannot outlive the session.
		_ = hs.Close()
		s.loading = false
		s.mu.Unlock()
		s.replaying.Store(false)
		if load {
			return s.loadClosed()
		}
		return fmt.Errorf("agent: session closed")
	}
	s.hs = hs
	s.table = table
	s.efforts = efforts
	s.plugins = entries
	s.snap.Plugins = rows
	s.snap.Models = infos
	// For a load, the stored session's own id: the harness reopened the
	// transcript under it, so it is the index row's sessionId, the id a bridge
	// or an attach resolves the session by (plan 028 §3.4, seam 2).
	s.snap.SessionID = hs.ID()
	// The three modes native advertises, and the one it opened in — read back
	// from the harness, which is where Options.Mode was resolved and where a
	// later switch is confirmed from (plan 023 §3.6). A load's is the
	// transcript's own unless --plan or --ask said otherwise (plan 028 §3.3).
	s.snap.Modes = nativeModes()
	s.snap.CurrentMode = nativeCurrentMode(hs.Mode())
	s.refreshCurrentLocked()
	// The wake worker, in the section that installs the harness it wakes,
	// so a Close that finds the harness finds the worker to join too
	// (native_wake.go). It has nothing to do until a background child's
	// result is pending, which the harness's OnPending kicks it for — and a
	// resumed session has no child of its own incarnation yet (§3.3 item 10).
	s.wakeDone = make(chan struct{})
	go s.wakeWorker(s.wakeDone)
	if load {
		// The install delta waits for the replay (load): it says what the
		// session is once its transcript has been shown, and it is the last
		// thing inside the bracket. s.loading has held since the start was
		// marked, above.
		s.mu.Unlock()
		if err := s.load(hs); err != nil {
			return err
		}
		s.notePromptSources(hs, content.extras)
		return nil
	}
	// The install says what it installed, in the section that installed it:
	// the model the harness opened on, the effort option that model brings,
	// the mode it opened in, the commands native advertises and the plugin
	// rows — which a native session resolves once here and never again (X3).
	// Without it a client folding the stream would have to call Snapshot() to
	// learn the session's starting state, which is the gap r23 finding 2 is
	// about. A new session's install carries no Title section: Start has
	// nothing to name it with (a load's seeds one, openReplay), and the first
	// prompt is what gives it one, publishing its own Title delta (plan 024
	// S1c C8, SF-01, superseding X47's "stays silent"). Event.Mode/Event.Text
	// stay empty: starting is nobody's agent update.
	//
	// Enqueued and not flushed, exactly as the live session's install is: Start
	// may not wait on the primary's reader, because a caller is allowed not to
	// be one until Start has returned (Session.Start, r25 finding 1). Every
	// later settings delta goes through the same outbox behind it, so a client
	// that folds them is never behind.
	s.enqueueDeltaLocked("", Event{}, s.installDeltaLocked(false))
	s.mu.Unlock()
	// Noted with s.mu released, as every note is (plan 020 §3.5); a Close in
	// that window drops it, which noteSession accepts and counts.
	s.log.noteSession(journal.SessionNote{ProviderSessionID: hs.ID()})
	s.notePromptSources(hs, content.extras)
	return nil
}

// installDeltaLocked is the one delta a Start's install publishes: every
// section it installed, in full, as the snapshot now stands — live.go's
// installDeltaLocked, whose rule it follows (a section is restated rather than
// left out). Commands is carried by both installs even while native
// advertises none, so a client that folds the stream learns the command list
// from the install on (plan 028 P21). withTitle adds the Title section, which
// only a load's install carries — "there is no title" included, for a load of
// an untitled row — because a new session's title is its first prompt's to
// publish. s.mu is held.
func (s *nativeSession) installDeltaLocked(withTitle bool) *StateDelta {
	model, mode := s.snap.CurrentModel, s.snap.CurrentMode
	st := &StateDelta{
		Model:    &model,
		Mode:     &mode,
		Config:   &ConfigState{Options: cloneConfig(s.snap.Config)},
		Commands: &CommandsState{Commands: append([]CommandInfo(nil), s.snap.Commands...)},
		Plugins:  &PluginsState{Plugins: append([]PluginCommand(nil), s.snap.Plugins...)},
	}
	if withTitle {
		title := s.snap.Title
		st.Title = &title
	}
	return st
}

// openReplay is the first half of a load (plan 028 §3.4, steps 1–2), before
// the harness is opened: the index row's title and pin are seeded, and the
// replay's bracket opens.
//
// The seed is the live session's (live.go's loadSession). No transcript
// records a title — the index row craze resolved the id from is the only
// place a resumed session's name can come from — so it is state the session
// had BEFORE the replay: a State-only Title delta with no Event.Text (craze's
// own row, not the agent naming the session, so it prints no `title` line and
// is never written back as an agent title), enqueued in the section that
// seeds it and flushed ahead of the bracket, outside s.mu. The pin is what
// keeps the first prompt after the resume from renaming a session the user
// named (prompt's rule: an untitled, unpinned session only).
//
// The flush may wait for the primary's reader: a load already requires one
// while Start runs (Session.Start), and the replay after it asks for the same
// thing. It is bounded by s.done, as every barrier here is.
func (s *nativeSession) openReplay() {
	s.mu.Lock()
	seeded := false
	if title := sanitizeText(s.opts.Title); title != "" {
		s.snap.Title = title
		s.enqueueDeltaLocked("", Event{}, &StateDelta{Title: &title})
		seeded = true
	}
	if s.opts.TitlePinned {
		s.titlePinned = true
	}
	s.mu.Unlock()
	if seeded {
		_ = s.log.Flush(context.Background(), s.done)
	}
	// The bracket opens before the flag goes up, so the opening itself is not
	// stamped replayed, as live.go's is not.
	s.emit(Event{Type: EventReplay, Replay: &ReplayInfo{Phase: ReplayStart}})
	s.replaying.Store(true)
}

// load is the rest of a load once the reopened harness is installed (plan 028
// §3.4, steps 4–7): the stored conversation replayed, the install delta, the
// session note, and the bracket closed. s.loading is set, and load clears it
// — by a defer, so however load ends, a panic out of the sink included
// (hs.Replay runs the sink on this goroutine and recovers nothing), neither it
// nor s.replaying is left up behind it.
//
// Every event goes out in order inside the bracket, before Start returns and
// so before the engine's Started (S2's conditions, seam 5). The replay's text,
// prompts and rows are published synchronously on this goroutine, as a live
// turn's are: hs.Replay calls the session's own sink holding no lock of the
// harness's, and the sink holds none of the adapter's — s.mu, toolMu, rosterMu
// — across a publish (sink's audit), so each is in the log, in order, as the
// path is walked; the restored todo list is the one EventTodos the replay ends
// with. The install delta goes through the outbox, enqueued under s.mu like
// every settings delta and flushed outside it before the end bracket, because
// EventReplay{end} means "the restored session is installed". Nothing is
// coalesced: a stored prompt is one message, so it is one EventUser.
//
// Publishing synchronously is why a native load, like every load, needs the
// primary read while Start runs (Session.Start): a transcript longer than the
// primary's buffer blocks the replay's next publish until a reader drains it.
//
// A Close that interrupts it anywhere — during the replay, during the install
// delta's flush, or between that flush and the end bracket — makes it return
// "session closed" with no end bracket (X19), through loadClosed, which waits
// for that Close to have finished, so the install delta cannot be delivered
// after Start has returned (astra r1-c3 F2).
func (s *nativeSession) load(hs *harness.Session) error {
	// Only after the end bracket may a turn or a setter begin: its first
	// event can no longer land inside the bracket. Deferred, so the flags go
	// down on every way out (the function's comment); on the ordinary one
	// they go down after the end bracket, which is published below.
	defer func() {
		s.replaying.Store(false)
		s.mu.Lock()
		s.loading = false
		s.mu.Unlock()
	}()
	replayErr := hs.Replay(s.sink)
	s.mu.Lock()
	closed := s.closed
	if replayErr == nil && !closed {
		// The session as the transcript left it — model, effort, mode — the
		// title the row seeded, and the commands and plugin rows this start
		// resolved: one delta, stamped replayed, as live.go's load install is
		// — by name, the one enqueue a load stamps (replaying's comment).
		s.enqueueDeltaLocked("", Event{Replayed: true}, s.installDeltaLocked(true))
	}
	s.mu.Unlock()
	if closed || errors.Is(replayErr, harness.ErrClosed) {
		// Close ran during the replay — it closed the harness, so the walk's
		// events went nowhere. The load did not complete, and like a failed
		// open it ends with no end bracket.
		s.replaying.Store(false)
		return s.loadClosed()
	}
	if replayErr != nil {
		// The replay was refused: no end bracket either.
		s.replaying.Store(false)
		return &nativeError{msg: "native: replaying session " + sanitizeLine(hs.ID()) + ": " + sanitizeLine(replayErr.Error()), cause: replayErr}
	}
	flushErr := s.log.Flush(context.Background(), s.done)
	// The install delta was the last thing inside the bracket: from here on
	// nothing this session publishes is stamped replayed.
	s.replaying.Store(false)
	// The flush fails only once a Close has begun — done closed, or the log's
	// Close under way — and a Close may begin just after a flush that
	// succeeded: either way the load is over, and the end bracket, which
	// would be dropped, is not attempted.
	s.mu.Lock()
	closed = s.closed
	s.mu.Unlock()
	if flushErr != nil || closed {
		return s.loadClosed()
	}
	// Noted with s.mu released, as every note is (plan 020 §3.5); a Close in
	// that window drops it, which noteSession accepts and counts.
	s.log.noteSession(journal.SessionNote{ProviderSessionID: hs.ID(), LoadedFrom: s.opts.LoadSessionID})
	// A Close in the last window, after the check above, drops the bracket
	// (emitCtx answers false only for a Close): the load did not end, so
	// Start does not say it did.
	if !s.emitCtx(context.Background(), Event{Type: EventReplay, Replay: &ReplayInfo{Phase: ReplayEnd}}) {
		return s.loadClosed()
	}
	return nil
}

// loadClosed is a load's "session closed" (X19): Start's answer when a Close
// interrupted the load. It returns only once that Close has finished — its
// log closed, and with it the outbox drained into the record — so nothing the
// load enqueued (the seeded title, the install delta) can reach a consumer
// after Start has returned (astra r1-c3 F2). Every caller has seen a Close
// begin (s.closed, or a flush or publish the log refused, which only Close
// makes it do), and nothing that Close waits for waits on this goroutine: no
// turn or wake runs while the session is loading, and the replay has
// returned. The wait is taken only while s.closed says a Close has begun, so
// a caller that is wrong about that cannot hang Start.
func (s *nativeSession) loadClosed() error {
	s.mu.Lock()
	closing := s.closed
	s.mu.Unlock()
	if closing {
		<-s.closeDone
	}
	return fmt.Errorf("agent: session closed")
}

// notePromptSources is the provenance of the prompt this session just froze
// (§3.4): its size and digest, and one line per instruction document and per
// catalog row that went into it. The prompt itself is never stored — not in
// the transcript, which records only its hash, and not here — so without this
// note there is no way afterwards to say which files a session was reading.
//
// Every element goes through the session's redactor, exactly as the prompt's
// own copy of the same text did: these lines hold paths craze read off disk,
// and a journal is a file on the owner's machine like any other.
//
// It also says, once, when the prompt has grown past the size at which the
// owner should know: it is sent with every request of the session, and nothing
// compacts it until H7 (R1). That is a diagnostic rather than a refusal — the
// files are the user's own and craze is not the one to decide they are too
// many — so the session starts either way.
func (s *nativeSession) notePromptSources(hs *harness.Session, x harness.PromptExtras) {
	size := hs.PromptSize()
	if size > maxNativePromptBytes {
		s.note(fmt.Sprintf("the system prompt is %d bytes, over %d: it is sent with every request of this session",
			size, maxNativePromptBytes))
	}
	instructions, catalog := promptSources(x, hs.Redact)
	s.log.Note(journal.DiagNote{Kind: journal.DiagPromptSources, Fields: map[string]any{
		"prompt_bytes":  size,
		"prompt_sha256": hs.PromptSHA256(),
		"instructions":  instructions,
		"catalog":       catalog,
	}})
}

// contentHome is the home directory this session reads the user's own Claude
// content under: Options.ContentHome, else the process's. The option is the
// whole of the seam — a loader that reached for HomeDir() itself would make a
// test's isolation a matter of which file it happened to touch.
func (s *nativeSession) contentHome() string {
	if home := strings.TrimSpace(s.opts.ContentHome); home != "" {
		return home
	}
	return HomeDir()
}

// contentWarn is where discovery's diagnostics go: Diag, falling back to
// Stderr, exactly as the live session's plugin scan resolves it
// (live.go's discoverPlugins). Every line it writes is craze's own — a file
// whose name craze could never offer, a plugin id that is reserved — and none
// is an agent's, so none belongs on the stderr lane a TUI defers (§3.7.1).
//
// craze's own voice is not the same as craze's own text. Every one of these
// lines interpolates something read off disk — a path, an import as its author
// wrote it, a filename craze cannot offer as a name — and any of those can
// hold a provider key: an import of "@../sk-live-.../missing.md", a rule file
// named after the key, a workspace under a directory that is one. So red
// covers the whole lane, here, at the one point every content diagnostic
// passes through. It cannot be left to the writers: they run before the
// session exists — the prompt they feed is frozen inside harness.Open (§3.4)
// — so there is no session redactor for them to reach, and the harness's
// refusal of a workspace whose path holds a key comes after discovery has
// already had the chance to print it.
//
// It is also where native says what it does not honour. --plugin-dir is
// cursor-agent's flag by another route and native reads no directory but the
// three of §3.2, so a session handed one must say so rather than start with a
// menu quietly missing what the user asked for (A14).
func (s *nativeSession) contentWarn(red *redact.Replacer) func(string) {
	// diagWriter rather than the fallback written out again: its own comment
	// already names this lane, and a third spelling of "Diag, else Stderr" is
	// a third place to miss when the lane grows a sink.
	diag := diagWriter(s.opts)
	warn := func(msg string) {
		if diag == nil {
			return
		}
		fmt.Fprintln(diag, red.String(msg))
	}
	if len(s.opts.PluginDirs) > 0 {
		warn("plugin dirs ignored for " + NativeProvider().Name())
	}
	return warn
}

// visibleNativeRows is the menu's projection of one resolved list: every row
// but the hidden ones (§3.2). The entries behind them are kept whole — naming
// ran over all of them, and C6's model-facing catalog lists the hidden ones —
// so this is a filter over the rows and never over the entries.
//
// Because the expansion lookup is built from these rows, a hidden entry is not
// expandable either: user-invocable: false says the user does not invoke it,
// and a name the menu never offered that expanded anyway would be exactly the
// surprise that setting exists to prevent.
func visibleNativeRows(entries []PluginEntry, rows []PluginCommand) []PluginCommand {
	hidden := make(map[string]bool, len(entries))
	for _, e := range entries {
		if e.Hidden {
			hidden[strings.ToLower(e.Plugin+":"+e.Name)] = true
		}
	}
	if len(hidden) == 0 {
		return rows
	}
	out := make([]PluginCommand, 0, len(rows))
	for _, r := range rows {
		if !hidden[strings.ToLower(r.Qualified)] {
			out = append(out, r)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// open resolves everything Start needs and opens the harness, returning it
// with the model table it was opened on and the content it read for it. Its
// errors are already phrased for the user.
//
// The directory is paths.NativeDir(), and the table is loaded from it after
// tweak has run, so a test's tweak can point Home somewhere else or hand in a
// Table outright — the frame runner isolates HOME, so a golden cannot count
// on files under it.
//
// The content is read here, and not by Start after this returns, because the
// system prompt is frozen inside harness.Open and can never be added to
// afterwards (D-30): the instruction documents and the catalog have to be
// complete by the time Options.Prompt is handed over. The workspace they are
// read for is hopts.Workspace — the one the harness itself opens on, after
// tweak, rather than a second nativeWorkspace call, which falls back to
// os.Getwd() and could answer differently: a session whose content came from
// one directory and whose tools ran in another would be a bug nobody could
// see. A failed Open therefore throws the reading away, which is what leaves
// such a session with no rows at all.
//
// A load (Options.LoadSessionID) opens the stored session instead
// (harness.Options.Resume, plan 028 §3.3): the same reading, the same seams,
// and "unspecified" left unspecified — no --model is no model, which the
// harness reads as the transcript's own (fundedModel is a new session's
// default only, P8), and no --plan/--ask is no mode, the transcript's last.
func (s *nativeSession) open() (*harness.Session, *modeltable.Table, nativeLoad, error) {
	var none nativeLoad
	// The mode the session starts in, resolved before anything is opened: an
	// unknown one must refuse Start rather than be silently ignored, and it is
	// refused here so the message names the modes native has rather than
	// arriving as the harness's own ErrUnknownMode (plan 023 §3.6).
	mode, err := nativeMode(s.opts.Mode)
	if err != nil {
		return nil, nil, none, err
	}
	ws, err := nativeWorkspace(s.opts.Workspace)
	if err != nil {
		return nil, nil, none, err
	}
	hopts := harness.Options{
		Home:      paths.NativeDir(),
		Workspace: ws,
		Version:   version.Version,
		Mode:      mode,
		Resume:    s.opts.LoadSessionID,
	}
	if s.tweak != nil {
		s.tweak(&hopts)
	}
	// The startup window reads the environment through one sealed memo, so
	// that the key set this adapter gates its content against and the key set
	// harness.Open resolves for its redactor cannot be two sets. The release
	// is deferred rather than written after Open, so that every way out of
	// this function — a table that will not load, a model that does not
	// resolve, a Close that landed meanwhile, or Open itself failing — leaves
	// the session reading the real environment again (sealedGetenv).
	getenv, release := sealedGetenv(hopts.Getenv)
	defer release()
	hopts.Getenv = getenv
	if hopts.Home == "" {
		return nil, nil, none, errors.New("native: there is no craze directory to read the model table from (set HOME or CRAZE_HOME)")
	}
	if hopts.Table == nil {
		table, err := modeltable.Load(hopts.Home)
		if errors.Is(err, modeltable.ErrNotConfigured) {
			return nil, nil, none, errNoModels
		}
		if err != nil {
			return nil, nil, none, fmt.Errorf("native: %w", err)
		}
		hopts.Table = table
	}
	table := hopts.Table
	for _, w := range table.Warnings {
		s.note(w)
	}

	switch {
	case strings.TrimSpace(s.opts.Model) != "":
		// Resolved the way every provider's --model is (MatchModel's
		// normalisation), so an alias typed with spaces or capitals, or a
		// model's display name, still finds it.
		alias, err := MatchModel(Snapshot{Models: tableModels(table)}, s.opts.Model)
		if err != nil {
			return nil, nil, none, fmt.Errorf("native: %v (models.toml has %s)", err, strings.Join(table.Aliases(), ", "))
		}
		hopts.Model = alias
	case hopts.Model == "" && hopts.Resume == "":
		// A new session's default. A resumed one leaves Model "" — unspecified,
		// which the harness resolves from the transcript's own model before
		// the table's default (plan 028 §3.3, P8); choosing a funded alias here
		// would make it explicit and switch the conversation's model.
		alias, err := s.fundedModel(table, hopts.Getenv)
		if err != nil {
			return nil, nil, none, err
		}
		hopts.Model = alias
	}

	// One resolution of the keys, for all three of the things that need them:
	// the gate that drops an entry whose identity holds one (dropKeyBearing),
	// the redactor every content diagnostic goes through (contentWarn), and —
	// through the memo above — the redactor Open builds for what it freezes.
	keys := nativeTableKeys(table, hopts.Getenv)
	red := redact.New(keys...)
	content := loadNativeContent(
		resolveNativeSources(hopts.Workspace, s.contentHome()),
		s.opts.Compat,
		keys,
		s.contentWarn(red),
	)
	// Only where the seam left it alone, so that tweak keeps its last word on
	// every field of harness.Options (NewNative). The assignment cannot simply
	// be moved above the tweak instead: the content is read for the workspace
	// tweak may have changed, against the table it may have handed in, with
	// the keys its own Getenv resolves, so there is nothing to assign until
	// after it has run.
	if len(hopts.Prompt.Instructions) == 0 && len(hopts.Prompt.Catalog) == 0 {
		hopts.Prompt = content.extras
	}
	// What the provenance note records is what was frozen, seam or no seam.
	content.extras = hopts.Prompt
	// How the harness's two blocking tools reach a person: this session's own
	// registry, behind tool.Asker (plan 023 §3.4). Left to the seam's last word
	// like Prompt above, so a harness test that hands in an asker of its own
	// keeps it; in every real build tweak is nil and this is the asker.
	if hopts.Asker == nil {
		hopts.Asker = nativeAsker{s: s}
	}
	// The sub-agent seams (plan 026 §3.4, §3.6), each left to tweak's last word
	// like the two above: the personas this reading found, already mapped to
	// native tool ids; a child's model matched the way --model was above; and
	// the lane the harness reports a persona's or the default's unresolved
	// model or effort on, journaled (native_personas.go). opened is how that
	// lane reaches the session's own redactor once Open has returned it.
	if hopts.Personas == nil {
		hopts.Personas = content.personas
	}
	if hopts.MatchModel == nil {
		hopts.MatchModel = nativeModelMatcher(table)
	}
	var opened atomic.Pointer[harness.Session]
	if hopts.Warn == nil {
		hopts.Warn = s.harnessWarn(red, &opened)
	}
	// Background children (plan 026 §3.11), each left to tweak's last word
	// like the seams above: on only for an interactive session, which has a
	// wake worker to deliver a result nobody typed for — headless `craze
	// prompt` runs every agent call in the foreground, as before; the
	// session-level sink is this session's own, the one every turn's events
	// already go through; and a result becoming pending kicks the wake
	// worker (native_wake.go), which returns at once as OnPending must.
	if s.opts.Interactive {
		hopts.Background = true
	}
	if hopts.Sink == nil {
		hopts.Sink = s.sink
	}
	if hopts.OnPending == nil {
		hopts.OnPending = s.kickWake
	}

	// The last look at closed before anything is opened. A Close racing Start
	// finds no harness under s.mu and returns (Close), while everything above
	// — the walk of the owner's .claude tree, the documents read for the
	// prompt — runs on; without this, a session nobody will ever use would go
	// on to open a harness, sweep its spill directory and write a diagnostic
	// or two. What is deliberately not done here is stopping the reading
	// itself: the loaders take no context, so a Close during the walk still
	// returns while it finishes. Giving them one belongs with the start/stop
	// lifecycle the session-control work owns, not to a check on the way past.
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return nil, nil, none, fmt.Errorf("agent: session closed")
	}

	hs, err := harness.Open(hopts)
	switch {
	case err == nil:
	case hopts.Resume != "" && errors.Is(err, harness.ErrNoTranscript):
		if hs, err = s.openEmpty(hopts); err != nil {
			return nil, nil, none, err
		}
	case hopts.Resume != "":
		return nil, nil, none, phraseLoadError(err, table, hopts.Model, hopts.Resume)
	default:
		return nil, nil, none, phraseSetupError(err, table, hopts.Model)
	}
	opened.Store(hs)
	return hs, table, content, nil
}

// diagResumeEmpty is the journal diag a load that opened empty is noted as
// (openEmpty): its one field, "session", is the id it was opened under.
const diagResumeEmpty = "resume_empty"

// openEmpty is a load of a session that has no transcript at all (plan 028
// §3.5, P35, PD8): Open found no file for hopts.Resume, so the session is
// opened new under that same id instead — the file is written, as any new
// session's is, with its first output — and journaled as resume_empty. That is
// the ordinary life of a native row: the index row is written by the session's
// first prompt, the transcript only by its first output, so "prompt, Esc, quit,
// craze -c" leaves a row with no file. The id, the row and the title the load
// seeded are unchanged, so this is the same session going on, not a different
// one standing in for it — which is why it is a deliberate exception to plan
// 013 §3.8's "a load never falls back to session/new" (D-60).
//
// Only ErrNoTranscript — no file at all — qualifies. A file that is there and
// cannot be resumed (busy, corrupt, not the session's, a child's, a newer
// craze's) is open()'s error, as it has always been.
//
// With no file there is no transcript's model, effort or mode to resume on,
// so they are a new session's: an explicit --model, else the funded default
// (fundedModel), and --plan/--ask or agent. Nothing is replayed; the load still
// opens and closes its bracket and installs its state (load).
func (s *nativeSession) openEmpty(hopts harness.Options) (*harness.Session, error) {
	id := hopts.Resume
	hopts.Resume, hopts.SessionID = "", id
	if hopts.Model == "" {
		alias, err := s.fundedModel(hopts.Table, hopts.Getenv)
		if err != nil {
			return nil, err
		}
		hopts.Model = alias
	}
	hs, err := harness.Open(hopts)
	if err != nil {
		return nil, phraseSetupError(err, hopts.Table, hopts.Model)
	}
	s.log.Note(journal.DiagNote{Kind: diagResumeEmpty, Fields: map[string]any{"session": hs.ID()}})
	return hs, nil
}

// sealedGetenv is getenv sealed for the startup window: until release is
// called, a variable is read once and every later reader is served from that
// reading; afterwards every read goes to getenv again. It returns both halves
// because both matter, and a reader who sees only the memo will take
// remembering for the point.
//
// The seal is what one startup needs. A session resolves the table's keys
// twice — in open(), to gate the content it read before the prompt is frozen,
// and again inside harness.Open, to build the redactor that covers what was
// frozen — and those two have to be the same set. A Getenv whose answer
// changes between them, a test seam or a process setting a variable while it
// starts, would leave the gate judging one set and the redactor covering
// another: an entry whose name holds a key the gate did not know would be
// listed raw in the menu and redacted to a marker in the model's catalog,
// which is exactly the disagreement between the two projections that dropping
// such an entry exists to prevent. Reading once makes them one set by
// construction rather than by two calls happening to agree.
//
// The release is what the rest of the session needs, and it is not a detail.
// The harness keeps this function as its own getenv for the session's whole
// life and calls it on every switch (toolset.resolve), where reading the
// environment again is the documented point: a session learns a key when a
// switch makes current a model whose provider's key the environment gained
// since Open, and the redactor grows to cover it from the next turn. A memo
// held past startup would silently take that away — a switch that used to
// work would fail for the rest of the session — in exchange for closing a
// window between two calls microseconds apart. So the seal covers exactly the
// window it was for, and nothing after it.
//
// The harness calls it from its own goroutines, so both halves are guarded.
func sealedGetenv(getenv func(string) string) (read func(string) string, release func()) {
	if getenv == nil {
		getenv = os.Getenv
	}
	var mu sync.Mutex
	seen := make(map[string]string) // nil once released
	read = func(name string) string {
		mu.Lock()
		defer mu.Unlock()
		if seen == nil {
			return getenv(name)
		}
		if v, ok := seen[name]; ok {
			return v
		}
		v := getenv(name)
		seen[name] = v
		return v
	}
	// Idempotent, and it drops what it remembered: nothing reads the map
	// again, and a session should not hold every key it resolved at startup
	// for as long as it runs.
	release = func() {
		mu.Lock()
		defer mu.Unlock()
		seen = nil
	}
	return read, release
}

// fundedModel is the model a session with no --model starts on: the table's
// default, unless its provider has no key, and then the lexicographically
// first alias whose key resolves — one unfunded provider must not lock the
// owner out of the others (plan 018 §3.8). getenv is the harness's, so the
// fallback judges keys exactly as Open will. A default that fails for any
// other reason is left for Open to report.
func (s *nativeSession) fundedModel(table *modeltable.Table, getenv func(string) string) (string, error) {
	def := table.DefaultModel
	_, err := table.Resolve(def, getenv)
	if !errors.Is(err, modeltable.ErrNoAPIKey) {
		return def, nil
	}
	for _, alias := range table.Aliases() {
		if _, rerr := table.Resolve(alias, getenv); rerr == nil {
			s.note(fmt.Sprintf("the default model %q has no API key; starting on %q", def, alias))
			return alias, nil
		}
	}
	return "", &nativeError{
		msg:   "native: no configured model has an API key; " + strings.TrimPrefix(noKeyText(table, def), "native: "),
		cause: err,
	}
}

// nativeWorkspace is the session's workspace as the harness needs it:
// absolute, the process's working directory when none was given — the same
// rule the live session applies before it spawns.
func nativeWorkspace(ws string) (string, error) {
	if ws == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("native: %w", err)
		}
		ws = wd
	}
	abs, err := filepath.Abs(ws)
	if err != nil {
		return "", fmt.Errorf("native: %w", err)
	}
	return abs, nil
}

// nativeModes is what a native session advertises: cursor's three ids, which
// are the harness's own three words as well (harness.Options.Mode), so `/plan`,
// `/ask` and `/agent` resolve on native exactly as they do there and nothing
// translates between the seam and the harness (plan 023 §3.6). A fresh slice
// per call, because a snapshot hands its modes out.
func nativeModes() []ModeInfo {
	return []ModeInfo{
		{ID: nativeAgentMode, Name: "Agent"},
		{ID: "plan", Name: "Plan"},
		{ID: "ask", Name: "Ask"},
	}
}

// nativeMode is the harness id for the mode a session was asked to start in:
// craze's vocabulary (--plan, --ask) resolved against the three above, so the
// word that reaches cursor reaches the harness as the same mode. "" stays "",
// which the harness reads as agent mode.
//
// An id none of the three answers to refuses Start rather than being ignored:
// a caller that asked for plan mode and got agent mode would edit the
// workspace.
func nativeMode(want string) (string, error) {
	if strings.TrimSpace(want) == "" {
		return "", nil
	}
	return nativeResolveMode(want, modeIDs(nativeModes()))
}

// nativeResolveMode is Start's and SetMode's one reading of a mode word:
// ResolveMode against the ids this session advertises — which is how every
// provider resolves one — and the adapter's refusal naming them when it is
// none. The refusal is the harness's ErrUnknownMode underneath, so a caller
// can still test for it, and phrased here because only the adapter knows what
// native offers.
func nativeResolveMode(want string, ids []string) (string, error) {
	if id, ok := ResolveMode(want, ids); ok {
		return id, nil
	}
	return "", &nativeError{
		msg:   fmt.Sprintf("native: mode %q is not one of %s", want, strings.Join(ids, ", ")),
		cause: harness.ErrUnknownMode,
	}
}

// nativeCurrentMode is the harness's mode as a snapshot spells it. The harness
// answers with one of the three words for an open session; "" is what
// Options.Mode spells for agent mode, and is read as agent here so a snapshot
// never shows a mode its own list has not got.
func nativeCurrentMode(mode string) string {
	if mode == "" {
		return nativeAgentMode
	}
	return mode
}

// tableModels is the model list a snapshot shows for table: aliases as ids,
// sorted, each named by its table name (sanitized: it is text from a file the
// owner edits by hand) or its alias. The ids stay as the table spells them,
// because they go back to SetModel verbatim.
func tableModels(table *modeltable.Table) []ModelInfo {
	out := make([]ModelInfo, 0, len(table.Models))
	for _, alias := range table.Aliases() {
		name := sanitizeLine(table.Models[alias].Name)
		if name == "" {
			name = alias
		}
		out = append(out, ModelInfo{ID: alias, Name: name})
	}
	return out
}

// note writes one of the adapter's few diagnostics to Options.Diag, falling
// back to Stderr as the live session's notes do, and to nowhere when neither
// is set.
func (s *nativeSession) note(msg string) {
	w := s.opts.Diag
	if w == nil {
		w = s.opts.Stderr
	}
	if w == nil {
		return
	}
	fmt.Fprintln(w, "native: "+msg)
}

// refreshCurrentLocked copies the harness's current model and effort into the
// snapshot and rebuilds the effort option for that model: present only when
// the model lists efforts, so the effort UI hides itself for one that has
// none. The harness is read under s.mu so that two switches racing each other
// publish in the order they took effect. s.mu is held.
func (s *nativeSession) refreshCurrentLocked() {
	alias, effort := s.hs.Current()
	s.snap.CurrentModel = alias
	levels := s.efforts[alias]
	if len(levels) == 0 {
		s.snap.Config = nil
		return
	}
	values := make([]SelectValue, 0, len(levels))
	for _, e := range levels {
		values = append(values, SelectValue{Value: e, Name: sanitizeLine(e)})
	}
	s.snap.Config = []ConfigOption{{
		ID:           nativeEffortID,
		Name:         "Effort",
		Category:     "thought_level",
		Type:         "select",
		Current:      effort,
		SelectValues: values,
	}}
}

// Prompt is Begin and its continuation back to back, as on the live session.
func (s *nativeSession) Prompt(ctx context.Context, text string) (Result, error) {
	return s.Begin(text)(ctx)
}

// Begin is the claim and its journal record, as on the live session (plan 020
// §3.5): the prompt note is written here with s.mu released, and the
// continuation writes the prompt_end its own (Result, error) says. A
// continuation run a second time is refused without a second ending: the
// attempt is already closed by the run that happened.
func (s *nativeSession) Begin(text string) func(context.Context) (Result, error) {
	run := s.claim(text)
	return s.log.wrapPrompt(journal.PromptKindPrompt, text, run)
}

// claim claims the prompt slot now, on the caller's goroutine, with the live
// session's semantics: a Begin while the slot is claimed or a turn is open
// claims nothing and its continuation returns ErrPromptInFlight; the claim
// clears a cancel asked before it, which was not for this prompt; and a
// Cancel from here until the continuation opens the turn makes it withdraw.
//
// A Begin while a wake holds the slot (native_wake.go) claims nothing and its
// continuation returns ErrForeignTurn, the live session's refusal for a turn
// the agent is running on its own: nothing is sent, nothing is emitted, and
// the wake's claim is untouched. The check is atomic with the wake's own
// claim — both are one s.mu section — so whichever took s.mu first wins, and
// the engine, which reads ForeignTurn under the same lock before it calls
// Begin, never meets this refusal in practice (plan 026 X30): a wake that
// claimed first makes ForeignTurn true and the engine queues instead. The
// refusal is for a caller that bypasses it, and for the race pinned at this
// level (TestNativeWakeRacesBegin).
//
// The continuation must be run exactly once, and is safe if it is not: a
// second call, after the first or alongside it, returns ErrPromptInFlight and
// touches nothing — the claim, its release and the turn all belong to the
// first call, and running them twice would send the prompt again and then
// close the claim's release a second time.
func (s *nativeSession) claim(text string) func(context.Context) (Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.wake {
		return func(context.Context) (Result, error) { return Result{}, ErrForeignTurn }
	}
	if s.claimed || s.inPrompt {
		return func(context.Context) (Result, error) { return Result{}, ErrPromptInFlight }
	}
	s.claimed = true
	s.cancelling = false
	rel := make(chan struct{})
	s.released = rel
	var ran atomic.Bool
	return func(ctx context.Context) (Result, error) {
		if !ran.CompareAndSwap(false, true) {
			return Result{}, ErrPromptInFlight
		}
		return s.prompt(ctx, text, rel)
	}
}

// prompt is the claim's continuation: one harness turn, ended the way the live
// session ends one (live.go's prompt) — success, or a cancel by Cancel or
// Close, is exactly one EventDone; failure, the caller's own context ending
// included (callerEnded), is exactly one EventError and no EventDone. What
// becomes of craze's queue after either is the engine's: it settles the turn
// above this seam, after the ending published here.
func (s *nativeSession) prompt(ctx context.Context, text string, rel chan struct{}) (Result, error) {
	// The registry's name for this turn, minted before the locked section that
	// installs it, as live mints its own (live.go's prompt): token minting and
	// retirement stay outside s.mu, which is held across one registry call and
	// only one (the struct's comment, plan 023 §3.5). A token nothing was ever
	// opened against costs one number and resolves nothing.
	tok := s.asks.BeginTurn()
	// One release for the whole claim, whichever way it ends, and after the
	// turn's last event: a Cancel or Close waiting on rel then finds the
	// ending already emitted and the slot free. The turn's steer pairings go
	// with it, before s.mu is taken rather than under it, so steerMu stays a
	// leaf with no lock ordering of its own.
	defer func() {
		// The turn's token is retired here for every way out that did not
		// retire it already: a withdrawal, a closed session, a refusal, a turn
		// that failed. The ordinary path retires it before its terminal event
		// (below), and a second EndTurn on a retired token is a no-op. Outside
		// s.mu and before close(rel), as live does it.
		s.asks.EndTurn(tok)
		s.forgetSteers()
		s.mu.Lock()
		s.claimed, s.inPrompt, s.cancelling = false, false, false
		s.turnCancel, s.turnToken = nil, TurnToken{}
		if s.released == rel {
			s.released = nil
		}
		close(rel)
		s.mu.Unlock()
		// Every release of the claim — this turn's success, its failure, a
		// cancel, a withdrawal, a closed or unstarted session — rechecks for a
		// background result the turn left pending (native_wake.go): one that
		// finished after the turn's last step boundary would otherwise wait
		// for the next thing to kick the worker, which may be nothing.
		s.kickWake()
	}()

	s.mu.Lock()
	if s.cancelling {
		// Cancelled since the claim, and the turn is not open: withdraw.
		// Nothing was sent, nothing is emitted and nothing is written.
		s.mu.Unlock()
		return Result{}, ErrPromptCancelled
	}
	if s.closed {
		s.mu.Unlock()
		return Result{}, fmt.Errorf("agent: session closed")
	}
	hs := s.hs
	if hs == nil || s.loading {
		// A load's harness is installed before its replay, and the session is
		// not started until the bracket has closed (s.loading).
		s.mu.Unlock()
		return Result{}, fmt.Errorf("agent: session not started")
	}
	// The turn opens in the same locked section that checked for a cancel,
	// and registers its cancel func and its ask token there, so a Cancel
	// either marked the claim above or finds the turn here: never neither,
	// and never one of the two without the other.
	turnCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	s.inPrompt = true
	s.doneEmitted = false
	s.turnCancel = cancel
	s.turnToken = tok
	if !s.titlePinned && s.snap.Title == "" {
		// The typed text, not the expansion: a session called after the whole
		// of a command file would say nothing about what the user asked for.
		// The journal's prompt note (Begin) keeps the typed text for the same
		// reason; only the store records what was actually sent.
		title := nativeTitle(text)
		s.snap.Title = title
		// A non-empty title only: the guard above fires while
		// s.snap.Title == "", so an empty one (a leading blank line, a
		// shell-context block with no message of its own, whitespace only)
		// leaves it "" and the NEXT prompt gets the same chance — today's
		// rule for the snapshot (plan 021 C10), now extended to the delta
		// rather than bypassed by it (sol r19 finding 2). Publishing one here
		// would say the wrong thing twice over: State.Title's own doc calls a
		// non-nil empty string a title that has been CLEARED, which no
		// prompt this session has taken up has done, and a title-less prompt
		// would otherwise publish on every retry until one finally names the
		// session.
		if title != "" {
			// Published now (plan 024 S1c C8, SF-01), where S1b left it
			// unpublished on purpose: a State-only Title delta with no
			// Event.Text, SetTitle's shape exactly (below), so a client that
			// folds the stream — S2's socket client, which has no other way
			// to learn it (SD-30) — learns the title from the first prompt on
			// instead of waiting for /rename or some other refresh.
			//
			// Cause is "" because none is in hand: unlike SetTitle/SetModel/
			// SetMode/SetConfig, each called with the client's Command cause,
			// Prompt/Begin take none — Engine.Submit holds a Command but does
			// not forward its cause into Begin (engine.go) — so this is the
			// session's own event, the same "" loadSession uses for the
			// title it seeds before a resumed session's replay (live.go),
			// the one other title delta no client asked for.
			//
			// No OutboxRoom check: unlike SetTitle, a rejectable command
			// that checks room before it mutates anything and refuses the
			// whole rename if there is none, this section has already
			// committed to running the turn, so refusing the prompt over a
			// full outbox is not on the table. It does not need to be:
			// Enqueue (eventlog.go) always accepts past the soft bound
			// OutboxRoom reports on; the only enqueue it refuses is one that
			// lands after Close has cut the outbox, and even then it drops
			// the event silently (counted in droppedAtClose) rather than
			// returning an error. enqueueDeltaLocked cannot fail this call.
			//
			// Indexing is unchanged: the index observer keys on Event.Text
			// (engine.go), which stays empty here — this is craze's own
			// title, not the agent naming the session — so a native row
			// (plan 028 §3.5) keeps the first-prompt title the engine seeds
			// it with, as any row the agent never names does.
			s.enqueueDeltaLocked("", Event{}, &StateDelta{Title: &title})
		}
	}
	// The references are resolved in the same locked section as the rest of
	// the turn's state, as the live session resolves its own: the rows and the
	// entries they were resolved from have to be read together, or a prompt
	// could expand a body under a name the menu means something else by.
	refs := s.refsLocked(text)
	sessionID := s.snap.SessionID
	behindAWake := s.wakeEndingQueued
	s.mu.Unlock()

	// A wake's ending still queued — enqueued in the section that released
	// the claim this turn then took (native_wake.go) — is flushed before this
	// turn says anything, as the engine's launch flushes before it runs a
	// continuation (engine.go): the turn's own publishes never wait for
	// queued events, so its text would otherwise overtake the ending, which
	// would then close this turn's stream instead of the wake's (astra r19).
	// Only then: with no wake ending in flight a turn asks its model whatever
	// the outbox holds, as it always has. Bounded by s.done, as every barrier
	// here is: a Close that has begun frees it, and the turn then finds the
	// session closed.
	if behindAWake {
		_ = s.log.Flush(context.Background(), s.done)
	}

	// One string, not content blocks: the harness takes the whole user message
	// at once (turn.go). Redacted with the session's own redactor, because Run
	// persists and sends it unchanged.
	sent, expanded := nativePrompt(text, refs, sessionID, hs.Redact)
	for _, cmd := range expanded {
		// Its own copy, not a pointer into the slice: the event outlives this
		// loop, and the range variable is per-iteration. A publish abandoned on
		// the turn's cancelled context stops the announcements and nothing else
		// — the turn below still runs, returns StopCancelled at once and emits
		// the one ending it owes.
		if !s.emitCtx(turnCtx, Event{Type: EventCommand, Command: &cmd}) {
			break
		}
	}

	res, err := hs.Run(turnCtx, sent, s.sink)
	// Translated the moment Run hands it over, and before anything reads it:
	// what a turn could not answer comes back in the spelling craze sent, and
	// every use of it downstream — the queue's row, the size cap that row is
	// judged by, the draft Begin re-expands when it drains — is the user's.
	unanswered := s.typedSteers(res.Unanswered)
	if errors.Is(err, harness.ErrInTurn) {
		// Unreachable: the claim admits one continuation at a time and each
		// Run returns before its claim is released. Said as the refusal it
		// would be, with no event, rather than as a failed turn.
		return Result{}, ErrPromptInFlight
	}
	// Every row the turn left running is closed before its ending goes out,
	// so no consumer ever sees a turn end with a call still spinning.
	s.settleTools()
	var failed error
	switch {
	case err != nil:
		failed = phraseTurnError(err)
	case res.StopReason == harness.StopCancelled:
		failed = s.callerEnded(ctx)
	}
	s.markDone()
	// Whatever the user interjected that the turn could not answer is reported,
	// not requeued: the engine above this seam owns craze's queue and puts the
	// steers back itself, at the head, before it decides what runs next (plan
	// 021 §3.5). It is reported on the error path too — the engine requeues and
	// then clears, so a consumer still hears why the queue emptied — which is
	// why unanswered, translated above as Run handed it over, is returned from
	// both branches. It has to have been translated by now: the pairs it is
	// read from are forgotten in this prompt's deferred release, so a caller
	// translating after the return would be handed the sent spelling back.
	// The turn's token is retired first and the outbox waited for second, both
	// before the terminal event, which is live's endAskTurn in the same order
	// (live.go:1056-1067) and outside s.mu. Retiring resolves every ask still
	// parked against this turn as turn_ended without touching any tool's
	// context (asks.go's EndTurn) — the tool has already returned by here,
	// since Fantasy joins it before Run returns — and the flush then puts those
	// endings, and any opening still behind them, in the record ahead of the
	// EventDone (plan 021 §3.6, plan 023 §3.5).
	//
	// It is bounded by s.done, as every emit below it is: Close closes done and
	// then waits for this continuation (rel) BEFORE it closes the log, and with
	// the primary full and its reader stopped only the log's close frees the
	// drainer this barrier waits on — so a flush that did not escape here would
	// be a Close waiting for a goroutine waiting for that same Close.
	s.asks.EndTurn(tok)
	_ = s.log.Flush(context.Background(), s.done)
	if failed != nil {
		// The error goes out first, so a consumer is already in its error
		// state by the time the engine's chain policy clears its own queue at
		// settlement (plan 021 §3.5) — this session has none of its own any
		// more.
		s.emit(Event{Type: EventError, Err: failed})
		return Result{Unanswered: unanswered}, failed
	}
	s.emit(Event{Type: EventDone, StopReason: res.StopReason})
	return Result{StopReason: res.StopReason, Unanswered: unanswered}, nil
}

// refsLocked is what a draft invokes, resolved against this session's own
// content. s.mu must be held: the rows and the entries behind them are one
// fact read twice, and the menu the user typed from is built from the same
// rows. Native's scan, not cursor's — a reference counts only at the start of
// a line (§3.3) — and the lookup is built from the visible rows, so a hidden
// entry is unreachable by name as well as unlisted.
func (s *nativeSession) refsLocked(text string) []pluginRef {
	return nativeRefs(text, buildPluginLookup(s.plugins, s.snap.Plugins))
}

// markDone closes the turn to interjections just before its ending event goes
// out, as the live session does (live.go): from here Interject is ErrNotInTurn
// rather than text merged into a turn that has already ended. The harness
// refuses by then too — it settles its steers before Run returns — so this is
// the adapter saying the same thing in its own state, and the reason the
// refusal needs no call into the harness.
func (s *nativeSession) markDone() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.doneEmitted = true
}

// The turn's unanswered steers are reported in Result.Unanswered, never
// pushed anywhere here: the engine above the seam puts them back — through
// PushFront, last first, in the same locked section that decides the turn's
// successor, so steered text is always ahead of whatever was waiting behind
// the turn that took it (plan 021 §3.5). The queue and the events describing
// it are mutated and enqueued under one mutex in internal/engine, with no
// lock held across a blocking send — the wedge this comment used to describe
// (queueTx holding emitMu across a blocking primary send, with a consumer
// that queues from its own receive on the other side) is gone with the
// session's own queue verbs, and the closed-session window went with them: a
// row can no longer be pushed onto a queue whose events are already
// suppressed, because nothing pushes one here.

// callerEnded says whether a turn the harness reports cancelled was ended by
// the caller's own context — its deadline, or its cancel — rather than by
// this session's Cancel or Close, and if so returns the failure it is. The
// harness cannot tell the two apart: any end of the turn's context is a
// clean cancel to it. The live session can, and treats the first as a failed
// prompt (live.go's prompt: the client's error is EventError, which is what
// makes the engine clear its queue), so the adapter does too; its own Cancel
// and Close stay the user's "stop", one EventDone{cancelled}, after which the
// engine's chain policy decides about the queue. A cancel of ours that lands
// alongside the caller's counts as ours. nil when the cancel was ours.
func (s *nativeSession) callerEnded(ctx context.Context) error {
	s.mu.Lock()
	ours := s.cancelling || s.closed
	s.mu.Unlock()
	cause := ctx.Err()
	if ours || cause == nil {
		return nil
	}
	return &nativeError{msg: "native: prompt cancelled: " + cause.Error(), cause: cause}
}

// nativeTitle is the title a native session carries until /rename: the first
// line of its first prompt, on one line and capped, as the TUI derives an
// index title.
//
// A shell context block in front of that prompt is not the prompt: a session
// called "<shell_context>" would say nothing about what the user asked for,
// which is the same reason the title is taken before the expansion above
// (§3.6).
func nativeTitle(prompt string) string {
	_, prompt = SplitShellContext(prompt)
	first, _, _ := strings.Cut(prompt, "\n")
	title := sanitizeLine(first)
	n := 0
	for i := range title {
		if n == nativeTitleRuneCap {
			return title[:i]
		}
		n++
	}
	return title
}

// sink is the harness's event sink: the model's text and thinking become
// the session's events, sanitized, because unsanitized model output is a
// terminal-escape path into the TUI, its tool events become the rows
// native_tools.go merges, its todo list becomes the snapshot's and one
// EventTodos (native_asks.go), and its sub-agents become the roster, the
// children's own tagged events and the agent row's task (native_subagents.go,
// plan 026 §3.9). StepDone, Retrying and Diag have no agent event and are
// dropped: usage goes to the transcript only — but for the sub-agent usage an
// unsaved step could not record, which is journaled (noteSubagentUsage) — the
// harness allows one silent retry (plan 018 §3.8), and a Diag is for the
// journal and for diagnosis, not for a consumer (plan 019 §3.5). ToolProgress
// never blocks here, since the harness drops a snapshot rather than wait on
// it.
//
// It runs synchronously on Fantasy's callbacks — the tool ones on tool
// goroutines — which is why emit gives up once Close has begun.
//
// A load's replay (plan 028 §3.4) hands it the same events on Start's
// goroutine — text, thinking, ToolStarted, ToolCalled and ToolFinished, Todos —
// and one of its own, Prompted, a stored user message; everything
// published meanwhile is stamped replayed (s.replaying), and nothing else here
// knows the difference, which is the point: a restored row is merged, capped
// and redacted exactly as a live one. The one exception is the result, which
// the transcript keeps only as text (ToolFinished.Replayed; toolFinished).
//
// # It is concurrent (plan 026 §3.9, panel P16): the audit
//
// Until sub-agents it had only ever been entered by one goroutine at a time:
// every harness emit ran under the parent turn's mutex (turn.go; progress
// takes it with TryLock). Now each child's events reach it under the child
// turn's own mutex, straight from the runner and never through the parent's,
// so the parent's goroutines and up to four children's are in here at once,
// plus the dispatcher's lossy progress goroutines of each. Every case below
// therefore touches only state behind a lock of its own that is a leaf where
// it is taken, and holds none of them across a publish:
//
//   - TextDelta, ThoughtDelta, Steered's emit, a child's text and thought and
//     its EventUser: the event log alone — Publish's boundary, entered with no
//     lock of the adapter's held (emit).
//   - the four tool events, the parent's and a child's alike: toolMu, for the
//     merge into the owner's set only, released before the row is published
//     (native_tools.go). A parent agent row's close reads the roster first,
//     under rosterMu, released before toolMu is taken.
//   - Steered: steerMu, a leaf, for the lookup of the typed text.
//   - Todos: s.mu, for the one assignment of the snapshot's list, and only
//     the parent's — a child has no todo list, and its Todos is dropped. s.mu
//     is held across nothing that waits on a sink.
//   - StepDone (the parent's): the journal's Note, which never blocks.
//   - the three sub-agent events: rosterMu for the roster and its enqueue (the
//     outbox mutex, a leaf beneath it), toolMu for the child's set in sections
//     of its own, never nested, and the log's Flush with no lock held. The
//     redactor is taken before rosterMu or toolMu is — read under s.mu, its
//     keys gathered under the harness's own leaf locks (redactor) — and then
//     applied inside the section, to the whole payload, where applying it
//     takes no lock (review r8).
//
// s.mu, when a case takes it, is taken alone: never under toolMu or rosterMu,
// which Snapshot takes under s.mu, one after the other.
func (s *nativeSession) sink(ev harness.Event) {
	if seam := s.sinkSeam; seam != nil {
		seam(ev)
	}
	switch e := ev.(type) {
	case harness.TextDelta:
		if t := sanitizeText(e.Text); t != "" {
			s.emit(Event{Type: EventText, Text: t})
		}
	case harness.ThoughtDelta:
		if t := sanitizeText(e.Text); t != "" {
			s.emit(Event{Type: EventThought, Text: t})
		}
	case harness.ToolStarted:
		s.toolStarted("", e)
	case harness.ToolCalled:
		s.toolCalled("", e)
	case harness.ToolProgress:
		s.toolProgress("", e)
	case harness.ToolFinished:
		s.toolFinished("", e)
	case harness.StepDone:
		s.noteSubagentUsage(e)
	case harness.SubagentStarted:
		s.subagentStarted(e)
	case harness.SubagentEvent:
		s.subagentEvent(e)
	case harness.SubagentFinished:
		s.subagentFinished(e)
	case harness.SubagentUndelivered:
		s.subagentUndelivered(e)
	case harness.Retrying, harness.Diag:
		// Dropped (the function's comment).
	case harness.Todos:
		s.applyTodos(e)
	case harness.Steered:
		// The turn took up an interjection. What the harness echoes is what
		// craze sent it, which since §3.3 is the expansion — so the row is the
		// line the user typed, looked up by that echo: a transcript that
		// answered "/linux-test" with the whole skill file would be showing the
		// wire back to the person who wrote four words. The text is the
		// caller's own either way, straight back out unsanitized, exactly as
		// the live session emits grok's interjection broadcast: it never went
		// near the model or a provider.
		//
		// It comes from the turn's goroutine, always before the turn's ending
		// event, so it can never follow the EventDone or EventError below.
		s.emit(Event{Type: EventUser, Text: s.typedSteer(e.Text), Interjection: true})
	case harness.Prompted:
		s.replayedPrompt(e)
	}
}

// replayedPrompt is a stored user message a load's replay walked (plan 028
// §3.4): the user row a restored transcript draws, as an ACP load's replayed
// prompt is. craze's shell-context block in front of it is wire content, never
// display content, so it is stripped here (nativeTitle's rule, and the fold's
// own for a replayed prompt); a plugin command's expansion is what was sent,
// and replays as stored — the typed spelling was never written down. A steer
// is an interjection row, as the live Steered above makes one.
//
// The text comes off disk, not from the caller, so it takes the task
// payload's discipline — redact, sanitize, redact (nativeSafe) — over the
// harness's own redaction of it: a key a zero-width space splits is whole
// again once sanitized.
func (s *nativeSession) replayedPrompt(e harness.Prompted) {
	_, text := SplitShellContext(e.Text)
	text = nativeSafe{red: s.redactor()}.text(text)
	s.emit(Event{Type: EventUser, Text: text, Interjection: e.Steer})
}

// Cancel stops the claimed prompt and waits for it, as the live session's
// does: a turn in flight has its context cancelled and Cancel returns once
// the continuation has returned — its EventDone already emitted, any partial
// answer persisted interrupted — or ctx ends; a prompt claimed and not yet
// open withdraws when its continuation runs, and Cancel waits for that. With
// nothing claimed it is a no-op.
//
// Native has no wire, so CancelOutcome maps onto its own state instead (plan
// 021 §3.7): Wrote is whether this call found a running turn's context to
// cancel — s.turnCancel is registered in the same locked section that
// s.cancelling is set here, so a nil turnCancel at that read means the
// continuation has not opened its turn yet and never will reach the harness
// for this call. Withdrew is exactly the complement: a claimed continuation
// that has not run, or not yet opened, and is now marked cancelling — it
// will see the mark and return ErrPromptCancelled with nothing ever sent.
// Wrote and Withdrew are decided in that one locked read and do not change
// with how the wait below ends; Settled is true only once rel has actually
// closed, because that is the one signal that says the claim — and whatever
// it was running — is over.
func (s *nativeSession) Cancel(ctx context.Context) (CancelOutcome, error) {
	s.mu.Lock()
	if s.hs == nil {
		s.mu.Unlock()
		return CancelOutcome{Settled: true}, nil
	}
	// The mark, the claim and the turn are read in one locked section, the
	// one the continuation's opening also takes: a claimed prompt either sees
	// the mark and withdraws or has already registered its cancel func here.
	s.cancelling = true
	claimed, cancel, rel := s.claimed, s.turnCancel, s.released
	if cancel != nil {
		// The turn's context first, its asks second, both in this one
		// critical section — the one place native calls the registry with s.mu
		// held (plan 023 §3.5; the struct's comment says so too).
		//
		// The order is the point. The woken waiter sees a context that is
		// already done, so the tool answers aborted and the turn's own halted
		// check sees t.ctx.Err() before Fantasy can start a step on an ask
		// that has just been answered.
		//
		// The one section is the point as well: CancelTurn drains EVERY open
		// ask, whatever turn it belongs to (asks.go's CancelTurn), so a cancel
		// that read the turn here and called the registry after releasing the
		// lock could answer the NEXT turn's card. Reading and cancelling
		// together is what makes "still the current turn" true by
		// construction. It is safe to hold s.mu across: the registry's mutex
		// is a leaf that is never held across anything that blocks, and the
		// one lock it takes under its own is the log's outbox, whose Enqueue
		// never waits (asks.go's "Locks").
		cancel()
		// Nil in production; a test's barrier inside the window (the struct's
		// field says why).
		if s.cancelSeam != nil {
			s.cancelSeam()
		}
		s.asks.CancelTurn(s.turnToken)
	}
	s.mu.Unlock()
	if !claimed || rel == nil {
		return CancelOutcome{Settled: true}, nil
	}
	wrote := cancel != nil
	outcome := CancelOutcome{Wrote: wrote, Withdrew: !wrote}
	select {
	case <-rel:
		outcome.Settled = true
		return outcome, nil
	case <-ctx.Done():
		return outcome, ctx.Err()
	case <-s.done:
		return outcome, nil
	}
}

// Close ends the session: the event signal closes first, so no emit can
// block on a reader that has gone; a live turn is cancelled, the harness —
// and with it the transcript — is closed, which waits for the turn (its
// partial answer is persisted interrupted, as for Cancel), and then the
// turn's continuation is waited for. A prompt claimed and not yet open is not
// waited for (its continuation may be due on the caller's own goroutine); it
// finds the session closed when it runs and sends nothing.
//
// The harness's Close is what waits for the turn, not the turn's own
// cancel: it tells a running tool the session is closing, so a command is
// killed at once rather than given the grace an ordinary cancel gets, and
// Close returns within about 3 s even while one runs (plan 019 §7.7).
//
// It is safe before Start and idempotent, and it returns nil: there is no
// agent process whose exit ErrAgentExited could report. A failure to close
// the transcript is a diagnostic, not a reason to fail the caller's shutdown.
//
// The event log closes last, as on the live session (plan 020 §3.5), with
// s.mu released: the continuation this does not wait for, and anything else
// that emits late, is refused by the log from then on.
func (s *nativeSession) Close() error {
	s.closeOnce.Do(func() {
		defer close(s.closeDone)
		s.mu.Lock()
		s.closed = true
		close(s.done)
		hs, in, cancel, rel := s.hs, s.inPrompt, s.turnCancel, s.released
		worker := s.wakeDone
		s.mu.Unlock()
		// The close order, in the one place it is decided (plan 023 §3.5, X10):
		// the turn's context, then the registry, then the harness, then the
		// log. The registry closing before the log is Plan 021's only
		// requirement; what puts the turn's context ahead of it is that a
		// parked ask answered `closing` while its tool's context was still
		// live would let the tool answer the model with an ordinary result,
		// and a request could go out for a closing session before the turn's
		// halted check saw t.ctx.Err(). The tools carry a belt for the same
		// window — they read `closing` and `cancelled` alike as aborted while
		// Env.Closing is closed (opencode's askStopped) — and this is the
		// brace.
		//
		// What it costs is that a native ask parked at Close ends `cancelled,
		// by call` where a live one ends `closing`: the tool's context is the
		// first thing to end, so Ask.Wait's own watch usually wins the race
		// with the registry's close. Nothing branches on the difference —
		// neither the engine nor the TUI reads an ask's outcome for any of the
		// three kinds — and the guarantee Plan 021 makes for ACP sessions is
		// untouched.
		live := in && cancel != nil && rel != nil
		if live {
			cancel()
		}
		s.asks.Close()
		if hs != nil {
			if err := hs.Close(); err != nil {
				s.note(sanitizeLine(err.Error()))
			}
		}
		if live {
			<-rel
		}
		// The wake worker is joined once the turn it may have been running
		// has released — a wake is a turn to the two waits above, and its
		// ending is what lets the worker see done (native_wake.go). It is
		// started in the section that installs the harness, so a Close that
		// found no harness finds no worker either.
		if worker != nil {
			<-worker
		}
		// The turn's own ending settled its rows; this settles the rows of
		// a claim that never opened one. Nothing is published — done is
		// closed, so every emit is a no-op by now — but a Snapshot taken
		// after Close must not show a call still running (plan 019 §3.10).
		s.settleTools()
		// Last, after every teardown that can still emit: the log's close
		// cuts admission, ends every subscription and closes the journal
		// (plan 020 §3.5).
		s.log.Close(context.Background())
	})
	<-s.closeDone
	return nil
}

// SetModel switches the model the next turn runs on. It is allowed while a
// turn runs, as on the live session: the running turn finishes on its own
// model and the snapshot shows the new one at once. The harness builds the
// new model's client now, so an unknown alias or a missing key fails here
// and leaves the current model in place. A switch that took is announced
// (announceCurrent), because the new model can bring or take away the effort
// option.
func (s *nativeSession) SetModel(_ context.Context, cause, modelID string) (SetOutcome, error) {
	s.mu.Lock()
	hs, table := s.hs, s.table
	models := s.snap.Models
	loading := s.loading
	s.mu.Unlock()
	// A load's harness is installed before its replay, and the session is not
	// started until the end bracket (s.loading). loading only ever goes down
	// once the harness is set, so a setter that passed this check enqueues
	// behind the end bracket.
	if hs == nil || loading {
		return SetOutcome{}, fmt.Errorf("agent: session not started")
	}
	alias, err := MatchModel(Snapshot{Models: models}, modelID)
	if err != nil {
		return SetOutcome{}, fmt.Errorf("native: %v", err)
	}
	if err := hs.SetModel(alias); err != nil {
		return SetOutcome{}, phraseSetupError(err, table, alias)
	}
	// The confirmed model is the harness's own, captured where the snapshot
	// took it: MatchModel resolves an alias — a prefix, a display name — to a
	// canonical id, so the value that was asked for and the value the delta
	// carries are not always the same string (SetOutcome).
	model, _, t, err := s.announceCurrent(cause)
	if err != nil {
		return SetOutcome{}, err
	}
	return SetOutcome{Value: model, Ticket: t}, nil
}

// announceCurrent republishes the current model and its effort option after a
// switch, as one EventMeta carrying both sections and no Text — so
// `craze prompt --json` still prints nothing for it. It is the native session's
// config_option_update: an ACP agent answers a model or effort change with one,
// the live session turns it into exactly this event (live.go's onUpdate), and
// the TUI re-reads the snapshot on it. Without it a /model with no effort —
// whose command returns no message of its own — left the TUI's snapshot on the
// old model's options, so a switch from a model with no efforts to one with
// them never showed the effort, and the next `/model <id> <effort>` could not
// tell the effort from the model name.
//
// The mutation and the delta are **one** locked section (plan 021 §3.8): until
// C10 the setter mutated and this re-locked to read it back, so two switches
// could publish in the other order. Enqueuing rather than emitting is what
// makes that possible — the outbox's mutex is a leaf, so nothing blocks under
// s.mu — and the caller learns the delta's Seq from the ticket.
//
// Plugins are not announced: a native session resolves them once in Start and
// they never change again (plan 021 X3), so there is nothing to say.
// It answers with the two values it published — the model, and the effort the
// harness resolved — read inside the locked section that captured them, which
// is what lets a setter answer its caller with the CONFIRMED value rather than
// with the one that was asked for (SetOutcome, r23 finding 4). A second read
// afterwards could see another client's switch and answer this caller about it.
//
// A Close that ran between the harness taking the switch and this section is
// seen here, under the same lock Close's transition takes — announceMode's
// guard, for the same window (r6 finding 1, plan 025 §1): a closed session's
// snapshot is not rewritten and nothing is enqueued into a log that has
// stopped admitting, so the setter answers "closed" rather than a success
// whose delta went nowhere.
func (s *nativeSession) announceCurrent(cause string) (model, effort string, t *Ticket, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return "", "", nil, fmt.Errorf("agent: session closed")
	}
	s.refreshCurrentLocked()
	model = s.snap.CurrentModel
	for _, opt := range s.snap.Config {
		if opt.ID == nativeEffortID {
			effort = opt.Current
			break
		}
	}
	return model, effort, s.enqueueDeltaLocked(cause, Event{}, &StateDelta{
		Model:  &model,
		Config: &ConfigState{Options: cloneConfig(s.snap.Config)},
	}), nil
}

// enqueueDeltaLocked is live.go's, for this session: one EventMeta carrying st,
// stamped with its time as emit stamps what it publishes, enqueued under s.mu
// in the section that changed the snapshot. s.mu is held.
//
// It does not read s.replaying. A load's install delta is part of the replay
// (plan 028 §3.4), as live.go's is, and says so through base.Replayed, set by
// the one caller that enqueues it (load): an enqueue is delivered whenever the
// drainer reaches it, so a stamp taken from the flag could ride on a delta
// that lands after the end bracket (astra r1-c3 F1).
func (s *nativeSession) enqueueDeltaLocked(cause string, base Event, st *StateDelta) *Ticket {
	base.Type = EventMeta
	base.State = st
	base.Cause = cause
	if base.At.IsZero() {
		base.At = time.Now()
	}
	return s.log.EnqueueTicket(base)
}

// SetMode puts the session in mode modeID: what the tool gate enforces from
// the next call it judges, and what the model is told at the turn's next step
// boundary (plan 023 §3.1, §3.6). Like SetModel it is allowed while a turn
// runs — the harness owns that boundary — and a switch the harness refuses,
// entering plan mode when the plan file cannot be created among them, leaves
// the snapshot exactly as it was.
//
// The id goes through craze's own mode vocabulary against the list this
// session advertises (ResolveMode), so `/plan`, Shift+Tab and a client's
// set_mode all land here exactly as they do on cursor, whose three ids these
// are. The harness is asked outside s.mu, as SetModel asks it: entering plan
// mode touches the file system.
func (s *nativeSession) SetMode(_ context.Context, cause, modeID string) (SetOutcome, error) {
	s.mu.Lock()
	hs, seam := s.hs, s.modeSeam
	ids := modeIDs(s.snap.Modes)
	loading := s.loading
	s.mu.Unlock()
	if hs == nil || loading { // SetModel's refusal, for its reason
		return SetOutcome{}, fmt.Errorf("agent: session not started")
	}
	id, err := nativeResolveMode(modeID, ids)
	if err != nil {
		return SetOutcome{}, err
	}
	if err := hs.SetMode(id); err != nil {
		return SetOutcome{}, phraseModeError(err, id)
	}
	if seam != nil {
		seam()
	}
	mode, t, err := s.announceMode(cause)
	if err != nil {
		return SetOutcome{}, err
	}
	return SetOutcome{Value: mode, Ticket: t}, nil
}

// announceMode publishes the mode the harness is now in as one EventMeta
// carrying the Mode section and no Text — the live session's SetMode exactly
// (live.go), and announceCurrent's shape for the mode instead of the model.
//
// The mutation and the delta are one locked section (plan 021 §3.8), and the
// value is read back from the HARNESS inside it rather than taken from what
// this caller asked for: two setters racing each other must each answer with
// what the harness holds, which is the rule announceCurrent states for the
// model (SetOutcome, r23 finding 4). Event.Mode stays empty, because that is
// what an *agent*-initiated update fills and a client retires a plan offer on
// it (plan 021 correction 20). s.hs is read under s.mu, as refreshCurrentLocked
// reads it: the harness's own mode lock is a leaf and waits on nothing.
//
// A Close that ran between the harness taking the switch and this section is
// seen here, under the same lock that Close's transition takes: the snapshot
// of a closed session is not rewritten and nothing is enqueued into a log that
// has stopped admitting, so the setter answers "closed" rather than a success
// whose delta went nowhere (r6 finding 1).
func (s *nativeSession) announceMode(cause string) (string, *Ticket, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return "", nil, fmt.Errorf("agent: session closed")
	}
	mode := nativeCurrentMode(s.hs.Mode())
	s.snap.CurrentMode = mode
	return mode, s.enqueueDeltaLocked(cause, Event{}, &StateDelta{Mode: &mode}), nil
}

// phraseModeError is a refused mode switch in the adapter's words: a closed
// session reads as every other closed-session error does, and everything else
// — the plan file that could not be created — names the mode that was asked
// for, the way phraseSetupError names the model.
func phraseModeError(err error, id string) error {
	if errors.Is(err, harness.ErrClosed) {
		return &nativeError{msg: "agent: session closed", cause: err}
	}
	return &nativeError{msg: fmt.Sprintf("native: mode %q: %s", id, sanitizeLine(err.Error())), cause: err}
}

// SetConfig sets the effort, the one option the native session advertises;
// any other id is unsupported. Like SetModel it is allowed during a turn, and
// a change that took is announced the same way.
//
// forModel is checked against the model read in the same section as the
// effort levels (Session): on this session only SetModel moves the model, and
// the engine's worker runs one Set at a time, so the check is the worker's own
// made again — what it adds is that a caller without an engine is held to the
// binding too.
func (s *nativeSession) SetConfig(_ context.Context, cause, id, value, forModel string) (SetOutcome, error) {
	if id != nativeEffortID {
		return SetOutcome{}, ErrUnsupported
	}
	s.mu.Lock()
	hs, table := s.hs, s.table
	alias := s.snap.CurrentModel
	levels := s.efforts[alias]
	loading := s.loading
	s.mu.Unlock()
	if hs == nil || loading { // SetModel's refusal, for its reason
		return SetOutcome{}, fmt.Errorf("agent: session not started")
	}
	if forModel != "" && alias != forModel {
		return SetOutcome{}, ErrStaleModel
	}
	// Checked here as well as by the harness so the refusal can name what
	// the model does offer. "" is the model's default and always allowed.
	if value != "" && !slices.Contains(levels, value) {
		if len(levels) == 0 {
			return SetOutcome{}, fmt.Errorf("native: model %q has no effort control", alias)
		}
		return SetOutcome{}, fmt.Errorf("native: model %q offers effort %s, not %q", alias, strings.Join(levels, ", "), value)
	}
	if err := hs.SetEffort(value); err != nil {
		return SetOutcome{}, phraseSetupError(err, table, alias)
	}
	// The confirmed effort, not the one asked for: "" means the model's own
	// default, and what the harness resolved it to is what the delta carries
	// (SetOutcome, r23 finding 4).
	_, effort, t, err := s.announceCurrent(cause)
	if err != nil {
		return SetOutcome{}, err
	}
	return SetOutcome{Value: effort, Ticket: t}, nil
}

// SetTitle is /rename: local, pinned against the title the first prompt would
// otherwise give the session, and said in a Title delta with no Event.Text —
// the live session's shape exactly (live.go's SetTitle), including the check
// for room that is atomic with the mutation because no provider is asked.
//
// It needs no harness, so before Start it takes as it always has; while a
// load runs it is refused as not started, as every setter is (s.loading):
// the title a load shows is the row's, seeded before the bracket.
func (s *nativeSession) SetTitle(cause, title string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loading {
		return fmt.Errorf("agent: session not started")
	}
	if !s.log.OutboxRoom() {
		return ErrSetUnavailable
	}
	clean := sanitizeText(title)
	s.snap.Title = clean
	s.titlePinned = true
	s.enqueueDeltaLocked(cause, Event{}, &StateDelta{Title: &clean})
	return nil
}

func (s *nativeSession) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.snap
	// The parent's tool rows and then the roster, one lock after the other and
	// never one inside the other (plan 026 §3.9, panel P17). Each is its own
	// deep copy: a child's rows are events only, as the live session keeps a
	// grok child's.
	out.Tools = s.toolRows()
	out.Subagents = s.rosterRows()
	out.Models = append([]ModelInfo(nil), s.snap.Models...)
	// Cloned as the live session clones it (live.go): the menu reads the rows
	// off a snapshot and the expansion lookup is built from the session's own
	// copy, so handing out the backing array would let a consumer that sorted
	// or filtered its rows change what a typed name resolves to.
	out.Plugins = append([]PluginCommand(nil), s.snap.Plugins...)
	out.Config = cloneConfig(s.snap.Config)
	// Cloned for the same reason, and for one of its own: the list the tasks
	// panel draws is replaced whole by every todo_write (applyTodos), and a
	// consumer that sorted or filtered the array it was handed would be
	// changing what the next Snapshot answers with (live.go does the same).
	out.Todos = append([]Todo(nil), s.snap.Todos...)
	return out
}

// ForeignTurn is the Session leaf accessor (plan 021 §3.3): true while a wake
// runs — the one turn native starts without a craze prompt, delivering a
// background sub-agent's result (native_wake.go, plan 026 §3.11) — and false
// otherwise. It is set and cleared in the same s.mu sections that claim and
// release the wake and enqueue its brackets, so the engine, which reads it
// under e.mu → s.mu before every Begin, sees the wake exactly when Begin would
// be refused for it; and s.snap.ForeignTurn moves with it, so Snapshot()
// answers the same.
func (s *nativeSession) ForeignTurn() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.foreign
}

// FenceUp and FenceDown are agent.AdmissionFence (plan 026 §3.11): the engine
// above this seam says it may be about to claim a turn, or has one, or owes a
// drain — and, on FenceDown, that it is idle again. Each takes s.mu only to
// record the state, under the engine's own lock (e.mu → s.mu, the order
// ForeignTurn and Begin keep), and never calls back; FenceDown kicks the wake
// worker without waiting, since a result that became pending while the fence
// was up is the worker's to deliver now (native_wake.go). Idempotent: the
// engine calls them on transitions, and a repeated call changes nothing.
func (s *nativeSession) FenceUp() {
	s.mu.Lock()
	s.fenced = true
	s.mu.Unlock()
}

func (s *nativeSession) FenceDown() {
	s.mu.Lock()
	s.fenced = false
	s.mu.Unlock()
	s.kickWake()
}

// Interject merges text into the running turn: the harness takes it up before
// the turn's next step, the model reads it there, and the step that saw it
// writes it to the transcript (plan 019 §3.10, D-34).
//
// It is refused in exactly the three states the live session refuses in
// (live_queue.go) — no turn running, the turn's ending event already out, and
// a cancel in progress — and on a closed session, which has no turn to merge
// into and no consumer left to show the text to. Refusing keeps the text in
// the caller's hands, which is where the user can see it. A wake is refused
// as no turn (ErrNotInTurn, native_wake.go): it is not a craze turn, and no
// continuation of it hands what it could not answer back to the engine —
// the prompt's does, through Result.Unanswered — so a steer taken into a
// wake that ended before its next step would be neither answered nor
// returned. The engine queues the text instead, behind the wake.
//
// The turn the text was typed into is named, not merely counted: the token is
// read in the same locked section that judged that turn live and handed
// straight to the harness, so the window between releasing s.mu and the accept
// — in which that turn can end and the next begin — refuses instead of
// letting the next turn take the text up. Without it the text would be neither
// written to the turn it was meant for nor returned by it, and its EventUser
// would arrive after that turn's EventDone.
//
// Nothing is emitted here. The turn's own goroutine reports the steer through
// the sink (harness.Steered), which is what makes it impossible for the
// interjection to reach a consumer after the turn's ending event, and lets
// Interject hold no lock while a consumer is being written to.
// It is journaled by the same wrapper the live session uses (plan 020 §3.5),
// so the text the user sent and what became of it — accepted, refused, or
// offered to a turn that had already ended — are both on the record, under an
// attempt id of the wrapper's own rather than any id the harness mints.
func (s *nativeSession) Interject(ctx context.Context, text string) error {
	a := s.log.beginAttempt(journal.PromptKindInterject, text)
	err := s.interject(ctx, text)
	a.end("", err)
	return err
}

func (s *nativeSession) interject(_ context.Context, text string) error {
	s.mu.Lock()
	supported := s.snap.Provider.Capabilities().Interject
	hs, closed := s.hs, s.closed
	live := s.inPrompt && !s.doneEmitted && !s.cancelling && !s.wake
	var turn harness.SteerToken
	if hs != nil && live {
		// Read here, with the liveness it belongs to: a token read after the
		// lock went could already name the next turn.
		//
		// The adapter marks itself live a moment before the harness opens the
		// turn's box, so a read caught in between is 0 and Steer refuses it:
		// a false ErrNotInTurn for an interjection typed in that instant,
		// never a misroute, and the caller keeps its text.
		turn = hs.SteerToken()
	}
	// Resolved under the same lock as the liveness above, for prompt()'s
	// reason: the rows and their entries are one fact.
	refs := s.refsLocked(text)
	sessionID := s.snap.SessionID
	s.mu.Unlock()
	switch {
	case !supported:
		return ErrUnsupported
	case closed:
		return fmt.Errorf("agent: session closed")
	case hs == nil:
		return fmt.Errorf("agent: session not started")
	case !live:
		return ErrNotInTurn
	}
	// An interjection expands by the same rules a prompt does, so the same
	// text does not mean two different things depending on whether the turn
	// happened to be running when it was sent: a refused interjection comes
	// back to the caller and is queued, and a queued row is drained through
	// Begin, which expands it.
	//
	// No EventCommand is published for the expansion, unlike the prompt path.
	// There it is announced before hs.Run under the turn's own context, which
	// is what keeps it ahead of the turn's events; here the turn is already
	// running and emitting, so an announcement made from this goroutine could
	// land anywhere among them — after the model's answer to the very block it
	// describes, or after the turn's ending. The expansion still reaches the
	// model, and the row the user sees is the EventUser the sink emits, which
	// is the only ordering that keeps an interjection ahead of that ending.
	sent, _ := nativePrompt(text, refs, sessionID, hs.Redact)
	// Paired before the steer, because the turn's goroutine may echo it back
	// through the sink before Steer has even returned here.
	s.rememberSteer(sent, text)
	err := hs.Steer(turn, sent)
	if err == nil {
		return nil
	}
	// Refused: nothing will ever be echoed or returned for it, so the pairing
	// goes rather than sit out the turn. Everything below is one error's
	// phrasing, which is why the steer is tested once rather than again per
	// case.
	s.dropSteer(sent)
	switch {
	case errors.Is(err, harness.ErrNotInTurn):
		return ErrNotInTurn
	case errors.Is(err, harness.ErrTooManySteers):
		// The turn has taken as many as the queue could hold if it answered
		// none of them. Said in the queue's own words, because that is what
		// the unanswered ones become.
		return ErrQueueFull
	default:
		return phraseTurnError(err)
	}
}

// emit delivers ev unless the session is closing, exactly as the live
// session's emitCtx does with no caller context: a reader that stopped
// draining can hold a turn back, but never Close. It is emitCtx with the
// background context and the answer thrown away, the shape live.go's own emit
// has, so that the two can never drift on what "delivered" means.
func (s *nativeSession) emit(ev Event) { s.emitCtx(context.Background(), ev) }

// emitCtx is emit under a caller's context, and it publishes exactly one kind
// of event with one: the EventCommand a prompt's expansion produces, before
// the turn has been handed to the harness. That event goes out while the
// turn's cancel func is already registered, so a consumer that has stopped
// draining would otherwise hold a prompt the user cancelled with Esc — where
// every other event of a turn can only hold back the turn. The live session
// publishes its own EventCommand for the same reason (live.go).
//
// Delivery is the event log's Publish, on this goroutine, so a call that
// returned true has its event in Events()'s buffer; Publish reads ev before it
// waits, so the payload must be the caller's own. s.mu is never held here.
//
// It is never a terminal event's path. EventDone and EventError stay on emit,
// because a session flushes its log's outbox before publishing one and a
// terminal event that could be abandoned on a cancelled context would leave
// that barrier with nothing to stand on.
//
// It reports whether the event reached the primary's buffer. A false answer is
// not a reason to stop the turn: the caller runs it anyway, and a cancelled
// context makes the harness return StopCancelled at once, which is the one
// ending the prompt then emits.
func (s *nativeSession) emitCtx(ctx context.Context, ev Event) bool {
	if ev.At.IsZero() {
		ev.At = time.Now()
	}
	// Everything a load's replay publishes says so (plan 028 §3.4), as
	// live.go's emitCtx stamps it.
	if s.replaying.Load() {
		ev.Replayed = true
	}
	select {
	case <-s.done:
		s.log.Abandoned()
		return false
	default:
	}
	return s.log.Publish(ctx, s.done, ev)
}

// emitLossy is emit for an event whose whole point is to be current: it
// gives the event up rather than wait for a reader. Only a tool's progress
// goes out this way, and only because the harness requires it — a sink that
// may block must deliver a ToolProgress without blocking or drop it (plan
// 019 §3.5) — and because the next snapshot repeats the whole output
// anyway, so a drop costs a frame and nothing else.
//
// It publishes through the log's TryPublish, which is that contract written
// into the ordering boundary (plan 020 §3.1): the event is dropped for
// everyone at once — the primary, every subscription and the journal — and
// consumes no sequence number, so a dropped progress frame can never leave a
// hole a reconnecting client would have to reason about. Sending on the
// channel directly would have bypassed the numbering entirely.
func (s *nativeSession) emitLossy(ev Event) {
	if ev.At.IsZero() {
		ev.At = time.Now()
	}
	select {
	case <-s.done:
		s.log.Abandoned()
		return
	default:
	}
	s.log.TryPublish(ev)
}

// nativeError is an error the adapter phrased itself. Its text is what the
// user reads — the TUI's error line, craze prompt --json's error.message —
// and it unwraps to the harness's typed error, so a caller can still match
// harness.ErrAuth and the rest with errors.Is. What it wraps carries no key
// and no raw response body: the harness keeps only the status and the
// provider's message, scrubbed (plan 018 §3.5, §3.7).
type nativeError struct {
	msg   string
	cause error
}

func (e *nativeError) Error() string { return e.msg }
func (e *nativeError) Unwrap() error { return e.cause }

// phraseTurnError is a failed turn in the adapter's own words (plan 018
// §3.8): the harness's typed errors each get a sentence that says what to do,
// and the provider's own message, already bounded to one line by the harness,
// is sanitized before a terminal shows it.
func phraseTurnError(err error) error {
	phrase := func(msg string) error { return &nativeError{msg: msg, cause: err} }
	if errors.Is(err, harness.ErrEmptyStep) {
		return phrase("native: the model ended its answer without sending anything " +
			"(a reasoning model can spend its whole output ceiling thinking; raise max_output_tokens in models.toml)")
	}
	if errors.Is(err, harness.ErrEmptyPrompt) {
		return phrase("native: nothing to send: the prompt is empty")
	}
	if errors.Is(err, harness.ErrClosed) {
		return phrase("agent: session closed")
	}
	var pe *harness.ProviderError
	if !errors.As(err, &pe) {
		return phrase("native: the turn failed: " + sanitizeLine(err.Error()))
	}
	provider, model := sanitizeLine(pe.Provider), sanitizeLine(pe.Model)
	status := ""
	if pe.StatusCode != 0 {
		status = fmt.Sprintf(" (HTTP %d)", pe.StatusCode)
	}
	switch {
	case errors.Is(err, harness.ErrAuth):
		return phrase(fmt.Sprintf("native: provider %q rejected the API key%s; check its env_keys or api_key in providers.toml",
			provider, status))
	case errors.Is(err, harness.ErrModelNotFound):
		return phrase(fmt.Sprintf("native: provider %q does not serve model %q%s; check its wire_model in models.toml",
			provider, model, status))
	case errors.Is(err, harness.ErrContextTooLarge):
		return phrase(fmt.Sprintf("native: the conversation no longer fits model %q's context window%s; start a new session (compaction arrives with H7)",
			model, status))
	}
	msg := fmt.Sprintf("native: provider %q failed%s", provider, status)
	if m := sanitizeLine(pe.Message); m != "" {
		msg += ": " + m
	}
	return phrase(msg)
}

// phraseSetupError is Open's, SetModel's or SetEffort's error in the adapter's
// words. A missing key names the variables to set (their names, never a
// value), which is the actionable part; table is nil when the caller has
// none to hand, and the harness's own text is used then.
func phraseSetupError(err error, table *modeltable.Table, alias string) error {
	switch {
	case errors.Is(err, harness.ErrClosed):
		return &nativeError{msg: "agent: session closed", cause: err}
	case errors.Is(err, harness.ErrNoAPIKey) && table != nil:
		return &nativeError{msg: noKeyText(table, alias), cause: err}
	}
	return &nativeError{msg: fmt.Sprintf("native: model %q: %s", alias, sanitizeLine(err.Error())), cause: err}
}

// phraseLoadError is a load's Open error in the adapter's words (plan 028
// §3.4): the session that could not be resumed, and why. An explicit --model
// that fails on its own — no key — is phraseSetupError's, since the model is
// what the user has to fix; anything else, the store's refusals of the file
// and ErrResumeModel's list of the models tried included, names the session.
// The harness's own "resume <id>" prefix is dropped rather than said twice.
// It unwraps to the harness's error (store.ErrCorrupt and the rest reach
// errors.Is through it).
func phraseLoadError(err error, table *modeltable.Table, alias, id string) error {
	if alias != "" && errors.Is(err, harness.ErrNoAPIKey) {
		return phraseSetupError(err, table, alias)
	}
	if errors.Is(err, harness.ErrClosed) {
		return &nativeError{msg: "agent: session closed", cause: err}
	}
	why := strings.TrimPrefix(sanitizeLine(err.Error()), "harness: resume "+sanitizeLine(id)+": ")
	return &nativeError{msg: fmt.Sprintf("native: session %q cannot be resumed: %s", sanitizeLine(id), why), cause: err}
}

// noKeyText says that alias's provider has no key and how to give it one.
func noKeyText(table *modeltable.Table, alias string) string {
	m := table.Models[alias]
	how := "add an api_key for it to providers.toml"
	if envs := table.Providers[m.Provider].EnvKeys; len(envs) > 0 {
		how = "set " + sanitizeLine(strings.Join(envs, " or ")) + ", or " + how
	}
	return fmt.Sprintf("native: model %q has no API key: its provider %q has none; %s", alias, m.Provider, how)
}

var (
	_ Session        = (*nativeSession)(nil)
	_ EventSource    = (*nativeSession)(nil)
	_ LogOwner       = (*nativeSession)(nil)
	_ Clocked        = (*nativeSession)(nil)
	_ AdmissionFence = (*nativeSession)(nil)
)
