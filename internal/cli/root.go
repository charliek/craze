package cli

import (
	"errors"
	"fmt"
	"os"

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
	if err := NewRootCmd().Execute(); err != nil {
		var ee *exitError
		code := 1
		if errors.As(err, &ee) {
			code = ee.code
			if ee.msg != "" {
				fmt.Fprintln(os.Stderr, ee.msg)
			}
			os.Exit(code)
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(code)
	}
}
