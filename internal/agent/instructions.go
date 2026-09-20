package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/charliek/craze/internal/harness"
)

// The loader's budgets.
//
// maxInstructionDocBytes mirrors internal/harness's own maxInstructionDoc,
// which is unexported and has to stay that way: the harness imports nothing
// from craze and nothing from craze reaches into it (D-02). The renderer cuts
// a document to that many bytes; this side's job is to make the cut
// meaningful, because a document's imports count against the same budget and a
// loader that inlined ten megabytes for the renderer to throw away would spend
// the memory and the reading either way. The two numbers drifting apart costs
// nothing worse than inlining slightly more or slightly less than is sent.
//
// maxInstructionFiles and the per-file ceiling bound the walk itself, in the
// spirit of maxSkillFiles / maxSkillBytes: a checkout with a rules directory
// nobody pruned, or a cycle of imports craze had not thought of, must not be
// able to stall a session start. The per-file ceiling is maxSkillBytes,
// enforced from the stat and again from what was read (readCappedFile), so a
// file that grows between the two is still refused.
//
// maxImportDepth is how far a chain of @imports is followed: a file at depth
// five is inlined and one at depth six is left as it was written. Five is
// Claude's own limit and is past any real arrangement of shared fragments.
const (
	maxInstructionDocBytes = 32 << 10
	maxInstructionFiles    = maxSkillFiles
	maxImportDepth         = 5
)

// loadInstructions is every instruction document a native session puts in
// front of the model, in the order the model should read them: the user's own
// file and rules first, then each directory of the chain outermost first, so
// the deepest file is the last word where two conflict. Each document arrives
// with its @imports already inlined, which is what harness.PromptExtras'
// per-document budget is a budget on.
//
// Like discoverNative it is a pure function of src and the filesystem, it
// takes no lock and touches no session state, and it never fails: a file craze
// cannot read contributes nothing, and where there is something useful to say
// it is said once through warn.
//
// Everything below is written against one rule, and that rule is why this is
// its own file rather than another pass of the scan in native_content.go.
// craze reads these files *itself* and sends them to a remote provider before
// the first turn, where no tool gate will ever see them: there is no approval
// prompt, no sandbox, and nothing downstream that can catch a mistake made
// here. So a file reached from a chain document — directly, through a symlink,
// or by import — must resolve, after EvalSymlinks, under Chain[0], and one
// reached from a user-root document under UserRoot (§3.4, A11). confinedPath
// is that rule, and nothing in this file reads a byte without going through
// it.
func loadInstructions(src contentSources, warn func(string)) []harness.PromptDoc {
	l := &instructionLoad{warn: warn, home: absOrSelf(src.Home)}
	l.readUserRoot(src)
	l.readChain(src)
	return l.docs
}

// instructionLoad is one run of that: what has been read, what it cost, and
// what came back. The file budget and the os.SameFile list are shared across
// every root and every import, so one inode is one document and costs one file
// however many names reach it.
type instructionLoad struct {
	warn func(string)
	// home is what "~/" means, and only in a user-root document. It is
	// contentSources.Home rather than the process's own environment for
	// resolveNativeSources' reason: the caller isolating a test's home is the
	// caller that would have to isolate this. It is cleaned and made absolute
	// once here; what an import writes after the "~/" is not, so that the
	// ".." in "~/a/../b" still reaches EvalSymlinks intact.
	home   string
	docs   []harness.PromptDoc
	files  []os.FileInfo
	nFiles int
}

func (l *instructionLoad) note(format string, args ...any) {
	if l.warn == nil {
		return
	}
	l.warn(fmt.Sprintf(format, args...))
}

// instructionRoot is the confinement boundary one document was reached under,
// and it travels with the document into its imports: a file imported by a
// chain document answers to the chain's root however deep the chain of imports
// goes, so no arrangement of files can launder a path into a wider boundary
// halfway down.
//
// dir is the *physical* root — EvalSymlinks has already been through it — so
// that underRoot compares like with like. An empty dir means there is nothing
// to read under it at all, which is what a missing user root and an
// unresolvable workspace both get.
type instructionRoot struct {
	dir string
	// user marks the user's own root, the one place "~/" is expanded. A chain
	// document that writes "@~/.ssh/id_rsa" is refused outright rather than
	// being read as a directory literally called "~": the tilde is a home
	// directory everywhere a person writes it, and honouring it as a filename
	// in a repository would make the refusal depend on whether a repository
	// had troubled to create that directory.
	user bool
}

// rootAt resolves one root, or gives back the empty root when there is nothing
// there. physicalPath is the resolver the chain itself was built with, so a
// home reached through a dotfiles symlink and a workspace reached through one
// are spelled the same way here as they are there.
func rootAt(dir string, user bool) instructionRoot {
	if strings.TrimSpace(dir) == "" {
		return instructionRoot{}
	}
	real, ok := physicalPath(dir)
	if !ok {
		return instructionRoot{}
	}
	return instructionRoot{dir: real, user: user}
}

// readUserRoot is step 1: the user's own CLAUDE.md, then their rules/*.md
// alphabetically. It is first because it is the weakest — a later document
// wins where two conflict — and it is the only root where "~/" means anything.
// Names are joined onto the resolved root rather than onto the spelling the
// caller gave, so that a ~/.claude which is a link into a dotfiles checkout
// reads its files at the same physical spelling it is confined to.
func (l *instructionLoad) readUserRoot(src contentSources) {
	root := rootAt(src.UserRoot, true)
	if root.dir == "" {
		return
	}
	for _, name := range src.Layout.UserInstructions {
		l.readInstruction(filepath.Join(root.dir, name), root)
	}
	l.readRules(root.dir, src.Layout.UserRules, root)
}

// readChain is step 2: every directory of the chain, outermost first, each
// contributing its instruction files and then its rules.
//
// The confinement root is Chain[0] — the repository root — for every one of
// them, not the directory being read: a CLAUDE.md three levels down that
// imports "../../shared/style.md" is importing a file of its own project, and
// confining it to its own directory would refuse the ordinary case while
// buying nothing, since everything under Chain[0] is content of the checkout
// craze was opened on. Chain[0] is already a physical path (repoChain), and a
// workspace that did not resolve is a chain of exactly one which cannot be
// climbed (X8), so there is no spelling of a chain whose root is wider than
// the checkout.
func (l *instructionLoad) readChain(src contentSources) {
	if len(src.Chain) == 0 {
		return
	}
	root := rootAt(src.Chain[0], false)
	if root.dir == "" {
		return
	}
	for _, dir := range src.Chain {
		// The chain holds physical paths already (repoChain); absOrSelf is for
		// a contentSources assembled by hand, where a relative directory would
		// otherwise be measured against the process's working directory.
		if base := absOrSelf(dir); base != "" {
			l.readChainDir(base, src.Layout, root)
		}
	}
}

// readChainDir is one directory's instruction files and rules.
//
// The override rule is the loader's and the names are the layout's
// (sources.go): the layout lists AGENTS.override.md first precisely because it
// *replaces* the name after it rather than adding to it, so the rule is
// written here positionally and the two filenames are not re-spelled. The
// trigger is the override having been read, not its merely existing: an
// override that is refused by confinement, or is not a file at all, leaves the
// user with the AGENTS.md they also wrote rather than with nothing.
func (l *instructionLoad) readChainDir(dir string, layout contentLayout, root instructionRoot) {
	overrode := false
	for i, name := range layout.Instructions {
		if i == 1 && overrode {
			continue
		}
		read := l.readInstruction(filepath.Join(dir, name), root)
		if i == 0 && read {
			overrode = true
		}
	}
	l.readRules(dir, layout.Rules, root)
}

// readInstruction loads one instruction file as its own document and reports
// whether the file was read — which the override rule above turns on, and
// which is not the same question as whether a document came back, since an
// empty file is read and contributes nothing.
//
// An instruction file follows symlinks, which is the opposite of the skill and
// command scan next door (pluginSubdir, walkUserSkills). The two are different
// situations: there, a link inside a plugin cache craze does not own would
// read a tree the plugin's author never shipped, while here "CLAUDE.md ->
// AGENTS.md" is an arrangement the owner made inside their own checkout and is
// common enough that refusing it would make craze wrong about the files in
// front of it. What makes following it safe is confinement — the link's
// *target* is what has to land under the root — and not a rule about links.
func (l *instructionLoad) readInstruction(path string, root instructionRoot) bool {
	real, ok, outside := confinedPath(root.dir, path)
	if !ok {
		if outside {
			l.note("instruction file %s not read: it resolves outside %s", path, root.dir)
		}
		return false
	}
	text, st := l.readFileText(real)
	switch st {
	case readNotText:
		// Said for a document and deliberately not for an import. An import
		// that is left as written still shows the model the "@name" line it
		// came from, so there is something to see; a CLAUDE.md that is not
		// UTF-8 would simply not apply, with nothing anywhere to say why.
		l.note("instruction file %s not read: it is not valid UTF-8", real)
		return false
	case readSkip:
		return false
	}
	l.addDoc(real, l.assemble(real, text, root))
	return true
}

// readRules is one rules directory, alphabetically — readDirCapped sorts, and
// the cap is the same bound every listing of a directory craze does not own
// has.
//
// The directory itself is confined before it is listed, rather than each file
// being refused one by one: a ".claude/rules" symlinked at /etc would
// otherwise produce a diagnostic per file in it, and one line about the
// directory says the same thing once.
//
// An empty rel is refused rather than left to resolve to base itself, for
// nativeSubdir's reason: a contentSources assembled without a layout should
// find nothing, not read every markdown file beside the workspace as a rule.
func (l *instructionLoad) readRules(base, rel string, root instructionRoot) {
	if rel == "" {
		return
	}
	dir := filepath.Join(base, rel)
	real, ok, outside := confinedPath(root.dir, dir)
	if !ok {
		if outside {
			l.note("rules directory %s not read: it resolves outside %s", dir, root.dir)
		}
		return
	}
	for _, e := range readDirCapped(real) {
		if e.IsDir() || !strings.EqualFold(filepath.Ext(e.Name()), ".md") {
			continue
		}
		l.readRule(filepath.Join(real, e.Name()), root)
	}
}

// readRule is one rule file: the same document as any other, less its
// frontmatter, and only if it is not gated.
//
// A rule whose frontmatter carries a paths: key is skipped whole rather than
// loaded ungated. Gating is deferred (D-47), and a rule loaded unconditionally
// would be applied in exactly the places its author said it should not be —
// which is worse than not having it, because the author wrote it believing it
// was scoped. The skip is silent: it is a deliberate, documented choice rather
// than a file craze could not offer, and a user with a dozen scoped rules
// would otherwise open every session under a dozen diagnostics.
//
// Frontmatter that opens and never closes is a different matter and does get a
// line. craze cannot then tell whether the file carries a paths: gate, and the
// one thing it must not do is load it as though it did not.
func (l *instructionLoad) readRule(path string, root instructionRoot) {
	real, ok, outside := confinedPath(root.dir, path)
	if !ok {
		if outside {
			l.note("rule %s not read: it resolves outside %s", path, root.dir)
		}
		return
	}
	text, st := l.readFileText(real)
	switch st {
	case readNotText:
		l.note("rule %s not read: it is not valid UTF-8", real)
		return
	case readSkip:
		return
	}
	fm, body, hadFrontmatter, err := splitFrontmatter(text)
	if err != nil {
		l.note("rule %s not read: its frontmatter opens and never closes", real)
		return
	}
	if hadFrontmatter && frontmatterHasKey(fm, "paths") {
		return
	}
	l.addDoc(real, l.assemble(real, body, root))
}

// addDoc appends one finished document. A file with nothing in it is dropped
// rather than appended: the renderer frames every document with its own path,
// and a heading over an empty body tells the model only that craze read
// something.
//
// The path recorded is the physical one — the file the bytes came from, not
// the name that reached it — so provenance names what a person would have to
// edit, and two names for one inode cannot appear as two rows.
func (l *instructionLoad) addDoc(path, text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	l.docs = append(l.docs, harness.PromptDoc{Path: path, Text: text})
}

// instructionRead is what became of one attempt to read a file. Only the
// middle case is worth a word to the user, and only for a document, so the
// three are told apart here rather than at each call site.
type instructionRead int

const (
	readOK instructionRead = iota
	// readNotText is a file that was read and is not UTF-8. It is separate
	// because it is the one failure where craze has the file in its hands and
	// is refusing it, rather than never having had it.
	readNotText
	// readSkip is everything else: the budget spent, a path that is not a
	// regular file, one this run has already read, one that could not be
	// opened. None of them is news.
	readSkip
)

// readFileText is the only place a path becomes text, so the budget, the
// per-file ceiling and the os.SameFile de-duplication are decided once.
//
// Lstat rather than Stat, on a path EvalSymlinks has already resolved: the two
// agree there, and where they would not — a component swapped for a symlink
// between the resolution and this call — Lstat is the one that sees the link
// and refuses it for not being a regular file.
//
// De-duplication is os.SameFile and never a string compare. A string compare
// is wrong twice over: on a case-insensitive volume, which macOS gives by
// default, "CLAUDE.md" and "claude.md" are one file under two spellings, and
// on any volume a hard link or a symlink is one file under two names. This is
// also what makes a CLAUDE.md symlinked at AGENTS.md yield one copy rather
// than two, and what makes an import of a file already in the prompt leave its
// line alone instead of repeating the file.
func (l *instructionLoad) readFileText(real string) (string, instructionRead) {
	if l.nFiles >= maxInstructionFiles {
		return "", readSkip
	}
	info, err := os.Lstat(real)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxSkillBytes {
		return "", readSkip
	}
	for _, seen := range l.files {
		if os.SameFile(seen, info) {
			return "", readSkip
		}
	}
	// Spent and recorded before the read rather than after it, so that a file
	// which cannot be read costs its budget once and is not attempted again
	// under another name.
	l.nFiles++
	l.files = append(l.files, info)
	data, ok := readCappedFile(real)
	if !ok {
		return "", readSkip
	}
	if !utf8.Valid(data) {
		return "", readNotText
	}
	// A byte-order mark is stripped the way every other reader in this package
	// strips it (parseSkillMarkdown): it is an encoding artefact, and left in
	// place it would be the document's first character.
	return strings.TrimPrefix(string(data), "\xef\xbb\xbf"), readOK
}

// frontmatterHasKey reports that a frontmatter block carries a top-level key
// of that name, whatever its value is or is not. It is the same grammar
// parseFrontmatterLines reads — top-level keys only, quotes off, a comment
// line ignored — written separately because the question here is about a key
// the shared frontmatter struct has no field for and should not grow one for:
// nothing reads paths:, which is the whole point of skipping the file.
func frontmatterHasKey(fm, key string) bool {
	for _, raw := range strings.Split(fm, "\n") {
		raw = strings.TrimSuffix(raw, "\r")
		if !topLevel(raw) {
			continue
		}
		line := trimLine(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, _, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		if strings.EqualFold(unquoteScalar(strings.TrimSpace(k)), key) {
			return true
		}
	}
	return false
}

// confinedPath is the hard rule of §3.4 in one function: the physical file a
// path names, and only when that file lies under root. Every read in this file
// goes through it.
//
// ok is false when nothing may be read. outside distinguishes the two reasons,
// because they deserve different treatment: outside means the path named a
// real file somewhere craze may not go, which is a refusal and gets a line,
// while the other case is a path that does not resolve at all — a typo, a
// file that is not there, a symlink loop — which is silent, since an import of
// a file that does not exist is left as written and there is nothing to tell
// the user beyond what they can already see.
//
// There are two barriers and a path must pass both.
//
//  1. Lexically, before the filesystem is touched: the cleaned spelling must
//     already name something under root. This one is a pre-filter, never the
//     decision — Clean resolves ".." textually, which is exactly the thing a
//     symlink can make a lie — and it earns its place by refusing a path that
//     names somewhere outside the root even when nothing is there to read, so
//     that "@../../.env" is answered with a line rather than with silence. It
//     can only ever refuse more than barrier 2 would: a path that leaves the
//     root and comes back through a link is refused here although the file it
//     names is inside, which is the direction this check is allowed to be
//     wrong in, and a path it lets through still has to survive barrier 2.
//  2. Physically, and this is the decision: EvalSymlinks resolves every
//     component of the path, the last one included, and the *result* is what
//     is compared and what is opened. That closes every escape at once — a
//     CLAUDE.md that is a symlink out of the tree, a symlink reached through
//     an import, and a ".." that only leaves the root once a component before
//     it has been resolved — because after EvalSymlinks there is no link and
//     no ".." left in the path for any of them to hide in. Go's EvalSymlinks
//     walks components in order and pops ".." off what it has *resolved* so
//     far, so "sub/../secret" where sub is a link is resolved the way the
//     kernel would resolve it and not the way Clean would rewrite it. Which is
//     why the raw, uncleaned spelling is what is handed to it: cleaning first
//     would throw away the ".." before the link under it had been seen.
//
// The comparison itself is underRoot, which is where "under" is defined.
//
// What this cannot close is a race: a component of the path replaced between
// the resolution and the open. Go has no openat2 and nothing here can hold the
// directory, so the window is real — and it is not this reader's threat. These
// files are read once, at session start, before any agent process exists; the
// adversary §3.4 is written against is the *content* of a checkout sitting
// still on disk, not a process running beside craze with write access inside
// the repository. A machine that already has one of those has lost more than
// an instruction file.
func confinedPath(root, path string) (real string, ok, outside bool) {
	// Both are absolute here or nothing happens: every caller joins onto a
	// root this file has already resolved, and a relative path would make
	// underRoot's answer depend on the process's working directory.
	if root == "" || !filepath.IsAbs(root) || !filepath.IsAbs(path) {
		return "", false, false
	}
	if !underRoot(root, filepath.Clean(path)) {
		return "", false, true
	}
	// path, not a cleaned copy of it: Clean would rewrite "sub/../x" to "x"
	// and throw away the ".." before EvalSymlinks had seen what "sub" is.
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", false, false
	}
	if !underRoot(root, resolved) {
		return "", false, true
	}
	return resolved, true, false
}

// underRoot reports that path lies at or under root. Both must be absolute,
// and for the check that decides anything both have been through
// EvalSymlinks, so this is a comparison of two physical paths and not of two
// names.
//
// filepath.Rel is the comparison, and a string prefix is not, because a byte
// prefix is not a path boundary: "/repo-evil/x" begins with "/repo" and is not
// in it, and so does "/repository". Rel works element by element, so it
// answers "../repo-evil/x" there. Rel also Cleans both sides, so a trailing
// separator or a doubled one cannot change the answer. The only results that
// mean "inside" are "." — the root itself — and a relative path whose first
// element is not "..".
//
// It is deliberately case-sensitive, on a macOS volume that is not. Fewer
// paths compare equal case-sensitively than case-insensitively, so the worst a
// case difference can do here is refuse a file that really was inside the root
// — a lost instruction file and a diagnostic line — and it can never admit one
// that was outside. A comparison that folded case would have its errors the
// other way round, and that is the direction that puts a file on the wire.
// Identity of *files*, where getting it wrong the other way does matter, is
// os.SameFile's job and not this one's (readFileText).
func underRoot(root, path string) bool {
	if !filepath.IsAbs(root) || !filepath.IsAbs(path) {
		return false
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	// Rel returns a relative path or an error for two absolute inputs, so this
	// is unreachable today; it is here because an absolute answer would not be
	// a position inside root whatever produced it.
	return !filepath.IsAbs(rel)
}

// assemble is one document's text: the file's own bytes with its @imports
// inlined, recursively, under the confinement root it was reached through.
func (l *instructionLoad) assemble(path, text string, root instructionRoot) string {
	d := &instructionText{l: l, root: root, doc: path, last: '\n'}
	d.write(text, path, 1)
	return d.b.String()
}

// instructionText is one document under construction. last is the final byte
// written, which strings.Builder cannot be asked for and an inlined import
// needs: a file whose text does not end in a newline must not run into the
// line that followed the import.
type instructionText struct {
	l    *instructionLoad
	root instructionRoot
	doc  string
	b    strings.Builder
	last byte
	// full records that the per-document budget was reached and the one line
	// about it written, so a document with twenty imports past the ceiling
	// says so once.
	full bool
}

func (d *instructionText) emit(s string) {
	if s == "" {
		return
	}
	d.b.WriteString(s)
	d.last = s[len(s)-1]
}

// write appends text, from the file at from, replacing its import lines as it
// goes. depth is the depth an import found in this text would be inlined at,
// so the document's own text is written at depth 1.
//
// Fence state is local to one file on purpose: an imported fragment that opens
// a fence and never closes it cannot swallow the rest of the document that
// imported it, and a document's own open fence cannot be closed from inside a
// file it imported.
func (d *instructionText) write(text, from string, depth int) {
	var fenceChar byte
	fenceLen := 0
	remaining := text
	for remaining != "" {
		line, rest, found := strings.Cut(remaining, "\n")
		c, n, tail := fenceRun(line)
		switch {
		case fenceLen > 0:
			// Inside a fence. It closes on a run of at least as many of the
			// same character with nothing but whitespace after it, which is
			// also what keeps a line of prose that happens to begin with
			// backticks from ending the block early.
			if c == fenceChar && n >= fenceLen && strings.TrimSpace(tail) == "" {
				fenceLen = 0
			}
			d.emitLine(line, found)
		case n > 0:
			fenceChar, fenceLen = c, n
			d.emitLine(line, found)
		default:
			// Outside a fence, and the only place an import line is one.
			if ref := importRef(line); ref != "" && d.inline(ref, from, depth) {
				// The line is gone, replaced by the file's text, which inline
				// has already ended with a newline of its own.
				break
			}
			d.emitLine(line, found)
		}
		if !found {
			break
		}
		remaining = rest
	}
}

func (d *instructionText) emitLine(line string, newline bool) {
	d.emit(line)
	if newline {
		d.emit("\n")
	}
}

// inline replaces one import line with the file it names, and reports whether
// it did. Every path that does not is a line left exactly as the author wrote
// it, which is §3.4's rule for all of them: a missing file, a cycle, a depth,
// a budget and a refusal all read the same way in the prompt, and the ones
// worth explaining are explained through warn.
func (d *instructionText) inline(ref, from string, depth int) bool {
	// Depth is silent. It is a shape the author chose rather than something
	// craze could not do, and a deep tree of fragments would otherwise write a
	// line per leaf.
	if depth > maxImportDepth {
		return false
	}
	if d.b.Len() >= maxInstructionDocBytes {
		if !d.full {
			d.full = true
			d.l.note("instruction file %s reached the %d byte budget: the imports after that are left as written", d.doc, maxInstructionDocBytes)
		}
		return false
	}
	target, ok := d.target(ref, from)
	if !ok {
		return false
	}
	real, ok, outside := confinedPath(d.root.dir, target)
	if !ok {
		if outside {
			d.l.note("instruction import %q in %s not read: it resolves outside %s", ref, from, d.root.dir)
		}
		return false
	}
	text, st := d.l.readFileText(real)
	if st != readOK {
		return false
	}
	d.write(text, real, depth+1)
	if d.last != '\n' {
		d.emit("\n")
	}
	return true
}

// target is one import's spelling as a path on this machine, deliberately
// uncleaned — confinedPath needs the ".." where the author put it, so that
// EvalSymlinks resolves it against components rather than against text.
//
// A relative path is relative to the *physical* importing file, which is the
// file the bytes came from rather than a name that reached it. For a CLAUDE.md
// symlinked at AGENTS.md that is the only reading that makes its neighbours
// its neighbours, and it is also the tighter one: the directory it resolves
// against is already known to be inside the root.
//
// An absolute path is taken as written and left to confinement, so "@/etc/
// passwd" is refused with a line rather than quietly read as a relative path
// under the repository, which is what filepath.Join would have made of it.
func (d *instructionText) target(ref, from string) (string, bool) {
	if strings.HasPrefix(ref, "~") {
		if !d.root.user {
			d.l.note("instruction import %q in %s not read: ~/ is expanded in a user-level instruction file only", ref, from)
			return "", false
		}
		rest, ok := strings.CutPrefix(ref, "~/")
		if !ok || d.l.home == "" {
			d.l.note("instruction import %q in %s not read: only ~/ is expanded, and only where craze knows the home directory", ref, from)
			return "", false
		}
		return joinRaw(d.l.home, rest), true
	}
	if filepath.IsAbs(ref) {
		return ref, true
	}
	return joinRaw(filepath.Dir(from), ref), true
}

// joinRaw is filepath.Join without the Clean: see target and confinedPath.
func joinRaw(dir, rel string) string {
	sep := string(filepath.Separator)
	return strings.TrimSuffix(dir, sep) + sep + rel
}

// importRef is the file an import line names, or "" when the line is not one.
//
// The grammar is narrow on purpose (§3.4): a line at column 0 that, with
// trailing spaces and a carriage return removed, is "@" followed by one run of
// non-whitespace. So "see @README.md" is prose and stays prose, an email
// address in the middle of a sentence is never a path, "@my notes.md" is not
// supported and an indented "@x" belongs to whatever list or block indented it.
// Every one of those is left as written rather than guessed at, because a
// guess here is a file read and sent.
func importRef(line string) string {
	s := strings.TrimRight(line, " \r")
	if len(s) < 2 || s[0] != '@' {
		return ""
	}
	ref := s[1:]
	// Go's \S, plus the vertical tab: whitespace anywhere in the reference
	// means this is not one reference on a line of its own.
	if strings.ContainsAny(ref, " \t\n\v\f\r") {
		return ""
	}
	return ref
}

// fenceRun reads a line as the opening or closing line of a fenced code block:
// the fence character, the length of its run, and the rest of the line after
// it. n is 0 when the line is not a fence.
//
// Up to three leading spaces, then three or more backticks or tildes, which is
// CommonMark's rule and Claude's. The fourth space is what makes an indented
// code block instead, and a run of one or two is inline code or a horizontal
// rule of tildes rather than a block.
func fenceRun(line string) (c byte, n int, tail string) {
	i := 0
	for i < len(line) && i < 3 && line[i] == ' ' {
		i++
	}
	if i >= len(line) || (line[i] != '`' && line[i] != '~') {
		return 0, 0, ""
	}
	c = line[i]
	for i+n < len(line) && line[i+n] == c {
		n++
	}
	if n < 3 {
		return 0, 0, ""
	}
	return c, n, line[i+n:]
}
