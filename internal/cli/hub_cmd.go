package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/hub"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/rundir"
	"github.com/charliek/craze/internal/sessions"
	"github.com/charliek/craze/internal/transcript"
	"github.com/charliek/craze/internal/tui"
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
		Args:   noArgs,
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
		Creates:   hubCreates(),
	})
	if err != nil {
		return exitf(1, "craze hub: %v", err)
	}
	return nil
}

// hubCreates is what the hub's session.create reads of this build (plan 032
// §3.10): the config file's provider, at each create — the default of a
// create that names none — and the providers craze can start, by the name
// --provider takes; and sessions.createOptions' answer (hubCreateOptions,
// plan 036 §3.4).
func hubCreates() *hub.Creates {
	return &hub.Creates{
		DefaultProvider: tui.ConfigProvider,
		KnownProvider: func(id string) bool {
			_, err := agent.ProviderByName(id)
			return err == nil
		},
		Options: hubCreateOptions,
	}
}

// hubCreateOptions is the hub's answer to sessions.createOptions (plan 036
// §3.4), computed afresh at each call, in the hub's process: the listed
// providers' availability by the check's hub column (hubAvailInputs — the
// binaries a host the hub creates would find, native and the login session
// the hub's own, its fix naming this pid), the default provider
// (hubDefaultProvider) and the recent directories (hubRecentDirs). Every
// file it reads is the process's own CRAZE_HOME's — config.toml, native's
// directory, the session index — which in production is the hub's namespace
// (hub.Options.Env is the same process environment's). logf is the hub's
// log.
func hubCreateOptions(logf func(string, ...any)) protocol.CreateOptionsResult {
	return protocol.CreateOptionsResult{
		Providers:       providerOptions(availability(hubAvailInputs(os.Getpid()), agent.Providers())),
		DefaultProvider: hubDefaultProvider(),
		RecentDirs:      hubRecentDirs(logf),
	}
}

// hubDefaultProvider is the provider a session.create that names none
// starts, for sessions.createOptions to say (plan 036 decision 8):
// config.toml's provider when it is set and names a provider craze knows,
// else "" (absent) — never cursor for an empty one, which
// agent.ProviderByName("") would answer.
func hubDefaultProvider() string {
	id := tui.ConfigProvider()
	if id == "" {
		return ""
	}
	if _, err := agent.ProviderByName(id); err != nil {
		return ""
	}
	return id
}

// hubRecentDirs is sessions.createOptions' recent directories (plan 036
// §3.4, decision 7): the session index's distinct workspaces that are still
// directories, newest first (sessions.Store.RecentDirs, read uncapped, so
// its own cap cannot count an entry this drops), absolute and within the
// wire's bound only — a relative one names no place a client can start in —
// then cut to protocol.CreateOptionsDirsMax, each time in UTC. Never nil: []
// for none, and for an index that cannot be read, which says why through
// logf.
func hubRecentDirs(logf func(string, ...any)) []protocol.RecentDir {
	out := []protocol.RecentDir{}
	dirs, err := (&sessions.Store{}).RecentDirs(0)
	if err != nil {
		logf("sessions.createOptions: the session index cannot be read (%v): no recent directories", err)
		return out
	}
	for _, d := range dirs {
		if !filepath.IsAbs(d.Dir) || utf8.RuneCountInString(d.Dir) > protocol.RecentDirMax {
			continue
		}
		out = append(out, protocol.RecentDir{Dir: d.Dir, UsedAt: d.UsedAt.UTC()})
		if len(out) == protocol.CreateOptionsDirsMax {
			break
		}
	}
	return out
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
	if err := l.TruncateIfOver(); err != nil {
		_ = l.Close()
		return nil, nil, err
	}
	return l, func() { _ = l.Close() }, nil
}
