//go:build linux

package acp

import (
	"bytes"
	"errors"
	"os"
	"strconv"
	"syscall"

	"golang.org/x/sys/unix"
)

// readProcTable reads /proc/<pid>/stat for every process: its pid, its
// parent's (field 4), its group (field 5) and its start time (field 22). A
// process that ends while the table is read is left out.
func readProcTable() ([]treeEntry, error) {
	dir, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	out := make([]treeEntry, 0, len(dir))
	for _, d := range dir {
		pid, err := strconv.Atoi(d.Name())
		if err != nil {
			continue
		}
		if e, ok := statEntry(pid); ok {
			out = append(out, e)
		}
	}
	return out, nil
}

// statEntry is pid's /proc stat line as a tree entry.
func statEntry(pid int) (treeEntry, bool) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return treeEntry{}, false
	}
	return parseStatTree(pid, b)
}

// parseStatTree reads a stat line's parent, group and start time: the
// command, field 2, ends at the line's last ")", after which field 3 is the
// first.
func parseStatTree(pid int, line []byte) (treeEntry, bool) {
	i := bytes.LastIndexByte(line, ')')
	if i < 0 {
		return treeEntry{}, false
	}
	f := bytes.Fields(line[i+1:])
	if len(f) < 20 {
		return treeEntry{}, false
	}
	ppid, err1 := strconv.Atoi(string(f[1]))
	pgid, err2 := strconv.Atoi(string(f[2]))
	if err1 != nil || err2 != nil {
		return treeEntry{}, false
	}
	return treeEntry{pid: pid, ppid: ppid, pgid: pgid, start: string(f[19])}, true
}

// platformKillVerified SIGKILLs e through a pidfd opened on its pid and
// checked to be e — its start time unchanged — so a pid reused since the
// snapshot is never signalled: the pidfd names one process for its life.
// A kernel with no pidfd_open (before 5.3) gets the start-time check and
// kill(2), best effort.
//
// A pidfd the kernel or a sandbox will not give or use — ENOSYS, or EPERM /
// EACCES from a seccomp profile (C8 K6) — falls back to the same checked
// kill(2), so a captured descendant is not left running because the safer
// path was refused.
func platformKillVerified(e treeEntry) bool {
	fd, err := pidfdOpen(e.pid, 0)
	if pidfdRefused(err) {
		return checkedKill(e)
	}
	if err != nil {
		return false
	}
	defer unix.Close(fd)
	if now, ok := statEntry(e.pid); !ok || now.start != e.start {
		return false
	}
	err = pidfdSendSignal(fd, unix.SIGKILL, nil, 0)
	if pidfdRefused(err) {
		return checkedKill(e)
	}
	return err == nil
}

// pidfdOpen and pidfdSendSignal are the pidfd calls, vars so a test can
// refuse them as a sandbox would.
var (
	pidfdOpen       = unix.PidfdOpen
	pidfdSendSignal = unix.PidfdSendSignal
)

// pidfdRefused reports a pidfd call the kernel lacks or a sandbox denies.
func pidfdRefused(err error) bool {
	return errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EPERM) || errors.Is(err, unix.EACCES)
}

// checkedKill is kill(2) after the start-time check: best effort against pid
// reuse, where no pidfd can be had.
func checkedKill(e treeEntry) bool {
	if now, ok := statEntry(e.pid); !ok || now.start != e.start {
		return false
	}
	return syscall.Kill(e.pid, syscall.SIGKILL) == nil
}
