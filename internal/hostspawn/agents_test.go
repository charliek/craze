package hostspawn

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/charliek/craze/internal/rundir"
)

// testStep bounds each wait of a test on its own, as internal/cli's serveStep
// does: long, since the tests run starved too.
const testStep = 30 * time.Second

func fileExists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// groupLeader is a process of the test's own leading a process group of its
// own, as an ACP agent does: its pid, the group's id; exited is closed once it
// has exited and been reaped, and err is then its Wait's. It is killed and
// reaped when the test ends, if it is still there.
type groupLeader struct {
	pid    int
	start  uint64
	exited chan struct{}
	err    error
}

// leadGroup starts `sleep 60` leading a group of its own, and reads its start
// time.
func leadGroup(t *testing.T) *groupLeader {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	g := &groupLeader{pid: cmd.Process.Pid, exited: make(chan struct{})}
	go func() {
		g.err = cmd.Wait()
		close(g.exited)
	}()
	t.Cleanup(func() {
		select {
		case <-g.exited:
		default:
			_ = cmd.Process.Kill()
			<-g.exited
		}
	})
	id, err := rundir.ProcessIdentity(g.pid)
	if err != nil {
		t.Fatal(err)
	}
	g.start = id.Start
	return g
}

// killed waits, within testStep, for g to have died of SIGKILL.
func (g *groupLeader) killed(t *testing.T) {
	t.Helper()
	select {
	case <-g.exited:
	case <-time.After(testStep):
		t.Fatalf("process %d was not killed within %v", g.pid, testStep)
	}
	var ee *exec.ExitError
	if !errors.As(g.err, &ee) {
		t.Fatalf("process %d exited %v, want killed", g.pid, g.err)
	}
	if ws, ok := ee.Sys().(syscall.WaitStatus); !ok || !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
		t.Fatalf("process %d exited %v, want killed", g.pid, g.err)
	}
}

// TestSpawnKillsOnlyTheGroupsProvablyItsAgents (astra r5-c3 1): the spawner's
// last resort signals a recorded group only while the process leading it is
// the agent the host recorded — the group's number as its pid, and the start
// time the record has. Three groups the test leads, each a sleep in a group
// of its own: one recorded with a start time not its own — an agent that has
// gone, its number now a stranger's — survives; one recorded as it is is
// killed; one whose leader has exited and been reaped is not signalled. A
// line that names no group — a bare number, as a record without start times
// had, or group 1 — is not acted on. Each group left alone is noted in the
// host's log, and the record is removed. The stranger comes first in the
// record, so it would have been signalled before the agent that is.
func TestSpawnKillsOnlyTheGroupsProvablyItsAgents(t *testing.T) {
	dir := t.TempDir()
	c := &Child{groups: filepath.Join(dir, "0123456789ab.pgids"), log: filepath.Join(dir, "0123456789ab.log")}
	stranger, agent, gone := leadGroup(t), leadGroup(t), leadGroup(t)
	if err := syscall.Kill(gone.pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	gone.killed(t)
	record := AgentGroup{PGID: stranger.pid, Start: stranger.start + 1}.Line() +
		AgentGroup{PGID: agent.pid, Start: agent.start}.Line() +
		AgentGroup{PGID: gone.pid, Start: gone.start}.Line() +
		strconv.Itoa(stranger.pid) + "\n" +
		"1 1\n"
	if err := os.WriteFile(c.groups, []byte(record), 0o600); err != nil {
		t.Fatal(err)
	}
	c.KillAgents()
	agent.killed(t)
	if fileExists(c.groups) {
		t.Fatal("the spawner left the record")
	}
	b, err := os.ReadFile(c.log)
	if err != nil {
		t.Fatal(err)
	}
	log := string(b)
	for _, want := range []string{
		fmt.Sprintf("craze: agent process group %d not killed: its number is another process's now", stranger.pid),
		fmt.Sprintf("craze: agent process group %d not killed: ", gone.pid),
		fmt.Sprintf("craze: a record that names no agent's process group not acted on: %q", strconv.Itoa(stranger.pid)),
		`craze: a record that names no agent's process group not acted on: "1 1"`,
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("the host's log does not say %q:\n%s", want, log)
		}
	}
	if strings.Contains(log, fmt.Sprintf("group %d not killed", agent.pid)) {
		t.Fatalf("the log says the agent was left:\n%s", log)
	}
	// The stranger was never signalled: its SIGKILL, had one been sent,
	// preceded the agent's, which has been delivered and reaped.
	select {
	case <-stranger.exited:
		t.Fatalf("the stranger's group was killed: %v", stranger.err)
	case <-time.After(200 * time.Millisecond):
	}
	if err := syscall.Kill(-stranger.pid, 0); err != nil {
		t.Fatalf("the stranger's group: %v", err)
	}
}
