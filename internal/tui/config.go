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
)

const (
	// configDir / configName make up ~/.craze/config.toml; CRAZE_CONFIG
	// replaces the whole path.
	configDir  = ".craze"
	configName = "config.toml"
)

// ErrConfigMalformed is a config file craze could not parse. It is never
// rewritten: the keys craze cannot read are still the user's.
var ErrConfigMalformed = errors.New("malformed config")

// configPath is where the persisted settings live, or "" when there is no home
// directory to put them in.
func configPath() string {
	if p := strings.TrimSpace(os.Getenv("CRAZE_CONFIG")); p != "" {
		return p
	}
	home := homeDir()
	if home == "" {
		return ""
	}
	return filepath.Join(home, configDir, configName)
}

// readConfig decodes the file into a plain map so keys craze knows nothing
// about survive a write. A missing file is an empty config, not an error.
func readConfig() (map[string]any, error) {
	path := configPath()
	if path == "" {
		return map[string]any{}, nil
	}
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
func SaveTheme(name string) error {
	path := configPath()
	if path == "" {
		return errors.New("craze: no home directory to save the theme in")
	}
	cfg, err := readConfig()
	if err != nil {
		return fmt.Errorf("craze: not saving the theme: %w", err)
	}
	cfg["theme"] = name
	return writeConfig(path, cfg)
}

func writeConfig(path string, cfg map[string]any) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(cfg); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".craze-config-*.toml")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	// Removing the temp file is a no-op once the rename succeeded, and the one
	// thing that matters when anything below fails.
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(buf.Bytes()); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
