package cli

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/tui"
)

type frameOpts struct {
	cols        int
	rows        int
	agentBin    string
	fakeScript  string
	keys        string
	ansi        bool
	theme       string
	provider    string
	noForce     bool
	timeout     time.Duration
	printFrames bool
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
	cmd.Flags().BoolVar(&o.noForce, "no-force", false, "disable yolo and handle permission requests")
	cmd.Flags().DurationVar(&o.timeout, "timeout", 10*time.Second, "per-wait timeout")
	cmd.Flags().BoolVar(&o.printFrames, "print-frames", false, "stream every frame to stderr")
	registerProviderFlag(cmd, &o.provider)
	return cmd
}

func (o *frameOpts) run(cmd *cobra.Command) error {
	if o.cols <= 0 || o.rows <= 0 {
		return usagef("craze frame: --cols and --rows must be positive")
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
	prov := resolved.Provider
	sess := agent.New(agent.Options{
		Binary:      o.agentBin,
		Workspace:   ws,
		Force:       force,
		Stderr:      cmd.ErrOrStderr(),
		Interactive: true,
		Provider:    &prov,
	})

	plain, raw, err := tui.RunFrameScript(tui.Config{
		Session:        sess,
		Theme:          theme,
		Workspace:      ws,
		Yolo:           force,
		Provider:       prov,
		ProviderLocked: true,
	}, o.cols, o.rows, o.keys, tui.FrameOpts{
		Timeout:     o.timeout,
		ANSI:        o.ansi,
		PrintFrames: o.printFrames,
		Out:         cmd.ErrOrStderr(),
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
