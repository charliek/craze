//go:build darwin

package rundir

import (
	"errors"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// processIdentity reads the kernel's kinfo_proc for pid (sysctl
// kern.proc.pid.<pid>): p_starttime, in microseconds since the epoch, the
// parent's pid, and p_stat, SZOMB for a zombie. The sysctl answers a pid no
// process has with no bytes, which x/sys reports as EIO; a read that fails
// while kill(pid, 0) finds nobody is ErrNoProcess. A struct naming another
// pid than the one asked for is not trusted to name this one.
func processIdentity(pid int) (ProcIdentity, error) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		if errors.Is(unix.Kill(pid, 0), unix.ESRCH) {
			return ProcIdentity{}, ErrNoProcess
		}
		return ProcIdentity{}, fmt.Errorf("sysctl kern.proc.pid: %w", err)
	}
	if int(kp.Proc.P_pid) != pid {
		return ProcIdentity{}, fmt.Errorf("sysctl kern.proc.pid: asked for pid %d, answered pid %d", pid, kp.Proc.P_pid)
	}
	tv := kp.Proc.P_starttime
	if tv.Sec <= 0 || tv.Usec < 0 {
		return ProcIdentity{}, fmt.Errorf("sysctl kern.proc.pid: start time %d.%06d", tv.Sec, tv.Usec)
	}
	return ProcIdentity{Start: uint64(tv.Sec)*1_000_000 + uint64(tv.Usec), PPID: int(kp.Eproc.Ppid), Zombie: kp.Proc.P_stat == sZomb}, nil
}

// processGroups asks the kernel for every process of this user's in one sysctl
// (kern.proc.uid): the kinfo_proc processIdentity reads, for each. There is no
// other user's process in it to be denied, and neither a reader of libproc nor
// cgo is needed. A process's session has no field there that means anything out
// of the kernel, so it is read with getsid; one that has gone by then is left out.
func processGroups(deadline time.Time) (map[int][]GroupMember, error) {
	kps, err := unix.SysctlKinfoProcSlice("kern.proc.uid", os.Geteuid())
	if err != nil {
		return nil, fmt.Errorf("sysctl kern.proc.uid: %w", err)
	}
	groups := map[int][]GroupMember{}
	for i := range kps {
		if i%64 == 0 && time.Now().After(deadline) {
			return nil, ErrScanCut
		}
		kp := &kps[i]
		pid, tv := int(kp.Proc.P_pid), kp.Proc.P_starttime
		if pid <= 0 || tv.Sec <= 0 || tv.Usec < 0 {
			continue
		}
		session, err := unix.Getsid(pid)
		if err != nil {
			continue
		}
		pgid := int(kp.Eproc.Pgid)
		groups[pgid] = append(groups[pgid], GroupMember{
			PID: pid, PGID: pgid, Session: session,
			Start:  uint64(tv.Sec)*1_000_000 + uint64(tv.Usec),
			Zombie: kp.Proc.P_stat == sZomb,
		})
	}
	return groups, nil
}

// processMember reads one kinfo_proc (ProcessMember).
func processMember(pid int) (GroupMember, error) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		if errors.Is(unix.Kill(pid, 0), unix.ESRCH) {
			return GroupMember{}, ErrNoProcess
		}
		return GroupMember{}, fmt.Errorf("sysctl kern.proc.pid: %w", err)
	}
	if int(kp.Proc.P_pid) != pid {
		return GroupMember{}, fmt.Errorf("sysctl kern.proc.pid: asked for pid %d, answered pid %d", pid, kp.Proc.P_pid)
	}
	session, err := unix.Getsid(pid)
	if err != nil {
		return GroupMember{}, err
	}
	tv := kp.Proc.P_starttime
	return GroupMember{
		PID: pid, PGID: int(kp.Eproc.Pgid), Session: session,
		Start:  uint64(tv.Sec)*1_000_000 + uint64(tv.Usec),
		Zombie: kp.Proc.P_stat == sZomb,
	}, nil
}

// processCarries reads the process's arguments and environment (sysctl
// kern.procargs2), which the kernel answers only for a process of this user's, or
// to root, so a process of another's is one that carries nothing (a denial
// is an error here, never a failure of the caller's). A root caller's scan named
// only its own processes (processGroups). The kernel also leaves the environment out
// for a restricted process (CS_RESTRICT) unless the caller is entitled to it, so such a
// process reads as carrying nothing either, which the docs say.
func processCarries(pid int, mark string) bool {
	b, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return false
	}
	return environHolds(procArgsEnviron(b), mark)
}

// sZomb is <sys/proc.h>'s SZOMB, kinfo_proc's p_stat for a process that has
// exited and waits for its parent to reap it.
const sZomb = 5

// processTokenScope is a start time's scope on macOS: the kernel's boot
// session UUID, and no PID namespace (bootSessionScope).
func processTokenScope() (string, string, error) { return bootSessionScope(unix.Sysctl) }
