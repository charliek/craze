package paths

import (
	"path/filepath"
	"strings"
	"testing"
)

// crazeEnv pins the three variables every function here reads, so a case
// never depends on what the developer has exported: HOME as given, CRAZE_HOME
// as given ("" is unset), and the removed CRAZE_CONFIG cleared unless a case
// sets it itself.
func crazeEnv(t *testing.T, home, crazeHome string) {
	t.Helper()
	t.Setenv("HOME", home)
	t.Setenv(crazeHomeEnv, crazeHome)
	t.Setenv(removedConfigEnv, "")
}

// all is every location this package hands out, in one comparable value.
type all struct{ craze, config, sessions, native, journal string }

func locations() all {
	return all{CrazeDir(), ConfigPath(), SessionsPath(), NativeDir(), JournalDir()}
}

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

// TestHomeDirIgnoresCrazeHome: CRAZE_HOME moves the craze directory, never
// the home directory the skills and plugin caches are keyed off.
func TestHomeDirIgnoresCrazeHome(t *testing.T) {
	home := t.TempDir()
	crazeEnv(t, home, t.TempDir())
	if got := HomeDir(); got != home {
		t.Fatalf("HomeDir() = %q, want %q", got, home)
	}
}

// TestDefaultsUnderHome is today's layout, unchanged: with no CRAZE_HOME
// everything sits under ~/.craze.
func TestDefaultsUnderHome(t *testing.T) {
	home := t.TempDir()
	crazeEnv(t, home, "")
	dir := filepath.Join(home, ".craze")
	want := all{dir, filepath.Join(dir, "config.toml"), filepath.Join(dir, "sessions.jsonl"), filepath.Join(dir, "native"), filepath.Join(dir, "journal")}
	if got := locations(); got != want {
		t.Fatalf("locations() = %+v, want %+v", got, want)
	}
}

// TestCrazeHomeAbsolute: CRAZE_HOME=/x puts config, index, native/ and
// journal/ under /x itself, not /x/.craze, and HOME plays no part.
func TestCrazeHomeAbsolute(t *testing.T) {
	dir := t.TempDir()
	crazeEnv(t, t.TempDir(), dir)
	want := all{dir, filepath.Join(dir, "config.toml"), filepath.Join(dir, "sessions.jsonl"), filepath.Join(dir, "native"), filepath.Join(dir, "journal")}
	if got := locations(); got != want {
		t.Fatalf("locations() = %+v, want %+v", got, want)
	}
}

// TestCrazeHomeWorksWithoutHome: CRAZE_HOME is enough on its own; a missing
// home directory only matters when it is the default parent.
func TestCrazeHomeWorksWithoutHome(t *testing.T) {
	dir := t.TempDir()
	crazeEnv(t, "", dir)
	if got, want := ConfigPath(), filepath.Join(dir, "config.toml"); got != want {
		t.Fatalf("ConfigPath() = %q, want %q", got, want)
	}
}

// TestCrazeHomeRelative: a relative CRAZE_HOME stays relative to the working
// directory for the config file and the index — never an absolute surprise —
// while NativeDir and JournalDir are made absolute, because the harness and a
// session's journal are each handed theirs once.
func TestCrazeHomeRelative(t *testing.T) {
	cwd := t.TempDir()
	t.Chdir(cwd)
	crazeEnv(t, t.TempDir(), filepath.Join("sub", "craze"))
	rel := filepath.Join("sub", "craze")
	if got := CrazeDir(); got != rel {
		t.Fatalf("CrazeDir() = %q, want %q", got, rel)
	}
	if got, want := ConfigPath(), filepath.Join(rel, "config.toml"); got != want {
		t.Fatalf("ConfigPath() = %q, want %q", got, want)
	}
	if got, want := SessionsPath(), filepath.Join(rel, "sessions.jsonl"); got != want {
		t.Fatalf("SessionsPath() = %q, want %q", got, want)
	}
	// t.Chdir resolves nothing, and the temp directory may sit behind a
	// symlink (macOS's /var), so compare against what Abs itself sees.
	wantNative, err := filepath.Abs(filepath.Join(rel, "native"))
	if err != nil {
		t.Fatal(err)
	}
	got := NativeDir()
	if !filepath.IsAbs(got) || got != wantNative {
		t.Fatalf("NativeDir() = %q, want the absolute %q", got, wantNative)
	}
	wantJournal, err := filepath.Abs(filepath.Join(rel, "journal"))
	if err != nil {
		t.Fatal(err)
	}
	if got := JournalDir(); !filepath.IsAbs(got) || got != wantJournal {
		t.Fatalf("JournalDir() = %q, want the absolute %q", got, wantJournal)
	}
}

// TestCrazeHomeWhitespaceIsUnset: a blank CRAZE_HOME is no CRAZE_HOME, not a
// directory called " ".
func TestCrazeHomeWhitespaceIsUnset(t *testing.T) {
	home := t.TempDir()
	crazeEnv(t, home, "  \t ")
	if got, want := CrazeDir(), filepath.Join(home, ".craze"); got != want {
		t.Fatalf("CrazeDir() = %q, want %q", got, want)
	}
}

// TestCrazeHomeTildeIsHome: an unexpanded "~" (an env file, a quoted
// assignment) means the home directory, never a directory called "~" in the
// working directory; "~user" is left alone, and with no home to expand into
// nothing is persisted.
func TestCrazeHomeTildeIsHome(t *testing.T) {
	home := t.TempDir()
	for _, tc := range []struct{ value, want string }{
		{"~", home},
		{"~/", home},
		{"~/x/y", filepath.Join(home, "x", "y")},
		{"  ~/x  ", filepath.Join(home, "x")},
		{"~other/x", filepath.Join("~other", "x")},
	} {
		crazeEnv(t, home, tc.value)
		if got := CrazeDir(); got != tc.want {
			t.Errorf("CRAZE_HOME=%q: CrazeDir() = %q, want %q", tc.value, got, tc.want)
		}
	}
	crazeEnv(t, "", "~/x")
	if got := CrazeDir(); got != "" {
		t.Errorf("CRAZE_HOME=~/x with no HOME: CrazeDir() = %q, want empty", got)
	}
}

// TestNoHomeMeansNoLocations is the "never ./sessions.jsonl" contract: with
// no home directory and no CRAZE_HOME, every location is "", not a relative
// path a careless caller might create in the current directory.
func TestNoHomeMeansNoLocations(t *testing.T) {
	crazeEnv(t, "", "")
	if got := locations(); got != (all{}) {
		t.Fatalf("locations() = %+v, want all empty", got)
	}
}

// TestRemovedConfigEnvFailsClosed is the tripwire: while the removed variable
// is set, every location is "" — CRAZE_HOME or not — so nothing that skips
// the CLI can reach the real ~/.craze either, and CheckEnv's error tells the
// user what to set instead.
func TestRemovedConfigEnvFailsClosed(t *testing.T) {
	for _, crazeHome := range []string{"", t.TempDir()} {
		crazeEnv(t, t.TempDir(), crazeHome)
		t.Setenv(removedConfigEnv, filepath.Join(t.TempDir(), "config.toml"))
		if got := locations(); got != (all{}) {
			t.Fatalf("CRAZE_HOME=%q: locations() = %+v, want all empty", crazeHome, got)
		}
		err := CheckEnv()
		if err == nil {
			t.Fatalf("CRAZE_HOME=%q: CheckEnv() = nil, want an error", crazeHome)
		}
		for _, want := range []string{removedConfigEnv, "CRAZE_HOME", "~/.craze"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("CheckEnv() = %q, want it to name %q", err, want)
			}
		}
	}
}

// TestRemovedConfigEnvBlankIsNotSet: a blank value — which is how tests used
// to clear it — trips nothing.
func TestRemovedConfigEnvBlankIsNotSet(t *testing.T) {
	home := t.TempDir()
	crazeEnv(t, home, "")
	for _, blank := range []string{"", "  "} {
		t.Setenv(removedConfigEnv, blank)
		if err := CheckEnv(); err != nil {
			t.Fatalf("%q: CheckEnv() = %v, want nil", blank, err)
		}
		if got, want := CrazeDir(), filepath.Join(home, ".craze"); got != want {
			t.Fatalf("%q: CrazeDir() = %q, want %q", blank, got, want)
		}
	}
}
