package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
	// --hub-pid: this hub (in this process), for the host's macOS
	// login-session hint (plan 035 P3).
	for _, want := range []string{"serve", "--host-id=" + row.HostID, "--workspace=" + work, "--provider=cursor",
		"--model=m-1", "--effort=high", "--fast", "--no-force", "--no-host-status", "--request-id=req-1",
		"--request-hash=" + e.RequestHash, "--hub-pid=" + strconv.Itoa(os.Getpid())} {
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
	if !alive(t, cmd.Process.Pid) {
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
			if _, listed := listedEntry(t, env, fakehost.ParseSpawnArgs(argv).HostID); listed || alive(t, cmd.Process.Pid) {
				t.Fatalf("the failed session's host is still there (listed %v, alive %v)", listed, alive(t, cmd.Process.Pid))
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
			if cmd, _ := s.cmd(0); alive(t, cmd.Process.Pid) {
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

// TestACutWaiterGetsOnlyAnAnswerFromBeforeTheCut (X80): once the teardown's
// cut has begun, a waiter is answered only with an answer its create
// published before it — the case of a create that finished just as the cut
// came — and never with one published after (a create the cut cancelled,
// answering closing), whichever of done and cut the waiter sees first. PR 5's
// macOS CI delivered the cut's own "closing" to a cut waiter: the cut cancels
// the creates before it closes cut, and on a slow runner the cancelled create
// published first.
func TestACutWaiterGetsOnlyAnAnswerFromBeforeTheCut(t *testing.T) {
	closing := createAnswer{err: refused(protocol.CodeUnavailable, protocol.ReasonClosing, "the hub is closing")}
	done := createAnswer{res: &protocol.CreateResult{Prompt: protocol.CreatePromptNone}}
	for _, tc := range []struct {
		name     string
		ans      createAnswer
		afterCut bool
		want     bool
	}{
		{"published after the cut began", closing, true, false},
		{"published before it", done, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &createCall{done: make(chan struct{}), ans: tc.ans, afterCut: tc.afterCut}
			close(c.done)
			cut := make(chan struct{})
			close(cut)
			for range 50 { // both ready: the select takes either
				ans, ok := c.wait(cut)
				if ok != tc.want {
					t.Fatalf("wait answered %v (%+v), want %v", ok, ans, tc.want)
				}
			}
		})
	}
}

// TestACutBetweenACreatesAnswerAndItsPublicationIsAfterTheCut (X80, r53): a
// create whose fn has returned and whose bookkeeping is done, but whose
// answer the cut overtakes before done closes — here run is held at its
// endCreate (the lifecycle lock) while the cut comes — is published as after
// the cut, so its waiter is not answered; one published before the cut still
// is. Red when the mark is sampled as fn returns rather than as done closes.
func TestACutBetweenACreatesAnswerAndItsPublicationIsAfterTheCut(t *testing.T) {
	ok := createAnswer{res: &protocol.CreateResult{Prompt: protocol.CreatePromptNone}}
	start := func(t *testing.T, held bool) (release func(), cr *creator, c *createCall) {
		h := &hub{}
		h.life.init()
		if perr := h.life.beginCreate(); perr != nil {
			t.Fatal(perr)
		}
		cr = newCreator(h, Creates{})
		t.Cleanup(cr.stop)
		release = func() {}
		if held {
			// Held before run starts, so it stops at endCreate: past fn and
			// its bookkeeping, short of its publication (r54).
			h.life.mu.Lock()
			release = sync.OnceFunc(h.life.mu.Unlock)
			t.Cleanup(release)
		}
		c = &createCall{done: make(chan struct{})}
		go cr.run(c, func(context.Context) createAnswer { return ok })
		return release, cr, c
	}

	t.Run("the cut overtakes the publication", func(t *testing.T) {
		release, cr, c := start(t, true)
		waitFor(t, "the create's bookkeeping", func() bool {
			cr.mu.Lock()
			defer cr.mu.Unlock()
			return c.finished
		})
		cr.stop()
		release()
		<-c.done
		if ans, answered := c.wait(cr.cut); answered {
			t.Fatalf("a create published after the cut was answered %+v", ans)
		}
	})
	t.Run("published before the cut", func(t *testing.T) {
		_, cr, c := start(t, false)
		<-c.done
		cr.stop()
		if ans, answered := c.wait(cr.cut); !answered || ans.res != ok.res {
			t.Fatalf("a create published before the cut: %+v, answered %v", ans, answered)
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
	if e, listed := listedEntry(t, env, id); !listed || e.RequestID != "r-td" || !alive(t, cmd.Process.Pid) {
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

	mu sync.Mutex
	// closes is each close's context, as live (true) or done when it came.
	closes []bool
	closed chan struct{}
}

func newFakeCreated() *fakeCreated {
	return &fakeCreated{start: func(context.Context) error { return nil }, submit: func(context.Context) error { return nil },
		closed: make(chan struct{})}
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

// TestAttendPromptOutcomes (§3.10, r32 2): the first prompt's outcome as the
// list's dispatch reads it — taken is accepted and refused the session's
// words, each detached from (the session left running); with no prompt,
// none, detached from too. Every other outcome is unknown and closes the
// connection at once — no detach, which would queue behind a write still out
// on it: an answer lost after the send (ErrOutcomeUnknown), a Submit that
// returned its own deadline or a cancellation — a resend may still be blocked
// on that connection — and a call still out at the deadline. The version is
// the hello's.
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
		}, step, protocol.CreatePromptUnknown, "the connection went: backend: the command's outcome is unknown: it may have run", false},
		{"the Submit returned its deadline", "hi", func(context.Context) error {
			return fmt.Errorf("remote: %w", context.DeadlineExceeded)
		}, step, protocol.CreatePromptUnknown, "remote: context deadline exceeded", false},
		{"the Submit returned a cancellation", "hi", func(context.Context) error {
			return context.Canceled
		}, step, protocol.CreatePromptUnknown, "context canceled", false},
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
			if at.version != "9.9.9" {
				t.Fatalf("the version %q, want the hello's", at.version)
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

// ------------------------------------------------ the r32 review's fixes

// TestACreatedHostsAgentsAreEndedWhenItDies (r32 1, P11): a host the hub
// spawned is the hub's to clean up after even once its create has answered:
// SIGKILLed later — its stop sequence never run — its recorded agent (a
// `sleep` leading a group of its own, as an ACP agent does) is ended by the
// hub, and the record removed.
func TestACreatedHostsAgentsAreEndedWhenItDies(t *testing.T) {
	env := testEnv(t)
	_, s, sock := creating(t, env, nil, always("ok,agent"))
	c := dial(t, sock)
	c.hello(t)
	res := result(t, createOn(t, c, map[string]any{"cwd": t.TempDir()}))
	dir, err := rundir.HostLogDir(env)
	if err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(dir, hostspawn.AgentGroupsName(res.Session.HostID))
	b, err := os.ReadFile(record)
	g, ok := hostspawn.ParseAgentGroup(strings.TrimSpace(string(b)))
	if err != nil || !ok {
		t.Fatalf("the host's agents' record %q (%v)", b, err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-g.PGID, syscall.SIGKILL) })
	if !alive(t, g.PGID) {
		t.Fatalf("the host's agent %d is not running", g.PGID)
	}
	cmd, _ := s.cmd(0)
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the dead host's agent ended", func() bool { return !alive(t, g.PGID) })
	waitFor(t, "the dead host's agents' record removed", func() bool {
		_, err := os.Stat(record)
		return errors.Is(err, os.ErrNotExist)
	})
}

// stalledPrompt is a created host's connection whose first prompt's call
// has come back with its deadline while — the review's schedule (r32 2) — a
// resend of it is blocked on a host that stopped reading: from then on any
// use of the connection but closing it at once (a detach, a read) waits
// behind that write, for good (until release, the test's end). A connection
// that sent no prompt is the real one.
type stalledPrompt struct {
	createdSession
	prompted *atomic.Bool
	release  chan struct{}
}

func (s stalledPrompt) Submit(context.Context, engine.Command, string, engine.SubmitMode, string) (engine.SubmitResult, error) {
	s.prompted.Store(true)
	return engine.SubmitResult{}, fmt.Errorf("remote: session.prompt: %w", context.DeadlineExceeded)
}

func (s stalledPrompt) CloseWithin(ctx context.Context) error {
	if s.prompted.Load() && ctx.Err() == nil {
		<-s.release
	}
	return s.createdSession.CloseWithin(ctx)
}

// TestAStalledPromptConnectionFreesItsSlot (r32 2): a create whose prompt's
// call came back with its deadline while its connection stays blocked
// answers within its bounds — the connection closed at once, the row read on
// a connection of its own, fresh — and frees its slot: with one create
// allowed in flight, the next is taken, not refused busy.
func TestAStalledPromptConnectionFreesItsSlot(t *testing.T) {
	setVar(t, &createsMax, 1)
	env := testEnv(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	var prompted atomic.Bool
	setVar(t, &createDial, func(ctx context.Context, socket, sid string) (createdSession, error) {
		sess, err := dialCreated(ctx, socket, sid)
		if err != nil {
			return nil, err
		}
		return stalledPrompt{createdSession: sess, prompted: &prompted, release: release}, nil
	})
	_, s, sock := creating(t, env, nil, always("ok"))
	c := dial(t, sock)
	c.hello(t)
	work := t.TempDir()
	res := result(t, createOn(t, c, map[string]any{"cwd": work, "prompt": "hi"}))
	if res.Prompt != protocol.CreatePromptUnknown || res.Session.Approximate || len(res.Session.Row) == 0 {
		t.Fatalf("the stalled prompt's create answered %+v", res)
	}
	prompted.Store(false)
	res2 := result(t, createOn(t, c, map[string]any{"cwd": work}))
	if res2.Session.HostID == res.Session.HostID || s.count() != 2 {
		t.Fatalf("the create after it answered %+v after %d spawns", res2, s.count())
	}
}

// TestAFreshRowReadIsBoundedByClosingItsConnection (r32 2): the row's read
// on a connection of its own against a host that never answers returns, an
// error, once its bound has passed — never later.
func TestAFreshRowReadIsBoundedByClosingItsConnection(t *testing.T) {
	setVar(t, &createReadWait, 200*time.Millisecond)
	path := filepath.Join(shortDir(t, "czr"), "h.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	var held []net.Conn
	var mu sync.Mutex
	go func() {
		for {
			nc, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			held = append(held, nc) // never read, never answered
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		for _, nc := range held {
			_ = nc.Close()
		}
	})
	done := make(chan error, 1)
	go func() {
		_, _, _, err := freshRow(context.Background(), path, "s")
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a host that never answered gave a row")
		}
	case <-time.After(step):
		t.Fatalf("the row read was still waiting %v after its 200ms bound", step)
	}
}

// TestRecoveryProvesAbsenceBeforeItSpawns (r32 3a): a requestId the memory
// does not hold is spawned for only once the registry has shown no live host
// of this namespace carries it — a registry that cannot be read, and a live
// host whose entry cannot be, refuse the create unavailable, reason
// host_unreachable, spawning nothing; a dead host's unreadable entry is
// swept and does not stand in the way.
func TestRecoveryProvesAbsenceBeforeItSpawns(t *testing.T) {
	t.Run("the registry cannot be read", func(t *testing.T) {
		env := testEnv(t)
		_, s, sock := creating(t, env, nil, always("ok"))
		setVar(t, &hostsScan, func(rundir.Env) ([]rundir.Entry, []string, error) {
			return nil, nil, errors.New("the registry is unreadable")
		})
		c := dial(t, sock)
		c.hello(t)
		e := refusedAs(t, createOn(t, c, map[string]any{"cwd": t.TempDir(), "requestId": "r-a"}),
			protocol.CodeUnavailable, protocol.ReasonHostUnreachable)
		if !strings.Contains(e.Message, "registry of session hosts cannot be read") || s.count() != 0 {
			t.Fatalf("refused %q after %d spawns", e.Message, s.count())
		}
	})
	t.Run("a live host's entry cannot be read", func(t *testing.T) {
		env := testEnv(t)
		_, s, sock := creating(t, env, nil, always("ok"))
		live, err := rundir.Bind(env, rundir.NewHostID(), rundir.Entry{CrazeSessionID: "s-live"})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = live.Close() })
		writeEntry(t, env, live.ID(), "{not json")
		c := dial(t, sock)
		c.hello(t)
		e := refusedAs(t, createOn(t, c, map[string]any{"cwd": t.TempDir(), "requestId": "r-b"}),
			protocol.CodeUnavailable, protocol.ReasonHostUnreachable)
		if !strings.Contains(e.Message, "entry of a live session host cannot be read") || s.count() != 0 {
			t.Fatalf("refused %q after %d spawns", e.Message, s.count())
		}
	})
	t.Run("a dead host's unreadable entry", func(t *testing.T) {
		env := testEnv(t)
		_, s, sock := creating(t, env, nil, always("ok"))
		gone, err := rundir.Bind(env, rundir.NewHostID(), rundir.Entry{})
		if err != nil {
			t.Fatal(err)
		}
		if err := gone.Close(); err != nil {
			t.Fatal(err)
		}
		dead := rundir.NewHostID()
		writeEntry(t, env, dead, "{not json")
		writeFile(t, filepath.Join(env.Home, ".cache", "craze", "hosts", dead+".lock"))
		c := dial(t, sock)
		c.hello(t)
		if res := result(t, createOn(t, c, map[string]any{"cwd": t.TempDir(), "requestId": "r-c"})); res.Session.HostID == "" || s.count() != 1 {
			t.Fatalf("the create answered %+v after %d spawns", res, s.count())
		}
	})
}

// writeEntry writes body as host id's registry entry in env.
func writeEntry(t *testing.T, env rundir.Env, id, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(env.Home, ".cache", "craze", "hosts", id+".json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestARepeatIsAnsweredBeforeTheWorldIsChecked (r32 3b): a create's
// directory and provider are checked only for a create that will spawn: a
// repeat of one whose answer is kept — a success, or a failure — and one a
// restarted hub joins are answered as before after the configured default
// provider has gone and the directory been removed, while a new create is
// refused for them (the check itself still holds).
func TestARepeatIsAnsweredBeforeTheWorldIsChecked(t *testing.T) {
	env := testEnv(t)
	cfg, setDefault := mutableCreates("cursor")
	s := hostsAsChildren(t, env, func(n int) string {
		if n == 1 {
			return "fail"
		}
		return "ok"
	})
	rn := runWith(t, env, nil, cfg)
	c := dial(t, rn.line(t).Socket)
	c.hello(t)
	work := filepath.Join(t.TempDir(), "work")
	if err := os.Mkdir(work, 0o700); err != nil {
		t.Fatal(err)
	}
	ok := map[string]any{"cwd": work, "prompt": "hi", "requestId": "r-ok"}
	failing := map[string]any{"cwd": work, "requestId": "r-fail"}
	first := createOn(t, c, ok)
	res := result(t, first)
	firstFail := refusedAs(t, createOn(t, c, failing), protocol.CodeNotAccepting, protocol.ReasonStartFailed)

	setDefault("")
	if err := os.RemoveAll(work); err != nil {
		t.Fatal(err)
	}
	refusedAs(t, createOn(t, c, map[string]any{"cwd": work, "requestId": "r-new"}), protocol.CodeBadRequest, protocol.ReasonBadRequest)
	if again := createOn(t, c, ok); string(again.Result) != string(first.Result) {
		t.Fatalf("the repeat answered %+v (%s), the first %s", again.Error, again.Result, first.Result)
	}
	if again := refusedAs(t, createOn(t, c, failing), protocol.CodeNotAccepting, protocol.ReasonStartFailed); again.Message != firstFail.Message {
		t.Fatalf("the failure's repeat says %q, the first %q", again.Message, firstFail.Message)
	}

	rn.sigs <- syscall.SIGTERM
	if err := rn.stopped(t); err != nil {
		t.Fatal(err)
	}
	rn2 := runWith(t, env, nil, cfg)
	c2 := dial(t, rn2.line(t).Socket)
	c2.hello(t)
	joined := result(t, createOn(t, c2, ok))
	if joined.Session.HostID != res.Session.HostID || joined.Prompt != protocol.CreatePromptUnknown || s.count() != 2 {
		t.Fatalf("the restarted hub's repeat answered %+v (first %+v) after %d spawns", joined, res, s.count())
	}
}

// TestRecoveryStaysInItsNamespace (r32 3c): two hubs of one HOME and two
// CRAZE_HOMEs: hub B, asked for hub A's requestId with the same params,
// creates its own session — a host of A's namespace is A's, never B's to join
// — and with other params under an id A used, creates too: no conflict across
// namespaces.
func TestRecoveryStaysInItsNamespace(t *testing.T) {
	a := testEnv(t)
	b := a
	b.CrazeDir = filepath.Join(a.Home, ".craze-b")
	work := t.TempDir()
	same := map[string]any{"cwd": work, "prompt": "hi", "requestId": "r-ns"}
	sa := hostsAsChildren(t, a, always("ok"))
	rnA := runWith(t, a, nil, creates("cursor"))
	ca := dial(t, rnA.line(t).Socket)
	ca.hello(t)
	resA := result(t, createOn(t, ca, same))
	result(t, createOn(t, ca, map[string]any{"cwd": work, "requestId": "r-other"}))

	sb := hostsAsChildren(t, b, always("ok"))
	rnB := runWith(t, b, nil, creates("cursor"))
	cb := dial(t, rnB.line(t).Socket)
	cb.hello(t)
	resB := result(t, createOn(t, cb, same))
	if resB.Session.HostID == resA.Session.HostID || resB.Prompt != protocol.CreatePromptAccepted || sb.count() != 1 {
		t.Fatalf("hub B answered %+v (A's %+v) after %d spawns of its own", resB, resA, sb.count())
	}
	resB2 := result(t, createOn(t, cb, map[string]any{"cwd": work, "prompt": "other", "requestId": "r-other"}))
	if resB2.Session.HostID == "" || sb.count() != 2 || sa.count() != 2 {
		t.Fatalf("hub B's create under A's other id answered %+v; spawns A %d, B %d", resB2, sa.count(), sb.count())
	}
}

// TestKeptAnswersAreCappedAsTheyArePublished (r32 5): the cap on kept
// answers holds as each one is kept, not only at the next admission; a create
// in flight is never what makes room.
func TestKeptAnswersAreCappedAsTheyArePublished(t *testing.T) {
	setVar(t, &createKeepMax, 2)
	h := &hub{}
	h.life.init()
	cr := newCreator(h, *creates("cursor"))
	cr.calls["flight"] = &createCall{id: "flight", done: make(chan struct{})}
	for i := range 3 {
		if perr := h.life.beginCreate(); perr != nil {
			t.Fatal(perr)
		}
		cr.mu.Lock()
		c := cr.newCallLocked(fmt.Sprintf("r-%d", i), "h")
		cr.mu.Unlock()
		cr.run(c, func(context.Context) createAnswer { return createAnswer{err: closingErr()} })
	}
	var ids []string
	for id := range cr.calls {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	if want := []string{"flight", "r-1", "r-2"}; !slices.Equal(ids, want) {
		t.Fatalf("kept %v, want %v", ids, want)
	}
}

// TestAnUnreadableRowIsAnApproximateSuccess (r32 6a, X49): a create whose
// session started is a success even when its row cannot then be read: the
// session's ids from the registry and its ready line, the row absent, and
// approximate true.
func TestAnUnreadableRowIsAnApproximateSuccess(t *testing.T) {
	env := testEnv(t)
	setVar(t, &createRowRead, func(context.Context, string, string) (json.RawMessage, string, time.Time, error) {
		return nil, "", time.Time{}, errors.New("the host does not answer")
	})
	_, _, sock := creating(t, env, nil, always("ok"))
	c := dial(t, sock)
	c.hello(t)
	res := result(t, createOn(t, c, map[string]any{"cwd": t.TempDir()}))
	e, listed := listedEntry(t, env, res.Session.HostID)
	if !listed || res.Session.SessionID != e.CrazeSessionID || !res.Session.Approximate || len(res.Session.Row) != 0 ||
		!res.Session.Host.Ready || res.Session.Host.PID != e.PID {
		t.Fatalf("a create whose row could not be read answered %+v (its entry %+v)", res.Session, e)
	}
}

// slowDetach is a created host's connection whose detach takes its whole
// bound (createDetachWait): a host slow to answer it.
type slowDetach struct{ createdSession }

func (s slowDetach) CloseWithin(ctx context.Context) error {
	<-ctx.Done()
	return s.createdSession.CloseWithin(ctx)
}

// TestTheRowIsFreshAfterASlowDetach (r32 6a): the row is read after the
// create has let go of its connection, and judged fresh at that read: a
// detach that takes longer than a row stays fresh (3 s) cannot age it.
func TestTheRowIsFreshAfterASlowDetach(t *testing.T) {
	setVar(t, &createDetachWait, 3500*time.Millisecond)
	setVar(t, &createDial, func(ctx context.Context, socket, sid string) (createdSession, error) {
		sess, err := dialCreated(ctx, socket, sid)
		if err != nil {
			return nil, err
		}
		return slowDetach{sess}, nil
	})
	env := testEnv(t)
	_, _, sock := creating(t, env, nil, always("ok"))
	c := dial(t, sock)
	c.hello(t)
	res := result(t, createOn(t, c, map[string]any{"cwd": t.TempDir()}))
	if res.Session.Approximate || len(res.Session.Row) == 0 {
		t.Fatalf("the row after a slow detach: %+v", res.Session)
	}
}

// TestAStalledWorldCheckHoldsOnlyItsCreate (r38 4): a new create's check of
// the world runs outside the table's lock, so one whose workspace stat stalls
// (held here at its check) holds its own create alone: a repeat of a kept
// answer is answered, and another create runs to its answer and frees its
// slot — with two creates allowed in flight, a third is then taken — while it
// stays held; let go, it is answered too.
func TestAStalledWorldCheckHoldsOnlyItsCreate(t *testing.T) {
	setVar(t, &createsMax, 2)
	env := testEnv(t)
	stalled := t.TempDir()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	setVar(t, &createCheck, func(cr *creator, p *createReq) *protocol.Error {
		if p.p.Cwd == stalled {
			close(entered)
			<-release
		}
		return cr.check(p)
	})
	rn, s, sock := creating(t, env, nil, always("ok"))
	// Registered after the fixtures, so it runs before they are taken down
	// (r41 5a): the held check let go and its execution joined while the hub,
	// its hosts' command and the seams are still this test's.
	h := rn.serving(t)
	t.Cleanup(func() {
		once.Do(func() { close(release) })
		joinCreates(t, h)
	})
	c := dial(t, sock)
	c.hello(t)
	work := t.TempDir()
	kept := map[string]any{"cwd": work, "requestId": "r-kept"}
	first := createOn(t, c, kept)
	result(t, first)

	held := dial(t, sock)
	held.hello(t)
	held.sendCreate(t, map[string]any{"cwd": stalled, "requestId": "r-stall"})
	select {
	case <-entered:
	case <-time.After(step):
		t.Fatal("the stalled create never reached its check")
	}
	if again := createOn(t, c, kept); string(again.Result) != string(first.Result) {
		t.Fatalf("the kept answer's repeat answered %+v (%s), want %s", again.Error, again.Result, first.Result)
	}
	if res := result(t, createOn(t, c, map[string]any{"cwd": work})); res.Session.HostID == "" {
		t.Fatalf("a create beside the stalled one answered %+v", res)
	}
	if res := result(t, createOn(t, c, map[string]any{"cwd": work})); res.Session.HostID == "" || s.count() != 3 {
		t.Fatalf("a create after one freed its slot answered %+v after %d spawns", res, s.count())
	}
	once.Do(func() { close(release) })
	if res := result(t, held.readAnswer(t)); res.Session.HostID == "" || s.count() != 4 {
		t.Fatalf("the stalled create, let go, answered %+v after %d spawns", res, s.count())
	}
}

// TestAnAgentTheHelperCannotRecordIsKilled (r38 7): the created-host child's
// agent (startRecordedAgent) that cannot be recorded — here the host logs'
// directory cannot be made, HOME being a file — is killed and waited for
// before the helper returns: no child of this process is left a `sleep`.
func TestAnAgentTheHelperCannotRecordIsKilled(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("reads this process's children from /proc")
	}
	home := filepath.Join(t.TempDir(), "home-is-a-file")
	writeFile(t, home)
	t.Setenv("HOME", home)
	if err := startRecordedAgent(rundir.NewHostID()); err == nil {
		t.Fatal("an agent whose record cannot be written was recorded")
	}
	if kids := sleepChildren(t); len(kids) != 0 {
		for _, pid := range kids {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			reap(pid)
		}
		t.Fatalf("the unrecorded agent %v was left running", kids)
	}
}

// reap waits for this process's child pid to exit, past an interrupted wait
// (r41 5b): a killed child is not left a zombie.
func reap(pid int) {
	var ws syscall.WaitStatus
	for {
		_, err := syscall.Wait4(pid, &ws, 0, nil)
		if !errors.Is(err, syscall.EINTR) {
			return
		}
	}
}

// joinCreates waits, within step, until h has no create in flight and every
// create it keeps has published its answer: each execution has returned.
func joinCreates(t *testing.T, h *hub) {
	t.Helper()
	waitFor(t, "every create's execution to end", func() bool {
		h.life.mu.Lock()
		n := h.life.creates
		h.life.mu.Unlock()
		if n != 0 {
			return false
		}
		h.cr.mu.Lock()
		defer h.cr.mu.Unlock()
		for _, c := range h.cr.calls {
			select {
			case <-c.done:
			default:
				return false
			}
		}
		return true
	})
}

// TestACreateCutAtItsCheckLaunchesNothing (r41 4): a create held at its check
// of the world while its hub tears down and cuts it — its waiter's
// connection closed — launches nothing once the check returns: it answers
// closing (the answer kept under its id, when it has one) and frees its slot,
// with no host command built, with a requestId and without one.
func TestACreateCutAtItsCheckLaunchesNothing(t *testing.T) {
	for _, id := range []string{"r-cut", ""} {
		name := "with a requestId"
		if id == "" {
			name = "without one"
		}
		t.Run(name, func(t *testing.T) {
			setVar(t, &teardownBound, 300*time.Millisecond)
			env := testEnv(t)
			held := t.TempDir()
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			setVar(t, &createCheck, func(cr *creator, p *createReq) *protocol.Error {
				if p.p.Cwd == held {
					close(entered)
					<-release
				}
				return cr.check(p)
			})
			rn, s, sock := creating(t, env, nil, always("ok"))
			h := rn.serving(t)
			t.Cleanup(func() {
				once.Do(func() { close(release) })
				joinCreates(t, h)
			})
			c := dial(t, sock)
			c.hello(t)
			params := map[string]any{"cwd": held}
			if id != "" {
				params["requestId"] = id
			}
			c.sendCreate(t, params)
			select {
			case <-entered:
			case <-time.After(step):
				t.Fatal("the create never reached its check")
			}
			rn.sigs <- syscall.SIGTERM
			if err := rn.stopped(t); err != nil {
				t.Fatal(err)
			}
			_ = c.nc.SetReadDeadline(time.Now().Add(step))
			if line, err := c.lr.ReadLine(); err == nil {
				t.Fatalf("the cut create was answered %s", line)
			}
			once.Do(func() { close(release) })
			joinCreates(t, h)
			if n := s.count(); n != 0 {
				t.Fatalf("the cut create built %d host commands, want none", n)
			}
			if id != "" {
				h.cr.mu.Lock()
				ans := h.cr.calls[id].ans
				h.cr.mu.Unlock()
				if ans.err == nil || ans.err.Data.Reason != protocol.ReasonClosing {
					t.Fatalf("the cut create's kept answer is %+v, want closing", ans)
				}
			}
		})
	}
}

// sleepChildren is every `sleep` child of this process, from /proc.
func sleepChildren(t *testing.T) []int {
	t.Helper()
	dirs, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatal(err)
	}
	var out []int
	for _, d := range dirs {
		pid, err := strconv.Atoi(d.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile(filepath.Join("/proc", d.Name(), "stat"))
		if err != nil {
			continue
		}
		s := string(b)
		open, closing := strings.IndexByte(s, '('), strings.LastIndexByte(s, ')')
		if open < 0 || closing < open {
			continue
		}
		f := strings.Fields(s[closing+1:])
		if s[open+1:closing] == "sleep" && len(f) > 1 && f[1] == strconv.Itoa(os.Getpid()) {
			out = append(out, pid)
		}
	}
	return out
}

// TestALaunchRacingTheClosingStartsNothing (r43 1, r45 F2): a create past
// its last stopping check whose hub closes in that instant — the teardown's
// quiesce, run by the seam between the check and the launch — starts no
// host: the launch's reservation is taken under the lifecycle lock the
// closing is set under, and is refused unavailable, reason closing, with
// nothing counted in flight.
func TestALaunchRacingTheClosingStartsNothing(t *testing.T) {
	env := testEnv(t)
	rn, s, sock := creating(t, env, nil, always("ok"))
	h := rn.serving(t)
	setVar(t, &createLaunching, func(hh *hub) { hh.life.quiesce() })
	t.Cleanup(func() { joinCreates(t, h) })
	c := dial(t, sock)
	c.hello(t)
	refusedAs(t, createOn(t, c, map[string]any{"cwd": t.TempDir()}), protocol.CodeUnavailable, protocol.ReasonClosing)
	for i := range s.count() {
		if cmd, _ := s.cmd(i); cmd.Process != nil {
			t.Fatalf("a host was started (pid %d) after the hub closed", cmd.Process.Pid)
		}
	}
	if !waitUntil(&h.cr.launches, time.Now().Add(step)) {
		t.Fatal("a refused launch is still counted in flight")
	}
}

// TestALaunchDoneAfterTheClosingEndsItsHost (r45 F2, r46 1): a launch whose
// start returns once the hub has begun closing — the closing set while the
// host was being started, outside the lifecycle lock — kills that host at
// once: when the create is answered unavailable, reason closing, the host has
// been killed and reaped, and the launch is no longer counted in flight. The
// negative control is the launch that does not look again: the host is
// handed on, and the create answers its session.
func TestALaunchDoneAfterTheClosingEndsItsHost(t *testing.T) {
	env := testEnv(t)
	launched := make(chan int, 1)
	setVar(t, &createLaunched, func(pid int) { launched <- pid })
	rn, _, sock := creating(t, env, nil, always("ok"))
	h := rn.serving(t)
	setVar(t, &createStart, func(cmd *exec.Cmd, marker, groups, log string) (*hostspawn.Child, *os.File, error) {
		child, r, err := hostspawn.StartCmd(cmd, marker, groups, log)
		h.life.quiesce()
		return child, r, err
	})
	t.Cleanup(func() { joinCreates(t, h) })
	c := dial(t, sock)
	c.hello(t)
	refusedAs(t, createOn(t, c, map[string]any{"cwd": t.TempDir()}), protocol.CodeUnavailable, protocol.ReasonClosing)
	var pid int
	select {
	case pid = <-launched:
	default:
		t.Fatal("no host was launched")
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("the host %d launched as the hub closed is still there (%v, %s) when the create is answered", pid, err, procState(pid))
	}
	if !waitUntil(&h.cr.launches, time.Now().Add(step)) {
		t.Fatal("the launch is still counted in flight")
	}
}

// TestALateLaunchIsKilledBeforeRunReturns (r46 1, r47 4): a launch whose
// start is held through the teardown's cut and then returns, successfully,
// inside the cleanup wait, finds the hub closing and kills its host at once,
// no grace: a host that ignores SIGTERM — and has said so — and never answers
// its ready line is gone, reaped, when Run returns. Nothing here is timed:
// the start is let go once the test has seen the cut, and the cleanup wait is
// long. The negative control is the graced end (terminate): SIGTERM, ignored,
// and a grace longer than the cleanup wait — Run returns with the host still
// there.
func TestALateLaunchIsKilledBeforeRunReturns(t *testing.T) {
	setVar(t, &teardownBound, 300*time.Millisecond)
	setVar(t, &createCleanupWait, step/2)
	setVar(t, &createCutGrace, 4*step)
	env := testEnv(t)
	dir := shortDir(t, "czi")
	rn, _, sock := creating(t, env, nil, always("hang,ignoreterm:"+dir))
	h := rn.serving(t)
	t.Cleanup(func() { joinCreates(t, h) })
	started, release := make(chan int, 1), make(chan struct{})
	var once sync.Once
	letGo := func() { once.Do(func() { close(release) }) }
	t.Cleanup(letGo)
	setVar(t, &createStart, func(cmd *exec.Cmd, marker, groups, log string) (*hostspawn.Child, *os.File, error) {
		child, r, err := hostspawn.StartCmd(cmd, marker, groups, log)
		if err != nil {
			return nil, nil, err
		}
		started <- child.PID()
		<-release
		return child, r, nil
	})
	var pid int
	t.Cleanup(func() {
		// The negative control's host, still in its grace: ended here, so
		// the create that waits on it can end.
		if t.Failed() && pid != 0 {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	c := dial(t, sock)
	c.hello(t)
	c.sendCreate(t, map[string]any{"cwd": t.TempDir()})
	select {
	case pid = <-started:
	case <-time.After(step):
		t.Fatal("no host was started")
	}
	waitFor(t, "the host to ignore SIGTERM", func() bool {
		_, err := os.Stat(filepath.Join(dir, "ignoring"))
		return err == nil
	})
	rn.sigs <- syscall.SIGTERM
	select {
	case <-h.cr.cut:
	case <-time.After(step):
		t.Fatal("the teardown never cut the creates")
	}
	letGo()
	if err := rn.stopped(t); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("the host %d whose start returned in the cleanup wait is still there (%v, %s) when Run has returned", pid, err, procState(pid))
	}
}

// TestAStalledLaunchHoldsNoTeardown (r45 F2): a create whose launch is
// stalled inside its start — cmd.Start not returning, as for a child stalled
// in its chdir into a hung filesystem — while its hub tears down, with a
// roster subscription and a splice open, holds the teardown only within its
// bounds: the lifecycle lock is not held across the start, so the quiesce
// goes on; the subscription is ended with reset{hub_closing}, the splice is
// closed both ways, Run returns within teardownBound, createCleanupWait and
// the splice's drain (taken on Run's own goroutine as it returns, not when
// this one next runs), the hub's lock is free and its files are gone, and the log says a
// start was left as it was. The tail and the reset's own wait are generous
// here (r46 5): a starved flusher still writes the reset in time — the tail
// is a deadline, never a wait. The negative controls: the start run under
// the lifecycle lock (r43 1's launch) — the teardown never gets past its
// quiesce, and nothing is written to the subscriber; and no tail of its own
// once the creates' waits have used the bound up — the reset's write is past
// its deadline, and the subscriber reads only the connection's end.
func TestAStalledLaunchHoldsNoTeardown(t *testing.T) {
	setVar(t, &teardownBound, 300*time.Millisecond)
	setVar(t, &createCleanupWait, 300*time.Millisecond)
	setVar(t, &teardownTail, step)
	setVar(t, &resetWait, step)
	env := testEnv(t)
	rh := newRawHost(t, env, hostOf(4), sessionOf(4))
	rn, _, sock := creating(t, env, quiet(), always("ok"))
	h := rn.serving(t)
	t.Cleanup(func() { joinCreates(t, h) })
	entered, release := make(chan struct{}, 1), make(chan struct{})
	setVar(t, &createStart, func(*exec.Cmd, string, string, string) (*hostspawn.Child, *os.File, error) {
		entered <- struct{}{}
		<-release
		return nil, nil, errors.New("the stalled start, let go")
	})
	t.Cleanup(func() { close(release) })

	// The splice first, its host leg the first connection the host accepts
	// (the subscriber's poll dials the host too): the host reads the client's
	// hello through it.
	sp := dialPeer(t, sock)
	if m := sp.call(protocol.MethodSessionConnect, protocol.ConnectParams{SessionID: sessionOf(4)}); m.Error != nil || string(m.Result) != "{}" {
		t.Fatalf("session.connect: %s", clip(m.raw))
	}
	hc := rh.accept(t)
	sent, err := protocol.MarshalLine(map[string]any{"jsonrpc": "2.0", "id": 9, "method": "hello", "params": hostHello})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sp.nc.Write(sent); err != nil {
		t.Fatal(err)
	}
	_ = hc.SetReadDeadline(time.Now().Add(step))
	hr := protocol.NewLineReader(hc, 0)
	if got, err := hr.ReadLine(); err != nil || !bytes.Equal(append(got, '\n'), sent) {
		t.Fatalf("the host read %q (%v), want the client's hello as written, %q", got, err, sent)
	}
	p := dialPeer(t, sock)
	sub := p.subscribe()
	c := dial(t, sock)
	c.hello(t)
	c.sendCreate(t, map[string]any{"cwd": t.TempDir()})
	select {
	case <-entered:
	case <-time.After(step):
		t.Fatal("the create's launch never reached its start")
	}

	start := time.Now()
	rn.sigs <- syscall.SIGTERM
	var last msg
	for {
		last = p.read()
		if last.Method != protocol.NotifyRoster {
			break
		}
	}
	var reset protocol.ResetParams
	if last.Method != protocol.NotifyReset || json.Unmarshal(last.Params, &reset) != nil ||
		reset != (protocol.ResetParams{Subscription: sub.Subscription, Reason: protocol.ResetHubClosing}) {
		t.Fatalf("the subscriber's last line: %s, want reset{%s, hub_closing}", clip(last.raw), sub.Subscription)
	}
	if !p.eof() {
		t.Fatal("the subscriber's connection was not closed after its reset")
	}
	if !sp.eof() {
		t.Fatal("the splice's client leg was not closed")
	}
	_ = hc.SetReadDeadline(time.Now().Add(step))
	if got, err := hr.ReadLine(); !errors.Is(err, io.EOF) {
		t.Fatalf("the splice's host leg read %q (%v), not its end", got, err)
	}
	if err := rn.stopped(t); err != nil {
		t.Fatal(err)
	}
	if took, bound := rn.returned.Sub(start), teardownBound+createCleanupWait+spliceDrain; took > bound+3*time.Second {
		t.Fatalf("the teardown took %v with a stalled launch, past its bounds %v", took, bound)
	}
	absent(t, "the record", recordPath(t, env))
	absent(t, "the socket", sock)
	lockFree(t, env)
	if log := rn.stderr.String(); !strings.Contains(log, "a start that has not returned is left as it is") {
		t.Fatalf("the hub's log does not say the stalled start was left: %s", log)
	}
}

// TestACutHostIsReapedBeforeRunReturns (r43 5): a create whose host has not
// answered its ready line — one that never will, and ignores SIGTERM — is cut
// by its hub's teardown, and that host is ended (SIGTERM, the cut's grace,
// SIGKILL) and reaped before Run returns: no such process is left the instant
// it has.
func TestACutHostIsReapedBeforeRunReturns(t *testing.T) {
	setVar(t, &teardownBound, 300*time.Millisecond)
	setVar(t, &createCutGrace, 200*time.Millisecond)
	env := testEnv(t)
	launched := make(chan int, 1)
	setVar(t, &createLaunched, func(pid int) { launched <- pid })
	rn, _, sock := creating(t, env, nil, always("hang,ignoreterm"))
	c := dial(t, sock)
	c.hello(t)
	c.sendCreate(t, map[string]any{"cwd": t.TempDir()})
	var pid int
	select {
	case pid = <-launched:
	case <-time.After(step):
		t.Fatal("no host was launched")
	}
	rn.sigs <- syscall.SIGTERM
	if err := rn.stopped(t); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("the cut host %d is still there (%v, %s) when Run has returned", pid, err, procState(pid))
	}
}
