package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

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

// ProviderByName looks up a provider by id. An empty name is unset, not
// unknown: it reads as cursor.
func ProviderByName(name string) (Provider, error) {
	switch name {
	case "", cursorName:
		return CursorProvider(), nil
	case grokName:
		return GrokProvider(), nil
	default:
		return Provider{}, fmt.Errorf("agent: unknown provider %q", name)
	}
}

func (p Provider) Name() string { return p.name }

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

// Label is the provider name the status row shows.
func (p ProviderInfo) Label() string { return p.provider().Name() }

// Kind is what the session's provider means by a mode id.
func (p ProviderInfo) Kind(id string) ModeKind { return p.provider().ModeKind(id) }

// ImplementPrompt is the turn craze sends when the user accepts a plan.
func (p ProviderInfo) ImplementPrompt() string { return p.provider().ImplementPrompt() }
