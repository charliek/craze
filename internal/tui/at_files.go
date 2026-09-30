package tui

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// The composer's `@` file source (plan 030 §3.16, owner decision 10): the
// files and directories under the shown session's workspace, for a mention
// written into the prompt as `@relative/path` — the text every provider
// receives, nothing expanded or attached (§3.16; attaching contents is a
// follow-up). It is completePopup's source (complete.go), for the composer's
// `@` popup (composer_at.go).
//
// Two halves:
//
//   - The search: one listing of the workspace per popup opening, off the
//     Update — the popup's keyed load (X122), keyed by the workspace, so every
//     keystroke after the first matches over what it brought back and nothing
//     is listed twice. `rg --files` when rg is on PATH; else `git ls-files`
//     in a git work tree; else a walk of the tree. Each is bounded — at most
//     atFilesMax paths, the walk atFilesWalkMax entries, and what the listing
//     holds, its files and the directories derived from them, at most
//     atFilesHoldMax candidates of atFilesBytesMax bytes — and the listing
//     runs on a goroutine of its own, handing what it keeps to the search,
//     which builds the index as it comes and answers at atFilesTimeout, or
//     at once when the popup stops awaiting it (it closed, its workspace or
//     the session shown changed: X125), whatever the listing is doing; its
//     tool is killed then, and reaped on a goroutine of its own whatever the
//     listing is inside, and the listing stops at its next check. Only files
//     are listed; the directories offered are derived from their paths, so
//     an empty directory is never offered.
//   - The match: every keystroke, in the Update, over the listing — a
//     case-insensitive subsequence of the path, scored with fzf's shape
//     (segment starts, runs, gaps) and a bonus above every other for a last
//     segment that starts with the query; ranked by score, then the shorter
//     path, then the path's bytes. The best atFilesRankMax are the popup's
//     candidates and the rest are counted (completeAnswer.More). A query with
//     a "/" in it names a directory: what is up to its last "/" is a prefix
//     every candidate's path starts with, and the rest is matched inside it —
//     tab on a directory writes `@dir/` and the popup stays open on it (X127).

const (
	// atFilesSourceID names the source: a load's result is stamped with it.
	atFilesSourceID = "composer-files"
	// atFilesMax is the most paths a tool's listing is read for (§3.16): past
	// it craze stops reading and kills the tool, and the list says it is
	// partial. A repository bigger than this is typed into more narrowly.
	atFilesMax = 50000
	// atFilesHoldMax and atFilesBytesMax bound what a listing holds, however
	// its paths are shaped. atFilesMax bounds the paths, but not the
	// directories derived from them — a file ten directories deep brings ten
	// — nor their bytes: a record may be 64 KiB, and each directory above it
	// nearly as long. So the candidates a listing holds — each file it keeps
	// and each directory it has met — are at most atFilesHoldMax, a
	// directory for every file at the paths' cap; and their bytes, each
	// candidate's own, at most atFilesBytesMax, some 170 a path at that cap.
	// A real tree of the paths' cap's size sits well inside both: the Google
	// Cloud SDK's 49,980 files are 59,142 candidates of 3.7 MiB (C17r's
	// measure; Go's own tree averages 42 bytes a path). A directory's bytes
	// are counted though they are a file's (its name is a substring of the
	// path it came from): the match reads every candidate's fold at every
	// keystroke, so the bytes bound that work as well as the memory. A path
	// that would take the listing past either stops it there, as the paths'
	// cap does — the tool killed, the list said to be partial — unless no
	// listing could hold it (atListing.overflow).
	atFilesHoldMax  = 100000
	atFilesBytesMax = 8 << 20
	// atFilesBatch is how many candidates the listing keeps before it hands
	// them to the search (atListing.flush) — which checks its deadline and
	// the popup's cancel between two batches — and it hands over what it
	// has, however few, before any call that may wait: a read of the tool's
	// output, a directory's open or read.
	atFilesBatch = 1024
	// atFilesWalkMax bounds the walk craze does itself when neither tool can
	// list the tree (§3.16): the directory entries it reads, all directories
	// together, atFilesWalkBatch at a time with the load's context checked
	// between batches — X137's listing, over a tree.
	atFilesWalkMax   = 20000
	atFilesWalkBatch = 256
	// atFilesTimeout is how long one search may take, tool and walk together
	// (§3.16): past it the search answers, offering what it had built, and
	// the listing stops.
	atFilesTimeout = 3 * time.Second
	// atFilesRankMax is how many of a query's matches the popup is handed,
	// best first: eight rows are drawn, the arrows scroll through these, and
	// the rest are counted under them. The popup checks each candidate it is
	// handed and measures its columns over all of them (completeColumns), so
	// a query's answer is kept to a size that costs nothing to draw however
	// many paths it matched.
	atFilesRankMax = 100
	// atFilesRecordMax is the longest record of a tool's listing read: past
	// it the record is dropped whole. A path the system opens is at most
	// PATH_MAX (4096 on Linux, 1024 on macOS); a record sixteen times that is
	// no path anyone could mention.
	atFilesRecordMax = 64 << 10
	// atFilesStderrMax is how much of a tool's stderr is kept: its first line
	// is the note a failed search shows.
	atFilesStderrMax = 4 << 10
	// atFilesWaitDelay is exec's bound on the tool's pipes once it has exited
	// or been killed: a process it left behind holding its stderr open cannot
	// hold the search past it.
	atFilesWaitDelay = time.Second
)

// The tools' command lines (§3.16). rg lists every file it would search —
// .gitignore, .ignore and git's excludes honoured, dotfiles shown — with
// NUL after each path, and never a .git directory's (or a submodule's .git
// file) that --hidden would reach. --no-config is craze's own addition, as
// internal/harness's grep and glob pass it: a user's RIPGREP_CONFIG_PATH
// cannot change what is listed (a `--type` or `--max-depth` there would
// silently narrow it). git lists what it tracks (-c) and what it does not
// but would not ignore (-o, --exclude-standard), NUL-separated, relative to
// the directory it runs in.
var (
	atFilesRGArgs  = []string{"--no-config", "--files", "--hidden", "-g", "!.git", "-0"}
	atFilesGitArgs = []string{"ls-files", "-co", "--exclude-standard", "-z"}
)

// errAtFilesTimeout is the cause a search's context ends with when
// atFilesTimeout passes, and a search that read nothing by then answers it.
var errAtFilesTimeout = errors.New("the file search timed out")

// The source's own words.
const (
	atFilesNoneNote   = "no files here"
	atFilesNoRootNote = "no workspace to search"
	// atFilesTitleLead leads the popup's title: `files in craze` — the
	// workspace by the name the status row gives it (workspaceName).
	atFilesTitleLead = "files in "
)

// atFilesCapTitle, atFilesWalkCapTitle and atFilesTimeoutTitle are what the
// title says of a list that is not the workspace's whole (atFilesTitle):
// drawn in the rule above the candidates, so that a file the list does not
// hold is not taken for one the workspace does not. A listing stopped by what
// it holds (atFilesHoldMax, atFilesBytesMax) is titled atFilesFullTitle.
var (
	atFilesCapTitle     = atFilesFullTitle(atFilesMax)
	atFilesWalkCapTitle = "only the first " + atGroupDigits(atFilesWalkMax) + " entries"
	atFilesTimeoutTitle = "listing stopped after " + atFilesTimeout.String()
)

// atFilesTitle is the popup's title rule: whose files these are — the
// workspace, by its status-row name — and, when the list is not the
// workspace's whole, why (partial: atFilesCapTitle, atFilesFullTitle's,
// atFilesWalkCapTitle or atFilesTimeoutTitle), after a `·`.
// Every answer has it, the search still running and a failure's note
// included, so the popup does not change height as the listing arrives, and
// its rows are never taken for the transcript's above them.
func atFilesTitle(root, partial string) string {
	t := atFilesTitleLead + workspaceName(root)
	if partial != "" {
		t += " · " + partial
	}
	return t
}

// atFilesFullTitle is the title of a list cut where it held files files: the
// paths' cap's words, with the number the list holds — which, cut by the
// directories or the bytes those files brought, is no round number (and is
// one only for a tree built to be cut there: a first path its directories'
// bytes all but fill the list with).
func atFilesFullTitle(files int) string {
	if files == 1 {
		return "only the first file"
	}
	return "only the first " + atGroupDigits(files) + " files"
}

// atGroupDigits is n written with a comma between each group of three
// digits: 50,000.
func atGroupDigits(n int) string {
	s := fmt.Sprint(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// ------------------------------------------------------------- the source

// atFileSource is the source: search lists a workspace — in production the
// disk's, atFileSource{search: newAtFileSearcher().search}; a test or a
// golden hands it a fixed listing of its own.
type atFileSource struct {
	search func(ctx context.Context, root string) completeLoaded
}

func (s atFileSource) completeID() string { return atFilesSourceID }

// complete answers a query from the workspace's listing — started, keyed by
// the workspace, the first time it is asked, and matched over from then on.
// A search that read nothing because it failed or ran out of time is a red
// note; one that read something is offered, its title saying so when it is
// partial (atFilesTitle).
func (s atFileSource) complete(q completeQuery) completeAnswer {
	root := q.Workspace
	if root == "" {
		// The composer offers no `@` popup without a workspace (§3.16).
		return completeAnswer{Note: atFilesNoRootNote}
	}
	title := atFilesTitle(root, "")
	l, back := q.Loaded(root)
	if !back {
		search := s.search
		return completeAnswer{Title: title, Load: &completeLoad{Key: root, Run: func(ctx context.Context) completeLoaded {
			return search(ctx, root)
		}}}
	}
	if l.Err != nil {
		return completeAnswer{Title: title, Note: atFilesErrNote(l.Err), NoteErr: true}
	}
	x, _ := l.Data.(*atFileIndex)
	if x != nil {
		title = atFilesTitle(root, x.title)
	}
	if x == nil || len(x.names) == 0 {
		return completeAnswer{Title: title, Note: atFilesNoneNote}
	}
	items, more := x.match(q.Text, atFilesRankMax)
	return completeAnswer{Title: title, Items: items, More: more}
}

// atFilesErrNote is the note for a search that offered nothing: it ran out of
// time, or it could not list the workspace — and why, in a few words.
func atFilesErrNote(err error) string {
	if errors.Is(err, errAtFilesTimeout) {
		return "listing files took over " + atFilesTimeout.String()
	}
	return "could not list files: " + sessErrText(err)
}

// ------------------------------------------------------------- the search

// atFileSearcher lists a workspace. Its seams are its fields: how a tool is
// looked for on PATH, whether a directory is in a git work tree, the timer,
// how the walk opens a directory, and how git's listing is checked on disk
// (atListing.lstat) — each the real one in production (newAtFileSearcher),
// and a test's own to force every fallback, fire the timeout without waiting
// for it, and hold a directory's read or a stat of git's as a stalled disk
// would.
type atFileSearcher struct {
	look    func(file string) (string, error)
	gitTree func(root string) bool
	after   func(d time.Duration, f func()) (stop func() bool)
	timeout time.Duration
	openDir func(name string) (atDir, error)
	lstat   func(name string) (os.FileInfo, error)

	// Test hooks, nil in production: started is told a tool's pid once it
	// has started, read how many records it has read after each one, walked
	// how many entries the walk has read after each batch — each on the
	// listing's goroutine; reaped a tool's pid once it has been reaped, on
	// its reaper's (tool); built how many candidates the search has built
	// after each batch it took, on the search's; ended that the listing's
	// goroutine has ended, its tool reaped.
	started func(pid int)
	read    func(records int)
	reaped  func(pid int)
	walked  func(entries int)
	built   func(candidates int)
	ended   func()
}

// atDir is a directory open for the walk: an *os.File in production.
type atDir interface {
	ReadDir(n int) ([]os.DirEntry, error)
	Close() error
}

// newAtFileSearcher is the searcher as craze runs it: tools found on PATH, a
// work tree found by its `.git` (discoverGit, the status row's search), the
// real clock, the disk's directories and its stats.
func newAtFileSearcher() atFileSearcher {
	return atFileSearcher{
		look:    exec.LookPath,
		gitTree: func(root string) bool { return discoverGit(root).dir != "" },
		after: func(d time.Duration, f func()) func() bool {
			return time.AfterFunc(d, f).Stop
		},
		timeout: atFilesTimeout,
		openDir: func(name string) (atDir, error) {
			f, err := os.Open(name)
			if err != nil {
				// Not f: a nil *os.File in an atDir is not a nil atDir.
				return nil, err
			}
			return f, nil
		},
		lstat: os.Lstat,
	}
}

// atFilesRun is how one listing went: whether it stopped at its cap — the
// paths', or the walk's entries' (walk) — or at what it holds (full), and so
// is partial; and why it failed when it did, which search offers nothing for
// only when nothing was kept.
type atFilesRun struct {
	capped bool
	full   bool
	walk   bool
	err    error
}

// search lists root and answers the listing as an index to match over — or,
// when it kept nothing, why: the timeout, or the failure. Its context is the
// popup's for the load (cancelled once the load is not awaited: the answer is
// then dropped, and nobody reads it), under the search's own timeout.
//
// The listing runs on a goroutine of its own (list), handing over what it
// keeps a batch at a time, and search builds each batch into the index as it
// takes it (atBuild) — so it answers at its deadline, or at once when its
// context ends, whatever the listing is doing: a directory read or a stat
// that the disk holds up (a stalled network mount) does not hold the answer.
// Between two batches it checks both, and at either offers what it had built
// — the timeout's partial list — or nothing, cancelled. Its return cancels
// the listing's context: its tool is killed then and reaped at once, on a
// goroutine of its own (tool), and the listing stops at its next check, its
// walk left. A call the listing is inside when that happens is not
// interrupted — no goroutine can be stopped inside a system call — and it
// ends when the call returns, holding nothing the popup waits on, nor any
// process.
func (s atFileSearcher) search(ctx context.Context, root string) completeLoaded {
	sctx, stop := context.WithCancelCause(ctx)
	defer stop(nil)
	disarm := s.after(s.timeout, func() { stop(errAtFilesTimeout) })
	defer disarm()

	batches := make(chan []string)
	result := make(chan atFilesRun, 1)
	go func() {
		result <- s.list(sctx, root, func(batch []string) bool {
			select {
			case batches <- batch:
				return true
			case <-sctx.Done():
				return false
			}
		})
		if s.ended != nil {
			s.ended()
		}
	}()

	var b atBuild
	var run atFilesRun
	listed := false
	// The context is checked before each batch as well as in the select,
	// which picks at random among the cases ready: a batch that is ready
	// when the deadline has passed is not built.
	for !listed && sctx.Err() == nil {
		select {
		case batch := <-batches:
			b.add(batch)
			if s.built != nil {
				s.built(b.size())
			}
		case run = <-result:
			// Every batch before it was taken: the channel is unbuffered.
			listed = true
		case <-sctx.Done():
		}
	}
	switch {
	case ctx.Err() != nil:
		return completeLoaded{Err: ctx.Err()}
	case !listed && b.size() == 0:
		return completeLoaded{Err: errAtFilesTimeout}
	case listed && run.err != nil && b.size() == 0:
		return completeLoaded{Err: run.err}
	}
	// A failure after some paths were kept — rg that could not read one
	// directory of many (its exit 2), git that failed late — leaves what it
	// kept standing, as internal/harness's glob keeps rg's rows at exit 2.
	x, files := b.index()
	switch {
	case !listed:
		// The timeout: whatever was built by then is offered, said to be
		// partial (nothing built was the note). A timer that fires in the
		// instant after a listing ended calls a whole list partial: the words
		// are wrong, the list is not.
		x.title = atFilesTimeoutTitle
	case run.full:
		x.title = atFilesFullTitle(files)
	case run.capped && run.walk:
		x.title = atFilesWalkCapTitle
	case run.capped:
		x.title = atFilesCapTitle
	}
	return completeLoaded{Data: x}
}

// list is the search's listing, by the first means there is (§3.16): rg on
// PATH; else git, in a git work tree; else the walk. git that fails having
// kept nothing — the tree is not a repository after all, git distrusts its
// owner (safe.directory), its index is broken — gives way to the walk, which
// needs none of that; rg that fails is the answer, since it walks the same
// directories the walk would. What each keeps goes to emit (atListing), and
// git's is checked on disk (s.lstat) as it is kept: rg and the walk list no
// symbolic link, and git lists what it tracks, whatever it has become.
func (s atFileSearcher) list(ctx context.Context, root string, emit func(batch []string) bool) atFilesRun {
	if bin, err := s.look("rg"); err == nil {
		return s.tool(newAtListing(ctx, root, emit, nil), bin, atFilesRGArgs, true)
	}
	if s.gitTree(root) {
		if bin, err := s.look("git"); err == nil {
			l := newAtListing(ctx, root, emit, s.lstat)
			run := s.tool(l, bin, atFilesGitArgs, false)
			if run.err == nil || l.files > 0 || ctx.Err() != nil {
				return run
			}
		}
	}
	return s.walk(newAtListing(ctx, root, emit, nil))
}

// tool runs bin with args in the listing's root and reads its NUL-separated
// listing into it (readPaths).
//
// It runs with root as its directory, with no shell and stdin /dev/null,
// under a context of its own inside the listing's: that context ending — the
// search no longer taking the listing (its timeout, or the popup no longer
// awaiting the load), or the listing reaching its cap or holding all it may
// — kills the tool (exec's own cancel, SIGKILL: a listing has nothing to
// flush) and closes craze's end of its stdout, so a read waiting on output
// that will never come returns.
//
// The tool is reaped (Wait) by a goroutine of its own, started with it, and
// not by the listing's once its reading is done: the listing's goroutine may
// be held in a call no context interrupts — git's check of a path (lstat) on
// a stalled mount — and a tool killed meanwhile would be left a zombie for
// as long as the call is stuck. So a tool that has ended, by itself or
// killed, is reaped at once, whatever the listing is inside. Its stdout is
// therefore a pipe of craze's own (os.Pipe), which Wait leaves alone, and not
// exec's StdoutPipe, which Wait closes: a Wait running beside the reading
// would take the rest of the listing from it. Its stderr is exec's, bounded
// by atFilesWaitDelay once the tool has ended. tool takes the reaper's
// answer before it returns, so a listing that ends — at the tool's end, a cap
// or holding all it may — has its tool reaped before the search answers. So
// tool returns promptly once its context ends, and never leaves a process
// behind that it started.
//
// rg's exit 1 with nothing listed is no files, not a failure (rg's own
// meaning); any other failure is its stderr's first line, or the exit.
func (s atFileSearcher) tool(l *atListing, bin string, args []string, rg bool) atFilesRun {
	ctx := l.ctx
	cctx, kill := context.WithCancel(ctx)
	defer kill()
	out, w, err := os.Pipe()
	if err != nil {
		return atFilesRun{err: err}
	}
	defer func() { _ = out.Close() }()
	cmd := exec.CommandContext(cctx, bin, args...)
	cmd.Dir = l.root
	cmd.Stdout = w
	stderr := &atHead{max: atFilesStderrMax}
	cmd.Stderr = stderr
	cmd.WaitDelay = atFilesWaitDelay
	err = cmd.Start()
	// The tool has its own copy of the write end: with craze's closed, a read
	// sees the listing's end once the tool's closes.
	_ = w.Close()
	if err != nil {
		return atFilesRun{err: err}
	}
	pid := cmd.Process.Pid
	if s.started != nil {
		s.started(pid)
	}
	// The reaper. Its answer is buffered: a listing held for good never
	// takes it, and the reaper ends all the same.
	waited := make(chan error, 1)
	go func() {
		werr := cmd.Wait()
		if s.reaped != nil {
			s.reaped(pid)
		}
		waited <- werr
	}()
	// A read of the pipe returns once it is closed (the pipe is pollable),
	// whether or not the kill reached whatever holds its other end.
	unhook := context.AfterFunc(cctx, func() { _ = out.Close() })
	capped, rerr := s.readPaths(out, l)
	if capped || l.full {
		// Enough: the tool is stopped, not waited for.
		kill()
	}
	// What was kept since the last hand-over, before a wait for the tool.
	l.flush()
	werr := <-waited
	unhook()

	run := atFilesRun{capped: capped, full: l.full}
	switch {
	case capped, l.full, ctx.Err() != nil:
		// Ended by craze — at a cap, or by the search's context, which
		// search reads for itself — and not by a failure of the tool's.
	case rerr != nil:
		run.err = rerr
	case werr == nil:
	case rg && atExitCode(werr) == 1 && l.files == 0:
		// rg found no files to list.
	default:
		run.err = atToolFailure(stderr.b, werr)
	}
	return run
}

// atExitCode is the exit code of a tool that exited by itself, -1 for one a
// signal ended or that never ran.
func atExitCode(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

// atToolFailure is a failed tool's error: the first line it wrote to stderr
// (`rg: ./x: Permission denied (os error 13)`, `fatal: not a git
// repository …`), else how it exited.
func atToolFailure(stderr []byte, werr error) error {
	for ln := range strings.SplitSeq(string(stderr), "\n") {
		if ln = strings.TrimSpace(ln); ln != "" {
			return errors.New(ln)
		}
	}
	return werr
}

// atHead keeps the first max bytes written to it and drops the rest, never
// failing, so the tool's stderr never blocks it.
type atHead struct {
	max int
	b   []byte
}

func (h *atHead) Write(p []byte) (int, error) {
	if room := h.max - len(h.b); room > 0 {
		h.b = append(h.b, p[:min(room, len(p))]...)
	}
	return len(p), nil
}

// readPaths reads a tool's listing, a path ended by NUL at a time (a last
// one with no NUL counts too; an empty one is nothing), into l each a token
// can carry (atFilePath) — until l says to stop: its context ended, or it
// holds all it may (l.full). It reads at most atFilesMax records: at the
// next one it stops, capped. A record longer than atFilesRecordMax is
// dropped whole and still counted. err is why reading stopped before the
// listing's end: io.EOF is its end, not an error — and a read of the pipe
// once the tool's context closed it is an error only search's context
// explains. Before each read of the pipe, which may wait for the tool, l
// hands over what it has kept (atFlushReader).
func (s atFileSearcher) readPaths(r io.Reader, l *atListing) (capped bool, err error) {
	br := bufio.NewReaderSize(atFlushReader{r: r, l: l}, atFilesRecordMax)
	records := 0
	long := false
	for {
		rec, rerr := br.ReadSlice(0)
		if errors.Is(rerr, bufio.ErrBufferFull) {
			// No NUL within the buffer: this record is too long to be a
			// path, and is dropped up to its NUL.
			long = true
			continue
		}
		rec = bytes.TrimSuffix(rec, []byte{0})
		wasLong := long
		long = false
		if len(rec) > 0 || wasLong {
			if records == atFilesMax {
				return true, nil
			}
			records++
			if p, ok := atFilePath(rec); ok && !wasLong && !l.add(p) {
				return false, nil
			}
			if s.read != nil {
				s.read(records)
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				return false, nil
			}
			return false, rerr
		}
	}
}

// atFlushReader is a tool's output as readPaths reads it: before each read,
// the listing hands over what it has kept since it last did (l.flush), so
// that the search holds every path read before the read waits — for a tool
// that has printed some and stalled. Once the search no longer takes them,
// the read fails with the listing's context's error.
type atFlushReader struct {
	r io.Reader
	l *atListing
}

func (f atFlushReader) Read(p []byte) (int, error) {
	if !f.l.flush() {
		return 0, f.l.ctx.Err()
	}
	return f.r.Read(p)
}

// atFilePath is a listed path as the popup offers it, and false for one it
// does not: a path with a control character or invalid UTF-8, which a token
// cannot carry (atTextOK, X126); and anything but a plain relative path under
// the root, which no tool listing it prints — absolute, with a `..` or a `.`
// in it, doubled slashes — and which could name a file outside the
// workspace. A leading "./" is taken off, as internal/harness's glob takes it
// off rg's paths.
func atFilePath(rec []byte) (string, bool) {
	p := string(rec)
	for strings.HasPrefix(p, "./") {
		p = p[2:]
	}
	if p == "" || p == "." || !atTextOK(p) || path.Clean(p) != p || !filepath.IsLocal(p) {
		return "", false
	}
	return p, true
}

// walk lists root by reading its directories, breadth first — the top of the
// tree before its depths, so a tree cut at the cap keeps what is nearest the
// root. It reads at most atFilesWalkMax entries in all, atFilesWalkBatch at a
// time, checking its context between batches, and keeps the regular files
// whose paths a token can carry. It never enters a `.git` or a
// `node_modules` (§3.16), and never follows a symbolic link — nor lists one,
// as rg does not (links are rg's to decide, and it leaves them out unless
// asked to follow them). A directory it cannot read is left out, as rg leaves
// one out; the root it cannot read is the failure. What it keeps goes into l,
// which hands it over before each open or read of a directory — the calls a
// stalled disk holds — and whose context each of those checks first.
func (s atFileSearcher) walk(l *atListing) atFilesRun {
	run := atFilesRun{walk: true}
	queue := []string{""}
	entries := 0
	for len(queue) > 0 && l.flush() {
		rel := queue[0]
		queue = queue[1:]
		d, err := s.openDir(filepath.Join(l.root, rel))
		if err != nil {
			if rel == "" {
				run.err = err
				return run
			}
			continue
		}
		queue = s.walkDir(l, d, rel, &entries, queue)
		_ = d.Close()
		if l.full || l.ctx.Err() != nil {
			break
		}
		if entries >= atFilesWalkMax {
			// The cap: whatever is left unread — of this directory, or in
			// the queue — is not listed. A tree of exactly this many entries
			// reads as cut too; telling the two apart would read one more.
			run.capped = true
			break
		}
	}
	l.flush()
	run.full = l.full
	return run
}

// walkDir reads one directory of the walk, rel its path under the root ("" or
// ending in "/"), in batches, until it ends, fails, the walk's entries reach
// their cap or l says to stop: its files into l, its directories onto queue.
func (s atFileSearcher) walkDir(l *atListing, d atDir, rel string, entries *int, queue []string) []string {
	for *entries < atFilesWalkMax && l.flush() {
		ents, err := d.ReadDir(min(atFilesWalkBatch, atFilesWalkMax-*entries))
		*entries += len(ents)
		if s.walked != nil {
			s.walked(*entries)
		}
		for _, e := range ents {
			name := e.Name()
			switch {
			case name == ".git" || !atTextOK(name):
				// A repository's own (a directory, or a submodule's file),
				// and a name no token can carry — nor anything under it.
			case e.IsDir():
				if name != "node_modules" {
					queue = append(queue, rel+name+"/")
				}
			case e.Type().IsRegular():
				if !l.add(rel + name) {
					return queue
				}
			}
		}
		if err != nil || len(ents) == 0 {
			// io.EOF, or a failure: either way this directory is done, with
			// what it gave.
			return queue
		}
	}
	return queue
}

// ------------------------------------------------------------ the listing

// atListing is a listing as it is read, on the listing's goroutine: the
// files a tool or the walk lists, each kept with every directory above it
// not yet met, and handed to the search (emit) a batch at a time. It holds
// every directory it has met — offered, or refused (git's) — so that a
// directory is derived once however many files are under it, and nothing
// under a refused one is looked at again. What it holds, files and
// directories, is counted against atFilesHoldMax and atFilesBytesMax: it is
// full, and stops, where another path would take it past either.
//
// git's listing is checked on disk (lstat, nil for rg's and the walk's), so
// that it offers what rg and the walk would — neither lists a symbolic link,
// nor anything reached through one — and nothing that leads outside the
// workspace: git lists what it tracks, whatever that has become, and a
// tracked link (or a tracked directory since replaced by a link) passes
// every lexical test atFilePath makes. A file is kept only if it is a
// regular file there (isFile; a link, a submodule or a file since deleted is
// not), and a directory only if it is a directory there (isDir; a link to
// one is not) — each directory stat'ed once, the first time a file under it
// is kept.
type atListing struct {
	ctx   context.Context
	root  string
	emit  func(batch []string) bool
	lstat func(name string) (os.FileInfo, error)

	dirs  map[string]bool
	batch []string
	held  int
	bytes int
	files int
	full  bool
}

// newAtListing is a listing of root, into emit, under ctx; lstat, when it is
// git's, is how its paths are checked on disk (os.Lstat in production).
func newAtListing(ctx context.Context, root string, emit func(batch []string) bool, lstat func(name string) (os.FileInfo, error)) *atListing {
	return &atListing{ctx: ctx, root: root, emit: emit, lstat: lstat, dirs: map[string]bool{}}
}

// add keeps the file at p — a clean relative path — with every directory
// above it not yet met, unless git's check refuses it (the listing goes on
// without it), and says whether the listing goes on: false once its context
// has ended, or once p would take what it holds past a cap (full, and p not
// kept).
//
// Its work on a path is bounded, and it checks the listing's context before
// each, so a listing stops within one path's work of its search's answer.
// Past one map lookup of p's parent (and git's stat of p), each directory it
// looks at is one it has not met, and it stops looking once those would take
// it past a cap: their bytes, which it hashes, are what the caps bound. It
// holds each once p is kept or refused below it — all but those above a
// directory git's check refuses, which it looks at (and stats) again for the
// next file under them — and a path that reaches it past git's stat of its
// file is at most PATH_MAX long (a longer one cannot be stat'ed).
func (l *atListing) add(p string) bool {
	if l.ctx.Err() != nil {
		return false
	}
	if l.lstat != nil && !l.isFile(p) {
		return true
	}
	// The directories above p not yet met, deepest first: once one is met,
	// so is every one above it. top is where the shallowest of them ends —
	// len(p) when there are none.
	top, held, bytes := len(p), 1, len(p)
	for i := strings.LastIndexByte(p, '/'); i > 0; i = strings.LastIndexByte(p[:i], '/') {
		offered, met := l.dirs[p[:i+1]]
		if met {
			if !offered {
				// Under a directory git's listing refused.
				return true
			}
			break
		}
		top, held, bytes = i, held+1, bytes+i+1
		if l.held+held > atFilesHoldMax || l.bytes+bytes > atFilesBytesMax {
			return l.overflow(p)
		}
	}
	if l.held+held > atFilesHoldMax || l.bytes+bytes > atFilesBytesMax {
		return l.overflow(p)
	}
	if l.lstat != nil {
		// Top down, each directory not yet met: the first that is not a
		// directory here refuses p, and is held refused with every one under
		// it on p's path. Those above it are real, and wait for a file of
		// their own to bring them: none is offered with no file under it.
		for j := top; j >= 0 && j < len(p); j = atNextSlash(p, j) {
			if l.isDir(p[:j+1]) {
				continue
			}
			for ; j >= 0; j = atNextSlash(p, j) {
				l.dirs[p[:j+1]] = false
				l.held++
				l.bytes += j + 1
			}
			return true
		}
	}
	for j := top; j >= 0 && j < len(p); j = atNextSlash(p, j) {
		l.dirs[p[:j+1]] = true
		l.batch = append(l.batch, p[:j+1])
	}
	l.batch = append(l.batch, p)
	l.held, l.bytes, l.files = l.held+held, l.bytes+bytes, l.files+1
	if len(l.batch) >= atFilesBatch {
		return l.flush()
	}
	return true
}

// overflow is add's answer for a path that would take what the listing holds
// past a cap: the listing is full, and stops — unless no listing could hold
// the path, its directories' bytes alone past atFilesBytesMax (a path
// thousands of directories deep), and it is dropped as a record too long is,
// the listing going on without it.
func (l *atListing) overflow(p string) bool {
	held, bytes := 1, len(p)
	for i := strings.IndexByte(p, '/'); i >= 0; i = atNextSlash(p, i) {
		held, bytes = held+1, bytes+i+1
	}
	if held > atFilesHoldMax || bytes > atFilesBytesMax {
		return true
	}
	l.full = true
	return false
}

// flush hands the search what the listing has kept since it last did, and
// says whether the search still takes it: false once the listing's context
// has ended.
func (l *atListing) flush() bool {
	if len(l.batch) == 0 {
		return l.ctx.Err() == nil
	}
	batch := l.batch
	l.batch = nil
	return l.emit(batch)
}

// atNextSlash is the index of the first "/" in p after byte j, or -1.
func atNextSlash(p string, j int) int {
	if k := strings.IndexByte(p[j+1:], '/'); k >= 0 {
		return j + 1 + k
	}
	return -1
}

// isFile and isDir say what the path p under the root is on disk, itself — a
// symbolic link is neither, whatever it points at (lstat): a regular file,
// and a directory. Anything that cannot be stat'ed is neither.
func (l *atListing) isFile(p string) bool {
	fi, err := l.lstat(filepath.Join(l.root, p))
	return err == nil && fi.Mode().IsRegular()
}

func (l *atListing) isDir(p string) bool {
	fi, err := l.lstat(filepath.Join(l.root, p))
	return err == nil && fi.IsDir()
}

// -------------------------------------------------------------- the index

// atFileIndex is a search's result in the form every query is matched over:
// the candidates — each file's path, and each directory derived from them,
// ending in "/" — in their base order (shorter first, then by bytes), which
// breaks every tie in a ranking; each with its case-folded form (atFold, the
// same bytes long), where its last segment starts, and the set of bytes its
// fold holds (atMask). title says why the list is not the workspace's whole,
// when it is not. It is built off the Update (atBuild), and only read after.
type atFileIndex struct {
	names []string
	folds []string
	bases []int32
	masks []uint64
	title string
}

// atBuild is an index as the search builds it, a batch of candidates at a
// time as the listing hands them over (atListing: each file kept, and each
// directory its path brought the first time one did — `a/b/c.go` brings
// `a/` and `a/b/`, and a directory with no file under it is in no path, and
// is not offered, §3.16): each folded, its last segment found and its mask
// taken as it comes, in the index's own columns but in the order they came.
// What is left when the listing ends, or the search stops taking it, is
// their order (index). (Building each batch as it comes, while the listing
// reads on, leaves the least for after: the order of candidates already
// built — some 16 ms at atFilesHoldMax — against 19 ms to sort and then build
// them, C17r's measure.)
type atBuild struct {
	names, folds []string
	masks        []uint64
	bases        []int32
}

func (b *atBuild) add(batch []string) {
	for _, n := range batch {
		f := atFold(n)
		b.names = append(b.names, n)
		b.folds = append(b.folds, f)
		b.masks = append(b.masks, atMask(f))
		b.bases = append(b.bases, int32(atBase(n)))
	}
}

// size is how many candidates have been built.
func (b *atBuild) size() int { return len(b.names) }

// index is the candidates built, in the base order — a file listed twice (git
// lists one with conflicts once per stage) merged into one — and how many of
// them are files. It is the one piece of the build left when the search stops
// taking batches, and checks nothing: bounded by what a listing may hold
// (atFilesHoldMax), it orders what the search built by its deadline.
func (b *atBuild) index() (*atFileIndex, int) {
	order := make([]int32, len(b.names))
	for i := range order {
		order[i] = int32(i)
	}
	slices.SortFunc(order, func(p, q int32) int {
		a, c := b.names[p], b.names[q]
		if len(a) != len(c) {
			return len(a) - len(c)
		}
		return strings.Compare(a, c)
	})
	n := len(order)
	x := &atFileIndex{
		names: make([]string, 0, n), folds: make([]string, 0, n), bases: make([]int32, 0, n), masks: make([]uint64, 0, n),
	}
	files := 0
	for k, i := range order {
		name := b.names[i]
		if k > 0 && name == x.names[len(x.names)-1] {
			continue
		}
		x.names = append(x.names, name)
		x.folds = append(x.folds, b.folds[i])
		x.bases = append(x.bases, b.bases[i])
		x.masks = append(x.masks, b.masks[i])
		if !strings.HasSuffix(name, "/") {
			files++
		}
	}
	return x, files
}

// atMask is the set of bytes s holds, as bits: a letter or a digit its own
// bit, any other byte one of the rest by its value. A query whose mask is not
// inside a path's has a byte the path lacks, so it cannot be a subsequence of
// it — most paths a query does not match are refused on one comparison,
// before a byte of them is read. (Two bytes sharing a bit only let a path
// through to the full test.)
func atMask(s string) uint64 {
	var m uint64
	for i := 0; i < len(s); i++ {
		m |= atByteBit(s[i])
	}
	return m
}

func atByteBit(c byte) uint64 {
	switch {
	case 'a' <= c && c <= 'z':
		return 1 << (c - 'a')
	case '0' <= c && c <= '9':
		return 1 << (26 + c - '0')
	}
	return 1 << (36 + c%28)
}

// atBase is where name's last segment starts: past its last "/" — for a
// directory, the last but its closing one.
func atBase(name string) int {
	return strings.LastIndexByte(strings.TrimSuffix(name, "/"), '/') + 1
}

// atFold is s case-folded rune by rune (unicode.ToLower), keeping any rune
// whose lower case is not as many bytes long as itself as it is, so that
// every byte offset into the fold is the same offset into s: the match finds
// its positions in the fold and scores them in s. ASCII with no upper case —
// most paths — comes back as it is.
func atFold(s string) string {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c >= utf8.RuneSelf || ('A' <= c && c <= 'Z') {
			return atFoldRunes(s)
		}
	}
	return s
}

func atFoldRunes(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if l := unicode.ToLower(r); utf8.RuneLen(l) == utf8.RuneLen(r) {
			r = l
		}
		b.WriteRune(r)
	}
	return b.String()
}

// ----------------------------------------------------------- the matching

// The match's scores: fzf's shape (its algo.go), simplified. Each query rune
// matched scores atScoreMatch; one at a segment start scores atScoreBoundary
// more — after `/`, `_`, `-`, `.` or a space, or the path's first — or
// atScoreCamel at a lower-to-upper case change (`fooBar`), doubled for the
// query's first rune; a rune right after the one before it scores
// atScoreConsec more; a gap between two costs atScoreGapStart and
// atScoreGapExtend a byte after its first; a rune in the last segment scores
// atScoreInBase more, so that of two otherwise equal matches the one in a
// file's name wins over one in its directories. A last segment that starts
// with the whole query scores atScoreBasePrefix above all of that (§3.16's
// basename-prefix bonus): what is typed as a name's start is ranked first.
const (
	atScoreMatch      = 16
	atScoreBoundary   = 8
	atScoreCamel      = 7
	atScoreConsec     = 4
	atScoreGapStart   = 3
	atScoreGapExtend  = 1
	atScoreInBase     = 2
	atScoreBasePrefix = 1000
)

// match is the query's candidates: the best k by score, then by base order
// (the shorter path, then its bytes), and how many more matched. A leading
// "./" is taken off the query; a "/" in it makes what is up to its last "/" a
// directory prefix — folded as the paths are — that every candidate's path
// starts with and is longer than (the directory itself is not in it), the
// rest matched within what follows the prefix. An empty rest matches
// everything, all scored alike: the base order alone, the shortest — the
// nearest the root — first.
func (x *atFileIndex) match(query string, k int) (items []completeItem, more int) {
	q := query
	for strings.HasPrefix(q, "./") {
		q = q[2:]
	}
	prefix, rest := "", q
	if i := strings.LastIndexByte(q, '/'); i >= 0 {
		prefix, rest = atFold(q[:i+1]), q[i+1:]
	}
	m := atCompile(atFold(rest))
	qmask := atMask(m.fold)
	top := atTop{k: k}
	total := 0
	for i, f := range x.folds {
		if x.masks[i]&qmask != qmask {
			continue
		}
		from := 0
		if prefix != "" {
			if len(f) <= len(prefix) || !strings.HasPrefix(f, prefix) {
				continue
			}
			from = len(prefix)
		}
		if len(m.units) == 0 {
			total++
			top.offer(atRank{idx: int32(i)})
			continue
		}
		run, ok := m.find(f, from)
		if !ok {
			continue
		}
		total++
		base := int(x.bases[i])
		prefixed := strings.HasPrefix(f[base:], m.fold)
		if top.full() && m.bound(prefixed) <= int(top.h[0].score) {
			// It cannot rank: the most it could score is no better than the
			// lowest kept, which is earlier in the base order. Counted, and
			// not scored — which is exact, and spares most matches of a
			// short query the alignments (the kept are soon all names
			// starting with it).
			continue
		}
		top.offer(atRank{score: int32(m.rate(x.names[i], f, base, from, run, prefixed)), idx: int32(i)})
	}
	ranked := top.ranked()
	items = make([]completeItem, len(ranked))
	for i, r := range ranked {
		n := x.names[r.idx]
		items[i] = completeItem{Name: n, Value: n, Insert: n, Openable: strings.HasSuffix(n, "/")}
	}
	return items, total - len(items)
}

// atMatcher is a query compiled for matching: its fold, its runes as the
// byte strings they are in the fold, and room for three alignments' positions
// (score's), reused across every path.
type atMatcher struct {
	fold       string
	units      []string
	fw, bw, bs []int
	// most is the most rate can give a match of the query without the
	// prefix bonus (bound).
	most int
}

func atCompile(fold string) atMatcher {
	m := atMatcher{fold: fold}
	for i := 0; i < len(fold); {
		_, n := utf8.DecodeRuneInString(fold[i:])
		m.units = append(m.units, fold[i:i+n])
		i += n
	}
	m.fw, m.bw, m.bs = make([]int, len(m.units)), make([]int, len(m.units)), make([]int, len(m.units))
	if n := len(m.units); n > 0 {
		// Every rune matched, at a segment start and in the last segment;
		// every one after the first in a run; the first's boundary doubled.
		bonus := max(atScoreBoundary, atScoreCamel)
		m.most = n*(atScoreMatch+bonus+atScoreInBase) + (n-1)*atScoreConsec + bonus
	}
	return m
}

// bound is the most rate can give a match of the query: most, and the prefix
// bonus when its last segment starts with the query. No gap is counted, so
// no alignment scores above it.
func (m *atMatcher) bound(prefixed bool) int {
	if prefixed {
		return m.most + atScoreBasePrefix
	}
	return m.most
}

// atIndex and atLastIndex find a query rune's bytes in a fold: a single byte
// by the vectorised byte search (measured faster than a plain loop even over
// paths this short). A multi-byte rune found by its bytes is found at a
// rune's start — UTF-8 never holds a whole rune's encoding inside another's.
func atIndex(s, u string) int {
	if len(u) == 1 {
		return strings.IndexByte(s, u[0])
	}
	return strings.Index(s, u)
}

func atLastIndex(s, u string) int {
	if len(u) == 1 {
		return strings.LastIndexByte(s, u[0])
	}
	return strings.LastIndex(s, u)
}

// find says whether the query matches in fold from byte from — its runes a
// subsequence there — leaving the forward alignment in fw (each rune at its
// first place after the one before), and run when that alignment is one
// unbroken run.
func (m *atMatcher) find(fold string, from int) (run, ok bool) {
	pos, run := from, true
	for i, u := range m.units {
		j := atIndex(fold[pos:], u)
		if j < 0 {
			return false, false
		}
		run = run && (i == 0 || j == 0)
		m.fw[i] = pos + j
		pos = m.fw[i] + len(u)
	}
	return run, true
}

// rate is how well the query matches the path name (fold, its fold; base,
// where its last segment starts) inside it from byte from, once find has
// matched it (fw, run) — prefixed saying its last segment starts with the
// query. A match can be aligned many ways and a score is an alignment's;
// three cheap ones are scored and the best kept, in place of fzf's full
// search over every alignment:
//
//   - forward (find's): each rune at its first place after the one before —
//     the alignment that finds segment starts early in the path (`sl` in
//     `sessions_list.go`);
//   - tightened: the forward alignment's last rune, and each before it at its
//     last place before the next — the shortest window ending there, which
//     finds a run the forward one broke up (`tui` in `internal/tui/`, where
//     the forward one takes the `t` of `internal`); one run already is its
//     own;
//   - in the last segment: forward from the last segment's start, when the
//     query can fit there — a match in the file's own name.
func (m *atMatcher) rate(name, fold string, base, from int, run, prefixed bool) int {
	best := m.eval(name, base, m.fw)

	if last := len(m.units) - 1; last > 0 && !run {
		lim, ok := m.fw[last]+len(m.units[last]), true
		for i := last; i >= 0 && ok; i-- {
			j := atLastIndex(fold[from:lim], m.units[i])
			ok = j >= 0
			m.bw[i] = from + j
			lim = m.bw[i]
		}
		if ok && !slices.Equal(m.bw, m.fw) {
			best = max(best, m.eval(name, base, m.bw))
		}
	}

	if base > m.fw[0] && len(fold)-base >= len(m.fold) {
		pos, ok := base, true
		for i, u := range m.units {
			j := atIndex(fold[pos:], u)
			if ok = j >= 0; !ok {
				break
			}
			m.bs[i] = pos + j
			pos = m.bs[i] + len(u)
		}
		if ok {
			best = max(best, m.eval(name, base, m.bs))
		}
	}

	if prefixed {
		best += atScoreBasePrefix
	}
	return best
}

// eval scores one alignment: the byte offsets of the query's runes in name.
func (m *atMatcher) eval(name string, base int, pos []int) int {
	score, end := 0, 0
	for i, p := range pos {
		score += atScoreMatch
		b := atBoundary(name, p)
		if i == 0 {
			b *= 2
		} else if p == end {
			score += atScoreConsec
		} else {
			score -= atScoreGapStart + (p-end-1)*atScoreGapExtend
		}
		score += b
		if p >= base {
			score += atScoreInBase
		}
		end = p + len(m.units[i])
	}
	return score
}

// atBoundary is the bonus for a match at byte p of name: a segment's start —
// the name's first byte, or one after `/`, `_`, `-`, `.` or a space — or a
// lower-to-upper case change; nothing elsewhere.
func atBoundary(name string, p int) int {
	if p == 0 {
		return atScoreBoundary
	}
	prev, cur := rune(name[p-1]), rune(name[p])
	if prev >= utf8.RuneSelf {
		prev, _ = utf8.DecodeLastRuneInString(name[:p])
	}
	switch prev {
	case '/', '_', '-', '.', ' ':
		return atScoreBoundary
	}
	if cur >= utf8.RuneSelf {
		cur, _ = utf8.DecodeRuneInString(name[p:])
	}
	if unicode.IsLower(prev) && unicode.IsUpper(cur) {
		return atScoreCamel
	}
	return 0
}

// atRank is a match: its score and its place in the base order.
type atRank struct{ score, idx int32 }

// below says r ranks below o: a lower score, or the same score later in the
// base order.
func (r atRank) below(o atRank) bool {
	return r.score < o.score || (r.score == o.score && r.idx > o.idx)
}

// atTop keeps the best k matches offered to it: a heap with the lowest
// ranked of them at its root, which a better match replaces. Matching tens of
// thousands of paths costs a comparison with the root for each, and never a
// sort of them all.
type atTop struct {
	k int
	h []atRank
}

// full says k matches are kept: a match that is not better than the lowest
// of them (h[0]) is not kept.
func (t *atTop) full() bool { return t.k > 0 && len(t.h) == t.k }

func (t *atTop) offer(r atRank) {
	if t.k <= 0 {
		return
	}
	if len(t.h) < t.k {
		t.h = append(t.h, r)
		for i := len(t.h) - 1; i > 0; {
			p := (i - 1) / 2
			if !t.h[i].below(t.h[p]) {
				break
			}
			t.h[i], t.h[p] = t.h[p], t.h[i]
			i = p
		}
		return
	}
	if !t.h[0].below(r) {
		return
	}
	t.h[0] = r
	for i, n := 0, len(t.h); ; {
		c := 2*i + 1
		if c >= n {
			break
		}
		if c+1 < n && t.h[c+1].below(t.h[c]) {
			c++
		}
		if !t.h[c].below(t.h[i]) {
			break
		}
		t.h[i], t.h[c] = t.h[c], t.h[i]
		i = c
	}
}

// ranked is the kept matches, best first.
func (t *atTop) ranked() []atRank {
	slices.SortFunc(t.h, func(a, b atRank) int {
		switch {
		case b.below(a):
			return -1
		case a.below(b):
			return 1
		}
		return 0
	})
	return t.h
}
