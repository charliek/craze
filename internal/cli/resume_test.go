package cli

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/sessions"
	"github.com/charliek/craze/internal/tui"
)

// indexHome points HOME and the config path at a fresh temp directory, so the
// session index a case reads and writes is its own. CRAZE_CONFIG is set rather
// than left alone because it is what decides where the index sits (it is the
// config file's sibling), and a developer with one exported would otherwise
// send these writes to their own ~/.craze.
func indexHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("CRAZE_CONFIG", filepath.Join(dir, "config.toml"))
	t.Setenv("CRAZE_PROVIDER", "")
	return dir
}

// seedRow writes one index row, with UpdatedAt forced so a case can say which
// row is the newest without sleeping. Upsert stamps its own timestamps, so the
// file is rewritten afterwards with the one the case asked for.
func seedRow(t *testing.T, row sessions.Row, age time.Duration) {
	t.Helper()
	store := &sessions.Store{}
	if err := store.Upsert(row); err != nil {
		t.Fatalf("seed %s: %v", row.SessionID, err)
	}
	if age == 0 {
		return
	}
	at := time.Now().UTC().Add(-age).Format(time.RFC3339Nano)
	path := indexPath(t)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	for i, line := range lines {
		if !strings.Contains(line, `"sessionId":"`+row.SessionID+`"`) {
			continue
		}
		lines[i] = replaceJSONTime(line, "updatedAt", at)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// replaceJSONTime rewrites one RFC3339 value in an encoded row. The index is
// one JSON object per line and the test only ever changes a timestamp, so a
// string splice is enough and keeps the rest of the line byte-identical.
func replaceJSONTime(line, field, value string) string {
	key := `"` + field + `":"`
	i := strings.Index(line, key)
	if i < 0 {
		return line
	}
	rest := line[i+len(key):]
	j := strings.Index(rest, `"`)
	if j < 0 {
		return line
	}
	return line[:i+len(key)] + value + rest[j:]
}

func indexPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(filepath.Dir(os.Getenv("CRAZE_CONFIG")), "sessions.jsonl")
}

// parseTUIFlags runs the root command's flag set over argv without running
// anything, which is what gives Changed("provider") the value the real command
// line would give it.
func parseTUIFlags(t *testing.T, argv ...string) (*cobra.Command, *tuiFlags) {
	t.Helper()
	f := &tuiFlags{force: true}
	cmd := &cobra.Command{Use: "craze", RunE: func(*cobra.Command, []string) error { return nil }}
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	registerTUIFlags(cmd, f)
	cmd.SetArgs(argv)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("parse %v: %v", argv, err)
	}
	return cmd, f
}

// runResolveLoad is --continue/--resume as runTUI runs it: the resolved
// provider goes in as Config.Provider (that is what an explicit --provider
// filters by) and the recorded Options are what the session would have been
// built from.
func runResolveLoad(t *testing.T, cwd string, argv ...string) (tui.Config, []agent.Options, error) {
	t.Helper()
	cmd, f := parseTUIFlags(t, argv...)
	resolved, err := resolveProvider(cmd, f.provider, io.Discard, false)
	if err != nil {
		t.Fatalf("resolve provider: %v", err)
	}
	cfg := tui.Config{
		Provider:        resolved.Provider,
		ProviderLocked:  resolved.Locked,
		PersistProvider: true,
		FallbackDefault: resolved.Fallback,
	}
	var built []agent.Options
	build := func(p agent.Provider, row sessions.Row) agent.Session {
		built = append(built, sessionOptions(f, cwd, "", io.Discard, nil, p, row))
		return nil
	}
	return cfg, built, resolveLoad(cmd, f, cwd, &cfg, build)
}

func exitCode(t *testing.T, err error) (int, string) {
	t.Helper()
	var ee *exitError
	if !errors.As(err, &ee) {
		t.Fatalf("not an exit error: %v", err)
	}
	return ee.code, ee.msg
}

// TestContinueAndResumeAreMutuallyExclusive: the two flags ask for different
// things out of the same index, so asking for both is a usage error — exit 2,
// the same helper --ask and --plan share, and before anything reads the index
// or draws a frame.
func TestContinueAndResumeAreMutuallyExclusive(t *testing.T) {
	indexHome(t)
	_, f := parseTUIFlags(t, "--continue", "--resume")
	code, msg := exitCode(t, runTUI(nil, f, hostEnv{}))
	if code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	if !strings.Contains(msg, "--continue and --resume are mutually exclusive") {
		t.Fatalf("msg %q", msg)
	}
}

// TestContinueWithNoRowExits1 is §3.8's first failure path: nothing to
// continue is exit 1 with the workspace named, before the TUI — not an empty
// craze, and never a silent session/new.
func TestContinueWithNoRowExits1(t *testing.T) {
	indexHome(t)
	ws := t.TempDir()
	for _, tc := range []struct {
		name string
		argv []string
		want string
	}{
		{"any provider", []string{"--continue"}, "craze: no session to continue in " + ws},
		{"filtered", []string{"--continue", "--provider", "grok"}, "craze: no session to continue in " + ws + " for provider grok"},
		{"resume", []string{"--resume"}, "craze: no session to continue in " + ws},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, _, err := runResolveLoad(t, ws, tc.argv...)
			code, msg := exitCode(t, err)
			if code != 1 {
				t.Fatalf("exit %d, want 1", code)
			}
			if msg != tc.want {
				t.Fatalf("msg %q, want %q", msg, tc.want)
			}
			if cfg.Session != nil || cfg.Resume != nil {
				t.Fatal("a refusal must not leave a session or a picker behind")
			}
		})
	}
}

// TestContinueWithAMalformedIndexExits1AndKeepsTheFile: a file craze cannot
// read is reported with the read error — which names the file and the line —
// and is left exactly as it was. A reader that repaired the index would throw
// away the rows it could not parse, which is the opposite of what a user whose
// index got corrupted wants (it mirrors ErrConfigMalformed).
func TestContinueWithAMalformedIndexExits1AndKeepsTheFile(t *testing.T) {
	indexHome(t)
	ws := t.TempDir()
	path := indexPath(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	broken := []byte("{\"sessionId\":\"a\",\"provider\":\"cursor\",\"cwd\":\"" + ws + "\"}\nnot json at all\n")
	if err := os.WriteFile(path, broken, 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := runResolveLoad(t, ws, "--continue")
	code, msg := exitCode(t, err)
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(msg, path) {
		t.Fatalf("the message does not name the file: %q", msg)
	}
	after, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(after) != string(broken) {
		t.Fatalf("the index was rewritten:\n%s", after)
	}
}

// TestContinueLoadsTheRow is the whole of §3.1's happy path: the newest row
// decides the provider (locked, so no picker), seeds the load id, the stored
// title and its pin — and does not touch the persisted default provider,
// because continuing yesterday's thread is not a choice about tomorrow's.
func TestContinueLoadsTheRow(t *testing.T) {
	indexHome(t)
	ws := t.TempDir()
	seedRow(t, sessions.Row{SessionID: "old", Provider: "cursor", CWD: ws, Title: "yesterday", TitleKind: sessions.TitleKindAgent}, 48*time.Hour)
	seedRow(t, sessions.Row{SessionID: "new", Provider: "grok", CWD: ws, Title: "the renamed one", Pinned: true, TitleKind: sessions.TitleKindUser}, time.Minute)

	cfg, built, err := runResolveLoad(t, ws, "--continue")
	if err != nil {
		t.Fatalf("continue: %v", err)
	}
	if !cfg.Loading {
		t.Fatal("a --continue session must be flagged as loading, or the TUI never enters the restoring state")
	}
	if len(built) != 1 {
		t.Fatalf("built %d sessions, want 1", len(built))
	}
	if !cfg.ProviderLocked {
		t.Fatal("the row's provider must be locked, so the picker is skipped")
	}
	if cfg.Provider.Name() != "grok" {
		t.Fatalf("provider %q, want grok", cfg.Provider.Name())
	}
	if cfg.PersistProvider {
		t.Fatal("a loaded row must never write the persisted default provider")
	}
	if cfg.Resume != nil {
		t.Fatal("--continue never shows a picker")
	}
	opts := built[0]
	if opts.LoadSessionID != "new" {
		t.Fatalf("LoadSessionID %q, want new", opts.LoadSessionID)
	}
	if opts.Title != "the renamed one" || !opts.TitlePinned {
		t.Fatalf("title %q pinned %v", opts.Title, opts.TitlePinned)
	}
	if opts.Provider == nil || opts.Provider.Name() != "grok" {
		t.Fatalf("the session was built for %v", opts.Provider)
	}
}

// TestContinueWithExplicitProviderPersistsIt: --provider on the command line
// is the same explicit choice it has always been, so this run may still write
// it — and it filters the index, which is what makes the cursor row below
// invisible even though it is the newest.
func TestContinueWithExplicitProviderPersistsIt(t *testing.T) {
	indexHome(t)
	ws := t.TempDir()
	seedRow(t, sessions.Row{SessionID: "grok-1", Provider: "grok", CWD: ws, Title: "grok row", TitleKind: sessions.TitleKindAgent}, time.Hour)
	seedRow(t, sessions.Row{SessionID: "cursor-1", Provider: "cursor", CWD: ws, Title: "cursor row", TitleKind: sessions.TitleKindAgent}, time.Minute)

	cfg, built, err := runResolveLoad(t, ws, "--continue", "--provider", "grok")
	if err != nil {
		t.Fatalf("continue: %v", err)
	}
	if !cfg.PersistProvider {
		t.Fatal("an explicit --provider is still a choice this run may persist")
	}
	if built[0].LoadSessionID != "grok-1" {
		t.Fatalf("--provider did not filter the index: %q", built[0].LoadSessionID)
	}
}

// TestIndexIsNotFilteredByAnImplicitProvider is §3.1's pin: $CRAZE_PROVIDER
// and config.toml are defaults for a *new* session. Filtering the index by one
// would hide the thread the user asked to continue behind a preference they
// set for something else — only the flag filters.
func TestIndexIsNotFilteredByAnImplicitProvider(t *testing.T) {
	for _, tc := range []struct {
		name  string
		apply func(t *testing.T, home string)
	}{
		{"CRAZE_PROVIDER", func(t *testing.T, _ string) { t.Setenv("CRAZE_PROVIDER", "grok") }},
		{"config file", func(t *testing.T, _ string) {
			if err := os.WriteFile(os.Getenv("CRAZE_CONFIG"), []byte("provider = \"grok\"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := indexHome(t)
			ws := t.TempDir()
			seedRow(t, sessions.Row{SessionID: "cursor-1", Provider: "cursor", CWD: ws, Title: "cursor row", TitleKind: sessions.TitleKindAgent}, time.Minute)
			tc.apply(t, home)

			cfg, built, err := runResolveLoad(t, ws, "--continue")
			if err != nil {
				t.Fatalf("continue: %v", err)
			}
			if len(built) != 1 || built[0].LoadSessionID != "cursor-1" {
				t.Fatalf("%s filtered the index: %v", tc.name, built)
			}
			if cfg.Provider.Name() != "cursor" {
				t.Fatalf("the row's provider lost to the default: %q", cfg.Provider.Name())
			}
			if cfg.PersistProvider {
				t.Fatal("an implicit provider is no reason to persist one")
			}
		})
	}
}

// TestRowsInAnotherWorkspaceAreNeverOffered: cursor's own session/load is
// permissive across directories (§2.1) and craze is stricter on purpose — a
// row is offered in the workspace it was created in and nowhere else, on every
// provider. The case runs on cursor because it is the one that would otherwise
// succeed.
func TestRowsInAnotherWorkspaceAreNeverOffered(t *testing.T) {
	indexHome(t)
	ws := t.TempDir()
	other := t.TempDir()
	seedRow(t, sessions.Row{SessionID: "elsewhere", Provider: "cursor", CWD: other, Title: "another project", TitleKind: sessions.TitleKindAgent}, time.Minute)

	for _, argv := range [][]string{{"--continue"}, {"--resume"}} {
		_, _, err := runResolveLoad(t, ws, argv...)
		code, _ := exitCode(t, err)
		if code != 1 {
			t.Fatalf("%v: exit %d, want 1", argv, code)
		}
	}
	// The same row, asked for from its own workspace, is there: the filter is
	// the cwd and not a broken read.
	if _, built, err := runResolveLoad(t, other, "--continue"); err != nil || built[0].LoadSessionID != "elsewhere" {
		t.Fatalf("the row is unreachable from its own workspace: %v %v", built, err)
	}
}

// TestResumeOffersTheLastTenNewestFirst: the picker is a window on the index,
// not the whole of it, and the order is the one the user thinks in.
func TestResumeOffersTheLastTenNewestFirst(t *testing.T) {
	indexHome(t)
	ws := t.TempDir()
	for i := 0; i < 14; i++ {
		seedRow(t, sessions.Row{
			SessionID: "s" + string(rune('a'+i)),
			Provider:  "cursor",
			CWD:       ws,
			Title:     "session " + string(rune('a'+i)),
			TitleKind: sessions.TitleKindAgent,
		}, time.Duration(14-i)*time.Hour)
	}
	cfg, built, err := runResolveLoad(t, ws, "--resume")
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if len(built) != 0 {
		t.Fatal("--resume must not build a session before a row is chosen")
	}
	if len(cfg.Resume) != resumeRowLimit {
		t.Fatalf("%d rows, want %d", len(cfg.Resume), resumeRowLimit)
	}
	if cfg.Resume[0].SessionID != "sn" {
		t.Fatalf("newest row is %q, want sn", cfg.Resume[0].SessionID)
	}
	if cfg.PersistProvider {
		t.Fatal("a picked row must not write the persisted default provider")
	}
}

// TestSeedSessionParsesATitleWithAColon: --seed-session is provider:id:title
// with the title taking everything after the second colon, so a golden can
// carry the kind of title people actually write.
func TestSeedSessionParsesATitleWithAColon(t *testing.T) {
	row, err := parseSeedSession("grok:sess-1:fix: the flaky pty test", "/ws")
	if err != nil {
		t.Fatal(err)
	}
	if row.Provider != "grok" || row.SessionID != "sess-1" || row.Title != "fix: the flaky pty test" {
		t.Fatalf("parsed %+v", row)
	}
	if row.CWD != "/ws" {
		t.Fatalf("cwd %q", row.CWD)
	}
	for _, bad := range []string{"grok", "grok:sess-1", ":id:title", "grok::title"} {
		if _, err := parseSeedSession(bad, "/ws"); err == nil {
			t.Fatalf("%q was accepted", bad)
		}
	}
}
