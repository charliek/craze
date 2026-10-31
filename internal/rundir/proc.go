package rundir

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
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
// read on the same machine and boot; PPID is its parent's pid. Zombie says
// the process has exited and waits only for its parent to reap it (plan 041
// C6 r1, 3): Linux's state Z — or X, its reaper at work — macOS's SZOMB. Its
// pid is still taken, and its start time still its own.
type ProcIdentity struct {
	Start  uint64
	PPID   int
	Zombie bool
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
// it are 3 (the state) on, so the state is the first of them — one letter, Z
// or X a zombie — the parent's pid, field 4, the second and the start time,
// field 22, the twentieth. It lives here, not in the Linux file, so every
// platform's tests check it.
func parseProcStat(line string) (ProcIdentity, error) {
	i := strings.LastIndexByte(line, ')')
	if i < 0 {
		return ProcIdentity{}, errors.New("/proc stat: no command")
	}
	f := strings.Fields(line[i+1:])
	if len(f) < 20 {
		return ProcIdentity{}, fmt.Errorf("/proc stat: %d fields after the command, want 20 or more", len(f))
	}
	if len(f[0]) != 1 {
		return ProcIdentity{}, fmt.Errorf("/proc stat: the state %q is not one letter", f[0])
	}
	ppid, err := strconv.Atoi(f[1])
	if err != nil {
		return ProcIdentity{}, fmt.Errorf("/proc stat: the parent: %w", err)
	}
	start, err := strconv.ParseUint(f[19], 10, 64)
	if err != nil {
		return ProcIdentity{}, fmt.Errorf("/proc stat: the start time: %w", err)
	}
	return ProcIdentity{Start: start, PPID: ppid, Zombie: f[0] == "Z" || f[0] == "X"}, nil
}

// GroupMember is one process a scan of the machine found in a process group:
// what tells it from a process that merely has its number. Session is its
// session's id; Zombie as ProcIdentity's: it holds its number and nothing else,
// and no signal can do anything to it.
type GroupMember struct {
	PID     int
	PGID    int
	Session int
	Start   uint64
	Zombie  bool
}

// ErrScanCut is ProcessGroups' error for a scan its deadline ended: what it
// did not reach is unknown, never "nothing there".
var ErrScanCut = errors.New("rundir: the process scan ran out of time")

// ProcessGroups is every process this user can see, by process group: the one
// way to find what is left of a group whose leader has gone. One pass over the
// machine's processes, so a caller asks once for all the groups it has in mind
// (each read is a syscall or three: a few milliseconds per thousand processes).
// A process that goes while it is read is left out; an error, ErrScanCut among
// them, is a scan that says nothing, and a process that cannot be read is one
// this user could not signal anyway.
func ProcessGroups(deadline time.Time) (map[int][]GroupMember, error) {
	return processGroups(deadline)
}

// ProcessMember is pid as a scan would find it, read now: ErrNoProcess when no
// process has that pid.
func ProcessMember(pid int) (GroupMember, error) {
	if pid <= 0 {
		return GroupMember{}, fmt.Errorf("rundir: process member: pid %d", pid)
	}
	return processMember(pid)
}

// ProcessScope is the scope a start time and a pid are meaningful in: the boot,
// and the PID namespace (processTokenScope; "-" on macOS, which has none). An
// error is no scope, and nothing a record says can be checked.
func ProcessScope() (boot, pidNS string, err error) { return tokenScope() }

// ProcessCarries reports whether pid is a process of this user's whose
// environment, as it was exec'd, holds the entry mark ("NAME=value"). known says the
// answer means something: it is false for a process still there whose environment
// could not be read, which may carry the mark or may not. Another user's process, or
// one that has gone, carries nothing, and that is known.
func ProcessCarries(pid int, mark string) (carries, known bool) {
	return processCarries(pid, mark)
}

// parseProcMember is the GroupMember a Linux /proc/<pid>/stat line names
// (parseProcStat's reading; the group and the session are fields 5 and 6). It
// lives here, not in the Linux file, so every platform's tests check it.
func parseProcMember(pid int, line string) (GroupMember, error) {
	id, err := parseProcStat(line)
	if err != nil {
		return GroupMember{}, err
	}
	f := strings.Fields(line[strings.LastIndexByte(line, ')')+1:])
	pgid, err := strconv.Atoi(f[2])
	if err != nil {
		return GroupMember{}, fmt.Errorf("/proc stat: the process group: %w", err)
	}
	session, err := strconv.Atoi(f[3])
	if err != nil {
		return GroupMember{}, fmt.Errorf("/proc stat: the session: %w", err)
	}
	return GroupMember{PID: pid, PGID: pgid, Session: session, Start: id.Start, Zombie: id.Zombie}, nil
}

// environHolds reports whether env, NUL-separated entries as /proc/<pid>/environ
// has them, holds mark whole.
func environHolds(env []byte, mark string) bool {
	for _, e := range bytes.Split(env, []byte{0}) {
		if string(e) == mark {
			return true
		}
	}
	return false
}

// procArgsEnviron is the environment in a macOS kern.procargs2 buffer, its entries
// NUL-separated as environHolds reads them: argc as a 32-bit little-endian int (every
// Mac craze runs on), the executable's path and its NUL, NUL padding, argc arguments,
// then the environment up to an empty string. XNU pads the path to a pointer's size
// (exec_extract_strings), so the padding is counted, not skipped: an empty argument
// looks just like it. Should the padding ever be wider than that, the extra NULs read
// as empty arguments, the last real ones read as the environment's first, and an
// entry is still found. nil for a buffer that does not hold that much. It lives here,
// not in the Darwin file, so every platform's tests check it.
func procArgsEnviron(b []byte) []byte {
	if len(b) < 4 {
		return nil
	}
	argc := int(binary.LittleEndian.Uint32(b))
	b = b[4:]
	i := bytes.IndexByte(b, 0) // the executable's path
	if i < 0 {
		return nil
	}
	pad := (procArgsAlign - (i+1)%procArgsAlign) % procArgsAlign
	b = b[i+1:]
	for ; pad > 0 && len(b) > 0 && b[0] == 0; pad-- {
		b = b[1:]
	}
	for range argc {
		i := bytes.IndexByte(b, 0)
		if i < 0 {
			return nil
		}
		b = b[i+1:]
	}
	b = bytes.TrimLeft(b, "\x00")
	if i := bytes.Index(b, []byte{0, 0}); i >= 0 {
		return b[:i+1]
	}
	if len(b) == 0 {
		return []byte{} // no environment, which is not nil: that is no reading at all
	}
	return b
}

// procArgsAlign is what XNU pads a kern.procargs2 buffer's path to: a 64-bit
// process's pointer.
const procArgsAlign = 8

// procArgsCarry is carriesRead for a kern.procargs2 buffer. One that does not hold an
// environment's shape, or holds nothing, is no reading of it. It lives here, not in the
// Darwin file, so every platform's tests check it.
func procArgsCarry(read func() ([]byte, error), there func() bool, mark string) (carries, known bool) {
	return carriesRead(func() ([]byte, error) {
		b, err := read()
		if err != nil {
			return nil, err
		}
		env := procArgsEnviron(b)
		if env == nil {
			return nil, errors.New("kern.procargs2 holds no environment")
		}
		return env, nil
	}, there, mark)
}

// carriesRead reads a process's environment for mark. A read that fails of a process
// still there is made again, for a moment: XNU refuses a process whose task has no IPC
// yet (task_is_ipc_active, EINVAL), which an exec's new task has not from when it takes
// over the pid until just after. What goes on failing is not known, never a no: nothing
// bounds that window, and a no would make one of a host's commands a stranger, its
// group's entry dropped with it running. A process that has gone carries nothing.
func carriesRead(read func() ([]byte, error), there func() bool, mark string) (carries, known bool) {
	for i := 1; ; i++ {
		env, err := read()
		if err == nil {
			return environHolds(env, mark), true
		}
		if !there() {
			return false, true
		}
		if i >= carriesReads {
			return false, false
		}
		time.Sleep(carriesGap)
	}
}

// carriesReads and carriesGap bound carriesRead: past an exec's window on most any
// machine, and small beside a sweep's reading bound for the few processes a group holds.
const (
	carriesReads = 10
	carriesGap   = 2 * time.Millisecond
)

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
