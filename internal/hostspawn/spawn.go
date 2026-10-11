// Package hostspawn is the process machinery of spawning a detached session
// host, craze serve (plan 030 §3.4; moved out of internal/cli by plan 032
// §3.16, so a spawner other than craze's own launcher — the hub — can use it):
// the host's command line (Args), its start (Start), its ready line
// (ReadReady), its end (Child.Terminate, Child.Settle) and the last resort
// for the agents it leaves (KillRecordedAgents), and the rendezvous with the
// holder of a session the host could not claim (Rendezvous). What a failure
// means, and what is said of it, is the caller's. It imports neither a client
// package (internal/cli, internal/tui) nor the hub: they build on it.
//
// A spawn, in order:
//
//  1. The spawner mints the host's id (rundir.NewHostID) and names its log,
//     <host-logs>/<hostId>.log — named for the host, as the log sweep
//     requires — and its agents' record beside it (AgentGroupsName), so it
//     knows where both are before the host has said a word; Args is the
//     command line that hands them over.
//  2. Start re-executes os.Executable() (Command) as `craze serve <flags>` in
//     a session of its own (setsid), stdin, stdout and stderr on /dev/null,
//     the write end of a pipe as its fd 3 (ReadyFDEnv=3) and HostChildEnv=1
//     marking it. The spawner closes its own copy of the write end at once,
//     so a host that dies before answering is EOF here at once, not a
//     timeout. One goroutine owns every started child's Wait from then on
//     (the reaper): setsid does not reparent, so a host that exits while its
//     spawner lives would otherwise be a zombie until the spawner exits.
//  3. The host's one ready line (ReadyLine) is read, bounded by ReadyWait, at
//     most ReadyLineMax bytes (ReadReady). ok — the host is in the registry
//     under its session's craze id — is the ref the caller dials. Anything
//     else ends the host (Child.Terminate): a timeout, an EOF with no line, a
//     line that is not one or is too long, or the caller's context ending.
//  4. A host refused for a session another host holds answers held: the
//     spawner waits, bounded, for the holder to be in the registry with that
//     session (Rendezvous). The spawner never claims a session itself: the
//     host claims it.
//
// Step 2 has an opt-in (plan 041 X32, reparent.go): StartReparented starts
// the host through a short-lived intermediate — craze itself, marked
// ReparentEnv, which craze's main hands to Reparent before anything else —
// that starts `craze serve` in a session of its own, reports the host's pid
// and start time back, and exits at once. The host is then reparented (to
// init, launchd or the nearest subreaper) and is no descendant of its spawner
// at all: a signal for the spawner's whole process tree, found by parent
// pid, never reaches it. Steps 3 and 4 are the same for it.
//
// Linux and macOS alike: Setsid and ExtraFiles mean the same on both (the
// first extra file is fd 3 in the child), and so do close-on-exec, group
// signals and reparenting. Nothing here reports to roost or herdr, and
// nothing is written to the terminal.
package hostspawn

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/charliek/craze/internal/rundir"
)

// The handshake's bounds: variables only so a test can shorten them (never in
// parallel).
var (
	// ReadyWait is how long a host has to answer (plan 030 §3.4: 10 s).
	ReadyWait = 10 * time.Second
	// TermGrace is how long a host sent SIGTERM has to run its stop
	// sequence — the engine's close ends its agent — before its process
	// group is killed, and the recorded agent groups with it; and how long
	// a host that answered not ok has to exit on its own.
	TermGrace = 5 * time.Second
	// RendezvousWait bounds the held rendezvous: the holder's registry
	// entry must carry the session within it.
	RendezvousWait = 5 * time.Second
	// rendezvousPoll is how often the rendezvous reads the registry.
	rendezvousPoll = 20 * time.Millisecond
)

// Command is the command a host is spawned with, argv craze serve's (its
// first word "serve"): os.Executable() run with argv, in production. A test
// runs its own binary as craze instead (internal/cli's serve_child_test.go).
// Start sets the rest — the session, stdio, fd 3, the environment's marks —
// on what it returns; an Env left nil is craze's own environment.
var Command = func(argv []string) (*exec.Cmd, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return exec.Command(exe, argv...), nil
}

// Pipe is the ready pipe's constructor, os.Pipe: a seam for the test that
// checks no descendant of the host holds it.
var Pipe = os.Pipe

// ReadyTimer is the ready wait's timer, time.After: a seam a test fires by
// hand, so a host that never answers is timed out exactly when the test has
// seen it serving, however slowly it got there.
var ReadyTimer = time.After

// Polled is told each read of the registry the held rendezvous makes: a seam
// that lets a test act while the rendezvous is known to be waiting; a no-op
// in production.
var Polled = func() {}

// Spec is what Args puts on craze serve's command line: the host's id and log,
// and the session's flags.
type Spec struct {
	// HostID and Log are the spawner's (the package's doc comment, step 1):
	// --host-id and --log.
	HostID, Log string
	// Workspace, Provider, Model and AgentBin are passed only when set, so
	// the host resolves what the spawner left to resolving (the provider,
	// the workspace) exactly as the spawner would, from the same environment
	// and config.
	Workspace, Provider, Model, AgentBin string
	// Effort is --effort and Fast --fast (true) or --no-fast (false), each
	// passed only when set (plan 032 §3.11, P6): nil leaves the provider's
	// own default.
	Effort string
	Fast   *bool
	// PluginDirs is --plugin-dir, once each.
	PluginDirs []string
	// NoForce is --no-force: the session handles permission requests
	// rather than allowing everything.
	NoForce bool
	// Ask, Plan and Continue are --ask, --plan and --continue.
	Ask, Plan, Continue bool
	// Load is --load: the session to load, "" for none.
	Load string
	// NoHostStatus is --no-host-status: the spawner reports no session
	// status, so the host leaves the agent's host hook gates in its
	// environment, as an in-process session would.
	NoHostStatus bool
	// Unattended is --unattended (plan 041 §3.2): a host nobody attends,
	// spawned by craze new --wait or craze send — craze prompt's approval
	// policy, no background work, and the session index's provenance record.
	Unattended bool
	// OutliveSpawner is --outlive-spawner (plan 041 C3 review r2, 1): the
	// host serves on when its spawner goes before taking its ready line —
	// craze send's reload, whose host another sender may have found and
	// prompted by then — where any other host stops.
	OutliveSpawner bool
	// RequestID and RequestHash are --request-id and --request-hash, each
	// passed only when set: the hub's session.create that spawns the host
	// (plan 032 §3.10), its idempotency id and its params' hash, which the
	// host writes into its registry entry.
	RequestID, RequestHash string
	// HubPID is --hub-pid, passed only when set (plan 035 P3): the pid of the
	// hub that creates the host, so a start failure the host words for macOS's
	// login session can name the hub to stop. An internal flag between the
	// hub and its hosts, not part of the wire protocol.
	HubPID int
}

// Args is craze serve's command line for s: the host's id and log, then every
// session flag s carries — each only when it says something — and --load,
// --no-host-status, --unattended, --outlive-spawner, --request-id,
// --request-hash and --hub-pid.
// Every value is spelled --flag=value, so none is read as a flag of its own.
func Args(s Spec) []string {
	argv := []string{"serve", "--host-id=" + s.HostID, "--log=" + s.Log}
	str := func(name, v string) {
		if v != "" {
			argv = append(argv, "--"+name+"="+v)
		}
	}
	str("workspace", s.Workspace)
	str("provider", s.Provider)
	str("model", s.Model)
	str("effort", s.Effort)
	if s.Fast != nil {
		if *s.Fast {
			argv = append(argv, "--fast")
		} else {
			argv = append(argv, "--no-fast")
		}
	}
	str("agent-bin", s.AgentBin)
	for _, d := range s.PluginDirs {
		argv = append(argv, "--plugin-dir="+d)
	}
	if s.NoForce {
		argv = append(argv, "--no-force")
	}
	if s.Ask {
		argv = append(argv, "--ask")
	}
	if s.Plan {
		argv = append(argv, "--plan")
	}
	if s.Continue {
		argv = append(argv, "--continue")
	}
	str("load", s.Load)
	if s.NoHostStatus {
		argv = append(argv, "--no-host-status")
	}
	if s.Unattended {
		argv = append(argv, "--unattended")
	}
	if s.OutliveSpawner {
		argv = append(argv, "--outlive-spawner")
	}
	str("request-id", s.RequestID)
	str("request-hash", s.RequestHash)
	if s.HubPID > 0 {
		argv = append(argv, "--hub-pid="+strconv.Itoa(s.HubPID))
	}
	return argv
}

// Child is a host this process started, and the one goroutine that waits for
// it (the reaper) — or, for a host started reparented (StartReparented),
// which is not this process's child and is reaped by its new parent, what
// tells that host from any later process with its pid: its start time. Every
// method means the same for both; for a reparented host, "exited and been
// reaped" is a look at its identity (gone) — a zombie, which its new parent
// may never reap, counts as gone — and a signal goes to it only while that
// identity is the host's, bound to the process by a pidfd on Linux
// (signalHost).
type Child struct {
	// cmd is the host's own command; nil for a reparented host.
	cmd *exec.Cmd
	pid int
	// start is a reparented host's start time (rundir.ProcIdentity), read
	// by its intermediate while the host was still that process's child —
	// so it is the host's, never a stranger's; 0 for a child of this
	// process's own.
	start uint64
	// groups is the host's record of its agents' process groups
	// (AgentGroupsName).
	groups string
	// log is the host's log, where the spawner notes a recorded group it
	// did not kill (note).
	log  string
	done chan struct{}
	err  error // cmd.Wait's, once done is closed
	// A reparented host's done is closed by whoever first sees it gone
	// (gone), once (closeDone); watch starts Done's poll of it once.
	closeDone, watch sync.Once
}

// Start starts `craze serve argv…` detached (the package's doc comment, step
// 2) and answers it with the read end of its ready pipe. unset names
// variables left out of the host's environment — CRAZE_AGENT_BIN for a host
// whose provider is not its launch's (plan 032 §3.11, P7) — which is
// otherwise this process's own (or Command's). groups and log are the host's
// record of its agents and its log.
func Start(argv, unset []string, groups, log string) (*Child, *os.File, error) {
	cmd, err := Command(argv)
	if err != nil {
		return nil, nil, err
	}
	if len(unset) > 0 {
		env := cmd.Env
		if env == nil {
			env = os.Environ()
		}
		cmd.Env = withoutEnv(env, unset...)
	}
	return StartCmd(cmd, HostChildEnv, groups, log)
}

// StartCmd is Start for a command its caller built — a host's (Start), or
// the hub's (plan 032 §3.5: internal/hub, through a seam of its own): cmd is
// started detached exactly as a host is — a session of its own, stdio on
// /dev/null, the write end of a fresh ready pipe as its fd 3 — with
// ReadyFDEnv=3 and marker=1 in its environment, and one goroutine waits for
// it. The environment is cmd.Env, or this process's own when that is nil,
// less any ReadyFDEnv or marker already in it; cmd.Dir is the caller's. groups
// and log are the child's record of its agents and its log, "" for none.
func StartCmd(cmd *exec.Cmd, marker, groups, log string) (*Child, *os.File, error) {
	r, w, err := Pipe()
	if err != nil {
		return nil, nil, err
	}
	env := cmd.Env
	if env == nil {
		env = os.Environ()
	}
	cmd.Env = append(withoutEnv(env, ReadyFDEnv, marker), ReadyFDEnv+"=3", marker+"=1")
	// stdin, stdout and stderr nil are /dev/null: the host writes to its log.
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	cmd.ExtraFiles = []*os.File{w}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	err = cmd.Start()
	// Ours closed at once, whatever Start said: the host's copy is the only
	// write end left, so its exit is EOF here.
	_ = w.Close()
	if err != nil {
		_ = r.Close()
		return nil, nil, err
	}
	c := &Child{cmd: cmd, pid: cmd.Process.Pid, groups: groups, log: log, done: make(chan struct{})}
	go func() {
		c.err = cmd.Wait()
		close(c.done)
	}()
	return c, r, nil
}

// withoutEnv copies environ less every entry whose key — everything before the
// first "=" — is exactly one of keys. A value holding "=" is untouched, and so
// is a key that merely starts with one of keys.
func withoutEnv(environ []string, keys ...string) []string {
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		k, _, _ := strings.Cut(kv, "=")
		if slices.Contains(keys, k) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// PID is the host's pid, the leader of its own session and process group.
func (c *Child) PID() int { return c.pid }

// Groups is the host's record of its agents' process groups.
func (c *Child) Groups() string { return c.groups }

// Done is closed once the host has exited and been reaped. A reparented
// host's is closed by a poll of its identity (gone), every reparentPoll,
// which the first call starts.
func (c *Child) Done() <-chan struct{} {
	if c.cmd == nil {
		c.watch.Do(func() {
			go func() {
				for !c.gone() {
					time.Sleep(reparentPoll)
				}
			}()
		})
	}
	return c.done
}

// Err is the host's exit, cmd.Wait's error, once Done is closed: nil for an
// exit 0 — and always nil for a reparented host, whose exit status is its new
// parent's to collect.
func (c *Child) Err() error { return c.err }

// Exited reports whether the host has exited and been reaped.
func (c *Child) Exited() bool {
	if c.cmd == nil {
		return c.gone()
	}
	select {
	case <-c.done:
		return true
	default:
		return false
	}
}

// WaitExit waits up to d for the host to exit: whether it did. A reparented
// host is looked for every reparentPoll (gone).
func (c *Child) WaitExit(d time.Duration) bool {
	if c.cmd == nil {
		deadline := time.Now().Add(d)
		for !c.gone() {
			left := time.Until(deadline)
			if left <= 0 {
				return false
			}
			time.Sleep(min(reparentPoll, left))
		}
		return true
	}
	select {
	case <-c.done:
		return true
	case <-time.After(d):
		return false
	}
}

// Settle gives a host that is exiting of its own accord — it answered not ok,
// or closed its pipe unwritten — TermGrace to, and terminates it past that.
// One that exited without its stop sequence (a crash) may have left an agent:
// its record is read either way (KillAgents).
func (c *Child) Settle() {
	if !c.WaitExit(TermGrace) {
		c.Terminate()
		return
	}
	c.KillAgents()
}

// Terminate ends the host, and everything it spawned (plan 030 §3.4, R2-2):
// SIGTERM, which runs its stop sequence — the engine's close ends its agent,
// whose process group is the agent's own — and TermGrace for that; past it,
// SIGKILL to the host's process group (setsid made it the leader of its own),
// a second grace for the reaper, and then, whichever way the host went,
// SIGKILL to every agent group it recorded that its agent still leads
// (KillAgents): an agent that outlives its pipes is ended there. It returns
// once all of that is done; the host is reaped unless it could not be killed
// within the second grace (an uninterruptible wait), when the reaper still
// takes it whenever it goes.
func (c *Child) Terminate() {
	c.End(TermGrace)
	c.KillAgents()
}

// End is Terminate without the agents' record: SIGTERM, grace for the child
// to exit, then SIGKILL to its process group (setsid made it the leader of its
// own) and a second grace for the reaper. It reports whether the child has
// exited and been reaped. A child that records no agents — the hub (plan 032
// §3.8), which runs none — is ended with it alone, within a grace of its
// caller's.
func (c *Child) End(grace time.Duration) bool {
	if !c.Exited() {
		c.terminate()
		if !c.WaitExit(grace) {
			if !c.Exited() {
				c.killGroup()
			}
			c.WaitExit(grace)
		}
	}
	return c.Exited()
}

// Kill ends the host at once — SIGKILL to its process group (setsid made it
// the leader of its own), no SIGTERM and no grace: a host that never got to
// its ready line owes nothing — and waits for the reaper. A host in an
// uninterruptible wait keeps it waiting until it goes. Like End's SIGKILL,
// the group signal follows an Exited check the reaper can overtake: a group
// id reused in between would be signalled instead (SF-120).
func (c *Child) Kill() {
	if !c.Exited() {
		c.killGroup()
	}
	<-c.Done()
}

// terminate sends the host SIGTERM. A child of this process's own is
// signalled on its Process, not the pid: once the reaper has waited for it,
// this is refused rather than sent to whatever reused the pid. A reparented
// host, which no reaper here waits for, only while the process with its pid
// is provably the host — started at the host's instant (rundir.ProcIdentity)
// — and on Linux through a pidfd that binds the signal to that process
// (signalHost).
func (c *Child) terminate() {
	if c.cmd != nil {
		_ = c.cmd.Process.Signal(syscall.SIGTERM)
		return
	}
	signalHost(c.pid, c.start, syscall.SIGTERM, false)
}

// killGroup sends SIGKILL to the host's process group (setsid made it the
// leader of its own): by its id for a child of this process's own, after an
// Exited check the reaper can overtake (SF-120); for a reparented host only
// while its leader is provably the host, through its pidfd on Linux
// (signalHost).
func (c *Child) killGroup() {
	if c.cmd != nil {
		_ = syscall.Kill(-c.pid, syscall.SIGKILL)
		return
	}
	signalHost(c.pid, c.start, syscall.SIGKILL, true)
}

// gone reports whether a reparented host has exited: no process has its pid,
// the one that has it started at another instant than the host — its pid
// reused — or it is the host, a zombie (plan 041 C6 r1, 3): exited, and left
// for a new parent to reap that may not, or not yet — a container's first
// process, a slow subreaper. A pid whose identity cannot be read is not gone.
// Seen gone once, the host stays gone — a later process given its pid never
// makes it alive again — and its done is closed.
func (c *Child) gone() bool {
	select {
	case <-c.done:
		return true
	default:
	}
	id, err := rundir.ProcessIdentity(c.pid)
	if !errors.Is(err, rundir.ErrNoProcess) && (err != nil || (id.Start == c.start && !id.Zombie)) {
		return false
	}
	c.closeDone.Do(func() { close(c.done) })
	return true
}

// isHost reports whether pid is the host whose start time is start: alive,
// or a zombie whose group may still have members.
func isHost(pid int, start uint64) bool {
	id, err := rundir.ProcessIdentity(pid)
	return err == nil && id.Start == start
}

// checkThenSignal is signalHost without a pidfd: the host's identity checked
// (isHost), then sig sent to its pid — or its group, with group — by number.
func checkThenSignal(pid int, start uint64, sig syscall.Signal, group bool) bool {
	if !isHost(pid, start) {
		return false
	}
	target := pid
	if group {
		target = -pid
	}
	return syscall.Kill(target, sig) == nil
}

// KillAgents kills every agent process group the host recorded and removes
// the record: nothing, for a host that stopped cleanly and removed it itself
// (KillRecordedAgents, at once: the host has gone, and so has any grace an
// agent would have had from it). The host's log says what was left alone
// (note).
func (c *Child) KillAgents() {
	KillRecordedAgents(c.groups, 0, c.note)
}

// note appends one line to the host's log: what the spawner did after the
// host, where whoever reads the host's log looks. Best effort — a log that
// cannot be opened loses it — and never through a link.
func (c *Child) note(format string, args ...any) {
	if c.log == "" {
		return
	}
	f, err := os.OpenFile(c.log, os.O_WRONLY|os.O_CREATE|os.O_APPEND|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(f, "craze: "+format+"\n", args...)
	_ = f.Close()
}

// Rendezvous's answers other than the holder's entry, and its caller's
// context's error.
var (
	// ErrHolderGone is a held session's holder that exited, or released the
	// session, before the rendezvous found it serving: a spawn past it may
	// claim the session now.
	ErrHolderGone = errors.New("the session's holder is gone")
	// ErrHolderElsewhere is a holder whose live entry serves another
	// session.
	ErrHolderElsewhere = errors.New("the session's holder serves another session")
	// ErrHolderNoSocket is a holder still there at the bound that the
	// registry does not list serving the session: its socket opted out, or
	// its entry is not written yet.
	ErrHolderNoSocket = errors.New("the session's holder serves no control socket")
)

// Rendezvous finds the host holding a session a spawn was refused (plan 030
// §3.4): polled every rendezvousPoll, up to RendezvousWait, until the
// registry lists the holder's host id serving that session — its identity
// published, which a host writes once bound with the session's engine; never
// its ready flag, which waits for the provider's start. A lock that names no
// holder yet has no host id to look for, and any live host serving the
// session will do.
//
// The holder is gone (ErrHolderGone) when its process has died (the pid its
// lock names), or when its entry has been seen and is no longer listed; a
// holder still there at the bound is refused as the picker refuses it —
// serving no socket (ErrHolderNoSocket), or another session
// (ErrHolderElsewhere). A caller's context that ends first is its error.
func Rendezvous(ctx context.Context, env rundir.Env, held *ReadyHeld) (rundir.Entry, error) {
	deadline := time.Now().Add(RendezvousWait)
	seen := false
	for {
		Polled()
		entries, _ := rundir.Hosts(env)
		listed := false
		for _, e := range entries {
			switch {
			case held.HostID != "" && e.HostID != held.HostID:
			case e.CrazeSessionID == held.CrazeSessionID:
				return e, nil
			case held.HostID == "":
			case e.CrazeSessionID == "":
				listed = true
			default:
				return rundir.Entry{}, ErrHolderElsewhere
			}
		}
		if listed {
			seen = true
		}
		if (seen && !listed) || (held.PID > 0 && !processAlive(held.PID)) {
			return rundir.Entry{}, ErrHolderGone
		}
		if time.Now().After(deadline) {
			return rundir.Entry{}, ErrHolderNoSocket
		}
		select {
		case <-ctx.Done():
			return rundir.Entry{}, ctx.Err()
		case <-time.After(rendezvousPoll):
		}
	}
}

// processAlive reports whether pid names a live process (a zombie counts:
// it has not been reaped).
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
