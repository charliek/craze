package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/charliek/craze/internal/acp"
)

// ModeKind is what craze takes a mode to mean, independent of the id the agent
// spells it with. ModeUnknown is the zero value on purpose: a mode nobody
// recognises — including a lookup in an empty table — must never read as one
// that implements.
type ModeKind int

const (
	ModeUnknown ModeKind = iota
	ModeImplement
	ModePlan
	ModeReadOnly
)

const (
	cursorName = "cursor"
	grokName   = "grok"
	gxName     = "gx"
	nativeName = "native"
	// cursorImplementPrompt is what leaving plan mode sends as the user's
	// turn; it is provider-shaped because the wording is the agent's, not
	// craze's. Grok uses the same wording this cut.
	cursorImplementPrompt = "Implement the plan above."
	// fastCategory is the config category cursor puts its fast toggle in.
	fastCategory = "model_config"
)

// Provider is everything craze knows about the agent behind a session that is
// not on the wire: what to call it, what its mode ids mean, and which of the
// config options it advertises are the effort and fast controls, plus how to
// spawn it, authenticate against it, and discover its skills.
//
// It is an immutable value with methods rather than a set of callbacks, so a
// Snapshot can carry a copy of it and the UI can read it without holding a
// session. A new provider adds a constructor here and changes nothing else.
type Provider struct {
	name            string
	modeKinds       map[string]ModeKind
	implementPrompt string
	// defaultBins is the PATH lookup order ResolveBinary tries.
	defaultBins []string
	// spawnArgs are the subcommand pieces (cursor: "--trust", "acp";
	// grok: "agent", "stdio"). forceAfter is the index into spawnArgs after
	// which the force args are inserted: 0 for cursor (Extra + --force +
	// --trust acp), 1 for grok (Extra + --no-auto-update + agent +
	// --always-approve + stdio).
	spawnArgs  []string
	forceAfter int
	// forceArgs replace cursor's --force (grok: --always-approve).
	forceArgs []string
	// extraGlobalArgs go before the subcommand (grok: --no-auto-update).
	extraGlobalArgs []string
	// authMethods is the preference order intersected with initialize's
	// advertised methods.
	authMethods  []authMethod
	loginHint    string
	capabilities Capabilities
	dialect      acp.DialectID
	skillScan    SkillScan
	// pluginScan is the plugin content the provider's own loader would find.
	// Zero means the provider needs none: grok advertises its plugin skills
	// over ACP and expands them itself.
	pluginScan PluginScan
	// fallbackModes are injected when session/new advertises no modes
	// (grok omits availableModes but accepts set_mode for these ids).
	fallbackModes []ModeInfo
	// planUpdatesAreTodos maps ACP `plan` session updates onto the todo
	// stream; cursor drives todos through cursor/update_todos instead.
	planUpdatesAreTodos bool
	// subagentToolName is the wire tool name that is a sub-agent spawn:
	// grok spawn_subagent, cursor task. Title-regex fallback only runs
	// when this is unknown and titleTaskFallback is set (cursor).
	subagentToolName  string
	titleTaskFallback bool
	// optional keeps a provider out of the startup picker unless a binary for
	// it resolves. gx is a personal fork; cursor and grok are always offered,
	// so an empty picker is impossible.
	optional bool
	// hidden keeps a provider out of Providers() altogether: it lives in the
	// hidden list, which only ProviderByName reads, so it resolves by id but
	// no listing ever shows it, and it is never persisted as the default nor
	// written to the session index (plan 018 §3.4). optional is "listed when
	// installed"; hidden is "never listed".
	hidden bool
	// inProcess is a provider craze runs itself rather than spawning over ACP,
	// so there is no binary to look up.
	inProcess bool
	// displayName is the label the UI shows. The id — name — is what flags,
	// the environment, config.toml and the session index hold, so the label
	// can change without touching anything persisted. Empty is the id.
	displayName string
}

// authMethod is one way a provider can authenticate, in preference order.
// envKeys, when set, make the method usable only while one of them is
// non-empty in the environment (grok's xai.api_key needs a key to send).
// meta rides on the authenticate request as _meta.
type authMethod struct {
	id      string
	envKeys []string
	meta    map[string]any
}

// usable reports whether the method is both advertised and, when it needs an
// env key, backed by one in the environment the child will see.
func (a authMethod) usable(initRes *acp.InitializeResult, getenv func(string) string) bool {
	if !initRes.OffersAuthMethod(a.id) {
		return false
	}
	for _, k := range a.envKeys {
		if getenv(k) != "" {
			return true
		}
	}
	return len(a.envKeys) == 0
}

// grokHeadlessMeta is what grok's authenticate carries so the daemon never
// opens a browser on craze's behalf.
func grokHeadlessMeta() map[string]any { return map[string]any{"headless": true} }

// Capabilities is what the TUI reads to hide surfaces a backend cannot back.
type Capabilities struct {
	FastToggle          bool
	Effort              bool
	Modes               bool
	SubagentRows        bool
	SubagentTranscript  bool
	Todos               bool
	AskCards            bool
	PlanCards           bool
	ParameterizedPicker bool
	// Interject is the mid-turn merge: text folded into the running turn
	// without cancelling it. Grok has it; cursor's only mid-turn path is a
	// second prompt, which cancels.
	Interject bool
	// SubagentCancel is the stop of one running sub-agent, the rest of the
	// turn going on (plan 026 §3.10): the session implements
	// SubagentCanceller, and the TUI offers Backspace/Delete on a running row
	// and in a running child's view, with its banner hint and help line. Native
	// has it; neither grok nor cursor has a per-child stop on the wire.
	SubagentCancel bool
}

// SkillScan is where a provider's on-disk skills come from: SKILL.md trees
// under RelRoots in the workspace and home. Skills the agent advertises over
// ACP (grok lists every bundled and plugin skill in
// available_commands_update) need no scan at all.
type SkillScan struct {
	RelRoots          []string
	SkipCursorPlugins bool
	// NameFromDir names a skill by its directory and ignores the frontmatter
	// name (cursor-agent's rule, verified). Off, the frontmatter name wins
	// and the directory is the fallback (grok-build's rule).
	NameFromDir bool
}

// Args builds the child argv from the provider pieces. ExtraArgs stay first
// so test fakes keep working; force decides whether the provider's force
// args are included.
func (p Provider) Args(extra []string, force bool) []string {
	args := append([]string{}, extra...)
	args = append(args, p.extraGlobalArgs...)
	args = append(args, p.spawnArgs[:p.forceAfter]...)
	if force {
		args = append(args, p.forceArgs...)
	}
	args = append(args, p.spawnArgs[p.forceAfter:]...)
	return args
}

// cursorModeKinds is the mode vocabulary flattened: every spelling onto what
// it means. It is written once, at init, which is what lets every copy of a
// Provider (and every Snapshot) share the map instead of cloning it.
var cursorModeKinds = flattenModeKinds()

func flattenModeKinds() map[string]ModeKind {
	out := make(map[string]ModeKind)
	for _, v := range modeVocabulary {
		for _, a := range v.aliases {
			out[a] = v.kind
		}
	}
	return out
}

// CursorProvider is the provider behind every session so far.
func CursorProvider() Provider {
	return Provider{
		name:            cursorName,
		modeKinds:       cursorModeKinds,
		implementPrompt: cursorImplementPrompt,
		defaultBins:     []string{"cursor-agent", "agent"},
		spawnArgs:       []string{"--trust", "acp"},
		forceArgs:       []string{"--force"},
		authMethods:     []authMethod{{id: acp.AuthCursorLogin}},
		loginHint:       "agent login",
		capabilities: Capabilities{
			FastToggle:          true,
			Effort:              true,
			Modes:               true,
			SubagentRows:        true,
			SubagentTranscript:  false,
			Todos:               true,
			AskCards:            true,
			PlanCards:           true,
			ParameterizedPicker: true,
		},
		dialect:           acp.DialectCursor,
		subagentToolName:  "task",
		titleTaskFallback: true,
		skillScan: SkillScan{
			RelRoots: []string{
				filepath.Join(".cursor", "skills"),
				filepath.Join(".agents", "skills"),
				filepath.Join(".codex", "skills"),
				filepath.Join(".claude", "skills"),
			},
			SkipCursorPlugins: true,
			NameFromDir:       true,
		},
		// cursor-agent's ACP server builds its catalog without a plugins
		// service and never expands a plugin command, so everything a plugin
		// ships has to be found — and later expanded — by craze itself.
		pluginScan: PluginScan{Dirs: true, CursorCache: true, ClaudePlugins: true},
	}
}

// GrokProvider is the grok CLI: spawn, auth, dialect and skill roots.
func GrokProvider() Provider {
	return Provider{
		name:            grokName,
		modeKinds:       cursorModeKinds,
		implementPrompt: cursorImplementPrompt,
		defaultBins:     []string{"grok"},
		spawnArgs:       []string{"agent", "stdio"},
		forceAfter:      1,
		forceArgs:       []string{"--always-approve"},
		extraGlobalArgs: []string{"--no-auto-update"},
		// The env key beats the daemon's own default (cached_token), as in
		// t3code; grok.com / oidc are interactive browser logins and are
		// never listed here.
		authMethods: []authMethod{
			{id: acp.AuthXAIAPIKey, envKeys: []string{"XAI_API_KEY", "GROK_CODE_XAI_API_KEY"}, meta: grokHeadlessMeta()},
			{id: acp.AuthCachedToken, meta: grokHeadlessMeta()},
		},
		loginHint: "grok login",
		capabilities: Capabilities{
			FastToggle:          false,
			Effort:              true,
			Modes:               true,
			SubagentRows:        true,
			SubagentTranscript:  true,
			Todos:               true,
			AskCards:            true,
			PlanCards:           true,
			ParameterizedPicker: true,
			Interject:           true,
		},
		dialect:          acp.DialectGrok,
		subagentToolName: "spawn_subagent",
		// Grok advertises its whole catalog, plugin skills included, over
		// ACP; the walk only adds SKILL.md trees the daemon has not loaded.
		// grok-build always scans both .grok and .agents (.claude/.cursor sit
		// behind compat flags craze does not read), project before home.
		skillScan: SkillScan{
			RelRoots: []string{
				filepath.Join(".grok", "skills"),
				filepath.Join(".agents", "skills"),
			},
		},
		fallbackModes: []ModeInfo{
			{ID: "default", Name: "Default"},
			{ID: "plan", Name: "Plan"},
			{ID: "ask", Name: "Ask"},
		},
		planUpdatesAreTodos: true,
	}
}

// GxProvider is charliek/grok-build's `gx` fork. It is grok on the wire —
// same dialect, same auth, same ~/.grok home, same skills and plugins — so it
// is spelled as the difference rather than a second copy of the table: a
// different binary, a different login hint, and the one field that keeps it
// out of the picker on machines that do not have it.
func GxProvider() Provider {
	p := GrokProvider()
	p.name = gxName
	p.defaultBins = []string{gxName}
	p.loginHint = "gx login"
	p.optional = true
	return p
}

// NativeProvider is craze's own harness (plan 018): a provider craze runs in
// process rather than spawning, so everything ACP-shaped — binaries, spawn
// args, dialect, auth, skill and plugin scans — is zero. It is hidden: it
// resolves by id (--provider native, CRAZE_PROVIDER, a hand-written
// config.toml) and no listing shows it, it is never persisted as the default,
// and its sessions are never indexed until H7 gives them a loader (§3.4).
//
// Effort and interject came with H2's tool loop, which gives a turn later
// steps, and with the steer that merges into the next one (plan 019 §3.10,
// D-34). H5 brings the rest: ask_user_question and exit_plan_mode open a
// question and a plan ask on the session's registry, todo_write fills the
// tasks panel, and the harness's three modes turn the chip, `/plan` `/ask`
// `/agent`, Shift+Tab and `--plan`/`--ask` on (plan 023 §3.4, §3.6). H6's
// sub-agents turn on the row band and the child view (plan 026 §3.9): the
// adapter keeps a roster in Snapshot().Subagents and streams each child's own
// events tagged with its id (native_subagents.go); and the user can stop one of
// them while the turn goes on (SubagentCancel, §3.10).
//
// The mode table and the implement prompt are cursor's: native's ids are the
// same three words, so craze's canonical mode commands (`/plan`, `--ask`, the
// cycle) resolve to the same spellings here as there, and a plan approved on
// native is implemented by the same offer sending the same sentence. What
// stays zero is everything ACP-shaped.
//
// The display label is its own field so the UI can later say "craze" without
// touching the id that flags and config hold.
func NativeProvider() Provider {
	return Provider{
		name:            nativeName,
		displayName:     nativeName,
		hidden:          true,
		inProcess:       true,
		modeKinds:       cursorModeKinds,
		implementPrompt: cursorImplementPrompt,
		capabilities: Capabilities{
			Effort:             true,
			Interject:          true,
			Modes:              true,
			Todos:              true,
			AskCards:           true,
			PlanCards:          true,
			SubagentRows:       true,
			SubagentTranscript: true,
			SubagentCancel:     true,
		},
	}
}

// Providers is every provider craze lists, in picker order. It is the single
// public registry: ProviderByName and ProviderNames both read it, so a new
// provider is a constructor and one line here. It builds the slice per call
// rather than handing back a package-level one, for the same reason Bins and
// SkillScan copy: nothing a caller does to the result can reach the registry.
//
// A hidden provider is not here at all but in hiddenProviders, so every
// listing — the picker, --help, the unknown-provider error, and any written
// later — leaves it out without having to remember a filter (plan 018
// §3.4).
func Providers() []Provider {
	return []Provider{CursorProvider(), GrokProvider(), GxProvider()}
}

// hiddenProviders is the second registry: providers ProviderByName resolves
// and nothing lists. Listing them in Providers and filtering at each listing
// site was the alternative; keeping them out makes the safe answer the
// default one. ProviderByName reads this list after the public one, so a
// hidden entry can never shadow a listed id.
//
// hiddenMu exists for RegisterHiddenProviderForTest. Production code never
// writes the list once the package is initialised, but a test in another
// package plants and removes an entry while a TUI it built may be resolving
// provider names on another goroutine.
var (
	hiddenMu        sync.RWMutex
	hiddenProviders = []Provider{NativeProvider()}
)

// hiddenProvider is ProviderByName's lookup in the hidden list. Ids are
// lowercase-exact there as in the public list: "Native" is no more an alias
// for "native" than "Grok" is for "grok".
func hiddenProvider(name string) (Provider, bool) {
	hiddenMu.RLock()
	defer hiddenMu.RUnlock()
	for _, p := range hiddenProviders {
		if p.name == name {
			return p, true
		}
	}
	return Provider{}, false
}

// RegisterHiddenProviderForTest is for tests only; production code never
// calls it. It adds a hidden, in-process provider with id name and display
// label label to the hidden list, and returns it with the function that takes
// it out again (idempotent; pass it to t.Cleanup). Its only purpose is to let
// the tests in internal/tui and internal/cli hold the hidden-provider rules —
// never listed, never persisted, never indexed — without a real hidden
// provider to hand. It lives outside a _test.go file because other packages'
// tests cannot see those.
//
// It panics on an empty name, or on one the registry already resolves: an
// entry that collided with a listed provider could never be reached, and one
// that collided with another hidden entry would be removed with it, so either
// is a broken test rather than something to carry on from.
func RegisterHiddenProviderForTest(name, label string) (Provider, func()) {
	if name == "" {
		panic("agent: RegisterHiddenProviderForTest: empty provider id")
	}
	p := Provider{name: name, displayName: label, hidden: true, inProcess: true}
	hiddenMu.Lock()
	defer hiddenMu.Unlock()
	taken := slices.ContainsFunc(Providers(), func(q Provider) bool { return q.name == name }) ||
		slices.ContainsFunc(hiddenProviders, func(q Provider) bool { return q.name == name })
	if taken {
		panic(fmt.Sprintf("agent: RegisterHiddenProviderForTest: provider %q is already registered", name))
	}
	hiddenProviders = append(hiddenProviders, p)
	var once sync.Once
	return p, func() {
		once.Do(func() {
			hiddenMu.Lock()
			defer hiddenMu.Unlock()
			hiddenProviders = slices.DeleteFunc(hiddenProviders, func(q Provider) bool { return q.name == name })
		})
	}
}

// ProviderNames is every registered provider's id, in Providers order.
func ProviderNames() []string {
	providers := Providers()
	names := make([]string, 0, len(providers))
	for _, p := range providers {
		names = append(names, p.name)
	}
	return names
}

// DefaultProviders is the non-optional subset of Providers: what the picker
// falls back to when the caller supplies no availability-filtered list.
func DefaultProviders() []Provider {
	providers := Providers()
	out := make([]Provider, 0, len(providers))
	for _, p := range providers {
		if !p.optional {
			out = append(out, p)
		}
	}
	return out
}

// ProviderByName looks up a provider by id: the listed providers first, then
// the hidden ones, which is how --provider, $CRAZE_PROVIDER and config.toml
// reach a provider no listing shows (plan 018 §3.4). An empty name is unset,
// not unknown: it reads as cursor. Ids are lowercase-exact.
func ProviderByName(name string) (Provider, error) {
	if name == "" {
		return CursorProvider(), nil
	}
	for _, p := range Providers() {
		if p.name == name {
			return p, nil
		}
	}
	if p, ok := hiddenProvider(name); ok {
		return p, nil
	}
	return Provider{}, fmt.Errorf("agent: unknown provider %q", name)
}

func (p Provider) Name() string { return p.name }

// DisplayName is the label the UI shows for p: the picker row, and the status
// bar and sub-agent view through ProviderInfo.Label. It falls back to the id,
// which is what every ACP provider shows. Flags, the environment, config.toml,
// the session index, craze prompt --json and host reports all keep the id
// (plan 018 §3.4).
func (p Provider) DisplayName() string {
	if p.displayName != "" {
		return p.displayName
	}
	return p.name
}

// Optional reports whether p is left out of the startup picker on a machine
// that has no binary for it.
func (p Provider) Optional() bool { return p.optional }

// Hidden reports whether p is one no listing shows: resolvable by id through
// ProviderByName, never in Providers, never persisted as the default and never
// written to the session index (plan 018 §3.4).
func (p Provider) Hidden() bool { return p.hidden }

// InProcess reports whether craze runs p itself rather than spawning an agent
// binary over ACP.
func (p Provider) InProcess() bool { return p.inProcess }

// BinaryResolves reports whether binary lookup for p succeeds — the same
// question acp.Spawn asks, with the same precedence: --agent-bin (explicit),
// then $CRAZE_AGENT_BIN, then p's own PATH candidates. It mirrors lookup and
// nothing more: an absolute override is accepted on os.Stat alone, so a true
// result is not a promise that the process will start.
//
// An in-process provider has nothing to spawn, so it always resolves and
// neither PATH nor the override is consulted.
func (p Provider) BinaryResolves(explicit string) bool {
	if p.inProcess {
		return true
	}
	_, err := acp.ResolveBinaryCandidates(explicit, p.Bins())
	return err == nil
}

// Bins is the provider's PATH lookup order for its agent binary.
func (p Provider) Bins() []string { return append([]string(nil), p.defaultBins...) }

// Dialect is the ACP dialect the client speaks to this provider.
func (p Provider) Dialect() acp.DialectID { return p.dialect }

// AuthMethodIDs is the preference order for authenticate, intersected with
// what initialize advertises.
func (p Provider) AuthMethodIDs() []string {
	out := make([]string, 0, len(p.authMethods))
	for _, a := range p.authMethods {
		out = append(out, a.id)
	}
	return out
}

// authFor picks the first usable auth method against what initialize
// advertised; getenv is the child's environment, since the key has to be
// there for the daemon to read. No advertised methods means no login, for
// every provider.
func (p Provider) authFor(initRes *acp.InitializeResult, getenv func(string) string) (id string, meta map[string]any, ok bool) {
	if initRes == nil || len(initRes.AuthMethods) == 0 {
		return "", nil, false
	}
	if getenv == nil {
		getenv = os.Getenv
	}
	for _, a := range p.authMethods {
		if !a.usable(initRes, getenv) {
			continue
		}
		if a.meta != nil {
			meta = make(map[string]any, len(a.meta))
			for k, v := range a.meta {
				meta[k] = v
			}
		}
		return a.id, meta, true
	}
	return "", nil, false
}

// FallbackModes are the modes craze assumes when session/new advertises none.
func (p Provider) FallbackModes() []ModeInfo { return append([]ModeInfo(nil), p.fallbackModes...) }

// LoginHint is the command the auth error tells the user to run.
func (p Provider) LoginHint() string { return p.loginHint }

// Capabilities is what the TUI reads to hide surfaces the backend lacks.
func (p Provider) Capabilities() Capabilities { return p.capabilities }

// SkillScan is where the provider's skills come from.
func (p Provider) SkillScan() SkillScan {
	return SkillScan{
		RelRoots:          append([]string(nil), p.skillScan.RelRoots...),
		SkipCursorPlugins: p.skillScan.SkipCursorPlugins,
		NameFromDir:       p.skillScan.NameFromDir,
	}
}

// PluginScan is where the provider's plugin commands and skills come from. The
// value holds no slices, so returning it is the whole of the clone.
func (p Provider) PluginScan() PluginScan { return p.pluginScan }

// ModeKind is what this provider's mode id means; an id it does not know is
// ModeUnknown.
func (p Provider) ModeKind(id string) ModeKind { return p.modeKinds[id] }

// ImplementPrompt is the turn craze sends when the user leaves plan mode by
// accepting the plan.
func (p Provider) ImplementPrompt() string { return p.implementPrompt }

// isEffortOption is the heuristic that finds the reasoning-effort select among
// the advertised options; EffortOption ranks the matches.
func (p Provider) isEffortOption(opt ConfigOption) bool { return isEffortSelect(opt) }

// isFastOption finds cursor's fast toggle, which is a model_config option
// rather than a model option and is not an effort level.
func (p Provider) isFastOption(opt ConfigOption) bool {
	if opt.Category != fastCategory {
		return false
	}
	return opt.ID == "fast" || strings.Contains(strings.ToLower(opt.Name), "fast")
}

// Info is the value copy a Snapshot carries.
func (p Provider) Info() ProviderInfo {
	return ProviderInfo{Name: p.name, modeKinds: p.modeKinds, implementPrompt: p.implementPrompt}
}

// ProviderInfo is a Provider flattened into plain fields so it can ride along
// in a Snapshot. Everything but the name is unexported and reached through a
// method, so the zero value — a stub session, a snapshot taken before
// session/new landed — can answer as cursor's and the default lives in this
// package alone. The mode table is shared by every snapshot rather than cloned
// into each one: it is written once, when CursorProvider builds it, and read
// from the TUI goroutine thereafter, so keeping it out of reach is what makes
// "immutable" a fact and not a comment.
type ProviderInfo struct {
	Name            string
	modeKinds       map[string]ModeKind
	implementPrompt string
}

// provider rebuilds the value with methods. The name alone decides which
// constructor runs, so a snapshot keeps its provider's capabilities, dialect
// and skill scan. A zero ProviderInfo — a stub session, a snapshot taken
// before session/new landed — is cursor's, which is what makes a nil
// Options.Provider behave identically to the real thing.
func (p ProviderInfo) provider() Provider {
	if prov, err := ProviderByName(p.Name); err == nil {
		return prov
	}
	return Provider{name: p.Name, modeKinds: p.modeKinds, implementPrompt: p.implementPrompt}
}

// Capabilities is the provider's TUI surface table, rebuilt from the name so
// a snapshot never loses what its provider can show.
func (p ProviderInfo) Capabilities() Capabilities { return p.provider().Capabilities() }

// Dialect is the ACP dialect the client speaks to the snapshot's provider.
func (p ProviderInfo) Dialect() acp.DialectID { return p.provider().Dialect() }

// SkillScan is where the snapshot's provider discovers skills.
func (p ProviderInfo) SkillScan() SkillScan { return p.provider().SkillScan() }

// Label is what the status row and the sub-agent view show for the snapshot's
// provider: its display label, rebuilt from the id like everything else here.
// For every ACP provider that is the id itself (plan 018 §3.4).
func (p ProviderInfo) Label() string { return p.provider().DisplayName() }

// Kind is what the session's provider means by a mode id.
func (p ProviderInfo) Kind(id string) ModeKind { return p.provider().ModeKind(id) }

// ImplementPrompt is the turn craze sends when the user accepts a plan.
func (p ProviderInfo) ImplementPrompt() string { return p.provider().ImplementPrompt() }
