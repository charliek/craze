package tui

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

	// The write is a rename over the target, so nothing else is left behind.
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "config.toml" {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("temp files left behind: %v", names)
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
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("a failed save left files behind: %v", entries)
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
