package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/charliek/craze/internal/fakehost"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/rundir"
)

// craze ps (plan 032 §3.12): its table pinned byte for byte, its title cut
// to a terminal, nothing running, the hub's roster through a hub child, the
// hosts read directly with --no-hub or when no hub can be had, and exit 1
// only when the path it took last fails.

// psNow is the format tests' clock.
var psNow = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

// psRosterRow is a hub's roster row for host n: its registry facts, its
// status, and — when row is not nil — the host's own row.
func psRosterRow(t *testing.T, n int, sessionID string, status protocol.RosterStatus, provider, workspace string, row *protocol.SessionRow) protocol.RosterRow {
	t.Helper()
	rr := protocol.RosterRow{HostID: strings.Repeat("0", 11) + string(rune('0'+n)), SessionID: sessionID, Status: status,
		Host: protocol.RosterHost{PID: 100 + n, CrazeVersion: "0.9.0", Protocol: 1, Provider: provider,
			Workspace: workspace, StartedAt: psNow.Add(-3 * time.Hour), Ready: true}}
	if row != nil {
		row.SessionID = sessionID
		b, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		rr.Row = b
	}
	return rr
}

// psFixture is a roster with a row in every state, given out of order.
func psFixture(t *testing.T, home string) protocol.HubSessionsListResult {
	t.Helper()
	long := strings.Repeat("a very long title that goes on ", 5)
	idle := protocol.ActivityIdle
	return protocol.HubSessionsListResult{Epoch: "0a1b2c3d4e5f", Cursor: 9, Sessions: []protocol.RosterRow{
		psRosterRow(t, 6, "0192f0aa-6666-7000-8000-00000000f006", protocol.RosterUnreachable, "cursor", "/srv/f",
			&protocol.SessionRow{Activity: idle, Title: "gone quiet", Since: psNow.Add(-90 * time.Minute)}),
		psRosterRow(t, 5, "0192f0aa-5555-7000-8000-00000000e005", protocol.RosterReachable, "cursor", home+"/older",
			&protocol.SessionRow{Activity: idle}),
		psRosterRow(t, 4, "0192f0aa-4444-7000-8000-00000000d004", protocol.RosterReachable, "grok", "/srv/d",
			&protocol.SessionRow{Activity: idle, Title: "a\x1b[31m red\nline", Since: psNow.Add(-6 * 24 * time.Hour),
				LastTurn: &protocol.LastTurn{Outcome: protocol.TurnFailed, Err: "boom"}}),
		psRosterRow(t, 3, "0192f0aa-3333-7000-8000-00000000c003", protocol.RosterConnecting, "cursor", home, nil),
		psRosterRow(t, 2, "0192f0aa-2222-7000-8000-00000000b002", protocol.RosterReachable, "cursor", "/srv/b",
			&protocol.SessionRow{SessionInfo: protocol.SessionInfo{Provider: protocol.Provider{Name: "grok"}, Workspace: "/srv/b2"},
				Activity: idle, ForeignTurn: true, Title: long, Since: psNow.Add(-42 * time.Second)}),
		psRosterRow(t, 1, "0192f0aa-1111-7000-8000-00000000a001", protocol.RosterReachable, "cursor", home+"/proj-a",
			&protocol.SessionRow{Activity: protocol.ActivityWorking, PendingAsks: 1, Title: "fix the flaky test",
				Since: psNow.Add(-2 * time.Minute)}),
		psRosterRow(t, 7, "0192f0aa-7777-7000-8000-00000000e007", protocol.RosterReachable, "native", home+"/newer",
			&protocol.SessionRow{Activity: idle, Since: psNow.Add(-5 * time.Minute)}),
	}}
}

// TestPsTable pins craze ps's table: a row a session, by state (needs you
// first), newest in its state first; the short id the craze id's last eight
// characters; the host's answer's provider and directory over its registry
// entry's, ~ for HOME; the title the session's own, else the index's, else
// "-", one line and cut to 100 cells with an ellipsis; SINCE in one unit, "-"
// with no time; MODEL "-" (no row carries it, SF-114). The negative control
// is any change to the format.
func TestPsTable(t *testing.T) {
	home := "/home/u"
	titles := map[string]string{"0192f0aa-3333-7000-8000-00000000c003": "the index's title"}
	got := psTable(psRows(psFixture(t, home), home, titles, psNow), 0)
	want := "" +
		"SESSION   STATE        PROVIDER  MODEL  DIR       SINCE  TITLE\n" +
		"0000a001  needs you    cursor    -      ~/proj-a  2m     fix the flaky test\n" +
		"0000b002  working      grok      -      /srv/b2   42s    " + strings.Repeat("a very long title that goes on ", 3) + "a very…\n" +
		"0000c003  starting     cursor    -      ~         3h     the index's title\n" +
		"0000d004  failed       grok      -      /srv/d    6d     a red line\n" +
		"0000e007  idle         native    -      ~/newer   5m     -\n" +
		"0000e005  idle         cursor    -      ~/older   -      -\n" +
		"0000f006  unreachable  cursor    -      /srv/f    1h     gone quiet\n"
	if got != want {
		t.Fatalf("craze ps's table:\n%s\nwant:\n%s", got, want)
	}
	cut := strings.Split(got, "\n")[2]
	if w := ansi.StringWidth(cut[strings.Index(cut, "a very"):]); w != psTitleCells {
		t.Fatalf("the cut title is %d cells, want %d", w, psTitleCells)
	}
}

// TestPsTitleFitsTheTerminal: on a terminal, the title takes what is left of
// its width — every line fits — and at least psTitleMin cells however narrow
// the terminal.
func TestPsTitleFitsTheTerminal(t *testing.T) {
	home := "/home/u"
	rows := psRows(psFixture(t, home), home, nil, psNow)
	// The columns before the title take 57 cells here.
	for _, width := range []int{80, 67} {
		lines := strings.Split(strings.TrimSuffix(psTable(rows, width), "\n"), "\n")
		for _, line := range lines {
			if w := ansi.StringWidth(line); w > width {
				t.Errorf("at %d cells a line is %d: %q", width, w, line)
			}
		}
		if w := ansi.StringWidth(lines[2]); w != width || !strings.HasSuffix(lines[2], "…") {
			t.Errorf("at %d cells the long title's line is %d cells: %q", width, w, lines[2])
		}
	}
	narrow := strings.Split(psTable(rows, 20), "\n")[2]
	if !strings.HasSuffix(narrow, "…") || ansi.StringWidth(narrow[strings.Index(narrow, "a very"):]) != psTitleMin {
		t.Fatalf("at 20 cells the long title reads %q, want %d cells", narrow, psTitleMin)
	}
}

// TestPsNothingRunning: no row prints the one line.
func TestPsNothingRunning(t *testing.T) {
	if got := psTable(psRows(protocol.HubSessionsListResult{}, "/home/u", nil, psNow), 0); got != "no sessions running\n" {
		t.Fatalf("an empty roster prints %q", got)
	}
}

// ------------------------------------------------------------- the paths

// psHost serves an in-process fake host, session id, listed in the process
// environment's registry, in workspace; it is closed when the test ends.
func psHost(t *testing.T, env rundir.Env, hostID, sessionID, workspace string) {
	t.Helper()
	h, err := fakehost.New(fakehost.Options{HostID: hostID, CrazeSessionID: sessionID, Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	reg, err := h.Register(env)
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- h.Serve(reg.Listener()) }()
	var once sync.Once
	t.Cleanup(func() {
		once.Do(func() {
			ctx, cancel := context.WithTimeout(context.Background(), serveStep)
			defer cancel()
			_ = h.Close(ctx)
			<-served
			_ = reg.Close()
		})
	})
}

// psTwo is two fake hosts in env, their workspaces under HOME, and the rows
// craze ps prints for them: SESSION, PROVIDER and DIR, keyed by short id.
func psTwo(t *testing.T, env rundir.Env) map[string][2]string {
	t.Helper()
	ids := []string{"0192f0aa-1111-7000-8000-0000000a0001", "0192f0aa-2222-7000-8000-0000000b0002"}
	want := map[string][2]string{}
	for i, id := range ids {
		ws := filepath.Join(env.Home, "ws"+string(rune('1'+i)))
		psHost(t, env, "00000000000"+string(rune('1'+i)), id, ws)
		// The fake host's session names no provider.
		want[psShort(id)] = [2]string{"-", "~/ws" + string(rune('1'+i))}
	}
	return want
}

// psColumns splits craze ps's output into its rows' cells, by the header's
// column starts; the header itself is checked.
func psColumns(t *testing.T, out string) [][]string {
	t.Helper()
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) == 0 || !strings.HasPrefix(lines[0], "SESSION") {
		t.Fatalf("craze ps printed no header: %q", out)
	}
	var starts []int
	for _, m := range regexp.MustCompile(`[A-Z]+`).FindAllStringIndex(lines[0], -1) {
		starts = append(starts, m[0])
	}
	if len(starts) != 7 {
		t.Fatalf("the header %q has %d columns", lines[0], len(starts))
	}
	var rows [][]string
	for _, l := range lines[1:] {
		var cells []string
		for i, s := range starts {
			end := len(l)
			if i+1 < len(starts) {
				end = starts[i+1]
			}
			cells = append(cells, strings.TrimSpace(l[s:end]))
		}
		rows = append(rows, cells)
	}
	return rows
}

// checkPsRows checks craze ps's table lists exactly want's sessions, each
// with its provider and directory, MODEL and TITLE "-" — and each idle, or,
// should the scheduler starve the one poll each host gets, unreachable.
func checkPsRows(t *testing.T, out string, want map[string][2]string) {
	t.Helper()
	rows := psColumns(t, out)
	var got []string
	for _, r := range rows {
		got = append(got, r[0])
		w, ok := want[r[0]]
		if !ok || r[2] != w[0] || r[4] != w[1] || r[3] != "-" || r[6] != "-" {
			t.Errorf("craze ps's row %q, want %s %v", r, r[0], w)
		}
		switch r[1] {
		case "idle":
		case "unreachable":
			t.Logf("session %s read unreachable: its one poll did not come back within its budget", r[0])
		default:
			t.Errorf("session %s is %q, want idle", r[0], r[1])
		}
	}
	var ids []string
	for id := range want {
		ids = append(ids, id)
	}
	slices.Sort(got)
	slices.Sort(ids)
	if !slices.Equal(got, ids) {
		t.Fatalf("craze ps lists %v, want %v:\n%s", got, ids, out)
	}
}

// TestPsReadsTheHostsWithNoHub: with no hub to be had — this test binary
// spawns none (hub.ErrNoHub) — craze ps says so on stderr and reads each
// host itself, exit 0; --no-hub does the same without asking for a hub, and
// says that; --json prints the same roster with no hub's epoch. The negative
// control: the hub path's test sees no such line.
func TestPsReadsTheHostsWithNoHub(t *testing.T) {
	env, _ := serveHome(t)
	want := psTwo(t, env)

	stdout, stderr, code := executeErr([]string{"ps"})
	if code != 0 {
		t.Fatalf("craze ps exited %d: %s%s", code, stdout, stderr)
	}
	if !regexp.MustCompile(`^craze ps: the hub is not available \(hub: no hub in a test binary[^\n]*\); reading each session's host directly\n$`).MatchString(stderr) {
		t.Fatalf("craze ps's stderr: %q", stderr)
	}
	checkPsRows(t, stdout, want)

	stdout, stderr, code = executeErr([]string{"ps", "--no-hub"})
	if code != 0 || stderr != "craze ps: --no-hub: reading each session's host directly\n" {
		t.Fatalf("craze ps --no-hub exited %d, stderr %q", code, stderr)
	}
	checkPsRows(t, stdout, want)

	stdout, _, code = executeErr([]string{"ps", "--no-hub", "--json"})
	var res protocol.HubSessionsListResult
	if code != 0 || json.Unmarshal([]byte(stdout), &res) != nil || res.Epoch != "" || len(res.Sessions) != 2 ||
		!strings.HasSuffix(stdout, "}\n") || strings.Count(stdout, "\n") != 1 {
		t.Fatalf("craze ps --no-hub --json exited %d: %q", code, stdout)
	}
	for _, r := range res.Sessions {
		if _, ok := want[psShort(r.SessionID)]; !ok || r.Host.Workspace == "" {
			t.Fatalf("craze ps --json's row %+v", r)
		}
	}
}

// TestPsExitsOneWhenNeitherPathCanRead: no hub and a registry that cannot be
// read (the cache tree's craze directory a file) is exit 1, the read's error
// on one line after the hub's; with nothing running and a registry to read,
// the same command is exit 0, `no sessions running` (the negative control).
func TestPsExitsOneWhenNeitherPathCanRead(t *testing.T) {
	env, _ := serveHome(t)
	stdout, stderr, code := executeErr([]string{"ps"})
	if code != 0 || stdout != "no sessions running\n" {
		t.Fatalf("craze ps with nothing running exited %d: %q %q", code, stdout, stderr)
	}
	if err := os.MkdirAll(filepath.Join(env.Home, ".cache"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(env.Home, ".cache", "craze"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, argv := range [][]string{{"ps"}, {"ps", "--no-hub"}, {"ps", "--json"}} {
		stdout, stderr, code = executeErr(argv)
		lines := strings.Split(strings.TrimSuffix(stderr, "\n"), "\n")
		if code != 1 || stdout != "" || len(lines) != 2 || !strings.HasPrefix(lines[1], "craze ps: ") {
			t.Fatalf("%v with no registry exited %d: stdout %q, stderr %q", argv, code, stdout, stderr)
		}
	}
}

// TestPsThroughTheHub: craze ps starts the hub (a child of this test, the
// seam hubAsChild installs) and lists its roster — no word on stderr — and
// --json prints the hub's result: its epoch the hub's id, its rows the two
// hosts'. The negative control is TestPsReadsTheHostsWithNoHub's stderr.
func TestPsThroughTheHub(t *testing.T) {
	env, _ := serveHome(t)
	want := psTwo(t, env)
	hubAsChild(t)

	stdout, stderr, code := executeErr([]string{"ps"})
	if code != 0 || stderr != "" {
		t.Fatalf("craze ps exited %d, stderr %q (log: %s)", code, stderr, hubLog(env)())
	}
	checkPsRows(t, stdout, want)

	rec, _, err := rundir.ReadHubRecord(env)
	if err != nil {
		t.Fatal(err)
	}
	stdout, stderr, code = executeErr([]string{"ps", "--json"})
	var res protocol.HubSessionsListResult
	if code != 0 || stderr != "" || json.Unmarshal([]byte(stdout), &res) != nil || res.Epoch != rec.HubID {
		t.Fatalf("craze ps --json exited %d (stderr %q): %q; the hub is %s", code, stderr, stdout, rec.HubID)
	}
	var got []string
	for _, r := range res.Sessions {
		got = append(got, psShort(r.SessionID))
	}
	if len(got) != 2 || want[got[0]] == [2]string{} || want[got[1]] == [2]string{} {
		t.Fatalf("the hub's roster lists %v, want %v", got, want)
	}
}
