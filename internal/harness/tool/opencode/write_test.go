package opencode

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/charliek/craze/internal/atomicfile"
	"github.com/charliek/craze/internal/harness/redact"
	"github.com/charliek/craze/internal/harness/tool"
)

func perm(t *testing.T, path string) fs.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}

// onlyEntries fails unless dir holds exactly want: no temp file was left.
func onlyEntries(t *testing.T, dir string, want ...string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s holds %q, want %q", dir, got, want)
	}
}

// umask022 runs the test under the usual umask, so the modes it checks are
// the ones a user sees; it restores the old one after.
func umask022(t *testing.T) {
	old := syscall.Umask(0o022)
	t.Cleanup(func() { syscall.Umask(old) })
}

// TestWriteNewFile ports "writes content to new file", "sets file
// permissions when writing sensitive data", "creates parent directories if
// needed" and "handles relative paths": opencode's result text, the edit
// the card diffs, mode 0644, parents 0755 — which is what this tool adds,
// since atomicfile.Write alone makes them 0700 (the negative control).
func TestWriteNewFile(t *testing.T) {
	umask022(t)
	f := newFixture(t)

	req, res := f.call(t, "write", map[string]any{"filePath": f.path("newfile.txt"), "content": "Hello, World!"})
	if ok(t, res) != "Wrote file successfully." {
		t.Fatalf("text = %q", res.Text)
	}
	if want := []tool.FileEdit{{Path: f.path("newfile.txt"), Old: "", New: "Hello, World!"}}; !reflect.DeepEqual(res.Edits, want) {
		t.Fatalf("edits = %+v, want %+v", res.Edits, want)
	}
	if load(t, f.path("newfile.txt")) != "Hello, World!" || perm(t, f.path("newfile.txt")) != 0o644 {
		t.Fatalf("file = %q mode %04o", load(t, f.path("newfile.txt")), perm(t, f.path("newfile.txt")))
	}
	if req.Title != "newfile.txt" || req.Kind != tool.KindEdit || req.ReadOnly {
		t.Fatalf("request = %+v", req)
	}

	_, res = f.call(t, "write", map[string]any{"filePath": "nested/deep/file.txt", "content": "nested content"})
	ok(t, res)
	if load(t, f.path("nested/deep/file.txt")) != "nested content" {
		t.Fatal("the nested file was not written")
	}
	for _, d := range []string{"nested", "nested/deep"} {
		if m := perm(t, f.path(d)); m != 0o755 {
			t.Errorf("%s is %04o, want 0755", d, m)
		}
	}
	onlyEntries(t, f.path("nested/deep"), "file.txt")

	plain := filepath.Join(t.TempDir(), "made", "by", "atomicfile.txt")
	if err := atomicfile.Write(plain, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if m := perm(t, filepath.Dir(plain)); m != 0o700 {
		t.Fatalf("atomicfile.Write made its parent %04o; the negative control expects 0700", m)
	}
}

// TestWriteOverwrites ports "overwrites existing file content", "returns
// diff in metadata for existing files", "returns relative path as title",
// and the content-type cases: JSON, binary-safe, empty, multi-line, and
// CRLF — each written byte for byte as given.
func TestWriteOverwrites(t *testing.T) {
	f := newFixture(t)
	put(t, f.path("existing.txt"), "old content")
	_, res := f.call(t, "write", map[string]any{"filePath": f.path("existing.txt"), "content": "new content"})
	ok(t, res)
	if load(t, f.path("existing.txt")) != "new content" {
		t.Fatal("not overwritten")
	}
	if want := []tool.FileEdit{{Path: f.path("existing.txt"), Old: "old content", New: "new content"}}; !reflect.DeepEqual(res.Edits, want) {
		t.Fatalf("edits = %+v, want %+v", res.Edits, want)
	}
	onlyEntries(t, f.env.Workspace, "existing.txt")

	req, _ := f.call(t, "write", map[string]any{"filePath": f.path("src/components/Button.tsx"), "content": "export const Button = () => {}"})
	if req.Title != filepath.Join("src", "components", "Button.tsx") {
		t.Fatalf("title = %q", req.Title)
	}

	contents := map[string]string{
		"data.json":      "{\n  \"key\": \"value\",\n  \"nested\": {\n    \"array\": [1, 2, 3]\n  }\n}",
		"binary.bin":     "Hello\x00World\x01\x02\x03",
		"empty.txt":      "",
		"multiline.txt":  "Line 1\nLine 2\nLine 3\n",
		"crlf.txt":       "Line 1\r\nLine 2\r\nLine 3",
		"mixed-ends.txt": "a\r\nb\nc\rd",
	}
	for name, content := range contents {
		_, res := f.call(t, "write", map[string]any{"filePath": f.path(name), "content": content})
		ok(t, res)
		if got := load(t, f.path(name)); got != content {
			t.Errorf("%s = %q, want %q", name, got, content)
		}
	}
}

// TestWriteKeepsTheBOM ports "preserves BOM when overwriting existing
// files": a BOM the file had, or the content brings, is written once; the
// edit shows text without it. A file with no BOM gets none: the negative
// control.
func TestWriteKeepsTheBOM(t *testing.T) {
	f := newFixture(t)
	const bomBytes = "\xef\xbb\xbf"
	cases := []struct {
		name, before, content, after, old, new string
		exists                                 bool
	}{
		{"the file's BOM is kept", bomBytes + "using System;\n", "using Up;\n", bomBytes + "using Up;\n", "using System;\n", "using Up;\n", true},
		{"the content's BOM is kept", "plain\n", bomBytes + "with bom\n", bomBytes + "with bom\n", "plain\n", "with bom\n", true},
		{"both: one BOM", bomBytes + "a\n", bomBytes + "b\n", bomBytes + "b\n", "a\n", "b\n", true},
		{"a new file with a BOM", "", bomBytes + "new\n", bomBytes + "new\n", "", "new\n", false},
		{"no BOM, none added", "plain\n", "still plain\n", "still plain\n", "plain\n", "still plain\n", true},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := f.path("bom" + string(rune('a'+i)) + ".cs")
			if tc.exists {
				put(t, p, tc.before)
			}
			_, res := f.call(t, "write", map[string]any{"filePath": p, "content": tc.content})
			ok(t, res)
			if got := load(t, p); got != tc.after {
				t.Fatalf("file = %q, want %q", got, tc.after)
			}
			if e := res.Edits[0]; e.Old != tc.old || e.New != tc.new {
				t.Fatalf("edit = %+v, want old %q new %q", e, tc.old, tc.new)
			}
		})
	}
}

// TestWriteKeepsTheMode: an existing file keeps its mode, where a plain
// rename would give it the temp file's; a new file is 0644.
func TestWriteKeepsTheMode(t *testing.T) {
	umask022(t)
	f := newFixture(t)
	for _, mode := range []fs.FileMode{0o600, 0o755, 0o640} {
		p := f.path("mode-" + mode.String())
		put(t, p, "before")
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
		_, res := f.call(t, "write", map[string]any{"filePath": p, "content": "after"})
		ok(t, res)
		if got := perm(t, p); got != mode || load(t, p) != "after" {
			t.Fatalf("%s: mode %04o content %q, want %04o and the new content", p, got, load(t, p), mode)
		}
	}
	_, res := f.call(t, "write", map[string]any{"filePath": f.path("fresh"), "content": "x"})
	ok(t, res)
	if got := perm(t, f.path("fresh")); got != 0o644 {
		t.Fatalf("a new file is %04o, want 0644", got)
	}
}

// TestWriteRefusesAReadOnlyFile ports "throws error when OS denies write
// access". A rename would replace a 0444 file without complaint, because it
// needs only the directory; the tool refuses, as opencode's in-place write
// fails. A writable file beside it is the negative control.
func TestWriteRefusesAReadOnlyFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root may write a read-only file")
	}
	f := newFixture(t)
	put(t, f.path("readonly.txt"), "test")
	if err := os.Chmod(f.path("readonly.txt"), 0o444); err != nil {
		t.Fatal(err)
	}
	_, res := f.call(t, "write", map[string]any{"filePath": f.path("readonly.txt"), "content": "new content"})
	if !res.IsError || res.Class != tool.ClassToolError || !strings.Contains(res.Text, "permission denied") {
		t.Fatalf("result = %+v, want permission denied", res)
	}
	if load(t, f.path("readonly.txt")) != "test" {
		t.Fatal("the read-only file changed")
	}
	put(t, f.path("writable.txt"), "test")
	_, res = f.call(t, "write", map[string]any{"filePath": f.path("writable.txt"), "content": "new content"})
	ok(t, res)
}

// TestWriteThroughASymlink: the link survives and its target is rewritten,
// through a relative link, a chain of links, and a dangling link (which
// creates the target). The negative control is atomicfile.Write on the
// link's own path, which replaces the link with a file.
func TestWriteThroughASymlink(t *testing.T) {
	f := newFixture(t)
	put(t, f.path("real/target.txt"), "old")
	must(t, os.Symlink(filepath.Join("real", "target.txt"), f.path("link.txt")))
	must(t, os.Symlink("link.txt", f.path("chain.txt")))
	must(t, os.Symlink(filepath.Join("real", "later.txt"), f.path("dangling.txt")))

	for _, tc := range []struct{ link, target, content string }{
		{"link.txt", "real/target.txt", "via link"},
		{"chain.txt", "real/target.txt", "via chain"},
		{"dangling.txt", "real/later.txt", "created through a dangling link"},
	} {
		_, res := f.call(t, "write", map[string]any{"filePath": tc.link, "content": tc.content})
		ok(t, res)
		info, err := os.Lstat(f.path(tc.link))
		if err != nil || info.Mode()&fs.ModeSymlink == 0 {
			t.Fatalf("%s is no longer a symlink", tc.link)
		}
		if got := load(t, f.path(tc.target)); got != tc.content {
			t.Fatalf("%s = %q, want %q", tc.target, got, tc.content)
		}
		if res.Edits[0].Path != f.path(tc.link) {
			t.Fatalf("edit path = %q, want the path as named", res.Edits[0].Path)
		}
	}

	must(t, atomicfile.Write(f.path("link.txt"), []byte("plain"), 0o644))
	if info, _ := os.Lstat(f.path("link.txt")); info.Mode()&fs.ModeSymlink != 0 {
		t.Fatal("the negative control failed: a plain rename kept the link")
	}
}

// TestEditKindTargets is what the mode gate judges (plan 023 §3.1): write and
// edit both name, in Request.Targets, the file their call will really land
// on — relative spellings joined to the workspace, symlinks followed, a
// dangling one followed to where its target would be, and a file that does
// not exist resolved as far as its directory does. Paths, the card's field,
// keeps the path as the call named it: the control that the two mean
// different things.
func TestEditKindTargets(t *testing.T) {
	f := newFixture(t)
	put(t, f.path("real/target.txt"), "old")
	must(t, os.Symlink(filepath.Join("real", "target.txt"), f.path("link.txt")))
	must(t, os.Symlink(filepath.Join("real", "later.txt"), f.path("dangling.txt")))
	real := func(name string) string {
		t.Helper()
		resolved, err := filepath.EvalSymlinks(f.path(name)) // macOS's /var is a link
		if err != nil {
			t.Fatal(err)
		}
		return resolved
	}

	for _, tc := range []struct{ name, spelled, want string }{
		{"a relative path", "real/target.txt", real("real/target.txt")},
		{"an absolute path", f.path("real/target.txt"), real("real/target.txt")},
		{"a symlink", "link.txt", real("real/target.txt")},
		{"a dangling symlink", "dangling.txt", filepath.Join(real("real"), "later.txt")},
		{"a file that is not there yet", "nested/deep/new.txt", filepath.Join(real("."), "nested/deep/new.txt")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for name, args := range map[string]map[string]any{
				"write": {"filePath": tc.spelled, "content": "x"},
				"edit":  {"filePath": tc.spelled, "oldString": "a", "newString": "b"},
			} {
				req := prepareOnly(t, name, f.env, args)
				if len(req.Targets) != 1 || req.Targets[0] != tc.want {
					t.Errorf("%s: Targets = %q, want [%q]", name, req.Targets, tc.want)
				}
				if len(req.Paths) != 1 || req.Paths[0] != f.env.Resolve(tc.spelled) {
					t.Errorf("control: %s: Paths = %q, want the path as named", name, req.Paths)
				}
			}
		})
	}
}

// prepareOnly is the Request a tool's own Prepare builds, before the
// dispatcher redacts it and drops its Targets. Nothing runs.
func prepareOnly(t *testing.T, name string, env tool.Env, args map[string]any) tool.Request {
	t.Helper()
	build := newWrite
	if name == "edit" {
		build = newEdit
	}
	tl, err := build()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	p, err := tl.Prepare(env, tool.Call{ID: "t1.1.1", Tool: name, Input: raw})
	if err != nil {
		t.Fatalf("%s.Prepare: %v", name, err)
	}
	return p.Request()
}

// TestPlanModeThroughTheGate is plan mode where the model actually meets it:
// the production write tool, prepared and run through the dispatcher with the
// session's mode gate over it (plan 023 §3.1). The plan file is the one file
// an edit may touch, however the call spells it — absolute, relative to the
// workspace, through a directory symlink, through ".." — because the tool
// resolves its target the same way the gate resolved the plan path. Nothing
// else is: a workspace file and a path that will not resolve at all are
// refused with grok-build's text, and neither reaches the disk.
func TestPlanModeThroughTheGate(t *testing.T) {
	f := newFixture(t)
	plan := filepath.Join(f.env.Home, "20260921T120000Z_s.plan.md")
	put(t, plan, "# plan\n")
	g := tool.NewModeGate(tool.ModePlan, nil) // nil inner is AllowAll, as a session's is today
	g.SetPlanPath(plan)
	p, err := Profile()
	if err != nil {
		t.Fatal(err)
	}
	d, err := tool.NewDispatcher(tool.Options{Tools: p.Tools, Gate: g, Env: f.env})
	if err != nil {
		t.Fatal(err)
	}
	f.d = d // every f.call from here goes through the mode's gate

	// A directory symlink onto the harness home, and two links that point at
	// each other, which nothing can resolve: the tool then names the path as
	// it was spelled, and the gate refuses that (amendment X4).
	must(t, os.Symlink(f.env.Home, f.path("home-link")))
	must(t, os.Symlink(f.path("loop-b"), f.path("loop-a")))
	must(t, os.Symlink(f.path("loop-a"), f.path("loop-b")))
	must(t, os.MkdirAll(f.path("sub"), 0o700))
	put(t, f.path("a.txt"), "alpha\n")
	rel, err := filepath.Rel(f.env.Workspace, plan)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name, path string
		allowed    bool
	}{
		{name: "the plan file, absolute", path: plan, allowed: true},
		{name: "the plan file, relative to the workspace", path: rel, allowed: true},
		{name: "the plan file through a directory symlink", path: filepath.Join("home-link", filepath.Base(plan)), allowed: true},
		{name: "the plan file through ..", path: filepath.Join("sub", "..", rel), allowed: true},
		{name: "a file in the workspace", path: "a.txt"},
		{name: "a path that will not resolve", path: "loop-a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			content := "## " + tc.name + "\n"
			_, res := f.call(t, "write", map[string]any{"filePath": tc.path, "content": content})
			if tc.allowed {
				ok(t, res)
				if got := load(t, plan); got != content {
					t.Fatalf("the plan file holds %q, want %q", got, content)
				}
				return
			}
			if !res.IsError || res.Class != tool.ClassDenied || !strings.Contains(res.Text, "the only editable file") {
				t.Fatalf("result = {IsError:%v Class:%q Text:%q}, want the plan-mode refusal", res.IsError, res.Class, res.Text)
			}
			if got := load(t, f.path("a.txt")); got != "alpha\n" {
				t.Fatalf("the refused write changed a.txt: %q", got)
			}
		})
	}
}

// TestWriteRefusesAnExternalChange: a file changed between the tool's read
// and its rename — by an editor, a formatter, anyone outside craze's lock —
// is left as they made it, and the call fails. A new file that appears in
// that window is refused too. A check that sees no change is the negative
// control.
func TestWriteRefusesAnExternalChange(t *testing.T) {
	f := newFixture(t)
	t.Cleanup(func() { beforeCheck = nil })

	put(t, f.path("shared.txt"), "original")
	beforeCheck = func(string) { put(t, f.path("shared.txt"), "the editor's change") }
	_, res := f.call(t, "write", map[string]any{"filePath": "shared.txt", "content": "craze's change"})
	failed(t, res, tool.ClassToolError, changedText)
	if load(t, f.path("shared.txt")) != "the editor's change" {
		t.Fatal("the external change was overwritten")
	}

	// Same size, and the modification time put back: the inode differs,
	// because the editor replaced the file.
	put(t, f.path("same.txt"), "aaaa")
	before, err := os.Stat(f.path("same.txt"))
	must(t, err)
	beforeCheck = func(string) {
		must(t, atomicfile.Write(f.path("same.txt"), []byte("bbbb"), 0o644))
		must(t, os.Chtimes(f.path("same.txt"), before.ModTime(), before.ModTime()))
	}
	_, res = f.call(t, "write", map[string]any{"filePath": "same.txt", "content": "cccc"})
	failed(t, res, tool.ClassToolError, changedText)

	beforeCheck = func(string) { put(t, f.path("appeared.txt"), "someone else's") }
	_, res = f.call(t, "write", map[string]any{"filePath": "appeared.txt", "content": "mine"})
	failed(t, res, tool.ClassToolError, changedText)
	if load(t, f.path("appeared.txt")) != "someone else's" {
		t.Fatal("a file created in the window was overwritten")
	}
	onlyEntries(t, f.env.Workspace, "appeared.txt", "same.txt", "shared.txt")

	beforeCheck = func(string) {}
	_, res = f.call(t, "write", map[string]any{"filePath": "shared.txt", "content": "craze's change"})
	ok(t, res)
	if load(t, f.path("shared.txt")) != "craze's change" {
		t.Fatal("an unchanged file was not written")
	}
}

// TestWriteOverALargeFileIsNotLoaded: a file over the cap is replaced
// without being read, so overwriting a huge one costs neither its size in
// memory for the card's diff nor a second read for the last check. Two
// things prove it was not read: the result carries no edit to diff, and a
// change only the content check could see is not seen. The same file under
// the cap is the negative control for both, and a change the inode, size
// and time do show is still refused above the cap.
func TestWriteOverALargeFileIsNotLoaded(t *testing.T) {
	f := newFixture(t)
	t.Cleanup(func() { maxWriteBytes, beforeCheck = maxEditBytes, nil })
	maxWriteBytes = 16

	// A rewrite in place of the same size, with the modification time put
	// back: the same inode, size and time, and only the content differs.
	inPlace := func(path, content string) func(string) {
		return func(string) {
			before, err := os.Stat(path)
			must(t, err)
			must(t, os.WriteFile(path, []byte(content), 0o644))
			must(t, os.Chtimes(path, before.ModTime(), before.ModTime()))
		}
	}

	big := strings.Repeat("a", 64)
	put(t, f.path("big.txt"), big)
	beforeCheck = inPlace(f.path("big.txt"), strings.Repeat("b", 64))
	_, res := f.call(t, "write", map[string]any{"filePath": "big.txt", "content": "small now"})
	ok(t, res)
	if res.Edits != nil {
		t.Fatalf("edits = %+v, want none: the old content was never read", res.Edits)
	}
	if load(t, f.path("big.txt")) != "small now" {
		t.Fatal("the large file was not written")
	}

	// The negative control: under the cap the same change is caught, and
	// the result carries the edit the card diffs.
	put(t, f.path("small.txt"), "aaaa")
	beforeCheck = inPlace(f.path("small.txt"), "bbbb")
	_, res = f.call(t, "write", map[string]any{"filePath": "small.txt", "content": "x"})
	failed(t, res, tool.ClassToolError, changedText)
	beforeCheck = nil
	_, res = f.call(t, "write", map[string]any{"filePath": "small.txt", "content": "x"})
	ok(t, res)
	if want := []tool.FileEdit{{Path: f.path("small.txt"), Old: "bbbb", New: "x"}}; !reflect.DeepEqual(res.Edits, want) {
		t.Fatalf("edits = %+v, want %+v", res.Edits, want)
	}

	// Above the cap the inode, size and time still guard the file.
	put(t, f.path("big2.txt"), big)
	beforeCheck = func(string) { put(t, f.path("big2.txt"), big+"grown") }
	_, res = f.call(t, "write", map[string]any{"filePath": "big2.txt", "content": "y"})
	failed(t, res, tool.ClassToolError, changedText)
	if load(t, f.path("big2.txt")) != big+"grown" {
		t.Fatal("the external change was overwritten")
	}
	beforeCheck = nil

	// A byte order mark is kept over the cap too: only those bytes are read.
	put(t, f.path("bom.txt"), "\xef\xbb\xbf"+big)
	_, res = f.call(t, "write", map[string]any{"filePath": "bom.txt", "content": "after"})
	ok(t, res)
	if got := load(t, f.path("bom.txt")); got != "\xef\xbb\xbfafter" {
		t.Fatalf("file = %q, want its BOM kept", got)
	}
	// The negative control: no mark, none added.
	put(t, f.path("plain.txt"), big)
	_, res = f.call(t, "write", map[string]any{"filePath": "plain.txt", "content": "after"})
	ok(t, res)
	if got := load(t, f.path("plain.txt")); got != "after" {
		t.Fatalf("file = %q, want no BOM", got)
	}
}

// TestWriteRefusesTheMarker: content holding the redaction marker would
// put the marker on disk in place of a real key (plan 019 §3.8). Content
// with only part of it is the negative control.
func TestWriteRefusesTheMarker(t *testing.T) {
	f := newFixture(t)
	put(t, f.path(".env"), "KEY=real\n")
	_, res := f.call(t, "write", map[string]any{"filePath": ".env", "content": "KEY=" + redact.Marker + "\nMORE=1\n"})
	failed(t, res, tool.ClassInvalidInput, markerText)
	if load(t, f.path(".env")) != "KEY=real\n" {
		t.Fatal("the file changed")
	}
	_, res = f.call(t, "write", map[string]any{"filePath": ".env", "content": "KEY=[craze:redacted\n"})
	ok(t, res)
}

// TestWriteRefusesTheCredentialsFile: <Home>/providers.toml is refused
// whether it exists or not, and through a symlink or a hard link; nothing
// is written. models.toml beside it is the negative control.
func TestWriteRefusesTheCredentialsFile(t *testing.T) {
	f := newFixture(t)
	cred := filepath.Join(f.env.Home, CredentialsFile)
	must(t, os.Symlink(cred, f.path("sneaky.toml")))

	_, res := f.call(t, "write", map[string]any{"filePath": cred, "content": "x"})
	failed(t, res, tool.ClassToolError, credentialsText)
	_, res = f.call(t, "write", map[string]any{"filePath": "sneaky.toml", "content": "x"})
	failed(t, res, tool.ClassToolError, credentialsText)
	if _, err := os.Lstat(cred); err == nil {
		t.Fatal("the credentials file was created")
	}

	put(t, cred, "api_key = \""+keyA+"\"\n")
	must(t, os.Link(cred, f.path("hard.toml")))
	for _, p := range []string{cred, f.path("sneaky.toml"), f.path("hard.toml")} {
		_, res := f.call(t, "write", map[string]any{"filePath": p, "content": "x"})
		failed(t, res, tool.ClassToolError, credentialsText)
	}
	if load(t, cred) != "api_key = \""+keyA+"\"\n" {
		t.Fatal("the credentials file changed")
	}
	_, res = f.call(t, "write", map[string]any{"filePath": filepath.Join(f.env.Home, "models.toml"), "content": "version = 1\n"})
	ok(t, res)
}

// TestWriteRefusesWhatIsNotAFile: a directory (opencode's own words), a
// FIFO — refused at once, not opened to wait for a reader — and a device.
func TestWriteRefusesWhatIsNotAFile(t *testing.T) {
	f := newFixture(t)
	must(t, os.Mkdir(f.path("adir"), 0o755))
	_, res := f.call(t, "write", map[string]any{"filePath": "adir", "content": "x"})
	failed(t, res, tool.ClassToolError, "Path is a directory, not a file: "+f.path("adir"))

	must(t, syscall.Mkfifo(f.path("fifo"), 0o644))
	done := make(chan tool.Result, 1)
	go func() {
		_, res := f.call(t, "write", map[string]any{"filePath": "fifo", "content": "x"})
		done <- res
	}()
	select {
	case res := <-done:
		failed(t, res, tool.ClassToolError, "Path is not a regular file: "+f.path("fifo"))
	case <-time.After(10 * time.Second):
		t.Fatal("writing a FIFO hung")
	}
	_, res = f.call(t, "write", map[string]any{"filePath": "/dev/null", "content": "x"})
	failed(t, res, tool.ClassToolError, "Path is not a regular file: /dev/null")
}

// TestWriteTakesThePathLock: a write waits for craze's lock on the file's
// resolved path — whichever name the other holder used — and gives up,
// aborted and writing nothing, when cancelled while it waits. With the lock
// free it writes: the negative control.
func TestWriteTakesThePathLock(t *testing.T) {
	f := newFixture(t)
	put(t, f.path("locked.txt"), "before")
	must(t, os.Symlink("locked.txt", f.path("alias.txt")))
	real, err := filepath.EvalSymlinks(f.path("locked.txt"))
	must(t, err)
	unlock, err := f.env.Locks.Lock(context.Background(), real)
	must(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan tool.Result, 1)
	go func() {
		_, res := f.callCtx(t, ctx, "write", map[string]any{"filePath": "alias.txt", "content": "after"})
		done <- res
	}()
	select {
	case res := <-done:
		t.Fatalf("the write did not wait for the lock: %+v", res)
	case <-time.After(100 * time.Millisecond):
	}
	cancel()
	select {
	case res := <-done:
		failed(t, res, tool.ClassAborted, tool.AbortedText)
	case <-time.After(10 * time.Second):
		t.Fatal("a cancelled write kept waiting")
	}
	if load(t, f.path("locked.txt")) != "before" {
		t.Fatal("a cancelled write wrote")
	}

	unlock()
	_, res := f.call(t, "write", map[string]any{"filePath": "alias.txt", "content": "after"})
	ok(t, res)
	if load(t, f.path("locked.txt")) != "after" {
		t.Fatal("not written")
	}
}

// TestWriteResultIsRedacted: a key in the file's old content reaches the
// card's edit only as the marker. Content without one is the negative
// control.
func TestWriteResultIsRedacted(t *testing.T) {
	f := newFixture(t)
	put(t, f.path(".env"), "KEY="+keyA+"\n")
	_, res := f.call(t, "write", map[string]any{"filePath": ".env", "content": "KEY=rotated\n"})
	ok(t, res)
	raw, _ := json.Marshal(res)
	if strings.Contains(string(raw), keyA) || res.Edits[0].Old != "KEY="+redact.Marker+"\n" || res.Edits[0].New != "KEY=rotated\n" {
		t.Fatalf("result = %s", raw)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
