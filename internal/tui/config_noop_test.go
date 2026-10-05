package tui

import (
	"os"
	"testing"
	"time"
)

// configWithComments is a config.toml as a person keeps it: comments, an
// inline one, a table, the layout their own.
const configWithComments = `# my own notes on this file
provider = "cursor" # the one I use
theme = "gruvbox"

[agents]
  # an override
  cursor = "/opt/cursor/agent"
`

// TestConfigSaveOfTheStoredValueWritesNothing is plan 037 LC-5: saving the
// provider or the theme the file already holds — exactly that string — is a
// no-op, so a start that saves the provider it started on leaves the file
// alone: its bytes, comments and layout included, and its inode (no rename
// over it). A value that differs at all, padding included, is written.
func TestConfigSaveOfTheStoredValueWritesNothing(t *testing.T) {
	for _, tc := range []struct {
		name string
		save func(string) error
		same string
	}{
		{"provider", SaveProvider, "cursor"},
		{"theme", SaveTheme, "gruvbox"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeConfigFile(t, configWithComments)
			before, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := tc.save(tc.same); err != nil {
				t.Fatal(err)
			}
			after, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if b, err := os.ReadFile(path); err != nil || string(b) != configWithComments {
				t.Fatalf("a save of the stored value rewrote the file (%v):\n%s", err, b)
			}
			if !os.SameFile(before, after) {
				t.Fatal("a save of the stored value replaced the file (another inode)")
			}
			assertNoConfigTempFiles(t, path)

			// Negative control: the same value padded is another string, and
			// is written — the comparison is exact, not trimmed.
			if err := tc.save(" " + tc.same); err != nil {
				t.Fatal(err)
			}
			if b, err := os.ReadFile(path); err != nil || string(b) == configWithComments {
				t.Fatalf("a different value was not written (%v)", err)
			}
		})
	}
}

// TestSaveProviderComparesUnderTheLock: whether the value is already stored
// is decided against the file as it is under the lock — read after another
// writer's change, not before it. Another writer holds the lock and stores
// grok; a save of cursor, the value the file held when it started waiting,
// must still be written, and a save of what the other writer stored must not.
func TestSaveProviderComparesUnderTheLock(t *testing.T) {
	path := writeConfigFile(t, "provider = \"cursor\"\n")
	unlock, err := lockConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	other, err := readConfigAt(path)
	if err != nil {
		t.Fatal(err)
	}
	saved := make(chan error, 1)
	go func() { saved <- SaveProvider("cursor") }()
	// Long enough for the save to reach the lock and block on it.
	time.Sleep(100 * time.Millisecond)
	other["provider"] = "grok"
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
		t.Fatal("SaveProvider never got the lock")
	}
	cfg, err := readConfigAt(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg["provider"] != "cursor" || cfg["mouse"] != false {
		t.Fatalf("the save judged a stale read: %v", cfg)
	}

	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveProvider("cursor"); err != nil {
		t.Fatal(err)
	}
	if after, err := os.Stat(path); err != nil || !os.SameFile(before, after) {
		t.Fatalf("a save of the stored value replaced the file (%v)", err)
	}
}

// TestSaveProviderSerialisesWithAnotherWriter is the theme's data-loss
// property (TestSaveThemeSerialisesWithAnotherWriter) for the provider, now
// that a save may write nothing: another writer's change made while the save
// waits for the lock is kept, and the save's own lands on top of it.
func TestSaveProviderSerialisesWithAnotherWriter(t *testing.T) {
	path := writeConfigFile(t, "provider = \"cursor\"\n")
	unlock, err := lockConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	other, err := readConfigAt(path)
	if err != nil {
		t.Fatal(err)
	}
	saved := make(chan error, 1)
	go func() { saved <- SaveProvider("grok") }()
	time.Sleep(100 * time.Millisecond)
	other["theme"] = "light"
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
		t.Fatal("SaveProvider never got the lock")
	}
	cfg, err := readConfigAt(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg["provider"] != "grok" || cfg["theme"] != "light" {
		t.Fatalf("a write was lost: %v", cfg)
	}
}
