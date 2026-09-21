package tui

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
	"unsafe"

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
// channel, a tool.Env, a spill file and a redactor it needs because it answers
// to a model rather than to the person at the keyboard. The composer runs one
// foreground command at a time for one human, and the shape that fits it is
// small enough that sharing would cost more than it saved. What is shared is
// the reasoning — the unreaped-leader dance below is that runner's, ported
// rather than reinvented — so the comments say where the two differ and why.

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
	// long as `sleep 300 &` lives. By the time Wait runs the group has had its
	// last SIGKILL (runShellCommand), so only a process that left the group can
	// still be holding the pipe. (opencode's runner bounds the same thing with
	// a drain timer of its own, because it owns the pipe; cmd.WaitDelay is used
	// nowhere else in this repo, so this much is new code rather than a port.)
	shellWaitDelay = 2 * time.Second
	// shellShutdownWait bounds a caller that waits for the runner to finish —
	// a quit path. The runner's own shutdown is bounded by shellTermGrace +
	// shellKillWait, and the reap that follows it by shellWaitDelay, so this
	// only has to exceed the three together.
	shellShutdownWait = shellTermGrace + shellKillWait + shellWaitDelay + time.Second
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
	// disowned is the last run whose output is no longer the composer's to
	// hand on; see disown. The zero value disowns nothing, because gen starts
	// at 1.
	disowned int
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

// disown gives up what every run started so far printed: those results may
// still settle the rows they opened, and none of them is context for anything
// the user sends next.
//
// It is the session change's, and it is a mark on the run rather than a moment
// in time on purpose. A command killed by a session change goes on running for
// as long as its group takes to die, and its shellDoneMsg therefore lands in a
// later Update than the one that changed sessions — after dropShellContext has
// cleared what was pending. Without this the result would be kept then, and the
// *last* session's command output would lead the *next* session's first prompt:
// content crossing a boundary the user was shown closing. A guard that read
// "has the session changed since?" would be the same ordering assumption in
// another shape; a run either belongs to the composer's current session or it
// does not, and its number says which (plan 022 A18).
func (c *shellController) disown() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.disowned = c.gen
	c.mu.Unlock()
}

// keepsContext reports that gen's output is still context for the next message:
// it was run for the session the composer is in now. A model without a
// controller has started nothing through one, so nothing it is handed can have
// been disowned.
func (c *shellController) keepsContext(gen int) bool {
	if c == nil {
		return true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return gen > c.disowned
}

// shutdown is cancel for a caller that must not leave the process group behind
// it: a quit path. It waits for the run's goroutine, which has already sent the
// group its last SIGKILL by the time it returns. The wait is bounded twice over
// — by the runner's own deadline and by shellShutdownWait — because a quit that
// hangs is worse than a stray process.
//
// It blocks for seconds in the worst case, so it belongs on a goroutine of its
// own and never inside Update: a caller that runs on bubbletea's one loop calls
// cancel and lets the run's own goroutine finish the killing (setSession).
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
//     `sleep 300 &` does not outlive the shell that started it — and that last
//     signal goes out before the leader is reaped, so it can never land on a
//     stranger (shellGroup.watch).
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
	g, err := startShellGroup(cmd)
	if err != nil {
		return shellResult{start: err}
	}

	why, exited := superviseShell(ctx, g)
	// Unconditional, and while the leader is still unreaped: whatever is left
	// of the group goes now.
	//
	// What craze signals is the group, and a group's id is its leader's pid. A
	// pid is free for reuse once its process has been reaped, so a kill sent
	// after the reap can reach a group a stranger has since been given the id
	// for — which is why nothing has reaped the leader yet (watch). A
	// descendant that made a session or a group of its own (setsid(1), a
	// daemon, `set -m`) has left this group and is not reachable, exactly as
	// internal/acp's group shutdown accepts (plan 019 §9): stated, not solved,
	// here and in docs/reference/tui.md's own words.
	g.signal(syscall.SIGKILL)
	// The leader may be reaped, on every path and not only the ones that waited
	// for it: an abandoned run that never released it would leave its watcher
	// blocked on this channel for good, and a zombie holding the pid with it.
	close(g.release)

	res := shellResult{exit: -1, why: why}
	if exited {
		// Immediate on the ordinary path — the leader is already a zombie, and
		// everything else in its group has just been killed, so the pipe is
		// closed — and bounded by cmd.WaitDelay where something that left the
		// group is still holding the output open: exec closes the descriptors
		// itself once its own delay passes.
		<-g.reaped
		res.exit = shellExitCode(g.err)
	}
	res.out = shellOutput(ring)
	return res
}

// shellGroup is a started command: the shell craze ran, and the session and the
// process group it leads. Setsid made the shell the leader of both, with its own
// pid as their id and no controlling terminal; everything it starts is in both
// unless it leaves.
type shellGroup struct {
	cmd *exec.Cmd
	pid int // the leader's, and so the group's and the session's id

	// exited is closed once the leader has exited. Where pinned, it is then an
	// unreaped zombie until release is closed (watch).
	exited chan struct{}
	// pinned reports that the leader is held unreaped. Written before exited
	// closes, so a reader that has waited for that channel sees it.
	pinned bool
	// release is closed by the run once it has sent the group its last signal:
	// the leader may be reaped.
	release chan struct{}
	// reaped is closed once the leader has been reaped; err is set then.
	reaped chan struct{}
	err    error
}

// startShellGroup starts cmd, which must set SysProcAttr.Setsid, and watches
// its leader.
func startShellGroup(cmd *exec.Cmd) (*shellGroup, error) {
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	g := &shellGroup{
		cmd:     cmd,
		pid:     cmd.Process.Pid,
		exited:  make(chan struct{}),
		release: make(chan struct{}),
		reaped:  make(chan struct{}),
	}
	go g.watch()
	return g, nil
}

// watch learns that the leader has exited, and reaps it — in that order, and
// not before the run has sent the group its last signal.
//
// This is opencode's watch (internal/harness/tool/opencode/process.go), and it
// is here for its reason: a process group's id is its leader's pid, and a pid
// is free for reuse the moment its process is reaped and nothing else holds it.
// Reaping first and signalling after would mean that a SIGKILL craze sends to
// what it believes is its own empty group can land on a stranger's group that
// was given the id in between. So on Linux this learns of the exit without
// reaping (waitNoReap) and reaps only once release is closed: until then the
// zombie leader holds the id, and the last signal reaches this command's group
// or no process at all.
//
// Where it cannot wait without reaping — macOS, or a Linux waitid that fails —
// reaping is how it learns that the leader has exited, and the last signal
// follows the reap. That leaves the window open there, accepted and narrow: the
// group must already be empty, since any live member keeps the id taken, and
// macOS hands out pids in sequence up to 99999, so the counter would have to go
// round the whole pid space inside the microseconds between the two lines. It
// is the window internal/acp/spawn.go's group shutdown accepts.
func (g *shellGroup) watch() {
	defer close(g.reaped)
	g.pinned = waitNoReap(g.pid)
	if g.pinned {
		close(g.exited)
		<-g.release
	}
	// exec's own Wait: it reaps the leader — immediately, the process being a
	// zombie by now on the pinned path — and joins the goroutine copying the
	// output into the ring, which cmd.WaitDelay bounds.
	g.err = g.cmd.Wait()
	if !g.pinned {
		close(g.exited)
	}
}

// pPID is waitid's idtype for "the process with this pid", P_PID.
const pPID = 1

// waitNoReap blocks until the process pid has exited, and leaves it unreaped:
// waitid(P_PID, pid, WEXITED|WNOWAIT), which is how os.Process.Wait itself
// waits on Linux before it reaps (os/wait_waitid.go). It reports false, having
// waited for nothing, where that is not reliable — macOS's waitid also returns
// for a stopped process (go.dev/issue/19314), which a command that stopped
// itself would be until superviseShell's SIGCONT — and when the call fails with
// anything but EINTR (ENOSYS under a seccomp filter or an emulator, say).
//
// That fallback is safe: watch then learns of the exit by reaping, as on macOS,
// so nothing hangs and nothing is signalled that would not be on macOS; only
// the pid-reuse window watch describes reopens. Ported from opencode's runner
// unchanged, comment included, because a copy that drifted from it would be a
// second answer to the same kernel question.
func waitNoReap(pid int) bool {
	if runtime.GOOS != "linux" {
		return false
	}
	var info [16]uint64 // a siginfo_t, 128 bytes; nothing reads it
	for {
		_, _, errno := syscall.Syscall6(syscall.SYS_WAITID, pPID, uintptr(pid),
			uintptr(unsafe.Pointer(&info)), syscall.WEXITED|syscall.WNOWAIT, 0, 0)
		switch errno {
		case 0:
			return true
		case syscall.EINTR:
			continue
		}
		return false
	}
}

// superviseShell waits for the command and ends it when it must. It returns how
// the command ended, and whether the leader has exited: where it has not —
// SIGKILL did not end it inside shellKillWait — craze gives up on it, and the
// watcher reaps it whenever the kernel lets go.
//
// So it returns as soon as the leader exits by itself, and at most
// shellTermGrace + shellKillWait after a cancel or the timeout.
//
// Giving up leaves the watcher goroutine behind, blocked where the kernel has
// it, and with it the goroutine copying the output and the pipe's two
// descriptors. Nothing can shorten that: a wait on a pid cannot be cancelled,
// and never reaping at all would hold the pid for as long as craze runs. What
// it costs is bounded per run — one goroutine, one pipe, and the fixed
// shellOutputCap bytes of the ring the copy may go on writing into — and it is
// reached only where SIGKILL did not end the leader, which means a process
// stuck in the kernel (uninterruptible sleep on a dead mount, say). Such a
// process ends when its syscall does and takes the goroutine with it, so what
// would accumulate is a box where command after command hangs that way, and
// there the stuck commands are the problem craze is reporting rather than the
// goroutines waiting on them.
func superviseShell(ctx context.Context, g *shellGroup) (shellEnding, bool) {
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
		g.signal(syscall.SIGTERM)
		g.signal(syscall.SIGCONT)
		grace = time.After(shellTermGrace)
		limit = time.After(shellTermGrace + shellKillWait)
	}
	for {
		select {
		case <-g.exited:
			return why, true
		case <-done:
			stop(shellKilled)
		case <-expire:
			stop(shellTimedOut)
		case <-grace:
			grace = nil
			g.signal(syscall.SIGKILL)
		case <-limit:
			return shellAbandoned, false
		}
	}
}

// signal sends sig to every process still in the command's group — a negative
// pid is the group's id, which is the leader's. Its error — ESRCH once the
// group is empty — is nothing to act on.
func (g *shellGroup) signal(sig syscall.Signal) { _ = syscall.Kill(-g.pid, sig) }

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

// shellOutput is the command's output as the transcript and the agent both see
// it — the row draws this string and the shell context block carries it
// (shell_context.go) — sanitised the way agent text is at ingestion, and led by
// a note where the ring dropped the head.
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
		// One write at least as big as the whole ring: only its tail can
		// survive it. Nothing is *dropped*, though, where the write is exactly
		// the ring and the ring was empty — all of it is kept, and saying
		// otherwise would lead the output with a truncation note for bytes
		// nobody lost.
		if len(p) > len(r.buf) || r.n > 0 {
			r.over = true
		}
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
