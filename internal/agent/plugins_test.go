package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charliek/craze/internal/acp"
)

// cursorScan is the scan cursor runs. Tests that want one source at a time
// build a PluginScan of their own.
var cursorScan = PluginScan{Dirs: true, CursorCache: true, ClaudePlugins: true}

// writeTree lays out a fixture: root is created even when files is empty, and
// each key is a slash-separated path under it.
func writeTree(t *testing.T, root string, files map[string]string) string {
	t.Helper()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	for rel, body := range files {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// commandFile is the shape of a plugin command: a description in the
// frontmatter and a body underneath.
func commandFile(desc, body string) string {
	return "---\ndescription: " + desc + "\n---\n" + body + "\n"
}

// cachePlugin lays out one cached plugin version under home, with or without
// the .cache-complete sentinel that stands in for "the install finished".
func cachePlugin(t *testing.T, home, market, plugin, version string, complete bool, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(home, ".cursor", "plugins", "cache", market, plugin, version)
	writeTree(t, dir, files)
	if complete {
		writeTree(t, dir, map[string]string{".cache-complete": ""})
	}
	return dir
}

// touch stamps a directory so a test can decide which cached version is the
// newest without sleeping for a filesystem clock tick.
func touch(t *testing.T, dir string, when time.Time) {
	t.Helper()
	if err := os.Chtimes(dir, when, when); err != nil {
		t.Fatal(err)
	}
}

// qualifiedNames is what a discovery run found, as the plugin:name keys that
// decide dedupe, in source order.
func qualifiedNames(entries []PluginEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Plugin+":"+e.Name)
	}
	return out
}

// wantNames pins a discovery run's entries, in order, as their qualified
// keys. No wants at all is the assertion that the run found nothing.
func wantNames(t *testing.T, got []PluginEntry, want ...string) {
	t.Helper()
	if names := qualifiedNames(got); strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("entries %v, want %v", names, want)
	}
}

// TestPluginCacheVersionChoice covers everything the cursor cache can look
// like: two complete versions, an incomplete newest, a plugin whose only
// version never finished, and the empty plugin directory a failed install
// leaves behind.
func TestPluginCacheVersionChoice(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	t.Run("newest complete version wins", func(t *testing.T) {
		home := t.TempDir()
		older := cachePlugin(t, home, "mkt", "p", "aaa", true, map[string]string{
			"commands/from-aaa.md": commandFile("a", "body"),
		})
		newer := cachePlugin(t, home, "mkt", "p", "bbb", true, map[string]string{
			"commands/from-bbb.md": commandFile("b", "body"),
		})
		touch(t, older, base)
		touch(t, newer, base.Add(time.Hour))
		wantNames(t, DiscoverPlugins(cursorScan, t.TempDir(), home, nil), "p:from-bbb")
	})

	t.Run("mtime tie goes to the lexically first version", func(t *testing.T) {
		home := t.TempDir()
		first := cachePlugin(t, home, "mkt", "p", "aaa", true, map[string]string{
			"commands/from-aaa.md": commandFile("a", "body"),
		})
		second := cachePlugin(t, home, "mkt", "p", "bbb", true, map[string]string{
			"commands/from-bbb.md": commandFile("b", "body"),
		})
		touch(t, first, base)
		touch(t, second, base)
		wantNames(t, DiscoverPlugins(cursorScan, t.TempDir(), home, nil), "p:from-aaa")
	})

	t.Run("a version without the sentinel is ignored", func(t *testing.T) {
		home := t.TempDir()
		complete := cachePlugin(t, home, "mkt", "p", "aaa", true, map[string]string{
			"commands/from-aaa.md": commandFile("a", "body"),
		})
		half := cachePlugin(t, home, "mkt", "p", "bbb", false, map[string]string{
			"commands/from-bbb.md": commandFile("b", "body"),
		})
		touch(t, complete, base)
		touch(t, half, base.Add(time.Hour))
		wantNames(t, DiscoverPlugins(cursorScan, t.TempDir(), home, nil), "p:from-aaa")
	})

	t.Run("a plugin with no complete version is skipped", func(t *testing.T) {
		home := t.TempDir()
		cachePlugin(t, home, "mkt", "p", "aaa", false, map[string]string{
			"commands/from-aaa.md": commandFile("a", "body"),
		})
		wantNames(t, DiscoverPlugins(cursorScan, t.TempDir(), home, nil))
	})

	t.Run("a plugin with no version directory at all is skipped", func(t *testing.T) {
		home := t.TempDir()
		// The shape a failed install leaves behind on a real machine.
		writeTree(t, filepath.Join(home, ".cursor", "plugins", "cache", "mkt", "flows"), nil)
		cachePlugin(t, home, "mkt", "ok", "v1", true, map[string]string{
			"commands/ok-cmd.md": commandFile("ok", "body"),
		})
		wantNames(t, DiscoverPlugins(cursorScan, t.TempDir(), home, nil), "ok:ok-cmd")
	})
}

// claudeFixture writes Claude Code's install database and the three settings
// files the dl rule reads. installs is keyed by the full "<id>@<marketplace>"
// spelling, exactly as the real file is.
type claudeFixture struct {
	installs  map[string][]claudeInstall
	user      map[string]bool
	workspace map[string]bool
	local     map[string]bool
	rawDB     string
}

func (f claudeFixture) write(t *testing.T, home, workspace string) {
	t.Helper()
	dbPath := filepath.Join(home, ".claude", "plugins", "installed_plugins.json")
	if f.rawDB != "" {
		writeTree(t, filepath.Dir(dbPath), map[string]string{"installed_plugins.json": f.rawDB})
	} else if f.installs != nil {
		writeJSON(t, dbPath, map[string]any{"version": 2, "plugins": f.installs})
	}
	if f.user != nil {
		writeJSON(t, filepath.Join(home, ".claude", "settings.json"), map[string]any{"enabledPlugins": f.user})
	}
	if f.workspace != nil {
		writeJSON(t, filepath.Join(workspace, ".claude", "settings.json"), map[string]any{"enabledPlugins": f.workspace})
	}
	if f.local != nil {
		writeJSON(t, filepath.Join(workspace, ".claude", "settings.local.json"), map[string]any{"enabledPlugins": f.local})
	}
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	writeTree(t, filepath.Dir(path), map[string]string{filepath.Base(path): string(data)})
}

// installedPlugin lays out one install directory shipping a single command
// named after the scope, so the table below can say which entry was taken.
func installedPlugin(t *testing.T, dir, command string) string {
	t.Helper()
	return writeTree(t, dir, map[string]string{
		"commands/" + command + ".md": commandFile("from "+command, "body"),
	})
}

// TestClaudePluginEnablement is cursor's dl rule as a table: the workspace
// settings decide first and only silence there falls through to the user
// settings, an absent key is never enabled, and the install has to match the
// scope the settings spoke for.
func TestClaudePluginEnablement(t *testing.T) {
	scan := PluginScan{ClaudePlugins: true}
	const key = "alpha@mkt"

	cases := []struct {
		name string
		fix  func(t *testing.T, home, workspace string) claudeFixture
		want []string
	}{
		{
			name: "user true takes the user entry",
			fix: func(t *testing.T, home, workspace string) claudeFixture {
				return claudeFixture{
					installs: map[string][]claudeInstall{key: {
						{Scope: "user", InstallPath: installedPlugin(t, filepath.Join(home, "user-install"), "user-cmd")},
					}},
					user: map[string]bool{key: true},
				}
			},
			want: []string{"alpha:user-cmd"},
		},
		{
			name: "a key no settings file mentions is not enabled",
			fix: func(t *testing.T, home, workspace string) claudeFixture {
				return claudeFixture{
					installs: map[string][]claudeInstall{key: {
						{Scope: "user", InstallPath: installedPlugin(t, filepath.Join(home, "user-install"), "user-cmd")},
					}},
					user: map[string]bool{"other@mkt": true},
				}
			},
		},
		{
			name: "workspace false beats user true",
			fix: func(t *testing.T, home, workspace string) claudeFixture {
				return claudeFixture{
					installs: map[string][]claudeInstall{key: {
						{Scope: "user", InstallPath: installedPlugin(t, filepath.Join(home, "user-install"), "user-cmd")},
					}},
					user:      map[string]bool{key: true},
					workspace: map[string]bool{key: false},
				}
			},
		},
		{
			name: "workspace true takes the matching project entry over user false",
			fix: func(t *testing.T, home, workspace string) claudeFixture {
				return claudeFixture{
					installs: map[string][]claudeInstall{key: {
						{Scope: "user", InstallPath: installedPlugin(t, filepath.Join(home, "user-install"), "user-cmd")},
						{Scope: "project", ProjectPath: workspace, InstallPath: installedPlugin(t, filepath.Join(home, "project-install"), "project-cmd")},
					}},
					user:      map[string]bool{key: false},
					workspace: map[string]bool{key: true},
				}
			},
			want: []string{"alpha:project-cmd"},
		},
		{
			name: "workspace true takes a local entry too",
			fix: func(t *testing.T, home, workspace string) claudeFixture {
				return claudeFixture{
					installs: map[string][]claudeInstall{key: {
						{Scope: "local", ProjectPath: workspace, InstallPath: installedPlugin(t, filepath.Join(home, "local-install"), "local-cmd")},
					}},
					workspace: map[string]bool{key: true},
				}
			},
			want: []string{"alpha:local-cmd"},
		},
		{
			name: "workspace true with a project entry for another workspace is nothing",
			fix: func(t *testing.T, home, workspace string) claudeFixture {
				return claudeFixture{
					installs: map[string][]claudeInstall{key: {
						{Scope: "project", ProjectPath: filepath.Join(home, "elsewhere"), InstallPath: installedPlugin(t, filepath.Join(home, "project-install"), "project-cmd")},
					}},
					workspace: map[string]bool{key: true},
				}
			},
		},
		{
			name: "workspace true with only a user entry is nothing",
			fix: func(t *testing.T, home, workspace string) claudeFixture {
				return claudeFixture{
					installs: map[string][]claudeInstall{key: {
						{Scope: "user", InstallPath: installedPlugin(t, filepath.Join(home, "user-install"), "user-cmd")},
					}},
					user:      map[string]bool{key: true},
					workspace: map[string]bool{key: true},
				}
			},
		},
		{
			name: "settings.local.json overrides settings.json per key",
			fix: func(t *testing.T, home, workspace string) claudeFixture {
				return claudeFixture{
					installs: map[string][]claudeInstall{key: {
						{Scope: "project", ProjectPath: workspace, InstallPath: installedPlugin(t, filepath.Join(home, "project-install"), "project-cmd")},
					}},
					workspace: map[string]bool{key: false},
					local:     map[string]bool{key: true},
				}
			},
			want: []string{"alpha:project-cmd"},
		},
		{
			name: "a relative installPath is skipped",
			fix: func(t *testing.T, home, workspace string) claudeFixture {
				installedPlugin(t, filepath.Join(home, "user-install"), "user-cmd")
				return claudeFixture{
					installs: map[string][]claudeInstall{key: {{Scope: "user", InstallPath: "user-install"}}},
					user:     map[string]bool{key: true},
				}
			},
		},
		{
			name: "an installPath that is not there is skipped",
			fix: func(t *testing.T, home, workspace string) claudeFixture {
				return claudeFixture{
					installs: map[string][]claudeInstall{key: {{Scope: "user", InstallPath: filepath.Join(home, "gone")}}},
					user:     map[string]bool{key: true},
				}
			},
		},
		{
			name: "a malformed database contributes nothing",
			fix: func(t *testing.T, home, workspace string) claudeFixture {
				installedPlugin(t, filepath.Join(home, "user-install"), "user-cmd")
				return claudeFixture{
					rawDB: `{"plugins": [not json`,
					user:  map[string]bool{key: true},
				}
			},
		},
		{
			name: "malformed settings enable nothing",
			fix: func(t *testing.T, home, workspace string) claudeFixture {
				writeTree(t, filepath.Join(home, ".claude"), map[string]string{"settings.json": "{not json"})
				return claudeFixture{
					installs: map[string][]claudeInstall{key: {
						{Scope: "user", InstallPath: installedPlugin(t, filepath.Join(home, "user-install"), "user-cmd")},
					}},
				}
			},
		},
		{
			name: "ids come out in lexical order, not map order",
			fix: func(t *testing.T, home, workspace string) claudeFixture {
				installs := map[string][]claudeInstall{}
				enabled := map[string]bool{}
				for _, id := range []string{"charlie", "alpha", "bravo"} {
					k := id + "@mkt"
					installs[k] = []claudeInstall{{
						Scope:       "user",
						InstallPath: installedPlugin(t, filepath.Join(home, id), id+"-cmd"),
					}}
					enabled[k] = true
				}
				return claudeFixture{installs: installs, user: enabled}
			},
			want: []string{"alpha:alpha-cmd", "bravo:bravo-cmd", "charlie:charlie-cmd"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home, workspace := t.TempDir(), t.TempDir()
			tc.fix(t, home, workspace).write(t, home, workspace)
			wantNames(t, DiscoverPlugins(scan, workspace, home, nil), tc.want...)
		})
	}
}

// TestPluginDatabaseWithoutTheVersionWrapper reads the flat shape: the file is
// the map itself rather than {"version": 2, "plugins": {…}}.
func TestPluginDatabaseWithoutTheVersionWrapper(t *testing.T) {
	home, workspace := t.TempDir(), t.TempDir()
	install := installedPlugin(t, filepath.Join(home, "user-install"), "user-cmd")
	writeJSON(t, filepath.Join(home, ".claude", "plugins", "installed_plugins.json"), map[string]any{
		"alpha@mkt": []map[string]string{{"scope": "user", "installPath": install}},
	})
	writeJSON(t, filepath.Join(home, ".claude", "settings.json"), map[string]any{
		"enabledPlugins": map[string]bool{"alpha@mkt": true},
	})
	wantNames(t, DiscoverPlugins(PluginScan{ClaudePlugins: true}, workspace, home, nil), "alpha:user-cmd")
}

// TestPluginConfigFilesAreCapped pins that the two JSON files craze does not
// own are refused past the per-file ceiling rather than allocated first. They
// live in a cache anything may write to, and the old os.ReadFile sized its
// buffer from the file before there was anything to reject.
func TestPluginConfigFilesAreCapped(t *testing.T) {
	pad := strings.Repeat("x", maxPluginBytes)

	t.Run("an oversized install database contributes nothing", func(t *testing.T) {
		home, workspace := t.TempDir(), t.TempDir()
		install := installedPlugin(t, filepath.Join(home, "user-install"), "user-cmd")
		claudeFixture{
			rawDB: `{"pad": "` + pad + `", "plugins": {"alpha@mkt": [{"scope": "user", "installPath": "` + install + `"}]}}`,
			user:  map[string]bool{"alpha@mkt": true},
		}.write(t, home, workspace)
		wantNames(t, DiscoverPlugins(PluginScan{ClaudePlugins: true}, workspace, home, nil))
	})

	t.Run("oversized settings enable nothing", func(t *testing.T) {
		home, workspace := t.TempDir(), t.TempDir()
		install := installedPlugin(t, filepath.Join(home, "user-install"), "user-cmd")
		writeJSON(t, filepath.Join(home, ".claude", "plugins", "installed_plugins.json"), map[string]any{
			"plugins": map[string]any{"alpha@mkt": []map[string]string{{"scope": "user", "installPath": install}}},
		})
		writeTree(t, filepath.Join(home, ".claude"), map[string]string{
			"settings.json": `{"pad": "` + pad + `", "enabledPlugins": {"alpha@mkt": true}}`,
		})
		wantNames(t, DiscoverPlugins(PluginScan{ClaudePlugins: true}, workspace, home, nil))
	})
}

// TestPluginSourceOrder pins which source wins a qualified key, and that the
// same plugin id reached twice contributes its entries once.
func TestPluginSourceOrder(t *testing.T) {
	t.Run("dirs beat the cache beat Claude", func(t *testing.T) {
		home, workspace := t.TempDir(), t.TempDir()
		dir := writeTree(t, filepath.Join(workspace, "git-commands"), map[string]string{
			"commands/watch-pr.md": commandFile("from the plugin dir", "body"),
		})
		cachePlugin(t, home, "cc-plugins", "git-commands", "v1", true, map[string]string{
			"commands/watch-pr.md": commandFile("from the cursor cache", "body"),
		})
		claudeFixture{
			installs: map[string][]claudeInstall{"git-commands@cc-plugins": {{
				Scope: "user",
				InstallPath: writeTree(t, filepath.Join(home, "claude-install"), map[string]string{
					"commands/watch-pr.md": commandFile("from Claude", "body"),
				}),
			}}},
			user: map[string]bool{"git-commands@cc-plugins": true},
		}.write(t, home, workspace)

		got := DiscoverPlugins(cursorScan, workspace, home, []string{dir})
		wantNames(t, got, "git-commands:watch-pr")
		if got[0].Description != "from the plugin dir" {
			t.Fatalf("description %q", got[0].Description)
		}
		if got[0].Root != dir {
			t.Fatalf("root %q, want %q", got[0].Root, dir)
		}

		// Drop the --plugin-dir and the cursor cache becomes the winner.
		got = DiscoverPlugins(cursorScan, workspace, home, nil)
		wantNames(t, got, "git-commands:watch-pr")
		if got[0].Description != "from the cursor cache" {
			t.Fatalf("description %q", got[0].Description)
		}
	})

	t.Run("one plugin id under two marketplaces collapses", func(t *testing.T) {
		home := t.TempDir()
		cachePlugin(t, home, "aaa-market", "shared", "v1", true, map[string]string{
			"commands/dup.md":  commandFile("first market", "body"),
			"commands/only.md": commandFile("only here", "body"),
		})
		cachePlugin(t, home, "zzz-market", "shared", "v1", true, map[string]string{
			"commands/dup.md":   commandFile("second market", "body"),
			"commands/extra.md": commandFile("only in the second", "body"),
		})
		got := DiscoverPlugins(cursorScan, t.TempDir(), home, nil)
		wantNames(t, got, "shared:dup", "shared:only", "shared:extra")
		if got[0].Description != "first market" {
			t.Fatalf("description %q", got[0].Description)
		}
	})
}

// TestPluginDirs covers the --plugin-dir half: where a relative path resolves,
// what a missing one does, what a symlink means, and the manifest name.
func TestPluginDirs(t *testing.T) {
	scan := PluginScan{Dirs: true}

	t.Run("a relative dir resolves against the workspace", func(t *testing.T) {
		workspace := t.TempDir()
		writeTree(t, filepath.Join(workspace, "probe-plugin"), map[string]string{
			"commands/probe-echo.md": commandFile("echo", "body"),
		})
		wantNames(t, DiscoverPlugins(scan, workspace, "", []string{"probe-plugin"}), "probe-plugin:probe-echo")
	})

	t.Run("a missing dir is a diagnostic, not an error", func(t *testing.T) {
		workspace := t.TempDir()
		var diag bytes.Buffer
		got := discoverPlugins(scan, workspace, "", []string{"nope"}, func(msg string) {
			fmt.Fprintln(&diag, msg)
		})
		wantNames(t, got)
		want := "plugin dir skipped: " + filepath.Join(workspace, "nope")
		if strings.TrimSpace(diag.String()) != want {
			t.Fatalf("diagnostic %q, want %q", diag.String(), want)
		}
	})

	t.Run("a file where a dir was named is a diagnostic too", func(t *testing.T) {
		workspace := t.TempDir()
		writeTree(t, workspace, map[string]string{"not-a-dir": "x"})
		var diag bytes.Buffer
		got := discoverPlugins(scan, workspace, "", []string{"not-a-dir"}, func(msg string) {
			fmt.Fprintln(&diag, msg)
		})
		wantNames(t, got)
		if !strings.Contains(diag.String(), "plugin dir skipped") {
			t.Fatalf("diagnostic %q", diag.String())
		}
	})

	t.Run("a duplicate dir is collapsed", func(t *testing.T) {
		workspace := t.TempDir()
		dir := writeTree(t, filepath.Join(workspace, "probe-plugin"), map[string]string{
			"commands/probe-echo.md": commandFile("echo", "body"),
		})
		wantNames(t, DiscoverPlugins(scan, workspace, "", []string{"probe-plugin", dir}), "probe-plugin:probe-echo")
	})

	t.Run("a symlinked dir is followed and symlinks inside are not", func(t *testing.T) {
		workspace := t.TempDir()
		real := writeTree(t, filepath.Join(workspace, "real"), map[string]string{
			"commands/kept.md":              commandFile("kept", "body"),
			"elsewhere/linked.md":           commandFile("linked", "body"),
			"skills/kept-skill/SKILL.md":    "---\nname: kept-skill\ndescription: kept\n---\nbody\n",
			"outside/linked-skill/SKILL.md": "---\nname: linked-skill\ndescription: linked\n---\nbody\n",
		})
		if err := os.Symlink(filepath.Join(real, "elsewhere", "linked.md"), filepath.Join(real, "commands", "linked.md")); err != nil {
			t.Skipf("symlink: %v", err)
		}
		if err := os.Symlink(filepath.Join(real, "outside", "linked-skill"), filepath.Join(real, "skills", "linked-skill")); err != nil {
			t.Skipf("symlink: %v", err)
		}
		link := filepath.Join(workspace, "link")
		if err := os.Symlink(real, link); err != nil {
			t.Skipf("symlink: %v", err)
		}
		wantNames(t, DiscoverPlugins(scan, workspace, "", []string{link}), "link:kept", "link:kept-skill")
	})

	t.Run("a symlinked commands or skills directory is refused", func(t *testing.T) {
		// The link is content of the plugin, not the user naming it, and
		// following it would read a tree outside the plugin into a prompt.
		workspace := t.TempDir()
		outside := writeTree(t, filepath.Join(workspace, "outside"), map[string]string{
			"secret.md":             commandFile("secret", "body"),
			"secret-skill/SKILL.md": "---\nname: secret-skill\ndescription: s\n---\nbody\n",
		})
		root := writeTree(t, filepath.Join(workspace, "p"), map[string]string{"README.md": "x\n"})
		if err := os.Symlink(outside, filepath.Join(root, "commands")); err != nil {
			t.Skipf("symlink: %v", err)
		}
		if err := os.Symlink(outside, filepath.Join(root, "skills")); err != nil {
			t.Skipf("symlink: %v", err)
		}
		wantNames(t, DiscoverPlugins(scan, workspace, "", []string{root}))
	})

	t.Run("an oversized manifest leaves the id as the directory name", func(t *testing.T) {
		workspace := t.TempDir()
		writeTree(t, filepath.Join(workspace, "dir-name"), map[string]string{
			".claude-plugin/plugin.json": `{"name": "manifest-name", "pad": "` + strings.Repeat("x", maxPluginBytes) + `"}`,
			"commands/cmd.md":            commandFile("c", "body"),
		})
		wantNames(t, DiscoverPlugins(scan, workspace, "", []string{"dir-name"}), "dir-name:cmd")
	})

	t.Run("the manifest names the plugin", func(t *testing.T) {
		workspace := t.TempDir()
		writeTree(t, filepath.Join(workspace, "dir-name"), map[string]string{
			".claude-plugin/plugin.json": `{"name": "manifest-name"}`,
			"commands/cmd.md":            commandFile("c", "body"),
		})
		wantNames(t, DiscoverPlugins(scan, workspace, "", []string{"dir-name"}), "manifest-name:cmd")
	})

	t.Run("a dir with neither commands nor skills gives nothing", func(t *testing.T) {
		workspace := t.TempDir()
		writeTree(t, filepath.Join(workspace, "empty"), map[string]string{"README.md": "# nothing here\n"})
		wantNames(t, DiscoverPlugins(scan, workspace, "", []string{"empty"}))
	})
}

// discoverDir lays the files out as the one plugin "p" under a fresh
// workspace and discovers it through --plugin-dir, which is the shortest route
// from a fixture to a set of entries.
func discoverDir(t *testing.T, files map[string]string) []PluginEntry {
	t.Helper()
	workspace := t.TempDir()
	writeTree(t, filepath.Join(workspace, "p"), files)
	return DiscoverPlugins(PluginScan{Dirs: true}, workspace, "", []string{"p"})
}

// TestPluginEntryParsing is the naming, description and body table for both
// kinds, including the rules that drop an entry outright.
func TestPluginEntryParsing(t *testing.T) {
	scan := PluginScan{Dirs: true}

	t.Run("a command is named by its frontmatter, else its stem", func(t *testing.T) {
		got := discoverDir(t, map[string]string{
			"commands/stem-name.md": commandFile("by stem", "stem body"),
			"commands/zz.md":        "---\nname: from-frontmatter\ndescription: by frontmatter\n---\nfm body\n",
		})
		wantNames(t, got, "p:stem-name", "p:from-frontmatter")
		if got[0].Body != "stem body" || got[1].Body != "fm body" {
			t.Fatalf("bodies %q %q", got[0].Body, got[1].Body)
		}
		if got[0].Kind != PluginKindCommand {
			t.Fatalf("kind %q", got[0].Kind)
		}
	})

	t.Run("a skill is named by its frontmatter, else its directory", func(t *testing.T) {
		got := discoverDir(t, map[string]string{
			"skills/dir-name/SKILL.md": "---\ndescription: by directory\n---\nbody\n",
			"skills/zz/SKILL.md":       "---\nname: Skill  From Frontmatter\ndescription: normalised\n---\nbody\n",
		})
		wantNames(t, got, "p:dir-name", "p:skill-from-frontmatter")
		if got[0].Kind != PluginKindSkill {
			t.Fatalf("kind %q", got[0].Kind)
		}
	})

	t.Run("a skill body is the whole file, frontmatter included", func(t *testing.T) {
		const file = "---\nname: whole\ndescription: d\n---\nthe body\n"
		got := discoverDir(t, map[string]string{"skills/whole/SKILL.md": file})
		wantNames(t, got, "p:whole")
		if got[0].Body != strings.TrimSpace(file) {
			t.Fatalf("body %q", got[0].Body)
		}
	})

	t.Run("user-invocable false hides a skill", func(t *testing.T) {
		got := discoverDir(t, map[string]string{
			"skills/hidden/SKILL.md": "---\nname: hidden\ndescription: d\nuser-invocable: false\n---\nbody\n",
			"skills/shown/SKILL.md":  "---\nname: shown\ndescription: d\n---\nbody\n",
		})
		wantNames(t, got, "p:shown")
	})

	// The heading wins wherever it is in the body, and the first non-empty
	// line is the fallback only when the body has no heading at all — cursor's
	// own order.
	t.Run("a skill description falls back to the heading, then the first line", func(t *testing.T) {
		got := discoverDir(t, map[string]string{
			"skills/heading/SKILL.md": "---\nname: heading\n---\n\nprose before it\n\n## Heading Wins\n\nrest\n",
			"skills/first/SKILL.md":   "---\nname: first\n---\n\nthe first line\n\nmore prose\n",
		})
		wantNames(t, got, "p:first", "p:heading")
		if got[0].Description != "the first line" {
			t.Fatalf("first %q", got[0].Description)
		}
		if got[1].Description != "Heading Wins" {
			t.Fatalf("heading %q", got[1].Description)
		}
	})

	t.Run("a command beats a skill of the same name in one plugin", func(t *testing.T) {
		got := discoverDir(t, map[string]string{
			"commands/dup.md":     commandFile("the command", "command body"),
			"skills/dup/SKILL.md": "---\nname: dup\ndescription: the skill\n---\nbody\n",
		})
		wantNames(t, got, "p:dup")
		if got[0].Kind != PluginKindCommand {
			t.Fatalf("kind %q", got[0].Kind)
		}
	})

	t.Run("an empty body is dropped", func(t *testing.T) {
		got := discoverDir(t, map[string]string{
			"commands/empty.md": "---\ndescription: nothing follows\n---\n\n   \n",
			"commands/kept.md":  commandFile("kept", "body"),
		})
		wantNames(t, got, "p:kept")
	})

	t.Run("a name outside the identifier class is dropped", func(t *testing.T) {
		got := discoverDir(t, map[string]string{
			"commands/colon.md": "---\nname: has:colon\ndescription: d\n---\nbody\n",
			"commands/dot.md":   "---\nname: has.dot\ndescription: d\n---\nbody\n",
			"commands/space.md": "---\nname: \"has space\"\ndescription: d\n---\nbody\n",
			"commands/ok.md":    commandFile("ok", "body"),
		})
		wantNames(t, got, "p:ok")
	})

	t.Run("a plugin id outside the identifier class skips the whole plugin", func(t *testing.T) {
		workspace := t.TempDir()
		for _, id := range []string{"has:colon", "has.dot", "has space"} {
			writeTree(t, filepath.Join(workspace, "roots", id), map[string]string{
				"commands/cmd.md": commandFile("d", "body"),
			})
		}
		writeTree(t, filepath.Join(workspace, "roots", "ok-id"), map[string]string{
			"commands/cmd.md": commandFile("d", "body"),
		})
		dirs := []string{
			filepath.Join("roots", "has:colon"),
			filepath.Join("roots", "has.dot"),
			filepath.Join("roots", "has space"),
			filepath.Join("roots", "ok-id"),
		}
		wantNames(t, DiscoverPlugins(scan, workspace, "", dirs), "ok-id:cmd")
	})

	t.Run("only top-level .md files under commands are read", func(t *testing.T) {
		got := discoverDir(t, map[string]string{
			"commands/top.md":        commandFile("top", "body"),
			"commands/nested/sub.md": commandFile("nested", "body"),
			"commands/other.txt":     commandFile("other", "body"),
			"skills/deep/one/two.md": "---\nname: deep\n---\nbody\n",
		})
		wantNames(t, got, "p:top")
	})
}

// TestPluginBudgets pins the two ceilings: how many files one scan reads and
// how big one of them may be.
func TestPluginBudgets(t *testing.T) {
	t.Run("the file budget stops the scan", func(t *testing.T) {
		files := make(map[string]string, maxPluginFiles+10)
		for i := 0; i < maxPluginFiles+10; i++ {
			files[fmt.Sprintf("commands/cmd-%04d.md", i)] = commandFile("d", "body")
		}
		if got := discoverDir(t, files); len(got) != maxPluginFiles {
			t.Fatalf("read %d entries, want the budget %d", len(got), maxPluginFiles)
		}
	})

	t.Run("an oversized file is skipped", func(t *testing.T) {
		wantNames(t, discoverDir(t, map[string]string{
			"commands/huge.md": commandFile("huge", strings.Repeat("x", maxPluginBytes+1)),
			"commands/kept.md": commandFile("kept", "body"),
		}), "p:kept")
	})
}

// TestSkillDescriptionFoldedScalar covers the folded and literal description
// scalars for both callers: a plugin skill and a disk skill. Six of the plugin
// entries cached on a real machine are written this way, and without the rule
// their rows would read ">-".
func TestSkillDescriptionFoldedScalar(t *testing.T) {
	cases := []struct {
		name string
		fm   string
		want string
	}{
		{
			name: "folded",
			fm:   "description: >-\n  first half\n  second half\n",
			want: "first half second half",
		},
		{
			name: "folded without the chomp",
			fm:   "description: >\n  first half\n  second half\n",
			want: "first half second half",
		},
		{
			name: "literal",
			fm:   "description: |\n  first line\n  second line\n",
			want: "first line\nsecond line",
		},
		{
			name: "literal with the chomp",
			fm:   "description: |-\n  only line\n",
			want: "only line",
		},
		{
			name: "a following top-level key ends the block",
			fm:   "description: >-\n  folded text\nuser-invocable: true\n",
			want: "folded text",
		},
		{
			name: "a blank line inside the block is not the end of it",
			fm:   "description: >-\n  before\n\n  after\n",
			want: "before after",
		},
		{
			name: "a plain scalar is untouched",
			fm:   "description: plain text\n",
			want: "plain text",
		},
		{
			// A quoted marker is a one-character description, not a block:
			// reading it as one would eat the indented line under it.
			name: "a quoted marker is not a block",
			fm:   "description: \">\"\n  not part of it\n",
			want: ">",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			file := "---\nname: folded-skill\n" + tc.fm + "---\nbody\n"

			got := discoverDir(t, map[string]string{"skills/folded-skill/SKILL.md": file})
			if len(got) != 1 || got[0].Description != tc.want {
				t.Fatalf("plugin skill description %q, want %q", qualifiedNames(got), tc.want)
			}

			disk := t.TempDir()
			writeSkill(t, filepath.Join(disk, ".cursor", "skills", "folded-skill", "SKILL.md"), file)
			skills := DiscoverSkills(CursorProvider(), disk, "")
			if len(skills) != 1 || skills[0].Description != tc.want {
				t.Fatalf("disk skill description %+v, want %q", skills, tc.want)
			}
		})
	}
}

// TestGrokDiscoversNoPlugins pins the zero PluginScan: grok advertises its
// plugin skills over ACP and expands them itself, so craze walks nothing for it
// and a --plugin-dir handed to it changes nothing.
func TestGrokDiscoversNoPlugins(t *testing.T) {
	home, workspace := t.TempDir(), t.TempDir()
	dir := writeTree(t, filepath.Join(workspace, "probe-plugin"), map[string]string{
		"commands/probe-echo.md": commandFile("echo", "body"),
	})
	cachePlugin(t, home, "mkt", "cached", "v1", true, map[string]string{
		"commands/cached-cmd.md": commandFile("cached", "body"),
	})
	wantNames(t, DiscoverPlugins(GrokProvider().PluginScan(), workspace, home, []string{dir}))
	if got := DiscoverPlugins(CursorProvider().PluginScan(), workspace, home, []string{dir}); len(got) != 2 {
		t.Fatalf("cursor found %v, want both", qualifiedNames(got))
	}
}

// TestGrokSessionNotesIgnoredPluginDirs pins the other half of grok's zero
// PluginScan: the dirs are accepted and ignored, and the session says so once
// rather than failing or silently dropping them.
func TestGrokSessionNotesIgnoredPluginDirs(t *testing.T) {
	grok := GrokProvider()
	var diag bytes.Buffer
	s := newTestSession(t, Options{
		Provider:   &grok,
		PluginDirs: []string{pluginFixtureDir(t)},
		Stderr:     &diag,
	})
	if got := s.discoverPlugins(t.TempDir()); len(got) != 0 {
		t.Fatalf("grok discovered %v", qualifiedNames(got))
	}
	if strings.TrimSpace(diag.String()) != "plugin dirs ignored for grok" {
		t.Fatalf("diagnostic %q", diag.String())
	}
}

func displayNames(cmds []PluginCommand) []string {
	out := make([]string, 0, len(cmds))
	for _, c := range cmds {
		out = append(out, c.Display)
	}
	return out
}

// TestResolvePluginNames is grok's naming rule: bare when it is unambiguous and
// unclaimed, qualified otherwise, nothing when both spellings are spoken for,
// and everything qualified while the agent's catalog is still unknown.
func TestResolvePluginNames(t *testing.T) {
	entry := func(plugin, name string) PluginEntry {
		return PluginEntry{Plugin: plugin, Name: name, Kind: PluginKindCommand}
	}

	cases := []struct {
		name        string
		entries     []PluginEntry
		taken       []string
		provisional bool
		want        []string
	}{
		{
			name:    "a unique name goes bare",
			entries: []PluginEntry{entry("git-commands", "watch-pr")},
			want:    []string{"watch-pr"},
		},
		{
			name:    "two plugins claiming one name both go qualified",
			entries: []PluginEntry{entry("forge", "gauntlet"), entry("flows", "gauntlet")},
			want:    []string{"forge:gauntlet", "flows:gauntlet"},
		},
		{
			name:    "a name the agent advertises goes qualified",
			entries: []PluginEntry{entry("forge", "simplify")},
			taken:   []string{"simplify"},
			want:    []string{"forge:simplify"},
		},
		{
			name:    "a name a builtin already means goes qualified",
			entries: []PluginEntry{entry("some-plugin", "help")},
			want:    []string{"some-plugin:help"},
		},
		{
			name:    "an entry whose qualified spelling is taken too is dropped",
			entries: []PluginEntry{entry("codex", "review")},
			taken:   []string{"review", "codex:review"},
			want:    nil,
		},
		{
			name:        "provisional makes every row qualified",
			entries:     []PluginEntry{entry("git-commands", "watch-pr"), entry("forge", "gauntlet")},
			provisional: true,
			want:        []string{"git-commands:watch-pr", "forge:gauntlet"},
		},
		{
			name:    "the comparison is case-insensitive",
			entries: []PluginEntry{entry("forge", "Simplify"), entry("flows", "SIMPLIFY")},
			taken:   []string{"HELP"},
			want:    []string{"forge:Simplify", "flows:SIMPLIFY"},
		},
		{
			name:    "a taken name differing only in case still qualifies",
			entries: []PluginEntry{entry("forge", "simplify")},
			taken:   []string{"SIMPLIFY"},
			want:    []string{"forge:simplify"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ResolvePluginNames(tc.entries, tc.taken, tc.provisional)
			if strings.Join(displayNames(got), ",") != strings.Join(tc.want, ",") {
				t.Fatalf("display %v, want %v", displayNames(got), tc.want)
			}
			for _, c := range got {
				if c.Qualified != c.Plugin+":"+c.Bare {
					t.Fatalf("qualified %q for %+v", c.Qualified, c)
				}
			}
		})
	}
}

// TestBuiltinNamesJoinTaken pins that the builtin list is folded in by
// ResolvePluginNames itself, so no caller can forget it.
func TestBuiltinNamesJoinTaken(t *testing.T) {
	for _, name := range BuiltinSlashNames() {
		got := ResolvePluginNames([]PluginEntry{{Plugin: "p", Name: name}}, nil, false)
		if len(got) != 1 || got[0].Display != "p:"+name {
			t.Fatalf("%q resolved to %v", name, displayNames(got))
		}
	}
}

// pluginFixtureDir is the fixture the session tests run against: one entry
// whose bare name the fake's `commands` catalog also advertises, and one that
// is the session's alone.
func pluginFixtureDir(t *testing.T) string {
	t.Helper()
	return writeTree(t, filepath.Join(t.TempDir(), "probe-plugin"), map[string]string{
		"commands/gauntlet-like.md": commandFile("collides with the advertised command", "collide body"),
		"commands/solo-thing.md":    commandFile("nobody else claims this", "solo body"),
	})
}

// commandsUpdate is the catalog notification the agent sends, as the session's
// own handler sees it: the one event that ends the provisional spelling and
// re-resolves every plugin name.
func commandsUpdate(names ...string) acp.SessionNotification {
	advertised := make([]map[string]any, 0, len(names))
	for _, name := range names {
		advertised = append(advertised, map[string]any{"name": name, "description": "the agent's own"})
	}
	return acp.SessionNotification{Update: mustJSON(map[string]any{
		"sessionUpdate":     acp.UpdateAvailableCommands,
		"availableCommands": advertised,
	})}
}

func displayOf(snap Snapshot, bare string) (PluginCommand, bool) {
	for _, c := range snap.Plugins {
		if c.Bare == bare {
			return c, true
		}
	}
	return PluginCommand{}, false
}

// TestSessionPluginsProvisionalUntilTheCatalog drives the resolution the way
// Start and the update handler do, without the wire in between: before the
// agent's first available_commands_update every row is qualified, and after it
// the unique one goes bare while the advertised one stays qualified.
func TestSessionPluginsProvisionalUntilTheCatalog(t *testing.T) {
	s := newTestSession(t, Options{PluginDirs: []string{pluginFixtureDir(t)}})
	s.plugins = DiscoverPlugins(CursorProvider().PluginScan(), t.TempDir(), "", s.opts.PluginDirs)
	if len(s.plugins) != 2 {
		t.Fatalf("fixture %v", qualifiedNames(s.plugins))
	}

	s.mu.Lock()
	s.snap.Plugins = s.resolvePluginsLocked()
	s.mu.Unlock()
	before := s.Snapshot()
	for _, c := range before.Plugins {
		if c.Display != c.Qualified {
			t.Fatalf("provisional row %+v must show its qualified spelling", c)
		}
	}

	s.onUpdate(commandsUpdate("gauntlet-like"))
	after := s.Snapshot()
	collide, ok := displayOf(after, "gauntlet-like")
	if !ok || collide.Display != "probe-plugin:gauntlet-like" {
		t.Fatalf("collision row %+v", collide)
	}
	solo, ok := displayOf(after, "solo-thing")
	if !ok || solo.Display != "solo-thing" {
		t.Fatalf("unique row %+v", solo)
	}
}

// TestSessionPluginsRenameAfterCommandsUpdate is the same rule end to end
// against the fake, whose `commands` catalog advertises gauntlet-like.
func TestSessionPluginsRenameAfterCommandsUpdate(t *testing.T) {
	dir := pluginFixtureDir(t)
	s := startScriptOpts(t, "commands", Options{PluginDirs: []string{dir}})

	deadline := time.Now().Add(5 * time.Second)
	var snap Snapshot
	for time.Now().Before(deadline) {
		snap = s.Snapshot()
		// Whatever the catalog has done so far, the advertised name is never
		// offered bare: that is the whole point of the provisional spelling.
		if c, ok := displayOf(snap, "gauntlet-like"); ok && c.Display != "probe-plugin:gauntlet-like" {
			t.Fatalf("advertised name offered as %q", c.Display)
		}
		if c, ok := displayOf(snap, "solo-thing"); ok && c.Display == "solo-thing" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	solo, ok := displayOf(snap, "solo-thing")
	if !ok || solo.Display != "solo-thing" {
		t.Fatalf("unique row %+v after the catalog landed (plugins %+v)", solo, snap.Plugins)
	}
	collide, ok := displayOf(snap, "gauntlet-like")
	if !ok || collide.Display != "probe-plugin:gauntlet-like" || collide.Kind != PluginKindCommand {
		t.Fatalf("collision row %+v", collide)
	}
	if collide.Description != "collides with the advertised command" {
		t.Fatalf("description %q", collide.Description)
	}
}

// TestSnapshotPluginsAreCloned pins that a caller cannot write through the
// snapshot into the session's own list.
func TestSnapshotPluginsAreCloned(t *testing.T) {
	s := startScriptOpts(t, "echo", Options{PluginDirs: []string{pluginFixtureDir(t)}})
	first := s.Snapshot()
	if len(first.Plugins) != 2 {
		t.Fatalf("plugins %+v", first.Plugins)
	}
	first.Plugins[0].Display = "overwritten"
	if second := s.Snapshot(); second.Plugins[0].Display == "overwritten" {
		t.Fatal("Snapshot handed out the session's own slice")
	}
}

// TestPromptDuringCommandsUpdate runs a turn while the catalog keeps landing,
// which under -race is what pins that the plugin list and the commands it was
// resolved against only ever change together, under s.mu.
func TestPromptDuringCommandsUpdate(t *testing.T) {
	s := startScriptOpts(t, "echo", Options{PluginDirs: []string{pluginFixtureDir(t)}})
	log := collect(t, s)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			s.onUpdate(commandsUpdate("gauntlet-like"))
			time.Sleep(time.Millisecond)
		}
	}()
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = s.Snapshot()
			time.Sleep(time.Millisecond)
		}
	}()

	if _, err := s.Prompt(t.Context(), "hi"); err != nil {
		t.Fatal(err)
	}
	close(stop)
	wg.Wait()
	log.waitTexts(t, "echo: hi")
}
