package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/charliek/craze/internal/harness/redact"
	"github.com/charliek/craze/internal/harness/tool"
)

// edit makes one edit call; all is replaceAll, sent only when true.
func (f *fixture) edit(t *testing.T, path, oldString, newString string, all bool) (tool.Request, tool.Result) {
	t.Helper()
	in := map[string]any{"filePath": path, "oldString": oldString, "newString": newString}
	if all {
		in["replaceAll"] = true
	}
	return f.call(t, "edit", in)
}

// TestEditCreatesAFile ports opencode's "creating new files" cases: "creates
// new file when oldString is empty", "creates new file with nested
// directories" (parents 0755 and the file 0644, as write makes them), and
// "rejects empty oldString on existing files and leaves content unchanged",
// whose file has a BOM. A BOM newString brings is kept. The negative
// control is the refusal next to each creation.
func TestEditCreatesAFile(t *testing.T) {
	umask022(t)
	f := newFixture(t)
	req, res := f.edit(t, f.path("newfile.txt"), "", "new content", false)
	if ok(t, res) != editedText || load(t, f.path("newfile.txt")) != "new content" {
		t.Fatalf("text %q, file %q", res.Text, load(t, f.path("newfile.txt")))
	}
	if want := []tool.FileEdit{{Path: f.path("newfile.txt"), New: "new content"}}; !reflect.DeepEqual(res.Edits, want) {
		t.Fatalf("edits = %+v, want %+v", res.Edits, want)
	}
	if req.Title != "newfile.txt" || !reflect.DeepEqual(req.Paths, []string{f.path("newfile.txt")}) ||
		req.Kind != tool.KindEdit || req.ReadOnly {
		t.Fatalf("request = %+v", req)
	}

	ok(t, second(f.edit(t, filepath.Join("nested", "dir", "file.txt"), "", "nested file", false)))
	if load(t, f.path("nested/dir/file.txt")) != "nested file" || perm(t, f.path("nested/dir/file.txt")) != 0o644 {
		t.Fatal("the nested file is wrong")
	}
	for _, d := range []string{"nested", "nested/dir"} {
		if m := perm(t, f.path(d)); m != 0o755 {
			t.Errorf("%s is %04o, want 0755", d, m)
		}
	}
	onlyEntries(t, f.path("nested/dir"), "file.txt")

	const bomBytes = "\xef\xbb\xbf"
	original := bomBytes + "using System;\n"
	put(t, f.path("existing.cs"), original)
	_, res = f.edit(t, f.path("existing.cs"), "", "using Up;\n", false)
	failed(t, res, tool.ClassInvalidInput, emptyOldText)
	if load(t, f.path("existing.cs")) != original {
		t.Fatal("the refused edit changed the file")
	}

	_, res = f.edit(t, f.path("bom.cs"), "", bomBytes+"class A {}\n", false)
	ok(t, res)
	if load(t, f.path("bom.cs")) != bomBytes+"class A {}\n" || res.Edits[0].New != "class A {}\n" {
		t.Fatalf("file %q, edit %+v", load(t, f.path("bom.cs")), res.Edits[0])
	}

	// An empty oldString on a directory meets the file tools' own check
	// first, in opencode's words for a directory.
	must(t, os.Mkdir(f.path("adir"), 0o755))
	_, res = f.edit(t, "adir", "", "x", false)
	failed(t, res, tool.ClassToolError, "Path is a directory, not a file: "+f.path("adir"))
}

// second returns the second of two values: a call's result.
func second(_ tool.Request, res tool.Result) tool.Result { return res }

// TestEditReplaces ports opencode's "editing existing files" and "edge
// cases" that are not about events: "replaces text in existing file",
// "throws error when file does not exist", "throws error when oldString
// equals newString" (both of them), "throws error when oldString not found
// in file", "rejects loose block-anchor matches and leaves content
// unchanged", "rejects block-anchor matches with unrelated middle content",
// "replaces all occurrences with replaceAll option", "handles multiline
// replacements", "handles CRLF line endings", "throws error when path is
// directory", and "tracks file diff statistics" (the edit the adapter
// diffs). Each refusal leaves the file as it was; the successes beside them
// are the negative controls.
func TestEditReplaces(t *testing.T) {
	f := newFixture(t)
	p := f.path("file.txt")
	cases := []struct {
		name, before, old, new string
		all                    bool
		after                  string // "" with a refusal
		class                  tool.ErrorClass
		text                   string
	}{
		{"replaces text in existing file", "old content here", "old content", "new content", false, "new content here", "", ""},
		{"oldString equals newString", "content", "same", "same", false, "", tool.ClassInvalidInput, identicalText},
		{"both empty", "content", "", "", false, "", tool.ClassInvalidInput, identicalText},
		{"oldString not found", "actual content", "not in file", "replacement", false, "", tool.ClassToolError, notFoundText},
		{"loose block-anchor match",
			"function configure() {\n  keepImportantState()\n  removeAllUserData()\n  archiveBackups()\n  auditLog()\n}",
			"function configure() {\n  const enabled = true\n}", "function configure() {\n  const enabled = false\n}",
			false, "", tool.ClassToolError, notFoundText},
		{"unrelated middle content", "function configure() {\n  removeAllUserData()\n}",
			"function configure() {\n  const enabled = true\n}", "function configure() {\n  const enabled = false\n}",
			false, "", tool.ClassToolError, notFoundText},
		{"replaceAll", "foo bar foo baz foo", "foo", "qux", true, "qux bar qux baz qux", "", ""},
		{"ambiguous without replaceAll", "foo bar foo baz foo", "foo", "qux", false, "", tool.ClassToolError, multipleText},
		{"multiline replacement", "line1\nline2\nline3", "line2", "new line 2\nextra line", false, "line1\nnew line 2\nextra line\nline3", "", ""},
		{"CRLF line endings", "line1\r\nold\r\nline3", "old", "new", false, "line1\r\nnew\r\nline3", "", ""},
		{"replaceAll is literal", "foo bar foo", "foo", "$& $1 $$ $' $`", true, "$& $1 $$ $' $` bar $& $1 $$ $' $`", "", ""},
		{"identical once in the file's line ending", "a\nb\n", "a\r\nb", "a\nb", false, "", tool.ClassInvalidInput, identicalText},
		{"disproportionate", "a\nb\nc\nd\n", `a\nb\nc\nd`, "x", false, "", tool.ClassToolError, disproportionText},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			put(t, p, tc.before)
			_, res := f.edit(t, p, tc.old, tc.new, tc.all)
			if tc.after == "" {
				failed(t, res, tc.class, tc.text)
				if load(t, p) != tc.before {
					t.Fatalf("a refused edit changed the file to %q", load(t, p))
				}
				return
			}
			if ok(t, res) != editedText {
				t.Fatalf("text = %q", res.Text)
			}
			if got := load(t, p); got != tc.after {
				t.Fatalf("file = %q, want %q", got, tc.after)
			}
			if want := []tool.FileEdit{{Path: p, Old: tc.before, New: tc.after}}; !reflect.DeepEqual(res.Edits, want) {
				t.Fatalf("edits = %+v, want %+v", res.Edits, want)
			}
		})
	}

	_, res := f.edit(t, f.path("nonexistent.txt"), "old", "new", false)
	failed(t, res, tool.ClassNotFound, "File "+f.path("nonexistent.txt")+" not found")
	must(t, os.Mkdir(f.path("adir"), 0o755))
	_, res = f.edit(t, f.path("adir"), "old", "new", false)
	failed(t, res, tool.ClassToolError, "Path is a directory, not a file: "+f.path("adir"))
}

// TestEditLineEndings ports opencode's ten "line endings" cases: the file's
// ending wins, whatever oldString and newString use, with replaceAll too.
// The LF-file cases are the negative controls of the CRLF ones and back.
func TestEditLineEndings(t *testing.T) {
	const (
		old  = "alpha\nbeta\ngamma"
		next = "alpha\nbeta-updated\ngamma"
		alt  = "alpha\nbeta\nomega"
	)
	norm := func(s, ending string) string {
		s = strings.ReplaceAll(s, "\r\n", "\n")
		if ending == "\n" {
			return s
		}
		return strings.ReplaceAll(s, "\n", "\r\n")
	}
	const lf, crlf = "\n", "\r\n"
	blockOld, blockNew := "alpha\nbeta", "alpha\nbeta-updated"
	cases := []struct {
		name, content, old, new string
		all                     bool
		want, ending            string
	}{
		{"preserves LF with LF multi-line strings", norm(old+"\n", lf), norm(old, lf), norm(next, lf), false, norm(next+"\n", lf), lf},
		{"preserves CRLF with CRLF multi-line strings", norm(old+"\n", crlf), norm(old, crlf), norm(next, crlf), false, norm(next+"\n", crlf), crlf},
		{"preserves LF when old/new use CRLF", norm(old+"\n", lf), norm(old, crlf), norm(next, crlf), false, norm(next+"\n", lf), lf},
		{"preserves CRLF when old/new use LF", norm(old+"\n", crlf), norm(old, lf), norm(next, lf), false, norm(next+"\n", crlf), crlf},
		{"preserves LF when newString uses CRLF", norm(old+"\n", lf), norm(old, lf), norm(next, crlf), false, norm(next+"\n", lf), lf},
		{"preserves CRLF when newString uses LF", norm(old+"\n", crlf), norm(old, crlf), norm(next, lf), false, norm(next+"\n", crlf), crlf},
		{"preserves LF with mixed old/new line endings", norm(old+"\n", lf), "alpha\nbeta\r\ngamma", "alpha\r\nbeta\nomega", false, norm(alt+"\n", lf), lf},
		{"preserves CRLF with mixed old/new line endings", norm(old+"\n", crlf), "alpha\r\nbeta\ngamma", "alpha\nbeta\r\nomega", false, norm(alt+"\n", crlf), crlf},
		{"replaceAll preserves LF for multi-line blocks", norm(blockOld+"\n"+blockOld+"\n", lf), norm(blockOld, lf), norm(blockNew, lf), true, norm(blockNew+"\n"+blockNew+"\n", lf), lf},
		{"replaceAll preserves CRLF for multi-line blocks", norm(blockOld+"\n"+blockOld+"\n", crlf), norm(blockOld, crlf), norm(blockNew, crlf), true, norm(blockNew+"\n"+blockNew+"\n", crlf), crlf},
	}
	f := newFixture(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := f.path("test.txt")
			put(t, p, tc.content)
			ok(t, second(f.edit(t, p, tc.old, tc.new, tc.all)))
			got := load(t, p)
			if got != tc.want {
				t.Fatalf("file = %q, want %q", got, tc.want)
			}
			crlfs := strings.Count(got, "\r\n")
			lfs := strings.Count(got, "\n") - crlfs
			if tc.ending == lf && (crlfs != 0 || lfs == 0) || tc.ending == crlf && (lfs != 0 || crlfs == 0) {
				t.Fatalf("%q has %d CRLF and %d LF, want only %q", got, crlfs, lfs, tc.ending)
			}
		})
	}
}

// TestEditKeepsTheBOM ports "replaces the first visible line in BOM files"
// — the file keeps its BOM and the edit the card diffs shows none — and
// adds opencode's other BOM rule and craze's difference from it: a BOM
// newString brings to the start of the file merges with the file's, but a
// BOM that is the file's own content is never dropped (opencode drops the
// second of two at the start, and one an edit brings to the front by
// deleting what stood before it). A file without a BOM gets none: the
// negative control.
func TestEditKeepsTheBOM(t *testing.T) {
	const b = "\xef\xbb\xbf"
	cases := []struct {
		name, before, old, new, after string
		editOld, editNew              string
	}{
		{"the first visible line", b + "using System;\nclass Test {}\n", "using System;", "using Up;",
			b + "using Up;\nclass Test {}\n", "using System;\nclass Test {}\n", "using Up;\nclass Test {}\n"},
		{"no BOM, none added", "using System;\n", "System", "Up", "using Up;\n", "using System;\n", "using Up;\n"},
		{"newString's BOM merges with the file's", b + "abc\n", "abc", b + "xyz", b + "xyz\n", "abc\n", "xyz\n"},
		{"newString's BOM on a file without one", "abc\n", "abc", b + "xyz", b + "xyz\n", "abc\n", "xyz\n"},
		{"a second BOM is content", b + b + "abc\n", "abc", "xyz", b + b + "xyz\n", b + "abc\n", b + "xyz\n"},
		{"a BOM brought to the front is content", b + "X" + b + "Y\n", "X", "", b + b + "Y\n", "X" + b + "Y\n", b + "Y\n"},
		{"a BOM inside the text is content", b + "a" + b + "b\n", "a", "c", b + "c" + b + "b\n", "a" + b + "b\n", "c" + b + "b\n"},
	}
	f := newFixture(t)
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := f.path(fmt.Sprintf("bom%d.cs", i))
			put(t, p, tc.before)
			_, res := f.edit(t, p, tc.old, tc.new, false)
			ok(t, res)
			if got := load(t, p); got != tc.after {
				t.Fatalf("file = %q, want %q", got, tc.after)
			}
			if e := res.Edits[0]; e.Old != tc.editOld || e.New != tc.editNew {
				t.Fatalf("edit = old %q new %q, want old %q new %q", e.Old, e.New, tc.editOld, tc.editNew)
			}
		})
	}
}

// TestEditConcurrent ports "preserves concurrent edits to different
// sections of the same file": the first edit is held just before its
// rename while a second, through a symlink to the same file, is started;
// the path lock makes the second wait, so both changes land. Without the
// lock the second would read the old content and the first would then be
// refused as changed (the negative control, run by deleting the Lock call
// in openTarget: this test then fails). Both calls are prepared in call
// order and run at once, as the runner does.
func TestEditConcurrent(t *testing.T) {
	f := newFixture(t)
	p := f.path("file.txt")
	put(t, p, "top = 0\nmiddle = keep\nbottom = 0\n")
	must(t, os.Symlink("file.txt", f.path("alias.txt")))

	held, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	beforeCheck = func(string) {
		once.Do(func() {
			close(held)
			<-release
		})
	}
	t.Cleanup(func() { beforeCheck = nil })

	prepare := func(id, path, oldString, newString string) {
		in, err := json.Marshal(map[string]any{"filePath": path, "oldString": oldString, "newString": newString})
		must(t, err)
		if _, res, ok := f.d.Prepare(tool.Call{ID: id, Tool: "edit", Input: in}); !ok {
			t.Fatalf("prepare %s: %+v", id, res)
		}
	}
	prepare("t9.1.1", p, "top = 0", "top = 1")
	prepare("t9.1.2", f.path("alias.txt"), "bottom = 0", "bottom = 2")
	results := make(chan tool.Result, 2)
	go func() { results <- f.d.Run(context.Background(), "t9.1.1", nil) }()
	<-held
	go func() { results <- f.d.Run(context.Background(), "t9.1.2", nil) }()
	time.Sleep(50 * time.Millisecond) // the second is waiting for the lock
	close(release)
	for range 2 {
		ok(t, <-results)
	}
	if got := load(t, p); got != "top = 1\nmiddle = keep\nbottom = 2\n" {
		t.Fatalf("file = %q", got)
	}
}

// TestEditRefusesAnExternalChange: a file changed between the edit's read
// and its rename is left as the other writer made it. The last case keeps
// inode, size and modification time — a rewrite in place within one tick
// of a coarse clock — so only the content check catches it. A check that
// sees no change is the negative control.
func TestEditRefusesAnExternalChange(t *testing.T) {
	f := newFixture(t)
	t.Cleanup(func() { beforeCheck = nil })
	p := f.path("shared.txt")

	put(t, p, "one\ntwo\n")
	beforeCheck = func(string) { put(t, p, "one\ntwo\nthree\n") }
	_, res := f.edit(t, p, "two", "2", false)
	failed(t, res, tool.ClassToolError, changedText)
	if load(t, p) != "one\ntwo\nthree\n" {
		t.Fatal("the external change was overwritten")
	}

	put(t, p, "one\ntwo\n")
	info, err := os.Stat(p)
	must(t, err)
	beforeCheck = func(string) {
		// In place: same inode, same size, the time put back.
		w, err := os.OpenFile(p, os.O_WRONLY, 0)
		must(t, err)
		_, err = w.WriteAt([]byte("ONE"), 0)
		must(t, err)
		must(t, w.Close())
		must(t, os.Chtimes(p, info.ModTime(), info.ModTime()))
	}
	_, res = f.edit(t, p, "two", "2", false)
	failed(t, res, tool.ClassToolError, changedText)
	if load(t, p) != "ONE\ntwo\n" {
		t.Fatalf("the in-place change was overwritten: %q", load(t, p))
	}
	onlyEntries(t, f.env.Workspace, "shared.txt")

	beforeCheck = func(string) {}
	ok(t, second(f.edit(t, p, "two", "2", false)))
	if load(t, p) != "ONE\n2\n" {
		t.Fatal("an unchanged file was not edited")
	}
}

// TestEditRefusesWhatIsNotAFile: a FIFO — refused at once, not opened to
// wait for a writer — and a device.
func TestEditRefusesWhatIsNotAFile(t *testing.T) {
	f := newFixture(t)
	must(t, syscall.Mkfifo(f.path("fifo"), 0o644))
	done := make(chan tool.Result, 1)
	go func() { done <- second(f.edit(t, "fifo", "a", "b", false)) }()
	select {
	case res := <-done:
		failed(t, res, tool.ClassToolError, "Path is not a regular file: "+f.path("fifo"))
	case <-time.After(10 * time.Second):
		t.Fatal("editing a FIFO hung")
	}
	_, res := f.edit(t, "/dev/null", "a", "b", false)
	failed(t, res, tool.ClassToolError, "Path is not a regular file: /dev/null")
}

// TestEditThroughASymlink: the link survives and its target is edited,
// through a relative link and a chain; the edit names the path as called.
func TestEditThroughASymlink(t *testing.T) {
	f := newFixture(t)
	put(t, f.path("real/target.txt"), "value = 1\n")
	must(t, os.Symlink(filepath.Join("real", "target.txt"), f.path("link.txt")))
	must(t, os.Symlink("link.txt", f.path("chain.txt")))
	for i, name := range []string{"link.txt", "chain.txt"} {
		_, res := f.edit(t, name, fmt.Sprintf("value = %d", i+1), fmt.Sprintf("value = %d", i+2), false)
		ok(t, res)
		if info, err := os.Lstat(f.path(name)); err != nil || info.Mode()&fs.ModeSymlink == 0 {
			t.Fatalf("%s is no longer a symlink", name)
		}
		if got, want := load(t, f.path("real/target.txt")), fmt.Sprintf("value = %d\n", i+2); got != want {
			t.Fatalf("target = %q, want %q", got, want)
		}
		if res.Edits[0].Path != f.path(name) {
			t.Fatalf("edit path = %q", res.Edits[0].Path)
		}
	}
}

// TestEditKeepsTheMode: an edited file keeps its mode, setuid bit and all.
func TestEditKeepsTheMode(t *testing.T) {
	f := newFixture(t)
	for _, mode := range []fs.FileMode{0o600, 0o755, 0o640, 0o755 | fs.ModeSetuid} {
		p := f.path("mode-" + strings.ReplaceAll(mode.String(), "-", "_"))
		put(t, p, "before\n")
		must(t, os.Chmod(p, mode))
		ok(t, second(f.edit(t, p, "before", "after", false)))
		info, err := os.Stat(p)
		must(t, err)
		if got := info.Mode() & keptModeBits; got != mode || load(t, p) != "after\n" {
			t.Fatalf("%s: mode %v content %q, want %v and the edit", p, got, load(t, p), mode)
		}
	}
}

// TestEditRefusesTheCredentialsFile: <Home>/providers.toml is refused by
// name, through a symlink and through a hard link, and not changed; the
// models.toml beside it is the negative control.
func TestEditRefusesTheCredentialsFile(t *testing.T) {
	f := newFixture(t)
	cred := filepath.Join(f.env.Home, CredentialsFile)
	body := "api_key = \"" + keyA + "\"\n"
	put(t, cred, body)
	must(t, os.Symlink(cred, f.path("sneaky.toml")))
	must(t, os.Link(cred, f.path("hard.toml")))
	for _, p := range []string{cred, f.path("sneaky.toml"), f.path("hard.toml")} {
		_, res := f.edit(t, p, "api_key", "stolen", false)
		failed(t, res, tool.ClassToolError, credentialsText)
	}
	_, res := f.edit(t, filepath.Join(f.env.Home, "absent", CredentialsFile), "", "x", false)
	ok(t, res) // a different providers.toml is just a file
	if load(t, cred) != body {
		t.Fatal("the credentials file changed")
	}
	put(t, filepath.Join(f.env.Home, "models.toml"), "version = 1\n")
	ok(t, second(f.edit(t, filepath.Join(f.env.Home, "models.toml"), "1", "2", false)))
}

// TestEditRefusesTheMarker: the redaction marker in oldString or newString
// is refused (plan 019 §3.8) and the file is left alone; part of the
// marker is the negative control.
func TestEditRefusesTheMarker(t *testing.T) {
	f := newFixture(t)
	put(t, f.path(".env"), "KEY=real\nMORE=1\n")
	_, res := f.edit(t, ".env", "KEY="+redact.Marker, "KEY=rotated", false)
	failed(t, res, tool.ClassInvalidInput, markerText)
	_, res = f.edit(t, ".env", "MORE=1", "MORE="+redact.Marker, false)
	failed(t, res, tool.ClassInvalidInput, markerText)
	_, res = f.edit(t, "new.env", "", "K="+redact.Marker, false)
	failed(t, res, tool.ClassInvalidInput, markerText)
	if load(t, f.path(".env")) != "KEY=real\nMORE=1\n" {
		t.Fatal("the file changed")
	}
	ok(t, second(f.edit(t, ".env", "MORE=1", "MORE=[craze:redacted", false)))
}

// TestEditResultIsRedacted: a key in the file reaches the card's edit only
// as the marker, in the old text and the new (plan 019 §3.8: "an edit diff
// of .env").
func TestEditResultIsRedacted(t *testing.T) {
	f := newFixture(t)
	put(t, f.path(".env"), "KEY="+keyA+"\nDEBUG=0\n")
	_, res := f.edit(t, ".env", "DEBUG=0", "DEBUG=1", false)
	ok(t, res)
	raw, _ := json.Marshal(res)
	if strings.Contains(string(raw), keyA) || res.Edits[0].Old != "KEY="+redact.Marker+"\nDEBUG=0\n" ||
		res.Edits[0].New != "KEY="+redact.Marker+"\nDEBUG=1\n" {
		t.Fatalf("result = %s", raw)
	}
	if load(t, f.path(".env")) != "KEY="+keyA+"\nDEBUG=1\n" {
		t.Fatal("the key on disk changed")
	}
}

// TestEditRefusesALargeFile: a file over 5 MiB is refused before any
// matching (plan 019 §3.9); one of exactly 5 MiB is edited, the negative
// control.
func TestEditRefusesALargeFile(t *testing.T) {
	f := newFixture(t)
	body := strings.Repeat("x", maxEditBytes-len("needle\n")) + "needle\n"
	put(t, f.path("big.txt"), body+"y")
	_, res := f.edit(t, "big.txt", "needle", "pin", false)
	failed(t, res, tool.ClassOutputLimit, tooLargeText)

	put(t, f.path("big.txt"), body)
	ok(t, second(f.edit(t, "big.txt", "needle", "pin", false)))
	if !strings.HasSuffix(load(t, f.path("big.txt")), "xpin\n") {
		t.Fatal("the 5 MiB file was not edited")
	}
}

// TestEditHonoursCancelDuringTheSearch: a file under the size cap with two
// thousand block-anchor candidates, each a Levenshtein comparison of two
// 1500-character lines, is seconds of work. A cancel during it returns
// promptly, aborted, with the file untouched. That the call is still
// running when cancelled is the negative control: it shows the search is
// long enough that only the cancel can have ended it. (Removing the ctx
// checks from blockAnchor and levenshtein makes this test fail.)
func TestEditHonoursCancelDuringTheSearch(t *testing.T) {
	f := newFixture(t)
	block := "start\n" + strings.Repeat("y", 1500) + "\nend\n"
	body := strings.Repeat(block, 2000)
	if len(body) >= maxEditBytes {
		t.Fatal("the fixture must be under the size cap")
	}
	put(t, f.path("slow.txt"), body)
	old := "start\n" + strings.Repeat("x", 1500) + "\nend"

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan tool.Result, 1)
	go func() {
		_, res := f.callCtx(t, ctx, "edit", map[string]any{"filePath": "slow.txt", "oldString": old, "newString": "z"})
		done <- res
	}()
	select {
	case res := <-done:
		t.Fatalf("the search ended before the cancel: %+v", res)
	case <-time.After(300 * time.Millisecond):
	}
	cancelled := time.Now()
	cancel()
	select {
	case res := <-done:
		failed(t, res, tool.ClassAborted, tool.AbortedText)
		if d := time.Since(cancelled); d > 2*time.Second {
			t.Fatalf("returned %v after the cancel", d)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a cancelled search kept running")
	}
	if load(t, f.path("slow.txt")) != body {
		t.Fatal("a cancelled edit changed the file")
	}
}

// TestEditMatchesWhatReadShows: in a file that is not valid UTF-8 the match
// is judged on the text read showed the model — each invalid byte as
// U+FFFD — and never takes in an invalid byte, which newString could not
// give back.
//
//   - The review's case: FF " target" and EF BF BD " target" read as two
//     identical lines. An oldString with U+FFFD is refused outright; on a
//     valid file the same oldString is an ordinary edit (the negative
//     control).
//   - Without U+FFFD in oldString, block-anchor scores the two blocks'
//     middle lines alike and picks the first; in the bytes that block is
//     unique, but in what the model saw it is not, so the answer is
//     opencode's "multiple matches", not an edit of the first block.
//   - A match whose span holds an invalid byte is refused, and so is a
//     deletion that would join two invalid fragments into a character.
//   - Everything else edits as usual, invalid bytes outside the span kept,
//     replaceAll included: the negative controls of the refusals.
func TestEditMatchesWhatReadShows(t *testing.T) {
	f := newFixture(t)
	fffd := string(utf8.RuneError)
	p := f.path("mixed.txt")

	two := "\xff target\n\xef\xbf\xbd target\n"
	put(t, p, two)
	_, res := f.call(t, "read", map[string]any{"filePath": p})
	if c := ok(t, res); !strings.Contains(c, "1: "+fffd+" target\n2: "+fffd+" target\n") {
		t.Fatalf("read shows %q", c)
	}
	_, res = f.edit(t, p, fffd+" target", "mine", false)
	failed(t, res, tool.ClassToolError, invalidOldText)
	if load(t, p) != two {
		t.Fatal("the refused edit changed the file")
	}
	put(t, f.path("valid.txt"), "\xef\xbf\xbd target\nother\n")
	ok(t, second(f.edit(t, "valid.txt", fffd+" target", "mine", false)))
	if load(t, f.path("valid.txt")) != "mine\nother\n" {
		t.Fatal("a valid file with U+FFFD was not edited")
	}

	blocks := "start\n  abcdefgh\xff\nend\nstart\n  abcdefgh\xef\xbf\xbd\nend\n"
	put(t, p, blocks)
	_, res = f.edit(t, p, "start\n  abcdefghi\nend", "X", false)
	failed(t, res, tool.ClassToolError, multipleText)
	if load(t, p) != blocks {
		t.Fatal("an ambiguous edit changed the file")
	}

	one := "start\n  abcdefgh\xff\nend\n"
	put(t, p, one)
	_, res = f.edit(t, p, "start\n  abcdefghi\nend", "X", false)
	failed(t, res, tool.ClassToolError, invalidBytesText)
	if load(t, p) != one {
		t.Fatal("an edit over an invalid byte changed the file")
	}

	// Deleting "b" would join E2 82 and 80 into U+2080, which read never
	// showed; replacing it keeps them apart and is edited.
	joined := "x\xe2\x82b\x80y\n"
	put(t, p, joined)
	_, res = f.edit(t, p, "b", "", false)
	failed(t, res, tool.ClassToolError, invalidBytesText)
	if load(t, p) != joined {
		t.Fatal("a joining edit changed the file")
	}
	ok(t, second(f.edit(t, p, "b", "c", false)))
	if got := load(t, p); got != "x\xe2\x82c\x80y\n" {
		t.Fatalf("file = %q", got)
	}

	put(t, p, "\xff header\nvalue = 1\n\xfe footer\n")
	ok(t, second(f.edit(t, p, "value = 1", "value = 2", false)))
	if got := load(t, p); got != "\xff header\nvalue = 2\n\xfe footer\n" {
		t.Fatalf("file = %q", got)
	}
	put(t, p, "x\xffx\nx\xc3\n")
	ok(t, second(f.edit(t, p, "x", "yy", true)))
	if got := load(t, p); got != "yy\xffyy\nyy\xc3\n" {
		t.Fatalf("replaceAll = %q", got)
	}
}

// TestFileToolsRefuseTheCredentialsFileInAnyCase: <Home>/providers.toml is
// refused under any spelling of its name — which on a case-insensitive
// file system is the same file, created by the first write while it does
// not exist — for edit and write alike, and under another name for Home's
// directory: a symlink stands in here for the case alias Linux cannot
// make. The same names elsewhere, and other names in Home, are the negative
// controls.
func TestFileToolsRefuseTheCredentialsFileInAnyCase(t *testing.T) {
	f := newFixture(t)
	longS := string(rune(0x17F)) // folds to "s", as it does on APFS
	for _, name := range []string{"PROVIDERS.TOML", "Providers.Toml", "provider" + longS + ".toml"} {
		p := filepath.Join(f.env.Home, name)
		_, res := f.edit(t, p, "", "api_key = \"x\"\n", false)
		failed(t, res, tool.ClassToolError, credentialsText)
		_, res = f.call(t, "write", map[string]any{"filePath": p, "content": "api_key = \"x\"\n"})
		failed(t, res, tool.ClassToolError, credentialsText)
	}
	onlyEntries(t, f.env.Home)

	alias := filepath.Join(t.TempDir(), "home-alias")
	must(t, os.Symlink(f.env.Home, alias))
	if !isCredentials(f.env, filepath.Join(alias, "PROVIDERS.TOML"), nil) {
		t.Fatal("another spelling of Home's directory was not caught")
	}
	base, err := filepath.EvalSymlinks(t.TempDir()) // macOS's /var is a link
	must(t, err)
	missing := tool.Env{Home: filepath.Join(base, "absent-home")}
	if !isCredentials(missing, filepath.Join(base, "ABSENT-HOME", "providers.toml"), nil) {
		t.Fatal("a Home that does not exist yet is not compared ignoring case")
	}

	ok(t, second(f.edit(t, "PROVIDERS.TOML", "", "not a key file\n", false)))
	ok(t, second(f.edit(t, filepath.Join(f.env.Home, "providers.toml.bak"), "", "x\n", false)))
	ok(t, second(f.edit(t, filepath.Join(f.env.Home, "sub", "PROVIDERS.TOML"), "", "x\n", false)))
}

// TestFileToolsCancelBeforeTheRenameOnly: the context is the last thing
// checked before the rename. A cancel that lands while the new content is
// being written (here, at the last check) writes nothing and reports
// aborted, with no temp file left; a cancel after the rename reports the
// success the file shows. Both, for edit and write.
func TestFileToolsCancelBeforeTheRenameOnly(t *testing.T) {
	f := newFixture(t)
	t.Cleanup(func() { beforeCheck, afterReplace = nil, nil })
	p := f.path("c.txt")
	calls := []struct {
		tool string
		in   map[string]any
		want string
	}{
		{"edit", map[string]any{"filePath": p, "oldString": "before", "newString": "after"}, "after\n"},
		{"write", map[string]any{"filePath": p, "content": "after\n"}, "after\n"},
	}
	for _, c := range calls {
		put(t, p, "before\n")
		ctx, cancel := context.WithCancel(context.Background())
		beforeCheck, afterReplace = func(string) { cancel() }, nil
		_, res := f.callCtx(t, ctx, c.tool, c.in)
		failed(t, res, tool.ClassAborted, tool.AbortedText)
		if load(t, p) != "before\n" {
			t.Fatalf("%s: a call cancelled before its rename wrote", c.tool)
		}
		onlyEntries(t, f.env.Workspace, "c.txt")

		ctx, cancel = context.WithCancel(context.Background())
		beforeCheck, afterReplace = nil, func(string) { cancel() }
		_, res = f.callCtx(t, ctx, c.tool, c.in)
		ok(t, res)
		if load(t, p) != c.want || ctx.Err() == nil {
			t.Fatalf("%s: file %q, cancelled %v", c.tool, load(t, p), ctx.Err())
		}
	}
}

// TestEditRefusesHugeArguments: an oldString or newString over 5 MiB is
// refused before any matching, so no replacer ever holds one; one of
// exactly 5 MiB is edited, the negative control.
func TestEditRefusesHugeArguments(t *testing.T) {
	f := newFixture(t)
	body := strings.Repeat("a", maxEditBytes)
	put(t, f.path("big.txt"), body)
	_, res := f.edit(t, "big.txt", body+"a", "x", false)
	failed(t, res, tool.ClassOutputLimit, argsTooLarge)
	_, res = f.edit(t, "big.txt", "a", body+"a", false)
	failed(t, res, tool.ClassOutputLimit, argsTooLarge)
	_, res = f.edit(t, "new.txt", "", body+"a", false)
	failed(t, res, tool.ClassOutputLimit, argsTooLarge)
	if load(t, f.path("big.txt")) != body {
		t.Fatal("a refused edit changed the file")
	}
	ok(t, second(f.edit(t, "big.txt", body, "x", false)))
	if load(t, f.path("big.txt")) != "x" {
		t.Fatal("a 5 MiB oldString was not edited")
	}
}

// TestEditChangesOnlyTheSpan is the lost-bytes property through the whole
// tool, file and all: for a file with invalid UTF-8, lone CRs, BOM
// characters (a leading BOM included) and emoji, and an oldString found
// once, the file afterwards is its BOM plus strings.Replace of the rest.
// CRLF is left out, since the tool rewrites oldString and newString into a
// CRLF file's ending, and so is a newString starting with a BOM, which
// merges with the file's.
func TestEditChangesOnlyTheSpan(t *testing.T) {
	f := newFixture(t)
	rng := rand.New(rand.NewPCG(20, 26))
	b := string(rune(0xFEFF))
	tokens := []string{"a", "b", "foo", " ", "\t", "\n", "\n", "\r", "é", emoji, "\xff", b, "{", "}", "$1"}
	valid := []string{"a", "b", "foo", " ", "\n", "é", emoji, "$&"}
	gen := func(tokens []string, n int) string {
		var s strings.Builder
		for range n {
			s.WriteString(tokens[rng.IntN(len(tokens))])
		}
		return s.String()
	}
	p := f.path("prop.txt")
	checked := 0
	for range 5000 {
		lead := ""
		if rng.IntN(3) == 0 {
			lead = b
		}
		content := gen(tokens, 1+rng.IntN(60))
		i := rng.IntN(len(content))
		j := i + 1 + rng.IntN(min(30, len(content)-i))
		old, nu := content[i:j], gen(valid, rng.IntN(5))
		switch {
		case strings.Contains(lead+content, "\r\n"):
			continue // a CRLF file: oldString and newString are converted
		case lead == "" && strings.HasPrefix(content, b) && i < len(b):
			continue // oldString overlaps what the tool reads as the file's BOM
		case strings.ToValidUTF8(old, "?") != old:
			continue // JSON would not carry it unchanged
		case strings.Index(content, old) != strings.LastIndex(content, old), old == nu:
			continue
		}
		put(t, p, lead+content)
		_, res := f.edit(t, p, old, nu, false)
		want := lead + strings.Replace(content, old, nu, 1)
		if res.IsError || load(t, p) != want {
			t.Fatalf("file %q, edit %q -> %q: got %q (%+v), want %q", lead+content, old, nu, load(t, p), res, want)
		}
		checked++
	}
	if checked < 1000 {
		t.Fatalf("only %d cases were checked", checked)
	}
}
