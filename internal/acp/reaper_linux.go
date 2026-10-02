//go:build linux

package acp

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// waitid is unix.Waitid: a variable so a test can interrupt the observation
// (EINTR) or refuse it outright; nothing else replaces it.
var waitid = unix.Waitid

// watchExit is the reaper's observation of the agent's exit on Linux:
// waitid(P_PID, pid, WEXITED|WNOWAIT), which waits for the process to exit
// and leaves it unreaped — a zombie, its pid still taken — and reports its
// status in a siginfo. EINTR is retried. The same call with WNOHANG is made
// here first, so a waitid the environment refuses (ENOSYS under a seccomp
// filter or an emulator) is known before the reaper runs: the error, and the
// reaper is the fallback (reapPolling). The wait is the returned function.
func watchExit(pid int) (func() (syscall.WaitStatus, bool, error), error) {
	var probe unix.Siginfo
	for {
		err := waitid(unix.P_PID, pid, &probe, unix.WEXITED|unix.WNOWAIT|unix.WNOHANG, nil)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("waitid: %w", err)
		}
		break
	}
	return func() (syscall.WaitStatus, bool, error) {
		for {
			var info unix.Siginfo
			err := waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
			if errors.Is(err, unix.EINTR) {
				continue
			}
			if err != nil {
				return 0, false, fmt.Errorf("waitid: %w", err)
			}
			st, err := siginfoStatus(&info, pid)
			return st, err == nil, err
		}
	}, nil
}

// sigchldInfo is a siginfo_t as waitid fills it for a child: the header
// (unix.Siginfo's own Signo, Errno and Code, read from there, where their
// order is the architecture's), then the union, aligned as a pointer is,
// whose _sigchld member starts with the child's pid, its uid and its status.
type sigchldInfo struct {
	_      [3]int32
	_      [0]uintptr
	pid    int32
	uid    uint32
	status int32
}

// The si_code values of a child's exit (asm-generic/siginfo.h).
const (
	cldExited = 1 // exited: si_status is the exit code
	cldKilled = 2 // killed by a signal: si_status is the signal
	cldDumped = 3 // killed by a signal, and dumped core
)

// siginfoStatus reads the exit waitid reported for pid as a wait status.
func siginfoStatus(info *unix.Siginfo, pid int) (syscall.WaitStatus, error) {
	sc := (*sigchldInfo)(unsafe.Pointer(info))
	if syscall.Signal(info.Signo) != syscall.SIGCHLD || int(sc.pid) != pid {
		return 0, fmt.Errorf("waitid: a siginfo for signal %d, pid %d, not process %d's exit", info.Signo, sc.pid, pid)
	}
	return cldStatus(info.Code, sc.status)
}

// cldStatus is a child's exit, as si_code and si_status say it, in the wait
// status encoding syscall.WaitStatus reads: the exit code in the second byte,
// or the signal in the low seven bits and 0x80 for a core dump.
func cldStatus(code, status int32) (syscall.WaitStatus, error) {
	switch code {
	case cldExited:
		return syscall.WaitStatus((uint32(status) & 0xff) << 8), nil
	case cldKilled:
		return syscall.WaitStatus(uint32(status) & 0x7f), nil
	case cldDumped:
		return syscall.WaitStatus(uint32(status)&0x7f | 0x80), nil
	}
	return 0, fmt.Errorf("waitid: si_code %d is not an exit", code)
}

// procListed, a test seam, runs in groupHasLiveMember between the listing of
// /proc and the reads of what it listed; nil in production.
var procListed func(pgid int)

// statRead reads up to len(buf) bytes of the file at path — an open, a read
// and a close — and answers the first error it met. A variable so a test can
// fail it; nothing else replaces it.
var statRead = func(path string, buf []byte) (int, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return 0, err
	}
	n, err := syscall.Read(fd, buf)
	_ = syscall.Close(fd)
	return n, err
}

// groupHasLiveMember reports whether a live process is in the process group
// pgid: a scan of /proc/<pid>/stat, whose fields 3, 5 and 20 are a process's
// state, its process group and its thread count (parseStat). A zombie is not
// live, except a zombie thread-group leader with threads left — its main
// thread exited, a worker still runs. A process gone between the listing and
// its read (ENOENT, ESRCH) is not there; a read that fails any other way, or
// a line that cannot be read, answers live with the error, and so does a
// /proc that cannot be listed. EINTR is retried.
func groupHasLiveMember(pgid int) (bool, error) {
	d, err := os.Open("/proc")
	if err != nil {
		return true, err
	}
	names, err := d.Readdirnames(-1)
	_ = d.Close()
	if err != nil {
		return true, err
	}
	if procListed != nil {
		procListed(pgid)
	}
	var buf [1024]byte
	for _, name := range names {
		if name == "" || name[0] < '0' || name[0] > '9' {
			continue
		}
		path := "/proc/" + name + "/stat"
		var n int
		for {
			n, err = statRead(path, buf[:])
			if !errors.Is(err, syscall.EINTR) {
				break
			}
		}
		switch {
		case errors.Is(err, syscall.ENOENT), errors.Is(err, syscall.ESRCH):
			continue
		case err != nil:
			return true, fmt.Errorf("%s: %w", path, err)
		}
		st, ok := parseStat(buf[:n])
		if !ok {
			return true, fmt.Errorf("%s: %q is not a stat line", path, buf[:n])
		}
		if st.pgrp != pgid {
			continue
		}
		if (st.state != 'Z' && st.state != 'X') || st.threads > 1 {
			return true, nil
		}
	}
	return false, nil
}
