package roster_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/roster"
)

// The session list's state rule, shared (plan 030 §3.10's table; plan 032
// §3.12): roster.Row.State is what the TUI's list groups a row by and what
// craze ps prints. The expectations here are the table's own, written out —
// not engine.RowStateOf's answers read back — so a change to either the rule
// or its use here fails one of them.

var (
	sinceAt   = time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	startedAt = time.Date(2026, 9, 30, 11, 0, 0, 0, time.UTC)
)

// stateCases is the table: a session as its host answered, and its state.
var stateCases = []struct {
	name string
	s    roster.Session
	want roster.State
}{
	{"idle", roster.Session{Activity: engine.ActivityIdle}, roster.StateIdle},
	{"idle after a done turn", roster.Session{Activity: engine.ActivityIdle, LastTurn: &engine.LastTurn{Outcome: engine.TurnDone}}, roster.StateIdle},
	{"idle after a cancelled turn", roster.Session{Activity: engine.ActivityIdle, LastTurn: &engine.LastTurn{Outcome: engine.TurnCancelled}}, roster.StateIdle},
	{"working", roster.Session{Activity: engine.ActivityWorking}, roster.StateWorking},
	{"starting engine", roster.Session{Activity: engine.ActivityStarting}, roster.StateWorking},
	{"replaying", roster.Session{Activity: engine.ActivityReplaying}, roster.StateWorking},
	{"closing", roster.Session{Activity: engine.ActivityClosing}, roster.StateWorking},
	{"a foreign turn over idle", roster.Session{Activity: engine.ActivityIdle, ForeignTurn: true}, roster.StateWorking},
	{"an ask over working", roster.Session{Activity: engine.ActivityWorking, PendingAsks: 1}, roster.StateNeedsYou},
	{"an ask over a failed turn", roster.Session{Activity: engine.ActivityIdle, PendingAsks: 2, LastTurn: &engine.LastTurn{Outcome: engine.TurnFailed}}, roster.StateNeedsYou},
	{"a failed start", roster.Session{Activity: engine.ActivityIdle, StartFailed: true}, roster.StateFailed},
	{"a failed turn", roster.Session{Activity: engine.ActivityIdle, LastTurn: &engine.LastTurn{Outcome: engine.TurnFailed}}, roster.StateFailed},
	{"a failed turn beside a foreign one", roster.Session{Activity: engine.ActivityIdle, ForeignTurn: true, LastTurn: &engine.LastTurn{Outcome: engine.TurnFailed}}, roster.StateFailed},
	{"an error whose ending is on its way", roster.Session{Activity: engine.ActivityError}, roster.StateFailed},
	{"an error a foreign turn superseded", roster.Session{Activity: engine.ActivityError, ForeignTurn: true}, roster.StateWorking},
	{"an error a done turn ended", roster.Session{Activity: engine.ActivityError, LastTurn: &engine.LastTurn{Outcome: engine.TurnDone}}, roster.StateIdle},
}

// TestSessionStateIsTheListsTable: an answered session's state, row by row
// of the table, and its words.
func TestSessionStateIsTheListsTable(t *testing.T) {
	for _, c := range stateCases {
		s := c.s
		if got := s.State(); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
	words := map[roster.State]string{
		roster.StateNeedsYou: "needs you", roster.StateWorking: "working", roster.StateStarting: "starting",
		roster.StateFailed: "failed", roster.StateIdle: "idle", roster.StateUnreachable: "unreachable",
	}
	for st, w := range words {
		if st.String() != w {
			t.Errorf("state %d reads %q, want %q", st, st.String(), w)
		}
	}
}

// TestRowStateAndSince: a row's status comes first — unreachable whatever
// its last row said, starting while there is no answer — and its since is
// the session's, the last one read for an unreachable host, and the host's
// start while it is starting.
func TestRowStateAndSince(t *testing.T) {
	answered := &roster.Session{Activity: engine.ActivityWorking, Since: sinceAt}
	host := roster.Host{ID: "0123456789ab", CrazeSessionID: "s-1", Ready: true, StartedAt: startedAt}
	for _, c := range []struct {
		name  string
		row   roster.Row
		want  roster.State
		since time.Time
	}{
		{"reachable", roster.Row{Host: host, Status: roster.Reachable, Session: answered}, roster.StateWorking, sinceAt},
		{"unreachable, its last row kept", roster.Row{Host: host, Status: roster.Unreachable, Session: answered}, roster.StateUnreachable, sinceAt},
		{"unreachable, never read", roster.Row{Host: host, Status: roster.Unreachable}, roster.StateUnreachable, time.Time{}},
		{"connecting, a row from before", roster.Row{Host: host, Status: roster.Connecting, Session: answered}, roster.StateStarting, startedAt},
		{"reachable, no session yet", roster.Row{Host: host, Status: roster.Reachable}, roster.StateStarting, startedAt},
	} {
		if got := c.row.State(); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
		if got := c.row.Since(); !got.Equal(c.since) {
			t.Errorf("%s: since %v, want %v", c.name, got, c.since)
		}
	}
}

// TestFromRosterRow: a hub's roster row read as the list's roster reads a
// host's — its host from row.host, its session decoded tolerantly from row
// (a member this build does not know ignored), its status in the roster's
// words (an unknown one connecting) — and the state rule applied to it. A
// row that does not decode is an error; an absent one is no session.
func TestFromRosterRow(t *testing.T) {
	row := protocol.SessionRow{
		SessionInfo: protocol.SessionInfo{SessionID: "s-1", ProviderSessionID: "p-1", Incarnation: "inc-1",
			HostID: "0123456789ab", Workspace: "/work/a", Provider: protocol.Provider{Name: "grok", Label: "Grok"}},
		Title: "fix it", Activity: protocol.ActivityIdle, PendingAsks: 1, Since: sinceAt,
	}
	b, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	m["aMemberFromANewerHost"] = map[string]any{"deep": true}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	rr := protocol.RosterRow{HostID: "0123456789ab", SessionID: "s-1", Status: protocol.RosterReachable,
		Host: protocol.RosterHost{PID: 7, CrazeVersion: "0.9.0", Protocol: 1, Provider: "cursor", Workspace: "/work/b",
			StartedAt: startedAt, Ready: true},
		Row: raw}
	got, err := roster.FromRosterRow(rr)
	if err != nil {
		t.Fatal(err)
	}
	wantHost := roster.Host{ID: "0123456789ab", PID: 7, StartedAt: startedAt, Protocol: 1, CrazeSessionID: "s-1",
		ProviderSessionID: "p-1", Incarnation: "inc-1", Provider: "cursor", Workspace: "/work/b", Ready: true}
	if got.Host != wantHost || got.Status != roster.Reachable || got.Version != "0.9.0" || string(got.Raw) != string(raw) {
		t.Fatalf("FromRosterRow = %+v", got)
	}
	if s := got.Session; s == nil || s.ID != "s-1" || s.Provider != "grok" || s.Workspace != "/work/a" || s.Title != "fix it" ||
		s.PendingAsks != 1 || !s.Since.Equal(sinceAt) {
		t.Fatalf("its session = %+v", got.Session)
	}
	if got.State() != roster.StateNeedsYou || !got.Since().Equal(sinceAt) {
		t.Fatalf("its state %v since %v", got.State(), got.Since())
	}

	rr.Status, rr.Row = "a-status-from-later", nil
	got, err = roster.FromRosterRow(rr)
	if err != nil || got.Session != nil || got.Status != roster.Connecting || got.State() != roster.StateStarting {
		t.Fatalf("no row and an unknown status: %+v, %v", got, err)
	}
	rr.Row = json.RawMessage(`{"activity":5}`)
	if _, err := roster.FromRosterRow(rr); err == nil {
		t.Fatal("a row that does not decode was read")
	}
}

// TestARowsModel (plan 035 C11, SF-114): a row's model is the session's, as
// the host wrote it — exact, and whole however long — and a rowFacts host's
// row from before 035, which carries the row facts but no model, reads as
// unknown: "", as an S2 host's row does.
func TestARowsModel(t *testing.T) {
	const facts = `"capabilities":{"rowFacts":true},"prompted":true`
	long := strings.Repeat("openrouter/vendor/a-model-id-", 40)
	for _, c := range []struct {
		name, row, want string
		rowFacts        bool
	}{
		{"a 035 host's row", `{"sessionId":"s-1","activity":"idle",` + facts + `,"model":"grok-4.7-build-fast"}`, "grok-4.7-build-fast", true},
		{"a long id, whole", `{"sessionId":"s-1","activity":"idle",` + facts + `,"model":"` + long + `"}`, long, true},
		{"an older rowFacts host's row", `{"sessionId":"s-1","activity":"idle",` + facts + `}`, "", true},
		{"an S2 host's row", `{"sessionId":"s-1","activity":"idle"}`, "", false},
	} {
		got, err := roster.FromRosterRow(protocol.RosterRow{HostID: "0123456789ab", SessionID: "s-1",
			Status: protocol.RosterReachable, Row: json.RawMessage(c.row)})
		switch {
		case err != nil || got.Session == nil:
			t.Fatalf("%s: FromRosterRow = %+v, %v", c.name, got, err)
		case got.Session.Model != c.want:
			t.Errorf("%s: model %q, want %q", c.name, got.Session.Model, c.want)
		case got.Session.RowFacts != c.rowFacts || got.Session.Prompted != c.rowFacts:
			t.Errorf("%s: rowFacts %v, prompted %v; want %v: the row is not the host's it names", c.name,
				got.Session.RowFacts, got.Session.Prompted, c.rowFacts)
		}
	}
}
