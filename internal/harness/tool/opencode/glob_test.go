package opencode

import (
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

	"github.com/charliek/craze/internal/harness/tool"
)

// glob's tests with the real rg. Ported from opencode (5f9d9187):
//
//	test/tool/glob.test.ts
//	  matches files from a directory path            TestGlobMatches
//	  rejects exact file paths                       TestGlobPaths
//	core/test/ripgrep.test.ts
//	  keeps ignored files out of catch-all find ...  TestGlobIgnores (find is glob's sibling; glob takes the same flags)
//	  never includes git metadata (find)             TestGlobIgnores
//	test/tool/parameters.test.ts (glob)              TestSearchParameters
//
// Skipped: the find cases' onEntry callback, which glob does not use, and
// opencode's permission requests (ctx.ask), since H2 asks for nothing (plan
// 019 decision 2).

// globbed is glob's answer as a set of lines, since rg's order is not fixed.
func globbed(t *testing.T, f *fixture, in map[string]any) []string {
	t.Helper()
	text := ok(t, callOK(t, f, "glob", in))
	if text == "No files found" {
		return nil
	}
	lines := strings.Split(text, "\n")
	slices.Sort(lines)
	return lines
}

// TestGlobMatches ports "matches files from a directory path": the files
// the pattern matches, one absolute path a line, and not the others. The
// pattern is rg's glob, gitignore-style: with no slash in it, it matches a
// name at any depth. A pattern that matches nothing says so and is not an
// error.
func TestGlobMatches(t *testing.T) {
	t.Parallel()
	requireRG(t)
	f := newFixture(t)
	put(t, f.path("a.ts"), "export const a = 1\n")
	put(t, f.path("b.txt"), "hello\n")
	if text := ok(t, callOK(t, f, "glob", map[string]any{"pattern": "*.ts", "path": f.env.Workspace})); text != f.path("a.ts") {
		t.Fatalf("glob *.ts = %q, want %q alone", text, f.path("a.ts"))
	}
	put(t, f.path("src/c.ts"), "x\n")
	both := []string{f.path("a.ts"), f.path("src/c.ts")}
	for _, pattern := range []string{"*.ts", "**/*.ts"} {
		if got := globbed(t, f, map[string]any{"pattern": pattern}); !slices.Equal(got, both) {
			t.Fatalf("glob %s = %q, want %q", pattern, got, both)
		}
	}
	for _, in := range []map[string]any{{"pattern": "src/*.ts"}, {"pattern": "*.ts", "path": "src"}} {
		if got, want := globbed(t, f, in), []string{f.path("src/c.ts")}; !slices.Equal(got, want) {
			t.Fatalf("glob %v = %q, want %q", in, got, want)
		}
	}
	res := callOK(t, f, "glob", map[string]any{"pattern": "*.go"})
	if res.IsError || res.Class != "" || res.Text != "No files found" {
		t.Fatalf("no match: %+v", res)
	}
}

// TestGlobPaths ports "rejects exact file paths": a path that is not a
// directory is refused in opencode's words, and one that does not exist is
// not_found. The controls are a relative and an empty path, which are the
// workspace's directories.
func TestGlobPaths(t *testing.T) {
	t.Parallel()
	requireRG(t)
	f := newFixture(t)
	put(t, f.path("sub/a.ts"), "x\n")
	if err := syscall.Mkfifo(f.path("fifo"), 0o644); err != nil {
		t.Fatal(err)
	}
	failed(t, callOK(t, f, "glob", map[string]any{"pattern": "*.ts", "path": f.path("sub/a.ts")}),
		tool.ClassToolError, "glob path must be a directory: "+f.path("sub/a.ts"))
	failed(t, callOK(t, f, "glob", map[string]any{"pattern": "*.ts", "path": "fifo"}),
		tool.ClassToolError, "glob path must be a directory: "+f.path("fifo"))
	failed(t, callOK(t, f, "glob", map[string]any{"pattern": "*.ts", "path": "missing"}),
		tool.ClassNotFound, "glob path does not exist: "+f.path("missing"))
	for _, p := range []string{"sub", f.path("sub")} {
		if text := ok(t, callOK(t, f, "glob", map[string]any{"pattern": "*.ts", "path": p})); text != f.path("sub/a.ts") {
			t.Errorf("control: path %q = %q", p, text)
		}
	}
	if got := globbed(t, f, map[string]any{"pattern": "**/*.ts", "path": ""}); !slices.Equal(got, []string{f.path("sub/a.ts")}) {
		t.Errorf("control: an empty path = %q", got)
	}
}

// TestGlobCap: 101 files show 100 and opencode's note; exactly 100 show
// all of them with no note, where opencode would add it (NOTICE).
func TestGlobCap(t *testing.T) {
	t.Parallel()
	requireRG(t)
	const note = "\n\n(Results are truncated: showing first 100 results. Consider using a more specific path or pattern.)"
	for _, n := range []int{101, 100} {
		f := newFixture(t)
		for i := range n {
			put(t, f.path(fmt.Sprintf("f-%03d.txt", i)), "")
		}
		text := ok(t, callOK(t, f, "glob", map[string]any{"pattern": "*.txt"}))
		files := strings.Split(strings.TrimSuffix(text, note), "\n")
		switch {
		case n == 101 && (len(files) != 100 || !strings.HasSuffix(text, note)):
			t.Fatalf("101 files: %d shown:\n%s", len(files), text)
		case n == 100 && (len(files) != 100 || strings.Contains(text, "truncated")):
			t.Fatalf("control: 100 files: %d shown:\n%s", len(files), text)
		}
		for _, file := range files {
			if !strings.HasPrefix(file, f.path("f-")) {
				t.Fatalf("%d files: a line is not a file's path: %q", n, file)
			}
		}
	}
}

// TestGlobIgnores: glob lists what rg lists. With no --hidden (grep has
// it, glob does not), a hidden directory is entered only when the pattern
// matches the directory itself: `**/*.txt` leaves .hidden/a.txt out, where
// grep finds it (TestGrepIgnores), and `**/*` takes it in. An rg glob
// overrides the hidden and ignore rules for what it matches, so a hidden
// file the pattern names is listed. A directory .gitignore names, in a git
// work tree, stays out of a pattern that does not match it. .git is never
// listed, even by `**/*`. The controls: the ignored directory is listed
// before the .gitignore names it, and .gitx, not .git, is listed.
func TestGlobIgnores(t *testing.T) {
	t.Parallel()
	requireRG(t)
	f := newFixture(t)
	if err := os.Mkdir(f.path(".git"), 0o755); err != nil { // what makes it a work tree to rg
		t.Fatal(err)
	}
	put(t, f.path(".git/config.txt"), "x\n")
	put(t, f.path(".gitx/config.txt"), "x\n")
	put(t, f.path(".hidden/a.txt"), "x\n")
	put(t, f.path(".b.txt"), "x\n")
	put(t, f.path("shown/a.txt"), "x\n")
	put(t, f.path("zz-ignored-by-test/c.txt"), "x\n")
	txt := []string{f.path(".b.txt"), f.path("shown/a.txt"), f.path("zz-ignored-by-test/c.txt")}
	if got := globbed(t, f, map[string]any{"pattern": "**/*.txt"}); !slices.Equal(got, txt) {
		t.Fatalf("glob **/*.txt = %q\nwant %q", got, txt)
	}
	put(t, f.path(".gitignore"), "zz-ignored-by-test/\n")
	if got := globbed(t, f, map[string]any{"pattern": "**/*.txt"}); !slices.Equal(got, txt[:2]) {
		t.Fatalf("with a .gitignore, glob **/*.txt = %q\nwant %q", got, txt[:2])
	}
	all := []string{f.path(".b.txt"), f.path(".gitignore"), f.path(".gitx/config.txt"), f.path(".hidden/a.txt"),
		f.path("shown/a.txt"), f.path("zz-ignored-by-test/c.txt")}
	if got := globbed(t, f, map[string]any{"pattern": "**/*"}); !slices.Equal(got, all) {
		t.Fatalf("glob **/* = %q\nwant %q", got, all)
	}
}

// TestGlobRequest: the card's title is the pattern, with the directory it
// names when it names one; the gate sees the resolved directory.
func TestGlobRequest(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	for _, tc := range []struct {
		in   map[string]any
		want tool.Request
	}{
		{map[string]any{"pattern": "**/*.go"}, tool.Request{Title: "**/*.go", Paths: []string{f.env.Workspace}}},
		{map[string]any{"pattern": "*.go", "path": "internal/tui"}, tool.Request{Title: "*.go in internal/tui", Paths: []string{f.path("internal/tui")}}},
	} {
		c := prepareSearch(t, newRipgrep(), f.env, "glob", tc.in)
		if got := c.Request(); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%v: Request = %+v, want %+v", tc.in, got, tc.want)
		}
	}
}

// TestGlobInvalidPattern: a glob rg cannot parse fails with rg's message,
// where opencode would say "No files found" (NOTICE); a valid one is the
// control.
func TestGlobInvalidPattern(t *testing.T) {
	t.Parallel()
	requireRG(t)
	f := newFixture(t)
	put(t, f.path("a.ts"), "x\n")
	res := callOK(t, f, "glob", map[string]any{"pattern": "*.{ts"})
	if !res.IsError || res.Class != tool.ClassToolError || !strings.Contains(res.Text, "error parsing glob") {
		t.Fatalf("an invalid glob: %+v", res)
	}
	if text := ok(t, callOK(t, f, "glob", map[string]any{"pattern": "*.{ts,js}"})); text != f.path("a.ts") {
		t.Fatalf("control: %q", text)
	}
}

// newlineNames makes, in root, a file whose name holds a newline and a
// directory named "x\n.." holding a file named secret: the names that
// newline-delimited rg output would misread, the second as a "../secret"
// above root.
func newlineNames(t *testing.T, root string) (file, secret string) {
	t.Helper()
	file = filepath.Join(root, "a\nb.txt")
	secret = filepath.Join(root, "x\n..", "secret")
	put(t, file, "needle\n")
	put(t, secret, "needle\n")
	if _, err := os.Stat(filepath.Join(filepath.Dir(root), "secret")); err == nil {
		t.Fatal("the phantom's path exists: the test would prove nothing")
	}
	return file, secret
}

// TestGlobNewlineNames: glob reads rg's paths NUL-delimited, so a file name
// with a newline in it is one path, and a directory named "x\n.." holds a
// file under the search, not a "../secret" above it; each such path is
// shown Go-quoted, so the answer's lines are still one path each. The
// negative control runs rg as opencode does, newline-delimited: its output
// splits into the phantom "../secret", which joins onto the search's
// parent.
func TestGlobNewlineNames(t *testing.T) {
	t.Parallel()
	requireRG(t)
	f := newFixture(t)
	file, secret := newlineNames(t, f.env.Workspace)

	got := globbed(t, f, map[string]any{"pattern": "**/*"})
	want := []string{strconv.Quote(file), strconv.Quote(secret)}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("glob **/* = %q\nwant %q", got, want)
	}
	for _, line := range got {
		p, err := strconv.Unquote(line)
		if err != nil {
			t.Fatalf("%q is not a quoted path: %v", line, err)
		}
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("glob shows a path that is not there: %v", err)
		}
	}

	bin, _ := exec.LookPath("rg")
	cmd := exec.Command(bin, "--no-config", "--files", "--glob=**/*", "--glob=!**/.git/**", ".")
	cmd.Dir = f.env.Workspace
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(out), "\n")
	if !slices.Contains(lines, "../secret") {
		t.Fatalf("control: newline-delimited rg output has no phantom ../secret: %q", lines)
	}
	if p := filepath.Join(f.env.Workspace, rgRelative("../secret")); p != filepath.Join(filepath.Dir(f.env.Workspace), "secret") {
		t.Fatalf("control: the phantom joins to %s", p)
	}
	if _, err := rgPath(f.env.Workspace, "../secret"); err == nil {
		t.Fatal("rgPath takes the phantom as a path under the search")
	}
}
