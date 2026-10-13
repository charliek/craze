package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/charliek/craze/internal/hub"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/rundir"
	"github.com/charliek/craze/internal/sessions"
	"github.com/charliek/craze/internal/version"
)

// session.reopen's CLI half (plan 046 B3, B6): the hub's resolution of an id
// against the index (reopenTarget), craze new --load's refusals and its
// short ids, an older hub, and a saved session reopened through a real hub
// child whose hosts are real craze serve children loading over the fake
// agent.

// TestReopenTargetResolvesTheIndex (B3 step 2): an id resolves as craze serve
// --load resolves it — a craze id to its newest row, <provider>:<session id>
// to its row — and a legacy key whose row has been given a craze id since
// resolves to that craze id and its newest row. A person's row reopens with
// no settings; a sub-agent's with its recorded ones and --unattended, as
// craze send reloads it. Nothing is stat'ed until the spawn path asks
// (Runnable). An id no row has is unknown_session; one whose deciding row —
// the craze id's newest, or the legacy row — cannot be resumed (a provider
// this build does not know, a malformed row) is not_resumable, never an older
// row in its place (C7 review, item 9); so is an index that cannot be read.
func TestReopenTargetResolvesTheIndex(t *testing.T) {
	env, ws := serveHome(t)
	gone := filepath.Join(env.Home, "gone")
	const (
		plain  = "0199c0de-1111-7000-8000-000000000001"
		minted = "0199c0de-3333-7000-8000-000000000003"
		marked = "0199c0de-4444-7000-8000-000000000004"
	)
	sub := savedRow(marked, "cursor", "p-sub", ws, "", 0)
	sub.Unattended = &sessions.Unattended{Mode: "plan", PermissionMode: sessions.PermissionPrompt, Model: "m-1", Effort: "high"}
	writeSavedIndex(t,
		savedRow(plain, "cursor", "p-1-old", ws, "", 2*time.Minute),
		savedRow(plain, "cursor", "p-1", ws, "", time.Minute),
		savedRow("", "grok", "p-2", gone, "", 0),
		savedRow(minted, "cursor", "p-3", ws, "", 3*time.Minute),
		savedRow(minted, "cursor", "p-3-new", ws, "", time.Second),
		sub,
		savedRow("0199c0de-5555-7000-8000-000000000005", "no-such-provider", "p-5", ws, "", 0),
		// 6's newest row is of a provider this build does not know, 7's is
		// malformed: each has an older row that could run, and neither is
		// taken.
		savedRow("0199c0de-6666-7000-8000-000000000006", "no-such-provider", "p-6b", ws, "", 0),
		savedRow("0199c0de-6666-7000-8000-000000000006", "cursor", "p-6a", ws, "", time.Hour),
		savedRow("0199c0de-7777-7000-8000-000000000007", "cursor", "p-7b", "relative/dir", "", 0),
		savedRow("0199c0de-7777-7000-8000-000000000007", "cursor", "p-7a", ws, "", time.Hour),
		savedRow("", "no-such-provider", "p-8", ws, "", 0),
	)
	resolve := func(id string) (hub.Reopening, *protocol.Error) {
		t.Helper()
		return reopenTarget(id, noLog(t))
	}
	for _, tc := range []struct {
		id         string
		load, sid  string
		crazeID    string
		workspace  string
		settings   hub.Reopening
		runnableOK bool
	}{
		{id: plain, load: plain, sid: "p-1", crazeID: plain, workspace: ws, runnableOK: true},
		{id: "grok:p-2", load: "grok:p-2", sid: "p-2", workspace: gone},
		{id: "cursor:p-3", load: minted, sid: "p-3-new", crazeID: minted, workspace: ws, runnableOK: true},
		{id: marked, load: marked, sid: "p-sub", crazeID: marked, workspace: ws, runnableOK: true,
			settings: hub.Reopening{Model: "m-1", Effort: "high", NoForce: true, Plan: true, Unattended: true}},
	} {
		got, perr := resolve(tc.id)
		if perr != nil {
			t.Fatalf("%s: refused %+v", tc.id, perr)
		}
		if got.Load != tc.load || got.ProviderSessionID != tc.sid || got.CrazeID != tc.crazeID || got.Workspace != tc.workspace ||
			got.Model != tc.settings.Model || got.Effort != tc.settings.Effort || got.NoForce != tc.settings.NoForce ||
			got.Ask != tc.settings.Ask || got.Plan != tc.settings.Plan || got.Unattended != tc.settings.Unattended {
			t.Errorf("%s resolved to %+v", tc.id, got)
		}
		err := got.Runnable()
		switch {
		case tc.runnableOK && err != nil:
			t.Errorf("%s: Runnable = %v", tc.id, err)
		case !tc.runnableOK && (err == nil || err.Error() != "that session ran in "+gone+", which is no longer a directory"):
			t.Errorf("%s: Runnable = %v, want the gone workspace in its words", tc.id, err)
		}
	}
	for _, id := range []string{"0199c0de-9999-7000-8000-000000000009", "cursor:nope", "nosuch:p-1"} {
		if _, perr := resolve(id); perr == nil || perr.Data.Code != protocol.CodeUnknownSession || perr.Data.Reason != protocol.ReasonUnknownSession {
			t.Errorf("%s: %+v, want unknown_session", id, perr)
		}
	}
	for id, why := range map[string]string{
		"0199c0de-5555-7000-8000-000000000005": `that session's provider "no-such-provider" is not one this craze can resume`,
		"0199c0de-6666-7000-8000-000000000006": `that session's provider "no-such-provider" is not one this craze can resume`,
		"cursor:p-6a":                          `that session's provider "no-such-provider" is not one this craze can resume`,
		"0199c0de-7777-7000-8000-000000000007": "that session's index row is malformed",
		"no-such-provider:p-8":                 `that session's provider "no-such-provider" is not one this craze can resume`,
	} {
		if _, perr := resolve(id); perr == nil || perr.Data.Code != protocol.CodeNotAccepting || perr.Data.Reason != protocol.ReasonNotResumable ||
			perr.Message != why {
			t.Errorf("%s: %+v, want not_resumable: %s", id, perr, why)
		}
	}
	writeFileT(t, filepath.Join(env.CrazeDir, "sessions.jsonl"), "{not json\n")
	var said []string
	_, perr := reopenTarget(plain, func(format string, args ...any) { said = append(said, fmt.Sprintf(format, args...)) })
	if perr == nil || perr.Data.Code != protocol.CodeNotAccepting || perr.Data.Reason != protocol.ReasonNotResumable ||
		strings.Contains(perr.Message, env.CrazeDir) || len(said) != 1 {
		t.Fatalf("an unreadable index: %+v, logged %q", perr, said)
	}
}

// TestNewLoadRefusesWhatAReopenDoesNotTake (B6): craze new --load with a
// prompt, or with any flag that starts a new session or waits for a turn, is
// a usage error, exit 2, before anything is dialled; an empty id too.
func TestNewLoadRefusesWhatAReopenDoesNotTake(t *testing.T) {
	serveHome(t)
	for _, tc := range []struct {
		argv []string
		want string
	}{
		{[]string{"new", "--load", "abcd", "do", "it"}, "craze new: --load takes no prompt: reopen the session, then craze send it one"},
		{[]string{"new", "--load", "abcd", "--prompt-file", "/dev/null"}, "craze new: --load does not take --prompt-file: a reopened session keeps its own"},
		{[]string{"new", "--load", "abcd", "--provider", "grok"}, "craze new: --load does not take --provider: a reopened session keeps its own"},
		{[]string{"new", "--load", "abcd", "--model", "m"}, "craze new: --load does not take --model: a reopened session keeps its own"},
		{[]string{"new", "--load", "abcd", "--effort", "high"}, "craze new: --load does not take --effort: a reopened session keeps its own"},
		{[]string{"new", "--load", "abcd", "--fast"}, "craze new: --load does not take --fast: a reopened session keeps its own"},
		{[]string{"new", "--load", "abcd", "--no-fast"}, "craze new: --load does not take --no-fast: a reopened session keeps its own"},
		{[]string{"new", "--load", "abcd", "--no-force"}, "craze new: --load does not take --no-force: a reopened session keeps its own"},
		{[]string{"new", "--load", "abcd", "--title", "t"}, "craze new: --load does not take --title: a reopened session keeps its own"},
		{[]string{"new", "--load", "abcd", "-C", "/tmp"}, "craze new: --load does not take -C/--dir: a reopened session keeps its own"},
		{[]string{"new", "--load", "abcd", "--wait"}, "craze new: --load does not take --wait: a reopened session keeps its own"},
		{[]string{"new", "--load", "abcd", "--plan"}, "craze new: --load does not take --plan: a reopened session keeps its own"},
		{[]string{"new", "--load", "abcd", "--ask"}, "craze new: --load does not take --ask: a reopened session keeps its own"},
		{[]string{"new", "--load", "abcd", "--timeout", "1s"}, "craze new: --load does not take --timeout: a reopened session keeps its own"},
		{[]string{"new", "--load", "abcd", "--max-message", "9"}, "craze new: --load does not take --max-message: a reopened session keeps its own"},
		{[]string{"new", "--load", "  "}, "craze new: --load needs a session id (craze ps --saved lists them)"},
		{[]string{"new", "--load", "abcd", "--json", "x"}, "craze new: --load takes no prompt: reopen the session, then craze send it one"},
	} {
		stdout, stderr, code := executeIn(tc.argv, noStdin())
		if code != 2 || stdout != "" || stderr != tc.want+"\n" {
			t.Errorf("%v exited %d: stdout %q, stderr %q; want 2, %q", tc.argv, code, stdout, stderr, tc.want)
		}
	}
}

// TestNewLoadResolvesShortIDs (B6): craze new --load's id is resolved over
// the index by the short-session-id tiers — a whole id (a craze id, a
// provider session id, a sessions.saved id) first; then a craze id's end, or
// a legacy row's provider session id's end (what craze ps --saved shows for
// it); then a craze id's beginning, each past the first at least four
// characters — running sessions included. Several in the deciding tier are
// exit 1 naming them; none is `no saved session`. Nothing is dialled: this
// test binary starts no hub, so a resolved id fails there, at the hub.
func TestNewLoadResolvesShortIDs(t *testing.T) {
	_, ws := serveHome(t)
	a, b := "0199c0de-aaaa-7000-8000-0000aaaa1111", "0199c0de-bbbb-7000-8000-0000bbbb1111"
	writeSavedIndex(t,
		savedRow(a, "cursor", "s-a-old", ws, "", 4*time.Minute),
		savedRow(a, "cursor", "s-a", ws, "", time.Minute),
		savedRow(b, "grok", "s-b", ws, "", 2*time.Minute),
		savedRow("", "grok", "grok-session-legacy01", ws, "", 3*time.Minute),
	)
	for id, want := range map[string]string{
		a: a, "aaaa1111": a, "0199c0de-aaaa": a, "s-b": b, "grok:s-b": b, "s-a-old": a, "cursor:s-a-old": a,
		"legacy01": "grok:grok-session-legacy01", "grok:grok-session-legacy01": "grok:grok-session-legacy01",
		"grok-session-legacy01": "grok:grok-session-legacy01",
	} {
		row, err := newLoadRow(id)
		got := ""
		if err == nil {
			got = row.CrazeID
			if got == "" {
				got = row.Provider + ":" + row.SessionID
			}
		}
		if got != want {
			t.Errorf("%s resolved to %q (%v), want %q", id, got, err, want)
		}
	}
	if _, err := newLoadRow("1111"); err == nil ||
		err.Error() != "2 saved sessions match 1111: aaaa1111 (cursor, "+ws+"), bbbb1111 (grok, "+ws+")" {
		t.Errorf("two matches: %v", err)
	}
	for _, id := range []string{"0199c0de-cccc", "aaa", "zzzz"} {
		if _, err := newLoadRow(id); err == nil || err.Error() != "no saved session "+id+" (craze ps --saved lists them)" {
			t.Errorf("%s: %v", id, err)
		}
	}
	stdout, stderr, code := loadIn(noStdin(), "zzzz")
	if code != 1 || stdout != "" || stderr != "craze new: no saved session zzzz (craze ps --saved lists them)\n" {
		t.Fatalf("craze new --load of no session: exit %d, %q %q", code, stdout, stderr)
	}
	stdout, stderr, code = loadIn(noStdin(), "zzzz", "--json")
	if code != 1 || stdout != `{"error":{"message":"no saved session zzzz (craze ps --saved lists them)"}}`+"\n" {
		t.Fatalf("craze new --load --json of no session: exit %d, %q %q", code, stdout, stderr)
	}
	stdout, stderr, code = loadIn(noStdin(), "aaaa1111")
	if code != 1 || stdout != "" || !regexp.MustCompile(`^craze new: hub: no hub in a test binary[^\n]*\n$`).MatchString(stderr) {
		t.Fatalf("craze new --load with no hub: exit %d, %q %q", code, stdout, stderr)
	}
}

// TestSavedAndReopenAgree (C7 review, item 9): sessions.saved lists an id
// exactly when session.reopen resolves it — over every id the index names,
// craze ids and legacy keys, whatever their rows: a craze id whose newest row
// cannot be resumed, one whose newest row is malformed, a legacy row of a
// provider not known, and good ones beside them.
func TestSavedAndReopenAgree(t *testing.T) {
	env, ws := serveHome(t)
	writeSavedIndex(t,
		savedRow("c-good", "cursor", "p-good", ws, "", time.Minute),
		savedRow("c-good", "nosuch", "p-good-old", ws, "", 2*time.Minute),
		savedRow("c-unknown", "nosuch", "p-u-new", ws, "", 3*time.Minute),
		savedRow("c-unknown", "cursor", "p-u-old", ws, "", 4*time.Minute),
		savedRow("c-bad", "grok", "p-b-new", "rel", "", 5*time.Minute),
		savedRow("c-bad", "grok", "p-b-old", ws, "", 6*time.Minute),
		savedRow("", "grok", "legacy-good", ws, "", 7*time.Minute),
		savedRow("", "nosuch", "legacy-unknown", ws, "", 8*time.Minute),
	)
	res, perr := savedSessions(env, protocol.SavedParams{SubAgents: true, Limit: protocol.SavedLimitMax}, noLog(t))
	if perr != nil {
		t.Fatal(perr)
	}
	listed := map[string]bool{}
	for _, s := range res.Sessions {
		listed[s.ID] = true
	}
	if !listed["c-good"] || !listed["grok:legacy-good"] || len(res.Sessions) != 2 {
		t.Fatalf("listed %v; want c-good and the good legacy row alone", listed)
	}
	// Each id the index names, and each legacy key of a craze id's row —
	// which reopens as its craze id does — against the id that lists it.
	for id, listedAs := range map[string]string{
		"c-good": "c-good", "c-unknown": "c-unknown", "c-bad": "c-bad",
		"grok:legacy-good": "grok:legacy-good", "nosuch:legacy-unknown": "nosuch:legacy-unknown",
		"cursor:p-good": "c-good", "nosuch:p-good-old": "c-good", "nosuch:p-u-new": "c-unknown",
		"cursor:p-u-old": "c-unknown", "grok:p-b-new": "c-bad", "grok:p-b-old": "c-bad",
	} {
		if _, perr := reopenTarget(id, noLog(t)); (perr == nil) != listed[listedAs] {
			t.Errorf("%s: reopened %v (%+v), and %s listed %v", id, perr == nil, perr, listedAs, listed[listedAs])
		}
	}
}

// TestAMalformedCrazeIDIsNeverReopened (C7 review r2, item 5): a row whose
// craze id is not a token a load takes (rundir.ValidToken) is malformed —
// the review's input: row A's craze id "cursor:p-b", which a reopen reads
// as row B's legacy key — so sessions.saved never lists A (by "cursor:p-b",
// which would reopen B), and neither session.reopen's resolution nor craze
// new --load's resolves to A by any of its ids: its legacy key is
// not_resumable (malformed), never reopened as B; "cursor:p-b" is B's own
// legacy key, and names B.
func TestAMalformedCrazeIDIsNeverReopened(t *testing.T) {
	env, ws := serveHome(t)
	writeSavedIndex(t,
		savedRow("cursor:p-b", "cursor", "p-a", ws, "row A", time.Minute),
		savedRow("c-b", "cursor", "p-b", ws, "row B", 2*time.Minute),
	)
	res, perr := savedSessions(env, protocol.SavedParams{SubAgents: true, Limit: protocol.SavedLimitMax}, noLog(t))
	if perr != nil {
		t.Fatal(perr)
	}
	if got := savedIDs(res); !slices.Equal(got, []string{"c-b"}) {
		t.Errorf("listed %v, want row B alone, c-b", got)
	}
	if _, perr := reopenTarget("cursor:p-a", noLog(t)); perr == nil || perr.Data.Code != protocol.CodeNotAccepting ||
		perr.Data.Reason != protocol.ReasonNotResumable || perr.Message != "that session's index row is malformed" {
		t.Errorf("row A's legacy key: %+v, want not_resumable, malformed", perr)
	}
	for _, id := range []string{"c-b", "cursor:p-b"} {
		got, perr := reopenTarget(id, noLog(t))
		if perr != nil || got.Load != "c-b" || got.CrazeID != "c-b" || got.ProviderSessionID != "p-b" {
			t.Fatalf("%s resolved to %+v (%+v), want row B", id, got, perr)
		}
	}
	for _, id := range []string{"cursor:p-a", "p-a"} {
		if row, err := newLoadRow(id); err == nil || err.Error() != "no saved session "+id+" (craze ps --saved lists them)" {
			t.Errorf("craze new --load %s: %+v, %v; want no saved session", id, row, err)
		}
	}
	for _, id := range []string{"c-b", "cursor:p-b", "p-b"} {
		if row, err := newLoadRow(id); err != nil || row.CrazeID != "c-b" || row.SessionID != "p-b" {
			t.Errorf("craze new --load %s: %+v, %v; want row B", id, row, err)
		}
	}
}

// noStdin is an empty stdin: what craze new --load is run with here (it reads
// none, as plain craze new reads none), never the test binary's own.
func noStdin() io.Reader { return strings.NewReader("") }

// loadIn is craze new --load with stdin, as craze runs it: its stdout, its
// stderr and its exit code.
func loadIn(stdin io.Reader, argv ...string) (stdout, stderr string, code int) {
	return executeIn(append([]string{"new", "--load"}, argv...), stdin)
}

// TestNewLoadAgainstAnOlderHub (B-5, B7): a running hub whose hello does not
// say reopen — the real hub given this build's answers less Reopen — is
// LacksError's words for craze new --load, with the hub's pid, exit 1, and no
// session.reopen sent; never another hub spawned beside it.
func TestNewLoadAgainstAnOlderHub(t *testing.T) {
	_, env := providersHubMachine(t)
	writeSavedIndex(t, savedRow("0199c0de-aaaa-7000-8000-0000aaaa1111", "cursor", "s-a", env.Home, "", time.Minute))
	creates := hubCreates()
	creates.Reopen = nil
	inProcessHub(t, env, creates)
	prev := hub.Command
	hub.Command = func([]string) (*exec.Cmd, error) {
		t.Error("craze new --load spawned a hub beside the running one")
		return nil, errors.New("no hub is spawned here")
	}
	t.Cleanup(func() { hub.Command = prev })
	stdout, stderr, code := loadIn(noStdin(), "aaaa1111")
	want := fmt.Sprintf("craze new: this hub (craze %s) cannot reopen a saved session; it exits once no session runs "+
		"and no client is connected, or stop it now with kill %d (no session ends)\n", version.Version, os.Getpid())
	if code != 1 || stdout != "" || stderr != want {
		t.Fatalf("craze new --load of an older hub: exit %d, stdout %q, stderr %q; want %q", code, stdout, stderr, want)
	}
}

// reopenMachine is a HOME whose config has the fake agent as cursor, its
// script script (load: every session/load is answered, replaying a little), a
// short host idle exit, and a hub that starts real hosts (cliChildHubHosts):
// its registry and a workspace. Every host still running when the test ends is
// stopped.
func reopenMachine(t *testing.T, script string, extra ...string) (rundir.Env, string) {
	t.Helper()
	_, ws := serveHome(t)
	writeCrazeConfig(t, fmt.Sprintf("host_idle_exit = \"30s\"\n\n[agents]\ncursor = %q\n", fakeAgentPath(t)))
	t.Setenv("CRAZE_FAKE_SCRIPT", script)
	env := rundir.ProcessEnv()
	hubAsChild(t, append([]string{cliChildHubHosts + "=1"}, extra...)...)
	t.Cleanup(func() { stopEveryHost(t, env) })
	return env, absDir(ws)
}

// serving is every live host in env's registry serving craze session id.
func serving(t *testing.T, env rundir.Env, id string) []rundir.Entry {
	t.Helper()
	entries, err := rundir.Hosts(env)
	if err != nil {
		t.Fatal(err)
	}
	var out []rundir.Entry
	for _, e := range entries {
		if e.CrazeSessionID == id {
			out = append(out, e)
		}
	}
	return out
}

// TestNewLoadThroughTheHub (B-4 (i), (ii), B-5): a saved session reopened by
// the id craze ps --saved prints is loaded in a host the hub starts — craze
// serve --load in its workspace — and says `reopened <id> in <dir>`; craze ps
// then lists it (polled: a roster subscription another client holds can
// answer a list a second stale) and craze ps --saved no longer does; craze new
// --load again says `already running`, the same host, and its --json is the
// hub's result, joined.
func TestNewLoadThroughTheHub(t *testing.T) {
	env, ws := reopenMachine(t, "load")
	const id = "0199c0de-feed-7000-8000-00000000feed"
	seedIndexRow(t, sessions.Row{SessionID: "fake-session-1", Provider: "cursor", CWD: ws, CrazeID: id, Title: "a saved one"})
	stdout, stderr, code := loadIn(noStdin(), "0000feed")
	if code != 0 || stderr != "" || stdout != "reopened 0000feed in "+psTilde(env.Home, ws)+"\n" {
		t.Fatalf("craze new --load: exit %d, %q %q (hub log: %s)", code, stdout, stderr, hubLog(env)())
	}
	hosts := serving(t, env, id)
	if len(hosts) != 1 || hosts[0].Workspace != ws || hosts[0].RequestID != "" {
		t.Fatalf("the hosts serving the reopened session: %+v", hosts)
	}
	waitFor(t, "craze ps listing the reopened session", func() bool {
		out, _, code := executeErr([]string{"ps"})
		return code == 0 && strings.Contains(out, "0000feed")
	})
	if out, _, code := executeErr([]string{"ps", "--saved"}); code != 0 || strings.Contains(out, "0000feed") {
		t.Fatalf("craze ps --saved lists the running session: exit %d\n%s", code, out)
	}
	stdout, stderr, code = loadIn(noStdin(), id)
	if code != 0 || stderr != "" || stdout != "already running 0000feed in "+psTilde(env.Home, ws)+"\n" {
		t.Fatalf("craze new --load of the running session: exit %d, %q %q", code, stdout, stderr)
	}
	stdout, _, code = loadIn(noStdin(), "0000feed", "--json")
	var res protocol.ReopenResult
	if code != 0 || json.Unmarshal([]byte(stdout), &res) != nil || !res.Joined || res.Session.HostID != hosts[0].HostID {
		t.Fatalf("craze new --load --json: exit %d, %q", code, stdout)
	}
	if again := serving(t, env, id); len(again) != 1 || again[0].HostID != hosts[0].HostID {
		t.Fatalf("hosts serving the session after the joins: %+v", again)
	}
}

// TestConcurrentLoadsThroughTheHubEndOnOneHost (B-4 (iii), (viii), B9 (vii)):
// two craze new --load of one saved session at once end on one host, one
// reopened and one already running. The moment is forced: each reopen's host
// is held once it has read the session's row, before it gives it a craze id
// or claims it (cliChildHubRowGates), until the other's is held too, and both
// are let go together — so both race the claim, and the real hosts' claim
// decides. A legacy row — no craze id yet — reopened twice at once, both its
// hosts held having read it with none, ends on one host too: each host gives
// the row its craze id under the index's lock, so both claim the one craze id
// (the real index-minting race), and both answers carry it, which the index
// then holds. Stopped, the session reopened by its legacy id and by its craze
// id at once, both hosts held the same way, ends on one host again.
func TestConcurrentLoadsThroughTheHubEndOnOneHost(t *testing.T) {
	// The hub's hosts in pairs: the craze id's, the legacy row's, then its
	// legacy id's and craze id's.
	gates := []rowGate{newRowGate(t), newRowGate(t), newRowGate(t), newRowGate(t), newRowGate(t), newRowGate(t)}
	paths := make([]string, len(gates))
	for i, g := range gates {
		paths[i] = g.path
	}
	t.Cleanup(func() {
		for _, g := range gates {
			if g.letGo != nil {
				g.letGo()
			}
		}
	})
	env, ws := reopenMachine(t, "load", cliChildHubRowGates+"="+strings.Join(paths, ","))
	held := func(pair []rowGate, crazeID string) {
		t.Helper()
		for i, g := range pair {
			if row := g.arrived(t); row.CrazeID != crazeID {
				t.Fatalf("held host %d read %+v, want a row of craze id %q", i+1, row, crazeID)
			}
		}
		for _, g := range pair {
			g.release()
		}
	}
	const id = "0199c0de-feed-7000-8000-0000000c0c0c"
	seedIndexRow(t, sessions.Row{SessionID: "fake-session-1", Provider: "cursor", CWD: ws, CrazeID: id})
	a, b := goCrazeIn(noStdin(), "new", "--load", psShort(id)), goCrazeIn(noStdin(), "new", "--load", id)
	held(gates[:2], id)
	outA, errA, codeA := a.within(t, serveStep)
	outB, errB, codeB := b.within(t, serveStep)
	short := psShort(id)
	reopenedLine := "reopened " + short + " in " + psTilde(env.Home, ws) + "\n"
	runningLine := "already running " + short + " in " + psTilde(env.Home, ws) + "\n"
	if codeA != 0 || codeB != 0 || errA != "" || errB != "" || !oneEach(outA, outB, reopenedLine, runningLine) {
		t.Fatalf("two loads at once: exit %d %q %q; exit %d %q %q (hub log: %s)", codeA, outA, errA, codeB, outB, errB, hubLog(env)())
	}
	if hosts := serving(t, env, id); len(hosts) != 1 {
		t.Fatalf("hosts serving the session: %+v", hosts)
	}

	seedIndexRow(t, sessions.Row{SessionID: "fake-legacy-1", Provider: "cursor", CWD: ws})
	a = goCrazeIn(noStdin(), "new", "--load", "cursor:fake-legacy-1", "--json")
	b = goCrazeIn(noStdin(), "new", "--load", "cursor:fake-legacy-1", "--json")
	held(gates[2:4], "")
	var results [2]protocol.ReopenResult
	for i, r := range []*crazeRun{a, b} {
		stdout, stderr, code := r.within(t, serveStep)
		if code != 0 || json.Unmarshal([]byte(stdout), &results[i]) != nil {
			t.Fatalf("a legacy row's reopen: exit %d, %q %q (hub log: %s)", code, stdout, stderr, hubLog(env)())
		}
	}
	minted := results[0].Session.SessionID
	if minted == "" || results[1].Session.SessionID != minted || results[0].Joined == results[1].Joined ||
		results[0].Session.HostID != results[1].Session.HostID {
		t.Fatalf("a legacy row reopened twice at once: %+v and %+v; want one host, one craze id, one joined", results[0], results[1])
	}
	row, ok, err := (&sessions.Store{}).Find("cursor", "fake-legacy-1")
	if err != nil || !ok || row.CrazeID != minted {
		t.Fatalf("the legacy row after its reopen: %+v, %v, %v; want craze id %s", row, ok, err, minted)
	}
	hosts := serving(t, env, minted)
	if len(hosts) != 1 {
		t.Fatalf("hosts serving the legacy session: %+v", hosts)
	}
	if err := stopHost(hosts[0]); err != nil {
		t.Fatal(err)
	}
	waitReaped(t, hosts[0].PID)
	a, b = goCrazeIn(noStdin(), "new", "--load", "cursor:fake-legacy-1"), goCrazeIn(noStdin(), "new", "--load", minted)
	held(gates[4:], minted)
	outA, errA, codeA = a.within(t, serveStep)
	outB, errB, codeB = b.within(t, serveStep)
	short = psShort(minted)
	reopenedLine = "reopened " + short + " in " + psTilde(env.Home, ws) + "\n"
	runningLine = "already running " + short + " in " + psTilde(env.Home, ws) + "\n"
	if codeA != 0 || codeB != 0 || !oneEach(outA, outB, reopenedLine, runningLine) {
		t.Fatalf("a legacy id and its craze id at once: exit %d %q %q; exit %d %q %q (hub log: %s)", codeA, outA, errA, codeB, outB, errB, hubLog(env)())
	}
	if again := serving(t, env, minted); len(again) != 1 || again[0].HostID == hosts[0].HostID {
		t.Fatalf("hosts serving the legacy session after the stop: %+v (the stopped one was %s)", again, hosts[0].HostID)
	}
}

// TestAReopenWhoseLoadOutlastsACPsBound (B9 (x), R1): a reopen's start covers
// the agent's session/load, which ACP bounds itself (90 s, scaled down in the
// host here, cliChildLoadTimeout): an agent that never answers the load is a
// start that failed — craze new --load's not_accepting, reason start_failed,
// its cause the host's — and the hub stops the host it started for it.
func TestAReopenWhoseLoadOutlastsACPsBound(t *testing.T) {
	// Carried on to the hub child, and from it to its hosts.
	t.Setenv(cliChildLoadTimeout, "300ms")
	env, ws := reopenMachine(t, "load-hang")
	const id = "0199c0de-feed-7000-8000-00000000d0d0"
	seedIndexRow(t, sessions.Row{SessionID: "fake-session-1", Provider: "cursor", CWD: ws, CrazeID: id})
	stdout, stderr, code := loadIn(noStdin(), id, "--json")
	var got struct {
		Error protocol.Error `json:"error"`
	}
	if code != 1 || json.Unmarshal([]byte(stdout), &got) != nil || got.Error.Data.Code != protocol.CodeNotAccepting ||
		got.Error.Data.Reason != protocol.ReasonStartFailed || got.Error.Data.Cause == "" ||
		stderr != "craze new: "+got.Error.Message+"\n" {
		t.Fatalf("a load past ACP's bound: exit %d, %q %q (hub log: %s)", code, stdout, stderr, hubLog(env)())
	}
	waitFor(t, "the reopen's host stopped", func() bool { return len(serving(t, env, id)) == 0 })
}

// oneEach says a and b are x and y, in either order.
func oneEach(a, b, x, y string) bool { return a == x && b == y || a == y && b == x }
