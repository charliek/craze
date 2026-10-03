package opencode

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
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
	// partialText says the output was not read to its end: something the
	// command left running outside its process group still held the pipe
	// when reading stopped.
	partialText = "The output may be incomplete: something the command started still held its output open when the call ended, and what it wrote after that was not read."
	// foregroundText says a call that asked for the background ran in the
	// foreground: the session runs no background jobs — headless `craze
	// prompt`, or a sub-agent's session (plan 033 §3.7, P11). Such a session's
	// bash does not offer run_in_background (X101), so this answers a model
	// that sends it all the same: a defence, not a path.
	foregroundText = "This session does not run background jobs: run_in_background was ignored, and the command ran in the foreground with the foreground's timeout."
)

// noEnvironText refuses a call whose Env has no child environment. The tool
// cannot tell which variables hold craze's provider keys, so it never falls
// back to its own environment (plan 019 §3.8). grep and glob refuse in the
// same words (noEnviron).
var noEnvironText = noEnviron("bash")

// host is what bash's description tells the model about the machine, and the
// shell it runs commands with.
type host struct {
	os    string // runtime.GOOS, as opencode's process.platform
	shell string // the shell's path
	tmp   string // the directory the description offers for temporary work
}

// thisHost returns this machine's host, with the temporary directory made
// (tmpDir): it is called once per profile, so that is the session's. The
// specs golden replaces it, so the description it pins does not depend on
// the machine the test runs on.
var thisHost = func() (host, error) {
	tmp, err := tmpDir(os.TempDir())
	if err != nil {
		return host{}, err
	}
	return host{os: runtime.GOOS, shell: pickShell(bashPath, shPath), tmp: tmp}, nil
}

// tmpDir makes the directory bash offers for temporary work and returns it:
// base/craze — opencode offers os.tmpdir()/opencode (core/src/global.ts:15)
// — when it can be made, or is already there and safely ours (ensureTmp);
// otherwise a directory with a random name under base, made for this
// session. A path anyone on the machine can predict is one anyone can plant
// — as a symlink, or a directory they own — and a command's scratch files
// would then land where they choose; the plant is left alone and not used.
// An error means base itself is unusable.
func tmpDir(base string) (string, error) {
	fixed := filepath.Join(base, "craze")
	if err := ensureTmp(fixed); err == nil {
		return fixed, nil
	}
	return os.MkdirTemp(base, "craze-")
}

// ensureTmp makes path as the temporary directory, mode 0700, or checks what
// is there: it must be a directory, not a symlink, owned by this user, with
// mode 0700 exactly. Anything else may be a plant, and is refused. It runs
// before every command as well as at the start: a directory that was safe
// can be replaced.
func ensureTmp(path string) error {
	if err := os.Mkdir(path, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	switch {
	case info.Mode()&fs.ModeSymlink != 0:
		return fmt.Errorf("%s is a symlink", path)
	case !info.IsDir():
		return fmt.Errorf("%s is not a directory", path)
	case info.Mode().Perm() != 0o700:
		return fmt.Errorf("%s has mode %04o, not 0700", path, info.Mode().Perm())
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("%s: the owner cannot be checked", path)
	}
	if int(st.Uid) != os.Getuid() {
		return fmt.Errorf("%s is owned by uid %d, not this user (uid %d)", path, st.Uid, os.Getuid())
	}
	return nil
}

// pickShell returns bash when it is an executable file, else sh.
func pickShell(bash, sh string) string {
	if info, err := os.Stat(bash); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
		return bash
	}
	return sh
}

// Shell returns the path of the shell this host's bash tool runs commands
// with: pickShell's choice between bashPath and shPath, the same one bash's
// own description names (host.shell above). It is exported so the harness can
// fill the system prompt's environment block (tool.SystemEnv.Shell) with the
// identical value, rather than a second guess at which shell bash actually
// runs.
func Shell() string { return pickShell(bashPath, shPath) }

type bashTool struct {
	spec tool.Spec
	host host
	// plain is the bash a session that runs no background jobs is offered in
	// this one's place (WithoutJobs, plan 033 X101); nil on plain itself.
	plain *bashTool
}

func newBash() (tool.Tool, error) {
	h, err := thisHost()
	if err != nil {
		return nil, fmt.Errorf("bash: no usable temporary directory: %w", err)
	}
	// The values opencode's ShellPrompt.render puts in for a bash shell
	// (shell/prompt.ts:273-291); NOTICE says how the rest were rendered.
	vars := map[string]string{
		"os":               h.os,
		"shell":            filepath.Base(h.shell),
		"tmp":              h.tmp,
		"defaultTimeoutMs": strconv.FormatInt(defaultTimeout.Milliseconds(), 10),
		"maxLines":         strconv.Itoa(tool.MaxLines),
		"maxBytes":         strconv.Itoa(tool.MaxBytes),
	}
	desc, err := description("bash", vars)
	if err != nil {
		return nil, err
	}
	plainDesc, err := descriptionWithoutJobs("bash", vars)
	if err != nil {
		return nil, err
	}
	// opencode's Parameters (shell/prompt.ts:15-23) as its JSON Schema
	// renders them (test/tool/__snapshots__/parameters.test.ts.snap):
	// timeout is a PositiveInt, which renders with both bounds.
	plain := &bashTool{host: h, spec: tool.Spec{
		ID:          tool.BashTool,
		Description: plainDesc,
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
	}}
	// A session that runs background jobs is offered the same tool with its
	// jobs: the description's jobs blocks kept, and run_in_background, which
	// is craze's (plan 033 §3.7) — Claude Code's name for the same request, as
	// the agent tool's is (plan 026 §3.11).
	full := plain.spec
	full.Description = desc
	full.Parameters = maps.Clone(plain.spec.Parameters)
	full.Parameters["run_in_background"] = map[string]any{"type": "boolean",
		"description": "Optional. true runs the command in the background where this session supports it: the call returns at once " +
			"with the job's id, timeout is the job's limit, and its result is delivered to you when it finishes. Where " +
			"background jobs are not supported the command runs in the foreground."}
	return &bashTool{host: h, spec: full, plain: plain}, nil
}

func (b *bashTool) Spec() tool.Spec { return b.spec }

// WithoutJobs is the bash a session that runs no background jobs — headless,
// or a sub-agent's — is offered (tool.JobsAware, plan 033 X101): C8's
// description, with no word of the background, promotion or job tools, and no
// run_in_background parameter. Its Prepare is this one's, so a model that
// sends run_in_background all the same gets the foreground and a line saying
// so (foregroundText), never an error.
func (b *bashTool) WithoutJobs() tool.Tool {
	if b.plain == nil {
		return b
	}
	return b.plain
}

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
	// run_in_background is a boolean, null or absent being false, and
	// anything else refused, as the agent tool reads its own.
	background, _, err := a.boolOrNull("run_in_background")
	if err != nil {
		return nil, err
	}
	call := &bashCall{host: b.host, id: c.ID, command: command, dir: env.Workspace, timeout: defaultTimeout,
		ops: realOps, spillCap: maxSpillBytes, spillWait: spillWait}
	// `params.workdir ? resolvePath(params.workdir, ...) : directory`
	// (shell.ts:612-614): an empty workdir is the workspace. Whether it
	// exists is checked when the call runs, since an earlier call in the
	// same step may create it.
	if workdir != "" {
		call.dir = env.Resolve(workdir)
	}
	// The timeout's meaning follows where the command will run (plan 033
	// §3.7): in the background — asked for, in a session that runs jobs — it
	// is the job's limit, 30 minutes when none is given and 2 hours at most;
	// anywhere else it is the foreground's, 2 minutes and 10 at most,
	// run_in_background or not, and a call that asked for the background
	// says it ran in the foreground (foregroundText).
	most := maxTimeout
	switch {
	case background && env.Jobs != nil:
		call.background, call.timeout, most = true, tool.JobDefaultLimit, tool.JobMaxLimit
	case background:
		call.foreground = true
	}
	if hasTimeout {
		if ms > most.Milliseconds() {
			call.requested, call.timeout = ms, most
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
	// timeout is the command's: in the foreground, how long it runs before
	// it is stopped (or promoted); in the background, the job's limit.
	timeout time.Duration
	// requested is the timeout the model asked for, in milliseconds, when
	// it was over the maximum (maxTimeout, or tool.JobMaxLimit in the
	// background) and reduced; 0 otherwise.
	requested int64
	// background: run_in_background, in a session that runs jobs (Env.Jobs);
	// foreground: run_in_background in one that does not, which runs the
	// command in the foreground and says so (plan 033 §3.7).
	background, foreground bool

	// ops is the call's filesystem and process work, which tests replace to
	// stall any one piece of it; spillCap bounds what the spill file holds;
	// spillWait bounds the wait, once the output has ended, for the spill
	// writer to finish the file — the constant spillWait, but in a test that
	// needs the file and is about something else, which gives the writer
	// longer so that a slow filesystem cannot cost it the file (plan 035 C5).
	ops       ops
	spillCap  int64
	spillWait time.Duration
}

// ops is every call a bash call makes that can stall on a filesystem that
// does not answer, by function, so a test can hold any one of them for ever
// and watch Run return all the same (output.go says how). realOps are the
// real ones.
type ops struct {
	stat      func(string) (os.FileInfo, error) // the workdir check
	ensureTmp func(string) error                // the temporary directory: made, or checked
	start     func(*exec.Cmd) (*group, error)   // exec, in the workdir
	openSpill spillOpener                       // the spill file, under the harness home
	// expiring is a test seam: promotion.expiring, run as the foreground
	// timeout fires in a session that runs jobs. nil in production.
	expiring func()
	// expire is a test seam: the foreground timeout's timer (supervise's
	// fire), which a test fires once the command has started and its output
	// so far has arrived, so that no test of what happens at the timeout
	// depends on how fast a loaded machine starts a shell (review r7 finding
	// 10). Such a test gives a timeout long enough that launch's own bound,
	// which is real time, is never what it meets. nil in production.
	expire <-chan time.Time
	// launching is a test seam: called on launch's own goroutine once it has
	// set the start going, before it waits for the start or a stop — where a
	// test holds it until the start has returned with a stop fired inside
	// it, so that both are ready when launch looks (plan 033 C14r2). nil in
	// production.
	launching func()
}

var realOps = ops{stat: os.Stat, ensureTmp: ensureTmp, start: startGroup, openSpill: openSpill}

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
//	the timeout passing, the command moved to the background            1 s: the wait for the spill file to open (bash_job.go)
//	a run_in_background command started                                1 s, the same wait
//
// (supervise, collect, spiller.wait).
//
// In a session that runs jobs (Env.Jobs, plan 033 §3.7–§3.8), a command
// still running at its timeout is moved to the background rather than
// stopped (promote, bash_job.go) — unless the call has been cancelled or the
// session is closing, or the session already runs as many jobs as it may:
// then it is stopped as it always was, and the result says why. A call that
// asked for the background starts its command as a job and returns at once
// (runBackground); one that asked for it where there are no jobs runs in the
// foreground, and says so.
func (c *bashCall) Run(ctx context.Context, env tool.Env) tool.Result {
	if ctx.Err() != nil || tool.SessionClosing(ctx, env.Closing) {
		return tool.Result{Text: tool.AbortedText, IsError: true, Class: tool.ClassAborted}
	}
	if env.Environ == nil {
		return errorResult(fail(tool.ClassToolError, noEnvironText))
	}
	if c.background {
		return c.runBackground(ctx, env)
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
	return c.supervised(ctx, env, c.attach(env, g, r, began), deadline, env.Jobs != nil)
}

// supervised runs a started command in the foreground to its end and
// returns its result — or, with promotable set (a session that runs jobs), to
// its timeout and then into the background (promote). j holds what was
// started: the group, its output pipe and the reader feeding the call's
// output. supervised closes the pipe when it is done with it, unless it has
// handed j to a job — exactly once, in promote — whose from then on it all is
// (plan 033 §3.8).
func (c *bashCall) supervised(ctx context.Context, env tool.Env, j *bashJob, deadline time.Time, promotable bool) (res tool.Result) {
	handed := false // j is a job's (promote): nothing here closes or kills it
	var slot tool.JobSlot
	var refused string // why a promotion was refused: the cap, or a close (P26)
	defer func() {
		if handed {
			return
		}
		// A slot taken for a promotion that never got as far as the hand-over
		// — a panic between the two, which the dispatcher recovers — is given
		// back, and its command killed and released for reaping, as supervise
		// would have: the slots are what the session's Close joins on. handed
		// is set just before the hand-over itself (promote), so this never
		// touches what a job may already own.
		if slot != nil {
			j.g.signal(syscall.SIGKILL)
			close(j.g.release)
			slot.Release()
		}
		_ = j.r.Close()
		j.untrack()
	}()
	stopProgress := j.out.report(env.Progress)
	var promote *promotion
	if promotable {
		promote = &promotion{expiring: c.ops.expiring, try: func() bool {
			s, err := env.Jobs.Reserve(c.id)
			if err != nil {
				refused = refusal(err)
				return false
			}
			slot = s
			return true
		}}
	}

	why, reaped := j.g.supervise(ctx, env.Closing, time.Until(deadline), c.ops.expire, j.copied, promote)
	if why == endPromote {
		stopProgress()
		return c.promote(env, j, slot, &handed)
	}
	o := j.ended(ctx, why, reaped, stopProgress)
	o.refused = refused
	return c.result(o)
}

// ended is what a command left once supervise has returned why and reaped —
// in the foreground (supervised) or as a job (bashJob.Wait), the same steps:
// the reader collected, the progress stopped, the bytes the stream's stages
// held back written out now that it has ended, the output finished and the
// spill file waited for — not at all once the session is closing, since
// nobody will read the result and the session must close — and the leader's
// exit status when it was reaped. It does not close the pipe: its caller
// does.
func (j *bashJob) ended(ctx context.Context, why ending, reaped bool, stopProgress func()) outcome {
	returned, complete := collect(j.r, j.copied, &j.copyErr)
	stopProgress()
	if returned {
		_ = j.stream.Close()
	}
	took := time.Since(j.began)

	kept, cut, trunc, capped, spill := j.out.finish()
	if spill != nil {
		wait := j.c.spillWait
		if tool.SessionClosing(ctx, j.closing) {
			wait = 0
		}
		if path := spill.wait(wait, j.closing); cut {
			trunc.Spill = path
		}
	}
	var state *os.ProcessState
	if reaped {
		state = j.g.state
	}
	return outcome{why: why, state: state, kept: kept, cut: cut, trunc: trunc,
		capped: capped && trunc.Spill != "", partial: !complete, took: took}
}

// attach starts reading a started command's output: the pipe's read end r,
// through the redaction and escape-sequence stages, into the call's output.
//
// The redactor and the escape-sequence stripper see the output before
// anything else does (modelStream, ansi.go), so the tail, every progress
// snapshot and the spill file hold only its redacted, stripped form (plan
// 019 §3.8, plan 033 §3.6). The redactor holds back a key's length less one
// byte, so a key split across two reads of the pipe is still caught, and the
// stripper carries an escape sequence the same way. Nothing the reader writes
// to waits on a file: the spill file is written by a goroutine of its own
// (spiller), so the reader always empties the pipe.
//
// The stream is tracked from here until the command is done with
// (bashJob.untrack): its redaction is widened with every key the session
// learns while it runs, and the output, its spill file and every bash_output
// read hold none of them. In a session that runs jobs it is tracked through
// tool.Jobs.Track — any command there may become a job and outlive its turn's
// redactor (plan 033 C10r) — and in any other through Env.Streams, the same
// registry: a token the ChatGPT sign-in mints while a command runs must not
// reach the rest of its output raw (plan 033 C14r, r12 #6a).
func (c *bashCall) attach(env tool.Env, g *group, r *os.File, began time.Time) *bashJob {
	out := &output{home: env.Home, id: c.id, open: c.ops.openSpill, cap: c.spillCap}
	j := &bashJob{c: c, g: g, r: r, out: out, stream: newModelStream(env.Redactor, out),
		copied: make(chan struct{}), began: began, closing: env.Closing, untrack: func() {}}
	tracker := env.Streams
	if env.Jobs != nil {
		tracker = env.Jobs
	}
	if tracker != nil {
		// Before the reader starts, so the stream decides no byte before it
		// knows every key the session does.
		j.untrack = tracker.Track(j.stream)
	}
	go func() {
		defer close(j.copied)
		_, j.copyErr = io.Copy(j.stream, r)
	}()
	return j
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
// grace: the stop fired before launch took the start's result — select took
// the stop, or took the start with the stop already fired (launch) — but
// Start had begun the command, so it has existed for the width of that
// window — microseconds, or the scheduler's delay under load — and has
// produced nothing worth draining. Accepted after review (plan 019 execution
// record); a command that runs past launch is always stopped through
// supervise, with the grace.
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
// nothing (stopped). One that fires while the command starts — after Start
// began it, before launch has its result — is a stop too: launch checks
// again as it takes the result, and discards the command (plan 033 C14r2).
// So a stop is never mistaken for one that came after the command had
// started — which select alone could not tell, ready as both may be when it
// looks — and a command launch has returned before the stop is a started
// command, stopped by supervise with its grace and its output kept.
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
	if c.ops.launching != nil {
		c.ops.launching()
	}
	err := context.Canceled
	select {
	case l := <-done:
		// The start's result and a stop can both be ready by the time select
		// looks — the stop fired inside the start, after the command began
		// and before Start returned, and this goroutine reached select only
		// then, as a loaded scheduler can leave it — and select takes either
		// at random. So a stop that has fired by the time launch takes the
		// start is still a stop, whichever it took: the command is discarded,
		// never supervised, as it is when select takes the stop (plan 033
		// C14r2: TestBashStopAsTheStartCompletes failed once under -race, the
		// start taken and the command supervised). A command is supervised,
		// with the grace, only once launch has returned it.
		if l.err == nil {
			if stop := stopped(ctx, env.Closing, deadline); stop != nil {
				l.discard()
				return nil, nil, stop
			}
		}
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
	if err := c.ops.ensureTmp(c.host.tmp); err != nil {
		return nil, nil, fail(tool.ClassToolError, "The temporary directory cannot be used: "+err.Error())
	}
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
// itself only when it builds the environment; then noPrompt. Run refuses a
// nil env.Environ before it gets here: this tool cannot know which variables
// hold provider keys, so it never falls back to craze's own environment.
//
// noPrompt comes last so that it overrides: exec.Cmd keeps the last value of
// a name it finds twice, so the user's PAGER or EDITOR never reaches the
// command. ripgrep (grep and glob) runs with this environment too, where
// none of it changes anything: rg writes JSON or NUL-separated paths to a
// pipe.
func environ(env tool.Env, dir string) []string {
	return append(append(tool.ChildEnviron(env.Environ, nil), "PWD="+dir), noPrompt...)
}

// noPrompt is the environment that keeps a command from waiting on a
// terminal it does not have (plan 033 §3.6). A command runs with no
// controlling terminal and stdin on /dev/null (start), so anything that asks
// for input fails or, worse, waits for the timeout; these make the usual
// askers not ask:
//
//   - pagers are cat, or none where an empty value means none (AWS, systemd);
//   - editors are true, which exits at once and leaves the file as it was —
//     so `git commit` without -m aborts on its empty message, `git rebase -i`
//     takes its plan as written, and a merge keeps its default message;
//   - git and ssh never prompt for a password or passphrase, on the terminal
//     (GIT_TERMINAL_PROMPT=0) or through a graphical askpass, which ssh
//     would otherwise run under a desktop session (SSH_ASKPASS_REQUIRE=never;
//     an empty GIT_ASKPASS ends git's search for one);
//   - apt and dpkg do not ask (DEBIAN_FRONTEND);
//   - the terminal is dumb and colour is off, for the commands that honour
//     one of these three conventions — the escape sequences of the rest are
//     stripped from the output (ansi.go);
//   - CRAZE_AGENT=1 tells a script it runs under craze's agent.
//
// The description tells the model the result: no terminal, editors and
// pagers do not open, `git commit` needs -m, colour is off
// (descriptions/bash.txt).
var noPrompt = []string{
	"PAGER=cat", "GIT_PAGER=cat", "MANPAGER=cat", "GH_PAGER=cat", "AWS_PAGER=", "SYSTEMD_PAGER=",
	"GIT_EDITOR=true", "GIT_SEQUENCE_EDITOR=true", "EDITOR=true", "VISUAL=true",
	"GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=", "SSH_ASKPASS=", "SSH_ASKPASS_REQUIRE=never",
	"DEBIAN_FRONTEND=noninteractive", "TERM=dumb", "NO_COLOR=1", "FORCE_COLOR=0", "CLICOLOR=0",
	"CRAZE_AGENT=1",
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
	// refused is why a command that reached its timeout was not moved to the
	// background (plan 033 P26): the job cap's refusal, or a close's; "" when
	// it was not asked to be.
	refused string
}

// result is the call's result: the tail of the output with opencode's
// notice of what was cut, then the <shell_metadata> lines (shell.ts:561-584).
// To opencode's lines craze adds one when the timeout was reduced, "exit
// code: N" when the command exited non-zero — a failed command with no output
// would otherwise read "(no output)" — and one when the output may be
// incomplete; and, in a session that runs jobs, why a command that reached its
// timeout was not moved to the background (P26), or, in one that runs none,
// that run_in_background ran it in the foreground (plan 033 §3.7). A non-zero
// exit is not an error: the command ran, and the model reads the code. A
// timeout and a cancel are, classed timeout and aborted; an aborted result's
// text starts with opencode's "Tool execution aborted" and keeps what the
// command wrote.
func (c *bashCall) result(o outcome) tool.Result {
	var meta []string
	if c.foreground {
		meta = append(meta, foregroundText)
	}
	if c.requested > 0 {
		meta = append(meta, c.reducedText())
	}
	code := -1 // none: the command was stopped, or its leader never reaped
	switch o.why {
	case endTimeout:
		meta = append(meta, fmt.Sprintf("shell tool terminated command after exceeding timeout %d ms. If this command is expected to take longer and is not waiting for interactive input, retry with a larger timeout value in milliseconds.",
			c.timeout.Milliseconds()))
		if o.refused != "" {
			meta = append(meta, o.refused) // why it was not moved to the background instead (P26)
		}
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

	text := c.text(o)
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

// text is the output as a result shows it: its tail, "(no output)" when there
// is none, and when the tail is less than all of it opencode's notice, with
// the file that holds the rest — "Full" only when the file holds every byte
// the command wrote, each shortfall stated otherwise. A foreground result
// puts its metadata after it (result); a job's result block holds it as its
// body (jobEnd).
func (c *bashCall) text(o outcome) string {
	text := o.kept
	if text == "" {
		text = "(no output)"
	}
	if !o.cut {
		return text
	}
	// "Full" only when the file holds every byte the command wrote: not when
	// it stops at the cap, and not when the output itself was not read to its
	// end.
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
	return "...output truncated...\n\n" + saved + "\n\n" + text
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
