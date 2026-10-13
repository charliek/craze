package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/charliek/craze/internal/fakehost"
	"github.com/charliek/craze/internal/hub"
	"github.com/charliek/craze/internal/paths"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/rundir"
	"github.com/charliek/craze/internal/sessions"
)

// The saved sessions (plan 046 B2, B6; B-3, B-5): savedSessions — the hub's
// sessions.saved answer and craze ps --saved's index fallback, one function
// — over a real index and registry, the hub serving it, and craze ps --saved
// through a running hub, with none, with --no-hub and against a hub that
// cannot answer it.

// savedSeen is when every seeded row was last active: an hour before now,
// so craze ps --saved's AGE says 1h.
var savedSeen = time.Now().Add(-time.Hour - time.Minute).UTC().Truncate(time.Second)

// writeSavedIndex writes rows as this test's session index (CRAZE_HOME's
// sessions.jsonl), each as the index writes a row.
func writeSavedIndex(t *testing.T, rows ...sessions.Row) {
	t.Helper()
	var b strings.Builder
	for _, r := range rows {
		line, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	writeFileT(t, paths.SessionsPath(), b.String())
}

// savedRow is an index row of craze id id (none for ""), in ws, updated ago
// before savedSeen.
func savedRow(id, provider, sid, ws, title string, ago time.Duration) sessions.Row {
	return sessions.Row{SessionID: sid, Provider: provider, CWD: ws, Title: title, CrazeID: id,
		CreatedAt: savedSeen.Add(-24 * time.Hour), UpdatedAt: savedSeen.Add(-ago)}
}

// savedIDs is each listed session's id, in order.
func savedIDs(res protocol.SavedResult) []string {
	var out []string
	for _, s := range res.Sessions {
		out = append(out, s.ID)
	}
	return out
}

// noLog is a logf that fails the test: nothing is expected to be said.
func noLog(t *testing.T) func(string, ...any) {
	return func(format string, args ...any) { t.Errorf("logged: "+format, args...) }
}

// TestSavedSessionsOverTheRegistry (B-3): savedSessions lists the index's
// saved sessions less those a live host serves — a stopping host's session
// listed at once, the same host unmarked keeping it out (the negative
// control) — sub-agents' only when asked for, marked; one workspace's only
// when one is named; newest first.
func TestSavedSessionsOverTheRegistry(t *testing.T) {
	env, _ := serveHome(t)
	wsA, wsB := filepath.Join(env.Home, "a"), filepath.Join(env.Home, "b")
	const run, stop = "0192f0aa-1111-7000-8000-00000000run1", "0192f0aa-2222-7000-8000-0000000stop2"
	sub := savedRow("0192f0aa-3333-7000-8000-000000000sub", "cursor", "p-sub", wsA, "a sub-agent's", 3*time.Minute)
	sub.Unattended = &sessions.Unattended{PermissionMode: sessions.PermissionBypass}
	writeSavedIndex(t,
		savedRow(run, "cursor", "p-run", wsA, "running", time.Minute),
		savedRow(stop, "cursor", "p-stop", wsA, "stopping", 2*time.Minute),
		sub,
		savedRow("", "grok", "p-legacy", wsB, "a legacy row", 4*time.Minute),
		savedRow("0192f0aa-4444-7000-8000-0000000old4", "native", "p-old", wsB, "", 5*time.Minute),
	)
	serveFakeHost(t, env, fakehost.Options{HostID: "000000000001", CrazeSessionID: run, Workspace: wsA})
	stopping := serveFakeHost(t, env, fakehost.Options{HostID: "000000000002", CrazeSessionID: stop, Workspace: wsA})

	list := func(q protocol.SavedParams) protocol.SavedResult {
		t.Helper()
		res, perr := savedSessions(env, q, noLog(t))
		if perr != nil {
			t.Fatalf("savedSessions(%+v) refused: %v", q, perr)
		}
		return res
	}
	if got, want := savedIDs(list(protocol.SavedParams{})), []string{"grok:p-legacy", "0192f0aa-4444-7000-8000-0000000old4"}; !slices.Equal(got, want) {
		t.Fatalf("both hosts serving: %v, want %v", got, want)
	}
	if err := stopping.MarkStopping(); err != nil {
		t.Fatal(err)
	}
	res := list(protocol.SavedParams{})
	if got, want := savedIDs(res), []string{stop, "grok:p-legacy", "0192f0aa-4444-7000-8000-0000000old4"}; !slices.Equal(got, want) || res.More {
		t.Fatalf("one host stopping: %v (more %v), want %v", got, res.More, want)
	}
	if s := res.Sessions[0]; s.CrazeSessionID != stop || s.Provider != "cursor" || s.ProviderSessionID != "p-stop" || s.Workspace != wsA ||
		s.Title != "stopping" || !s.UpdatedAt.Equal(savedSeen.Add(-2*time.Minute)) || s.SubAgent {
		t.Fatalf("the stopping host's session %+v", s)
	}
	if s := res.Sessions[1]; s.CrazeSessionID != "" || s.ProviderSessionID != "p-legacy" || s.Provider != "grok" {
		t.Fatalf("the legacy row %+v", s)
	}
	res = list(protocol.SavedParams{Workspace: wsA, SubAgents: true})
	if got := savedIDs(res); !slices.Equal(got, []string{stop, sub.CrazeID}) || !res.Sessions[1].SubAgent || res.Sessions[0].SubAgent {
		t.Fatalf("workspace a, sub-agents too: %+v", res.Sessions)
	}
	res = list(protocol.SavedParams{Workspace: wsA, Limit: 1})
	if got := savedIDs(res); !slices.Equal(got, []string{stop}) || res.More {
		t.Fatalf("workspace a, limit 1: %v (more %v), want the stopping one alone, no more", got, res.More)
	}
	res = list(protocol.SavedParams{Limit: 2})
	if got := savedIDs(res); len(got) != 2 || !res.More {
		t.Fatalf("limit 2: %v (more %v), want two and more", got, res.More)
	}
}

// TestSavedResultOfClipsAndBounds (B-3): the wire's rows — a title cut to
// 120 characters (runes, not bytes), times in UTC, a legacy row's id and no
// craze id — and the whole result held to 1 MiB, trailing rows dropped and
// more set, no further than it must; rows within it are all kept, more as
// it came (the negative control).
func TestSavedResultOfClipsAndBounds(t *testing.T) {
	east := time.FixedZone("east", 5*3600)
	title := strings.Repeat("é", protocol.SavedTitleMax+30)
	got := savedResultOf([]sessions.Row{
		{SessionID: "p-1", Provider: "cursor", CWD: "/w", Title: title, CrazeID: "c-1",
			CreatedAt: time.Date(2026, 10, 12, 9, 0, 0, 0, east), UpdatedAt: time.Date(2026, 10, 12, 10, 0, 0, 0, east)},
		{SessionID: "p-2", Provider: "grok", CWD: "/w", Title: "exactly", UpdatedAt: time.Date(2026, 10, 12, 8, 0, 0, 0, time.UTC)},
	}, false)
	if s := got.Sessions[0]; utf8.RuneCountInString(s.Title) != protocol.SavedTitleMax || !strings.HasPrefix(title, s.Title) ||
		s.ID != "c-1" || s.CreatedAt.Location() != time.UTC || s.UpdatedAt != time.Date(2026, 10, 12, 5, 0, 0, 0, time.UTC) {
		t.Fatalf("the first row %+v", s)
	}
	if s := got.Sessions[1]; s.ID != "grok:p-2" || s.CrazeSessionID != "" || s.Title != "exactly" || got.More {
		t.Fatalf("the legacy row %+v (more %v)", s, got.More)
	}
	enc, err := protocol.MarshalLine(got)
	if err != nil || !strings.Contains(string(enc), `"createdAt":"2026-10-12T04:00:00Z"`) {
		t.Fatalf("written %s (%v), want its times in UTC", enc, err)
	}

	// A workspace of control characters costs six bytes each on the wire.
	ws := "/" + strings.Repeat("\x01", protocol.SavedWorkspaceMax-1)
	var rows []sessions.Row
	for i := range protocol.SavedLimitMax {
		rows = append(rows, sessions.Row{SessionID: fmt.Sprintf("p-%03d", i), Provider: "cursor", CWD: ws, CrazeID: fmt.Sprintf("c-%03d", i)})
	}
	capped := savedResultOf(rows, false)
	enc, err = protocol.MarshalLine(capped)
	if err != nil {
		t.Fatal(err)
	}
	n := len(capped.Sessions)
	if size := len(enc) - 1; size > protocol.SavedResultBytesMax || !capped.More || n == 0 || n == len(rows) {
		t.Fatalf("the capped result: %d bytes, %d of %d rows, more %v", size, n, len(rows), capped.More)
	}
	if capped.Sessions[n-1].ID != rows[n-1].CrazeID {
		t.Fatalf("the capped result's last row %s, want the rows in order", capped.Sessions[n-1].ID)
	}
	plusOne := capped
	plusOne.Sessions = append(slices.Clone(capped.Sessions), savedResultOf(rows[n:n+1], false).Sessions...)
	if b, _ := protocol.MarshalLine(plusOne); len(b)-1 <= protocol.SavedResultBytesMax {
		t.Fatalf("one more row is %d bytes, within the bound: the cap dropped a row it had room for", len(b)-1)
	}
	if kept := savedResultOf(rows[:n], true); len(kept.Sessions) != n || !kept.More {
		t.Fatalf("rows within the bound: %d of %d kept, more %v", len(kept.Sessions), n, kept.More)
	}
	if kept := savedResultOf(rows[:n], false); kept.More {
		t.Fatal("rows within the bound say more")
	}
}

// TestSavedSessionsOfAnUnreadableIndexOrRegistry (B-3): an index that does
// not read is no sessions, [] — as sessions.createOptions answers it — and
// one log line saying why; a registry that does not read is refused
// unavailable, reason host_unreachable, its message naming no path, and one
// log line naming the cause. A readable pair answers (the negative control).
func TestSavedSessionsOfAnUnreadableIndexOrRegistry(t *testing.T) {
	env, _ := serveHome(t)
	ws := filepath.Join(env.Home, "a")
	writeSavedIndex(t, savedRow("c-1", "cursor", "p-1", ws, "one", 0))
	var log []string
	logf := func(format string, args ...any) { log = append(log, fmt.Sprintf(format, args...)) }
	if res, perr := savedSessions(env, protocol.SavedParams{}, logf); perr != nil || len(res.Sessions) != 1 || len(log) != 0 {
		t.Fatalf("a readable index and registry: %+v, %v, logged %q", res, perr, log)
	}

	f, err := os.OpenFile(paths.SessionsPath(), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("{not json\n")
	_ = f.Close()
	res, perr := savedSessions(env, protocol.SavedParams{}, logf)
	if perr != nil || res.Sessions == nil || len(res.Sessions) != 0 || res.More {
		t.Fatalf("an unreadable index: %#v, %v; want no sessions, []", res, perr)
	}
	if len(log) != 1 || !strings.Contains(log[0], "the session index cannot be read") || !strings.Contains(log[0], "sessions.jsonl:2") {
		t.Fatalf("logged %q, want one line saying the index (line 2) cannot be read", log)
	}

	log = nil
	writeSavedIndex(t, savedRow("c-1", "cursor", "p-1", ws, "one", 0))
	hosts := filepath.Join(env.Home, ".cache", "craze", "hosts")
	if err := os.MkdirAll(hosts, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(hosts, 0o777); err != nil {
		t.Fatal(err)
	}
	_, perr = savedSessions(env, protocol.SavedParams{}, logf)
	if perr == nil || perr.Code != protocol.RPCRefused || perr.Data.Code != protocol.CodeUnavailable ||
		perr.Data.Reason != protocol.ReasonHostUnreachable || strings.Contains(perr.Message, env.Home) {
		t.Fatalf("an unreadable registry: %+v, want unavailable/host_unreachable naming no path", perr)
	}
	if len(log) != 1 || !strings.Contains(log[0], "the registry cannot be read") || !strings.Contains(log[0], "0700 is required") {
		t.Fatalf("logged %q, want one line naming the registry's trouble", log)
	}
}

// TestTheHubServesSavedSessions (B-3): the real hub given this build's
// answer (hubCreates) serves savedSessions over the wire — an oversized
// index row skipped, the rest listed; an unreadable index answered [];
// an unreadable registry refused unavailable, reason host_unreachable — and
// its hello still says no capability for it.
func TestTheHubServesSavedSessions(t *testing.T) {
	env, _ := serveHome(t)
	sock := inProcessHub(t, env, hubCreates())
	ws := filepath.Join(env.Home, "a")
	writeSavedIndex(t,
		savedRow("c-1", "cursor", "p-1", ws, "one", time.Minute),
		savedRow("c-big", "cursor", strings.Repeat("s", protocol.SavedProviderSessionIDMax+1), ws, "oversized", 2*time.Minute),
		savedRow("", "grok", "p-2", ws, "two", 3*time.Minute),
	)
	// reopen (plan 046 B1): the hub serves sessions.saved and session.reopen
	// both.
	want := protocol.HubCapabilities()
	want.CreateOptions, want.CreateTitle, want.Reopen = true, true, true
	if hello := hubHello(t, sock); hello.Capabilities != want {
		t.Fatalf("the hub's hello %+v, want %+v", hello.Capabilities, want)
	}
	saved := func(q protocol.SavedParams) (protocol.SavedResult, error) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), serveStep)
		defer cancel()
		raw, err := hub.Saved(ctx, sock, psClient, q)
		var res protocol.SavedResult
		if err == nil {
			err = json.Unmarshal(raw, &res)
		}
		return res, err
	}
	res, err := saved(protocol.SavedParams{})
	if err != nil || !slices.Equal(savedIDs(res), []string{"c-1", "grok:p-2"}) || res.More {
		t.Fatalf("sessions.saved = %+v, %v; want c-1 and the legacy row, the oversized row skipped", res, err)
	}
	res, err = saved(protocol.SavedParams{Workspace: ws + "/", Limit: 1})
	if err != nil || !slices.Equal(savedIDs(res), []string{"c-1"}) || !res.More {
		t.Fatalf("one workspace, limit 1 = %+v, %v", res, err)
	}

	writeFileT(t, paths.SessionsPath(), "{not json\n")
	if res, err = saved(protocol.SavedParams{}); err != nil || res.Sessions == nil || len(res.Sessions) != 0 {
		t.Fatalf("an unreadable index = %#v, %v; want []", res, err)
	}

	writeSavedIndex(t, savedRow("c-1", "cursor", "p-1", ws, "one", time.Minute))
	hosts := filepath.Join(env.Home, ".cache", "craze", "hosts")
	if err := os.MkdirAll(hosts, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(hosts, 0o777); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(hosts, 0o700) })
	_, err = saved(protocol.SavedParams{})
	var pe *protocol.Error
	if !errors.As(err, &pe) || pe.Data.Code != protocol.CodeUnavailable || pe.Data.Reason != protocol.ReasonHostUnreachable {
		t.Fatalf("an unreadable registry = %v, want unavailable/host_unreachable", err)
	}
}

// savedTable is craze ps --saved's table split into cells by the header's
// column starts, the header checked.
func savedTable(t *testing.T, out string) [][]string {
	t.Helper()
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) == 0 || lines[0] != strings.TrimRight(lines[0], " ") {
		t.Fatalf("craze ps --saved printed %q", out)
	}
	var starts []int
	for _, m := range regexp.MustCompile(`[A-Z]+`).FindAllStringIndex(lines[0], -1) {
		starts = append(starts, m[0])
	}
	var names []string
	for _, s := range starts {
		names = append(names, strings.Fields(lines[0][s:])[0])
	}
	if !slices.Equal(names, []string{"SESSION", "PROVIDER", "DIR", "AGE", "TITLE"}) {
		t.Fatalf("the header %q", lines[0])
	}
	var rows [][]string
	for _, l := range lines[1:] {
		var cells []string
		for i, s := range starts {
			end := len(l)
			if i+1 < len(starts) {
				end = min(starts[i+1], len(l))
			}
			cells = append(cells, strings.TrimSpace(l[min(s, len(l)):end]))
		}
		rows = append(rows, cells)
	}
	return rows
}

// psSavedMachine is a HOME whose index holds three saved sessions — a craze
// id's and a legacy row's in ~/a, a sub-agent's — and one running session's,
// whose host serves it; the table craze ps --saved prints for it, every
// workspace, and for ~/a alone.
func psSavedMachine(t *testing.T) (env rundir.Env, all, onlyA [][]string) {
	t.Helper()
	env, _ = serveHome(t)
	wsA, wsB := filepath.Join(env.Home, "a"), filepath.Join(env.Home, "b")
	const run = "0192f0aa-1111-7000-8000-00000000run1"
	sub := savedRow("0192f0aa-3333-7000-8000-000000000sub", "cursor", "p-sub", wsA, "a sub-agent's", 0)
	sub.Unattended = &sessions.Unattended{PermissionMode: sessions.PermissionBypass}
	writeSavedIndex(t,
		savedRow(run, "cursor", "p-run", wsA, "running", 0),
		sub,
		savedRow("0192f0aa-2222-7000-8000-0000000aaaa2", "cursor", "p-a", wsA, "fix \x1b[31mthe\ttest", time.Second),
		savedRow("", "grok", "grok-session-legacy01", wsA, "", 2*time.Second),
		savedRow("0192f0aa-4444-7000-8000-0000000bbbb4", "native", "p-b", wsB, "in b", 3*time.Second),
	)
	serveFakeHost(t, env, fakehost.Options{HostID: "000000000001", CrazeSessionID: run, Workspace: wsA})
	all = [][]string{
		{"000aaaa2", "cursor", "~/a", "1h", "fix the test"},
		{"legacy01", "grok", "~/a", "1h", "-"},
		{"000bbbb4", "native", "~/b", "1h", "in b"},
	}
	return env, all, all[:2]
}

// TestPsSavedFallsBackToTheIndex (B-5): with no hub running — this test
// binary starts none, and craze ps --saved would not — and with --no-hub,
// craze ps --saved reads the index itself and says so on stderr: the saved
// sessions newest first, the running one and the sub-agent's left out, a
// legacy row by its provider session id's end, every cell one line; -C keeps
// one directory; --json prints sessions.saved's result. Neither path starts
// a hub.
func TestPsSavedFallsBackToTheIndex(t *testing.T) {
	env, all, onlyA := psSavedMachine(t)
	for _, tc := range []struct {
		argv   []string
		stderr string
		want   [][]string
	}{
		{[]string{"ps", "--saved"}, "craze ps: no hub is running; reading the session index directly\n", all},
		{[]string{"ps", "--saved", "--no-hub"}, "craze ps: --no-hub: reading the session index directly\n", all},
		{[]string{"ps", "--saved", "-C", filepath.Join(env.Home, "a", "..", "a")}, "craze ps: no hub is running; reading the session index directly\n", onlyA},
	} {
		stdout, stderr, code := executeErr(tc.argv)
		if code != 0 || stderr != tc.stderr {
			t.Fatalf("%v exited %d, stderr %q", tc.argv, code, stderr)
		}
		if got := savedTable(t, stdout); fmt.Sprint(got) != fmt.Sprint(tc.want) {
			t.Fatalf("%v printed\n%s\nwant rows %q", tc.argv, stdout, tc.want)
		}
	}
	stdout, _, code := executeErr([]string{"ps", "--saved", "--no-hub", "--json"})
	var res protocol.SavedResult
	if code != 0 || json.Unmarshal([]byte(stdout), &res) != nil || len(res.Sessions) != 3 || res.More ||
		strings.Count(stdout, "\n") != 1 || res.Sessions[1].ID != "grok:grok-session-legacy01" {
		t.Fatalf("craze ps --saved --no-hub --json exited %d: %q", code, stdout)
	}
	if _, _, err := rundir.ReadHubRecord(env); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a hub record exists after craze ps --saved: %v", err)
	}
}

// TestPsSavedThroughTheHub (B-5): with a hub running, craze ps --saved lists
// through its sessions.saved — nothing on stderr — the same table the index
// fallback prints, and --json prints the hub's result: byte for byte what
// --no-hub --json prints over the same index.
func TestPsSavedThroughTheHub(t *testing.T) {
	env, all, onlyA := psSavedMachine(t)
	inProcessHub(t, env, hubCreates())
	stdout, stderr, code := executeErr([]string{"ps", "--saved"})
	if code != 0 || stderr != "" {
		t.Fatalf("craze ps --saved exited %d, stderr %q", code, stderr)
	}
	if got := savedTable(t, stdout); fmt.Sprint(got) != fmt.Sprint(all) {
		t.Fatalf("craze ps --saved through the hub printed\n%s\nwant rows %q", stdout, all)
	}
	stdout, stderr, code = executeErr([]string{"ps", "--saved", "--dir", filepath.Join(env.Home, "a")})
	if got := savedTable(t, stdout); code != 0 || stderr != "" || fmt.Sprint(got) != fmt.Sprint(onlyA) {
		t.Fatalf("craze ps --saved --dir through the hub exited %d (%q):\n%s", code, stderr, stdout)
	}
	viaHub, stderr, code := executeErr([]string{"ps", "--saved", "--json"})
	if code != 0 || stderr != "" {
		t.Fatalf("craze ps --saved --json exited %d, stderr %q", code, stderr)
	}
	direct, _, code := executeErr([]string{"ps", "--saved", "--json", "--no-hub"})
	if code != 0 || viaHub != direct {
		t.Fatalf("through the hub %q\nwith --no-hub %q", viaHub, direct)
	}
}

// TestPsSavedOfAnOlderHub (B-5, plan 046 B6's gate): craze ps --saved lists
// through a hub only when its hello says reopen. A running hub without it —
// the real hub given this build's answers less Saved (a hub from before the
// method), or less Reopen alone (a hub that answers sessions.saved but whose
// hello cannot say reopen) — is said on stderr, and craze ps --saved reads the
// index itself, exit 0. The second is the gate's negative control: that hub
// would answer sessions.saved, and is never asked (its log names no call).
func TestPsSavedOfAnOlderHub(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*hub.Creates)
	}{
		{"no sessions.saved", func(c *hub.Creates) { c.Saved = nil }},
		{"no session.reopen", func(c *hub.Creates) { c.Reopen = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, all, _ := psSavedMachine(t)
			creates := hubCreates()
			tc.edit(creates)
			var asked atomic.Bool
			if saved := creates.Saved; saved != nil {
				creates.Saved = func(q protocol.SavedParams, logf func(string, ...any)) (protocol.SavedResult, *protocol.Error) {
					asked.Store(true)
					return saved(q, logf)
				}
			}
			inProcessHub(t, env, creates)
			stdout, stderr, code := executeErr([]string{"ps", "--saved"})
			if code != 0 || stderr != "craze ps: the hub cannot list saved sessions (an older craze's); reading the session index directly\n" {
				t.Fatalf("craze ps --saved of an older hub exited %d, stderr %q", code, stderr)
			}
			if got := savedTable(t, stdout); fmt.Sprint(got) != fmt.Sprint(all) {
				t.Fatalf("craze ps --saved of an older hub printed\n%s\nwant rows %q", stdout, all)
			}
			if asked.Load() {
				t.Fatal("craze ps --saved asked sessions.saved of a hub whose hello does not say reopen")
			}
		})
	}
}

// TestPsSavedEdges (B-5): -C without --saved is a usage error, exit 2;
// nothing saved prints `no saved sessions`; more saved sessions than the 200
// listed says so on stderr; a registry that cannot be read is exit 1, its
// trouble on stderr, never an empty list.
func TestPsSavedEdges(t *testing.T) {
	env, _ := serveHome(t)
	if stdout, stderr, code := executeErr([]string{"ps", "-C", env.Home}); code != 2 || stdout != "" ||
		stderr != "craze ps: -C/--dir needs --saved\n" {
		t.Fatalf("craze ps -C exited %d: %q %q", code, stdout, stderr)
	}
	if stdout, _, code := executeErr([]string{"ps", "--saved", "--no-hub"}); code != 0 || stdout != "no saved sessions\n" {
		t.Fatalf("craze ps --saved with no index exited %d: %q", code, stdout)
	}

	var rows []sessions.Row
	for i := range protocol.SavedLimitMax + 1 {
		rows = append(rows, savedRow(fmt.Sprintf("c-%03d", i), "cursor", fmt.Sprintf("p-%03d", i), env.Home, "", time.Duration(i)*time.Second))
	}
	writeSavedIndex(t, rows...)
	stdout, stderr, code := executeErr([]string{"ps", "--saved", "--no-hub"})
	if code != 0 || strings.Count(stdout, "\n") != protocol.SavedLimitMax+1 ||
		!strings.HasSuffix(stderr, "craze ps: more saved sessions than these 200: only the newest are listed\n") {
		t.Fatalf("201 saved sessions: exit %d, %d lines, stderr %q", code, strings.Count(stdout, "\n"), stderr)
	}

	hosts := filepath.Join(env.Home, ".cache", "craze", "hosts")
	if err := os.MkdirAll(hosts, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(hosts, 0o777); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, code = executeErr([]string{"ps", "--saved"})
	lines := strings.Split(strings.TrimSuffix(stderr, "\n"), "\n")
	if code != 1 || stdout != "" || len(lines) != 3 || !strings.Contains(lines[1], "0700 is required") ||
		lines[2] != "craze ps: the registry cannot be read, so the saved sessions cannot be told from the running ones" {
		t.Fatalf("craze ps --saved with an unreadable registry exited %d: stdout %q, stderr %q", code, stdout, stderr)
	}
}

// TestSavedSessionsReadTheWholeIndex (C6 review; C7 review, item 9): the
// index is read whole, through the real Store, so a craze id's newest row is
// its newest whatever its provider: one whose newest row is of a provider this
// build does not know (a newer craze's) is not offered — never by its older
// cursor row, which a load of the id would not take — and one whose newest row
// is cursor's is offered by it, its title its own; a craze id of only an
// unknown provider's rows is not offered either.
func TestSavedSessionsReadTheWholeIndex(t *testing.T) {
	env, _ := serveHome(t)
	ws := filepath.Join(env.Home, "a")
	writeSavedIndex(t,
		savedRow("c-7", "nosuch", "p-9", ws, "renamed elsewhere", time.Minute),
		savedRow("c-7", "cursor", "p-7b", ws, "older title", 2*time.Minute),
		savedRow("c-9", "cursor", "p-9b", ws, "the newest title", 3*time.Minute),
		savedRow("c-9", "nosuch", "p-9a", ws, "an older craze's title", 4*time.Minute),
		savedRow("c-8", "nosuch", "p-8", ws, "a newer craze's", 5*time.Minute),
	)
	res, perr := savedSessions(env, protocol.SavedParams{}, noLog(t))
	if perr != nil || len(res.Sessions) != 1 {
		t.Fatalf("savedSessions = %+v, %v; want c-9 alone", res, perr)
	}
	if s := res.Sessions[0]; s.ID != "c-9" || s.Provider != "cursor" || s.ProviderSessionID != "p-9b" || s.Title != "the newest title" {
		t.Fatalf("c-9 listed %+v; want its newest row, its own title", s)
	}
}

// TestSavedSessionsRefuseAnUnreadableLiveEntry (C6 review): a live host
// whose registry entry cannot be read — its lock held, its entry garbage —
// serves a session no one can name, so the listing is refused unavailable,
// reason host_unreachable, the log naming the host, rather than offer that
// session as saved. The same host's entry readable keeps its session out
// (the negative control), and once the host is gone it is listed.
func TestSavedSessionsRefuseAnUnreadableLiveEntry(t *testing.T) {
	env, _ := serveHome(t)
	ws := filepath.Join(env.Home, "a")
	const run = "0192f0aa-1111-7000-8000-00000000run1"
	writeSavedIndex(t, savedRow(run, "cursor", "p-run", ws, "running", time.Minute))
	h := serveFakeHost(t, env, fakehost.Options{HostID: "000000000001", CrazeSessionID: run, Workspace: ws})
	if res, perr := savedSessions(env, protocol.SavedParams{}, noLog(t)); perr != nil || len(res.Sessions) != 0 {
		t.Fatalf("its entry readable: %+v, %v; want nothing saved", res, perr)
	}
	entry := filepath.Join(env.Home, ".cache", "craze", "hosts", "000000000001.json")
	if _, err := os.Stat(entry); err != nil {
		t.Fatal(err)
	}
	writeFileT(t, entry, "{not an entry")
	var log []string
	_, perr := savedSessions(env, protocol.SavedParams{}, func(format string, args ...any) { log = append(log, fmt.Sprintf(format, args...)) })
	if perr == nil || perr.Code != protocol.RPCRefused || perr.Data.Code != protocol.CodeUnavailable || perr.Data.Reason != protocol.ReasonHostUnreachable {
		t.Fatalf("an unreadable live entry: %+v, want unavailable/host_unreachable", perr)
	}
	if len(log) != 1 || !strings.Contains(log[0], "000000000001") {
		t.Fatalf("logged %q, want one line naming the host", log)
	}
	if err := h.Unlist(); err != nil {
		t.Fatal(err)
	}
	if res, perr := savedSessions(env, protocol.SavedParams{}, noLog(t)); perr != nil || !slices.Equal(savedIDs(res), []string{run}) {
		t.Fatalf("the host gone: %+v, %v; want its session saved", res, perr)
	}
}

// TestSavedSessionsSkipATimeTheWireCannotWrite (C6 review): through the real
// index, a row whose time parses but cannot be written in UTC
// (9999-12-31T23:59:59-01:00) is skipped before the cut — never a place the
// limit counts and then dropped — so the limit lists the rows it can write
// and more is right.
func TestSavedSessionsSkipATimeTheWireCannotWrite(t *testing.T) {
	env, _ := serveHome(t)
	ws := filepath.Join(env.Home, "a")
	good := func(id string, ago time.Duration) string {
		b, err := json.Marshal(savedRow(id, "cursor", "p-"+id, ws, id, ago))
		if err != nil {
			t.Fatal(err)
		}
		return string(b) + "\n"
	}
	late := fmt.Sprintf(`{"sessionId":"p-late","provider":"cursor","cwd":%q,"title":"late","pinned":false,`+
		`"createdAt":"2026-10-12T09:00:00Z","updatedAt":"9999-12-31T23:59:59-01:00","crazeId":"c-late"}`+"\n", ws)
	writeFileT(t, paths.SessionsPath(), late+good("c-1", time.Minute)+good("c-2", 2*time.Minute)+good("c-3", 3*time.Minute))
	res, perr := savedSessions(env, protocol.SavedParams{Limit: 2}, noLog(t))
	if perr != nil || !slices.Equal(savedIDs(res), []string{"c-1", "c-2"}) || !res.More {
		t.Fatalf("limit 2 = %+v, %v; want c-1, c-2 and more", res, perr)
	}
	res, perr = savedSessions(env, protocol.SavedParams{Limit: 3}, noLog(t))
	if perr != nil || !slices.Equal(savedIDs(res), []string{"c-1", "c-2", "c-3"}) || res.More {
		t.Fatalf("limit 3 = %+v, %v; want c-1, c-2, c-3 and no more", res, perr)
	}
}

// TestPsSavedShortensByCharacters (C6 review): a legacy row's SESSION is its
// provider session id's last eight characters, never a cut inside one; a
// shorter id is shown whole.
func TestPsSavedShortensByCharacters(t *testing.T) {
	res := protocol.SavedResult{Sessions: []protocol.SavedSession{
		{ID: "grok:abcdef界界界", Provider: "grok", ProviderSessionID: "abcdef界界界", Workspace: "/w"},
		{ID: "grok:x界界界界界界界界", Provider: "grok", ProviderSessionID: "x界界界界界界界界", Workspace: "/w"},
		{ID: "grok:界界", Provider: "grok", ProviderSessionID: "界界", Workspace: "/w"},
		{ID: "0192f0aa-2222-7000-8000-0000000aaaa2", CrazeSessionID: "0192f0aa-2222-7000-8000-0000000aaaa2", Provider: "cursor",
			ProviderSessionID: "界界界界界界界界界", Workspace: "/w"},
	}}
	var got []string
	for _, r := range psSavedRows(res, "/home/u", time.Now()) {
		got = append(got, r[0])
	}
	if want := []string{"bcdef界界界", "界界界界界界界界", "界界", "000aaaa2"}; !slices.Equal(got, want) {
		t.Fatalf("SESSION cells %q, want %q", got, want)
	}
}
