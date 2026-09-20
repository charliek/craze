package tui

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
)

// writeConfigFile points CRAZE_HOME at a fresh directory and writes body as
// its config.toml. Returns the config file's path.
func writeConfigFile(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, configName)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRAZE_HOME", dir)
	return path
}

// TestSaveThemePreservesUnknownKeys is the safety property the config feature
// is judged on: craze reads the file as a map, so a key it knows nothing about
// still exists after it has written the theme.
func TestSaveThemePreservesUnknownKeys(t *testing.T) {
	path := writeConfigFile(t, `theme = "dark"
mouse = false
future = ["a", "b"]

[agent]
binary = "/usr/bin/cursor-agent"
timeout = 30
`)
	if got := ConfigTheme(); got != "dark" {
		t.Fatalf("theme %q", got)
	}
	if err := SaveTheme("gruvbox"); err != nil {
		t.Fatal(err)
	}
	if got := ConfigTheme(); got != "gruvbox" {
		t.Fatalf("theme after save %q", got)
	}

	cfg, err := readConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg["mouse"] != false {
		t.Fatalf("mouse = %v", cfg["mouse"])
	}
	future, _ := cfg["future"].([]any)
	if len(future) != 2 || future[0] != "a" {
		t.Fatalf("future = %v", cfg["future"])
	}
	agentCfg, _ := cfg["agent"].(map[string]any)
	if agentCfg["binary"] != "/usr/bin/cursor-agent" || agentCfg["timeout"] != int64(30) {
		t.Fatalf("agent table = %v", cfg["agent"])
	}

	// The write is a rename over the target, so no half-written copy is left
	// behind; the lock file the save serialises on is expected to stay.
	assertNoConfigTempFiles(t, path)
}

// assertNoConfigTempFiles holds the atomic-write contract: the directory ends
// up with the config and the lock file SaveTheme flocks, and nothing else.
func assertNoConfigTempFiles(t *testing.T, path string) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	want := map[string]bool{filepath.Base(path): true, filepath.Base(path) + configLockSuffix: true}
	for _, n := range names {
		if !want[n] {
			t.Fatalf("temp files left behind: %v", names)
		}
	}
}

// TestSaveThemeLeavesAMalformedConfigAlone: rewriting a file craze could not
// parse would throw away settings that are still the user's.
func TestSaveThemeLeavesAMalformedConfigAlone(t *testing.T) {
	body := "theme = \"dark\"\nthis is not toml [[[\n"
	path := writeConfigFile(t, body)

	err := SaveTheme("gruvbox")
	if err == nil {
		t.Fatal("saving over a malformed config should fail")
	}
	if !errors.Is(err, ErrConfigMalformed) {
		t.Fatalf("error %v, want ErrConfigMalformed", err)
	}
	if !strings.Contains(err.Error(), path) {
		t.Fatalf("the error should name the file: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != body {
		t.Fatalf("the config was rewritten:\n%s", b)
	}
	if got := ConfigTheme(); got != "" {
		t.Fatalf("an unreadable config has no theme, got %q", got)
	}
	assertNoConfigTempFiles(t, path)
}

// TestSaveThemeSerialisesWithAnotherWriter is the data-loss property: a second
// writer that reads the file, takes its time and then renames its own copy back
// must not be able to drop the theme craze wrote in between. SaveTheme holds
// the lock across its whole read-modify-write, so the other writer's rename
// lands first and craze reads what it left.
func TestSaveThemeSerialisesWithAnotherWriter(t *testing.T) {
	path := writeConfigFile(t, "theme = \"dark\"\n")

	// The other writer takes the lock and reads, exactly as SaveTheme does.
	unlock, err := lockConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	other, err := readConfigAt(path)
	if err != nil {
		t.Fatal(err)
	}

	saved := make(chan error, 1)
	go func() { saved <- SaveTheme("gruvbox") }()
	// Long enough for the save to reach the lock and block on it.
	time.Sleep(100 * time.Millisecond)

	other["mouse"] = false
	if err := writeConfig(path, other); err != nil {
		t.Fatal(err)
	}
	unlock()

	select {
	case err := <-saved:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SaveTheme never got the lock")
	}

	cfg, err := readConfigAt(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg["theme"] != "gruvbox" {
		t.Fatalf("the other writer's stale copy overwrote the theme: %v", cfg)
	}
	if cfg["mouse"] != false {
		t.Fatalf("the other writer's key was lost: %v", cfg)
	}
}

// TestConfigThemeWithoutAFile reads empty, and the first save creates the
// craze directory itself as well as the file.
func TestConfigThemeWithoutAFile(t *testing.T) {
	t.Setenv("CRAZE_HOME", filepath.Join(t.TempDir(), "nested"))
	if got := ConfigTheme(); got != "" {
		t.Fatalf("theme %q, want empty", got)
	}
	if err := SaveTheme("catppuccin"); err != nil {
		t.Fatal(err)
	}
	if got := ConfigTheme(); got != "catppuccin" {
		t.Fatalf("theme %q after the first save", got)
	}
}

// TestConfigPathFollowsHomeAndCrazeHome pins where the file lives, because
// the isolation every test and the frame runner rely on is HOME and
// CRAZE_HOME based: ~/.craze/config.toml by default, and config.toml directly
// inside CRAZE_HOME when that is set.
func TestConfigPathFollowsHomeAndCrazeHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CRAZE_HOME", "")
	if got, want := configPath(), filepath.Join(home, configDir, configName); got != want {
		t.Fatalf("configPath %q, want %q", got, want)
	}
	crazeHome := t.TempDir()
	t.Setenv("CRAZE_HOME", crazeHome)
	if got, want := configPath(), filepath.Join(crazeHome, configName); got != want {
		t.Fatalf("configPath %q with CRAZE_HOME set, want %q", got, want)
	}
}

// TestConfigTerminalTitleDefaultsTrue is the off switch's default: a config
// file that never mentions terminal_title at all still shows the tab title.
func TestConfigTerminalTitleDefaultsTrue(t *testing.T) {
	writeConfigFile(t, "theme = \"dark\"\n")
	if !ConfigTerminalTitle() {
		t.Fatal("terminal_title should default to true")
	}
}

// TestConfigTerminalTitleOff is the explicit off switch itself.
func TestConfigTerminalTitleOff(t *testing.T) {
	writeConfigFile(t, "terminal_title = false\n")
	if ConfigTerminalTitle() {
		t.Fatal("terminal_title = false should read false")
	}
	writeConfigFile(t, "terminal_title = true\n")
	if !ConfigTerminalTitle() {
		t.Fatal("terminal_title = true should read true")
	}
}

// TestConfigTerminalTitleMalformedDefaultsTrue matches ConfigTheme's own
// unreadable-config rule: a config craze cannot parse is not a reason to also
// lose the tab title, so it reads as the default rather than as off.
func TestConfigTerminalTitleMalformedDefaultsTrue(t *testing.T) {
	writeConfigFile(t, "this is not toml [[[\n")
	if !ConfigTerminalTitle() {
		t.Fatal("an unreadable config should default terminal_title to true")
	}
}

// TestConfigBackgroundTable is the whole of the setting's parsing rule (§3.4):
// only a literal `background = false` turns the themed terminal background
// off. A missing file, a file craze cannot parse, a missing key and a value
// that is not a bool all read as on — losing the theme's background is a
// worse answer to a broken config than ignoring the key it could not read.
func TestConfigBackgroundTable(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want bool
	}{
		{"missing key", "theme = \"dark\"\n", true},
		{"malformed file", "this is not toml [[[\n", true},
		{"true", "background = true\n", true},
		{"false", "background = false\n", false},
		{"non-bool", "background = \"no\"\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writeConfigFile(t, tc.body)
			if got := ConfigBackground(); got != tc.want {
				t.Fatalf("ConfigBackground() = %v, want %v", got, tc.want)
			}
		})
	}

	t.Run("missing file", func(t *testing.T) {
		t.Setenv("CRAZE_HOME", filepath.Join(t.TempDir(), "nothing"))
		if !ConfigBackground() {
			t.Fatal("no config file at all should still theme the background")
		}
	})
}

// TestConfigHostStatusTable is the whole of host_status's parsing rule (plan
// 015 §3.5): only a literal `host_status = false` turns host reporting off, and
// everything else — a missing file, an unparseable one, a missing key, a value
// that is not a bool — reads as on, exactly as background does.
func TestConfigHostStatusTable(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want bool
	}{
		{"missing key", "theme = \"dark\"\n", true},
		{"malformed file", "this is not toml [[[\n", true},
		{"true", "host_status = true\n", true},
		{"false", "host_status = false\n", false},
		{"non-bool", "host_status = \"no\"\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writeConfigFile(t, tc.body)
			if got := ConfigHostStatus(); got != tc.want {
				t.Fatalf("ConfigHostStatus() = %v, want %v", got, tc.want)
			}
		})
	}

	t.Run("missing file", func(t *testing.T) {
		t.Setenv("CRAZE_HOME", filepath.Join(t.TempDir(), "nothing"))
		if !ConfigHostStatus() {
			t.Fatal("no config file at all should still report host status")
		}
	})
}

// TestConfigCompatClaudeTable is the whole of [compat.claude]'s parsing rule
// (plan 022 §3.5, A13): every key defaults to true, only a literal false turns
// one off, and an absent table, a partial one and a config craze cannot read
// all leave every class on.
//
// The one place it parts company with the switches above is a value that is
// not a bool: it is the default *and* a line, because a typo there silently
// strips the user's own instructions or half their slash menu out of a session
// rather than losing a tab title.
func TestConfigCompatClaudeTable(t *testing.T) {
	allOn := agent.ClaudeCompat{}
	for _, tc := range []struct {
		name string
		body string
		want agent.ClaudeCompat
		why  []string
	}{
		{"absent table", "theme = \"dark\"\n", allOn, nil},
		{"malformed file", "this is not toml [[[\n", allOn, nil},
		{"empty table", "[compat.claude]\n", allOn, nil},
		{"a table that is not claude's", "[compat.other]\nskills = false\n", allOn, nil},
		{"everything on", "[compat.claude]\ninstructions = true\nrules = true\nskills = true\ncommands = true\nplugins = true\n", allOn, nil},
		{
			name: "everything off",
			body: "[compat.claude]\ninstructions = false\nrules = false\nskills = false\ncommands = false\nplugins = false\n",
			want: agent.ClaudeCompat{NoInstructions: true, NoRules: true, NoSkills: true, NoCommands: true, NoPlugins: true},
		},
		{
			name: "partial",
			body: "[compat.claude]\nskills = false\n",
			want: agent.ClaudeCompat{NoSkills: true},
		},
		{
			name: "one key at a time: instructions",
			body: "[compat.claude]\ninstructions = false\n",
			want: agent.ClaudeCompat{NoInstructions: true},
		},
		{
			name: "one key at a time: rules",
			body: "[compat.claude]\nrules = false\n",
			want: agent.ClaudeCompat{NoRules: true},
		},
		{
			name: "one key at a time: commands",
			body: "[compat.claude]\ncommands = false\n",
			want: agent.ClaudeCompat{NoCommands: true},
		},
		{
			name: "one key at a time: plugins",
			body: "[compat.claude]\nplugins = false\n",
			want: agent.ClaudeCompat{NoPlugins: true},
		},
		{
			name: "a value that is not a bool",
			body: "[compat.claude]\nskills = \"no\"\ncommands = false\n",
			want: agent.ClaudeCompat{NoCommands: true},
			why:  []string{"config.toml compat.claude.skills is not a bool; leaving it on"},
		},
		{
			// Two of them, and the order is the table's own rather than the
			// map's, so one run's diagnostics are every run's.
			name: "two values that are not bools",
			body: "[compat.claude]\nplugins = 1\ninstructions = \"yes\"\n",
			want: allOn,
			why: []string{
				"config.toml compat.claude.instructions is not a bool; leaving it on",
				"config.toml compat.claude.plugins is not a bool; leaving it on",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writeConfigFile(t, tc.body)
			got, why := ConfigCompatClaude()
			if got != tc.want {
				t.Errorf("ConfigCompatClaude() = %+v, want %+v", got, tc.want)
			}
			if strings.Join(why, "|") != strings.Join(tc.why, "|") {
				t.Errorf("diagnostics %q, want %q", why, tc.why)
			}
		})
	}

	t.Run("missing file", func(t *testing.T) {
		t.Setenv("CRAZE_HOME", filepath.Join(t.TempDir(), "nothing"))
		got, why := ConfigCompatClaude()
		if got != allOn || why != nil {
			t.Fatalf("no config file at all gave %+v and %q, want every class on and nothing said", got, why)
		}
	})
}
