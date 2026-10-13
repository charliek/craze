package roster_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/roster"
	"github.com/charliek/craze/internal/rundir"
	"github.com/charliek/craze/internal/sessions"
)

// SavedSessions (plan 046 B2, B-3): the one choice of the saved sessions the
// session list's saved rows and the hub's sessions.saved both make — the
// candidates, less the running, then the workspace filter, the sub-agent
// filter, the order and the limit, in that order — and RunningOfEntries,
// which counts no stopping host.

// savedAt is an hour of a fixed day: an index row's UpdatedAt.
func savedAt(h int) time.Time { return time.Date(2026, 10, 12, h, 0, 0, 0, time.UTC) }

// ids is each row's SavedID, in order.
func ids(rows []sessions.Row) []string {
	var out []string
	for _, r := range rows {
		out = append(out, roster.SavedID(r))
	}
	return out
}

// TestSavedSessionsCandidates: of a provider craze can resume only; the
// newest row per craze id, carrying the craze id's newest title — a newer
// row's of a provider it does not know too — a legacy row by its own
// <provider>:<sessionId>; less every session a serving entry names, by craze
// id or, for a legacy row, its provider and provider session id.
func TestSavedSessionsCandidates(t *testing.T) {
	all := []sessions.Row{
		{SessionID: "p-9", Provider: "nosuch", CWD: "/w", Title: "renamed elsewhere", CrazeID: "c-7", UpdatedAt: savedAt(10)},
		{SessionID: "p-7b", Provider: "cursor", CWD: "/w", Title: "older title", CrazeID: "c-7", UpdatedAt: savedAt(9)},
		{SessionID: "legacy-1", Provider: "grok", CWD: "/w", Title: "a legacy row", UpdatedAt: savedAt(8)},
		{SessionID: "p-7a", Provider: "cursor", CWD: "/w", Title: "oldest of 7", CrazeID: "c-7", UpdatedAt: savedAt(7)},
		{SessionID: "p-5", Provider: "native", CWD: "/w", Title: "five", CrazeID: "c-5", UpdatedAt: savedAt(6)},
		{SessionID: "p-x", Provider: "nosuch", CWD: "/w", Title: "unknown provider", CrazeID: "c-x", UpdatedAt: savedAt(5)},
		{SessionID: "legacy-2", Provider: "cursor", CWD: "/w", Title: "another legacy row", UpdatedAt: savedAt(4)},
	}
	rows, more := roster.SavedSessions(all, roster.RunningOfEntries(nil), roster.SavedQuery{})
	if got, want := ids(rows), []string{"c-7", "grok:legacy-1", "c-5", "cursor:legacy-2"}; !slices.Equal(got, want) || more {
		t.Fatalf("nothing running: %v (more %v), want %v", got, more, want)
	}
	if rows[0].SessionID != "p-7b" || rows[0].Title != "renamed elsewhere" {
		t.Fatalf("craze id c-7's row %+v: want its newest cursor row carrying its newest title", rows[0])
	}
	running := roster.RunningOfEntries([]rundir.Entry{
		{HostID: "000000000001", CrazeSessionID: "c-7", Provider: "cursor", ProviderSessionID: "p-7c"},
		// A host serving the legacy row's provider session under a craze id of
		// its own: the legacy row is running by its provider and its id.
		{HostID: "000000000002", CrazeSessionID: "c-new", Provider: "grok", ProviderSessionID: "legacy-1"},
		// The same provider session id under another provider is another
		// session.
		{HostID: "000000000003", CrazeSessionID: "c-other", Provider: "grok", ProviderSessionID: "legacy-2"},
	})
	rows, _ = roster.SavedSessions(all, running, roster.SavedQuery{})
	if got, want := ids(rows), []string{"c-5", "cursor:legacy-2"}; !slices.Equal(got, want) {
		t.Fatalf("c-7 and legacy-1 running: %v, want %v", got, want)
	}
}

// TestARowWhoseProviderSessionRunsIsNotSaved (C6 review): a row with a
// craze id is running by its provider session too — another CRAZE_HOME's
// index can have given that provider session another craze id, and the
// registry is the whole HOME's — so a host serving the row's provider and
// provider session id under a craze id of its own takes it out. The same
// host serving another provider session id, or the same id of another
// provider, leaves it saved (the negative controls).
func TestARowWhoseProviderSessionRunsIsNotSaved(t *testing.T) {
	all := []sessions.Row{{SessionID: "P", Provider: "cursor", CWD: "/w", Title: "a", CrazeID: "A", UpdatedAt: savedAt(9)}}
	for _, tc := range []struct {
		name  string
		entry rundir.Entry
		saved bool
	}{
		{"its provider session under another craze id", rundir.Entry{HostID: "000000000001", CrazeSessionID: "B", Provider: "cursor", ProviderSessionID: "P"}, false},
		{"another provider session", rundir.Entry{HostID: "000000000001", CrazeSessionID: "B", Provider: "cursor", ProviderSessionID: "Q"}, true},
		{"the same id of another provider", rundir.Entry{HostID: "000000000001", CrazeSessionID: "B", Provider: "grok", ProviderSessionID: "P"}, true},
	} {
		rows, _ := roster.SavedSessions(all, roster.RunningOfEntries([]rundir.Entry{tc.entry}), roster.SavedQuery{})
		if got := len(rows) == 1; got != tc.saved {
			t.Errorf("%s: saved %v, want %v", tc.name, got, tc.saved)
		}
	}
	// A snapshot listing it saved beside such a running row is caught.
	snap := roster.Snapshot{Saved: all, Running: []roster.Row{{Host: roster.Host{ID: "000000000001"}, Status: roster.Reachable,
		Session: &roster.Session{ID: "B", Provider: "cursor", ProviderSessionID: "P"}}}}
	if !snap.SavesRunning() {
		t.Error("SavesRunning misses a saved row whose provider session runs under another craze id")
	}
}

// TestAStoppingHostsSessionIsSaved (B-3): a host whose entry is marked
// stopping is not counted running, so its session is listed saved at once;
// the same entry unmarked takes it out (the negative control).
func TestAStoppingHostsSessionIsSaved(t *testing.T) {
	all := []sessions.Row{
		{SessionID: "p-1", Provider: "cursor", CWD: "/w", Title: "one", CrazeID: "c-1", UpdatedAt: savedAt(9)},
		{SessionID: "legacy", Provider: "grok", CWD: "/w", Title: "legacy", UpdatedAt: savedAt(8)},
	}
	entries := []rundir.Entry{
		{HostID: "000000000001", CrazeSessionID: "c-1", Provider: "cursor", ProviderSessionID: "p-1"},
		{HostID: "000000000002", CrazeSessionID: "c-2", Provider: "grok", ProviderSessionID: "legacy"},
	}
	if rows, _ := roster.SavedSessions(all, roster.RunningOfEntries(entries), roster.SavedQuery{}); len(rows) != 0 {
		t.Fatalf("both serving: saved %v, want none", ids(rows))
	}
	for i := range entries {
		entries[i].Stopping = true
	}
	rows, _ := roster.SavedSessions(all, roster.RunningOfEntries(entries), roster.SavedQuery{})
	if got, want := ids(rows), []string{"c-1", "grok:legacy"}; !slices.Equal(got, want) {
		t.Fatalf("both stopping: saved %v, want %v", got, want)
	}
}

// TestSavedSessionsFiltersOrderAndLimit: the workspace filter is an exact
// match; a sub-agent's session is left out unless asked for; the order is
// newest first, ties by id whatever the index's order; the limit cuts after
// every filter, more saying it left some out — and not when it cut nothing.
func TestSavedSessionsFiltersOrderAndLimit(t *testing.T) {
	sub := &sessions.Unattended{PermissionMode: sessions.PermissionBypass}
	all := []sessions.Row{
		{SessionID: "p-b", Provider: "cursor", CWD: "/a", CrazeID: "c-b", UpdatedAt: savedAt(9)},
		{SessionID: "p-a", Provider: "cursor", CWD: "/a", CrazeID: "c-a", UpdatedAt: savedAt(9)},
		{SessionID: "p-s", Provider: "cursor", CWD: "/a", CrazeID: "c-s", UpdatedAt: savedAt(8), Unattended: sub},
		{SessionID: "p-c", Provider: "cursor", CWD: "/a/", CrazeID: "c-c", UpdatedAt: savedAt(7)},
		{SessionID: "p-d", Provider: "cursor", CWD: "/b", CrazeID: "c-d", UpdatedAt: savedAt(6)},
	}
	none := roster.RunningOfEntries(nil)
	for _, tc := range []struct {
		name string
		q    roster.SavedQuery
		want []string
		more bool
	}{
		{"everything", roster.SavedQuery{}, []string{"c-a", "c-b", "c-c", "c-d"}, false},
		{"sub-agents too", roster.SavedQuery{SubAgents: true}, []string{"c-a", "c-b", "c-s", "c-c", "c-d"}, false},
		{"one workspace, exactly", roster.SavedQuery{Workspace: "/a"}, []string{"c-a", "c-b"}, false},
		{"one workspace, sub-agents too", roster.SavedQuery{Workspace: "/a", SubAgents: true}, []string{"c-a", "c-b", "c-s"}, false},
		{"the limit", roster.SavedQuery{Limit: 2}, []string{"c-a", "c-b"}, true},
		{"the limit, exactly", roster.SavedQuery{Limit: 4}, []string{"c-a", "c-b", "c-c", "c-d"}, false},
		{"the limit, more than there are", roster.SavedQuery{Limit: 5}, []string{"c-a", "c-b", "c-c", "c-d"}, false},
		{"a workspace, then the limit", roster.SavedQuery{Workspace: "/b", Limit: 1}, []string{"c-d"}, false},
	} {
		rows, more := roster.SavedSessions(all, none, tc.q)
		if got := ids(rows); !slices.Equal(got, tc.want) || more != tc.more {
			t.Errorf("%s: %v (more %v), want %v (more %v)", tc.name, got, more, tc.want, tc.more)
		}
	}
}

// TestSavedSessionsFiltersBeforeTheCut: with the newest sessions all in
// another workspace, or all a sub-agent's, a limit still lists the ones its
// filter keeps — the cut comes after every filter — and more says only that
// the filter's own matches went past the limit.
func TestSavedSessionsFiltersBeforeTheCut(t *testing.T) {
	sub := &sessions.Unattended{PermissionMode: sessions.PermissionBypass}
	var all []sessions.Row
	for i := range 6 {
		all = append(all, sessions.Row{SessionID: fmt.Sprintf("p-new-%d", i), Provider: "cursor", CWD: "/other",
			CrazeID: fmt.Sprintf("c-new-%d", i), UpdatedAt: savedAt(20 - i)})
	}
	for i := range 3 {
		all = append(all, sessions.Row{SessionID: fmt.Sprintf("p-sub-%d", i), Provider: "cursor", CWD: "/mine",
			CrazeID: fmt.Sprintf("c-sub-%d", i), UpdatedAt: savedAt(13 - i), Unattended: sub})
	}
	for i := range 3 {
		all = append(all, sessions.Row{SessionID: fmt.Sprintf("p-mine-%d", i), Provider: "cursor", CWD: "/mine",
			CrazeID: fmt.Sprintf("c-mine-%d", i), UpdatedAt: savedAt(9 - i)})
	}
	rows, more := roster.SavedSessions(all, roster.RunningOfEntries(nil), roster.SavedQuery{Workspace: "/mine", Limit: 2})
	if got, want := ids(rows), []string{"c-mine-0", "c-mine-1"}; !slices.Equal(got, want) || !more {
		t.Fatalf("/mine, limit 2: %v (more %v), want %v and more", got, more, want)
	}
	rows, more = roster.SavedSessions(all, roster.RunningOfEntries(nil), roster.SavedQuery{Workspace: "/mine", Limit: 3})
	if got, want := ids(rows), []string{"c-mine-0", "c-mine-1", "c-mine-2"}; !slices.Equal(got, want) || more {
		t.Fatalf("/mine, limit 3: %v (more %v), want %v and no more", got, more, want)
	}
}

// TestSavedSessionsSkipsAMalformedRow: a row over a wire bound — its craze
// id or legacy id over SavedIDMax bytes, its provider session id over
// SavedProviderSessionIDMax, its workspace over SavedWorkspaceMax or not
// absolute — is skipped as if the index had no such line: a craze id's next
// row stands in for its newest when only the newest is malformed. A row at
// each bound is listed (the negative control).
func TestSavedSessionsSkipsAMalformedRow(t *testing.T) {
	long := func(n int) string { return strings.Repeat("x", n) }
	ws := func(n int) string { return "/" + long(n-1) }
	at := []sessions.Row{
		{SessionID: "p-1", Provider: "cursor", CWD: ws(protocol.SavedWorkspaceMax), CrazeID: long(protocol.SavedIDMax), UpdatedAt: savedAt(9)},
		{SessionID: long(protocol.SavedProviderSessionIDMax), Provider: "cursor", CWD: "/w", CrazeID: "c-2", UpdatedAt: savedAt(8)},
		{SessionID: long(protocol.SavedIDMax - len("grok:")), Provider: "grok", CWD: "/w", UpdatedAt: savedAt(7)},
	}
	if rows, _ := roster.SavedSessions(at, roster.RunningOfEntries(nil), roster.SavedQuery{}); len(rows) != 3 {
		t.Fatalf("rows at their bounds: %d listed, want 3", len(rows))
	}
	over := []sessions.Row{
		{SessionID: "p-1", Provider: "cursor", CWD: "/w", CrazeID: long(protocol.SavedIDMax + 1), UpdatedAt: savedAt(9)},
		{SessionID: long(protocol.SavedProviderSessionIDMax + 1), Provider: "cursor", CWD: "/w", CrazeID: "c-2", UpdatedAt: savedAt(8)},
		{SessionID: long(protocol.SavedIDMax - len("grok:") + 1), Provider: "grok", CWD: "/w", UpdatedAt: savedAt(7)},
		{SessionID: "p-4", Provider: "cursor", CWD: ws(protocol.SavedWorkspaceMax + 1), CrazeID: "c-4", UpdatedAt: savedAt(6)},
		{SessionID: "p-5", Provider: "cursor", CWD: "relative/dir", CrazeID: "c-5", UpdatedAt: savedAt(5)},
		// c-6's newest row is malformed: its older one is the session's row,
		// with its own title.
		{SessionID: "p-6b", Provider: "cursor", CWD: "rel", Title: "malformed newest", CrazeID: "c-6", UpdatedAt: savedAt(4)},
		{SessionID: "p-6a", Provider: "cursor", CWD: "/w", Title: "the good one", CrazeID: "c-6", UpdatedAt: savedAt(3)},
	}
	rows, more := roster.SavedSessions(over, roster.RunningOfEntries(nil), roster.SavedQuery{})
	if len(rows) != 1 || rows[0].SessionID != "p-6a" || rows[0].Title != "the good one" || more {
		t.Fatalf("malformed rows: %+v (more %v), want c-6's good row alone", rows, more)
	}
}

// TestSavedSessionsBoundsAreBytes (C6 review): the row bounds are bytes,
// whatever the characters — a craze id, a provider session id and a
// workspace of two-byte characters one byte under and at its bound are
// listed, one byte over is skipped — where a count of characters would let
// the over-long ones through.
func TestSavedSessionsBoundsAreBytes(t *testing.T) {
	// sized is a string of n bytes, two-byte characters but for a last
	// ASCII byte when n is odd.
	sized := func(prefix string, n int) string {
		n -= len(prefix)
		return prefix + strings.Repeat("é", n/2) + strings.Repeat("x", n%2)
	}
	for _, n := range []int{protocol.SavedIDMax - 1, protocol.SavedIDMax, protocol.SavedIDMax + 1} {
		want := n <= protocol.SavedIDMax
		rows := []sessions.Row{
			{SessionID: "p", Provider: "cursor", CWD: "/w", CrazeID: sized("", n), UpdatedAt: savedAt(9)},
			{SessionID: sized("", n), Provider: "cursor", CWD: "/w", CrazeID: "c-sid", UpdatedAt: savedAt(8)},
			{SessionID: sized("", n-len("grok:")), Provider: "grok", CWD: "/w", UpdatedAt: savedAt(7)},
		}
		for i, r := range rows {
			if len(roster.SavedID(r)) > protocol.SavedIDMax+1 || len(r.SessionID) > protocol.SavedProviderSessionIDMax+1 {
				t.Fatalf("row %d is not the size meant: %d, %d bytes", i, len(roster.SavedID(r)), len(r.SessionID))
			}
			got, _ := roster.SavedSessions([]sessions.Row{r}, roster.RunningOfEntries(nil), roster.SavedQuery{})
			if (len(got) == 1) != want {
				t.Errorf("row %d at %d bytes (%d characters): listed %v, want %v", i, n, len([]rune(sized("", n))), len(got) == 1, want)
			}
		}
	}
	for _, n := range []int{protocol.SavedWorkspaceMax - 1, protocol.SavedWorkspaceMax, protocol.SavedWorkspaceMax + 1} {
		r := sessions.Row{SessionID: "p", Provider: "cursor", CWD: sized("/", n), CrazeID: "c-ws", UpdatedAt: savedAt(9)}
		got, _ := roster.SavedSessions([]sessions.Row{r}, roster.RunningOfEntries(nil), roster.SavedQuery{})
		if want := n <= protocol.SavedWorkspaceMax; (len(got) == 1) != want || len(r.CWD) != n {
			t.Errorf("a workspace of %d bytes: listed %v, want %v", len(r.CWD), len(got) == 1, want)
		}
	}
}

// TestSavedSessionsSkipsATimeTheWireCannotWrite (C6 review): an index time
// that parses with an offset carrying it past the year 9999 in UTC cannot be
// written (time.Time.MarshalJSON refuses it), so its row is malformed —
// skipped before the craze ids are deduplicated and before the cut, never
// taking a place the limit counts: the craze id's older, well-formed row
// stands for it, and the limit's places go to rows that can be written.
func TestSavedSessionsSkipsATimeTheWireCannotWrite(t *testing.T) {
	late := time.Date(9999, 12, 31, 23, 59, 59, 0, time.FixedZone("-01:00", -3600))
	if _, err := late.UTC().MarshalJSON(); err == nil {
		t.Fatal("the late time can be written: the test proves nothing")
	}
	all := []sessions.Row{
		{SessionID: "p-late", Provider: "cursor", CWD: "/w", Title: "late", CrazeID: "c-1", UpdatedAt: late},
		{SessionID: "p-born", Provider: "cursor", CWD: "/w", Title: "born late", CrazeID: "c-2", CreatedAt: late, UpdatedAt: savedAt(9)},
		{SessionID: "p-1", Provider: "cursor", CWD: "/w", Title: "one", CrazeID: "c-1", UpdatedAt: savedAt(8)},
		{SessionID: "p-3", Provider: "cursor", CWD: "/w", Title: "three", CrazeID: "c-3", UpdatedAt: savedAt(7)},
		{SessionID: "p-4", Provider: "cursor", CWD: "/w", Title: "four", CrazeID: "c-4", UpdatedAt: savedAt(6)},
	}
	rows, more := roster.SavedSessions(all, roster.RunningOfEntries(nil), roster.SavedQuery{Limit: 2})
	if got, want := ids(rows), []string{"c-1", "c-3"}; !slices.Equal(got, want) || !more || rows[0].SessionID != "p-1" {
		t.Fatalf("limit 2: %v (more %v), want %v and more, c-1 its well-formed row", got, more, want)
	}
	rows, more = roster.SavedSessions(all, roster.RunningOfEntries(nil), roster.SavedQuery{Limit: 3})
	if got, want := ids(rows), []string{"c-1", "c-3", "c-4"}; !slices.Equal(got, want) || more {
		t.Fatalf("limit 3: %v (more %v), want %v and no more", got, more, want)
	}
}
