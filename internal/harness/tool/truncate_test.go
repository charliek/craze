package tool

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func numbered(n int) string {
	lines := make([]string, n)
	for i := range lines {
		lines[i] = fmt.Sprintf("line%d", i)
	}
	return strings.Join(lines, "\n")
}

// The first group ports opencode's own cases (test/tool/truncation.test.ts),
// with its per-call limits as truncate's limits argument.

func TestTruncateUnderLimitsIsUnchanged(t *testing.T) {
	home := t.TempDir()
	text := "line1\nline2\nline3"
	got, tr := Truncate(home, "t1.1.1", text, Head)
	if got != text || tr != (Truncation{17, 17, 3, 3, ""}) || tr.Truncated() {
		t.Fatalf("Truncate = %q, %+v", got, tr)
	}
	// "does not write file when not truncated"
	if _, err := os.Stat(filepath.Join(home, SpillDir)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a spill directory appeared for a short text: %v", err)
	}
}

func TestTruncateByLineCount(t *testing.T) {
	got, tr := truncate(t.TempDir(), "t1.1.1", numbered(100), Head, limits{10, MaxBytes}, nil)
	if !strings.Contains(got, "...90 lines truncated...") || tr.KeptLines != 10 || tr.TotalLines != 100 {
		t.Fatalf("got %q, %+v", got, tr)
	}
}

func TestTruncateByByteCount(t *testing.T) {
	got, tr := truncate(t.TempDir(), "t1.1.1", strings.Repeat("a", 1000), Head, limits{MaxLines, 100}, nil)
	// One line over the byte limit keeps nothing of itself, as in opencode.
	if !strings.Contains(got, "...1000 bytes truncated...") || tr.KeptBytes != 0 || tr.TotalBytes != 1000 {
		t.Fatalf("got %q, %+v", got, tr)
	}
}

func TestTruncateHeadAndTail(t *testing.T) {
	text := numbered(10)
	head, _ := truncate(t.TempDir(), "t1.1.1", text, Head, limits{3, MaxBytes}, nil)
	for _, want := range []string{"line0", "line1", "line2"} {
		if !strings.Contains(head, want) {
			t.Fatalf("head %q lacks %q", head, want)
		}
	}
	if strings.Contains(head, "line9") {
		t.Fatalf("head %q keeps line9", head)
	}
	tail, _ := truncate(t.TempDir(), "t1.1.1", text, Tail, limits{3, MaxBytes}, nil)
	for _, want := range []string{"line7", "line8", "line9"} {
		if !strings.Contains(tail, want) {
			t.Fatalf("tail %q lacks %q", tail, want)
		}
	}
	if strings.Contains(tail, "line0") {
		t.Fatalf("tail %q keeps line0", tail)
	}
}

func TestTruncateDefaults(t *testing.T) {
	if MaxLines != 2000 || MaxBytes != 50*1024 {
		t.Fatalf("limits %d lines, %d bytes; want opencode's 2000 and 50 KiB", MaxLines, MaxBytes)
	}
	// A large single line: the byte message.
	got, _ := Truncate(t.TempDir(), "t1.1.1", strings.Repeat("{\"k\":1}", 10000), Head)
	if !strings.Contains(got, "bytes truncated...") {
		t.Fatalf("got %.200q", got)
	}
}

// TestTruncateLayout pins the whole text around the preview, both ways,
// and that the spill file holds the full text.
func TestTruncateLayout(t *testing.T) {
	home := t.TempDir()
	text := numbered(100)
	spill := filepath.Join(home, SpillDir, "tool_t1.1.1")
	hint := "The tool call succeeded but the output was truncated. Full output saved to: " + spill +
		"\nUse Grep to search the full content or Read with offset/limit to view specific sections."

	got, tr := truncate(home, "t1.1.1", text, Head, limits{2, MaxBytes}, nil)
	if want := "line0\nline1\n\n...98 lines truncated...\n\n" + hint; got != want {
		t.Fatalf("head =\n%q\nwant\n%q", got, want)
	}
	if tr != (Truncation{KeptBytes: 11, TotalBytes: len(text), KeptLines: 2, TotalLines: 100, Spill: spill}) {
		t.Fatalf("Trunc = %+v", tr)
	}
	if data, err := os.ReadFile(spill); err != nil || string(data) != text {
		t.Fatalf("spill = %q, %v; want the full text", data, err)
	}

	got, tr = truncate(home, "t1.1.2", text, Tail, limits{2, MaxBytes}, nil)
	spill2 := filepath.Join(home, SpillDir, "tool_t1.1.2")
	hint2 := strings.Replace(hint, spill, spill2, 1)
	if want := "...98 lines truncated...\n\n" + hint2 + "\n\nline98\nline99"; got != want {
		t.Fatalf("tail =\n%q\nwant\n%q", got, want)
	}
	if tr.Spill != spill2 || tr.KeptLines != 2 {
		t.Fatalf("Trunc = %+v", tr)
	}
	// The hint never mentions a Task tool: opencode's variant that sends the
	// spill file to a sub-agent stays out, agent tool or not (plan 026 §3.3).
	if strings.Contains(got, "Task") {
		t.Fatal("the hint names the Task tool")
	}
}

func TestSpillFilesArePrivateAndNeverOverwritten(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, SpillDir)
	// A directory someone loosened is tightened back.
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	planted := filepath.Join(dir, "tool_t1.1.1")
	if err := os.WriteFile(planted, []byte("planted"), 0o644); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(outside, []byte("target"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "tool_t1.1.2")); err != nil {
		t.Fatal(err)
	}

	suffixed := regexp.MustCompile(`^tool_t1\.1\.[12]-[0-9a-f]{8}$`)
	for _, id := range []string{"t1.1.1", "t1.1.2"} {
		_, tr := truncate(home, id, numbered(100), Head, limits{2, MaxBytes}, nil)
		if !suffixed.MatchString(filepath.Base(tr.Spill)) || filepath.Dir(tr.Spill) != dir {
			t.Fatalf("id %s spilled to %q, want a suffixed name beside the taken one", id, tr.Spill)
		}
		if data, _ := os.ReadFile(tr.Spill); string(data) != numbered(100) {
			t.Fatalf("id %s: spill content wrong", id)
		}
		if m := mode(t, tr.Spill); m != 0o600 {
			t.Fatalf("spill mode %04o, want 0600", m)
		}
	}
	if data, _ := os.ReadFile(planted); string(data) != "planted" {
		t.Fatal("a planted file was overwritten")
	}
	if data, _ := os.ReadFile(outside); string(data) != "target" {
		t.Fatal("a planted symlink was followed")
	}
	if m := mode(t, dir); m != 0o700 {
		t.Fatalf("spill directory mode %04o, want 0700", m)
	}

	// The negative control: a free name is used as it is.
	_, tr := truncate(home, "t1.1.3", numbered(100), Head, limits{2, MaxBytes}, nil)
	if tr.Spill != filepath.Join(dir, "tool_t1.1.3") {
		t.Fatalf("free name spilled to %q", tr.Spill)
	}
}

// TestSpillRefusals: a symlinked spill directory and an unsafe id each mean
// no spill file — the text is still cut, and says the rest was not saved.
func TestSpillRefusals(t *testing.T) {
	home := t.TempDir()
	elsewhere := t.TempDir()
	if err := os.Symlink(elsewhere, filepath.Join(home, SpillDir)); err != nil {
		t.Fatal(err)
	}
	got, tr := truncate(home, "t1.1.1", numbered(100), Head, limits{2, MaxBytes}, nil)
	if tr.Spill != "" || !tr.Truncated() || !strings.HasSuffix(got, "The tool call succeeded but the output was truncated. The full output could not be saved.") {
		t.Fatalf("got %q, %+v", got, tr)
	}
	if entries, _ := os.ReadDir(elsewhere); len(entries) != 0 {
		t.Fatalf("a spill file went through the symlink: %v", entries)
	}

	for _, id := range []string{"", "..", "../x", "a/b", ".hidden"} {
		if f, err := OpenSpill(t.TempDir(), id); err == nil {
			f.Close()
			t.Fatalf("OpenSpill accepted id %q", id)
		}
	}
	if _, err := OpenSpill("relative/home", "t1.1.1"); err == nil {
		t.Fatal("OpenSpill accepted a relative home")
	}
	// The negative control.
	f, err := OpenSpill(t.TempDir(), "t12.34.5")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
}

// atSpillCheck runs f in the window between openSpillDir's check of the
// directory and its open of it, for the rest of the test.
func atSpillCheck(t *testing.T, f func()) {
	t.Helper()
	prev := spillDirChecked
	spillDirChecked = f
	t.Cleanup(func() { spillDirChecked = prev })
}

// TestSpillDirSwappedAfterTheCheck forces open the race a model with bash
// could try: tool-output passes the check as a real directory, then is
// renamed away and a symlink put in its place before it is opened. A link
// leaving home is refused by os.Root; one to another directory inside home
// (relative, as os.Root requires) is refused by the same-file comparison.
// Either way no spill file is written through the link and Sweep neither
// lists nor removes anything behind it. The negative control, with no
// swap, writes the spill and sweeps the old file.
func TestSpillDirSwappedAfterTheCheck(t *testing.T) {
	old := time.Now().Add(-10 * 24 * time.Hour)
	plant := func(t *testing.T, dir string) string {
		p := filepath.Join(dir, "tool_victim")
		if err := os.WriteFile(p, []byte("victim"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
		return p
	}
	for _, where := range []string{"outside home", "inside home"} {
		t.Run(where, func(t *testing.T) {
			home := t.TempDir()
			dir := filepath.Join(home, SpillDir)
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			target, link := t.TempDir(), ""
			link = target
			if where == "inside home" {
				target, link = filepath.Join(home, "elsewhere"), "elsewhere"
				if err := os.Mkdir(target, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			victim := plant(t, target)
			ours := plant(t, dir)
			swap := func() {
				if err := os.Rename(dir, dir+".moved"); err != nil {
					t.Error(err)
				}
				if err := os.Symlink(link, dir); err != nil {
					t.Error(err)
				}
			}
			unswap := func() {
				if err := os.Remove(dir); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(dir+".moved", dir); err != nil {
					t.Fatal(err)
				}
			}

			atSpillCheck(t, swap)
			if _, tr := truncate(home, "t1.1.1", numbered(100), Head, limits{2, MaxBytes}, nil); tr.Spill != "" {
				t.Fatalf("a spill went through the swapped directory to %s", tr.Spill)
			}
			unswap()
			if n, err := Sweep(home, time.Now()); err == nil || n != 0 {
				t.Fatalf("Sweep through the swap = %d, %v; want an error and nothing removed", n, err)
			}
			unswap()
			entries, _ := os.ReadDir(target)
			if len(entries) != 1 || entries[0].Name() != "tool_victim" {
				t.Fatalf("the link's target holds %v, want only tool_victim", entries)
			}
			if _, err := os.Stat(victim); err != nil {
				t.Fatal("Sweep removed a file behind the link")
			}

			// The negative control: the same calls, with nothing swapped.
			atSpillCheck(t, func() {})
			if _, tr := truncate(home, "t1.1.1", numbered(100), Head, limits{2, MaxBytes}, nil); tr.Spill != filepath.Join(dir, "tool_t1.1.1") {
				t.Fatalf("without a swap, spill = %q", tr.Spill)
			}
			if n, err := Sweep(home, time.Now()); err != nil || n != 1 {
				t.Fatalf("without a swap, Sweep = %d, %v; want the one old file", n, err)
			}
			if _, err := os.Stat(ours); !errors.Is(err, fs.ErrNotExist) {
				t.Fatal("the old spill file in the real directory survived")
			}
		})
	}
}

func TestSweep(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, SpillDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	old, recent := now.Add(-10*24*time.Hour), now.Add(-3*24*time.Hour)
	write := func(name string, mtime time.Time) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatal(err)
		}
		return p
	}
	oldFile := write("tool_t1.1.1", old)
	recentFile := write("tool_t9.9.9", recent)
	notOurs := write("notes.txt", old)
	// An old symlink and an old directory under our prefix are left alone,
	// and the symlink's target is untouched.
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("target"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "tool_link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(target, old, old); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(dir, "tool_dir")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(sub, old, old); err != nil {
		t.Fatal(err)
	}

	n, err := Sweep(home, now)
	if err != nil || n != 1 {
		t.Fatalf("Sweep = %d, %v; want 1 removed", n, err)
	}
	if _, err := os.Lstat(oldFile); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("the old spill file survived")
	}
	// The negative controls: everything else is still there.
	for _, p := range []string{recentFile, notOurs, link, sub, target} {
		if _, err := os.Lstat(p); err != nil {
			t.Fatalf("%s was removed: %v", p, err)
		}
	}

	// No directory: nothing to do. A symlinked directory: refused, and
	// what it points at is untouched.
	if n, err := Sweep(t.TempDir(), now); n != 0 || err != nil {
		t.Fatalf("Sweep of an empty home = %d, %v", n, err)
	}
	linked := t.TempDir()
	elsewhere := t.TempDir()
	victim := filepath.Join(elsewhere, "tool_x")
	if err := os.WriteFile(victim, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(victim, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(linked, SpillDir)); err != nil {
		t.Fatal(err)
	}
	if _, err := Sweep(linked, now); err == nil {
		t.Fatal("Sweep followed a symlinked directory without complaint")
	}
	if _, err := os.Lstat(victim); err != nil {
		t.Fatal("Sweep removed a file through a symlinked directory")
	}
}
