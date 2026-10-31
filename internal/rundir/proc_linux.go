//go:build linux

package rundir

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
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

// processGroups reads /proc/<pid>/stat for every number in /proc (ProcessGroups).
// The read's own buffer is one page, past any stat line, so a scan is three
// syscalls a process. The deadline is looked at every so often, not every read.
func processGroups(deadline time.Time) (map[int][]GroupMember, error) {
	d, err := os.Open("/proc")
	if err != nil {
		return nil, err
	}
	defer d.Close()
	names, err := d.Readdirnames(-1)
	if err != nil {
		return nil, err
	}
	groups := map[int][]GroupMember{}
	buf := make([]byte, 4096)
	for i, name := range names {
		pid, err := strconv.Atoi(name)
		if err != nil {
			continue
		}
		if i%64 == 0 && time.Now().After(deadline) {
			return nil, ErrScanCut
		}
		n := readStat(name, buf)
		if n <= 0 {
			continue
		}
		m, err := parseProcMember(pid, string(buf[:n]))
		if err != nil {
			continue
		}
		groups[m.PGID] = append(groups[m.PGID], m)
	}
	return groups, nil
}

// processMember reads one /proc/<pid>/stat (ProcessMember).
func processMember(pid int) (GroupMember, error) {
	buf := make([]byte, 4096)
	n := readStat(strconv.Itoa(pid), buf)
	if n <= 0 {
		return GroupMember{}, ErrNoProcess
	}
	return parseProcMember(pid, string(buf[:n]))
}

// readStat reads /proc/<pid>/stat into buf: how much, or 0 for a process that
// has gone or cannot be read.
func readStat(pid string, buf []byte) int {
	fd, err := syscall.Open("/proc/"+pid+"/stat", syscall.O_RDONLY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return 0
	}
	defer func() { _ = syscall.Close(fd) }()
	n, err := syscall.Read(fd, buf)
	if err != nil {
		return 0
	}
	return n
}

// processCarries reads /proc/<pid>/environ, which the kernel shows only to whoever
// may ptrace the process, once /proc/<pid> says the process is this user's: a
// process running as root reads every other user's too.
func processCarries(pid int, mark string) bool {
	dir := "/proc/" + strconv.Itoa(pid)
	var st syscall.Stat_t
	if syscall.Stat(dir, &st) != nil || int(st.Uid) != os.Geteuid() {
		return false
	}
	b, err := os.ReadFile(dir + "/environ")
	return err == nil && environHolds(b, mark)
}

// procSelfStatus is /proc/self/status: os.ReadFile, which a test replaces
// (never in parallel) to play a /proc mounted for another PID namespace.
var procSelfStatus = func() ([]byte, error) { return os.ReadFile("/proc/self/status") }

// processTokenScope is a start time's scope on Linux: the boot
// (/proc/sys/kernel/random/boot_id, a random UUID the kernel makes at each
// boot: field 22 of /proc/<pid>/stat counts clock ticks since boot, so the
// same pid and the same tick recur across boots) and the PID namespace this
// process is a member of (the number in /proc/self/ns/pid's "pid:[N]"),
// which is the one its pids — os.Getpid's, a record's — are numbers in.
//
// /proc must be that namespace's own: one mounted for an ancestor (an
// unshare -p without a remount) names other processes by these numbers, and
// its /proc/self/ns/pid is still this process's namespace, so it proves
// nothing. Neither does /proc/self naming os.Getpid(): a process can have the
// same number in its namespace and in an ancestor's. What does is the
// NStgid line of /proc/self/status, this process's pid in each namespace from
// the /proc mount's down to its own (Linux 4.1 on): exactly one field, equal
// to os.Getpid(), when the mount is this process's namespace — two or more
// when it is an ancestor's, whatever the numbers. (/proc/1/ns/pid compared
// with /proc/self/ns/pid would say the same, but pid 1 is root's, and its
// namespace link cannot be read by anyone else.) Anything else — no NStgid
// line, a line that cannot be read — is no scope: no token is made.
func processTokenScope() (string, string, error) {
	b, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", "", err
	}
	boot := strings.TrimSpace(string(b))
	if boot == "" || strings.ContainsAny(boot, "/ ") {
		return "", "", fmt.Errorf("the boot id %q", boot)
	}
	status, err := procSelfStatus()
	if err != nil {
		return "", "", err
	}
	if err := ownNamespaceProc(string(status), os.Getpid()); err != nil {
		return "", "", err
	}
	link, err := os.Readlink("/proc/self/ns/pid")
	if err != nil {
		return "", "", err
	}
	n, prefixed := strings.CutPrefix(link, "pid:[")
	n, closed := strings.CutSuffix(n, "]")
	if _, err := strconv.ParseUint(n, 10, 64); !prefixed || !closed || err != nil {
		return "", "", fmt.Errorf("/proc/self/ns/pid is %q", link)
	}
	return boot, n, nil
}

// ownNamespaceProc checks a /proc/self/status read through the /proc in use
// (status) for the process pid: its NStgid line must hold pid alone, which it
// does only when the /proc mount is the process's own PID namespace's.
func ownNamespaceProc(status string, pid int) error {
	for line := range strings.Lines(status) {
		rest, ok := strings.CutPrefix(line, "NStgid:")
		if !ok {
			continue
		}
		f := strings.Fields(rest)
		if len(f) != 1 || f[0] != strconv.Itoa(pid) {
			return fmt.Errorf("/proc is not this process's PID namespace's: NStgid is %q, this process %d", strings.Join(f, " "), pid)
		}
		return nil
	}
	return errors.New("/proc/self/status has no NStgid line, so /proc cannot be told to be this PID namespace's")
}
