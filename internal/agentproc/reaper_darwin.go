//go:build darwin

package agentproc

import (
	"errors"
	"fmt"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

var kevent = unix.Kevent

var kinfoProc = unix.SysctlKinfoProc

var kinfoProcs = unix.SysctlKinfoProcSlice

const szomb = 5

const pWexit = 0x00002000

const exitPoll = time.Second

const zombieQueryErrors = 3

const (
	branchEvent              = "event"
	branchGoneAtRegistration = "gone at registration"
	branchGoneAtEvent        = "gone at the event"
	branchZombiePoll         = "zombie poll"
	branchQueriesFailed      = "zombie queries failed"
)

var watchBranch func(pid int, branch string)

func tookBranch(pid int, branch string) {
	if watchBranch != nil {
		watchBranch(pid, branch)
	}
}

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

func awaitZombie(pid int) error {
	deadline := time.Now().Add(ShutdownGrace)
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
