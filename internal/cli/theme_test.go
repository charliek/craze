package cli

import (
	"io"
	"os"
	"path/filepath"
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
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("theme = \""+name+"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRAZE_CONFIG", path)
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
		t.Setenv("CRAZE_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
		if got := themeFor(t); got != tui.DefaultTheme {
			t.Fatalf("theme %q, want the default %q", got, tui.DefaultTheme)
		}
		if tui.DefaultTheme != "craze-dark" {
			t.Fatalf("the pinned default is %q", tui.DefaultTheme)
		}
	})

	t.Run("a malformed config falls back to the default", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.toml")
		if err := os.WriteFile(path, []byte("not [[ toml"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("CRAZE_CONFIG", path)
		if got := themeFor(t); got != tui.DefaultTheme {
			t.Fatalf("theme %q, want the default", got)
		}
	})
}
