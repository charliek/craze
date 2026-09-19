package opencode

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"syscall"
	"time"
	"unsafe"

	"github.com/charliek/craze/internal/harness/tool"
)

// A command's lifetime (plan 019 §3.9).
const (
	// termGrace is how long a command's process group has between SIGTERM
	// and SIGKILL: opencode's forceKillAfter (shell.ts:550).
	termGrace = 3 * time.Second
	// drainWait bounds the wait, once the leader has exited, for the output
	// pipe to close. Something the command left running may hold it open.
	drainWait = 2 * time.Second
	// killWait bounds the wait, after SIGKILL, for the shutdown to end — the
	// leader to exit and the output to close: a process in uninterruptible
	// sleep dies only when the sleep ends. After SIGTERM the bound is
	// termGrace + killWait, whenever within it the SIGKILL and the exit come.
	killWait = 2 * time.Second
)

// ending is how a command ended.
type ending int

const (
	endExit    ending = iota // the leader exited by itself
	endTimeout               // its timeout passed first
	endAbort                 // its call was cancelled first
)

// group is a started command: the shell craze ran, and the session and the
// process group it leads. Setsid made the shell the leader of a new session
// and of a new process group, both with the shell's pid as their id, with no
// controlling terminal; everything the command starts is in both unless it
// leaves. What craze signals is the process group: a process that starts its
// own session or process group (setsid, setpgid, `set -m`) has left it.
type group struct {
	cmd *exec.Cmd
	pid int // the leader's, and so the group's and the session's id

	// exited is closed once the leader has exited. On Linux it is then an
	// unreaped zombie until release is closed (see watch).
	exited chan struct{}
	// release is closed by supervise once it has sent the group its last
	// signal: the leader may be reaped.
	release chan struct{}
	// reaped is closed once the leader has been reaped; state is set then.
	reaped chan struct{}
	state  *os.ProcessState
}

// startGroup starts cmd, which must set SysProcAttr.Setsid, and watches its
// leader.
func startGroup(cmd *exec.Cmd) (*group, error) {
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	g := &group{
		cmd:     cmd,
		pid:     cmd.Process.Pid,
		exited:  make(chan struct{}),
		release: make(chan struct{}),
		reaped:  make(chan struct{}),
	}
	go g.watch()
	return g, nil
}

// watch learns that the leader has exited, and reaps it.
//
// A process group's id is its leader's pid, and a pid is free for reuse
// once its process is reaped and nothing else holds it. So on Linux watch
// learns of the exit without reaping (waitNoReap), and reaps only once
// supervise has sent the group its last SIGKILL: until then the zombie
// leader holds the id, and the signal reaches this command's group or no
// process at all, never a stranger's group that happened to get the id.
// Where it cannot wait without reaping — macOS, or a Linux waitid that fails
// — reaping is how it learns, and the last signal follows the reap.
//
// That leaves a window, accepted: once the leader is reaped, an empty group's
// id is free, and a new process given the same pid that made itself a group
// leader before the last SIGKILL (at most the 2 s drain wait later) would get
// that signal. The group must already be empty — while any member lives the
// id stays taken — and macOS hands out pids in sequence up to 99999, so the
// counter would have to go round the whole pid space within those 2 s. It is
// the window internal/acp/spawn.go's group shutdown accepts; on Linux,
// waitNoReap closes it.
func (g *group) watch() {
	defer close(g.reaped)
	pinned := waitNoReap(g.pid)
	if pinned {
		close(g.exited)
		<-g.release
	}
	_ = g.cmd.Wait() // a non-zero exit is an error here; state says what happened
	g.state = g.cmd.ProcessState
	if !pinned {
		close(g.exited)
	}
}

// pPID is waitid's idtype for "the process with this pid", P_PID.
const pPID = 1

// waitNoReap blocks until the process pid has exited, and leaves it
// unreaped: waitid(P_PID, pid, WEXITED|WNOWAIT), which is how os.Process.Wait
// itself waits on Linux before it reaps (os/wait_waitid.go). It reports
// false, having waited for nothing, where that is not reliable — macOS's
// waitid also returns for a stopped process (go.dev/issue/19314) — and when
// the call fails with anything but EINTR (ENOSYS under a seccomp filter or an
// emulator, say). That fallback is safe: watch then learns of the exit by
// reaping, as on macOS, so nothing hangs and nothing is signalled that
// would not be on macOS; only the pid-reuse window watch describes reopens.
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

// signal sends sig to every process in the command's group. Its error —
// ESRCH once the group is empty — is nothing to act on.
func (g *group) signal(sig syscall.Signal) { _ = syscall.Kill(-g.pid, sig) }

// terminate asks the group to stop. A stopped process acts on SIGTERM only
// once it runs again, so the group is continued too.
func (g *group) terminate() {
	g.signal(syscall.SIGTERM)
	g.signal(syscall.SIGCONT)
}

// supervise waits for the command, ends it when it must, and returns how it
// ended and whether its leader was reaped. closing is the session's close
// signal, Env.Closing (nil: never). output is closed once the command's
// output pipe has closed: everything that held it has exited or closed it.
//
// The rules are plan 019 §3.9's:
//
//   - A timeout or a cancel sends the group SIGTERM, then SIGKILL 3 s later
//     if the leader still runs.
//   - A close — ctx cancelled with tool.ErrClosing, or closing closed —
//     sends SIGKILL at once, whenever it comes: while the command runs,
//     during the grace after a timeout or an ordinary cancel, or during the
//     drain wait below, which it ends. Closing is watched in every state,
//     so a call already cancelled can still be closed (tool.Env.Closing).
//   - Once the leader has exited, for any reason, supervise waits at most
//     2 s for the output to close (not at all once the session is closing,
//     and no longer once the call is cancelled), then sends the group
//     SIGKILL, unconditionally: what the command left running in its group
//     dies with the call, whether it holds the pipe (`server &`) or not
//     (`server >/dev/null 2>&1 &`).
//   - A shutdown — everything from the first signal on — has one deadline,
//     set when it starts: 3 s + 2 s after a timeout or an ordinary cancel, 2 s
//     after a close. Whatever the state then, supervise stops waiting: a
//     leader that SIGKILL has not ended — a process in uninterruptible sleep
//     — is given up on, and watch reaps it whenever it does exit; a drain
//     wait that began late, because the leader took most of the deadline to
//     die, is cut short. The drain wait never restarts the clock.
//
// So supervise returns at most 3 s + 2 s after a timeout or an ordinary
// cancel, and at most 2 s after a close, whatever came before it and
// whatever the command does. What it cannot reach is a process that has left
// the group: one that called setsid (setsid(1), a daemon) or setpgid (a
// shell's job control, `set -m`) escapes, as it does internal/acp's group
// kill (plan 019 §9).
func (g *group) supervise(ctx context.Context, closing <-chan struct{}, timeout time.Duration, output <-chan struct{}) (why ending, reaped bool) {
	expiry := time.NewTimer(timeout + timeoutSlack)
	defer expiry.Stop()
	var (
		exited = g.exited
		done   = ctx.Done()
		closed = closing
		expire = expiry.C
		grace  <-chan time.Time // SIGTERM sent: SIGKILL when it fires
		limit  <-chan time.Time // the shutdown's deadline: stop waiting, whatever the state, when it fires
		end    time.Time        // when limit fires
		drain  <-chan time.Time // the leader has exited: stop waiting for the output when it fires
	)
	isClosing := func() bool { return tool.SessionClosing(ctx, closing) }
	// deadline sets the shutdown's deadline d from now, unless one sooner is
	// set already: a deadline only ever moves earlier.
	deadline := func(d time.Duration) {
		if t := time.Now().Add(d); end.IsZero() || t.Before(end) {
			end, limit = t, time.After(d)
		}
	}
	// kill and stop act only while the leader lives: every channel that
	// leads to them is dropped once it has exited. A close's kill brings the
	// deadline to the close's.
	kill := func(closing bool) {
		g.signal(syscall.SIGKILL)
		grace = nil
		if closing {
			deadline(killWait)
		}
	}
	stop := func(reason ending) {
		why, expire = reason, nil
		if isClosing() {
			kill(true)
			return
		}
		g.terminate()
		grace = time.After(termGrace)
		deadline(termGrace + killWait)
	}
wait:
	for exited != nil || output != nil {
		select {
		case <-exited:
			exited, expire, grace = nil, nil, nil
			if isClosing() {
				break wait
			}
			drain = time.After(drainWait) // limit, if set, stays: the drain does not restart the clock
		case <-output:
			output = nil
		case <-done:
			done = nil
			switch {
			case exited == nil:
				break wait // only the output was left to wait for
			case why == endExit:
				stop(endAbort)
			case isClosing():
				kill(true) // the session closed during a timeout's grace
			}
		case <-closed:
			closed = nil
			switch {
			case exited == nil:
				break wait // the drain wait: the close ends it
			case why == endExit:
				stop(endAbort) // running: stop sees the close, and kills
			default:
				kill(true) // the grace of a timeout or a cancel
			}
		case <-expire:
			stop(endTimeout)
		case <-grace:
			kill(false)
		case <-limit:
			break wait
		case <-drain:
			break wait
		}
	}
	g.signal(syscall.SIGKILL)
	close(g.release)
	if exited != nil {
		return why, false
	}
	<-g.reaped // at once: the leader has exited, and watch has been released
	return why, true
}
