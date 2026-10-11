package tui

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/charliek/craze/internal/agent"
)

// The descriptor table moved to the agent package (plan 043 D1), and the host
// now builds the catalog a client draws its menu from. These tests hold the
// table and the builder to what this package dispatches and draws today, so C3
// can switch the menu to the builder with nothing moving (P14, P16, P20).

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

// tuiProjection is what a client that draws the TUI's menu from a catalog
// does with it, as P4 and P13 lay it down: a /btw row (requires aside) only
// where the backend serves side questions, /exit always (this client quits
// on its own whatever the server serves), /sessions — its own row — before
// /exit where it has a session list, and of each name the first row it shows.
// It is a test's reading of the rules, for the parity check below; C3 writes
// the menu's own.
func tuiProjection(c agent.SlashCatalog, m Model) []slashItem {
	btw := m.btwOffered()
	seen := map[string]bool{}
	var out []slashItem
	for _, it := range c.Items {
		if it.Requires == agent.SlashRequiresAside && !btw {
			continue
		}
		if it.Source == agent.SlashSourceBuiltin && it.Name == "exit" && m.sessions != nil {
			out = append(out, sessionsBuiltin)
			seen[sessionsBuiltin.Name] = true
		}
		n := strings.ToLower(it.Name)
		if seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, slashItem{
			Name: it.Name, Desc: it.Description,
			Builtin: it.Source == agent.SlashSourceBuiltin, Skill: it.Source == agent.SlashSourceSkill,
			Plugin: it.Plugin, Qualified: it.Qualified,
		})
	}
	return out
}

// slashInputsOf is the builder's inputs as this model holds them.
func slashInputsOf(m Model) agent.SlashInputs {
	skills := make([]agent.Skill, 0, len(m.skills))
	for _, it := range m.skills {
		skills = append(skills, agent.Skill{Name: it.Name, Description: it.Desc})
	}
	return agent.SlashInputs{
		Provider:     m.snap.Provider.Name,
		Capabilities: m.caps(),
		Modes:        modeIDs(m.snap.Modes),
		Commands:     m.snap.Commands,
		Plugins:      m.snap.Plugins,
		Skills:       skills,
	}
}

// rowKey is a menu row as everything downstream reads it: the qualified
// spelling only through qualifiedAlias, which is the one rule that reads it.
func rowKey(it slashItem) string {
	return strings.Join([]string{it.Name, it.Desc, it.labeledDesc(), it.Plugin, it.qualifiedAlias(),
		map[bool]string{true: "builtin", false: ""}[it.Builtin], map[bool]string{true: "skill", false: ""}[it.Skill]}, "\x00")
}

// TestTheBuilderReproducesTheTUIsCatalog is the parity C3 rests on: over the
// same session — advertised commands, plugin rows, disk skills, names that
// collide with builtins and with each other, untypable names — the builder's
// catalog, projected as a client of the TUI's kind would, is the TUI's
// slashCatalog row for row: on cursor, on a session that answers side
// questions, on native, with no modes, and with a session list.
func TestTheBuilderReproducesTheTUIsCatalog(t *testing.T) {
	commands := []string{"research", "btw", "exit", "help", "sessions", "two words", "Deploy"}
	entries := []agent.PluginEntry{
		pluginEntry("probe-plugin", "probe-echo", "the plugin one"),
		pluginEntry("helper", "help", "a plugin help"),
		pluginEntry("alpha", "rescue", "one"),
		pluginEntry("beta", "rescue", "the other"),
		pluginEntry("side", "btw", "a plugin btw"),
		pluginEntry("ops", "deploy", "shadowed by the advertised Deploy"),
	}
	// Descriptions the menu has to fold — a newline, a tab, an escape
	// sequence, runs of spaces — so the comparison is over DisplayLine's work
	// and not over strings that are already one clean line.
	entries[0].Description = "the plugin\none"
	decorate := func(t *testing.T, m Model, stub *Stub) Model {
		t.Helper()
		cmds := make([]agent.CommandInfo, 0, len(commands))
		for _, n := range commands {
			cmds = append(cmds, agent.CommandInfo{Name: n, Description: "advertised\t" + n + "\n\x1b[1mbold\x1b[0m  twice"})
		}
		stub.SetCommands(cmds)
		stub.SetPlugins(agent.ResolvePluginNames(entries, commands, false))
		m = republish(t, m)
		root := filepath.Join(m.cwd, ".cursor", "skills")
		writeSkillMD(t, filepath.Join(root, "on-disk", "SKILL.md"), "---\nname: on-disk\ndescription: found\tby the   walk\n---\n")
		writeSkillMD(t, filepath.Join(root, "research", "SKILL.md"), "---\nname: research\ndescription: shadowed\n---\n")
		writeSkillMD(t, filepath.Join(root, "probe-echo", "SKILL.md"), "---\nname: probe-echo\ndescription: shadowed by the plugin\n---\n")
		m.rescanSkills()
		return m
	}
	for _, tc := range []struct {
		name  string
		model func(t *testing.T) Model
	}{
		{"cursor", func(t *testing.T) Model {
			m, stub := slashModel(t, 100, 30)
			return decorate(t, m, stub)
		}},
		{"side questions", func(t *testing.T) Model {
			m, stub := asideModel(t)
			return decorate(t, m, stub)
		}},
		{"native", func(t *testing.T) Model {
			stub := nativeStub()
			isolateSkillsHome(t)
			return decorate(t, startStub(t, stub, t.TempDir(), 100, 30), stub)
		}},
		{"no modes", func(t *testing.T) Model {
			m, stub := slashModel(t, 100, 30)
			m = decorate(t, m, stub)
			m.snap.Modes = nil
			return m
		}},
		{"a session list", func(t *testing.T) Model {
			m, _, stub := sessModel(t, 100, 30)
			return decorate(t, m, stub)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := tc.model(t)
			want := m.slashCatalog()
			got := tuiProjection(agent.BuildSlashCatalog(slashInputsOf(m)), m)
			var w, g []string
			for _, it := range want {
				w = append(w, rowKey(it))
			}
			for _, it := range got {
				g = append(g, rowKey(it))
			}
			if !slices.Equal(g, w) {
				t.Fatalf("the builder's catalog, projected:\n  %s\nthe TUI's:\n  %s",
					strings.ReplaceAll(strings.Join(g, "\n  "), "\x00", " | "),
					strings.ReplaceAll(strings.Join(w, "\n  "), "\x00", " | "))
			}
			if len(want) < 15 {
				t.Fatalf("fixture: the TUI's catalog has %d rows; the decoration did not land", len(want))
			}
		})
	}
}
