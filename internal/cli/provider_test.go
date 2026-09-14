package cli

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/charliek/craze/internal/tui"
)

func providerFor(t *testing.T, hermetic bool, args ...string) (resolvedProvider, string, error) {
	t.Helper()
	var flag string
	var stderr bytes.Buffer
	var got resolvedProvider
	var resolveErr error
	cmd := &cobra.Command{
		Use:  "craze",
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			got, resolveErr = resolveProvider(cmd, flag, &stderr, hermetic)
			return resolveErr
		},
	}
	registerProviderFlag(cmd, &flag)
	cmd.SetArgs(args)
	cmd.SetOut(io.Discard)
	cmd.SetErr(&stderr)
	err := cmd.Execute()
	return got, stderr.String(), err
}

func TestUnknownProviderFlagExits2(t *testing.T) {
	for _, args := range [][]string{
		{"prompt", "--provider", "codex", "hi"},
		{"frame", "--provider", "codex"},
	} {
		cmd := NewRootCmd()
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs(args)
		err := cmd.Execute()
		var ee *exitError
		if !errors.As(err, &ee) || ee.code != 2 {
			t.Fatalf("args %v: %v", args, err)
		}
		if !strings.Contains(ee.msg, "unknown provider") {
			t.Fatalf("msg %q", ee.msg)
		}
	}
}

func TestProviderFlagEmptyIsUnset(t *testing.T) {
	t.Setenv("CRAZE_PROVIDER", "")
	t.Setenv("CRAZE_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	got, _, err := providerFor(t, false, "--provider", "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Provider.Name() != "cursor" || !got.Locked || got.Fallback {
		t.Fatalf("%+v", got)
	}
}

func TestProviderPrecedence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("provider = \"grok\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRAZE_CONFIG", path)

	t.Run("flag beats env and config", func(t *testing.T) {
		t.Setenv("CRAZE_PROVIDER", "grok")
		got, _, err := providerFor(t, false, "--provider", "cursor")
		if err != nil {
			t.Fatal(err)
		}
		if got.Provider.Name() != "cursor" || !got.Locked {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("env beats config", func(t *testing.T) {
		t.Setenv("CRAZE_PROVIDER", "cursor")
		got, _, err := providerFor(t, false)
		if err != nil {
			t.Fatal(err)
		}
		if got.Provider.Name() != "cursor" || got.Locked {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("config", func(t *testing.T) {
		t.Setenv("CRAZE_PROVIDER", "")
		got, _, err := providerFor(t, false)
		if err != nil {
			t.Fatal(err)
		}
		if got.Provider.Name() != "grok" {
			t.Fatalf("%+v", got)
		}
	})
}

func TestUnknownEnvAndConfigFallback(t *testing.T) {
	t.Run("env", func(t *testing.T) {
		t.Setenv("CRAZE_PROVIDER", "codex")
		t.Setenv("CRAZE_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
		got, stderr, err := providerFor(t, false)
		if err != nil {
			t.Fatal(err)
		}
		if !got.Fallback || got.Provider.Name() != "cursor" {
			t.Fatalf("%+v", got)
		}
		if !strings.Contains(stderr, `unknown provider "codex"`) {
			t.Fatalf("stderr %q", stderr)
		}
	})
	t.Run("config", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.toml")
		if err := os.WriteFile(path, []byte("provider = \"codex\"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("CRAZE_CONFIG", path)
		t.Setenv("CRAZE_PROVIDER", "")
		got, stderr, err := providerFor(t, false)
		if err != nil {
			t.Fatal(err)
		}
		if !got.Fallback || got.Provider.Name() != "cursor" {
			t.Fatalf("%+v", got)
		}
		if !strings.Contains(stderr, `unknown provider "codex"`) {
			t.Fatalf("stderr %q", stderr)
		}
	})
}

func TestFrameIgnoresEnvAndConfigProvider(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("provider = \"grok\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRAZE_CONFIG", path)
	t.Setenv("CRAZE_PROVIDER", "grok")
	got, _, err := providerFor(t, true)
	if err != nil {
		t.Fatal(err)
	}
	if got.Provider.Name() != "cursor" || !got.Locked || got.Fallback {
		t.Fatalf("hermetic %+v", got)
	}
	got, _, err = providerFor(t, true, "--provider", "grok")
	if err != nil {
		t.Fatal(err)
	}
	if got.Provider.Name() != "grok" {
		t.Fatalf("frame --provider grok %+v", got)
	}
}

func TestPromptPersistsProviderAfterStart(t *testing.T) {
	t.Setenv("XAI_API_KEY", "")
	t.Setenv("GROK_CODE_XAI_API_KEY", "")
	t.Setenv("CRAZE_PROVIDER", "")
	cfg := filepath.Join(t.TempDir(), "config.toml")
	t.Setenv("CRAZE_CONFIG", cfg)
	t.Setenv("CRAZE_FAKE_SCRIPT", "grok-echo")
	var stdout, stderr bytes.Buffer
	cmd := NewRootCmd()
	cmd.SetIn(&bytes.Buffer{})
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"prompt", "--json", "--provider", "grok", "--agent-bin", fakeAgentPath(t), "--workspace", t.TempDir(), "hi"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("prompt: %v\nstderr: %s", err, stderr.String())
	}
	if got := tui.ConfigProvider(); got != "grok" {
		t.Fatalf("persisted %q", got)
	}
}

func TestPromptDoesNotPersistFallback(t *testing.T) {
	t.Setenv("CRAZE_PROVIDER", "codex")
	cfg := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(cfg, []byte("theme = \"gruvbox\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRAZE_CONFIG", cfg)
	t.Setenv("CRAZE_FAKE_SCRIPT", "echo")
	var stdout, stderr bytes.Buffer
	cmd := NewRootCmd()
	cmd.SetIn(&bytes.Buffer{})
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"prompt", "--json", "--agent-bin", fakeAgentPath(t), "--workspace", t.TempDir(), "hi"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("prompt: %v\nstderr: %s", err, stderr.String())
	}
	if got := tui.ConfigProvider(); got != "" {
		t.Fatalf("fallback must not persist, got %q", got)
	}
	if !strings.Contains(stderr.String(), `unknown provider "codex"`) {
		t.Fatalf("stderr %q", stderr.String())
	}
}

func TestPromptDoesNotPersistFailedStart(t *testing.T) {
	t.Setenv("CRAZE_PROVIDER", "")
	cfg := filepath.Join(t.TempDir(), "config.toml")
	t.Setenv("CRAZE_CONFIG", cfg)
	t.Setenv("CRAZE_FAKE_SCRIPT", "authfail")
	cmd := NewRootCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"prompt", "--json", "--provider", "grok", "--agent-bin", fakeAgentPath(t), "--workspace", t.TempDir(), "hi"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected start failure")
	}
	if got := tui.ConfigProvider(); got != "" {
		t.Fatalf("failed start persisted %q", got)
	}
}
