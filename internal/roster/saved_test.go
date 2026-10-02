package roster_test

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/charliek/craze/internal/roster"
	"github.com/charliek/craze/internal/rundir"
	"github.com/charliek/craze/internal/sessions"
)

// The saved half on its own (roster.Saved, plan 032 §3.13): what the hub's
// list roster keeps beside the hub's running rows — the poller's own rules,
// over rows that did not come from a poll.

// flakyIndex is an index whose reads can fail.
type flakyIndex struct {
	mu   sync.Mutex
	rows []sessions.Row
	err  error
	n    int
}

func (f *flakyIndex) All() ([]sessions.Row, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	return append([]sessions.Row(nil), f.rows...), f.err
}

func (f *flakyIndex) set(rows []sessions.Row, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows, f.err = rows, err
}

func (f *flakyIndex) reads() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.n
}

// TestASnapshotSavesRunning: SavesRunning says a Snapshot lists saved a
// session its running rows serve — by craze id, through a row's host or its
// answered session, or a legacy row by its provider session id — and says no
// of the same rows with that saved row taken out (the negative control).
func TestASnapshotSavesRunning(t *testing.T) {
	saved := sessions.Row{SessionID: "p-7", Provider: "cursor", CWD: "/w", Title: "seven", CrazeID: sessionID(7)}
	legacy := sessions.Row{SessionID: "legacy-1", Provider: "grok", CWD: "/w", Title: "a legacy row"}
	other := sessions.Row{SessionID: "p-5", Provider: "cursor", CWD: "/w", Title: "five", CrazeID: sessionID(5)}
	byHost := roster.Row{Host: roster.Host{ID: hostID(7), CrazeSessionID: sessionID(7)}, Status: roster.Connecting}
	bySession := roster.Row{Host: roster.Host{ID: hostID(1)}, Status: roster.Reachable,
		Session: &roster.Session{ID: sessionID(1), Provider: "grok", ProviderSessionID: "legacy-1"}}
	for _, tc := range []struct {
		name string
		s    roster.Snapshot
		want bool
	}{
		{"a craze id its host names", roster.Snapshot{Running: []roster.Row{byHost}, Saved: []sessions.Row{saved, other}}, true},
		{"a legacy row its session names", roster.Snapshot{Running: []roster.Row{bySession}, Saved: []sessions.Row{legacy, other}}, true},
		{"neither", roster.Snapshot{Running: []roster.Row{byHost, bySession}, Saved: []sessions.Row{other}}, false},
		{"nothing running", roster.Snapshot{Saved: []sessions.Row{saved, legacy}}, false},
	} {
		if got := tc.s.SavesRunning(); got != tc.want {
			t.Errorf("%s: SavesRunning = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestSavedIsThePollersSavedHalf: Saved keeps the poller's rules over rows
// handed to it — the index read once while its file's stamp stands and again
// once it moves; the newest row per craze id, a legacy row by its own id, a
// provider craze can resume; every session a running row names taken out,
// by its host's craze id, its answered session's id, or a legacy row's
// provider session id; the index's title per craze id — and Subtract takes
// the running out again with no read. The negative controls are the
// poller's: TestUnreachableIsNeverSaved's same index and running session
// give the same rows, and a Read with nothing moved says nothing changed.
func TestSavedIsThePollersSavedHalf(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.jsonl")
	if err := os.WriteFile(path, []byte("one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	at := func(h int) time.Time { return time.Date(2026, 9, 29, h, 0, 0, 0, time.UTC) }
	idx := &flakyIndex{rows: []sessions.Row{
		{SessionID: "p-7b", Provider: "cursor", CWD: "/w", Title: "newest of 7", CrazeID: sessionID(7), UpdatedAt: at(9)},
		{SessionID: "legacy", Provider: "grok", CWD: "/w", Title: "a legacy row", UpdatedAt: at(8)},
		{SessionID: "p-7a", Provider: "cursor", CWD: "/w", Title: "older of 7", CrazeID: sessionID(7), UpdatedAt: at(7)},
		{SessionID: "p-5", Provider: "cursor", CWD: "/w", Title: "five", CrazeID: sessionID(5), UpdatedAt: at(6)},
		{SessionID: "p-x", Provider: "nosuch", CWD: "/w", Title: "unknown provider", CrazeID: "x", UpdatedAt: at(5)},
	}}
	s := roster.NewSaved(idx, func() string { return path })
	// A running row as the hub lists it: its host by id and craze id, no
	// socket, its session not read yet.
	seven := roster.Row{Host: roster.HostOf(rundir.Entry{HostID: hostID(7), CrazeSessionID: sessionID(7), Provider: "cursor"}),
		Status: roster.Connecting}
	if !s.Read([]roster.Row{seven}) {
		t.Fatal("the first read said nothing changed")
	}
	if got := titles(s.Rows()); got != "a legacy row|five" {
		t.Fatalf("saved %q with session 7 running", got)
	}
	if got := s.Title(sessionID(7)); got != "newest of 7" {
		t.Fatalf("session 7's index title %q", got)
	}
	if s.Read([]roster.Row{seven}) || idx.reads() != 1 {
		t.Fatalf("a read with nothing moved: %d reads of the index", idx.reads())
	}

	// Subtract reads nothing: the running moved, the index did not.
	if !s.Subtract(nil) || idx.reads() != 1 {
		t.Fatalf("Subtract of nothing running read the index (%d reads) or changed nothing", idx.reads())
	}
	if got := titles(s.Rows()); got != "newest of 7|a legacy row|five" {
		t.Fatalf("saved %q with nothing running", got)
	}
	// A legacy row is running by its provider session id, as an answered
	// session names it; a craze id by the session's own id.
	legacy := roster.Row{Host: roster.Host{ID: hostID(1)}, Status: roster.Reachable,
		Session: &roster.Session{ID: sessionID(5), Provider: "grok", ProviderSessionID: "legacy"}}
	if !s.Subtract([]roster.Row{legacy}) {
		t.Fatal("Subtract of the legacy row's and session 5's host changed nothing")
	}
	if got := titles(s.Rows()); got != "newest of 7" {
		t.Fatalf("saved %q with the legacy row's session and session 5 running", got)
	}

	// The file's stamp moves: read again — a title alone moving is a change.
	rows := idx.rows
	rows[0].Title = "renamed 7"
	idx.set(rows, nil)
	if err := os.WriteFile(path, []byte("one\ntwo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !s.Read([]roster.Row{seven}) || idx.reads() != 2 {
		t.Fatalf("a read once the index moved and a running title changed: %d reads", idx.reads())
	}
	if got := s.Title(sessionID(7)); got != "renamed 7" {
		t.Fatalf("session 7's index title %q after the read", got)
	}

	// A read that fails keeps the last good rows and says why.
	idx.set(nil, errors.New("the index is gone"))
	if err := os.WriteFile(path, []byte("three\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !s.Read([]roster.Row{seven}) || s.Err() == nil || titles(s.Rows()) != "a legacy row|five" {
		t.Fatalf("a failed read: err %v, saved %q", s.Err(), titles(s.Rows()))
	}
}
