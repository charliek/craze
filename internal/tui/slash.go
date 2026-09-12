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
		{Name: "clear", Desc: "Clear transcript", Builtin: true},
		{Name: "tasks", Desc: "Tasks panel: compact, expanded, hidden", Builtin: true},
		{Name: "theme", Desc: "Theme picker, or /theme <name>", Builtin: true},
		{Name: "plan", Desc: "Set plan mode", Builtin: true},
		{Name: "ask", Desc: "Set ask mode", Builtin: true},
		{Name: "agent", Desc: "Set agent mode", Builtin: true},
		{Name: "exit", Desc: "Quit craze", Builtin: true},
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
		m.input.SetValue("")
		return m.openHelp(), nil
	case "exit":
		m.input.SetValue("")
		return m.requestQuit()
	case "clear":
		m.input.SetValue("")
		m.clearTranscript()
		return m, nil
	case "tasks":
		m.input.SetValue("")
		return m.cycleTasks()
	case "theme":
		m.input.SetValue("")
		if args == "" {
			return m.openThemePicker(), nil
		}
		return m.setThemeNamed(args), nil
	case "model":
		if args == "" {
			m.input.SetValue("")
			return m.openModelDialog(), nil
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
	// Leaving the mode the plan was made in retires the offer with it, and the
	// kill is recorded against the turn so a late ending cannot bring it back.
	m.retirePlanOffer()
	m.addNote(modeNote(m.snap.Modes, id))
	sess := m.sess
	return m, func() tea.Msg {
		if err := sess.SetMode(context.Background(), id); err != nil {
			return revertModeMsg{prev: prev, err: err}
		}
		return nil
	}
}

// modeNote is what a mode change writes to the transcript: the id, and the
// agent's own description of the mode when it advertised one. Notes render
// dim as a whole, so the description needs no style of its own.
func modeNote(modes []agent.ModeInfo, id string) string {
	note := "mode → " + id
	for _, md := range modes {
		if md.ID == id && md.Description != "" {
			return note + " · " + sanitizeLine(md.Description)
		}
	}
	return note
}

// applyModelEffort is `/model <id> [effort]`: optimistic, with the same
// SetModel → SetConfig(model_config) fallback the dialog's model step uses.
func (m Model) applyModelEffort(id, effort string) (tea.Model, tea.Cmd) {
	prev := m.snap.CurrentModel
	m.snap.CurrentModel = id
	m.model = id
	m.input.SetValue("")
	explicit := effort != ""

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
