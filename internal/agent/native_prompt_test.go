package agent

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/charliek/craze/internal/harness"
)

// The wiring between what native reads and what the model is sent (plan 022
// §3.4, §3.5): the catalog's projection of the discovered list, the
// [compat.claude] toggles, and the provenance note. The scan itself is
// native_content_test.go's, the instruction loader instructions_test.go's and
// the rendering internal/harness's; these are the rules that live in between.

// catalogOf runs the projection over entries named the way a session names
// them, and hands back both halves of its contract: the rows and every line it
// wrote about one it would not list.
func catalogOf(entries []PluginEntry, keys ...string) ([]harness.CatalogRow, []string) {
	var lines []string
	rows := nativeCatalog(entries, ResolvePluginNames(entries, nil, false), keys,
		func(msg string) { lines = append(lines, msg) })
	return rows, lines
}

// catalogNames is the rows' names, which are the menu's names, in order.
func catalogNames(rows []harness.CatalogRow) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Name)
	}
	return out
}

// joinDocs is the documents' texts on one line, trimmed, for a table that
// cares which files were read rather than what was in them.
func joinDocs(docs []harness.PromptDoc) string {
	out := make([]string, 0, len(docs))
	for _, d := range docs {
		out = append(out, strings.TrimSpace(d.Text))
	}
	return strings.Join(out, "|")
}

func wantCatalog(t *testing.T, rows []harness.CatalogRow, want ...string) {
	t.Helper()
	if got := catalogNames(rows); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("catalog rows %v, want %v", got, want)
	}
}

// described is entryOf with a description, since a row with none is not
// listed at all and every case below is about some other rule.
func described(plugin, name, kind, desc string) PluginEntry {
	e := entryOf(plugin, name, kind, "body of "+name)
	e.Description = desc
	return e
}

// TestNativeCatalogIsListedByDescriptionAndNoModel is the first half of
// §3.4's gating: a row needs something said about it, and an entry its author
// kept from the model is not offered to the model.
func TestNativeCatalogIsListedByDescriptionAndNoModel(t *testing.T) {
	quiet := described("pack", "quiet", PluginKindCommand, "")
	loud := described("pack", "loud", PluginKindCommand, "says a lot")
	noModel := described("pack", "menu-only", PluginKindSkill, "the user's alone")
	noModel.NoModel = true

	rows, lines := catalogOf([]PluginEntry{quiet, loud, noModel})
	wantNoLines(t, lines)
	wantCatalog(t, rows, "loud")

	// The same three through the menu's projection, which is the other way
	// round for two of them: disable-model-invocation leaves a row in the
	// menu, and a description is not what the menu lists on.
	menu := visibleNativeRows([]PluginEntry{quiet, loud, noModel},
		ResolvePluginNames([]PluginEntry{quiet, loud, noModel}, nil, false))
	wantDisplays(t, menu, "quiet", "loud", "menu-only")
}

// TestNativeCatalogListsHiddenEntries is the second half, and the one that
// makes the two projections different rather than one filter: user-invocable:
// false says the user does not type it, not that the model may not read it.
func TestNativeCatalogListsHiddenEntries(t *testing.T) {
	hidden := described("pack", "internal", PluginKindSkill, "used by the other steps")
	hidden.Hidden = true
	open := described("pack", "public", PluginKindSkill, "the one to start with")
	entries := []PluginEntry{hidden, open}

	rows, lines := catalogOf(entries)
	wantNoLines(t, lines)
	wantCatalog(t, rows, "internal", "public")
	// And it is out of the menu, so the two lists really do differ.
	wantDisplays(t, visibleNativeRows(entries, ResolvePluginNames(entries, nil, false)), "public")
}

// TestNativeCatalogNamesAreTheMenusNames: "run /name" in the prompt has to
// mean what a person would type, so a row pushed to its qualified spelling by
// a collision is listed under that spelling and not its bare one.
func TestNativeCatalogNamesAreTheMenusNames(t *testing.T) {
	entries := []PluginEntry{
		described("project", "ship", PluginKindCommand, "the project's"),
		described("user", "ship", PluginKindCommand, "the user's"),
		described("project", "alone", PluginKindCommand, "nobody else claims it"),
	}
	rows, lines := catalogOf(entries)
	wantNoLines(t, lines)
	wantCatalog(t, rows, "project:ship", "user:ship", "alone")
}

// TestNativeCatalogRootIsPluginRowsOnly: Root is what a plugin's own file
// expands ${CLAUDE_PLUGIN_ROOT} to. The pseudo ids have a root of their own —
// this project's chain directory, the user's own root — and it is not that, so
// telling the model it was would be telling it something untrue about every
// file in the repository.
func TestNativeCatalogRootIsPluginRowsOnly(t *testing.T) {
	entries := []PluginEntry{
		described("project", "own", PluginKindCommand, "the project's"),
		described("user", "mine", PluginKindCommand, "the user's"),
		described("pack", "theirs", PluginKindCommand, "a plugin's"),
	}
	rows, lines := catalogOf(entries)
	wantNoLines(t, lines)
	wantCatalog(t, rows, "own", "mine", "theirs")
	for i, want := range []string{"", "", "/plugins/pack"} {
		if rows[i].Root != want {
			t.Fatalf("row %s has Root %q, want %q", rows[i].Name, rows[i].Root, want)
		}
	}
	// Every row still carries the file, which is the only thing that loads it.
	for _, r := range rows {
		if !filepath.IsAbs(r.Path) {
			t.Fatalf("row %s has no absolute path: %q", r.Name, r.Path)
		}
	}
}

// TestNativeCatalogDropsARowWhosePathHoldsAKey is errWorkspaceKey's reasoning
// one level down (internal/harness's tools.go). The renderer would redact such
// a path — it has no way to say anything — and the model would be offered a
// file at a path that opens nothing; the adapter is the layer that can write a
// line instead, so the row goes here.
func TestNativeCatalogDropsARowWhosePathHoldsAKey(t *testing.T) {
	leaky := described("user", "leaky", PluginKindSkill, "in a directory with a key in its name")
	leaky.Path = "/home/" + nativeCanary + "/.claude/skills/leaky/SKILL.md"
	clean := described("user", "clean", PluginKindSkill, "somewhere ordinary")

	rows, lines := catalogOf([]PluginEntry{leaky, clean}, nativeCanary)
	wantCatalog(t, rows, "clean")
	if len(lines) != 1 || !strings.Contains(lines[0], "leaky") {
		t.Fatalf("diagnostics %q, want one naming the row", lines)
	}
	if strings.Contains(strings.Join(lines, "\n"), nativeCanary) {
		t.Fatalf("the diagnostic holds the key itself: %q", lines)
	}
	// With no keys configured there is nothing to match and both are listed:
	// the rule is about this machine's keys, not about the shape of a path.
	rows, lines = catalogOf([]PluginEntry{leaky, clean})
	wantNoLines(t, lines)
	wantCatalog(t, rows, "leaky", "clean")

	// A row whose path holds a key usually has a name that holds it too — a
	// skill in a directory named after the key is named after the directory —
	// so the line itself is redacted before it is written.
	named := described("user", nativeCanary, PluginKindSkill, "named after the key")
	rows, lines = catalogOf([]PluginEntry{named, clean}, nativeCanary)
	wantCatalog(t, rows, "clean")
	if len(lines) != 1 || strings.Contains(lines[0], nativeCanary) {
		t.Fatalf("diagnostics %q, want one with the key redacted out of it", lines)
	}
}

// compatTree is one world for the toggles: a project with an instruction file,
// a rule, a command and a skill; a user root with the same four; and one
// enabled plugin shipping a command. Every toggle of §3.5's matrix has
// something of its own to remove here, and something of everyone else's to
// leave alone.
func compatTree(t *testing.T) contentFixture {
	t.Helper()
	f := contentTree(t,
		map[string]string{
			"CLAUDE.md":                     "the project's instructions\n",
			".claude/rules/style.md":        "the project's rule\n",
			".claude/commands/ship.md":      commandFile("ship it", "ship body"),
			".claude/skills/build/SKILL.md": skillDoc("build", "build it"),
		},
		map[string]string{
			".claude/CLAUDE.md":              "the user's instructions\n",
			".claude/rules/tone.md":          "the user's rule\n",
			".claude/commands/note.md":       commandFile("take a note", "note body"),
			".claude/skills/review/SKILL.md": skillDoc("review", "review it"),
		},
	)
	claudeFixture{
		installs: map[string][]claudeInstall{"pack@mkt": {{
			Scope: "user",
			InstallPath: writeTree(t, filepath.Join(f.home, "install"), map[string]string{
				"commands/deploy.md": commandFile("deploy it", "deploy body"),
			}),
		}}},
		user: map[string]bool{"pack@mkt": true},
	}.write(t, f.home, f.workspace)
	return f
}

// TestCompatToggles walks §3.5's matrix: each toggle removes exactly its row
// and nothing else, from the menu and from the catalog together, and the
// instructions likewise.
func TestCompatToggles(t *testing.T) {
	// What the world above yields with everything on. Every case below is
	// this, less exactly one class.
	const (
		allDocs = "the user's instructions|the user's rule|the project's instructions|the project's rule"
		allRows = "ship,build,note,review,deploy"
	)
	cases := []struct {
		name    string
		compat  ClaudeCompat
		docs    string
		rows    string
		catalog string
	}{
		{"nothing off", ClaudeCompat{}, allDocs, allRows, allRows},
		{
			name:    "instructions",
			compat:  ClaudeCompat{NoInstructions: true},
			docs:    "",
			rows:    allRows,
			catalog: allRows,
		},
		{
			name:    "rules",
			compat:  ClaudeCompat{NoRules: true},
			docs:    "the user's instructions|the project's instructions",
			rows:    allRows,
			catalog: allRows,
		},
		{
			name:    "skills",
			compat:  ClaudeCompat{NoSkills: true},
			docs:    allDocs,
			rows:    "ship,note,deploy",
			catalog: "ship,note,deploy",
		},
		{
			name:    "commands",
			compat:  ClaudeCompat{NoCommands: true},
			docs:    allDocs,
			rows:    "build,review,deploy",
			catalog: "build,review,deploy",
		},
		{
			// A plugin's command goes with the plugins and not with the
			// commands: enabling a plugin was one decision about everything it
			// ships (§3.5).
			name:    "plugins",
			compat:  ClaudeCompat{NoPlugins: true},
			docs:    allDocs,
			rows:    "ship,build,note,review",
			catalog: "ship,build,note,review",
		},
	}
	f := compatTree(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var lines []string
			got := loadNativeContent(f.src, tc.compat, nil, func(m string) { lines = append(lines, m) })
			wantNoLines(t, lines)
			if docs := joinDocs(got.extras.Instructions); docs != tc.docs {
				t.Errorf("documents %q, want %q", docs, tc.docs)
			}
			if rows := strings.Join(displays(visibleNativeRows(got.entries, got.rows)), ","); rows != tc.rows {
				t.Errorf("menu rows %q, want %q", rows, tc.rows)
			}
			if cat := strings.Join(catalogNames(got.extras.Catalog), ","); cat != tc.catalog {
				t.Errorf("catalog rows %q, want %q", cat, tc.catalog)
			}
		})
	}
}

// TestCompatTogglesLeaveTheSourcesAlone: the toggles are applied to a copy of
// the sources, so a session that turned something off cannot change what the
// next resolution of the same contentSources finds. The seam is a value and
// this keeps it one.
func TestCompatTogglesLeaveTheSourcesAlone(t *testing.T) {
	f := compatTree(t)
	before := f.src
	loadNativeContent(f.src, ClaudeCompat{NoRules: true, NoSkills: true, NoCommands: true, NoPlugins: true}, nil, nil)
	if f.src.Layout.Commands != before.Layout.Commands || f.src.Layout.Rules != before.Layout.Rules ||
		len(f.src.Layout.SkillRoots) != len(before.Layout.SkillRoots) || f.src.Plugins != before.Plugins {
		t.Fatalf("the toggles rewrote the caller's sources: %+v", f.src)
	}
	got := loadNativeContent(f.src, ClaudeCompat{}, nil, nil)
	if len(got.extras.Catalog) != 5 {
		t.Fatalf("a second resolution found %d rows, want all five", len(got.extras.Catalog))
	}
}

// TestPromptSourcesShapes pins what the journal note carries: one element per
// document and per row, each "<sha256> <bytes> <path>", and the digest over
// what craze contributed rather than over the file it came from — a document
// arrives with its imports inlined, and a row is a few fields out of a file
// the model has not been sent.
func TestPromptSourcesShapes(t *testing.T) {
	x := harness.PromptExtras{
		Instructions: []harness.PromptDoc{{Path: "/repo/CLAUDE.md", Text: "hello\n"}},
		Catalog:      []harness.CatalogRow{{Name: "ship", Kind: "command", Description: "ship it", Path: "/repo/.claude/commands/ship.md"}},
	}
	docs, rows := promptSources(x, nil)
	if len(docs) != 1 || len(rows) != 1 {
		t.Fatalf("%d documents and %d rows, want one of each", len(docs), len(rows))
	}
	// "hello\n" is six bytes; its digest is the one any sha256 of those bytes
	// gives, so the element is checked whole rather than by its shape.
	const helloSHA = "5891b5b522d5df086d0ff0b110fbd9d21bb4fc7163af34d08286a2e846f6be03"
	if want := helloSHA + " 6 /repo/CLAUDE.md"; docs[0] != want {
		t.Fatalf("document element %q, want %q", docs[0], want)
	}
	if !strings.HasSuffix(rows[0], " /repo/.claude/commands/ship.md") {
		t.Fatalf("row element %q does not end in its path", rows[0])
	}
	// A row's digest is over its fields, so two rows at one path that say
	// different things are two different lines.
	other := x
	other.Catalog = []harness.CatalogRow{{Name: "ship", Kind: "command", Description: "ship it twice", Path: x.Catalog[0].Path}}
	_, otherRows := promptSources(other, nil)
	if otherRows[0] == rows[0] {
		t.Fatal("two rows with different descriptions produced one digest")
	}
	// Empty is [] and not null: "craze looked and found nothing" is a fact
	// worth being able to read out of the note.
	docs, rows = promptSources(harness.PromptExtras{}, nil)
	if docs == nil || rows == nil || len(docs) != 0 || len(rows) != 0 {
		t.Fatalf("empty classes are %v and %v, want two empty slices", docs, rows)
	}
	// And every element goes through the redactor it is handed.
	docs, _ = promptSources(harness.PromptExtras{
		Instructions: []harness.PromptDoc{{Path: "/home/" + nativeCanary + "/CLAUDE.md", Text: "hi\n"}},
	}, func(s string) string { return strings.ReplaceAll(s, nativeCanary, "[redacted]") })
	if strings.Contains(docs[0], nativeCanary) {
		t.Fatalf("the canary reached the note: %q", docs[0])
	}
}
