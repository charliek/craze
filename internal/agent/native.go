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
	// session and empty: nothing in the harness opens an ask yet (Asks).
	asks *AskRegistry
	// done is closed first by Close, so no emit — the harness's sink
	// included, which runs on Fantasy's callbacks — can block on a reader
	// that has gone.
	done chan struct{}

	closeOnce sync.Once
	closeDone chan struct{}

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
	// harness.
	toolMu    sync.Mutex
	tools     map[string]ToolEvent
	toolOrder []string

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
	claimed     bool
	inPrompt    bool
	cancelling  bool
	// doneEmitted marks that the turn's ending event has gone out while the
	// slot is still claimed, as on the live session: Interject is refused from
	// then on, so an interjection can never follow an EventDone.
	doneEmitted bool
	// turnCancel cancels the open turn's context; nil until the continuation
	// opens its turn. released is the claim's: closed when its continuation
	// returns, which is what Cancel and Close wait on.
	turnCancel context.CancelFunc
	released   chan struct{}
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
	}
	// Whatever provider the caller named, this session is the native one, and
	// every snapshot — one taken before Start included — says so.
	s.snap.Provider = NativeProvider().Info()
	// Beside the log, as on every session, and empty: nothing in the harness
	// opens an ask yet. It exists so that a client above the seam holds one
	// surface whatever the provider is — Control.Asks answers with nothing here
	// rather than with "unsupported" — and so that a Gate's Ask has somewhere to
	// go when H3 gives it one, through this adapter and nowhere else.
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

// Asks is the session's AskSource (plan 021 §3.6): an empty registry, because
// the harness has no permission gate, questions or plans yet.
func (s *nativeSession) Asks() *AskRegistry { return s.asks }

// Start loads the model table, opens the harness on the requested model (or
// the table's default) and publishes the first snapshot. It does no network
// I/O: the first request goes out with the first prompt.
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
	s.mu.Unlock()

	hs, table, content, err := s.open()
	if err != nil {
		// Nothing has been assigned yet — the content below is assigned only
		// once Open has succeeded — so a session whose harness would not open
		// has no plugins either, and the menu of the next attempt is built
		// from scratch.
		s.mu.Lock()
		s.started = false
		s.mu.Unlock()
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
		s.mu.Unlock()
		return fmt.Errorf("agent: session closed")
	}
	s.hs = hs
	s.table = table
	s.efforts = efforts
	s.plugins = entries
	s.snap.Plugins = rows
	s.snap.Models = infos
	s.snap.SessionID = hs.ID()
	s.refreshCurrentLocked()
	// The install says what it installed, in the section that installed it:
	// the model the harness opened on, the effort option that model brings,
	// and the plugin rows — which a native session resolves once here and
	// never again (X3). Without it a client folding the stream would have to
	// call Snapshot() to learn the session's starting state, which is the gap
	// r23 finding 2 is about. The title is not touched here and native's
	// first-prompt title stays silent (X47), so no Title section, and
	// Event.Mode/Event.Text stay empty: starting is nobody's agent update.
	model := s.snap.CurrentModel
	s.enqueueDeltaLocked("", Event{}, &StateDelta{
		Model:   &model,
		Config:  &ConfigState{Options: cloneConfig(s.snap.Config)},
		Plugins: &PluginsState{Plugins: append([]PluginCommand(nil), s.snap.Plugins...)},
	})
	s.mu.Unlock()
	// Outside the lock, as the live session's install flushes: "emitted means
	// buffered" when Start returns, so a client's first read of the stream is
	// the session's starting state rather than a race with the first turn.
	_ = s.log.Flush(context.Background(), s.done)
	// Noted with s.mu released, as every note is (plan 020 §3.5); a Close in
	// that window drops it, which noteSession accepts and counts.
	s.log.noteSession(journal.SessionNote{ProviderSessionID: hs.ID()})
	s.notePromptSources(hs, content.extras)
	return nil
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
func (s *nativeSession) open() (*harness.Session, *modeltable.Table, nativeLoad, error) {
	var none nativeLoad
	if s.opts.LoadSessionID != "" {
		// Native sessions are never indexed (plan 018 §3.4), so no row can
		// ask for one; a hand-edited index row is refused, not silently
		// started fresh.
		return nil, nil, none, errors.New("agent: native does not support session/load yet")
	}
	if s.opts.Mode != "" {
		// The CLI refuses --ask/--plan with an in-process provider as a usage
		// error; this is the same refusal for any other caller, because
		// silently ignoring a requested plan mode would be worse than failing.
		return nil, nil, none, fmt.Errorf("native: mode %q is not supported: the native harness has no modes yet", s.opts.Mode)
	}
	ws, err := nativeWorkspace(s.opts.Workspace)
	if err != nil {
		return nil, nil, none, err
	}
	hopts := harness.Options{
		Home:      paths.NativeDir(),
		Workspace: ws,
		Version:   version.Version,
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
	case hopts.Model == "":
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
	content := loadNativeContent(
		resolveNativeSources(hopts.Workspace, s.contentHome()),
		s.opts.Compat,
		keys,
		s.contentWarn(redact.New(keys...)),
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
	if err != nil {
		return nil, nil, none, phraseSetupError(err, table, hopts.Model)
	}
	return hs, table, content, nil
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
// The continuation must be run exactly once, and is safe if it is not: a
// second call, after the first or alongside it, returns ErrPromptInFlight and
// touches nothing — the claim, its release and the turn all belong to the
// first call, and running them twice would send the prompt again and then
// close the claim's release a second time.
func (s *nativeSession) claim(text string) func(context.Context) (Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
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
	// One release for the whole claim, whichever way it ends, and after the
	// turn's last event: a Cancel or Close waiting on rel then finds the
	// ending already emitted and the slot free. The turn's steer pairings go
	// with it, before s.mu is taken rather than under it, so steerMu stays a
	// leaf with no lock ordering of its own.
	defer func() {
		s.forgetSteers()
		s.mu.Lock()
		s.claimed, s.inPrompt, s.cancelling = false, false, false
		s.turnCancel = nil
		if s.released == rel {
			s.released = nil
		}
		close(rel)
		s.mu.Unlock()
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
	if hs == nil {
		s.mu.Unlock()
		return Result{}, fmt.Errorf("agent: session not started")
	}
	// The turn opens in the same locked section that checked for a cancel,
	// and registers its cancel func there, so a Cancel either marked the
	// claim above or finds the turn here: never neither.
	turnCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	s.inPrompt = true
	s.doneEmitted = false
	s.turnCancel = cancel
	if !s.titlePinned && s.snap.Title == "" {
		// The typed text, not the expansion: a session called after the whole
		// of a command file would say nothing about what the user asked for.
		// The journal's prompt note (Begin) keeps the typed text for the same
		// reason; only the store records what was actually sent.
		//
		// It publishes NOTHING, as it never has, and that is deliberate rather
		// than an omission (plan 021 C10). Every other change to shared state
		// this session makes now carries a StateDelta; this is the one change
		// no consumer has ever been told about, and telling them is a change of
		// behaviour rather than a change of mechanism. The TUI re-reads the
		// snapshot on any EventMeta, so a delta here would put this title in
		// the frame's header from the first prompt on, where the baseline shows
		// it only when something else happens to refresh — and S1b ships no
		// behaviour change. The golden that says so is native-echo-80x24, which
		// plan 021 X18 already caught this title moving once.
		//
		// The title is in Snapshot from here on, as it always was; a client
		// that folds the stream learns it when the phase that owns titles and
		// the session index does (C12, S1c).
		s.snap.Title = nativeTitle(text)
	}
	// The references are resolved in the same locked section as the rest of
	// the turn's state, as the live session resolves its own: the rows and the
	// entries they were resolved from have to be read together, or a prompt
	// could expand a body under a name the menu means something else by.
	refs := s.refsLocked(text)
	sessionID := s.snap.SessionID
	s.mu.Unlock()

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
	// Every session waits for the outbox before it publishes a turn's terminal
	// event (plan 021 §3.6), so anything enqueued while the turn ran is in the
	// record ahead of it. Native has no asks yet, so today this only orders what
	// a component above the seam enqueued; it is here because the rule is "every
	// session", not "every session that has asks".
	//
	// It is bounded by s.done, as every emit below it is: Close closes done and
	// then waits for this continuation (rel) BEFORE it closes the log, and with
	// the primary full and its reader stopped only the log's close frees the
	// drainer this barrier waits on — so a flush that did not escape here would
	// be a Close waiting for a goroutine waiting for that same Close.
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
// terminal-escape path into the TUI, and its tool events become the rows
// native_tools.go merges. StepDone, Retrying and Diag have no agent event
// and are dropped: usage goes to the transcript only, the harness allows
// one silent retry (plan 018 §3.8), and a Diag is for the journal and for
// diagnosis, not for a consumer (plan 019 §3.5). ToolProgress never blocks
// here, since the harness drops a snapshot rather than wait on it.
//
// It runs synchronously on Fantasy's callbacks — the tool ones on tool
// goroutines — which is why emit gives up once Close has begun.
func (s *nativeSession) sink(ev harness.Event) {
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
		s.toolStarted(e)
	case harness.ToolCalled:
		s.toolCalled(e)
	case harness.ToolProgress:
		s.toolProgress(e)
	case harness.ToolFinished:
		s.toolFinished(e)
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
	}
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
	s.mu.Unlock()
	if !claimed || rel == nil {
		return CancelOutcome{Settled: true}, nil
	}
	wrote := cancel != nil
	if cancel != nil {
		cancel()
	}
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
		s.mu.Unlock()
		// Before the log's close, as on the live session: the registry is empty
		// here, and closing it is what keeps the two sessions one shape.
		s.asks.Close()
		live := in && cancel != nil && rel != nil
		if live {
			cancel()
		}
		if hs != nil {
			if err := hs.Close(); err != nil {
				s.note(sanitizeLine(err.Error()))
			}
		}
		if live {
			<-rel
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
	s.mu.Unlock()
	if hs == nil {
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
	model, _, t := s.announceCurrent(cause)
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
func (s *nativeSession) announceCurrent(cause string) (model, effort string, t *Ticket) {
	s.mu.Lock()
	defer s.mu.Unlock()
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
	})
}

// enqueueDeltaLocked is live.go's, for this session: one EventMeta carrying st,
// stamped as emit stamps what it publishes, enqueued under s.mu in the section
// that changed the snapshot. s.mu is held.
func (s *nativeSession) enqueueDeltaLocked(cause string, base Event, st *StateDelta) *Ticket {
	base.Type = EventMeta
	base.State = st
	base.Cause = cause
	if base.At.IsZero() {
		base.At = time.Now()
	}
	return s.log.EnqueueTicket(base)
}

// SetMode is unsupported: the harness has no modes until H5.
func (s *nativeSession) SetMode(context.Context, string, string) (SetOutcome, error) {
	return SetOutcome{}, ErrUnsupported
}

// SetConfig sets the effort, the one option the native session advertises;
// any other id is unsupported. Like SetModel it is allowed during a turn, and
// a change that took is announced the same way.
func (s *nativeSession) SetConfig(_ context.Context, cause, id, value string) (SetOutcome, error) {
	if id != nativeEffortID {
		return SetOutcome{}, ErrUnsupported
	}
	s.mu.Lock()
	hs, table := s.hs, s.table
	alias := s.snap.CurrentModel
	levels := s.efforts[alias]
	s.mu.Unlock()
	if hs == nil {
		return SetOutcome{}, fmt.Errorf("agent: session not started")
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
	_, effort, t := s.announceCurrent(cause)
	return SetOutcome{Value: effort, Ticket: t}, nil
}

// SetTitle is /rename: local, pinned against the title the first prompt would
// otherwise give the session, and said in a Title delta with no Event.Text —
// the live session's shape exactly (live.go's SetTitle), including the check
// for room that is atomic with the mutation because no provider is asked.
func (s *nativeSession) SetTitle(cause, title string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
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
	out.Tools = s.toolRows()
	out.Models = append([]ModelInfo(nil), s.snap.Models...)
	// Cloned as the live session clones it (live.go): the menu reads the rows
	// off a snapshot and the expansion lookup is built from the session's own
	// copy, so handing out the backing array would let a consumer that sorted
	// or filtered its rows change what a typed name resolves to.
	out.Plugins = append([]PluginCommand(nil), s.snap.Plugins...)
	out.Config = cloneConfig(s.snap.Config)
	return out
}

// ForeignTurn is the Session leaf accessor (plan 021 §3.3). Native drives no
// ACP agent of its own, so nothing it runs is ever a turn craze did not ask
// for: this always answers false, taking s.mu only for the same reason
// Snapshot does — so a reader never observes a half-written s.snap.
func (s *nativeSession) ForeignTurn() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return false
}

// Interject merges text into the running turn: the harness takes it up before
// the turn's next step, the model reads it there, and the step that saw it
// writes it to the transcript (plan 019 §3.10, D-34).
//
// It is refused in exactly the three states the live session refuses in
// (live_queue.go) — no turn running, the turn's ending event already out, and
// a cancel in progress — and on a closed session, which has no turn to merge
// into and no consumer left to show the text to. Refusing keeps the text in
// the caller's hands, which is where the user can see it.
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
	live := s.inPrompt && !s.doneEmitted && !s.cancelling
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
	_ Session     = (*nativeSession)(nil)
	_ EventSource = (*nativeSession)(nil)
	_ LogOwner    = (*nativeSession)(nil)
	_ Clocked     = (*nativeSession)(nil)
)
