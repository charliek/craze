package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/charliek/craze/internal/atomicfile"
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

// agentGroups is a host's record of the process groups it runs (plan 030 §3.4,
// R2-2, X22): <host-logs>/<hostId>.pgids, one hostspawn.AgentGroup line per group.
//
// An ACP agent's line is appended as the agent is spawned (agent.Options.AgentGroup)
// and removed once the host's stop sequence has ended every agent its session
// spawned — the engine's close, and then the session's start joined
// (serveHost.joinStart), since a start caught between spawning an agent and
// adopting it ends that agent itself. An ACP agent leads a process group of its
// own and may outlive its pipes closing; a host killed outright cannot end it. A
// spawned host only: one run by hand has no agent to record it for.
//
// A native shell command's line is appended as it starts, before it can be reaped,
// and removed once its group has had its last signal and been reaped
// (agent.Options.ShellGroups): every host's, so that a dead host's commands can be
// ended by the next craze to look (hostspawn.SweepDeadHosts). So a line stands for
// a group running now, and the file stays small however many commands a session
// runs: at most the agents and the commands running at once. Each command costs an
// identity read, an append and either an unlink or a rewrite of those few lines.
//
// A record that cannot be made fails an agent's start (record's error; the agent
// is ended at once): an agent nothing wrote down is one nothing could end after its
// host (astra r5-c3 2). A command's does not: a command runs either way, the
// failure is said on the log, and a command nothing wrote down is one that will
// outlive a host killed outright, as the docs say. That covers a directory that
// cannot be used, an identity or a scope that cannot be read, a failed write and the
// cap. The same goes for the one group the record cannot cover: one whose host is
// killed in the instant between its fork and its line's write (accepted: plan 030
// X22).
type agentGroups struct {
	path string
	log  io.Writer
	// err is why nothing can be recorded at all — the host logs' directory
	// is unusable — which every record answers.
	err error
	// owner is this host, on every line; zero when its scope cannot be read: an
	// agent's line then has the older record's two fields, for the spawner alone to
	// act on, and a shell command goes unrecorded (hostspawn.NewGroupOwner).
	owner hostspawn.GroupOwner
	// write writes a line to the file: (*os.File).WriteString, or a test's that
	// fails partway.
	write func(*os.File, string) (int, error)

	mu     sync.Mutex
	f      *os.File
	lines  []string // what the file holds
	torn   bool     // a failed write left part of a line it could not take back
	full   bool     // the cap's line has been said
	closed bool
}

// newAgentGroups is the record for host hostID in the host logs' directory.
// A directory that cannot be used is a record every agent's start fails on.
func newAgentGroups(env rundir.Env, hostID string, log io.Writer) *agentGroups {
	g := &agentGroups{log: log, write: (*os.File).WriteString}
	dir, err := rundir.HostLogDir(env)
	if err != nil {
		g.err = fmt.Errorf("the host logs' directory: %w", err)
		fmt.Fprintf(log, "craze serve: no process-group record: %v\n", g.err)
		return g
	}
	g.path = filepath.Join(dir, hostspawn.AgentGroupsName(hostID))
	if g.owner, err = hostspawn.NewGroupOwner(); err != nil {
		fmt.Fprintf(log, "craze serve: this host cannot name itself in its process-group record, and shell commands go unrecorded: %v\n", err)
	}
	return g
}

// line is the record's line for a group, with kind and this host when it knows
// them.
func (g *agentGroups) line(pgid int, start uint64, kind string) string {
	if g.owner == (hostspawn.GroupOwner{}) {
		kind = ""
	}
	return hostspawn.AgentGroup{PGID: pgid, Start: start, Kind: kind, Owner: g.owner}.Line()
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
	if len(g.lines) >= hostspawn.AgentGroupsMax {
		return fmt.Errorf("the record holds %d process groups already", len(g.lines))
	}
	return g.append(g.line(pgid, id.Start, hostspawn.GroupAgent))
}

// recordShell writes a native command's process group down, and answers what
// removes it (agent.Options.ShellGroups). pid has not been reaped, and cannot be
// until this returns (tool.GroupRecorder). Nothing it meets fails the command: it
// is said on the log, and the command goes unrecorded. Past the cap too, where a
// host has that many groups running and cannot be helped by one more line. A host
// that cannot name itself writes nothing: a line with no owner is one no sweep acts
// on, and newAgentGroups has said so.
func (g *agentGroups) recordShell(pid int) (forget func()) {
	forget = func() {}
	if g == nil || g.err != nil || g.owner == (hostspawn.GroupOwner{}) {
		return forget
	}
	id, err := rundir.ProcessIdentity(pid)
	if err != nil {
		fmt.Fprintf(g.log, "craze serve: shell command's process group %d not recorded: %v\n", pid, err)
		return forget
	}
	line := g.line(pid, id.Start, hostspawn.GroupShell)
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return forget // the session is closing: its close ends the command
	}
	if len(g.lines) >= hostspawn.AgentGroupsMax {
		if !g.full {
			g.full = true
			fmt.Fprintf(g.log, "craze serve: %d process groups recorded, the most a record holds: this and later shell commands are not, and would outlive a host killed outright\n", len(g.lines))
		}
		return forget
	}
	if err := g.append(line); err != nil {
		fmt.Fprintf(g.log, "craze serve: shell command's process group %d not recorded: %v\n", pid, err)
		return forget
	}
	return func() { g.remove(line) }
}

// append writes line in one write(2), and keeps it. Held under mu. A write that fails
// partway must not cost the next line, which would run on from what it left: what it
// wrote is taken back, or if that fails too, the next line starts a line of its own
// (a blank or a broken line is one a reader passes over).
func (g *agentGroups) append(line string) error {
	if g.f == nil {
		f, err := os.OpenFile(g.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND|syscall.O_NOFOLLOW, 0o600)
		if err != nil {
			return err
		}
		g.f = f
	}
	text := line
	if g.torn {
		text = "\n" + line
	}
	// The file's own size, not what g.lines adds up to: a rewrite that failed left it
	// holding a line g.lines has let go of.
	st, statErr := g.f.Stat()
	if n, err := g.write(g.f, text); err != nil {
		if n > 0 && (statErr != nil || g.f.Truncate(st.Size()) != nil) {
			g.torn = true
		}
		return err
	}
	g.torn = false
	g.lines = append(g.lines, line)
	return nil
}

// remove takes one line out of the record: the file removed when it was the last,
// and otherwise rewritten whole and renamed into place, so that a host killed at
// any instant leaves either file, and a reader never meets half of one. A rewrite
// that fails leaves the line behind, for the next one to take out; an entry for a
// group that has ended is safe, since a reader checks what it names. Nothing is
// durable: a crash of the machine ends what the file would name.
func (g *agentGroups) remove(line string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	i := slices.Index(g.lines, line)
	if g.closed || i < 0 {
		return
	}
	g.lines = slices.Delete(g.lines, i, i+1)
	if g.f != nil {
		_ = g.f.Close()
		g.f = nil
	}
	var err error
	if len(g.lines) == 0 {
		err = os.Remove(g.path)
	} else {
		err = atomicfile.Write(g.path, []byte(strings.Join(g.lines, "")), 0o600)
	}
	if err != nil {
		fmt.Fprintf(g.log, "craze serve: the process-group record not rewritten: %v\n", err)
		return
	}
	g.torn = false // a rewrite or a removal leaves whole lines
}

// close ends the record and removes it: the host's stop sequence, once the
// engine's close has ended every agent the session adopted and every command it
// ran, and the start has returned, having ended any it had not (serveHost.stop).
// A background job left running at the stop is ended there too, so nothing is left
// that the file would name. When that join times out the host seals the record
// instead and kills what it names itself (serveHost.killOwnAgents), which removes
// it.
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
