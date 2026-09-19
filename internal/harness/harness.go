// Package harness is craze's native agent harness: a Session holds one
// conversation with a model from the harness's own model table and runs it a
// turn at a time on Fantasy's agent loop (plan 018 §3.7). A turn streams its
// text and thinking to a sink as they arrive, is written to the session's
// transcript from Fantasy's callbacks (package store), and ends in a stop
// reason or in one of a small set of errors the adapter can phrase
// (errors.go).
//
// H1 gives the model no tools, so a turn is one model step, and the system
// prompt says so (system.go).
//
// The harness imports nothing from the rest of craze but internal/atomicfile
// (through modeltable): its directory, workspace, model table and craze's
// version all come in through Options, from the native adapter in
// internal/agent (plan 018 §3.1).
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
// # Concurrency
//
// One Run at a time (ErrInTurn otherwise). Every other method may be called
// from any goroutine, and all of them but Close from inside Run's sink too:
// the session never holds its lock while calling the sink or the model.
// Close cancels a live Run and waits for it to return, so it must not be
// called from the sink, which runs on that Run and would wait for itself.
package harness

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"runtime"
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
	// Effort is the effort the session starts at; "" is the model's
	// default_effort (none, for a model with no effort control).
	Effort string
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
	system   string // the frozen system prompt
	store    *store.Store
	getenv   func(string) string
	newModel func(modeltable.Resolved) (fantasy.LanguageModel, error)

	closeOnce sync.Once
	closeErr  error

	mu      sync.Mutex
	table   *modeltable.Table
	cur     model  // what the next turn runs on; Current reports it
	logged  logged // what the transcript was last told the model and effort are
	closed  bool
	running bool
	cancel  context.CancelFunc // the live turn's; nil when idle
	done    chan struct{}      // closed when the live turn has returned
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

// logged is the model and effort the transcript last had a turn or a
// change entry for (held or written). A turn that runs on anything else
// first appends a change entry.
type logged struct {
	model  store.Model
	effort string
}

// Open starts a session: it resolves and builds the starting model, and
// builds the system prompt. It does no I/O — the transcript appears with the
// first turn that produces output — so a session closed before that leaves
// nothing behind. A starting model whose provider has no key is ErrNoAPIKey;
// the caller decides whether to fall back to another model (plan 018 §3.8).
func Open(opts Options) (*Session, error) {
	if opts.Table == nil {
		return nil, errors.New("harness: no model table")
	}
	s := &Session{
		getenv:   opts.Getenv,
		newModel: opts.NewModel,
		table:    opts.Table,
	}
	if s.getenv == nil {
		s.getenv = os.Getenv
	}
	if s.newModel == nil {
		s.newModel = func(r modeltable.Resolved) (fantasy.LanguageModel, error) { return llm.New(r) }
	}
	s.system = systemPrompt(filepath.Clean(opts.Workspace), runtime.GOOS)

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
	st, err := store.New(store.Options{
		Home:         opts.Home,
		Workspace:    opts.Workspace,
		CrazeVersion: opts.Version,
		SystemPrompt: s.system,
		Now:          opts.Now,
	})
	if err != nil {
		return nil, fmt.Errorf("harness: %w", err)
	}
	s.store = st
	s.cur = m
	s.logged = logged{model: m.id(), effort: m.effort}
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

// Close ends the session: it cancels a live Run and waits for it to return —
// by which time a partial answer has been persisted, interrupted — then
// closes the transcript. It is idempotent and safe from any goroutine,
// concurrently with a Run that is finishing on its own; a second Close waits
// for the first and returns its result. Every Run, SetModel and SetEffort
// after it is ErrClosed. Close must not be called from inside Run's sink,
// which runs on the turn it would wait for.
func (s *Session) Close() error {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		cancel, done := s.cancel, s.done
		s.mu.Unlock()
		if cancel != nil {
			cancel()
			<-done
		}
		if err := s.store.Close(); err != nil {
			s.closeErr = fmt.Errorf("harness: %w", err)
		}
	})
	return s.closeErr
}
