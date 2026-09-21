package harness

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/charliek/craze/internal/harness/modeltable"
	"github.com/charliek/craze/internal/harness/redact"
	"github.com/charliek/craze/internal/harness/tool"
	"github.com/charliek/craze/internal/harness/tool/opencode"
)

// A session's tools are chosen once, at Open, from its starting model: the
// tool profile — the tools and the system prompt written for them — and
// with it the dispatcher every call goes through, the redactor that keeps
// craze's own keys out of everything a tool shows, and the environment a
// command runs in (plan 019 §3.1, §3.8). This file knows the tool framework
// and not Fantasy; toolbridge.go is the one file that knows both.

// errClosing is the cause Close cancels a running turn with: a tool that
// sees it stops at once, with no grace (tool.ErrClosing).
var errClosing = tool.ErrClosing

// toolSeams are Open's test seams for the tool set, settable only inside the
// package; the zero value is production.
type toolSeams struct {
	// profiles builds the session's profile registry; nil registers the
	// opencode profile alone, which is then everyone's default.
	profiles func() (*tool.Registry, error)
	// gate judges every call before it runs; nil is tool.AllowAll.
	gate tool.Gate
}

// toolset is one session's tools, fixed from Open to Close.
type toolset struct {
	registry *tool.Registry
	// modeGate is the session's gate: the mode's rules over the gate the
	// session would otherwise have used (plan 023 §3.1). Open hands it the
	// plan file's path, which is known only once the store has named the
	// transcript; the modes box (reminders.go) owns it from then on.
	modeGate *tool.ModeGate
	// todos is the session's todo list (todos.go, plan 023 §3.4), reached
	// through the dispatcher's fixed Env.Todos; a turn attaches to it for its
	// own life (turn.go's Run) so Write's one emit lands on the running
	// turn's sink.
	todos *sessionTodos
	// asker is the caller's asker under the session's watch (asker.go), or
	// nil when the session was opened with none.
	asker   *watchedAsker
	profile string      // the profile's name, which the header records
	specs   []tool.Spec // the profile's tools' specs, in the order the model is offered them
	byID    map[string]tool.Spec
	wire    []byte // the tools as the model is offered them (tool.SpecsJSON), for the header's hash
	system  string // the frozen system prompt
	d       *tool.Dispatcher

	// planPath is the session's plan file, which every plan-mode reminder
	// hands the model verbatim (reminders.go). It is held here for one reason:
	// it is a third text this session sends unredacted, beside the system
	// prompt and the tools, so resolve scans it for a key learned later
	// (errFrozenKey) as it scans those. Fixed by adoptPlanPath in Open, before
	// the session is handed out, and read under mu like the keys it is
	// compared against.
	planPath string

	// keys are every provider key the session knows, sorted, and red is the
	// redactor over them, which the turn and the dispatcher read. A session
	// learns a key when a switch to another provider's model resolves one
	// the environment did not have at Open; resolve then prepares the
	// replacer over the larger set and the next turn adopts it, so one turn
	// always uses one redactor and nothing can see the toolset's pointer and
	// the dispatcher's disagree. A key is only ever added, and a Replacer is
	// immutable: they are swapped, never changed.
	mu      sync.Mutex // guards keys and pending
	keys    []string
	pending *redact.Replacer // resolved, waiting for the next turn (adopt)
	red     atomic.Pointer[redact.Replacer]

	// closing is Env.Closing: closed by Close, so a command already
	// cancelled for another reason is killed at once rather than after its
	// grace (plan 019 §3.9, §7.7).
	closing   chan struct{}
	closeOnce sync.Once
}

// openTools builds a session's tools for r, its starting model:
//
//   - the redactor, over every key the table knows of — every provider's,
//     used or not, from the environment and inline (Table.Keys) — so a key
//     too short to redact, or one the marker could print back, fails Open;
//   - the profile ProfileFor picks for r, its specs, its tools array and its
//     system prompt for workspace, with prompt — the caller's instruction
//     documents and catalog — rendered after it (system.go, plan 022 §3.4);
//   - the tools' descriptions redacted, one of which names the environment's
//     temporary directory: it goes to the model with every request, and
//     unlike a tool's output nothing else redacts it. The tools array the
//     header hashes is these same descriptions, encoded once (plan 019 §3.8).
//     The prompt's extras are redacted with that same redactor, and for the
//     same reason: they are assembled out of files on disk, they go out with
//     every request, and nothing downstream looks at them again;
//   - before any of that, a refusal of a workspace whose path holds a
//     provider key (errWorkspaceKey): the prompt names the working directory
//     and the header records it, and neither can hold a key;
//   - the dispatcher, with the mode's gate over the session's own (plan 023
//     §3.1) and with the session's Env: the workspace and home, the
//     redactor, a path-lock table, the closing channel, and the environment
//     a command gets — the user's, less every env_keys variable of every
//     provider and every OPENAI_* (never nil: bash refuses to run on a nil
//     one rather than fall back to craze's own).
//
// It also sweeps the spill directory of files older than seven days; a
// sweep that fails is housekeeping undone, not a reason to refuse a session.
func openTools(home, workspace, mode string, asker tool.Asker, table *modeltable.Table, getenv func(string) string, r modeltable.Resolved, prompt PromptExtras, seams toolSeams) (*toolset, error) {
	keys, err := table.Keys(getenv)
	if err != nil {
		return nil, fmt.Errorf("harness: %w", err)
	}
	vals := make([]string, len(keys))
	for i, k := range keys {
		vals[i] = k.Reveal()
	}
	ts := &toolset{keys: vals, closing: make(chan struct{}), todos: newSessionTodos()}
	slices.Sort(ts.keys)
	ts.red.Store(redact.New(ts.keys...))
	if holdsAKey(workspace, ts.keys) {
		return nil, errWorkspaceKey
	}

	build := seams.profiles
	if build == nil {
		build = defaultProfiles
	}
	if ts.registry, err = build(); err != nil {
		return nil, fmt.Errorf("harness: %w", err)
	}
	p, err := ts.registry.ProfileFor(modelRef(r))
	if err != nil {
		return nil, fmt.Errorf("harness: %w", err)
	}
	ts.profile = p.Name
	ts.byID = make(map[string]tool.Spec, len(p.Tools))
	red := ts.redactor()
	for _, t := range p.Tools {
		s := t.Spec()
		// bash's description names the machine's temporary directory, which
		// comes from the environment. These specs are what the bridge offers
		// the model, so the hash below is of exactly what is sent.
		s.Description = red.String(s.Description)
		// The same canonical form the bridge offers: a number becomes the
		// value it parses as, so a literal written 1.0 is hashed as the 1 that
		// goes out. Without this the digest would describe the profile's
		// spelling rather than the wire.
		s.Parameters = canonicalSchema(s.Parameters)
		ts.specs = append(ts.specs, s)
		ts.byID[s.ID] = s
	}
	// These bytes are what the transcript's header hashes (store.Options.Tools):
	// craze's own tool definitions, as the profile wrote them and this session
	// redacted them — not Fantasy's normalized form of them, which is what
	// actually goes on the wire (schema.Normalize fills in an array's missing
	// "items" and turns a union "type" into "anyOf", agent.go:1131-1137). The
	// opencode profile's schemas use scalar types only, so for it the two are
	// the same bytes; a profile that uses array or union types should hash the
	// normalized form instead, or the digest will describe something slightly
	// different from what was sent.
	if ts.wire, err = tool.SpecsJSON(ts.specs); err != nil {
		return nil, fmt.Errorf("harness: %w", err)
	}
	// The profile's text, and the caller's extras rendered after it and
	// redacted. With no extras this is the profile's text alone, byte for
	// byte, and with them it still begins with it, so every session's
	// requests share that prefix (D-30) — and a key that would have cost
	// either of those two properties refuses the session rather than be
	// redacted out of them (errProfileKey, withPromptExtras). Whatever it
	// leaves in ts.system is covered from here on by resolve's scan for a key
	// learned later (errFrozenKey) and by the transcript header's hash: both
	// read this one field, and neither needed a line of its own for the
	// extras.
	if ts.system, err = withPromptExtras(systemPrompt(p, workspace, runtime.GOOS), prompt, red); err != nil {
		return nil, err
	}

	var keyNames []string
	for _, prov := range table.Providers {
		keyNames = append(keyNames, prov.EnvKeys...)
	}
	// The session's mode wraps the gate it would otherwise use — the test
	// seam's, or AllowAll — rather than replacing it: a call the mode allows
	// is still the inner gate's to judge, which is how H3's evaluator will
	// slot in underneath (plan 023 §3.1).
	ts.modeGate = tool.NewModeGate(mode, seams.gate)
	env := tool.Env{
		Workspace: workspace,
		Home:      home,
		Redactor:  red,
		Environ:   tool.ChildEnviron(os.Environ(), keyNames),
		Locks:     &tool.PathLocks{},
		Closing:   ts.closing,
		Todos:     ts.todos,
	}
	// The caller's asker is wrapped, so the session sees a plan approved
	// (asker.go). With none, Env.Asker stays a nil interface — not a wrapper
	// around nothing — which is what the ask tools test for.
	if asker != nil {
		ts.asker = &watchedAsker{inner: asker}
		env.Asker = ts.asker
	}
	ts.d, err = tool.NewDispatcher(tool.Options{Tools: p.Tools, Gate: ts.modeGate, Env: env})
	if err != nil {
		return nil, fmt.Errorf("harness: %w", err)
	}
	_, _ = tool.Sweep(home, time.Now())
	return ts, nil
}

// The refusals that keep a key out of what a session freezes. None of them
// names the key or where it matched.
//
// They are all one rule. A key can be redacted out of text craze is quoting —
// a tool's description, a document, a catalog row — because a marker there
// still says what the text was for. It cannot be redacted out of something
// whose whole value is being exactly the bytes it is: a path that must open a
// file, a digest that must equal another copy of itself, the prompt prefix
// every request of every session shares. For those the answer is to refuse,
// and to say what to do about it.
var (
	// errWorkspaceKey is Open's refusal of a working directory whose path
	// holds a provider key. Redacting the path instead would put in the
	// prompt, and in the header, something that is neither the directory nor
	// — with a key at its front — an absolute path at all.
	errWorkspaceKey = errors.New("harness: the working directory's path contains a configured provider key; " +
		"start craze from another directory, or change the key")

	// errPlanPathKey is errWorkspaceKey's twin for the session's plan file:
	// Open's refusal of a harness home whose plan path holds a provider key.
	// Every plan-mode reminder hands the model that path so it can write the
	// plan there (§3.3), and a redacted path is one the model cannot open, so
	// the answer is the working directory's — refuse, and say what to do.
	errPlanPathKey = errors.New("harness: the plan file's path contains a configured provider key; " +
		"move the craze directory, or change the key")

	// errFrozenKey is resolve's refusal of a switch whose provider key is in
	// what this session already sends unredacted: the system prompt, anywhere
	// in the encoded tools — both frozen when it opened (D-30) — or the plan
	// file's path, fixed at Open and named in every plan-mode reminder.
	errFrozenKey = errors.New("harness: this model's provider key appears in text this session already sends " +
		"with every request; start a new session, or change the key")

	// errProfileKey is Open's refusal of a system prompt whose profile text a
	// configured key is inside — wholly, or spanning the join with the extras
	// rendered after it. withPromptExtras says why neither can be redacted.
	errProfileKey = errors.New("harness: a configured provider key is inside the system prompt's own text, or spans " +
		"the join between it and the instructions rendered after it; change the key, or remove that provider from the model table")

	// errDigestKey is Open's refusal of a transcript header whose SHA-256 of
	// the frozen prompt, or of the encoded tools, holds a configured key. The
	// header records both verbatim and the adapter's prompt_sources note
	// repeats the prompt's, which C6 requires to be the same string: redacting
	// one copy would break that equality and leave the other, and a digest
	// with a marker in it is not a digest.
	errDigestKey = errors.New("harness: a configured provider key appears in one of this session's transcript-header " +
		"digests; change the key, or remove that provider from the model table")
)

// holdsAKey reports whether text contains any of keys.
func holdsAKey(text string, keys []string) bool {
	return slices.ContainsFunc(keys, func(k string) bool { return strings.Contains(text, k) })
}

// holdsKey is holdsAKey over the keys this session knows of now. Under ts.mu,
// because a switch can add to them (resolve).
func (ts *toolset) holdsKey(text string) bool {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return holdsAKey(text, ts.keys)
}

// adoptPlanPath fixes the session's plan file and refuses a path that holds a
// provider key (errPlanPathKey). Open calls it once, as soon as the store has
// named the transcript the plan file is a sibling of and before the session is
// handed out; from then on the path is only read, by resolve, against keys a
// switch learns later. One critical section, so the check and the path that
// was checked cannot be two different things.
func (ts *toolset) adoptPlanPath(path string) error {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if holdsAKey(path, ts.keys) {
		return errPlanPathKey
	}
	ts.planPath = path
	ts.d.SetPlanPath(path) // what exit_plan_mode reads (tool.Env.PlanPath)
	return nil
}

// redactor is the session's, as it is now. It is never nil.
func (ts *toolset) redactor() *redact.Replacer { return ts.red.Load() }

// widest is the redactor over every key the session knows: the one resolve
// prepared when there is one, and the installed one otherwise. It is never
// nil.
//
// The two differ exactly while a switch has learned a key the environment
// gained since Open and no turn has begun since (resolve, adopt). A caller
// that redacts text and then hands it to Run — Session.Redact's whole reason
// for existing — would otherwise redact with the narrower replacer and then
// watch begin adopt the wider one and send the string unchanged, so the very
// key that turn redacts everything else with would ride out inside the
// prompt. Redacting more than a session strictly needs is never a leak
// (resolve says so of the keys it leaves resolved); redacting less is the
// bug. Under ts.mu rather than off the atomic, because pending and the
// installed pointer have to be read as one fact.
func (ts *toolset) widest() *redact.Replacer {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if ts.pending != nil {
		return ts.pending
	}
	return ts.red.Load()
}

// resolve takes the keys the table resolves now and, when one of them is new
// to the session, prepares the redactor over all of them — the ones it had
// included — for the next turn to adopt. A session learns a key this way
// when a switch makes current a model whose provider's key the environment
// gained since Open; until a session uses it, a value in the environment is
// not craze's credential.
//
// It prepares nothing and refuses when a key cannot be redacted at all
// (modeltable.Keys' floor), and when a new one turns out to be inside what
// this session sends unredacted — the system prompt, the working directory it
// names among it, anywhere in the encoded tools, or the plan file's path,
// which every plan-mode reminder hands the model — none of which it can
// rewrite (errFrozenKey). Both refuse the switch that asked for it.
//
// It never installs: a turn already running must keep the redactor it
// redacted its earlier steps with, or a step persisted later would be
// written with another (adopt). So a switch that fails after this — on the
// session's closing, or on the effort the new model does not have — leaves
// nothing installed. What it does leave is the keys resolved, which the next
// turn adopts anyway: they are keys the table has, and redacting more than a
// session needs is never a leak.
func (ts *toolset) resolve(table *modeltable.Table, getenv func(string) string) error {
	found, err := table.Keys(getenv)
	if err != nil {
		return fmt.Errorf("harness: %w", err)
	}
	ts.mu.Lock()
	defer ts.mu.Unlock()
	var added []string
	for _, k := range found {
		if v := k.Reveal(); !slices.Contains(ts.keys, v) && !slices.Contains(added, v) {
			added = append(added, v)
		}
	}
	if len(added) == 0 {
		return nil
	}
	// The whole tools payload, not the descriptions alone: a tool's name, a
	// parameter's name and a schema's own strings all go out with it. The plan
	// path is the third: it reaches the model verbatim in every plan-mode
	// reminder and cannot be redacted without becoming a path that opens
	// nothing, so a key found inside it refuses the switch exactly as one
	// inside the working directory does — that one through the prompt, which
	// names it (plan 023 §3.3).
	if holdsAKey(ts.system, added) || holdsAKey(string(ts.wire), added) || holdsAKey(ts.planPath, added) {
		return errFrozenKey
	}
	ts.keys = append(ts.keys, added...)
	slices.Sort(ts.keys)
	ts.pending = redact.New(ts.keys...)
	return nil
}

// adopt installs a redactor resolve prepared, in the toolset and in the
// dispatcher, and reports whether it installed one. Run calls it as a turn
// begins — before the turn's first request, and with no turn running, since
// the session admits one at a time — so a turn uses exactly one redactor
// from its first step to its last, and the two pointers are never seen
// disagreeing. A call still running from an earlier turn keeps the Env it
// was given (Dispatcher.SetRedactor).
func (ts *toolset) adopt() bool {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if ts.pending == nil {
		return false
	}
	red := ts.pending
	ts.pending = nil
	ts.red.Store(red)
	ts.d.SetRedactor(red)
	return true
}

// redactedError is err with a redacted message. It unwraps to err, so
// errors.Is and errors.As still see what it was, but every printed form of
// it — the adapter's error line, `craze prompt --json` — is redacted.
type redactedError struct {
	msg string
	err error
}

func (e *redactedError) Error() string { return e.msg }
func (e *redactedError) Unwrap() error { return e.err }

// redactErr returns err with every provider key gone from its message. The
// store's errors name the file they could not write, under a home craze was
// configured with, and a path can hold anything (plan 019 §3.8). An error
// whose message holds no key is returned as it is; nil stays nil.
func (ts *toolset) redactErr(err error) error {
	if err == nil {
		return nil
	}
	text := err.Error()
	msg := ts.redactor().String(text)
	if msg == text {
		return err
	}
	return &redactedError{msg: msg, err: err}
}

// defaultProfiles is the registry a session gets: the opencode profile,
// built fresh — its tools are the session's own, so grep and glob share one
// ripgrep lookup per session — and so the default for every model.
func defaultProfiles() (*tool.Registry, error) {
	p, err := opencode.Profile()
	if err != nil {
		return nil, err
	}
	var reg tool.Registry
	if err := reg.Register(p); err != nil {
		return nil, err
	}
	return &reg, nil
}

// modelRef is how ProfileFor sees a resolved model.
func modelRef(r modeltable.Resolved) tool.ModelRef {
	return tool.ModelRef{Provider: r.ProviderID, Alias: r.Alias, WireModel: r.WireModel, Profile: r.ToolProfile}
}

// check refuses a switch to r unless r's model gets the session's profile:
// the tools and the system prompt are the session's, fixed at Open.
func (ts *toolset) check(r modeltable.Resolved) error {
	p, err := ts.registry.ProfileFor(modelRef(r))
	switch {
	case err != nil:
		return fmt.Errorf("harness: model %q: %w", r.Alias, err)
	case p.Name != ts.profile:
		return fmt.Errorf("%w (model %q has profile %q, the session %q)", ErrProfileMismatch, r.Alias, p.Name, ts.profile)
	}
	return nil
}

// close closes the closing channel, once.
func (ts *toolset) close() { ts.closeOnce.Do(func() { close(ts.closing) }) }
