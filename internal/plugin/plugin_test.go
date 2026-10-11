// Package plugin_test checks the inline Claude Code plugin the repository
// ships at its root (plan 041 §3.6): .claude-plugin/, agents/ and skills/.
// There is no package code — the plugin is markdown and JSON — so the checks
// are tests, and they run under make test and CI.
package plugin_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/charliek/craze/internal/version"
)

// minCraze is the first craze release with the commands the plugin runs
// (new --wait, send, wait, cancel). Every place that states a minimum
// spells it this way.
const minCraze = "0.2.0"

// wantAgents are the agents plan 041 §3.6 lists.
var wantAgents = []string{"astra", "cursor", "delegate", "grok", "muse", "sol"}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd() // go test sets it to this package's directory
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the test's directory")
		}
		dir = parent
	}
}

func readFile(t *testing.T, root, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// frontmatter splits a markdown file into its `---` fenced header's
// key: value pairs (one line each, surrounding double quotes removed) and
// its body.
func frontmatter(text string) (map[string]string, string, error) {
	rest, ok := strings.CutPrefix(text, "---\n")
	if !ok {
		return nil, "", fmt.Errorf("does not start with a --- line")
	}
	head, body, ok := strings.Cut(rest, "\n---\n")
	if !ok {
		return nil, "", fmt.Errorf("frontmatter is not closed by a --- line")
	}
	fm := map[string]string{}
	for _, line := range strings.Split(head, "\n") {
		key, val, ok := strings.Cut(line, ": ")
		if !ok || strings.TrimSpace(key) == "" {
			return nil, "", fmt.Errorf("frontmatter line %q is not key: value", line)
		}
		val = strings.TrimSpace(val)
		if len(val) >= 2 && val[0] == '"' && val[len(val)-1] == '"' {
			val = val[1 : len(val)-1]
		}
		fm[strings.TrimSpace(key)] = val
	}
	return fm, body, nil
}

// checkAgent returns what is wrong with one agent file, or nil.
func checkAgent(text string) []string {
	fm, body, err := frontmatter(text)
	if err != nil {
		return []string{err.Error()}
	}
	var bad []string
	for _, key := range []string{"name", "description", "effort", "tools"} {
		if fm[key] == "" {
			bad = append(bad, "frontmatter has no "+key)
		}
	}
	if fm["model"] != "haiku" {
		bad = append(bad, fmt.Sprintf("model is %q, want haiku", fm["model"]))
	}
	if !strings.Contains(body, "timeout: 600000") {
		bad = append(bad, "body does not name timeout: 600000")
	}
	// The temp file's name: 12 hex digits made up fresh, never an example's
	// (a live run copied the example verbatim, and once wrote a space).
	for _, want := range []string{"exactly 12 characters", "<12 hex digits>"} {
		if !strings.Contains(body, want) {
			bad = append(bad, "body does not contain "+want)
		}
	}
	// Every message is forwarded (a live wrapper answered a recap from its own
	// transcript, so the message never reached craze).
	for _, want := range []string{"EVERY message you receive is forwarded to craze", "Never reply from what you already know"} {
		if !strings.Contains(body, want) {
			bad = append(bad, "body does not contain "+want)
		}
	}
	if !strings.Contains(body, "## Follow-ups\n\nEVERY message after the first is forwarded too") {
		bad = append(bad, "Follow-ups section does not repeat that every message is forwarded")
	}
	for _, banned := range []string{"3fa91c07be42", "<random hex>"} {
		if strings.Contains(body, banned) {
			bad = append(bad, "body still contains "+banned)
		}
	}
	for _, want := range []string{"--max-message 20000", "--remove-prompt-file", "craze ≥ " + minCraze} {
		if !strings.Contains(body, want) {
			bad = append(bad, "body does not contain "+want)
		}
	}
	return bad
}

// checkSkill returns what is wrong with one skill file, or nil.
func checkSkill(text string) []string {
	fm, _, err := frontmatter(text)
	if err != nil {
		return []string{err.Error()}
	}
	var bad []string
	for _, key := range []string{"name", "description"} {
		if fm[key] == "" {
			bad = append(bad, "frontmatter has no "+key)
		}
	}
	return bad
}

func TestManifestsParse(t *testing.T) {
	root := repoRoot(t)
	for _, rel := range []string{".claude-plugin/plugin.json", ".claude-plugin/marketplace.json"} {
		var v map[string]any
		if err := json.Unmarshal([]byte(readFile(t, root, rel)), &v); err != nil {
			t.Errorf("%s: %v", rel, err)
		}
	}
}

func TestPluginVersionIsTheBinarysVersion(t *testing.T) {
	root := repoRoot(t)
	var p struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal([]byte(readFile(t, root, ".claude-plugin/plugin.json")), &p); err != nil {
		t.Fatal(err)
	}
	if p.Name != "craze" {
		t.Errorf("plugin name = %q, want craze", p.Name)
	}
	if p.Version != version.Version {
		t.Errorf("plugin.json version = %q, internal/version.Version = %q; scripts/release/update-version.sh bumps both", p.Version, version.Version)
	}
}

func TestMarketplaceListsThePluginAtTheRoot(t *testing.T) {
	root := repoRoot(t)
	var m struct {
		Plugins []struct {
			Name   string `json:"name"`
			Source string `json:"source"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal([]byte(readFile(t, root, ".claude-plugin/marketplace.json")), &m); err != nil {
		t.Fatal(err)
	}
	if len(m.Plugins) != 1 || m.Plugins[0].Name != "craze" || m.Plugins[0].Source != "./" {
		t.Errorf("marketplace plugins = %+v, want one craze entry with source ./", m.Plugins)
	}
}

func TestAgents(t *testing.T) {
	root := repoRoot(t)
	files, err := filepath.Glob(filepath.Join(root, "agents", "*.md"))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), ".md")
		got = append(got, name)
		text := readFile(t, root, filepath.Join("agents", name+".md"))
		for _, bad := range checkAgent(text) {
			t.Errorf("agents/%s.md: %s", name, bad)
		}
		if fm, _, err := frontmatter(text); err == nil && fm["name"] != name {
			t.Errorf("agents/%s.md: name is %q, want %q", name, fm["name"], name)
		}
	}
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(wantAgents, ",") {
		t.Errorf("agents = %v, want %v", got, wantAgents)
	}
}

func TestSkills(t *testing.T) {
	root := repoRoot(t)
	files, err := filepath.Glob(filepath.Join(root, "skills", "*", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no skills/*/SKILL.md")
	}
	for _, f := range files {
		rel, _ := filepath.Rel(root, f)
		for _, bad := range checkSkill(readFile(t, root, rel)) {
			t.Errorf("%s: %s", rel, bad)
		}
	}
	skill := readFile(t, root, "skills/craze/SKILL.md")
	if !strings.Contains(skill, minCraze) {
		t.Errorf("skills/craze/SKILL.md does not state the minimum craze %s", minCraze)
	}
}

// TestAllowListUsesEditRule pins the permission rule for the task file
// (plan 041 X31): Claude Code checks its Write tool's paths against Edit
// rules only, and `//` marks an absolute path, so Write(...) and a
// single-slash path are accepted and never consulted.
func TestAllowListUsesEditRule(t *testing.T) {
	root := repoRoot(t)
	for _, rel := range []string{"skills/craze/SKILL.md", "skills/craze/references/commands.md", "docs/getting-started/claude-code.md", "README.md"} {
		for _, bad := range checkAllowList(readFile(t, root, rel), rel != "README.md") {
			t.Errorf("%s: %s", rel, bad)
		}
	}
}

// checkAllowList returns what is wrong with a file's mention of the task
// file's permission rule; a file that must list the rule has it.
func checkAllowList(text string, mustList bool) []string {
	var bad []string
	if strings.Contains(text, "Write(/") {
		bad = append(bad, "has a Write(/…) rule, which Claude Code never consults")
	}
	if mustList && !strings.Contains(text, "Edit(//tmp/craze-delegate-*)") {
		bad = append(bad, "does not list Edit(//tmp/craze-delegate-*)")
	}
	return bad
}

// TestCheckersRefuse proves the checkers above can fail: each broken input
// is reported, so a passing TestAgents is not vacuous.
func TestCheckersRefuse(t *testing.T) {
	good := "---\nname: x\ndescription: \"d\"\ntools: Bash, Write\nmodel: haiku\neffort: high\n---\n" +
		"EVERY message you receive is forwarded to craze Never reply from what you already know ## Follow-ups\n\nEVERY message after the first is forwarded too exactly 12 characters <12 hex digits> timeout: 600000 --max-message 20000 --remove-prompt-file craze ≥ " + minCraze + "\n"
	if bad := checkAgent(good); len(bad) != 0 {
		t.Fatalf("good agent refused: %v", bad)
	}
	cases := map[string]string{
		"no timeout":     strings.Replace(good, "timeout: 600000", "", 1),
		"wrong model":    strings.Replace(good, "model: haiku", "model: opus", 1),
		"no effort":      strings.Replace(good, "effort: high\n", "", 1),
		"no tools":       strings.Replace(good, "tools: Bash, Write\n", "", 1),
		"no description": strings.Replace(good, "description: \"d\"\n", "", 1),
		"no frontmatter": "just text",
	}
	for name, text := range cases {
		if len(checkAgent(text)) == 0 {
			t.Errorf("%s: checkAgent accepted it", name)
		}
	}
	if len(checkAgent(strings.Replace(good, "Never reply from what you already know", "", 1))) == 0 {
		t.Error("checkAgent accepted a body without the forward-everything rule")
	}
	if len(checkAllowList("Write(/tmp/craze-delegate-*)", true)) != 2 {
		t.Error("checkAllowList accepted the Write rule")
	}
	if len(checkAllowList("no rule here", false)) != 0 {
		t.Error("checkAllowList refused a file that need not list the rule")
	}
	if len(checkAgent(strings.Replace(good, "<12 hex digits>", "3fa91c07be42", 1))) == 0 {
		t.Error("checkAgent accepted a body with no placeholder")
	}
	if len(checkSkill("---\nname: x\n---\nbody")) == 0 {
		t.Error("checkSkill accepted a skill with no description")
	}
}
