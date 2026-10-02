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
// status in a siginfo. EINTR is retried. The wait is the returned function;
// nothing has to be set up before it.
func watchExit(pid int) func() (syscall.WaitStatus, error) {
	return func() (syscall.WaitStatus, error) {
		for {
			var info unix.Siginfo
			err := waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
			if errors.Is(err, unix.EINTR) {
				continue
			}
			if err != nil {
				return 0, fmt.Errorf("waitid: %w", err)
			}
			return siginfoStatus(&info, pid)
		}
	}
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

// groupHasLiveMember reports whether a process that is not a zombie is in the
// process group pgid: a scan of /proc/<pid>/stat, whose fields 3 and 5 are a
// process's state and its process group (parseStatGroup). A process that goes
// between the listing and its read is not counted; /proc that cannot be
// listed is an error.
func groupHasLiveMember(pgid int) (bool, error) {
	d, err := os.Open("/proc")
	if err != nil {
		return false, err
	}
	names, err := d.Readdirnames(-1)
	_ = d.Close()
	if err != nil {
		return false, err
	}
	var buf [512]byte
	for _, name := range names {
		if name == "" || name[0] < '0' || name[0] > '9' {
			continue
		}
		fd, err := syscall.Open("/proc/"+name+"/stat", syscall.O_RDONLY|syscall.O_CLOEXEC, 0)
		if err != nil {
			continue
		}
		n, err := syscall.Read(fd, buf[:])
		_ = syscall.Close(fd)
		if err != nil || n <= 0 {
			continue
		}
		state, pgrp, ok := parseStatGroup(buf[:n])
		if ok && pgrp == pgid && state != 'Z' && state != 'X' {
			return true, nil
		}
	}
	return false, nil
}
