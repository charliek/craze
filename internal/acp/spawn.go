package acp

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// shutdownGrace is how long the agent — and then, once it has exited, what is
// left in its process group — has between SIGTERM and SIGKILL, and how long
// the reaper waits after a group SIGKILL for the group to empty (reaper.go).
const shutdownGrace = 2 * time.Second

// groupPoll is how often the reaper looks for the agent's process group to
// have no live member left after a signal.
const groupPoll = 20 * time.Millisecond

// ErrAgentExited is Client.Close's answer when the non-blocking probe of
// child.exitedCh found it already closed: the reaper had observed the agent's
// exit before craze's first Close on it sampled it. It does not mean Close
// failed — match it with errors.Is, never a nil check, because a
// craze-initiated close still returns nil. There is a real window between
// the process exiting and the reaper closing exitedCh, so an agent that
// crashes in the same microseconds as craze's own close is reported as
// craze-initiated (§9, accepted).
var ErrAgentExited = errors.New("acp: agent exited")

// agentExitedErr wraps ErrAgentExited with how the reaper recorded the agent
// end (Child.exitErr): an *ExitError when it exited non-zero or on a signal,
// and "exit 0" when it exited clean — the one case Conn.Err() cannot
// otherwise tell apart from craze's own close (both read ErrClosed).
func agentExitedErr(exitErr error) error {
	if exitErr != nil {
		return fmt.Errorf("%w: %v", ErrAgentExited, exitErr)
	}
	return fmt.Errorf("%w: exit 0", ErrAgentExited)
}

// ExitError is how the agent ended — non-zero, or on a signal — as the reaper
// observed it before reaping it (plan 032 §3.2a): the status the kernel
// reported for the exit (waitid's siginfo on Linux, the NOTE_EXIT kevent's
// data on macOS). It is what a call still pending when the agent exits fails
// with, wrapped as "acp: agent exited: …", and what Close's ErrAgentExited
// carries. Its message is os.ProcessState's ("exit status 3", "signal:
// killed"), the words of the *exec.ExitError the reap that follows returns
// for the same exit.
type ExitError struct {
	Status syscall.WaitStatus
}

func (e *ExitError) Error() string {
	var s string
	switch st := e.Status; {
	case st.Exited():
		s = "exit status " + strconv.Itoa(st.ExitStatus())
	case st.Signaled():
		s = "signal: " + st.Signal().String()
	default:
		s = "wait status " + strconv.FormatUint(uint64(st), 10)
	}
	if e.Status.CoreDump() {
		s += " (core dumped)"
	}
	return s
}

// ExitCode is the agent's exit code, or -1 when a signal ended it — what
// *exec.ExitError's ExitCode answers for the same exit.
func (e *ExitError) ExitCode() int {
	if e.Status.Exited() {
		return e.Status.ExitStatus()
	}
	return -1
}

// exitErrorOf is how an observed wait status reads as an error: nil for an
// exit 0, an *ExitError for anything else.
func exitErrorOf(st syscall.WaitStatus) error {
	if st.Exited() && st.ExitStatus() == 0 {
		return nil
	}
	return &ExitError{Status: st}
}

type SpawnOptions struct {
	Binary string
	// Candidates is the provider's PATH lookup order when Binary and
	// CRAZE_AGENT_BIN are both unset. An explicit Binary still wins.
	Candidates []string
	Args       []string
	Dir        string
	Env        []string
	Stderr     io.Writer
	Dialect    DialectID
}

func ResolveBinary(explicit string) (string, error) {
	return ResolveBinaryCandidates(explicit, []string{"cursor-agent", "agent"})
}

// ResolveBinaryCandidates resolves explicit, then CRAZE_AGENT_BIN, then the
// candidate list. An explicit binary is returned as-is after the PATH and
// absolute-path checks; candidates are PATH lookups only, so a grok lookup
// can never fall back to a stray "agent" on PATH.
func ResolveBinaryCandidates(explicit string, candidates []string) (string, error) {
	if explicit == "" {
		explicit = os.Getenv("CRAZE_AGENT_BIN")
	}
	return LookupBinary(explicit, candidates)
}

// LookupBinary is ResolveBinaryCandidates without CRAZE_AGENT_BIN: explicit
// when it is set, else the candidate list, by the same checks. It is the
// lookup for a caller that has already decided whether the variable is its
// session's to take (plan 032 §3.11, P7: only the launch's own provider's),
// and must not have it read again here for a session of another provider.
//
// Its error names what it looked for, in its own words and never in
// exec's (plan 037 LC-3): "agent binary not found: <path>" for an explicit
// one, and for the candidates the first of them and the rest it tried —
// "agent binary not found: cursor-agent is not on PATH (also tried agent)".
// What to do about it is the caller's to add: acp knows neither the
// provider nor the config file.
func LookupBinary(explicit string, candidates []string) (string, error) {
	var names []string
	if explicit != "" {
		names = append(names, explicit)
	} else {
		names = append(names, candidates...)
	}
	for _, name := range names {
		path, err := exec.LookPath(name)
		if err == nil {
			return path, nil
		}
		if filepath.IsAbs(name) {
			if _, statErr := os.Stat(name); statErr == nil {
				return name, nil
			}
		}
	}
	switch {
	case explicit != "":
		return "", fmt.Errorf("agent binary not found: %s", explicit)
	case len(names) == 0:
		return "", errors.New("agent binary not found: no binary to look for")
	case len(names) == 1:
		return "", fmt.Errorf("agent binary not found: %s is not on PATH", names[0])
	}
	return "", fmt.Errorf("agent binary not found: %s is not on PATH (also tried %s)", names[0], strings.Join(names[1:], ", "))
}

// Child is the agent process Spawn started, the leader of a process group of
// its own (Setpgid: the group's id is the agent's pid), and its reaper
// (reaper.go), the one goroutine that waits for it and signals it. It goes
// through three states: running; exited — the exit observed but the agent
// not reaped, so the zombie keeps its pid, and with it the group's id, from
// being reused (exitedCh); and reaped (reapedCh).
//
// How the agent ended (exitErr, statusCh) is known at the exit when the
// observation carries the status — Linux's always does, macOS's NOTE_EXIT
// does — and only at the reap when it does not: a macOS agent found already
// a zombie, whose kernel record saturates the status (reaper_darwin.go). So
// statusCh closes with exitedCh in the first case, and after it, at the reap,
// in the second; whatever reads exitErr waits for statusCh. In the fallback
// (reapPolling) the exit, its status and the reap are one wait4.
type Child struct {
	cmd *exec.Cmd
	// pid is the agent's, kept here: the fallback releases cmd.Process
	// (reapPolling), which sets its Pid to -1. pgid is the same number, the
	// agent's group's id.
	pid  int
	pgid int
	// exitedCh is closed once the reaper has observed the agent's exit,
	// without reaping it.
	exitedCh chan struct{}
	// statusCh is closed once exitErr — how the agent ended (exitErrorOf:
	// nil for an exit 0) — and endedByShutdown are set: at the exit, or at
	// the reap (above).
	statusCh chan struct{}
	exitErr  error
	// endedByShutdown is whether a shutdown had been asked for when the
	// reaper observed the exit — latched then, so a Shutdown that comes
	// later, while the reaper cleans up after an agent that exited on its
	// own, does not make that exit craze's (endErr).
	endedByShutdown bool
	// reapedCh is closed once the reaper has reaped the agent — after the
	// last signal it will ever send, and after statusCh; waitErr is
	// cmd.Wait's answer, set before (in the fallback, which reaps with wait4
	// first, the status that reap took).
	reapedCh chan struct{}
	waitErr  error
	// shutdownCh is closed by the first Shutdown: the reaper's shutdown
	// request.
	shutdownCh   chan struct{}
	shutdownOnce sync.Once
	stderrDone   chan struct{}
	// stderrTail keeps the end of what the agent wrote to its stderr, on its
	// way to the sink (Client.StderrTail); nil for a child Spawn did not
	// start.
	stderrTail *stderrTail
}

// newChild starts watching cmd, just started, and its reaper. Whether the
// exit can be observed without a reap is settled here, before the reaper
// runs and so before any signal: if it cannot, the reaper is the fallback
// from the start (reapPolling), which never signals the group.
func newChild(cmd *exec.Cmd) *Child {
	ch := &Child{
		cmd:        cmd,
		pid:        cmd.Process.Pid,
		pgid:       cmd.Process.Pid,
		exitedCh:   make(chan struct{}),
		statusCh:   make(chan struct{}),
		reapedCh:   make(chan struct{}),
		shutdownCh: make(chan struct{}),
		stderrDone: make(chan struct{}),
	}
	// Right after the start: on macOS this registers the exit watch, and a
	// child that has already exited by then is noticed as one (reaper_darwin.go).
	observe, err := watch(ch.pid)
	if err != nil {
		go ch.reapPolling()
	} else {
		go ch.reap(observe)
	}
	return ch
}

// endErr is what the calls still pending fail with once the agent has ended
// and its status is known (statusCh): ErrClosed when craze had asked for the
// end (Shutdown) by the time the exit was observed, or the agent exited 0 —
// the connection's own word for both — and otherwise "acp: agent exited: …"
// with how it ended.
func (ch *Child) endErr() error {
	if ch.exitErr == nil || ch.endedByShutdown {
		return ErrClosed
	}
	return fmt.Errorf("acp: agent exited: %w", ch.exitErr)
}

// exitAfterEOF bounds endedWith's wait for the agent's exit.
const exitAfterEOF = time.Second

// endedWith is the connection's ended (Conn): what the calls still pending
// fail with when the agent's stdout ends. An agent's stdout ends as it exits
// — a moment before the reaper observes the exit — or, when a tool held it,
// as the reaper's cleanup ends the tool, which comes before the reap that
// gives the status when the observation had none (Child). So the end waits
// up to exitAfterEOF for the exit and then for its status, and the calls
// fail with endErr; an agent that closed its stdout and runs on is the
// connection's own end, def.
func (ch *Child) endedWith(def error) error {
	exit := time.NewTimer(exitAfterEOF)
	defer exit.Stop()
	select {
	case <-ch.exitedCh:
	case <-exit.C:
		return def
	}
	<-ch.statusCh
	return ch.endErr()
}

// setStatus records how the agent ended and closes statusCh.
func (ch *Child) setStatus(err error) {
	ch.exitErr = err
	close(ch.statusCh)
}

// PID is the agent's pid, 0 for no child.
func (ch *Child) PID() int {
	if ch == nil {
		return 0
	}
	return ch.pid
}

// Wait waits for the agent to have been reaped and its stderr copied to the
// end.
func (ch *Child) Wait() {
	if ch == nil {
		return
	}
	<-ch.reapedCh
	if ch.stderrDone != nil {
		<-ch.stderrDone
	}
}

// Shutdown asks the reaper to end the agent and waits until it has been
// reaped. It sends no signal itself: the reaper sends every one (reaper.go),
// so none can follow the reap. An agent still running gets SIGTERM, and
// SIGKILL if it outlives the grace; then, as after an agent's own exit, what
// is left alive in its process group — a tool's subprocess — gets SIGTERM and,
// past a second grace, SIGKILL, and the group a last SIGKILL. (In the
// fallback, reapPolling, the agent alone is signalled.) An agent that had
// already exited and been reaped is not signalled again: its group was
// cleaned up at its exit, and its id may by now be someone else's. Every
// call, the first and any later one, returns once the agent has been reaped.
func (ch *Child) Shutdown() {
	if ch == nil || ch.cmd == nil || ch.cmd.Process == nil {
		return
	}
	ch.shutdownOnce.Do(func() { close(ch.shutdownCh) })
	<-ch.reapedCh
}

func Spawn(opts SpawnOptions) (*Client, error) {
	bin, err := ResolveBinaryCandidates(opts.Binary, opts.Candidates)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(bin, opts.Args...)
	if opts.Dir != "" {
		cmd.Dir = opts.Dir
	}
	if opts.Env != nil {
		cmd.Env = opts.Env
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start agent: %w", err)
	}

	child := newChild(cmd)

	stderrDst := opts.Stderr
	if stderrDst == nil {
		stderrDst = io.Discard
	}
	// Every byte still reaches the sink; the recorder keeps the last of
	// them for a failed start's words (Client.StderrTail).
	child.stderrTail = newStderrTail(stderrDst)
	go func() {
		_, _ = io.Copy(child.stderrTail, stderr)
		close(child.stderrDone)
	}()

	conn := NewConn(stdout, stdin)
	conn.ended = child.endedWith
	client := newClient(conn, child)
	if opts.Dialect != "" {
		client.dialect = opts.Dialect
	}
	conn.Start()
	// The calls still pending fail at the agent's exit, with how it ended,
	// not at its reap: that waits for its process group to be cleaned up,
	// which a tool that ignores SIGTERM stretches by a grace or two. Where
	// the status is known only at the reap (Child), they fail then, with it.
	// The agent's stdin is closed then too: nothing more is for it, and a
	// write still blocked there — a pipe it stopped reading, held open by a
	// tool it left behind — fails now, its call answered with that status,
	// rather than at the tool's end or the client's Close. Its stdout stays
	// open, so what it wrote before it went is still read (X72).
	go func() {
		<-child.statusCh
		conn.failAll(child.endErr())
		conn.closeWriter()
	}()
	return client, nil
}
