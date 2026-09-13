package tui

import (
	"os"
	"strings"

	"github.com/charliek/craze/internal/agent"
)

// homeDir is the home directory craze reads its own files out of: the config
// file and the user-level skills. HOME wins over the account database so a
// test (and the frame runner) can isolate both with one variable.
func homeDir() string {
	if home := strings.TrimSpace(os.Getenv("HOME")); home != "" {
		return home
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(home)
}

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
