package tui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// The composer's `@` file source (at_files.go, plan 030 §3.16): the search —
// its tools, fallbacks, bounds and cancellation, each forced with fake tools
// and a held clock rather than the machine's rg and git or the real 3 s —
// and the match, pinned query by query. Nothing is wired to the composer in
// C17: the popup-level tests hold a popup over the source and play its
// owner, as complete_test.go does.

// atFilesStep bounds each wait of a search test on its own. Generous: a
// search of 50,000 paths at a 5 % CPU quota under -race is slow, and the
// bound is there to catch a search that never returns, not to time one.
const atFilesStep = 30 * time.Second

// atClock is the search's timer as a test holds it: it records the duration
// the search armed it with, and fires only when the test says.
type atClock struct {
	mu sync.Mutex
	d  time.Duration
	f  func()
}

func newAtClock() *atClock { return &atClock{} }

func (c *atClock) after(d time.Duration, f func()) func() bool {
	c.mu.Lock()
	c.d, c.f = d, f
	c.mu.Unlock()
	return func() bool { return true }
}

// fire runs the timer's function, as the clock would at the timeout, and
// says whether the search had armed it. Any goroutine may call it (a search
// hook does).
func (c *atClock) fire() bool {
	c.mu.Lock()
	f := c.f
	c.mu.Unlock()
	if f == nil {
		return false
	}
	f()
	return true
}

func (c *atClock) duration() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.d
}

// atSearcherFor is the searcher with tools found only where tools says (a
// name to its path; anything else is not on PATH), gitTree for whether the
// root is a git work tree, and clock as its timer.
func atSearcherFor(clock *atClock, tools map[string]string, gitTree bool) atFileSearcher {
	s := newAtFileSearcher()
	s.look = func(name string) (string, error) {
		if p, ok := tools[name]; ok {
			return p, nil
		}
		return "", &exec.Error{Name: name, Err: exec.ErrNotFound}
	}
	s.gitTree = func(string) bool { return gitTree }
	s.after = clock.after
	return s
}

// atShellQuote is s single-quoted for sh, any byte but NUL allowed.
func atShellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// atFakeBin is the directory of the fake rg and git, written once by
// TestMain before any test runs (installAtFakeTools) and never changed: a
// script written while another goroutine forks can fail to run with ETXTBSY
// — the forked child holds it open for writing until it execs
// (go.dev/issue/22315), as internal/harness's fake rgs learned. A test says
// what the fakes do in body files they source (atFakes), in a directory of
// its own that atFakeDirEnv names.
var atFakeBin string

const atFakeDirEnv = "CRAZE_AT_FAKE_DIR"

// installAtFakeTools writes the fake rg and git. Each records, in the test's
// directory, its working directory (physical) in <name>.cwd, its arguments
// one to a line in <name>.args and a line per run in <name>.runs, then
// sources <name>.sh from there.
func installAtFakeTools() error {
	dir, err := os.MkdirTemp("", "craze-at-fakes-")
	if err != nil {
		return err
	}
	for _, name := range []string{"rg", "git"} {
		script := "#!/bin/sh\nd=\"$" + atFakeDirEnv + "\"\n" +
			"pwd -P > \"$d/" + name + ".cwd\"\n" +
			"printf '%s\\n' \"$@\" > \"$d/" + name + ".args\"\n" +
			"echo run >> \"$d/" + name + ".runs\"\n" +
			". \"$d/" + name + ".sh\"\n"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			_ = os.RemoveAll(dir)
			return err
		}
	}
	atFakeBin = dir
	return nil
}

// atFakeSet is one test's fake tools: where each is, and the directory their
// bodies and records are in.
type atFakeSet struct {
	dir   string
	tools map[string]string
}

// atFakes gives the fakes named in bodies those bodies for the rest of the
// test, and returns them: the tools a searcher finds (atSearcherFor) — only
// these.
func atFakes(t *testing.T, bodies map[string]string) atFakeSet {
	t.Helper()
	if atFakeBin == "" {
		t.Fatal("TestMain installed no fake tools")
	}
	set := atFakeSet{dir: t.TempDir(), tools: map[string]string{}}
	t.Setenv(atFakeDirEnv, set.dir)
	for name, body := range bodies {
		if err := os.WriteFile(filepath.Join(set.dir, name+".sh"), []byte(body+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		set.tools[name] = filepath.Join(atFakeBin, name)
	}
	return set
}

// record is what the fake name recorded as what (cwd, args, runs).
func (f atFakeSet) record(t *testing.T, name, what string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(f.dir, name+"."+what))
	if err != nil {
		t.Fatalf("%s recorded no %s: %v", name, what, err)
	}
	return string(b)
}

// ran says the fake name ran.
func (f atFakeSet) ran(name string) bool {
	_, err := os.Stat(filepath.Join(f.dir, name+".runs"))
	return err == nil
}

// atPrintPaths is a script line printing each path followed by NUL.
func atPrintPaths(paths ...string) string {
	if len(paths) == 0 {
		return ":"
	}
	var b strings.Builder
	b.WriteString(`printf '%s\0'`)
	for _, p := range paths {
		b.WriteString(" ")
		b.WriteString(atShellQuote(p))
	}
	return b.String()
}

// atStall is a script line that leaves the tool running for good with its
// output still open: exec, so the process the search started is the one
// that must be killed.
const atStall = "exec sleep 3600"

// atSearch runs s.search on a goroutine, within one step.
func atSearch(t *testing.T, s atFileSearcher, ctx context.Context, root string) completeLoaded {
	t.Helper()
	ch := make(chan completeLoaded, 1)
	go func() { ch <- s.search(ctx, root) }()
	select {
	case l := <-ch:
		return l
	case <-time.After(atFilesStep):
		t.Fatalf("the search of %s did not return within %v", root, atFilesStep)
		return completeLoaded{}
	}
}

// atIndexOf is the index a search answered, or the test's failure.
func atIndexOf(t *testing.T, l completeLoaded) *atFileIndex {
	t.Helper()
	x, ok := l.Data.(*atFileIndex)
	if l.Err != nil || !ok || x == nil {
		t.Fatalf("the search answered no index: err %v, data %T", l.Err, l.Data)
	}
	return x
}

// atReaped says the process pid is gone: killed and reaped, not a zombie (a
// zombie still answers signal 0).
func atReaped(t *testing.T, pid int) {
	t.Helper()
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("the tool (pid %d) is still there after the search returned: signal 0 answered %v", pid, err)
	}
}

// atAwait is the next value on ch, within one step.
func atAwait[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(atFilesStep):
		t.Fatalf("%s: nothing within %v", what, atFilesStep)
		var zero T
		return zero
	}
}

// atTree makes files (and, for a name ending in "/", empty directories)
// under a new temporary root.
func atTree(t *testing.T, names ...string) string {
	t.Helper()
	root := t.TempDir()
	for _, n := range names {
		p := filepath.Join(root, n)
		if strings.HasSuffix(n, "/") {
			if err := os.MkdirAll(p, 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// matchNames is the names the index offers for query, best first, and how
// many more matched.
func matchNames(x *atFileIndex, query string, k int) ([]string, int) {
	items, more := x.match(query, k)
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.Name
	}
	return out, more
}

// ------------------------------------------------------------ the reading

// A tool's listing is read a NUL-ended path at a time; a path with spaces or
// Unicode is offered as it is, one with a control character or invalid UTF-8
// is not (X126: no token can carry it), nor anything but a plain relative
// path; a leading "./" comes off; a record too long for a path is dropped
// whole without losing the one after it; a last path with no NUL counts.
func TestAtFilesReadsNULSeparatedPaths(t *testing.T) {
	long := strings.Repeat("x", atFilesRecordMax+10)
	var in bytes.Buffer
	for _, rec := range []string{
		"a.go", "sp ace.txt", "ünï/cödé.go", "", "./dot/x.go",
		"bad\x01name", "bad\x1b[31mred", "ff\xffname", "line\nbreak", "tab\there", "del\x7f",
		"/abs/x", "../up", "a/../b", "a//b", ".", "a/./b",
		long, "after/long.go", "last-without-nul",
	} {
		in.WriteString(rec)
		if rec != "last-without-nul" {
			in.WriteByte(0)
		}
	}
	var counts []int
	s := atFileSearcher{read: func(n int) { counts = append(counts, n) }}
	paths, capped, err := s.readPaths(&in)
	want := []string{"a.go", "sp ace.txt", "ünï/cödé.go", "dot/x.go", "after/long.go", "last-without-nul"}
	if err != nil || capped || !slices.Equal(paths, want) {
		t.Fatalf("read %q (capped %v, err %v), want %q", paths, capped, err, want)
	}
	// Every non-empty record counts toward the cap, offered or not — the
	// long one too.
	if len(counts) != 19 || counts[len(counts)-1] != 19 {
		t.Fatalf("counted records %v, want 1…19", counts)
	}
}

// The directories offered are the files' own: every proper prefix directory
// of each listed file, once, ending in "/". An empty directory is in no
// path, so it is never offered; the order is the base order, shorter first,
// then by bytes, and a file git lists twice (a conflict's stages) is one.
func TestAtFilesDerivesItsDirectories(t *testing.T) {
	x := newAtFileIndex([]string{"a/b/c.go", "a/d.go", "e.go", "a/b/c.go", "z/y/x/w.txt"}, "")
	want := []string{"a/", "z/", "a/b/", "e.go", "z/y/", "a/d.go", "z/y/x/", "a/b/c.go", "z/y/x/w.txt"}
	if !slices.Equal(x.names, want) {
		t.Fatalf("names %q, want %q", x.names, want)
	}
	for i, n := range x.names {
		if x.folds[i] != n {
			t.Fatalf("%q folds to %q", n, x.folds[i])
		}
	}
	i := slices.Index(x.names, "z/y/x/")
	if got := x.names[i][x.bases[i]:]; got != "x/" {
		t.Fatalf("the last segment of z/y/x/ is %q", got)
	}

	// And through a real search: an empty directory under the root is not
	// offered; its sibling holding a file is.
	root := atTree(t, "empty/", "deep/empty/", "full/f.go")
	x = atIndexOf(t, atSearch(t, atSearcherFor(newAtClock(), nil, false), context.Background(), root))
	if !slices.Equal(x.names, []string{"full/", "full/f.go"}) {
		t.Fatalf("the walk offered %q", x.names)
	}
}

// A fold is the same bytes long as its path, so the match's offsets into one
// are offsets into the other: upper case folds, and a rune whose lower case
// has another length is kept as it is.
func TestAtFilesFoldKeepsEveryOffset(t *testing.T) {
	for in, want := range map[string]string{
		"src/FooBar.go": "src/foobar.go",
		"ÜNÏ/Cödé.go":   "ünï/cödé.go",
		"İx":            "İx", // U+0130 lowers to one byte fewer
		"KELVIN_K":      "kelvin_k",
		"plain/path.go": "plain/path.go",
	} {
		if got := atFold(in); got != want || len(got) != len(in) {
			t.Errorf("atFold(%q) = %q, want %q", in, got, want)
		}
	}
}

// ------------------------------------------------------------ the matching

// atFixturePaths is a small repository, files only: its directories are
// derived.
var atFixturePaths = []string{
	"README.md",
	"go.mod",
	"cmd/craze/main.go",
	"internal/tui/app.go",
	"internal/tui/at_dirs.go",
	"internal/tui/at_files.go",
	"internal/tui/at_files_test.go",
	"internal/tui/complete.go",
	"internal/tui/complete_test.go",
	"internal/tui/composer.go",
	"internal/tui/sessions_list.go",
	"internal/tui/slash.go",
	"internal/sessions/sessions.go",
	"internal/agent/live.go",
	"docs/reference/tui.md",
	"web/src/FooBar.tsx",
	"notes/my plan.txt",
	"ünï/Cödé.go",
}

// The ranking, query by query (§3.16): a last segment starting with the
// query first; then segment starts, runs and fewer gaps; a tie to the
// shorter path, then its bytes. Case never matters.
func TestAtFilesRanksMatches(t *testing.T) {
	x := newAtFileIndex(atFixturePaths, "")
	for _, tc := range []struct {
		query string
		want  []string
	}{
		// Nothing typed: everything, the nearest the root first (the first
		// eight here).
		{"", []string{"cmd/", "web/", "docs/", "go.mod", "notes/", "ünï/", "web/src/", "README.md"}},
		// A last segment that starts with the query first, whatever else
		// matches; a tie to the shorter path, then its bytes.
		{"comp", []string{
			"internal/tui/complete.go", "internal/tui/composer.go", "internal/tui/complete_test.go",
		}},
		{"sl", []string{"internal/tui/slash.go", "internal/tui/sessions_list.go", "notes/my plan.txt"}},
		{"SL", []string{"internal/tui/slash.go", "internal/tui/sessions_list.go", "notes/my plan.txt"}},
		{"sess", []string{"internal/sessions/", "internal/sessions/sessions.go", "internal/tui/sessions_list.go"}},
		// A directory whose name starts with the query is a candidate like a
		// file; the files under it match it as a run at a segment start.
		{"tui", []string{
			"internal/tui/", "docs/reference/tui.md",
			"internal/tui/app.go", "internal/tui/slash.go", "internal/tui/at_dirs.go",
			"internal/tui/at_files.go", "internal/tui/complete.go", "internal/tui/composer.go",
			"internal/tui/at_files_test.go", "internal/tui/complete_test.go", "internal/tui/sessions_list.go",
		}},
		// One rune: a last segment starting with it, then a match in a last
		// segment (in-base), then anywhere.
		{"a", []string{
			"internal/agent/", "internal/tui/app.go", "internal/tui/at_dirs.go", "internal/tui/at_files.go",
			"internal/tui/at_files_test.go",
			"README.md", "internal/", "cmd/craze/", "cmd/craze/main.go", "notes/my plan.txt", "web/src/FooBar.tsx",
			"internal/tui/slash.go",
			"internal/tui/", "internal/sessions/", "internal/agent/live.go", "internal/tui/complete.go",
			"internal/tui/composer.go", "internal/sessions/sessions.go", "internal/tui/complete_test.go",
			"internal/tui/sessions_list.go",
		}},
		// Segment starts: after `_`, a case change, a path's first byte.
		{"atf", []string{"internal/tui/at_files.go", "internal/tui/at_files_test.go"}},
		{"fb", []string{"web/src/FooBar.tsx"}},
		{"ct", []string{
			"internal/tui/complete.go", "internal/tui/complete_test.go", "docs/reference/tui.md", "web/src/FooBar.tsx",
		}},
		{"rd", []string{"README.md", "docs/reference/tui.md", "internal/tui/at_dirs.go"}},
		// A run across a directory and a name (the tightened alignment).
		{"tuiapp", []string{"internal/tui/app.go"}},
		{"live", []string{"internal/agent/live.go"}},
		{"cöd", []string{"ünï/Cödé.go"}},
		{"CÖD", []string{"ünï/Cödé.go"}},
		{"my p", []string{"notes/my plan.txt"}},
		{"zzz", nil},
	} {
		got, more := matchNames(x, tc.query, 100)
		if tc.query == "" {
			got = got[:min(len(got), len(tc.want))]
			more = 0
		}
		if !slices.Equal(got, tc.want) || more != 0 {
			t.Errorf("%q ranks %q (more %d), want %q", tc.query, got, more, tc.want)
		}
	}
}

// The match's shortcuts change nothing (§3.16's ranking, measured fast): the
// byte mask refusing a path, the heap keeping only the best, and a match
// counted without being scored because its bound cannot beat the lowest kept
// — against every match scored and every one sorted, over a tree of the
// benchmark's shape, query by query. No match scores above its bound.
func TestAtFilesShortcutsRankAsAFullSortDoes(t *testing.T) {
	x := newAtFileIndex(append(atBenchPaths(5000, 400), atFixturePaths...), "")
	full := func(query string, k int) ([]string, int) {
		q := query
		for strings.HasPrefix(q, "./") {
			q = q[2:]
		}
		prefix, rest := "", q
		if i := strings.LastIndexByte(q, '/'); i >= 0 {
			prefix, rest = atFold(q[:i+1]), q[i+1:]
		}
		m := atCompile(atFold(rest))
		type scored struct{ score, idx int }
		var all []scored
		for i, f := range x.folds {
			from := 0
			if prefix != "" {
				if len(f) <= len(prefix) || !strings.HasPrefix(f, prefix) {
					continue
				}
				from = len(prefix)
			}
			if len(m.units) == 0 {
				all = append(all, scored{0, i})
				continue
			}
			run, ok := m.find(f, from)
			if !ok {
				continue
			}
			base := int(x.bases[i])
			prefixed := strings.HasPrefix(f[base:], m.fold)
			score := m.rate(x.names[i], f, base, from, run, prefixed)
			if score > m.bound(prefixed) {
				t.Errorf("%q on %q scores %d, above its bound %d", query, x.names[i], score, m.bound(prefixed))
			}
			all = append(all, scored{score, i})
		}
		slices.SortFunc(all, func(a, b scored) int {
			if a.score != b.score {
				return b.score - a.score
			}
			return a.idx - b.idx
		})
		var names []string
		for _, s := range all[:min(k, len(all))] {
			names = append(names, x.names[s.idx])
		}
		return names, len(all) - len(names)
	}
	for _, q := range []string{
		"", "s", "se", "sess", "SESS", "tui", "t_u", "comptest", "sesslist", "cfg", "a", "z", "zzzz",
		"internal/", "internal/tui/", "internal/se", "./tui/c", "control/remote/", "fb", "cöd", "my p", ".go", "_test",
	} {
		for _, k := range []int{1, 8, atFilesRankMax} {
			got, gotMore := matchNames(x, q, k)
			want, wantMore := full(q, k)
			if !slices.Equal(got, want) || gotMore != wantMore {
				t.Errorf("%q (k %d): ranked %q (more %d), a full sort %q (more %d)", q, k, got, gotMore, want, wantMore)
			}
		}
	}
}

// A "/" in the query names a directory (§3.16): what is up to its last "/"
// is a prefix every candidate's path starts with — case folded, a leading
// "./" taken off — and the rest is matched inside it. The directory itself
// is not a candidate of its own listing; a prefix nothing starts with,
// absolute or outside the root, offers nothing.
func TestAtFilesFiltersByPathPrefix(t *testing.T) {
	x := newAtFileIndex(atFixturePaths, "")
	tuiAll := []string{
		"internal/tui/app.go", "internal/tui/slash.go", "internal/tui/at_dirs.go", "internal/tui/at_files.go",
		"internal/tui/complete.go", "internal/tui/composer.go", "internal/tui/at_files_test.go",
		"internal/tui/complete_test.go", "internal/tui/sessions_list.go",
	}
	for _, tc := range []struct {
		query string
		want  []string
	}{
		{"internal/tui/", tuiAll},
		{"Internal/TUI/", tuiAll},
		{"./internal/tui/", tuiAll},
		{"internal/", []string{
			"internal/tui/", "internal/agent/", "internal/sessions/", "internal/tui/app.go",
			"internal/tui/slash.go", "internal/agent/live.go", "internal/tui/at_dirs.go", "internal/tui/at_files.go",
		}},
		{"internal/tui/at", []string{
			"internal/tui/at_dirs.go", "internal/tui/at_files.go", "internal/tui/at_files_test.go",
		}},
		// The rest is matched inside the prefix: its directory first (its name
		// starts with it), then every file under it the rest matches.
		{"internal/tu", append([]string{"internal/tui/"}, tuiAll...)},
		{"internal/se", []string{
			"internal/sessions/", "internal/sessions/sessions.go", "internal/tui/sessions_list.go",
			"internal/tui/composer.go", "internal/tui/at_files_test.go",
		}},
		{"docs/", []string{"docs/reference/", "docs/reference/tui.md"}},
		{"nope/", nil},
		{"/internal/", nil},
		{"../internal/", nil},
		{"~/", nil},
	} {
		got, more := matchNames(x, tc.query, 100)
		if tc.query == "internal/" {
			got = got[:min(len(got), len(tc.want))]
			more = 0
		}
		if !slices.Equal(got, tc.want) || more != 0 {
			t.Errorf("%q offers %q (more %d), want %q", tc.query, got, more, tc.want)
		}
	}
}

// A query's candidates are what the popup writes (§3.16): the relative path,
// a directory's ending in "/" and openable (tab descends); the grammar
// quotes a path with a space in it.
func TestAtFilesCandidatesWriteTheirPath(t *testing.T) {
	x := newAtFileIndex(atFixturePaths, "")
	items, _ := x.match("cöd", 100)
	if len(items) != 1 || items[0] != (completeItem{Name: "ünï/Cödé.go", Value: "ünï/Cödé.go", Insert: "ünï/Cödé.go"}) {
		t.Fatalf("a file's candidate: %+v", items)
	}
	items, _ = x.match("internal/tu", 100)
	if len(items) == 0 || items[0] != (completeItem{Name: "internal/tui/", Value: "internal/tui/", Insert: "internal/tui/", Openable: true}) {
		t.Fatalf("a directory's candidate: %+v", items)
	}

	// Through the popup: accept, descend, accept inside.
	src := atFileSource{search: func(context.Context, string) completeLoaded {
		return completeLoaded{Data: newAtFileIndex(atFixturePaths, "")}
	}}
	env := completeEnv{Workspace: "/w", Shown: 1}
	p := newCompletePopup(src, atGrammar)
	p.loaded(awaitLoaded(t, runLoad(t, synced(&p, "see @my", env))))
	c, _ := popKey(&p, keyOf(tea.KeyEnter))
	if c.value != `see @"notes/my plan.txt" ` {
		t.Fatalf("accepting a path with a space wrote %q", c.value)
	}
	// The accept's space closed that opening; a token elsewhere is a new
	// one, which searches afresh.
	synced(&p, c.value, env)
	if p.visible() {
		t.Fatal("the popup is still open after the accept's space")
	}
	p.loaded(awaitLoaded(t, runLoad(t, synced(&p, "@internal/tu", env))))
	c, _ = popKey(&p, keyOf(tea.KeyTab))
	if c.verb != completeDescended || c.value != "@internal/tui/" {
		t.Fatalf("tab on internal/tui/: %v %q", c.verb, c.value)
	}
	if cmd := p.sync(c.value, c.cursor, env); cmd != nil || !p.visible() {
		t.Fatalf("descending searched again (%v) or closed the popup (visible %v)", cmd != nil, p.visible())
	}
	if got := popupNames(p); len(got) != 9 || got[0] != "internal/tui/app.go" {
		t.Fatalf("inside internal/tui/: %q", got)
	}
	c, _ = popKey(&p, keyOf(tea.KeyEnter))
	if c.value != "@internal/tui/app.go " {
		t.Fatalf("accepting inside wrote %q", c.value)
	}
}

// The popup is handed the best atFilesRankMax matches and the count of the
// rest, which its count line adds to the rows below the window: `↓ N more`
// down to the last row it was handed, where the rest are still below.
func TestAtFilesHandsThePopupItsBestAndCountsTheRest(t *testing.T) {
	var paths []string
	for i := range 150 {
		paths = append(paths, fmt.Sprintf("f%03d.go", i))
	}
	x := newAtFileIndex(paths, "")
	got, more := matchNames(x, "f", atFilesRankMax)
	if len(got) != atFilesRankMax || more != 50 || got[0] != "f000.go" || got[99] != "f099.go" {
		t.Fatalf("150 matches: handed %d (%q … %q), more %d", len(got), got[0], got[len(got)-1], more)
	}

	src := atFileSource{search: func(context.Context, string) completeLoaded { return completeLoaded{Data: x} }}
	env := completeEnv{Workspace: "/w", Shown: 1}
	p := newCompletePopup(src, atGrammar)
	p.loaded(awaitLoaded(t, runLoad(t, synced(&p, "@f", env))))
	last := func() string {
		lines := trimmed(popupText(p, 40, 20))
		return lines[len(lines)-1]
	}
	if l := last(); l != "  ↓ 142 more" || p.height(20) != 9 {
		t.Fatalf("at the top: %q, height %d", l, p.height(20))
	}
	popKey(&p, keyOf(tea.KeyUp)) // wraps to the last of the 100
	if l := last(); l != "  ↓ 50 more" {
		t.Fatalf("at the last row handed: %q", l)
	}
	// Narrowed until everything fits: no count line.
	synced(&p, "@f149", env)
	if got := popupNames(p); !slices.Equal(got, []string{"f149.go"}) || p.height(20) != 1 {
		t.Fatalf("@f149: %q, height %d", got, p.height(20))
	}
}

// ------------------------------------------------------------ the search

// rg first (§3.16): its command line exactly, run in the workspace; what it
// lists is offered with its directories, a path it lists that no token can
// carry is not.
func TestAtFilesListsWithRipgrep(t *testing.T) {
	root := t.TempDir()
	fakes := atFakes(t, map[string]string{
		"rg":  atPrintPaths("a.go", "sub/b.go", "sp ace.txt", "ünï/x.md", "bad\x01name", "ff\xff"),
		"git": "exit 99",
	})
	clock := newAtClock()
	x := atIndexOf(t, atSearch(t, atSearcherFor(clock, fakes.tools, true), context.Background(), root))
	if want := []string{"a.go", "sub/", "ünï/", "sub/b.go", "sp ace.txt", "ünï/x.md"}; !slices.Equal(x.names, want) || x.title != "" {
		t.Fatalf("offered %q (title %q), want %q", x.names, x.title, want)
	}
	if got := fakes.record(t, "rg", "args"); got != "--no-config\n--files\n--hidden\n-g\n!.git\n-0\n" {
		t.Fatalf("rg ran with %q", got)
	}
	wantCwd, _ := filepath.EvalSymlinks(root)
	if got := strings.TrimSpace(fakes.record(t, "rg", "cwd")); got != wantCwd {
		t.Fatalf("rg ran in %q, want %q", got, wantCwd)
	}
	if fakes.ran("git") {
		t.Fatal("git ran although rg was there")
	}
	if clock.duration() != 3*time.Second || newAtFileSearcher().timeout != 3*time.Second {
		t.Fatalf("the search's timeout: armed %v, default %v; want 3s", clock.duration(), newAtFileSearcher().timeout)
	}
}

// Without rg, a git work tree is listed by git ls-files (§3.16): its command
// line exactly, run in the workspace.
func TestAtFilesFallsBackToGitLsFiles(t *testing.T) {
	root := t.TempDir()
	fakes := atFakes(t, map[string]string{"git": atPrintPaths("tracked.go", "untracked/new.go", "tracked.go")})
	x := atIndexOf(t, atSearch(t, atSearcherFor(newAtClock(), fakes.tools, true), context.Background(), root))
	if want := []string{"tracked.go", "untracked/", "untracked/new.go"}; !slices.Equal(x.names, want) {
		t.Fatalf("offered %q, want %q", x.names, want)
	}
	if got := fakes.record(t, "git", "args"); got != "ls-files\n-co\n--exclude-standard\n-z\n" {
		t.Fatalf("git ran with %q", got)
	}
	wantCwd, _ := filepath.EvalSymlinks(root)
	if got := strings.TrimSpace(fakes.record(t, "git", "cwd")); got != wantCwd {
		t.Fatalf("git ran in %q, want %q", got, wantCwd)
	}
}

// Without rg, and not in a git work tree — or in one with no git, or whose
// git fails having listed nothing — the tree is walked (§3.16): dotfiles
// kept, `.git` (a directory or a submodule's file) and `node_modules` never
// entered at any depth, symbolic links neither followed nor listed, a name
// no token can carry left out with everything under it.
func TestAtFilesFallsBackToTheWalk(t *testing.T) {
	root := atTree(t,
		"a.go", ".env", ".config/c.toml", "sub/deep/b.go", "sp ace/x.txt",
		".git/HEAD", ".git/objects/o", "sub/.git", "node_modules/m.js", "sub/node_modules/n.js",
		"bad\x01dir/f.go", "bad\x01file", "empty/",
	)
	if err := os.Symlink(filepath.Join(root, "a.go"), filepath.Join(root, "link.go")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "sub"), filepath.Join(root, "linkdir")); err != nil {
		t.Fatal(err)
	}
	want := []string{
		".env", "a.go", "sub/", "sp ace/", ".config/", "sub/deep/", "sp ace/x.txt", "sub/deep/b.go", ".config/c.toml",
	}
	failing := atFakes(t, map[string]string{"git": "echo 'fatal: detected dubious ownership in repository' >&2\nexit 128"})
	for _, tc := range []struct {
		name    string
		tools   map[string]string
		gitTree bool
	}{
		{"not a git work tree", failing.tools, false},
		{"a work tree with no git", nil, true},
		{"a work tree whose git fails", failing.tools, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var walked []int
			s := atSearcherFor(newAtClock(), tc.tools, tc.gitTree)
			s.walked = func(n int) { walked = append(walked, n) }
			x := atIndexOf(t, atSearch(t, s, context.Background(), root))
			if !slices.Equal(x.names, want) || x.title != "" {
				t.Fatalf("walked %q (title %q), want %q", x.names, x.title, want)
			}
			if len(walked) == 0 {
				t.Fatal("the tree was not walked")
			}
		})
	}
}

// A failing tool (§3.16): rg's exit 1 with nothing listed is no files; its
// exit 2 with paths leaves them standing; a failure with nothing listed is
// the red note, in the tool's own first line.
func TestAtFilesToolFailures(t *testing.T) {
	root := t.TempDir()
	search := func(t *testing.T, body string) completeLoaded {
		t.Helper()
		fakes := atFakes(t, map[string]string{"rg": body})
		return atSearch(t, atSearcherFor(newAtClock(), fakes.tools, false), context.Background(), root)
	}
	src := func(l completeLoaded) atFileSource {
		return atFileSource{search: func(context.Context, string) completeLoaded { return l }}
	}
	answer := func(t *testing.T, l completeLoaded, query string) completeAnswer {
		t.Helper()
		p := newCompletePopup(src(l), atGrammar)
		env := completeEnv{Workspace: root, Shown: 1}
		p.loaded(awaitLoaded(t, runLoad(t, synced(&p, "@"+query, env))))
		return p.ans
	}

	t.Run("no files", func(t *testing.T) {
		l := search(t, "exit 1")
		if x := atIndexOf(t, l); len(x.names) != 0 {
			t.Fatalf("offered %q", x.names)
		}
		if a := answer(t, l, ""); a.Note != "no files here" || a.NoteErr {
			t.Fatalf("the answer: %+v", a)
		}
	})
	t.Run("some unreadable", func(t *testing.T) {
		l := search(t, atPrintPaths("a.go")+"\necho 'rg: ./x: Permission denied (os error 13)' >&2\nexit 2")
		if x := atIndexOf(t, l); !slices.Equal(x.names, []string{"a.go"}) || x.title != "" {
			t.Fatalf("offered %q (title %q)", x.names, x.title)
		}
	})
	t.Run("nothing readable", func(t *testing.T) {
		l := search(t, "echo\necho 'rg: ./: Permission denied (os error 13)' >&2\necho 'rg: second line' >&2\nexit 2")
		a := answer(t, l, "")
		if a.Note != "could not list files: rg: ./: Permission denied (os error 13)" || !a.NoteErr || len(a.Items) != 0 {
			t.Fatalf("the answer: %+v", a)
		}
	})
	t.Run("silent failure", func(t *testing.T) {
		a := answer(t, search(t, "exit 3"), "")
		if a.Note != "could not list files: exit status 3" || !a.NoteErr {
			t.Fatalf("the answer: %+v", a)
		}
	})
	t.Run("a workspace that is gone", func(t *testing.T) {
		fakes := atFakes(t, map[string]string{"rg": atPrintPaths("a.go")})
		l := atSearch(t, atSearcherFor(newAtClock(), fakes.tools, false), context.Background(), filepath.Join(root, "gone"))
		if a := answer(t, l, ""); a.Note != "could not list files: no such file or directory" || !a.NoteErr {
			t.Fatalf("the answer: %+v", a)
		}
	})
}

// The cap (§3.16): a listing of more than atFilesMax paths is read to
// atFilesMax and no further — the tool, which would otherwise print on and
// then stall with its output open for good, is killed and reaped — and the
// list says it is partial.
func TestAtFilesStopsReadingAtTheCap(t *testing.T) {
	paths := make([]string, atFilesMax+50)
	for i := range paths {
		paths[i] = fmt.Sprintf("f%05d", i)
	}
	fakes := atFakes(t, map[string]string{"rg": atPrintPaths(paths...) + "\n" + atStall})
	s := atSearcherFor(newAtClock(), fakes.tools, false)
	pids := make(chan int, 1)
	s.started = func(pid int) { pids <- pid }
	maxRead := 0
	s.read = func(n int) { maxRead = max(maxRead, n) }
	x := atIndexOf(t, atSearch(t, s, context.Background(), t.TempDir()))
	if len(x.names) != atFilesMax || x.names[atFilesMax-1] != "f49999" || maxRead != atFilesMax {
		t.Fatalf("offered %d paths (last %q), read %d records; want %d", len(x.names), x.names[len(x.names)-1], maxRead, atFilesMax)
	}
	if x.title != "only the first 50,000 files" {
		t.Fatalf("title %q", x.title)
	}
	atReaped(t, atAwait(t, pids, "the tool's pid"))
}

// The walk's cap (§3.16): at most atFilesWalkMax entries are read, in
// batches of atFilesWalkBatch; the list says it is partial.
func TestAtFilesWalkStopsAtItsCap(t *testing.T) {
	root := t.TempDir()
	for i := range atFilesWalkMax + 50 {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("f%05d", i)), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	s := atSearcherFor(newAtClock(), nil, false)
	var walked []int
	s.walked = func(n int) { walked = append(walked, n) }
	x := atIndexOf(t, atSearch(t, s, context.Background(), root))
	if len(x.names) != atFilesWalkMax || len(walked) == 0 || walked[len(walked)-1] != atFilesWalkMax {
		t.Fatalf("offered %d, batches read to %v", len(x.names), walked)
	}
	for i, n := range walked[:len(walked)-1] {
		if n != (i+1)*atFilesWalkBatch {
			t.Fatalf("batch %d ended at %d: not %d at a time", i, n, atFilesWalkBatch)
		}
	}
	if x.title != "only the first 20,000 entries" {
		t.Fatalf("title %q", x.title)
	}
}

// The timeout (§3.16), fired by the test once the tool has printed and
// stalled: the tool is killed and reaped, and what it printed is offered as
// a partial list; a tool that printed nothing by then is the red note. The
// walk stops at it between batches the same way.
func TestAtFilesTimeoutStopsTheSearch(t *testing.T) {
	t.Run("rg printed some", func(t *testing.T) {
		fakes := atFakes(t, map[string]string{"rg": atPrintPaths("a.go", "b/c.go") + "\n" + atStall})
		clock := newAtClock()
		s := atSearcherFor(clock, fakes.tools, false)
		pids, reads := make(chan int, 1), make(chan int, 4)
		s.started = func(pid int) { pids <- pid }
		s.read = func(n int) { reads <- n }
		ch := make(chan completeLoaded, 1)
		go func() { ch <- s.search(context.Background(), t.TempDir()) }()
		pid := atAwait(t, pids, "the tool's start")
		atAwait(t, reads, "the first path")
		atAwait(t, reads, "the second path")
		if !clock.fire() {
			t.Fatal("the search never armed its timer")
		}
		x := atIndexOf(t, atAwait(t, ch, "the search's end after the timeout"))
		if !slices.Equal(x.names, []string{"b/", "a.go", "b/c.go"}) || x.title != "listing stopped after 3s" {
			t.Fatalf("offered %q (title %q)", x.names, x.title)
		}
		atReaped(t, pid)
	})
	t.Run("rg printed nothing", func(t *testing.T) {
		fakes := atFakes(t, map[string]string{"rg": atStall})
		clock := newAtClock()
		s := atSearcherFor(clock, fakes.tools, false)
		pids := make(chan int, 1)
		s.started = func(pid int) { pids <- pid }
		ch := make(chan completeLoaded, 1)
		root := t.TempDir()
		go func() { ch <- s.search(context.Background(), root) }()
		pid := atAwait(t, pids, "the tool's start")
		if !clock.fire() {
			t.Fatal("the search never armed its timer")
		}
		l := atAwait(t, ch, "the search's end after the timeout")
		if !errors.Is(l.Err, errAtFilesTimeout) {
			t.Fatalf("answered %v, %T", l.Err, l.Data)
		}
		atReaped(t, pid)
		a := atFileSource{search: func(context.Context, string) completeLoaded { return l }}.complete(
			completeQuery{Workspace: root, loads: map[string]completeLoaded{root: l}})
		if a.Note != "listing files took over 3s" || !a.NoteErr {
			t.Fatalf("the answer: %+v", a)
		}
	})
	t.Run("the walk", func(t *testing.T) {
		root := t.TempDir()
		for i := range 3 * atFilesWalkBatch {
			if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("f%04d", i)), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		clock := newAtClock()
		s := atSearcherFor(clock, nil, false)
		var walked []int
		armed := false
		s.walked = func(n int) {
			walked = append(walked, n)
			if len(walked) == 1 {
				armed = clock.fire()
			}
		}
		x := atIndexOf(t, atSearch(t, s, context.Background(), root))
		if !armed || !slices.Equal(walked, []int{atFilesWalkBatch}) || len(x.names) != atFilesWalkBatch || x.title != "listing stopped after 3s" {
			t.Fatalf("armed %v, batches %v, offered %d (title %q)", armed, walked, len(x.names), x.title)
		}
	})
}

// A search the popup no longer awaits is cancelled (X122, X125): a running
// tool is killed and reaped and the search returns at once, its answer
// dropped by the popup; a walk stops between two batches.
func TestAtFilesCancelKillsTheSearch(t *testing.T) {
	t.Run("the context", func(t *testing.T) {
		fakes := atFakes(t, map[string]string{"rg": atPrintPaths("a.go") + "\n" + atStall})
		s := atSearcherFor(newAtClock(), fakes.tools, false)
		pids, reads := make(chan int, 1), make(chan int, 1)
		s.started = func(pid int) { pids <- pid }
		s.read = func(n int) { reads <- n }
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ch := make(chan completeLoaded, 1)
		go func() { ch <- s.search(ctx, t.TempDir()) }()
		pid := atAwait(t, pids, "the tool's start")
		atAwait(t, reads, "the first path")
		cancel()
		if l := atAwait(t, ch, "the search's end after its cancel"); !errors.Is(l.Err, context.Canceled) || l.Data != nil {
			t.Fatalf("a cancelled search answered %v, %v", l.Err, l.Data)
		}
		atReaped(t, pid)
	})
	t.Run("the popup closing", func(t *testing.T) {
		fakes := atFakes(t, map[string]string{"rg": atStall})
		s := atSearcherFor(newAtClock(), fakes.tools, false)
		pids := make(chan int, 1)
		s.started = func(pid int) { pids <- pid }
		p := newCompletePopup(atFileSource{search: s.search}, atGrammar)
		ch := runLoad(t, synced(&p, "@a", completeEnv{Workspace: t.TempDir(), Shown: 1}))
		pid := atAwait(t, pids, "the tool's start")
		p.close()
		msg := awaitLoaded(t, ch)
		if _, taken := p.loaded(msg); taken {
			t.Fatal("the closed popup took the cancelled search's answer")
		}
		atReaped(t, pid)
	})
	t.Run("the walk", func(t *testing.T) {
		root := t.TempDir()
		for i := range 3 * atFilesWalkBatch {
			if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("f%04d", i)), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		s := atSearcherFor(newAtClock(), nil, false)
		var walked []int
		s.walked = func(n int) {
			walked = append(walked, n)
			if len(walked) == 1 {
				cancel()
			}
		}
		if l := atSearch(t, s, ctx, root); !errors.Is(l.Err, context.Canceled) || !slices.Equal(walked, []int{atFilesWalkBatch}) {
			t.Fatalf("answered %v after batches %v", l.Err, walked)
		}
	})
}

// One search per popup opening (§3.16, X122): the listing the first `@`
// started serves every keystroke after it, and only a new opening searches
// again.
func TestAtFilesSearchesOncePerOpening(t *testing.T) {
	fakes := atFakes(t, map[string]string{"rg": atPrintPaths("alpha.go", "beta/ab.go", "gamma.md")})
	s := atSearcherFor(newAtClock(), fakes.tools, false)
	env := completeEnv{Workspace: t.TempDir(), Shown: 1}
	p := newCompletePopup(atFileSource{search: s.search}, atGrammar)
	cmd := synced(&p, "@", env)
	if !p.pending() {
		t.Fatal("the popup is not searching")
	}
	if _, ok := p.loaded(awaitLoaded(t, runLoad(t, cmd))); !ok {
		t.Fatal("the search's answer was dropped")
	}
	for _, q := range []string{"@a", "@ab", "@a", "@beta/", "@"} {
		if cmd := synced(&p, q, env); cmd != nil {
			t.Fatalf("%s searched again", q)
		}
	}
	synced(&p, "@ab", env)
	if got := popupNames(p); !slices.Equal(got, []string{"beta/ab.go"}) {
		t.Fatalf("@ab offers %q", got)
	}
	synced(&p, "@a", env)
	if got := popupNames(p); !slices.Equal(got, []string{"alpha.go", "beta/ab.go", "beta/", "gamma.md"}) {
		t.Fatalf("@a offers %q", got)
	}
	if runs := fakes.record(t, "rg", "runs"); runs != "run\n" {
		t.Fatalf("rg ran %d times", strings.Count(runs, "\n"))
	}
	// A new opening searches again.
	p.close()
	if cmd := synced(&p, "@", env); cmd == nil {
		t.Fatal("a new opening did not search")
	}
	p.close()
}

// The real tools, where the machine has them: rg honours .gitignore in a
// work tree, shows dotfiles, never lists .git's; git lists tracked and
// untracked files but not ignored ones. (The fakes above prove the command
// lines; these prove the command lines mean what the fakes assume.)
func TestAtFilesWithTheRealTools(t *testing.T) {
	names := []string{
		"a.go", ".hidden/h.txt", "ignored.log", "sub/deep/b.go", "sp ace.txt", "ünï.go", "empty/",
	}
	want := []string{"a.go", "sub/", ".hidden/", "ünï.go", "sub/deep/", ".gitignore", "sp ace.txt", ".hidden/h.txt", "sub/deep/b.go"}
	t.Run("rg", func(t *testing.T) {
		requireFrameRG(t)
		root := atTree(t, append(names, ".git/HEAD", ".git/objects/o")...)
		if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("*.log\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		s := newAtFileSearcher()
		asked := false
		s.gitTree = func(string) bool { asked = true; return false }
		x := atIndexOf(t, atSearch(t, s, context.Background(), root))
		if !slices.Equal(x.names, want) || asked {
			t.Fatalf("rg offered %q (a work tree looked for: %v), want %q", x.names, asked, want)
		}
	})
	t.Run("git", func(t *testing.T) {
		gitBin, err := exec.LookPath("git")
		if err != nil {
			t.Skipf("git is not on PATH: %v", err)
		}
		root := atTree(t, names...)
		if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("*.log\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		for _, args := range [][]string{{"init", "-q"}, {"add", "a.go", ".gitignore"}} {
			cmd := exec.Command(gitBin, args...)
			cmd.Dir = root
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git %v: %v\n%s", args, err, out)
			}
		}
		s := newAtFileSearcher()
		s.look = func(name string) (string, error) {
			if name == "rg" {
				return "", exec.ErrNotFound
			}
			return exec.LookPath(name)
		}
		x := atIndexOf(t, atSearch(t, s, context.Background(), root))
		if !slices.Equal(x.names, want) {
			t.Fatalf("git offered %q, want %q", x.names, want)
		}
	})
}

// ------------------------------------------------------------ the measure

// atBenchPaths is n paths shaped like a large repository's, the same every
// run: files spread over dirs directories (a few levels deep, named from a
// vocabulary), their names words from it joined by `_`, a handful of
// extensions. A real tree has far fewer directories than files — the Linux
// kernel ~5,000 for ~80,000 — and every directory is a candidate too.
func atBenchPaths(n, dirs int) []string {
	words := []string{
		"internal", "tui", "session", "sessions", "complete", "agent", "harness", "tool", "opencode",
		"control", "remote", "protocol", "engine", "native", "model", "cache", "roster", "list", "input",
		"dispatch", "switch", "band", "status", "layout", "frame", "golden", "fixture", "journal", "index",
		"transcript", "render", "markdown", "theme", "config", "launch", "spawn", "serve", "host", "client",
	}
	exts := []string{".go", "_test.go", ".md", ".json", ".py", ".ts", ".tsx", ".yaml"}
	r := rand.New(rand.NewPCG(30, 17))
	seen := map[string]bool{}
	var ds []string
	for len(ds) < dirs {
		var b strings.Builder
		for range 1 + r.IntN(5) {
			b.WriteString(words[r.IntN(len(words))])
			b.WriteString("/")
		}
		if d := b.String(); !seen[d] {
			seen[d] = true
			ds = append(ds, d)
		}
	}
	out := make([]string, 0, n)
	for len(out) < n {
		var b strings.Builder
		b.WriteString(ds[r.IntN(len(ds))])
		for i := range 1 + r.IntN(3) {
			if i > 0 {
				b.WriteString("_")
			}
			b.WriteString(words[r.IntN(len(words))])
		}
		b.WriteString(exts[r.IntN(len(exts))])
		if p := b.String(); !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

// BenchmarkAtFilesMatch is one keystroke's match over atFilesMax paths —
// run in the Update, so it must stay well under a frame (plan 030 C17
// records the numbers).
//
// Two trees of atFilesMax files: a repository's shape (4,000 directories)
// and a pathological one with a directory for nearly every file, where the
// candidates double.
func BenchmarkAtFilesMatch(b *testing.B) {
	for _, tree := range []struct {
		name string
		dirs int
	}{{"repo", 4000}, {"flat-dirs", 45000}} {
		x := newAtFileIndex(atBenchPaths(atFilesMax, tree.dirs), "")
		b.Run(tree.name, func(b *testing.B) {
			b.Logf("%d paths, %d candidates with their directories", atFilesMax, len(x.names))
			benchQueries(b, x)
		})
	}
}

func benchQueries(b *testing.B, x *atFileIndex) {
	for _, q := range []string{"", "s", "se", "tui", "sess", "sesslist", "comptest", "internal/", "internal/tui/sess", "zzzz"} {
		b.Run(fmt.Sprintf("%q", q), func(b *testing.B) {
			for b.Loop() {
				x.match(q, atFilesRankMax)
			}
		})
	}
}

// BenchmarkAtFilesIndex is building the index of atFilesMax paths: off the
// Update, once per popup opening.
func BenchmarkAtFilesIndex(b *testing.B) {
	paths := atBenchPaths(atFilesMax, 4000)
	for b.Loop() {
		newAtFileIndex(paths, "")
	}
}
