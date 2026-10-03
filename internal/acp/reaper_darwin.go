//go:build darwin

package acp

import (
	"errors"
	"fmt"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// kevent is unix.Kevent: a variable so a test can interrupt the observation
// (EINTR), refuse it, or hold its registration; nothing else replaces it.
var kevent = unix.Kevent

// kinfoProc is unix.SysctlKinfoProc, the zombie query's: a variable so a
// test can fail it; nothing else replaces it.
var kinfoProc = unix.SysctlKinfoProc

// kinfoProcs is unix.SysctlKinfoProcSlice, the group scan's: a variable so a
// test can hand it a listing; nothing else replaces it.
var kinfoProcs = unix.SysctlKinfoProcSlice

// szomb is a zombie's p_stat, SZOMB in sys/proc.h, which x/sys/unix does not
// name.
const szomb = 5

// pWexit is the p_flag bit of a process working on its exit, P_WEXIT in XNU's
// bsd/sys/proc.h, which x/sys/unix does not name.
const pWexit = 0x00002000

// exitPoll is how often the exit watch, while it waits, also asks the kernel
// whether the process is a zombie already (isZombie): a belt to the kqueue's
// braces, so an exit whose NOTE_EXIT the watch never gets is still seen.
const exitPoll = time.Second

// zombieQueryErrors is how many zombie queries in a row may fail before the
// exit watch gives up and the reaper falls back to reapPolling.
const zombieQueryErrors = 3

// The ways the exit watch learns of an exit, as watchBranch names them.
const (
	branchEvent              = "event"                 // NOTE_EXIT, with the status
	branchGoneAtRegistration = "gone at registration"  // ESRCH, then the zombie
	branchGoneAtEvent        = "gone at the event"     // an EV_ERROR ESRCH, then the zombie
	branchZombiePoll         = "zombie poll"           // the wait's own zombie query
	branchQueriesFailed      = "zombie queries failed" // the wait gave up: the fallback
)

// watchBranch, a test seam, is told which way the exit watch went for pid
// (the branch constants); nil in production.
var watchBranch func(pid int, branch string)

// tookBranch tells watchBranch, when set.
func tookBranch(pid int, branch string) {
	if watchBranch != nil {
		watchBranch(pid, branch)
	}
}

// watchExit is the reaper's observation of the agent's exit on macOS, which
// has no waitid(WNOWAIT) to call: a kqueue with an EVFILT_PROC filter for
// NOTE_EXIT|NOTE_EXITSTATUS on pid, registered here, right after the start,
// and waited on by the returned function. The kernel posts NOTE_EXIT as the
// process exits, before its parent reaps it, with the exit's wait status
// (its low 16 bits, as wait4 reports it) as the event's data; nothing is
// reaped. A kqueue that cannot be had, or a registration refused other than
// with ESRCH, is the error: the reaper is the fallback (reapPolling).
//
// A registration refused with ESRCH is a process that has already exited —
// the kernel finds no live process by that pid, and only the reap could have
// freed it, which is the reaper's own — so the wait confirms it is a zombie
// (awaitZombie) and reports the exit with no status: the zombie's kinfo_proc
// p_xstat saturates (exit(256), stored as 0x10000, reads 0xffff, where wait4
// reports exit 0), so the status is the reap's. The same holds when the wait,
// which also looks for the zombie every exitPoll, finds it that way. EINTR is
// retried.
func watchExit(pid int) (func() (syscall.WaitStatus, bool, error), error) {
	kq, err := unix.Kqueue()
	if err != nil {
		return nil, fmt.Errorf("kqueue: %w", err)
	}
	var reg unix.Kevent_t
	unix.SetKevent(&reg, pid, unix.EVFILT_PROC, unix.EV_ADD)
	reg.Fflags = unix.NOTE_EXIT | unix.NOTE_EXITSTATUS
	for {
		_, err = kevent(kq, []unix.Kevent_t{reg}, nil, nil)
		if !errors.Is(err, unix.EINTR) {
			break
		}
	}
	if err != nil {
		_ = unix.Close(kq)
		if errors.Is(err, unix.ESRCH) {
			tookBranch(pid, branchGoneAtRegistration)
			return func() (syscall.WaitStatus, bool, error) { return 0, false, awaitZombie(pid) }, nil
		}
		return nil, fmt.Errorf("kevent EVFILT_PROC: %w", err)
	}
	return func() (syscall.WaitStatus, bool, error) {
		defer func() { _ = unix.Close(kq) }()
		events := make([]unix.Kevent_t, 1)
		poll := unix.NsecToTimespec(int64(exitPoll))
		fails := 0
		for {
			n, err := kevent(kq, nil, events, &poll)
			if errors.Is(err, unix.EINTR) {
				continue
			}
			if err != nil {
				return 0, false, fmt.Errorf("kevent: %w", err)
			}
			if n == 0 {
				zombie, err := isZombie(pid)
				switch {
				case err != nil:
					if fails++; fails >= zombieQueryErrors {
						tookBranch(pid, branchQueriesFailed)
						return 0, false, err
					}
				case zombie:
					tookBranch(pid, branchZombiePoll)
					return 0, false, nil
				default:
					fails = 0
				}
				continue
			}
			if int(events[0].Ident) != pid || events[0].Filter != unix.EVFILT_PROC {
				continue
			}
			ev := events[0]
			if ev.Flags&unix.EV_ERROR != 0 {
				if syscall.Errno(ev.Data) == unix.ESRCH {
					tookBranch(pid, branchGoneAtEvent)
					return 0, false, awaitZombie(pid)
				}
				return 0, false, fmt.Errorf("kevent EVFILT_PROC: %w", syscall.Errno(ev.Data))
			}
			if ev.Fflags&unix.NOTE_EXIT == 0 {
				continue
			}
			tookBranch(pid, branchEvent)
			return syscall.WaitStatus(uint32(ev.Data) & 0xffff), true, nil
		}
	}, nil
}

// isZombie reads pid's kinfo_proc (sysctl kern.proc.pid): whether it is a
// zombie — exited, and not reaped, which only this child's reaper does.
func isZombie(pid int) (bool, error) {
	kp, err := kinfoProc("kern.proc.pid", pid)
	if err != nil {
		return false, fmt.Errorf("sysctl kern.proc.pid: %w", err)
	}
	if int(kp.Proc.P_pid) != pid {
		return false, fmt.Errorf("sysctl kern.proc.pid: asked for pid %d, answered pid %d", pid, kp.Proc.P_pid)
	}
	return kp.Proc.P_stat == szomb, nil
}

// awaitZombie waits for pid, which the exit watch found already gone, to be
// a zombie: polled every groupPoll up to the grace, for a process between its
// exit and its zombie state. Its error — zombieQueryErrors queries in a row
// failed, or the process never became one — sends the reaper to the
// fallback.
func awaitZombie(pid int) error {
	deadline := time.Now().Add(shutdownGrace)
	fails := 0
	for {
		zombie, err := isZombie(pid)
		switch {
		case err != nil:
			if fails++; fails >= zombieQueryErrors {
				return err
			}
		case zombie:
			return nil
		default:
			fails = 0
			if !time.Now().Before(deadline) {
				return fmt.Errorf("process %d: no exit to watch, and not a zombie", pid)
			}
		}
		time.Sleep(groupPoll)
	}
}

// groupHasLiveMember reports whether the process group pgid has a live member
// (liveMember): the kernel's kinfo_proc for every process in it (sysctl
// kern.proc.pgrp, one snapshot), whose p_stat is SZOMB for a zombie and whose
// p_flag has P_WEXIT for a process in its exit. The agent leads the group, so
// pgid is its pid too. A macOS process becomes a zombie only once its last
// thread has exited.
func groupHasLiveMember(pgid int) (bool, error) {
	procs, err := kinfoProcs("kern.proc.pgrp", pgid)
	if err != nil {
		return true, err
	}
	entries := make([]procEntry, len(procs))
	for i, p := range procs {
		entries[i] = procEntry{
			pid:     int(p.Proc.P_pid),
			zombie:  p.Proc.P_stat == szomb,
			exiting: p.Proc.P_flag&pWexit != 0,
		}
	}
	return liveMember(entries, pgid), nil
}
