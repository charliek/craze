package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/sessions"
	"github.com/charliek/craze/internal/tui"
)

type frameOpts struct {
	cols       int
	rows       int
	agentBin   string
	fakeScript string
	keys       string
	ansi       bool
	theme      string
	provider   string
	// pluginDirs are extra plugin roots, as on the root command. There is no
	// --workspace here, so a relative one is resolved against the cwd the
	// frame runs in — the same directory the session takes as its workspace.
	pluginDirs  []string
	noForce     bool
	timeout     time.Duration
	printFrames bool
	freeze      bool
	// cont and resume are the root command's --continue/--resume, so a golden
	// can be rendered of what they do; seedSessions writes the index rows
	// they resolve against, inside the runner's isolated HOME (§3.7).
	cont         bool
	resume       bool
	seedSessions []string
}

func newFrameCmd() *cobra.Command {
	o := &frameOpts{}
	cmd := &cobra.Command{
		Use:    "frame",
		Short:  "Render a headless frame from a key script",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return o.run(cmd)
		},
	}
	cmd.Flags().IntVar(&o.cols, "cols", 100, "terminal width")
	cmd.Flags().IntVar(&o.rows, "rows", 30, "terminal height")
	cmd.Flags().StringVar(&o.agentBin, "agent-bin", "", "path to cursor-agent / fake agent (or CRAZE_AGENT_BIN)")
	cmd.Flags().StringVar(&o.fakeScript, "fake-script", "", "CRAZE_FAKE_SCRIPT for the child agent")
	cmd.Flags().StringVar(&o.keys, "keys", "", "key script, e.g. \"go<enter><wait:text:TASKS>\"")
	cmd.Flags().BoolVar(&o.ansi, "ansi", false, "print the raw frame with a forced true-colour profile")
	cmd.Flags().StringVar(&o.theme, "theme", "", themeFlagUsage)
	registerPluginDirFlag(cmd, &o.pluginDirs)
	cmd.Flags().BoolVar(&o.noForce, "no-force", false, "disable yolo and handle permission requests")
	cmd.Flags().DurationVar(&o.timeout, "timeout", 10*time.Second, "per-wait timeout")
	cmd.Flags().BoolVar(&o.printFrames, "print-frames", false, "stream every frame to stderr")
	cmd.Flags().BoolVar(&o.freeze, "freeze", false, "stop the clock and the spinner cycle, so a frame of a turn in progress does not depend on wall time")
	cmd.Flags().BoolVar(&o.cont, "continue", false, "load the newest seeded session instead of starting a new one")
	cmd.Flags().BoolVar(&o.resume, "resume", false, "open the resume picker over the seeded sessions")
	cmd.Flags().StringArrayVar(&o.seedSessions, "seed-session", nil,
		"seed one index row as provider:id:title (repeatable; the last one is the newest)")
	registerProviderFlag(cmd, &o.provider)
	return cmd
}

func (o *frameOpts) run(cmd *cobra.Command) error {
	if o.cols <= 0 || o.rows <= 0 {
		return usagef("craze frame: --cols and --rows must be positive")
	}
	if o.cont && o.resume {
		return usagef("craze frame: --continue and --resume are mutually exclusive")
	}
	ws, err := resolveWorkspace("")
	if err != nil {
		return err
	}
	force := !o.noForce
	if o.fakeScript != "" {
		// Set it on this process rather than snapshotting an env here: the
		// child inherits the environment as it is when RunFrameScript spawns
		// it, which is what carries the isolated HOME through to the agent.
		if err := os.Setenv("CRAZE_FAKE_SCRIPT", o.fakeScript); err != nil {
			return err
		}
	}
	theme := o.theme
	if theme == "" {
		// The frame runner is hermetic on purpose: it never reads the config
		// file, so a golden cannot depend on the developer's saved theme.
		theme = tui.DefaultTheme
	}
	resolved, err := resolveProvider(cmd, o.provider, cmd.ErrOrStderr(), true)
	if err != nil {
		return err
	}
	if !o.cont && !o.resume {
		// As on the root command: a load starts the seeded row's provider,
		// not the resolved one, so a load is checked against the row's once
		// it is known (seedAndResolve, plan 028 §3.5). The frame runner has
		// no --ask/--plan.
		if err := refuseInProcess("craze frame", resolved.Provider, o.agentBin, ""); err != nil {
			return err
		}
	}
	prov := resolved.Provider
	// JournalDir stays empty here and in seedAndResolve's build: the frame
	// runner is hermetic, and journalDir is never asked (plan 020 §3.5).
	sess := agent.New(agent.Options{
		Binary:      o.agentBin,
		Workspace:   ws,
		Force:       force,
		Stderr:      cmd.ErrOrStderr(),
		PluginDirs:  o.pluginDirs,
		Interactive: true,
		Provider:    &prov,
		// Spelled out rather than left implicit: the frame runner never reads
		// config.toml (the theme, above), so every [compat.claude] toggle is
		// at its default here and a golden cannot depend on a table in the
		// developer's own config (plan 022 §3.5).
		Compat: agent.ClaudeCompat{},
	})

	base := tui.Config{
		Session:        sess,
		Theme:          theme,
		Workspace:      ws,
		Yolo:           force,
		Provider:       prov,
		ProviderLocked: true,
		// TerminalTitle is deliberately left false: the frame runner builds
		// its program with tea.WithoutRenderer(), so tea.SetWindowTitle is a
		// no-op here regardless, and a golden must never depend on it (§3.10).
	}
	var setup func() (tui.Config, error)
	if o.cont || o.resume || len(o.seedSessions) > 0 {
		// The index lives in the craze directory (CRAZE_HOME, else under
		// HOME), and the runner only isolates it — HOME swapped, CRAZE_HOME
		// unset — after this function has handed it a Config. So the seeding
		// and the row lookup have to happen in a callback it runs inside the
		// isolation, or a golden would read (and write) the developer's own
		// index (§3.7).
		setup = func() (tui.Config, error) { return o.seedAndResolve(cmd, base, ws, force) }
	}

	plain, raw, err := tui.RunFrameScript(base, o.cols, o.rows, o.keys, tui.FrameOpts{
		Timeout:     o.timeout,
		ANSI:        o.ansi,
		PrintFrames: o.printFrames,
		Out:         cmd.ErrOrStderr(),
		Freeze:      o.freeze,
		Setup:       setup,
	})
	if err != nil {
		return frameExitError(cmd, err)
	}
	out := plain
	if o.ansi {
		out = raw
	}
	_, err = fmt.Fprintln(cmd.OutOrStdout(), out)
	return err
}

// seedAndResolve writes the --seed-session rows and answers --continue /
// --resume out of them. It runs inside the frame runner's isolated HOME, so
// the store it builds is the isolated one and the rows are gone with the
// directory when the run ends.
func (o *frameOpts) seedAndResolve(cmd *cobra.Command, base tui.Config, ws string, force bool) (tui.Config, error) {
	cwd := ws
	if abs, err := filepath.Abs(ws); err == nil {
		cwd = abs
	}
	index := &sessions.Store{KnownProvider: knownProvider}
	for _, spec := range o.seedSessions {
		row, err := parseSeedSession(spec, cwd)
		if err != nil {
			return base, err
		}
		if err := index.Upsert(row); err != nil {
			return base, err
		}
	}
	cfg := base
	cfg.SessionIndex = index
	// The seeded row's provider is held to the spawn flags a new session's is,
	// as the root command's loads are (resolveLoad, plan 028 §3.5).
	refuseLoad := func(p agent.Provider) error { return refuseInProcess("craze frame", p, o.agentBin, "") }
	build := func(p agent.Provider, row sessions.Row) agent.Session {
		prov := p
		return agent.New(agent.Options{
			Binary:      o.agentBin,
			Workspace:   ws,
			Force:       force,
			Stderr:      cmd.ErrOrStderr(),
			PluginDirs:  o.pluginDirs,
			Interactive: true,
			Provider:    &prov,
			// The default, for the reason the session above spells it out.
			Compat:        agent.ClaudeCompat{},
			LoadSessionID: row.SessionID,
			Title:         row.Title,
			TitlePinned:   row.Pinned,
		})
	}
	switch {
	case o.cont:
		row, ok, err := index.Latest(cwd, "")
		if err != nil {
			return cfg, err
		}
		if !ok {
			return cfg, usagef("%s", noSessionMsg(cwd, ""))
		}
		p, err := agent.ProviderByName(row.Provider)
		if err != nil {
			return cfg, err
		}
		if err := refuseLoad(p); err != nil {
			return cfg, err
		}
		cfg.Provider = p
		cfg.Session = build(p, row)
		cfg.Loading = true
		// The same carry as runTUI's --continue: the row's durable craze id
		// belongs to the session built from it (session control SD-22).
		cfg.CrazeSessionID = row.CrazeID
	case o.resume:
		rows, err := index.Recent(cwd, "", resumeRowLimit)
		if err != nil {
			return cfg, err
		}
		if len(rows) == 0 {
			return cfg, usagef("%s", noSessionMsg(cwd, ""))
		}
		// The picker builds the session itself, once a row is chosen; the one
		// assembled before the runner started is not the one to start.
		cfg.Session = nil
		cfg.Resume = rows
		cfg.LoadSession = build
		cfg.RefuseLoad = refuseLoad
	}
	return cfg, nil
}

// parseSeedSession reads provider:id:title. SplitN with a limit of 3 so a
// title may hold a colon — which real ones do, and a session called
// "fix: the flaky test" is exactly the row a picker golden wants.
func parseSeedSession(spec, cwd string) (sessions.Row, error) {
	parts := strings.SplitN(spec, ":", 3)
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" {
		return sessions.Row{}, usagef("craze frame: --seed-session wants provider:id:title, got %q", spec)
	}
	return sessions.Row{
		SessionID: parts[1],
		Provider:  parts[0],
		CWD:       cwd,
		Title:     parts[2],
		// An agent title: it fills the row without pinning it, which is what
		// a seeded session looks like before anyone renames it.
		TitleKind: sessions.TitleKindAgent,
	}, nil
}

// frameExitError maps a script failure onto craze's exit codes: 2 for a bad
// script, 3 for a wait that timed out (with the last frame as diagnostics).
func frameExitError(cmd *cobra.Command, err error) error {
	var se *tui.ScriptError
	if errors.As(err, &se) {
		return usagef("craze frame: %v", err)
	}
	var te *tui.WaitTimeoutError
	if errors.As(err, &te) {
		fmt.Fprintf(cmd.ErrOrStderr(), "craze frame: %v\nlast frame:\n%s\n", te, te.LastFrame)
		return exitf(3, "")
	}
	return err
}
