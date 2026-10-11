//go:build linux

package hostspawn

import (
	"errors"
	"syscall"

	"golang.org/x/sys/unix"
)

// pidfdSignalProcessGroup is <linux/pidfd.h>'s PIDFD_SIGNAL_PROCESS_GROUP
// (Linux 6.9): pidfd_send_signal to the process group of the process the
// pidfd names. x/sys/unix does not name it yet.
const pidfdSignalProcessGroup = 1 << 2

// signalHost sends sig to a reparented host — the process group it leads,
// with group — only while pid is that host (plan 041 C6 r1, 4). On Linux the
// signal is bound to the process: a pidfd for pid is opened first, the
// process's start time is checked against the host's with the pidfd held,
// and the signal goes through the pidfd (pidfd_send_signal). A number the
// host had cannot be anyone else's at the check unless the host was gone by
// then, and once the check has found the host, the pidfd names it and no
// later process: a host gone after the check is answered ESRCH, never a
// stranger signalled. The group goes through the pidfd too
// (PIDFD_SIGNAL_PROCESS_GROUP); a kernel before 6.9 refuses the flag, and the
// group is then signalled by its id, the host proven its leader an instant
// before — SF-120's window, as for a child of this process's own. A kernel
// without pidfd_open (before 5.3) takes the same check-then-kill
// (checkThenSignal). sent: a signal went; bound: through the pidfd alone —
// or nothing went because the pidfd's process is not the host.
func signalHost(pid int, start uint64, sig syscall.Signal, group bool) (sent, bound bool) {
	fd, err := unix.PidfdOpen(pid, 0)
	if errors.Is(err, unix.ESRCH) {
		return false, true
	}
	if err != nil {
		return checkThenSignal(pid, start, sig, group), false
	}
	defer func() { _ = unix.Close(fd) }()
	if !isHost(pid, start) {
		return false, true
	}
	flags := 0
	if group {
		flags = pidfdSignalProcessGroup
	}
	err = unix.PidfdSendSignal(fd, unix.Signal(sig), nil, flags)
	if group && errors.Is(err, unix.EINVAL) {
		return syscall.Kill(-pid, sig) == nil, false
	}
	return err == nil, true
}
