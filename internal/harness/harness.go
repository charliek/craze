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
	Model string
	// Prompt is what the caller adds to the frozen system prompt: the
	// instruction documents this workspace's user and project wrote, and the
	// catalog of the skills and commands installed for it (plan 022 §3.4).
	// The harness renders and redacts it once, in Open, and reads no file for
	// it: the adapter in internal/agent resolves it first and hands it over
	// as data. The zero value sends the tool profile's text alone.
	Prompt PromptExtras
	// Effort is the effort the session starts at; "" is the model's
	// default_effort (none, for a model with no effort control).
	Effort string
	// Mode is the mode the session starts in: "" or "agent" (implement),
	// "plan" or "ask". Anything else is ErrUnknownMode and refuses Open. A
	// session opened in plan mode creates its plan file and tells the model
	// at its first step (reminders.go, plan 023 §3.1).
	Mode string
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
	system   string // the frozen system prompt, the tool profile's
	store    *store.Store
	tools    *toolset // fixed at Open
	getenv   func(string) string
	newModel func(modeltable.Resolved) (fantasy.LanguageModel, error)
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

	mu      sync.Mutex
	table   *modeltable.Table
	cur     model  // what the next turn runs on; Current reports it
	logged  logged // what the transcript was last told the model, effort and mode are
	closed  bool
	running bool
	turns   int                     // turns begun, which number their tool calls' ids
	cancel  context.CancelCauseFunc // the live turn's; nil when idle
	done    chan struct{}           // closed when the live turn has returned
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
func Open(opts Options) (*Session, error) {
	if opts.Table == nil {
		return nil, errors.New("harness: no model table")
	}
	s := &Session{
		getenv:   opts.Getenv,
		newModel: opts.NewModel,
		newAgent: defaultAgent,
		table:    opts.Table,
	}
	if s.getenv == nil {
		s.getenv = os.Getenv
	}
	if s.newModel == nil {
		s.newModel = func(r modeltable.Resolved) (fantasy.LanguageModel, error) { return llm.New(r) }
	}

	mode, err := normalizeMode(opts.Mode)
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
	if s.tools, err = openTools(opts.Home, filepath.Clean(opts.Workspace), mode, opts.Table, s.getenv, m.r, opts.Prompt, opts.tools); err != nil {
		return nil, err
	}
	s.system = s.tools.system
	st, err := store.New(store.Options{
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
	})
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
	s.store = st
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
func (s *Session) SetMode(id string) error {
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
	s.modes.set(mode)
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
// out: its size in bytes, and the digest the transcript's header already
// records for it (store.New). The adapter writes both into the journal note
// that says which files went into the prompt (plan 022 §3.4), and it is the
// header's own value rather than a second SHA-256 of the same string, so a
// note and a transcript can never disagree about which prompt a session sent.
// The text itself stays unexported: it is never stored, and a caller that
// could read it back would be a caller that could log it.
//
// Both are fixed at Open and take no lock.
func (s *Session) PromptSize() int      { return len(s.system) }
func (s *Session) PromptSHA256() string { return s.store.Header().SystemPromptSHA256 }

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
func (s *Session) Redact(text string) string { return s.tools.widest().String(text) }

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
func (s *Session) Close() error {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		cancel, done := s.cancel, s.done
		s.mu.Unlock()
		if cancel != nil {
			cancel(errClosing)
		}
		s.tools.close()
		if cancel != nil {
			<-done
		}
		if err := s.store.Close(); err != nil {
			s.closeErr = fmt.Errorf("harness: %w", s.tools.redactErr(err))
		}
	})
	return s.closeErr
}
