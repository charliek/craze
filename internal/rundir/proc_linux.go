//go:build linux

package rundir

import (
	"errors"
	"io/fs"
	"os"
	"strconv"
	"syscall"
)

// processIdentity reads /proc/<pid>/stat (parseProcStat). A pid with no
// entry there is ErrNoProcess — and so is one whose process went between the
// open and the read, which the kernel answers ESRCH.
func processIdentity(pid int) (ProcIdentity, error) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	switch {
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, syscall.ESRCH):
		return ProcIdentity{}, ErrNoProcess
	case err != nil:
		return ProcIdentity{}, err
	}
	return parseProcStat(string(b))
}
