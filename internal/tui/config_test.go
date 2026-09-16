package tui

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeConfigFile(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRAZE_CONFIG", path)
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

func TestConfigThemeWithoutAFile(t *testing.T) {
	t.Setenv("CRAZE_CONFIG", filepath.Join(t.TempDir(), "nested", "config.toml"))
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

// TestConfigPathFollowsHomeAndOverride pins where the file lives, because the
// isolation every test and the frame runner rely on is HOME-based.
func TestConfigPathFollowsHomeAndOverride(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CRAZE_CONFIG", "")
	if got, want := configPath(), filepath.Join(home, ".craze", "config.toml"); got != want {
		t.Fatalf("configPath %q, want %q", got, want)
	}
	override := filepath.Join(t.TempDir(), "elsewhere.toml")
	t.Setenv("CRAZE_CONFIG", override)
	if got := configPath(); got != override {
		t.Fatalf("CRAZE_CONFIG ignored: %q", got)
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
