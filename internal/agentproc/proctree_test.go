package agentproc

import (
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func alive(pid int) bool {
	if syscall.Kill(pid, 0) != nil {
		return false
	}
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	return err == nil && !strings.HasPrefix(strings.TrimSpace(string(out)), "Z")
}

func waitPidGone(t *testing.T, what string, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for alive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("%s (pid %d) is still alive", what, pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func entryOf(t *testing.T, pid int) treeEntry {
	t.Helper()
	table, err := procTable()
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range table {
		if e.pid == pid {
			return e
		}
	}
	t.Fatalf("pid %d is not in the process table", pid)
	return treeEntry{}
}

func TestKillVerifiedSparesAReusedPid(t *testing.T) {
	cmd := exec.Command("sleep", "600")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	go func() { _ = cmd.Wait() }()
	now := entryOf(t, cmd.Process.Pid)
	stale := now
	stale.start = "1"
	if killVerified(stale) {
		t.Fatal("a pid whose start time is not the snapshot's was signalled")
	}
	time.Sleep(100 * time.Millisecond)
	if !alive(cmd.Process.Pid) {
		t.Fatal("the process the pid now names was killed")
	}
	if !killVerified(now) {
		t.Fatal("the snapshot's own process was not signalled")
	}
	waitPidGone(t, "the snapshot's process", cmd.Process.Pid)
}

func TestSnapshotHoldsTheReap(t *testing.T) {
	ch := spawnShell(t, `exit 0`, nil)
	parked := make(chan struct{})
	release := make(chan struct{})
	prev := testDuringSnapshot
	testDuringSnapshot = func() { close(parked); <-release }
	t.Cleanup(func() { testDuringSnapshot = prev })
	go ch.SnapshotTree()
	<-parked
	waitClosed(t, ch.exitedCh, 5*time.Second, "the agent's exit")
	time.Sleep(200 * time.Millisecond)
	if isClosed(ch.reapedCh) {
		t.Fatal("the agent was reaped while its tree was read")
	}
	close(release)
	waitClosed(t, ch.reapedCh, 5*time.Second, "the reap")
}

func TestKillTreeSendsNoGroupSignalAfterTheReap(t *testing.T) {
	var sent []int
	prev := groupKill
	groupKill = func(pid int, sig syscall.Signal) error {
		sent = append(sent, pid)
		return nil
	}
	t.Cleanup(func() { groupKill = prev })
	ch := spawnShell(t, `exit 0`, nil)
	waitClosed(t, ch.reapedCh, 5*time.Second, "the reap")
	ch.KillTree(nil)
	if len(sent) != 0 {
		t.Fatalf("raw group signals %v after the agent was reaped", sent)
	}
	r := spawnShell(t, `sleep 60`, nil)
	r.KillTree(nil)
	if len(sent) != 1 || sent[0] != -r.pgid {
		t.Fatalf("group signals %v for an unreaped agent; want one to -%d", sent, r.pgid)
	}
}

func TestDescendantsOfTheAgent(t *testing.T) {
	table := []treeEntry{
		{pid: 1, ppid: 0, pgid: 1},
		{pid: 100, ppid: 1, pgid: 100},
		{pid: 101, ppid: 100, pgid: 100},
		{pid: 102, ppid: 100, pgid: 102},
		{pid: 103, ppid: 102, pgid: 103},
		{pid: 200, ppid: 1, pgid: 200},
	}
	var got []int
	for _, e := range descendants(table, 100) {
		got = append(got, e.pid)
	}
	slices.Sort(got)
	if !slices.Equal(got, []int{101, 102, 103}) {
		t.Fatalf("descendants %v", got)
	}
}
