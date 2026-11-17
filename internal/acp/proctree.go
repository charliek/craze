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
// the group last while the agent is unreaped. Once it has been reaped, the
// group's remaining members and what the captured processes started since are
// read from the process table and killed the same checked way (review C5d
// item 7).
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
	entries := append(append(TreeSnapshot(nil), earlier...), ch.SnapshotTree()...)
	swept := false
	if ch.reapedNow() {
		// The agent exited first and has been reaped — on the polling reaper
		// nothing cleaned its group at that exit (review C5d item 7) — so
		// what is left is found in the process table, before anything is
		// killed (a captured process's children are its descendants only
		// while it lives): its group's members, and whatever a captured
		// process has started since.
		entries = append(entries, leftBehind(ch.pgid, entries)...)
		swept = true
	}
	seen := map[treeEntry]bool{}
	kill := func(es []treeEntry) {
		for _, e := range es {
			if e.pid <= 1 || seen[e] {
				continue
			}
			seen[e] = true
			killVerified(e)
		}
	}
	kill(entries)
	ch.reapMu.Lock()
	reaped := ch.reaped || isClosed(ch.reapedCh)
	if !reaped {
		// The agent is unreaped (the reap waits on reapMu): its group's id
		// cannot be anyone else's.
		_ = groupKill(-ch.pgid, syscall.SIGKILL)
	}
	ch.reapMu.Unlock()
	if reaped && !swept {
		// Reaped while the kills ran: the group's members are read now.
		kill(leftBehind(ch.pgid, entries))
	}
	ch.Kill()
}

// reapedNow reports whether the agent has been reaped.
func (ch *Child) reapedNow() bool {
	ch.reapMu.Lock()
	defer ch.reapMu.Unlock()
	return ch.reaped || isClosed(ch.reapedCh)
}

// leftBehind is what a reaped agent left, from the process table read now:
// every member of its process group pgid, and every descendant of a captured
// process that is still that process (its start time unchanged).
//
// The group's id is safe to match while any member is left: a pid that is a
// live group's id is not handed to a new process. Once the group is empty the
// id is free, and a new process may hold it, with a group of its own of that
// id; so when some process's pid is pgid, the group is not read at all.
func leftBehind(pgid int, captured []treeEntry) []treeEntry {
	table, err := procTable()
	if err != nil {
		return nil
	}
	byPid := make(map[int]treeEntry, len(table))
	for _, e := range table {
		byPid[e.pid] = e
	}
	var out []treeEntry
	if _, reused := byPid[pgid]; !reused && pgid > 1 {
		for _, e := range table {
			if e.pgid == pgid {
				out = append(out, e)
			}
		}
	}
	for _, c := range captured {
		if now, ok := byPid[c.pid]; ok && now.start == c.start {
			out = append(out, descendants(table, c.pid)...)
		}
	}
	return out
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
