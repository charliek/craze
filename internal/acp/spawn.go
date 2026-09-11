package acp

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

const shutdownGrace = 2 * time.Second

type SpawnOptions struct {
	Binary string
	Args   []string
	Dir    string
	Env    []string
	Stderr io.Writer
}

func ResolveBinary(explicit string) (string, error) {
	var candidates []string
	if explicit != "" {
		candidates = append(candidates, explicit)
	} else if env := os.Getenv("CRAZE_AGENT_BIN"); env != "" {
		candidates = append(candidates, env)
	} else {
		candidates = append(candidates, "cursor-agent", "agent")
	}
	var last error
	for _, name := range candidates {
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
	cmd        *exec.Cmd
	pgid       int
	waitCh     chan struct{}
	waitErr    error
	stderrDone chan struct{}
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

func (ch *Child) Shutdown() {
	if ch == nil || ch.cmd == nil || ch.cmd.Process == nil {
		return
	}
	_ = signalGroup(ch.pgid, ch.cmd.Process, syscall.SIGTERM)
	select {
	case <-ch.waitCh:
		return
	case <-time.After(shutdownGrace):
		_ = signalGroup(ch.pgid, ch.cmd.Process, syscall.SIGKILL)
		<-ch.waitCh
	}
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
	bin, err := ResolveBinary(opts.Binary)
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
