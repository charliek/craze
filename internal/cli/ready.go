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

	"github.com/charliek/craze/internal/rundir"
)

// The ready handshake's host end (plan 030 §3.4). A host the ordinary craze
// spawns (spawnHost, spawn.go) is handed the write end of a pipe as fd 3 and
// told so in its environment, CRAZE_READY_FD=3; it answers on it with exactly
// one JSON line (readyLine) and closes it:
//
//   - ok, once the registry entry carries its session's identity — bound,
//     engine installed, claim held, and the identity-bearing rewrite landed
//     (runHost.publish) — so a launcher that reads ok can resolve the session
//     by its craze id, as the held rendezvous does. Not once the provider is
//     ready: a start that fails travels the socket, start_failed, as it always
//     has;
//   - not ok, when serve returns before that: its refusal, word for word as its
//     log has it, and — when the session's claim is another host's — who holds
//     it (readyHeld), so the launcher attaches to the holder instead.
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

// The environment a spawner hands its host: the ready pipe's descriptor, and
// the mark of a host spawned detached.
const (
	readyFDEnv   = "CRAZE_READY_FD"
	hostChildEnv = "CRAZE_HOST_CHILD"
)

// readyLineMax bounds the ready line, its newline included: the launcher reads
// no further (spawn.go), and a longer line is a host it terminates.
const readyLineMax = 64 << 10

// readyLine is the one line a spawned host writes on its ready pipe. OK names
// the host (HostID — the id the spawner minted and passed, --host-id), its
// socket, its session's craze id and the craze that serves it (the host may be
// a newer binary than its launcher, when craze was upgraded on disk between
// the two). Not OK carries the host's refusal (Error) and, for a session
// another host holds, Held.
type readyLine struct {
	OK             bool       `json:"ok"`
	HostID         string     `json:"hostId,omitempty"`
	Socket         string     `json:"socket,omitempty"`
	CrazeSessionID string     `json:"crazeSessionId,omitempty"`
	CrazeVersion   string     `json:"crazeVersion,omitempty"`
	Error          string     `json:"error,omitempty"`
	Held           *readyHeld `json:"held,omitempty"`
}

// readyHeld is who holds the session a host could not claim, as its lock
// names it: the holder's host id and its pid — each zero when the lock names
// no holder yet (the instant between a claim's flock and its line, "pid ?") —
// and the session. The launcher waits for that host's registry entry to
// carry the session (the held rendezvous), and the pid tells it at once when
// the holder has died meanwhile rather than after its whole wait (plan 030
// §3.4's "a holder that crashes or releases meanwhile").
type readyHeld struct {
	HostID         string `json:"hostId"`
	PID            int    `json:"pid,omitempty"`
	CrazeSessionID string `json:"crazeSessionId"`
}

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
	_ = os.Unsetenv(hostChildEnv)
	raw, ok := os.LookupEnv(readyFDEnv)
	if !ok {
		return nil, nil
	}
	_ = os.Unsetenv(readyFDEnv)
	fd, err := strconv.Atoi(raw)
	if err != nil || fd < 3 {
		return nil, usagef("craze serve: %s=%q does not name a descriptor", readyFDEnv, raw)
	}
	syscall.CloseOnExec(fd)
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil || uint32(st.Mode)&syscall.S_IFMT != syscall.S_IFIFO {
		return nil, usagef("craze serve: %s=%d is not a pipe", readyFDEnv, fd)
	}
	return &readyPipe{f: os.NewFile(uintptr(fd), "craze-ready")}, nil
}

// send writes line and closes the pipe: an error is a launcher that did not
// take it — gone, or done waiting (a closed read end is EPIPE here, never a
// signal: the runtime raises SIGPIPE only for stdout and stderr).
func (p *readyPipe) send(line readyLine) error {
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
// the log words it (diagnose), and who holds the session when that is why.
// Nothing for a nil err, or once the pipe is done.
func (p *readyPipe) fail(cmd *cobra.Command, err error) {
	if p == nil || err == nil {
		return
	}
	msg, _ := diagnose(cmd, err)
	line := readyLine{Error: msg}
	if held := heldBy(err); held != nil {
		line.Held = &readyHeld{HostID: held.Holder.HostID, PID: held.Holder.PID, CrazeSessionID: held.CrazeID}
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

// agentGroupsName is a host's record of its agents' process groups, in the
// host logs' directory beside its log.
func agentGroupsName(hostID string) string { return hostID + ".pgids" }

// agentGroups is a spawned host's record of every agent process group its
// session spawns (plan 030 §3.4, R2-2): <host-logs>/<hostId>.pgids, one
// decimal pgid per line, appended as each agent is spawned
// (agent.Options.AgentGroup) and removed when the host stops cleanly. An ACP
// agent leads a process group of its own and may outlive its pipes closing;
// the host's own stop sequence ends it, but a host its spawner has to kill
// outright — one that never answered, or would not stop — cannot, and the
// spawner kills each group recorded here after it (hostChild.killAgents).
//
// A record that cannot be written is one line on the log, once, and the
// session runs on: the record is a last resort, not a condition of running.
// The one agent it cannot cover is one whose host is killed in the instant
// between the agent's spawn and its line's write.
type agentGroups struct {
	path string
	log  io.Writer

	mu     sync.Mutex
	f      *os.File
	warned bool
	closed bool
}

// newAgentGroups is the record for host hostID in the host logs' directory,
// or nil — recording nothing — when that directory cannot be used, which is
// said on log.
func newAgentGroups(env rundir.Env, hostID string, log io.Writer) *agentGroups {
	dir, err := rundir.HostLogDir(env)
	if err != nil {
		fmt.Fprintf(log, "craze serve: the agents' process groups will not be recorded: %v\n", err)
		return nil
	}
	return &agentGroups{path: filepath.Join(dir, agentGroupsName(hostID)), log: log}
}

// record appends pgid, one write(2) for the line; it opens the file on the
// first, 0600, never through a link. Nothing once closed.
func (g *agentGroups) record(pgid int) {
	if g == nil || pgid <= 0 {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return
	}
	var err error
	if g.f == nil {
		g.f, err = os.OpenFile(g.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND|syscall.O_NOFOLLOW, 0o600)
	}
	if err == nil {
		_, err = g.f.WriteString(strconv.Itoa(pgid) + "\n")
	}
	if err != nil && !g.warned {
		g.warned = true
		fmt.Fprintf(g.log, "craze serve: agent process group %d not recorded: %v\n", pgid, err)
	}
}

// close ends the record and removes it: the host's stop sequence, once the
// engine's close has ended every agent it spawned. A spawn after it records
// nothing — a session closed while its agent spawned shuts that agent down
// itself.
func (g *agentGroups) close() {
	if g == nil {
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
	if err := os.Remove(g.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(g.log, "craze serve: the agents' process-group record not removed: %v\n", err)
	}
}
