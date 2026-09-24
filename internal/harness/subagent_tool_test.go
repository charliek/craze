package harness

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/charliek/craze/internal/harness/modeltable"
	"github.com/charliek/craze/internal/harness/redact"
	"github.com/charliek/craze/internal/harness/tool"
)

// The agent tool's per-session description (plan 026 §3.3, §7 A5). No profile
// registers the agent tool until the runner exists (C3b), so a session here is
// opened on a test profile holding one: the decorator wraps any tool with the
// agent id, and the real tool's static text is pinned in opencode's tests.

// agentProfile is a profile shaped like opencode's with the agent tool in it:
// the six child tools, agent, and a tool a child never gets.
func agentProfile() (*tool.Registry, error) {
	var tools []tool.Tool
	for _, id := range []string{"bash", "read", "glob", "grep", "edit", "write", "agent", "todo_write"} {
		tools = append(tools, &namedTool{id: id})
	}
	var reg tool.Registry
	return &reg, reg.Register(tool.Profile{Name: "opencode", Tools: tools,
		System: func(tool.SystemEnv) string { return "probe prompt\n" }})
}

// specOf is the spec s offers for id.
func specOf(t *testing.T, s *Session, id string) tool.Spec {
	t.Helper()
	for _, sp := range s.tools.specs {
		if sp.ID == id {
			return sp
		}
	}
	t.Fatalf("the session offers no %s tool", id)
	return tool.Spec{}
}

// TestAgentDescriptionTails (A5): a session offers the agent tool with its
// static text and then two sections of its own. The agent types are listed in
// resolution precedence — project, built-in, user, plugin — each with the
// tools it gives a child out of the profile's, so a shadowed built-in or user
// persona is not there; every field is folded onto one line and redacted. The
// models are those with a key, the ones configured for children first, each
// with its efforts and default, then the tier line with the mapped tiers whose
// model has a key. That text is what the model is offered on the wire and
// what the header's tools digest covers — another set of personas is another
// digest — and the parent's alone: a child has no agent tool.
func TestAgentDescriptionTails(t *testing.T) {
	f := newFixture(t, "http://127.0.0.1:1/v1")
	f.table.Subagents = modeltable.Subagents{Model: "other/c", Tiers: map[string]string{"opus": "test/b", "haiku": "nokey/d", "speedy": "other/c"}}
	opts := f.options()
	opts.tools.profiles = agentProfile
	opts.Personas = testPersonas()
	parent := f.open(opts)

	const want = "Does nothing.\n" +
		"\n" +
		"## Agent types\n" +
		"- reviewer: Reviews a diff. Thoroughly. (tools: read, grep)\n" +
		"- explore: The project's own explorer. (tools: bash, read, glob, grep, edit, write)\n" +
		"- general-purpose: A general-purpose agent for multi-step research and code tasks: it searches, reads, runs commands and edits files. (tools: bash, read, glob, grep, edit, write)\n" +
		"- plan: A read-only software architect: studies the code and returns an implementation plan. (tools: read, glob, grep)\n" +
		"- writer: Writes docs. Key: " + redact.Marker + " (tools: read, write)\n" +
		"- toolkit:tester: Runs the tests. (tools: bash, read, glob, grep)\n" +
		"- toolkit:quiet (tools: none)\n" +
		"\n" +
		"## Models\n" +
		"- other/c\n" +
		"- test/b: medium, high; default medium\n" +
		"- test/a (Model A): low, high; default high\n" +
		"Tiers: opus → test/b, speedy → other/c\n"
	if got := specOf(t, parent, "agent").Description; got != want {
		t.Fatalf("the agent tool's description is\n%s\nwant\n%s", got, want)
	}
	if strings.Contains(string(parent.tools.wire), canary) {
		t.Fatal("the key reached the tools the session offers")
	}

	// What the model is offered on the wire is that text.
	a := f.models["test/a"]
	a.push(answerWith("done"))
	run(t, parent, "hello")
	var offered string
	for _, tl := range a.requests()[0].Tools {
		if ft, ok := tl.(fantasy.FunctionTool); ok && ft.Name == "agent" {
			offered = ft.Description
		}
	}
	if offered != want {
		t.Fatalf("the model was offered the agent tool as\n%s\nwant\n%s", offered, want)
	}

	// The header's digest is of the tools with the tail in them, so a session
	// offering other types records another digest.
	sum := sha256.Sum256(parent.tools.wire)
	if h := parent.store.Header().ToolsSHA256; h != hex.EncodeToString(sum[:]) || !strings.Contains(string(parent.tools.wire), "## Agent types") {
		t.Fatalf("tools_sha256 %s does not hash the tools with their tail", h)
	}
	bare := f.options()
	bare.tools.profiles = agentProfile
	other := f.open(bare)
	if other.store.Header().ToolsSHA256 == parent.store.Header().ToolsSHA256 {
		t.Fatal("two sessions offering different agent types record one tools digest")
	}
	if d := specOf(t, other, "agent").Description; !strings.Contains(d, "- general-purpose: ") || strings.Contains(d, "reviewer") {
		t.Fatalf("a session with no personas lists\n%s\nwant the built-ins alone", d)
	}

	// A child is offered no agent tool, so it renders no tail and resolves no
	// type.
	copts := childOf(f, parent, ChildOptions{AllTools: true})
	copts.tools.profiles = agentProfile
	child := f.open(copts)
	for _, sp := range child.tools.specs {
		if sp.ID == "agent" || strings.Contains(sp.Description, "## Agent types") {
			t.Fatalf("the child offers %s with %q", sp.ID, sp.Description)
		}
	}
	if child.tools.types != nil || child.tools.offered != nil {
		t.Fatalf("the child holds agent types %q and child tools %q", typeNames(child.tools.types), child.tools.offered)
	}
	// The parent keeps the tools the description listed a type's out of, for
	// the runner to give the child the same: the profile's, less the four no
	// child gets.
	equal(t, "the parent's child tools", parent.tools.offered, childTools)
}

// TestAgentDescriptionBudgets (A5): each section keeps within its budget
// whatever it is handed — a row at most 300 bytes, the types 8 KiB and the
// models 2 KiB, their headings and the tier line included — cutting from the
// end and saying how many it left out. A long description is cut with an
// ellipsis and its row keeps its name and its tools; a model's long name is
// dropped and its row kept; a row that cannot be read whole even so — a name
// or an alias past the row's budget — is left out and counted. Folding makes a
// field one line; the tier line has its own cap.
func TestAgentDescriptionBudgets(t *testing.T) {
	red := redact.New(canary)
	lines := func(t *testing.T, section string) []string {
		t.Helper()
		out := strings.SplitAfter(section, "\n")
		if out[len(out)-1] != "" {
			t.Fatalf("the section does not end in a newline: %q", section[max(0, len(section)-80):])
		}
		out = out[:len(out)-1]
		for _, l := range out {
			// The tier line is not a row: it has its own cap, checked below.
			if len(l) > maxTailRow && !strings.HasPrefix(l, "Tiers: ") {
				t.Errorf("a row of %d bytes: %q", len(l), l)
			}
		}
		return out
	}
	moreOf := func(t *testing.T, line string) int {
		t.Helper()
		var n int
		if _, err := fmt.Sscanf(line, "… and %d more\n", &n); err != nil {
			t.Fatalf("%q is not the count of what was left out: %v", line, err)
		}
		return n
	}

	t.Run("types", func(t *testing.T) {
		long := strings.Repeat("word ", 200)
		personas := []tool.Persona{
			{Name: "folded", Description: "one\ntwo\t\tthree\x1b[31m", Tools: []string{"read"}, Scope: tool.PersonaProject},
			{Name: "toolkit:" + strings.Repeat("n", maxTailRow), Description: "unreadable", Scope: tool.PersonaPlugin},
		}
		for i := range 400 {
			personas = append(personas, tool.Persona{Name: fmt.Sprintf("toolkit:agent-%03d", i), Description: long, AllTools: true, Scope: tool.PersonaPlugin})
		}
		types := agentTypes(personas)
		section := renderAgentTypes(types, childTools, red)
		if len(section) > maxTypesSection || !strings.HasPrefix(section, agentTypesHeading) {
			t.Fatalf("the types section is %d bytes (budget %d)", len(section), maxTypesSection)
		}
		rows := lines(t, section)
		if rows[1] != "- folded: one two three[31m (tools: read)\n" {
			t.Fatalf("a multi-line description rows as %q", rows[1])
		}
		shown := len(rows) - 2 // the heading and the count
		if n := moreOf(t, rows[len(rows)-1]); shown+n != len(types) {
			t.Fatalf("%d rows shown and %d counted; want all %d types accounted for", shown, n, len(types))
		}
		cut := rows[len(rows)-2]
		if !strings.HasPrefix(cut, "- toolkit:agent-") || !strings.Contains(cut, ": word word ") || !strings.Contains(cut, "…") ||
			!strings.HasSuffix(cut, " (tools: bash, read, glob, grep, edit, write)\n") {
			t.Fatalf("a long description's row is %q; want its name, its description cut, its tools whole", cut)
		}
		if strings.Contains(section, strings.Repeat("n", 50)) {
			t.Fatal("a name longer than a row was listed")
		}

		// Everything fits, but a row that could not be read whole is still counted.
		section = renderAgentTypes(agentTypes(personas[:2]), childTools, red)
		rows = lines(t, section)
		if n := moreOf(t, rows[len(rows)-1]); n != 1 || len(rows) != 1+len(builtinTypes)+1+1 {
			t.Fatalf("with one unreadable name the section is\n%s", section)
		}
	})

	t.Run("models", func(t *testing.T) {
		table := &modeltable.Table{
			DefaultModel: "p/model-000",
			Providers: map[string]modeltable.Provider{
				"p": {Driver: modeltable.DriverOpenAICompat, BaseURL: "http://127.0.0.1:1/v1", APIKey: modeltable.Secret(canary)},
				"q": {Driver: modeltable.DriverOpenAICompat, BaseURL: "http://127.0.0.1:1/v1", EnvKeys: []string{"Q_API_KEY"}},
			},
			Models: map[string]modeltable.Model{
				"p/long-name":                          {Provider: "p", WireModel: "w", Name: strings.Repeat("N", maxTailRow), Efforts: []string{"low"}, DefaultEffort: "low"},
				"p/" + strings.Repeat("a", maxTailRow): {Provider: "p", WireModel: "w"},
				"q/keyless":                            {Provider: "q", WireModel: "w"},
			},
			Subagents: modeltable.Subagents{Tiers: map[string]string{}},
		}
		for i := range 200 {
			alias := fmt.Sprintf("p/model-%03d", i)
			table.Models[alias] = modeltable.Model{Provider: "p", WireModel: "w", Name: fmt.Sprintf("Model %d", i),
				Efforts: []string{"low", "medium", "high"}, DefaultEffort: "medium"}
			if i%10 == 0 {
				table.Subagents.Tiers[fmt.Sprintf("tier-%03d", i)] = alias
			}
		}
		table.Subagents.Tiers["opus"] = "p/model-150"
		table.Subagents.Tiers["haiku"] = "q/keyless"
		getenv := func(string) string { return "" }

		section := renderSubagentModels(table, getenv, red)
		if len(section) > maxModelsSection || !strings.HasPrefix(section, modelsHeading) {
			t.Fatalf("the models section is %d bytes (budget %d)", len(section), maxModelsSection)
		}
		rows := lines(t, section)
		tiers := rows[len(rows)-1]
		if len(tiers) > maxTiersLine || !strings.HasPrefix(tiers, "Tiers: opus → p/model-150, tier-000 → p/model-000, ") ||
			!strings.Contains(tiers, ", … and ") || strings.Contains(tiers, "haiku") {
			t.Fatalf("the tier line (%d bytes) is %q", len(tiers), tiers)
		}
		// The tiers' models first, in the tiers' order, the default among them.
		if rows[1] != "- p/model-150 (Model 150): low, medium, high; default medium\n" ||
			rows[2] != "- p/model-000 (Model 0): low, medium, high; default medium\n" {
			t.Fatalf("the rows open with %q, %q; want the tiers' models first", rows[1], rows[2])
		}
		usable := len(table.Models) - 1 // all but q/keyless
		shown := len(rows) - 3          // the heading, the count, the tier line
		if n := moreOf(t, rows[len(rows)-2]); shown+n != usable {
			t.Fatalf("%d rows shown and %d counted; want all %d models with a key accounted for", shown, n, usable)
		}
		if strings.Contains(section, "keyless") || strings.Contains(section, canary) {
			t.Fatalf("the section lists a model with no key, or the key:\n%s", section)
		}

		// Few enough to show whole: a long name is dropped and its row kept, a
		// long alias is counted, and no tier line without a mapped tier.
		small := *table
		small.Models = map[string]modeltable.Model{
			"p/long-name":                          table.Models["p/long-name"],
			"p/" + strings.Repeat("a", maxTailRow): table.Models["p/"+strings.Repeat("a", maxTailRow)],
			"q/keyless":                            table.Models["q/keyless"],
		}
		small.DefaultModel, small.Subagents = "p/long-name", modeltable.Subagents{}
		if got := renderSubagentModels(&small, getenv, red); got != modelsHeading+"- p/long-name: low; default low\n… and 1 more\n" {
			t.Fatalf("the small section is\n%s", got)
		}
	})
}
