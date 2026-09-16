package paths

import (
	"path/filepath"
	"testing"
)

func TestHomeDirPrefersHOME(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if got := HomeDir(); got != home {
		t.Fatalf("HomeDir() = %q, want %q", got, home)
	}
}

func TestHomeDirEmptyWithoutHOME(t *testing.T) {
	// On Linux, os.UserHomeDir consults only $HOME, so clearing it (rather
	// than merely leaving it unset) reproduces "no home directory" without
	// touching the real account database.
	t.Setenv("HOME", "")
	if got := HomeDir(); got != "" {
		t.Fatalf("HomeDir() = %q, want empty", got)
	}
}

func TestConfigPathFollowsHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CRAZE_CONFIG", "")
	want := filepath.Join(home, ".craze", "config.toml")
	if got := ConfigPath(); got != want {
		t.Fatalf("ConfigPath() = %q, want %q", got, want)
	}
}

func TestConfigPathHonoursOverride(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	override := filepath.Join(t.TempDir(), "elsewhere.toml")
	t.Setenv("CRAZE_CONFIG", override)
	if got := ConfigPath(); got != override {
		t.Fatalf("ConfigPath() = %q, want %q (CRAZE_CONFIG ignored)", got, override)
	}
}

func TestConfigPathEmptyWithoutHome(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("CRAZE_CONFIG", "")
	if got := ConfigPath(); got != "" {
		t.Fatalf("ConfigPath() = %q, want empty", got)
	}
}

func TestSessionsPathIsConfigSibling(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CRAZE_CONFIG", "")
	want := filepath.Join(home, ".craze", "sessions.jsonl")
	if got := SessionsPath(); got != want {
		t.Fatalf("SessionsPath() = %q, want %q", got, want)
	}
}

// TestSessionsPathFollowsAbsoluteOverride pins the example from the plan:
// CRAZE_CONFIG=/x/y.toml puts the index at /x/sessions.jsonl.
func TestSessionsPathFollowsAbsoluteOverride(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CRAZE_CONFIG", filepath.Join(dir, "y.toml"))
	want := filepath.Join(dir, "sessions.jsonl")
	if got := SessionsPath(); got != want {
		t.Fatalf("SessionsPath() = %q, want %q", got, want)
	}
}

// TestSessionsPathFollowsRelativeOverride: a relative CRAZE_CONFIG stays
// relative to the cwd, as today, and the index follows it as a sibling --
// never an absolute "./sessions.jsonl" surprise.
func TestSessionsPathFollowsRelativeOverride(t *testing.T) {
	t.Setenv("CRAZE_CONFIG", filepath.Join("sub", "y.toml"))
	want := filepath.Join("sub", "sessions.jsonl")
	if got := SessionsPath(); got != want {
		t.Fatalf("SessionsPath() = %q, want %q", got, want)
	}
}

// TestSessionsPathEmptyWithoutHome is the "never ./sessions.jsonl" contract:
// with no home directory and no override, SessionsPath is "", not a relative
// path a careless caller might create in the current directory.
func TestSessionsPathEmptyWithoutHome(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("CRAZE_CONFIG", "")
	if got := SessionsPath(); got != "" {
		t.Fatalf("SessionsPath() = %q, want empty", got)
	}
}
