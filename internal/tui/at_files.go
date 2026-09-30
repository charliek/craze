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
// follow-up). It is completePopup's source (complete.go); the composer wires
// it in C18.
//
// Two halves:
//
//   - The search: one listing of the workspace per popup opening, off the
//     Update — the popup's keyed load (X122), keyed by the workspace, so every
//     keystroke after the first matches over what it brought back and nothing
//     is listed twice. `rg --files` when rg is on PATH; else `git ls-files`
//     in a git work tree; else a walk of the tree. Each is bounded — at most
//     atFilesMax paths, atFilesTimeout, the walk atFilesWalkMax entries — and
//     stops when the popup stops awaiting it (it closed, its workspace or the
//     session shown changed: X125): the tool is killed and reaped. Only files
//     are listed; the directories offered are derived from their paths, so an
//     empty directory is never offered.
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
	// atFilesWalkMax bounds the walk craze does itself when neither tool can
	// list the tree (§3.16): the directory entries it reads, all directories
	// together, atFilesWalkBatch at a time with the load's context checked
	// between batches — X137's listing, over a tree.
	atFilesWalkMax   = 20000
	atFilesWalkBatch = 256
	// atFilesTimeout is how long one search may take, tool and walk together
	// (§3.16): past it the search stops, and what it read is offered.
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
)

// atFilesCapTitle, atFilesWalkCapTitle and atFilesTimeoutTitle are the title
// of a list that is not the workspace's whole: drawn in the rule above the
// candidates, so that a file the list does not hold is not taken for one the
// workspace does not.
var (
	atFilesCapTitle     = "only the first " + atGroupDigits(atFilesMax) + " files"
	atFilesWalkCapTitle = "only the first " + atGroupDigits(atFilesWalkMax) + " entries"
	atFilesTimeoutTitle = "listing stopped after " + atFilesTimeout.String()
)

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
// note; one that read something is offered, with a title when it is partial.
func (s atFileSource) complete(q completeQuery) completeAnswer {
	root := q.Workspace
	if root == "" {
		// The composer offers no `@` popup without a workspace (§3.16).
		return completeAnswer{Note: atFilesNoRootNote}
	}
	l, back := q.Loaded(root)
	if !back {
		search := s.search
		return completeAnswer{Load: &completeLoad{Key: root, Run: func(ctx context.Context) completeLoaded {
			return search(ctx, root)
		}}}
	}
	if l.Err != nil {
		return completeAnswer{Note: atFilesErrNote(l.Err), NoteErr: true}
	}
	x, _ := l.Data.(*atFileIndex)
	if x == nil || len(x.names) == 0 {
		ans := completeAnswer{Note: atFilesNoneNote}
		if x != nil {
			ans.Title = x.title
		}
		return ans
	}
	items, more := x.match(q.Text, atFilesRankMax)
	return completeAnswer{Title: x.title, Items: items, More: more}
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
// looked for on PATH, whether a directory is in a git work tree, and the
// timer — each the real one in production (newAtFileSearcher), and a test's
// own to force every fallback and fire the timeout without waiting for it.
type atFileSearcher struct {
	look    func(file string) (string, error)
	gitTree func(root string) bool
	after   func(d time.Duration, f func()) (stop func() bool)
	timeout time.Duration

	// Test hooks, nil in production: started is told a tool's pid once it
	// has started, read how many records it has read after each one, walked
	// how many entries the walk has read after each batch.
	started func(pid int)
	read    func(records int)
	walked  func(entries int)
}

// newAtFileSearcher is the searcher as craze runs it: tools found on PATH, a
// work tree found by its `.git` (discoverGit, the status row's search), the
// real clock.
func newAtFileSearcher() atFileSearcher {
	return atFileSearcher{
		look:    exec.LookPath,
		gitTree: func(root string) bool { return discoverGit(root).dir != "" },
		after: func(d time.Duration, f func()) func() bool {
			return time.AfterFunc(d, f).Stop
		},
		timeout: atFilesTimeout,
	}
}

// atFilesRun is how one listing went: the paths it read that a token can
// carry, whether it stopped at its cap (and so is partial), whether it was
// the walk (whose cap counts entries, not paths), and why it failed when it
// did — which search offers nothing for only when nothing was read.
type atFilesRun struct {
	paths  []string
	capped bool
	walk   bool
	err    error
}

// search lists root and answers the listing as an index to match over — or,
// when it read nothing, why: the timeout, or the failure. Its context is the
// popup's for the load (cancelled once the load is not awaited: the answer is
// then dropped, and nobody reads it), under the search's own timeout.
func (s atFileSearcher) search(ctx context.Context, root string) completeLoaded {
	sctx, stop := context.WithCancelCause(ctx)
	defer stop(nil)
	disarm := s.after(s.timeout, func() { stop(errAtFilesTimeout) })
	defer disarm()

	run := s.list(sctx, root)
	title := ""
	switch {
	case ctx.Err() != nil:
		return completeLoaded{Err: ctx.Err()}
	case sctx.Err() != nil:
		// The timeout: whatever was read by then is offered, said to be
		// partial; nothing read is the note. (A timer that fires in the
		// instant after a listing ended calls a whole list partial: the words
		// are wrong, the list is not.)
		if len(run.paths) == 0 {
			return completeLoaded{Err: errAtFilesTimeout}
		}
		title = atFilesTimeoutTitle
	case run.err != nil && len(run.paths) == 0:
		return completeLoaded{Err: run.err}
	case run.capped && run.walk:
		title = atFilesWalkCapTitle
	case run.capped:
		title = atFilesCapTitle
	}
	// A failure after some paths were read — rg that could not read one
	// directory of many (its exit 2), git that failed late — leaves what it
	// read standing, as internal/harness's glob keeps rg's rows at exit 2.
	return completeLoaded{Data: newAtFileIndex(run.paths, title)}
}

// list is the search's listing, by the first means there is (§3.16): rg on
// PATH; else git, in a git work tree; else the walk. git that fails having
// listed nothing — the tree is not a repository after all, git distrusts its
// owner (safe.directory), its index is broken — gives way to the walk, which
// needs none of that; rg that fails is the answer, since it walks the same
// directories the walk would.
func (s atFileSearcher) list(ctx context.Context, root string) atFilesRun {
	if bin, err := s.look("rg"); err == nil {
		return s.tool(ctx, root, bin, atFilesRGArgs, true)
	}
	if s.gitTree(root) {
		if bin, err := s.look("git"); err == nil {
			run := s.tool(ctx, root, bin, atFilesGitArgs, false)
			if run.err == nil || len(run.paths) > 0 || ctx.Err() != nil {
				return run
			}
		}
	}
	return s.walk(ctx, root)
}

// tool runs bin with args in root and reads its NUL-separated listing
// (readPaths).
//
// It runs with root as its directory, with no shell and stdin /dev/null,
// under a context of its own inside the search's: that context ending — the
// popup no longer awaiting the load, the timeout, or the listing reaching its
// cap — kills the tool (exec's own cancel, SIGKILL: a listing has nothing to
// flush) and closes craze's end of its stdout, so a read waiting on output
// that will never come returns; then the tool is reaped (Wait), its pipes
// bounded by atFilesWaitDelay. So tool returns promptly once its context
// ends, and never leaves a process behind that it started.
//
// rg's exit 1 with nothing listed is no files, not a failure (rg's own
// meaning); any other failure is its stderr's first line, or the exit.
func (s atFileSearcher) tool(ctx context.Context, root, bin string, args []string, rg bool) atFilesRun {
	cctx, kill := context.WithCancel(ctx)
	defer kill()
	cmd := exec.CommandContext(cctx, bin, args...)
	cmd.Dir = root
	stderr := &atHead{max: atFilesStderrMax}
	cmd.Stderr = stderr
	cmd.WaitDelay = atFilesWaitDelay
	out, err := cmd.StdoutPipe()
	if err != nil {
		return atFilesRun{err: err}
	}
	if err := cmd.Start(); err != nil {
		return atFilesRun{err: err}
	}
	if s.started != nil {
		s.started(cmd.Process.Pid)
	}
	// A read of the pipe returns once it is closed (the pipe is pollable),
	// whether or not the kill reached whatever holds its other end.
	unhook := context.AfterFunc(cctx, func() { _ = out.Close() })
	paths, capped, rerr := s.readPaths(out)
	if capped {
		// Enough: the tool is stopped, not waited for.
		kill()
	}
	werr := cmd.Wait()
	unhook()

	run := atFilesRun{paths: paths, capped: capped}
	switch {
	case capped, ctx.Err() != nil:
		// Ended by craze — at the cap, or by the search's context, which
		// search reads for itself — and not by a failure of the tool's.
	case rerr != nil:
		run.err = rerr
	case werr == nil:
	case rg && atExitCode(werr) == 1 && len(paths) == 0:
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
// one with no NUL counts too; an empty one is nothing), and keeps each a
// token can carry (atFilePath). It reads at most atFilesMax records: at the
// next one it stops, capped. A record longer than atFilesRecordMax is
// dropped whole and still counted. err is why reading stopped before the
// listing's end: io.EOF is its end, not an error — and a read of the pipe
// once the tool's context closed it is an error only search's context
// explains.
func (s atFileSearcher) readPaths(r io.Reader) (paths []string, capped bool, err error) {
	br := bufio.NewReaderSize(r, atFilesRecordMax)
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
				return paths, true, nil
			}
			records++
			if p, ok := atFilePath(rec); ok && !wasLong {
				paths = append(paths, p)
			}
			if s.read != nil {
				s.read(records)
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				return paths, false, nil
			}
			return paths, false, rerr
		}
	}
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
// one out; the root it cannot read is the failure.
func (s atFileSearcher) walk(ctx context.Context, root string) atFilesRun {
	run := atFilesRun{walk: true}
	queue := []string{""}
	entries := 0
	for len(queue) > 0 {
		rel := queue[0]
		queue = queue[1:]
		f, err := os.Open(filepath.Join(root, rel))
		if err != nil {
			if rel == "" {
				run.err = err
				return run
			}
			continue
		}
		queue = s.walkDir(ctx, f, rel, &entries, &run.paths, queue)
		_ = f.Close()
		if ctx.Err() != nil {
			return run
		}
		if entries >= atFilesWalkMax {
			// The cap: whatever is left unread — of this directory, or in
			// the queue — is not listed. A tree of exactly this many entries
			// reads as cut too; telling the two apart would read one more.
			run.capped = true
			return run
		}
	}
	return run
}

// walkDir reads one directory of the walk, rel its path under the root ("" or
// ending in "/"), in batches, until it ends, fails or the walk's entries
// reach their cap: its files onto paths, its directories onto queue.
func (s atFileSearcher) walkDir(ctx context.Context, f *os.File, rel string, entries *int, paths *[]string, queue []string) []string {
	for *entries < atFilesWalkMax {
		if ctx.Err() != nil {
			return queue
		}
		ents, err := f.ReadDir(min(atFilesWalkBatch, atFilesWalkMax-*entries))
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
				*paths = append(*paths, rel+name)
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

// -------------------------------------------------------------- the index

// atFileIndex is a search's result in the form every query is matched over:
// the candidates — each file's path, and each directory derived from them,
// ending in "/" — in their base order (shorter first, then by bytes), which
// breaks every tie in a ranking; each with its case-folded form (atFold, the
// same bytes long), where its last segment starts, and the set of bytes its
// fold holds (atMask). title says why the list is not the workspace's whole,
// when it is not. It is built once, off the Update, and only read after.
type atFileIndex struct {
	names []string
	folds []string
	bases []int32
	masks []uint64
	title string
}

// newAtFileIndex is the index of the files at paths (relative, each one a
// token can carry; duplicates allowed — git lists a file with conflicts once
// per stage), with every proper prefix directory of each as a candidate of
// its own: `a/b/c.go` brings `a/` and `a/b/`. A directory with no file under
// it is in no path, and is not offered (§3.16).
func newAtFileIndex(paths []string, title string) *atFileIndex {
	names := make([]string, 0, len(paths)+len(paths)/4)
	names = append(names, paths...)
	dirs := map[string]struct{}{}
	for _, p := range paths {
		// Deepest first: once a directory is known, so is every one above it.
		for i := strings.LastIndexByte(p, '/'); i > 0; i = strings.LastIndexByte(p[:i], '/') {
			d := p[:i+1]
			if _, ok := dirs[d]; ok {
				break
			}
			dirs[d] = struct{}{}
			names = append(names, d)
		}
	}
	slices.SortFunc(names, func(a, b string) int {
		if len(a) != len(b) {
			return len(a) - len(b)
		}
		return strings.Compare(a, b)
	})
	names = slices.Compact(names)
	x := &atFileIndex{
		names: names, folds: make([]string, len(names)), bases: make([]int32, len(names)),
		masks: make([]uint64, len(names)), title: title,
	}
	for i, n := range names {
		x.folds[i] = atFold(n)
		x.bases[i] = int32(atBase(n))
		x.masks[i] = atMask(x.folds[i])
	}
	return x
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
