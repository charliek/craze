package tui

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

// The composer's shell runner (plan 022 §3.6).
//
// Its lifecycle is `internal/harness/tool/opencode/process.go`'s, written again
// here rather than imported. Nothing forbids the import — depguard restricts
// what may be imported *into* internal/harness, and the Makefile's `go list`
// check forbids only internal/tui importing internal/acp — so this is a design
// choice: that runner is the harness's tool contract, with a session's closing
// channel, a tool.Env and an unreaped-leader dance it needs because an agent's
// bash tool outlives many calls. The composer runs one foreground command at a
// time for one human, and the shape that fits it is small enough that sharing
// would cost more than it saved. What is shared is the reasoning, so the
// comments below say where the two differ and why.

// shellTimeout is how long a command may run before craze ends it. It is the
// composer, not a build system: something still going after two minutes wants a
// terminal of its own. A var, as modeCallTimeout is, so a test can prove the
// timeout path without spending two minutes in it.
var shellTimeout = 120 * time.Second

const (
	// shellTermGrace is what the process group has between SIGTERM and
	// SIGKILL, the same 3 s opencode's runner gives it.
	shellTermGrace = 3 * time.Second
	// shellKillWait bounds the wait after SIGKILL for the leader to go: a
	// process in uninterruptible sleep dies only when the sleep ends, and craze
	// gives up on it rather than holding the row open for it.
	shellKillWait = 2 * time.Second
	// shellWaitDelay is exec's own bound on the *other* delay — a descendant
	// that inherited the output pipe and is still holding it open after the
	// leader exited. Without it `Wait` blocks on the copying goroutine for as
	// long as `sleep 300 &` lives. (opencode's runner bounds the same thing
	// with a drain timer of its own; cmd.WaitDelay is used nowhere else in
	// this repo, so this is new code rather than a port.)
	shellWaitDelay = 2 * time.Second
	// shellShutdownWait bounds a caller that waits for the runner to finish —
	// a quit path, a session change. The runner's own shutdown is bounded by
	// shellTermGrace + shellKillWait, so this only has to exceed that.
	shellShutdownWait = shellTermGrace + shellKillWait + time.Second
	// shellOutputCap is how much of the command's output craze keeps: the tail,
	// because the end of a build log is the part that says what happened.
	shellOutputCap = 16 << 10
	// shellTruncNote leads output whose head was dropped.
	shellTruncNote = "[craze: output truncated]"
	// fallbackShell is what runs when $SHELL names nothing craze can execute.
	fallbackShell = "/bin/sh"
)

// shellEnding is how a command ended, for the row's suffix.
type shellEnding int

const (
	shellExited    shellEnding = iota // the shell exited by itself, for good or ill
	shellKilled                       // Esc, Ctrl+C, a quit or a session change ended it
	shellTimedOut                     // shellTimeout passed first
	shellAbandoned                    // SIGKILL did not end it inside shellKillWait
)

// shellResult is one finished command.
type shellResult struct {
	out string
	// exit is the leader's status, or -1 where a signal ended it (which is
	// what every ending but shellExited means). start is set instead when the
	// command could not be started at all: a $SHELL that vanished between the
	// check and the exec, a workspace that is gone.
	exit  int
	why   shellEnding
	start error
}

// shellDoneMsg carries a finished command back into Update. gen names the run,
// so a result that outlived its row — a second command cannot start while one
// runs, but a kill and a quit race — is matched to the row it belongs to and
// to no other.
type shellDoneMsg struct {
	gen int
	// cmd is what was run, carried so a result whose row is gone — /clear took
	// it, the entry cap trimmed it — can still be drawn in full.
	cmd string
	res shellResult
}

// shellController owns the command the composer is running. Every copy of the
// model shares one, the way they share the session owner and the terminal
// colours: the model is copied on every Update, and a quit path — including
// SIGTERM's, which reaches finishRun with the model Run *started* with — has to
// reach the command a later copy started.
type shellController struct {
	mu sync.Mutex
	// gen numbers the runs from 1, so the zero value names none.
	gen  int
	stop context.CancelFunc
	// done is closed by the run's own goroutine as it returns, so a caller that
	// must not outlive the process group can wait for it. nil when nothing runs.
	done chan struct{}
}

func newShellController() *shellController { return &shellController{} }

// running reports a command still going.
func (c *shellController) running() bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.done != nil
}

// start mints a run and returns it with the tea.Cmd that carries it out. The
// work happens on bubbletea's own command goroutine, so Update never blocks on
// a command: what comes back is one shellDoneMsg.
func (c *shellController) start(script, dir string) (int, tea.Cmd) {
	c.mu.Lock()
	c.gen++
	gen := c.gen
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	c.stop, c.done = cancel, done
	c.mu.Unlock()
	return gen, func() tea.Msg {
		res := runShellCommand(ctx, script, dir)
		cancel() // the context outlives nothing; releasing it here keeps vet quiet and the timer freed
		c.finish(gen, done)
		return shellDoneMsg{gen: gen, cmd: script, res: res}
	}
}

// finish retires the run, but only if it is still the current one: a kill that
// raced a natural ending must not clear the state of the run after it.
func (c *shellController) finish(gen int, done chan struct{}) {
	c.mu.Lock()
	if c.gen == gen {
		c.stop, c.done = nil, nil
	}
	c.mu.Unlock()
	close(done)
}

// cancel asks the command to stop and returns at once. It is what Esc and
// Ctrl+C call: the row is updated when the shellDoneMsg lands, which is how
// every other asynchronous thing craze does reports itself, and blocking
// Update for the three seconds of a SIGTERM grace would freeze the frame.
func (c *shellController) cancel() {
	if c == nil {
		return
	}
	c.mu.Lock()
	stop := c.stop
	c.mu.Unlock()
	if stop != nil {
		stop()
	}
}

// shutdown is cancel for a caller that must not leave the process group behind
// it: a quit path or a session change. It waits for the run's goroutine, which
// has already sent the group its last SIGKILL by the time it returns. The wait
// is bounded twice over — by the runner's own deadline and by
// shellShutdownWait — because a quit that hangs is worse than a stray process.
func (c *shellController) shutdown() {
	if c == nil {
		return
	}
	c.mu.Lock()
	stop, done := c.stop, c.done
	c.mu.Unlock()
	if stop != nil {
		stop()
	}
	if done == nil {
		return
	}
	select {
	case <-done:
	case <-time.After(shellShutdownWait):
	}
}

// userShell is the shell a command runs under: $SHELL when it names an absolute
// path craze can execute, /bin/sh otherwise. Absolute because a relative $SHELL
// would resolve against the session's workspace rather than against anything
// the user meant, and executable because an exec that fails says nothing useful
// about why.
func userShell() string {
	sh := os.Getenv("SHELL")
	if !filepath.IsAbs(sh) {
		return fallbackShell
	}
	info, err := os.Stat(sh)
	if err != nil || info.IsDir() || info.Mode().Perm()&0o111 == 0 {
		return fallbackShell
	}
	return sh
}

// runShellCommand runs one command to its end and returns what it said.
//
// The shape, and why each piece is here:
//
//   - The whole draft after the `!` is the shell's one `-c` argument, so a
//     multi-line paste is one script rather than a line craze has to parse.
//   - Setsid, not Setpgid: the plan asks for a process group of the command's
//     own, and a new session is that plus a detachment from craze's controlling
//     terminal. Without it a child that opens /dev/tty and reads would take
//     SIGTTIN and stop, which craze could only wait out; with it the open fails
//     and the command reports that itself.
//   - stdin is /dev/null. Shell mode is not a terminal (§4): a command that
//     wants to be answered sees EOF instead of hanging on a terminal craze is
//     drawing over.
//   - One writer for both streams. exec reuses stdout's pipe for stderr when
//     the two are the same value, so the child's own interleaving is what craze
//     shows, one goroutine drains it, and the ring below is the only buffer.
//   - The group is killed on *every* ending, a normal exit included, so
//     `sleep 300 &` does not outlive the shell that started it.
func runShellCommand(ctx context.Context, script, dir string) shellResult {
	ring := &shellRing{}
	null, err := os.OpenFile(os.DevNull, os.O_RDONLY, 0)
	if err != nil {
		return shellResult{start: err}
	}
	defer func() { _ = null.Close() }()

	cmd := exec.Command(userShell(), "-c", script)
	cmd.Dir = dir
	cmd.Env = os.Environ()
	cmd.Stdin = null
	cmd.Stdout = ring
	cmd.Stderr = ring
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.WaitDelay = shellWaitDelay
	if err := cmd.Start(); err != nil {
		return shellResult{start: err}
	}
	// A session leader's pid is its session's and its process group's id. What
	// craze signals is the group: a descendant that made a group of its own
	// (setsid(1), a daemon, `set -m`) has left it and is not reachable, exactly
	// as internal/acp's group shutdown accepts (plan 019 §9). Stated, not
	// solved.
	pgid := cmd.Process.Pid
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()

	res := superviseShell(ctx, pgid, waited)
	// Unconditional, and last: whatever is left of the group goes now.
	//
	// The leader has usually been reaped by the Wait above, which frees its pid
	// — and with it, once the group is empty, the group id. A process given
	// that pid in the microseconds between the reap and this line, which had
	// also already made itself a group leader, would take this signal. It is
	// the window opencode's runner documents and internal/acp's shutdown
	// accepts; closing it costs a waitid(WNOWAIT) dance that this runner, which
	// signals once at the end of one foreground command, does not earn.
	signalShellGroup(pgid, syscall.SIGKILL)
	res.out = shellOutput(ring)
	return res
}

// superviseShell waits for the command and ends it when it must. It returns
// with the leader reaped, or — only where SIGKILL did not end it inside
// shellKillWait — having given up on it, which leaves the Wait goroutine to
// reap it whenever the kernel lets go.
//
// So it returns at most shellTermGrace + shellKillWait after a cancel or the
// timeout, and at most shellWaitDelay after the leader exits by itself.
func superviseShell(ctx context.Context, pgid int, waited <-chan error) shellResult {
	timeout := time.NewTimer(shellTimeout)
	defer timeout.Stop()
	var (
		done   = ctx.Done()
		expire = timeout.C
		grace  <-chan time.Time // SIGTERM sent: SIGKILL when it fires
		limit  <-chan time.Time // the shutdown's own deadline: stop waiting when it fires
		why    = shellExited
	)
	// stop is the first signal of a shutdown, and it happens once: a cancel
	// during a timeout's grace, or a second Esc, must not restart the clock.
	stop := func(reason shellEnding) {
		why = reason
		done, expire = nil, nil
		// A stopped process acts on SIGTERM only once it runs again.
		signalShellGroup(pgid, syscall.SIGTERM)
		signalShellGroup(pgid, syscall.SIGCONT)
		grace = time.After(shellTermGrace)
		limit = time.After(shellTermGrace + shellKillWait)
	}
	for {
		select {
		case err := <-waited:
			return shellResult{exit: shellExitCode(err), why: why}
		case <-done:
			stop(shellKilled)
		case <-expire:
			stop(shellTimedOut)
		case <-grace:
			grace = nil
			signalShellGroup(pgid, syscall.SIGKILL)
		case <-limit:
			return shellResult{exit: -1, why: shellAbandoned}
		}
	}
}

// signalShellGroup sends sig to every process still in the command's group. Its
// error — ESRCH once the group is empty — is nothing to act on.
func signalShellGroup(pgid int, sig syscall.Signal) { _ = syscall.Kill(-pgid, sig) }

// shellExitCode reads Wait's answer. A signalled process has no exit status, so
// it reports -1, which is what the row draws as "killed" rather than as an exit
// code; ErrWaitDelay means the leader exited cleanly and a descendant was still
// holding the pipe, which is the command's business and not a failure.
func shellExitCode(err error) int {
	var ee *exec.ExitError
	switch {
	case err == nil, errors.Is(err, exec.ErrWaitDelay):
		return 0
	case errors.As(err, &ee):
		return ee.ExitCode()
	default:
		return -1
	}
}

// shellOutput is the command's output as the transcript and, from C9, the agent
// will see it: sanitised the way agent text is at ingestion, and led by a note
// where the ring dropped the head.
func shellOutput(r *shellRing) string {
	raw, dropped := r.text()
	out := sanitizeShellOutput(raw)
	if !dropped {
		return out
	}
	return shellTruncNote + "\n" + out
}

// sanitizeShellOutput folds command output into text craze can draw and hand
// on: escape sequences go, control characters go, invalid UTF-8 goes — the same
// rules internal/agent's sanitizeText applies to everything an agent says,
// applied here because output is equally untrusted input. Newlines and tabs
// survive, because they are the shape of the output.
//
// Invalid bytes are dropped rather than replaced: the tail of a ring almost
// always starts mid-rune, and a truncated command's first character should not
// be a replacement mark for a rune craze cut in half itself.
func sanitizeShellOutput(s string) string {
	if s == "" {
		return ""
	}
	s = strings.ToValidUTF8(ansi.Strip(s), "")
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\n' || r == '\t':
			b.WriteRune(r)
		case unicode.IsControl(r):
			// Every other C0 and C1 control, the lone \r of a progress line
			// included: it would redraw the row craze already drew.
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// shellRing is the output buffer: a fixed shellOutputCap bytes that keeps the
// tail. It is a ring and not a growing buffer because the cap has to hold
// against `yes` — output arrives as it is produced, and a buffer that trimmed
// after the fact would have allocated the flood first.
//
// It is written by exec's copying goroutine and read once the command is over.
// Wait joins that goroutine before it returns (os/exec's awaitGoroutines), so
// the two do not overlap on the ordinary path; the lock is for the one that
// does, where craze gave up on a leader SIGKILL could not end and reads the
// output it has while the goroutine may still be appending to it.
type shellRing struct {
	mu   sync.Mutex
	buf  [shellOutputCap]byte
	n    int // bytes held, up to len(buf)
	w    int // where the next byte goes
	over bool
}

func (r *shellRing) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	total := len(p)
	if len(p) >= len(r.buf) {
		// One write bigger than the whole ring: only its tail can survive it.
		r.over = true
		copy(r.buf[:], p[len(p)-len(r.buf):])
		r.n, r.w = len(r.buf), 0
		return total, nil
	}
	if r.n+len(p) > len(r.buf) {
		r.over = true
	}
	for len(p) > 0 {
		k := copy(r.buf[r.w:], p)
		p = p[k:]
		r.w = (r.w + k) % len(r.buf)
		r.n = min(r.n+k, len(r.buf))
	}
	return total, nil
}

// text is what the ring holds, oldest byte first, and whether anything was
// dropped to make room for it.
func (r *shellRing) text() (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.n < len(r.buf) {
		return string(r.buf[:r.n]), r.over
	}
	return string(r.buf[r.w:]) + string(r.buf[:r.w]), r.over
}
