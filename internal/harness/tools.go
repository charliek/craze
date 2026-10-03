package harness

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	// turn's sink. nil for a sub-agent, which has no todo_write (plan 026
	// §3.2).
	todos *sessionTodos
	// asker is the caller's asker under the session's watch (asker.go), or
	// nil when the session was opened with none.
	asker   *watchedAsker
	profile string      // the profile's name, which the header records
	specs   []tool.Spec // the profile's tools' specs, in the order the model is offered them
	byID    map[string]tool.Spec
	wire    []byte // the tools as the model is offered them (tool.SpecsJSON), for the header's hash; nil when there are none
	system  string // the frozen system prompt
	// startSection is the session-start section as system ends with it
	// (withSnapshot), "" when it has none. A sub-agent on its parent's profile
	// leaves it "": its section is inside the parent's bytes, which it
	// inherits whole. Fixed at Open.
	startSection string
	d            *tool.Dispatcher
	// locks is Env.Locks: the session's own path-lock table, or, for a
	// sub-agent, its parent's, so a parent's and its children's edits of one
	// file serialize (plan 026 §3.2). Fixed at Open.
	locks *tool.PathLocks
	// types are the agent types the session's agent tool offers: the
	// caller's personas and the built-ins merged in precedence order, each
	// name once (agentTypes, plan 026 §3.4). The tool's description lists them
	// and a call's subagent_type resolves against them — one list, so the type
	// a call gets is the one the model was shown. nil for a sub-agent, which
	// has no agent tool. Fixed at Open.
	types []tool.Persona
	// offered are the profile's tools a child may be given, in the profile's
	// order: what a type's tools resolve out of (childToolSet), both in the
	// description and for the child the runner opens, so the tools a type is
	// listed with are the tools its child gets. nil for a sub-agent. Fixed at
	// Open.
	offered []string

	// planPath is the session's plan file, which every plan-mode reminder
	// hands the model verbatim (reminders.go). It is held here for one reason:
	// it is a third text this session sends unredacted, beside the system
	// prompt and the tools, so resolve and learn scan it for a key learned
	// later (errFrozenKey, ErrStoredKeyFrozen) as they scan those. Fixed by
	// adoptPlanPath in Open, before the session is handed out, and read under
	// mu like the keys it is compared against.
	planPath string

	// keys are every provider key the session knows, sorted, and red is the
	// redactor over them, which the turn and the dispatcher read. A session
	// learns a key in two ways: when a switch to another provider's model
	// resolves one the environment did not have at Open (resolve), and when
	// the adapter hands it one stored in providers.toml since Open (learn,
	// plan 031 §3.8). Either prepares the replacer over the larger set and
	// the next turn adopts it, so one turn always uses one redactor and
	// nothing can see the toolset's pointer and the dispatcher's disagree. A
	// key is only ever added, and a Replacer is immutable: they are swapped,
	// never changed.
	mu      sync.Mutex // guards keys, pending, pendingKeys, installed, streams, learned, and refusing's writes
	keys    []string
	pending *redact.Replacer // resolved, waiting for the next turn (adopt)
	red     atomic.Pointer[redact.Replacer]
	// installed are the keys red redacts, sorted, and pendingKeys the ones
	// pending does: what addSecrets widens the installed redactor from, since
	// a Replacer does not expose its keys. installed is ts.keys but for a
	// switch's or a stored key's that the next turn adopts.
	installed, pendingKeys []string

	// streams are the output streams of the commands running in this
	// session's bash calls and jobs, as the bash tool tracks them
	// (tool.Jobs.Track, plan 033 C10r, review r7 finding 9), by a number of
	// track's: each is widened to the session's widest key set when it is
	// tracked, and again by every extend, under mu — so a stream tracked
	// while a key is learned is widened by one or the other, never missed.
	// Only a session that runs jobs has any (Env.Jobs). The lock order is
	// mu, then a stream's own lock, a leaf held for one write's memory work
	// (opencode's modelStream).
	streams    map[uint64]tool.KeyedStream
	nextStream uint64

	// learned is every stored key learn has accepted, in the order it first
	// did, each once — the ones the session already knew included, since a
	// key it knew from the environment is not one a sub-agent opened later
	// would find there again. A child the runner opens from here on starts
	// with them (ChildOptions.learned, r2-1); nothing else reads them.
	learned []string
	// refusing is the refusal state learn puts the session in (r2-2,
	// ErrStoredKeyFrozen): set, under mu, in the one section that found a
	// stored key inside a frozen surface, and never cleared. begin reads it
	// under mu, in the section that adopts the redactor, so a turn either
	// began before the key was learned — and runs as a turn already running
	// does (R1) — or is refused. HasPending and BackgroundOwed read it
	// without the lock: an atomic, so neither takes a lock the adapter's
	// wake worker does not already take under its own.
	refusing atomic.Bool

	// environ is the environment a command the session starts gets
	// (Env.Environ): the user's as it was at Open, less every variable in
	// envNames and every OPENAI_* (tool.ChildEnviron). envNames is every
	// variable a model table the session has held knows holds a key — the
	// one it opened with, and each SetTable handed it since (plan 034 §3.4,
	// A23) — sorted: names are only ever added, so a variable a provider
	// configured mid-session declares never reaches a command once the
	// session can send it, and one a table no longer names is still kept
	// out. narrowEnviron grows both, under mu, and hands the dispatcher the
	// new environment; a sub-agent starts with its parent's names
	// (ChildOptions.dropEnv).
	environ  []string
	envNames []string

	// closing is Env.Closing: closed by Close, so a command already
	// cancelled for another reason is killed at once rather than after its
	// grace (plan 019 §3.9, §7.7).
	closing   chan struct{}
	closeOnce sync.Once
}

// openTools builds a session's tools for r, its starting model:
//
//   - the redactor, over every key the table knows of — every provider's,
//     used or not, from the environment and inline (Table.Keys) — so an
//     inline key too short to redact, or one the marker could print back,
//     fails Open (an env value like that is no key, and skipped: plan 031
//     §3.2);
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
//     a command gets — the user's, less every variable the table knows holds
//     a key (CredentialEnvNames: every provider's env_keys, and a shipped
//     name no provider takes its key from any more, plan 031 C2r2) and every
//     OPENAI_* (never nil: bash refuses to run on a nil one rather than fall
//     back to craze's own).
//
// It also sweeps the spill directory of files older than seven days; a
// sweep that fails is housekeeping undone, not a reason to refuse a session.
//
// start is what the prompt says of the session's start (sessionStart): the
// snapshot and the model line withSnapshot renders after the extras, and a
// sub-agent's own model, which its role section names.
//
// child is non-nil for a sub-agent (child.go, plan 026 §3.2), and changes
// six things: a home whose path holds a key it knows is refused
// (errChildHomeKey); the profile's tools are filtered before anything is
// built from them (ChildOptions.keeps); the system prompt is the parent's
// frozen string, or the child's own under another profile with the parent's
// session-start section, with the role section after it (withChildRole); the
// gate is a child's
// (tool.NewChildModeGate); the path-lock table is the parent's; and there is
// no todo list and no sweep — the parent's Open swept, and a fan-out would
// otherwise walk the directory once per child.
//
// A session that is not a child merges personas with the built-in agent types
// (agentTypes), and, when its profile has the agent tool, offers it with this
// session's types and models after its description (describedTool, plan 026
// §3.3); a child has no agent tool, and personas are not read for it. subs is
// the session's sub-agent runner, which the agent tool hands its calls to
// (Env.Subagents); nil for a child, whose Env.Subagents stays a nil interface.
//
// A session that runs no background jobs — every child, and one whose runner
// is not background (headless) — is offered each tool.JobsAware tool's
// variant in its place, or nothing where it has none: no jobs surface at all
// (plan 033 X101).
//
// ref is how the profile is chosen: a new session's starting model
// (modelRef), or, for a resumed one, the profile its transcript's header names
// and nothing else — the tools and the prompt are that profile's whichever
// model the session resumes on, and a model with another profile is not one
// it may resume on (plan 028 §3.3). The mode only seeds the gate: a resumed
// session learns its own from the transcript and sets it (newModes) before it
// is handed out.
func openTools(home, workspace, mode string, asker tool.Asker, table *modeltable.Table, getenv func(string) string, ref tool.ModelRef, prompt PromptExtras, start sessionStart, personas []tool.Persona, child *ChildOptions, subs *subagents, seams toolSeams) (*toolset, error) {
	keys, err := table.Keys(getenv)
	if err != nil {
		return nil, fmt.Errorf("harness: %w", err)
	}
	vals := make([]string, len(keys))
	for i, k := range keys {
		vals[i] = k.Reveal()
	}
	// A sub-agent opened after its parent learned stored keys starts with
	// them (plan 031 §3.8, r2-1): they are in no table and no environment, so
	// the child's own Keys cannot find them, and everything below — the
	// redactor its prompt, tools, header and spill files go through, and the
	// refusals of a key its home or its prompt holds — is built from vals.
	// The runner took them valid (learn) and never kept one the parent had
	// not judged.
	if child != nil {
		for _, k := range child.learned {
			if !slices.Contains(vals, k) {
				vals = append(vals, k)
			}
		}
	}
	ts := &toolset{keys: vals, closing: make(chan struct{})}
	if child == nil {
		ts.todos = newSessionTodos()
	}
	slices.Sort(ts.keys)
	ts.installed = slices.Clone(ts.keys)
	ts.red.Store(redact.New(ts.keys...))
	if holdsAKey(workspace, ts.keys) {
		return nil, errWorkspaceKey
	}
	// A sub-agent's home begins every spill path of its calls and of the
	// runner's cut of its answer (review r4). Each is redacted where it is
	// added — the child's by its own keys, the runner's by both sessions'
	// (review r6) — and a redacted path opens nothing, so a key in it refuses
	// the child, as the parent refuses its own through its plan path, the same
	// home's (adoptPlanPath, resolve). The raw home and the spill directory as
	// joined, so a key spanning the join is refused too.
	if child != nil && (holdsAKey(home, ts.keys) || holdsAKey(filepath.Join(home, tool.SpillDir), ts.keys)) {
		return nil, errChildHomeKey
	}

	build := seams.profiles
	if build == nil {
		build = defaultProfiles
	}
	if ts.registry, err = build(); err != nil {
		return nil, fmt.Errorf("harness: %w", err)
	}
	p, err := ts.registry.ProfileFor(ref)
	if err != nil {
		return nil, fmt.Errorf("harness: %w", err)
	}
	ts.profile = p.Name
	ts.byID = make(map[string]tool.Spec, len(p.Tools))
	red := ts.redactor()
	// The agent types and the tools a child may be given (toolset.types,
	// toolset.offered): only a session that can start children has either.
	if child == nil {
		ts.types = agentTypes(personas)
		every := &ChildOptions{AllTools: true}
		for _, t := range p.Tools {
			if id := t.Spec().ID; every.keeps(id) {
				ts.offered = append(ts.offered, id)
			}
		}
	}
	// Whether the session runs background jobs (plan 033 §3.8, P11): only one
	// that runs background work at all (Options.Background — an interactive
	// one, which something wakes) and is not a sub-agent, whose subs is nil.
	// It is fixed here, before a spec is read, since it decides what bash and
	// its job tools offer (tool.JobsAware, X101), and the env below has Jobs
	// by the same value — what the model is offered and what runs never
	// disagree.
	runsJobs := subs != nil && subs.background
	// tools are the profile's tools this session offers: all of them, or a
	// child's filtered set, each as the session offers it — a session that
	// runs no jobs gets a JobsAware tool's variant in its place, or nothing
	// (bash without run_in_background, no bash_output or bash_stop: X101). The
	// same list feeds the specs below and the dispatcher, so a tool the filter
	// drops is neither offered nor run.
	var tools []tool.Tool
	for _, t := range p.Tools {
		s := t.Spec()
		if !child.keeps(s.ID) {
			continue
		}
		if j, ok := t.(tool.JobsAware); ok && !runsJobs {
			if t = j.WithoutJobs(); t == nil {
				continue
			}
			s = t.Spec()
		}
		// The agent tool is offered with this session's agent types and models
		// after its own description (plan 026 §3.3). It is wrapped before its
		// spec is read for anything else, so the spec offered, the tools array
		// the header hashes and the dispatcher's tool are the one decorated
		// tool. A child never gets here: keeps withheld the tool from it.
		if s.ID == tool.AgentTool {
			t = &describedTool{Tool: t, tail: agentTail(ts.types, ts.offered, table, getenv, red)}
			s = t.Spec()
		}
		tools = append(tools, t)
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
	//
	// A text-only child has no tools at all, and its requests carry no tools
	// array — every provider driver omits an empty one — so it has no bytes to
	// hash either: wire stays nil, and its header records the profile its
	// prompt was written for and no tools_sha256, the store's rule for a
	// session with no tools (plan 026 §3.2).
	if child == nil || len(ts.specs) > 0 {
		if ts.wire, err = tool.SpecsJSON(ts.specs); err != nil {
			return nil, fmt.Errorf("harness: %w", err)
		}
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
	//
	// The session-start section goes last (withSnapshot, D-67): it is the one
	// part that varies between sessions, so everything before it stays the
	// prefix every session on the workspace shares. The same checks cover it —
	// the scan for a key learned later reads ts.system, and the header hashes
	// it.
	//
	// A sub-agent whose model resolves the profile its parent's prompt was
	// written for is handed that prompt, frozen, and adds only its role: the
	// parent's bytes are its prefix by construction, where re-rendering the
	// extras under this child's redactor could differ from them if a key had
	// appeared between the two Opens (plan 026 §3.2, panel GLM 12). One on
	// another profile renders its own from the clone of the extras it was
	// given and from its parent's snapshot, with its parent's model line —
	// the runner hands it both (childOpenOptions) — and adds its role to that,
	// so no child's prompt lacks the section its parent's has.
	if child != nil && child.BaseSystem != "" && child.BaseProfile == p.Name {
		ts.system, err = withChildRole(child.BaseSystem, child.Role, start.own, red)
	} else {
		var base string
		base, err = withPromptExtras(systemPrompt(p, workspace, runtime.GOOS, opencode.Shell()), prompt, red)
		if err == nil {
			ts.system, err = withSnapshot(base, start.snap, start.top, red)
		}
		if err == nil && len(ts.system) > len(base) {
			ts.startSection = ts.system[len(base)+1:] // after the blank line's newline
		}
		if err == nil && child != nil {
			ts.system, err = withChildRole(ts.system, child.Role, start.own, red)
		}
	}
	if err != nil {
		return nil, err
	}

	// Every variable the table knows holds a key, and for a sub-agent every
	// one its parent's tables did (ChildOptions.dropEnv, plan 034 §3.4): a
	// child opens on its parent's current table, which may no longer name one
	// the parent keeps out of its own commands.
	keyNames := table.CredentialEnvNames()
	if child != nil {
		keyNames = mergeNames(keyNames, child.dropEnv)
	}
	// The session's mode wraps the gate it would otherwise use — the test
	// seam's, or AllowAll — rather than replacing it: a call the mode allows
	// is still the inner gate's to judge, which is how H3's evaluator will
	// slot in underneath (plan 023 §3.1). A child's is the same gate over the
	// same inner one, with its parent's pushed strictness (plan 026 §3.5).
	if child != nil {
		ts.modeGate = tool.NewChildModeGate(mode, child.Strictness, seams.gate)
	} else {
		ts.modeGate = tool.NewModeGate(mode, seams.gate)
	}
	ts.locks = &tool.PathLocks{}
	if child != nil && child.Locks != nil {
		ts.locks = child.Locks
	}
	env := tool.Env{
		Workspace: workspace,
		Home:      home,
		Redactor:  red,
		Environ:   tool.ChildEnviron(os.Environ(), keyNames),
		Locks:     ts.locks,
		Closing:   ts.closing,
		// Every session's commands are tracked (plan 033 C14r, r12 #6a),
		// a child's and a headless one's too: a token minted while one runs
		// reaches its stream through extend, as a job's does (X91).
		Streams: streams{ts},
	}
	// Set only when there is one, so a child's Env.Todos is a nil interface
	// rather than an interface around a nil store: todo_write tests the
	// former.
	if ts.todos != nil {
		env.Todos = ts.todos
	}
	// The caller's asker is wrapped, so the session sees a plan approved
	// (asker.go). With none, Env.Asker stays a nil interface — not a wrapper
	// around nothing — which is what the ask tools test for. Open hands a
	// child none.
	if asker != nil {
		ts.asker = &watchedAsker{inner: asker}
		env.Asker = ts.asker
	}
	// The same rule for the runner: a child has none, and its agent tool —
	// which it is never offered anyway — would read a nil interface and say
	// sub-agents are not available (plan 026 §3.2).
	if subs != nil {
		env.Subagents = subs
	}
	// And for the jobs (runsJobs above). Anywhere else a timeout kills, as
	// D-59 has it for background sub-agents, and a run_in_background the
	// model sends though its bash does not offer it runs in the foreground.
	if runsJobs {
		env.Jobs = jobs{r: subs}
	}
	ts.environ, ts.envNames = env.Environ, keyNames
	ts.d, err = tool.NewDispatcher(tool.Options{Tools: tools, Gate: ts.modeGate, Env: env})
	if err != nil {
		return nil, fmt.Errorf("harness: %w", err)
	}
	if child == nil {
		_, _ = tool.Sweep(home, time.Now())
	}
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

	// errChildHomeKey is errPlanPathKey's twin for a sub-agent: its Open's
	// refusal of a harness home whose path holds a key the child knows (review
	// r4). The home begins the path of every spill file the child's calls
	// write and of the one the runner's cut of its answer writes; each path is
	// redacted where it is added (review r6), and a redacted path is one the
	// model cannot open, so the answer is the plan path's — refuse, and say
	// what to do; the parent refuses a key in the same home through its plan
	// path. The runner reports it to the parent's model as a failed sub-agent.
	errChildHomeKey = errors.New("harness: the path of the directory sub-agents save full tool output in contains " +
		"a configured provider key; move the craze directory, or change the key")

	// errFrozenKey is resolve's refusal of a switch whose provider key is in
	// what this session already sends unredacted: the system prompt, anywhere
	// in the encoded tools — both frozen when it opened (D-30) — or the plan
	// file's path, fixed at Open and named in every plan-mode reminder.
	errFrozenKey = errors.New("harness: this model's provider key appears in text this session already sends " +
		"with every request; start a new session, or change the key")

	// errProfileKey is Open's refusal of a system prompt whose profile text a
	// configured key is inside — wholly, or spanning the join with the extras
	// rendered after it, or with the session-start section after those.
	// withPromptExtras and withSnapshot say why none of them can be redacted.
	errProfileKey = errors.New("harness: a configured provider key is inside the system prompt's own text, or spans " +
		"the join between it and the instructions or session-start section rendered after it; change the key, or remove that provider from the model table")

	// errSnapshotKey is Open's refusal of a session-start section the final
	// redaction would break: one where a key is spelled across craze's own
	// framing — the heading, the sentence before the git block, a fence — so
	// that redacting it would leave quoted git text outside its quotation, or
	// where the markers of keys spelled across the joins between fields grow it
	// past its bound with no status left to give back. withSnapshot says why
	// neither can be redacted.
	errSnapshotKey = errors.New("harness: a configured provider key spans the framing of the session-start section, " +
		"or grows it past its bound when redacted; change the key, or remove that provider from the model table")

	// errDigestKey is Open's refusal of a transcript header whose SHA-256 of
	// the frozen prompt, or of the encoded tools, holds a configured key. The
	// header records both verbatim and the adapter's prompt_sources note
	// repeats the prompt's, which C6 requires to be the same string: redacting
	// one copy would break that equality and leave the other, and a digest
	// with a marker in it is not a digest.
	errDigestKey = errors.New("harness: a configured provider key appears in one of this session's transcript-header " +
		"digests; change the key, or remove that provider from the model table")

	// errHeaderKey is Open's refusal of a transcript header whose encoded line
	// holds a configured key that no single field does: one spelled across the
	// JSON between two fields — a sub-agent's type and its persona's path, say,
	// each of which a plugin or a repository wrote. The fields were redacted
	// one at a time, and the line is what goes to disk.
	errHeaderKey = errors.New("harness: a configured provider key appears in this session's transcript header, " +
		"spanning two of its fields; change the key, or the file or directory names it spans")

	// errChildPromptKey is a sub-agent's Open refusing a system prompt that a
	// configured key is inside: in the parent's frozen prompt it was handed —
	// a key the parent did not know when it froze it — or spanning a join the
	// role section made. withChildRole says why neither can be redacted. The
	// runner reports it to the parent's model as a failed sub-agent (plan 026
	// §3.2).
	errChildPromptKey = errors.New("harness: a configured provider key is inside the sub-agent's system prompt — in the " +
		"parent's prompt it starts from, or spanning the join with its role; change the key, or remove that provider from the model table")
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
	return ts.widestLocked()
}

// widestLocked is widest under ts.mu.
func (ts *toolset) widestLocked() *redact.Replacer {
	if ts.pending != nil {
		return ts.pending
	}
	return ts.red.Load()
}

// track is tool.Jobs.Track (jobs.go) and tool.Env.Streams' Track (streams):
// s registered, and widened to every key
// the session knows now — the redactor its call was given may be an earlier
// turn's, or narrower than one a switch has prepared — in one section with the
// registration, so a key learned meanwhile reaches s through this widening or
// through extend's. untrack forgets it; a second call does nothing.
//
// It covers the keys the session itself knows (widest), not those only one of
// its sub-agents knows (Session.Redact's wider set: a key a child found in the
// environment as it opened, which the session never switched to). Every text
// a job gives the model goes through that wider set as it is made (union,
// jobs.go), so such a key is caught there whole; one a job printed split
// across two reads is not — a residual of plan 033 C10r's, as narrow as a key
// the environment gained mid-session that only a sub-agent ever used.
func (ts *toolset) track(s tool.KeyedStream) (untrack func()) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if ts.streams == nil {
		ts.streams = map[uint64]tool.KeyedStream{}
	}
	ts.nextStream++
	id := ts.nextStream
	ts.streams[id] = s
	s.Widen(ts.widestLocked())
	return func() {
		ts.mu.Lock()
		defer ts.mu.Unlock()
		delete(ts.streams, id)
	}
}

// knownKeys are the keys widest redacts — every key the session knows — as a
// copy the caller owns. The sub-agent runner builds one replacer over its
// parent's and its child's together from them, at each use (review r3, r4): a
// Replacer does not expose its keys, and two replacers run one after the
// other are not one over the union (subagents.union says why).
func (ts *toolset) knownKeys() []string {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return slices.Clone(ts.keys)
}

// resolve takes the keys the table resolves now and, when one of them is new
// to the session, prepares the redactor over all of them — the ones it had
// included — for the next turn to adopt. A session learns a key this way
// when a switch makes current a model whose provider's key the environment
// gained since Open; until a session uses it, a value in the environment is
// not craze's credential.
//
// It prepares nothing and refuses when an inline key cannot be redacted at
// all (modeltable.Keys' floor; an env value that fails it is skipped, plan
// 031 §3.2), and when a new one turns out to be inside what
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
	if ts.frozenHolds(added) {
		return errFrozenKey
	}
	ts.extend(added)
	return nil
}

// holdsFrozen is frozenHolds for one value, under ts.mu (Session.FrozenKey).
func (ts *toolset) holdsFrozen(key string) bool {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return ts.frozenHolds([]string{key})
}

// narrowEnviron adds names to the variables every command the session starts
// from now on is started without (plan 034 §3.4, A23): the environment is
// filtered again (tool.ChildEnviron, which only ever removes) and handed to
// the dispatcher, which gives it to every call from now on — a call already
// running keeps the one it began with. A name already kept out changes
// nothing: environ never holds one of envNames, so only the new names remove
// anything. Under ts.mu, a leaf.
func (ts *toolset) narrowEnviron(names []string) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	merged := mergeNames(ts.envNames, names)
	if len(merged) == len(ts.envNames) {
		return // envNames is sorted and unique: none of names is new
	}
	ts.envNames = merged
	ts.environ = tool.ChildEnviron(ts.environ, names)
	ts.d.SetEnviron(ts.environ)
}

// credentialNames are envNames, a copy the caller owns: what a sub-agent
// opened now starts without (ChildOptions.dropEnv).
func (ts *toolset) credentialNames() []string {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return slices.Clone(ts.envNames)
}

// mergeNames is a and b together, sorted and without repeats, in a new slice.
func mergeNames(a, b []string) []string {
	out := append(slices.Clone(a), b...)
	slices.Sort(out)
	return slices.Compact(out)
}

// frozenHolds reports whether one of keys is inside what this session sends
// unredacted with every request and cannot rewrite: the system prompt, the
// whole tools payload — not the descriptions alone: a tool's name, a
// parameter's name and a schema's own strings all go out with it — and the
// plan path, the third: it reaches the model verbatim in every plan-mode
// reminder and cannot be redacted without becoming a path that opens nothing,
// so a key found inside it counts exactly as one inside the working directory
// does — that one through the prompt, which names it (plan 023 §3.3). resolve
// refuses a switch on it (errFrozenKey); learn enters the refusal state
// (ErrStoredKeyFrozen, plan 031 §3.8). Under ts.mu, which the plan path is
// read under.
func (ts *toolset) frozenHolds(keys []string) bool {
	return holdsAKey(ts.system, keys) || holdsAKey(string(ts.wire), keys) || holdsAKey(ts.planPath, keys)
}

// extend adds keys new to the session — added, which resolve or learn found
// in none of ts.keys — and prepares the redactor over all of them for the
// next turn to adopt; it never installs one. Under ts.mu.
//
// It is the one place the session's key set grows, and so where every
// tracked stream — a running command's, a background job's above all — is
// widened with it at once, not at the next turn (track; plan 033 C10r): a job
// prints for longer than any turn lasts, and what its stream passes on
// unredacted reaches bash_output's reads and the spill file in pieces no
// whole-text pass matches. Whatever else grows the set — ChatGPT's tokens as
// they are minted (AddSecrets, plan 033 §3.12) — comes through here too, and
// so reaches the streams the same way.
func (ts *toolset) extend(added []string) {
	ts.keys = append(ts.keys, added...)
	slices.Sort(ts.keys)
	ts.pending = redact.New(ts.keys...)
	ts.pendingKeys = slices.Clone(ts.keys)
	for _, s := range ts.streams {
		s.Widen(ts.pending)
	}
}

// learn adds stored keys to the session's (plan 031 §3.8): vals, each already
// trimmed and held to modeltable.KeyProblem by LearnKeys. It is resolve for a
// key no table resolves — prepared for the next turn to adopt, never
// installed, only ever added — with one difference, which is the refusal
// (r2-2). A switch that would bring a key inside what this session sends
// unredacted can be refused, and nothing is learned; a stored key cannot be
// refused — it is stored, and the tools can show it — so it is learned all the
// same, for everything the session still redacts, and the session enters the
// refusal state instead (refusing): the surfaces are frozen, every later
// request would carry them, and begin refuses every turn from here on. It
// reports whether this call found such a key.
//
// One section under mu, which is what serializes learning: two calls — or a
// call and a switch's resolve — each extend the set the other left, never a
// copy of it from before, and the check, the keys and the flag are one fact to
// begin, which reads the flag in its own section under the same lock (adopt).
// A key the session already knew is only recorded for its children (learned):
// it passed the same checks when it first became known.
func (ts *toolset) learn(vals []string) (frozen bool) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return ts.learnLocked(vals)
}

// learnLocked is learn's section, ts.mu held: learn's, and addSecrets', which
// installs what it learned in the same section.
func (ts *toolset) learnLocked(vals []string) (frozen bool) {
	var added []string
	for _, v := range vals {
		if !slices.Contains(ts.learned, v) {
			ts.learned = append(ts.learned, v)
		}
		if !slices.Contains(ts.keys, v) && !slices.Contains(added, v) {
			added = append(added, v)
		}
	}
	if len(added) == 0 {
		return false
	}
	// The surfaces resolve refuses a switch on, but the key is learned all
	// the same.
	if ts.frozenHolds(added) {
		ts.refusing.Store(true)
		frozen = true
	}
	ts.extend(added)
	return frozen
}

// addSecrets is Session.AddSecrets' section (plan 033 §3.12, P19): vals —
// each already vetted by modeltable.KeyProblem — learned exactly as learn
// learns a stored key, through extend, the one place the key set grows (the
// next turn's redactor covers them, a child opened later starts with them,
// and one inside a frozen surface puts the session in its refusal state), and
// then installed at once, in the toolset and the dispatcher: the redactor a
// running turn uses widened by these values alone.
//
// That install is the one exception to "one redactor per turn" (adopt,
// tools.go's R1). The rule keeps a turn from persisting a key it sent raw in
// an earlier step, and a key the next turn adopts may be in this turn's
// history already. A sign-in's token value is not: it was minted or adopted
// this instant, by this process (the token source notifies before it writes
// the file or uses the token), so no request of the session has carried it,
// and a tool's output that would print it from here on — a cat of the token
// file, an echo — must not wait for the next turn to be redacted. Every other
// key still waits for begin: installed grows by vals alone. It reports
// whether learning found one inside a frozen surface.
func (ts *toolset) addSecrets(vals []string) (frozen bool) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	frozen = ts.learnLocked(vals)
	grown := false
	for _, v := range vals {
		if !slices.Contains(ts.installed, v) {
			ts.installed, grown = append(ts.installed, v), true
		}
	}
	if !grown {
		return frozen
	}
	slices.Sort(ts.installed)
	red := redact.New(ts.installed...)
	ts.red.Store(red)
	ts.d.SetRedactor(red)
	return frozen
}

// learnedKeys are the stored keys learn has accepted (toolset.learned), as a
// copy the caller owns: what the runner hands a child it opens.
func (ts *toolset) learnedKeys() []string {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return slices.Clone(ts.learned)
}

// adopt installs a redactor resolve or learn prepared, if there is one, in the
// toolset and in the dispatcher. Run calls it as a turn begins — before the
// turn's first request, and with no turn running, since the session admits
// one at a time — so a turn uses exactly one redactor
// from its first step to its last, and the two pointers are never seen
// disagreeing. A call still running from an earlier turn keeps the Env it
// was given (Dispatcher.SetRedactor).
//
// In the refusal state (learn) it installs nothing and returns
// ErrStoredKeyFrozen, which begin refuses the turn with: the check and the
// adoption are one section, so no turn can adopt a redactor learn prepared
// after it found a frozen key and then go on to send the prompt that holds it.
func (ts *toolset) adopt() error {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if ts.refusing.Load() {
		return ErrStoredKeyFrozen
	}
	if ts.pending == nil {
		return nil
	}
	red := ts.pending
	ts.pending = nil
	ts.installed, ts.pendingKeys = ts.pendingKeys, nil
	ts.red.Store(red)
	ts.d.SetRedactor(red)
	return nil
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
func (ts *toolset) redactErr(err error) error { return redactErrWith(ts.redactor(), err) }

// redactErrWith is redactErr with red, for an error met before the session
// has a toolset to redact with (a resume's search for its transcript).
func redactErrWith(red *redact.Replacer, err error) error {
	if err == nil {
		return nil
	}
	text := err.Error()
	msg := red.String(text)
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

// startModelOf is how the prompt names a resolved model (startModel): the
// table's name — Resolve's, which is the alias when the entry has none — and
// the provider and wire model a request goes to.
func startModelOf(r modeltable.Resolved) startModel {
	return startModel{name: r.Name, provider: r.ProviderID, wire: r.WireModel}
}

// profileRef is how a resumed session's profile is chosen: by the name its
// transcript's header records, and no model's (plan 028 §3.3). "" is the
// registry's default, as a model with no tool_profile gets.
func profileRef(name string) tool.ModelRef { return tool.ModelRef{Profile: name} }

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
