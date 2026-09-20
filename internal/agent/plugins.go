package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Plugin content is budgeted like the skill walk: the same file count and the
// same per-file ceiling, so a cache nobody pruned cannot stall a session start.
const (
	maxPluginFiles = maxSkillFiles
	maxPluginBytes = maxSkillBytes
	// maxPluginDirEntries bounds one directory listing. The file budget above
	// only limits what is read, not what is enumerated: a cache holding a
	// million plugin directories would still be walked, and every entry of it
	// allocated, before the first file was opened.
	maxPluginDirEntries = 4096
)

// PluginKindCommand and PluginKindSkill are the two shapes a plugin ships. They
// differ in how they are named, described and — from C2 on — expanded, so the
// kind travels with the entry rather than being inferred from its path.
const (
	PluginKindCommand = "command"
	PluginKindSkill   = "skill"
)

// PluginEntry is one command or skill a plugin ships, found on disk by the
// provider's rules. Plugin is the plugin id (the cache directory, the key
// before "@" in installed_plugins.json, or the manifest name), never the
// marketplace. Kind is PluginKindCommand or PluginKindSkill. Path is the
// markdown file, Root the plugin's install directory (what
// ${CLAUDE_PLUGIN_ROOT} means).
//
// The last three are read off the frontmatter under the native parse option
// only (§3.2) and are the zero value for every provider that loads its own
// content: cursor's rows are what cursor's loader would offer, and a field
// cursor never fills cannot change one.
type PluginEntry struct {
	Plugin, Name, Description, Body, Path, Root, Kind string
	// WhenToUse is the frontmatter's when-to-use, the sentence that says when
	// an entry applies rather than what it is. The model's catalog draws it
	// under the description; the menu has no room for it.
	WhenToUse string
	// Hidden is user-invocable: false. The entry still takes part in naming, so
	// a visible row's spelling does not depend on what is hidden, and it is
	// still offered to the model; it is the menu and the typed /name that leave
	// it out.
	Hidden bool
	// NoModel is disable-model-invocation: true, which is the opposite
	// projection — the entry stays in the menu for the user and is kept out of
	// the model's catalog.
	NoModel bool
}

// PluginScan is where a provider's plugin content comes from. The zero value
// means the provider needs no scan and ignores Options.PluginDirs: grok
// advertises every plugin skill over ACP and expands it itself, so craze
// walking a cache on its behalf could only produce rows the agent would not
// honour. Cursor's ACP server never loads a plugin at all, which is why
// everything below exists.
type PluginScan struct {
	Dirs          bool // honour Options.PluginDirs
	CursorCache   bool // ~/.cursor/plugins/cache/<mkt>/<plugin>/<ver>/ with .cache-complete
	ClaudePlugins bool // ~/.claude/plugins/installed_plugins.json ∩ enabledPlugins
}

// DiscoverPlugins is the provider-owned plugin catalog: the commands and skills
// the provider's own loader would find on this machine, in the provider's
// source order, deduped by the qualified key plugin:name with the first source
// winning. workspace is the session's directory (relative dirs resolve against
// it and the Claude project scope is compared to it), home the directory the
// caches live under.
//
// It never fails. A cache directory that is half-written, a settings file that
// is not JSON and a --plugin-dir that is not there all contribute nothing; a
// session is better off without a plugin than refusing to start over one.
func DiscoverPlugins(scan PluginScan, workspace, home string, dirs []string) []PluginEntry {
	return discoverPlugins(scan, workspace, home, dirs, nil)
}

// discoverPlugins is DiscoverPlugins with somewhere to put the one diagnostic
// the design asks for (§3.1: a --plugin-dir that is not a directory is a line,
// not an error). The exported signature is pinned and carries no writer, so the
// session passes its own sink through here instead.
func discoverPlugins(scan PluginScan, workspace, home string, dirs []string, warn func(string)) []PluginEntry {
	d := newPluginDiscovery(workspace, home, warn)
	d.scanSources(scan, dirs)
	return d.out
}

// newPluginDiscovery starts one scan. It is separate from the source loop below
// because native's discovery (native_content.go) reads its own two sources on
// the same run before handing it the plugin sources: one budget, one dedupe and
// one entry list across all three is the whole point.
func newPluginDiscovery(workspace, home string, warn func(string)) *pluginDiscovery {
	return &pluginDiscovery{
		workspace: absOrSelf(workspace),
		home:      strings.TrimSpace(home),
		warn:      warn,
		seen:      make(map[string]struct{}),
	}
}

// scanSources runs the sources a PluginScan asks for, in the order that decides
// who wins a qualified key.
func (d *pluginDiscovery) scanSources(scan PluginScan, dirs []string) {
	if scan.Dirs {
		d.scanDirs(dirs)
	}
	if scan.CursorCache {
		d.scanCursorCache()
	}
	if scan.ClaudePlugins {
		d.scanClaudePlugins()
	}
}

// pluginDiscovery is one run of the scan: the budget, the qualified-key dedupe
// and the accumulating entries, so each source is a method that only has to
// name plugin roots.
type pluginDiscovery struct {
	workspace string
	home      string
	warn      func(string)
	// skipID, when set, vetoes a plugin id before its root is read, and is
	// where the veto writes its own line. Native reserves "project" and "user"
	// for the pseudo plugins its workspace and user content go under (§3.2): a
	// real plugin shipping either id would otherwise take entries under a
	// spelling the menu and the expansion path already mean something else by.
	skipID func(id string) bool
	// parse is how every entry file of this run is read. The zero value is
	// cursor's reading and is what newPluginDiscovery leaves here, so a
	// provider scan is byte-identical; native sets its own before any source
	// runs, because a plugin's entries go into the same list as its workspace's
	// and its user's and have to answer the same questions — a Hidden skill
	// kept rather than dropped, WhenToUse and NoModel filled for the
	// model-facing catalog, one diagnostic for a file whose name craze could
	// never offer. Three sources parsed two different ways would be one list
	// whose rows meant different things depending on where they came from.
	parse pluginParseOpts
	// dedupeFiles turns on the os.SameFile check in readFile, and files is what
	// that check has seen. It is off for cursor, whose budget accounting and
	// results are a promise craze has already made, and on for native, whose
	// three sources can reach one inode by several routes: a home directory
	// that is also a chain directory offers every user skill twice, under two
	// different plugin:name keys, and a hard link — or two plugin roots
	// exposing one file — does the same inside a single source. A path compare
	// would miss both, and a case-insensitive volume besides.
	dedupeFiles bool
	files       []os.FileInfo
	seen        map[string]struct{}
	nFiles      int
	out         []PluginEntry
}

func (d *pluginDiscovery) note(format string, args ...any) {
	if d.warn == nil {
		return
	}
	d.warn(fmt.Sprintf(format, args...))
}

// addPlugin reads one plugin root under the id it is known by. The qualified
// key is what dedupes, so the same plugin id reached through two marketplaces —
// or through a --plugin-dir and a cache — contributes its entries once, and a
// command beats a skill of the same name because rootEntries offers it first.
func (d *pluginDiscovery) addPlugin(id, root string) {
	if !pluginIDOK(id) || (d.skipID != nil && d.skipID(id)) {
		return
	}
	for _, e := range d.rootEntries(id, root) {
		d.addEntry(e)
	}
}

// addEntry is that dedupe on its own, for the sources that name their entries
// themselves rather than reading a plugin root: native's project and user
// content arrives entry by entry and has to land in the same list, under the
// same key, as the plugins scanned after it.
func (d *pluginDiscovery) addEntry(e PluginEntry) {
	key := strings.ToLower(e.Plugin + ":" + e.Name)
	if _, ok := d.seen[key]; ok {
		return
	}
	d.seen[key] = struct{}{}
	d.out = append(d.out, e)
}

// scanDirs is source 1: the directories the user named, in the order given.
// A relative path is the workspace's, as cursor-agent's own --plugin-dir is.
func (d *pluginDiscovery) scanDirs(dirs []string) {
	seenDir := make(map[string]struct{}, len(dirs))
	for _, dir := range dirs {
		if strings.TrimSpace(dir) == "" {
			continue
		}
		path := dir
		if !filepath.IsAbs(path) {
			path = filepath.Join(d.workspace, path)
		}
		path = filepath.Clean(path)
		if _, ok := seenDir[path]; ok {
			continue
		}
		seenDir[path] = struct{}{}
		// os.Stat, not Lstat: a --plugin-dir the user pointed at a symlink is
		// the directory they meant. Symlinks *inside* it are still skipped.
		if info, err := os.Stat(path); err != nil || !info.IsDir() {
			d.note("plugin dir skipped: %s", path)
			continue
		}
		d.addPlugin(pluginDirID(path), path)
	}
}

// scanCursorCache is source 2: the marketplace plugins cursor materialised
// under ~/.cursor/plugins/cache. The .cache-complete sentinel is the proxy for
// "the install finished", which is the closest thing on disk to enablement: a
// directory left behind by a failed install has no sentinel and no version.
func (d *pluginDiscovery) scanCursorCache() {
	if d.home == "" {
		return
	}
	cache := filepath.Join(d.home, ".cursor", "plugins", "cache")
	for _, market := range childDirs(cache) {
		for _, plugin := range childDirs(filepath.Join(cache, market)) {
			root, ok := completeVersionDir(filepath.Join(cache, market, plugin))
			if !ok {
				continue
			}
			d.addPlugin(plugin, root)
		}
	}
}

// scanClaudePlugins is source 3: Claude Code's own installed plugins,
// intersected with what the settings enable. cursor-agent reads the same two
// files and craze follows its rule exactly (dl in the bundle), including that
// an absent key is not enabled.
func (d *pluginDiscovery) scanClaudePlugins() {
	if d.home == "" {
		return
	}
	installed := readInstalledPlugins(filepath.Join(d.home, ".claude", "plugins", "installed_plugins.json"))
	if len(installed) == 0 {
		return
	}
	user := readEnabledPlugins(filepath.Join(d.home, ".claude", "settings.json"))
	// The workspace's two files overlay in this order, later winning per key,
	// which is what makes settings.local.json the developer's own last word.
	ws := make(map[string]bool)
	for _, name := range []string{"settings.json", "settings.local.json"} {
		maps.Copy(ws, readEnabledPlugins(filepath.Join(d.workspace, ".claude", name)))
	}
	for _, key := range sortedPluginKeys(installed) {
		entry, ok := resolveClaudePlugin(installed[key], key, ws, user, d.workspace)
		if !ok {
			continue
		}
		d.addPlugin(claudePluginID(key), entry)
	}
}

// pluginDirID is the id a --plugin-dir goes by: the manifest name when the
// directory declares one, the basename otherwise. Both manifest spellings are
// read because cursor accepts both.
func pluginDirID(root string) string {
	for _, rel := range []string{
		filepath.Join(".cursor-plugin", "plugin.json"),
		filepath.Join(".claude-plugin", "plugin.json"),
	} {
		data, ok := readCappedFile(filepath.Join(root, rel))
		if !ok {
			continue
		}
		var manifest struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(data, &manifest) != nil {
			continue
		}
		if name := strings.TrimSpace(manifest.Name); name != "" {
			return name
		}
	}
	return filepath.Base(root)
}

// completeVersionDir picks the version of a cached plugin craze reads: the one
// with the newest mtime among those carrying the .cache-complete sentinel. An
// exact tie goes to the lexically first name, so the choice is deterministic
// rather than whatever the filesystem happened to list first. A plugin with no
// complete version — an empty directory from a failed install — has none.
func completeVersionDir(pluginDir string) (string, bool) {
	best := ""
	var bestMod int64
	for _, version := range childDirs(pluginDir) {
		dir := filepath.Join(pluginDir, version)
		if _, err := os.Stat(filepath.Join(dir, ".cache-complete")); err != nil {
			continue
		}
		info, err := os.Stat(dir)
		if err != nil {
			continue
		}
		mod := info.ModTime().UnixNano()
		if best == "" || mod > bestMod {
			best, bestMod = dir, mod
		}
	}
	return best, best != ""
}

// childDirs lists the directory entries of dir that are directories, in
// lexical order. It names both the cached versions of a plugin and the skill
// directories inside one. Symlinks are not followed (a directory entry reports
// the link itself, so IsDir is false for one): a link inside a cache is not a
// version craze has any reason to trust.
func childDirs(dir string) []string {
	ents := readDirCapped(dir)
	out := make([]string, 0, len(ents))
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		out = append(out, e.Name())
	}
	return out
}

// readDirCapped lists at most maxPluginDirEntries of a directory craze does not
// own, in lexical order. os.ReadDir would read and sort every child first, so a
// cache directory with a million entries would be craze's allocation rather
// than the filesystem's; entries past the cap are dropped, which is the shape
// every other budget here has. File.ReadDir does not sort, so this does.
func readDirCapped(dir string) []os.DirEntry {
	if dir == "" {
		return nil
	}
	f, err := os.Open(dir)
	if err != nil {
		return nil
	}
	defer f.Close()
	ents, err := f.ReadDir(maxPluginDirEntries)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil
	}
	sort.Slice(ents, func(i, j int) bool { return ents[i].Name() < ents[j].Name() })
	return ents
}

// readCappedFile reads a file craze does not own, refusing anything past the
// per-file ceiling without allocating it first. os.ReadFile sizes its buffer
// from the stat and only then hands back something to reject, which turns a
// junk cache entry into craze's memory problem.
func readCappedFile(path string) ([]byte, bool) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxPluginBytes {
		return nil, false
	}
	data, err := io.ReadAll(io.LimitReader(f, maxPluginBytes+1))
	if err != nil || len(data) > maxPluginBytes {
		return nil, false
	}
	return data, true
}

// claudeInstall is one install record of installed_plugins.json. Scope is
// user, project or local; ProjectPath is set for the latter two.
type claudeInstall struct {
	Scope       string `json:"scope"`
	InstallPath string `json:"installPath"`
	ProjectPath string `json:"projectPath"`
}

// readInstalledPlugins reads Claude Code's install database. The current file
// wraps the map in {"version": 2, "plugins": {...}}; an older or newer shape
// that is just the map is read too, and anything else contributes nothing.
func readInstalledPlugins(path string) map[string][]claudeInstall {
	data, ok := readCappedFile(path)
	if !ok {
		return nil
	}
	var wrapped struct {
		Plugins map[string][]claudeInstall `json:"plugins"`
	}
	if json.Unmarshal(data, &wrapped) == nil && wrapped.Plugins != nil {
		return wrapped.Plugins
	}
	var flat map[string][]claudeInstall
	if json.Unmarshal(data, &flat) != nil {
		return nil
	}
	return flat
}

// readEnabledPlugins reads one settings file's enabledPlugins map. The keys are
// the full "<id>@<marketplace>" spelling installed_plugins.json uses. A missing
// or malformed file is no opinion at all, which under the dl rule means not
// enabled rather than disabled.
func readEnabledPlugins(path string) map[string]bool {
	data, ok := readCappedFile(path)
	if !ok {
		return nil
	}
	var settings struct {
		EnabledPlugins map[string]bool `json:"enabledPlugins"`
	}
	if json.Unmarshal(data, &settings) != nil {
		return nil
	}
	return settings.EnabledPlugins
}

// sortedPluginKeys orders the install database by plugin id, then by the full
// key. The file is a JSON object and Go's map order is not an order, so without
// this the source order — which decides who wins a name collision — would
// change from run to run.
func sortedPluginKeys(installed map[string][]claudeInstall) []string {
	keys := make([]string, 0, len(installed))
	for k := range installed {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := claudePluginID(keys[i]), claudePluginID(keys[j])
		if a != b {
			return a < b
		}
		return keys[i] < keys[j]
	})
	return keys
}

// claudePluginID is the plugin id inside an install key: everything before the
// last "@", which separates the id from its marketplace.
func claudePluginID(key string) string {
	if at := strings.LastIndex(key, "@"); at > 0 {
		return key[:at]
	}
	return key
}

// resolveClaudePlugin is cursor's dl rule, spelled out: the workspace settings
// decide first — false skips outright, true takes the project- or local-scoped
// install whose projectPath is this workspace — and only a key the workspace
// says nothing about falls through to the user settings and the user-scoped
// install. A key neither file mentions is not enabled.
func resolveClaudePlugin(installs []claudeInstall, key string, ws, user map[string]bool, workspace string) (string, bool) {
	if on, ok := ws[key]; ok {
		if !on {
			return "", false
		}
		for _, in := range installs {
			if (in.Scope == "project" || in.Scope == "local") &&
				in.ProjectPath != "" && samePath(in.ProjectPath, workspace) {
				return installDir(in.InstallPath)
			}
		}
		return "", false
	}
	if !user[key] {
		return "", false
	}
	for _, in := range installs {
		if in.Scope == "user" {
			return installDir(in.InstallPath)
		}
	}
	return "", false
}

// installDir accepts an installPath as it is written. It has to be absolute —
// a relative one would be resolved against whatever craze's cwd happens to
// be — and it has to be there.
func installDir(path string) (string, bool) {
	path = strings.TrimSpace(path)
	if path == "" || !filepath.IsAbs(path) {
		return "", false
	}
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return "", false
	}
	return filepath.Clean(path), true
}

func samePath(a, b string) bool {
	return filepath.Clean(a) == filepath.Clean(b)
}

func absOrSelf(path string) string {
	if strings.TrimSpace(path) == "" {
		return ""
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	return abs
}

// rootEntries reads one plugin's conventional layout: commands/*.md at the top
// level, then skills/*/SKILL.md one level down, each in lexical order. The
// commands go first so that addPlugin's dedupe, the only one there is, resolves
// a command and a skill of one name the way cursor's loader does. The files are
// read under the run's own parse option, never a fresh zero value: which
// reading a plugin gets is the scan's decision, not this function's.
func (d *pluginDiscovery) rootEntries(id, root string) []PluginEntry {
	var out []PluginEntry
	add := func(e PluginEntry, ok bool) {
		if !ok {
			return
		}
		e.Plugin, e.Root = id, root
		out = append(out, e)
	}
	commands := pluginSubdir(root, "commands")
	for _, name := range pluginMarkdownFiles(commands) {
		path := filepath.Join(commands, name)
		if data, ok := d.readFile(path); ok {
			add(parsePluginCommand(path, data, d.parse))
		}
	}
	skills := pluginSubdir(root, "skills")
	for _, dir := range childDirs(skills) {
		path := filepath.Join(skills, dir, "SKILL.md")
		if data, ok := d.readFile(path); ok {
			add(parsePluginSkill(path, data, d.parse))
		}
	}
	return out
}

// pluginParseOpts is the whole difference between the two readings of one entry
// file. The zero value is what every provider that loads its own content gets,
// and it has to stay exactly what it was: cursor's rows are craze's promise
// about what cursor's loader would offer, so a skill its frontmatter hides is
// dropped here as cursor drops it, and a name craze cannot offer goes without a
// word because cursor says nothing either.
type pluginParseOpts struct {
	// Native fills PluginEntry's three extra fields and keeps a hidden entry
	// rather than dropping it: native's menu and its model-facing catalog are
	// two projections of one list (§3.2), and only the list's owner can decide
	// which of them an entry belongs in.
	Native bool
	// Warn, when set, takes one line for each file refused for its name. It is
	// native's alone: a project whose skill directory is called "my skill"
	// would otherwise wonder why the menu is short.
	Warn func(string)
}

// badName is that line. The name is quoted because the interesting ones are
// invisible — a trailing space, a non-breaking space, a colon someone meant as
// a plugin qualifier.
func (o pluginParseOpts) badName(path, name string) {
	if o.Warn == nil {
		return
	}
	o.Warn(fmt.Sprintf("skipped %s: %q is not a usable name", path, name))
}

// fill copies the fields only native reads. sanitizeText, not sanitizeLine:
// when-to-use is written as a literal block often enough that folding it here
// would lose the author's line breaks, and the one place that cannot take a
// newline — the prompt's catalog — folds every field itself.
func (o pluginParseOpts) fill(e *PluginEntry, meta frontmatter) {
	if !o.Native {
		return
	}
	e.WhenToUse = sanitizeText(strings.TrimSpace(meta.WhenToUse))
	e.Hidden = !meta.Invocable
	e.NoModel = !meta.ModelInvocable
}

// pluginSubdir is a directory of the conventional layout, or "" when the plugin
// does not have it. A symlink is refused here even though a --plugin-dir that
// is itself a symlink is followed: that link is the user pointing at a plugin,
// while this one is content of a cache craze does not own, and following it
// would read a tree outside the plugin into a prompt.
func pluginSubdir(root, name string) string {
	dir := filepath.Join(root, name)
	if info, err := os.Lstat(dir); err != nil || !info.IsDir() {
		return ""
	}
	return dir
}

// pluginMarkdownFiles lists the regular .md files directly inside dir, in
// lexical order. Subdirectories, symlinks and other extensions are out of
// scope (§4: the conventional layout only).
func pluginMarkdownFiles(dir string) []string {
	ents := readDirCapped(dir)
	out := make([]string, 0, len(ents))
	for _, e := range ents {
		if e.IsDir() || e.Type()&os.ModeSymlink != 0 {
			continue
		}
		if !strings.EqualFold(filepath.Ext(e.Name()), ".md") {
			continue
		}
		out = append(out, e.Name())
	}
	return out
}

// readFile spends one file of the scan's budget and returns the contents of a
// regular file within the per-file ceiling. The size is checked twice — once
// from the stat and once from what was read — because the file can grow in
// between.
//
// Lstat, not Stat: a symlinked entry file is not read at all, the convention
// every walk feeding this holds to. When dedupeFiles is on, an inode this run
// has already read is refused here rather than after parsing, so a file reached
// twice costs the budget once and the first source to reach it is the one whose
// id it keeps.
func (d *pluginDiscovery) readFile(path string) ([]byte, bool) {
	if d.nFiles >= maxPluginFiles {
		return nil, false
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxPluginBytes {
		return nil, false
	}
	if d.dedupeFiles {
		for _, seen := range d.files {
			if os.SameFile(seen, info) {
				return nil, false
			}
		}
	}
	d.nFiles++
	data, ok := readCappedFile(path)
	if !ok {
		return nil, false
	}
	if d.dedupeFiles {
		// Recorded from the stat above, not a second one: readCappedFile stats
		// the open file itself, and what matters here is the inode the walk
		// named, so that a file which changed underneath is still not read
		// twice under two names.
		d.files = append(d.files, info)
	}
	return data, true
}

// parsePluginCommand reads a commands/*.md the way cursor's own plugin command
// loader does: the frontmatter name wins and the file stem is the fallback, the
// description is the frontmatter's alone, and the content is the body after the
// frontmatter. argument-hint is deliberately not read (§4).
func parsePluginCommand(path string, data []byte, opts pluginParseOpts) (PluginEntry, bool) {
	text := strings.TrimPrefix(string(data), "\ufeff")
	fm, body, _, err := splitFrontmatter(text)
	if err != nil {
		return PluginEntry{}, false
	}
	// A file with no frontmatter has an empty block, which reads as no keys at
	// all, so the fallbacks below are the whole of its identity.
	meta := parseFrontmatterLines(fm)
	name := strings.TrimSpace(meta.Name)
	if name == "" {
		base := filepath.Base(path)
		name = strings.TrimSuffix(base, filepath.Ext(base))
	}
	if !pluginNameOK(name) {
		opts.badName(path, name)
		return PluginEntry{}, false
	}
	body = strings.TrimSpace(body)
	if body == "" {
		return PluginEntry{}, false
	}
	e := PluginEntry{
		Name:        name,
		Description: sanitizeText(meta.Description),
		Body:        body,
		Path:        path,
		Kind:        PluginKindCommand,
	}
	opts.fill(&e, meta)
	return e, true
}

// parsePluginSkill reads a plugin's skills/<dir>/SKILL.md. It is deliberately
// not parseSkillMarkdown: a *plugin* skill is named by its frontmatter first
// and its directory only as a fallback — the opposite of the project-skill rule
// cursor uses and craze verified in plan 009 — its description falls back to
// the first heading and then the first body line, and its content is the whole
// file, frontmatter included, because that is what cursor attaches.
//
// The name is normalised the way cursor's plugin loader normalises it
// (lowercased, whitespace runs folded to "-") and then has to pass the strict
// identifier class, because a name the prompt scanner could never match is a
// menu row that does nothing.
func parsePluginSkill(path string, data []byte, opts pluginParseOpts) (PluginEntry, bool) {
	text := strings.TrimPrefix(string(data), "\ufeff")
	fm, body, _, err := splitFrontmatter(text)
	if err != nil {
		return PluginEntry{}, false
	}
	meta := parseFrontmatterLines(fm)
	// Cursor's loader drops a skill its frontmatter hides, so craze drops it
	// too and the menu matches what cursor would offer. Native keeps it as a
	// Hidden entry instead: it stays out of the menu there as well, but it
	// takes part in naming and it is still listed to the model (§3.2).
	if !meta.Invocable && !opts.Native {
		return PluginEntry{}, false
	}
	name := strings.TrimSpace(meta.Name)
	if name == "" {
		name = filepath.Base(filepath.Dir(path))
	}
	name = normalizePluginName(name)
	if !pluginNameOK(name) {
		opts.badName(path, name)
		return PluginEntry{}, false
	}
	desc := meta.Description
	if strings.TrimSpace(desc) == "" {
		desc = skillHeadingDescription(body)
	}
	whole := strings.TrimSpace(text)
	if whole == "" {
		return PluginEntry{}, false
	}
	e := PluginEntry{
		Name:        name,
		Description: sanitizeText(strings.TrimSpace(desc)),
		Body:        whole,
		Path:        path,
		Kind:        PluginKindSkill,
	}
	opts.fill(&e, meta)
	return e, true
}

// skillHeadingDescription is the description a plugin skill gets when its
// frontmatter has none: the first "#" heading with its hashes stripped, else
// the first non-empty line of the body.
func skillHeadingDescription(body string) string {
	remaining := body
	for remaining != "" {
		line, next, found := strings.Cut(remaining, "\n")
		if trimmed := trimLine(line); strings.HasPrefix(trimmed, "#") {
			return strings.TrimSpace(strings.TrimLeft(trimmed, "#"))
		}
		if !found {
			break
		}
		remaining = next
	}
	return firstBodyLine(body)
}

// normalizePluginName is cursor's plugin-skill name normalisation as far as the
// minified bundle showed it: lowercase, with whitespace runs folded to a single
// "-". Every installed skill is already kebab-case, so the rule only decides
// what happens to one nobody has installed.
func normalizePluginName(name string) string {
	return strings.Join(strings.Fields(strings.ToLower(name)), "-")
}

// pluginNameOK is the one identifier class plugin ids, entry names and the
// prompt scanner all share: cursor's own command-name class, [A-Za-z0-9_-]+.
// It is deliberately stricter than skillNameOK, which allows "." and Unicode: a
// name the scanner cannot match is a row whose Enter would send plain text.
func pluginNameOK(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		if !isPluginNameByte(name[i]) {
			return false
		}
	}
	return true
}

// isPluginNameByte is that class one byte at a time, which is how the prompt
// scanner reads it (scanNameRun). It lives here rather than beside the scanner
// so the rule the discovery side admits a name by and the rule the wire side
// matches one by cannot drift: a name only one of them accepts is a menu row
// whose Enter sends plain text.
func isPluginNameByte(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	case c == '_' || c == '-':
		return true
	}
	return false
}

// pluginIDOK is the same class for a plugin id. A plugin whose id fails it is
// skipped whole: its qualified spelling plugin:name could never be typed, so
// every entry it ships would be unreachable the moment it collided.
func pluginIDOK(id string) bool { return pluginNameOK(id) }

// BuiltinSlashNames are craze's own slash commands, the names the TUI answers
// itself. They live here so the session resolving plugin names and the menu
// drawing them agree on what is already spoken for: a plugin shipping a
// command called "help" has to show as plugin:help everywhere, because a bare
// row would be shadowed by the builtin in the menu and expanded by craze in
// `craze prompt` — the same name meaning two things.
//
// internal/tui/slash.go's builtinSlash is the other half of this list, with the
// descriptions the menu draws; the two are pinned to each other by a test.
func BuiltinSlashNames() []string {
	return []string{"help", "model", "clear", "tasks", "theme", "rename", "plan", "ask", "agent", "exit"}
}

// PluginCommand is a plugin entry as the menu and the JSON stream see it. Bare
// is the entry's own name, Qualified is plugin:name, Display is the spelling
// the menu shows — Bare when it is unambiguous, else Qualified.
type PluginCommand struct {
	Plugin, Bare, Display, Qualified, Description, Kind string
}

// ResolvePluginNames is grok's naming rule applied to the entries craze owns: a
// bare name when no other entry claims it and nothing else already means it,
// the qualified plugin:name otherwise, and nothing at all when both spellings
// are spoken for. taken is what the agent advertised over ACP; the builtins
// join it here rather than at the call site so every caller gets the same
// answer.
//
// provisional holds from Start until the agent's first available_commands_update
// has been applied, and makes every row qualified. Cursor's catalog lands
// seconds after session/new, and a bare /simplify accepted in that window would
// silently change meaning when the agent's own simplify arrives; the qualified
// spelling cannot.
func ResolvePluginNames(entries []PluginEntry, taken []string, provisional bool) []PluginCommand {
	if len(entries) == 0 {
		return nil
	}
	claimed := make(map[string]bool, len(taken)+len(entries))
	for _, n := range taken {
		if n = strings.TrimSpace(n); n != "" {
			claimed[strings.ToLower(n)] = true
		}
	}
	for _, n := range BuiltinSlashNames() {
		claimed[n] = true
	}
	bareCount := make(map[string]int, len(entries))
	for _, e := range entries {
		bareCount[strings.ToLower(e.Name)]++
	}
	out := make([]PluginCommand, 0, len(entries))
	for _, e := range entries {
		qualified := e.Plugin + ":" + e.Name
		bare := strings.ToLower(e.Name)
		display := ""
		switch {
		case !provisional && bareCount[bare] == 1 && !claimed[bare]:
			display = e.Name
		case !claimed[strings.ToLower(qualified)]:
			display = qualified
		default:
			continue
		}
		claimed[strings.ToLower(display)] = true
		out = append(out, PluginCommand{
			Plugin:      e.Plugin,
			Bare:        e.Name,
			Display:     display,
			Qualified:   qualified,
			Description: e.Description,
			Kind:        e.Kind,
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
