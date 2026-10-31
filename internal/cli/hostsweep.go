package cli

import (
	"context"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/charliek/craze/internal/hostspawn"
	"github.com/charliek/craze/internal/rundir"
)

// hostSweepBound is the most a command waits, as it exits, for its sweep of dead
// hosts. It is past the sweep's own bound (hostspawn.SweepBound), so it is met only
// by something that does not answer at all, a stalled file system: the command exits,
// which takes nothing from the record the sweep was at, for the next sweep to finish.
const hostSweepBound = hostspawn.SweepBound + 500*time.Millisecond

// registryReaders is the commands that read the host registry, by name: each ends
// what dead hosts left running (hostspawn.SweepDeadHosts) alongside its own work, so
// a host that died with no waiter attached has its commands signalled by the next of
// these to run. The root is the TUI, and its launcher reads the registry too. The
// hub has a sweep of its own, and a starting host has no business with others'.
var registryReaders = map[string]bool{
	"craze": true, "attach": true, "bridge": true, "cancel": true,
	"new": true, "ps": true, "send": true, "stop": true, "wait": true,
}

// readsRegistry reports whether cmd is one of the registryReaders.
func readsRegistry(cmd *cobra.Command) bool {
	if parent := cmd.Parent(); parent != nil && parent != cmd.Root() {
		return false // a subcommand's subcommand: auth's, say
	}
	return registryReaders[cmd.Name()]
}

// hostSweepDone is the running sweep's end, for joinHostSweep.
var hostSweepDone atomic.Pointer[chan struct{}]

// startHostSweep starts the sweep beside the command, which it adds nothing to the
// start of: it is outside any --timeout, since the command's own clock starts as it
// always did, and it wants no signal handling, since a process that dies mid-sweep
// loses nothing. The environment is read here, on the command's own goroutine.
func startHostSweep() {
	env := rundir.ProcessEnv()
	done := make(chan struct{})
	hostSweepDone.Store(&done)
	go boundedSweep(done, func() { hostspawn.SweepDeadHosts(env, nil) })
}

// hostSweepJoining sees a join with its signals caught: a no-op, and the barrier a test
// sends one at.
var hostSweepJoining = func() {}

// joinHostSweep waits for the sweep as the process exits, at most hostSweepBound, so
// that a short command still lets the SIGKILL round happen with its output already
// written. A long command has let it finish by now. On a healthy machine there is
// nothing to wait for. A SIGINT, a SIGTERM or a SIGHUP here, what the waiting commands
// and the TUI catch, ends the wait at once: the command's handlers are gone by now, and
// without this one the signal would take the exit code the command has already
// determined. The record stays for the next sweep. It reports whether a signal ended it.
func joinHostSweep() (signalled bool) {
	done := hostSweepDone.Swap(nil)
	if done == nil {
		return false
	}
	select {
	case <-*done:
		return false
	default:
	}
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigs)
	hostSweepJoining()
	select {
	case <-*done:
	case <-sigs:
		return true
	case <-time.After(hostSweepBound):
	}
	return false
}

// reapHostGroups ends what the host hostID left running, for a waiter that has just
// seen it gone, and returns when that is done, when ctx ends or at a bound past
// hostspawn.ReapHost's own: another sweep's bound to wait out, and its own.
func reapHostGroups(ctx context.Context, hostID string) {
	done := make(chan struct{})
	go boundedSweep(done, func() { hostspawn.ReapHost(ctx, rundir.ProcessEnv(), hostID, nil) })
	select {
	case <-done:
	case <-ctx.Done():
	case <-time.After(2 * hostSweepBound):
	}
}

// boundedSweep runs sweep and closes done. Nothing about a sweep can fail the
// command that ran it: it has no error to return, and even a panic in one stays here.
func boundedSweep(done chan<- struct{}, sweep func()) {
	defer close(done)
	defer func() { _ = recover() }()
	sweep()
}
