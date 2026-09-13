package tui

import (
	"errors"
	"os"
	"strings"
	"testing"
)

// TestSaveProviderPreservesUnknownKeys mirrors the theme contract: the file
// is a map craze does not own, so unknown keys survive a provider write.
func TestSaveProviderPreservesUnknownKeys(t *testing.T) {
	path := writeConfigFile(t, `provider = "cursor"
theme = "gruvbox"
mouse = false
`)
	if got := ConfigProvider(); got != "cursor" {
		t.Fatalf("provider %q", got)
	}
	if err := SaveProvider("grok"); err != nil {
		t.Fatal(err)
	}
	if got := ConfigProvider(); got != "grok" {
		t.Fatalf("provider after save %q", got)
	}
	cfg, err := readConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg["theme"] != "gruvbox" || cfg["mouse"] != false {
		t.Fatalf("other keys = %v", cfg)
	}
	assertNoConfigTempFiles(t, path)
}

// TestSaveProviderLeavesAMalformedConfigAlone: a file craze cannot parse is
// the user's, not a blank to overwrite.
func TestSaveProviderLeavesAMalformedConfigAlone(t *testing.T) {
	body := "provider = \"cursor\"\nthis is not toml [[[\n"
	path := writeConfigFile(t, body)

	err := SaveProvider("grok")
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
	if got := ConfigProvider(); got != "" {
		t.Fatalf("an unreadable config has no provider, got %q", got)
	}
	assertNoConfigTempFiles(t, path)
}

// TestConfigProviderWithoutAFile reads empty and saves through a missing dir.
func TestConfigProviderWithoutAFile(t *testing.T) {
	path := writeConfigFile(t, "")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if got := ConfigProvider(); got != "" {
		t.Fatalf("provider %q, want empty", got)
	}
	if err := SaveProvider("cursor"); err != nil {
		t.Fatal(err)
	}
	if got := ConfigProvider(); got != "cursor" {
		t.Fatalf("provider %q after the first save", got)
	}
}
