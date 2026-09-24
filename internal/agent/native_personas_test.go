package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/charliek/craze/internal/harness"
	"github.com/charliek/craze/internal/harness/redact"
	"github.com/charliek/craze/internal/harness/tool"
)

// Personas (plan 026 §3.4, A10): the three sources, their own namespace and
// budget, the agent-only frontmatter reader, the mapping the adapter does at
// discovery time, the agents toggle, and the harness's three seams wired in
// open().

// personaDoc is the shape of a persona file: a frontmatter name and
// description, extra frontmatter lines verbatim, and a body.
func personaDoc(name, desc string, extra ...string) string {
	fm := "---\n"
	if name != "" {
		fm += "name: " + name + "\n"
	}
	if desc != "" {
		fm += "description: " + desc + "\n"
	}
	for _, line := range extra {
		fm += strings.TrimSuffix(line, "\n") + "\n"
	}
	return fm + "---\nYou are " + name + ". Do the task.\n"
}

// installPack installs and enables one Claude plugin, "pack", under home with
// the files given, and returns its install directory.
func installPack(t *testing.T, home, workspace string, files map[string]string) string {
	t.Helper()
	install := writeTree(t, filepath.Join(home, ".claude", "plugins", "pack"), files)
	claudeFixture{
		installs: map[string][]claudeInstall{"pack@mkt": {{Scope: "user", InstallPath: install}}},
		user:     map[string]bool{"pack@mkt": true},
	}.write(t, home, workspace)
	return install
}

// discoverPersonas runs the whole loader over f and hands back the personas it
// would give the harness, with every line it wrote.
func (f contentFixture) discoverPersonas(c ClaudeCompat, keys ...string) ([]harness.Persona, []string) {
	var lines []string
	load := loadNativeContent(f.src, c, keys, func(msg string) { lines = append(lines, msg) })
	return load.personas, lines
}

// personaNames is the personas' names, in order.
func personaNames(ps []harness.Persona) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.Name)
	}
	return out
}

func wantPersonas(t *testing.T, got []harness.Persona, want ...string) {
	t.Helper()
	if names := personaNames(got); !slices.Equal(names, want) {
		t.Fatalf("personas %q, want %q", names, want)
	}
}

// agentTypesOf is the "## Agent types" section of the agent tool a request
// offered, as the names its rows list, in order: the list the harness merged
// the adapter's personas into.
func agentTypesOf(t *testing.T, call fantasy.Call) []string {
	t.Helper()
	for _, tl := range call.Tools {
		ft, ok := tl.(fantasy.FunctionTool)
		if !ok || ft.Name != tool.AgentTool {
			continue
		}
		_, section, found := strings.Cut(ft.Description, "## Agent types\n")
		if !found {
			t.Fatalf("the agent tool's description has no types section:\n%s", ft.Description)
		}
		section, _, _ = strings.Cut(section, "\n\n")
		var names []string
		for _, row := range strings.Split(section, "\n") {
			row = strings.TrimPrefix(row, "- ")
			// "name: description (tools: …)" or "name (tools: …)"; a name holds
			// no space, so it ends at the first ": " or " (".
			if i := strings.Index(row, ": "); i >= 0 && !strings.Contains(row[:i], " ") {
				row = row[:i]
			}
			name, _, _ := strings.Cut(row, " ")
			names = append(names, name)
		}
		return names
	}
	t.Fatalf("the request offered no agent tool")
	return nil
}

// toolNamesOf is every tool a request offered, by name.
func toolNamesOf(call fantasy.Call) []string {
	out := make([]string, 0, len(call.Tools))
	for _, tl := range call.Tools {
		out = append(out, tl.GetName())
	}
	return out
}

// runAgentCall runs one turn in which the parent calls the agent tool once with
// args, the child answers on child's scripted model, and the parent then
// answers. It returns the child's request.
func (c *nativeContent) runAgentCall(t *testing.T, child string, args map[string]any) fantasy.Call {
	t.Helper()
	parent := c.f.models["test/a"]
	before := len(c.f.models[child].requests())
	parent.push(nativeCallStep("call-1", tool.AgentTool, nativeArgs(t, args)))
	c.f.models[child].push(answer("the child's findings"))
	parent.push(answer("done"))
	if _, err := c.Prompt(context.Background(), "delegate it"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	reqs := c.f.models[child].requests()
	if child == "test/a" {
		// The parent's two steps share the model: the child's request is the one
		// between them.
		if len(reqs) != before+3 {
			t.Fatalf("test/a saw %d requests, want the parent's two and the child's", len(reqs)-before)
		}
		return reqs[before+1]
	}
	if len(reqs) != before+1 {
		t.Fatalf("%s saw %d requests, want the child's one", child, len(reqs)-before)
	}
	return reqs[before]
}

// TestNativePersonaSources (A10): a persona in each of the three sources — the
// workspace chain's .claude/agents, the user root's agents, an enabled
// plugin's agents — named bare, bare and plugin:name, scoped for the harness's
// precedence, with the body as the role, the frontmatter's model and effort,
// and tools mapped to native ids. The chain is innermost first, so a
// subdirectory's persona beats the repository root's of the same name. Then a
// real session offers all three in its agent tool.
func TestNativePersonaSources(t *testing.T) {
	f := contentTree(t,
		map[string]string{
			".claude/agents/proj.md": personaDoc("proj", "the project's reviewer", "tools: Read, Grep", "model: opus", "effort: high"),
		},
		map[string]string{
			".claude/agents/mine.md": personaDoc("", "the user's own"),
		})
	install := installPack(t, f.home, f.workspace, map[string]string{
		"agents/rev.md": personaDoc("rev", "the plugin's reviewer", "tools:", "  - Bash", "  - Glob"),
	})

	got, lines := f.discoverPersonas(ClaudeCompat{})
	wantNoLines(t, lines)
	wantPersonas(t, got, "proj", "mine", "pack:rev")
	want := []harness.Persona{
		{Name: "proj", Description: "the project's reviewer", Model: "opus", Effort: "high",
			Role: "You are proj. Do the task.", Path: filepath.Join(f.workspace, ".claude", "agents", "proj.md"),
			Scope: tool.PersonaProject, Tools: []string{"read", "grep"}},
		// No frontmatter name: the file's base name. No tools key: every tool.
		{Name: "mine", Description: "the user's own", Role: "You are . Do the task.",
			Path: filepath.Join(f.src.UserRoot, "agents", "mine.md"), Scope: tool.PersonaUser, AllTools: true},
		{Name: "pack:rev", Description: "the plugin's reviewer", Role: "You are rev. Do the task.",
			Path: filepath.Join(install, "agents", "rev.md"), Scope: tool.PersonaPlugin, Tools: []string{"bash", "glob"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("personas\n%+v\nwant\n%+v", got, want)
	}

	t.Run("the chain is innermost first", func(t *testing.T) {
		base := t.TempDir()
		repo := writeTree(t, filepath.Join(base, "repo"), map[string]string{
			".claude/agents/dup.md":  personaDoc("dup", "from the root"),
			".claude/agents/root.md": personaDoc("root", "only at the root"),
		})
		gitDir(t, repo)
		sub := writeTree(t, filepath.Join(repo, "service"), map[string]string{
			".claude/agents/dup.md": personaDoc("dup", "from the subdirectory"),
		})
		load := loadNativeContent(resolveNativeSources(sub, ""), ClaudeCompat{}, nil, nil)
		wantPersonas(t, load.personas, "dup", "root")
		if load.personas[0].Description != "from the subdirectory" {
			t.Fatalf("dup is %+v, want the subdirectory's", load.personas[0])
		}
	})

	t.Run("a session offers them", func(t *testing.T) {
		nf := newNativeFixture(t)
		s := startTrees(t, nf, Options{}, f.workspace, f.home)
		types := agentTypesOf(t, s.oneRequest(t, "hi"))
		for _, name := range []string{"proj", "mine", "pack:rev"} {
			if !slices.Contains(types, name) {
				t.Fatalf("the agent tool lists %q, want %q among them", types, name)
			}
		}
	})
}

// TestPersonaPrecedence (A10): project > built-in > user > plugin. A user
// persona named like a built-in — in any case, compared as strings.EqualFold
// compares — is skipped with one line, a project persona of that name shadows
// the built-in, a user persona of a project persona's name loses to it
// silently (first wins in the persona set), and a plugin's is qualified and
// collides with nothing. The session's agent tool then lists them in the
// harness's order.
func TestPersonaPrecedence(t *testing.T) {
	f := contentTree(t,
		map[string]string{
			".claude/agents/explore.md": personaDoc("explore", "the project's explorer"),
			".claude/agents/helper.md":  personaDoc("helper", "the project's helper"),
		},
		map[string]string{
			".claude/agents/helper.md": personaDoc("helper", "the user's helper"),
			".claude/agents/plan.md":   personaDoc("Plan", "the user's planner"),
			".claude/agents/mine.md":   personaDoc("mine", "the user's own"),
		})
	installPack(t, f.home, f.workspace, map[string]string{
		"agents/explore.md": personaDoc("explore", "the plugin's explorer"),
	})

	got, lines := f.discoverPersonas(ClaudeCompat{})
	wantPersonas(t, got, "explore", "helper", "mine", "pack:explore")
	if got[1].Description != "the project's helper" {
		t.Fatalf("helper is %+v, want the project's", got[1])
	}
	if len(lines) != 1 || !strings.Contains(lines[0], `persona "Plan"`) || !strings.Contains(lines[0], `built-in agent type "plan"`) {
		t.Fatalf("diagnostics %q, want one line about the user's Plan", lines)
	}

	nf := newNativeFixture(t)
	s := startTrees(t, nf, Options{}, f.workspace, f.home)
	types := agentTypesOf(t, s.oneRequest(t, "hi"))
	want := []string{"explore", "helper", "general-purpose", "plan", "mine", "pack:explore"}
	if !slices.Equal(types, want) {
		t.Fatalf("the agent tool lists %q, want %q", types, want)
	}
}

// TestPersonaNamespaceSeparate (A10): a command, a skill and a persona of one
// name coexist. The persona is keyed agent:<name> in a set of its own, so it
// takes nothing from the entries and they take nothing from it; the command
// still beats the skill of its name within one source, as before.
func TestPersonaNamespaceSeparate(t *testing.T) {
	f := contentTree(t,
		map[string]string{
			".claude/commands/review.md":     commandFile("the project's command", "body"),
			".claude/skills/review/SKILL.md": skillDoc("review", "the project's skill"),
			".claude/agents/review.md":       personaDoc("review", "the project's persona"),
		},
		map[string]string{
			".claude/skills/review/SKILL.md": skillDoc("review", "the user's skill"),
		})
	installPack(t, f.home, f.workspace, map[string]string{
		"commands/review.md": commandFile("the plugin's command", "body"),
		"agents/review.md":   personaDoc("review", "the plugin's persona"),
	})

	var lines []string
	load := loadNativeContent(f.src, ClaudeCompat{}, nil, func(msg string) { lines = append(lines, msg) })
	wantNoLines(t, lines)
	wantNames(t, load.entries, "project:review", "user:review", "pack:review")
	if load.entries[0].Kind != PluginKindCommand || load.entries[1].Kind != PluginKindSkill || load.entries[2].Kind != PluginKindCommand {
		t.Fatalf("entries %+v, want the project's command, the user's skill and the plugin's command", load.entries)
	}
	wantPersonas(t, load.personas, "review", "pack:review")
	for _, e := range load.entries {
		if e.Kind == PluginKindAgent || strings.Contains(e.Path, string(filepath.Separator)+"agents"+string(filepath.Separator)) {
			t.Fatalf("a persona reached the entries: %+v", e)
		}
	}
	for _, r := range load.extras.Catalog {
		if strings.Contains(r.Path, string(filepath.Separator)+"agents"+string(filepath.Separator)) {
			t.Fatalf("a persona reached the catalog: %+v", r)
		}
	}
}

// TestNativeCatalogUnchangedByAgents (A10): a plugin whose persona is named
// like one of its commands changes neither the menu nor the frozen prompt.
// The same workspace and home are started twice, before and after the
// persona file is written, and the menu rows and the system prompt's bytes
// are compared whole; the second session's agent tool listing the persona is
// the control that it was read at all.
func TestNativeCatalogUnchangedByAgents(t *testing.T) {
	base := t.TempDir()
	ws := writeTree(t, filepath.Join(base, "ws"), map[string]string{
		"CLAUDE.md":                     "Run make lint.\n",
		".claude/commands/ship.md":      commandFile("ship it", "Ship $ARGUMENTS."),
		".claude/skills/build/SKILL.md": skillDoc("build", "build the thing"),
	})
	gitDir(t, ws)
	home := writeTree(t, filepath.Join(base, "home"), nil)
	install := installPack(t, home, physical(t, ws), map[string]string{
		"commands/review.md":   commandFile("review a diff", "Review $ARGUMENTS."),
		"skills/lint/SKILL.md": skillDoc("lint", "lint the tree"),
	})
	src := resolveNativeSources(ws, home)

	f := newNativeFixture(t)
	request := func(s *nativeContent) fantasy.Call {
		t.Helper()
		f.models["test/a"].push(answer("done"))
		if _, err := s.Prompt(context.Background(), "hi"); err != nil {
			t.Fatalf("Prompt: %v", err)
		}
		reqs := f.models["test/a"].requests()
		return reqs[len(reqs)-1]
	}
	before := loadNativeContent(src, ClaudeCompat{}, nil, nil)
	first := startTrees(t, f, Options{}, ws, home)
	firstCall := request(first)

	writeTree(t, install, map[string]string{
		"agents/review.md": personaDoc("review", "a persona named like the command", "tools: Read"),
	})
	after := loadNativeContent(src, ClaudeCompat{}, nil, nil)
	second := startTrees(t, f, Options{}, ws, home)
	secondCall := request(second)

	if !reflect.DeepEqual(before.entries, after.entries) || !reflect.DeepEqual(before.rows, after.rows) ||
		!reflect.DeepEqual(before.extras, after.extras) {
		t.Fatalf("the load moved with a persona:\n before %+v %+v %+v\n after  %+v %+v %+v",
			before.entries, before.rows, before.extras, after.entries, after.rows, after.extras)
	}
	if !reflect.DeepEqual(first.Snapshot().Plugins, second.Snapshot().Plugins) {
		t.Fatalf("the menu moved with a persona:\n before %+v\n after  %+v", first.Snapshot().Plugins, second.Snapshot().Plugins)
	}
	wantDisplays(t, second.Snapshot().Plugins, "ship", "build", "review", "lint")
	if a, b := systemText(t, firstCall), systemText(t, secondCall); a != b {
		t.Fatalf("the frozen prompt moved with a persona:\n before %s\n after  %s", a, b)
	}
	// The control.
	wantPersonas(t, after.personas, "pack:review")
	if types := agentTypesOf(t, secondCall); !slices.Contains(types, "pack:review") {
		t.Fatalf("the second session's agent tool lists %q, want pack:review", types)
	}
}

// TestCursorDiscoveryIgnoresAgentsDir (A10): cursor's scan shares rootEntries
// with native, and a plugin's agents/ changes nothing about it — no entry, and
// not a file read: the persona pass runs only under native's parse option.
// Every one of cursor's sources is exercised: a --plugin-dir, the cursor
// cache and a Claude install.
func TestCursorDiscoveryIgnoresAgentsDir(t *testing.T) {
	home, ws := t.TempDir(), t.TempDir()
	files := map[string]string{
		"commands/review.md":   commandFile("review a diff", "body"),
		"skills/lint/SKILL.md": skillDoc("lint", "lint the tree"),
		"agents/review.md":     personaDoc("review", "a persona"),
		"agents/other.md":      personaDoc("other", "another persona"),
	}
	dir := writeTree(t, filepath.Join(t.TempDir(), "dirpack"), files)
	cachePlugin(t, home, "mkt", "cachepack", "v1", true, files)
	installPack(t, home, ws, files)

	d := newPluginDiscovery(ws, home, nil)
	d.scanSources(cursorScan, []string{dir})
	wantNames(t, d.out, "dirpack:review", "dirpack:lint", "cachepack:review", "cachepack:lint", "pack:review", "pack:lint")
	if d.agents != nil || d.nAgentFiles != 0 || d.nFiles != 6 {
		t.Fatalf("cursor's scan read personas: %d agents, %d persona files, %d files (want 6)", len(d.agents), d.nAgentFiles, d.nFiles)
	}
	// The exported entry point says the same.
	wantNames(t, DiscoverPlugins(cursorScan, ws, home, []string{dir}),
		"dirpack:review", "dirpack:lint", "cachepack:review", "cachepack:lint", "pack:review", "pack:lint")
}

// TestSkillParseUnchangedByAgents (A10): the agent-only reader is a second
// pass of its own; the reader skills and commands share is untouched. Each
// base document below — the shapes skills.go's tests pin — reads to exactly
// what it read to before, pinned absolutely, and reads to the same again with
// every agent-only syntax dropped into it: a tools block list indented and
// not, a flow sequence over several lines, both disallowed spellings, model
// and effort. The entry parsers built on it agree too.
func TestSkillParseUnchangedByAgents(t *testing.T) {
	bases := []struct {
		name string
		fm   string
		want frontmatter
	}{
		{"plain", "name: alpha\ndescription: the alpha skill\n",
			frontmatter{Name: "alpha", Description: "the alpha skill", Invocable: true, ModelInvocable: true}},
		{"quoted", "name: \"beta\"\ndescription: 'the beta skill'\n",
			frontmatter{Name: "beta", Description: "the beta skill", Invocable: true, ModelInvocable: true}},
		{"folded description", "name: gamma\ndescription: >-\n  one line\n  and another\n",
			frontmatter{Name: "gamma", Description: "one line and another", Invocable: true, ModelInvocable: true}},
		{"literal when-to-use", "name: delta\ndescription: d\nwhen-to-use: |\n  first\n  second\n",
			frontmatter{Name: "delta", Description: "d", WhenToUse: "first\nsecond", Invocable: true, ModelInvocable: true}},
		{"hidden and not for the model", "name: eps\nuser-invocable: false # hidden\ndisable-model-invocation: true\n",
			frontmatter{Name: "eps", Invocable: false, ModelInvocable: false}},
		{"nested metadata is not the document's", "name: zeta\nmetadata:\n  user-invocable: false\n  description: nested\n",
			frontmatter{Name: "zeta", Invocable: true, ModelInvocable: true}},
		{"keys read by nothing", "name: eta\nallowed-tools: Bash(git:*)\nmodel: opus\neffort: high\ncontext: fork\n",
			frontmatter{Name: "eta", Invocable: true, ModelInvocable: true}},
		{"CRLF", "name: theta\r\ndescription: crlf\r\n",
			frontmatter{Name: "theta", Description: "crlf", Invocable: true, ModelInvocable: true}},
	}
	inserts := []string{
		"tools: Read, Glob, Agent(a, b)\n",
		"tools:\n  - Read\n  - \"Bash(git status:*)\"\n",
		"tools:\n- Read\n- Grep\n",
		"tools: [\n  Read,\n  Grep\n]\n",
		"disallowedTools: [\"Write\"]\ndisallowed-tools:\n  - Edit\n",
		"model: sonnet\neffort: xhigh\n",
		"color: blue\nskills:\n  - one\ninitialPrompt: go\n",
	}
	for _, b := range bases {
		if got := parseFrontmatterLines(b.fm); got != b.want {
			t.Errorf("%s: parseFrontmatterLines = %+v, want %+v", b.name, got, b.want)
		}
		for _, ins := range inserts {
			for _, doc := range []string{ins + b.fm, b.fm + ins} {
				if got := parseFrontmatterLines(doc); got != b.want {
					t.Errorf("%s with %q: parseFrontmatterLines = %+v, want %+v", b.name, ins, got, b.want)
				}
			}
		}
	}

	// The entry parsers, cursor's reading and native's, over a file with and
	// without the agent keys.
	plain := "---\nname: probe\ndescription: a probe\n---\nbody\n"
	agentish := "---\nname: probe\ntools:\n  - Read\ndescription: a probe\ndisallowedTools: Bash(rm:*)\nmodel: opus\n---\nbody\n"
	for _, opts := range []pluginParseOpts{{}, {Native: true}} {
		for _, parse := range []func(string, []byte, pluginParseOpts) (PluginEntry, bool){parsePluginCommand, parsePluginSkill} {
			a, okA := parse("/x/probe/SKILL.md", []byte(plain), opts)
			b, okB := parse("/x/probe/SKILL.md", []byte(agentish), opts)
			a.Body, b.Body = "", "" // a skill's body is its whole file, frontmatter included
			if okA != okB || a != b {
				t.Errorf("native=%v: %+v (%v) became %+v (%v) with the agent keys", opts.Native, a, okA, b, okB)
			}
		}
	}
	nA, dA, iA, errA := parseSkillMarkdown("/x/probe/SKILL.md", []byte(plain), false)
	nB, dB, iB, errB := parseSkillMarkdown("/x/probe/SKILL.md", []byte(agentish), false)
	if nA != nB || dA != dB || iA != iB || (errA == nil) != (errB == nil) {
		t.Errorf("parseSkillMarkdown moved with the agent keys: %q %q %v %v / %q %q %v %v", nA, dA, iA, errA, nB, dB, iB, errB)
	}
}

// TestPersonaToolsSyntaxes (A10): tools and disallowedTools in each of the
// three syntaxes installed persona files use — a comma string, a flow
// sequence, a block list — with the split respecting parentheses and quotes,
// comments cut, a flow sequence running on over lines, and the keys after a
// list still read.
func TestPersonaToolsSyntaxes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		fm     string
		tools  []string
		denied []string
		model  string
		effort string
	}{
		{name: "a comma string", fm: "tools: Read, Glob, Grep\n", tools: []string{"Read", "Glob", "Grep"}},
		{name: "a quoted comma string", fm: "tools: \"Read, Glob\"\n", tools: []string{"Read", "Glob"}},
		{name: "a comment after it", fm: "tools: Read, Grep  # read-only\n", tools: []string{"Read", "Grep"}},
		{name: "parentheses keep their commas", fm: "tools: Read, Agent(a, b, c), Bash(git status:*)\n",
			tools: []string{"Read", "Agent(a, b, c)", "Bash(git status:*)"}},
		{name: "a real plugin's line", fm: "tools: Glob, Grep, LS, Read, NotebookRead, WebFetch, TodoWrite, WebSearch, KillShell, BashOutput\n",
			tools: []string{"Glob", "Grep", "LS", "Read", "NotebookRead", "WebFetch", "TodoWrite", "WebSearch", "KillShell", "BashOutput"}},
		{name: "a flow sequence", fm: "tools: [\"Read\", \"Grep\"]\n", tools: []string{"Read", "Grep"}},
		{name: "a flow sequence, unquoted, with parentheses", fm: "tools: [Read, Agent(x, y), 'Bash(git diff:*)'] # c\n",
			tools: []string{"Read", "Agent(x, y)", "Bash(git diff:*)"}},
		{name: "a flow sequence over lines", fm: "tools: [\n  Read,  # the reader\n  Grep\n]\nmodel: opus\n",
			tools: []string{"Read", "Grep"}, model: "opus"},
		{name: "a block list", fm: "tools:\n  - Read\n  - \"Grep\"\n  - Bash(git log:*)  # history\nmodel: sonnet\n",
			tools: []string{"Read", "Grep", "Bash(git log:*)"}, model: "sonnet"},
		{name: "a block list at the key's own indentation", fm: "tools:\n- Read\n- Glob\neffort: high\n",
			tools: []string{"Read", "Glob"}, effort: "high"},
		{name: "a block list with blank lines and comments", fm: "tools:\n\n  # the readers\n  - Read\n\n  - Grep\n",
			tools: []string{"Read", "Grep"}},
		{name: "the key in any case", fm: "Tools: Read\n", tools: []string{"Read"}},
		{name: "disallowedTools, block", fm: "disallowedTools:\n  - Write\n  - Bash(rm:*)\n", denied: []string{"Write", "Bash(rm:*)"}},
		{name: "disallowed-tools, comma", fm: "disallowed-tools: Edit, MultiEdit\n", denied: []string{"Edit", "MultiEdit"}},
		{name: "both spellings deny both", fm: "disallowedTools: [Write]\ndisallowed-tools: Edit\n", denied: []string{"Write", "Edit"}},
		{name: "model and effort, quoted and commented", fm: "model: \"inherit\"\neffort: xhigh # max\n", model: "inherit", effort: "xhigh"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := parseAgentFrontmatter(tc.fm)
			if got.HasTools != (tc.tools != nil) || !slices.Equal(got.Tools, tc.tools) ||
				!slices.Equal(got.Disallowed, tc.denied) || got.Model != tc.model || got.Effort != tc.effort {
				t.Fatalf("parseAgentFrontmatter(%q) = %+v, want tools %q (present %v), denied %q, model %q, effort %q",
					tc.fm, got, tc.tools, tc.tools != nil, tc.denied, tc.model, tc.effort)
			}
		})
	}
}

// TestPersonaToolsFailClosed (A10): a tools key that is present but reads to
// nothing — empty, an empty sequence, an empty string, a sequence that never
// closes, a shape the reader does not take — or that maps to nothing gives a
// text-only child (AllTools false, no tools) and one line; only an absent key
// gives every tool. A deny list's restricted name takes its whole tool away
// (tool.MapClaudeDisallowed), so a persona with no tools key and
// disallowedTools: Bash(rm:*) has no bash — pinned through a real child's
// request, beside a text-only child's, which is offered nothing.
func TestPersonaToolsFailClosed(t *testing.T) {
	for _, tc := range []struct {
		name, fm string
		unknown  bool // the unmapped-names line as well
	}{
		{name: "an empty value", fm: "tools:\n"},
		{name: "an empty flow sequence", fm: "tools: []\n"},
		{name: "an empty string", fm: "tools: \"\"\n"},
		{name: "only commas", fm: "tools: ,\n"},
		{name: "a flow sequence that never closes", fm: "tools: [Read, Grep\nmodel: opus\n"},
		{name: "a shape the reader does not take", fm: "tools:\n  Read, Grep\n"},
		{name: "every name dropped", fm: "tools: WebFetch, Agent(x), TodoWrite\n"},
		{name: "every name unknown", fm: "tools: Frobnicate, Bash(git status:*)\n", unknown: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := contentTree(t, map[string]string{".claude/agents/p.md": "---\n" + tc.fm + "---\nrole\n"}, nil)
			got, lines := f.discoverPersonas(ClaudeCompat{})
			wantPersonas(t, got, "p")
			if got[0].AllTools || got[0].Tools != nil {
				t.Fatalf("persona %+v, want a text-only child", got[0])
			}
			want := 1
			if tc.unknown {
				want = 2
			}
			if len(lines) != want || !strings.Contains(lines[len(lines)-1], "runs with no tools") {
				t.Fatalf("diagnostics %q, want %d ending in the text-only line", lines, want)
			}
		})
	}

	t.Run("an absent key is every tool", func(t *testing.T) {
		f := contentTree(t, map[string]string{".claude/agents/p.md": personaDoc("p", "d", "model: opus")}, nil)
		got, lines := f.discoverPersonas(ClaudeCompat{})
		wantNoLines(t, lines)
		if !got[0].AllTools || got[0].Tools != nil {
			t.Fatalf("persona %+v, want every tool", got[0])
		}
	})

	t.Run("a restricted deny takes the whole tool, through a real child", func(t *testing.T) {
		nf := newNativeFixture(t)
		s := startContent(t, nf, Options{}, map[string]string{
			".claude/agents/fenced.md": personaDoc("fenced", "no rm", "disallowedTools: Bash(rm:*)"),
			".claude/agents/mute.md":   personaDoc("mute", "nothing it can read", "tools: []"),
		}, nil)
		if p := s.personasOffered(t); p["fenced"].DisallowedTools == nil || !p["fenced"].AllTools {
			t.Fatalf("fenced is %+v, want every tool less bash", p["fenced"])
		}
		child := s.runAgentCall(t, "test/a", map[string]any{"description": "fenced", "prompt": "look", "subagent_type": "fenced"})
		if names := toolNamesOf(child); slices.Contains(names, "bash") || !slices.Contains(names, "read") {
			t.Fatalf("the fenced child was offered %q, want everything but bash", names)
		}
		child = s.runAgentCall(t, "test/a", map[string]any{"description": "mute", "prompt": "look", "subagent_type": "mute"})
		if names := toolNamesOf(child); len(names) != 0 {
			t.Fatalf("the text-only child was offered %q, want nothing", names)
		}
	})
}

// personasOffered is the personas this session handed the harness, by name, as
// its own discovery reads them again: the harness keeps them to itself.
func (c *nativeContent) personasOffered(t *testing.T) map[string]harness.Persona {
	t.Helper()
	load := loadNativeContent(resolveNativeSources(c.ws, c.home), c.opts.Compat, nil, nil)
	out := make(map[string]harness.Persona, len(load.personas))
	for _, p := range load.personas {
		out[p.Name] = p
	}
	return out
}

// TestCompatAgentsToggle (A10): [compat.claude] agents = false removes every
// persona, from all three sources, the plugin pass included — and reads none
// of their files — while the entries, the menu and the prompt stay exactly
// what they were. plugins = false removes a plugin's personas with everything
// else it ships and leaves the project's and the user's.
func TestCompatAgentsToggle(t *testing.T) {
	f := contentTree(t,
		map[string]string{
			".claude/agents/proj.md":   personaDoc("proj", "d"),
			".claude/commands/ship.md": commandFile("ship it", "body"),
		},
		map[string]string{".claude/agents/mine.md": personaDoc("mine", "d")})
	installPack(t, f.home, f.workspace, map[string]string{
		"agents/rev.md":      personaDoc("rev", "d"),
		"commands/review.md": commandFile("review", "body"),
	})

	on := loadNativeContent(f.src, ClaudeCompat{}, nil, nil)
	wantPersonas(t, on.personas, "proj", "mine", "pack:rev")

	off := loadNativeContent(f.src, ClaudeCompat{NoAgents: true}, nil, nil)
	if off.personas != nil {
		t.Fatalf("agents off still found %q", personaNames(off.personas))
	}
	if !reflect.DeepEqual(on.entries, off.entries) || !reflect.DeepEqual(on.rows, off.rows) || !reflect.DeepEqual(on.extras, off.extras) {
		t.Fatal("agents off moved the entries, the menu or the prompt")
	}
	scan := newNativeScan(ClaudeCompat{NoAgents: true}.sources(f.src), nil)
	scan.run()
	if scan.d.nAgentFiles != 0 || scan.d.parse.Agents != "" {
		t.Fatalf("agents off still read %d persona files (plugin pass %q)", scan.d.nAgentFiles, scan.d.parse.Agents)
	}

	noPlugins := loadNativeContent(f.src, ClaudeCompat{NoPlugins: true}, nil, nil)
	wantPersonas(t, noPlugins.personas, "proj", "mine")

	// Through a session: the agent tool offers the built-ins alone.
	nf := newNativeFixture(t)
	s := startTrees(t, nf, Options{Compat: ClaudeCompat{NoAgents: true}}, f.workspace, f.home)
	if types := agentTypesOf(t, s.oneRequest(t, "hi")); !slices.Equal(types, harness.BuiltinAgentTypes()) {
		t.Fatalf("agents off offers %q, want the built-ins alone", types)
	}
}

// TestNativePersonaSymlinks: a symlink where a persona directory or a persona
// file should be is not followed, exactly as H4 treats one where a command
// directory or a command is — in the chain, under the user root and in a
// plugin — while a user root that is itself a link is followed.
func TestNativePersonaSymlinks(t *testing.T) {
	link := func(t *testing.T, target, at string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(at), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, at); err != nil {
			t.Skipf("symlink: %v", err)
		}
	}
	t.Run("a symlinked agents directory is skipped in every source", func(t *testing.T) {
		f := contentTree(t, map[string]string{"outside/secret.md": personaDoc("secret", "d")}, nil)
		outside := filepath.Join(f.workspace, "outside")
		link(t, outside, filepath.Join(f.workspace, ".claude", "agents"))
		link(t, outside, filepath.Join(f.src.UserRoot, "agents"))
		install := installPack(t, f.home, f.workspace, map[string]string{"commands/c.md": commandFile("c", "body")})
		link(t, outside, filepath.Join(install, "agents"))
		got, lines := f.discoverPersonas(ClaudeCompat{})
		wantNoLines(t, lines)
		wantPersonas(t, got)
	})
	t.Run("a symlinked persona file is skipped", func(t *testing.T) {
		f := contentTree(t, map[string]string{
			"outside/linked.md":      personaDoc("linked", "d"),
			".claude/agents/kept.md": personaDoc("kept", "d"),
		}, nil)
		link(t, filepath.Join(f.workspace, "outside", "linked.md"), filepath.Join(f.workspace, ".claude", "agents", "linked.md"))
		got, _ := f.discoverPersonas(ClaudeCompat{})
		wantPersonas(t, got, "kept")
	})
	t.Run("a symlinked user root is followed", func(t *testing.T) {
		f := contentTree(t, nil, map[string]string{"dotfiles/claude/agents/mine.md": personaDoc("mine", "d")})
		link(t, filepath.Join(f.home, "dotfiles", "claude"), f.src.UserRoot)
		got, _ := f.discoverPersonas(ClaudeCompat{})
		wantPersonas(t, got, "mine")
	})
}

// TestNativePersonaBudget: the persona budget is its own — 128 files and
// 1 MiB across the three sources — and the skills' and commands' is untouched
// by it: a workspace that fills both budgets gets every one of its commands
// and its first 128 personas, and nothing past either. A file too big for the
// bytes left is not read, and costs nothing.
func TestNativePersonaBudget(t *testing.T) {
	t.Run("files", func(t *testing.T) {
		ws := make(map[string]string, maxPluginFiles+maxAgentFiles+2)
		for i := 0; i < maxPluginFiles; i++ {
			ws[fmt.Sprintf(".claude/commands/cmd-%04d.md", i)] = commandFile("d", "body")
		}
		for i := 0; i < maxAgentFiles+2; i++ {
			ws[fmt.Sprintf(".claude/agents/a-%04d.md", i)] = personaDoc(fmt.Sprintf("a-%04d", i), "d")
		}
		f := contentTree(t, ws, map[string]string{".claude/agents/mine.md": personaDoc("mine", "d")})
		scan := newNativeScan(f.src, nil)
		entries := scan.run()
		if len(entries) != maxPluginFiles || scan.d.nFiles != maxPluginFiles {
			t.Fatalf("%d entries from %d files, want every one of the %d commands", len(entries), scan.d.nFiles, maxPluginFiles)
		}
		if len(scan.d.agents) != maxAgentFiles || scan.d.nAgentFiles != maxAgentFiles {
			t.Fatalf("%d personas from %d files, want the budget %d", len(scan.d.agents), scan.d.nAgentFiles, maxAgentFiles)
		}
		for _, a := range scan.d.agents {
			if a.Plugin != nativeProjectID {
				t.Fatalf("%s:%s was read past the budget", a.Plugin, a.Name)
			}
		}
	})
	t.Run("bytes", func(t *testing.T) {
		big := strings.Repeat("a line of the role.\n", (400<<10)/20)
		f := contentTree(t, map[string]string{
			".claude/agents/a.md": personaDoc("a", "d") + big,
			".claude/agents/b.md": personaDoc("b", "d") + big,
			".claude/agents/c.md": personaDoc("c", "d") + big, // past 1 MiB with a and b
			".claude/agents/d.md": personaDoc("d", "d"),       // small enough for what is left
		}, nil)
		scan := newNativeScan(f.src, func(string) {})
		scan.run()
		var names []string
		for _, a := range scan.d.agents {
			names = append(names, a.Name)
		}
		if !slices.Equal(names, []string{"a", "b", "d"}) || scan.d.agentBytes > maxAgentBytes {
			t.Fatalf("personas %q in %d bytes, want a, b and d within %d", names, scan.d.agentBytes, maxAgentBytes)
		}
	})
}

// TestNativePersonaIdentity: a persona's name is its frontmatter name, else
// its file's base name; a name craze could not offer as one token — a colon
// included, so a project file cannot pass for a plugin's plugin:name — is
// refused with a line; an identity holding a provider key drops the persona
// whole, as it drops an entry, with a line (which the session's lane redacts,
// contentWarn); a body past 32 KiB is cut at a line to be the role, with a
// line.
func TestNativePersonaIdentity(t *testing.T) {
	long := strings.Repeat("a line of a very long role.\n", (40<<10)/28)
	f := contentTree(t, map[string]string{
		".claude/agents/by-file.md":              "---\ndescription: d\n---\nrole\n",
		".claude/agents/spaced.md":               personaDoc("my agent", "d"),
		".claude/agents/colon.md":                personaDoc("pack:forged", "d"),
		".claude/agents/" + nativeCanary + ".md": "---\ndescription: d\n---\nrole\n",
		".claude/agents/long.md":                 personaDoc("long", "d") + long,
	}, nil)
	var lines []string
	load := loadNativeContent(f.src, ClaudeCompat{}, []string{nativeCanary}, func(msg string) { lines = append(lines, msg) })
	wantPersonas(t, load.personas, "by-file", "long")
	role := load.personas[1].Role
	if len(role) > maxPersonaRole || !strings.HasSuffix(role, "a line of a very long role.") {
		t.Fatalf("the long role is %d bytes ending %q, want whole lines within %d", len(role), role[len(role)-30:], maxPersonaRole)
	}
	joined := strings.Join(lines, "\n")
	for _, want := range []string{`"my agent" is not a usable name`, `"pack:forged" is not a usable name`,
		"contains a configured provider key", `persona "long"`} {
		if !strings.Contains(joined, want) {
			t.Errorf("diagnostics lack %q:\n%s", want, joined)
		}
	}
}

// TestNativePersonaKeyGateBeforeNameClaim (review r5, finding 5): a project
// persona whose *path* holds a configured provider key must not claim its
// name before the key gate drops it — a valid, lower-precedence persona of
// that same name then stays offered, rather than discovery having already
// discarded it as a duplicate. The reviewer's exact case: a project file
// named after the canary key (so its path holds it), with frontmatter name
// "review", and a clean "review" persona in the user root. Without the fix,
// addAgent's scan-time dedupe let the project file claim "review" first, the
// key gate then dropped it, and the user's "review" was already gone —
// leaving no "review" persona offered at all.
func TestNativePersonaKeyGateBeforeNameClaim(t *testing.T) {
	f := contentTree(t,
		map[string]string{
			".claude/agents/" + nativeCanary + ".md": personaDoc("review", "the project's, but its path holds a key"),
		},
		map[string]string{
			".claude/agents/review.md": personaDoc("review", "the user's own, and clean"),
		})
	got, lines := f.discoverPersonas(ClaudeCompat{}, nativeCanary)
	wantPersonas(t, got, "review")
	if got[0].Description != "the user's own, and clean" {
		t.Fatalf("review is %+v, want the user's", got[0])
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, `"review" not offered`) || !strings.Contains(joined, "contains a configured provider key") {
		t.Fatalf("diagnostics %q, want one line about the dropped project persona", lines)
	}
}

// TestNativePersonaUnmappedNames: names craze cannot map, in either list, are
// one line per persona naming them, and the names it can map still map.
func TestNativePersonaUnmappedNames(t *testing.T) {
	f := contentTree(t, map[string]string{
		".claude/agents/p.md": personaDoc("p", "d", "tools: Read, Frobnicate, Bash(git status:*)", "disallowedTools: Zap, Edit"),
	}, nil)
	got, lines := f.discoverPersonas(ClaudeCompat{})
	if !slices.Equal(got[0].Tools, []string{"read"}) || !slices.Equal(got[0].DisallowedTools, []string{"edit"}) {
		t.Fatalf("persona %+v, want read, less edit", got[0])
	}
	if len(lines) != 1 || !strings.Contains(lines[0], `tools "Frobnicate", "Bash(git status:*)"; disallowedTools "Zap"`) {
		t.Fatalf("diagnostics %q, want one line naming the three", lines)
	}
}

// TestNativeSubagentModelMatched: open() hands the harness the adapter's
// --model normalisation (Options.MatchModel), so a persona's model written as
// a display name in another case reaches that model: the child runs on
// test/b, whose display name is "Model B", and nothing is warned.
func TestNativeSubagentModelMatched(t *testing.T) {
	nf := newNativeFixture(t)
	dir := filepath.Join(t.TempDir(), "journal")
	s := startContent(t, nf, Options{JournalDir: dir}, map[string]string{
		".claude/agents/b.md": personaDoc("b", "on model b", "model: model b"),
	}, nil)
	w := journalOf(t, s.log)
	inc := s.Incarnation()
	s.runAgentCall(t, "test/b", map[string]any{"description": "on b", "prompt": "look", "subagent_type": "b"})
	closeJournaled(t, s, w)
	if notes := diags(assertOneJournal(t, dir, w, inc), diagSubagentWarning); len(notes) != 0 {
		t.Fatalf("a matched model was warned about: %v", notes)
	}
}

// TestNativeSubagentWarnJournaled: open() hands the harness a Warn that
// journals (Options.Warn): a persona model that names nothing falls through
// to the parent's model with one subagent_warning note, redacted — the model
// here is the provider key itself, and the note carries the marker instead.
func TestNativeSubagentWarnJournaled(t *testing.T) {
	nf := newNativeFixture(t)
	dir := filepath.Join(t.TempDir(), "journal")
	s := startContent(t, nf, Options{JournalDir: dir}, map[string]string{
		".claude/agents/lost.md": personaDoc("lost", "a model nobody has", "model: "+nativeCanary),
	}, nil)
	w := journalOf(t, s.log)
	inc := s.Incarnation()
	s.runAgentCall(t, "test/a", map[string]any{"description": "lost", "prompt": "look", "subagent_type": "lost"})
	closeJournaled(t, s, w)
	lines := assertOneJournal(t, dir, w, inc)
	notes := diags(lines, diagSubagentWarning)
	if len(notes) != 1 {
		t.Fatalf("%d subagent_warning notes, want one", len(notes))
	}
	text, _ := notes[0]["text"].(string)
	if !strings.Contains(text, redact.Marker) || !strings.Contains(text, "falling through") {
		t.Fatalf("the note is %q, want the redacted fall-through line", text)
	}
	for _, l := range lines {
		if strings.Contains(fmt.Sprint(l), nativeCanary) {
			t.Fatalf("the journal carries the key: %v", l)
		}
	}
}
