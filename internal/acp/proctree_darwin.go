//go:build darwin

package acp

import (
	"bytes"
	"context"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// readProcTable is `ps -A -o pid=,ppid=,pgid=,lstart=`: every process, its
// parent, its group and its start time (the rest of the line).
func readProcTable() ([]treeEntry, error) {
	out, err := psOutput("-A", "-o", "pid=,ppid=,pgid=,lstart=")
	if err != nil {
		return nil, err
	}
	var table []treeEntry
	for _, line := range bytes.Split(out, []byte("\n")) {
		f := strings.Fields(string(line))
		if len(f) < 4 {
			continue
		}
		pid, e1 := strconv.Atoi(f[0])
		ppid, e2 := strconv.Atoi(f[1])
		pgid, e3 := strconv.Atoi(f[2])
		if e1 != nil || e2 != nil || e3 != nil {
			continue
		}
		table = append(table, treeEntry{pid: pid, ppid: ppid, pgid: pgid, start: strings.Join(f[3:], " ")})
	}
	return table, nil
}

func psOutput(args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "/bin/ps", args...).Output()
}

// platformKillVerified checks e's start time again and then SIGKILLs it:
// best effort on macOS, which has no pidfd — a pid reused between the check
// and the signal would be signalled.
func platformKillVerified(e treeEntry) bool {
	out, err := psOutput("-o", "lstart=", "-p", strconv.Itoa(e.pid))
	if err != nil || strings.Join(strings.Fields(string(out)), " ") != e.start {
		return false
	}
	return syscall.Kill(e.pid, syscall.SIGKILL) == nil
}
