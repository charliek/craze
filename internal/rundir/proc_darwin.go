//go:build darwin

package rundir

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

// processIdentity reads the kernel's kinfo_proc for pid (sysctl
// kern.proc.pid.<pid>): p_starttime, in microseconds since the epoch, and the
// parent's pid. The sysctl answers a pid no process has with no bytes, which
// x/sys reports as EIO; a read that fails while kill(pid, 0) finds nobody is
// ErrNoProcess. A struct naming another pid than the one asked for is not
// trusted to name this one.
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
	return ProcIdentity{Start: uint64(tv.Sec)*1_000_000 + uint64(tv.Usec), PPID: int(kp.Eproc.Ppid)}, nil
}

// processTokenScope is a start time's scope on macOS: the kernel's boot
// session UUID, and no PID namespace (bootSessionScope).
func processTokenScope() (string, string, error) { return bootSessionScope(unix.Sysctl) }
