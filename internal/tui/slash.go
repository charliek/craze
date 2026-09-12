package tui

import (
	"context"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/craze/internal/agent"
)

type slashItem struct {
	Name, Desc string
	Builtin    bool
}

func builtinSlash() []slashItem {
	return []slashItem{
		{Name: "help", Desc: "Keybindings and commands", Builtin: true},
		{Name: "model", Desc: "Switch model", Builtin: true},
		{Name: "models", Desc: "Switch model", Builtin: true},
		{Name: "clear", Desc: "Clear transcript", Builtin: true},
		{Name: "plan", Desc: "Set plan mode", Builtin: true},
		{Name: "ask", Desc: "Set ask mode", Builtin: true},
		{Name: "agent", Desc: "Set agent mode", Builtin: true},
		{Name: "exit", Desc: "Quit craze", Builtin: true},
		{Name: "quit", Desc: "Quit craze", Builtin: true},
	}
}

func builtinNamed(name string) bool {
	want := strings.ToLower(name)
	for _, b := range builtinSlash() {
		if b.Name == want {
			return true
		}
	}
	return false
}

func parseSlashLine(s string) (name, args string, ok bool) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "/") || strings.ContainsRune(s, '\n') {
		return "", "", false
	}
	rest := strings.TrimSpace(s[1:])
	if rest == "" {
		return "", "", true
	}
	fields := strings.Fields(rest)
	name = strings.ToLower(fields[0])
	args = strings.Join(fields[1:], " ")
	return name, args, true
}

func (m Model) slashCatalog() []slashItem {
	items := builtinSlash()
	seen := make(map[string]struct{}, len(items))
	for _, it := range items {
		seen[it.Name] = struct{}{}
	}
	for _, c := range m.snap.Commands {
		n := strings.ToLower(c.Name)
		if n == "" {
			continue
		}
		if _, ok := seen[n]; ok {
			continue
		}
		seen[n] = struct{}{}
		items = append(items, slashItem{Name: c.Name, Desc: c.Description, Builtin: false})
	}
	return items
}

func (m Model) slashMenuOpen() bool {
	if m.slashHide {
		return false
	}
	_, args, ok := parseSlashLine(m.input.Value())
	return ok && args == ""
}

func (m Model) filteredSlash() []slashItem {
	name, _, ok := parseSlashLine(m.input.Value())
	if !ok {
		return nil
	}
	var out []slashItem
	for _, it := range m.slashCatalog() {
		if name == "" || strings.HasPrefix(strings.ToLower(it.Name), name) {
			out = append(out, it)
		}
	}
	if len(out) > 6 {
		out = out[:6]
	}
	return out
}

func (m Model) runBuiltin(name, args string) (tea.Model, tea.Cmd) {
	switch name {
	case "help":
		m.help = true
		m.picking = false
		m.input.SetValue("")
		return m, nil
	case "exit", "quit":
		m.input.SetValue("")
		return m.requestQuit()
	case "clear":
		m.lines = nil
		m.input.SetValue("")
		m.refreshViewport()
		return m, nil
	case "model", "models":
		if args == "" {
			m.picking = true
			m.help = false
			m.modelSel = 0
			m.input.SetValue("")
			for i, md := range m.snap.Models {
				if md.ID == m.snap.CurrentModel {
					m.modelSel = i
					break
				}
			}
			return m, nil
		}
		id, err := agent.MatchModel(m.snap, args)
		if err != nil {
			m.input.SetValue("")
			m.addLine("error", err.Error())
			return m, nil
		}
		return m.applyModel(id)
	case "plan", "ask", "agent":
		m.input.SetValue("")
		id, ok := agent.ResolveMode(name, modeIDs(m.snap.Modes))
		if !ok {
			m.addLine("error", "mode "+name+" is not advertised")
			return m, nil
		}
		return m.applyMode(id)
	}
	return m.send()
}

func modeIDs(modes []agent.ModeInfo) []string {
	ids := make([]string, 0, len(modes))
	for _, m := range modes {
		ids = append(ids, m.ID)
	}
	return ids
}

func (m Model) applyMode(id string) (tea.Model, tea.Cmd) {
	if id == "" || id == m.snap.CurrentMode {
		return m, nil
	}
	prev := m.snap.CurrentMode
	m.snap.CurrentMode = id
	sess := m.sess
	return m, func() tea.Msg {
		if err := sess.SetMode(context.Background(), id); err != nil {
			return revertModeMsg{prev: prev, err: err}
		}
		return nil
	}
}

func (m Model) applyModel(id string) (tea.Model, tea.Cmd) {
	prev := m.snap.CurrentModel
	m.snap.CurrentModel = id
	m.model = id
	m.picking = false
	m.input.SetValue("")
	sess := m.sess
	return m, func() tea.Msg {
		if err := sess.SetModel(context.Background(), id); err != nil {
			return revertModelMsg{prev: prev, err: err}
		}
		return nil
	}
}

func (m Model) completeSlash() Model {
	items := m.filteredSlash()
	if len(items) == 0 {
		return m
	}
	idx := m.slashSel
	if idx < 0 || idx >= len(items) {
		idx = 0
	}
	m.input.SetValue("/" + items[idx].Name)
	return m
}
