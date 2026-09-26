// Package harness is craze's native agent harness: a Session holds one
// conversation with a model from the harness's own model table and runs it a
// turn at a time on Fantasy's agent loop (plan 018 §3.7). A turn streams its
// text, thinking and tool calls to a sink as they happen, is written to the
// session's transcript a step at a time from Fantasy's callbacks (package
// store), and ends in a stop reason or in one of a small set of errors the
// adapter can phrase (errors.go).
//
// The model has tools (plan 019): the session's tool profile — opencode's
// read, write, edit, bash, grep and glob, and the system prompt written for
// them — is chosen when it opens, and a turn runs as many steps as the model
// needs, running the calls of each through the tool framework
// (internal/harness/tool), up to a limit. toolbridge.go is the one file that
// adapts the framework to Fantasy; tools.go builds a session's tools.
//
// The harness imports nothing from the rest of craze but internal/atomicfile
// (through modeltable and the tools): its directory, workspace, model table
// and craze's version all come in through Options, from the native adapter
// in internal/agent (plan 018 §3.1).
//
// # Models and effort
//
// A session starts on Options.Model (or the table's default) and switches
// with SetModel and SetEffort, which build the new model's client at once —
// so a missing key or an unknown alias fails the switch, not the next turn —
// and are allowed while a turn runs: Current changes immediately, the
// running turn finishes on the model it started with, and the next Run
// records the switch in the transcript before its own messages, so each
// turn's prompt and answer stay adjacent in the file.
//
// # Modes
//
// A session runs in agent, plan or ask mode (Options.Mode, SetMode, Mode).
// The mode is enforced by the tool gate — plan mode lets an edit-kind call
// touch only the session's plan file, ask mode refuses everything that is not
// read-only — and told to the model by a reminder spliced into the step's
// input, which is never persisted and never shown (reminders.go, plan 023
// §3.1, §3.3).
//
// # Concurrency
//
// One Run at a time (ErrInTurn otherwise). Every other method may be called
// from any goroutine, and all of them but Close from inside Run's sink too:
// the session never holds its lock while calling the sink or the model.
// Close cancels a live Run and waits for it to return, so it must not be
// called from the sink, which runs on that Run and would wait for itself.
//
// # Interjection
//
// Steer merges a user message into the running turn: the turn takes it up
// before its next step, the model reads it, and the step that saw it writes it
// to the transcript. Whatever no step persisted comes back in
// Result.Unanswered (steer.go, plan 019 §3.10).
//
// # Sub-agents
//
// The agent tool starts a sub-agent: a second Session, opened in-process as
// the parent's child (child.go), which runs one turn on the call's goroutine
// and answers the call with its final message (subagents.go, plan 026). At
// most four run at once per session; a call blocks until its child has run
// and closed, so the turn outlives every child it started. The child's own
// events reach Run's sink wrapped in SubagentEvent, from the child's turn,
// which makes the sink concurrent while children run (see Run). Close tells
// the children it is closing before it joins its turn.
//
// A session opened with Options.Background runs a call that asks for it in
// the background (background.go, plan 026 §3.11): the call returns once the
// child has started, the child reports to the session's sink (Options.Sink),
// and its result is delivered to the model once — at a running turn's next
// step boundary, at the start of the next turn, by Wake, a turn of its own, or
// by the agent_output tool — never through the steer box.
//
// # Resume
//
// A session opened with Options.Resume continues a stored one (resume.go,
// plan 028 §3.3): the same transcript, reopened under its lock, with the
// model, effort, mode, todo list and turn numbering its path records. Replay
// hands the stored conversation to a sink as the events a turn would have
// emitted for it, once, before the first turn (replay.go, §3.4).
package harness

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"charm.land/fantasy"
	"github.com/charliek/craze/internal/harness/llm"
	"github.com/charliek/craze/internal/harness/modeltable"
	"github.com/charliek/craze/internal/harness/redact"
	"github.com/charliek/craze/internal/harness/store"
)

// Options configure Open.
type Options struct {
	// Home is the harness's directory, <craze dir>/native; absolute.
	// Transcripts are written under Home/sessions.
	Home string
	// Workspace is the session's working directory; absolute. The system
	// prompt names it, and the transcript is filed under it.
	Workspace string
	// Table is the model catalog. The session reads it and never changes it.
	Table *modeltable.Table
	// Model is the alias the session starts on; "" is the table's default.
	// For a resumed session (Resume) "" is unspecified — the transcript's own
	// model, found by its identity, else the default — and anything else is
	// explicit and must resolve (plan 028 §3.3).
	Model string
	// Resume reopens the stored session with this id instead of starting one
	// (plan 028 §3.3): its transcript in Workspace's session directory under
	// Home is found (store.Find) and reopened for appending under its lock
	// (store.Open), and the session continues it — the same id, the same file,
	// the next turn numbered after the last recorded. The header's tool
	// profile is the session's, and a model with another is never resumed
	// on. Model, Effort and Mode set are explicit and win; each left "" is
	// the transcript's own: the last model on its path, matched by (provider,
	// wire model) — its own alias if that still names the model, else the
	// first alias in sorted order that does — then the table's default, and
	// ErrResumeModel if neither resolves (a fall-back from the transcript's
	// model is reported to Warn); the last effort, if that model accepts it,
	// else its default; the last mode_change, else agent. The todo list is
	// the last one a tool entry on the path recorded, and the model is not
	// told again of a mode it heard. The system prompt is rebuilt from Prompt
	// (D-30: frozen per incarnation), and the transcript records its digest
	// in a resume entry written with the first step.
	//
	// A session with no file at all is ErrNoTranscript, returned wrapped with
	// nothing opened — a caller continuing it opens a new session under the
	// same id instead (SessionID); a file that is not the session's, or that
	// the store cannot reopen — busy, corrupt, a child's, a newer craze's
	// tail — is the store's error, wrapped "harness: resume <id>: …". ""
	// starts a new session. A sub-agent is never resumed.
	Resume string
	// SessionID is the id a NEW session is opened under; "" mints a fresh
	// one. It is for a session whose id is already known and whose file was
	// never written: an index row seeded by a first prompt that produced no
	// output, which a load then opens empty under the row's own id rather than
	// as another session (plan 028 §3.5, P35) — the caller's answer to
	// ErrNoTranscript. It must be an id Find would accept (store.ErrBadSessionID
	// otherwise). Nothing is written until the first turn that produces
	// output, as for any new session. It cannot be combined with Resume, which
	// names the id itself, nor with Child, whose id is its runner's.
	SessionID string
	// Prompt is what the caller adds to the frozen system prompt: the
	// instruction documents this workspace's user and project wrote, and the
	// catalog of the skills and commands installed for it (plan 022 §3.4).
	// The harness renders and redacts it once, in Open, and reads no file for
	// it: the adapter in internal/agent resolves it first and hands it over
	// as data. The zero value sends the tool profile's text alone.
	Prompt PromptExtras
	// Effort is the effort the session starts at; "" is the model's
	// default_effort (none, for a model with no effort control) — for a
	// resumed session, the transcript's last effort first (Resume).
	Effort string
	// Mode is the mode the session starts in: "" or "agent" (implement),
	// "plan" or "ask". Anything else is ErrUnknownMode and refuses Open. A
	// session opened in plan mode creates its plan file and tells the model
	// at its first step (reminders.go, plan 023 §3.1). A sub-agent's mode is
	// Child.Mode, and this is not read. For a resumed session "" is the
	// transcript's own mode, and "agent" is explicit (Resume).
	Mode string
	// Asker is how the session's tools reach a person: ask_user_question and
	// exit_plan_mode block on it (tool.Asker, plan 023 §3.4). The adapter in
	// internal/agent hands in one over craze's ask registry; nil is nobody to
	// ask, and both tools then answer at once that nobody answered. A
	// sub-agent has nobody to ask, and this is not read.
	Asker Asker
	// Personas are the agent types the adapter found in persona files — the
	// workspace's .claude/agents chain, ~/.claude/agents and installed
	// plugins' agents/ — with their tools already mapped to native ids
	// (tool.MapClaudeTools) and a user persona that would shadow a built-in
	// already dropped (plan 026 §3.4). The session merges them with its own
	// built-ins (BuiltinAgentTypes) in precedence order, lists them in the
	// agent tool's description, and resolves a call's subagent_type against
	// that one list. Like Prompt it is data, taken once at Open; nil offers
	// the built-ins alone. A sub-agent has no agent tool, and this is not read
	// for one.
	Personas []Persona
	// Child opens the session as a sub-agent of another (child.go, plan 026
	// §3.2); nil is an ordinary session. Only the runner that starts children
	// sets it, and fills the rest of these Options with the parent's own
	// values.
	Child *ChildOptions
	// NewModel builds a model's client; nil is llm.New. It is the test seam:
	// a test hands in a scripted fantasy.LanguageModel.
	NewModel func(modeltable.Resolved) (fantasy.LanguageModel, error)
	// Getenv looks up the environment variables that hold API keys; nil is
	// os.Getenv. The ACP child's environment (agent.Options.Env) is not the
	// harness's, and tests never read the real one (plan 018 §3.5).
	Getenv func(string) string
	// Now stamps the transcript; nil is time.Now.
	Now func() time.Time
	// Version is craze's version, recorded in the transcript's header. It is
	// passed in so the harness does not import craze's version package.
	Version string

	// MatchModel is the adapter's `--model` normalisation (case, spaces, a
	// display name; internal/agent.MatchModel), handed in at Open so the
	// harness itself never imports internal/agent. It resolves a sub-agent
	// call's `model` field to a table alias (subagent_models.go, plan 026
	// §3.6); nothing else in the harness uses it. nil is exact alias match
	// only: a call's `model` must name a table alias byte for byte.
	MatchModel func(raw string) (alias string, ok bool)
	// Warn is a diagnostic channel for runtime fall-throughs that are not
	// errors: a sub-agent persona's or the configured default's model or
	// effort that does not resolve, so resolution moves on to the next
	// candidate instead of failing the call (subagent_models.go, plan 026
	// §3.6); and a resumed session that cannot continue on its transcript's
	// model, or read its last mode, and moves on to the next (Resume, plan
	// 028 §3.3). nil discards; the adapter journals what it is handed.
	Warn func(string)

	// Background lets the agent tool start a sub-agent in the background
	// when the model asks for it (run_in_background, plan 026 §3.11): the call
	// returns once the child has started, the child runs on for the session's
	// life, and its result is delivered to the parent's model later — at a step
	// boundary of a running turn, at the start of the next one, by Wake, or by
	// an agent_output call. False, the default, runs every agent call in the
	// foreground, run_in_background or not, which is what a session nobody
	// wakes (headless `craze prompt`) needs. Not read for a sub-agent.
	Background bool
	// Sink is the session's own sink, beside each Run's (plan 026 §3.11): a
	// background child outlives the turn that started it, so its own events
	// (wrapped in SubagentEvent), its SubagentFinished and, at Close, a
	// SubagentUndelivered for a result never delivered come here. It is
	// entered concurrently — each child's events from the child's turn, under
	// that turn's lock and nothing of the parent's, and a child's end from the
	// goroutine that ran it, with no lock of the harness's held — and must
	// not block for long, nor call Close. nil discards. Not read for a
	// sub-agent.
	Sink func(Event)
	// OnPending is called whenever a background child's result becomes
	// waiting to be delivered (HasPending): the child finished, or a result
	// taken for a turn that then wrote nothing was given back. It is called
	// with no lock of the harness's held, so it may call the session's
	// methods (HasPending), but it must return at once: the caller that
	// delivers — Wake — belongs on a goroutine of the caller's own, which this
	// only signals. nil is none. Not read for a sub-agent.
	OnPending func()

	// tools are the tool set's test seams (tools.go); zero is production.
	tools toolSeams
}

// ModelInfo is one model-table entry as a model picker shows it.
type ModelInfo struct {
	Alias         string   // what SetModel takes
	Name          string   // the table's name, or the alias when it has none
	Provider      string   // the provider id
	Efforts       []string // the efforts SetEffort takes, in display order; nil = no effort control
	DefaultEffort string
}

// Session is one conversation. See the package comment.
type Session struct {
	system string // the frozen system prompt, the tool profile's
	// promptSHA is the hex SHA-256 of system, as the transcript records it
	// for this incarnation: the header's for a new session, the resume
	// entry's for a resumed one (PromptSHA256).
	promptSHA string
	store     *store.Store
	tools     *toolset // fixed at Open
	getenv    func(string) string
	newModel  func(modeltable.Resolved) (fantasy.LanguageModel, error)
	// newAgent builds a turn's agent: the model, the system prompt and the
	// session's tools. It is fantasy.NewAgent (defaultAgent); a test may
	// replace it before the first Run.
	newAgent func(lm fantasy.LanguageModel, system string, tools []fantasy.AgentTool) fantasy.Agent

	closeOnce sync.Once
	closeErr  error

	// steers is Interject's accept side: the text Steer has handed the running
	// turn and no step has taken up yet. It carries its own lock, which mu is
	// never held across and which is held across nothing (steer.go), so an
	// interjection can never wait on the turn it is meant for.
	steers steerbox

	// modes is the session's mode, the plan file and the reminders that carry
	// both to the model (reminders.go). Like the steer box it has its own leaf
	// lock, so a turn reading it at a step boundary and a SetMode from the UI
	// never wait on each other. Fixed at Open, never nil.
	modes *modes

	// child is set for a sub-agent's session (Options.Child), fixed at Open:
	// its mode never changes (SetMode), and it starts no sub-agent of its own.
	child bool

	// subs is the sub-agent runner (subagents.go, plan 026 §3.8): the
	// registry of this session's live children, the cap on them, and the
	// running turn they report to. The agent tool reaches it through the
	// dispatcher's Env.Subagents. nil for a sub-agent, which starts none.
	// Fixed at Open.
	subs *subagents
	// base is what a child inherits of the Options this session was opened
	// with and keeps nowhere else (plan 026 §3.2); zero for a sub-agent.
	// Fixed at Open.
	base childBase
	// now is Options.Now, defaulted: the clock a sub-agent's lifecycle
	// events are stamped with, as the transcript is.
	now func() time.Time

	// matchModel and warn are Options.MatchModel and Options.Warn, read only
	// by a sub-agent's model and effort resolution (subagent_models.go, plan
	// 026 §3.6). Neither is defaulted here: matchModel's nil behaviour (exact
	// alias match) and warn's (discard) are handled where each is called.
	matchModel func(raw string) (alias string, ok bool)
	warn       func(string)

	mu      sync.Mutex
	table   *modeltable.Table
	cur     model  // what the next turn runs on; Current reports it
	logged  logged // what the transcript was last told the model, effort and mode are
	closed  bool
	running bool // a turn, or a Replay, holds the session
	// turns numbers the turns: the turns begun, which number their tool
	// calls' ids — for a resumed session counted on from the largest turn its
	// transcript recorded (plan 028 §3.3).
	turns    int
	begun    bool                    // a turn has begun since Open: Replay is too late
	replayed bool                    // Replay has run
	cancel   context.CancelCauseFunc // the live turn's; nil when idle
	done     chan struct{}           // closed when the live turn has returned
}

// defaultAgent is a turn's agent: Fantasy's, with the frozen system prompt,
// one retry, and the session's tools in the profile's order.
func defaultAgent(lm fantasy.LanguageModel, system string, tools []fantasy.AgentTool) fantasy.Agent {
	return fantasy.NewAgent(lm,
		fantasy.WithSystemPrompt(system),
		fantasy.WithMaxRetries(maxRetries),
		fantasy.WithTools(tools...))
}

// model is a built model and the effort a turn sends it: everything a turn
// needs, fixed from the turn's start to its end whatever SetModel does
// meanwhile.
type model struct {
	r          modeltable.Resolved
	lm         fantasy.LanguageModel
	effort     string
	effortOpts fantasy.ProviderOptions // nil when effort is ""
}

// id is how the transcript names m's model: the table's values as they are
// now, so the file stays readable after the alias is re-pointed.
func (m model) id() store.Model {
	return store.Model{Provider: m.r.ProviderID, Alias: m.r.Alias, WireModel: m.r.WireModel}
}

// logged is the model, effort and mode the transcript last had a turn or a
// change entry for (held or written). A turn that runs on anything else
// first appends a change entry.
//
// Model and effort are compared when a turn begins; the mode is compared as
// the step whose request announced it is appended, so a switch made mid-turn
// is recorded where it became visible rather than at the next turn's start
// (plan 023 §3.1). It starts at agent: a session opened in another mode
// records the switch to it, since the header carries no mode.
type logged struct {
	model  store.Model
	effort string
	mode   string
}

// Open starts a session: it resolves and builds the starting model, and from
// it the session's tools (tools.go) — the profile, and with it the system
// prompt, which is the profile's text and Options.Prompt rendered after it
// and whose hash the transcript's header records, and the redactor over every
// key the table knows of. It writes nothing — the transcript appears with
// the first turn that produces output — so a session closed before that
// leaves nothing behind; it only sweeps old spill files. A starting model
// whose provider has no key is ErrNoAPIKey; the caller decides whether to
// fall back to another model (plan 018 §3.8). A key anywhere in the table,
// used or not, that is too short to redact from tool output fails it
// (modeltable.ErrKeyTooShort, plan 019 §3.8).
//
// With Options.Child set it opens a sub-agent (child.go, plan 026 §3.2): the
// runner's id, the parent's links in the header, a filtered toolset, the
// parent's frozen prompt with a role section after it, the parent's mode and
// no plan file, nobody to ask, and the parent's path locks. A key inside that
// prompt refuses it (errChildPromptKey), as does one in its home's path, which
// begins every spill path of its calls (errChildHomeKey).
//
// With Options.Resume set it reopens a stored session instead (resume.go,
// plan 028 §3.3). That Open holds the transcript's file and its lock from the
// store's Open on, and releases both on every error after it.
func Open(opts Options) (*Session, error) {
	if opts.Table == nil {
		return nil, errors.New("harness: no model table")
	}
	child := opts.Child
	s := &Session{
		getenv:     opts.Getenv,
		newModel:   opts.NewModel,
		newAgent:   defaultAgent,
		table:      opts.Table,
		child:      child != nil,
		matchModel: opts.MatchModel,
		warn:       opts.Warn,
		now:        opts.Now,
	}
	if s.getenv == nil {
		s.getenv = os.Getenv
	}
	if s.newModel == nil {
		s.newModel = func(r modeltable.Resolved) (fantasy.LanguageModel, error) { return llm.New(r) }
	}
	if s.now == nil {
		s.now = time.Now
	}
	// A session that is not a sub-agent can start them: its runner exists
	// before its tools, which hand it to the agent tool (Env.Subagents), and
	// reads the rest of the session only when a call arrives, by which time
	// Open has returned it. A child gets neither: depth is 1 (plan 026 §3.2).
	if child == nil {
		s.subs = newSubagents(s)
		s.subs.background, s.subs.sink, s.subs.onPending = opts.Background, opts.Sink, opts.OnPending
		s.base = childBase{
			home: opts.Home, workspace: opts.Workspace, version: opts.Version, now: opts.Now,
			prompt: opts.Prompt.clone(), seams: opts.tools,
		}
	}
	if opts.SessionID != "" && (opts.Resume != "" || child != nil) {
		return nil, errors.New("harness: SessionID names a new session's id, and is not one to resume nor a sub-agent's")
	}
	if opts.Resume != "" {
		if child != nil {
			return nil, errors.New("harness: a sub-agent's session is never resumed")
		}
		return s.openResumed(opts)
	}

	modeID, asker := opts.Mode, opts.Asker
	if child != nil {
		// A child's id is its runner's, so the parent can name it before it
		// writes anything; one that minted none has nothing to give it.
		if child.ID == "" {
			return nil, errors.New("harness: a sub-agent's session needs the id its runner minted")
		}
		modeID, asker = child.Mode, nil
	}
	mode, err := normalizeMode(modeID)
	if err != nil {
		return nil, err
	}

	alias := opts.Model
	if alias == "" {
		alias = opts.Table.DefaultModel
	}
	m, err := s.build(opts.Table, alias)
	if err != nil {
		return nil, err
	}
	effort := opts.Effort
	if effort == "" {
		effort = m.r.DefaultEffort
	}
	if m, err = withEffort(m, effort); err != nil {
		return nil, err
	}
	if s.tools, err = openTools(opts.Home, filepath.Clean(opts.Workspace), mode, asker, opts.Table, s.getenv, modelRef(m.r), opts.Prompt, opts.Personas, child, s.subs, opts.tools); err != nil {
		return nil, err
	}
	s.system = s.tools.system
	if opts.SessionID != "" {
		// Held to Find's rule rather than only New's, so that the session a
		// caller opens under an id of its own can be found by it again (plan
		// 028 §3.5).
		if err := store.CheckSessionID(opts.SessionID); err != nil {
			return nil, fmt.Errorf("harness: %w", s.tools.redactErr(err))
		}
	}
	sopts := store.Options{
		Home: opts.Home,
		// The real working directory: the header records it and the prompt
		// names it, and openTools has refused one whose path holds a provider
		// key (errWorkspaceKey), so neither can carry one.
		Workspace:    opts.Workspace,
		CrazeVersion: opts.Version,
		SystemPrompt: s.system,
		ToolProfile:  s.tools.profile,
		Tools:        s.tools.wire,
		Now:          opts.Now,
		// "" for a fresh id; a caller's own for a session whose id is known
		// before it has a file (Options.SessionID). A child's is set below.
		SessionID: opts.SessionID,
	}
	if child != nil {
		// The header goes to disk as it is. The type and the persona's path
		// come from files a plugin or a repository wrote, so they are redacted
		// as the prompt's extras are; the two ids are the runner's own, and
		// are redacted alike because one rule is simpler to check than two.
		// The session id cannot be: it names the file.
		red := s.tools.redactor()
		sopts.SessionID = child.ID
		sopts.ParentSession = red.String(child.ParentSession)
		sopts.ParentToolCall = red.String(child.ParentCall)
		sopts.SubagentType = red.String(child.Type)
		sopts.PersonaPath = red.String(child.PersonaPath)
	}
	st, err := store.New(sopts)
	if err != nil {
		return nil, fmt.Errorf("harness: %w", s.tools.redactErr(err))
	}
	// The header's two digests are disclosed exactly as they are: the
	// transcript holds them, and the adapter repeats the prompt's in its
	// prompt_sources note, which C6 requires to be the same string as the
	// header's. So a configured key that happens to be inside one refuses the
	// session — errWorkspaceKey's reasoning applied to the one text craze
	// computes rather than reads, since redacting a digest would break the
	// equality and leave something that is no longer a digest. Contrived, and
	// closed here because it is one comparison.
	if h := st.Header(); s.tools.holdsKey(h.SystemPromptSHA256) || s.tools.holdsKey(h.ToolsSHA256) {
		return nil, errDigestKey
	}
	// And the line as a whole, which is what reaches the disk. Every field
	// that came from outside the harness is redacted above, one at a time, but
	// the encoding writes `","persona_path":"` and the like between two of
	// them, so a key spelled across that framing is in the line and in neither
	// field (plan 026 r1): the prompt's join, one level down. Nothing here can
	// be rewritten — the fields are what they are — so it refuses, as the
	// digests do. An ordinary header holds no key and this changes nothing.
	if line, err := st.HeaderLine(); err != nil || s.tools.holdsKey(string(line)) {
		return nil, errHeaderKey
	}
	s.store = st
	s.promptSHA = st.Header().SystemPromptSHA256
	s.cur = m
	s.logged = logged{model: m.id(), effort: m.effort, mode: modeAgent}
	// The plan file is the transcript's sibling, so it is named only now that
	// the store has named the transcript; the gate learns it here and nowhere
	// else. A session that opens in plan mode creates the file at once, so the
	// reminder, the gate and the model all mean one path that is there.
	//
	// The path itself goes to the model verbatim in every plan-mode reminder
	// (§3.3), so a configured key inside it refuses the session the way the
	// working directory's does (errPlanPathKey): redacting it would leave the
	// model a path that opens nothing. A key the session learns later is
	// refused with the switch that brought it (toolset.resolve).
	//
	// A sub-agent has no plan file at all (plan 026 §3.2): nothing is adopted
	// or created, the gate's plan path stays "", and in plan mode every edit
	// it attempts is refused.
	if child != nil {
		s.modes = newChildModes(mode, s.tools.modeGate)
		return s, nil
	}
	plan := store.PlanPath(st.Path())
	if err := s.tools.adoptPlanPath(plan); err != nil {
		return nil, err
	}
	s.modes = newModes(mode, plan, s.tools.modeGate)
	if mode == modePlan {
		if err := store.CreatePlanFile(s.modes.planPath); err != nil {
			return nil, fmt.Errorf("harness: %w", s.tools.redactErr(err))
		}
	}
	return s, nil
}

// build resolves alias in table and builds its client, with no effort set.
// Resolve's errors (ErrUnknownModel, ErrNoAPIKey) name the alias and the
// provider and never a key; so do llm.New's.
func (s *Session) build(table *modeltable.Table, alias string) (model, error) {
	r, err := table.Resolve(alias, s.getenv)
	if err != nil {
		return model{}, fmt.Errorf("harness: %w", err)
	}
	lm, err := s.newModel(r)
	if err != nil {
		return model{}, fmt.Errorf("harness: model %q: %w", alias, err)
	}
	return model{r: r, lm: lm}, nil
}

// withEffort is m at effort, with the provider options that ask for it. An
// effort m's model does not list, or one its driver cannot send, is an
// error, so a bad switch fails when it is made.
func withEffort(m model, effort string) (model, error) {
	opts, err := llm.EffortOptions(m.r, effort)
	if err != nil {
		return model{}, fmt.Errorf("harness: effort %q: %w", effort, err)
	}
	m.effort, m.effortOpts = effort, opts
	return m, nil
}

// carriedEffort is the effort a switch to r's model lands on: the current
// one when the new model lists it, otherwise its default_effort, otherwise
// none (plan 018 §3.7).
func carriedEffort(current string, r modeltable.Resolved) string {
	if slices.Contains(r.Efforts, current) {
		return current
	}
	return r.DefaultEffort
}

// SetModel switches the session to alias for the next turn, keeping the
// current effort when the new model lists it (carriedEffort). The client is
// built now, so an unknown alias or a missing key fails here and leaves the
// current model in place. A turn already running finishes on its own model.
// Switching to the current alias rebuilds its client from the table, which
// is how a turn picks up a key that was just exported.
//
// A model whose tool profile is not the session's is refused with
// ErrProfileMismatch: the tools and the system prompt were fixed at Open
// (plan 019 §3.1).
//
// A switch is also where a session learns a provider key: the table is
// re-read for keys the environment gained since Open, and a redactor over
// them is prepared for the next turn to use (toolset.resolve, and begin,
// which takes it up). A key that cannot be redacted, or that is already
// inside what this session sends with every request, refuses the switch and
// leaves the model in place.
func (s *Session) SetModel(alias string) error {
	s.mu.Lock()
	table, closed := s.table, s.closed
	s.mu.Unlock()
	if closed {
		return ErrClosed
	}
	// Built outside the lock: NewModel is the caller's code, and a sink
	// calling Current must never wait on it.
	m, err := s.build(table, alias)
	if err != nil {
		return err
	}
	if err := s.tools.check(m.r); err != nil {
		return err
	}
	// Outside the lock, like the build: it reads the environment. It only
	// resolves; the next turn takes the redactor up (begin), so a turn
	// already running keeps the one it has redacted its steps with, and a
	// switch that fails below leaves nothing installed either.
	if err := s.tools.resolve(table, s.getenv); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	if m, err = withEffort(m, carriedEffort(s.cur.effort, m.r)); err != nil {
		return err
	}
	s.cur = m
	return nil
}

// SetEffort sets the effort the next turn asks for; "" means the current
// model's default_effort. A level the model does not list fails and changes
// nothing. A turn already running keeps its effort.
func (s *Session) SetEffort(level string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	if level == "" {
		level = s.cur.r.DefaultEffort
	}
	m, err := withEffort(s.cur, level)
	if err != nil {
		return err
	}
	s.cur = m
	return nil
}

// SetMode puts the session in mode id — "" or "agent", "plan", "ask" — and,
// like SetModel, is allowed while a turn runs: the gate judges the next call
// that reaches it under the new mode, a call already past the check runs, and
// the model is told at the turn's next step boundary (reminders.go, plan 023
// §3.1). An id that is none of the three is ErrUnknownMode and changes
// nothing; a session already in the mode is put in it again, which restarts
// the plan reminder's alternation and nothing else.
//
// Entering plan mode creates the session's plan file if it is not there — the
// only file an edit-kind call may touch in that mode — and a failure to
// create it fails the switch and leaves the mode alone, as a model that
// cannot be built leaves SetModel's alone. After Close it is ErrClosed.
//
// A sub-agent's mode is its parent's and fixed at Open: SetMode on one is
// ErrChildMode, whatever id it names, and changes nothing. The parent's later
// switches reach a running child through its gate instead, and only ever
// tighten it (tool.NewChildModeGate, plan 026 §3.5): the switch raises every
// registered child's strictness in the same critical section that sets the
// mode, under the runner's registry lock, and a child's registration reads
// the mode under that lock too — so a child is either registered before the
// switch and raised by it, or registered after it and opened in the new mode,
// and a round trip such as agent → ask → agent can never slip between two of
// a child's checks unseen (panel P50). The lock order is s.mu → regMu →
// modes.mu, and registration's regMu → modes.mu; nothing takes them the other
// way round.
func (s *Session) SetMode(id string) error {
	if s.child {
		return ErrChildMode
	}
	mode, err := normalizeMode(id)
	if err != nil {
		return err
	}
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return ErrClosed
	}
	// Outside the lock, like SetModel's build: it touches the file system.
	if mode == modePlan {
		if err := store.CreatePlanFile(s.modes.planPath); err != nil {
			return fmt.Errorf("harness: %w", s.tools.redactErr(err))
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	s.subs.setMode(mode, s.modes.set)
	return nil
}

// Mode is the mode the session is in: "agent", "plan" or "ask". It changes
// the moment SetMode succeeds, even while a turn runs. Current keeps its two
// results; the mode has never been one of them.
func (s *Session) Mode() string { return s.modes.current() }

// Models lists the model table's entries by alias. It includes models whose
// provider has no key: SetModel reports that when one is chosen.
func (s *Session) Models() []ModelInfo {
	s.mu.Lock()
	table := s.table
	s.mu.Unlock()
	out := make([]ModelInfo, 0, len(table.Models))
	for _, alias := range slices.Sorted(maps.Keys(table.Models)) {
		m := table.Models[alias]
		name := m.Name
		if name == "" {
			name = alias
		}
		out = append(out, ModelInfo{
			Alias:         alias,
			Name:          name,
			Provider:      m.Provider,
			Efforts:       slices.Clone(m.Efforts),
			DefaultEffort: m.DefaultEffort,
		})
	}
	return out
}

// Current is the model alias and effort the next turn will use. It changes
// the moment SetModel or SetEffort succeeds, even while a turn runs.
func (s *Session) Current() (model, effort string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cur.r.Alias, s.cur.effort
}

// ID is the session id: a UUID, in the transcript's header and file name.
func (s *Session) ID() string { return s.store.ID() }

// PromptSize and PromptSHA256 describe the frozen system prompt — the
// profile's text with Options.Prompt rendered after it — without handing it
// out: its size in bytes, and the digest the transcript records for it — the
// header's own value for a new session (store.New), rather than a second
// SHA-256 of the same string, and for a resumed one the digest its resume
// entry records for this incarnation (store.Open), the hex SHA-256 of the
// same string by the same rule (promptDigest). The adapter writes both into
// the journal note that says which files went into the prompt (plan 022
// §3.4), so a note and a transcript never disagree about which prompt a
// session sent. The text itself stays unexported: it is never stored, and a
// caller that could read it back would be a caller that could log it.
//
// Both are fixed at Open and take no lock.
func (s *Session) PromptSize() int      { return len(s.system) }
func (s *Session) PromptSHA256() string { return s.promptSHA }

// Redact is the session's redactor, over text a caller is about to hand to
// Run or Steer. Everything the harness itself writes or reports goes through
// that redactor already, but the user's own prompt does not: Run persists it
// and sends it exactly as given (turn.go), which is the right rule for text a
// person typed and the wrong one for text craze assembled out of files on
// disk. A command body holding a provider key would otherwise reach the wire,
// the transcript, the journal and `craze prompt --json` at once.
//
// It is the redactor over every key the session knows, which is a superset of
// the one installed: a switch that resolved a provider key the environment
// gained since Open leaves it prepared until the next turn's begin adopts it
// (toolset.resolve, adopt), and a caller redacting between the two has to
// cover it — the turn it is about to hand the text to will. Redacting more
// than a turn needs is never a leak; redacting less puts the key on the wire.
// Safe from any goroutine. What it cannot cover is a switch that lands after
// it has returned and before the Run it was redacting for: two calls cannot
// be made one from out here, and the caller that cares holds the prompt path
// between them.
//
// It covers the session's sub-agents' keys too (plan 026 §3.9, review r8,
// finding 1): a child that opened after the environment gained a key knows
// one its parent does not, and the adapter publishes what the child wrote —
// its roster row, its lifecycle, the failure its call returns — through this
// redactor, after sanitizing, which can put a key the runner's union could not
// see back together. So every registered child's keys are covered — a child
// is still registered when its SubagentFinished reaches the sink — and so are
// those of every child the running turn has retired, until that turn ends:
// the call's ToolFinished follows the retirement and carries the child's
// text (subagents.childKeys). One replacer over them all, never two in turn
// (subagents.union says why).
func (s *Session) Redact(text string) string { return s.redactor().String(text) }

// Redactor is Redact taken once: a function over the keys Redact covers now,
// which takes no lock when it is applied. A caller that redacts inside a lock
// of its own, where Redact's — the toolset's and the runner's registry's —
// may not be taken, takes it before that lock (the adapter's roster and task
// rows, plan 026 §3.9, review r8). It cannot cover a key learned after it was
// taken: Redact's own limit, for one call.
func (s *Session) Redactor() func(string) string { return s.redactor().String }

// redactor is the replacer Redact applies: the widest one while no sub-agent
// knows a key the session does not, and otherwise one over both sessions'
// keys together. The toolset's lock and the runner's are each taken alone
// (knownKeys, widest, childKeys): both are leaves.
func (s *Session) redactor() *redact.Replacer {
	keys, kids := s.tools.knownKeys(), s.subs.childKeys()
	if !slices.ContainsFunc(kids, func(k string) bool { return !slices.Contains(keys, k) }) {
		return s.tools.widest()
	}
	return redact.New(append(keys, kids...)...)
}

// Close ends the session: it cancels a live Run and waits for it to return —
// by which time a partial answer has been persisted, interrupted — then
// closes the transcript. It is idempotent and safe from any goroutine,
// concurrently with a Run that is finishing on its own; a second Close waits
// for the first and returns its result. Every Run, SetModel and SetEffort
// after it is ErrClosed. Close must not be called from inside Run's sink,
// which runs on the turn it would wait for.
//
// The cancel says the session is closing (tool.ErrClosing), and Close also
// closes the tools' closing channel, which reaches a call whose turn was
// already cancelled for another reason: either way a running command is
// killed at once, not given its grace, so Close returns within the tools'
// close bound — about 3 s — even after a cancel (plan 019 §3.9, §7.7).
//
// A session's sub-agents are told first (plan 026 §3.8, panel P3): Close
// seals the runner's registry, so no child registers after it, and signals
// every registered child's closing — the child's own cancel with
// tool.ErrClosing and its own closing channel, without waiting for either —
// before it cancels and joins its own turn. A cause is fixed by the first
// cancel, and the adapter's Close cancels the turn ordinarily before it calls
// this one, which reaches a child through its context first; without the
// signal the child's closing channel would stay open until the child itself
// closed, after its commands had had their grace. The children are joined by
// the join of the turn: an agent call returns only once its child has run and
// closed, and the turn only once its calls have.
//
// Background children (plan 026 §3.11) are signalled with the rest, and
// joined after the turn: Close cancels the session's background context, then
// waits for every background child's goroutine to have closed the child and
// settled its result. Then it reports, once each, every background result
// that was never delivered — as a SubagentUndelivered through Options.Sink,
// the only record of what those children spent — before the transcript
// closes. Nothing delivers a result after that.
func (s *Session) Close() error {
	s.closeOnce.Do(func() {
		s.subs.closeChildren()
		if done := s.signalClose(); done != nil {
			<-done
		}
		s.subs.closeBackground()
		if err := s.store.Close(); err != nil {
			s.closeErr = fmt.Errorf("harness: %w", s.tools.redactErr(err))
		}
	})
	return s.closeErr
}

// signalClose is the first half of Close, and does not wait (plan 026 §3.8):
// the session is marked closed, so no turn begins from here; a live turn is
// cancelled with tool.ErrClosing; and the tools' closing channel is closed,
// which reaches a call whose turn was already cancelled for another reason.
// It returns the live turn's done channel, which Close waits on, or nil when
// no turn was running — read in the same critical section that marks the
// session closed, so a turn cannot begin between the two (begin checks closed
// under the same lock, and registers its cancel there).
//
// It is idempotent and safe from any goroutine, and takes no lock but s.mu,
// briefly: a parent's Close calls it on each of its children, through their
// registry handles, while the children's own turns may still be running, and
// the runner's deferred Close of each child calls it again.
func (s *Session) signalClose() (done chan struct{}) {
	s.mu.Lock()
	s.closed = true
	cancel, done := s.cancel, s.done
	s.mu.Unlock()
	if cancel != nil {
		cancel(errClosing)
	}
	s.tools.close()
	return done
}
