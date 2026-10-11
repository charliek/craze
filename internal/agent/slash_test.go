package agent

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/charliek/craze/internal/harness/redact"
	"github.com/charliek/craze/internal/protocol"
)

// The builder's tests (plan 043 C1). Every precedence and gate case the TUI's
// own catalog tests pin (internal/tui/slash_test.go, skills_test.go) is here
// against BuildSlashCatalog, which C3 makes the TUI's menu: the same inputs,
// the same rows, in the same order.

// cursorInputs is a cursor session's facts: modes shown, tasks panel, the
// three canonical modes advertised, no catalog yet.
func cursorInputs() SlashInputs {
	return SlashInputs{
		Provider:     CursorProvider().Name(),
		Capabilities: CursorProvider().Capabilities(),
		Modes:        []string{"agent", "plan", "ask"},
	}
}

func slashNames(c SlashCatalog) []string {
	out := make([]string, 0, len(c.Items))
	for _, it := range c.Items {
		out = append(out, it.Name)
	}
	return out
}

// nonBuiltinNames is the catalog's names less the builtins, which is what the
// TUI's precedence tests compare.
func nonBuiltinNames(c SlashCatalog) []string {
	var out []string
	for _, it := range c.Items {
		if it.Source != SlashSourceBuiltin {
			out = append(out, it.Name)
		}
	}
	return out
}

func slashRow(t *testing.T, c SlashCatalog, name string) SlashItem {
	t.Helper()
	for _, it := range c.Items {
		if it.Name == name {
			return it
		}
	}
	t.Fatalf("no row %q in %v", name, slashNames(c))
	return SlashItem{}
}

// TestBuiltinSlashNamesAreTheReservedTen is P12: the reserved set is exactly
// today's ten, in today's order, so the plugin resolver qualifies exactly the
// names it always has — and /btw and /connect are not among them.
func TestBuiltinSlashNamesAreTheReservedTen(t *testing.T) {
	want := []string{"help", "model", "clear", "tasks", "theme", "rename", "plan", "ask", "agent", "exit"}
	if got := BuiltinSlashNames(); !slices.Equal(got, want) {
		t.Fatalf("reserved names %v, want %v", got, want)
	}
	// A plugin /btw stays bare, as it always has; a plugin /help does not.
	rows := ResolvePluginNames([]PluginEntry{
		{Plugin: "side", Name: "btw"}, {Plugin: "side", Name: "connect"}, {Plugin: "helper", Name: "help"},
	}, nil, false)
	var display []string
	for _, r := range rows {
		display = append(display, r.Display)
	}
	if want := []string{"btw", "connect", "helper:help"}; !slices.Equal(display, want) {
		t.Fatalf("plugin rows %v, want %v", display, want)
	}
}

// TestSlashBuiltinTableIsNormative is §3.2's table, row for row (P17): each
// builtin's exact TUI description, its run, its argument, what it requires and
// whether it runs mid-turn (P16). The descriptions are the strings the TUI's
// goldens draw, so a change here is a golden change.
func TestSlashBuiltinTableIsNormative(t *testing.T) {
	want := []SlashItem{
		{Name: "help", Description: "Keybindings and commands", Run: SlashRun{Kind: SlashRunLocal, Local: "help"}, WhileWorking: true},
		{Name: "model", Description: "Switch model", Run: SlashRun{Kind: SlashRunPicker, Picker: "model", Arg: SlashArgOptional, ArgHint: "<model> [effort]"}},
		{Name: "connect", Description: "Connect a model provider: store its API key", Run: SlashRun{Kind: SlashRunLocal, Local: "connect"}},
		{Name: "clear", Description: "Clear transcript", Run: SlashRun{Kind: SlashRunLocal, Local: "clear"}, WhileWorking: true},
		{Name: "tasks", Description: "Tasks panel: compact, expanded, hidden", Run: SlashRun{Kind: SlashRunLocal, Local: "tasks"}, WhileWorking: true},
		{Name: "theme", Description: "Theme picker, or /theme <name>", Run: SlashRun{Kind: SlashRunLocal, Local: "theme", Arg: SlashArgOptional, ArgHint: "<name>"}, WhileWorking: true},
		{Name: "rename", Description: "Rename this session, /rename <title>", Run: SlashRun{Kind: SlashRunMethod, Method: SlashMethodSetTitle, Param: "title", Arg: SlashArgRequired, ArgHint: "<title>"}, WhileWorking: true},
		{Name: "plan", Description: "Set plan mode", Run: SlashRun{Kind: SlashRunMethod, Method: SlashMethodSet, Setting: SlashSetting{Kind: "mode", Value: "plan"}}},
		{Name: "ask", Description: "Set ask mode", Run: SlashRun{Kind: SlashRunMethod, Method: SlashMethodSet, Setting: SlashSetting{Kind: "mode", Value: "ask"}}},
		{Name: "agent", Description: "Set agent mode", Run: SlashRun{Kind: SlashRunMethod, Method: SlashMethodSet, Setting: SlashSetting{Kind: "mode", Value: "agent"}}},
		{Name: "btw", Description: "Ask a side question (not saved): /btw <question>", Requires: SlashRequiresAside, Run: SlashRun{Kind: SlashRunAside, Param: "question", Arg: SlashArgRequired, ArgHint: "<question>"}, WhileWorking: true},
		{Name: "exit", Description: "End this session", Requires: SlashRequiresStop, Run: SlashRun{Kind: SlashRunMethod, Method: SlashMethodStop}, WhileWorking: true},
	}
	for i := range want {
		want[i].Source = SlashSourceBuiltin
	}
	var got []SlashItem
	for _, b := range SlashBuiltins() {
		got = append(got, b.Item)
	}
	if len(got) != len(want) {
		t.Fatalf("the table has %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d:\n got %+v\nwant %+v", i, got[i], want[i])
		}
	}
	// The whole native catalog with the three modes advertised is the table
	// itself, mode rows resolved to the same names.
	native := BuildSlashCatalog(SlashInputs{Provider: NativeProvider().Name(), Capabilities: NativeProvider().Capabilities(), Modes: []string{"agent", "plan", "ask"}})
	if !slices.Equal(native.Items, want) {
		t.Fatalf("native's catalog\n %+v\nwant the table\n %+v", native.Items, want)
	}
}

// TestSlashMethodsAreTheProtocols: a method row names the control protocol's
// own method, which a client calls verbatim.
func TestSlashMethodsAreTheProtocols(t *testing.T) {
	for got, want := range map[string]string{
		SlashMethodSetTitle: protocol.MethodSessionSetTitle,
		SlashMethodSet:      protocol.MethodSessionSet,
		SlashMethodStop:     protocol.MethodSessionStop,
	} {
		if got != want {
			t.Errorf("slash method %q, the protocol's %q", got, want)
		}
	}
	if string(protocol.SettingMode) != "mode" {
		t.Errorf("a mode row's setting kind is %q, the protocol's %q", "mode", protocol.SettingMode)
	}
}

// TestSlashCatalogBuiltinGates is the TUI's builtin order under every gate:
// the mode rows only where modes are shown (the capability AND a list), /tasks
// only with a tasks panel, /connect only on native and right after /model,
// /btw always (its Requires is the client's to read, P4) and /sessions never
// (P13).
func TestSlashCatalogBuiltinGates(t *testing.T) {
	noModes := cursorInputs()
	noModes.Modes = nil
	modesOff := cursorInputs()
	modesOff.Capabilities.Modes = false
	noTodos := cursorInputs()
	noTodos.Capabilities.Todos = false
	native := SlashInputs{Provider: NativeProvider().Name(), Capabilities: NativeProvider().Capabilities(), Modes: []string{"agent", "plan", "ask"}}
	for _, tc := range []struct {
		name string
		in   SlashInputs
		want string
	}{
		{"cursor", cursorInputs(), "help,model,clear,tasks,theme,rename,plan,ask,agent,btw,exit"},
		{"native adds connect after model", native, "help,model,connect,clear,tasks,theme,rename,plan,ask,agent,btw,exit"},
		{"no modes advertised", noModes, "help,model,clear,tasks,theme,rename,btw,exit"},
		{"modes not shown by the provider", modesOff, "help,model,clear,tasks,theme,rename,btw,exit"},
		{"no tasks panel", noTodos, "help,model,clear,theme,rename,plan,ask,agent,btw,exit"},
		{"the zero inputs", SlashInputs{}, "help,model,clear,theme,rename,btw,exit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := BuildSlashCatalog(tc.in)
			if got := strings.Join(slashNames(c), ","); got != tc.want {
				t.Fatalf("builtins %s, want %s", got, tc.want)
			}
			for _, it := range c.Items {
				if it.Source != SlashSourceBuiltin {
					t.Fatalf("row %+v is not a builtin", it)
				}
			}
		})
	}
}

// TestSlashCatalogUnavailableModesStayListed is P15: where the session shows
// modes, plan, ask and agent are all listed as the TUI lists them; each is
// resolved to the id the session advertised for it (ResolveMode's aliases),
// and one with no such id cannot run, in the TUI's own refusal words.
func TestSlashCatalogUnavailableModesStayListed(t *testing.T) {
	in := cursorInputs()
	in.Modes = []string{"code", "architect"}
	c := BuildSlashCatalog(in)
	for name, want := range map[string]SlashRun{
		"agent": {Kind: SlashRunMethod, Method: SlashMethodSet, Setting: SlashSetting{Kind: "mode", Value: "code"}},
		"plan":  {Kind: SlashRunMethod, Method: SlashMethodSet, Setting: SlashSetting{Kind: "mode", Value: "architect"}},
		"ask":   {Kind: SlashRunUnavailable, Reason: "mode ask is not advertised"},
	} {
		it := slashRow(t, c, name)
		if it.Run != want {
			t.Errorf("/%s runs %+v, want %+v", name, it.Run, want)
		}
		if it.WhileWorking {
			t.Errorf("/%s runs mid-turn; a mode change is refused while a turn runs (P16)", name)
		}
	}
	// grok's fallback list (default, plan, ask): agent resolves to default.
	in.Modes = []string{"default", "plan", "ask"}
	if got := slashRow(t, BuildSlashCatalog(in), "agent").Run.Setting.Value; got != "default" {
		t.Fatalf("/agent on grok sets %q, want default", got)
	}
}

// TestSlashCatalogRejectsUntypableNames is the TUI's test of the same name:
// a name with whitespace, a control character, U+FFFD or invalid UTF-8 never
// reaches the catalog, and a description is folded onto one safe line.
func TestSlashCatalogRejectsUntypableNames(t *testing.T) {
	in := cursorInputs()
	in.Commands = []CommandInfo{
		{Name: "two words", Description: "not one token"},
		{Name: "ctrl\x07bell", Description: "control character"},
		{Name: "bad\ufffdrune", Description: "the replacement rune"},
		{Name: "bad\xffbyte", Description: "invalid UTF-8, which range reads as U+FFFD"},
		{Name: "good", Description: "first line\nsecond\u009b2J line"},
	}
	in.Skills = []Skill{{Name: "spaced skill"}, {Name: ""}}
	c := BuildSlashCatalog(in)
	if got := nonBuiltinNames(c); !slices.Equal(got, []string{"good"}) {
		t.Fatalf("catalog kept an untypable name: %v", got)
	}
	if d := slashRow(t, c, "good").Description; d != "first line second2J line" {
		t.Fatalf("the description was not folded onto one safe line: %q", d)
	}
}

// TestSlashCatalogOrderAgentThenPluginThenSkill is the TUI's
// TestSlashCatalogOrderACPThenPluginThenDisk: advertised commands, then the
// plugin rows, then the disk skills — and a plugin row says its plugin and its
// second spelling.
func TestSlashCatalogOrderAgentThenPluginThenSkill(t *testing.T) {
	in := cursorInputs()
	in.Commands = []CommandInfo{{Name: "advertised", Description: "advertised advertised"}}
	in.Plugins = ResolvePluginNames([]PluginEntry{{Plugin: "probe-plugin", Name: "probe-echo", Description: "the plugin one"}}, []string{"advertised"}, false)
	in.Skills = []Skill{{Name: "on-disk", Description: "found by the walk"}}
	c := BuildSlashCatalog(in)
	if got := nonBuiltinNames(c); !slices.Equal(got, []string{"advertised", "probe-echo", "on-disk"}) {
		t.Fatalf("catalog order %v", got)
	}
	want := SlashItem{Name: "probe-echo", Description: "the plugin one", Source: SlashSourcePlugin,
		Plugin: "probe-plugin", Qualified: "probe-plugin:probe-echo", Run: SlashRun{Kind: SlashRunPrompt, Arg: SlashArgOptional}}
	if got := slashRow(t, c, "probe-echo"); got != want {
		t.Fatalf("plugin row %+v, want %+v", got, want)
	}
	if got := slashRow(t, c, "advertised"); got.Source != SlashSourceAgent || got.Plugin != "" || got.Qualified != "" {
		t.Fatalf("advertised row %+v", got)
	}
	if got := slashRow(t, c, "on-disk"); got.Source != SlashSourceSkill || got.Description != "found by the walk" {
		t.Fatalf("skill row %+v", got)
	}
	// Every non-builtin row is prompt text: it queues, so it never says it
	// runs mid-turn, and it takes whatever follows the name.
	for _, it := range c.Items {
		if it.Source == SlashSourceBuiltin {
			continue
		}
		if it.Run != (SlashRun{Kind: SlashRunPrompt, Arg: SlashArgOptional}) || it.WhileWorking || it.Requires != "" {
			t.Fatalf("non-builtin row %+v", it)
		}
	}
}

// TestSlashCatalogSanitisesPluginRows is the TUI's test of the same name: a
// plugin row passes the token gate on its name, its id and its qualified
// spelling, and its description is folded onto one line.
func TestSlashCatalogSanitisesPluginRows(t *testing.T) {
	in := cursorInputs()
	in.Plugins = []PluginCommand{
		{Plugin: "ok", Bare: "good", Display: "good", Qualified: "ok:good", Description: "first line\nsecond line"},
		{Plugin: "two words", Bare: "spaced", Display: "spaced", Qualified: "two words:spaced"},
		{Plugin: "ok", Bare: "bell", Display: "be\x07ll", Qualified: "ok:be\x07ll"},
	}
	c := BuildSlashCatalog(in)
	if got := nonBuiltinNames(c); !slices.Equal(got, []string{"good"}) {
		t.Fatalf("catalog kept an untypable plugin row: %v", got)
	}
	if it := slashRow(t, c, "good"); it.Description != "first line second line" || it.Plugin != "ok" || it.Qualified != "ok:good" {
		t.Fatalf("plugin row %+v", it)
	}
}

// TestSlashCatalogFirstClaimWins is the precedence rule, case by case: the
// first source to claim a name keeps it, case-insensitively — builtin, then
// advertised (the TUI's TestDiskSkillACPWinsOverDisk), then plugin, then disk
// skill — except that a row with Requires claims nothing, a builtin the gates
// left out claims nothing either, and /sessions is the client's (P13).
func TestSlashCatalogFirstClaimWins(t *testing.T) {
	agentRow := func(name, desc string) SlashItem {
		return SlashItem{Name: name, Description: desc, Source: SlashSourceAgent, Run: SlashRun{Kind: SlashRunPrompt, Arg: SlashArgOptional}}
	}
	t.Run("advertised beats plugin beats disk", func(t *testing.T) {
		in := cursorInputs()
		in.Commands = []CommandInfo{{Name: "research", Description: "Agent-advertised command"}, {Name: "dup", Description: "the agent's"}}
		in.Plugins = []PluginCommand{
			{Plugin: "p", Bare: "dup", Display: "dup", Qualified: "p:dup", Description: "the plugin's dup"},
			{Plugin: "p", Bare: "deploy", Display: "deploy", Qualified: "p:deploy", Description: "the plugin's deploy"},
		}
		in.Skills = []Skill{{Name: "research", Description: "disk research skill"}, {Name: "Deploy", Description: "the skill's"}, {Name: "RESEARCH"}}
		c := BuildSlashCatalog(in)
		if got := nonBuiltinNames(c); !slices.Equal(got, []string{"research", "dup", "deploy"}) {
			t.Fatalf("rows %v", got)
		}
		if got := slashRow(t, c, "research"); got != agentRow("research", "Agent-advertised command") {
			t.Fatalf("the advertised row must win over the disk skill: %+v", got)
		}
		if got := slashRow(t, c, "dup"); got.Source != SlashSourceAgent {
			t.Fatalf("the advertised row must win over the plugin row: %+v", got)
		}
		if got := slashRow(t, c, "deploy"); got.Source != SlashSourcePlugin {
			t.Fatalf("the plugin row must win over the disk skill: %+v", got)
		}
	})
	t.Run("a builtin claims its name", func(t *testing.T) {
		in := cursorInputs()
		in.Commands = []CommandInfo{{Name: "HELP"}, {Name: "plan"}}
		in.Skills = []Skill{{Name: "rename"}}
		if got := nonBuiltinNames(BuildSlashCatalog(in)); len(got) != 0 {
			t.Fatalf("an advertised or disk row shadowed a builtin: %v", got)
		}
	})
	t.Run("a builtin the gates left out claims nothing", func(t *testing.T) {
		// The TUI's catalog seeds its seen set from the builtins it listed, so
		// on a session with no modes an agent's own /ask is offered.
		in := cursorInputs()
		in.Modes = nil
		in.Commands = []CommandInfo{{Name: "ask", Description: "the agent's ask"}}
		in.Plugins = []PluginCommand{{Plugin: "keys", Bare: "connect", Display: "connect", Qualified: "keys:connect"}}
		c := BuildSlashCatalog(in)
		if got := nonBuiltinNames(c); !slices.Equal(got, []string{"ask", "connect"}) {
			t.Fatalf("rows %v", got)
		}
	})
	t.Run("a row with requires claims nothing", func(t *testing.T) {
		// /btw and /exit are listed for every session, and the client decides
		// whether to show them (P4). What follows them keeps its place: a
		// client that shows the builtin hides it, one that does not shows it
		// — the TUI's menu today with and without side questions.
		in := cursorInputs()
		in.Commands = []CommandInfo{{Name: "btw", Description: "the agent's btw"}, {Name: "exit", Description: "the agent's exit"}}
		in.Plugins = []PluginCommand{{Plugin: "side", Bare: "btw", Display: "btw", Qualified: "side:btw"}}
		c := BuildSlashCatalog(in)
		if got := strings.Join(slashNames(c), ","); got != "help,model,clear,tasks,theme,rename,plan,ask,agent,btw,exit,btw,exit" {
			t.Fatalf("catalog %s", got)
		}
		if got := nonBuiltinNames(c); !slices.Equal(got, []string{"btw", "exit"}) {
			t.Fatalf("the agent's rows after the requires rows: %v (the plugin's btw loses to the agent's)", got)
		}
	})
	t.Run("sessions is the client's", func(t *testing.T) {
		in := cursorInputs()
		in.Commands = []CommandInfo{{Name: "sessions", Description: "the agent's sessions"}}
		if got := nonBuiltinNames(BuildSlashCatalog(in)); !slices.Equal(got, []string{"sessions"}) {
			t.Fatalf("the host claimed /sessions: %v", got)
		}
	})
}

// TestSlashCatalogPluginNamedLikeABuiltin is the TUI's
// TestPluginNamedLikeABuiltinShowsQualified: the resolver qualifies a plugin's
// "help" (BuiltinSlashNames), so the catalog has craze's bare /help and the
// plugin's helper:help — displayed qualified, with no second spelling.
func TestSlashCatalogPluginNamedLikeABuiltin(t *testing.T) {
	in := cursorInputs()
	in.Plugins = ResolvePluginNames([]PluginEntry{{Plugin: "helper", Name: "help", Description: "the plugin one"}}, nil, false)
	c := BuildSlashCatalog(in)
	if got := slashRow(t, c, "help"); got.Source != SlashSourceBuiltin {
		t.Fatalf("the bare /help row is craze's own: %+v", got)
	}
	if got := slashRow(t, c, "helper:help"); got.Plugin != "helper" || got.Qualified != "" {
		t.Fatalf("the plugin's row %+v: displayed qualified, it has no second spelling", got)
	}
}

// TestSlashCatalogQualifiedOnlyWhenItDiffers is the TUI's qualifiedAlias: a
// plugin row answers to plugin:name only while it is displayed bare. Two
// plugins shipping /rescue are both displayed qualified, so neither has one.
func TestSlashCatalogQualifiedOnlyWhenItDiffers(t *testing.T) {
	in := cursorInputs()
	in.Plugins = ResolvePluginNames([]PluginEntry{
		{Plugin: "alpha", Name: "rescue"}, {Plugin: "beta", Name: "rescue"}, {Plugin: "zeta", Name: "runner"},
	}, nil, false)
	c := BuildSlashCatalog(in)
	if got := nonBuiltinNames(c); !slices.Equal(got, []string{"alpha:rescue", "beta:rescue", "runner"}) {
		t.Fatalf("rows %v", got)
	}
	for name, want := range map[string]string{"alpha:rescue": "", "beta:rescue": "", "runner": "zeta:runner"} {
		if got := slashRow(t, c, name).Qualified; got != want {
			t.Errorf("%s: qualified %q, want %q", name, got, want)
		}
	}
	// A case-only difference is the same spelling, as qualifiedAlias reads it.
	in.Plugins = []PluginCommand{{Plugin: "p", Bare: "x", Display: "P:X", Qualified: "p:x"}}
	if got := slashRow(t, BuildSlashCatalog(in), "P:X").Qualified; got != "" {
		t.Fatalf("a case-only difference gave a second spelling %q", got)
	}
}

// TestSlashCatalogDescriptionsAreDisplayLines is P18's half that is the
// builder's: every advertised, plugin and skill description is DisplayLine's
// — the TUI's sanitizeLine — so a description the menu would fold is folded
// on the wire too.
func TestSlashCatalogDescriptionsAreDisplayLines(t *testing.T) {
	raw := "\x1b[31mred\x1b[0m and\tthen\r\n\u009d0;title\u009c a  title"
	in := cursorInputs()
	in.Commands = []CommandInfo{{Name: "c", Description: raw}}
	in.Plugins = []PluginCommand{{Plugin: "p", Bare: "pc", Display: "pc", Qualified: "p:pc", Description: raw}}
	in.Skills = []Skill{{Name: "s", Description: raw}}
	c := BuildSlashCatalog(in)
	// The TUI's rule exactly, quirks included: ansi.Strip takes the 7-bit
	// CSI whole, while a C1 OSC spelled in UTF-8 loses only the introducer and
	// the terminator, which are controls.
	want := DisplayLine(raw)
	if want != "red and then 0;title a title" {
		t.Fatalf("fixture: DisplayLine(%q) = %q", raw, want)
	}
	for _, name := range []string{"c", "pc", "s"} {
		if got := slashRow(t, c, name).Description; got != want {
			t.Errorf("%s: description %q, want %q", name, got, want)
		}
	}
}

// TestSlashCatalogForReadsTheSnapshot is P19: the helper every session builds
// through derives the inputs from the snapshot — the provider's id and
// capabilities, the advertised modes' ids, the commands and plugin rows — and
// adds only the skills.
func TestSlashCatalogForReadsTheSnapshot(t *testing.T) {
	snap := Snapshot{
		Provider: NativeProvider().Info(),
		Modes:    nativeModes(),
		Commands: nativeCommands(),
		Plugins:  []PluginCommand{{Plugin: "p", Bare: "x", Display: "x", Qualified: "p:x"}},
	}
	skills := []Skill{{Name: "s"}}
	want := BuildSlashCatalog(SlashInputs{
		Provider: "native", Capabilities: NativeProvider().Capabilities(), Modes: modeIDs(snap.Modes),
		Commands: snap.Commands, Plugins: snap.Plugins, Skills: skills,
	})
	got := SlashCatalogFor(snap, skills)
	if !slices.Equal(got.Items, want.Items) {
		t.Fatalf("SlashCatalogFor\n %+v\nwant\n %+v", got.Items, want.Items)
	}
	if slashRow(t, got, "connect").Source != SlashSourceBuiltin {
		t.Fatal("native's catalog lists /connect")
	}
	// A zero ProviderInfo is cursor's (ProviderInfo.provider): no /connect.
	if slices.Contains(slashNames(SlashCatalogFor(Snapshot{}, nil)), "connect") {
		t.Fatal("a cursor snapshot listed /connect")
	}
}

// TestSlashItemHoldsNoReferences is what makes cloneSlash a deep copy (P6):
// no field reachable from a SlashItem is a pointer, slice, map, interface,
// channel or function. A field that became one would let a client reach the
// session's own rows through a snapshot it was handed.
func TestSlashItemHoldsNoReferences(t *testing.T) {
	var walk func(path string, typ reflect.Type)
	walk = func(path string, typ reflect.Type) {
		switch typ.Kind() {
		case reflect.Struct:
			for f := range typ.Fields() {
				walk(path+"."+f.Name, f.Type)
			}
		case reflect.Pointer, reflect.Slice, reflect.Map, reflect.Interface, reflect.Chan, reflect.Func, reflect.UnsafePointer, reflect.Array:
			t.Errorf("%s is a %s: cloneSlash would share it", path, typ.Kind())
		}
	}
	walk("SlashItem", reflect.TypeFor[SlashItem]())

	c := BuildSlashCatalog(cursorInputs())
	cp := cloneSlash(c.Items)
	cp[0].Description = "changed"
	if c.Items[0].Description == "changed" {
		t.Fatal("a clone shares its rows with the catalog it came from")
	}
}

// TestRedactSkills is P18's half that is the scan's: an ACP session's disk
// skills are redacted before anything stores them, by the keys native's
// redactor is built from — the description redacted, folded and redacted
// again, so a key an escape sequence split comes back as the marker and not
// whole; a skill whose name holds a key dropped, since a name cannot be
// altered (P7).
func TestRedactSkills(t *testing.T) {
	const key = "sk-test-0123456789abcdefghij"
	keys := []string{key}
	skills := []Skill{
		{Name: "plain", Description: "nothing to hide"},
		{Name: "leaky", Description: "use " + key + " to deploy"},
		{Name: "split", Description: "use sk-test-0123456789\x1b[0mabcdefghij to deploy"},
		{Name: "named-" + key, Description: "a name holding the key"},
	}
	got := redactSkills(slices.Clone(skills), keys)
	want := []Skill{
		{Name: "plain", Description: "nothing to hide"},
		{Name: "leaky", Description: "use " + redact.Marker + " to deploy"},
		{Name: "split", Description: "use " + redact.Marker + " to deploy"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("redacted\n %+v\nwant\n %+v", got, want)
	}
	// What the catalog then draws carries no key either.
	c := BuildSlashCatalog(SlashInputs{Skills: got})
	for _, it := range c.Items {
		if strings.Contains(it.Name+it.Description, key) {
			t.Fatalf("row %+v carries the key", it)
		}
	}
	// No keys known is no change at all.
	if got := redactSkills(slices.Clone(skills), nil); !slices.Equal(got, skills) {
		t.Fatalf("with no keys the skills changed: %+v", got)
	}
}
