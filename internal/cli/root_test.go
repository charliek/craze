package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/charliek/craze/internal/version"
)

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
