package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/charliek/craze/internal/harness"
	"github.com/charliek/craze/internal/harness/redact"
	"github.com/charliek/craze/internal/journal"
)

// The session-start snapshot's collector (plan 029 §3.2 L2): a fake git and a
// fixed clock for everything but the one case that runs the real git.

// startClock is the fixed clock the collector's own cases read the date from:
// the native fixture's, so a case through Start and one without agree.
func startClock() time.Time { return time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC) }

const startDate = "2026-09-18 (Friday)"

// gitReply is one scripted answer of the fake git.
type gitReply func(ctx context.Context) ([]byte, error)

// gitOK answers with out; gitExit fails as a command that exited with code,
// having said msg on standard error.
func gitOK(out string) gitReply {
	return func(context.Context) ([]byte, error) { return []byte(out), nil }
}

func gitExit(code int, msg string) gitReply {
	return func(context.Context) ([]byte, error) { return nil, gitExitError{code: code, msg: msg} }
}

// gitHangs answers only when the budget is spent, as a git stuck on a slow
// disk would.
func gitHangs(ctx context.Context) ([]byte, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

type gitExitError struct {
	code int
	msg  string
}

func (e gitExitError) Error() string { return fmt.Sprintf("exit status %d: %s", e.code, e.msg) }
func (e gitExitError) ExitCode() int { return e.code }

// gitPrefix is what every snapshot command starts with: no optional locks, no
// fsmonitor, and the workspace by -C.
func gitPrefix(ws string) []string {
	return []string{"git", "--no-optional-locks", "-c", "core.fsmonitor=false", "-C", ws}
}

// fakeGit answers each command by its subcommand — the word after gitPrefix —
// and records every argv it was given.
type fakeGit struct {
	mu    sync.Mutex
	argv  [][]string
	reply map[string]gitReply
}

// repoGit is a fake git for a work tree on feature/x, with origin/HEAD at
// main, one modified file and two commits; over replaces any answer.
func repoGit(over map[string]gitReply) *fakeGit {
	g := &fakeGit{reply: map[string]gitReply{
		"rev-parse":    gitOK("true\n"),
		"branch":       gitOK("feature/x\n"),
		"symbolic-ref": gitOK("origin/main\n"),
		"status":       gitOK("## feature/x...origin/feature/x [ahead 1]\n M a.go\n"),
		"log":          gitOK("1234567 the last commit\n89abcde the one before\n"),
	}}
	maps.Copy(g.reply, over)
	return g
}

func (g *fakeGit) run(ctx context.Context, argv []string) ([]byte, error) {
	g.mu.Lock()
	g.argv = append(g.argv, slices.Clone(argv))
	sub := argv[len(gitPrefix(""))]
	reply := g.reply[sub]
	g.mu.Unlock()
	if reply == nil {
		return nil, gitExitError{code: 129, msg: "unscripted " + sub}
	}
	return reply(ctx)
}

func (g *fakeGit) calls() [][]string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.argv)
}

// warnings is a warn lane that keeps what it is told.
type warnings struct {
	mu    sync.Mutex
	lines []string
}

func (w *warnings) warn(msg string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.lines = append(w.lines, msg)
}

func (w *warnings) all() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.lines)
}

// fakeRepo is a directory that looks like a repository from outside — a .git
// with a HEAD in it, which is what inGitTree asks — for the fake git to answer
// for. Real git would not open it.
func fakeRepo(t *testing.T) string {
	t.Helper()
	ws := t.TempDir()
	writeTree(t, filepath.Join(ws, ".git"), map[string]string{"HEAD": "ref: refs/heads/main\n"})
	return ws
}

// collectStart is collectSessionStart on ws with the fixed clock.
func collectStart(ws string, run gitRunner, budget time.Duration) (harness.SessionStart, []string) {
	var w warnings
	return collectSessionStart(ws, startClock, run, budget, w.warn), w.all()
}

// TestSessionStartCollectsFromGit: the date from the clock, and each part as
// git reported it — the default branch without its remote's name — with
// every command run lock-free, with the repository's fsmonitor off, against
// the workspace by -C.
func TestSessionStartCollectsFromGit(t *testing.T) {
	ws := fakeRepo(t)
	g := repoGit(nil)
	snap, warned := collectStart(ws, g.run, sessionStartBudget)
	want := harness.SessionStart{
		Date:          startDate,
		Branch:        "feature/x",
		DefaultBranch: "main",
		Status:        "## feature/x...origin/feature/x [ahead 1]\n M a.go\n",
		Log:           "1234567 the last commit\n89abcde the one before\n",
	}
	if snap != want || len(warned) != 0 {
		t.Fatalf("snapshot %+v, warnings %q; want %+v and none", snap, warned, want)
	}
	var got []string
	prefix := gitPrefix(ws)
	for _, argv := range g.calls() {
		if !slices.Equal(argv[:len(prefix)], prefix) {
			t.Fatalf("%q is not run as %q", argv, prefix)
		}
		got = append(got, strings.Join(argv[len(prefix):], " "))
	}
	slices.Sort(got)
	wantArgs := []string{
		"branch --show-current",
		"log --oneline --no-color -n 5",
		"rev-parse --is-inside-work-tree",
		"status --porcelain=v1 --branch",
		"symbolic-ref -q --short refs/remotes/origin/HEAD",
	}
	if !slices.Equal(got, wantArgs) {
		t.Fatalf("git ran\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(wantArgs, "\n"))
	}
}

// TestSessionStartRunsEveryCommandAtOnce: no command answers until all five
// have been started, so a collector that ran them one after another would
// spend the whole budget on the first and have nothing to show.
func TestSessionStartRunsEveryCommandAtOnce(t *testing.T) {
	var arrived sync.WaitGroup
	arrived.Add(int(gitParts))
	all := make(chan struct{})
	go func() { arrived.Wait(); close(all) }()
	g := repoGit(nil)
	run := func(ctx context.Context, argv []string) ([]byte, error) {
		arrived.Done()
		select {
		case <-all:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return g.run(ctx, argv)
	}
	snap, warned := collectStart(fakeRepo(t), run, sessionStartBudget)
	if snap.Branch == "" || snap.DefaultBranch == "" || snap.Status == "" || snap.Log == "" || len(warned) != 0 {
		t.Fatalf("snapshot %+v, warnings %q: the commands did not run at once", snap, warned)
	}
}

// TestSessionStartDropsWhatDoesNotAnswer: a part that fails, or that has not
// answered when the budget is spent, is left out and said on the warn lane;
// the rest stand. A work-tree check that does not answer leaves only the
// date, and is said once, for the whole.
func TestSessionStartDropsWhatDoesNotAnswer(t *testing.T) {
	const budget = 50 * time.Millisecond
	t.Run("a part that times out", func(t *testing.T) {
		start := time.Now()
		snap, warned := collectStart(fakeRepo(t), repoGit(map[string]gitReply{"status": gitHangs}).run, budget)
		if took := time.Since(start); took > 5*time.Second {
			t.Fatalf("the collection took %v with a %v budget", took, budget)
		}
		if snap.Status != "" || snap.Branch != "feature/x" || snap.Log == "" {
			t.Fatalf("snapshot %+v; want every part but the status", snap)
		}
		want := "session start: git status did not finish within 50ms; the system prompt goes without it"
		if !slices.Equal(warned, []string{want}) {
			t.Fatalf("warnings %q; want %q", warned, want)
		}
	})
	t.Run("a part that fails", func(t *testing.T) {
		snap, warned := collectStart(fakeRepo(t), repoGit(map[string]gitReply{"log": gitExit(128, "fatal: bad object HEAD")}).run, budget)
		if snap.Log != "" || snap.Status == "" {
			t.Fatalf("snapshot %+v; want every part but the log", snap)
		}
		want := "session start: git log failed (exit status 128: fatal: bad object HEAD); the system prompt goes without it"
		if !slices.Equal(warned, []string{want}) {
			t.Fatalf("warnings %q; want %q", warned, want)
		}
	})
	t.Run("a work-tree check that times out", func(t *testing.T) {
		snap, warned := collectStart(fakeRepo(t), repoGit(map[string]gitReply{"rev-parse": gitHangs}).run, budget)
		if snap != (harness.SessionStart{Date: startDate}) || len(warned) != 1 || !strings.Contains(warned[0], "git rev-parse did not finish") {
			t.Fatalf("snapshot %+v, warnings %q; want the date alone and one warning", snap, warned)
		}
	})
	t.Run("an origin/HEAD that is broken", func(t *testing.T) {
		snap, warned := collectStart(fakeRepo(t), repoGit(map[string]gitReply{"symbolic-ref": gitExit(128, "fatal: bad ref")}).run, budget)
		if snap.DefaultBranch != "" || len(warned) != 1 || !strings.Contains(warned[0], "symbolic-ref origin/HEAD failed") {
			t.Fatalf("snapshot %+v, warnings %q; want no default branch and one warning", snap, warned)
		}
	})
}

// TestSessionStartStartsNothingOutsideARepository: before any process, the
// collector looks for a .git at or above the workspace. With none — or only a
// .git directory with no HEAD, which git skips as well — the snapshot is the
// date alone and git never runs, which is also what keeps every native test
// whose workspace is a bare temporary directory free of processes. A
// subdirectory of a repository, and a work tree whose .git is a file, do run
// it, against the workspace itself.
func TestSessionStartStartsNothingOutsideARepository(t *testing.T) {
	headless := t.TempDir()
	writeTree(t, filepath.Join(headless, ".git"), map[string]string{"config": "[core]\n"})
	for name, ws := range map[string]string{"no .git": t.TempDir(), "a .git with no HEAD": headless} {
		g := repoGit(nil)
		snap, warned := collectStart(ws, g.run, sessionStartBudget)
		if calls := g.calls(); len(calls) != 0 || snap != (harness.SessionStart{Date: startDate}) || len(warned) != 0 {
			t.Errorf("%s: git ran %d times; snapshot %+v, warnings %q; want no process, the date alone, nothing said",
				name, len(calls), snap, warned)
		}
	}

	sub := filepath.Join(fakeRepo(t), "a", "b")
	worktree := t.TempDir()
	writeTree(t, worktree, map[string]string{".git": "gitdir: /elsewhere/.git/worktrees/w\n", "a/.keep": ""})
	for name, ws := range map[string]string{"a repository's subdirectory": writeTree(t, sub, nil), "a linked work tree": filepath.Join(worktree, "a")} {
		g := repoGit(nil)
		if snap, _ := collectStart(ws, g.run, sessionStartBudget); snap.Branch != "feature/x" || len(g.calls()) != int(gitParts) {
			t.Errorf("%s: git ran %d times; snapshot %+v; want a whole snapshot", name, len(g.calls()), snap)
		}
		for _, argv := range g.calls() {
			if !slices.Equal(argv[:len(gitPrefix(ws))], gitPrefix(ws)) {
				t.Fatalf("%s: %q is not run against the workspace", name, argv)
			}
		}
	}
}

// TestSessionStartSaysWhyARepositoryIsRefused: a .git was found, so a
// work-tree check that fails is git refusing a repository that is there — one
// whose ownership it will not trust, whose config it cannot parse — and the
// warn lane says so; the snapshot is the date alone.
func TestSessionStartSaysWhyARepositoryIsRefused(t *testing.T) {
	snap, warned := collectStart(fakeRepo(t), repoGit(map[string]gitReply{
		"rev-parse": gitExit(128, "fatal: detected dubious ownership in repository at '/w'"),
	}).run, sessionStartBudget)
	want := "session start: git rev-parse failed (exit status 128: fatal: detected dubious ownership in repository at '/w'); the system prompt goes without it"
	if snap != (harness.SessionStart{Date: startDate}) || !slices.Equal(warned, []string{want}) {
		t.Fatalf("snapshot %+v, warnings %q; want the date alone and %q", snap, warned, want)
	}
}

// TestSessionStartSaysNothingOfAnAbsence: what is not there is an answer, not
// a failure. With no git installed, or the workspace inside a .git directory,
// the snapshot is the date alone; a repository with no origin/HEAD has no
// default branch; a branch with no commits yet has no log. None of them warns.
func TestSessionStartSaysNothingOfAnAbsence(t *testing.T) {
	for name, run := range map[string]gitRunner{
		"no git": func(context.Context, []string) ([]byte, error) {
			return nil, &exec.Error{Name: "git", Err: exec.ErrNotFound}
		},
		"inside .git": repoGit(map[string]gitReply{"rev-parse": gitOK("false\n")}).run,
	} {
		if snap, warned := collectStart(fakeRepo(t), run, sessionStartBudget); snap != (harness.SessionStart{Date: startDate}) || len(warned) != 0 {
			t.Errorf("%s: snapshot %+v, warnings %q; want the date alone and nothing said", name, snap, warned)
		}
	}

	snap, warned := collectStart(fakeRepo(t), repoGit(map[string]gitReply{
		"symbolic-ref": gitExit(1, ""),
		"status":       gitOK("## No commits yet on main\n?? a.go\n"),
		"log":          gitExit(128, "fatal: your current branch 'main' does not have any commits yet"),
	}).run, sessionStartBudget)
	if snap.DefaultBranch != "" || snap.Log != "" || snap.Status != "## No commits yet on main\n?? a.go\n" || len(warned) != 0 {
		t.Fatalf("snapshot %+v, warnings %q; want a new repository's status, and nothing said", snap, warned)
	}
}

// TestSessionStartBounds: every part is bounded before it leaves the adapter —
// a branch name to 200 bytes; the status to 100 lines and 4 KiB of whole
// lines, the marker's line counted in both; the log to five lines of 160
// bytes — and no cut splits a rune.
func TestSessionStartBounds(t *testing.T) {
	lines := func(n int, line func(i int) string) string {
		var b strings.Builder
		for i := range n {
			b.WriteString(line(i) + "\n")
		}
		return b.String()
	}
	long := strings.Repeat("ü", 400) // 800 bytes, two to a rune
	for name, tc := range map[string]struct {
		status    string
		wantLines int // of the status, the marker's included
	}{
		"many lines": {status: lines(300, func(i int) string { return fmt.Sprintf(" M %03d.go", i) }), wantLines: maxStartStatusLines},
		// 103 bytes a line with its newline: 39 whole lines and the marker's
		// 19 bytes fit in 4 KiB.
		"many bytes":      {status: lines(60, func(i int) string { return fmt.Sprintf(" M %03d-%s", i, strings.Repeat("x", 95)) }), wantLines: 40},
		"one long line":   {status: "## " + strings.Repeat("ü", 5000) + "\n M a.go\n", wantLines: 2},
		"within its caps": {status: lines(3, func(i int) string { return fmt.Sprintf(" M %d.go", i) }), wantLines: 3},
	} {
		t.Run(name, func(t *testing.T) {
			snap, warned := collectStart(fakeRepo(t), repoGit(map[string]gitReply{
				"branch":       gitOK(long + "\n"),
				"symbolic-ref": gitOK("origin/" + long + "\n"),
				"status":       gitOK(tc.status),
				"log":          gitOK(lines(8, func(i int) string { return fmt.Sprintf("%07x %s", i, long) })),
			}).run, sessionStartBudget)
			if len(warned) != 0 {
				t.Fatalf("warnings %q", warned)
			}
			for field, s := range map[string]string{"branch": snap.Branch, "default branch": snap.DefaultBranch} {
				if len(s) > maxStartBranch || !utf8.ValidString(s) || !strings.HasSuffix(s, ellipsis) {
					t.Errorf("the %s is %d bytes (valid: %v); want at most %d, cut with an ellipsis", field, len(s), utf8.ValidString(s), maxStartBranch)
				}
			}
			got := strings.Split(strings.TrimSuffix(snap.Status, "\n"), "\n")
			cut := snap.Status != tc.status
			if cut && got[len(got)-1] != startTruncated {
				t.Fatalf("the cut status ends %q; want the marker", got[len(got)-1])
			}
			if len(got) != tc.wantLines || len(got) > maxStartStatusLines || len(snap.Status) > maxStartStatus || !utf8.ValidString(snap.Status) {
				t.Fatalf("the status is %d lines, %d bytes; want %d lines, within %d lines and %d bytes, its marker included",
					len(got), len(snap.Status), tc.wantLines, maxStartStatusLines, maxStartStatus)
			}
			if !cut && snap.Status != tc.status {
				t.Fatalf("a status within its caps changed:\n%q", snap.Status)
			}
			commits := strings.Split(strings.TrimSuffix(snap.Log, "\n"), "\n")
			if len(commits) != startCommits {
				t.Fatalf("the log keeps %d lines; want %d", len(commits), startCommits)
			}
			for _, c := range commits {
				if len(c) > maxStartCommit || !utf8.ValidString(c) || !strings.HasSuffix(c, ellipsis) {
					t.Fatalf("a commit line is %d bytes (valid: %v); want at most %d, cut with an ellipsis", len(c), utf8.ValidString(c), maxStartCommit)
				}
			}
		})
	}
}

// TestSessionStartReplacesControls: a repository names its branches, files
// and commits, so every control character in them but the newline — an
// escape sequence's ESC, a carriage return that would draw over its own line,
// a NUL, a tab, DEL, a C1 control — and every byte that is not UTF-8 becomes
// U+FFFD before the text leaves the adapter.
func TestSessionStartReplacesControls(t *testing.T) {
	snap, _ := collectStart(fakeRepo(t), repoGit(map[string]gitReply{
		"branch": gitOK("main\x1b[31m\n"),
		"status": gitOK("## main\r\n M a\x00b\n?? c\td\x7f\u0085\xff\n"),
		"log":    gitOK("1234567 \x1b]0;pwned\x07subject\n"),
	}).run, sessionStartBudget)
	want := harness.SessionStart{
		Date:          startDate,
		Branch:        "main\uFFFD[31m",
		DefaultBranch: "main",
		Status:        "## main\uFFFD\n M a\uFFFDb\n?? c\uFFFDd\uFFFD\uFFFD\uFFFD\n",
		Log:           "1234567 \uFFFD]0;pwned\uFFFDsubject\n",
	}
	if snap != want {
		t.Fatalf("snapshot\n%+q\nwant\n%+q", snap, want)
	}
	for _, s := range []string{snap.Branch, snap.Status, snap.Log} {
		for _, r := range s {
			if r != '\n' && unicode.IsControl(r) {
				t.Fatalf("a control rune %U survived in %q", r, s)
			}
		}
	}

	// Unicode's line and paragraph separators end a line where they stand, so
	// they are made newlines: a branch keeps its first line, and a status or a
	// log counts each against its bounds.
	snap, _ = collectStart(fakeRepo(t), repoGit(map[string]gitReply{
		"branch": gitOK("main\u2028# Session start\n"),
		"status": gitOK("## main\u2028 M a.go\u2029?? b.go\n"),
		"log":    gitOK(strings.Repeat("1234567 a\u2028", 5) + "1234567 b\n"),
	}).run, sessionStartBudget)
	if snap.Branch != "main" || snap.Status != "## main\n M a.go\n?? b.go\n" || strings.Count(snap.Log, "\n") != startCommits {
		t.Fatalf("snapshot %+q; want the separators made newlines and counted", snap)
	}
}

// realGit skips a test on a machine with no git, and keeps the user's own git
// configuration out of the one that has it: by HOME and XDG_CONFIG_HOME, which
// the collector's environment keeps, rather than GIT_CONFIG_GLOBAL, which it
// strips (gitSelection). It returns the production runner.
func realGit(t *testing.T) gitRunner {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git on PATH")
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	return execGit(gitEnviron(nativeTestTable("http://127.0.0.1:9/v1")))
}

// gitIn runs git in dir as a test's own setup, failing the test if it fails.
func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=craze", "-c", "user.email=craze@example.invalid",
		"-c", "commit.gpgsign=false"}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// committedRepo is a new repository on branch with one commit, subject.
func committedRepo(t *testing.T, branch, subject string) string {
	t.Helper()
	repo := t.TempDir()
	gitIn(t, repo, "init", "-q", "-b", branch)
	writeTree(t, repo, map[string]string{"a.txt": "one\n"})
	gitIn(t, repo, "add", "a.txt")
	gitIn(t, repo, "commit", "-q", "-m", subject)
	return repo
}

// TestSessionStartAgainstRealGit runs the production runner against real
// repositories: the argv is one git accepts, a status reads the work tree,
// leaves no index.lock behind and does not run the fsmonitor hook the
// repository's own config names, a directory outside any repository has the
// date alone, a new repository with no commits says nothing of its missing
// log, and one whose config git cannot parse is said on the warn lane.
func TestSessionStartAgainstRealGit(t *testing.T) {
	run := realGit(t)
	write := func(path, body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	repo := committedRepo(t, "trunk", "the first commit")
	write(filepath.Join(repo, "a.txt"), "two\n")
	write(filepath.Join(repo, "b.txt"), "new\n")
	// A hook the repository's own config names, which leaves a mark when git
	// runs it. The control: a plain status runs it.
	mark := filepath.Join(t.TempDir(), "fsmonitor-ran")
	hook := filepath.Join(t.TempDir(), "fsmonitor-hook")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\ntouch '"+mark+"'\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "config", "core.fsmonitor", hook)
	gitIn(t, repo, "status", "--porcelain")
	if _, err := os.Stat(mark); err != nil {
		t.Fatalf("control: a plain status did not run the repository's fsmonitor hook (%v), so its absence proves nothing", err)
	}
	if err := os.Remove(mark); err != nil {
		t.Fatal(err)
	}

	snap, warned := collectStart(repo, run, 20*time.Second)
	if snap.Branch != "trunk" || snap.DefaultBranch != "" || snap.Status != "## trunk\n M a.txt\n?? b.txt\n" ||
		!regexp.MustCompile(`^[0-9a-f]{7,} the first commit\n$`).MatchString(snap.Log) || len(warned) != 0 {
		t.Fatalf("snapshot %+v, warnings %q", snap, warned)
	}
	if _, err := os.Stat(filepath.Join(repo, ".git", "index.lock")); !os.IsNotExist(err) {
		t.Fatalf("index.lock: %v; want none", err)
	}
	if _, err := os.Stat(mark); !os.IsNotExist(err) {
		t.Fatalf("the snapshot ran the repository's fsmonitor hook (%v)", err)
	}

	if snap, warned := collectStart(t.TempDir(), run, 20*time.Second); snap != (harness.SessionStart{Date: startDate}) || len(warned) != 0 {
		t.Fatalf("outside a repository: snapshot %+v, warnings %q; want the date alone", snap, warned)
	}

	fresh := t.TempDir()
	gitIn(t, fresh, "init", "-q", "-b", "trunk")
	snap, warned = collectStart(fresh, run, 20*time.Second)
	if snap.Branch != "trunk" || snap.Status != "## No commits yet on trunk\n" || snap.Log != "" || len(warned) != 0 {
		t.Fatalf("a new repository: snapshot %+v, warnings %q", snap, warned)
	}

	broken := committedRepo(t, "trunk", "a commit")
	write(filepath.Join(broken, ".git", "config"), "[core\n")
	snap, warned = collectStart(broken, run, 20*time.Second)
	if snap != (harness.SessionStart{Date: startDate}) || len(warned) != 1 || !strings.Contains(warned[0], "git rev-parse failed") {
		t.Fatalf("a repository whose config git cannot parse: snapshot %+v, warnings %q; want the date alone and it said", snap, warned)
	}
}

// TestSessionStartIgnoresRepositoryOverrides: craze started with GIT_DIR and
// GIT_WORK_TREE pointing at another repository — from a hook, say — still
// snapshots its own workspace, because the collector's environment has none
// of the variables that choose a repository, a work tree, an index, an object
// store or a configuration in place of the one -C finds. The control shows
// the same environment redirects a plain git. Ordinary variables stay.
func TestSessionStartIgnoresRepositoryOverrides(t *testing.T) {
	realGit(t)
	other := committedRepo(t, "elsewhere", "another repository's commit")
	ws := committedRepo(t, "trunk", "the workspace's commit")
	t.Setenv("GIT_DIR", filepath.Join(other, ".git"))
	t.Setenv("GIT_WORK_TREE", other)
	if out, err := exec.Command("git", "-C", ws, "branch", "--show-current").Output(); err != nil || string(out) != "elsewhere\n" {
		t.Fatalf("control: with GIT_DIR set a plain git reports %q (%v), so ignoring it proves nothing", out, err)
	}
	// Built now, so its environment is read with the overrides in it.
	run := execGit(gitEnviron(nativeTestTable("http://127.0.0.1:9/v1")))
	snap, warned := collectStart(ws, run, 20*time.Second)
	if snap.Branch != "trunk" || !strings.Contains(snap.Log, "the workspace's commit") || len(warned) != 0 {
		t.Fatalf("snapshot %+v, warnings %q; want the workspace's own", snap, warned)
	}

	selection := append(slices.Clone(gitSelection), "GIT_CONFIG_KEY_0", "GIT_CONFIG_VALUE_0", "GIT_CONFIG_KEY_12")
	for _, name := range selection {
		t.Setenv(name, "set")
	}
	env := gitEnviron(nativeTestTable("http://127.0.0.1:9/v1"))
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if slices.Contains(selection, name) {
			t.Fatalf("%s reached git's environment", name)
		}
	}
	for _, name := range []string{"HOME", "PATH", "XDG_CONFIG_HOME", "GIT_CONFIG_NOSYSTEM"} {
		if !slices.ContainsFunc(env, func(kv string) bool { return strings.HasPrefix(kv, name+"=") }) {
			t.Fatalf("%s is missing from git's environment", name)
		}
	}
}

// TestSessionStartKillsWhatGitStarted: a git that does not answer is killed
// with everything it started — here a wrapper's child that would sleep on —
// and the collection, the kill and the reaping included, ends within its
// budget. The wrapper stands in for git on PATH and records each child's pid.
func TestSessionStartKillsWhatGitStarted(t *testing.T) {
	bin, pids := t.TempDir(), filepath.Join(t.TempDir(), "pids")
	wrapper := "#!/bin/sh\nsleep 30 &\necho $! >> '" + pids + "'\nwait\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(wrapper), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	run := execGit(gitEnviron(nativeTestTable("http://127.0.0.1:9/v1")))

	const budget = 800 * time.Millisecond
	start := time.Now()
	snap, warned := collectStart(fakeRepo(t), run, budget)
	if took := time.Since(start); took > budget+200*time.Millisecond {
		t.Fatalf("the collection took %v; want it done within its %v budget", took, budget)
	}
	if snap != (harness.SessionStart{Date: startDate}) || len(warned) != 1 || !strings.Contains(warned[0], "git rev-parse did not finish within 800ms") {
		t.Fatalf("snapshot %+v, warnings %q; want the date alone and the timeout said", snap, warned)
	}
	raw, err := os.ReadFile(pids)
	if err != nil || len(strings.Fields(string(raw))) == 0 {
		t.Fatalf("the wrapper recorded no child (%v)", err)
	}
	gone := time.Now().Add(5 * time.Second)
	for _, field := range strings.Fields(string(raw)) {
		pid, err := strconv.Atoi(field)
		if err != nil {
			t.Fatal(err)
		}
		for syscall.Kill(pid, 0) == nil {
			if time.Now().After(gone) {
				t.Fatalf("the wrapper's child %d outlived the collection", pid)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}

// TestSessionStartGitHasNoProviderKeys: the production runner gives git the
// bash tool's environment — craze's own less every provider's env_keys and
// every OPENAI_* (tool.ChildEnviron) — so a key in craze's environment never
// reaches git or anything git starts. An alias that runs env shows git's own
// environment as its children see it; the control is a variable that is no
// key, which stays.
func TestSessionStartGitHasNoProviderKeys(t *testing.T) {
	realGit(t)
	t.Setenv("NATIVE_TEST_KEY", nativeCanary) // the test provider's env_keys
	t.Setenv("OPENAI_API_KEY", "sk-openai-in-the-parent")
	t.Setenv("CRAZE_SNAPSHOT_KEPT", "kept")
	// Built now, so its environment is read with the keys in it.
	run := execGit(gitEnviron(nativeTestTable("http://127.0.0.1:9/v1")))
	out, err := run(context.Background(), []string{"git", "-c", "alias.environ=!env", "environ"})
	if err != nil {
		t.Fatalf("git environ: %v", err)
	}
	env := strings.Split(string(out), "\n")
	if !slices.Contains(env, "CRAZE_SNAPSHOT_KEPT=kept") {
		t.Fatalf("control: git's environment lacks an ordinary variable:\n%s", out)
	}
	for _, line := range env {
		if strings.HasPrefix(line, "NATIVE_TEST_KEY=") || strings.HasPrefix(line, "OPENAI_API_KEY=") ||
			strings.Contains(line, nativeCanary) {
			t.Fatalf("a provider key reached git's environment: %q", line)
		}
	}
}

// startWithGit starts a fixture session on ws whose git is g, journaled in
// dir, with its diagnostics in diag.
func startWithGit(t *testing.T, f *nativeFixture, opts Options, g *fakeGit) *nativeSession {
	t.Helper()
	s := f.session(opts)
	s.gitRun = g.run
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	takeStartDelta(t, s.log)
	return s
}

// lastSystem runs one turn on s and returns the system prompt its request
// sent.
func lastSystem(t *testing.T, f *nativeFixture, s *nativeSession, prompt string) string {
	t.Helper()
	a := f.models["test/a"]
	a.push(answer("done"))
	if _, err := s.Prompt(context.Background(), prompt); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	calls := a.requests()
	return systemText(t, calls[len(calls)-1])
}

// TestNativeSessionStartReachesThePrompt: through Start, the collector runs
// against the workspace the harness opens on, the prompt ends with the
// section it rendered — the fixture clock's date, the model the session
// started on, git's parts — and the prompt_sources note records that section
// apart: its digest and its size, as sent.
func TestNativeSessionStartReachesThePrompt(t *testing.T) {
	f := newNativeFixture(t)
	ws := fakeRepo(t)
	dir := filepath.Join(t.TempDir(), "journal")
	g := repoGit(nil)
	s := startWithGit(t, f, Options{Workspace: ws, JournalDir: dir}, g)
	w := journalOf(t, s.log)
	inc := s.Incarnation()
	sent := lastSystem(t, f, s, "hi")
	closeJournaled(t, s, w)

	for _, argv := range g.calls() {
		if at := len(gitPrefix("")) - 1; argv[at] != ws {
			t.Fatalf("git ran in %q; want the session's workspace %q", argv[at], ws)
		}
	}
	at := strings.LastIndex(sent, "\n# Session start\n")
	if at < 0 {
		t.Fatalf("the prompt has no session-start section:\n%s", sent)
	}
	section := sent[at+1:]
	for _, want := range []string{
		"Today's date at session start: " + startDate + "\n",
		"The top-level session started on ",
		"Current branch: feature/x\nDefault branch: main\n",
		"## feature/x...origin/feature/x [ahead 1]\n M a.go\n",
		"1234567 the last commit\n",
	} {
		if !strings.Contains(section, want) {
			t.Errorf("the section lacks %q:\n%s", want, section)
		}
	}

	notes := diags(assertOneJournal(t, dir, w, inc), journal.DiagPromptSources)
	if len(notes) != 1 {
		t.Fatalf("%d prompt_sources notes, want exactly one", len(notes))
	}
	if got, want := jsonString(notes[0], "snapshot"), fmt.Sprintf("%x %d", sha256.Sum256([]byte(section)), len(section)); got != want {
		t.Fatalf("the note's snapshot is %q; want the section's %q", got, want)
	}
}

// TestNativeSessionStartSeamWins: a tweak that sets its own snapshot keeps it
// — tweak has the last word on every harness option — and no git runs.
func TestNativeSessionStartSeamWins(t *testing.T) {
	f := newNativeFixture(t)
	f.edit = func(o *harness.Options) { o.Snapshot = harness.SessionStart{Date: "the seam's own date"} }
	g := repoGit(nil)
	s := startWithGit(t, f, Options{Workspace: fakeRepo(t)}, g)
	sent := lastSystem(t, f, s, "hi")
	if !strings.Contains(sent, "Today's date at session start: the seam's own date\n") || strings.Contains(sent, startDate) {
		t.Fatalf("the prompt does not carry the seam's snapshot:\n%s", sent)
	}
	if calls := g.calls(); len(calls) != 0 {
		t.Fatalf("git ran %d times under a seam that set the snapshot", len(calls))
	}
}

// TestNativeSessionStartWarnsRedacted: a part that fails is said on the lane
// content problems are, which redacts a configured key — git's own message
// names paths and refs a repository chose — and the session starts without
// that part.
func TestNativeSessionStartWarnsRedacted(t *testing.T) {
	f := newNativeFixture(t)
	diag := &bytes.Buffer{}
	g := repoGit(map[string]gitReply{"log": gitExit(128, "fatal: bad revision '"+nativeCanary+"'")})
	s := startWithGit(t, f, Options{Workspace: fakeRepo(t), Diag: diag}, g)
	sent := lastSystem(t, f, s, "hi")
	if got := diag.String(); !strings.Contains(got, "session start: git log failed") ||
		strings.Contains(got, nativeCanary) || !strings.Contains(got, redact.Marker) {
		t.Fatalf("diagnostics %q; want the log's failure, redacted", got)
	}
	if strings.Contains(sent, "git log --oneline") || !strings.Contains(sent, "Current branch: feature/x") {
		t.Fatalf("the prompt should have every part but the log:\n%s", sent)
	}
}

// TestNativeLoadCollectsItsOwnSnapshot: a resumed incarnation collects again —
// its date and its repository are this Open's — and its prompt's section has
// no model line, since the prompt is frozen before the resumed model resolves.
func TestNativeLoadCollectsItsOwnSnapshot(t *testing.T) {
	f := newNativeFixture(t)
	ws := fakeRepo(t)
	// The stored session's own snapshot is the seam's, so it runs no git; the
	// load's is collected, through the fake.
	f.edit = func(o *harness.Options) { o.Snapshot = harness.SessionStart{Date: "the stored session's"} }
	id := storeNativeSession(t, f, Options{Workspace: ws}, "test/a", []step{answer("first")})
	f.edit = nil

	g := repoGit(map[string]gitReply{"branch": gitOK("since-then\n")})
	loaded := f.session(Options{Workspace: ws, LoadSessionID: id})
	loaded.gitRun = g.run
	if _, err := startLoad(t, loaded); err != nil {
		t.Fatalf("the load: %v", err)
	}
	sent := lastSystem(t, f, loaded, "again")
	if !strings.Contains(sent, "Current branch: since-then\n") || strings.Contains(sent, "started on") {
		t.Fatalf("the resumed prompt's section is not this load's, or names a model:\n%s", sent[strings.LastIndex(sent, "# Session start"):])
	}
	if len(g.calls()) != int(gitParts) {
		t.Fatalf("the load ran git %d times; want one snapshot's %d", len(g.calls()), gitParts)
	}
}
