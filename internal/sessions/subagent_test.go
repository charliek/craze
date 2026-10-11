package sessions

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// Sub-agent sessions in the index (plan 041 §3.3, §7 W3 17-18): Latest skips
// marked rows before it limits, eviction drops them first, the picker fills
// from unmarked rows first.

// markedLine is a hand-written index line for a row at a time, marked when
// marked is set. Upsert stamps its own times, so a case that needs to say
// which row is newest (or that two tie) writes the file itself.
func markedLine(id, provider, cwd string, at time.Time, marked bool) string {
	ts := at.UTC().Format(time.RFC3339Nano)
	line := `{"sessionId":"` + id + `","provider":"` + provider + `","cwd":"` + cwd + `","title":"t","pinned":false,"createdAt":"` + ts + `","updatedAt":"` + ts + `"`
	if marked {
		line += `,"unattended":{"effort":"","mode":"","model":"","permissionMode":"bypass"}`
	}
	return line + "}"
}

func writeLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	writeRaw(t, path, strings.Join(lines, "\n")+"\n")
}

var t0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func latestID(t *testing.T, s *Store, cwd, provider string) (string, bool) {
	t.Helper()
	row, ok, err := s.Latest(cwd, provider)
	if err != nil {
		t.Fatal(err)
	}
	return row.SessionID, ok
}

// TestLatestSkipsMarkedRows: criterion 17. A person's session older than one
// marked row, older than several, and the filter applied before the limit.
func TestLatestSkipsMarkedRows(t *testing.T) {
	path := setIndex(t)
	var s Store
	writeLines(t, path,
		markedLine("person", "cursor", "/ws", t0, false),
		markedLine("sub1", "cursor", "/ws", t0.Add(time.Minute), true),
	)
	if id, ok := latestID(t, &s, "/ws", ""); !ok || id != "person" {
		t.Fatalf("one newer marked row: Latest = %q, %v; want person", id, ok)
	}
	writeLines(t, path,
		markedLine("person", "cursor", "/ws", t0, false),
		markedLine("sub1", "cursor", "/ws", t0.Add(time.Minute), true),
		markedLine("sub2", "cursor", "/ws", t0.Add(2*time.Minute), true),
		markedLine("sub3", "cursor", "/ws", t0.Add(3*time.Minute), true),
	)
	if id, ok := latestID(t, &s, "/ws", ""); !ok || id != "person" {
		t.Fatalf("several newer marked rows: Latest = %q, %v; want person", id, ok)
	}
	// The newest unmarked row wins when there are several.
	writeLines(t, path,
		markedLine("old", "cursor", "/ws", t0, false),
		markedLine("person", "cursor", "/ws", t0.Add(time.Minute), false),
		markedLine("sub1", "cursor", "/ws", t0.Add(2*time.Minute), true),
	)
	if id, _ := latestID(t, &s, "/ws", ""); id != "person" {
		t.Fatalf("Latest = %q, want the newest unmarked row", id)
	}
}

// TestLatestAllMarkedIsNone: a directory of sub-agent sessions only has no
// latest row; Recent still lists them and SubAgentCount counts them.
func TestLatestAllMarkedIsNone(t *testing.T) {
	path := setIndex(t)
	var s Store
	writeLines(t, path,
		markedLine("sub1", "cursor", "/ws", t0, true),
		markedLine("sub2", "grok", "/ws", t0.Add(time.Minute), true),
		markedLine("elsewhere", "cursor", "/other", t0, false),
	)
	if id, ok := latestID(t, &s, "/ws", ""); ok {
		t.Fatalf("Latest = %q, want none", id)
	}
	rows, err := s.Recent("/ws", "", 0)
	if err != nil || len(rows) != 2 {
		t.Fatalf("Recent = %d rows, %v; want both marked rows", len(rows), err)
	}
	if n, err := s.SubAgentCount("/ws", ""); err != nil || n != 2 {
		t.Fatalf("SubAgentCount = %d, %v; want 2", n, err)
	}
	if n, _ := s.SubAgentCount("/ws", "grok"); n != 1 {
		t.Fatalf("SubAgentCount with a provider filter = %d, want 1", n)
	}
	if n, _ := s.SubAgentCount("/other", ""); n != 0 {
		t.Fatalf("SubAgentCount of a directory with no marked rows = %d, want 0", n)
	}
}

// TestLatestWithProviderSkipsMarkedRows: the provider filter and the mark
// compose; a newer marked row of the asked provider is still skipped.
func TestLatestWithProviderSkipsMarkedRows(t *testing.T) {
	path := setIndex(t)
	var s Store
	writeLines(t, path,
		markedLine("grok-person", "grok", "/ws", t0, false),
		markedLine("cursor-person", "cursor", "/ws", t0.Add(time.Minute), false),
		markedLine("grok-sub", "grok", "/ws", t0.Add(2*time.Minute), true),
	)
	if id, _ := latestID(t, &s, "/ws", "grok"); id != "grok-person" {
		t.Fatalf("Latest(grok) = %q, want grok-person", id)
	}
	if id, _ := latestID(t, &s, "/ws", ""); id != "cursor-person" {
		t.Fatalf("Latest(any) = %q, want cursor-person", id)
	}
}

// TestLatestTiesOrderAsRecentDoes: equal timestamps resolve to the later row
// in the file, among unmarked rows only.
func TestLatestTiesOrderAsRecentDoes(t *testing.T) {
	path := setIndex(t)
	var s Store
	writeLines(t, path,
		markedLine("a", "cursor", "/ws", t0, false),
		markedLine("b", "cursor", "/ws", t0, false),
		markedLine("sub", "cursor", "/ws", t0, true),
	)
	if id, _ := latestID(t, &s, "/ws", ""); id != "b" {
		t.Fatalf("Latest = %q, want b (the later unmarked row of the tie)", id)
	}
}

// TestEvictionDropsMarkedRowsFirst: criterion 18. At the cap the oldest
// marked row goes before an older unmarked one; once no marked row is left
// the oldest unmarked goes.
func TestEvictionDropsMarkedRowsFirst(t *testing.T) {
	path := setIndex(t)
	var s Store
	lines := make([]string, 0, maxRows)
	for i := 0; i < maxRows; i++ {
		// s0 is the oldest of all and unmarked; s10 and s11 are marked and
		// newer than it.
		marked := i == 10 || i == 11
		lines = append(lines, markedLine("s"+strconv.Itoa(i), "cursor", "/ws", t0.Add(time.Duration(i)*time.Second), marked))
	}
	writeLines(t, path, lines...)

	mustUpsert(t, &s, Row{SessionID: "new1", Provider: "cursor", CWD: "/ws", Title: "t", TitleKind: TitleKindFallback})
	if _, ok, _ := s.Find("cursor", "s10"); ok {
		t.Fatal("the oldest marked row survived the overflow")
	}
	if _, ok, _ := s.Find("cursor", "s0"); !ok {
		t.Fatal("the oldest unmarked row was evicted while marked rows remained")
	}
	if _, ok, _ := s.Find("cursor", "s11"); !ok {
		t.Fatal("one overflow dropped more than one row")
	}

	mustUpsert(t, &s, Row{SessionID: "new2", Provider: "cursor", CWD: "/ws", Title: "t", TitleKind: TitleKindFallback})
	if _, ok, _ := s.Find("cursor", "s11"); ok {
		t.Fatal("the second marked row survived")
	}
	mustUpsert(t, &s, Row{SessionID: "new3", Provider: "cursor", CWD: "/ws", Title: "t", TitleKind: TitleKindFallback})
	if _, ok, _ := s.Find("cursor", "s0"); ok {
		t.Fatal("with no marked row left, the oldest unmarked row should go")
	}
}

// TestPickerFillsFromUnmarkedRowsFirst: the picker's n rows are the unmarked
// ones, newest first, then the marked ones for any remainder.
func TestPickerFillsFromUnmarkedRowsFirst(t *testing.T) {
	path := setIndex(t)
	var s Store
	writeLines(t, path,
		markedLine("p1", "cursor", "/ws", t0, false),
		markedLine("sub1", "cursor", "/ws", t0.Add(1*time.Minute), true),
		markedLine("p2", "cursor", "/ws", t0.Add(2*time.Minute), false),
		markedLine("sub2", "cursor", "/ws", t0.Add(3*time.Minute), true),
		markedLine("sub3", "cursor", "/ws", t0.Add(4*time.Minute), true),
	)
	ids := func(n int) string {
		rows, err := s.Picker("/ws", "", n)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, r := range rows {
			out = append(out, r.SessionID)
		}
		return strings.Join(out, ",")
	}
	if got := ids(2); got != "p2,p1" {
		t.Fatalf("Picker(2) = %s, want the two unmarked rows", got)
	}
	if got := ids(4); got != "p2,p1,sub3,sub2" {
		t.Fatalf("Picker(4) = %s, want unmarked first, then marked newest first", got)
	}
	if got := ids(0); got != "p2,p1,sub3,sub2,sub1" {
		t.Fatalf("Picker(0) = %s", got)
	}
}
