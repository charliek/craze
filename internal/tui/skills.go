package tui

import (
	"os"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

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

// rescanSkills walks the provider's filesystem roots. Inspect is a tea.Cmd
// after Start; grok's `/` retrigger must not spawn it, so this is a no-op
// when the provider advertises inspect.
func (m *Model) rescanSkills() {
	p := m.providerValue()
	if len(p.SkillScan().InspectArgs) > 0 {
		return
	}
	m.applySkills(agent.DiscoverSkills(p, m.cwd, homeDir(), m.sessionBinary()))
}

func (m Model) discoverSkillsCmd() tea.Cmd {
	p := m.providerValue()
	if len(p.SkillScan().InspectArgs) == 0 {
		return nil
	}
	bin := m.sessionBinary()
	if bin == "" {
		return nil
	}
	gen := m.skillsGen
	cwd, home := m.cwd, homeDir()
	return func() tea.Msg {
		d := agent.DiscoverSkillsReport(p, cwd, home, bin)
		return skillsMsg{gen: gen, skills: skillsToSlash(d.Skills), inspectErr: d.InspectErr}
	}
}

// scanDiskSkills is the cursor filesystem walk, kept for tests that pin
// project-beats-home and the plugin skip without constructing a Model.
func scanDiskSkills(workspace, home string) []slashItem {
	return skillsToSlash(agent.DiscoverSkills(agent.CursorProvider(), workspace, home, ""))
}
