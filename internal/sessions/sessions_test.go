package sessions

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// setIndex points CRAZE_CONFIG at a fresh file under t.TempDir(), so the
// index sits next to it -- never the developer's real ~/.craze/sessions.jsonl.
// Returns the sessions.jsonl path.
func setIndex(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CRAZE_CONFIG", filepath.Join(dir, "config.toml"))
	return filepath.Join(dir, "sessions.jsonl")
}

func mustUpsert(t *testing.T, s *Store, row Row) {
	t.Helper()
	if err := s.Upsert(row); err != nil {
		t.Fatalf("Upsert(%+v): %v", row, err)
	}
}

// --- merge rules -----------------------------------------------------------

func TestUpsertCreatesRowWithFallbackTitle(t *testing.T) {
	setIndex(t)
	var s Store
	mustUpsert(t, &s, Row{SessionID: "s1", Provider: "cursor", CWD: "/ws", Title: "hello world", TitleKind: TitleKindFallback})

	row, ok, err := s.Latest("/ws", "")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("want a row")
	}
	if row.Title != "hello world" {
		t.Fatalf("title = %q", row.Title)
	}
	if row.Pinned {
		t.Fatal("a fallback title must not pin")
	}
	if row.CreatedAt.IsZero() || row.UpdatedAt.IsZero() {
		t.Fatalf("timestamps not set: %+v", row)
	}
	if !row.CreatedAt.Equal(row.UpdatedAt) {
		t.Fatalf("a fresh row's CreatedAt and UpdatedAt should match: %+v", row)
	}
}

func TestUpsertEmptyTitleNeverOverwrites(t *testing.T) {
	setIndex(t)
	var s Store
	mustUpsert(t, &s, Row{SessionID: "s1", Provider: "cursor", CWD: "/ws", Title: "first", TitleKind: TitleKindAgent})
	mustUpsert(t, &s, Row{SessionID: "s1", Provider: "cursor", CWD: "/ws", Title: "", TitleKind: TitleKindUser})

	row, ok, err := s.Latest("/ws", "")
	if err != nil || !ok {
		t.Fatalf("Latest: ok=%v err=%v", ok, err)
	}
	if row.Title != "first" {
		t.Fatalf("empty title overwrote: %q", row.Title)
	}
	if row.Pinned {
		t.Fatal("an empty user title must not pin either")
	}
}

func TestUpsertAgentTitleFillsAndUpdates(t *testing.T) {
	setIndex(t)
	var s Store
	mustUpsert(t, &s, Row{SessionID: "s1", Provider: "cursor", CWD: "/ws", Title: "fallback", TitleKind: TitleKindFallback})
	mustUpsert(t, &s, Row{SessionID: "s1", Provider: "cursor", CWD: "/ws", Title: "agent title", TitleKind: TitleKindAgent})

	row, _, err := s.Latest("/ws", "")
	if err != nil {
		t.Fatal(err)
	}
	if row.Title != "agent title" {
		t.Fatalf("title = %q, want agent title to overwrite an unpinned row", row.Title)
	}
}

func TestUpsertAgentTitleNeverOverwritesPinnedRow(t *testing.T) {
	setIndex(t)
	var s Store
	mustUpsert(t, &s, Row{SessionID: "s1", Provider: "cursor", CWD: "/ws", Title: "renamed by user", TitleKind: TitleKindUser})
	mustUpsert(t, &s, Row{SessionID: "s1", Provider: "cursor", CWD: "/ws", Title: "agent wants this instead", TitleKind: TitleKindAgent})

	row, _, err := s.Latest("/ws", "")
	if err != nil {
		t.Fatal(err)
	}
	if row.Title != "renamed by user" {
		t.Fatalf("agent title overwrote a pinned row: %q", row.Title)

	}
	if !row.Pinned {
		t.Fatal("row should still be pinned")
	}
}

func TestUpsertFallbackTitleOnlyFillsEmpty(t *testing.T) {
	setIndex(t)
	var s Store
	mustUpsert(t, &s, Row{SessionID: "s1", Provider: "cursor", CWD: "/ws", Title: "agent title", TitleKind: TitleKindAgent})
	mustUpsert(t, &s, Row{SessionID: "s1", Provider: "cursor", CWD: "/ws", Title: "fallback title", TitleKind: TitleKindFallback})

	row, _, err := s.Latest("/ws", "")
	if err != nil {
		t.Fatal(err)
	}
	if row.Title != "agent title" {
		t.Fatalf("fallback title overwrote a non-empty title: %q", row.Title)
	}
}

func TestUpsertUserTitleAlwaysWinsAndPins(t *testing.T) {
	setIndex(t)
	var s Store
	mustUpsert(t, &s, Row{SessionID: "s1", Provider: "cursor", CWD: "/ws", Title: "agent title", TitleKind: TitleKindAgent})
	mustUpsert(t, &s, Row{SessionID: "s1", Provider: "cursor", CWD: "/ws", Title: "user chosen", TitleKind: TitleKindUser})

	row, _, err := s.Latest("/ws", "")
	if err != nil {
		t.Fatal(err)
	}
	if row.Title != "user chosen" {
		t.Fatalf("title = %q, want the user title to win", row.Title)
	}
	if !row.Pinned {
		t.Fatal("a user title must pin the row")
	}

	// A subsequent agent title must not override the pin (covered above,
	// TestUpsertAgentTitleNeverOverwritesPinnedRow); double-check a further
	// user title still wins even though the row is now pinned.
	mustUpsert(t, &s, Row{SessionID: "s1", Provider: "cursor", CWD: "/ws", Title: "renamed again", TitleKind: TitleKindUser})
	row, _, err = s.Latest("/ws", "")
	if err != nil {
		t.Fatal(err)
	}
	if row.Title != "renamed again" {
		t.Fatalf("title = %q, want a second user title to win over a pinned row", row.Title)
	}
}

func TestUpsertTouchBumpsUpdatedAtOnly(t *testing.T) {
	setIndex(t)
	var s Store
	mustUpsert(t, &s, Row{SessionID: "s1", Provider: "cursor", CWD: "/ws", Title: "keep me", TitleKind: TitleKindFallback})
	before, _, err := s.Latest("/ws", "")
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(2 * time.Millisecond)
	mustUpsert(t, &s, Row{SessionID: "s1", Provider: "cursor", CWD: "/ws", TitleKind: TitleKindNone})

	after, _, err := s.Latest("/ws", "")
	if err != nil {
		t.Fatal(err)
	}
	if after.Title != "keep me" {
		t.Fatalf("a touch changed the title: %q", after.Title)
	}
	if !after.UpdatedAt.After(before.UpdatedAt) {
		t.Fatalf("touch did not bump UpdatedAt: before=%v after=%v", before.UpdatedAt, after.UpdatedAt)
	}
	if !after.CreatedAt.Equal(before.CreatedAt) {
		t.Fatalf("touch changed CreatedAt: before=%v after=%v", before.CreatedAt, after.CreatedAt)
	}
}

func TestUpsertCreatedAtSetOnce(t *testing.T) {
	setIndex(t)
	var s Store
	mustUpsert(t, &s, Row{SessionID: "s1", Provider: "cursor", CWD: "/ws", Title: "a", TitleKind: TitleKindFallback})
	first, _, err := s.Latest("/ws", "")
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(2 * time.Millisecond)
	mustUpsert(t, &s, Row{SessionID: "s1", Provider: "cursor", CWD: "/ws", Title: "b", TitleKind: TitleKindAgent})
	second, _, err := s.Latest("/ws", "")
	if err != nil {
		t.Fatal(err)
	}
	if !second.CreatedAt.Equal(first.CreatedAt) {
		t.Fatalf("CreatedAt changed on update: %v -> %v", first.CreatedAt, second.CreatedAt)
	}
}

func TestUpsertRequiresIdentityFields(t *testing.T) {
	setIndex(t)
	var s Store
	cases := []Row{
		{Provider: "cursor", CWD: "/ws"},
		{SessionID: "s1", CWD: "/ws"},
		{SessionID: "s1", Provider: "cursor"},
	}
	for _, row := range cases {
		if err := s.Upsert(row); err == nil {
			t.Fatalf("Upsert(%+v) should have failed", row)
		}
	}
}

// --- Latest / Recent ---------------------------------------------------

func TestRecentFiltersByExactCWD(t *testing.T) {
	setIndex(t)
	var s Store
	mustUpsert(t, &s, Row{SessionID: "s1", Provider: "cursor", CWD: "/ws", Title: "a", TitleKind: TitleKindFallback})
	mustUpsert(t, &s, Row{SessionID: "s2", Provider: "cursor", CWD: "/ws/sub", Title: "b", TitleKind: TitleKindFallback})
	mustUpsert(t, &s, Row{SessionID: "s3", Provider: "cursor", CWD: "/other", Title: "c", TitleKind: TitleKindFallback})

	rows, err := s.Recent("/ws", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].SessionID != "s1" {
		t.Fatalf("rows = %+v, want only s1 (exact cwd match, no prefix/symlink logic)", rows)
	}
}

func TestRecentFiltersByProvider(t *testing.T) {
	setIndex(t)
	var s Store
	mustUpsert(t, &s, Row{SessionID: "s1", Provider: "cursor", CWD: "/ws", Title: "a", TitleKind: TitleKindFallback})
	mustUpsert(t, &s, Row{SessionID: "s2", Provider: "grok", CWD: "/ws", Title: "b", TitleKind: TitleKindFallback})

	rows, err := s.Recent("/ws", "grok", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].SessionID != "s2" {
		t.Fatalf("rows = %+v, want only the grok row", rows)
	}

	rows, err = s.Recent("/ws", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("empty provider should mean any provider: %+v", rows)
	}
}

func TestRecentNewestFirst(t *testing.T) {
	setIndex(t)
	var s Store
	mustUpsert(t, &s, Row{SessionID: "s1", Provider: "cursor", CWD: "/ws", Title: "a", TitleKind: TitleKindFallback})
	time.Sleep(2 * time.Millisecond)
	mustUpsert(t, &s, Row{SessionID: "s2", Provider: "cursor", CWD: "/ws", Title: "b", TitleKind: TitleKindFallback})
	time.Sleep(2 * time.Millisecond)
	mustUpsert(t, &s, Row{SessionID: "s3", Provider: "cursor", CWD: "/ws", Title: "c", TitleKind: TitleKindFallback})

	rows, err := s.Recent("/ws", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	got := []string{rows[0].SessionID, rows[1].SessionID, rows[2].SessionID}
	want := []string{"s3", "s2", "s1"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

func TestRecentCapsAtN(t *testing.T) {
	setIndex(t)
	var s Store
	for i := 0; i < 5; i++ {
		mustUpsert(t, &s, Row{SessionID: "s" + strconv.Itoa(i), Provider: "cursor", CWD: "/ws", Title: "t", TitleKind: TitleKindFallback})
		time.Sleep(time.Millisecond)
	}
	rows, err := s.Recent("/ws", "", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("len(rows) = %d, want 2", len(rows))
	}
	if rows[0].SessionID != "s4" || rows[1].SessionID != "s3" {
		t.Fatalf("rows = %+v, want the two newest", rows)
	}
}

// TestRecentTiesBreakByFileOrder writes two rows with an identical
// UpdatedAt (by writing the raw file directly, since Upsert always uses
// time.Now()) and checks the later line in the file sorts first.
func TestRecentTiesBreakByFileOrder(t *testing.T) {
	path := setIndex(t)
	same := "2024-01-01T00:00:00Z"
	body := strings.Join([]string{
		lineFor(t, "a", "cursor", "/ws", same),
		lineFor(t, "b", "cursor", "/ws", same),
	}, "\n") + "\n"
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	var s Store
	rows, err := s.Recent("/ws", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].SessionID != "b" || rows[1].SessionID != "a" {
		t.Fatalf("rows = %+v, want [b, a] (later file line wins the UpdatedAt tie)", rows)
	}
}

func lineFor(t *testing.T, sessionID, provider, cwd, updatedAt string) string {
	t.Helper()
	return `{"sessionId":"` + sessionID + `","provider":"` + provider + `","cwd":"` + cwd + `","title":"t","pinned":false,"createdAt":"` + updatedAt + `","updatedAt":"` + updatedAt + `"}`
}

func TestLatestNoRow(t *testing.T) {
	setIndex(t)
	var s Store
	_, ok, err := s.Latest("/ws", "")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("want no row")
	}
}

// --- cap ------------------------------------------------------------------

func TestUpsertCapsAt500Rows(t *testing.T) {
	setIndex(t)
	var s Store
	for i := 0; i < 501; i++ {
		mustUpsert(t, &s, Row{
			SessionID: "s" + strconv.Itoa(i), Provider: "cursor", CWD: "/ws",
			Title: "t", TitleKind: TitleKindFallback,
		})
	}
	rows, err := s.Recent("/ws", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 500 {
		t.Fatalf("len(rows) = %d, want 500", len(rows))
	}
	for _, row := range rows {
		if row.SessionID == "s0" {
			t.Fatal("the oldest row should have been evicted")
		}
	}
	if rows[0].SessionID != "s500" {
		t.Fatalf("newest row = %q, want s500", rows[0].SessionID)
	}
}

// --- unknown fields ---------------------------------------------------

func TestUnknownFieldsRoundTrip(t *testing.T) {
	path := setIndex(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	line := `{"sessionId":"s1","provider":"cursor","cwd":"/ws","title":"t","pinned":false,` +
		`"createdAt":"2024-01-01T00:00:00Z","updatedAt":"2024-01-01T00:00:00Z","futureField":"kept",` +
		`"futureNum":42}` + "\n"
	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}

	var s Store
	// Touch the row so the file gets rewritten.
	mustUpsert(t, &s, Row{SessionID: "s1", Provider: "cursor", CWD: "/ws", TitleKind: TitleKindNone})

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"futureField":"kept"`) {
		t.Fatalf("unknown string field lost: %s", b)
	}
	if !strings.Contains(string(b), `"futureNum":42`) {
		t.Fatalf("unknown numeric field lost: %s", b)
	}
}

// --- validation on read -------------------------------------------------

func writeRaw(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestReadValidation(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"not a JSON object", `"just a string"` + "\n"},
		{"missing sessionId", `{"provider":"cursor","cwd":"/ws","updatedAt":"2024-01-01T00:00:00Z"}` + "\n"},
		{"empty sessionId", `{"sessionId":"","provider":"cursor","cwd":"/ws","updatedAt":"2024-01-01T00:00:00Z"}` + "\n"},
		{"missing provider", `{"sessionId":"s1","cwd":"/ws","updatedAt":"2024-01-01T00:00:00Z"}` + "\n"},
		{"missing cwd", `{"sessionId":"s1","provider":"cursor","updatedAt":"2024-01-01T00:00:00Z"}` + "\n"},
		{"unparsable updatedAt", `{"sessionId":"s1","provider":"cursor","cwd":"/ws","updatedAt":"not-a-time"}` + "\n"},
		{"missing updatedAt", `{"sessionId":"s1","provider":"cursor","cwd":"/ws"}` + "\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := setIndex(t)
			writeRaw(t, path, tc.body)
			var s Store
			_, err := s.Recent("/ws", "", 10)
			if err == nil {
				t.Fatalf("want an error for %q", tc.name)
			}
			if !strings.Contains(err.Error(), path) {
				t.Fatalf("error %q does not name the file", err)
			}
			if !strings.Contains(err.Error(), ":1:") {
				t.Fatalf("error %q does not name the 1-based line number", err)
			}
		})
	}
}

func TestReadValidationNamesTheRightLine(t *testing.T) {
	path := setIndex(t)
	good := lineFor(t, "s1", "cursor", "/ws", "2024-01-01T00:00:00Z")
	bad := `{"sessionId":"s2","provider":"cursor","cwd":"/ws","updatedAt":"garbage"}`
	writeRaw(t, path, good+"\n"+bad+"\n")

	var s Store
	_, err := s.Recent("/ws", "", 10)
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), ":2:") {
		t.Fatalf("error %q should name line 2", err)
	}
}

func TestBlankLinesSkipped(t *testing.T) {
	path := setIndex(t)
	good := lineFor(t, "s1", "cursor", "/ws", "2024-01-01T00:00:00Z")
	writeRaw(t, path, "\n"+good+"\n\n   \n")

	var s Store
	rows, err := s.Recent("/ws", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].SessionID != "s1" {
		t.Fatalf("rows = %+v", rows)
	}
}

// --- unknown provider ----------------------------------------------------

func TestUnknownProviderKeptButNotOffered(t *testing.T) {
	path := setIndex(t)
	writeRaw(t, path, lineFor(t, "s1", "futureprovider", "/ws", "2024-01-01T00:00:00Z")+"\n")

	s := Store{KnownProvider: func(p string) bool { return p == "cursor" || p == "grok" }}
	rows, err := s.Recent("/ws", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("an unknown provider row should not be offered: %+v", rows)
	}

	// The row must still be in the file -- touch a different, known-provider
	// row and check the unknown one survives the rewrite.
	mustUpsert(t, &s, Row{SessionID: "s2", Provider: "cursor", CWD: "/ws", Title: "t", TitleKind: TitleKindFallback})
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"futureprovider"`) {
		t.Fatalf("unknown-provider row was dropped from the file: %s", b)
	}

	// A nil KnownProvider means every provider is known.
	var open Store
	rows, err = open.Recent("/ws", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("nil KnownProvider should offer every row: %+v", rows)
	}
}

// --- duplicate keys ---------------------------------------------------

func TestDuplicateKeyLastWins(t *testing.T) {
	path := setIndex(t)
	first := lineFor(t, "s1", "cursor", "/ws", "2024-01-01T00:00:00Z")
	second := `{"sessionId":"s1","provider":"cursor","cwd":"/ws","title":"newer","pinned":true,` +
		`"createdAt":"2024-01-01T00:00:00Z","updatedAt":"2024-06-01T00:00:00Z"}`
	writeRaw(t, path, first+"\n"+second+"\n")

	var s Store
	row, ok, err := s.Latest("/ws", "")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("want a row")
	}
	if row.Title != "newer" || !row.Pinned {
		t.Fatalf("row = %+v, want the later duplicate to win", row)
	}
}

// --- no home directory --------------------------------------------------

func TestNoHomeDirectoryReadsEmptyAndWritesError(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("CRAZE_CONFIG", "")
	dir := t.TempDir()
	t.Chdir(dir)

	var s Store
	rows, err := s.Recent("/ws", "", 10)
	if err != nil {
		t.Fatalf("Recent with no home directory should not error: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("rows = %+v, want none", rows)
	}
	_, ok, err := s.Latest("/ws", "")
	if err != nil || ok {
		t.Fatalf("Latest with no home directory: ok=%v err=%v", ok, err)
	}

	err = s.Upsert(Row{SessionID: "s1", Provider: "cursor", CWD: "/ws", Title: "t", TitleKind: TitleKindFallback})
	if err == nil {
		t.Fatal("Upsert with no home directory should error")
	}

	if _, statErr := os.Stat(filepath.Join(dir, "sessions.jsonl")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("a relative ./sessions.jsonl must never be created: %v", statErr)
	}
}

// --- concurrency ---------------------------------------------------------

func TestConcurrentUpsertKeepsBothRows(t *testing.T) {
	setIndex(t)
	var s Store
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		errs <- s.Upsert(Row{SessionID: "s1", Provider: "cursor", CWD: "/ws", Title: "one", TitleKind: TitleKindFallback})
	}()
	go func() {
		defer wg.Done()
		errs <- s.Upsert(Row{SessionID: "s2", Provider: "cursor", CWD: "/ws", Title: "two", TitleKind: TitleKindFallback})
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	rows, err := s.Recent("/ws", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %+v, want both concurrent upserts to survive", rows)
	}
}

// --- lock failure ----------------------------------------------------------

func TestUpsertLockFailureIsAnError(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	t.Setenv("CRAZE_CONFIG", configPath)

	// Make the directory the index would live in read-only. Upsert's own
	// MkdirAll is a no-op (the directory already exists), so this exercises
	// the lock file's os.OpenFile(O_CREATE), not the mkdir.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	var s Store
	err := s.Upsert(Row{SessionID: "s1", Provider: "cursor", CWD: "/ws", Title: "t", TitleKind: TitleKindFallback})
	if err == nil {
		t.Fatal("want an error when the lock file cannot be created")
	}
}

// --- file mode -------------------------------------------------------------

func TestFileModeIs0600(t *testing.T) {
	path := setIndex(t)
	var s Store
	mustUpsert(t, &s, Row{SessionID: "s1", Provider: "cursor", CWD: "/ws", Title: "t", TitleKind: TitleKindFallback})

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
	}
}
