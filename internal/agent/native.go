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
	// the queue's own lock → the harness's lock (Current, under s.mu), and
	// emitMu → the event log's publishing boundary, a leaf that is never
	// taken with s.mu held: a publish blocked on a full primary under s.mu
	// would stop Close from closing done, which is what releases it.
	queueOp sync.Mutex
	emitMu  sync.Mutex
	queue   PromptQueue

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
	efforts     map[string][]string
	snap        Snapshot
	titlePinned bool
	claimed     bool
	inPrompt    bool
	cancelling  bool
	// turnCancel cancels the open turn's context; nil until the continuation
	// opens its turn. released is the claim's: closed when its continuation
	// returns, which is what Cancel and Close wait on.
	turnCancel context.CancelFunc
	released   chan struct{}
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
	log := NewEventLog(EventLogOptions{})
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

// Start loads the model table, opens the harness on the requested model (or
// the table's default) and publishes the first snapshot. It does no network
// I/O: the first request goes out with the first prompt.
func (s *nativeSession) Start(context.Context) error {
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

	hs, table, err := s.open()
	if err != nil {
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

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		// Close ran while the table was loading and found no harness to
		// close; this one is closed here so it cannot outlive the session.
		_ = hs.Close()
		return fmt.Errorf("agent: session closed")
	}
	s.hs = hs
	s.table = table
	s.efforts = efforts
	s.snap.Models = infos
	s.snap.SessionID = hs.ID()
	s.refreshCurrentLocked()
	return nil
}

// open resolves everything Start needs and opens the harness, returning it
// with the model table it was opened on. Its errors are already phrased for
// the user.
//
// The directory is paths.NativeDir(), and the table is loaded from it after
// tweak has run, so a test's tweak can point Home somewhere else or hand in a
// Table outright — the frame runner isolates HOME, so a golden cannot count
// on files under it.
func (s *nativeSession) open() (*harness.Session, *modeltable.Table, error) {
	if s.opts.LoadSessionID != "" {
		// Native sessions are never indexed (plan 018 §3.4), so no row can
		// ask for one; a hand-edited index row is refused, not silently
		// started fresh.
		return nil, nil, errors.New("agent: native does not support session/load yet")
	}
	if s.opts.Mode != "" {
		// The CLI refuses --ask/--plan with an in-process provider as a usage
		// error; this is the same refusal for any other caller, because
		// silently ignoring a requested plan mode would be worse than failing.
		return nil, nil, fmt.Errorf("native: mode %q is not supported: the native harness has no modes yet", s.opts.Mode)
	}
	ws, err := nativeWorkspace(s.opts.Workspace)
	if err != nil {
		return nil, nil, err
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
		return nil, nil, errors.New("native: there is no craze directory to read the model table from (set HOME or CRAZE_HOME)")
	}
	if hopts.Table == nil {
		table, err := modeltable.Load(hopts.Home)
		if errors.Is(err, modeltable.ErrNotConfigured) {
			return nil, nil, errNoModels
		}
		if err != nil {
			return nil, nil, fmt.Errorf("native: %w", err)
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
			return nil, nil, fmt.Errorf("native: %v (models.toml has %s)", err, strings.Join(table.Aliases(), ", "))
		}
		hopts.Model = alias
	case hopts.Model == "":
		alias, err := s.fundedModel(table, hopts.Getenv)
		if err != nil {
			return nil, nil, err
		}
		hopts.Model = alias
	}

	hs, err := harness.Open(hopts)
	if err != nil {
		return nil, nil, phraseSetupError(err, table, hopts.Model)
	}
	return hs, table, nil
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

// Begin claims the prompt slot now, on the caller's goroutine, with the live
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
func (s *nativeSession) Begin(text string) func(context.Context) (Result, error) {
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

// prompt is Begin's continuation: one harness turn, ended the way the live
// session ends one (live.go's prompt) — success, or a cancel by Cancel or
// Close, is exactly one EventDone; failure, the caller's own context ending
// included (callerEnded), is exactly one EventError and no EventDone,
// followed by an EventQueue removal per queued row.
func (s *nativeSession) prompt(ctx context.Context, text string, rel chan struct{}) (Result, error) {
	// One release for the whole claim, whichever way it ends, and after the
	// turn's last event: a Cancel or Close waiting on rel then finds the
	// ending already emitted and the slot free.
	defer func() {
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
	s.turnCancel = cancel
	if !s.titlePinned && s.snap.Title == "" {
		s.snap.Title = nativeTitle(text)
	}
	s.mu.Unlock()

	res, err := hs.Run(turnCtx, text, s.sink)
	if errors.Is(err, harness.ErrInTurn) {
		// Unreachable: the claim admits one continuation at a time and each
		// Run returns before its claim is released. Said as the refusal it
		// would be, with no event, rather than as a failed turn.
		return Result{}, ErrPromptInFlight
	}
	var failed error
	switch {
	case err != nil:
		failed = phraseTurnError(err)
	case res.StopReason == harness.StopCancelled:
		failed = s.callerEnded(ctx)
	}
	if failed != nil {
		// The error goes out first, so a consumer is already in its error
		// state when the removals arrive and can say why the queue emptied.
		s.emit(Event{Type: EventError, Err: failed})
		// Nothing drains from an error state, and a queue that outlived one
		// would run behind whatever the user sends next.
		s.clearQueueOnError()
		return Result{}, failed
	}
	s.emit(Event{Type: EventDone, StopReason: res.StopReason})
	return Result{StopReason: res.StopReason}, nil
}

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
// terminal-escape path into the TUI. StepDone and Retrying have no agent
// event in H1 and are dropped: usage goes to the transcript only, and the
// harness allows one silent retry (plan 018 §3.8). It runs synchronously on
// Fantasy's callbacks, which is why emit gives up once Close has begun.
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
	}
}

// Cancel stops the claimed prompt and waits for it, as the live session's
// does: a turn in flight has its context cancelled and Cancel returns once
// the continuation has returned — its EventDone already emitted, any partial
// answer persisted interrupted — or ctx ends; a prompt claimed and not yet
// open withdraws when its continuation runs, and Cancel waits for that. With
// nothing claimed it is a no-op.
func (s *nativeSession) Cancel(ctx context.Context) error {
	s.mu.Lock()
	if s.hs == nil {
		s.mu.Unlock()
		return nil
	}
	// The mark, the claim and the turn are read in one locked section, the
	// one the continuation's opening also takes: a claimed prompt either sees
	// the mark and withdraws or has already registered its cancel func here.
	s.cancelling = true
	claimed, cancel, rel := s.claimed, s.turnCancel, s.released
	s.mu.Unlock()
	if !claimed || rel == nil {
		return nil
	}
	if cancel != nil {
		cancel()
	}
	select {
	case <-rel:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-s.done:
		return nil
	}
}

// Close ends the session: the event signal closes first, so no emit can
// block on a reader that has gone; a live turn is cancelled and waited for —
// its partial answer is persisted interrupted, as for Cancel — and then the
// harness, and with it the transcript, is closed. A prompt claimed and not
// yet open is not waited for (its continuation may be due on the caller's own
// goroutine); it finds the session closed when it runs and sends nothing.
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
		if in && cancel != nil && rel != nil {
			cancel()
			<-rel
		}
		if hs != nil {
			if err := hs.Close(); err != nil {
				s.note(sanitizeLine(err.Error()))
			}
		}
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
	// s.mu → the queue's lock is the order every queue transaction takes.
	out.Queue = s.queue.List()
	out.Models = append([]ModelInfo(nil), s.snap.Models...)
	out.Config = cloneConfig(s.snap.Config)
	return out
}

// Interject is deferred to H2 (plan 018 §3.4): an H1 turn has no tool step to
// merge text into, and queueing already runs a follow-up next.
func (s *nativeSession) Interject(context.Context, string) error { return ErrUnsupported }

// The harness has no tools in H1, so nothing ever asks for permission, asks a
// question or proposes a plan.

func (s *nativeSession) AnswerPermission(string, string) error { return ErrUnsupported }

func (s *nativeSession) AnswerQuestion(string, map[string][]string, bool) error {
	return ErrUnsupported
}

func (s *nativeSession) AnswerPlan(string, bool) error { return ErrUnsupported }

// emit delivers ev unless the session is closing, exactly as the live
// session's emitCtx does with no caller context: a reader that stopped
// draining can hold a turn back, but never Close. Delivery is the event log's
// Publish, on this goroutine, so an emit that returned has its event in
// Events()'s buffer; Publish reads ev before it waits, so the payload must be
// the caller's own. s.mu is never held here.
func (s *nativeSession) emit(ev Event) {
	if ev.At.IsZero() {
		ev.At = time.Now()
	}
	select {
	case <-s.done:
		return
	default:
	}
	s.log.Publish(context.Background(), s.done, ev)
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
		return phrase(fmt.Sprintf("native: the conversation no longer fits model %q's context window%s; start a new session",
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
)
