package acp

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

const shutdownGrace = 2 * time.Second

// ErrAgentExited is Client.Close's answer when the non-blocking probe of
// child.waitCh found it already closed: the agent's exit had been reaped
// before craze's first Close on it sampled it. It does not mean Close
// failed — match it with errors.Is, never a nil check, because a
// craze-initiated close still returns nil. There is a real window between
// the process exiting, cmd.Wait returning and the reaper closing waitCh, so
// an agent that crashes in the same microseconds as craze's own close is
// reported as craze-initiated (§9, accepted).
var ErrAgentExited = errors.New("acp: agent exited")

// agentExitedErr wraps ErrAgentExited with what the reaper recorded: the
// wait error when the agent exited non-zero or crashed, and "exit 0" when it
// exited clean — the one case Conn.Err() cannot otherwise tell apart from
// craze's own close (both read ErrClosed).
func agentExitedErr(waitErr error) error {
	if waitErr != nil {
		return fmt.Errorf("%w: %v", ErrAgentExited, waitErr)
	}
	return fmt.Errorf("%w: exit 0", ErrAgentExited)
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
	var names []string
	if explicit != "" {
		names = append(names, explicit)
	} else if env := os.Getenv("CRAZE_AGENT_BIN"); env != "" {
		names = append(names, env)
	} else {
		names = append(names, candidates...)
	}
	var last error
	for _, name := range names {
		path, err := exec.LookPath(name)
		if err == nil {
			return path, nil
		}
		last = err
		if filepath.IsAbs(name) {
			if _, statErr := os.Stat(name); statErr == nil {
				return name, nil
			}
		}
	}
	return "", fmt.Errorf("agent binary not found: %w", last)
}

type Child struct {
	cmd          *exec.Cmd
	pgid         int
	waitCh       chan struct{}
	waitErr      error
	stderrDone   chan struct{}
	shutdownOnce sync.Once
}

func (ch *Child) PID() int {
	if ch == nil || ch.cmd == nil || ch.cmd.Process == nil {
		return 0
	}
	return ch.cmd.Process.Pid
}

func (ch *Child) Wait() {
	if ch == nil {
		return
	}
	<-ch.waitCh
	if ch.stderrDone != nil {
		<-ch.stderrDone
	}
}

// Shutdown signals the agent's whole process group, once per child. The first
// call signals even when the agent itself has already exited: what it started
// — a tool's subprocess — is still in its group, and that is exactly what
// would otherwise be left running. A later call signals nothing, because by
// then the group may be gone and its id free for something else.
func (ch *Child) Shutdown() {
	if ch == nil || ch.cmd == nil || ch.cmd.Process == nil {
		return
	}
	ch.shutdownOnce.Do(func() {
		_ = signalGroup(ch.pgid, ch.cmd.Process, syscall.SIGTERM)
		select {
		case <-ch.waitCh:
			return
		case <-time.After(shutdownGrace):
			_ = signalGroup(ch.pgid, ch.cmd.Process, syscall.SIGKILL)
			<-ch.waitCh
		}
	})
}

func signalGroup(pgid int, proc *os.Process, sig syscall.Signal) error {
	if pgid > 0 {
		return syscall.Kill(-pgid, sig)
	}
	if proc != nil {
		return proc.Signal(sig)
	}
	return nil
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

	child := &Child{
		cmd:        cmd,
		pgid:       cmd.Process.Pid,
		waitCh:     make(chan struct{}),
		stderrDone: make(chan struct{}),
	}
	go func() {
		child.waitErr = cmd.Wait()
		close(child.waitCh)
	}()

	stderrDst := opts.Stderr
	if stderrDst == nil {
		stderrDst = io.Discard
	}
	go func() {
		_, _ = io.Copy(stderrDst, stderr)
		close(child.stderrDone)
	}()

	conn := NewConn(stdout, stdin)
	client := newClient(conn, child)
	if opts.Dialect != "" {
		client.dialect = opts.Dialect
	}
	conn.Start()
	go func() {
		<-child.waitCh
		err := child.waitErr
		if err == nil {
			err = ErrClosed
		} else {
			err = fmt.Errorf("acp: agent exited: %w", err)
		}
		conn.failAll(err)
	}()
	return client, nil
}
