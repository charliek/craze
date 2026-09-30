package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/charliek/craze/internal/harness"
	"github.com/charliek/craze/internal/harness/modeltable"
	"github.com/charliek/craze/internal/harness/tool"
)

// The session-start snapshot (plan 029 §3.2 L2, D-67): what the model is told
// of the moment its session started — the local date and, for a workspace
// inside a git work tree, the branch, the default branch, the status and the
// last few commits. The harness renders it as the frozen prompt's last
// section, but it holds no clock and runs no command (D-02), so this adapter
// collects it, once per open() — a new session's or a resumed incarnation's —
// and hands it over as data (harness.Options.Snapshot).
//
// Everything here is bounded before it leaves the adapter: a repository
// writes every byte of it, and the prompt it lands in rides on every request
// of the session.

const (
	// sessionStartBudget is the one deadline every git command of a snapshot
	// shares. They run at once, so it bounds the whole collection, and a part
	// that has not answered by then is left out rather than waited for: a
	// session never starts late because a repository is slow.
	sessionStartBudget = 2 * time.Second
	// sessionStartDate is the date's layout: the day, and its name, which a
	// model asked about "next Friday" otherwise has to work out.
	sessionStartDate = "2006-01-02 (Monday)"

	maxStartBranch      = 200     // bytes of one branch name
	maxStartStatus      = 4 << 10 // bytes of the status, its truncation marker's line included
	maxStartStatusLines = 100     // lines of the status, the marker's included
	maxStartCommit      = 160     // bytes of one line of the log
	startCommits        = 5       // lines of the log
	// startTruncated ends a status the bounds cut, as the harness ends a
	// document its budget cut.
	startTruncated = "[craze: truncated]"
	// maxGitOutput is how much of one command's output is kept at all. The
	// bounds above keep far less; this only stops a status of a hundred
	// thousand untracked files from being held whole to be thrown away.
	maxGitOutput = 64 << 10
)

// gitRunner runs one git command line — argv[0] is "git" — under ctx, and
// returns what it wrote to standard output. A command that exits non-zero
// fails with an error that has an ExitCode method, as *exec.ExitError does,
// so the collector can tell "no" from "broken"; one that could not be started
// because there is no git fails with exec.ErrNotFound. Production's is
// execGit(gitEnviron(table)); a test hands in its own (nativeSession.gitRun).
type gitRunner func(ctx context.Context, argv []string) ([]byte, error)

// gitPart is one of the commands a snapshot runs.
type gitPart int

const (
	gitWorkTree gitPart = iota // is the workspace inside a work tree at all
	gitBranch
	gitDefault // what origin/HEAD names
	gitStatus
	gitLog
	gitParts
)

// gitPartNames are the parts as the warn lane names them.
var gitPartNames = [gitParts]string{"rev-parse", "branch", "symbolic-ref origin/HEAD", "status", "log"}

// sessionStartArgv is every command a snapshot of workspace runs. Each one
// is `git --no-optional-locks -c core.fsmonitor=false -C <workspace> …`:
//
//   - no optional locks, so that `git status` never takes index.lock in the
//     user's repository — a status that refreshed the index behind the user's
//     back could make their own git command, run at that moment, fail;
//   - no fsmonitor, because core.fsmonitor may name a command, and the
//     repository's own config may set it: a status would run whatever a
//     cloned or unpacked repository chose, the moment craze opened in it;
//   - -C rather than a working directory, so the runner needs nothing but its
//     argv.
func sessionStartArgv(workspace string) [gitParts][]string {
	git := func(args ...string) []string {
		return append([]string{"git", "--no-optional-locks", "-c", "core.fsmonitor=false", "-C", workspace}, args...)
	}
	return [gitParts][]string{
		gitWorkTree: git("rev-parse", "--is-inside-work-tree"),
		gitBranch:   git("branch", "--show-current"),
		gitDefault:  git("symbolic-ref", "-q", "--short", "refs/remotes/origin/HEAD"),
		gitStatus:   git("status", "--porcelain=v1", "--branch"),
		// --no-color: a user's color.ui=always would otherwise put escape
		// sequences in the subjects, which the bounds would then replace
		// with noise.
		gitLog: git("log", "--oneline", "--no-color", "-n", strconv.Itoa(startCommits)),
	}
}

// collectSessionStart is the snapshot of workspace: the date from now, and
// the git parts, all run at once through run under one budget. It never
// fails.
//
// A workspace with no repository above it (inGitTree) has only the date, and
// not one process is started for it. Otherwise a part whose absence is an
// answer says nothing: no origin/HEAD, no commits yet on a new repository's
// branch, a workspace inside a .git directory, no git installed. Every other
// failure — the work-tree check's included, since a .git was found: a config
// git cannot parse, a repository it will not trust, one it cannot read — and
// every part that has not finished in time is left out and reported through
// warn, as content problems are (contentWarn), which redacts the line. A
// work-tree check that fails is reported once, for the whole: the other parts
// mean nothing without it.
func collectSessionStart(workspace string, now func() time.Time, run gitRunner, budget time.Duration, warn func(string)) harness.SessionStart {
	snap := harness.SessionStart{Date: now().Format(sessionStartDate)}
	if !inGitTree(workspace) {
		return snap
	}
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()

	argv := sessionStartArgv(workspace)
	var (
		out   [gitParts][]byte
		errs  [gitParts]error
		timed [gitParts]bool
		wg    sync.WaitGroup
	)
	for i := range argv {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i], errs[i] = run(ctx, argv[i])
			// Read as the part ends, so a part that failed on its own before
			// the deadline is not blamed on a slower one that met it. A runner
			// may stop a command a little before the deadline, to be done with
			// it by then (execGit), and says so with the deadline's error.
			timed[i] = errs[i] != nil && (ctx.Err() != nil || errors.Is(errs[i], context.DeadlineExceeded))
		}()
	}
	wg.Wait()

	report := func(p gitPart) {
		if timed[p] {
			warn(fmt.Sprintf("session start: git %s did not finish within %s; the system prompt goes without it", gitPartNames[p], budget))
			return
		}
		warn(fmt.Sprintf("session start: git %s failed (%v); the system prompt goes without it", gitPartNames[p], errs[p]))
	}

	// The work-tree check decides whether the rest mean anything. A .git was
	// found above the workspace, so git refusing it is a failure to say, not
	// an absence: the repository is there and the model will not hear of it.
	// Only a machine with no git at all is quiet about it.
	if err := errs[gitWorkTree]; err != nil {
		if !errors.Is(err, exec.ErrNotFound) {
			report(gitWorkTree)
		}
		return snap
	}
	if strings.TrimSpace(string(out[gitWorkTree])) != "true" {
		return snap // inside a .git directory: not a work tree
	}

	if errs[gitBranch] != nil {
		report(gitBranch)
	} else {
		snap.Branch = startLine(out[gitBranch])
	}
	if err := errs[gitDefault]; err != nil {
		// symbolic-ref -q exits 1, and says nothing, when origin/HEAD is not
		// there or not a symbolic ref: a repository with no origin, or one
		// cloned before git recorded it.
		if code, exited := exitCode(err); !exited || code != 1 {
			report(gitDefault)
		}
	} else {
		snap.DefaultBranch = startLine([]byte(strings.TrimPrefix(firstLine(out[gitDefault]), "origin/")))
	}
	if errs[gitStatus] != nil {
		report(gitStatus)
	} else {
		snap.Status = startStatus(out[gitStatus])
	}
	if errs[gitLog] != nil {
		// A branch with no commits yet has no log, and git log fails on it;
		// porcelain's header, which is not translated, says so.
		if timed[gitLog] || !strings.HasPrefix(snap.Status, "## No commits yet on ") {
			report(gitLog)
		}
	} else {
		snap.Log = startLog(out[gitLog])
	}
	return snap
}

// sessionStart is this session's snapshot of workspace, reported on warn.
// The date is read from now — harness.Options.Now, the clock the transcript
// is stamped with, which a test fixes — or the wall clock when it is nil, and
// the commands go through the session's gitRun, else execGit in the
// environment table leaves a command (gitEnviron).
func (s *nativeSession) sessionStart(workspace string, now func() time.Time, table *modeltable.Table, warn func(string)) harness.SessionStart {
	if now == nil {
		now = time.Now
	}
	run := s.gitRun
	if run == nil {
		run = execGit(gitEnviron(table))
	}
	return collectSessionStart(workspace, now, run, sessionStartBudget, warn)
}

// inGitTree reports whether workspace, or a directory above it, holds a .git
// that could be a repository: a file — a linked worktree's or a submodule's
// pointer to its git directory — or a directory with a HEAD in it, which git
// requires of a repository and without which it goes on looking further up,
// as this does. It is checked before any process is started, so a workspace
// outside every repository — a new directory, a temporary one, every test's —
// costs a few stats and nothing else. The environment that could point git
// somewhere else (GIT_DIR and the rest) is not consulted: gitEnviron strips
// it, so this walk and git's own discovery look at the same directories.
func inGitTree(workspace string) bool {
	for dir := filepath.Clean(workspace); ; dir = filepath.Dir(dir) {
		dot := filepath.Join(dir, ".git")
		if fi, err := os.Stat(dot); err == nil {
			if !fi.IsDir() {
				return true
			}
			if _, err := os.Stat(filepath.Join(dot, "HEAD")); err == nil {
				return true
			}
		}
		if filepath.Dir(dir) == dir {
			return false
		}
	}
}

// gitSelection are the variables with which an environment tells git which
// repository, work tree, index, object store or configuration to use in place
// of the ones it would find from -C, or where to stop looking for them. craze
// may have been started with them — from a git hook, an alias, a script that
// exported GIT_DIR — and the snapshot is of the workspace, never of wherever
// they point. GIT_CONFIG_KEY_<n> and GIT_CONFIG_VALUE_<n>, which are
// numbered, go by prefix (gitSelectionPrefixes).
var (
	gitSelection = []string{
		"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES",
		"GIT_COMMON_DIR", "GIT_NAMESPACE", "GIT_CEILING_DIRECTORIES", "GIT_DISCOVERY_ACROSS_FILESYSTEM",
		"GIT_CONFIG", "GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM", "GIT_CONFIG_COUNT", "GIT_CONFIG_PARAMETERS",
	}
	gitSelectionPrefixes = []string{"GIT_CONFIG_KEY_", "GIT_CONFIG_VALUE_"}
)

// gitEnviron is the environment the snapshot's git runs in: the one the
// harness's bash tool gives every command it runs (openTools), by the same
// call — craze's own environment less every variable the table knows holds a
// key (CredentialEnvNames, plan 031 C2r2) and every OPENAI_*
// (tool.ChildEnviron) — and less gitSelection too. git needs no provider key,
// and whatever it starts in turn — a hook, a helper, an alias — should not be
// handed one either. Everything else stays: HOME and XDG_CONFIG_HOME, which
// say where the user's own git config is, PATH, the locale.
func gitEnviron(table *modeltable.Table) []string {
	names := append(slices.Clone(gitSelection), table.CredentialEnvNames()...)
	return slices.DeleteFunc(tool.ChildEnviron(os.Environ(), names), func(kv string) bool {
		return slices.ContainsFunc(gitSelectionPrefixes, func(p string) bool { return strings.HasPrefix(kv, p) })
	})
}

// exitCode is the status err says a command exited with, and whether it says
// one at all.
func exitCode(err error) (int, bool) {
	var e interface{ ExitCode() int }
	if errors.As(err, &e) {
		return e.ExitCode(), true
	}
	return 0, false
}

// startLine is a one-line part: its first line once every control character
// is replaced and every line separator made a newline, trimmed, and cut to
// maxStartBranch bytes on a rune boundary.
func startLine(out []byte) string {
	line, _, _ := strings.Cut(replaceControls(string(out)), "\n")
	return truncateUTF8(strings.TrimSpace(line), maxStartBranch)
}

// startStatus is the status within its bounds, the truncation marker counted
// in both: at most maxStartStatusLines lines and maxStartStatus bytes, whole
// lines, and when anything was left out startTruncated on the last line of
// its own. A first line longer than the whole budget keeps what fits of it.
func startStatus(out []byte) string {
	text := strings.TrimRight(replaceControls(string(out)), "\n")
	if text == "" {
		return ""
	}
	lines := strings.Split(text, "\n")
	if len(text)+1 <= maxStartStatus && len(lines) <= maxStartStatusLines {
		return text + "\n"
	}
	marker := startTruncated + "\n"
	room := maxStartStatus - len(marker)
	var b strings.Builder
	for i, line := range lines {
		if i == maxStartStatusLines-1 || b.Len()+len(line)+1 > room {
			if b.Len() == 0 {
				b.WriteString(truncateUTF8(line, room-1) + "\n")
			}
			break
		}
		b.WriteString(line + "\n")
	}
	return b.String() + marker
}

// startLog is the log within its bounds: at most startCommits lines, each cut
// to maxStartCommit bytes on a rune boundary.
func startLog(out []byte) string {
	text := strings.TrimRight(replaceControls(string(out)), "\n")
	if text == "" {
		return ""
	}
	lines := strings.Split(text, "\n")
	if len(lines) > startCommits {
		lines = lines[:startCommits]
	}
	for i, line := range lines {
		lines[i] = truncateUTF8(line, maxStartCommit)
	}
	return strings.Join(lines, "\n") + "\n"
}

// replaceControls is s with every control character but the newline — an
// escape sequence's ESC, a carriage return that would draw over its own line,
// a NUL — replaced by U+FFFD, and every byte that is not UTF-8 too: nothing a
// repository names can act on whatever later shows the prompt. A Unicode line
// or paragraph separator (U+2028, U+2029) becomes the newline it is, so the
// bounds count every line there is.
func replaceControls(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case unicode.In(r, unicode.Zl, unicode.Zp):
			return '\n'
		case r != '\n' && unicode.IsControl(r):
			return utf8.RuneError
		}
		return r
	}, strings.ToValidUTF8(s, string(utf8.RuneError)))
}

// firstLine is out up to its first newline.
func firstLine(out []byte) string {
	line, _, _ := strings.Cut(string(out), "\n")
	return line
}

// gitReap is how long before the collection's deadline execGit kills a
// command that is still running, and so the longest it then waits for the
// command's output to close: a command that has to be killed is gone and
// reaped by the deadline, not after it.
const gitReap = 100 * time.Millisecond

// execGit is the production gitRunner over env (gitEnviron): each argv run as
// a process of its own in exactly that environment, in a process group of its
// own. Its output is kept up to maxGitOutput and its error names the first
// line git wrote to standard error, which is where git says what was wrong.
//
// A command still running gitReap before ctx's deadline is killed with its
// whole group — whatever git started, a helper or a hook or a wrapper's
// child, and not only git — and reported with the deadline's error. The group
// is signalled by its id, which is git's pid. While any member of the group
// lives that id is the group's and no one else's; the one window left is git
// reaped with its group empty in the very instant the deadline fires, when the
// signal reaches a group only if a new one claimed the id within that instant.
func execGit(env []string) gitRunner {
	return func(ctx context.Context, argv []string) ([]byte, error) {
		if deadline, ok := ctx.Deadline(); ok {
			var cancel context.CancelFunc
			ctx, cancel = context.WithDeadline(ctx, deadline.Add(-gitReap))
			defer cancel()
		}
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
		// Never nil: a nil Env is craze's whole environment, keys and all.
		cmd.Env = append([]string{}, env...)
		stdout, stderr := &cappedWriter{limit: maxGitOutput}, &cappedWriter{limit: 512}
		cmd.Stdout, cmd.Stderr = stdout, stderr
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
		// A process that left the group can still hold the pipes open;
		// closing them is bounded, and within the budget.
		cmd.WaitDelay = gitReap
		if err := cmd.Run(); err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if msg := strings.TrimSpace(firstLine(stderr.buf)); msg != "" {
				return nil, fmt.Errorf("%w: %s", err, msg)
			}
			return nil, err
		}
		return stdout.buf, nil
	}
}

// cappedWriter keeps the first limit bytes written to it and accepts, and
// drops, the rest: a command writing more than is wanted is neither blocked
// nor held whole in memory. It is deliberately not an io.ReaderFrom, which
// io.Copy would use in place of Write and so skip the cap.
type cappedWriter struct {
	buf   []byte
	limit int
}

func (w *cappedWriter) Write(p []byte) (int, error) {
	if room := w.limit - len(w.buf); room > 0 {
		w.buf = append(w.buf, p[:min(len(p), room)]...)
	}
	return len(p), nil
}
