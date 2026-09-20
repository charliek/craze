package agent

import (
	"os"
	"path/filepath"
	"strings"
)

// maxChainDirs bounds the walk up from the workspace looking for a repository
// root, the workspace itself counted as the first of them. A session started in
// a directory that is not in a repository at all would otherwise stat its way
// to "/" — and, worse, a chain reaching that far would load instruction files
// from every level of a home directory. Sixteen is past any checkout the owner
// has and short of anywhere that walk could go wrong.
const maxChainDirs = 16

// contentLayout is Claude's convention for what a project and a user call their
// content, as data: names relative to a directory, never a location. Where
// those directories are is contentSources' business and the owner may move it;
// these names move only if Claude's convention does, which is why they are one
// table in one file rather than a string at each use site.
type contentLayout struct {
	// Instructions are the instruction files read at every directory of the
	// chain, in the order C5 loads them. AGENTS.override.md leads because it
	// replaces AGENTS.md rather than adding to it; that rule is the loader's,
	// the names are the layout's.
	Instructions []string
	// Rules is the directory of rule files at a chain directory, loaded
	// alphabetically after the instruction files (C5).
	Rules string
	// Commands is the flat command directory at a chain directory and
	// SkillRoots the skill directories, each holding one <name>/SKILL.md per
	// skill. Two roots, because .agents/skills is the cross-agent spelling and
	// the owner's repositories use both.
	Commands   string
	SkillRoots []string
	// UserCommands, UserSkills and UserRules are those three again, relative to
	// the user root. They carry no ".claude" prefix because the user root is
	// itself a .claude directory — <home>/.claude today — and an imported tree
	// the owner may point UserRoot at later would mirror that shape rather than
	// nest a second copy of it.
	UserCommands string
	UserSkills   string
	UserRules    string
	// UserInstructions are the instruction files read at the user root, before
	// UserRules and before any chain directory (C5). It is a list for
	// Instructions' reason — Claude's convention may grow another spelling —
	// and it is here rather than a constant at C5's use site because a loader
	// that knew one filename of its own would be a loader the owner could not
	// relocate by changing this table.
	UserInstructions []string
}

// claudeLayout is that convention, the whole of it, in one place.
func claudeLayout() contentLayout {
	return contentLayout{
		Instructions: []string{
			"AGENTS.override.md",
			"AGENTS.md",
			"CLAUDE.md",
			"CLAUDE.local.md",
			filepath.Join(".claude", "CLAUDE.md"),
			filepath.Join(".claude", "CLAUDE.local.md"),
		},
		Rules:    filepath.Join(".claude", "rules"),
		Commands: filepath.Join(".claude", "commands"),
		SkillRoots: []string{
			filepath.Join(".claude", "skills"),
			filepath.Join(".agents", "skills"),
		},
		UserCommands:     "commands",
		UserSkills:       "skills",
		UserRules:        "rules",
		UserInstructions: []string{"CLAUDE.md"},
	}
}

// contentSources is everything the native loaders are told about where content
// comes from, and the only thing: discovery here, and the instruction loader in
// C5, are pure functions of this struct and the filesystem. The owner may point
// UserRoot at an imported tree or Plugins at configured paths later, and that
// has to be a change to resolveNativeSources and its table test alone — a
// loader that went looking for a home directory of its own would make that
// change a hunt through the package.
type contentSources struct {
	// Chain is the repository root down to the workspace, outermost first, in
	// physical paths.
	Chain []string
	// UserRoot is the user's own content directory, <home>/.claude today. Empty
	// means there is no user content to read at all, which is what a session
	// with no home gets and what the relocation test asserts against.
	UserRoot string
	Layout   contentLayout
	// Plugins is which plugin sources the scan runs: Claude's installed and
	// enabled plugins today (decision 5 — no --plugin-dir for native).
	Plugins PluginScan
	// Home is what the plugin readers resolve their caches under, and what "~/"
	// means in a user-level instruction import. It is not where UserRoot comes
	// from once this struct exists: a relocated UserRoot with an empty Home is
	// exactly the shape the relocation test builds.
	Home string
}

// workspace is the session's own directory: the innermost element of the chain.
// The plugin readers need it — the workspace settings overlay decides which
// plugins are enabled here, and a project-scoped install is matched by its
// projectPath — and the chain is the only place native records it, so it is
// read back rather than carried twice and allowed to disagree with itself.
func (s contentSources) workspace() string {
	if len(s.Chain) == 0 {
		return ""
	}
	return s.Chain[len(s.Chain)-1]
}

// resolveNativeSources answers the question no loader downstream asks itself:
// where content lives on this machine. workspace is the session's directory and
// home the user's; an empty home is a session with no user content rather than
// one that falls back to the process's own environment, because the caller
// isolating a test home is the same caller that would have to isolate this.
func resolveNativeSources(workspace, home string) contentSources {
	home = strings.TrimSpace(home)
	src := contentSources{
		Chain:   repoChain(workspace),
		Layout:  claudeLayout(),
		Plugins: PluginScan{ClaudePlugins: true},
		Home:    home,
	}
	if home != "" {
		src.UserRoot = filepath.Join(home, ".claude")
	}
	return src
}

// repoChain is the workspace and every directory up to the repository root that
// holds it, outermost first. Content is read from each of them, so a repository
// whose root is two directories up contributes its .claude/commands to a
// session started in a subdirectory, which is what makes "run the skill from
// wherever I happen to be" work.
//
// The walk stops at the *nearest* .git, which means a submodule or a linked
// worktree stops at its own: a superproject's instruction files are not this
// repository's, and loading them would put another project's rules in front of
// the model (§4, deliberate). When there is no .git within maxChainDirs, or the
// filesystem root arrives first, the chain is the workspace alone — a directory
// that is not in a checkout gets its own content and nothing above it. So is a
// workspace that does not resolve, and that one is not walked at all; see
// below.
func repoChain(workspace string) []string {
	start, resolved := physicalPath(workspace)
	if start == "" {
		return nil
	}
	if !resolved {
		// The workspace is not there, or a component of it is unreadable or a
		// symlink loop. Its ancestors are then not this session's ancestors in
		// any sense craze can stand behind: walking the cleaned spelling would
		// load /repo's instruction files, commands and skills for a session
		// opened on /repo/missing, which is another project's content in front
		// of the model on the strength of a path that resolved to nothing. The
		// spelling is still returned rather than nothing, because a chain of
		// one is still a session and refusing to open one over this would not
		// be.
		return []string{start}
	}
	// up holds the directories examined so far, innermost first, so the moment
	// a .git turns up the chain is that slice read backwards.
	up := []string{start}
	dir := start
	for i := 0; i < maxChainDirs; i++ {
		if hasGitEntry(dir) {
			out := make([]string, 0, len(up))
			for j := len(up) - 1; j >= 0; j-- {
				out = append(out, up[j])
			}
			return out
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
		up = append(up, dir)
	}
	return []string{start}
}

// physicalPath is the workspace as the filesystem knows it, and whether the
// filesystem knew it at all. EvalSymlinks resolves every component, so a
// checkout reached through a symlinked path walks up its real ancestors — the
// link's parent is not the repository — and the os.SameFile de-duplication in
// the discovery below compares like with like. A path that cannot be resolved,
// because it is not there yet or a component is unreadable or links to itself,
// comes back as its cleaned absolute spelling with false: the caller may still
// open a session on that spelling, but it has no ancestors it may climb.
func physicalPath(path string) (string, bool) {
	abs := absOrSelf(path)
	if abs == "" {
		return "", false
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real, true
	}
	return abs, false
}

// hasGitEntry reports that dir is the top of a checkout. Lstat, not Stat, and
// the mode is not looked at: a normal clone has a .git directory, a submodule
// and a linked worktree have a .git *file* holding a gitdir: line, and a .git
// symlink — including one pointing nowhere any more — is still the marker its
// owner meant. All three stop the walk, because all three mean "this is where
// the project begins".
func hasGitEntry(dir string) bool {
	_, err := os.Lstat(filepath.Join(dir, ".git"))
	return err == nil
}
