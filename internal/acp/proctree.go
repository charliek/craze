package acp

import (
	"sync"
	"syscall"
)

// The overage guard's descendant kill (plan 052 § D3, reviews C5 item 6 and
// C5b items 3–4). A kill of the agent's process group reaches everything
// that stayed in it; a descendant that made a group or a session of its own
// (setsid, setpgid) does not, and one the polling reaper's agent left behind
// at an exit of its own is not cleaned up either. So the agent's process tree
// — every process whose parent chain leads to the agent, with its start time
// — is read when the stop begins, and again at the kill while the agent is
// unreaped (the reaper waits for the read: Child.reapMu); every process of
// either reading is killed, each one only once it has been checked to still
// be that process (pidfd and start time on Linux, start time on macOS), and
// the group last, whatever became of the agent.
//
// What it cannot reach, and the docs say so: a process that left the tree
// before either reading (forked twice and handed to init). On macOS the
// identity check and the signal are two calls, so a pid reused between them
// is a residual, best-effort window; Linux signals through the pidfd that was
// checked.

// treeEntry is one process of the process table: its pid, its parent's, its
// process group, and its start time as the platform spells it (Linux: stat's
// field 22, in clock ticks since boot; macOS: ps's lstart), the identity a
// reused pid does not share.
type treeEntry struct {
	pid, ppid, pgid int
	start           string
}

// TreeSnapshot is the agent's descendants at one moment.
type TreeSnapshot []treeEntry

// procTable is the platform's process table (proctree_linux.go,
// proctree_darwin.go): a variable so a test can see it.
var procTable = readProcTable

// killVerified is the platform's checked kill of one snapshot entry: false
// when the process is gone or the pid is no longer that process. A variable
// so a test can see it.
var killVerified = platformKillVerified

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

// testDuringSnapshot is a test seam, nil in production: it runs inside
// SnapshotTree while the reaper is held off.
var testDuringSnapshot func()

// SnapshotTree reads the agent's descendants, holding the reaper off for the
// read (reapMu), so the agent's pid — the tree's root — cannot be reaped and
// reused while it is read. An agent already reaped has no tree to read: nil.
func (ch *Child) SnapshotTree() TreeSnapshot {
	if ch == nil || ch.cmd == nil || ch.cmd.Process == nil {
		return nil
	}
	ch.reapMu.Lock()
	defer ch.reapMu.Unlock()
	if testDuringSnapshot != nil {
		testDuringSnapshot()
	}
	if isClosed(ch.reapedCh) || ch.reaped {
		return nil
	}
	table, err := procTable()
	if err != nil {
		return nil
	}
	return descendants(table, ch.pid)
}

// KillTree is the overage guard's kill (plan 052 § D3): every descendant of
// the agent that earlier or now reads in its tree — the agent's group members
// among them — each checked to still be that process (killVerified), then the
// group with no grace (Kill). A raw signal to the group's id goes only while
// the agent is unreaped, its pid pinning the id (review C5c item 4): an agent
// that exited first and was reaped may leave an empty group whose id is
// someone else's by now, so its members are reached one by one, through the
// snapshots. It waits until the agent has been reaped.
func (ch *Child) KillTree(earlier TreeSnapshot) {
	if ch == nil || ch.cmd == nil || ch.cmd.Process == nil {
		return
	}
	seen := map[treeEntry]bool{}
	for _, e := range append(append(TreeSnapshot(nil), earlier...), ch.SnapshotTree()...) {
		if e.pid <= 1 || seen[e] {
			continue
		}
		seen[e] = true
		killVerified(e)
	}
	ch.reapMu.Lock()
	if !ch.reaped && !isClosed(ch.reapedCh) {
		// The agent is unreaped (the reap waits on reapMu): its group's id
		// cannot be anyone else's.
		_ = groupKill(-ch.pgid, syscall.SIGKILL)
	}
	ch.reapMu.Unlock()
	ch.Kill()
}

// groupKill is kill(2) to a group, a variable for tests.
var groupKill = func(pid int, sig syscall.Signal) error { return syscall.Kill(pid, sig) }

// reapGate is the reaper's side of reapMu: the reap — cmd.Wait, or the
// fallback's wait4 — waits for a tree read in progress.
type reapGate struct {
	reapMu sync.Mutex
	// reaped is set, under reapMu, once the reap has been made.
	reaped bool
}
