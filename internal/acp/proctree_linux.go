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
func platformKillVerified(e treeEntry) bool {
	fd, err := unix.PidfdOpen(e.pid, 0)
	if errors.Is(err, unix.ENOSYS) {
		if now, ok := statEntry(e.pid); !ok || now.start != e.start {
			return false
		}
		return syscall.Kill(e.pid, syscall.SIGKILL) == nil
	}
	if err != nil {
		return false
	}
	defer unix.Close(fd)
	if now, ok := statEntry(e.pid); !ok || now.start != e.start {
		return false
	}
	return unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0) == nil
}
