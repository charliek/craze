package acp

import (
	"bytes"
	"strconv"
	"syscall"
	"time"
)

// The agent's reaper (plan 032 §3.2a, SF-80).
//
// A process group's id is its leader's pid, and a pid is free for the kernel
// to hand out again once its process has been reaped. A signal to -pgid sent
// after the agent was reaped can therefore reach a stranger's group. So the
// agent is reaped last: one goroutine, the reaper, owns cmd.Wait and sends
// every group signal, and it reaps only after its last signal. Until then the
// agent is unreaped — running, or a zombie — and its pid, which is the
// group's id, cannot be reused.
//
// The reaper's steps:
//
//  1. Observing. It waits for whichever comes first: the agent's exit,
//     observed without reaping it (watchExit: waitid(WEXITED|WNOWAIT) on
//     Linux, a kqueue NOTE_EXIT on macOS, on a goroutine of its own that only
//     reports), or a shutdown request (Shutdown). The exit's status, from
//     that observation, is recorded and exitedCh closed.
//  2. Escalation, for a shutdown request while the agent runs: SIGTERM to the
//     group; past the grace without the exit, SIGKILL to the group, and the
//     exit waited for — SIGKILL cannot be caught or ignored, so it follows as
//     soon as the kernel lets the agent go. A leader in an uninterruptible
//     wait holds the reaper until then, exactly as cmd.Wait always has:
//     nothing is left to send, and it cannot be reaped before it exits.
//  3. Group cleanup, the zombie pinning the group's id. While the group has a
//     live member — a process in it that is not a zombie (groupHasLiveMember;
//     the zombie agent itself still answers kill(-pgid, 0), so that cannot
//     tell) — SIGTERM to the group, a poll every groupPoll up to the grace,
//     SIGKILL, and a poll up to the grace again. An agent that exits on its
//     own goes through this too, so the tools it leaves running die at its
//     exit, not at a later Close. A group whose only member is the zombie
//     costs no signal and no wait.
//  4. Reap: cmd.Wait, whose answer says what step 1 recorded, and reapedCh
//     closed. Nothing is sent after it. cmd.Wait closes craze's end of the
//     agent's stderr pipe, so the copy of it is first given stderrDrain to
//     read what the group wrote last (drainStderr).
//
// Where the exit cannot be observed without reaping — watchExit failed: a
// waitid refused under a seccomp filter or an emulator (ENOSYS), a kqueue
// that could not be had — the reaper falls back to reapUnobserved.

// observation is what watchExit's wait reports: the agent's wait status, or
// why it could not be observed.
type observation struct {
	status syscall.WaitStatus
	err    error
}

// killGroup sends sig to the process group pgid. Only the reaper calls it. It
// is a variable so a test can see every group signal and when it came against
// the reap (TestEveryGroupSignalPrecedesTheReap); nothing else replaces it.
var killGroup = func(pgid int, sig syscall.Signal) error { return syscall.Kill(-pgid, sig) }

// reap is the reaper: the four steps above. observe is watchExit's wait.
func (ch *Child) reap(observe func() (syscall.WaitStatus, error)) {
	observed := make(chan observation, 1)
	go func() {
		st, err := observe()
		observed <- observation{status: st, err: err}
	}()
	var obs observation
	select {
	case obs = <-observed:
	case <-ch.shutdownCh:
		obs = ch.escalate(observed)
	}
	if obs.err != nil {
		ch.reapUnobserved()
		return
	}
	ch.exitErr = exitErrorOf(obs.status)
	close(ch.exitedCh)
	ch.cleanGroup()
	ch.drainStderr()
	ch.waitErr = ch.cmd.Wait()
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

// cleanGroup is step 3: the agent has exited, unreaped.
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
// the reap, whose cmd.Wait closes the read end under the copy: a line a tool
// wrote as the cleanup ended it would otherwise be lost. Once the group has
// been cleaned up the end comes at once; only a process that left the group
// (setsid) can still hold the pipe, and the bound is for that.
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
func (ch *Child) signal(sig syscall.Signal) { _ = killGroup(ch.pgid, sig) }

// reapUnobserved is the reaper where the exit could not be observed without
// reaping it: the reap is then the observation, cmd.Wait on a goroutine of its
// own, and no signal goes to the group at all — the id is not pinned, so a
// group signal could follow the reap. A shutdown request signals the agent
// alone, through its os.Process, which refuses a signal once the process is
// reaped (a pidfd on Linux): SIGTERM, and SIGKILL past the grace. What the
// agent left running in its group is not cleaned up, and its stderr is not
// drained — the cost of this fallback, which only an environment refusing
// the observation pays.
func (ch *Child) reapUnobserved() {
	reaped := make(chan struct{})
	go func() {
		ch.waitErr = ch.cmd.Wait()
		close(reaped)
	}()
	select {
	case <-reaped:
	case <-ch.shutdownCh:
		_ = ch.cmd.Process.Signal(syscall.SIGTERM)
		grace := time.NewTimer(shutdownGrace)
		select {
		case <-reaped:
		case <-grace.C:
			_ = ch.cmd.Process.Signal(syscall.SIGKILL)
			<-reaped
		}
		grace.Stop()
	}
	ch.exitErr = ch.waitErr
	close(ch.exitedCh)
	close(ch.reapedCh)
}

// parseStatGroup reads a Linux /proc/<pid>/stat line's state (field 3) and
// process group (field 5). The command, field 2, is in parentheses and may
// hold anything — spaces and parentheses included — so it ends at the line's
// last ")"; it is at most 64 bytes, so a read of the line's first 512 holds it
// whole, and the fields after it are numbers. It lives here, not in the Linux
// file, so every platform's tests check it.
func parseStatGroup(line []byte) (state byte, pgrp int, ok bool) {
	i := bytes.LastIndexByte(line, ')')
	if i < 0 {
		return 0, 0, false
	}
	f := bytes.Fields(line[i+1:]) // state, ppid, pgrp, …
	if len(f) < 3 || len(f[0]) != 1 {
		return 0, 0, false
	}
	n, err := strconv.Atoi(string(f[2]))
	if err != nil {
		return 0, 0, false
	}
	return f[0][0], n, true
}
