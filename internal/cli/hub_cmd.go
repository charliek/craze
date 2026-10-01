package cli

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/hub"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/rundir"
	"github.com/charliek/craze/internal/transcript"
)

// craze hub (plan 032 §3.5): the per-machine hub, internal/hub's Run. Hidden:
// hub.Ensure starts it on demand — re-executed, in a session of its own,
// stdio on /dev/null, its ready pipe on fd 3 — and it exits when idle. Run by
// hand it is the same hub in the foreground, saying what it does on stderr
// until its log is open, and answering no ready line.
func newHubCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "hub",
		Short:  "Run the per-machine session hub (started on demand)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// First: the ready pipe is close-on-exec, and the spawner's marks
			// leave the environment, before anything could start a process.
			ready, err := hub.TakeReadyPipe()
			if err != nil {
				return usagef("craze hub: %v", err)
			}
			sigs, stop := serveSignals()
			defer stop()
			return runHub(cmd, ready, sigs)
		},
	}
}

// runHub runs the hub over the process's environment: its log at
// hub.LogPath, capped like a host's (openHubLog), its grace from
// CRAZE_HUB_IDLE. A hub that lost the namespace's lock to another returns nil
// (exit 0); one that could not come up is exit 1.
func runHub(cmd *cobra.Command, ready *hub.ReadyPipe, sigs <-chan os.Signal) error {
	stderr := errWriter(cmd)
	grace, why := hub.IdleGraceFromEnv(os.Getenv)
	if why != "" {
		fmt.Fprintln(stderr, "craze hub:", why)
	}
	env := rundir.ProcessEnv()
	err := hub.Run(context.Background(), hub.Options{
		Env:       env,
		Codecs:    protocol.Codecs{Event: agent.EventCodecVersion, Snapshot: transcript.SnapshotVersion},
		Ready:     ready,
		Signals:   sigs,
		Stderr:    stderr,
		OpenLog:   func(path string) (io.Writer, func(), error) { return openHubLog(env, path) },
		IdleGrace: grace,
	})
	if err != nil {
		return exitf(1, "craze hub: %v", err)
	}
	return nil
}

// openHubLog opens the hub's log at path (hub.LogPath: hub-<ns>.log in the
// host logs' directory) capped like a host's log — hostLog, rotated once at
// hostLogMax — and truncated to empty as it opens when it is over that cap
// already. The hub opens it only once it holds its namespace's lock, so no
// other hub writes, rotates or truncates it meanwhile.
func openHubLog(env rundir.Env, path string) (io.Writer, func(), error) {
	l, err := openHostLog(env, path, hostLogMax)
	if err != nil {
		return nil, nil, err
	}
	if l.size > l.max {
		if err := l.f.Truncate(0); err != nil {
			_ = l.Close()
			return nil, nil, err
		}
		l.size = 0
	}
	return l, func() { _ = l.Close() }, nil
}
