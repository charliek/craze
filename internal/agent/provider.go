package agent

import "strings"

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
	// cursorImplementPrompt is what leaving plan mode sends as the user's
	// turn; it is provider-shaped because the wording is the agent's, not
	// craze's.
	cursorImplementPrompt = "Implement the plan above."
	// fastCategory is the config category cursor puts its fast toggle in.
	fastCategory = "model_config"
)

// Provider is everything craze knows about the agent behind a session that is
// not on the wire: what to call it, what its mode ids mean, and which of the
// config options it advertises are the effort and fast controls.
//
// It is an immutable value with methods rather than a set of callbacks, so a
// Snapshot can carry a copy of it and the UI can read it without holding a
// session. One provider is implemented; a second one adds a constructor here
// and changes nothing else.
type Provider struct {
	name            string
	modeKinds       map[string]ModeKind
	implementPrompt string
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

// CursorProvider is the only provider this cut speaks to.
func CursorProvider() Provider {
	return Provider{
		name:            cursorName,
		modeKinds:       cursorModeKinds,
		implementPrompt: cursorImplementPrompt,
	}
}

func (p Provider) Name() string { return p.name }

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
	return ProviderInfo{Name: p.name, modeKinds: p.modeKinds, ImplementPrompt: p.implementPrompt}
}

// ProviderInfo is a Provider flattened into plain fields so it can ride along
// in a Snapshot. The mode table stays unexported and is shared by every
// snapshot rather than cloned into each one: it is written once, when
// CursorProvider builds it, and read from the TUI goroutine thereafter, so
// keeping it out of reach is what makes "immutable" a fact and not a comment.
// Kind is how anything outside this package asks.
type ProviderInfo struct {
	Name            string
	modeKinds       map[string]ModeKind
	ImplementPrompt string
}

// provider rebuilds the value with methods. A zero ProviderInfo — a stub
// session, a snapshot taken before session/new landed — is cursor's, which is
// what makes a nil Options.Provider behave identically to the real thing.
func (p ProviderInfo) provider() Provider {
	if p.Name == "" {
		return CursorProvider()
	}
	return Provider{name: p.Name, modeKinds: p.modeKinds, implementPrompt: p.ImplementPrompt}
}

// Label is the provider name the status row shows.
func (p ProviderInfo) Label() string { return p.provider().Name() }

// Kind is what the session's provider means by a mode id.
func (p ProviderInfo) Kind(id string) ModeKind { return p.provider().ModeKind(id) }
