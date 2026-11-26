//go:build linux

package agentproc

import (
	"bytes"
	"errors"
	"os"
	"strconv"
	"syscall"

	"golang.org/x/sys/unix"
)

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

func statEntry(pid int) (treeEntry, bool) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return treeEntry{}, false
	}
	return parseStatTree(pid, b)
}

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

var (
	pidfdOpen       = unix.PidfdOpen
	pidfdSendSignal = unix.PidfdSendSignal
)

func pidfdRefused(err error) bool {
	return errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EPERM) || errors.Is(err, unix.EACCES)
}

func checkedKill(e treeEntry) bool {
	if now, ok := statEntry(e.pid); !ok || now.start != e.start {
		return false
	}
	return syscall.Kill(e.pid, syscall.SIGKILL) == nil
}
