package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// journalDir is the one place craze decides whether a run journals (plan 020
// §3.5). The switches are a privacy opt-out, so these hold the fail-closed
// rule: anything craze cannot read as a clear "yes" leaves the journal off,
// and says why in exactly one line (A24).

// journalCase is one resolution: the environment and the config file it runs
// against, and what it must decide.
type journalCase struct {
	name string
	// env is CRAZE_JOURNAL; unset leaves it out of the environment entirely.
	env   string
	unset bool
	// config is the config.toml body; none writes no file, and dir makes
	// config.toml a directory, which craze cannot read.
	config  string
	none    bool
	confDir bool
	on      bool
	// why is the substring the one diag line must carry; "" means no line.
	why string
}

func TestJournalDirResolution(t *testing.T) {
	for _, tc := range []journalCase{
		{name: "no config, no env", unset: true, none: true, on: true},
		{name: "empty env is unset", env: "", none: true, on: true},
		{name: "blank env is unset", env: "   ", none: true, on: true},
		{name: "env true", env: "true", none: true, on: true},
		{name: "env 1", env: "1", none: true, on: true},
		{name: "env false", env: "false", none: true},
		{name: "env 0", env: "0", none: true},
		{name: "env maybe", env: "maybe", none: true, why: `CRAZE_JOURNAL="maybe" is not a bool`},
		{name: "config journal = true", unset: true, config: "journal = true\n", on: true},
		{name: "config journal = false", unset: true, config: "journal = false\n"},
		{name: "config journal is a string", unset: true, config: "journal = \"false\"\n",
			why: "config.toml journal is not a bool"},
		{name: "config journal = 1", unset: true, config: "journal = 1\n", why: "config.toml journal is not a bool"},
		{name: "another key, no journal key", unset: true, config: "theme = \"gruvbox\"\n", on: true},
		{name: "a config craze cannot parse", unset: true, config: "journal = \n", why: "config.toml could not be parsed"},
		{name: "a config craze cannot read", unset: true, confDir: true, why: "config.toml could not be read"},
		// The env var cannot turn journaling on against the config, and the
		// config's own reason is not printed on top of the env's.
		{name: "env true against journal = false", env: "true", config: "journal = false\n"},
		{name: "env 1 against a string journal", env: "1", config: "journal = \"false\"\n",
			why: "config.toml journal is not a bool"},
		{name: "env maybe against a malformed config", env: "maybe", config: "journal = \n",
			why: `CRAZE_JOURNAL="maybe" is not a bool`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := crazeHome(t)
			switch {
			case tc.confDir:
				if err := os.Mkdir(filepath.Join(home, "config.toml"), 0o700); err != nil {
					t.Fatal(err)
				}
			case !tc.none:
				if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(tc.config), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if tc.unset {
				if err := os.Unsetenv(journalEnv); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Unsetenv(journalEnv) })
			} else {
				t.Setenv(journalEnv, tc.env)
			}
			var diag bytes.Buffer
			got := journalDir(&diag)
			want := ""
			if tc.on {
				want = filepath.Join(home, "journal")
			}
			if got != want {
				t.Fatalf("journalDir = %q, want %q", got, want)
			}
			lines := diagLines(diag.String())
			switch {
			case tc.why == "" && len(lines) != 0:
				t.Fatalf("journalDir said %q, want nothing", lines)
			case tc.why != "":
				if len(lines) != 1 {
					t.Fatalf("journalDir said %q, want exactly one line about %q", lines, tc.why)
				}
				if !strings.HasPrefix(lines[0], "craze: journal off: ") || !strings.Contains(lines[0], tc.why) {
					t.Fatalf("the diag line is %q, want craze: journal off: … %s", lines[0], tc.why)
				}
			}
		})
	}
}

// diagLines is what was written to a diag writer, split into lines.
func diagLines(s string) []string {
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// TestJournalDirWithNoCrazeDirectory: with nowhere to keep craze's own files
// there is nowhere to journal either, and that is not a mistake to report —
// paths.JournalDir is "" exactly when the config file and the session index
// have no home either.
func TestJournalDirWithNoCrazeDirectory(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("CRAZE_HOME", "")
	t.Setenv(journalEnv, "true")
	var diag bytes.Buffer
	if got := journalDir(&diag); got != "" {
		t.Fatalf("journalDir = %q with no craze directory, want off", got)
	}
	if diag.Len() != 0 {
		t.Fatalf("journalDir said %q, want nothing", diag.String())
	}
}

// TestJournalDirIsAbsoluteUnderARelativeCrazeHome is A20's half of the
// resolver: a session is handed the directory once, at construction, so it
// must not follow a later change of working directory.
func TestJournalDirIsAbsoluteUnderARelativeCrazeHome(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("HOME", dir)
	t.Setenv("CRAZE_HOME", "relative-home")
	t.Setenv(journalEnv, "")
	var diag bytes.Buffer
	got := journalDir(&diag)
	if !filepath.IsAbs(got) {
		t.Fatalf("journalDir = %q, want an absolute path", got)
	}
	// The directory is resolved before it exists, and t.TempDir can hand back
	// a path through a symlink (/tmp on macOS), so the comparison is between
	// the working directory as the process reports it and as the test knows
	// it — never between two spellings of the same path.
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(wd, "relative-home", "journal"); got != want {
		t.Fatalf("journalDir = %q, want %q", got, want)
	}
	if diag.Len() != 0 {
		t.Fatalf("journalDir said %q, want nothing", diag.String())
	}
}

// TestPromptJournalsItsRun is the wiring, end to end in process: `craze
// prompt` against the fake agent leaves exactly one journal under
// $CRAZE_HOME/journal, and leaves none when either switch says so. The
// pytest suite asserts the file's contents through the built binary; this
// holds that the CLI asks the resolver at all, which a journal-negative
// suite alone could not.
func TestPromptJournalsItsRun(t *testing.T) {
	for _, tc := range []struct {
		name   string
		env    string
		config string
		want   int
	}{
		{name: "on by default", want: 1},
		{name: "CRAZE_JOURNAL=0", env: "0"},
		{name: "journal = false", config: "journal = false\n"},
		{name: "CRAZE_JOURNAL=1 against journal = false", env: "1", config: "journal = false\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateRunEnv(t)
			home := os.Getenv("CRAZE_HOME")
			if tc.config != "" {
				if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(tc.config), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if tc.env != "" {
				t.Setenv(journalEnv, tc.env)
			}
			t.Setenv("CRAZE_FAKE_SCRIPT", "echo")
			var stdout, stderr bytes.Buffer
			cmd := NewRootCmd()
			cmd.SetIn(&bytes.Buffer{})
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			cmd.SetArgs([]string{"prompt", "--agent-bin", fakeAgentPath(t), "--workspace", t.TempDir(), "hello"})
			if err := cmd.Execute(); err != nil {
				t.Fatalf("prompt: %v\nstderr: %s", err, stderr.String())
			}
			files, err := filepath.Glob(filepath.Join(home, "journal", "*", "*.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			if len(files) != tc.want {
				t.Fatalf("%d journals under %s, want %d: %v", len(files), home, tc.want, files)
			}
		})
	}
}
