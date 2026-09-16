// Package paths is where craze decides where its own files live on disk: the
// user's home directory, the config file inside it, and the session index
// that sits next to the config file. Both internal/tui (config) and
// internal/sessions (the index) call this package so neither depends on the
// other for something as basic as "where is home".
package paths

import (
	"os"
	"path/filepath"
	"strings"
)

const (
	// configDir / configName make up ~/.craze/config.toml; CRAZE_CONFIG
	// replaces the whole path.
	configDir  = ".craze"
	configName = "config.toml"
	// sessionsName is the session index file, always a sibling of the
	// config file.
	sessionsName = "sessions.jsonl"
)

// HomeDir is the home directory craze reads its own files out of: the config
// file, the session index, the user-level skills and the plugin caches. HOME
// wins over the account database so a test (and the frame runner) can
// isolate all of them with one variable. "" means there is no home directory
// to use.
func HomeDir() string {
	if home := strings.TrimSpace(os.Getenv("HOME")); home != "" {
		return home
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(home)
}

// ConfigPath is where the persisted settings live, or "" when there is no
// home directory to put them in. CRAZE_CONFIG overrides the whole path and
// may be relative, in which case it stays relative to the current working
// directory.
func ConfigPath() string {
	if p := strings.TrimSpace(os.Getenv("CRAZE_CONFIG")); p != "" {
		return p
	}
	home := HomeDir()
	if home == "" {
		return ""
	}
	return filepath.Join(home, configDir, configName)
}

// SessionsPath is the session index, always the sibling of the config file
// (same directory, "sessions.jsonl") -- so a relative CRAZE_CONFIG carries
// the index along with it, and the frame runner's isolated HOME isolates the
// index for free. "" when ConfigPath is "" (no home directory): callers must
// never fall back to a relative "./sessions.jsonl".
func SessionsPath() string {
	cfg := ConfigPath()
	if cfg == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(cfg), sessionsName)
}
