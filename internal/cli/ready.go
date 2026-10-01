package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/charliek/craze/internal/hostspawn"
	"github.com/charliek/craze/internal/rundir"
)

// The ready handshake's host end (plan 030 §3.4). A host the ordinary craze
// spawns (spawnHost, spawn.go; hostspawn.Start) is handed the write end of a
// pipe as fd 3 and told so in its environment, CRAZE_READY_FD=3; it answers on
// it with exactly one JSON line (hostspawn.ReadyLine) and closes it:
//
//   - ok, once the registry entry carries its session's identity — bound,
//     engine installed, claim held, and the identity-bearing rewrite landed
//     (runHost.publish) — so a launcher that reads ok can resolve the session
//     by its craze id, as the held rendezvous does. Not once the provider is
//     ready: a start that fails travels the socket, start_failed, as it always
//     has;
//   - not ok, when serve returns before that: its refusal, word for word as its
//     log has it, and — when the session's claim is another host's — who holds
//     it (hostspawn.ReadyHeld), so the launcher attaches to the holder
//     instead. A refusal of what it was asked to run says so (refused), and
//     the launcher shows it as the choice refused; any other is a host that
//     could not come up (plan 030 X24, astra r7-c4 2).
//
// The descriptor is marked close-on-exec the moment serve starts
// (takeReadyPipe): a descriptor passed in ExtraFiles is not, and an agent child
// or a tool it runs would otherwise hold the launcher's pipe open for its whole
// life. A host started without CRAZE_READY_FD — craze serve run by hand — has
// no pipe and answers nothing.
//
// A spawned host also records its agents' process groups for the spawner's
// last resort (agentGroups), and leaves the environment marks behind: neither
// CRAZE_READY_FD nor CRAZE_HOST_CHILD reaches its agent or anything the agent
// runs.

// readyPipe is a spawned host's end of the ready handshake: one line, then
// closed. Every method is safe on a nil *readyPipe — a host run by hand has
// none — and once the line is sent, or the pipe closed, every later call does
// nothing.
type readyPipe struct {
	mu sync.Mutex
	f  *os.File // nil once sent or closed
}

// takeReadyPipe is the pipe CRAZE_READY_FD names, nil without one. It runs
// first thing in craze serve, before anything can start a process: the
// descriptor is marked close-on-exec at once, and both marks the spawner set
// are removed from this process's environment, which every agent child is
// given (agentEnv), so none of them reaches the agent or its tools. A value
// that does not name an open pipe or FIFO is a usage error: craze serve would
// otherwise write its line into whatever the number happens to be.
func takeReadyPipe() (*readyPipe, error) {
	_ = os.Unsetenv(hostspawn.HostChildEnv)
	raw, ok := os.LookupEnv(hostspawn.ReadyFDEnv)
	if !ok {
		return nil, nil
	}
	_ = os.Unsetenv(hostspawn.ReadyFDEnv)
	fd, err := strconv.Atoi(raw)
	if err != nil || fd < 3 {
		return nil, usagef("craze serve: %s=%q does not name a descriptor", hostspawn.ReadyFDEnv, raw)
	}
	syscall.CloseOnExec(fd)
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil || uint32(st.Mode)&syscall.S_IFMT != syscall.S_IFIFO {
		return nil, usagef("craze serve: %s=%d is not a pipe", hostspawn.ReadyFDEnv, fd)
	}
	return &readyPipe{f: os.NewFile(uintptr(fd), "craze-ready")}, nil
}

// send writes line and closes the pipe: an error is a launcher that did not
// take it — gone, or done waiting (a closed read end is EPIPE here, never a
// signal: the runtime raises SIGPIPE only for stdout and stderr).
func (p *readyPipe) send(line hostspawn.ReadyLine) error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.f == nil {
		return errors.New("the ready line was sent already")
	}
	b, err := json.Marshal(line)
	if err != nil {
		return err
	}
	_, werr := p.f.Write(append(b, '\n'))
	cerr := p.f.Close()
	p.f = nil
	return errors.Join(werr, cerr)
}

// fail answers a host that returned err before it was ready: its refusal as
// the log words it (diagnose), and who holds the session when that is why —
// or, for any other refusal of what it was asked to run (refusedChoice), that
// it was one. Nothing for a nil err, or once the pipe is done.
func (p *readyPipe) fail(cmd *cobra.Command, err error) {
	if p == nil || err == nil {
		return
	}
	msg, code := diagnose(cmd, err)
	if msg == "" {
		// A not-ok line always says why (the launcher refuses one that does
		// not, hostspawn.ParseReady); an exit with nothing to print still has
		// its code.
		msg = fmt.Sprintf("craze serve: exit %d", code)
	}
	line := hostspawn.ReadyLine{Error: msg}
	if held := heldBy(err); held != nil {
		line.Held = &hostspawn.ReadyHeld{HostID: held.Holder.HostID, PID: held.Holder.PID, CrazeSessionID: held.CrazeID}
	} else {
		var refused *choiceRefusal
		line.Refused = errors.As(err, &refused)
	}
	_ = p.send(line)
}

// close closes the pipe unwritten: a host that stopped before it was ready,
// whose launcher reads EOF.
func (p *readyPipe) close() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.f != nil {
		_ = p.f.Close()
		p.f = nil
	}
}

// file is the pipe itself, nil once done: for the handshake's test hooks.
func (p *readyPipe) file() *os.File {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.f
}

// agentGroups is a spawned host's record of every agent process group its
// session spawns (plan 030 §3.4, R2-2, X22): <host-logs>/<hostId>.pgids, one
// hostspawn.AgentGroup line per agent, appended as each agent is spawned
// (agent.Options.AgentGroup) and removed once the host's stop sequence has
// ended every agent its session spawned — the engine's close, and then the
// session's start joined (serveHost.joinStart), since a start caught between
// spawning an agent and adopting it ends that agent itself. An ACP agent
// leads a process group of its own and may outlive its pipes closing; a host
// its spawner has to kill outright — one that never answered, or would not
// stop — cannot end it, and the spawner kills each group recorded here after
// it (hostspawn.Child.KillAgents), each only while its leader is the agent
// recorded.
//
// A record that cannot be made fails the agent's start (record's error; the
// agent is ended at once): an agent nothing wrote down is one nothing could
// end after its host (astra r5-c3 2). The one agent the record cannot cover
// is one whose host is killed outright in the instant between the agent's
// fork and its line's write (accepted: plan 030 X22).
type agentGroups struct {
	path string
	log  io.Writer
	// err is why nothing can be recorded at all — the host logs' directory
	// is unusable — which every record answers.
	err error

	mu     sync.Mutex
	f      *os.File
	closed bool
}

// newAgentGroups is the record for host hostID in the host logs' directory.
// A directory that cannot be used is a record every agent's start fails on.
func newAgentGroups(env rundir.Env, hostID string, log io.Writer) *agentGroups {
	g := &agentGroups{log: log}
	dir, err := rundir.HostLogDir(env)
	if err != nil {
		g.err = fmt.Errorf("the host logs' directory: %w", err)
		return g
	}
	g.path = filepath.Join(dir, hostspawn.AgentGroupsName(hostID))
	return g
}

// record appends pgid's line — the group and its leader's start time — in one
// write(2), opening the file on the first, 0600, never through a link. It is
// agent.Options.AgentGroup: called on the session's start goroutine the
// moment the agent is spawned, and an error fails that start.
//
// The leader is read here, the instant after its spawn, and is the agent only
// while it is this host's own child: the ACP client reaps an agent that exits
// at once, and its number is then anyone's. A leader gone already, or a
// process with its number that is not this host's child, is an agent that has
// exited: nothing is recorded — there is nothing left the spawner could prove
// its own (hostspawn.KillRecordedAgents) — and the start goes on to fail on
// its own. A record after close is an error; nothing spawns after the stop
// sequence has joined the start.
func (g *agentGroups) record(pgid int) error {
	if g == nil || pgid <= 0 {
		return nil
	}
	if g.err != nil {
		return g.err
	}
	id, err := rundir.ProcessIdentity(pgid)
	if errors.Is(err, rundir.ErrNoProcess) || (err == nil && id.PPID != os.Getpid()) {
		fmt.Fprintf(g.log, "craze serve: agent process group %d not recorded: its agent has exited already\n", pgid)
		return nil
	}
	if err != nil {
		return err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return errors.New("the host has stopped")
	}
	if g.f == nil {
		f, err := os.OpenFile(g.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND|syscall.O_NOFOLLOW, 0o600)
		if err != nil {
			return err
		}
		g.f = f
	}
	_, err = g.f.WriteString(hostspawn.AgentGroup{PGID: pgid, Start: id.Start}.Line())
	return err
}

// close ends the record and removes it: the host's stop sequence, once the
// engine's close has ended every agent the session adopted and the start has
// returned, having ended any it had not (serveHost.stop). When that join
// times out the host seals the record instead and kills what it names itself
// (serveHost.killOwnAgents), which removes it.
func (g *agentGroups) close() {
	if g == nil || g.err != nil {
		return
	}
	g.seal()
	if err := os.Remove(g.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(g.log, "craze serve: the agents' process-group record not removed: %v\n", err)
	}
}

// seal ends the record without removing it: every record after it is
// refused, which fails the start that spawned the agent and ends that agent.
func (g *agentGroups) seal() {
	if g == nil || g.err != nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return
	}
	g.closed = true
	if g.f != nil {
		_ = g.f.Close()
		g.f = nil
	}
}
