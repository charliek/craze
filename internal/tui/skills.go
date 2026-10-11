package tui

import (
	"github.com/charliek/craze/internal/agent"
)

// homeDir is agent.HomeDir under the name the TUI already calls it by; the
// config file (config.go) and the skill walk both read it. The definition
// moved into internal/agent because the plugin scan needs it too, and per-
// provider data lives there.
func homeDir() string { return agent.HomeDir() }

// rescanSkills walks the provider's filesystem roots for the disk skills the
// menu lists when it builds its own catalog (localSlash; plan 043 P14). Where
// the menu draws from the host's catalog the host walks them (its scanner,
// plan 043 P5) and these are not read. It runs when the session comes up
// (sessionUp), so they are ready for any moment the menu has no catalog of
// the host's — before the install, after a snapshot that left it out — when
// the menu goes over from its host's catalog to its own (recompute), and on a
// bare `/` while the menu builds its own (updateComposer). Skills the agent
// advertises over ACP arrive as available_commands_update and are merged by
// the builder.
func (m *Model) rescanSkills() {
	m.skills = agent.DiscoverSkills(m.providerValue(), m.cwd, homeDir())
}
