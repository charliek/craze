package cli

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/charliek/craze/internal/version"
)

func NewRootCmd() *cobra.Command {
	flags := &tuiFlags{force: true, theme: "tokyo-night"}
	cmd := &cobra.Command{
		Use:           "craze",
		Short:         "A Cursor ACP client TUI",
		Long:          "craze is a terminal UI that drives cursor-agent over ACP.",
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       version.Version,
		Args:          cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			if f, ok := out.(*os.File); ok {
				st, err := f.Stat()
				if err == nil && st.Mode()&os.ModeCharDevice == 0 {
					return usagef("craze: refusing to start TUI on a non-tty")
				}
			} else if !stdoutIsTTY() {
				return usagef("craze: refusing to start TUI on a non-tty")
			}
			return runTUI(flags)
		},
	}
	cmd.SetVersionTemplate("{{.Version}}\n")
	registerTUIFlags(cmd, flags)
	cmd.AddCommand(newVersionCmd())
	cmd.AddCommand(newPromptCmd())
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
