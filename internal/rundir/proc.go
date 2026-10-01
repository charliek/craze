package rundir

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// A process's identity (plan 030 §3.4, astra r5-c3 1). A pid names a process
// only while it lives: once it has exited and been reaped, the number is free
// for the next process the kernel starts, and a process group's id — its
// leader's pid — with it. So a number written down earlier proves nothing
// about who has it now. What does is the process's start time, which the
// kernel records once, when the process is created, and which a later process
// given the same number cannot share: it had to start after the first one
// exited. A detached host writes each agent's process group down with its
// leader's start time, and its spawner signals a group only when the process
// leading it now started at that same instant (internal/cli's killAgents).
//
// Linux reads /proc/<pid>/stat (proc_linux.go), macOS the kernel's kinfo_proc
// for the pid (proc_darwin.go); any other OS answers an error for every pid
// (proc_other.go), so nothing is ever recorded or signalled there.

// ErrNoProcess is ProcessIdentity's error for a pid no process has: it
// exited and was reaped (a zombie, not yet reaped, is still there), or never
// existed.
var ErrNoProcess = errors.New("rundir: no such process")

// ProcIdentity is what tells one process from another that later has its pid.
// Start is its start time as the kernel keeps it — on Linux in clock ticks
// since boot (/proc/<pid>/stat field 22), on macOS in microseconds since the
// epoch (kinfo_proc's p_starttime) — meaningful only compared with another
// read on the same machine and boot; PPID is its parent's pid.
type ProcIdentity struct {
	Start uint64
	PPID  int
}

// ProcessIdentity is pid's identity, read from the kernel: ErrNoProcess when
// no process has that pid, another error when it cannot be read — which a
// caller treats as a process it cannot name.
func ProcessIdentity(pid int) (ProcIdentity, error) {
	if pid <= 0 {
		return ProcIdentity{}, fmt.Errorf("rundir: process identity: pid %d", pid)
	}
	id, err := processIdentity(pid)
	if err != nil && !errors.Is(err, ErrNoProcess) {
		return ProcIdentity{}, fmt.Errorf("rundir: the identity of process %d: %w", pid, err)
	}
	return id, err
}

// parseProcStat is the identity a Linux /proc/<pid>/stat line names. The
// command, field 2, is in parentheses and may hold anything — spaces and
// parentheses included — so it ends at the line's last ")"; the fields after
// it are 3 (the state) on, so the parent's pid, field 4, is the second of
// them and the start time, field 22, the twentieth. It lives here, not in the
// Linux file, so every platform's tests check it.
func parseProcStat(line string) (ProcIdentity, error) {
	i := strings.LastIndexByte(line, ')')
	if i < 0 {
		return ProcIdentity{}, errors.New("/proc stat: no command")
	}
	f := strings.Fields(line[i+1:])
	if len(f) < 20 {
		return ProcIdentity{}, fmt.Errorf("/proc stat: %d fields after the command, want 20 or more", len(f))
	}
	ppid, err := strconv.Atoi(f[1])
	if err != nil {
		return ProcIdentity{}, fmt.Errorf("/proc stat: the parent: %w", err)
	}
	start, err := strconv.ParseUint(f[19], 10, 64)
	if err != nil {
		return ProcIdentity{}, fmt.Errorf("/proc stat: the start time: %w", err)
	}
	return ProcIdentity{Start: start, PPID: ppid}, nil
}

// bootSessionScope is a start time's scope on macOS, read through sysctl
// (unix.Sysctl there; a fake in a test — it lives here, not in the Darwin
// file, so every platform's tests check it): the boot is
// kern.bootsessionuuid, a UUID the kernel makes at each boot, as Linux's
// boot_id, and there is no PID namespace ("-": Darwin has none). Nothing else
// stands in for the UUID: not kern.boottime, which XNU moves when the wall
// clock is stepped (keeping the uptime), so a live hub's token would stop
// matching its own process after an NTP correction — whole seconds or not.
// Without the UUID there is no scope, so no token is made and none is
// carried: a wedged hub is then never terminated on its record's word, only
// reported (plan 032 P17).
func bootSessionScope(sysctl func(name string) (string, error)) (string, string, error) {
	id, err := sysctl("kern.bootsessionuuid")
	if err != nil {
		return "", "", fmt.Errorf("sysctl kern.bootsessionuuid: %w", err)
	}
	if !validUUID(id) {
		return "", "", fmt.Errorf("sysctl kern.bootsessionuuid is %q, not a UUID", id)
	}
	return id, "-", nil
}

// validUUID reports whether s has a UUID's shape: 8-4-4-4-12 hex digits,
// either case, nothing else.
func validUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := range len(s) {
		c := s[i]
		switch {
		case i == 8 || i == 13 || i == 18 || i == 23:
			if c != '-' {
				return false
			}
		case (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F'):
			return false
		}
	}
	return true
}
