package hostspawn

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/charliek/craze/internal/rundir"
)

// scope is this machine's boot and PID namespace, as a live host's record has them.
func scope(t *testing.T) (boot, pidNS string) {
	t.Helper()
	boot, pidNS, err := rundir.ProcessScope()
	if err != nil {
		t.Fatal(err)
	}
	return boot, pidNS
}

// deadHost is an owner that has gone: a real process's pid and start time,
// read while it ran.
func deadHost(t *testing.T) GroupOwner {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	id, err := rundir.ProcessIdentity(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	boot, pidNS := scope(t)
	return GroupOwner{PID: cmd.Process.Pid, Start: id.Start, Boot: boot, PIDNS: pidNS}
}

// liveHost is this test's own process as an owner.
func liveHost(t *testing.T) GroupOwner {
	t.Helper()
	o, err := NewGroupOwner()
	if err != nil {
		t.Fatal(err)
	}
	return o
}

// a child this test started, or one of those's: a process it will end if the test
// does not, checked by pid and start time, so nothing but this test's own is ever
// signalled by the cleanup.
type proc struct {
	pid   int
	start uint64
}

func (p proc) alive() bool {
	id, err := rundir.ProcessIdentity(p.pid)
	return err == nil && id.Start == p.start && !id.Zombie
}

// awaitGone waits for p to have died, within testStep.
func (p proc) awaitGone(t *testing.T, what string) {
	t.Helper()
	deadline := time.Now().Add(testStep)
	for p.alive() {
		if time.Now().After(deadline) {
			t.Fatalf("%s (%d) was still running after %v", what, p.pid, testStep)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// survives checks p is running, after everything that could have signalled it
// has returned: a signal sent has been delivered by then to anything running, so
// give the process a moment to read as dead if it was hit.
func (p proc) survives(t *testing.T, what string) {
	t.Helper()
	time.Sleep(100 * time.Millisecond)
	if !p.alive() {
		t.Fatalf("%s (%d) was signalled", what, p.pid)
	}
}

// markHost is the host every marked test command names (shellCommand), and the id of
// the record that must recognise one with its leader gone.
const markHost = "0a0b0c0d0e0f"

// command is a shell command as craze runs one: `sh -c script` in the temporary
// directory dir, as the leader of a session and a group of its own, carrying
// CommandMark(markHost) unless env says otherwise. reaped closes once the shell has exited
// and been reaped (this process is its parent, as a dead host no longer would
// be); what is left of its group is ended by the cleanup.
type command struct {
	proc
	dir string
	// mark is an entry only this command's environment has: what its cleanup goes by,
	// so that nothing but what it started is ever signalled, whoever has its group's
	// number by then.
	mark   string
	reaped chan struct{}
}

var testMarks atomic.Int64

// end is the test's cleanup: what is left in the command's group that carries its
// mark, killed by pid, and its shell reaped, each within testStep.
func (c *command) end(t *testing.T) {
	t.Helper()
	groups, err := rundir.ProcessGroups(time.Now().Add(testStep))
	if err != nil {
		t.Errorf("the cleanup could not scan: %v", err)
	}
	for _, m := range groups[c.pid] {
		if rundir.ProcessCarries(m.PID, c.mark) {
			_ = syscall.Kill(m.PID, syscall.SIGKILL)
		}
	}
	select {
	case <-c.reaped:
	case <-time.After(testStep):
		t.Errorf("the command's shell (%d) was not reaped within %v", c.pid, testStep)
	}
}

func runCommand(t *testing.T, env []string, attr *syscall.SysProcAttr, script string) *command {
	t.Helper()
	dir := t.TempDir()
	mark := fmt.Sprintf("CRAZE_REAP_TEST=%d-%d", os.Getpid(), testMarks.Add(1))
	cmd := exec.Command("sh", "-c", script)
	cmd.Dir = dir
	// Not the mark of a craze this test itself may be running under.
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, CommandMarkName+"=") {
			cmd.Env = append(cmd.Env, e)
		}
	}
	cmd.Env = append(append(cmd.Env, mark), env...)
	cmd.SysProcAttr = attr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	c := &command{dir: dir, mark: mark, reaped: make(chan struct{})}
	id, err := rundir.ProcessIdentity(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	c.proc = proc{cmd.Process.Pid, id.Start}
	go func() {
		_ = cmd.Wait()
		close(c.reaped)
	}()
	t.Cleanup(func() { c.end(t) })
	return c
}

// shellCommand is a command as craze runs one: marked, and in its own session.
func shellCommand(t *testing.T, script string) *command {
	t.Helper()
	return runCommand(t, []string{CommandMark(markHost)}, &syscall.SysProcAttr{Setsid: true}, script)
}

// child is the process whose pid the command wrote to name (the whole line).
func (c *command) child(t *testing.T, name string) proc {
	t.Helper()
	deadline := time.Now().Add(testStep)
	for {
		b, err := os.ReadFile(filepath.Join(c.dir, name))
		if err == nil && strings.HasSuffix(string(b), "\n") {
			pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
			if err != nil {
				t.Fatal(err)
			}
			id, err := rundir.ProcessIdentity(pid)
			if err != nil {
				t.Fatalf("the command's %s: %v", name, err)
			}
			return proc{pid, id.Start}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the command never wrote %s", name)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// awaitExit waits for the shell itself to have exited and been reaped.
func (c *command) awaitExit(t *testing.T) {
	t.Helper()
	select {
	case <-c.reaped:
	case <-time.After(testStep):
		t.Fatal("the command's shell did not exit")
	}
}

// recordIn writes host id's record in env's host logs: its path.
func recordIn(t *testing.T, env rundir.Env, id string, gs ...AgentGroup) string {
	t.Helper()
	dir, err := rundir.HostLogDir(env)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, g := range gs {
		b.WriteString(g.Line())
	}
	path := filepath.Join(dir, AgentGroupsName(id))
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func (c *command) entry(owner GroupOwner) AgentGroup {
	return AgentGroup{PGID: c.pid, Start: c.start, Kind: GroupShell, Owner: owner}
}

func noNote(string, ...any) {}

// TestParseAgentGroupLines is the record's two forms, and what it refuses.
func TestParseAgentGroupLines(t *testing.T) {
	owner := GroupOwner{PID: 42, Start: 7, Boot: "boot", PIDNS: "ns"}
	for _, g := range []AgentGroup{
		{PGID: 9, Start: 3},
		{PGID: 9, Start: 3, Kind: GroupShell, Owner: owner},
		{PGID: 9, Start: 3, Kind: GroupAgent, Owner: owner},
	} {
		got, ok := ParseAgentGroup(g.Line())
		if !ok || got != g {
			t.Fatalf("%q reads back as %+v, %v", g.Line(), got, ok)
		}
	}
	for _, line := range []string{
		"", "9", "9 x", "x 3", "0 3", "-9 3", "4294967296 3", "9 3 shell", "9 3 other 42 7 boot ns",
		"9 3 shell 42 7 boot", "9 3 shell x 7 boot ns", "9 3 shell 42 x boot ns", "9 3 shell 1 7 boot ns",
		"9 3 shell 0 7 boot ns", "9 3 shell -2 7 boot ns", "9 3 shell 42 0 boot ns", "9 0 shell 42 7 boot ns",
		"9 3 shell 42 7 boot ns extra", "9 3 shell 4294967296 7 boot ns",
	} {
		if g, ok := ParseAgentGroup(line); ok {
			t.Fatalf("%q read as %+v", line, g)
		}
	}
}

// TestReapAWholeGroupByItsLiveLeader: the leader alive and its identity matching,
// the whole group goes, a child of the shell included, and the record with it.
func TestReapAWholeGroupByItsLiveLeader(t *testing.T) {
	env := holderEnv(t)
	c := shellCommand(t, `sleep 120 & echo $! > pid.child; wait`)
	kid := c.child(t, "pid.child")
	id := rundir.NewHostID()
	path := recordIn(t, env, id, c.entry(deadHost(t)))
	ReapHost(context.Background(), env, id, noNote)
	c.awaitGone(t, "the shell")
	kid.awaitGone(t, "the shell's child")
	if fileExists(path) {
		t.Fatal("the record was left")
	}
}

// TestReapAGroupWhoseLeaderHasGone: the leader exited and reaped, a backgrounded
// grandchild lives on in its group, and the scan finds and kills it.
func TestReapAGroupWhoseLeaderHasGone(t *testing.T) {
	env := holderEnv(t)
	c := shellCommand(t, `sleep 120 & echo $! > pid.child`)
	kid := c.child(t, "pid.child")
	c.awaitExit(t)
	path := recordIn(t, env, markHost, c.entry(deadHost(t)))
	SweepDeadHosts(env, nil)
	kid.awaitGone(t, "the orphaned grandchild")
	if fileExists(path) {
		t.Fatal("the record was left")
	}
}

// TestReapSparesWhatItCannotTellFromAStranger: a group whose number is the record's
// and whose leader has gone, in each way what is left might not be the command's:
// nothing carrying the mark, in a session that is not the group's, or started before
// the leader recorded. It may be a stranger that took the number once the
// command's own group had emptied. Left running, and the record dropped.
func TestReapSparesWhatItCannotTellFromAStranger(t *testing.T) {
	session := &syscall.SysProcAttr{Setsid: true}
	for _, tc := range []struct {
		name  string
		env   []string
		attr  *syscall.SysProcAttr
		start func(uint64) uint64
	}{
		{name: "no mark", attr: session},
		{name: "a mark that only looks like it", env: []string{"X" + CommandMark(markHost), CommandMark(markHost) + "0"}, attr: session},
		// Dead host A's stale record, and live host B's daemon on A's old number.
		{name: "another host's mark", env: []string{"CRAZE_AGENT=1", CommandMark("ffffffffffff")}, attr: session},
		{name: "the global mark alone", env: []string{"CRAZE_AGENT=1"}, attr: session},
		{name: "not the group's session", env: []string{CommandMark(markHost)}, attr: &syscall.SysProcAttr{Setpgid: true}},
		{name: "older than the leader written down", env: []string{CommandMark(markHost)}, attr: session, start: func(s uint64) uint64 { return s + 1<<40 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := holderEnv(t)
			c := runCommand(t, tc.env, tc.attr, `sleep 120 & echo $! > pid.child`)
			kid := c.child(t, "pid.child")
			c.awaitExit(t)
			g := c.entry(deadHost(t))
			if tc.start != nil {
				g.Start = tc.start(g.Start)
			}
			path := recordIn(t, env, markHost, g)
			ReapHost(context.Background(), env, markHost, noNote)
			kid.survives(t, "what is left of the group")
			if fileExists(path) {
				t.Fatal("the record was left")
			}
		})
	}
}

// TestReapNeverSignalsAReusedPID: the group's number is a live process's with
// another start time, its pid reused: never signalled, and the entry dropped.
func TestReapNeverSignalsAReusedPID(t *testing.T) {
	env := holderEnv(t)
	c := shellCommand(t, `sleep 120 & echo $! > pid.child; wait`)
	kid := c.child(t, "pid.child")
	id := rundir.NewHostID()
	g := c.entry(deadHost(t))
	g.Start++
	path := recordIn(t, env, id, g)
	ReapHost(context.Background(), env, id, noNote)
	c.survives(t, "the stranger with the number")
	kid.survives(t, "the stranger's child")
	if fileExists(path) {
		t.Fatal("the record was left")
	}
}

// TestReapDropsARecordWithNoLiveProcess: nothing has the number, and nothing is
// in a group with it.
func TestReapDropsARecordWithNoLiveProcess(t *testing.T) {
	env := holderEnv(t)
	c := shellCommand(t, `:`)
	c.awaitExit(t)
	id := rundir.NewHostID()
	path := recordIn(t, env, id, c.entry(deadHost(t)))
	SweepDeadHosts(env, nil)
	if fileExists(path) {
		t.Fatal("the record was left")
	}
}

// TestReapKeepsOffAHealthyHost: a sweep leaves a running host's groups and its
// record alone, whatever they say, and with a run of them costs only reads.
func TestReapKeepsOffAHealthyHost(t *testing.T) {
	env := holderEnv(t)
	c := shellCommand(t, `sleep 120 & echo $! > pid.child; wait`)
	kid := c.child(t, "pid.child")
	id := rundir.NewHostID()
	path := recordIn(t, env, id, c.entry(liveHost(t)))
	SweepDeadHosts(env, nil)
	ReapHost(context.Background(), env, id, noNote) // a caller wrong about the host costs nothing
	c.survives(t, "a healthy host's shell")
	kid.survives(t, "a healthy host's child")
	if !fileExists(path) {
		t.Fatal("a healthy host's record was removed")
	}
}

// TestReapKeepsOffAHalfWrittenRecord: a record with nothing in it to read an owner
// from yet, as a host's first write might be seen half done, is not a dead host's.
func TestReapKeepsOffAHalfWrittenRecord(t *testing.T) {
	env := holderEnv(t)
	c := shellCommand(t, `sleep 120 & echo $! > pid.child; wait`)
	kid := c.child(t, "pid.child")
	id := rundir.NewHostID()
	path := recordIn(t, env, id)
	line := strings.TrimSuffix(c.entry(deadHost(t)).Line(), "\n")
	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	SweepDeadHosts(env, nil)
	c.survives(t, "the shell a half-written line names")
	kid.survives(t, "its child")
	if !fileExists(path) {
		t.Fatal("the half-written record was removed")
	}
}

// TestReapReadsAWholeLineBesideAHalfOne: a dead host's torn last line is not an
// entry, and the whole one before it is.
func TestReapReadsAWholeLineBesideAHalfOne(t *testing.T) {
	env := holderEnv(t)
	whole, torn := shellCommand(t, `sleep 120`), shellCommand(t, `sleep 120`)
	owner := deadHost(t)
	id := rundir.NewHostID()
	path := recordIn(t, env, id)
	text := whole.entry(owner).Line() + strings.TrimSuffix(torn.entry(owner).Line(), "\n")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	SweepDeadHosts(env, nil)
	whole.awaitGone(t, "the whole line's shell")
	torn.survives(t, "the torn line's shell")
}

// TestReapLeavesAnotherPlacesRecord: a record of another boot or PID namespace
// names processes by numbers that mean nothing here: not one signalled, and it
// is left for the logs' sweep.
func TestReapLeavesAnotherPlacesRecord(t *testing.T) {
	env := holderEnv(t)
	c := shellCommand(t, `sleep 120`)
	for name, change := range map[string]func(*GroupOwner){
		"boot":      func(o *GroupOwner) { o.Boot = "another-boot" },
		"namespace": func(o *GroupOwner) { o.PIDNS = "another-namespace" },
	} {
		t.Run(name, func(t *testing.T) {
			owner := deadHost(t)
			change(&owner)
			id := rundir.NewHostID()
			path := recordIn(t, env, id, c.entry(owner))
			SweepDeadHosts(env, nil)
			ReapHost(context.Background(), env, id, noNote)
			c.survives(t, "the shell whose number the record has")
			if !fileExists(path) {
				t.Fatal("the record was removed")
			}
		})
	}
}

// TestReapKeepsOffAnAgentWhoseLeaderHasGone: an agent's entry is signalled only
// while its leader lives. Its entry is not removed when the agent exits, so a group
// with its number and no such leader, carrying the mark even, is not looked into.
// A live leader's group goes, with its grace.
func TestReapKeepsOffAnAgentWhoseLeaderHasGone(t *testing.T) {
	env := holderEnv(t)
	gone := shellCommand(t, `sleep 120 & echo $! > pid.child`)
	kid := gone.child(t, "pid.child")
	gone.awaitExit(t)
	agent := leadGroup(t)
	owner := deadHost(t)
	id := rundir.NewHostID()
	path := recordIn(t, env, id,
		AgentGroup{PGID: gone.pid, Start: gone.start, Kind: GroupAgent, Owner: owner},
		AgentGroup{PGID: agent.pid, Start: agent.start, Kind: GroupAgent, Owner: owner})
	SweepDeadHosts(env, nil)
	agent.terminated(t)
	kid.survives(t, "what the gone agent's number holds")
	if fileExists(path) {
		t.Fatal("the record was left")
	}
}

// TestRecordsThatNameNothingOfOurs: a record corrupted on disk can never have
// anything signalled that it could not have started: 0, 1, -1 and the rest are
// refused at the line, and the caller's own group and pid and its host's own are
// refused at the entry. Run in a process of its own, leading a session of its own,
// whose exit says whether it was hit: a mistake here takes out nothing but it.
func TestRecordsThatNameNothingOfOurs(t *testing.T) {
	if os.Getenv("CRAZE_REAP_CORRUPT_CALLER") == "1" {
		corruptRecordCaller()
		return
	}
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "-test.run=^TestRecordsThatNameNothingOfOurs$", "-test.v")
	cmd.Dir = t.TempDir()
	cmd.Env = append(os.Environ(), "CRAZE_REAP_CORRUPT_CALLER=1", "HOME="+cmd.Dir)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the caller: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "the caller survived") {
		t.Fatalf("the caller did not say it survived:\n%s", out)
	}
}

// corruptRecordCaller is TestRecordsThatNameNothingOfOurs's caller: it starts a
// child in its own group, as a craze the agent ran would have the agent's shell,
// and reaps a record that names its group, itself, and worse.
func corruptRecordCaller() {
	fail := func(format string, args ...any) {
		fmt.Printf(format+"\n", args...)
		os.Exit(1)
	}
	inGroup := exec.Command("sleep", "60")
	if err := inGroup.Start(); err != nil {
		fail("%v", err)
	}
	defer func() {
		_ = inGroup.Process.Kill()
		_ = inGroup.Wait()
	}()
	home, err := os.MkdirTemp(os.Getenv("HOME"), "home")
	if err != nil {
		fail("%v", err)
	}
	env := rundir.Env{Home: home, CrazeDir: filepath.Join(home, ".craze"), CrazeRuntimeDir: home, EUID: os.Geteuid()}
	dir, err := rundir.HostLogDir(env)
	if err != nil {
		fail("%v", err)
	}
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		fail("%v", err)
	}
	hostID, _ := rundir.ProcessIdentity(cmd.Process.Pid)
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	boot, pidNS, err := rundir.ProcessScope()
	if err != nil {
		fail("%v", err)
	}
	owner := GroupOwner{PID: cmd.Process.Pid, Start: hostID.Start, Boot: boot, PIDNS: pidNS}
	own, _ := rundir.ProcessIdentity(os.Getpid())
	suffix := fmt.Sprintf(" shell %d %d %s %s\n", owner.PID, owner.Start, boot, pidNS)
	agentLine := AgentGroup{PGID: syscall.Getpgrp(), Start: own.Start, Kind: GroupAgent, Owner: owner}.Line()
	agentID := rundir.NewHostID()
	agentPath := filepath.Join(dir, AgentGroupsName(agentID))
	if err := os.WriteFile(agentPath, []byte(agentLine), 0o600); err != nil {
		fail("%v", err)
	}
	lines := []string{
		"0 1" + suffix, "1 1" + suffix, "-1 1" + suffix, "-" + strconv.Itoa(os.Getpid()) + " 1" + suffix,
		// Its own session and group, by the caller's own start time: every other
		// check it could meet says "signal this".
		strconv.Itoa(os.Getpid()) + " " + strconv.FormatUint(own.Start, 10) + suffix,
		strconv.Itoa(syscall.Getpgrp()) + " " + strconv.FormatUint(own.Start, 10) + suffix,
		// Its host's own number, whatever has it.
		strconv.Itoa(owner.PID) + " " + strconv.FormatUint(owner.Start, 10) + suffix,
		"0 1\n", "1 1\n", "-1 1\n", "garbage\n",
	}
	id := rundir.NewHostID()
	path := filepath.Join(dir, AgentGroupsName(id))
	if err := os.WriteFile(path, []byte(strings.Join(lines, "")), 0o600); err != nil {
		fail("%v", err)
	}
	SweepDeadHosts(env, nil)
	ReapHost(context.Background(), env, id, noNote)
	time.Sleep(50 * time.Millisecond)
	if err := syscall.Kill(inGroup.Process.Pid, 0); err != nil {
		fail("the caller's group was signalled: %v", err)
	}
	if !fileExists(path) || !fileExists(agentPath) {
		fail("a record naming the caller's own group was removed: a craze outside the group should finish it")
	}
	fmt.Println("the caller survived")
}

// TestReapIgnoresAnythingButAPlainFile: a link is never followed, and a file of
// another shape is not read as a record.
func TestReapIgnoresAnythingButAPlainFile(t *testing.T) {
	env := holderEnv(t)
	c := shellCommand(t, `sleep 120`)
	target := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.WriteFile(target, []byte(c.entry(deadHost(t)).Line()), 0o600); err != nil {
		t.Fatal(err)
	}
	dir, err := rundir.HostLogDir(env)
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, AgentGroupsName(rundir.NewHostID()))
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, AgentGroupsName(rundir.NewHostID())), 0o700); err != nil {
		t.Fatal(err)
	}
	SweepDeadHosts(env, nil)
	c.survives(t, "the shell a linked file names")
	if !fileExists(link) || !fileExists(target) {
		t.Fatal("the link or its target was removed")
	}
}

// TestAFileOfAnotherUsersIsNoRecord: owned by anyone else, a record is not read. A
// test cannot make such a file without root, so this asks the question directly.
func TestAFileOfAnotherUsersIsNoRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !ownedBy(st, os.Geteuid()) || ownedBy(st, os.Geteuid()+1) {
		t.Fatalf("owned by this user: %v; by another: %v", ownedBy(st, os.Geteuid()), ownedBy(st, os.Geteuid()+1))
	}
}

// TestASweepKeepsOutOfADirectoryItCannotTrust: a host-logs directory others could
// write in holds nothing a sweep may believe.
func TestASweepKeepsOutOfADirectoryItCannotTrust(t *testing.T) {
	env := holderEnv(t)
	c := shellCommand(t, `sleep 120`)
	path := recordIn(t, env, rundir.NewHostID(), c.entry(deadHost(t)))
	dir := filepath.Dir(path)
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	SweepDeadHosts(env, nil)
	c.survives(t, "the shell the record names")
	if !fileExists(path) {
		t.Fatal("the record was removed")
	}
}

// TestReapASigtermIgnoringChildGetsSIGKILL: SIGTERM, and SIGKILL only once the
// grace is over: what ignores the first goes at the second, however it was reached.
func TestReapASigtermIgnoringChildGetsSIGKILL(t *testing.T) {
	env := holderEnv(t)
	c := shellCommand(t, `trap '' TERM; sleep 120 & echo $! > pid.child; wait`)
	kid := c.child(t, "pid.child")
	id := rundir.NewHostID()
	path := recordIn(t, env, id, c.entry(deadHost(t)))
	began := time.Now()
	ReapHost(context.Background(), env, id, noNote)
	if took := time.Since(began); took < ReapGrace {
		t.Fatalf("the reap was over in %v, inside the grace", took)
	}
	c.awaitGone(t, "the shell ignoring SIGTERM")
	kid.awaitGone(t, "its child")
	if fileExists(path) {
		t.Fatal("the record was left")
	}
}

// TestReapAStoppedGroup: a group stopped when its host died still goes.
func TestReapAStoppedGroup(t *testing.T) {
	env := holderEnv(t)
	c := shellCommand(t, `sleep 120 & echo $! > pid.child; kill -STOP $$`)
	kid := c.child(t, "pid.child")
	id := rundir.NewHostID()
	recordIn(t, env, id, c.entry(deadHost(t)))
	ReapHost(context.Background(), env, id, noNote)
	c.awaitGone(t, "the stopped shell")
	kid.awaitGone(t, "its child")
}

// TestReapAZombieLeader: a zombie is still in the group, still "found", but
// no signal does anything to it: with nothing else there it is nothing left to
// wait for, and the reap ends with the record gone; with something live beside it
// that goes. The process stays a zombie throughout: this test holds it unreaped.
func TestReapAZombieLeader(t *testing.T) {
	for _, tc := range []struct {
		name, script string
	}{
		{"alone", `exit 0`},
		{"beside a live child", `sleep 120 & echo $! > pid.child`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := holderEnv(t)
			dir := t.TempDir()
			cmd := exec.Command("sh", "-c", tc.script)
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), CommandMark(markHost))
			cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = cmd.Wait() })
			pid := cmd.Process.Pid
			deadline := time.Now().Add(testStep)
			var id rundir.ProcIdentity
			for {
				var err error
				if id, err = rundir.ProcessIdentity(pid); err != nil {
					t.Fatal(err)
				}
				if id.Zombie {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("the leader never exited")
				}
				time.Sleep(5 * time.Millisecond)
			}
			var kid *proc
			if tc.script != `exit 0` {
				c := &command{dir: dir}
				k := c.child(t, "pid.child")
				kid = &k
				t.Cleanup(func() {
					if kid.alive() {
						_ = syscall.Kill(kid.pid, syscall.SIGKILL)
					}
				})
			}
			hostID := rundir.NewHostID()
			path := recordIn(t, env, hostID, AgentGroup{PGID: pid, Start: id.Start, Kind: GroupShell, Owner: deadHost(t)})
			ReapHost(context.Background(), env, hostID, noNote)
			if kid != nil {
				kid.awaitGone(t, "the zombie's live child")
			}
			if fileExists(path) {
				t.Fatal("the record was left")
			}
		})
	}
}

// TestTwoSweepsAtOnce: sweeps of the same records at the same time are harmless:
// each record is acted on by one at a time, every group goes, every record goes,
// and nobody hangs or waits on another past what its own bound allows.
func TestTwoSweepsAtOnce(t *testing.T) {
	env := holderEnv(t)
	var cs []*command
	var kids []proc
	var paths []string
	owner := deadHost(t)
	for range 4 {
		c := shellCommand(t, `sleep 120 & echo $! > pid.child; wait`)
		cs = append(cs, c)
		kids = append(kids, c.child(t, "pid.child"))
		paths = append(paths, recordIn(t, env, rundir.NewHostID(), c.entry(owner)))
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			SweepDeadHosts(env, nil)
		}()
	}
	wg.Wait()
	for i, c := range cs {
		c.awaitGone(t, "a shell")
		kids[i].awaitGone(t, "a child")
		// All eight returned: one of them took each record, and removed it.
		if fileExists(paths[i]) {
			t.Fatalf("record %d was left", i)
		}
	}
}

// TestReapWaitsForAnotherSweep: a waiter that meets another sweep at its host's
// record waits for it, so that when it returns the group has had its signals, the
// holder's. A holder that lets go having done nothing leaves the waiter to do it.
func TestReapWaitsForAnotherSweep(t *testing.T) {
	for _, tc := range []struct {
		name string
		let  func(*groupRecord)
	}{
		{"the holder ends it", func(held *groupRecord) {
			recs := []*groupRecord{held}
			r := &reaper{ctx: context.Background(), firstBy: time.Now().Add(2 * sweepRead), grace: ReapGrace}
			r.second(recs, r.first(recs))
		}},
		{"the holder lets it go", func(held *groupRecord) { held.release() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := holderEnv(t)
			c := shellCommand(t, `exec sleep 120`)
			id := rundir.NewHostID()
			path := recordIn(t, env, id, c.entry(deadHost(t)))
			held, _ := openRecord(path, noNote)
			if held == nil {
				t.Fatal("the record could not be taken")
			}
			done := make(chan struct{})
			go func() {
				ReapHost(context.Background(), env, id, noNote)
				close(done)
			}()
			select {
			case <-done:
				t.Fatal("the reap did not wait for the sweep holding the record")
			case <-time.After(3 * groupsPoll):
			}
			if !c.alive() {
				t.Fatal("the group was signalled with another's lock held")
			}
			tc.let(held)
			select {
			case <-done:
			case <-time.After(testStep):
				t.Fatal("the reap did not end")
			}
			// The postcondition: its return is the group's end, signalled by now.
			c.awaitGone(t, "the shell")
			if fileExists(path) {
				t.Fatal("the record was left")
			}
		})
	}
}

// TestAReapCutShortLeavesTheRecord: the waiter's context ending, in the grace or
// waiting for another's lock, returns at once, sends nothing more and leaves the
// record whole, however old, for the next sweep, which finishes the job.
func TestAReapCutShortLeavesTheRecord(t *testing.T) {
	env := holderEnv(t)
	c := shellCommand(t, `trap '' TERM; exec sleep 120`)
	id := rundir.NewHostID()
	path := recordIn(t, env, id, c.entry(deadHost(t)))
	long := time.Now().Add(-2 * RecordKeep)
	if err := os.Chtimes(path, long, long); err != nil {
		t.Fatal(err)
	}
	waiting := make(chan struct{})
	waitForGroupsIs(t, func(ctx context.Context, _ []entry, _ time.Time) {
		close(waiting)
		<-ctx.Done()
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		ReapHost(ctx, env, id, noNote)
		close(done)
	}()
	<-waiting
	cancel()
	select {
	case <-done:
	case <-time.After(testStep):
		t.Fatal("the reap did not end with its context")
	}
	c.survives(t, "the shell ignoring SIGTERM")
	if !fileExists(path) {
		t.Fatal("the record was removed")
	}

	held, _ := openRecord(path, noNote)
	if held == nil {
		t.Fatal("the record could not be taken")
	}
	// Its own bound is no help here: only the context can end this wait.
	prevWait := reapLockWait
	reapLockWait = time.Hour
	t.Cleanup(func() { reapLockWait = prevWait })
	ctx, cancel = context.WithCancel(context.Background())
	done = make(chan struct{})
	go func() {
		ReapHost(ctx, env, id, noNote)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(testStep):
		t.Fatal("the wait for another's lock did not end with its context")
	}
	held.release()
	if !fileExists(path) {
		t.Fatal("the record was removed")
	}

	waitForGroups = waitGroups
	SweepDeadHosts(env, nil)
	c.awaitGone(t, "the shell ignoring SIGTERM")
	if fileExists(path) {
		t.Fatal("the next sweep left the record")
	}
}

// waitForGroupsIs replaces the grace for a test, never in parallel.
func waitForGroupsIs(t *testing.T, wait func(context.Context, []entry, time.Time)) {
	t.Helper()
	prev := waitForGroups
	waitForGroups = wait
	t.Cleanup(func() { waitForGroups = prev })
}

// TestASweepExitingBeforeSIGKILLLeavesTheRecord: a short command exits with the grace
// not over: its process dies with SIGKILL unsent, and what it took nothing from is
// there for the next. Run in a process of its own that really exits there.
func TestASweepExitingBeforeSIGKILLLeavesTheRecord(t *testing.T) {
	if os.Getenv("CRAZE_REAP_EXITING_SWEEP") != "" {
		home := os.Getenv("CRAZE_REAP_EXITING_SWEEP")
		env := rundir.Env{Home: home, CrazeDir: filepath.Join(home, ".craze"), CrazeRuntimeDir: home, EUID: os.Geteuid()}
		waitForGroups = func(context.Context, []entry, time.Time) { os.Exit(0) }
		SweepDeadHosts(env, nil)
		os.Exit(1)
	}
	env := holderEnv(t)
	c := shellCommand(t, `trap '' TERM; exec sleep 120`)
	path := recordIn(t, env, rundir.NewHostID(), c.entry(deadHost(t)))
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "-test.run=^TestASweepExitingBeforeSIGKILLLeavesTheRecord$")
	cmd.Env = append(os.Environ(), "CRAZE_REAP_EXITING_SWEEP="+env.Home)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the sweep: %v\n%s", err, out)
	}
	c.survives(t, "the shell ignoring SIGTERM")
	if !fileExists(path) {
		t.Fatal("the record went with the process")
	}
	SweepDeadHosts(env, nil)
	c.awaitGone(t, "the shell ignoring SIGTERM")
	if fileExists(path) {
		t.Fatal("the next sweep left the record")
	}
}

// TestAHostsLastWordsAreRead: the host writes B after the read that named it, and
// dies. Death is established first and the record read after, so B is ended too and
// the record removed; a read before it would have ended A and removed B's line
// unsignalled. A rewrite swapped into place at the same instant is taken by its name.
func TestAHostsLastWordsAreRead(t *testing.T) {
	for _, tc := range []struct {
		name  string
		write func(path, line string) error
	}{
		{"an append", func(path, line string) error {
			f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
			if err != nil {
				return err
			}
			defer f.Close()
			_, err = f.WriteString(line)
			return err
		}},
		{"an atomic replacement", func(path, line string) error {
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			tmp := path + ".tmp"
			if err := os.WriteFile(tmp, append(b, line...), 0o600); err != nil {
				return err
			}
			return os.Rename(tmp, path)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := holderEnv(t)
			a, b := shellCommand(t, `exec sleep 120`), shellCommand(t, `exec sleep 120`)
			host := exec.Command("sleep", "120")
			if err := host.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				_ = host.Process.Kill()
				_, _ = host.Process.Wait()
			})
			hostID, err := rundir.ProcessIdentity(host.Process.Pid)
			if err != nil {
				t.Fatal(err)
			}
			boot, pidNS := scope(t)
			owner := GroupOwner{PID: host.Process.Pid, Start: hostID.Start, Boot: boot, PIDNS: pidNS}
			path := recordIn(t, env, rundir.NewHostID(), a.entry(owner))
			prev := recordPeeked
			recordPeeked = func(p string) {
				recordPeeked = prev
				if err := tc.write(p, b.entry(owner).Line()); err != nil {
					t.Error(err)
				}
				_ = host.Process.Kill()
				_ = host.Wait()
			}
			t.Cleanup(func() { recordPeeked = prev })
			SweepDeadHosts(env, nil)
			a.awaitGone(t, "the host's first command")
			b.awaitGone(t, "the command its last write named")
			if fileExists(path) {
				t.Fatal("the record was left")
			}
		})
	}
}

// signalsAre records every signal, sent as ever, and runs each after the record.
func signalsAre(t *testing.T, after func(pid int, sig syscall.Signal)) *[]string {
	t.Helper()
	var mu sync.Mutex
	sent := &[]string{}
	prev := sendSignal
	sendSignal = func(pid int, sig syscall.Signal) error {
		mu.Lock()
		*sent = append(*sent, fmt.Sprintf("%d %v", -pid, sig))
		mu.Unlock()
		err := syscall.Kill(pid, sig)
		if after != nil {
			after(-pid, sig)
		}
		return err
	}
	t.Cleanup(func() { sendSignal = prev })
	return sent
}

// TestAGroupIsReadStraightBeforeItsOwnSignal: each group is read again for its own
// signal, in both rounds, whatever another group's took. While one group's signal goes
// out the next group's leader exits and is reaped, its number free: it has no signal,
// where a list read beforehand would have sent one to whoever had it by then.
func TestAGroupIsReadStraightBeforeItsOwnSignal(t *testing.T) {
	for _, round := range []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL} {
		t.Run(round.String(), func(t *testing.T) {
			env := holderEnv(t)
			first := shellCommand(t, `trap '' TERM; exec sleep 120`)
			next := shellCommand(t, `trap '' TERM; exec sleep 120`)
			owner := deadHost(t)
			id := rundir.NewHostID()
			recordIn(t, env, id, first.entry(owner), next.entry(owner))
			var once sync.Once
			sent := signalsAre(t, func(pid int, sig syscall.Signal) {
				if pid == first.pid && sig == round {
					once.Do(func() {
						_ = syscall.Kill(next.pid, syscall.SIGKILL)
						next.awaitExit(t)
					})
				}
			})
			waitForGroupsIs(t, func(context.Context, []entry, time.Time) {})
			ReapHost(context.Background(), env, id, noNote)
			for _, s := range *sent {
				if strings.HasPrefix(s, strconv.Itoa(next.pid)+" ") && strings.HasSuffix(s, round.String()) {
					t.Fatalf("the group whose leader went was sent %q: %q", s, *sent)
				}
			}
		})
	}
}

// TestWhatHoldsTheNumberIsReadLast: the read that decides a group can take a scan and
// environment reads. What holds its number, the leader or the member that was
// recognised, goes in that time, and its number is free: no signal, in either case.
func TestWhatHoldsTheNumberIsReadLast(t *testing.T) {
	for _, tc := range []struct {
		name, script string
		wait         bool
	}{
		{"the leader", `exec sleep 120`, false},
		{"the member", `sleep 120 & echo $! > pid.child`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := holderEnv(t)
			c := shellCommand(t, tc.script)
			holder := c.proc
			if tc.wait {
				holder = c.child(t, "pid.child")
				c.awaitExit(t)
			}
			recordIn(t, env, markHost, c.entry(deadHost(t)))
			prev := groupRead
			t.Cleanup(func() { groupRead = prev })
			groupRead = func(AgentGroup) {
				_ = syscall.Kill(holder.pid, syscall.SIGKILL)
				holder.awaitGone(t, "what held the number")
				deadline := time.Now().Add(testStep)
				for {
					if _, err := rundir.ProcessMember(holder.pid); errors.Is(err, rundir.ErrNoProcess) {
						return
					}
					if time.Now().After(deadline) {
						t.Error("what held the number was never reaped")
						return
					}
					time.Sleep(5 * time.Millisecond)
				}
			}
			sent := signalsAre(t, nil)
			waitForGroupsIs(t, func(context.Context, []entry, time.Time) {})
			ReapHost(context.Background(), env, markHost, noNote)
			if len(*sent) != 0 {
				t.Fatalf("a number that had gone free was signalled: %q", *sent)
			}
		})
	}
}

// TestAReusedHostIDIsNotTheOldHosts: a host id used again after a clean exit, with the
// old spawner's watch resuming late: the record at the name is the new host's. Its
// commands run on and its record stays. Neither a record of lines that disagree nor
// one naming another start time of the same pid is acted on.
func TestAReusedHostIDIsNotTheOldHosts(t *testing.T) {
	old := deadHost(t)
	samePID := old
	samePID.Start++
	for name, body := range map[string]func(*command) string{
		"another host":     func(c *command) string { return c.entry(liveHost(t)).Line() },
		"another start":    func(c *command) string { return c.entry(samePID).Line() },
		"lines that clash": func(c *command) string { return c.entry(old).Line() + c.entry(liveHost(t)).Line() },
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "0123456789ab.pgids")
			c := shellCommand(t, `exec sleep 120`)
			if err := os.WriteFile(path, []byte(body(c)), 0o600); err != nil {
				t.Fatal(err)
			}
			watcher := &Child{pid: old.PID, start: old.Start, groups: path, log: filepath.Join(dir, "0123456789ab.log")}
			watcher.KillAgents()
			c.survives(t, "the new host's command")
			if !fileExists(path) {
				t.Fatal("the new host's record was removed")
			}
		})
	}
}

// TestACleanupLeavesAReplacementAtItsName: the async end of a spawner's cleanup, and a
// drop of an aged record, each meet another host's record at the name, put there while
// they were at work. It is not theirs to remove.
func TestACleanupLeavesAReplacementAtItsName(t *testing.T) {
	replace := func(t *testing.T, path string) {
		t.Helper()
		line := AgentGroup{PGID: 1 << 22, Start: 1, Kind: GroupShell, Owner: liveHost(t)}.Line()
		if err := os.WriteFile(path+".tmp", []byte(line), 0o600); err != nil {
			t.Error(err)
		}
		if err := os.Rename(path+".tmp", path); err != nil {
			t.Error(err)
		}
	}
	t.Run("the end of a spawner's cleanup", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "0123456789ab.pgids")
		c := shellCommand(t, `trap '' TERM; exec sleep 120`)
		owner := deadHost(t)
		if err := os.WriteFile(path, []byte(c.entry(owner).Line()), 0o600); err != nil {
			t.Fatal(err)
		}
		inGrace, end := make(chan struct{}), make(chan struct{})
		waitForGroupsIs(t, func(context.Context, []entry, time.Time) {
			close(inGrace)
			<-end
		})
		(&Child{pid: owner.PID, start: owner.Start, groups: path}).KillAgents()
		<-inGrace
		replace(t, path)
		close(end)
		c.awaitGone(t, "the group the record it read named")
		time.Sleep(50 * time.Millisecond)
		if !fileExists(path) {
			t.Fatal("the cleanup removed another host's record")
		}
	})
	t.Run("a drop of an aged record", func(t *testing.T) {
		env := holderEnv(t)
		other := deadHost(t)
		other.Boot = "another-boot"
		c := shellCommand(t, `exec sleep 120`)
		path := recordIn(t, env, rundir.NewHostID(), c.entry(other))
		long := time.Now().Add(-2 * RecordKeep)
		if err := os.Chtimes(path, long, long); err != nil {
			t.Fatal(err)
		}
		prev := recordPeeked
		t.Cleanup(func() { recordPeeked = prev })
		recordPeeked = func(p string) { replace(t, p) }
		SweepDeadHosts(env, nil)
		if !fileExists(path) {
			t.Fatal("the drop removed another host's record")
		}
	})
}

// TestACancelStopsTheNextSignal: a context ended after a group has been read, in
// either round, sends nothing more, and the record stays, however old.
func TestACancelStopsTheNextSignal(t *testing.T) {
	for _, tc := range []struct {
		name string
		at   int // which of the group's reads the cancel lands in
		want []syscall.Signal
	}{
		{"before the first round", 1, nil},
		{"before the second round", 2, []syscall.Signal{syscall.SIGTERM, syscall.SIGCONT}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := holderEnv(t)
			c := shellCommand(t, `trap '' TERM; exec sleep 120`)
			id := rundir.NewHostID()
			path := recordIn(t, env, id, c.entry(deadHost(t)))
			long := time.Now().Add(-2 * RecordKeep)
			if err := os.Chtimes(path, long, long); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			reads := 0
			prev := groupRead
			t.Cleanup(func() { groupRead = prev })
			groupRead = func(AgentGroup) {
				if reads++; reads == tc.at {
					cancel()
				}
			}
			sent := signalsAre(t, nil)
			waitForGroupsIs(t, func(context.Context, []entry, time.Time) {})
			ReapHost(ctx, env, id, noNote)
			var want []string
			for _, sig := range tc.want {
				want = append(want, fmt.Sprintf("%d %v", c.pid, sig))
			}
			if !slices.Equal(*sent, want) {
				t.Fatalf("sent %q, want %q", *sent, want)
			}
			if !fileExists(path) {
				t.Fatal("the record was removed")
			}
		})
	}
}

// TestASearchOfAGroupStopsWhenTheRunDoes: the search for a member that is the host's
// reads an environment a member, so it answers to the context and the round's bound.
func TestASearchOfAGroupStopsWhenTheRunDoes(t *testing.T) {
	c := shellCommand(t, `sleep 120 & echo $! > pid.child`)
	c.child(t, "pid.child")
	c.awaitExit(t)
	rec := &groupRecord{hostID: markHost}
	e := entry{c.entry(deadHost(t)), rec}
	fresh := func() *reaper {
		r := &reaper{ctx: context.Background(), firstBy: time.Now().Add(testStep), readBy: time.Now().Add(testStep)}
		r.scan = r.newScan()
		return r
	}
	if fate, why, _ := fresh().fate(e); fate != groupHere {
		t.Fatalf("with time to read: %v, %s", fate, why)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cancelled := fresh()
	cancelled.ctx = ctx
	if fate, why, _ := cancelled.fate(e); fate != groupCut {
		t.Fatalf("with the context ended: %v, %s", fate, why)
	}
	spent := fresh()
	if _, err := spent.scan.of(c.pid); err != nil {
		t.Fatal(err)
	}
	spent.readBy = time.Now().Add(-time.Second)
	if fate, why, _ := spent.fate(e); fate != groupCut {
		t.Fatalf("with its reading out of time: %v, %s", fate, why)
	}
}

// TestASlowListingStillExaminesRecords: the budget for selecting records starts after
// the listing, or a big directory would make no progress however many passes came.
func TestASlowListingStillExaminesRecords(t *testing.T) {
	env := holderEnv(t)
	c := shellCommand(t, `exec sleep 120`)
	path := recordIn(t, env, rundir.NewHostID(), c.entry(deadHost(t)))
	prev := recordsListed
	t.Cleanup(func() { recordsListed = prev })
	recordsListed = func() { time.Sleep(sweepRead + 100*time.Millisecond) }
	SweepDeadHosts(env, nil)
	c.awaitGone(t, "the shell")
	if fileExists(path) {
		t.Fatal("the record was left")
	}
}

// TestALateSIGKILLStillGoes: a pass that took long over its first round still sends
// its SIGKILL. A pass that kept every record back here would send SIGTERM each time
// and SIGKILL never.
func TestALateSIGKILLStillGoes(t *testing.T) {
	env := holderEnv(t)
	c := shellCommand(t, `trap '' TERM; exec sleep 120`)
	path := recordIn(t, env, rundir.NewHostID(), c.entry(deadHost(t)))
	waitForGroupsIs(t, func(context.Context, []entry, time.Time) { time.Sleep(2*sweepRead + 100*time.Millisecond) })
	SweepDeadHosts(env, nil)
	c.awaitGone(t, "the shell ignoring SIGTERM")
	if fileExists(path) {
		t.Fatal("the record was left")
	}
}

// TestATestsCleanupKeepsToItsOwn: the cleanup goes by the command's own mark: a process
// in its group without one, as a stranger with its number would be, is left alone.
func TestATestsCleanupKeepsToItsOwn(t *testing.T) {
	c := shellCommand(t, `env -u `+strings.SplitN(CommandMark(markHost), "=", 2)[0]+` -u CRAZE_REAP_TEST sleep 120 & echo $! > pid.child; exec sleep 120`)
	stranger := c.child(t, "pid.child")
	t.Cleanup(func() { _ = syscall.Kill(stranger.pid, syscall.SIGKILL) })
	c.end(t)
	stranger.survives(t, "a process in the group without the command's mark")
	if c.alive() {
		t.Fatal("the command itself was left")
	}
}

// TestALogComesAfterTheSignal: nothing is written between a group's read and its
// signal, and no group's signal waits on another's log.
func TestALogComesAfterTheSignal(t *testing.T) {
	env := holderEnv(t)
	a, b := shellCommand(t, `exec sleep 120`), shellCommand(t, `exec sleep 120`)
	owner := deadHost(t)
	id := rundir.NewHostID()
	recordIn(t, env, id, a.entry(owner), b.entry(owner))
	var seq []string
	sent := 0
	signalsAre(t, func(int, syscall.Signal) { sent++ })
	waitForGroupsIs(t, func(context.Context, []entry, time.Time) {})
	ReapHost(context.Background(), env, id, func(format string, _ ...any) {
		seq = append(seq, fmt.Sprintf("%d signals, then %s", sent, format))
	})
	if len(seq) == 0 || !strings.HasPrefix(seq[0], "4 signals") {
		t.Fatalf("a log came before the round's signals had gone: %q", seq)
	}
}

// TestASweepReachesPastRetainedRecords: records that stay use up a pass's bound, so a
// pass starting at the top of the listing would never reach what sorts after them.
// Each pass starts somewhere else: the record after 25 retained ones is reached.
func TestASweepReachesPastRetainedRecords(t *testing.T) {
	env := holderEnv(t)
	owner := deadHost(t)
	own := AgentGroup{PGID: syscall.Getpgrp(), Start: 1, Kind: GroupShell, Owner: owner}
	for i := range SweepHostsMax + 5 {
		recordIn(t, env, fmt.Sprintf("%012x", i), own)
	}
	c := shellCommand(t, `exec sleep 120`)
	last := recordIn(t, env, "ffffffffffff", c.entry(owner))
	prev := sweepStart
	t.Cleanup(func() { sweepStart = prev })
	sweepStart = func(int) int { return 0 }
	SweepDeadHosts(env, nil)
	c.survives(t, "the record past the bound, from the top")
	sweepStart = func(n int) int {
		if n != SweepHostsMax+6 {
			t.Errorf("a pass over %d records", n)
		}
		return n - 1
	}
	SweepDeadHosts(env, nil)
	c.awaitGone(t, "the record past the retained ones")
	if fileExists(last) {
		t.Fatal("the record was left")
	}
}

// TestSweepStartsAnywhere: the real start lands on every place in a listing.
func TestSweepStartsAnywhere(t *testing.T) {
	seen := map[int]bool{}
	for range 1000 {
		seen[sweepStart(5)] = true
	}
	if len(seen) != 5 {
		t.Fatalf("starts seen: %v", seen)
	}
}

// TestRecordsAgeOutOnlyWhenNothingCanActOnThem: a record nothing here can act on stays
// until RecordKeep and then goes, nothing it names signalled. A dead host's actionable
// record is acted on however old.
func TestRecordsAgeOutOnlyWhenNothingCanActOnThem(t *testing.T) {
	env := holderEnv(t)
	c := shellCommand(t, `exec sleep 120`)
	long := time.Now().Add(-2 * RecordKeep)
	other := deadHost(t)
	other.PIDNS = "another-namespace"
	otherBoot := deadHost(t)
	otherBoot.Boot = "another-boot"
	mixedA, mixedB := deadHost(t), deadHost(t)
	mixed := func() string {
		return c.entry(mixedA).Line() + c.entry(mixedB).Line()
	}()
	aged := func(body string, at time.Time) string {
		path := recordIn(t, env, rundir.NewHostID())
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
		return path
	}
	now := time.Now()
	young := []string{aged(c.entry(other).Line(), now), aged(c.entry(otherBoot).Line(), now), aged(mixed, now)}
	oversize := strings.Repeat(c.entry(other).Line(), recordBytes/len(c.entry(other).Line())+1)
	old := []string{aged(c.entry(other).Line(), long), aged(c.entry(otherBoot).Line(), long), aged(mixed, long), aged(oversize, long)}
	SweepDeadHosts(env, nil)
	c.survives(t, "the shell those records name")
	for _, p := range young {
		if !fileExists(p) {
			t.Fatalf("a young record was removed: %q", p)
		}
	}
	for _, p := range old {
		if fileExists(p) {
			t.Fatalf("an aged record was left: %q", p)
		}
	}
	// Eight days old and still running: rundir.SweepHostLogs leaves it, and this ends it.
	actionable := aged(c.entry(deadHost(t)).Line(), long)
	SweepDeadHosts(env, nil)
	c.awaitGone(t, "the aged record's shell")
	if fileExists(actionable) {
		t.Fatal("the aged actionable record was left")
	}
}

// TestASpawnerGivesAShellCommandAGraceAndDoesNotWait: a spawner's last resort takes the
// record's lock, kills an agent at once, sends a shell command SIGTERM, and returns
// with the grace still to run. Then SIGKILL, and the record goes. A record another
// sweep holds is that sweep's.
func TestASpawnerGivesAShellCommandAGraceAndDoesNotWait(t *testing.T) {
	dir := t.TempDir()
	c := &Child{groups: filepath.Join(dir, "0123456789ab.pgids"), log: filepath.Join(dir, "0123456789ab.log")}
	shell := shellCommand(t, `trap 'echo term > got; exit 0' TERM; echo $$ > pid.ready; while :; do sleep 0.05; done`)
	shell.child(t, "pid.ready")
	stubborn := shellCommand(t, `trap '' TERM; exec sleep 120`)
	agent := leadGroup(t)
	owner := deadHost(t)
	c.pid, c.start = owner.PID, owner.Start
	body := shell.entry(owner).Line() + stubborn.entry(owner).Line() + AgentGroup{PGID: agent.pid, Start: agent.start, Kind: GroupAgent, Owner: owner}.Line()
	if err := os.WriteFile(c.groups, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	held, _ := openRecord(c.groups, noNote)
	if held == nil {
		t.Fatal("the record could not be taken")
	}
	c.KillAgents()
	stubborn.survives(t, "a group of a record another sweep holds")
	select {
	case <-agent.exited:
		t.Fatalf("the spawner killed an agent of a record another sweep holds: %v", agent.err)
	default:
	}
	if !fileExists(c.groups) {
		t.Fatal("the spawner removed a record another sweep holds")
	}
	held.release()

	inGrace, end := make(chan struct{}), make(chan struct{})
	waitForGroupsIs(t, func(context.Context, []entry, time.Time) {
		close(inGrace)
		<-end
	})
	c.KillAgents() // returns with the grace blocked: the watch does not wait on it
	agent.killed(t)
	select {
	case <-inGrace:
	case <-time.After(testStep):
		t.Fatal("the grace never began")
	}
	if !fileExists(c.groups) {
		t.Fatal("the record went before the grace")
	}
	deadline := time.Now().Add(testStep)
	for !fileExists(filepath.Join(shell.dir, "got")) {
		if time.Now().After(deadline) {
			t.Fatal("the shell command was never sent SIGTERM")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !stubborn.alive() {
		t.Fatal("what ignores SIGTERM was killed inside the grace")
	}
	close(end)
	stubborn.awaitGone(t, "what ignores SIGTERM")
	for fileExists(c.groups) {
		if time.Now().After(deadline) {
			t.Fatal("the record was left")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestSweepBound: a sweep acts on at most SweepHostsMax dead hosts, and the next
// finds the rest; a running host does not use up the bound.
func TestSweepBound(t *testing.T) {
	env := holderEnv(t)
	owner, live := deadHost(t), liveHost(t)
	c := shellCommand(t, `:`)
	c.awaitExit(t)
	dead := c.entry(owner)
	for i := range SweepHostsMax + 5 {
		// A running host's record comes first in the listing, more than the bound's worth.
		name := fmt.Sprintf("%012x", i)
		recordIn(t, env, name, c.entry(live))
		name = fmt.Sprintf("a%011x", i)
		recordIn(t, env, name, dead)
	}
	prev := sweepStart
	t.Cleanup(func() { sweepStart = prev })
	sweepStart = func(int) int { return 0 }
	count := func() (n int) {
		dir, err := rundir.HostLogDir(env)
		if err != nil {
			t.Fatal(err)
		}
		names, _ := filepath.Glob(filepath.Join(dir, "a*.pgids"))
		return len(names)
	}
	SweepDeadHosts(env, nil)
	if n := count(); n != 5 {
		t.Fatalf("%d dead hosts' records left after a sweep, want 5", n)
	}
	SweepDeadHosts(env, nil)
	if n := count(); n != 0 {
		t.Fatalf("%d dead hosts' records left after a second sweep, want 0", n)
	}
}

// TestASweepNeverSignalsALineWithNoOwner: a two-field line has no boot or namespace to
// check its numbers against, however its leader reads. A sweep or a waiter signals
// nothing it names: the record goes once it is old and its host's lock is free. Only
// a spawner whose own child just died may act on one.
func TestASweepNeverSignalsALineWithNoOwner(t *testing.T) {
	env := holderEnv(t)
	young, old, held, direct := leadGroup(t), leadGroup(t), leadGroup(t), leadGroup(t)
	youngPath := recordIn(t, env, rundir.NewHostID(), AgentGroup{PGID: young.pid, Start: young.start})
	oldID := rundir.NewHostID()
	oldPath := recordIn(t, env, oldID, AgentGroup{PGID: old.pid, Start: old.start})
	long := time.Now().Add(-2 * bareRecordAge)
	if err := os.Chtimes(oldPath, long, long); err != nil {
		t.Fatal(err)
	}
	liveID := rundir.NewHostID()
	bindEntry(t, env, liveID, "sid", "cursor", "psid")
	heldPath := recordIn(t, env, liveID, AgentGroup{PGID: held.pid, Start: held.start})
	if err := os.Chtimes(heldPath, long, long); err != nil {
		t.Fatal(err)
	}
	SweepDeadHosts(env, nil)
	ReapHost(context.Background(), env, liveID, noNote)
	if fileExists(oldPath) {
		t.Fatal("the old record was left")
	}
	if !fileExists(youngPath) || !fileExists(heldPath) {
		t.Fatal("a record was removed that should not be")
	}
	for name, g := range map[string]*groupLeader{"old": old, "young": young, "held": held} {
		if err := syscall.Kill(g.pid, 0); err != nil {
			t.Fatalf("the %s record's group was signalled: %v", name, err)
		}
	}
	c := &Child{pid: 1 << 22, groups: recordIn(t, env, rundir.NewHostID(), AgentGroup{PGID: direct.pid, Start: direct.start})}
	c.KillAgents()
	direct.killed(t)
}

// TestSweepOfNothingMakesNothing: no cache tree, nothing to do, and it is left
// unmade.
func TestSweepOfNothingMakesNothing(t *testing.T) {
	env := holderEnv(t)
	SweepDeadHosts(env, nil)
	if _, err := os.Lstat(filepath.Join(env.Home, ".cache")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the sweep made the cache tree: %v", err)
	}
}

// terminated waits, within testStep, for g to have gone by a signal.
func (g *groupLeader) terminated(t *testing.T) {
	t.Helper()
	select {
	case <-g.exited:
	case <-time.After(testStep):
		t.Fatalf("process %d was not signalled within %v", g.pid, testStep)
	}
	var ee *exec.ExitError
	if !errors.As(g.err, &ee) {
		t.Fatalf("process %d exited %v, want a signal", g.pid, g.err)
	}
	if ws, ok := ee.Sys().(syscall.WaitStatus); !ok || !ws.Signaled() {
		t.Fatalf("process %d exited %v, want a signal", g.pid, g.err)
	}
}

// BenchmarkSweepOfHealthyHosts is a healthy command's whole cost, by how many
// hosts are running: what `craze ps` pays on its way in.
func BenchmarkSweepOfHealthyHosts(b *testing.B) {
	for _, hosts := range []int{0, 1, 10, 100} {
		b.Run(fmt.Sprintf("hosts=%d", hosts), func(b *testing.B) {
			home := b.TempDir()
			runtimeDir, err := os.MkdirTemp("", "cz")
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() { _ = os.RemoveAll(runtimeDir) })
			env := rundir.Env{Home: home, CrazeDir: filepath.Join(home, ".craze"), CrazeRuntimeDir: runtimeDir, EUID: os.Geteuid()}
			dir, err := rundir.HostLogDir(env)
			if err != nil {
				b.Fatal(err)
			}
			owner, err := NewGroupOwner()
			if err != nil {
				b.Fatal(err)
			}
			for i := range hosts {
				id := rundir.NewHostID()
				line := AgentGroup{PGID: i + 100000, Start: 1, Kind: GroupShell, Owner: owner}.Line()
				if err := os.WriteFile(filepath.Join(dir, AgentGroupsName(id)), []byte(line), 0o600); err != nil {
					b.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, id+".log"), []byte("log\n"), 0o600); err != nil {
					b.Fatal(err)
				}
			}
			b.ResetTimer()
			for b.Loop() {
				SweepDeadHosts(env, nil)
			}
		})
	}
}

// BenchmarkScanOfTheMachine is what one scan costs here, for a reader who wants to
// know what a dead host's record costs a box with many processes.
func BenchmarkScanOfTheMachine(b *testing.B) {
	for b.Loop() {
		groups, err := rundir.ProcessGroups(time.Now().Add(testStep))
		if err != nil {
			b.Fatal(err)
		}
		n := 0
		for _, m := range groups {
			n += len(m)
		}
		b.ReportMetric(float64(n), "procs")
	}
}
