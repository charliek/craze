package tui

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/atomicfile"
	"github.com/charliek/craze/internal/paths"
)

const (
	// configDir / configName make up ~/.craze/config.toml; kept here (in
	// addition to internal/paths, the actual source of truth for configPath)
	// because theme_test.go asserts against the literal path.
	configDir  = ".craze"
	configName = "config.toml"
	// configLockSuffix names the sibling file SaveTheme flocks. It is never
	// renamed, so every writer locks the same inode.
	configLockSuffix = ".lock"
)

// ErrConfigMalformed is a config file craze could not parse. It is never
// rewritten: the keys craze cannot read are still the user's.
var ErrConfigMalformed = errors.New("malformed config")

// configPath is where the persisted settings live, or "" when there is no home
// directory to put them in. It defers to internal/paths, which config.go and
// internal/sessions both call so neither package depends on the other.
func configPath() string {
	return paths.ConfigPath()
}

// readConfig decodes the file into a plain map so keys craze knows nothing
// about survive a write. A missing file is an empty config, not an error.
func readConfig() (map[string]any, error) {
	path := configPath()
	if path == "" {
		return map[string]any{}, nil
	}
	return readConfigAt(path)
}

func readConfigAt(path string) (map[string]any, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, err
	}
	cfg := map[string]any{}
	if err := toml.Unmarshal(b, &cfg); err != nil {
		return nil, fmt.Errorf("%s: %w: %v", path, ErrConfigMalformed, err)
	}
	return cfg, nil
}

// ConfigTheme is the persisted theme name, or "" when there is none. A config
// craze cannot read is not worth refusing to start over, so it reads as empty
// and the caller falls back to the default.
func ConfigTheme() string {
	cfg, err := readConfig()
	if err != nil {
		return ""
	}
	name, _ := cfg["theme"].(string)
	return strings.TrimSpace(name)
}

// SaveTheme persists the theme name, keeping every other key in the file. The
// write lands in a temp file in the same directory and is renamed over the
// target, so an interrupted write cannot truncate a config.
//
// Read, modify and rename all happen under an exclusive lock on a sibling lock
// file, so two crazes cannot each read the file and then rename their own stale
// copy over the other's keys. The lock is on a file that is never renamed,
// because a lock on the config itself would be a lock on an inode the next
// rename replaces.
func SaveTheme(name string) error {
	path := configPath()
	if path == "" {
		return errors.New("craze: no home directory to save the theme in")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	unlock, err := lockConfig(path)
	if err != nil {
		return err
	}
	defer unlock()

	cfg, err := readConfigAt(path)
	if err != nil {
		return fmt.Errorf("craze: not saving the theme: %w", err)
	}
	cfg["theme"] = name
	return writeConfig(path, cfg)
}

// ConfigTerminalTitle is whether craze may set the terminal tab title
// (§3.10), defaulting to true: only an explicit `terminal_title = false`
// turns it off. A config craze cannot read defaults true rather than false —
// a broken config file is not a reason to also lose the tab title.
func ConfigTerminalTitle() bool { return configSwitch("terminal_title") }

// ConfigBackground is whether craze may set the terminal's own default
// background and text colours from the theme (§3.3), defaulting to true: only
// an explicit `background = false` turns it off. A config craze cannot read
// defaults true rather than false, exactly as the tab title does — a broken
// config file is not a reason to also lose the themed background.
func ConfigBackground() bool { return configSwitch("background") }

// ConfigHostStatus is whether craze may report its status to the terminal
// multiplexer it runs in (plan 015 §3.5), defaulting to true: only an explicit
// `host_status = false` turns it off. A config craze cannot read defaults true
// rather than false, exactly as the tab title and the themed background do.
// The host's own environment is still the gate: this only ever turns reporting
// off.
func ConfigHostStatus() bool { return configSwitch("host_status") }

// ConfigJournal is whether craze may journal its sessions (plan 020 §3.5),
// and why not when it may not. It defaults to on, like the switches above,
// but it is a privacy switch — a journal holds prompts and tool output — so
// it fails closed where they fail open: on only when the config parsed and
// the `journal` key is absent or exactly `true` (a missing file parses as an
// empty config). An explicit `journal = false` is off with no reason given,
// since that is the user's own choice; anything else off is a mistake the
// user should hear about, so why says what it was, in a few words for a
// one-line diagnostic: a file that could not be read or parsed, or a value
// that is not a bool (`journal = "false"` is a string, and is off).
func ConfigJournal() (on bool, why string) {
	cfg, err := readConfig()
	switch {
	case errors.Is(err, ErrConfigMalformed):
		return false, "config.toml could not be parsed"
	case err != nil:
		return false, "config.toml could not be read"
	}
	v, ok := cfg["journal"]
	if !ok {
		return true, ""
	}
	on, ok = v.(bool)
	if !ok {
		return false, "config.toml journal is not a bool"
	}
	return on, ""
}

// ConfigCompatClaude is the [compat.claude] table — which classes of Claude's
// own content a native session reads (plan 022 §3.5) — and the lines to say
// about it. Every key defaults to true, so an absent table, a partial one and
// a config craze cannot read all mean "read everything", and what comes back
// is the set the user turned off (agent.ClaudeCompat).
//
// A value that is not a bool is the default *and* a line, which is where this
// parts company with configSwitch above: those keys govern a decoration — a
// tab title, a themed background — and a typo in one is not worth a word. A
// typo here silently strips the user's own instructions or half their slash
// menu out of a session, which looks like craze having lost them rather than
// like a config file having a string where a bool goes. It is not a privacy
// switch either, so unlike ConfigJournal it fails open: a config file craze
// cannot parse must not also mean a model that no longer knows how this
// repository builds.
func ConfigCompatClaude() (agent.ClaudeCompat, []string) {
	var c agent.ClaudeCompat
	cfg, err := readConfig()
	if err != nil {
		return c, nil
	}
	table, ok := cfg["compat"].(map[string]any)
	if !ok {
		return c, nil
	}
	claude, ok := table["claude"].(map[string]any)
	if !ok {
		return c, nil
	}
	// Read in the order the table's own documentation lists them, so two
	// mistakes are always reported in the same order.
	off := []struct {
		key string
		to  *bool
	}{
		{"instructions", &c.NoInstructions},
		{"rules", &c.NoRules},
		{"skills", &c.NoSkills},
		{"commands", &c.NoCommands},
		{"plugins", &c.NoPlugins},
		// Personas, the sub-agent types (plan 026 §3.4): off removes them from
		// all three of their sources, a plugin's included.
		{"agents", &c.NoAgents},
	}
	var why []string
	for _, k := range off {
		v, present := claude[k.key]
		if !present {
			continue
		}
		on, isBool := v.(bool)
		if !isBool {
			why = append(why, fmt.Sprintf("config.toml compat.claude.%s is not a bool; leaving it on", k.key))
			continue
		}
		*k.to = !on
	}
	return c, why
}

// configSwitch reads a default-on bool key: only a literal `key = false` turns
// it off, and a missing file, an unparseable one, a missing key or a value
// that is not a bool all read as true.
func configSwitch(key string) bool {
	cfg, err := readConfig()
	if err != nil {
		return true
	}
	on, ok := cfg[key].(bool)
	if !ok {
		return true
	}
	return on
}

// ConfigProvider is the persisted provider id, or "" when there is none. An
// unreadable config reads as empty, like the theme.
func ConfigProvider() string {
	cfg, err := readConfig()
	if err != nil {
		return ""
	}
	name, _ := cfg["provider"].(string)
	return strings.TrimSpace(name)
}

// SaveProvider persists the provider id with the same lock and atomic write
// as the theme. The TUI writes it on startedMsg and craze prompt writes it
// after Start; the session itself never persists.
func SaveProvider(name string) error {
	path := configPath()
	if path == "" {
		return errors.New("craze: no home directory to save the provider in")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	unlock, err := lockConfig(path)
	if err != nil {
		return err
	}
	defer unlock()

	cfg, err := readConfigAt(path)
	if err != nil {
		return fmt.Errorf("craze: not saving the provider: %w", err)
	}
	cfg["provider"] = name
	return writeConfig(path, cfg)
}

// lockConfig takes the exclusive lock that covers one read-modify-write. A
// lock craze cannot take (a read-only directory, say) is not a reason to
// refuse to save: the write itself still reports that, so the open/flock
// error atomicfile.Lock now returns is swallowed here, exactly as it always
// was before Lock moved into its own package.
func lockConfig(path string) (func(), error) {
	unlock, err := atomicfile.Lock(path + configLockSuffix)
	if err != nil {
		return unlock, nil //nolint:nilerr // never block a save on the lock; the write below reports the real problem
	}
	return unlock, nil
}

func writeConfig(path string, cfg map[string]any) error {
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(cfg); err != nil {
		return err
	}
	return atomicfile.Write(path, buf.Bytes(), 0o600)
}
