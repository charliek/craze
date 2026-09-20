// Package paths is where craze decides where its own files live on disk: the
// user's home directory, the craze directory inside it, and the fixed names
// in that directory — the config file, the session index next to it, the
// native harness's own directory, and the session journals'. Both internal/tui (config) and
// internal/sessions (the index) call this package so neither depends on the
// other for something as basic as "where is home".
package paths

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

const (
	// crazeHomeEnv relocates the craze directory: everything below lives in
	// $CRAZE_HOME instead of ~/.craze when it is set.
	crazeHomeEnv = "CRAZE_HOME"
	// crazeDirName is the craze directory's name under the home directory.
	crazeDirName = ".craze"
	// configName, sessionsName, nativeName and journalName are fixed names
	// inside the craze directory: the config file, the session index next
	// to it, the native harness's directory, and the session journals'.
	configName   = "config.toml"
	sessionsName = "sessions.jsonl"
	nativeName   = "native"
	journalName  = "journal"

	// removedConfigEnv named the config *file* before CRAZE_HOME replaced it.
	// It is kept only as a tripwire (tripped, CheckEnv): a script or test
	// still isolating itself with it would otherwise be silently ignored and
	// read and write the real ~/.craze. Delete it, the tripwire in CrazeDir,
	// and CheckEnv together, a release after the switch.
	removedConfigEnv = "CRAZE_CONFIG"
)

// HomeDir is the user's home directory: HOME wins over the account database,
// so a test (and the frame runner) can isolate it with one variable. It keys
// the user-level skills and plugin caches (see agent.HomeDir), and it is the
// default parent of the craze directory. CRAZE_HOME relocates only the craze
// directory's contents, never the skills or plugin caches. "" means there is
// no home directory to use.
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

// CrazeDir is the directory craze keeps its own files in: $CRAZE_HOME when it
// is set, else ~/.craze. A relative CRAZE_HOME stays relative to the current
// working directory, except that a leading "~" or "~/" means the home
// directory: an env file or a quoted shell assignment passes the tilde
// through unexpanded, and taking it literally would put the owner's config in
// a directory called "~" under wherever craze happened to start. "" when
// there is neither — or when the removed
// CRAZE_CONFIG is still set (see CheckEnv) — and every caller then persists
// nothing rather than fall back to a relative path of its own.
func CrazeDir() string {
	if tripped() {
		return ""
	}
	if dir := strings.TrimSpace(os.Getenv(crazeHomeEnv)); dir != "" {
		return expandTilde(dir)
	}
	home := HomeDir()
	if home == "" {
		return ""
	}
	return filepath.Join(home, crazeDirName)
}

// ConfigPath is where the persisted settings live, or "" when CrazeDir is "".
func ConfigPath() string {
	return inCrazeDir(configName)
}

// SessionsPath is the session index, always the config file's sibling in the
// craze directory. "" when CrazeDir is "": callers must never fall back to a
// relative "./sessions.jsonl".
func SessionsPath() string {
	return inCrazeDir(sessionsName)
}

// NativeDir is the native harness's directory inside the craze directory. It
// is absolute even when CRAZE_HOME is relative, because the harness is handed
// it once and must not follow a later change of working directory. "" when
// CrazeDir is "" or the working directory cannot be read.
func NativeDir() string {
	return absInCrazeDir(nativeName)
}

// JournalDir is where session journals live, one directory per workspace
// below it (internal/journal). It is absolute even when CRAZE_HOME is
// relative, for NativeDir's reason: a session is handed it once, at
// construction, and its journal must not follow a later change of working
// directory. "" when CrazeDir is "" or the working directory cannot be read,
// and the caller then journals nothing.
func JournalDir() string {
	return absInCrazeDir(journalName)
}

// expandTilde cleans dir, first replacing a leading "~" or "~/" with the home
// directory. "~user" is not expanded. With no home directory to expand into
// it returns "", so nothing is persisted rather than a literal "~" directory.
func expandTilde(dir string) string {
	if dir != "~" && !strings.HasPrefix(dir, "~/") && !strings.HasPrefix(dir, "~"+string(filepath.Separator)) {
		return filepath.Clean(dir)
	}
	home := HomeDir()
	if home == "" {
		return ""
	}
	return filepath.Join(home, dir[1:])
}

func inCrazeDir(name string) string {
	dir := CrazeDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, name)
}

// absInCrazeDir is inCrazeDir made absolute once, against the working
// directory at the time of the call; "" when either step fails.
func absInCrazeDir(name string) string {
	dir := inCrazeDir(name)
	if dir == "" {
		return ""
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return ""
	}
	return abs
}

// CheckEnv refuses the removed CRAZE_CONFIG. The CLI calls it before any
// command runs and reports its error as a usage error; CrazeDir enforces the
// same rule for every caller that never goes through the CLI.
func CheckEnv() error {
	if !tripped() {
		return nil
	}
	return errors.New(removedConfigEnv + " is no longer supported; unset it and set " +
		crazeHomeEnv + " to the directory that holds config.toml instead (it defaults to ~/.craze)")
}

// tripped reports whether the removed CRAZE_CONFIG is set. Set means
// non-empty after trimming, so a test that cleared it the old way, with
// t.Setenv(name, ""), does not trip it.
func tripped() bool {
	return strings.TrimSpace(os.Getenv(removedConfigEnv)) != ""
}
