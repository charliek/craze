package agent

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/charliek/craze/internal/harness"
)

// The fixtures here are built in t.TempDir() rather than in testdata/ for two
// reasons that are both about the chain: a .git cannot be committed inside
// testdata, so a fixture there would have no repository root of its own, and
// the walk up from it would climb into craze's own checkout and load craze's
// own CLAUDE.md into the assertions.

// instructionFixture is a session's world for this loader: a workspace that is
// its own repository, a home beside it, and the sources a session would
// resolve for them. It is contentTree's shape (native_content_test.go) with
// the sources kept alongside so a case can point them somewhere else.
type instructionFixture struct {
	src  contentSources
	base string
	// workspace and userRoot are the physical spellings, which is what the
	// loader records as a document's path.
	workspace string
	home      string
	userRoot  string
}

func instructionTree(t *testing.T, ws, home map[string]string) instructionFixture {
	t.Helper()
	base := t.TempDir()
	workspace := writeTree(t, filepath.Join(base, "ws"), ws)
	gitDir(t, workspace)
	homeDir := writeTree(t, filepath.Join(base, "home"), home)
	writeTree(t, filepath.Join(homeDir, ".claude"), nil)
	return instructionFixture{
		src:       resolveNativeSources(workspace, homeDir),
		base:      physical(t, base),
		workspace: physical(t, workspace),
		home:      physical(t, homeDir),
		userRoot:  physical(t, filepath.Join(homeDir, ".claude")),
	}
}

// load runs the loader and hands back both halves of its contract: the
// documents, and every line it wrote about what it refused.
func (f instructionFixture) load() ([]harness.PromptDoc, []string) {
	var lines []string
	docs := loadInstructions(f.src, func(msg string) { lines = append(lines, msg) })
	return docs, lines
}

// docTexts and docPaths are the two projections a case usually asserts on.
func docTexts(docs []harness.PromptDoc) []string {
	out := make([]string, 0, len(docs))
	for _, d := range docs {
		out = append(out, d.Text)
	}
	return out
}

func docPaths(docs []harness.PromptDoc) []string {
	out := make([]string, 0, len(docs))
	for _, d := range docs {
		out = append(out, d.Path)
	}
	return out
}

func wantDocs(t *testing.T, got []harness.PromptDoc, want ...string) {
	t.Helper()
	if g := docPaths(got); !reflect.DeepEqual(g, want) {
		t.Fatalf("documents\n got %v\nwant %v", g, want)
	}
}

func wantNoNotes(t *testing.T, lines []string) {
	t.Helper()
	if len(lines) != 0 {
		t.Fatalf("unexpected diagnostics %q", lines)
	}
}

// wantOneNote is A11's second half: a refusal says so exactly once. Every
// confinement case asserts it, because a silent refusal and a refusal nobody
// can explain are the two ways this rule goes wrong in the field.
func wantOneNote(t *testing.T, lines []string, substr string) {
	t.Helper()
	if len(lines) != 1 {
		t.Fatalf("want exactly one diagnostic, got %d: %q", len(lines), lines)
	}
	if !strings.Contains(lines[0], substr) {
		t.Fatalf("diagnostic %q does not mention %q", lines[0], substr)
	}
}

func symlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	skipUnsupported(t, "symlink", os.Symlink(target, link))
}

// skipUnsupported turns exactly one kind of failure into a skip — this
// filesystem cannot do that at all — and every other kind into a failure.
//
// Most of the confinement suite is built on links, and a blanket t.Skipf on
// any error is how such a suite loses its coverage without anyone noticing: a
// fixture bug, a permissions change or a CI image whose temporary directory
// moved would all read as "this machine has no symlinks", and every case built
// on one would go green having asserted nothing. A filesystem that really
// cannot make the link answers with ENOSYS or ENOTSUP, which Go reports as
// errors.ErrUnsupported, or with EPERM, which it reports as fs.ErrPermission;
// nothing else here is that.
func skipUnsupported(t *testing.T, what string, err error) {
	t.Helper()
	switch {
	case err == nil:
	case errors.Is(err, errors.ErrUnsupported), errors.Is(err, fs.ErrPermission):
		t.Skipf("%s: this filesystem cannot: %v", what, err)
	default:
		t.Fatalf("%s: %v", what, err)
	}
}

// allText is the whole prompt's instruction text, which is what a case about
// "is this content in the prompt at all" asks about.
func allText(docs []harness.PromptDoc) string {
	return strings.Join(docTexts(docs), "\n")
}

// TestInstructionOrder is A10's first half: which files are loaded, and in
// which order. The user's root leads, then the chain outermost first, and
// inside a directory the layout's six names come before its rules.
func TestInstructionOrder(t *testing.T) {
	base := t.TempDir()
	repo := writeTree(t, filepath.Join(base, "repo"), map[string]string{
		"AGENTS.md":                     "repo agents\n",
		"CLAUDE.md":                     "repo claude\n",
		"CLAUDE.local.md":               "repo claude local\n",
		".claude/CLAUDE.md":             "repo dot claude\n",
		".claude/CLAUDE.local.md":       "repo dot claude local\n",
		".claude/rules/b.md":            "repo rule b\n",
		".claude/rules/a.md":            "repo rule a\n",
		"sub/CLAUDE.md":                 "sub claude\n",
		"sub/.claude/rules/only.md":     "sub rule\n",
		"sub/.claude/rules/skip.txt":    "not markdown\n",
		"sub/.claude/rules/nested/x.md": "not listed\n",
	})
	gitDir(t, repo)
	home := writeTree(t, filepath.Join(base, "home"), map[string]string{
		".claude/CLAUDE.md":  "user claude\n",
		".claude/rules/z.md": "user rule z\n",
		".claude/rules/a.md": "user rule a\n",
	})

	src := resolveNativeSources(filepath.Join(repo, "sub"), home)
	docs, lines := loadDocs(t, src)
	wantNoNotes(t, lines)

	userRoot := physical(t, filepath.Join(home, ".claude"))
	real := physical(t, repo)
	wantDocs(t, docs,
		filepath.Join(userRoot, "CLAUDE.md"),
		filepath.Join(userRoot, "rules", "a.md"),
		filepath.Join(userRoot, "rules", "z.md"),
		filepath.Join(real, "AGENTS.md"),
		filepath.Join(real, "CLAUDE.md"),
		filepath.Join(real, "CLAUDE.local.md"),
		filepath.Join(real, ".claude", "CLAUDE.md"),
		filepath.Join(real, ".claude", "CLAUDE.local.md"),
		filepath.Join(real, ".claude", "rules", "a.md"),
		filepath.Join(real, ".claude", "rules", "b.md"),
		filepath.Join(real, "sub", "CLAUDE.md"),
		filepath.Join(real, "sub", ".claude", "rules", "only.md"),
	)
	if got, want := docTexts(docs)[0], "user claude\n"; got != want {
		t.Fatalf("first document %q, want %q", got, want)
	}
}

// loadDocs is the loader over a src a case built itself.
func loadDocs(t *testing.T, src contentSources) ([]harness.PromptDoc, []string) {
	t.Helper()
	var lines []string
	docs := loadInstructions(src, func(msg string) { lines = append(lines, msg) })
	return docs, lines
}

// TestInstructionAgentsOverride is pi's rule: AGENTS.override.md replaces
// AGENTS.md rather than adding to it, and only where it is really there.
func TestInstructionAgentsOverride(t *testing.T) {
	t.Run("the override replaces AGENTS.md", func(t *testing.T) {
		f := instructionTree(t, map[string]string{
			"AGENTS.override.md": "the override\n",
			"AGENTS.md":          "the plain one\n",
			"CLAUDE.md":          "claude\n",
		}, nil)
		docs, lines := f.load()
		wantNoNotes(t, lines)
		wantDocs(t, docs,
			filepath.Join(f.workspace, "AGENTS.override.md"),
			filepath.Join(f.workspace, "CLAUDE.md"),
		)
	})

	t.Run("AGENTS.md alone is loaded", func(t *testing.T) {
		f := instructionTree(t, map[string]string{"AGENTS.md": "the plain one\n"}, nil)
		docs, lines := f.load()
		wantNoNotes(t, lines)
		wantDocs(t, docs, filepath.Join(f.workspace, "AGENTS.md"))
	})

	t.Run("an override that cannot be read leaves AGENTS.md in place", func(t *testing.T) {
		f := instructionTree(t, map[string]string{"AGENTS.md": "the plain one\n"}, nil)
		// A directory where the override belongs: it exists, so a rule keyed
		// on existence would drop AGENTS.md, and it is not a file, so nothing
		// was read and the fallback has to stand.
		writeTree(t, filepath.Join(f.workspace, "AGENTS.override.md"), nil)
		docs, lines := f.load()
		wantNoNotes(t, lines)
		wantDocs(t, docs, filepath.Join(f.workspace, "AGENTS.md"))
	})
}

// TestInstructionRules is A10's rules half: alphabetical, frontmatter
// stripped, a paths: rule skipped whole and silently, and a block that never
// closes refused with a line.
func TestInstructionRules(t *testing.T) {
	f := instructionTree(t, map[string]string{
		".claude/rules/10-first.md": "---\ndescription: numbered\n---\nfirst rule\n",
		".claude/rules/20-gated.md": "---\ndescription: scoped\npaths:\n  - \"**/*.go\"\n---\nnever loaded\n",
		".claude/rules/30-plain.md": "no frontmatter at all\n",
		".claude/rules/40-upper.MD": "an upper-case extension is still markdown\n",
		".claude/rules/50-open.md":  "---\ndescription: unclosed\nstill frontmatter\n",
	}, nil)

	docs, lines := f.load()
	rules := filepath.Join(f.workspace, ".claude", "rules")
	wantDocs(t, docs,
		filepath.Join(rules, "10-first.md"),
		filepath.Join(rules, "30-plain.md"),
		filepath.Join(rules, "40-upper.MD"),
	)
	if got, want := docTexts(docs)[0], "first rule\n"; got != want {
		t.Fatalf("frontmatter was not stripped: %q", got)
	}
	if strings.Contains(allText(docs), "never loaded") {
		t.Fatal("a paths: rule was loaded")
	}
	wantOneNote(t, lines, "opens and never closes")
}

// TestInstructionOneCopyPerFile is A10's de-duplication: every file is
// de-duplicated by os.SameFile, so one inode is one document however many
// names reach it.
func TestInstructionOneCopyPerFile(t *testing.T) {
	t.Run("a symlinked CLAUDE.md yields one copy", func(t *testing.T) {
		f := instructionTree(t, map[string]string{"AGENTS.md": "the only copy\n"}, nil)
		symlink(t, "AGENTS.md", filepath.Join(f.workspace, "CLAUDE.md"))
		docs, lines := f.load()
		wantNoNotes(t, lines)
		// The path recorded is the file the bytes came from, not the link.
		wantDocs(t, docs, filepath.Join(f.workspace, "AGENTS.md"))
		if n := strings.Count(allText(docs), "the only copy"); n != 1 {
			t.Fatalf("content appears %d times", n)
		}
	})

	t.Run("a CLAUDE.md that is only an import yields one copy", func(t *testing.T) {
		f := instructionTree(t, map[string]string{
			"AGENTS.md": "the only copy\n",
			"CLAUDE.md": "@AGENTS.md\n",
		}, nil)
		docs, lines := f.load()
		wantNoNotes(t, lines)
		wantDocs(t, docs,
			filepath.Join(f.workspace, "AGENTS.md"),
			filepath.Join(f.workspace, "CLAUDE.md"),
		)
		if n := strings.Count(allText(docs), "the only copy"); n != 1 {
			t.Fatalf("content appears %d times: %q", n, docTexts(docs))
		}
		// AGENTS.md was loaded first, so the import is of a file already in
		// the prompt and its line stays exactly as it was written.
		if got, want := docTexts(docs)[1], "@AGENTS.md\n"; got != want {
			t.Fatalf("import line %q, want %q", got, want)
		}
	})

	t.Run("two names for one inode yield one copy", func(t *testing.T) {
		// This is the case-insensitive volume, reproduced on a case-sensitive
		// one. On a macOS volume "CLAUDE.md" and "claude.md" are two spellings
		// of one inode; a hard link is two names for one inode, which is the
		// same thing for everything this loader does, and it is the shape the
		// dedupe has to survive on any filesystem. A string compare passes
		// neither. Only os.SameFile answers both.
		f := instructionTree(t, map[string]string{"AGENTS.md": "the only copy\n"}, nil)
		skipUnsupported(t, "hard link", os.Link(filepath.Join(f.workspace, "AGENTS.md"), filepath.Join(f.workspace, "CLAUDE.md")))
		docs, lines := f.load()
		wantNoNotes(t, lines)
		wantDocs(t, docs, filepath.Join(f.workspace, "AGENTS.md"))
		if n := strings.Count(allText(docs), "the only copy"); n != 1 {
			t.Fatalf("content appears %d times", n)
		}
	})
}

// TestInstructionSharedImportIsInlinedOnce is de-duplication across documents
// rather than inside one: two files of the same checkout importing a third —
// a shared style guide, which is what imports are for — put its text in the
// prompt once, and the second document keeps the line its author wrote. The
// model can still see which files claimed it.
func TestInstructionSharedImportIsInlinedOnce(t *testing.T) {
	f := instructionTree(t, map[string]string{
		"CLAUDE.md":       "one\n@shared.md\n",
		"CLAUDE.local.md": "two\n@shared.md\n",
		"shared.md":       "SHARED\n",
	}, nil)
	docs, lines := f.load()
	wantNoNotes(t, lines)
	wantDocs(t, docs,
		filepath.Join(f.workspace, "CLAUDE.md"),
		filepath.Join(f.workspace, "CLAUDE.local.md"),
	)
	if n := strings.Count(allText(docs), "SHARED\n"); n != 1 {
		t.Fatalf("the shared file is in the prompt %d times: %q", n, docTexts(docs))
	}
	if got, want := docTexts(docs)[0], "one\nSHARED\n"; got != want {
		t.Fatalf("the first document is %q, want %q", got, want)
	}
	if got, want := docTexts(docs)[1], "two\n@shared.md\n"; got != want {
		t.Fatalf("the second document is %q, want %q", got, want)
	}
}

// TestInstructionImportDepth is A10's depth: a file at depth five is inlined
// and one at depth six is left as it was written.
func TestInstructionImportDepth(t *testing.T) {
	files := map[string]string{"CLAUDE.md": "level 0\n@f1.md\n"}
	for i := 1; i <= maxImportDepth+1; i++ {
		files[fmt.Sprintf("f%d.md", i)] = fmt.Sprintf("level %d\n@f%d.md\n", i, i+1)
	}
	f := instructionTree(t, files, nil)
	docs, lines := f.load()
	wantNoNotes(t, lines)
	got := allText(docs)
	for i := 0; i <= maxImportDepth; i++ {
		if !strings.Contains(got, fmt.Sprintf("level %d\n", i)) {
			t.Fatalf("level %d is missing from %q", i, got)
		}
	}
	if strings.Contains(got, fmt.Sprintf("level %d\n", maxImportDepth+1)) {
		t.Fatalf("a file past depth %d was inlined: %q", maxImportDepth, got)
	}
	// The line that was not followed stays exactly as it was written.
	if !strings.Contains(got, fmt.Sprintf("@f%d.md\n", maxImportDepth+1)) {
		t.Fatalf("the refused import line is gone: %q", got)
	}
}

// TestInstructionImportCycle is A10's cycle: an already-loaded file is not
// loaded again, so a cycle terminates and its line stays literal.
func TestInstructionImportCycle(t *testing.T) {
	f := instructionTree(t, map[string]string{
		"CLAUDE.md": "start\n@a.md\n",
		"a.md":      "a\n@b.md\n",
		"b.md":      "b\n@a.md\n@CLAUDE.md\n",
	}, nil)
	docs, lines := f.load()
	wantNoNotes(t, lines)
	got := allText(docs)
	for _, want := range []string{"start\n", "a\n", "b\n"} {
		if !strings.Contains(got, want) {
			t.Fatalf("%q is missing from %q", want, got)
		}
	}
	if n := strings.Count(got, "\na\n"); n != 1 {
		t.Fatalf("a.md was inlined %d times: %q", n, got)
	}
	if !strings.Contains(got, "@a.md\n") || !strings.Contains(got, "@CLAUDE.md\n") {
		t.Fatalf("the cycle's lines were not left as written: %q", got)
	}
}

// TestInstructionImportGrammar is A10's grammar, line by line: what is an
// import and what is prose that merely looks like one.
func TestInstructionImportGrammar(t *testing.T) {
	cases := []struct {
		name     string
		claude   string
		inlined  bool
		wantText string
	}{
		{name: "a line of its own", claude: "@x.md\n", inlined: true},
		{name: "trailing spaces are ignored", claude: "@x.md   \n", inlined: true},
		{name: "a carriage return is ignored", claude: "@x.md\r\n", inlined: true},
		{name: "the last line needs no newline", claude: "@x.md", inlined: true},
		{
			name: "inline is prose", claude: "see @x.md for more\n",
			wantText: "see @x.md for more\n",
		},
		{
			name: "a path with a space is not supported", claude: "@x .md\n",
			wantText: "@x .md\n",
		},
		{
			name: "a trailing tab is not a trailing space", claude: "@x.md\t\n",
			wantText: "@x.md\t\n",
		},
		{
			name: "an indented import belongs to its list", claude: " @x.md\n",
			wantText: " @x.md\n",
		},
		{name: "a bare at is nothing", claude: "@\n", wantText: "@\n"},
		{
			name:     "inside a backtick fence",
			claude:   "```\n@x.md\n```\n",
			wantText: "```\n@x.md\n```\n",
		},
		{
			name:     "inside a tilde fence",
			claude:   "~~~\n@x.md\n~~~\n",
			wantText: "~~~\n@x.md\n~~~\n",
		},
		{
			name:     "inside an indented fence",
			claude:   "   ```md\n@x.md\n   ```\n",
			wantText: "   ```md\n@x.md\n   ```\n",
		},
		{
			// A long fence is closed only by one at least as long, so the
			// three-backtick line in the middle is content and the import
			// under it is still fenced.
			name:     "inside a long fence",
			claude:   "`````\n```\n@x.md\n`````\n",
			wantText: "`````\n```\n@x.md\n`````\n",
		},
		{
			// A backtick fence is not closed by tildes.
			name:     "a fence is closed by its own character",
			claude:   "```\n~~~\n@x.md\n```\n",
			wantText: "```\n~~~\n@x.md\n```\n",
		},
		{
			// Four spaces is an indented code block, not a fence, so nothing
			// is open and the import that follows is an import.
			name:    "four spaces is not a fence",
			claude:  "    ```\n@x.md\n",
			inlined: true,
		},
		{
			// A closing fence carries nothing but whitespace, so this one is
			// still content of the block.
			name:     "a fence with an info string does not close",
			claude:   "```\n``` still open\n@x.md\n```\n",
			wantText: "```\n``` still open\n@x.md\n```\n",
		},
		{
			// A fence opened inside an imported file closes with that file and
			// cannot swallow the document that imported it.
			name:    "an unclosed fence in an import does not leak",
			claude:  "@fence.md\n@x.md\n",
			inlined: true,
		},
		{
			// The document's own fence is the other direction, and it does run
			// to the end of the file: everything after it is code.
			name:     "an unclosed fence in the document itself",
			claude:   "```\n@x.md\n",
			wantText: "```\n@x.md\n",
		},
		{
			// CommonMark: a backtick fence's info string may hold no backtick,
			// so this line is inline code in a sentence and opens nothing. A
			// parser that took it for a fence would silence the import under it.
			name:    "a backtick in a backtick fence's info string is not a fence",
			claude:  "```a`` and ``b`\n@x.md\n",
			inlined: true,
		},
		{
			// A tilde fence has no inline form to be confused with, so its info
			// string may say anything, backticks included.
			name:     "a backtick in a tilde fence's info string is still a fence",
			claude:   "~~~a`b\n@x.md\n~~~\n",
			wantText: "~~~a`b\n@x.md\n~~~\n",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := instructionTree(t, map[string]string{
				"CLAUDE.md": tc.claude,
				"x.md":      "IMPORTED\n",
				"fence.md":  "```\nnever closed\n",
			}, nil)
			docs, lines := f.load()
			wantNoNotes(t, lines)
			got := allText(docs)
			if tc.inlined {
				if !strings.Contains(got, "IMPORTED") {
					t.Fatalf("the import was not inlined: %q", got)
				}
				return
			}
			if strings.Contains(got, "IMPORTED") {
				t.Fatalf("the line was treated as an import: %q", got)
			}
			if got != tc.wantText {
				t.Fatalf("text %q, want %q", got, tc.wantText)
			}
		})
	}
}

// TestInstructionImportReplacesTheLine pins what an inlined import does to the
// text around it: the line is gone, the file's text stands in its place, and a
// file that does not end in a newline does not run into what followed.
func TestInstructionImportReplacesTheLine(t *testing.T) {
	f := instructionTree(t, map[string]string{
		"CLAUDE.md": "before\n@x.md\nafter\n",
		"x.md":      "no trailing newline",
	}, nil)
	docs, lines := f.load()
	wantNoNotes(t, lines)
	if got, want := allText(docs), "before\nno trailing newline\nafter\n"; got != want {
		t.Fatalf("text %q, want %q", got, want)
	}
}

// TestInstructionImportKeepsFrontmatter is §3.4's rule for what an import
// brings with it: the target's text, frontmatter and all. Only a rule file's
// own frontmatter is stripped, and only from the rule.
func TestInstructionImportKeepsFrontmatter(t *testing.T) {
	f := instructionTree(t, map[string]string{
		"CLAUDE.md": "@x.md\n",
		"x.md":      "---\ntitle: kept\n---\nbody\n",
	}, nil)
	docs, lines := f.load()
	wantNoNotes(t, lines)
	if got, want := allText(docs), "---\ntitle: kept\n---\nbody\n"; got != want {
		t.Fatalf("text %q, want %q", got, want)
	}
}

// TestInstructionImportNotUTF8 is A10: a target that is not text is left as it
// was written, and silently — the line the author wrote is still in the
// prompt, which is more than a diagnostic would add.
func TestInstructionImportNotUTF8(t *testing.T) {
	f := instructionTree(t, map[string]string{"CLAUDE.md": "@bad.md\n"}, nil)
	if err := os.WriteFile(filepath.Join(f.workspace, "bad.md"), []byte{0xff, 0xfe, 0x00, 'x'}, 0o644); err != nil {
		t.Fatal(err)
	}
	docs, lines := f.load()
	wantNoNotes(t, lines)
	if got, want := allText(docs), "@bad.md\n"; got != want {
		t.Fatalf("text %q, want %q", got, want)
	}
}

// TestInstructionFileNotUTF8 is the other half of that asymmetry: a document
// that is not text vanishes, so craze says so once. Without the line there
// would be nothing anywhere to explain why the user's file did not apply.
func TestInstructionFileNotUTF8(t *testing.T) {
	f := instructionTree(t, nil, nil)
	if err := os.WriteFile(filepath.Join(f.workspace, "CLAUDE.md"), []byte{0xff, 0xfe}, 0o644); err != nil {
		t.Fatal(err)
	}
	docs, lines := f.load()
	if len(docs) != 0 {
		t.Fatalf("documents %v", docPaths(docs))
	}
	wantOneNote(t, lines, "not valid UTF-8")
}

// fillerLine is 32 bytes, and the budget cases count in them: a document cut
// at the ceiling may end one whole line past it and no more, so the bound the
// loader promises is the ceiling plus the length of the line that crossed it.
const fillerLine = "filler line to spend the budget\n"

// wantBounded is that promise on one document.
func wantBounded(t *testing.T, text string, longestLine int) {
	t.Helper()
	if len(text) > maxInstructionDocBytes+longestLine {
		t.Fatalf("the document is %d bytes, past the %d-byte ceiling by more than the line that crossed it",
			len(text), maxInstructionDocBytes)
	}
	if !strings.HasSuffix(text, "\n") {
		t.Fatalf("the document does not end at a line boundary: %q", text[max(0, len(text)-40):])
	}
}

// TestInstructionImportBudget is A10's budget, and it is a budget on what the
// loader hands over rather than on what it bothers to inline. A document's own
// text and everything its imports bring count against the same ceiling; the
// text ends on the first whole line at or past it, and what would have come
// after — the rest of the file, and every import still to be expanded — is not
// read at all.
//
// The one line past the ceiling is deliberate: it leaves the renderer
// something to cut, which is what puts "[craze: truncated]" in front of the
// model.
func TestInstructionImportBudget(t *testing.T) {
	big := strings.Repeat(fillerLine, 600) // ~19 KiB
	files := map[string]string{}
	var doc strings.Builder
	for i := 0; i < 4; i++ {
		fmt.Fprintf(&doc, "marker %d\n@big%d.md\n", i, i)
		files[fmt.Sprintf("big%d.md", i)] = fmt.Sprintf("chunk %d\n", i) + big
	}
	files["CLAUDE.md"] = doc.String()
	f := instructionTree(t, files, nil)

	docs, lines := f.load()
	if len(docs) != 1 {
		t.Fatalf("documents %v", docPaths(docs))
	}
	got := docs[0].Text
	// Two chunks fit inside 32 KiB; the second of them is where the ceiling
	// falls, and nothing after it is in the document at all.
	if !strings.Contains(got, "chunk 0\n") || !strings.Contains(got, "chunk 1\n") {
		t.Fatalf("the first chunks were not inlined (%d bytes)", len(got))
	}
	for _, gone := range []string{"chunk 2\n", "chunk 3\n", "marker 2\n", "@big2.md\n"} {
		if strings.Contains(got, gone) {
			t.Fatalf("%q is past the ceiling and is in the document (%d bytes)", gone, len(got))
		}
	}
	// Nothing is ever cut mid-line: every line of the document is a whole line.
	for _, line := range strings.Split(strings.TrimSuffix(got, "\n"), "\n") {
		switch {
		case line == "" || strings.HasPrefix(line, "marker ") || strings.HasPrefix(line, "chunk ") ||
			strings.HasPrefix(line, "@big") || line == strings.TrimSuffix(fillerLine, "\n"):
		default:
			t.Fatalf("a line was cut: %q", line)
		}
	}
	wantBounded(t, got, len(fillerLine))
	if len(got) <= maxInstructionDocBytes {
		t.Fatalf("the document is %d bytes, so the renderer will not mark it truncated", len(got))
	}
	wantOneNote(t, lines, "byte budget")
}

// TestInstructionDocumentBudget is the half the ceiling used to miss entirely:
// a document that imports nothing. It was checked only before an import, so a
// megabyte CLAUDE.md came back whole and the loader's stated bound was not one.
func TestInstructionDocumentBudget(t *testing.T) {
	f := instructionTree(t, map[string]string{
		"CLAUDE.md": strings.Repeat(fillerLine, 16<<10), // 512 KiB, no imports
	}, nil)
	docs, lines := f.load()
	if len(docs) != 1 {
		t.Fatalf("documents %v", docPaths(docs))
	}
	wantBounded(t, docs[0].Text, len(fillerLine))
	wantOneNote(t, lines, "byte budget")
}

// TestInstructionRulesBudgetTogether is the same bound over a whole directory,
// which is where it is paid for: maxInstructionFiles rule files of the
// per-file ceiling are 256 MiB of text, and craze used to hold all of it until
// the renderer threw most of it away. Every document is bounded, so the whole
// set is bounded by the count times the bound.
func TestInstructionRulesBudgetTogether(t *testing.T) {
	const (
		rules = 16
		each  = 128 << 10 // well past the per-document ceiling, cheap to write
	)
	files := make(map[string]string, rules)
	for i := range rules {
		files[fmt.Sprintf(".claude/rules/%02d.md", i)] = strings.Repeat(fillerLine, each/len(fillerLine))
	}
	f := instructionTree(t, files, nil)
	docs, lines := f.load()
	if len(docs) != rules {
		t.Fatalf("loaded %d documents, want %d", len(docs), rules)
	}
	var total int
	for _, d := range docs {
		wantBounded(t, d.Text, len(fillerLine))
		total += len(d.Text)
	}
	if want := rules * (maxInstructionDocBytes + len(fillerLine)); total > want {
		t.Fatalf("the rules are %d bytes together, past the %d the ceiling allows", total, want)
	}
	if total >= rules*each {
		t.Fatalf("control: %d bytes retained, which is every byte on disk", total)
	}
	if len(lines) != rules {
		t.Fatalf("want one line per cut document, got %d: %q", len(lines), lines)
	}
}

// TestInstructionFileBudget bounds the walk itself: past maxInstructionFiles
// the loader stops reading, so a rules directory nobody pruned cannot stall a
// session start.
func TestInstructionFileBudget(t *testing.T) {
	files := make(map[string]string, maxInstructionFiles+20)
	for i := 0; i < maxInstructionFiles+20; i++ {
		files[fmt.Sprintf(".claude/rules/%04d.md", i)] = fmt.Sprintf("rule %d\n", i)
	}
	f := instructionTree(t, files, nil)
	docs, lines := f.load()
	wantNoNotes(t, lines)
	if len(docs) != maxInstructionFiles {
		t.Fatalf("loaded %d documents, want %d", len(docs), maxInstructionFiles)
	}
}

// TestInstructionGitignoreIsNotConsulted is §3.4 item 7, recorded as a test
// because grok-build does the opposite: an ignored instruction file is still
// the user's instruction file.
func TestInstructionGitignoreIsNotConsulted(t *testing.T) {
	f := instructionTree(t, map[string]string{
		".gitignore":      "CLAUDE.local.md\n.claude/\n*.md\n",
		"CLAUDE.md":       "tracked\n",
		"CLAUDE.local.md": "ignored and loaded\n",
	}, nil)
	docs, lines := f.load()
	wantNoNotes(t, lines)
	wantDocs(t, docs,
		filepath.Join(f.workspace, "CLAUDE.md"),
		filepath.Join(f.workspace, "CLAUDE.local.md"),
	)
}

// TestInstructionEmptyFilesAndSources is the loader's "never fails" half: no
// sources, no roots, a file with nothing in it, and a nil warn.
func TestInstructionEmptyFilesAndSources(t *testing.T) {
	if docs := loadInstructions(contentSources{}, nil); len(docs) != 0 {
		t.Fatalf("documents from nothing: %v", docPaths(docs))
	}
	f := instructionTree(t, map[string]string{"CLAUDE.md": "   \n\n"}, nil)
	if docs := loadInstructions(f.src, nil); len(docs) != 0 {
		t.Fatalf("an empty file became a document: %v", docPaths(docs))
	}
	// A workspace that does not resolve has no chain to read (X8), and a home
	// craze was not given has no user root.
	missing := resolveNativeSources(filepath.Join(f.workspace, "gone"), "")
	if docs, lines := loadDocs(t, missing); len(docs) != 0 || len(lines) != 0 {
		t.Fatalf("documents %v, lines %v", docPaths(docs), lines)
	}
}

// TestInstructionConfinement is A11, written as an attacker would: every way
// there is to name a file craze must not read, each asserting both that the
// content never reached a document and that craze said so exactly once.
//
// The fixture is one repository with a file beside it, and it is the file
// beside it that must never appear.
func TestInstructionConfinement(t *testing.T) {
	const secret = "SECRET-CONTENT"

	cases := []struct {
		name string
		// build lays out the repository and returns the substring the one
		// diagnostic must carry.
		build func(t *testing.T, f instructionFixture) string
	}{
		{
			name: "a home-relative import from a repository file",
			build: func(t *testing.T, f instructionFixture) string {
				writeTree(t, f.workspace, map[string]string{"CLAUDE.md": "@~/.ssh/id_rsa\n"})
				writeTree(t, filepath.Join(f.home, ".ssh"), map[string]string{"id_rsa": secret + "\n"})
				return "~/ is expanded in a user-level instruction file only"
			},
		},
		{
			name: "an import that climbs out of the repository",
			build: func(t *testing.T, f instructionFixture) string {
				writeTree(t, f.workspace, map[string]string{"CLAUDE.md": "@../outside/.env\n"})
				writeTree(t, filepath.Join(f.base, "outside"), map[string]string{".env": secret + "\n"})
				// Refused before the filesystem is even asked: the cleaned
				// spelling already names somewhere outside the root, so the
				// line is answered rather than left silent, which is what the
				// first of confinedPath's two barriers is for.
				return "resolves outside"
			},
		},
		{
			name: "an absolute import",
			build: func(t *testing.T, f instructionFixture) string {
				outside := writeTree(t, filepath.Join(f.base, "outside"), map[string]string{"passwd": secret + "\n"})
				writeTree(t, f.workspace, map[string]string{
					"CLAUDE.md": "@" + filepath.Join(outside, "passwd") + "\n",
				})
				return "resolves outside"
			},
		},
		{
			name: "an instruction file that is a symlink out of the repository",
			build: func(t *testing.T, f instructionFixture) string {
				outside := writeTree(t, filepath.Join(f.base, "outside"), map[string]string{"creds": secret + "\n"})
				symlink(t, filepath.Join(outside, "creds"), filepath.Join(f.workspace, "CLAUDE.md"))
				return "resolves outside"
			},
		},
		{
			name: "an import of an in-repository symlink that points out",
			build: func(t *testing.T, f instructionFixture) string {
				outside := writeTree(t, filepath.Join(f.base, "outside"), map[string]string{"creds": secret + "\n"})
				writeTree(t, f.workspace, map[string]string{"CLAUDE.md": "@link.md\n"})
				symlink(t, filepath.Join(outside, "creds"), filepath.Join(f.workspace, "link.md"))
				return "resolves outside"
			},
		},
		{
			name: "a climb that only escapes once a symlink is resolved",
			build: func(t *testing.T, f instructionFixture) string {
				// This is the case that decides confinedPath's shape. Read
				// lexically, "@sub/../creds" is "<repo>/creds" and is inside
				// the repository; resolved the way the kernel resolves it,
				// "sub" is a link to "<base>/hole", so ".." lands on "<base>"
				// and the file is "<base>/creds", which is not. A check made
				// on a cleaned path — or made after a Clean that threw the
				// ".." away before anything had looked at "sub" — passes this
				// and reads the file.
				writeTree(t, f.base, map[string]string{"creds": secret + "\n"})
				hole := writeTree(t, filepath.Join(f.base, "hole"), nil)
				writeTree(t, f.workspace, map[string]string{"CLAUDE.md": "@sub/../creds\n"})
				symlink(t, hole, filepath.Join(f.workspace, "sub"))
				return "resolves outside"
			},
		},
		{
			name: "a rules directory symlinked out of the repository",
			build: func(t *testing.T, f instructionFixture) string {
				outside := writeTree(t, filepath.Join(f.base, "outside"), map[string]string{
					"a.md": secret + "\n",
					"b.md": secret + "\n",
				})
				symlink(t, outside, filepath.Join(f.workspace, ".claude", "rules"))
				return "rules directory"
			},
		},
		{
			name: "a rule that is a symlink out of the repository",
			build: func(t *testing.T, f instructionFixture) string {
				outside := writeTree(t, filepath.Join(f.base, "outside"), map[string]string{"creds": secret + "\n"})
				writeTree(t, filepath.Join(f.workspace, ".claude", "rules"), nil)
				symlink(t, filepath.Join(outside, "creds"), filepath.Join(f.workspace, ".claude", "rules", "a.md"))
				return "resolves outside"
			},
		},
		{
			name: "an import that escapes a sibling of the repository by prefix",
			build: func(t *testing.T, f instructionFixture) string {
				// "<base>/ws-evil" begins with "<base>/ws", the repository
				// root, byte for byte. A prefix compare would let it through;
				// a comparison on path elements cannot.
				writeTree(t, f.workspace, map[string]string{"CLAUDE.md": "@../ws-evil/creds\n"})
				writeTree(t, f.workspace+"-evil", map[string]string{"creds": secret + "\n"})
				return "resolves outside"
			},
		},
		{
			name: "a user-level import that leaves the user root",
			build: func(t *testing.T, f instructionFixture) string {
				writeTree(t, f.userRoot, map[string]string{"CLAUDE.md": "@~/.ssh/id_rsa\n"})
				writeTree(t, filepath.Join(f.home, ".ssh"), map[string]string{"id_rsa": secret + "\n"})
				// "~/" is expanded here, and confinement still applies to what
				// it expands to: the user root is the boundary, not the home.
				return "resolves outside"
			},
		},
		{
			name: "a user-level import that climbs out of the user root",
			build: func(t *testing.T, f instructionFixture) string {
				writeTree(t, f.userRoot, map[string]string{"CLAUDE.md": "@../.aws/credentials\n"})
				writeTree(t, filepath.Join(f.home, ".aws"), map[string]string{"credentials": secret + "\n"})
				return "resolves outside"
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := instructionTree(t, nil, nil)
			want := tc.build(t, f)
			docs, lines := f.load()
			if strings.Contains(allText(docs), secret) {
				t.Fatalf("the file outside the root was read: %q", docTexts(docs))
			}
			wantOneNote(t, lines, want)
		})
	}
}

// TestInstructionTilde is A11's last line: "~/" is expanded in a user-level
// file and nowhere else, and what it expands to is confined like everything
// else.
func TestInstructionTilde(t *testing.T) {
	t.Run("a user-level file expands it inside the user root", func(t *testing.T) {
		f := instructionTree(t, nil, nil)
		writeTree(t, f.userRoot, map[string]string{
			"CLAUDE.md":       "@~/.claude/shared/style.md\n",
			"shared/style.md": "SHARED STYLE\n",
		})
		docs, lines := f.load()
		wantNoNotes(t, lines)
		if !strings.Contains(allText(docs), "SHARED STYLE") {
			t.Fatalf("~/ was not expanded: %q", docTexts(docs))
		}
	})

	t.Run("a repository file never expands it", func(t *testing.T) {
		f := instructionTree(t, nil, nil)
		writeTree(t, f.workspace, map[string]string{"CLAUDE.md": "@~/.claude/shared/style.md\n"})
		writeTree(t, f.userRoot, map[string]string{"shared/style.md": "SHARED STYLE\n"})
		docs, lines := f.load()
		if strings.Contains(allText(docs), "SHARED STYLE") {
			t.Fatalf("~/ was expanded in a repository file: %q", docTexts(docs))
		}
		if got, want := allText(docs), "@~/.claude/shared/style.md\n"; got != want {
			t.Fatalf("text %q, want %q", got, want)
		}
		wantOneNote(t, lines, "user-level instruction file only")
	})

	t.Run("a user root that is a link into a dotfiles checkout expands it", func(t *testing.T) {
		// The owner's own arrangement, and the one the lexical pre-filter used
		// to refuse: "~/" expands against the home craze was given, which spells
		// the path through the ~/.claude symlink, while the root it is confined
		// to is the physical dotfiles directory. Nothing lexical can reconcile
		// those two spellings; EvalSymlinks does, and it is the only thing that
		// decides.
		base := t.TempDir()
		dotfiles := writeTree(t, filepath.Join(base, "dotfiles", "claude"), map[string]string{
			"CLAUDE.md":       "@~/.claude/shared/style.md\n",
			"shared/style.md": "SHARED STYLE\n",
		})
		home := writeTree(t, filepath.Join(base, "home"), nil)
		symlink(t, dotfiles, filepath.Join(home, ".claude"))

		docs, lines := loadDocs(t, resolveNativeSources("", home))
		wantNoNotes(t, lines)
		if !strings.Contains(allText(docs), "SHARED STYLE") {
			t.Fatalf("~/ through a linked user root was refused: %q", docTexts(docs))
		}
	})

	t.Run("another user's home is not a home", func(t *testing.T) {
		f := instructionTree(t, nil, nil)
		writeTree(t, f.userRoot, map[string]string{"CLAUDE.md": "@~root/.ssh/id_rsa\n"})
		docs, lines := f.load()
		if got, want := allText(docs), "@~root/.ssh/id_rsa\n"; got != want {
			t.Fatalf("text %q, want %q", got, want)
		}
		wantOneNote(t, lines, "only ~/ is expanded")
	})

	t.Run("with no home there is nothing to expand", func(t *testing.T) {
		// UserRoot pointed somewhere of its own with an empty Home is the
		// relocation shape (A2), and "~/" has no meaning in it.
		base := t.TempDir()
		root := writeTree(t, filepath.Join(base, "elsewhere"), map[string]string{
			"CLAUDE.md": "@~/x.md\n",
			"x.md":      "NOT REACHED\n",
		})
		src := contentSources{UserRoot: root, Layout: claudeLayout()}
		docs, lines := loadDocs(t, src)
		if strings.Contains(allText(docs), "NOT REACHED") {
			t.Fatalf("~/ was expanded without a home: %q", docTexts(docs))
		}
		wantOneNote(t, lines, "home directory")
	})
}

// TestInstructionMissingImportIsSilent is the other side of A11's "one line":
// a line craze refuses gets one, and a line that simply names nothing gets
// none. A typo is not a refusal, and the author can see the line either way.
func TestInstructionMissingImportIsSilent(t *testing.T) {
	f := instructionTree(t, map[string]string{"CLAUDE.md": "@nowhere.md\n@sub/\n"}, nil)
	writeTree(t, filepath.Join(f.workspace, "sub"), nil)
	docs, lines := f.load()
	wantNoNotes(t, lines)
	if got, want := allText(docs), "@nowhere.md\n@sub/\n"; got != want {
		t.Fatalf("text %q, want %q", got, want)
	}
}

// TestInstructionRefusesAnAbsentPathOutside is confinedPath's first barrier on
// its own: a path that names somewhere outside the root is refused with a line
// even when nothing is there to read. EvalSymlinks cannot tell a missing file
// from a forbidden one, so without the lexical check the ordinary "@../../.env"
// of a repository that has no .env would be as silent as a typo, and the
// author would have no way to learn that the line would never have worked.
func TestInstructionRefusesAnAbsentPathOutside(t *testing.T) {
	f := instructionTree(t, map[string]string{"CLAUDE.md": "@../outside/.env\n"}, nil)
	docs, lines := f.load()
	if got, want := allText(docs), "@../outside/.env\n"; got != want {
		t.Fatalf("text %q, want %q", got, want)
	}
	wantOneNote(t, lines, "resolves outside")
}

// TestInstructionLexicallyOutsideButPhysicallyInside is the other side of
// that, and the reason the cleaned spelling decides nothing: an import may
// name its way out of the repository and land back inside it, and confinement
// has no business refusing a file that is in the root it is protecting.
//
// A pre-filter that refused here dropped a legitimate import and, worse, said
// it "resolves outside" when it resolves squarely inside — a diagnostic that
// would send its author looking for a problem that is not there. The decision
// is underRoot(root, EvalSymlinks(path)) and nothing else; the cleaned
// spelling is only ever consulted when there is nothing to resolve.
func TestInstructionLexicallyOutsideButPhysicallyInside(t *testing.T) {
	f := instructionTree(t, map[string]string{
		"shared/style.md": "SHARED STYLE\n",
		"CLAUDE.md":       "@../alias/style.md\n",
	}, nil)
	// "<base>/alias" is outside the repository by every lexical reading, and
	// is the repository's own "shared" directory.
	symlink(t, filepath.Join(f.workspace, "shared"), filepath.Join(f.base, "alias"))
	docs, lines := f.load()
	wantNoNotes(t, lines)
	if !strings.Contains(allText(docs), "SHARED STYLE") {
		t.Fatalf("an import that resolves inside the root was refused: %q", docTexts(docs))
	}
}

// TestInstructionDiagnosticsAreOneLine: a diagnostic is craze's own voice, and
// a path in one is the checkout's content. A directory name may hold a newline
// on Unix, so every path is written with %q — without it a repository could
// add lines of its own to craze's diagnostics, in the channel the user reads
// as craze's.
func TestInstructionDiagnosticsAreOneLine(t *testing.T) {
	base := t.TempDir()
	repo := filepath.Join(base, "re\npo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		skipUnsupported(t, "a newline in a directory name", err)
	}
	gitDir(t, repo)
	writeTree(t, repo, map[string]string{
		// One of each shape of diagnostic: a refused import, a file that is not
		// text, and a document cut at its ceiling.
		"CLAUDE.md":               "@../outside/.env\n",
		".claude/rules/big.md":    strings.Repeat(fillerLine, 2<<10),
		".claude/rules/notext.md": "\xff\xfe",
	})
	_, lines := loadDocs(t, resolveNativeSources(repo, ""))
	if len(lines) != 3 {
		t.Fatalf("want three diagnostics, got %d: %q", len(lines), lines)
	}
	for _, line := range lines {
		if strings.ContainsAny(line, "\n\r") {
			t.Errorf("a diagnostic is more than one line: %q", line)
		}
		if !strings.Contains(line, `re\npo`) {
			t.Errorf("a diagnostic did not quote the path it names: %q", line)
		}
	}
}

// TestUnderRoot is the containment check on its own, where the cases that
// matter can be stated without a filesystem: the boundary is a path element
// and never a byte prefix, and the comparison is case-sensitive on purpose.
func TestUnderRoot(t *testing.T) {
	root := string(filepath.Separator) + filepath.Join("srv", "repo")
	inside := []string{
		root,
		filepath.Join(root, "CLAUDE.md"),
		filepath.Join(root, "a", "b", "c.md"),
		root + string(filepath.Separator), // Rel cleans both sides
	}
	outside := []string{
		// The prefix cases: every one of these begins with the root byte for
		// byte and is not in it.
		root + "-evil",
		root + "-evil" + string(filepath.Separator) + "x.md",
		root + "sibling",
		// The climb.
		filepath.Join(root, "..", "other", "x.md"),
		filepath.Dir(root),
		string(filepath.Separator) + "etc",
		// Case. On a case-insensitive volume these name the same directory;
		// refusing them loses an instruction file, which is the direction this
		// check is allowed to be wrong in. Admitting a path outside the root
		// is the direction it is not.
		strings.ToUpper(root) + string(filepath.Separator) + "x.md",
		filepath.Join(filepath.Dir(root), "REPO", "x.md"),
		// Not absolute, either side.
		"relative/x.md",
	}
	for _, p := range inside {
		if !underRoot(root, p) {
			t.Errorf("underRoot(%q, %q) = false, want true", root, p)
		}
	}
	for _, p := range outside {
		if underRoot(root, p) {
			t.Errorf("underRoot(%q, %q) = true, want false", root, p)
		}
	}
	if underRoot("relative", filepath.Join(root, "x.md")) {
		t.Error("a relative root must confine nothing")
	}
	if underRoot("", filepath.Join(root, "x.md")) {
		t.Error("an empty root must confine nothing")
	}
}

// TestConfinedPathRejectsRelativeAndEmpty pins the guards at the top of the
// one function every read goes through: nothing is resolved unless both sides
// are absolute, so no answer here ever depends on the process's working
// directory.
func TestConfinedPathRejectsRelativeAndEmpty(t *testing.T) {
	f := instructionTree(t, map[string]string{"CLAUDE.md": "x\n"}, nil)
	for _, tc := range []struct{ root, path string }{
		{"", filepath.Join(f.workspace, "CLAUDE.md")},
		{"ws", filepath.Join(f.workspace, "CLAUDE.md")},
		{f.workspace, ""},
		{f.workspace, "CLAUDE.md"},
	} {
		real, ok, outside := confinedPath(tc.root, tc.path)
		if ok || outside || real != "" {
			t.Fatalf("confinedPath(%q, %q) = %q, %v, %v", tc.root, tc.path, real, ok, outside)
		}
	}
	// And the ordinary case still resolves, so the guards are not the whole
	// function.
	if real, ok, _ := confinedPath(f.workspace, filepath.Join(f.workspace, "CLAUDE.md")); !ok || real != filepath.Join(f.workspace, "CLAUDE.md") {
		t.Fatalf("confinedPath refused an in-root file: %q, %v", real, ok)
	}
}

// TestInstructionRootsMayBeLinks is the arrangement confinement must not
// break: a ~/.claude that is a link into a dotfiles checkout, and a workspace
// reached through a link. Both roots are followed — they are where the owner
// said their content is — and what is confined is everything under them, at
// the physical spelling they resolve to.
func TestInstructionRootsMayBeLinks(t *testing.T) {
	base := t.TempDir()
	dotfiles := writeTree(t, filepath.Join(base, "dotfiles", "claude"), map[string]string{
		"CLAUDE.md":  "@rules/shared.md\n",
		"rules/a.md": "user rule\n",
		"rules/shared.md": "---\npaths: gated so it is not a document of its own\n---\n" +
			"SHARED\n",
	})
	home := writeTree(t, filepath.Join(base, "home"), nil)
	symlink(t, dotfiles, filepath.Join(home, ".claude"))

	repo := writeTree(t, filepath.Join(base, "repo"), map[string]string{"CLAUDE.md": "repo\n"})
	gitDir(t, repo)
	link := filepath.Join(base, "checkout")
	symlink(t, repo, link)

	docs, lines := loadDocs(t, resolveNativeSources(link, home))
	wantNoNotes(t, lines)
	wantDocs(t, docs,
		filepath.Join(physical(t, dotfiles), "CLAUDE.md"),
		filepath.Join(physical(t, dotfiles), "rules", "a.md"),
		filepath.Join(physical(t, repo), "CLAUDE.md"),
	)
	// The import inside the linked user root resolved and was inlined, and
	// the gated rule it imported is still not a document of its own: gating
	// applies to a rule file, not to what another file chose to import.
	if !strings.Contains(allText(docs), "SHARED") {
		t.Fatalf("the import inside a linked user root was refused: %q", docTexts(docs))
	}
}

// TestInstructionChainRootIsTheRepository is what confinement is anchored on:
// a file deep in the checkout may import a file at the root, because the whole
// checkout is one project, and may not import anything above it.
func TestInstructionChainRootIsTheRepository(t *testing.T) {
	base := t.TempDir()
	repo := writeTree(t, filepath.Join(base, "repo"), map[string]string{
		"shared/style.md": "SHARED STYLE\n",
		"a/b/CLAUDE.md":   "@../../shared/style.md\n",
	})
	gitDir(t, repo)
	src := resolveNativeSources(filepath.Join(repo, "a", "b"), "")
	docs, lines := loadDocs(t, src)
	wantNoNotes(t, lines)
	if !strings.Contains(allText(docs), "SHARED STYLE") {
		t.Fatalf("a file of the same project was refused: %q", docTexts(docs))
	}
}
