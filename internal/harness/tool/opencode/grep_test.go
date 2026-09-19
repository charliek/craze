package opencode

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/charliek/craze/internal/harness/redact"
	"github.com/charliek/craze/internal/harness/tool"
)

// grep's tests with the real rg. Ported from opencode (5f9d9187):
//
//	test/tool/grep.test.ts
//	  basic search                                   TestGrepFindsMatches (a workspace of its own, not opencode's source tree)
//	  no matches returns correct output              TestGrepFindsMatches
//	  finds matches in tmp instance                  TestGrepFindsMatches
//	  does not report an unknown total when ...      TestGrepCap
//	  supports exact file paths                      TestGrepExactFile
//	  does not ask for external_directory when ...   TestGrepThroughASymlink (the paths it shows; there is no permission to ask)
//	core/test/ripgrep.test.ts
//	  never includes git metadata (grep)             TestGrepIgnores
//	  does not split surrogate pairs in oversized... TestGrepLines (in runes: a 4-byte rune at the cut is kept whole)
//	test/tool/parameters.test.ts (grep)              TestSearchParameters
//
// Skipped: nothing else in those files is grep's; opencode's permission
// requests (ctx.ask) have no counterpart, since H2 asks for nothing (plan
// 019 decision 2).

// TestGrepFindsMatches: matches are grouped by file, as "  Line n: text",
// under a count; a search that finds nothing says so and is not an error.
func TestGrepFindsMatches(t *testing.T) {
	t.Parallel()
	requireRG(t)
	f := newFixture(t)
	put(t, f.path("test.txt"), "line1\nline2\nline3")
	text := ok(t, callOK(t, f, "grep", map[string]any{"pattern": "line", "path": f.env.Workspace}))
	if want := "Found 3 matches\n" + f.path("test.txt") + ":\n  Line 1: line1\n  Line 2: line2\n  Line 3: line3"; text != want {
		t.Fatalf("grep = %q\nwant %q", text, want)
	}

	// Two files: each is a group of its own, in rg's order, which is not
	// fixed.
	put(t, f.path("sub/other.txt"), "no\nline9\n")
	text = ok(t, callOK(t, f, "grep", map[string]any{"pattern": "line"}))
	a := f.path("test.txt") + ":\n  Line 1: line1\n  Line 2: line2\n  Line 3: line3"
	b := f.path("sub/other.txt") + ":\n  Line 2: line9"
	if text != "Found 4 matches\n"+a+"\n\n"+b && text != "Found 4 matches\n"+b+"\n\n"+a {
		t.Fatalf("grep = %q\nwant the two groups under the count", text)
	}

	res := callOK(t, f, "grep", map[string]any{"pattern": "xyznonexistentpatternxyz123", "path": f.env.Workspace})
	if res.IsError || res.Class != "" || res.Text != "No files found" {
		t.Fatalf("no match: %+v", res)
	}
}

// TestGrepExactFile ports "supports exact file paths": a file named as the
// path is searched alone, include or not. opencode searches the whole
// directory holding it (NOTICE); the control is that directory's search,
// which finds the sibling too.
func TestGrepExactFile(t *testing.T) {
	t.Parallel()
	requireRG(t)
	f := newFixture(t)
	file := f.path("test.txt")
	put(t, file, "line1\nline2\nline3")
	put(t, f.path("sibling.txt"), "line2\n")
	want := "Found 1 matches\n" + file + ":\n  Line 2: line2"
	for _, in := range []map[string]any{
		{"pattern": "line2", "path": file},
		{"pattern": "line2", "path": "test.txt"},
		{"pattern": "line2", "path": file, "include": "*.go"}, // rg filters no file it is given by name
	} {
		if text := ok(t, callOK(t, f, "grep", in)); text != want {
			t.Fatalf("grep %v = %q\nwant %q", in, text, want)
		}
	}
	if text := ok(t, callOK(t, f, "grep", map[string]any{"pattern": "line2"})); !strings.Contains(text, "Found 2 matches") {
		t.Fatalf("control: the directory's search = %q, want both files", text)
	}
}

// TestGrepThroughASymlink ports the paths half of "does not ask for
// external_directory when alias path is allowed": a search through a
// symlinked directory shows the paths through the link, not the real ones.
func TestGrepThroughASymlink(t *testing.T) {
	t.Parallel()
	requireRG(t)
	f := newFixture(t)
	tmp := t.TempDir()
	real, alias := filepath.Join(tmp, "real"), filepath.Join(tmp, "alias")
	put(t, filepath.Join(real, "test.txt"), "needle")
	if err := os.Symlink(real, alias); err != nil {
		t.Fatal(err)
	}
	text := ok(t, callOK(t, f, "grep", map[string]any{"pattern": "needle", "path": alias, "include": "*.txt"}))
	if want := "Found 1 matches\n" + filepath.Join(alias, "test.txt") + ":\n  Line 1: needle"; text != want {
		t.Fatalf("grep = %q\nwant %q", text, want)
	}
	// The control: the real directory's search shows the real path.
	if text := ok(t, callOK(t, f, "grep", map[string]any{"pattern": "needle", "path": real})); !strings.Contains(text, filepath.Join(real, "test.txt")) {
		t.Fatalf("control: %q", text)
	}
}

// TestGrepCap ports "does not report an unknown total when results are
// truncated": 101 matches show 100, say more are available, and end with
// opencode's note, with no "showing N of M". The control is exactly 100,
// which opencode would call truncated too, and craze does not (NOTICE).
func TestGrepCap(t *testing.T) {
	t.Parallel()
	requireRG(t)
	for _, n := range []int{101, 100} {
		f := newFixture(t)
		for i := range n {
			put(t, f.path(fmt.Sprintf("match-%d.txt", i)), "needle")
		}
		text := ok(t, callOK(t, f, "grep", map[string]any{"pattern": "needle", "path": f.env.Workspace, "include": "*.txt"}))
		lines := strings.Count(text, "  Line 1: needle")
		note := strings.HasSuffix(text, "\n\n(Results truncated. Consider using a more specific path or pattern.)")
		switch {
		case n == 101 && (lines != 100 || !note || !strings.HasPrefix(text, "Found 100 matches (more matches available)\n")):
			t.Fatalf("101 matches: %d shown, note %v:\n%s", lines, note, text)
		case n == 100 && (lines != 100 || note || !strings.HasPrefix(text, "Found 100 matches\n")):
			t.Fatalf("control: 100 matches: %d shown, note %v:\n%s", lines, note, text)
		}
		if strings.Contains(text, "showing") {
			t.Fatalf("%d matches: an unknown total is reported: %s", n, text)
		}
	}
}

// TestGrepIgnores: grep respects .gitignore in a git work tree, searches
// hidden files (--hidden), and never a .git directory. Each has its
// control: the ignored file is found before the .gitignore names it, and
// a directory named like .git but not .git is searched.
func TestGrepIgnores(t *testing.T) {
	t.Parallel()
	requireRG(t)
	f := newFixture(t)
	if err := os.Mkdir(f.path(".git"), 0o755); err != nil { // what makes it a work tree to rg
		t.Fatal(err)
	}
	put(t, f.path(".git/config"), "needle\n")
	put(t, f.path(".gitx/config"), "needle\n")
	put(t, f.path(".hidden/secret.txt"), "needle\n")
	put(t, f.path("zz-ignored-by-test/out.txt"), "needle\n")
	put(t, f.path("src/main.txt"), "needle\n")
	shown := func() string { return ok(t, callOK(t, f, "grep", map[string]any{"pattern": "needle"})) }
	text := shown()
	for _, p := range []string{".gitx/config", ".hidden/secret.txt", "zz-ignored-by-test/out.txt", "src/main.txt"} {
		if !strings.Contains(text, f.path(p)+":") {
			t.Errorf("%s is not found:\n%s", p, text)
		}
	}
	if strings.Contains(text, f.path(".git/config")) {
		t.Errorf(".git is searched:\n%s", text)
	}
	put(t, f.path(".gitignore"), "zz-ignored-by-test/\n")
	if text := shown(); strings.Contains(text, f.path("zz-ignored-by-test/out.txt")) || !strings.Contains(text, f.path("src/main.txt")) {
		t.Errorf("with zz-ignored-by-test/ ignored:\n%s", text)
	}
}

// TestGrepInclude: include keeps the search to the files its glob matches;
// without it, every file is searched.
func TestGrepInclude(t *testing.T) {
	t.Parallel()
	requireRG(t)
	f := newFixture(t)
	put(t, f.path("a.ts"), "needle\n")
	put(t, f.path("b.go"), "needle\n")
	text := ok(t, callOK(t, f, "grep", map[string]any{"pattern": "needle", "include": "*.{ts,tsx}"}))
	if want := "Found 1 matches\n" + f.path("a.ts") + ":\n  Line 1: needle"; text != want {
		t.Fatalf("grep = %q\nwant %q", text, want)
	}
	if text := ok(t, callOK(t, f, "grep", map[string]any{"pattern": "needle"})); !strings.Contains(text, "Found 2 matches") {
		t.Fatalf("control: without include: %q", text)
	}
}

// TestGrepLines: how a matching line is shown. Its line ending goes, "\r\n"
// included (opencode keeps it, NOTICE); past 2000 runes it is cut, then
// "...", with a 4-byte rune at the cut kept whole (opencode counts UTF-16
// units and drops half a surrogate pair); a line that is not UTF-8 shows
// U+FFFD for each bad byte (opencode fails the search, NOTICE); and a
// record past 64 KiB fails the search in opencode's words. The controls: a
// line of exactly 2000 runes is kept whole, and one of 60,000 bytes is
// read and cut.
func TestGrepLines(t *testing.T) {
	t.Parallel()
	requireRG(t)
	emoji := "\U0001F600"
	for _, tc := range []struct {
		name, content, want string
		errText             string
	}{
		{name: "LF", content: "a needle\nb", want: "a needle"},
		{name: "CRLF", content: "a needle\r\nb\r\n", want: "a needle"},
		{name: "no final newline", content: "b\na needle", want: "a needle"},
		{name: "a lone CR stays", content: "a needle\rstill\n", want: "a needle\rstill"},
		{name: "2000 runes", content: "needle" + strings.Repeat("x", 1993) + emoji + "\n", want: "needle" + strings.Repeat("x", 1993) + emoji},
		{name: "2001 runes", content: "needle" + strings.Repeat("x", 1993) + emoji + emoji + "\n", want: "needle" + strings.Repeat("x", 1993) + emoji + "..."},
		{name: "not UTF-8", content: "a \xffneedle\xfe\n", want: "a \uFFFDneedle\uFFFD"},
		{name: "60,000 bytes", content: "needle" + strings.Repeat("y", 60000) + "\n", want: "needle" + strings.Repeat("y", 1994) + "..."},
		{name: "past 64 KiB", content: "needle" + strings.Repeat("y", 70000) + "\n", errText: "Ripgrep JSON record exceeded 65536 bytes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			put(t, f.path("f.txt"), tc.content)
			res := callOK(t, f, "grep", map[string]any{"pattern": "needle"})
			if tc.errText != "" {
				failed(t, res, tool.ClassToolError, tc.errText)
				return
			}
			if want := "Found 1 matches\n" + f.path("f.txt") + ":\n  Line " + lineOf(tc.content) + ": " + tc.want; ok(t, res) != want {
				t.Fatalf("grep = %q\nwant %q", res.Text, want)
			}
		})
	}
}

// lineOf is the number of the line holding "needle" in content.
func lineOf(content string) string {
	i := strings.Index(content, "needle")
	return fmt.Sprint(strings.Count(content[:i], "\n") + 1)
}

// TestGrepInvalidPattern: a regex rg cannot parse, or an include glob it
// cannot, fails with rg's message; the same text escaped is found.
func TestGrepInvalidPattern(t *testing.T) {
	t.Parallel()
	requireRG(t)
	f := newFixture(t)
	put(t, f.path("a.ts"), "call a(\n")
	res := callOK(t, f, "grep", map[string]any{"pattern": "a("})
	if !res.IsError || res.Class != tool.ClassToolError || !strings.Contains(res.Text, "regex parse error") {
		t.Fatalf("an invalid regex: %+v", res)
	}
	res = callOK(t, f, "grep", map[string]any{"pattern": "call", "include": "*.{ts"})
	if !res.IsError || !strings.Contains(res.Text, "error parsing glob") {
		t.Fatalf("an invalid include: %+v", res)
	}
	if text := ok(t, callOK(t, f, "grep", map[string]any{"pattern": `a\(`, "include": "*.{ts,js}"})); !strings.Contains(text, "  Line 1: call a(") {
		t.Fatalf("control: %q", text)
	}
}

// TestGrepPaths: a relative path is taken against the workspace, and an
// empty one is the workspace; a path that does not exist is not_found, and
// one that is neither a file nor a directory is refused before rg runs.
func TestGrepPaths(t *testing.T) {
	t.Parallel()
	requireRG(t)
	f := newFixture(t)
	put(t, f.path("sub/a.txt"), "needle\n")
	if err := syscall.Mkfifo(f.path("fifo"), 0o644); err != nil {
		t.Fatal(err)
	}
	want := "Found 1 matches\n" + f.path("sub/a.txt") + ":\n  Line 1: needle"
	for _, p := range []string{"sub", "", f.path("sub")} {
		if text := ok(t, callOK(t, f, "grep", map[string]any{"pattern": "needle", "path": p})); text != want {
			t.Errorf("path %q: %q", p, text)
		}
	}
	failed(t, callOK(t, f, "grep", map[string]any{"pattern": "needle", "path": "missing"}),
		tool.ClassNotFound, "grep path does not exist: "+f.path("missing"))
	failed(t, callOK(t, f, "grep", map[string]any{"pattern": "needle", "path": "sub/a.txt/x"}),
		tool.ClassNotFound, "grep path does not exist: "+f.path("sub/a.txt/x"))
	failed(t, callOK(t, f, "grep", map[string]any{"pattern": "needle", "path": "fifo"}),
		tool.ClassToolError, "Path is not a regular file or a directory: "+f.path("fifo"))
}

// TestGrepRequest: the card's title is the pattern, with the path it names
// relative to the workspace when it names one; the gate sees the resolved
// path searched.
func TestGrepRequest(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	outside := t.TempDir()
	for _, tc := range []struct {
		in   map[string]any
		want tool.Request
	}{
		{map[string]any{"pattern": "TODO"}, tool.Request{Title: "TODO", Paths: []string{f.env.Workspace}}},
		{map[string]any{"pattern": "TODO", "path": "src", "include": "*.go"}, tool.Request{Title: "TODO in src", Paths: []string{f.path("src")}}},
		{map[string]any{"pattern": "TODO", "path": outside}, tool.Request{Title: "TODO in " + title(f.env, outside), Paths: []string{outside}}},
	} {
		c := prepareSearch(t, newRipgrep(), f.env, "grep", tc.in)
		if got := c.Request(); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%v: Request = %+v, want %+v", tc.in, got, tc.want)
		}
	}
}

// TestGrepRedacts: a provider key in a file grep matches never reaches the
// model — not whole, and not its first half when the 2000-rune cut falls
// inside it, since grep redacts a line before cutting it. The control, a
// session with no redactor, shows the key and the half.
func TestGrepRedacts(t *testing.T) {
	t.Parallel()
	requireRG(t)
	content := "API_KEY=" + keyA + "\n" + strings.Repeat("x", 1990) + keyA + "\n"
	run := func(red *redact.Replacer) string {
		f := newFixtureWith(t, red)
		put(t, f.path(".env"), content)
		return ok(t, callOK(t, f, "grep", map[string]any{"pattern": "API_KEY|x{1990}"}))
	}
	text := run(redact.New(keyA))
	if strings.Contains(text, "sk-canary-") || strings.Contains(text, "alpha-0001") {
		t.Fatalf("the key, or part of it, reached the result:\n%s", text)
	}
	if !strings.Contains(text, "  Line 1: API_KEY="+redact.Marker+"\n") ||
		!strings.Contains(text, "  Line 2: "+strings.Repeat("x", 1990)+redact.Marker[:10]+"...") {
		t.Fatalf("the lines are not redacted, then cut:\n%s", text)
	}
	text = run(nil)
	if !strings.Contains(text, "API_KEY="+keyA) || !strings.Contains(text, strings.Repeat("x", 1990)+"sk-canary-...") {
		t.Fatalf("control: with no redactor, the key and its half are not in the result:\n%s", text)
	}
}

// TestGrepNewlineNames: grep's paths come in rg's JSON, where a newline in
// a name is escaped inside a string, so a file named with one, and one in a
// directory named "x\n..", are found where they are, under the search; each
// header is Go-quoted, so it is still one line. The control is rg's JSON
// itself: split on newlines it is still one valid record a line, and the
// directory's file is "./x\n../secret", one path, not two.
func TestGrepNewlineNames(t *testing.T) {
	t.Parallel()
	requireRG(t)
	f := newFixture(t)
	file, secret := newlineNames(t, f.env.Workspace)
	text := ok(t, callOK(t, f, "grep", map[string]any{"pattern": "needle"}))
	a := strconv.Quote(file) + ":\n  Line 1: needle"
	b := strconv.Quote(secret) + ":\n  Line 1: needle"
	if text != "Found 2 matches\n"+a+"\n\n"+b && text != "Found 2 matches\n"+b+"\n\n"+a {
		t.Fatalf("grep = %q\nwant the two quoted groups under the count", text)
	}

	bin, _ := exec.LookPath("rg")
	cmd := exec.Command(bin, "--no-config", "--json", "--", "needle", ".")
	cmd.Dir = f.env.Workspace
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, line := range strings.Split(strings.TrimSuffix(string(out), "\n"), "\n") {
		var rec struct {
			Type string `json:"type"`
			Data struct {
				Path rgData `json:"path"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("control: rg's JSON split on newlines gave %q: %v", line, err)
		}
		if p, ok := rec.Data.Path.value(); rec.Type == "match" && ok {
			paths = append(paths, p)
		}
	}
	if !slices.Contains(paths, "./x\n../secret") {
		t.Fatalf("control: rg's match paths are %q", paths)
	}
}
