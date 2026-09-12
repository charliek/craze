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
	Skill      bool
}

func (it slashItem) labeledDesc() string {
	if !it.Skill {
		return it.Desc
	}
	if strings.TrimSpace(it.Desc) == "" {
		return "(skill)"
	}
	return it.Desc + " (skill)"
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
	for _, sk := range m.skills {
		n := strings.ToLower(sk.Name)
		if n == "" {
			continue
		}
		if _, ok := seen[n]; ok {
			continue
		}
		seen[n] = struct{}{}
		items = append(items, slashItem{Name: sk.Name, Desc: sk.Desc, Skill: true})
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
		m.effortStep = false
		m.input.SetValue("")
		return m, nil
	case "exit", "quit":
		m.input.SetValue("")
		return m.requestQuit()
	case "clear":
		m.input.SetValue("")
		m.clearTranscript()
		return m, nil
	case "model", "models":
		if args == "" {
			m.picking = true
			m.effortStep = false
			m.help = false
			m.modelSel = 0
			m.input.SetValue("")
			for i, md := range agent.OrderModels(m.snap) {
				if md.ID == m.snap.CurrentModel {
					m.modelSel = i
					break
				}
			}
			return m, nil
		}
		modelArg, effortArg := agent.SplitModelEffort(args, m.snap)
		id, err := agent.MatchModel(m.snap, modelArg)
		if err != nil {
			m.input.SetValue("")
			m.addError(err.Error())
			return m, nil
		}
		return m.applyModelEffort(id, effortArg)
	case "plan", "ask", "agent":
		m.input.SetValue("")
		id, ok := agent.ResolveMode(name, modeIDs(m.snap.Modes))
		if !ok {
			m.addError("mode " + name + " is not advertised")
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
	m.addNote("mode → " + id)
	sess := m.sess
	return m, func() tea.Msg {
		if err := sess.SetMode(context.Background(), id); err != nil {
			return revertModeMsg{prev: prev, err: err}
		}
		return nil
	}
}

func (m Model) applyModel(id string) (tea.Model, tea.Cmd) {
	return m.applyModelEffort(id, "")
}

func (m Model) enterEffortStep() Model {
	opt := agent.EffortOption(m.snap)
	if opt == nil || len(opt.SelectValues) == 0 {
		m.picking = false
		m.effortStep = false
		return m
	}
	m.picking = true
	m.effortStep = true
	m.effortSel = 0
	for i, v := range opt.SelectValues {
		if v.Value == opt.Current {
			m.effortSel = i
			break
		}
	}
	return m
}

func (m Model) applyModelEffort(id, effort string) (tea.Model, tea.Cmd) {
	prev := m.snap.CurrentModel
	m.snap.CurrentModel = id
	m.model = id
	m.input.SetValue("")
	explicit := effort != ""
	if explicit {
		m.picking = false
		m.effortStep = false
	} else {
		m = m.enterEffortStep()
	}

	sess := m.sess
	modelCfgID := ""
	if opt := agent.ModelConfigOption(m.snap); opt != nil {
		modelCfgID = opt.ID
	}
	effortID := ""
	if explicit {
		if opt := agent.EffortOption(m.snap); opt != nil {
			effortID = opt.ID
		}
	}

	return m, func() tea.Msg {
		ctx := context.Background()
		if err := sess.SetModel(ctx, id); err != nil {
			if modelCfgID != "" {
				if err2 := sess.SetConfig(ctx, modelCfgID, id); err2 == nil {
					err = nil
				}
			}
			if err != nil {
				return revertModelMsg{prev: prev, err: err}
			}
		}
		if explicit && effortID != "" {
			if err := sess.SetConfig(ctx, effortID, effort); err != nil {
				return actionErrMsg{err}
			}
			return refreshSnapMsg{}
		}
		return nil
	}
}

func (m Model) applyEffort(value string) (tea.Model, tea.Cmd) {
	opt := agent.EffortOption(m.snap)
	m.picking = false
	m.effortStep = false
	m.input.SetValue("")
	if opt == nil || value == "" {
		return m, nil
	}
	id := opt.ID
	sess := m.sess
	return m, func() tea.Msg {
		if err := sess.SetConfig(context.Background(), id, value); err != nil {
			return actionErrMsg{err}
		}
		return refreshSnapMsg{}
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
