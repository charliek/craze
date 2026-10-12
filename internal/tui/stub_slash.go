package tui

import (
	"slices"

	"github.com/charliek/craze/internal/agent"
)

// The Stub's slash catalog (plan 043 C2): what a PublishSlash Stub publishes,
// so the fake host's wire fixtures and a remote client's tests can watch the
// slash section travel without an agent. The catalog is built by the one
// builder the live and native sessions use (agent.SlashCatalogFor, P19) over
// the Stub's snapshot and the skills SetSkills gave it, and it is published by
// the live session's rules (P3, D3): every install restates it, a delta that
// carries commands or plugins restates it beside them, a skills change
// publishes it alone, and a change that leaves it as it was publishes
// nothing. A Stub without PublishSlash builds none and publishes none.

var _ agent.SlashCataloger = (*Stub)(nil)

// PublishesSlashCatalog is agent.SlashCataloger's: PublishSlash, which is set
// before Start and never after, so the read takes no lock.
func (s *Stub) PublishesSlashCatalog() bool { return s.PublishSlash }

// SetSkills replaces the disk skills a PublishSlash Stub's catalog lists — what
// the live session's scan found — and publishes the slash section alone when
// the catalog changed, as the live session's applySkills does; nothing when
// it did not. On a Stub without PublishSlash it only records them. Like the
// other catalog setters it is test-side set-up (flushSetter).
func (s *Stub) SetSkills(skills []agent.Skill) {
	s.mu.Lock()
	s.skills = append([]agent.Skill(nil), skills...)
	if s.PublishSlash {
		if slash := s.slashChangedLocked(); slash != nil {
			s.enqueueDeltaLocked("", &agent.StateDelta{Slash: slash})
		}
	}
	s.mu.Unlock()
	s.flushSetter()
}

// SeedPlugins replaces Snapshot.Plugins with an already-resolved list and
// publishes nothing: the plugin rows a session starts with, which its install
// then carries — as SetModels seeds the models — for a fake host whose session
// is native's, whose rows are resolved once, at Start, and never published on
// their own. A list that changes while the session runs is SetPlugins'.
func (s *Stub) SeedPlugins(plugins []agent.PluginCommand) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snap.Plugins = append([]agent.PluginCommand(nil), plugins...)
}

// installSlashLocked is the slash section an install carries: the catalog
// built now and kept as Snapshot.Slash, in a copy of the delta's own; nil for
// a Stub without PublishSlash. s.mu is held.
func (s *Stub) installSlashLocked() *agent.SlashCatalog {
	if !s.PublishSlash {
		return nil
	}
	s.snap.Slash = agent.SlashCatalogFor(s.snap, s.skills).Items
	return &agent.SlashCatalog{Items: slices.Clone(s.snap.Slash)}
}

// slashChangedLocked rebuilds Snapshot.Slash and returns the section a delta
// carries when the catalog changed, nil when it is the one already published
// (the live session's own rule, live_slash.go). s.mu is held.
func (s *Stub) slashChangedLocked() *agent.SlashCatalog {
	next := agent.SlashCatalogFor(s.snap, s.skills).Items
	if s.snap.Slash != nil && slices.Equal(next, s.snap.Slash) {
		return nil
	}
	s.snap.Slash = next
	return &agent.SlashCatalog{Items: slices.Clone(next)}
}

// catalogDeltaLocked is the delta a commands or plugins setter publishes: st
// carries the setter's section, and same says that section is the one the
// Stub already had; nil is "publish nothing". A Stub without PublishSlash publishes st as it always
// has. A PublishSlash Stub publishes as the live session's
// available_commands_update does: when the section changed, st with the slash
// catalog restated beside it — whether or not the catalog moved, since a
// client reads the two as one change — and when it did not, the catalog alone
// if that moved (a catalog never yet built) and nothing at all otherwise.
// s.mu is held.
func (s *Stub) catalogDeltaLocked(st *agent.StateDelta, same bool) *agent.StateDelta {
	if !s.PublishSlash {
		return st
	}
	slash := s.slashChangedLocked()
	if same {
		if slash == nil {
			return nil
		}
		return &agent.StateDelta{Slash: slash}
	}
	if slash == nil {
		slash = &agent.SlashCatalog{Items: slices.Clone(s.snap.Slash)}
	}
	st.Slash = slash
	return st
}
