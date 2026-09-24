package harness

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/charliek/craze/internal/harness/tool"
	"github.com/charliek/craze/internal/harness/tool/opencode"
)

// Agent types (plan 026 §3.4): the built-ins, precedence, resolution and what a
// type gives a child. The adapter's discovery and its diagnostics are C4's.

// childTools are the opencode profile's tools a child may have, in its order:
// what an agent type's tools are resolved out of.
var childTools = []string{"bash", "read", "glob", "grep", "edit", "write"}

// testPersonas are one of each shape the list merges: two project personas,
// one of them shadowing a built-in; a user persona shadowing another built-in
// in another case, and one that does not; two plugin personas; and two the
// harness ignores — a scope it does not take, and a name that folds to
// nothing.
func testPersonas() []tool.Persona {
	return []tool.Persona{
		{Name: "reviewer", Description: "Reviews a diff.\nThoroughly.", Tools: []string{"read", "grep"}, Scope: tool.PersonaProject,
			Path: "/w/.claude/agents/reviewer.md"},
		{Name: "explore", Description: "The project's own explorer.", AllTools: true, Scope: tool.PersonaProject},
		{Name: "Plan", Description: "A user's planner, shadowed.", Scope: tool.PersonaUser},
		{Name: "writer", Description: "Writes docs. Key: " + canary, Tools: []string{"write", "read"}, Scope: tool.PersonaUser},
		{Name: "toolkit:tester", Description: "Runs the tests.", AllTools: true, DisallowedTools: []string{"edit", "write"},
			Scope: tool.PersonaPlugin},
		{Name: "toolkit:quiet", Scope: tool.PersonaPlugin},
		{Name: "ghost", Description: "Claims to be built in.", AllTools: true, Scope: tool.PersonaBuiltin},
		{Name: " \n\t", Description: "No name at all.", Scope: tool.PersonaProject},
	}
}

func typeNames(types []tool.Persona) []string {
	out := make([]string, len(types))
	for i, p := range types {
		out[i] = p.Name
	}
	return out
}

// TestBuiltinAgentTypes: the three built-ins, in their order, with §3.4's
// tools — general-purpose every child tool, explore and plan read and search
// only — and roles of their own; BuiltinAgentTypes hands the adapter their
// names in a slice it may keep.
func TestBuiltinAgentTypes(t *testing.T) {
	names := BuiltinAgentTypes()
	equal(t, "the built-ins", names, []string{"general-purpose", "explore", "plan"})
	names[0] = "mutated"
	equal(t, "the built-ins after the caller changed its copy", BuiltinAgentTypes(), []string{"general-purpose", "explore", "plan"})

	for _, b := range builtinTypes {
		all, ids := childToolSet(b, childTools)
		switch b.Name {
		case "general-purpose":
			if !all || ids != nil {
				t.Errorf("general-purpose gives (%v, %q), want every child tool", all, ids)
			}
		default:
			if all || !slices.Equal(ids, []string{"read", "glob", "grep"}) {
				t.Errorf("%s gives (%v, %q), want read, glob and grep alone", b.Name, all, ids)
			}
		}
		if b.Scope != tool.PersonaBuiltin || b.Path != "" || strings.TrimSpace(b.Role) == "" || strings.TrimSpace(b.Description) == "" {
			t.Errorf("built-in %s: %+v", b.Name, b)
		}
	}
}

// TestAgentTypePrecedence: the merged list is project, built-in, user, plugin,
// each name once and the first claim winning by case-insensitive name — so a
// project persona shadows a built-in and a built-in shadows a user persona —
// with a persona of a scope the harness does not take, or with no name, left
// out. A name is folded onto one line, and resolves by the name the list
// shows. The list shares no slice with the caller's personas, and resolution
// shares none with the list.
func TestAgentTypePrecedence(t *testing.T) {
	personas := testPersonas()
	personas = append(personas, tool.Persona{Name: "two\nlines", Scope: tool.PersonaUser})
	types := agentTypes(personas)
	equal(t, "the list", typeNames(types),
		[]string{"reviewer", "explore", "general-purpose", "plan", "writer", "two lines", "toolkit:tester", "toolkit:quiet"})
	if types[1].Description != "The project's own explorer." || types[3].Description != builtinTypes[2].Description {
		t.Fatalf("explore is %q and plan %q; want the project's explore and the built-in plan", types[1].Description, types[3].Description)
	}

	for name, want := range map[string]string{
		"Explore":         "The project's own explorer.",
		" PLAN\n":         builtinTypes[2].Description,
		"":                builtinTypes[0].Description,
		"two lines":       "",
		"TOOLKIT:Tester":  "Runs the tests.",
		"general-purpose": builtinTypes[0].Description,
	} {
		p, err := resolveAgentType(types, name)
		if err != nil || p.Description != want {
			t.Errorf("resolve %q = %q, %v; want %q", name, p.Description, err, want)
		}
	}
	if _, err := resolveAgentType(types, "ghost"); err == nil {
		t.Error("a persona claiming the built-in scope resolved")
	}

	// Names compare as strings.EqualFold does, not by lowering: Σ, σ and ς are
	// one name, so the project's wins whichever the model sends (review r2 of
	// C3a), and the user's spelling of it is not a second type.
	sigma := agentTypes([]tool.Persona{
		{Name: "Σ", Description: "the project's", Scope: tool.PersonaProject},
		{Name: "ς", Description: "the user's", Scope: tool.PersonaUser},
	})
	if n := len(sigma) - len(builtinTypes); n != 1 {
		t.Errorf("Σ and ς are %d types; want one", n)
	}
	for _, name := range []string{"Σ", "σ", "ς"} {
		if p, err := resolveAgentType(sigma, name); err != nil || p.Description != "the project's" {
			t.Errorf("resolve %q = %q, %v; want the project's", name, p.Description, err)
		}
	}

	// No aliasing either way.
	personas[0].Tools[0] = "bash"
	got, _ := resolveAgentType(types, "reviewer")
	equal(t, "the list's copy after the caller changed its persona", got.Tools, []string{"read", "grep"})
	got.Tools[0] = "edit"
	again, _ := resolveAgentType(types, "reviewer")
	equal(t, "the list after a caller changed a resolved persona", again.Tools, []string{"read", "grep"})
}

// TestAgentUnknownType (A5): a type the list does not have is refused before
// any child opens, as invalid_input — the model retries — naming what it sent
// and every type there is, in the order the description lists them. The text
// stays within 1 KiB however many types there are, counting what it left
// out, and shows a name the model sent folded and cut.
func TestAgentUnknownType(t *testing.T) {
	types := agentTypes(testPersonas())
	_, err := resolveAgentType(types, "no-such")
	var ce *callError
	if !errors.As(err, &ce) {
		t.Fatalf("an unknown type = %v, want a *callError", err)
	}
	const want = "Unknown agent type `no-such`. Available types: reviewer, explore, general-purpose, plan, writer, toolkit:tester, toolkit:quiet."
	if res := ce.result(); !res.IsError || res.Class != tool.ClassInvalidInput || res.Text != want {
		t.Fatalf("the refusal = %+v\nwant invalid_input %q", res, want)
	}

	// Past 1 KiB of names: as many as fit, and the count of the rest.
	many := testPersonas()
	for i := range 200 {
		many = append(many, tool.Persona{Name: fmt.Sprintf("toolkit:agent-%03d-with-a-longer-name", i), Scope: tool.PersonaPlugin})
	}
	types = agentTypes(many)
	_, err = resolveAgentType(types, "no-such")
	text := err.Error()
	if len(text) > maxUnknownTypeText || !strings.HasPrefix(text, "Unknown agent type `no-such`. Available types: reviewer, explore, ") {
		t.Fatalf("the refusal is %d bytes (cap %d):\n%s", len(text), maxUnknownTypeText, text)
	}
	list := strings.TrimSuffix(strings.TrimPrefix(text, "Unknown agent type `no-such`. Available types: "), ".")
	head, tail, cut := strings.Cut(list, " (…and ")
	if !cut {
		t.Fatalf("the refusal does not count what it left out:\n%s", text)
	}
	shown, more := strings.Count(head, ", ")+1, 0
	if _, err := fmt.Sscanf(tail, "%d more)", &more); err != nil || shown+more != len(types) {
		t.Fatalf("the refusal shows %d names and counts %d more (%v); want %d in all:\n%s", shown, more, err, len(types), text)
	}

	// A name that is anything the model sent: folded, cut, and still within the cap.
	long := strings.Repeat("x\n", 3000)
	_, err = resolveAgentType(types, long)
	if text := err.Error(); len(text) > maxUnknownTypeText || strings.Contains(text, "\n") ||
		!strings.HasPrefix(text, "Unknown agent type `x x x") || !strings.Contains(text, "…`. Available types: ") {
		t.Fatalf("a long name's refusal (%d bytes):\n%s", len(text), text)
	}
}

// TestPersonaToolMapping (A10): Claude's tool names map to native ids
// case-insensitively, native ids are taken verbatim, the drop list goes
// without a word — Agent(a, b) included — and anything else is reported
// (MapClaudeTools); both results deduplicated in order. Then what the harness
// makes of a persona's mapped lists (childToolSet): no tools key is every
// child tool, disallowedTools is subtracted after mapping, and a list that
// maps to nothing is a text-only child — never every tool.
func TestPersonaToolMapping(t *testing.T) {
	for _, tc := range []struct {
		name     string
		in       []string
		ids, unk []string
	}{
		{"Claude's names", []string{"Read", "Glob", "Grep", "Bash"}, []string{"read", "glob", "grep", "bash"}, nil},
		{"case-insensitive, aliases, deduplicated", []string{"Read", "WRITE", "edit", "MultiEdit", "LS", " grep ", "Glob", "bash", "read"},
			[]string{"read", "write", "edit", "grep", "glob", "bash"}, nil},
		{"native ids verbatim", []string{"bash", "read", "glob", "grep", "edit", "write"}, childTools, nil},
		{"the drop list", []string{
			"Agent", "Agent(claude-security:explore)", "Agent(a, b)", "Task", "TaskCreate", "TaskOutput", "TodoWrite",
			"AskUserQuestion", "ExitPlanMode", "WebFetch", "WebFetch(domain:example.com)", "WebSearch", "NotebookEdit",
			"NotebookRead", "KillShell", "BashOutput", "Workflow", "Skill", "mcp__github__create_issue", "MCP__x",
			"agent", "todo_write", "ask_user_question", "exit_plan_mode",
		}, nil, nil},
		{"a real plugin's list", []string{"Glob", "Grep", "LS", "Read", "NotebookRead", "WebFetch", "TodoWrite", "WebSearch", "KillShell", "BashOutput"},
			[]string{"glob", "grep", "read"}, nil},
		{"unknown names, and restrictions craze cannot honour", []string{"Read", "Frobnicate", "Bash(git status:*)", "Read(./src/**)", "Frobnicate", "", "  "},
			[]string{"read"}, []string{"Frobnicate", "Bash(git status:*)", "Read(./src/**)"}},
		{"nothing", nil, nil, nil},
	} {
		ids, unknown := tool.MapClaudeTools(tc.in)
		if !slices.Equal(ids, tc.ids) || !slices.Equal(unknown, tc.unk) {
			t.Errorf("%s: MapClaudeTools(%q) = %q, %q; want %q, %q", tc.name, tc.in, ids, unknown, tc.ids, tc.unk)
		}
	}

	// Every tool the opencode profile has is in the vocabulary: a child's own
	// id maps to itself, one withheld from every child is dropped, and none is
	// ever reported unknown — so a tool added to the profile fails here until
	// the map knows it.
	p, err := opencode.Profile()
	if err != nil {
		t.Fatal(err)
	}
	every := &ChildOptions{AllTools: true}
	for _, tl := range p.Tools {
		id := tl.Spec().ID
		ids, unknown := tool.MapClaudeTools([]string{id})
		var want []string
		if every.keeps(id) {
			want = []string{id}
		}
		if !slices.Equal(ids, want) || unknown != nil {
			t.Errorf("the profile's %s maps to %q, %q; want %q and nothing unknown", id, ids, unknown, want)
		}
	}

	// A persona's two lists, mapped by the adapter, then resolved here.
	mapped := func(names ...string) []string { ids, _ := tool.MapClaudeTools(names); return ids }
	for _, tc := range []struct {
		name string
		p    tool.Persona
		all  bool
		ids  []string
	}{
		{"no tools key: every tool", tool.Persona{AllTools: true}, true, nil},
		{"no tools key, less the disallowed", tool.Persona{AllTools: true, DisallowedTools: mapped("Edit", "MultiEdit", "Write")},
			false, []string{"bash", "read", "glob", "grep"}},
		{"a list, less the disallowed, in the profile's order",
			tool.Persona{Tools: mapped("Grep", "Read", "Bash", "Write"), DisallowedTools: mapped("bash", "WebFetch")},
			false, []string{"read", "grep", "write"}},
		{"a list that maps to nothing: text-only", tool.Persona{Tools: mapped("WebFetch", "TodoWrite", "Agent(x)")}, false, nil},
		{"everything listed is disallowed: text-only", tool.Persona{Tools: mapped("Read"), DisallowedTools: mapped("LS")}, false, nil},
	} {
		all, ids := childToolSet(tc.p, childTools)
		if all != tc.all || !slices.Equal(ids, tc.ids) {
			t.Errorf("%s: childToolSet = (%v, %q), want (%v, %q)", tc.name, all, ids, tc.all, tc.ids)
		}
	}
}
