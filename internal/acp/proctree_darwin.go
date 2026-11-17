//go:build darwin

package acp

import (
	"bytes"
	"context"
	"os/exec"
	"strconv"
	"time"
)

// readProcTable is `ps -A -o pid=,ppid=,pgid=`: every process, its parent and
// its group.
func readProcTable() ([]treeEntry, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/bin/ps", "-A", "-o", "pid=,ppid=,pgid=").Output()
	if err != nil {
		return nil, err
	}
	var table []treeEntry
	for _, line := range bytes.Split(out, []byte("\n")) {
		f := bytes.Fields(line)
		if len(f) != 3 {
			continue
		}
		pid, e1 := strconv.Atoi(string(f[0]))
		ppid, e2 := strconv.Atoi(string(f[1]))
		pgid, e3 := strconv.Atoi(string(f[2]))
		if e1 != nil || e2 != nil || e3 != nil {
			continue
		}
		table = append(table, treeEntry{pid: pid, ppid: ppid, pgid: pgid})
	}
	return table, nil
}
