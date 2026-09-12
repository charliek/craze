package cli

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

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
	noMouse   bool
	ask       bool
	plan      bool
}

func registerTUIFlags(cmd *cobra.Command, f *tuiFlags) {
	// The default is empty so Changed("theme") can tell an explicit --theme
	// from an unset one, which is what the config file loses to.
	cmd.Flags().StringVar(&f.theme, "theme", "", themeFlagUsage)
	cmd.Flags().StringVar(&f.workspace, "workspace", "", "existing workspace directory (default: current directory)")
	cmd.Flags().StringVar(&f.model, "model", "", "ACP model id")
	cmd.Flags().StringVar(&f.agentBin, "agent-bin", "", "path to cursor-agent / fake agent (or CRAZE_AGENT_BIN)")
	cmd.Flags().BoolVar(&f.force, "force", true, "spawn the agent with --force (yolo)")
	cmd.Flags().BoolVar(&f.noForce, "no-force", false, "disable yolo and handle permission requests")
	cmd.Flags().BoolVar(&f.noMouse, "no-mouse", false, "disable mouse reporting (wheel scroll and clicks)")
	cmd.Flags().BoolVar(&f.ask, "ask", false, "set session mode to ask after session/new")
	cmd.Flags().BoolVar(&f.plan, "plan", false, "set session mode to plan after session/new")
}

func runTUI(cmd *cobra.Command, f *tuiFlags) error {
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
	// The TUI owns the alt screen for the whole run, so nothing else may write
	// to the terminal: a diagnostic from cursor-agent lands on top of a frame,
	// takes none of the renderer's locks, and would garble it. The agent's
	// stderr and craze's own warnings are held here and printed once the screen
	// is back.
	diag := &deferredStderr{}
	sess := agent.New(agent.Options{
		Binary:      f.agentBin,
		Workspace:   ws,
		Force:       f.force,
		Model:       f.model,
		Mode:        mode,
		Stderr:      diag,
		Interactive: true,
	})
	err = tui.Run(tui.Config{
		Session:   sess,
		Theme:     resolveTheme(cmd, f.theme),
		Workspace: ws,
		Model:     f.model,
		Yolo:      f.force,
		NoMouse:   f.noMouse,
		Diag:      diag,
	})
	diag.flush(os.Stderr)
	return err
}

// deferredStderr holds what the agent said until the terminal is craze's to
// write on again. internal/acp copies the child's stderr from a goroutine of
// its own, so the buffer is guarded.
type deferredStderr struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	dropped int
}

// deferredStderrMax is how much diagnostic craze will hold. An agent looping on
// stderr for an hour must not grow the buffer without bound; past the cap the
// tail is dropped and counted, because the first thing it said is the thing
// worth reading.
const deferredStderrMax = 256 << 10

func (d *deferredStderr) Write(p []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if room := deferredStderrMax - d.buf.Len(); room < len(p) {
		d.dropped += len(p) - max(room, 0)
		if room <= 0 {
			return len(p), nil
		}
		p = p[:room]
	}
	return d.buf.Write(p)
}

// flush prints the diagnostics to the real stderr. It is called after Run has
// returned, which is after bubbletea has left the alt screen.
func (d *deferredStderr) flush(w io.Writer) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.buf.Len() > 0 {
		_, _ = w.Write(d.buf.Bytes())
		d.buf.Reset()
	}
	if d.dropped > 0 {
		_, _ = fmt.Fprintf(w, "craze: dropped %d further bytes of agent diagnostics\n", d.dropped)
		d.dropped = 0
	}
}

// themeFlagUsage names the presets once, for both commands that take --theme.
var themeFlagUsage = "TUI theme: " + strings.Join(tui.ThemeNames(), ", ") +
	" (default: ~/.craze/config.toml, else " + tui.DefaultTheme + ")"

// resolveTheme is the pinned precedence: an explicitly passed --theme wins,
// then the config file, then the default preset. "Explicitly passed" is
// Changed, not a non-empty value, so --theme "" is still a choice.
func resolveTheme(cmd *cobra.Command, flag string) string {
	if cmd != nil && cmd.Flags().Changed("theme") {
		return flag
	}
	if name := tui.ConfigTheme(); name != "" {
		return name
	}
	return tui.DefaultTheme
}

func stdoutIsTTY() bool {
	st, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return st.Mode()&os.ModeCharDevice != 0
}
