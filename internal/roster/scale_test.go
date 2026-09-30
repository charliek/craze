package roster_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/charliek/craze/internal/roster"
	"github.com/charliek/craze/internal/rundir"
)

// The scale budget (plan 030 §3.9): with fifty hosts listed, an open list
// costs less than 5 % of one core and fewer than 64 goroutines.
//
// The roster runs in a child process — this test binary again, running
// TestScaleChild — and the fifty fake hosts in this one, as they would in
// production (the list is a TUI process, each host its own): so the child's
// own resource use is the roster's alone — the hosts' answers, the server
// side of every connection, are not in it — and it is read from the kernel
// with getrusage(RUSAGE_SELF), user plus system time, over a window of
// wall time once every host has answered. That makes the bound robust: it is
// CPU the roster used, not wall time it waited, so a loaded machine does not
// inflate it — a starved one only runs fewer ticks in a second, each costing
// the same. The goroutines are the child's runtime.NumGoroutine above what it
// had before Open, sampled through the window. The CPU bound is not asserted
// under the race detector, whose instrumentation costs several times the
// program's own (it is reported); everything else is.
//
// The window is the steady state, and it is shown to be (sol r17-c9 6): it
// lasts at least scaleWindow and until every host has answered scaleRounds
// more times within it — full poll rounds over the fifty hosts, counted from
// the roster's own results (TestOptions.Applied) — with every host reachable
// in the list's latest Snapshot as it ends; the child reports both, and the
// test holds them. A poller that stopped asking after its first round would
// use no CPU at all, and fails here instead. A starved machine lengthens the
// window rather than failing it, up to scaleWindowMax.

const (
	scaleHosts  = 50
	scaleWindow = 5 * time.Second
	// scaleRounds is how many full rounds over the hosts the window holds
	// at least, and scaleWindowMax how long it may take to.
	scaleRounds    = 3
	scaleWindowMax = 45 * time.Second
	// scaleChildEnv marks the child; scaleEnvHome and scaleEnvRuntime carry
	// the registry it reads.
	scaleChildEnv   = "CRAZE_ROSTER_SCALE_CHILD"
	scaleEnvHome    = "CRAZE_ROSTER_SCALE_HOME"
	scaleEnvRuntime = "CRAZE_ROSTER_SCALE_RUNTIME"
	scaleMark       = "roster-scale: "
)

// scaleResult is what the child measured.
type scaleResult struct {
	// Reached is how many hosts answered at least once; Reachable how many
	// were reachable in the latest Snapshot at the window's end.
	Reached, Reachable int
	// Rounds is how many full rounds the window held: the fewest answers any
	// one host gave within it. Failed is how many attempts failed within it.
	Rounds, Failed int
	// CPU is the child's CPU time over the window, as a fraction of the
	// window's wall time: 0.05 is 5 % of one core.
	CPU    float64
	Window time.Duration
	// Baseline is the child's goroutines before Open; Max the most sampled
	// while open; Left how many more than Baseline remained after Close.
	Baseline, Max, Left int
}

func TestTheRosterStaysInBudgetAtFiftyHosts(t *testing.T) {
	if testing.Short() {
		t.Skip("the scale test runs fifty hosts and a child process")
	}
	env := testEnv(t)
	for n := 1; n <= scaleHosts; n++ {
		bindFakeHost(t, env, n)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestScaleChild$", "-test.v")
	cmd.Env = append(os.Environ(), scaleChildEnv+"=1", scaleEnvHome+"="+env.Home, scaleEnvRuntime+"="+env.CrazeRuntimeDir)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	// The child bounds each of its own waits; this bound is the whole run's
	// ceiling, for a child that never gets that far.
	timer := time.AfterFunc(3*time.Minute, func() { _ = cmd.Process.Kill() })
	defer timer.Stop()
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("the child: %v\n%s\n%s", err, out, stderr.Bytes())
	}
	var res scaleResult
	found := false
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		if rest, ok := strings.CutPrefix(sc.Text(), scaleMark); ok {
			if err := json.Unmarshal([]byte(rest), &res); err != nil {
				t.Fatal(err)
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("the child reported nothing:\n%s", out)
	}
	t.Logf("%d hosts: CPU %.2f%% of one core over %s (%d full rounds, %d attempts failed); %d goroutines at most over a baseline of %d; %d reachable at the end",
		scaleHosts, 100*res.CPU, res.Window, res.Rounds, res.Failed, res.Max, res.Baseline, res.Reachable)
	switch {
	case res.Reached != scaleHosts:
		t.Fatalf("%d of %d hosts ever answered", res.Reached, scaleHosts)
	case res.Rounds < scaleRounds:
		t.Fatalf("%d full rounds over the %d hosts in %s of window, want at least %d", res.Rounds, scaleHosts, res.Window, scaleRounds)
	case res.Reachable != scaleHosts:
		t.Fatalf("%d of %d hosts reachable at the window's end", res.Reachable, scaleHosts)
	case res.Max-res.Baseline >= 64:
		t.Fatalf("%d goroutines while open, want fewer than 64", res.Max-res.Baseline)
	case res.Left != 0:
		t.Fatalf("%d goroutines left after Close", res.Left)
	case !raceEnabled && res.CPU >= 0.05:
		t.Fatalf("CPU %.2f%% of one core, want under 5%%", 100*res.CPU)
	}
}

// TestScaleChild is the scale test's child (TestTheRosterStaysInBudgetAtFiftyHosts):
// skipped unless run as one.
func TestScaleChild(t *testing.T) {
	if os.Getenv(scaleChildEnv) == "" {
		t.Skip("run by TestTheRosterStaysInBudgetAtFiftyHosts")
	}
	env := rundir.Env{Home: os.Getenv(scaleEnvHome), CrazeRuntimeDir: os.Getenv(scaleEnvRuntime), EUID: os.Geteuid()}
	env.CrazeDir = env.Home + "/.craze"
	var res scaleResult
	// answers counts each host's answered attempts, failed the attempts that
	// failed: from the poller, as it takes each result.
	var mu sync.Mutex
	answers := map[string]int{}
	failed := 0
	res.Baseline = runtime.NumGoroutine()
	// Production's roster (roster.Open) but for the count of its results:
	// the registry, the ticker, the budgets, the dial and the peer check are
	// production's, and there is no index.
	r := roster.OpenForTest(nil, roster.TestOptions{
		Hosts: func() ([]rundir.Entry, error) { return rundir.Hosts(env) },
		Check: rundir.DialCheck(os.Geteuid()),
		Applied: func(id string, err error) {
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failed++
				return
			}
			answers[id]++
		},
	})
	// rounds is how many full rounds have been answered since base: the
	// fewest answers any host gave.
	rounds := func(base map[string]int) int {
		mu.Lock()
		defer mu.Unlock()
		n := -1
		for id, a := range answers {
			if n < 0 || a-base[id] < n {
				n = a - base[id]
			}
		}
		if len(answers) < scaleHosts {
			return 0
		}
		return n
	}
	reachable := func(s roster.Snapshot) int {
		n := 0
		for _, row := range s.Running {
			if row.Status == roster.Reachable {
				n++
			}
		}
		return n
	}
	reached := map[string]bool{}
	var last roster.Snapshot
	// Every host answered once, within a minute: a round is fifty attempts,
	// eight at a time, so this is many rounds of room.
	deadline := time.After(time.Minute)
	for len(reached) < scaleHosts {
		select {
		case last = <-r.Updates():
			for _, row := range last.Running {
				if row.Session != nil {
					reached[row.Host.ID] = true
				}
			}
		case <-deadline:
			t.Fatalf("%d of %d hosts answered within a minute", len(reached), scaleHosts)
		}
	}
	res.Reached = len(reached)
	// One more tick's round before the window, so the window is the steady
	// state and not the first dials.
	time.Sleep(2 * roster.TickEvery)
	mu.Lock()
	base, failedBase := maps.Clone(answers), failed
	mu.Unlock()
	before, t0 := cpuTime(t), time.Now()
	sample := time.NewTicker(50 * time.Millisecond)
	least, most := time.After(scaleWindow), time.After(scaleWindowMax)
	for long, done := false, false; !done; {
		select {
		case last = <-r.Updates():
		case <-sample.C:
			res.Max = max(res.Max, runtime.NumGoroutine())
			done = long && rounds(base) >= scaleRounds && reachable(last) == scaleHosts
		case <-least:
			long = true
		case <-most:
			done = true
		}
	}
	sample.Stop()
	res.Window = time.Since(t0)
	res.CPU = (cpuTime(t) - before).Seconds() / res.Window.Seconds()
	res.Rounds, res.Reachable = rounds(base), reachable(last)
	mu.Lock()
	res.Failed = failed - failedBase
	mu.Unlock()
	r.Close()
	// Close has joined every goroutine the roster started; one that handed
	// back its result may still be returning.
	for wait := time.Now().Add(10 * time.Second); ; {
		res.Left = max(0, runtime.NumGoroutine()-res.Baseline)
		if res.Left == 0 || time.Now().After(wait) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	b, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Println(scaleMark + string(b))
}

// cpuTime is this process's CPU time so far, user and system.
func cpuTime(t *testing.T) time.Duration {
	t.Helper()
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		t.Fatal(err)
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}
