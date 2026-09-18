package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/charliek/craze/internal/acp"
)

type session struct {
	opts   Options
	client *acp.Client
	events chan Event
	done   chan struct{}

	closeOnce sync.Once
	closeDone chan struct{}

	mu         sync.Mutex
	started    bool
	closed     bool
	inPrompt   bool
	promptDone chan struct{}
	// wire is the running turn's wire outcome, set in the same locked section
	// as inPrompt and cleared with it: what Cancel waits on so that its
	// session/cancel can never reach the agent ahead of the prompt it stops.
	wire    *turnWire
	waiting map[string]pendingAsk
	seq     map[string]int
	// turn counts prompts; cancelledTurn records the one a cancel was issued
	// for, so a request that arrives while the turn is being cancelled is
	// answered instead of parked. Which turn a *card* belongs to is the client's
	// turn, not this one: only the client knows when a request arrived, and a
	// handler goroutine can start long after that (see park).
	turn          int
	cancelledTurn int
	snap          Snapshot
	sessionID     string
	tools         map[string]ToolEvent
	toolOrder     []string
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
	// queue is craze's own message queue (§3.1). queueOp orders whole
	// transactions — the mutation and the events it produced — so the event
	// stream can be replayed into the same queue. emitMu is taken inside
	// queueOp and held across the transaction's emits, which happen after
	// queueOp is released: emit blocks on a full event channel, and a reader
	// that is waiting on something holding queueOp would wedge the session.
	// Lock order: queueOp → emitMu → s.mu → the queue's own lock.
	queueOp sync.Mutex
	emitMu  sync.Mutex
	queue   PromptQueue
	// doneEmitted marks that the turn is over — Prompt has returned, either
	// way. inPrompt is still true until Prompt's defer runs, so it alone
	// cannot say whether there is still a turn to interject into.
	doneEmitted bool
	// cancelling holds from Cancel until the turn ends. An interjection sent
	// in that window is exactly what grok strands.
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
	// wireFailed: the write failed or the connection was already closed, so
	// nothing reached the agent.
	wireFailed
)

// turnWire carries one turn's wire outcome from Prompt, which learns it, to
// Cancel, which has to wait for it. Each turn gets its own: grok's writer can
// report a write after the turn it belongs to has ended, and a report into the
// old turn's value can then no longer speak for the next one.
type turnWire struct {
	outcome wireOutcome // written under s.mu, before done is closed
	done    chan struct{}
}

// publishWire settles a turn's wire outcome. The first outcome wins and later
// ones change nothing, so every path that learns something may say it: the
// written bytes, the client's return, and the turn's release. It takes only
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

// testBeforeWire runs, when set, right before Prompt hands its request to the
// client: the one point where a turn is open and its prompt is not yet on the
// wire, which a test can only hold still from here. It is a var only so the
// tests can set it; nothing in craze writes it.
var testBeforeWire func(s *session)

// pendingAsk is one blocking request the UI still owes an answer to. kind is
// askPermission, askQuestion or askPlan; decide carries that kind's own
// acp decision type. turn is the client turn the request arrived in, which is
// what keeps a card out of a later turn.
type pendingAsk struct {
	kind    string
	turn    int
	options []acp.PermissionOption
	decide  chan any
}

const (
	askPermission = "perm"
	askQuestion   = "ask"
	askPlan       = "plan"
)

func New(opts Options) Session {
	return newSession(opts)
}

func newSession(opts Options) *session {
	s := &session{
		opts:      opts,
		events:    make(chan Event, 256),
		done:      make(chan struct{}),
		closeDone: make(chan struct{}),
		waiting:   make(map[string]pendingAsk),
		seq:       make(map[string]int),
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
	return s.events
}

func (s *session) Start(ctx context.Context) error {
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

	args := s.provider().Args(s.opts.ExtraArgs, s.opts.Force)

	cwd := s.opts.Workspace
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			s.unstart()
			return err
		}
	}
	cwd, err := filepath.Abs(cwd)
	if err != nil {
		s.unstart()
		return err
	}

	client, err := acp.Spawn(acp.SpawnOptions{
		Binary:     s.opts.Binary,
		Candidates: s.provider().Bins(),
		Args:       args,
		Dir:        cwd,
		Env:        s.opts.Env,
		Stderr:     s.opts.Stderr,
		Dialect:    s.provider().Dialect(),
	})
	if err != nil {
		s.unstart()
		return err
	}
	// Close may have run while we were spawning; adopt the child only if the
	// session is still open, otherwise reap it here so it cannot be orphaned.
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = client.Close()
		return fmt.Errorf("agent: session closed")
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
	client.SetInterjectionHandler(func(n acp.InterjectionNotification) {
		s.onInterjection(interjectionText{Text: n.Text})
	})
	client.SetForeignTurnHandler(func(f acp.ForeignTurn) {
		s.onForeignTurn(ForeignTurnInfo{ID: f.ID, Text: f.Text, Running: f.Running})
	})

	initRes, err := client.Initialize(ctx)
	if err != nil {
		_ = s.Close()
		return err
	}
	if methodID, meta, ok := s.authMethod(initRes); ok {
		if err := client.Authenticate(ctx, methodID, meta); err != nil {
			_ = s.Close()
			return fmt.Errorf("%w (run `%s`)", err, s.provider().LoginHint())
		}
	} else if len(initRes.AuthMethods) > 0 && len(s.provider().AuthMethodIDs()) > 0 {
		// The daemon offered only methods craze will not start (interactive
		// browser login); say how to fix it instead of hanging later.
		_ = s.Close()
		return fmt.Errorf("agent: no supported auth method (run `%s`)", s.provider().LoginHint())
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
		if err := s.loadSession(ctx, client, initRes, cwd); err != nil {
			_ = s.Close()
			return err
		}
	} else {
		sess, err := client.NewSession(ctx, cwd)
		if err != nil {
			_ = s.Close()
			return err
		}
		snap = snapshotFromNewProvider(sess, s.provider(), initRes)
		snap.Provider = s.provider().Info()
		snap.SessionID = sess.SessionID
		s.mu.Lock()
		s.sessionID = sess.SessionID
		s.mu.Unlock()
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
			_ = s.Close()
			return fmt.Errorf("agent: session did not advertise mode %q", s.opts.Mode)
		}
		if err := client.SetMode(ctx, modeID); err != nil {
			_ = s.Close()
			return err
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
			_ = s.Close()
			return err
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
		return nil
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
	return nil
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
func (s *session) loadSession(ctx context.Context, client *acp.Client, initRes *acp.InitializeResult, cwd string) error {
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
	warn := func(msg string) {
		if s.opts.Stderr == nil {
			return
		}
		fmt.Fprintln(s.opts.Stderr, msg)
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
// so the refusal costs the refused caller nothing to roll back.
func (s *session) awaitCatalog(ctx context.Context, text string) (chan struct{}, error) {
	s.mu.Lock()
	if s.catalogAbort != nil {
		s.mu.Unlock()
		return nil, acp.ErrPromptInFlight
	}
	applied := s.commandsApplied
	var abort chan struct{}
	if !s.commandsSeen && len(s.plugins) > 0 && hasUnresolvedSlash(text, s.pluginLookupLocked()) {
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
// before anything has been claimed — or finds the turn and cancels that.
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

// abortCatalogWait ends a pending catalog wait, at most once, and reports
// whether it ended one: the registration is dropped under the same lock that
// closes it, so a second Cancel finds nothing to close and answers false. A
// prompt already on the wire is not this — that turn is the agent's to cancel —
// which is why a registration is only honoured while there is no turn in
// flight.
func (s *session) abortCatalogWait() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inPrompt || s.catalogAbort == nil {
		return false
	}
	close(s.catalogAbort)
	s.catalogAbort = nil
	return true
}

func (s *session) Prompt(ctx context.Context, text string) (Result, error) {
	// Before any of the turn's bookkeeping: this waits, and a turn that has
	// been opened must not be left open across a wait — nothing has been
	// claimed yet, so a caller that gives up here gives up on nothing. Its
	// refusal comes back the same way: the slot was already another prompt's,
	// and returning here spends no turn number and rolls nothing back.
	abort, err := s.awaitCatalog(ctx, text)
	if err != nil {
		return Result{}, err
	}
	s.mu.Lock()
	if s.clearCatalogWaitLocked(abort) {
		// Cancelled while waiting. Nothing was opened, nothing was sent and
		// nothing will be emitted, so saying so to the caller is the whole of
		// it — the turn it drew is its own to settle.
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
	// A new turn: it has not ended and no cancel has been asked for it, so
	// an interjection is live again.
	s.doneEmitted = false
	s.cancelling = false
	done := make(chan struct{})
	s.promptDone = done
	// The wire outcome is opened with the turn, so a Cancel that finds the
	// turn always finds its wire too.
	wire := &turnWire{done: make(chan struct{})}
	s.wire = wire
	// The references are read in the same locked section as everything else
	// the turn depends on: an available_commands_update landing now either
	// renamed the entries before this prompt resolved them or after, never
	// halfway through.
	refs := pluginRefs(text, s.pluginLookupLocked())
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		// The client's return has settled the wire by now; this is the
		// backstop for a turn that ends any other way, so that no Cancel can
		// outlive the turn waiting on its wire.
		wire.publishLocked(wireFailed)
		if s.wire == wire {
			s.wire = nil
		}
		s.inPrompt = false
		s.cancelling = false
		s.clearTaskReceiptsLocked()
		close(done)
		s.mu.Unlock()
	}()

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
		if s.cancelledTurn != s.turn {
			// A cancel that landed on this number has already spent it: giving
			// it back would leave the marker pointing at the turn after this
			// one, which nobody cancelled.
			s.turn = prevTurn
		}
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
	if err != nil {
		// The error goes out first, so a consumer is already in its error
		// state when the removals arrive and can say why the queue emptied.
		s.emit(Event{Type: EventError, Err: err})
		// Nothing drains from an error state, and a queue that outlived one
		// would run behind whatever the user sends next.
		s.clearQueueOnError()
		return Result{}, err
	}
	if res.StopReason == acp.StopCancelled {
		s.closeInFlightTools(acp.StopCancelled)
	}
	s.emit(Event{Type: EventDone, StopReason: res.StopReason})
	return Result{StopReason: res.StopReason}, nil
}

// refusedBeforeWire reports an error that means the prompt never reached the
// agent: a turn craze did not start is running, or one of craze's own still
// is. Nothing was attempted and no queued message was lost, so the refusal is
// the caller's to retry — it is not a turn that failed, and the queue it would
// have drained stays exactly as it was.
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
	// s.mu → the queue's lock is the order every queue transaction takes;
	// reading them the other way round here would close the cycle.
	out.Queue = s.queue.List()
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

func (s *session) Cancel(ctx context.Context) error {
	client := s.clientRef()
	if client == nil {
		return nil
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
	if s.abortCatalogWait() {
		return nil
	}
	// From here until the turn ends an interjection would be stranded, which
	// is what makes grok mint a turn of its own. The turn and its wire are read
	// in the same section that marks it, so the wire is the marked turn's own:
	// Prompt sets both in one locked section and clears both in another.
	s.mu.Lock()
	s.cancelling = true
	in, wire := s.inPrompt, s.wire
	s.mu.Unlock()
	s.cancelWaiting()
	if !in || wire == nil {
		// No turn of craze's own is open: nothing is running, or the agent is
		// running one it started itself. Either way there is no prompt of
		// ours for the cancel to overtake, so it goes now.
		return client.Cancel(ctx)
	}
	// Prompt raises inPrompt before its request is written, so the cancel
	// waits for the wire to say what became of it: written first, a cancel
	// can only land behind it. Without the wait it could reach the agent
	// first, be dropped as belonging to no turn, and leave the prompt running
	// as if Esc had never been pressed. The wait is bounded by the caller's
	// context, the same one the wait on promptDone below has always used; it
	// is one marshal and one pipe write unless the agent has stopped reading,
	// and then saying the cancel failed is the honest answer.
	select {
	case <-wire.done:
	case <-ctx.Done():
		return ctx.Err()
	case <-s.done:
		return nil
	}
	switch s.outcomeOf(wire) {
	case wireSent, wireRefused:
		// Each Cancel writes once at most: here, or on the no-turn path above.
		// Two racing Cancels can each write one, which the agent tolerates.
		// A refused prompt is cancelled too: the refusal that reaches here
		// is a foreign turn, and that turn is still running.
		if err := client.Cancel(ctx); err != nil {
			return err
		}
	default:
		// wireFailed: the prompt never reached the agent, so there is no turn
		// there to stop and nothing is written.
	}
	// The wait is for the turn this cancel was for, and only that one: the
	// release clears s.wire together with inPrompt, so a wire that is no
	// longer s.wire means that turn is over and whatever runs now started
	// after this cancel.
	s.mu.Lock()
	done := s.promptDone
	in = s.inPrompt && s.wire == wire
	s.mu.Unlock()
	if !in || done == nil {
		return nil
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// cancelWaiting answers every blocking request of every kind, exactly once:
// the map is swapped out under the lock so a concurrent Answer* finds nothing.
func (s *session) cancelWaiting() {
	s.mu.Lock()
	s.cancelledTurn = s.turn
	pending := s.waiting
	s.waiting = make(map[string]pendingAsk)
	s.mu.Unlock()
	for _, p := range pending {
		p.answer(cancelledFor(p.kind))
	}
}

func cancelledFor(kind string) any {
	switch kind {
	case askQuestion:
		return acp.AskDecision{Cancelled: true}
	case askPlan:
		return acp.PlanDecision{Cancelled: true}
	default:
		return acp.PermissionDecision{Cancelled: true}
	}
}

// answer never blocks: the channel is buffered and each pendingAsk is taken
// out of the map before it is answered, so at most one value is ever sent.
func (p pendingAsk) answer(v any) {
	select {
	case p.decide <- v:
	default:
	}
}

// take removes a waiting request of the expected kind.
func (s *session) take(id, kind string) (pendingAsk, error) {
	s.mu.Lock()
	p, ok := s.waiting[id]
	if ok && p.kind == kind {
		delete(s.waiting, id)
	}
	s.mu.Unlock()
	if !ok || p.kind != kind {
		return pendingAsk{}, fmt.Errorf("agent: unknown %s request %q", kind, id)
	}
	return p, nil
}

func (s *session) AnswerPermission(id, optionID string) error {
	p, err := s.take(id, askPermission)
	if err != nil {
		return fmt.Errorf("agent: unknown permission request %q", id)
	}
	if optionID == "" {
		p.answer(acp.PermissionDecision{Cancelled: true})
		return nil
	}
	found := false
	for _, o := range p.options {
		if o.OptionID == optionID {
			found = true
			break
		}
	}
	if !found {
		p.answer(acp.PermissionDecision{Cancelled: true})
		return fmt.Errorf("agent: optionId %q is not in the permission request", optionID)
	}
	p.answer(acp.PermissionDecision{OptionID: optionID})
	return nil
}

// AnswerQuestion answers a cursor/ask_question card. Option ids that the
// request did not offer are dropped when the reply is built.
func (s *session) AnswerQuestion(id string, answers map[string][]string, skip bool) error {
	p, err := s.take(id, askQuestion)
	if err != nil {
		return err
	}
	if skip {
		p.answer(acp.AskDecision{Skip: true})
		return nil
	}
	p.answer(acp.AskDecision{Answers: answers})
	return nil
}

// AnswerPlan accepts or rejects a cursor/create_plan card.
func (s *session) AnswerPlan(id string, accept bool) error {
	p, err := s.take(id, askPlan)
	if err != nil {
		return err
	}
	p.answer(acp.PlanDecision{Accept: accept})
	return nil
}

// Close reaps the child exactly once; later callers block until that reap has
// finished rather than returning while the child is still alive.
func (s *session) Close() error {
	s.closeOnce.Do(func() {
		defer close(s.closeDone)
		s.mu.Lock()
		s.closed = true
		close(s.done)
		client := s.client
		s.mu.Unlock()
		if client != nil {
			_ = client.Close()
		}
	})
	<-s.closeDone
	return nil
}

func (s *session) onPermission(turn int, req acp.PermissionRequest) acp.PermissionDecision {
	if s.opts.Force {
		id, ok := acp.PickYoloAllow(req.Options)
		if !ok {
			return acp.PermissionDecision{Cancelled: true}
		}
		return acp.PermissionDecision{OptionID: id}
	}
	id, ch, ok := s.park(askPermission, turn, pendingAsk{options: req.Options})
	if !ok {
		return acp.PermissionDecision{Cancelled: true}
	}

	opts := make([]PermissionOption, 0, len(req.Options))
	for _, o := range req.Options {
		opts = append(opts, PermissionOption{
			OptionID: o.OptionID,
			Name:     sanitizeText(o.Name),
			Kind:     o.Kind,
		})
	}
	if !s.emitParked(id, Event{
		Type: EventPermission,
		Permission: &PermissionEvent{
			ID:      id,
			Tool:    sanitizeText(req.ToolCall.Title),
			Options: opts,
		},
	}) {
		return acp.PermissionDecision{Cancelled: true}
	}
	return awaitDecision(s, ch, acp.PermissionDecision{Cancelled: true})
}

// onAskQuestion blocks on the UI when Interactive, and otherwise answers with
// each question's first option and reports what it sent.
func (s *session) onAskQuestion(turn int, req acp.AskQuestionRequest) acp.AskDecision {
	ev := &QuestionEvent{
		Title:     sanitizeText(req.Title),
		Questions: questionsFromRequest(req),
	}
	if !s.opts.Interactive {
		ev.ID = s.nextID(askQuestion)
		ev.Auto = true
		ev.Answers = acp.AskAutoAnswers(req)
		s.emit(Event{Type: EventQuestion, Question: ev})
		return acp.AskDecision{Answers: ev.Answers}
	}
	id, ch, ok := s.park(askQuestion, turn, pendingAsk{})
	if !ok {
		return acp.AskDecision{Cancelled: true}
	}
	ev.ID = id
	if !s.emitParked(id, Event{Type: EventQuestion, Question: ev}) {
		return acp.AskDecision{Cancelled: true}
	}
	return awaitDecision(s, ch, acp.AskDecision{Cancelled: true})
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

// onCreatePlan mirrors onAskQuestion: block when Interactive, accept and
// report otherwise.
func (s *session) onCreatePlan(turn int, req acp.CreatePlanRequest) acp.PlanDecision {
	ev := &PlanEvent{
		Name:     sanitizeText(req.DisplayName()),
		Overview: sanitizeText(req.Overview),
		Plan:     sanitizeText(req.PlanText()),
		Todos:    todosFromWire(req.Todos),
	}
	if !s.opts.Interactive {
		ev.ID = s.nextID(askPlan)
		ev.Auto = true
		ev.Accepted = true
		s.emit(Event{Type: EventPlan, Plan: ev})
		return acp.PlanDecision{Accept: true}
	}
	id, ch, ok := s.park(askPlan, turn, pendingAsk{})
	if !ok {
		return acp.PlanDecision{Cancelled: true}
	}
	ev.ID = id
	if !s.emitParked(id, Event{Type: EventPlan, Plan: ev}) {
		return acp.PlanDecision{Cancelled: true}
	}
	return awaitDecision(s, ch, acp.PlanDecision{Cancelled: true})
}

// park registers a blocking request and returns its craze-local id (perm-N,
// ask-N, plan-N — never a JSON-RPC id or a toolCallId) and decision channel.
// turn is the turn the request arrived in, handed to the handler by the client.
//
// It refuses once the request's own turn is over, the current turn has been
// cancelled, or the session closed. The turn check is what a handler goroutine
// the runtime delayed past the end of its turn runs into: cancelWaiting only
// drains what is already in the map, so without it a late request would park
// into the turn now running and raise a card for a plan that turn never made —
// and without the cancelled-turn check a request arriving while a cancel is in
// flight would park forever and leave the agent waiting on a reply that never
// comes.
func (s *session) park(kind string, turn int, p pendingAsk) (string, chan any, bool) {
	ch := make(chan any, 1)
	p.kind = kind
	p.turn = turn
	p.decide = ch
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || !s.turnLiveLocked(turn) || (s.turn > 0 && s.cancelledTurn == s.turn) {
		return "", nil, false
	}
	s.seq[kind]++
	id := fmt.Sprintf("%s-%d", kind, s.seq[kind])
	s.waiting[id] = p
	return id, ch, true
}

// emitParked publishes a card event only while its request is still parked and
// still belongs to the turn it arrived in. A cancel that landed between the
// park and the emit has already answered the request and taken it out of
// waiting, so emitting anyway would raise a card for a request nobody can
// answer any more; a turn that ended in that same window would put the card in
// front of the next turn, which is not the turn that asked for it. The waiting
// check takes the same lock cancelWaiting does; it cannot be held across the
// emit, because emit blocks on the event channel and the reader of that channel
// is the goroutine that answers cards.
func (s *session) emitParked(id string, ev Event) bool {
	s.mu.Lock()
	p, live := s.waiting[id]
	if live && !s.turnLiveLocked(p.turn) {
		// Nobody will ever answer it now, so it does not stay in the map: the
		// handler replies cancelled on its way out instead.
		delete(s.waiting, id)
		live = false
	}
	s.mu.Unlock()
	if !live {
		return false
	}
	s.emit(ev)
	return true
}

// turnLiveLocked reports whether the turn a request arrived in is still the one
// the client is running; callers hold s.mu. With no client — before Start, and
// in the unit tests — there is one turn and it is turn 0. The lock order is
// session then client: the client answers this without taking any lock the
// session holds, and it never calls back into the session under its own.
func (s *session) turnLiveLocked(turn int) bool {
	if s.client == nil {
		return turn == 0
	}
	return s.client.TurnLive(turn)
}

func (s *session) nextID(kind string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq[kind]++
	return fmt.Sprintf("%s-%d", kind, s.seq[kind])
}

// awaitDecision blocks for the UI's answer, falling back to onClose once the
// session is closing or if another kind's decision arrived.
func awaitDecision[T any](s *session, ch chan any, onClose T) T {
	select {
	case v := <-ch:
		if d, ok := v.(T); ok {
			return d
		}
		return onClose
	case <-s.done:
		return onClose
	}
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
func (s *session) emitCtx(ctx context.Context, ev Event) bool {
	if ev.At.IsZero() {
		ev.At = time.Now()
	}
	if s.replaying.Load() {
		ev.Replayed = true
	}
	select {
	case <-s.done:
		return false
	default:
	}
	select {
	case s.events <- ev:
		return true
	case <-s.done:
	case <-ctx.Done():
	}
	return false
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
