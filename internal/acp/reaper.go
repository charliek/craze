package acp

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"
)

// The agent's reaper (plan 032 §3.2a, SF-80).
//
// A process group's id is its leader's pid, and a pid is free for the kernel
// to hand out again once its process has been reaped. A signal to -pgid sent
// after the agent was reaped can therefore reach a stranger's group. So the
// agent is reaped last: one goroutine, the reaper, owns the agent's reap and
// sends every signal to it and its group, and it reaps only after its last
// signal. Until then the agent is unreaped — running, or a zombie — and its
// pid, which is the group's id, cannot be reused.
//
// The reaper's steps:
//
//  1. Observing. It waits for whichever comes first: the agent's exit,
//     observed without reaping it (watchExit: waitid(WEXITED|WNOWAIT) on
//     Linux, a kqueue NOTE_EXIT on macOS, on a goroutine of its own that only
//     reports), or a shutdown request (Shutdown). exitedCh is closed, and the
//     exit's status recorded (statusCh) when the observation carries it.
//  2. Escalation, for a shutdown request while the agent runs: SIGTERM to the
//     group; past the grace without the exit, SIGKILL to the group, and the
//     exit waited for — SIGKILL cannot be caught or ignored, so it follows as
//     soon as the kernel lets the agent go. A leader in an uninterruptible
//     wait holds the reaper until then, exactly as cmd.Wait always has:
//     nothing is left to send, and it cannot be reaped before it exits.
//  3. Group cleanup, the zombie pinning the group's id. While the group has a
//     live member — a process in it that is not a zombie, or a zombie leader
//     of threads still running (groupHasLiveMember; the zombie agent itself
//     still answers kill(-pgid, 0), so that cannot tell) — SIGTERM to the group, a poll every groupPoll up to the grace,
//     SIGKILL, and a poll up to the grace again. An agent that exits on its
//     own goes through this too, so the tools it leaves running die at its
//     exit, not at a later Close. A group whose only member is the zombie
//     costs no signal and no wait. Then one last SIGKILL to the group,
//     whatever the scans said: a scan is not atomic (Linux reads /proc a
//     process at a time, so a member can fork after the listing and exit
//     before its own read), and the zombie still pins the id.
//  4. Reap: cmd.Wait, whose answer says what step 1 recorded (or, when step 1
//     had no status, is where the status comes from), and reapedCh closed.
//     Nothing is sent after it. cmd.Wait closes craze's end of the agent's
//     stderr pipe, so the copy of it is first given stderrDrain to read what
//     the group wrote last (drainStderr).
//
// Where the exit cannot be observed without reaping — the observation
// refused (a waitid refused under a seccomp filter or an emulator, ENOSYS),
// no kqueue, or a macOS exit watch whose zombie queries keep failing — the
// reaper is reapPolling, which reaps with wait4 and never signals the group.
// newChild settles which before the reaper runs, so before any signal. An
// observation that fails later hands over to it too; any signal sent before
// that went while the agent was unreaped.

// observation is what watchExit's wait reports: the agent's exit, with its
// wait status when known (the observation carried it), or why it could not
// be observed.
type observation struct {
	status syscall.WaitStatus
	known  bool
	err    error
}

// watch is watchExit: a variable so a test can change what the observation
// reports (an exit with no status); nothing else replaces it.
var watch = watchExit

// sendSignal is kill(2): pid, or -pgid for a process group. Only the reaper
// calls it. It is a variable so a test can see every signal and when it came
// against the reap (TestEveryGroupSignalPrecedesTheReap); nothing else
// replaces it.
var sendSignal = func(pid int, sig syscall.Signal) error { return syscall.Kill(pid, sig) }

// wait4 is the fallback's wait4(2), never blocking: a variable so a test can
// see the reap it makes against the signals; nothing else replaces it.
var wait4 = func(pid int, ws *syscall.WaitStatus, options int) (int, error) {
	return syscall.Wait4(pid, ws, options, nil)
}

// fallbackEntered, a test seam, runs as the reaper enters reapPolling,
// before its first wait4; nil in production.
var fallbackEntered func(pid int)

// finalWait is the fallback's cmd.Wait, after its own reap and the
// Process's release: a variable so a test can see that wait wait on no pid;
// nothing else replaces it.
var finalWait = func(cmd *exec.Cmd) error { return cmd.Wait() }

// reap is the reaper: the four steps above. observe is watchExit's wait.
func (ch *Child) reap(observe func() (syscall.WaitStatus, bool, error)) {
	observed := make(chan observation, 1)
	go func() {
		st, known, err := observe()
		observed <- observation{status: st, known: known, err: err}
	}()
	var obs observation
	select {
	case obs = <-observed:
	case <-ch.shutdownCh:
		obs = ch.escalate(observed)
	}
	if obs.err != nil {
		ch.reapPolling()
		return
	}
	ch.endedByShutdown = isClosed(ch.shutdownCh)
	if obs.known {
		ch.setStatus(exitErrorOf(obs.status))
	}
	close(ch.exitedCh)
	ch.cleanGroup()
	ch.signal(syscall.SIGKILL)
	ch.drainStderr()
	ch.waitErr = ch.cmd.Wait()
	if !obs.known {
		ch.setStatus(reapedExit(ch.cmd.ProcessState, ch.waitErr))
	}
	close(ch.reapedCh)
}

// escalate is step 2: the agent still runs and has been asked to stop.
func (ch *Child) escalate(observed <-chan observation) observation {
	ch.signal(syscall.SIGTERM)
	grace := time.NewTimer(shutdownGrace)
	defer grace.Stop()
	select {
	case obs := <-observed:
		return obs
	case <-grace.C:
	}
	ch.signal(syscall.SIGKILL)
	return <-observed
}

// cleanGroup is step 3, up to its last SIGKILL: the agent has exited,
// unreaped.
func (ch *Child) cleanGroup() {
	if !ch.groupLive() {
		return
	}
	ch.signal(syscall.SIGTERM)
	if ch.groupDrained(shutdownGrace) {
		return
	}
	ch.signal(syscall.SIGKILL)
	ch.groupDrained(shutdownGrace)
}

// groupDrained polls, every groupPoll for up to d, for the agent's group to
// have no live member left: whether it got there.
func (ch *Child) groupDrained(d time.Duration) bool {
	deadline := time.Now().Add(d)
	for {
		time.Sleep(groupPoll)
		if !ch.groupLive() {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
	}
}

// groupLive reports whether the agent's group has a live member. A group that
// cannot be looked at counts as live: signalling it is safe while the agent
// is unreaped, and leaving a member running is not.
func (ch *Child) groupLive() bool {
	live, err := groupHasLiveMember(ch.pgid)
	return live || err != nil
}

// stderrDrain bounds drainStderr's wait.
const stderrDrain = 500 * time.Millisecond

// drainStderr waits, up to stderrDrain, for the copy of the agent's stderr to
// reach its end — every process that held the pipe's write end gone — before
// cmd.Wait closes the read end under the copy: a line a tool wrote as the
// cleanup ended it would otherwise be lost. Once the group has been cleaned
// up the end comes at once; only a process that left the group (setsid) can
// still hold the pipe, and the bound is for that.
func (ch *Child) drainStderr() {
	drain := time.NewTimer(stderrDrain)
	defer drain.Stop()
	select {
	case <-ch.stderrDone:
	case <-drain.C:
	}
}

// signal sends sig to the agent's process group. Its error — ESRCH once
// nothing is left in the group but the zombie — is nothing to act on.
func (ch *Child) signal(sig syscall.Signal) { _ = sendSignal(-ch.pgid, sig) }

// reapedExit is how the agent ended as its reap says: the status cmd.Wait
// collected, or — no status collected — cmd.Wait's own error.
func reapedExit(ps *os.ProcessState, waitErr error) error {
	if ps != nil {
		if st, ok := ps.Sys().(syscall.WaitStatus); ok {
			return exitErrorOf(st)
		}
	}
	return waitErr
}

// reapPoll is how often reapPolling asks wait4 whether the agent has exited.
const reapPoll = 50 * time.Millisecond

// reapPolling is the reaper where the exit cannot be observed without
// reaping it. It stays the only owner of the agent's reap and its signals:
// it polls wait4(pid, WNOHANG) itself — the exit, its status and the reap in
// one call — and sends a shutdown's signals only between its own polls,
// right after one found the agent unreaped, so no signal can follow the
// reap: SIGTERM, and SIGKILL past the grace, to the agent alone. The group
// is never signalled here — nothing holds its id once the poll that finds
// the exit has reaped it — so what the agent left running in it is not
// cleaned up, the cost of this fallback. Once reaped, the stderr copy is
// drained, cmd.Process released — so no wait on the pid follows the reap —
// and cmd.Wait called for what it still owns, the pipes and their
// goroutines; its error, the released Process's, is expected.
//
// A wait4 that fails (an environment that refuses it as well, under a
// seccomp filter) leaves the reap to cmd.Wait, with no signal at all: such an
// environment cannot be waited for safely, and a cmd.Wait that cannot either
// returns at once, its error then standing for the exit — the agent may
// still be there, as it always could before this reaper (accepted).
func (ch *Child) reapPolling() {
	if fallbackEntered != nil {
		fallbackEntered(ch.pid)
	}
	var (
		termed  time.Time
		killed  bool
		request = ch.shutdownCh
	)
	for {
		st, reaped, err := pollReap(ch.pid)
		if err != nil {
			ch.waitErr = ch.cmd.Wait()
			ch.endedByShutdown = isClosed(ch.shutdownCh)
			ch.setStatus(reapedExit(ch.cmd.ProcessState, ch.waitErr))
			close(ch.exitedCh)
			close(ch.reapedCh)
			return
		}
		if reaped {
			ch.endedByShutdown = isClosed(ch.shutdownCh)
			ch.setStatus(exitErrorOf(st))
			close(ch.exitedCh)
			break
		}
		// Unreaped, and only this goroutine reaps it: a signal now reaches
		// the agent, or its zombie.
		switch {
		case termed.IsZero() && isClosed(ch.shutdownCh):
			_ = sendSignal(ch.pid, syscall.SIGTERM)
			termed, request = time.Now(), nil
		case !termed.IsZero() && !killed && time.Since(termed) >= shutdownGrace:
			_ = sendSignal(ch.pid, syscall.SIGKILL)
			killed = true
		}
		poll := time.NewTimer(reapPoll)
		select {
		case <-poll.C:
		case <-request:
		}
		poll.Stop()
	}
	ch.drainStderr()
	// The agent is reaped: no wait on its pid may follow, for the pid may be
	// another child's of this process by now, and a second wait would block
	// on that child or take its status from its own reaper. Released first,
	// cmd.Process answers cmd.Wait's process wait at once (EINVAL), with no
	// system call, and cmd.Wait only awaits its copies and closes the pipes.
	_ = ch.cmd.Process.Release()
	_ = finalWait(ch.cmd)
	ch.waitErr = ch.exitErr
	close(ch.reapedCh)
}

// pollReap asks wait4(pid, WNOHANG), EINTR retried, whether the agent has
// exited; if it has, the call reaped it and st is its status.
func pollReap(pid int) (st syscall.WaitStatus, reaped bool, err error) {
	for {
		got, err := wait4(pid, &st, syscall.WNOHANG)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if err != nil {
			return 0, false, err
		}
		return st, got == pid, nil
	}
}

// isClosed reports whether ch is closed, without waiting.
func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// procStat is what the reaper reads of a Linux /proc/<pid>/stat line.
type procStat struct {
	state   byte // field 3
	pgrp    int  // field 5
	threads int  // field 20, num_threads
}

// parseStat reads a Linux /proc/<pid>/stat line's state, process group and
// thread count. The command, field 2, is in parentheses and may hold anything
// — spaces and parentheses included — so it ends at the line's last ")"; it
// is at most 64 bytes, and the fields after it are numbers, so a read of the
// line's first 1024 bytes holds field 20. It lives here, not in the Linux
// file, so every platform's tests check it.
func parseStat(line []byte) (procStat, bool) {
	i := bytes.LastIndexByte(line, ')')
	if i < 0 {
		return procStat{}, false
	}
	f := bytes.Fields(line[i+1:]) // field 3 on
	if len(f) < 18 || len(f[0]) != 1 {
		return procStat{}, false
	}
	pgrp, err := strconv.Atoi(string(f[2]))
	if err != nil {
		return procStat{}, false
	}
	threads, err := strconv.Atoi(string(f[17]))
	if err != nil {
		return procStat{}, false
	}
	return procStat{state: f[0][0], pgrp: pgrp, threads: threads}, true
}
