package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/charliek/craze/internal/backend"
	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/fakehost"
	"github.com/charliek/craze/internal/hostspawn"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/remote"
	"github.com/charliek/craze/internal/rundir"
)

// session.create (plan 032 §3.10, §3.18): the hub in this process, every
// created host a real child (create_helpers_test.go), every schedule forced —
// a gated start is held at its gate until the test opens it — and each bound
// that is on trial shortened for its own test only.

// TestCreateStartsASession (A12): a create spawns its host — craze serve's
// command line with the session's directory, provider, settings and request,
// --no-host-status, the environment contract's environment, its working
// directory the session's — waits for its start, sends its first prompt, and
// answers the session as the roster would list it read now: its host listed
// with the request, ready, reachable, not approximate, its own row. The host
// runs on after the hub has let it go. Without a prompt the answer says none.
func TestCreateStartsASession(t *testing.T) {
	env := testEnv(t)
	t.Setenv("CRAZE_PROVIDER", "grok")
	t.Setenv("TMUX", "/tmp/tmux-1/default,1,0")
	t.Setenv("CRAZE_AGENT_BIN", "/bin/false")
	_, s, sock := creating(t, env, nil, always("ok"))
	c := dial(t, sock)
	if res := c.hello(t); !res.Capabilities.SessionCreate {
		t.Fatalf("a hub that creates says sessionCreate false: %+v", res.Capabilities)
	}
	work := t.TempDir()
	fast := true
	res := result(t, createOn(t, c, protocol.CreateParams{Cwd: work + "/./", Prompt: "hello there", Model: "m-1",
		Effort: "high", Fast: &fast, PermissionMode: protocol.PermissionPrompt, RequestID: "req-1"}))
	if res.Prompt != protocol.CreatePromptAccepted || res.PromptError != "" {
		t.Fatalf("the prompt came to %q (%q), want accepted", res.Prompt, res.PromptError)
	}
	row := res.Session
	e, listed := listedEntry(t, env, row.HostID)
	switch {
	case !listed:
		t.Fatalf("the created host %s is not listed", row.HostID)
	case e.RequestID != "req-1" || !strings.HasPrefix(e.RequestHash, "sha256:"):
		t.Fatalf("the created host's entry carries request %q %q", e.RequestID, e.RequestHash)
	case row.SessionID != e.CrazeSessionID || row.Status != protocol.RosterReachable || row.Approximate || !row.Host.Ready ||
		row.Host.PID != e.PID || row.Host.Workspace != work || row.Host.CrazeVersion != "0.0.0-fakehost":
		t.Fatalf("the created session's row %+v, its entry %+v", row, e)
	}
	srow, ok, err := row.SessionRow()
	if !ok || err != nil || srow.SessionID != e.CrazeSessionID {
		t.Fatalf("the created session's own row: %+v, %v, %v", srow, ok, err)
	}
	cmd, argv := s.cmd(0)
	for _, want := range []string{"serve", "--host-id=" + row.HostID, "--workspace=" + work, "--provider=cursor",
		"--model=m-1", "--effort=high", "--fast", "--no-force", "--no-host-status", "--request-id=req-1",
		"--request-hash=" + e.RequestHash} {
		if !slices.Contains(argv, want) {
			t.Errorf("the host's command line %q lacks %s", argv, want)
		}
	}
	if slices.ContainsFunc(argv, func(a string) bool { return strings.HasPrefix(a, "--agent-bin") }) {
		t.Errorf("the host's command line %q names an agent binary", argv)
	}
	if cmd.Dir != work {
		t.Errorf("the host runs in %q, want %q", cmd.Dir, work)
	}
	for _, kv := range cmd.Env {
		k, _, _ := strings.Cut(kv, "=")
		if k == "CRAZE_PROVIDER" || k == "TMUX" || k == "CRAZE_AGENT_BIN" {
			t.Errorf("the host's environment carries %s (the contract removes it)", kv)
		}
	}
	if !slices.Contains(cmd.Env, "CRAZE_HOME="+env.CrazeDir) {
		t.Errorf("the host's environment has no absolute CRAZE_HOME %s", env.CrazeDir)
	}
	if !alive(cmd.Process.Pid) {
		t.Fatal("the created host did not run on")
	}

	// No prompt: none; a second host.
	res = result(t, createOn(t, c, map[string]any{"cwd": work}))
	if res.Prompt != protocol.CreatePromptNone || res.Session.HostID == row.HostID || s.count() != 2 {
		t.Fatalf("a create with no prompt answered %+v after %d spawns", res, s.count())
	}
	_, argv = s.cmd(1)
	for _, a := range argv {
		if strings.HasPrefix(a, "--request-") || a == "--no-force" || strings.HasPrefix(a, "--model") {
			t.Errorf("a create with no request, mode or model passes %s", a)
		}
	}
}

// TestCreateRefusesBadParams: every params the schema or the hub refuses is
// bad_request — unknown_field for a member it does not define — and spawns
// nothing; a hub with no default provider refuses a create that names none.
// The negative control: a good create on the same hub is answered.
func TestCreateRefusesBadParams(t *testing.T) {
	env := testEnv(t)
	s := hostsAsChildren(t, env, always("ok"))
	rn := runWith(t, env, nil, creates(""))
	sock := rn.line(t).Socket
	c := dial(t, sock)
	c.hello(t)
	work := t.TempDir()
	file := filepath.Join(work, "file")
	writeFile(t, file)
	for _, tc := range []struct {
		name   string
		params any
		reason protocol.Reason
	}{
		{"no params", nil, protocol.ReasonBadRequest},
		{"null", json.RawMessage("null"), protocol.ReasonBadRequest},
		{"not an object", []int{1}, protocol.ReasonBadRequest},
		{"an unknown member", map[string]any{"cwd": work, "provider": "cursor", "agentBin": "/bin/sh"}, protocol.ReasonUnknownField},
		{"a session id", map[string]any{"cwd": work, "provider": "cursor", "sessionId": "s"}, protocol.ReasonUnknownField},
		{"no cwd", map[string]any{"provider": "cursor"}, protocol.ReasonBadRequest},
		{"a relative cwd", map[string]any{"cwd": "work", "provider": "cursor"}, protocol.ReasonBadRequest},
		{"a cwd that does not exist", map[string]any{"cwd": work + "/nope", "provider": "cursor"}, protocol.ReasonBadRequest},
		{"a cwd that is a file", map[string]any{"cwd": file, "provider": "cursor"}, protocol.ReasonBadRequest},
		{"a cwd of a number", map[string]any{"cwd": 7, "provider": "cursor"}, protocol.ReasonBadRequest},
		{"an empty prompt", map[string]any{"cwd": work, "provider": "cursor", "prompt": ""}, protocol.ReasonBadRequest},
		{"a blank prompt", map[string]any{"cwd": work, "provider": "cursor", "prompt": " \n"}, protocol.ReasonBadRequest},
		{"an unknown provider", map[string]any{"cwd": work, "provider": "claude"}, protocol.ReasonBadRequest},
		{"no provider and no default", map[string]any{"cwd": work}, protocol.ReasonBadRequest},
		{"a null model", map[string]any{"cwd": work, "provider": "cursor", "model": nil}, protocol.ReasonBadRequest},
		{"a model with a NUL", map[string]any{"cwd": work, "provider": "cursor", "model": "a\x00b"}, protocol.ReasonBadRequest},
		{"an effort too long", map[string]any{"cwd": work, "provider": "cursor", "effort": strings.Repeat("e", 65)}, protocol.ReasonBadRequest},
		{"fast of a string", map[string]any{"cwd": work, "provider": "cursor", "fast": "yes"}, protocol.ReasonBadRequest},
		{"a permission mode there is none of", map[string]any{"cwd": work, "provider": "cursor", "permissionMode": "ask"}, protocol.ReasonBadRequest},
		{"a request id with a space", map[string]any{"cwd": work, "provider": "cursor", "requestId": "a b"}, protocol.ReasonBadRequest},
		{"a request id too long", map[string]any{"cwd": work, "provider": "cursor", "requestId": strings.Repeat("r", 65)}, protocol.ReasonBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			refusedAs(t, createOn(t, c, tc.params), protocol.CodeBadRequest, tc.reason)
		})
	}
	if n := s.count(); n != 0 {
		t.Fatalf("refused creates spawned %d hosts", n)
	}
	res := result(t, createOn(t, c, map[string]any{"cwd": work, "provider": "grok"}))
	if res.Session.HostID == "" || s.count() != 1 {
		t.Fatalf("the good create answered %+v after %d spawns", res, s.count())
	}
}

// TestCreateStartFailureLeavesNoHost (A12): a session whose start fails is
// not_accepting, reason start_failed, data.cause the host's first error line;
// the hub stops that host — session.stop, or, where it refuses that, its
// termination — so it has exited and left the registry by the time the
// answer comes. The negative control is TestCreateStartsASession's host,
// which runs on.
func TestCreateStartFailureLeavesNoHost(t *testing.T) {
	for _, mode := range []string{"fail", "fail,nostop"} {
		t.Run(mode, func(t *testing.T) {
			env := testEnv(t)
			_, s, sock := creating(t, env, nil, always(mode))
			c := dial(t, sock)
			c.hello(t)
			e := refusedAs(t, createOn(t, c, map[string]any{"cwd": t.TempDir(), "prompt": "never sent"}),
				protocol.CodeNotAccepting, protocol.ReasonStartFailed)
			if e.Data.Cause != failCause || e.Message != "the session did not start: "+failCause {
				t.Fatalf("the start failure says %q, cause %q; want the cause %q", e.Message, e.Data.Cause, failCause)
			}
			cmd, argv := s.cmd(0)
			if _, listed := listedEntry(t, env, fakehost.ParseSpawnArgs(argv).HostID); listed || alive(cmd.Process.Pid) {
				t.Fatalf("the failed session's host is still there (listed %v, alive %v)", listed, alive(cmd.Process.Pid))
			}
		})
	}
}

// TestCreateSpawnFailures: a host that exits before its ready line, or
// answers one that is not ok, is unavailable, reason spawn_failed — the host
// reaped, the message its own words and no path; a hub in a test binary with
// no HostCommand spawns nothing and says so.
func TestCreateSpawnFailures(t *testing.T) {
	setVar(t, &hostspawn.TermGrace, step)
	for _, tc := range []struct{ mode, want string }{
		{"exit", "the session host exited before it was ready (exit status 3)"},
		{"notok", "the session host could not start: no such provider here"},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			env := testEnv(t)
			_, s, sock := creating(t, env, nil, always(tc.mode))
			c := dial(t, sock)
			c.hello(t)
			e := refusedAs(t, createOn(t, c, map[string]any{"cwd": t.TempDir()}), protocol.CodeUnavailable, protocol.ReasonSpawnFailed)
			if e.Message != tc.want {
				t.Fatalf("spawn_failed says %q, want %q", e.Message, tc.want)
			}
			if cmd, _ := s.cmd(0); alive(cmd.Process.Pid) {
				t.Fatal("the host that failed to spawn is still running")
			}
		})
	}
	t.Run("no host command", func(t *testing.T) {
		env := testEnv(t)
		rn := runWith(t, env, nil, creates("cursor"))
		c := dial(t, rn.line(t).Socket)
		c.hello(t)
		e := refusedAs(t, createOn(t, c, map[string]any{"cwd": t.TempDir()}), protocol.CodeUnavailable, protocol.ReasonSpawnFailed)
		if !strings.Contains(e.Message, "no session host in a test binary") {
			t.Fatalf("spawn_failed says %q", e.Message)
		}
	})
}

// TestCreateStartOverItsBound (§3.10): a start that has not ended when the
// bound (shortened here) passes is not_accepting, reason start_failed, and
// its host is stopped; the negative control — the same gated start let go
// before the bound — is answered started.
func TestCreateStartOverItsBound(t *testing.T) {
	setVar(t, &createStartWait, 300*time.Millisecond)
	env := testEnv(t)
	gate := gateDir(t)
	_, s, sock := creating(t, env, nil, always("gate:"+gate))
	c := dial(t, sock)
	c.hello(t)
	e := refusedAs(t, createOn(t, c, map[string]any{"cwd": t.TempDir()}), protocol.CodeNotAccepting, protocol.ReasonStartFailed)
	if want := "the session did not start within 300ms"; e.Data.Cause != want {
		t.Fatalf("the slow start's cause is %q, want %q", e.Data.Cause, want)
	}
	s.gone(t, env, 0)

	setVar(t, &createStartWait, step)
	gate2 := gateDir(t)
	_, _, sock2 := creating(t, testEnv(t), nil, always("gate:"+gate2))
	c2 := dial(t, sock2)
	c2.hello(t)
	c2.sendCreate(t, map[string]any{"cwd": t.TempDir()})
	waitGate(t, gate2)
	openGate(t, gate2, "ok")
	if res := result(t, c2.readAnswer(t)); res.Session.HostID == "" {
		t.Fatalf("the start let go in time answered %+v", res)
	}
}

// TestCreateRequestIDs (§3.10's idempotency): a repeat with the same params
// answers the first create's result, byte for byte, with no second spawn —
// done, or joined while it runs, from another connection, after the first
// waiter has gone (which did not cancel it); the same id with other params is
// bad_request, reason request_conflict, in flight or done. The negative
// control: a create with no id spawns again.
func TestCreateRequestIDs(t *testing.T) {
	env := testEnv(t)
	gate := gateDir(t)
	_, s, sock := creating(t, env, nil, func(n int) string {
		if n == 0 {
			return "gate:" + gate
		}
		return "ok"
	})
	work := t.TempDir()
	p := map[string]any{"cwd": work, "prompt": "one", "requestId": "r-1"}
	first := dial(t, sock)
	first.hello(t)
	first.sendCreate(t, p)
	waitGate(t, gate)
	// The first waiter goes: the create runs on.
	_ = first.nc.Close()
	other := dial(t, sock)
	other.hello(t)
	refusedAs(t, createOn(t, other, map[string]any{"cwd": work, "prompt": "two", "requestId": "r-1"}),
		protocol.CodeBadRequest, protocol.ReasonRequestConflict)
	joiner := dial(t, sock)
	joiner.hello(t)
	joiner.sendCreate(t, p)
	openGate(t, gate, "ok")
	joined := joiner.readAnswer(t)
	res := result(t, joined)
	if res.Prompt != protocol.CreatePromptAccepted || s.count() != 1 {
		t.Fatalf("the joined create answered %+v after %d spawns", res, s.count())
	}
	again := createOn(t, other, p)
	if string(again.Result) != string(joined.Result) || s.count() != 1 {
		t.Fatalf("the repeat answered %s, the first %s, after %d spawns", again.Result, joined.Result, s.count())
	}
	refusedAs(t, createOn(t, other, map[string]any{"cwd": work, "requestId": "r-1"}),
		protocol.CodeBadRequest, protocol.ReasonRequestConflict)
	res2 := result(t, createOn(t, other, map[string]any{"cwd": work, "prompt": "one"}))
	if res2.Session.HostID == res.Session.HostID || s.count() != 2 {
		t.Fatalf("a create with no id answered %+v after %d spawns", res2, s.count())
	}
}

// TestCreateFailureIsKept: a create's failure is its answer for its id too —
// a repeat is answered the same start_failed, nothing spawned again.
func TestCreateFailureIsKept(t *testing.T) {
	env := testEnv(t)
	_, s, sock := creating(t, env, nil, always("fail"))
	c := dial(t, sock)
	c.hello(t)
	p := map[string]any{"cwd": t.TempDir(), "requestId": "r-f"}
	a := refusedAs(t, createOn(t, c, p), protocol.CodeNotAccepting, protocol.ReasonStartFailed)
	b := refusedAs(t, createOn(t, c, p), protocol.CodeNotAccepting, protocol.ReasonStartFailed)
	if a.Message != b.Message || a.Data.Cause != b.Data.Cause || s.count() != 1 {
		t.Fatalf("the repeat answered %+v, the first %+v, after %d spawns", b, a, s.count())
	}
}

// TestCreateTableEviction: a done create's answer is kept for createKeep,
// and at most createKeepMax of them, the oldest forgotten first; one in
// flight is never forgotten.
func TestCreateTableEviction(t *testing.T) {
	setVar(t, &createKeepMax, 2)
	cr := newCreator(&hub{}, *creates("cursor"))
	now := time.Now()
	done := func(id string, at time.Time) {
		cr.calls[id] = &createCall{id: id, done: make(chan struct{}), at: at, finished: true}
	}
	cr.calls["flight"] = &createCall{id: "flight", done: make(chan struct{})}
	done("old", now.Add(-createKeep-time.Second))
	done("a", now.Add(-3*time.Second))
	done("b", now.Add(-2*time.Second))
	done("c", now.Add(-time.Second))
	cr.evictLocked(now)
	var ids []string
	for id := range cr.calls {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	if want := []string{"b", "c", "flight"}; !slices.Equal(ids, want) {
		t.Fatalf("kept %v, want %v", ids, want)
	}
}

// TestCreateReplayAfterARestart (§3.10, R2-3, R3-2): a hub with no memory of
// a requestId — this one restarted — joins the live host a create of that id
// started, from its registry entry: a start still running is waited for and
// its failure answered (and that host stopped), or its success (its prompt
// unknown); a session that started long before the bound answers at once;
// other params are request_conflict. The first hub is torn down mid-create
// (its creates cut), which leaves a gated host waiting at its gate.
func TestCreateReplayAfterARestart(t *testing.T) {
	setVar(t, &teardownBound, 500*time.Millisecond)
	for _, end := range []string{"fail", "ok"} {
		t.Run("a start still running that ends "+end, func(t *testing.T) {
			env := testEnv(t)
			gate := gateDir(t)
			s := hostsAsChildren(t, env, always("gate:"+gate))
			first := runWith(t, env, nil, creates("cursor"))
			c := dial(t, first.line(t).Socket)
			c.hello(t)
			work := t.TempDir()
			p := map[string]any{"cwd": work, "prompt": "hi", "requestId": "r-restart"}
			c.sendCreate(t, p)
			id := waitGate(t, gate)
			first.sigs <- syscall.SIGTERM
			if err := first.stopped(t); err != nil {
				t.Fatal(err)
			}
			if e, listed := listedEntry(t, env, id); !listed || e.RequestID != "r-restart" {
				t.Fatalf("the gated host is not listed with its request: %+v, %v", e, listed)
			}
			second := runWith(t, env, nil, creates("cursor"))
			c2 := dial(t, second.line(t).Socket)
			c2.hello(t)
			refusedAs(t, createOn(t, c2, map[string]any{"cwd": work, "requestId": "r-restart"}),
				protocol.CodeBadRequest, protocol.ReasonRequestConflict)
			c2.sendCreate(t, p)
			openGate(t, gate, end)
			resp := c2.readAnswer(t)
			if end == "fail" {
				e := refusedAs(t, resp, protocol.CodeNotAccepting, protocol.ReasonStartFailed)
				if e.Data.Cause != failCause {
					t.Fatalf("the joined failure's cause is %q", e.Data.Cause)
				}
				s.gone(t, env, 0)
			} else {
				res := result(t, resp)
				if res.Session.HostID != id || res.Prompt != protocol.CreatePromptUnknown || res.Session.Approximate {
					t.Fatalf("the joined start answered %+v, want host %s, prompt unknown", res, id)
				}
			}
			if s.count() != 1 {
				t.Fatalf("the join spawned again: %d spawns", s.count())
			}
		})
	}
	t.Run("a session that started long before the bound", func(t *testing.T) {
		// The fake host's registry entry says it started at its pinned clock's
		// start — months before this join — so a join that counted its 60 s
		// from the session's start, not from the join, would have timed out
		// already: no wait, no shortened bound, is needed to tell them apart.
		env := testEnv(t)
		s := hostsAsChildren(t, env, always("ok"))
		first := runWith(t, env, nil, creates("cursor"))
		c := dial(t, first.line(t).Socket)
		c.hello(t)
		p := map[string]any{"cwd": t.TempDir(), "prompt": "hi", "requestId": "r-old"}
		res := result(t, createOn(t, c, p))
		first.sigs <- syscall.SIGTERM
		if err := first.stopped(t); err != nil {
			t.Fatal(err)
		}
		if e, _ := listedEntry(t, env, res.Session.HostID); time.Since(e.StartedAt) < 2*createStartWait {
			t.Fatalf("the session's entry says it started at %v: not long before the bound", e.StartedAt)
		}
		second := runWith(t, env, nil, creates("cursor"))
		c2 := dial(t, second.line(t).Socket)
		c2.hello(t)
		again := result(t, createOn(t, c2, p))
		if again.Session.HostID != res.Session.HostID || again.Session.SessionID != res.Session.SessionID ||
			again.Prompt != protocol.CreatePromptUnknown || s.count() != 1 {
			t.Fatalf("the replay answered %+v (first %+v) after %d spawns", again, res, s.count())
		}
	})
}

// TestCreateTeardownMidCreate (§3.10): a create still in flight when its hub
// tears down gets the teardown's bound; past it the waiter's connection
// closes unanswered, and the host — left at its gate — runs on, its request
// in its entry, and starts once let go. The negative control: a create that
// ends within the bound is answered before the hub goes.
func TestCreateTeardownMidCreate(t *testing.T) {
	setVar(t, &teardownBound, 500*time.Millisecond)
	env := testEnv(t)
	gate := gateDir(t)
	rn, s, sock := creating(t, env, nil, always("gate:"+gate))
	c := dial(t, sock)
	c.hello(t)
	c.sendCreate(t, map[string]any{"cwd": t.TempDir(), "requestId": "r-td"})
	id := waitGate(t, gate)
	rn.sigs <- syscall.SIGTERM
	if err := rn.stopped(t); err != nil {
		t.Fatal(err)
	}
	_ = c.nc.SetReadDeadline(time.Now().Add(step))
	if line, err := c.lr.ReadLine(); err == nil {
		t.Fatalf("the cut create was answered %s", line)
	}
	cmd, _ := s.cmd(0)
	if e, listed := listedEntry(t, env, id); !listed || e.RequestID != "r-td" || !alive(cmd.Process.Pid) {
		t.Fatalf("the cut create's host did not run on: %+v, %v", e, listed)
	}
	openGate(t, gate, "ok")
	waitFor(t, "the cut create's host started", func() bool {
		e, listed := listedEntry(t, env, id)
		return listed && e.Ready
	})

	gate2 := gateDir(t)
	rn2, _, sock2 := creating(t, testEnv(t), nil, always("gate:"+gate2))
	c2 := dial(t, sock2)
	c2.hello(t)
	c2.sendCreate(t, map[string]any{"cwd": t.TempDir()})
	waitGate(t, gate2)
	setVar(t, &teardownBound, step)
	rn2.sigs <- syscall.SIGTERM
	openGate(t, gate2, "ok")
	if res := result(t, c2.readAnswer(t)); res.Session.HostID == "" {
		t.Fatalf("the create within the bound answered %+v", res)
	}
	if err := rn2.stopped(t); err != nil {
		t.Fatal(err)
	}
}

// TestCreatesInFlightAreBounded: at createsMax creates in flight (two here)
// a third is unavailable, reason busy, spawning nothing; a request joining
// one in flight is no new create, and is never busy; once one has answered,
// another is taken.
func TestCreatesInFlightAreBounded(t *testing.T) {
	setVar(t, &createsMax, 2)
	env := testEnv(t)
	gates := []string{gateDir(t), gateDir(t)}
	_, s, sock := creating(t, env, nil, func(n int) string {
		if n < 2 {
			return "gate:" + gates[n]
		}
		return "ok"
	})
	var waiters []*client
	var params []map[string]any
	for i, g := range gates {
		w := dial(t, sock)
		w.hello(t)
		p := map[string]any{"cwd": t.TempDir(), "requestId": fmt.Sprintf("r-%d", i)}
		w.sendCreate(t, p)
		waitGate(t, g)
		waiters, params = append(waiters, w), append(params, p)
	}
	c := dial(t, sock)
	c.hello(t)
	refusedAs(t, createOn(t, c, map[string]any{"cwd": t.TempDir()}), protocol.CodeUnavailable, protocol.ReasonBusy)
	if n := s.count(); n != 2 {
		t.Fatalf("the busy create spawned: %d spawns", n)
	}
	joiner := dial(t, sock)
	joiner.hello(t)
	joiner.sendCreate(t, params[0])
	openGate(t, gates[0], "ok")
	first := waiters[0].readAnswer(t)
	if joined := joiner.readAnswer(t); string(joined.Result) != string(first.Result) || joined.Error != nil {
		t.Fatalf("the join answered %+v, the create %s", joined, first.Result)
	}
	res := result(t, createOn(t, c, map[string]any{"cwd": t.TempDir()}))
	if res.Session.HostID == "" || s.count() != 3 {
		t.Fatalf("the create after one ended answered %+v after %d spawns", res, s.count())
	}
	openGate(t, gates[1], "ok")
	result(t, waiters[1].readAnswer(t))
}

// TestACreateKeepsTheHubBusy (§3.5's idle rule): a create in flight keeps
// the hub from arming its idle grace though no client is left — a waiter's
// connection is counted until its answer, so here the create is admitted on
// the lifecycle alone — and its end lets the hub arm it. The negative
// control: with none in flight the last client's leaving arms it.
func TestACreateKeepsTheHubBusy(t *testing.T) {
	env := testEnv(t)
	hk := quiet()
	var g grace
	g.install(hk)
	rn := runWith(t, env, hk, creates("cursor"))
	sock := rn.line(t).Socket
	h := rn.serving(t)
	select {
	case <-g.armed:
	case <-time.After(step):
		t.Fatal("no grace armed at start, with nothing live")
	}
	c := dial(t, sock)
	c.hello(t)
	if perr := h.life.beginCreate(); perr != nil {
		t.Fatal(perr)
	}
	_ = c.nc.Close()
	waitFor(t, "the client gone", func() bool { return clients(h) == 0 })
	h.life.wakeLoop()
	time.Sleep(50 * time.Millisecond)
	g.none(t)
	if idle, _ := h.life.idle(); idle {
		t.Fatal("the hub reads idle with a create in flight")
	}
	h.life.endCreate()
	select {
	case <-g.armed:
	case <-time.After(step):
		t.Fatal("no grace armed once the create ended")
	}

	c2 := dial(t, sock)
	c2.hello(t)
	_ = c2.nc.Close()
	select {
	case <-g.armed:
	case <-time.After(step):
		t.Fatal("no grace armed once the last client left")
	}
}

// TestBeginCreateRefusesOnceClosing: a create admitted after the hub's
// decision to close is unavailable, reason closing.
func TestBeginCreateRefusesOnceClosing(t *testing.T) {
	var l lifecycle
	l.init()
	if perr := l.beginCreate(); perr != nil {
		t.Fatalf("before closing: %v", perr)
	}
	l.endCreate()
	l.quiesce()
	if perr := l.beginCreate(); perr == nil || perr.Data.Reason != protocol.ReasonClosing {
		t.Fatalf("once closing: %+v", perr)
	}
}

// ------------------------------------------------- attend, over a fake

// fakeCreated is a createdSession a test holds: its start, its prompt's
// answer, and the context each close was given.
type fakeCreated struct {
	start  func(context.Context) error
	submit func(context.Context) error
	row    json.RawMessage

	mu sync.Mutex
	// closes is each close's context, as live (true) or done when it came.
	closes []bool
	closed chan struct{}
}

func newFakeCreated() *fakeCreated {
	return &fakeCreated{start: func(context.Context) error { return nil }, submit: func(context.Context) error { return nil },
		row: json.RawMessage(`{ "sessionId": "s-1" }`), closed: make(chan struct{})}
}

func (f *fakeCreated) Start(ctx context.Context) error { return f.start(ctx) }
func (f *fakeCreated) Read(ctx context.Context) (backend.Item, error) {
	select {
	case <-ctx.Done():
		return backend.Item{}, ctx.Err()
	case <-f.closed:
		return backend.Item{}, backend.ErrClosed
	}
}
func (f *fakeCreated) Submit(ctx context.Context, c engine.Command, _ string, mode engine.SubmitMode, _ string) (engine.SubmitResult, error) {
	if c.ID != createCommandID || mode != engine.SubmitQueue {
		return engine.SubmitResult{}, fmt.Errorf("a prompt with command %q, mode %q", c.ID, mode)
	}
	return engine.SubmitResult{}, f.submit(ctx)
}
func (f *fakeCreated) ClientID() string { return "c-1" }
func (f *fakeCreated) CloseWithin(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.closes) == 0 {
		close(f.closed)
	}
	f.closes = append(f.closes, ctx.Err() == nil)
	return nil
}
func (f *fakeCreated) hostVersion() string { return "9.9.9" }
func (f *fakeCreated) sessionRow(context.Context) (json.RawMessage, error) {
	return f.row, nil
}

// detached says the one close was a detach: its context still live.
func (f *fakeCreated) detached(t *testing.T) bool {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.closes) != 1 {
		t.Fatalf("closed %d times, want once", len(f.closes))
	}
	return f.closes[0]
}

// TestAttendPromptOutcomes (§3.10): the first prompt's outcome as the list's
// dispatch reads it — taken is accepted; refused is the session's words, the
// session left running and detached from; an answer lost after the send
// (ErrOutcomeUnknown) or a call still out at the deadline is unknown, the
// latter closed at once (no detach, which could only wait behind the write);
// and with no prompt, none. The row is the host's, compacted; the version
// its hello's.
func TestAttendPromptOutcomes(t *testing.T) {
	h := &hub{}
	far := func() time.Time { return time.Now().Add(step) }
	for _, tc := range []struct {
		name      string
		prompt    string
		submit    func(context.Context) error
		wait      time.Duration
		want      protocol.CreatePrompt
		wantErr   string
		wantClean bool
	}{
		{"none", "", nil, step, protocol.CreatePromptNone, "", true},
		{"accepted", "hi", func(context.Context) error { return nil }, step, protocol.CreatePromptAccepted, "", true},
		{"refused", "hi", func(context.Context) error {
			return &remote.Error{Code: protocol.CodeQueueFull, Reason: protocol.ReasonQueueFull, Message: "the queue is full\nmore"}
		}, step, protocol.CreatePromptRefused, "the queue is full", true},
		{"its answer lost", "hi", func(context.Context) error {
			return fmt.Errorf("the connection went: %w", backend.ErrOutcomeUnknown)
		}, step, protocol.CreatePromptUnknown, "the connection went: backend: the command's outcome is unknown: it may have run", true},
		{"no answer by the deadline", "hi", func(context.Context) error {
			select {} // a write blocked where no context reaches it
		}, 200 * time.Millisecond, protocol.CreatePromptUnknown, "no answer to the prompt within 200ms", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setVar(t, &createPromptWait, tc.wait)
			f := newFakeCreated()
			if tc.submit != nil {
				f.submit = tc.submit
			}
			at := h.attend(context.Background(), f, tc.prompt, far())
			if at.kind != attendStarted || at.prompt != tc.want || at.promptErr != tc.wantErr {
				t.Fatalf("attend came to %+v, want %s (%q)", at, tc.want, tc.wantErr)
			}
			if got := f.detached(t); got != tc.wantClean {
				t.Fatalf("detached %v, want %v", got, tc.wantClean)
			}
			if tc.wantClean && (string(at.row) != `{"sessionId":"s-1"}` || at.readAt.IsZero() || at.version != "9.9.9") {
				t.Fatalf("the row %s read at %v, version %q", at.row, at.readAt, at.version)
			}
		})
	}
}

// TestAttendStartOutcomes: a start that fails is its first error line; one
// past its deadline the bound's words; one cut by the hub's teardown is cut;
// one whose connection went is lost. Each closes at once, sending nothing.
func TestAttendStartOutcomes(t *testing.T) {
	h := &hub{}
	for _, tc := range []struct {
		name     string
		start    func(context.Context) error
		ctx      func() context.Context
		deadline time.Duration
		want     attendKind
		cause    string
	}{
		{"failed", func(context.Context) error { return &remote.StartError{Text: "\n agent missing \nmore"} },
			context.Background, step, attendStartFailed, "agent missing"},
		{"over its bound", func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() },
			context.Background, 50 * time.Millisecond, attendStartTimeout, "the session did not start within 1m0s"},
		{"cut", func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() },
			func() context.Context {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx
			}, step, attendCut, ""},
		{"lost", func(context.Context) error {
			return errors.New("remote: the stream ended before the session was ready")
		},
			context.Background, step, attendLost, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeCreated()
			f.start = tc.start
			f.submit = func(context.Context) error { t.Error("a prompt was sent to a session that did not start"); return nil }
			at := h.attend(tc.ctx(), f, "hi", time.Now().Add(tc.deadline))
			if at.kind != tc.want || at.cause != tc.cause {
				t.Fatalf("attend came to %+v, want kind %d cause %q", at, tc.want, tc.cause)
			}
			if f.detached(t) {
				t.Fatal("a session that did not start was detached from, not closed at once")
			}
		})
	}
}

// TestCreateClient (Create, craze new's ask): a hub's result is answered as
// the hub wrote it; its refusal wraps the hub's *protocol.Error; a hub whose
// hello says it creates nothing is a *LacksError, nothing asked; and a
// connection that ends after the create was sent, before its answer, wraps
// ErrUnanswered — the one failure craze new tries again, under the same
// requestId. Only that one wraps it.
func TestCreateClient(t *testing.T) {
	env := testEnv(t)
	_, _, sock := creating(t, env, nil, always("ok"))
	who := protocol.ClientInfo{Kind: "test"}
	ctx, cancel := context.WithTimeout(context.Background(), step)
	defer cancel()
	raw, err := Create(ctx, sock, who, protocol.CreateParams{Cwd: t.TempDir(), RequestID: "r-c"})
	var res protocol.CreateResult
	if err != nil || json.Unmarshal(raw, &res) != nil || res.Session.HostID == "" || res.Prompt != protocol.CreatePromptNone {
		t.Fatalf("Create = %s, %v", raw, err)
	}
	_, err = Create(ctx, sock, who, protocol.CreateParams{Cwd: "relative"})
	var perr *protocol.Error
	if !errors.As(err, &perr) || perr.Data.Reason != protocol.ReasonBadRequest || errors.Is(err, ErrUnanswered) {
		t.Fatalf("a refused Create = %v", err)
	}

	older := runIn(t, testEnv(t), nil)
	_, err = Create(ctx, older.line(t).Socket, who, protocol.CreateParams{Cwd: "/"})
	var lacks *LacksError
	if !errors.As(err, &lacks) || err.Error() != "this hub (craze "+lacks.Version+") cannot create sessions; it exits when idle" {
		t.Fatalf("Create of a hub that creates nothing = %v", err)
	}

	// A hub that answers hello, reads the create and goes.
	path := filepath.Join(shortDir(t, "czd"), "hub.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		lr := protocol.NewLineReader(c, 0)
		line, err := lr.ReadLine()
		if err != nil {
			return
		}
		var req protocol.Request
		_ = json.Unmarshal(line, &req)
		result, _ := json.Marshal(hubResult(rundir.NewHubID(), "goner", protocol.HubCapabilities()))
		_ = protocol.WriteLine(c, protocol.Response{JSONRPC: "2.0", ID: req.ID, Result: result})
		_, _ = lr.ReadLine()
	}()
	_, err = Create(ctx, path, who, protocol.CreateParams{Cwd: "/"})
	if !errors.Is(err, ErrUnanswered) || errors.As(err, &perr) {
		t.Fatalf("Create on a connection that ends unanswered = %v", err)
	}
}
