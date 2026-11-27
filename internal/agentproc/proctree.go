package agentproc

import (
	"sync"
	"syscall"
)

type treeEntry struct {
	pid, ppid, pgid int
	start           string
}

// TreeSnapshot is the agent's descendants at one moment.
type TreeSnapshot []treeEntry

var procTable = readProcTable

var killVerified = platformKillVerified

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

// KillTree kills every descendant of the agent captured in earlier or now
// (each checked by start time / pidfd), signals the group while unreaped, and
// waits until the agent has been reaped.
func (ch *Child) KillTree(earlier TreeSnapshot) {
	if ch == nil || ch.cmd == nil || ch.cmd.Process == nil {
		return
	}
	entries := append(append(TreeSnapshot(nil), earlier...), ch.SnapshotTree()...)
	swept := false
	if ch.reapedNow() {
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
		_ = groupKill(-ch.pgid, syscall.SIGKILL)
	}
	ch.reapMu.Unlock()
	if reaped && !swept {
		kill(leftBehind(ch.pgid, entries))
	}
	ch.Kill()
}

func (ch *Child) reapedNow() bool {
	ch.reapMu.Lock()
	defer ch.reapMu.Unlock()
	return ch.reaped || isClosed(ch.reapedCh)
}

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

// killCapturedTree terminates every descendant in earlier plus any surviving
// members found by leftBehind, using identity-checked killVerified and never
// waiting on ch.reapedCh (safe to invoke from inside the reaper goroutine).
func killCapturedTree(pgid int, earlier TreeSnapshot) {
	entries := append(TreeSnapshot(nil), earlier...)
	entries = append(entries, leftBehind(pgid, entries)...)
	seen := map[treeEntry]bool{}
	for _, e := range entries {
		if e.pid <= 1 || seen[e] {
			continue
		}
		seen[e] = true
		killVerified(e)
	}
}

var groupKill = func(pid int, sig syscall.Signal) error { return syscall.Kill(pid, sig) }

type reapGate struct {
	reapMu sync.Mutex
	reaped bool
}
