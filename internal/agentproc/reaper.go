package agentproc

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

type observation struct {
	status syscall.WaitStatus
	known  bool
	err    error
}

var watch = watchExit

var sendSignal = func(pid int, sig syscall.Signal) error { return syscall.Kill(pid, sig) }

var wait4 = func(pid int, ws *syscall.WaitStatus, options int) (int, error) {
	return syscall.Wait4(pid, ws, options, nil)
}

var fallbackEntered func(pid int)

var finalWait = func(cmd *exec.Cmd) error { return cmd.Wait() }

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
		ch.setStatus(ExitErrorOf(obs.status))
	}
	close(ch.exitedCh)
	ch.cleanGroup()
	ch.signal(syscall.SIGKILL)
	ch.drainStderr()
	ch.reapMu.Lock()
	ch.waitErr = ch.cmd.Wait()
	ch.reaped = true
	ch.reapMu.Unlock()
	if !obs.known {
		ch.setStatus(reapedExit(ch.cmd.ProcessState, ch.waitErr))
	}
	close(ch.reapedCh)
}

func (ch *Child) escalate(observed <-chan observation) observation {
	ch.signal(ch.termSignal())
	grace := time.NewTimer(ShutdownGrace)
	defer grace.Stop()
	select {
	case obs := <-observed:
		return obs
	case <-grace.C:
	}
	ch.signal(syscall.SIGKILL)
	return <-observed
}

func (ch *Child) cleanGroup() {
	if !ch.groupLive() {
		return
	}
	ch.signal(ch.termSignal())
	if ch.groupDrained(ShutdownGrace) {
		return
	}
	ch.signal(syscall.SIGKILL)
	ch.groupDrained(ShutdownGrace)
}

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

func (ch *Child) groupLive() bool {
	live, err := groupHasLiveMember(ch.pgid)
	return live || err != nil
}

const stderrDrain = 500 * time.Millisecond

func (ch *Child) drainStderr() {
	drain := time.NewTimer(StderrWait(stderrDrain))
	defer drain.Stop()
	select {
	case <-ch.stderrDone:
	case <-drain.C:
	}
}

func (ch *Child) signal(sig syscall.Signal) { _ = sendSignal(-ch.pgid, sig) }

func reapedExit(ps *os.ProcessState, waitErr error) error {
	if ps != nil {
		if st, ok := ps.Sys().(syscall.WaitStatus); ok {
			return ExitErrorOf(st)
		}
	}
	return waitErr
}

const reapPoll = 50 * time.Millisecond

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
		ch.reapMu.Lock()
		st, reaped, err := pollReap(ch.pid)
		if reaped || err != nil {
			ch.reaped = true
		}
		ch.reapMu.Unlock()
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
			ch.setStatus(ExitErrorOf(st))
			close(ch.exitedCh)
			break
		}
		switch {
		case termed.IsZero() && isClosed(ch.shutdownCh):
			ch.signal(ch.termSignal())
			termed, request = time.Now(), nil
		case !termed.IsZero() && !killed && time.Since(termed) >= ShutdownGrace:
			ch.signal(syscall.SIGKILL)
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
	_ = ch.cmd.Process.Release()
	_ = finalWait(ch.cmd)
	ch.waitErr = ch.exitErr
	close(ch.reapedCh)
}

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

func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

type procStat struct {
	state   byte // field 3
	pgrp    int  // field 5
	threads int  // field 20, num_threads
}

func parseStat(line []byte) (procStat, bool) {
	i := bytes.LastIndexByte(line, ')')
	if i < 0 {
		return procStat{}, false
	}
	f := bytes.Fields(line[i+1:])
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

type procEntry struct {
	pid     int
	zombie  bool
	exiting bool
}

func liveMember(entries []procEntry, agent int) bool {
	for _, e := range entries {
		if e.pid != agent && !e.zombie && !e.exiting {
			return true
		}
	}
	return false
}
