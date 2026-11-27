package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/charliek/craze/internal/acp"
	"github.com/charliek/craze/internal/agentproc"
	"github.com/charliek/craze/internal/codexrpc"
	"github.com/charliek/craze/internal/errkind"
	"github.com/charliek/craze/internal/journal"
)

// Compile-time interface assertions required by Craze's engine & host contracts
// (Plan 054 § C1).
var (
	_ Session        = (*codexSession)(nil)
	_ LogOwner       = (*codexSession)(nil)
	_ AskSource      = (*codexSession)(nil)
	_ EventSource    = (*codexSession)(nil)
	_ Clocked        = (*codexSession)(nil)
	_ SeedAnnouncer  = (*codexSession)(nil)
	_ EffortFactser  = (*codexSession)(nil)
	_ SlashCataloger = (*codexSession)(nil)
	_ SkillRescanner = (*codexSession)(nil)
)

var (
	codexCancelFallbackGrace   = 5 * time.Second
	codexStartRPCTimeout       = 30 * time.Second
	codexRateLimitStartTimeout = 5 * time.Second
)

// codexTurnPhase tracks a claimed prompt's lifecycle generation (Plan 054 § D1).
type codexTurnPhase int

const (
	codexPhaseIdle codexTurnPhase = iota
	codexPhaseClaimed
	codexPhaseSent
	codexPhaseIdentified
	codexPhaseSettled
)

// codexTurnState holds the per-turn state for one claimed prompt generation.
type codexTurnState struct {
	gen               uint64
	phase             codexTurnPhase
	settling          bool
	turnID            string
	idReady           chan struct{}
	idClosed          bool
	token             TurnToken
	mode              string
	model             string
	approvalPolicy    codexrpc.ApprovalPolicy
	cancelPending     bool
	interruptSent     bool
	interruptInFlight bool
	interruptDone     chan struct{}
	failedErr         error
	stopReason        string
	tempFiles         []string
	steerInFlight     int
	steerZeroCh       chan struct{}
	done              chan struct{}
}

func (t *codexTurnState) bindIDLocked(id string) {
	if id != "" && t.turnID == "" {
		t.turnID = id
	}
	if !t.idClosed {
		t.idClosed = true
		close(t.idReady)
	}
}

// codexError wraps a Codex failure with a classified errkind.Info.
type codexError struct {
	msg   string
	info  errkind.Info
	cause error
}

func (e *codexError) Error() string           { return e.msg }
func (e *codexError) Unwrap() error           { return e.cause }
func (e *codexError) ErrorInfo() errkind.Info { return e.info }

func newCodexErr(kind errkind.Kind, format string, args ...any) error {
	return &codexError{
		msg:  "agent: " + fmt.Sprintf(format, args...),
		info: errkind.New(kind),
	}
}

// codexSession implements agent.Session over `codex app-server --listen stdio://`.
//
// Lock ordering (strict): s.mu -> log.outbox (leaf). Never hold s.mu across RPC
// calls, Publish, Flush, Note, or blocking AskRegistry.Wait.
type codexSession struct {
	opts   Options
	log    *EventLog
	events <-chan Event
	asks   *AskRegistry
	tee    *stderrTee

	done       chan struct{}
	closingCh  chan struct{}
	lifeCtx    context.Context
	lifeCancel context.CancelFunc
	startWG    sync.WaitGroup
	closeOnce  sync.Once
	closeDone  chan struct{}
	logOnce    sync.Once
	closeErr   error

	mu            sync.Mutex
	started       bool
	ready         bool
	transportDead bool
	closed        bool
	seedOwed      bool
	titlePinned   bool
	skillsDirty   bool

	client   *codexrpc.Client
	cwd      string
	threadID string
	imgDir   string

	// Serialized coalescing worker for best-effort thread/name/set RPCs.
	titleCh      chan struct{}
	pendingTitle string
	titleWorker  bool

	// Saved workspaceWrite sandboxPolicy from thread/start|resume so agent-mode
	// turns preserve any user-configured writableRoots/networkAccess.
	workspaceSandbox *codexrpc.SandboxPolicy

	// Full model catalog from model/list (including hidden models for lookup).
	allModels     map[string]codexrpc.ModelEntry
	visibleOrder  []string
	currentEffort string
	catalogRev    uint64
	snap          Snapshot
	usage         codexUsage

	// Tool & per-turn streaming state.
	tools          map[string]ToolEvent
	toolOrder      []string
	cmdBuffers     map[string]*codexCmdBuffer
	turnItemDeltas map[string]bool
	lastMsgItemID  string

	// Subagent rows (settled per launching turn in unified settlement) and
	// receiver child-thread -> subagent row ID index.
	subagents           map[string]*subagentRec
	subagentOrder       []string
	childThreadSubagent map[string]string

	// Turn generation state machine.
	turnGen    uint64
	activeTurn *codexTurnState
	// Early completions arriving before turn/start reply (indexed by turnID).
	earlyCompleted map[string]codexrpc.Turn
	// Recently settled turn IDs, so late notifications from prior turns never
	// contaminate a successor generation.
	settledTurns     map[string]bool
	settledTurnOrder []string

	// Foreign turn state (autonomous main-thread turns).
	foreignTurnID string

	// Active server->client requests keyed by canonical JSON request ID so
	// serverRequest/resolved immediately withdraws parked asks.
	pendingReqs map[string]*codexPendingReq

	// Skills catalog from Codex's skills/list + coalescing async rescan worker.
	skills      []Skill
	codexSkills map[string]codexrpc.SkillMetadata
	skillCh     chan struct{}
	skillWorker bool
}

func newCodexSession(opts Options) *codexSession {
	p := CodexProvider()
	if opts.Provider != nil {
		p = *opts.Provider
	}
	opts.Provider = &p
	opts.PluginDirs = append([]string(nil), opts.PluginDirs...)

	lifeCtx, lifeCancel := context.WithCancel(context.Background())
	s := &codexSession{
		opts:                opts,
		done:                make(chan struct{}),
		closingCh:           make(chan struct{}),
		lifeCtx:             lifeCtx,
		lifeCancel:          lifeCancel,
		closeDone:           make(chan struct{}),
		titleCh:             make(chan struct{}, 1),
		skillCh:             make(chan struct{}, 1),
		allModels:           make(map[string]codexrpc.ModelEntry),
		tools:               make(map[string]ToolEvent),
		cmdBuffers:          make(map[string]*codexCmdBuffer),
		turnItemDeltas:      make(map[string]bool),
		subagents:           make(map[string]*subagentRec),
		childThreadSubagent: make(map[string]string),
		earlyCompleted:      make(map[string]codexrpc.Turn),
		settledTurns:        make(map[string]bool),
		pendingReqs:         make(map[string]*codexPendingReq),
		codexSkills:         make(map[string]codexrpc.SkillMetadata),
	}
	s.log = newSessionLog(s.opts, journalHeader{provider: p.Name(), binary: s.opts.Binary})
	s.events = s.log.Primary()
	s.asks = NewAskRegistry(s.log, s.Now)
	s.tee = newStderrTee(s.opts.Stderr, s.log)

	s.snap.Provider = p.Info()
	s.snap.Modes = append([]ModeInfo(nil), codexModesInfo...)
	s.snap.CurrentMode = codexAgentMode
	s.seedOwed = seedNewTitle(&s.snap, &s.titlePinned, opts)
	return s
}

func (s *codexSession) provider() Provider {
	if s.opts.Provider != nil {
		return *s.opts.Provider
	}
	return CodexProvider()
}

func (s *codexSession) Events() <-chan Event { return s.events }
func (s *codexSession) Subscribe(o SubscribeOptions) (*Subscription, error) {
	return s.log.Subscribe(o)
}
func (s *codexSession) Incarnation() string         { return s.log.Incarnation() }
func (s *codexSession) EventLog() *EventLog         { return s.log }
func (s *codexSession) Now() time.Time              { return time.Now() }
func (s *codexSession) Asks() *AskRegistry          { return s.asks }
func (s *codexSession) PublishesEffortFacts() bool  { return true }
func (s *codexSession) PublishesSlashCatalog() bool { return true }

func (s *codexSession) AnnounceSeed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.seedOwed {
		return false
	}
	s.seedOwed = false
	title := s.snap.Title
	s.enqueueDeltaLocked("", Event{}, &StateDelta{Title: &title})
	return true
}

func (s *codexSession) ForeignTurn() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.foreignTurnID != ""
}

func (s *codexSession) isMainThread(threadID string) bool {
	s.mu.Lock()
	main := s.threadID
	s.mu.Unlock()
	if main == "" {
		return false
	}
	return threadID != "" && threadID == main
}

func (s *codexSession) workspace() string {
	s.mu.Lock()
	cwd := s.cwd
	s.mu.Unlock()
	if cwd != "" {
		return cwd
	}
	abs, err := absWorkspace(s.opts.Workspace)
	if err != nil {
		return s.opts.Workspace
	}
	return abs
}

func (s *codexSession) stderrSink() io.Writer {
	if s.tee != nil {
		return s.tee
	}
	return s.opts.Stderr
}

func (s *codexSession) diagLine(line string) {
	if w := diagWriter(s.opts); w != nil {
		fmt.Fprintln(w, line)
	}
}

func (s *codexSession) emit(ev Event) {
	s.emitCtx(context.Background(), ev)
}

func (s *codexSession) emitCtx(ctx context.Context, ev Event) bool {
	if ev.At.IsZero() {
		ev.At = s.Now()
	}
	return s.log.Publish(ctx, s.done, ev)
}

func (s *codexSession) enqueueDeltaLocked(cause string, base Event, st *StateDelta) *Ticket {
	base.Type = EventMeta
	base.Cause = cause
	base.State = st
	if base.At.IsZero() {
		base.At = s.Now()
	}
	return s.log.EnqueueTicket(base)
}

func (s *codexSession) publishUsageLocked() {
	u := s.usage.state()
	s.snap.Usage = &u
	if !s.ready {
		return
	}
	d := u
	ev := Event{Type: EventMeta, State: &StateDelta{Usage: &d}, At: s.Now()}
	s.log.EnqueueLatest("usage", ev)
}

func (s *codexSession) decorateStartErr(err error) error {
	if !s.opts.StartErrAgentStderr || err == nil {
		return err
	}
	var exit *codexrpc.ExitError
	if !errors.As(err, &exit) {
		return err
	}
	s.mu.Lock()
	c := s.client
	s.mu.Unlock()
	var raw []byte
	if c != nil {
		raw = c.StderrTail(agentproc.StderrWait(startErrStderrWait))
	}
	return withAgentWords(err, stderrTailLine(raw), s.opts.StartErrExitHint)
}

// codexStartMode validates Options.Mode against codexStartModes before spawn.
func (s *codexSession) codexStartMode() (string, error) {
	if s.opts.Mode == "" {
		return codexAgentMode, nil
	}
	id, ok := ResolveMode(s.opts.Mode, codexStartModes)
	if !ok {
		return "", newCodexErr(errkind.Config, "mode %s is not advertised", sanitizeLine(s.opts.Mode))
	}
	return id, nil
}

// Start spawns `codex app-server --listen stdio://`, runs auth preflight, loads
// the model catalog, opens (or resumes) the thread, and enqueues the install
// delta (never flushing on a new session per Session.Start).
func (s *codexSession) Start(ctx context.Context) error {
	teardown, err := s.start(ctx)
	if err == nil {
		return nil
	}
	err = s.decorateStartErr(err)
	s.log.noteStartFailed(err)
	if teardown {
		if s.opts.KeepLogOnFailedStart {
			s.teardown()
		} else {
			_ = s.Close()
		}
	}
	return err
}

// startCtx joins caller ctx with session lifetime (s.lifeCtx) and bounds
// startup RPCs by codexStartRPCTimeout so a stalled app-server cannot trap
// detached startup indefinitely (H5).
func (s *codexSession) startContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	base, cancel := context.WithTimeout(ctx, codexStartRPCTimeout)
	stop := context.AfterFunc(s.lifeCtx, cancel)
	return base, func() {
		stop()
		cancel()
	}
}

func (s *codexSession) start(ctx context.Context) (teardown bool, err error) {
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
	s.startWG.Add(1)
	s.mu.Unlock()
	defer s.startWG.Done()

	unstart := func() {
		s.mu.Lock()
		s.started = false
		s.mu.Unlock()
	}

	rpcCtx, rpcCancel := s.startContext(ctx)
	defer rpcCancel()

	startMode, err := s.codexStartMode()
	if err != nil {
		unstart()
		return false, err
	}

	if len(s.opts.PluginDirs) > 0 {
		s.diagLine("plugin dirs ignored for " + codexName)
	}

	cwd, err := absWorkspace(s.opts.Workspace)
	if err != nil {
		unstart()
		return false, err
	}

	p := s.provider()
	resolve := acp.ResolveBinaryCandidates
	if s.opts.NoBinaryEnv {
		resolve = acp.LookupBinary
	}
	binary, err := resolve(s.opts.Binary, p.Bins())
	if err != nil {
		unstart()
		return false, binaryNotFound(err, p.Name())
	}

	if err := rpcCtx.Err(); err != nil {
		unstart()
		return false, err
	}

	env := p.spawnEnv(s.opts.Env, "", cwd)
	args := p.Args(s.opts.ExtraArgs, s.opts.Force)

	client, err := codexrpc.Spawn(codexrpc.SpawnOptions{
		Binary: binary,
		Args:   args,
		Dir:    cwd,
		Env:    env,
		Stderr: s.stderrSink(),
	})
	if err != nil {
		unstart()
		return false, err
	}

	if g := s.opts.AgentGroup; g != nil {
		if err := g(client.ProcessGroup()); err != nil {
			_ = client.Close()
			unstart()
			return false, fmt.Errorf("agent: the agent's process group could not be recorded: %w", err)
		}
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = client.Close()
		return false, fmt.Errorf("agent: session closed")
	}
	s.client = client
	s.cwd = cwd
	s.snap.CurrentMode = startMode
	s.mu.Unlock()

	client.SetNotifyHandler(s.onNotify)
	client.SetRequestHandler(s.onServerRequest)
	go s.watchTransportDone(client)

	initRes, err := client.Initialize(rpcCtx)
	if err != nil {
		return true, fmt.Errorf("agent: codex initialize: %w", err)
	}
	if initRes != nil && initRes.UserAgent != "" {
		s.log.Note(journal.DiagNote{
			Kind:   "codex_init",
			Fields: map[string]any{"userAgent": sanitizeLine(initRes.UserAgent)},
		})
	}

	// Auth preflight (Plan 054 § C2): catch signed-out state in <50ms instead of
	// hanging through 5 WebSocket 401 retries, while permitting custom providers
	// where requiresOpenaiAuth == false.
	acctRes, err := client.AccountRead(rpcCtx, false)
	if err != nil {
		return true, fmt.Errorf("agent: codex account/read: %w", err)
	}
	if acctRes != nil {
		if acctRes.Account == nil && acctRes.RequiresOpenaiAuth {
			return true, newCodexErr(errkind.Auth, "codex is not signed in (run `%s`, or `%s --with-api-key` for API-key auth)", codexLoginHint, codexLoginHint)
		}
		if acctRes.Account != nil {
			s.mu.Lock()
			if pt := strings.TrimSpace(acctRes.Account.PlanType); pt != "" {
				s.usage.plan = sanitizeLine(pt)
			}
			s.mu.Unlock()
			s.log.Note(journal.DiagNote{
				Kind: "codex_account",
				Fields: map[string]any{
					"type":     sanitizeLine(acctRes.Account.Type),
					"planType": sanitizeLine(acctRes.Account.PlanType),
				},
			})
		}
	}

	// Best-effort rate-limit read with a shorter independent deadline so an
	// unresponsive rate-limit endpoint never blocks startup (H5).
	rlCtx, rlCancel := context.WithTimeout(rpcCtx, codexRateLimitStartTimeout)
	rlRes, rlErr := client.AccountRateLimitsRead(rlCtx)
	rlCancel()
	if rlErr == nil && rlRes != nil {
		s.mu.Lock()
		s.usage.mergeRateLimitsRead(rlRes)
		s.mu.Unlock()
	}

	// Load model catalog with includeHidden: true and pagination.
	if err := s.loadModelCatalog(rpcCtx, client); err != nil {
		return true, fmt.Errorf("agent: codex model/list: %w", err)
	}

	// Best-effort Codex skills discovery with a short independent deadline so
	// slow skills/list never stalls startup.
	skCtx, skCancel := context.WithTimeout(rpcCtx, codexSkillStartTimeout)
	skills, byName, skErr := loadCodexSkills(skCtx, client, cwd, false)
	skCancel()
	if skErr == nil {
		s.mu.Lock()
		s.skills = skills
		s.codexSkills = byName
		s.mu.Unlock()
	}

	// Determine requested model & validate against catalog (allowing custom
	// models not in model/list to be validated by Codex itself on thread/start).
	wantModel := strings.TrimSpace(s.opts.Model)
	if wantModel == "" && s.opts.LoadSessionID != "" {
		wantModel = strings.TrimSpace(s.opts.LoadModel)
	}
	resolvedModel := s.resolveRequestedModel(wantModel)

	// Select initial mode's thread sandbox & approvalPolicy.
	sbMode := s.threadSandboxForMode(startMode)
	apPolicy := s.approvalPolicyForMode(startMode)

	if s.opts.LoadSessionID != "" {
		// Resume path (extended with full paginated replay in M2b).
		if err := s.resumeThread(rpcCtx, client, cwd, binary, startMode, resolvedModel, sbMode, apPolicy); err != nil {
			return true, err
		}
		return false, nil
	}

	ephemeral := false
	tr, err := client.ThreadStart(rpcCtx, codexrpc.ThreadStartParams{
		Cwd:            cwd,
		Sandbox:        sbMode,
		ApprovalPolicy: apPolicy,
		Model:          resolvedModel,
		Ephemeral:      &ephemeral,
	})
	if err != nil {
		return true, s.classifyRPCStartError(err)
	}
	if tr == nil || tr.Thread.ID == "" {
		return true, newCodexErr(errkind.StartFailed, "codex thread/start returned no thread id")
	}

	// Capture returned workspaceWrite sandbox policy when available so agent-mode
	// turns preserve user-configured writableRoots / networkAccess.
	var savedWS *codexrpc.SandboxPolicy
	if tr.Sandbox != nil && tr.Sandbox.Type == codexrpc.SandboxPolicyWorkspaceWrite {
		cp := *tr.Sandbox
		cp.WritableRoots = slices.Clone(tr.Sandbox.WritableRoots)
		savedWS = &cp
	}

	activeModel := resolvedModel
	if tr.Model != "" {
		activeModel = tr.Model
	}
	if activeModel == "" {
		activeModel = s.defaultCatalogModel()
	}
	activeEffort := ""
	if tr.ReasoningEffort != nil {
		activeEffort = strings.TrimSpace(*tr.ReasoningEffort)
	}
	if activeEffort == "" {
		activeEffort = s.defaultEffortForModel(activeModel)
	}

	// Apply --effort and --fast start settings (Plan 054 § C4).
	activeEffort, err = s.applyStartSettings(activeModel, activeEffort)
	if err != nil {
		return true, err
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return true, fmt.Errorf("agent: session closed")
	}
	s.threadID = tr.Thread.ID
	s.workspaceSandbox = savedWS
	s.snap.SessionID = tr.Thread.ID
	s.snap.CurrentMode = startMode
	s.snap.CurrentModel = activeModel
	s.currentEffort = activeEffort
	s.rebuildCatalogAndConfigLocked()
	s.rebuildSlashLocked()
	if s.usage.hasAny() {
		u := s.usage.state()
		u.Model = activeModel
		s.snap.Usage = &u
	}
	s.ready = true
	dirtySkills := s.skillsDirty
	s.skillsDirty = false
	delta := s.installDeltaLocked()
	// New session Start enqueues install delta (with non-nil Config) and NEVER flushes.
	s.enqueueDeltaLocked("", Event{}, delta)
	s.mu.Unlock()

	s.log.noteSession(journal.SessionNote{ProviderSessionID: tr.Thread.ID, AgentBinary: binary})
	if dirtySkills {
		s.triggerSkillRefresh()
	}
	return false, nil
}

func (s *codexSession) classifyRPCStartError(err error) error {
	var rpc *codexrpc.RPCError
	if errors.As(err, &rpc) && rpc != nil {
		info := rpc.ErrorInfo()
		if info.Kind == errkind.Auth {
			return &codexError{
				msg:   fmt.Sprintf("agent: %s (run `%s`)", sanitizeLine(rpc.Message), codexLoginHint),
				info:  info,
				cause: err,
			}
		}
		if info.Kind != errkind.Agent {
			return &codexError{
				msg:   "agent: " + sanitizeLine(rpc.Message),
				info:  info,
				cause: err,
			}
		}
	}
	return err
}

func (s *codexSession) loadModelCatalog(ctx context.Context, client *codexrpc.Client) error {
	all := make(map[string]codexrpc.ModelEntry)
	var visible []string
	cursor := ""
	seenCursors := make(map[string]bool)
	const maxPages = 20
	for page := range maxPages {
		res, err := client.ModelList(ctx, codexrpc.ModelListParams{
			IncludeHidden: true,
			Cursor:        cursor,
		})
		if err != nil {
			return err
		}
		if res == nil {
			break
		}
		for _, m := range res.Data {
			id := strings.TrimSpace(m.Model)
			if id == "" {
				id = strings.TrimSpace(m.ID)
			}
			if id == "" {
				continue
			}
			m.ID = id
			m.Model = id
			if _, seen := all[id]; !seen {
				all[id] = m
				if !m.Hidden {
					visible = append(visible, id)
				}
			}
		}
		if res.NextCursor == nil || *res.NextCursor == "" {
			break
		}
		next := *res.NextCursor
		if seenCursors[next] {
			return fmt.Errorf("repeated pagination cursor %q", sanitizeLine(next))
		}
		seenCursors[next] = true
		cursor = next
		if page == maxPages-1 {
			return fmt.Errorf("exceeded %d model catalog pages with remaining cursor %q", maxPages, sanitizeLine(next))
		}
	}
	s.mu.Lock()
	s.allModels = all
	s.visibleOrder = visible
	s.mu.Unlock()
	return nil
}

func (s *codexSession) resolveModelLocked(want string) (string, bool) {
	if m, ok := s.allModels[want]; ok {
		return m.ID, true
	}
	norm := normalizeIdent(want)
	// Search visible models first in server order, then remaining hidden models
	// in sorted order so alias resolution is 100% deterministic.
	for _, id := range s.visibleOrder {
		m := s.allModels[id]
		if normalizeIdent(id) == norm || normalizeIdent(m.DisplayName) == norm {
			return id, true
		}
	}
	hidden := make([]string, 0, len(s.allModels))
	for id := range s.allModels {
		if !slices.Contains(s.visibleOrder, id) {
			hidden = append(hidden, id)
		}
	}
	slices.Sort(hidden)
	for _, id := range hidden {
		m := s.allModels[id]
		if normalizeIdent(id) == norm || normalizeIdent(m.DisplayName) == norm {
			return id, true
		}
	}
	return want, false
}

func (s *codexSession) resolveRequestedModel(want string) string {
	if want == "" {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	resolved, _ := s.resolveModelLocked(want)
	// Allow custom/unlisted models (e.g., custom model_provider) through to
	// thread/start so Codex itself validates them (Plan 054 § C4).
	return resolved
}

func (s *codexSession) defaultCatalogModel() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range s.visibleOrder {
		if m := s.allModels[id]; m.IsDefault {
			return id
		}
	}
	if len(s.visibleOrder) > 0 {
		return s.visibleOrder[0]
	}
	for id := range s.allModels {
		return id
	}
	return ""
}

func (s *codexSession) defaultEffortForModel(modelID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m, ok := s.allModels[modelID]; ok {
		if m.DefaultReasoningEffort != "" {
			return m.DefaultReasoningEffort
		}
		if len(m.SupportedReasoningEfforts) > 0 {
			return m.SupportedReasoningEfforts[0].ReasoningEffort
		}
	}
	return ""
}

func (s *codexSession) effortsForModelLocked(modelID string) (efforts []string, def string) {
	m, ok := s.allModels[modelID]
	if !ok {
		return nil, ""
	}
	for _, e := range m.SupportedReasoningEfforts {
		if e.ReasoningEffort != "" && !slices.Contains(efforts, e.ReasoningEffort) {
			efforts = append(efforts, e.ReasoningEffort)
		}
	}
	def = m.DefaultReasoningEffort
	if def == "" && len(efforts) > 0 {
		def = efforts[0]
	}
	return efforts, def
}

func (s *codexSession) buildEffortOptionLocked(modelID, curEffort string) *ConfigOption {
	efforts, def := s.effortsForModelLocked(modelID)
	if len(efforts) == 0 {
		return nil
	}
	vals := make([]SelectValue, 0, len(efforts))
	for _, e := range efforts {
		vals = append(vals, SelectValue{Value: e, Name: e})
	}
	cur := curEffort
	if cur == "" || !slices.Contains(efforts, cur) {
		cur = def
	}
	return &ConfigOption{
		ID:           codexEffortID,
		Name:         "Effort",
		Category:     "thought_level",
		Type:         "select",
		Current:      cur,
		SelectValues: vals,
	}
}

func (s *codexSession) applyStartSettings(modelID, currentEffort string) (string, error) {
	notes := startNotes{log: s.log, say: s.diagLine}
	if s.opts.Fast != nil {
		notes.fastUnmatched(*s.opts.Fast)
	}
	wantEffort := strings.TrimSpace(s.opts.Effort)
	if wantEffort == "" {
		return currentEffort, nil
	}
	s.mu.Lock()
	opt := s.buildEffortOptionLocked(modelID, currentEffort)
	s.mu.Unlock()
	val, why := effortValue(opt, wantEffort)
	if why == "" {
		return val, nil
	}
	// Plan 054 § C4: strict refusal on unattended runs; lenient notice + fallback
	// on interactive/reload starts (matching startsettings.go).
	if s.opts.Unattended {
		if opt == nil {
			return "", newCodexErr(errkind.Config, "--effort %q: model %s offers no effort setting", sanitizeLine(wantEffort), sanitizeLine(modelID))
		}
		var allowed []string
		for _, v := range opt.SelectValues {
			allowed = append(allowed, v.Value)
		}
		return "", newCodexErr(errkind.Config, "--effort %q is not supported by %s (supported: %s)", sanitizeLine(wantEffort), sanitizeLine(modelID), strings.Join(allowed, ", "))
	}
	notes.effortUnmatched(wantEffort, why, opt)
	return currentEffort, nil
}

// rebuildCatalogAndConfigLocked rebuilds Snapshot.Models (with SwitchEffort),
// Snapshot.Config (effort option), and bumps CatalogRevision. s.mu is held.
func (s *codexSession) rebuildCatalogAndConfigLocked() {
	curModel := s.snap.CurrentModel
	// Normalize active effort against the active model BEFORE computing
	// SwitchEffort so a stale restored effort never leaks into picker previews.
	var cfg []ConfigOption
	if opt := s.buildEffortOptionLocked(curModel, s.currentEffort); opt != nil {
		cfg = append(cfg, *opt)
		s.currentEffort = opt.Current
	}
	curEffort := s.currentEffort

	ids := make([]string, 0, len(s.visibleOrder)+1)
	ids = append(ids, s.visibleOrder...)
	if curModel != "" && !slices.Contains(ids, curModel) {
		ids = append(ids, curModel)
	}

	infos := make([]ModelInfo, 0, len(ids))
	for _, id := range ids {
		m, ok := s.allModels[id]
		name := id
		if ok && m.DisplayName != "" {
			name = sanitizeText(m.DisplayName)
		}
		efforts, def := s.effortsForModelLocked(id)
		switchEff := def
		if curEffort != "" && slices.Contains(efforts, curEffort) {
			switchEff = curEffort
		}
		infos = append(infos, ModelInfo{
			ID:            id,
			Name:          name,
			Efforts:       efforts,
			DefaultEffort: def,
			SwitchEffort:  switchEff,
		})
	}
	bounded, _ := boundCatalog(infos, curModel)
	s.snap.Models = bounded
	s.catalogRev++
	s.snap.CatalogRevision = s.catalogRev
	s.snap.Config = cfg
}

func (s *codexSession) rebuildSlashLocked() *SlashCatalog {
	next := SlashCatalogFor(s.snap, s.skills).Items
	if s.snap.Slash != nil && slices.Equal(next, s.snap.Slash) {
		return nil
	}
	s.snap.Slash = next
	return &SlashCatalog{Items: cloneSlash(next)}
}

// installDeltaLocked builds the full install StateDelta. Config is ALWAYS
// non-nil so Engine.observeCatalog updates modelcache (Plan 054 § C1).
func (s *codexSession) installDeltaLocked() *StateDelta {
	title := s.snap.Title
	mode := s.snap.CurrentMode
	model := s.snap.CurrentModel
	d := &StateDelta{
		Title:    &title,
		Mode:     &mode,
		Model:    &model,
		Config:   &ConfigState{Options: cloneConfig(s.snap.Config)},
		Commands: &CommandsState{Commands: append([]CommandInfo(nil), s.snap.Commands...)},
		Plugins:  &PluginsState{Plugins: append([]PluginCommand(nil), s.snap.Plugins...)},
		Slash:    &SlashCatalog{Items: cloneSlash(s.snap.Slash)},
		Catalog: &CatalogState{
			Models:   CloneModels(s.snap.Models),
			Revision: s.snap.CatalogRevision,
		},
	}
	if s.snap.Usage != nil {
		u := *s.snap.Usage
		d.Usage = &u
	}
	return d
}

// --- Mode, Sandbox & Approval Policy Mapping (Plan 054 § C3) ---

func (s *codexSession) threadSandboxForMode(mode string) codexrpc.SandboxMode {
	switch s.provider().ModeKind(mode) {
	case ModeReadOnly, ModePlan:
		return codexrpc.SandboxReadOnly
	default:
		return codexrpc.SandboxWorkspaceWrite
	}
}

func (s *codexSession) turnSandboxForModeLocked(mode string) *codexrpc.SandboxPolicy {
	switch s.provider().ModeKind(mode) {
	case ModeReadOnly, ModePlan:
		return &codexrpc.SandboxPolicy{Type: codexrpc.SandboxPolicyReadOnly}
	default:
		if s.workspaceSandbox != nil {
			cp := *s.workspaceSandbox
			cp.WritableRoots = slices.Clone(s.workspaceSandbox.WritableRoots)
			return &cp
		}
		return &codexrpc.SandboxPolicy{Type: codexrpc.SandboxPolicyWorkspaceWrite}
	}
}

// approvalPolicyForMode enforces the hard read-only invariant (Plan 054 § C3):
// ask and plan ALWAYS send approvalPolicy: "never" (even under --no-force) so
// an approved retry can never escape the OS read-only sandbox.
func (s *codexSession) approvalPolicyForMode(mode string) codexrpc.ApprovalPolicy {
	switch s.provider().ModeKind(mode) {
	case ModeReadOnly, ModePlan:
		return codexrpc.ApprovalNever
	default:
		if EffectiveApproval(s.opts).Permission == ApprovalAllow {
			return codexrpc.ApprovalNever
		}
		return codexrpc.ApprovalOnRequest
	}
}

func (s *codexSession) collaborationModeLocked(mode, model, effort string) *codexrpc.CollaborationMode {
	cmMode := "default"
	if s.provider().ModeKind(mode) == ModePlan {
		cmMode = "plan"
	}
	var effPtr *string
	if effort != "" {
		e := effort
		effPtr = &e
	}
	return &codexrpc.CollaborationMode{
		Mode: cmMode,
		Settings: codexrpc.CollaborationSettings{
			Model:                 model,
			ReasoningEffort:       effPtr,
			DeveloperInstructions: nil,
		},
	}
}

// --- Prompt, Begin, Cancel, Interject (Plan 054 § D1) ---

func (s *codexSession) Prompt(ctx context.Context, text string) (Result, error) {
	return s.Begin(text)(ctx)
}

func (s *codexSession) Begin(text string) func(ctx context.Context) (Result, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return func(context.Context) (Result, error) { return Result{}, codexrpc.ErrClosed }
	}
	if s.transportDead {
		err := s.transportErrLocked()
		s.mu.Unlock()
		return func(context.Context) (Result, error) { return Result{}, err }
	}
	if !s.ready || s.client == nil {
		s.mu.Unlock()
		return func(context.Context) (Result, error) { return Result{}, fmt.Errorf("agent: session not started") }
	}
	if s.activeTurn != nil {
		s.mu.Unlock()
		return func(context.Context) (Result, error) { return Result{}, ErrPromptInFlight }
	}
	if s.foreignTurnID != "" {
		s.mu.Unlock()
		return func(context.Context) (Result, error) { return Result{}, ErrForeignTurn }
	}
	s.turnGen++
	turn := &codexTurnState{
		gen:     s.turnGen,
		phase:   codexPhaseClaimed,
		idReady: make(chan struct{}),
		done:    make(chan struct{}),
	}
	s.activeTurn = turn
	s.turnItemDeltas = make(map[string]bool)
	s.lastMsgItemID = ""
	s.usage.beginTurn()
	s.mu.Unlock()

	return func(ctx context.Context) (Result, error) {
		return s.runPrompt(ctx, turn, text)
	}
}

func codexImageExt(mime string) string {
	switch strings.ToLower(strings.TrimSpace(mime)) {
	case "image/png":
		return ".png"
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	default:
		return ".img"
	}
}

// stageCodexImageCopy writes already-validated attachment bytes into a
// host-owned 0700 temp directory with 0400 mode so Codex opens the exact
// validated image rather than re-opening the original path (M8 TOCTOU guard).
func (s *codexSession) stageCodexImageCopy(a Attachment) (string, error) {
	s.mu.Lock()
	dir := s.imgDir
	if dir == "" {
		d, err := os.MkdirTemp("", "craze-codex-img-*")
		if err != nil {
			s.mu.Unlock()
			return "", err
		}
		_ = os.Chmod(d, 0o700)
		s.imgDir = d
		dir = d
	}
	s.mu.Unlock()

	f, err := os.CreateTemp(dir, "img-*"+codexImageExt(a.MIME))
	if err != nil {
		return "", err
	}
	p := f.Name()
	if _, err := f.Write(a.Data); err != nil {
		_ = f.Close()
		_ = os.Remove(p)
		return "", err
	}
	if err := f.Chmod(0o400); err != nil {
		_ = f.Close()
		_ = os.Remove(p)
		return "", err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(p)
		return "", err
	}
	return p, nil
}

func removeTempFiles(paths []string) {
	for _, p := range paths {
		if p != "" {
			_ = os.Remove(p)
		}
	}
}

func (s *codexSession) runPrompt(ctx context.Context, turn *codexTurnState, text string) (Result, error) {
	// Parse Craze's image attachment envelope off-lock (Plan 054 § D1) and stage
	// immutable host-owned copies of validated bytes before handing paths to Codex.
	rest, ims, problems := promptImagesOf(text, true)
	var (
		imgInputs  []codexrpc.UserInput
		tempFiles  []string
		extraFails []string
	)
	for _, a := range ims.atts {
		if len(a.Data) > 0 {
			copyPath, err := s.stageCodexImageCopy(a)
			if err != nil {
				problems = append(problems, fmt.Sprintf("image #%d stage failed: %v", a.N, err))
				extraFails = append(extraFails, attachmentPathText(a.N, a.Path, "could not stage image copy"))
				continue
			}
			tempFiles = append(tempFiles, copyPath)
			imgInputs = append(imgInputs, codexrpc.UserInput{Type: "localImage", Path: copyPath})
		} else if a.Path != "" {
			imgInputs = append(imgInputs, codexrpc.UserInput{Type: "localImage", Path: a.Path})
		}
	}
	ims.dropData()
	noteAttachments(s.log, "prompt", problems)

	fallbacks := append(append([]string(nil), ims.fallbacks...), extraFails...)
	promptText := PlacePathText(rest, fallbacks)
	var inputs []codexrpc.UserInput
	if sk, after, isSkill := s.parseCodexSkillPrompt(promptText); isSkill {
		inputs = append(inputs, codexrpc.UserInput{Type: "skill", Name: sk.Name, Path: sk.Path})
		if after != "" {
			inputs = append(inputs, codexrpc.UserInput{Type: "text", Text: after})
		}
	} else if promptText != "" {
		inputs = append(inputs, codexrpc.UserInput{Type: "text", Text: promptText})
	}
	inputs = append(inputs, imgInputs...)
	if len(inputs) == 0 {
		inputs = append(inputs, codexrpc.UserInput{Type: "text", Text: text})
	}

	token := s.asks.BeginTurn()

	s.mu.Lock()
	if turn.settling {
		done := turn.done
		s.mu.Unlock()
		removeTempFiles(tempFiles)
		s.asks.EndTurn(token)
		<-done
		return s.turnResult(turn)
	}
	if turn.phase == codexPhaseSettled {
		err := turn.failedErr
		if err == nil && turn.cancelPending {
			err = ErrPromptCancelled
		}
		s.mu.Unlock()
		removeTempFiles(tempFiles)
		s.asks.EndTurn(token)
		if err != nil {
			return Result{}, err
		}
		return s.turnResult(turn)
	}
	if s.closed {
		s.clearTurnLocked(turn)
		s.mu.Unlock()
		removeTempFiles(tempFiles)
		s.asks.EndTurn(token)
		return Result{}, codexrpc.ErrClosed
	}
	if s.transportDead {
		err := s.transportErrLocked()
		s.clearTurnLocked(turn)
		s.mu.Unlock()
		removeTempFiles(tempFiles)
		s.asks.EndTurn(token)
		return Result{}, err
	}
	if turn.cancelPending {
		s.clearTurnLocked(turn)
		s.mu.Unlock()
		removeTempFiles(tempFiles)
		s.asks.EndTurn(token)
		return Result{}, ErrPromptCancelled
	}
	if err := ctx.Err(); err != nil {
		s.clearTurnLocked(turn)
		s.mu.Unlock()
		removeTempFiles(tempFiles)
		s.asks.EndTurn(token)
		return Result{}, err
	}
	// Recheck foreign turn ownership at the claimed -> sent boundary (M6).
	if s.foreignTurnID != "" {
		s.clearTurnLocked(turn)
		s.mu.Unlock()
		removeTempFiles(tempFiles)
		s.asks.EndTurn(token)
		return Result{}, ErrForeignTurn
	}
	// Transition to sent right before writing turn/start, capturing the turn's
	// immutable mode, model, and approvalPolicy so mid-turn SetMode/SetModel
	// changes cannot affect the running turn (H1, M10).
	mode := s.snap.CurrentMode
	model := s.snap.CurrentModel
	effort := s.currentEffort
	sbPolicy := s.turnSandboxForModeLocked(mode)
	apPolicy := s.approvalPolicyForMode(mode)
	collabMode := s.collaborationModeLocked(mode, model, effort)
	turn.token = token
	turn.mode = mode
	turn.model = model
	turn.approvalPolicy = apPolicy
	turn.tempFiles = tempFiles
	turn.phase = codexPhaseSent
	client := s.client
	threadID := s.threadID
	s.mu.Unlock()

	params := codexrpc.TurnStartParams{
		ThreadID:          threadID,
		Input:             inputs,
		SandboxPolicy:     sbPolicy,
		ApprovalPolicy:    apPolicy,
		CollaborationMode: collabMode,
		Model:             model,
		Effort:            effort,
		Summary:           "auto",
	}

	// Execute turn/start under the session lifetime context (NOT prompt caller
	// ctx) so caller cancellation while turn/start is in flight does not abandon
	// an already-written request before its turnID is known and interrupted (H2).
	type startReply struct {
		res *codexrpc.TurnStartResult
		err error
	}
	startCh := make(chan startReply, 1)
	go func() {
		r, e := client.TurnStart(s.lifeCtx, params)
		startCh <- startReply{res: r, err: e}
	}()

	var (
		reply     startReply
		gotReply  bool
		callerCtx = ctx.Done()
	)
	for !gotReply {
		select {
		case reply = <-startCh:
			gotReply = true
		case <-turn.done:
			// Already settled (e.g. turn/completed arrived before reply or transport died).
			return s.turnResult(turn)
		case <-callerCtx:
			callerCtx = nil
			// Latch cancel and start bounded grace wait; do NOT drop pending start.
			s.latchCancelAndUnblockAsks(turn)
			go s.enforceCancelGrace(turn, client, threadID, ctx.Err())
		case <-s.closingCh:
			return s.settleTurn(turn, "", codexrpc.ErrClosed, true)
		}
	}

	if reply.err != nil {
		// If the transport died, wait for ordered notifications already buffered
		// on stdout to drain first so an already-read turn/completed wins (H4).
		if errors.Is(reply.err, codexrpc.ErrClosed) || errors.Is(reply.err, codexrpc.ErrAgentExited) {
			select {
			case <-client.NotifyDone():
			case <-s.closingCh:
			}
			s.mu.Lock()
			settled := turn.settling || turn.phase == codexPhaseSettled
			s.mu.Unlock()
			if settled {
				<-turn.done
				return s.turnResult(turn)
			}
		}
		settledErr := s.classifyRPCStartError(reply.err)
		return s.settleTurn(turn, "", settledErr, true)
	}

	// Bind turnID from reply, honoring any early turn/completed or latched Cancel.
	var needInterruptID string
	var earlyTurn *codexrpc.Turn
	s.mu.Lock()
	if !turn.settling && turn.phase != codexPhaseSettled {
		if reply.res != nil && reply.res.Turn.ID != "" {
			turn.bindIDLocked(reply.res.Turn.ID)
			turn.phase = codexPhaseIdentified
			if ec, ok := s.earlyCompleted[turn.turnID]; ok {
				delete(s.earlyCompleted, turn.turnID)
				cp := ec
				earlyTurn = &cp
			} else if turn.cancelPending && !turn.interruptSent && !turn.interruptInFlight {
				turn.interruptSent = true
				needInterruptID = turn.turnID
			}
		}
	}
	s.mu.Unlock()

	if earlyTurn != nil {
		s.finishTurnFromWire(turn, *earlyTurn)
	} else if needInterruptID != "" {
		go func() {
			iCtx, cancel := context.WithTimeout(s.lifeCtx, codexCancelFallbackGrace)
			defer cancel()
			_ = client.TurnInterrupt(iCtx, threadID, needInterruptID)
		}()
	}

	// Wait for unified settlement, caller context cancellation, or session close.
	for {
		select {
		case <-turn.done:
			return s.turnResult(turn)
		case <-callerCtx:
			callerCtx = nil
			s.latchCancelAndUnblockAsks(turn)
			go s.enforceCancelGrace(turn, client, threadID, ctx.Err())
		case <-s.closingCh:
			return s.settleTurn(turn, "", codexrpc.ErrClosed, true)
		}
	}
}

func (s *codexSession) latchCancelAndUnblockAsks(turn *codexTurnState) {
	s.mu.Lock()
	tok := turn.token
	turn.cancelPending = true
	tid := turn.turnID
	client := s.client
	threadID := s.threadID
	sendNow := tid != "" && !turn.interruptSent && !turn.interruptInFlight && !turn.settling && turn.phase != codexPhaseSettled
	if sendNow {
		turn.interruptSent = true
	}
	s.mu.Unlock()
	if !tok.NoTurn() {
		s.asks.CancelTurn(tok)
	}
	if sendNow && client != nil {
		go func() {
			iCtx, cancel := context.WithTimeout(s.lifeCtx, codexCancelFallbackGrace)
			defer cancel()
			_ = client.TurnInterrupt(iCtx, threadID, tid)
		}()
	}
}

// enforceCancelGrace bounds how long a caller-cancelled prompt waits for Codex
// to settle the interrupted turn. If Codex fails to settle within the grace
// window, it aborts the hung transport rather than releasing the slot while
// Codex still owns a live turn (H2).
func (s *codexSession) enforceCancelGrace(turn *codexTurnState, client *codexrpc.Client, _ string, cause error) {
	t := time.NewTimer(codexCancelFallbackGrace)
	defer t.Stop()
	select {
	case <-turn.done:
		return
	case <-s.closingCh:
		return
	case <-t.C:
		if client != nil {
			client.CloseAsync()
		}
		err := cause
		if err == nil {
			err = newCodexErr(errkind.Agent, "codex cancel timed out waiting for turn settlement")
		}
		_, _ = s.settleTurn(turn, "", err, true)
	}
}

func (s *codexSession) clearTurnLocked(turn *codexTurnState) {
	// If settleTurn has already claimed this turn, it owns closing turn.done
	// and releasing s.activeTurn; never double-close or release early.
	if turn.settling {
		return
	}
	if turn.phase != codexPhaseSettled {
		turn.phase = codexPhaseSettled
		close(turn.done)
	}
	if s.activeTurn == turn {
		s.activeTurn = nil
	}
}

func (s *codexSession) recordSettledTurnLocked(turnID string) {
	if turnID == "" {
		return
	}
	delete(s.earlyCompleted, turnID)
	if s.settledTurns == nil {
		s.settledTurns = make(map[string]bool)
	}
	if s.settledTurns[turnID] {
		return
	}
	s.settledTurns[turnID] = true
	s.settledTurnOrder = append(s.settledTurnOrder, turnID)
	const maxSettled = 256
	if len(s.settledTurnOrder) > maxSettled {
		oldest := s.settledTurnOrder[0]
		s.settledTurnOrder = s.settledTurnOrder[1:]
		delete(s.settledTurns, oldest)
	}
}

func (s *codexSession) turnResult(turn *codexTurnState) (Result, error) {
	s.mu.Lock()
	err := turn.failedErr
	sr := turn.stopReason
	s.mu.Unlock()
	if err != nil {
		return Result{}, err
	}
	return Result{StopReason: sr}, nil
}

// settleTurn is the single unified settlement path for completed, interrupted,
// failed, and transport-crash endings (Plan 054 § D1, H3).
//
// Invariant: the active turn slot stays occupied and turn.done stays OPEN until
// every retired tool row, flushed outbox delta (usage/settings), ask ending,
// and terminal EventDone/EventError has been committed to EventLog.
func (s *codexSession) settleTurn(turn *codexTurnState, stopReason string, termErr error, releaseSlot bool) (Result, error) {
	var (
		toolEvs     []Event
		subagentEvs []Event
		tempFiles   []string
		tok         TurnToken
		won         bool
		closing     bool
	)
	toolFinal := toolCompleted
	subFinal := SubagentCompleted
	if termErr != nil {
		toolFinal = toolFailed
		subFinal = SubagentFailed
	} else if stopReason == acp.StopCancelled {
		toolFinal = "cancelled"
		subFinal = SubagentCancelled
	}

	now := s.Now()
	s.mu.Lock()
	if turn.settling || turn.phase == codexPhaseSettled {
		done := turn.done
		s.mu.Unlock()
		<-done
		return s.turnResult(turn)
	}
	won = true
	turn.settling = true
	turn.stopReason = stopReason
	turn.failedErr = termErr
	tok = turn.token
	tempFiles = turn.tempFiles
	turn.tempFiles = nil
	closing = s.closed
	// Release all live command streaming buffers so interrupted commands never
	// retain memory across turns (M14).
	clear(s.cmdBuffers)
	// Retire any open in-flight tool rows before terminal event publication.
	for _, id := range s.toolOrder {
		row, ok := s.tools[id]
		if !ok || !toolStatusInFlight(row.Status) {
			continue
		}
		row.Status = toolFinal
		row.At = now
		s.tools[id] = row
		ct := cloneTool(row)
		toolEvs = append(toolEvs, Event{Type: EventTool, Tool: &ct})
	}
	// Retire only still-running subagent rows launched by this turn (M7).
	for _, id := range s.subagentOrder {
		rec, ok := s.subagents[id]
		if !ok || rec.info.Status != SubagentRunning {
			continue
		}
		if rec.turn != 0 && rec.turn != int(turn.gen) {
			continue
		}
		rec.info.Status = subFinal
		if rec.info.EndedAt.IsZero() {
			rec.info.EndedAt = now
		}
		if !rec.info.StartedAt.IsZero() {
			rec.info.DurationMs = int(rec.info.EndedAt.Sub(rec.info.StartedAt).Milliseconds())
		}
		info := cloneSubagent(rec.info)
		subagentEvs = append(subagentEvs, Event{Type: EventSubagent, Subagent: &info, SubagentChange: SubagentChangeFinished})
	}
	var steerWait <-chan struct{}
	if turn.steerInFlight > 0 {
		if turn.steerZeroCh == nil {
			turn.steerZeroCh = make(chan struct{})
		}
		steerWait = turn.steerZeroCh
	}
	s.mu.Unlock()

	if won {
		// Wait boundedly for any reserved in-flight Interject to publish its
		// confirmed EventUser ahead of EventDone/EventError; closingCh breaks
		// immediately on Close() so shutdown can never deadlock (H3).
		if steerWait != nil {
			t := time.NewTimer(500 * time.Millisecond)
			select {
			case <-steerWait:
			case <-s.closingCh:
			case <-t.C:
			}
			t.Stop()
		}
		removeTempFiles(tempFiles)
		pubCtx := context.Background()
		var pubCancel context.CancelFunc
		var stopCloseWatch chan struct{}
		if !s.opts.NoPrimary {
			// Bound publication on Close (whether Close began before or during
			// settlement) when a primary channel exists and might be unread, so
			// teardown waiting on turn.done never deadlocks behind a full primary
			// channel while detached hosts (NoPrimary) still deliver cleanly.
			if closing {
				pubCtx, pubCancel = context.WithTimeout(context.Background(), 250*time.Millisecond)
			} else {
				pubCtx, pubCancel = context.WithCancel(context.Background())
				stopCloseWatch = make(chan struct{})
				go func(cancel context.CancelFunc, stop <-chan struct{}) {
					select {
					case <-s.closingCh:
						t := time.NewTimer(250 * time.Millisecond)
						select {
						case <-t.C:
							cancel()
						case <-stop:
							t.Stop()
						}
					case <-stop:
					}
				}(pubCancel, stopCloseWatch)
			}
		}
		for _, ev := range toolEvs {
			s.emitCtx(pubCtx, ev)
		}
		for _, ev := range subagentEvs {
			s.emitCtx(pubCtx, ev)
		}
		// Flush queued outbox deltas (usage, settings) BEFORE terminal event so
		// direct Publish never overtakes them (H3).
		_ = s.log.Flush(pubCtx, s.done)
		if !tok.NoTurn() {
			s.asks.EndTurn(tok)
		}
		if termErr != nil {
			s.emitCtx(pubCtx, Event{Type: EventError, Err: termErr})
		} else {
			s.emitCtx(pubCtx, Event{Type: EventDone, StopReason: stopReason})
		}
		if stopCloseWatch != nil {
			close(stopCloseWatch)
		}
		if pubCancel != nil {
			pubCancel()
		}

		// Commit slot release and signal waiters ONLY AFTER terminal records are
		// published to EventLog (H3).
		s.mu.Lock()
		turn.phase = codexPhaseSettled
		if turn.turnID != "" {
			s.recordSettledTurnLocked(turn.turnID)
		}
		if releaseSlot && s.activeTurn == turn {
			s.activeTurn = nil
		}
		close(turn.done)
		s.mu.Unlock()
	}
	return s.turnResult(turn)
}

func (s *codexSession) finishTurnFromWire(turn *codexTurnState, wireTurn codexrpc.Turn) {
	switch wireTurn.Status {
	case "completed":
		_, _ = s.settleTurn(turn, acp.StopEndTurn, nil, true)
	case "interrupted":
		_, _ = s.settleTurn(turn, acp.StopCancelled, nil, true)
	case "failed":
		err := s.turnErrorFromWire(wireTurn.Error)
		_, _ = s.settleTurn(turn, "", err, true)
	default:
		_, _ = s.settleTurn(turn, acp.StopEndTurn, nil, true)
	}
}

func (s *codexSession) turnErrorFromWire(te *codexrpc.TurnError) error {
	if te == nil {
		return newCodexErr(errkind.Agent, "codex turn failed")
	}
	info := codexrpc.ClassifyTurnError(te)
	msg := sanitizeLine(te.Message)
	if msg == "" {
		msg = "codex turn failed"
	}
	if info.Kind == errkind.Auth {
		return &codexError{
			msg:  fmt.Sprintf("agent: %s (run `%s`)", msg, codexLoginHint),
			info: info,
		}
	}
	return &codexError{
		msg:  "agent: " + msg,
		info: info,
	}
}

// Cancel stops whatever prompt of craze's own is running or claimed, following
// Plan 054 § D1 and H2:
//  1. Unblock parked asks via AskRegistry.CancelTurn immediately.
//  2. If claimed (pre-wire): withdraw immediately (Withdrew: true, Settled: true).
//  3. If sent or identified: latch cancelPending, send turn/interrupt once turnID
//     is known under a single deadline covering BOTH the interrupt RPC and the
//     settlement wait. Never report fake StopCancelled success if Codex fails to
//     settle within the grace window; instead abort the hung transport and report
//     an error.
func (s *codexSession) Cancel(ctx context.Context) (CancelOutcome, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.Lock()
	turn := s.activeTurn
	client := s.client
	threadID := s.threadID
	if turn == nil {
		s.mu.Unlock()
		s.asks.CancelTurn(TurnToken{})
		return CancelOutcome{Settled: true}, nil
	}
	tok := turn.token
	if turn.phase == codexPhaseClaimed && !turn.settling {
		turn.cancelPending = true
		turn.failedErr = ErrPromptCancelled
		turn.phase = codexPhaseSettled
		close(turn.done)
		s.activeTurn = nil
		s.mu.Unlock()
		if !tok.NoTurn() {
			s.asks.CancelTurn(tok)
		}
		return CancelOutcome{Withdrew: true, Settled: true}, nil
	}
	turn.cancelPending = true
	done := turn.done
	s.mu.Unlock()

	if !tok.NoTurn() {
		s.asks.CancelTurn(tok)
	} else {
		s.asks.CancelTurn(TurnToken{})
	}
	if err := ctx.Err(); err != nil {
		return CancelOutcome{Wrote: false, Settled: false}, err
	}

	// Single bounded deadline covering BOTH TurnInterrupt RPC and turn settlement.
	graceCtx, graceCancel := context.WithTimeout(ctx, codexCancelFallbackGrace)
	defer graceCancel()

	// If an earlier Cancel left a TurnInterrupt RPC in flight, wait for it to
	// complete (bounded by graceCtx/done/closing) and retry if it failed.
	wrote := false
	for {
		s.mu.Lock()
		tid := turn.turnID
		alreadySettled := turn.settling || turn.phase == codexPhaseSettled
		alreadySent := turn.interruptSent
		inFlight := turn.interruptInFlight
		waitCh := turn.interruptDone
		sendNow := false
		if tid != "" && !alreadySettled && !alreadySent && !inFlight && client != nil {
			turn.interruptInFlight = true
			turn.interruptDone = make(chan struct{})
			sendNow = true
		}
		s.mu.Unlock()

		if alreadySettled || alreadySent || tid == "" || client == nil {
			break
		}
		if inFlight && waitCh != nil {
			select {
			case <-waitCh:
				continue
			case <-done:
				return CancelOutcome{Wrote: true, Settled: true}, nil
			case <-graceCtx.Done():
				if ctx.Err() != nil {
					return CancelOutcome{Wrote: false, Settled: false}, ctx.Err()
				}
				client.CloseAsync()
				err := newCodexErr(errkind.Agent, "codex cancel timed out waiting for turn/interrupt")
				_, _ = s.settleTurn(turn, "", err, true)
				return CancelOutcome{Wrote: false, Settled: false}, err
			case <-s.closingCh:
				<-done
				return CancelOutcome{Wrote: wrote, Settled: true}, nil
			}
		}
		if !sendNow {
			break
		}

		type intRes struct{ err error }
		ch := make(chan intRes, 1)
		go func() {
			err := client.TurnInterrupt(graceCtx, threadID, tid)
			s.mu.Lock()
			turn.interruptInFlight = false
			if err == nil {
				turn.interruptSent = true
			}
			if turn.interruptDone != nil {
				close(turn.interruptDone)
				turn.interruptDone = nil
			}
			s.mu.Unlock()
			ch <- intRes{err: err}
		}()
		select {
		case r := <-ch:
			if r.err == nil {
				wrote = true
			} else if ctx.Err() != nil {
				return CancelOutcome{Wrote: false, Settled: false}, ctx.Err()
			}
		case <-done:
			return CancelOutcome{Wrote: true, Settled: true}, nil
		case <-graceCtx.Done():
			if ctx.Err() != nil {
				return CancelOutcome{Wrote: false, Settled: false}, ctx.Err()
			}
			// Grace expired while turn/interrupt hung: abort hung transport and
			// report failure rather than pretending cancellation succeeded.
			client.CloseAsync()
			err := newCodexErr(errkind.Agent, "codex cancel timed out waiting for turn/interrupt")
			_, _ = s.settleTurn(turn, "", err, true)
			return CancelOutcome{Wrote: false, Settled: false}, err
		case <-s.closingCh:
			<-done
			return CancelOutcome{Wrote: wrote, Settled: true}, nil
		}
		break
	}

	select {
	case <-done:
		return CancelOutcome{Wrote: wrote, Settled: true}, nil
	case <-graceCtx.Done():
		if ctx.Err() != nil {
			return CancelOutcome{Wrote: wrote, Settled: false}, ctx.Err()
		}
		// Grace expired without confirmed turn/completed: abort the hung
		// transport so Codex cannot keep running a turn on this connection,
		// and return a real failure error (H2).
		if client != nil {
			client.CloseAsync()
		}
		err := newCodexErr(errkind.Agent, "codex cancel timed out waiting for turn settlement")
		_, _ = s.settleTurn(turn, "", err, true)
		return CancelOutcome{Wrote: wrote, Settled: false}, err
	case <-s.closingCh:
		<-done
		return CancelOutcome{Wrote: wrote, Settled: true}, nil
	}
}

// Interject merges text into an active turn via turn/steer (full steering wire
// and user-echo dedup live in M2b; when idle or not yet identified returns
// ErrPromptRequired so Engine.Interject queues cleanly).
func (s *codexSession) Interject(ctx context.Context, text string) error {
	clean := interjectText(s.log, text)
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return codexrpc.ErrClosed
	}
	turn := s.activeTurn
	client := s.client
	threadID := s.threadID
	if turn == nil || turn.settling || turn.phase != codexPhaseIdentified || turn.turnID == "" || client == nil {
		s.mu.Unlock()
		return ErrPromptRequired
	}
	expectedTurnID := turn.turnID
	// Reserve the steer BEFORE issuing turn/steer so a racing turn/completed
	// waits for our confirmed EventUser publication instead of dropping it (H3).
	turn.steerInFlight++
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		turn.steerInFlight--
		if turn.steerInFlight == 0 && turn.steerZeroCh != nil {
			close(turn.steerZeroCh)
			turn.steerZeroCh = nil
		}
		s.mu.Unlock()
	}()

	_, err := client.TurnSteer(ctx, codexrpc.TurnSteerParams{
		ThreadID:       threadID,
		ExpectedTurnID: expectedTurnID,
		Input:          []codexrpc.UserInput{{Type: "text", Text: clean}},
	})
	if err != nil {
		if codexrpc.IsNotSteerable(err) {
			return ErrPromptRequired
		}
		return err
	}
	// Publish confirmed EventUser through s.lifeCtx (cancelled immediately on
	// Close so a full primary channel during shutdown never deadlocks — H3).
	s.emitCtx(s.lifeCtx, Event{Type: EventUser, Text: sanitizeText(clean), Interjection: true})
	return nil
}

// --- Settings Verbs (next-turn settings applied unconditionally on turn/start) ---

func (s *codexSession) SetModel(_ context.Context, cause, modelID string) (SetOutcome, error) {
	want := strings.TrimSpace(modelID)
	if want == "" {
		return SetOutcome{}, fmt.Errorf("agent: empty model")
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return SetOutcome{}, codexrpc.ErrClosed
	}
	if !s.ready {
		s.mu.Unlock()
		return SetOutcome{}, fmt.Errorf("agent: session not started")
	}
	if !s.log.OutboxRoom() {
		s.mu.Unlock()
		return SetOutcome{}, ErrSetUnavailable
	}
	resolved, ok := s.resolveModelLocked(want)
	if len(s.allModels) > 0 && !ok {
		s.mu.Unlock()
		return SetOutcome{}, newCodexErr(errkind.ModelNotFound, "unknown model %q", sanitizeLine(want))
	}
	s.snap.CurrentModel = resolved
	efforts, def := s.effortsForModelLocked(resolved)
	if s.currentEffort == "" || (len(efforts) > 0 && !slices.Contains(efforts, s.currentEffort)) {
		s.currentEffort = def
	}
	s.rebuildCatalogAndConfigLocked()
	model := s.snap.CurrentModel
	ticket := s.enqueueDeltaLocked(cause, Event{}, &StateDelta{
		Model:  &model,
		Config: &ConfigState{Options: cloneConfig(s.snap.Config)},
		Catalog: &CatalogState{
			Models:   CloneModels(s.snap.Models),
			Revision: s.snap.CatalogRevision,
		},
	})
	midTurn := s.activeTurn != nil
	s.mu.Unlock()
	if midTurn {
		s.emit(Event{
			Type:   EventNotice,
			Notice: &NoticeInfo{Severity: "info", Title: "Model updated for next turn", Description: model},
		})
	}
	return SetOutcome{Value: model, Ticket: ticket}, nil
}

func (s *codexSession) SetMode(_ context.Context, cause, modeID string) (SetOutcome, error) {
	id, ok := ResolveMode(strings.TrimSpace(modeID), codexStartModes)
	if !ok {
		return SetOutcome{}, newCodexErr(errkind.Config, "mode %s is not advertised", sanitizeLine(modeID))
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return SetOutcome{}, codexrpc.ErrClosed
	}
	if !s.ready {
		s.mu.Unlock()
		return SetOutcome{}, fmt.Errorf("agent: session not started")
	}
	if !s.log.OutboxRoom() {
		s.mu.Unlock()
		return SetOutcome{}, ErrSetUnavailable
	}
	s.snap.CurrentMode = id
	mode := id
	ticket := s.enqueueDeltaLocked(cause, Event{}, &StateDelta{Mode: &mode})
	midTurn := s.activeTurn != nil
	s.mu.Unlock()
	if midTurn {
		s.emit(Event{
			Type:   EventNotice,
			Notice: &NoticeInfo{Severity: "info", Title: "Mode updated for next turn", Description: mode},
		})
	}
	return SetOutcome{Value: mode, Ticket: ticket}, nil
}

func (s *codexSession) SetConfig(_ context.Context, cause, id, value, forModel string) (SetOutcome, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return SetOutcome{}, codexrpc.ErrClosed
	}
	if !s.ready {
		s.mu.Unlock()
		return SetOutcome{}, fmt.Errorf("agent: session not started")
	}
	if forModel != "" && s.snap.CurrentModel != forModel {
		s.mu.Unlock()
		return SetOutcome{}, ErrStaleModel
	}
	if id != codexEffortID {
		s.mu.Unlock()
		return SetOutcome{}, fmt.Errorf("agent: unknown config option %q", sanitizeLine(id))
	}
	opt := s.buildEffortOptionLocked(s.snap.CurrentModel, s.currentEffort)
	if opt == nil {
		s.mu.Unlock()
		return SetOutcome{}, fmt.Errorf("agent: model %q offers no effort setting", sanitizeLine(s.snap.CurrentModel))
	}
	matched, why := effortValue(opt, strings.TrimSpace(value))
	if why != "" {
		s.mu.Unlock()
		return SetOutcome{}, fmt.Errorf("agent: invalid effort %q for model %q", sanitizeLine(value), sanitizeLine(s.snap.CurrentModel))
	}
	if !s.log.OutboxRoom() {
		s.mu.Unlock()
		return SetOutcome{}, ErrSetUnavailable
	}
	s.currentEffort = matched
	s.rebuildCatalogAndConfigLocked()
	ticket := s.enqueueDeltaLocked(cause, Event{}, &StateDelta{
		Config: &ConfigState{Options: cloneConfig(s.snap.Config)},
		Catalog: &CatalogState{
			Models:   CloneModels(s.snap.Models),
			Revision: s.snap.CatalogRevision,
		},
	})
	midTurn := s.activeTurn != nil
	s.mu.Unlock()
	if midTurn {
		s.emit(Event{
			Type:   EventNotice,
			Notice: &NoticeInfo{Severity: "info", Title: "Effort updated for next turn", Description: matched},
		})
	}
	return SetOutcome{Value: matched, Ticket: ticket}, nil
}

func (s *codexSession) SetTitle(cause, title string) error {
	clean := sanitizeText(strings.TrimSpace(title))
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return codexrpc.ErrClosed
	}
	if !s.log.OutboxRoom() {
		s.mu.Unlock()
		return ErrSetUnavailable
	}
	s.snap.Title = clean
	s.titlePinned = true
	t := clean
	s.enqueueDeltaLocked(cause, Event{}, &StateDelta{Title: &t})
	shouldSync := s.ready && s.client != nil && s.threadID != "" && clean != ""
	if shouldSync {
		s.pendingTitle = clean
		if !s.titleWorker {
			s.titleWorker = true
			go s.runTitleWorker()
		}
	}
	s.mu.Unlock()
	if shouldSync {
		select {
		case s.titleCh <- struct{}{}:
		default:
		}
	}
	return nil
}

// runTitleWorker serializes best-effort thread/name/set RPCs so rapid renames
// coalesce and always persist the latest title in strict order (L3).
func (s *codexSession) runTitleWorker() {
	for {
		select {
		case <-s.closingCh:
			return
		case <-s.titleCh:
		}
		for {
			s.mu.Lock()
			if s.closed || s.client == nil || s.threadID == "" {
				s.pendingTitle = ""
				s.mu.Unlock()
				return
			}
			next := s.pendingTitle
			s.pendingTitle = ""
			client := s.client
			tid := s.threadID
			s.mu.Unlock()
			if next == "" {
				break
			}
			ctx, cancel := context.WithTimeout(s.lifeCtx, 5*time.Second)
			_ = client.ThreadNameSet(ctx, tid, next)
			cancel()
		}
	}
}

// --- Snapshot & Close / Teardown ---

func (s *codexSession) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap := s.snap
	snap.Models = CloneModels(s.snap.Models)
	snap.Modes = append([]ModeInfo(nil), s.snap.Modes...)
	snap.Commands = append([]CommandInfo(nil), s.snap.Commands...)
	snap.Plugins = append([]PluginCommand(nil), s.snap.Plugins...)
	snap.Slash = cloneSlash(s.snap.Slash)
	snap.Config = cloneConfig(s.snap.Config)
	snap.Todos = append([]Todo(nil), s.snap.Todos...)
	snap.Tools = snapshotTools(s.toolOrder, s.tools)
	snap.Subagents = snapshotSubagents(s.subagentOrder, s.subagents)
	snap.ForeignTurn = s.foreignTurnID != ""
	if s.snap.Usage != nil {
		u := *s.snap.Usage
		snap.Usage = &u
	}
	return snap
}

func (s *codexSession) Close() error {
	s.teardown()
	s.logOnce.Do(func() { s.log.Close(context.Background()) })
	return s.closeErr
}

func (s *codexSession) teardown() {
	s.closeOnce.Do(func() {
		defer close(s.closeDone)
		s.mu.Lock()
		s.closed = true
		s.cancelAllPendingReqsLocked(codexrpc.ErrClosed)
		close(s.closingCh)
		s.mu.Unlock()

		// Cancel startup/session lifetime context and wait for any concurrent
		// Start() to finish spawning and cleaning up its child before closing (H5).
		s.lifeCancel()
		s.startWG.Wait()

		s.mu.Lock()
		client := s.client
		turn := s.activeTurn
		imgDir := s.imgDir
		s.imgDir = ""
		s.mu.Unlock()

		s.asks.Close()
		// Settle any active turn BEFORE closing s.done so EventLog.Publish
		// commits final tool rows and EventError cleanly instead of dropping them (H3).
		if turn != nil {
			_, _ = s.settleTurn(turn, "", codexrpc.ErrClosed, true)
		}
		close(s.done)

		if client != nil {
			s.closeErr = client.Close()
		}
		if imgDir != "" {
			_ = os.RemoveAll(imgDir)
		}
		if s.tee != nil {
			s.tee.Flush()
		}
	})
	<-s.closeDone
}

// --- Incoming Notifications, Server Requests & Transport Watch ---

func (s *codexSession) transportErrLocked() error {
	if s.client != nil {
		if err := s.client.Err(); err != nil {
			return err
		}
	}
	return codexrpc.ErrClosed
}

func (s *codexSession) watchTransportDone(client *codexrpc.Client) {
	<-client.Done()
	// Stop admitting new turns immediately on unexpected transport termination,
	// then wait for already-read stdout notifications (including final text,
	// edits, and turn/completed) to drain before settling an active turn (H4).
	s.mu.Lock()
	s.transportDead = true
	s.mu.Unlock()

	select {
	case <-client.NotifyDone():
	case <-s.closingCh:
	}

	err := client.Err()
	if err == nil {
		err = codexrpc.ErrClosed
	}
	s.mu.Lock()
	turn := s.activeTurn
	foreign := s.foreignTurnID
	s.foreignTurnID = ""
	closed := s.closed
	s.mu.Unlock()

	if turn != nil {
		_, _ = s.settleTurn(turn, "", err, true)
	}
	if foreign != "" && !closed {
		info := ErrorInfo(err)
		s.emit(Event{
			Type: EventForeignTurn,
			ForeignTurn: &ForeignTurnInfo{
				ID:      foreign,
				Running: false,
				Err:     &TurnError{Kind: info.Kind, Message: err.Error()},
			},
		})
	}
}

func (s *codexSession) onNotify(msg *codexrpc.Message) {
	switch msg.Method {
	case codexrpc.NotifyTurnStarted:
		s.handleTurnStarted(msg.Params)
	case codexrpc.NotifyTurnCompleted:
		s.handleTurnCompleted(msg.Params)
	case codexrpc.NotifyItemStarted:
		s.handleItemStarted(msg.Params)
	case codexrpc.NotifyItemCompleted:
		s.handleItemCompleted(msg.Params)
	case codexrpc.NotifyAgentMessageDelta:
		s.handleAgentMessageDelta(msg.Params)
	case codexrpc.NotifyPlanDelta:
		s.handlePlanDelta(msg.Params)
	case codexrpc.NotifyReasoningSummaryPart:
		s.handleReasoningSummaryPartAdded(msg.Params)
	case codexrpc.NotifyReasoningSummaryDelta, codexrpc.NotifyReasoningTextDelta:
		s.handleReasoningDelta(msg.Params)
	case codexrpc.NotifyCommandOutputDelta:
		s.handleCommandOutputDelta(msg.Params)
	case codexrpc.NotifyTurnPlanUpdated:
		s.handlePlanUpdated(msg.Params)
	case codexrpc.NotifyThreadTokenUsageUpdated:
		s.handleTokenUsageUpdated(msg.Params)
	case codexrpc.NotifyRateLimitsUpdated:
		s.handleRateLimitsUpdated(msg.Params)
	case codexrpc.NotifyThreadNameUpdated:
		s.handleThreadNameUpdated(msg.Params)
	case codexrpc.NotifySubAgentActivity:
		s.handleSubAgentActivity(msg.Params)
	case codexrpc.NotifyServerRequestResolved:
		s.handleServerRequestResolved(msg.Params)
	case codexrpc.NotifySkillsChanged:
		s.handleSkillsChanged()
	case codexrpc.NotifyError:
		s.handleErrorNotification(msg.Params)
	case codexrpc.NotifyConfigWarning, codexrpc.NotifyDeprecationNotice:
		s.log.Note(journal.DiagNote{
			Kind:   strings.ReplaceAll(msg.Method, "/", "_"),
			Fields: map[string]any{"params": sanitizeLine(string(msg.Params))},
		})
	}
}

func (s *codexSession) handleTurnStarted(raw json.RawMessage) {
	var p codexrpc.TurnEventParams
	if json.Unmarshal(raw, &p) != nil || p.Turn.ID == "" {
		return
	}
	if !s.isMainThread(p.ThreadID) {
		return
	}
	var (
		needInterruptID string
		client          *codexrpc.Client
		threadID        string
		isForeign       bool
	)
	s.mu.Lock()
	if s.settledTurns[p.Turn.ID] {
		s.mu.Unlock()
		return
	}
	if s.activeTurn != nil && !s.activeTurn.settling && s.activeTurn.phase == codexPhaseSent {
		if s.activeTurn.turnID == "" {
			s.activeTurn.bindIDLocked(p.Turn.ID)
			s.activeTurn.phase = codexPhaseIdentified
			if s.activeTurn.cancelPending && !s.activeTurn.interruptSent && !s.activeTurn.interruptInFlight {
				s.activeTurn.interruptSent = true
				needInterruptID = p.Turn.ID
				client = s.client
				threadID = s.threadID
			}
		}
	} else if s.activeTurn != nil && s.activeTurn.turnID == p.Turn.ID {
		// Duplicate turn/started for already-identified active turn: ignore.
	} else {
		// Autonomous main-thread turn (Plan 054 § D1).
		s.foreignTurnID = p.Turn.ID
		isForeign = true
	}
	s.mu.Unlock()

	if needInterruptID != "" && client != nil {
		go func() {
			ctx, cancel := context.WithTimeout(s.lifeCtx, codexCancelFallbackGrace)
			defer cancel()
			_ = client.TurnInterrupt(ctx, threadID, needInterruptID)
		}()
	}
	if isForeign {
		s.emit(Event{
			Type:        EventForeignTurn,
			ForeignTurn: &ForeignTurnInfo{ID: p.Turn.ID, Running: true},
		})
	}
}

func (s *codexSession) handleTurnCompleted(raw json.RawMessage) {
	var p codexrpc.TurnEventParams
	if json.Unmarshal(raw, &p) != nil {
		return
	}
	if !s.isMainThread(p.ThreadID) {
		return
	}
	s.mu.Lock()
	if p.Turn.ID != "" && s.settledTurns[p.Turn.ID] {
		s.mu.Unlock()
		return
	}
	turn := s.activeTurn
	foreign := s.foreignTurnID
	// Check known foreign turn ownership FIRST so an early foreign completion
	// can never bind to an unidentified local turn (M6).
	if foreign != "" && (foreign == p.Turn.ID || (p.Turn.ID == "" && turn == nil)) {
		toolFinal := toolCompleted
		subFinal := SubagentCompleted
		switch p.Turn.Status {
		case "failed":
			toolFinal = toolFailed
			subFinal = SubagentFailed
		case "interrupted":
			toolFinal = "cancelled"
			subFinal = SubagentCancelled
		}
		now := s.Now()
		var retireEvs []Event
		for _, id := range s.toolOrder {
			row, ok := s.tools[id]
			if !ok || !toolStatusInFlight(row.Status) {
				continue
			}
			row.Status = toolFinal
			row.At = now
			s.tools[id] = row
			ct := cloneTool(row)
			retireEvs = append(retireEvs, Event{Type: EventTool, Tool: &ct})
		}
		for _, id := range s.subagentOrder {
			rec, ok := s.subagents[id]
			if !ok || rec.info.Status != SubagentRunning || rec.turn != 0 {
				continue
			}
			rec.info.Status = subFinal
			if rec.info.EndedAt.IsZero() {
				rec.info.EndedAt = now
			}
			info := cloneSubagent(rec.info)
			retireEvs = append(retireEvs, Event{Type: EventSubagent, Subagent: &info, SubagentChange: SubagentChangeFinished})
		}
		s.foreignTurnID = ""
		if p.Turn.ID != "" {
			s.recordSettledTurnLocked(p.Turn.ID)
		}
		s.mu.Unlock()
		for _, ev := range retireEvs {
			s.emit(ev)
		}
		info := &ForeignTurnInfo{
			ID:        foreign,
			Running:   false,
			Cancelled: p.Turn.Status == "interrupted",
		}
		if p.Turn.Status == "failed" {
			err := s.turnErrorFromWire(p.Turn.Error)
			info.Err = &TurnError{Kind: ErrorInfo(err).Kind, Message: err.Error()}
		}
		s.emit(Event{Type: EventForeignTurn, ForeignTurn: info})
		return
	}
	if turn != nil && !turn.settling && (turn.phase == codexPhaseSent || turn.phase == codexPhaseIdentified) {
		if turn.phase == codexPhaseSent && turn.turnID == "" && p.Turn.ID != "" {
			// turn/completed arrived before turn/start reply: bind and settle now.
			turn.bindIDLocked(p.Turn.ID)
			turn.phase = codexPhaseIdentified
		}
		if turn.turnID == p.Turn.ID || (p.Turn.ID == "" && turn.phase == codexPhaseIdentified) {
			delete(s.earlyCompleted, p.Turn.ID)
			s.mu.Unlock()
			s.finishTurnFromWire(turn, p.Turn)
			return
		}
	}
	// Only retain bounded provisional early completion when a turn/start is sent
	// and awaiting identification (M6).
	if p.Turn.ID != "" && turn != nil && turn.phase == codexPhaseSent && !turn.settling {
		if len(s.earlyCompleted) < 64 {
			s.earlyCompleted[p.Turn.ID] = p.Turn
		}
	}
	s.mu.Unlock()
}

func (s *codexSession) handleTokenUsageUpdated(raw json.RawMessage) {
	var p codexrpc.TokenUsageUpdatedParams
	if json.Unmarshal(raw, &p) != nil {
		return
	}
	if !s.isMainThread(p.ThreadID) {
		return
	}
	s.mu.Lock()
	active := s.activeTurn
	foreign := s.foreignTurnID
	model := s.snap.CurrentModel
	asBaseline := false
	switch {
	case active != nil && !active.settling && (active.phase == codexPhaseSent || active.phase == codexPhaseIdentified):
		if active.model != "" {
			model = active.model
		}
		// If this update carries a TurnID that does not match our identified
		// active turn (e.g. a late historical token update on resume), treat it
		// as baseline rather than attributing prior-turn spend to this turn (M10).
		if p.TurnID != "" && active.turnID != "" && p.TurnID != active.turnID {
			asBaseline = true
		} else if p.TurnID != "" && s.settledTurns[p.TurnID] {
			asBaseline = true
		}
	case foreign != "" && (p.TurnID == "" || p.TurnID == foreign):
		asBaseline = false
	default:
		// No live turn in flight (or still in pre-wire claimed): historical or
		// baseline usage update (M10).
		asBaseline = true
	}
	s.usage.applyTokenUsage(p.TokenUsage, model, asBaseline)
	s.publishUsageLocked()
	s.mu.Unlock()
}

func (s *codexSession) handleRateLimitsUpdated(raw json.RawMessage) {
	var p codexrpc.RateLimitsUpdatedParams
	if json.Unmarshal(raw, &p) != nil || p.RateLimits == nil {
		return
	}
	s.mu.Lock()
	if s.usage.mergeRateLimits(p.RateLimits) {
		s.publishUsageLocked()
	}
	s.mu.Unlock()
}

func (s *codexSession) handleErrorNotification(raw json.RawMessage) {
	var p codexrpc.ErrorNotificationParams
	if json.Unmarshal(raw, &p) != nil {
		return
	}
	if !s.isMainThread(p.ThreadID) {
		return
	}
	s.mu.Lock()
	if p.TurnID != "" && s.settledTurns[p.TurnID] {
		s.mu.Unlock()
		return
	}
	var turn *codexTurnState
	if s.activeTurn != nil && !s.activeTurn.settling && (s.activeTurn.phase == codexPhaseSent || s.activeTurn.phase == codexPhaseIdentified) {
		if p.TurnID == "" || s.activeTurn.turnID == "" || s.activeTurn.turnID == p.TurnID {
			turn = s.activeTurn
		}
	}
	foreignMatch := s.foreignTurnID != "" && (p.TurnID == "" || s.foreignTurnID == p.TurnID)
	client := s.client
	s.mu.Unlock()

	// Ignore turn-scoped errors that belong to an unknown/stale turn ID (M6).
	if p.TurnID != "" && turn == nil && !foreignMatch {
		return
	}

	if p.WillRetry {
		// Fast-fail non-transient terminal errors (401/auth, explicit usage
		// limits, contextWindowExceeded) even when willRetry: true (Plan 054 § D5, M13).
		if codexrpc.IsTerminalEvenWhenWillRetry(&p.Error) {
			err := s.turnErrorFromWire(&p.Error)
			// Abort transport immediately independent of event publication backpressure.
			if client != nil {
				client.CloseAsync()
			}
			if turn != nil {
				_, _ = s.settleTurn(turn, "", err, true)
			} else {
				s.emit(Event{Type: EventError, Err: err})
			}
			return
		}
		s.emit(Event{
			Type: EventNotice,
			Notice: &NoticeInfo{
				Severity:    "warning",
				Title:       "Codex retrying",
				Description: sanitizeLine(p.Error.Message),
			},
		})
		return
	}
	// Non-retryable error: deduplicate with subsequent turn/completed{status: "failed"}
	// by settling the matching active turn once here.
	err := s.turnErrorFromWire(&p.Error)
	if turn != nil {
		_, _ = s.settleTurn(turn, "", err, true)
		return
	}
	s.emit(Event{Type: EventError, Err: err})
}
