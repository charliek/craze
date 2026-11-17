package acp

import "syscall"

// The overage guard's descendant sweep (plan 052 § D3, review C5 item 6). A
// kill of the agent's process group reaches everything that stayed in it; a
// descendant that made a group or a session of its own (setsid, setpgid) does
// not. So before the group kill, the agent's process tree is read — every
// process whose parent chain leads to the agent — and each one outside the
// group is killed outright. It is best effort, and says so in the docs:
// a process that has already left the tree (double-forked and reparented to
// init) cannot be found this way, and a pid read in the table and reused by
// the time the signal goes would be a stranger's — the window is the time
// between one read of the process table and the signals that follow it.

// treeEntry is one process of the process table: its pid, its parent's and
// its process group.
type treeEntry struct{ pid, ppid, pgid int }

// procTable is the platform's process table (proctree_linux.go,
// proctree_darwin.go): a variable so a test can see it.
var procTable = readProcTable

// descendants is every process of table whose parent chain reaches root,
// root excluded.
func descendants(table []treeEntry, root int) []treeEntry {
	children := make(map[int][]treeEntry, len(table))
	for _, e := range table {
		if e.pid != e.ppid {
			children[e.ppid] = append(children[e.ppid], e)
		}
	}
	var out []treeEntry
	seen := map[int]bool{root: true}
	queue := []int{root}
	for len(queue) > 0 {
		p := queue[0]
		queue = queue[1:]
		for _, c := range children[p] {
			if seen[c.pid] {
				continue
			}
			seen[c.pid] = true
			out = append(out, c)
			queue = append(queue, c.pid)
		}
	}
	return out
}

// killEscaped SIGKILLs every descendant of the agent that is outside its
// process group, and answers how many it signalled. It is called only while
// the agent is unreaped (its pid still pins the tree's root), before the
// group kill.
func (ch *Child) killEscaped() int {
	table, err := procTable()
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range descendants(table, ch.pid) {
		if e.pgid == ch.pgid || e.pid <= 1 {
			continue
		}
		if sweepSignal(e.pid, syscall.SIGKILL) == nil {
			n++
		}
	}
	return n
}

// sweepSignal is kill(2) for the sweep, a variable for tests.
var sweepSignal = func(pid int, sig syscall.Signal) error { return syscall.Kill(pid, sig) }

// KillTree is the overage guard's kill (plan 052 § D3): the agent's
// descendants outside its process group first, while its tree can still be
// read, then the group itself with no grace (Kill). It waits until the agent
// has been reaped.
func (ch *Child) KillTree() {
	if ch == nil || ch.cmd == nil || ch.cmd.Process == nil {
		return
	}
	if !isClosed(ch.exitedCh) {
		ch.killEscaped()
	}
	ch.Kill()
}
