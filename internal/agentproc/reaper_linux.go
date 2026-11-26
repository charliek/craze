//go:build linux

package agentproc

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

var waitid = unix.Waitid

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

type sigchldInfo struct {
	_      [3]int32
	_      [0]uintptr
	pid    int32
	uid    uint32
	status int32
}

const (
	cldExited = 1
	cldKilled = 2
	cldDumped = 3
)

func siginfoStatus(info *unix.Siginfo, pid int) (syscall.WaitStatus, error) {
	sc := (*sigchldInfo)(unsafe.Pointer(info))
	if syscall.Signal(info.Signo) != syscall.SIGCHLD || int(sc.pid) != pid {
		return 0, fmt.Errorf("waitid: a siginfo for signal %d, pid %d, not process %d's exit", info.Signo, sc.pid, pid)
	}
	return cldStatus(info.Code, sc.status)
}

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

var procListed func(pgid int)

var statRead = func(path string, buf []byte) (int, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return 0, err
	}
	n, err := syscall.Read(fd, buf)
	_ = syscall.Close(fd)
	return n, err
}

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
