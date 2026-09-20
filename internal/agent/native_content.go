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
	n := &nativeScan{
		src: src,
		// The workspace the plugin readers are given is the innermost chain
		// directory, which is the physical spelling of the session's own
		// directory: it decides the workspace settings overlay and matches a
		// project-scoped install's projectPath.
		d: newPluginDiscovery(src.workspace(), src.Home, warn),
	}
	n.d.skipID = n.reservedID
	n.scanChain()
	n.scanUserRoot()
	n.d.scanSources(src.Plugins, nil)
	return n.d.out
}

// nativeScan is one run of that: the shared discovery — its file budget, its
// plugin:name dedupe and its accumulating entries — plus the two things only
// native has, where content comes from and which files it has already read.
type nativeScan struct {
	src contentSources
	d   *pluginDiscovery
	// files are the entry files this scan has read, kept for os.SameFile. The
	// plugin:name dedupe cannot stand in for it: a home directory that is also
	// a chain directory — a dotfiles repository, or craze started in ~ — offers
	// every one of the user's skills twice, once as project:name and once as
	// user:name, and those are two different keys. Nor would comparing paths
	// do, because a case-insensitive volume answers to two spellings of one
	// file and a hard link to two names for it.
	files []os.FileInfo
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

// scanChain is source 1: every directory of the chain, innermost first, so a
// command a subdirectory ships beats the repository root's of the same name.
// The chain is outermost-first because that is the order the instruction loader
// wants — a deeper file is read later and wins where two conflict — and
// discovery wants the opposite, so it reads it backwards rather than keeping a
// second copy of the same list in the other order.
func (n *nativeScan) scanChain() {
	for i := len(n.src.Chain) - 1; i >= 0; i-- {
		dir := n.src.Chain[i]
		n.readCommands(nativeSubdir(dir, n.src.Layout.Commands), nativeProjectID, dir)
		for _, rel := range n.src.Layout.SkillRoots {
			n.readSkillDirs(nativeSubdir(dir, rel), nativeProjectID, dir)
		}
	}
}

// nativeSubdir is one of the layout's names under a directory, or "" when
// there is nothing there to read. pluginSubdir refuses a symlink, which is why
// it is used rather than a join: a link where a project's commands belong is a
// tree craze has no reason to read into a prompt. An empty name is refused
// here rather than left to resolve to the directory itself, so a
// contentSources assembled without a layout finds nothing instead of reading
// every markdown file beside the workspace.
func nativeSubdir(dir, rel string) string {
	if dir == "" || rel == "" {
		return ""
	}
	return pluginSubdir(dir, rel)
}

// scanUserRoot is source 2: the user's own commands and skills. Their Root is
// the user root itself, which is what ${CLAUDE_PLUGIN_ROOT} has to mean for a
// file that belongs to no plugin.
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
		data, ok := n.read(path)
		if !ok {
			continue
		}
		if e, parsed := parsePluginCommand(path, data, n.opts()); parsed {
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
	data, ok := n.read(path)
	if !ok {
		return
	}
	if e, parsed := parsePluginSkill(path, data, n.opts()); parsed {
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

// read spends one file of the shared budget on a path this scan has not read
// before. The os.SameFile check is here rather than after parsing because a
// file reached twice should cost the budget once, and because the first source
// to reach it is the one whose id it keeps.
//
// Lstat, not Stat: a symlinked SKILL.md or command file is not read at all,
// which is the convention the plugin and skill walks already hold to.
func (n *nativeScan) read(path string) ([]byte, bool) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return nil, false
	}
	for _, seen := range n.files {
		if os.SameFile(seen, info) {
			return nil, false
		}
	}
	// readFile is the budget and the per-file ceiling, and it stats the path
	// again itself: the file can change between the two calls, which is the
	// same reason it checks the size twice.
	data, ok := n.d.readFile(path)
	if !ok {
		return nil, false
	}
	n.files = append(n.files, info)
	return data, true
}

// opts is native's reading of an entry file, everywhere in this scan: the three
// extra frontmatter fields, a hidden entry kept rather than dropped, and one
// line about a file whose name craze could never offer.
func (n *nativeScan) opts() pluginParseOpts {
	return pluginParseOpts{Native: true, Warn: n.d.warn}
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
