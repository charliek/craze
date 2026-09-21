package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/charliek/craze/internal/acp"
	"github.com/charliek/craze/internal/journal"
)

// session is the ACP-backed provider session.
//
// # Locks
//
// s.mu guards the fields below it and is never held across anything that
// blocks: a provider call, a publish, a flush, a wait. Two components below the
// seam have locks of their own and the order between them is fixed (plan 021
// §3.3):
//
//   - s.mu → the client's own mutex, for the one leaf read turnLiveLocked
//     makes. The client never calls back into the session under its own lock.
//   - registry.mu → the log's outbox mutex, which is a strict leaf. Every ask
//     event is enqueued in the registry section that changed the ask, so the
//     order of the registry's events is the order of its state.
//   - **s.mu and registry.mu are never nested, in either direction.** The
//     session calls BeginTurn, CancelTurn, EndTurn, Open, Automatic,
//     AnsweredEarly and Report with s.mu released — each takes a value read
//     under s.mu a moment earlier — and the registry calls nothing but its log
//     and its clock, so it can never reach back.
type session struct {
	opts   Options
	client *acp.Client
	// log is where every event goes (eventlog.go): emitCtx publishes into it
	// and Events is its primary. events is the same channel as Primary, kept
	// receive-only so that nothing can send on it past the log's numbering;
	// a few tests read it by name.
	log    *EventLog
	events <-chan Event
	// asks is every blocking request this session parks, answers and ends
	// (asks.go). It is built beside the log, because every ending it writes is
	// an event in this session's one sequence.
	asks *AskRegistry
	// askCtx is the context every ask this session opens is watched on: it ends
	// when the session closes, which is the only cancellation an ACP handler has
	// of its own. Closing resolves every parked ask as closing before this fires
	// (Close), so it is the backstop and not the mechanism; a harness Gate's Ask
	// will bring a real per-call context.
	askCtx  context.Context
	askStop context.CancelFunc
	// tee is the agent child's stderr on its way to opts.Stderr, copied into
	// the journal a line at a time (stderr.go). nil without a journal, and
	// then the child writes straight to opts.Stderr as it always did.
	tee  *stderrTee
	done chan struct{}

	closeOnce sync.Once
	closeDone chan struct{}
	// closeErr is client.Close()'s stored result: the sentinel wrapping
	// agent.ErrAgentExited when the agent's exit had been reaped before this
	// Close sampled it, nil otherwise. Every Close call after the first
	// returns this rather than nil, so a second caller sees the same answer
	// the first did (§3.7.3).
	closeErr error

	mu       sync.Mutex
	started  bool
	closed   bool
	inPrompt bool
	// claimed holds from Begin until the prompt it claimed has returned. It is
	// wider than inPrompt, which is the turn itself: a claimed prompt may not
	// have opened its turn yet, and a Cancel in that gap is still for it.
	claimed    bool
	promptDone chan struct{}
	// wire is the claimed prompt's wire outcome, set by Begin with the claim
	// and cleared when the claim ends: what Cancel waits on so that its
	// session/cancel can never reach the agent ahead of the prompt it stops.
	wire *turnWire
	// turn counts prompts, and token is the registry's name for the one that is
	// open: every ask this turn raises is parked against it, and the turn's end
	// — or a cancel — resolves exactly those. It is the no-turn token whenever
	// no prompt of craze's own has an open turn, which is what a request that
	// arrived between turns or during a turn the agent started itself parks
	// against. Both are written in the section that opens the turn and cleared
	// in the one that releases the claim.
	turn      int
	token     TurnToken
	snap      Snapshot
	sessionID string
	tools     map[string]ToolEvent
	toolOrder []string
	// taskReceipts holds cursor/task receipts whose tool_call has not landed
	// yet; it is bounded and cleared at turn end so an unmatched receipt can
	// never leak or attach itself to a later tool with a reused id.
	taskReceipts     map[string]TaskInfo
	taskReceiptOrder []string
	subagents        map[string]*subagentRec
	subagentOrder    []string
	// subagentFinishSeq stamps records in the order they finished, which is
	// not spawn order; the finished-record cap evicts by it.
	subagentFinishSeq uint64
	// doneEmitted marks that the turn is over — Prompt has returned, either
	// way. inPrompt is still true until Prompt's defer runs, so it alone
	// cannot say whether there is still a turn to interject into.
	doneEmitted bool
	// cancelling holds from Cancel until the claim it was for is released,
	// and Begin clears it, so a cancel asked before a claim is never that
	// prompt's. An interjection sent in that window is exactly what grok
	// strands.
	cancelling bool
	// foreign mirrors the client's foreign-turn state for Snapshot.
	foreign      bool
	interjectSeq int
	// plugins are the on-disk plugin entries discovered once, in Start,
	// before session/new; they never change again. commandsSeen records that
	// the agent's first available_commands_update has been applied, which is
	// what takes the resolved names out of their provisional all-qualified
	// spelling. Both are read under s.mu, because a Prompt can run against
	// them while an update is rewriting the resolution. commandsApplied is
	// commandsSeen as something to block on: it closes in the same locked
	// section that sets the flag, so a prompt that read the flag as false is
	// guaranteed to see the close.
	plugins         []PluginEntry
	commandsSeen    bool
	commandsApplied chan struct{}
	// catalogAbort is the abort channel of the one prompt parked in that wait,
	// nil when none is. It lives on the session because the prompt holding it
	// has opened no turn yet, so Cancel has nothing else to find it by.
	catalogAbort chan struct{}
	// titlePinned is /rename's pin: once set, an agent session_info_update no
	// longer replaces the title. Options.TitlePinned seeds it on a load, so
	// the pin survives any number of --continues. Guarded by s.mu.
	titlePinned bool
	// replayUser coalesces a replayed main-session user_message_chunk. The
	// agents split a prompt over several chunks and craze has to draw exactly
	// one user block, so the chunks accumulate here and go out as one
	// EventUser at the next non-user update or at replay end. Guarded by
	// s.mu.
	replayUser strings.Builder
	// replaying is true between the two EventReplay phases. It is written on
	// Start's goroutine and read on the client's read loop, and emitCtx —
	// which stamps Event.Replayed from it — is deliberately lock-free, so it
	// is an atomic rather than a field under s.mu.
	replaying atomic.Bool
}

// taskReceiptCap bounds the parked receipts of a single turn.
const taskReceiptCap = 32

// wireOutcome is what became of one turn's session/prompt: still on its way
// out, written, refused before the wire, or failed without reaching it.
type wireOutcome int

const (
	wirePending wireOutcome = iota
	// wireSent: the prompt's bytes were written. A write whose reply then
	// failed still counts: the agent has the prompt.
	wireSent
	// wireRefused: the client refused the prompt before the wire. The one
	// refusal Prompt can reach is ErrForeignTurn, and then a turn of the
	// agent's own is running — which is what a cancel would stop.
	wireRefused
	// wireFailed: nothing reached the agent — the write failed, the
	// connection was already closed, or the claimed prompt returned before
	// its turn opened without withdrawing.
	wireFailed
	// wireWithdrawn: the claimed prompt found itself cancelled before its
	// turn opened, and withdrew. Nothing was sent, and nothing will be.
	wireWithdrawn
)

// turnWire carries one prompt's wire outcome from the prompt, which learns it,
// to Cancel, which has to wait for it. Begin creates it with the claim, and the
// same value then carries the prompt through its turn's opening to the write.
// Each prompt gets its own: grok's writer can report a write after the turn it
// belongs to has ended, and a report into the old turn's value can then no
// longer speak for the next one.
type turnWire struct {
	outcome wireOutcome // written under s.mu, before done is closed
	done    chan struct{}
}

// publishWire settles a turn's wire outcome. The first outcome wins and later
// ones change nothing, so every path that learns something may say it: the
// written bytes, the client's return, and the claim's release. It takes only
// s.mu, sets one field and closes one channel: as the prompt's sent hook it
// runs inside acp's write path, where anything that blocked would hold the
// prompt's reply wait with it.
func (s *session) publishWire(w *turnWire, o wireOutcome) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w.publishLocked(o)
}

func (w *turnWire) publishLocked(o wireOutcome) {
	if w.outcome != wirePending {
		return
	}
	w.outcome = o
	close(w.done)
}

// outcomeOf reads a settled outcome. The close of done already orders the
// read after the write; the lock is for uniformity with every other field.
func (s *session) outcomeOf(w *turnWire) wireOutcome {
	s.mu.Lock()
	defer s.mu.Unlock()
	return w.outcome
}

// testAfterBinaryResolved runs, when set, between Start resolving the agent
// binary and the spawn that runs it: the one point where a test can change
// what that name resolves to next and see that nothing after it looks again.
// It is a var only so the tests can set it; nothing in craze writes it.
var testAfterBinaryResolved func()

// testBeforeWire runs, when set, right before Prompt hands its request to the
// client: the one point where a turn is open and its prompt is not yet on the
// wire, which a test can only hold still from here. It is a var only so the
// tests can set it; nothing in craze writes it.
var testBeforeWire func(s *session)

func New(opts Options) Session {
	if opts.Provider != nil && opts.Provider.InProcess() {
		return newNative(opts, nil)
	}
	return newSession(opts)
}

func newSession(opts Options) *session {
	s := &session{
		opts:      opts,
		done:      make(chan struct{}),
		closeDone: make(chan struct{}),
		tools:     make(map[string]ToolEvent),

		commandsApplied: make(chan struct{}),
		taskReceipts:    make(map[string]TaskInfo),
		subagents:       make(map[string]*subagentRec),
	}
	if opts.Provider != nil {
		p := *opts.Provider
		s.opts.Provider = &p
	}
	s.opts.PluginDirs = append([]string(nil), opts.PluginDirs...)
	// The log is built once the provider is settled, because a journal's
	// header names it, with the binary as it was asked for: which file that
	// name resolves to is Start's to settle (the session note).
	s.log = newSessionLog(s.opts, journalHeader{provider: s.provider().Name(), binary: s.opts.Binary})
	s.events = s.log.Primary()
	// Beside the log, and stamped from the same clock its own emits use.
	s.asks = NewAskRegistry(s.log, s.Now)
	s.askCtx, s.askStop = context.WithCancel(context.Background())
	// The tee is the child's lane alone. Options.Stderr stays what it was, so
	// craze's own notes about the session — which fall back to it when Diag is
	// unset — are still craze's and are never journaled as the agent's words.
	s.tee = newStderrTee(s.opts.Stderr, s.log)
	// The provider is decided once, here, so every snapshot — including one
	// taken before Start — names it.
	s.snap.Provider = s.provider().Info()
	return s
}

// provider is the agent behind this session; nil Options.Provider is cursor.
func (s *session) provider() Provider {
	if s.opts.Provider != nil {
		return *s.opts.Provider
	}
	return CursorProvider()
}

// clientRef reads the spawned client under the lock; it is nil before Start
// assigns it and after a concurrent Close.
func (s *session) clientRef() *acp.Client {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.client
}

func (s *session) Events() <-chan Event {
	return s.log.Primary()
}

// Subscribe and Incarnation are the session's EventSource: its log's.
func (s *session) Subscribe(o SubscribeOptions) (*Subscription, error) { return s.log.Subscribe(o) }
func (s *session) Incarnation() string                                 { return s.log.Incarnation() }

// EventLog is the session's LogOwner (plan 021 §3.3): the log a component above
// the seam publishes into, so that what it records lands in this session's one
// sequence, ahead of or behind the session's own events by nothing but order.
func (s *session) EventLog() *EventLog { return s.log }

// Now is the session's Clocked (plan 021 §3.9): the clock emitCtx stamps At
// from, so a caller above the seam stamps from the same one.
func (s *session) Now() time.Time { return time.Now() }

// Asks is the session's AskSource (plan 021 §3.6): the registry a client above
// the seam lists and answers through engine.Control.
func (s *session) Asks() *AskRegistry { return s.asks }

var (
	_ EventSource = (*session)(nil)
	_ LogOwner    = (*session)(nil)
	_ Clocked     = (*session)(nil)
	_ AskSource   = (*session)(nil)
)

// Start spawns the agent and sets the session up. Its body is start; what is
// here is the journal's half (plan 020 §3.5): a failure is noted before the
// session tears down what it built, because Close is the log's admission
// cutoff and a note written after it would be counted and dropped. teardown
// says the failure is one of those the body used to answer with an inner
// Close; the paths that only give the start back (unstart) keep doing that.
// A Close racing this start can cut that admission first, which
// noteStartFailed accepts and counts.
func (s *session) Start(ctx context.Context) error {
	teardown, err := s.start(ctx)
	if err == nil {
		return nil
	}
	s.log.noteStartFailed(err)
	if teardown {
		_ = s.Close()
	}
	return err
}

func (s *session) start(ctx context.Context) (teardown bool, _ error) {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return false, fmt.Errorf("agent: session already started")
	}
	if s.closed {
		s.mu.Unlock()
		return false, fmt.Errorf("agent: session closed")
	}
	s.started = true
	s.mu.Unlock()

	args := s.provider().Args(s.opts.ExtraArgs, s.opts.Force)

	cwd := s.opts.Workspace
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			s.unstart()
			return false, err
		}
	}
	cwd, err := filepath.Abs(cwd)
	if err != nil {
		s.unstart()
		return false, err
	}

	// The agent binary is resolved exactly once, here, and that one path is
	// both what is spawned and what the session note records (plan 020 §3.5).
	// Asking twice — once inside the spawn, once again for the note — was
	// enough for a PATH change, a changed CRAZE_AGENT_BIN or a candidate
	// swapped on disk between the two lookups to put a binary in the note that
	// the session never ran, or to leave the note empty when the second lookup
	// found nothing.
	//
	// Handing the answer back to Spawn as its Binary changes nothing about the
	// spawn: resolving is the first thing Spawn does, an explicit Binary is
	// what it resolves (so CRAZE_AGENT_BIN's precedence is applied here, in
	// the same call, and the candidates below it are unreachable either way),
	// and a resolved path resolves to itself — exec.LookPath returns an
	// executable it is handed by path unchanged, and an absolute path it
	// refuses still passes Spawn's own os.Stat fallback, which is where such a
	// path came from. Candidates are left out for that reason: with a resolved
	// Binary they can never be reached.
	binary, err := acp.ResolveBinaryCandidates(s.opts.Binary, s.provider().Bins())
	if err != nil {
		s.unstart()
		return false, err
	}
	if testAfterBinaryResolved != nil {
		testAfterBinaryResolved()
	}
	client, err := acp.Spawn(acp.SpawnOptions{
		Binary:  binary,
		Args:    args,
		Dir:     cwd,
		Env:     s.opts.Env,
		Stderr:  s.stderrSink(),
		Dialect: s.provider().Dialect(),
	})
	if err != nil {
		s.unstart()
		return false, err
	}
	// Close may have run while we were spawning; adopt the child only if the
	// session is still open, otherwise reap it here so it cannot be orphaned.
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = client.Close()
		return false, fmt.Errorf("agent: session closed")
	}
	s.client = client
	s.mu.Unlock()
	client.SetUpdateHandler(s.onUpdate)
	client.SetSubagentHandler(s.onSubagent)
	client.SetPermissionHandler(s.onPermission)
	client.SetAskHandler(s.onAskQuestion)
	client.SetPlanHandler(s.onCreatePlan)
	client.SetTodosHandler(s.onUpdateTodos)
	client.SetTaskHandler(s.onTaskReceipt)
	client.SetEarlyAnswerHandler(s.onEarlyAnswer)
	client.SetInterjectionHandler(func(n acp.InterjectionNotification) {
		s.onInterjection(interjectionText{Text: n.Text})
	})
	client.SetForeignTurnHandler(func(f acp.ForeignTurn) {
		s.onForeignTurn(ForeignTurnInfo{ID: f.ID, Text: f.Text, Running: f.Running})
	})

	initRes, err := client.Initialize(ctx)
	if err != nil {
		return true, err
	}
	if methodID, meta, ok := s.authMethod(initRes); ok {
		if err := client.Authenticate(ctx, methodID, meta); err != nil {
			return true, fmt.Errorf("%w (run `%s`)", err, s.provider().LoginHint())
		}
	} else if len(initRes.AuthMethods) > 0 && len(s.provider().AuthMethodIDs()) > 0 {
		// The daemon offered only methods craze will not start (interactive
		// browser login); say how to fix it instead of hanging later.
		return true, fmt.Errorf("agent: no supported auth method (run `%s`)", s.provider().LoginHint())
	}
	// The disk scan runs before session/new so the first snapshot the session
	// ever publishes already carries its plugins; a catalog update that beats
	// session/new home then has entries to resolve against.
	plugins := s.discoverPlugins(cwd)
	s.mu.Lock()
	s.plugins = plugins
	s.mu.Unlock()
	// A load replaces session/new entirely: there is no fallback, because a
	// silent new session is the one outcome a user who typed --continue must
	// never get (plan 013 §3.8).
	loading := s.opts.LoadSessionID != ""
	var snap Snapshot
	if loading {
		if err := s.loadSession(ctx, client, initRes, cwd, binary); err != nil {
			return true, err
		}
	} else {
		sess, err := client.NewSession(ctx, cwd)
		if err != nil {
			return true, err
		}
		snap = snapshotFromNewProvider(sess, s.provider(), initRes)
		snap.Provider = s.provider().Info()
		snap.SessionID = sess.SessionID
		s.mu.Lock()
		s.sessionID = sess.SessionID
		s.mu.Unlock()
		// Noted with s.mu released, as every note is (plan 020 §3.5); a
		// Close in that window drops it, which noteSession accepts and counts.
		s.log.noteSession(journal.SessionNote{ProviderSessionID: sess.SessionID, AgentBinary: binary})
	}
	// The --ask/--plan/--model tail, unchanged: it runs after session setup
	// either way. On a load that means after the restored snapshot has been
	// installed and the replay bracket closed, so it writes through s.mu
	// instead of into a snapshot that has not been published yet.
	modes := snap.Modes
	if loading {
		modes = s.Snapshot().Modes
	}
	if s.opts.Mode != "" {
		modeID, ok := ResolveMode(s.opts.Mode, modeIDs(modes))
		if !ok {
			return true, fmt.Errorf("agent: session did not advertise mode %q", s.opts.Mode)
		}
		if err := client.SetMode(ctx, modeID); err != nil {
			return true, err
		}
		if loading {
			s.mu.Lock()
			s.snap.CurrentMode = modeID
			s.mu.Unlock()
		} else {
			snap.CurrentMode = modeID
		}
	}
	if s.opts.Model != "" {
		if err := client.SetModel(ctx, s.opts.Model); err != nil {
			return true, err
		}
		if loading {
			s.mu.Lock()
			s.snap.CurrentModel = s.opts.Model
			s.mu.Unlock()
		} else {
			snap.CurrentModel = s.opts.Model
		}
	}
	if loading {
		return false, nil
	}
	s.mu.Lock()
	commands := s.snap.Commands
	s.snap = snap
	if len(s.snap.Commands) == 0 {
		s.snap.Commands = commands
	}
	// Same locked section as the carry-over on purpose: the names resolve
	// against whatever commands survived it, so an update that arrived early
	// is honoured and one that arrives next redoes the work.
	s.snap.Plugins = s.resolvePluginsLocked()
	s.mu.Unlock()
	return false, nil
}

// stderrSink is where the agent child's stderr goes: the journal's tee when
// this session has one, and Options.Stderr itself otherwise.
func (s *session) stderrSink() io.Writer {
	if s.tee == nil {
		return s.opts.Stderr
	}
	return s.tee
}

// loadSession is Start's session/load path: the replay, bracketed.
//
// The bracket is a phase, not a wire tag — cursor replays with no _meta at all
// (plan 013 §2.1) — and it closes only once the restored snapshot is in place,
// so EventReplay{end} means "the session is ready to read", not merely "the
// wire went quiet". Everything the replay accumulated on the way through — tool
// rows, sub-agent rows, todos, commands, the seeded title — is kept: the load
// result is merged into the snapshot rather than assigned over it, which is the
// whole difference from the session/new path above.
//
// binary is the agent binary Start resolved and spawned, for the session note,
// which is written here because this is where Start learns the id: after the
// replay, and before the end bracket.
func (s *session) loadSession(ctx context.Context, client *acp.Client, initRes *acp.InitializeResult, cwd, binary string) error {
	if !initRes.LoadSession() {
		// Nothing has reached the wire: an agent without the capability is
		// told so before the RPC rather than by its own error.
		return fmt.Errorf("agent: %s does not support session/load", s.provider().Name())
	}
	// The stored title and its pin are seeded *before* the replay. No agent
	// replays a session_info_update (plan 013 §2.1), so the index row craze
	// resolved the id from is the only place a resumed title can come from,
	// and the composer rule reads it from the first frame on.
	s.mu.Lock()
	if title := sanitizeText(s.opts.Title); title != "" {
		s.snap.Title = title
	}
	if s.opts.TitlePinned {
		s.titlePinned = true
	}
	s.mu.Unlock()

	s.emit(Event{Type: EventReplay, Replay: &ReplayInfo{Phase: ReplayStart}})
	s.replaying.Store(true)
	res, err := client.LoadSession(ctx, s.opts.LoadSessionID, cwd)
	if err != nil {
		// No end bracket: the load failed, so Start returns and closes the
		// client. Clearing the flag keeps a late notification from being
		// stamped replayed on its way into a channel nobody will read.
		s.replaying.Store(false)
		return err
	}
	// The last chunks of a replayed prompt have no update behind them to push
	// them out, so replay end is their flush.
	s.flushReplayUser()
	restored := snapshotFromNewProvider(res, s.provider(), initRes)
	s.mu.Lock()
	s.sessionID = res.SessionID
	// The merge. Models come from the result (else initialize's modelState),
	// modes from the result else the provider's fallback — grok and gx return
	// neither modes nor configOptions on a load — so the config chips are
	// simply absent after a resume, which plan 013 §4 documents. Tools,
	// Subagents, Todos, Commands, Plugins and Title keep whatever the replay
	// left behind.
	s.snap.Models = restored.Models
	s.snap.Modes = restored.Modes
	s.snap.Config = restored.Config
	s.snap.CurrentModel = restored.CurrentModel
	s.snap.CurrentMode = restored.CurrentMode
	s.snap.SessionID = res.SessionID
	s.snap.Provider = s.provider().Info()
	s.snap.Plugins = s.resolvePluginsLocked()
	s.mu.Unlock()
	// Noted with s.mu released, as every note is (plan 020 §3.5); a Close in
	// that window drops it, which noteSession accepts and counts.
	s.log.noteSession(journal.SessionNote{ProviderSessionID: res.SessionID, LoadedFrom: s.opts.LoadSessionID, AgentBinary: binary})
	s.replaying.Store(false)
	s.emit(Event{Type: EventReplay, Replay: &ReplayInfo{Phase: ReplayEnd}})
	return nil
}

// flushReplayUser emits the coalesced replayed prompt as exactly one EventUser.
// The builder is drained under the lock, so the read loop and Start racing to
// flush it cannot emit the same text twice.
func (s *session) flushReplayUser() {
	s.mu.Lock()
	text := s.replayUser.String()
	s.replayUser.Reset()
	s.mu.Unlock()
	if text == "" {
		return
	}
	s.emit(Event{Type: EventUser, Text: text})
}

// discoverPlugins runs the provider's plugin scan for this workspace. It never
// fails: a --plugin-dir that is not there, and one handed to a provider that
// reads its plugins off the wire instead, are each one line on the session's
// stderr. Plugins are an extra, and a session that refused to start over one
// would be strictly worse than a session without them.
func (s *session) discoverPlugins(workspace string) []PluginEntry {
	scan := s.provider().PluginScan()
	// Both of this closure's lines are craze's own — "plugin dirs ignored for
	// …" below and "plugin dir skipped: …" from the package function — so
	// both go to Diag, not the agent's own Stderr lane (§3.7.1).
	diag := s.opts.Diag
	if diag == nil {
		diag = s.opts.Stderr
	}
	warn := func(msg string) {
		if diag == nil {
			return
		}
		fmt.Fprintln(diag, msg)
	}
	if !scan.Dirs && len(s.opts.PluginDirs) > 0 {
		warn("plugin dirs ignored for " + s.provider().Name())
	}
	return discoverPlugins(scan, workspace, HomeDir(), s.opts.PluginDirs, warn)
}

// resolvePluginsLocked names the discovered entries against what the agent has
// advertised so far. s.mu must be held: the snapshot's plugin list and the
// commands it was resolved against have to change together, or a prompt reading
// one and the menu drawing the other would disagree about what a name means.
func (s *session) resolvePluginsLocked() []PluginCommand {
	if len(s.plugins) == 0 {
		return nil
	}
	taken := make([]string, 0, len(s.snap.Commands))
	for _, c := range s.snap.Commands {
		taken = append(taken, c.Name)
	}
	return ResolvePluginNames(s.plugins, taken, !s.commandsSeen)
}

// pluginLookupLocked is every spelling the resolved rows can be typed by. It is
// derived rather than kept, so there is no third thing to rewrite in step with
// the other two: the menu's rows and the wire's lookup are the same list read
// twice under the same lock.
func (s *session) pluginLookupLocked() map[string]pluginTarget {
	return buildPluginLookup(s.plugins, s.snap.Plugins)
}

// authMethod is the provider's pick against what initialize advertised: the
// first of its preferred methods that is offered and, when it needs an env
// key, backed by one. No advertised methods means no login.
func (s *session) authMethod(initRes *acp.InitializeResult) (string, map[string]any, bool) {
	return s.provider().authFor(initRes, s.childEnv())
}

// childEnv is the lookup for what the spawned agent will see: Options.Env
// replaces the parent environment when set, exactly as acp.Spawn applies
// it, and inherits it otherwise.
func (s *session) childEnv() func(string) string {
	if s.opts.Env == nil {
		return os.Getenv
	}
	env := s.opts.Env
	return func(key string) string {
		prefix := key + "="
		for i := len(env) - 1; i >= 0; i-- {
			if strings.HasPrefix(env[i], prefix) {
				return env[i][len(prefix):]
			}
		}
		return ""
	}
}

func (s *session) unstart() {
	s.mu.Lock()
	s.started = false
	s.mu.Unlock()
}

// catalogWait bounds the one wait of §3.3. Five seconds is longer than any
// catalog observed live and short enough that a prompt which will never be
// expanded is not held hostage to one. It is a var only so the tests can
// shorten it; nothing in craze writes it.
var catalogWait = 5 * time.Second

// awaitCatalog holds a prompt back, once and briefly, for the agent's first
// available_commands_update. Until that update is applied every plugin row
// shows its qualified spelling (§3.2), so a bare /probe-echo resolves to
// nothing — and a one-shot `craze prompt` sends before the update lands, which
// is how a live run put the draft on the wire verbatim and left the model to
// guess what /probe-echo meant.
//
// It waits only where waiting can change the answer: the first update has not
// been applied, the session has plugins at all, and the draft names something
// the lookup cannot answer yet. So a qualified spelling never waits, a draft
// with no slash in it never waits, grok — which has no plugins — never waits,
// and nothing waits again once the catalog has landed. Falling through the
// timeout is exactly the behaviour without this: resolve against whatever is
// known and send, which is safe, because an unexpanded /name reaches the agent
// as the text the user typed.
//
// The alternative — resolving bare names against the provisional catalog — is
// precisely the wrong-meaning window §3.2 exists to close, and is not done.
//
// A waiting prompt is cancellable: it registers an abort channel that Cancel
// closes, because the turn it would open does not exist yet and s.inPrompt —
// the only thing Cancel used to look at — is still false. The channel it
// registered comes back to Prompt, which clears it under the lock that opens
// the turn; nil means nothing waited.
//
// A registered wait reserves the prompt slot: a second Prompt arriving while
// one is parked is refused here with ErrPromptInFlight, the same answer
// s.inPrompt would give it a step later. The registration is a prompt that has
// not opened its turn yet, so without the reservation the second caller would
// overwrite s.catalogAbort — leaving Cancel able to close only the later
// channel while the first waiter woke on the timeout and went on to open a turn
// and reach the wire, which is the prompt Esc was pressed to stop. A second
// caller that would not have waited at all (a qualified name, plain text) is
// refused for the same reason: starting its turn while a waiter is parked
// leaves two prompts racing for one slot. Nothing has been spent at this point,
// so the refusal costs the refused caller nothing to roll back. Begin's claim
// now refuses such a caller a step earlier; this stays the wait's own guard.
//
// A prompt that has been cancelled since its claim does not park at all. The
// Cancel that marked it looked for a wait to abort in the same locked section
// and found none, so it is waiting on the wire instead, and a parked prompt
// would hold it there for the whole window. Registering nothing sends the
// prompt straight on to its opening, which withdraws it.
func (s *session) awaitCatalog(ctx context.Context, text string) (chan struct{}, error) {
	s.mu.Lock()
	if s.catalogAbort != nil {
		s.mu.Unlock()
		return nil, acp.ErrPromptInFlight
	}
	applied := s.commandsApplied
	var abort chan struct{}
	if !s.cancelling && !s.commandsSeen && len(s.plugins) > 0 && hasUnresolvedSlash(text, s.pluginLookupLocked()) {
		abort = make(chan struct{})
		s.catalogAbort = abort
	}
	s.mu.Unlock()
	if abort == nil {
		return nil, nil
	}
	timer := time.NewTimer(catalogWait)
	defer timer.Stop()
	select {
	case <-applied:
	case <-timer.C:
	case <-ctx.Done():
	case <-s.done:
	case <-abort:
	}
	return abort, nil
}

// clearCatalogWaitLocked takes the wait's registration back and reports whether
// Cancel closed it. It runs in the same locked section that marks the turn
// started, so an Esc can never land between the end of the wait and the
// bookkeeping: a Cancel either finds the registration — and this returns true,
// before the turn has been opened — or finds no wait and marks the prompt,
// which the same section then withdraws, or finds the turn and cancels that.
func (s *session) clearCatalogWaitLocked(abort chan struct{}) bool {
	if abort == nil {
		return false
	}
	if s.catalogAbort == abort {
		s.catalogAbort = nil
	}
	select {
	case <-abort:
		return true
	default:
		return false
	}
}

// abortCatalogWaitLocked ends a pending catalog wait, at most once, and reports
// whether it ended one: the registration is dropped under the same lock that
// closes it, so a second Cancel finds nothing to close and answers false. A
// prompt already on the wire is not this — that turn is the agent's to cancel —
// which is why a registration is only honoured while there is no turn in
// flight. The caller holds s.mu: Cancel, which has to look for the wait and
// mark the prompt cancelling in one locked section.
func (s *session) abortCatalogWaitLocked() bool {
	if s.inPrompt || s.catalogAbort == nil {
		return false
	}
	close(s.catalogAbort)
	s.catalogAbort = nil
	return true
}

// Prompt is Begin and its continuation back to back, on the caller's own
// goroutine. It claims nothing earlier than the prompt itself starts, so a
// Cancel from another goroutine can still land before the claim: headless
// `craze prompt`'s signal handler is such a caller, and keeps that narrower
// window. Only a caller that can claim in the step that shows the turn
// working — the TUI's Update — closes it, with Begin.
func (s *session) Prompt(ctx context.Context, text string) (Result, error) {
	return s.Begin(text)(ctx)
}

// Begin is the claim and the journal's record of it: the prompt note is
// written here, after the claim and with s.mu released, and the continuation
// that comes back writes the prompt_end its own (Result, error) says (plan 020
// §3.5). Wrapping the continuation rather than the prompt's body is what makes
// every ending a record: a claim that was refused, a prompt withdrawn before
// the wire and one cancelled in the catalog wait all return from here, and
// none of them reaches the body at all.
func (s *session) Begin(text string) func(context.Context) (Result, error) {
	run := s.claim(text)
	return s.log.wrapPrompt(journal.PromptKindPrompt, text, run)
}

// claim claims the prompt slot for text now, on the caller's goroutine, and
// returns the rest of the prompt to run. The TUI claims inside Update, in the
// same step that shows the turn working, and runs the continuation on a Cmd
// goroutine. Without the claim, an Esc that Update handled before that
// goroutine opened the turn found no turn at all and wrote its cancel at once,
// and the prompt followed it onto the wire — to an agent that drops a cancel
// for a turn it has not seen.
//
// The claim clears cancelling, because a cancel asked before it was for
// whatever ran then and not for this prompt, and it opens the prompt's wire
// outcome, still pending. A Cancel from here on waits on that outcome. If the
// continuation finds the prompt cancelling before its turn is open, it
// withdraws: it publishes wireWithdrawn and returns ErrPromptCancelled, and
// the Cancel writes nothing, because nothing reached the agent.
//
// A Begin while the slot is already claimed, or a turn is open, claims nothing
// and changes nothing: its continuation returns ErrPromptInFlight, the answer
// the session's gate has always given a second prompt, only given earlier. A
// claim that went ahead would replace the running prompt's wire and clear its
// cancel, and a Cancel for that prompt would then decide on the wrong one.
// Nothing in craze begins a prompt over a running one — the TUI queues while
// a turn is working and sends only once it has settled, by which time the
// claim has been released — so the refusal is the gate's, not a new one.
//
// The continuation must be run, once: the slot stays claimed until it returns.
func (s *session) claim(text string) func(context.Context) (Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.claimed || s.inPrompt {
		return func(context.Context) (Result, error) { return Result{}, acp.ErrPromptInFlight }
	}
	s.claimed = true
	s.cancelling = false
	wire := &turnWire{done: make(chan struct{})}
	s.wire = wire
	return func(ctx context.Context) (Result, error) { return s.prompt(ctx, text, wire) }
}

// prompt is the claim's continuation: the claimed prompt from its catalog wait to
// the end of its turn.
func (s *session) prompt(ctx context.Context, text string, wire *turnWire) (Result, error) {
	// One release for the whole claim, whichever way it ends. done is set once
	// the turn has opened; until then there is no turn to close, only the claim
	// to give back. For an opened turn the client's return has settled the
	// wire by now, and the publish here is the backstop for a prompt that ends
	// any other way — every return before the opening included — so that no
	// Cancel can outlive the claim waiting on its wire.
	var done chan struct{}
	defer func() {
		s.mu.Lock()
		wire.publishLocked(wireFailed)
		if s.wire == wire {
			s.wire = nil
		}
		s.claimed = false
		s.cancelling = false
		if done != nil {
			s.inPrompt = false
			// The turn is no longer the one an ask can be parked against: a
			// request that arrives now belongs to no turn of craze's own.
			s.token = TurnToken{}
			s.clearTaskReceiptsLocked()
			close(done)
		}
		s.mu.Unlock()
	}()
	// Before any of the turn's bookkeeping: this waits, and a turn that has
	// been opened must not be left open across a wait — no turn is open yet,
	// so a caller that gives up here gives up only its claim. Its refusal comes
	// back the same way: the slot was already another prompt's, and returning
	// here spends no turn number and rolls nothing back.
	abort, err := s.awaitCatalog(ctx, text)
	if err != nil {
		return Result{}, err
	}
	// The registry's name for this turn, minted before the section that opens
	// it — the registry is never called with s.mu held — and ended on every
	// return path, this prompt's refusals and withdrawals included. A token
	// nothing was ever parked against costs one number and resolves nothing;
	// leaving one open would leave an ask that arrives late parked against a
	// turn that is over. EndTurn is idempotent, so the paths below that end it
	// before their terminal event (endAskTurn) leave this a no-op.
	//
	// It is minted before s.mu and installed under it, in one section with the
	// cancel check, so a Cancel in between marks the turn *this prompt is
	// withdrawing from* — never this one, which no ask can have reached yet
	// because nothing knows its token.
	token := s.asks.BeginTurn()
	defer s.asks.EndTurn(token)
	s.mu.Lock()
	if s.clearCatalogWaitLocked(abort) {
		// Cancelled while waiting. Nothing was opened, nothing was sent and
		// nothing will be emitted, so saying so to the caller is the whole of
		// it — the turn it drew is its own to settle.
		s.mu.Unlock()
		return Result{}, ErrPromptCancelled
	}
	if s.cancelling {
		// Cancelled since the claim, and the turn is not open: withdraw. The
		// Cancel that marked it is waiting on this wire, and withdrawn tells it
		// nothing reached the agent. The mark is read here and not cleared,
		// which is why the opening below no longer clears it: Begin does, before
		// any cancel could be this prompt's.
		wire.publishLocked(wireWithdrawn)
		s.mu.Unlock()
		return Result{}, ErrPromptCancelled
	}
	client := s.client
	if client == nil {
		s.mu.Unlock()
		return Result{}, fmt.Errorf("agent: session not started")
	}
	if s.inPrompt {
		s.mu.Unlock()
		return Result{}, acp.ErrPromptInFlight
	}
	// The turn bookkeeping is rolled back below if the client refuses the
	// prompt before the wire: nothing was attempted, so nothing happened.
	prevTurn, prevDone := s.turn, s.doneEmitted
	s.inPrompt = true
	s.turn++
	s.token = token
	// A new turn: it has not ended, so an interjection is live again. No
	// cancel has been asked for it either — the withdraw above says so.
	s.doneEmitted = false
	done = make(chan struct{})
	s.promptDone = done
	// The references are read in the same locked section as everything else
	// the turn depends on: an available_commands_update landing now either
	// renamed the entries before this prompt resolved them or after, never
	// halfway through.
	refs := pluginRefs(text, s.pluginLookupLocked())
	s.mu.Unlock()

	// Block 1 is the draft; what craze expanded follows it. The events go out
	// from the hook, which runs only once the client has accepted the prompt:
	// a refused one expanded nothing, because nothing was sent.
	blocks, expanded := promptBlocks(text, refs)
	var accepted func()
	if len(expanded) > 0 {
		accepted = func() {
			for _, cmd := range expanded {
				// Its own copy, not a pointer into the shared slice: the event
				// outlives this loop. And emitted under the prompt's own
				// context, because this hook runs before the request is
				// written: a consumer that has stopped draining would
				// otherwise hold the prompt back from the wire entirely,
				// where every other event of a turn only holds back the turn.
				if !s.emitCtx(ctx, Event{Type: EventCommand, Command: &cmd}) {
					return
				}
			}
		}
	}
	// sent reports the bytes leaving, and it reports them into this turn's own
	// wire, never s.wire: on grok it can run after PromptBlocks has returned,
	// and by then s.wire may be the next turn's, whose prompt is not out yet.
	sent := func() { s.publishWire(wire, wireSent) }
	if testBeforeWire != nil {
		testBeforeWire(s)
	}
	res, err := client.PromptBlocks(ctx, blocks, accepted, sent)
	// The outcome is settled the moment the client returns — before the
	// rollback below and before the deferred release — so a Cancel landing
	// anywhere after this finds it decided. The first outcome wins: a prompt
	// whose bytes went out and whose reply then failed stays sent.
	switch {
	case refusedBeforeWire(err):
		s.publishWire(wire, wireRefused)
	case err != nil:
		s.publishWire(wire, wireFailed)
	default:
		// A clean return is taken as sent: grok's writer may not have
		// reported yet, and short of Close ending the turn from this side,
		// the agent only ends a turn it was given.
		s.publishWire(wire, wireSent)
	}
	if refusedBeforeWire(err) {
		// The prompt never left craze: no turn ran, so this turn number was
		// never spent and there is nothing for the stream to report. Telling
		// the caller is the whole of it — an error event here would show up on
		// a run that goes on to succeed.
		s.mu.Lock()
		s.turn = prevTurn
		// No turn of craze's own opened, so nothing may be parked against this
		// token from here; the deferred EndTurn retires it.
		s.token = TurnToken{}
		s.doneEmitted = prevDone
		s.mu.Unlock()
		return Result{}, err
	}
	// The turn is over the moment Prompt returns, whichever way it went.
	// Marking it here and not beside the EventDone below is what closes the
	// window an interjection could otherwise slip through on the error path,
	// where there is no EventDone at all — and an interjection that lands
	// after a turn is exactly what makes grok mint one of its own.
	s.mu.Lock()
	s.doneEmitted = true
	s.mu.Unlock()
	// Before the terminal event, whichever it is: every ask this turn still
	// holds ends now, and the outbox is drained, so a late opening and every
	// turn_ended ending are in the record ahead of the ending that says the turn
	// is over (plan 021 §3.6).
	s.endAskTurn(token)
	if err != nil {
		// The error goes out first, so a consumer already in its error state
		// by the time the engine's chain policy clears its own queue at
		// settlement (plan 021 §3.5) — this session has none of its own any
		// more.
		s.emit(Event{Type: EventError, Err: err})
		return Result{}, err
	}
	if res.StopReason == acp.StopCancelled {
		s.closeInFlightTools(acp.StopCancelled)
	}
	s.emit(Event{Type: EventDone, StopReason: res.StopReason})
	return Result{StopReason: res.StopReason}, nil
}

// endAskTurn ends token for the registry and then waits for the outbox, so
// everything that ending produced — and any opening still in flight behind it —
// is delivered before the caller publishes the turn's own terminal event. The
// flush is an ordering nicety and never a condition: a log that is closing has
// nothing left to order and returns at once, and a session that is closing
// abandons the wait (flushAsks).
//
// It waits for the outbox, not for handler goroutines: one that has not reached
// the registry yet writes its ending after this turn's terminal event, which is
// review r17's finding 6 and is recorded rather than fixed (decideAsk).
func (s *session) endAskTurn(token TurnToken) {
	s.asks.EndTurn(token)
	s.flushAsks()
}

// refusedBeforeWire reports an error that means the prompt never reached the
// agent: a turn craze did not start is running, or one of craze's own still
// is. Nothing was attempted and no queued message was lost, so the refusal is
// the caller's to retry — it is not a turn that failed.
func refusedBeforeWire(err error) bool {
	return errors.Is(err, acp.ErrForeignTurn) || errors.Is(err, acp.ErrPromptInFlight)
}

// closeInFlightTools settles every tool the turn left running. Grok answers
// a cancel with the turn's end and nothing more: the interrupted tool never
// gets a terminal tool_call_update, so without this its row spins forever and
// the status row keeps counting it. Cursor closes its own tools, so this
// finds nothing there.
func (s *session) closeInFlightTools(status string) {
	s.mu.Lock()
	ids := append([]string(nil), s.toolOrder...)
	s.mu.Unlock()
	for _, id := range ids {
		// The in-flight check happens under the merge lock: a terminal
		// update that lands between the scan and the merge wins.
		st := status
		tool, changed, extras := s.applyToolDelta("", toolDelta{id: id, status: &st, onlyIfInFlight: true})
		if changed {
			s.emit(Event{Type: EventTool, Tool: &tool})
		}
		s.emitAll(extras)
	}
}

func (s *session) SetModel(ctx context.Context, modelID string) error {
	client := s.clientRef()
	if client == nil {
		return fmt.Errorf("agent: session not started")
	}
	if err := client.SetModel(ctx, modelID); err != nil {
		return err
	}
	s.mu.Lock()
	s.snap.CurrentModel = modelID
	s.mu.Unlock()
	return nil
}

func (s *session) SetMode(ctx context.Context, modeID string) error {
	client := s.clientRef()
	if client == nil {
		return fmt.Errorf("agent: session not started")
	}
	if err := client.SetMode(ctx, modeID); err != nil {
		return err
	}
	s.mu.Lock()
	s.snap.CurrentMode = modeID
	s.mu.Unlock()
	return nil
}

func (s *session) SetConfig(ctx context.Context, id, value string) error {
	client := s.clientRef()
	if client == nil {
		return fmt.Errorf("agent: session not started")
	}
	if err := client.SetConfig(ctx, id, value); err != nil {
		return err
	}
	s.mu.Lock()
	for i := range s.snap.Config {
		if s.snap.Config[i].ID == id {
			s.snap.Config[i].Current = value
			break
		}
	}
	s.mu.Unlock()
	return nil
}

// SetTitle is /rename: craze's own name for this session, since ACP v1 has no
// rename verb. It emits nothing — it is called from the UI's update goroutine,
// and an emit onto a full event channel there would block the UI on a consumer
// the UI itself schedules; the caller re-reads the snapshot instead. It pins
// the title, so a session_info_update the agent sends later is ignored.
func (s *session) SetTitle(title string) {
	s.mu.Lock()
	s.snap.Title = sanitizeText(title)
	s.titlePinned = true
	s.mu.Unlock()
}

func (s *session) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.snap
	out.Models = append([]ModelInfo(nil), s.snap.Models...)
	out.Modes = append([]ModeInfo(nil), s.snap.Modes...)
	out.Commands = append([]CommandInfo(nil), s.snap.Commands...)
	out.Plugins = append([]PluginCommand(nil), s.snap.Plugins...)
	out.Config = cloneConfig(s.snap.Config)
	out.Todos = append([]Todo(nil), s.snap.Todos...)
	out.Tools = snapshotTools(s.toolOrder, s.tools)
	out.Subagents = snapshotSubagents(s.subagentOrder, s.subagents)
	out.ForeignTurn = s.foreign
	return out
}

// ForeignTurn is the Session leaf accessor (plan 021 §3.3): s.foreign under
// s.mu alone, the same field Snapshot reports, with none of Snapshot's clones.
func (s *session) ForeignTurn() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.foreign
}

// Cancel maps onto CancelOutcome as follows (plan 021 §3.7): with no client at
// all, or with nothing of craze's own claimed or running (a foreign turn or
// idle), Settled is true and Wrote is whether the write below succeeded — the
// no-claim path and the wireSent/wireRefused path are the two writing paths,
// and both set Wrote from client.Cancel's own return. A catalog-wait abort, or
// a wire that settles wireWithdrawn, is a Withdrew with nothing written and
// nothing left to wait for, so Settled is true too. Waiting on the wire's
// outcome or the turn's own ending can be cut short by ctx or by the session
// closing: Settled is then false, because this call does not know what
// finished the wait — only what it already knew before giving up, which is
// exactly what is reported alongside the error.
func (s *session) Cancel(ctx context.Context) (CancelOutcome, error) {
	client := s.clientRef()
	if client == nil {
		return CancelOutcome{Settled: true}, nil
	}
	// A prompt still waiting for the catalog has opened no turn, so nothing
	// below would reach it: without this, Esc would return having cancelled
	// nothing and the wait would go on to send the very prompt it was pressed
	// to stop.
	//
	// Aborting one is the whole of this cancel, and everything below is
	// skipped. The abort happened under s.mu with !s.inPrompt, so there was no
	// turn of craze's own to cancel; the rest of the path — marking the turn
	// cancelling, answering blocked requests against s.turn, waiting for its
	// prompt to be written, session/cancel, waiting on s.promptDone — is
	// written for a turn that is on the wire, and from here it can only land
	// on one that starts later. The aborted prompt returns ErrPromptCancelled,
	// its consumer settles the turn and drains whatever it had queued, and the
	// tail running on would cancel that next prompt instead: Esc for the one
	// the user stopped would stop the one they did not.
	//
	// So an Esc that lands here does not answer a foreign turn's blocked
	// request. Nothing is lost by that: no wait is registered any more, so a
	// second Esc falls straight through to the normal path and answers it
	// exactly as it always did.
	//
	// The abort and the mark below are one locked section. A claimed prompt
	// decides whether to park by reading that mark in the section that
	// registers its wait, so it either registered before this — and is
	// aborted here — or finds the mark and does not park. Two sections would
	// leave a prompt free to park between them, unaborted and unmarked, and
	// this Cancel waiting on its wire for the whole window.
	s.mu.Lock()
	if s.abortCatalogWaitLocked() {
		s.mu.Unlock()
		return CancelOutcome{Withdrew: true, Settled: true}, nil
	}
	// From here until the turn ends an interjection would be stranded, which
	// is what makes grok mint a turn of its own. The claim, the turn and the
	// wire are read in the same section that marks it, so the wire is the
	// marked prompt's own: Begin sets the claim and the wire together, the
	// opening sets the turn, and the claim's release clears all three.
	s.cancelling = true
	in, claimed, wire := s.inPrompt, s.claimed, s.wire
	token := s.token
	s.mu.Unlock()
	// Every ask the session is holding is answered cancelled, of whatever turn,
	// and the turn this cancel is for is marked so that a request arriving while
	// it is in flight is refused rather than parked — today's cancelWaiting and
	// its cancelledTurn mark, in one atomic registry call. With no turn of
	// craze's own the token is the no-turn one: every open ask is still
	// answered, and nothing is marked, because there is no turn to mark.
	s.asks.CancelTurn(token)
	if (!in && !claimed) || wire == nil {
		// No prompt of craze's own is claimed: nothing is running, or the
		// agent is running a turn it started itself. Either way there is no
		// prompt of ours for the cancel to overtake, so it goes now. Settled
		// is true regardless of whether the write itself succeeded: craze had
		// no turn of its own in flight either way, so there is nothing here
		// for a later wait to resolve. Wrote is not merely "no error": the
		// client answers nil without writing while it has no session id, which
		// is the whole of Start before session/new returns, and a cancel then
		// reached nobody.
		known := client.SessionID() != ""
		returned, err := s.writeCancel(ctx, client)
		return CancelOutcome{Wrote: known && returned && err == nil, Settled: true}, err
	}
	// The prompt is claimed before its turn opens and its turn opens before
	// its request is written, so the cancel waits for the wire to say what
	// became of it: written first, a cancel can only land behind it. Without
	// the wait it could reach the agent first, be dropped as belonging to no
	// turn, and leave the prompt running as if Esc had never been pressed. A
	// prompt claimed and not yet open cannot get as far as the wire any more
	// — it finds the mark just set and withdraws — so for it the wait is only
	// for the withdraw, and nothing is written. The wait is bounded by the
	// caller's context, the same one the wait on promptDone below has always
	// used; it is one marshal and one pipe write unless the agent has stopped
	// reading, and then saying the cancel failed is the honest answer.
	select {
	case <-wire.done:
	case <-ctx.Done():
		// The write outcome is still unknown, so nothing is known: no write,
		// no withdrawal, no settlement.
		return CancelOutcome{}, ctx.Err()
	case <-s.done:
		// The session closed before the wire said anything. The outcome is
		// exactly as unknown as the ctx.Done case above; "as today" is the nil
		// error only.
		return CancelOutcome{}, nil
	}
	var wrote, withdrew bool
	switch s.outcomeOf(wire) {
	case wireSent, wireRefused:
		// Each Cancel writes once at most: here, or on the no-turn path above.
		// Two racing Cancels can each write one, which the agent tolerates.
		// A refused prompt is cancelled too: the refusal that reaches here
		// is a foreign turn, and that turn is still running.
		returned, err := s.writeCancel(ctx, client)
		switch {
		case err != nil:
			return CancelOutcome{}, err
		case !returned:
			// The session closed with the write still in the pipe. Nothing about
			// it is known, which is the same answer the wire wait above gives for
			// a close: no write, no withdrawal, no settlement, and no error.
			return CancelOutcome{}, nil
		}
		wrote = true
	default:
		// wireFailed or wireWithdrawn: the prompt never reached the agent, so
		// there is no turn there to stop and nothing is written. Only the
		// latter is a withdrawal this cancel caused; wireFailed is a prompt
		// that ended some other way before this cancel could reach it.
		withdrew = s.outcomeOf(wire) == wireWithdrawn
	}
	// The wait is for the turn this cancel was for, and only that one: the
	// release clears s.wire together with inPrompt, so a wire that is no
	// longer s.wire means that turn is over and whatever runs now started
	// after this cancel. Both are read again rather than trusted from the
	// section above, so a claimed prompt whose turn opened after that read is
	// waited for like any other.
	s.mu.Lock()
	done := s.promptDone
	in = s.inPrompt && s.wire == wire
	s.mu.Unlock()
	if !in || done == nil {
		// Nothing left to wait for: either this wire's outcome already says
		// there is no turn of ours running (withdrawn, failed), or the turn
		// it did open has already ended by this read.
		return CancelOutcome{Wrote: wrote, Withdrew: withdrew, Settled: true}, nil
	}
	select {
	case <-done:
		return CancelOutcome{Wrote: wrote, Withdrew: withdrew, Settled: true}, nil
	case <-ctx.Done():
		return CancelOutcome{Wrote: wrote, Withdrew: withdrew}, ctx.Err()
	}
}

// writeCancel writes the one session/cancel a Cancel owes, and makes the
// caller's context mean what this session has always promised it means: a bound
// on the whole call. returned says the write itself came back, so its error is
// the wire's own answer; false says this call gave the write up while it was
// still in flight, and then nothing about it is known.
//
// The bound has to be here because it is nowhere below: Conn.Notify checks the
// context once, *before* a synchronous write (internal/acp/conn.go), and
// Encoder.WriteMessage then marshals, takes the encoder's mutex and writes. With
// the agent's stdin full — an agent that has stopped reading, which is exactly
// the state a user presses Esc in — that write blocks for as long as the pipe
// does, past any deadline. Before the engine such a cancel stranded one
// goroutine; now the engine holds every admission path while a cancel is in
// flight, so a cancel that never returns is a session that never settles another
// turn and never admits another prompt. Giving the write up is the lesser
// failure, and the seam already promised it.
//
// Abandoning a write is not free, and this is exactly what it costs. The write
// is in one of two places. Either it is inside the encoder, holding its mutex,
// in which case every later write — a prompt's included — takes that same mutex
// afterwards and so reaches the agent *behind* this cancel, which an agent
// answers by dropping a cancel that names no running turn: that is the same
// tolerance the no-turn path above has always relied on. Or it is still waiting
// for the encoder's mutex behind some other write, and then a prompt admitted
// after this call returns can overtake it and be the turn the cancel lands on.
// That window is not closed here, and is not claimed to be: plan 017's rule is
// kept by the wire wait above for the turn this cancel *was* for, while this
// residual case is a cancel the caller was told nothing is known about — the
// engine reports it as `unknown`, which is precisely that — and the honest
// remedy is a second cancel, not a longer wait that wedges the session.
//
// The goroutine outlives the call and ends when the pipe takes the bytes or the
// transport is closed under it, which fails the parked write. Nothing is
// published from it and its result is dropped. The client's own completion of the
// requests it was holding rides in it too, so on an abandoned write that can land
// after this returns — but only for the requests held when this cancel was made
// (acp.Client.Held, taken below before the goroutine exists), never for one a
// later turn registered. Nothing a client can see waits on it either, because
// the session answered its *own* parked asks before getting here (cancelWaiting,
// above).
func (s *session) writeCancel(ctx context.Context, client *acp.Client) (returned bool, err error) {
	// What this cancel is a cancel of is fixed here, on the caller's goroutine,
	// before anything is handed over. The client answers the requests it holds
	// cancelled as the first step of its Cancel; left to gather them on the
	// goroutine below, a write given up on its context could run late — after
	// the caller had returned, the engine had released its hold, and a new
	// prompt had been admitted and asked for a permission — and answer *that*
	// request cancelled, for a turn nobody cancelled.
	held := client.Held()
	res := make(chan error, 1)
	go func() { res <- client.CancelHeld(ctx, held) }()
	select {
	case err := <-res:
		return true, err
	case <-ctx.Done():
		return false, ctx.Err()
	case <-s.done:
		return false, nil
	}
}

// Close reaps the child exactly once; later callers block until that reap has
// finished rather than returning while the child is still alive. It stores
// client.Close()'s result and returns the same value on every call, so
// requestQuit's own close and finishRun's later one — the same session,
// closed twice — agree: errors.Is(err, ErrAgentExited) holds on both when the
// agent's exit was reaped first, on neither when craze closed it.
//
// The event log closes last (plan 020 §3.5), once the teardown above has run:
// done is closed first, so every publisher blocked on the primary gives up,
// and client.Close is where the agent's last words are written. Closing the
// log is the admission cutoff: a handler goroutine that emits after this —
// Close does not join the prompt goroutine or the client's handlers — is
// refused by the log and reaches no subscriber. It is called with s.mu
// released, as every publish is.
func (s *session) Close() error {
	s.closeOnce.Do(func() {
		defer close(s.closeDone)
		s.mu.Lock()
		s.closed = true
		close(s.done)
		client := s.client
		s.mu.Unlock()
		// Before the client is torn down and well before the log is closed:
		// every parked ask ends as closing, and those endings are committed by
		// the log's own close phases rather than dropped (plan 021 §3.3, X4).
		// The handlers they wake then reply to the agent, exactly as falling
		// through on s.done used to.
		//
		// It is also what keeps the outcome "closing" rather than "cancelled by
		// call" now that each parked ask watches its own request's context
		// (decideAsk): those contexts are ended by the client's own close
		// below, which is strictly after this.
		s.asks.Close()
		s.askStop()
		if client != nil {
			s.closeErr = client.Close()
		}
		// The child's stderr copy has finished by now (Client.Close waits for
		// it), so this is where its last unterminated line can be noted — and
		// it has to be before the log, which is the note cutoff.
		s.tee.Flush()
		s.log.Close(context.Background())
	})
	<-s.closeDone
	return s.closeErr
}

// The three blocking requests an agent can make all take the same path (plan
// 021 §3.6), on the handler goroutine ACP gave them, which may block:
//
//  1. craze's approval policy (ApprovalPolicy). A resolution it can make itself
//     is written as one Automatic record — the Auto opening a question or a plan
//     has always published, plus its ending — and answered without ever parking.
//  2. the turn the request belongs to (askToken). A request from a turn that is
//     over raises no card at all: it is one self-contained ending with the body,
//     so the record says what was asked without anyone being shown a question
//     nobody could answer.
//  3. the registry: open, flush, wait, flush, answer (decideAsk).
//
// The flushes are the causal barriers. The one after an opening delivers the
// card before the handler blocks; the one before the decision goes back to the
// agent keeps the ask's ending ahead of whatever the agent writes once it has
// its answer — which is what makes an automatic question's line still precede
// the text that follows it (panel: astra 9, CodeRabbit 11). A flush that fails
// is an ordering nicety lost, never a reason to change the decision.
func (s *session) onPermission(a acp.Arrival, req acp.PermissionRequest) acp.PermissionDecision {
	ask := AskRequest{Kind: AskPermission, Body: AskBody{Permission: permissionBody(req)}}
	token, live := s.askToken(a)
	if EffectiveApproval(s.opts).Permission == ApprovalAllow {
		// Force. An option that allows it, or — when the request offers none —
		// the cancel today's PickYoloAllow miss produced, now with a record
		// saying policy did it (§2.3's "none at all" row).
		ans := AskAnswer{Cancel: true}
		if id, ok := acp.PickYoloAllow(req.Options); ok {
			ans = AskAnswer{OptionID: id}
		}
		rec := s.automaticAsk(token, ask, ans)
		dec := permissionDecision(rec)
		dec.Replied = s.reportedBy(rec.ID)
		return dec
	}
	if !live {
		return permissionDecision(s.staleAsk(ask))
	}
	rec, replied, ok := s.decideAsk(a, token, ask)
	if !ok {
		return acp.PermissionDecision{Cancelled: true}
	}
	dec := permissionDecision(rec)
	dec.Replied = replied
	return dec
}

// onAskQuestion parks a question under policy ask, and otherwise answers it
// with each question's first option and reports what it sent.
func (s *session) onAskQuestion(a acp.Arrival, req acp.AskQuestionRequest) acp.AskDecision {
	ask := AskRequest{Kind: AskQuestion, Body: AskBody{Question: questionBody(req)}}
	token, live := s.askToken(a)
	if EffectiveApproval(s.opts).Question == ApprovalFirstOption {
		rec := s.automaticAsk(token, ask, AskAnswer{Answers: acp.AskAutoAnswers(req)})
		dec := askDecision(rec)
		dec.Replied = s.reportedBy(rec.ID)
		return dec
	}
	if !live {
		return askDecision(s.staleAsk(ask))
	}
	rec, replied, ok := s.decideAsk(a, token, ask)
	if !ok {
		return acp.AskDecision{Cancelled: true}
	}
	dec := askDecision(rec)
	dec.Replied = replied
	return dec
}

func questionsFromRequest(req acp.AskQuestionRequest) []Question {
	out := make([]Question, 0, len(req.Questions))
	for _, q := range req.Questions {
		opts := make([]Option, 0, len(q.Options))
		for _, o := range q.Options {
			opts = append(opts, Option{ID: o.ID, Label: sanitizeText(o.Label)})
		}
		if len(opts) == 0 {
			// A question with no options still needs something to press; the
			// empty id is dropped when the reply is built, so craze never
			// invents an option id.
			opts = append(opts, Option{Label: "OK"})
		}
		out = append(out, Question{
			ID:            q.ID,
			Prompt:        sanitizeText(q.Prompt),
			Options:       opts,
			AllowMultiple: q.AllowMultiple,
		})
	}
	return out
}

// onCreatePlan mirrors onAskQuestion: park under policy ask, accept and report
// otherwise.
func (s *session) onCreatePlan(a acp.Arrival, req acp.CreatePlanRequest) acp.PlanDecision {
	ask := AskRequest{Kind: AskPlan, Body: AskBody{Plan: planBody(req)}}
	token, live := s.askToken(a)
	if EffectiveApproval(s.opts).Plan == ApprovalAccept {
		rec := s.automaticAsk(token, ask, AskAnswer{Accept: true})
		dec := planDecision(rec)
		dec.Replied = s.reportedBy(rec.ID)
		return dec
	}
	if !live {
		return planDecision(s.staleAsk(ask))
	}
	rec, replied, ok := s.decideAsk(a, token, ask)
	if !ok {
		return acp.PlanDecision{Cancelled: true}
	}
	dec := planDecision(rec)
	dec.Replied = replied
	return dec
}

// onEarlyAnswer is ACP reporting a blocking request it answered before any
// handler of craze's ever ran: a cancel or a close that got there first, or a
// turn that had gone stale (plan 021 §3.6; panel: astra 12). No handler runs
// for it, so nothing parks and — without this — the agent's question would
// leave no trace at all (§2.3's last-but-two row). It becomes one
// self-contained ending, with the body and no opening, so the record says what
// was asked without a card ever being raised for a request nobody can answer.
//
// NOT CLOSED, deliberately (review r17, finding 6): this runs on the request's
// own handler goroutine, and Close joins no such goroutine. One that has not
// run by the time the session answers its request and closes the log writes
// nothing at all — the enqueue below is refused at the cut — so that request
// leaves ZERO published endings. Closing it needs requests accounted for at
// registration and their recording completed before the log's cutoff, which
// would mean joining goroutines that may be parked on a decision: the deadlock
// the close phases exist to avoid.
func (s *session) onEarlyAnswer(e acp.EarlyAnswer) {
	req, ok := earlyAskRequest(e.Params)
	if !ok {
		return
	}
	outcome := AskCancelled
	switch e.Reason {
	case acp.EarlyStaleTurn:
		outcome = AskTurnEnded
	case acp.EarlyClosed:
		outcome = AskClosing
	}
	// The token is only the record's: the ask is resolved as it is minted, so
	// no turn's lifecycle can reach it. A request that belonged to no turn of
	// craze's own says so.
	token, _ := s.askToken(e.Arrival())
	s.asks.AnsweredEarly(token, req, outcome, AskByProvider)
}

// earlyAskRequest is one early-answered request as the registry takes it. The
// bodies are built by the same three functions the handlers use, so what an
// early ending records is exactly what the card would have shown.
func earlyAskRequest(p acp.RequestParams) (AskRequest, bool) {
	switch {
	case p.Permission != nil:
		return AskRequest{Kind: AskPermission, Body: AskBody{Permission: permissionBody(*p.Permission)}}, true
	case p.Ask != nil:
		return AskRequest{Kind: AskQuestion, Body: AskBody{Question: questionBody(*p.Ask)}}, true
	case p.Plan != nil:
		return AskRequest{Kind: AskPlan, Body: AskBody{Plan: planBody(*p.Plan)}}, true
	default:
		return AskRequest{}, false
	}
}

// askToken is the registry token a blocking request parks against, and whether
// it may be parked at all. It is the whole of "which turn does this belong to",
// and it needs both halves of the request's Arrival (acp.Arrival): ACP's
// counter alone cannot tell a retired turn's request from one that belongs to
// no turn, because a prompt returning clears inTurn without moving the counter.
func (s *session) askToken(a acp.Arrival) (TurnToken, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !a.InTurn {
		// It arrived between craze's turns, or during a turn the agent started
		// itself. It belongs to no turn of craze's own, so no turn's end may
		// take it away: it parks on the no-turn token and ends by an answer, a
		// cancel, or the close.
		return TurnToken{}, true
	}
	if s.inPrompt && s.turnActiveLocked(a.Turn) {
		// The client says this exact turn is the one it is running and that its
		// prompt is still in flight, so s.token — installed before that prompt
		// entered the client and cleared only after it came back — is that
		// turn's own and not its successor's (turnActiveLocked).
		return s.token, true
	}
	// It belonged to a turn of craze's own and that turn is over: either a
	// later prompt has been accepted (the counter moved on), or this one has
	// returned (no prompt is in flight), or the next one is installed and not
	// yet on the wire (the counter still says this turn, but the prompt that
	// held it has ended). A card for it would be answered into whatever is
	// running now, so there is no card — only the record.
	return TurnToken{}, false
}

// decideAsk is the whole of a handler's parked path: open, flush, wait, flush.
// It answers with the record the handler replies from, the Replied hook that
// records what became of that reply, and false for the one case that has no
// record at all.
//
// **The ask is opened against the request's own context** (acp.Arrival.Call),
// not against something session-wide: the client ends it the moment anything
// else answers the request — a Cancel, a CancelHeld, a Close — so an ask whose
// request has already been answered resolves as cancelled by the call at once,
// rather than sitting on screen with the agent no longer listening (review r17,
// finding 3). A handler called directly, by a test or by anything else with no
// context of its own, falls back to the session's, which is the backstop it
// always was.
//
// A signal already up **before** the open raises no card at all: the request is
// gone, so there is nothing to show anyone, and what it is owed is the same
// self-contained record an early answer gets — cancelled, by the call, with the
// body. That keeps the two sides of the window saying the same thing: the
// ending is "cancelled by call" whether the signal arrived a moment before the
// insert or a moment after it, and only the card differs.
//
// The only error Open has is an adopted id already in use, and nothing here
// adopts one; a lifecycle refusal — the turn ended or was cancelled between the
// token being read and the insert, the session closed, the outbox full — comes
// back as an ask that is already resolved, and Wait answers with that ending
// like any other. That check is the registry's own, atomic with the insert,
// which is what closes the window park and emitParked needed two steps for.
//
// NOT CLOSED, deliberately (review r17, finding 6): a handler that captured
// this turn's token and lost the race to EndTurn has its refused Open's
// self-contained ending enqueued AFTER the turn's EventDone, so a consumer sees
// one ending for a request it was never shown arrive past the end of the turn.
// The ending is in the record and nothing is lost; only its position is odd.
// Closing it needs the terminal flush to join handler goroutines that may be
// parked on a decision, which is the deadlock the close phases exist to avoid.
func (s *session) decideAsk(a acp.Arrival, token TurnToken, req AskRequest) (AskRecord, func(acp.ReplyDisposition), bool) {
	ctx := s.askCall(a)
	if ctx.Err() != nil {
		rec := s.asks.AnsweredEarly(token, req, AskCancelled, AskByCall)
		s.flushAsks()
		return rec, s.reportedBy(rec.ID), true
	}
	parked, err := s.asks.Open(ctx, token, req)
	if err != nil {
		return AskRecord{}, nil, false
	}
	s.flushAsks()
	rec := parked.Wait()
	s.flushAsks()
	return rec, askReported(parked), true
}

// askCall is the context an ask opened for a is watched on: the request's own,
// and the session's when the arrival carries none (decideAsk). The session's is
// the backstop it has always been — Close resolves every parked ask before it
// fires — and a future harness Gate's Ask brings a real per-call context of its
// own through the same argument.
func (s *session) askCall(a acp.Arrival) context.Context {
	if a.Call != nil {
		return a.Call
	}
	return s.askCtx
}

// automaticAsk records a resolution craze's own policy made and waits for the
// outbox, so the opening and its ending are both in the record before the text
// the agent writes once it has been answered.
func (s *session) automaticAsk(token TurnToken, req AskRequest, a AskAnswer) AskRecord {
	rec := s.asks.Automatic(token, req, a)
	s.flushAsks()
	return rec
}

// staleAsk records a request whose own turn is over, as one self-contained
// ending with no opening, and waits for the outbox for the same reason every
// other handler path does.
func (s *session) staleAsk(req AskRequest) AskRecord {
	rec := s.asks.AnsweredEarly(TurnToken{}, req, AskTurnEnded, AskByTurn)
	s.flushAsks()
	return rec
}

// flushAsks waits for everything the registry has enqueued so far. It runs on a
// provider handler goroutine or on the prompt's, never the primary's reader, and
// blocks exactly as an emit from the same goroutine blocks today — **the
// session's own done is what ends it**, as it ends an emit (emitCtx), so a
// Close that waits for one of those goroutines can never be waiting for a
// barrier only its own last phase could free (review r17, finding 1).
func (s *session) flushAsks() { _ = s.log.Flush(context.Background(), s.done) }

// askReported is the Replied hook for a parked ask: what became of the reply
// carrying its decision, recorded against THIS ask and not against its id,
// because a reply that comes back late must not land on whatever holds that id
// now (Ask.Report).
func askReported(a *Ask) func(acp.ReplyDisposition) {
	return func(d acp.ReplyDisposition) {
		a.Report(AskReport{Delivered: d.Delivered, Lost: d.Lost})
	}
}

// reportedBy is askReported for a record with no handle: an automatic
// resolution, whose id was minted and is never reused.
func (s *session) reportedBy(id string) func(acp.ReplyDisposition) {
	return func(d acp.ReplyDisposition) {
		s.asks.Report(id, AskReport{Delivered: d.Delivered, Lost: d.Lost})
	}
}

// permissionBody, questionBody and planBody are the three openings, built once
// and shared by the handlers and by the early-answer path, so a request that
// never raised a card is recorded exactly as one that did. The id is the
// registry's to fill in.
func permissionBody(req acp.PermissionRequest) *PermissionEvent {
	opts := make([]PermissionOption, 0, len(req.Options))
	for _, o := range req.Options {
		opts = append(opts, PermissionOption{
			OptionID: o.OptionID,
			Name:     sanitizeText(o.Name),
			Kind:     o.Kind,
		})
	}
	return &PermissionEvent{Tool: sanitizeText(req.ToolCall.Title), Options: opts}
}

func questionBody(req acp.AskQuestionRequest) *QuestionEvent {
	return &QuestionEvent{Title: sanitizeText(req.Title), Questions: questionsFromRequest(req)}
}

func planBody(req acp.CreatePlanRequest) *PlanEvent {
	return &PlanEvent{
		Name:     sanitizeText(req.DisplayName()),
		Overview: sanitizeText(req.Overview),
		Plan:     sanitizeText(req.PlanText()),
		Todos:    todosFromWire(req.Todos),
	}
}

// permissionDecision, askDecision and planDecision are one ask's record as the
// decision the agent is waiting for. Only an answer and an automatic
// resolution decide anything; every other ending — cancelled, its turn's end,
// the close — is the cancelled outcome, which is what the agent was always told
// then.
func permissionDecision(rec AskRecord) acp.PermissionDecision {
	switch rec.Outcome {
	case AskAnswered, AskAutomatic:
		return acp.PermissionDecision{OptionID: rec.Answer.OptionID}
	default:
		return acp.PermissionDecision{Cancelled: true}
	}
}

func askDecision(rec AskRecord) acp.AskDecision {
	switch rec.Outcome {
	case AskAnswered, AskAutomatic:
		if rec.Answer.Skip {
			return acp.AskDecision{Skip: true}
		}
		return acp.AskDecision{Answers: rec.Answer.Answers}
	default:
		return acp.AskDecision{Cancelled: true}
	}
}

func planDecision(rec AskRecord) acp.PlanDecision {
	switch rec.Outcome {
	case AskAnswered, AskAutomatic:
		return acp.PlanDecision{Accept: rec.Answer.Accept}
	default:
		return acp.PlanDecision{Cancelled: true}
	}
}

// turnActiveLocked reports whether the turn a request arrived in is the one the
// client is running *with a prompt of craze's own still in flight for it*;
// callers hold s.mu. With no client — before Start, and in the unit tests —
// there is one turn and it is turn 0. The lock order is session then client:
// the client answers this without taking any lock the session holds, and it
// never calls back into the session under its own.
//
// It is one atomic ACP check and not a counter comparison, because counter
// equality alone cannot say whose token s.token is (acp.Client.TurnActive,
// review r17 finding 2): between one prompt returning and the next entering
// PromptBlocks the counter still names the old turn while the session already
// holds the new one's.
func (s *session) turnActiveLocked(turn int) bool {
	if s.client == nil {
		return turn == 0
	}
	return s.client.TurnActive(turn)
}

// onUpdateTodos merges the request into Snapshot.Todos and returns the merged
// list, which the client echoes back to cursor.
func (s *session) onUpdateTodos(req acp.UpdateTodosRequest) []acp.TodoItem {
	s.mu.Lock()
	s.snap.Todos = mergeTodos(s.snap.Todos, todosFromWire(req.Todos), req.Merge)
	s.snap.TodosUpdatedAt = time.Now()
	todos := append([]Todo(nil), s.snap.Todos...)
	s.mu.Unlock()
	s.emit(Event{Type: EventTodos, Todos: todos})
	return todosToWire(todos)
}

// onTaskReceipt joins the sub-agent receipt onto its tool call, in either
// arrival order: the receipt is parked when the tool_call has not landed yet.
func (s *session) onTaskReceipt(req acp.TaskRequest) {
	if req.ToolCallID == "" {
		return
	}
	info := TaskInfo{
		Description:  sanitizeText(req.Description),
		Prompt:       truncateUTF8(sanitizeText(req.Prompt), taskPromptCap),
		Model:        sanitizeText(req.Model),
		AgentID:      sanitizeText(req.AgentID),
		SubagentType: subagentTypeName(req.SubagentType),
		DurationMs:   req.DurationMs,
		Receipt:      true,
	}
	s.mu.Lock()
	tool, ok := s.tools[req.ToolCallID]
	if !ok {
		s.parkTaskReceiptLocked(req.ToolCallID, info)
		s.mu.Unlock()
		return
	}
	tool.Task = mergeTaskInfo(tool.Task, info)
	s.tools[req.ToolCallID] = tool
	extras := s.syncCursorLocked(tool)
	out := cloneTool(s.tools[req.ToolCallID])
	s.mu.Unlock()
	s.emit(Event{Type: EventTool, Tool: &out})
	s.emitAll(extras)
}

type sessionUpdateWire struct {
	SessionUpdate     string                 `json:"sessionUpdate"`
	Content           json.RawMessage        `json:"content,omitempty"`
	ToolCallID        string                 `json:"toolCallId,omitempty"`
	Title             *string                `json:"title,omitempty"`
	Kind              *string                `json:"kind,omitempty"`
	Status            *string                `json:"status,omitempty"`
	RawInput          json.RawMessage        `json:"rawInput,omitempty"`
	RawOutput         json.RawMessage        `json:"rawOutput,omitempty"`
	Locations         json.RawMessage        `json:"locations,omitempty"`
	AvailableCommands []acp.AvailableCommand `json:"availableCommands,omitempty"`
	CurrentModeID     string                 `json:"currentModeId,omitempty"`
	ConfigOptions     json.RawMessage        `json:"configOptions,omitempty"`
	Entries           json.RawMessage        `json:"entries,omitempty"`
}

func (s *session) onUpdate(n acp.SessionNotification) {
	var u sessionUpdateWire
	if err := json.Unmarshal(n.Update, &u); err != nil {
		return
	}
	if n.Child != "" {
		s.onChildUpdate(n.Child, n.ToolName, u)
		return
	}
	if s.replaying.Load() {
		// A replayed prompt arrives in chunks and has to draw one user block,
		// so the chunks are coalesced and pushed out by the next update of
		// any other kind (or by replay end). Outside a replay the case is
		// deliberately absent: grok and gx echo the user's own prompt live,
		// and emitting that would double every user block in the TUI and add
		// user lines to `craze prompt --json`.
		if u.SessionUpdate == updateUserMessage {
			text := messageText(u.Content)
			s.mu.Lock()
			s.replayUser.WriteString(text)
			s.mu.Unlock()
			return
		}
		s.flushReplayUser()
	}
	switch u.SessionUpdate {
	case acp.UpdateAgentMessage:
		s.emit(Event{Type: EventText, Text: messageText(u.Content)})
	case acp.UpdateAgentThought:
		s.emit(Event{Type: EventThought, Text: messageText(u.Content)})
	case acp.UpdateToolCall, acp.UpdateToolCallUpd:
		delta, ok := toolDeltaFromWire(u)
		if !ok {
			return
		}
		delta.wireName = n.ToolName
		tool, emit, extras := s.applyToolDelta("", delta)
		if emit {
			s.emit(Event{Type: EventTool, Tool: &tool})
		}
		s.emitAll(extras)
	case acp.UpdateAvailableCommands:
		s.mu.Lock()
		s.snap.Commands = commandsFromUpdate(u.AvailableCommands)
		// The first update of the session ends the provisional spelling, and
		// every update re-resolves: a name the agent has taken over must stop
		// being offered bare the moment it does.
		first := !s.commandsSeen
		s.commandsSeen = true
		s.snap.Plugins = s.resolvePluginsLocked()
		if first {
			// Closed after the rows are resolved and before s.mu is released,
			// so a prompt waiting on it cannot wake into the old resolution:
			// it has to take this very lock to read one.
			close(s.commandsApplied)
		}
		s.mu.Unlock()
		s.emit(Event{Type: EventMeta})
	case acp.UpdateCurrentMode:
		if u.CurrentModeID != "" {
			mode := sanitizeText(u.CurrentModeID)
			s.mu.Lock()
			s.snap.CurrentMode = mode
			s.mu.Unlock()
			// The mode rides on the event: a mode that changed and changed
			// back is invisible in the snapshot, and the UI has to see it.
			s.emit(Event{Type: EventMeta, Mode: mode})
		}
	case acp.UpdateSessionInfo:
		// session_info_update reuses the tool title field on the wire.
		title := ""
		if u.Title != nil {
			title = sanitizeText(*u.Title)
		}
		if title == "" {
			return
		}
		s.mu.Lock()
		if s.titlePinned {
			// /rename won: the user's name for the session outlives every
			// title the agent invents, including one produced by a live turn
			// after a --continue.
			s.mu.Unlock()
			return
		}
		s.snap.Title = title
		s.mu.Unlock()
		// EventMeta carries the new title so `prompt --json` can emit a
		// title line; the TUI only re-reads the snapshot.
		s.emit(Event{Type: EventMeta, Text: title})
	case acp.UpdateConfigOption:
		cfg := parseConfigOptions(u.ConfigOptions)
		s.mu.Lock()
		s.snap.Config = cfg
		s.mu.Unlock()
		s.emit(Event{Type: EventMeta})
	case acp.UpdatePlan:
		if !s.provider().planUpdatesAreTodos {
			return
		}
		todos, ok := planEntriesToTodos(u.Entries)
		if !ok {
			return
		}
		s.mu.Lock()
		s.snap.Todos = todos
		s.snap.TodosUpdatedAt = time.Now()
		out := append([]Todo(nil), todos...)
		s.mu.Unlock()
		s.emit(Event{Type: EventTodos, Todos: out})
	}
}

func planEntriesToTodos(raw json.RawMessage) ([]Todo, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '[' {
		return nil, false
	}
	var entries []struct {
		ID      string `json:"id"`
		Content string `json:"content"`
		Status  string `json:"status"`
	}
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, false
	}
	out := make([]Todo, 0, len(entries))
	for i, e := range entries {
		content := sanitizeText(e.Content)
		if strings.TrimSpace(content) == "" {
			continue
		}
		id := sanitizeText(e.ID)
		if id == "" {
			id = fmt.Sprintf("plan-%d", i)
		}
		out = append(out, Todo{
			ID:      id,
			Content: content,
			Status:  normalizeTodoStatus(e.Status),
		})
	}
	if len(out) == 0 {
		return nil, true
	}
	return out, true
}

func toolDeltaFromWire(u sessionUpdateWire) (toolDelta, bool) {
	if u.ToolCallID == "" {
		return toolDelta{}, false
	}
	d := toolDelta{
		id:     u.ToolCallID,
		title:  u.Title,
		kind:   u.Kind,
		status: u.Status,
	}
	if raw, ok := presentJSON(u.RawInput); ok {
		d.rawInput = raw
		d.hasRawInput = true
	}
	if raw, ok := presentJSON(u.RawOutput); ok {
		d.rawOutput = raw
		d.hasRawOutput = true
	}
	if raw, ok := presentJSON(u.Content); ok && raw[0] == '[' {
		d.content = raw
		d.hasContent = true
	}
	if raw, ok := presentJSON(u.Locations); ok {
		d.locations = raw
		d.hasLocations = true
	}
	return d, true
}

func messageText(raw json.RawMessage) string {
	raw, ok := presentJSON(raw)
	if !ok || raw[0] != '{' {
		return ""
	}
	var b acp.ContentBlock
	if err := json.Unmarshal(raw, &b); err != nil {
		return ""
	}
	return sanitizeText(b.Text)
}

func (s *session) emit(ev Event) { s.emitCtx(context.Background(), ev) }

// emitCtx is emit with a caller's cancellation as a third way out, and reports
// whether the event was delivered. A session that is closing drops it either
// way; ctx is for a caller that must not be held by a consumer's backlog.
//
// Delivery is the event log's Publish, which numbers the event and hands it
// to the primary on this goroutine, so a true return still means the event is
// in Events()'s buffer. Publish encodes ev before it waits for anything,
// which is why every caller hands emit a payload of its own (cloneTool,
// cloneSubagent, a copied slice): nothing may change it while it is read.
// No caller holds s.mu here; the log's boundary is never taken under it.
func (s *session) emitCtx(ctx context.Context, ev Event) bool {
	if ev.At.IsZero() {
		ev.At = time.Now()
	}
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

func (s *session) promptInFlight() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inPrompt
}

// mergeTodos applies one cursor/update_todos request. merge=false replaces the
// list in request order; merge=true upserts by id, keeping the existing order
// and appending ids it has not seen. Duplicate ids inside one request: last
// wins, at the position of the first occurrence.
func mergeTodos(prev, in []Todo, merge bool) []Todo {
	if !merge {
		return dedupeTodos(in)
	}
	out := append([]Todo(nil), prev...)
	index := make(map[string]int, len(out))
	for i, t := range out {
		index[t.ID] = i
	}
	for _, t := range in {
		if i, ok := index[t.ID]; ok {
			out[i] = t
			continue
		}
		index[t.ID] = len(out)
		out = append(out, t)
	}
	return out
}

func dedupeTodos(in []Todo) []Todo {
	out := make([]Todo, 0, len(in))
	index := make(map[string]int, len(in))
	for _, t := range in {
		if i, ok := index[t.ID]; ok {
			out[i] = t
			continue
		}
		index[t.ID] = len(out)
		out = append(out, t)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func todosFromWire(in []acp.TodoItem) []Todo {
	if len(in) == 0 {
		return nil
	}
	out := make([]Todo, 0, len(in))
	for _, t := range in {
		out = append(out, Todo{
			ID:      sanitizeText(t.ID),
			Content: sanitizeText(t.Content),
			Status:  normalizeTodoStatus(t.Status),
		})
	}
	return out
}

func todosToWire(in []Todo) []acp.TodoItem {
	out := make([]acp.TodoItem, 0, len(in))
	for _, t := range in {
		out = append(out, acp.TodoItem{ID: t.ID, Content: t.Content, Status: t.Status})
	}
	return out
}

// normalizeTodoStatus accepts the documented lowercase values, cursor's
// TODO_STATUS_* enum names and the camelCase inProgress spelling.
func normalizeTodoStatus(s string) string {
	v := strings.ToLower(strings.TrimSpace(sanitizeText(s)))
	v = strings.TrimPrefix(v, "todo_status_")
	v = strings.ReplaceAll(v, "-", "_")
	switch v {
	case "in_progress", "inprogress":
		return "in_progress"
	case "completed", "complete", "done":
		return "completed"
	case "cancelled", "canceled":
		return "cancelled"
	default:
		return "pending"
	}
}

// parkTaskReceiptLocked stores a receipt whose tool_call has not arrived,
// dropping the oldest once the turn's cap is reached.
func (s *session) parkTaskReceiptLocked(id string, info TaskInfo) {
	if _, dup := s.taskReceipts[id]; !dup {
		s.taskReceiptOrder = append(s.taskReceiptOrder, id)
	}
	s.taskReceipts[id] = info
	for len(s.taskReceiptOrder) > taskReceiptCap {
		oldest := s.taskReceiptOrder[0]
		s.taskReceiptOrder = s.taskReceiptOrder[1:]
		delete(s.taskReceipts, oldest)
	}
}

func (s *session) dropTaskReceiptLocked(id string) {
	delete(s.taskReceipts, id)
	for i, x := range s.taskReceiptOrder {
		if x == id {
			s.taskReceiptOrder = append(s.taskReceiptOrder[:i], s.taskReceiptOrder[i+1:]...)
			return
		}
	}
}

func (s *session) clearTaskReceiptsLocked() {
	if len(s.taskReceiptOrder) == 0 {
		return
	}
	s.taskReceipts = make(map[string]TaskInfo)
	s.taskReceiptOrder = nil
}
