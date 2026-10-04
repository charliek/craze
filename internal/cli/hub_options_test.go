package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/rundir"
)

// The hub's sessions.createOptions answer as craze hub builds it (plan 036
// §3.5, A4): hubCreates().Options, run in this process over a scratch machine
// whose HOME, CRAZE_HOME and PATH are three different places (t.Setenv, so
// nothing here is parallel) — so a file read from the wrong one, or a binary
// found by the wrong rule, shows.

// optionsMachine is the scratch machine: HOME, CRAZE_HOME and PATH distinct
// directories, a decoy config.toml and session index under HOME's own .craze
// (which CRAZE_HOME overrides, so neither may be read), no CRAZE_PROVIDER or
// CRAZE_AGENT_BIN. It answers CRAZE_HOME and PATH's directory.
func optionsMachine(t *testing.T) (crazeDir, pathDir string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	decoy := filepath.Join(home, ".craze")
	if err := os.MkdirAll(decoy, 0o700); err != nil {
		t.Fatal(err)
	}
	decoyDir := t.TempDir()
	writeFileT(t, filepath.Join(decoy, "config.toml"), "provider = \"cursor\"\n")
	writeFileT(t, filepath.Join(decoy, "sessions.jsonl"), indexRow("decoy", decoyDir, "2026-10-03T23:59:59Z"))
	crazeDir = crazeHome(t)
	pathDir = availPath(t)
	t.Setenv("CRAZE_PROVIDER", "")
	t.Setenv(envAgentBin, "")
	return crazeDir, pathDir
}

// writeFileT writes body to path.
func writeFileT(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// indexRow is one session index line: a session id, its workspace, and when
// it was last updated (RFC 3339).
func indexRow(id, cwd, updatedAt string) string {
	b, err := json.Marshal(map[string]string{"sessionId": id, "provider": "grok", "cwd": cwd, "updatedAt": updatedAt})
	if err != nil {
		panic(err)
	}
	return string(b) + "\n"
}

// optionsLog is a logf that keeps every line.
type optionsLog []string

func (l *optionsLog) logf(format string, args ...any) { *l = append(*l, fmt.Sprintf(format, args...)) }

// hubOptions is the hub's answer on this machine, with what it logged.
func hubOptions(t *testing.T) (protocol.CreateOptionsResult, optionsLog) {
	t.Helper()
	var log optionsLog
	opts := hubCreates().Options
	if opts == nil {
		t.Fatal("craze hub's Creates has no Options")
	}
	return opts(log.logf), log
}

// TestHubCreateOptionsReadsTheProcessCrazeHome (§3.5, A4): the answer's
// config.toml, native directory and session index are CRAZE_HOME's, never
// HOME's own .craze; its binaries are found as a host the hub creates finds
// them — [agents] in that config, then PATH — whatever CRAZE_AGENT_BIN says
// (here, for a cursor launch, a binary that is not there, which the TUI's own
// column would report); gx, missing, is not listed; the default is the
// config's provider; and nothing is logged.
func TestHubCreateOptionsReadsTheProcessCrazeHome(t *testing.T) {
	crazeDir, pathDir := optionsMachine(t)
	writeExecutable(t, pathDir, "cursor-agent")
	grok := writeExecutable(t, t.TempDir(), "my-grok")
	writeFileT(t, filepath.Join(crazeDir, "config.toml"), fmt.Sprintf("provider = \"grok\"\n\n[agents]\ngrok = %q\n", grok))
	work := t.TempDir()
	writeFileT(t, filepath.Join(crazeDir, "sessions.jsonl"), indexRow("s-1", work, "2026-10-03T12:00:00Z"))
	t.Setenv("CRAZE_PROVIDER", "cursor")
	t.Setenv(envAgentBin, filepath.Join(t.TempDir(), "no-such-agent"))

	got, log := hubOptions(t)
	want := protocol.CreateOptionsResult{
		Providers: []protocol.ProviderOption{
			{ID: "cursor", Label: "cursor", State: protocol.ProviderReady},
			{ID: "grok", Label: "grok", State: protocol.ProviderReady},
			{ID: "native", Label: "native", State: protocol.ProviderNeedsSetup, Reason: nativeNoKeyReason, Fix: nativeNoKeyFix},
		},
		DefaultProvider: "grok",
		RecentDirs:      []protocol.RecentDir{{Dir: work, UsedAt: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the hub's answer:\n%+v\nwant:\n%+v", got, want)
	}
	if len(log) != 0 {
		t.Fatalf("it logged %q", log)
	}
	// The TUI's own column, on the same machine, takes CRAZE_AGENT_BIN for
	// the cursor launch: the hub's answer is not that one.
	if own := availability(processAvailInputs(agent.CursorProvider(), ""), agent.Providers()); own[0].State != availUnavailable ||
		!strings.HasPrefix(own[0].Reason, envAgentBin+" ") {
		t.Fatalf("the TUI's cursor on this machine: %+v, want CRAZE_AGENT_BIN's miss", own[0])
	}
}

// TestHubCreateOptionsNamesTheHubsPID (§3.1's hub column): outside the macOS
// login session, the answer's cursor fix names the pid of the process that
// answers — the hub's own.
func TestHubCreateOptionsNamesTheHubsPID(t *testing.T) {
	_, pathDir := optionsMachine(t)
	writeExecutable(t, pathDir, "cursor-agent")
	t.Setenv(rundir.GUISessionEnv, "0")
	got, _ := hubOptions(t)
	want := protocol.ProviderOption{ID: "cursor", Label: "cursor", State: protocol.ProviderUnavailable, Reason: notGUIReason,
		Fix: fmt.Sprintf(notGUIHubFix, os.Getpid())}
	if got.Providers[0] != want {
		t.Fatalf("cursor outside the login session: %+v, want %+v", got.Providers[0], want)
	}
}

// TestHubDefaultProvider (decision 8): the default is present only for a
// set, known provider — one craze knows need be neither listed (gx, not
// installed) nor ready — and absent for none set, an empty one, an unknown
// one, and no config.toml at all: never agent.ProviderByName("")'s cursor.
func TestHubDefaultProvider(t *testing.T) {
	crazeDir, _ := optionsMachine(t)
	config := filepath.Join(crazeDir, "config.toml")
	for _, tc := range []struct {
		name, config, want string
	}{
		{"no config.toml", "", ""},
		{"no provider set", "theme = \"dark\"\n", ""},
		{"an empty provider", "provider = \"\"\n", ""},
		{"a blank provider", "provider = \"  \"\n", ""},
		{"an unknown provider", "provider = \"nope\"\n", ""},
		{"grok", "provider = \"grok\"\n", "grok"},
		{"native, not ready", "provider = \"native\"\n", "native"},
		{"gx, not installed", "provider = \"gx\"\n", "gx"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_ = os.Remove(config)
			if tc.config != "" {
				writeFileT(t, config, tc.config)
			}
			if got, _ := hubOptions(t); got.DefaultProvider != tc.want {
				t.Fatalf("defaultProvider %q, want %q", got.DefaultProvider, tc.want)
			}
		})
	}
}

// TestHubRecentDirs (§3.4, decision 7): the session index's workspaces,
// newest first — equal times to the later row in the file — each once,
// cleaned (a duplicate under another spelling folded into its newest row);
// a relative one dropped (here one that is a directory relative to the
// working directory, which the index's own read keeps), as are a deleted
// directory and an ordinary file; each time in UTC; and [] — never null —
// for none.
func TestHubRecentDirs(t *testing.T) {
	crazeDir, _ := optionsMachine(t)
	index := filepath.Join(crazeDir, "sessions.jsonl")
	wd := t.TempDir()
	t.Chdir(wd)
	if err := os.Mkdir(filepath.Join(wd, "rel"), 0o700); err != nil {
		t.Fatal(err)
	}
	a, b, c := t.TempDir(), t.TempDir(), t.TempDir()
	gone := filepath.Join(t.TempDir(), "gone")
	file := filepath.Join(t.TempDir(), "a-file")
	writeFileT(t, file, "")
	utc := func(s string) time.Time {
		tm, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return tm.UTC()
	}
	rows := indexRow("s-a-old", a, "2026-10-01T08:00:00Z") +
		indexRow("s-rel", "rel", "2026-10-03T23:00:00Z") +
		indexRow("s-gone", gone, "2026-10-03T22:00:00Z") +
		indexRow("s-file", file, "2026-10-03T21:00:00Z") +
		indexRow("s-b", b, "2026-10-02T10:00:00+02:00") +
		indexRow("s-c", c, "2026-10-02T08:00:00Z") +
		indexRow("s-a-new", a+"/./", "2026-10-03T09:00:00Z")
	writeFileT(t, index, rows)
	got, log := hubOptions(t)
	// b's 10:00+02:00 is c's 08:00Z: equal, so c — the later row — first.
	want := []protocol.RecentDir{
		{Dir: a, UsedAt: utc("2026-10-03T09:00:00Z")},
		{Dir: c, UsedAt: utc("2026-10-02T08:00:00Z")},
		{Dir: b, UsedAt: utc("2026-10-02T08:00:00Z")},
	}
	if !reflect.DeepEqual(got.RecentDirs, want) || len(log) != 0 {
		t.Fatalf("recentDirs %+v (logged %q)\nwant %+v", got.RecentDirs, log, want)
	}
	if enc := mustJSON(t, got.RecentDirs[2]); !strings.Contains(enc, `"usedAt":"2026-10-02T08:00:00Z"`) {
		t.Fatalf("a time written %s, want it in UTC", enc)
	}

	for name, body := range map[string]string{
		"no index":       "",
		"nothing usable": indexRow("s-rel", "rel", "2026-10-03T23:00:00Z") + indexRow("s-gone", gone, "2026-10-03T22:00:00Z"),
	} {
		_ = os.Remove(index)
		if body != "" {
			writeFileT(t, index, body)
		}
		got, _ := hubOptions(t)
		if got.RecentDirs == nil || len(got.RecentDirs) != 0 || !strings.Contains(mustJSON(t, got), `"recentDirs":[]`) {
			t.Fatalf("%s: recentDirs %#v, written %s; want []", name, got.RecentDirs, mustJSON(t, got))
		}
	}
}

// TestHubRecentDirsDropRelativeBeforeTheCap (decision 7): the cap of 20
// applies to what is left once relative entries are dropped — a newer
// relative one does not take one of the twenty places — and the twenty are
// the newest.
func TestHubRecentDirsDropRelativeBeforeTheCap(t *testing.T) {
	crazeDir, _ := optionsMachine(t)
	wd := t.TempDir()
	t.Chdir(wd)
	var rows strings.Builder
	var want []string
	for i := range protocol.CreateOptionsDirsMax + 5 {
		dir := t.TempDir()
		rows.WriteString(indexRow(fmt.Sprintf("s-%d", i), dir, fmt.Sprintf("2026-10-01T10:%02d:00Z", i)))
		want = append([]string{dir}, want...)
	}
	for i := range 3 {
		rel := fmt.Sprintf("rel-%d", i)
		if err := os.Mkdir(filepath.Join(wd, rel), 0o700); err != nil {
			t.Fatal(err)
		}
		rows.WriteString(indexRow("s-"+rel, rel, fmt.Sprintf("2026-10-03T0%d:00:00Z", i)))
	}
	writeFileT(t, filepath.Join(crazeDir, "sessions.jsonl"), rows.String())
	got, _ := hubOptions(t)
	var dirs []string
	for _, d := range got.RecentDirs {
		dirs = append(dirs, d.Dir)
	}
	if !reflect.DeepEqual(dirs, want[:protocol.CreateOptionsDirsMax]) {
		t.Fatalf("recentDirs (%d):\n%q\nwant the %d newest absolute ones:\n%q", len(dirs), dirs, protocol.CreateOptionsDirsMax,
			want[:protocol.CreateOptionsDirsMax])
	}
}

// TestHubRecentDirsOfAnUnreadableIndex (§3.4): an index that does not read
// is no recent directories, [] — the providers and the default answered all
// the same — and one line in the hub's log saying why.
func TestHubRecentDirsOfAnUnreadableIndex(t *testing.T) {
	crazeDir, _ := optionsMachine(t)
	writeFileT(t, filepath.Join(crazeDir, "config.toml"), "provider = \"grok\"\n")
	writeFileT(t, filepath.Join(crazeDir, "sessions.jsonl"), indexRow("s-1", t.TempDir(), "2026-10-03T12:00:00Z")+"{not json\n")
	got, log := hubOptions(t)
	if got.RecentDirs == nil || len(got.RecentDirs) != 0 || got.DefaultProvider != "grok" || len(got.Providers) != 3 {
		t.Fatalf("the answer over a malformed index: %+v", got)
	}
	if len(log) != 1 || !strings.Contains(log[0], "the session index cannot be read") || !strings.Contains(log[0], "sessions.jsonl:2") {
		t.Fatalf("logged %q, want one line saying the index (line 2) cannot be read", log)
	}
}

// mustJSON is v as JSON.
func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
