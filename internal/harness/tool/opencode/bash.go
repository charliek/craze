package opencode

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/charliek/craze/internal/harness/tool"
)

// bash's timings and sizes (plan 019 §3.9).
const (
	// defaultTimeout is opencode's (shell.ts:347).
	defaultTimeout = 2 * time.Minute
	// maxTimeout is craze's cap: a longer timeout is reduced to it, and the
	// result says so (NOTICE).
	maxTimeout = 10 * time.Minute
	// timeoutSlack is how long past its timeout a command is stopped, as in
	// opencode (shell.ts:540).
	timeoutSlack = 100 * time.Millisecond
	// progressInterval is the least time between two progress snapshots
	// (plan 019 §3.5).
	progressInterval = 100 * time.Millisecond
)

// The shells bash runs a command with: bash when it is there, else sh (plan
// 019 §3.9). opencode runs the user's $SHELL when it is an acceptable one.
const (
	bashPath = "/bin/bash"
	shPath   = "/bin/sh"
)

// The texts craze adds to opencode's (NOTICE).
const (
	// noEnvironText refuses a call whose Env has no child environment. The
	// tool cannot tell which variables hold craze's provider keys, so it
	// never falls back to its own environment (plan 019 §3.8).
	noEnvironText = "The bash tool cannot run: no child environment configured."
	// partialText says the output was not read to its end: something the
	// command left running outside its process group still held the pipe
	// when reading stopped.
	partialText = "The output may be incomplete: something the command started still held its output open when the call ended, and what it wrote after that was not read."
)

// host is what bash's description tells the model about the machine, and the
// shell it runs commands with.
type host struct {
	os    string // runtime.GOOS, as opencode's process.platform
	shell string // the shell's path
	tmp   string // the directory the description offers for temporary work
}

// thisHost returns this machine's host. The specs golden replaces it, so the
// description it pins does not depend on the machine the test runs on.
var thisHost = func() host {
	// opencode offers os.tmpdir()/opencode (core/src/global.ts:15).
	return host{os: runtime.GOOS, shell: pickShell(bashPath, shPath), tmp: filepath.Join(os.TempDir(), "craze")}
}

// pickShell returns bash when it is an executable file, else sh.
func pickShell(bash, sh string) string {
	if info, err := os.Stat(bash); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
		return bash
	}
	return sh
}

type bashTool struct {
	spec tool.Spec
	host host
}

func newBash() (tool.Tool, error) {
	h := thisHost()
	// The values opencode's ShellPrompt.render puts in for a bash shell
	// (shell/prompt.ts:273-291); NOTICE says how the rest were rendered.
	desc, err := description("bash", map[string]string{
		"os":               h.os,
		"shell":            filepath.Base(h.shell),
		"tmp":              h.tmp,
		"defaultTimeoutMs": strconv.FormatInt(defaultTimeout.Milliseconds(), 10),
		"maxLines":         strconv.Itoa(tool.MaxLines),
		"maxBytes":         strconv.Itoa(tool.MaxBytes),
	})
	if err != nil {
		return nil, err
	}
	// opencode's Parameters (shell/prompt.ts:15-23) as its JSON Schema
	// renders them (test/tool/__snapshots__/parameters.test.ts.snap):
	// timeout is a PositiveInt, which renders with both bounds.
	return &bashTool{host: h, spec: tool.Spec{
		ID:          "bash",
		Description: desc,
		Parameters: map[string]any{
			"command": map[string]any{"type": "string", "description": "The command to execute"},
			"timeout": map[string]any{"type": "integer", "exclusiveMinimum": 0, "minimum": -maxSafeInteger, "maximum": maxSafeInteger,
				"description": "Optional timeout in milliseconds"},
			"workdir": map[string]any{"type": "string",
				"description": "The working directory to run the command in. Defaults to the current directory. Use this instead of 'cd' commands."},
		},
		Required: []string{"command"},
		Kind:     tool.KindExecute,
		// Not parallel: Fantasy runs it one at a time among the tools that
		// are not, so a batch like [edit, bash test] runs in order.
		//
		// bash keeps the tail of its output, spills the rest, and says so in
		// opencode's own words, so the dispatcher leaves its text alone.
		Truncate: tool.None,
	}}, nil
}

func (b *bashTool) Spec() tool.Spec { return b.spec }

func (b *bashTool) Prepare(env tool.Env, c tool.Call) (tool.Prepared, error) {
	a, err := parseArgs(c.Input)
	if err != nil {
		return nil, err
	}
	command, _, err := a.str("command", true)
	if err != nil {
		return nil, err
	}
	ms, hasTimeout, err := a.integer("timeout", 1)
	if err != nil {
		return nil, err
	}
	workdir, _, err := a.str("workdir", false)
	if err != nil {
		return nil, err
	}
	call := &bashCall{host: b.host, id: c.ID, command: command, dir: env.Workspace, timeout: defaultTimeout,
		ops: realOps, spillCap: maxSpillBytes}
	// `params.workdir ? resolvePath(params.workdir, ...) : directory`
	// (shell.ts:612-614): an empty workdir is the workspace. Whether it
	// exists is checked when the call runs, since an earlier call in the
	// same step may create it.
	if workdir != "" {
		call.dir = env.Resolve(workdir)
	}
	if hasTimeout {
		if ms > maxTimeout.Milliseconds() {
			call.requested, call.timeout = ms, maxTimeout
		} else {
			call.timeout = time.Duration(ms) * time.Millisecond
		}
	}
	return call, nil
}

type bashCall struct {
	host    host
	id      string // the harness's id, which names the spill file
	command string
	dir     string // resolved; not yet checked
	timeout time.Duration
	// requested is the timeout the model asked for, in milliseconds, when
	// it was over maxTimeout and reduced; 0 otherwise.
	requested int64

	// ops is the call's filesystem and process work, which tests replace to
	// stall any one piece of it; spillCap bounds what the spill file holds.
	ops      ops
	spillCap int64
}

// ops is every call a bash call makes that can stall on a filesystem that
// does not answer, by function, so a test can hold any one of them for ever
// and watch Run return all the same (output.go says how). realOps are the
// real ones.
type ops struct {
	stat      func(string) (os.FileInfo, error) // the workdir check
	mkdirAll  func(string, os.FileMode) error   // the temporary directory
	start     func(*exec.Cmd) (*group, error)   // exec, in the workdir
	openSpill spillOpener                       // the spill file, under the harness home
}

var realOps = ops{stat: os.Stat, mkdirAll: os.MkdirAll, start: startGroup, openSpill: openSpill}

// errLaunchTimeout is launch's error when the call's timeout passed before the
// command had started.
var errLaunchTimeout = errors.New("bash: the timeout passed before the command started")

func (c *bashCall) Request() tool.Request {
	// opencode's title is the command (shell.ts:586).
	return tool.Request{Title: c.command, Command: c.command, Workdir: c.dir}
}

// Run runs the command with the shell, in its own session and process group,
// and returns the tail of its output with opencode's <shell_metadata>.
//
// It returns within fixed bounds whatever the command and the filesystem
// do (output.go says how that is arranged). The timeout runs from here, so
// it covers the work before the command as well as the command. The bounds:
//
//	ctx done or the session closing, before the command has started    at once
//	the timeout passing before the command has started                 at the timeout (+0.1 s slack)
//	the leader exiting by itself                                       3.7 s: the 2 s drain wait, the 0.7 s read of what is left in the pipe, the 1 s wait for the spill file
//	ctx done, or the timeout passing, while the command runs           6.7 s: the 3 s grace comes first
//	the session closing — ctx cancelled with tool.ErrClosing, or        2.7 s, whatever came before, an ordinary cancel's grace included;
//	env.Closing closed                                                 a close does not wait for the spill file
//
// (supervise, collect, spiller.wait).
func (c *bashCall) Run(ctx context.Context, env tool.Env) tool.Result {
	if ctx.Err() != nil || tool.SessionClosing(ctx, env.Closing) {
		return tool.Result{Text: tool.AbortedText, IsError: true, Class: tool.ClassAborted}
	}
	if env.Environ == nil {
		return errorResult(fail(tool.ClassToolError, noEnvironText))
	}
	began := time.Now()
	deadline := began.Add(c.timeout)
	g, r, err := c.launch(ctx, env, deadline)
	if err != nil {
		if errors.Is(err, errLaunchTimeout) {
			return c.result(outcome{why: endTimeout, took: time.Since(began)})
		}
		return errorResult(err)
	}
	defer func() { _ = r.Close() }()

	out := &output{home: env.Home, id: c.id, open: c.ops.openSpill, cap: c.spillCap}
	// The redactor sees the output before anything else does, so the tail,
	// every progress snapshot and the spill file hold only its redacted form
	// (plan 019 §3.8). It holds back a key's length less one byte, so a key
	// split across two reads of the pipe is still caught. Nothing the reader
	// writes to waits on a file: the spill file is written by a goroutine of
	// its own (spiller), so the reader always empties the pipe.
	stream := env.Redactor.NewWriter(out)
	copied := make(chan struct{})
	var copyErr error // set before copied closes
	go func() {
		defer close(copied)
		_, copyErr = io.Copy(stream, r)
	}()
	stopProgress := out.report(env.Progress)

	why, reaped := g.supervise(ctx, env.Closing, time.Until(deadline), copied)
	returned, complete := collect(r, copied, &copyErr)
	stopProgress()
	if returned {
		_ = stream.Close() // the bytes it held back, now that the stream has ended
	}
	took := time.Since(began)

	kept, cut, trunc, capped, spill := out.finish()
	if spill != nil {
		wait := spillWait
		if tool.SessionClosing(ctx, env.Closing) {
			wait = 0 // nobody will read this result; the session must close
		}
		if path := spill.wait(wait, env.Closing); cut {
			trunc.Spill = path
		}
	}
	var state *os.ProcessState
	if reaped {
		state = g.state
	}
	return c.result(outcome{why: why, state: state, kept: kept, cut: cut, trunc: trunc,
		capped: capped && trunc.Spill != "", partial: !complete, took: took})
}

// launched is what setup produced: the started command and the read end of
// its output, or an error.
type launched struct {
	g   *group
	r   *os.File
	err error
}

// discard kills what was started, lets watch reap it, and closes the read
// end. A start that failed left nothing to discard.
//
// This is the one place a started command is killed without the SIGTERM
// grace: the stop was fired before setup's check, but Start had already
// returned by the time launch read the result, so the command has existed
// for the width of that window — microseconds, or the scheduler's delay
// under load — and has produced nothing worth draining. Accepted after
// review (plan 019 execution record); a command that runs past launch is
// always stopped through supervise, with the grace.
func (l launched) discard() {
	if l.err != nil {
		return
	}
	l.g.signal(syscall.SIGKILL)
	close(l.g.release) // watch reaps the leader once it has died
	_ = l.r.Close()
}

// readEnd hands the output pipe's read end from start to launch, so that
// launch can close it when it abandons a start that never returns. put and
// close may come in either order: the end is closed either way.
type readEnd struct {
	mu     sync.Mutex
	f      *os.File
	closed bool
}

func (e *readEnd) put(f *os.File) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.f = f
	if e.closed {
		_ = f.Close()
	}
}

func (e *readEnd) close() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.closed = true
	if e.f != nil {
		_ = e.f.Close()
	}
}

// launch runs setup — the filesystem work before the command, and its start
// — on a goroutine of its own, so that a filesystem that does not answer (a
// stalled network mount under the workspace, or under the temporary
// directory) cannot hold Run past a cancel, a close or the call's timeout. If
// ctx is done or env.Closing closes first, launch returns context.Canceled
// at once (an aborted result); if the deadline passes first, with the
// timeout's slack, errLaunchTimeout (a timeout result). A stop that has
// fired by the time the work before the command is done is still a stop:
// setup checks once more, just before it starts the command, and starts
// nothing (stopped). So a stop is never mistaken for one that came after the
// command had started — which select alone could not tell, ready as both may
// be when it looks — and a command that did start before the stop is a
// started command, stopped by supervise with its grace and its output kept.
//
// An abandoned start is left to the one goroutine running it: it discards
// whatever it goes on to start, the moment it has started. Until then, if
// that is never, the goroutine leaks with the command's exec.Cmd and
// environment and, once start has made the pipe, its write end: Start may be
// using that descriptor's number, so it cannot be closed from outside (start
// closes it itself once Start returns). The read end, which Start never
// touches, is closed here at once. So an abandoned start that never returns
// holds one goroutine, one descriptor, and — once the fork has happened — the
// child's own copy of the write end.
func (c *bashCall) launch(ctx context.Context, env tool.Env, deadline time.Time) (*group, *os.File, error) {
	done := make(chan launched) // unbuffered: the goroutine learns whether launch is still there
	quit := make(chan struct{}) // closed when launch has given the start up
	var r readEnd
	go func() {
		var l launched
		l.g, l.r, l.err = c.setup(ctx, env, deadline, &r)
		select {
		case done <- l:
		case <-quit:
			l.discard()
		}
	}()
	expiry := time.NewTimer(time.Until(deadline) + timeoutSlack)
	defer expiry.Stop()
	err := context.Canceled
	select {
	case l := <-done:
		return l.g, l.r, l.err
	case <-ctx.Done():
	case <-env.Closing:
	case <-expiry.C:
		err = errLaunchTimeout
	}
	close(quit)
	r.close()
	return nil, nil, err
}

// stopped reports, without waiting, why the call should stop before its
// command runs: context.Canceled when ctx is done or the session is closing,
// errLaunchTimeout when the deadline, with the timeout's slack, has passed,
// and nil when it should go on. A cancel or a close comes before the
// deadline: an abort ends the call sooner. setup asks just before it starts
// the command (launch).
func stopped(ctx context.Context, closing <-chan struct{}, deadline time.Time) error {
	if ctx.Err() != nil || tool.SessionClosing(ctx, closing) {
		return context.Canceled
	}
	if !time.Now().Before(deadline.Add(timeoutSlack)) {
		return errLaunchTimeout
	}
	return nil
}

// setup is the work before the command, on launch's goroutine: the working
// directory must exist and be a directory; the directory the description
// offers for temporary work is made, since the description tells the model
// it already exists (failing to make it is left for a command that uses it
// to meet); then, unless the call has been stopped meanwhile, the command is
// started. r receives the output's read end as soon as there is one (launch).
func (c *bashCall) setup(ctx context.Context, env tool.Env, deadline time.Time, r *readEnd) (*group, *os.File, error) {
	if err := checkWorkdir(c.ops.stat, c.dir); err != nil {
		return nil, nil, err
	}
	_ = c.ops.mkdirAll(c.host.tmp, 0o700)
	if err := stopped(ctx, env.Closing, deadline); err != nil {
		return nil, nil, err
	}
	g, f, err := c.start(env, r)
	if err != nil {
		return nil, nil, fail(tool.ClassToolError, "The command could not be started: "+err.Error())
	}
	return g, f, nil
}

// start starts the command: `<shell> -c <command>` in its working directory,
// in a new session and process group with no controlling terminal (Setsid,
// which is what opencode's `detached: true` does, shell.ts:303-309), so
// sudo, ssh and git credential prompts fail rather than read the user's
// keystrokes or draw over the TUI. Its stdin is /dev/null (exec's default);
// stdout and stderr are one pipe, whose read end start returns, and puts in
// hold first, for a launch that gives up while Start never returns.
func (c *bashCall) start(env tool.Env, hold *readEnd) (*group, *os.File, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, nil, err
	}
	hold.put(r)
	cmd := exec.Command(c.host.shell, "-c", c.command)
	cmd.Dir = c.dir
	cmd.Env = environ(env, c.dir)
	cmd.Stdout, cmd.Stderr = w, w
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	g, err := c.ops.start(cmd)
	// The command has its own copy of the write end. Once craze's is
	// closed, the pipe closes when the last process holding it exits.
	_ = w.Close()
	if err != nil {
		_ = r.Close()
		return nil, nil, err
	}
	return g, r, nil
}

// environ is the command's environment: env.Environ, which the harness
// builds with tool.ChildEnviron so that craze's own provider keys are gone
// (plan 019 §3.8), less every OPENAI_* variable once more in case a caller
// did not, and with PWD set to the working directory, as exec.Cmd does
// itself only when it builds the environment. Run refuses a nil
// env.Environ before it gets here: this tool cannot know which variables
// hold provider keys, so it never falls back to craze's own environment.
func environ(env tool.Env, dir string) []string {
	return append(tool.ChildEnviron(env.Environ, nil), "PWD="+dir)
}

// checkWorkdir refuses a working directory that does not exist or is not a
// directory, before anything is started in it. stat is os.Stat, or a test's.
func checkWorkdir(stat func(string) (os.FileInfo, error), dir string) error {
	info, err := stat(dir)
	switch {
	case missing(err):
		return fail(tool.ClassNotFound, "workdir does not exist: "+dir)
	case err != nil:
		return err
	case !info.IsDir():
		return fail(tool.ClassToolError, "workdir is not a directory: "+dir)
	}
	return nil
}

// outcome is what result makes a call's result from.
type outcome struct {
	why     ending
	state   *os.ProcessState // nil when the leader was not reaped
	kept    string           // the tail of the output the model is shown
	cut     bool             // kept is less than all of the output
	trunc   tool.Truncation  // Spill is the spill file, when there is one
	capped  bool             // the spill file stops at the cap
	partial bool             // the output was not read to its end
	took    time.Duration
}

// result is the call's result: the tail of the output with opencode's
// notice of what was cut, then the <shell_metadata> lines (shell.ts:561-584).
// To opencode's lines craze adds one when the timeout was reduced, "exit
// code: N" when the command exited non-zero — a failed command with no output
// would otherwise read "(no output)" — and one when the output may be
// incomplete. A non-zero exit is not an error: the command ran, and the model
// reads the code. A timeout and a cancel are, classed timeout and aborted; an
// aborted result's text starts with opencode's "Tool execution aborted" and
// keeps what the command wrote.
func (c *bashCall) result(o outcome) tool.Result {
	var meta []string
	if c.requested > 0 {
		meta = append(meta, fmt.Sprintf("The requested timeout of %d ms is above the maximum of %d ms; the command ran with a timeout of %d ms.",
			c.requested, maxTimeout.Milliseconds(), maxTimeout.Milliseconds()))
	}
	code := -1 // none: the command was stopped, or its leader never reaped
	switch o.why {
	case endTimeout:
		meta = append(meta, fmt.Sprintf("shell tool terminated command after exceeding timeout %d ms. If this command is expected to take longer and is not waiting for interactive input, retry with a larger timeout value in milliseconds.",
			c.timeout.Milliseconds()))
	case endAbort:
		meta = append(meta, "User aborted the command")
	default:
		// A leader that exited by itself has been reaped, unless reaping
		// failed; then there is no code to report.
		if o.state != nil {
			if code = exitCode(o.state); code != 0 {
				meta = append(meta, fmt.Sprintf("exit code: %d", code))
			}
		}
	}
	if o.partial {
		meta = append(meta, partialText)
	}

	text := o.kept
	if text == "" {
		text = "(no output)"
	}
	if o.cut {
		// "Full" only when the file holds every byte the command wrote:
		// not when it stops at the cap, and not when the output itself was
		// not read to its end.
		saved := "The full output could not be saved."
		switch {
		case o.trunc.Spill != "" && !o.capped && !o.partial:
			saved = "Full output saved to: " + o.trunc.Spill
		case o.trunc.Spill != "":
			// Each shortfall is stated; a file can have both.
			saved = "Output saved to: " + o.trunc.Spill
			if o.capped {
				saved += "\nThe saved output stops at " + size(c.spillCap) + "; the rest was not saved."
			}
			if o.partial {
				saved += "\nThe saved output holds only what was read of the output."
			}
		}
		text = "...output truncated...\n\n" + saved + "\n\n" + text
	}
	if len(meta) > 0 {
		text += "\n\n<shell_metadata>\n" + strings.Join(meta, "\n") + "\n</shell_metadata>"
	}

	res := tool.Result{Text: text, Output: &tool.ExecOutput{ExitCode: code, Output: o.kept, Duration: o.took}, Trunc: o.trunc}
	switch o.why {
	case endTimeout:
		res.IsError, res.Class = true, tool.ClassTimeout
	case endAbort:
		res.IsError, res.Class, res.Text = true, tool.ClassAborted, tool.AbortedText+"\n\n"+text
	}
	return res
}

// size says n bytes the way the spill cap is stated: in MiB when it is a
// whole number of them.
func size(n int64) string {
	if n > 0 && n%(1<<20) == 0 {
		return strconv.FormatInt(n>>20, 10) + " MiB"
	}
	return strconv.FormatInt(n, 10) + " bytes"
}

// exitCode is the leader's exit status as a shell reports one: its exit
// code, or 128 plus the number of the signal that killed it.
func exitCode(s *os.ProcessState) int {
	if ws, ok := s.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return s.ExitCode()
}
