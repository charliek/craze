package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/charliek/craze/internal/paths"
	"github.com/charliek/craze/internal/version"
)

func NewRootCmd() *cobra.Command {
	flags := &tuiFlags{force: true}
	var showVersion bool
	cmd := &cobra.Command{
		Use:           "craze",
		Short:         "An ACP client TUI for Cursor, Grok, and gx",
		Long:          "craze is a terminal UI that drives cursor-agent, grok, or gx (a third-party Grok CLI fork) over ACP.",
		SilenceUsage:  true,
		SilenceErrors: true,
		// cobra's own version flag answered before argument validation; keep that.
		Args: func(cmd *cobra.Command, args []string) error {
			if showVersion {
				return nil
			}
			return cobra.NoArgs(cmd, args)
		},
		// Every command, the root included, refuses to run while the removed
		// config-file variable is set (paths.CheckEnv), before it can read or
		// write anything. No subcommand may define a PersistentPreRunE of its
		// own: it would replace this one. --help never reaches a hook, and
		// --version is let through here; both touch no file, so they still
		// answer.
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			// Only the root's RunE honours --version, so only the root may skip
			// the check for it.
			if showVersion && cmd == cmd.Root() {
				return nil
			}
			if err := paths.CheckEnv(); err != nil {
				return usagef("craze: %v", err)
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			if showVersion {
				_, err := fmt.Fprintln(out, version.Version)
				return err
			}
			if f, ok := out.(*os.File); ok {
				st, err := f.Stat()
				if err == nil && st.Mode()&os.ModeCharDevice == 0 {
					return usagef("craze: refusing to start TUI on a non-tty")
				}
			} else if !stdoutIsTTY() {
				return usagef("craze: refusing to start TUI on a non-tty")
			}
			return runTUI(cmd, flags, processHostEnv())
		},
	}
	// craze owns --version instead of setting cobra's Version field, because
	// the bare output needs SetVersionTemplate, and any cobra Set*Template call
	// makes text/template's executor reachable. That executor looks methods up
	// by name, so the linker then keeps every exported method of every linked
	// type: 14 MB of openai-go alone (TestNoReflectiveMethodLookupLinked in cmd/craze).
	cmd.Flags().BoolVarP(&showVersion, "version", "v", false, "version for craze")
	registerTUIFlags(cmd, flags)
	cmd.AddCommand(newVersionCmd())
	cmd.AddCommand(newPromptCmd())
	cmd.AddCommand(newFrameCmd())
	cmd.AddCommand(newImportCmd())
	cmd.AddCommand(newBridgeCmd())
	return cmd
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the craze version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			_, err := fmt.Fprintln(cmd.OutOrStdout(), version.Version)
			return err
		},
	}
}

func Execute() {
	ranCmd, err := NewRootCmd().ExecuteC()
	if err == nil {
		return
	}
	line, code := diagnose(ranCmd, err)
	if line != "" {
		fmt.Fprintln(os.Stderr, line)
	}
	os.Exit(code)
}

// diagnose is what Execute prints (one line, or none) and the code it exits
// with, for ranCmd's error: craze bridge's own contract (bridgeLine, exit 1)
// for every error that reaches it once the ran command is bridge's — flag
// parsing, an extra argument, and the root's shared PersistentPreRunE (a
// stray removed config-file variable in an SSH environment) included, since bridge defines
// none of its own (root.go's own rule: no subcommand may) — or, for every
// other command, today's mapping: an exitError's own code and message, else
// exit 1 with the error printed as it is.
func diagnose(ranCmd *cobra.Command, err error) (line string, code int) {
	if ranCmd != nil && ranCmd.Name() == "bridge" {
		return bridgeLine(err), 1
	}
	var ee *exitError
	if errors.As(err, &ee) {
		return ee.msg, ee.code
	}
	return err.Error(), 1
}

// bridgeLine is diagnose's bridge-specific mapping: an exitError's own
// message — a stray removed config-file variable's "craze: ..." (usagef, exit 2) included,
// its leading "craze: " stripped — or, for a raw cobra error (an unknown
// flag, an extra argument), its own text; bridgePrefix is added once, never
// twice, since bridge.go's own errors already carry it.
func bridgeLine(err error) string {
	msg := err.Error()
	var ee *exitError
	if errors.As(err, &ee) {
		msg = ee.msg
	}
	msg = strings.TrimPrefix(msg, "craze: ")
	if strings.HasPrefix(msg, bridgePrefix) {
		return msg
	}
	return bridgePrefix + msg
}
