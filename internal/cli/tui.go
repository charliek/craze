package cli

import (
	"os"

	"github.com/spf13/cobra"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/tui"
)

type tuiFlags struct {
	theme     string
	workspace string
	model     string
	agentBin  string
	force     bool
	noForce   bool
	ask       bool
	plan      bool
}

func registerTUIFlags(cmd *cobra.Command, f *tuiFlags) {
	cmd.Flags().StringVar(&f.theme, "theme", "tokyo-night", "TUI theme: tokyo-night, dark, or light")
	cmd.Flags().StringVar(&f.workspace, "workspace", "", "existing workspace directory (default: current directory)")
	cmd.Flags().StringVar(&f.model, "model", "", "ACP model id")
	cmd.Flags().StringVar(&f.agentBin, "agent-bin", "", "path to cursor-agent / fake agent (or CRAZE_AGENT_BIN)")
	cmd.Flags().BoolVar(&f.force, "force", true, "spawn the agent with --force (yolo)")
	cmd.Flags().BoolVar(&f.noForce, "no-force", false, "disable yolo and handle permission requests")
	cmd.Flags().BoolVar(&f.ask, "ask", false, "set session mode to ask after session/new")
	cmd.Flags().BoolVar(&f.plan, "plan", false, "set session mode to plan after session/new")
}

func runTUI(f *tuiFlags) error {
	if f.ask && f.plan {
		return usagef("craze: --ask and --plan are mutually exclusive")
	}
	if f.noForce {
		f.force = false
	}
	ws, err := resolveWorkspace(f.workspace)
	if err != nil {
		return err
	}
	mode := ""
	switch {
	case f.ask:
		mode = "ask"
	case f.plan:
		mode = "plan"
	}
	sess := agent.New(agent.Options{
		Binary:    f.agentBin,
		Workspace: ws,
		Force:     f.force,
		Model:     f.model,
		Mode:      mode,
		Stderr:    os.Stderr,
	})
	return tui.Run(tui.Config{
		Session:   sess,
		Theme:     f.theme,
		Workspace: ws,
		Model:     f.model,
		Yolo:      f.force,
	})
}

func stdoutIsTTY() bool {
	st, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return st.Mode()&os.ModeCharDevice != 0
}
