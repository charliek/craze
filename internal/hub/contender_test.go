package hub

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/rundir"
)

// A spawned hub that answers its ready line late and wrong, and ignores
// SIGTERM (r28 3): Ensure ends it but waits for that end only until its
// context ends, and the session list's roster is on its poller at its own
// bound whatever Ensure does. The contender is this test binary re-executed
// (contenderChild); each test here is also run under a 5% CPU quota.

// contenderChild runs this test binary as a contender: it ignores SIGTERM,
// waits the duration its value names, writes a ready line that is not one
// ({"ok":false}, no reason) and then does nothing until it is killed — or its
// watchdog ends it once the test process is gone.
const contenderChild = "CRAZE_HUB_TEST_CONTENDER"

func init() {
	v, ok := os.LookupEnv(contenderChild)
	if !ok {
		return
	}
	_ = os.Unsetenv(contenderChild)
	parent := os.Getppid()
	if n, err := strconv.Atoi(os.Getenv(hubTestParent)); err == nil && n > 1 {
		parent = n
	}
	go childWatchdog(parent)
	signal.Ignore(syscall.SIGTERM)
	delay, err := time.ParseDuration(v)
	if err != nil {
		fmt.Fprintln(os.Stderr, "contender child:", err)
		os.Exit(97)
	}
	ready, err := TakeReadyPipe()
	if err != nil || ready == nil {
		fmt.Fprintln(os.Stderr, "contender child: no ready pipe:", err)
		os.Exit(97)
	}
	time.Sleep(delay)
	_ = ready.Send(ReadyLine{})
	select {}
}

// asContenders installs Command for one test: every hub Ensure spawns is a
// contender (contenderChild) that writes its line delay after it starts. When
// the test ends each one still running is killed, and its exit waited for.
func asContenders(t *testing.T, delay time.Duration) *children {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	c := &children{}
	prev := Command
	Command = func(argv []string) (*exec.Cmd, error) {
		cmd := exec.Command(exe, "-test.run=^$")
		cmd.Env = append(os.Environ(), contenderChild+"="+delay.String(), hubTestParent+"="+strconv.Itoa(os.Getpid()))
		c.mu.Lock()
		c.cmds = append(c.cmds, cmd)
		c.mu.Unlock()
		return cmd, nil
	}
	t.Cleanup(func() {
		Command = prev
		for _, pid := range c.pids() {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			waitGone(t, pid)
		}
	})
	return c
}

// TestEnsureLeavesAContenderItCannotWaitFor (r28 3): a spawned hub whose
// ready line is not one is ended — but one that ignores SIGTERM takes the
// whole grace to, and Ensure waits for that end only until its context ends:
// it returns at the context's end, saying the hub is being ended, with the
// contender still running. The negative control: an Ensure that ended it
// before returning would hold its caller the grace — here step — past the
// context's end.
func TestEnsureLeavesAContenderItCannotWaitFor(t *testing.T) {
	env := processEnv(t)
	generous(t)
	setVar(t, &contenderGrace, step)
	asContenders(t, 0)
	ending := make(chan int, 4)
	setVar(t, &endingContender, func(pid int) { ending <- pid })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := make(chan error, 1)
	go func() {
		_, err := Ensure(ctx, env, protocol.ConnectionCapabilities{})
		got <- err
	}()
	var pid int
	select {
	case pid = <-ending:
	case <-time.After(step):
		t.Fatal("Ensure never came to end a contender")
	}
	cancel()
	var err error
	select {
	case err = <-got:
	case <-time.After(step / 3):
		t.Fatalf("Ensure did not return within %v of its context's end: it waited for the contender's end", step/3)
	}
	if !alive(t, pid) {
		t.Fatalf("the contender (pid %d) is gone already: nothing here tells an Ensure that waited for it", pid)
	}
	if err == nil || !strings.Contains(err.Error(), "answered no ready line") || !strings.Contains(err.Error(), "is being ended") {
		t.Fatalf("Ensure = %v; want the contender's bad line, being ended", err)
	}
}

// TestTheListIsOnItsPollerAtItsBound (r28 3): a hub that answers its ready
// line wrong at 1.9 s and ignores SIGTERM — with production's bounds — leaves
// the session list's roster on its poller by its two seconds, and the
// contender is still ended after (SIGKILL, past its grace). The negative
// control: an Ensure that waited out the contender's grace, with a list that
// waited for Ensure, would hold the list until about 3.9 s.
func TestTheListIsOnItsPollerAtItsBound(t *testing.T) {
	env := processEnv(t)
	kids := asContenders(t, 1900*time.Millisecond)
	ending := make(chan int, 4)
	setVar(t, &endingContender, func(pid int) { ending <- pid })
	start := time.Now()
	rg := newListRig(t, env, newListIndex(t), nil)
	rg.mode(ModePoller)
	took := time.Since(start)
	select {
	case pid := <-ending:
		t.Logf("the list was on its poller %v after it opened; Ensure was ending contender %d", took, pid)
	default:
		t.Logf("the list was on its poller %v after it opened; the contender's line came after the bound", took)
	}
	if took > listSeedWait+time.Second {
		t.Fatalf("the list was on its poller %v after it opened, past its %v bound", took, listSeedWait)
	}
	for _, pid := range kids.pids() {
		waitGone(t, pid)
	}
}

// TestTheListDoesNotWaitForALateReach (r28 3): a reach of the hub that does
// not return at its bound — an Ensure that does not heed its context — leaves
// the list on its poller at the bound all the same; the reach's outcome is
// taken when it comes, and Close waits for it. The negative control: a list
// that waited for the reach would stay seeding until it returned, which here
// is only once the test lets it.
func TestTheListDoesNotWaitForALateReach(t *testing.T) {
	hold, release := onceCloser(t)
	returned := make(chan struct{})
	rg := newListRig(t, testEnv(t), newListIndex(t), func(o *listOptions) {
		o.seedWait = 100 * time.Millisecond
		o.ensure = func(context.Context, rundir.Env, protocol.ConnectionCapabilities) (string, error) {
			<-hold
			defer close(returned)
			return "", fmt.Errorf("a late reach")
		}
	})
	// Released before the rig's Close runs (cleanups run last first), so a
	// failing test ends rather than waiting on the reach for good.
	t.Cleanup(release)
	rg.mode(ModePoller)
	select {
	case <-returned:
		t.Fatal("the reach returned before the test let it")
	default:
	}
	closed := make(chan struct{})
	go func() {
		rg.r.Close()
		close(closed)
	}()
	select {
	case <-closed:
		t.Fatal("Close returned with the reach still running")
	case <-time.After(closeGrace):
	}
	release()
	select {
	case <-closed:
	case <-time.After(step):
		t.Fatal("Close did not return once the reach had")
	}
}
