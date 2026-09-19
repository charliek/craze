package opencode

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/charliek/craze/internal/harness/tool"
)

// grep's and glob's limits (plan 019 §3.2, §3.9).
const (
	// searchLimit is how many matches grep returns and how many files glob
	// does: opencode's (grep.ts:67, glob.ts:48).
	searchLimit = 100
	// searchTimeout is how long rg may run. opencode has none; craze stops
	// rg after it (NOTICE).
	searchTimeout = 20 * time.Second
	// maxRecordBytes is the longest line of rg's output read: opencode's
	// MAX_RECORD_BYTES (core/src/ripgrep.ts:19), past which a grep fails.
	maxRecordBytes = 64 * 1024
	// stderrBytes is how much of rg's stderr is kept for an error message:
	// opencode's ERROR_BYTES (ripgrep.ts:18).
	stderrBytes = 8 * 1024
)

// rgMissingText is grep's and glob's result when ripgrep is not on PATH
// (plan 019 §3.9). craze downloads nothing and walks no tree itself (owner
// decision 3); the model is pointed at bash instead.
const rgMissingText = "ripgrep (rg) is not installed or not on PATH. Use the bash tool with grep or find instead."

// The flags both tools pass rg (ripgrep.ts:154-231): no config file, so a
// user's RIPGREP_CONFIG_PATH cannot change what the model sees, and never a
// .git directory, even where --hidden would reach one.
const (
	rgNoConfig = "--no-config"
	rgNoGit    = "--glob=!**/.git/**"
)

// ripgrep is rg as one session's grep and glob run it. The two share one,
// built by Profile, so rg is looked for on PATH once per session, when a
// search first needs it (plan 019 §3.9): rg installed mid-session is found
// by the next session.
type ripgrep struct {
	look    func(file string) (string, error) // exec.LookPath, or a test's own
	timeout time.Duration

	once sync.Once
	bin  string
	err  error
}

func newRipgrep() *ripgrep { return &ripgrep{look: exec.LookPath, timeout: searchTimeout} }

// find returns rg's path, looking for it the first time only.
func (r *ripgrep) find() (string, error) {
	r.once.Do(func() { r.bin, r.err = r.look("rg") })
	return r.bin, r.err
}

// rgMissing is the result for a search with no rg to run.
func rgMissing() tool.Result {
	return tool.Result{Text: rgMissingText, IsError: true, Class: tool.ClassToolError}
}

// errRecordTooLong is a line of rg's output longer than maxRecordBytes, in
// opencode's words (ripgrep.ts:234). Only grep's JSON records reach it: a
// path glob prints is at most PATH_MAX, 4096 bytes.
var errRecordTooLong = fail(tool.ClassToolError, fmt.Sprintf("Ripgrep JSON record exceeded %d bytes", maxRecordBytes))

// errEnough is the cause run cancels rg's context with once it has read all
// it wants: the lines it asked for, or a line it could not use.
var errEnough = errors.New("opencode: no more ripgrep output is wanted")

// rgEnd is how a run of rg ended.
type rgEnd struct {
	why ending
	// stopped says run stopped reading before rg's output ended — each had
	// enough, or readErr — and stopped rg.
	stopped bool
	// code is rg's exit code, or -1 when rg did not exit by itself or was
	// never reaped.
	code int
	// stderr is the start of what rg wrote to stderr.
	stderr string
}

// run runs `bin args...` in dir — in a session of its own, as bash runs a
// command, with stdin /dev/null and the environment bash gives a command —
// and hands each record of its stdout, without the sep that ends it, to
// each, until each returns false or an error: a line of grep's JSON, ended
// by "\n", or a path in glob's --null listing, ended by NUL. An empty
// record is skipped, as opencode skips an empty line. Once each wants no more, rg's group is sent SIGTERM (and
// SIGKILL 3 s later if it lingers), as opencode ends rg once it has taken
// the lines it wants; what each has already taken is kept. readErr is why
// reading stopped early, when it was not each's choice: a record longer than
// maxRecordBytes, or each's error. err is set only when rg could not be
// started.
//
// rg is stopped as bash stops a command (supervise): a timeout or a cancel
// sends its group SIGTERM, a close SIGKILL at once, and whatever is left in
// the group once rg has exited is killed too. So run returns promptly after
// ctx is done or the timeout passes. The records it reads are bounded too:
// at most maxRecordBytes each, and none once each has had enough.
func (r *ripgrep) run(ctx context.Context, env tool.Env, bin, dir string, args []string, sep byte, each func(rec []byte) (more bool, err error)) (end rgEnd, readErr, err error) {
	outR, outW, err := os.Pipe()
	if err != nil {
		return rgEnd{}, nil, err
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		_, _ = outR.Close(), outW.Close()
		return rgEnd{}, nil, err
	}
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Env = environ(env, dir)
	cmd.Stdout, cmd.Stderr = outW, errW
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	g, err := startGroup(cmd)
	// rg has its own copies of the write ends; the pipes close when it exits.
	_, _ = outW.Close(), errW.Close()
	if err != nil {
		_, _ = outR.Close(), errR.Close()
		return rgEnd{}, nil, err
	}

	runCtx, stop := context.WithCancelCause(ctx)
	defer stop(nil)
	stdout, stderr := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stdout)
		if end.stopped, readErr = readRecords(outR, sep, each); end.stopped {
			stop(errEnough)
		}
	}()
	errText := &headBuffer{max: stderrBytes}
	go func() {
		defer close(stderr)
		_, _ = io.Copy(errText, errR)
	}()
	output := make(chan struct{})
	go func() {
		<-stdout
		<-stderr
		close(output)
	}()

	why, reaped := g.supervise(runCtx, env.Closing, r.timeout, output)
	// As bash does: read what is already in the pipes, then close them, since
	// something that left the group could hold them open for ever.
	if !tool.SessionClosing(ctx, env.Closing) {
		select {
		case <-output:
		case <-env.Closing:
		case <-time.After(flushWait):
		}
	}
	_, _ = outR.Close(), errR.Close()
	<-output // at once: a read of a closed pipe returns

	end.why, end.code, end.stderr = why, -1, string(errText.b)
	if reaped && !end.stopped && why == endExit {
		end.code = exitCode(g.state)
	}
	return end, readErr, nil
}

// readRecords reads r a record at a time, each ended by sep, and hands each
// non-empty record, less its sep, to each. It returns when r ends or fails,
// and stopped when it stopped first: each asked for no more or failed, or a
// record was longer than maxRecordBytes (errRecordTooLong). A last record
// with no sep counts as a record.
func readRecords(r io.Reader, sep byte, each func([]byte) (bool, error)) (stopped bool, err error) {
	br := bufio.NewReaderSize(r, maxRecordBytes+1)
	for {
		rec, rerr := br.ReadSlice(sep)
		if rerr == bufio.ErrBufferFull {
			return true, errRecordTooLong
		}
		rec = bytes.TrimSuffix(rec, []byte{sep})
		if len(rec) > maxRecordBytes {
			return true, errRecordTooLong
		}
		if len(rec) > 0 {
			more, err := each(rec)
			if err != nil {
				return true, err
			}
			if !more {
				return true, nil
			}
		}
		if rerr != nil {
			// The end, or a pipe closed once rg was stopped: either way there
			// is nothing more to read, and the caller knows why.
			return false, nil
		}
	}
}

// headBuffer keeps the first max bytes written to it and drops the rest,
// never failing, so a writer is never blocked by it.
type headBuffer struct {
	max int
	b   []byte
}

func (h *headBuffer) Write(p []byte) (int, error) {
	if room := h.max - len(h.b); room > 0 {
		h.b = append(h.b, p[:min(room, len(p))]...)
	}
	return len(p), nil
}

// searchOutcome is the result for how a search's run of rg ended, when that
// is not the tool's own to phrase: ok is false when the result is a
// failure, and the tool reports res; ok is true when rg's lines are the
// answer, and partial rows (exit 2) are too, as in opencode.
//
// The order matters: a cancel is aborted, whatever else happened; a timeout
// is a timeout, even if rg's last line was cut short by the kill; a line
// run could not use fails the call; a search stopped for having enough
// lines is an answer; and then rg's exit code decides, as opencode's
// ripgrep.ts:130-141 does. Exit 1 is no match, not an error. Exit 2 with an
// invalid pattern is rg's message as an error; any other exit 2 is a search
// that could not read something, and its rows stand.
func searchOutcome(ctx context.Context, name string, end rgEnd, readErr, runErr error, timeout time.Duration) (res tool.Result, ok bool) {
	switch {
	case ctx.Err() != nil:
		return tool.Result{Text: tool.AbortedText, IsError: true, Class: tool.ClassAborted}, false
	case runErr != nil:
		// opencode's words for any failure to run rg (ripgrep.ts:149), with
		// the cause, which opencode keeps out of the message.
		return tool.Result{Text: "ripgrep execution failed: " + runErr.Error(), IsError: true, Class: tool.ClassToolError}, false
	case end.why == endTimeout:
		return tool.Result{Text: fmt.Sprintf("%s tool terminated ripgrep after exceeding timeout %d ms. Consider using a more specific path or pattern.",
			name, timeout.Milliseconds()), IsError: true, Class: tool.ClassTimeout}, false
	case readErr != nil:
		return errorResult(readErr), false
	case end.stopped, end.code == 0, end.code == 1:
		return tool.Result{}, true
	case end.code == 2 && invalidPattern(end.stderr):
		return tool.Result{Text: strings.TrimSpace(end.stderr), IsError: true, Class: tool.ClassToolError}, false
	case end.code == 2:
		return tool.Result{}, true
	}
	text := strings.TrimSpace(end.stderr)
	if text == "" {
		text = fmt.Sprintf("ripgrep failed with code %d", end.code)
	}
	return tool.Result{Text: text, IsError: true, Class: tool.ClassToolError}, false
}

// invalidPattern reports whether rg's stderr says the search's pattern is
// invalid: opencode's two regex messages (ripgrep.ts:89-90), and rg's
// message for a glob it cannot parse, which opencode would read as "No
// files found" (NOTICE).
func invalidPattern(stderr string) bool {
	return strings.Contains(stderr, "regex parse error") ||
		strings.Contains(stderr, "error parsing regex") ||
		strings.Contains(stderr, "error parsing glob")
}

// rgRelative is a path rg printed, relative to the directory it searched,
// as opencode cleans one (ripgrep.ts:171-174, 256-259): leading "./" and
// "/" removed. opencode also turns "\" into "/", for Windows; craze keeps
// it, since on Linux and macOS it is part of the name.
func rgRelative(p string) string {
	for strings.HasPrefix(p, "./") {
		p = p[2:]
	}
	return strings.TrimLeft(p, "/")
}

// rgPath is a path rg printed, cleaned (rgRelative) and joined onto root,
// the directory rg searched from; its error is the search's when the result
// is not strictly under root. rg searching "." (or a file it was given by
// name in root) prints nothing else, so such a path means rg's output was
// misread or is not rg's, and none of it is to be trusted: the search fails
// rather than drop the path, which would hide that. A name is one path
// element however odd: "x\n.." names a directory, not "..", so it stays
// under root.
func rgPath(root, printed string) (string, error) {
	p := filepath.Join(root, rgRelative(printed))
	rel, err := filepath.Rel(root, p)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", fail(tool.ClassToolError, fmt.Sprintf("Invalid ripgrep output: %q is not a path under %s", printed, root))
	}
	return p, nil
}

// shownPath is a path as a search's answer shows it: as it is or, when it
// holds a control character, Go-quoted. An answer shows one path, or one
// file's header, a line, so a name with a newline in it would otherwise
// read as two paths: "x\n../secret" as "x" and a "../secret" outside the
// search.
func shownPath(p string) string {
	if strings.ContainsFunc(p, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return strconv.Quote(p)
	}
	return p
}
