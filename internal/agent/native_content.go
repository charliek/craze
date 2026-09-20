package agent

import (
	"os"
	"path/filepath"
	"strings"
)

// The pseudo plugin ids native's own two sources go under. Neither is a plugin:
// a project's .claude/commands has no manifest and no install record. But
// everything downstream is keyed by a plugin id — the dedupe key, the qualified
// spelling the menu draws and the prompt scanner matches, the sentence that
// says where an expanded block came from — so each source needs one, and these
// are the words a person would use for them.
//
// They are reserved: a real plugin whose id is either of these is skipped
// whole, because two different things answering to "project:" would make the
// menu's spelling a lie about which file a name expands.
const (
	nativeProjectID = "project"
	nativeUserID    = "user"
)

// userSyncedSkills is the one directory under the user's skills native does not
// read. Claude's own app syncs its default skills into
// skills/synced/<snapshot>/<name>/SKILL.md and keeps more than one snapshot
// around — two of them, holding 17 SKILL.md files, on the owner's machine — so
// walking it offers every one of those skills twice, under two versions of
// itself, and spends the file budget doing it.
const userSyncedSkills = "synced"

// vendorDefaultSkills are the skills that arrive with Claude rather than being
// written by the user or the project. They are grok-build's denylist, and they
// are dropped only where the path says they came from Claude — see
// hasClaudeSegment — so a project that deliberately ships a skill of its own
// called "pdf" keeps it.
//
// The denylist is native's own two sources only; a plugin is filtered by
// nothing here. Installing a plugin and enabling it is a decision about the
// skills it ships, whatever they are called, and Claude's own defaults do not
// arrive that way: on this machine they live under the user root's
// skills/synced, which this scan skips whole. Since a plugin installs under
// <home>/.claude/plugins, the path test below would match every one of them,
// so applying the list there could only ever drop somebody's deliberate work.
var vendorDefaultSkills = map[string]bool{
	"pdf":           true,
	"docx":          true,
	"xlsx":          true,
	"pptx":          true,
	"skill-creator": true,
}

// discoverNative is the whole content catalog of a native session: the
// project's commands and skills, then the user's own, then the plugins Claude
// has installed and enabled. Three sources, first winning on the plugin:name
// key, and within each of them commands before skills — the rule rootEntries
// already applies inside one plugin, for the same reason: somebody who wrote
// both under one name meant the command.
//
// Like DiscoverPlugins it never fails. A half-written directory, a file that
// cannot be read, a settings file that is not JSON: each contributes nothing
// and, where craze can say something useful about it, one line through warn. A
// session is better off without a skill than refusing to start over one.
//
// It takes no lock and touches no session state: it is a pure function of src
// and what is on disk, so a caller can run it before the session exists and
// assign the result afterwards.
func discoverNative(src contentSources, warn func(string)) []PluginEntry {
	return newNativeScan(src, warn).run()
}

// redactNativeEntries runs the session's redactor over the fields of a
// discovered entry that are carried rather than used, and returns entries. The
// expansion path redacts the block it sends (nativeBlock), which is the body
// and nothing else; these two travel by another road entirely — Description
// becomes PluginCommand.Description, which is Snapshot.Plugins, the slash menu
// and the EventCommand record the lossless codec writes to the journal, and
// WhenToUse is the model-facing catalog's second line.
//
// A SKILL.md with no frontmatter description is given one from its body
// (skillHeadingDescription), so a file whose first heading is a provider key
// would put that key in every one of those places while the block it expands
// to stayed clean. A8 names a catalog description explicitly.
//
// Name is deliberately left alone: it is what the menu draws, what the lookup
// is keyed by and what the user types, and a redacted one would be a row that
// answers to nothing. Path too — here it is a filesystem handle, the thing
// ${CLAUDE_SKILL_DIR} is derived from — and it is redacted where it is
// *recorded* instead (nativePrompt's ExpandedCommand.Path). Body is redacted
// at expansion time, after substitution, which is the first point at which
// what goes out is known.
//
// It is native's alone, and it runs at Start rather than inside discovery:
// cursor's rows are what cursor's own loader would offer, byte for byte, and
// cursor has no harness redactor to run anything through.
func redactNativeEntries(entries []PluginEntry, redact func(string) string) []PluginEntry {
	if redact == nil {
		return entries
	}
	for i := range entries {
		entries[i].Description = redact(entries[i].Description)
		entries[i].WhenToUse = redact(entries[i].WhenToUse)
	}
	return entries
}

// newNativeScan and run are discoverNative in two halves, so that a test can
// hold the scan and read what it spent as well as what it found: the file
// budget is shared across all three sources, and "one inode costs one file"
// is not visible in the entries alone.
func newNativeScan(src contentSources, warn func(string)) *nativeScan {
	n := &nativeScan{
		src: src,
		// The workspace the plugin readers are given is the innermost chain
		// directory, which is the physical spelling of the session's own
		// directory: it decides the workspace settings overlay and matches a
		// project-scoped install's projectPath.
		d: newPluginDiscovery(src.workspace(), src.Home, warn),
	}
	n.d.skipID = n.reservedID
	// Native's reading of an entry file, for every source of this run, plugins
	// included: the three extra frontmatter fields, a hidden entry kept rather
	// than dropped, and one line about a file whose name craze could never
	// offer. The plugins are the bulk of the owner's content, and C6's catalog
	// draws WhenToUse and NoModel from exactly those rows.
	n.d.parse = pluginParseOpts{Native: true, Warn: n.d.warn}
	// And the os.SameFile de-duplication, which therefore covers the plugin
	// source too: a plugin command hard-linked to a project command, or one
	// file exposed by two plugin roots, is one entry that costs one file.
	n.d.dedupeFiles = true
	return n
}

func (n *nativeScan) run() []PluginEntry {
	n.scanChain()
	n.scanUserRoot()
	n.d.scanSources(n.src.Plugins, nil)
	return n.d.out
}

// nativeScan is one run of that: the shared discovery — its file budget, its
// plugin:name dedupe, its os.SameFile list and its accumulating entries — plus
// the one thing only native has, where content comes from.
type nativeScan struct {
	src contentSources
	d   *pluginDiscovery
}

// reservedID is the veto the plugin sources run against. It writes its own line
// because a plugin that vanishes without one is indistinguishable from a plugin
// that is not installed.
func (n *nativeScan) reservedID(id string) bool {
	switch strings.ToLower(id) {
	case nativeProjectID, nativeUserID:
		n.d.note("plugin %q skipped: craze means this project's and your own content by that name", id)
		return true
	}
	return false
}

// scanChain is source 1: the whole chain, which is one source and is ordered
// as one. Every command directory first, innermost first, and only then every
// skill root, innermost first — not a directory at a time. §3.2's rule is
// "within every source, commands before skills", and a per-directory loop
// would break it across the chain: a skill called foo three directories down
// would beat the repository root's command of that name, so which of the two a
// typed /foo meant would depend on where in the checkout craze was started.
// Within one directory .claude/skills still precedes .agents/skills, the
// layout's own order.
//
// The chain is outermost-first because that is the order the instruction loader
// wants — a deeper file is read later and wins where two conflict — and
// discovery wants the opposite, so both loops read it backwards rather than
// keeping a second copy of the same list in the other order.
func (n *nativeScan) scanChain() {
	for i := len(n.src.Chain) - 1; i >= 0; i-- {
		dir := n.src.Chain[i]
		n.readCommands(nativeSubdir(dir, n.src.Layout.Commands), nativeProjectID, dir)
	}
	for i := len(n.src.Chain) - 1; i >= 0; i-- {
		dir := n.src.Chain[i]
		for _, rel := range n.src.Layout.SkillRoots {
			n.readSkillDirs(nativeSubdir(dir, rel), nativeProjectID, dir)
		}
	}
}

// nativeSubdir is one of the layout's names under a directory, or "" when
// there is nothing there to read. pluginSubdir refuses a symlink, which is why
// it is used rather than a join: a link where a project's commands belong is a
// tree craze has no reason to read into a prompt.
//
// What is refused is a symlink *inside* a root, never a root itself. A chain
// directory and the user root are where the owner said their content is —
// UserRoot is configuration, and a ~/.claude that is a link into a dotfiles
// checkout is the ordinary arrangement — so those are followed, exactly as
// cursor follows a --plugin-dir the user pointed at a link while refusing a
// link within the plugin (scanDirs, pluginSubdir).
//
// An empty name is refused here rather than left to resolve to the directory
// itself, so a contentSources assembled without a layout finds nothing instead
// of reading every markdown file beside the workspace.
func nativeSubdir(dir, rel string) string {
	if dir == "" || rel == "" {
		return ""
	}
	return pluginSubdir(dir, rel)
}

// scanUserRoot is source 2: the user's own commands and skills. Their Root is
// the user root itself, which is what ${CLAUDE_PLUGIN_ROOT} has to mean for a
// file that belongs to no plugin. The root is used as it is given, symlink or
// not — see nativeSubdir — and only what is under it is filtered.
func (n *nativeScan) scanUserRoot() {
	root := n.src.UserRoot
	if root == "" {
		return
	}
	n.readCommands(nativeSubdir(root, n.src.Layout.UserCommands), nativeUserID, root)
	n.walkUserSkills(nativeSubdir(root, n.src.Layout.UserSkills), 0)
}

// readCommands reads one flat command directory: commands/*.md and nothing
// below it. A nested commands/<topic>/*.md is not read, which is grok-build's
// rule and matches the one installed plugin on this machine that has such a
// directory — what it keeps there is a .toml, not a command.
func (n *nativeScan) readCommands(dir, id, root string) {
	if dir == "" {
		return
	}
	for _, name := range pluginMarkdownFiles(dir) {
		path := filepath.Join(dir, name)
		data, ok := n.d.readFile(path)
		if !ok {
			continue
		}
		if e, parsed := parsePluginCommand(path, data, n.d.parse); parsed {
			n.add(e, id, root)
		}
	}
}

// readSkillDirs is the flat skill layout: <root>/<name>/SKILL.md, one level, as
// a plugin's own skills are read. childDirs skips a symlinked directory for
// pluginSubdir's reason.
func (n *nativeScan) readSkillDirs(dir, id, root string) {
	if dir == "" {
		return
	}
	for _, sub := range childDirs(dir) {
		n.readSkill(filepath.Join(dir, sub, "SKILL.md"), id, root)
	}
}

// walkUserSkills is the one recursive walk native does. The user's own skills
// are filed by hand — a directory per topic, holding a directory per skill —
// where a project's sit one level under .claude/skills, so the user root is the
// only place a nested SKILL.md is normal. depth is the walk's bound and is
// maxSkillDepth for the reason the provider skill walk has it: the tree below
// belongs to nobody in particular.
func (n *nativeScan) walkUserSkills(dir string, depth int) {
	if dir == "" {
		return
	}
	for _, e := range readDirCapped(dir) {
		// A symlink is skipped whichever it points at: the walk stays inside
		// the user root, and a link out of it would read a tree the user never
		// meant as a skill.
		if e.Type()&os.ModeSymlink != 0 {
			continue
		}
		switch {
		case e.IsDir():
			if depth == 0 && e.Name() == userSyncedSkills {
				continue
			}
			if depth < maxSkillDepth {
				n.walkUserSkills(filepath.Join(dir, e.Name()), depth+1)
			}
		case e.Name() == "SKILL.md":
			n.readSkill(filepath.Join(dir, e.Name()), nativeUserID, n.src.UserRoot)
		}
	}
}

// readSkill is one SKILL.md, wherever the two walks above found it. Both go
// through here so that a skill's identity — frontmatter name first, the
// directory basename as the fallback, grok's rule — is decided in one place.
func (n *nativeScan) readSkill(path, id, root string) {
	data, ok := n.d.readFile(path)
	if !ok {
		return
	}
	if e, parsed := parsePluginSkill(path, data, n.d.parse); parsed {
		n.add(e, id, root)
	}
}

// add finishes one entry: the vendor denylist, then the id and the root it was
// found under, then the dedupe every source shares.
func (n *nativeScan) add(e PluginEntry, id, root string) {
	if e.Kind == PluginKindSkill && vendorDefaultSkills[strings.ToLower(e.Name)] && hasClaudeSegment(e.Path) {
		return
	}
	e.Plugin, e.Root = id, root
	n.d.addEntry(e)
}

// hasClaudeSegment reports that a path passes through a .claude directory,
// which is where Claude puts what it installed rather than what the user wrote.
// The vendor denylist turns on it: the same five names under .agents/skills, or
// inside a plugin, are somebody's own work and are kept.
func hasClaudeSegment(path string) bool {
	for _, part := range strings.Split(filepath.ToSlash(filepath.Clean(path)), "/") {
		if part == ".claude" {
			return true
		}
	}
	return false
}
