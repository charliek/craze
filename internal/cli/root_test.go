package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/charliek/craze/internal/version"
)

// TestRemovedConfigEnvIsAUsageErrorEverywhere is the CLI half of the
// tripwire: while the removed config variable is set, every command — the
// root, each subcommand, and any added later — exits 2 before it runs, with a
// message that names CRAZE_HOME, and leaves both HOME and CRAZE_HOME
// untouched. --version still answers: it reads nothing.
func TestRemovedConfigEnvIsAUsageErrorEverywhere(t *testing.T) {
	// Spelled in two parts on purpose: the repo-walk test in internal/paths
	// keeps the whole name out of every file outside that package, so a branch
	// still isolating itself with it fails CI. This is the one deliberate use.
	removed := "CRAZE_" + "CONFIG"
	home, crazeHome := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CRAZE_HOME", crazeHome)
	t.Setenv("CRAZE_PROVIDER", "")
	t.Setenv(removed, filepath.Join(t.TempDir(), "config.toml"))

	// Every runnable command, found by walking the tree so a new one is covered
	// without being listed, plus one prompt that would persist a provider.
	argvs := [][]string{{"prompt", "--json", "--provider", "grok", "hi"}}
	var walk func(c *cobra.Command, argv []string)
	walk = func(c *cobra.Command, argv []string) {
		if c.Runnable() {
			argvs = append(argvs, argv)
		}
		for _, sub := range c.Commands() {
			walk(sub, append(slices.Clone(argv), sub.Name()))
		}
	}
	walk(NewRootCmd(), nil)
	for _, argv := range argvs {
		var stdout, stderr bytes.Buffer
		cmd := NewRootCmd()
		cmd.SetIn(&bytes.Buffer{})
		cmd.SetOut(&stdout)
		cmd.SetErr(&stderr)
		cmd.SetArgs(argv)
		err := cmd.Execute()
		var ee *exitError
		if !errors.As(err, &ee) || ee.code != 2 {
			t.Fatalf("craze %q: got %v, want a usage error (exit 2)", argv, err)
		}
		for _, want := range []string{removed, "CRAZE_HOME"} {
			if !strings.Contains(ee.msg, want) {
				t.Fatalf("craze %q: message %q does not name %s", argv, ee.msg, want)
			}
		}
		if stdout.Len() != 0 {
			t.Fatalf("craze %q ran anyway: %q", argv, stdout.String())
		}
	}

	for _, dir := range []string{home, crazeHome} {
		if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
			t.Fatalf("%s was touched: %v %v", dir, entries, err)
		}
	}

	var stdout bytes.Buffer
	cmd := NewRootCmd()
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{"--version"})
	if err := cmd.Execute(); err != nil || strings.TrimSpace(stdout.String()) != version.Version {
		t.Fatalf("--version: %v %q", err, stdout.String())
	}
}

func TestVersionCommand(t *testing.T) {
	var buf bytes.Buffer
	cmd := NewRootCmd()
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"version"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	got := strings.TrimSpace(buf.String())
	if got != version.Version {
		t.Fatalf("version output %q, want %q", got, version.Version)
	}
}

// TestVersionFlag pins what cobra's built-in version flag did, now that craze
// owns the flag (D-37): the bare version and a newline, for both spellings, and
// answered before argument validation.
func TestVersionFlag(t *testing.T) {
	for _, argv := range [][]string{{"--version"}, {"-v"}, {"--version", "stray"}} {
		var buf bytes.Buffer
		cmd := NewRootCmd()
		cmd.SetOut(&buf)
		cmd.SetArgs(argv)
		if err := cmd.Execute(); err != nil {
			t.Fatalf("craze %q: %v", argv, err)
		}
		if got, want := buf.String(), version.Version+"\n"; got != want {
			t.Fatalf("craze %q printed %q, want %q", argv, got, want)
		}
	}
}

// TestPluginDirFlagOnEveryEntryPoint is §3.4: the same flag, spelled the same
// way and repeatable, on the root TUI command, on craze prompt and on craze
// frame. An entry point that lost it would start a session that silently
// expands nothing, and no golden would notice.
func TestPluginDirFlagOnEveryEntryPoint(t *testing.T) {
	root := NewRootCmd()
	cmds := map[string]*cobra.Command{"craze": root}
	for _, sub := range root.Commands() {
		cmds[sub.Name()] = sub
	}
	for _, name := range []string{"craze", "prompt", "frame"} {
		cmd, ok := cmds[name]
		if !ok {
			t.Fatalf("no %q command", name)
		}
		f := cmd.Flags().Lookup("plugin-dir")
		if f == nil {
			t.Fatalf("%s has no --plugin-dir", name)
		}
		// stringArray, not stringSlice: a plugin path may hold a comma, and a
		// repeatable flag is how cursor-agent spells this one.
		if f.Value.Type() != "stringArray" {
			t.Fatalf("%s --plugin-dir is a %s, want stringArray", name, f.Value.Type())
		}
	}
}
