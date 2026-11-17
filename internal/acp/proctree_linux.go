//go:build linux

package acp

import (
	"bytes"
	"os"
	"strconv"
)

// readProcTable reads /proc/<pid>/stat for every process: its pid, its
// parent's (field 4) and its group (field 5). A process that ends while the
// table is read is left out.
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
		b, err := os.ReadFile("/proc/" + d.Name() + "/stat")
		if err != nil {
			continue
		}
		if e, ok := parseStatTree(pid, b); ok {
			out = append(out, e)
		}
	}
	return out, nil
}

// parseStatTree reads a stat line's parent and group: the command, field 2,
// ends at the line's last ")".
func parseStatTree(pid int, line []byte) (treeEntry, bool) {
	i := bytes.LastIndexByte(line, ')')
	if i < 0 {
		return treeEntry{}, false
	}
	f := bytes.Fields(line[i+1:])
	if len(f) < 3 {
		return treeEntry{}, false
	}
	ppid, err1 := strconv.Atoi(string(f[1]))
	pgid, err2 := strconv.Atoi(string(f[2]))
	if err1 != nil || err2 != nil {
		return treeEntry{}, false
	}
	return treeEntry{pid: pid, ppid: ppid, pgid: pgid}, true
}
