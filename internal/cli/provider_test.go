package cli

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/charliek/craze/internal/agent"
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

func TestJoinOr(t *testing.T) {
	cases := []struct {
		names []string
		want  string
	}{
		{nil, ""},
		{[]string{"cursor"}, "cursor"},
		{[]string{"cursor", "grok"}, "cursor or grok"},
		{[]string{"cursor", "grok", "gx"}, "cursor, grok, or gx"},
	}
	for _, c := range cases {
		if got := joinOr(c.names); got != c.want {
			t.Fatalf("joinOr(%v) = %q, want %q", c.names, got, c.want)
		}
	}
}

func TestUnknownProviderErrorNamesEveryProvider(t *testing.T) {
	cmd := NewRootCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"prompt", "--provider", "codex", "hi"})
	err := cmd.Execute()
	var ee *exitError
	if !errors.As(err, &ee) {
		t.Fatalf("%v", err)
	}
	const want = `craze: unknown provider "codex" (want cursor, grok, gx, or native)`
	if ee.msg != want {
		t.Fatalf("msg %q, want %q", ee.msg, want)
	}
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
	crazeHome(t)
	got, _, err := providerFor(t, false, "--provider", "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Provider.Name() != "cursor" || !got.Locked || got.Fallback {
		t.Fatalf("%+v", got)
	}
}

func TestProviderPrecedence(t *testing.T) {
	writeCrazeConfig(t, "provider = \"grok\"\n")

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
		crazeHome(t)
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
		writeCrazeConfig(t, "provider = \"codex\"\n")
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
	writeCrazeConfig(t, "provider = \"grok\"\n")
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
	isolateHome(t)
	crazeHome(t)
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
	isolateHome(t)
	writeCrazeConfig(t, "theme = \"gruvbox\"\n")
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
	isolateHome(t)
	crazeHome(t)
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

// writeExecutable writes an executable stub at dir/name. Mode 0o755 matters: a
// 0o644 file makes exec.LookPath fail for the wrong reason, so a negative case
// would pass by accident.
func writeExecutable(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func providerNames(list []agent.Provider) []string {
	out := make([]string, 0, len(list))
	for _, p := range list {
		out = append(out, p.Name())
	}
	return out
}

// TestPickerProviders pins the picker's availability filter: cursor, grok and
// native are unconditional, so the picker is never empty, and gx appears only
// when a binary for it resolves. Resolution is the same question spawn asks,
// so --agent-bin reveals gx and an unresolvable --agent-bin hides it even with
// gx on PATH (§3.3, AC 4 and AC 9). Native is in-process and never optional
// (plan 028 §3.16), so it is unaffected by PATH or --agent-bin.
//
// CRAZE_AGENT_BIN is cleared once for every case and each builds its own PATH
// in a temp directory, so none of them can read what the host happens to have
// installed.
func TestPickerProviders(t *testing.T) {
	t.Setenv("CRAZE_AGENT_BIN", "")
	t.Run("gx absent", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		if got := providerNames(pickerProviders("")); !reflect.DeepEqual(got, []string{"cursor", "grok", "native"}) {
			t.Fatalf("rows %q, want [cursor grok native]", got)
		}
	})
	t.Run("gx on PATH", func(t *testing.T) {
		dir := t.TempDir()
		writeExecutable(t, dir, "gx")
		t.Setenv("PATH", dir)
		if got := providerNames(pickerProviders("")); !reflect.DeepEqual(got, []string{"cursor", "grok", "gx", "native"}) {
			t.Fatalf("rows %q, want [cursor grok gx native]", got)
		}
	})
	t.Run("agent-bin reveals gx", func(t *testing.T) {
		bin := writeExecutable(t, t.TempDir(), "some-agent")
		t.Setenv("PATH", t.TempDir())
		if got := providerNames(pickerProviders(bin)); !reflect.DeepEqual(got, []string{"cursor", "grok", "gx", "native"}) {
			t.Fatalf("rows %q, want [cursor grok gx native]", got)
		}
	})
	t.Run("invalid agent-bin hides gx", func(t *testing.T) {
		dir := t.TempDir()
		writeExecutable(t, dir, "gx")
		t.Setenv("PATH", dir)
		missing := filepath.Join(t.TempDir(), "does-not-exist")
		if got := providerNames(pickerProviders(missing)); !reflect.DeepEqual(got, []string{"cursor", "grok", "native"}) {
			t.Fatalf("rows %q, want [cursor grok native] — the override is exclusive", got)
		}
	})
}

// TestGxResolvesThroughEveryEntryPoint pins §3.2: gx is a provider id like any
// other at all three precedence levels, and resolution never consults PATH — a
// missing binary is a spawn error, not an unknown provider (AC 5).
func TestGxResolvesThroughEveryEntryPoint(t *testing.T) {
	// No binary override and an empty PATH for every case: whichever entry
	// point names gx, resolution must succeed without one.
	t.Setenv("CRAZE_AGENT_BIN", "")
	t.Setenv("PATH", t.TempDir())
	t.Run("flag", func(t *testing.T) {
		t.Setenv("CRAZE_PROVIDER", "")
		crazeHome(t)
		got, _, err := providerFor(t, false, "--provider", "gx")
		if err != nil {
			t.Fatal(err)
		}
		if got.Provider.Name() != "gx" || !got.Locked || got.Fallback {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("env", func(t *testing.T) {
		t.Setenv("CRAZE_PROVIDER", "gx")
		crazeHome(t)
		got, stderr, err := providerFor(t, false)
		if err != nil {
			t.Fatal(err)
		}
		if got.Provider.Name() != "gx" || got.Fallback {
			t.Fatalf("%+v stderr %q", got, stderr)
		}
	})
	t.Run("config", func(t *testing.T) {
		t.Setenv("CRAZE_PROVIDER", "")
		writeCrazeConfig(t, "provider = \"gx\"\n")
		got, stderr, err := providerFor(t, false)
		if err != nil {
			t.Fatal(err)
		}
		if got.Provider.Name() != "gx" || got.Fallback {
			t.Fatalf("%+v stderr %q", got, stderr)
		}
	})
}

// TestPromptPersistsGxProviderKeepingOtherKeys is AC 5's automatable half:
// choosing gx writes provider = "gx" and leaves the rest of the config file
// alone. The run goes through --agent-bin, so it proves the id flows all the
// way through CLI resolution without gx being installed.
func TestPromptPersistsGxProviderKeepingOtherKeys(t *testing.T) {
	bin := fakeAgentPath(t)
	t.Setenv("XAI_API_KEY", "")
	t.Setenv("GROK_CODE_XAI_API_KEY", "")
	t.Setenv("CRAZE_PROVIDER", "")
	isolateHome(t)
	writeCrazeConfig(t, "theme = \"gruvbox\"\n")
	t.Setenv("CRAZE_FAKE_SCRIPT", "grok-echo")
	var stdout, stderr bytes.Buffer
	cmd := NewRootCmd()
	cmd.SetIn(&bytes.Buffer{})
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"prompt", "--json", "--provider", "gx", "--agent-bin", bin, "--workspace", t.TempDir(), "hi"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("prompt: %v\nstderr: %s", err, stderr.String())
	}
	if got := tui.ConfigProvider(); got != "gx" {
		t.Fatalf("persisted %q", got)
	}
	if got := tui.ConfigTheme(); got != "gruvbox" {
		t.Fatalf("saving the provider disturbed theme: %q", got)
	}
}

// TestNativeIsListedAndPersistable is A34, plan 028 §3.16/D-65: native sits in
// Providers() right after gx and ProviderNames names it, so it is no longer
// the hidden exception knownProvider and the picker used to carve out — and,
// being a listed in-process provider, it can be loaded again and has modes,
// the safe answer C19's narrowed registry test (internal/agent) also pins.
// Choosing it persists it the same way any other provider's session would
// (persistProvider/SaveProvider), which through H7 a hidden provider never
// did (TestPersistProviderSkipsAHiddenProvider, above).
func TestNativeIsListedAndPersistable(t *testing.T) {
	if got, want := agent.ProviderNames(), []string{"cursor", "grok", "gx", "native"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ProviderNames %q, want %q", got, want)
	}
	native, err := agent.ProviderByName("native")
	if err != nil {
		t.Fatalf("ProviderByName(native): %v", err)
	}
	if native.Hidden() {
		t.Fatal("native is listed now (D-65): it must not be hidden")
	}
	if !native.InProcess() || !native.Resumable() || !native.Capabilities().Modes {
		t.Fatalf("native inProcess=%v resumable=%v modes=%v, want all true",
			native.InProcess(), native.Resumable(), native.Capabilities().Modes)
	}

	writeCrazeConfig(t, "provider = \"grok\"\ntheme = \"gruvbox\"\n")
	if err := persistProvider(resolvedProvider{Provider: native}); err != nil {
		t.Fatalf("persistProvider: %v", err)
	}
	if got := tui.ConfigProvider(); got != "native" {
		t.Fatalf("choosing native did not persist it: %q", got)
	}
	if got := tui.ConfigTheme(); got != "gruvbox" {
		t.Fatalf("persisting the provider disturbed theme: %q", got)
	}
}

// TestProviderFlagUsageSaysWhatRunsWhere is C19a (sol r1-c19): once D-65 listed
// native, --provider's help called all four "ACP provider"s, and native is not
// one — it runs inside craze. Every command that takes the flag says which are
// the ACP agents craze spawns and which runs inside craze.
func TestProviderFlagUsageSaysWhatRunsWhere(t *testing.T) {
	const want = "provider: cursor, grok, gx (ACP agents) or native (runs inside craze)"
	root := NewRootCmd()
	for _, path := range [][]string{nil, {"prompt"}, {"frame"}} {
		cmd, _, err := root.Find(path)
		if err != nil {
			t.Fatalf("%v: %v", path, err)
		}
		flag := cmd.Flags().Lookup("provider")
		if flag == nil {
			t.Fatalf("%q has no --provider", cmd.Name())
		}
		if flag.Usage != want {
			t.Fatalf("%s --provider usage %q, want %q", cmd.Name(), flag.Usage, want)
		}
	}
	// A registry of ACP agents alone names no harness.
	if got := providerUsage([]agent.Provider{agent.CursorProvider(), agent.GrokProvider()}); got != "provider: cursor, grok (ACP agents)" {
		t.Fatalf("ACP agents alone: %q", got)
	}
}
