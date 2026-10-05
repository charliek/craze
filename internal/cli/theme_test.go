package cli

import (
	"errors"
	"io"
	"testing"

	"github.com/spf13/cobra"

	"github.com/charliek/craze/internal/tui"
)

// themeFor parses argv the way the root command does and returns the theme the
// TUI would be started with.
func themeFor(t *testing.T, args ...string) string {
	t.Helper()
	f := &tuiFlags{force: true}
	got := ""
	cmd := &cobra.Command{
		Use:  "craze",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			got = resolveTheme(cmd, f.theme)
			return nil
		},
	}
	registerTUIFlags(cmd, f)
	cmd.SetArgs(args)
	cmd.SetOut(io.Discard)
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	return got
}

func writeThemeConfig(t *testing.T, name string) {
	t.Helper()
	writeCrazeConfig(t, "theme = \""+name+"\"\n")
}

// TestThemePrecedence is the pinned order: an explicit --theme, then the config
// file, then the default preset. The flag's own default is empty precisely so
// Changed can tell "--theme craze-dark" from "no --theme at all".
func TestThemePrecedence(t *testing.T) {
	t.Run("flag beats config", func(t *testing.T) {
		writeThemeConfig(t, "gruvbox")
		if got := themeFor(t, "--theme", "light"); got != "light" {
			t.Fatalf("theme %q, want light", got)
		}
	})

	// The discriminating case for Changed over a non-empty check: an explicit
	// empty --theme is a choice, and it must not fall through to the config.
	t.Run("an explicit empty flag still beats config", func(t *testing.T) {
		writeThemeConfig(t, "gruvbox")
		if got := themeFor(t, "--theme", ""); got != "" {
			t.Fatalf("theme %q, want the explicitly empty flag", got)
		}
		if got := tui.Preset("").Name; got != tui.DefaultTheme {
			t.Fatalf("an empty theme should render as the default, got %q", got)
		}
	})

	t.Run("config beats the default", func(t *testing.T) {
		writeThemeConfig(t, "gruvbox")
		if got := themeFor(t); got != "gruvbox" {
			t.Fatalf("theme %q, want gruvbox", got)
		}
	})

	t.Run("default without a config", func(t *testing.T) {
		crazeHome(t)
		if got := themeFor(t); got != tui.DefaultTheme {
			t.Fatalf("theme %q, want the default %q", got, tui.DefaultTheme)
		}
		if tui.DefaultTheme != "craze-dark" {
			t.Fatalf("the pinned default is %q", tui.DefaultTheme)
		}
	})

	t.Run("a malformed config falls back to the default", func(t *testing.T) {
		writeCrazeConfig(t, "not [[ toml")
		if got := themeFor(t); got != tui.DefaultTheme {
			t.Fatalf("theme %q, want the default", got)
		}
	})
}

// TestUnknownThemeFlagIsAUsageError is plan 037 LC-10: an explicitly passed
// --theme that names no preset is a usage error — exit 2, the value quoted,
// as --provider's is, and the presets listed — on every command that takes
// one, before anything starts: the TUI (through runTUI, so before the
// detached or in-process split), craze attach and craze frame. A preset's
// alias and an explicitly empty --theme (the default's spelling) still pass,
// and so does a typo in config.toml, which falls back to the default as it
// always has.
func TestUnknownThemeFlagIsAUsageError(t *testing.T) {
	crazeHome(t)
	want := `craze: unknown theme "nosuch" (want craze-dark, craze-light, tokyo-night, dark, light, catppuccin, or gruvbox)`
	cmd, f := parseTUIFlags(t, "--theme", "nosuch")
	var ee *exitError
	if err := runTUI(cmd, f, hostEnv{}); !errors.As(err, &ee) || ee.code != 2 || ee.msg != want {
		t.Fatalf("craze --theme nosuch: %v, want exit 2, %q", err, want)
	}
	for _, argv := range [][]string{{"attach", "--theme", "nosuch"}, {"frame", "--theme", "nosuch"}} {
		if stdout, stderr, code := executeErr(argv); code != 2 || stdout != "" || stderr != want+"\n" {
			t.Fatalf("craze %q: exit %d, stdout %q, stderr %q; want 2, %q", argv, code, stdout, stderr, want)
		}
	}
	for _, theme := range []string{"gruvbox", "Tokyo_Night", "craze", ""} {
		cmd, f := parseTUIFlags(t, "--theme", theme)
		if err := checkThemeFlag(cmd, f.theme); err != nil {
			t.Fatalf("--theme %q: %v, want it taken", theme, err)
		}
	}
	writeThemeConfig(t, "nosuch")
	if got := themeFor(t); got != "nosuch" || tui.Preset(got).Name != tui.DefaultTheme {
		t.Fatalf("config.toml's typo: %q, want it kept and drawn as the default", got)
	}
}
