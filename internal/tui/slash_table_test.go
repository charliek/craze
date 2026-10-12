package tui

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/craze/internal/agent"
)

// The descriptor table moved to the agent package (plan 043 D1), the host
// builds the catalog a client draws its menu from, and the menu here is that
// catalog projected — the host's, or one built here by the same builder (C3).
// These tests hold the table to what this package dispatches, and both of the
// menu's paths to the menu as it was drawn before, so nothing moved (P14, P16,
// P20).

// TestSlashTableMatchesTheTUIsDispatch is P16's pin, and the rest of each
// builtin's run against runBuiltin: a row's WhileWorking is what handleEnter
// does with it while a turn runs, and its Run is the branch runBuiltin takes.
//
//   - The reserved rows are the fixed set builtinNamed dispatches, and
//     WhileWorking is runsWhileWorking's answer for each.
//   - /btw runs mid-turn: handleEnter's btw gate (btwDraft) comes before the
//     busy refusal, and it opens the side view whatever the session is doing
//     (TestASideQuestionOpensMidTurnAndStreams).
//   - /connect does not: runBuiltin opens it through openConnect, which
//     refuses busy work with connectBusyText.
//   - /sessions is this client's own row and is not in the table (P13).
func TestSlashTableMatchesTheTUIsDispatch(t *testing.T) {
	run := map[string]agent.SlashRun{
		"help":    {Kind: agent.SlashRunLocal, Local: "help"},    // openHelp
		"model":   {Kind: agent.SlashRunPicker, Picker: "model"}, // showModelDialog, or /model <id> [effort]
		"connect": {Kind: agent.SlashRunLocal, Local: "connect"}, // openConnect
		"clear":   {Kind: agent.SlashRunLocal, Local: "clear"},   // clearPending + clearTranscript
		"tasks":   {Kind: agent.SlashRunLocal, Local: "tasks"},   // cycleTasks
		"theme":   {Kind: agent.SlashRunLocal, Local: "theme"},   // openThemePicker, or setThemeNamed
		"rename":  {Kind: agent.SlashRunMethod, Method: agent.SlashMethodSetTitle, Param: "title"},
		"plan":    {Kind: agent.SlashRunMethod, Method: agent.SlashMethodSet}, // applyMode
		"ask":     {Kind: agent.SlashRunMethod, Method: agent.SlashMethodSet},
		"agent":   {Kind: agent.SlashRunMethod, Method: agent.SlashMethodSet},
		"btw":     {Kind: agent.SlashRunAside, Param: "question"}, // openBtw
		"exit":    {Kind: agent.SlashRunMethod, Method: agent.SlashMethodStop},
	}
	var names []string
	for _, b := range agent.SlashBuiltins() {
		it := b.Item
		names = append(names, it.Name)
		if b.Reserved != builtinNamed(it.Name) {
			t.Errorf("/%s: reserved %v, but builtinNamed says %v", it.Name, b.Reserved, builtinNamed(it.Name))
		}
		var want bool
		switch it.Name {
		case btwBuiltin.Name:
			want = true
		case connectBuiltin.Name:
			want = false
		default:
			if !builtinNamed(it.Name) {
				t.Fatalf("/%s is in the table and dispatched by nothing here", it.Name)
			}
			want = runsWhileWorking(it.Name)
		}
		if it.WhileWorking != want {
			t.Errorf("/%s: WhileWorking %v, the TUI's dispatch %v", it.Name, it.WhileWorking, want)
		}
		r, ok := run[it.Name]
		if !ok {
			t.Fatalf("/%s has no runBuiltin branch named here", it.Name)
		}
		got := agent.SlashRun{Kind: it.Run.Kind, Method: it.Run.Method, Param: it.Run.Param, Picker: it.Run.Picker, Local: it.Run.Local}
		if got != r {
			t.Errorf("/%s runs %+v, runBuiltin's branch is %+v", it.Name, got, r)
		}
	}
	if slices.Contains(names, sessionsBuiltin.Name) {
		t.Error("/sessions is in the host's table; it is the client's row (P13)")
	}
}

// todaysSlashCatalog is the menu as this package built it before plan 043 C3
// moved it onto the builder — slashCatalog's code at 5ca125e, frozen here,
// with m.skills read as the agent.Skill it now is. The parity tests hold both
// of the menu's paths, the host's catalog and the one built here, to it: to
// what users had, not only to each other.
func todaysSlashCatalog(m Model) []slashItem {
	modes, todos, btw := m.showModes(), m.showTodos(), m.btwOffered()
	var items []slashItem
	for _, it := range builtinSlash() {
		switch it.Name {
		case "plan", "ask", "agent":
			if !modes {
				continue
			}
		case "tasks":
			if !todos {
				continue
			}
		case "exit":
			if btw {
				items = append(items, btwBuiltin)
			}
			if m.sessions != nil {
				items = append(items, sessionsBuiltin)
			}
		}
		items = append(items, it)
		if it.Name == "model" && m.connectOffered() {
			items = append(items, connectBuiltin)
		}
	}
	seen := map[string]struct{}{}
	for _, it := range items {
		seen[it.Name] = struct{}{}
	}
	add := func(it slashItem) {
		if !slashNameOK(it.Name) {
			return
		}
		n := strings.ToLower(it.Name)
		if _, ok := seen[n]; ok {
			return
		}
		seen[n] = struct{}{}
		items = append(items, it)
	}
	for _, c := range m.snap.Commands {
		add(slashItem{Name: c.Name, Desc: sanitizeLine(c.Description)})
	}
	for _, p := range m.snap.Plugins {
		if !slashNameOK(p.Qualified) || !slashNameOK(p.Plugin) {
			continue
		}
		add(slashItem{Name: p.Display, Desc: sanitizeLine(p.Description), Plugin: sanitizeLine(p.Plugin), Qualified: sanitizeLine(p.Qualified)})
	}
	for _, sk := range m.skills {
		add(slashItem{Name: sk.Name, Desc: sanitizeLine(sk.Description), Skill: true})
	}
	return items
}

// rowKey is a menu row as everything downstream reads it — the filter, the
// accept, the Enter rule, the band and /help: its name, its description bare
// and labelled, its plugin, its second spelling (only through qualifiedAlias,
// the one rule that reads it) and its flags.
func rowKey(it slashItem) string {
	return strings.Join([]string{it.Name, it.Desc, it.labeledDesc(), it.Plugin, it.qualifiedAlias(),
		map[bool]string{true: "builtin", false: ""}[it.Builtin], map[bool]string{true: "skill", false: ""}[it.Skill]}, "\x00")
}

// sameMenu fails t unless got is want row for row (rowKey), saying what of.
func sameMenu(t *testing.T, what string, got, want []slashItem) {
	t.Helper()
	var g, w []string
	for _, it := range got {
		g = append(g, rowKey(it))
	}
	for _, it := range want {
		w = append(w, rowKey(it))
	}
	if !slices.Equal(g, w) {
		t.Fatalf("%s:\n  %s\nwant:\n  %s", what,
			strings.ReplaceAll(strings.Join(g, "\n  "), "\x00", " | "),
			strings.ReplaceAll(strings.Join(w, "\n  "), "\x00", " | "))
	}
}

// menuSession is one of the sessions the parity test draws the menu over: a
// Stub (publishing the slash catalog or not) wrapped as the case needs, with
// or without a session list.
type menuSession struct {
	name string
	// wrap is the session over the Stub (nil: the Stub itself); prep sets the
	// Stub up before it starts.
	wrap func(*Stub) agent.Session
	prep func(*Stub)
	list bool
	// commands and entries decorate the session after it starts; skills are
	// written under .cursor/skills or .grok/skills as the provider walks.
	commands []string
	entries  []agent.PluginEntry
	skills   map[string]string
}

// startMenuSession starts c's session, publishing the catalog or not, in
// workspace ws, and decorates it: the agent's commands, the plugin rows the
// live session's resolver names, and the disk skills — found by this client's
// own walk of ws where the session publishes no catalog, and handed to the
// Stub as its host's scan where it does (from skillsWS, a workspace the
// model never walks, so a skill on the hosted menu can only have come from the
// catalog).
func startMenuSession(t *testing.T, c menuSession, publish bool, ws, skillsWS string) Model {
	t.Helper()
	stub := NewStub()
	stub.PublishSlash = publish
	if c.prep != nil {
		c.prep(stub)
	}
	var sess agent.Session = stub
	if c.wrap != nil {
		sess = c.wrap(stub)
	}
	cfg := Config{Session: sess, Theme: "tokyo-night", Workspace: ws, Model: "grok", Yolo: true}
	if c.list {
		cfg.Sessions = &fakeSessions{}
	}
	tm, _ := New(cfg).Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m := startedLikeInit(t, tm.(Model))
	cmds := make([]agent.CommandInfo, 0, len(c.commands))
	for _, n := range c.commands {
		// Descriptions the menu has to fold — a tab, a newline, an escape
		// sequence, runs of spaces — so the comparison is over DisplayLine's
		// work and not over strings that are already one clean line.
		cmds = append(cmds, agent.CommandInfo{Name: n, Description: "advertised\t" + n + "\n\x1b[1mbold\x1b[0m  twice"})
	}
	stub.SetCommands(cmds)
	stub.SetPlugins(agent.ResolvePluginNames(c.entries, c.commands, false))
	if publish {
		stub.SetSkills(agent.DiscoverSkills(m.providerValue(), skillsWS, homeDir()))
	} else {
		m.rescanSkills()
	}
	return applyPending(t, m)
}

// TestTheMenuIsTheSameWithTheSectionAndWithout is P14's bar (plan 043 §5 C3):
// over one session's facts — advertised commands, plugin rows (a collision
// qualified both ways, one named like a builtin, one shadowed by a command),
// disk skills (one shadowed by a command, one by a plugin row), names that
// collide with builtins and with each other, untypable names — the menu the
// host's slash section projects to and the menu built here without one are
// row for row the menu this package built before the catalog existed: on
// cursor, on a session that answers side questions, on native, with no modes,
// with a session list, and on grok, whose skills keep their frontmatter
// names. The hosted model's own walk finds no skill at all, so the skill rows
// on its menu came through the section; and each model says which path it
// drew.
//
// Negative controls (each run by hand, each fails here): a projection that
// ignores requires (every case without side questions gains the builtin /btw
// over the agent's); one that adds no /sessions row, or adds it without
// hiding the agent's ("a session list"); one that maps no plugin label (every
// case).
func TestTheMenuIsTheSameWithTheSectionAndWithout(t *testing.T) {
	commands := []string{"research", "btw", "exit", "help", "sessions", "two words", "Deploy", "Plan"}
	entries := []agent.PluginEntry{
		pluginEntry("probe-plugin", "probe-echo", "the plugin\none"),
		pluginEntry("helper", "help", "a plugin help"),
		pluginEntry("alpha", "rescue", "one"),
		pluginEntry("beta", "rescue", "the other"),
		pluginEntry("side", "btw", "a plugin btw"),
		pluginEntry("ops", "deploy", "shadowed by the advertised Deploy"),
	}
	cursorSkills := map[string]string{
		".cursor/skills/on-disk/SKILL.md":    "---\nname: on-disk\ndescription: found\tby the   walk\n---\n",
		".cursor/skills/research/SKILL.md":   "---\nname: research\ndescription: shadowed\n---\n",
		".cursor/skills/probe-echo/SKILL.md": "---\nname: probe-echo\ndescription: shadowed by the plugin\n---\n",
		".agents/skills/fromdir/SKILL.md":    "---\ndescription: named by its directory\n---\n",
	}
	native := func(s *Stub) { s.SetProvider(agent.NativeProvider()) }
	for _, c := range []menuSession{
		{name: "cursor"},
		{name: "side questions", wrap: func(s *Stub) agent.Session { return AsidingStub{Stub: s} }},
		{name: "native", prep: native},
		{name: "no modes", prep: func(s *Stub) { s.snap.Modes = nil }},
		{name: "a session list", list: true},
		{name: "grok", prep: func(s *Stub) { s.SetProvider(agent.GrokProvider()) },
			wrap: func(s *Stub) agent.Session { return AsidingStub{Stub: s} },
			skills: map[string]string{
				".grok/skills/dir-alpha/SKILL.md":    "---\nname: fm-alpha\ndescription: grok keeps the frontmatter name\n---\n",
				".agents/skills/agentskill/SKILL.md": "---\nname: agentskill\ndescription: from .agents\n---\n",
			}},
	} {
		t.Run(c.name, func(t *testing.T) {
			isolateSkillsHome(t)
			c.commands, c.entries = commands, entries
			if c.skills == nil {
				c.skills = cursorSkills
			}
			skillsWS := t.TempDir()
			for rel, body := range c.skills {
				writeSkillMD(t, filepath.Join(skillsWS, filepath.FromSlash(rel)), body)
			}
			local := startMenuSession(t, c, false, skillsWS, "")
			hosted := startMenuSession(t, c, true, t.TempDir(), skillsWS)
			if local.slashHosted() || !hosted.slashHosted() {
				t.Fatalf("fixture: the local model draws the host's catalog %v, the hosted one %v", local.slashHosted(), hosted.slashHosted())
			}
			if len(hosted.skills) != 0 {
				t.Fatalf("fixture: the hosted model's own walk found %+v", hosted.skills)
			}
			want := todaysSlashCatalog(local)
			sameMenu(t, "the menu built here", local.slashCatalog(), want)
			sameMenu(t, "the menu the host's section projects to", hosted.slashCatalog(), want)
			var skills, plugins int
			for _, it := range want {
				if it.Skill {
					skills++
				}
				if it.Plugin != "" {
					plugins++
				}
			}
			if len(want) < 15 || (c.name != "native" && skills < 2) || plugins < 4 {
				t.Fatalf("fixture: the menu has %d rows, %d skills, %d plugin rows; the decoration did not land", len(want), skills, plugins)
			}
		})
	}
}
