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
// (EINTR) or refuse it outright; nothing else replaces it.
var kevent = unix.Kevent

// szomb is a zombie's p_stat, SZOMB in sys/proc.h, which x/sys/unix does not
// name.
const szomb = 5

// exitPoll is how often the exit watch, while it waits, also asks the kernel
// whether the process is a zombie already (zombie): a belt to the kqueue's
// braces, so an exit whose NOTE_EXIT the watch never gets is still seen.
const exitPoll = time.Second

// watchExit is the reaper's observation of the agent's exit on macOS, which
// has no waitid(WNOWAIT) to call: a kqueue with an EVFILT_PROC filter for
// NOTE_EXIT|NOTE_EXITSTATUS on pid, registered here, right after the start,
// and waited on by the returned function. The kernel posts NOTE_EXIT as the
// process exits, before its parent reaps it, with the exit's wait status as
// the event's data; nothing is reaped. A registration refused with ESRCH is a
// process that has already exited — the kernel finds no live process by that
// pid, and only the reap could have freed it, which is the reaper's own — and
// its status is the zombie's (zombieStatus). The wait also looks for the
// zombie every exitPoll. EINTR is retried.
func watchExit(pid int) func() (syscall.WaitStatus, error) {
	kq, err := unix.Kqueue()
	if err != nil {
		return func() (syscall.WaitStatus, error) { return 0, fmt.Errorf("kqueue: %w", err) }
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
			return func() (syscall.WaitStatus, error) { return zombieStatus(pid) }
		}
		return func() (syscall.WaitStatus, error) { return 0, fmt.Errorf("kevent EVFILT_PROC: %w", err) }
	}
	return func() (syscall.WaitStatus, error) {
		defer func() { _ = unix.Close(kq) }()
		events := make([]unix.Kevent_t, 1)
		poll := unix.NsecToTimespec(int64(exitPoll))
		for {
			n, err := kevent(kq, nil, events, &poll)
			if errors.Is(err, unix.EINTR) {
				continue
			}
			if err != nil {
				return 0, fmt.Errorf("kevent: %w", err)
			}
			if n == 0 {
				if st, ok, _ := zombie(pid); ok {
					return st, nil
				}
				continue
			}
			if int(events[0].Ident) != pid || events[0].Filter != unix.EVFILT_PROC {
				continue
			}
			ev := events[0]
			if ev.Flags&unix.EV_ERROR != 0 {
				if syscall.Errno(ev.Data) == unix.ESRCH {
					return zombieStatus(pid)
				}
				return 0, fmt.Errorf("kevent EVFILT_PROC: %w", syscall.Errno(ev.Data))
			}
			if ev.Fflags&unix.NOTE_EXIT == 0 {
				continue
			}
			return syscall.WaitStatus(uint32(ev.Data) & 0xffff), nil
		}
	}
}

// zombie reads pid's kinfo_proc (sysctl kern.proc.pid): whether it is a
// zombie — exited, and not reaped, which only this child's reaper does — and
// if so its p_xstat, the wait status the kernel keeps for its parent's wait.
func zombie(pid int) (syscall.WaitStatus, bool, error) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return 0, false, fmt.Errorf("sysctl kern.proc.pid: %w", err)
	}
	if int(kp.Proc.P_pid) != pid {
		return 0, false, fmt.Errorf("sysctl kern.proc.pid: asked for pid %d, answered pid %d", pid, kp.Proc.P_pid)
	}
	if kp.Proc.P_stat != szomb {
		return 0, false, nil
	}
	return syscall.WaitStatus(kp.Proc.P_xstat), true, nil
}

// zombieStatus is the exit status of pid, a process the exit watch found
// already gone: the zombie's (zombie), polled for every groupPoll up to the
// grace, for a process between its exit and its zombie state.
func zombieStatus(pid int) (syscall.WaitStatus, error) {
	deadline := time.Now().Add(shutdownGrace)
	for {
		st, ok, err := zombie(pid)
		if err != nil {
			return 0, err
		}
		if ok {
			return st, nil
		}
		if !time.Now().Before(deadline) {
			return 0, fmt.Errorf("process %d: no exit to watch, and not a zombie", pid)
		}
		time.Sleep(groupPoll)
	}
}

// groupHasLiveMember reports whether a process that is not a zombie is in the
// process group pgid: the kernel's kinfo_proc for every process in it (sysctl
// kern.proc.pgrp), whose p_stat is SZOMB for a zombie.
func groupHasLiveMember(pgid int) (bool, error) {
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.pgrp", pgid)
	if err != nil {
		return false, err
	}
	for _, p := range procs {
		if p.Proc.P_stat != szomb {
			return true, nil
		}
	}
	return false, nil
}
