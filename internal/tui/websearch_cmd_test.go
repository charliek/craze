package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/harness/websearch"
)

// /websearch (issues #137, #141).

// webSearchModel is a started native session's model whose native directory
// is dir, with config.toml in a home of its own (configToml, "" for none).
func webSearchModel(t *testing.T, dir, configToml string) Model {
	t.Helper()
	t.Setenv("CRAZE_HOME", t.TempDir())
	m := connectModelOf(t, Config{Session: nativeStub(), NativeDir: dir})
	if configToml != "" {
		path := configPath()
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(configToml), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return m
}

// TestWebSearchPanel: the bare command's rows from the session's web status
// and the home's settings; rows with nothing to count are left out.
func TestWebSearchPanel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, websearch.StateFile)
	st := &agent.WebStatusState{Provider: "exa", Key: "stored", Consent: "allowed",
		Searches: 14, Fetches: 1, Failed: 2, RateLimited: 1, Retries: 4}
	got := strings.Join(webSearchPanel(st, agent.WebConfig{}, path), "\n")
	want := strings.Join([]string{
		"Web search · Exa (exa.ai)",
		"  This session   on",
		"  Key            stored key (craze auth login websearch/exa)",
		"  Calls          14 searches, 1 fetch",
		"  Failed         2, 1 rate limited",
		"  Retried        4 rate-limited attempts sent again",
		"  New sessions   ask on the first web call · /websearch on to allow without asking",
	}, "\n")
	if got != want {
		t.Fatalf("panel:\n%s\nwant:\n%s", got, want)
	}

	quiet := &agent.WebStatusState{Provider: "exa", Key: "keyless", Consent: "undecided"}
	got = strings.Join(webSearchPanel(quiet, agent.WebConfig{Setting: agent.WebSettingOff}, path), "\n")
	for _, want := range []string{
		"This session   asks on its first web call",
		"Key            keyless (Exa's free limit; craze auth login websearch/exa raises it)",
		"Calls          none yet",
		"New sessions   config.toml turns web search off (websearch = false)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("panel lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Failed") || strings.Contains(got, "Retried") {
		t.Errorf("rows with nothing to count:\n%s", got)
	}

	if got := strings.Join(webSearchPanel(nil, agent.WebConfig{}, path), "\n"); !strings.Contains(got, "no web status yet") {
		t.Errorf("before any status:\n%s", got)
	}
}

// TestWebSearchNewSessions: the remembered choice as the panel's last row.
func TestWebSearchNewSessions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, websearch.StateFile)
	for _, tc := range []struct {
		file, want string
	}{
		{`{"version":1,"provider":"exa"}`, "on (websearch.json) · /websearch off or ask to change"},
		{`{"version":1,"provider":false}`, "off (websearch.json) · /websearch on or ask to change"},
		{`{`, "ask on the first web call (websearch.json is not valid JSON, so no web search choice is remembered)"},
	} {
		if err := os.WriteFile(path, []byte(tc.file), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := newSessions(agent.WebConfig{}, path); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.file, got, tc.want)
		}
	}
	if got := newSessions(agent.WebConfig{Setting: agent.WebSettingOn, Provider: "exa"}, path); !strings.HasPrefix(got, "config.toml turns web search on") {
		t.Errorf("config on: %q", got)
	}
}

// TestWebSearchOnOffAsk: the three arguments write, rewrite and remove the
// remembered choice, each saying so; the session keeps its own tools; an
// unknown argument is a usage error that changes nothing.
func TestWebSearchOnOffAsk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, websearch.StateFile)
	m := webSearchModel(t, dir, "")
	for _, tc := range []struct {
		cmd  string
		want websearch.Choice
	}{
		{"/websearch on", websearch.Choice{Provider: "exa"}},
		{"/websearch OFF", websearch.Choice{Disabled: true}},
		{"/websearch ask", websearch.Choice{}},
	} {
		m = runSlash(t, m, tc.cmd)
		if got, _ := websearch.ReadChoice(path); got != tc.want {
			t.Fatalf("%s: remembered %+v, want %+v", tc.cmd, got, tc.want)
		}
		notes := texts(m, entryNote)
		want := "(a session that has decided keeps its choice; one still undecided adopts this at its next web call)"
		if tc.want == (websearch.Choice{}) {
			want = "(a session that has decided keeps its choice; one still undecided asks at its next web call)"
		}
		if len(notes) == 0 || !strings.HasSuffix(notes[len(notes)-1], want) {
			t.Fatalf("%s: notes %q", tc.cmd, notes)
		}
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("ask left the file: %v", err)
	}
	m = runSlash(t, m, "/websearch sometimes")
	if got := texts(m, entryError); len(got) != 1 || got[0] != webSearchUsage {
		t.Fatalf("errors %q", got)
	}
	m = runSlash(t, m, "/websearch ask") // nothing to remove is no error
	if got := texts(m, entryError); len(got) != 1 {
		t.Fatalf("errors %q", got)
	}
}

// TestWebSearchSaysConfigWins: a change under a config.toml websearch
// setting is made, and the note says the setting wins.
func TestWebSearchSaysConfigWins(t *testing.T) {
	dir := t.TempDir()
	m := webSearchModel(t, dir, "websearch = false\n")
	m = runSlash(t, m, "/websearch on")
	notes := texts(m, entryNote)
	if len(notes) < 2 || notes[len(notes)-1] != "web search: config.toml turns web search off (websearch = false), which wins over this" {
		t.Fatalf("notes %q", notes)
	}
}

// TestWebSearchShowsTheSessionsStatus: the bare command reads the session's
// last web status section.
func TestWebSearchShowsTheSessionsStatus(t *testing.T) {
	m := webSearchModel(t, t.TempDir(), "")
	m.snap.WebStatus = &agent.WebStatusState{Provider: "exa", Key: "env", Consent: "allowed for this session", Searches: 1}
	m = runSlash(t, m, "/websearch")
	got := strings.Join(texts(m, entryNote), "\n")
	for _, want := range []string{"This session   on, for this session", "Key            EXA_API_KEY", "Calls          1 search, 0 fetches"} {
		if !strings.Contains(got, want) {
			t.Errorf("notes lack %q:\n%s", want, got)
		}
	}
}

// TestWebSearchIsNativeOnly: in a session that is not native the command is
// refused and writes nothing; it runs mid-turn.
func TestWebSearchIsNativeOnly(t *testing.T) {
	if !runsWhileWorking("websearch") {
		t.Fatal("/websearch needs no agent and must run mid-turn")
	}
	dir := t.TempDir()
	t.Setenv("CRAZE_HOME", t.TempDir())
	m := connectModelOf(t, Config{Session: NewStub(), NativeDir: dir})
	_ = runSlash(t, m, "/websearch on")
	if _, err := os.Stat(filepath.Join(dir, websearch.StateFile)); !os.IsNotExist(err) {
		t.Fatalf("a non-native session wrote the choice: %v", err)
	}
}

// TestStatusRowCountsSearchesAndFetches: the status row's in-flight counts
// read "searches" and "fetches": the kinds ending in "ch" take "es".
func TestStatusRowCountsSearchesAndFetches(t *testing.T) {
	for _, tc := range []struct {
		kind string
		n    int
		want string
	}{
		{"search", 1, ""}, {"search", 8, "es"}, {"fetch", 2, "es"}, {"read", 2, "s"}, {"shell", 3, "s"},
	} {
		if got := kindPlural(tc.kind, tc.n); got != tc.want {
			t.Errorf("kindPlural(%q, %d) = %q, want %q", tc.kind, tc.n, got, tc.want)
		}
	}
}
