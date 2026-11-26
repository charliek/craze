package acp

import (
	"io"
	"time"

	"github.com/charliek/craze/internal/agentproc"
)

// ErrAgentExited is Client.Close's answer when the non-blocking probe of
// child.Exited() found it already closed. Aliased to agentproc.ErrAgentExited
// so all process-backed transports share one sentinel.
var ErrAgentExited = agentproc.ErrAgentExited

// ExitError is how the agent ended — non-zero, or on a signal — as the reaper
// observed it before reaping it. Aliased to agentproc.ExitError so errors.As
// matches identically across ACP and Codex transports.
type ExitError = agentproc.ExitError

// TreeSnapshot is the agent's descendants at one moment.
type TreeSnapshot = agentproc.TreeSnapshot

// StderrWaitEnv and StderrWait re-export agentproc's stderr drain test hook.
const StderrWaitEnv = agentproc.StderrWaitEnv

var StderrWait = agentproc.StderrWait

type SpawnOptions struct {
	Binary     string
	Candidates []string
	Args       []string
	Dir        string
	Env        []string
	Stderr     io.Writer
	Dialect    DialectID
}

func ResolveBinary(explicit string) (string, error) {
	return agentproc.ResolveBinaryCandidates(explicit, []string{"cursor-agent", "agent"})
}

func ResolveBinaryCandidates(explicit string, candidates []string) (string, error) {
	return agentproc.ResolveBinaryCandidates(explicit, candidates)
}

func LookupBinary(explicit string, candidates []string) (string, error) {
	return agentproc.LookupBinary(explicit, candidates)
}

// StderrDropped is how many chunks of the agent's stderr claude's queued sink
// dropped because its sink was behind; 0 for every other dialect.
func (c *Client) StderrDropped() int64 {
	return c.child.StderrDropped()
}

// StderrTail is the last 2 KiB of what the agent wrote to its stderr, raw.
func (c *Client) StderrTail(wait time.Duration) []byte {
	return c.child.StderrTail(wait)
}

func Spawn(opts SpawnOptions) (*Client, error) {
	child, stdout, stdin, err := agentproc.Start(agentproc.StartOptions{
		Binary:      opts.Binary,
		Candidates:  opts.Candidates,
		Args:        opts.Args,
		Dir:         opts.Dir,
		Env:         opts.Env,
		Stderr:      opts.Stderr,
		AsyncStderr: opts.Dialect == DialectClaude,
	})
	if err != nil {
		return nil, err
	}

	conn := NewConn(stdout, stdin)
	conn.ended = func(def error) error {
		return child.EndedWith("acp", ErrClosed, def)
	}
	client := newClient(conn, child)
	if opts.Dialect != "" {
		client.dialect = opts.Dialect
	}
	conn.Start()
	go func() {
		<-child.StatusReady()
		conn.failAll(child.EndErr("acp", ErrClosed))
		conn.closeWriter()
	}()
	return client, nil
}
