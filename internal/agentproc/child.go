package agentproc

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
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// ShutdownGrace is how long the agent — and then, once it has exited, what is
// left in its process group — has between SIGTERM and SIGKILL, and how long
// the reaper waits after a group SIGKILL for the group to empty (reaper.go).
const ShutdownGrace = 2 * time.Second

// groupPoll is how often the reaper looks for the agent's process group to
// have no live member left after a signal.
const groupPoll = 20 * time.Millisecond

// ErrAgentExited is Close's answer when the non-blocking probe of
// child.Exited() found it already closed: the reaper had observed the agent's
// exit before craze's first Close on it sampled it. Match it with errors.Is,
// never a nil check, because a craze-initiated close still returns nil.
var ErrAgentExited = errors.New("agent exited")

// AgentExitedErr wraps ErrAgentExited with how the reaper recorded the agent
// end (Child.ExitErr): an *ExitError when it exited non-zero or on a signal,
// and "exit 0" when it exited clean.
func AgentExitedErr(exitErr error) error {
	return WrapAgentExited(ErrAgentExited, exitErr)
}

// WrapAgentExited wraps sentinel (typically a transport-prefixed error wrapping
// ErrAgentExited) with how the reaper recorded the agent end.
func WrapAgentExited(sentinel, exitErr error) error {
	if exitErr != nil {
		return fmt.Errorf("%w: %v", sentinel, exitErr)
	}
	return fmt.Errorf("%w: exit 0", sentinel)
}

// ExitError is how the agent ended — non-zero, or on a signal — as the reaper
// observed it before reaping it (plan 032 §3.2a): the status the kernel
// reported for the exit (waitid's siginfo on Linux, the NOTE_EXIT kevent's
// data on macOS). Its message matches os.ProcessState's ("exit status 3",
// "signal: killed").
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

// ExitErrorOf is how an observed wait status reads as an error: nil for an
// exit 0, an *ExitError for anything else.
func ExitErrorOf(st syscall.WaitStatus) error {
	if st.Exited() && st.ExitStatus() == 0 {
		return nil
	}
	return &ExitError{Status: st}
}

// ResolveBinaryCandidates resolves explicit, then CRAZE_AGENT_BIN, then the
// candidate list. An explicit binary is returned as-is after the PATH and
// absolute-path checks; candidates are PATH lookups only.
func ResolveBinaryCandidates(explicit string, candidates []string) (string, error) {
	if explicit == "" {
		explicit = os.Getenv("CRAZE_AGENT_BIN")
	}
	return LookupBinary(explicit, candidates)
}

// LookupBinary is ResolveBinaryCandidates without CRAZE_AGENT_BIN: explicit
// when it is set, else the candidate list, by the same checks.
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

// StartOptions configures spawning a managed agent child process.
type StartOptions struct {
	Binary     string
	Candidates []string
	Args       []string
	Dir        string
	Env        []string
	Stderr     io.Writer
	// AsyncStderr places a bounded non-blocking queue in front of Stderr so a
	// blocked sink cannot stall draining the child's stderr pipe (used by
	// claude's overage stop).
	AsyncStderr bool
	// OwnedStdout allocates an explicit os.Pipe for stdout instead of using
	// exec.Cmd.StdoutPipe so the caller owns the read end and cmd.Wait never
	// closes stdout before the caller finishes draining buffered replies.
	OwnedStdout bool
}

// Child is the agent process Start spawned, the leader of a process group of
// its own (Setpgid: the group's id is the agent's pid), and its reaper
// (reaper.go), the one goroutine that waits for it and signals it. It goes
// through three states: running; exited — the exit observed but the agent
// not reaped, so the zombie keeps its pid, and with it the group's id, from
// being reused (exitedCh); and reaped (reapedCh).
type Child struct {
	cmd             *exec.Cmd
	pid             int
	pgid            int
	exitedCh        chan struct{}
	statusCh        chan struct{}
	exitErr         error
	endedByShutdown bool
	reapedCh        chan struct{}
	waitErr         error
	shutdownCh      chan struct{}
	shutdownOnce    sync.Once
	killNow         atomic.Bool
	reapGate
	stderrDone chan struct{}
	stderrTail *stderrTail
	stderrSink *asyncSink
}

// Start resolves the binary, spawns the child in its own process group, starts
// its reaper and stderr copy, and returns the child along with its stdout and
// stdin pipes.
func Start(opts StartOptions) (*Child, io.ReadCloser, io.WriteCloser, error) {
	bin, err := ResolveBinaryCandidates(opts.Binary, opts.Candidates)
	if err != nil {
		return nil, nil, nil, err
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
		return nil, nil, nil, err
	}
	var (
		stdout      io.ReadCloser
		stdoutWrite *os.File
	)
	if opts.OwnedStdout {
		r, w, pipeErr := os.Pipe()
		if pipeErr != nil {
			return nil, nil, nil, pipeErr
		}
		cmd.Stdout = w
		stdout = r
		stdoutWrite = w
	} else {
		stdout, err = cmd.StdoutPipe()
		if err != nil {
			return nil, nil, nil, err
		}
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		if stdoutWrite != nil {
			_ = stdoutWrite.Close()
			_ = stdout.Close()
		}
		return nil, nil, nil, err
	}
	if err := cmd.Start(); err != nil {
		if stdoutWrite != nil {
			_ = stdoutWrite.Close()
			_ = stdout.Close()
		}
		return nil, nil, nil, fmt.Errorf("start agent: %w", err)
	}
	if stdoutWrite != nil {
		_ = stdoutWrite.Close()
	}

	child := newChild(cmd)

	stderrDst := opts.Stderr
	if stderrDst == nil {
		stderrDst = io.Discard
	}
	if !opts.AsyncStderr {
		child.stderrTail = newStderrTail(stderrDst)
		go func() {
			_, _ = io.Copy(child.stderrTail, stderr)
			close(child.stderrDone)
		}()
	} else {
		sink := newAsyncSink(stderrDst)
		child.stderrSink = sink
		child.stderrTail = newStderrTail(sink)
		go func() {
			_, _ = io.Copy(child.stderrTail, stderr)
			sink.closeWithin(stderrWaitBound)
			close(child.stderrDone)
		}()
	}

	return child, stdout, stdin, nil
}

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
	observe, err := watch(ch.pid)
	if err != nil {
		go ch.reapPolling()
	} else {
		go ch.reap(observe)
	}
	return ch
}

// EndErr is what the calls still pending fail with once the agent has ended
// and its status is known (StatusReady): closedErr when craze had asked for the
// end (Shutdown) by the time the exit was observed, or the agent exited 0 — and
// otherwise "<prefix>: agent exited: …" with how it ended.
func (ch *Child) EndErr(prefix string, closedErr error) error {
	if ch == nil || ch.exitErr == nil || ch.endedByShutdown {
		return closedErr
	}
	return fmt.Errorf("%s: agent exited: %w", prefix, ch.exitErr)
}

// exitAfterEOF bounds EndedWith's wait for the agent's exit.
const exitAfterEOF = time.Second

// EndedWith is what pending calls fail with when the agent's stdout ends.
func (ch *Child) EndedWith(prefix string, closedErr, def error) error {
	if ch == nil {
		return def
	}
	exit := time.NewTimer(exitAfterEOF)
	defer exit.Stop()
	select {
	case <-ch.exitedCh:
	case <-exit.C:
		return def
	}
	<-ch.statusCh
	return ch.EndErr(prefix, closedErr)
}

// UnexpectedEndErr returns closedErr when the child ended due to an intentional
// Shutdown, sentinel when the child exited 0 unexpectedly, and
// WrapAgentExited(sentinel, exitErr) when the child exited non-zero so callers
// match both sentinel (via errors.Is) and *ExitError (via errors.As).
func (ch *Child) UnexpectedEndErr(sentinel, closedErr error) error {
	if ch == nil || ch.endedByShutdown {
		return closedErr
	}
	if ch.exitErr == nil {
		return fmt.Errorf("%w: exit 0", sentinel)
	}
	return fmt.Errorf("%w: %w", sentinel, ch.exitErr)
}

// EndedWithUnexpected waits briefly for the child's exit status after stdout
// EOF and classifies the termination via UnexpectedEndErr.
func (ch *Child) EndedWithUnexpected(sentinel, closedErr, def error) error {
	if ch == nil {
		return def
	}
	exit := time.NewTimer(exitAfterEOF)
	defer exit.Stop()
	select {
	case <-ch.exitedCh:
	case <-exit.C:
		return def
	}
	<-ch.statusCh
	return ch.UnexpectedEndErr(sentinel, closedErr)
}

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

// PGID is the process group id the child leads (equal to its pid), 0 for nil.
func (ch *Child) PGID() int {
	if ch == nil {
		return 0
	}
	return ch.pgid
}

// Exited is closed once the reaper has observed the agent's exit, without
// reaping it.
func (ch *Child) Exited() <-chan struct{} {
	if ch == nil {
		return nil
	}
	return ch.exitedCh
}

// StatusReady is closed once ExitErr and endedByShutdown are settled.
func (ch *Child) StatusReady() <-chan struct{} {
	if ch == nil {
		return nil
	}
	return ch.statusCh
}

// Reaped is closed once the reaper has reaped the agent and will send no more
// signals.
func (ch *Child) Reaped() <-chan struct{} {
	if ch == nil {
		return nil
	}
	return ch.reapedCh
}

// StderrDone is closed once the stderr copy goroutine has finished.
func (ch *Child) StderrDone() <-chan struct{} {
	if ch == nil {
		return nil
	}
	return ch.stderrDone
}

// ShutdownRequested is closed once Shutdown or Kill has been called.
func (ch *Child) ShutdownRequested() <-chan struct{} {
	if ch == nil {
		return nil
	}
	return ch.shutdownCh
}

// ExitErr returns how the agent ended (*ExitError or nil for exit 0). Only safe
// to call after StatusReady() is closed.
func (ch *Child) ExitErr() error {
	if ch == nil {
		return nil
	}
	return ch.exitErr
}

// WaitErr returns cmd.Wait's answer. Only safe to call after Reaped() is closed.
func (ch *Child) WaitErr() error {
	if ch == nil {
		return nil
	}
	return ch.waitErr
}

// Wait waits for the agent to have been reaped and its stderr copied to the
// end — the copy at most stderrWaitBound.
func (ch *Child) Wait() {
	if ch == nil {
		return
	}
	<-ch.reapedCh
	if ch.stderrDone != nil {
		t := time.NewTimer(stderrWaitBound)
		defer t.Stop()
		select {
		case <-ch.stderrDone:
		case <-t.C:
		}
	}
}

// stderrWaitBound bounds Wait's wait for the stderr copy, a var for tests.
var stderrWaitBound = 2 * time.Second

// Shutdown asks the reaper to end the agent and waits until it has been reaped.
func (ch *Child) Shutdown() {
	if ch == nil || ch.cmd == nil || ch.cmd.Process == nil {
		return
	}
	ch.shutdownOnce.Do(func() { close(ch.shutdownCh) })
	<-ch.reapedCh
}

// Kill is Shutdown with no grace: SIGKILL from the first.
func (ch *Child) Kill() {
	if ch == nil || ch.cmd == nil || ch.cmd.Process == nil {
		return
	}
	ch.killNow.Store(true)
	ch.Shutdown()
}

func (ch *Child) termSignal() syscall.Signal {
	if ch.killNow.Load() {
		return syscall.SIGKILL
	}
	return syscall.SIGTERM
}

// KillProcess sends SIGKILL directly to the child process (test helper for
// simulating abrupt agent crash).
func (ch *Child) KillProcess() error {
	if ch == nil || ch.cmd == nil || ch.cmd.Process == nil {
		return errors.New("agentproc: no child process")
	}
	return ch.cmd.Process.Kill()
}

// SetWatchForTest replaces the exit-observation hook for tests in sibling
// packages (such as internal/acp) and returns a restore function. Panics
// outside go test.
func SetWatchForTest(fn func(pid int) (func() (syscall.WaitStatus, bool, error), error)) func() {
	if !testing.Testing() {
		panic("agentproc.SetWatchForTest called outside test")
	}
	prev := watch
	if fn != nil {
		watch = fn
	}
	return func() { watch = prev }
}

// WatchExitDefault is the platform's default exit watcher, for tests wrapping
// watch via SetWatchForTest.
func WatchExitDefault(pid int) (func() (syscall.WaitStatus, bool, error), error) {
	if !testing.Testing() {
		panic("agentproc.WatchExitDefault called outside test")
	}
	return watchExit(pid)
}

// SetSendSignalForTest replaces the reaper's kill(2) hook for tests in sibling
// packages and returns a restore function. Panics outside go test.
func SetSendSignalForTest(fn func(pid int, sig syscall.Signal) error) func() {
	if !testing.Testing() {
		panic("agentproc.SetSendSignalForTest called outside test")
	}
	prev := sendSignal
	if fn != nil {
		sendSignal = fn
	}
	return func() { sendSignal = prev }
}

// SetStderrWaitBoundForTest overrides stderrWaitBound for tests in sibling
// packages and returns a restore function. Panics outside go test.
func SetStderrWaitBoundForTest(d time.Duration) func() {
	if !testing.Testing() {
		panic("agentproc.SetStderrWaitBoundForTest called outside test")
	}
	prev := stderrWaitBound
	stderrWaitBound = d
	return func() { stderrWaitBound = prev }
}
