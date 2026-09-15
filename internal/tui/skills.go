package tui

import (
	"github.com/charliek/craze/internal/agent"
)

// homeDir is agent.HomeDir under the name the TUI already calls it by; the
// config file (config.go) and the skill walk both read it. The definition
// moved into internal/agent because the plugin scan needs it too, and per-
// provider data lives there.
func homeDir() string { return agent.HomeDir() }

func skillsToSlash(skills []agent.Skill) []slashItem {
	out := make([]slashItem, 0, len(skills))
	for _, sk := range skills {
		out = append(out, slashItem{Name: sk.Name, Desc: sk.Description, Skill: true})
	}
	return out
}

func (m *Model) applySkills(skills []agent.Skill) {
	m.skills = skillsToSlash(skills)
}

// rescanSkills walks the provider's filesystem roots. It runs at Start and
// on a bare `/`; skills the agent advertises over ACP arrive as
// available_commands_update and are merged by slashCatalog.
func (m *Model) rescanSkills() {
	m.applySkills(agent.DiscoverSkills(m.providerValue(), m.cwd, homeDir()))
}

// scanDiskSkills is the cursor filesystem walk, kept for tests that pin
// project-beats-home and the plugin skip without constructing a Model.
func scanDiskSkills(workspace, home string) []slashItem {
	return skillsToSlash(agent.DiscoverSkills(agent.CursorProvider(), workspace, home))
}
