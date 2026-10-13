package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/charliek/craze/internal/hostspawn"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/remote"
	"github.com/charliek/craze/internal/rundir"
)

// session.reopen (plan 046 B3, B9): the hub in this process, every host a
// real child (reopen_helpers_test.go's load child) that takes the session's
// claim as a real host does, every schedule forced through a seam — a scan
// held at a barrier, a holder's stop held at its gates, a wait known to be
// waiting (hostspawn.AwaitPolled) — and each bound on trial shortened for its
// own test only.

// reopenAsync is a reopen of id on a connection of its own to sock, its
// answer sent on the channel returned: a goroutine's, which may not fail the
// test itself.
func reopenAsync(sock, id string) <-chan reopenOutcome {
	out := make(chan reopenOutcome, 1)
	go func() {
		resp, err := rawCall(sock, protocol.MethodSessionReopen, protocol.ReopenParams{ID: id})
		out <- reopenOutcome{resp, err}
	}()
	return out
}

// reopenOutcome is a reopen's answer, or why there was none.
type reopenOutcome struct {
	resp protocol.Response
	err  error
}

// rawCall says hello on a connection of its own to sock and sends method
// with params: the answer, within step.
func rawCall(sock, method string, params any) (protocol.Response, error) {
	nc, err := net.DialTimeout("unix", sock, step)
	if err != nil {
		return protocol.Response{}, err
	}
	defer nc.Close()
	_ = nc.SetDeadline(time.Now().Add(2 * step))
	lr := protocol.NewLineReader(nc, protocol.OutboundLineMax)
	var resp protocol.Response
	for i, req := range []map[string]any{
		{"jsonrpc": "2.0", "id": 1, "method": protocol.MethodHello,
			"params": protocol.HelloParams{Protocols: []int{1}, Client: protocol.ClientInfo{Kind: "test"}}},
		{"jsonrpc": "2.0", "id": 2, "method": method, "params": params},
	} {
		if err := protocol.WriteLine(nc, req); err != nil {
			return protocol.Response{}, err
		}
		line, err := lr.ReadLine()
		if err != nil {
			return protocol.Response{}, err
		}
		resp = protocol.Response{}
		if err := json.Unmarshal(line, &resp); err != nil {
			return protocol.Response{}, err
		}
		if i == 0 && resp.Error != nil {
			return resp, fmt.Errorf("hello refused: %v", resp.Error)
		}
	}
	return resp, nil
}

// outcome is a reopen's answer from ch, within step.
func outcome(t *testing.T, ch <-chan reopenOutcome) protocol.Response {
	t.Helper()
	select {
	case o := <-ch:
		if o.err != nil {
			t.Fatalf("session.reopen: %v", o.err)
		}
		return o.resp
	case <-time.After(2 * step):
		t.Fatalf("session.reopen: no answer within %v", 2*step)
	}
	return protocol.Response{}
}

// awaiting is a hand on hostspawn.AwaitPolled: each of its reads of the
// registry is counted, so a test knows a reopen is waiting for a holder.
type awaiting struct{ polls atomic.Int64 }

func awaitingPolls(t *testing.T) *awaiting {
	t.Helper()
	a := &awaiting{}
	setVar(t, &hostspawn.AwaitPolled, func() { a.polls.Add(1) })
	return a
}

// waiting waits, within step, for a holder's wait to be polling.
func (a *awaiting) waiting(t *testing.T) {
	t.Helper()
	waitFor(t, "a reopen waiting for a holder", func() bool { return a.polls.Load() > 0 })
}

// pending fails the test when ch has an answer already.
func pending(t *testing.T, ch <-chan reopenOutcome, what string) {
	t.Helper()
	select {
	case o := <-ch:
		t.Fatalf("%s: the reopen answered already: %+v %v", what, o.resp, o.err)
	default:
	}
}

// sameDir says a and b are one directory (a temporary directory's path can
// name it through a link: macOS's /var is /private/var).
func sameDir(t *testing.T, a, b string) bool {
	t.Helper()
	ra, err := filepath.EvalSymlinks(a)
	if err != nil {
		return false
	}
	rb, err := filepath.EvalSymlinks(b)
	return err == nil && ra == rb
}

// TestReopenIsServedByAHubGivenItsAnswer (B1, B7): a hub given the means to
// resolve a saved session (Creates.Reopen) serves session.reopen, and its
// hello says reopen only when it serves sessions.saved too (Creates.Saved);
// a hub without Reopen — and one with no way to start a host — refuses it
// unsupported, reason unsupported, its hello silent.
func TestReopenIsServedByAHubGivenItsAnswer(t *testing.T) {
	withoutSaved := reopenCreates(newTargets())
	withoutSaved.Saved = nil
	for _, tc := range []struct {
		name   string
		cr     *Creates
		says   bool
		served bool
	}{
		{"no creates", nil, false, false},
		{"no reopen", creates("cursor"), false, false},
		{"reopen without saved", withoutSaved, false, true},
		{"reopen and saved", reopenCreates(newTargets()), true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rn := runWith(t, testEnv(t), nil, tc.cr)
			c := dial(t, rn.line(t).Socket)
			if got := c.hello(t).Capabilities.Reopen; got != tc.says {
				t.Fatalf("the hello says reopen %v, want %v", got, tc.says)
			}
			resp := reopenOn(t, c, protocol.ReopenParams{ID: "s-1"})
			if tc.served {
				refusedAs(t, resp, protocol.CodeUnknownSession, protocol.ReasonUnknownSession)
				return
			}
			refusedAs(t, resp, protocol.CodeUnsupported, protocol.ReasonUnsupported)
		})
	}
}

// TestReopenRefusesBadParams (B9's wire negatives): params the schema or the
// hub refuses are bad_request — unknown_field for a member it does not
// define — each resolving nothing and spawning nothing. The negative control:
// an id at its bound, in bytes, is resolved (unknown_session here).
func TestReopenRefusesBadParams(t *testing.T) {
	env := testEnv(t)
	ts := newTargets()
	_, s, sock := reopenHub(t, env, nil, ts, always("ok"))
	c := dial(t, sock)
	c.hello(t)
	for _, tc := range []struct {
		name   string
		params any
		reason protocol.Reason
	}{
		{"no params", nil, protocol.ReasonBadRequest},
		{"null", json.RawMessage("null"), protocol.ReasonBadRequest},
		{"not an object", []int{1}, protocol.ReasonBadRequest},
		{"no id", map[string]any{}, protocol.ReasonBadRequest},
		{"an unknown member", map[string]any{"id": "s-1", "requestId": "r-1"}, protocol.ReasonUnknownField},
		{"an unknown member alone", map[string]any{"cwd": "/work"}, protocol.ReasonUnknownField},
		{"a null id", json.RawMessage(`{"id":null}`), protocol.ReasonBadRequest},
		{"an id of a number", json.RawMessage(`{"id":7}`), protocol.ReasonBadRequest},
		{"an id of an object", json.RawMessage(`{"id":{"crazeSessionId":"s-1"}}`), protocol.ReasonBadRequest},
		{"an empty id", protocol.ReopenParams{}, protocol.ReasonBadRequest},
		{"an id over its bound", protocol.ReopenParams{ID: strings.Repeat("i", protocol.ReopenIDMax+1)}, protocol.ReasonBadRequest},
		{"an id over its bound in bytes", protocol.ReopenParams{ID: strings.Repeat("é", protocol.ReopenIDMax/2) + "i"}, protocol.ReasonBadRequest},
		{"an id given twice", json.RawMessage(`{"id":"s-1","id":"s-2"}`), protocol.ReasonBadRequest},
		{"an id given twice, once escaped", json.RawMessage(`{"id":"s-1","\u0069d":"s-2"}`), protocol.ReasonBadRequest},
	} {
		resp := reopenOn(t, c, tc.params)
		if resp.Error == nil || resp.Error.Data.Code != protocol.CodeBadRequest || resp.Error.Data.Reason != tc.reason {
			t.Errorf("%s: answered %+v (%s), want bad_request/%s", tc.name, resp.Error, resp.Result, tc.reason)
		}
	}
	ts.mu.Lock()
	asked := ts.asked
	ts.mu.Unlock()
	if asked != 0 || s.count() != 0 {
		t.Fatalf("bad params resolved %d ids and spawned %d hosts", asked, s.count())
	}
	refusedAs(t, reopenOn(t, c, protocol.ReopenParams{ID: strings.Repeat("é", protocol.ReopenIDMax/2)}),
		protocol.CodeUnknownSession, protocol.ReasonUnknownSession)
	if s.count() != 0 {
		t.Fatal("an unknown session spawned a host")
	}
}

// TestReopenLoadsASavedSession (B-4 (i), (ii)): a saved session no host serves
// is loaded in a host the hub spawns — craze serve --load with the id, run in
// the session's workspace, --no-host-status and the hub's pid, and never
// --provider, --workspace, --title, --model, --effort, --fast, --no-fast,
// --no-force or a request — and answered once started, joined false, its
// row fresh. Reopened again it is joined: the same host, joined true, nothing
// spawned. A sub-agent's row loads with its reload settings, as craze send
// reloads it.
func TestReopenLoadsASavedSession(t *testing.T) {
	env := testEnv(t)
	ws := t.TempDir()
	ts := newTargets(plainTarget("s-1", ws))
	_, s, sock := reopenHub(t, env, nil, ts, always("ok"))
	res := reopened(t, reopenID(t, sock, "s-1"))
	row := res.Session
	switch {
	case res.Joined:
		t.Fatal("a reopen that started a host says joined")
	case row.SessionID != "s-1" || row.Approximate || !row.Host.Ready || row.Status != protocol.RosterReachable || !sameDir(t, row.Host.Workspace, ws):
		t.Fatalf("the reopened session's row %+v", row)
	case s.count() != 1:
		t.Fatalf("%d hosts spawned, want 1", s.count())
	}
	if hosts := hostsServing(t, env, "s-1"); len(hosts) != 1 || hosts[0].HostID != row.HostID {
		t.Fatalf("the hosts serving the session: %+v, want %s alone", hosts, row.HostID)
	}
	cmd, argv := s.cmd(0)
	for _, want := range []string{"serve", "--host-id=" + row.HostID, "--load=s-1", "--no-host-status", "--hub-pid=" + strconv.Itoa(os.Getpid())} {
		if !slices.Contains(argv, want) {
			t.Errorf("the host's command line %q lacks %s", argv, want)
		}
	}
	for _, not := range []string{"--provider", "--workspace", "--title", "--model", "--effort", "--fast", "--no-fast", "--no-force",
		"--request-id", "--request-hash", "--unattended", "--ask", "--plan", "--agent-bin"} {
		if slices.ContainsFunc(argv, func(a string) bool { return a == not || strings.HasPrefix(a, not+"=") }) {
			t.Errorf("the host's command line %q passes %s", argv, not)
		}
	}
	if cmd.Dir != ws {
		t.Errorf("the host runs in %q, want the session's workspace %q", cmd.Dir, ws)
	}

	again := reopened(t, reopenID(t, sock, "s-1"))
	if !again.Joined || again.Session.HostID != row.HostID || again.Session.SessionID != "s-1" || s.count() != 1 {
		t.Fatalf("a reopen of the running session answered %+v after %d spawns; want host %s joined", again, s.count(), row.HostID)
	}

	ts.set("s-2", Reopening{Load: "s-2", CrazeID: "s-2", Provider: "cursor", Workspace: ws, Model: "m-1", Effort: "high",
		NoForce: true, Plan: true, Unattended: true})
	if res := reopened(t, reopenID(t, sock, "s-2")); res.Joined || res.Session.SessionID != "s-2" {
		t.Fatalf("the sub-agent's session reopened %+v", res)
	}
	_, argv = s.cmd(1)
	for _, want := range []string{"--load=s-2", "--model=m-1", "--effort=high", "--no-force", "--plan", "--unattended", "--no-host-status"} {
		if !slices.Contains(argv, want) {
			t.Errorf("the sub-agent's host's command line %q lacks %s", argv, want)
		}
	}
}

// TestReopenStatsOnlyToSpawn (B3.5, B-4 (v)): a session whose workspace is no
// longer a directory is not_accepting, reason not_resumable, in the check's
// own words, spawning nothing — and the same session running already is
// joined all the same: a join never looks at the workspace.
func TestReopenStatsOnlyToSpawn(t *testing.T) {
	env := testEnv(t)
	ws := t.TempDir()
	gone := filepath.Join(t.TempDir(), "gone")
	runnable := func(dir string) func() error {
		return func() error {
			if st, err := os.Stat(dir); err != nil || !st.IsDir() {
				return fmt.Errorf("that session ran in %s, which is no longer a directory", dir)
			}
			return nil
		}
	}
	t1 := plainTarget("s-1", gone)
	t1.Runnable = runnable(gone)
	ts := newTargets(t1)
	_, s, sock := reopenHub(t, env, nil, ts, always("ok"))
	e := refusedAs(t, reopenID(t, sock, "s-1"), protocol.CodeNotAccepting, protocol.ReasonNotResumable)
	if e.Message != "that session ran in "+gone+", which is no longer a directory" || s.count() != 0 {
		t.Fatalf("a gone workspace answered %q after %d spawns", e.Message, s.count())
	}

	t2 := plainTarget("s-2", ws)
	t2.Runnable = runnable(ws)
	ts.set("s-2", t2)
	holder := startHolder(t, env, "s-2", ws, "ok", false)
	if err := os.Remove(ws); err != nil {
		t.Fatal(err)
	}
	if res := reopened(t, reopenID(t, sock, "s-2")); !res.Joined || res.Session.HostID != holder.id || s.count() != 0 {
		t.Fatalf("a running session whose workspace has gone answered %+v after %d spawns; want host %s joined", res, s.count(), holder.id)
	}
}

// TestReopenReadyLineAnswers (B3 b.3, B-4 (v)): what a spawned host's ready
// line says is the reopen's answer — a transient refusal (the index busy)
// unavailable, reason busy; any other refusal not_accepting, reason
// not_resumable, in the host's words; a not-ok line that is neither held nor
// refused spawn_failed — each host settled and reaped. A held answer whose
// holder has gone is spawned past, three spawns at most: then busy.
func TestReopenReadyLineAnswers(t *testing.T) {
	env := testEnv(t)
	ws := t.TempDir()
	dead := exec.Command("true")
	if err := dead.Run(); err != nil {
		t.Fatal(err)
	}
	modes := []string{"refuse:" + hostspawn.IndexBusy, "refuse:no session s-1", "notok"}
	_, s, sock := reopenHub(t, env, nil, newTargets(plainTarget("s-1", ws)), func(n int) string {
		if n < len(modes) {
			return modes[n]
		}
		return "heldby:" + strconv.Itoa(dead.Process.Pid)
	})
	for i, want := range []struct {
		code    protocol.Code
		reason  protocol.Reason
		message string
	}{
		{protocol.CodeUnavailable, protocol.ReasonBusy, hostspawn.IndexBusy},
		{protocol.CodeNotAccepting, protocol.ReasonNotResumable, "no session s-1"},
		{protocol.CodeUnavailable, protocol.ReasonSpawnFailed, "the session host could not start: the control socket: no such place"},
	} {
		if e := refusedAs(t, reopenID(t, sock, "s-1"), want.code, want.reason); e.Message != want.message {
			t.Errorf("spawn %d answered %q, want %q", i, e.Message, want.message)
		}
		s.gone(t, env, i)
	}
	e := refusedAs(t, reopenID(t, sock, "s-1"), protocol.CodeUnavailable, protocol.ReasonBusy)
	if e.Message != "that session changed hands 3 times while the hub reopened it; try again" || s.count() != len(modes)+reopenSpawnsMax {
		t.Fatalf("a session held by holders that go answered %q after %d spawns", e.Message, s.count())
	}
}

// TestConcurrentReopensEndOnOneHost (B-4 (iii), B9 (vii)): two reopens of one
// session at once — both past their scan before either spawns (a barrier),
// so both spawn and the claim decides — end on one host: one answers joined
// false, the other joined true, both naming it. The same holds for a legacy
// id and the craze id its host gives the session, reopened at once.
func TestConcurrentReopensEndOnOneHost(t *testing.T) {
	setVar(t, &reopenHolderWait, step)
	ws := t.TempDir()
	for _, tc := range []struct {
		name     string
		ids      [2]string
		targets  []Reopening
		session  string
		wantJoin int
	}{
		{"one id twice", [2]string{"s-1", "s-1"}, []Reopening{plainTarget("s-1", ws)}, "s-1", 1},
		{"a legacy id and its craze id", [2]string{"cursor:p-1", "cursor-p-1"}, []Reopening{
			{Load: "cursor:p-1", Provider: "cursor", ProviderSessionID: "p-1", Workspace: ws},
			{Load: "cursor-p-1", CrazeID: "cursor-p-1", Provider: "cursor", ProviderSessionID: "p-1", Workspace: ws},
		}, "cursor-p-1", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := testEnv(t)
			b := newBarrier(2)
			setVar(t, &reopenScanned, func(rundir.Entry, bool) { b.wait() })
			_, s, sock := reopenHub(t, env, nil, newTargets(tc.targets...), always("ok"))
			a, c := reopenAsync(sock, tc.ids[0]), reopenAsync(sock, tc.ids[1])
			ra, rc := reopened(t, outcome(t, a)), reopened(t, outcome(t, c))
			joined := 0
			for _, r := range []protocol.ReopenResult{ra, rc} {
				if r.Joined {
					joined++
				}
			}
			switch {
			case ra.Session.HostID != rc.Session.HostID || ra.Session.SessionID != tc.session || rc.Session.SessionID != tc.session:
				t.Fatalf("the two reopens answered %+v and %+v: two hosts", ra.Session, rc.Session)
			case joined != tc.wantJoin:
				t.Fatalf("%d of the two reopens joined, want %d", joined, tc.wantJoin)
			case s.count() != 2:
				t.Fatalf("%d hosts spawned, want 2 (one held)", s.count())
			}
			if hosts := hostsServing(t, env, tc.session); len(hosts) != 1 {
				t.Fatalf("hosts serving the session: %+v", hosts)
			}
		})
	}
}

// TestAReopenAndAnotherLoaderEndOnOneHost (B9 (vi)): a reopen racing another
// loader of the session — a direct craze serve --load — ends with one host,
// and both name it, whichever claims it: the other loader first (the reopen's
// spawn is answered held and joins it), or the reopen's host first (the
// other loader is answered held, naming it).
func TestAReopenAndAnotherLoaderEndOnOneHost(t *testing.T) {
	setVar(t, &reopenHolderWait, step)
	env := testEnv(t)
	ws := t.TempDir()
	scanned, proceed := make(chan struct{}), make(chan struct{})
	var once sync.Once
	setVar(t, &reopenScanned, func(rundir.Entry, bool) {
		once.Do(func() {
			close(scanned)
			<-proceed
		})
	})
	_, s, sock := reopenHub(t, env, nil, newTargets(plainTarget("s-1", ws), plainTarget("s-2", ws)), always("ok"))
	ch := reopenAsync(sock, "s-1")
	<-scanned
	other := startHolder(t, env, "s-1", ws, "ok", false)
	close(proceed)
	res := reopened(t, outcome(t, ch))
	if !res.Joined || res.Session.HostID != other.id || other.line.HostID != other.id || s.count() != 1 {
		t.Fatalf("the reopen answered %+v after %d spawns; the other loader's line %+v", res, s.count(), other.line)
	}

	res = reopened(t, reopenID(t, sock, "s-2"))
	late := startHolder(t, env, "s-2", ws, "ok", true)
	if res.Joined || late.line.Held == nil || late.line.Held.HostID != res.Session.HostID || late.line.Held.CrazeSessionID != "s-2" {
		t.Fatalf("the reopen answered %+v; the later loader's line %+v", res, late.line)
	}
	if hosts := hostsServing(t, env, "s-2"); len(hosts) != 1 || hosts[0].HostID != res.Session.HostID {
		t.Fatalf("hosts serving s-2: %+v", hosts)
	}
}

// TestReopenAfterAStopWaitsForTheClaim (B9 (ii), B-4 (iv)): a session stopped
// whose host has unlinked its registry entry and still holds its claim — its
// start's join running long, held here at its gate — is reopened: the scan
// finds nothing, the spawn is answered held, and the reopen waits for the
// holder's process, never answering meanwhile; once it has gone it spawns
// again and answers a new host.
func TestReopenAfterAStopWaitsForTheClaim(t *testing.T) {
	setVar(t, &reopenHolderWait, step)
	env := testEnv(t)
	ws := t.TempDir()
	stop := shortDir(t, "czs")
	_, s, sock := reopenHub(t, env, nil, newTargets(plainTarget("s-1", ws)), func(n int) string {
		if n == 0 {
			return "ok,mark,stop:" + stop
		}
		return "ok"
	})
	first := reopened(t, reopenID(t, sock, "s-1"))
	e, listed := listedEntry(t, env, first.Session.HostID)
	if !listed {
		t.Fatal("the reopened host is not listed")
	}
	if err := stopCreated(e.Socket, "s-1"); err != nil {
		t.Fatal(err)
	}
	waitFile(t, filepath.Join(stop, "stopping"))
	writeFile(t, filepath.Join(stop, "unlist"))
	waitFile(t, filepath.Join(stop, "unlisted"))
	if _, listed := listedEntry(t, env, first.Session.HostID); listed {
		t.Fatal("the stopped host is listed still")
	}
	aw := awaitingPolls(t)
	ch := reopenAsync(sock, "s-1")
	aw.waiting(t)
	if s.count() != 2 {
		t.Fatalf("%d spawns before the wait, want 2: the second answered held", s.count())
	}
	pending(t, ch, "while the stopped host holds the claim")
	writeFile(t, filepath.Join(stop, "exit"))
	res := reopened(t, outcome(t, ch))
	if res.Joined || res.Session.HostID == first.Session.HostID || res.Session.SessionID != "s-1" || s.count() != 3 {
		t.Fatalf("the reopen after the stop answered %+v after %d spawns; the stopped host was %s", res, s.count(), first.Session.HostID)
	}
}

// TestReopenWaitsOutAHeldStoppingHolder (B9 (iii)): a holder the scan misses
// — a legacy row's session, its host's entry not carrying the row's provider
// session id — which is stopping, its entry marked, is named by the spawn's
// held answer: the reopen waits for it (never joining a host marked
// stopping), then spawns again and answers a new host, by the craze id the
// held answer named.
func TestReopenWaitsOutAHeldStoppingHolder(t *testing.T) {
	setVar(t, &reopenHolderWait, step)
	env := testEnv(t)
	ws := t.TempDir()
	stop := shortDir(t, "czs")
	_, s, sock := reopenHub(t, env, nil, newTargets(Reopening{Load: "cursor:p-1", Provider: "cursor", ProviderSessionID: "p-1", Workspace: ws}),
		always("ok"))
	holder := startHolder(t, env, "cursor-p-1", ws, "ok,mark,stop:"+stop, false)
	stopHolder(t, holder)
	waitFile(t, filepath.Join(stop, "stopping"))
	if e, listed := listedEntry(t, env, holder.id); !listed || !e.Stopping || e.ProviderSessionID == "p-1" {
		t.Fatalf("the holder's entry %+v, listed %v: want listed, stopping, not carrying p-1", e, listed)
	}
	aw := awaitingPolls(t)
	ch := reopenAsync(sock, "cursor:p-1")
	aw.waiting(t)
	if s.count() != 1 {
		t.Fatalf("%d spawns before the wait, want 1: answered held", s.count())
	}
	pending(t, ch, "while the holder stops")
	writeFile(t, filepath.Join(stop, "unlist"))
	writeFile(t, filepath.Join(stop, "exit"))
	res := reopened(t, outcome(t, ch))
	if res.Joined || res.Session.HostID == holder.id || res.Session.SessionID != "cursor-p-1" || s.count() != 2 {
		t.Fatalf("the reopen answered %+v after %d spawns; the stopping holder was %s", res, s.count(), holder.id)
	}
}

// TestAJoinRefusedClosingWaits (B9 (iv), (xi)): a holder found serving whose
// attach is refused closing — stopped between the scan and the join, its mark
// read too late; or a host from before the mark, whose stop marks nothing —
// is a holder on its way out: the reopen waits for its process, then spawns
// and answers a new host. Marked by then, its entry is never joined again;
// unmarked, it is on its way out only tentatively (an idle check's fence
// refuses attaches closing too, and lifts: TestAClosingRefusalIsTriedAgain),
// so it is joined again once the reopen has waited a poll. That join's dial
// fails, or its connection is lost, its entry still listed and unmarked — a
// host from before the mark closes its server before it unlinks its entry
// (forced through the dial, the host held until the reopen waits after it,
// not timed): a host that refused closing and now cannot be reached is on its
// way out tentatively again, waited for, never host_unreachable, and the
// answer is a new host all the same once it goes.
func TestAJoinRefusedClosingWaits(t *testing.T) {
	setVar(t, &reopenHolderWait, step)
	for _, tc := range []struct {
		name string
		mark string
		// lost: the join again is dialled, and its connection lost before
		// its attach is answered, rather than its dial failing.
		lost bool
	}{
		{"stopped after the scan", ",mark", false},
		{"a host from before the mark", "", false},
		{"a host from before the mark, its connection lost", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := testEnv(t)
			ws := t.TempDir()
			stop := shortDir(t, "czs")
			holder := startHolder(t, env, "s-1", ws, "ok,stop:"+stop+tc.mark, false)
			stopErr := make(chan error, 1)
			var joined atomic.Int64
			// The waits' time runs on their polls (loopHooks.tick): a
			// tentative mark's poll passes at a wait's second poll, never at
			// its first, however late.
			var dials atomic.Int64
			// unmarked: the entry listed, unmarked, as the failure was
			// injected; injected: the failure is; waiting closes at the
			// reopen's first poll after it — its wait begun, the holder still
			// held at its stop.
			var unmarked, injected atomic.Bool
			waiting := make(chan struct{})
			var waitingOnce sync.Once
			c := countLoop(t, loopHooks{tick: rendezvousPoll, joining: func(e rundir.Entry) {
				if joined.Add(1) != 1 {
					return
				}
				if tc.mark == "" {
					stopErr <- nil
					return
				}
				stopErr <- stopCreated(e.Socket, e.CrazeSessionID)
				awaitFile(filepath.Join(stop, "stopping"))
			}, polled: func() {
				if injected.Load() {
					waitingOnce.Do(func() { close(waiting) })
				}
			}})
			if tc.mark == "" {
				// Its second dial — the join again — fails, or its connection
				// is lost, its entry still listed, unmarked: its server
				// closed, its entry not yet unlinked.
				setVar(t, &createDial, func(ctx context.Context, socket, sid string) (createdSession, error) {
					if socket != holder.socket || dials.Add(1) != 2 {
						return dialCreated(ctx, socket, sid)
					}
					e, listed, err := rundir.HostEntry(env, holder.id)
					unmarked.Store(err == nil && listed && !e.Stopping)
					if !tc.lost {
						injected.Store(true)
						return nil, fmt.Errorf("dial unix %s: connect: connection refused", socket)
					}
					sess, err := dialCreated(ctx, socket, sid)
					if err != nil {
						return nil, err
					}
					return lostStart{sess, &injected}, nil
				})
				// Stopped before the reopen: its entry unmarked, every attach
				// refused closing.
				stopHolder(t, holder)
				waitFile(t, filepath.Join(stop, "stopping"))
				if e, listed := listedEntry(t, env, holder.id); !listed || e.Stopping {
					t.Fatalf("the old-style holder's entry %+v, listed %v", e, listed)
				}
			}
			_, s, sock := reopenHub(t, env, nil, newTargets(plainTarget("s-1", ws)), always("ok"))
			ch := reopenAsync(sock, "s-1")
			waitFor(t, "a reopen waiting for a holder", func() bool { return c.polls.Load() > 0 })
			if err := <-stopErr; err != nil {
				t.Fatal(err)
			}
			if tc.mark == "" {
				select {
				case <-waiting:
				case o := <-ch:
					t.Fatalf("the reopen answered at the injected failure, never waiting: %+v %v", o.resp.Error, o.err)
				case <-time.After(step):
					t.Fatal("the reopen never waited after the injected failure")
				}
			}
			pending(t, ch, "while the holder stops")
			if s.count() != 0 {
				t.Fatalf("%d spawns while the holder is stopping", s.count())
			}
			writeFile(t, filepath.Join(stop, "unlist"))
			writeFile(t, filepath.Join(stop, "exit"))
			res := reopened(t, outcome(t, ch))
			if res.Joined || res.Session.HostID == holder.id || s.count() != 1 {
				t.Fatalf("the reopen answered %+v after %d spawns; the closing holder was %s", res, s.count(), holder.id)
			}
			// Marked: waited for, never joined again — one join, the refused
			// one. Unmarked: joined again after a slept poll, that join's
			// failure injected with its entry listed, unmarked — then waited
			// for, and joined again a poll later as long as it is listed.
			if n := c.joins.Load(); (tc.mark != "" && n != 1) ||
				(tc.mark == "" && (n < 2 || c.slept.Load() < 1 || dials.Load() < 2 || !unmarked.Load() || !injected.Load())) {
				t.Fatalf("the closing holder was joined %d times (%d dials, the failure injected %v, its entry unmarked then %v), after %d slept polls",
					n, dials.Load(), injected.Load(), unmarked.Load(), c.slept.Load())
			}
		})
	}
}

// TestAStoppingHolderIsNeverJoined (B-4 (iv)): a holder whose entry is marked
// stopping and that still takes attaches — a signal's stop marks the entry
// before it raises the attach fence — is never joined: the reopen waits for
// it to go, then answers a new host.
func TestAStoppingHolderIsNeverJoined(t *testing.T) {
	setVar(t, &reopenHolderWait, step)
	env := testEnv(t)
	ws := t.TempDir()
	stop := shortDir(t, "czs")
	holder := startHolder(t, env, "s-1", ws, "ok,mark,stop:"+stop, false)
	if err := syscall.Kill(holder.pid, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	waitFile(t, filepath.Join(stop, "stopping"))
	if e, listed := listedEntry(t, env, holder.id); !listed || !e.Stopping {
		t.Fatalf("the signalled holder's entry %+v, listed %v: want marked", e, listed)
	}
	var joins atomic.Int64
	setVar(t, &reopenJoining, func(rundir.Entry) { joins.Add(1) })
	_, s, sock := reopenHub(t, env, nil, newTargets(plainTarget("s-1", ws)), always("ok"))
	aw := awaitingPolls(t)
	ch := reopenAsync(sock, "s-1")
	aw.waiting(t)
	pending(t, ch, "while the marked holder stops")
	writeFile(t, filepath.Join(stop, "unlist"))
	writeFile(t, filepath.Join(stop, "exit"))
	res := reopened(t, outcome(t, ch))
	if res.Joined || res.Session.HostID == holder.id || s.count() != 1 || joins.Load() != 0 {
		t.Fatalf("the reopen answered %+v after %d spawns and %d joins; the marked holder was %s", res, s.count(), joins.Load(), holder.id)
	}
}

// TestAJoinedHostIsNeverStopped (B9 (v), B-4 (vi)): a joined host whose start
// failed, or ran past the reopen's start budget, is the reopen's start_failed
// — and is left exactly as it was: no session.stop reached it, no signal, and
// it runs on.
func TestAJoinedHostIsNeverStopped(t *testing.T) {
	env := testEnv(t)
	ws := t.TempDir()
	watchFail, watchSlow := shortDir(t, "czw"), shortDir(t, "czw")
	gate := gateDir(t)
	failing := startHolder(t, env, "s-1", ws, "fail,watch:"+watchFail, false)
	slow := startHolder(t, env, "s-2", ws, "gate:"+gate+",watch:"+watchSlow, false)
	_, s, sock := reopenHub(t, env, nil, newTargets(plainTarget("s-1", ws), plainTarget("s-2", ws)), always("ok"))

	e := refusedAs(t, reopenID(t, sock, "s-1"), protocol.CodeNotAccepting, protocol.ReasonStartFailed)
	if e.Data.Cause != failCause {
		t.Errorf("the failed start's cause %q, want %q", e.Data.Cause, failCause)
	}
	// The short start bound is the hanging start's alone: the failed one's
	// cause must come from its host, however slow the machine.
	setVar(t, &reopenStartWait, 300*time.Millisecond)
	e = refusedAs(t, reopenID(t, sock, "s-2"), protocol.CodeNotAccepting, protocol.ReasonStartFailed)
	if e.Data.Cause != "the session did not start within 300ms" {
		t.Errorf("the slow start's cause %q", e.Data.Cause)
	}
	for _, h := range []struct {
		p     holderProc
		watch string
	}{{failing, watchFail}, {slow, watchSlow}} {
		switch {
		case exists(filepath.Join(h.watch, "stopped")):
			t.Errorf("host %s was sent session.stop", h.p.id)
		case exists(filepath.Join(h.watch, "signalled")):
			t.Errorf("host %s was signalled", h.p.id)
		case h.p.child.Exited():
			t.Errorf("host %s has gone", h.p.id)
		}
		if _, listed := listedEntry(t, env, h.p.id); !listed {
			t.Errorf("host %s is no longer listed", h.p.id)
		}
	}
	if s.count() != 0 {
		t.Fatalf("%d hosts spawned beside the joined ones", s.count())
	}
}

// TestTheHolderBudgetRunsOut (B-4 (ix)): a holder that does not finish
// stopping within the holder allowance is unavailable, reason busy, naming it.
func TestTheHolderBudgetRunsOut(t *testing.T) {
	setVar(t, &reopenHolderWait, 300*time.Millisecond)
	env := testEnv(t)
	ws := t.TempDir()
	stop := shortDir(t, "czs")
	holder := startHolder(t, env, "s-1", ws, "ok,mark,stop:"+stop, false)
	stopHolder(t, holder)
	waitFile(t, filepath.Join(stop, "stopping"))
	_, s, sock := reopenHub(t, env, nil, newTargets(plainTarget("s-1", ws)), always("ok"))
	e := refusedAs(t, reopenID(t, sock, "s-1"), protocol.CodeUnavailable, protocol.ReasonBusy)
	want := fmt.Sprintf("that session is held by host %s (pid %d), which has not finished stopping; try again", holder.id, holder.pid)
	if e.Message != want || s.count() != 0 {
		t.Fatalf("the reopen answered %q after %d spawns, want %q", e.Message, s.count(), want)
	}
}

// TestAReopenStartsWithinItsOwnBudget (B9 (x), B-4 (vii)): a reopen's start
// is waited for within the reopen's start budget, not a create's: one slower
// than a create's bound succeeds, and one past the reopen's is start_failed,
// its cause naming that bound, its host — the reopen's own — stopped.
func TestAReopenStartsWithinItsOwnBudget(t *testing.T) {
	setVar(t, &createStartWait, time.Nanosecond)
	setVar(t, &reopenStartWait, step)
	env := testEnv(t)
	ws := t.TempDir()
	gate, never := gateDir(t), gateDir(t)
	_, s, sock := reopenHub(t, env, nil, newTargets(plainTarget("s-1", ws), plainTarget("s-2", ws)), func(n int) string {
		if n == 0 {
			return "gate:" + gate
		}
		return "gate:" + never
	})
	ch := reopenAsync(sock, "s-1")
	waitGate(t, gate)
	openGate(t, gate, "ok")
	if res := reopened(t, outcome(t, ch)); res.Joined || res.Session.SessionID != "s-1" {
		t.Fatalf("a start slower than a create's bound answered %+v", res)
	}

	setVar(t, &reopenStartWait, 300*time.Millisecond)
	e := refusedAs(t, reopenID(t, sock, "s-2"), protocol.CodeNotAccepting, protocol.ReasonStartFailed)
	if e.Data.Cause != "the session did not start within 300ms" {
		t.Fatalf("a start past the reopen's budget: cause %q", e.Data.Cause)
	}
	s.gone(t, env, 1)
}

// TestALostAnswerIsSentAgain (B9 (xii)): a reopen whose connection closes
// before its answer runs on; the same id sent again joins the host it
// started — joined true, the same host — and nothing more is spawned.
func TestALostAnswerIsSentAgain(t *testing.T) {
	env := testEnv(t)
	ws := t.TempDir()
	gate := gateDir(t)
	joining := make(chan struct{})
	var once sync.Once
	setVar(t, &reopenJoining, func(rundir.Entry) { once.Do(func() { close(joining) }) })
	_, s, sock := reopenHub(t, env, nil, newTargets(plainTarget("s-1", ws)), always("gate:"+gate))
	c := dial(t, sock)
	c.hello(t)
	c.send(t, protocol.MethodSessionReopen, protocol.ReopenParams{ID: "s-1"})
	first := waitGate(t, gate)
	// The fake host's gated start runs from before it registers: the scan
	// of the reopen sent again is to find it listed, which is waited for.
	waitFor(t, "the first reopen's host listed", func() bool {
		_, listed := listedEntry(t, env, first)
		return listed
	})
	_ = c.nc.Close()
	ch := reopenAsync(sock, "s-1")
	<-joining
	openGate(t, gate, "ok")
	res := reopened(t, outcome(t, ch))
	if !res.Joined || res.Session.HostID != first || s.count() != 1 {
		t.Fatalf("the reopen sent again answered %+v after %d spawns; the first started %s", res, s.count(), first)
	}
}

// TestAReopenIsCutAtTheTeardown (B3.6): a reopen in flight when the hub tears
// down is cut as a create is — its waiter's connection closes unanswered —
// and the host it was joining is left alone: never stopped, never signalled.
func TestAReopenIsCutAtTheTeardown(t *testing.T) {
	setVar(t, &teardownBound, 500*time.Millisecond)
	env := testEnv(t)
	ws := t.TempDir()
	gate, watch := gateDir(t), shortDir(t, "czw")
	holder := startHolder(t, env, "s-1", ws, "gate:"+gate+",watch:"+watch, false)
	joining := make(chan struct{})
	var once sync.Once
	setVar(t, &reopenJoining, func(rundir.Entry) { once.Do(func() { close(joining) }) })
	rn, s, sock := reopenHub(t, env, nil, newTargets(plainTarget("s-1", ws)), always("ok"))
	c := dial(t, sock)
	c.hello(t)
	c.send(t, protocol.MethodSessionReopen, protocol.ReopenParams{ID: "s-1"})
	<-joining
	rn.sigs <- syscall.SIGTERM
	if err := rn.stopped(t); err != nil {
		t.Fatal(err)
	}
	_ = c.nc.SetReadDeadline(time.Now().Add(step))
	if line, err := c.lr.ReadLine(); err == nil {
		t.Fatalf("the cut reopen was answered %s", line)
	}
	if exists(filepath.Join(watch, "stopped")) || exists(filepath.Join(watch, "signalled")) || holder.child.Exited() || s.count() != 0 {
		t.Fatalf("the joined host was touched: stopped %v, signalled %v, gone %v; %d spawns",
			exists(filepath.Join(watch, "stopped")), exists(filepath.Join(watch, "signalled")), holder.child.Exited(), s.count())
	}
}

// TestReopenFailsClosedOnTheRegistry (B9's wire negatives): a registry that
// cannot be read, or a live host's entry in it, is unavailable, reason
// host_unreachable — whether a host serves the session already cannot be
// known — and nothing is spawned.
func TestReopenFailsClosedOnTheRegistry(t *testing.T) {
	env := testEnv(t)
	ws := t.TempDir()
	_, s, sock := reopenHub(t, env, nil, newTargets(plainTarget("s-1", ws)), always("ok"))
	for _, scan := range []func(rundir.Env) ([]rundir.Entry, []string, error){
		func(rundir.Env) ([]rundir.Entry, []string, error) {
			return nil, nil, errors.New("the registry is gone")
		},
		func(rundir.Env) ([]rundir.Entry, []string, error) { return nil, []string{"0123456789ab"}, nil },
	} {
		setVar(t, &hostsScan, scan)
		refusedAs(t, reopenID(t, sock, "s-1"), protocol.CodeUnavailable, protocol.ReasonHostUnreachable)
	}
	if s.count() != 0 {
		t.Fatalf("%d hosts spawned over a registry that cannot be read", s.count())
	}
}

// rowSession is the session id an answer's inner row names, "" for none.
func rowSession(t *testing.T, row protocol.RosterRow) string {
	t.Helper()
	if len(row.Row) == 0 {
		return ""
	}
	var probe struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(row.Row, &probe); err != nil {
		t.Fatalf("the answer's row %s: %v", row.Row, err)
	}
	return probe.SessionID
}

// TestAReopenAnswersTheSessionItWasAskedFor (C7 review, item 3): a host whose
// session is no longer the one asked for by the time the reopen reads its row
// — a TUI-hosted session's engine changed between the start and the reads,
// played by its fresh row naming another session, or its registry entry
// naming another, or by an attach refused unknown_session for a host the scan
// read stale — is not the answer: the reopen looks again, and answers the
// session it was asked for, its outer id and its row's both, and the host it
// looked past is never stopped. Each host looked past counts once against the
// reopen's moves (TestAReopenThatKeepsMovingIsBusy), and one seen again with
// its entry as it was is waited for until the entry changes
// (TestAStaleHostIsWaitedForNotCounted): here, the host's engine back on the
// session asked for, its entry rewritten as the reopen waits (rewrittenOnce).
func TestAReopenAnswersTheSessionItWasAskedFor(t *testing.T) {
	setVar(t, &reopenHolderWait, step)
	// rewrittenOnce has the first wait for a holder rewrite host id's entry
	// as its engine's start on the session again would: a new incarnation.
	rewrittenOnce := func(t *testing.T, env rundir.Env, id string) {
		var once sync.Once
		setVar(t, &hostspawn.AwaitPolled, func() {
			once.Do(func() {
				if err := rewriteEntry(env, id, func(e *rundir.Entry) { e.Incarnation = "again" }); err != nil {
					panic(err)
				}
			})
		})
	}
	t.Run("a fresh row of another session", func(t *testing.T) {
		env := testEnv(t)
		ws := t.TempDir()
		holder := startHolder(t, env, "s-1", ws, "ok", false)
		rewrittenOnce(t, env, holder.id)
		var reads atomic.Int64
		setVar(t, &createRowRead, func(ctx context.Context, socket, sid string) (json.RawMessage, string, time.Time, error) {
			raw, v, at, err := freshRow(ctx, socket, sid)
			if reads.Add(1) == 1 && err == nil {
				raw = json.RawMessage(strings.Replace(string(raw), `"sessionId":"s-1"`, `"sessionId":"s-2"`, 1))
			}
			return raw, v, at, err
		})
		_, s, sock := reopenHub(t, env, nil, newTargets(plainTarget("s-1", ws)), always("ok"))
		res := reopened(t, reopenID(t, sock, "s-1"))
		if !res.Joined || res.Session.HostID != holder.id || res.Session.SessionID != "s-1" || rowSession(t, res.Session) != "s-1" ||
			reads.Load() != 2 || s.count() != 0 {
			t.Fatalf("answered %+v (row %s) after %d row reads and %d spawns; want host %s, s-1 inside and out, read twice",
				res.Session, res.Session.Row, reads.Load(), s.count(), holder.id)
		}
	})
	t.Run("an entry naming another session", func(t *testing.T) {
		env := testEnv(t)
		ws := t.TempDir()
		holder := startHolder(t, env, "s-1", ws, "ok", false)
		rewrittenOnce(t, env, holder.id)
		var reads atomic.Int64
		var swap atomic.Bool
		setVar(t, &createRowRead, func(ctx context.Context, socket, sid string) (json.RawMessage, string, time.Time, error) {
			if reads.Add(1) == 1 {
				swap.Store(true)
			}
			return freshRow(ctx, socket, sid)
		})
		setVar(t, &hostsRead, func(env rundir.Env) ([]rundir.Entry, error) {
			entries, err := rundir.Hosts(env)
			if swap.CompareAndSwap(true, false) {
				for i := range entries {
					if entries[i].HostID == holder.id {
						entries[i].CrazeSessionID = "s-2"
					}
				}
			}
			return entries, err
		})
		// No ticker of the hub's reads the registry meanwhile: the swapped
		// read is the row's.
		_, s, sock := reopenHub(t, env, quiet(), newTargets(plainTarget("s-1", ws)), always("ok"))
		res := reopened(t, reopenID(t, sock, "s-1"))
		if !res.Joined || res.Session.HostID != holder.id || res.Session.SessionID != "s-1" || rowSession(t, res.Session) != "s-1" ||
			reads.Load() != 2 || s.count() != 0 {
			t.Fatalf("answered %+v (row %s) after %d row reads and %d spawns; want host %s, s-1 inside and out, read twice",
				res.Session, res.Session.Row, reads.Load(), s.count(), holder.id)
		}
	})
	t.Run("an attach refused unknown_session", func(t *testing.T) {
		env := testEnv(t)
		ws := t.TempDir()
		// The host serves s-9; the scan, and the entry read after the
		// refusal, read it stale, serving s-1.
		holder := startHolder(t, env, "s-9", ws, "watch:"+shortDir(t, "czw"), false)
		var scans, entryReads atomic.Int64
		setVar(t, &hostsScan, func(env rundir.Env) ([]rundir.Entry, []string, error) {
			entries, unreadable, err := rundir.HostsScan(env)
			if scans.Add(1) == 1 {
				for i := range entries {
					if entries[i].HostID == holder.id {
						entries[i].CrazeSessionID = "s-1"
					}
				}
			}
			return entries, unreadable, err
		})
		setVar(t, &hostEntry, func(env rundir.Env, id string) (rundir.Entry, bool, error) {
			e, ok, err := rundir.HostEntry(env, id)
			if entryReads.Add(1) == 1 && id == holder.id {
				e.CrazeSessionID = "s-1"
			}
			return e, ok, err
		})
		_, s, sock := reopenHub(t, env, nil, newTargets(plainTarget("s-1", ws)), always("ok"))
		res := reopened(t, reopenID(t, sock, "s-1"))
		if res.Joined || res.Session.HostID == holder.id || res.Session.SessionID != "s-1" || s.count() != 1 {
			t.Fatalf("answered %+v after %d spawns; want a host of its own loading s-1, not %s", res.Session, s.count(), holder.id)
		}
		if holder.child.Exited() {
			t.Fatalf("the host looked past (%s) has gone", holder.id)
		}
	})
}

// TestAReopenThatKeepsMovingIsBusy: hosts the scan finds serving the session
// that never serve it when joined are looked past, each counted once: a
// fourth host looked past is busy, "changed hands", every host left alone.
// Played by four hosts, each serving a session of its own, their entries
// rewritten to name s-1 (attached for s-1, each refuses unknown_session):
// the reopen joins the newest, then waits for it, seeing its entry as it was
// (TestAStaleHostIsWaitedForNotCounted), until the test lands its rewrite —
// and so on, to the oldest.
func TestAReopenThatKeepsMovingIsBusy(t *testing.T) {
	setVar(t, &reopenHolderWait, step)
	env := testEnv(t)
	ws := t.TempDir()
	base := time.Now().Add(-time.Hour)
	var hosts []holderProc
	var real []rundir.Entry
	for i := range reopenSpawnsMax + 1 {
		h := startHolder(t, env, fmt.Sprintf("s-9%d", i), ws, "ok", false)
		hosts, real = append(hosts, h), append(real, entryOf(t, env, h.id))
		if err := rewriteEntry(env, h.id, func(e *rundir.Entry) {
			e.CrazeSessionID, e.Incarnation, e.StartedAt = "s-1", "stale", base.Add(time.Duration(i)*time.Minute)
		}); err != nil {
			t.Fatal(err)
		}
	}
	var joins []string
	var mu sync.Mutex
	var failed error
	setVar(t, &reopenJoining, func(e rundir.Entry) {
		mu.Lock()
		joins = append(joins, e.HostID)
		mu.Unlock()
	})
	var polls atomic.Int64
	setVar(t, &hostspawn.AwaitPolled, func() {
		// Each wait's first poll lands the rewrite of the host it waits
		// for: the newest still stale.
		if n := int(polls.Add(1)); n <= reopenSpawnsMax {
			i := len(hosts) - n
			if err := rewriteEntry(env, hosts[i].id, func(e *rundir.Entry) { *e = real[i] }); err != nil {
				mu.Lock()
				failed = errors.Join(failed, err)
				mu.Unlock()
			}
		}
	})
	_, s, sock := reopenHub(t, env, nil, newTargets(plainTarget("s-1", ws)), always("ok"))
	e := refusedAs(t, reopenID(t, sock, "s-1"), protocol.CodeUnavailable, protocol.ReasonBusy)
	mu.Lock()
	defer mu.Unlock()
	if failed != nil {
		t.Fatal(failed)
	}
	want := []string{hosts[3].id, hosts[2].id, hosts[1].id, hosts[0].id}
	if msg := fmt.Sprintf("that session changed hands %d times while the hub reopened it; try again", reopenSpawnsMax+1); e.Message != msg ||
		!slices.Equal(joins, want) || s.count() != 0 {
		t.Fatalf("answered %q after joins %v and %d spawns, want %q after joins %v", e.Message, joins, s.count(), msg, want)
	}
	for _, h := range hosts {
		if h.child.Exited() {
			t.Fatalf("host %s, looked past, has gone", h.id)
		}
	}
}

// TestAStaleHostIsWaitedForNotCounted (C7 review r2, item 1): a host found
// not to serve the session while its entry still names it — a TUI-hosted
// session's engine changed, its entry's rewrite not landed yet — is looked
// past and counted once, however often it is seen: seen again with its entry
// as it was, it is waited for, within the holder allowance, until that entry
// changes — never joined again on it, never counted again — so one host
// never answers "changed hands". Played by a host serving s-9 whose entry
// the test rewrites to name s-1 (attached for s-1, it refuses
// unknown_session), rewritten three times more, each a new incarnation still
// naming s-1, as the reopen waits for it: the reopen sees it, and joins it,
// four times. Its rewrite then never landing is the holder allowance's busy,
// naming it; landing — its entry naming s-9 again — a host spawned for s-1.
// The host is never stopped, never signalled.
func TestAStaleHostIsWaitedForNotCounted(t *testing.T) {
	setVar(t, &reopenHolderWait, 500*time.Millisecond)
	for _, lands := range []bool{false, true} {
		name := "its rewrite never lands"
		if lands {
			name = "its rewrite lands"
		}
		t.Run(name, func(t *testing.T) {
			env := testEnv(t)
			ws := t.TempDir()
			watch := shortDir(t, "czw")
			holder := startHolder(t, env, "s-9", ws, "watch:"+watch, false)
			real := entryOf(t, env, holder.id)
			stale := func(n int64) func(*rundir.Entry) {
				return func(e *rundir.Entry) { e.CrazeSessionID, e.Incarnation = "s-1", fmt.Sprintf("stale-%d", n) }
			}
			if err := rewriteEntry(env, holder.id, stale(0)); err != nil {
				t.Fatal(err)
			}
			var joins, polls atomic.Int64
			var mu sync.Mutex
			var failed error
			setVar(t, &reopenJoining, func(e rundir.Entry) {
				if e.HostID == holder.id {
					joins.Add(1)
				}
			})
			setVar(t, &hostspawn.AwaitPolled, func() {
				var err error
				switch n := polls.Add(1); {
				case n <= 3:
					err = rewriteEntry(env, holder.id, stale(n))
				case n == 4 && lands:
					err = rewriteEntry(env, holder.id, func(e *rundir.Entry) { *e = real })
				}
				if err != nil {
					mu.Lock()
					failed = errors.Join(failed, err)
					mu.Unlock()
				}
			})
			_, s, sock := reopenHub(t, env, nil, newTargets(plainTarget("s-1", ws)), always("ok"))
			resp := reopenID(t, sock, "s-1")
			mu.Lock()
			if failed != nil {
				t.Fatal(failed)
			}
			mu.Unlock()
			if lands {
				res := reopened(t, resp)
				if res.Joined || res.Session.HostID == holder.id || res.Session.SessionID != "s-1" || s.count() != 1 || joins.Load() != 4 {
					t.Fatalf("answered %+v after %d joins and %d spawns; want a host of its own for s-1 after 4 joins", res.Session,
						joins.Load(), s.count())
				}
			} else {
				e := refusedAs(t, resp, protocol.CodeUnavailable, protocol.ReasonBusy)
				want := fmt.Sprintf("that session is held by host %s (pid %d), which is still moving to another session; try again", holder.id, holder.pid)
				if e.Message != want || s.count() != 0 || joins.Load() != 4 {
					t.Fatalf("answered %q after %d joins and %d spawns, want %q after 4 joins", e.Message, joins.Load(), s.count(), want)
				}
			}
			if exists(filepath.Join(watch, "stopped")) || exists(filepath.Join(watch, "signalled")) || holder.child.Exited() {
				t.Fatalf("the stale host: stopped %v, signalled %v, gone %v", exists(filepath.Join(watch, "stopped")),
					exists(filepath.Join(watch, "signalled")), holder.child.Exited())
			}
		})
	}
}

// staleHolder is a host serving a session of its own (s-9) whose registry
// entry the test has rewritten to name s-1 — a TUI-hosted session's engine
// changed, its entry's rewrite not landed — started after newerThan: attached
// for s-1, it refuses unknown_session, and a reopen looks past it. Its
// stop and its signals are watched (watch:<dir>); real is its own entry, to
// land the rewrite with.
func staleHolder(t *testing.T, env rundir.Env, ws string, newerThan time.Time) (h holderProc, real rundir.Entry, watch string) {
	t.Helper()
	watch = shortDir(t, "czw")
	h = startHolder(t, env, "s-9", ws, "watch:"+watch, false)
	real = entryOf(t, env, h.id)
	if err := rewriteEntry(env, h.id, func(e *rundir.Entry) {
		e.CrazeSessionID, e.Incarnation, e.StartedAt = "s-1", "stale", newerThan.Add(time.Minute)
	}); err != nil {
		t.Fatal(err)
	}
	return h, real, watch
}

// TestAHostLookedPastRanksLast (C7 review r3): a host the reopen has looked
// past, its entry as it was, ranks below every other host serving the
// session: with an older host J serving it, the reopen joins J — never waits
// on the stale host, whose wait would end at once on J, scan after scan, to
// the deadline. Two scans: the stale host's (joined, looked past), then J's.
func TestAHostLookedPastRanksLast(t *testing.T) {
	setVar(t, &reopenHolderWait, step)
	// A loop that spins runs to this deadline, not the test's.
	setVar(t, &reopenWait, 10*time.Second)
	env := testEnv(t)
	ws := t.TempDir()
	j := startHolder(t, env, "s-1", ws, "ok", false)
	stale, _, watch := staleHolder(t, env, ws, entryOf(t, env, j.id).StartedAt)
	var scans atomic.Int64
	setVar(t, &reopenScanned, func(rundir.Entry, bool) { scans.Add(1) })
	_, s, sock := reopenHub(t, env, nil, newTargets(plainTarget("s-1", ws)), always("ok"))
	resp := reopenID(t, sock, "s-1")
	if resp.Error != nil {
		t.Fatalf("refused %q after %d scans", resp.Error.Message, scans.Load())
	}
	res := reopened(t, resp)
	if !res.Joined || res.Session.HostID != j.id || res.Session.SessionID != "s-1" || s.count() != 0 || scans.Load() != 2 {
		t.Fatalf("answered %+v after %d scans and %d spawns; want J (%s) joined after 2 scans", res.Session, scans.Load(), s.count(), j.id)
	}
	if exists(filepath.Join(watch, "stopped")) || exists(filepath.Join(watch, "signalled")) || stale.child.Exited() {
		t.Fatal("the stale host was stopped, signalled or has gone")
	}
}

// TestAWaitNeverAnswersAHostLookedPast (C7 review r3): no wait the reopen
// makes answers an entry its scan would not join — here, a wait for a host on
// its way out (J, its entry marked stopping, its stop held) never ends at
// once on the stale host it has looked past, whose entry says it serves the
// session. The reopen joins the stale host (it alone is to join) and looks
// past it, waits for J to go — J ranks before a host looked past — then, the
// stale host alone, waits for its entry to change: its rewrite never landing
// is the holder allowance's busy, naming it; landing, a host spawned. Scans
// counted: one per turn of the loop, never a spin. J goes at the wait's
// second poll: a wait that ended at once on the stale host would scan, and
// find J, again first.
func TestAWaitNeverAnswersAHostLookedPast(t *testing.T) {
	for _, lands := range []bool{false, true} {
		name := "its rewrite never lands"
		if lands {
			name = "its rewrite lands"
		}
		t.Run(name, func(t *testing.T) {
			if lands {
				setVar(t, &reopenHolderWait, step)
			} else {
				// Spent in the stale host's wait: the wait for J, which
				// goes at its second poll, takes a poll or two of it.
				setVar(t, &reopenHolderWait, 3*time.Second)
			}
			setVar(t, &reopenWait, 10*time.Second)
			env := testEnv(t)
			ws := t.TempDir()
			stop := shortDir(t, "czs")
			j := startHolder(t, env, "s-1", ws, "ok,mark,stop:"+stop, false)
			stopHolder(t, j)
			waitFile(t, filepath.Join(stop, "stopping"))
			jEntry, listed := listedEntry(t, env, j.id)
			if !listed || !jEntry.Stopping {
				t.Fatalf("J's entry %+v, listed %v; want it listed, marked stopping", jEntry, listed)
			}
			stale, real, watch := staleHolder(t, env, ws, jEntry.StartedAt)
			var mu sync.Mutex
			var failed error
			var released, staleScanned, landed atomic.Bool
			var c *loopCounts
			c = countLoop(t, loopHooks{scanned: func(e rundir.Entry, found bool) {
				if found && e.HostID == stale.id && released.Load() {
					staleScanned.Store(true)
				}
			}, polled: func() {
				var err error
				switch {
				case c.polls.Load() == 2 && released.CompareAndSwap(false, true):
					// Waiting for J — and still waiting a poll later, the
					// stale host's entry never its answer: J goes.
					if err = os.WriteFile(filepath.Join(stop, "unlist"), nil, 0o600); err == nil {
						err = os.WriteFile(filepath.Join(stop, "exit"), nil, 0o600)
					}
				case lands && staleScanned.Load() && landed.CompareAndSwap(false, true):
					err = rewriteEntry(env, stale.id, func(e *rundir.Entry) { *e = real })
				}
				if err != nil {
					mu.Lock()
					failed = errors.Join(failed, err)
					mu.Unlock()
				}
			}})
			_, s, sock := reopenHub(t, env, nil, newTargets(plainTarget("s-1", ws)), always("ok"))
			resp := reopenID(t, sock, "s-1")
			mu.Lock()
			if failed != nil {
				t.Fatal(failed)
			}
			mu.Unlock()
			// The stale host joined, the wait for J, then the stale host's
			// wait; and, landed, a last scan finding none.
			want := int64(3)
			if lands {
				want = 4
			}
			if c.scans.Load() != want || c.joins.Load() != 1 {
				t.Fatalf("%d scans and %d joins; want %d scans, 1 join (answered %+v)", c.scans.Load(), c.joins.Load(), want, resp.Error)
			}
			// Gone as the wait saw it (its process); reaped by its own
			// watcher in its own time.
			select {
			case <-j.child.Done():
			case <-time.After(step):
				t.Fatal("J has not exited")
			}
			if lands {
				res := reopened(t, resp)
				if res.Joined || res.Session.HostID == stale.id || res.Session.HostID == j.id || s.count() != 1 {
					t.Fatalf("answered %+v after %d spawns; want a host of its own", res.Session, s.count())
				}
			} else {
				e := refusedAs(t, resp, protocol.CodeUnavailable, protocol.ReasonBusy)
				msg := fmt.Sprintf("that session is held by host %s (pid %d), which is still moving to another session; try again", stale.id, stale.pid)
				if e.Message != msg || s.count() != 0 {
					t.Fatalf("answered %q after %d spawns, want %q", e.Message, s.count(), msg)
				}
			}
			if exists(filepath.Join(watch, "stopped")) || exists(filepath.Join(watch, "signalled")) || stale.child.Exited() {
				t.Fatal("the stale host was stopped, signalled or has gone")
			}
		})
	}
}

// closingOnce is createDial, every attach to a host whose socket closing says
// is refused unavailable, reason closing, before it reaches the host — as an
// idle check's fence refuses one while the host decides whether to stay
// (control's FenceClose) — the connection closed by the attend as a real
// refusal's is.
func closingOnce(t *testing.T, closing func(socket string) bool) {
	t.Helper()
	setVar(t, &createDial, func(ctx context.Context, socket, sid string) (createdSession, error) {
		sess, err := dialCreated(ctx, socket, sid)
		if err != nil || !closing(socket) {
			return sess, err
		}
		return closingStart{sess}, nil
	})
}

// lostStart is a created session whose connection is lost before its attach
// is answered, reached saying so (reached).
type lostStart struct {
	createdSession
	reached *atomic.Bool
}

func (l lostStart) Start(context.Context) error {
	l.reached.Store(true)
	return io.ErrUnexpectedEOF
}

// closingStart is a created session whose attach is refused closing.
type closingStart struct{ createdSession }

func (closingStart) Start(context.Context) error {
	return &remote.Error{RPC: protocol.RPCRefused, Code: protocol.CodeUnavailable, Reason: protocol.ReasonClosing,
		Message: "the session is closing, or its host is deciding whether to: attach again once it has ended or stayed"}
}

// TestAClosingRefusalIsTriedAgain (C7 review r4, item 2): an attach refused
// closing while the host's entry still serves the session, unmarked, is not
// a stop for certain — an idle check's fence refuses attaches for a moment
// and lifts when the host stays — so the host is on its way out tentatively:
// passed over until the reopen has waited a poll, then joined again. The
// review's schedule: a newer host H serving s-1 refuses one attach closing,
// then takes them; an older host J's entry names s-1 though it serves
// another session (its rewrite not landed). The reopen joins H (refused),
// joins J (unknown_session: looked past), waits a poll, joins H again: the
// answer, joined, H's — never busy for want of H.
func TestAClosingRefusalIsTriedAgain(t *testing.T) {
	setVar(t, &reopenHolderWait, 2*time.Second)
	setVar(t, &reopenWait, 10*time.Second)
	env := testEnv(t)
	ws := t.TempDir()
	h := startHolder(t, env, "s-1", ws, "ok", false)
	j, _, watch := staleHolder(t, env, ws, entryOf(t, env, h.id).StartedAt.Add(-2*time.Minute))
	var refusedOnce atomic.Bool
	closingOnce(t, func(socket string) bool { return socket == h.socket && refusedOnce.CompareAndSwap(false, true) })
	// The waits' time runs on their polls: H's mark's poll passes at a
	// wait's second poll, never at its first, however late it comes.
	c := countLoop(t, loopHooks{tick: rendezvousPoll})
	_, s, sock := reopenHub(t, env, nil, newTargets(plainTarget("s-1", ws)), always("ok"))
	resp := reopenID(t, sock, "s-1")
	if resp.Error != nil {
		t.Fatalf("refused %q after %d scans and %d joins", resp.Error.Message, c.scans.Load(), c.joins.Load())
	}
	res := reopened(t, resp)
	// H refused, J looked past, H joined: four scans, three joins, the last
	// after a poll's sleep — the one the wait slept.
	if !res.Joined || res.Session.HostID != h.id || res.Session.SessionID != "s-1" || s.count() != 0 ||
		c.scans.Load() != 4 || c.joins.Load() != 3 || c.slept.Load() != 1 {
		t.Fatalf("answered %+v after %d scans, %d joins, %d slept polls and %d spawns; want H (%s) joined after 4 scans and 3 joins",
			res.Session, c.scans.Load(), c.joins.Load(), c.slept.Load(), s.count(), h.id)
	}
	if exists(filepath.Join(watch, "stopped")) || exists(filepath.Join(watch, "signalled")) || j.child.Exited() {
		t.Fatal("J, looked past, was stopped, signalled or has gone")
	}
}

// TestAPassingFailureAfterClosingIsTriedAgain (C7 soak fix review, item 1):
// a host that refused closing — an idle check's fence — and whose join again
// then fails once in passing (its dial, or its connection lost before its
// attach is answered) while its entry still serves the session, unmarked, is
// on its way out tentatively again, never for good: a poll later it is joined
// again, and, having stayed, it is the answer, joined — never the holder
// allowance's busy saying it has not finished stopping. Three scans, three
// joins, a slept poll before each join again.
func TestAPassingFailureAfterClosingIsTriedAgain(t *testing.T) {
	setVar(t, &reopenHolderWait, 500*time.Millisecond)
	setVar(t, &reopenWait, 10*time.Second)
	for _, lost := range []bool{false, true} {
		name := "its dial fails once"
		if lost {
			name = "its connection is lost once"
		}
		t.Run(name, func(t *testing.T) {
			env := testEnv(t)
			ws := t.TempDir()
			h := startHolder(t, env, "s-1", ws, "ok", false)
			var dials atomic.Int64
			var reached atomic.Bool
			setVar(t, &createDial, func(ctx context.Context, socket, sid string) (createdSession, error) {
				if socket != h.socket {
					return dialCreated(ctx, socket, sid)
				}
				n := dials.Add(1)
				if n == 2 && !lost {
					return nil, fmt.Errorf("dial unix %s: hello: i/o timeout", socket)
				}
				sess, err := dialCreated(ctx, socket, sid)
				switch {
				case err != nil:
					return nil, err
				case n == 1:
					return closingStart{sess}, nil
				case n == 2:
					return lostStart{sess, &reached}, nil
				}
				return sess, nil
			})
			c := countLoop(t, loopHooks{tick: rendezvousPoll})
			_, s, sock := reopenHub(t, env, nil, newTargets(plainTarget("s-1", ws)), always("ok"))
			resp := reopenID(t, sock, "s-1")
			if resp.Error != nil {
				t.Fatalf("refused %q after %d scans and %d joins", resp.Error.Message, c.scans.Load(), c.joins.Load())
			}
			res := reopened(t, resp)
			if !res.Joined || res.Session.HostID != h.id || res.Session.SessionID != "s-1" || s.count() != 0 || dials.Load() != 3 ||
				c.scans.Load() != 3 || c.joins.Load() != 3 || c.slept.Load() != 2 || lost != reached.Load() {
				t.Fatalf("answered %+v after %d dials, %d scans, %d joins, %d slept polls, %d spawns (the lost connection reached %v); want H (%s) joined after 3 of each, 2 slept polls",
					res.Session, dials.Load(), c.scans.Load(), c.joins.Load(), c.slept.Load(), s.count(), reached.Load(), h.id)
			}
		})
	}
}

// TestHostsAlwaysClosingTakeTurnsOnlyAPollApart (C7 review r4, item 2): two
// hosts whose attaches are refused closing every time — each on its way out
// tentatively, each tried again once the reopen has waited a poll — are
// joined in turns, never hot: while both are held leaving the wait sleeps a
// poll, charged to the holder allowance, so the allowance ends it, busy,
// naming the host it was waiting for: a slept poll for every two scans, the
// allowance's worth of them at most.
func TestHostsAlwaysClosingTakeTurnsOnlyAPollApart(t *testing.T) {
	setVar(t, &reopenHolderWait, 600*time.Millisecond)
	// A loop that spins runs to this deadline, not the test's.
	setVar(t, &reopenWait, 10*time.Second)
	env := testEnv(t)
	ws := t.TempDir()
	h1 := startHolder(t, env, "s-1", ws, "ok", false)
	h2, _, _ := staleHolder(t, env, ws, entryOf(t, env, h1.id).StartedAt)
	closingOnce(t, func(socket string) bool { return socket == h1.socket || socket == h2.socket })
	// The waits' time runs on their polls: each slept poll is a
	// rendezvousPoll of the allowance, whatever the scheduling.
	c := countLoop(t, loopHooks{tick: rendezvousPoll})
	_, s, sock := reopenHub(t, env, nil, newTargets(plainTarget("s-1", ws)), always("ok"))
	e := refusedAs(t, reopenID(t, sock, "s-1"), protocol.CodeUnavailable, protocol.ReasonBusy)
	named := false
	for _, h := range []holderProc{h1, h2} {
		named = named || e.Message == fmt.Sprintf("that session is held by host %s (pid %d), which has not finished stopping; try again", h.id, h.pid)
	}
	// Two scans a turn: the first's wait ends at once on the other host,
	// whose poll is up; the second's sleeps a poll, both held leaving, and
	// is charged it. So at most the allowance's worth of slept polls (fewer
	// when a late poll finds a wait's deadline, which is the wall clock's,
	// passed), and never two scans without one.
	turns := int64(reopenHolderWait / rendezvousPoll)
	if !named || s.count() != 0 || c.slept.Load() > turns || 2*(c.slept.Load()+1) < c.scans.Load() {
		t.Fatalf("answered %q after %d scans, %d joins, %d polls (%d slept) and %d spawns; want the holder allowance's busy, a slept poll for every two scans",
			e.Message, c.scans.Load(), c.joins.Load(), c.polls.Load(), c.slept.Load(), s.count())
	}
}

// TestASpentAllowanceEndsTheRetries (C7 review r5, item 3): the holder
// allowance spent is the end — busy, naming the holder — even for a host
// tried again each time its tentative mark's poll has passed. Played by a
// host whose every attach is refused closing, every poll of the reopen's
// waits slower than the mark's poll (25 ms of the waits' clock, which
// loopHooks.tickEvery runs on polls): each wait's first poll finds the mark
// passed and answers the host itself, before any deadline is read, and each
// such wait takes 25 ms of the allowance. Twenty turns spend the 500 ms; the
// twenty-first join's wait is never made: busy, long before the reopen's
// deadline.
func TestASpentAllowanceEndsTheRetries(t *testing.T) {
	setVar(t, &reopenHolderWait, 500*time.Millisecond)
	// A loop past the allowance runs to this deadline, not the test's.
	setVar(t, &reopenWait, 10*time.Second)
	env := testEnv(t)
	ws := t.TempDir()
	h := startHolder(t, env, "s-1", ws, "ok", false)
	closingOnce(t, func(socket string) bool { return socket == h.socket })
	c := countLoop(t, loopHooks{tick: 25 * time.Millisecond, tickEvery: true})
	_, s, sock := reopenHub(t, env, nil, newTargets(plainTarget("s-1", ws)), always("ok"))
	e := refusedAs(t, reopenID(t, sock, "s-1"), protocol.CodeUnavailable, protocol.ReasonBusy)
	want := fmt.Sprintf("that session is held by host %s (pid %d), which has not finished stopping; try again", h.id, h.pid)
	if e.Message != want || s.count() != 0 || c.scans.Load() != 21 || c.joins.Load() != 21 || c.polls.Load() != 20 {
		t.Fatalf("answered %q after %d scans, %d joins, %d polls and %d spawns; want %q after 21 scans, 21 joins, 20 polls",
			e.Message, c.scans.Load(), c.joins.Load(), c.polls.Load(), s.count(), want)
	}
}

// TestAReopensOwnHostServingAnotherSessionIsStopped (C7 review r2, item 1):
// a host the reopen spawned whose row, read once its start is done, names
// another session than the one it was spawned to load is still the reopen's
// own: stopped, as a create stops its own (abandonHost), before the reopen
// looks past it — never let go of running — and the reopen goes on to a host
// that serves the session. Played by the spawned host's first row read naming
// s-2; its stop is seen by its stop hook (stop:<dir>, its gates open).
func TestAReopensOwnHostServingAnotherSessionIsStopped(t *testing.T) {
	env := testEnv(t)
	ws := t.TempDir()
	stop := shortDir(t, "czs")
	writeFile(t, filepath.Join(stop, "unlist"))
	writeFile(t, filepath.Join(stop, "exit"))
	var reads atomic.Int64
	setVar(t, &createRowRead, func(ctx context.Context, socket, sid string) (json.RawMessage, string, time.Time, error) {
		raw, v, at, err := freshRow(ctx, socket, sid)
		if reads.Add(1) == 1 && err == nil {
			raw = json.RawMessage(strings.Replace(string(raw), `"sessionId":"s-1"`, `"sessionId":"s-2"`, 1))
		}
		return raw, v, at, err
	})
	_, s, sock := reopenHub(t, env, nil, newTargets(plainTarget("s-1", ws)), func(n int) string {
		if n == 0 {
			return "ok,stop:" + stop
		}
		return "ok"
	})
	res := reopened(t, reopenID(t, sock, "s-1"))
	if s.count() != 2 {
		t.Fatalf("answered %+v after %d spawns, want 2", res.Session, s.count())
	}
	first, second := spawnedID(s, 0), spawnedID(s, 1)
	if res.Joined || res.Session.HostID != second || res.Session.SessionID != "s-1" || rowSession(t, res.Session) != "s-1" {
		t.Fatalf("answered %+v (row %s); want the second spawn, %s, serving s-1 (the first was %s)", res.Session, res.Session.Row,
			second, first)
	}
	if !exists(filepath.Join(stop, "stopping")) {
		t.Fatalf("the first spawn, %s, serving another session, was never stopped", first)
	}
	s.gone(t, env, 0)
}

// TestAReopenKeepsToItsDeadline (C7 review, item 5a): reopenWait bounds the
// whole reopen. A join whose dial outlasts what is left of it is spent —
// busy, never a start then given the whole start budget again — the joined
// host left alone; and a spawn whose ready line does not come by it (the
// deadline moved to pass as the spawn launches: reopenSpawning) is spent, its
// host ended — never waited for to hostspawn.ReadyWait's end.
func TestAReopenKeepsToItsDeadline(t *testing.T) {
	setVar(t, &reopenStartWait, step)
	spentMsg := "that session was not running within the reopen's 300ms; try again"
	t.Run("a join's dial", func(t *testing.T) {
		env := testEnv(t)
		ws := t.TempDir()
		gate, watch := gateDir(t), shortDir(t, "czw")
		holder := startHolder(t, env, "s-1", ws, "gate:"+gate+",watch:"+watch, false)
		var starts atomic.Int64
		setVar(t, &createDial, func(ctx context.Context, socket, sid string) (createdSession, error) {
			time.Sleep(400 * time.Millisecond)
			sess, err := dialCreated(ctx, socket, sid)
			if err != nil {
				return nil, err
			}
			return countedStart{sess, &starts}, nil
		})
		_, s, sock := reopenHub(t, env, nil, newTargets(plainTarget("s-1", ws)), always("ok"))
		setVar(t, &reopenWait, 300*time.Millisecond)
		e := refusedAs(t, reopenID(t, sock, "s-1"), protocol.CodeUnavailable, protocol.ReasonBusy)
		// No start is waited for once the deadline has passed in the dial.
		if e.Message != spentMsg || s.count() != 0 || starts.Load() != 0 || exists(filepath.Join(watch, "stopped")) ||
			exists(filepath.Join(watch, "signalled")) || holder.child.Exited() {
			t.Fatalf("answered %q after %d spawns and %d starts; the joined host stopped %v, signalled %v, gone %v", e.Message, s.count(),
				starts.Load(), exists(filepath.Join(watch, "stopped")), exists(filepath.Join(watch, "signalled")), holder.child.Exited())
		}
	})
	t.Run("a spawn's ready line", func(t *testing.T) {
		env := testEnv(t)
		_, s, sock := reopenHub(t, env, nil, newTargets(plainTarget("s-1", t.TempDir())), always("hang"))
		// The deadline is the whole budget (reopenWait's own, which no
		// scheduling before the spawn outlasts) until the spawn is about to
		// read its ready line, and passes then: forced, not timed.
		var spawning atomic.Int64
		setVar(t, &reopenSpawning, func(d *time.Time) {
			spawning.Add(1)
			*d = time.Now()
		})
		e := refusedAs(t, reopenID(t, sock, "s-1"), protocol.CodeUnavailable, protocol.ReasonBusy)
		if want := fmt.Sprintf("that session was not running within the reopen's %v; try again", reopenWait); e.Message != want ||
			spawning.Load() != 1 || s.count() != 1 {
			t.Fatalf("answered %q after %d spawns (%d at the seam)", e.Message, s.count(), spawning.Load())
		}
		s.gone(t, env, 0)
	})
}

// TestTheHolderAllowanceIsSpentOnlyWaiting (C7 review, item 5b): the holder
// allowance is the time spent waiting for holders, not the time since the
// reopen began: a join that took longer than the whole allowance before its
// attach was refused closing (its time played by the allowance's clock jumping
// past it) still waits for that holder, and answers a new host once it goes.
func TestTheHolderAllowanceIsSpentOnlyWaiting(t *testing.T) {
	setVar(t, &reopenHolderWait, step)
	var offset atomic.Int64
	setVar(t, &reopenClock, func() time.Time { return time.Now().Add(time.Duration(offset.Load())) })
	env := testEnv(t)
	ws := t.TempDir()
	stop := shortDir(t, "czs")
	holder := startHolder(t, env, "s-1", ws, "ok,mark,stop:"+stop, false)
	stopErr := make(chan error, 1)
	var once sync.Once
	setVar(t, &reopenJoining, func(e rundir.Entry) {
		once.Do(func() {
			offset.Add(int64(2 * step))
			stopErr <- stopCreated(e.Socket, e.CrazeSessionID)
			awaitFile(filepath.Join(stop, "stopping"))
		})
	})
	_, s, sock := reopenHub(t, env, nil, newTargets(plainTarget("s-1", ws)), always("ok"))
	aw := awaitingPolls(t)
	ch := reopenAsync(sock, "s-1")
	aw.waiting(t)
	if err := <-stopErr; err != nil {
		t.Fatal(err)
	}
	pending(t, ch, "while the holder stops")
	writeFile(t, filepath.Join(stop, "unlist"))
	writeFile(t, filepath.Join(stop, "exit"))
	res := reopened(t, outcome(t, ch))
	if res.Joined || res.Session.HostID == holder.id || s.count() != 1 {
		t.Fatalf("the reopen answered %+v after %d spawns; the closing holder was %s", res, s.count(), holder.id)
	}
}

// countedStart is a created session whose starts are counted.
type countedStart struct {
	createdSession
	n *atomic.Int64
}

func (c countedStart) Start(ctx context.Context) error {
	c.n.Add(1)
	return c.createdSession.Start(ctx)
}
