package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/errkind"
	"github.com/charliek/craze/internal/hostspawn"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/rundir"
)

// TestASignalDuringTheJoinKeepsTheExitCode: a command has written its result and is
// waiting for its sweep, whose grace has a good way to run. Any signal a waiting
// command catches ends the wait at once, and the process exits with the code the
// command determined, not by the signal. Run in a process of its own, as Execute's exit
// would be, and signalled only once its handler is registered.
func TestASignalDuringTheJoinKeepsTheExitCode(t *testing.T) {
	if os.Getenv("CRAZE_JOIN_SIGNAL") != "" {
		done := make(chan struct{}) // a sweep that never ends
		hostSweepDone.Store(&done)
		hostSweepJoining = func() { os.Stdout.WriteString("joining\n") }
		if joinHostSweep() {
			os.Exit(3)
		}
		os.Exit(9) // the bound, not the signal
	}
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP} {
		t.Run(sig.String(), func(t *testing.T) {
			cmd := exec.Command(bin, "-test.run=^TestASignalDuringTheJoinKeepsTheExitCode$")
			cmd.Env = append(os.Environ(), "CRAZE_JOIN_SIGNAL=1")
			cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
			out, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = cmd.Process.Kill() })
			line, err := bufio.NewReader(out).ReadString('\n')
			if err != nil || strings.TrimSpace(line) != "joining" {
				t.Fatalf("the join's first line: %q, %v", line, err)
			}
			if err := cmd.Process.Signal(sig); err != nil {
				t.Fatal(err)
			}
			_ = cmd.Wait()
			if code := cmd.ProcessState.ExitCode(); code != 3 {
				t.Fatalf("exit %d (%v), want the command's 3", code, cmd.ProcessState)
			}
		})
	}
}

// TestAJoinOfAFinishedSweepRegistersNothing: nothing to wait for is nothing to catch, so
// a healthy command's exit is as it was.
func TestAJoinOfAFinishedSweepRegistersNothing(t *testing.T) {
	done := make(chan struct{})
	close(done)
	hostSweepDone.Store(&done)
	if joinHostSweep() {
		t.Fatal("a signal was reported with none sent")
	}
	if joinHostSweep() {
		t.Fatal("a second join found something")
	}
}

// ---------------------------------------------------------- reaping a host gone

// recordedCommand is a real command as a native host runs one: a shell in a session
// and a group of its own, carrying its host's mark, written down in the host's record.
// Its cleanup goes by a mark of its own, so nothing but what it started is ever
// signalled, whoever has the group's number by then.
type recordedCommand struct {
	pid    int
	dir    string
	record string
	reaped chan struct{}
}

var recordedMarks atomic.Int64

// recordCommand starts script for the host hostID, its record naming owner.
func recordCommand(t *testing.T, env rundir.Env, hostID string, owner hostspawn.GroupOwner, script string) *recordedCommand {
	t.Helper()
	dir, err := rundir.HostLogDir(env)
	if err != nil {
		t.Fatal(err)
	}
	c := &recordedCommand{dir: t.TempDir(), record: filepath.Join(dir, hostspawn.AgentGroupsName(hostID)), reaped: make(chan struct{})}
	mark := fmt.Sprintf("CRAZE_CLI_REAP_TEST=%d-%d", os.Getpid(), recordedMarks.Add(1))
	cmd := exec.Command("sh", "-c", script)
	cmd.Dir = c.dir
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, hostspawn.CommandMarkName+"=") {
			cmd.Env = append(cmd.Env, e)
		}
	}
	cmd.Env = append(cmd.Env, mark, hostspawn.CommandMark(hostID))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	c.pid = cmd.Process.Pid
	go func() {
		_ = cmd.Wait()
		close(c.reaped)
	}()
	t.Cleanup(func() {
		groups, err := rundir.ProcessGroups(time.Now().Add(stubWatchdog))
		if err != nil {
			t.Errorf("the cleanup could not scan: %v", err)
		}
		for _, m := range groups[c.pid] {
			if rundir.ProcessCarries(m.PID, mark) {
				_ = syscall.Kill(m.PID, syscall.SIGKILL)
			}
		}
		select {
		case <-c.reaped:
		case <-time.After(stubWatchdog):
			t.Errorf("the command's shell (%d) was not reaped within %v", c.pid, stubWatchdog)
		}
	})
	id, err := rundir.ProcessIdentity(c.pid)
	if err != nil {
		t.Fatal(err)
	}
	line := hostspawn.AgentGroup{PGID: c.pid, Start: id.Start, Kind: hostspawn.GroupShell, Owner: owner}.Line()
	if err := os.WriteFile(c.record, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	return c
}

// awaitReaped is the command signalled and its record gone, within the watchdog.
func (c *recordedCommand) awaitReaped(t *testing.T) {
	t.Helper()
	select {
	case <-c.reaped:
	case <-time.After(stubWatchdog):
		t.Fatalf("the command (%d) was still running with its host gone", c.pid)
	}
	if fileExists(c.record) {
		t.Fatal("the record was left")
	}
}

// survives is the command running on after everything that could have signalled it
// has returned, and its record still there.
func (c *recordedCommand) survives(t *testing.T) {
	t.Helper()
	select {
	case <-c.reaped:
		t.Fatalf("the command (%d) was signalled", c.pid)
	case <-time.After(100 * time.Millisecond):
	}
	if !fileExists(c.record) {
		t.Fatal("the record was removed")
	}
}

// groupOwner is pid and start as the owner of a record, in this machine's scope.
func groupOwner(t *testing.T, pid int, start uint64) hostspawn.GroupOwner {
	t.Helper()
	o, err := hostspawn.NewGroupOwner()
	if err != nil {
		t.Skipf("no process identity here: %v", err)
	}
	o.PID, o.Start = pid, start
	return o
}

// killAndAwait is the host's process SIGKILLed, and gone by the time this returns.
func killAndAwait(t *testing.T, pid int, gone func() bool) {
	t.Helper()
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Error(err)
	}
	waitFor(t, "the host to go", gone)
}

// earlierOwner is an owner that has gone: what an earlier host at this id would name.
func earlierOwner(t *testing.T) hostspawn.GroupOwner {
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
	return groupOwner(t, cmd.Process.Pid, id.Start)
}

// markStoppingAs is the rig's registry entry marked stopping, naming pid and start: the
// entry of a host that is that process.
func markStoppingAs(t *testing.T, r *waitRig, pid int, start uint64) {
	t.Helper()
	if err := r.reg.MarkStopping(); err != nil {
		t.Fatal(err)
	}
	path := entryPath(r.env, r.entry().HostID)
	e, ok := readEntryFile(t, path)
	if !ok {
		t.Fatal("the rig's entry")
	}
	e.PID, e.PIDStart = pid, start
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestAWaiterReapsAHostItKnowsHasGone: however the wait reads its end, a host gone by
// its identity has its commands ended before the waiter returns, and a host still
// there does not, however it reads that. Each with a real process for the host and a
// real command. What the waiter says is the same in both.
//
// A host still there has a record naming an earlier, dead owner here, which a sweep
// would act on: only the waiter's own look at its host keeps it off. A record of the
// host's own would keep it off regardless (TestReapKeepsOffAHealthyHost).
func TestAWaiterReapsAHostItKnowsHasGone(t *testing.T) {
	stoppedBy := func(cause string) func(*testing.T, *turnResult, *waitFailure) {
		return func(t *testing.T, res *turnResult, f *waitFailure) {
			t.Helper()
			wantStopped(t, res, f, cause)
		}
	}
	failedAs := func(kind errkind.Kind) func(*testing.T, *turnResult, *waitFailure) {
		return func(t *testing.T, res *turnResult, f *waitFailure) {
			t.Helper()
			if res != nil || f == nil || f.kind != kind {
				t.Fatalf("the wait: %+v, %+v, want kind %s", res, f, kind)
			}
		}
	}
	lostStream := func(t *testing.T, r *waitRig) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), serveStep)
		defer cancel()
		_ = r.srv.Close(ctx)
	}
	writeRecord := func(turn string) func(*testing.T, *waitRig, rundir.Entry) {
		return func(t *testing.T, r *waitRig, e rundir.Entry) {
			t.Helper()
			rec := rundir.StopRecord{Cause: agent.StopCauseStop, Turn: turn, At: time.Now(), PID: e.PID, PIDStart: e.PIDStart}
			if err := rundir.WriteStopRecord(r.env, e.HostID, rec); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, tc := range []struct {
		name         string
		before       func(*testing.T, *waitRig, rundir.Entry)
		lose         func(*testing.T, *waitRig)
		gone, living func(*testing.T, *turnResult, *waitFailure)
	}{
		{
			name: "its stop record names the turn", before: writeRecord("turn-1"), lose: lostStream,
			gone: stoppedBy(agent.StopCauseStop), living: stoppedBy(agent.StopCauseStop),
		},
		{
			name: "its stop record names another turn", before: writeRecord("turn-9"), lose: lostStream,
			gone: failedAs(errkind.Stopped), living: failedAs(errkind.Stopped),
		},
		{
			name: "its registry entry marked stopping",
			before: func(t *testing.T, r *waitRig, e rundir.Entry) {
				t.Helper()
				markStoppingAs(t, r, e.PID, e.PIDStart)
			},
			lose: lostStream, gone: stoppedBy(""), living: stoppedBy(""),
		},
		{
			name: "the close's ending on the stream",
			lose: func(t *testing.T, r *waitRig) {
				t.Helper()
				if err := r.eng.CloseFor(agent.StopCauseStop); err != nil {
					t.Fatal(err)
				}
			},
			gone: stoppedBy(agent.StopCauseStop), living: stoppedBy(agent.StopCauseStop),
		},
		{
			name: "no word of a stop", lose: lostStream,
			gone: failedAs(errkind.HostExited), living: failedAs(errkind.ConnectionLost),
		},
	} {
		for _, hostGoes := range []bool{true, false} {
			name := tc.name + ", the host still there"
			if hostGoes {
				name = tc.name + ", the host gone"
			}
			t.Run(name, func(t *testing.T) {
				r := newWaitRig(t, 0)
				turn, emitted := heldAfter(r, usageEv(7, false))
				r.script(turn)
				conn, tp := r.dialTapped()
				pid, start, gone := liveProcess(t)
				conn.entry.PID, conn.entry.PIDStart = pid, start
				owner := earlierOwner(t)
				if hostGoes {
					owner = groupOwner(t, pid, start)
				}
				c := recordCommand(t, r.env, conn.entry.HostID, owner, `exec sleep 120`)
				w := goWait(conn, waitStart{submit: submitPrompt(conn, "mine")}, waitOptions{})
				waitFor(t, "the waiter's look", func() bool { return len(tp.lines(protocol.MethodSessionSnapshot)) >= 1 })
				<-emitted
				if tc.before != nil {
					tc.before(t, r, conn.entry)
				}
				if hostGoes {
					killAndAwait(t, pid, gone)
				}
				tc.lose(t, r)
				res, f := w.result(t)
				if hostGoes {
					tc.gone(t, res, f)
					c.awaitReaped(t)
					return
				}
				tc.living(t, res, f)
				c.survives(t)
			})
		}
	}
}

// TestASignalCutsAWaitersReapShort: the host gone, the waiter at its command's grace, a
// signal comes. The wait says what it was going to, at once, without the SIGKILL, and
// the record stays for the next craze.
func TestASignalCutsAWaitersReapShort(t *testing.T) {
	r := newWaitRig(t, 0)
	turn, emitted := heldAfter(r, usageEv(7, false))
	r.script(turn)
	conn, tp := r.dialTapped()
	pid, start, gone := liveProcess(t)
	conn.entry.PID, conn.entry.PIDStart = pid, start
	c := recordCommand(t, r.env, conn.entry.HostID, groupOwner(t, pid, start),
		`trap 'echo term > term' TERM; while :; do sleep 0.05; done`)
	sigs := make(chan os.Signal, 2)
	w := goWait(conn, waitStart{submit: submitPrompt(conn, "mine")}, waitOptions{sigs: sigs})
	waitFor(t, "the waiter's look", func() bool { return len(tp.lines(protocol.MethodSessionSnapshot)) >= 1 })
	<-emitted
	killAndAwait(t, pid, gone)
	ctx, cancel := context.WithTimeout(context.Background(), serveStep)
	defer cancel()
	_ = r.srv.Close(ctx)
	waitFor(t, "the command's SIGTERM", func() bool { return fileExists(filepath.Join(c.dir, "term")) })
	sigs <- syscall.SIGTERM
	begun := time.Now()
	_, f := w.result(t)
	if took := time.Since(begun); took >= hostspawn.ReapGrace {
		t.Fatalf("the signal took %v to end a grace of %v", took, hostspawn.ReapGrace)
	}
	if f == nil || f.kind != errkind.HostExited {
		t.Fatalf("the failure: %+v", f)
	}
	c.survives(t)
}

// TestASignalsCleanupReapsAHostGone: a signal has ended the wait's work, and its cleanup
// reads the turn's ending on with the host gone by then: the cleanup's own context ends
// what the host left, and the result stands.
func TestASignalsCleanupReapsAHostGone(t *testing.T) {
	r := signalRig(t)
	conn, tp := r.dialTapped()
	pid, start, gone := liveProcess(t)
	conn.entry.PID, conn.entry.PIDStart = pid, start
	c := recordCommand(t, r.env, conn.entry.HostID, groupOwner(t, pid, start), `exec sleep 120`)
	sigs := make(chan os.Signal, 2)
	w := goWait(conn, waitStart{submit: submitPrompt(conn, "go")}, waitOptions{sigs: sigs})
	r.waitStarted("turn-1")
	waitFor(t, "the waiter's target", func() bool { return len(tp.lines(protocol.MethodSessionPrompt)) == 1 })
	killAndAwait(t, pid, gone)
	sigs <- syscall.SIGINT
	res, f := w.result(t)
	wantResult(t, res, f, statusCancelled)
	c.awaitReaped(t)
}

// TestASecondSignalDuringTheReapLeavesASettledFailure: a signal's cleanup learns the
// host stopped, in a turn other than its own: stopped, with the cause. The host has gone
// and left a command that ignores SIGTERM, so the reap waits out its grace, and a second
// signal comes then. It ends the reap and abandons the detach, and says nothing of what
// was already settled.
func TestASecondSignalDuringTheReapLeavesASettledFailure(t *testing.T) {
	shorten(t, &cleanupWait, time.Hour)
	r := newWaitRig(t, 0)
	turn, emitted := heldAfter(r, usageEv(7, false))
	r.script(turn)
	conn, tp := r.dialTapped()
	pid, start, gone := liveProcess(t)
	conn.entry.PID, conn.entry.PIDStart = pid, start
	c := recordCommand(t, r.env, conn.entry.HostID, groupOwner(t, pid, start),
		`trap 'echo term > term' TERM; while :; do sleep 0.05; done`)
	sigs := make(chan os.Signal, 2)
	w := goWait(conn, waitStart{submit: submitPrompt(conn, "mine")}, waitOptions{sigs: sigs})
	waitFor(t, "the waiter's look", func() bool { return len(tp.lines(protocol.MethodSessionSnapshot)) >= 1 })
	<-emitted
	rec := rundir.StopRecord{Cause: agent.StopCauseSignal, Turn: "turn-9", At: time.Now(), PID: pid, PIDStart: start}
	if err := rundir.WriteStopRecord(r.env, conn.entry.HostID, rec); err != nil {
		t.Fatal(err)
	}
	killAndAwait(t, pid, gone)
	sigs <- syscall.SIGTERM
	waitFor(t, "the cleanup's cancel", func() bool { return len(tp.lines(protocol.MethodSessionCancel)) == 1 })
	ctx, cancel := context.WithTimeout(context.Background(), serveStep)
	defer cancel()
	_ = r.srv.Close(ctx)
	waitFor(t, "the command's SIGTERM", func() bool { return fileExists(filepath.Join(c.dir, "term")) })
	sigs <- syscall.SIGTERM
	res, f := w.result(t)
	if res != nil || f == nil || f.kind != errkind.Stopped || f.cause != agent.StopCauseSignal || f.follow || f.handle == "" || f.usage == nil {
		t.Fatalf("the wait: %+v, %+v", res, f)
	}
	if !conn.abandon {
		t.Fatal("the second signal did not abandon the detach")
	}
	c.survives(t)
}

// TestAnotherHostsStopIsNoEvidence: A crashes under an id, and before its waiter has
// read the loss, B starts under that id and begins to stop: B's record names the very
// turn A's waiter is following, and B's entry is marked. Neither is A's word. A's waiter
// reads host_exited, with no cause, and B's commands are not touched.
func TestAnotherHostsStopIsNoEvidence(t *testing.T) {
	r := newWaitRig(t, 0)
	turn, emitted := heldAfter(r, usageEv(7, false))
	r.script(turn)
	conn, tp := r.dialTapped()
	pid, start, gone := liveProcess(t)
	conn.entry.PID, conn.entry.PIDStart = pid, start
	self, err := rundir.ProcessIdentity(os.Getpid())
	if err != nil {
		t.Skipf("no process identity here: %v", err)
	}
	c := recordCommand(t, r.env, conn.entry.HostID, groupOwner(t, os.Getpid(), self.Start), `exec sleep 120`)
	w := goWait(conn, waitStart{submit: submitPrompt(conn, "mine")}, waitOptions{})
	waitFor(t, "the waiter's look", func() bool { return len(tp.lines(protocol.MethodSessionSnapshot)) >= 1 })
	<-emitted
	killAndAwait(t, pid, gone)
	rec := rundir.StopRecord{Cause: agent.StopCauseIdle, Turn: "turn-1", At: time.Now(), PID: os.Getpid(), PIDStart: self.Start}
	if err := rundir.WriteStopRecord(r.env, conn.entry.HostID, rec); err != nil {
		t.Fatal(err)
	}
	if err := r.reg.MarkStopping(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), serveStep)
	defer cancel()
	_ = r.srv.Close(ctx)
	res, f := w.result(t)
	if res != nil || f == nil || f.kind != errkind.HostExited || f.cause != "" {
		t.Fatalf("the wait: %+v, %+v", res, f)
	}
	c.survives(t)
}

// TestAStoppingMarkIsItsOwnHosts: the mark counts only with the same pid and start on
// the entry the waiter holds and the one now at the id. Either with no identity is none.
func TestAStoppingMarkIsItsOwnHosts(t *testing.T) {
	r := newWaitRig(t, 0)
	e := r.entry()
	if hostMarkedStopping(e) {
		t.Fatal("marked, with no mark")
	}
	if err := r.reg.MarkStopping(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		pid   int
		start uint64
		want  bool
	}{
		{"its own", e.PID, e.PIDStart, true},
		{"another pid", e.PID + 1, e.PIDStart, false},
		{"another start", e.PID, e.PIDStart + 1, false},
		{"no start", e.PID, 0, false},
		{"no pid", 0, e.PIDStart, false},
	} {
		held := e
		held.PID, held.PIDStart = tc.pid, tc.start
		if got := hostMarkedStopping(held); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
	markStoppingAs(t, r, e.PID, 0)
	held := e
	held.PIDStart = 0
	if hostMarkedStopping(held) {
		t.Error("an entry with no identity, and a waiter with none, matched")
	}
	if hostMarkedStopping(e) {
		t.Error("an entry with no identity was this host's")
	}
}

// TestAnEarlierHostsWaiterKeepsItsEvidence: A stopped and left its record; its waiter
// has not yet read the loss when B binds the same id. The record is still there and
// still A's: A's waiter reads the stop, and a waiter on B reads none.
func TestAnEarlierHostsWaiterKeepsItsEvidence(t *testing.T) {
	env, _ := serveHome(t)
	self, err := rundir.ProcessIdentity(os.Getpid())
	if err != nil {
		t.Skipf("no process identity here: %v", err)
	}
	id, a := rundir.NewHostID(), exitedPID(t)
	rec := rundir.StopRecord{Cause: agent.StopCauseStop, Turn: "turn-1", At: time.Now(), PID: a, PIDStart: self.Start}
	if err := rundir.WriteStopRecord(env, id, rec); err != nil {
		t.Fatal(err)
	}
	b, err := rundir.Bind(env, id, rundir.Entry{StartedAt: time.Now(), Workspace: "/w", PIDStart: self.Start})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Close() }()
	onA := waiterAt(nil)
	onA.conn.entry = rundir.Entry{HostID: id, PID: a, PIDStart: self.Start}
	onA.lost(errors.New("broken pipe"))
	wantStopped(t, onA.result, onA.fail, agent.StopCauseStop)
	onB := waiterAt(nil)
	onB.conn.entry = rundir.Entry{HostID: id, PID: os.Getpid(), PIDStart: self.Start}
	onB.lost(errors.New("broken pipe"))
	if f := onB.fail; onB.result != nil || f == nil || f.kind != errkind.ConnectionLost || f.cause != "" {
		t.Fatalf("the waiter on the later host: %+v, %+v", onB.result, f)
	}
}
