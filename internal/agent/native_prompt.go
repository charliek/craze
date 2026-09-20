package agent

import (
	"crypto/sha256"
	"fmt"
	"strings"

	"github.com/charliek/craze/internal/harness"
	"github.com/charliek/craze/internal/harness/modeltable"
	"github.com/charliek/craze/internal/harness/redact"
)

// What a native session reads off disk before its harness opens, and what it
// hands the harness to freeze into the system prompt (plan 022 §3.4). The scan
// itself is native_content.go's and the instruction loader is instructions.go's;
// this file is the wiring — the catalog projection, the toggles that decide
// which sources are read at all, and the provenance note that says afterwards
// what was read.

// maxNativePromptBytes is the size of frozen prompt above which a session says
// so on its diagnostics lane (§3.4, R1). It is not a cap: the renderer's own
// budgets are the cap, and they sum to more than this. It is the point at
// which the owner should know, because the prompt goes out with *every*
// request of the session and there is no compaction until H7 — 64 KiB of it is
// roughly 16k tokens on every turn.
const maxNativePromptBytes = 64 << 10

// ClaudeCompat is craze's [compat.claude] table as a session carries it: which
// classes of Claude's content this session reads at all.
//
// Every key of that table defaults to true and only a literal `false` removes
// anything (§3.5), so what a session needs to carry is the set the user turned
// *off* — which makes the zero value, and therefore an absent table, a missing
// key and every construction site that does not mention it, craze's default:
// everything on. A struct of positive fields would make the zero value "read
// nothing", and the next caller to build an Options without thinking about it
// would silently drop the user's own instructions out of the prompt.
type ClaudeCompat struct {
	// NoInstructions removes every instruction document, rules included: it is
	// the whole of the prompt's first section. NoRules removes only the rule
	// files, at the user root and at every chain directory, and leaves
	// CLAUDE.md and its companions in place.
	NoInstructions bool
	NoRules        bool
	// NoSkills and NoCommands remove this project's and this user's own
	// entries of that kind — from the menu and from the model's catalog alike,
	// because the two are projections of one discovered list. A plugin's
	// skills and commands are not theirs to remove: NoPlugins governs those,
	// whichever kind they are, since enabling a plugin was one decision about
	// everything it ships.
	NoSkills   bool
	NoCommands bool
	NoPlugins  bool
}

// sources is src with the classes this table turns off taken out of it, which
// is the whole of how four of the five toggles are implemented: contentSources
// already carries "where content lives" as data (§3.1), so removing a class is
// removing its name from the layout rather than threading a flag down into
// every loop that reads one. A loader asked for a directory called "" finds
// nothing (nativeSubdir, readRules), so each of these is exactly one class
// less and nothing else.
//
// NoInstructions is not here: it removes the loader's whole output rather than
// one of its inputs, and loadNativeContent simply does not call it.
func (c ClaudeCompat) sources(src contentSources) contentSources {
	if c.NoRules {
		src.Layout.Rules, src.Layout.UserRules = "", ""
	}
	if c.NoSkills {
		src.Layout.SkillRoots, src.Layout.UserSkills = nil, ""
	}
	if c.NoCommands {
		src.Layout.Commands, src.Layout.UserCommands = "", ""
	}
	if c.NoPlugins {
		// The zero scan is "this provider needs no plugin scan" (PluginScan),
		// which is how every provider that reads no plugin already says so.
		src.Plugins = PluginScan{}
	}
	return src
}

// nativeLoad is everything one session resolved from disk: the entries
// discovery found, hidden ones included; the name each of them resolved to;
// and the extras the harness freezes into the system prompt.
//
// It exists because of an ordering the prompt imposes. The prompt is frozen
// inside harness.Open and can never be added to afterwards (D-30), so every
// file that goes into it has to have been read *before* Open — while the menu,
// the expansion lookup and the redaction of what they carry all need the
// session Open returns. So the reading happens first, in one place, and what
// comes back is assigned to the session afterwards.
type nativeLoad struct {
	entries []PluginEntry
	// rows is ResolvePluginNames over entries, and there is deliberately only
	// one call to it: the catalog's Name is the menu's Display because it is
	// the same row, not because two runs of the resolver agree.
	rows   []PluginCommand
	extras harness.PromptExtras
}

// loadNativeContent is that reading: the scan, the names, the instruction
// documents and the catalog, under the toggles. keys are the provider keys a
// catalog row's path may not hold; warn is craze's own diagnostics lane.
//
// Like everything it calls it is a pure function of src, the toggles and what
// is on disk: no lock, no session state, no failure. A caller runs it before
// the session exists.
func loadNativeContent(src contentSources, c ClaudeCompat, keys []string, warn func(string)) nativeLoad {
	src = c.sources(src)
	content := nativeLoad{}
	content.entries = discoverNative(src, warn)
	// taken is nil because native advertises no commands of its own
	// (ResolvePluginNames adds craze's builtins itself), and provisional is
	// false because there is no catalog still to arrive that could rename a
	// row — and therefore no catalog wait (§3.2).
	content.rows = ResolvePluginNames(content.entries, nil, false)
	if !c.NoInstructions {
		content.extras.Instructions = loadInstructions(src, warn)
	}
	content.extras.Catalog = nativeCatalog(content.entries, content.rows, keys, warn)
	return content
}

// nativeCatalog is the model's projection of that one resolved list, in the
// menu's order: what the model is told exists and where to read it.
//
// It is the second of §3.2's two projections, and it is not the menu's. A
// Hidden entry — user-invocable: false — is out of the menu and unexpandable
// and is *in* here: its author said the user does not invoke it, not that the
// model may not use it. A NoModel entry — disable-model-invocation — is the
// exact opposite and is in the menu only. An entry with no description is left
// out, because a row's whole value to the model is a name it can resolve to a
// file, and a name with nothing said about it is one the model cannot choose
// between. §3.4 words that rule as "a frontmatter description": the test here
// is a description at all, because a skill without one is given one from its
// own first heading (parsePluginSkill) and nothing on a PluginEntry records
// which of the two it was. Telling them apart would mean a new field written
// by cursor's shared parsers, whose bytes PR 1 deliberately froze, to drop a
// row that does say what it is for.
//
// Root is filled for a plugin's entries alone: it is what a plugin's own file
// expands ${CLAUDE_PLUGIN_ROOT} to, and the pseudo ids' roots — this
// project's chain directory, the user's own root — are not that. The renderer
// drops a Root that is not an absolute clean path anyway, so this is about not
// telling the model something that is true of another kind of entry.
func nativeCatalog(entries []PluginEntry, rows []PluginCommand, keys []string, warn func(string)) []harness.CatalogRow {
	if len(rows) == 0 {
		return nil
	}
	if warn == nil {
		warn = func(string) {}
	}
	byKey := make(map[string]PluginEntry, len(entries))
	for _, e := range entries {
		byKey[strings.ToLower(e.Plugin+":"+e.Name)] = e
	}
	// The session's own redactor does not exist yet — it is built inside Open,
	// which is where this catalog is going — so the line below is redacted
	// with a replacer over the same keys, which is the same class the harness
	// builds. It is needed because a row whose *path* holds a key is usually a
	// row whose *name* does too: a skill in a directory called after the key
	// is named after that directory.
	red := redact.New(keys...)
	out := make([]harness.CatalogRow, 0, len(rows))
	for _, r := range rows {
		e, ok := byKey[strings.ToLower(r.Qualified)]
		// Unreachable: every row was resolved from one of these entries. The
		// guard is here because the alternative to skipping is a row whose
		// path came from nowhere.
		if !ok || e.NoModel || strings.TrimSpace(e.Description) == "" {
			continue
		}
		if holdsNativeKey(e.Path, keys) {
			// errWorkspaceKey's reasoning one level down (internal/harness's
			// tools.go): a path with a provider key in it is not something
			// craze can show the model. Redacting it — which is what the
			// renderer does, because it cannot say anything — would offer a
			// file at a path that opens nothing, so the row goes instead, and
			// the line is here because this is the layer that can write one.
			warn(red.String(fmt.Sprintf("%q not listed for the model: its path contains a configured provider key", r.Display)))
			continue
		}
		row := harness.CatalogRow{
			Name:        r.Display,
			Kind:        e.Kind,
			Description: e.Description,
			WhenToUse:   e.WhenToUse,
			Path:        e.Path,
		}
		if !nativePseudoID(e.Plugin) {
			row.Root = e.Root
		}
		out = append(out, row)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// nativePseudoID reports that an entry came from one of native's own two
// sources rather than from a plugin.
func nativePseudoID(id string) bool {
	switch strings.ToLower(id) {
	case nativeProjectID, nativeUserID:
		return true
	}
	return false
}

// redactNativeRows runs the session's redactor over the one field of a menu row
// that is text rather than a name: the description, which a skill with no
// frontmatter description takes from its own body (X13). The names are left
// alone for redactNativeEntries' reason — a redacted name is a row that answers
// to nothing the user could type.
//
// It exists because the rows are resolved before Open, which is where the
// redactor comes from: the catalog has to carry the menu's names into the
// prompt, and the prompt is frozen inside Open. The harness redacts the
// catalog's own copy of these strings as it freezes them; this is the copy the
// menu, the snapshot and the journal see.
func redactNativeRows(rows []PluginCommand, redact func(string) string) []PluginCommand {
	if redact == nil {
		return rows
	}
	for i := range rows {
		rows[i].Description = redact(rows[i].Description)
	}
	return rows
}

// nativeTableKeys is every provider key the model table knows of, which is the
// set harness.Open will build this session's redactor from (openTools). The
// adapter needs them one step earlier than the session exists: a catalog row
// whose path holds one has to be dropped before the prompt is frozen, and
// after Open the prompt cannot be changed.
//
// A table whose keys Open will refuse (one too short to redact) comes back
// empty rather than as an error: Open is called within a few lines and reports
// it properly, and a second phrasing of the same refusal here would be one
// more place for it to be phrased differently.
func nativeTableKeys(table *modeltable.Table, getenv func(string) string) []string {
	if table == nil {
		return nil
	}
	secrets, err := table.Keys(getenv)
	if err != nil {
		return nil
	}
	keys := make([]string, 0, len(secrets))
	for _, s := range secrets {
		if v := s.Reveal(); v != "" {
			keys = append(keys, v)
		}
	}
	return keys
}

// holdsNativeKey reports whether text contains any of keys (harness's
// holdsAKey, which craze may not import from here).
func holdsNativeKey(text string, keys []string) bool {
	for _, k := range keys {
		if strings.Contains(text, k) {
			return true
		}
	}
	return false
}

// promptSourceLine is one element of a prompt_sources class: the SHA-256 of
// what that source put into the prompt, how many bytes of it there were, and
// the file it came from. Three fields on one line rather than three fields of
// their own, because DiagNote takes scalars and []string and collapses a note
// past 64 fields (internal/journal's format.go) — a session with thirty
// commands would otherwise lose the whole note to that rule.
//
// The digest is of what craze contributed, not of the file: an instruction
// document arrives with its imports already inlined, and a catalog row is a
// few fields out of a file the model has not been sent. A reader comparing two
// sessions wants to know whether what went into the prompt changed, which is
// the same question either way.
func promptSourceLine(text, path string) string {
	return fmt.Sprintf("%x %d %s", sha256.Sum256([]byte(text)), len(text), path)
}

// promptSources is the two classes of the prompt_sources note, each element
// through redact. The catalog's digest is over the row's own fields in a fixed
// order, which is everything of it that reaches the model; the separator is a
// newline so no two fields can run together into one digest.
//
// Both slices are allocated even when empty, so the note says "craze looked
// and found nothing" as [] rather than as null.
func promptSources(x harness.PromptExtras, redact func(string) string) (instructions, catalog []string) {
	if redact == nil {
		redact = func(s string) string { return s }
	}
	instructions = make([]string, 0, len(x.Instructions))
	for _, d := range x.Instructions {
		instructions = append(instructions, redact(promptSourceLine(d.Text, d.Path)))
	}
	catalog = make([]string, 0, len(x.Catalog))
	for _, r := range x.Catalog {
		fields := strings.Join([]string{r.Name, r.Kind, r.Description, r.WhenToUse, r.Root}, "\n")
		catalog = append(catalog, redact(promptSourceLine(fields, r.Path)))
	}
	return instructions, catalog
}
