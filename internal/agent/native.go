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
	"github.com/charliek/craze/internal/journal"
	"github.com/charliek/craze/internal/paths"
	"github.com/charliek/craze/internal/version"
)

// The native adapter is agent.Session over craze's own harness (plan 018
// §3.8): no process, no wire, a model from ~/.craze/native's table. The live
// session is its behavioural contract — the claim, the withdraw, error XOR
// done, the queue guards, a Cancel that waits — and tui.Stub its structural
// template. It is the first file in internal/agent to import internal/paths
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
	// done is closed first by Close, so no emit — the harness's sink
	// included, which runs on Fantasy's callbacks — can block on a reader
	// that has gone.
	done chan struct{}

	closeOnce sync.Once
	closeDone chan struct{}

	// The queue's transactions are ordered exactly as the live session's
	// (live_queue.go): queueOp orders the mutation, emitMu is held across its
	// emits after queueOp is released. Lock order: queueOp → emitMu → s.mu →
	// toolMu → the queue's own lock → the harness's lock (Current, under
	// s.mu). No lock is ever held across an emit (plan 019 §3.10), and
	// emitMu → the event log's publishing boundary, a leaf that is never
	// taken with s.mu held: a publish blocked on a full primary under s.mu
	// would stop Close from closing done, which is what releases it.
	//
	// Interject adds no ordering: it reads the turn state under s.mu, releases
	// it, and then hands the text to the harness's steer box, which takes a
	// leaf lock of its own. So an interjection never waits on the turn it is
	// meant for, even while that turn is emitting into a consumer that is slow
	// — or that is the very goroutine interjecting.
	queueOp sync.Mutex
	emitMu  sync.Mutex
	queue   PromptQueue

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
// the moment Run hands it over, not to whatever is downstream of it today. The
// caller that puts these rows in the queue is being rewritten in parallel to
// return them from Prompt instead, and a translation done here survives that
// where one woven into the requeue would not.
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

	hs, table, ws, err := s.open()
	if err != nil {
		// Nothing has been assigned yet — the scan below runs only once Open
		// has succeeded — so a session whose harness would not open has no
		// plugins either, and the menu of the next attempt is built from
		// scratch.
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
	// Discovery is filesystem work — three sources, hundreds of files — and it
	// runs out here for the reason the tables above do: s.mu is the lock a
	// consumer's Snapshot takes, and holding it across a walk of the owner's
	// whole .claude tree would stall every frame of the first second.
	//
	// ws is the workspace the harness itself opened on, not a second
	// os.Getwd(): a session whose content came from one directory and whose
	// tools ran in another would be a bug nobody could see.
	entries := discoverNative(resolveNativeSources(ws, s.contentHome()), s.contentWarn())
	// Redacted before anything is named or stored: a description and a
	// when-to-use travel into the menu, the snapshot and the journal by a road
	// the block's own redaction never touches, and a skill with no frontmatter
	// description has one taken from its body, where a provider key can be
	// (redactNativeEntries).
	entries = redactNativeEntries(entries, hs.Redact)
	// Naming runs over every entry, hidden ones included, so a visible row's
	// spelling never depends on what is hidden; taken is nil because native
	// advertises no commands of its own (ResolvePluginNames adds craze's
	// builtins itself), and provisional is false because there is no catalog
	// still to arrive that could rename a row — and therefore no catalog wait.
	rows := visibleNativeRows(entries, ResolvePluginNames(entries, nil, false))

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
	s.mu.Unlock()
	// Noted with s.mu released, as every note is (plan 020 §3.5); a Close in
	// that window drops it, which noteSession accepts and counts.
	s.log.noteSession(journal.SessionNote{ProviderSessionID: hs.ID()})
	return nil
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
// It is also where native says what it does not honour. --plugin-dir is
// cursor-agent's flag by another route and native reads no directory but the
// three of §3.2, so a session handed one must say so rather than start with a
// menu quietly missing what the user asked for (A14).
func (s *nativeSession) contentWarn() func(string) {
	// diagWriter rather than the fallback written out again: its own comment
	// already names this lane, and a third spelling of "Diag, else Stderr" is
	// a third place to miss when the lane grows a sink.
	diag := diagWriter(s.opts)
	warn := func(msg string) {
		if diag == nil {
			return
		}
		fmt.Fprintln(diag, msg)
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
// with the model table it was opened on and the workspace it opened on — the
// third is returned rather than worked out again because nativeWorkspace falls
// back to os.Getwd(), and a second call could answer differently. Its errors
// are already phrased for the user.
//
// The directory is paths.NativeDir(), and the table is loaded from it after
// tweak has run, so a test's tweak can point Home somewhere else or hand in a
// Table outright — the frame runner isolates HOME, so a golden cannot count
// on files under it.
func (s *nativeSession) open() (*harness.Session, *modeltable.Table, string, error) {
	if s.opts.LoadSessionID != "" {
		// Native sessions are never indexed (plan 018 §3.4), so no row can
		// ask for one; a hand-edited index row is refused, not silently
		// started fresh.
		return nil, nil, "", errors.New("agent: native does not support session/load yet")
	}
	if s.opts.Mode != "" {
		// The CLI refuses --ask/--plan with an in-process provider as a usage
		// error; this is the same refusal for any other caller, because
		// silently ignoring a requested plan mode would be worse than failing.
		return nil, nil, "", fmt.Errorf("native: mode %q is not supported: the native harness has no modes yet", s.opts.Mode)
	}
	ws, err := nativeWorkspace(s.opts.Workspace)
	if err != nil {
		return nil, nil, "", err
	}
	hopts := harness.Options{
		Home:      paths.NativeDir(),
		Workspace: ws,
		Version:   version.Version,
	}
	if s.tweak != nil {
		s.tweak(&hopts)
	}
	if hopts.Home == "" {
		return nil, nil, "", errors.New("native: there is no craze directory to read the model table from (set HOME or CRAZE_HOME)")
	}
	if hopts.Table == nil {
		table, err := modeltable.Load(hopts.Home)
		if errors.Is(err, modeltable.ErrNotConfigured) {
			return nil, nil, "", errNoModels
		}
		if err != nil {
			return nil, nil, "", fmt.Errorf("native: %w", err)
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
			return nil, nil, "", fmt.Errorf("native: %v (models.toml has %s)", err, strings.Join(table.Aliases(), ", "))
		}
		hopts.Model = alias
	case hopts.Model == "":
		alias, err := s.fundedModel(table, hopts.Getenv)
		if err != nil {
			return nil, nil, "", err
		}
		hopts.Model = alias
	}

	hs, err := harness.Open(hopts)
	if err != nil {
		return nil, nil, "", phraseSetupError(err, table, hopts.Model)
	}
	return hs, table, hopts.Workspace, nil
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
// included (callerEnded), is exactly one EventError and no EventDone,
// followed by an EventQueue removal per queued row.
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
	if failed != nil {
		// The error goes out first, so a consumer is already in its error
		// state when the removals arrive and can say why the queue emptied.
		s.emit(Event{Type: EventError, Err: failed})
		// Nothing drains from an error state, and a queue that outlived one
		// would run behind whatever the user sends next. This is the session's
		// own queue, which only `craze prompt` still fills (--follow-up) until
		// the queue leaves the provider seam; the engine's is cleared by its own
		// chain policy.
		s.clearQueueOnError()
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

// The turn's unanswered steers used to be pushed back onto this session's own
// queue here (queueUnanswered, plan 019 §3.10). They are reported in
// Result.Unanswered instead, and the engine above the seam puts them back —
// through PushFront, last first, in the same locked section that decides the
// turn's successor, so steered text is always ahead of whatever was waiting
// behind the turn that took it (plan 021 §3.5).
//
// Two things follow from the move. The wedge this comment used to describe is
// gone from the requeue: it was queueTx holding emitMu across a blocking
// primary send, with a consumer that queues from its own receive on the other
// side, and the engine holds no lock across a send at all — it enqueues into the
// log's outbox, whose mutex is a leaf. What remains of that shape is the
// session's own queue verbs, which only `craze prompt` still uses and which the
// queue's move off the provider seam deletes. And the closed-session window is
// gone with it: a row can no longer be pushed onto a queue whose events are
// already suppressed, because nothing pushes one here.

// callerEnded says whether a turn the harness reports cancelled was ended by
// the caller's own context — its deadline, or its cancel — rather than by
// this session's Cancel or Close, and if so returns the failure it is. The
// harness cannot tell the two apart: any end of the turn's context is a
// clean cancel to it. The live session can, and treats the first as a failed
// prompt (live.go's prompt: the client's error is EventError and the queue
// is cleared), so the adapter does too; its own Cancel and Close stay the
// user's "stop", one EventDone{cancelled} with the queue kept. A cancel of
// ours that lands alongside the caller's counts as ours. nil when the cancel
// was ours.
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
func nativeTitle(prompt string) string {
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
func (s *nativeSession) SetModel(_ context.Context, modelID string) error {
	s.mu.Lock()
	hs, table := s.hs, s.table
	models := s.snap.Models
	s.mu.Unlock()
	if hs == nil {
		return fmt.Errorf("agent: session not started")
	}
	alias, err := MatchModel(Snapshot{Models: models}, modelID)
	if err != nil {
		return fmt.Errorf("native: %v", err)
	}
	if err := hs.SetModel(alias); err != nil {
		return phraseSetupError(err, table, alias)
	}
	s.announceCurrent()
	return nil
}

// announceCurrent republishes the current model and its effort option after
// a switch, then emits one bare EventMeta — no Text, so `craze prompt --json`
// prints nothing for it — outside s.mu, through the close-aware emit. It is
// the native session's config_option_update: an ACP agent answers a model or
// effort change with one, the live session turns it into exactly this event
// (live.go's onUpdate), and the TUI re-reads the snapshot on it. Without it a
// /model with no effort — whose command returns no message of its own —
// left the TUI's snapshot on the old model's options, so a switch from a
// model with no efforts to one with them never showed the effort, and the
// next `/model <id> <effort>` could not tell the effort from the model name.
//
// It runs on the caller's goroutine, which for SetModel and SetConfig is a
// command goroutine in the TUI (never Update) and may therefore wait on a
// full channel like any other emit; Close releases it.
func (s *nativeSession) announceCurrent() {
	s.mu.Lock()
	s.refreshCurrentLocked()
	s.mu.Unlock()
	s.emit(Event{Type: EventMeta})
}

// SetMode is unsupported: the harness has no modes until H5.
func (s *nativeSession) SetMode(context.Context, string) error { return ErrUnsupported }

// SetConfig sets the effort, the one option the native session advertises;
// any other id is unsupported. Like SetModel it is allowed during a turn, and
// a change that took is announced the same way.
func (s *nativeSession) SetConfig(_ context.Context, id, value string) error {
	if id != nativeEffortID {
		return ErrUnsupported
	}
	s.mu.Lock()
	hs, table := s.hs, s.table
	alias := s.snap.CurrentModel
	levels := s.efforts[alias]
	s.mu.Unlock()
	if hs == nil {
		return fmt.Errorf("agent: session not started")
	}
	// Checked here as well as by the harness so the refusal can name what
	// the model does offer. "" is the model's default and always allowed.
	if value != "" && !slices.Contains(levels, value) {
		if len(levels) == 0 {
			return fmt.Errorf("native: model %q has no effort control", alias)
		}
		return fmt.Errorf("native: model %q offers effort %s, not %q", alias, strings.Join(levels, ", "), value)
	}
	if err := hs.SetEffort(value); err != nil {
		return phraseSetupError(err, table, alias)
	}
	s.announceCurrent()
	return nil
}

// SetTitle is /rename: local, emitting nothing, and pinned against the title
// the first prompt would otherwise give the session.
func (s *nativeSession) SetTitle(title string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snap.Title = sanitizeText(title)
	s.titlePinned = true
}

func (s *nativeSession) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.snap
	// s.mu → toolMu → the queue's lock is the order every transaction takes.
	out.Tools = s.toolRows()
	out.Queue = s.queue.List()
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

// The harness has no permission gate, questions or plans in H2, so nothing
// ever asks for one.

func (s *nativeSession) AnswerPermission(string, string) error { return ErrUnsupported }

func (s *nativeSession) AnswerQuestion(string, map[string][]string, bool) error {
	return ErrUnsupported
}

func (s *nativeSession) AnswerPlan(string, bool) error { return ErrUnsupported }

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

// queueTx is the live session's queue transaction (live_queue.go): fn
// mutates under queueOp, and its events go out after queueOp is released,
// under emitMu, so a consumer waiting on something that holds queueOp cannot
// wedge the session while the transactions still reach the stream whole and
// in order.
func (s *nativeSession) queueTx(fn func() []QueueEvent) {
	s.queueOp.Lock()
	s.emitMu.Lock()
	defer s.emitMu.Unlock()
	evs := func() []QueueEvent {
		defer s.queueOp.Unlock()
		return fn()
	}()
	for _, ev := range evs {
		s.emit(ev.Event())
	}
}

func (s *nativeSession) Queue(text string) (p QueuedPrompt, err error) {
	s.queueTx(func() []QueueEvent {
		var ev QueueEvent
		p, ev, err = s.queue.Add(text, time.Now())
		if err != nil {
			return nil
		}
		return []QueueEvent{ev}
	})
	return p, err
}

func (s *nativeSession) EditQueued(id, text string) (err error) {
	s.queueTx(func() []QueueEvent {
		var ev QueueEvent
		ev, err = s.queue.Edit(id, text)
		if err != nil {
			return nil
		}
		return []QueueEvent{ev}
	})
	return err
}

func (s *nativeSession) Unqueue(id string) (p QueuedPrompt, ok bool) {
	s.queueTx(func() []QueueEvent {
		var ev QueueEvent
		ev, ok = s.queue.Remove(id)
		if !ok {
			return nil
		}
		p = ev.Prompt
		return []QueueEvent{ev}
	})
	return p, ok
}

// TakeQueued hands a row to the caller to prompt, under the live session's
// guard: refused while a prompt is claimed or in flight.
func (s *nativeSession) TakeQueued(id string) (p QueuedPrompt, ok bool) {
	s.queueTx(func() []QueueEvent {
		ev, taken := s.takeGuarded(func() (QueueEvent, bool) { return s.queue.Take(id) })
		if !taken {
			return nil
		}
		p, ok = ev.Prompt, true
		return []QueueEvent{ev}
	})
	return p, ok
}

// PopQueue is TakeQueued of the head, under the same guard.
func (s *nativeSession) PopQueue() (p QueuedPrompt, ok bool) {
	s.queueTx(func() []QueueEvent {
		ev, taken := s.takeGuarded(s.queue.Pop)
		if !taken {
			return nil
		}
		p, ok = ev.Prompt, true
		return []QueueEvent{ev}
	})
	return p, ok
}

// takeGuarded is the guard and the removal as one critical section. A claimed
// prompt counts as in flight before its turn opens, as on the live session:
// Begin would refuse the row's prompt then. The caller holds queueOp.
func (s *nativeSession) takeGuarded(take func() (QueueEvent, bool)) (QueueEvent, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inPrompt || s.claimed {
		return QueueEvent{}, false
	}
	return take()
}

func (s *nativeSession) ClearQueue() int {
	n := 0
	s.queueTx(func() []QueueEvent {
		evs := s.queue.Clear()
		n = len(evs)
		return evs
	})
	return n
}

// clearQueueOnError empties the queue when a turn fails, as the live session
// does: a queue that survived an error would run behind the next prompt.
func (s *nativeSession) clearQueueOnError() {
	s.ClearQueue()
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
